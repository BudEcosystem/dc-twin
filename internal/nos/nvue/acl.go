package nvue

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// ACLTables are the nftables tables the twin's NVUE renders ACLs to; applying replaces them whole
// and touches no other table.
const (
	aclBridgeTable = "dctwin_acl"
	aclInetTable   = "dctwin_acl"
)

// renderACL renders the configuration's ACLs, and the interfaces they are bound to, to an
// nftables ruleset (`nft -f`). An ACL on an interface filters what the switch forwards, as a
// Cumulus ACL does, not what it delivers to itself (a DHCP request to the switch's own server
// still arrives): bridged traffic in the bridge family's forward hook; routed traffic in the inet
// family's forward hook, where a packet that came in through a bridge port is recognised by the
// mark the bridge's prerouting hook gave it, as the port it came in on is the bridge's SVI by
// then. Rules are taken in order; a packet no rule matches is permitted. `hw-offload` is accepted
// and has no effect: there is no hardware.
func renderACL(t Tree, ports, bridged map[string]bool) (string, error) {
	acls := sub(t, "acl")
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	// create-then-delete removes the tables whether or not they exist
	w("table bridge %s", aclBridgeTable)
	w("delete table bridge %s", aclBridgeTable)
	w("table inet %s", aclInetTable)
	w("delete table inet %s", aclInetTable)

	type bind struct{ iface, acl, dir string }
	var binds []bind
	ifaces := sub(t, "interface")
	for _, i := range keys(ifaces) {
		for _, a := range keys(sub(ifaces, i, "acl")) {
			if acls[a] == nil {
				return "", fmt.Errorf("interface %s acl %s: no such ACL", i, a)
			}
			if !ports[i] && !strings.HasPrefix(i, "vlan") {
				return "", fmt.Errorf("interface %s acl %s: an ACL is bound to a port or an SVI", i, a)
			}
			for _, dir := range []string{"inbound", "outbound"} {
				if _, ok := sub(ifaces, i, "acl", a)[dir]; ok {
					binds = append(binds, bind{i, a, dir})
				}
			}
		}
	}
	if len(binds) == 0 {
		return b.String(), nil
	}
	used := map[string]bool{}
	for _, bd := range binds {
		used[bd.acl] = true
	}
	chains := map[string][]string{}
	var chainOrder []string
	for _, a := range keys(acls) {
		if !used[a] {
			continue
		}
		body, err := aclRules(a, sub(acls, a))
		if err != nil {
			return "", err
		}
		chains[a] = body
		chainOrder = append(chainOrder, a)
	}
	// a mark per bridge port with an inbound ACL, so the routed half knows the port
	marks := map[string]int{}
	for _, bd := range binds {
		if bd.dir == "inbound" && bridged[bd.iface] && marks[bd.iface] == 0 {
			marks[bd.iface] = 0xd71000 + len(marks) + 1
		}
	}
	chainName := func(a string) string { return "acl_" + sanitize(a) }
	for _, family := range []string{"bridge", "inet"} {
		table := aclBridgeTable
		if family == "inet" {
			table = aclInetTable
		}
		w("table %s %s {", family, table)
		for _, a := range chainOrder {
			w("  chain %s {", chainName(a))
			for _, r := range chains[a] {
				if family == "bridge" {
					r = strings.NewReplacer("@ip@", "ether type ip", "@ip6@", "ether type ip6").Replace(r)
				} else {
					r = strings.NewReplacer("@ip@", "meta nfproto ipv4", "@ip6@", "meta nfproto ipv6").Replace(r)
				}
				w("    %s", r)
			}
			w("  }")
		}
		if family == "bridge" {
			if len(marks) > 0 {
				w("  chain prerouting {")
				w("    type filter hook prerouting priority -300; policy accept;")
				for _, i := range sortedKeys(marks) {
					w("    iifname %q meta mark set 0x%x", i, marks[i])
				}
				w("  }")
			}
			w("  chain forward {")
			w("    type filter hook forward priority 0; policy accept;")
			for _, bd := range binds {
				if !bridged[bd.iface] {
					continue
				}
				if bd.dir == "inbound" {
					w("    iifname %q jump %s", bd.iface, chainName(bd.acl))
				} else {
					w("    oifname %q jump %s", bd.iface, chainName(bd.acl))
				}
			}
			w("  }")
			w("  chain output {")
			w("    type filter hook output priority 0; policy accept;")
			for _, bd := range binds {
				if bridged[bd.iface] && bd.dir == "outbound" {
					w("    oifname %q jump %s", bd.iface, chainName(bd.acl))
				}
			}
			w("  }")
		} else {
			w("  chain forward {")
			w("    type filter hook forward priority 0; policy accept;")
			for _, bd := range binds {
				switch {
				case bd.dir == "inbound" && bridged[bd.iface]:
					w("    meta mark 0x%x jump %s", marks[bd.iface], chainName(bd.acl))
				case bd.dir == "inbound":
					w("    iifname %q jump %s", bd.iface, chainName(bd.acl))
				case !bridged[bd.iface]:
					w("    oifname %q jump %s", bd.iface, chainName(bd.acl))
				}
			}
			w("  }")
		}
		w("}")
	}
	return b.String(), nil
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

// aclRules is one ACL's rules as nftables statements, in rule order.
func aclRules(name string, acl map[string]any) ([]string, error) {
	typ := str(acl, "type")
	if typ == "" {
		typ = "ipv4"
	}
	if typ != "ipv4" && typ != "ipv6" {
		return nil, fmt.Errorf("acl %s: type %s: ipv4 and ipv6 ACLs are emulated", name, typ)
	}
	fam := "ip"
	if typ == "ipv6" {
		fam = "ip6"
	}
	var out []string
	for _, seq := range rules(sub(acl, "rule")) {
		r := sub(acl, "rule", seq)
		at := fmt.Sprintf("acl %s rule %s", name, seq)
		a, err := action(r, at)
		if err != nil {
			return nil, err
		}
		verdict := "accept"
		if a == "deny" {
			verdict = "drop"
		}
		// a rule of this ACL's family only; the other family passes it
		// the family match is the table's: ether type in the bridge family, nfproto in inet
		parts := []string{"@" + fam + "@"}
		m := sub(r, "match", "ip")
		addr := func(field, nft string) error {
			v := str(m, field)
			if v == "" {
				return nil
			}
			var p netip.Prefix
			if pp, err := netip.ParsePrefix(v); err == nil {
				p = pp.Masked()
			} else if a, err := netip.ParseAddr(v); err == nil {
				p = netip.PrefixFrom(a, a.BitLen())
			} else {
				return fmt.Errorf("%s: %s %s", at, field, v)
			}
			if p.Addr().Is6() != (fam == "ip6") {
				return fmt.Errorf("%s: %s %s is not an %s address", at, field, v, typ)
			}
			if p.Bits() == 0 {
				return nil
			}
			parts = append(parts, fmt.Sprintf("%s %s %s", fam, nft, p))
			return nil
		}
		if err := addr("source-ip", "saddr"); err != nil {
			return nil, err
		}
		if err := addr("dest-ip", "daddr"); err != nil {
			return nil, err
		}
		proto := str(m, "protocol")
		switch proto {
		case "":
		// protocols by number: the switch need not have /etc/protocols
		case "tcp":
			parts = append(parts, "meta l4proto 6")
		case "udp":
			parts = append(parts, "meta l4proto 17")
		case "icmp":
			if fam != "ip" {
				return nil, fmt.Errorf("%s: protocol icmp in an ipv6 ACL", at)
			}
			parts = append(parts, "meta l4proto 1")
		case "icmpv6":
			if fam != "ip6" {
				return nil, fmt.Errorf("%s: protocol icmpv6 in an ipv4 ACL", at)
			}
			parts = append(parts, "meta l4proto 58")
		default:
			if n, err := strconv.Atoi(proto); err == nil && n >= 0 && n < 256 {
				parts = append(parts, fmt.Sprintf("meta l4proto %d", n))
			} else {
				return nil, fmt.Errorf("%s: protocol %s", at, proto)
			}
		}
		ports := func(field, nft string) error {
			ps := keys(sub(m, field))
			if len(ps) == 0 {
				return nil
			}
			if proto != "tcp" && proto != "udp" {
				return fmt.Errorf("%s: %s needs protocol tcp or udp", at, field)
			}
			for _, p := range ps {
				if n, err := strconv.Atoi(p); err != nil || n < 0 || n > 65535 {
					if _, _, ok := strings.Cut(p, "-"); !ok {
						return fmt.Errorf("%s: %s %s", at, field, p)
					}
				}
			}
			parts = append(parts, fmt.Sprintf("%s %s { %s }", proto, nft, strings.Join(ps, ", ")))
			return nil
		}
		if err := ports("source-port", "sport"); err != nil {
			return nil, err
		}
		if err := ports("dest-port", "dport"); err != nil {
			return nil, err
		}
		if t := str(m, "icmpv6-type"); t != "" {
			if proto != "icmpv6" {
				return nil, fmt.Errorf("%s: icmpv6-type needs protocol icmpv6", at)
			}
			n, ok := icmpv6Types[t]
			if !ok {
				return nil, fmt.Errorf("%s: icmpv6-type %s", at, t)
			}
			parts = append(parts, fmt.Sprintf("icmpv6 type %d", n))
		}
		if t := str(m, "icmp-type"); t != "" {
			if proto != "icmp" {
				return nil, fmt.Errorf("%s: icmp-type needs protocol icmp", at)
			}
			n, ok := icmpTypes[t]
			if !ok {
				return nil, fmt.Errorf("%s: icmp-type %s", at, t)
			}
			parts = append(parts, fmt.Sprintf("icmp type %d", n))
		}
		if states := keys(sub(r, "match", "conntrack")); len(states) > 0 {
			parts = append(parts, "ct state { "+strings.Join(states, ", ")+" }")
		}
		out = append(out, strings.Join(parts, " ")+" counter "+verdict)
	}
	return out, nil
}

// NVUE's ICMP and ICMPv6 type names, by number (a number is its own name)
var icmpTypes, icmpv6Types = typeNames(map[string]int{
	"echo-reply": 0, "dest-unreachable": 3, "redirect": 5, "echo-request": 8, "router-advertisement": 9,
	"router-solicitation": 10, "time-exceeded": 11, "parameter-problem": 12, "timestamp-request": 13, "timestamp-reply": 14,
}), typeNames(map[string]int{
	"dest-unreachable": 1, "packet-too-big": 2, "time-exceeded": 3, "parameter-problem": 4, "echo-request": 128,
	"echo-reply": 129, "router-solicitation": 133, "router-advertisement": 134, "neighbor-solicitation": 135,
	"neighbor-advertisement": 136, "redirect": 137,
})

func typeNames(m map[string]int) map[string]int {
	for i := 0; i < 256; i++ {
		m[strconv.Itoa(i)] = i
	}
	return m
}

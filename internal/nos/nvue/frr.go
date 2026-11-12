package nvue

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// renderFRR is /etc/frr/frr.conf for a configuration: the VRFs (their L3 VNIs, static routes and
// next-hop tracking), the route policy (prefix, community and AS-path lists, route maps), and BGP
// in each VRF that has it on, with peer groups, numbered and unnumbered neighbours and their
// address families, as Cumulus Linux's NVUE renders them to FRR.
func renderFRR(t Tree, sw nos.Switch, l3 map[string]int) (string, error) {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("frr version 8.4.4")
	w("frr defaults datacenter")
	host := str(t, "system", "hostname")
	if host == "" {
		host = sw.Hostname
	}
	w("hostname %s", host)
	w("log syslog informational")
	w("service integrated-vtysh-config")
	w("!")

	vrfs := sub(t, "vrf")
	// the default VRF's static routes and next-hop tracking are global; a tenant VRF's are in its
	// stanza, with its L3 VNI
	if err := renderStatics(w, " ", sub(vrfs, "default", "router"), ""); err != nil {
		return "", fmt.Errorf("vrf default: %w", err)
	}
	for _, name := range keys(vrfs) {
		if name == "default" {
			continue
		}
		r := sub(vrfs, name, "router")
		if l3[name] == 0 && sub(r, "static") == nil && sub(r, "nexthop-tracking") == nil {
			continue
		}
		w("vrf %s", name)
		if v := l3[name]; v != 0 {
			w(" vni %d", v)
		}
		if err := renderStatics(w, " ", r, name); err != nil {
			return "", fmt.Errorf("vrf %s: %w", name, err)
		}
		w("exit-vrf")
		w("!")
	}
	if err := renderPolicy(w, sub(t, "router", "policy")); err != nil {
		return "", err
	}
	renderBFD(w, sub(t, "router", "bfd"))

	asn := str(t, "router", "bgp", "autonomous-system")
	rid := str(t, "router", "bgp", "router-id")
	// BGP runs once it has an AS, unless turned off: Cumulus's own examples configure it with
	// `nv set router bgp autonomous-system <n>` and neighbours, never `router bgp enable on`
	if str(t, "router", "bgp", "enable") == "off" || asn == "" {
		for _, name := range keys(vrfs) {
			if on(t, "vrf", name, "router", "bgp", "enable") {
				return "", fmt.Errorf("vrf %s router bgp is on and router bgp has no autonomous-system or is off", name)
			}
		}
		return b.String() + snippet(t), nil
	}
	evpn := on(t, "evpn", "enable")
	for _, name := range append([]string{"default"}, without(keys(vrfs), "default")...) {
		bgp := sub(t, "vrf", name, "router", "bgp")
		if name != "default" && !on(bgp, "enable") {
			continue
		}
		// graceful restart is NVUE's global router bgp's; the twin gives it to the default VRF's
		// instance, the fabric's (whether Cumulus also renders it in each tenant VRF's is
		// UNCONFIRMED)
		var gr map[string]any
		if name == "default" {
			gr = sub(t, "router", "bgp", "graceful-restart")
		}
		if err := renderBGP(w, bgp, gr, name, asn, rid, evpn, l3[name] != 0); err != nil {
			return "", err
		}
	}
	return b.String() + snippet(t), nil
}

// snippet is the configuration's frr.conf snippet (system config snippet frr.conf), appended as
// Cumulus appends it.
func snippet(t Tree) string {
	s := str(t, "system", "config", "snippet", "frr.conf")
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "! system config snippet frr.conf\n" + strings.TrimRight(s, "\n") + "\n!\n"
}

// renderStatics renders a VRF's static routes and next-hop tracking: ip route <prefix> <via>
// [distance], a via an interface, a gateway or a blackhole.
func renderStatics(w func(string, ...any), indent string, router map[string]any, vrf string) error {
	in := indent
	if vrf == "" {
		in = ""
	}
	for _, fam := range []string{"ipv4", "ipv6"} {
		if on(router, "nexthop-tracking", fam, "resolved-via-default") {
			if fam == "ipv4" {
				w("%sip nht resolve-via-default", in)
			} else {
				w("%sipv6 nht resolve-via-default", in)
			}
		}
	}
	statics := sub(router, "static")
	for _, p := range keys(statics) {
		pfx, err := netip.ParsePrefix(p)
		if err != nil {
			return fmt.Errorf("router static %s: not a prefix", p)
		}
		cmd := "ip route"
		if pfx.Addr().Is6() {
			cmd = "ipv6 route"
		}
		if af := str(statics, p, "address-family"); af != "" && af != "ipv4-unicast" && af != "ipv6-unicast" {
			return fmt.Errorf("router static %s: address-family %s", p, af)
		}
		emit := func(vias map[string]any, distance string) error {
			for _, v := range keys(vias) {
				target := v
				switch typ := str(vias, v, "type"); typ {
				case "blackhole":
					target = "blackhole"
				case "interface", "ipv4-address", "ipv6-address", "":
				default:
					return fmt.Errorf("router static %s via %s: type %s", p, v, typ)
				}
				line := fmt.Sprintf("%s%s %s %s", in, cmd, pfx.Masked(), target)
				if distance != "" {
					line += " " + distance
				}
				w("%s", line)
			}
			return nil
		}
		if err := emit(sub(statics, p, "via"), ""); err != nil {
			return err
		}
		for _, d := range keys(sub(statics, p, "distance")) {
			if err := emit(sub(statics, p, "distance", d, "via"), d); err != nil {
				return err
			}
		}
	}
	if vrf == "" && (len(statics) > 0 || sub(router, "nexthop-tracking") != nil) {
		w("!")
	}
	return nil
}

// rules is a list's or a route map's rules in their sequence order
func rules(m map[string]any) []string {
	ks := keys(m)
	sort.Slice(ks, func(i, j int) bool {
		a, _ := strconv.Atoi(ks[i])
		b, _ := strconv.Atoi(ks[j])
		return a < b
	})
	return ks
}

func action(v map[string]any, at string) (string, error) {
	a := str(v, "action")
	if m := sub(v, "action"); m != nil {
		switch {
		case m["permit"] != nil:
			a = "permit"
		case m["deny"] != nil:
			a = "deny"
		}
	}
	if a != "permit" && a != "deny" {
		return "", fmt.Errorf("%s: action is permit or deny", at)
	}
	return a, nil
}

// renderPolicy renders router policy: prefix lists, community lists, AS-path lists, route maps.
func renderPolicy(w func(string, ...any), pol map[string]any) error {
	if pol == nil {
		return nil
	}
	for _, name := range keys(sub(pol, "prefix-list")) {
		pl := sub(pol, "prefix-list", name)
		v6 := str(pl, "type") == "ipv6"
		for _, seq := range rules(sub(pl, "rule")) {
			r := sub(pl, "rule", seq)
			at := fmt.Sprintf("router policy prefix-list %s rule %s", name, seq)
			a, err := action(r, at)
			if err != nil {
				return err
			}
			for _, m := range keys(sub(r, "match")) {
				line, six := "any", v6
				if m != "any" {
					p, err := netip.ParsePrefix(m)
					if err != nil {
						return fmt.Errorf("%s: match %s", at, m)
					}
					six = p.Addr().Is6()
					line = p.Masked().String()
					ge, le := str(r, "match", m, "min-prefix-len"), str(r, "match", m, "max-prefix-len")
					if n, _ := strconv.Atoi(ge); ge != "" && n > p.Bits() {
						line += " ge " + ge
					}
					if le != "" {
						line += " le " + le
					}
				}
				cmd := "ip"
				if six {
					cmd = "ipv6"
				}
				w("%s prefix-list %s seq %s %s %s", cmd, name, seq, a, line)
			}
		}
	}
	for _, name := range keys(sub(pol, "community-list")) {
		for _, seq := range rules(sub(pol, "community-list", name, "rule")) {
			r := sub(pol, "community-list", name, "rule", seq)
			a, err := action(r, "router policy community-list "+name+" rule "+seq)
			if err != nil {
				return err
			}
			// in rule order, without the sequence number: frr-reload cannot remove an entry
			// given by its sequence (FRR 8.4)
			w("bgp community-list standard %s %s %s", name, a, strings.Join(keys(sub(r, "community")), " "))
		}
	}
	for _, name := range keys(sub(pol, "as-path-list")) {
		for _, seq := range rules(sub(pol, "as-path-list", name, "rule")) {
			r := sub(pol, "as-path-list", name, "rule", seq)
			a, err := action(r, "router policy as-path-list "+name+" rule "+seq)
			if err != nil {
				return err
			}
			w("bgp as-path access-list %s seq %s %s %s", name, seq, a, str(r, "aspath-exp"))
		}
	}
	for _, name := range keys(sub(pol, "route-map")) {
		for _, seq := range rules(sub(pol, "route-map", name, "rule")) {
			r := sub(pol, "route-map", name, "rule", seq)
			at := fmt.Sprintf("router policy route-map %s rule %s", name, seq)
			a, err := action(r, at)
			if err != nil {
				return err
			}
			w("route-map %s %s %s", name, a, seq)
			if d := str(r, "description"); d != "" {
				w(" description %s", d)
			}
			// match type (ipv4, ipv6) is the family the rule's prefix list and length are read
			// in; FRR has no match of its own for it, so a rule matching only its type and an
			// interface (net-configurator's LOOPBACK_BGP) renders as its interface match
			m := sub(r, "match")
			if pl := str(m, "ip-prefix-list"); pl != "" {
				if str(m, "type") == "ipv6" {
					w(" match ipv6 address prefix-list %s", pl)
				} else {
					w(" match ip address prefix-list %s", pl)
				}
			}
			if n := str(m, "ip-prefix-len"); n != "" {
				if str(m, "type") == "ipv6" {
					w(" match ipv6 address prefix-len %s", n)
				} else {
					w(" match ip address prefix-len %s", n)
				}
			}
			if c := str(m, "community-list"); c != "" {
				w(" match community %s", c)
			}
			if ap := str(m, "as-path-list"); ap != "" {
				w(" match as-path %s", ap)
			}
			switch rt := str(m, "evpn-route-type"); rt {
			case "":
			case "ip-prefix":
				w(" match evpn route-type prefix")
			case "macip":
				w(" match evpn route-type macip")
			case "multicast":
				w(" match evpn route-type multicast")
			default:
				return fmt.Errorf("%s: match evpn-route-type %s", at, rt)
			}
			if i := str(m, "interface"); i != "" {
				w(" match interface %s", i)
			}
			if tag := str(m, "tag"); tag != "" {
				w(" match tag %s", tag)
			}
			set := sub(r, "set")
			if c := keys(sub(set, "community")); len(c) > 0 {
				w(" set community %s", strings.Join(c, " "))
			}
			if c := keys(sub(set, "large-community")); len(c) > 0 {
				w(" set large-community %s", strings.Join(c, " "))
			}
			if tag := str(set, "tag"); tag != "" {
				w(" set tag %s", tag)
			}
			if bw := str(set, "ext-community-bw"); bw != "" {
				line, err := extCommunityBW(bw)
				if err != nil {
					return fmt.Errorf("%s: set ext-community-bw %s: %w", at, bw, err)
				}
				w(" set extcommunity bandwidth %s", line)
			}
			w("exit")
		}
	}
	w("!")
	return nil
}

// extCommunityBW is FRR's `set extcommunity bandwidth` for NVUE's set ext-community-bw: the
// link bandwidth weighted ECMP spreads by (net-configurator's WEIGHTED_ECMP sets multipaths, the
// number of paths, FRR's num-multipaths). The -non-transitive spellings are UNCONFIRMED.
func extCommunityBW(v string) (string, error) {
	switch v {
	case "multipaths":
		return "num-multipaths", nil
	case "cumulative":
		return "cumulative", nil
	case "multipaths-non-transitive":
		return "num-multipaths non-transitive", nil
	case "cumulative-non-transitive":
		return "cumulative non-transitive", nil
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 25600 {
		return v, nil
	}
	return "", fmt.Errorf("a bandwidth in Mbps (1 to 25600), cumulative or multipaths")
}

// renderBFD renders router bfd's profiles as FRR's bfd node, which bfdd reads: the detection
// multiplier and the receive and transmit intervals (NVUE's min-rx-interval and min-tx-interval,
// milliseconds). Nothing when router bfd is off.
func renderBFD(w func(string, ...any), bfd map[string]any) {
	profiles := sub(bfd, "profile")
	if len(profiles) == 0 || str(bfd, "enable") == "off" || str(bfd, "state") == "disabled" {
		return
	}
	w("bfd")
	for _, name := range keys(profiles) {
		p := sub(profiles, name)
		w(" profile %s", name)
		if m := str(p, "detect-multiplier"); m != "" {
			w("  detect-multiplier %s", m)
		}
		if rx := str(p, "min-rx-interval"); rx != "" {
			w("  receive-interval %s", rx)
		}
		if tx := str(p, "min-tx-interval"); tx != "" {
			w("  transmit-interval %s", tx)
		}
		w(" exit")
		w(" !")
	}
	w("exit")
	w("!")
}

// families NVUE has, and FRR's name for each
var families = []struct{ nvue, frr string }{
	{"ipv4-unicast", "ipv4 unicast"},
	{"ipv6-unicast", "ipv6 unicast"},
	{"l2vpn-evpn", "l2vpn evpn"},
}

// renderBGP renders one VRF's router bgp.
func renderBGP(w func(string, ...any), bgp, gr map[string]any, vrf, asn, rid string, evpn, l3vni bool) error {
	if a := str(bgp, "autonomous-system"); a != "" {
		asn = a
	}
	if r := str(bgp, "router-id"); r != "" {
		rid = r
	}
	if vrf == "default" {
		w("router bgp %s", asn)
	} else {
		w("router bgp %s vrf %s", asn, vrf)
	}
	if rid != "" {
		w(" bgp router-id %s", rid)
	}
	w(" no bgp ebgp-requires-policy")
	// NVUE's multipath aspath-ignore, on unless turned off
	if str(bgp, "path-selection", "multipath", "aspath-ignore") != "off" {
		w(" bgp bestpath as-path multipath-relax")
	}
	if str(bgp, "path-selection", "multipath", "bandwidth") == "ignore" {
		w(" bgp bestpath bandwidth ignore")
	}
	// graceful restart: full is FRR's bgp graceful-restart, off its graceful-restart-disable,
	// helper-only FRR's default; the timers are FRR's restart, select-defer and stalepath times
	switch str(gr, "mode") {
	case "full":
		w(" bgp graceful-restart")
	case "off":
		w(" bgp graceful-restart-disable")
	}
	if v := str(gr, "restart-time"); v != "" {
		w(" bgp graceful-restart restart-time %s", v)
	}
	if v := str(gr, "path-selection-deferral-time"); v != "" {
		w(" bgp graceful-restart select-defer-time %s", v)
	}
	if v := str(gr, "stale-routes-time"); v != "" {
		w(" bgp graceful-restart stalepath-time %s", v)
	}
	groups := sub(bgp, "peer-group")
	neighbors := sub(bgp, "neighbor")
	// peer: a neighbour's name in FRR (its address, its interface, or the group's name)
	peerOpts := func(id string, p map[string]any, inherits bool) {
		if ra := str(p, "remote-as"); ra != "" {
			w(" neighbor %s remote-as %s", id, ra)
		}
		if d := str(p, "description"); d != "" {
			w(" neighbor %s description %s", id, d)
		}
		if h := str(p, "multihop-ttl"); h != "" {
			w(" neighbor %s ebgp-multihop %s", id, h)
		}
		if u := str(p, "update-source"); u != "" {
			w(" neighbor %s update-source %s", id, u)
		}
		if pw := str(p, "password"); pw != "" {
			w(" neighbor %s password %s", id, pw)
		}
		if on(p, "passive-mode") {
			w(" neighbor %s passive", id)
		}
		if str(p, "nexthop-connected-check") == "off" {
			w(" neighbor %s disable-connected-check", id)
		}
		tm := sub(p, "timers")
		ka, hold := str(tm, "keepalive"), str(tm, "hold")
		switch {
		case ka != "" && hold != "":
			w(" neighbor %s timers %s %s", id, ka, hold)
		case !inherits:
			w(" neighbor %s timers 3 9", id)
		}
		switch cr := str(tm, "connection-retry"); {
		case cr != "":
			w(" neighbor %s timers connect %s", id, cr)
		case !inherits:
			w(" neighbor %s timers connect 10", id)
		}
		if ra := str(tm, "route-advertisement"); ra != "" {
			if ra == "none" {
				ra = "0"
			}
			w(" neighbor %s advertisement-interval %s", id, ra)
		}
		// BFD for the session, through bfdd: a profile of router bfd's, its own timers (FRR's
		// order: multiplier, receive, transmit), or on with bfdd's defaults
		if b := sub(p, "bfd"); b != nil && str(b, "enable") != "off" {
			prof, mult, rx, tx := str(b, "profile"), str(b, "detect-multiplier"), str(b, "min-rx-interval"), str(b, "min-tx-interval")
			switch {
			case prof != "":
				w(" neighbor %s bfd profile %s", id, prof)
			case mult != "" || rx != "" || tx != "":
				w(" neighbor %s bfd %s %s %s", id, or(mult, "3"), or(rx, "300"), or(tx, "300"))
			case on(b, "enable"):
				w(" neighbor %s bfd", id)
			}
		}
	}
	for _, g := range keys(groups) {
		w(" neighbor %s peer-group", g)
		peerOpts(g, sub(groups, g), false)
	}
	for _, n := range keys(neighbors) {
		nb := sub(neighbors, n)
		pg := str(nb, "peer-group")
		if pg != "" && groups[pg] == nil {
			return fmt.Errorf("vrf %s router bgp neighbor %s: peer-group %s does not exist", vrf, n, pg)
		}
		_, addrErr := netip.ParseAddr(n)
		unnumbered := addrErr != nil
		if t := str(nb, "type"); t == "unnumbered" && !unnumbered || t == "numbered" && unnumbered {
			return fmt.Errorf("vrf %s router bgp neighbor %s: type %s", vrf, n, t)
		}
		if pg == "" && str(nb, "remote-as") == "" {
			return fmt.Errorf("vrf %s router bgp neighbor %s: no remote-as", vrf, n)
		}
		switch {
		case unnumbered && pg != "":
			w(" neighbor %s interface peer-group %s", n, pg)
		case unnumbered:
			w(" neighbor %s interface remote-as %s", n, str(nb, "remote-as"))
			nb = without2(nb, "remote-as")
		case pg != "":
			w(" neighbor %s peer-group %s", n, pg)
		}
		peerOpts(n, nb, pg != "")
	}
	w(" !")
	for _, f := range families {
		af := sub(bgp, "address-family", f.nvue)
		lines := familyLines(f.nvue, af, bgp, groups, neighbors, vrf, evpn, l3vni)
		// the IPv4 family is always rendered, as Cumulus renders it; the others when they have
		// something in them
		if len(lines) == 0 && f.nvue != "ipv4-unicast" {
			continue
		}
		if f.nvue == "ipv4-unicast" && str(af, "enable") == "off" && len(lines) == 0 {
			continue
		}
		w(" address-family %s", f.frr)
		for _, l := range lines {
			w("  %s", l)
		}
		w(" exit-address-family")
	}
	w("exit")
	w("!")
	return nil
}

// familyLines is the body of one address family of one VRF's BGP.
func familyLines(fam string, af, bgp, groups, neighbors map[string]any, vrf string, evpn, l3vni bool) []string {
	var out []string
	add := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
	switch fam {
	case "ipv4-unicast", "ipv6-unicast":
		for _, n := range keys(sub(af, "network")) {
			add("network %s", n)
		}
		for _, r := range []string{"connected", "static"} {
			if on(af, "redistribute", r, "enable") {
				if rm := str(af, "redistribute", r, "route-map"); rm != "" {
					add("redistribute %s route-map %s", r, rm)
				} else {
					add("redistribute %s", r)
				}
			}
		}
		if m := str(af, "multipaths", "ebgp"); m != "" {
			add("maximum-paths %s", m)
		}
		if m := str(af, "multipaths", "ibgp"); m != "" {
			add("maximum-paths ibgp %s", m)
		}
		if fv := sub(af, "route-import", "from-vrf"); fv != nil && str(fv, "enable") != "off" {
			if rm := str(fv, "route-map"); rm != "" {
				add("import vrf route-map %s", rm)
			}
			for _, v := range keys(sub(fv, "list")) {
				add("import vrf %s", v)
			}
		}
	}
	// a peer's family is on: NVUE's default is IPv4 unicast on, the others off
	enabled := func(p map[string]any) (bool, bool) {
		e := str(p, "address-family", fam, "enable")
		if e == "" {
			return fam == "ipv4-unicast", false
		}
		return e == "on", true
	}
	peerAF := func(id string, p map[string]any, member bool) {
		switch en, explicit := enabled(p); {
		case member && !explicit:
			// a member takes its group's family; it adds its own policy, if any
		case en:
			add("neighbor %s activate", id)
		case fam == "ipv4-unicast":
			add("no neighbor %s activate", id)
		}
		a := sub(p, "address-family", fam)
		if rm := str(a, "policy", "inbound", "route-map"); rm != "" {
			add("neighbor %s route-map %s in", id, rm)
		}
		if rm := str(a, "policy", "outbound", "route-map"); rm != "" {
			add("neighbor %s route-map %s out", id, rm)
		}
		if fam != "l2vpn-evpn" {
			if pl := str(a, "policy", "inbound", "prefix-list"); pl != "" {
				add("neighbor %s prefix-list %s in", id, pl)
			}
			if pl := str(a, "policy", "outbound", "prefix-list"); pl != "" {
				add("neighbor %s prefix-list %s out", id, pl)
			}
		}
		if fam != "l2vpn-evpn" && on(a, "default-route-origination", "enable") {
			add("neighbor %s default-originate", id)
		}
		if am := sub(a, "aspath", "allow-my-asn"); am != nil && on(am, "enable") {
			switch {
			case on(am, "origin"):
				add("neighbor %s allowas-in origin", id)
			case str(am, "occurrences") != "":
				add("neighbor %s allowas-in %s", id, str(am, "occurrences"))
			default:
				add("neighbor %s allowas-in", id)
			}
		}
	}
	for _, g := range keys(groups) {
		peerAF(g, sub(groups, g), false)
	}
	for _, n := range keys(neighbors) {
		nb := sub(neighbors, n)
		peerAF(n, nb, str(nb, "peer-group") != "")
	}
	if fam == "l2vpn-evpn" {
		v4 := sub(bgp, "address-family", "ipv4-unicast")
		v6 := sub(bgp, "address-family", "ipv6-unicast")
		switch {
		case vrf == "default" && evpn:
			add("advertise-all-vni")
			if rd := str(bgp, "rd"); rd != "" {
				add("rd %s", rd)
			}
		case vrf != "default":
			if rd := str(bgp, "rd"); rd != "" {
				add("rd %s", rd)
			}
			if on(v4, "route-export", "to-evpn", "enable") || l3vni && str(v4, "route-export", "to-evpn", "enable") != "off" {
				add("advertise ipv4 unicast")
			}
			if on(v6, "route-export", "to-evpn", "enable") {
				add("advertise ipv6 unicast")
			}
			// "auto" is the route target FRR derives itself (<AS>:<VNI>), which it has
			// without being told
			for _, rt := range keys(sub(bgp, "route-import", "from-evpn", "route-target")) {
				if rt != "auto" {
					add("route-target import %s", rt)
				}
			}
			for _, rt := range keys(sub(bgp, "route-export", "to-evpn", "route-target")) {
				if rt != "auto" {
					add("route-target export %s", rt)
				}
			}
		}
		if vrf == "default" && !evpn {
			// EVPN is off: no peer carries it
			return nil
		}
	}
	return out
}

func without(list []string, v string) []string {
	var out []string
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// or is v, or def when v is "".
func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// without2 is a copy of an object without one key.
func without2(m map[string]any, k string) map[string]any {
	out := map[string]any{}
	for kk, v := range m {
		if kk != k {
			out[kk] = v
		}
	}
	return out
}

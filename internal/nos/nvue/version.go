package nvue

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a Cumulus Linux release (5.18.3): which NVUE grammar a switch speaks. NVUE's object
// model changed in 5.15 (NVIDIA's "New and Changed NVUE Commands | Cumulus Linux 5.15", as
// NVIDIA's net-configurator records it in docs/NVUE_VERSION_COMPAT.md): a feature's switch is
// `state enabled|disabled` where it was `enable on|off`; an interface's addresses, VRR and
// gateway are under ipv4 and ipv6 where they were under ip, and its VRF is `interface <id> vrf`;
// NTP, DNS and LLDP moved from service to system; `system timezone` is `system date-time
// timezone`. The emulated schema holds both grammars; a node a release does not have is refused
// by name there, saying what replaced it (or what it was before), and Render reads either
// (normalize).
//
// The zero Version is no release in particular: every form of every release is accepted (what
// Validate and CLIPath check, and the twin's `nv` before the switch's NVUE checks again).
type Version struct{ Major, Minor, Patch int }

// the releases the schema's gates name
var (
	v59  = Version{5, 9, 0}  // system config auto-save (vendor facts C.2: "Auto-saves from 5.9 on")
	v511 = Version{5, 11, 0} // system snmp-server (net-configurator's compat notes)
	v515 = Version{5, 15, 0} // the 5.15 grammar
)

// DefaultVersion is the release the emulated NVUE follows when the estate names none: DefaultBuild's.
var DefaultVersion = Version{5, 6, 0}

// ParseVersion reads a Cumulus Linux release: 5.18, 5.18.3.
func ParseVersion(s string) (Version, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) < 2 || len(parts) > 3 {
		return Version{}, fmt.Errorf("%q is not a Cumulus Linux release (5.18.3)", s)
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return Version{}, fmt.Errorf("%q is not a Cumulus Linux release (5.18.3)", s)
		}
		n[i] = x
	}
	v := Version{n[0], n[1], n[2]}
	if v.Major != 5 {
		return Version{}, fmt.Errorf("%q: the emulated NVUE is Cumulus Linux 5's", s)
	}
	return v, nil
}

// buildPrefix is how a Cumulus switch's build names its release (GET /system's build, the
// header of `nv config show`: "Cumulus Linux 5.18.0", vendor facts C.2).
const buildPrefix = "Cumulus Linux "

// Build is the build a switch running a release reports (GET /system).
func Build(v Version) string { return buildPrefix + v.String() }

// BuildVersion is the release a build names; a build that is not Cumulus Linux's (an emulated
// BlueField's HBN, whose schema is NICo's templates', written in the grammar before 5.15) speaks
// DefaultVersion's grammar.
func BuildVersion(build string) Version {
	if rest, ok := strings.CutPrefix(build, buildPrefix); ok {
		if v, err := ParseVersion(rest); err == nil {
			return v
		}
	}
	return DefaultVersion
}

func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// short is a release as the gates' messages name it: 5.15.
func (v Version) short() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

func (v Version) IsZero() bool { return v == Version{} }

// Less is whether v is an earlier release than o.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// gate refuses a node a release does not have: one introduced after it (since), or removed in it
// or before (removed). instead is what to use: the replacement for a removed node, the earlier
// form for a new one.
func (n *node) gate(v Version, name string) error {
	if v.IsZero() || n == nil {
		return nil
	}
	if !n.since.IsZero() && v.Less(n.since) {
		msg := fmt.Sprintf("%q is Cumulus Linux %s's NVUE and this switch runs %s", name, n.since.short(), v)
		if n.instead != "" {
			msg += ": before " + n.since.short() + ", " + n.instead
		}
		return fmt.Errorf("%s", msg)
	}
	if !n.removed.IsZero() && !v.Less(n.removed) {
		msg := fmt.Sprintf("%q was replaced in Cumulus Linux %s and this switch runs %s", name, n.removed.short(), v)
		if n.instead != "" {
			msg += ": use " + n.instead
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// since is a node a release introduced, before it spelt as instead says.
func since(v Version, instead string, n *node) *node {
	c := *n
	c.since, c.instead = v, instead
	return &c
}

// until is a node a release removed, replaced by instead.
func until(v Version, instead string, n *node) *node {
	c := *n
	c.removed, c.instead = v, instead
	return &c
}

// toggle is a feature's switch as the release spells it: `enable on|off` to 5.14, `state
// enabled|disabled` from 5.15 (the 5.15 change's first item, which names evpn, nve vxlan, router
// bgp, router vrr, qos roce, a VRF's evpn and router bgp, its address families, redistribute
// connected, route-export to-evpn, route-import from-vrf, and a peer group's address families and
// bfd), with the node's other children. Where the 5.15 list does not name a switch (router ptm,
// reflexive-acl, default-route-origination, allow-my-asn), the schema keeps `enable` in every
// release: whether 5.15 renamed those is UNCONFIRMED.
func toggle(more map[string]*node) map[string]*node {
	return with(more, map[string]*node{
		"enable": until(v515, "state enabled|disabled", lf(kOnOff)),
		"state":  since(v515, "enable on|off", &node{leaf: kToggle, rename: "enable"}),
	})
}

// state is a switch spelt `state enabled|disabled` in every release the twin emulates, for a
// node new to the emulation whose spelling before 5.15 is UNCONFIRMED (system api, ssh-server,
// wjh, a user's state): recorded, read by nothing in 5.14's grammar.
var state = map[string]*node{"state": lf(kToggle)}

// switchOn is whether a node's switch is on, in either release's spelling: enable on, state
// enabled.
func switchOn(m map[string]any) bool {
	return str(m, "enable") == "on" || str(m, "state") == "enabled"
}

// normalize is a configuration in the grammar the renderers read, 5.14's, whichever release's it
// is in: a 5.15 switch (state enabled|disabled) is enable on|off; on|off leaves set enabled or
// disabled are on or off; an interface's ipv4 and ipv6 (addresses, gateways, VRR) and vrf are its
// ip's; system ntp, dns and lldp are service ntp <vrf>, dns <vrf> and lldp. What 5.15 added with
// no 5.14 equivalent stays as it is, for the renderers that read it.
func normalize(t Tree) Tree {
	t = Clone(t)
	toggles(schema, t, false)
	for _, name := range keys(sub(t, "interface")) {
		it := sub(t, "interface", name)
		ip := sub(it, "ip")
		get := func() map[string]any {
			if ip == nil {
				ip = map[string]any{}
				it["ip"] = ip
			}
			return ip
		}
		for _, fam := range []string{"ipv4", "ipv6"} {
			f := sub(it, fam)
			if f == nil {
				continue
			}
			delete(it, fam)
			for _, k := range []string{"address", "gateway"} {
				if m := sub(f, k); m != nil {
					Patch(get(), []string{k}, Clone(m))
				}
			}
			if vrr := sub(f, "vrr"); vrr != nil {
				vrr = Clone(vrr)
				// 5.15's vrr-state is 5.14's ip vrr state (up|down)
				if vs := sub(vrr, "vrr-state"); vs != nil {
					delete(vrr, "vrr-state")
					vrr["state"] = vs
				}
				Patch(get(), []string{"vrr"}, vrr)
			}
		}
		if v, ok := it["vrf"]; ok {
			delete(it, "vrf")
			get()["vrf"] = v
		}
	}
	sys := sub(t, "system")
	if ntp := sub(sys, "ntp"); ntp != nil {
		delete(sys, "ntp")
		// the VRF NTP runs in when 5.15's `system ntp vrf` is not set: default (UNCONFIRMED)
		vrf := str(ntp, "vrf")
		if vrf == "" {
			vrf = "default"
		}
		if servers := sub(ntp, "server"); len(servers) > 0 {
			Patch(t, []string{"service", "ntp", vrf, "server"}, Clone(servers))
		}
	}
	if dns := sub(sys, "dns"); dns != nil {
		delete(sys, "dns")
		// 5.15's `system dns server <ip>` names no VRF; the twin resolves through the default VRF
		// (where Cumulus 5.15 sends the queries is UNCONFIRMED)
		if servers := sub(dns, "server"); len(servers) > 0 {
			Patch(t, []string{"service", "dns", "default", "server"}, Clone(servers))
		}
	}
	if lldp := sub(sys, "lldp"); lldp != nil {
		delete(sys, "lldp")
		Patch(t, []string{"service", "lldp"}, Clone(lldp))
	}
	return t
}

// upgrade is a configuration in 5.14's grammar (Startup's, Factory's) in 5.15's: normalize's
// inverse, for a switch that runs 5.15 or later, so what it serves is what its release has.
func upgrade(t Tree) Tree {
	t = Clone(t)
	for _, name := range keys(sub(t, "interface")) {
		it := sub(t, "interface", name)
		ip := sub(it, "ip")
		if ip == nil {
			continue
		}
		delete(it, "ip")
		for _, k := range []string{"address", "gateway"} {
			for _, a := range keys(sub(ip, k)) {
				fam := "ipv4"
				if strings.Contains(a, ":") {
					fam = "ipv6"
				}
				Patch(it, []string{fam, k, a}, Tree{})
			}
		}
		if vrr := sub(ip, "vrr"); vrr != nil {
			vrr = Clone(vrr)
			if st := sub(vrr, "state"); st != nil {
				delete(vrr, "state")
				vrr["vrr-state"] = st
			}
			Patch(it, []string{"ipv4", "vrr"}, vrr)
		}
		if v, ok := ip["vrf"]; ok {
			it["vrf"] = v
		}
	}
	svc := sub(t, "service")
	if ntp := sub(svc, "ntp"); ntp != nil {
		delete(svc, "ntp")
		for _, vrf := range keys(ntp) {
			if servers := sub(ntp, vrf, "server"); len(servers) > 0 {
				Patch(t, []string{"system", "ntp", "server"}, Clone(servers))
				Patch(t, []string{"system", "ntp", "vrf"}, vrf)
			}
		}
	}
	if dns := sub(svc, "dns"); dns != nil {
		delete(svc, "dns")
		for _, vrf := range keys(dns) {
			if servers := sub(dns, vrf, "server"); len(servers) > 0 {
				Patch(t, []string{"system", "dns", "server"}, Clone(servers))
			}
		}
	}
	if lldp := sub(svc, "lldp"); lldp != nil {
		delete(svc, "lldp")
		Patch(t, []string{"system", "lldp"}, Clone(lldp))
	}
	if svc != nil && len(svc) == 0 {
		delete(t, "service")
	}
	if tz := str(t, "system", "timezone"); tz != "" {
		delete(sub(t, "system"), "timezone")
		Patch(t, []string{"system", "date-time", "timezone"}, tz)
	}
	toggles(schema, t, true)
	return t
}

// toggles rewrites a tree's switches in place, by the schema: to 5.15's state enabled|disabled
// (up), or to 5.14's enable on|off and an on|off leaf's on or off (down).
func toggles(n *node, m map[string]any, up bool) {
	if n == nil || m == nil {
		return
	}
	// the node's 5.15 switch, and what it was before
	var stateName, enableName string
	for k, c := range n.kids {
		if c.leaf == kToggle && c.rename != "" {
			stateName, enableName = k, c.rename
		}
	}
	for _, k := range keys(m) {
		c, err := n.child(k)
		if err != nil {
			continue
		}
		v := m[k]
		switch {
		case !up && k == stateName:
			delete(m, k)
			if s := map[string]string{"enabled": "on", "disabled": "off"}[str(v)]; s != "" {
				m[enableName] = s
			}
		case up && stateName != "" && k == enableName:
			delete(m, k)
			if s := map[string]string{"on": "enabled", "off": "disabled"}[str(v)]; s != "" {
				m[stateName] = s
			}
		case !up && c.leaf == kOnOff:
			if s := map[string]string{"enabled": "on", "disabled": "off"}[str(v)]; s != "" {
				m[k] = s
			}
		case c.leaf == 0:
			if cm, ok := v.(map[string]any); ok {
				toggles(c, cm, up)
			}
		}
	}
}

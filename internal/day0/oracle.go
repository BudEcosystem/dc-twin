package day0

import (
	"fmt"
	"sort"
	"strings"
)

// FactoryHostnames are the hostnames a NOS ships with (18 — Factory state): a switch still
// called one of these has not been configured.
var FactoryHostnames = map[string]bool{"cumulus": true, "sonic": true, "localhost": true, "": true}

// Evaluate is the convergence oracle (18 — Converged, D77): for every switch of the design, in
// path order, what the NOS observer read from the switch itself (obs, by switch path) against
// the design. A switch the observer has not read is in factory phase, with one diff saying so.
//
// With plan: given the comparison is exact. A switch is converged when
//
//   - its hostname is the design's;
//   - every design interface of kind fabric has a BGP session keyed by its peer address
//     (PeerAddr; by the interface's name when the design gives no address), and every one of
//     kind unnumbered a session keyed by its interface name, each Established, and carrying
//     L2VPN EVPN when the switch's design has EVPN;
//   - every design loopback of every switch of its fabric is in its RIB as a /32 (fabrics route
//     apart: a frontend leaf has no route to the OOB switches'), but that of another switch of
//     its own AS it has no session with (spines sharing an AS, RFC 7938: eBGP's loop check drops
//     each other's paths);
//   - each of its design VLANs exists with the design's gateway address;
//   - its management port holds a lease.
//
// With plan: tool the SUT allocated its own addresses and AS numbers, so the comparison is
// structural, and deliberately simple:
//
//   - its hostname is not a factory one and no other observed switch has the same one;
//   - every design fabric or unnumbered interface has an Established session (EVPN as above):
//     a session keyed by the interface's name counts for that interface; the rest of the
//     interfaces are matched by count against Established sessions keyed by an address, since
//     which address a tool gave which link is not known;
//   - each of its design VLANs exists (with any address);
//   - its management port holds a lease.
//
// Loopback uniqueness and reachability are not checked with plan: tool (the observer does not
// say which /32 in a RIB is whose loopback); a stack's policy can add it. Relay forwarding,
// listed under Converged in 18, is exercised by the twin's own endpoints leasing through it and
// judged from their lease events, not here.
//
// The phase is converged when the switch is; otherwise the phase the observer reported (a
// reported converged the oracle disagrees with is configured), else configured when its
// hostname is no longer a factory one or it has any BGP session, else nos-booted when it holds a
// lease or reports a hostname, else factory.
func Evaluate(d Design, obs map[string]Observed) []Verdict {
	d = sorted(d)
	loopbacks := map[string]map[string]Switch{} // fabric -> /32 -> its switch
	for _, s := range d.Switches {
		if s.Loopback != "" {
			if loopbacks[s.Fabric] == nil {
				loopbacks[s.Fabric] = map[string]Switch{}
			}
			loopbacks[s.Fabric][withLen(s.Loopback, 32)] = s
		}
	}
	names := map[string][]string{} // observed hostname -> paths
	for _, s := range d.Switches {
		if o, ok := obs[s.Path]; ok && !FactoryHostnames[o.Hostname] {
			names[o.Hostname] = append(names[o.Hostname], s.Path)
		}
	}
	out := make([]Verdict, 0, len(d.Switches))
	for _, s := range d.Switches {
		if !s.Factory {
			continue // the twin configured it: there is nothing of day 0 to judge
		}
		o, seen := obs[s.Path]
		if !seen {
			out = append(out, Verdict{Path: s.Path, Phase: PhaseFactory, Diffs: []string{"not observed yet"}})
			continue
		}
		var diffs []string
		if d.Plan == "tool" {
			diffs = structural(s, o, names)
		} else {
			diffs = exact(s, o, loopbacks[s.Fabric])
		}
		v := Verdict{Path: s.Path, Converged: len(diffs) == 0, Diffs: diffs}
		v.Phase = phase(o, v.Converged)
		out = append(out, v)
	}
	return out
}

func phase(o Observed, converged bool) string {
	switch {
	case converged:
		return PhaseConverged
	case o.Phase == PhaseConverged:
		return PhaseConfigured
	case o.Phase != "":
		return o.Phase
	case !FactoryHostnames[o.Hostname] || len(o.Sessions) > 0:
		return PhaseConfigured
	case o.Mgmt != "" || o.Hostname != "":
		return PhaseBooted
	}
	return PhaseFactory
}

// exact compares a switch with its design value by value (plan: given).
func exact(s Switch, o Observed, loopbacks map[string]Switch) []string {
	var diffs []string
	if o.Hostname != s.Hostname {
		diffs = append(diffs, fmt.Sprintf("hostname is %s, the design says %s", orNone(o.Hostname), s.Hostname))
	}
	for _, f := range sessionIfaces(s) {
		key := f.Name
		if f.Kind == "fabric" && f.PeerAddr != "" {
			key = f.PeerAddr
		}
		if why := sessionDiff(o.Sessions[key], has(o.Sessions, key), s.EVPN); why != "" {
			diffs = append(diffs, linkName(f)+": "+why)
		}
	}
	rib := map[string]bool{}
	for _, r := range o.Routes {
		rib[r] = true
	}
	peers := map[string]bool{}
	for _, it := range s.Interfaces {
		if host, _, ok := strings.Cut(it.Peer, ":"); ok {
			peers[host] = true
		}
	}
	for _, lo := range keys(loopbacks) {
		x := loopbacks[lo]
		// another switch of its own AS (the spines of RFC 7938's scheme) is reached only over a
		// session of their own: eBGP drops a path whose AS path holds its own AS
		if x.Path != s.Path && x.ASN != 0 && x.ASN == s.ASN && !peers[x.Hostname] {
			continue
		}
		if !rib[lo] {
			diffs = append(diffs, fmt.Sprintf("%s (%s's loopback) is not in the RIB", lo, x.Hostname))
		}
	}
	for _, v := range sortedVlans(s.Vlans) {
		got, ok := o.Vlans[v.Name]
		switch {
		case !ok:
			diffs = append(diffs, fmt.Sprintf("%s is missing, the design says %s", v.Name, v.Gateway))
		case got != v.Gateway:
			diffs = append(diffs, fmt.Sprintf("%s has %s, the design says %s", v.Name, orNone(got), v.Gateway))
		}
	}
	if o.Mgmt == "" {
		diffs = append(diffs, "eth0 holds no management lease")
	}
	return diffs
}

// structural compares a switch with the design's shape (plan: tool).
func structural(s Switch, o Observed, names map[string][]string) []string {
	var diffs []string
	switch {
	case FactoryHostnames[o.Hostname]:
		diffs = append(diffs, fmt.Sprintf("hostname is %s, a factory one", orNone(o.Hostname)))
	case len(names[o.Hostname]) > 1:
		var others []string
		for _, p := range names[o.Hostname] {
			if p != s.Path {
				others = append(others, p)
			}
		}
		diffs = append(diffs, fmt.Sprintf("hostname %s is also %s's", o.Hostname, strings.Join(others, ", ")))
	}
	byAddr := 0 // Established sessions keyed by an address, usable for any link
	ifaceNames := map[string]bool{}
	for _, f := range sessionIfaces(s) {
		ifaceNames[f.Name] = true
	}
	for _, k := range keys(o.Sessions) {
		if !ifaceNames[k] && sessionDiff(o.Sessions[k], true, s.EVPN) == "" {
			byAddr++
		}
	}
	var unmatched []Interface
	for _, f := range sessionIfaces(s) {
		x, ok := o.Sessions[f.Name]
		if ok && sessionDiff(x, true, s.EVPN) == "" {
			continue
		}
		if ok {
			diffs = append(diffs, linkName(f)+": "+sessionDiff(x, true, s.EVPN))
			continue
		}
		unmatched = append(unmatched, f)
	}
	if len(unmatched) > byAddr {
		var ports []string
		for _, f := range unmatched {
			ports = append(ports, f.Name)
		}
		diffs = append(diffs, fmt.Sprintf("%d Established session(s) for %d design link(s) without one of their own (%s)",
			byAddr, len(unmatched), strings.Join(ports, ", ")))
	}
	for _, v := range sortedVlans(s.Vlans) {
		if got := o.Vlans[v.Name]; got == "" {
			diffs = append(diffs, v.Name+" is missing")
		}
	}
	if o.Mgmt == "" {
		diffs = append(diffs, "eth0 holds no management lease")
	}
	return diffs
}

// sessionIfaces are the design interfaces that carry a BGP session, in port order.
func sessionIfaces(s Switch) []Interface {
	var out []Interface
	for _, f := range s.Interfaces {
		if f.Kind == "fabric" || f.Kind == "unnumbered" {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return portLess(out[i].Name, out[j].Name) })
	return out
}

func sessionDiff(x Session, ok, evpn bool) string {
	switch {
	case !ok:
		return "no BGP session"
	case x.State != "Established":
		return orNone(x.State) + ", not Established"
	case evpn && !x.EVPN:
		return "Established, but L2VPN EVPN is not up"
	}
	return ""
}

func linkName(f Interface) string {
	if f.Peer == "" {
		return f.Name
	}
	return f.Name + " → " + f.Peer
}

func has(m map[string]Session, k string) bool { _, ok := m[k]; return ok }

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func sortedVlans(v []Vlan) []Vlan {
	out := append([]Vlan(nil), v...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

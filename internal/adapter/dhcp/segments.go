package dhcp

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

// ordinalStride is the fabric generator's row stride: compile's rackOrdinal is
// row.Index*16 + position - 1, a pure function of where a rack stands, so adding a rack never
// moves another rack's cables. Segment addressing uses the same function, for the same reason:
// adding a rack never renumbers another rack's subnet.
const ordinalStride = 16

// Addressing schemes a network's segments can be numbered by. Each is a pure function of the
// hall layout (halls, rows, their declared positions) and the network's CIDR, never of which
// racks happen to exist, so adding or removing a rack renumbers nothing else.
const (
	// schemeOrdinal: segment i = the fabric generator's rack ordinal (row×16 + position − 1).
	schemeOrdinal = "ordinal"
	// schemeCompact: segment i = row×W + position − 1, W the widest row's declared positions.
	// Used when the CIDR is too small for the ordinal's 16 /24s per row: the shared lab estate's
	// 10.30.0.0/20 holds 16 /24s, and its rows declare 6+6+4 = 16 positions, but the ordinal of
	// its third row's racks reaches 34.
	schemeCompact = "compact"
	// schemeShared: the CIDR is smaller than a /24, so every rack shares it as one segment.
	schemeShared = "shared"
	// schemeNone: the network has no (IPv4) CIDR; the stack alone knows its subnets, and the
	// relay can send no link selection.
	schemeNone = "none"
)

// Segment is one modelled L2 segment of a DHCP network: one rack's ports on that network,
// behind the rack's model switches, relayed by one twin relay. Its subnet is a /24 of the
// network's CIDR chosen by the network's addressing scheme; the gateway is that /24's .1. A
// stack is handed these as its "BMC subnets" (02 — The software under test).
type Segment struct {
	Network string
	Rack    string // rack path
	Index   int    // position of the /24 in the CIDR; -1 when there is none
	Prefix  netip.Prefix
	Gateway netip.Addr
	// Switches are the model switches the segment's endpoint ports are cabled to, by path.
	Switches []string
}

// addressing is how one network is split into segments.
type addressing struct {
	net      *model.Network
	cidr     netip.Prefix
	scheme   string
	stride   int
	capacity int // /24 segments the CIDR holds
	hallBase map[*model.Hall]int
	why      string
}

func planAddressing(m *model.Model, n *model.Network) *addressing {
	a := &addressing{net: n, hallBase: map[*model.Hall]int{}}
	if n.CIDR == "" {
		a.scheme, a.why = schemeNone, "no cidr: the stack's own subnets apply, and the relay sends no link selection"
		return a
	}
	p, err := netip.ParsePrefix(n.CIDR)
	if err != nil || !p.Addr().Is4() {
		a.scheme, a.why = schemeNone, fmt.Sprintf("cidr %s is not IPv4 (DHCPv6 is owed)", n.CIDR)
		return a
	}
	a.cidr = p.Masked()
	if a.cidr.Bits() > 24 {
		a.scheme, a.capacity = schemeShared, 1
		a.why = fmt.Sprintf("%s is smaller than a /24: every rack shares it as one subnet", a.cidr)
		return a
	}
	a.capacity = 1 << (24 - a.cidr.Bits())
	// needed(w) is how many /24s numbering rows w apart takes: the furthest any declared
	// position reaches, over every hall.
	needed := func(w int) int {
		n, base := 0, 0
		for _, h := range m.Halls {
			for _, r := range h.Rows {
				if end := base + r.Index*w + r.Positions; end > n {
					n = end
				}
			}
			base += len(h.Rows) * w
		}
		return n
	}
	widest := 1
	for _, h := range m.Halls {
		for _, r := range h.Rows {
			if r.Positions > widest {
				widest = r.Positions
			}
		}
	}
	if widest <= ordinalStride && needed(ordinalStride) <= a.capacity {
		a.scheme, a.stride = schemeOrdinal, ordinalStride
		a.why = fmt.Sprintf("one /24 per rack at its rack ordinal (row×16+position−1, the fabric generator's function), %d of the %d /24s in %s",
			needed(ordinalStride), a.capacity, a.cidr)
	} else {
		a.scheme, a.stride = schemeCompact, widest
		a.why = fmt.Sprintf("one /24 per rack at row×%d+position−1 (%d, the widest row's positions, as the row stride): %s holds %d /24s, and the rack ordinal (row×16+position−1) would need %d",
			widest, widest, a.cidr, a.capacity, needed(ordinalStride))
		if n := needed(widest); n > a.capacity {
			a.why += fmt.Sprintf("; even this needs %d, so racks past the %dth /24 have no segment", n, a.capacity)
		}
	}
	base := 0
	for _, h := range m.Halls {
		a.hallBase[h] = base
		base += len(h.Rows) * a.stride
	}
	return a
}

// index is the rack's segment number: a pure function of where the rack stands.
func (a *addressing) index(r *model.Rack) int {
	switch a.scheme {
	case schemeShared:
		return 0
	case schemeNone:
		return -1
	}
	return a.hallBase[r.Hall] + r.Row.Index*a.stride + r.Position - 1
}

// subnet returns the rack's segment prefix and gateway, or an error when the CIDR cannot hold it.
func (a *addressing) subnet(r *model.Rack) (netip.Prefix, netip.Addr, error) {
	switch a.scheme {
	case schemeNone:
		return netip.Prefix{}, netip.Addr{}, nil
	case schemeShared:
		return a.cidr, a.cidr.Addr().Next(), nil
	}
	i := a.index(r)
	if i < 0 || i >= a.capacity {
		return netip.Prefix{}, netip.Addr{}, fmt.Errorf("rack %s: segment %d is outside %s (%d /24s)", r.Name, i, a.cidr, a.capacity)
	}
	b := a.cidr.Addr().As4()
	v := binary.BigEndian.Uint32(b[:]) + uint32(i)*256
	binary.BigEndian.PutUint32(b[:], v)
	pfx := netip.PrefixFrom(netip.AddrFrom4(b), 24)
	return pfx, pfx.Addr().Next(), nil
}

// serverID is the twin site DHCP's own address on a network: the first segment's .2, which no
// allocation hands out (.2–.9 of every segment are reserved).
func (a *addressing) serverID() netip.Addr {
	if !a.cidr.IsValid() {
		return netip.MustParseAddr("192.0.2.2") // TEST-NET-1: there is no CIDR to take one from
	}
	return a.cidr.Addr().Next().Next()
}

// PlanSegments computes every DHCP network's segments from the model: what a stack must be
// configured with (one subnet per segment, its gateway) for the twin relay's link selection to
// select it. It reads only compiled structure, never state.
func PlanSegments(m *model.Model) []Segment {
	var out []Segment
	names := make([]string, 0, len(m.Networks))
	for n := range m.Networks {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		n := m.Networks[name]
		if n.DHCP != "twin" && n.DHCP != "stack" {
			continue
		}
		a := planAddressing(m, n)
		seen := map[*model.Rack]*Segment{}
		var order []*model.Rack
		for _, p := range n.Ports {
			r := p.Device.RackOf()
			if r == nil || p.Link == nil {
				continue
			}
			s := seen[r]
			if s == nil {
				pfx, gw, err := a.subnet(r)
				if err != nil {
					continue
				}
				s = &Segment{Network: name, Rack: r.Path, Index: a.index(r), Prefix: pfx, Gateway: gw}
				seen[r] = s
				order = append(order, r)
			}
			sw := p.Link.Peer(p).Device.Path
			if !contains(s.Switches, sw) {
				s.Switches = append(s.Switches, sw)
			}
		}
		sort.Slice(order, func(i, j int) bool { return order[i].Path < order[j].Path })
		for _, r := range order {
			sort.Strings(seen[r].Switches)
			out = append(out, *seen[r])
		}
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- relay facts

// circuitID is what the twin relay inserts as option 82 sub-option 1, in the style of ISC
// dhcrelay as SONiC runs it (`-a %h:%p`): the switch's hostname and the ingress interface. A
// model switch's hostname is "<its rack>-<its device name>".
func circuitID(sw *model.Device, swPort *model.Port) string {
	host := sw.Name
	if r := sw.RackOf(); r != nil {
		host = r.Name + "-" + sw.Name
	}
	return host + ":" + swPort.Name
}

// switchMAC is the switch's system MAC: its management interface's, else its first Ethernet
// port's in profile order. It is the relay's remote id (sub-option 2), sent as text (raiBytes).
func switchMAC(sw *model.Device) net.HardwareAddr {
	if p := sw.PortBy["mgmt0"]; p != nil && p.MAC != "" {
		if hw, err := net.ParseMAC(p.MAC); err == nil {
			return hw
		}
	}
	for _, p := range sw.Ports {
		if p.Kind == "ethernet" && p.MAC != "" {
			if hw, err := net.ParseMAC(p.MAC); err == nil {
				return hw
			}
		}
	}
	return nil
}

var trailingNumber = regexp.MustCompile(`(\d+)$`)

// portNumber is the number a switch port's name ends in ("eth17" → 17), or 0.
func portNumber(name string) int {
	m := trailingNumber.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// portsOf is a switch's highest numbered front-panel Ethernet port: its port count, for laying
// out per-port addresses.
func portsOf(sw *model.Device) int {
	max := 0
	for _, p := range sw.Ports {
		if p.Kind != "ethernet" || p.Role == "management" || strings.HasPrefix(p.Name, "up") {
			continue
		}
		if n := portNumber(p.Name); n > max {
			max = n
		}
	}
	return max
}

// staticOffset is the host part the twin site DHCP gives the endpoint behind a switch port:
// 10 + the ports of the segment's earlier switches + the port's number. It is a pure function of
// the cabling, so a lease never depends on the order requests arrive in, and a device keeps its
// address when another is added. Zero means the port has no number to place it by.
func staticOffset(switches []*model.Device, sw *model.Device, swPort *model.Port) int {
	n := portNumber(swPort.Name)
	if n == 0 {
		return 0
	}
	off := 10
	for _, s := range switches {
		if s == sw {
			return off + n
		}
		off += portsOf(s)
	}
	return 0
}

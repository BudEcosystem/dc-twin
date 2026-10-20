package ibsim

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

// Node is one InfiniBand node of a rendered fabric: a switch, or one host channel adapter.
//
// Every InfiniBand port of a host is its own single-port channel adapter, as the ufm adapter
// numbers them (`HCA-<n>/1`): a ConnectX-7 or ConnectX-8 port in a GB200 or GB300 tray, or in an
// HGX server, is its own PCI function and its own HCA with one port.
type Node struct {
	// ID is the node's id in the topology file, ibnetdiscover's: S-<node GUID> for a switch,
	// H-<node GUID> for a channel adapter. It is the SIM_HOST a MAD client attaches at.
	ID     string
	Switch bool
	// GUID is the node GUID. A switch's is its system GUID (the GUID of its first InfiniBand
	// port, as the ufm adapter reports it); a channel adapter's is its port GUID minus one,
	// because ibsim answers NodeInfo with PortGUID = NodeGUID + port number (see PortGUID).
	GUID uint64
	// PortGUID is a switch's port 0 GUID (every port of a switch shares it) or a channel
	// adapter's port 1 GUID: the model port's GUID, which is what the ufm adapter reports,
	// partitions bind and a stack keys on.
	PortGUID uint64
	// SysImage is the system image GUID: a channel adapter carries its host's system GUID (the
	// GUID of the host's first InfiniBand port, the ufm adapter's systemID), a switch its own.
	SysImage uint64
	// Desc is the node description: the ufm adapter's node_description, the device's host name
	// (`<rack>-<device>`) for a switch and `<host name> HCA-<n>` for a channel adapter.
	Desc string
	// Ports is the number of external ports: a switch's highest port number, 1 for a CA.
	Ports int
	// Device is the model device; Port the model port of a channel adapter (empty for a switch).
	Device string
	Port   string
	// ModelGUID is the model's GUID string the port GUID came from.
	ModelGUID string
}

// End is one end of a link: a node, its port number and the model port.
type End struct {
	Node *Node
	Port int
	Ref  string
}

// Link is one cable of the fabric, as the model cables it.
type Link struct {
	ID string // the model link id
	// A is the switch end when the link has one, so a console command names the switch port
	// (the switch is what sends the SM its link-state trap).
	A, B End
	// Speed is the speed both ends train to, as ibsim knows speeds (SDR … NDR); ModelSpeed is
	// the model's (the lower of the two ports'), which differs when ibsim cannot simulate it.
	Speed, ModelSpeed string

	ma, mb *model.Port
}

// Topology is one InfiniBand fabric rendered for ibsim, a pure function of the model's
// structure: devices, their InfiniBand ports and GUIDs, and the fabric's cables. Operational
// state (a pulled cable, a powered-off host) is not in it; the adapter applies it through
// ibsim's console.
type Topology struct {
	Fabric string
	// SM is the subnet manager's point of attachment, first in the file so that a MAD client
	// with no SIM_HOST attaches there: the fabric manager appliance's HCA (the UFM appliance
	// runs the subnet manager on its own port), or the first channel adapter by device path
	// when no appliance is cabled to the fabric.
	SM    *Node
	Nodes []*Node // SM first, then switches by device path, then channel adapters by device path and port
	Links []*Link // in the model's link order
	// Notes are the approximations made rendering this fabric (speeds ibsim cannot simulate,
	// GUIDs longer than 64 bits). Empty when the file is the model exactly.
	Notes []string
}

// Counts returns the number of switches, channel adapters (ports) and links.
func (t *Topology) Counts() (switches, cas, links int) {
	for _, n := range t.Nodes {
		if n.Switch {
			switches++
		} else {
			cas++
		}
	}
	return switches, cas, len(t.Links)
}

// Sizes returns what ibsim must be able to hold: nodes, switches and ports (a switch has its
// external ports and port 0).
func (t *Topology) Sizes() (nodes, switches, ports int) {
	for _, n := range t.Nodes {
		if n.Switch {
			switches++
			ports += n.Ports + 1
		} else {
			ports += n.Ports
		}
	}
	return len(t.Nodes), switches, ports
}

// Fabrics lists the model's InfiniBand fabrics by name.
func Fabrics(m *model.Model) []string {
	var out []string
	for n, f := range m.Fabrics {
		if f.Technology == "infiniband" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Render renders one InfiniBand fabric of the model. The caller holds the engine's read lock
// (or owns the model).
func Render(m *model.Model, fabric string) (*Topology, error) {
	f := m.Fabrics[fabric]
	if f == nil {
		return nil, fmt.Errorf("no fabric %q", fabric)
	}
	if f.Technology != "infiniband" {
		return nil, fmt.Errorf("fabric %s is %s, not infiniband", fabric, f.Technology)
	}
	t := &Topology{Fabric: fabric}
	var truncated []string
	guidOf := func(s, what string) (uint64, error) {
		g, cut, err := guid64(s)
		if err != nil {
			return 0, fmt.Errorf("fabric %s: %s: %w", fabric, what, err)
		}
		if cut {
			truncated = append(truncated, what)
		}
		return g, nil
	}
	switches := map[*model.Device]*Node{}
	cas := map[*model.Port]*Node{}
	node := func(p *model.Port) (*Node, int, error) {
		d := p.Device
		if d.BaseClass == "switch" {
			number := trailingNumber(p.Name)
			n := switches[d]
			if n == nil {
				first := firstIBPort(d)
				if first == nil {
					return nil, 0, fmt.Errorf("fabric %s: switch %s has no InfiniBand port with a GUID", fabric, d.Path)
				}
				g, err := guidOf(first.GUID, first.Ref)
				if err != nil {
					return nil, 0, err
				}
				n = &Node{ID: fmt.Sprintf("S-%016x", g), Switch: true, GUID: g, PortGUID: g, SysImage: g,
					Desc: hostname(d), Ports: switchPorts(d), Device: d.Path, ModelGUID: first.GUID}
				switches[d] = n
			}
			if number < 1 || number > n.Ports {
				return nil, 0, fmt.Errorf("fabric %s: switch port %s has no port number in 1..%d", fabric, p.Ref, n.Ports)
			}
			return n, number, nil
		}
		if n := cas[p]; n != nil {
			return n, 1, nil
		}
		if p.GUID == "" {
			return nil, 0, fmt.Errorf("fabric %s: InfiniBand port %s has no GUID", fabric, p.Ref)
		}
		pg, err := guidOf(p.GUID, p.Ref)
		if err != nil {
			return nil, 0, err
		}
		sys := pg
		if first := firstIBPort(d); first != nil && first != p {
			if sys, err = guidOf(first.GUID, first.Ref); err != nil {
				return nil, 0, err
			}
		}
		n := &Node{ID: fmt.Sprintf("H-%016x", pg-1), GUID: pg - 1, PortGUID: pg, SysImage: sys,
			Desc: hostname(d) + " HCA-" + strconv.Itoa(hcaIndex(d, p)), Ports: 1, Device: d.Path, Port: p.Ref, ModelGUID: p.GUID}
		cas[p] = n
		return n, 1, nil
	}
	fast := 0
	for _, l := range m.Links {
		if l.Fabric != fabric || l.A == nil || l.B == nil || l.A.Kind != "infiniband" || l.B.Kind != "infiniband" {
			continue
		}
		a, b := l.A, l.B
		if b.Device.BaseClass == "switch" && a.Device.BaseClass != "switch" {
			a, b = b, a
		}
		na, pa, err := node(a)
		if err != nil {
			return nil, err
		}
		nb, pb, err := node(b)
		if err != nil {
			return nil, err
		}
		speed := activeSpeed(a.Speed, b.Speed)
		sim := speed
		if speedRank[sim] > speedRank[maxSimSpeed] {
			sim = maxSimSpeed
			fast++
		}
		t.Links = append(t.Links, &Link{ID: l.ID, A: End{na, pa, a.Ref}, B: End{nb, pb, b.Ref}, Speed: sim, ModelSpeed: speed, ma: a, mb: b})
	}
	if len(t.Links) == 0 {
		return nil, fmt.Errorf("fabric %s has no InfiniBand links", fabric)
	}
	var sw, ca []*Node
	for _, n := range switches {
		sw = append(sw, n)
	}
	for _, n := range cas {
		ca = append(ca, n)
	}
	sort.Slice(sw, func(i, j int) bool { return sw[i].Device < sw[j].Device })
	sort.Slice(ca, func(i, j int) bool {
		if ca[i].Device != ca[j].Device {
			return ca[i].Device < ca[j].Device
		}
		return portIndex(m, ca[i].Port) < portIndex(m, ca[j].Port)
	})
	for _, n := range ca {
		if d := m.DeviceBy[n.Device]; d != nil && d.BaseClass == "appliance" {
			t.SM = n
			break
		}
	}
	if t.SM == nil && len(ca) > 0 {
		t.SM = ca[0]
	}
	if t.SM != nil {
		t.Nodes = append(t.Nodes, t.SM)
	}
	t.Nodes = append(t.Nodes, sw...)
	for _, n := range ca {
		if n != t.SM {
			t.Nodes = append(t.Nodes, n)
		}
	}
	// ibsim refuses a node GUID it has seen; a subnet manager refuses a port GUID it has seen
	nodeGUIDs, portGUIDs := map[uint64]string{}, map[uint64]string{}
	for _, n := range t.Nodes {
		where := n.Device
		if n.Port != "" {
			where = n.Port
		}
		if other, ok := nodeGUIDs[n.GUID]; ok {
			return nil, fmt.Errorf("fabric %s: %s and %s render the same node GUID 0x%016x", fabric, other, where, n.GUID)
		}
		nodeGUIDs[n.GUID] = where
		if other, ok := portGUIDs[n.PortGUID]; ok {
			return nil, fmt.Errorf("fabric %s: %s and %s render the same port GUID 0x%016x", fabric, other, where, n.PortGUID)
		}
		portGUIDs[n.PortGUID] = where
	}
	if len(truncated) > 0 {
		sort.Strings(truncated)
		t.Notes = append(t.Notes, fmt.Sprintf("%d model GUID(s) are longer than 64 bits (the profile's guidPrefix and identity/v1's five derived bytes make more than eight bytes); the leading 64 bits are used, e.g. %s", len(truncated), truncated[0]))
	}
	if fast > 0 {
		t.Notes = append(t.Notes, fmt.Sprintf("%d link(s) train above %s in the model; ibsim simulates at most %s, so they are written as %s", fast, maxSimSpeed, maxSimSpeed, maxSimSpeed))
	}
	return t, nil
}

// Text renders the topology in ibnetdiscover's output format, which ibsim reads: a record per
// node, the SM's channel adapter first, then switches, then channel adapters. Every LID is 0
// (the subnet manager assigns them) and every link is cabled; a link the model has down is
// unlinked through the console once ibsim runs.
func (t *Topology) Text() []byte {
	var b strings.Builder
	sw, ca, links := t.Counts()
	fmt.Fprintf(&b, "#\n# Topology of InfiniBand fabric %s in ibnetdiscover's format, rendered by dc-twin from the\n", t.Fabric)
	fmt.Fprintf(&b, "# estate's model for ibsim: %d switches, %d channel adapter ports, %d links.\n", sw, ca, links)
	b.WriteString("# A channel adapter's node GUID is its port GUID minus one: ibsim answers PortGUID = NodeGUID +\n")
	b.WriteString("# port number. Its port GUID is the model's.\n")
	for _, n := range t.Notes {
		// ibsim reads a line into 256 bytes and parses what is left over as a line of its own,
		// so a long comment could become a directive ("do …", "Ca …"): every line stays short
		for _, l := range wrap("Note: "+n+".", 96) {
			fmt.Fprintf(&b, "# %s\n", l)
		}
	}
	if t.SM != nil {
		fmt.Fprintf(&b, "# Subnet manager attachment (SIM_HOST): %s, %s.\n", t.SM.ID, t.SM.Port)
	}
	b.WriteString("#\n")
	ports := map[*Node]map[int]*Link{}
	for _, l := range t.Links {
		for _, e := range []End{l.A, l.B} {
			if ports[e.Node] == nil {
				ports[e.Node] = map[int]*Link{}
			}
			ports[e.Node][e.Port] = l
		}
	}
	for _, n := range t.Nodes {
		b.WriteString("\n")
		if n.Switch {
			fmt.Fprintf(&b, "switchguid=0x%016x(%x)\n", n.GUID, n.PortGUID)
			fmt.Fprintf(&b, "Switch\t%d %q\t\t# %q enhanced port 0 lid 0 lmc 0\n", n.Ports, n.ID, n.Desc)
		} else {
			fmt.Fprintf(&b, "sysimgguid=0x%016x\n", n.SysImage)
			fmt.Fprintf(&b, "caguid=0x%016x\n", n.GUID)
			fmt.Fprintf(&b, "Ca\t%d %q\t\t# %q\n", n.Ports, n.ID, n.Desc)
		}
		var nums []int
		for p := range ports[n] {
			nums = append(nums, p)
		}
		sort.Ints(nums)
		for _, p := range nums {
			l := ports[n][p]
			peer := l.B
			if l.B.Node == n && l.B.Port == p {
				peer = l.A
			}
			remote := fmt.Sprintf("%q[%d]", peer.Node.ID, peer.Port)
			if !peer.Node.Switch {
				remote += fmt.Sprintf("(%x)", peer.Node.PortGUID)
			}
			if n.Switch {
				fmt.Fprintf(&b, "[%d]\t%s \t\t# %q lid 0 4x%s\n", p, remote, peer.Node.Desc, l.Speed)
			} else {
				fmt.Fprintf(&b, "[%d](%x) \t%s\t\t# lid 0 lmc 0 %q lid 0 4x%s\n", p, n.PortGUID, remote, peer.Node.Desc, l.Speed)
			}
		}
	}
	return []byte(b.String())
}

func wrap(s string, width int) []string {
	var out []string
	line := ""
	for _, w := range strings.Fields(s) {
		if line != "" && len(line)+1+len(w) > width {
			out = append(out, line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += w
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}

// ---- the model's naming, as the ufm adapter names it (internal/adapter/ufm/topology.go), so
// the subnet manager and UFM REST describe the same node with the same words

// hostname is the name a device reports on the fabric: rack and slot, unique in the estate.
func hostname(d *model.Device) string {
	if r := d.RackOf(); r != nil && d.Parent == nil {
		return r.Name + "-" + d.Name
	}
	if d.Parent != nil {
		return hostname(d.Parent) + "-" + d.Name
	}
	return d.Name
}

// hcaIndex numbers a host's InfiniBand ports from 1 in the device's port order.
func hcaIndex(d *model.Device, p *model.Port) int {
	n := 0
	for _, x := range d.Ports {
		if x.Kind == "infiniband" {
			n++
		}
		if x == p {
			break
		}
	}
	return n
}

func firstIBPort(d *model.Device) *model.Port {
	for _, x := range d.Ports {
		if x.Kind == "infiniband" && x.GUID != "" {
			return x
		}
	}
	return nil
}

// switchPorts is a switch's port count: its highest InfiniBand port number (ib1..ib64: 64).
func switchPorts(d *model.Device) int {
	n := 0
	for _, x := range d.Ports {
		if x.Kind == "infiniband" {
			if k := trailingNumber(x.Name); k > n {
				n = k
			}
		}
	}
	return n
}

func trailingNumber(name string) int {
	i := len(name)
	for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(name[i:])
	return n
}

func portIndex(m *model.Model, ref string) int {
	p := m.Ports[ref]
	if p == nil {
		return -1
	}
	for i, x := range p.Device.Ports {
		if x == p {
			return i
		}
	}
	return -1
}

// guid64 parses a model GUID ("0x" and hex digits). InfiniBand GUIDs are EUI-64: a GUID with
// more than 16 hex digits is cut to its leading 64 bits, which keep the profile's prefix (an
// OUI and the vendor's byte after it), and cut reports it.
func guid64(s string) (g uint64, cut bool, err error) {
	h := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x")
	if len(h) > 16 {
		h, cut = h[:16], true
	}
	g, err = strconv.ParseUint(h, 16, 64)
	if err != nil || h == "" {
		return 0, false, fmt.Errorf("GUID %q is not hexadecimal", s)
	}
	return g, cut, nil
}

// ---- speeds

// maxSimSpeed is the fastest speed ibsim 0.12 parses from a topology (sim_net.c).
const maxSimSpeed = "NDR"

var speedRank = map[string]int{"SDR": 1, "DDR": 2, "QDR": 3, "FDR": 4, "EDR": 5, "HDR": 6, "NDR": 7, "XDR": 8}

// ibSpeed names a 4x port's speed as InfiniBand does, as the ufm adapter does.
func ibSpeed(speed string) string {
	switch strings.ToUpper(speed) {
	case "800G":
		return "XDR"
	case "400G":
		return "NDR"
	case "200G":
		return "HDR"
	case "100G":
		return "EDR"
	case "56G":
		return "FDR"
	case "40G":
		return "QDR"
	case "20G":
		return "DDR"
	case "10G":
		return "SDR"
	}
	return ""
}

// activeSpeed is the speed a link trains to: the lower of its two ports' (a port with no
// speed in the model leaves it to the other; neither: SDR, ibsim's default).
func activeSpeed(a, b string) string {
	sa, sb := ibSpeed(a), ibSpeed(b)
	switch {
	case sa == "" && sb == "":
		return "SDR"
	case sa == "":
		return sb
	case sb == "":
		return sa
	case speedRank[sa] <= speedRank[sb]:
		return sa
	}
	return sb
}

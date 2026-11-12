package day0

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WriteInputs writes the design into dir (<state>/stack-inputs/day0/) as a site hands it to a
// day-0 tool (18 — What the SUT is given, D77):
//
//   - expected-switches.json: one entry per switch, its identity and what it should become;
//   - cabling.csv: every cable of the design, both ends by device and port, the model's name and
//     the NOS's where an end is a real switch;
//   - topology.dot: the switch-to-switch cables in the form Cumulus's PTM reads;
//   - plan.json, only with plan: given: the address plan the twin would have applied itself;
//   - netbox/: the same design as NetBox bulk-import CSVs, with a README naming the version and
//     the import order.
//
// Every file is written in a fixed order (switches by path, cables by id), so the same estate
// gives the same bytes and a tool's fixtures can be diffed. With plan: tool a plan.json left by
// an earlier run is removed, so the tool cannot read a plan it was not given.
func WriteInputs(dir string, d Design) error {
	d = sorted(d)
	if err := os.MkdirAll(filepath.Join(dir, "netbox"), 0o755); err != nil {
		return err
	}
	files := map[string][]byte{}
	var err error
	if files["expected-switches.json"], err = expectedSwitches(d); err != nil {
		return err
	}
	files["cabling.csv"] = cablingCSV(d)
	files["topology.dot"] = topologyDOT(d)
	if d.Plan == "given" {
		if files["plan.json"], err = planJSON(d); err != nil {
			return err
		}
	} else if err := os.Remove(filepath.Join(dir, "plan.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	for name, data := range netboxCSVs(d) {
		files[filepath.Join("netbox", name)] = data
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), files[n], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// sorted is a copy of the design in output order.
func sorted(d Design) Design {
	d.Switches = append([]Switch(nil), d.Switches...)
	d.Cables = append([]Cable(nil), d.Cables...)
	d.Mgmt = append([]MgmtNet(nil), d.Mgmt...)
	sort.SliceStable(d.Switches, func(i, j int) bool { return d.Switches[i].Path < d.Switches[j].Path })
	sort.SliceStable(d.Cables, func(i, j int) bool { return d.Cables[i].ID < d.Cables[j].ID })
	sort.SliceStable(d.Mgmt, func(i, j int) bool { return d.Mgmt[i].Network < d.Mgmt[j].Network })
	return d
}

// ExpectedSwitch is one entry of expected-switches.json: what a site's inventory says of a
// switch before it is powered on, as expected-machines.json says it of a machine.
type ExpectedSwitch struct {
	Path         string `json:"path"`
	Hostname     string `json:"hostname"`
	Role         string `json:"role"`
	Fabric       string `json:"fabric"`
	Rack         string `json:"rack,omitempty"`
	U            int    `json:"u,omitempty"`
	Profile      string `json:"profile"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Model        string `json:"model,omitempty"`
	PartNumber   string `json:"partNumber,omitempty"`
	Platform     string `json:"platform,omitempty"` // the ONIE platform string
	Serial       string `json:"serial"`
	BaseMAC      string `json:"baseMAC"`
	MgmtMAC      string `json:"mgmtMAC,omitempty"`
	NOS          string `json:"nos"`
	Factory      bool   `json:"factory"`
}

func expectedSwitches(d Design) ([]byte, error) {
	out := make([]ExpectedSwitch, 0, len(d.Switches))
	for _, s := range d.Switches {
		out = append(out, ExpectedSwitch{Path: s.Path, Hostname: s.Hostname, Role: s.Role, Fabric: s.Fabric, Rack: s.Rack, U: s.U,
			Profile: s.Profile, Manufacturer: s.Manufacturer, Model: s.Model, PartNumber: s.PartNumber, Platform: s.Platform,
			Serial: s.Serial, BaseMAC: s.BaseMAC, MgmtMAC: s.MgmtMAC, NOS: s.NOS, Factory: s.Factory})
	}
	return jsonBytes(out)
}

func cablingCSV(d Design) []byte {
	rows := [][]string{{"id", "fabric", "a_device", "a_port", "a_interface", "b_device", "b_port", "b_interface", "speed"}}
	for _, c := range d.Cables {
		rows = append(rows, []string{c.ID, c.Fabric, c.ADevice, c.APort, c.AIf, c.BDevice, c.BPort, c.BIf, c.Speed})
	}
	return csvBytes(rows)
}

// topologyDOT is the cabling plan as PTM's topology file: an undirected graph whose edges are
// "<hostname>":"<port>" pairs, ports as the NOS names them. PTM checks only switch-to-switch
// cables it can see LLDP on, so a cable with an end that is not a real switch, or that has no
// NOS name for a port, is left out (it is in cabling.csv).
func topologyDOT(d Design) []byte {
	hosts := map[string]bool{}
	for _, s := range d.Switches {
		hosts[s.Hostname] = true
	}
	var b bytes.Buffer
	b.WriteString("graph G {\n")
	for _, c := range d.Cables {
		if !hosts[c.ADevice] || !hosts[c.BDevice] || c.AIf == "" || c.BIf == "" {
			continue
		}
		fmt.Fprintf(&b, "  %q:%q -- %q:%q;\n", c.ADevice, c.AIf, c.BDevice, c.BIf)
	}
	b.WriteString("}\n")
	return b.Bytes()
}

// Plan is plan.json: the address plan the twin would have applied itself (plan: given).
type Plan struct {
	Estate   string       `json:"estate"`
	Site     string       `json:"site,omitempty"`
	Switches []PlanSwitch `json:"switches"`
	Mgmt     []MgmtNet    `json:"managementNetworks,omitempty"`
}

// PlanSwitch is one switch's part of the address plan.
type PlanSwitch struct {
	Path       string      `json:"path"`
	Hostname   string      `json:"hostname"`
	Serial     string      `json:"serial"`
	Role       string      `json:"role"`
	ASN        uint32      `json:"asn,omitempty"`
	RouterID   string      `json:"routerId,omitempty"`
	Loopback   string      `json:"loopback,omitempty"`
	EVPN       bool        `json:"evpn"`
	Interfaces []Interface `json:"interfaces"`
	Vlans      []Vlan      `json:"vlans"`
}

func planJSON(d Design) ([]byte, error) {
	p := Plan{Estate: d.Estate, Site: d.Site, Mgmt: d.Mgmt, Switches: []PlanSwitch{}}
	for _, s := range d.Switches {
		ifs := append([]Interface{}, s.Interfaces...)
		sort.SliceStable(ifs, func(i, j int) bool { return portLess(ifs[i].Name, ifs[j].Name) })
		vl := append([]Vlan{}, s.Vlans...)
		sort.SliceStable(vl, func(i, j int) bool { return vl[i].ID < vl[j].ID })
		if ifs == nil {
			ifs = []Interface{}
		}
		if vl == nil {
			vl = []Vlan{}
		}
		p.Switches = append(p.Switches, PlanSwitch{Path: s.Path, Hostname: s.Hostname, Serial: s.Serial, Role: s.Role, ASN: s.ASN,
			RouterID: s.RouterID, Loopback: s.Loopback, EVPN: s.EVPN, Interfaces: ifs, Vlans: vl})
	}
	return jsonBytes(p)
}

// portLess orders port names as a person reads them: swp2 before swp10, Ethernet4 before
// Ethernet16.
func portLess(a, b string) bool {
	sa, na := splitNum(a)
	sb, nb := splitNum(b)
	if sa != sb {
		return sa < sb
	}
	if na != nb {
		return na < nb
	}
	return a < b
}

func splitNum(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	n := 0
	for _, c := range s[i:] {
		n = n*10 + int(c-'0')
	}
	if i == len(s) {
		n = -1
	}
	return s[:i], n
}

func jsonBytes(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func csvBytes(rows [][]string) []byte {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	_ = w.WriteAll(rows)
	return b.Bytes()
}

// slug is NetBox's slug of a name: lower case, letters, digits, - and _.
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
			b.WriteRune(c)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

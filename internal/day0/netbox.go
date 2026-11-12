package day0

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// NetBoxVersion is the NetBox release the bulk-import CSVs are written for. The column names are
// the fields of NetBox's import forms (netbox/dcim/forms/bulk_import.py and
// netbox/ipam/forms/bulk_import.py; TestNetBoxImportForms checks them against the v4.2.9 tag's,
// testdata/netbox-import-4.2.9.json): MAC addresses are their own objects
// since 4.2, so they are in mac_addresses.csv, not a column of interfaces.csv. Only columns that
// every 4.2 and later release accepts are written (18 — What the SUT is given; the stack
// definition pins the version a stack runs, D77).
const NetBoxVersion = "4.2"

// ASNField is the device custom field that holds a switch's AS number (custom_fields.csv, the
// devices' cf_bgp_asn column).
const ASNField = "bgp_asn"

// mgmtRD is the management VRF's route distinguisher, which interfaces.csv names it by. NetBox
// keeps an RD as free text; the VRF never leaves the switch, so it is its name.
const mgmtRD = "mgmt"

// netboxOrder is the order the files import in: each names objects only the files before it make.
var netboxOrder = []string{"custom_fields.csv", "sites.csv", "racks.csv", "manufacturers.csv", "platforms.csv", "device_types.csv", "device_roles.csv",
	"devices.csv", "vrfs.csv", "interfaces.csv", "mac_addresses.csv", "cables.csv", "rirs.csv", "asns.csv", "vlans.csv",
	"prefixes.csv", "ip_addresses.csv"}

// netboxCSVs renders the design as NetBox bulk-import CSVs, with a README.md. Devices and cables
// are imported as planned (status planned): on day 0 nothing has been seen yet. Only
// switch-to-switch cables with both ports named by their NOS are written, since a cable's ends
// must be interfaces NetBox already holds; cabling.csv has every cable. Prefixes carry no scope:
// a scope is a NetBox primary key or, in later releases only, a name.
func netboxCSVs(d Design) map[string][]byte {
	site := d.Site
	if site == "" {
		site = d.Estate
	}
	out := map[string][]byte{}
	// a device's AS number: NetBox relates an ASN to sites and tenants, not to a device, so the
	// per-switch AS of an eBGP fabric is a device custom field, as sites commonly keep it
	// search_weight, filter_logic and weight are the model's defaults: NetBox's import form requires
	// every column its model does not let be blank, defaults or not
	out["custom_fields.csv"] = csvBytes([][]string{{"name", "label", "type", "object_types", "description", "search_weight", "filter_logic", "weight"},
		{ASNField, "BGP ASN", "integer", "dcim.device", "The switch's BGP autonomous system (stack-inputs/day0 plan.json)", "1000", "loose", "100"}})
	out["sites.csv"] = csvBytes([][]string{{"name", "slug", "status"}, {site, slug(site), "active"}})

	// racks, manufacturers, platforms, device types and roles: each once, by name
	racks := map[string]int{}
	mans := map[string]bool{}
	plats := map[string]bool{}
	type dtype struct{ man, model, part string }
	dtypes := map[string]dtype{}
	roles := map[string]bool{}
	for _, s := range d.Switches {
		if s.Rack != "" {
			racks[s.Rack] = max(racks[s.Rack], s.U)
		}
		mans[manufacturer(s)] = true
		if s.NOS != "" {
			plats[s.NOS] = true
		}
		dtypes[manufacturer(s)+"\x00"+model(s)] = dtype{manufacturer(s), model(s), s.PartNumber}
		roles[s.Role] = true
	}
	rows := [][]string{{"site", "name", "status", "width", "u_height"}}
	for _, r := range keys(racks) {
		h := 42
		if racks[r] > 42 {
			h = 48
		}
		if racks[r] > 48 {
			h = racks[r]
		}
		rows = append(rows, []string{site, r, "active", "19", strconv.Itoa(h)})
	}
	out["racks.csv"] = csvBytes(rows)
	rows = [][]string{{"name", "slug"}}
	for _, m := range keys(mans) {
		rows = append(rows, []string{m, slug(m)})
	}
	out["manufacturers.csv"] = csvBytes(rows)
	rows = [][]string{{"name", "slug"}}
	for _, p := range keys(plats) {
		rows = append(rows, []string{p, slug(p)})
	}
	out["platforms.csv"] = csvBytes(rows)
	rows = [][]string{{"manufacturer", "model", "slug", "part_number", "u_height"}}
	for _, k := range keys(dtypes) {
		t := dtypes[k]
		rows = append(rows, []string{t.man, t.model, slug(t.model), t.part, "1"})
	}
	out["device_types.csv"] = csvBytes(rows)
	rows = [][]string{{"name", "slug", "color"}}
	for _, r := range keys(roles) {
		rows = append(rows, []string{r, slug(r), roleColor(r)})
	}
	out["device_roles.csv"] = csvBytes(rows)

	// devices
	rows = [][]string{{"name", "role", "manufacturer", "device_type", "platform", "serial", "status", "site", "rack", "position", "face", "description", "cf_" + ASNField}}
	for _, s := range d.Switches {
		pos, face := "", ""
		if s.Rack != "" && s.U > 0 {
			pos, face = strconv.Itoa(s.U), "front"
		}
		asn := ""
		if s.ASN != 0 {
			asn = strconv.FormatUint(uint64(s.ASN), 10)
		}
		rows = append(rows, []string{s.Hostname, s.Role, manufacturer(s), model(s), s.NOS, s.Serial, "planned", site, s.Rack, pos, face, s.Path, asn})
	}
	out["devices.csv"] = csvBytes(rows)
	// an interface's VRF is looked up by its route distinguisher (InterfaceImportForm's vrf has
	// to_field_name rd), a prefix's and an address's by its name: the VRF has both
	vrfs := [][]string{{"name", "rd"}}
	if len(d.Mgmt) > 0 {
		vrfs = append(vrfs, []string{"mgmt", mgmtRD})
	}
	out["vrfs.csv"] = csvBytes(vrfs)

	// interfaces: the management port, the loopback, every cabled port, every VLAN interface
	hosts := map[string]*Switch{}
	for i := range d.Switches {
		hosts[d.Switches[i].Hostname] = &d.Switches[i]
	}
	type iface struct{ name, typ, speed, vrf, desc string }
	ifs := map[string]map[string]iface{}
	add := func(host string, f iface) {
		if ifs[host] == nil {
			ifs[host] = map[string]iface{}
		}
		if old, ok := ifs[host][f.name]; ok && old.speed != "" && f.speed == "" {
			f.speed = old.speed
		}
		ifs[host][f.name] = f
	}
	for _, s := range d.Switches {
		mgmtVRF := ""
		if len(d.Mgmt) > 0 {
			mgmtVRF = mgmtRD
		}
		add(s.Hostname, iface{name: "eth0", typ: "1000base-t", speed: "1000000", vrf: mgmtVRF, desc: "management"})
		if s.Loopback != "" {
			add(s.Hostname, iface{name: "lo", typ: "virtual", desc: "loopback"})
		}
		for _, f := range s.Interfaces {
			desc := f.Kind
			if f.Peer != "" {
				desc += " to " + f.Peer
			}
			add(s.Hostname, iface{name: f.Name, typ: ifType(f.Speed), speed: kbps(f.Speed), desc: desc})
		}
		for _, v := range s.Vlans {
			add(s.Hostname, iface{name: v.Name, typ: "virtual", desc: v.Network})
		}
	}
	for _, c := range d.Cables {
		if hosts[c.ADevice] != nil && c.AIf != "" {
			if _, ok := ifs[c.ADevice][c.AIf]; !ok {
				add(c.ADevice, iface{name: c.AIf, typ: ifType(c.Speed), speed: kbps(c.Speed)})
			}
		}
		if hosts[c.BDevice] != nil && c.BIf != "" {
			if _, ok := ifs[c.BDevice][c.BIf]; !ok {
				add(c.BDevice, iface{name: c.BIf, typ: ifType(c.Speed), speed: kbps(c.Speed)})
			}
		}
	}
	rows = [][]string{{"device", "name", "type", "speed", "enabled", "mgmt_only", "vrf", "description"}}
	macs := [][]string{{"mac_address", "device", "interface", "is_primary"}}
	for _, s := range d.Switches {
		names := make([]string, 0, len(ifs[s.Hostname]))
		for n := range ifs[s.Hostname] {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool { return portLess(names[i], names[j]) })
		for _, n := range names {
			f := ifs[s.Hostname][n]
			mgmt := "false"
			if n == "eth0" {
				mgmt = "true"
			}
			rows = append(rows, []string{s.Hostname, n, f.typ, f.speed, "true", mgmt, f.vrf, f.desc})
		}
		if s.MgmtMAC != "" {
			macs = append(macs, []string{strings.ToUpper(s.MgmtMAC), s.Hostname, "eth0", "true"})
		}
	}
	out["interfaces.csv"] = csvBytes(rows)
	out["mac_addresses.csv"] = csvBytes(macs)

	rows = [][]string{{"side_a_device", "side_a_type", "side_a_name", "side_b_device", "side_b_type", "side_b_name", "status", "label"}}
	for _, c := range d.Cables {
		if hosts[c.ADevice] == nil || hosts[c.BDevice] == nil || c.AIf == "" || c.BIf == "" {
			continue
		}
		rows = append(rows, []string{c.ADevice, "dcim.interface", c.AIf, c.BDevice, "dcim.interface", c.BIf, "planned", c.ID})
	}
	out["cables.csv"] = csvBytes(rows)

	// AS numbers, from a private RIR
	asns := map[uint32][]string{}
	for _, s := range d.Switches {
		if s.ASN != 0 {
			asns[s.ASN] = append(asns[s.ASN], s.Hostname)
		}
	}
	rirs := [][]string{{"name", "slug", "is_private"}}
	if len(asns) > 0 {
		rirs = append(rirs, []string{"private", "private", "true"})
	}
	out["rirs.csv"] = csvBytes(rirs)
	rows = [][]string{{"asn", "rir", "description"}}
	nums := make([]uint32, 0, len(asns))
	for n := range asns {
		nums = append(nums, n)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	for _, n := range nums {
		rows = append(rows, []string{strconv.FormatUint(uint64(n), 10), "private", strings.Join(asns[n], " ")})
	}
	out["asns.csv"] = csvBytes(rows)

	// VLANs, prefixes and addresses
	vlans := map[int]string{}
	prefixes := map[string][2]string{} // prefix -> vrf, description
	gwCount := map[string]int{}
	for _, s := range d.Switches {
		for _, v := range s.Vlans {
			vlans[v.ID] = v.Name
			// a segment's network is a prefix, or the name of an estate network (oob), which is
			// no prefix NetBox could hold
			if p, err := netip.ParsePrefix(v.Network); err == nil {
				prefixes[p.Masked().String()] = [2]string{"", v.Name}
			}
			gwCount[v.Gateway]++
		}
		for _, f := range s.Interfaces {
			if p, err := netip.ParsePrefix(f.Address); err == nil {
				prefixes[p.Masked().String()] = [2]string{"", "fabric link " + s.Hostname + " " + f.Name}
			}
		}
	}
	for _, m := range d.Mgmt {
		net := m.Prefix
		if net == "" {
			net = m.Network
		}
		if _, err := netip.ParsePrefix(net); err == nil {
			prefixes[net] = [2]string{"mgmt", "management " + m.Rack}
		}
	}
	rows = [][]string{{"site", "vid", "name", "status"}}
	ids := make([]int, 0, len(vlans))
	for id := range vlans {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		rows = append(rows, []string{site, strconv.Itoa(id), vlans[id], "active"})
	}
	out["vlans.csv"] = csvBytes(rows)
	rows = [][]string{{"prefix", "vrf", "status", "description"}}
	for _, p := range keys(prefixes) {
		rows = append(rows, []string{p, prefixes[p][0], "active", prefixes[p][1]})
	}
	out["prefixes.csv"] = csvBytes(rows)
	rows = [][]string{{"address", "status", "role", "device", "interface", "is_primary", "description"}}
	for _, s := range d.Switches {
		if s.Loopback != "" {
			rows = append(rows, []string{withLen(s.Loopback, 32), "active", "loopback", s.Hostname, "lo", "true", "router id " + s.RouterID})
		}
		ifs := append([]Interface{}, s.Interfaces...)
		sort.SliceStable(ifs, func(i, j int) bool { return portLess(ifs[i].Name, ifs[j].Name) })
		for _, f := range ifs {
			if f.Address != "" {
				rows = append(rows, []string{f.Address, "active", "", s.Hostname, f.Name, "", f.Peer})
			}
		}
		for _, v := range s.Vlans {
			if v.Gateway == "" {
				continue
			}
			role := ""
			if gwCount[v.Gateway] > 1 {
				role = "anycast" // the same gateway on every leaf serving the segment
			}
			rows = append(rows, []string{v.Gateway, "active", role, s.Hostname, v.Name, "", "gateway"})
		}
	}
	out["ip_addresses.csv"] = csvBytes(rows)

	var b strings.Builder
	fmt.Fprintf(&b, "# NetBox import: %s\n\n", d.Estate)
	fmt.Fprintf(&b, "The day-0 design of this estate as NetBox %s bulk-import CSVs (Import → CSV, one file per\n", NetBoxVersion)
	b.WriteString("object type, or `POST /api/<app>/<model>/` with the rows as JSON). The columns are those NetBox's\n")
	b.WriteString("import forms accept from 4.2 on: MAC addresses are objects of their own, imported after the\n")
	b.WriteString("interfaces they belong to.\n\n")
	b.WriteString("Devices and cables are `planned`. Only switch-to-switch cables are here; `../cabling.csv` has\n")
	b.WriteString("every cable. Prefixes carry no scope. Import in this order:\n\n")
	for i, n := range netboxOrder {
		fmt.Fprintf(&b, "%d. `%s`\n", i+1, n)
	}
	out["README.md"] = []byte(b.String())
	return out
}

func manufacturer(s Switch) string {
	if s.Manufacturer != "" {
		return s.Manufacturer
	}
	return "Generic"
}

func model(s Switch) string {
	if s.Model != "" {
		return s.Model
	}
	return s.Profile
}

func roleColor(role string) string {
	switch role {
	case "spine":
		return "2196f3"
	case "super-spine":
		return "3f51b5"
	case "leaf":
		return "4caf50"
	case "oob":
		return "9e9e9e"
	}
	return "607d8b"
}

// ifType is NetBox's interface type for a port of a speed.
func ifType(speed string) string {
	switch strings.ToUpper(speed) {
	case "1G":
		return "1000base-t"
	case "10G":
		return "10gbase-x-sfpp"
	case "25G":
		return "25gbase-x-sfp28"
	case "40G":
		return "40gbase-x-qsfpp"
	case "50G":
		return "50gbase-x-sfp56"
	case "100G":
		return "100gbase-x-qsfp28"
	case "200G":
		return "200gbase-x-qsfp56"
	case "400G":
		return "400gbase-x-qsfpdd"
	case "800G":
		return "800gbase-x-osfp"
	}
	return "other"
}

// kbps is a speed as NetBox holds it, in kbit/s ("" when unknown).
func kbps(speed string) string {
	s := strings.ToUpper(strings.TrimSpace(speed))
	mult := map[byte]float64{'M': 1e3, 'G': 1e6, 'T': 1e9}
	if s == "" {
		return ""
	}
	m, ok := mult[s[len(s)-1]]
	if !ok {
		return ""
	}
	n, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return ""
	}
	return strconv.FormatInt(int64(n*m), 10)
}

// withLen gives an address its prefix length when it has none.
func withLen(a string, n int) string {
	if strings.Contains(a, "/") || a == "" {
		return a
	}
	return fmt.Sprintf("%s/%d", a, n)
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

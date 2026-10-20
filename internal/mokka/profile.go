package mokka

import (
	"fmt"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Profile is a host's rendered Mokka GPU profile: a complete nvml-mock config document, which the
// chart takes as gpu.customConfig (it replaces the selected profile whole, never merges).
type Profile struct {
	Host   Host
	Base   string // the Mokka profile it was rendered over (gb300, h100, …)
	Config []byte // the config.yaml
	// Notes say where the base's value was kept because the model has none, and what of the
	// base was removed because it would describe hardware the twin does not model.
	Notes []string
}

// bases maps an NVML name to the Mokka profile (deployments/nvml-mock/helm/nvml-mock/profiles at
// v0.5.0) of the same silicon. Order matters: GB300 before B300-like substrings, GB200 before
// B200.
var bases = []struct{ match, base string }{
	{"GB300", "gb300"}, {"GB200", "gb200"}, {"B200", "b200"},
	{"H100", "h100"}, {"A100", "a100"}, {"L40S", "l40s"}, {"T4", "t4"},
}

// BaseFor names the Mokka profile a product renders over, or "" when Mokka ships none for it.
func BaseFor(product string) string {
	p := strings.ToUpper(product)
	for _, b := range bases {
		if strings.Contains(p, b.match) {
			return b.base
		}
	}
	return ""
}

// Arch is NVML's architecture and CUDA compute capability for a product, in Mokka's spelling.
// It is the same table the kwok projection derives GPU Feature Discovery's family and
// compute-capability labels from (D43), so the labels real GFD reads from the mock and the
// labels the projection writes agree; the harness and the tests hold them together.
func Arch(product string) (arch string, major, minor int, ok bool) {
	p := strings.ToUpper(product)
	for _, a := range []struct {
		match, arch  string
		major, minor int
	}{
		{"GB300", "blackwell", 10, 3}, {"B300", "blackwell", 10, 3},
		{"GB200", "blackwell", 10, 0}, {"B200", "blackwell", 10, 0},
		{"GH200", "hopper", 9, 0}, {"H200", "hopper", 9, 0}, {"H100", "hopper", 9, 0},
		{"L40", "ada", 8, 9}, {"L4", "ada", 8, 9},
		{"A100", "ampere", 8, 0}, {"A30", "ampere", 8, 0}, {"A10", "ampere", 8, 6},
	} {
		if strings.Contains(p, a.match) {
			return a.arch, a.major, a.minor, true
		}
	}
	return "", 0, 0, false
}

// Family is GPU Feature Discovery's nvidia.com/gpu.family for a Mokka architecture.
func Family(arch string) string {
	if arch == "ada" {
		return "ada-lovelace"
	}
	return arch
}

const zeroUUID = "00000000-0000-0000-0000-000000000000"

// Render writes the host's identity over a Mokka base profile (the YAML of the profile BaseFor
// names, read from the pinned chart). Everything the model says replaces the base's value;
// everything it does not is the base's, and Notes say which.
func Render(h Host, baseName string, base []byte) (*Profile, error) {
	if len(h.GPUs) == 0 {
		return nil, fmt.Errorf("%s: no NVIDIA GPU", h.Path)
	}
	name := h.GPUs[0].Name
	for _, g := range h.GPUs {
		if g.Name != name {
			// One device_defaults name per node, as every Mokka profile and every NVIDIA node has.
			return nil, fmt.Errorf("%s: GPUs of two products (%q, %q): Mokka renders one product per node", h.Path, name, g.Name)
		}
	}
	var doc map[string]any
	if err := yaml.Unmarshal(base, &doc); err != nil {
		return nil, fmt.Errorf("base profile %s: %w", baseName, err)
	}
	dd, ok := doc["device_defaults"].(map[string]any)
	if !ok || doc["version"] == nil {
		return nil, fmt.Errorf("base profile %s: not an nvml-mock config (no version or device_defaults)", baseName)
	}
	p := &Profile{Host: h, Base: baseName}
	note := func(f string, a ...any) { p.Notes = append(p.Notes, fmt.Sprintf(f, a...)) }

	// ---- the node's GPUs: name, architecture, memory, VBIOS
	dd["name"] = name
	if arch, major, minor, ok := Arch(name); ok {
		dd["architecture"] = arch
		dd["compute_capability"] = map[string]any{"major": major, "minor": minor}
		if b, ok := baseCC(base); ok && b != [2]int{major, minor} {
			note("compute capability %d.%d is the twin's (D43); Mokka's %s profile says %d.%d", major, minor, baseName, b[0], b[1])
		}
	} else {
		note("architecture and compute capability are Mokka's %s profile's: the twin has none for %q", baseName, name)
	}
	if mib := h.GPUs[0].MemoryMiB; mib > 0 {
		mem, _ := dd["memory"].(map[string]any)
		if mem == nil {
			mem = map[string]any{}
		}
		total := mib << 20
		reserved := asInt64(mem["reserved_bytes"])
		if reserved >= total {
			reserved = 0
		}
		mem["total_bytes"], mem["reserved_bytes"], mem["free_bytes"], mem["used_bytes"] = total, reserved, total-reserved, 0
		dd["memory"] = mem
	} else {
		note("memory is Mokka's %s profile's (%d MiB): the twin's profile states none for %q", baseName, asInt64(dig(dd, "memory", "total_bytes"))>>20, name)
	}
	if v := h.GPUs[0].VBIOS; v != "" {
		dd["vbios_version"] = v
	}
	delete(dd, "serial") // per device below: a default serial would be one board's for all

	// ---- NVLink fabric and the tray's place in its rack
	if fab, ok := dd["fabric"].(map[string]any); ok {
		if h.Fabric != nil {
			fab["cluster_uuid"], fab["clique_id"] = h.Fabric.ClusterUUID, h.Fabric.CliqueID
		} else {
			// What an HGX H100 reports (Mokka's own capture, qx-h100.xml): a completed fabric
			// with a zero cluster UUID, which GFD turns into no clique label.
			fab["cluster_uuid"], fab["clique_id"] = zeroUUID, 0
		}
	} else if h.Fabric != nil {
		dd["fabric"] = map[string]any{"cluster_uuid": h.Fabric.ClusterUUID, "clique_id": h.Fabric.CliqueID, "state": "completed"}
	}
	if h.Platform != nil {
		dd["platform"] = map[string]any{
			"chassis_serial_number": h.Platform.ChassisSerial,
			"slot_number":           h.Platform.Slot,
			"tray_index":            h.Platform.TrayIndex,
			"host_id":               1,
			"peer_type":             "switch_connected",
		}
	} else if _, had := dd["platform"]; had {
		delete(dd, "platform")
		note("platform info removed: the host is not a compute tray of an NVL rack, and the base's describes one")
	}

	// ---- one device entry per GPU, keeping the base's per-device shape (CPU pairing) by index
	baseDevs, _ := doc["devices"].([]any)
	byIndex := map[int]map[string]any{}
	var baseBDF []string // the base's GPU addresses, by index
	for _, x := range baseDevs {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		i := int(asInt64(m["index"]))
		byIndex[i] = m
		for len(baseBDF) <= i {
			baseBDF = append(baseBDF, "")
		}
		baseBDF[i], _ = dig(m, "pci", "bus_id").(string)
	}
	var devs []any
	for _, g := range h.GPUs {
		m := byIndex[g.Index]
		if m == nil {
			m = map[string]any{}
		}
		pci, _ := m["pci"].(map[string]any)
		if pci == nil {
			pci = map[string]any{}
		}
		pci["bus_id"] = g.BusID
		m["index"], m["uuid"], m["serial"], m["pci"], m["minor_number"] = g.Index, g.UUID, g.Serial, pci, g.Index
		delete(m, "name")
		if h.Platform != nil {
			// GPU ids are module ids from 1 in index order, as the twin's NMX-C reports them.
			m["platform"] = map[string]any{"module_id": g.Index + 1}
		} else {
			delete(m, "platform")
		}
		devs = append(devs, m)
	}
	doc["devices"] = devs
	if len(baseDevs) != len(h.GPUs) {
		note("%d GPUs (Mokka's %s profile has %d)", len(h.GPUs), baseName, len(baseDevs))
	}

	// ---- PCIe root complexes: the base's layout, with the twin's addresses in the base's slots
	if topo, ok := doc["pcie_topology"].(map[string]any); ok {
		repl := map[string]string{}
		for i, old := range baseBDF {
			if old == "" {
				continue
			}
			repl[strings.ToUpper(old)] = "" // a base GPU slot no twin GPU fills is dropped
			for _, g := range h.GPUs {
				if g.Index == i {
					repl[strings.ToUpper(old)] = g.BusID
				}
			}
		}
		placed := map[string]bool{}
		rcs, _ := topo["root_complexes"].([]any)
		for _, x := range rcs {
			rc, ok := x.(map[string]any)
			if !ok {
				continue
			}
			list, _ := rc["devices"].([]any)
			var out []any
			for _, y := range list {
				s, _ := y.(string)
				if nb, isGPU := repl[strings.ToUpper(s)]; isGPU {
					if nb != "" {
						out = append(out, nb)
						placed[nb] = true
					}
					continue
				}
				out = append(out, y) // a bridge or NVSwitch of the base's baseboard
			}
			rc["devices"] = out
		}
		if len(rcs) > 0 {
			last, _ := rcs[len(rcs)-1].(map[string]any)
			for _, g := range h.GPUs {
				if !placed[g.BusID] && last != nil {
					l, _ := last["devices"].([]any)
					last["devices"] = append(l, g.BusID)
				}
			}
		}
		note("PCIe root complexes and NUMA placement are Mokka's %s layout: the twin models no PCIe topology", baseName)
	}

	// ---- InfiniBand: not rendered (IsNot)
	if ib, ok := doc["infiniband"].(map[string]any); ok && ib["enabled"] == true {
		doc["infiniband"] = map[string]any{"enabled": false}
		note("InfiniBand off: Mokka's HCA GUIDs cannot be the twin's port GUIDs")
	}
	if _, ok := doc["nvlink"].(map[string]any); ok && h.Fabric != nil {
		note("NVSwitch UUIDs are Mokka's: the twin's NVLink switch trays are devices of their own")
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	head := fmt.Sprintf("# nvml-mock (Mokka %s) GPU profile for %s (node %s), rendered by dc-twin over Mokka's %s profile.\n"+
		"# Identity, count, memory and fabric are the twin's model; the rest is Mokka's. Do not edit: re-render.\n", Mokka, h.Path, h.Node, baseName)
	p.Config = append([]byte(head), out...)
	sort.Strings(p.Notes)
	return p, nil
}

// baseCC reads the base's compute capability.
func baseCC(base []byte) ([2]int, bool) {
	var d struct {
		DD struct {
			CC *struct{ Major, Minor int } `yaml:"compute_capability"`
		} `yaml:"device_defaults"`
	}
	if yaml.Unmarshal(base, &d) != nil || d.DD.CC == nil {
		return [2]int{}, false
	}
	return [2]int{d.DD.CC.Major, d.DD.CC.Minor}, true
}

func dig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func asInt64(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case uint64:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

// Labels is what GPU Feature Discovery publishes for a rendered host, by its own rules: the
// product is the NVML name with spaces as hyphens, the memory NVML's total in MiB, the family and
// compute capability the architecture's, and the machine Mokka's machine-type file, which is the
// GPU's name (Mokka's node agent writes no platform name). The clique is the fabric's cluster
// UUID and clique id. It is the model side of the harness's comparison.
func Labels(p *Profile) map[string]string {
	var doc map[string]any
	_ = yaml.Unmarshal(p.Config, &doc)
	dd, _ := doc["device_defaults"].(map[string]any)
	name, _ := dd["name"].(string)
	l := map[string]string{
		"nvidia.com/gpu.present": "true",
		"nvidia.com/gpu.count":   fmt.Sprint(len(p.Host.GPUs)),
		"nvidia.com/gpu.product": strings.ReplaceAll(name, " ", "-"),
		"nvidia.com/gpu.machine": strings.ReplaceAll(name, " ", "-"),
		"nvidia.com/gpu.memory":  fmt.Sprint(asInt64(dig(dd, "memory", "total_bytes")) >> 20),
	}
	if arch, major, minor, ok := Arch(name); ok {
		l["nvidia.com/gpu.family"] = Family(arch)
		l["nvidia.com/gpu.compute.major"] = fmt.Sprint(major)
		l["nvidia.com/gpu.compute.minor"] = fmt.Sprint(minor)
	}
	if p.Host.Fabric != nil {
		l["nvidia.com/gpu.clique"] = fmt.Sprintf("%s.%d", p.Host.Fabric.ClusterUUID, p.Host.Fabric.CliqueID)
	}
	return l
}

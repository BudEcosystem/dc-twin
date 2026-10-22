package control

import (
	"bufio"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
)

// catalogueFull is what a designer draws on (09 — Catalogues): every rack format with its
// parameters, slots, layout, power and cooling; every profile with its form, ports, power,
// cooling and fidelity cost; the fabric templates and their parameters; the NOS registry; the
// BMS (topic) and SNMP (MIB) dialects; stacks; and the packs, marked pinned when the running
// estate pins them. Lists are sorted by name, so a client can diff two reads.
func (s *Server) catalogueFull() map[string]any {
	reg := s.T.Reg
	pinned := map[string]string{}
	if s.T.Lock != nil {
		for _, l := range s.T.Lock.Packs {
			pinned[l.Name] = l.Version
		}
	}
	packOf := func(name string) string { return strings.SplitN(name, "/", 2)[0] }
	isPinned := func(name string) bool { _, ok := pinned[packOf(name)]; return ok }
	isNot := func(n schema.IsNot) map[string][]string {
		out := map[string][]string{}
		for _, l := range []string{"model", "vm", "hardware"} {
			if v := n.At(l); len(v) > 0 {
				out[l] = v
			}
		}
		return out
	}

	formats := []any{}
	for _, name := range sortedKeys(reg.Formats) {
		f := reg.Formats[name].Spec
		slots := []any{}
		for _, sl := range f.Slots {
			slots = append(slots, map[string]any{"names": sl.Names, "profile": sl.Profile, "accepts": sl.Accepts, "side": sl.Side, "at": sl.At, "when": sl.When, "u": sl.U})
		}
		inputs := []any{}
		for _, in := range f.Power.Inputs {
			inputs = append(inputs, map[string]any{"name": in.Name, "kind": in.Kind, "capacityW": in.CapacityW, "redundancy": in.Redundancy, "minSources": in.MinSources, "feeds": in.Feeds})
		}
		domains := []any{}
		for _, d := range f.Domains {
			domains = append(domains, map[string]any{"kind": d.Kind, "technology": d.Technology, "spans": d.Spans, "unit": d.Unit})
		}
		formats = append(formats, map[string]any{"name": name, "pinned": isPinned(name), "archetype": f.Archetype, "params": f.Params, "slots": slots,
			"layout": map[string]any{"height": f.Layout.Height, "front": f.Layout.Front, "back": f.Layout.Back, "reserved": f.Layout.Reserved},
			"power":  map[string]any{"capacityW": f.Power.CapacityW, "inputs": inputs}, "cooling": f.Cooling, "domains": domains, "isNot": isNot(f.IsNot)})
	}

	profiles := []any{}
	for _, name := range sortedKeys(reg.Profiles) {
		p := reg.Profiles[name].Spec
		// ports summarised by kind, role and speed: what the profile can be cabled to
		type pk struct{ kind, role, speed string }
		counts := map[pk]int{}
		var order []pk
		add := func(ps schema.PortSpec, mult int) {
			k := pk{ps.Kind, ps.Role, ps.Speed}
			n := ps.Count
			if n == 0 {
				n = expandCount(ps.Name)
			}
			if _, ok := counts[k]; !ok {
				order = append(order, k)
			}
			counts[k] += n * mult
		}
		for _, ps := range p.Ports {
			add(ps, 1)
		}
		comps := []any{}
		for _, c := range p.Components {
			for _, ps := range c.Ports {
				add(ps, max(1, c.Count))
			}
			comps = append(comps, map[string]any{"name": c.Name, "kind": c.Kind, "count": c.Count, "model": c.Model, "vendor": c.Vendor, "profile": c.Profile, "maxW": c.MaxW})
		}
		ports := []any{}
		for _, k := range order {
			ports = append(ports, map[string]any{"kind": k.kind, "role": k.role, "speed": k.speed, "count": counts[k]})
		}
		levels := []string{}
		for _, l := range []string{"model", "vm", "hardware"} {
			if _, ok := p.Fidelity[l]; ok {
				levels = append(levels, l)
			}
		}
		var vm any
		if v, ok := p.Fidelity["vm"]; ok {
			vm = map[string]any{"arch": v["arch"], "cost": v["cost"], "tpm": v["tpm"]}
		}
		prof := map[string]any{"name": name, "pinned": isPinned(name), "class": p.Class, "form": p.Form, "ports": ports, "components": comps,
			"power": p.Power, "cooling": p.Cooling.Kind, "fidelity": levels, "vm": vm, "provides": p.Provides, "isNot": isNot(p.IsNot)}
		if p.BMC != nil {
			prof["dialect"] = p.BMC.Dialect
		}
		if p.Identity != nil {
			prof["manufacturer"], prof["model"] = p.Identity.Manufacturer, p.Identity.Model
		}
		profiles = append(profiles, prof)
	}

	templates := []any{}
	for _, name := range sortedKeys(reg.Templates) {
		t := reg.Templates[name].Spec
		templates = append(templates, map[string]any{"name": name, "pinned": isPinned(name), "generator": t.Generator, "params": t.Params,
			"layout": t.Layout, "provides": t.Provides, "isNot": isNot(t.IsNot)})
	}

	noses := []any{}
	names := nos.Names()
	sort.Strings(names)
	for _, n := range names {
		e, _ := nos.Lookup(n)
		noses = append(noses, map[string]any{"name": n, "real": e.Real, "arch": e.Arch})
	}

	topics := []any{}
	for _, name := range sortedKeys(reg.Topics) {
		topics = append(topics, map[string]any{"name": name, "pinned": isPinned(name)})
	}
	mibs := []any{}
	for _, name := range sortedKeys(reg.MIBs) {
		mibs = append(mibs, map[string]any{"name": name, "pinned": isPinned(name)})
	}
	stacks := []any{}
	for _, name := range sortedKeys(reg.Stacks) {
		st := reg.Stacks[name].Spec
		stacks = append(stacks, map[string]any{"name": name, "title": st.Title, "components": sortedKeys(st.Components), "options": st.Options})
	}
	packs := []any{}
	for _, id := range sortedKeys(reg.Packs) {
		p := reg.Packs[id]
		_, pin := pinned[id]
		packs = append(packs, map[string]any{"id": id, "version": p.Version, "title": p.Manifest.Spec.Title, "dependsOn": p.Manifest.Spec.DependsOn, "pinned": pin})
	}
	return map[string]any{"formats": formats, "profiles": profiles, "templates": templates, "nos": noses, "topics": topics, "mibs": mibs,
		"stacks": stacks, "packs": packs, "templateAliases": map[string]string{"clos-2tier": "generic/clos-2tier", "clos-3tier": "generic/clos-2tier",
			"rail-optimised": "generic/rail-optimised", "oob-flat": "generic/oob-flat"}}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// expandCount is how many names a brace pattern like "nic-{0..7}" or "{a,b}" stands for.
func expandCount(name string) int {
	n := 1
	for {
		i := strings.IndexByte(name, '{')
		if i < 0 {
			return n
		}
		j := strings.IndexByte(name[i:], '}')
		if j < 0 {
			return n
		}
		body := name[i+1 : i+j]
		if a, b, ok := strings.Cut(body, ".."); ok {
			lo, e1 := strconv.Atoi(a)
			hi, e2 := strconv.Atoi(b)
			if e1 == nil && e2 == nil {
				if hi < lo {
					lo, hi = hi, lo
				}
				n *= hi - lo + 1
			}
		} else {
			n *= strings.Count(body, ",") + 1
		}
		name = name[i+j+1:]
	}
}

// hostInfo is the machine the twin runs on, for weighing a design's VM cost (09 — Sizing):
// its architecture, CPUs and memory, and where QEMU can run guests of each architecture.
func hostInfo() map[string]any {
	qemus := []any{}
	for _, arch := range []string{"aarch64", "x86_64"} {
		in, err := qemu.Find(arch, "", true)
		e := map[string]any{"arch": arch, "available": err == nil}
		if err == nil {
			e["accel"], e["version"] = in.Accel, in.Version
		} else {
			e["why"] = err.Error()
		}
		qemus = append(qemus, e)
	}
	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64"
	}
	return map[string]any{"arch": arch, "os": runtime.GOOS, "cpus": runtime.NumCPU(), "memoryGB": memoryGB(), "qemu": qemus}
}

func memoryGB() float64 {
	switch runtime.GOOS {
	case "darwin":
		if b, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			if n, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
				return n / (1 << 30)
			}
		}
	case "linux":
		if f, err := os.Open("/proc/meminfo"); err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if fs := strings.Fields(sc.Text()); len(fs) >= 2 && fs[0] == "MemTotal:" {
					kb, _ := strconv.ParseFloat(fs[1], 64)
					return kb / (1 << 20)
				}
			}
		}
	}
	return 0
}

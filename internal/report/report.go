// Package report builds the fidelity report (docs/spec/07-fidelity.md): for what a run used —
// devices that received requests or acts, devices observed, their racks, the adapters that
// served them — what fidelity it ran at, what was impersonated or stubbed, the twin incidents,
// and everything used that the twin does not model. A result is never reported without it.
package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Used is what a run touched; nil means the whole estate.
type Used struct {
	Devices map[string]bool
	Faults  map[string]bool // fault ids injected
	Since   uint64          // event seq the run started at
}

type Subject struct {
	Subject string   `json:"subject"`
	IsNot   []string `json:"isNot"`
}

type Report struct {
	Estate         string           `json:"estate"`
	Scenario       string           `json:"scenario,omitempty"`
	Generated      string           `json:"generated"`
	Clock          string           `json:"clock"`
	TimeScale      float64          `json:"timeScale"`
	Devices        map[string]int   `json:"devices"`
	Racks          int              `json:"racks"`
	FabricDevices  map[string]int   `json:"fabricDevices"`
	Impersonated   []string         `json:"impersonated"`
	Stubs          []string         `json:"stubs"`
	Unsynchronised []string         `json:"unsynchronised"`
	TCG            []string         `json:"tcgGuests"`
	SubstrateOwned []string         `json:"substrateOwnedState"`
	Adapters       []map[string]any `json:"adapters"`
	Incidents      []string         `json:"twinIncidents"`
	NotApplied     []string         `json:"faultEffectsNotApplied"`
	NotModelled    []Subject        `json:"notModelled"`
	Unverified     []string         `json:"unverifiedProfiles"`
	Substrate      []string         `json:"substrate"`
	Notes          []string         `json:"notes,omitempty"`
}

// Build assembles the report for a twin and what a run used.
func Build(t *twin.Twin, used *Used) *Report {
	eng := t.Engine
	r := &Report{Estate: t.Estate.Metadata.Name, Generated: time.Now().UTC().Format(time.RFC3339), Clock: eng.M.Clock, TimeScale: eng.Scale(),
		Devices: map[string]int{}, FabricDevices: map[string]int{}}
	inUse := func(d *model.Device) bool {
		if used == nil || used.Devices == nil {
			return true
		}
		for x := d; x != nil; x = x.Parent {
			if used.Devices[x.Path] {
				return true
			}
		}
		return false
	}
	profiles, formats, dialects, templates := map[string]string{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	racks := map[string]bool{}
	eng.Read(func(m *model.Model) {
		for _, d := range m.Devices {
			if !inUse(d) {
				continue
			}
			if d.Generated != "" {
				nos := "model switch (nos: none)"
				if f := m.Fabrics[d.Generated]; f != nil && f.NOS != "none" {
					nos = f.NOS
				}
				r.FabricDevices[nos]++
				templates[m.Fabrics[d.Generated].Template] = true
				continue
			}
			key := d.Fidelity
			if d.OS != "" {
				key += " (os: " + d.OS + ")"
			}
			r.Devices[key]++
			if !strings.Contains(" "+profiles[d.ProfileName]+" ", " "+d.Fidelity+" ") {
				// a profile used at several fidelities says what it is not at each
				profiles[d.ProfileName] = strings.TrimSpace(profiles[d.ProfileName] + " " + d.Fidelity)
			}
			if d.Dialect != "" {
				dialects[d.Dialect] = true
			}
			if rk := d.RackOf(); rk != nil {
				racks[rk.Path] = true
				formats[rk.FormatName] = true
			}
		}
		if used == nil {
			for _, f := range m.Fabrics {
				templates[f.Template] = true
			}
		}
	})
	r.Racks = len(racks)
	reg := t.Reg
	add := func(subject string, lines []string) {
		if len(lines) > 0 {
			r.NotModelled = append(r.NotModelled, Subject{Subject: subject, IsNot: dedupe(lines)})
		}
	}
	for _, name := range sortedKeys(profiles) {
		if p := reg.Profiles[name]; p != nil {
			var lines []string
			for _, level := range strings.Fields(profiles[name]) {
				lines = append(lines, p.Spec.IsNot.At(level)...)
			}
			add(name, lines)
			r.Unverified = append(r.Unverified, name+" (source: documentation; no hardware-run ledger entry)")
		}
	}
	for _, name := range sortedBool(formats) {
		if f := reg.Formats[name]; f != nil {
			add(name, f.Spec.IsNot.All)
		}
	}
	for _, name := range sortedBool(dialects) {
		if d, _ := reg.Dialect(name); d != nil {
			add(name, d.Spec.IsNot.All)
		}
	}
	for _, name := range sortedBool(templates) {
		if tp := reg.Templates[name]; tp != nil {
			add(name, tp.Spec.IsNot.All)
		}
	}
	if used != nil {
		for id := range used.Faults {
			if ref := reg.Faults[id]; ref != nil {
				add(id, append(append([]string{}, ref.Catalogue.Spec.IsNot.All...), ref.Spec.IsNot.All...))
			}
		}
	}
	var substrateOwned []string
	for _, a := range t.Adapters {
		rep := a.Report()
		r.Adapters = append(r.Adapters, map[string]any{"adapter": rep.Adapter, "served": rep.Served, "lines": rep.Lines})
		r.Impersonated = append(r.Impersonated, rep.Impersonated...)
		r.Stubs = append(r.Stubs, rep.Stubs...)
		r.TCG = append(r.TCG, rep.TCG...)
		substrateOwned = append(substrateOwned, rep.SubstrateOwned...)
		add("adapter "+rep.Adapter, rep.IsNot)
	}
	for _, s := range t.Estate.Spec.Stacks {
		for _, imp := range s.Impersonators {
			r.Impersonated = append(r.Impersonated, s.Name+": "+imp)
		}
	}
	r.Substrate = t.Net.SubstrateIsNot()
	if eng.Scale() != 1 {
		r.Notes = append(r.Notes, fmt.Sprintf("behaviour durations scaled by %.3g (spec.timeScale): a 40 s power-on takes %.3g s", eng.Scale(), 40*eng.Scale()))
	}
	r.SubstrateOwned = []string{"none: every device is at model fidelity, so all state is owned by the model"}
	if len(substrateOwned) > 0 {
		r.SubstrateOwned = substrateOwned
	}
	since := uint64(0)
	if used != nil {
		since = used.Since
	}
	for _, inc := range eng.Incidents() {
		if inc.Seq > since {
			r.Incidents = append(r.Incidents, fmt.Sprintf("#%d %s: %s", inc.Seq, inc.Source.Principal, inc.Message))
		}
	}
	for _, ev := range eng.Events(since, func(e schema.Event) bool { return e.Kind == "fault" && e.Data != nil }) {
		_ = ev
	}
	for _, f := range eng.Faults("") {
		for _, na := range f.NotApplied {
			r.NotApplied = append(r.NotApplied, f.Fault+" on "+f.Target+": "+na)
		}
	}
	r.Impersonated, r.Stubs = dedupe(r.Impersonated), dedupe(r.Stubs)
	return r
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedBool(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Text renders the report the way 07 shows it.
func (r *Report) Text() string {
	var b strings.Builder
	title := r.Estate
	if r.Scenario != "" {
		title = "scenario " + r.Scenario + " @ " + r.Estate
	}
	fmt.Fprintf(&b, "fidelity report — %s\n", title)
	var dev []string
	for _, k := range sortedInt(r.Devices) {
		dev = append(dev, fmt.Sprintf("%d %s", r.Devices[k], k))
	}
	var fab []string
	for _, k := range sortedInt(r.FabricDevices) {
		fab = append(fab, fmt.Sprintf("%d %s", r.FabricDevices[k], k))
	}
	fmt.Fprintf(&b, "  devices used: %s · %d racks · fabric devices: %s\n", strings.Join(dev, ", "), r.Racks, orNone(strings.Join(fab, ", ")))
	fmt.Fprintf(&b, "  clock: %s (time scale %.3g)\n", r.Clock, r.TimeScale)
	fmt.Fprintf(&b, "  tcg guests: %s\n", orNone(strings.Join(r.TCG, "; ")))
	fmt.Fprintf(&b, "  impersonated: %s\n", orNone(strings.Join(r.Impersonated, "; ")))
	fmt.Fprintf(&b, "  stubs: %s\n", orNone(strings.Join(r.Stubs, "; ")))
	fmt.Fprintf(&b, "  unsynchronised adapters: %s\n", orNone(strings.Join(r.Unsynchronised, "; ")))
	fmt.Fprintf(&b, "  substrate-owned state in use: %s\n", strings.Join(r.SubstrateOwned, "; "))
	for _, a := range r.Adapters {
		lines, _ := a["lines"].([]string)
		fmt.Fprintf(&b, "  adapter %s: %v endpoints\n", a["adapter"], a["served"])
		for _, l := range lines {
			fmt.Fprintf(&b, "    %s\n", l)
		}
	}
	fmt.Fprintf(&b, "  twin incidents: %s\n", orNone(strings.Join(r.Incidents, "; ")))
	if len(r.NotApplied) > 0 {
		fmt.Fprintf(&b, "  fault effects not applied at this fidelity:\n")
		for _, n := range r.NotApplied {
			fmt.Fprintf(&b, "    %s\n", n)
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "  note: %s\n", n)
	}
	fmt.Fprintf(&b, "  substrate: %s\n", strings.Join(r.Substrate, " "))
	fmt.Fprintf(&b, "  not modelled, and used by this run:\n")
	for _, s := range r.NotModelled {
		for i, l := range s.IsNot {
			name := ""
			if i == 0 {
				name = s.Subject
			}
			fmt.Fprintf(&b, "    %-34s %s\n", name, l)
		}
	}
	fmt.Fprintf(&b, "  unverified profiles (no hardware-run ledger entry):\n")
	for _, u := range r.Unverified {
		fmt.Fprintf(&b, "    %s\n", u)
	}
	return b.String()
}

func sortedInt(m map[string]int) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

package engine

import (
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// twoCabledSwitchPorts finds a switch with two cabled data ports whose cables go to two other
// ports, for a cable swap.
func twoCabledSwitchPorts(t *testing.T, e *Engine) (*model.Port, *model.Port) {
	t.Helper()
	for _, d := range e.M.Devices {
		if d.BaseClass != "switch" {
			continue
		}
		var ps []*model.Port
		for _, p := range d.Ports {
			if p.Kind == "ethernet" && p.Role != "management" && p.Link != nil {
				ps = append(ps, p)
			}
		}
		if len(ps) >= 2 && ps[0].Link != ps[1].Link {
			return ps[0], ps[1]
		}
	}
	t.Skip("the estate has no switch with two cabled data ports")
	return nil, nil
}

// A cable swap exchanges two cables' far ends as built and leaves the design alone; each link and
// each of the four ports gets an asBuilt event; clearing it puts both back (18 — LLDP and
// cabling, D78).
func TestCableSwapIsAsBuiltOnly(t *testing.T) {
	e := virtualNVL(t)
	p1, p2 := twoCabledSwitchPorts(t, e)
	l1, l2 := p1.Link, p2.Link
	far1, far2 := l1.Peer(p1), l2.Peer(p2)
	id1, id2, a1, b1 := l1.ID, l2.ID, l1.A, l1.B
	since := e.Events(0, nil)
	last := since[len(since)-1].Seq

	fs, err := e.Inject(p1.Ref, "generic/cabling/cable-swap", map[string]any{"with": p2.Ref}, "swap", user)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || len(fs[0].NotApplied) != 0 {
		t.Fatalf("inject: %+v", fs)
	}
	if e.AsBuilt(l1) != l2 || e.AsBuilt(l2) != l1 {
		t.Fatalf("AsBuilt: %v, %v; want each other", e.AsBuilt(l1), e.AsBuilt(l2))
	}
	if got := e.AsBuiltPeer(p1); got != far2 {
		t.Errorf("as built %s reaches %s, want %s", p1.Ref, got.Ref, far2.Ref)
	}
	if got := e.AsBuiltPeer(p2); got != far1 {
		t.Errorf("as built %s reaches %s, want %s", p2.Ref, got.Ref, far1.Ref)
	}
	if got := e.AsBuiltPeer(far1); got != p2 {
		t.Errorf("as built %s reaches %s, want %s", far1.Ref, got.Ref, p2.Ref)
	}
	if got := e.AsBuiltPeer(far2); got != p1 {
		t.Errorf("as built %s reaches %s, want %s", far2.Ref, got.Ref, p1.Ref)
	}
	if l1.ID != id1 || l2.ID != id2 || l1.A != a1 || l1.B != b1 || l1.Peer(p1) != far1 {
		t.Error("the swap changed the design")
	}
	if !e.LinkUp(p1) || !e.LinkUp(p2) {
		t.Error("a swapped cable lost carrier")
	}
	asBuilt := func(from uint64) map[string]any {
		out := map[string]any{}
		for _, ev := range e.Events(from, func(ev schema.Event) bool { return ev.State != nil && ev.State.Field == "asBuilt" }) {
			out[ev.Path] = ev.State.To
		}
		return out
	}
	got := asBuilt(last)
	if got[id1] != "swapped with "+id2 || got[id2] != "swapped with "+id1 {
		t.Errorf("link events: %v", got)
	}
	if got[p1.Ref] != far2.Ref || got[far1.Ref] != p2.Ref || len(got) != 6 {
		t.Errorf("port events: %v", got)
	}

	all := e.Events(0, nil)
	last = all[len(all)-1].Seq
	if _, err := e.Clear("swap", "", user); err != nil {
		t.Fatal(err)
	}
	if e.AsBuilt(l1) != nil || e.AsBuilt(l2) != nil || e.AsBuiltPeer(p1) != far1 {
		t.Error("clearing the swap left the cables swapped")
	}
	got = asBuilt(last)
	if got[id1] != "as designed" || got[p1.Ref] != far1.Ref || got[far1.Ref] != p1.Ref {
		t.Errorf("clear events: %v", got)
	}
}

// A swap that cannot apply is recorded as not applied: with its own cable, or a cable another
// swap already moved.
func TestCableSwapRefusals(t *testing.T) {
	e := virtualNVL(t)
	p1, p2 := twoCabledSwitchPorts(t, e)
	fs, err := e.Inject(p1.Ref, "generic/cabling/cable-swap", map[string]any{"with": p1.Link.Peer(p1).Ref}, "", user)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs[0].NotApplied) != 1 || p1.Link.SwappedWith != nil {
		t.Errorf("swap with its own cable: %+v", fs[0])
	}
	if _, err := e.Inject(p1.Ref, "generic/cabling/cable-swap", map[string]any{"with": p2.Link.ID}, "", user); err != nil {
		t.Fatal(err)
	}
	if e.AsBuilt(p1.Link) != p2.Link {
		t.Fatal("swap by link id did not apply")
	}
	fs, _ = e.Inject(p2.Ref, "generic/cabling/cable-swap", map[string]any{"with": p1.Ref}, "", user)
	if len(fs) != 1 || len(fs[0].NotApplied) != 1 {
		t.Errorf("a second swap of the same cables: %+v", fs)
	}
}

// The day-0 faults are recorded on the switch as day0.* sets for the TAP substrate.
func TestDay0FaultsAreSets(t *testing.T) {
	e := virtualNVL(t)
	p, _ := twoCabledSwitchPorts(t, e)
	sw := p.Device
	if _, err := e.Inject(sw.Path, "generic/day0/switch-slow-boot", nil, "", user); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Inject(sw.Path, "generic/day0/ztp-dhcp-no-option", nil, "", user); err != nil {
		t.Fatal(err)
	}
	s := e.FaultSets(sw)
	if s["day0.bootFactor"] != 4.0 || s["day0.dhcpStripZTP"] != true {
		t.Errorf("sets: %v", s)
	}
}

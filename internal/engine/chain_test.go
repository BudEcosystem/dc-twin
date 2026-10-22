package engine

import (
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// mixedHall is examples/estates/mixed-hall.yaml: feeds A and B, each through a UPS to a panel
// for row a and a panel for rows b and c.
func mixedHall(t *testing.T) *Engine {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../examples/estates/mixed-hall.yaml")
	if err != nil {
		t.Fatal(err)
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: "aarch64"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(res.Model, reg, "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func node(t *testing.T, e *Engine, name string) *model.PowerNode {
	t.Helper()
	for _, n := range e.M.Power {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no power node %s", name)
	return nil
}

// sourcesVia are the rack sources (PDUs, shelves) of a rack that draw through a node.
func sourcesVia(e *Engine, rack string, n *model.PowerNode) []*model.Device {
	var out []*model.Device
	for _, r := range e.M.Racks {
		if r.Name != rack {
			continue
		}
		for _, in := range r.Inputs {
			for _, s := range in.Sources {
				if in.SourceNode[s.Path] == n {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

func allAC(ds []*model.Device, want bool) bool {
	for _, d := range ds {
		if d.State.AC != want {
			return false
		}
	}
	return len(ds) > 0
}

func TestUPSRidesThroughAFeedLossThenTheSideGoesDark(t *testing.T) {
	e := mixedHall(t)
	ups, rpp := node(t, e, "ups-a"), node(t, e, "rpp-a1")
	viaA := sourcesVia(e, "a01", rpp)
	if ups.Mode != "online" || rpp.Mode != "closed" || !allAC(viaA, true) {
		t.Fatalf("at up: ups %s, panel %s, a01's A-side sources powered %v", ups.Mode, rpp.Mode, allAC(viaA, true))
	}
	if err := e.SetFeed("A", false, user); err != nil {
		t.Fatal(err)
	}
	if ups.Mode != "battery" || !rpp.Up || !allAC(viaA, true) {
		t.Fatalf("on feed loss the UPS carries the load from its battery: ups %s, panel up %v", ups.Mode, rpp.Up)
	}
	left := e.upsRuntimeLeft(ups)
	if left <= 0 || left > 300/0.05 {
		t.Fatalf("runtime left %.0f s", left)
	}
	e.Advance(time.Duration(left/2) * time.Second)
	if c := e.chargeNow(ups); c <= 0.3 || c >= 0.7 {
		t.Fatalf("half way through its ride-through the battery should be about half: %.2f", c)
	}
	e.Advance(time.Duration(left/2+5) * time.Second)
	if ups.Mode != "depleted" || rpp.Mode != "dead" || !allAC(viaA, false) {
		t.Fatalf("once the battery is empty side A is lost: ups %s, panel %s, A-side powered %v", ups.Mode, rpp.Mode, allAC(viaA, true))
	}
	if viaB := sourcesVia(e, "a01", node(t, e, "rpp-b1")); !allAC(viaB, true) {
		t.Fatal("side B keeps its power")
	}
	if err := e.SetFeed("A", true, user); err != nil {
		t.Fatal(err)
	}
	if ups.Mode != "online" || !allAC(viaA, true) {
		t.Fatalf("the feed back: ups %s, A-side powered %v", ups.Mode, allAC(viaA, true))
	}
	c0 := e.chargeNow(ups)
	e.Advance(60 * time.Second)
	if c := e.chargeNow(ups); c <= c0 {
		t.Fatalf("the battery recharges once the input is back: %.3f → %.3f", c0, c)
	}
	modes := 0
	for _, ev := range e.Events(0, func(ev schema.Event) bool {
		return ev.State != nil && ev.State.Field == "mode" && ev.Path == ups.Device.Path
	}) {
		_ = ev
		modes++
	}
	if modes != 3 {
		t.Fatalf("online → battery → depleted → online is three mode events, got %d", modes)
	}
}

func TestBreakerTripDropsOnlyThePanelsRacks(t *testing.T) {
	e := mixedHall(t)
	rpp := node(t, e, "rpp-a2")
	c01, a01 := sourcesVia(e, "c01", rpp), sourcesVia(e, "a01", node(t, e, "rpp-a1"))
	if _, err := e.Inject(rpp.Device.Path, "generic/power/breaker-trip", nil, "trip", user); err != nil {
		t.Fatal(err)
	}
	if rpp.Mode != "open" || !allAC(c01, false) {
		t.Fatalf("a tripped panel drops its racks' sources: panel %s, c01 A-side powered %v", rpp.Mode, allAC(c01, true))
	}
	if !allAC(a01, true) || node(t, e, "ups-a").Mode != "online" {
		t.Fatal("the other panel on the same UPS keeps its racks")
	}
	if e.deviceHealth(rpp.Device) != model.Critical {
		t.Fatalf("a tripped panel is Critical, got %s", e.deviceHealth(rpp.Device))
	}
	if _, err := e.Clear("trip", "", user); err != nil {
		t.Fatal(err)
	}
	if rpp.Mode != "closed" || !allAC(c01, true) {
		t.Fatalf("closing the breaker restores the racks: panel %s", rpp.Mode)
	}
	if _, err := e.Inject("a01/pdu-a", "generic/power/breaker-trip", nil, "", user); err == nil {
		t.Fatal("a breaker trip targets power-chain nodes only")
	}
}

func TestUPSFailureDropsItsWholeSide(t *testing.T) {
	e := mixedHall(t)
	ups := node(t, e, "ups-b")
	if _, err := e.Inject(ups.Device.Path, "generic/power/ups-failure", nil, "", user); err != nil {
		t.Fatal(err)
	}
	if ups.Mode != "fault" {
		t.Fatalf("ups mode %s", ups.Mode)
	}
	for _, p := range []string{"rpp-b1", "rpp-b2"} {
		if n := node(t, e, p); n.Up || n.Mode != "dead" {
			t.Fatalf("%s is downstream of the failed UPS: up %v, mode %s", p, n.Up, n.Mode)
		}
	}
	if !allAC(sourcesVia(e, "b01", node(t, e, "rpp-b2")), false) {
		t.Fatal("b01's B side is lost")
	}
	fac := e.Facility()
	chain := fac["power"].([]any)
	if len(chain) != 6 {
		t.Fatalf("the facility view lists the chain: %d nodes", len(chain))
	}
	for _, x := range chain {
		if m := x.(map[string]any); m["name"] == "ups-b" && (m["mode"] != "fault" || m["health"] != model.Critical) {
			t.Fatalf("ups-b in the view: %v", m)
		}
	}
}

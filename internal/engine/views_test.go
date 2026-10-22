package engine

import "testing"

func TestLinksViewShowsAPulledCable(t *testing.T) {
	e := virtualNVL(t)
	all, err := e.Links("")
	if err != nil || len(all) == 0 {
		t.Fatalf("links: %v (%d)", err, len(all))
	}
	if len(all) != len(e.M.Links) {
		t.Fatalf("listed %d of %d links", len(all), len(e.M.Links))
	}
	mine, err := e.Links(tray7)
	if err != nil || len(mine) == 0 {
		t.Fatalf("links of %s: %v (%d)", tray7, err, len(mine))
	}
	id := mine[0]["id"].(string)
	if err := e.SetLink(id, false, user); err != nil {
		t.Fatal(err)
	}
	after, _ := e.Links(tray7)
	for _, l := range after {
		if l["id"] == id && l["up"] != false {
			t.Fatalf("pulled link %s still up", id)
		}
		a, b := l["a"].(map[string]any), l["b"].(map[string]any)
		if a["device"] != tray7 && b["device"] != tray7 {
			t.Fatalf("link %s has no end on %s", l["id"], tray7)
		}
	}
}

func TestFacilityViewMovesLoadOnFeedLoss(t *testing.T) {
	e := virtualNVL(t)
	feedLoad := func() map[string]float64 {
		out := map[string]float64{}
		for _, f := range e.Facility()["feeds"].([]any) {
			m := f.(map[string]any)
			out[m["name"].(string)] = m["loadW"].(float64)
		}
		return out
	}
	before := feedLoad()
	if before["A"] <= 0 || before["B"] <= 0 {
		t.Fatalf("both feeds should carry load: %v", before)
	}
	fac := e.Facility()
	r := fac["racks"].([]any)[0].(map[string]any)
	if r["capacityW"].(float64) <= 0 {
		t.Fatalf("rack capacity: %v", r["capacityW"])
	}
	if err := e.SetFeed("A", false, user); err != nil {
		t.Fatal(err)
	}
	if after := feedLoad(); after["A"] != 0 {
		t.Fatalf("a lost feed carries no load: %v", after)
	}
	for _, f := range e.Facility()["feeds"].([]any) {
		if m := f.(map[string]any); m["name"] == "A" && m["up"] != false {
			t.Fatal("feed A should be down")
		}
	}
}

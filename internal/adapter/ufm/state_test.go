package ufm

import (
	"net/http"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

func TestParsePKey(t *testing.T) {
	for in, want := range map[any]uint16{"0x7fff": 0x7fff, "0X10": 0x10, "103": 103, float64(5): 5, " 0x1 ": 1} {
		if got, err := parsePKey(in); err != nil || got != want {
			t.Errorf("parsePKey(%v) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []any{"0x8000", "-1", "pk", nil, "65535"} {
		if _, err := parsePKey(in); err == nil {
			t.Errorf("parsePKey(%v) accepted", in)
		}
	}
	if pkeyString(0x67) != "0x67" {
		t.Fatal(pkeyString(0x67))
	}
}

func TestVersionAtLeast(t *testing.T) {
	for v, want := range map[string]bool{"6.19.0": true, "6.18.0": false, "6.20.1-3": true, "7.0": true, "x": false} {
		if versionAtLeast(v, 6, 19) != want {
			t.Errorf("%s", v)
		}
	}
}

func waitPower(t *testing.T, tw *twin.Twin, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var p string
		tw.Engine.Read(func(m *model.Model) { p = m.DeviceBy[path].State.Power })
		if p == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never reached %s", path, want)
}

// TestStateSurvivesARestart: partition keys, LIDs and tokens are the UFM's database, kept in the
// state directory; the twin-issued credentials are derived, so they are the same after a restart.
func TestStateSurvivesARestart(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	up := func() (*twin.Twin, Credential, *nicoClient) {
		tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{"ufm"}, StateDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		c := tw.Adapter("ufm").(*Adapter).Credentials()[0]
		cl, _ := newNicoClient(c.URL, "", "", c.Token)
		return tw, c, cl
	}
	tw, c1, cl := up()
	guid := guidOf(tw, "site-1/hall-a/a02/compute-tray-03:cx7-2")
	if err := cl.bindPorts(nicoPartition{PKey: 0x2fb, IPoIB: true}, []nicoPortConfig{{GUID: guid, Index0: true, Membership: "full"}}); err != nil {
		t.Fatal(err)
	}
	ports, _ := cl.listPort(nil, nil, "")
	lid := portByGUID(ports, guid).LID
	tw.Down()

	tw, c2, cl := up()
	defer tw.Down()
	if c1.Token != c2.Token || c1.Password != c2.Password {
		t.Fatal("the twin-issued credentials changed across a restart")
	}
	p, err := cl.getPartition(0x2fb, true, false)
	if err != nil || !p.GUIDs[guid] {
		t.Fatalf("partition after restart: %v %+v", err, p)
	}
	ports, _ = cl.listPort(nil, nil, "")
	if got := portByGUID(ports, guid).LID; got != lid {
		t.Fatalf("LID %d became %d", lid, got)
	}

	// a powered-off host's ports are down; a powered-off appliance answers nothing
	const host = "site-1/hall-a/a02/compute-tray-03"
	if err := tw.Engine.Power(engine.Request{Path: host, Canonical: "power.off.force", Source: actor}); err != nil {
		t.Fatal(err)
	}
	waitPower(t, tw, host, model.Off)
	down, _ := cl.listPort(nil, nil, "Down")
	if len(down) != 4 || portByGUID(down, guid) == nil {
		t.Fatalf("ports down with %s off: %d", host, len(down))
	}
	const ufm = "site-1/hall-a/c03/ufm-1"
	if err := tw.Engine.Power(engine.Request{Path: ufm, Canonical: "power.off.force", Source: actor}); err != nil {
		t.Fatal(err)
	}
	waitPower(t, tw, ufm, model.Off)
	if _, err := cl.version(); err == nil {
		t.Fatal("a powered-off UFM appliance answered")
	} else if e, ok := err.(*ufmError); ok && e.status == http.StatusOK {
		t.Fatal(err)
	}
}

package ibsim

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// recorder is a console that answers every command as ibsim does when it succeeds (nothing but
// the prompt) and keeps them.
type recorder struct {
	mu   sync.Mutex
	cmds []string
}

func (r *recorder) Do(cmd string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, cmd)
	return "", nil
}

// since returns the commands after the first n.
func (r *recorder) since(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.cmds[n:]...)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cmds)
}

var actor = schema.EventSource{Class: "scenario", Principal: "ibsim-test"}

// TestLiveLinkState runs the lab estate with the adapter on a recording console and checks
// that what changes a port in the model reaches ibsim as the console command that changes the
// link: a pulled and replaced cable, a cable-pulled fault and its clearing, a host powered off
// and on (all four of its rails), and an AC cycle of the leaf the UFM appliance hangs off (all
// nineteen of its links, down and back).
func TestLiveLinkState(t *testing.T) {
	rec := &recorder{}
	twin.Register("ibsim-recorder", func() adapter.Adapter {
		return New(Config{newConsole: func(fabric, netFile string) (console, error) {
			if fabric != "backend-ib" {
				t.Errorf("fabric %s", fabric)
			}
			if _, err := os.Stat(netFile); err != nil {
				t.Errorf("topology not written before the console starts: %v", err)
			}
			return rec, nil
		}})
	})
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{"ibsim-recorder"}, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	a := tw.Adapter("ibsim").(*Adapter)
	topo := a.sims[0].topo
	if n := rec.count(); n != 0 {
		t.Fatalf("the healthy estate sent %d commands: %q", n, rec.since(0))
	}
	infos, err := ReadInfo(state)
	if err != nil || len(infos) != 1 || !infos[0].Running || infos[0].SMHost != topo.SM.ID || infos[0].Links != 193+40 {
		t.Fatalf("info file: %v %+v", err, infos)
	}

	rail1 := "S-" + strings.TrimPrefix(linkOf(topo, "site-1/hall-a/a01/compute-tray-07:cx8-0").A.Node.ID, "S-")
	tray7 := linkOf(topo, "site-1/hall-a/a01/compute-tray-07:cx8-0").B.Node.ID
	unlink7 := `Unlink "` + rail1 + `"[7]`
	link7 := `Link "` + rail1 + `"[7] "` + tray7 + `"[1]`

	// wait until the commands sent after mark satisfy ok
	wait := func(what string, mark int, ok func([]string) bool) []string {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			got := rec.since(mark)
			if ok(got) {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: commands %q", what, got)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	exactly := func(want ...string) func([]string) bool {
		return func(got []string) bool { return strings.Join(got, "\n") == strings.Join(want, "\n") }
	}

	t.Run("cable pull and replace", func(t *testing.T) {
		mark := rec.count()
		if err := tw.Engine.SetLink("site-1/hall-a/a01/compute-tray-07:cx8-0", false, actor); err != nil {
			t.Fatal(err)
		}
		wait("pull", mark, exactly(unlink7))
		if err := tw.Engine.SetLink("site-1/hall-a/a01/compute-tray-07:cx8-0", true, actor); err != nil {
			t.Fatal(err)
		}
		t.Logf("%q", wait("replace", mark, exactly(unlink7, link7)))
	})

	t.Run("cable-pulled fault and its clearing", func(t *testing.T) {
		mark := rec.count()
		fv, err := tw.Engine.Inject("a01/compute-tray-07:cx8-0", "generic/link/cable-pulled", nil, "", actor)
		if err != nil || len(fv) != 1 {
			t.Fatalf("inject: %v %+v", err, fv)
		}
		wait("fault", mark, exactly(unlink7))
		if _, err := tw.Engine.Clear(fv[0].ID, "", actor); err != nil {
			t.Fatal(err)
		}
		wait("clear", mark, exactly(unlink7, link7))
	})

	t.Run("a host powered off and on", func(t *testing.T) {
		mark := rec.count()
		path := "site-1/hall-a/a01/compute-tray-08"
		if err := tw.Engine.Power(engine.Request{Path: path, Canonical: "power.off.force", Source: actor}); err != nil {
			t.Fatal(err)
		}
		got := wait("off", mark, func(c []string) bool { return len(c) == 4 })
		for i, c := range got {
			if !strings.HasPrefix(c, "Unlink ") || !strings.HasSuffix(c, `"[8]`) {
				t.Fatalf("rail %d: %q", i+1, c)
			}
		}
		if err := tw.Engine.Power(engine.Request{Path: path, Canonical: "power.on", Source: actor}); err != nil {
			t.Fatal(err)
		}
		got = wait("on", mark, func(c []string) bool { return len(c) == 8 })
		for _, c := range got[4:] {
			if !strings.HasPrefix(c, "Link ") || !strings.Contains(c, `"[8] "H-`) {
				t.Fatalf("power on: %q", got)
			}
		}
		t.Logf("off and on: %q", got)
	})

	t.Run("the UFM's leaf switch loses AC and gets it back", func(t *testing.T) {
		mark := rec.count()
		if err := tw.Engine.Power(engine.Request{Path: "site-1/hall-a/c03/backend-ib-rail-a01-1", Canonical: "ac.cycle", Source: actor}); err != nil {
			t.Fatal(err)
		}
		// the leaf's 18 host links, its 2 spine uplinks and the UFM appliance's link, each
		// unlinked when it loses AC and linked again when it is back
		got := wait("ac cycle", mark, func(c []string) bool { return len(c) == 42 })
		for i, c := range got {
			want := "Unlink "
			if i >= 21 {
				want = "Link "
			}
			if !strings.HasPrefix(c, want+`"`+rail1+`"`) {
				t.Fatalf("command %d: %q", i, c)
			}
		}
	})

	r := a.Report()
	if r.Served != 1 || len(r.SubstrateOwned) != 1 || !strings.Contains(r.Lines[0], "SIMULATED") || !strings.Contains(r.Lines[0], "link change(s) applied") {
		t.Fatalf("report %+v", r)
	}
	t.Logf("report: %q", r.Lines)
}

// TestRendersWithoutIbsim: with no ibsim the adapter writes the topology and says it simulates
// nothing.
func TestRendersWithoutIbsim(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	twin.Register("ibsim-none", func() adapter.Adapter { return New(Config{Binary: "none"}) })
	state := t.TempDir()
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{"ibsim-none"}, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	b, err := os.ReadFile(state + "/ibsim-backend-ib.net")
	if err != nil || !strings.Contains(string(b), `Switch	64 "S-`) {
		t.Fatalf("topology: %v", err)
	}
	r := tw.Adapter("ibsim").Report()
	if r.Served != 0 || !strings.Contains(r.Lines[0], "not simulated: ibsim.binary is none") {
		t.Fatalf("report %+v", r.Lines)
	}
	infos, err := ReadInfo(state)
	if err != nil || len(infos) != 1 || infos[0].Running || infos[0].Why == "" {
		t.Fatalf("info %v %+v", err, infos)
	}
}

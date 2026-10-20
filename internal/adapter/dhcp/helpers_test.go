package dhcp

import (
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const labEstate = "../../../examples/estates/lab-model.yaml"

// tune adjusts the adapter the "dhcp-test" factory builds; tests run one at a time.
var tune func(a *Adapter)

func init() {
	twin.Register("dhcp-test", func() adapter.Adapter {
		a := New()
		if tune != nil {
			tune(a)
		}
		return a
	})
}

func loadLab(t testing.TB) *schema.Estate {
	t.Helper()
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	return est
}

// up brings an estate up with only this adapter, tuned by f.
func up(t testing.TB, est *schema.Estate, f func(a *Adapter)) (*twin.Twin, *Adapter) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	tune = f
	defer func() { tune = nil }()
	tw, err := twin.UpEstate(est, "", reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{"dhcp-test"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	return tw, tw.Adapters[0].(*Adapter)
}

// waitLeased waits until every BMC endpoint has a lease, and returns how long it took.
func waitLeased(t testing.TB, a *Adapter, timeout time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	want := len(a.endpoints)
	for {
		got := len(a.Leases())
		if got >= want {
			return time.Since(start)
		}
		if time.Since(start) > timeout {
			t.Fatalf("%d of %d BMC endpoints leased after %s", got, want, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// leases reads every BMC endpoint's committed lease from the model.
func leases(tw *twin.Twin, a *Adapter) map[string]model.Lease {
	out := map[string]model.Lease{}
	tw.Engine.Read(func(m *model.Model) {
		for path := range a.endpoints {
			if l := m.DeviceBy[path].State.Lease; l != nil {
				out[path] = *l
			}
		}
	})
	return out
}

// checkRelayFacts asserts what every lease must carry from the cabling: the circuit id and
// remote id of the port the BMC is cabled to, an address inside its rack's segment, and the
// segment's gateway.
func checkRelayFacts(t testing.TB, tw *twin.Twin, a *Adapter, ls map[string]model.Lease) {
	t.Helper()
	seen := map[string]string{}
	for path, ep := range a.endpoints {
		l, ok := ls[path]
		if !ok {
			t.Errorf("%s: no lease in the model", path)
			continue
		}
		want := circuitID(ep.sw, ep.swPort)
		if l.Circuit != want {
			t.Errorf("%s: circuit id %q, want %q", path, l.Circuit, want)
		}
		if l.Remote != switchMAC(ep.sw).String() {
			t.Errorf("%s: remote id %q, want the switch's MAC %s", path, l.Remote, switchMAC(ep.sw))
		}
		ip, err := netip.ParseAddr(l.IP)
		if err != nil || !ep.seg.Prefix.Contains(ip) {
			t.Errorf("%s: lease %s is outside its segment %s", path, l.IP, ep.seg.Prefix)
		}
		if l.Gateway != ep.seg.Gateway.String() {
			t.Errorf("%s: gateway %s, want %s", path, l.Gateway, ep.seg.Gateway)
		}
		if prev, dup := seen[l.IP]; dup {
			t.Errorf("%s and %s share %s", prev, path, l.IP)
		}
		seen[l.IP] = path
		if l.VendorClass == "" {
			t.Errorf("%s: no vendor class recorded", path)
		}
	}
}

func compileEstate(t testing.TB, est *schema.Estate) *model.Model {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: compile.HostArch()})
	if err != nil {
		t.Fatal(err)
	}
	return res.Model
}

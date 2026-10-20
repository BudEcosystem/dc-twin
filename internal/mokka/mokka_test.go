package mokka_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	pmodel "github.com/prometheus/common/model"
	"go.yaml.in/yaml/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/BudEcosystem/dc-twin/internal/adapter/kwok"
	"github.com/BudEcosystem/dc-twin/internal/adapter/metrics"
	"github.com/BudEcosystem/dc-twin/internal/adapter/nvl"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/control"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/mokka"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const labEstate = "../../examples/estates/lab-model.yaml"

var src = schema.EventSource{Class: "user", Principal: "mokka-test"}

// The hosts the harness (compat/gpu-operator) runs as container hosts, and a GB200 tray.
const (
	gb300 = "site-1/hall-a/a01/compute-tray-01"
	gb200 = "site-1/hall-a/a02/compute-tray-03"
	h100  = "site-1/hall-a/b01/server-01"
)

func labEngine(t *testing.T) *engine.Engine {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: compile.HostArch()})
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(res.Model, reg, "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func hostAt(t *testing.T, m *model.Model, path string) (mokka.Host, *model.Device) {
	t.Helper()
	d := deviceAt(m, path)
	if d == nil {
		t.Fatalf("no device %s", path)
	}
	h, ok := mokka.HostOf(m, d)
	if !ok {
		t.Fatalf("%s: no NVIDIA GPU", path)
	}
	return h, d
}

func deviceAt(m *model.Model, path string) *model.Device {
	for _, d := range m.Devices {
		if d.Path == path {
			return d
		}
	}
	return nil
}

func render(t *testing.T, h mokka.Host) (*mokka.Profile, map[string]any) {
	t.Helper()
	base := mokka.BaseFor(h.GPUs[0].Name)
	b, err := os.ReadFile(filepath.Join("testdata", base+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := mokka.Render(h, base, b)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(p.Config, &doc); err != nil {
		t.Fatalf("rendered profile is not YAML: %v", err)
	}
	return p, doc
}

func get(m any, keys ...string) any {
	for _, k := range keys {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[k]
	}
	return m
}

// Every NVIDIA host of the lab estate renders, and the node names are the ones the kwok
// projection and the dcgm-exporter impersonation use.
func TestHostsOfTheLabEstate(t *testing.T) {
	e := labEngine(t)
	hs := mokka.Hosts(e.M)
	if len(hs) == 0 {
		t.Fatal("no hosts")
	}
	products := map[string]int{}
	for _, h := range hs {
		d := deviceAt(e.M, h.Path)
		if h.Node != kwok.NodeName(d) || h.Node != metrics.NodeName(d) {
			t.Fatalf("%s: node %q, kwok %q, metrics %q", h.Path, h.Node, kwok.NodeName(d), metrics.NodeName(d))
		}
		products[h.GPUs[0].Name] += len(h.GPUs)
		if mokka.BaseFor(h.GPUs[0].Name) == "" {
			t.Fatalf("%s: no Mokka base for %q", h.Path, h.GPUs[0].Name)
		}
		uuids := map[string]bool{}
		for i, g := range h.GPUs {
			if g.Index != i || uuids[g.UUID] || !strings.HasPrefix(g.UUID, "GPU-") || len(g.Serial) != 13 {
				t.Fatalf("%s: GPU %d: %+v", h.Path, i, g)
			}
			uuids[g.UUID] = true
		}
		// a GB tray's superchip carries two GPUs on one board: they share its serial
		if strings.HasPrefix(h.GPUs[0].Name, "NVIDIA GB") && (h.GPUs[0].Serial != h.GPUs[1].Serial || h.GPUs[1].Serial == h.GPUs[2].Serial) {
			t.Fatalf("%s: GPU serials %s %s %s, want pairs", h.Path, h.GPUs[0].Serial, h.GPUs[1].Serial, h.GPUs[2].Serial)
		}
	}
	t.Logf("%d hosts: %v", len(hs), products)
	for _, p := range []string{gb300, gb200, h100} {
		if _, ok := mokka.HostOf(e.M, deviceAt(e.M, p)); !ok {
			t.Fatalf("%s is not a Mokka host", p)
		}
	}
	// AMD and Gaudi servers are not NVIDIA hosts.
	for _, h := range hs {
		if strings.Contains(h.Path, "/b03/") || strings.Contains(h.Path, "/b04/") {
			t.Fatalf("%s rendered as an NVIDIA host", h.Path)
		}
	}
}

func TestRenderGB300Tray(t *testing.T) {
	e := labEngine(t)
	h, d := hostAt(t, e.M, gb300)
	p, doc := render(t, h)
	dd := doc["device_defaults"]
	if get(dd, "name") != "NVIDIA GB300" || get(dd, "architecture") != "blackwell" ||
		get(dd, "compute_capability", "major") != 10 || get(dd, "compute_capability", "minor") != 3 {
		t.Fatalf("identity: %v %v %v", get(dd, "name"), get(dd, "architecture"), get(dd, "compute_capability"))
	}
	// NVML's figure (the profile's memoryMiB), not the marketed 288 GiB
	if get(dd, "memory", "total_bytes") != 284208<<20 || get(dd, "memory", "free_bytes") != 284208<<20-1610612736 {
		t.Fatalf("memory: %v", get(dd, "memory"))
	}
	if get(dd, "vbios_version") != d.State.Firmware["gpu"].Version || get(dd, "serial") != nil {
		t.Fatalf("vbios %v serial %v", get(dd, "vbios_version"), get(dd, "serial"))
	}
	dom := e.M.Domains[0]
	for _, x := range nvl.ScaleUpDomains(e.M) {
		for _, m := range x.Members {
			if m == h.GPUs[0].Path {
				dom = x
			}
		}
	}
	if get(dd, "fabric", "cluster_uuid") != nvl.DomainUUID(e.M.Seed, dom) || get(dd, "fabric", "clique_id") != 1 || get(dd, "fabric", "state") != "auto" {
		t.Fatalf("fabric: %v", get(dd, "fabric"))
	}
	if get(dd, "platform", "chassis_serial_number") != nvl.ChassisSerial(e.M.Seed, d.RackOf()) ||
		get(dd, "platform", "slot_number") != int(nvl.Slot(d)) || get(dd, "platform", "tray_index") != int(nvl.TrayIndex(d)) {
		t.Fatalf("platform: %v", get(dd, "platform"))
	}
	devs := doc["devices"].([]any)
	if len(devs) != 4 {
		t.Fatalf("%d devices", len(devs))
	}
	for i, x := range devs {
		g := h.GPUs[i]
		if get(x, "index") != i || get(x, "uuid") != g.UUID || get(x, "serial") != g.Serial || get(x, "pci", "bus_id") != g.BusID ||
			get(x, "minor_number") != i || get(x, "platform", "module_id") != i+1 || get(x, "grace_cpu_pair") != i/2 {
			t.Fatalf("device %d: %v", i, x)
		}
	}
	rcs := get(doc, "pcie_topology", "root_complexes").([]any)
	if got := get(rcs[0], "devices"); len(got.([]any)) != 2 || got.([]any)[0] != "0000:18:00.0" || got.([]any)[1] != "0000:28:00.0" {
		t.Fatalf("root complex 0: %v", got)
	}
	if got := get(rcs[1], "devices"); got.([]any)[0] != "0000:38:00.0" || got.([]any)[1] != "0000:48:00.0" {
		t.Fatalf("root complex 1: %v", got)
	}
	if get(doc, "infiniband", "enabled") != false || get(doc, "version") != "1.0" || get(doc, "system", "driver_version") != "580.65.06" {
		t.Fatalf("infiniband %v version %v system %v", get(doc, "infiniband"), get(doc, "version"), get(doc, "system"))
	}
	if !strings.Contains(strings.Join(p.Notes, "\n"), "compute capability 10.3 is the twin's") {
		t.Fatalf("notes: %v", p.Notes)
	}
	l := mokka.Labels(p)
	if l["nvidia.com/gpu.product"] != "NVIDIA-GB300" || l["nvidia.com/gpu.count"] != "4" || l["nvidia.com/gpu.memory"] != "284208" ||
		l["nvidia.com/gpu.family"] != "blackwell" || l["nvidia.com/gpu.clique"] != kwok.DomainID(e.M.Seed, dom) {
		t.Fatalf("labels: %v", l)
	}
}

func TestRenderH100Server(t *testing.T) {
	e := labEngine(t)
	h, _ := hostAt(t, e.M, h100)
	if h.Fabric != nil || h.Platform != nil || h.Arch != "amd64" {
		t.Fatalf("an HGX server: fabric %v platform %v arch %s", h.Fabric, h.Platform, h.Arch)
	}
	p, doc := render(t, h)
	dd := doc["device_defaults"]
	if get(dd, "fabric", "cluster_uuid") != "00000000-0000-0000-0000-000000000000" || get(dd, "platform") != nil {
		t.Fatalf("fabric %v platform %v", get(dd, "fabric"), get(dd, "platform"))
	}
	rcs := get(doc, "pcie_topology", "root_complexes").([]any)
	first := get(rcs[0], "devices").([]any)
	if len(first) != 5 || first[0] != "0000:18:00.0" || first[4] != "0000:05:00.0" {
		t.Fatalf("root complex 0 keeps the NVSwitch and takes the twin's addresses: %v", first)
	}
	l := mokka.Labels(p)
	if _, ok := l["nvidia.com/gpu.clique"]; ok || l["nvidia.com/gpu.count"] != "8" || l["nvidia.com/gpu.compute.major"] != "9" {
		t.Fatalf("labels: %v", l)
	}
}

func TestRenderRefusesWhatItCannotRender(t *testing.T) {
	e := labEngine(t)
	h, _ := hostAt(t, e.M, gb300)
	if _, err := mokka.Render(h, "gb300", []byte("not: a profile")); err == nil {
		t.Fatal("rendered over a document that is not an nvml-mock config")
	}
	mixed := h
	mixed.GPUs = append([]mokka.GPU{}, h.GPUs...)
	mixed.GPUs[1].Name = "NVIDIA GB200"
	b, _ := os.ReadFile("testdata/gb300.yaml")
	if _, err := mokka.Render(mixed, "gb300", b); err == nil {
		t.Fatal("rendered two products on one node")
	}
	if mokka.BaseFor("AMD Instinct MI355X") != "" || mokka.BaseFor("NVIDIA GB200") != "gb200" || mokka.BaseFor("NVIDIA H100 80GB HBM3") != "h100" {
		t.Fatal("BaseFor")
	}
}

// What a profile says of each GPU equals what the twin's dcgm-exporter impersonation serves for
// it: the NVML index, UUID, PCI bus id and model name, scraped from the running adapter.
func TestIdentityAgreesWithTheDCGMImpersonation(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{metrics.Name}, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	eps := map[string]*metrics.Endpoint{}
	for _, ep := range tw.Adapter(metrics.Name).(*metrics.Adapter).Endpoints() {
		eps[ep.Device.Path] = ep
	}
	for _, path := range []string{gb300, gb200, h100} {
		h, _ := hostAt(t, tw.Engine.M, path)
		ep := eps[path]
		if ep == nil {
			t.Fatalf("%s: no exporter", path)
		}
		var body []byte
		deadline := time.Now().Add(10 * time.Second)
		for {
			resp, err := http.Get("http://" + ep.Local + "/metrics")
			if err == nil {
				body, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: scrape: %v", path, err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		parser := expfmt.NewTextParser(pmodel.LegacyValidation)
		fams, err := parser.TextToMetricFamilies(strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		xid := fams["DCGM_FI_DEV_XID_ERRORS"]
		if xid == nil || len(xid.Metric) != len(h.GPUs) {
			t.Fatalf("%s: xid series %v", path, xid)
		}
		for _, m := range xid.Metric {
			l := map[string]string{}
			for _, lp := range m.Label {
				l[lp.GetName()] = lp.GetValue()
			}
			found := false
			for _, g := range h.GPUs {
				if l["gpu"] == strconv.Itoa(g.Index) {
					found = true
					if l["UUID"] != g.UUID || l["pci_bus_id"] != g.NVMLBusID() || l["modelName"] != g.Name || l["hostname"] != h.Node {
						t.Fatalf("%s gpu %d: exporter %v, profile %+v", path, g.Index, l, g)
					}
				}
			}
			if !found {
				t.Fatalf("%s: exporter gpu %s not in the profile", path, l["gpu"])
			}
		}
	}
}

func xid79() []mokka.Fault {
	return []mokka.Fault{{Fault: "nvidia/xid/xid-79", Title: "GPU has fallen off the bus (Xid 79)",
		Shims: []map[string]any{{"driver": "nvidia/mokka", "fault": "fallen-off-bus"}}}}
}

func TestFailureOf(t *testing.T) {
	for _, c := range []struct {
		name string
		g    mokka.GPUState
		want *mokka.Failure
	}{
		{"healthy", mokka.GPUState{Health: "OK", State: "Enabled"}, nil},
		{"xid-79 by its shim", mokka.GPUState{Health: "Critical", State: "UnavailableOffline", Faults: xid79()}, &mokka.Failure{Mode: mokka.ModeFallenOffBus, Xid: 79}},
		{"xid-48: a shim with a code only, on an addressable GPU", mokka.GPUState{Health: "Critical", State: "Enabled",
			Faults: []mokka.Fault{{Fault: "nvidia/xid/xid-48", Shims: []map[string]any{{"driver": "nvidia/mokka", "xid": 48}}}}},
			&mokka.Failure{Mode: mokka.ModeECCUncorrectable, Xid: 48}},
		{"xid-94: no shim, a Warning", mokka.GPUState{Health: "Warning", State: "Enabled",
			Faults: []mokka.Fault{{Fault: "nvidia/xid/xid-94"}}}, nil},
		{"offline without a shim is lost", mokka.GPUState{Health: "Critical", State: "UnavailableOffline",
			Faults: []mokka.Fault{{Fault: "generic/x/y"}}}, &mokka.Failure{Mode: mokka.ModeLost}},
		{"another driver's shim is not ours", mokka.GPUState{Health: "Critical", State: "Enabled",
			Faults: []mokka.Fault{{Fault: "amd/ras/ue", Shims: mokka.ShimsOf([]map[string]any{{"shim": map[string]any{"driver": "amd/smi", "fault": "lost"}}})}}}, nil},
	} {
		got := mokka.FailureOf(c.g)
		if (got == nil) != (c.want == nil) || got != nil && (got.Mode != c.want.Mode || got.Xid != c.want.Xid) {
			t.Fatalf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestOverridesDocument(t *testing.T) {
	doc := mokka.Overrides([]mokka.GPUState{{Index: 0, Health: "OK", State: "Enabled"},
		{Index: 2, Health: "Critical", State: "UnavailableOffline", Faults: xid79()}})
	var d struct {
		Version int
		Devices map[string]map[string]any
	}
	if err := yaml.Unmarshal(doc, &d); err != nil {
		t.Fatal(err)
	}
	if d.Version != 1 || len(d.Devices) != 1 || get(d.Devices["2"], "failure", "mode") != "fallen_off_bus" || get(d.Devices["2"], "failure", "xid", "code") != 79 {
		t.Fatalf("%s", doc)
	}
	var empty map[string]any
	_ = yaml.Unmarshal(mokka.Overrides([]mokka.GPUState{{Index: 0, Health: "OK"}}), &empty)
	if len(empty) != 1 || empty["version"] != 1 {
		t.Fatalf("a healthy host's document: %v", empty)
	}
}

func readDoc(t *testing.T, dir, node string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, node, "overrides.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The Syncer follows the engine: an Xid 79 on one GPU becomes that device's failure block on the
// tray's node, and clearing it empties the document, within the time events take to arrive.
func TestSyncerFollowsTheEngine(t *testing.T) {
	e := labEngine(t)
	h, _ := hostAt(t, e.M, gb300)
	h2, _ := hostAt(t, e.M, h100)
	dir := t.TempDir()
	var applied []string
	s := &mokka.Syncer{Source: mokka.EngineSource{E: e}, Sink: mokka.DirSink{Dir: dir}, Hosts: []mokka.Host{h, h2}, Resync: time.Hour,
		Applied: func(node string, _ []byte) { applied = append(applied, node) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, t.Logf)
	waitFor := func(what string, f func() bool) {
		t.Helper()
		for t0 := time.Now(); time.Since(t0) < 5*time.Second; time.Sleep(20 * time.Millisecond) {
			if f() {
				return
			}
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	waitFor("the first documents", func() bool {
		_, e1 := os.Stat(filepath.Join(dir, h.Node, "overrides.yaml"))
		_, e2 := os.Stat(filepath.Join(dir, h2.Node, "overrides.yaml"))
		return e1 == nil && e2 == nil
	})
	if strings.Contains(readDoc(t, dir, h.Node), "failure") {
		t.Fatalf("a healthy tray's document: %s", readDoc(t, dir, h.Node))
	}
	fs, err := e.Inject(h.GPUs[2].Path, "nvidia/xid/xid-79", nil, "", src)
	if err != nil {
		t.Fatal(err)
	}
	waitFor("the Xid 79 failure block", func() bool { return strings.Contains(readDoc(t, dir, h.Node), "fallen_off_bus") })
	doc := readDoc(t, dir, h.Node)
	if !strings.Contains(doc, `"2":`) || !strings.Contains(doc, "code: 79") {
		t.Fatalf("%s", doc)
	}
	if strings.Contains(readDoc(t, dir, h2.Node), "failure") {
		t.Fatal("the H100 server's document changed for the tray's fault")
	}
	if _, err := e.Clear(fs[0].ID, "", src); err != nil {
		t.Fatal(err)
	}
	waitFor("recovery", func() bool { return !strings.Contains(readDoc(t, dir, h.Node), "failure") })
	sort.Strings(applied)
	t.Logf("applied: %v", applied)
}

// A document replaced behind the Syncer's back is restored at the next resync, though the model
// has not changed: Mokka's node agent clears the override document whenever it starts (a node pod
// restart, a host reboot), and the twin cannot read what a node holds, so the resync applies every
// host's document again. Found by compat/fleet-intelligence: with the resync applying only what had
// changed since its own last write, a node agent restart left an Xid 79 GPU answering in NVML while
// the model held it lost, until the model next changed. Applied is told only of changes, so a
// restore is not one.
func TestSyncerRestoresADocumentReplacedBehindIt(t *testing.T) {
	e := labEngine(t)
	h, _ := hostAt(t, e.M, h100)
	dir := t.TempDir()
	var applied atomic.Int32
	s := &mokka.Syncer{Source: mokka.EngineSource{E: e}, Sink: mokka.DirSink{Dir: dir}, Hosts: []mokka.Host{h},
		Resync: 50 * time.Millisecond, Applied: func(string, []byte) { applied.Add(1) }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, t.Logf)
	file := filepath.Join(dir, h.Node, "overrides.yaml")
	failed := func() bool { b, _ := os.ReadFile(file); return strings.Contains(string(b), "fallen_off_bus") }
	waitFor := func(what string, f func() bool) {
		t.Helper()
		for t0 := time.Now(); time.Since(t0) < 5*time.Second; time.Sleep(10 * time.Millisecond) {
			if f() {
				return
			}
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	if _, err := e.Inject(h.GPUs[2].Path, "nvidia/xid/xid-79", nil, "", src); err != nil {
		t.Fatal(err)
	}
	waitFor("the Xid 79 failure block", failed)
	n := applied.Load()
	// what Mokka's node agent does to the document when it starts
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor("the document restored by the resync", failed)
	time.Sleep(200 * time.Millisecond) // a few more resyncs
	if got := applied.Load(); got != n {
		t.Fatalf("Applied was told of %d documents after the restore; it is told of changes only", got-n)
	}
}

// The control API source sees what the engine source sees, and refuses a host of another estate.
func TestControlSource(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{metrics.Name}, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	srv, err := control.Serve(tw, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	addr, token, err := mokka.ReadControl(filepath.Join(state, "control.json"))
	if err != nil {
		t.Fatal(err)
	}
	cs := mokka.ControlSource{Addr: addr, Token: token, Reg: reg}
	h, _ := hostAt(t, tw.Engine.M, gb300)
	ctx := context.Background()
	if err := cs.Check(ctx, h); err != nil {
		t.Fatal(err)
	}
	other := h
	other.UUID = "00000000-0000-4000-8000-000000000000"
	if err := cs.Check(ctx, other); err == nil {
		t.Fatal("accepted a host whose system UUID differs")
	}
	changes := cs.Changes(ctx)
	time.Sleep(500 * time.Millisecond) // the stream subscribes when its request arrives
	if _, err := tw.Engine.Inject(h.GPUs[1].Path, "nvidia/xid/xid-79", nil, "", src); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-time.After(5 * time.Second):
		t.Fatal("no change from the event stream")
	}
	viaAPI, err := cs.GPUs(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	viaEngine, _ := mokka.EngineSource{E: tw.Engine}.GPUs(ctx, h)
	a, b := string(mokka.Overrides(viaAPI)), string(mokka.Overrides(viaEngine))
	if a != b || !strings.Contains(a, `"1":`) || !strings.Contains(a, "code: 79") {
		t.Fatalf("control API:\n%s\nengine:\n%s", a, b)
	}
}

// envtestAssets finds a kube-apiserver and etcd, as the kwok adapter's tests do.
func envtestAssets() string {
	if d := os.Getenv("KUBEBUILDER_ASSETS"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	var cands []string
	for _, base := range []string{
		filepath.Join(home, "Library", "Application Support", "io.kubebuilder.envtest", "k8s"),
		filepath.Join(home, ".local", "share", "kubebuilder-envtest", "k8s"),
	} {
		ms, _ := filepath.Glob(filepath.Join(base, "*"))
		for _, m := range ms {
			if _, err := os.Stat(filepath.Join(m, "kube-apiserver")); err == nil {
				cands = append(cands, m)
			}
		}
	}
	sort.Strings(cands)
	if len(cands) == 0 {
		return ""
	}
	return cands[len(cands)-1]
}

// The GPU Feature Discovery labels a rendered profile implies equal the labels the twin's kwok
// projection writes for the same host (D43), except the machine: Mokka's node agent serves the
// GPU's name as the machine type, the projection the system's product name.
func TestLabelsAgreeWithTheKwokProjection(t *testing.T) {
	assets := envtestAssets()
	if assets == "" {
		t.Skip("no envtest binaries: set KUBEBUILDER_ASSETS (setup-envtest use -p path)")
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	user, err := env.AddUser(envtest.User{Name: "dc-twin-kwok", Groups: []string{"system:masters"}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	kc, err := user.KubeConfig()
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, kc, 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{kwok.Name}, StateDir: t.TempDir(),
		Outputs: map[string]string{kwok.OutputKubeconfig: kubeconfig}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	cs := kubernetes.NewForConfigOrDie(cfg)
	for _, path := range []string{gb300, gb200, h100} {
		h, d := hostAt(t, tw.Engine.M, path)
		p, _ := render(t, h)
		want := mokka.Labels(p)
		var got map[string]string
		for t0 := time.Now(); time.Since(t0) < 30*time.Second; time.Sleep(100 * time.Millisecond) {
			if n, err := cs.CoreV1().Nodes().Get(context.Background(), h.Node, metav1.GetOptions{}); err == nil {
				got = n.Labels
				break
			}
		}
		if got == nil {
			t.Fatalf("%s: no node %s", path, h.Node)
		}
		for k, v := range want {
			if k == "nvidia.com/gpu.machine" {
				if g := got[k]; g != strings.ReplaceAll(d.Profile.Spec.Identity.Model, " ", "-") {
					t.Fatalf("%s: kwok machine %q", path, g)
				}
				continue
			}
			if got[k] != v {
				t.Fatalf("%s: %s: kwok %q, the rendered profile implies %q", path, k, got[k], v)
			}
		}
		for k := range got {
			if strings.HasPrefix(k, "nvidia.com/gpu.") && want[k] == "" {
				t.Fatalf("%s: kwok writes %s=%s, which the profile does not imply", path, k, got[k])
			}
		}
	}
}

package kwok

import (
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

const labEstate = "../../../examples/estates/lab-model.yaml"

var src = schema.EventSource{Class: "user", Principal: "kwok-test"}

// labEngine compiles the lab estate into an engine with no adapters and no clock running:
// faults and power requests apply at once, and the projection is read directly.
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

func device(t *testing.T, e *engine.Engine, suffix string) *model.Device {
	t.Helper()
	for _, d := range e.M.Devices {
		if strings.HasSuffix(d.Path, "/"+suffix) {
			return d
		}
	}
	t.Fatalf("no device %s", suffix)
	return nil
}

func static(e *engine.Engine, d *model.Device) staticNode {
	return buildStatic(d, e.M.Name, e.M.Site, e.M.Seed, scaleUpDomain(e.M, d), "v1.37.0")
}

func cond(des desired, t corev1.NodeConditionType) corev1.NodeCondition {
	for _, c := range des.conditions {
		if c.Type == t {
			return c
		}
	}
	return corev1.NodeCondition{}
}

func TestStaticShapeOfEveryHost(t *testing.T) {
	e := labEngine(t)
	names := map[string]string{}
	for _, d := range e.M.Devices {
		if !d.IsHost() {
			continue
		}
		s := static(e, d)
		n := NodeName(d)
		if errs := validation.IsDNS1123Subdomain(n); len(errs) > 0 {
			t.Fatalf("%s: node name %q: %v", d.Path, n, errs)
		}
		if p, dup := names[n]; dup {
			t.Fatalf("%s and %s both map to %s", p, d.Path, n)
		}
		names[n] = d.Path
		for k, v := range s.labels {
			if errs := validation.IsQualifiedName(k); len(errs) > 0 {
				t.Fatalf("%s: label key %q: %v", d.Path, k, errs)
			}
			if errs := validation.IsValidLabelValue(v); len(errs) > 0 {
				t.Fatalf("%s: label %s=%q: %v", d.Path, k, v, errs)
			}
			if strings.Contains(v, "{") {
				t.Fatalf("%s: unexpanded placeholder in %s=%q", d.Path, k, v)
			}
		}
		if s.labels[corev1.LabelTopologyZone] != "hall-a" || s.labels[LabelRack] == "" || s.labels[LabelPath] == "" {
			t.Fatalf("%s: topology labels %v", d.Path, s.labels)
		}
	}
	if len(names) < 40 {
		t.Fatalf("only %d hosts", len(names))
	}
}

func TestProfileResourcesAndCliqueLabel(t *testing.T) {
	e := labEngine(t)
	gb300 := static(e, device(t, e, "a01/compute-tray-07"))
	if q := gb300.capacity["nvidia.com/gpu"]; q.Value() != 4 {
		t.Fatalf("gb300 nvidia.com/gpu = %v", q.String())
	}
	if gb300.nodeInfo.Architecture != "arm64" || gb300.labels[corev1.LabelArchStable] != "arm64" {
		t.Fatalf("a Grace tray is arm64: %v", gb300.nodeInfo.Architecture)
	}
	if q := gb300.capacity[corev1.ResourceCPU]; q.Value() != 144 {
		t.Fatalf("two 72-core Graces: cpu %v", q.String())
	}
	clique := gb300.labels["nvidia.com/gpu.clique"]
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.1$`).MatchString(clique) {
		t.Fatalf("clique %q is not GFD's <ClusterUUID>.<CliqueID>", clique)
	}
	// Every tray of a rack shares its NVL72 domain's clique; another rack's differs.
	if other := static(e, device(t, e, "a01/compute-tray-01")).labels["nvidia.com/gpu.clique"]; other != clique {
		t.Fatalf("trays of one rack: %q vs %q", clique, other)
	}
	if other := static(e, device(t, e, "a02/compute-tray-07")).labels["nvidia.com/gpu.clique"]; other == clique || other == "" {
		t.Fatalf("a02's clique %q must differ from a01's %q", other, clique)
	}
	if q := static(e, device(t, e, "b03/server-01")).capacity["amd.com/gpu"]; q.Value() != 8 {
		t.Fatalf("mi355x amd.com/gpu = %v", q.String())
	}
	if q := static(e, device(t, e, "b04/server-01")).capacity["habana.ai/gaudi"]; q.Value() != 8 {
		t.Fatalf("gaudi3 habana.ai/gaudi = %v", q.String())
	}
}

// GFD's labels come from the GPU component, the same one dcgm-exporter's modelName names, so a
// consumer joining a node to its metrics by product finds them equal as NVML makes them on a host.
func TestGPUFeatureDiscoveryLabels(t *testing.T) {
	e := labEngine(t)
	for _, c := range []struct{ path, product, count, family, memory string }{
		{"a01/compute-tray-07", "NVIDIA-GB300", "4", "blackwell", "284208"},
		{"a02/compute-tray-01", "NVIDIA-GB200", "4", "blackwell", "190464"},
		{"b01/server-01", "NVIDIA-H100-80GB-HBM3", "8", "hopper", "81559"},
		{"b05/server-02", "NVIDIA-A100-SXM4-80GB", "8", "ampere", "81920"},
	} {
		d := device(t, e, c.path)
		l := static(e, d).labels
		var model string
		for _, comp := range d.Components {
			if comp.Kind == "accelerator" {
				model = comp.Model
				break
			}
		}
		if l["nvidia.com/gpu.present"] != "true" || l["nvidia.com/gpu.product"] != c.product || l["nvidia.com/gpu.count"] != c.count ||
			l["nvidia.com/gpu.family"] != c.family || l["nvidia.com/gpu.memory"] != c.memory {
			t.Errorf("%s: present=%q product=%q count=%q family=%q memory=%q", c.path, l["nvidia.com/gpu.present"],
				l["nvidia.com/gpu.product"], l["nvidia.com/gpu.count"], l["nvidia.com/gpu.family"], l["nvidia.com/gpu.memory"])
		}
		if want := strings.ReplaceAll(model, " ", "-"); l["nvidia.com/gpu.product"] != want {
			t.Errorf("%s: gpu.product %q, dcgm modelName %q", c.path, l["nvidia.com/gpu.product"], model)
		}
	}
	for _, path := range []string{"b03/server-01", "b04/server-01", "c01/server-01"} {
		if l := static(e, device(t, e, path)).labels; l["nvidia.com/gpu.present"] != "" || l["nvidia.com/gpu.product"] != "" {
			t.Errorf("%s has no NVIDIA GPU but is labelled %v", path, l)
		}
	}
}

func TestConditionsFollowTheModel(t *testing.T) {
	e := labEngine(t)
	tray := device(t, e, "a01/compute-tray-07")
	s := static(e, tray)
	des := desire(s, tray.Path, observe(e, tray))
	if c := cond(des, corev1.NodeReady); c.Status != corev1.ConditionTrue || !des.up || des.cordon {
		t.Fatalf("healthy tray: %+v up=%v cordon=%v", c, des.up, des.cordon)
	}
	if c := cond(des, "GpuHealthy"); c.Status != corev1.ConditionTrue {
		t.Fatalf("GpuHealthy: %+v", c)
	}
	if _, err := e.Inject("a01/compute-tray-07/gpu-2", "nvidia/xid/xid-79", nil, "", src); err != nil {
		t.Fatal(err)
	}
	des = desire(s, tray.Path, observe(e, tray))
	// A failed accelerator does not stop the kubelet: the node stays Ready (so the
	// controller-manager's not-ready:NoExecute taint does not evict the healthy GPUs' pods), and
	// the accelerator condition and allocatable carry the fault.
	if ready := cond(des, corev1.NodeReady); ready.Status != corev1.ConditionTrue {
		t.Fatalf("after Xid 79: Ready %+v", ready)
	}
	if g := cond(des, "GpuHealthy"); g.Status != corev1.ConditionFalse || g.Reason != "AcceleratorCritical" || !strings.Contains(g.Message, "gpu-2") {
		t.Fatalf("after Xid 79: GpuHealthy %+v", g)
	}
	if q := des.allocatable["nvidia.com/gpu"]; q.Value() != 3 || !des.cordon {
		t.Fatalf("after Xid 79: allocatable %v cordon %v", q.String(), des.cordon)
	}

	// Critical from something other than an accelerator is the host's: NotReady.
	fanSrv := device(t, e, "b01/server-01")
	if _, err := e.Inject("b01/server-01/fan-0", "generic/thermal/fan-failure", nil, "", src); err != nil {
		t.Fatal(err)
	}
	des = desire(static(e, fanSrv), fanSrv.Path, observe(e, fanSrv))
	if c := cond(des, corev1.NodeReady); c.Status != corev1.ConditionFalse || c.Reason != ReasonCritical || !des.cordon {
		t.Fatalf("fan failed: Ready %+v cordon %v", c, des.cordon)
	}
	if c := cond(des, "GpuHealthy"); c.Status != corev1.ConditionTrue {
		t.Fatalf("fan failed: GpuHealthy %+v", c)
	}

	srv := device(t, e, "b01/server-02")
	ss := static(e, srv)
	if err := e.Power(engine.Request{Path: srv.Path, Canonical: "power.off.force", Source: src}); err != nil {
		t.Fatal(err)
	}
	des = desire(ss, srv.Path, observe(e, srv))
	if c := cond(des, corev1.NodeReady); c.Status != corev1.ConditionFalse || c.Reason != ReasonPoweredOff || des.up {
		t.Fatalf("powered off: %+v up=%v", c, des.up)
	}
	if c := cond(des, "GpuHealthy"); c.Status != corev1.ConditionUnknown {
		t.Fatalf("powered off: GpuHealthy %+v", c)
	}
}

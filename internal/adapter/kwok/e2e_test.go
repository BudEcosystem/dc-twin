package kwok_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	pmodel "github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/BudEcosystem/dc-twin/internal/adapter/kwok"
	"github.com/BudEcosystem/dc-twin/internal/adapter/metrics"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const labEstate = "../../../examples/estates/lab-model.yaml"

var src = schema.EventSource{Class: "user", Principal: "kwok-e2e"}

// envtestAssets finds a kube-apiserver and etcd: $KUBEBUILDER_ASSETS, else the newest version
// setup-envtest installed in its default directory.
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

func TestInactiveWithoutKubeconfig(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{kwok.Name}, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	r := tw.Adapter(kwok.Name).Report()
	if len(r.Impersonated) != 0 || len(r.Lines) != 1 || !strings.Contains(r.Lines[0], "inactive") {
		t.Fatalf("report without a kubeconfig: %+v", r)
	}
	t.Logf("report: %s", r.Lines[0])
}

// TestKwokAndMetricsEndToEnd runs the lab estate against a real kube-apiserver and etcd.
func TestKwokAndMetricsEndToEnd(t *testing.T) {
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
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, kc, 0o600); err != nil {
		t.Fatal(err)
	}
	cs := kubernetes.NewForConfigOrDie(cfg)
	kubectl := func(args ...string) string {
		t.Helper()
		// An explicit kubeconfig and cache: never the user's ~/.kube.
		full := append([]string{"--kubeconfig", kubeconfig, "--cache-dir", filepath.Join(dir, "kcache")}, args...)
		out, err := exec.Command(filepath.Join(assets, "kubectl"), full...).CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl %v: %v\n%s", args, err, out)
		}
		return string(out)
	}

	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	defer kwok.SetRenewEvery(kwok.SetRenewEvery(time.Second))
	start := time.Now()
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{kwok.Name, metrics.Name}, StateDir: t.TempDir(),
		Outputs: map[string]string{kwok.OutputKubeconfig: kubeconfig}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	e := tw.Engine
	hosts := 0
	for _, d := range e.M.Devices {
		if d.IsHost() {
			hosts++
		}
	}
	ctx := context.Background()
	waitFor := func(what string, limit time.Duration, f func() bool) time.Duration {
		t.Helper()
		t0 := time.Now()
		for time.Since(t0) < limit {
			if f() {
				return time.Since(t0)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out after %v waiting for %s", limit, what)
		return 0
	}
	waitFor(fmt.Sprintf("%d nodes", hosts), 30*time.Second, func() bool {
		l, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil || len(l.Items) != hosts {
			return false
		}
		for _, n := range l.Items {
			if len(n.Status.Conditions) == 0 {
				return false
			}
		}
		return true
	})
	t.Logf("%d nodes registered and posting status %v after up", hosts, time.Since(start).Round(time.Millisecond))

	node := func(name string) *corev1.Node {
		t.Helper()
		n, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	condition := func(n *corev1.Node, ct corev1.NodeConditionType) corev1.NodeCondition {
		for _, c := range n.Status.Conditions {
			if c.Type == ct {
				return c
			}
		}
		return corev1.NodeCondition{}
	}
	for name, want := range map[string]map[corev1.ResourceName]int64{
		"a01-compute-tray-07": {"nvidia.com/gpu": 4},
		"a02-compute-tray-01": {"nvidia.com/gpu": 4},
		"b01-server-01":       {"nvidia.com/gpu": 8},
		"b03-server-01":       {"amd.com/gpu": 8},
		"b04-server-02":       {"habana.ai/gaudi": 8},
	} {
		n := node(name)
		for rn, v := range want {
			c, a := n.Status.Capacity[rn], n.Status.Allocatable[rn]
			if c.Value() != v || a.Value() != v {
				t.Fatalf("%s: %s capacity %v allocatable %v, want %d", name, rn, c.String(), a.String(), v)
			}
		}
		if condition(n, corev1.NodeReady).Status != corev1.ConditionTrue || n.Annotations[kwok.AnnImpersonated] != "kwok" {
			t.Fatalf("%s: not Ready or not marked impersonated: %+v", name, n.Status.Conditions)
		}
	}
	lease, err := cs.CoordinationV1().Leases(corev1.NamespaceNodeLease).Get(ctx, "a01-compute-tray-07", metav1.GetOptions{})
	if err != nil || lease.Spec.RenewTime == nil {
		t.Fatalf("node lease: %v %v", lease, err)
	}
	first := lease.Spec.RenewTime.Time
	waitFor("the lease to be renewed", 5*time.Second, func() bool {
		l, err := cs.CoordinationV1().Leases(corev1.NamespaceNodeLease).Get(ctx, "a01-compute-tray-07", metav1.GetOptions{})
		return err == nil && l.Spec.RenewTime != nil && l.Spec.RenewTime.Time.After(first)
	})

	t.Logf("$ kubectl get nodes -L nvidia.com/gpu.product -L amd.com/gpu.product-name\n%s",
		kubectl("get", "nodes", "-L", "nvidia.com/gpu.product", "-L", "amd.com/gpu.product-name", "-L", "habana.ai/gaudi.product"))

	// Metrics: every exporter, every accelerator's temperature, parsed with expfmt.
	ma := tw.Adapter(metrics.Name).(*metrics.Adapter)
	tempFamily := map[string][2]string{"dcgm": {"DCGM_FI_DEV_GPU_TEMP", "gpu"}, "amd": {"gpu_junction_temperature", "gpu_id"}, "gaudi": {"habanalabs_temperature_onchip", "id"}}
	sampled := map[string]bool{}
	var trayEP *metrics.Endpoint
	for _, ep := range ma.Endpoints() {
		fams, body, err := scrape(ep.Local)
		if err != nil {
			t.Fatalf("scrape %s: %v", ep.Site, err)
		}
		accels := 0
		for _, c := range ep.Device.Components {
			if c.Kind == "accelerator" {
				accels++
			}
		}
		tf := tempFamily[ep.Exporter.ID]
		if f := fams[tf[0]]; f == nil || len(f.Metric) != accels {
			t.Fatalf("%s: want %d %s series", ep.Site, accels, tf[0])
		}
		if !sampled[ep.Exporter.ID] {
			sampled[ep.Exporter.ID] = true
			t.Logf("%s sample (%s, %s):\n%s", ep.Exporter.Impersonator, ep.Site, ep.Device.Path, sample(body, tf[0]))
		}
		if strings.HasSuffix(ep.Device.Path, "/a01/compute-tray-07") {
			trayEP = ep
		}
	}
	if len(sampled) != 3 {
		t.Fatalf("exporters served: %v", sampled)
	}

	// Xid 79 on a GB300 tray's GPU: the node stays Ready (a kubelet keeps posting after an Xid),
	// its GPU condition goes False, one GPU is withheld and the twin cordons it, within seconds.
	const tray = "a01-compute-tray-07"
	t0 := time.Now()
	if _, err := e.Inject("a01/compute-tray-07/gpu-2", "nvidia/xid/xid-79", nil, "", src); err != nil {
		t.Fatal(err)
	}
	took := waitFor(tray+" GPU unhealthy and cordoned", 10*time.Second, func() bool {
		n := node(tray)
		return condition(n, "GpuHealthy").Status == corev1.ConditionFalse && n.Spec.Unschedulable
	})
	n := node(tray)
	if r := condition(n, corev1.NodeReady); r.Status != corev1.ConditionTrue {
		t.Fatalf("Ready after Xid 79: %+v", r)
	}
	if g := condition(n, "GpuHealthy"); g.Status != corev1.ConditionFalse {
		t.Fatalf("GpuHealthy after Xid 79: %+v", g)
	}
	if a := n.Status.Allocatable["nvidia.com/gpu"]; a.Value() != 3 {
		t.Fatalf("allocatable nvidia.com/gpu after Xid 79: %v", a.String())
	}
	hasTaint := false
	for _, tn := range n.Spec.Taints {
		hasTaint = hasTaint || tn.Key == kwok.TaintUnschedulable
	}
	if !hasTaint {
		t.Fatalf("no unschedulable taint: %+v", n.Spec.Taints)
	}
	t.Logf("Xid 79 injected -> %s GpuHealthy=False, Ready, unschedulable, allocatable nvidia.com/gpu=3 after %v (inject to observed %v)",
		tray, took.Round(time.Millisecond), time.Since(t0).Round(time.Millisecond))
	t.Logf("$ kubectl describe node %s\n%s", tray, kubectl("describe", "node", tray))

	fams, body, err := scrape(trayEP.Local)
	if err != nil {
		t.Fatal(err)
	}
	x := series(fams["DCGM_FI_DEV_XID_ERRORS"], "gpu", "2")
	if x == nil || x.GetGauge().GetValue() != 79 {
		t.Fatalf("DCGM_FI_DEV_XID_ERRORS{gpu=\"2\"}: %v", x)
	}
	t.Logf("dcgm-exporter after Xid 79 (%s):\n%s", trayEP.Site, sample(body, "DCGM_FI_DEV_XID_ERRORS"))

	// Recovery: clearing the fault makes the node Ready, schedulable and whole again.
	if _, err := e.Clear("a01/compute-tray-07/gpu-2", "nvidia/xid/xid-79", src); err != nil {
		t.Fatal(err)
	}
	took = waitFor(tray+" Ready and uncordoned", 10*time.Second, func() bool {
		n := node(tray)
		a := n.Status.Allocatable["nvidia.com/gpu"]
		return condition(n, corev1.NodeReady).Status == corev1.ConditionTrue && !n.Spec.Unschedulable && a.Value() == 4
	})
	t.Logf("fault cleared -> %s Ready, schedulable, allocatable nvidia.com/gpu=4 after %v", tray, took.Round(time.Millisecond))

	// A cordon the stack made is the stack's: the twin neither takes it over nor lifts it.
	const cordoned = "a01-compute-tray-05"
	t.Logf("$ kubectl cordon %s\n%s", cordoned, kubectl("cordon", cordoned))
	if _, err := e.Inject("a01/compute-tray-05/gpu-0", "nvidia/xid/xid-79", nil, "", src); err != nil {
		t.Fatal(err)
	}
	waitFor(cordoned+" GPU unhealthy", 10*time.Second, func() bool {
		return condition(node(cordoned), "GpuHealthy").Status == corev1.ConditionFalse
	})
	if _, err := e.Clear("a01/compute-tray-05/gpu-0", "nvidia/xid/xid-79", src); err != nil {
		t.Fatal(err)
	}
	waitFor(cordoned+" GPU healthy", 10*time.Second, func() bool {
		return condition(node(cordoned), "GpuHealthy").Status == corev1.ConditionTrue
	})
	if n := node(cordoned); !n.Spec.Unschedulable || n.Annotations[kwok.AnnCordoned] != "" {
		t.Fatalf("the stack's cordon on %s was taken over or lifted: unschedulable=%v annotations=%v", cordoned, n.Spec.Unschedulable, n.Annotations)
	}
	t.Logf("%s: the stack's cordon survived a twin cordon cycle (Xid 79 then clear)", cordoned)

	// Power off a server: Ready=False with the model's reason.
	const srv = "b01-server-02"
	if err := e.Power(engine.Request{Path: "site-1/hall-a/b01/server-02", Canonical: "power.off.graceful", Source: src}); err != nil {
		t.Fatal(err)
	}
	took = waitFor(srv+" Ready=False PoweredOff", 15*time.Second, func() bool {
		c := condition(node(srv), corev1.NodeReady)
		return c.Status == corev1.ConditionFalse && c.Reason == kwok.ReasonPoweredOff
	})
	c := condition(node(srv), corev1.NodeReady)
	t.Logf("power.off.graceful -> %s Ready=%s reason=%s message=%q after %v", srv, c.Status, c.Reason, c.Message, took.Round(time.Millisecond))
	// A host that is off has no kubelet: its lease is no longer renewed.
	l0, err := cs.CoordinationV1().Leases(corev1.NamespaceNodeLease).Get(ctx, srv, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	l1, err := cs.CoordinationV1().Leases(corev1.NamespaceNodeLease).Get(ctx, srv, metav1.GetOptions{})
	if err != nil || !l1.Spec.RenewTime.Time.Equal(l0.Spec.RenewTime.Time) {
		t.Fatalf("a powered-off host's lease was renewed: %v -> %v (%v)", l0.Spec.RenewTime, l1.Spec.RenewTime, err)
	}

	t.Logf("$ kubectl get node a01-compute-tray-06 -o jsonpath={.spec.taints} (a healthy node; no node-lifecycle controller runs in envtest)\n%s",
		kubectl("get", "node", "a01-compute-tray-06", "-o", "jsonpath={.spec.taints}"))
	t.Logf("$ kubectl get nodes (after)\n%s", kubectl("get", "nodes", cordoned, "a01-compute-tray-06", tray, "b01-server-01", srv, "b03-server-01", "b04-server-02"))
	t.Logf("$ kubectl get leases -n kube-node-lease (head)\n%s", headLines(kubectl("get", "leases", "-n", corev1.NamespaceNodeLease), 6))
	for _, ad := range []string{kwok.Name, metrics.Name} {
		r := tw.Adapter(ad).Report()
		t.Logf("report %s: served=%d impersonated=%v\n  %s", r.Adapter, r.Served, r.Impersonated, strings.Join(r.Lines, "\n  "))
	}
}

func scrape(addr string) (map[string]*dto.MetricFamily, []byte, error) {
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + addr + "/metrics")
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	p := expfmt.NewTextParser(pmodel.LegacyValidation)
	fams, err := p.TextToMetricFamilies(bytes.NewReader(body))
	return fams, body, err
}

func series(f *dto.MetricFamily, label, value string) *dto.Metric {
	if f == nil {
		return nil
	}
	for _, m := range f.Metric {
		for _, l := range m.Label {
			if l.GetName() == label && l.GetValue() == value {
				return m
			}
		}
	}
	return nil
}

// sample is one family's lines from a scrape.
func sample(body []byte, fam string) string {
	var out []string
	for _, l := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(l, fam+"{") || strings.HasPrefix(l, "# HELP "+fam+" ") || strings.HasPrefix(l, "# TYPE "+fam+" ") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func headLines(s string, n int) string {
	ls := strings.Split(s, "\n")
	if len(ls) > n {
		ls = ls[:n]
	}
	return strings.Join(ls, "\n")
}

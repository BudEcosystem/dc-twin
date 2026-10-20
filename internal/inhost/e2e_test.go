package inhost_test

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/BudEcosystem/dc-twin/internal/adapter/vm"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/inhost"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// The host the test boots, by this machine's architecture: the GB300 tray of gb300-vm on arm64,
// one of the lab estate's Dell XE9680s on x86_64 (a guest of the host's architecture runs
// accelerated; any other would need TCG).
type vmCase struct {
	estate, host, node string
	gpus               int64
	override           *schema.FidelityOverride
}

var vmCases = map[string]vmCase{
	"aarch64": {estate: "../../examples/estates/gb300-vm.yaml", host: "site-1/hall-a/a01/compute-tray-01", node: "a01-compute-tray-01", gpus: 4},
	"x86_64": {estate: "../../examples/estates/lab-model.yaml", host: "site-1/hall-a/b01/server-01", node: "b01-server-01", gpus: 8,
		override: &schema.FidelityOverride{Select: "b01/server-01", Level: "vm", OS: "twin"}},
}

var user = schema.EventSource{Class: "user", Principal: "inhost-e2e"}

// TestInHostAgent boots a GPU host (vmCases) as a real virtual machine with the twin's guest and
// drives the in-host agent end to end: the agent attaches over
// the serial console, an injected Xid 79 lands in the guest's own kernel ring buffer and comes
// back out as a kernel record and a journal entry, and a reset of the machine is a new boot ID.
// With envtest's binaries it also checks the Node the agent keeps for the host (impersonated,
// D24): Ready, the guest's boot ID, allocatable down by one GPU after the Xid, and a new boot ID
// after the reset.
func TestInHostAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a virtual machine")
	}
	if _, err := qemu.Find(qemu.HostArch(), "", false); err != nil {
		t.Skipf("needs QEMU for an accelerated guest: %v", err)
	}
	g := vm.Guests[qemu.HostArch()]
	vc, ok := vmCases[qemu.HostArch()]
	if g.Name == "" || !ok || g.Present(vm.GuestDir(twin.AssetsDir(), "", g)) != nil {
		t.Skipf("needs the twin's guest image for %s (dctwin assets fetch)", qemu.HostArch())
	}
	vmTray := vc.host
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(vc.estate)
	if err != nil {
		t.Fatal(err)
	}
	if vc.override != nil {
		est.Spec.Fidelity.Overrides = append(est.Spec.Fidelity.Overrides, *vc.override)
		est.Spec.Adapters.VM.MemoryMB, est.Spec.Adapters.VM.CPUs = 1024, 2
	}
	outputs := map[string]string{}
	var cs kubernetes.Interface
	if assets := envtestAssets(); assets != "" {
		env := &envtest.Environment{BinaryAssetsDirectory: assets}
		cfg, err := env.Start()
		if err != nil {
			t.Fatalf("envtest: %v", err)
		}
		t.Cleanup(func() { _ = env.Stop() })
		u, err := env.AddUser(envtest.User{Name: "dc-twin-inhost", Groups: []string{"system:masters"}}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		kc, err := u.KubeConfig()
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), "kubeconfig")
		if err := os.WriteFile(p, kc, 0o600); err != nil {
			t.Fatal(err)
		}
		outputs[inhost.OutputKubeconfig] = p
		cs = kubernetes.NewForConfigOrDie(cfg)
	} else {
		t.Log("no envtest binaries: the Node projection is not checked")
	}
	tw, err := twin.UpEstate(est, vc.estate, reg, twin.Options{Adapters: []string{"vm", inhost.Name}, StateDir: t.TempDir(), Outputs: outputs})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	a := tw.Adapter(inhost.Name).(*inhost.Adapter)
	waitFor := func(what string, limit time.Duration, f func() bool) {
		t.Helper()
		for t0 := time.Now(); time.Since(t0) < limit; time.Sleep(200 * time.Millisecond) {
			if f() {
				return
			}
		}
		t.Fatalf("timed out after %v waiting for %s; agent %+v", limit, what, a.Guests())
	}
	guest := func() inhost.Guest { return a.Guests()[0] }
	waitFor("the agent to attach", 180*time.Second, func() bool { return guest().Attached })
	boot0 := guest().Hello.Boot
	t.Logf("attached: boot %s, kernel %s %s, %d CPUs, %d KiB; %d kernel records so far", boot0, guest().Hello.Kernel, guest().Hello.Arch, guest().Hello.CPUs, guest().Hello.MemKiB, guest().Records)
	if guest().Hello.CPUs < 1 || guest().Hello.MemKiB < 1 || guest().Hello.Kernel == "" {
		t.Fatalf("hello %+v", guest().Hello)
	}
	dir := a.Dir(vmTray)
	// with the agent's kernel module built for the image (internal/inhost/kmod/build.sh) the line is
	// a kernel record; without it, a /dev/kmsg record the journal entry marks (D50)
	_, err = os.Stat(filepath.Join(vm.GuestDir(twin.AssetsDir(), "", g), inhost.KernelModule))
	kmod := err == nil
	if want := map[bool]string{true: "loaded", false: "absent"}[kmod]; guest().Hello.Kmod != want {
		t.Fatalf("kernel module %q, want %q", guest().Hello.Kmod, want)
	}
	t.Logf("kernel module: %s", guest().Hello.Kmod)

	// an Xid 79 on gpu-2: the catalogue's lines, with gpu-2's PCI address, in the guest's kernel log
	want := "NVRM: Xid (PCI:" + strings.TrimSuffix(inhost.PCIAddress(2), ".0") + "): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus."
	if _, err := tw.Engine.Inject(vmTray+"/gpu-2", "nvidia/xid/xid-79", nil, "", user); err != nil {
		t.Fatal(err)
	}
	waitFor("the Xid line in the guest's kernel log", 30*time.Second, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "kmsg.log"))
		return strings.Contains(string(b), boot0+" ") && strings.Contains(string(b), ";"+want)
	})
	var e map[string]string
	waitFor("its journal entry", 10*time.Second, func() bool {
		for _, x := range readExport(t, filepath.Join(dir, "journal.export")) {
			if x["MESSAGE"] == want {
				e = x
				return true
			}
		}
		return false
	})
	wantEntry := map[string]string{"_TRANSPORT": "kernel", "SYSLOG_IDENTIFIER": "kernel", "SYSLOG_FACILITY": "0", "PRIORITY": "4",
		"_BOOT_ID": strings.ReplaceAll(boot0, "-", ""), "_HOSTNAME": vc.node, "DCTWIN_FAULT": "", "DCTWIN_KMSG_FACILITY": ""}
	if !kmod {
		wantEntry["DCTWIN_FAULT"], wantEntry["DCTWIN_KMSG_FACILITY"] = "nvidia/xid/xid-79", "1"
	}
	for k, v := range wantEntry {
		if e[k] != v {
			t.Errorf("journal entry %s=%q, want %q", k, e[k], v)
		}
	}
	// the record itself: priority 4 (kern, warning) from the module, 12 (user, warning) through /dev/kmsg
	b, _ := os.ReadFile(filepath.Join(dir, "kmsg.log"))
	prio := map[bool]string{true: " 4,", false: " 12,"}[kmod]
	if !strings.Contains(string(b), prio) || !regexp.MustCompile(regexp.QuoteMeta(boot0+prio)+`[0-9]+,[0-9]+,[^;]*;`+regexp.QuoteMeta(want)).Match(b) {
		t.Errorf("no record of priority%s holding the Xid line in kmsg.log", strings.TrimSuffix(prio, ","))
	}
	var seen []string
	for _, ev := range tw.Engine.Events(0, func(ev schema.Event) bool {
		return ev.Kind == "observe" && ev.Source.Principal == "inhost" && ev.State != nil && ev.State.Field == "kernelLog"
	}) {
		seen = append(seen, ev.State.To.(string))
	}
	// the catalogue's Xid 79 writes two lines (the GPU's identity, then the Xid): each written, each recorded
	sort.Strings(seen)
	if strings.Join(seen, ",") != "recorded,recorded,written,written" {
		t.Fatalf("kernelLog observations %v, want two written and two recorded", seen)
	}

	ctx := context.Background()
	node := func() *corev1.Node {
		n, err := cs.CoreV1().Nodes().Get(ctx, vc.node, metav1.GetOptions{})
		if err != nil {
			return nil
		}
		return n
	}
	ready := func(n *corev1.Node) bool {
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				return c.Status == corev1.ConditionTrue
			}
		}
		return false
	}
	if cs != nil {
		waitFor("the Node to be Ready with the guest's boot ID and one GPU fewer", 30*time.Second, func() bool {
			n := node()
			if n == nil || !ready(n) || n.Status.NodeInfo.BootID != boot0 {
				return false
			}
			q := n.Status.Allocatable["nvidia.com/gpu"]
			return q.Value() == vc.gpus-1
		})
		n := node()
		if n.Annotations["dc-twin.io/impersonated"] != "inhost" || n.Labels["nvidia.com/gpu.present"] != "true" || n.Spec.Unschedulable {
			t.Fatalf("node %s: annotations %v labels %v unschedulable %v", n.Name, n.Annotations, n.Labels, n.Spec.Unschedulable)
		}
		if _, err := cs.CoordinationV1().Leases(corev1.NamespaceNodeLease).Get(ctx, n.Name, metav1.GetOptions{}); err != nil {
			t.Fatalf("lease: %v", err)
		}
		t.Logf("node %s Ready, boot %s, kernel %s, capacity cpu %s memory %s nvidia.com/gpu %s (allocatable %s)", n.Name, n.Status.NodeInfo.BootID, n.Status.NodeInfo.KernelVersion,
			n.Status.Capacity.Cpu(), n.Status.Capacity.Memory(), ptr(n.Status.Capacity["nvidia.com/gpu"]), ptr(n.Status.Allocatable["nvidia.com/gpu"]))
	}

	// before any reboot, no boot-time line: the driver logs it when it probes the lost GPU
	fallen := "NVRM: The NVIDIA GPU " + inhost.PCIAddress(2) + " installed in this system has fallen off the bus and is not responding to commands."
	if strings.Contains(string(b), fallen) {
		t.Fatalf("the boot-time line is in the kernel log before a reboot")
	}

	// a reset: the machine's reset line, a new boot, a new boot ID; the Xid 79 stays (only an AC
	// cycle clears it) and is not logged again, but the new boot logs the GPU as fallen off the bus
	if err := tw.Engine.Power(engine.Request{Path: vmTray, Canonical: "power.restart", Protocol: "twin", Source: user}); err != nil {
		t.Fatal(err)
	}
	waitFor("the agent of a new boot", 180*time.Second, func() bool { return guest().Attached && guest().Hello.Boot != boot0 })
	boot1 := guest().Hello.Boot
	if cs != nil {
		waitFor("the Node's new boot ID", 30*time.Second, func() bool { n := node(); return n != nil && ready(n) && n.Status.NodeInfo.BootID == boot1 })
	}
	b, _ = os.ReadFile(filepath.Join(dir, "kmsg.log"))
	if !strings.Contains(string(b), boot1+" 6,0,0,") && !strings.Contains(string(b), boot1+" 5,0,0,") {
		t.Fatalf("the new boot's kernel log does not start at record 0")
	}
	if strings.Count(string(b), want) != 1 {
		t.Fatalf("the Xid line is in the kernel log %d times", strings.Count(string(b), want))
	}
	waitFor("the boot-time fallen-off-the-bus line in the new boot's kernel log", 30*time.Second, func() bool {
		b, _ = os.ReadFile(filepath.Join(dir, "kmsg.log"))
		return regexp.MustCompile(regexp.QuoteMeta(boot1+" ") + `[0-9]+,[0-9]+,[0-9]+,[^;]*;` + regexp.QuoteMeta(fallen)).Match(b)
	})
	if n := strings.Count(string(b), fallen); n != 1 {
		t.Fatalf("the boot-time line is in the kernel log %d times", n)
	}
	t.Logf("reset: boot %s → %s", boot0, boot1)
	r := a.Report()
	if (cs != nil) != (len(r.Impersonated) == 1) || r.Served != 1 {
		t.Fatalf("report %+v", r)
	}
	for _, l := range r.Lines {
		t.Log(l)
	}
}

func ptr[T any](v T) *T { return &v }

// readExport reads a journal export file's text fields (the twin writes no binary ones here).
func readExport(t *testing.T, p string) []map[string]string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []map[string]string
	cur := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := sc.Text()
		if l == "" {
			if len(cur) > 0 {
				out = append(out, cur)
			}
			cur = map[string]string{}
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			cur[k] = v
		}
	}
	return out
}

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

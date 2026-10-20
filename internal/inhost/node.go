package inhost

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/BudEcosystem/dc-twin/internal/adapter/kwok"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// OutputKubeconfig is the stack output naming the cluster the twin registers Nodes in (the kwok
// projection's credential, D24).
const OutputKubeconfig = kwok.OutputKubeconfig

const (
	fieldManager = "dc-twin-inhost"
	// AnnImpersonated's value on the Nodes this adapter registers (kwok's annotation, D24).
	impersonator = "inhost"
	leaseSeconds = 40
	renewEvery   = 10 * time.Second
	reportEvery  = time.Minute
)

// nodes is the Kubernetes view of `os: twin` hosts. The kwok projection registers `model` hosts,
// whose liveness is the model's; a guest that boots has its own, so its Node is this adapter's:
// registered when the guest's agent first says hello (as a kubelet registers once the OS is up),
// Ready while the agent of the running boot is attached and the machine is on, its boot ID and
// kernel the guest's own, its CPU and memory capacity what the guest's kernel counts. The twin
// never cordons or taints it: that is the stack's to decide.
type nodes struct {
	a              *Adapter
	cs             kubernetes.Interface
	server         string
	kubeletVersion string
	estate         string

	mu         sync.Mutex
	dirty      map[*guest]bool
	state      map[*guest]*nodeState
	registered int
	errCount   int
	lastErr    string
	wake       chan struct{}
}

type nodeState struct {
	exists    bool
	uid       types.UID
	labelsFP  string
	statusFP  string
	statusAt  time.Time
	ready     bool
	lease     *coordinationv1.Lease
	condSince map[corev1.NodeConditionType]metav1.Time
	condWas   map[corev1.NodeConditionType]corev1.ConditionStatus
}

func newNodes(a *Adapter, kc string) (*nodes, error) {
	var cfg *rest.Config
	var err error
	if strings.Contains(kc, "apiVersion:") || strings.Contains(kc, "clusters:") {
		cfg, err = clientcmd.RESTConfigFromKubeConfig([]byte(kc))
	} else {
		// an explicit path only, never $KUBECONFIG or ~/.kube/config
		cfg, err = clientcmd.BuildConfigFromFlags("", kc)
	}
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	cfg.UserAgent = "dc-twin-inhost"
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	n := &nodes{a: a, cs: cs, kubeletVersion: "v0.0.0-dc-twin", dirty: map[*guest]bool{}, state: map[*guest]*nodeState{}, wake: make(chan struct{}, 1)}
	if u, err := url.Parse(cfg.Host); err == nil {
		n.server = u.Host
	}
	if v, err := cs.Discovery().ServerVersion(); err == nil {
		n.kubeletVersion = v.GitVersion
	}
	a.rt.Engine.Read(func(m *model.Model) { n.estate = m.Name })
	for _, g := range a.guests {
		n.state[g] = &nodeState{condSince: map[corev1.NodeConditionType]metav1.Time{}, condWas: map[corev1.NodeConditionType]corev1.ConditionStatus{}}
	}
	return n, nil
}

// mark asks for a guest's Node to be brought up to date.
func (n *nodes) mark(g *guest) {
	n.mu.Lock()
	n.dirty[g] = true
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (n *nodes) run(ctx context.Context) {
	defer n.a.wg.Done()
	t := time.NewTicker(renewEvery)
	defer t.Stop()
	for {
		renew := false
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			renew = true
			n.mu.Lock()
			for _, g := range n.a.guests {
				n.dirty[g] = true
			}
			n.mu.Unlock()
		case <-n.wake:
		}
		n.mu.Lock()
		var batch []*guest
		for g := range n.dirty {
			batch = append(batch, g)
		}
		n.dirty = map[*guest]bool{}
		n.mu.Unlock()
		for _, g := range batch {
			if err := n.sync(ctx, g, renew); err != nil && ctx.Err() == nil {
				n.mu.Lock()
				n.errCount++
				n.lastErr = nodeName(g.d) + ": " + err.Error()
				n.mu.Unlock()
				n.a.rt.Log.Warn("inhost: node write failed", "node", nodeName(g.d), "err", err)
			}
		}
	}
}

// desired is what the Node should say.
type desired struct {
	labels, annotations map[string]string
	capacity, alloc     corev1.ResourceList
	conditions          []corev1.NodeCondition
	info                corev1.NodeSystemInfo
	ready               bool
}

func (n *nodes) desire(g *guest) (desired, bool) {
	g.mu.Lock()
	h, attached := g.info, g.attached
	g.mu.Unlock()
	if h.Boot == "" {
		return desired{}, false // the guest has never booted far enough for a kubelet to register
	}
	var power string
	gpus, healthy := 0, 0
	var product string
	n.a.rt.Engine.Read(func(*model.Model) { power = g.d.State.Power })
	for _, c := range g.d.Components {
		if c.Kind != "accelerator" || c.Vendor != "nvidia" {
			continue
		}
		gpus++
		if product == "" {
			product = c.Model
		}
		if hl, st := n.a.rt.Engine.ComponentHealth(c); hl != model.Critical && st != "UnavailableOffline" {
			healthy++
		}
	}
	d := desired{ready: attached && power == model.On}
	name := nodeName(g.d)
	arch := map[string]string{"aarch64": "arm64", "x86_64": "amd64"}[h.Arch]
	if arch == "" {
		arch = h.Arch
	}
	d.labels = map[string]string{
		corev1.LabelHostname: name, corev1.LabelOSStable: "linux", corev1.LabelArchStable: arch,
		kwok.LabelPath: labelValue(g.d.Path), kwok.LabelEstate: labelValue(n.estate), kwok.LabelProfile: labelValue(g.d.ProfileName),
	}
	if r := g.d.RackOf(); r != nil {
		d.labels[kwok.LabelRack] = labelValue(r.Name)
	}
	if gpus > 0 {
		d.labels["nvidia.com/gpu.present"] = "true"
		d.labels["nvidia.com/gpu.count"] = strconv.Itoa(gpus)
		d.labels["nvidia.com/gpu.product"] = labelValue(strings.ReplaceAll(product, " ", "-"))
	}
	d.annotations = map[string]string{kwok.AnnImpersonated: impersonator, kwok.AnnPath: g.d.Path}
	cpus := h.CPUs
	if cpus <= 0 {
		cpus = 1
	}
	d.capacity = corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(cpus), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(h.MemKiB*1024, resource.BinarySI),
		corev1.ResourcePods:   *resource.NewQuantity(110, resource.DecimalSI),
	}
	d.alloc = d.capacity.DeepCopy()
	if gpus > 0 {
		d.capacity["nvidia.com/gpu"] = *resource.NewQuantity(int64(gpus), resource.DecimalSI)
		d.alloc["nvidia.com/gpu"] = *resource.NewQuantity(int64(healthy), resource.DecimalSI)
	}
	ready := corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady", Message: "kubelet is posting ready status"}
	if !d.ready {
		why := "the guest's in-host agent is not attached (booting, reset or stopped)"
		if power != model.On {
			why = "the machine is " + strings.ToLower(power)
		}
		ready = corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse, Reason: "KubeletNotReady", Message: why}
	}
	d.conditions = []corev1.NodeCondition{
		{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, Reason: "KubeletHasSufficientMemory", Message: "kubelet has sufficient memory available"},
		{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse, Reason: "KubeletHasNoDiskPressure", Message: "kubelet has no disk pressure"},
		{Type: corev1.NodePIDPressure, Status: corev1.ConditionFalse, Reason: "KubeletHasSufficientPID", Message: "kubelet has sufficient PID available"},
		ready,
	}
	d.info = corev1.NodeSystemInfo{BootID: h.Boot, KernelVersion: h.Kernel, OSImage: "dc-twin os: twin guest (Alpine Linux netboot)",
		OperatingSystem: "linux", Architecture: arch, KubeletVersion: n.kubeletVersion, SystemUUID: g.d.Identity.UUID}
	return d, true
}

func (n *nodes) sync(ctx context.Context, g *guest, renew bool) error {
	d, ok := n.desire(g)
	if !ok {
		return nil
	}
	st := n.state[g]
	api := n.cs.CoreV1().Nodes()
	name := nodeName(g.d)
	now := metav1.Now()
	if !st.exists {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: d.labels, Annotations: d.annotations}}
		created, err := api.Create(ctx, node, metav1.CreateOptions{FieldManager: fieldManager})
		switch {
		case err == nil:
			st.uid = created.UID
		case apierrors.IsAlreadyExists(err):
			cur, err := api.Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			st.uid = cur.UID
			for _, c := range cur.Status.Conditions {
				st.condSince[c.Type], st.condWas[c.Type] = c.LastTransitionTime, c.Status
			}
		default:
			return err
		}
		st.exists, st.labelsFP, st.statusFP = true, fingerprint(d.labels, d.annotations), ""
		n.mu.Lock()
		n.registered++
		n.mu.Unlock()
	}
	if fp := fingerprint(d.labels, d.annotations); fp != st.labelsFP {
		// a merge patch of the twin's own keys: labels and annotations the stack adds stay
		body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"labels": d.labels, "annotations": d.annotations}})
		if _, err := api.Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{FieldManager: fieldManager}); err != nil {
			return n.lost(st, err)
		}
		st.labelsFP = fp
	}
	fp := fingerprint(d.ready, d.capacity, d.alloc, d.info, d.conditions)
	if fp != st.statusFP || (d.ready && time.Since(st.statusAt) >= reportEvery) {
		conds := make([]corev1.NodeCondition, 0, len(d.conditions))
		for _, c := range d.conditions {
			since, ok := st.condSince[c.Type]
			if !ok || st.condWas[c.Type] != c.Status {
				since = now
			}
			st.condSince[c.Type], st.condWas[c.Type] = since, c.Status
			c.LastHeartbeatTime, c.LastTransitionTime = now, since
			conds = append(conds, c)
		}
		// a strategic-merge patch of the status subresource, as a kubelet posts it: conditions
		// other components own (NVSentinel's, by type) are left alone
		body, _ := json.Marshal(map[string]any{"status": map[string]any{
			"capacity": d.capacity, "allocatable": d.alloc, "conditions": conds, "nodeInfo": d.info,
			"addresses": []corev1.NodeAddress{{Type: corev1.NodeHostName, Address: name}},
		}})
		if _, err := api.Patch(ctx, name, types.StrategicMergePatchType, body, metav1.PatchOptions{FieldManager: fieldManager}, "status"); err != nil {
			return n.lost(st, err)
		}
		st.statusFP, st.statusAt = fp, time.Now()
	}
	became := d.ready && !st.ready
	st.ready = d.ready
	if d.ready && (renew || became) {
		return n.renew(ctx, st, name)
	}
	return nil
}

// lost forgets a Node someone deleted, so the next pass registers it again.
func (n *nodes) lost(st *nodeState, err error) error {
	if apierrors.IsNotFound(err) {
		st.exists, st.lease, st.labelsFP, st.statusFP = false, nil, "", ""
		n.mu.Lock()
		n.registered--
		n.mu.Unlock()
	}
	return err
}

// renew creates or renews the Node's Lease in kube-node-lease, as a kubelet does every 10 s.
func (n *nodes) renew(ctx context.Context, st *nodeState, name string) error {
	leases := n.cs.CoordinationV1().Leases(corev1.NamespaceNodeLease)
	now := metav1.NewMicroTime(time.Now())
	holder, dur := name, int32(leaseSeconds)
	if st.lease == nil {
		l, err := leases.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			l = &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: corev1.NamespaceNodeLease,
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: name, UID: st.uid}}},
				Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &dur, RenewTime: &now},
			}
			if l, err = leases.Create(ctx, l, metav1.CreateOptions{FieldManager: fieldManager}); err != nil {
				return err
			}
			st.lease = l
			return nil
		}
		if err != nil {
			return err
		}
		st.lease = l
	}
	l := st.lease.DeepCopy()
	l.Spec.HolderIdentity, l.Spec.LeaseDurationSeconds, l.Spec.RenewTime = &holder, &dur, &now
	updated, err := leases.Update(ctx, l, metav1.UpdateOptions{FieldManager: fieldManager})
	if err != nil {
		st.lease = nil
		return err
	}
	st.lease = updated
	return nil
}

func (n *nodes) reportLine() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := fmt.Sprintf("kubernetes: %d of %d os: twin hosts registered as Nodes in %s (impersonated kubelet: Ready follows the guest's in-host agent, the boot ID is the guest's)",
		n.registered, len(n.a.guests), n.server)
	if n.errCount > 0 {
		s += fmt.Sprintf("; %d failed writes, last: %s", n.errCount, n.lastErr)
	}
	return s
}

func (n *nodes) isNot() []string {
	return []string{
		"No kubelet, container runtime, device plugin or CNI runs in the guest: a pod bound to its Node never starts, and nothing confirms a pod's termination.",
		"Ready is the in-host agent's liveness while the machine is on, not a kubelet's own checks; the Node is never cordoned or tainted by the twin.",
		"nvidia.com/gpu capacity is the model's accelerator count and allocatable drops for an accelerator the model holds Critical or offline, as a device plugin's would; no device plugin runs.",
	}
}

// nodeName is the Node a host registers as, the name the kwok projection and the metrics
// adapter use.
func nodeName(d *model.Device) string { return kwok.NodeName(d) }

var notLabel = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// labelValue makes a label value: "/" as ".", anything else not allowed as "-", at most 63.
func labelValue(s string) string {
	v := notLabel.ReplaceAllString(strings.ReplaceAll(s, "/", "."), "-")
	if len(v) > 63 {
		v = v[:63]
	}
	return strings.Trim(v, "-._")
}

func fingerprint(v ...any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:12])
}

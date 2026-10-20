// Package kwok is the Kubernetes view of `model` hosts (docs/spec/05-interfaces.md — Kubernetes
// view): each model host is registered as a Node in the stack's own cluster, advertising its
// profile's `kubernetes` resources and labels, with conditions, allocatable and a cordon that
// follow the model's health, and a node Lease renewed while the host would be running.
//
// This is IMPERSONATION (D24, 07 — Impersonation): the adapter stands in for a kubelet with a
// credential the stack's outputs hand it, and a run that enables it is a scale run, never a
// conformance pass. No pod runs on these nodes.
//
// It is called kwok because the spec names the projection after KWOK (sigs.k8s.io/kwok), whose
// role it plays; it does not run KWOK's controller. Nodes it registers carry
// dc-twin.io/impersonated=kwok and not KWOK's own kwok.x-k8s.io/node annotation, so a KWOK
// controller in the same cluster leaves them alone.
package kwok

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Name is the adapter's registered name and the impersonator name stacks list.
const Name = "kwok"

// OutputKubeconfig is the stack output that hands the twin its credential: a path to a
// kubeconfig file (or the kubeconfig itself).
const OutputKubeconfig = "kubeconfig"

const (
	fieldManager = "dc-twin-kwok"
	leaseNS      = corev1.NamespaceNodeLease
	// The kubelet's defaults: a 40 s lease renewed every 10 s, and a full status report at least
	// every 5 minutes when nothing changed (nodeLeaseDurationSeconds, nodeStatusReportFrequency).
	leaseSeconds    = 40
	reportFrequency = 5 * time.Minute
	resyncEvery     = 10 * time.Second
	debounce        = 100 * time.Millisecond
	workers         = 16
)

// renewEvery is the kubelet's lease renewal interval (a var so a test can shorten it).
var renewEvery = 10 * time.Second

func init() {
	twin.Register(Name, func() adapter.Adapter { return New() })
}

// Adapter is the kwok projection.
type Adapter struct {
	rt     *adapter.Runtime
	cs     kubernetes.Interface
	server string
	estate string

	hosts  []*host
	byPath map[string]*host

	mu         sync.Mutex
	dirty      map[*host]bool
	inactive   string // why nothing is registered, when nothing is
	lastErr    string
	errCount   int
	gcDone     bool
	registered int

	wake   chan struct{}
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// host is one model host and what the adapter last wrote for it. Fields after `d` are touched
// only by the worker syncing that host, which the dirty set serialises.
type host struct {
	d      *model.Device
	name   string
	static staticNode

	mu        sync.Mutex
	exists    bool
	uid       types.UID
	specFP    string
	statusFP  string
	statusAt  time.Time
	up        bool
	condSince map[corev1.NodeConditionType]metav1.Time
	condWas   map[corev1.NodeConditionType]corev1.ConditionStatus
	lease     *coordinationv1.Lease
}

// New returns an unstarted adapter.
func New() *Adapter { return &Adapter{dirty: map[*host]bool{}, wake: make(chan struct{}, 1)} }

func (a *Adapter) Name() string { return Name }

// Start connects to the cluster named by the stack's kubeconfig output. With no kubeconfig the
// adapter does nothing, and its report says so.
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt = rt
	kc := strings.TrimSpace(rt.Outputs[OutputKubeconfig])
	if kc == "" {
		a.inactive = "no kubeconfig among the stack's outputs (outputs." + OutputKubeconfig + ")"
		return nil
	}
	cfg, err := restConfig(kc)
	if err != nil {
		return fmt.Errorf("kwok: kubeconfig: %w", err)
	}
	cfg.UserAgent = "dc-twin-kwok"
	cfg.QPS, cfg.Burst = 100, 200
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("kwok: %w", err)
	}
	a.cs = cs
	if u, err := url.Parse(cfg.Host); err == nil {
		a.server = u.Host
	} else {
		a.server = cfg.Host
	}
	kubeletVersion := "v0.0.0-dc-twin"
	if v, err := cs.Discovery().ServerVersion(); err == nil {
		// A kubelet at the control plane's version: no version-skew rule can object.
		kubeletVersion = v.GitVersion
	}
	e := rt.Engine
	a.byPath = map[string]*host{}
	e.Read(func(m *model.Model) {
		a.estate = m.Name
		for _, d := range m.Devices {
			if !d.IsHost() || d.Fidelity != "model" {
				continue
			}
			h := &host{d: d, name: NodeName(d), condSince: map[corev1.NodeConditionType]metav1.Time{}, condWas: map[corev1.NodeConditionType]corev1.ConditionStatus{}}
			h.static = buildStatic(d, m.Name, m.Site, m.Seed, scaleUpDomain(m, d), kubeletVersion)
			a.hosts = append(a.hosts, h)
			a.byPath[d.Path] = h
		}
	})
	seen := map[string]string{}
	for _, h := range a.hosts {
		if p, dup := seen[h.name]; dup {
			return fmt.Errorf("kwok: %s and %s both map to node %s", p, h.d.Path, h.name)
		}
		seen[h.name] = h.d.Path
	}
	lctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.markAll()
	a.wg.Add(3)
	go a.watch(lctx)
	go a.loop(lctx)
	go a.heartbeat(lctx)
	rt.Log.Info("kwok: registering nodes", "server", a.server, "hosts", len(a.hosts))
	return nil
}

func restConfig(kc string) (*rest.Config, error) {
	if strings.Contains(kc, "apiVersion:") || strings.Contains(kc, "clusters:") {
		return clientcmd.RESTConfigFromKubeConfig([]byte(kc))
	}
	// An explicit path only: never the default loading rules, which would read $KUBECONFIG or
	// ~/.kube/config and could register nodes in a cluster nobody named.
	return clientcmd.BuildConfigFromFlags("", kc)
}

// Stop ends heartbeats and updates. Nodes stay registered, as a host's Node stays when its
// kubelet stops; their leases lapse.
func (a *Adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	return nil
}

// Report is the adapter's fidelity-report contribution.
func (a *Adapter) Report() adapter.Report {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := adapter.Report{Adapter: Name}
	if a.inactive != "" {
		r.Lines = []string{"kubernetes: kwok inactive: " + a.inactive + "; no Node registered"}
		return r
	}
	r.Served = a.registered
	r.Impersonated = []string{"kwok (kubelet)"}
	r.Lines = []string{fmt.Sprintf("kubernetes: %d of %d model hosts registered as Nodes in %s (kwok projection, impersonated kubelet; leases in %s)",
		a.registered, len(a.hosts), a.server, leaseNS)}
	if a.errCount > 0 {
		r.Lines = append(r.Lines, fmt.Sprintf("kubernetes: %d failed writes; last: %s", a.errCount, a.lastErr))
	}
	r.IsNot = []string{
		"No pods run: there is no kubelet, container runtime, device plugin or CNI behind these Nodes; a pod bound to one stays Pending.",
		"Ready follows the model (input power, power state, boot, and host health Critical from anything but an accelerator) with the model's reason; a real kubelet says KubeletNotReady or stops posting, and does not go NotReady for a failed fan. A failed accelerator leaves Ready True, as a kubelet does, and shows in its accelerator condition and allocatable.",
		"cpu, memory, ephemeral-storage and pods capacity are twin defaults where the profile is silent (cores from the CPU model, else 32 a socket; memory from DIMMs, else 8 GiB a core; one drive as the root filesystem; 110 pods).",
		"Allocatable accelerators drop as a device plugin's would when the model marks one Critical or offline; no device plugin or DRA driver runs, and no ResourceSlice is published.",
		"A lost or failed accelerator withholds that one device from allocatable; NVIDIA's device plugin v0.19.3 marks every GPU of the node unhealthy on ERROR_GPU_IS_LOST (Xid 79) and re-admits them only when it restarts (compat/gpu-operator).",
		"The twin cordons a node whose host health is Critical and uncordons it when health recovers, which pre-empts a stack's own cordon logic; a cordon already in place is never taken over or undone, but one made while the twin's holds is lifted with it.",
		"Taints a node-lifecycle controller derives from conditions are left to the cluster's controller: where none runs (envtest), the not-ready:NoSchedule taint the TaintNodesByCondition admission plugin puts on every new Node stays.",
		"The credential registers every node; a real kubelet's is limited to its own Node by NodeRestriction.",
	}
	return r
}

// ---------------------------------------------------------------- scheduling work

func (a *Adapter) markAll() {
	a.mu.Lock()
	for _, h := range a.hosts {
		a.dirty[h] = true
	}
	a.mu.Unlock()
	a.poke()
}

func (a *Adapter) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// markPath marks the host an event path belongs to: a device, one of its components or ports,
// or one of its sub-devices. Events on racks, feeds, links and domains reach hosts through the
// device events the engine commits with them, and the periodic resync covers the rest.
func (a *Adapter) markPath(p string) {
	if i := strings.IndexByte(p, ':'); i >= 0 {
		p = p[:i]
	}
	for p != "" {
		if h, ok := a.byPath[p]; ok {
			a.mu.Lock()
			a.dirty[h] = true
			a.mu.Unlock()
			a.poke()
			return
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			return
		}
		p = p[:i]
	}
}

// watch follows the engine's committed events. A subscriber that falls behind is dropped by the
// engine (its channel closes); the watcher resubscribes and resyncs everything.
func (a *Adapter) watch(ctx context.Context) {
	defer a.wg.Done()
	id, ch := a.rt.Engine.Subscribe(8192)
	for {
		select {
		case <-ctx.Done():
			a.rt.Engine.Unsubscribe(id)
			return
		case ev, ok := <-ch:
			if !ok {
				id, ch = a.rt.Engine.Subscribe(8192)
				a.markAll()
				continue
			}
			if ev.Kind == "request" {
				continue
			}
			a.markPath(ev.Path)
		}
	}
}

// loop writes dirty hosts, after a short debounce so one fault's cascade of events is one write.
func (a *Adapter) loop(ctx context.Context) {
	defer a.wg.Done()
	resync := time.NewTicker(resyncEvery)
	defer resync.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-resync.C:
			a.markAll()
		case <-a.wake:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(debounce):
		}
		a.mu.Lock()
		var batch []*host
		for h := range a.dirty {
			batch = append(batch, h)
		}
		a.dirty = map[*host]bool{}
		a.mu.Unlock()
		if !a.gcDone {
			a.gc(ctx)
		}
		a.parallel(ctx, batch, a.sync)
	}
}

func (a *Adapter) parallel(ctx context.Context, hs []*host, f func(context.Context, *host) error) {
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for _, h := range hs {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(h *host) {
			defer func() { <-sem; wg.Done() }()
			h.mu.Lock()
			err := f(ctx, h)
			h.mu.Unlock()
			if err != nil && ctx.Err() == nil {
				a.fail(h, err)
			}
		}(h)
	}
	wg.Wait()
}

func (a *Adapter) fail(h *host, err error) {
	a.mu.Lock()
	a.errCount++
	a.lastErr = h.name + ": " + err.Error()
	a.dirty[h] = true // retried on the next pass
	a.mu.Unlock()
	a.rt.Log.Warn("kwok: write failed", "node", h.name, "err", err)
}

// gc deletes nodes this estate registered earlier for hosts it no longer has.
func (a *Adapter) gc(ctx context.Context) {
	list, err := a.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: LabelEstate + "=" + labelValue(a.estate)})
	if err != nil {
		return
	}
	a.gcDone = true
	want := map[string]bool{}
	for _, h := range a.hosts {
		want[h.name] = true
	}
	for _, n := range list.Items {
		if n.Annotations[AnnImpersonated] == "kwok" && !want[n.Name] {
			_ = a.cs.CoreV1().Nodes().Delete(ctx, n.Name, metav1.DeleteOptions{})
		}
	}
}

// heartbeat renews the lease of every host whose kubelet would be running.
func (a *Adapter) heartbeat(ctx context.Context) {
	defer a.wg.Done()
	t := time.NewTicker(renewEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var live []*host
		for _, h := range a.hosts {
			h.mu.Lock()
			if h.exists && h.up {
				live = append(live, h)
			}
			h.mu.Unlock()
		}
		a.parallel(ctx, live, a.renew)
	}
}

// ---------------------------------------------------------------- writes

// sync makes one host's Node say what the model says. The caller holds h.mu.
func (a *Adapter) sync(ctx context.Context, h *host) error {
	o := observe(a.rt.Engine, h.d)
	des := desire(h.static, h.d.Path, o)
	nodes := a.cs.CoreV1().Nodes()
	now := metav1.Now()
	if !h.exists {
		n := a.newNode(h, des, now)
		created, err := nodes.Create(ctx, n, metav1.CreateOptions{FieldManager: fieldManager})
		switch {
		case err == nil:
			h.uid = created.UID
			h.specFP = specFingerprint(des)
		case apierrors.IsAlreadyExists(err):
			// Adopt the node a previous run registered: keep its condition transition times.
			cur, err := nodes.Get(ctx, h.name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			h.uid = cur.UID
			for _, c := range cur.Status.Conditions {
				h.condSince[c.Type], h.condWas[c.Type] = c.LastTransitionTime, c.Status
			}
		default:
			return err
		}
		h.exists = true
		a.mu.Lock()
		a.registered++
		a.mu.Unlock()
	}
	if fp := specFingerprint(des); fp != h.specFP {
		if err := a.updateSpec(ctx, h, des); err != nil {
			return a.lost(h, err)
		}
		h.specFP = fp
	}
	fp := des.fingerprint()
	// A running kubelet reports at least every reportFrequency; a stopped one never does, so the
	// last thing a host said before it stopped stays until it comes back (or a node-lifecycle
	// controller marks it Unknown, which the twin does not fight).
	if fp != h.statusFP || (des.up && time.Since(h.statusAt) >= reportFrequency) {
		if !h.up && h.exists {
			// The twin renewed no lease while the host was down, so the node-lifecycle
			// controller may have marked its conditions Unknown since the last post. A kubelet
			// computes each transition against the Node as it is now; without this, Ready going
			// from the controller's Unknown back to False (a host powering on) kept the time it
			// first became False, and a health checker read it as long overdue (compat/medik8s).
			if err := a.seedConditions(ctx, h); err != nil {
				return a.lost(h, err)
			}
		}
		if err := a.patchStatus(ctx, h, des, now); err != nil {
			return a.lost(h, err)
		}
		h.statusFP, h.statusAt = fp, time.Now()
	}
	wasUp := h.up
	h.up = des.up
	if des.up && !wasUp {
		return a.renew(ctx, h)
	}
	return nil
}

// lost forgets a node someone deleted, so the next pass registers it again, as a kubelet
// re-registers its Node.
func (a *Adapter) lost(h *host, err error) error {
	if apierrors.IsNotFound(err) {
		h.exists, h.lease, h.specFP, h.statusFP = false, nil, "", ""
		a.mu.Lock()
		a.registered--
		a.mu.Unlock()
	}
	return err
}

func specFingerprint(des desired) string {
	if des.cordon {
		return "cordon:" + des.cordonWhy
	}
	return "schedulable"
}

// seedConditions takes each condition's status and transition time from the Node as the API
// server has it, as the base the next status write is compared with. The caller holds h.mu.
func (a *Adapter) seedConditions(ctx context.Context, h *host) error {
	n, err := a.cs.CoreV1().Nodes().Get(ctx, h.name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for _, c := range n.Status.Conditions {
		h.condSince[c.Type], h.condWas[c.Type] = c.LastTransitionTime, c.Status
	}
	return nil
}

func (a *Adapter) conditions(h *host, des desired, now metav1.Time) []corev1.NodeCondition {
	out := make([]corev1.NodeCondition, 0, len(des.conditions))
	for _, c := range des.conditions {
		since, ok := h.condSince[c.Type]
		if !ok || h.condWas[c.Type] != c.Status {
			since = now
		}
		h.condSince[c.Type], h.condWas[c.Type] = since, c.Status
		c.LastHeartbeatTime, c.LastTransitionTime = now, since
		out = append(out, c)
	}
	return out
}

func (a *Adapter) newNode(h *host, des desired, now metav1.Time) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: h.name, Labels: map[string]string{}, Annotations: map[string]string{}},
		Spec:       corev1.NodeSpec{ProviderID: h.static.providerID},
		Status: corev1.NodeStatus{
			Capacity:    h.static.capacity,
			Allocatable: des.allocatable,
			Conditions:  a.conditions(h, des, now),
			NodeInfo:    h.static.nodeInfo,
			Addresses:   []corev1.NodeAddress{{Type: corev1.NodeHostName, Address: h.name}},
		},
	}
	n.Status.NodeInfo.BootID = des.bootID
	for k, v := range h.static.labels {
		n.Labels[k] = v
	}
	for k, v := range h.static.annotations {
		n.Annotations[k] = v
	}
	if des.cordon {
		applyCordon(n, des)
	}
	return n
}

// applyCordon cordons a node for the twin. A node someone else already cordoned is left as it is
// and not marked as the twin's, so the twin never lifts a cordon it did not make.
func applyCordon(n *corev1.Node, des desired) {
	if _, ours := n.Annotations[AnnCordoned]; n.Spec.Unschedulable && !ours {
		return
	}
	n.Spec.Unschedulable = true
	n.Annotations[AnnCordoned] = trim(des.cordonWhy, 1024)
	for _, t := range n.Spec.Taints {
		if t.Key == TaintUnschedulable {
			return
		}
	}
	n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: TaintUnschedulable, Effect: corev1.TaintEffectNoSchedule})
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// updateSpec brings labels, annotations and the twin's cordon up to date. A cordon the twin did
// not make (no dc-twin.io/cordoned annotation) is never lifted.
func (a *Adapter) updateSpec(ctx context.Context, h *host, des desired) error {
	nodes := a.cs.CoreV1().Nodes()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		n, err := nodes.Get(ctx, h.name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		for k, v := range h.static.labels {
			n.Labels[k] = v
		}
		for k, v := range h.static.annotations {
			n.Annotations[k] = v
		}
		if des.cordon {
			applyCordon(n, des)
		} else if _, ours := n.Annotations[AnnCordoned]; ours {
			delete(n.Annotations, AnnCordoned)
			n.Spec.Unschedulable = false
			var keep []corev1.Taint
			for _, t := range n.Spec.Taints {
				if t.Key != TaintUnschedulable {
					keep = append(keep, t)
				}
			}
			n.Spec.Taints = keep
		}
		_, err = nodes.Update(ctx, n, metav1.UpdateOptions{FieldManager: fieldManager})
		return err
	})
}

// patchStatus posts the node's status as a kubelet does: a strategic-merge patch of the status
// subresource, so conditions other components own (by type) are left alone.
func (a *Adapter) patchStatus(ctx context.Context, h *host, des desired, now metav1.Time) error {
	info := h.static.nodeInfo
	info.BootID = des.bootID
	body := map[string]any{"status": map[string]any{
		"capacity":    h.static.capacity,
		"allocatable": des.allocatable,
		"conditions":  a.conditions(h, des, now),
		"nodeInfo":    info,
		"addresses":   []corev1.NodeAddress{{Type: corev1.NodeHostName, Address: h.name}},
	}}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = a.cs.CoreV1().Nodes().Patch(ctx, h.name, types.StrategicMergePatchType, data, metav1.PatchOptions{FieldManager: fieldManager}, "status")
	return err
}

// renew creates or renews the host's node Lease in kube-node-lease. The caller holds h.mu.
func (a *Adapter) renew(ctx context.Context, h *host) error {
	leases := a.cs.CoordinationV1().Leases(leaseNS)
	now := metav1.NewMicroTime(time.Now())
	if h.lease == nil {
		l, err := leases.Get(ctx, h.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			holder, dur := h.name, int32(leaseSeconds)
			l = &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: h.name, Namespace: leaseNS,
					OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: h.name, UID: h.uid}}},
				Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &dur, RenewTime: &now},
			}
			l, err = leases.Create(ctx, l, metav1.CreateOptions{FieldManager: fieldManager})
			if apierrors.IsNotFound(err) {
				// kube-node-lease is a system namespace the API server creates; make it if not.
				_, _ = a.cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: leaseNS}}, metav1.CreateOptions{})
				return err
			}
			if err != nil {
				return err
			}
			h.lease = l
			return nil
		}
		if err != nil {
			return err
		}
		h.lease = l
	}
	l := h.lease.DeepCopy()
	holder, dur := h.name, int32(leaseSeconds)
	l.Spec.HolderIdentity, l.Spec.LeaseDurationSeconds, l.Spec.RenewTime = &holder, &dur, &now
	updated, err := leases.Update(ctx, l, metav1.UpdateOptions{FieldManager: fieldManager})
	if err != nil {
		h.lease = nil // re-read on the next renewal
		return err
	}
	h.lease = updated
	return nil
}

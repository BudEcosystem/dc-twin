# KAI Scheduler against the twin

NVIDIA's [KAI Scheduler](https://github.com/kai-scheduler/KAI-Scheduler) (formerly
`NVIDIA/KAI-Scheduler`), built from the released tag `v0.18.6` and run **unmodified**, schedules
GPU, AMD and Gaudi pods onto the Kubernetes Nodes dc-twin projects for the lab estate's model
hosts, places an NVL72 gang inside one NVLink domain, and steers new work away from trays and
servers the twin faults or powers off.

> **Impersonated.** The Nodes are the twin's `kwok` projection ([D24](../../docs/spec/11-decisions.md)):
> no kubelet, container runtime, device plugin or CNI runs behind them, so a pod KAI binds stays
> `Pending` forever. What this shows is **placement and control flow**: what a scheduler decides
> from Node objects whose capacity, labels, conditions and cordons follow the twin's model. It says
> nothing about a workload running.

## What runs

| Process | Source | Role |
|---|---|---|
| `etcd`, `kube-apiserver` 1.37.0 | envtest's binaries (`setup-envtest`) | the cluster |
| `kube-controller-manager` 1.37.0 | `kubernetes/kubernetes` at `v1.37.0`, built from source (`KCM=build`, the default) | node lifecycle (taints from conditions, NoExecute eviction), Jobs, garbage collection |
| `dctwin up examples/estates/lab-model.yaml --adapters redfish,kwok,metrics` | this repository | 50 Nodes, their leases, conditions, allocatable and cordons |
| KAI `scheduler`, `binder`, `podgrouper`, `queuecontroller` (and `podgroupcontroller` with `KAI_PODGROUPCONTROLLER=1`) | `kai-scheduler/KAI-Scheduler` at `v0.18.6`, `go build ./cmd/<name>` | the minimal set that schedules a pod: PodGroups from pods, the scheduling cycle, BindRequests to bindings, queue status. `podgroupcontroller` (PodGroup status only) is opt-in: v0.18.6 ignores its `--metrics-bind-address` and binds `:8080` on every interface (found by `compat/grove`), so the harness starts it only when asked and only while nothing listens on 8080 |

KAI's CRDs come from its chart (`deployments/kai-scheduler/crds/`), its priority classes from the
chart's templates (`manifests/03-priorityclasses.yaml`). The operator, admission webhooks,
resource-reservation pods (fractional GPUs) and node-scale adjuster are not run: none is needed to
place whole-device pods. Each component gets its own loopback ports and the scratch kubeconfig;
nothing reads `~/.kube/config`.

`manifests/` holds the queues (`dc-twin` → `training`, `inference`), the `nvlink` Topology
(zone → `nvidia.com/gpu.clique` → hostname), and the pod and PodGroup templates `run.sh` renders.
Gangs are external PodGroups (`pod-group-name` plus `kai.scheduler/skip-podgrouper`, KAI's
documented contract) with `topologyConstraint.requiredTopologyLevel: nvidia.com/gpu.clique`;
single pods get their PodGroup from KAI's pod grouper.

## Running it

```bash
make build
DCTWIN=$PWD/bin/dctwin compat/kai/run.sh                 # PASS/FAIL table; artefacts under $WORK
WORK=/tmp/kai DCTWIN=$PWD/bin/dctwin compat/kai/run.sh   # reuse builds (the KCM build takes ~10 min once)
KCM=none DCTWIN=$PWD/bin/dctwin compat/kai/run.sh        # without kube-controller-manager
KEEP=1 ...                                               # leave everything running to poke at
```

Needs Go, git, python3 and envtest's binaries (`KUBEBUILDER_ASSETS`, or `setup-envtest`'s
default directory). `$WORK/evidence` keeps the rendered workloads, `kubectl` output at each step,
the PodGroup that could not be placed, and the twin's fidelity report.

### Modes: with and without kube-controller-manager

envtest has no controller manager. Without one, the `node.kubernetes.io/not-ready:NoSchedule` taint
that the `TaintNodesByCondition` admission plugin puts on every new Node is never lifted, and KAI
leaves every pod without a toleration unschedulable (`no nodes with enough resources were found:
44 node(s) had untolerated taint(s)…`). The default run
builds the real `kube-controller-manager` so the taint is lifted (and re-added on a fault, with the
`NoExecute` eviction that follows) by the same code a cluster runs. `KCM=none` instead removes the
creation taint from Ready Nodes itself and says so (row U3 is `CANNOT`). KAI also checks the
`Ready` condition directly (`scheduler_util.CheckNodeConditionPredicate`), so powered-off Nodes are
avoided in both modes.

## Result (2026-10-18, KAI v0.18.6 `279f61c`, kube-controller-manager v1.37.0)

| # | Check | Observed | Result |
|---|---|---|---|
| U1 | Nodes the twin registers | 50 Ready, 50 annotated `dc-twin.io/impersonated=kwok` | PASS |
| U2 | Accelerator allocatable | `nvidia.com/gpu` 192, `amd.com/gpu` 16, `habana.ai/gaudi` 16 | PASS |
| U3 | Creation `not-ready` taint lifted by the node-lifecycle controller | 0 Nodes keep it after 4.6 s | PASS |
| U4 | KAI components running | 5 of 5 | PASS |
| A1 | Gang 18 × 4 `nvidia.com/gpu`, required in one `nvidia.com/gpu.clique`, GB300 | 18 of 18 bound in 1.0 s, on 18 trays of rack a01, 1 clique | PASS |
| A2–A6 | Single pods by product: GB200 ×1, H100 ×8, A100 ×8, `amd.com/gpu` ×4, `habana.ai/gaudi` ×4 | each on a Node of its product (a02 tray, b01, b05, b03, b04) in 0.1–0.6 s | PASS |
| A7 | `amd.com/gpu: 2` with **no** node selector | bound to `a02-compute-tray-01`, an NVIDIA GB200 tray with no `amd.com/gpu` | **FINDING** |
| A8 | Queue accounting | `training` {nvidia.com/gpu: 72}; `inference` {amd.com/gpu: 4, habana.ai/gaudi: 4, nvidia.com/gpu: 17} | PASS |
| B1 | `dctwin fault inject a01/compute-tray-07/gpu-2 nvidia/xid/xid-79` | Ready kept (KubeletReady), GpuHealthy=False, cordoned, allocatable 3, in 0.6 s; no `not-ready` taint, so nothing on the healthy GPUs is evicted (D43) | PASS |
| B2 | `dctwin power off a02/compute-tray-03` | Ready=False (PoweredOff), `not-ready:NoSchedule` added by the controller, 0.2 s | PASS |
| B3 | `dctwin power off b01/server-02` | Ready=False (PoweredOff), 0.2 s | PASS |
| B4 | AMD RAS uncorrectable ECC on `b03/server-01/gpu-3` | Ready kept, GpuHealthy=False, cordoned, allocatable `amd.com/gpu` 7 | PASS |
| B5 | Gaudi HBM uncorrectable on `b04/server-01/hpu-5` | Ready kept, HpuHealthy=False, cordoned, allocatable `habana.ai/gaudi` 7 | PASS |
| C1 | Gang 18 × 4, one clique, after the faults | 0 bound; PodGroup: `node-group hall-a.<clique> can allocate only 17 of 18 required pods` for both domains | PASS |
| C2 | Gang 17 × 4, one clique, no product selector | 17 bound in 1.0 s, all in a01's clique, none on the Xid tray or the powered-off tray | PASS |
| C3 | 3 × 8 H100 GPUs with b01-server-02 off | 2 bound (b02-server-01, b02-server-02), none on b01-server-02, 1 Pending | PASS |
| C4, C5 | New AMD and Gaudi pods after the RAS and HBM errors | on b03-server-02 and b04-server-02 | PASS |
| D1 | `dctwin power on b01/server-02` | the Pending H100 pod bound to b01-server-02 3.4 s later | PASS |
| D2 | `dctwin fault clear` (Xid 79) | Ready, uncordoned, allocatable 4 in 0.7 s | PASS |

24 checks: 23 PASS, 1 FINDING with `KAI_PODGROUPCONTROLLER=1`. By default (no podgroupcontroller, 2026-10-20 on the lab): 22 PASS, 1 FINDING, and A8 `CANNOT` (queue accounting sums PodGroup status, which only the podgroupcontroller writes). `KCM=none`: 22 PASS, 1 FINDING, and U3 `CANNOT` (taint removed by the
harness). Times are from one run on a laptop and vary from run to run.

### Finding: KAI folds `amd.com/gpu` into NVIDIA GPUs

KAI's resource model counts `nvidia.com/gpu` and `amd.com/gpu` as one GPU dimension, on pods and
on Nodes alike (`pkg/scheduler/api/resource_info/resource_requirment.go` and `resource_info.go`:
`case GPUResourceName, amdGpuResourceName`). A pod that requests `amd.com/gpu` and has no node
selector can therefore be bound to an NVIDIA Node that advertises no `amd.com/gpu` at all (row A7),
and the reverse holds for `nvidia.com/gpu` pods on AMD Nodes. A real kubelet would reject such a
pod at admission (`OutOfamd.com/gpu`); the twin has none to do so, which is why the harness reports
the binding instead. In a mixed estate, pin AMD pods with a node selector (A5 does:
`amd.com/gpu.product-name`). `habana.ai/gaudi` is handled as an ordinary extended resource and only
fits Gaudi Nodes.

### What the gang placement looks like

```text
$ kubectl get podgroup -n dc-twin-kai nvl72-b -o yaml     # C1, after the faults
  schedulingConditions:
  - message: "Unable to schedule podgroup: node-group hall-a.162c0be9-….1 can allocate only 17 of 18
      required pods. node-group hall-a.6b678361-….1 can allocate only 17 of 18 required pods.
      topology nvlink, requirement nvidia.com/gpu.clique couldn't be satisfied for job <dc-twin-kai/nvl72-b>."
    reason: Unschedulable
$ kubectl describe node a01-compute-tray-07                # B1
  Labels: nvidia.com/gpu.product=NVIDIA-GB300 nvidia.com/gpu.present=true nvidia.com/gpu.family=blackwell nvidia.com/gpu.memory=294912 …
  Ready       True   KubeletReady         kubelet is posting ready status
  GpuHealthy  False  AcceleratorCritical  gpu-2 Critical, UnavailableOffline: GPU has fallen off the bus (Xid 79) [nvidia/xid/xid-79]
  Allocatable: nvidia.com/gpu: 3
  Taints: node.kubernetes.io/unschedulable:NoSchedule
```

## What this does not show

- That anything runs: bound pods stay `Pending`, images are never pulled, and pod deletion needs
  `--force` because no kubelet confirms it.
- Device-plugin, DRA or GPU-sharing behaviour: no device plugin runs; allocatable is written by
  the twin, and fractional GPUs (KAI's reservation pods) cannot work without a kubelet.
- That cordoning is right. The twin cordons a host whose health is Critical, which on a real
  cluster is NVSentinel's or the operator's decision; KAI's avoidance of the faulted trays (C1, C2)
  is avoidance of a cordoned Node. The first run of this harness also found the projection setting
  `Ready=False` for one failed GPU, which made the real controller manager add
  `not-ready:NoExecute` and evict a tolerating pod (`TaintManagerEviction`); since D43 the Node
  stays Ready as a kubelet's does, and B1 checks no `not-ready` taint is added.
- KAI's admission webhooks, operator, preemption across queues and time-based fair share were not
  exercised.

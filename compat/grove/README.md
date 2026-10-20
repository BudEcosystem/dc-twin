# Grove and the Dynamo operator against the twin

NVIDIA's [Grove](https://github.com/ai-dynamo/grove) (the Kubernetes API for gang-scheduled,
topology-aware multi-pod inference; `NVIDIA/grove` redirects there), built from the released tag
`v0.1.0-alpha.16`, and the [NVIDIA Dynamo](https://github.com/ai-dynamo/dynamo) Kubernetes operator,
built from `v1.5.1` (`deploy/operator`), run **unmodified** with NVIDIA KAI Scheduler `v0.18.6` as the
scheduler, over the Kubernetes Nodes dc-twin projects for two GB300 NVL72 racks. A disaggregated
inference PodCliqueSet is packed into one NVLink domain; the twin then faults and powers off trays
under it and the placement reacts. A DynamoGraphDeployment is rendered by the Dynamo operator into
Grove's objects and placed the same way. This is the "scheduling and serving" layer of
[NVIDIA DSX OS](../../docs/stacks/nvidia-dsx-os.md#kai-scheduler-runai-dynamo-grove-cloud-functions).

> **Impersonated.** The Nodes are the twin's `kwok` projection ([D24](../../docs/spec/11-decisions.md)):
> no kubelet, container runtime, device plugin or CNI runs behind them, so a bound pod stays
> `Pending`, no model loads, no PodCliqueSet becomes Available and no DynamoGraphDeployment Ready.
> What this shows is **placement and control flow** ([06](../../docs/spec/06-packs-and-stacks.md)):
> what Grove, Dynamo and KAI decide from Node objects whose capacity, NVLink-clique labels,
> conditions and cordons follow the twin's model.

## What runs

| Process | Source | Role |
|---|---|---|
| `etcd`, `kube-apiserver` 1.37.0 | envtest's binaries | the cluster |
| `kube-controller-manager` 1.37.0 | `kubernetes/kubernetes` at `v1.37.0` (`KCM=build`, or a path) | node lifecycle (taints from conditions, `NoExecute` eviction), pod GC (non-graceful node shutdown) |
| `dctwin up compat/grove/estate.yaml --adapters kwok` | this repository | 36 GB300 trays as Nodes in two NVLink cliques |
| KAI `scheduler`, `binder`, `podgrouper`, `queuecontroller` | `kai-scheduler/KAI-Scheduler` at `v0.18.6` (as `compat/kai`; `KAI_BIN` reuses its build) | placement of the PodGroups Grove creates |
| `grove-operator` | `ai-dynamo/grove` at `v0.1.0-alpha.16`, `go build ./cmd` in `operator/` | PodCliqueSet → PodCliqueScalingGroups, PodCliques, PodGangs, pods; its KAI backend turns each PodGang into a KAI PodGroup and the ClusterTopologyBinding into a KAI Topology |
| Dynamo operator, container `dctwin-dynamo-operator` | `ai-dynamo/dynamo` at `v1.5.1`, `CGO_ENABLED=0 go build ./cmd` in `deploy/operator`, in a `FROM scratch` image built here | DynamoGraphDeployment → PodCliqueSet (Grove enabled, KAI enabled, both detected from the API) |

Grove runs with its chart's operator configuration (`helm template` of `operator/charts`): KAI as the
default scheduler profile, topology-aware scheduling on, manual certificates from a scratch CA, and
its chart's five webhook configurations pointed at the process by URL (`lib/webhooks.py`). The Dynamo
operator runs with its chart's configuration (`components/operator`, Grove and KAI enabled), its
chart's RBAC and webhook configurations, and its CRDs as shipped.

**Why a container for Dynamo.** The operator reads its namespace from the in-cluster service-account
file (`internal/cert/cert.go`, `/var/run/secrets/kubernetes.io/serviceaccount/namespace`) and exits
without it; the image holds the static binary and that one file. With rootless Podman
(`DYNAMO_NETWORK=host`, chosen when `docker` is Podman) it shares this machine's network namespace,
reaches the scratch apiserver on `127.0.0.1` and serves its webhooks on loopback ports; no port is
published and no firewall rule added. With a Docker VM (`DYNAMO_NETWORK=publish`: Colima, Docker
Desktop) it reaches this machine's loopback through the VM's host address (`DYNAMO_APISERVER_HOST`,
default `192.168.5.2`, Lima's `host.lima.internal`; Docker Desktop's is `host.docker.internal`) and
its webhook port is published on `127.0.0.1`. It is limited to 1 CPU and 512 MiB where the runtime
can, and removed with its image at the end (`KEEP_IMAGE=1` keeps the image).

**Conversion webhooks.** Dynamo's CRDs serve `v1alpha1` and `v1beta1` through a conversion webhook
that names a Service, and the operator refuses to start unless it does (it injects its CA there).
No Service can carry the apiserver to a port on this machine's loopback (Endpoints refuse
`127.0.0.0/8`), so once the operator has injected its CA the harness points the four conversion
webhooks at it by URL with that CA. Without that, the operator's `v1alpha1` informers never sync
(listing them needs a conversion) and it reconciles nothing.

`manifests/` holds the `serving` KAI queue, the `nvlink` ClusterTopologyBinding (zone →
`nvidia.com/gpu.clique` → hostname), and the workload templates `run.sh` renders: the disaggregated
PodCliqueSet (frontend; prefill and decode instances of a leader and three workers, 4 GPUs each), a
filler pod, and the DynamoGraphDeployment (Dynamo's `disagg-multinode` shape, two-node prefill and
decode workers on GB300 trays). `estate.yaml` is two GB300 NVL72 racks and a network rack.

## Running it

```bash
make build
DCTWIN=$PWD/bin/dctwin compat/grove/run.sh                  # PASS/FAIL table; artefacts under $WORK
KAI_BIN=<compat/kai's $WORK/bin> KCM=<…/kube-controller-manager> DCTWIN=$PWD/bin/dctwin compat/grove/run.sh
DYNAMO=none DCTWIN=$PWD/bin/dctwin compat/grove/run.sh      # Grove only; no Docker needed
KEEP=1 ...                                                  # leave everything running
```

Needs Go, git, helm, python3, openssl and envtest's binaries; Docker or Podman for the Dynamo operator
(`DYNAMO_NETWORK=host|publish` overrides the choice above).
`$WORK/evidence` keeps the rendered workloads, pods and PodGroups at each step, Grove's objects, the
DynamoGraphDeployment and the PodCliqueSet it produced, and the twin's fidelity report.

## Result (2026-10-20, Grove `e33b835`, Dynamo `2093662`, KAI `279f61c`, kube-controller-manager v1.37.0; x86_64 Linux, rootless Podman)

| # | Check | Observed | Result |
|---|---|---|---|
| U1–U3 | Nodes, GPUs, creation taint | 36 Ready GB300 trays, all impersonated; `nvidia.com/gpu` 144 in 2 cliques; `not-ready` taint lifted by the controller | PASS |
| U4 | KAI and Grove running | 5 of 5 processes | PASS |
| U5 | ClusterTopologyBinding `nvlink` → KAI Topology | Grove created KAI Topology `nvlink` with levels zone, `nvidia.com/gpu.clique`, hostname; `inSync` | PASS |
| U6 | KAI podgroupcontroller's `--metrics-bind-address` | not started: v0.18.6 parses the flag but never gives it to its manager, which binds `:8080` on all interfaces | **FINDING** |
| G0 | One PodCliqueSet replica, `pack.required: nvlink-domain`, scaling groups at `minAvailable: 1`, with a01 holding 12 free trays and a02 18 | 17 of 17 bound as **3** KAI PodGroups: the base PodGang (frontend, prefill-0, decode-0) and decode-1 in a01, prefill-1 in **a02** — one replica across two NVLink domains | **FINDING** |
| A1 | PodCliqueSet `disagg` (frontend + 2 prefill + 2 decode instances, 17 pods, 16 of them 4-GPU), scaling groups at `minAvailable` = replicas | 17 of 17 bound in 6.5 s, the GPU pods on 16 GB300 trays, 1 clique (a01) | PASS |
| A2 | PodGang → KAI PodGroup | one PodGroup: queue `serving`, minMember 17, 9 subgroups (one per PodClique), required `nvidia.com/gpu.clique` of topology `nvlink` | PASS |
| A3 | Scale to 2 replicas | replica 1: 17 of 17 in the other clique (a02) in 1.2 s | PASS |
| A4 | Scale to 3 replicas (2 free trays per domain) | 0 of 17 bound; PodGroup: `topology nvlink, requirement nvidia.com/gpu.clique couldn't be satisfied … not enough resources in hall-a` | PASS |
| B1 | `dctwin fault inject …/gpu-1 nvidia/xid/xid-79` on the tray under a decode worker | Ready kept, GpuHealthy=False, cordoned, allocatable 3 in 0.4 s; the pod stays bound (nothing evicts it, D43) | PASS |
| B2 | Drain that pod (what NVSentinel or an operator does) | Grove recreates it; KAI binds the replacement on a spare tray of **the same clique** (a01-compute-tray-17) in 0.2 s | PASS |
| B3 | `dctwin power off` the tray under a prefill worker | `Ready=Unknown/PoweredOff`, the controller's `unreachable` NoSchedule and NoExecute taints in 1.6 s; kube-controller-manager evicts the pod 11 s after power-off (its 10 s toleration); the replacement is bound on the last spare tray of the clique | PASS |
| B4 | A third a01 tray faulted, its decode worker drained, while a02 has 2 free trays | the replacement stays Pending (`no nodes with enough resources … 17 node(s) didn't have enough resources: GPUs`): KAI keeps a running gang's pods in its clique | PASS |
| D1 | `dctwin fault clear` on that tray | the Pending pod binds to it in 0.7 s | PASS |
| D2, D3 | Power the tray on; clear the first Xid | Ready, taints lifted in 3.6 s; uncordoned, allocatable 4 in 0.2 s | PASS |
| D4 | PodCliqueSet Available | 0: no kubelet makes a pod Ready; PodGang `Ready=False` | CANNOT |
| E1 | Dynamo operator v1.5.1 in its container | running; feature gates `grove: true, kaiScheduler: true` from API discovery; DynamoGraphDeployment controller started | PASS |
| E2 | DynamoGraphDeployment `vllm-disagg` (Frontend, 2-node prefill, 2-node decode, `topologyConstraint` nvlink/nvlink-domain) | a PodCliqueSet with `pack.required: nvlink-domain` on topology `nvlink`; PodCliqueScalingGroups `prefill`, `decode`; PodCliques `frontend`, `prefill-ldr`, `prefill-wkr`, `decode-ldr`, `decode-wkr`; `TopologyLevelsAvailable=True` | PASS |
| E3 | Its pods placed by KAI | 5 of 5 bound in 0.3 s, the 4 GPU pods on 4 GB300 trays in 1 clique; PodGroup queue `serving` (from `nvidia.com/kai-scheduler-queue`), required `nvidia.com/gpu.clique` | PASS |
| E4 | `dctwin power off` the tray under Dynamo's decode worker (grace period 60 s) | evicted after its 10 s toleration, and then `Terminating` indefinitely: no kubelet confirms the kill, as behind a dead node | PASS |
| E5 | Taint that Node `node.kubernetes.io/out-of-service=nodeshutdown:NoExecute` (Kubernetes' non-graceful node shutdown) | pod GC force-deletes it; Grove recreates it; KAI binds it in the same clique on another tray, 0.9 s later | PASS |
| E6 | DynamoGraphDeployment Ready | `state: pending`: `Frontend: scheduled but ready=0/1; prefill: desired=1, updated=0; …` | CANNOT |

25 checks: 21 PASS, 0 FAIL, 2 FINDING, 2 CANNOT, on an x86_64 Linux host with rootless Podman; the
same verdicts on an arm64 Mac with Colima (where a powered-off tray then read `Ready=False`, so the
taints were `not-ready`). Times vary from run to run.

### Finding: a PodCliqueSet replica's scaled PodGangs are packed on their own (G0)

With a scaling group's `minAvailable` below its `replicas`, Grove puts the instances above
`minAvailable` in PodGangs of their own (“scaled” PodGangs), and its KAI backend creates a separate KAI
PodGroup for each, every one carrying the replica's `required: nvidia.com/gpu.clique`. KAI satisfies
each independently, so one PodCliqueSet replica can span two NVLink domains although its template
says `pack.required: nvlink-domain` (G0: base gang and decode-1 in a01, prefill-1 in a02). Grove
documents this as a limitation of topology-aware scheduling (`docs/proposals/244-topology-aware-scheduling`,
“Scale-Out of PodCliqueScalingGroup … there is no guarantee that these … topology domains should be
within the same … PodCliqueSet replica”); GREP-525's design of one PodGroup per replica with
subgroups for the scaled gangs is not what alpha.16 does. The twin makes it visible because it has a
second domain with room: keep `minAvailable` equal to `replicas` (A1) when every instance must share
the domain.

### Finding: KAI's podgroupcontroller takes `:8080` (U6)

`cmd/podgroupcontroller` at v0.18.6 accepts `--metrics-bind-address` but builds its manager without
it (`app/app.go`), so controller-runtime's default metrics address `:8080` on all interfaces is used
whatever is passed. The harness does not start it (nothing here reads the PodGroup status it writes)
unless the source uses the flag. `compat/kai/run.sh` starts it with that flag, so it binds `:8080`
on this machine (or exits if the port is taken).

### What the placement looks like

```text
$ kubectl get pods -n dc-twin-serving -o wide          # G0: one replica, two domains
split-0-decode-0-dworker-…   a01-compute-tray-09     split-0-decode-1-dworker-…   a02-compute-tray-04
split-0-prefill-0-pworker-…  a01-compute-tray-13     split-0-prefill-1-pworker-…  a01-compute-tray-18
$ kubectl get podgroup -n dc-twin-serving <disagg-2> -o yaml     # A4
  message: "PodSchedulingErrors: topology nvlink, requirement nvidia.com/gpu.clique couldn't be satisfied
    for job <dc-twin-serving/disagg-2-…>: not enough resources in hall-a to allocate the job."
$ kubectl get pcs -n dc-twin-serving vllm-disagg -o yaml          # E2, written by the Dynamo operator
  template: { topologyConstraint: { topologyName: nvlink, pack: { required: nvlink-domain } }, … }
```

## What this does not show

- That anything runs: no image is pulled, no model loads, no NIXL transfer or NVLink traffic exists,
  and Grove's startup ordering (its init container) never executes.
- Dynamo's runtime (frontend, router, planner, KV-aware routing), its DynamoGraphDeploymentRequest
  profiling flow, and autoscaling: they need running pods.
- That the faults are detected: the twin cordons a host whose health is Critical; on a cluster that is
  NVSentinel's or an operator's decision, and so is the drain the harness does in B2 and B4 and the
  out-of-service taint in E5.
- KAI's admission webhooks, operator and podgroupcontroller; Grove's authorizer webhook and
  auto-MNNVL (ComputeDomains need NVIDIA's DRA driver).

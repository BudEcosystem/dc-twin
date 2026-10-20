# Run:ai against the twin

[NVIDIA Run:ai](https://github.com/dsx-ai-factory/run-ai) is open source (Apache-2.0) in the DSX AI
Factory organisation, released as one monorepo (control plane, cluster services, installer). This
harness runs the part of its cluster side that decides placement, **as release `3.0.20` pins it**,
unmodified and built from source, over the Kubernetes Nodes dc-twin projects for the lab estate:

- [KAI Resource Management](https://github.com/kai-scheduler/kai-resource-management) (KRM) `v0.18.3`:
  `nodepool-controller`, `project-controller` and `pod-group-assigner`, with their admission webhooks.
  Run:ai 3.0 installs KRM as a chart dependency of `runai-chart` in place of the node-pool, project
  and pod-group-assigner controllers it used to build itself, and pins it in
  `runai-cluster/kai-scheduler/kai-version.yaml` (`krm: chart-version: v0.18.3`; see
  `docs/developer/architecture.md`, “External Dependencies”). `run.sh` reads that file from the tag.
- [KAI Scheduler](https://github.com/kai-scheduler/KAI-Scheduler) as the scheduler: **one
  `kai-scheduler` per node pool** (Run:ai's “each node pool runs its own Scheduler instance”), plus
  the cluster-wide binder, pod grouper and queue controller.
- Run:ai's own workload controller, `anyworkload-controller` (`runai-cluster/cluster/anyworkload-controller`,
  the `dromos` library with Run:ai's plugins), built from the `3.0.20` tag: it reconciles
  `WorkloadRunner`s, the cluster-side form of a Run:ai workload, into the Kubernetes objects they
  wrap.

Node pools are cut by GPU product, a department and two projects carry per-pool quota, and a
training Job and inference Deployments are placed by product, NVLink clique and quota; the twin then
faults and powers off hosts and the placement reacts.

[`controlplane.sh`](#runais-control-plane-controlplanesh) adds **Run:ai's control plane**: its
published `3.0.20` chart and images in a KVM guest, with the cluster side above connected to it
through Run:ai's own agent and cluster-sync, so that the node pool, the project with its quota and
the workload are made through Run:ai's API and still placed on the twin's Nodes by KAI.

> **Impersonated.** The Nodes are the twin's `kwok` projection ([D24](../../docs/spec/11-decisions.md)):
> no kubelet, so bound pods stay `Pending` and nothing trains or serves. What this shows is
> **placement, quota and control flow**. In addition, the harness stands in for the KAI operator
> (below), which is labelled in row N2.

## What runs

| Process | Source | Role |
|---|---|---|
| `etcd`, `kube-apiserver` 1.37.0 | envtest's binaries | the cluster |
| `kube-controller-manager` 1.37.0 | `kubernetes/kubernetes` at `v1.37.0` | node lifecycle; the Job, Deployment and ReplicaSet controllers that create the workloads' pods |
| `dctwin up examples/estates/lab-model.yaml --adapters redfish,kwok,metrics` | this repository | 50 Nodes: GB300 and GB200 NVL72 trays, HGX H100 and A100, AMD, Gaudi, CPU |
| KRM `nodepool-controller`, `project-controller`, `pod-group-assigner` | `kai-scheduler/kai-resource-management` at `v0.18.3` (`b77c416`), `go build ./cmd/<name>` | node pools → node labels and SchedulingShards; departments and projects → KAI Queues and namespaces; pod admission (scheduler, project, default node pools); PodGroup assignment to a node pool and its queue |
| `kai-scheduler` × 4 (one per SchedulingShard), `binder`, `podgrouper`, `queuecontroller` | `kai-scheduler/KAI-Scheduler` at `v0.18.6` (`KAI_BIN` reuses compat/kai's build) | the scheduling |
| Run:ai `anyworkload-controller` with its WorkloadRunner webhooks | `dsx-ai-factory/run-ai` at `3.0.20` (`7428019`), a sparse checkout of its cluster modules, `go build ./cmd` | WorkloadRunner → the Job it wraps, through the Karta (workload map) for `batch/v1` Job (`RUNAI_WORKLOADS=none` skips it) |

KRM's services get the arguments its operator would give them (`pkg/operator/operands/*/resources.go`
at the chart's defaults) and serve their webhooks on loopback ports with a scratch CA; the chart's
webhook configurations (`helm template`, KAI subchart not fetched) are pointed at them by URL with
`../grove/lib/webhooks.py`. The chart's `default` NodePool is applied as rendered.

**Standing in for the KAI operator (IMPERSONATED).** KRM writes one `SchedulingShard` per node pool;
the KAI operator turns each into a scheduler Deployment and reports it `Deployed` and `Available`, and
KRM calls a node pool `Ready` only then. No Deployment can run on the twin, so `run.sh` starts one
`kai-scheduler` process per shard with the arguments the operator builds
(`pkg/operator/operands/scheduler/resources_for_shard.go`: scheduler name, node-pool label key,
`--partition-label-value`, queue label key, and the shard's `args` the scheduler knows) and writes
those two conditions itself. `$WORK/runai/shards.txt` lists the arguments each got.

**Standing in for Run:ai's agent and the workload-map operator (IMPERSONATED).** anyworkload-controller
renders a WorkloadRunner's template through the Karta for its kind, which a Run:ai cluster's agent
syncs from the control plane's workload types (labelled `run.ai/karta-group`, `-kind`, `-version`)
and the workload-map operator marks `Ready`. Neither runs here: the harness applies the Karta for
`batch/v1` Job from the karta module the controller requires (`docs/samples/batch-job.yaml`) with
those labels and a `Ready` condition. The controller runs configured by environment, as its
Deployment is, with the WorkloadRunner and RestartRequest CRDs `runai-chart` ships and the two
admission webhooks `go-operator` defines (`manifests/04-anyworkload-webhooks.yaml.tmpl`).

**KAI version.** KRM v0.18.3 bundles KAI v0.18.3; the run uses KAI v0.18.6 (compat/kai's build) for
the scheduler processes and CRDs. `KAI_REF=v0.18.3` builds the pinned one. KAI's podgroupcontroller
is not run: at v0.18.6 it binds `:8080` whatever `--metrics-bind-address` says
([compat/grove](../grove/README.md#finding-kais-podgroupcontroller-takes-8080-u6)); nothing here
reads the PodGroup status it writes.

`manifests/`: the NodePools (`gb300`, `gb200`, `h100` on `nvidia.com/gpu.product`; the NVL72 pools
prefer the `nvlink` Topology, `../kai/manifests/02-topology.yaml`), the Department `ai`, the Projects
`llm-train` (GB300 then GB200; `llm-train-gb300` 72/72 GPUs) and `serving` (H100, `serving-h100`
deserved 8, limit 16; also GB200), and the workload templates: a `batch/v1` Job gang (KAI's
`kai.scheduler/batch-min-member`, topology annotations) and an `apps/v1` Deployment. Neither names a
scheduler, queue or node pool: KRM supplies them from the project.

## Running it

```bash
make build
DCTWIN=$PWD/bin/dctwin compat/runai/run.sh
KAI_BIN=<compat/kai's $WORK/bin> KCM=<…/kube-controller-manager> DCTWIN=$PWD/bin/dctwin compat/runai/run.sh
RUNAI_WORKLOADS=none ...                                    # without Run:ai's workload controller
KEEP=1 ...
```

Needs Go, git, helm, curl, python3, openssl and envtest's binaries. `$WORK/evidence` keeps the
NodePools, SchedulingShards, Queues, Projects, the workloads (and the WorkloadRunner's status), their
pods and PodGroups, the pod-group-assigner's assignments in F5, and the twin's fidelity report.

## Result (2026-10-20, Run:ai 3.0.20 `7428019`, KRM `b77c416`, KAI `279f61c`, kube-controller-manager v1.37.0; x86_64 Linux)

| # | Check | Observed | Result |
|---|---|---|---|
| U1 | Nodes the twin registers | 50 Ready, 50 impersonated | PASS |
| U2 | KRM, KAI and Run:ai services | 7 of 7 running (KRM's three, KAI's binder, pod grouper, queue controller, anyworkload-controller); Run:ai 3.0.20 pins KRM v0.18.3 and KAI v0.18.3 | PASS |
| N1 | NodePools `gb300`, `gb200`, `h100` by `nvidia.com/gpu.product`, and `default` | all `Ready` in 0.2 s; Nodes labelled `kai.scheduler/node-pool` gb300 18, gb200 18, h100 4; the other 10 in `default` | PASS |
| N2 | A KAI scheduler shard per node pool | 4 SchedulingShards from KRM (`default` partition `""`, `gb200`, `gb300`, `h100`); 4 `kai-scheduler` processes started by the harness (IMPERSONATED: the KAI operator's part) | PASS |
| P1 | Department `ai`, Projects `llm-train`, `serving` | Queues per (owner, node pool): `llm-train-gb300` → `ai-gb300` 72/72, `llm-train-gb200` → `ai-gb200` 36/72, `serving-h100` → `ai-h100` 8/16, `serving-gb200` → `ai-gb200` 8/16 | PASS |
| P2 | Project namespaces | `kai-llm-train`, `kai-serving`, labelled `kai/project` | PASS |
| W1 | Job `pretrain`: 18 pods × 4 GPUs, one gang, required in one `nvidia.com/gpu.clique`; no scheduler, queue or pool named | 18 of 18 bound in 2.2 s on GB300 trays, 1 clique; PodGroup assigned node pool `gb300`, queue `llm-train-gb300` | PASS |
| W2 | KRM pod admission | `schedulerName: kai-scheduler`, label `project=llm-train`, required node affinity `nvidia.com/gpu.product In [NVIDIA-GB300]` or `[NVIDIA-GB200]` (the project's default pools) | PASS |
| W3 | Deployment `llm-serve`: 3 × 8 GPUs; `serving-h100` deserved 8, limit 16; 32 H100 GPUs free | 1 bound on an H100 server; 2 Pending: `NonPreemptibleOverQuota: Non-preemptible workload is over quota … serving-h100 quota is 8 GPUs` (inference is non-preemptible, so KAI holds it to deserved quota) | PASS |
| W4 | WorkloadRunner `finetune` (a Job of 4 × 4 GPUs, one clique) while `pretrain` holds every GB300 tray | anyworkload-controller created the Job, owned by the WorkloadRunner; the gang was moved from the full `gb300` pool to `gb200`: queue `llm-train-gb200`, 4 GB200 trays in 1 clique in 1.9 s; WorkloadRunner phase `Initializing` (from the Karta's status mapping) | PASS |
| F1 | `dctwin fault inject` Xid 79 under a `pretrain` pod, which is then drained | tray cordoned, allocatable 3 in 0.5 s; the Job controller's replacement stays Pending: the gang's clique has no other free GB300 tray | PASS |
| F2 | `dctwin fault clear` | the replacement binds to that tray in 0.9 s; 18 of 18 in 1 clique | PASS |
| F3 | `dctwin power off` the four H100 servers | `Ready=False/PoweredOff` on all four in 0.4 s | PASS |
| F4 | NodePool `h100` phase with none of its Nodes Ready | still `Ready` after 30 s, its status listing all four Nodes `Ready` | **FINDING** |
| F5 | Deployment `llm-serve-b` (2 × 4 GPUs) asking for `kai.scheduler/node-pools: "h100 gb200"`, with all H100 servers off | pod-group-assigner assigned `h100` first, the h100 shard could not place it, the assigner moved it to `gb200`: queue `serving-gb200`, both bound on GB200 trays in 2.6 s | PASS |
| F6 | `dctwin power on` the H100 servers | Ready again in 14.3 s; NodePool `h100` `Ready` | PASS |
| F7 | Workloads running; Run:ai workload status | not reached: no kubelet; Run:ai's control plane and status-updater not run | CANNOT |

17 checks: 15 PASS, 0 FAIL, 1 FINDING, 1 CANNOT, again after the bring-up was shared with
`controlplane.sh` (`lib/bringup.sh`). An earlier run on an arm64 Mac (without W4) gave the same
verdicts; times vary from run to run.

### Finding: KRM's node pool phase does not follow Node readiness (F4)

KRM documents a node pool whose Nodes are all not ready as `Unschedulable`, and its status code reads
the `Ready` condition (`isNodeReady`). But the nodepool-controller's Node watch only passes updates
that change `status.phase`, `spec.unschedulable` or labels
(`pkg/nodepool-controller/common/filter_common.go`, `FilterNodeUpdatesForNPController`). Kubelets
never set `status.phase` (the field is deprecated and unpopulated), so a Node whose `Ready` condition
goes `False` or `Unknown` (powered off, kubelet stopped) never re-evaluates its pool, which stays
`Ready` until some other change touches it. The twin does what a dead node does here: it sets the
condition and the node-lifecycle controller taints the Node; nothing cordons it. Placement still
recovers (F5) through KRM's other path, the shard reporting the PodGroup unschedulable and the
assigner moving it to the next requested pool, but a workload with only one candidate pool waits on
the dead pool without a phase that says so. A Node the twin cordons (an Xid, F1) does re-evaluate it.

### What KRM's placement looks like

```text
$ kubectl get pod -n kai-llm-train pretrain-… -o jsonpath='{.spec.schedulerName} {.metadata.labels.project}'
kai-scheduler llm-train
$ kubectl get podgroup -n kai-llm-train -o custom-columns=NAME:.metadata.name,QUEUE:.spec.queue,POOL:'.metadata.labels.kai\.scheduler/node-pool'
pg-pretrain-…   llm-train-gb300   gb300
$ grep 'Successfully assigned pod group <kai-serving/pg-llm-serve-b' krm-pod-group-assigner.log   # F5
… to: <&{Queue:serving-h100 NodePoolAssignmentParams:{NodePoolName:h100 MarkUnschedulable:false …
… to: <&{Queue:serving-gb200 NodePoolAssignmentParams:{NodePoolName:gb200 MarkUnschedulable:true …
```

## Run:ai's control plane (`controlplane.sh`)

```bash
BASE=<debian-13-genericcloud-amd64.qcow2> GUEST_DIR=<scratch> BIN=<binaries> SRC=<sources> \
  KUBEBUILDER_ASSETS=<envtest> compat/runai/controlplane.sh          # CPUS=6 MEM=7168 by default
```

Run:ai's control plane is about thirty services on Kubernetes, so it gets kubelets of its own: a
Debian 13 cloud image under QEMU/KVM with user-mode networking (the host's network and firewall are
untouched), k3s `v1.35.9+k3s1` in it, and the **published `3.0.20` control-plane chart and images**
(`helm.ngc.nvidia.com/nvidia/runai-oss`, `nvcr.io/nvidia/runai-oss/*`). Both are pulled
anonymously: **no NGC account or key is needed.** `controlplane/install-cp.sh` does what Run:ai's
`local-dev.sh` does on Kind (a self-signed CA and certificate for the domain, the
`runai-backend-tls` and `runai-ca-cert` secrets, the domain resolved to the ingress inside and outside
the cluster), installs the chart with `controlplane/values-cp.yaml`, waits for `/_version`, registers
a cluster through `POST /v1/k8s/clusters` and reads the installer's client. The values are the
chart's own switches only:

- one replica of everything but NATS: the chart's NATS configuration always declares a cluster with
  one route per replica, and a single replica waits for a JetStream meta leader that never forms
  until its liveness probe restarts it;
- Thanos, notifications (service and proxy) and Unleash off. `frontend.enabled` and
  `cliExposer.enabled` are set to `false` too, but 3.0.20's templates render those Deployments
  regardless, so the web UI and the CLI download server still run. metrics-service has to stay:
  the tenant is only created once its metric store is registered there;
- requests lowered so the pods fit a 6-core, 7 GiB guest (Run:ai documents 10 cores and 12 GB for
  the control plane's system nodes); limits are the chart's, except Keycloak's.

`controlplane/cluster.sh` then runs, in the same guest, the cluster side of `run.sh` over the twin's
Nodes (`lib/bringup.sh`; the twin with its `kwok` adapter only) plus the cluster services that talk
to the control plane, each from the `3.0.20` tag, configured by environment as `go-operator`'s
Deployments configure them:

| Process | Role here |
|---|---|
| Run:ai `agent` | pulls desired state: the node pools, departments and projects become KRM's `NodePool`, `Department` and `Project` objects; workload types become Kartas; a workload becomes a `WorkloadRunner` in the project's namespace |
| Run:ai `cluster-sync` | pushes state: cluster info, Nodes with their GPUs, node pools, Kartas' readiness, workloads and their pods |
| Run:ai `external-workload-integrator` | grants each workload type's RBAC and marks its Karta `SyncEnabled`; records the Job as an `ExternalWorkload`, which cluster-sync reports |
| workload-map (Karta) operator `v0.2.6` | the Karta chart `3.0.20` deploys (`dsx-ai-factory/workload-map`, `operator/`): validates each Karta and marks its kind present, built-in kinds included |
| anyworkload-controller, KRM, KAI | as in `run.sh`, with the names a Run:ai cluster gives KAI and KRM (below) |

The agent and cluster-sync use the cluster's own client from
`GET /api/v1/clusters/{uuid}/config/control-plane` (what the cluster chart's install hook reads with
the installer's credentials), from `/etc/secret/secret.json`, the file `go-operator` writes per
Deployment and the kubelet mounts there; the harness writes it. The agent, cluster-sync and the
integrator all start controller-runtime's metrics server at its default `:8080` with no setting to
move it; each has a pod, and so a network namespace, of its own in a cluster. Here cluster-sync and
the integrator each get one (`ip netns`), whose loopback carries the API server's port and the
control plane's 443, bridged over Unix sockets with `socat`, and read the scratch cluster's kubeconfig
(cluster-sync through `KUBE_CONFIG_FILE_PATH`, its out-of-cluster fallback). The three are restarted
when they exit, as their pods would be, and every restart is reported.

**Run:ai's names for KAI and KRM** (`RUNAI_PROFILE=1` in `lib/bringup.sh`). In a Run:ai cluster
`go-operator` configures KRM and KAI (`operands/krm-operator/resources/krm_configs.go`,
`operands/kai-operator/resources/global.go`): scheduler `runai-scheduler`, queue label `runai/queue`,
node-pool label `runai/node-pool`, the namespace's project label `runai/queue`, the scheduler
enforcement annotation `runai/enforce-scheduler-name`, finalizer domain `run.ai`; and the cluster
chart turns off KRM's node pool, project and department webhooks and KRM's own default NodePool
(the agent syncs it). Run:ai's own services find a project's namespace, and count a pod, only by those
names. Pod mutation, which the chart leaves to Run:ai's `admission-controller`, stays with KRM's
pod-group-assigner webhook here.

**Standing in for `go-operator` (IMPERSONATED).** `go-operator` reports the cluster's version and
status in the `runai-public` ConfigMap, which cluster-sync sends to the control plane; without it the
control plane refuses every workload (`cluster version is empty`). The harness writes it in
`go-operator`'s format: the chart's version, the platform and the API server's version, no operand
status, and the two dependencies it always reports, `gpu-stack` and `prometheus`, both unavailable
(the twin has neither). The control plane then calls the cluster `MissingPrerequisites`, which is
true, and still accepts workloads. The harness also does the KAI operator's part continuously, since
node pools now arrive whenever the agent writes them.

### Result (2026-10-20, Run:ai 3.0.20 control plane chart and images, cluster side `7428019`, workload-map `v0.2.6`; KVM guest 6 vCPU, 7 GiB, on x86_64 Linux)

| # | Check | Observed | Result |
|---|---|---|---|
| C1 | Cluster in the control plane after the agent and cluster-sync start | `MissingPrerequisites` (connected; GPU stack and Prometheus reported absent), version `3.0.20`, in 2.8 s | PASS |
| C2 | The twin's Nodes in the control plane (cluster-sync) | 50 nodes, 192 GPUs, 18 GB300 | PASS |
| C3 | Node pool `gb300` (`nvidia.com/gpu.product=NVIDIA-GB300`) by `POST /api/v1/node-pools` | the agent wrote the NodePool; KRM labelled 18 trays `runai/node-pool=gb300` and added its shard, in 6.2 s | PASS |
| C4 | Project `llm-train`, 72 GB300 GPUs deserved, `gb300` its default pool, by `POST /api/v1/org-unit/projects` | the agent wrote the Project; KRM made namespace `kai-llm-train` and queue `llm-train-gb300` (72), in 9.3 s | PASS |
| C5 | Workload `pretrain` (a `batch/v1` Job, 18 × 4 GPUs, required in one `nvidia.com/gpu.clique`) by `POST /api/v2/workloads` | accepted once the Job type was Ready; the agent wrote a WorkloadRunner, anyworkload-controller the Job; KAI bound 18 of 18 on GB300 trays of one rack (`a01`), 12 s later | PASS |
| C6 | The workload as the control plane sees it | `ExternalWorkload` `ew-job-pretrain`; phase `Pending`; 18 pods reported, each with its Node | PASS |

6 checks: 6 PASS. One earlier run of the same script also reported **R1 FINDING**: the agent
restarted once (below). The control plane came up healthy 234–269 s after `helm upgrade --install`
began; its 23 images take 6.1 GB in the guest.

### Memory

Working set per control-plane component (`crictl stats`, one replica each but NATS's three, with the
cluster side connected and the workload placed):

| Component | MiB | Component | MiB |
|---|---|---|---|
| Keycloak | 586 | identity-manager | 44 |
| runai-backend-traefik | 151 | datavolumes | 44 |
| postgresql | 109 | catalog-service | 43 |
| workloads-manager | 107 | k8s-objects-tracker | 43 |
| workloads | 87 | tenants-manager | 41 |
| NATS (3) | 81 | temp-backend-settings | 41 |
| assets-service | 77 | audit-service | 39 |
| policy-service | 74 | bff-service | 39 |
| authorization | 51 | cli-exposer | 34 |
| cluster-service | 51 | workloads-helper | 24 |
| org-unit-service | 51 | org-unit-helper | 12 |
| metrics-service | 48 | frontend | 12 |

The Run:ai pods total about 1.85 GiB; k3s's own (Traefik, CoreDNS, local-path, servicelb) 0.24 GiB,
the k3s server process 0.75 GiB and containerd 0.22 GiB. The cluster side (kube-apiserver 0.6 GiB,
kube-controller-manager, etcd, the twin, KAI's five processes, KRM's three, the Run:ai services,
the Karta operator) adds about 1.9 GiB RSS. With everything up, the guest used 3.9 GiB of 6.8 and
had 2.9 GiB available. On the host, QEMU's resident size was 5.7 GiB: the guest drops its page cache
every 30 s and reports freed pages to the host (virtio-balloon free page reporting), without which
the cache of pulled images kept a 10.5 GiB guest at 10.3 GiB resident. That guest, on a 14 GiB host
that was running other services, ran the host out of memory, and the kernel killed processes that
were not the guest's; hence the 7 GiB default. Give the guest `MEM` plus about 1 GiB of headroom on
the host, and cap it (for example `systemd-run --user --scope -p MemoryMax=…`) on a shared host.

### Findings

**The agent can panic on its first start.** `RunaiAgent.Start` creates the agent's ConfigMap with its
direct client (`preInitHandler`), then builds its handlers from the cache
(`handlers.NewWorkloadsHandler`, `configmaps.GetAgentConfig(cachedClient)`), whose error it discards;
when the cache has not seen the new ConfigMap yet, the nil result is dereferenced
(`workloads_handler.go:451`). Two of five first starts here panicked; the next start finds the
ConfigMap and runs. In a cluster the pod restarts, so this costs a restart, not the cluster.

**cluster-sync needs `runai-public` to carry `runaiConfigStatus`.** Given a `runai-public` without
it, cluster-sync panics in `parseClusterInfoStatusToServerScheme` (`cluster_info_parser.go:73`), and
the control plane rejects cluster info without `dependencies.required` (`400 required field
'required' is zero value`). `go-operator` always writes both; only a hand-written ConfigMap trips them.

**Three services cannot share a network namespace.** The agent, cluster-sync and the
external-workload-integrator (and KAI's podgroupcontroller, [compat/grove](../grove/README.md#finding-kais-podgroupcontroller-takes-8080-u6))
bind `:8080` with no flag or variable to move it. Harmless in pods; it is why two of them get
namespaces here.

## What was not run, and why

| Part of Run:ai 3.0.20 | Why not here |
|---|---|
| The control plane and the agent and cluster-sync, in `run.sh` | `run.sh` writes the cluster objects the agent would (Projects, Departments, NodePools, WorkloadRunners) directly. `controlplane.sh` runs all three (above). |
| `go-operator` (`RunaiConfig`) | It deploys every cluster component as a Deployment; none can run without a kubelet. Its components are run as processes instead, with the arguments and environment it would give them, and `controlplane.sh` writes its `runai-public` report. |
| The workload-map (Karta) operator, the agent's workload-type sync, in `run.sh` | `run.sh` writes the one Karta it needs (above); `controlplane.sh` runs both. |
| `admission-controller` | Pod attribute injection; KRM's pod-group-assigner webhook does the pod mutation placement needs (scheduler, project, queue, node pools). |
| `status-updater`, `runtime-rules`, `accessrule-controller`, metrics exporters | Workload status beyond what cluster-sync reports, time limits, RBAC from access rules, metrics: none decides placement. |
| The web UI and the Run:ai CLI | The API is driven directly with `curl`, as the UI and CLI drive it. |
| GPU fractions (`gpu-fractioning`), KAI reservation pods, MNNVL ComputeDomains | Need a kubelet, a device plugin or NVIDIA's DRA driver. |

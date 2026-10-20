# Kueue against the twin

[Kueue](https://github.com/kubernetes-sigs/kueue), built from the released tag `v0.20.2` and run
**unmodified**, admits batch Jobs by quota into ResourceFlavors keyed on the labels the twin's
`kwok` projection puts on its Nodes, places NVL72 gangs inside one NVLink domain with
topology-aware scheduling (TAS), and reacts to the twin's faults and power changes. The real
kube-scheduler binds the pods Kueue admits; the real kube-controller-manager runs the Jobs and the
node lifecycle.

> **Impersonated.** The Nodes are the twin's `kwok` projection ([D24](../../docs/spec/11-decisions.md)):
> no kubelet, container runtime, device plugin or CNI runs behind them, so a pod that is bound stays
> `Pending` forever. What this shows is **admission, placement and control flow**: what Kueue and
> kube-scheduler decide from Node objects whose capacity, labels, conditions and cordons follow the
> twin's model. It says nothing about a workload running.

## What runs

| Process | Source | Role |
|---|---|---|
| `etcd`, `kube-apiserver` 1.37.0 | envtest's binaries (`setup-envtest`) | the cluster |
| `kube-controller-manager`, `kube-scheduler` 1.37.0 | `kubernetes/kubernetes` at `v1.37.0`, built from source | Jobs to pods, node lifecycle (taints from conditions, `NoExecute` eviction); binding |
| `dctwin up examples/estates/lab-model.yaml --adapters redfish,kwok,metrics` | this repository | 50 Nodes, their leases, conditions, allocatable and cordons |
| Kueue manager | `kubernetes-sigs/kueue` at `v0.20.2` (`296ec3d`), `go build ./cmd/kueue` | admission, flavors, TAS, its mutating and validating webhooks, CRD conversion |

Kueue's install manifests are the ones its release is built from, `config/default` at the tag,
rendered offline by kubectl's built-in kustomize: the CRDs (with their conversion webhooks), the
webhook configurations and the empty webhook-certificate Secret. Its configuration is its own
(`config/components/manager/controller_manager_config.yaml`) with loopback ports, no leader
election, metrics off and `integrations.frameworks: [batch/job]` (`manifests/kueue-config.yaml.tmpl`).

What a pod gets that a process does not, and how the harness stands in for it:

- **The webhook Service.** `kueue-webhook-service` is an `ExternalName` Service for `localhost`,
  and the webhook configurations and CRD conversion stanzas carry the loopback port Kueue serves on
  (`lib/render.py service-port`, the only edit to Kueue's objects). kube-apiserver resolves
  the Service to `localhost:<port>` and still verifies the certificate as
  `kueue-webhook-service.kueue-system.svc`, so Kueue's own certificate rotation does the rest: it
  issues the certificate, stores it in the Secret and injects its CA into the configurations and
  CRDs.
- **The Secret volume.** Kueue's cert rotator waits for the certificate to appear in its
  certificate directory, which in a cluster is the Secret mounted into the pod; `lib/secret-volume.sh`
  copies the Secret's keys into that directory every two seconds, as the kubelet would.
- **The visibility API server** (`VisibilityOnDemand`) is off: it writes its certificates to
  `/visibility` (`pkg/visibility/server.go`), a path a process outside a container cannot create.

Every Job is created *unsuspended* with only the `kueue.x-k8s.io/queue-name` label; Kueue's
mutating webhook suspends it (row A5 shows a Job that stays suspended for lack of quota).

`manifests/` holds the flavors (one per product: `gb300`, `gb200` with the `nvlink` Topology,
`h100`, `a100`, `mi355x`, `gaudi3`), the ClusterQueues (`nvl72`: 72 GPUs each for the GB300 and
GB200 racks; `accel`: one resource group per device-plugin resource, quota equal to the estate's
devices), the LocalQueues and the Job template. TAS Jobs carry
`kueue.x-k8s.io/podset-required-topology: nvidia.com/gpu.clique`.

## Running it

```bash
make build
DCTWIN=$PWD/bin/dctwin compat/kueue/run.sh                          # PASS/FAIL table; artefacts under $WORK
WORK=/tmp/kueue DCTWIN=$PWD/bin/dctwin compat/kueue/run.sh          # reuse builds (Kueue ~15 min cold; KCM and scheduler ~10)
KCM=/path/kube-controller-manager KUBE_SCHEDULER=/path/kube-scheduler ...   # prebuilt at v1.37.0
KEEP=1 ...                                                          # leave everything running to poke at
```

Needs Go, git, python3 and envtest's binaries (its `kubectl` renders the manifests).
`BUILD_ONLY=1` clones and builds, then stops. `$WORK/evidence` keeps the rendered Jobs, the Workloads and ClusterQueues at
each step, the events and the twin's fidelity report.

## Result (2026-10-19, Kueue v0.20.2 `296ec3d`, kube-controller-manager and kube-scheduler v1.37.0)

On a Linux x86_64 box, the whole harness pinned to two cores (`taskset`), in about two minutes;
the same 20 checks passed earlier on macOS arm64. Times are from one run.

| # | Check | Observed | Result |
|---|---|---|---|
| U1 | Nodes the twin registers | 50 Ready, 50 impersonated | PASS |
| U2 | Creation `not-ready` taint lifted by the node-lifecycle controller | 0 Nodes keep it after 8.0 s | PASS |
| U3 | Kueue, kube-scheduler running; webhooks served | CA injected by Kueue into both webhook configurations; ClusterQueues `nvl72`, `accel` Active | PASS |
| A1 | Job 2 × 8 `nvidia.com/gpu` on `inference` | flavor `h100`, node selector injected, bound to 2 H100 Nodes in 0.5 s | PASS |
| A2, A3 | Jobs 1 × 8 `amd.com/gpu`, 1 × 8 `habana.ai/gaudi` | flavors `mi355x`, `gaudi3`; on an MI355X and a Gaudi 3 Node | PASS |
| A4 | Two more 2 × 8 GPU Jobs | the first takes the rest of `h100`; the second falls through to `a100` and binds to the A100 Nodes | PASS |
| A5 | 1 × 8 with `h100` and `a100` used up | not admitted ("insufficient unused quota"), Job stays suspended, no pods | PASS |
| A6 | 18 × 4, required in one `nvidia.com/gpu.clique` (TAS) | flavor `gb300`, topology assignment of 18 hosts, 18 pods bound in one clique in 1.6 s | PASS |
| A7 | ClusterQueue usage | `accel` h100=32 a100=16 mi355x=8 gaudi3=8; `nvl72` gb300=72 | PASS |
| B1 | Xid 79 on `a01/compute-tray-07/gpu-2` | Ready kept, GpuHealthy=False, cordoned, allocatable 3 | PASS |
| B2, B3 | `dctwin power off` a GB200 tray and an H100 server | Ready=False (PoweredOff), `not-ready` taints | PASS |
| C1 | 18 × 4 TAS gang after the faults | not admitted: `topology "nvlink" allows to fit only 17 out of 18 pod(s)`; no pods | PASS |
| C2 | two 15 × 4 TAS gangs | one in each NVLink domain (gb300, then gb200), none on a faulted tray | PASS |
| C3 | 2 × 8 H100 with one H100 server off and two held | **admitted by quota**; one pod bound, the other Pending (kube-scheduler: no Ready Node fits) | PASS |
| C4 | Xid 79 on a host of a running TAS gang, then that pod drained | the pod stays on the cordoned tray until drained; then Kueue moves that host in the assignment to a spare tray in the same clique and the replacement pod is bound there (9.8 s) | PASS |
| C5 | `dctwin power off` a host of a running TAS gang | after the pod's 20 s `not-ready` toleration the controller manager evicts it; Kueue replaces the host with a spare in the same clique (31.4 s) | PASS |
| D1 | power the H100 server on | the Pending pod of C3 binds to it (2.8 s) | PASS |
| D2 | faults cleared, trays on, gangs deleted | the 18-tray gang waiting since C1 is admitted and bound in one clique | PASS |

20 checks: 20 PASS.

### What the twin's faults do to Kueue

- **Quota is nominal; TAS reads the Nodes.** A ClusterQueue's quota is a number an administrator
  sets, not the Ready capacity: with an H100 server powered off, Kueue still admits a Job its
  quota allows, and the pod that no Ready Node can hold waits in kube-scheduler (C3) until the
  twin powers the server on (D1). Flavors with a Topology are different: TAS builds its view from
  the Nodes' allocatable, taints, readiness and the pods already bound, so the Xid'd tray (cordoned,
  3 GPUs) and the powered-off tray (`not-ready` taint) are not offered (C1, C2). This is Kueue's
  design, not a twin effect.
- **A cordon is not a failure.** Kueue's TAS node controller leaves a running pod on a Node that
  became unschedulable; only when the pod is gone (an operator or a health controller drains it) is
  the host marked unhealthy and replaced (C4). A powered-off host is replaced once the
  controller manager's taint eviction removes its pod (C5); with the default 300 s toleration that
  takes five minutes, so the Job template tolerates `not-ready` and `unreachable` for 20 s.

No twin bug was found by this harness. A powered-off host's Node goes `Ready=False` (reason
`PoweredOff`) at once and, when its lease has lapsed for the controller manager's 50 s grace,
`Unknown` with `unreachable` taints; a real Node skips the `False` (its kubelet says nothing).

## What this does not show

- That anything runs: bound pods stay `Pending`; Jobs never complete, so `waitForPodsReady`,
  completion and requeueing after success are not exercised. Pod deletion needs `--force`.
- Device plugins, DRA or partitioned GPUs: allocatable is written by the twin.
- MultiKueue, ProvisioningRequest admission checks, fair sharing, preemption and Kueue's other
  integrations (JobSet, Kubeflow, Ray, LeaderWorkerSet, plain pods): their CRDs are not installed.
- The visibility API (feature gate off, above).

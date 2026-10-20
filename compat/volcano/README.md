# Volcano against the twin

[Volcano](https://github.com/volcano-sh/volcano), built from the released tag `v1.16.0` and run
**unmodified**, discovers the twin's NVLink domains as HyperNodes from the labels its `kwok`
projection puts on the Nodes, binds gangs all-or-nothing inside one domain, places GPU, AMD and
Gaudi pods, and steers around the trays and servers the twin faults or powers off.

> **Impersonated.** The Nodes are the twin's `kwok` projection ([D24](../../docs/spec/11-decisions.md)):
> no kubelet, container runtime, device plugin or CNI runs behind them, so a bound pod stays
> `Pending` and a Volcano Job never reaches `Running`. What this shows is **placement and gang
> control flow** from Node objects whose capacity, labels, conditions and cordons follow the
> twin's model. It says nothing about a workload running.

## What runs

| Process | Source | Role |
|---|---|---|
| `etcd`, `kube-apiserver` 1.37.0 | envtest's binaries (`setup-envtest`) | the cluster |
| `kube-controller-manager` 1.37.0 | `kubernetes/kubernetes` at `v1.37.0`, built from source | node lifecycle (taints from conditions) |
| `dctwin up examples/estates/lab-model.yaml --adapters redfish,kwok,metrics` | this repository | 50 Nodes, their leases, conditions, allocatable and cordons |
| `vc-webhook-manager`, `vc-controller-manager`, `vc-scheduler` | `volcano-sh/volcano` at `v1.16.0` (`41f27d5`), `go build ./cmd/<name>` | admission (Jobs, PodGroups, Queues, HyperNodes); Job, PodGroup, Queue and HyperNode controllers with label-based HyperNode discovery; the scheduler |

The install comes from the release's own manifest at the tag
(`installer/volcano-development.yaml`): its CRDs, its seven admission webhook configurations and its
ConfigMaps. The components run with the chart's arguments and loopback addresses. Two ConfigMaps
are this harness's (`manifests/01-config.yaml`): the scheduler's, which is the chart's default plus
the `network-topology-aware` plugin as Volcano's network-topology guide says to enable it, and
`volcano-controller-configmap`, which turns on HyperNode discovery from the label
`nvidia.com/gpu.clique` (GPU feature discovery's, which the twin projects).

As for a pod: the admission Service is an `ExternalName` for `localhost` with the webhook
configurations' port set to the one vc-webhook-manager serves on (`../kueue/lib/render.py`); the
certificate is made with the openssl steps of the chart's admission-init Job
(`installer/dockerfile/webhook-manager/gen-admission-secret.sh`) and vc-webhook-manager patches its
CA into the configurations itself; ConfigMap keys are written to files as a ConfigMap volume would.

Workloads (`manifests/`): Volcano Jobs (`vcjob.yaml.tmpl`, one task, `minAvailable` for the gang,
optionally `networkTopology: {mode: hard, highestTierAllowed: 1}`: every pod in one tier-1
HyperNode) and bare pods with `schedulerName: volcano` (a PodGroup of one each), in the Queue
`dc-twin`.

## Running it

```bash
make build
DCTWIN=$PWD/bin/dctwin compat/volcano/run.sh                     # PASS/FAIL table; artefacts under $WORK
WORK=/tmp/volcano DCTWIN=$PWD/bin/dctwin compat/volcano/run.sh   # reuse builds
KCM=/path/kube-controller-manager ...                            # prebuilt at v1.37.0
KEEP=1 ...                                                       # leave everything running
```

Needs Go, git, python3, openssl and envtest's binaries. `BUILD_ONLY=1` clones and builds, then stops.

## Result (2026-10-19, Volcano v1.16.0 `41f27d5`, kube-controller-manager v1.37.0)

On a Linux x86_64 box, the whole harness pinned to two cores (`taskset`), in 70 s; the same 20
checks passed earlier on macOS arm64. Times are from one run.

| # | Check | Observed | Result |
|---|---|---|---|
| U1, U2 | Nodes; creation taint lifted | 50 Ready; 0 `not-ready` taints after 9.0 s | PASS |
| U3 | Volcano running | 3 of 3; CA in 7 of 7 webhook configurations; queues `root`, `default` made by the scheduler; `dc-twin` admitted through the queue webhooks, Open | PASS |
| U4 | HyperNodes from the twin's labels | two tier-1 HyperNodes of 18 Nodes, one per NVL72 rack, labelled with the clique id | PASS |
| A1 | Volcano Job 18 × 4 GPU, `minAvailable` 18, one HyperNode, GB300 | 18 bound to 18 Nodes in one clique in 1.2 s | PASS |
| A2–A5 | Pods: H100 × 8, A100 × 8, `amd.com/gpu` × 4, `habana.ai/gaudi` × 4 | each on a Node of its product in 0.1–0.5 s | PASS |
| A6 | `amd.com/gpu: 2` with **no** node selector | on an MI355X server (allocatable `amd.com/gpu` 8) | PASS |
| A7 | Queue accounting | `nvidia.com/gpu` 88, `amd.com/gpu` 6, `habana.ai/gaudi` 4 | PASS |
| B1–B3 | Xid 79 on a GB300 tray; a GB200 tray and an H100 server powered off | cordoned and allocatable 3; Ready=False (PoweredOff) with `not-ready` taints | PASS |
| C1 | 18 × 4 gang, one HyperNode, after the faults | 0 bound; PodGroup: `1/18 tasks in gang unschedulable … HyperNode: … 1 node(s) had untolerated taint(s), 17 Insufficient nvidia.com/gpu` | PASS |
| C2 | 17 × 4 gang, one HyperNode | 17 bound in one clique (rack a01), none on a faulted tray | PASS |
| C3 | 3 × 8 H100 gang with one H100 server off and one taken | 0 bound (two fit, the gang needs three); PodGroup Unschedulable | PASS |
| D1 | power the H100 server on | the whole gang binds, one pod on that server (3.0 s) | PASS |
| D2 | clear the Xid, power the tray on | Ready, uncordoned, allocatable 4 | PASS |
| D3 | delete the 17-tray gang | the 18-tray gang waiting since C1 binds in one clique | PASS |

20 checks: 20 PASS.

### Findings

- **Volcano keeps AMD GPUs apart from NVIDIA GPUs** (A6): `amd.com/gpu` is an ordinary scalar
  resource to it, so a pod with no node selector lands only where `amd.com/gpu` is allocatable.
  The same estate run through KAI Scheduler binds such a pod to an NVIDIA tray
  ([compat/kai](../kai/README.md), finding in row A7), because KAI folds the two into one GPU
  dimension.
- **HyperNodes come straight from the twin's projection.** The label discoverer turns the
  `nvidia.com/gpu.clique` values the twin derives from each NVL72 domain (GFD's
  `<ClusterUUID>.<CliqueID>` shape) into one HyperNode per rack, and the network-topology plugin
  then names the HyperNodes and the faulted Nodes in a gang's Unschedulable condition (C1).

No twin bug was found by this harness.

## What this does not show

- That anything runs: no pod becomes Running, so Volcano Job lifecycle policies (restart, abort on
  pod failure), `minSuccess`, and the job plugins (ssh, svc, env, PyTorch, MPI) are not exercised.
- Device sharing (`deviceshare`, vGPU) or NUMA-aware scheduling: no device plugin or kubelet.
- Preemption, reclaim and proportion across queues with capacities; HyperNode discovery from UFM
  or RoCE (the twin serves a UFM API, `internal/adapter/ufm`, which this run does not wire to
  Volcano's UFM discoverer).

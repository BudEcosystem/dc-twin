# AI Cluster Runtime (aicr) against the twin

NVIDIA's [AI Cluster Runtime](https://github.com/NVIDIA/aicr) CLI, built from the `v1.0.0` tag
with the release's version stamp and run **unmodified**, against the twin's Kubernetes view: what
`aicr snapshot` recognises about the twin's Nodes, the recipe `aicr recipe` derives from that, what
`aicr validate --no-cluster` accepts, and which steps cannot work on a twin.

> **Impersonated.** The Nodes are the twin's `kwok` projection ([D24](../../docs/spec/11-decisions.md)):
> labels, capacity and conditions follow the model, and there is no kubelet, operating system, PCI
> bus or GPU behind them. aicr's cluster-wide collectors read the Nodes; its host collectors and its
> Jobs have nothing to read or run on.

## What runs

The same control plane as [`compat/kai`](../kai/README.md) (envtest's `etcd` and `kube-apiserver`
1.37.0, and `kube-controller-manager` 1.37.0 built from source unless `KCM=none`), with two estates
in turn:

| Estate | Nodes | Why |
|---|---|---|
| `examples/estates/lab-model.yaml` | 50: 18 GB300 and 18 GB200 trays, 4 H100, 2 A100, 2 MI355X, 2 Gaudi 3 servers, 4 CPU servers | every accelerator the twin projects |
| `examples/estates/nvl72-refstack.yaml` | 18 GB300 trays of one NVL72 rack | a single GPU product, which is when aicr names an accelerator |

`aicr snapshot` runs its collectors locally (`AICR_AGENT_MODE=true`), the mode that deploys no Job.
In that mode the node-topology and Kubernetes collectors take the cluster from `$KUBECONFIG`
(`pkg/snapshotter/snapshot.go`), not from `--kubeconfig`: with only the flag set they fall back to
in-cluster configuration and return empty measurements. `run.sh` sets both, and a `HOME` of its
own, so aicr never reads a user's `~/.kube/config`.

## Running it

```bash
make build
DCTWIN=$PWD/bin/dctwin compat/aicr/run.sh
WORK=/tmp/aicr KCM=/path/to/kube-controller-manager DCTWIN=$PWD/bin/dctwin compat/aicr/run.sh
```

`$WORK/evidence` keeps both snapshots (JSON), the recipes, the CTRF reports and every log.

## Result (2026-10-18, aicr v1.0.0 `82bccef`)

| # | Check | Observed | Result |
|---|---|---|---|
| X1 | `aicr snapshot` (local collectors), lab estate | exit 0; fingerprint nodeCount 50, gpuNodeCount 42, k8sVersion 1.37.0, region site-1 | PASS |
| X2 | GPU products from `nvidia.com/gpu.product` | NVIDIA-GB300=18, NVIDIA-GB200=18, NVIDIA-H100-80GB-HBM3=4, NVIDIA-A100-SXM4-80GB=2 | PASS |
| X3 | Other accelerator and NVLink labels | AMD_Instinct_MI355X=2, HL-325L=2, two `nvidia.com/gpu.clique` values of 18 Nodes | PASS |
| X4 | Fingerprint accelerator, mixed estate | empty, note `multi-gpu` | PASS |
| X5 | Host collectors (PCI GPU discovery, OS release, kernel modules, GRUB, systemd) | 5 skipped; GPU measurement empty | CANNOT |
| X6 | `aicr recipe --snapshot`, lab estate | exit 0; 12 components; criteria accelerator any, nodes 50 | PASS |
| X7 | `aicr validate --no-cluster` | exit 0; readiness `K8s.server.version >= 1.32` passes (v1.37.0); 24 checks skipped | PASS |
| X8 | `aicr snapshot` as an agent Job (its default mode), 30 s timeout | exit 5 (TIMEOUT); the Job's pod is `Pending` with `schedulerName: default-scheduler` and no node | CANNOT |
| X9 | `aicr snapshot`, one GB300 rack | fingerprint accelerator **gb300** (source `nodeTopology.label.nvidia.com/gpu.product`), gpuNodeCount 18; gpuModel empty (it comes from PCI IDs) | PASS |
| X10 | `aicr recipe --snapshot`, GB300 rack | exit 0; overlays base, monitoring-hpa, **gb300-any** | PASS |
| X11 | `aicr recipe --snapshot --service generic --intent training --os ubuntu` | exit 2; the `gb300-generic-ubuntu-training` overlay excluded: its `os-ubuntu` mixin's three constraints have no OS to read | CANNOT |
| X12 | `aicr validate --no-cluster`, generic GB300 training recipe | exit 2; readiness cannot evaluate `OS.release.ID` | CANNOT |

12 checks: 8 PASS, 4 CANNOT (expected on a twin).

```text
$ AICR_AGENT_MODE=true aicr snapshot --format json -o snapshot-gb300.json     # X9
fingerprint: accelerator gb300 (nodeTopology.label.nvidia.com/gpu.product), gpuNodeCount 18,
             nodeCount 18, k8sVersion 1.37.0, region site-1
$ aicr validate --recipe recipe-gb300-training.yaml --snapshot snapshot-gb300.json --no-cluster   # X12
[cli] readiness constraint passed: name=K8s.server.version expected=>= 1.34 actual=v1.37.0
[cli] command failed: error=[INVALID_REQUEST] readiness check could not evaluate: OS.release.ID:
      [NOT_FOUND] value not found in snapshot
```

## What aicr cannot do here

- **Anything that runs in a pod**: the snapshot agent Job (X8), every validator Job of the
  deployment, conformance and performance phases, and NCCL all-reduce. Without
  `kube-controller-manager` the Job does not even get a pod. With one, the pod waits for a
  `default-scheduler` nobody runs, and even a scheduler could only bind it to a Node with no kubelet.
- **OS- and hardware-derived readings**: OS release, kernel version, kernel modules, GRUB, systemd
  units and PCI GPU discovery read the machine the collector runs on. On the twin's Nodes there is
  no such machine, so a recipe whose overlays constrain the OS (every `os=ubuntu` training recipe)
  cannot be composed from a twin snapshot or validated against one (X11, X12). Query mode (no
  snapshot) composes it; it just cannot be checked.
- **GPU-node auto-targeting** works since D43: the twin's Nodes carry GPU Feature Discovery's
  `nvidia.com/gpu.present=true`, and aicr logs `auto-targeting GPU nodes:
  selector=nvidia.com/gpu.present=true` before the Job times out as above (X8). The first run found
  the label missing, so the Job had no node selector.

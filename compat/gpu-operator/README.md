# NVIDIA's Kubernetes GPU stack on the twin's GPUs

NVIDIA's device plugin, GPU Feature Discovery, the DRA driver for GPUs and dcgm-exporter, and Node
Feature Discovery, all released and pinned and run **unmodified**, on **real kubelets** whose GPUs
are the twin's: each selected twin host is one Kind node named for it, and NVIDIA's
[Mokka](https://github.com/NVIDIA/k8s-test-infra) (the `nvml-mock` driver tree, mock
`libnvidia-ml.so`, CDI specs and NRI injection) runs on it with a GPU profile dc-twin renders from
the model. This fills the "Kubernetes enablement" and "Metrics" slots of
[06 — The same slots, every vendor](../../docs/spec/06-packs-and-stacks.md#the-same-slots-every-vendor)
with the real components, where the twin's `kwok` projection and dcgm-exporter impersonation
(D24) stand in for them otherwise.

> **Container hosts** ([D49](../../docs/spec/11-decisions.md)). The kubelet, containerd and
> everything scheduled on them are real. The operating system is a container (a Kind node) on the
> Docker host, so kernel, firmware, PCI bus, NUMA and power are the Docker host's; the GPUs are the
> twin's model as Mokka's mock NVML presents it — identity, count, memory, NVLink domain, rack
> placement and fault state from the model, every reading the model has no source for (clocks,
> power, temperature, ECC shape) from Mokka's profile of the same silicon. No kernel runs on a GPU.

## What runs

| Component | Pinned | Role |
|---|---|---|
| Kind | `v0.31.0` (built with `go install`), node image `kindest/node:v1.35.0@sha256:452d707d…` | one control plane and one worker per twin host, the worker named `<rack>-<device>` (kubeadm `nodeRegistration.name`) as the twin's kwok projection and dcgm-exporter impersonation name it, labelled `dc-twin.io/host=container` and `dc-twin.io/path` |
| Mokka `nvml-mock` | chart `oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock` `0.5.0` (sha256 `f9c290ae…`), image `ghcr.io/nvidia/nvml-mock:0.5.0`; k8s-test-infra `v0.5.0` (`e2950d58`) | one Helm release per container host (`nodeSelector` on its hostname, the chart's documented heterogeneous-fleet pattern), each with `gpu.customConfig` = the profile `dctwin-mokka render` wrote for that host |
| NVIDIA device plugin with GPU Feature Discovery | chart `nvidia-device-plugin` `0.19.3` (sha256 `8f010675…`), image `nvcr.io/nvidia/k8s-device-plugin:v0.19.3` | `nvidia.com/gpu` allocatable from NVML; GFD's `nvidia.com/gpu.*` labels. The release the GPU Operator `v26.3.3` deploys as its operands |
| Node Feature Discovery | chart `node-feature-discovery` `0.19.0` (sha256 `9e93b360…`), image `registry.k8s.io/nfd/node-feature-discovery:v0.19.0` | `feature.node.kubernetes.io/pci-10de.present` from Mokka's feature file; GFD's labels through its master. The version Mokka's CI pins |
| NVIDIA DRA driver for GPUs | chart `nvidia/dra-driver-nvidia-gpu` `0.5.0` (sha256 `f9fd2856…`), image `nvcr.io/nvidia/dra-driver-nvidia-gpu:v0.5.0` | ResourceSlices (`resource.k8s.io/v1`) and claims for `gpu.nvidia.com`. The version Mokka's CI pins |
| dcgm-exporter | image `nvcr.io/nvidia/k8s/dcgm-exporter:4.5.3-4.8.2-distroless` | the GPU Operator `v26.3.3`'s operand, as a plain DaemonSet (`manifests/dcgm-exporter.yaml`) with its embedded host engine |
| `dctwin up examples/estates/lab-model.yaml --adapters kwok,metrics` | this repository | the twin; its kwok Nodes on an envtest kube-apiserver and its dcgm-exporter impersonation are what the real software is compared with |
| `dctwin-mokka render`, `dctwin-mokka sync` | this repository (`cmd/dctwin-mokka`, `internal/mokka`) | the profiles; and, while the twin runs, each node's Mokka override document kept equal to the model through `put-override.sh` |

Every image is published for `amd64` and `arm64` (checked with `docker manifest inspect`); the run
below is `x86_64`, in a KVM guest (see [Rootless hosts](#rootless-hosts-kind-inside-a-kvm-guest)).

**Why not the whole GPU Operator.** Its operands are installed one by one, at the versions the
operator `v26.3.3` pins (the release Mokka's CI validates; `v26.7.x` gates every operand on an
`nvidia` line in `/proc/modules` that Mokka serves only to preloaded processes). The operator's
own image (and its validator's) would add a pull to every node of a Docker host whose disk this
run shares; what the operator adds over its operands here is lifecycle management of a driver and
toolkit that Mokka replaces (`driver.enabled=false`, `toolkit.enabled=false` in Mokka's own
operator values). Mokka's guide `docs/guides/gpu-operator.md` is the route for the operator
itself, and `values/` here carry the same settings its values do.

## How the twin drives Mokka

1. **The profile** (`dctwin-mokka render`, `internal/mokka.Render`). For each host, Mokka's own
   profile of the same silicon (`gb300`, `gb200`, `h100`, …, from the pinned chart's `profiles/`)
   with the model written over it: the NVML name the twin's packs use (D43: `NVIDIA GB300`), the
   count, identity/v1 UUIDs (`GPU-…`) and thirteen-digit serials, the PCI bus numbering the twin's
   dcgm-exporter impersonation and fault catalogue use (`0000:18:00.0` + `0x10` per index), memory
   from the profile's `memoryGiB` (or the model name), the device's `gpu` firmware as VBIOS, the
   compute capability of the twin's GFD derivation, the NVL72 domain's cluster UUID with clique 1
   (kwok's `nvidia.com/gpu.clique`), and the tray's chassis serial, slot and tray index (what the
   twin's NMX-C and RMS report). A host outside an NVL domain gets the zero cluster UUID a real
   HGX H100 reports. InfiniBand is off (Mokka cannot carry the twin's port GUIDs). `hosts.json`
   records each host's GPUs and the GFD labels the profile implies; the notes say what is Mokka's.
2. **The override document** (`dctwin-mokka sync`, `internal/mokka.Overrides`). Mokka's one
   mechanism for runtime change is `overrides.yaml` next to the profile, which every loaded mock
   library re-reads on a one-second TTL (`nvml-mock-ctl` writes it; Mokka has no API). The sync
   follows the twin's control API (event stream, plus a resync every 10 s) and rewrites the whole
   document of a host whenever its accelerators change, through `put-override.sh` (`kubectl exec`
   into that node's nvml-mock pod, an atomic rename, then `nvml-mock-ctl status` to read it back).
   A fault whose catalogue entry carries `shim: {driver: nvidia/mokka, …}` becomes a `failure`
   block: `nvidia/xid/xid-79` (`fault: fallen-off-bus`) is `{mode: fallen_off_bus, xid: {code:
   79}}`; clearing the fault empties the document.

## Running it

```bash
compat/gpu-operator/run.sh                    # table on stdout; evidence under $WORK
HOSTS='a01/compute-tray-01 b01/server-01' FAULT_GPU=a01/compute-tray-01/gpu-2 compat/gpu-operator/run.sh
NODE_MEMORY=1500m compat/gpu-operator/run.sh  # cap each Kind node's memory on a shared host
KEEP=1 compat/gpu-operator/run.sh             # leave the cluster, the twin and the sync running
KEEP=wait compat/gpu-operator/run.sh          # the same until $WORK/release exists, then tear down
```

Needs a **rootful** Docker or Podman, helm 3.8+, kubectl, Go, python3 and envtest's binaries
(`KUBEBUILDER_ASSETS`). Nothing reads `~/.kube/config` or the user's helm repositories: the
cluster's kubeconfig and a helm home are under `$WORK`. Charts are pulled at their pinned versions
and checked against their sha256. The cluster is `dctwin-gpuop`; it is deleted at the end, and the
Kind node image with it when the run pulled it (`KEEP_IMAGES=1` keeps it). Images the nodes pull
live inside the node containers and go with them.

### Rootless hosts: Kind inside a KVM guest

**Mokka cannot run under rootless Kind.** Its node agent creates its driver tree's character
devices with `mknod` (`/dev/nvidia<N>`, `nvidiactl`, `nvidia-uvm`, the MIG capability nodes), and
the kernel refuses `mknod` of a device in every user namespace but the first — a `--privileged`
rootless container included (`mknod: Operation not permitted`). A Kind node of rootless Docker or
Podman is in one, so the agent fails to stage and Mokka never starts; no cgroup delegation or
engine setting changes that, and Mokka has no rootless mode. `run.sh` refuses a rootless engine
with that reason.

`guest.sh` runs the harness inside a KVM guest instead: a Debian 13 genericcloud image (only read;
the guest writes a qcow2 overlay, deleted at the end) under QEMU with user-mode networking, so the
host's network and firewall are not touched; cloud-init installs `docker.io` in the guest; the Go
toolchain, Kind, helm, kubectl, envtest's binaries and this repository are copied in; `run.sh` runs
there, and its evidence is copied back. The results below were produced this way, on an x86_64
host whose container engine is rootless Podman.

```bash
BASE=/path/debian-13-genericcloud-amd64.qcow2 GUEST_DIR=/path/scratch \
  KUBEBUILDER_ASSETS=/path/envtest CPUS=4 MEM=6144 DISK=25G RUN_ENV="NODE_MEMORY=1500m" \
  compat/gpu-operator/guest.sh
RUN_TESTS=1 …   # then `go test ./internal/...` in the same guest
HOLD=1 …        # keep the guest up for further runs: touch $GUEST_DIR/rerun, or $GUEST_DIR/shutdown
```

## Results

Run on 2026-10-20 with `guest.sh` (Debian 13 guest, 4 vCPU, 6 GiB, Docker 26.1.5, x86_64, under
KVM), `NODE_MEMORY=1500m`, the lab estate's GB300 tray `a01/compute-tray-01` and HGX H100 server
`b01/server-01` as container hosts, and Xid 79 on `a01/compute-tray-01/gpu-2`.
**40 checks: 20 PASS, 0 FAIL, 18 FINDING, 2 CANNOT.** The full table is the harness's output
(`$WORK/results.md`); here grouped:

| # | Check | Result | Observed |
|---|---|---|---|
| N | each twin host is a real kubelet's Node, named and labelled for it | PASS ×2 | `a01-compute-tray-01`, `b01-server-01`: Ready, kubelet v1.35.0, containerd 2.2.0 |
| S | real `nvidia-smi` (Mokka's) on each node reports the twin's GPUs: name, UUID, serial, PCI bus id, memory, VBIOS, compute capability | PASS ×2 | every field equal to the model's derivation, e.g. GPU 0 of the tray `GPU-c4625f88-…` at `00000000:18:00.0`, 294912 MiB, VBIOS `97.10.4A.00.01`, 10.3. The H100 profile has no `gpu` firmware, so its VBIOS is Mokka's and not compared |
| D | the device plugin's `nvidia.com/gpu` allocatable | PASS ×2 | 4 and 8, equal to the model and to the twin's kwok Nodes |
| G | GFD's `product`, `count`, `memory`, `family`, `compute.major/minor`, `machine` and `clique` equal the model and the twin's projection (D43) | PASS ×2 | `NVIDIA-GB300`, 4, 294912, blackwell, 10.3, clique `162c0be9-….1` (the NVL72 domain's, as kwok writes it); `NVIDIA-H100-80GB-HBM3`, 8, 81920, hopper, 9.0, no clique |
| F | NFD's `feature.node.kubernetes.io/pci-10de.present` from Mokka's feature file | PASS ×2 | `true` |
| P | a pod asking the device plugin for one GPU; `nvidia-smi -L` inside it | PASS ×2 | `GPU 0: NVIDIA GB300 (UUID: GPU-64fc3cb3-…)`, the twin's `a01/compute-tray-01/gpu-2`; `NVIDIA H100 80GB HBM3 GPU-21b56eca-…`, the twin's `b01/server-01/gpu-6` |
| R | DRA ResourceSlices (`resource.k8s.io/v1`, driver `gpu.nvidia.com`) list the twin's GPUs; a ResourceClaim allocates one, and its pod sees it | PASS ×3 | 4 and 8 devices whose `uuid` set equals the model's, `productName` the twin's, `resource.kubernetes.io/pciBusID` `0000:18:00.0`…, `memory` 288Gi / 80Gi; the claim got `gpu-0` = `GPU-c4625f88-…`, which `nvidia-smi -L` in the pod lists alone |
| M | real dcgm-exporter (Mokka under its embedded DCGM) against the twin's dcgm-exporter impersonation | PASS ×2 | `gpu`, `UUID`, `pci_bus_id`, `modelName`, `device` equal for all 12 GPUs |
| F1 | Xid 79 on `gpu-2` in the twin reaches the tray's Mokka override document | PASS | `{mode: fallen_off_bus, xid: {code: 79}}` on device 2, applied 228 ms after the twin committed the fault |
| F2 | NVML on the node after it | PASS | `nvidia-smi`: GPU 2 `[GPU is lost]`, GPUs 0, 1, 3 answer |
| F6 | clearing the fault in the twin | PASS | the document empties 232 ms later; NVML answers for GPU 2 again |
| C1 | the twin's power and AC state reach the node | CANNOT | a container host keeps running (D49) |
| C2 | an Xid line on the node's kernel log | CANNOT | a Kind node's kernel ring buffer is its host's, shared by every container on it, so Mokka's `nodeAgent.kernelLog` stays off |

### Findings

| # | Finding | Whose |
|---|---|---|
| F3 | **After Xid 79 on one GPU the device plugin v0.19.3 withdraws all four** (allocatable 4 → 0 in 8.5 s) where the twin's projection withholds one (4 → 3, D43). Its log: `health.go:126] Error waiting for event: ERROR_GPU_IS_LOST; Marking all devices as unhealthy`: once one GPU of the event set has fallen off the bus, NVML's event-set wait returns `GPU_IS_LOST` (as NVML documents and Mokka models), and the plugin marks every device unhealthy on any such error | the device plugin's behaviour; the twin's D43 rule does not reproduce it |
| F7 | **Allocatable does not come back when the fault clears**, with NVML healthy again: the device plugin never re-admits a device it marked unhealthy. It returns to 4 1.8 s after the plugin's pod restarts, as on a host whose Xid 79 is cleared by a reboot | the device plugin; the twin's projection restores 4 at once |
| F4 | The DRA driver 0.5.0, at its chart's defaults, keeps publishing the lost GPU, untainted | the DRA driver |
| F5 | **DCGM on Mokka does not see the GPU fall off the bus**: the real exporter keeps serving all 23 families for GPU 2 (temperature 38, power 175 W) and serves no `DCGM_FI_DEV_XID_ERRORS` series before or after; the twin's impersonation serves `DCGM_FI_DEV_XID_ERRORS 79` ("GPU has fallen off the bus") and drops the GPU's telemetry | Mokka with DCGM 4.5.3 (Mokka's docs say injected Xids reach DCGM; not for this mode here) |
| M-labels | The GPU Operator v26.3.3's dcgm-exporter (4.5.3-4.8.2) labels `Hostname` and adds `DCGM_FI_DRIVER_VERSION`; the twin impersonates 4.8.4, whose label is `hostname` and which has no driver label (no driver is modelled) | version skew: the twin's impersonation follows a later release than the operator ships |
| M-families | The real exporter's default counters give 23 families (clocks, energy, remapped rows, PCIe replay, `DCGM_FI_PROF_*`, …); the impersonation serves 9, and serves the ECC volatile totals and `DCGM_FI_DEV_XID_ERRORS` the real one did not | the twin's impersonation (its own `isNot` says so) |
| X, Y | GFD also writes `nvidia.com/cuda.driver*`, `cuda.runtime*`, `gpu.mode`, `gpu.replicas`, `gpu.sharing-strategy`, `mig.capable`, `mps.capable`, `vgpu.present`, which the twin's projection does not; the projection writes `nvidia.com/gpu.present`, which GFD does not (the GPU Operator's controller writes it) | the twin's projection (a subset of GFD's labels) |
| G-machine | On the H100 server GFD's `nvidia.com/gpu.machine` is `NVIDIA-H100-80GB-HBM3`: Mokka's node agent serves the GPU's name as the machine type (its profiles have no platform field); the twin projects `PowerEdge-XE9680` | Mokka |
| A, K, H | `kubernetes.io/arch` amd64 where the tray's model is arm64 (Grace); cpu 4 and memory 6 GiB where the model has 144 cores and 1152 GiB; NFD finds the guest's QEMU VGA (`pci-0300_1234`) | container-host fidelity (D49), by design |

What rendering the profiles found, against Mokka's own hardware captures
(`tests/e2e/go/assertions/nvidiasmi/testdata/hardware`, `nvidia-smi -q -x` of real nodes):

- **The GB300 NVML name is `NVIDIA GB300`**, as the twin's packs say (D43); Mokka's `gb300`
  profile says `NVIDIA GB300 NVL` (Mokka).
- **Compute capability**: the twin derives 10.3 for GB300 (Blackwell Ultra); Mokka's `gb300`
  profile says 10.0 (Mokka, by the CUDA documentation the twin's table follows).
- **Memory** (fixed after this run): a real GB300 reports 284208 MiB and a real H100 81559 MiB;
  the twin's profiles gave the marketed sizes (294912 and 81920 MiB). Profiles now carry NVML's
  figure (`memoryMiB`), which GFD's `nvidia.com/gpu.memory`, Mokka and the exporters report.
- **Serials** (fixed after this run): on GB200/GB300 the two GPUs of a superchip report the same
  board serial; the twin derived one per GPU. The GB tray profiles now set `serialShare: 2`.
- **PCI addresses**: real GB300 GPUs sit in domains 8 and 9 (`00000008:06:00.0`); the twin's
  numbering (`00000000:18:00.0` + `0x10` per GPU) is its own convention, shared by its
  dcgm-exporter impersonation and fault catalogue, and the profile follows it.
- **Redfish serves no GPU UUID or serial** on the captured GB tray (`Processors/GPU_<n>`,
  `Chassis/HGX_GPU_<n>`) or the generated tree, so there is nothing there to compare with NVML.

### What this run does not show

The GPU Operator's own controller, driver and toolkit operands, and its validator (see
[Why not the whole GPU Operator](#what-runs)); MIG; anything that executes on a GPU; IMEX and
ComputeDomains (the DRA driver's are off); NVSentinel, which reads the kernel log this harness
cannot write (its own harness). Everything in [D49](../../docs/spec/11-decisions.md)'s "neither".

# NVIDIA Fleet Intelligence agent against the twin

NVIDIA's Fleet Intelligence agent, `fleetint` **1.5.0** (`a83abe9`), installed **unmodified** from its
released `.deb` and run under its own systemd unit in its **offline mode**, on a Debian guest whose
GPUs are the twin's: NVIDIA's Mokka (`nvml-mock` 0.5.0) presents the eight H100s of the estate's Dell
XE9680 (`b01/server-01`) to NVML from a profile `dctwin-mokka render` writes from the model. It lists
the twin's GPUs (UUIDs, serials, PCI ids, memory) and reports healthy; an Xid 79 and an Xid 48 the twin
injects reach it as the twin's own kernel-log lines and as NVML's view of the GPU, and it reports
each against the right GPU, `Fatal`, with `REBOOT_SYSTEM`; it keeps them until the guest reboots.

> **Two guests, one stand-in.** The twin's kernel-log lines are printk'd by the twin's in-host agent
> (`internal/inhost`, both halves and its kernel module `dctwin-kmsg` unmodified) into the kernel of
> the XE9680's own `os: twin` guest, a real x86_64 VM under KVM; that guest is Alpine's netboot
> initramfs (musl) and cannot host a cgo, glibc agent. `fleetint` therefore runs in a second guest
> (Debian 13, glibc), and the records the twin's guest kernel recorded are entered into that guest's
> kernel through the same module, built there for its kernel, by `kmsg-relay.py`, the harness's one
> stand-in, for the last hop only: the text is the record's own, at its level, and both kernels give
> it facility `kern`, as a driver's printk has ([D50](../../docs/spec/11-decisions.md)). The
> GPUs are the twin's model as Mokka's mock NVML presents them (identity, count, memory, fault state
> from the model; clocks, power, temperature, ECC shape from Mokka's `h100` profile); the server
> around them (CPU, memory, DMI) is the Debian guest's, and no kernel runs on a GPU.

## What runs

| Process | Pinned | Role |
|---|---|---|
| `fleetint` | `fleetint_1.5.0_amd64.deb` (sha256 `cd5136e1…`), checked against the release's `checksums.txt` and its signature by the release key `FE0C8B74…087199B3`; [`dsx-ai-factory/fleet-intelligence-agent`](https://github.com/dsx-ai-factory/fleet-intelligence-agent) `v1.5.0` (`a83abe9`), Apache-2.0 | the package's `fleetintd.service`, configured through its `/etc/default/fleetint`: `run --offline-mode --path=/var/lib/fleetint-offline --format=json`, collect and check every 15 s, attestation and inventory off; its local API on its default socket `/run/fleetint/fleetint.sock`. Its three Depends (`datacenter-gpu-manager-4-proprietary`, `nvattest`, `corelib`) are NVIDIA CUDA-repository packages for a host with a driver and are not installed (`dpkg --force-depends`): the binary needs libc only and dlopens NVML and DCGM |
| Mokka `nvml-mock` | image `ghcr.io/nvidia/nvml-mock:0.5.0@sha256:9583cc6e…` (k8s-test-infra `v0.5.0`, `e2950d58`), its base profile `h100.yaml` at that commit (sha256 `5121352b…`) | its node agent from the image, run as a systemd unit on the guest itself (`--host-root=/`) as the nvml-mock DaemonSet runs it on a node, with the profile rendered for `b01/server-01`: it stages the driver tree (`/var/lib/nvml-mock/driver`: the mock `libnvidia-ml.so.580.65.06`, `libcuda`, `nvidia-smi`, `/proc/driver/nvidia`, device nodes) and bind-mounts it on `/run/nvidia/driver`. Its own kernel-log announcement of an Xid (`--kernel-log`) is off |
| Debian 13 genericcloud (trixie, kernel 6.12, glibc 2.41) | the image `BASE` names (only read; a qcow2 overlay per run) | the fleetint guest: `qemu-system-x86_64 -accel kvm`, 2 vCPU, 3 GiB, QEMU user-mode networking with one loopback port forwarded to its ssh; cloud-init (NoCloud from the host's loopback) adds a user and nothing else |
| `dctwin up estate.yaml --adapters vm,inhost` | this repository | the estate (`compat/nvsentinel`'s, under its own name): `b01/server-01` at `vm`, `os: twin` (Alpine 3.24.2 netboot, x86_64, KVM, 2 vCPU, 1 GiB), its in-host agent |
| `dctwin-mokka render`, `dctwin-mokka sync … -- put-override.sh {node}` | this repository (`internal/mokka`) | the profile; and, while the twin runs, the guest's Mokka override document kept equal to the model (an atomic replace over ssh, then `nvml-mock-ctl status`) |
| `dctwin-kmsg` | `internal/inhost/kmod`: for the twin's guest built by `kmod/build.sh` (Alpine's headers, vermagic and symbol versions checked against the image's modules); in the fleetint guest built by `kmod/guest-load.sh` against `linux-headers-6.12.113+deb13-cloud-amd64` and loaded on every boot | a line written to `/proc/dctwin-kmsg` is printk'd: a kernel record with facility `kern` |
| `kmsg-relay.py` | this harness | **stand-in**: follows `state/inhost/<host>/kmsg.log` and writes each `NVRM:` record into the fleetint guest's `/proc/dctwin-kmsg` (its `/dev/kmsg` where that guest has no module), one write per record, `<level>text`, as the in-host agent's guest half writes it; logs where each went |

## A fault's two paths

```text
dctwin fault inject b01/server-01/gpu-2 nvidia/xid/xid-79
  ├ the in-host agent (internal/inhost) renders the catalogue's kernelLog lines and, over the XE9680
  │ guest's serial console, has its kernel module printk them there; that kernel's records come back to kmsg.log
  │   └ kmsg-relay.py (STAND-IN) → the fleetint guest's /proc/dctwin-kmsg → its kernel log (facility kern):
  │       <4>NVRM: GPU at PCI:0000:38:00: GPU-43febe06-…
  │       <4>NVRM: Xid (PCI:0000:38:00): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus.
  │         └ fleetint's Xid component (its /dev/kmsg watcher) → error_xid event, PCI:0000:38:00 → NVML's
  │           bus id 0000:38:00.0 → UUID GPU-43febe06-… → Unhealthy, REBOOT_SYSTEM
  └ dctwin-mokka sync (the shim effect nvidia/mokka, fault: fallen-off-bus) → overrides.yaml
      {devices: {"2": {failure: {mode: fallen_off_bus, xid: {code: 79}}}}} → every loaded mock library
      re-reads it within 1 s → NVML: GPU 2 lost (nvidia-smi "[GPU is lost]")
```

`fleetint` names the GPU of an Xid line from NVML: it turns `PCI:0000:38:00` into `0000:38:00.` and
finds the device whose bus id (go-nvlib's form, `0000:38:00.0`) starts with it. The driver's
`NVRM: GPU at PCI:…: GPU-<uuid>` line, which the catalogue also writes, is not read by it.

## Running it

```bash
BASE=/path/debian-13-genericcloud-amd64.qcow2 compat/fleet-intelligence/run.sh   # or make compat-fleet-intelligence BASE=…
KEEP=1 …          # leave both guests, the twin, the sync and the relay running
KEEP=wait …       # the same until $WORK/release exists, then tear down
MOKKA_DIR=/path … # an extracted nvml-mock image (bin/ and lib/ of its /usr/local), instead of extracting it here
HOST=b01/server-01 GPU79=gpu-2 GPU48=gpu-5 CPUS=2 MEM=3072 DISK=12G
```

Needs Linux with KVM, `qemu-system-x86_64`, `qemu-img`, OVMF and `swtpm` (the twin's vm adapter),
Go, python3, curl, ssh, Podman or Docker (to extract Mokka's image, unless `MOKKA_DIR`), and gpg to
check the release's signature (without it the package is checked against the pinned sha256 and
the release's `checksums.txt` only). It builds its own `dctwin` and `dctwin-mokka` into `$WORK/bin`.
The host's network and firewall are not touched. The guests hold about 4.5 GiB for the run's whole
life (under three minutes once the downloads are cached), and the overlay is deleted at the end.
Evidence is under `$WORK/evidence` (fleetint's API answers, `fleetint scan` and `machine-info`, its offline files, the
guest's journal and dmesg, the twin's events and fidelity report, the sync's and the relay's logs).

## Results

Run on 2026-10-20 on an x86_64 Ubuntu host under KVM, against `b01/server-01`, Xid 79 on `gpu-2` and
Xid 48 on `gpu-5`, with `dctwin-kmsg` in both guests. **23 checks: 18 PASS, 0 FAIL, 2 FINDING,
3 CANNOT**. The run before the module, on the same code otherwise, had C3 as CANNOT (17 PASS,
4 CANNOT); the first, before the fix below, failed M1.

| # | Check | Result | Observed |
|---|---|---|---|
| B1 | Mokka's NVML in the fleetint guest (`nvidia-smi`) reports the twin's GPUs | PASS | 8 × `NVIDIA H100 80GB HBM3`; UUID, serial, bus id and memory (81559 MiB) of each equal to the model, e.g. GPU 2 `GPU-43febe06-2a4e-483d-b95e-d8dc7af5ac96`, `3566696323257`, `00000000:38:00.0` |
| B2 | fleetint from its `.deb`, under its own unit, offline mode | PASS | `fleetint v1.5.0 (go go1.26.6, built 2026-09-08T20:40:09Z)`, `fleetintd` active, Debian 13, kernel 6.12.113, glibc 2.41; `checksums.txt` signed by the release key |
| B3 | `fleetint machine-info` lists the twin's GPUs | PASS | 8 GPUs; UUID, index, bus id (`0000:38:00.0`), serial equal to the model's for every one |
| B4 | its GPU product and memory | PASS | `NVIDIA-H100-80GB-HBM3` (the model's NVML name, D43), 85520809984 bytes (81559 MiB), architecture `hopper`, driver 580.65.06 (Mokka's) |
| B5 | its Xid and NVML components on the healthy twin | PASS | `accelerator-nvidia-error-xid` Healthy, "XIDComponent is healthy" (NVML loaded, so the Xid check is on); `accelerator-nvidia-nvml` Healthy, "no NVML collection errors reported" |
| B6 | its library component | PASS | Healthy, "all libraries exist" |
| X1 | `dctwin fault inject b01/server-01/gpu-2 nvidia/xid/xid-79`: the in-host agent has the lines printk'd in the twin's guest kernel | PASS | both recorded by that kernel 22 ms after (records 438, 439; level 4, facility 0, "printk'd by the in-host agent's kernel module") |
| X2 | **stand-in**: those records entered into the fleetint guest's kernel | PASS | through `/proc/dctwin-kmsg`: `<4>[143.934746] NVRM: Xid (PCI:0000:38:00): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus.` 519 ms after |
| X3 | the fault in the guest's NVML (Mokka override) | PASS | `{fallen_off_bus, xid 79}` on device 2 applied 15 ms after the twin committed the fault; `nvidia-smi`: GPU 2 `[GPU is lost]`, the others answer |
| X4 | fleetint's Xid component | PASS | 212 ms after the line: **Unhealthy**, `XID 79 ROBUST_CHANNEL_GPU_HAS_FALLEN_OFF_THE_BUS GPU has fallen off the bus detected on GPU PCI:0000:38:00 UUID:GPU-43febe06-2a4e-483d-b95e-d8dc7af5ac96`, suggested `REBOOT_SYSTEM` |
| X5 | its `error_xid` event | PASS | type `Fatal`, device `PCI:0000:38:00`, `raw_kmsg` the twin's line |
| X6 | its NVML component while the GPU is lost | FINDING | Healthy, last evaluated at the agent's start (below) |
| X7 | `fleetint scan` while the fault is active | PASS | "matched 1 xid errors from 539 kmsg(s)": 79, `PCI:0000:38:00`, GPU has fallen off the bus, `Fatal`, `REBOOT_SYSTEM, HARDWARE_INSPECTION` |
| M1 | Mokka's node agent restarts while the twin holds the Xid 79 (it clears the override document) | PASS | restored 1.6 s after the restart; GPU 2 lost again in NVML. **FAIL before the fix** (not restored in 40 s) |
| Y1 | `dctwin fault inject b01/server-01/gpu-5 nvidia/xid/xid-48` | PASS | lines recorded and relayed; `{ecc_uncorrectable, xid 48}` on device 5; `nvidia-smi -q -d ECC`: DRAM uncorrectable, volatile 4, aggregate 7 |
| Y2 | fleetint reports the Xid 48 | PASS | 178 ms after: `error_xid` `Fatal`, `XID 48 ROBUST_CHANNEL_GPU_ECC_DBE Double Bit ECC Error detected on GPU PCI:0000:68:00 UUID:GPU-fd463ddc-4905-4ce3-87c2-0e49c77ea825` (the model's `gpu-5`); the component's reason now names it |
| R1 | the twin clears both faults | PASS | the document empties 448 ms after; NVML answers for all 8 GPUs |
| R3 | fleetint's Xid component after the clear | FINDING | still Unhealthy (Xid 48, `REBOOT_SYSTEM`): see below |
| R4 | after the reboot fleetint asks for (the harness reboots the fleetint guest) | PASS | new boot ID after 32 s; fleetint records the reboot (its `os` component: "system reboot detected …") and its Xid component is Healthy, "XIDComponent is healthy", 356 ms after its API answers; NVML 8 GPUs |
| C1 | its DCGM components | CANNOT | "DCGM library is not loaded" (Healthy, unsupported): no DCGM installed, no host engine; `fleetint precheck` says so, and that `nvattest` is absent |
| C2 | enrollment, attestation, OTLP export | CANNOT | no backend ([Q9](../../docs/spec/11-decisions.md)); offline mode only |
| C3 | the kernel records have the driver's facility | PASS | facility `kern` (0) in both kernels: the twin's guest (module loaded) and the fleetint guest (`dctwin-kmsg` loaded for `6.12.113+deb13-cloud-amd64`); the printk is `dctwin-kmsg`'s, not `nvidia.ko`'s, which does not run; fleetint does not look at the facility |
| C4 | the server around the GPUs (CPU, memory, DMI, power) | CANNOT | the fleetint guest's: the host's `i5-10300H` × 2 vCPU, 2.8 GiB, QEMU's DMI; a twin power action reaches the XE9680's own guest, not this one |

The table is the harness's own output (`$WORK/results.md`), shortened here.

### Findings

| # | Finding | Whose |
|---|---|---|
| M1 | **The Mokka sync did not restore a document replaced behind it.** Mokka's node agent clears the override document every time it starts (a node pod restart, a host reboot). The Syncer's resync, documented as converging "a node whose document was replaced behind the twin's back", applied only documents that differed from its own last write, so after a node agent restart GPU 2 answered in NVML for as long as the model held it lost (until the model next changed). **Fixed** in `internal/mokka` (`sync.go`): the resync applies every host's document again; `Applied` is still told only of changes. `TestSyncerRestoresADocumentReplacedBehindIt` fails on the old code and passes now; M1 passes | the twin (fixed) |
| X6 | **fleetint's NVML component did not see the lost GPU.** It reports the errors of fleetint's machine-info collection, which its exporter refreshes at most every five minutes (`internal/exporter/collector/machine_info_provider.go`, `defaultMachineInfoStaleAfter`), longer than the fault lasted; and that collection reads identity (UUID, serial, PCI, VBIOS, name, index) of every GPU and memory of the first one that answers, while Mokka 0.5.0's `fallen_off_bus` mode fails only measurement getters (memory, temperature, power, clocks, ECC, utilisation), so a lost GPU still answers identity, where NVML on hardware answers `GPU_IS_LOST` | fleetint's refresh period; Mokka's failure coverage |
| R3 | **An Xid read from the kernel log is kept until a reboot.** With NVML healthy again fleetint's Xid component stays Unhealthy (`evolveHealthyState`: an Xid whose first suggested action is `REBOOT_SYSTEM` clears only at a `reboot` event; the component's `SetHealthy`, which would purge it, has no caller in fleetint 1.5.0's own code). It names the newest of equal severity, so after the Xid 48 the reason is the Xid 48, and both events stay listed. After the reboot it is Healthy at once | fleetint, by design |
| — | **Mokka's own Xid announcement would not be read by fleetint.** Mokka 0.5.0's `kernellog` writes `kernel: NVRM: Xid (PCI:0000:38:00): 79` (`internal/agent/kernellog/kmsg.go`), with no comma after the code; fleetint's pattern requires one (`…: (\d+),`). Probed with fleetint's own `xid.Match` at its tag, not run end to end: the twin's two lines match (79 and 48, both `Fatal`), Mokka's line and the catalogue's earlier form (`PCI:0000:38:00.0`) do not | Mokka (or fleetint's pattern) |
| — | **Mokka's ECC counts are per call, not the model's.** On the Xid 48 GPU NVML reports DRAM uncorrectable volatile 4, aggregate 7 after `nvidia-smi`'s queries (Mokka's `ecc_uncorrectable` mode accrues on each guarded call); the model's counter (`ecc.volatile.dbe`) is 1, and the override document carries no count | Mokka's override schema |
| — | **On a host, Mokka's node agent leaves a second mock library on the linker's path.** It stages from `/usr/local/lib/libnvidia-ml.so.*.*.*` (its image's path), which on a host is on the default linker path, where `ldconfig` links it; a process that loads that copy reads `/etc/nvml-mock/config.yaml` and an `overrides.yaml` beside it that nothing writes, so its GPUs never fail. `guest-setup.sh` puts the driver tree first (`ld.so.conf.d/00-…`) and refuses a setup in which `dlopen("libnvidia-ml.so.1")` maps anything else. fleetint's library check also wants `libnvidia-ml.so.1` and `libcuda.so.1` in the distribution's library directories, where they are linked | running Mokka outside its container (not a mode it documents) |

### What was a stand-in

- **The last hop of the kernel-log path** (`kmsg-relay.py`, X2, Y1): the twin renders the lines
  and has them printk'd in its own guest's kernel, and the relay enters the records that kernel
  recorded into the fleetint guest's kernel through the same module. The in-host agent does not reach
  the Debian guest: it follows only `os: twin` guests of the twin's vm adapter, over their serial
  consoles.
- **The reboot** (R4) is the harness's `systemctl reboot` of the fleetint guest, standing for the
  reboot fleetint suggests; the twin's model is not told (its XE9680 is the other guest), and the
  faults had been cleared in the twin before.

### What this run does not show

The DCGM components (no host engine), enrollment, attestation and the managed backend; InfiniBand
(Mokka's IB is off: it cannot carry the twin's port GUIDs); SXid (the twin's catalogue has no NVSwitch
kernel lines); that the NVIDIA driver printed the lines (they are the catalogue's, printk'd by
`dctwin-kmsg`); anything about the server beyond
its GPUs (CPU, memory, DMI, power state are the guest's); fleetint's machine-info view of a lost GPU
after its five-minute refresh (X6); and anything that executes on a GPU.

## From its source (read at `v1.5.0`)

[`dsx-ai-factory/fleet-intelligence-agent`](https://github.com/dsx-ai-factory/fleet-intelligence-agent)
(`NVIDIA/fleet-intelligence-agent` redirects there), **Apache-2.0**, Go: the node agent of NVIDIA
Fleet Intelligence, a managed fleet-health service NVIDIA announced in May 2026 and lists among DSX
OS's components. A fork of Lepton AI's `gpud` (vendored as `third_party/fleet-intelligence-sdk`).
Latest release **v1.5.0** (`a83abe9`, 2026-09-08); `v1.6.0-rc.1` is a pre-release. Release artefacts:
`.deb`/`.rpm` for amd64 and arm64, `fleetint-linux-{x86_64,sbsa}` tarballs,
`ghcr.io/dsx-ai-factory/fleet-intelligence-agent` images and a Helm chart; documentation at
<https://docs.nvidia.com/fleet-intel/agent>. It is publicly obtainable, so Q9's question is only
about its backend.

| It | Detail (v1.5.0) |
|---|---|
| Builds for | Linux only, with cgo (go-nvml, go-dcgm, go-sqlite3); `internal/nodeidentity` does not build for darwin. The released binary needs only libc |
| Offline mode | `fleetint run --offline-mode --path=<dir> --duration=HH:MM:SS --format=json\|csv` writes OTLP-shaped JSON every `FLEETINT_COLLECT_INTERVAL` (`fleetint_logs_*.json`: events and every component's health; `fleetint_metrics_*.json`); the local API (`/healthz`, `/machine-info`, `/v1/states`, `/v1/events`, `/v1/info`, `/metrics`) is served on `/run/fleetint/fleetint.sock` in either mode. `fleetint scan` runs every component's check once, reading the whole kernel log |
| Xid | read from **`/dev/kmsg` directly** (`pkg/kmsg`, as root; `KMSG_FILE_PATH` may name another device under `/dev/`), matched by `NVRM: Xid \(((?:PCI:)?[0-9a-fA-F:]+)\).*?: (\d+),`; the device is named from NVML's bus ids |
| Gating | the Xid component reports Healthy, "NVIDIA NVML library is not loaded", unless NVML loads **and** names a product (`NVMLExists() && ProductName() != ""`); the DCGM components need a DCGM host engine (`DCGM_URL`); InfiniBand reads `/sys/class/infiniband` |
| Backend | `fleetint enroll --endpoint … --token …` (`v1/agent/enroll`, node, nonce and attestation APIs; attestation through `nvattest`); OTLP export with `FLEETINT_COLLECTOR_ENDPOINT` |

Before this harness it could not run against the twin: its Xid source is the kernel log of the
machine it runs on, the only kernel carrying the twin's fault lines was an `os: twin` guest that
cannot host it, and without NVML its Xid check is off. The Debian guest with Mokka answers the
second; the relay is what remains of the first.

### A finding for the twin, from its parser

The twin's Xid lines (`packs/nvidia/faults/nvidia.yaml`) put the PCI function in the address
(`PCI:0000:38:00.0`); the NVIDIA driver prints domain, bus and device only (`PCI:0000:38:00`), and
`fleetint`'s pattern stops at the first character that is not a hex digit or a colon, so it did not
match the twin's line. **Fixed (2026-10-20)**: the catalogue's Xid 79 and Xid 48 entries write the
driver's two lines, `NVRM: GPU at PCI:{pcidev}: {uuid}` then `NVRM: Xid (PCI:{pcidev}): 79,
pid='<unknown>', name=<unknown>, …`, rendered by the in-host agent (`internal/inhost`). This run is
that fix's end-to-end check (X4, X5, X7, Y2).

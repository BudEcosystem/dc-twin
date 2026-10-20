# NVSentinel against the twin

NVIDIA's [NVSentinel](https://github.com/NVIDIA/NVSentinel) (Apache-2.0), built from the released
tag `v1.27.0` and run **unmodified**, detects an Xid 79 that the twin puts into the kernel log of a
Dell PowerEdge XE9680 (8 × H100) running as a real x86_64 virtual machine under KVM, quarantines
and drains the server's Node, and asks for a reboot, which goes through the server's Redfish BMC
(the `oem-dell/idrac` dialect); the twin observes the reboot, the guest comes back with a new boot
ID, and NVSentinel completes the remediation. The Xid line is **printk'd by the in-host agent's
kernel module**, a kernel record with facility `kern` as the driver's is
([D50](../../docs/spec/11-decisions.md)).

With `BASE` (a Debian 13 genericcloud image) the node also has its **GPUs**: a second guest in
which NVIDIA's Mokka presents the server's eight H100s to NVML as the twin's model holds them, and
NVSentinel's DCGM-based `gpu-health-monitor` watches them through DCGM 4.7.0's in-process host
engine. The same Xid 79 reaches that guest twice, as a driver's would: NVML loses the GPU, and the
kernel line is printk'd into that guest's kernel too, where DCGM's own kmsg reader looks for Xids.
The model keeps `gpu-2` dead across the warm reboot NVSentinel asks for (an Xid 79 clears only on
an AC cycle), so the run then watches what the monitors make of a node that comes back with a GPU
missing, cycles the server's power, and injects an Xid 48 to compare.

> **Impersonated** ([D24](../../docs/spec/11-decisions.md)). No kubelet runs in the twin's guest:
> the server's Node is registered and kept by the twin's in-host agent (`internal/inhost`), Ready
> while the guest's agent is alive, with the guest's own boot ID. What is not impersonated is the
> fault's path into NVSentinel: the Xid line is a record in the **guest kernel's own ring buffer**,
> read back from `/dev/kmsg` record by record, and NVSentinel's `syslog-health-monitor` (its
> production `systemd` build) reads it from a real journal file written by systemd's own
> `systemd-journal-remote`; the GPU guest's DCGM reads NVML and `/dev/kmsg` as on a GPU node. The
> run is a scale run in D24's terms, never a conformance pass.

## What runs

| Process | Source | Role |
|---|---|---|
| `dctwin up compat/nvsentinel/estate.yaml --adapters redfish,ipmi,dhcp,vm,inhost` | this repository | an HGX rack with one XE9680, `b01/server-01`, a QEMU machine (KVM; UEFI, SMBIOS from its identity, swtpm) booting the twin's `os: twin` guest (Alpine 3.24.2 netboot, x86_64) with the in-host agent and its kernel module; its iDRAC; the server's Node |
| `dctwin-kmsg.ko` | `internal/inhost/kmod`, built by `kmod/build.sh` in a throwaway Alpine container | the agent's kernel half: a line written to `/proc/dctwin-kmsg` is printk'd. Alpine keeps only a branch's newest `linux-virt-dev` (6.18.57), so it is built against those headers carrying the guest's release (6.18.52-0-virt) and kept only if its vermagic and every kernel symbol's version (CRC) equal those of the image's own modules |
| `etcd`, `kube-apiserver` 1.37.0 | envtest's binaries | the cluster, serving a certificate from the harness's CA (`lib/cluster.sh`) |
| `kube-controller-manager` 1.37.0 | `kubernetes/kubernetes` at `v1.37.0` (`KCM=build`, or a path) | node lifecycle, as in `compat/kai` |
| PostgreSQL 17 | this host's, else the official image (`dctwin-nvs-postgres`) | NVSentinel's datastore; NVSentinel's modules create its schema |
| `platform-connectors`, `syslog-health-monitor`, `fault-quarantine`, `node-drainer`, `fault-remediation`, `janitor`, `janitor-provider` (`CSP=label`) | `NVIDIA/NVSentinel` at `v1.27.0` (`3b46832`), `go build` per module; `syslog-health-monitor` with cgo and `-tags systemd` as its Dockerfile builds it | one container each (`dctwin-nvs-*`, from Debian bookworm with libsystemd, on this host's network, memory-capped), configured from the chart's own ConfigMaps (`helm template` at the tag with `values.yaml`) |
| `systemd-journal-remote` (systemd 252, Debian bookworm) | Debian | the node's journal: writes the journal file `syslog-health-monitor` reads, from the twin's export of the guest's kernel log |
| the GPU guest (`BASE`; `lib/gpu.sh`, `gpu-guest-setup.sh`) | Debian 13 genericcloud (kernel 6.12.113), QEMU/KVM, user-mode networking, prepared once | Mokka 0.5.0's node agent (from its released image) on a profile `dctwin-mokka render` writes from the model, kept equal to the model by `dctwin-mokka sync` (`put-override.sh`); `dctwin-kmsg` built there for its kernel (`kmod/guest-load.sh`); Docker |
| `gpu-health-monitor` | NVSentinel at the tag, built in the GPU guest by its own Dockerfile (on `nvcr.io/nvidia/cloud-native/dcgm:4.7.0-ubuntu24.04`, as its Makefile's `docker-build-dcgm4`) | the chart's DaemonSet command and arguments (`embedded-mode`: `--dcgm-mode local-managed`, an in-process DCGM host engine) and ConfigMap, privileged, with Mokka's driver tree mounted where the NVIDIA runtime class would put the driver's libraries; it publishes to the node's one `platform-connectors`, whose socket `run.sh` forwards into the guest's `/run/nvsentinel` over ssh |
| `kmsg-relay.py` (from `compat/fleet-intelligence`) | this repository | **stand-in, last hop only**: every NVRM record the twin's guest kernel recorded is entered into the GPU guest's kernel through its `dctwin-kmsg`, at the record's level |
| the lifecycle-manager stand-in (`run.sh`, `standin`) | this harness | does what NVSentinel's label provider asks of the site: a Redfish `ComputerSystem.Reset` (`ForceRestart`) of the server, at the target its system resource names, and the GPU guest's reset with it (QMP `system_reset`: the same machine); once both are back it restarts the node's monitors, as the kubelet restarts DaemonSet pods, and removes the label; logged to `evidence/standin.log` |

### Why containers, and what each one is given

NVSentinel's datastore client calls `rest.InClusterConfig()` on start in every module that
watches the store (`store-client/pkg/client/resume_token.go`), and `janitor-provider` knows no other
configuration, so those modules need `/var/run/secrets/kubernetes.io/serviceaccount` and
`KUBERNETES_SERVICE_HOST`; `syslog-health-monitor` needs Linux and libsystemd. Each module therefore
runs as its pod would, in a container of its own, given what the pod would mount: the in-cluster
ServiceAccount directory (the cluster's admin token and the harness's CA), the chart's ConfigMaps
as files, and the chart's arguments. The containers share this host's network, as pods with
`hostNetwork` would, so every module listens on a free port the harness gives it (the janitor's
admission webhook server keeps controller-runtime's `:9443`, which has no flag; nothing calls it).

### The node: its kernel log and its journal

```text
dctwin fault inject b01/server-01/gpu-2 nvidia/xid/xid-79
  └ engine: fault event, effects (kernelLog: "NVRM: GPU at PCI:{pcidev}: {uuid}", "NVRM: Xid (PCI:{pcidev}): 79, …")
      └ in-host agent (twin side, internal/inhost): renders the lines, sends each over the guest's serial console
          └ in-host agent (guest side, the twin's /init): writes "<4>NVRM: Xid (PCI:0000:38:00): 79, …" to /proc/dctwin-kmsg
              └ dctwin-kmsg: printk(KERN_WARNING …): a kernel record, facility kern (dmesg shows it; the console prints it)
                  └ agent: every /dev/kmsg record back over the console → state/inhost/<host>/{kmsg.log,journal.export}
                      └ systemd-journal-remote → /nvsentinel/var/log/journal/node.journal
                          └ syslog-health-monitor (_TRANSPORT=kernel) → platform-connectors (unix socket) → PostgreSQL, Node condition
```

**No journald runs in the twin's guest** (Alpine's netboot initramfs), so the mapping of a kernel
record to a journal entry is the in-host agent's, following journald's `dev_kmsg_record`
(`_TRANSPORT=kernel`, `PRIORITY`, `SYSLOG_FACILITY`, `SYSLOG_IDENTIFIER`, `_BOOT_ID`, …). The
fault's record is a printk, so its entry is the one journald makes of a driver's line
(`SYSLOG_FACILITY=0`, `SYSLOG_IDENTIFIER=kernel`, the message whole) with nothing added. Where the
image carries no module or the kernel refuses it (`KMOD=0` forces it), the agent writes the line
through `/dev/kmsg` instead, which the kernel marks facility `user` (`devkmsg_write`: "to be able to
reliably distinguish kernel-generated messages from userspace-injected ones"); journald would then
read the line's `NVRM:` as a syslog identifier, so that entry is entered as a driver's printk and
marked `DCTWIN_FAULT`, `DCTWIN_FAULT_ID` and `DCTWIN_KMSG_FACILITY=1`, and the fidelity report names
the path.

`syslog-health-monitor` reads the node's boot ID from `/proc/sys/kernel/random/boot_id` and detects
a reboot when its pod restarts and finds a different one. Its container shows it the guest's boot ID
there (a bind mount in the container's own mount namespace, hence `CAP_SYS_ADMIN`), and the
stand-in restarts it once the Node is back with a new boot ID, as the kubelet restarts a DaemonSet's
pod after its node reboots.

### The node's GPUs (`BASE`)

```text
the same fault
  ├ dctwin-mokka sync: GPU 2's override {mode: fallen_off_bus, xid: 79} → the GPU guest's Mokka driver tree
  │   └ Mokka's NVML: GPU 2 trips on its next guarded call (NVML_ERROR_GPU_IS_LOST), Xid 79 queued on the event set
  └ kmsg-relay.py (stand-in, last hop): the twin guest's NVRM records → the GPU guest's /proc/dctwin-kmsg → its kernel log
      └ gpu-health-monitor: DCGM 4.7.0 in process (NVML events, its /dev/kmsg reader, its health watches), polled every 15 s
          └ platform-connectors (the node's, through the forwarded socket) → PostgreSQL, Node condition GpuAllWatch
```

The GPU guest stands for the same machine as the twin's guest: every reset of the server (the
stand-in's Redfish reboot, the harness's, the AC cycle) resets it too, Mokka's node agent starts
again on boot, and the model's current GPU state is applied to it before the monitor's pod is
started again (`dctwin-mokka sync --once`).

## Running it

```bash
make build
DCTWIN=$PWD/bin/dctwin compat/nvsentinel/run.sh                  # PASS/FAIL table; artefacts under $WORK
BASE=/path/debian-13-genericcloud-amd64.qcow2 ...                # with the GPU guest (x86_64, KVM)
WORK=/tmp/nvs DCTWIN=$PWD/bin/dctwin compat/nvsentinel/run.sh    # reuse builds, the image, the prepared guest
KCM=/path/kube-controller-manager ...                            # skip building the controller-manager
KMOD=0 ...                                                       # without the kernel module (the /dev/kmsg path)
KEEP=1 ...                                                       # leave everything running to poke at
```

Needs a **Linux** host with KVM for a guest of its own architecture (the XE9680 is x86_64), Go,
git, python3, openssl, curl, a Docker engine or rootless Podman as `docker`, QEMU and envtest's
binaries; helm and the twin's guest image are fetched into `$WORK` when missing, and PostgreSQL runs
as a container when this host has none. The first run builds six modules here, the cgo monitor in a
throwaway container, the kernel module in another, and one image, `dctwin-nvsentinel:v1.27.0-<arch>`;
with `BASE` it also prepares the GPU guest once (`$WORK/guest/prepared.qcow2`: Docker, Mokka, the
module, and the 4.8 GB `gpu-health-monitor` image built from the tag; about 20 minutes), and every
run boots an overlay of it with 3 GiB. Every container carries a memory cap
(`POD_MEMORY`, default 384m). Containers, their network and volumes are `dctwin-nvs*` and are removed
at the end; the image and the prepared guest are kept for the next run. `$WORK/evidence` keeps the
rendered chart, every module's log, both guests' kernel logs, the journal entries, the health
events from the datastore, the Node at each step, the RebootNode, the twin's events and its
fidelity report.

## Result

Run on 2026-10-20 on an x86_64 Linux host (KVM, rootless Podman; every process tree in a
memory-capped scope): NVSentinel v1.27.0 `3b46832`, kube-apiserver and kube-controller-manager
1.37.0, PostgreSQL 17.6; the XE9680's guest Alpine 3.24.2 (`6.18.52-0-virt`, 2 vCPUs, 1 GiB) with
`dctwin-kmsg` loaded; the GPU guest Debian 13 (`6.12.113+deb13-cloud-amd64`, 4 vCPUs, 3 GiB) with
Mokka 0.5.0 (driver 580.65.06 as the profile renders it) and `dctwin-kmsg`; `gpu-health-monitor`
on DCGM 4.7.0.

| # | Check | Observed | Result |
|---|---|---|---|
| U1 | The server's guest boots; the in-host agent registers its Node | Ready, boot ID the guest's own (`e52de74e…`), annotated `dc-twin.io/impersonated=inhost`, kernel `6.18.52-0-virt` amd64, 3.9 s after the cluster saw the twin | PASS |
| K1 | The in-host agent's kernel module in the twin's guest | built against `linux-virt-dev` 6.18.57 with the guest's release; vermagic `6.18.52-0-virt SMP preempt mod_unload modversions` and all 11 symbol versions equal to the image's own modules'; the agent: `kernel module loaded` | PASS |
| G0 | The GPU guest | Debian 13, `dctwin-kmsg` built there and loaded; Mokka's node agent active on the rendered profile; `dlopen("libnvidia-ml.so.1")` maps the driver tree's `libnvidia-ml.so.580.65.06` | PASS |
| G1 | Its NVML presents the twin's GPUs | 8 GPUs, every UUID and PCI bus ID the model's (`GPU-43febe06-…` at `00000000:38:00.0` is `gpu-2`) | PASS |
| U2 | The seven NVSentinel modules, the node's journal, the GPU guest's monitor | 7 of 7, `systemd-journal-remote` following the twin's export, `gpu-health-monitor` running | PASS |
| U3 | PostgreSQL schema, made by NVSentinel's own modules | 4 tables | PASS |
| U4 | syslog-health-monitor's first pass (it starts a fresh node's journal at the tail) | done 3.4 s after start | PASS |
| G2 | gpu-health-monitor on DCGM sees the GPUs healthy | DCGM's group `[0, 1, 2, 3, 4, 5, 6, 7]`; a healthy event for each GPU on each of its 13 watches (`GpuAllWatch`, `GpuPcieWatch`, `GpuMemWatch`, …), through the node's platform-connectors, 16 s after start | PASS |
| X1 | `dctwin fault inject b01/server-01/gpu-2 nvidia/xid/xid-79` | both catalogue lines printk'd through `/proc/dctwin-kmsg`: records 438 and 439, **level 4, facility 0**, `NVRM: Xid (PCI:0000:38:00): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus.`, 25 ms | PASS |
| X2 | The line in the node's journal | `_TRANSPORT=kernel`, `PRIORITY=4`, `SYSLOG_FACILITY=0`, `SYSLOG_IDENTIFIER=kernel`, `_BOOT_ID` the guest's, the message whole; no `DCTWIN_*` field, 62 ms | PASS |
| G3 | **Stand-in** (last hop): the records entered into the GPU guest's kernel | both NVRM records through its `/proc/dctwin-kmsg`, `<4>` (kern, warning), 125 ms | PASS |
| G4 | gpu-health-monitor raises a DCGM health event for the Xid 79 | no Xid incident (`DCGM_FR_FALLEN_OFF_BUS`) in 60 s: DCGM had the Xid twice and kept neither (see the finding). What it raised instead, 14 s after the fault, was `GpuPowerWatch` `DCGM_FR_POWER_UNREADABLE` on GPU 2 (fatal, `RESTART_VM`), from power samples that came back blank before DCGM marked the GPU lost, and 15 s later the same watch healthy again; DCGM lists 7 active GPUs and refuses GPU 2 (`GPU is lost while adding GPU 2 to our entity group`). In the run before this one the power incident did not come at all | **FINDING** |
| D1 | syslog-health-monitor reports it | `SysLogsXIDError`, error code 79, fatal, `RESTART_BM`, entities `PCI 0000:38:00` and `GPU_UUID GPU-43febe06-…` (from the catalogue's `NVRM: GPU at PCI:…` line), 13 s after the line (its 15 s poll) | PASS |
| D2 | platform-connectors' Kubernetes connector | `SysLogsXIDError=True` (`ErrorCode:79 PCI:0000:38:00 GPU_UUID:GPU-43febe06-…`); `GpuAllWatch=False` | PASS |
| Q1 | fault-quarantine | cordoned in the event's second (14:10:38.391), `cordon-by=NVSentinel`, `cordon-reason=Syslog-fatal-error-ruleset`, `quarantineHealthEvent` | PASS |
| N1 | node-drainer | `training-0` evicted (Immediate), Terminating with no kubelet to confirm it, counted as drained past its grace; `drain-succeeded` | PASS |
| R1 | fault-remediation | a `RebootNode` (`maintenance-b01-server-01-…`) | PASS |
| R2 | janitor through janitor-provider (`label`) | `SignalSent=True`; `nke.nvidia.com/reboot=requested-by-nvsentinel`; the stand-in POSTs `ForceRestart` to `/redfish/v1/Systems/System.Embedded.1/Actions/ComputerSystem.Reset` | PASS |
| R3 | The reboot, as the twin sees it | the Redfish `power.restart` (source: stack), QEMU's `RESET`, the guest's new boot `5163fb8d…` 4.4 s later; the GPU guest reset with it | PASS |
| R4 | janitor completes the RebootNode | `NodeReady=True` 28 s later (it checks every 60 s) | PASS |
| C1 | The restarted syslog-health-monitor clears its check | `SysLogsXIDError=False` ("No Health Failures"); with no other check holding it, uncordoned (`uncordon-by=NVSentinel`) | PASS |
| C2 | After NVSentinel's warm reboot `gpu-2` is still dead in the model (Xid 79 holds until an AC cycle): gpu-health-monitor (DCGM reading Mokka's NVML from the model) should see it unhealthy, and the node stay or be re-quarantined | the model's state reached the rebooted node's NVML: DCGM lists **7 active GPUs**, `gpu-2` lost. The restarted monitor's group is `[0, 1, 2, 3, 4, 5, 6, 7]` (DCGM enumerated GPU 2 before its first failing call), and in 180 s it published **healthy** events for every GPU on every watch, 13 of them for `gpu-2`, and no unhealthy one; the node stays uncordoned, one RebootNode in all. Run twice, the same both times | **FINDING** |
| A1 | An AC cycle (`dctwin power ac-cycle`) brings the GPU back | Xid 79 cleared by `device.acCycle`; the server boots (`c27ddcff…`), the GPU guest with it; DCGM lists 8 active GPUs; healthy events for all 8; uncordoned, `SysLogsXIDError=False`, `GpuAllWatch=False` | PASS |
| C3 | An Xid 48 logged after a reboot, before the monitor's pod starts again | the line is in the new boot's kernel log and journal; the post-reboot scan of the current boot reports nothing in 90 s (12 boot-filtered scans finished) | **FINDING** |
| G5 | gpu-health-monitor raises a DCGM health event for the Xid 48 (`gpu-5`) | `GpuMemWatch` on GPU 5: `DCGM_FR_VOLATILE_DBE_DETECTED` ("Detected 5 volatile double-bit ECC error(s)") and `DCGM_FR_ROW_REMAP_FAILURE`, both fatal, `RESTART_VM` (the chart's `COMPONENT_RESET`, overridden because no metadata collector gave the GPU's UUID); `DCGM_FR_XID_ERROR` (Xid 48), not fatal (`NONE`) | PASS |
| Q2 | fault-quarantine acts on it as before | cordoned on the two fatal events, and a second remediation began | PASS |

26 checks: 23 PASS, 0 FAIL, 3 FINDING, 0 CANNOT, the same in the run before it (whose only difference
is G4's power incident). The in-host agent's own end-to-end test
(`TestInHostAgent`, KVM) passed with the module (`kernel module: loaded`, 2 lines printk'd, 0 marked)
and without it (`absent`, 2 lines through `/dev/kmsg`, 2 marked).

### Finding: the post-reboot scan skips every entry of the current boot

syslog-health-monitor scans the current boot's journal from its start after a reboot, "so entries
emitted between boot and monitor startup are not missed" (`handleBootIDChange`). Each entry is first
checked by `isStaleBootEntry`, which compares `journal.GetData("_BOOT_ID")` with the boot ID it
read from `/proc/sys/kernel/random/boot_id`. With the production journal (`-tags systemd`,
go-systemd's `sdjournal`), `GetData` returns the field as `FIELD=value`, and the journal writes boot
IDs without dashes, so the comparison is `_BOOT_ID=ad654181b459441b81834e773c033c27` against
`ad654181-b459-441b-8183-4e773c033c27`: every entry is stale and skipped. C3 shows it: the Xid 48
the twin wrote into the new boot is in the journal, the scans end at the journal's last entry
(`Finished processing boot-filtered journal entries`), and no Xid 48 is parsed or reported.
NVSentinel's unit tests use a mock journal that returns bare, matching values
(`TestIsStaleBootEntry`), which is why they pass. The same `GetData` is why every event's message
starts `MESSAGE=` (D1). Comparing `GetDataValue("_BOOT_ID")` with the boot ID with its dashes removed
would fix both readings. Entries written after the monitor starts are unaffected (D1).

### Finding: an Xid 79 does not reach DCGM's health watch

DCGM 4.7.0, as the monitor runs it, gets an Xid two ways, and on this node it lost both for the
Xid 79 (G4). What follows is from DCGM's own debug log, in a run of its `nv-hostengine` from the
monitor's image against the same guest, Mokka and kernel line (the monitor's in-process engine
does not log at debug level):

- **NVML's event.** Mokka queues Xid 79 on the event set when GPU 2 trips, as NVML delivers it, and
  DCGM's event thread receives it, then asks `nvmlDeviceGetIndex` for the event's device, which
  answers `NVML_ERROR_GPU_IS_LOST`, as `nvml.h` documents for a GPU that has fallen off the bus;
  DCGM logs `Unable to convert device handle to index` and drops the event. Every later wait returns
  `GPU_IS_LOST` (`Got st 15 from nvmlEventSetWait`), and `UpdateLostGpus` marks GPU 2 lost.
- **Its own kmsg reader** (`DcgmKmsgReader`, Xids 79, 119 and 120 by default) parses the twin's
  line from the kernel log, facility `kern` as `dctwin-kmsg` printk'd it: `XID 79 detected on GPU
  2(0000:38:00.0) in /dev/kmsg`. `EventThreadMain` records it into that loop's field-value buffer,
  but forwards the buffer to subscribers (the health module among them) only after an NVML event it
  could attribute; on the usual timeout, or the lost GPU's error, it moves to the next loop, whose
  `threadCtx.Clear()` empties the buffer (DCGM v4.6.1's `DcgmCacheManager.cpp`, which 4.7.0's
  behaviour matches). The health module learns of Xids only through that callback.

So `dcgmi health` reports Healthy, the monitor raises nothing, and an Xid 79 reaches NVSentinel only
through the syslog path. The same host engine reported an Xid 48 at once (`Detected XID 48 for GPU
5`, with the DBE counters, G5), because NVML's event for a GPU that is not lost converts to an index.

### Finding: a GPU still lost after the warm reboot is reported healthy

The twin keeps `gpu-2` dead after the reboot, and that state does reach the node: Mokka fails the
GPU's NVML calls with `GPU_IS_LOST`, and DCGM lists 7 active GPUs. What is missing is a health
signal for a lost GPU, in DCGM and in NVSentinel: DCGM's health watches raise no incident for a GPU
whose status is lost (no Xid reached them, G4), and `gpu-health-monitor` publishes a healthy event
for every GPU of its group with no incident, so it reported `gpu-2` healthy on all 13 watches, and
NVSentinel had nothing that held the quarantine. Two details are the twin's to say: Mokka fails a
lost GPU from its first guarded call, so DCGM enumerated GPU 2 before marking it lost (a driver that
never brings the GPU up would leave it out of the group, and the monitor would publish nothing for
it, with the same outcome); and the NVIDIA driver also logs, when it probes a GPU that reads all
ones, `NVRM: The NVIDIA GPU 0000:38:00.0 (PCI ID: 10de:2330) installed in this system has fallen
off the bus and is not responding to commands` (open-gpu-kernel-modules, `rm_is_supported_device`),
which NVSentinel's `SysLogsGPUFallenOff` check exists for. Since this run the twin
writes it: Xid 79's catalogue entry carries the line as a `when: boot` kernel line, which the in-host
agent printk's at every boot while the fault holds (`internal/inhost`'s end-to-end test checks it
once per boot, and not before the first reboot). It has not been run against NVSentinel yet, and
the post-reboot scan above (C3) would skip it, since the driver logs it before the monitor's pod
starts.

## The Xid 79, end to end

From the run above (`$WORK/evidence`; times UTC, `#n` the twin's event numbers):

| Time | Who | What |
|---|---|---|
| 14:10:24 | gpu-health-monitor | healthy events for GPUs 0–7 on every watch (G2) |
| 14:10:25.403 | `dctwin` | `#23` fault `inject nvidia/xid/xid-79` on `b01/server-01/gpu-2`; `#26` `gpu-2` Critical; `#25` the iDRAC's SEL |
| 14:10:25.403 | in-host agent | `#30`, `#31` `kernelLog → written` to the agent's kernel module (`/proc/dctwin-kmsg`); `#32`, `#33` `→ recorded`: the guest kernel's records 438 and 439, level 4, **facility 0** |
| 14:10:25.598 | `dctwin-mokka sync` | GPU 2's override `{mode: fallen_off_bus, xid: {code: 79}}` in the GPU guest's driver tree |
| 14:10:25.823 | `kmsg-relay.py` | both records into the GPU guest's `/proc/dctwin-kmsg`: `<4>NVRM: Xid (PCI:0000:38:00): 79, …` in its kernel log, where DCGM's kmsg reader parses it; no Xid incident follows (G4) |
| 14:10:38 | syslog-health-monitor | its next 15 s pass: `SysLogsXIDError`, 79, fatal, `RESTART_BM`, through the platform-connectors socket; the Node condition |
| 14:10:38.391 | fault-quarantine | `Syslog fatal error ruleset`: the Node cordoned, `quarantineHealthEvent` |
| 14:10:39, 14:10:54 | gpu-health-monitor | `GpuPowerWatch` on GPU 2: `DCGM_FR_POWER_UNREADABLE`, fatal; one poll later healthy again (DCGM has marked the GPU lost) |
| 14:10:38–14:13:08 | node-drainer, fault-remediation | `training-0` evicted (Immediate), Terminating; `drain-succeeded`; a `RebootNode` |
| 14:13:08 | janitor, stand-in | `nke.nvidia.com/reboot` on the Node; `POST …/ComputerSystem.Reset {"ResetType":"ForceRestart"}` to the iDRAC: `204`; the GPU guest's QMP `system_reset` |
| 14:13:09.103 | the twin | `#38` request `power.restart redfish` (source: stack); `#39` QEMU `reset → host-qmp-system-reset`; `#41` agent detached |
| 14:13:13.403 | the twin | `#44` agent attached, boot `5163fb8d…`; the Node Ready with the new boot ID |
| 14:13:39 | stand-in | the GPU guest back, the model's GPU state applied to Mokka (`gpu-2` still lost); the node's monitors restarted; the label removed |
| 14:13:40.148 | syslog-health-monitor, fault-quarantine | healthy events for the new boot; `All health checks recovered`: uncordoned |
| 14:14:08 | janitor | `Node reached ready state post-reboot`: `RebootNode NodeReady=True` |
| 14:14:10 | gpu-health-monitor | its new DCGM lists 7 active GPUs; the monitor publishes healthy events for GPUs 0–7, `gpu-2` included (C2) |
| 14:17:10.703 | the harness, the twin | `#46` `ac.cycle`: the server off, the BMC unavailable; `#55` (14:17:15.503) `clear nvidia/xid/xid-79 (device.acCycle)`; `#58` the machine started again; the GPU guest reset; Mokka's document empty |
| 14:18:10 | gpu-health-monitor | 8 active GPUs in DCGM, healthy events for all (A1) |

## What did not run, and why

| NVSentinel part | Why not |
|---|---|
| `metadata-collector`, `labeler` | `labeler` reads the GPU Operator's DCGM and driver pods, which do not run; `metadata-collector` (GPU UUIDs and PCI addresses from NVML, written to `gpu_metadata.json` on the node) could run in the GPU guest and is not yet wired: without its file the DCGM events name the GPU by index only, and `COMPONENT_RESET` becomes `RESTART_VM` (G5) |
| `health-events-analyzer`, `csp-health-monitor`, `nic-health-monitor`, `kubernetes-object-monitor`, preflight | not on the Xid path (the NIC monitor reads `/sys/class/infiniband`, Q16) |
| node-binding authentication on the platform-connectors socket, janitor-provider TLS and token audience | they need projected ServiceAccount tokens and cert-manager; turned off (`values.yaml`, `run.sh`) |

## What this does not show

- That anything runs on the tray: no kubelet, no container runtime; the workload pod is bound and
  never started, and the Node is the twin's (D24).
- The NVIDIA driver's behaviour: the lines are the fault catalogue's, printk'd by `dctwin-kmsg`, not
  by `nvidia.ko`; the GPUs are Mokka's NVML, and DCGM sees what Mokka answers (a lost GPU fails from
  its first guarded call, identity calls included).
- That one machine has one kernel: the GPU guest is a second kernel standing for the same server,
  reset with it; the relay is a labelled stand-in for that last hop.
- That the reboot was the right repair: the twin keeps an Xid 79 across a warm reboot (an AC cycle
  clears it), and NVSentinel lifted the quarantine on the reboot (C2).
- Anything about NVSentinel's scale, its MongoDB backend, its circuit breaker (off: one Node), or
  the deployment-mode platform connector.

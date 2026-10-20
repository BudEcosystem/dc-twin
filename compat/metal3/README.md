# Metal³ against the twin

Metal³ is bare-metal management for Kubernetes: its **bare-metal operator** (BMO) reconciles a
`BareMetalHost` per machine (a BMC address, a credentials Secret, `online`) and drives
[OpenStack Ironic](../ironic/) to register, inspect, power, provision and deprovision it. Cluster
API's Metal³ provider consumes the hosts BMO makes `available`. This harness runs **BMO itself,
built from a released tag and unmodified**, on a real kube-apiserver, in front of a real Ironic
configured the way Metal³'s own `ironic-image` configures it, and gives it BareMetalHosts for the
twin's BMCs over Redfish and IPMI. Everything is driven the way a Metal³ user drives it, through
Kubernetes objects; each effect is then checked in the twin's model and event log, in what Ironic
recorded (read-only) and in BMO's status and the objects it creates.

| | |
|---|---|
| BMO | `metal3-io/baremetal-operator` **v0.14.2** (commit `f3d404e`, released 2026-10-15), built from the tag: the manager binary and, since v0.14, the Ironic provisioner it loads as a Go plugin (`pkg/provisioner/ironic/plugin` → `ironic-provisioner.so`), from one toolchain as the project's Makefile builds them. Run with `-provisioner ironic -webhook-port 0` |
| Ironic | **39.0.0** (2026.2), the Ironic Metal³'s `ironic-image` **v39.0.0** ships, from PyPI with OpenStack's 2026.2 constraints, one process (API in the conductor), SQLite, no auth, no message bus. [ironic.conf.in](ironic.conf.in) is `ironic-image`'s `ironic.conf.j2` rendered with that image's defaults: hardware types `ipmi, idrac, redfish, …`, every interface list as the image enables it, `[ipmi]` as the image sets it, `[conductor]verify_step_priority_override = management.clear_job_queue:90`, and Ironic's own defaults where the image keeps them (`force_power_state_during_sync = True`, `power_state_sync_max_retries = 3`). What the harness changes is marked `HARNESS` there: the periodic intervals (power sync every 10 s, not 60) and one Redfish connection attempt per call |
| Kubernetes | kube-apiserver and etcd from envtest 1.37, as processes, with the real kube-controller-manager v1.37.0 when `KCM` is given (garbage collection of the objects BMO owns). Every CRD in BMO's `config/base/crds/bases` at the tag. No kubelet, no webhook server (`-webhook-port 0`) |
| Twin | `examples/estates/lab-model.yaml`, adapters `redfish, ipmi, dhcp` |
| Redfish BMCs | at each BMC's **leased site address**, reached through the twin's site gateway (Ironic runs with `HTTPS_PROXY` set to it, as `compat/ironic`), `disableCertificateVerification: true` |
| IPMI BMCs | at the BMC's loopback UDP port (`ipmi://127.0.0.1:<port>`), through `ipmitool -I lanplus`, as Ironic's `ipmi` hardware type runs it |

The hosts (`flow.py`, `HOSTS`):

| BareMetalHost | Twin device | `bmc.address` | `inspectionMode` | What it exercises |
|---|---|---|---|---|
| `generic-c02` | c02/server-01, generated DMTF tree | `redfish://<lease>/redfish/v1/Systems/1` | `fast` | the whole lifecycle; BMC loss |
| `dell-b01` | b01/server-01, Dell XE9680 (captured iDRAC) | `idrac-redfish://…/System.Embedded.1` | `disabled` (+ `bootMACAddress`) | Ironic's `idrac` hardware type (`idrac-redfish` power, management, BIOS, RAID, vendor); BMC loss |
| `smc-b02` | b02/server-01, Supermicro SYS-821GE-TNHR (captured) | `redfish://…/Systems/1` | `fast` | |
| `xcc-b05` | b05/server-01, Lenovo SR670 V2 (captured XCC) | `redfish://…/Systems/1` | `fast` | |
| `ilo-c01` | c01/server-02, HPE DL385 Gen10 Plus (captured iLO 5) | `ilo5-redfish://…/Systems/1` | `fast` | |
| `gb300-a01` | a01/compute-tray-01, NVIDIA GB300 tray (captured) | `redfish://…/Systems/System_0` | `fast` | |
| `dell-c01-ext` | c01/server-01, Dell R760 (captured iDRAC) | `redfish://…/System.Embedded.1` | `fast`, `externallyProvisioned: true` | adoption, the reboot annotation, BMC loss |
| `generic-c02-ipmi` | c02/server-02 | `ipmi://127.0.0.1:<port>` | `disabled` (+ `bootMACAddress`) | power over IPMI, BMC loss, deletion |
| `dell-b01-ipmi` | b01/server-02 | `ipmi://127.0.0.1:<port>` | `agent` | how far agent (in-band) inspection gets on a `model` host |
| `smc-b02-creds` | b02/server-02 | `redfish://…/Systems/1` | `fast` | created with a wrong password, then corrected |
| `dell-b01-oob` | b01/server-01 again | `idrac-redfish://…` | `fast` | a BMO/Ironic mismatch (below); no request reaches the BMC |

Every host has `automatedCleaningMode: disabled` and `bootMode: UEFI`; credentials Secrets are
`dctwin credentials` for the device.

## Running it

```bash
make build
KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.37.0) DCTWIN=$PWD/bin/dctwin compat/metal3/run.sh
KCM=/path/to/kube-controller-manager IRONIC_VENV=/tmp/ironic/venv WORK=/tmp/m3 compat/metal3/run.sh
HOLD=1 WORK=/tmp/m3 compat/metal3/run.sh   # start everything and keep it up; $WORK/env.sh says where
```

`run.sh` clones BMO at the tag and checks the commit, builds the operator and its plugin (about
four minutes; `BMO_BIN` and `BMO_PLUGIN_DIR` reuse a pair built together), makes Ironic's venv
unless `IRONIC_VENV` names one (`compat/ironic/run.sh` makes the same), starts etcd,
kube-apiserver (and kube-controller-manager) on kernel-chosen loopback ports, applies BMO's CRDs,
brings up the twin, serves Ironic's HTTP root (placeholder deploy kernel and ramdisk: no host here
can run IPA) and starts Ironic and BMO, runs [flow.py](flow.py), prints the table and stops
everything it started. BMO and Ironic run with `HOME` under `$WORK`; the only kubeconfig any
process is given is the scratch cluster's.

## What each check does

| Check | Metal³ | Checked in the twin and in Ironic |
|---|---|---|
| `register` | Secret + BareMetalHost; BMO creates the Ironic node and moves it to manageable | the node's driver (`redfish`, `idrac`, `ipmi`) and `driver_info` address are what the BMH says; the BMC saw requests from the Secret's user only |
| `credentials.bad` / `.fixed` | a wrong password, then the Secret corrected | the BMC answered 401; BMO reports `registration error` with Ironic's message, then registers |
| `inspect` | `fast`: Ironic's out-of-band `redfish` inspection → `HardwareData` | RAM = `MemorySummary`; NIC MACs = the system's `EthernetInterfaces`; manufacturer, model and serial = the ComputerSystem. `agent`: see CANNOT |
| `firmware.settings` | BMO's `HostFirmwareSettings` and `FirmwareSchema` | every BIOS attribute the BMC serves (less the ones BMO drops by name, `*Password*`) with the BMC's value |
| `firmware.components` | `HostFirmwareComponents` | BIOS and BMC versions = what the BMC serves and what the model holds |
| `power.off` / `power.on` | `spec.online: false` / `true` | BMO's soft power-off is `power.off.graceful` (Redfish `GracefulShutdown`, IPMI soft off); the model reaches Off / On; `status.poweredOn` follows |
| `reboot` | annotation `reboot.metal3.io` on the externally provisioned host | power off then on in the twin's log; BMO removes the annotation |
| `twin.off` | none: `dctwin power off` underneath an `online: true` host | Ironic's power sync (`force_power_state_during_sync`, Metal³'s default) powers it back on over the BMH's protocol; the BMH stays `poweredOn: true` |
| `health.critical` / `.ok` | none: a failed PSU (`generic/power/psu-failure`) | `Status.Health: Critical` → Ironic node `health` → the BMH condition `Healthy=False` (`CriticalError`), and back |
| `bmc.lost` / `.back` | none: `generic/bmc/unreachable` | Ironic: maintenance, fault `power failure`, after its sync retries; what BMO shows meanwhile; recovery |
| `delete` | `kubectl delete bmh` | BMO powers the host off (`poweringOffBeforeDelete`), deletes the Ironic node, the BMH goes; with KCM the HardwareData, HostFirmwareSettings and HostFirmwareComponents are collected |

## Results

Run 2026-10-20 on the lab machine (x86_64 Ubuntu 26.04, Go 1.27, Python 3.14, envtest 1.37 with
kube-controller-manager v1.37.0, ipmitool 1.8.19), after the twin fix below, with hosts that spend
their profile's POST time before booting; 807 s (an earlier run without the POST time gave the same
table in 922 s):

```
| check               | generic-c02 | dell-b01 | smc-b02 | xcc-b05 | ilo-c01 | gb300-a01 | dell-c01-ext | generic-c02-ipmi | dell-b01-ipmi | smc-b02-creds | dell-b01-oob |
|---------------------|-------------|----------|---------|---------|---------|-----------|--------------|------------------|---------------|---------------|--------------|
| register            | PASS        | PASS     | PASS    | PASS    | PASS    | PASS      | PASS         | PASS             | PASS          | BLOCK         | FINDING      |
| credentials.bad     | -           | -        | -       | -       | -       | -         | -            | -                | -             | PASS          | -            |
| credentials.fixed   | -           | -        | -       | -       | -       | -         | -            | -                | -             | PASS          | -            |
| inspect             | PASS        | PASS     | PASS    | PASS    | PASS    | XFAIL     | PASS         | PASS             | CANNOT        | PASS          | -            |
| firmware.settings   | PASS        | PASS     | PASS    | PASS    | PASS    | PASS      | PASS         | -                | -             | PASS          | -            |
| firmware.components | PASS        | PASS     | PASS    | PASS    | PASS    | PASS      | PASS         | -                | -             | PASS          | -            |
| power.off           | PASS        | PASS     | PASS    | PASS    | PASS    | BLOCK     | -            | PASS             | BLOCK         | PASS          | -            |
| power.on            | PASS        | PASS     | PASS    | PASS    | PASS    | BLOCK     | -            | PASS             | BLOCK         | PASS          | -            |
| reboot              | -           | -        | -       | -       | -       | -         | PASS         | -                | -             | -             | -            |
| twin.off            | PASS        | PASS     | PASS    | PASS    | PASS    | BLOCK     | PASS         | PASS             | BLOCK         | PASS          | -            |
| health.critical     | PASS        | PASS     | PASS    | PASS    | PASS    | BLOCK     | PASS         | -                | -             | PASS          | -            |
| health.ok           | PASS        | PASS     | PASS    | PASS    | PASS    | BLOCK     | PASS         | -                | -             | PASS          | -            |
| bmc.lost            | PASS        | PASS     | -       | -       | -       | -         | PASS         | PASS             | -             | -             | -            |
| bmc.back            | PASS        | PASS     | -       | -       | -       | -         | PASS         | PASS             | -             | -             | -            |
| delete              | -           | -        | -       | -       | -       | -         | -            | PASS             | PASS          | PASS          | -            |

83 PASS, 0 FAIL, 1 XFAIL, 1 CANNOT, 1 FINDING, 9 BLOCK
```

**83 PASS, 0 FAIL.** The BLOCKs follow from the XFAIL and the CANNOT (BMO manages no power for a
host left in an inspection error) and from `smc-b02-creds`, whose registration is the two
`credentials.*` rows. Earlier runs on macOS (arm64) gave the same verdicts once the harness's own
comparisons were settled; the first of them found twin bug 1 below (`dell-b01-ipmi` could not start
inspection).

- **XFAIL `gb300-a01 inspect`**: Ironic's out-of-band inspection raises `MissingAttributeError:
  The attribute EthernetInterfaces is missing from the resource /redfish/v1/Systems/System_0`, as on
  the captured tray (`compat/ironic` found the same); BMO reports `inspection error` and keeps the
  host in `inspecting`. Its BIOS settings (403) and BMC firmware still reach HostFirmwareSettings
  and HostFirmwareComponents.
- **CANNOT `dell-b01-ipmi inspect`** (agent inspection): Ironic caches the deploy kernel and
  ramdisk, sets a one-time UEFI PXE boot and powers the host on over IPMI (twin:
  `boot.override … parameter 5: Pxe Once UEFI`, `power.off.force`, `power.on`); the twin's boot walk
  for the `model` host ends at `dhcp (no boot server offered)` because nothing serves PXE on the
  provisioning network here; no IPA calls back; after `inspect_wait_timeout` Ironic records
  `inspect failed` ("Timeout reached while inspecting the node…") and BMO `inspection error`,
  and retries. Everything up to the boot is what a real host's BMC sees.
- **FINDING `dell-b01-oob register`**: below (BMO and Ironic, not the twin).

What the notes record (`$WORK/flow.txt`):

| Host | Inspection (HardwareData) | HostFirmwareSettings | HostFirmwareComponents |
|---|---|---|---|
| generic DMTF | 1 TiB, x86_64 ×128, 2 NICs, 2 disks, `dc-twin` / Generic 2U Server | 9 = the BMC's 9 | bios 2.4.1, bmc 1.12.0 |
| Dell XE9680 (idrac) | inspection disabled; port from `bootMACAddress` | 448 = 455 less 7 `*Password*` | bios 1.8.2, bmc 6.00.30.00, 4 NICs |
| Supermicro | 2 TiB, Xeon 8462Y+ ×128, 11 NICs, 8 disks | 367 = 379 less 12 `*Password*` (nulls as "") | bios 1.4, bmc 11.01.01, 9 NICs |
| Lenovo XCC | 1 TiB, Xeon 8362 ×128, 5 NICs, 4 disks | 348 = 348 | bios U8E122J, bmc TGBT42S 2.83 |
| HPE iLO 5 | 1 TiB, EPYC 7763 ×256, 7 NICs, 18 disks | 277 = 277 | bios A42 v2.60, bmc iLO 5 v2.72 (the model has no BIOS version; below) |
| GB300 tray | XFAIL | 403 = 403 | bmc GB200Nvl-24.12-8 |
| Dell R760 (externally provisioned) | none: adopted (`active`) | 448 = 455 less 7 | bios 1.8.2, bmc 6.00.30.00 |

Registration wrote nothing to any BMC except one request: on the `idrac` node, Metal³'s
`verify_step_priority_override = management.clear_job_queue:90` runs Ironic's iDRAC verify step,
which the twin's iDRAC answers (`POST …/DellJobService/Actions/DellJobService.DeleteJobQueue →
204`). BMO's `online: false` is a soft power-off (`power.off.graceful`: Redfish `GracefulShutdown`,
IPMI soft off), which the model completes. The externally provisioned host is adopted by Ironic
(`deploy_interface direct`, `active`) and its `reboot.metal3.io` annotation is
`power.off.graceful` then `power.on`. A twin-side power-off of an `online: true` host is undone by
Ironic's power sync within 10–30 s ("state 'power off' does not match expected state. Changing
hardware state to 'power on'."), so BMO never sees `poweredOn: false`. A failed PSU reaches the
BMH's `Healthy` condition (`False`, `CriticalError`) in 7–68 s and clears in 20–65 s (BMO's
60-second poll). An unreachable BMC puts the Ironic node into maintenance with fault
`power failure` after 294–339 s (three failed syncs; sushy's own retries make each Redfish one take
about 30 s, ipmitool's about 60 s), and Ironic lifts it 10–12 s after the BMC answers again.
Deleting a host powers it off first (`powering off before delete`: `power.off.force`), removes the
Ironic node and, with kube-controller-manager, its HardwareData and firmware objects.

The twin's log of `generic-c02` across the run (reads omitted):

```
#2276  power.off.graceful redfish POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset   (online: false)
#2277  power: On -> PoweringOff;  #2302 PoweringOff -> Off
#2419  power.on redfish POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset             (online: true)
#2599  act user power.off.force control POST /v1/power                                      (dctwin power off)
#2771  power.on redfish POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset             (Ironic's power sync)
#2828  log.SEL "Power supply psu-0 failed."; #2830 health: OK -> Critical;  #3380 Critical -> OK
#3852  fault inject generic/bmc/unreachable;  #4502 fault clear
```

## Findings

### Twin bugs

1. **IPMI: Set System Boot Options parameter 3 was refused** (fixed here, in
   `internal/adapter/ipmi`). Before every boot device change Ironic's `ipmitool` management
   interface sends `raw 0x00 0x08 0x03 0x08` (parameter 3, BMC boot flag valid bit clearing: do not
   clear the boot flags on the 60-second timeout; `[ipmi]disable_boot_timeout`, default true). The
   twin answered completion code `0x80` (parameter not supported), so every boot device change over
   IPMI failed and agent inspection never started:
   `Unable to send RAW command (channel=0x0 netfn=0x0 lun=0x0 cmd=0x8 rsp=0x80): Unknown (0x80)` →
   `Failed to inspect hardware. Reason: unable to start inspection: IPMI call failed: raw 0x00 0x08
   0x03 0x08.` FreeIPMI's `ipmi-chassis-config`, which MAAS uses, writes the same parameter
   ([compat/maas](../maas/)). The BMC now accepts and reads back parameter 3
   (`TestBootFlagValidBitClearing`; `TestIpmitool` sends Ironic's raw command and reads it with
   `chassis bootparam get 3`), and the CANNOT row above is what follows.
2. **Fixed after this run: a restart at `model` fidelity passed through Off.** A warm restart now
   keeps the host On while it resets, applies pending settings and runs POST (`warmReset`). As found: `internal/engine/power.go` (`powerLocked`)
   handles `power.restart` and `power.restart.graceful` like `power.cycle`: PowerState goes On →
   PoweringOff → Off → PoweringOn → On. A warm restart (Redfish `ForceRestart`, `GracefulRestart`,
   IPMI hard reset) does not remove power, and a BMC keeps reporting `On`; `PowerCycle` is the reset
   that does. Not visible through Metal³ (Ironic reboots with ForceOff and On); MAAS resets a host
   four times because of it ([compat/maas](../maas/)).
3. **Fixed after this run: Redfish reads were logged without the user** (the authenticated user is
   now carried to the read log). As found: `logRead` (`internal/adapter/redfish/handler.go`)
   builds its event source without a principal, so a read authenticated by session token or Basic
   auth has `source.principal` empty; only session creation and writes name the user. The harness
   counts the requests of a registration but cannot attribute the reads to the Secret's user.
4. **Fixed after this run: `dctwin fault clear <id>` also cleared every fault whose id starts with
   `<id>-`** (an id now names exactly one fault; its propagated children go with it by their parent
   link). As found:
   (`internal/engine/faults.go`, `Clear`: `strings.HasPrefix(f.ID, idOrSelector+"-")`, meant for a
   cascade's children). Clearing `m3-health-smc-b02` cleared `m3-health-smc-b02-creds`, another
   host's fault, in an early run; the harness now names faults so that none is another's prefix.
5. **Fixed after this run: the HPE DL385 Gen10 Plus model had no BIOS version.** The profile now
   carries the System ROM as its `bios` firmware, the iLO dialect binds inventory entries 1 and 2
   to the model, and every dialect's BiosVersion and manager FirmwareVersion follow the model. As
   found: Its iLO serves the capture's
   `BiosVersion: "A42 v2.60 (08/11/2022)"`, which BMO records, but the model's `firmware` has no
   `bios` entry, so a BIOS update would not show on the BMC. (Every other profile here binds both.)

### Metal³ and Ironic behaviour the twin surfaced (not twin bugs)

1. **`idrac-redfish://` with `inspectionMode: fast` cannot register.** BMO v0.14.2's iDRAC access
   details return the `redfish` inspect interface (`pkg/hardwareutils/bmc/redfish.go`: "FIXME
   (dtantsur): this should be using idracRedfish, but it's not currently enabled in ironic-image"),
   and Ironic 39 refuses it for the `idrac` hardware type: `400 … inspect interface implementation
   '<…RedfishInspect …>' is not supported by hardware type IDRACHardware`. The BMH stays
   `registering` with no `errorType`, retrying for ever; no Ironic node is created. With
   `inspectionMode: disabled` (and `bootMACAddress`) the same BMC registers as `idrac`.
2. **BMO shows nothing while Ironic has lost the BMC.** With the node in maintenance, fault `power
   failure`, `power_state` None, the BMH stays `operationalStatus: OK`, `poweredOn: true` (stale),
   `Healthy: True` (Redfish; `Unknown` over IPMI). BMO's `UpdateHardwareState` only logs "could not
   determine power state".
3. **An out-of-band power change is reverted, not reported.** Metal³ keeps Ironic's
   `force_power_state_during_sync = True`, so Ironic powers the host back on within a sync interval;
   the BMH never shows the change (BMO polls every 60 s).
4. **HostFirmwareSettings can be empty for five minutes.** BMO's controller may read Ironic's BIOS
   settings before Ironic has cached them (seen on the adopted host and the GB300 tray in one run),
   stores an empty list, and reads again only after `reconcilerRequeueDelay` (5 minutes).
5. A host left in `inspection error` gets no power management and no `Healthy` refresh from BMO.
6. Ironic on SQLite in one process answers API reads with `500` after a five-second database lock
   wait while its power sync is stuck on dead BMCs; the harness's reads retry (`compat/ironic` saw
   the same as slow reads).

## Not run, and why

| Item | Why |
|---|---|
| Provisioning (`spec.image`), deprovisioning with cleaning, automated cleaning, agent inspection, RAID and BIOS changes through `HostFirmwareSettings.spec` (BMO applies them in a manual clean that boots IPA) | Need the IPA ramdisk running on the host and calling back. Hosts here are `model` fidelity; the CANNOT row shows exactly where it stops: Ironic sets PXE and powers the host on, the twin's boot walk asks the provisioning network for a boot server, and nothing answers (Metal³'s dnsmasq is not run) |
| PXE to Ironic's iPXE, through the twin's boot walker | Would need Metal³'s provisioning DHCP (dnsmasq in the Ironic pod) answering the twin relay; the walker would then fetch Ironic's iPXE script and the (placeholder) kernel and ramdisk and stop at `boot`. Not wired here |
| Virtual media (`redfish-virtualmedia://`), `DataImage`, `PreprovisioningImage` | Need an ISO built and served and a host that boots it |
| BMO's webhooks | `-webhook-port 0`: they need a serving certificate the API server trusts; they validate specs only |
| `BMCEventSubscription` | Not exercised |
| Ironic standalone operator (`Ironic` CR) | BMO is pointed at Ironic by `IRONIC_ENDPOINT`, as without the operator |

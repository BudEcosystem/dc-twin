# OpenStack Ironic against the twin

`compat/sushy` drives the twin with **sushy**, the Redfish library Ironic uses. This harness runs
**Ironic itself**: the bare-metal service OpenStack and Metal3 (through its bare-metal operator)
put in front of every machine, released, pinned and unmodified. Ironic decides what to send and
when; the twin only answers. Every action is then checked on the twin's side as well: the
canonical request events Ironic's Redfish traffic produced, and the model's state.

| | |
|---|---|
| Ironic | **39.0.0** (released 2026-09, the 2026.2 series), from PyPI |
| Client | python-ironicclient 6.3.0 (the `baremetal` CLI) |
| Constraints | OpenStack's 2026.2 upper constraints: sushy 5.13.0, openstacksdk 4.20.0, oslo.service 4.8.0, SQLAlchemy 2.0.52, eventlet 0.41.2 |
| Shape | one `ironic` process (API embedded in the conductor), SQLite, `auth_strategy = noauth`, `rpc_transport = none` ([ironic.conf.in](ironic.conf.in)) |
| Driver | hardware type `redfish`; power, management, inspect, bios, firmware and vendor interfaces `redfish`; boot `redfish-virtual-media`; deploy `ramdisk`; network `noop` |
| BMCs | c02/server-01 (generated DMTF tree), b01/server-01 (Dell iDRAC capture), b02/server-01 (Supermicro), c01/server-02 (HPE iLO 5), b05/server-01 (Lenovo XCC), a01/compute-tray-01 (NVIDIA GB300 tray, `redfish_system_id=/redfish/v1/Systems/System_0`) |
| Addressing | each BMC at its **leased site address** (`https://10.30.x.y`), reached through the twin's site gateway, which Ironic uses as `HTTPS_PROXY` (requests, and so sushy, honour it); TLS unverified, as Metal3's `disableCertificateVerification`. `ADDRESSING=hostname` enrolls `https://<rack>-<device>-bmc` with `redfish_verify_ca` set to the estate CA; `ADDRESSING=loopback` uses the BMC's loopback port |

No IPA ramdisk is booted: the harness exercises everything Ironic does out of band. The periodic
intervals are shortened (power sync every 10 s, BIOS-apply polling every 5 s, power-failure
recovery every 15 s) so a run takes ten to fifteen minutes; [ironic.conf.in](ironic.conf.in) says
why each option is set.

## Running it

```bash
make build                                  # bin/dctwin (or use any dctwin binary)
DCTWIN=$PWD/bin/dctwin compat/ironic/run.sh # PASS/FAIL table on stdout; artefacts under $WORK
WORK=/tmp/ironic DCTWIN=$PWD/bin/dctwin compat/ironic/run.sh   # keep the venv between runs
ADDRESSING=hostname DEVICES="c02/server-01 b01/server-01" DCTWIN=$PWD/bin/dctwin compat/ironic/run.sh
```

`run.sh` creates a Python 3.11+ venv in `$WORK` (Ironic 39 needs 3.11), installs the pins, brings
up `examples/estates/lab-model.yaml` with the `redfish` and `dhcp` adapters (or reuses a running
estate named by `DCTWIN_STATE`), renders `ironic.conf`, creates the schema (`ironic-dbsync
create_schema`), starts `ironic` on `127.0.0.1:16385` (`API_PORT`) with `HTTPS_PROXY` set to the
site gateway, runs [flow.py](flow.py), prints the closing `baremetal node list` and `baremetal port
list` from the real CLI (`CLI=0` skips them; the CLI takes about twenty seconds per call to start
here), and stops Ironic and the estate. The first start of Ironic takes about two minutes.
Artefacts: `ironic.txt` (the table and a note per step), `evidence.txt` (conductor log lines,
twin events and each node's Ironic history), `ironic.json`, `ironic/log/ironic.log`.

**macOS only:** `run.sh` uninstalls `setproctitle` from the venv. On macOS it calls LaunchServices,
which is not fork-safe, and every conductor process cotyledon forks dies with SIGSEGV in
`set_process_title`; cotyledon treats the module as optional. Ironic's code is untouched.

## What each step does and how it is checked

flow.py calls Ironic's REST API (microversion 1.115, the calls the `baremetal` CLI makes). The
lifecycle runs node by node; the reactions run on every node at once, since each waits on
Ironic's periodic tasks.

| Step | Ironic | Checked on the twin |
|---|---|---|
| `enroll` | `POST /v1/nodes`, driver `redfish`. With `ADDRESSING=hostname` a throwaway enrollment trusting only the system CAs comes first: its `manage` must fail with `CERTIFICATE_VERIFY_FAILED` | — |
| `validate` | `GET …/validate`: power, management, inspect, bios, firmware must be true (boot and deploy report the deploy image they would need) | — |
| `manage` | enroll → verifying → manageable: Ironic syncs power, caches vendor, boot mode, BIOS settings and firmware components | Redfish reads authenticated as the BMC user, **no write**; Ironic's `power_state` equals the model's power |
| `bios.cached` | `GET …/bios?detail=True` | every cached setting equals the BMC's `Bios.Attributes` (read in-process, outside the request log) |
| `firmware.cached` | `GET …/firmware` | the BIOS and BMC versions equal the model's firmware |
| `power.off`, `power.on` | `PUT …/states/power` | `power.off.force` / `power.on` request events from protocol `redfish`; the model reaches Off / On |
| `boot.pxe` | `PUT …/management/boot_device {pxe, persistent: false}` | a `boot.override` request event; model override `Pxe`/`Once` |
| `reboot` | `PUT …/states/power rebooting` | Ironic's reboot is `power.off.force` then `power.on`; the model goes On → Off → PoweringOn → On |
| `boot.consumed` | — | the reboot used the one-time override: model override `Disabled` |
| `inspect` | out-of-band inspection (`redfish` inspect interface, hooks `validate-interfaces,ports,architecture`) | no write; `memory_mb` equals the BMC's `MemorySummary`; the ports Ironic created are exactly the MACs of the system's `EthernetInterfaces`; inventory serial equals the model's identity |
| `bios.clean` | manual clean, step `bios.apply_configuration` (one Enabled/Disabled attribute flipped), `disable_ramdisk: true` | `bios.set` request (to the vendor's settings object), `bios.pending` in the model, then Ironic's reboot (`power.off.force`, `power.on`) |
| `bios.applied` | Ironic polls the BMC until the value is current, then finishes the step and refreshes its cache | the model applied the pending setting on that reboot; the BMC and Ironic's cache both read the new value |
| `provide` | manageable → available (`automated_clean = false`) | no write |
| `sync.twin-off` / `sync.twin-on` | the periodic power sync (`force_power_state_during_sync = false`, Metal3's setting) | `dctwin power off/on` (an operator act, protocol `control`) is recorded by Ironic within one or two sync intervals; Ironic sends **no** power request back; the conductor log line is quoted |
| `health.degraded` / `health.recovered` | the power sync also caches `System.Status.Health` into the node's `health` and history | a failed PSU (`generic/power/psu-failure`), or Xid 79 on the GB300 tray's first GPU, rolls up to the system's `Status.Health: Critical`; clearing it returns `OK` |
| `bmc.unreachable` | the power sync fails | `generic/bmc/unreachable`: the BMC drops every connection |
| `bmc.maintenance` | after `power_state_sync_max_retries` failures: maintenance, fault `power failure`, `power_state` None, an ERROR history event | — |
| `bmc.recovered` | the power-failure recovery periodic clears maintenance once the BMC answers | the fault is cleared; Ironic's `power_state` equals the model's; reads only |

## Results

Run 2026-10-19 on macOS (arm64), Python 3.11, `lab-model` at time scale 0.05, all six BMCs by
leased site address through the site gateway (`run.sh`), after the twin fixes the first runs led
to (below). The first runs (2026-10-18; 451 s and 436 s once Ironic was up) scored 123 PASS,
1 FAIL (b02 `bios.clean`), 1 XFAIL, 1 BLOCK (b02 `bios.applied`):

```
| step             | a01/compute-tray-01 | b01/server-01  | b02/server-01          | b05/server-01  | c01/server-02 | c02/server-01 |
|                  | nvidia/gb300-bmc    | oem-dell/idrac | oem-supermicro/redfish | oem-lenovo/xcc | oem-hpe/ilo   | generic/dmtf  |
|------------------|---------------------|----------------|------------------------|----------------|---------------|---------------|
| enroll           | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| validate         | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| manage           | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| bios.cached      | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| firmware.cached  | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| power.off        | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| power.on         | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| boot.pxe         | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| reboot           | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| boot.consumed    | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| inspect          | XFAIL               | PASS           | PASS                   | PASS           | PASS          | PASS          |
| bios.clean       | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| bios.applied     | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| provide          | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| sync.twin-off    | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| sync.twin-on     | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| health.degraded  | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| health.recovered | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| bmc.unreachable  | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| bmc.maintenance  | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |
| bmc.recovered    | PASS                | PASS           | PASS                   | PASS           | PASS          | PASS          |

125 PASS, 0 FAIL, 1 XFAIL, 0 BLOCK, 0 SKIP  (XFAIL: fails as on the captured hardware; BLOCK: a prerequisite failed; SKIP: not exposed)
```

**125 PASS, 0 FAIL, 1 XFAIL.** In the first runs, `ADDRESSING=hostname` (every BMC at
`https://<rack>-<device>-bmc`, `redfish_verify_ca` = the estate CA; 601 s, run alongside the
other) gave a table identical to by-address mode; it was not rerun after the fixes. In that mode each of the six enrollments is first tried
trusting only the system CAs, and Ironic refuses all six (`CERTIFICATE_VERIFY_FAILED`), so the
verification is real.

- **`b02 bios.clean`**, FAIL in the first runs, passes: Ironic waits after its reboot for
  BootProgress to reach an OS state, and the twin served the capture's
  `SystemHardwareInitializationComplete` for ever (twin bug 1, fixed). flow.py still recovers a
  node stuck in a clean step as an operator must (maintenance, delete, re-enroll, manage).
- **XFAIL `a01 inspect`**: Ironic's out-of-band inspection raises `MissingAttributeError: The
  attribute EthernetInterfaces is missing from the resource /redfish/v1/Systems/System_0`. The GB
  tray capture's host system has no `EthernetInterfaces` link, so Ironic fails the same way on the
  captured hardware; its inspection error handler then powers the node off (twin:
  `power.off.force` from protocol redfish). flow.py returns the node to manageable and on.

What the notes record per vendor (artefact `ironic.txt`):

| BMC | BIOS settings Ironic cached (registry) | Firmware Ironic cached | BIOS change by manual clean (settings object Ironic patched) | Boot override written to | Inspection |
|---|---|---|---|---|---|
| GB300 tray | 403 (0) | bmc GB200Nvl-24.12-8 | AssetTagProtection → Enabled (`/Systems/System_0/Bios/Settings`) | `/Systems/System_0/Settings` | XFAIL, above |
| Dell iDRAC | 455 (455) | bios 1.8.2, bmc 6.00.30.00, 4 NICs | ProcVirtualization → Disabled (`/Systems/System.Embedded.1/Bios/Settings`) | `/Systems/System.Embedded.1` | 512 GiB, 5 ports, local_gb 1787, **cpu_arch ''** (below) |
| Supermicro | 379 (0) | bios 1.4, bmc 11.01.01, 10 NICs | ADDDCSparing_3C3F → Disabled (`/Systems/1/Bios/SD`) | `/Systems/1` | 2 TiB, cpu count 128, x86_64, 12 ports, local_gb 7152 |
| Lenovo XCC | 348 (348) | bios U8E122J, bmc TGBT42S 2.83, 2 NICs | DevicesandIOPorts_IntelVTforDirectedIOVTd → Disabled (`/Systems/1/Bios/Pending`) | `/Systems/1` | 1 TiB, cpu count 128, x86_64, 5 ports, local_gb 893 |
| HPE iLO 5 | 277 (277) | bios A42 v2.60, bmc iLO 5 v2.72, 3 NICs | AmdVirtualDrtmDevice → Enabled (`/Systems/1/bios/settings`) | `/Systems/1` | 1 TiB, cpu count 256, x86_64, 7 ports, local_gb 446 |
| generic DMTF | 9 (0) | bios 2.4.1, bmc 1.12.0 | ProcVirtualization → Disabled (`/Systems/1/Bios/Settings`) | `/Systems/1` | 1 TiB, cpu count 128, x86_64, 2 ports, local_gb 1787 (the first runs: cpus 0, cpu_arch '', local_gb 0; twin bugs 2 and 3, fixed) |

Every power, boot and BIOS write Ironic made appears in the twin's log as the canonical request
from protocol `redfish` by the BMC's user (`power.off.force`, `power.on`, `boot.override`,
`bios.set`), with the model transition it caused; `manage`, `inspect` and `provide` wrote nothing.
Ironic's reboot is `ForceOff` then `On`, and the one-time PXE override set before it was consumed
by that boot. In the reactions, Ironic recorded `dctwin power off`/`on` within 4-10 s
(`force_power_state_during_sync = false`, so it sent nothing back), mirrored a failed PSU or
Xid 79 into `health: Critical` and back within 8-10 s, saw a hung BMC fail its first power sync
after about 36 s (sushy retries each request 10 times, 3 s apart), put every node into maintenance
with fault `power failure` after about 64 s, and lifted it 9-16 s after the fault cleared,
opening a new Redfish session.

Log excerpts (c02/server-01, from `evidence.txt`):

```
WARNING During sync_power_state, node 527ccd97-… state does not match expected state 'power on'. Updating recorded state to 'power off'.
WARNING During sync_power_state, node 527ccd97-… state does not match expected state 'power off'. Updating recorded state to 'power on'.
[history] INFO  Node health status changed: health='Critical' (previously 'OK')
WARNING During sync_power_state, could not get power state for node 527ccd97-…, attempt 1 of 1. Error: Redfish connection failed …: Unable to connect to https://10.30.13.13/redfish/v1/Systems …
ERROR During sync_power_state, max retries exceeded for node 527ccd97-…, node state None does not match expected state 'power on'. Updating DB state to 'None' Switching node to maintenance mode. …
INFO Node 527ccd97-… is recovered from power failure with actual power state 'power on'.
```

and b01/server-01's BIOS clean, Ironic and twin side by side:

```
ironic  Executing {'interface': 'bios', 'step': 'apply_configuration', 'args': {'settings': [{'name': 'ProcVirtualization', 'value': 'Disabled'}]} …
twin      790 request stack/root redfish PATCH /redfish/v1/Systems/System.Embedded.1/Bios/Settings -> bios.set 200
twin      791 state   stack/root bios.pending: None -> 1
twin      795 request stack/root redfish POST …/Actions/ComputerSystem.Reset -> power.off.force
twin      802 request stack/root redfish POST …/Actions/ComputerSystem.Reset -> power.on
twin      817 state   behaviour/engine power: PoweringOn -> On
twin      818 state   behaviour/engine bios: None -> pending settings applied
ironic  Detected post-BIOS reboot completion for node 789f3725-…, resuming the current step.
ironic  Node 789f3725-… cleaning complete
```

## Findings

### Twin bugs the first runs found

Bugs 1–4 are fixed in the twin (BootProgress is derived from the model's power and boot state;
the generated tree serves processor architecture, cores and threads, and a `Storage` with its
`Drives`; the gateway hijacks before it answers CONNECT), each with a test
(`TestBootProgressFollowsPower`, `TestGeneratedTreeInventory`, `TestGatewayConnectReply`), and
the rerun above shows them. Bug 5 stands: a generic profile has no vendor to name.

1. **BootProgress is the capture's, not the model's.** Nothing in the twin binds
   `ComputerSystem.BootProgress`; it is served as captured whatever the host does.
   - Supermicro (b02, `GET /redfish/v1/Systems/1`): after Ironic's `ForceOff` + `On` for the
     BIOS step the model is On, has applied the pending setting and booted `os`, yet the BMC
     answers `"BootProgress": {"LastState": "SystemHardwareInitializationComplete"}` on every
     poll. Ironic 39 polls it every 5 s ("boot progress: BootProgressStates.HARDWARE_COMPLETE.
     Waiting for boot progress to reach OS started.") and the clean step never finishes.
   - GB300 tray (`/redfish/v1/Systems/System_0`): `LastState: OSRunning` with the capture's
     `LastStateTime: 2025-01-09T20:11:07Z`, also while the host is powered off.
   - A real BMC reports `None` when the host is off, moves through the POST states on power-on,
     and reaches `OSBootStarted` (and `OSRunning` where the host OS reports it) with a current
     `LastStateTime`. The model's `power` and `booted` already carry what is needed.
2. **The generated DMTF tree's Processors say nothing Ironic can use.**
   `GET /redfish/v1/Systems/1/Processors/CPU_0` answers `Model: "x86-64, 32 cores"`,
   `Manufacturer: "dc-twin"`, `ProcessorType: CPU` and no `ProcessorArchitecture`,
   `InstructionSet`, `TotalCores`, `TotalThreads` or `MaxSpeedMHz`. Ironic records `cpus 0`,
   `cpu_arch ''` ("Discovered CPU architecture:  for node"). A real BMC answers
   `ProcessorArchitecture: x86`, `InstructionSet: x86-64`, `TotalCores: 32`, `TotalThreads: 64`
   and a CPU model string.
3. **The generated DMTF tree's Storage is empty.** `GET /redfish/v1/Systems/1/Storage` answers
   `Members: []` although the model has two drives; there is no `SimpleStorage` either. Ironic
   logs "Could not provide a valid storage size … Assuming this is a disk-less node" and sets
   `local_gb 0`. A real BMC lists a `Storage` member whose `Drives` carry `CapacityBytes`.
4. **The site gateway's CONNECT reply carries `Transfer-Encoding: chunked`.** For an HTTP/1.1
   `CONNECT 10.30.12.14:443` it answers `HTTP/1.1 200 OK` with `Transfer-Encoding: chunked`
   (`serveConnect` calls `WriteHeader(200)` before `Hijack`, and the hijack flushes a chunked
   header). RFC 9110 9.3.6: a server MUST NOT send Transfer-Encoding or Content-Length in a 2xx
   response to CONNECT. curl ignores it ("Ignoring Transfer-Encoding in CONNECT 200 response");
   Python 3.11's `http.client` sends `CONNECT … HTTP/1.0` and gets a reply without it, and
   discards tunnel reply headers anyway, so Ironic is not affected; a strict client may read the
   tunnel as a chunked body.
5. Minor: the generic system's `Manufacturer` is `dc-twin`, so Ironic's vendor detection records
   `vendor: dc-twin` ("Detected vendor dc-twin").

Not bugs, but worth knowing: BMC certificates name the host (`c02-server-01-bmc`) and 127.0.0.1,
not the leased address, so by-address enrollment can only run unverified (as with most real
BMCs' self-signed certificates); by host name it verifies. `generic/bmc/unreachable` is a hung
BMC: through the gateway the CONNECT and TLS succeed and the BMC then drops the connection; a BMC
off the network would fail the CONNECT instead. sushy treats both alike.

### Ironic behaviour the twin surfaced (not twin bugs)

1. **Out-of-band inspection crashes on a system without `EthernetInterfaces`** (GB200/GB300
   host system as captured): `_get_interface_info` reads `system.ethernet_interfaces`, which sushy
   raises for a missing link, instead of treating it as no interfaces. The inspection error
   handler then powers the node off.
2. **Inspection takes the first `Processors` member as the CPU.** The XE9680's iDRAC lists
   `Video.Slot.2-1` (an H100) before `CPU.Socket.2` and `CPU.Socket.1` (the twin keeps the
   capture's order), so Ironic records `cpu_arch ''` and `model_name "NVIDIA H100 PCIe"`.
3. **A BIOS step can wait for ever on BootProgress.** When the system reports BootProgress,
   `_check_node_redfish_bios_apply` waits for `OSBootStarted`/`OSRunning` and calls
   `touch_provisioning()` on every poll, so the clean timeout never fires; the step is not
   abortable (`abort` only sets `abort_after`), and maintenance plus delete is the only way out.
4. **A recorded out-of-band power change counts as a failed sync until a later sync confirms it.**
   With `power_state_sync_max_retries = 1`, two changes within one sync interval sent nodes to
   maintenance (`max retries exceeded … node state power on does not match expected state 'power
   off'`) in an early run; flow.py now leaves one sync interval between acts, as an operator would.
5. Ironic maps `PoweringOn` to `power on`, so a power-on returns while the host is still powering
   on; flow.py waits for the model rather than reading it at that instant.
6. SQLite in one process: with six dead BMCs at once, writers hit `database is locked` (retried)
   and one API read took 92 s in an early run; flow.py reads with a 180 s timeout and retries.
7. sushy retries every request 10 times, 3 s apart, on connection errors before Ironic sees one;
   with Ironic's default `[redfish]connection_attempts = 5` (4 s apart) a dead BMC would cost
   about 5 x (30 + 4) s per call (this harness sets 1). Ironic drops a cached session on a connection error and logs in again
   when the BMC is back.
8. BIOS settings are stored as strings, JSON booleans as `"1"`/`"0"` (GB, Supermicro and XCC
   attributes include booleans); flow.py compares accordingly.
9. After a one-time override is consumed Ironic still reads `boot_device: pxe, persistent: None`:
   it reports `BootSourceOverrideTarget`, which stays `Pxe` with `BootSourceOverrideEnabled:
   Disabled`, as on real BMCs.

## Not run, and why

| Item | Why |
|---|---|
| Deploy, in-band inspection, automated cleaning, rescue | Need the IPA ramdisk booted on the host and calling back; hosts here are `model` fidelity (no VM substrate on this machine), so there is nothing to boot it |
| Virtual-media boot (`redfish-virtual-media` insert/eject) | Needs Ironic to build and serve an ISO and the host to boot it; `validate` reports the missing deploy image instead |
| Firmware update (`firmware.update` clean step) | Needs an image served over HTTP that the BMC fetches; the twin stages firmware, but the end-to-end path was not wired in this run |
| RAID, secure boot toggling, BMC clock verification beyond the verify step | Not exercised |
| Metal3's bare-metal operator | Needs Kubernetes; BMO's Redfish behaviour is Ironic's, which this harness runs |
| PDU outlet loss as the out-of-band power change | Not exercised; `dctwin power off` (an operator act) is the out-of-band change used |

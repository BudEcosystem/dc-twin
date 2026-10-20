# Tinkerbell's Rufio against dc-twin

Rufio is Tinkerbell's BMC controller: it reads `Machine` objects (a BMC address and a credential
Secret), reports each machine's power state and reachability in the Machine's status, and runs
`Job`s (power off, boot device, power on, reset) by creating one `Task` per step. Tinkerbell's
workflow controller creates those Jobs itself when a Workflow asks for a network boot. This harness
runs **Rufio, unmodified, at a released tag** on a real kube-apiserver and etcd, points its Machines
at dc-twin's emulated BMCs, and checks each effect in the twin's model and event log, not only in
what Rufio writes back to its own status.

| | |
|---|---|
| Rufio | `github.com/tinkerbell/tinkerbell` **v0.25.0** (commit `63a0254`, released 2026-07-31), `rufio/`, built into the single `tinkerbell` binary and run with every other service switched off by its own flags. The standalone `tinkerbell/rufio` repository is archived; its last tag was v0.6.5 |
| BMC library | bmclib v2.3.5 (Rufio's dependency at the tag): the `gofish` provider for Redfish, the `ipmitool` provider for IPMI (runs `ipmitool -I lanplus`) |
| Kubernetes | kube-apiserver 1.37 and etcd from envtest, started as plain processes with a client-certificate kubeconfig under `$WORK`. No kubelet, scheduler or controller manager: Rufio needs none |
| Twin | `examples/estates/lab-model.yaml`, adapters `redfish,ipmi` |
| BMCs | `b01/server-01` Dell XE9680 (captured iDRAC tree), `b02/server-01` Supermicro SYS-821GE-TNHR (captured), `c02/server-01` generic 2U (generated DMTF tree), `a01/compute-tray-01` NVIDIA GB300 tray (captured; two ComputerSystems, so its Machine names `systemName: System_0`). Each BMC gets two Machines, one per protocol, sharing one Secret |

## Run

```bash
make build                                            # bin/dctwin
KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.37.0) \
  DCTWIN=$PWD/bin/dctwin compat/tinkerbell/run.sh     # PASS/FAIL table; artefacts under $WORK
```

`run.sh` clones Tinkerbell at the tag, checks the commit, builds `tinkerbell` (a large module graph:
about fifteen minutes cold; `TINKERBELL_BIN=` reuses a binary built at the same tag), starts etcd
and kube-apiserver on free loopback ports, applies Rufio's three CRDs from the source tree
(`crd/bases/v1alpha1/bmc.tinkerbell.org_{machines,jobs,tasks}.yaml`), brings up the twin, starts
Rufio, runs `flow.py`, prints the table and stops everything it started. `INTERVAL` sets
`--rufio-power-check-interval` (default `10s`; Tinkerbell's own default is 30 minutes, which would
make the "twin changes underneath" checks wait half an hour). Without `ipmitool` on `PATH` (or
`IPMITOOL=`) only the Redfish Machines run, and the table says so by having no IPMI rows.

`run.sh` never reads `~/.kube/config` or a kube context: Rufio is given `--backend-kube-config`
and a `HOME` under `$WORK`, and every `kubectl` call carries `--kubeconfig`.

### How Rufio reaches the BMCs

The twin serves each BMC on its own loopback port, so a Machine's `host` is `127.0.0.1` and the port
goes in `providerOptions.redfish.port` or `providerOptions.ipmitool.port` (`manifests/`). Two things
rule out the twin's site gateway here: a BMC has a site address only when the `dhcp` adapter has
leased one, and bmclib builds its own HTTP transport without `Proxy`, so `HTTPS_PROXY` is ignored;
IPMI is UDP in any case. `preferredOrder` puts the intended provider first but does not remove the
others, so every connection also tries Dell, Supermicro, OpenBMC and AsRock Rack on `:443`, Intel
AMT on `:16992` and, on a Redfish Machine, ipmitool on `:623`, all on loopback. The TCP ones are
refused at once; ipmitool waits out its timeout, which is most of the eight seconds a Redfish Task
takes (Results), and changes nothing else.

## What it checks

`flow.py` drives Rufio only through Kubernetes objects, as tink-controller or an operator would.

| Phase | Rufio objects | Checked in the twin |
|---|---|---|
| Machines | a Secret per BMC, a Machine per BMC and protocol | Rufio reports `Contactable=True` and the twin's power state |
| `power-off` | Job `powerAction: "off"` | model power `Off`; a `power.off.force` request over that protocol; the Machine's powerState follows on Rufio's next poll |
| `pxe-efi` | Job `bootDevice: {device: pxe, efiBoot: true}` | Redfish `Boot` of the host system reads `Pxe`/`Once`/`UEFI` (rendered in-process); a `boot.override` request over that protocol |
| `power-on` | Job `powerAction: "on"` | model power `On`; a `power.on` request |
| `reboot` | Job `powerAction: reset` | a `power.cycle` (Redfish PowerCycle) or `power.restart` (IPMI hard reset) request; power ends `On` |
| `netboot` | the three-task Job tink-controller creates for `bootMode: netboot` (`off`, `oneTimeBootDeviceAction` pxe EFI, `on`) | requests `power.off.force` → `boot.override` → `power.on` in that order; power ends `On` |
| twin off/on | none: `dctwin power off/on c02/server-01` | both c02 Machines' powerState follows on Rufio's next poll |
| unreachable | none: `dctwin fault inject b01/server-01 generic/bmc/unreachable`, then clear | both b01 Machines go `Contactable=False`, powerState `unknown`; back to `True`/`on` once cleared |

Rufio has a race of its own: its Job controller lists a Job's Tasks from the informer cache, and
when the Job is reconciled again before the cache holds the Task it has just created (19 ms apart in
one run), it creates the same Task again, gets `AlreadyExists` and marks the whole Job `Failed`
(`rufio/internal/controller/job.go`). The Task already created still runs; the Job's later Tasks
never do. Nothing the BMC answers is involved. When a Job fails with that message the flow submits it
once more under a new name, says so in the row, and checks the twin for everything Rufio sent since
the first submission; any other failure is a FAIL.

## Results

Run on 2026-10-18 (macOS arm64, Go 1.27, ipmitool 1.8.19): **136 of 144 checks pass; all 8
failures are one twin bug** (finding 1 below), in the Redfish boot-override rows. Rufio itself
completed all 40 Jobs and 56 Tasks; it never saw an error from the twin that a real BMC would not
give.

| Phase | Redfish (Dell, Supermicro, generic, GB300) | IPMI (the same four) |
|---|---|---|
| Machines: `Contactable=True`, powerState = twin's | 4 of 4 | 4 of 4 |
| `power-off`: Job, model `Off`, `power.off.force`, Machine status follows | 4 of 4 | 4 of 4 |
| `pxe-efi`: Job, system `Boot` = `Pxe`/`Once`/`UEFI` | 4 of 4 | 4 of 4 |
| `pxe-efi`: logged as `boot.override` | **0 of 4**, logged `boot.order` (finding 1) | 4 of 4 |
| `power-on`: Job, model `On`, `power.on` | 4 of 4 | 4 of 4 |
| `reboot`: Job, `power.cycle` (Redfish PowerCycle) / `power.restart` (IPMI hard reset), ends `On` | 4 of 4 | 4 of 4 |
| `netboot` (3 Tasks): Job, ends `On` | 4 of 4 | 4 of 4 |
| `netboot`: `power.off.force` → `boot.override` → `power.on` | **0 of 4**, middle step logged `boot.order` | 4 of 4 |
| twin powers c02 off, then on: Machine powerState follows | yes (5.4 s, 8.0 s) | yes |
| BMC unreachable: `Contactable=False`, `unknown`; cleared: `True`, `on` | yes (8.8 s; 12.3 s) | yes |

What Rufio wrote, after the run (`evidence/machines.txt`, `jobs.txt`, `tasks.txt` under `$WORK`):

```
NAME                  HOST        REDFISH   IPMI     POWER   CONTACTABLE
dell-b01-ipmi         127.0.0.1   <none>    24498    on      True
dell-b01-redfish      127.0.0.1   24434     <none>   on      True
gb300-a01-ipmi        127.0.0.1   <none>    24462    on      True
gb300-a01-redfish     127.0.0.1   24308     <none>   on      True
...
NAME                               TASK                                                          START                  DONE                   COMPLETED
generic-c02-redfish-netboot-task-0 map[powerAction:off]                                          2026-10-18T17:35:53Z   2026-10-18T17:36:01Z   True
generic-c02-redfish-netboot-task-1 map[oneTimeBootDeviceAction:map[device:[pxe] efiBoot:true]]   2026-10-18T17:36:10Z   2026-10-18T17:36:19Z   True
generic-c02-redfish-netboot-task-2 map[powerAction:on]                                           2026-10-18T17:36:27Z   2026-10-18T17:36:36Z   True
```

and the twin's log of the same BMC (`dctwin events --select c02/server-01 --no-reads`, sessions
omitted), Redfish first, then IPMI, then the twin acting underneath Rufio:

```
#424   request stack     power.off.force redfish POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset
#425   state   behaviour power: On → Off  ←#424
#810   request stack     boot.order redfish PATCH /redfish/v1/Systems/1
#811   state   stack     boot.overrideTarget: None → Pxe  ←#810
#812   state   stack     boot.overrideEnabled: Disabled → Once  ←#810
#813   state   stack     boot.overrideMode:  → UEFI  ←#810
#814   state   stack     boot.order: [Pxe Hdd] → [Pxe Hdd]  ←#810
#1022  request stack     power.on redfish POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset
#1036  state   behaviour boot.overrideEnabled: Once → Disabled  ←#1035
#1339  request stack     power.cycle redfish POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset
#2338  request stack     power.off.force ipmi Chassis Control netfn 0x00 cmd 0x02: power down
#2617  request stack     boot.override ipmi Set System Boot Options netfn 0x00 cmd 0x08: parameter 5: Pxe Once UEFI
#2824  request stack     power.on ipmi Chassis Control netfn 0x00 cmd 0x02: power up
#3117  request stack     power.restart ipmi Chassis Control netfn 0x00 cmd 0x02: hard reset
#3993  act     user      power.off.force control POST /v1/power
#4068  act     user      power.on control POST /v1/power
```

A Redfish Task takes eight to nine seconds and an IPMI Task at most four: bmclib opens every provider
in parallel and waits for all of them, and on a Redfish Machine the ipmitool provider waits out its
timeout against `:623`, where nothing answers. With the twin's BMC unreachable, Rufio's Redfish
Machine reports what bmclib saw, `provider: gofish: ... /redfish/v1/Systems: EOF` (the twin completes
the TLS handshake and drops the connection, as a hung BMC does), and its IPMI Machine
`provider: ipmitool: context deadline exceeded`.

## Findings

In the twin (not fixed here):

1. **A Redfish boot override sent by gofish is logged as `boot.order`.** gofish's
   `ComputerSystem.SetBoot` (what bmclib, and so Rufio, uses) PATCHes the whole `Boot` object,
   including the unchanged `BootOrder`. `internal/adapter/redfish/handler.go:562-568` names the
   request `boot.order` whenever `BootOrder` is present, even when `BootSourceOverride*` are set
   too. Every Rufio `bootDevice` and `oneTimeBootDeviceAction` over Redfish is therefore logged as a
   boot-order change; scenarios and reports that look for `boot.override` miss them. It should be
   `boot.override` when an override field is present, `boot.order` only when the order changes.
2. **`SetBoot` emits a `boot.order` state event when the order did not change**
   (`internal/engine/bmcstate.go:280-284`, `[Pxe Hdd] → [Pxe Hdd]` above). The other boot fields
   are guarded by `*dst != *src`; the order is not.
3. **`dctwin get` does not show the boot override mode.** The device view's `boot`
   (`internal/engine/views.go:53`) has `overrideTarget`, `overrideEnabled` and `order`, not
   `overrideMode` or `httpBootUri`, so EFI versus legacy cannot be checked from the model view; this
   harness reads the system's `Boot` with `dctwin inspect` instead.
4. **IPMI request events mix two kinds of status.** An IPMI boot override is logged with
   `status: 200` (an HTTP status: `engine.SetBoot` logs `e.logRequest(r, 200)`,
   `internal/engine/bmcstate.go:267`), while the IPMI adapter logs completion codes (`193` = 0xC1
   for the DCMI probes ipmitool sends) and power requests carry none. A consumer of the event log
   has to know which writer produced an event to read its status.
5. **`dctwin up --state DIR` still rewrites `~/.dc-twin/current`** (`cmd/dctwin/main.go:189`), so a
   second estate started with an explicit state directory takes over the operator's default
   estate. `run.sh` puts the previous pointer back.

In Rufio and bmclib (upstream, reported for whoever runs Rufio against the twin):

- The Job controller's Task-creation race described above: seen in 2 of 4 full runs, once each in
  40 Jobs (Rufio log: two reconciles of the Job 19 ms apart, the second failing with
  `tasks.bmc.tinkerbell.org "...-task-0" already exists`).
- `--rufio-bmc-connect-timeout` has no effect: `controller.NewManager` builds its client with
  `NewClientFunc(time.Minute)` and never reads `Config.BMCConnectTimeout`.
- `Machine.spec.connection.port` (which the CRD defaults to 623) is not passed to any provider;
  ports come only from `providerOptions`.
- `preferredOrder` reorders providers; it does not limit them (above).
- The Machine controller only polls: a change made at the BMC is seen at the next
  `--rufio-power-check-interval`, which the `tinkerbell` binary defaults to 30 minutes.

## Not run

- **bmclib's vendor providers** (`dell`, `supermicro`, `openbmc`, `asrockrack`, Intel AMT): they
  dial the BMC's address on fixed ports (443, 16992) that Rufio gives no option to change, and the
  twin's BMCs are on loopback ports. They need each BMC at its own address, which the loopback
  substrate does not give (D34; the TAP substrate is not built).
- **Rufio's `virtualMediaAction`.** The twin serves `VirtualMedia.InsertMedia`; the flow does not
  yet send it.
- **tink-controller creating the netboot Job from a Workflow**, and Smee serving the PXE boot that
  follows: the Job is created by hand in the shape tink-controller uses, and without the `dhcp`
  adapter the twin's power-on does not walk a network boot.
- **Rufio's v1alpha2 API** (`crd/bases/v1alpha2/bmc.tinkerbell.org_jobs.yaml`): the controller at
  v0.25.0 serves v1alpha1.
- **Rufio in a cluster with its chart's RBAC**: here it runs as a process with a `system:masters`
  client certificate.

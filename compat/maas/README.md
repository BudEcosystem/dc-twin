# Canonical MAAS against the twin

MAAS (Metal as a Service) is Canonical's bare-metal manager: a **region controller** (the API, the
database, and since 3.5 a Temporal server and worker) and **rack controllers** that do the
machine-facing work. In MAAS 3.7 every power action runs as a Temporal workflow whose activity
executes on **maas-agent**, which runs one process per action,
`maas.power <on|off|status|cycle|reset> <driver> --<param> <value> …` (maas-agent's
`internal/power/service.go`, `powerCommand`), and that process loads MAAS's Python power drivers
(`provisioningserver/drivers/power/{redfish,ipmi}.py`). The IPMI driver shells out to FreeIPMI
(`ipmipower`, `ipmi-chassis-config`); the Redfish driver speaks HTTPS through Twisted.

## Can MAAS run here?

MAAS is distributed as a **snap** and, for 3.x, as **Debian packages** in Canonical's PPAs
(`ppa:maas/3.7`, Ubuntu 24.04, amd64 and arm64). This directory runs both:

- **[run-snap.sh](run-snap.sh): MAAS itself, as the snap**, in an Ubuntu 24.04 cloud-image KVM guest
  with real systemd and snapd, the twin inside the same guest. The snap needs both, which no
  unprivileged container gives. This is how Canonical ships MAAS, and the route that shows MAAS
  controlling the twin's BMCs (stage 2).
- **[run.sh](run.sh): MAAS's power drivers from the 3.7.3 packages** in a rootless container
  ([Containerfile](Containerfile)), the twin inside it too (stage 1). It needs no KVM and runs on
  macOS's Docker as well. The packages also install the region and rack, which
  [start-maas.sh](start-maas.sh) starts the way their systemd units do (`STAGES="drivers maas"`), but
  without systemd maas-agent never finishes configuring itself and runs no power action (below), so
  the container is for the drivers.

**BMC addresses.** MAAS stores a BMC by IP address: each power type extracts one from
`power_address` and refuses anything else (`https://127.0.0.1:24150`: "IPAddress() does not support
netmasks or subnet prefixes!"), its drivers use the standard ports (Redfish `https://<ip>` on 443,
IPMI on 623), and the region sends a machine's power action to the agent of a rack controller on
the BMC's subnet. The twin serves each BMC on its own loopback port, so the guest (and the
container) gets a BMC network: a dummy interface `dctwin-bmc` with 10.250.0.1/24, made before MAAS
starts so that the rack controller reports it, and the whole /24 routed locally; `flow.py` gives
each BMC an address of its own there, `10.250.0.<n>`, relaying TCP 443 and UDP 623 to the BMC's
ports (`BMCNet`, one upstream socket per IPMI client). TLS and RMCP+ pass through untouched: the
management network as MAAS expects to find it. (The container needs `--cap-add NET_ADMIN` for the
interface, which acts on its own network namespace only.)

| | |
|---|---|
| MAAS, stage 2 | the **`maas` snap, channel `3.7/stable`, revision 43264** (3.7.4-18072-g.5db61257d, released 2026-10-16; `run-snap.sh` stops if the channel carries another revision), with **`maas-test-db` `3.7/stable`** (PostgreSQL 16, Canonical's database snap for MAAS); `maas init region+rack` |
| Guest | Ubuntu 24.04 server cloud image `release-20260926` (sha256 checked against that release's `SHA256SUMS`), a qcow2 overlay, 4 vCPUs, 6 GiB, `qemu-system-x86_64 -enable-kvm` with user-mode networking (no bridge, no libvirt; SSH on a loopback port), cloud-init seeded over HTTP from the host |
| MAAS, stage 1 | **3.7.3** packages `1:3.7.3-18030-g.9122eee68-0ubuntu1~24.04.1` from the MAAS 3.7 PPA (signing key `3AB6DCF1…684D4A1C`, checked), built from tag `3.7.3` (`9122eee`); `run.sh` diffs the packaged `provisioningserver/drivers/power/` and `power_driver_command.py` (what `maas.power` runs) against the tag: identical. Image `dctwin-maas:3.7.3`, container `dctwin-maas-<pid>`, removed on exit |
| Twin | `examples/estates/lab-model.yaml`, adapters `redfish, ipmi`, a static dctwin binary |
| Image | MAAS accepts a machine only for an architecture it has a boot image for, so stage 2 first imports one, as an operator does after installing: Ubuntu 24.04, GA kernel (about 470 MB from images.maas.io, in the guest) |

## Running it

```bash
compat/maas/run-snap.sh                  # MAAS itself, the snap, in a KVM guest (Linux, /dev/kvm)
KEEP=1 WORK=/tmp/m compat/maas/run-snap.sh  # leave the guest up; $WORK/ssh.sh logs in
compat/maas/run.sh                       # stage 1, MAAS's drivers, in a container (Docker or Podman)
```

`run-snap.sh` downloads and checks the cloud image (or takes `BASE_IMAGE`), builds a static dctwin,
boots the guest, installs the snaps and initialises MAAS ([guest-setup.sh](guest-setup.sh)), starts
the twin, runs `flow.py --stage maas` as root in the guest, copies the results, the twin's event
log and MAAS's logs back to `$WORK`, and powers the guest off and deletes its overlay. `run.sh`
builds the image on its first run (about fifteen minutes).

## Stage 1: `maas.power`, as maas-agent runs it

For each BMC, `flow.py --stage drivers` runs the command maas-agent would run, with the parameters
MAAS stores for a machine of that power type: Redfish `power_address=10.250.0.<n>`, user,
password (and `node_id=System_0` for the GB300 tray, whose BMC serves two systems); IPMI
`power_address=10.250.0.<n>` with MAAS's defaults (`LAN_2_0`, boot type auto, cipher suite 3,
`OPERATOR`, workaround `opensesspriv`, hard power off). Each effect is checked in the twin's model
and event log.

| Check | `maas.power` | Checked in the twin |
|---|---|---|
| `status` | `status` | the answer equals the model's power |
| `off` | `off` | Redfish: `power.off.force` (ForceOff, then MAAS waits for Off) and a one-time PXE override (MAAS leaves every machine it powers off set to network-boot); IPMI: `power.off.force` |
| `on` | `on` | Redfish: `ForceOff` first if the host is on, PXE once (a PATCH of `Boot` on the ComputerSystem; 3.7.4 adds a lookup of the system's `@Redfish.Settings` object), then `power.on`; IPMI: `ipmi-chassis-config --commit` writing the boot flags (parameter 5: no override, then PXE once; parameter 3; parameter 4), then `ipmipower --cycle --on-if-off`. The model is On with a one-time PXE override, which the host consumes as it leaves POST |
| `cycle` | `cycle` | off (if on), then the `on` sequence |
| `reset` | `reset` | Redfish: PXE once, then `GracefulRestart`; IPMI: MAAS's ipmi driver has no reset |
| `twin.off` | `status` after `dctwin power off` | MAAS's driver reads `off` |
| `bad.password` | `status` with a wrong password | refused: the error MAAS records |
| `bmc.lost` | `status` while `generic/bmc/unreachable` | an error after MAAS's retries, not a state |

## Stage 2: MAAS itself

`flow.py --stage maas` runs as root in the guest, where `maas login admin …` was done. It imports
the image, then creates one machine per BMC through the MAAS CLI (`maas admin machines create …
power_type=redfish|ipmi power_parameters_*=…`): seven `deployed=true` (machines already in service,
which MAAS does not commission) and two `commission=false` (New) for the cycle. Every step runs on all
machines at once, and each MAAS action is checked in the twin's event log and model as well as in
MAAS's record:

| Check | MAAS | Checked in the twin |
|---|---|---|
| `create` | `machines create`, the power parameters read back | — |
| `query` | `machine query-power-state` | equals the model; MAAS's requests (reads) in the log |
| `power.off` / `power.on` | `machine power-off` / `power-on` | the model; the `power.off.*` / `power.on` request; MAAS's `power_state` |
| `cycle` | `machine commission` of a New machine that is on: MAAS's CYCLE power workflow (`Node._start(allow_power_cycle=True)`), then `machine abort` | off, then on, in the log; the abort powers it off. (Commissioning itself needs the host to boot MAAS's ephemeral image) |
| `poll.off` / `poll.on` | none: `dctwin power off`, then `on`, underneath | MAAS's `power_state` follows from its own polling (the rack queries every machine not queried in five minutes, every 15 s) and MAAS writes nothing |
| `bmc.lost` | `query-power-state` while `generic/bmc/unreachable` | a power error, not a state; recovery once cleared |

## Results

### Stage 1: `maas.power`

Run 2026-10-20 on the lab machine (x86_64, rootless Podman 5.7; image built for amd64), every BMC
at its `10.250.0.<n>` address, the twin from this tree with the IPMI fix and the engine's warm
restart (finding 1, since fixed); 794 s:

```
| check        | generic-c02 | dell-b01 | smc-b02 | xcc-b05 | ilo-c01 | gb300-a01 | generic-c02-ipmi | dell-b01-ipmi | gb300-a01-ipmi |
|--------------|-------------|----------|---------|---------|---------|-----------|------------------|---------------|----------------|
| status       | PASS        | PASS     | PASS    | PASS    | PASS    | PASS      | PASS             | PASS          | PASS           |
| off          | PASS        | PASS     | PASS    | PASS    | PASS    | PASS      | PASS             | PASS          | PASS           |
| on           | PASS        | PASS     | PASS    | PASS    | PASS    | PASS      | PASS             | PASS          | PASS           |
| cycle        | PASS        | PASS     | PASS    | PASS    | PASS    | PASS      | PASS             | PASS          | PASS           |
| reset        | PASS        | PASS     | PASS    | PASS    | PASS    | PASS      | -                | -             | -              |
| twin.off     | PASS        | PASS     | PASS    | PASS    | PASS    | PASS      | PASS             | PASS          | PASS           |
| bad.password | PASS        | PASS     | PASS    | PASS    | PASS    | PASS      | PASS             | PASS          | PASS           |
| bmc.lost     | PASS        | PASS     | PASS    | PASS    | PASS    | PASS      | PASS             | PASS          | PASS           |

69 PASS, 0 FAIL
```

**69 of 69.** MAAS's own drivers, unmodified, switch every BMC off and on, cycle and reset it and
read it, and each action is the canonical request in the twin's log with its model transition:

```
redfish  off    power.off.force, boot.override (Pxe, Once)    -- MAAS leaves a machine it powers off set to network-boot
redfish  on     boot.override (Pxe, Once), power.on
redfish  reset  boot.override (Pxe, Once), power.restart      -- GracefulRestart; the host stays On, MAAS reads 'on' once
ipmi     on     boot.override (parameter 5: no override), boot.override (parameter 5: Pxe Once Legacy),
                parameter 3, parameter 4 (ipmi-chassis-config --commit), power.on (ipmipower --cycle --on-if-off)
```

A twin-side power-off reads `off`. A wrong password is MAAS's `PowerAuthError` at once (Redfish
`401`; IPMI "Incorrect password", from FreeIPMI's RAKP failure), not retried. An unreachable BMC
fails after 30 s over Redfish (`ResponseNeverReceived`: the twin completes TLS and drops the
connection, as a hung BMC does) and after 141 s over IPMI (`PowerConnError: Connection timed out`,
after FreeIPMI's own retries), with no state. The GB300 tray is driven at `node_id=System_0`; MAAS
3.7.3 PATCHes `Boot` on the ComputerSystem itself.

Before the engine kept a warm restart On, the six `reset` rows failed: one `maas.power reset` sent
two to four `GracefulRestart`s, because the twin took PowerState through `Off` and MAAS, reading
`off`, reset again (finding 1). The runs addressing each BMC at `127.0.1.<n>` and at its loopback
port gave the same table otherwise.

### Stage 2: MAAS itself

Run 2026-10-20 on the lab machine: the `maas` snap 3.7.4 (revision 43264, `3.7/stable`) and
`maas-test-db` (revision 185) in the Ubuntu 24.04 guest (4 vCPUs, 6 GiB, under a 8 GiB memory cap),
nine machines, the twin with the warm-restart fix; 1142 s once MAAS was up (the snaps' installation
and `maas init` take about four minutes; the image import 20 s, MAAS having fetched the image
index at `maas init`). In that first run the guest setup's ssh session stayed open after MAAS was up
(the twin, started in the background, still held it) and was released by hand; `guest-setup.sh` now
starts the twin detached, and a second run the same day went end to end unaided: `run-snap.sh`
alone, 55 PASS, 0 FAIL, 1140 s, the guest and its overlay removed at the end. The table is that
run's:

```
MAAS 3.7.4 (region, rack, Temporal, maas-agent) managing power through dc-twin's BMCs

| check     | generic-c02 | dell-b01 | smc-b02 | xcc-b05 | ilo-c01 | gb300-a01 | generic-c02-ipmi | dell-b01-ipmi | gb300-a01-ipmi |
|-----------|-------------|----------|---------|---------|---------|-----------|------------------|---------------|----------------|
| create    | PASS        | PASS     | PASS    | PASS    | PASS    | PASS      | PASS             | PASS          | PASS           |
| query     | PASS        | PASS     | PASS    | PASS    | PASS    | PASS      | PASS             | PASS          | PASS           |
| power.off | PASS        | PASS     | -       | PASS    | PASS    | PASS      | PASS             | PASS          | -              |
| power.on  | PASS        | PASS     | -       | PASS    | PASS    | PASS      | PASS             | PASS          | -              |
| cycle     | -           | -        | PASS    | -       | -       | -         | -                | -             | PASS           |
| poll.off  | PASS        | PASS     | -       | PASS    | PASS    | PASS      | PASS             | PASS          | -              |
| poll.on   | PASS        | PASS     | -       | PASS    | PASS    | PASS      | PASS             | PASS          | -              |
| bmc.lost  | PASS        | PASS     | -       | PASS    | PASS    | PASS      | PASS             | PASS          | -              |

55 PASS, 0 FAIL
```

**MAAS itself controls the twin's BMCs: 55 of 55.** Through MAAS's API, over Redfish (generic,
Dell, Supermicro, XCC, iLO 5, GB300 at `node_id=System_0`) and IPMI (generic, Dell, GB300):

- **query**: `query-power-state` answers the model's state; the twin logs MAAS's reads (Redfish
  `GET` of the system; IPMI a RMCP+ session and Get Chassis Status).
- **off / on**: `power-off` and `power-on` reach the twin as `power.off.force` (+ a one-time PXE
  PATCH) and `boot.override` + `power.on` over Redfish, `power.off.force` and the
  `ipmi-chassis-config` boot flags + `power.on` over IPMI; the model follows, and MAAS records
  `off`/`on` in 11–16 s.
- **cycle**: commissioning a New machine that is on makes MAAS power-cycle it (its CYCLE workflow):
  the twin logs `power.off.force`, then the PXE override and `power.on`, within 9–12 s; MAAS then
  waits for the commissioning image to boot, which a `model` host never does, and `machine abort`
  powers it off again (status back to New).
- **polling**: a power change made in the twin, which nobody tells MAAS about, reaches MAAS's
  record by itself in 253–312 s (the rack's power monitor; five minutes since the last query),
  through reads only: MAAS writes nothing back.
- **BMC loss**: with `generic/bmc/unreachable`, `query-power-state` fails (the API's 60-second
  wait ends in `TimeoutError`; maas-agent logs `Error executing power command … ActivityType=power-query
  … TaskQueue=agent:power@vlan-5001`) and the machine's `power_state` becomes `error`; once the
  fault is cleared, a query answers `on` again.

The twin's log of two of the machines (reads omitted; the source is the BMC relay, the principal the
machine's BMC user):

```
c02/server-01 (Redfish)
#221  power.off.force redfish POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset   admin   (machine power-off)
#229  boot.override   redfish PATCH /redfish/v1/Systems/1                               admin
#346  boot.override   redfish PATCH /redfish/v1/Systems/1                               admin   (machine power-on)
#348  power.on        redfish POST /redfish/v1/Systems/1/Actions/ComputerSystem.Reset   admin
#582  act user power.off.force control POST /v1/power                                           (dctwin power off; MAAS polls, writes nothing)
#635  act user power.on control POST /v1/power
#690  fault inject generic/bmc/unreachable;  #709 fault clear
c02/server-02 (IPMI)
#237  power.off.force ipmi Chassis Control: power down                                  admin   (machine power-off)
#322  boot.override   ipmi Set System Boot Options: parameter 5: no override            admin   (machine power-on)
#324  boot.override   ipmi Set System Boot Options: parameter 5: Pxe Once Legacy        admin
#335  power.on        ipmi Chassis Control: power up                                    admin
```

**The container (`run.sh`, `STAGES="drivers maas"`).** The same packages, started by hand without
systemd, bring up regiond, the v3 API server, Temporal and its worker, rackd (with the BMC subnet)
and maas-agent, import the image and accept the machines, but power actions never complete there:
the region's Temporal worker starts `PowerQueryWorkflow`, the agent never runs its activity, and
the API answers `504` after 60 s. maas-agent's own `configure-agent` workflow fails in the
container (`Workflow timeout (type: StartToClose)`; its DHCP step restarts `maas-dhcpd` through
systemd, which is absent). In the guest, where the snap runs every service under systemd, the
agent configures itself and serves `agent:power@vlan-5001`. So the container is for stage 1.

## Findings

### Twin bugs

1. **Fixed after this run: a restart passed through Off, so MAAS reset a host up to four times.**
   A warm restart now keeps the host On while it resets (`warmReset` in `internal/engine/power.go`);
   `TestWarmRestartKeepsPowerOn` (`internal/engine`) shows power staying On through both restarts;
   the stage-1 re-run passes all six `reset` rows, one `power.restart` per reset. As found: the engine
   (`internal/engine/power.go`, `powerLocked`) treats `power.restart` and `power.restart.graceful`
   (Redfish `ForceRestart`/`GracefulRestart`, IPMI hard reset) like `power.cycle`: On →
   PoweringOff → Off → PoweringOn → On. A warm restart keeps the platform powered and a BMC keeps
   reporting `PowerState: On`; only `PowerCycle` passes through Off. MAAS's Redfish `reset` reads
   PowerState a second later, maps `Off` and `PoweringOn` to `off` (`_power_query`) and resets again,
   so the twin's log shows two to four `power.restart` for one reset (the `reset` row; evidence:
   `power: On → PoweringOff → Off → PoweringOn`, then the next `power.restart`, each within about a
   second). Metal³ is not affected (Ironic reboots with ForceOff and On); Rufio's IPMI reset
   (`compat/tinkerbell`) makes the same transition, and its check only asks that the host end On.
2. **IPMI boot option parameter 3** (fixed in `internal/adapter/ipmi`, see
   [compat/metal3](../metal3/)): `ipmi-chassis-config --commit`, which MAAS's IPMI driver runs before
   every power-on, writes parameter 3 with the boot flags. Against the twin before the fix (built with
   `go build -overlay` of the old `commands.go`), MAAS 3.7.3's `maas.power on ipmi` logged
   `Failed to change the boot order to PXE <bmc>: ERROR: Section post-commit
   `Chassis_Boot_Flags'` and powered the host on anyway; with the fix the same command logs nothing
   and the twin records `parameter 3: valid bit clearing 0x1f`.

### MAAS behaviour the twin surfaced (not twin bugs)

1. MAAS's Redfish driver sets a one-time PXE boot after every power-off and before every power-on,
   so a machine MAAS has touched always has an override pending.
2. With MAAS's default `power_boot_type` (`auto`), the IPMI driver writes the boot flags without
   the EFI bit: the twin records the override as `Legacy`. A UEFI-only host needs
   `power_boot_type=efi`.
3. MAAS 3.7.3's default IPMI cipher suite is 3 (`maaslog: using a non-secure cipher suite id` on
   every call); the twin's BMCs offer 3 and 17.
4. MAAS's Redfish `power_query` maps `PoweringOn` to `off` and `PoweringOff` to `on`, which is
   what turns finding 1 into repeated resets.

## Not run, and why

| Item | Why |
|---|---|
| Commissioning, deployment, enlistment by PXE | The machine must boot MAAS's ephemeral image and report back; hosts are `model` fidelity, and MAAS's own DHCP (maas-dhcpd) is not wired to the twin's relay. The power cycle that starts commissioning is shown (stage 2, `cycle`) |
| The snap in a container | Needs snapd and systemd; the snap runs in the KVM guest instead (stage 2) |
| BMC discovery by MAAS (`maas … chassis`, Redfish probing) | Not exercised |

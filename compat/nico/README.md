# NICo against the twin

This harness runs **NVIDIA Infra Controller (NICo)**, the bare-metal controller of NVIDIA DSX OS
(`dsx-ai-factory/infra-controller`, Apache-2.0), **unmodified**, natively on one host against the
twin's emulated datacenter, and records what NICo does. Before it, only NICo's Redfish client
library (`libredfish`, [`../libredfish`](../libredfish)) had been run against the twin.

NICo is pinned at `02b92f18fed776c8ef0056803338707c5e6bcb6b` (main, read 2026-10-18). Every NICo
component runs from that source as built; one file of one component needs a two-function patch
to compile on macOS, and on Linux nothing is patched ([below](#the-one-macos-patch)).

## Result (2026-10-19, macOS 26 on Apple silicon, Rust 1.98.1)

`run.sh all`, from empty containers and a fresh twin: **22 PASS, 0 FAIL, 3 KNOWN**
([run.sh's own table](#runsh-passfail-table)). On x86_64 Linux, `run.sh ready` drives a DGX H100
virtual machine through NICo's whole host lifecycle to **Ready**: **16 PASS, 0 FAIL, 1 KNOWN**
([the Ready path](#the-ready-path-a-zero-dpu-host-that-boots-nicos-scout)). NICo runs with its deployment defaults where the
twin can serve them: DPU-managing ingestion (`dpu_policy = "manage"`) and the Rack Management
Service as the NVLink-switch and power-shelf backend (the twin's RMS adapter).

| # | Check | Result | Evidence |
|---|---|---|---|
| 1 | NICo's BMC classifier (`bmc-explorer-cli`) on one twin BMC of each kind | **PASS**: through libredfish all 13 explored, each of the 12 vendor BMCs as its intended vendor (the generic DMTF server is `Unknown`); the site explorer's default client (`nv-redfish`) explores **8 of 13** (it rejects two values of the captured GB200 and Supermicro trees, [NICo-side findings](#nico-side-findings)) | [classification](#1-bmc-explorer-cli) |
| 2a | NICo's `forge-dhcp-server` leases every BMC through the twin relay | **PASS**, 68 of 68 BMC ports, each from the per-rack underlay segment its option 82 link selection names; the NVSwitch trays' NVOS management ports lease beside their BMCs | [DHCP](#2-discovery-through-nicos-own-dhcp-server) |
| 2b | NICo's site explorer reaches each BMC at its leased address (`https://10.30.x.y`) and explores it | **PASS**, 58 of 58 Redfish BMCs; the 10 PDUs refused as non-BMCs (`MissingVendor`) | [explored endpoints](#explored-endpoints) |
| 2c | Serials NICo reads equal the twin's model | **PASS** for all 40 non-GB BMCs (18 DPUs, 9 NVSwitch trays, 8 power shelves, 5 OEM servers); **KNOWN**: the 18 GB300 trays report the HGX baseboard's serial through `libredfish`, the fallback NICo uses because `nv-redfish` rejects the GB200 capture | [explored endpoints](#explored-endpoints) |
| 2d | NICo rotates BMC credentials and runs preingestion | **PASS**: `account.password` on 58 BMCs, lockout policy, BMC resets, power cycles, Dell iDRAC attributes and BIOS jobs | [what NICo asked the twin](#what-nico-asked-the-twin) |
| 2e | NICo creates machines, switches, power shelves and the rack | **PASS**: all **18 GB300 trays are managed hosts paired with their BlueField-3** (36 machines), driven by NICo into DPU discovery (`DISABLESECUREBOOT`, `REBOOTALLDPUS`, `SETUEFIHTTPBOOT`); **9 of 9 NVLink switches `ready`** (NVOS found at its mgmt0 lease); **8 of 8 power shelves `ready`** (through RMS); rack `a03` | [machines](#machines-shelves-switches-rack) |
| 3a | In-tray leak → NICo health report | **PASS**: `TrayLeak, SensorCritical, PreventAllocations, Hardware` on the NVSwitch tray; with two trays, rack `a03` alerts (`RackLeak`) | [faults](#3-faults) |
| 3a' | In-tray leak → NICo acts | **PASS**: NICo Flow submits a force power-off; NICo core sends it to the twin's RMS (`BatchSetPowerState`), and the twin powers the leaking switch tray off (the state change chained to NICo's request) | [faults](#3-faults) |
| 3a'' | Two leaking trays → nico-api asks the BMS to isolate the rack | **PASS**: nico-api publishes `RackElectricalIsolationRequest=1` on the topic its metadata names, and the twin's BMS opens rack `a03`'s power and closes its liquid supply. **KNOWN**: nico-api does not release the rack after the leaks clear (the cleared detectors sit behind the BMCs isolation unpowered) | [faults](#3-faults) |
| 3b | BMC unreachable → NICo records it | **PASS**: `NICO-SITEEXPLORER-102 the endpoint was not reachable` | [faults](#3-faults) |
| 3c | Power-off → NICo acts | **PASS**: an explored, uningested host (a Dell) powered off on the twin is powered back on by the site explorer; an ingested host is its state machine's | [faults](#3-faults) |
| 4 | BMS rack leak point → `dsx-exchange-consumer` → NICo rack alert | **PASS**: rack `a03` `Alerting BmsLeakDetectRack`; the clear removes it. **KNOWN**: the consumer also removes it early, on a sibling point's periodic Clear (a NICo consumer bug) | [consumer](#4-dsx-exchange-consumer) |

The first runs (2026-10-18; 19 PASS, 3 KNOWN with two harness workarounds) found five twin bugs and
gaps ([below](#twin-bugs-and-missing-features)): frozen BMC clocks, GB-tray DPUs that were not the
model's, a Dell BIOS PATCH without a job, a third DSX Exchange metadata shape in nico-api, and no
NVOS on the switch trays' management ports. All five are fixed in the twin and the workarounds are
gone; what each fix is, and its test, is under each item. Fixing them let NICo get further, which
found one more: every BlueField reported the capture's host PF MAC, so NICo created one GB host
and refused the rest as duplicates; it too is fixed. Findings about NICo itself are listed
separately ([NICo-side findings](#nico-side-findings)).

## Running it

```bash
# prerequisites: cargo (rustup), go, docker, jq, openssl, curl, python3
RUSTUP_TOOLCHAIN=stable \
NICO_DHCP_PATCH=/path/to/dhcp-server-macos.patch \    # macOS only, see below
  compat/nico/run.sh all
```

`all` is `fetch build classify up nico load dhcp discover health faults consumer report down`;
each step can be run on its own. Everything goes under `WORK` (default `$TMPDIR/dctwin-nico`):
the NICo source, the Rust target directory (about 10 GB), binaries, state, logs (`logs/`) and the
evidence files (`out/`). The first build takes about 50 minutes on a 10-core laptop; binaries
already in `$WORK/bin` are not rebuilt. On a slow link, `VAULT_BINARY` (a linux vault binary run in
`alpine:3.21`) and `TEMPORAL_BIN` avoid the two large downloads.

The Ready path ([below](#the-ready-path-a-zero-dpu-host-that-boots-nicos-scout)) runs on x86_64
Linux with KVM, QEMU, OVMF and swtpm (with `swtpm_setup` and GnuTLS's `certtool`), rootless
Podman or Docker, ipmitool, and passwordless sudo for one network namespace:

```bash
NICO_SRC=/path/to/infra-controller compat/nico/run.sh ready
```

`ready` is `fetch boot build up pxe nico load dhcp host report down`. `boot` builds what is missing
under `BOOT_ARTIFACTS` (default `$WORK/boot`) with `boot-artifacts.sh` in a `dctwin-nico-build`
container (`boot-artifacts.sh down` removes it and its volume, about 7 GB: NICo's Rust target, the
toolchain, iPXE's tree); `host` waits up to `READY_WAIT` (5400 s) for the host to reach Ready and
gives up when one state has held for `READY_STALL` (1800 s).

Ports (all loopback, all overridable): nico-api 11079 (metrics 11080), Postgres 55432, Vault 18201,
NICo DHCP 26767/udp, site gateway 18443, Flow 15051, Temporal 17233–17235, metrics 19009/19010/16768.
Containers: `dctwin-nico-pg` (Postgres 16, TLS) and `dctwin-nico-vault` (Vault 1.20.2, dev mode);
`run.sh down` removes them and stops every process it started.

## What runs, and how it is wired

```
 twin (dctwin up nvl72-nico.yaml: redfish, dhcp, bms, nvue, rms)  NICo, natively on the host
 ───────────────────────────────────────────────────              ─────────────────────────────
 68 BMCs ── DHCP ─▶ twin relay (option 82: circuit id,  ─udp─▶  forge-dhcp-server (controller)
                    remote id, link selection 10.30.N.1,            │ DiscoverDhcp (gRPC, mTLS)
                    relay source port)                  ◀─udp──     ▼
 BMC Redfish (one per BMC) ◀── site gateway (CONNECT) ◀─HTTPS─ nico-api: site explorer,
                                 10.30.N.x:443                      preingestion, machine/switch/
                                                                    shelf/rack state controllers
 BMS publisher (mochi MQTT) ──▶ BMS/v1/PUB/… and the        ─▶ nico-api DSX Exchange bus
                               nico-api mirror              ◀── nico-api's isolation requests
                               nico-legacy mirror           ─▶ carbide-dsx-exchange-consumer
                                                                 forge-hw-health ─HTTPS via gateway
                                                                 NICo Flow + Temporal (leak policy)
                                                                 Postgres, Vault (containers)
```

- **Addresses come from NICo.** The estate's BMC network is `dhcp: stack`; the twin relay of each
  rack forwards to NICo's `forge-dhcp-server` with option 82. `nicoinputs` gives nico-api one
  `underlay` segment per twin rack (a /24 at the rack ordinal, gateway `.1`, from
  `internal/adapter/dhcp.PlanSegments`), so NICo picks the segment by the relay's link selection
  and allocates from it. NICo's server answers `giaddr` at a fixed `--relay-response-port`, so
  that is set to the twin relay's upstream port.
- **NICo reaches BMCs at their leased addresses** through the twin's site gateway:
  `HTTPS_PROXY=http://127.0.0.1:18443` for nico-api and forge-hw-health (both of NICo's Redfish
  clients, libredfish and nv-redfish, honour it). No `bmc_proxy` is configured.
- **What the operator loads**, from the twin's `stack-inputs/expected-machines.json` and the floor
  plan (`nicoinputs`): expected machines (compute trays, servers; factory credentials, chassis
  serial, rack), expected power shelves, expected switches (with the NVOS management port MAC,
  the file's `nvosMACs`), an expected rack group and rack for the NVL72 rack, and a rack profile.
- **Configuration** (`nico-api-config.toml.in`) is NICo's `dev/mac-local-dev` configuration with:
  `site_explorer.explore_mode = "compare-result"` (both of NICo's Redfish clients; the default
  `nv-redfish` rejects two captured trees, see [NICo-side findings](#nico-side-findings));
  `site_explorer.dpu_policy = "manage"` (NICo's default; `DPU_POLICY=ignore` ingests zero-DPU
  hosts); `compute_tray_backend = "core"`, and RMS for the NVLink-switch and power-shelf backends,
  as NICo's deployment defaults them, at the twin's RMS (`[rms] api_url` from the twin's
  `rms.json`, TLS against the estate CA; `COMPONENT_BACKEND=mock` uses NICo's mock instead); the
  DSX Exchange event bus on the twin-hosted broker.
- **Service identities.** nico-api only issues BMC credentials to, and admits load from, SPIFFE
  service identities, so forge-hw-health, Flow and the consumer get client certificates with
  `spiffe://nico.local/forge-system/sa/<service>` from the dev CA `gen-certs.sh` makes.
- **Postgres over TLS** (nico-api refuses plain connections), Vault in dev mode with the secrets
  NICo's `run-nico-api.sh` writes (site-wide BMC root `root`/`vault-password`, UEFI defaults, PKI).
- **Toolchain.** NICo pins Rust 1.97.1; it builds with stable 1.98.1 when the workspace's
  `-D warnings` is dropped (`RUSTFLAGS` repeats NICo's flags without it). No other change.

### The one macOS patch

`forge-dhcp-server` binds its socket to an interface with `socket2::Socket::bind_device`
(SO_BINDTODEVICE), which socket2 defines on Linux only, so the crate does not compile on macOS:

```
error[E0599]: no method named `bind_device` found for struct `Socket` in the current scope
   --> crates/dhcp-server/src/util.rs:159:37   (and :231:12)
```

The patch keeps the Linux call and, on other platforms, binds by interface index
(`IP_BOUND_IF`/`IPV6_BOUND_IF`, socket2's `bind_device_by_index_v4/v6`) using the crate's own
`if_index_for`. It is kept out of this repository; save it as a file and pass `NICO_DHCP_PATCH`:

```diff
--- a/crates/dhcp-server/src/util.rs
+++ b/crates/dhcp-server/src/util.rs
@@ -156,7 +156,7 @@
         let mut retries_left = INTERFACE_BIND_ATTEMPTS;
         while retries_left > 0 {
-            let Err(error) = socket.bind_device(Some(interface.as_bytes())) else {
+            let Err(error) = bind_socket_to_interface_v4(&socket, &interface) else {
                 break;
             };
             retries_left -= 1;
@@ -228,7 +228,7 @@
     socket.set_reuse_address(true)?;
     socket.set_only_v6(true)?;
     socket.set_nonblocking(true)?;
-    socket.bind_device(Some(interface.as_bytes()))?;
+    bind_socket_to_interface_v6(&socket, interface)?;
 
     // SO_BINDTODEVICE scopes this wildcard listener to the selected interface.
     // The interface index below selects the same interface for multicast.
@@ -248,7 +248,29 @@
     }
 
     Ok(UdpSocket::from_std(socket.into())?)
+}
+
+// dc-twin compat (macOS only): socket2 offers SO_BINDTODEVICE (`bind_device`) on Linux only.
+// macOS scopes a socket to an interface with IP_BOUND_IF / IPV6_BOUND_IF, which socket2 exposes
+// as `bind_device_by_index_v4/v6`. Linux keeps NICo's original call unchanged.
+#[cfg(any(target_os = "android", target_os = "fuchsia", target_os = "linux"))]
+fn bind_socket_to_interface_v4(socket: &socket2::Socket, interface: &str) -> std::io::Result<()> {
+    socket.bind_device(Some(interface.as_bytes()))
+}
+#[cfg(not(any(target_os = "android", target_os = "fuchsia", target_os = "linux")))]
+fn bind_socket_to_interface_v4(socket: &socket2::Socket, interface: &str) -> std::io::Result<()> {
+    let index = if_index_for(interface).map_err(|e| std::io::Error::other(e.to_string()))?;
+    socket.bind_device_by_index_v4(std::num::NonZeroU32::new(index))
 }
+#[cfg(any(target_os = "android", target_os = "fuchsia", target_os = "linux"))]
+fn bind_socket_to_interface_v6(socket: &socket2::Socket, interface: &str) -> std::io::Result<()> {
+    socket.bind_device(Some(interface.as_bytes()))
+}
+#[cfg(not(any(target_os = "android", target_os = "fuchsia", target_os = "linux")))]
+fn bind_socket_to_interface_v6(socket: &socket2::Socket, interface: &str) -> std::io::Result<()> {
+    let index = if_index_for(interface).map_err(|e| std::io::Error::other(e.to_string()))?;
+    socket.bind_device_by_index_v6(std::num::NonZeroU32::new(index))
+}
 
 /// Resolve a Linux interface name to the scope index required by IPv6 sockets.
 fn if_index_for(interface: &str) -> Result<u32, DhcpError> {
```

The server then runs with `--interfaces lo0 --listen-addr 127.0.0.1:26767`. Nothing else in NICo
is changed for macOS.

## 1. bmc-explorer-cli

NICo's own classifier (`crates/bmc-explorer-cli`, the site explorer's exploration report for one
BMC) against one twin BMC of each kind in `examples/estates/lab-model.yaml`, at each BMC's loopback
endpoint with its factory account, once through each of NICo's Redfish clients (`classify.sh`):

| Twin BMC | Twin profile | libredfish: vendor / hardware class | nv-redfish (site explorer default) |
|---|---|---|---|
| a01/compute-tray-01 | nvidia/gb300-compute-tray | Nvidia / `nvidia_gb-bmc` | error: chassis collection, `UUID parsing failed` |
| a02/compute-tray-01 | nvidia/gb200-compute-tray | Nvidia / `nvidia_gb200-nvl` | error: chassis collection, `UUID parsing failed` |
| a01/switch-tray-01 | nvidia/nvlink-switch-tray | Nvidia / `nvidia_n5110-ld` | Nvidia / `nvidia_n5110-ld` |
| a01/compute-tray-01/bf3-0 | nvidia/bluefield-3 | Nvidia / `https-www-mellanox-com_bluefield-3-smartnic-main-card` | same |
| a01/power-shelf-01 | liteon/power-shelf-33kw | Liteon / `lite-on-technology-corp_nomodel` | Liteon / `unknown_nomodel` |
| b01/server-01 | oem-dell/xe9680-h100 | Dell / `dell-inc_poweredge-xe9680` | same |
| c01/server-01 | oem-dell/r760 | Dell / `dell-inc_poweredge-r760` | same |
| b02/server-01 | oem-supermicro/sys-821ge-tnhr | Supermicro / `supermicro_sys-821ge-tnhr` | error: firmware inventories, `the 'year' component could not be parsed` |
| c01/server-02 | oem-hpe/dl385-gen10p | Hpe / `hpe_proliant-dl385-gen10-plus-v2` | same |
| b05/server-01 | oem-lenovo/sr670-v2 | Lenovo / `lenovo_thinksystem-sr670-v2` | same |
| b03/server-01 | amd/instinct-mi355x-platform | Supermicro / `supermicro_as-8126gs-tnmr` | error: firmware inventories (as b02) |
| b04/server-01 | intel/gaudi3-hls | Supermicro / `supermicro_sys-822ga-ngr3` | error: firmware inventories (as b02) |
| c02/server-01 | generic/server-2u | Unknown / `dc-twin_generic-2u-server` | — / `dc-twin_generic-2u-server` |

The vendor is NICo's `BMCVendor`; the hardware class is NICo's `derive_hardware_class` (system
manufacturer and model, else the service root's vendor and product). For the GB300 tray libredfish
also reports the GB-family machine-setup diffs (`EmbeddedUefiShell`,
`Socket0Pcie6DisableOptionROM`), i.e. it chose its NVIDIA GB implementation. A PDU is refused
(`BMC has not provided systems`), as on a site. Through the site gateway (`HTTPS_PROXY`, leased
address `10.30.0.55`) the same CLI explores the NVSwitch tray and the Lenovo server unchanged.

## 2. Discovery through NICo's own DHCP server

NICo's dhcp-server log for one Lite-On shelf, relayed by the twin:

```
msg="Decoded DHCP packet" chaddr=00:0b:ab:bf:a3:29 giaddr=127.0.0.1 message_type=discover source_address=127.0.0.1:24333
msg="Sent DHCP reply" chaddr=00:0b:ab:bf:a3:29 destination_address=127.0.0.1:24333 message_type=ack siaddr=10.90.0.17 yiaddr=10.30.2.10
msg="returning cached response" circuit_id="Some(\"a03-mgmt-switch-1:eth16\")" link_address=10.30.2.1 vendor_id="unknown: 'dc-twin'"
```

and the twin: `network oob (management), dhcp stack — relayed to the stack's DHCP at
127.0.0.1:26767; 68 of 68 BMC ports leased`. `nico-admin-cli network-segment show` lists the six
rack segments `oob-a03 10.30.2.0/24`, `oob-a10 10.30.9.0/24`, `oob-b01…b04 10.30.16–19.0/24`.

### Explored endpoints

`nico-report.py` joins `nico-admin-cli site-explorer get-report all` with the twin's model
(`out/explored-vs-twin.txt`; excerpt, `compare-result` mode):

```
twin device                NICo lease   type     vendor      NICo serial           = twin  result
a03/compute-tray-01        10.30.2.40   Bmc      nvidia      2893313701446         no      ok   (HGX_Baseboard_0's serial)
a03/compute-tray-01/bf3-0  10.30.2.41   Bmc      nvidia      MT6811356LYX          yes     ok
a03/power-shelf-01         10.30.2.10   Bmc      liteon      6133547RXX74X26938UG  yes     ok
a03/switch-tray-01         10.30.2.14   Bmc      nvidia      MT9064301IJO          yes     ok
b01/server-01              10.30.16.11  Bmc      dell        OT80502               yes     ok
b02/server-01              10.30.17.12  Bmc      supermicro  S875020X1796162       yes     ok
b03/server-01              10.30.18.11  Bmc      lenovo      J874896C              yes     ok
b04/server-01              10.30.19.10  Bmc      dell        EN02522               yes     ok
b04/server-02              10.30.19.11  Bmc      hpe         MXQ186583Q            yes     NICO-SITEEXPLORER-102 (fault 3b)
b01/pdu-a                  10.30.16.12  Unknown  -           -                     -       NICO-SITEEXPLORER-122 MissingVendor
68 BMCs leased by NICo; explored ok 57, exploration error 11; 23 managed hosts identified
```

With NICo's default `nv-redfish` mode the 18 GB300 trays fail with `NICO-SITEEXPLORER-130 …
UUID parsing failed` and the Supermicro server with `… the 'year' component could not be parsed`
(39 explored); `compare-result` falls back to libredfish for them.

### What NICo asked the twin

Writes from NICo in the twin's event log (`twin-requests.py --writes`, first 30 minutes):

```
  bmc.reset                             131  on 50 devices: bf3 54, compute-tray 36, switch-tray 27, server 14
  bios.set                               67  on 5 devices: server 67
  power.on                               63  on 32 devices: bf3 36, switch-tray 18, server 8, compute-tray 1
  account.password                       58  on 58 devices: compute-tray 18, bf3 18, switch-tray 9, power-shelf 8, server 5
  POST …/Oem/Dell/DellJobService/Actions/DellJobService.DeleteJobQueue     54  on 2 devices
  account.policy                         38  on 38 devices: compute-tray 18, switch-tray 9, power-shelf 8, server 3
  power.off.force                        31  on 31 devices: bf3 18, switch-tray 9, server 4
  session.create                         17  on 17 devices: switch-tray 9, power-shelf 8
  power.restart                          11  on 5 devices: server 11
  PATCH /redfish/v1/Managers/{id}       581  (the harness's clock keeper, twin gap 1)
```

Per device, e.g. an NVSwitch tray: `account.password` (`PATCH …/AccountService/Accounts/root`,
the factory password rotated to the site password), `account.policy` (lockout threshold and
duration), `bmc.reset`, then NICo preingestion's time-sync reset: `power.off.force`, `bmc.reset`,
`power.on`. A Dell server additionally gets `PATCH …/Oem/Dell/DellAttributes/iDRAC.Embedded.1`,
`DeleteJobQueue` and `PATCH …/Bios/Settings` (`bios.set`). hw-health creates Redfish sessions
(`session.create`) with the session tokens nico-api mints for it.

### Machines, shelves, switches, rack

The final run (2026-10-19):

```
18 GB300 trays   managed hosts, each paired with its BlueField-3 (36 machines): DPUDISCOVERING
                 (5 DISABLESECUREBOOT, 5 REBOOTALLDPUS, 8 SETUEFIHTTPBOOT), NICo driving the DPU
                 BMCs over Redfish (Secure Boot, UEFI HTTP boot, DPU reboots)
switches         a03-switch-tray-01…09   {"state":"ready"}   NVOS at its mgmt0 lease (D46)
power shelves    a03-power-shelf-01…08   {"state":"ready"}   through RMS
OEM servers      explored, credentials rotated, preingestion (Dell BIOS jobs), powered by NICo; no
                 machine on this estate (a zero-DPU host waits for a boot NIC that DHCPs to NICo)
rack a03         Created; 8 power shelves, 9 NVLink switches; profile GB300_NVL72R1_C2G4_NVIDIA
```

The first runs had the five OEM servers in `HostInitializing` (with `dpu_policy = "ignore"`, the
Dells stopped at `machine_setup failed: Remote returned empty header`, twin bug 3), the switches
in `initializing/WaitForOsMachineInterface` (twin gap 5) and no GB300 tray created. With the DPU
binding fixed but every BlueField reporting the capture's host PF MAC, NICo created one GB host
and refused the rest as duplicates (twin bug 6).

## 3. Faults

| Injected on the twin | What NICo recorded | What NICo did |
|---|---|---|
| `generic/leak/tray-detector` on `a03/switch-tray-03` | forge-hw-health: 7 `LeakDetection` alerts (`…/LeakDetectors/leakage1` … `reports critical`), then the switch's health source `hardware-health.tray-leak-detection`: `BmcLeakDetection`, classifications **`TrayLeak, SensorCritical, PreventAllocations, Hardware`** | NICo Flow's leak job: `Found 1 leaking switch IDs` → `Power-off task submitted for leaking component` → Temporal `PowerControl` → `NVSwitch power control … op ForcePowerOff via NICo` → NICo core: `power control failed …: NVOS MAC or IP not available` (no request reaches the tray) |
| the same on `a03/switch-tray-05` (2 trays) | `Rack leak detected: 2 leaking trays reached threshold 2` (`RackLeak, SensorCritical, PreventAllocations`); `rack health-history a03`: `Alerting BmcLeakDetection` | the rack isolation request over DSX Exchange is not sent: nico-api's BMS integration rejects the twin's metadata (twin gap 4, since fixed by the `nico-api` mirror; `run.sh` now checks the request and the isolation, not yet run) |
| `generic/bmc/unreachable` on `b04/server-02` | `NICO-SITEEXPLORER-102 the endpoint was not reachable … error sending request for url (https://10.30.19.11/redfish/v1/)` | retries each run |
| `dctwin power off a03/compute-tray-07` (explored, not ingested) | — | site explorer powers it on 0.9 s later: `POST /redfish/v1/Systems/System_0/Actions/ComputerSystem.Reset` (`power.on`) |
| `dctwin power off b03/server-01` (ingested, `HostInitializing`) | — | nothing within 2 minutes (NICo does not remediate ingested hosts from the site explorer) |

Clearing the leak faults returns the rack to `Healthy` in its health history.

## 4. dsx-exchange-consumer

`carbide-dsx-exchange-consumer` on the twin-hosted broker, with the estate's stack output
`bmsConsumerMirror: nico-legacy` (`run.sh` now sets `"nico-legacy,nico-api"` in its copy of the
estate, `BMS_MIRRORS`, so nico-api reads the twin's BMS too: twin gap 4);
`generic/leak/rack-point` on `a03/leak-point-01`:

```
consumer: msg="Leak alert detected, inserting health override" point_path=/PUB/Rack/LeakDetectRack/site-1/hall-a/a03/leak-point-01/leak point_type=LeakDetectRack rack_id=a03 value=Faulting
NICo:     rack health-history a03 → 2026-10-18T18:31:09Z  rack-aggregate-health  Alerting  BmsLeakDetectRack
clear:    msg="Leak cleared, removing health override" … value=Clear
```

No power request followed (NICo documents that a BMS leak powers nothing off). The AsyncAPI-shaped
messages the twin also publishes are ignored by the consumer (`No registered pattern matched topic`),
as the stack document says.

The alert does not last until the clear, though. The twin's BMS re-publishes every point's value
every 1 min 40 s (the AsyncAPI cadence), and rack `a03` has four leak points (two leak detectors,
two sensor faults). The consumer keeps **one** override per rack (source `dsx-exchange-consumer`)
and removes it on any Clear it has not yet recorded; a removal that fails (`rack override with
source not found`) is not recorded, so it is retried on every re-publication. The next Clear of a
sibling point therefore removes the alert while `leak-point-01` is still leaking:

```
msg="Leak alert detected, inserting health override" point_path=/PUB/Rack/LeakDetectRack/site-1/hall-a/a03/leak-point-01/leak … value=Faulting
msg="Leak cleared, removing health override" point_path=/PUB/Rack/LeakSensorFaultRack/site-1/hall-a/a03/leak-point-0… value=Clear
```

In the second run the override was gone before the rack's health history recorded it. This is
NICo's consumer (`health_updater.rs`), not the twin; a real BMS that re-publishes values does the
same.

## The Ready path: a zero-DPU host that boots NICo's scout

`run.sh ready` drives one host through NICo's whole host lifecycle on the twin: a **DGX H100 at
`vm` fidelity** (`examples/estates/dgx-nico-ready.yaml`): a real x86_64 virtual machine under KVM
behind its emulated BMC (NICo's captured DGX H100 tree, NICo's "Viking"), whose UEFI firmware
network-boots whatever NICo serves it, with no DPU (so NICo is told to ingest it as a zero-DPU
host). Linux only, with sudo for one network namespace.

### Result (2026-10-20, x86_64 Linux with KVM)

`run.sh ready`, from empty containers, a fresh twin and an empty NICo database: **16 PASS, 0 FAIL,
1 KNOWN** ([run.sh's own table](#runsh-passfail-table)). The DGX H100 is **Ready** 4 min 47 s after
NICo created it (created 01:43:27, Ready 01:48:14 UTC, NICo's own record of the machine), and
everything on the way is NICo's doing against the twin: its DHCP, its iPXE and scout, its state
machine, its Redfish and IPMI calls to the DGX's BMC.

| Check | Result | Evidence |
|---|---|---|
| The operator brings the DGX to the CPLD NICo pairs it at (`operator-cpld`) | **PASS** | Redfish `SimpleUpdate` of `CPLDMB_0` 0.2.1.8 → 0.2.1.9 through the twin's BMC, applied at a DC power cycle (ForceOff, On), as an operator would ([NICo-side findings](#nico-side-findings)) |
| NICo's DHCP leases the BMC; the site explorer creates a zero-DPU host (`dhcp`, `ready-machine`) | **PASS** | the BMC leased 10.30.16.10 through the twin relay; NICo explored it, rotated its password, reset it, and created predicted host `fm100psdhp…` with no DPUs, its interfaces the VM's NICs as the BMC reports them |
| NICo's ipmitool reaches the BMC (`ipmi`) | **PASS** | `lanplus`, cipher suite 17, at the BMC's lease in the namespace; NICo reset the host this way four times |
| **DHCP through the relay to NICo** (`ready-dhcp`) | **PASS** | the guest leased 10.40.16.10 from NICo's dhcp-server (server id 10.90.0.16) through the twin relay at `leaf-b01-1:eth1` (circuit id `b01-leaf-b01-1:eth1`), at every boot; NICo's answers by the client's vendor class: `PXEClient` 9 (the firmware's PXE client, given no boot file, and iPXE), `HTTPClient` 3 (the firmware's HTTP boot client, given `http://10.90.0.17:8080/public/blobs/internal/x86_64/ipxe.efi`), none 3 (scout's OS); 7 lease events on the port |
| **The iPXE/HTTP fetches** (`ready-boot`) | **PASS** | carbide-pxe served `ipxe.efi`, iPXE's boot script (`/api/v0/pxe/boot?…&serial=1337618079559`), `scout.efi` (124 MB) and the root CA, three times each, every request from 10.40.16.10, the guest's own lease: NICo found the host's interface by the peer address and handed it scout with `machine_id=<interface id>` |
| **scout's discovery report** (`ready-discovery`) | **PASS** | `DiscoverMachine` from 10.40.16.10: SMBIOS product, board and chassis serial 1337618079559, the BMC's Redfish serial and the model's; `NVIDIA DGXH100`; the boot NIC 3c:6d:66:26:cc:b8 among scout's NICs; the TPM's EK certificate reported and its CA found (the estate CA, loaded with `tpm-ca add`, `tpm-ca`); the predicted host became stable host `fm100ht5s0…` |
| **Machine validation** (`ready-validation`) | **KNOWN** | NICo entered `Validation/MachineValidation` and skipped it: `[machine_validation_config] enabled = false` is NICo's default (its test suites run NVIDIA's driver, DCGM and GPU stress on GPUs the VM does not have) |
| **The state machine to Ready** (`ready-state`) | **PASS** | below, from NICo's record (`out/host-state-history.txt`) |

NICo's record of the host, with what the twin saw it do (`out/host.json`, `out/host-events.txt`):

| UTC | NICo's state | What NICo did, as the twin recorded it |
|---|---|---|
| 01:43:27 | `ConfigureAstra/EnableNics`, `DpuDiscoveringState` | nothing to enable (no CX9 NIC), no DPU to discover: on to host init. The host had already booted scout once (01:42:47), which registered it as soon as the site explorer created it |
| 01:43:58 | `HostInit/WaitingForPlatformConfiguration` | BIOS attributes (`PATCH /Systems/DGX/Bios/SD`), then `chassis power reset` over IPMI; the twin applied them at the reset |
| 01:44:00 | `PollingBiosSetup` | BIOS verified; no boot-order change, as NICo now knew the host for a DGX H100 ([NICo-side findings](#nico-side-findings)) |
| 01:44:02 | `Measuring`, `SpdmMeasuring` | attestation and SPDM off (NICo's defaults): passed through |
| 01:44:07 | `WaitingForDiscovery`, `WaitingForCleanup` | the reset's boot: the firmware's PXE, then HTTP boot of NICo's iPXE, which chained scout (kernel at 01:45:32); scout's storage cleanup ("No NVMe devices found to clean") |
| 01:45:39 | `WaitingForDiscovery` | IPMI reset; scout again (01:47:11), registered |
| 01:47:53 | `UefiSetup` (4 sub-states) | the UEFI password (`Bios.ChangePassword`), IPMI reset |
| 01:48:02 | `WaitingForLockdown` | lockdown BIOS attributes, IPMI reset |
| 01:48:07 | `BomValidating/MatchingSku`, `Validation/MachineValidation`, `Discovered` | SKU matching, validation skipped |
| 01:48:14 | **`Ready`** | |

In the run before (run 5 below), scout had not registered the host when its host init began, and
NICo set the boot order: `SetBootOrder` sent `PATCH /Systems/DGX/SD` with `Boot0004` ("UEFI HTTPv4
(MAC:3C6D6626CCB8)", found by its alias, device path and MAC) first, then an IPMI reset; the twin
wrote the order into the VM's NVRAM and cold-started it (`nvram.bootOrder` Boot0004 first), NICo
verified it, and with HTTP boot first every later boot skipped the PXE attempt: Ready 2 min 17 s
after creation.

#### The runs that got there

Each stopped somewhere new; what stopped it was fixed in the twin, in the harness's configuration
of NICo, or is NICo's and recorded as a finding:

1. **NICo created no host** (2026-10-19): its site explorer pairs a DGX H100 only at CPLD 0.2.1.9
   and the captured DGX's is 0.2.1.8. The operator's update step, and twin bug 11.
2. **"rack b01 has no rack_profile_id for RMS slot and tray lookup"**: with an RMS configured NICo
   requires a rack profile of every expected machine's rack. The Ready path configures no RMS.
3. **Every IPMI restart failed** ("Could not open socket!"): NICo restarts a DGX through IPMI, which
   no proxy carries; the IPMI relay. The host then waited in `SetBootOrder` for a boot NIC the BMC
   did not report (twin bug 12), and reading the DGX implementation NICo was about to call showed it
   would not have found the boot options it reorders (twin bug 13).
4. **scout registered, and every call after it was refused** (nico-api: "invalid peer certificate:
   UnknownIssuer"): Vault issued scout's machine certificate under a root of its own. The Ready
   path's Vault issues under the site CA. With that applied to the run's Vault while it ran, the
   host reached Ready 22 minutes after it was created, 15 of them NICo's wait before its next reboot.
5. **Ready** 2 min 17 s after creation, by way of `SetBootOrder`; then the final run (the tables
   above), 4 min 47 s, with the firmware's own order.

The vm host's own changes for this path are twin bugs 7–13 ([below](#twin-bugs-and-missing-features)).

### Why this host, and why Linux

- **Zero-DPU at `vm`, of the build host's architecture.** A GB300 tray (aarch64, the first plan)
  carries a BlueField the model does not boot, and NICo's own aarch64 scout image is built on a
  **64K-page kernel** (`linux-image-6.17.0-1032-nvidia-64k`, `pxe/mkosi.profiles/scout-oss-aarch64`),
  which Apple silicon's Hypervisor.framework cannot run (it supports 4K and 16K pages); under TCG on
  an x86 host it would run, slowly. NICo's x86_64 image pins `linux-image-6.8.0-111-generic`
  (4K pages). The DGX H100 has no DPU, its profile is at `vm` for x86_64, and NICo knows its BMC.
- **NICo identifies a booting host by its packets' source address, and only by it.** carbide-pxe
  takes the TCP peer address of each request as the client (`ClientIpSource::ConnectInfo`,
  `crates/pxe/src/main.rs`) and deliberately ignores `X-Forwarded-For`
  (`crates/pxe/src/extractors/machine_interface.rs`); nico-api's `GetPxeInstructions` and
  `DiscoverMachine` look that address up among the interfaces NICo's DHCP leased. Any proxy
  between the host and them breaks identification. On macOS no native process can see a guest's
  own address without root (no TAP, no vmnet, no address aliases), so the twin cannot give a
  native carbide-pxe a booting host's peer address there (nico-api's own escape hatch,
  `allow_insecure_discovery`, "intended only for test environments using machine-a-tron in
  single-IP mode", covers DiscoverMachine and not the PXE service). On Linux the twin routes the
  guest at L3 into a network namespace where NICo's PXE service and API run (D48).

### What runs

```
 the twin (redfish, ipmi, dhcp, bms, vm)              NICo (built at the pin)
 ─────────────────────────────────                    ───────────────────────
 DGX H100 BMC (Viking tree) ◀── site gateway ◀─HTTPS── nico-api: site explorer, state machine
 BMC DHCP ──▶ twin relay ──────────────udp──────────▶ forge-dhcp-server (host loopback)
 QEMU/KVM x86_64 VM, OVMF, swtpm (EK certs)
   eth0 ═stream═▶ twin switch port (leaf-b01-1:eth1)
                    DHCP ──▶ twin relay (circuit id, link selection) ──▶ forge-dhcp-server
                    IPv4 ──▶ segment gateway 10.40.N.1 ══ L3, no NAT ══▶ svcnet TAP in netns
                                                                         dctwin-nico-svc:
                                                       10.90.0.17:80,8080  carbide-pxe (×2)
                                                       10.90.0.20:11079    nico-api
                                                       forwards in: Postgres, Vault, site gateway, broker
                                                       10.30.N.x:623/udp → the twin's IPMI for the DGX
                                                       exposed out: nico-api at 127.0.0.1:11079
                                                       Postgres, Vault: rootless containers
```

- **NICo's programs** are built from the source at the pin, unmodified, in an `ubuntu:24.04`
  container (NICo's build distribution) with NICo's pinned toolchain (Rust 1.97.1, `-D warnings`
  kept), by `boot-artifacts.sh`: nico-api (`carbide-api`, default features), `forge-dhcp-server`,
  the admin CLI, carbide-pxe (`carbide`) and `forge-scout`. No patch on Linux.
- **NICo's iPXE**: upstream iPXE at the commit NICo pins as its `pxe/ipxe/upstream` submodule,
  with NICo's config headers, its x86_64 patches (TPM measurement, load-image watchdog, HTTP IPv6
  source) and its embedded script, built with its make target
  (`bin-x86_64-efi/snponly.efi DEBUG=tls`).
- **The boot image** (`scout.efi`): NICo builds its scout image with mkosi from
  `pxe/mkosi.profiles/scout-oss-x86_64` into a loader UKI (`scout.efi`) that downloads a root
  filesystem (`scout.squashfs`, gigabytes with NVIDIA's driver, CUDA, DCGM and MFT) from
  `carbide-static-pxe.forge`, and publishes neither. The harness assembles a smaller image from the
  same pieces: Ubuntu noble, **the kernel NICo's profile pins** (`linux-image-6.8.0-111-generic`),
  `forge-scout` at `/opt/forge`, and NICo's own unit (`crates/scout/misc/forge-scout.service`),
  drop-ins, `forge-scout-pre.sh`, `forge-scout-network.sh` and `load_modules.sh`, with the profile's
  packages less what serves hardware the VM has not (NVIDIA driver, DCGM, MFT, InfiniBand,
  ipmitool) or what scout never runs (compilers, cloud-init, a container runtime); one UKI with the
  whole root as its initramfs, so there is no loader stage. Kernel command line, scout's arguments,
  its root-CA fetch and its network setup are NICo's. Without cloud-init, scout reports "cloud-init
  could not be run at all" to nico-api at each boot (`ReportForgeScoutError`), which NICo records
  and goes on.
- **The network** (D48): the VM's NICs are QEMU `stream` sockets to the twin's switch ports; its
  DHCP goes through the twin relay of its leaf to NICo's dhcp-server; its IPv4 to the service
  network is routed at L3 into `dctwin-nico-svc`, where svcnet's TAP holds 10.90.0.17 and
  10.90.0.20. Nothing is added to the host's interfaces, routes or firewall.
- **The host's BMC** serves the VM firmware's own boot options and NICs (D48): NICo takes the
  host's interfaces from the system's EthernetInterfaces and picks its boot option by the
  firmware's alias and device path, and the boot order it sets is written into the VM's NVRAM before
  the next start.
- **The TPM** is swtpm, manufactured with EK and platform certificates signed by the estate CA, so
  scout reports an EK certificate and nico-api's default `tpm_required = true` holds; the operator
  loads the estate CA into NICo as the TPM vendor's (`tpm-ca add`), and NICo verifies the EK
  certificate against it.
- **Machine certificates.** nico-api issues scout a machine certificate from Vault's PKI at
  registration, and scout calls it with that certificate from then on; Vault issues under the site
  CA nico-api verifies clients against (NICo's mac-local-dev script gives Vault a root of its own).
- **IPMI.** NICo restarts a DGX through IPMI, not Redfish (`ipmitool -I lanplus -C 17 … chassis
  power reset` at the BMC's lease, `needs_ipmi_restart`), and ipmitool goes through no proxy, so a
  second svcnet puts the BMC's lease on the namespace's loopback and relays its UDP port 623 to the
  twin's IPMI endpoint for that BMC, as the site gateway carries the BMC's HTTPS.

What NICo is told, as an operator would tell it (`nicoinputs`): one `hostinband` segment per
rack on the provisioning network (where NICo's zero-DPU hosts lease), and the DGX's expected
machine with `dpu_policy: ignore` and its boot NIC (eth0's MAC) declared primary on the HostInband
segment (`network_segment_type: 3`), as NICo's own simulator registers zero-DPU hosts
(`machine-a-tron`). Configuration beyond the harness's: nico-api listens at its site address in
the namespace; `failure_retry_time = "15m"` (NICo's default retries a failed boot after 90
minutes); NICo's mock switch and shelf backends, and no rack manager (`[rms] api_url = ""`): a DGX
H100 rack has none, and with one configured NICo's site explorer requires a rack profile of the
host's rack to look up its slot and tray, and creates no host without it.

## The trays path: a GB300 NVL72 rack to Ready, with NICo's machine software impersonated

`run.sh trays` (x86_64 Linux; estate `nvl72-nico-impersonated`) runs NICo as the Ready path does,
its PXE service and API in a Linux network namespace the twin routes the machines into at L3, on
the GB300 NVL72 rack with NICo's software on the `model` machines impersonated by the twin's
`nicoagent` adapter (D55): NICo's DPU image and forge-dpu-agent on each BlueField
(`nico-dpu-agent`), scout on each tray and server (`nico-scout`). The run is labelled
impersonated: it tests NICo's state machines, rack controller and NVLink manager against the
twin, not NICo's machine software. Each machine boots on its own network path (a DPU on its OOB
port behind its BMC's switch port, a tray through its DPU, a server on its provisioning port), so
NICo's DHCP server, carbide-pxe and nico-api see each machine's own MAC and address.

### Result (2026-10-29, x86_64 Linux, NICo at 02b92f18)

```
trays-dpu-init      PASS   18 DPUs discovered by NICo; no managed host left in DPUInitializing
trays-ready         PASS   18 of 18 GB300 trays Ready in 12 min
trays-rack          PASS   rack a03 through Maintenance(ConfigureNmxCluster(...)) and validation to Ready
trays-nvlink        PASS   nvlink-manager: 18 machines scanned, NMX-C reachable, 18 partitions,
                           72 GPUs; every tray's nvlink_info domain_uuid backfilled from NMX-C as
                           2341b8dc-…c855, the twin's NMX-C domain, with the rack's chassis serial
```

NICo took each tray from `DPUDiscovering` through `DPUInitializing` (Init, platform power cycle,
platform configuration, BIOS polling, `WaitingForNetworkConfig`) and `HostInitializing`
(`EnableIpmiOverLan`, BIOS setup, `SetBootOrder` DPU first, `WaitingForDiscovery`, cleanup,
UEFI setup, lockdown) and BOM validation to `Ready`; then the rack controller ran rack
maintenance (`ConfigureNmxCluster`: switch certificates, the scale-up fabric manager, the primary
switch's certificate, on the twin's RMS; NVOS update skipped, no firmware source configured) and
validation, and the rack became `Ready`. (The first form of the `trays-nvlink` row counted NMX-C
requests in the twin's events, which the NMX-C adapter does not record, and read FAIL; it now reads
nvlink-manager's metrics and NICo's machines' domain UUID. The rack and NVLink rows above were read
by hand from the same sources on that run, before the row was changed.)

What a site configures that this path configures too: the estate CA as a TPM vendor CA (`tpm-ca
add`, scout's EK certificates), and NICo's `dpu_models` catalog naming the BlueField firmware the
estate's BlueFields run, since the impersonated DPU image installs no bundle (NICo's
`docs/operations/firmware-updates/configuration.md`: the catalog follows the bundle). The OEM
servers lease from NICo and are answered `exit` (NICo creates no machine for them on this estate);
only the aarch64 iPXE is served, so an x86 server's scout is not reached here.

## Twin bugs and missing features

1. **BMC clocks are frozen.** Every Manager's `DateTime` is served from the captured tree
   (`2024-08-08T18:48:10-05:00` on the Dell, `2025-11-18T21:26:33Z` on the NVSwitch tray), not the
   twin's clock. NICo's preingestion compares it with its own time (300 s allowed), finds every
   BMC out of sync and runs its time-sync remediation (`set_utc_timezone`, `power.off.force`,
   `bmc.reset`, `power.on`, a 20-minute host-boot wait, up to three cycles), so no host completes
   preingestion. A real BMC's clock runs (and is NTP-set). **Fixed:** every Manager's `DateTime`
   is the twin's clock in the Manager's `DateTimeLocalOffset`, and a `DateTime` PATCH sets where
   the clock runs from (`bmc.clock`; `TestBMCClockRuns`). NTP settings are stored, not modelled.
   The first runs' workaround (a script PATCHing every BMC's clock every 120 s) is removed.
2. **A GB tray's DPUs are not the DPUs in the model.** The GB300 tray's tree (the GB200 capture)
   lists two riser BlueField-3s, `Riser_Slot1_BlueField_3_DPU` and `Riser_Slot2_…`, with derived
   serials (e.g. `AA6234JA8JMA`, `NE9814WY7NWF`), while the model has one sub-device `bf3-0` whose
   own BMC reports `MT6811356LYX`. NICo pairs a host with its DPUs by those serials and waits
   forever: `cannot identify managed host because the site explorer has not discovered all
   attached DPUs … discovered_dpu_count=0 expected_managed_dpu_count=2`. The Supermicro server's
   tree likewise advertises a DPU the model does not have (`expected_managed_dpu_count=1`). On a
   tray the riser chassis serial *is* the DPU's serial. **Fixed:** a dialect's `dpus` patterns
   bind the captured DPU resources to the model's DPU sub-devices in order: each riser (and
   everything under it) carries its DPU's own serial, and a DPU the model's host lacks is absent,
   from its collections too: a GB300 tray shows one riser (as NICo's own GB300 mock has), a GB200
   tray two, the Supermicro server none (`TestHostDPUsAreTheModelsDPUs`). The PF MAC is not bound
   yet. `dpu_policy = "ignore"` was the first runs' workaround.
3. **Dell BIOS settings PATCH has no job.** `PATCH /redfish/v1/Systems/System.Embedded.1/Bios/Settings`
   (with `@Redfish.SettingsApplyTime: OnReset`) answers `204` with no `Location`; iDRAC answers
   `202 Accepted` with `Location: /redfish/v1/Managers/iDRAC.Embedded.1/Jobs/JID_…` and creates
   the job. libredfish's Dell `machine_setup` needs the header (`Remote returned empty header`),
   so NICo leaves both Dell servers in `WaitingForPlatformConfiguration` and reboots them.
   **Fixed:** the capture records iDRAC's answer for one request body only; for any other
   settings PATCH the Dell dialect's `settingsJobs` schedules a job and answers `202` with its
   `Location` (`/redfish/v1/TaskService/Tasks/JID_7…`; `TestDellBiosPatchSchedulesAJob`).
4. **nico-api's own DSX Exchange integration reads a third metadata shape.** `crates/bms-dsx-exchange`
   requires `integration` on every metadata message and `rackName`/`rackId` (not the AsyncAPI's
   `rackLocationName`/`rackLocationId`) on `RackLiquidIsolationRequest` and
   `RackElectricalIsolationRequest`, and expects `HeartbeatTimestampIntegration` metadata:
   `Failed to parse BMS metadata … missing rackName`. So NICo never learns the isolation request
   topics and cannot ask the twin's BMS to isolate a rack. The `nico-legacy` mirror covers the
   consumer's leak points only. **Fixed:** a second mirror, `nico-api` (D40, amended), publishes
   each isolation request's metadata again in nico-api's shape, `{pointType, objectType, rackName,
   rackId, integration}` (`rackId` is the rack's name, NICo's rack id here), retained on a topic of
   its own, `BMS/v1/PUB/Metadata/Rack/<pointType>/<tagPath>/nico-api` (the AsyncAPI forbids those
   names on the request's own metadata topic, and a retained topic holds one message). nico-api
   derives `BMS/v1/<integration>/Value/Rack/<pointType>/<tagPath>/nico-api` from it and publishes
   `{"value":1,"timestamp":<ms>,"quality":"1"}` there (quality a string, where the AsyncAPI's is an
   integer); the twin's BMS takes that as it takes an AsyncAPI request: `1` opens the rack's power
   or closes its liquid supply after the dialect's delay and publishes
   `RackElectricalIsolationStatus`/`RackLiquidIsolationStatus` = 1 on the AsyncAPI topics, `0`
   releases, a republished value changes nothing. nico-api reads no status point, so none is
   mirrored. The heartbeat needs no mirror: the BMS now publishes `HeartbeatTimestampIntegration`
   metadata for its integration in the AsyncAPI's own shape (`objectName`, `objectId`,
   `integration`), which nico-api's parser accepts, and logs the heartbeat nico-api then sends
   every 5 s (`dctwin report`). Enabled by the stack output `bmsConsumerMirror`, now a list:
   `run.sh` sets `"nico-legacy,nico-api"` (`BMS_MIRRORS`) in its copy of the estate. Checked
   against a transcription of nico-api's parser with serde's strictness (`TestNICoAPIParserOracle`,
   `TestNICoAPIMirror`), and against nico-api itself in the final run: `nico-api-bms` and
   `nico-api-isolation` PASS (nico-api isolates a03 at two leaking trays), `nico-api-release`
   KNOWN. Electrical
   isolation takes AC from the rack's trays and so their BMCs: `run.sh` now does the power-off and
   unreachable-BMC faults before the tray leaks, and records whether a03 is released once its tray
   leaks are cleared (forge-hw-health reads the cleared detectors through those BMCs). The worker
   that stopped was not stopped by the twin ([NICo-side findings](#nico-side-findings)).
5. **NVLink switch OS is not emulated.** An NVSwitch tray's NVOS management port (`mgmt0`) has a MAC
   but never DHCPs and serves nothing, and `stack-inputs/expected-machines.json` does not carry it.
   NICo's switch state machine needs it (`No NVOS MAC addresses on expected switch`); with the MAC
   supplied by `nicoinputs` the switches wait in `initializing/WaitForOsMachineInterface` for an
   NVOS interface that never DHCPs, and Flow's power-off fails with `NVOS MAC or IP not available`.
   *Fixed in the twin (D46), and in the final run all 9 switches are `ready`:* `mgmt0` is now a DHCP client of its own
   while the tray is powered on (NVOS runs on the tray's CPU, not the BMC's standby power), relayed
   by the same twin relay with its own circuit id, so on this estate it reaches NICo's
   dhcp-server, whose discovery creates the machine interface the switch state machine looks for
   by MAC; it sends option 12 `nvos-<serial>` and no option 60 (NICo keys on the MAC alone, and its
   own switch simulator sends none). A power-off stops the client at once and a power-on asks for
   the same address again. NVUE answers at that lease (`https://<NVOS IP>`, and
   `https://nvos-<serial>`) through the site gateway while the tray is on, and nothing answers
   there while it is off. `expected-machines.json` carries `nvosMACs` beside `bmcMAC`, and
   `nicoinputs` takes `nvos_mac_addresses` from it instead of deriving them from the estate.
   NMX-C answers at that lease too (as well as at the BMC's address, for clients configured
   with it), with a certificate naming both (`TestNMXCAtNVOSAddress`), so NICo's nvlink-manager,
   which derives NMX-C's address from NVOS's, reaches it. `nicoinputs` sets
   `nvos_username`/`nvos_password` on each expected switch to the tray BMC's factory account
   (the twin's NVOS users are the BMC's accounts). *Still missing:* NVOS SSH/SFTP
   (`switch/nvos-ssh`) and NVUE writes.

6. **Every BlueField reported the captured host PF MAC.** The BlueField tree's OEM `BaseMAC`
   (`5c257308f352`, no separators) and the MACs in its boot options' UEFI device paths
   (`MAC(5C257308F362,0x1)`) were served as captured, since the scrub derived only MACs written
   with colons; NICo keys a host's primary interface on the DPU's `HostPFMacAddress`, so it created
   one GB host and refused the rest. **Fixed:** compact MACs and device-path MACs are scrubbed as
   the same MAC written with colons is, so one NIC keeps one derived MAC however the capture
   spells it (`TestDPUMACsAreTheirOwn`).

7. **A `vm` host was on no segment.** Its NICs were QEMU user-mode networking (`restrict=on`):
   the guest's DHCP was QEMU's own, and nothing a stack serves was reachable. **Fixed** (D48): each
   NIC is cabled to the twin's switch port for it; the guest's DHCP goes through the twin relay of
   its leaf with the cabling's circuit id, and its traffic is routed to the stack
   ([the Ready path](#the-ready-path-a-zero-dpu-host-that-boots-nicos-scout)).
8. **A `vm` host's BMC served the capture's boot options.** NICo picks the host's boot option by
   the firmware's own name and device path for its boot NIC ("UEFI HTTPv4 (MAC:…)", alias
   `UefiHttp`, a device path with the MAC and `IPv4`), sets it first, and verifies it; the
   capture's options name the captured machine's NICs and disks, so NICo would find none
   (`no IPv4 Uefi Http boot option found`). **Fixed** (D48): a `vm` host's BootOptions and
   BootOrder are its firmware's, read from its UEFI variable store, and an order NICo sets is
   written into it before the next start (`TestBootOptionsAreTheFirmwares`).
9. **A `vm` host's TPM had no EK certificate.** NICo's scout refuses a host whose TPM has none
   (`refusing serial fallback`), and nico-api requires one by default (`tpm_required = true`).
   **Fixed:** each machine's swtpm is manufactured on first use with RSA and ECC EK certificates
   and a platform certificate signed by the estate CA.
10. **Two networks relayed to one stack server could not both be answered.** Each network had its
   own upstream relay socket, and NICo's dhcp-server answers a relay at one fixed port
   (`--relay-response-port`), so a host network beside the BMC network got no replies. **Fixed:**
   networks relayed to the same stack server share one upstream leg.
11. **The DGX H100's firmware inventory was the capture's.** Its BMC reported the captured box's
   BMC, BIOS and CPLD versions whatever the model held, so a firmware update through its
   UpdateService changed nothing NICo could see. **Fixed:** the `nvidia/dgx` dialect binds
   `HostBMC_0`, `HostBIOS_0` and `CPLDMB_0` to the model's `bmc`, `bios` and `cpld` components
   (the profile's CPLD baseline is the capture's 0.2.1.8), as the GB dialects bind theirs.
12. **A `vm` host's BMC reported the capture's NICs.** The DGX's `Systems/DGX/EthernetInterfaces`
   were the captured machine's five adapters (with derived MACs), not the VM's; NICo's site explorer
   takes a zero-DPU host's interfaces from there, so it predicted five NICs the host does not have,
   never adopted the boot NIC that had leased from it, and waited in `SetBootOrder` ("Waiting for
   zero-DPU host … to discover its boot NIC before setting boot order"). **Fixed** (D48): a `vm`
   host's system EthernetInterfaces are the NICs its firmware enumerated (the MACs its boot options
   name, with the interface's UEFI device path), under the capture's member ids
   (`TestFirmwareNICsAreTheBootOptionsMACs`, `TestBootOptionsAreTheFirmwares`).
13. **The DGX's firmware boot options were served at the wrong ids.** The DGX's BMC numbers its
   boot options (`BootOptions/0004`, `Id` `0004`, `Name` and `BootOptionReference` `Boot0004`), and
   NICo's DGX implementation fetches `BootOptions/<BootOrder entry less "Boot">` and writes
   `"Boot"+Id` back into the order; served as `Boot0004`, the options were not found and the order
   NICo wrote named none of them. **Fixed:** a `vm` host's options take the capture's id style.

Smaller observations:

- The twin relay's remote id (sub-option 2) is the switch MAC in binary; NICo's dhcp-server reads
  remote and circuit ids as UTF-8 and records `remote_id=None`. Both encodings are legal (RFC 3046
  leaves it opaque); a text remote id would match what NICo logs. *Fixed:* the remote id is the
  switch's MAC as text (`b8:3f:d2:13:04:d4`). NICo's Kea hook also reads it as UTF-8 and drops a
  request whose remote id is not (`crates/dhcp/src/discovery.rs`, `discovery_set_remote_id`).
- Only the NVIDIA GB dialect sends a DHCP vendor class; a BlueField BMC sends `dc-twin`, where a
  real one sends `NVIDIA/BF/BMC` (implementation finding 7). *Fixed:* the BlueField dialect's
  `identity.dhcpVendorClass` is `NVIDIA/BF/BMC`, which NICo's DHCP recognises
  (`crates/dhcp-common`), and the Dell and HPE dialects send `iDRAC` and `CPQRIB3`, the x86 BMC
  classes it recognises. The power shelves, the NVSwitch tray's BMC, Lenovo and Supermicro still
  send `dc-twin`.
- Site addresses are reachable only through the gateway. forge-hw-health's reachability probe opens
  plain TCP to `10.30.x.y:443` and reports every BMC `TcpPortUnreachable` (D20's TAP substrate).
- The twin stores NICo's `AccountLockoutThreshold`/`Duration` but does not enforce lockout.
- The twin-hosted broker (mochi-mqtt) wrote a server DISCONNECT, which MQTT 3.1.1 does not have, to
  every client when it closed; nico-api's rumqttc logged `Invalid incoming packet = Disconnect` and
  an event-loop error at each twin shutdown. **Fixed:** 3.1.1 connections are closed without one
  (`TestEmbeddedBrokerClosesMQTT311WithoutDisconnect`).
- On this estate the zero-DPU hosts wait for their boot NIC: its provisioning network is
  `dhcp: twin` and its hosts are at `model` fidelity, so NICo never sees a host NIC DHCP. The Ready
  path ([below](#the-ready-path-a-zero-dpu-host-that-boots-nicos-scout)) has a host that does.

Found on the trays path ([above](#the-trays-path-a-gb300-nvl72-rack-to-ready-with-nicos-machine-software-impersonated)):

14. **Option 60 for UEFI HTTP boot had four fields.** The boot walker and the stand-in sent
    `HTTPClient:Arch:00019`; EDK2 sends `HTTPClient:Arch:00019:UNDI:003000`, and NICo's DHCP
    server reads any shape but five fields as an unknown client and offers it no boot URI.
    **Fixed** in both (`vendorClassFor`, the stand-in's firmware and iPXE).
15. **A GB tray's boot options did not name its DPU.** The tray and its BlueField are separate
    captures, so the tray's UEFI network boot options for its riser BlueField carried a MAC of
    the tray's own derivation, and NICo's `set_boot_order_dpu_first` found no option for the
    DPU's host PF; and a boot option's `DisplayName` kept the captured `(MAC:…)` while its device
    path's was re-identified. **Fixed:** the dialect's `dpuHostMACs` binds the capture's DPU NIC to
    the model's DPU, whose BMC's `BaseMAC` the tray then names, and display-name MACs are scrubbed
    as device-path MACs are (`TestTrayBootsFromItsDPUByTheDPUsMAC`).
16. **The stand-in read the host's address from deprecated fields.** NICo writes a host
    interface's address in `addresses` (an IPv4 entry with the gateway as a CIDR); the flat `ip`
    and `gateway` are deprecated and empty, so the DPU's DHCP server had nothing to offer its host.
    **Fixed** (`ifaceV4`), and the fake NICo of the tests writes NICo's shape.
17. **`sanboot` was not interpreted.** NICo answers a DPU past discovery with
    `sanboot --no-describe --drive 0x80`, a boot from the local disk; the interpreter refused it,
    and the DPU retried the network instead of starting its agent, so its host's DHCP went
    unanswered. **Fixed:** a local `sanboot` is the firmware's next option, as `exit` is.
18. **The routed site network did not reach estate endpoints.** nvlink-manager, in the
    namespace, dials NMX-C at a switch tray's NVOS lease on 9370; the twin's router forwarded only
    to machines' leases and dropped the rest (`ClientCreateFailed`). **Fixed:** the router
    terminates the namespace's TCP to an address the substrate's site routes serve and connects to
    the twin's listener (`Uplink.SetSiteRoutes`, `TestSiteNetworkReachesEstateEndpoints`).
19. **A stand-in firmware gave up after its first failed network boot**, where UEFI cycles its
    boot options; a DHCP server or boot service that answers late left the machine with no OS.
    **Fixed:** a boot with no answer is tried again after 30 s.

Since fixed: the BlueField-3, GB200 tray and DL385 profiles stated firmware levels their BMCs did
not serve (the capture seeds them); they now state the capture's, and a GB300 tray, whose BMC
presents the GB200 capture, serves its profile's (D35, clarified). A GB tray's GPU firmware and
InfoROM inventory reported the capture's `Absent`; it now follows the model's GPUs.

## NICo-side findings

These are about NICo, not the twin; they are recorded because the twin made them visible.

- **nv-redfish, the site explorer's default client, rejects two captured real-hardware trees** that
  libredfish reads: the GB200 capture's `HGX_FPGA_0` has `"UUID": "STATIC:3:0"` (NICo already
  skips the same pattern on BlueField-4's `BlueField_IRoT_NIC_0`), and the Supermicro capture's GPU
  firmware entries have `"ReleaseDate": "T00:00:00Z"`. Both values are in libredfish's own test
  fixtures (`a51ffb1`), i.e. real BMC output. With the default mode NICo cannot explore those
  hosts at all. (The twin's GB300 dialect reuses the GB200 capture; whether real GB300 firmware
  carries the same UUID is open.)
- `dev/mac-local-dev/README.md` documents admin-cli flags (`-c`, `--format`) the CLI no longer has
  (`--api-url`, `-f`); the consumer README's environment prefix `NICO_DSX_CONSUMER__` is
  `CARBIDE_DSX_CONSUMER__` in the code; the consumer needs Vault (`VAULT_CACERT`) although its
  README does not say so.
- **The DSX Exchange consumer clears a rack's leak alert on any other point's Clear** of the same
  rack, including periodic re-publications ([section 4](#4-dsx-exchange-consumer)).
- nico-api admits Flow's requests only under a SPIFFE identity; with the dev client certificate
  Flow's calls fail with `ResourceExhausted: API admission capacity exhausted`.
- **nico-api counts every AsyncAPI metadata message as a parse error.** `parse_supported_metadata`
  requires `integration` before it looks at the point type (`crates/bms-dsx-exchange/src/lib.rs:244`),
  so each BMS-published point, which by the AsyncAPI carries no `integration`, is a
  `Failed to parse BMS metadata` warning and a `status=serialization_error` count rather than
  `Ok(None)`: 32 every 100 s on this estate, mirror or not. It also publishes `quality` as the
  string `"1"` (`lib.rs:26`); the AsyncAPI's is an integer.
- **`BMS DSX Exchange command dropped because the worker stopped` is a nico-api instance going
  away, not something the BMS sent.** The worker (`crates/rack/src/bms_client.rs:191-240`) leaves
  its loop only when its cancellation token is cancelled (SIGINT or SIGTERM,
  `crates/utils/src/shutdown_handler.rs`) or its `JoinSet` is dropped (`run` returning an error);
  a parse error, a publish error or timeout and a broker disconnect are counted and the loop goes
  on, so no message from the twin can end it. The MQTT client that feeds it runs on detached
  `tokio::spawn` tasks (`crates/mqttea/src/client/core.rs:243, 347`), outside that `JoinSet` and
  token, so an instance that is shutting down, or whose start failed after the BMS handle was
  made, keeps its subscription without a worker and logs one drop per message the broker
  delivers. The 2026-10-18 interactive log points to that: 33 `command_kind=metadata` drops in
  one burst (one set of the BMS's retained metadata, between two of its 100 s re-publications),
  followed at once by `CommonPool metrics stop was requested` and `WorkLockManager: all handles
  dropped` (an API instance being dropped) and a torn log line, while the long-running instance's
  worker parsed every later re-publication to the end of the run: a second nico-api on the same
  broker and log file, being torn down (the log does not say which process it was). The clean
  `run.sh all` runs have none. `run.sh`'s
  `stop_bg` now waits for a stopped process to exit before its replacement starts.

Found on the Ready path ([below](#the-ready-path-a-zero-dpu-host-that-boots-nicos-scout)):

- **NICo identifies a booting host by its packets' source address, and only by it.** carbide-pxe
  takes each request's TCP peer address as the client (`ClientIpSource::ConnectInfo`,
  `crates/pxe/src/main.rs`) and ignores `X-Forwarded-For` by design
  (`crates/pxe/src/extractors/machine_interface.rs`); nico-api's `DiscoverMachine` does the same
  unless `allow_insecure_discovery` is set (for "test environments using machine-a-tron in
  single-IP mode"). Any NAT or proxy between a host and NICo (a container's published port, a
  Kubernetes Service without `externalTrafficPolicy: Local`) breaks the boot of every host.
- **NICo's aarch64 scout image runs a 64K-page kernel** (`linux-image-6.17.0-1032-nvidia-64k`,
  `pxe/mkosi.profiles/scout-oss-aarch64` and `scout-loader-aarch64`), which Apple silicon's
  Hypervisor.framework cannot run (4K and 16K pages only); its x86_64 image pins
  `linux-image-6.8.0-111-generic` (4K pages).
- **NICo publishes no boot artifacts**: every reference to them is a placeholder registry
  (`deploy/components/boot-artifacts-containers`, `helm/examples/values-full.yaml`), the CI
  artifacts at the pin hold only DPU binaries, and its own build of them needs Linux and 150 GB
  (`docs/manuals/building_nico_containers.md`). Its scout loader fetches the root filesystem from a
  hardcoded `http://carbide-static-pxe.forge/…/scout.squashfs` (`pxe/common_files/scout-loader-rclocal`).
- **NICo ingests a DGX H100 only at one exact CPLD version.** Its site explorer refuses to pair a
  "Viking" host unless the BMC reports `CPLDMB_0` at exactly 0.2.1.9
  (`crates/site-explorer/src/lib.rs:4004`, `PairingBlockerReason::VikingCpldVersionIssue`, "A DC
  power cycle may be needed"), so a DGX on any other CPLD, older or newer, is never created; the
  captured DGX's is 0.2.1.8. The Ready path updates it as an operator would (Redfish
  `SimpleUpdate` of `CPLDMB_0`, then a DC power cycle) before NICo explores the host.
- **NICo restarts a DGX H100 only through IPMI.** For an NVIDIA BMC with system `DGX` and manager
  `BMC` it resets the host with `ipmitool -I lanplus -C 17 … chassis power reset` at the BMC's
  address rather than through Redfish (`needs_ipmi_restart`,
  `crates/machine-controller/src/handler.rs`), so a site that allows only Redfish (HTTPS) to its
  BMCs, or reaches them through a proxy, cannot reboot a DGX: every restart fails ("Could not open
  socket!") and the host never leaves `WaitingForPlatformConfiguration`.
- **NICo sets a DGX H100's boot order only while it does not yet know it is one.** Boot-order
  remediation is skipped once the host's hardware information says DGX H100
  (`should_skip_boot_order_remediation`, checked when its BIOS settings are verified: "Viking BMC
  firmware cannot safely run boot-order remediation"). When scout has already registered the host
  by the time NICo's host init gets there (as in the Ready path's final run), NICo leaves the order
  as it is, and the host reaches NICo's iPXE only because its firmware's own order gets to UEFI HTTP
  boot after PXE, to which NICo's DHCP gives nothing to boot.
- **With an RMS configured, NICo creates no host whose expected machine names a rack without a
  rack profile** ("rack … has no rack_profile_id for RMS slot and tray lookup",
  `crates/site-explorer/src/machine_creator.rs`), whether or not the rack has anything RMS manages,
  such as a DGX H100 rack. The Ready path configures no RMS (`[rms] api_url = ""`).
- **NICo's dev Vault issues machine certificates nico-api refuses.** `dev/mac-local-dev` gives
  Vault's PKI a root of its own (`myvault.com`) while nico-api verifies client certificates against
  the site CA, so scout registers a host and then every call it makes with its machine certificate
  is refused ("invalid peer certificate: UnknownIssuer"), and the host waits in cleanup. A site's
  Vault issues under the site's CA, as the Ready path's does.
- **NICo's DHCP hands a boot file only to UEFI HTTP boot clients** (`HTTPClient`): a PXE client is
  leased an address and given nothing to boot, and NICo serves no TFTP; and its embedded iPXE
  script fetches the boot script from port 80 of `next-server` while the DHCP boot URL names port
  8080 (`pxe/ipxe/local/embed.ipxe`, `crates/dhcp-server/src/util.rs`), so carbide-pxe must answer
  on both.

## What this does not show

- A host with DPUs reaching `Ready` with NICo's own machine software: on the trays path the
  GB300 trays reach `Ready` with that software impersonated (D55); without the impersonators
  they stop in DPU discovery, the DPU OS not being modelled (Q1). The OEM hosts
  of `nvl72-nico` are explored, preingested and powered by NICo but not ingested there (they are at
  `model` fidelity). The zero-DPU DGX of the Ready path reaches `Ready` with: a scout image
  assembled from NICo's pieces rather than NICo's own (which NICo does not publish; without
  NVIDIA's driver, DCGM and MFT), machine validation skipped (NICo's default; its tests need
  GPUs), attestation and SPDM off (NICo's defaults), and a TPM whose EK chain is the estate CA's.
  Nothing after `Ready` (an instance, an OS install) is exercised.
- NICo's own NSM and PSM: the twin's RMS stands where NICo's deployment puts RMS, which is what
  NICo talks to; nv-rms's own drive of the trays (NVOS over SSH) is not exercised.
- nico-api releasing an isolated rack, and the consumer keeping a leak override through a
  sibling point's Clear (both KNOWN, NICo-side).
- Everything else the stack document lists: attestation, firmware updates, NMX-C through NICo's
  nvlink-manager (`compat/nico-fabric` runs NICo's `libnmxc` against the twin instead), UFM
  through NICo, HBN.

## run.sh PASS/FAIL table

A clean `run.sh all` (fresh `WORK`, binaries from the first build copied into `$WORK/bin`,
`NICO_SRC`, `VAULT_BINARY` and `TEMPORAL_BIN` set; defaults otherwise: `DPU_POLICY=manage`,
`COMPONENT_BACKEND=rms`, `BMS_MIRRORS=nico-legacy,nico-api`), 2026-10-19:

```
fetch                   PASS   NICo 02b92f18fed776c8ef0056803338707c5e6bcb6b; temporal 1.9.1
build                   PASS   nico-api, forge-dhcp-server, forge-hw-health, dsx-exchange-consumer, admin-cli, bmc-explorer-cli, flow
classify                PASS   libredfish classified 13 of 13 BMCs; nv-redfish (the site explorer's default) explored 8 of 13 (out/classify.txt)
up                      PASS   postgres (TLS), vault, twin with 83 endpoints
nico-api                PASS   listening on 127.0.0.1:11079; 6 underlay segments, one per twin rack
flow                    PASS   NICo Flow up; leak-detection job every 15 s
load                    PASS   23 expected machines, 1 expected NVL rack, 8 shelves, 9 switches (with NVOS MACs)
dhcp                    PASS   68 of 68 BMC ports leased by NICo's forge-dhcp-server through the twin relay (option 82, link selection)
discover                PASS   58 of 68 leased BMCs explored (10 PDUs refused as non-BMCs); serial = the twin's on all 40 non-GB BMCs (out/explored-vs-twin.txt)
discover-gb-serial      KNOWN  18 GB300 trays report the HGX baseboard's serial, not the tray's (libredfish fallback; README)
credential-rotation     PASS   58 BMC root passwords rotated to the site password (account.password on the twin)
machines                PASS   18 managed hosts identified, 36 machines created (out/machines.txt)
machines-gb-trays       PASS   18 of 18 GB300 trays are managed hosts paired with their DPU (5 DPUDISCOVERING/DISABLESECUREBOOT{DISABLE_SECURE_BOOT_STATE:SOME(CHECKSECUREBOOTSTATUS),COUNT:0}, 5 DPUDISCOVERING/REBOOTALLDPUS, 8 DP
health                  PASS   forge-hw-health polls 44 BMCs' sensors and leak detectors (the NVSwitch trays and power shelves NICo created)
bmc-unreachable         PASS   b04/server-02 (10.30.19.11): NICO-SITEEXPLORER-102, the endpoint was not reachable
power-off               PASS   b01/server-01 off on the twin; NICo's site explorer powered it on (power.on POST /redfish/v1/Systems/System.Embedded.1/Actions/ComputerSystem.Reset)
tray-leak               PASS   switch-tray-03: TrayLeak, SensorCritical, PreventAllocations, Hardware (hardware-health.tray-leak-detection)
tray-leak-flow          PASS   Flow -> NICo core -> the twin's RMS (power.off.force rms BatchSetPowerState): switch-tray-03 powered off
rack-leak-from-trays    PASS   two leaking trays: rack a03 Alerting (RackLeak at the threshold of 2)
nico-api-bms            PASS   nico-api reads the BMS's metadata: 46 values received on BMS/v1/NICO/Value/System/HeartbeatTimestampIntegration/site-1/integration/NICO (its HeartbeatTimestampIntegration, every 5 s)
nico-api-isolation      PASS   two leaking trays → nico-api asked the BMS to isolate a03 (RackElectricalIsolationRequest=1 on the nico-api mirror's topic) → the twin's BMS opened a03's power and closed its liquid supply
nico-api-release        KNOWN  a03 still isolated 180 s after its tray leaks were cleared on the twin: nico-api sent no RackElectricalIsolationRequest=0 (README)
consumer-rack-leak      PASS   BMS LeakDetectRack on a03 → consumer inserted the rack a03 override (removed again before the rack's health history recorded it, see consumer-sibling-clear)
consumer-sibling-clear  KNOWN  a Clear re-published for another point of rack a03 removed the leak override while leak-point-01 was still leaking (NICo consumer, README)
consumer-clear          PASS   cleared → the consumer removed the rack health override
```

`KNOWN` rows are limits named [above](#result-2026-10-19-macos-26-on-apple-silicon-rust-1981);
they do not fail the run. `consumer-sibling-clear` shows only when the leak lands where the BMS's
re-publication cycle lets a sibling point's Clear arrive first.

The first clean run (2026-10-18, before the twin fixes, with `dpu_policy = "ignore"`, NICo's mock
switch and shelf backends and a script keeping the BMC clocks) recorded 19 PASS and 3 KNOWN: no
GB300 tray created, switches waiting for NVOS, the leak power-off failing in NICo core
(`NVOS MAC or IP not available`) and nico-api rejecting the BMS's metadata.

A clean `run.sh ready` (x86_64 Linux with KVM, fresh `WORK` but for `BOOT_ARTIFACTS`, the
binaries and boot image `boot-artifacts.sh` built at the pin), 2026-10-20:

```
fetch             PASS   NICo 02b92f18fed776c8ef0056803338707c5e6bcb6b; temporal 1.9.1
boot              PASS   NICo's iPXE (355328 B), scout.efi (118 MiB: forge-scout and NICo's scout units, Ubuntu noble, kernel 6.8.0-111-generic), carbide-pxe, nico-api, dhcp-server, admin CLI for linux/amd64 at 02b92f18
build             PASS   nico-api, forge-dhcp-server, admin-cli, carbide-pxe and forge-scout for linux/amd64 at 02b92f18fed776c8ef0056803338707c5e6bcb6b (boot-artifacts.sh)
up                PASS   postgres (TLS), vault, twin with 10 endpoints
operator-cpld     PASS   the operator updated the DGX H100's CPLDMB_0 0.2.1.8 → 0.2.1.9 (Redfish SimpleUpdate through its BMC, applied at a DC power cycle: ForceOff, On), the version NICo's site explorer pairs a DGX at
pxe               PASS   carbide-pxe (linux/amd64) at 10.90.0.17:80 and :8080 in namespace dctwin-nico-svc, linked to the twin's routed site network (logs/svcnet.log)
nico-api          PASS   listening on 10.90.0.20:11079 in dctwin-nico-svc (and 127.0.0.1:11079 through svcnet); 3 segments, one per twin rack and network
load              PASS   1 expected machines, 0 expected NVL rack, 0 shelves, 0 switches (with NVOS MACs)
tpm-ca            PASS   the estate CA, which signed the vm host's EK certificates, loaded as a TPM CA (tpm-ca add)
dhcp              PASS   5 of 5 BMC ports leased by NICo's forge-dhcp-server through the twin relay (option 82, link selection)
ipmi              PASS   the DGX's BMC answers IPMI (lanplus, cipher suite 17) at its lease 10.30.16.10:623 in dctwin-nico-svc, where NICo's ipmitool reaches it: "Chassis Power is on"
ready-machine     PASS   site-1/hall-a/b01/server-01 is NICo machine fm100psdhp8370odidpvpr6ae3qvn5l5ttfhuc7mmilt9cral6361sv9kr0, created as a zero-DPU host (its expected machine: dpu_policy ignore, the boot NIC primary on the HostInband segment)
ready-dhcp        PASS   the guest (3c:6d:66:26:cc:b8) leased 10.40.16.10 from NICo's dhcp-server (server id 10.90.0.16) through the twin relay (circuit id b01-leaf-b01-1:eth1), boot file http://10.90.0.17:8080/public/blobs/internal/x86_64/ipxe.efi; its answers by the client's DHCP vendor class: HTTPClient 3, PXEClient 9, none 3; 7 lease events
ready-boot        PASS   carbide-pxe served ipxe.efi (7), the iPXE boot script (3) and scout.efi (3), and scout fetched the root CA (3), to 10.40.16.10 (the guest's own lease as the peer)
ready-discovery   PASS   scout's DiscoverMachine: SMBIOS product/board/chassis serial 1337618079559/1337618079559/1337618079559 (the BMC's Redfish serial, the twin's: 1337618079559), NVIDIA DGXH100; boot NIC 3c:6d:66:26:cc:b8 among the NICs scout reported; TPM EK certificate reported, its CA found: true (O=dc-twin, CN=dc-twin estate CA)
ready-validation  KNOWN  NICo entered machine validation (Validation/MachineValidation) and skipped it: [machine_validation_config] enabled = false, NICo's default
ready-state       PASS   site-1/hall-a/b01/server-01 reached Ready in 7 min: configureastra/enablenics → dpudiscoveringstate → hostinit/waitingforplatformconfiguration → hostinit/pollingbiossetup → hostinit/measuring → hostinit/spdmmeasuring → hostinit/waitingfordiscovery → waitingforcleanup/init → waitingforcleanup/hostcleanup → hostinit/waitingfordiscovery → hostinit/uefisetup → hostinit/waitingforlockdown → bomvalidating/MatchingSku → validation/machinevalidation → hostinit/discovered → ready (out/host-state-history.txt)
```

## Files

| File | What |
|---|---|
| `run.sh` | the whole run, in steps; prints the PASS/FAIL table |
| `nicoinputs/` | Go: the twin's stack inputs and floor plan → NICo's underlay segments, rack profile, expected machines, shelves, switches, rack group and rack |
| `nico-api-config.toml.in`, `dhcp-server.yaml.in`, `hw-health.toml.in`, `dsx-exchange-consumer.toml.in` | configuration templates |
| `classify.sh` | bmc-explorer-cli on one BMC of each kind, through both of NICo's Redfish clients |
| `nico-report.py` | NICo's site-explorer report joined with the twin's model |
| `twin-requests.py` | the stack's requests in a twin event log, by canonical name |
| `../../examples/estates/nvl72-nico.yaml` | the estate: an NVL72 rack, its network rack, four OEM racks, NICo's DHCP |
| `boot-artifacts.sh` | the Ready path's builds, in an `ubuntu:24.04` container: NICo's iPXE, carbide-pxe, nico-api, the dhcp-server, the admin CLI, forge-scout, and the scout boot image (`scout.efi`); `rust-cross` cross-compiles carbide-pxe and forge-scout for linux/arm64 on macOS |
| `svcnet/` | Go, Linux: the routed site network's far end, a TAP in a network namespace (or a container) carrying the twin's frame link, with TCP forwards in and out, and UDP relays at an estate address (a BMC's IPMI) |
| `ready.py` | the Ready path's evidence: NICo's record of the host (its state history) and the twin's events and logs, as result rows |
| `pkg-config-sysroot.py` | a minimal pkg-config for `rust-cross` (a target sysroot copied from the build container) |
| `../../examples/estates/dgx-nico-ready.yaml` | the Ready path's estate: a DGX H100 at `vm` fidelity, its network rack, NICo's DHCP on both networks, the routed site network |

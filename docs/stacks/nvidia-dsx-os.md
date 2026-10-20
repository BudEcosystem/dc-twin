# Stack: NVIDIA DSX OS

The first stack dc-twin targets. This document records what each DSX OS component needs from a
datacenter, what the twin provides for it, which of NVIDIA's own test tooling the twin reuses as
an oracle, and what the twin cannot show. It was rewritten after the
[review of 2026-10-18](../reviews/2026-10-18-spec-review.md) against the components' source.

> **Read on 2026-10-18** from the components' public repositories and documentation. Several
> repositories moved from `github.com/NVIDIA` to `github.com/dsx-ai-factory` during 2026. Each
> fact is re-checked when the stack definition is pinned.

## What DSX OS is

NVIDIA describes DSX OS as open, modular software for operating multi-tenant AI factories. It is
the software NVIDIA runs DGX Cloud on, released component by component. It spans facility and
power (DSX Exchange, MaxLPS, DSX Flex), bare-metal lifecycle and isolation (Infra Controller, DOCA
Platform Framework), cluster runtime (AI Cluster Runtime), health (NVSentinel, Fleet Intelligence),
and scheduling and serving (KAI Scheduler, Run:ai, Dynamo, Grove, Cloud Functions).

## NVIDIA Infra Controller (NICo)

`dsx-ai-factory/infra-controller`, Rust, Apache-2.0. Site-local bare-metal lifecycle
management: discovery, firmware baselines, DPU provisioning, network isolation, attestation and
sanitisation between tenants.

### Dialects NICo recognises

NICo selects a hardware type by **exact match** on Redfish identity strings
(`crates/bmc-explorer/src/lib.rs`, `libredfish/src/model/service_root.rs`). A dialect that does
not reproduce them is treated as unsupported hardware, or as a Sushy emulator with no BIOS or
lockdown steps, which defeats the test.

| Twin dialect | ServiceRoot `Vendor` / `Product` | Further match | Factory login |
|---|---|---|---|
| `nvidia/gb-bmc` (GB200/GB300 host trays) | `NVIDIA` or `WIWYNN` / `GB BMC` or `GB200 NVL` | GB300 by a Chassis member with `Manufacturer: NVIDIA`, `Model: NVIDIA GB300`; then split by host vendor (NVIDIA, Supermicro, Lenovo) | per vendor |
| `nvidia/nvswitch-tray` | `NVIDIA` / `P3809` | | |
| `nvidia/bluefield` | `Nvidia` (mixed case) / — | System `Id` `Bluefield` or `BlueField_0`; generation from Product `BlueField-2/3/4` | `root`/`0penBmc` (BF2/3), `admin`/`0penBmc` (BF4) |
| `nvidia/dgx` ("Viking") | `AMI` / — | System `Id` `DGX` | |
| `nvidia/vera-rubin` | — / `VR NVL72` | | |
| `liteon/power-shelf`, `delta/power-shelf` | separate shelf types; Liteon's chassis reports `LITE-ON TECHNOLOGY CORP.`, `PF-1333-7R` | | |
| `oem-dell/idrac`, `oem-hpe/ilo`, `oem-supermicro/redfish` | `Dell`, `HPE`, `Supermicro` | | |
| `oem-lenovo/xcc` | `Lenovo` | an `Ami` key under `Oem` selects the LenovoAMI path | |
| (emulators) | `Sushy`, `Contoso`, `RedVirt` | collapsed to one type with no BIOS or lockdown steps: **never what a twin dialect presents** | |

NICo's `nv-redfish` client also requires `Links.Sessions` in the ServiceRoot. The `bmc-mock`
profiles in NICo's repository are the capture oracle for each of these ([16](../spec/16-testing.md#oracles-per-adapter)).

### What NICo asks a BMC

NICo's `libredfish` trait has about eighty operations. The twin's dialects implement them
([D25](../spec/11-decisions.md#d25--a-protocol-engines-scope-is-the-stacks-call-list-the-dialect-binding-is-data)),
mapped to [canonical request names](../spec/14-expressions.md#canonical-request-names):

| Group | Operations (trait method names) |
|---|---|
| Inventory and health | `get_service_root`, `get_systems`/`get_system`, `get_managers`/`get_manager`, `get_chassis_all`/`get_chassis`, `get_chassis_assembly`, `get_chassis_network_adapters`, `get_base_network_adapters`, `get_ports`, `get_network_device_functions`, `pcie_devices`, `get_manager_ethernet_interfaces`, `get_system_ethernet_interfaces`, `get_base_mac_address`, `get_power_metrics`, `get_thermal_metrics`, `get_gpu_sensors`, `get_drives_metrics`, `get_resource`, `get_collection` |
| Power and boot | `get_power_state`, `power`, `chassis_reset`, `bmc_reset`, `bmc_reset_to_defaults`, `get_boot_options`, `boot_once`, `boot_first`, `set_boot_override`, `change_boot_order`, `set_boot_order_dpu_first` |
| Accounts and credentials | `get_accounts`, `create_user`, `delete_user`, `change_username`, `change_password`, `change_password_by_id`, `set_machine_password_policy`, `change_uefi_password`, `clear_uefi_password` |
| Secure Boot and lockdown | `get_secure_boot`, `enable_secure_boot`, `disable_secure_boot`, `get_secure_boot_certificate(s)`, `add_secure_boot_certificate`, `lockdown`, `lockdown_status`, `machine_setup`, `machine_setup_status`, `is_bios_setup` |
| BIOS | `bios`, `set_bios`, `reset_bios`, `pending`, `clear_pending` |
| Firmware | `get_firmware`, `get_software_inventories`, `get_update_service`, `update_firmware`, `update_firmware_simple_update`, `update_firmware_multipart`, `get_tasks`/`get_task`, `get_job_state` |
| Console and logs | `setup_serial_console`, `serial_console_status`, `get_system_event_log`, `get_bmc_event_log` |
| TPM | `clear_tpm` |

Other crates matter too: `credential-rotation` (BMC passwords are rotated after discovery, so
AccountService writes must persist), `measured-boot` and `nras` (attestation), `spdm-controller`
(SPDM against components, which the twin cannot serve and lists in `isNot`), `dhcpv6`, `pxe` and
`ipxe-renderer` (the boot scripts the walker interprets), `ssh-console`, `power-shelf-controller`,
`rack-controller`, `switch-controller`, `preingestion-manager`.

### What NICo needs from the datacenter

| It needs | The twin provides | Notes |
|---|---|---|
| **Redfish BMCs over HTTPS** with the operations above and EventService SSE | The Redfish engine with the dialects above | One dialect complete at M0/M1 ([Q19](../spec/11-decisions.md#q19)) |
| **Relayed DHCP only.** Its Kea hook accepts relayed requests; the relay address or option-82 link selection picks the segment; circuit id, remote id and vendor class are read | A relay in front of every endpoint: SONiC-VS by giaddr, the twin relay or a Kea relay for link selection ([05](../spec/05-interfaces.md#boot-and-addressing)) | SONiC-VS has no RFC 3527 link selection; giaddr is accepted |
| **PXE/iPXE boot, OS install, and its discovery agent (`scout`) on the host** | `vm` hosts in `os: stack` mode boot NICo's images for real; `model` hosts boot-walk | At `model`, `scout` cannot run; the `machine-a-tron` impersonator may stand in for scale runs, labelled |
| **TPM 2.0 measured boot** (`/dev/tpmrm0`, then `/dev/tpm0`; EK checked against CAs uploaded with `admin-cli tpm_ca add`) | swtpm with persisted state and the estate's EK CA | [Q5](../spec/11-decisions.md#q5) |
| **Switches** (site-configured): eBGP unnumbered toward DPUs with a 32-bit ASN each and /32 loopbacks, EVPN with its route targets, a default route into the overlay, LACP, DHCP relay on BMC-facing networks | SONiC-VS fabric from `clos-2tier` with the stack's `fabricProfile` | [Q3](../spec/11-decisions.md#q3) |
| **BlueField DPUs** as paired sub-devices: DPU BMC (Redfish), DPU OS, DPU agent, HBN driven through NVUE, `lldpd` | The `nvidia/bluefield` dialect on a sub-device, fronted by the `bluefield` adapter (D51); no DPU OS at `model`; at `vm`, an Arm (QEMU aarch64) that the BMC's BFB push installs and boots (D52) | **The largest gap:** at `vm` the BMC's push installs the BFB on the Arm, its DPU OS boots and runs DPF's first boot, and DPF's DPU agent reports to DPF; the agent stops at its first operation that needs the ConnectX (`nvconfig`: no `p0`/`p1` through devlink), and reaches the DPU cluster join and a Ready DPU only when its flavor tells it to skip the ConnectX's steps; HBN does not run. The ConnectX ports with their representors and SFs, rshim's `tmfifo_net0` and IPMI towards the BMC have no QEMU device ([Q1](../spec/11-decisions.md#q1)); `dpuMode: false` until it closes |
| **UFM REST**: `/ufmRest` with basic auth and `/ufmRestV3` with a token; version, SM configuration, ports, partition keys and membership, QoS | The UFM REST adapter bound to the model | NICo's `ufm-mock` is an oracle |
| **NMX-C** gRPC on each NVL rack's switch tray, port 9370 with TLS, including `Subscribe` telemetry; **NVUE REST** on NVOS | NMX-C and NVUE adapters bound to the scale-up domain | NICo's `nmxc-mock` returns UNIMPLEMENTED for `Subscribe` ([Q8](../spec/11-decisions.md#q8)) |
| **Rack Management Service** (`dsx-ai-factory/nv-rms`): gRPC to NICo, and **NVUE REST, SSH/SFTP and NMX-C** to switch trays | The rack-management adapter and the switch tray's NVUE and SSH surfaces (`switch/nvos-ssh`) | NICo's `rms-mock` changes nothing on firmware or image jobs |
| **In-tray leak detectors** over Redfish `Chassis/…/ThermalSubsystem/LeakDetection/LeakDetectors` | The tray profiles' `leak` sensor, projected as `LeakDetectors` | see [The two leak paths](#the-two-leak-paths) |
| **BMS points over DSX Exchange**, consumed by `nico-dsx-exchange-consumer` | The BMS publisher in the `nvidia/dsx-exchange` dialect | |
| **NVIDIA's attestation service** | `external/stub:nvidia-nras` | Always labelled a stub |
| **Its own control plane**: a three-node Kubernetes site controller with PostgreSQL, Vault, Temporal, Keycloak, cert-manager, External Secrets and MetalLB | The service zone, provisioned by the twin from the stack's `install` steps, pulling from the artifact mirror | The largest sizing term ([09](../spec/09-runtime.md#sizing)) |

### The two leak paths

NICo handles leaks two ways (`docs/operations/leak-detection-handling.md`,
`crates/dsx-exchange-consumer`), and the twin models both:

| Path | Source | What NICo does | What the twin provides | Scenario asserts |
|---|---|---|---|---|
| **Tray detector** | the compute or NVSwitch tray's own leak detector, read over Redfish `LeakDetectors` | its default policy shuts down **the leaking trays**; above a threshold (default 2 trays) it asks the BMS over Exchange for electrical or liquid **isolation** | the tray profile's `leak` sensor and the `generic/leak/tray-detector` fault; the isolation request and status points in the `nvidia/dsx-exchange` dialect, answered by the twin's BMS | `power.off.graceful` on the leaking tray and no other; the isolation request above the threshold |
| **BMS point** | a rack rope, drip pan or CDU point, published as `Rack/RackLeakDetect` (also `RackLeakSensorFault`, `RackLeakDetectTray`) | creates a `RackLeak` health alert with `PreventAllocations`; **powers nothing off**; rack isolation (breaker and valve) is the BMS's job | the rack format's leak points and the `generic/leak/rack-point` fault, publishing the Rack point types the consumer reads | the alert through the `alerts` accessor; no power request |

NICo's overview mentions gravity-aware handling of trays below a leak source; the operations
guide documents the per-leaking-tray default. The scenario asserts the documented default until
[Q18](../spec/11-decisions.md#q18) closes. The consumer's README and the AsyncAPI schema name the
point types differently in places (`Sensor/LeakDetect` vs `Rack/RackLeakDetect`, `LeakDetectRack`);
the twin publishes what the **AsyncAPI** defines and validates every message against it (D40).
Read on 2026-10-18, NICo's consumer source (`messages.rs`, `mqtt_consumer.rs`) parses an older
shape — point types `LeakDetectRack`, `LeakSensorFaultRack`, `LeakDetectRackTray`, metadata
`rackName`/`rackID`, timestamps in seconds — so an opt-in `nico-legacy` mirror publishes the rack
leak points in that shape too (stack output `bmsConsumerMirror: nico-legacy`).

The isolation request above the threshold is nico-api's own (`crates/rack/src/bms_client.rs`
through `crates/bms-dsx-exchange`, read at `02b92f18`), and it reads a third shape. nico-api learns
where to publish a rack's request only from metadata under `BMS/v1/PUB/Metadata/#` that carries
`integration` (required of every metadata message, so each BMS-published point counts as a parse
error), `rackName` and `rackId` (not the AsyncAPI's `rackLocationName`/`rackLocationId`); it
publishes `{value: 0|1, timestamp: epoch ms, quality: "1"}` — quality a string — on
`BMS/v1/{integration}/Value/{objectType}/{pointType}/{tagPath}` derived from that metadata topic,
on change and every 100 s, keyed by its own rack id. It parses `HeartbeatTimestampIntegration`
metadata (`pointType`, `objectType`, `integration`) and heartbeats every 5 s, and reads no
isolation status. An opt-in `nico-api` mirror publishes each isolation request's metadata in that
shape on a topic of its own (`…/{tagPath}/nico-api`) and the twin's BMS acts on nico-api's
requests there as on AsyncAPI ones; the heartbeat metadata needs no mirror, since the AsyncAPI's
shape, which the BMS publishes, is enough for nico-api. A NICo stack names both mirrors:
`bmsConsumerMirror: "nico-legacy,nico-api"` (D40).

**NICo's own test tooling**, reused as oracles and references, not as the engine
([D7](../spec/11-decisions.md#d7--the-core-owns-its-redfish-engine-upstream-mocks-are-oracles), [Q2](../spec/11-decisions.md#q2)):
`crates/bmc-mock` (about 20 hardware profiles with a libvirt backend, an IPMI simulator, SSE and a
GET-only NVUE mock), `crates/machine-a-tron` (impersonates the scout and DPU agents, DHCP relays,
switches and power shelves: the impersonator), `ufm-mock`, `nmxc-mock`, `rms-mock`, `mockdpa`,
`dev/deployment/devspace`, and the sushy-tools and QEMU/swtpm development guides.

## DOCA Platform Framework (DPF)

`NVIDIA/doca-platform`, Apache-2.0. Provisions and operates BlueField DPUs from Kubernetes, and
runs DPU services (HBN, OVN, SNAP) on them through a DPU cluster the DPUs join. Read at `v26.8.0`
(`f1b50e8`, 2026-10-10). Two modes: **host-trusted** (a host agent pod on each host drives the DPU
through rshim and DOCA's DMS) and **zero-trust** (DPF reaches only the DPU's own BMC, over
Redfish; the hosts stay outside DPF's cluster). The twin runs zero-trust (`compat/dpf`).

What zero-trust provisioning asks of a BlueField-3 BMC, in order (`internal/provisioning/controllers`:
`dpu/discovery`, `dpudevice`, `dpu/state/redfish`):

| Step | DPF does | The BMC must |
|---|---|---|
| Discovery | crawls an IP range: `GET /redfish/v1`, then `root` with the shared secret or the factory `0penBmc`, `GET Chassis/Card1` for serial and OPN | answer as a BlueField BMC (`Product` without `B4`/`BlueField-4` is BF3), refuse a wrong password |
| Factory reset (default policy `OnInitialization`) | `Manager.ResetToDefaults` `ResetAll`, waits 60 s, then expects the factory password back (accepting `403 PasswordChangeRequired`) | reset its accounts, certificates and settings and reboot |
| Credentials | changes the factory password (`PATCH Accounts/root`) | |
| BMC firmware | below `BF-24.10-17`: pushes `bf3-bmc-24.10-17_opn.fwpkg` to `UpdateService` (octet-stream), polls the task, `Manager.Reset` | take a PLDM package, run a task, report the new `BMC_Firmware` version |
| Clock | compares Manager `DateTime` with its own | keep time |
| Mutual TLS | installs its CA in `Managers/…/Truststore/Certificates`, `CertificateService.GenerateCSR`, has cert-manager sign it, `ReplaceCertificate`, `PATCH AccountService` `Oem.OpenBMC.AuthMethods.TLS`; from then on verifies the BMC against its CA with the BMC's address as identity and authenticates with a client certificate (CN `root`) | generate the key, serve the installed certificate, verify client certificates against the Truststore, map CN to an account, answer these writes **200** (bmcweb's Success) |
| Device discovery | `Systems/Bluefield/Oem/Nvidia` (mode), `Chassis/Card1`, `FirmwareInventory/DPU_BOARD` (PSID), `NetworkDeviceFunctions/eth0f0` (PF0 MAC), SecureBoot | |
| InitializeInterface, ConfigFWParameters | mode (and `Mode.Set` when in NIC mode), Secure Boot, capacity from the part number; `HostRshim.Set` Disabled, `Oem/Nvidia` `BmcRShimEnabled` true, read back | keep and report the rshim owner |
| PrepareBFB | renders a bf.cfg per DPU (cloud-init, a `kubeadm join` for the DPU cluster with a bootstrap token it creates there, the DPU agent's identity) into its bfb-registry | |
| OS Installing | `UpdateService.SimpleUpdate` to `DPU_OS` with `ImageURI` `<registry>/bfb/??<bfb>,bfcfg/<dpu>?/bfb-to-install` (the BFB with the bf.cfg appended), polls the task to 100% | fetch the image, stream it to the DPU over rshim (the boot stream resets the Arm into the BFB's installer), report progress; `400 Another update is in progress` while busy |
| Then | waits for its **DPU agent** (`cmd/dpuagent`, on the new DPU OS) to report, configures the DPU through it, waits for the DPU to join the DPU cluster as a Node, deploys DPU services (HBN) there through ArgoCD. The agent arrives through the bf.cfg: the installer writes DPF's cloud-init into the new OS, and on its first boot cloud-init brings `oob_net0` up by DHCP, trusts DPF's CA, apt-installs `dpu-agent` from the bfb-registry's `/deb` repository (an arm64 package DPF builds from its source with nfpm, `make deb-dpuagent`, into the registry's image) and starts it with a bootstrap kubeconfig for the host cluster | at `vm` the DPU OS's own (D52): the agent reports, and stops at its first ConnectX operation; with those skipped, the DPU joins and is Ready |

| It needs | The twin provides | Notes |
|---|---|---|
| BlueField BMCs on the management network, Redfish over HTTPS | The `nvidia/bluefield` dialect on each DPU sub-device, with the `bluefield` adapter in front (certificates and mutual TLS, ResetToDefaults, the BFB push, BMC firmware packages, the OEM writes, bmcweb's 200s); reached through the site gateway | everything the table above asks of a BMC, verified with DPF itself (below) |
| A DPU OS: the BFB's installer, Ubuntu with DOCA, the DPU agent, kubelet, HBN's containers | At `model`, nothing: a pushed BFB is transferred and checked. At `vm` (D52), the Arm: a QEMU aarch64 machine the push installs with the BFB's own installer and the bf.cfg DPF appended, booting GRUB's entry with `virt`'s early console first, `oob_net0` on the management network | `compat/dpf` `DPU=vm`: DPF's first boot installs `dpu-agent` from the registry's `/deb` and the agent reports to DPF; it stops at `nvconfig` (no ConnectX), or, told to skip the ConnectX's steps, joins the DPU cluster and the DPU is Ready; HBN is not deployed ([Q1](../spec/11-decisions.md#q1)) |
| Its own cluster: cert-manager, the DPF operator's deployments, a DPU cluster (Kamaji or static), ArgoCD for DPU services | `compat/dpf` runs the provisioning controller, the static cluster manager and cert-manager as processes on envtest; the operator's work is done from its own manifests | host-trusted mode would need a kubelet per host for the host agent |

DPF's own test doubles are **impersonators** if used ([D24](../spec/11-decisions.md#d24--kwok-and-vendor-agent-mocks-are-impersonation)):
the `mock` install interface (`--dpu-install-interface=mock` skips every hardware step), the
DPUDevice label `skip-hw-provisioning`, the DMS mock (`test/mock/dms`) and the in-tree Redfish
mock (`internal/provisioning/controllers/dpu/state/redfish/mock`, which was the reference for the
shapes DPF expects). `compat/dpf` uses none of them.

## DSX Exchange

`dsx-ai-factory/dsx-exchange`. The IT/OT event bus.

| It needs | The twin provides | Notes |
|---|---|---|
| Publishers on its NATS bus over **MQTT 3.1.1 only** (optional CloudEvents), through its auth callout | BMS and grid publishers in the `nvidia/dsx-exchange` and `nvidia/dsx-flex` topic dialects, driven by the facility model; credentials from the stack's `outputs` | Topics from its AsyncAPI schemas: `BMS/v1/PUB/{Value,Metadata}/{objectType}/{pointType}/{tagPath}` (objectType AHU, ATS, BESS, Breaker, CDU, Chiller, Pump, Rack, Sensor, UPS, Valve, …), `grid/v1/…`, `Weather/v1/PUB/…`, `NICO/v1/machine/{id}/state`. Several schema files (leak response, break-fix, node readiness) were empty stubs when read |
| Its own services (NATS, auth callout, optional federation and MCP bridge) | Service zone | Its `make dummy-bms` replays a recorded CSV and seeds the twin's BMS point list |

Exchange defines **no BACnet, Modbus or SNMP adapters**. Field-protocol translation is left to the
BMS vendor, so the twin publishes at the MQTT level first.

## NVSentinel

`NVIDIA/NVSentinel`, Apache-2.0. GPU fault detection and remediation on Kubernetes. Node-local
health monitors publish health events over gRPC to `platform-connectors`, which stores them
(MongoDB or PostgreSQL) and sets Node conditions; `fault-quarantine` cordons by CEL rule,
`node-drainer` evicts, `fault-remediation` creates a maintenance resource (`RebootNode`,
`GPUReset`, `TerminateNode`) that `janitor` carries out through `janitor-provider`: a cloud API, or
on bare metal the `label` provider, which labels the Node (`nke.nvidia.com/reboot`) for the site's
lifecycle manager and waits for the label to go and the Node's boot ID to change. Read at
`v1.27.0` (`3b46832`).

| It needs | The twin provides | Notes |
|---|---|---|
| journald holding the NVIDIA driver's kernel lines (Xid, SXid, "fallen off the bus", mlx5): `syslog-health-monitor`, a cgo build against libsystemd, reads `_TRANSPORT=kernel` entries from a journal directory | **The in-host agent** (`internal/inhost`): a fault's `kernelLog` line printk'd into an `os: twin` guest's kernel by the agent's kernel module (`dctwin-kmsg`, built for the guest image's kernel), a kernel record with facility `kern` as the driver's are, and the guest's kernel log carried back out record by record as the journal entries journald makes of them (`journal.export`), which `systemd-journal-remote` writes into the journal file the monitor reads | No journald runs in the twin's guest. Without the module the line goes through `/dev/kmsg`, where the kernel gives it facility `user`; the twin then enters it as a driver's printk and marks it `DCTWIN_FAULT` ([D50](../spec/11-decisions.md#d50--the-in-host-agent-speaks-on-the-serial-console-a-faults-kernel-line-is-printkd-by-its-kernel-module)) |
| A DCGM host engine (:5555) and NVML: `gpu-health-monitor` (Python, DCGM's bindings), `metadata-collector` (GPU UUIDs by PCI address), `labeler` | **Mokka's NVML** with the server's GPUs as the model holds them (`dctwin-mokka render`/`sync`), in a Debian guest beside the `os: twin` guest, where the monitor runs its in-process DCGM 4.7.0 (the chart's `embedded-mode`); the twin guest's NVRM records are printk'd into that guest's kernel too, where DCGM's own kmsg reader looks for Xids | A second guest, because the twin's guest is musl; `compat/nvsentinel` with `BASE`. `metadata-collector` and `labeler` not run |
| NIC sysfs (`/sys/class/infiniband`, `/sys/class/net`) read by a **Go** monitor | A model-driven sysfs tree **bind-mounted** by the in-host agent, because Mokka's `LD_PRELOAD` surfaces are invisible to Go ([Q16](../spec/11-decisions.md#q16)) | Not built |
| Kubernetes Nodes for GPU hosts (`nvidia.com/gpu.present=true`) with a boot ID that changes on reboot; MongoDB or PostgreSQL; cert-manager for its TLS | The in-host agent registers an `os: twin` host's Node (**impersonated**, D24): Ready while the guest's agent is alive, the guest's own boot ID | A tenant cluster on `os: twin` hosts with a real kubelet is M3's; its Tilt setup's KWOK GPU nodes cover scale runs, labelled impersonated |
| On bare metal, a lifecycle manager that reboots what the `label` provider asks for | The host's BMC: a Redfish `ComputerSystem.Reset` resets the `vm` host's machine, which the twin observes | The manager itself is the stack's (NICo in DSX OS); `compat/nvsentinel` uses a labelled stand-in |

## Fleet Intelligence agent

`dsx-ai-factory/fleet-intelligence-agent` (`fleetint`, a fork of Lepton AI's `gpud`), Apache-2.0,
public: v1.5.0 (`a83abe9`) with `.deb`/`.rpm` packages, `ghcr.io` images and a Helm chart. Linux
only, cgo; the released binary needs only libc and dlopens NVML and DCGM. Run against the twin from
its `.deb` ([`compat/fleet-intelligence`](../../compat/fleet-intelligence/README.md)).

| It needs | The twin provides | Notes |
|---|---|---|
| The kernel's own `/dev/kmsg`, as root, for Xid (no journald); the device named from NVML's bus ids | The in-host agent printk's the catalogue's lines (`NVRM: GPU at PCI:…: GPU-<uuid>`, `NVRM: Xid (PCI:0000:38:00): 79, …`) into an `os: twin` guest's kernel through its kernel module (facility `kern`, D50) | That guest (Alpine's netboot initramfs, musl) cannot host the agent, so it runs in a Debian guest and the twin guest's records are entered into that guest's kernel through the same module, built there: a **stand-in** for the last hop only |
| NVML naming a GPU product (without it the Xid component reports Healthy, "NVIDIA NVML library is not loaded"); `libnvidia-ml.so.1` and `libcuda.so.1` where a driver install puts them | Mokka 0.5.0 on the Debian guest with a profile `dctwin-mokka render` writes from the model (names, UUIDs, serials, PCI ids, memory), faults through `dctwin-mokka sync` | fleetint's NVML component reads machine info every 5 minutes, and Mokka answers identity calls for a lost GPU, so only the Xid component reports an Xid 79 |
| A DCGM host engine for its DCGM components; `/sys/class/infiniband` | Nothing | DCGM components report "DCGM library is not loaded" |
| For **offline mode**, nothing more: `fleetint run --offline-mode --path … --format json\|csv` writes events and every component's health (`fleetint_logs_*.json`) and metrics; the local API answers on `/run/fleetint/fleetint.sock` | | Run this way, under the package's own unit |
| Optional enrolment with NVIDIA's backend (`fleetint enroll`); attestation through `nvattest` | Nothing yet | The backend is not open source; a stub is [Q9](../spec/11-decisions.md#q9). Attestation needs a confidential-computing GPU and cannot be emulated |

## AI Cluster Runtime (AICR)

`NVIDIA/aicr`. Version-locked, validated runtime recipes.

| It needs | The twin provides | Notes |
|---|---|---|
| A Kubernetes cluster with GPU nodes | KWOK nodes at `model` (AICR's own KWOK profiles cover H100, H200, GB200, GB300, B300 and VR200), labelled impersonated; or `os: twin` hosts with Mokka | `aicr validate --no-cluster` needs no cluster (RBAC and Jobs skipped). **`--snapshot` replaces live capture only; validator Jobs still deploy.** Its performance phase (NCCL all-reduce) needs real GPUs and cannot pass on the twin |

## GPU Operator, device plugin, GPU Feature Discovery, DRA driver, dcgm-exporter

`NVIDIA/gpu-operator` and its operands: `NVIDIA/k8s-device-plugin` (the device plugin and GPU
Feature Discovery), `NVIDIA/dcgm-exporter`, and the DRA driver for GPUs (`nvidia/dra-driver-nvidia-gpu`,
now `kubernetes-sigs/dra-driver-nvidia-gpu`), with Node Feature Discovery: the Kubernetes-enablement
and metrics slots of [06](../spec/06-packs-and-stacks.md#the-same-slots-every-vendor).

| It needs | The twin provides | Notes |
|---|---|---|
| A kubelet and containerd on each GPU host | **Container hosts** ([D49](../spec/11-decisions.md)): a Kind node per twin host, named `<rack>-<device>` as the kwok projection names it; or `vm` hosts with kubelets (M3) | The kwok projection (D24) impersonates the kubelet and runs none of these operands |
| NVML (`libnvidia-ml.so`), the driver tree (`/dev/nvidia*`, `/proc/driver/nvidia`), CDI specs, an `nvidia` runtime handler | NVIDIA's **Mokka** (`NVIDIA/k8s-test-infra`, nvml-mock 0.5.0) on each container host, its GPU profile rendered from the model (`internal/mokka`, `dctwin-mokka render`): the NVML name (D43), count, identity/v1 UUIDs and serials, the twin's PCI addresses, memory, VBIOS, the NVL domain's cluster UUID and clique, the tray's rack placement | Mokka creates its device nodes with `mknod`, which no user namespace allows: it needs a rootful container engine, so on a rootless host the cluster runs in a KVM guest |
| GPU health and Xid events through NVML's event set | The twin's faults as Mokka failure blocks (`dctwin-mokka sync`, through Mokka's override document; Xid 79 → `fallen_off_bus`, Xid 79) | Kernel-log Xid lines are not written on container hosts (the ring buffer is the Docker host's) |
| A GPU Operator to manage the driver and toolkit | Not needed: Mokka is the driver and registers the runtime; the operands run from their own charts at the operator `v26.3.3`'s versions | The operator itself runs against Mokka per Mokka's own guide (`driver`, `toolkit` disabled) |

The device plugin's and GFD's view of a host, and DRA's ResourceSlices, are the twin's GPUs: what
the twin's kwok projection says about the same host is held against them in `compat/gpu-operator`,
and disagreements are reported on whichever side they fall.

## KAI Scheduler, Run:ai, Dynamo, Grove, Cloud Functions

They need a Kubernetes cluster with accelerator resources. The twin provides KWOK (impersonated)
or Mokka-backed nodes. **Placement, quotas, gang admission and control flow are testable;
inference and training are not**, because no kernel runs.

| Component | Source (read 2026-10-19) | It needs | On the twin |
|---|---|---|---|
| KAI Scheduler | `kai-scheduler/KAI-Scheduler` (was `NVIDIA/KAI-Scheduler`), Apache-2.0 | Nodes with `nvidia.com/gpu` capacity and GFD labels, `nvidia.com/gpu.clique` for NVLink domains | `compat/kai`: v0.18.6, five components |
| Grove | `ai-dynamo/grove` (`NVIDIA/grove` redirects), Apache-2.0; `v0.1.0-alpha.16` | a topology-aware scheduler backend (KAI ≥ v0.15) and stable topology labels bound by a `ClusterTopologyBinding` | `compat/grove`: the operator with KAI, a `ClusterTopologyBinding` on `nvidia.com/gpu.clique`, disaggregated PodCliqueSets on two GB300 NVL72 domains |
| Dynamo | `ai-dynamo/dynamo`, `deploy/operator`, Apache-2.0; `v1.5.1` (its platform chart pins Grove alpha.13 and KAI v0.17.0; 1.5.x needs Grove ≥ alpha.13, KAI ≥ v0.17.0) | Grove and KAI (detected from the API), or LWS with Volcano | `compat/grove`: the operator renders a DynamoGraphDeployment into Grove's objects. Runtime (frontend, router, planner, NIXL) needs running pods |
| Run:ai | `dsx-ai-factory/run-ai`, Apache-2.0 monorepo (control plane, cluster services, installer); release `3.0.20` | its cluster side places through KAI Resource Management (KRM, `kai-scheduler/kai-resource-management`, pinned at `v0.18.3`) and a KAI scheduler per node pool; its control plane is about 50 services on NATS, PostgreSQL and Keycloak | `compat/runai`: KRM's three controllers, per-pool KAI shards and Run:ai's anyworkload-controller; with `controlplane.sh`, the control plane from its published chart (no NGC key needed) in a 7 GiB KVM guest, and Run:ai's agent, cluster-sync, external-workload-integrator and Karta operator connecting the twin's Nodes to it |
| Cloud Functions (NVCF) | `NVIDIA/nvcf` (moving to `dsx-ai-factory/nvcf`), Apache-2.0: control plane, invocation plane, the NVIDIA Cluster Agent (NVCA) and its operator, `nvcf-cli` | a control plane of Cassandra, NATS JetStream and OpenBao with the NVCF services; NVCA registered to it on each GPU cluster, with the GPU Operator (or Run:ai's fake GPU operator) and the SMB CSI driver; images and charts from NGC | **not run**: its self-hosted install pulls charts and images with an NGC API key, runs the control plane, NVCA and the functions NVCA deploys as pods on a k3d cluster with kubelets (the twin's kwok Nodes run none), claims host ports 8080, 8443 and 4222, and its smallest sized tier is 16 vCPU and 64 GiB (the k3d laptop flow is unsized). NVCA without a control plane has nothing to register with or receive work from |

## MaxLPS and DSX Flex

Neither can be run against the twin today, because neither is obtainable as software (read
2026-10-19). [DSX MaxLPS](https://docs.nvidia.com/dsx/maxlps/overview) is a design and operating
framework whose documentation requires NVOnline access; its software part, [NVIDIA Dynamic Power
Software (DPS)](https://docs.nvidia.com/datacenter/dps/versions/latest/index.html), is a developer
preview "not to be used in production except as part of an authorized collaboration".
[DSX Flex](https://docs.nvidia.com/dsx/flex/overview) is a set of specifications (power intent,
timing, scope, reported state, exception semantics at the grid-facing boundary), not an
installable component. They would need GPU, rack and facility power telemetry and caps, plus grid
signals; the twin can provide power-shelf and PDU readings, BMS and grid topics, and GPU power
through Mokka once DPS is available ([Q4](../spec/11-decisions.md#q4)).

## Capability requirements

The stack definition ([06 — Stacks](../spec/06-packs-and-stacks.md#stacks) shows it) requires,
with `gate: install`: one recognised Redfish dialect on every host-class device, `dhcp/relay-giaddr`,
and `host/os:twin` for the NVSentinel cluster's nodes. Everything else gates scenarios: `boot/pxe`
and `tpm2/measured-boot` preferred at `vm`; `fabric/ufm-rest` with InfiniBand; `fabric/nmx-c`,
`fabric/nmx-c-telemetry`, `fabric/rack-management` and `switch/nvos-ssh` with NVLink racks;
`facility/bms:nvidia/dsx-exchange` and `facility/isolation-request:nvidia/dsx-exchange`;
`dpu/bmc:nvidia/bluefield`, `dpu/os` and `dpu/hbn` only when `dpuMode` is on (and `dpu/hbn` cannot
be met until Q1 closes); `external/stub:nvidia-nras`.

## Conformance scenarios

| Scenario | Shows | Milestone |
|---|---|---|
| `bmc-discovery` | NICo discovers emulated BMCs of several dialects through relayed DHCP, identifies each as the intended hardware type, and pairs identities | M1 |
| `host-ingest-zero-dpu` | A `vm` host in `os: stack` mode goes from racked to ready: firmware baseline, measured-boot attestation, PXE install of NICo's images, credential rotation | M1 |
| `firmware-drift-quarantine` | A host whose firmware drifts from baseline is quarantined, updated through UpdateService and returned | M1 |
| `tenant-release-sanitise` | Release triggers NVMe secure erase, TPM clear and re-attestation, observed from the hardware side | M1 |
| `bmc-unreachable`, `bmc-credentials-rotated`, `psu-failure`, `link-flap` | Generic resilience | M1–M2 |
| `relay-and-bgp` | DHCP relayed by a real NOS by giaddr, and BGP unnumbered from a host to its leaves with NICo's ASN scheme | M2 |
| `xid-cordon` | An injected Xid 79 on an `os: twin` host makes NVSentinel cordon, drain and reboot; the same fault is visible in Redfish and power | M3 |
| `nvl72-ingest` (impersonated) | A full NVL72 rack at `model` is ingested with the scout impersonated and its scale-up domain formed | M4 |
| `switch-tray-failure` | A failed switch tray degrades the domain and blocks allocation; the switch tray's SSH and NVUE surfaces answer RMS | M4 |
| `nvl72-leak-response` | Both leak paths, as documented above | M5 |

## Verified on the twin (2026-10-19)

| What | How | Result |
|---|---|---|
| **NICo itself** | NICo at `02b92f18`, built from source and run unmodified (one file patched to compile on macOS only): nico-api (site explorer, state controllers, its DSX Exchange integration), forge-dhcp-server, forge-hw-health, the DSX Exchange consumer, NICo Flow with Temporal, Postgres and Vault (`compat/nico`) | **22 PASS, 0 FAIL, 3 KNOWN**: every BMC leased by NICo through the twin relay and explored; all 18 GB300 trays managed hosts paired with their BlueField-3, in DPU discovery; 9 NVLink switches and 8 power shelves `ready`; a tray leak powers the leaking switch tray off through the twin's RMS; two leaking trays make nico-api isolate the rack through the twin's BMS. KNOWN: GB300 serials (NICo's default Redfish client rejects a value of the GB200 capture), and two NICo-side behaviours |
| NICo's Redfish client against every NVIDIA and OEM dialect | `libredfish`'s own integration suite (`tests/integration_test.rs` at `a51ffb1`, unmodified) with its mockup server replaced by a forwarder to the twin's BMC of the same vendor (`compat/libredfish`) | 12 of 12 vendor tests pass: GB200, NVSwitch tray, BlueField, DGX H100, GH200, Lite-On, Delta, Dell, Dell with DPUs, HPE, Lenovo, Supermicro. The `forbidden` test parses a synthetic fixture no device produces |
| NMX-C and RMS | NICo's `libnmxc` and nv-rms-client's `librms`, unmodified, on both NVL72 domains (`compat/nico-fabric`) | partitions, Subscribe with a switch-tray failure and an Xid 79, placement, power, the V2 fabric manager, firmware and NVOS image jobs: passed |
| KAI Scheduler and AICR | KAI v0.18.6 (five components) with the real kube-controller-manager over the twin's Nodes; AICR v1.0.0 `snapshot`, `recipe`, `validate` (`compat/kai`, `compat/aicr`; Nodes impersonated, D24) | KAI: 23 PASS and 1 finding in KAI (it folds `amd.com/gpu` into NVIDIA GPUs); an 18-pod gang in one NVLink clique, faulted trays avoided. AICR: 8 PASS, 4 that need an OS or a kubelet |
| Grove and the Dynamo operator | Grove v0.1.0-alpha.16 and the Dynamo operator v1.5.1, built from their tags and run unmodified (Dynamo's in a container), with KAI v0.18.6 and the real kube-controller-manager over two GB300 NVL72 racks of Nodes (`compat/grove`; Nodes impersonated, D24) | **21 PASS, 2 FINDING, 2 CANNOT**: a 17-pod disaggregated PodCliqueSet replica (frontend, two prefill and two decode instances of four 4-GPU pods) packed into one NVLink clique, a second replica in the other, a third refused whole; under an Xid 79, a powered-off tray and a third fault the gang's replacements stay in its clique (or wait); a DynamoGraphDeployment becomes a PodCliqueSet packed on the same clique and recovers from a powered-off tray through non-graceful node shutdown. FINDINGS: a replica's scaled PodGangs are packed on their own and can span two domains (documented Grove limitation); KAI's podgroupcontroller binds `:8080` whatever its flag says. CANNOT: Available/Ready need running pods |
| Run:ai's cluster side | Run:ai 3.0.20's pinned KAI Resource Management v0.18.3 (nodepool-controller, project-controller, pod-group-assigner) with a KAI scheduler per node pool, and Run:ai's anyworkload-controller, over the lab estate's Nodes (`compat/runai`; Nodes impersonated, the KAI operator's shard status and the agent's Karta written by the harness) | **15 PASS, 1 FINDING, 1 CANNOT**: node pools by GPU product, department and project quotas as KAI queues, an 18-pod training gang placed on GB300 in one clique without naming a scheduler, queue or pool; a WorkloadRunner rendered into its Job and moved to a GB200 clique while GB300 is full; inference held to its queue's deserved quota (`NonPreemptibleOverQuota`); a drained Xid tray's replacement waits and binds on recovery; with every H100 server off, a service asking for h100 then gb200 lands on GB200. FINDING: KRM's node pool stays `Ready` when its Nodes go not ready (its Node watch ignores the Ready condition) |
| **Run:ai's control plane** | Run:ai 3.0.20's control plane from its published chart and images, unmodified, one replica of each service but NATS, on k3s in a Debian 13 KVM guest (6 vCPU, 7 GiB); the cluster side above in the same guest with KAI and KRM given Run:ai's names, and Run:ai's agent, cluster-sync, external-workload-integrator and the workload-map (Karta) operator v0.2.6 built from their tags, connected with the cluster's own client (`compat/runai/controlplane.sh`; Nodes impersonated, go-operator's `runai-public` report written by the harness) | **6 PASS**: the cluster registered through Run:ai's API connects and reports the twin's 50 nodes and 192 GPUs; a node pool and a project with 72 GB300 GPUs deserved, made through the API, become KRM's NodePool (with its KAI shard) and Project (with its namespace and queue); a Job of 18 × 4 GPUs submitted with `POST /api/v2/workloads` is bound by KAI on 18 GB300 trays in one NVLink clique, and the control plane lists its pods with their Nodes. Findings: the agent can panic on its first start (a cache race, 2 of 5 starts; a pod would restart); three services take `:8080`. The Run:ai pods use about 1.85 GiB, Keycloak 0.57 of it |
| DCGM exporter consumers | Prometheus 3.15.0 scraping the twin's exporters through the site gateway, with accelerator alert rules (`compat/prometheus`; exporters impersonated) | 17 of 17: Xid 79 as `DCGM_FI_DEV_XID_ERRORS … 79` firing `GPUXidError` and `GPUFellOffTheBus`; `modelName` equals the Nodes' `nvidia.com/gpu.product` |
| NICo's hardware-type selection | the twin presents `NVIDIA`/`GB BMC` with a `Chassis_0` model `NVIDIA GB300`, `GB200 NVL`, `P3809`, `Nvidia` with system `Bluefield`, and Lite-On by chassis manufacturer; NICo's rules transcribed and NICo's own classifier (`bmc-explorer-cli`) | every NVL72 BMC identified as its intended type |
| Relayed DHCP only | ISC Kea 2.6.2 and NICo's forge-dhcp-server behind the twin relay (option 82 circuit id, remote id, link selection, relay source port); FRR with ISC dhcrelay as a real NOS in front of Kea on the TAP substrate | every BMC leased, each with its own circuit id |
| UFM REST | NICo's UFM client's request sequence reproduced against the twin's UFM (`/ufmRestV3` token and `/ufmRest` basic) | ports and GUIDs match the model; a pulled cable reads down; partitions bind and unbind |
| DSX Exchange BMS | a NATS server with MQTT; AsyncAPI validation of every message; NICo's consumer and nico-api against the twin-hosted broker | rack leak points, CDU points, isolation request and status; both NICo readers |
| Both leak paths and isolation | `nvl72-leak-response` and `tray-leak-isolation` against the reference stack, and the same paths through NICo above | passed |
| **DPF**, zero-trust | DPF `v26.8.0` (`f1b50e8`), built from its tag and run unmodified: the provisioning controller and static cluster manager as processes, cert-manager `v1.21.2`, envtest host and DPU clusters, the HBN guide's DPFOperatorConfig, DPUDiscovery, BFB (the 1.7 GB `bf-bundle-3.5.0-89_26.07…64k`) and DPU flavor; two BlueField-3 BMCs behind the `bluefield` adapter (`compat/dpf`) | **16 PASS, 2 CANNOT**: discovery through the site gateway (only the BlueField BMCs adopted), factory reset, credentials, a BMC firmware update with NVIDIA's own package (BF-24.10-10 → BF-24.10-17), mutual TLS with DPF's CA (CSR from the BMC's key, cert-manager-signed certificate served, client certificate verified), device discovery, InitializeInterface, ConfigFWParameters (rshim), PrepareBFB, and the BFB transferred to both DPUs (49 images checked) at OS Installing. CANNOT: the DPU agent on the DPU OS, the DPU cluster join and HBN (no DPU OS at `model`, Q1). **`DPU=vm`** (server-01's DPU an Arm the BMC's push installs, D52; the whole run in a 9G memory cap): the BFB's installer runs on the Arm with DPF's bf.cfg (file-backed 8 GB, with the clusters stopped), the DPU OS boots as DPF's named DPU, DPF's first boot installs `dpu-agent` 26.8.0 (built from DPF's source) from the registry's `/deb`, and **the agent reports to DPF** (P7 PASS: OS Installing → DPU Config). It stops at its first ConnectX operation, `nvconfig` (`no physical ports from devlink (p0/p1)`): no DPU Node (P8 FAIL). With `AGENT_SKIP=connectx` (the flavor tells the agent to skip the NVConfig, the reboot, kernel parameters, SFs, VF MACs and OVS script) **the DPU joins the DPU cluster as a Ready Node and DPF puts it in service, Ready**: 26 checks, 25 PASS, 1 CANNOT (HBN: DPF's DPUService controller and Argo CD not run; its pod asks 6Gi and four SFs of a Node with 3.3 GiB and none) |
| GPU Operator operands, DRA driver, NFD | k8s-device-plugin and GFD v0.19.3, dcgm-exporter 4.5.3-4.8.2 (the operator v26.3.3's versions), DRA driver 0.5.0 and NFD 0.19.0, unmodified, on Kind nodes named for a GB300 tray and an H100 server, with Mokka 0.5.0 profiles rendered from the model and twin faults carried into Mokka (`compat/gpu-operator`; container hosts, D49) | 20 PASS, 0 FAIL, 18 FINDING: the twin's GPUs in `nvidia-smi` (node, device-plugin pod, DRA-claimed pod), allocatable, GFD labels (equal to the kwok projection, clique included), ResourceSlices and dcgm-exporter identity; Xid 79 in NVML 228 ms after the twin injects it. Findings: the device plugin withdraws every GPU of the tray on one Xid 79 and keeps them until it restarts (the projection withholds one), the DRA driver and DCGM on Mokka do not react, label and metric-set differences against the impersonations |
| **NVSentinel** (`compat/nvsentinel`) | v1.27.0 (`3b46832`), built from its tag and run unmodified (platform-connectors, the `systemd` build of syslog-health-monitor, fault-quarantine, node-drainer, fault-remediation, janitor, janitor-provider with the `label` provider, and `gpu-health-monitor` on DCGM 4.7.0 in the chart's `embedded-mode`), configured from its own chart, PostgreSQL, the real kube-controller-manager; a Dell XE9680 at `vm` (`os: twin`, x86_64 under KVM) into whose kernel the in-host agent's module printk's the fault's lines (facility `kern`) and whose kernel log the agent carries into the node's journal (`systemd-journal-remote`); with `BASE`, the node's GPUs in a Debian guest, Mokka's NVML kept equal to the model, the NVRM records entered into that kernel too (**stand-in**, last hop); the Node is the agent's (impersonated, D24) | `xid-cordon` end to end: an Xid 79 injected into the twin is a kernel record in the guest and an entry in the journal, NVSentinel reports it (`SysLogsXIDError`, `RESTART_BM`), cordons, drains, asks for a reboot, which a stand-in for the lifecycle manager sends to the server's iDRAC; the twin observes the Redfish reset and QEMU's, the guest boots with a new boot ID, the janitor completes the RebootNode. `gpu-health-monitor` sees the model's eight GPUs healthy, reports an Xid 48 (`DCGM_FR_VOLATILE_DBE_DETECTED`, fatal) and the node is cordoned again; an AC cycle clears Xid 79 and every monitor reports healthy. **23 PASS, 3 FINDING**. FINDINGS: DCGM's health watch never sees the Xid 79 (NVML's event names a device `nvmlDeviceGetIndex` reports lost; its kmsg reader's Xid is dropped with the loop's buffer); after the warm reboot the model keeps `gpu-2` dead and DCGM lists 7 GPUs, but no health signal exists for a lost GPU and the monitor publishes `gpu-2` healthy, so the quarantine lifts; the syslog monitor's post-reboot scan skips every entry of the current boot (`isStaleBootEntry` compares go-systemd's `_BOOT_ID=<hex>` with `/proc`'s dashed boot ID) |
| **Fleet Intelligence agent** | `fleetint` 1.5.0 (`a83abe9`) from its released `.deb`, unmodified, under its own unit in offline mode, in a Debian 13 KVM guest whose eight H100s are the XE9680's (`b01/server-01`) through Mokka 0.5.0 profiles rendered from the model; the XE9680 at `vm` (`os: twin`) in the twin, its in-host agent's module printk'ing the Xid lines into that guest's kernel, the records entered into the fleetint guest's kernel through the same module (**stand-in**, last hop; facility `kern` in both) (`compat/fleet-intelligence`) | **18 PASS, 0 FAIL, 2 FINDING, 3 CANNOT**: the twin's GPUs listed (UUIDs, serials, PCI ids, product, memory) and healthy; Xid 79 on `gpu-2` reported 212 ms after its line, Unhealthy, `Fatal`, `REBOOT_SYSTEM`, against the model's UUID; Xid 48 on `gpu-5` likewise; the GPU lost in NVML; kept until the reboot fleetint suggests, then Healthy. A twin bug in the Mokka sync found and fixed. FINDING: its NVML component does not see the lost GPU (5-minute machine-info refresh; Mokka answers identity calls); Xids are kept until a reboot. CANNOT: DCGM, the backend, the server around the GPUs |

What has not been run of DSX OS: the DPU agent's ConnectX operations (NVConfig, SFs, VF MACs,
OVS-DOCA) and HBN (Q1), NVSentinel's
metadata collector and labeler (its DCGM-based GPU health monitor runs, in a second guest whose
NVML is Mokka's, with `BASE`),
the Fleet Intelligence agent's DCGM components, enrolment and backend, NICo's own NSM
and PSM (the twin's RMS stands where NICo's deployment puts RMS), Run:ai's `go-operator` and
`admission-controller` (their parts are played by the harness and by KRM), Cloud Functions (see
[the table above](#kai-scheduler-runai-dynamo-grove-cloud-functions)), and the GPU Operator's own
controller and its driver, toolkit and validator operands (its device plugin, GFD and
dcgm-exporter ran from their own charts).

## What passing this stack on the twin does not show

- That BlueField-enforced isolation holds. HBN does not run: the DPU OS runs at `vm` and the DPU
  reaches Ready, but on a machine with no ConnectX, whose steps the agent skips (Q1).
- That firmware updates are safe on real devices. Update services are modelled, not flashed.
- Anything about performance: NCCL, NVLink bandwidth, InfiniBand, inference throughput.
- GPU, SPDM and confidential-computing attestation.
- That vendor BMC quirks are complete. Dialects carry the quirks recorded in the fidelity ledger
  and no others.
- Anything from an impersonated run.

## Sources

- [dsx-ai-factory/infra-controller](https://github.com/dsx-ai-factory/infra-controller): `README.md`, `docs/overview/*`, `docs/hcl.md`, `docs/faq.md`, `docs/operations/leak-detection-handling.md`, `docs/getting-started/prerequisites/network.md`, `crates/bmc-explorer`, `crates/bmc-mock`, `crates/machine-a-tron`, `crates/dsx-exchange-consumer`, `crates/scout`, `crates/ib-fabric`
- [dsx-ai-factory/libredfish](https://github.com/dsx-ai-factory/libredfish): `src/lib.rs` (the trait), `src/model/service_root.rs`, vendor modules
- [dsx-ai-factory/nv-rms](https://github.com/dsx-ai-factory/nv-rms)
- [dsx-ai-factory/dsx-exchange](https://github.com/dsx-ai-factory/dsx-exchange): `schemas/asyncapi/`, `docs/bms-integration.md`
- [NVIDIA/NVSentinel](https://github.com/NVIDIA/NVSentinel): `demos/local-fault-injection-demo`, `tilt/`, `docs/nic-health-monitor.md`
- [dsx-ai-factory/fleet-intelligence-agent](https://github.com/dsx-ai-factory/fleet-intelligence-agent)
- [NVIDIA/aicr](https://github.com/NVIDIA/aicr): `docs/user/validation.md`, `docs/user/cli-reference.md`, `kwok/`
- [NVIDIA/k8s-test-infra](https://github.com/NVIDIA/k8s-test-infra) (Mokka; read at `v0.5.0`, `e2950d58`): `docs/components/libraries-and-shims.md`, `docs/architecture.md`, `docs/nvml-mock-ctl.md`, `docs/configuration.md`, `docs/helm-chart.md`, `docs/guides/{gpu-operator,dra,device-plugin}.md`, `tests/e2e/VERSION-MATRIX.md`, `pkg/gpu/mockctl`, `internal/agent/gpudriver`
- [NVIDIA/doca-platform](https://github.com/NVIDIA/doca-platform) at `v26.8.0`: `internal/provisioning/controllers` (`dpu/discovery`, `dpudevice`, `dpu/state`, `dpu/state/redfish`), `docs/public/user-guides/zero-trust/use-cases/hbn`, `internal/operator/inventory/manifests/provisioning-controller.yaml`, `test/objects/infrastructure`, `test/mock/`
- [sonic-net/sonic-buildimage](https://github.com/sonic-net/sonic-buildimage): `dockers/docker-dhcp-relay/`
- [ai-dynamo/grove](https://github.com/ai-dynamo/grove) at `v0.1.0-alpha.16`: `docs/user-guide/topology-aware-scheduling.md`, `docs/proposals/244-topology-aware-scheduling`, `docs/proposals/525-KAI-Scheduler-Backend`, `operator/internal/scheduler/kai`
- [ai-dynamo/dynamo](https://github.com/ai-dynamo/dynamo) at `v1.5.1`: `deploy/operator`, `deploy/helm/charts/platform`, `docs/fern/pages/kubernetes/installation/multinode-orchestration.md`, `examples/backends/vllm/deploy/disagg-multinode.yaml`
- [dsx-ai-factory/run-ai](https://github.com/dsx-ai-factory/run-ai) at `3.0.20`: `README.md`, `docs/developer/architecture.md`, `docs/developer/services.md`, `local-dev.md`, `runai-cluster/kai-scheduler/kai-version.yaml`, `runai-cluster/cluster/dromos`, `runai-cluster/cluster/{agent,cluster-sync,external-workload-integrator,go-operator}`, `runai-cluster/cluster/runai-chart`; its published control-plane chart `helm.ngc.nvidia.com/nvidia/runai-oss/charts/control-plane-3.0.20.tgz`
- [dsx-ai-factory/workload-map](https://github.com/dsx-ai-factory/workload-map) (Karta) at `v0.2.6`: `operator/`, `charts/karta`
- [kai-scheduler/kai-resource-management](https://github.com/kai-scheduler/kai-resource-management) at `v0.18.3`: `docs/overview.md`, `docs/concepts/*`, `pkg/operator/operands`, `pkg/nodepool-controller`, `pkg/pod-group-assigner`
- [NVIDIA/nvcf](https://github.com/NVIDIA/nvcf): `README.md`, `docs/self-managed/installation.md`, `docs/dev/local-development.md`, `docs/overview/infrastructure-sizing.md`, `src/compute-plane-services/nvca/README.md`
- [NVIDIA DSX OS announcement](https://developer.nvidia.com/blog/nvidia-dsx-os-delivers-open-modular-software-for-operating-ai-factories-at-scale/)

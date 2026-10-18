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
| **BlueField DPUs** as paired sub-devices: DPU BMC (Redfish), DPU OS, DPU agent, HBN driven through NVUE, `lldpd` | The `nvidia/bluefield` dialect on a sub-device; a DPU OS stand-in at `vm` | **The largest gap:** no public way to run a BFB without hardware ([Q1](../spec/11-decisions.md#q1)); `dpuMode: false` until it closes |
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
the twin publishes what the **consumer** reads and tests against it.

**NICo's own test tooling**, reused as oracles and references, not as the engine
([D7](../spec/11-decisions.md#d7--the-core-owns-its-redfish-engine-upstream-mocks-are-oracles), [Q2](../spec/11-decisions.md#q2)):
`crates/bmc-mock` (about 20 hardware profiles with a libvirt backend, an IPMI simulator, SSE and a
GET-only NVUE mock), `crates/machine-a-tron` (impersonates the scout and DPU agents, DHCP relays,
switches and power shelves: the impersonator), `ufm-mock`, `nmxc-mock`, `rms-mock`, `mockdpa`,
`dev/deployment/devspace`, and the sushy-tools and QEMU/swtpm development guides.

## DOCA Platform Framework (DPF)

`NVIDIA/doca-platform`. Provisions and operates BlueField DPUs from Kubernetes.

| It needs | The twin provides | Notes |
|---|---|---|
| DPU BMC (Redfish), DPU OS, DPU-side agent | The `nvidia/bluefield` dialect on the sub-device; a DPU stand-in | DPF's `test/mock/dpuagent` (one pod per DPU, serving the DPU BMC's Redfish and the real bootstrap exchange) is the reference for what a stand-in must answer, and is itself an **impersonator** if enabled ([D24](../spec/11-decisions.md#d24--kwok-and-vendor-agent-mocks-are-impersonation)) |

## DSX Exchange

`dsx-ai-factory/dsx-exchange`. The IT/OT event bus.

| It needs | The twin provides | Notes |
|---|---|---|
| Publishers on its NATS bus over **MQTT 3.1.1 only** (optional CloudEvents), through its auth callout | BMS and grid publishers in the `nvidia/dsx-exchange` and `nvidia/dsx-flex` topic dialects, driven by the facility model; credentials from the stack's `outputs` | Topics from its AsyncAPI schemas: `BMS/v1/PUB/{Value,Metadata}/{objectType}/{pointType}/{tagPath}` (objectType AHU, ATS, BESS, Breaker, CDU, Chiller, Pump, Rack, Sensor, UPS, Valve, …), `grid/v1/…`, `Weather/v1/PUB/…`, `NICO/v1/machine/{id}/state`. Several schema files (leak response, break-fix, node readiness) were empty stubs when read |
| Its own services (NATS, auth callout, optional federation and MCP bridge) | Service zone | Its `make dummy-bms` replays a recorded CSV and seeds the twin's BMS point list |

Exchange defines **no BACnet, Modbus or SNMP adapters**. Field-protocol translation is left to the
BMS vendor, so the twin publishes at the MQTT level first.

## NVSentinel

`NVIDIA/NVSentinel`, Apache-2.0. GPU fault detection and remediation on Kubernetes.

| It needs | The twin provides | Notes |
|---|---|---|
| DCGM host engine (:5555) and NVML | Mokka in `os: twin` hosts, under the GPU Operator's DCGM | NVSentinel's own `nvsentinel-fake-dcgm` (DCGM with NVML injection) and Mokka's NVSentinel guide are references |
| journald: Xid, SXid, "fallen off the bus", mlx5 NIC errors | The in-host agent writing fault templates to the kernel log | |
| NIC sysfs (`/sys/class/infiniband`, `/sys/class/net`) read by a **Go** monitor | A model-driven sysfs tree **bind-mounted** by the in-host agent, because Mokka's `LD_PRELOAD` surfaces are invisible to Go ([Q16](../spec/11-decisions.md#q16)) | |
| Kubernetes; MongoDB or PostgreSQL; cert-manager | A tenant cluster the twin builds from the stack definition's `clusters` on `os: twin` hosts | Its Tilt setup's KWOK GPU nodes cover scale runs, labelled impersonated |

## Fleet Intelligence agent

`dsx-ai-factory/fleet-intelligence-agent` (built on `gpud`).

| It needs | The twin provides | Notes |
|---|---|---|
| DCGM, NVML, Xid, InfiniBand, system metrics | Mokka in `os: twin` hosts | Runs in **offline mode** (local API, metrics, files) or exports OTLP to a collector the stack provides |
| Optional enrolment with NVIDIA's backend; attestation through `nvattest` | Nothing yet | The backend is not open source; a stub is [Q9](../spec/11-decisions.md#q9). Attestation needs a confidential-computing GPU and cannot be emulated |

## AI Cluster Runtime (AICR)

`NVIDIA/aicr`. Version-locked, validated runtime recipes.

| It needs | The twin provides | Notes |
|---|---|---|
| A Kubernetes cluster with GPU nodes | KWOK nodes at `model` (AICR's own KWOK profiles cover H100, H200, GB200, GB300, B300 and VR200), labelled impersonated; or `os: twin` hosts with Mokka | `aicr validate --no-cluster` needs no cluster (RBAC and Jobs skipped). **`--snapshot` replaces live capture only; validator Jobs still deploy.** Its performance phase (NCCL all-reduce) needs real GPUs and cannot pass on the twin |

## KAI Scheduler, Run:ai, Dynamo, Grove, Cloud Functions

They need a Kubernetes cluster with accelerator resources. The twin provides KWOK (impersonated)
or Mokka-backed nodes. **Placement, quotas, gang admission and control flow are testable;
inference and training are not**, because no kernel runs.

## MaxLPS and DSX Flex

No public repository was found when this was read. They need GPU, rack and facility power
telemetry and caps, plus grid signals. The twin can provide Mokka power limits, power-shelf and
PDU readings, and grid topics. Whether they can be obtained, and whether they run against a
shimmed NVML, is [Q4](../spec/11-decisions.md#q4).

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

## What passing this stack on the twin does not show

- That BlueField-enforced isolation holds. HBN runs on a stand-in, if at all.
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
- [NVIDIA/k8s-test-infra](https://github.com/NVIDIA/k8s-test-infra) (Mokka): `docs/components/libraries-and-shims.md`
- [NVIDIA/doca-platform](https://github.com/NVIDIA/doca-platform): `test/mock/`
- [sonic-net/sonic-buildimage](https://github.com/sonic-net/sonic-buildimage): `dockers/docker-dhcp-relay/`
- [NVIDIA DSX OS announcement](https://developer.nvidia.com/blog/nvidia-dsx-os-delivers-open-modular-software-for-operating-ai-factories-at-scale/)

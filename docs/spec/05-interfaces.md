# 05 — Interfaces

Every interface is an adapter projecting the model ([02](02-architecture.md#adapters)). The
protocol engines are vendor-neutral and live in the core. Everything vendor-specific about a
protocol is a **dialect** supplied by a pack, and every engine's scope is **what the stacks under
test call** ([D25](11-decisions.md#d25--a-protocol-engines-scope-is-the-stacks-call-list-the-dialect-binding-is-data)).

## Dialects and quirks

A dialect is data plus small hooks:

| Part | Example |
|---|---|
| **Identity strings** | Redfish ServiceRoot `Vendor`, `Product` and `Oem` keys, the system and chassis `Id`s and the chassis `Model`; the IPMI manufacturer id; the DHCP vendor class (option 60); the LLDP chassis id. Lifecycle managers pick a code path from these **by exact match**: NICo's GB-series host path needs `Product: GB BMC` or `GB200 NVL` and tells GB300 apart by a chassis `Model: NVIDIA GB300`; its NVSwitch-tray path needs `P3809`; its BlueField path needs `Vendor: Nvidia` (mixed case) and a system Id of `Bluefield` or `BlueField_0`. A dialect reproduces them exactly, and the pack records where each came from ([stack doc](../stacks/nvidia-dsx-os.md#dialects-nico-recognises)) |
| **Mockup** | A captured, scrubbed resource tree that supplies the static shape ([04](04-devices-and-racks.md#capturing-a-real-device)) |
| **Binding** | Data that binds the mockup to the model: JSON-pointer overlays naming which properties are dynamic and which model field feeds each; and an **action map** from the dialect's actions and PATCHable properties to behaviours and [canonical request names](14-expressions.md#canonical-request-names). This is the whole of a dialect for most devices |
| **Request list** (`requests.yaml`) | The operations the stacks under test make against this dialect, with their canonical names. It is what the engine implements and what the Interop profile checks ([16](16-testing.md#oracles-per-adapter)) |
| **OEM resources and actions** | Dell's lockdown and job queue, HPE's OEM links, NVIDIA's DPU and NVSwitch-tray resources: a `DialectHandler` where data cannot express them |
| **Error shapes** | The message registries and status codes this vendor actually returns |
| **Quirks** | Behaviour real devices exhibit that a stack must survive, each **named and switchable**: power state reported late after a reset, a session limit, a 500 under concurrent requests, a task that completes before its monitor reports it, a boot override cleared by a firmware update. A profile lists the quirks on by default; a scenario can disable them to separate stack bugs from tolerance gaps |

## Management

| Interface | Projects | Implementation | Initial dialects |
|---|---|---|---|
| **Redfish** | What the stack's call list names. For NICo ([stack doc](../stacks/nvidia-dsx-os.md#what-nico-asks-a-bmc)): service root and inventory (Systems, Chassis, Managers, Processors, NetworkAdapters and Ports, PCIe, Drives), power and reset, boot order and overrides, Bios and pending settings, Secure Boot and its certificates, AccountService (create, delete, password change, policy), sessions, LockdownOEM actions, serial-console setup, UpdateService (SimpleUpdate and multipart) with TaskService or an OEM job queue, LogServices and SEL, EventService subscriptions and SSE, power and thermal metrics, GPU sensors, **leak detectors**, TPM clear, secure erase. Query options only in the shapes the call list uses | **The core's own Redfish engine** ([D7](11-decisions.md)), serving dialect mockups through the binding, pending [Q2](11-decisions.md#q2) | generic DMTF, OpenBMC, AMI; NVIDIA GB-series host, NVSwitch tray, BlueField, DGX ("Viking"); Liteon and Delta power shelves; Dell iDRAC, HPE iLO, Lenovo XCC (with the `Oem.Ami` variant), Supermicro — one completely at M0/M1 ([Q19](11-decisions.md#q19)), the rest as the roadmap needs them |
| **IPMI over LAN** | Chassis power, boot device, SEL, FRU, sensors | Core engine (IPMI 2.0, RMCP+); OpenIPMI's `lanserv` is the oracle and a candidate implementation | Manufacturer ids and OEM commands per dialect |
| **Serial console** | Serial over LAN, and SSH console on BMCs that offer it | Core engine. At `vm` the VM's serial port; at `model` the boot transcript the behaviour generates | — |

## Boot and addressing

| Interface | Projects | Implementation |
|---|---|---|
| **DHCP client** | A lease for every BMC, sub-device BMC and `model` host, from the stack's DHCP through the relays the fabric provides; vendor class per dialect | Userspace network stack, one endpoint per modelled switch port ([D20](11-decisions.md#d20--endpoints-are-hosted-per-modelled-switch-port)). DHCPv6 is owed: NICo ships a `dhcpv6` crate and a DHCPv6 deployment guide |
| **DHCP relay** | What a relayed stack needs: giaddr, option 82 circuit id and remote id, and link selection (RFC 3527) when asked | **A real NOS relays by giaddr.** SONiC-VS runs ISC `dhcrelay`: circuit id `hostname:interface`, remote id, **no link selection** ([R-10](../reviews/2026-10-18-spec-review.md)). NICo accepts giaddr-based segment selection. Where a stack or a test needs link selection, the fabric's `relay.linkSelection: twin` puts the twin relay behind the NOS, or `kea` runs a Kea relay container. **A model switch's twin relay** inserts the circuit id and remote id the model's cabling implies, and link selection on request |
| **Network boot** | PXE, iPXE and UEFI HTTP boot | **Real at `vm`:** OVMF does it. **At `model`: the boot walk** below |
| **Virtual media** | Insert, eject, boot from it | Libvirt CD-ROM at `vm`; recorded and boot-walked at `model` |

### The boot walk

A `model` host has no firmware, so the twin walks the boot protocol on its behalf, far enough for
the stack's boot service to see what a real host would ask, and stops before anything executes.
The walker is a state machine on the host's provisioning-role endpoint:

1. **DHCP** with the PXE client options a UEFI x64 or aarch64 host sends: option 60
   `PXEClient:Arch:00007:UNDI:003016` (or `00011` for aarch64; `HTTPClient:Arch:00016` for UEFI
   HTTP boot), option 93 (architecture), 94 (NDI), 97 (client machine id = the system UUID).
2. **Boot server answer**: options 66/67 in the ACK, or a ProxyDHCP answer on port 4011, or an
   HTTP URI for HTTP boot.
3. **TFTP** `RRQ` of the network boot program (iPXE or `snponly.efi`), fully transferred.
4. **iPXE's second DHCP** with user class `iPXE` (option 77) and option 175, as iPXE does.
5. **Fetch the boot script** over HTTP and **interpret the iPXE subset** the stacks emit: `set`,
   `isset`, `iseq`, `goto`, `chain`, `kernel`, `initrd`, `imgfetch`, `boot`, and the variables
   `${mac}`, `${net0/mac}`, `${serial}`, `${uuid}`, `${asset}`, `${manufacturer}`, `${product}`.
6. **GET every artifact the script names**, fully by default (a `HEAD` mode exists for scale runs
   and is labelled), so the boot service's logs and counters see a real client.
7. **Stop** at the first in-OS action (`boot`), mark the device booted per the profile's `boot`
   behaviour, and record `boot-walked to: <step>`.

The fidelity report prints the step reached for every walked host. A walker that meets a protocol
it does not implement (a vendor PXE extension, HTTPS boot with client certificates) stops there and
says so, instead of pretending.

## Inside a booted host (`vm` fidelity)

A `vm` host runs in one of two modes ([D23](11-decisions.md#d23--two-in-host-modes-for-vm-hosts)):

| | `os: twin` | `os: stack` |
|---|---|---|
| Who installs the OS | the twin, from its own image | the stack (NICo's PXE install, its discovery image) |
| In-host agent and shim | present | **absent** |
| What the guest sees of accelerators | the vendor's real library over the shim | PCI stub devices with the profile's vendor and device ids; no driver binds |
| Good for | NVSentinel, GPU Operator, kernel-log faults, tenant clusters | NICo's own ingestion, attestation, sanitisation |
| Boot by the stack | allowed; a reinstall ends the mode | expected |

What both modes provide, because the hardware would: **SMBIOS** type 1/2/3 with the identity's
serial, UUID, manufacturer and product; **PCI** devices and ids from the components; NIC MACs from
the identity; a **swtpm** with persisted state and an EK certificate from the estate CA, exposed
as `/dev/tpm0` and `/dev/tpmrm0`; Redfish `Bios`, `SecureBoot` and `BootOrder` **applied to OVMF
NVRAM on the next reset** and read back by the NVRAM observer.

| Interface | Projects | Implementation (`os: twin`) |
|---|---|---|
| **Accelerator management library** | Device inventory, health, clocks, power and limits, ECC and error counters, topology, driver-visible faults | A shim under the vendor's real library, so the real CLI, DCGM-class daemons, device plugins, DRA drivers and feature discovery all see accelerators. NVIDIA: **Mokka** (`NVIDIA/k8s-test-infra`), which replaces `libnvidia-ml.so`. AMD and Intel shims: [Q7](11-decisions.md#q7) |
| **sysfs for Go consumers** | `/sys/class/infiniband`, `/sys/bus/pci` as the model says | Mokka's sysfs surfaces are `LD_PRELOAD` interception, which **Go binaries bypass**; NVSentinel's NIC monitor is Go. The in-host agent **bind-mounts a model-driven tree** at the real paths ([Q16](11-decisions.md#q16)) |
| **Kernel log** | Driver messages a fault would produce (`NVRM: Xid …`, amdgpu RAS lines, habanalabs errors) | The in-host agent writes to `/dev/kmsg`, with message templates from the pack's fault catalogue |
| **TPM** | Measured boot, endorsement key, quote | swtpm; PCRs are the real measurements OVMF makes |
| **In-host agent** | Applies the above | Talks to the engine over **virtio-vsock only**. Never on a twin network ([D10](11-decisions.md)) |

## Kubernetes view (`model` fidelity)

For stacks that need only a Kubernetes view of hosts (schedulers, operators' placement logic,
recipe validators), `model` hosts can appear as **KWOK** nodes advertising the profile's
`kubernetes` resources, labels and DRA ResourceSlices, with conditions following the model's
health. They run no pods. **This is impersonation** ([D24](11-decisions.md#d24--kwok-and-vendor-agent-mocks-are-impersonation)):
the twin registers Node objects in the stack's own cluster with a credential the stack's
`outputs` hand it, scoped to node registration. A run that enables it is labelled `impersonated`.

## Network

| Interface | Implementation |
|---|---|
| **Switch operating systems** | **Real NOS images**, wired by containerlab from the model's cabling ([D4](11-decisions.md)): **SONiC-VS** by default (freely available); Cumulus VX where the operator has an image; other containerlab kinds as packs add them. BGP, EVPN, LLDP, the relay by giaddr and the NOS's management API (gNMI, REST, NVUE, CLI) are all real. A real NOS is `vm` fidelity, forces the wall clock, and is **observed** (port state, LLDP, BGP sessions, routes) by the NOS observer ([D19](11-decisions.md#d19--model-fields-are-owned-or-observed-observers-reconcile-the-substrate)) |
| **Model switches** (`nos: none`) | For scale and twin-only estates ([D21](11-decisions.md#d21--model-switches-are-a-fidelity-level-fabric-device-fidelity-derives-from-nos)): one bridge per modelled L2 segment, each modelled port a TAP the userspace stack owns, and a **twin relay** per segment inserting the circuit id and remote id the cabling implies. Cabling, port state, LLDP and counters are projected from the model. **No routing protocol runs**, so `switch/bgp-*`, `switch/evpn` and NOS management APIs are not provided, and `dctwin check` says so |
| **LLDP from endpoints** | `model` endpoints send LLDP on their own port, with chassis and port ids from the model. `vm` hosts run whatever LLDP agent their OS or stack runs |
| **NVLink switch trays** | NVOS emulated at the surfaces the stacks use: the **NVUE REST API** and, for the Rack Management Service, **SSH/SFTP** (firmware, image and certificate jobs), because no runnable NVOS image is public ([Q11](11-decisions.md#q11), [R-12](../reviews/2026-10-18-spec-review.md)) |
| **Data plane** | Linux bridges and VXLAN. Packets flow, which is enough for functional tests. Nothing is measured |

## Fabric managers

| Interface | Projects | Implementation |
|---|---|---|
| **InfiniBand: UFM REST** | Version, SM configuration, ports and GUIDs, partition keys and membership, QoS, health | NVIDIA pack, bound to the model's InfiniBand ports and links. Both surfaces NICo uses: `/ufmRest` with basic authentication and `/ufmRestV3` with a token; some calls need UFM ≥ 6.19 semantics. NICo's `ufm-mock` serves as a conformance oracle |
| **InfiniBand: subnet manager** | SM-level tools (`ibnetdiscover`, `opensm`) | Optional: OpenSM over `ibsim` for stacks that drive the SM directly. Separate fidelity: no UFM semantics |
| **NVLink: NMX-C** | Domain membership, partitions, health, **telemetry subscription** | NVIDIA pack, gRPC on the switch tray's management address, port 9370 with TLS, bound to the model's scale-up domain. NICo's `nmxc-mock` is the reference; its `Subscribe` is UNIMPLEMENTED, so the twin's is written to [Q8](11-decisions.md#q8) |
| **Rack management (RMS)** | Rack power sequencing, firmware and image jobs, certificates, inventory | NVIDIA pack: the Rack Management Service API, bound to the rack format's behaviours, **plus the NVUE REST and SSH/SFTP surfaces on switch trays that RMS drives** (`switch/nvos-ssh`) |
| **UALink** | Pod membership and health | AMD pack, later |

## Facility

| Interface | Projects | Implementation |
|---|---|---|
| **BMS points** | CDU supply and return temperature, flow, pressure, valve and pump state; power meters; rack power; **rack leak points**; ambient | A publisher on the stack's broker over MQTT 3.1.1, using a **topic dialect** and each device's `bms` mapping. First: `nvidia/dsx-exchange`, the `BMS/v1/PUB/{Value,Metadata}/{objectType}/{pointType}/{tagPath}` topics from DSX Exchange's AsyncAPI schemas, **publishing the point types the stack's consumer actually reads** (`Rack/RackLeakDetect`, `RackLeakSensorFault`, `RackLeakDetectTray`, CDU and PowerMeter points) rather than only what the schema permits ([stack doc](../stacks/nvidia-dsx-os.md#the-two-leak-paths)). BACnet/IP and Modbus TCP servers later, for stacks that integrate at field level |
| **Isolation requests** | The stack's request that the BMS isolate a rack electrically or hydraulically, and the status points answering it | The twin's BMS answers the request topics the stack's consumer defines, after the delay the estate's facility profile gives, and drops the rack's power and flow accordingly |
| **Grid signals** | Demand response, load targets, power-breach notices | A topic dialect; first `nvidia/dsx-flex` (`grid/v1/…`). OpenADR 3 later |
| **PDUs** | Outlet and phase power, outlet switching | SNMP with MIB data per dialect, and Redfish power distribution, per profile |
| **Power shelves** | Shelf and PSU state, output power, faults | Redfish, per profile dialect (Liteon, Delta) |

## External services the stack calls

Some stacks call services outside the datacenter: attestation services, telemetry backends,
licence servers. The twin can provide a **stub** at the address the stack is configured with,
answering the documented API with test values. A stub is always labelled in the fidelity report.
A scenario can withhold it to test the stack's offline behaviour. The twin never forwards to the
real service. The **artifact mirror** the stack installs from is not a stub: it is a real registry
and cache, populated before the run ([15](15-security.md#the-install-path)).

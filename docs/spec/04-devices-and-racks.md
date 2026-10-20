# 04 — Devices and rack formats

Hardware is data. A **device profile** describes one hardware model. A **rack format** fills a
rack from profiles. A **fabric template** builds networks across racks. All three live in packs
([06](06-packs-and-stacks.md)), so adding a vendor never changes the engine. The normative field
lists are in [13](13-schemas.md); this document explains them.

## Device profiles

```yaml
apiVersion: dc-twin/v1alpha1
kind: DeviceProfile
metadata: { name: nvidia/gb300-compute-tray, version: 0.1.0 }
spec:
  class: compute-tray
  form: { mounting: tray, units: 1 }
  identity:
    serialFormat: "1{digits:12}"             # illustrative; the vendor's real pattern goes here
    macOUI: "aa:bb:cc"                       # illustrative; the vendor's OUI
  components:
    - { name: cpu, kind: cpu, count: 2, model: "NVIDIA Grace, 72 cores" }
    - { name: gpu, kind: accelerator, vendor: nvidia, count: 4, model: "NVIDIA GB300", maxW: 1400,
        ports: [{ name: nvl, kind: scale-up, technology: nvlink, lanes: 18 }] }   # component-owned ports
    - { name: cx8, kind: nic, count: 4, model: "ConnectX-8 800G" }
    - { name: bf3, kind: dpu, count: 1, profile: nvidia/bluefield-3 }            # a SUB-DEVICE: own BMC, own ports
    - { name: nvme, kind: drive, count: 4, model: "NVMe 3.84 TB" }
    - { name: tpm, kind: tpm, count: 1, model: "TPM 2.0" }
    - { name: bmc, kind: bmc, count: 1, model: "NVIDIA BMC + HMC" }
  bmc:
    dialect: nvidia/gb-bmc                   # identity strings NICo keys on live in the dialect (stack doc)
    mockup: mockups/gb300-compute-tray/      # captured and scrubbed; licence in pack.yaml assets
    ipmi: true
    console: sol
  ports:
    - { name: bmc0,     kind: ethernet,   speed: 1G,   role: management }
    - { name: cx8-{0..3}, kind: infiniband, speed: 800G, role: scale-out }
    - { name: pwr-in,   kind: power-in,   source: busbar }
    # frontend ports belong to the BlueField sub-device
  power:   { standbyW: 60, idleW: 1200, maxW: 6000, model: per-component }   # illustrative
  cooling: { kind: liquid, loop: rack-manifold }
  sensors:
    - { name: coolant-in, quantity: temperature, unit: C, warn: 45, crit: 55, source: coolant-in }
    - { name: gpu-max,    quantity: temperature, unit: C, warn: 85, crit: 95, source: accelerator-max }
    - { name: leak,       quantity: leak, source: tray-leak-detector }   # projected as Redfish LeakDetectors
  firmware:
    - { component: bmc, baseline: "25.09.12", applyOn: reset }
    - { component: uefi, baseline: "1.07.03", applyOn: reset }
    - { component: cpld, baseline: "0.3.1", applyOn: ac-cycle }
  behaviours:
    power: { onSeconds: 40, offSeconds: 10, requires: "rack.power.sourcesOn >= rack.power.minSources" }
    boot:  { postSeconds: 120 }
  faults: [generic/power, generic/bmc, generic/thermal, generic/leak, generic/firmware, generic/tpm, nvidia/xid, nvidia/dpu]
  fidelity:
    model: {}
    vm: { arch: aarch64, firmware: uefi, tpm: true, inHost: [nvidia/mokka], cost: { ramGB: 12, vcpu: 4 } }
  kubernetes: { resources: { "nvidia.com/gpu": 4 }, labels: { "nvidia.com/gpu.clique": "{domain}" } }   # GFD product, count, family: from the gpu component (D43)
  provides:
    - { capability: "redfish/dialect:nvidia/gb-bmc", at: [model, vm] }
    - { capability: ipmi/lan, at: [model, vm] }
    - { capability: console/sol, at: [model, vm] }
    - { capability: tpm2/measured-boot, at: [vm] }
    - { capability: accelerator/nvml, at: [vm], via: nvidia/mokka }
  isNot:
    all: ["No NVLink data path: NVLink lanes carry state only.", "The HMC is projected through the BMC Redfish tree; it has no separate network presence."]
    vm:  ["Grace is QEMU aarch64; on an x86 host this profile is model-only (09).", "In os: stack mode the GPUs are PCI stubs with no driver behind them (D23)."]
  sources:
    - { what: "tray composition", url: "https://docs.nvidia.com/enterprise-reference-architectures/nvl72-ai-factory/latest/components.html", date: 2026-10-18 }
```

Every number above is illustrative. A pack's real profile cites the source and date for each fact
and is checked against hardware when hardware is available ([07](07-fidelity.md)).

### What a profile says

The fields are defined in [13 — Device profile](13-schemas.md#device-profile). The ones that carry
decisions:

- **Ports bind to roles, never to estate network names.** `role: management` means "the
  management network of whatever estate I am compiled into" ([R-37](../reviews/2026-10-18-spec-review.md)).
- **Components may own ports.** A GPU's NVLink lanes, an OAM's Infinity Fabric links and a Gaudi's
  RoCE ports are `scale-up` or `ethernet` ports owned by the component, so a fault on the component
  can take exactly its ports down and a format's fixed links can wire them.
- **A component with a `profile` is a sub-device** ([Sub-devices](#sub-devices)).
- **`faults` is a strict allow-list.** A fault outside it is refused at injection.
- **`provides` declares at which fidelity levels** each capability holds; `dctwin check` uses that
  to answer `met` or `reduced` ([06](06-packs-and-stacks.md#the-capability-contract)).
- **`isNot` is required** and may differ by level ([07](07-fidelity.md)).
- **`sources` is required.** Every count and version names where it came from and when.

### Sub-devices

Some components are devices in their own right: a BlueField DPU has its own BMC, its own
out-of-band port, its own OS and its own place in a lifecycle manager's model (NICo treats a
host and its DPUs as a paired set, each with a Redfish endpoint). A component with a `profile` is
compiled as a **sub-device**:

- its path is the component's path (`…/compute-tray-07/bf3-0`), and it appears in the parent's
  component list and as a device;
- it has its own `bmc`, `ports` (its `management` port is cabled by the fabric like any BMC),
  firmware, faults and `provides`;
- its **fidelity** is the parent's unless overridden (`select: "…/bf3-0", level: vm` makes it a
  DPU stand-in VM, [10](10-roadmap.md) M6);
- its **power** follows the parent's (a tray off is a DPU off), unless the profile says it has
  standby power of its own;
- its **identity** derives from its own path, and the parent-child pairing is what a lifecycle
  manager discovers by serial and LLDP.

### Behaviours

The core provides a small set of **behaviour primitives**, which profiles parameterise and packs
extend with hooks. Preconditions are CEL over `device`, `rack` and `estate` ([14](14-expressions.md)).

| Primitive | Models |
|---|---|
| `power` | The off / powering-on / on / powering-off state machine; reset types (On, ForceOff, GracefulShutdown, ForceRestart, PowerCycle); preconditions on other devices; durations; AC cycle. At `vm` the power state is **observed** from libvirt once the request is issued ([D19](11-decisions.md#d19--model-fields-are-owned-or-observed-observers-reconcile-the-substrate)) |
| `boot` | POST time, boot order and one-time overrides, PXE, HTTP, virtual media and disk. At `model` the boot walk ([05](05-interfaces.md#boot-and-addressing)); at `vm` delegated to OVMF, with boot order and Secure Boot written to NVRAM on the next reset and read back by the observer |
| `firmware` | Inventory; staged updates; apply on reset, on AC cycle or immediately; failure and rollback; per-component versions against the profile's baseline |
| `settings` | BIOS/UEFI attributes applied on the next boot; lockdown |
| `accounts` | BMC users, password change on first login, sessions, certificates; rotations are recorded so a scenario can assert the stack rotated them |
| `sensors` | Derived values that track a target with a first-order lag. Targets derive from power, the coolant loop and ambient temperature. Thresholds raise committed health events |
| `links` | Port up and down, training time, LLDP neighbours derived from cabling |
| `health` | Rolls component health up to the device. Faults set component health |

A **pack hook** adds vendor-specific behaviour that data cannot express. Examples are an OEM
lockdown action, a rack-sequenced power-on, or an NVLink domain that forms only when every switch
tray is healthy. Hooks are named in the profile and implemented once in the pack
([06](06-packs-and-stacks.md#code-in-packs)).

### Capturing a real device

Profiles are built from real hardware where possible:

- `dctwin capture redfish <url>` walks a real BMC's Redfish tree, in the manner of DMTF's
  Redfish-Mockup-Creator. It scrubs serials, MACs, UUIDs, addresses, certificates and anything
  user-identifying, then writes a mockup. The mockup's licence is recorded in `pack.yaml`
  `assets` ([Q10](11-decisions.md#q10)).
- `dctwin capture host` runs inside a real host and records its inventory: PCI tree, DMI, NIC
  and accelerator inventory from the vendor's tools (`nvidia-smi -q -x`, `amd-smi`, `hl-smi`).
  A pack's `CaptureParser` turns the vendor tool's output into components ([06](06-packs-and-stacks.md#code-in-packs)).
- `dctwin diff profile <profile> <capture>` compares a profile with a newer capture. Any
  difference goes into the [fidelity ledger](07-fidelity.md#the-fidelity-ledger).

## Rack formats

A rack format places devices into slots, wires their **fixed links**, declares the rack's
**domains**, its **power inputs and redundancy**, how facility faults **propagate** into it, and its
layout. **Counts are parameters with declared ranges, not code** ([D14](11-decisions.md)), because
the same family ships in several layouts. The field list is [13 — Rack format](13-schemas.md#rack-format).

```yaml
apiVersion: dc-twin/v1alpha1
kind: RackFormat
metadata: { name: nvidia/gb300-nvl72, version: 0.1.0 }
spec:
  archetype: rack-scale
  params:
    computeTrays: { type: int, default: 18, allowed: [18], label: Compute trays }
    switchTrays:  { type: int, default: 9,  allowed: [9],  label: NVLink switch trays }
    shelves:      { type: int, default: 8,  range: [6, 8], label: Power shelves }
    powerShelf:   { type: profile, class: power-shelf, provides: [power/busbar-dc], default: nvidia/power-shelf-33kw }
    mgmtSwitch:   { type: profile, class: switch, default: generic/mgmt-switch-1g }
  slots:
    - { names: "power-shelf-{01..$shelves}", profile: $powerShelf }
    - { names: "mgmt-switch", profile: $mgmtSwitch }
    - { names: "compute-tray-{01..$computeTrays}", profile: nvidia/gb300-compute-tray,
        accepts: { class: compute-tray, mounting: tray, units: 1, cooling: liquid } }
    - { names: "switch-tray-{01..$switchTrays}", profile: nvidia/nvlink-switch-tray-gb300 }
    - { names: "leak-point-{01..02}", profile: generic/leak-point, at: ["compute-tray-10", "compute-tray-18"] }  # rack-level BMS points
  layout:
    height: 48
    uNumbering: eia                                      # U1 at the bottom
    front:                                               # top to bottom
      - { slots: "power-shelf-{01..04}", group: power shelves }
      - { slots: "mgmt-switch" }
      - { slots: "compute-tray-{01..10}" }
      - { slots: "switch-tray-{01..09}", group: NVLink switch trays }
      - { slots: "compute-tray-{11..18}" }
      - { slots: "power-shelf-{05..08}", group: power shelves }
    back: [nvlink-spine, busbar, management, scale-out, frontend]
    reserved: []                                         # no room for template-generated leaves: see fabrics.placeIn
  fixedLinks:
    - { template: nvidia/nvlink-spine }                  # every GPU's nvl lanes to switch-tray ports
  domains:
    - kind: scale-up
      technology: nvlink
      members: "compute-tray-*/gpu-*"
      manager: "switch-tray-01"
      formation: 'rack.devices.filter(d, d.class == "switch-tray").all(d, d.health == "OK" && d.power.state == "On")'
    - { kind: maintenance, members: "*", unit: rack }
  power:
    inputs: [{ name: busbar, kind: busbar, capacityW: 132000, redundancy: N+N, minSources: 6 }]
    capacityW: 132000
  cooling: { kind: liquid, manifold: liquid, loop: external }
  propagation:
    leak:
      - { from: "leak-point-*", to: "compute-tray-{11..18} switch-tray-*", effect: generic/leak/tray-detector }  # illustrative
  management: { rackController: nvidia/rack-management }
  behaviours:
    powerOn: { sequence: ["power-shelf-*", "switch-tray-*", "compute-tray-*"] }
  provides: [{ capability: fabric/rack-management, at: [model, vm] }]
  isNot:
    - "The NVLink spine is fixed links in the model; no cartridge or cable is individually modelled."
    - "Rack-sequenced power-on is a hook with illustrative ordering."
    - "Leak propagation is the format's rule, not a fluid model."
  sources:
    - { what: "trays, switch trays, shelves", url: "https://docs.nvidia.com/enterprise-reference-architectures/nvl72-ai-factory/latest/components.html", date: 2026-10-18 }
```

### The formats

| Archetype | Example formats | Scale-up domain | What the format must model |
|---|---|---|---|
| **Rack-scale scale-up** | `nvidia/gb200-nvl72`, `nvidia/gb300-nvl72` | One rack. 72 GPUs across 18 compute trays and 9 NVLink switch trays, joined by the NVLink spine | Domain formation and health gating; the switch trays' NVOS, NMX controller and SSH surface; power shelves on a DC busbar with N+N redundancy; liquid cooling, in-tray leak detectors and rack leak points; rack-sequenced power and firmware; the rack as the unit of maintenance; DPUs as sub-devices |
| **Multi-rack scale-up** | `nvidia/gb200-nvl36x2` | Two racks, 36 GPUs each, 9 switch trays per rack, linked by cross-rack NVLink cables. NVIDIA's partition guide v1.2 lists 9 trays of 4 GPUs or 18 trays of 2 GPUs per rack, so `computeTrays` selects the tray profile as well as the count | A domain spanning two racks, owned by the first in sorted order; `peer` must be mutual; failure of either rack degrades the domain |
| **Rack-scale scale-up, other vendors** | `amd/helios` (previewed June 2025, launched July 2026) | One rack. 72 Instinct MI455X GPUs in 18 four-GPU trays, scale-up over UALink on Ethernet; Pensando Vulcano NICs for scale-out | UALink domain and its switches; written when the pack is built from hardware documentation ([Q7](11-decisions.md#q7)) |
| **Server-scale scale-up** | `generic/hgx-8gpu` (HGX H100, H200, B200, B300), `generic/oam-8gpu` (Instinct MI300X, MI325X, MI355X), `generic/gaudi3-8` | One server. NVSwitch chips on the HGX baseboard; a fully connected Infinity Fabric on AMD's OAM platform; for Gaudi 3, the accelerators' own integrated RoCE ports (24 per accelerator: 21 in-node, 3 scale-out) | Servers per rack as a parameter (air-cooled racks hold fewer); one backend NIC per accelerator for rail-optimised scale-out; the baseboard management controller behind the host BMC |
| **PCIe accelerators** | `generic/pcie-gpu-server` | None | Accelerators as PCIe components only |
| **CPU, storage, control plane** | `generic/cpu-rack`, `generic/storage-rack` | — | Servers, drives and PDUs. Where a stack's own control plane often lives |
| **Network** | `generic/network-rack` | — | Spine and aggregation switches, OOB aggregation, fabric-manager appliances; `reserved` U for template-generated leaves of racks that have no room |

Sources, read 2026-10-18: NVIDIA's NVL72 reference architecture components page and *GB200 NVL
Partition Guide* v1.2; AMD's Helios launch materials; Intel's Gaudi 3 cluster reference white
paper; Dell's XE9680 specification (6U). Each pack re-verifies its counts when its profiles are
built and records them in `sources`.

### Facility devices

Facility devices are profiles like any other, with `class: pdu`, `power-shelf`, `ups`, `cdu` or
`sensor`. They are placed by rack formats (shelves, PDUs, rack leak points) or by the estate's
`facility.devices` (UPS, under-floor sensors, room sensors):

- power shelves and PDUs speak Redfish power distribution, SNMP or both, per profile;
- CDUs, leak points and building sensors publish **BMS points** through the profile's `bms`
  mapping (`objectType`, `pointType` per sensor) in the dialect the estate names
  ([05](05-interfaces.md#facility));
- in-tray leak **detectors** are sensors of the tray itself, projected over Redfish
  `LeakDetectors`; rack and floor leak **points** are BMS points. The two reach a stack by
  different paths and it treats them differently ([stack doc](../stacks/nvidia-dsx-os.md#the-two-leak-paths));
- a facility fault reaches devices only through a format's `propagation` rule.

### Layout

Rack formats and device profiles carry **layout data** so that the console draws any format and
any device with no vendor code ([D18](11-decisions.md)):

| Where | Field | Says |
|---|---|---|
| Estate | `rows[].positions`, `racks[].position`, `halls[].layout` | Floor positions, including empty ones, and row orientation |
| Rack format | `layout.height`, `uNumbering`, `front`, `back`, `reserved` | Slot order and U positions; which slot groups collapse into one row; what the back view shows; room kept for generated devices |
| Device profile | `form.units`, `form.mounting` | Height in the rack |
| Device profile | `layout.faceplate` | Where components and ports sit on the front and back |
| Fabric template | `layout.tiers` | How a generated fabric is laid out as a graph |
| Fault catalogue | each fault's `target`, `params` and `effects` | The fault form: what it applies to, what it will do, what it asks for |

A profile or format without layout data still renders, as a plain list of slots and ports.

## Open registries

The core's enumerations were closed and shaped like today's NVIDIA hardware
([R-42](../reviews/2026-10-18-spec-review.md)). They are now **registries**: the core defines base
values with behaviour attached, and a pack may register `pack/value` entries that name the base
value they inherit from.

| Registry | Core values | Pack extension names |
|---|---|---|
| device class | `server`, `compute-tray`, `switch`, `switch-tray`, `dpu`, `power-shelf`, `pdu`, `ups`, `cdu`, `sensor`, `rack-controller`, `appliance` | `nvidia/nvswitch-tray` inherits `switch-tray` |
| component kind | `cpu`, `accelerator`, `nic`, `dpu`, `drive`, `psu`, `fan`, `tpm`, `bmc`, `memory`, `asic`, `pump`, `valve`, `breaker` | |
| port kind | `ethernet`, `infiniband`, `scale-up` (with `technology`), `pcie`, `power-in`, `power-out`, `coolant-in`, `coolant-out`, `serial` | technologies `nvlink`, `ualink`, `xgmi`, … |
| domain kind | `scale-up`, `ib-subnet`, `ethernet-fabric`, `power`, `cooling`, `failure`, `maintenance` | |
| NOS | `none`, `sonic-vs`, `cumulus-vx` | `pack/nos` |
| fabric template | `clos-2tier`, `clos-3tier`, `rail-optimised`, `oob-flat` | `nvidia/nvlink-spine`, `amd/ualink-rack` |
| triggers, canonical requests, effect verbs | [14](14-expressions.md), [08](08-scenarios-and-faults.md#faults) | |
| capabilities | [06](06-packs-and-stacks.md#the-capability-contract) | |

A pack that needs a new base value (a behaviour the core lacks) proposes it to the core; the pack
never patches the engine.

## Fabric templates

A fabric template generates switches, cabling and NOS configuration from the estate. It is a
function from (racks selected, their devices' ports by role, parameters) to (devices with
placement, links, per-NOS configuration, provided capabilities).

| Template | Builds |
|---|---|
| `clos-2tier`, `clos-3tier` | Leaf and spine (and super-spine). Underlay BGP, optionally unnumbered (RFC 5549); optional EVPN overlay; DHCP relay on server-facing ports, by giaddr from a real NOS, with link selection from the twin or a Kea relay when `relay.linkSelection` asks ([05](05-interfaces.md#boot-and-addressing)) |
| `rail-optimised` | The scale-out fabric for 8-accelerator servers: NIC *i* of every server goes to rail leaf *i*, and every rail leaf to every spine (`spines`, default 2; `0` for single-tier rails), so one subnet manager reaches the whole fabric. InfiniBand (with a subnet manager or UFM) or Ethernet RoCE |
| `oob-flat` | Per-rack management switches into an OOB aggregation layer; BMC, sub-device BMC and switch management ports; the relay for the management network |
| `nvidia/nvlink-spine`, `nvidia/nvlink-cross-rack` | NVLink fixed links inside an NVL72, and cross-rack links for NVL36x2. Supplied by the nvidia pack |
| `amd/ualink-rack` | Helios scale-up links. Supplied by the amd pack once it exists |

Rules every template obeys:

- **Placement.** Generated devices are placed in a rack: the rack's own `reserved` U when the
  format has it, otherwise the fabric's `placeIn` racks (network racks by default). They draw power
  from that rack's inputs and appear in its elevation.
- **Deterministic port allocation.** Which leaf and which port an endpoint gets is a pure function
  of the endpoint's path, so adding or removing a rack never moves another rack's links
  ([03](03-estate-model.md#changing-a-running-estate)).
- **`provides`.** A template declares the capabilities its switches provide at the fabric's `nos`
  level: a real NOS provides `switch/bgp-unnumbered`, `switch/evpn`, `switch/lldp`,
  `dhcp/relay-giaddr`; a model switch provides `switch/lldp` and `dhcp/relay-option-82` (twin relay)
  and nothing routed ([D21](11-decisions.md#d21--model-switches-are-a-fidelity-level-fabric-device-fidelity-derives-from-nos)).
- A hand-written `cabling` list in the estate can amend or replace a template's output and is
  validated the same way ([03](03-estate-model.md#validation)).

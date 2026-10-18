# 01 — Overview

## The problem

The software that runs an AI datacenter talks to hardware almost entirely through management
protocols:

| Software | What it talks to |
|---|---|
| Bare-metal lifecycle manager | BMCs (Redfish, IPMI), relayed DHCP, PXE/HTTP boot, and a TPM for attestation |
| Network automation | Switch operating systems (BGP, EVPN, LLDP, NVUE/gNMI) |
| Fabric managers | InfiniBand subnet managers, NVLink and UALink switch controllers |
| Fleet-health agents | GPU management libraries (NVML, DCGM, AMD SMI), kernel logs and NIC counters |
| Facility integration | Building-management points, PDUs, CDUs and grid signals |

Each of these surfaces is testable in isolation. Vendors ship mocks for their own components. But
no public tool assembles them into **one datacenter whose devices agree with each other**: where
the GPU that logs an Xid is the GPU in tray 7 of rack A03, whose BMC reports it critical, whose
power draw drops on the power shelf feeding that rack, and whose NVLink ports go dark on the switch
trays the fabric manager is watching.

Without that, a stack can only be tested component by component against mocks that disagree.
Otherwise it waits for hardware, which arrives late, costs a lot, and cannot be broken on purpose.

## Goals

- **G1 — Unmodified software under test.** A stack is installed on the twin exactly as it would
  be on a site. Nothing in the stack is patched, configured into a test mode, or replaced by an
  impersonator, except in an explicitly labelled scale mode ([D3](11-decisions.md)).
- **G2 — One consistent datacenter.** Every interface is a projection of one model. A change
  made through any interface, or injected as a fault, is reflected on every other interface that
  would observe it ([D2](11-decisions.md)).
- **G3 — Vendor-neutral core.** Engine, model, clock, scenario runner and protocol engines know
  no vendor. NVIDIA, AMD, Intel, Dell, HPE, Lenovo, Supermicro and others arrive as **packs**
  ([06](06-packs-and-stacks.md)).
- **G4 — Any rack format.** Rack-scale systems (NVL72, NVL36x2, Helios), 8-GPU servers (HGX,
  OAM, Gaudi), PCIe servers, CPU, storage and network racks are all data, not code.
- **G5 — Fidelity per device.** One estate can mix devices that really boot (`vm`), devices that
  only speak their protocols (`model`) and real devices bridged in (`hardware`)
  ([07](07-fidelity.md)).
- **G6 — Failure as a first-class input.** Faults are injected by scenario or by hand, are
  reproducible, and are asserted on ([08](08-scenarios-and-faults.md)).
- **G7 — Honest results.** Every run prints what the twin did not model for the devices and
  interfaces it used, what state the substrate owned, what was impersonated or stubbed, and any
  twin incident. A result that depends on something unmodelled says so ([D11](11-decisions.md)).
- **G8 — Runs on one machine.** A useful estate fits on one Linux server with KVM, local or cloud
  bare metal. Larger estates scale out across hosts later ([Q6](11-decisions.md)).
- **G9 — A console to see, change and break it.** A web UI shows the estate from hall to
  component; adds racks and machines; injects faults and technician acts; and follows what the
  software under test does in response. It holds no capability the API lacks ([12](12-ui.md)).

## Non-goals

- **Performance.** No accelerator executes kernels, no fabric carries realistic bandwidth, and no
  number from the twin predicts throughput or latency.
- **Physics.** Power and thermal behaviour is first-order and exists to drive alarms and
  policies. It is not computational fluid dynamics.
- **Hardware-rooted security.** DPU-enforced isolation, confidential-computing attestation and
  real firmware signing can be exercised for control-flow, never proven.
- **Vendor qualification.** Passing on the twin is not a certification of anything.
- **Being a DCIM.** The estate file describes a datacenter so it can be emulated, not so it can be
  managed. Importing from a DCIM may come later.

## Principles

1. **One model, many projections.** Adapters hold no device state of their own.
2. **Emulate the device, never the software under test.** The twin pretends to be a BMC, a
   switch or a GPU, never NICo's agent or the GPU Operator. Impersonating an agent is a
   separately labelled scale mode.
3. **Real software where it exists.** Switches run a real network operating system. GPUs inside
   booted hosts are presented through the real management library over a shimmed device layer.
   Only protocols with no runnable implementation are written from scratch.
4. **Data first, code second.** A new device is a profile. A new vendor dialect is a mockup plus
   behaviour hooks. Code is written only for behaviour data cannot express.
5. **Faults go into the model, not into one interface.**
6. **Every emulator states what it is not, and reality corrects it.** When a real device disagrees
   with its profile, the profile changes and the divergence is recorded in the
   [fidelity ledger](07-fidelity.md#the-fidelity-ledger).
7. **The software under test cannot see the twin.** The twin's control plane is unreachable from
   every network the stack is attached to, so a stack cannot pass by reading the answer.
8. **Deterministic where it can be.** A model-only run under a virtual clock with a fixed seed
   and **no stack installed** replays exactly; with a stack, runs are repeatable, not bit-exact
   ([D22](11-decisions.md#d22--an-installed-stack-forces-the-wall-clock-exact-replay-is-a-twin-only-run)).

## Vocabulary

| Term | Meaning |
|---|---|
| **Estate** | The emulated datacenter: sites, halls, rows, racks, devices, cables, networks, power and cooling. Declared in an **estate file** ([03](03-estate-model.md)) |
| **Device** | Anything with an identity and an interface: server, compute tray, switch, switch tray, DPU, power shelf, PDU, CDU, sensor, rack controller |
| **Component** | A part inside a device: CPU, accelerator, NIC, DPU, drive, PSU, fan, TPM, BMC |
| **Device profile** | The data describing one hardware model: components, ports, interfaces, dialects, firmware, power, faults and fidelity ([04](04-devices-and-racks.md)) |
| **Rack format** | A template that fills a rack with devices, internal links, domains, power and cooling. NVL72 is one |
| **Fabric template** | A generator that builds a network from the estate, for example a two-tier Clos or a rail-optimised backend |
| **Domain** | A group the hardware treats as one: an NVLink domain, a UALink pod, an InfiniBand subnet, a power feed, a cooling loop or a failure domain |
| **Adapter** | A process that projects the model onto one protocol, such as the Redfish engine, DHCP, IPMI, UFM REST or MQTT ([05](05-interfaces.md)) |
| **Dialect** | One vendor's form of a protocol: its OEM extensions, quirks and paths |
| **Pack** | A versioned bundle of profiles, rack formats, dialects, in-host shims, fault catalogues and adapters for one vendor or family ([06](06-packs-and-stacks.md)) |
| **Stack** | A set of software under test, what it requires of the estate, how it is installed, and its conformance scenarios. The first is NVIDIA DSX OS |
| **Capability** | A named thing a device or adapter provides and a stack requires, for example `redfish/dialect:nvidia-gb`, `dhcp/relay-option-82` or `fabric/ufm-rest` |
| **Fidelity** | How real a device is: `model` (protocols only), `vm` (it boots; in `os: twin` or `os: stack` mode) or `hardware` (a real device bridged in) |
| **Owned / observed field** | A model field the engine is authoritative for and adapters project, or one the substrate is authoritative for and an observer reports ([D19](11-decisions.md#d19--model-fields-are-owned-or-observed-observers-reconcile-the-substrate)) |
| **Observer** | A process that reads substrate-owned state (libvirt, OVMF NVRAM, a NOS) into the model |
| **Model switch** | A switch with `nos: none`: cabling, port state, LLDP and the twin relay, no routing protocol ([D21](11-decisions.md#d21--model-switches-are-a-fidelity-level-fabric-device-fidelity-derives-from-nos)) |
| **Sub-device** | A component that is also a device, with its own BMC and ports: a DPU in a tray |
| **Impersonator** | Something that stands in for the stack's own agent or kubelet; enabling one makes a run `impersonated`, never a conformance pass ([D24](11-decisions.md#d24--kwok-and-vendor-agent-mocks-are-impersonation)) |
| **Canonical request** | The protocol-neutral name of a request the stack made (`power.off.graceful`), from the registry in [14](14-expressions.md#canonical-request-names) |
| **Twin incident** | An adapter crash, a lagging projection or a lost observer; it voids a PASS ([16](16-testing.md#twin-incidents)) |
| **Scenario** | A scripted run: estate, stack, faults over time and assertions |
| **Software under test (SUT)** | Whatever is installed on the twin to be tested |

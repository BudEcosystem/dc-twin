# 07 — Fidelity

A twin that hides what it does not model produces false confidence, which is worse than no test
at all. Fidelity is therefore declared per device, reported per run, and corrected against
hardware.

## Levels

| Level | The device… | Good for | Cannot show |
|---|---|---|---|
| **`model`** | Speaks its protocols (Redfish, IPMI, DHCP, LLDP, fabric APIs, BMS points) from the model, on its own modelled switch port. No operating system runs; boot is a *boot walk* ([05](05-interfaces.md#boot-and-addressing)) | Scale (thousands of devices on one machine); ingestion, inventory, firmware-baseline and health-policy logic; fabric-manager logic; facility integration; deterministic replay under the virtual clock **with no stack** | Anything that executes on the host: OS install, in-host agents, attestation, sanitisation, drivers. A stack whose agent must run on the host stops at that step unless an impersonator is enabled |
| **`vm`** | Is a QEMU/KVM machine behind its emulated BMC: real UEFI, real network boot, real disk, real TPM measurements, SMBIOS and PCI from the profile; in one of two modes ([D23](11-decisions.md#d23--two-in-host-modes-for-vm-hosts)): **`os: twin`** with the vendor's real accelerator library over a shim and the in-host agent, or **`os: stack`** with the OS the stack installs and PCI stubs for accelerators | `os: stack`: the stack's own lifecycle of a host (install, attestation, sanitisation). `os: twin`: in-host fault detection and remediation, Kubernetes on the hosts | Silicon: accelerator execution, DPU datapath offload, firmware internals, real timing. An `os: stack` host shows no in-host behaviour at all |
| **`hardware`** | Is a real device, bridged into the twin's networks | Keeping profiles honest; testing a stack against a mix of real and emulated devices | Fault injection beyond what the device itself allows (power via a PDU the twin controls; link via a switch port the twin controls) |

**Switches derive their level from their fabric's `nos`** ([D21](11-decisions.md#d21--model-switches-are-a-fidelity-level-fabric-device-fidelity-derives-from-nos)):
`nos: none` is a model switch (cabling, port state, LLDP, the twin relay, no routing protocol), a
real NOS in containerlab is `vm`, and a real switch on the hardware bridge is `hardware`.

**Mixing is the point.** A typical estate puts one rack at `vm` and the rest at `model`, plus,
when available, one real server at `hardware`. Every device answers through the same protocols on
its own port, so the software under test cannot tell which is which by anything a lifecycle
manager keys on ([D3](11-decisions.md), [15](15-security.md#threat-model)).

### Impersonation

Some stacks run an agent on every host, or a kubelet on every node. At `model` fidelity no host
runs, so the agent cannot run, and ingestion stops at that step. For **scale tests only**, an
estate may enable an **impersonator** the stack definition lists: a vendor's own (NICo's
`machine-a-tron` for its scout and DPU agent, DPF's mock DPU agent) or the twin's KWOK projection
(it registers Node objects in the stack's cluster). Such a run is labelled **impersonated**: it
tests the stack's central services at scale, not its agents, its result is `impersonated` rather
than `passed`, and it never counts toward a conformance suite
([D24](11-decisions.md#d24--kwok-and-vendor-agent-mocks-are-impersonation)).

## `isNot`, and the fidelity report

Every profile, dialect, adapter, in-host shim, fabric template, **rack format, fault catalogue**
and stack carries an **`isNot`** list; it may differ by level (`isNot: {all, model, vm, hardware}`).
It is required; a pack without one fails validation.

At the end of every run, `dctwin` prints a **fidelity report** for what the run **used**, which is
defined as: every device that received a request from the software under test or an act, every
device an assertion observed, their racks and the fabric devices on their paths, every adapter and
stub that served them, and every stack component installed.

```
fidelity report — scenario xid-cordon @ hall-a-demo
  devices used: 2 vm (os: twin), 18 model, 0 hardware · 1 rack · 4 fabric devices (sonic-vs)
  tcg guests: none
  impersonated: none
  stubs: external/stub:nvidia-nras (attestation answered with test values)
  unsynchronised adapters: none
  substrate-owned state in use: libvirt power and NVRAM for 2 hosts; swtpm for 2 hosts;
                                SONiC-VS port, LLDP and BGP state for 4 switches
  addressing: stack DHCP, relayed by SONiC-VS (giaddr) and the twin relay (link selection)
  boot: 2 hosts booted by OVMF; 18 hosts boot-walked to "boot"
  twin incidents: none
  not modelled, and used by this run:
    nvidia/gb300-compute-tray      No NVLink data path: NVLink lanes carry state only.
    nvidia/mokka                   No CUDA execution; diagnostics that run kernels return canned results (Q16).
    nvidia/nvlink-switch-tray-gb300  NVOS is emulated at the NVUE and SSH surfaces; no NVOS image runs.
    generic/clos-2tier             Packets flow; nothing is measured.
  unverified profiles (no hardware-run ledger entry):
    nvidia/gb300-compute-tray (source: documentation)
  result: PASS (conditional on the above)
```

**A result is never reported without its report** ([D11](11-decisions.md)). CI publishes it
beside the result. "PASS" is always "PASS, conditional on the above", and a run with a twin
incident is `not-run`, never PASS ([16](16-testing.md#twin-incidents)).

## Hardware

`hardware` mode bridges a dedicated physical NIC into a twin network, so a real device can join
the estate. The estate declares it in `spec.hardware[]` ([13 — Estate](13-schemas.md#estate)):
the selector it stands in for, the bridge interface and network, whether its identity is observed
from the device or pinned, the controls the twin has over it, and a **reference** to its
credentials in the secret store.

- **Out-of-band.** A real BMC joins the management network. The stack under test discovers it
  beside the emulated ones, through the same relay.
- **In-band.** A real server's NICs join the provisioning or frontend networks.
- **Fabric.** A real switch can be cabled to the containerlab fabric through a bridge port.
- **Faults** are limited to what the twin physically controls: a PDU outlet it switches, a switch
  port it disables. The estate declares those controls.
- **Credentials** for real devices are supplied through the control API at run time, by a
  designer, and are never written to the estate file, the snapshot or the event log
  ([15](15-security.md#secrets)).
- The bridge host requirements are in [15](15-security.md#hardware-bridge-host-requirements).

The same machinery is how profiles are kept honest: run a capture against the real device and
diff it with its profile.

## The fidelity ledger

Each pack keeps a ledger: one entry per profile, dialect and adapter.

```yaml
- subject: nvidia/gb300-compute-tray@0.1.0
  source: documentation            # documentation | capture | hardware-run
  checkedAgainst: []               # { device: "<model>", firmware: "<bmc version>", date, by, evidence: <url or path> }
  divergences:
    - found: 2026-11-02
      what: "BMC reports PowerState=On for ~8 s after ForceOff completes"
      action: "added quirk power-state-lag (on by default)"
  isNot: [ … ]
```

- **When reality disagrees, the twin changes, not the test.** Every divergence found against
  hardware is recorded with the change it caused.
- A `hardware-run` entry needs evidence: the capture or the request log it was checked with, who
  ran it and when.
- A profile with `source: documentation` and no `checkedAgainst` entries is printed as
  **unverified** in fidelity reports.
- Upstream mocks used as oracles, such as NICo's `bmc-mock` profiles and `ufm-mock`, are recorded
  as **sources**, not as verification. They show what one consumer expects, not what the
  hardware does.

# 16 — Verifying the twin

A twin that is wrong is worse than no twin. This document says how the twin's own correctness is
established, what it measures about itself, and how a run tells the difference between the stack
failing and the twin failing ([R-26](../reviews/2026-10-18-spec-review.md)).

## Oracles per adapter

Each protocol engine is checked against something that is not the twin.

| Adapter | Oracle | How |
|---|---|---|
| Redfish | DMTF **Redfish Service Validator** and **Redfish Protocol Validator**; a DMTF **Interop Validator** profile generated from the stack's call list ([14](14-expressions.md#canonical-request-names)) | Run against every dialect at every milestone; a dialect that fails its interop profile cannot be marked `provides` |
| Redfish dialects | NICo's `bmc-explorer-cli` and its `bmc-mock` profiles | `bmc-explorer-cli` must identify an emulated BMC as the intended hardware type (not Sushy, not unknown); **differential replay**: the requests NICo made to `bmc-mock` for a profile are replayed against the dialect and the responses compared field by field |
| IPMI | `ipmitool` and OpenIPMI's `lanserv` behaviour | chassis, SEL, FRU, SDR round-trips |
| DHCP client and relay | ISC Kea in a container as the server; NICo's own Kea hook image | leases through a NOS relay and through the twin relay, with option 82 as the model says |
| LLDP | `lldpd`/`lldpcli` on a NOS | neighbours per port match the model's cabling |
| BMS publisher | AsyncAPI validation of every published message; DSX Exchange's `dsx-exchange-consumer` | the consumer must produce the alerts the stack doc says it produces |
| Fabric managers | NICo's `ufm-mock`, `nmxc-mock`, `rms-mock` | the twin's adapters answer at least what the mocks answer, and the stack's clients accept them |
| KWOK projection | `kubectl` against a scratch cluster | nodes, resources and conditions as the profile declares |
| Boot walker | a real UEFI firmware (OVMF) in a VM against the same boot service | the walker's request sequence equals the firmware's, step for step, up to where the walker stops |

## Cross-projection properties

Property tests over randomly generated estates and fault sequences, under the virtual clock with
no stack, assert G2:

- Redfish `PowerState`, IPMI chassis status, the KWOK node condition and the power shelf's output
  agree with the model within one committed event;
- every fault's effects appear on every interface its catalogue entry names, at the fidelity the
  effect `appliesAt`;
- identities are stable across `up`, `apply` and `down`/`up` for the same file and seed;
- port allocation is stable when racks are added or removed;
- a model-only replay of a recorded run produces an identical event log.

## Performance targets

Measured at each milestone and published in `docs/measurements/`; the sizing table in
[09](09-runtime.md#sizing) is replaced by these numbers.

| Target | Value | From |
|---|---|---|
| Redfish p99 latency under the stack's poll | ≤ 250 ms at 1,000 `model` BMCs polled every 30 s | M0 |
| DHCP lease storm | 1,000 BMCs lease within 60 s of `up` through the twin relay | M0 |
| Event delivery to the console | ≤ 1 s from commit at 200 committed events/s | M0 |
| Fault fan-out | every projection updated within 1 committed event | M3 |
| Memory per `model` BMC endpoint; per SONiC-VS instance; per `vm` host; per NICo node | measured, not estimated | M0, M2, M1 |

## Twin incidents

An adapter crash, a lagging projection (more than N events behind), an observer that lost contact
with the substrate, a sidecar restart, or an engine resume are **twin incidents**. They are
committed events (`kind: twin`), the fidelity report prints a `twin incidents:` line, and **any
incident during a scenario voids a PASS**: the result becomes `not-run (twin incident)`. A `not`
assertion can otherwise pass only because the twin was not serving.

## Observability

The engine and every adapter export Prometheus metrics and structured logs on the control
address: events committed per second, projection lag per adapter, requests per protocol and
status, lease counts, VM and NOS state, incident counts. The console shows projection lag and
incidents on every screen's status line.

## CI

- `examples/` validate against the schemas in [13](13-schemas.md); link and anchor checks over
  `docs/`.
- Unit and property tests on every change.
- Oracles above on every change for the Redfish and DHCP adapters, nightly for the rest.
- The isolation scan ([15](15-security.md#network-isolation-d10)) nightly on a bare-metal runner.
- The DSX conformance suite nightly from M7, each result with its fidelity report.

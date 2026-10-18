# dc-twin specification

Draft 0.2, 2026-10-18. Not implemented yet. Draft 0.1 was reviewed the same day; the findings
and what resolved each are in [`docs/reviews/2026-10-18-spec-review.md`](../reviews/2026-10-18-spec-review.md).

## Reading order

| # | Document | What it settles |
|---|---|---|
| 01 | [Overview](01-overview.md) | The problem, goals, non-goals, principles and vocabulary |
| 02 | [Architecture](02-architecture.md) | The engine, the adapters, the substrate, and how a fault reaches every interface |
| 03 | [Estate model](03-estate-model.md) | The estate file (the only input) and the runtime model it compiles to |
| 04 | [Devices and rack formats](04-devices-and-racks.md) | Device profiles, rack formats (NVL72, NVL36x2, HGX, Helios, Gaudi 3, …) and fabric templates |
| 05 | [Interfaces](05-interfaces.md) | Every protocol the twin speaks, and the vendor dialects each one carries |
| 06 | [Packs and stacks](06-packs-and-stacks.md) | How a vendor is added: packs for hardware, stacks for software, and the capability contract between them |
| 07 | [Fidelity](07-fidelity.md) | The model, vm and hardware levels, the "is not" lists, and the fidelity ledger |
| 08 | [Scenarios and faults](08-scenarios-and-faults.md) | Fault catalogues, clocks, scenarios and assertions |
| 09 | [Runtime](09-runtime.md) | Processes, networks, sizing, the CLI and the control API |
| 10 | [Roadmap](10-roadmap.md) | Milestones, with NVIDIA DSX OS first |
| 11 | [Decisions and open questions](11-decisions.md) | D1–D32 and Q1–Q19 |
| 12 | [UI](12-ui.md) | The console: floor, rack, device, topology, facility, faults, events, scenarios, and design mode for editing the estate |
| 13 | [Schemas](13-schemas.md) | The normative field lists: estate, profile, rack format, fault catalogue, scenario, stack, event; identity derivation; names and pins; compatibility |
| 14 | [Expressions](14-expressions.md) | The one expression language (CEL), its environment, the trigger registry and canonical request names |
| 15 | [Security](15-security.md) | Threat model, the isolation mechanism behind D10, the install path, users and roles, secrets |
| 16 | [Verifying the twin](16-testing.md) | Oracles per adapter, cross-projection properties, performance targets, twin incidents, observability, CI |

Stack-specific documents live in [`docs/stacks/`](../stacks/), starting with
[NVIDIA DSX OS](../stacks/nvidia-dsx-os.md). Illustrative estate and scenario files are in
[`examples/`](../../examples/).

## Conventions

- **Decisions are numbered** (`D1`, …) and recorded in [11](11-decisions.md), each with the
  condition that would reverse it. A change to a decision becomes a new numbered decision; the
  old one is never silently edited.
- **Open questions are numbered** (`Q1`, …). A capability that depends on an open question is a
  requirement, not a promise, until the question closes.
- **Review findings are numbered** (`R-nn`) in `docs/reviews/`, and a change that closes one cites
  it.
- **Where a document and [13 — Schemas](13-schemas.md) disagree, 13 wins** and the other is a bug.
- **Vendor facts carry a date and a source.** Hardware counts, protocol details and repository
  locations change. Each was read on the date given and is checked again when a pack is built.
- **MUST / SHOULD / MAY** follow RFC 2119 where they appear in capitals.

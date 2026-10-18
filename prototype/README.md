# Console prototype

A clickable prototype of the dc-twin console ([spec 12](../docs/spec/12-ui.md)). It runs
entirely in the browser over an in-memory estate. There is no backend, no build step and no
external request, so it works offline.

**Open it:** open `index.html` directly, or serve the repository root and visit
`/prototype/`:

```bash
python3 -m http.server 8000
```

## What is real here and what is not

| Part | In the prototype |
|---|---|
| **Estate and compiler** | An estate file (`js/estate.js`) compiled with the packs into racks, devices, components, ports, links and domains, following [03](../docs/spec/03-estate-model.md). Identities derive from the seed and the path |
| **Packs** | Profiles, rack formats with layout data, Redfish dialects and fault catalogues as data (`js/packs.js`). The console draws every rack from layout data ([D18](../docs/spec/11-decisions.md)) |
| **Engine** | One writer of the model: clock, event log with cause chains, power behaviour, first-order sensors, health roll-up, the fault engine, Redfish trees and requests, and the fidelity report (`js/engine.js`). **Durations are shortened** so a demo stays interactive |
| **Software under test** | **Simulated.** `js/stack.js` stands in for NICo, NVSentinel and DSX Exchange so there is something to watch respond. It reaches devices only through the engine's protocol paths (Redfish requests, DHCP, BMS topics), and its policies are illustrative, not NVIDIA's. It is labelled "simulated" everywhere it appears |
| **Scenarios** | A small runner with waits, acts and assertions; every result carries its fidelity report (`js/scenarios.js`) |
| **Numbers** | Counts, firmware versions and power figures are **illustrative**. No profile has been checked against hardware |

## A tour

1. **Floor.** Hall A has four GB300 NVL72 racks, HGX racks, one AMD MI355X rack, one Gaudi 3
   rack, a CPU rack and a network rack. Switch the overlay between health, power, temperature
   and domains. Double-click a rack to open it.
2. **Rack a03.** The elevation is drawn from the NVL72 format's layout: power shelves, compute
   trays, the nine-tray NVLink switch band, and leak sensors marked on the left. Trays 01 and 02
   are at `vm` fidelity. Toggle **back** to see cabling. Click a tray for its panel.
3. **Break a GPU.** Open `a03/compute-tray-01` → **Faults** → inject *GPU has fallen off the bus
   (Xid 79)* on `gpu-2`. Then watch the events drawer for about 30 seconds:
   - health goes Critical;
   - NVSentinel (simulated) cordons and drains the node;
   - it requests a reboot over Redfish (`POST …/ComputerSystem.Reset`);
   - the tray power-cycles, the fault survives the reboot, and the machine is quarantined.

   Indented rows are effects of the row above. **Power → AC cycle** clears the fault.
4. **See what the stack sees.** Open any device → **Interfaces**: the Redfish tree as the stack
   reads it (the dialect's identity strings included) and every request the stack made to it.
5. **Run a scenario.** **Scenarios** → *nvl72-leak-response* → Run. A leak at a03's sensor 02
   reaches the BMS topic, the stack shuts down the trays at and below it, and the result prints
   with its fidelity report.
6. **Add a rack.** Switch to **Design**, click an empty position in row a, pick *GB300 NVL72*,
   set "first host at vm", then **Add to draft**. The draft panel validates it (cabling, spine
   ports, feeds, cooling loop, VM sizing, clock) and shows the estate-file diff. **Apply** racks
   it, cables it, powers it with every BMC on standby, and the simulated stack discovers each BMC
   through DHCP, detects its vendor and reads its inventory. **Export YAML** shows the estate file
   with the draft applied.
7. **Pull a cable.** **Topology** → click a leaf–spine line, or use a device's **Ports & cables**
   tab.
8. **Turn on chaos.** **Faults** → chaos, at a rate per device per hour, drawn from the seed.
9. **Keep what you did.** **Events** → *Save session as scenario* exports your faults and acts,
   with their timing, as a scenario file to add assertions to.

## Structure

```
index.html          shell
console.css         tokens (light and dark), layout, components. Severity decides colour; nominal is grey
js/util.js          seeded PRNG, brace expansion, selectors, formatting
js/packs.js         profiles, rack formats, dialects, fault catalogues
js/estate.js        the estate file, the compiler, fabric generation, add/remove rack, YAML export
js/engine.js        the model's single writer
js/stack.js         the SIMULATED stack
js/scenarios.js     the scenario runner and "save session as scenario"
js/views-*.js       screens: floor and events drawer; rack and device; topology, facility,
                    faults, events, scenarios, stack, design and modals
js/app.js           state, router, rendering, and one delegated handler mapping actions to engine calls
```

In the real console every action in `app.js` is a control API call ([D16](../docs/spec/11-decisions.md)),
and the engine, packs and stack live behind it. This prototype exists to settle the console's
shape before that API exists.

## Known limits

The [spec review of 2026-10-18](../docs/reviews/2026-10-18-spec-review.md) found these, and they
stand until the prototype is reworked against draft 0.2 of the spec:

- **The simulated stack reads the model directly** (`E.model`, `E.dev()`, internal `twin` and
  `state` events) rather than only the protocol projections, so the property the real twin must
  hold, a stack driven purely by what devices expose, is not exercised here. Its request log is
  produced by the stack calling `E.request`, not by an adapter.
- **It prints PASSED on impersonated runs.** The spec's result for such a run is `impersonated`.
- **The fidelity report is partly hard-coded** (the NRAS stub, the addressing line, no
  substrate-owned state, no twin incidents), and impersonation is inferred from the presence of
  `model` hosts rather than from an enabled impersonator.
- **Design-mode apply rewrites `spec.clock` outside the diff**, and the compiler ignores
  `spec.fabrics`.
- **Its leak policy is illustrative** ("four trays of the same class from the sensor's level
  down"). The spec's scenario has two paths that follow NICo's documented behaviour.
- **Switches are at `model` even when the estate says `sonic-vs`**, and the stand-in DPU OS is
  labelled "stub".
- **Data shapes differ from the schemas** (`faultPacks`, tuple firmware, `at:` sensors,
  `minShelves`, `nvlinkLanes`, `network:` on ports instead of `role:`, `generic/leak/tray-level`).
- One hall. Several halls or sites need the hall picker the spec implies but does not yet draw.
- The floor plan is SVG. At thousands of racks the real console moves to canvas or WebGL
  ([Q14](../docs/spec/11-decisions.md)).
- No shared users, no roles and no rebase of drafts. Reloading the page resets the estate.

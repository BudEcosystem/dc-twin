# Objectives gap analysis — 2026-11-10

What the twin does against five objectives, what is missing for each, and how the missing parts
could be built. **Nothing here is decided.** Section 8 lists the decisions to take, section 9
proposes a phased path, and each finding is registered (`G-nn`) so later changes can cite the one
they close, as the [spec review](2026-10-18-spec-review.md) and the
[storage deep dive](2026-11-08-storage-deep-dive.md) do.

Read on 2026-11-10 from this repository at `41d9f90` (the spec, [implementation.md](../implementation.md),
the engine, the packs and the harnesses' recorded results) and the public sources in
[Sources](#sources). No harness was re-run for this review.

The objectives:

1. **Test bed.** Any datacenter software can be tested against a DC setup without a real DC.
2. **Faults and events.** Emulate them, so their impact is understood and acted on before it happens.
3. **Physics.** The DC's actual physics, so software can evaluate it and act on it.
4. **Compatibility.** Compatibility and integration checks for software.
5. **Critic.** The twin judges GPU Foundry, or any software developed and tested on it.

## 1. Summary

| Objective | Standing | The largest gap |
|---|---|---|
| 1 Test bed | **Strong.** 20 adapters, 10 packs, 28 harnesses; NICo, Ironic, Metal³, MAAS, Tinkerbell, KAI, Run:ai, Kueue, Volcano, NVSentinel, DPF, GPU Operator operands run unmodified | Nothing ever *runs* on the datacenter: no workload, so no load, no tenant, no job to lose. In-host software at rack scale is impersonated |
| 2 Faults | **Good mechanism, thin content.** Faults are model effects seen on every interface; 32 faults in 11 catalogues; seeded chaos; a virtual week in 45 s | No impact analysis (what-if, blast radius, campaigns), no degradation before failure, no correlated failures, no empirical rates, and no fast-forward once a stack is installed |
| 3 Physics | **Weak, by design.** "Physics" and "Performance" are non-goals in [01](../spec/01-overview.md#non-goals) | One time constant, one ambient temperature, linear formulas; no feedback from temperature or overload to the machines; nothing a controller can optimise against |
| 4 Compatibility | **Strong per harness, not a system.** `dctwin check --stack` plus real-client harnesses that have found 25+ twin bugs and a dozen upstream findings | Each harness is a bespoke script; no version matrix, no machine-readable results, no CI, no hardware-verified profile |
| 5 Critic | **Raw material only.** Every request the SUT makes is logged by canonical name with cause chains; scenarios give pass/fail | No Foundry stack or harness; no scoring, invariants, campaigns, baselines or CI gate; a verdict is one bit per scenario |

The twin today is a very good **protocol-and-state** twin. Objectives 2, 3 and 5 need it to also be
a **behavioural** twin (things happen over time for physical reasons, under load) and a **judge**
(it grades what the software did). Objective 3 contradicts the spec's non-goals, so it starts
with a decision, not with code (section 8).

## 2. Objective 1 — test bed

**Built.** See [implementation.md](../implementation.md). The breadth is the twin's strongest
property: lifecycle managers, schedulers, health agents, fabric managers, DPU provisioning and
storage clients all run unmodified against one consistent estate.

**Missing.**

| | Gap | Evidence |
|---|---|---|
| G-01 | **No workload.** Pods are bound to kwok Nodes and never run; `DCGM_FI_DEV_GPU_UTIL` and power come from `State.Utilisation`, which is `0.5` at compile and changes only by a scenario's `act: {utilisation}` | `compile.go:1272`, `scenario.go:409`, `metrics/exporters.go:204` ("no pod runs") |
| G-02 | **In-host software at scale is impersonated.** Mokka inside `os: twin` guests, the sysfs tree and a real kubelet are owed (M3); `os: stack` PCI stubs, BIOS and Secure Boot writes to firmware are not built | implementation.md, "Not built", row 1 |
| G-03 | **`hardware` fidelity is not built**, and no pack carries a `hardware-run` ledger entry: every profile is "unverified" by the spec's own rule | [07](../spec/07-fidelity.md#the-fidelity-ledger); no `checkedAgainst` in `packs/` |
| G-04 | **One host, one site.** No scale-out across hosts (Q6), one site per estate (D30); in Kubernetes only `model` estates | 10-roadmap M11; implementation.md |
| G-05 | **Protocol holes**: NVOS SSH/SFTP and NVUE writes, LLDP frames, DHCPv6, IPMI SOL, HTTPS boot, ProxyDHCP, a DPU's ConnectX (SFs, OVS, HBN), BGP from host to leaf | implementation.md, "Not built" |
| G-06 | **Facility protocols**: the BMS speaks DSX Exchange (MQTT) and PDUs SNMP. No Modbus/TCP, BACnet/IP or OPC UA, which is what most BMS/EPMS/DCIM software integrates with | `internal/adapter/` |
| G-07 | **No import of a real design.** An estate is hand-written; a customer's DC cannot be loaded from NetBox/DCIM or captured from hardware (`capture host`, `diff profile` of M3 are not in the CLI) | `cmd/dctwin` |
| G-08 | **Operability**: hot `apply` not built (a change restarts the estate), no published image, sidecar interface for out-of-tree packs not built, OIDC not built | implementation.md |

**How.**

- **G-01, a workload engine** (the keystone; objectives 2, 3 and 5 all need it). A new engine
  behaviour, not an adapter: a *job* is a set of accelerators, a utilisation profile over time
  (ramp, steady, checkpoint dips, inference diurnal curve) and a failure rule per fault class
  (the Delta study: every GPU error class but MMU and NVLink kills the job). Three sources, in
  order of cost: (a) scenario-declared jobs; (b) trace replay, in the format RAPS reads, so
  public telemetry datasets drive it; (c) **pods as jobs**: the kwok projection already sees
  bindings, so a pod bound to a Node becomes a job on that host's GPUs, with its profile taken
  from an annotation or a default per workload class. With (c), what a scheduler under test
  decides changes the hall's power and heat, and a fault under a placed gang produces "job lost,
  N GPU-hours wasted" as an event. Per-GPU utilisation replaces the per-host scalar; DCGM, Redfish
  power and the BMS follow because they already read the engine.
- **G-02**: finish M3 as planned; for scale keep D49's container hosts (real kubelet, Mokka) as
  the middle tier between kwok and `vm`.
- **G-03**: build the capture half first (`dctwin capture` against one real BMC, diff against the
  profile); it needs no bridge and gives the first ledger entries.
- **G-06**: a `modbus` and a `bacnet` adapter over the same facility devices, with a points-map
  dialect per vendor as the SNMP MIB dialect is today.
- **G-07**: `dctwin import netbox` → an estate file (racks, U positions, cables, power feeds map
  one to one onto [03](../spec/03-estate-model.md)).

## 3. Objective 2 — faults and events

**Built.** Effects on the model, shown on every interface; triggers and `after`; propagation
rules; seeded chaos; exact replay on the virtual clock; cause chains in the event log.

**Missing.**

| | Gap | Evidence |
|---|---|---|
| G-09 | **The catalogue is small.** 3 Xids (79, 48, 94); the spec's `generic/tpm`, `generic/memory`, link flap, optic failure, LLDP change, brown-out, update interrupted by power loss are not in `packs/`. No PCIe AER, thermal throttle, NIC/DPU firmware faults, PSU derating, pump or valve faults, sensor drift | `packs/*/faults` (11 files, 32 faults) vs [08](../spec/08-scenarios-and-faults.md) |
| G-10 | **Faults are steps, not processes.** Nothing degrades: no correctable-error rate that climbs before an uncorrectable one, no fan bearing, no fouling, no optic power decay. Software that predicts failure has nothing to predict from | `faults.go`: `set`, `counter` are instantaneous |
| G-11 | **No correlation or empirical rates.** Chaos is one uniform rate per device-hour. Real fleets fail by batch, by firmware, by temperature, by rack position, and in bursts | [08](../spec/08-scenarios-and-faults.md) `chaos`; Delta and Meta studies below |
| G-12 | **No impact analysis.** The twin can *do* a feed loss; it cannot *answer* "what goes down if feed A is lost", "which single failure takes out more than one NVLink domain", or "how many GPU-hours does a CDU failure cost" without someone scripting it | no such API in `internal/control` |
| G-13 | **No fast-forward with a stack.** A stack forces the wall clock (D22), so "before it happens" studies run in real time or without the software whose reaction is the question | D22 |
| G-14 | **No operational events**: maintenance windows, rolling firmware, technician error (wrong cable, wrong tray pulled), supply events (grid sag, demand response, heat wave), security events (BMC credential compromise, rogue DHCP) | scenario `act` kinds |
| G-15 | **Re-drive is not built**: a recorded incident cannot be replayed against a new version of the software | implementation.md |

**How.**

- **G-09** is data: write catalogues from NVIDIA's Xid table, the PCIe AER classes, Redfish
  message registries. Budget it per harness: each fault needs one real consumer that sees it.
- **G-10, degradation processes.** A new effect verb `process`: a named state variable with a
  rate law (`wear += k * load * arrhenius(temp)`), thresholds that fire other faults, and
  projections onto counters (SBE count, CRC count, fan RPM, optic dBm). This depends on physics
  (section 4), because the rate laws read temperature and load.
- **G-11, a reliability model.** Per component class a hazard function (Weibull shape and scale),
  covariates (temperature, load, age, firmware, batch from identity/v1) and common-cause groups
  taken from the model's own domains (feed, loop, rack, fabric tier). Seed the parameters from
  the published studies and state them in the catalogue's `isNot`.
- **G-12, `dctwin whatif` and campaigns.** (a) *Static*: the compiler already holds the power
  chain, loops, fabrics and domains as graphs; a query walks them for blast radius, single points
  of failure and N-1/N-2 survival. (b) *Dynamic*: fork the engine in memory on the virtual clock,
  apply the fault, run to quiescence, diff against the unfaulted fork. (c) *Campaign*: Monte
  Carlo over seeds with the reliability model, reporting distributions (availability, GPU-hours
  lost, time above thermal limit) rather than one run.
- **G-13.** Three partial answers, none complete: `spec.timeScale` already shortens behaviours
  under the wall clock; a **policy model** of the stack (its observed reactions, learned from the
  event log of real runs, replayed as a ghost) lets virtual-clock campaigns include an
  approximation of the stack, labelled as such; and stacks that take an injected clock can be
  run dilated. Recommend the first two.
- **G-15**: build re-drive as specified; it is also what objective 5's regression baseline needs.

## 4. Objective 3 — physics

**Built** (`internal/engine/sensors.go`, `power.go`, `chain.go`). Device power is
`idle + (max − idle) × utilisation`. Every temperature follows its target with one 30 s lag.
Ambient is a constant 25 °C. Chip temperature is `inlet + 15 + k × load` with `k` 35 (liquid) or
50 (air). On flow loss coolant rises 3 °C a minute, capped at +35. Loop return is a ratio of rack
power to loop capacity. PDU current is `P / (√3 × 400)`. A UPS drains linearly with load.
Redundancy, feeds, breakers and UPS modes are discrete and correct.

**Missing.**

| | Gap |
|---|---|
| G-16 | **No feedback from physics to the IT load.** A GPU at critical temperature keeps running at full power: no throttling, no thermal shutdown. An overloaded breaker or busway never trips (capacity is checked at compile only). Physics alarms, it never *acts* |
| G-17 | **No thermal network.** No per-component thermal mass, no cold plate or heat-sink resistance, no dependence on flow rate or fan speed, no air path (rack inlet does not depend on neighbours, containment, CRAH supply or recirculation) |
| G-18 | **No hydraulics.** Flow is `1.5 × capacity` or zero. No pumps with curves, valves, pressure, per-rack flow split, secondary/primary loops, heat exchanger approach, chiller or tower, dew point |
| G-19 | **No electrical model beyond a load sum.** No PSU efficiency curve, power factor, phase balance, inrush at rack power-on, voltage sag, transfer time, generator start, ATS, BESS, battery discharge curve |
| G-20 | **Nothing to control.** No Redfish power limit, no GPU power cap, no writable BMS set-points (supply temperature, pump speed, valve). Power-capping, demand-response and cooling-optimisation software has no actuator and no response |
| G-21 | **No plant.** The DSX Exchange catalogue names Chiller, CoolingTower, CRAH, Generator, ATS, BESS (`bms/catalogue.go`); the packs have only PDU, CDU, UPS, busway, panel and leak point |
| G-22 | **No energy accounting**: PUE, energy per job, water, carbon, cost |
| G-23 | **No performance physics**: fabric latency, loss and congestion; storage latency and throughput; job throughput as a function of throttling and placement |
| G-24 | **No calibration.** No path from real telemetry to model parameters, so numbers cannot be trusted even where formulas exist |

**How: physics as a pluggable, stepped provider.**

Today sensors are closed-form functions of committed state, which is what makes D27 (derived
state) and exact replay cheap. Real physics is state that integrates over time. Keep the engine
as the single writer and add a **physics provider** it steps on its own clock:

```
inputs  (from the engine each step):  per-component electrical load, power states, breaker and
                                      valve positions, set-points, ambient and grid boundary conditions
outputs (to the engine):              temperatures, flows, pressures, currents, voltages, state of charge,
                                      and trips (a breaker opened, a component shut down, a throttle level)
```

Trips come back as ordinary engine events with the physics step as cause, so the fidelity report,
cause chains and every adapter work unchanged. Three providers, declared per estate
(`spec.physics.level`) and named in the fidelity report as fidelity levels are:

1. **`first-order`**: today's formulas. Default; keeps virtual-week replay as it is.
2. **`lumped`** (build this; pure Go, deterministic, fixed step, runs on the virtual clock):
   - *Thermal*: an RC network per device from the profile (junction → cold plate or heat sink →
     coolant or air), resistance a function of flow or fan speed. Racks as air nodes with a
     recirculation fraction; halls as zones with CRAH supply.
   - *Hydraulic*: loops as a pipe network, pumps with quadratic curves, valves with Cv; solve
     flows each step (small systems, Newton or Hardy Cross); CDU heat exchanger by
     effectiveness-NTU; facility water with a chiller COP curve and a tower approach to wet-bulb.
   - *Electrical*: a radial load flow down the existing power chain: PSU efficiency curves, losses
     per node, phase currents, breaker inverse-time (I²t) trip curves, UPS battery as a
     state-of-charge model with a discharge curve, generator start delay and ATS transfer time,
     inrush on power-on.
   - *Feedback* (G-16): throttle levels and shutdown thresholds in the profile; a throttled GPU
     draws less, runs its job slower (the workload engine, G-01) and logs what the driver logs.
   - *Actuators* (G-20): Redfish `PowerLimit`/`Controls`, NVML power caps through Mokka, writable
     BMS points in the dialect. Each is a set-point the provider reads.
3. **`fmu`** (later): an FMI 2/3 co-simulation master as a sidecar, so a Modelica plant model
   (Modelica Buildings Library, ExaDigiT's AutoCSM output, a vendor's CDU model) replaces the
   lumped cooling model. This is the route to "actual physics" without writing a CFD solver, and
   the same seam is where an Omniverse DSX or Cadence Reality twin would attach. It forces the
   wall or a stepped clock and is not bit-exact; the report says so.

Add **`dctwin calibrate`** (G-24): fit the lumped parameters to a telemetry export (Redfish
telemetry, DCGM, BMS history) and record the fit error in the fidelity ledger. For **G-23**, a
flow-level fabric model (htsim's abstraction, not ns-3's packets) fed by the workload engine
gives link utilisation, ECN/PFC counters and a job slowdown factor; on the TAP substrate, `netem`
on a port gives real latency and loss to real clients. Storage gets the same treatment with a
queueing model per system.

What stays out: CFD, electromagnetic transients, silicon-accurate performance. They belong to
tools the `fmu` seam can attach, and each stays in `isNot`.

## 5. Objective 4 — compatibility and integration

**Built.** The capability contract (`dctwin check --stack`: met, reduced, impersonated, missing),
real-client harnesses per protocol, DMTF and SNIA validators, the canonical request log.

**Missing.**

| | Gap |
|---|---|
| G-25 | **Harnesses are bespoke.** 28 directories of shell with their own check counting; results are prose in implementation.md. No common runner, no JSON or JUnit output, no history |
| G-26 | **No CI.** No workflow in the repository; M7's nightly conformance run is not built. Results are dated snapshots |
| G-27 | **No version matrix.** One pinned version per stack, one captured firmware per device. "Does NICo vN+1 still work?" and "does it work on iDRAC 7.x as well as 6.x?" are manual |
| G-28 | **Stack definitions lag the harnesses.** 2 stack definitions for 20+ stacks run; install is what each `run.sh` does, not `dctwin stack install` |
| G-29 | **No contract extraction.** The event log knows exactly which requests a stack made; nothing turns that into a per-stack call list to diff between versions or check against a new device's dialect before running anything |
| G-30 | **No quirk library.** Real BMCs misbehave (lagging power state, stale ETags, 5xx under load, partial updates). The ledger's `divergences` mechanism is specified; no quirks exist as selectable behaviours |

**How.** One runner (`dctwin compat run <harness> --matrix`) that every harness reports to in one
result schema (check id, verdict PASS/FAIL/KNOWN/FINDING/CANNOT, evidence, fidelity report);
harness manifests declaring versions as parameters; a nightly matrix on a KVM runner publishing a
compatibility table (stack version × hardware profile × firmware). **G-29**: `dctwin contract
record` derives a stack's call list from a run, and `dctwin contract check --profile X` answers
statically whether a device's dialect serves it: a compatibility check that needs no run at all.
**G-30**: quirks as dialect-level behaviours with a fault-like catalogue, on by ledger entry.

## 6. Objective 5 — critic

**Built.** The pieces a critic needs to *see*: every SUT request by canonical name and source,
cause chains, accessors into the SUT, `expect` with `not` over a window, the fidelity report.

**Missing.**

| | Gap |
|---|---|
| G-31 | **GPU Foundry is not a stack.** No `stacks/bud-gpu-foundry`, no accessors, no `compat/foundry`. D58 names Foundry as the behaviour under test; nothing tests it |
| G-32 | **A verdict is one bit.** No measures: time to detect, time to mitigate, time to recover, blast radius of the *response*, wrong or redundant actions, capacity lost, jobs lost |
| G-33 | **No standing invariants.** Safety properties ("never power off a healthy tray", "never allocate onto a degraded domain", "never exceed a feed's capacity", "never erase a tenant's disk before release") are re-written per scenario or not checked |
| G-34 | **9 scenarios.** No library by capability (ingest, firmware, leak, power, thermal, fabric, tenant lifecycle, scheduling), no generated scenarios |
| G-35 | **No campaigns or baselines.** No matrix of scenarios × estates × seeds, no comparison of this build of the SUT against the last, no flake statistics |
| G-36 | **No adversarial search.** Nothing looks for the fault sequence that breaks the SUT |
| G-37 | **No critic output for developers or agents.** No report a PR can gate on, no structured critique an AI coding agent can consume |

**How.**

- **G-31**: write the Foundry stack definition first (requirements, install, accessors for
  machine, allocation, alert, job). Everything else in this section is generic and Foundry is its
  first user.
- **G-32, G-33, a judgement layer over the event log.** *Invariants* are CEL over the model and
  the event stream, declared in the stack definition or a policy file, evaluated continuously for
  a whole run, in every scenario. *Measures* are computed from the log: fault commit → first SUT
  read that could reveal it → first SUT act → model healthy again. A **scorecard** per run:
  correctness (assertions), safety (invariant violations), timeliness (MTTD/MTTR against SLOs the
  scenario states), efficiency (requests made, redundant actions, capacity and GPU-hours lost,
  which needs G-01), with the fidelity report attached as always.
- **G-34 to G-36**: a scenario library organised by capability; a **campaign runner**; property-
  based generation (random valid fault sequences from the catalogue, shrunk to a minimal failing
  sequence when an invariant breaks: on the virtual clock for the twin itself, under `timeScale`
  with the SUT); stored baselines and a diff ("Foundry build B took 40 s longer to cordon and
  powered off two healthy trays").
- **G-37**: JSON and JUnit from `scenario run` and `campaign run`; an HTML report; a GitHub
  Action; and an MCP server over the control API (inject, observe, score) so an agent developing
  Foundry can run the critic in its own loop and read *why* it failed from the cause chain.

## 7. What the research says

- **ExaDigiT** (ORNL, SC24) is the closest open design for objective 3: a resource and power
  simulator (RAPS) driving a Modelica thermo-fluid cooling model through an FMU, validated by
  replaying six months of Frontier telemetry. It has what the twin lacks (workload-driven power,
  transient cooling, validation against telemetry) and lacks what the twin has (protocols, real
  software in the loop). The provider seam in section 4 is how to join them.
- **NVIDIA Omniverse DSX Blueprint** is the commercial direction: OpenUSD SimReady assets with
  CFD and electrical simulation from Cadence, ETAP, Schneider, Siemens and Vertiv, and agents
  (Phaidra, Emerald AI) trained in the twin to manage power, cooling and workloads. It confirms
  that controllers acting on physics (G-20) is the use case, and that high-fidelity physics is
  something to attach, not to rewrite.
- **Reliability studies** give the parameters G-11 needs. NCSA Delta (2.5 years, 1,056 A100/H100):
  H100 per-GPU mean time between errors 3.2× lower than A100, nearly every error class fatal to
  the job, about 5 % overprovisioning needed at scale. Meta's RSC clusters: InfiniBand links,
  file-system mounts, GPU memory and PCIe errors dominate, and a few percent of failures co-occur.
- **Network simulation**: packet-level simulators (ns-3) are too slow at cluster scale; flow-level
  ones (htsim, as used by ASTRA-sim and SimAI) are what G-23 should copy. NVIDIA DSX Air is
  configuration and automation validation, which the twin's real-NOS substrate already covers.

## 8. Decisions to take

| | Decision | Recommendation |
|---|---|---|
| X-1 | Do "Physics" and "Performance" stay non-goals? | Revise [01](../spec/01-overview.md#non-goals): physics becomes a **declared level** (`first-order`, `lumped`, `fmu`), reported like device fidelity. CFD and silicon performance stay non-goals |
| X-2 | Is a workload model in scope, given principle 2 ("never emulate the software under test")? | Yes: a job is the *load on the hardware*, not the scheduler. Pods-as-jobs reads the SUT's decisions, it does not make them |
| X-3 | Lumped physics in Go, or FMU only? | Both, Go first: it is deterministic, needs no toolchain and runs on the virtual clock; FMU is the accuracy route |
| X-4 | Virtual time with a stack (G-13) | Keep D22; add policy-model ghosts for campaigns, labelled |
| X-5 | Where the critic lives | In the twin (`internal/judge`), stack-neutral; Foundry supplies a stack definition and policies |

## 9. A phased path

| Phase | Builds | Closes | Exit |
|---|---|---|---|
| **A — Critic scaffold** | Foundry stack definition and harness; result schema (JSON/JUnit); standing invariants; measures and scorecard; CI workflow | G-25, G-26, G-31, G-32, G-33, G-37 | A Foundry PR is gated by a scorecard from the existing scenarios |
| **B — Workload** | Job model, per-GPU utilisation, pods-as-jobs, trace replay, job-loss events | G-01 | A scheduler's placement changes rack power; Xid 79 under a gang reports GPU-hours lost |
| **C — Lumped physics** | Provider interface; thermal, hydraulic and electrical models; throttling, shutdown and breaker trips; power caps and writable set-points; chiller, tower, CRAH, generator, ATS, BESS profiles; energy accounting; Modbus and BACnet | G-06, G-16 to G-22 | A CDU pump failure throttles then shuts down trays in an order the model explains; a power-capping controller holds a feed under its limit |
| **D — Fault realism and impact** | Catalogue growth, degradation processes, reliability model, `whatif`, campaigns, re-drive, operational events | G-09 to G-15, G-34 to G-36 | "Lose feed A at 80 % load" returns the distribution of jobs lost and the time to thermal limit, with and without the stack's policy model |
| **E — Trust** | `capture` and `calibrate`, first hardware-run ledger entries, version matrix, contract extraction, quirks, FMU provider, flow-level fabric model | G-03, G-07, G-23, G-24, G-27 to G-30 | One rack's model is fitted to real telemetry with a stated error |
| **F — Depth and scale** | Mokka and kubelet in guests, protocol holes, hardware bridge, scale-out | G-02, G-04, G-05, G-08 | The roadmap's M3, M7 and M11 exits |

A comes first because it is small, makes the twin useful to Foundry at once, and gives every later
phase a way to show its value. B precedes C because physics without a varying load has nothing to
respond to. E is where "real DC emulation" becomes a claim with evidence instead of a design.

## Sources

- [ExaDigiT documentation](https://exadigit.readthedocs.io/); [A Digital Twin Framework for Liquid-cooled Supercomputers as Demonstrated at Exascale](https://arxiv.org/html/2410.05133v1)
- [NVIDIA Omniverse DSX Blueprint overview](https://docs.omniverse.nvidia.com/dsx/latest/overview.html); [NVIDIA blog](https://blogs.nvidia.com/blog/omniverse-dsx-blueprint)
- [Delta GPU resilience study, arXiv 2503.11901](https://arxiv.org/html/2503.11901v4); [Revisiting Reliability in Large-Scale Machine Learning Research Clusters (notes)](https://glennklockwood.com/garden/papers/revisiting-reliability-in-large-scale-machine-learning-research-clusters)
- [SimAI](https://github.com/aliyun/SimAI); [ASTRA-sim with ns-3](https://bpb-us-e1.wpmucdn.com/sites.gatech.edu/dist/c/332/files/2020/09/astrasim-ns3_hoti2020.pdf); [Towards Easy and Realistic Network Infrastructure Testing](https://arxiv.org/html/2504.20854v1)

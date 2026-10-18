# 08 — Scenarios and faults

## Faults

A fault is defined **once, in a pack's catalogue, as effects on the model**
([13 — Fault catalogue](13-schemas.md#fault-catalogue)). It is never defined as a change to one
interface ([principle 5](01-overview.md#principles)). Adapters then show it wherever a real
device would.

```yaml
apiVersion: dc-twin/v1alpha1
kind: FaultCatalogue
metadata: { name: nvidia/xid, version: 0.1.0 }
spec:
  faults:
    - name: xid-79
      title: "GPU has fallen off the bus"
      target: { component: accelerator, vendor: nvidia }
      effects:
        - set:        { health: Critical, state: offline }
        - set:        { power.watts: 0 }
        - ports:      { owner: self, kind: scale-up, state: down }
        - kernelLog:  { line: "NVRM: Xid (PCI:{pci}): 79, pid=*, GPU has fallen off the bus.", appliesAt: [vm] }
        - shim:       { driver: nvidia/mokka, fault: fallen-off-bus, appliesAt: [vm] }
        - redfish.logEntry: { severity: Critical, messageId: "{dialect}" }
      clears: { on: [device.acCycle], manual: true }
      isNot: ["A real Xid 79 also leaves PCIe link-state artefacts the host firmware reports; this one does not."]
    - name: xid-48
      title: "Double-bit ECC error"
      target: { component: accelerator, vendor: nvidia }
      effects:
        - set:        { health: Critical }
        - counter:    { ecc.volatile.dbe: +1 }
        - kernelLog:  { line: "NVRM: Xid (PCI:{pci}): 48, …", appliesAt: [vm] }
        - shim:       { driver: nvidia/mokka, xid: 48, appliesAt: [vm] }
      clears: { on: [device.reset], manual: true }
      isNot: []
```

### Effect verbs

| Verb | Does | At `model` | At `vm` |
|---|---|---|---|
| `set` | sets owned fields on the target | yes | yes |
| `counter` | increments an error counter | yes | yes (also visible through the shim) |
| `ports` | changes port state for ports the target owns or the device has | yes | yes |
| `kernelLog` | writes a driver line the fault would produce | recorded on the model only | written to `/dev/kmsg` by the in-host agent |
| `shim` | tells the in-host accelerator shim to present the fault | no effect | yes |
| `redfish.logEntry`, `redfish.property` | SEL and LogService entries; a dialect-specific property | yes | yes |
| `bms` | publishes a point in the estate's BMS dialect | yes | yes |
| `power.loss` | the redundancy outcome: evaluates the device's or rack's power inputs (`minSources`, `redundancy`) and hard-powers-off what has lost supply | yes | yes |
| `link` | takes a link down or up (both ends, LLDP) | yes | yes |
| `attestation` | makes the next measured-boot quote fail or pass | the boot walk reports it | PCRs really differ (the agent changes a measured component) |
| `pack/handler` | an `EffectHandler` from a pack ([06](06-packs-and-stacks.md#code-in-packs)) | as declared | as declared |

A profile's `faults` list is a **strict allow-list**; a fault outside it is refused. Facility faults
reach devices through the rack format's **`propagation`** rule (a leak at a rope sensor becomes
leak detectors tripping in the trays the rule names) and never by code in the catalogue.

| Catalogue | Examples |
|---|---|
| `generic/power` | Feed loss, PSU failure, PDU outlet off, power-shelf output loss, brown-out |
| `generic/bmc` | BMC unreachable, BMC slow (latency, 5xx), BMC reset, credentials changed out of band, certificate expired |
| `generic/thermal` | Fan failure, inlet temperature rise, CDU flow loss, coolant temperature excursion |
| `generic/leak` | In-tray leak detector trips; a rope, drip-pan or CDU leak point; the format's propagation applies |
| `generic/link` | Cable pulled, link flap, CRC errors, optic failure, LLDP neighbour change |
| `generic/firmware` | Version drift from baseline, a failed update, an update interrupted by power loss |
| `generic/storage`, `generic/memory` | Drive failure, SMART warnings, correctable and uncorrectable memory errors |
| `generic/tpm` | PCR change (boot-chain tamper), EK not chaining to a known CA |
| `nvidia/xid`, `nvidia/nvlink`, `nvidia/dpu` | Xid classes; NVLink link and switch-tray failures; DPU unresponsive |
| `amd/ras`, `intel/gaudi` | Written with their packs |

A fault can be **injected** (`dctwin fault inject <selector> <fault>`), **scheduled** in a
scenario, or **random**: a rate per device class, drawn from the estate's seed, set in the
scenario's `chaos` block or through the control API. A fault's effects are applied by the engine;
a fault never reaches into a protocol directly.

## Clocks

| Clock | When | Properties |
|---|---|---|
| **`virtual`** | Every device is `model`, every fabric is `nos: none`, and **no stack is installed** | Time advances by command or as fast as events allow. A run with a fixed seed replays exactly. It is the twin's **self-test mode**: property tests, replay of a recorded run, a week of fleet behaviour in minutes with no software under test |
| **`wall`** | Anything real: a `vm` or `hardware` device, a real NOS, or any installed stack | Behaviours take real time. Runs are repeatable, not bit-exact |

Why a stack forces the wall clock: the software under test runs on wall time and cannot be
time-warped. Advancing virtual time would hand it a thousand lease renewals in a burst and tasks
that completed before its first poll, and "replaying exactly" would mean re-driving its recorded
requests without running it, which tests nothing about it
([D22](11-decisions.md#d22--an-installed-stack-forces-the-wall-clock-exact-replay-is-a-twin-only-run)).
The validator refuses `clock: virtual` for such an estate.

## Scenarios

A scenario is a file ([13 — Scenario](13-schemas.md#scenario)): estate constraints, the stack
components it needs, steps and assertions. It runs on the estate it is given, and reads the stack
only through **aliases** mapped to the stack definition's accessors, so the same scenario can run
against another vendor's software. This one is
[`examples/scenarios/nvl72-leak-response.yaml`](../../examples/scenarios/nvl72-leak-response.yaml),
written against NICo's documented leak handling ([stack doc](../stacks/nvidia-dsx-os.md#the-two-leak-paths)):

```yaml
apiVersion: dc-twin/v1alpha1
kind: Scenario
metadata: { name: nvl72-leak-response }
spec:
  estate: { requires: { formats: [nvidia/gb300-nvl72], capabilities: [facility/bms:nvidia/dsx-exchange] } }
  stack:
    components: [nico, dsx-exchange]
    accessors: { machine: nico.machine, alerts: nico.alerts }
  timeout: 30m
  steps:
    - wait: { for: 'model.select("a03/compute-tray-*").all(d, accessors.machine(d.identity.serial).state == "Ready")', within: 20m }

    # Path 1 — a tray's own leak detector. The stack reads it over Redfish and shuts that tray down.
    - fault: { id: tray, select: "a03/compute-tray-07", inject: generic/leak/tray-detector }
    - expect:
        within: 3m
        all:
          - redfish: { select: "a03/compute-tray-07", property: "Chassis/*/ThermalSubsystem/LeakDetection/LeakDetectors/*/DetectorState", equals: Leak }
          - event:   { canonical: power.off.graceful, from: stack, on: "a03/compute-tray-07" }
          - redfish: { select: "a03/compute-tray-07", property: "Systems/*/PowerState", equals: "Off" }
          - not: { event: { canonical: power.off.graceful, on: "a03/compute-tray-{01..06}" } }
          - not: { event: { canonical: power.off.graceful, on: "a03/compute-tray-{08..18}" } }

    # Path 2 — a BMS rack leak point. The stack raises an alert that blocks allocation; it powers nothing off.
    - fault: { id: rack, select: "a03/leak-sensor-02", inject: generic/leak/rack-point }
    - expect:
        within: 2m
        all:
          - bms:      { topic: "BMS/v1/PUB/Value/Rack/RackLeakDetect/#", value: true }
          - accessor: { alias: alerts, args: { rack: a03 }, equals: { kind: RackLeak, preventAllocations: true } }
          - not: { event: { canonical: power.off.graceful, on: "a03/compute-tray-*" } }   # beyond path 1's tray

    - fault: { clear: tray }
    - fault: { clear: rack }
  report: fidelity
```

### Step kinds

| Step | Meaning |
|---|---|
| `wait` | a CEL condition over the model, accessors, events and clock ([14](14-expressions.md)), with a `within` |
| `act` | a technician act: power, cable pull or replace, reseat, or a plain delay |
| `fault` | inject (optionally with an `id` for a later `clear`) or clear |
| `expect` | a window and a list of observations. A positive observation must become true within the window. **A `not` must hold for the whole window**, so the step cannot pass before the window ends |
| `setup` | steps run before the scenario's clock starts (a tenant cluster to exist, a fault pre-injected) |

### What an assertion may observe

| Source | Example | Rule |
|---|---|---|
| **Device protocols** | a Redfish property, an IPMI sensor, a NOS port state, a UFM partition | What the stack's effect looks like **from the hardware side**. The preferred source |
| **The twin's publishers** | `bms: {topic, value}` | Observed at the **publisher's log**, never by subscribing to the stack's broker |
| **The event log** | `event: {canonical: power.off.graceful, from: stack, on: …}` | Which requests arrived, from whom, in what order, by [canonical name](14-expressions.md#canonical-request-names) |
| **The stack's own API** | `accessor: {alias: machine, …}` | Only through accessors the stack definition declares, with a **read-only credential the twin issued at install time** and the declared identity join (serial or BMC MAC) ([13 — Stack](13-schemas.md#stack)). A scenario never reads a stack's database |

The scenario runner is **outside** the software under test's view ([D10](11-decisions.md),
[15](15-security.md)). Accessor reads go through the one-way observer path. The stack sees only
the faults' effects, never the scenario.

### Results

A scenario ends `passed`, `failed`, `not-run` or `impersonated`, always with its fidelity report.
`not-run` covers an unmet requirement and a **twin incident** during the run ([16](16-testing.md#twin-incidents)).
`impersonated` is a run in which an impersonator was enabled: useful at scale, never a conformance
pass ([D24](11-decisions.md#d24--kwok-and-vendor-agent-mocks-are-impersonation)). A stack's
conformance suite is its set of scenarios ([06](06-packs-and-stacks.md#stacks)); a suite result
counts only `passed` and `failed`.

## Record and replay

- **Record.** The committed event log is the run's recording: every request, state change, fault,
  act and observation, with sequence, clock time and wall time ([13 — Event](13-schemas.md#event)).
- **Replay** is a **twin-only** operation under the virtual clock: the recorded inputs (acts,
  faults, and the stack's requests re-driven as a ghost) are applied to the engine and the resulting
  log must be identical. It debugs the twin and reproduces a flaky observation. **It does not run
  the stack and says nothing about it.**
- **Re-drive** under the wall clock replays the same faults and acts at the same offsets against a
  live estate with the stack installed. It is repeatable, not exact, and the comparison report says
  where the two runs diverged.

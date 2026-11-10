# 17 — The critic

Draft 0.2, 2026-11-10. **Built** (`internal/critic`, `policies/`, C1 to C5 of the
[build order](#build-order)); what the build changed is in [As built](#as-built), and the results
in [implementation.md](../implementation.md#the-critic). It closes G-25 and G-31 to G-37 of the
[objectives gap analysis](../reviews/2026-11-10-objectives-gap-analysis.md). Its decisions are
D64 to D70 in [11](11-decisions.md).

A scenario today ends in one word: `passed` or `failed`. That says whether the software under
test (SUT) did the few things the scenario's author thought to assert. It does not say how fast,
at what cost, what else the SUT did meanwhile, or whether it did something no scenario forbade.
The **critic** is the part of the twin that judges a run: it watches everything the SUT did to the
datacenter, checks it against stated rules, measures it, scores it, and explains each failure with
the events that prove it.

The critic is **stack-neutral**. It holds no knowledge of GPU Foundry, NICo or any other stack.
Bud GPU Foundry is its first user and brings only data ([What a stack brings](#what-a-stack-brings)).

## Principles

1. **The critic judges from the hardware side.** Its evidence is the committed event log
   ([13 — Event](13-schemas.md#event)), the model, and the stack's declared accessors. It never
   reads the SUT's database, logs or source, and the SUT cannot see it
   ([D10](11-decisions.md), [08](08-scenarios-and-faults.md#what-an-assertion-may-observe)).
2. **The SUT is not changed.** No SDK, no annotation, no test mode ([G1](01-overview.md#goals)).
   Everything the critic needs is in what the SUT already does to devices.
3. **Someone states what is right.** The critic cannot know that powering off a tray is correct.
   Rules come from three places, in rising specificity: the twin's generic policies (things no
   datacenter software should do), the stack's policy (what this software promises), and the
   scenario (what this situation requires).
4. **A judgement is evidence, not opinion.** Every violation carries its **witness**: the event
   sequence numbers, the offending request with its protocol and principal, and the cause chain.
   A rule that cannot produce a witness is not a rule.
5. **No judgement without its fidelity report** ([D11](11-decisions.md)). A score is conditional
   on what the run modelled; a measure that depends on an effect not applied at this fidelity is
   `n/a`, never zero and never full marks.
6. **The log is the recording, so judging is repeatable.** A recorded run can be re-judged
   offline under a new policy without running the SUT again.
7. **The critic is tested like an adapter**: against a SUT with known defects
   ([Verifying the critic](#verifying-the-critic)).

## Vocabulary

| Term | Meaning |
|---|---|
| **Run** | One execution being judged: a scenario, one cell of a campaign, or an open session an operator starts and stops |
| **Subject** | What is being judged: the stack, its component versions, and a build reference the caller supplies (`--subject foundry@<git sha>`) |
| **Policy** | A file of invariants, measures and objectives (`kind: Policy`) |
| **Invariant** | A rule over the whole run, in one of five temporal forms |
| **Measure** | A number computed from the log: an interval, a count, an integral |
| **Objective** | A measure with pass and warn thresholds, absolute or against a baseline |
| **Witness** | The events that prove a violation or define a measure's value |
| **Judgement** | The critic's output for a run (`kind: Judgement`): verdict, scorecard, violations, measures, coverage, fidelity report |
| **Campaign** | A matrix of runs (`kind: Campaign`) and their aggregate judgement |
| **Baseline** | A stored judgement or campaign result that later ones are compared with |

## What a stack brings

Nothing in the SUT's code. A stack that wants to be judged supplies, as files beside its stack
definition:

| | What | Required | Why |
|---|---|---|---|
| 1 | A **stack definition** ([13 — Stack](13-schemas.md#stack)): components, how to install them, `requires`, `inputs`, `outputs` | yes | So the twin can check capabilities, hand it the estate, and attribute its requests |
| 2 | **Principals**: which source addresses or client certificates are which component | recommended | So the log says `foundry.scheduler` powered the tray off, not `stack` |
| 3 | **Accessors** mapped to the [standard aliases](#standard-aliases) | recommended | Without them the critic sees only device-side behaviour: it cannot tell "noticed and chose not to act" from "did not notice", nor judge allocation decisions |
| 4 | A **policy**: its playbook (what it promises to do for each fault class), its own invariants, its SLOs | optional | Without it only the generic policies and the scenarios' assertions apply |
| 5 | **Scenarios** of its own | optional | The library's stack-neutral scenarios run against any stack that maps the aliases |

A stack with only item 1 still gets a judgement: generic safety, timeliness of device-side
reactions, efficiency, and every scenario that needs no accessor.

### Standard aliases

Scenarios and policies read a stack only through aliases ([08](08-scenarios-and-faults.md#scenarios)).
The critic's generic policies and the scenario library use this fixed set, so a stack maps each one
it can answer and is judged on those:

| Alias | Answers | Joined by |
|---|---|---|
| `machine(id)` | lifecycle state of a host as the stack sees it: `state` (`Discovered`, `Ready`, `Assigned`, `Maintenance`, `Failed`, …, the stack's own words mapped by `stateMap`), `healthy` | serial or BMC MAC |
| `alerts(scope)` | open alerts: `kind`, `severity`, `subject`, `preventAllocations`, `since` | rack, device |
| `allocation(id)` | a tenant's allocation: `machines[]`, `state`, `tenant` | — |
| `allocatable(scope)` | machines the stack would hand out now | rack, domain |
| `node(name)` | a Kubernetes Node as the stack's cluster reports it | host path |
| `workload(id)` | a job or service: `nodes[]`, `state`, `restarts` | — |

`stateMap` in the stack definition maps the stack's state names onto the alias's vocabulary. An
alias a stack does not map makes the rules that need it `n/a` for that stack, reported as such.

### GPU Foundry

Foundry is a stack like any other: `stacks/bud-gpu-foundry/stack.yaml`, `policy.yaml` and a
harness under `compat/foundry`. Where it runs above another manager (NICo), the estate installs
both stacks and the principals keep their requests apart, so a judgement can say which of the two
made a request. This chapter does not specify Foundry's definition: it is written against
Foundry's own install and API ([Q21](#open-questions)).

## Evidence

Everything is derived from three sources, and each addition below is a change to the twin, not to
the SUT.

| Source | Today | Added for the critic |
|---|---|---|
| Event log | requests with canonical names and status, state changes, faults, acts, observations, cause chains | **(a)** the canonical registry ([14](14-expressions.md#canonical-request-names)) gains a `class` column: `read`, `write` or `destructive` (`power.off.*`, `erase.start`, `bmc.defaults`, `firmware.update.*`, `tpm.clear`, …). **(b)** a `fault` event carries `data.affected`: the paths its effects touched, extended as cause-chained state events commit. **(c)** `kind: run` events mark a run's start and end with its id and subject. **(d)** every accessor read the critic makes is committed as an `observe` event with the decoded answer (redacted), so an offline re-judgement has it |
| Model | owned and observed state | `model.at(seq)`: the state as of an event, rebuilt from the log, for rules that ask what was true *before* a request |
| Accessors | read on demand by `wait` and `expect` | sampled on a period (`policy.sampling.accessors`, default 5 s) and at every fault and stack write, through the one-way observer path |

### The five moments of a fault

For every injected fault the critic derives five timestamps. They are the basis of every
timeliness measure and are defined from the hardware side:

| Moment | Defined as | Needs |
|---|---|---|
| **Injected** | the fault's event | — |
| **Exposed** | the first time evidence reached the SUT: a push the twin delivered (an SSE event, a BMS publish, an SNMP trap, a kernel-log record), or the first SUT read of a path in `affected` that the twin answered after the fault | — |
| **Acknowledged** | the stack's own view first shows it: an `alerts` entry on the subject, or `machine().healthy == false` | accessors |
| **First action** | the first SUT request of class `write` or `destructive` on a path in `affected`, or on a path the policy's playbook names for this fault | — |
| **Mitigated** | the playbook's `mitigated` condition for this fault class first holds (the leaking tray is off; the node is cordoned and drained) | playbook |
| **Recovered** | after the fault clears: the model is healthy again and, with accessors, `machine().state` is back in service | — |

*Exposed* separates the twin's delay from the SUT's. A stack that polls every five minutes is
exposed late by its own choice, and `exposed − injected` shows it; `acknowledged − exposed` is its
processing time; `first action − acknowledged` is its decision time.

### Collateral

The **blast radius of a fault** is `affected`. The **blast radius of the response** is every path
the SUT wrote to between *exposed* and *recovered* whose write is cause-attributable to no other
open fault or scenario act. **Collateral** is the response's radius minus the fault's and minus
what the playbook permits. Collateral is a measure (device count, capacity in accelerators); whether
any collateral fails the run is for an invariant to say.

## Rules

### Invariants

Five forms, chosen so each can be checked incrementally on the event stream and each yields a
witness. All conditions are CEL ([14](14-expressions.md)) over `model`, `events`, `accessors`,
`clock`, plus `event` (the matched event), `before` (`model.at(event.seq − 1)` for its path),
`fault` (in fault-scoped rules) and `run`.

| Form | Holds when | Violation witness | From |
|---|---|---|---|
| `never: {event, where?}` | no committed event matches the filter and condition | the matching event and its cause chain | safety |
| `always: {each?, holds}` | the condition is true after every committed event that touches its scope, and at every sample | the first event after which it was false, and the state | safety |
| `leadsTo: {when, then, within, unless?}` | every occurrence of `when` (an event filter, a fault filter, or a condition's rising edge) is followed by `then` within the window, unless `unless` held | the trigger, and the window's end with what was observed instead | liveness |
| `atMost: {count, per, max, window?}` | matching events per device, fault or run stay within a budget | the event that exceeded it and those before | restraint |
| `sometimes: {holds \| event}` | it happened at least once. Never a failure of the SUT: unmet, it is reported as **uncovered** | — | coverage |

`leadsTo` and `never` cover what `expect` does within one scenario step; as invariants they hold
for the **whole run and in every scenario**, which is the difference. A `never` and an `always`
are void for any interval in which a twin incident was open
([16](16-testing.md#twin-incidents)): the run is `not-run`, as today.

Each invariant has a `dimension`, a `severity` and may be `key: true`. A violated key invariant
fails the run whatever its score.

### The generic policies

Shipped with the twin, applied to every run unless a policy disables one by name with a stated
reason (printed in the judgement). They need no accessors unless marked.

| Policy | Rules (selected) |
|---|---|
| `generic/safety` | no `destructive` request on a device that was healthy, on, and outside every open fault's `affected` set and every maintenance the scenario declared (**key**) · no erase, TPM clear or BMC factory reset on a machine whose `allocation` is active (**key**, accessors) · no firmware update pushed to a BMC while another is running on it · no request with a credential the twin issued to a different principal (**key**) · no power-on into a rack the BMS has isolated, or onto a feed at capacity |
| `generic/restraint` | at most N resets per host per hour · no repeated identical write within its task's lifetime · no unbounded retry against an unreachable BMC (back-off observed) · request rate per BMC under a ceiling |
| `generic/hygiene` | every session created is deleted or expires · every event subscription removed on teardown · no credential in a URL · 4xx answers to the SUT under a ratio (it is asking for things that are not there) |
| `generic/liveness` | every fault with severity Critical is *first-actioned* or *acknowledged* within a default window, else it is reported as **unnoticed** (a warning by default: the right response to some faults is none) |
| `generic/allocation` (accessors) | nothing in `allocatable` is Critical, off, isolated or in an unformed or degraded scale-up domain · an allocation's machines are all in one domain when it asked for one |

### The playbook

A stack's policy states what it promises per fault class. The critic turns each entry into
`leadsTo`, `never` and measure definitions:

```yaml
apiVersion: dc-twin/v1alpha1
kind: Policy
metadata: { name: bud-gpu-foundry/default, version: 0.1.0 }
spec:
  extends: [generic/safety, generic/restraint, generic/hygiene, generic/liveness, generic/allocation]
  playbook:
    - on: generic/leak/tray-detector
      mitigated: 'model.device(fault.path).power.state == "Off"'
      within: 3m
      permits:  [{ canonical: power.off.graceful, on: fault.path }]
      forbids:  [{ class: destructive, on: 'fault.rack + "/compute-tray-*"', except: fault.path }]
    - on: nvidia/xid/xid-79
      acknowledged: 'accessors.alerts({"device": fault.host}).exists(a, a.kind == "GpuFault")'
      mitigated: 'accessors.node(fault.host).unschedulable'
      within: 2m
      then: { recovered: 'accessors.machine(fault.host).state == "Ready"', within: 30m }
    - on: generic/bmc/unreachable
      mitigated: 'accessors.machine(fault.device).state == "Maintenance"'
      within: 10m
      forbids: [{ class: destructive, on: fault.device }]
  invariants:
    - name: one-tenant-per-nvlink-partition
      dimension: safety
      key: true
      always: { each: "?class=compute-tray", holds: 'run.tenantsOf(device.domain).size() <= 1' }
  objectives:
    - { measure: fault.mitigated, where: 'fault.id.startsWith("generic/leak/")', pass: "<=3m", warn: "<=5m", key: true }
    - { measure: fault.acknowledged, pass: "<=60s", warn: "<=120s" }
    - { measure: response.collateral.accelerators, pass: "==0" }
    - { measure: requests.perDeviceHour, pass: "<=+10%", compare: baseline }
  weights: { correctness: 4, safety: 4, timeliness: 2, restraint: 1, hygiene: 1 }
  isNot: ["Says nothing about tenant-visible performance: no workload runs (G-01)."]
```

A fault with no playbook entry is judged by the generic policies only. A playbook entry whose
fault the run never injected is **uncovered**.

### Measures

A measure is `interval` (between two moments or event filters), `count` (of events), `ratio`, or
`integral` (of a model expression over the run, on the twin's clock). Each has a `per` (`fault`,
`device`, `run`) and a unit. Built in:

| Measure | Kind | Per |
|---|---|---|
| `fault.exposed`, `fault.acknowledged`, `fault.firstAction`, `fault.mitigated`, `fault.recovered` | interval from *injected* | fault |
| `response.requests`, `response.writes`, `response.destructive` | count between *exposed* and *recovered* | fault |
| `response.collateral.devices`, `response.collateral.accelerators` | count | fault |
| `capacity.lost` | integral of accelerators not allocatable, accelerator-seconds | run |
| `requests.perDeviceHour`, `requests.errorRatio`, `sessions.leaked` | ratio, count | run |
| `ingest.time` | interval from a BMC's first lease to `machine().state == "Ready"` | device |

**Time.** The SUT lives on wall time ([D22](11-decisions.md#d22--an-installed-stack-forces-the-wall-clock-exact-replay-is-a-twin-only-run)),
so intervals are wall-time. A run with `spec.timeScale ≠ 1` shortens the twin's behaviours but not
the SUT's; its timeliness objectives are reported `scaled` and do not count toward a baseline.

### Objectives and the scorecard

An objective is a measure with `pass` and `warn` criteria: absolute (`<=3m`), or relative to a
baseline (`<=+10%`, with `compare: baseline`). Per-fault measures aggregate by `p50`, `p95` or
`max` (default `max`).

The scorecard has six **dimensions**:

| Dimension | Fed by |
|---|---|
| **Correctness** | scenario assertions; playbook `mitigated` and `recovered` conditions |
| **Safety** | `never` and `always` invariants; playbook `forbids`; collateral objectives |
| **Timeliness** | objectives on the five moments and on ingest |
| **Restraint** | `atMost` budgets; request volume and redundant-action objectives |
| **Hygiene** | sessions, subscriptions, error ratio |
| **Coverage** | `sometimes` rules and playbook entries exercised. Reported, never scored: it measures the run, not the SUT |

Each scored item is 1 (pass), 0.5 (warn) or 0 (fail); `n/a` items are left out and listed. A
dimension's score is the weighted mean of its items, the run's score the weighted mean of its
dimensions by the policy's `weights`. The score is a summary for trends. **The verdict does not
come from the score**:

| Verdict | When |
|---|---|
| `failed` | any scenario assertion failed, any `key` invariant was violated, or any `key` objective failed |
| `passed` | none of those; non-key violations and objective failures are listed and lower the score |
| `not-run` | an unmet requirement, or a twin incident |
| `impersonated` | an impersonator was enabled ([D24](11-decisions.md#d24--kwok-and-vendor-agent-mocks-are-impersonation)); scored and listed, never a pass |

These are the four results of [08](08-scenarios-and-faults.md#results); the critic adds none. A
policy may set `gate: {score: ">=0.9"}` for CI, which is a property of the pipeline, not a fifth
verdict.

## The judgement

`kind: Judgement`, JSON, written to the state directory for every run and returned by the control
API.

| Field | Meaning |
|---|---|
| `run` | id, kind (`scenario` \| `campaign-cell` \| `session`), scenario, seed, start and end `seq` and wall time |
| `subject` | stack name and version, component versions, the caller's build reference |
| `estate` | name, file digest, lockfile digest, `timeScale` |
| `policies[]` | name, version and digest of every policy applied; rules disabled, each with its reason |
| `verdict`, `score`, `dimensions{}` | as above |
| `assertions[]` | the scenario's steps and results |
| `violations[]` | `rule`, `policy`, `dimension`, `severity`, `key`, `message`, `witness: {events: [seq], request?, principal?, causeChain: [seq]}`, `expected`, `observed` |
| `faults[]` | per injected fault: id, path, `affected`, the five moments, its response's requests by canonical name and principal, collateral |
| `measures[]`, `objectives[]` | value, unit, per, witness; criteria, result, baseline value |
| `coverage` | playbook entries and `sometimes` rules: exercised or not; fault classes injected; canonical requests the SUT made |
| `na[]` | rules and measures not applicable, each with why (alias unmapped, effect not applied at this fidelity, `scaled`) |
| `fidelity` | the fidelity report ([07](07-fidelity.md)) |
| `incidents[]` | twin incidents |

Renderings of the same judgement: text (the default, printed after the fidelity report), JUnit
XML (one test case per assertion, invariant and objective), Markdown (a pull-request comment),
and HTML with the event timeline. `dctwin scenario run` exits 0 for `passed`, 1 for `failed`,
2 for `not-run`, 3 for `impersonated`, and 4 when the policy's `gate` fails.

### Explanations

For each violation the critic builds an explanation from the witness, with no model of the SUT:

```
VIOLATION generic/safety/no-destructive-on-healthy  (safety, key)
  foundry.remediator sent power.off.force to a03/compute-tray-08 at +41.2 s (seq 18844, Redfish
  POST …/Actions/ComputerSystem.Reset ForceOff).
  Before it: a03/compute-tray-08 was On, health OK, in no open fault's affected set.
  Open faults: generic/leak/tray-detector on a03/compute-tray-07 (seq 18102), affected
  [a03/compute-tray-07].
  Nearest permitted action: power.off.graceful on a03/compute-tray-07 (playbook), which the SUT
  made at +38.9 s (seq 18790).
  Reads that preceded the request (same principal, 60 s): LeakDetectors of tray-07 (Leak),
  tray-08 (OK), Chassis of a03.
```

The last paragraph, what the SUT read before it acted, is the most useful line for whoever fixes
it, and is available because every read is logged.

## Campaigns

One run is an anecdote. A campaign is a matrix and its statistics.

```yaml
apiVersion: dc-twin/v1alpha1
kind: Campaign
metadata: { name: foundry-nightly }
spec:
  estates:   [examples/estates/nvl72-nico.yaml, examples/estates/mixed-hall.yaml]
  scenarios: { library: [ingest, leak, power, accelerator, bmc], files: [scenarios/foundry/*.yaml] }
  policy:    stacks/bud-gpu-foundry/policy.yaml
  seeds:     { count: 5 }
  generate:                       # property-based runs beside the written scenarios
    faults:   { from: catalogue, classes: [generic/power, generic/bmc, generic/leak, nvidia/xid], maxConcurrent: 3 }
    acts:     [power, cable]
    length:   { faults: 8, within: 30m }
    runs:     20
    shrink:   true
  holdout:   { files: [scenarios/holdout/*.yaml], reveal: verdict-only }
  baseline:  { from: last-passed, branch: main }
  budget:    { wall: 6h, parallel: 2 }
```

- **Matrix**: estates × scenarios × seeds, each cell one run with its own judgement. An estate
  restart between cells unless the scenario declares `isolation: none`.
- **Generated runs.** The generator draws a fault and act sequence from the catalogues the estate's
  profiles allow, from the seed. There are no assertions; the invariants and the playbook are the
  oracle. On a violation, **shrinking** re-runs with faults removed and delays shortened until the
  sequence is minimal, and the minimal sequence is written out as a scenario file to keep.
  With a stack installed this costs real time; the campaign's budget bounds it.
- **Flakiness.** A cell that both passes and fails across seeds is `flaky`, reported with both
  judgements diffed. It is the SUT's until a twin incident says otherwise.
- **Aggregate**: verdict counts, per-dimension score distributions, each objective's p50/p95/max
  across cells, coverage across the campaign (which playbook entries, fault classes and
  `sometimes` rules were never exercised), and the comparison with the baseline.
- **Baseline and diff.** `dctwin critic diff <a> <b>` lists what changed between two judgements
  or campaigns: verdicts flipped, new and fixed violations, objectives that moved beyond their
  noise (the spread across seeds), and changes in the SUT's request profile (canonical names it
  started or stopped using, which is also G-29's contract diff).
- **Holdouts.** Scenarios marked holdout report only their verdict and dimension scores to the
  caller, never their steps or witnesses. They exist because a developer, or an agent, iterating
  against a critic it can read in full will fit the critic rather than fix the software.

### The scenario library

Stack-neutral scenarios shipped under `scenarios/library/`, written against the standard aliases
and grouped by capability, so a new stack has something to be judged by on its first day:
`ingest` (discovery, identity, firmware baseline), `lifecycle` (allocate, release, sanitise),
`power` (feed loss, PSU, UPS on battery, breaker), `thermal` (CDU flow, coolant excursion),
`leak` (tray, rack point, isolation), `accelerator` (Xid classes, ECC, off the bus),
`fabric` (cable, switch tray, NVLink domain), `bmc` (unreachable, slow, credentials, reset),
`storage` (drive, RAID, file system full), `compound` (two faults that interact). A scenario whose
aliases a stack does not map is `not-run` for it, with the alias named.

## Interfaces

**CLI.**

```
dctwin scenario run <file> [--policy <file>] [--subject <ref>] [--judgement out.json] [--junit out.xml]
dctwin critic judge  <state dir | events.jsonl> --policy <file>     # offline, a recorded run under a policy
dctwin critic start  --policy <file> [--subject <ref>]              # an open session: judge whatever happens
dctwin critic stop                                                  #   … and print the judgement
dctwin critic explain <judgement> <violation>
dctwin critic diff   <a> <b>
dctwin campaign run  <file> [--out dir]
dctwin policy validate <file> --stack <stack>                       # CEL type-check, aliases mapped, fault ids exist
```

**Control API.** `POST /v1/runs` (start), `GET /v1/runs/{id}` (the judgement so far: violations
appear as they are found), `GET /v1/runs/{id}/events` (the stream, with violations interleaved),
`POST /v1/campaigns`, `GET /v1/judgements`. The same token and isolation as the rest of the
control API ([15](15-security.md)).

**Console.** A Critic screen: the scorecard, the fault timeline with its five moments as a bar per
fault, violations with their witness events highlighted in the event stream, and campaign trends.
It reads the control API and nothing else ([D16](11-decisions.md)).

**CI.** A GitHub Action wrapping `campaign run`: brings the estate up, installs the subject at the
pull request's build, runs, posts the Markdown judgement with the baseline diff, and fails on the
verdict or the policy's gate. JUnit for everything else.

**Agents.** An MCP server over the control API (`dctwin mcp`), so an agent developing the SUT can
use the critic in its own loop: `list_scenarios`, `run_scenario`, `inject_fault`, `get_judgement`,
`explain_violation`, `diff_judgements`. It is a control-plane client and is unreachable from the
SUT's networks like any other ([D10](11-decisions.md)). Holdouts stay hidden from it.

## Verifying the critic

A critic that passes everything is worse than none ([16](16-testing.md)). Its oracle is a SUT with
known defects:

- `cmd/refstack` gains **mutants**, each one seeded defect behind a flag: powers off the
  neighbouring tray, never deletes sessions, retries without back-off, acts on a rack leak point
  as if it were a tray's, allocates a degraded domain, reacts after its deadline, polls ten times
  too often. The test matrix is mutants × the scenario library; **each mutant must produce exactly
  its violation, and the unmutated refstack none**.
- **Re-judgement is deterministic**: judging one recorded log twice gives byte-identical
  judgements.
- **Property**: on the virtual clock with no stack, a generated run with no SUT requests produces
  no safety, restraint or hygiene violation (the critic never blames the SUT for the twin's own
  behaviour).
- The measures' five moments are checked against a scripted SUT with known delays.

## What the critic is not

- **Not a proof.** No violation in the runs made is not no defect. Coverage says what was never
  exercised.
- **Not a judge of what no one stated.** A SUT that does the wrong thing permitted by every policy
  passes. The generic policies are a floor.
- **Not a performance or capacity judge.** No workload runs; `capacity.lost` counts accelerators
  out of service, not jobs or tenants harmed, until the twin has a workload model (G-01).
- **Not a view inside the SUT.** It cannot say *why* the SUT acted, only what it had read before.
- **Not exact with a stack installed.** Runs are repeatable, not bit-exact; timeliness has noise,
  which is why objectives against a baseline use the spread across seeds.

## Build order

| Step | Builds | Exit |
|---|---|---|
| **C1 — Evidence and offline judging** | request `class`; `fault.affected`; `run` events; accessor samples as events; `model.at(seq)`; `kind: Judgement`; `dctwin critic judge`; `never`, `always`, `leadsTo`; the five moments; `generic/safety`; text and JSON | The recorded logs of the existing NICo, Ironic and MAAS harness runs are judged offline and the results read sensibly; refstack's "wrong tray" mutant is caught with its witness |
| **C2 — Policies and scoring** | `kind: Policy`, playbooks, `atMost`, `sometimes`, objectives, the scorecard, the remaining generic policies, `policy validate`, JUnit and Markdown, exit codes, all refstack mutants | Mutants × library is green; a refstack run prints a scorecard |
| **C3 — Foundry** | Foundry's stack definition, principals, alias mapping, playbook, `compat/foundry`; the scenario library's first groups | A Foundry build gets a judgement from the library with no Foundry-specific code in the twin |
| **C4 — Campaigns and CI** | `kind: Campaign`, the matrix, baselines, `critic diff`, flake detection, the GitHub Action | A Foundry pull request is gated by a campaign with a baseline diff |
| **C5 — Search and agents** | the generator and shrinking, holdouts, open sessions, the MCP server, the console's Critic screen, HTML | A generated run finds a refstack mutant no written scenario covers and shrinks it to a scenario file |

C1 needs no SUT at all, only recorded logs, which is why it is first.

## As built

What the first implementation settled or changed against draft 0.1:

- **`affected` is derived, not stored.** A fault's reach is its target device and every device a
  state, fault or observe event touched whose cause chain reaches the fault; the critic computes
  it from the log, so nothing is written back into a committed event. Effects the engine commits
  without a cause (a threshold crossed later by derived physics, a cooling loop's coolant rising
  over minutes) are not attributed to the fault.
- **Collateral is destructive only.** A write the stack makes outside a fault's reach (a session
  opened, a boot override) is response volume, not damage; collateral counts destructive requests
  outside every open fault's reach that no playbook permitted.
- **Exposure by push** is the BMS's: a discrete point's change (`RackLeakDetect`, an isolation
  status) is committed as an `observe` event with `push: true`. Redfish SSE deliveries and SNMP
  traps are not yet logged as pushes, so a stack told only by them is exposed at its first read.
- **Accessor answers** are looked up at or before an event; a `never` rule's condition may also
  take the first answer recorded within 10 s after the request, since the sampler reads the stack
  just after a request it judges.
- **Generic thresholds** come from the reference stack's measured behaviour: its peak is about
  1,000 requests per device per minute and 300 per 10 s, so the ceiling is 600 per 10 s and the
  objective passes at 1,200 per minute.
- **The console** shows the judgement on the Scenarios screen (verdict, scorecard, each fault's
  moments, violations with their witness and the reads before them), not on a screen of its own.
- **`ingest.time`** is measured from the run's start to the stack's first `Ready` for the device's
  serial, from recorded `machine` answers.
- **The scenario library** is `examples/scenarios/library/<group>/`: twelve scenarios in the ten
  groups (`ingest`, `leak` ×3, `power`, `thermal`, `fabric`, `bmc`, `accelerator`, `lifecycle`,
  `storage`, `compound`). They name the standard aliases they read (`stack.aliases`), resolved
  through the installed stack's `critic.aliases`, and say "no destructive request" by request class
  (`event: {class: destructive}`), so none names a stack's calls.
- **The MCP server** speaks JSON-RPC 2.0 over stdio (`dctwin critic mcp`), protocol version
  2025-06-18, tools only.

## Decisions (D64–D70 in 11)

| | Decision | Why | Reversed if |
|---|---|---|---|
| D64 | The critic judges only from the event log, the model and declared accessors | D10 and G1; it is what makes it stack-neutral and re-judgeable offline | a class of defect matters that leaves no trace on any device or accessor |
| D65 | The verdict is rule-based; the score never decides it | A weighted mean can average a safety violation away | scores prove stable enough across seeds to gate on alone |
| D66 | The four results of 08 stand; the critic adds none | One vocabulary for scenario, suite and judgement | — |
| D67 | Generic policies apply by default and are disabled only by name with a reason | A floor that needs no work from the stack | they produce false violations on stacks with legitimately different behaviour |
| D68 | Accessor reads by the critic are committed events | Offline re-judgement and witnesses need them | their volume crowds the log (then: a side log with the same retention) |
| D69 | Timeliness is wall-time and void for baselines under `timeScale ≠ 1` | The SUT is not time-scaled | — |
| D70 | Holdout scenarios reveal verdicts only | A readable critic gets fitted | — |

## Open questions

- **Q20 — Exposure by read.** "The first SUT read of an affected path" counts a read of a resource
  the fault did not change. Tightening it to "a read whose response differed because of the
  fault" needs the adapters to say which model fields a response rendered. Is the looser
  definition good enough for C1?
- **Q21 — Foundry's surface.** Which of the standard aliases Foundry's API can answer, how its
  components are told apart on the wire (addresses, client certificates), and whether it runs
  above NICo or reaches devices itself. Gates C3.
- **Q22 — Attribution under two stacks.** When Foundry asks NICo and NICo powers the tray off, the
  device-side principal is NICo. Linking the two needs an accessor on the upper stack (its request
  log) or trace-context propagation the twin can observe.
- **Q23 — Maintenance.** `generic/safety` exempts declared maintenance. Is it declared by the
  scenario only, or may the SUT declare it through an accessor (and so excuse itself)?
- **Q24 — Cost of generated runs with a stack.** At wall-clock speed 20 generated runs of 30 min
  are 10 h. Parallel estates on one host, or a policy-model ghost (G-13), or neither?

## Prior art

Read on 2026-11-10; each shaped one part of this chapter.

| | What was taken |
|---|---|
| [Jepsen](https://github.com/jepsen-io/jepsen) | Judge a recorded **history** with checkers, separately from the fault injector (its nemesis). Hence offline re-judgement and D68 |
| [Antithesis assertions](https://antithesis.com/docs/concepts/properties_assertions/assertions/) | `always` and `sometimes` as distinct property kinds; an unmet [`sometimes`](https://antithesis.com/docs/concepts/properties_assertions/sometimes_assertions/) means the test did not reach something, not that the software failed. Hence the coverage dimension |
| [Keptn quality gates](https://v1.keptn.sh/docs/1.0.x/reference/files/slo) | Objectives with pass and warn criteria, key objectives that fail the evaluation alone, weights, and criteria relative to previous results. Hence objectives and baselines |
| [LitmusChaos probes](https://docs.litmuschaos.io/docs/concepts/probes) | Continuous checks during a fault, not only at its end, and a weighted resilience score. Hence invariants evaluated for the whole run, and the caution of D65 about the score |
| Property-based testing (QuickCheck, Hypothesis) | Generated sequences with shrinking to a minimal failing case |
| Mutation testing | Verifying the critic with seeded defects |

# 11 — Decisions and open questions

Each decision states why it was made and what would reverse it. A changed decision becomes a new
decision that names the one it supersedes. D19–D32 come from the
[review of 2026-10-18](../reviews/2026-10-18-spec-review.md).

## Decisions

### D1 — The estate file is the only input
Everything that runs is derived from the estate file and the packs it pins. **Why:** a twin
configured by hand drifts from its description, and then no two runs test the same datacenter.
**Reversed if:** never wholesale. Run-time acts (faults, power, cables) go through the control
API and the event log, not into the file.

### D2 — One model, many projections
Adapters hold no device state of their own; every interface renders the same model. **Why:**
consistency across interfaces is the twin's whole value over a set of mocks. **Reversed if:** an
interface is found that cannot be expressed as a projection. In that case it runs as an
*unsynchronised* adapter and the fidelity report prints it. **Narrowed by D19:** state that the
substrate owns is observed into the model, not projected from it.

### D3 — Emulate devices, never the software under test
The twin pretends to be hardware, never a component of the stack. Impersonating a stack's agent
is a labelled scale mode that never counts as a conformance pass. **Why:** a test that replaces
part of the stack tests the replacement. **Reversed if:** never for conformance runs. **Clarified
by D24.**

### D4 — Switches run real network operating systems
BGP, EVPN, LLDP and DHCP relay come from a real NOS in containerlab; SONiC-VS is the default
because it is freely available. **Why:** routing protocols are too subtle to emulate faithfully,
and real images exist. **Reversed if:** a stack needs a NOS no runnable image exists for. Then
that NOS's management API is emulated and labelled (NVOS, [Q11](#q11)). **Narrowed by D21:** a
model switch is a fidelity level below this.

### D5 — Accelerators in booted hosts run the vendor's real management library over a shim
For NVIDIA that is Mokka under the real NVML; AMD and Intel follow the same rule ([Q7](#q7)).
**Why:** the real operator, DCGM-class daemons, device plugins and exporters then run
unmodified, which a fake operator would defeat. **Reversed if:** no shim is achievable for a
vendor. Then that vendor's in-host layer is `model`-only and its stack definition says so.

### D6 — Vendor-neutral core; vendors are packs; data first
The engine knows protocols, not vendors. Hardware arrives as profiles, formats, dialects and
catalogues; code only through named extension points. **Why:** AMD, Intel and every OEM must be
addable without touching the engine. **Reversed if:** never wholesale. A needed extension point
is added to the core as its own change.

### D7 — The core owns its Redfish engine; upstream mocks are oracles
The core serves captured mockups through its own engine, with model-bound dynamic resources and
dialect hooks. NICo's `bmc-mock` and sushy-tools are references and conformance oracles, not the
engine. **Why:**
- *State:* both hold their own state, which D2 forbids.
- *Neutrality:* `bmc-mock` is shaped by one consumer.
- *Coverage:* sushy-tools has no DPU, BIOS or AccountService support, and identifies itself as
  `Sushy`, which NICo treats as a special case.

**Reversed if:** [Q2](#q2) finds that `bmc-mock` or a successor can be driven from external state
and covers the vendors needed. Then wrap it rather than build. **Q2 must close before M0's Redfish
work starts.** Refined by D25.

### D8 — A userspace network stack hosts `model` endpoints
One process presents thousands of BMCs and hosts, each with its own MAC, DHCP lease, ARP and TCP.
**Why:** a kernel network namespace per endpoint does not scale to a hall, and endpoints must
really DHCP through the stack's relay. **Reversed if:** measurements show a kernel-per-endpoint
approach is cheap enough at the target scale. **Superseded in part by D20:** not one TAP for all of
them.

### D9 — Two clocks; anything real forces the wall clock
`virtual` only when every device is `model` and every adapter is model-driven. **Why:** a VM's
boot and a NOS's BGP timers cannot be time-warped. **Superseded by D22**, which adds the stack.

### D10 — The twin's control plane is unreachable from the software under test
Control API, event log, scenario runner and the in-host vsock channel are on no twin network.
**Why:** a stack that can read the twin can pass by reading the answer. **Reversed if:** never.
**Mechanism:** [15](15-security.md).

### D11 — No result without its fidelity report
Every result prints what the run used that the twin does not model, every stub, every
impersonated agent, every unsynchronised adapter, every piece of substrate-owned state, and every
twin incident. **Why:** a pass that hides its conditions is worse than no test. **Reversed if:**
never.

### D12 — Go for the core and in-tree packs
**Why:** it is the language of Kubernetes, containerlab, most bare-metal tooling and most likely
contributors; it handles concurrent protocol servers well, and it builds to a single static
binary. **Reversed if:** a core component has a materially better implementation elsewhere. That
component then runs behind D15's sidecar interface rather than prompting a rewrite.

### D13 — The twin redistributes no vendor binaries or images
Stack definitions pin versions and say where to obtain anything not redistributable (UFM, NOS
images under restrictive licences, closed components). Captured mockups are scrubbed before they
are committed, and every captured or derived asset names its licence in `pack.yaml`
([Q10](#q10)). **Why:** the repository is public and Apache-2.0. **Reversed if:** never; vendor
permission changes what may be obtained, not what is committed.

### D14 — Rack formats and counts are data and parameters
NVL72, NVL36x2, Helios and 8-GPU racks are formats with declared parameter ranges, not code
paths. **Why:** families ship in several layouts (NVIDIA's partition guide lists NVL36x2 with 9
or 18 compute trays per rack), and vendors will add more. **Reversed if:** a format has behaviour
data cannot express. That behaviour becomes a pack hook, and the format stays data.

### D15 — Out-of-tree packs extend through sidecars
In-tree packs are compiled in. Out-of-tree packs implement the same extension points as sidecar
processes over a documented gRPC interface whose contract is batched snapshots per tick plus the
committed-event stream. **Why:** a vendor or user can add hardware without forking or rebuilding
the twin. **Reversed if:** sidecar latency proves too high for a high-frequency projection. That
pack then moves in-tree.

### D16 — The console is a client of the control API and nothing else
Every console action has a CLI and API equivalent; the console holds no logic the API lacks, and
calls only published operations. **Why:** a capability only the console has cannot be scripted,
tested or used in CI, and a console that reaches past the API can disagree with it. **Reversed
if:** never.

### D17 — Structural edits in the console are estate-file changes
Adding, removing or changing racks, devices, fidelity, cabling, fabrics or facility in the
console produces a draft of the estate file, which is validated, diffed and applied, never a
direct change to the running model. Run-time acts (faults, power, cables, clock) are events, and
never touch the file. **Why:** D1. **Reversed if:** never wholesale. A frequently used edit that
is clumsy as a draft gets a shortcut that still produces the same diff.

### D18 — The console draws from layout data; it has no vendor code
Rack elevations, faceplates, fabric graphs and fault forms come from layout fields in formats,
profiles, templates and catalogues ([04](04-devices-and-racks.md#layout)). **Why:** D6 applies to
the console too. **Reversed if:** a device cannot be drawn usefully from data. That device gets a
richer layout field, not a special case in the console.

### D19 — Model fields are owned or observed; observers reconcile the substrate
Every model field is either **owned** (the engine is authoritative and adapters project it) or
**observed** (the substrate is authoritative: a VM's power state and NVRAM, a NOS's port and
routing state, swtpm's keys; an observer reports it into the model as a committed event). The table
is in [03 — State](03-estate-model.md#state). **Why:** D2's one-way flow made Redfish report a
power state a crashed VM did not have, and a boot order an installer had already rewritten.
**Supersedes:** part of D2. **Reversed if:** never; the owned/observed split may move per field as
substrates gain control hooks.

### D20 — Endpoints are hosted per modelled switch port
The userspace stack owns **one TAP per modelled switch port**, veth-wired into the real NOS's
matching interface or bridged into a model switch's segment. Where the switch is a model switch
(`nos: none`), the substrate is one bridge per modelled L2 segment plus a **twin relay** per
segment that inserts the circuit id and remote id the model's cabling implies, and link selection
on request. **Why:** one TAP for every BMC put them all behind one switch port: one circuit id,
`lldpd`'s per-port neighbour cap, no per-port link-down, and no relay at all on a plain bridge.
Netdevs are cheap; what D8 rightly avoids is a kernel stack per endpoint. **Supersedes:** D8's
single TAP. **Reversed if:** measurements show per-port TAPs cannot reach hall scale; then a
model switch would aggregate ports behind one TAP and be labelled as such.

### D21 — Model switches are a fidelity level; fabric-device fidelity derives from `nos`
A fabric with `nos: none` is served by **model switches**: cabling, port state, LLDP, interface
counters and the twin relay, projected from the model, with **no routing protocol**. They provide
no `switch/bgp-*`, `switch/evpn` or NOS management capability, and `dctwin check` says so. A
switch's fidelity is **derived from its fabric's `nos`**: `none` → `model`, a real NOS → `vm`;
an override that disagrees is a validation error. **Why:** scale and the virtual clock need
switches that are not a NOS each, and D4 had no place for them; two sources of truth for switch
fidelity let the estate contradict itself. **Supersedes:** part of D4. **Reversed if:** never.

### D22 — An installed stack forces the wall clock; exact replay is a twin-only run
`clock: virtual` is legal only when every device is `model`, every fabric is `nos: none` and the
estate installs **no stack**. A run with a stack is wall-clock, repeatable and not bit-exact.
**Replay** re-drives recorded inputs (acts, faults and the stack's requests as a ghost) against the
engine under the virtual clock, with no stack, and must reproduce the log; it debugs the twin and
says nothing about the stack. **Why:** the stack runs on wall time; time-warping the devices under
it distorts what it sees, and replaying its requests does not run it. **Supersedes:** D9.
**Reversed if:** never.

### D23 — Two in-host modes for `vm` hosts
A `vm` host runs in one of two labelled modes, chosen per device (`os:` in a fidelity override):
- **`os: twin`** — the twin provisions the guest OS from its own image with the in-host agent
  and the accelerator shim baked in. The stack may still PXE-boot it, but a reinstall by the stack
  ends the mode. Needed for anything in-host: NVSentinel, the GPU Operator, kernel-log faults.
- **`os: stack`** — the stack installs the OS it wants over PXE. There is no agent and no shim in
  the guest. The twin supplies what the hardware would: SMBIOS serial and UUID from the identity,
  PCI stub devices with the profile's vendor and device ids so inventory tools see the accelerators
  (with no driver behind them), NIC MACs, the swtpm. Needed for NICo's own ingestion path.

The fidelity report prints the mode of every `vm` host used. **Why:** nothing in draft 0.1 said how
a twin process gets into an OS the stack installs; it cannot, so the two cases must be named.
**Reversed if:** Q15 finds a supported way to inject the shim into a stack-installed image, which
would merge the modes for that stack.

### D24 — KWOK and vendor agent mocks are impersonation
Any component that registers itself in the stack's own control plane as if it were the stack's
agent or kubelet is an **impersonator**: NICo's `machine-a-tron`, DPF's mock DPU agent, and the
twin's own KWOK projection (it registers Node objects with a credential the twin holds). Enabling
one labels the run `impersonated`; such a run is a scale run and never a conformance pass. Stack
definitions list their impersonators; estates enable them by name. **Why:** D3 without this
let the most convenient projection escape the rule. **Clarifies:** D3. **Reversed if:** never.

### D25 — A protocol engine's scope is the stack's call list; the dialect binding is data
The Redfish engine (and every other protocol engine) implements **what the stacks under test call**,
enumerated per dialect in `requests.yaml` from the stack's source (NICo's `libredfish` trait, about
80 operations), mapped to canonical request names ([14](14-expressions.md#canonical-request-names)),
and verified by a DMTF Interop profile generated from that list. It does not implement the Redfish
schema catalogue. The **binding** of a captured mockup to the model is data in the dialect:
JSON-pointer overlays naming which properties are dynamic and from which model field, and an
action-to-behaviour map; OEM behaviour data cannot express is a `DialectHandler`. **Why:** the
scope listed in draft 0.1 was a BMC firmware stack, and the one thing that makes it tractable (the
binding) was undefined. **Refines:** D7. **Reversed if:** never; the call list grows with each
stack.

### D26 — Identity derivation is versioned and includes the profile
Identities derive from `(seed, path, profile@major, kind, index)` by HKDF-SHA256 as specified in
[13](13-schemas.md#identity-derivation), with per-profile serial grammar and OUI. A profile change
at a path is a new machine; a fidelity or substrate change keeps the identity. TPM endorsement keys
are not derived: swtpm state is persisted per device path. **Why:** D1 and the ledger depend on
identities that are stable and specified; the draft gave a new machine the old serial. **Reversed
if:** never within an apiVersion; a new derivation is a new version.

### D27 — Committed and derived state; event retention and redaction
Requests, behaviour transitions, faults, acts, health changes and observer reports are
**committed events**, logged with sequence, clock and wall time. Sensor readings and computed power
are **derived**: computed on read from seed, clock and committed state, never logged. The log has
a retention policy (by count and by age) with snapshot-based compaction, and request bodies are
redacted of credentials and tokens before commit ([15](15-security.md#secrets)). **Why:** logging
sensors every tick was thousands of events per second, and the prototype already truncated and
broke cause chains. **Reversed if:** never.

### D28 — One expression language and a trigger registry
Every condition, predicate and input mapping in profiles, formats, stacks and scenarios is **CEL**
over the environment in [14](14-expressions.md), type-checked at validation. Named triggers
(`device.acCycle`, `firmware.applied`, …) and canonical request names are registries the core
defines and packs extend. **Why:** the draft had five informal languages. **Reversed if:** CEL
cannot express a needed predicate; then a named predicate is added to the registry, not a second
language.

### D29 — Security baseline from M0
The control API is authenticated from the first release (unix socket and per-estate token locally;
OIDC with three roles for shared twins); secrets live in a secret store and never in the estate
file, the snapshot or the event log; adapters and sidecars run in their own network namespaces with
no forwarding; observers use a one-way path; the install path goes through an artifact mirror; CI
scans for the control address from inside the twin ([15](15-security.md)). **Why:** D10 had no
mechanism and the draft left the API open for six milestones. **Reversed if:** never.

### D30 — One site per estate in v1alpha1
Networks, fabrics and facility are estate-global and an estate has exactly one site. **Why:**
multi-site scoping touches every section and no milestone needs it before M11. **Reversed if:**
M11 (scale-out) or a stack needs it; then `v1beta1` scopes those sections per site.

### D31 — Capability contract semantics
A provider declares `{capability, at: [levels], via}`. A requirement declares `{capability,
scope: estate | each(selector), min, anyOf, when, gate}`. The verdict per requirement is `met`,
`reduced` (met at a lower level than the stack's preferred level, or on some but not all of
`each`), `stub`, `impersonated` or `missing`. Only requirements with `gate: install` refuse the
estate; the rest warn and mark their scenarios `not-run`. The qualifier after `:` is a parameter
whose meaning the capability's registry entry defines. **Why:** the draft's contract was met by
one tray while every other BMC spoke something else, and `dpu/hbn` made the full stack
unvalidatable. **Reversed if:** never; the registry grows.

### D32 — Compatibility policy
`v1alpha1` may change until frozen as `v1beta1`; within an apiVersion fields and enums only grow,
and identity derivation, path grammar and event schema do not change; `dctwin convert` moves files
between apiVersions; snapshots and logs carry their apiVersion; the control API and sidecar
interface are versioned separately and keep one deprecated version. **Closes:** Q12.
**Reversed if:** never.

## Open questions

### Q1
**BlueField.** Can a BFB's kernel and initramfs, extracted with `mlx-mkbfb`, boot under QEMU
aarch64 well enough to run the DPU agent and HBN? No public route is known. *Spike before M6.*

### Q2
**Wrapping `bmc-mock`.** Can NICo's `bmc-mock`, or its successor, take its state from outside?
Its libvirt backend suggests a seam. If it can, D7 is reversed and D25's binding applies to it.
*Must close before M0's Redfish work starts.*

### Q3
**SONiC-VS against NICo's switch expectations.** BGP unnumbered toward DPUs with NICo's ASN
scheme, its EVPN route targets, and a default route into the overlay. Its ISC `dhcrelay` has no
RFC 3527 link selection and its circuit-id format is `hostname:interface`; does NICo's Kea hook
parse that, and is giaddr selection enough for every NICo flow? Is the relay's source-interface
option enabled in containerlab's images? Cumulus VX is no longer downloadable on its own.
*Answered by M2.*

### Q4
**MaxLPS and DSX Flex.** Can they be obtained, under what licence, and do they run against a
shimmed NVML? *Gates their part of M5.*

### Q5
**TPM attestation end to end.** An swtpm endorsement certificate issued by the estate's CA,
uploaded through NICo's `tpm_ca add`, with a PCR policy over OVMF's measurements; swtpm state
persisted per device path so a reseat keeps its EK; scout's probe order `/dev/tpmrm0` then
`/dev/tpm0`. *Answered by M1.*

### Q6
**Scale-out.** How should one estate be partitioned across hosts: VXLAN between segments,
containerlab's multi-host mode, one engine with remote adapter shards? *Before M11.*

### Q7
**AMD and Intel shims.** Do AMD SMI and HLML have upstream mock or injection modes, or must the
twin build library shims as Mokka does for NVML? *Before M8 and M9.*

### Q8
**NMX-C telemetry.** What does the subscription stream carry, and at what rate, so that
health-gated allocation can be tested? NICo's mock returns UNIMPLEMENTED for it. *Before M4's exit.*

### Q9
**Fleet Intelligence backend.** May a stub implement the agent's enrolment and node APIs for
testing, under the agent's licence and NVIDIA's terms? Until answered, the agent runs only in
offline mode.

### Q10
**Provenance and licensing of captured and derived assets.** What may be captured from a vendor's
BMC and published under Apache-2.0? Scrubbing covers identities, but OEM resources and message
registries may carry vendor content; topic dialects derived from DSX Exchange's AsyncAPI schemas
and vendor SNMP MIBs raise the same question. *Before the first captured mockup is committed;
`pack.yaml` `assets` records the answer per asset.*

### Q11
**NVOS.** Is there a runnable NVOS image, or is emulating the NVUE REST API (and now SSH/SFTP for
RMS) the ceiling for switch trays?

### Q12
**Closed by D32.**

### Q13
**DSX Air as an optional backend.** Should an estate be able to delegate its network fabric or its
DPUs to NVIDIA's cloud twin? It would raise fidelity there, at the cost of the twin being
standalone (G8).

### Q14
**Console rendering at hall scale.** Canvas 2D or WebGL for the floor plan, and which graph
layout library for fabric topology, decided by measuring a hall of several thousand racks and a
rail-optimised fabric of several hundred switches. *Before the M0 console.*

### Q15
**Injecting the shim into a stack-installed image.** Is there a supported way to get Mokka and the
in-host agent into an OS that NICo installs (an image hook, a cloud-init stage, a bind-mounted
volume), so that `os: stack` hosts could also serve in-host tests? Does Mokka support runtime
fault injection, or does fault fan-out at `vm` need a driver reload? *Spike before M3.*

### Q16
**Mokka's sysfs for Go consumers.** Mokka's PCI and InfiniBand surfaces are `LD_PRELOAD`
interception, which Go binaries bypass. Does a bind-mounted, model-driven `/sys/class/infiniband`
tree satisfy NVSentinel's NIC health monitor, and do DCGM diagnostics under Mokka return canned
results as assumed? *Answered by M3.*

### Q17
**arm64 hosts for GB-series `vm` trays.** Which arm64 KVM hosts (Grace, Ampere, cloud) run the
substrate, and what does a `vm` GB300 tray cost there? Until measured, GB trays are `model` on x86
and the M4 conformance exit is arm64-only.

### Q18
**NICo's leak policy.** The overview mentions gravity-aware handling of trays below a leak source;
the operations guide's default policy is "shut down all leaking trays" with a threshold for the
isolation request, and the BMS path powers nothing off. Which is current, and is the policy
configurable? *Before M5's scenario is final; the scenario asserts the documented default.*

### Q19
**The first dialect.** Which device on NICo's hardware compatibility list has the smallest OEM
surface in `libredfish` (the generic AMI path, DGX H100 "Viking", or Dell's XE9680 with its job
queue), so that M0 and M1 implement one dialect completely rather than three partially? *Decides
M0's scope and whether `oem-dell` ships at M1.*

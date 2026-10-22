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

### D33 — Every file is read with YAML 1.2 semantics
Estates, packs, scenarios and stacks are decoded with YAML 1.2 rules and then strictly, with
unknown fields refused. **Why:** YAML 1.1 reads the key `on:` as the boolean `true`, which broke
every `clears.on` and every `event.on` written the way this specification writes them, and the
failure looked like an unknown field. **Reversed if:** never.

### D34 — A loopback substrate and a site gateway where there are no TAPs
On a host without TAPs (macOS, an unprivileged CI container) every endpoint is its own loopback
TCP or UDP port, allocated in a stable order, and the software under test reaches a device at its
site address (the address its DHCP lease gave it) or its host name through the **site gateway**,
an HTTP CONNECT proxy that routes only to estate endpoints. DHCP segments are identified by
RFC 3527 link selection, because giaddr cannot be a per-segment address there. The fidelity
report prints the substrate's `isNot` (no L2: no ARP, LLDP frames or MAC learning). `vm` and
`hardware` devices and real NOSes are refused on this substrate with the reason. **Why:** D20's
TAP substrate needs Linux privileges; the protocols a lifecycle manager speaks are all reachable
this way, and the gateway is the honest place to say what is not. **Reversed if:** never; the
TAP substrate (D20) is added beside it for Linux hosts.

### D35 — Dialects bind captured trees obtained at a pinned commit
The vendor dialects serve the captured BMC trees NICo's `libredfish` ships as test mockups (MIT,
obtained by `dctwin assets fetch` from a pinned commit, never committed), bound to the model by
data: identities scrubbed through the identity derivation (equal captured values stay equal; the
bound serial, UUID and BMC MAC become the device's), overlays, component, sensor and leak bindings,
an action map, seeded accounts, boot order, Secure Boot state and firmware versions, and **the
captured answers to writes** (`patch_response.json`: status, headers such as a job `Location`,
body) replayed with the job recorded as a model task. A dialect's factory user is the capture's
admin account. **Closes:** Q2 (the core's engine stands; `bmc-mock` is not wrapped) and Q19 (every
NVIDIA and OEM capture at once; libredfish's integration suite is the completeness test).
**Reversed if:** a vendor's capture cannot be redistributed or obtained; that dialect falls back to
the generated DMTF tree, labelled. *Amended after running NICo, Ironic and libredfish against the
twin:* what a capture records as a moment of the captured machine is bound to the model too: each
Manager's `DateTime` runs on the twin's clock (a `DateTime` PATCH sets it, `bmc.clock`), the
system's `BootProgress` follows power and boot, and the host's DPU resources follow the model's
DPU sub-devices (`dpus`: their serials, and absent where the model has fewer). A captured write
answer that names the request body it answers (`match_request_body_contains`) answers only that
body; another settings PATCH gets the dialect's `settingsJobs` job (iDRAC) or `204`.

### D36 — Behaviour durations scale
`spec.timeScale` multiplies every behaviour duration (power transitions, BMC boot, tasks, thermal
lag), so a wall-clock estate with a stack can run its scenarios in minutes. The fidelity report
prints the scale. **Why:** a 40 s power-on per tray makes a leak scenario take most of an hour at
scale 1 without telling anything more. **Reversed if:** a stack's timing behaviour is the subject
of a test; then the estate runs at 1.

### D37 — Oracle mode for Redfish authentication
`spec.adapters.redfish.auth: optional` accepts unauthenticated requests as the factory account
(so a factory password that must be changed still answers 403). It exists for client test suites
written against an unauthenticated mockup server, such as libredfish's, and the fidelity report
prints it. **Reversed if:** those suites gain credentials. *Amended:* `spec.adapters.redfish.dpus:
capture` is the same kind of oracle switch: a host's captured tree is otherwise bound to the
model's DPUs (D35), so a suite that counts the captured machine's devices (libredfish's
Supermicro test counts the BlueField-2 the capture holds) needs the capture's.

### D38 — The twin hands a stack its expected machines
At `up`, an estate with a stack gets `stack-inputs/expected-machines.json` in its state directory:
each BMC's MAC, serial, factory credentials and rack, as a site hands a lifecycle manager its
expected-machines list. Nothing else of the twin's state is handed over. **Why:** a stack cannot
log in to a BMC whose credentials it was never given, and inventing them in the stack would be
reading the twin. **Reversed if:** never. *Amended by D46:* a switch tray's entry also carries its
NVOS management MACs (`nvosMACs`).

### D39 — A reference stack for protocol-level end-to-end runs
`cmd/refstack` is the twin's own stand-in for software under test: relayed DHCP, Redfish through
the site gateway, MQTT, and the leak policies NICo documents, read through declared accessors. Its
results show the twin is a datacenter protocol-level software can discover, read and act on; they
say nothing about any vendor's stack, and its stack definition's `isNot` says so. **Reversed if:**
never; vendor stacks are installed beside it.

### D40 — The BMS publisher follows the AsyncAPI; consumer divergences are mirrors
The DSX Exchange publisher publishes exactly what the AsyncAPI schemas define and validates every
message against them. NICo's current `dsx-exchange-consumer` reads an older shape (point types
`LeakDetectRack`…, `rackName`/`rackID`, timestamps in seconds); an opt-in `nico-legacy` mirror
publishes that shape for the rack leak points. **Why:** the stack doc said to publish what the
consumer reads; the consumer and the schema disagree, and the schema is the contract both sides
will converge on. **Reversed if:** the consumer moves to the AsyncAPI; the mirror is removed.
*Amended after running nico-api against the twin:* nico-api's own integration
(`crates/bms-dsx-exchange`) reads a third shape — `integration` on every metadata message,
`rackName`/`rackId` on the isolation requests, value `quality` the string `"1"` — and the AsyncAPI
forbids those names on a request's metadata topic (`unevaluatedProperties: false`). So a second
opt-in mirror, `nico-api`, publishes each isolation request's metadata in that shape, retained on a
topic of its own (the request's tagPath plus `/nico-api`, under `BMS/v1/PUB/Metadata/`, the one
prefix nico-api reads), and the BMS takes a request on the value topic nico-api derives from it as
it takes one on the AsyncAPI topic: the same rack, effect, delay and status points. Mirrors are a
list (`bmsConsumerMirror: "nico-legacy,nico-api"`; NICo runs both components on one broker). The
integration's `HeartbeatTimestampIntegration` metadata is not a mirror: the BMS publishes it in the
AsyncAPI's shape, which nico-api's parser accepts as it is. **Reversed if:** each mirror goes when
the component it serves reads the AsyncAPI.

### D41 — LeakDetector state is a Resource.Health value
Tray leak detectors answer `DetectorState` with DMTF `Resource.Health` values (`OK`, `Warning`,
`Critical`), as the captured GB200 tree does; a scenario asserts `Critical`. **Corrects:** the
draft's scenario, which asserted `Leak`, a value no Redfish service returns.

### D42 — `vm` fidelity runs QEMU directly; libvirt is not required
The `vm` adapter starts one QEMU process per `vm` device and controls it over its QMP socket:
start, `quit`, ACPI `system_powerdown` and `system_reset` for power, QMP events and the process's
exit as the observer, the UEFI variable store read from its file for `BootOrder`. It uses `-accel
hvf` on macOS and `kvm` on Linux for a guest of the host's architecture; TCG only with
`--allow-tcg`, printed in the fidelity report. **Why:** libvirt on macOS has no maintained
Hypervisor.framework driver, and the observations 02 and 19 name (running, paused, crashed, off; a
guest-initiated shutdown or reboot; NVRAM) come from QEMU in any case: QMP gives them without a
daemon. **Corrects:** 02, 03, 04, 09 and M1's wording, which say libvirt. **Reversed if:** a
site's hosts are managed through libvirt and the twin must share them; the observer contract is
the same either way.

### D43 — A Node goes NotReady for its host, not for one accelerator; GPU labels are derived
The Kubernetes projection (D24) posts `Ready=False` when the host has no input power, is off or
in transition, has not booted, or is Critical from something other than an accelerator. A failed
accelerator leaves `Ready=True`: its own condition (`GpuHealthy`, `HpuHealthy`) goes `False` and
allocatable drops by one, as a kubelet and a device plugin behave after an Xid. The twin's cordon
of a Critical host is unchanged and stays labelled in the adapter's `isNot`. GPU Feature
Discovery's labels (`nvidia.com/gpu.present`, `.product`, `.count`, `.family`, `.memory`,
`.compute.major/minor`, `.machine`) are derived from the host's NVIDIA accelerator component,
whose `model` is the NVML name dcgm-exporter's `modelName` also reports, so the two agree as NVML
makes them agree on a host. **Why:** with a real kube-controller-manager, `Ready=False` adds the
`not-ready:NoExecute` taint and evicts every pod on the tray after 300 s, including those on its
healthy GPUs; and hand-written product labels disagreed with the exporters' names on three
profiles (found by running KAI Scheduler, AICR and Prometheus against the twin). **Corrects:** the
first implementation, which set `Ready=False` for any Critical host health.

### D44 — A NOS registry; SONiC-VS runs, emulated where it is not native, and SONiC's own components stand in where it cannot
A fabric's `nos` names an entry of the twin's NOS registry (`internal/nos`): what the image is,
which architectures it runs on natively and which under emulation, whether the twin can render its
start-up configuration, what its DHCP relay inserts, and its `isNot`. `none` is a model switch;
`cumulus-vx` is registered but not runnable (its reason is printed). **`sonic-vs`** is SONiC's own
virtual switch, `docker-sonic-vs` as the SONiC project publishes it (pinned by digest), one
container per switch, with SONiC's own `dhcp_relay` container beside it in its network namespace
(pinned by build and sha256: the project publishes it only as a build artifact). The twin renders
what the switch boots from and hands over to its supervisord: a HWSKU whose lane map binds each
cabled port to the container's `eth<n>`, n its place on the front panel, which SONiC names
`Ethernet<first lane>` (lanes counted by speed) and aliases with the model's port name; CONFIG_DB
(`Vlan1000`… with the segment's gateway and `dhcp_servers`, a routed /31 per fabric link,
`BGP_NEIGHBOR`); and, because `docker-sonic-vs` carries no bgpcfgd, FRR's bgpd configuration for
the same neighbours. SONiC's relay renders its own `dhcrelay -a %h:%p` from CONFIG_DB, so a BMC's
circuit id is SONiC's `<hostname>:<alias>`, the same string the twin relay sends for that port
(`<rack>-<switch>:<port>`), and its remote id is the switch's MAC (its management port's). The
image is linux/amd64 only: on an aarch64 Docker host it runs under the host's binary translation
(binfmt_misc: Rosetta in an Apple-silicon Mac's Linux VM, or QEMU user mode), unmodified; `up`
checks that the host can run it, and the fidelity report says the switches are emulated. Only
cabled ports are configured (SONiC-VS needs an interface behind every port it creates), at MTU
1500 like the twin's cables. A host cabled to a SONiC-VS port, an attached server or a peer
switch's fabric port, checksums in software: SAI's virtual switch copies frames between a port and
its host interface with a packet socket, which passes a checksum left to offload on unfilled, and
the switch's kernel drops the frame (Kea's replies did, `bad udp cksum`), so the twin turns transmit
checksum offload off on those veths, as SONiC's own test harness does. **`generic/frr-dhcrelay`**
is FRR, SONiC's routing suite, and ISC dhcrelay, the relay SONiC's dhcp_relay container runs, from
Debian bookworm, in one container per switch, configured by the twin: a bridge per rack segment
named as SONiC names VLANs (`Vlan1000`…) holding the segment's gateway, dhcrelay on it relaying to
the stack's server, numbered eBGP over a /31 per fabric link, connected routes redistributed; it
runs natively on any architecture. A name not in the registry is a validation error; a registered
NOS that cannot run on the host is refused at `up` with its reason. **Why:** D4's default should
be SONiC itself wherever it can run, and an arm64 Docker host does run amd64 containers under
binary translation; what SONiC adds over FRR and dhcrelay is what a stack meets on a management
network: CONFIG_DB, SONiC's CLI, and its relay's per-port circuit id. `generic/frr-dhcrelay` stays
for a host with no amd64 emulation and for an estate whose switch count emulation makes too slow;
its dhcrelay is unpatched, so its circuit id is the VLAN interface, the same for every port of a
segment, and the registry and the fidelity report say so. **Narrows:** D4's default where it cannot
run; D45's "named as the model names the port": a SONiC-VS port is its container's `eth<n>`.
**Amends:** this decision's first form, under which `sonic-vs` was registered but not runnable
("x86_64 only; the twin does not render config_db.json"). **Reversed if:** SONiC publishes
`docker-sonic-vs` for arm64 (it then runs natively), or SONiC publishes its relay image to a
registry (it is then pinned by digest like the switch).

### D45 — The TAP is the switch port; a cable is its carrier
On the TAP substrate (D20) each BMC's management port is one TAP whose file descriptor is a NIC of
the twin's userspace stack (gVisor's netstack) with the model's MAC, and whose network device is
the switch port: a port of the segment's bridge, or moved into the real NOS's network namespace
and named as the model names the port, with the switch port's MAC. There is no veth between them.
A pulled cable, a port forced down by a fault or a BMC without power turns the TAP's carrier off
(TUNSETCARRIER), so the switch sees its port lose its link with its admin state untouched; a fabric
link between real switches is a cable the twin holds the middle of (two veths and a two-port
bridge), downed the same way. Each TAP carries the NIC's receive filter in the kernel (its own MAC
and group addresses, TUNSETTXFILTER). The twin's TAPs, bridges and cable middles live in a network
namespace of their own (`dctwin-<estate>`); hosts outside the twin (the stack's DHCP server, a
probe) are cabled to a real switch's port by `spec.adapters.network.attach`. **Why:** a veth's
admin state is the only way to drop its peer's carrier, and taking the twin's end down makes its
packet socket report `ENETDOWN`, which ends the userspace stack's reader for good; a TAP's carrier
is set without touching either side's admin state. gVisor's reader for a TAP stops on the first
frame not addressed to its NIC, and a bridge floods unknown unicast to every port (it did, the
moment one BMC's cable was pulled and the bridge forgot its MAC): the filter is what NIC hardware does, and
without it a single pulled cable silenced its whole segment. **Supersedes:** D20's "veth-wired into the real
NOS's matching interface". **Reversed if:** a NOS must see a veth (a driver it requires).

### D46 — NVOS leases its own address, and the stack is handed its MAC
An NVLink switch tray's NVOS management ports (its management-role Ethernet ports other than the
BMC's: `mgmt0`) are DHCP clients while the tray is powered on, relayed as a BMC's port is, with
their own circuit id; NVOS runs on the tray's CPU, so its client stops the moment the tray leaves
On and asks for its address again (INIT-REBOOT) when it is On again. NVUE answers at the lease
(`:443` and `nvos-<serial>`, through the site gateway) only while NVOS runs. The model holds one
lease per device, and a switch tray's is its BMC's, so NVOS's lease is committed as a `state`
event on the port's path (field `lease`, from the address it held to the new one) that the
adapters serving NVOS follow. `stack-inputs/expected-machines.json` gives each switch tray's NVOS
management MACs as `nvosMACs`, beside its `bmcMAC`. **Why:** a stack finds a switch's OS by that
MAC alone: NICo will not manage a switch whose expected entry has none (`No NVOS MAC addresses on
expected switch`), waits until a DHCP request from one has created its interface
(`WaitForOsMachineInterface`), and reaches NVUE, NMX-C and power control at that interface's
address (`NVOS MAC or IP not available` without it); a site's inventory lists the MAC as it lists
the BMC's. What NVOS's DHCP client sends is not published: the twin's sends no vendor class, as
NICo's own switch simulator sends none, and the fidelity report says so. **Amends:** D38.
**Reversed if:** the model holds a lease per port; the events become the port's state.

### D47 — A stack's own subnet manager runs against ibsim, fed from the model
For stacks that run a subnet manager or read the fabric with infiniband-diags (05's "InfiniBand:
subnet manager"; 06's vendor-neutral "OpenSM" slot), the `ibsim` adapter renders each InfiniBand
fabric as a topology in ibnetdiscover's format and runs ibsim on it; OpenSM and the diags reach it,
unmodified, through ibsim's preload library in place of `/dev/infiniband`. The topology is a pure
function of the model's structure, named as the `ufm` adapter names it: each switch once, with the
GUID of its first InfiniBand port (UFM REST's system GUID) shared by all its ports; each InfiniBand
port of a host a one-port channel adapter carrying the model's port GUID, with node GUID = port
GUID − 1 because ibsim answers PortGUID = NodeGUID + port number; node descriptions as UFM REST's;
a model GUID longer than 64 bits cut to its leading 64. The fabric manager appliance's HCA comes
first, so a subnet manager attaches where UFM runs its own. The model's link state is applied while
the twin runs through ibsim's console, `Unlink` when either end of a cable is down by
`engine.PortUp` (a pulled cable, a port fault, a host off, a switch without AC) and `Link` when
both are up again, on the event that changed it, so the SM receives the switch's trap 128 and
sweeps as it would. What ibsim then holds (port logical state, LIDs, forwarding tables, the SM's own
state) is the substrate's and the stack's SM's, not the model's (D19); the fidelity report labels
the fabric SIMULATED, with no data plane. Once a subnet manager has assigned LIDs in ibsim, UFM
REST serves those LIDs, read from ibsim (its console `Dump`, at most twice a second, through
package `smstate`, so neither adapter imports the other), as a UFM serves what its embedded
OpenSM assigned; without one it serves LIDs of its own, kept like guid2lid. Its port states stay
the model's. Where no ibsim runs (macOS), the topology is still written to the state directory.
**Why:** UFM REST serves the model's InfiniBand (D25: NICo's call list), but a stack whose fabric
manager is OpenSM drives SMPs and reads what a subnet manager assigned; ibsim answers those MADs
from a topology file, so the model can be that file and need not be reimplemented as a MAD
responder. Running it (`compat/opensm`) found three things a projection of the model could not
show, all since fixed: the `rail` generator built no spine tier, so one SM reached one rail leaf of
20 while UFM REST reported every port Active (the generator now builds `spines`, default 2); the
QM9700 profile's GUIDs were 72 bits (identity/v1 now derives 8 − len(prefix) bytes); and UFM REST's
LIDs were its own (now the SM's, above). **Refines:** 05 (InfiniBand: subnet manager).
**Reversed if:** a stack needs the SM's port states through UFM REST (then the `ufm` adapter reads
them from ibsim as it reads LIDs), or a data plane.

### D48 — A `vm` host's NICs are cabled to the twin's switch ports; the segment gateway routes to the stack
Each NIC of a `vm` host is a QEMU `stream` netdev to a unix socket the twin holds, with the model's
MAC: the twin is the switch port the model cables that NIC to (`netsim.HostPort`), in userspace,
on any host. The guest's DHCP enters the twin relay of the port's segment at that port, so the
relay inserts the circuit id, remote id and link selection the cabling implies and forwards to the
segment's server (the stack's, on a `dhcp: stack` network), and its replies leave the port as a
relay delivers them. The segment gateway is a router with the switch's MAC (gVisor's netstack, as
on the TAP substrate) that routes the guest's traffic to three places and nowhere else: **at L3,
without NAT**, a routed site network the stack's outputs name (`siteNetwork`): a Linux network
namespace whose TAP is the far end of a frame link from the router, where the stack's services
run and see the guest's own lease as its address; an estate endpoint at its site address (the
site gateway's routes); and a stack service on this host at the site address the outputs name
(`siteServices`), through a proxy that terminates the guest's TCP (the service then sees this
host's loopback address). What the guest's DHCP was leased and every connection it opened or was
refused are committed as `observe` events on the port's path. The host's **boot options are its
firmware's**: the observer reads the machine's UEFI variable store whenever the firmware writes
it and commits the load options (`nvram.bootOptions`), which the BMC serves as the system's
BootOptions (display name, UEFI device path, alias, at ids in the captured BMC's style) and
BootOrder in place of the capture's, and the NICs those options name as the system's
EthernetInterfaces (MAC, the interface's device path); a boot order set through the BMC is written
into the store before the machine next starts, a restart with one pending being a cold start. The machine's TPM is manufactured on first use with RSA and ECC EK
certificates and a platform certificate signed by the estate CA (`swtpm_setup`, `swtpm_localca`),
as a vendor provisions one. Two networks relayed to the same stack server share one upstream relay
leg. **Why:** QEMU user-mode networking put the guest on QEMU's own NAT, where no stack's DHCP or
boot service is; on macOS a TAP or a vmnet interface needs root or an entitlement; and running NICo
found that its PXE service identifies a booting host only by the TCP peer address (it ignores
`X-Forwarded-For` by design) and nico-api's `DiscoverMachine` the same way, so a proxy cannot carry
what they key on, while a namespace the twin routes into can (on macOS no native process can see a
guest's own address, so the Ready path runs on Linux). A lifecycle manager picks the host's boot
option by the firmware's own name for it ("UEFI HTTPv4 (MAC:…)", an alias and a device path with
the NIC's MAC), which the capture's options, naming the captured machine's NICs and disks, never
matched, and takes the host's NICs from the system's EthernetInterfaces (NICo waits there for a
zero-DPU host's boot NIC). NICo answers a relay at one fixed port, which two upstream legs could not share.
**Supersedes:** the first `vm` implementation's `user,restrict=on` NICs (D42). **Amends:** D34,
which refused `vm` devices on the loopback substrate. **Reversed if:** the TAP substrate (D20) gives
`vm` hosts TAPs on their segments' bridges, where the gateway can be a real router; the observer
contract stays. *Amended by D52's amendment:* the site gateway routes `siteServices` for stack
processes too, and a switch port's relay serves more than one DHCP client behind it, each by its
own MAC (a BlueField's BMC and its Arm).

### D49 — Container hosts: real kubelets whose GPUs are the model, through Mokka
A twin host's Kubernetes software may run on a **container host**: a real kubelet and containerd
in a container (a Kind node) registered under the host's Node name (`<rack>-<device>`, as the
`kwok` projection and the dcgm-exporter impersonation name it) and labelled
`dc-twin.io/host=container`, with NVIDIA's Mokka (nvml-mock: a mock `libnvidia-ml.so`, driver
tree, CDI specs and NRI injection) staged on it from a GPU profile the twin renders from the model
(`internal/mokka`, `cmd/dctwin-mokka`): the NVML name, count, identity/v1 UUIDs and serials, the
twin's PCI bus numbering, memory, VBIOS, the NVL domain's cluster UUID and clique and the tray's
rack placement, written over Mokka's own profile of the same silicon, which supplies what the model
has no source for. While the twin runs, each node's Mokka override document (Mokka's one runtime
mechanism, re-read by every loaded library on a one-second TTL) is rendered whole from committed
model state: an accelerator fault whose catalogue declares a `shim: {driver: nvidia/mokka}` effect
becomes that GPU's failure block (`xid-79` → `fallen_off_bus` with Xid 79 on the NVML event set),
and clearing it empties the block. A container host is not a fidelity level of the estate: the
device stays `model` in the twin (whose fidelity report still lists the shim effect as recorded
only), and the container host is software run beside it. Real: the kubelet, containerd, the
device plugin, GFD, NFD, the DRA driver, dcgm-exporter, `nvidia-smi` and the pods. The twin's: the
GPUs' identity and fault state. Neither: the OS, kernel, PCI bus, NUMA, architecture and power
(the Docker host's) and every GPU reading the model has no source for (Mokka's). Not carried:
power and AC state, port, thermal and NVLink-lane faults, kernel-log lines (a Kind node's ring
buffer is the Docker host's, shared by every node). Mokka makes its driver tree's character devices
with `mknod`, which the kernel refuses in every user namespace but the first, so a container host
needs a rootful container engine; where the host's is rootless, the cluster runs inside a KVM
guest (`compat/gpu-operator/guest.sh`). **Why:** the Kubernetes-enablement and
Metrics slots (06) need the vendor's real device plugin, GFD, DRA driver and exporter on a kubelet;
the `kwok` projection impersonates the kubelet and runs none of them, and `vm` hosts with kubelets
are M3's. A container host runs them now, cheaply, against the twin's GPUs, and shows where the
twin's projections and the real software disagree (`compat/gpu-operator`). **Amends:** 10's M3
row (Mokka also runs outside `os: twin` guests). **Reversed if:** `vm` hosts run kubelets with the
in-host agent's Mokka; container hosts then remain for runs that need many nodes.

### D50 — The in-host agent speaks on the serial console; a fault's kernel line is printk'd by its kernel module
The in-host agent (`internal/inhost`, [05](05-interfaces.md#inside-a-booted-host-vm-fidelity)) reaches
an `os: twin` guest through the guest's **serial console**, in lines marked `@@dc-twin`, and
through nothing else. The guest sends nothing until the twin attaches, and the twin attaches only
once the guest's OS is up (bytes on the console while the firmware owns it are keystrokes to the
firmware); a kernel record travels base64-encoded, because the vm adapter's boot observer reads the
same console for the kernel's banner and the guest's ready line. A fault's `kernelLog` line is
**printk'd by the agent's kernel module**, `dctwin-kmsg` (`internal/inhost/kmod`): the agent writes
`<level>line` to the module's `/proc/dctwin-kmsg`, and the record the kernel keeps is a kernel
record, facility `kern`, at the catalogue entry's `level` (default 4, `KERN_WARNING`, at which the
NVIDIA driver prints its Xids), with the text whole. The module is built for the guest image's own
kernel (`kmod/build.sh`, in a throwaway container) and goes into the guest's initramfs beside the
agent, which loads it at boot. Every kernel record comes back out as `/dev/kmsg` gives it, kept
once per boot by sequence number as journald keeps its kernel-seqnum: to `kmsg.log`, and, mapped as
journald's `dev_kmsg_record` maps a record, to `journal.export` in the journal export format that
`systemd-journal-remote` writes into a journal file, so a fault's line is a `_TRANSPORT=kernel`,
`SYSLOG_FACILITY=0`, `SYSLOG_IDENTIFIER=kernel` entry with the line as its message, as a driver's
is. Where the image carries no module, or its kernel refuses it, the agent says so when it attaches
(`kmod=absent|refused`) and writes the line to the guest's `/dev/kmsg` instead; the kernel marks such
a record facility `user`, and journald would then take a driver line's `NVRM:` for a syslog
identifier and strip it from the message, so that entry is entered as a driver's printk is and
marked `DCTWIN_FAULT`, `DCTWIN_FAULT_ID` and `DCTWIN_KMSG_FACILITY`, and the fidelity report names
the path. Alpine's mirror keeps only a branch's newest `linux-virt-dev`, so the pinned netboot
image's kernel has no headers of its own: the module is built against the branch's current headers
carrying the guest kernel's release, and kept only if the kernel would load it — its `vermagic`
equal to the image's own modules', and the version (CRC) of every kernel symbol it uses equal to the
one those modules were linked against. A second guest a compat harness boots beside the twin (a
Debian guest with Mokka's NVML, where a glibc agent runs) builds the same source against its own
kernel's headers (`kmod/guest-load.sh`), and the harness's relay enters the twin guest's NVRM
records into that kernel through it. With the stack's kubeconfig the agent also registers the host's
Node and keeps its Lease, as the kwok projection does for `model` hosts (an impersonated kubelet,
D24): Ready while the agent of the running boot is alive and the machine is on, the guest's own boot
ID, kernel, CPUs and memory, never cordoned or tainted by the twin.
**Why:** QEMU on macOS has no vsock device (vhost-vsock is Linux's), and the console is a channel no
stack network carries (D10); NVSentinel's syslog-health-monitor reads only `_TRANSPORT=kernel`
journal entries and its remediation completes on a changed boot ID, so a kernel log forged outside
the guest, or a Node whose boot ID the twin invents, would test neither (found by running NVSentinel
against the twin, `compat/nvsentinel`). A record userspace writes is told apart from a driver's by
its facility, which no userspace write can set; a kernel module's printk is the only way to give a
twin-rendered line the facility a driver's has, and DCGM's host engine reads Xids from `/dev/kmsg`
itself, so the record has to be in the kernel that DCGM's process shares. **Corrects:** 05's
"virtio-vsock only" for the in-host agent; this decision's first form, which wrote through
`/dev/kmsg` and marked every fault entry, now the labelled fallback. **Reversed if:** the substrate
gives every guest vsock or virtio-serial (the channel moves, the protocol stays), or the twin's
guest gets journald (the mapping goes) or the NVIDIA driver itself (the module goes).

### D51 — BlueField BMCs are fronted by the `bluefield` adapter until the engine has dialect handlers
What a BlueField BMC (OpenBMC's bmcweb) does beyond the generic Redfish service is served by the
`bluefield` adapter in front of the engine's service for each `nvidia/bluefield` BMC: it takes the
BMC's site routes over, answers what a BlueField BMC owns and forwards everything else. It owns
certificates and mutual TLS (a Manager Truststore of ten slots; `CertificateService.GenerateCSR`
from a key the BMC generates and holds in memory, lost when the BMC reboots; `ReplaceCertificate`
checked against that key and then served; client certificates verified against the Truststore and
mapped to an account by CommonName), `ResetToDefaults` (Truststore, certificate, OEM settings and the
factory account's password reset, `PasswordChangeRequired` until changed, a BMC reboot), the BFB
push (`SimpleUpdate` to `DPU_OS`: the image fetched, its boot stream read image by image with each
CRC checked, the transfer as task progress, the Arm reset into the stream), BMC firmware pushed to
the HttpPushUri as a DSP0267 PLDM package (staged by the engine under the version its BMC image
carries), the rshim and mode OEM writes, and bmcweb's 200 with a Base Success message where the
engine answers a write with 204. What changes the model goes through the engine (firmware versions,
the account's password, the BMC's reboot, the Arm's reset); the Truststore, the installed
certificate and its key, the update tasks and the rshim and mode settings are the front's own
state, an unsynchronised adapter in D2's sense, which its fidelity report says. The canonical names
it logs are in [14](14-expressions.md#canonical-request-names)'s registry:
`certificate.read`, `certificate.install`, `certificate.delete`, `certificate.csr`,
`certificate.replace`, `firmware.update.push`, `dpu.rshim.set`, `dpu.mode.set`. A pushed BMC image
is staged `applyOn: reset` and runs when the BMC reboots, which DPF requests once the update task
completes (the engine applies a staged BMC component at the BMC's reboot, not only at a reset of
the host). **Why:** DPF in zero-trust
mode does all of this to every DPU before it provisions it, and the engine has no hook through which
a dialect answers a request or chooses a BMC's TLS identity (D25 names a `DialectHandler`; it is not
built): the engine's 204s, its inert `ResetToDefaults` and its one certificate per BMC stopped DPF at
its first BMC write (found by running DPF against the twin, `compat/dpf`). **Refines:** D25.
**Reversed if:** the engine gains a per-dialect handler consulted before dispatch and a per-BMC TLS
configuration callback; the front's handlers move behind them unchanged, and the route takeover
and the forwarding hop go.

### D52 — A BlueField at `vm` has an Arm the BMC's BFB push installs
A BlueField DPU at `vm` fidelity keeps its model BMC (fronted, D51) and gets an Arm: a QEMU aarch64
machine (edk2, an NVMe disk) that the `bluefield` adapter runs as the DPU's substrate (power
observed from it, D19). It runs NVIDIA's software and nothing the twin wrote: with nothing
installed, its UEFI firmware; after a BFB push (SimpleUpdate to `DPU_OS`, the stream saved as the
BMC receives it), the stream's own installer, booted from the stream's Linux image, initramfs and
boot arguments, with the bf.cfg that followed the last image placed as `/etc/bf.cfg`; after the
installer, GRUB's default entry read from the installed disk at every reset, so what the DPU OS
writes to its boot configuration takes effect at its next reset. The installer's ARM capsule (ATF
and UEFI, which it writes to a BlueField's boot partitions and on the Arm reports passed although
the write fails for want of an eMMC) is modelled as firmware: when the installer reports it, the
stream's `info` versions (`BF3_UEFI`, `BF3_ATF`, `BF3_BSP`) are staged on the DPU's `uefi`, `atf`
and `bsp` components and applied at the Arm's next reset, so the BMC's `DPU_UEFI`, `DPU_ATF` and
`DPU_BSP` report the BFB's, which DPF checks before it puts a DPU in service. Nothing is flashed:
the Arm keeps QEMU's edk2. The DPU OS runs in the profile's cost (4 GB); the installer is given 8
GB, the least a 64K-page BFB's installer unpacks its initramfs with (Q1), backed by a file in the
Arm's state directory (QEMU's `memory-backend-file`, shared, removed when the installer ends), so
that the host writes the guest's pages back to disk rather than holding them: anonymous, the 8 GB
guest, QEMU's own copy of the 1.6 GB initramfs and the translation cache exceed a 9G memory cap, and
file-backed the installer finished with QEMU at 5.5 GiB resident. Under TCG the translation cache is
capped at 256 MiB, which QEMU would otherwise grow to 1 GiB. The twin supplies, and the fidelity
report names, what QEMU's `virt` machine lacks of a BlueField and the boot cannot do without: a UART
at the early console (`earlycon=pl011,0x9000000` ahead of GRUB's command line, which follows
verbatim; Linux takes the first earlycon, so `/proc/cmdline` still carries every parameter the DPU
OS set); the boot FIFO at installation (a cpio archive appended to the initramfs); the out-of-band
port's name (a systemd link file matching the NIC's MAC, written into the installed root once,
clean, after the installer); and the out-of-band address (answered on the Arm's cable with a fixed
host .9 in its BMC's segment, because the twin relay serves one client per switch port and a
BlueField's OOB port has two). The Arm's NIC is cabled to the BMC's switch port through
`netsim.HostPort`, so it reaches exactly what D48's guests reach: the stack's site services and the
estate's endpoints. **Why:** DPF's provisioning ends where the DPU OS begins (its agent, the DPU
cluster join, HBN); Q1 showed the BFB installs and boots under QEMU once the early console is given
a UART, and that DPF's first boot needs only a network to run. Run with DPF (`compat/dpf`,
`DPU=vm`), DPF's own DPU agent reports from the Arm and stops at its first operation that needs the
ConnectX; told by its flavor to skip those, the DPU joins its cluster and DPF puts it in service
(Q1). **Refines:** D25, D48, D51. **Reversed if:** the twin relay serves two clients behind one
switch port (the address stand-in goes), or a machine model with a BlueField's UART map, rshim and
ConnectX ports exists. *Amended (2026-10-20): the address stand-in is gone, and the two other
things the first form left to the `bluefield` adapter are the twin's.* **The twin relay serves
several clients behind one switch port:** it keeps each port's clients by hardware address and
delivers a server's reply by the circuit id the server echoed and then, within the port, by
chaddr, as a switch delivers a frame out of the port it learned the MAC on (a port with one client
still takes every reply for its circuit, as before). The Arm's NIC is plugged into its BMC's
switch port by its own MAC (`netsim.Net.PlugRelay(ref, mac)`), a second client behind the port,
so its DHCP is the network's: on a `dhcp: twin` network the twin site DHCP leases it from the
segment's dynamic range (from .200: the port's cabled address stays the BMC's whichever of the two
asks first), on a `dhcp: stack` network the stack's server answers it (where the first form refused
the Arm), and the BMC keeps its lease and its renewals. The Arm's lease is observed (`oob.lease`,
not `lease`, which is its BMC's field on the same device path), and the dhcp adapter's report
counts each shared port's replies by client. **The site gateway routes the stack's site
services:** up gives the substrate the `siteServices` output, and the site gateway routes a stack
process's TCP to a service at its site address, as a guest's segment gateway does, whichever
adapters run; the `bluefield` adapter no longer adds those routes. **A `vm` device is run by one
substrate:** up claims each `vm` device for one enabled substrate adapter that accepts it, a
specialised one (`bluefield`, which runs only BlueFields' Arms) before the general `vm` adapter,
in `--adapters` order among equals, and each plans only the devices claimed for it, so
`--adapters …,vm,bluefield` runs a BlueField's Arm once (the `vm` adapter had planned every `vm`
device, and the engine kept whichever substrate attached last). **Why:** the stand-in gave the Arm
an address no site DHCP chose and refused `dhcp: stack` networks, stack processes reached the
stack's services at their site addresses only when an Arm ran, and a BlueField at `vm` could not
run beside `vm` hosts. **Amends:** D48 (the site gateway, not only a guest's segment gateway,
routes `siteServices`).

### D53 — A firmware update task lives in its BMC and is lost when the BMC goes down
An update's task (`firmware.update.simple`, `.multipart`, `.push`, and the RMS updates that start
the same tasks) runs only while its BMC is up. When the BMC goes down while the task is running —
its reset (`Manager.Reset`, `ResetToDefaults`, the `generic/bmc/reset` fault), the loss of its
standby power (an AC cycle, a reseat, a feed, outlet or isolation that removes the device's input),
or a hang (`generic/bmc/unreachable`) — the task ends there: `TaskState` Exception, its end time the
moment the BMC went down, its percentage where it stood, the Redfish message `TaskAborted`, and a
`task.<id>` state event in the log. Nothing is staged and nothing is applied; once the BMC answers
again the task reads Exception, never Running and never Completed. A BMC that is down refuses an
update outright (`ErrBMCDown`, as it refuses a power request). An update whose task **completed**
before the BMC went down is unaffected: its image is staged, and a BMC component staged
`applyOn: reset` is applied when the BMC reboots (D51), so DPF's order — the task completes, DPF
resets the BMC — still lands, and a second push DPF makes in the second before the reset takes the
BMC down (it reads the version again within about 100 ms and finds the old one, `compat/dpf`) is
accepted, then lost with the BMC, while the first push's image is applied at the reboot. **Why:**
the engine completed the task on its own timer whatever happened to the BMC meanwhile, so an
update survived a reset, a power loss or a hang that a real BMC's would not, and a stack's recovery
from a lost update (polling a task that never completes, re-pushing after a reset) went untested.
DMTF's TaskState offers Exception ("stopped due to an exception condition"), Interrupted ("expected
to restart") and Cancelled ("by an operator or internal process"); the task does not restart, so
it is Exception, with the TaskEvent registry's `TaskAborted`. bmcweb, which holds its tasks in
memory, answers 404 for the task after its reboot instead; the twin keeps the task so that its
numbering stays stable and a client keyed on its id (RMS's jobs, which treat a missing task as
still running) sees it end — a client polling it sees a failed update either way. **Amends:** D51
(the BMC's reboot applies what a completed task staged, not what a running one would have).
**Reversed if:** a dialect's behaviour is captured that resumes an interrupted update after the
BMC's reboot (then that dialect's tasks go Interrupted and resume), or a stack is found that
behaves differently on a 404 from a rebooted bmcweb than on an Exception (then bmcweb dialects
forget the task, with task ids that do not collide).

## Open questions

### Q1
**BlueField.** Can a BFB's kernel and initramfs, extracted with `mlx-mkbfb`, boot under QEMU
aarch64 well enough to run the DPU agent and HBN? *Spike before M6.* **Partly answered (2026-10-19
and 20, `compat/dpf/q1.sh` and `compat/dpf` with `DPU=vm`):** the BFBs are public
(`content.mellanox.com/BlueField/BFBs`, NVIDIA's licence, never committed: D13); DPF's HBN guide
installs `bf-bundle-3.5.0-89_26.07_ubuntu-24.04_64k_prod.bfb` (1,677,035,416 bytes), and a
4K-page build of the same bundle exists. Each is a boot stream of 49 images: boot loaders, a UEFI
capsule, boot arguments, an `info` inventory, the Linux `Image` (LZMA, Ubuntu
`6.8.0-1030-bluefield`) and a 1.6 GB installer initramfs holding the DPU OS as a 1.5 GB
`image.tar.xz`. Found:
- **The kernel boots on QEMU's `virt` machine.** The 4K-page kernel under HVF on Apple silicon
  reaches its root mount in 0.4 s; the 64K-page kernel (the guide's) prints nothing under HVF,
  because Apple silicon has no 64 KB translation granule, and boots under TCG (to the root mount in
  about 2 s of guest time, behind edk2 for aarch64 on QEMU 10.2 only with `-cpu neoverse-n1` or
  `cortex-a57`: with `-cpu max` edk2 prints nothing past its banner).
- **The BFB's own installer runs to completion** (2026-10-20, the guide's 64K BFB under TCG on
  x86_64, 4 vCPUs, 8 GB, edk2, 777 s; earlier the 4K BFB under HVF as far as the host's disk
  allowed). It needs 8 GB of guest memory (it unpacks its 1.6 GB initramfs into a RAM file system of
  half the memory; 4–6 GB fail for the 64K kernel, 4 GB for the 4K one). Every BlueField probe fails
  (no IPMB on `i2c-1`, no ConnectX/MST device, no eMMC, no `RshimMacAddr` EFI variable, no boot FIFO
  and therefore no bf.cfg) and it carries on: it partitions the NVMe disk, extracts the DPU OS
  (540 s of the 777), rebuilds its initramfs, installs GRUB with shim for arm64-efi, writes the
  `ubuntu0` UEFI boot entry, then reports its firmware steps passed without hardware to apply them
  to (the ATF/UEFI capsule fails on the missing eMMC and is reported as passed; the BMC components
  are skipped for want of credentials; the NIC firmware is "the same as provided") and reboots.
- **The installed DPU OS does not boot on `virt` unmodified.** edk2 starts the installer's
  `ubuntu0` entry, shim, GRUB and its two-second countdown, and the kernel's EFI stub exits boot
  services; then nothing more, with one vCPU busy. GRUB's command line carries the BFB's
  `earlycon=pl011,0x01000000` (the installer rewrites the address to BlueField-3's `0x13010000`
  only when it finds BlueField-3's ACPI table), which on `virt` is inside the firmware flash. QEMU's
  pflash trace shows the kernel writing `[` and a space to it: the flash takes 0x20 for the first
  cycle of a CFI block erase and answers every later read with its status, `0xA0`, whose bit 5 is the
  PL011's "transmit FIFO full", so the kernel waits on the second character of its first line
  (39 million reads a minute). With `virt`'s UART address the same kernel prints at once.
  With that one argument changed (`q1.sh`'s `BOOT_EARLYCON`, booting GRUB's kernel, initramfs and
  command line directly) **the DPU OS boots**: Ubuntu 24.04.4 to a login prompt in 111 s, the BFB's
  default cloud-init finished by 361 s (it enables containerd and kubelet and imports the bundled
  container images); `mlx_ipmid`, `systemd-networkd-wait-online` and SPDK's `vhost` fail, and the
  only network device is QEMU's: no `oob_net0`, `tmfifo_net0` or ConnectX ports.
  Given **DPF's bf.cfg** (the one DPF rendered for a DPU at PrepareBFB, handed to the installer as
  `/etc/bf.cfg` in a second initramfs archive where the boot FIFO would put it), the installer runs
  DPF's hooks (`bfb_modify_os` writes DPF's cloud-init; its BMC user and BMC firmware steps fail for
  want of IPMI and the out-of-band port) and the DPU OS boots as DPF's named DPU and runs DPF's
  first boot: DPF's CA trusted, then `apt-get install dpu-agent` from the bfb-registry's `/deb`,
  refused 60 times because the registry address DPF wrote is the harness host's loopback, and
  `cloud-final` fails. That is as far as a DPU OS gets without the twin's help.
- **What a `vm` DPU would need from the twin** for that OS to become a DPU that DPF can provision
  and HBN can run on, in the order the runs above meet them: a UART where GRUB's `earlycon` points
  (or a BlueField-3 ACPI table, so that the installer writes 0x13010000, and a UART there); the
  BlueField boot FIFO (`MLNXBF04:00/bootfifo`), through which the BMC's rshim push hands the
  installer the rest of the stream and the bf.cfg DPF appends (`q1.sh` stands in for it with a
  second initramfs archive) — the twin's BMC already receives and checks that stream (D51) and would
  hand it over; `oob_net0`, the out-of-band port the `mlxbf_gige` driver names, on the management
  network, which DPF's netplan brings up by DHCP and through which the DPU reaches the bfb-registry,
  the host cluster's API and the BMC (the installer's BMC firmware flow needs it too); the
  bfb-registry's `/deb` repository with DPF's arm64 `dpu-agent` package, which DPF builds from its
  source; IPMI towards the BMC (`mlx_ipmid`, the installer's BMC user); rshim's `tmfifo_net0`
  between the host and the DPU (DPF's agent syncs its clock over it); and the ConnectX ports
  `p0`/`p1` with their representors and SFs (OVS-DOCA, HBN's uplinks), which have no QEMU device.
- **As a `vm` DPU with DPF** (D52, `compat/dpf` `DPU=vm`, 2026-10-20, the whole run in a 9G
  memory cap): the twin now supplies the first four of those (the UART, the boot FIFO's bf.cfg,
  `oob_net0` with an address on the management network, and the `/deb` repository with DPF's
  `dpu-agent` built from its source). The BMC's push installs the BFB on the Arm with DPF's bf.cfg,
  the DPU OS boots as DPF's named DPU, DPF's first boot installs the agent from the registry at its
  site address, and **DPF's DPU agent reports to DPF** (`lastStartupTime`; DPF moves the DPU from
  OS Installing to DPU Config). The agent then runs every operation that needs only an operating
  system and a network (13 conditions True, among them the kernel command line written to GRUB and
  containerd configured) and stops at the first that needs the ConnectX: `nvconfig` finds no
  physical ports through devlink (`no physical ports from devlink (p0/p1)`) and retries for good;
  DPF waits for the reboot method the agent reports after it. So with the guide's flavor the DPU
  does not join the DPU cluster. With a flavor that tells the agent to skip the ConnectX's steps
  (`AGENT_SKIP=connectx`: a systemd drop-in among the flavor's `configFiles` adding the agent's own
  skip flags for the NVConfig, the reboot, the kernel parameters, SFs, VF MACs and the OVS
  script), the agent starts its kubelet with DPF's `kubeadm join`, **the DPU joins the DPU cluster
  as a Ready Node** (kubelet v1.34.11 against the 1.37.0 API server), and once the twin modelled
  the installer's ARM capsule (D52), whose versions DPF checks through the BMC, **DPF puts the DPU
  in service, phase Ready**, 403 s after the Arm's reset into its OS. HBN is not deployed: DPF's
  DPUService controller and Argo CD are not run, and its pod could not be placed anyway, its
  DPUServiceTemplate asking 6Gi of memory (the Node allocates 3.3 GiB) and four
  `nvidia.com/bf_sf`, scalable functions of the ConnectX. Its chart and image are fetched from NGC
  without an account. Memory: the installer needs its 8 GB guest file-backed to fit the cap
  (anonymous it is killed at the cap); the DPU OS at 4 GB beside the twin, both clusters and DPF's
  controllers peaks at about 6 GiB of the 9. What stays open is the ConnectX: `p0`/`p1`, their
  representors and SFs, which have no QEMU device.

### Q2
**Wrapping `bmc-mock`.** Can NICo's `bmc-mock`, or its successor, take its state from outside?
Its libvirt backend suggests a seam. If it can, D7 is reversed and D25's binding applies to it.
*Closed by D35: the core's engine binds captured trees; `bmc-mock` is not wrapped.*

### Q3
**SONiC-VS against NICo's switch expectations.** BGP unnumbered toward DPUs with NICo's ASN
scheme, its EVPN route targets, and a default route into the overlay. Its ISC `dhcrelay` has no
RFC 3527 link selection and its circuit-id format is `hostname:interface`; does NICo's Kea hook
parse that, and is giaddr selection enough for every NICo flow? Is the relay's source-interface
option enabled in containerlab's images? Cumulus VX is no longer downloadable on its own.
*Answered by M2.*

### Q4
**MaxLPS and DSX Flex.** Can they be obtained, under what licence, and do they run against a
shimmed NVML? *Gates their part of M5.* *Partly answered (2026-10-19):* not obtainable. MaxLPS's
documentation requires NVOnline access and its software, Dynamic Power Software, is a developer
preview for authorised collaborations; DSX Flex is a set of specifications, not software. Open
until NVIDIA publishes them.

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
M0's scope and whether `oem-dell` ships at M1.* *Closed by D35: every capture libredfish ships is
bound at once, and libredfish's own integration suite is the completeness test.*

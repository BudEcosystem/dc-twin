# 13 — Schemas

The normative shape of every file the twin reads or writes. Earlier documents show examples; this
one says what is required, what is optional, what the enums are, and what each field means. A
machine-readable copy (`schemas/*.v1alpha1.json`, JSON Schema) is generated from this document at
M0 and validated against `examples/` in CI ([16](16-testing.md)). Where this document and an
example disagree, this document wins and the example is a bug.

Conventions: **R** required, **O** optional, **D** default. Field paths are dotted. Lists marked
`[]`. Enumerations are closed unless the field says it is a **registry**, in which case packs
may add values ([04 — Open registries](04-devices-and-racks.md#open-registries)).

## Estate

`apiVersion: dc-twin/v1alpha1`, `kind: Estate`.

| Field | | Type | Meaning |
|---|---|---|---|
| `metadata.name` | R | name | The estate's name; also the default file name |
| `spec.seed` | R | integer | Root of every derived identity and of fault randomness ([D26](11-decisions.md#d26--identity-derivation-is-versioned-and-includes-the-profile)) |
| `spec.clock` | O | `wall` \| `virtual`, D `wall` | `virtual` is refused if any device is `vm` or `hardware`, any fabric has a real `nos`, or any stack is installed ([D22](11-decisions.md#d22--an-installed-stack-forces-the-wall-clock-exact-replay-is-a-twin-only-run)) |
| `spec.packs[]` | R | `name@version` | Pinned packs. Resolution and the lockfile: [Names, pins and the lockfile](#names-pins-and-the-lockfile) |
| `spec.fidelity.default` | O | level, D `model` | `model` \| `vm` \| `hardware` |
| `spec.fidelity.overrides[]` | O | `{select, level, os?}` | Evaluated in order; **the last match wins**. `os` is `twin` \| `stack` for `vm` hosts ([D23](11-decisions.md#d23--two-in-host-modes-for-vm-hosts)), D `twin` |
| `spec.initialState` | O | `on` \| `off`, D `on` | Power state of every device at `dctwin up`. Devices added by `apply` always start `off` with the BMC on standby |
| `spec.sites[]` | R | exactly one in v1alpha1 ([D30](11-decisions.md#d30--one-site-per-estate-in-v1alpha1)) | |
| `sites[].name` | R | name | |
| `sites[].halls[]` | R | | |
| `halls[].name` | R | name | |
| `halls[].layout` | O | `{rowOrientation: ew\|ns, aisles: [...]}` | Floor-plan hints for the console; no effect on behaviour |
| `halls[].rows[]` | R | | |
| `rows[].name` | R | name | Not a path segment; rack names are unique per hall |
| `rows[].positions` | R | integer ≥ 1 | Number of rack positions in the row; empty positions are legal |
| `rows[].racks[]` | O | | |
| `racks[].name` | R | name | Unique per hall |
| `racks[].position` | R | 1..`positions` | |
| `racks[].format` | R | `pack/format` | |
| `racks[].params` | O | map | Values for the format's declared parameters; unset ones take the format's defaults |
| `racks[].feeds` | O | `[feedName]` | Binds the format's declared power inputs to estate feeds in order; D the first inputs' worth of `facility.power.feeds` |
| `racks[].slots` | O | map `slotName → {profile?, params?, empty?: true}` | Per-slot overrides of the format: a different profile that the slot `accepts`, or an empty slot |
| `spec.cabling` | O | `{add: [{a, b, kind}], remove: [portRef]}` | Hand-written cabling that amends or replaces what fabric templates generate; validated like generated links |
| `spec.networks` | R | map `name → network` | |
| `network.role` | R | `management` \| `provisioning` \| `frontend` \| `scale-out` \| `storage` \| `service` | Profiles bind ports to roles, never to network names ([R-37](../reviews/2026-10-18-spec-review.md)). A role maps to at most one network per rack |
| `network.cidr` | O | CIDR | Required when `dhcp: twin` or when the twin allocates static addresses on it |
| `network.dhcp` | O | `stack` \| `twin` \| `none`, D `stack` | Who serves DHCP on it. `stack`: the software under test, through the relays the fabric provides ([05](05-interfaces.md#boot-and-addressing)) |
| `network.fabric` | R for every network a NOS or model switch serves | fabric name | The fabric that carries it and relays it |
| `spec.fabrics[]` | R | | |
| `fabric.name` | R | name | |
| `fabric.template` | R | `clos-2tier` \| `clos-3tier` \| `rail-optimised` \| `oob-flat` \| `pack/template` (registry) | |
| `fabric.nos` | R | `none` \| `sonic-vs` \| `cumulus-vx` \| `pack/nos` (registry) | `none` is a model switch ([D21](11-decisions.md#d21--model-switches-are-a-fidelity-level-fabric-device-fidelity-derives-from-nos)); anything else is a real NOS and forces `clock: wall` |
| `fabric.technology` | O | `ethernet` \| `ethernet-roce` \| `infiniband`, D `ethernet` | |
| `fabric.manager` | O | `pack/manager` | A fabric-manager adapter (UFM, NMX-C, UALink) |
| `fabric.params` | O | map | Template parameters (`leafsPerRack`, `spines`, `rails`, `underlay`, `overlay`, `relay: {linkSelection: nos\|twin\|kea}`) |
| `fabric.select` | O | selector, D every rack | Which racks the template wires |
| `fabric.placeIn` | O | rack selector | Where generated switches are placed when the format reserves no room ([13 — Rack format](#rack-format) `reserved`); D the hall's network racks |
| `spec.facility.power.feeds[]` | R | `{name, capacityW, phases?: 1\|3, voltage?}` | |
| `spec.facility.power.topology[]` | O | `{name, kind: ups\|busway\|pdu-panel, from: feed\|node, capacityW, serves: [rack selector]}` | The one-line diagram between feeds and racks; D feeds serve racks directly |
| `spec.facility.cooling.loops[]` | O | `{name, profile, racks: [rackName], capacityKW, supplyC?}` | |
| `spec.facility.devices[]` | O | `{name, profile, at: hall\|row\|floor, position?}` | Facility devices that belong to no rack: UPS, under-floor leak sensors, room sensors |
| `spec.facility.bms.dialect` | R if any device publishes BMS points | `pack/dialect` | |
| `spec.facility.grid.dialect` | O | `pack/dialect` | |
| `spec.stacks[]` | O | | |
| `stack.name` | R | `name@version` | A stack definition the twin knows ([Stack](#stack)) |
| `stack.components[]` | O | names, D all | |
| `stack.options` | O | map | Validated against the stack's `options` schema |
| `stack.impersonators[]` | O | names from the stack's `impersonators` | Enabling one labels the run **impersonated** ([D24](11-decisions.md#d24--kwok-and-vendor-agent-mocks-are-impersonation)) |
| `spec.hardware[]` | O | `{select, bridge: {interface, network}, identity: observed\|{serial,…}, controls: [{kind: pdu-outlet\|switch-port, ref}], credentialsRef}` | Real devices on the hardware bridge ([07](07-fidelity.md#hardware)). `credentialsRef` names a secret in the secret store, never a value ([15](15-security.md)) |

## Device profile

`kind: DeviceProfile`, `metadata.name: pack/name`, `metadata.version`.

| Field | | Meaning |
|---|---|---|
| `class` | R | registry; core classes: `server`, `compute-tray`, `switch`, `switch-tray`, `dpu`, `power-shelf`, `pdu`, `ups`, `cdu`, `sensor`, `rack-controller`, `appliance`. A pack class names the core class whose behaviours it inherits |
| `form` | R | `{mounting: rack\|tray\|zero-u\|none, units}` |
| `identity` | R if the device has a BMC, MAC or serial | `{serialFormat, macOUI, guidPrefix?, uuidNamespace?}` ([Identity derivation](#identity-derivation)) |
| `components[]` | O | `{name, kind, count, model, vendor?, profile?, ports?, maxW?, pad?}`. `kind` is a registry over core kinds `cpu`, `accelerator`, `nic`, `dpu`, `drive`, `psu`, `fan`, `tpm`, `bmc`, `memory`, `asic`, `pump`, `valve`, `breaker`. Component names are `<name>-<index>`, index from 0, zero-padded to `pad` digits when given. A component with `profile` is a **sub-device** ([04](04-devices-and-racks.md#sub-devices)). Components may own ports (`ports`), which is how a GPU owns its scale-up lanes |
| `bmc` | O | `{dialect, mockup, ipmi: bool, console: sol\|ssh\|none, dhcpVendorClass?}`. Absent for devices with no management controller |
| `ports[]` | R | `{name, kind, speed?, role?, count?, lanes?, owner?}`. `kind` is a registry over `ethernet`, `infiniband`, `scale-up`, `pcie`, `power-in`, `power-out`, `coolant-in`, `coolant-out`, `serial`; `scale-up` ports carry `technology` (`nvlink`, `ualink`, `xgmi`, …). `role` is a network role ([Estate](#estate)); never a network name. `owner` names a component group for component-owned ports |
| `power` | R | `{standbyW, idleW, maxW, model: per-device\|per-component}` |
| `cooling` | R | `{kind: air\|liquid\|none, loop?: rack-manifold\|external}` |
| `sensors[]` | O | `{name, quantity, unit, warn?, crit?, source}`; `source` is a core sensor behaviour (`coolant-in`, `accelerator-max`, `inlet`, `exhaust`, `power`, `flow`, …) or a pack hook |
| `firmware[]` | O | `{component, baseline, updateable: bool, applyOn: reset\|ac-cycle\|immediate}` |
| `behaviours` | O | Parameters for core primitives (`power`, `boot`, `firmware`, `settings`, `accounts`, `sensors`, `links`, `health`) and named pack hooks; preconditions are CEL ([14](14-expressions.md)) |
| `faults[]` | R | Catalogues that may target this profile. **A strict allow-list**: a fault outside it is refused |
| `quirks[]` | O | Dialect quirks that are **on** for this profile by default |
| `kubernetes` | O | `{resources: {name: count}, labels, dra?}` — what a KWOK projection advertises for this profile |
| `bms` | O | `{objectType, points: [{name, pointType, sensor}]}` — how the device's points are published in a BMS dialect |
| `fidelity` | R | `{model: {}, vm?: {arch, firmware: uefi, tpm: bool, image?, inHost: [], cost: {ramGB, vcpu}}, hardware?: {}}` |
| `layout` | O | `{faceplate: {front: [...], back: [...]}}` — positions of components and ports for the console |
| `provides[]` | R | `{capability, at: [levels], via?}` ([06](06-packs-and-stacks.md#the-capability-contract)) |
| `isNot` | R | `{all: [], model?: [], vm?: [], hardware?: []}` or a plain list meaning `all` ([07](07-fidelity.md)) |
| `sources[]` | R | `{what, url, date}` for every count and version |

## Rack format

`kind: RackFormat`, `metadata.name: pack/name`.

| Field | | Meaning |
|---|---|---|
| `archetype` | R | `rack-scale` \| `server-scale` \| `pcie` \| `cpu` \| `storage` \| `network` |
| `params` | O | map `name → {type: int\|profile\|string\|rack, default, range?, allowed?, class?, provides?, label}`. A `profile`-typed parameter is constrained by `class` and `provides`, never by a list of names from other packs |
| `slots[]` | R | `{names, profile, accepts?: {class, mounting, units, cooling, maxW}, u?, side?: front\|back\|zero-u, at?}`. `names` takes brace ranges and `$param`. `u` is explicit when given, otherwise assigned from `layout.front` top to bottom. `at` places an unslotted device (a leak sensor) at another slot's level |
| `layout` | R | `{height, uNumbering: eia, front: [{slots, group?}], back: [views], reserved?: [{u, units, for: fabric\|facility}]}`. `uNumbering: eia` means U1 is at the bottom. `reserved` is room kept for template-generated devices |
| `fixedLinks[]` | O | `{template}` or explicit `{a, b, kind: fixed}` |
| `domains[]` | O | `{kind, technology?, members, manager, formation: <CEL>, spans?: $param}`. `kind` is a registry over `scale-up`, `ib-subnet`, `ethernet-fabric`, `power`, `cooling`, `failure`, `maintenance`. A spanning domain lives at the path of the rack named first in sorted order and requires `peer` to be mutual |
| `power` | R | `{inputs: [{name, kind: busbar\|pdu\|psu, capacityW, redundancy: N\|N+1\|N+N, minSources}], capacityW}` |
| `cooling` | R | `{kind, manifold?: liquid, loop: external\|internal}` |
| `propagation` | O | `{leak: {from: sensorSelector, to: deviceSelector, effect}}` — the format's rule for how a facility fault reaches devices |
| `management` | O | `{rackController: pack/adapter}` |
| `behaviours` | O | `{powerOn: {sequence}, …}` |
| `provides[]` | O | capabilities the format itself provides (a rack controller) |
| `isNot` | R | |
| `sources[]` | R | |

## Fabric template

`kind: FabricTemplate`. Input: racks selected, their devices' ports by role, `params`. Output:
devices (with `placeIn`), links, per-NOS configuration, and `provides`. **Port allocation is a pure
function of the endpoint's path** so that adding a rack never moves another rack's links
([03](03-estate-model.md#changing-a-running-estate)). `layout.tiers` for the console. `isNot` required.

## Fault catalogue

`kind: FaultCatalogue`, `metadata.name: pack/catalogue`. Each fault:

| Field | | Meaning |
|---|---|---|
| `name` | R | Fault id is `pack/catalogue/name` |
| `title` | R | |
| `target` | R | `{component?: kind, device?: class, port?: true, deviceWith?: bmc\|firmware\|tpm, vendor?}` |
| `params` | O | map of typed parameters the injector may set |
| `effects[]` | R | Effect verbs ([08](08-scenarios-and-faults.md#faults)): `set`, `counter`, `ports`, `kernelLog`, `shim`, `redfish.logEntry`, `redfish.property`, `bms`, `power.loss`, `link`, `attestation`, or a pack `EffectHandler`. Each may carry `appliesAt: [levels]` |
| `clears` | R | `{on: [triggers], after?: duration, manual: bool}`; triggers from the registry ([14](14-expressions.md#triggers)) |
| `isNot` | R | What the fault does not model (a real Xid 79 also resets PCIe link state; this one does not) |

## Scenario

`kind: Scenario`.

| Field | | Meaning |
|---|---|---|
| `estate` | R | `{requires: {formats?: [], capabilities?: [], racks?: {min}}}` — constraints, not a file. The scenario runs on the estate it is given |
| `stack` | R | `{components: [], accessors: {alias: <stack>.<component>.<accessor>}}` — the installed stack comes from the estate; the scenario names the components it needs and maps **stack-neutral aliases** to accessors so the same scenario can run against another stack |
| `timeout` | R | duration |
| `chaos` | O | `{enabled, ratePerDeviceHour}` |
| `setup[]` | O | steps run before the clock of `steps` starts |
| `steps[]` | R | each one of `wait`, `act`, `fault`, `expect` |
| `wait` | | `{for: <CEL over model, accessors, events>, within}` |
| `act` | | `{power: On\|…, select}` \| `{cable: pull\|replace, link}` \| `{reseat, select}` \| `{seconds}` |
| `fault` | | `{select, inject: faultId, params?, id?: name}` or `{clear: name \| {select, inject}}` |
| `expect` | | `{within, all: [observation \| {not: observation}]}`. A positive observation must become true within the window; **a `not` must hold for the entire window** |
| observation | | `redfish: {select, property, equals}` · `ipmi: {select, sensor, equals}` · `bms: {topic (MQTT filter with + and #), value}` (observed at the publisher's log) · `event: {canonical, from?: stack\|user\|scenario, on: selector}` · `domain: {path, state}` · `faults: {select, count}` · `accessor: {alias, args, equals}` |
| `report` | O | D `fidelity` |

A scenario result is `passed`, `failed`, `not-run` (a requirement unmet) or `impersonated` (it ran
with an impersonator, so it is not a conformance pass), and always carries its fidelity report.

## Stack

`kind: Stack`, `metadata.name`, `metadata.version`.

| Field | | Meaning |
|---|---|---|
| `components` | R | map `name → component` |
| `component.obtain` | R | `{source, ref, licence}` — pinned; nothing redistributable is bundled ([D13](11-decisions.md#d13--the-twin-redistributes-no-vendor-binaries-or-images)) |
| `component.install[]` | R | ordered steps: `{helm: {chart, values}}` \| `{kubectl: {manifests}}` \| `{script}`; images pinned by digest; pulled through the twin's artifact mirror ([15](15-security.md#the-install-path)) |
| `component.serviceZone` | O | `{kubernetes: {nodes, distro}, vms: [...]}` — the control plane the twin provisions for it |
| `component.clusters[]` | O | `{name, on: selector of vm hosts, distro, version}` — tenant clusters the twin builds on `vm` hosts for components that `installInto` one |
| `component.attach[]` | R | network roles its service zone joins |
| `component.requires[]` | R | `{capability, scope?: estate\|{each: selector}, min?, anyOf?: [...], when?: <CEL>, gate?: install\|scenario}` ([06](06-packs-and-stacks.md#the-capability-contract)) |
| `component.inputs` | O | map `name → <CEL over estate>` — what the twin hands it, and nothing more |
| `component.outputs` | O | map `name → {kind: endpoint\|credential\|kubeconfig, from}` — what flows back to the twin's adapters (a broker URL, an auth-callout credential, a cluster kubeconfig) |
| `component.accessors` | O | map `name → {kind: http\|grpc\|kube, endpoint, credential: twin-issued read-only, join: {twin: serial\|bmcMac, stack: field}}` — the only way scenarios and the console read the stack ([08](08-scenarios-and-faults.md#what-an-assertion-may-observe)) |
| `component.fabricProfile` | O | what it expects of the fabric (ASN scheme, route targets, default route) |
| `options` | O | JSON-Schema-like typed options the estate may set |
| `impersonators[]` | O | `{name, replaces: agent, obtain, appliesTo: model}` |
| `scenarios[]` | R | |
| `isNot` | R | |

## Event

The committed event log ([D27](11-decisions.md#d27--committed-and-derived-state-event-retention-and-redaction)).

| Field | | Meaning |
|---|---|---|
| `seq` | R | monotonic |
| `t` | R | clock time |
| `wall` | R | wall time |
| `kind` | R | `request` \| `state` \| `fault` \| `act` \| `observe` \| `twin` \| `stack` |
| `source` | R | `{class: stack\|user\|scenario\|behaviour\|twin\|observer\|impersonator, principal, address?}` |
| `path` | R | |
| `request` | O | `{protocol, method, target, canonical}` — `canonical` from [14](14-expressions.md#canonical-request-names) |
| `state` | O | `{field, from, to}` |
| `cause` | O | seq |
| `severity` | O | |
| `data` | O | **redacted**: credentials, session tokens and certificate private material never appear ([15](15-security.md)) |

Sensor readings and computed power are **derived state**: computed on read from the seed, the
clock and committed state, never logged.

## Identity derivation

Version `identity/v1` ([D26](11-decisions.md#d26--identity-derivation-is-versioned-and-includes-the-profile)):

```
prk      = HKDF-Extract(SHA-256, salt = "dc-twin/identity/v1", ikm = utf8(seed))
material = HKDF-Expand(SHA-256, prk, info = kind ‖ 0x00 ‖ path ‖ 0x00 ‖ profile@major ‖ 0x00 ‖ index, len)
```

| Identity | Kind | From material |
|---|---|---|
| Serial | `serial` | Rendered through the profile's `serialFormat`: literal text plus `{digits:n}`, `{upper:n}`, `{hex:n}`, `{alnum:n}` |
| System UUID | `uuid` | 16 bytes, version and variant bits set |
| MAC | `mac:<port>` | profile `identity.macOUI` + 3 bytes |
| InfiniBand GUID | `guid:<port>` | `guidPrefix` + 5 bytes |
| Factory BMC credentials | `bmc-password` | 16 alphanumerics; the username is the dialect's factory default |
| DPU ↔ host pairing | derived from both paths | |
| TPM endorsement key | **not derived** | swtpm creates it; its state is persisted per device path and its EK certificate is issued by the estate CA with the serial in the subject |

Because `profile@major` is in the derivation, **changing a device's profile gives a new identity**
and changing its fidelity (same profile) keeps it. The derivation version is part of the apiVersion
compatibility contract.

## Names, pins and the lockfile

- A pack id has no `/`: `generic`, `nvidia`, `oem-dell`, `amd`. Every object a pack defines is
  named `<pack>/<name>`; a catalogue's faults are `<pack>/<catalogue>/<name>`. An object's prefix
  must equal its pack.
- A dialect id is `<pack>/<name>` and is the **only** spelling: the capability qualifier
  `redfish/dialect:<pack>/<name>` and the directory `dialects/redfish/<name>/` both derive from it.
- `name@version` in an estate is an exact version. `dctwin up` writes `<estate>.lock` with each
  pack's resolved version and digest and its transitive dependencies; `apply` and CI run against
  the lockfile.
- `pack.yaml` lists `assets: [{path, licence, obtain?}]`: anything captured or derived (a mockup,
  a MIB, a topic schema) names its licence, and anything that may not be committed names how to
  obtain it ([D13](11-decisions.md#d13--the-twin-redistributes-no-vendor-binaries-or-images), Q10).

## Compatibility

`v1alpha1` may change between minor releases until it is frozen as `v1beta1`
([D32](11-decisions.md#d32--compatibility-policy)). Within an apiVersion: fields are only added;
enums only grow; the identity derivation, the path grammar and the event schema do not change.
`dctwin convert` rewrites an estate, scenario or stack file from one apiVersion to the next and
refuses what it cannot convert. The model-store snapshot and the event-log format carry their
apiVersion; an engine resumes only a snapshot of its own apiVersion. The control API and the
sidecar interface are versioned independently and keep one deprecated version alive.

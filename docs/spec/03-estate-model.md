# 03 — Estate model

## The estate file is the only input (D1)

Everything the twin runs is derived from one estate file plus the packs it pins: devices,
cabling, addresses, credentials, VMs, NOS containers, adapter configuration and the stack's
inputs. Nothing is configured by hand after `dctwin up`. A change to the estate is a change to
the file, then `dctwin apply`. **The normative field list is [13 — Estate](13-schemas.md#estate)**;
this document explains the model the file compiles to.

## Shape of the file

```yaml
apiVersion: dc-twin/v1alpha1
kind: Estate
metadata:
  name: hall-a-demo
spec:
  seed: 1729                       # every identity and every random fault derives from it (D26)
  clock: wall                      # wall | virtual — virtual is refused with vm/hardware devices,
                                   # a real NOS, or any installed stack (D22)
  packs: [generic@0.1.0, nvidia@0.1.0, oem-dell@0.1.0]

  fidelity:
    default: model
    overrides:                     # last match wins
      - { select: "a03/compute-tray-{01,02}", level: vm, os: stack }   # os: twin | stack (D23)

  sites:
    - name: site-1                 # exactly one site in v1alpha1 (D30)
      halls:
        - name: hall-a
          rows:
            - name: a
              positions: 10
              racks:
                - { name: a03, position: 3, format: nvidia/gb300-nvl72, params: { shelves: 8 } }
                - { name: a05, position: 5, format: generic/hgx-8gpu,
                    params: { server: oem-dell/xe9680-h100, servers: 4 } }
                - { name: a10, position: 10, format: generic/network-rack }

  networks:                        # profiles bind ports to ROLES, never to these names
    oob:          { role: management,   dhcp: stack, fabric: oob }
    provisioning: { role: provisioning, dhcp: stack, fabric: frontend }
    frontend:     { role: frontend,     fabric: frontend }
    backend:      { role: scale-out,    fabric: backend }
    service:      { role: service,      cidr: 10.90.0.0/24, dhcp: none }   # the stack's control plane

  fabrics:
    - { name: oob,      template: oob-flat,   nos: none }                   # model switches (D21)
    - { name: frontend, template: clos-2tier, nos: sonic-vs,
        params: { leafsPerRack: 2, spines: 2, underlay: bgp-unnumbered, overlay: evpn,
                  relay: { linkSelection: twin } } }                       # SONiC-VS has none (05)
    - { name: backend,  template: rail-optimised, technology: infiniband, manager: nvidia/ufm,
        params: { rails: 4 }, select: "a0{1..4}" }

  facility:
    power:
      feeds: [{ name: A, capacityW: 900000 }, { name: B, capacityW: 900000 }]
    cooling:
      loops: [{ name: cdu-1, profile: generic/cdu-inrow, racks: [a03], capacityKW: 600 }]
    bms:  { dialect: nvidia/dsx-exchange }
    grid: { dialect: nvidia/dsx-flex }

  stacks:
    - name: nvidia-dsx-os@0.1.0
      components: [nico, nvsentinel, dsx-exchange]
      options: { dpuMode: false }
      impersonators: []            # naming one labels every run "impersonated" (D24)
```

Every value above is illustrative. Rack formats, profiles and dialects are defined in packs
([04](04-devices-and-racks.md), [06](06-packs-and-stacks.md)).

## Paths and selectors

Every object has a **path** that is stable across runs. The grammar:

```
path       = segment *( "/" segment )
segment    = 1*( ALPHA / DIGIT / "-" / "." / "_" )          ; case-sensitive, no spaces
port-ref   = path ":" portname                               ; a port is addressed with ":"
link-ref   = port-ref "~" port-ref                           ; a link, ends in sorted order
```

Canonical paths:

| Object | Path |
|---|---|
| rack | `<site>/<hall>/<rack>` (rows are not segments; rack names are unique per hall) |
| device in a rack | `<site>/<hall>/<rack>/<device>` |
| component | `<device path>/<name>-<index>` (index from 0; padded only when the profile says `pad`) |
| sub-device (a DPU with its own BMC) | `<device path>/<name>-<index>`, the same form; it is a component that is also a device ([04](04-devices-and-racks.md#sub-devices)) |
| the device's BMC | `<device path>/bmc-0` |
| port | `<device path>:<port>`, e.g. `…/switch-tray-04:nvl17`, `…/leaf-a03-1:eth12` |
| template-generated device | the rack it is placed in: `<site>/<hall>/<rack>/<device>`, never a separate `fabric/` tree |
| facility device | `<site>/<hall>/facility/<device>`; feeds `<site>/facility/power/<feed>` |
| domain | `<rack path>/domain/<kind>` (a spanning domain: the first rack in sorted order) |
| network, fabric | `<site>/networks/<name>`, `<site>/fabrics/<name>` |

**Selectors** are patterns over paths:

```
selector   = pattern *( SP pattern ) [ SP filter ]
pattern    = pseg *( "/" pseg )
pseg       = 1*( segment-char / "*" ) / "**" / brace
brace      = "{" item *( "," item ) "}" / "{" DIGITS ".." DIGITS "}"   ; lists and padded ranges
filter     = "?" key "=" value *( "&" key "=" value )                  ; class=, profile=, fidelity=
```

- `*` matches one segment or part of one; `**` matches any number of segments.
- `{a,b}` expands to alternatives, `{01..18}` to a zero-padded range.
- **Anchoring is a suffix match on a segment boundary:** `a03/compute-tray-*` matches
  `site-1/hall-a/a03/compute-tray-07`; `compute-tray-07` alone matches every rack's tray 07.
- There are **no character classes**: `0[1-4]` is written `0{1..4}`. The `[…]` syntax is reserved.
- A filter restricts matches by attribute: `a0*/** ?class=switch-tray`.
- Overrides, faults, scenarios, the CLI and the control API all use the same grammar.

## The runtime model

The compiler expands the file into these objects.

| Object | Key fields |
|---|---|
| **Device** | path, profile, class (a registry over core classes, [04](04-devices-and-racks.md#open-registries)), location (rack, slot or U), fidelity, `os` mode for `vm` hosts, identity, components, ports, interfaces, **owned and observed fields** ([D19](11-decisions.md#d19--model-fields-are-owned-or-observed-observers-reconcile-the-substrate)) |
| **Component** | path, kind (registry), model, identity, state; a component with a profile is a sub-device |
| **Port** | path, kind (registry: `ethernet`, `infiniband`, `scale-up` with a `technology`, `pcie`, `power-in`, `power-out`, `coolant-in`, `coolant-out`, `serial`), role, speed, MAC or GUID, state, owner (device or component) |
| **Link** | two port refs, kind `cable` or `fixed`, state; allocation is a pure function of the endpoint paths |
| **Domain** | kind (registry: `scale-up`, `ib-subnet`, `ethernet-fabric`, `power`, `cooling`, `failure`, `maintenance`), technology, members, manager, state |
| **Network** | role, addressing, `dhcp`, fabric, attached ports, substrate segments |
| **Sensor** | path, quantity, unit, source behaviour, thresholds; **derived** state, never logged ([D27](11-decisions.md#d27--committed-and-derived-state-event-retention-and-redaction)) |
| **Feed, distribution node, loop** | facility power and cooling, with the devices they serve and their capacities |

### Identity

Every identity is derived from the seed, the path and the profile by the versioned function in
[13 — Identity derivation](13-schemas.md#identity-derivation): serial numbers in the profile's
format, system UUIDs, MACs under the profile's OUI, InfiniBand GUIDs, factory BMC credentials and
the DPU-to-host pairing. The same file and seed give the same estate on every run, and a device
removed and re-added at the same path with the same profile returns with the same identity.
**A different profile at the same path is a different machine** ([D26](11-decisions.md#d26--identity-derivation-is-versioned-and-includes-the-profile)).
TPM endorsement keys are the exception: swtpm creates them, and the twin persists swtpm state per
device path so a reseat keeps the same TPM.

### Addressing

- **`dhcp: stack`** (the default): devices that obtain addresses by DHCP get them from the
  software under test, exactly as on a site, and the twin assigns them nothing. Requests reach the
  stack through a **relay**, because stacks such as NICo accept relayed requests only: the NOS's own
  relay where a real NOS serves the network, otherwise the twin relay that model switches provide
  ([D20](11-decisions.md#d20--endpoints-are-hosted-per-modelled-switch-port), [05](05-interfaces.md#boot-and-addressing)).
- **`dhcp: twin`**: for an estate with no stack serving DHCP, a labelled **twin site DHCP**. The
  fidelity report prints that addressing was the twin's.
- **`dhcp: none`** and a `cidr`: the twin allocates static addresses from the CIDR, which is how
  the `service` network and NOS management interfaces are addressed. **Service-zone addresses are
  static and estate-derived**, because the NOS relays must be configured with the stack's DHCP
  address at compile time.

### State

State lives on devices and components, and each field is **owned** (the model is authoritative)
or **observed** (the substrate is authoritative; the model caches what an observer last saw):

| Field | `model` device | `vm` device | real NOS |
|---|---|---|---|
| power state and transitions | owned | observed (libvirt lifecycle) | observed |
| boot order, overrides, Secure Boot keys | owned | observed (OVMF NVRAM, read on shutdown and on request) | — |
| BIOS/UEFI settings and lockdown | owned | owned, **applied to OVMF NVRAM on the next reset** | — |
| health roll-up, firmware inventory, BMC accounts and sessions, certificates, SEL and Redfish logs, error counters | owned | owned | owned (for the BMC projection) |
| link state, LLDP neighbours | owned | observed on the host side | observed (NOS port state) |
| routing and relay state | — | — | observed, read-only |
| disk contents | — | the VM's; the model holds a digest | — |
| TPM state | — | swtpm's; persisted per path | — |

An **owned** field changes only through the engine. An **observed** field changes when its observer
reports, and the report is a committed event with `source.class: observer`.

## Validation

`dctwin validate` refuses an estate before anything starts if:

- a path, profile, rack format, dialect, fabric template, pack version or stack cannot be
  resolved, or the lockfile disagrees with the pins;
- a rack format's parameters are out of the range or type its format declares; a slot override
  names a profile the slot does not `accept`; two racks share a name in one hall; a position is
  out of range or used twice;
- a port has two cables, or a cable joins incompatible port kinds, roles or speeds; a network role
  maps to more than one network for a rack; a relayed network names no fabric;
- a domain has a member its kind forbids, or a spanning domain's `peer` is not mutual;
- a device is placed at a fidelity its profile does not support; a `vm` profile's `arch` differs
  from the host's and `--allow-tcg` was not given ([R-06](../reviews/2026-10-18-spec-review.md));
- `fidelity.overrides` and a fabric's `nos` disagree about a switch
  ([D21](11-decisions.md#d21--model-switches-are-a-fidelity-level-fabric-device-fidelity-derives-from-nos));
- `clock: virtual` is requested with any `vm` or `hardware` device, any real `nos`, or any entry
  in `stacks` ([D22](11-decisions.md#d22--an-installed-stack-forces-the-wall-clock-exact-replay-is-a-twin-only-run));
- a stack requirement with `gate: install` is unmet ([06](06-packs-and-stacks.md#the-capability-contract)).
  Other unmet requirements are **warnings**, and the scenarios that need them are marked
  `not-run` rather than refusing the estate;
- a CEL expression fails type-checking against its environment ([14](14-expressions.md)).

`dctwin check --stack <name>` prints the capability verdicts.

## Changing a running estate

`dctwin apply`, or **Apply** in the console's design mode ([12](12-ui.md#design-mode)), converges
a running estate to a changed file. Each change takes the **physical form** the stack under test
would see on a site, and each is classified **hot** (done without disturbing anything else) or
**cold** (requires restarting part of the substrate, which `apply` refuses unless `--allow-cold`
is given, naming what restarts):

| Change | Class | What the twin does | What the stack sees |
|---|---|---|---|
| Add a rack or device | hot | Racks it, allocates its ports by the deterministic rule, wires veths into existing NOS ports or the model switch, connects power; devices start **off, BMC on standby** | New BMCs requesting DHCP through the relay, new LLDP neighbours, switch ports coming up |
| Remove a device or rack | hot | Powers it off, unplugs power, removes its links; **no other rack's links move** | BMCs and ports disappearing, as when a technician de-racks a machine |
| Change a device's profile | hot | Remove, then add, in the same slot: a **new identity** | A different machine in the same place |
| Change a device's fidelity or `os` mode | hot | Remove, then add, with a new substrate (a VM created or destroyed); same identity, same swtpm state | The same identity disappearing and reappearing: a reseat |
| Hand cabling: add or remove a link between existing ports | hot | Rewires; the links flap | Link down and up, LLDP changes |
| Change a fabric template's parameters, add or remove a real NOS | **cold** | Regenerates the containerlab topology; the affected NOS restart | Every session on those switches drops. Refused by default |
| Change facility | hot | Feeds, loops and sensors re-bound | New and changed BMS points |
| Change `clock`, `seed`, `apiVersion` | **cold** (engine restart) | Refused by `apply`; `down`/`up` | — |

**Port allocation is a pure function of the endpoint's path** (which leaf, which port) so adding a
rack never re-cables another. **Drafts rebase**: the control API's drafts carry the estate file's
etag; applying a draft made against an older file requires a rebase, which merges by key (racks by
`name`, overrides by `select`, cabling by port ref) and refuses a conflict ([12](12-ui.md#users-and-access)).

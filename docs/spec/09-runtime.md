# 09 — Runtime

## Where it runs

One Linux machine with KVM: a workstation, a rack server, or a cloud bare-metal instance (the
deployment model of [brave](https://github.com/RafaySystems/brave)). macOS and Windows can run
`model`-only estates in a Linux VM, but not `vm`-fidelity devices at any useful density.

**The host's architecture decides which profiles can be `vm`.** A `vm` guest of another
architecture runs under TCG at a 10–50× slowdown, which is useless for an OS install or a
Kubernetes node. The GB-series compute trays are aarch64; an x86 host can run them only at
`model`, and `vm` GB trays need an arm64 host ([R-06](../reviews/2026-10-18-spec-review.md), Q17).
The validator refuses a mismatched `vm` profile unless `--allow-tcg` is given, and the fidelity
report prints any TCG guest.

Spreading one estate across several hosts is later work ([Q6](11-decisions.md)).

## Processes

| Process | Runs as | Notes |
|---|---|---|
| `dctwin engine` | One per estate, in the control namespace | Model, behaviours, clock, fault engine, scenario runner, control API, console, event log, secret store ([15](15-security.md)) |
| Adapters | **Sharded**: one process per protocol engine per shard (a rack group, or N endpoints), each in its own network namespace | Redfish, IPMI, DHCP/LLDP endpoints (userspace stack), boot walker, fabric managers, facility publishers, KWOK projection, twin relays. A shard restart is a committed twin incident; an endpoint-level act (BMC reset) never restarts a process |
| Observers | One per substrate kind | libvirt lifecycle and OVMF NVRAM, NOS state (gNMI or CLI), containerlab; they report observed fields ([D19](11-decisions.md#d19--model-fields-are-owned-or-observed-observers-reconcile-the-substrate)) |
| Libvirt domains | One per `vm` device | OVMF, swtpm with persisted state, virtio NICs on twin segments, virtio-vsock to the engine for the in-host agent (`os: twin`) |
| containerlab | One topology per estate | Real NOS containers or VMs, wired from the model's cabling; changing it is a **cold** apply |
| Service zone | VMs, or a Kubernetes cluster on VMs, that the twin provisions per the stack definition | The stack's control plane, on the `service` network and the roles it `attach`es; pulls from the artifact mirror |
| Artifact mirror | One per estate, on the `service` network | A registry and HTTP cache the stack installs from ([15](15-security.md#the-install-path)) |

## Networks

```
 control namespace: engine · control API · console · secret store · scenario runner
   │ unix socket / vsock only; one-way observer path outward; nothing inbound (15)
   ▼
 adapters (per shard, own netns) ──┐
                                    │  one TAP per MODELLED SWITCH PORT (D20)
 oob ─────── model OOB switch ──────┤  ← emulated BMCs, each on its own port
      └── or SONiC-VS OOB switch ───┤  ← vm BMC endpoints, hardware bridge
             relay (giaddr; link-selection from the twin or a Kea relay) → stack DHCP
 provisioning ── vm host NICs (PXE) · model hosts' boot walker · NOS relay → stack DHCP/PXE
 frontend ────── containerlab Clos (SONiC-VS): BGP unnumbered, EVPN; or model switches
 backend ─────── containerlab rail fabric (RoCE) │ InfiniBand: model-only, behind UFM REST
 service ─────── service zone (stack control plane) · artifact mirror · observer path
```

- A **network** is one or more L2 segments. Where a real NOS serves it, the segments are
  containerlab links; where a model switch serves it, each modelled switch port is a TAP owned by
  the userspace stack, bridged per segment, with the twin relay as the segment's gateway
  ([D20](11-decisions.md#d20--endpoints-are-hosted-per-modelled-switch-port)).
- **The userspace network stack** presents every `model` endpoint with its own MAC, DHCP lease,
  ARP, LLDP and TCP on its own port, so per-port facts (circuit id, LLDP neighbour, link state)
  match the model's cabling.
- **InfiniBand has no packet path.** Its ports, GUIDs, links and partitions exist in the model,
  behind UFM REST and in-host sysfs.
- Twin bridges carry no host address; the host forwards nothing between them and its own
  interfaces ([15](15-security.md#network-isolation-d10)).

## Sizing

Draft 0.1 gave estate totals that did not add up ([R-16](../reviews/2026-10-18-spec-review.md)).
Sizing is now **unit costs**, and every unit cost is **measured at the milestone that first ships
it** and published in `docs/measurements/` ([16](16-testing.md#performance-targets)). Until then
the figures below are estimates for planning only.

| Unit | RAM | vCPU | Measured at |
|---|---|---|---|
| `model` BMC endpoint (Redfish, IPMI, DHCP, LLDP) | ~10–20 MB | shared | M0 |
| model switch (`nos: none`), per 64 ports | ~50 MB | shared | M0 |
| SONiC-VS instance (container) | ~2–4 GB | 1–2 | M2 |
| `vm` host, 8-GPU server profile, `os: twin` | 8 GB | 4 | M1 |
| `vm` GB compute tray (aarch64, on an arm64 host) | 8–16 GB | 4 | M4 |
| NICo site-controller node (Kubernetes, Postgres, Vault, Temporal, Keycloak) | 16–24 GB each, ×3 | 4–8 each | M1 |
| artifact mirror | 2 GB + cache | 1 | M1 |
| engine + console, per 1,000 devices | ~1 GB | 2 | M0 |

Worked examples with these estimates:

| Estate | Sum |
|---|---|
| 1,000 `model` BMCs on model switches, no stack (twin self-test) | ~20 GB, 8 vCPU |
| One NVL72 rack at `model`, model OOB switch, frontend of 2 leaves + 2 spines in SONiC-VS, NICo | ~16 GB NOS + ~60 GB NICo + ~2 GB twin ≈ **80 GB**, 24 vCPU |
| The same with two `vm` GB trays | + 32 GB, and an **arm64 host** |
| Hall of 16 racks at `model`, model switches everywhere, NICo impersonated | ~70 GB, 32 vCPU |
| Hall with a SONiC-VS Clos (`leafsPerRack: 2`, 16 racks) | + ~34 instances ≈ **+100 GB**; prefer model switches for scale |

The largest single terms are a real NOS per switch and the stack's own control plane.

## CLI

Each verb names the milestone that ships it ([10](10-roadmap.md)).

```bash
dctwin validate estate.yaml                          # M0  schema, packs, lockfile, cabling, fidelity, clock, CEL (03)
dctwin check --stack nvidia-dsx-os estate.yaml       # M0  the capability contract (06)
dctwin up estate.yaml                                # M0  compile, start substrate and adapters; writes the lockfile
dctwin apply estate.yaml [--allow-cold]              # M1  converge a running estate to a changed file (03)
dctwin status                                        # M0  devices by state, fidelity and health; twin incidents
dctwin get site-1/hall-a/a03/compute-tray-07 -o yaml # M0  one device's model state (owned and observed)
dctwin fault inject 'a03/compute-tray-07/gpu-2' nvidia/xid/xid-79      # M0 (generic catalogues), M3 (nvidia)
dctwin fault clear  'a03/compute-tray-07/gpu-2'
dctwin power off 'a03/switch-tray-04'                # M0  an out-of-band act, as a technician would
dctwin cable pull 'a10/leaf-a03-1:eth12'             # M2
dctwin clock advance 6h                              # M0  virtual clock, twin-only estates
dctwin scenario run scenarios/xid-cordon.yaml        # M0  runner; the suites grow with the milestones
dctwin report                                        # M0  the fidelity report for the running estate
dctwin events --follow --select 'a03/**'             # M0
dctwin mirror pull nvidia-dsx-os                     # M1  populate the artifact mirror for a stack
dctwin capture redfish https://<bmc> --out packs/oem-dell/dialects/redfish/idrac/mockups/xe9680   # M1
dctwin capture host --out capture.yaml               # M3  inside a real host: PCI, DMI, NIC and accelerator inventory
dctwin diff profile oem-dell/xe9680-h100 capture.yaml   # M3
dctwin convert estate.yaml --to v1beta1              # when a new apiVersion ships (13 — Compatibility)
dctwin ui                                            # M0  print the console's address
dctwin down                                          # M0
```

## Control API

- gRPC, with a REST mapping and an OpenAPI description, plus a server-sent event stream of the
  committed log. Versioned independently of the file formats ([13 — Compatibility](13-schemas.md#compatibility)).
- Bound to the control namespace: a unix socket and a loopback listener with a per-estate token;
  OIDC for shared twins; three roles ([15](15-security.md#users-and-the-control-api)).
- The same operations as the CLI, plus model queries by selector and snapshot export.
- **The web console** ([12](12-ui.md)) is served by the engine on the same address and is a client
  of this API and nothing else. What it needs, the API publishes first ([D16](11-decisions.md)):

| Area | Operations |
|---|---|
| Model | Query by selector, at a sequence number or now, owned and observed fields; layout data for halls, racks and devices ([04](04-devices-and-racks.md#layout)) |
| Catalogues | The pinned packs' profiles, rack formats with their parameters and ranges, fabric templates, dialects and quirks, and fault catalogues with targets and effects |
| Drafts | Create a draft of the estate file; edit; validate; diff; classify hot/cold; apply; export; rebase onto a newer file by etag ([D17](11-decisions.md), [03](03-estate-model.md#changing-a-running-estate)) |
| Acts | Faults (inject, schedule, clear, chaos rates), power, reseat, cable pull and replace, BMC reset |
| Clock | Pause, step, advance and speed (virtual, twin-only); read (wall) |
| Events | A stream filtered on the server by selector, kind, source and severity; cause chains; history by range; twin incidents |
| Inspector | A device's protocol view as the stack sees it (Redfish tree, IPMI sensors and SEL, BMS topics, DHCP lease), read through the adapter from the control namespace; the requests the stack made to it, by canonical name |
| Consoles | Serial over LAN, and VM consoles for `vm` devices, over WebSocket |
| Scenarios and stacks | Run, follow and read results with fidelity reports; stack inputs and outputs; the capability check; accessor reads through the one-way path |
| Health | Projection lag per adapter, incidents, metrics ([16](16-testing.md#observability)) |
| Sessions | Export a session's acts as a scenario file |

## Implementation language

The core and in-tree packs are written in **Go** ([D12](11-decisions.md)), the language of
Kubernetes, containerlab and most of the surrounding tooling. CEL has a native Go implementation
([D28](11-decisions.md#d28--one-expression-language-and-a-trigger-registry)). Out-of-tree packs and
reused emulators can be anything, behind the sidecar interface ([D15](11-decisions.md)), whose
contract is batched snapshots per tick plus the committed-event stream, with the engine as the
only clock.

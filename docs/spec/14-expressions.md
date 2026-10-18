# 14 — Expressions, triggers and canonical requests

Draft 0.1 used five different informal expression languages ([R-33](../reviews/2026-10-18-spec-review.md)).
There is now one ([D28](11-decisions.md#d28--one-expression-language-and-a-trigger-registry)).

## The language

**CEL** (Common Expression Language). It is small, side-effect free, has mature Go and TypeScript
implementations, and is what Kubernetes admission already speaks. Every `when`, `for`, `requires`,
`formation` and `inputs` value in a profile, format, stack or scenario is a CEL expression over a
documented environment.

| Where | Environment |
|---|---|
| Stack `requires[].when` | `estate` |
| Stack `inputs` | `estate`, `stack.options` |
| Format `domains[].formation` | `rack` (the compiled rack: `rack.devices`, `rack.domains`, `rack.power`) |
| Profile `behaviours.*.requires` | `device`, `rack`, `estate` |
| Fault `params` defaults | `device`, `component` |
| Scenario `wait.for` | `model`, `accessors`, `events`, `clock` |

### The environment

| Name | Shape (selected) |
|---|---|
| `estate` | `name`, `racks[]` (`name`, `format`, `archetype`, `params`), `fabrics[]` (`name`, `technology`, `nos`, `manager`), `networks{}`, `facility`, `has(kind, value)`: `estate.has("domain", "scale-up/nvlink")`, `estate.has("fabric.technology", "infiniband")` |
| `rack` | `name`, `format`, `devices[]`, `domains[]`, `power.sourcesOn`, `power.minSources`, `feeds[]` |
| `device` | `path`, `class`, `fidelity`, `power.state`, `health`, `components[]`, `rack` |
| `model` | `device(path)`, `rack(path)`, `select(selector)` → list of devices, `domain(path)` |
| `accessors` | `name(args…)` → the accessor's decoded response, read-only |
| `events` | `since(seq)`, `any(filter)`, `count(filter)`; filters by `kind`, `canonical`, `on`, `from` |
| `clock` | `now`, `elapsed` (since the step started) |
| `stack.options` | the estate's values for the stack's typed options |

Examples:

```
estate.has("fabric.technology", "infiniband")
stack.options.dpuMode == true
rack.devices.filter(d, d.class == "switch-tray").all(d, d.health == "OK" && d.power.state == "On")
rack.power.sourcesOn >= rack.power.minSources
model.select("a03/compute-tray-*").all(d, accessors.machine(d.identity.serial).state == "Ready")
```

Expressions are **type-checked at validation time** against this environment; an unknown field is
an estate or pack error, not a runtime surprise.

## Triggers

Named events that clear faults, fire hooks and gate behaviours. A **registry**: the core defines
these, a pack may add `pack/name`.

| Trigger | Fires when |
|---|---|
| `device.powerOn` | the device completes a power-on |
| `device.powerOff` | the device reaches Off |
| `device.reset` | a reset or power cycle completes (the On that follows) |
| `device.acCycle` | an AC cycle completes |
| `device.reseat` | a fidelity or substrate change re-adds the device |
| `firmware.applied` | staged firmware applied on a reset |
| `component.replaced` | a technician act replaces a component |
| `cable.replaced` | a link is restored by an act |
| `manual` | an operator or scenario clears the fault explicitly |

A fault's `clears.on` lists triggers; `manual: true` additionally allows explicit clearing.

## Canonical request names

Every request the software under test makes to a device is logged with the protocol's own method
and path **and** a canonical name, so that assertions, the M0 exit ("a `dctwin power` and a Redfish
reset are the same kind of request") and cross-protocol comparisons do not depend on dialect paths.
Dialects map their resources and actions onto this registry; a request that maps to none is logged
as `other`.

| Canonical | Covers |
|---|---|
| `inventory.read` | ServiceRoot, Systems, Chassis, Managers, Processors, NetworkAdapters, PCIe, Storage reads; IPMI FRU |
| `health.read` | Status reads, sensors, thermal and power metrics; IPMI SDR |
| `power.on` · `power.off.graceful` · `power.off.force` · `power.restart` · `power.cycle` | ComputerSystem.Reset by ResetType; IPMI chassis control |
| `boot.override` · `boot.order` | Boot source override; BootOrder changes |
| `bios.read` · `bios.set` · `bios.reset` · `bios.pending.clear` | Bios, Bios/Settings, ResetBios |
| `secureboot.read` · `secureboot.set` · `secureboot.certificate.add` | SecureBoot and its databases |
| `account.read` · `account.create` · `account.delete` · `account.password` · `account.policy` | AccountService |
| `session.create` · `session.delete` | SessionService |
| `lockdown.set` · `lockdown.read` | OEM lockdown (iDRAC system lockdown, NVIDIA UEFI lockdown) |
| `firmware.inventory.read` · `firmware.update.simple` · `firmware.update.multipart` · `task.read` | UpdateService, TaskService, OEM job queues |
| `log.read` · `log.clear` | LogServices; IPMI SEL |
| `event.subscribe` · `event.sse` | EventService |
| `console.setup` · `console.sol` | Serial console settings; SOL |
| `tpm.clear` | OEM or Bios TPM clear |
| `media.insert` · `media.eject` | VirtualMedia |
| `bmc.reset` · `bmc.defaults` | Manager.Reset, ResetToDefaults |
| `network.read` | Manager and System EthernetInterfaces, NetworkDeviceFunctions, Ports |
| `erase.start` | SecureErase, Drive.SecureErase |
| `other` | anything unmapped |

NICo's `libredfish` trait (about 80 operations) maps onto this list without remainder; the mapping
is the first entry in each NVIDIA dialect's `requests.yaml` ([stack doc](../stacks/nvidia-dsx-os.md#what-nico-asks-a-bmc)).

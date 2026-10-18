# 06 — Packs and stacks

The core knows protocols, not vendors ([D6](11-decisions.md)). Two kinds of extension make the
twin about something specific:

- **Packs** describe **hardware**: device profiles, rack formats, fabric templates, protocol
  dialects, in-host shims, fault catalogues and vendor-specific adapters.
- **Stacks** describe **software under test**: its components, what each requires of the estate,
  how it is installed, what it hands back, how it may be read, and the scenarios that show it works.

The two meet in the **capability contract**. Packs provide capabilities, stacks require them,
and `dctwin check` matches the two before anything runs.

## Packs

```
packs/nvidia/
  pack.yaml                      name, version, depends-on, assets (path, licence, obtain), index
  profiles/                      gb300-compute-tray, nvlink-switch-tray-gb300, bluefield-3,
                                 connectx-8, hgx-b300-baseboard, ufm-appliance, …
  racks/                         gb200-nvl72, gb300-nvl72, gb200-nvl36x2
  fabrics/                       nvlink-spine, nvlink-cross-rack
  dialects/
    redfish/gb-bmc/              dialect.yaml (identity, binding, quirks) · requests.yaml · mockups/
    redfish/nvswitch-tray/
    redfish/bluefield/
    nvue/nvos/                   NVUE REST + SSH/SFTP surfaces of a switch tray
    mqtt/dsx-exchange/           topic dialect for BMS points and isolation requests
  faults/                        xid.yaml, nvlink.yaml, dpu.yaml
  inhost/                        mokka.yaml: how the in-host agent drives the shim and the sysfs tree
  adapters/                      ufm-rest, nmx-c, rack-management (code)
  hooks/                         nvlink-domain-formation, rack-power-sequence (code)
  captures/                      CaptureParser for nvidia-smi -q -x (code)
  ledger/                        fidelity ledger entries for this pack's profiles and dialects
```

Pack ids contain no `/` (`generic`, `nvidia`, `oem-dell`, `amd`), and every object a pack defines
is named `<pack>/<name>` ([13 — Names, pins and the lockfile](13-schemas.md#names-pins-and-the-lockfile)).

| Pack | Contents | When |
|---|---|---|
| `generic` | Generic server, switches, PDU, UPS, CDU, leak point, temperature sensor, management switch; rack formats `hgx-8gpu`, `oam-8gpu`, `gaudi3-8`, `pcie-gpu-server`, `cpu-rack`, `storage-rack`, `network-rack`; fabric templates `clos-*`, `rail-optimised`, `oob-flat`; the twin relay; generic faults (power, BMC, thermal, leak, link, firmware, storage, memory, TPM) | First |
| `nvidia` | As above | First |
| `oem-dell`, `oem-hpe`, `oem-lenovo`, `oem-supermicro` | Server chassis profiles and their BMC dialects (iDRAC, iLO, XCC with the `Oem.Ami` variant, Supermicro), for 8-GPU servers and control-plane machines | With the first stack that needs them ([Q19](11-decisions.md#q19)) |
| `liteon`, `delta` | Power-shelf dialects NICo recognises | M4 |
| `amd` | Instinct OAM-8 profiles (MI300X, MI325X, MI355X); `helios` rack format; UALink template and manager adapter; Pensando NIC and DPU profiles; AMD SMI shim; RAS fault catalogue | After DSX OS ([10](10-roadmap.md)) |
| `intel` | Gaudi 3 server profiles, with the integrated RoCE ports as component-owned ports; HLML shim; Gaudi fault catalogue; Xeon control-plane profiles | After DSX OS |

**Versioning.** Packs are semantically versioned and an estate pins `name@version` exactly;
`dctwin up` writes a lockfile with digests and transitive dependencies. Capability names, profile
names, fault names and dialect ids are a pack's public API: renaming or removing one is a major
version.

### Code in packs

Data first ([principle 4](01-overview.md#principles)). Where a pack needs code, it implements one
of these **extension points**:

| Extension point | Used for |
|---|---|
| `BehaviourHook` | Vendor behaviour over the model: rack power sequencing, scale-up domain formation, an OEM lockdown |
| `DialectHandler` | Redfish OEM resources and actions the binding cannot express, IPMI OEM commands, NVUE and SSH endpoints |
| `Adapter` | A whole protocol the core does not have: UFM REST, NMX-C, rack management, UALink |
| `TopicDialect` | A BMS or grid topic scheme over MQTT: topic shapes, payloads, metadata, request/response points |
| `MibDialect` | A vendor's SNMP MIB for PDUs and UPSs |
| `EffectHandler` | A fault effect verb data cannot express ([08](08-scenarios-and-faults.md#faults)) |
| `InHostDriver` | How the in-host agent drives a vendor's accelerator shim, its sysfs tree and its kernel messages |
| `KubernetesProjection` | What a KWOK node advertises for a vendor: resource names, labels, DRA driver and ResourceSlice shape |
| `CaptureParser` | Turns a vendor tool's inventory output (`nvidia-smi -q -x`, `amd-smi`, `hl-smi`) into components |
| `FabricTemplate` | A generator of devices, links and NOS configuration |
| `Observer` | A reader of substrate-owned state for a substrate the core does not know |

**In-tree packs** are compiled into `dctwin`. **Out-of-tree packs** supply the same extension
points as sidecar processes over a documented gRPC interface whose contract is batched snapshots
per tick plus the committed-event stream ([D15](11-decisions.md)).

**Reusing an upstream emulator** (a vendor's mock, a NOS image) is done behind `Adapter`. The pack
declares whether the emulator is driven from the model or keeps its own state. If it keeps its own
state, the fidelity report prints it as **unsynchronised** ([07](07-fidelity.md)).

## The capability contract

A capability is a name `<area>/<name>[:<qualifier>]`. The registry defines each name and what its
qualifier means ([D31](11-decisions.md#d31--capability-contract-semantics)):

| Area | Capabilities | Qualifier |
|---|---|---|
| `redfish` | `redfish/dialect:<pack>/<name>`, `redfish/event-service-sse`, `redfish/update-service`, `redfish/update-service-multipart`, `redfish/secure-erase`, `redfish/leak-detectors`, `redfish/account-service` | the dialect id |
| `ipmi`, `console` | `ipmi/lan`, `console/sol`, `console/ssh` | — |
| `dhcp`, `boot` | `dhcp/relay-giaddr`, `dhcp/relay-option-82`, `dhcp/relay-link-selection`, `boot/pxe`, `boot/http`, `boot/virtual-media`, `boot/walk` | — |
| `tpm2` | `tpm2/measured-boot`, `tpm2/ek-ca:<issuer>` | the EK CA (`estate`) |
| `accelerator` | `accelerator/nvml`, `accelerator/dcgm`, `accelerator/amd-smi`, `accelerator/hlml`, `accelerator/pci-stub` | — |
| `switch` | `switch/bgp-unnumbered`, `switch/evpn`, `switch/lldp`, `switch/nvue`, `switch/nvos-ssh`, `switch/gnmi` | — |
| `fabric` | `fabric/ufm-rest`, `fabric/nmx-c`, `fabric/nmx-c-telemetry`, `fabric/rack-management`, `fabric/ualink` | — |
| `dpu` | `dpu/bmc:<dialect>`, `dpu/os`, `dpu/hbn` | the DPU BMC dialect |
| `facility` | `facility/bms:<dialect>`, `facility/grid:<dialect>`, `facility/pdu-snmp:<mib>`, `facility/isolation-request:<dialect>` | the topic or MIB dialect |
| `kubernetes` | `kubernetes/nodes:vm`, `kubernetes/nodes:kwok` (impersonation), `kubernetes/cluster:<distro>` | — |
| `host` | `host/os:twin`, `host/os:stack` | the in-host mode |
| `external` | `external/stub:<service>` | the stubbed service |

**Providers** declare `{capability, at: [model, vm, hardware], via?}`: a profile, a dialect, an
adapter, a fabric template (at its `nos` level) or a substrate feature.

**Requirements** declare:

```yaml
- capability: redfish/dialect:nvidia/gb-bmc
  scope: { each: "?class=compute-tray" }     # every selected device must provide it
  anyOf: [redfish/dialect:nvidia/gb-bmc, redfish/dialect:oem-dell/idrac]   # per device, any one
  min: 1                                     # at least this many providers (with scope: estate)
  preferred: vm                              # below this level the verdict is "reduced"
  when: 'estate.has("fabric.technology", "infiniband")'   # CEL (14)
  gate: install                              # install | scenario (default)
```

**Verdicts** per requirement: `met` (every selected provider at the preferred level or none
declared), `reduced` (met, but below the preferred level or on some of `each`), `stub` (met by an
`external/stub`), `impersonated` (met by an impersonator), `missing`. `dctwin check` prints the
verdict, the providers, and for `missing` the packs that would provide it. **Only `gate: install`
requirements refuse an estate**; the rest warn, and the scenarios needing them are marked
`not-run` ([03](03-estate-model.md#validation)). So an estate can install NICo without `dpu/hbn`
and run every scenario that does not need a DPU.

## Stacks

The stack kind's field list is [13 — Stack](13-schemas.md#stack).

```yaml
apiVersion: dc-twin/v1alpha1
kind: Stack
metadata: { name: nvidia-dsx-os, version: 0.1.0 }
spec:
  options:
    dpuMode: { type: boolean, default: false }
  impersonators:
    - { name: machine-a-tron, replaces: scout and dpu-agent, obtain: { source: "https://github.com/dsx-ai-factory/infra-controller", path: crates/machine-a-tron }, appliesTo: model }
    - { name: kwok, replaces: kubelet, appliesTo: model }
  components:
    nico:
      obtain: { source: "https://github.com/dsx-ai-factory/infra-controller", ref: "<pinned>", licence: Apache-2.0 }
      serviceZone: { kubernetes: { nodes: 3, distro: k3s } }
      install:
        - { helmfile: helm-prereqs, values: inputs }            # MetalLB, cert-manager, Vault, ESO, PostgreSQL
        - { helm: { chart: nico, values: inputs } }
        - { helm: { chart: nico-rest } }                        # Temporal, Keycloak, site agent
      attach: [management, provisioning, service]
      requires:
        - { capability: "redfish/dialect:*", scope: { each: "?class=compute-tray ?class=server" },
            anyOf: ["redfish/dialect:nvidia/gb-bmc", "redfish/dialect:oem-dell/idrac", "redfish/dialect:oem-hpe/ilo", "redfish/dialect:oem-lenovo/xcc", "redfish/dialect:oem-supermicro/redfish"], gate: install }
        - { capability: redfish/event-service-sse, scope: { each: "?class=compute-tray ?class=server" } }
        - { capability: dhcp/relay-giaddr, gate: install }
        - { capability: boot/pxe, preferred: vm }
        - { capability: tpm2/measured-boot, preferred: vm }
        - { capability: fabric/ufm-rest,  when: 'estate.has("fabric.technology", "infiniband")' }
        - { capability: fabric/nmx-c,     when: 'estate.has("domain", "scale-up/nvlink")' }
        - { capability: switch/bgp-unnumbered, when: "stack.options.dpuMode" }
        - { capability: dpu/hbn, when: "stack.options.dpuMode" }
      inputs:
        bmcNetworks:   'estate.networks.filter(n, n.role == "management").map(n, n.cidr)'
        hostNetworks:  'estate.networks.filter(n, n.role == "provisioning").map(n, n.cidr)'
        ufmEndpoint:   'estate.fabrics.filter(f, f.manager == "nvidia/ufm")[0].endpoint'
        tpmCA:         'estate.pki.ekCA'
      outputs:
        exchangeBroker: { kind: endpoint, from: "dsx-exchange" }
      accessors:
        machine: { kind: http, endpoint: "https://{nico-rest}/v1/machines/{id}", credential: twin-issued-readonly,
                   join: { twin: serial, stack: bmc_serial } }
        alerts:  { kind: http, endpoint: "https://{nico-rest}/v1/alerts?rack={rack}", credential: twin-issued-readonly }
      fabricProfile: { underlay: bgp-unnumbered, asn: 32-bit-per-dpu, evpnRouteTargets: ["50100-50500"], defaultRoute: overlay }
    nvsentinel:
      obtain: { source: "https://github.com/NVIDIA/NVSentinel", ref: "<pinned>", licence: Apache-2.0 }
      clusters: [{ name: tenant, on: "?fidelity=vm&class=server", distro: k3s }]
      install: [{ helm: { chart: nvsentinel, into: tenant } }]
      requires:
        - { capability: accelerator/nvml, scope: { each: "?fidelity=vm&class=server" } }
        - { capability: accelerator/dcgm, scope: { each: "?fidelity=vm&class=server" } }
        - { capability: host/os:twin, scope: { each: "?fidelity=vm&class=server" }, gate: install }
  scenarios: [scenarios/bmc-discovery.yaml, scenarios/host-ingest-zero-dpu.yaml, scenarios/xid-cordon.yaml, scenarios/nvl72-leak-response.yaml]
  isNot:
    - "BlueField-enforced isolation is exercised for control flow only; dpu/hbn is a stand-in until Q1 closes."
    - "A pass with machine-a-tron or kwok enabled is a scale run, not conformance."
```

A stack definition:
- **pins** what it installs and where to obtain it, and installs by digest from the artifact
  mirror ([D13](11-decisions.md#d13--the-twin-redistributes-no-vendor-binaries-or-images), [15](15-security.md#the-install-path));
- declares the **service zone** and the **tenant clusters** the twin must provision for it;
- maps estate facts to its **inputs** in CEL, and names its **outputs**, the only things that flow
  back to the twin ([02](02-architecture.md#the-software-under-test));
- declares **accessors** with twin-issued read-only credentials and an identity join, the only way
  the console and scenarios read it ([08](08-scenarios-and-faults.md#what-an-assertion-may-observe));
- lists its **impersonators** and typed **options**;
- owns its **conformance scenarios**;
- states its own **isNot**: what passing these scenarios does not show.

## The same slots, every vendor

Each vendor's datacenter software fills the same functional slots. That is what lets one estate
and one scenario format serve all of them. **The AMD and Intel columns are candidates to verify
when those packs are written. They are not commitments.**

| Slot | NVIDIA (DSX OS and neighbours) | AMD Instinct (candidates) | Intel Gaudi (candidates) | Vendor-neutral |
|---|---|---|---|---|
| Bare-metal lifecycle | NICo | OEM tooling, or vendor-neutral | OEM tooling, or vendor-neutral | Metal³/Ironic, OpenStack Ironic, MAAS, Tinkerbell |
| Host-edge isolation | NICo with DPF on BlueField | Pensando DPU software | — | Switch automation |
| Scale-up fabric | NMX-C, rack management (NVL72) | UALink management (Helios) | Integrated RoCE (no manager) | — |
| Scale-out fabric | UFM (InfiniBand), Spectrum-X | Pensando NIC management | Standard Ethernet | OpenSM |
| In-host accelerator management | NVML, DCGM | AMD SMI, RDC | HLML, `hl-smi` | — |
| Kubernetes enablement | GPU Operator, DRA driver, GPU Feature Discovery | AMD GPU Operator | Intel Gaudi Base Operator | Node Feature Discovery |
| Metrics | DCGM-exporter | AMD Device Metrics Exporter | Gaudi metric exporter | node-exporter |
| Fleet health and remediation | NVSentinel, Fleet Intelligence | — | — | node-problem-detector, medik8s |
| Validation and burn-in | AICR, DCGM diagnostics, nccl-tests | ROCm Validation Suite, AGFHC, rccl-tests | `hl_qual` | — |
| Scheduling | KAI Scheduler, Run:ai | — | — | Kueue, Volcano, Slurm |
| Facility | DSX Exchange, MaxLPS, DSX Flex | — | — | — |

Workload-level slots (scheduling, serving, collective benchmarks) can be tested only for
placement and control flow. No kernel runs ([non-goals](01-overview.md#non-goals)).

## Adding a vendor

Taking AMD as the example:

1. **Capture.** Run `dctwin capture redfish` against each real device's BMC, and `capture host`
   with the vendor's tools on a real host; write the pack's `CaptureParser`. Where no hardware is
   available, write profiles from vendor documentation and mark them `source: documentation` in the
   ledger. Record every asset's licence in `pack.yaml`.
2. **Profiles and formats.** Write OAM-8 server profiles with component-owned `scale-up` ports
   (`technology: xgmi`) and, when documented, the Helios rack format with its UALink domain
   (`kind: scale-up, technology: ualink`). Register any new class or port technology.
3. **Dialects.** Usually an OEM pack's Redfish dialect already exists (the server is a Supermicro,
   a Dell, …). Add only what is new, and the vendor's `requests.yaml` from its stack's source.
4. **In-host shim.** Find or build a management-library shim (AMD SMI) and its `InHostDriver` so
   the vendor's real tools, operator and exporter run unmodified ([Q7](11-decisions.md#q7)).
5. **Faults.** Write a catalogue of the vendor's error classes, with their kernel-log templates,
   library error states and Redfish health effects.
6. **Stack.** Write a stack definition for the vendor's software, with requirements, inputs,
   outputs, accessors and conformance scenarios.
7. **Ledger.** Record what each profile was checked against, and what it is not.

None of these steps touches the engine. If one would, that is a missing extension point or
registry value, and it is added to the core as its own change.

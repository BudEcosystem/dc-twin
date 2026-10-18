# Spec review — 2026-10-18

A review of specification draft 0.1 from three angles: engineering feasibility, correctness
against the real software and hardware, and internal consistency. Each finding has an id (`R-nn`),
a severity, where it was found, and **what resolved it**: a decision (`D19`…), a document section,
or an open question it was turned into. The register is kept so that later changes can cite the
finding they close.

Severity: **B** blocking (the first milestones cannot be met as written), **M** major (an
implementation would go wrong or two implementations would diverge), **m** minor.

## Design

| Id | Sev | Finding | Resolved by |
|---|---|---|---|
| R-01 | B | Nothing says how the in-host agent and the Mokka shim get into an OS that the stack (NICo) installs itself over PXE; a NICo-installed `vm` host has no agent and zero GPUs | [D23](../spec/11-decisions.md#d23--two-in-host-modes-for-vm-hosts) (`os: twin` / `os: stack`), [05 — Inside a booted host](../spec/05-interfaces.md#inside-a-booted-host-vm-fidelity), Q15 |
| R-02 | B | One TAP for every `model` BMC puts them all behind one switch port: same option-82 circuit-id, `lldpd`'s per-port neighbour cap, no per-port link-down; the model's cabling and the substrate disagree on exactly what NICo pairs machines by | [D20](../spec/11-decisions.md#d20--endpoints-are-hosted-per-modelled-switch-port) |
| R-03 | B | `nos: none` had no substrate: one Linux bridge gives the stack's DHCP unrelayed broadcasts with no giaddr, which NICo refuses | [D20](../spec/11-decisions.md#d20--endpoints-are-hosted-per-modelled-switch-port), [05 — Network](../spec/05-interfaces.md#network) |
| R-04 | B | Substrate-owned state (a crashed VM, OVMF boot order rewritten by an installer, Secure Boot keys enrolled in the OS, NOS tables) never flowed back into the model; D2's consistency claim was false for every `vm` device and real NOS | [D19](../spec/11-decisions.md#d19--model-fields-are-owned-or-observed-observers-reconcile-the-substrate), [02 — Observers](../spec/02-architecture.md#observers) |
| R-05 | B | The virtual clock cannot coexist with a real stack (it runs on wall time); "replays exactly" is meaningless when the stack's requests are inputs; `mixed-hall.yaml` installed a stack under `clock: virtual` | [D22](../spec/11-decisions.md#d22--an-installed-stack-forces-the-wall-clock-exact-replay-is-a-twin-only-run), [08 — Clocks](../spec/08-scenarios-and-faults.md#clocks), example fixed |
| R-06 | B | GB-series compute trays at `vm` are aarch64; on an x86 host they run under TCG at 10–50× slowdown, so the flagship example did not run on "one Linux machine with KVM" | [03 — Validation](../spec/03-estate-model.md#validation) (arch check), [09 — Where it runs](../spec/09-runtime.md#where-it-runs), [10](../spec/10-roadmap.md) M3/M4, Q17 |
| R-07 | B | The Redfish identity strings would be rejected by NICo: GB hosts are `Product: GB BMC` / `GB200 NVL` with GB300 told apart by a chassis `Model: NVIDIA GB300`; NVSwitch trays are `P3809`; power shelves are Liteon/Delta; BlueField is `Vendor: Nvidia` plus a system Id. One `nvidia/gb-bmc` dialect with `GB300 NVL` matched nothing | [stack doc — Dialects](../stacks/nvidia-dsx-os.md#dialects-nico-recognises), [05 — Dialects](../spec/05-interfaces.md#dialects-and-quirks), prototype data fixed |
| R-08 | B | The leak model contradicted NICo: it reads *in-tray* leak detectors over Redfish `LeakDetectors` and shuts down the leaking trays; *BMS* leaks arrive as `Rack/RackLeakDetect`, raise an alert that blocks allocation, and power nothing off; rack isolation is the BMS's job. The scenario asserted undocumented behaviour | [stack doc — Leaks](../stacks/nvidia-dsx-os.md#the-two-leak-paths), [08 — Scenarios](../spec/08-scenarios-and-faults.md#scenarios), example rewritten, Q18 |
| R-09 | B | M1 scoped Redfish to "power, boot override, virtual media"; NICo's `libredfish` trait has about 80 operations it drives during ingestion (accounts and password rotation, Secure Boot certificates, UEFI lockdown, `machine_setup`, serial-console setup, DPU-first boot order, `clear_tpm`, BIOS pending settings, multipart firmware update) | [D25](../spec/11-decisions.md#d25--a-protocol-engines-scope-is-the-stacks-call-list-the-dialect-binding-is-data), [10](../spec/10-roadmap.md) M0/M1, [stack doc — Call list](../stacks/nvidia-dsx-os.md#what-nico-asks-a-bmc) |
| R-10 | M | SONiC-VS runs ISC `dhcrelay` with no RFC 3527 link selection; NICo accepts giaddr-based segment selection; M2's exit required link selection from a real NOS | [05 — Boot and addressing](../spec/05-interfaces.md#boot-and-addressing), [10](../spec/10-roadmap.md) M2, Q3 |
| R-11 | M | Mokka's PCI and InfiniBand sysfs surfaces are `LD_PRELOAD` interception, which Go binaries bypass; NVSentinel's NIC monitor is Go | [05 — Inside a booted host](../spec/05-interfaces.md#inside-a-booted-host-vm-fidelity), Q16 |
| R-12 | M | NVIDIA's Rack Management Service reaches NVSwitch trays over SSH/SFTP for firmware, image and certificate jobs; the tray profile offered none | [05 — Fabric managers](../spec/05-interfaces.md#fabric-managers), capability `switch/nvos-ssh` |
| R-13 | M | AICR `--snapshot` still deploys validator Jobs; only `--no-cluster` is cluster-free | [stack doc — AICR](../stacks/nvidia-dsx-os.md#ai-cluster-runtime-aicr) |
| R-14 | M | swtpm's endorsement key cannot be derived from the seed, so "a fidelity change is a reseat with the same identity" needed per-path swtpm state | [D26](../spec/11-decisions.md#d26--identity-derivation-is-versioned-and-includes-the-profile), Q5 |
| R-15 | M | M4/M5 exits required NICo to ingest a `model` NVL72 rack, which is only possible with the scout impersonated, which 07 said never counts | [10](../spec/10-roadmap.md) M4/M5 (labelled impersonated scale runs; `vm` trays need an arm64 host) |
| R-16 | M | Sizing was not credible: the "all model" row contained a SONiC-VS Clos (`vm` fidelity, roughly 2–4 GB per instance, 100+ instances for a hall) and omitted NICo's three-node controller | [09 — Sizing](../spec/09-runtime.md#sizing) (unit costs, measured at M0/M2) |
| R-17 | M | D10 had no mechanism: adapters are dual-homed by construction, the stack must be installed and pull images from somewhere, scenarios read the stack's API, the hardware bridge imports the lab's L2 | [15 — Security](../spec/15-security.md), [02 — Isolation](../spec/02-architecture.md#isolation-of-the-twin-itself) |
| R-18 | M | KWOK registers Node objects in the stack's own cluster with the twin holding a cluster credential: impersonation without the label. The same for DPF's mock DPU agent at M6 | [D24](../spec/11-decisions.md#d24--kwok-and-vendor-agent-mocks-are-impersonation) |
| R-19 | M | Identity stability was claimed for devices but not for ports and links: greedy port allocation re-cables the hall when a rack is added (the prototype had this bug); containerlab has no incremental add | [03 — Changing a running estate](../spec/03-estate-model.md#changing-a-running-estate) (hot/cold applies; deterministic port allocation) |
| R-20 | M | The boot walk named no protocol: PXE options 60/93/94/97, 66/67 or ProxyDHCP, TFTP of the NBP, iPXE's second DHCP, the iPXE script subset, where it stops, HEAD vs GET | [05 — Boot and addressing](../spec/05-interfaces.md#boot-and-addressing) |
| R-21 | M | Logging "every state change" with sensors ticking each second is thousands of events per second; the prototype already truncates and breaks cause chains | [D27](../spec/11-decisions.md#d27--committed-and-derived-state-event-retention-and-redaction) |
| R-22 | M | Scenario accessors into the stack's API needed a credential nobody provisioned, a read-only guarantee, and an identity join from twin paths to stack ids | [13 — Stack](../spec/13-schemas.md#stack) (`accessors`), [08](../spec/08-scenarios-and-faults.md#what-an-assertion-may-observe) |
| R-23 | M | "Used by this run" in the fidelity report was undefined; "unsynchronised adapters: none" was printed for an estate full of substrate-owned state | [07 — The fidelity report](../spec/07-fidelity.md#isnot-and-the-fidelity-report) |
| R-24 | M | `vm` hosts need SMBIOS serial/UUID, PCI inventory and NIC MACs coherent with the Redfish tree, and Redfish `Bios`/`SecureBoot`/`BootOrder` writes applied to OVMF NVRAM | [05 — Inside a booted host](../spec/05-interfaces.md#inside-a-booted-host-vm-fidelity), [10](../spec/10-roadmap.md) M1 |
| R-25 | M | One adapter process for a thousand BMCs makes any restart a site-wide BMC outage the stack would never see on hardware | [02 — Adapters](../spec/02-architecture.md#adapters) (sharding; restarts reported) |
| R-26 | M | No plan to verify the twin itself, no performance targets, no observability, no event retention | [16 — Testing](../spec/16-testing.md) |

## The normative layer

| Id | Sev | Finding | Resolved by |
|---|---|---|---|
| R-30 | B | The estate file had no normative schema; `positions`, `position`, `select`, `nos`, `dhcp`, `grid`, `options`, feed and loop capacities appeared only in examples | [13 — Estate](../spec/13-schemas.md#estate) |
| R-31 | B | The path and selector grammar was undefined and used inconsistently (`[…]` as both character class and attribute filter; `/port-17` vs `:eth12`; `/bmc` vs `bmc-0`; generated and facility paths in two places) | [03 — Paths and selectors](../spec/03-estate-model.md#paths-and-selectors) |
| R-32 | B | Identity derivation named no function, OUI or serial grammar, and "change a device's profile" gave the new machine the old serial | [D26](../spec/11-decisions.md#d26--identity-derivation-is-versioned-and-includes-the-profile), [13 — Identity](../spec/13-schemas.md#identity-derivation) |
| R-33 | B | Five undefined expression languages (stack `when:`, format `forms.when`, behaviour `requires`, fault `clears.on`, stack `inputs`) | [D28](../spec/11-decisions.md#d28--one-expression-language-and-a-trigger-registry), [14 — Expressions](../spec/14-expressions.md) |
| R-34 | B | Capability-contract semantics were ambiguous (qualifiers meant three things; `any-of` over dialects met by one tray; "reduced" had no threshold; 03 refused what 08 let run; `dpu/hbn` unmeetable so the full stack could never validate) | [D31](../spec/11-decisions.md#d31--capability-contract-semantics), [06 — The capability contract](../spec/06-packs-and-stacks.md#the-capability-contract) |
| R-35 | M | Switch fidelity had two sources of truth (`nos:` and `fidelity.overrides`); `nos: none` and emulated NVUE departed from D4 without a decision | [D21](../spec/11-decisions.md#d21--model-switches-are-a-fidelity-level-fabric-device-fidelity-derives-from-nos) |
| R-36 | M | Devices generated by fabric templates had no physical location or power source | [13 — Rack format](../spec/13-schemas.md#rack-format) (`reserved`), [04 — Fabric templates](../spec/04-devices-and-racks.md#fabric-templates) |
| R-37 | M | Profiles bound ports to estate network *names*; `service` and `storage` roles were undeclared | [13 — Device profile](../spec/13-schemas.md#device-profile) (port `role`), [13 — Estate](../spec/13-schemas.md#estate) |
| R-38 | M | The facility model was too thin for the UI's one-line diagram and M5; formats hard-coded feed names; no BMS point mapping | [13 — Estate](../spec/13-schemas.md#estate) (`facility`), [13 — Device profile](../spec/13-schemas.md#device-profile) (`bms`) |
| R-39 | M | No hall or floor layout data (rows had only names) | [13 — Estate](../spec/13-schemas.md#estate) (`positions`, `position`, `layout`) |
| R-40 | M | Slot-level edits and hand-written cabling could not be expressed | [13 — Estate](../spec/13-schemas.md#estate) (`slots`, `cabling`) |
| R-41 | M | Format parameters could not express typed profile choices; NVL36x2's 9-vs-18 trays implied a different tray profile; `peer` symmetry undefined | [13 — Rack format](../spec/13-schemas.md#rack-format) |
| R-42 | M | Core enums were closed and NVIDIA-shaped (`nvlink`, `ualink`, `nvlinkLanes`); scale-up ports could not be owned by a GPU; the prototype equated `rack-scale` with InfiniBand | [04 — Registries](../spec/04-devices-and-racks.md#open-registries), [13](../spec/13-schemas.md) |
| R-43 | M | Missing extension points (topic dialects, MIBs, effect verbs, Kubernetes projection, capture parsers); `capture host` undefined | [06 — Code in packs](../spec/06-packs-and-stacks.md#code-in-packs), [09 — CLI](../spec/09-runtime.md#cli) |
| R-44 | M | Nested devices undefined: a DPU is both a device class and a component kind; NICo's model is host+DPU pairs with the DPU's own BMC | [04 — Sub-devices](../spec/04-devices-and-racks.md#sub-devices), [13](../spec/13-schemas.md#device-profile) |
| R-45 | M | Profile field table disagreed with the example and lacked sensors, firmware baseline, standby power, Kubernetes resources, VM cost, quirks, `fidelity.hardware` | [13 — Device profile](../spec/13-schemas.md#device-profile) |
| R-46 | M | Rack-format layout had no explicit U positions, numbering direction, sensor placement, capacity or `isNot` | [13 — Rack format](../spec/13-schemas.md#rack-format) |
| R-47 | M | Object names, namespaces and pack pins were ambiguous; transitive dependencies unresolved | [13 — Names and pins](../spec/13-schemas.md#names-pins-and-the-lockfile) |
| R-48 | M | Fault-catalogue effect verbs and per-fidelity semantics were undefined; redundancy outcomes needed code; leak propagation had no field | [13 — Fault catalogue](../spec/13-schemas.md#fault-catalogue), [08](../spec/08-scenarios-and-faults.md#faults) |
| R-49 | M | Scenario schema existed only by example (`act` steps, the `not` window, fault-instance ids, `**` vs MQTT `#`, canonical request names) | [13 — Scenario](../spec/13-schemas.md#scenario), [14 — Canonical requests](../spec/14-expressions.md#canonical-request-names) |
| R-50 | M | Scenarios bound one estate and one stack, contradicting "comparing stacks" | [13 — Scenario](../spec/13-schemas.md#scenario) (estate constraints; accessor mappings per stack) |
| R-51 | M | Stack definition lacked install steps, tenant clusters, accessors, outputs, impersonators, typed options, a fabric profile | [13 — Stack](../spec/13-schemas.md#stack) |
| R-52 | M | Event schema and canonical request vocabulary undefined; passwords in request bodies would be logged | [13 — Event](../spec/13-schemas.md#event), [D27](../spec/11-decisions.md#d27--committed-and-derived-state-event-retention-and-redaction) |
| R-53 | M | No security model (unauthenticated control API until M7, `hardware` credentials, CA keys, bridge host) | [D29](../spec/11-decisions.md#d29--security-baseline-from-m0), [15 — Security](../spec/15-security.md) |
| R-54 | M | `hardware` fidelity had no estate schema | [13 — Estate](../spec/13-schemas.md#estate) (`hardware`) |
| R-55 | M | No compatibility policy while M0 ships `v1alpha1` (Q12) | [D32](../spec/11-decisions.md#d32--compatibility-policy), [13 — Compatibility](../spec/13-schemas.md#compatibility) |
| R-56 | M | Roadmap omitted the scenario runner, `apply`, `check`, `capture`, the fidelity report; M1 scenarios needed M3's fault engine | [10](../spec/10-roadmap.md) |
| R-57 | M | Licensing and governance gaps: a captured mockup in-tree while Q10 open; derived AsyncAPI topics and vendor MIBs; no owner for capability names | Q10 widened, [13 — Names and pins](../spec/13-schemas.md#names-pins-and-the-lockfile) (`assets`), `GOVERNANCE.md` listed as owed |
| R-58 | m | Capability names inconsistent (`ipmi` vs `ipmi/lan`; `redfish/dialect:…bluefield` vs `dpu/bmc:bluefield`) | [06 — Capability registry](../spec/06-packs-and-stacks.md#the-capability-contract) |
| R-59 | m | Cross-reference errors (05 cited Q3 for NVOS; 09 called a SONiC-VS estate "all model"; the fidelity-report example named a format that does not exist; `ingest-rack.yaml` not in the stack's list) | corrected in place |
| R-60 | m | Multi-site semantics undefined; no hall picker | [D30](../spec/11-decisions.md#d30--one-site-per-estate-in-v1alpha1), [12](../spec/12-ui.md) |
| R-61 | m | Override precedence, initial power state and chaos settings unspecified | [13 — Estate](../spec/13-schemas.md#estate) (`fidelity.overrides` last match wins; `initialState`), [13 — Scenario](../spec/13-schemas.md#scenario) (`chaos`) |
| R-62 | m | `isNot` not required on formats and catalogues, and could not vary by level | [07](../spec/07-fidelity.md#isnot-and-the-fidelity-report) |
| R-63 | m | Draft rebase had no merge semantics; no estate etag | [12 — Users and access](../spec/12-ui.md#users-and-access) |
| R-64 | m | Smaller domain corrections: UFM `/ufmRest` basic auth vs `/ufmRestV3` token; scout probes `/dev/tpmrm0` first; Helios launched July 2026; AICR KWOK profiles include H200; MI355X 1400 W is the liquid-cooled part | corrected in [stack doc](../stacks/nvidia-dsx-os.md), [04](../spec/04-devices-and-racks.md) |

## The prototype

| Id | Sev | Finding | Resolved by |
|---|---|---|---|
| R-70 | M | The README said the simulated stack reaches devices only through protocols; it reads the model directly and reacts to internal events, so the hard property was never exercised | [prototype/README.md — Known limits](../../prototype/README.md#known-limits) |
| R-71 | M | It printed PASSED on impersonated runs, hard-coded the fidelity report, rewrote `spec.clock` outside the diff, ignored `spec.fabrics` | listed as known limits; verdict text changed to name impersonation |
| R-72 | M | Its dialect identity strings were the wrong ones (R-07) | `prototype/js/packs.js` dialects corrected |
| R-73 | m | Data shapes diverge from the schemas (`faultPacks`, tuple firmware, `at:` sensors, `minShelves`, `nvlinkLanes`) | listed as known limits |

## Verified correct on 2026-10-18

NVL72: 18 compute trays, 9 switch trays with 2 NVSwitch ASICs each, 8 × 33 kW shelves of six
5.5 kW PSUs (Lenovo: 6 or 8), DC busbar, liquid cooling, tray- and rack-level leak detection,
about 142 kW; a GB300 tray is 4 B300 + 2 Grace + 4 ConnectX-8 + 1 BlueField-3. NVL36x2: 9 or 18
compute trays per rack and 9 switch trays, per NVIDIA's partition guide v1.2. Gaudi 3: 24 × 200 GbE,
21 in-node and 3 scale-out. XE9680: 6U. NICo: Apache-2.0, Rust, moved to `dsx-ai-factory`;
Sushy/Contoso/RedVirt collapse to one type with no BIOS or lockdown steps; its Kea hook reads
link selection, circuit id, remote id and vendor class and accepts relayed requests only; eBGP
unnumbered with a 32-bit ASN per DPU; NMX-C on port 9370 with TLS; `nmxc-mock` returns
UNIMPLEMENTED for `Subscribe`; `admin-cli tpm_ca add`; zero-DPU mode; `machine-a-tron`;
`bmc-mock` with about 20 profiles, a libvirt backend, an IPMI simulator and SSE; its sushy and
QEMU/swtpm development guides exist. DSX Exchange: NATS, MQTT 3.1.1 only, auth callout, the
AsyncAPI topic shapes, `grid/v1/…`, `NICO/v1/machine/{id}/state`, `make dummy-bms`, no BACnet,
Modbus or SNMP. NVSentinel: DCGM :5555, journald, MongoDB or PostgreSQL, the fake-DCGM demo, the
Mokka guide. Mokka: replaces `libnvidia-ml.so`; profiles a100, b200, gb200, gb300, h100, l40s, t4;
no CUDA. AICR: `--no-cluster`; the NCCL phase needs real GPUs. SONiC-VS is free and runs BGP
unnumbered and EVPN; Cumulus VX has no standalone image for 5.13+. DMTF's Service Validator,
Mockup Creator and Mockup Server exist; swtpm's local CA accepts a custom signing key and issuer.

## Still unverified

Whether a runnable NVOS image exists (negative claim); NICo's "gravity-aware" leak handling (the
overview mentions it, the operations guide's default policy is per leaking tray — Q18); NVL72 at
48U from an NVIDIA source (Lenovo only); whether DCGM diagnostics under Mokka return canned
results; whether containerlab's SONiC-VS enables the relay's source-interface option by default;
MaxLPS and DSX Flex availability (Q4).

# 10 — Roadmap

NVIDIA DSX OS is the first stack. Each milestone ends in **something an unmodified component of
that stack does on the twin**, which is how it is judged finished. No dates: each milestone's
exit is a run, not a calendar entry. Exits that can only be reached with an impersonator say so
and are **scale runs, not conformance passes** ([D24](11-decisions.md#d24--kwok-and-vendor-agent-mocks-are-impersonation)).

Before M0 begins, these are settled: Q2 (build the Redfish engine or wrap `bmc-mock`), Q19 (the
first dialect) and the schemas in [13](13-schemas.md) exist as JSON Schema.

| | Milestone | Adds | Exit: what runs, unmodified |
|---|---|---|---|
| **M0** | Engine, BMCs, the normative layer | Schemas `v1alpha1` as JSON Schema and the lockfile; compiler and validator with CEL type-checking; model store with owned and observed fields; committed event log with redaction; control API with local auth; CLI (`validate`, `check`, `up`, `status`, `get`, `events`, `power`, `fault`, `clock`, `report`, `scenario run`, `down`); the fidelity report; a minimal scenario runner; the `generic/power` and `generic/bmc` catalogues. Userspace network stack with **one TAP per modelled port**, the model OOB switch and the **twin relay with option 82**. Redfish engine for **one dialect**, scoped by NICo's `libredfish` call list ([stack doc](../stacks/nvidia-dsx-os.md#what-nico-asks-a-bmc)): inventory, health, power, boot, accounts, sessions, EventService SSE, LogServices, Secure Boot read. Performance measured | DMTF's Service Validator and an Interop profile derived from the call list pass. NICo's `bmc-explorer-cli` identifies an emulated BMC as the intended hardware type, not as Sushy or unknown. 1,000 `model` BMCs on model switches lease addresses from an external Kea through the twin relay within 60 s, each with its own circuit id. A `dctwin power` and a Redfish reset carry the same canonical request name. The isolation scan finds nothing |
| **M1** | Booting hosts | `vm` fidelity: libvirt domains with OVMF, **swtpm with persisted state** and the estate's EK CA, SMBIOS and PCI inventory coherent with the profile, Redfish `Bios`/`SecureBoot`/`BootOrder` applied to OVMF NVRAM on reset, libvirt and NVRAM observers. Both in-host modes (`os: twin`, `os: stack`). The boot walker's state machine. Serial over LAN. The **service-zone provisioner** and the **artifact mirror**. `apply` for hot changes. The rest of the dialect's call list: BIOS pending settings, lockdown, console setup, multipart firmware update, TPM clear, account rotation, secure erase. Catalogues `generic/firmware`, `generic/tpm`, `generic/leak` (tray detectors) | NICo's site controller, provisioned by the twin, discovers `model` and `vm` BMCs through the twin relay and ingests a `vm` host (x86, 8-GPU server profile, `os: stack`) in zero-DPU mode: identity coherent between BMC and scout, attestation against the estate CA (Q5), machine validation reported, NVMe secure erase and TPM clear on release observed from the twin's side. Firmware drift is quarantined and remediated through UpdateService. Scenarios `bmc-discovery`, `host-ingest-zero-dpu`, `firmware-drift-quarantine`, `bmc-unreachable` pass. `vm` host and NICo node costs measured |
| **M2** | Fabric | containerlab integration with per-port veths, the `clos-2tier` and `oob-flat` templates, SONiC-VS, LLDP from `model` endpoints, NOS observers, cabling from the model, `dctwin cable pull`, hot/cold apply classes. Relay by **giaddr** from the NOS; link selection from the twin relay or a Kea relay container | NICo's DHCP receives requests relayed by a real NOS, segment selected by relay address (Q3). BGP unnumbered sessions from a `vm` host to its leaves come up, with NICo's ASN scheme. A pulled cable is seen as port-down on the NOS and as link-down on the host. SONiC-VS instance cost measured |
| **M3** | Accelerators | Mokka in `os: twin` hosts through the in-host agent, with a **bind-mounted sysfs tree** for Go consumers (Q16); the KWOK projection, **labelled impersonated**; tenant clusters built from the stack definition's `clusters`; `nvidia/xid` and `nvidia/nvlink` catalogues; fault fan-out; the `hgx-8gpu` format and the XE9680 profile; `capture host` and `diff profile` | A Kubernetes cluster the twin built on `vm` hosts runs the GPU Operator, DCGM and NVSentinel unmodified. `xid-79` makes NVSentinel cordon, drain and reboot, and the same fault shows in Redfish and in power. NVSentinel's NIC monitor reads the shimmed sysfs. The Fleet Intelligence agent reports in offline mode. Scenario `xid-cordon` passes |
| **M4** | NVL72 | `gb200-nvl72`, `gb300-nvl72` and `gb200-nvl36x2` formats; the **correct NVIDIA dialects** (`nvidia/gb-bmc` with the GB300 chassis model, `nvidia/nvswitch-tray`, `nvidia/bluefield`, Liteon and Delta power shelves); in-tray leak detectors; the NVLink domain and its formation; NMX-C with telemetry (Q8); rack management including **SSH/SFTP to switch trays**; rack power sequencing; DPUs as sub-devices | **Impersonated scale run:** NICo ingests a `model` NVL72 rack with the scout impersonated, forms the domain, and blocks allocation when a switch tray fails; an NVL36x2 domain spans two racks. **Conformance:** on an arm64 host (Q17), one `vm` GB300 tray ingests without impersonation |
| **M5** | Facility | Power and thermal behaviours with redundancy; CDU, PDU, UPS, power-shelf and leak-point profiles; `propagation` rules; the BMS publisher in the `nvidia/dsx-exchange` dialect using the Rack point types NICo's consumer reads; the isolation request and status points; the grid dialect | Both leak paths of the scenario pass with DSX Exchange and NICo: a tray detector shuts down its tray; a BMS rack point raises the alert that blocks allocation and powers nothing off; above the threshold the isolation request appears and the twin's BMS answers it. The power drop shows on shelves, PDUs and BMS meters together. MaxLPS and DSX Flex only if Q4 allows |
| **M6** | DPUs | BlueField as a sub-device at `vm`: the real BFB if Q1 closes, otherwise a stand-in DPU OS VM running the stack's DPU agent over FRR and an NVUE-compatible surface, labelled; DPF's mock DPU agent, **labelled impersonated** | Decided by Q1. Either NICo provisions a DPU end to end, or the DSX stack's `isNot` names the gap and the scenarios that cannot run |
| **M7** | Conformance and CI | The DSX OS conformance suite; CI on a bare-metal runner; record and replay (twin-only) and re-drive; the fidelity ledger filled in; one real server and BMC in `hardware` mode; OIDC roles for shared twins; the sidecar interface for out-of-tree packs | The suite runs nightly and publishes each result with its fidelity report. At least one profile carries a `hardware-run` ledger entry. The isolation scan runs on the bare-metal runner |

## The console, alongside

The console ([12](12-ui.md)) grows with the milestones. Each step is usable on its own, and each
needs only what its milestone has already put in the control API ([D16](11-decisions.md)).

| With | The console adds |
|---|---|
| **M0** | Floor and rack views from layout data; device overview, components, sensors and logs; the live event stream with cause chains; power acts; the fault panel for the generic catalogues; the protocol inspector for Redfish; the fidelity report; twin health line |
| **M1** | Serial and VM consoles; fidelity and `os` badges; the stack-requests log per device by canonical name; design mode for hot changes (add rack from a format, fidelity per device, validate, diff, apply) ([D17](11-decisions.md)) |
| **M2** | Topology view of fabrics; cable pull and replace; LLDP and BGP state as observed; cold-change warnings |
| **M3** | Vendor fault catalogues; accelerator components; tenant clusters |
| **M4** | NVLink domain view; sub-devices |
| **M5** | Facility views: power one-line from the estate's topology, cooling loops, BMS points; power and temperature overlays |
| **M7** | Scenario runs with results; save a session as a scenario; stacks, accessors and capability check; shared twins with OIDC roles; draft rebase by etag |

## After DSX OS

| | Milestone | Adds |
|---|---|---|
| **M8** | AMD | `oam-8gpu` profiles (Instinct MI300X, MI325X, MI355X); an AMD SMI shim ([Q7](11-decisions.md)); the `amd/ras` catalogue; a stack for the AMD GPU Operator and metrics exporter; the Helios format and UALink once documented |
| **M9** | Intel | Gaudi 3 server profiles, with the integrated RoCE ports wired by the format; an HLML shim; the `intel/gaudi` catalogue; a stack for the Intel Gaudi Base Operator |
| **M10** | Vendor-neutral stacks | Metal³/Ironic, Tinkerbell and MAAS as stacks, so the same estates and scenarios compare lifecycle managers |
| **M11** | Scale-out | One estate across several hosts ([Q6](11-decisions.md)) |

OEM packs (`oem-dell`, `oem-hpe`, `oem-lenovo`, `oem-supermicro`) arrive with the first milestone
whose stack needs them: `oem-dell` at M1 if Q19 picks the XE9680 as the first dialect.

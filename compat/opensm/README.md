# OpenSM over ibsim, on the twin's InfiniBand fabric

The real **OpenSM** subnet manager and **infiniband-diags**, unmodified, manage and read the
twin's InfiniBand fabric: the vendor-neutral "Scale-out fabric: OpenSM" slot of
[06 — The same slots](../../docs/spec/06-packs-and-stacks.md#the-same-slots-every-vendor), and the
"InfiniBand: subnet manager" row of
[05 — Fabric managers](../../docs/spec/05-interfaces.md#fabric-managers).

The fabric is **simulated by [ibsim](https://github.com/linux-rdma/ibsim) from the model**
([D47](../../docs/spec/11-decisions.md#d47--a-stacks-own-subnet-manager-runs-against-ibsim-fed-from-the-model)).
The twin's `ibsim` adapter (`internal/adapter/ibsim`) does three things:

- It renders each InfiniBand fabric as a topology in ibnetdiscover's output format: every switch
  with the system GUID UFM REST reports, every host channel adapter port with the model's port
  GUID, and every cable with its port numbers.
- It runs ibsim on that topology.
- While the twin runs, it applies the model's link state through ibsim's console. A pulled cable,
  a port fault, a powered-off host or a switch without AC becomes `Unlink`; its return becomes
  `Link`.

OpenSM and the diags reach ibsim through its `LD_PRELOAD` library, libumad2sim, in place of the
kernel's `/dev/infiniband` devices. ibsim answers subnet management and performance MADs only.
**There is no data plane**, as there is none anywhere in the twin's InfiniBand. While the SM runs,
the twin's UFM REST serves the LIDs the SM assigned, read from ibsim, as a UFM serves what its
embedded OpenSM assigned.

## Running it

```bash
make compat-opensm                 # or: compat/opensm/run.sh
WORK=/tmp/osm compat/opensm/run.sh # keep the evidence somewhere known
KEEP=1 compat/opensm/run.sh        # leave the (stopped) container for inspection
```

It needs Docker and took about 10 minutes on the VM it was run on. ibsim and libumad2sim are Linux
software, so everything runs in one container native to the Docker host's architecture,
`dctwin-opensm:trixie`. It is built from [Containerfile](Containerfile) with `opensm`,
`infiniband-diags`, `ibsim-utils` and `libumad2sim0` from Debian trixie.

`run.sh` cross-compiles `dctwin` for the container and copies in the binary, the lab estate and
[in-container.sh](in-container.sh). It runs the checks there with no network: everything is on the
container's loopback. The container is named `dctwin-opensm-<pid>` and is removed at the end. The
table and the evidence are copied to `$WORK/out`: OpenSM's log and LFT dumps, ibsim's console
transcript, every diag's output, the rendered topology, and the UFM REST comparisons.

What `in-container.sh` does:

1. **Twin.** It runs `dctwin up examples/estates/lab-model.yaml --adapters ufm,ibsim`. The ibsim
   adapter starts ibsim on `ibsim-backend-ib.net`. It writes `ibsim.json` and
   `ibsim-backend-ib.env` to the state directory, with the socket name, the subnet manager's
   `SIM_HOST` and the preload library.
2. **Subnet manager.** OpenSM attaches at the UFM appliance's `ib0` HCA (`SIM_HOST`, first in the
   topology), as UFM runs its subnet manager on its own port. It uses OpenSM's default options
   plus a flushed log. The lab's `backend-ib` is one subnet: 20 rail leaves joined by 2 spines,
   with 193 HCA ports and 233 links.
3. **Reads.** sminfo, ibstat, iblinkinfo, ibnetdiscover, ibtracert, saquery, smpquery and
   perfquery read the fabric from trays' HCAs. [ibnet.py](ibnet.py) compares three things: what
   ibnetdiscover finds against the model's topology and UFM REST; OpenSM's LFT dump against
   minimum-hop paths over the model's cabling; and UFM REST's LIDs against the SM's.
4. **Changes.** In the twin it runs `dctwin cable pull` and `cable replace` on a tray's cable and
   on a leaf's spine uplink, `power off` and `power on` on a tray, and `power ac-cycle` on the
   SM's own leaf. Then it reads what OpenSM logs and the diags see.

## Results

Run 2026-10-19 on an arm64 Linux VM (Docker 29.5.2): **25 PASS, 1 CANNOT, 0 FAIL** (26 checks).
OpenSM 3.3.23-3.1, infiniband-diags 56.1-1, ibsim 0.12-1 (libumad2sim 0.12-1), Debian 13.

| # | Check | Expected | Observed | Result |
|---|---|---|---|---|
| B1 | The twin runs ibsim on the model's InfiniBand fabric | ibsim running, the topology parsed with no warnings: 22 switches, 193 CA ports, 233 links | ibsim 0.12 running; 1,311 lines parsed, 0 warnings; 22, 193, 233 | PASS |
| B2 | The fidelity report labels the fabric | `backend-ib: SIMULATED by ibsim …`; port state, LIDs and forwarding tables substrate-owned | `backend-ib: SIMULATED by ibsim 0.12 (pid 89, IBSIM_SOCKNAME=dctwin-backend-ib-6c9006, preload …/libumad2sim.so) from the model: 22 switches, 193 channel adapter ports, 233 links`; one substrate-owned line | PASS |
| S1 | OpenSM at the UFM appliance's `ib0` (`SIM_HOST H-b83fd2c6aa950d28`) | Entering MASTER state; SUBNET UP | MASTER after 8.8 s, SUBNET UP after 14.2 s (OpenSM's log), measured 16.7 s by the poller | PASS |
| S2 | `sminfo` from a01-compute-tray-07 HCA-1 | state 3 SMINFO_MASTER, SM GUID the UFM appliance's `ib0` | `sm lid 1 sm guid 0xb83fd2c6aa950d29 … state 3 SMINFO_MASTER` | PASS |
| S3 | `ibstat` at a01-compute-tray-07 HCA-1 | Active/LinkUp, a LID, SM lid 1, port GUID the model's `cx8-0` | Active/LinkUp, LID 83, SM lid 1, port GUID 0xb83fd286a49f9171 | PASS |
| S4 | One subnet: a CA on every rail leaf | one connected fabric; on each of the 20 rail leaves a CA Active, configured by the master | 1 component (22 switches, 193 CA ports, 233 links); 20 of 20 leaves: Active, `sminfo` the master | PASS |
| S5 | `iblinkinfo` over the fabric | 273 switch ports Active/LinkUp (193 HCA links, 40 leaf-spine links at both ends), none Initialize | 273 Active/LinkUp, 0 Initialize; 4X 106.25 Gbps (NDR lanes) | PASS |
| S6 | `ibnetdiscover` of the whole fabric against the model | 22 switches, 193 CA ports, 233 links; node and port GUIDs, port numbers, descriptions equal | equal | PASS |
| S7 | LIDs and routing: OpenSM's LFT dump against the model's cabling | 215 LIDs, one per node; every switch holds all of them and routes every CA LID on a minimum-hop path (4,246 routes) | 215 LIDs; 22 switches with 215 each; 4,246 of 4,246 minimum-hop | PASS |
| S8 | Routing through the spines | leaf a01-1 sends the 174 CAs it does not cable out of uplinks 33 and 34, split; `ibtracert` tray-07 → b05-server-02 HCA-8 crosses a spine | uplinks 33: 87, 34: 87; path rail-a01-1, spine-2, rail-b05-4 | PASS |
| S9 | The SA and PortInfo | the SA's NodeRecord for LID 83 is tray-07's; LinkState Active | port_guid 0xb83fd286a49f9171, `a01-compute-tray-07 HCA-1`; Active | PASS |
| S9b | An SA table query: `saquery NR` with no filter | 215 NodeRecords | 1: libumad2sim carries no RMPP (ibsim's TODO), so an SA response longer than one MAD reaches the client as its first segment | CANNOT |
| S10 | `perfquery` of tray-07's port | PortCounters answered (ibsim's counters, not the model's) | 15 counter lines | PASS |
| U1 | UFM REST against discovery: channel adapter ports | 193 computer ports, each discovered with the same port GUID, node description and system GUID | 193 / 193 / 193 / 193 | PASS |
| U2 | UFM REST against discovery: switch GUIDs | 64-bit GUIDs, each a discovered switch's node GUID | 16 hex digits on all 273 switch ports; 273 of 273 a discovered switch's GUID | PASS |
| U3 | UFM REST LIDs against OpenSM's | every port reports the LID the SM assigned | CA ports 193 of 193, switch ports 273 of 273 (`b83fd2d6d3edea4a`: UFM 58, OpenSM 58; rail-a01-1: 72, 72) | PASS |
| L1 | `dctwin cable pull site-1/hall-a/a01/compute-tray-07:cx8-0` | console `Unlink` at the leaf's port 7; trap 128; the port removed; SUBNET UP; LID 83 out of every LFT | `Unlink "S-fc6a1c0343dddb5e"[7]`; trap 128; `Removed port with GUID:0xb83fd286a49f9171 LID range [83, 83]`; SUBNET UP 532 ms after the pull; LID 83 in 0 LFTs | PASS |
| L2 | After the pull: OpenSM, the diags and UFM | leaf port 7 Down/Polling; 192 CAs; UFM Down/Link Down | as expected (UFM keeps reporting LID 83, which ibsim's port keeps) | PASS |
| L3 | `dctwin cable replace` | console `Link`; Active with the same LID 83 (guid2lid), in all 22 LFTs, out of port 7 at the leaf; 193 CAs; UFM Active, lid 83 | as expected; in every LFT 3.7 s after the replace | PASS |
| L4 | `dctwin power off site-1/hall-a/a01/compute-tray-08` | 4 `Unlink`s (one per rail); OpenSM removes all four HCAs; their LIDs leave every LFT | 4; 4 of 4 removed; 4 of 4 LIDs gone | PASS |
| L5 | `dctwin power on` the same tray | 4 `Link`s; all four Active with the LIDs they had, in every LFT | 4; 4 of 4; 4 of 4 | PASS |
| L6 | Rerouting: `dctwin cable pull site-1/hall-a/c03/backend-ib-rail-a01-1:ib33`, leaf a01-1's uplink to spine 1 | console `Unlink`; leaf a01-1 sends every CA it does not cable out of port 34; every route minimum-hop on the fabric without that cable; a path through spine 1 moves to spine 2 | `Unlink "S-fc6a1c0343dddb5e"[33]`; rerouted 1.4 s after the pull; uplinks 34: 174; 4,246 of 4,246 minimum-hop; LID 83 → 2: rail-a01-1, spine-1, rail-a01-4 before; rail-a01-1, spine-2, rail-a01-4 after | PASS |
| L7 | `dctwin cable replace` of the uplink | console `Link`; both uplinks carry routes again; every route minimum-hop | uplinks 33: 87, 34: 87 after 5.7 s; 4,246 of 4,246 | PASS |
| L8 | `dctwin power ac-cycle site-1/hall-a/c03/backend-ib-rail-a01-1`, the SM's own leaf (a 100 ms outage at the lab's time scale) | 21 `Unlink`s then 21 `Link`s (18 trays, the UFM, 2 uplinks); OpenSM loses its port and re-sweeps; the fabric whole again, equal to the model, every LID kept | 21 and 21; 20 traps, 214 ports removed, `SM port is down`, then `SM port is up`, MASTER, SUBNET UP; whole after 5.4 s; equal to the model; 215 of 215 LIDs kept | PASS |
| R1 | The fidelity report after the run | 54 link changes applied, none refused; UFM REST serving the SM's LIDs | `54 link change(s) applied through the console`; `LIDs the subnet manager assigned, read from ibsim` | PASS |
| C1 | `dctwin down` | the ibsim it started is gone | 0 ibsim processes; the transcript ends with `Quit` | PASS |

CANNOT is a limit of ibsim's preload library, not of the twin.

### What the first run found, and how it was fixed

The first run (2026-10-19, 20 PASS, 3 FINDING, 1 CANNOT) found three things that a projection of
the model could not show. All three are fixed:

- **One SM reached one rail leaf.** The `rail` generator built no spine tier, so the lab's
  `backend-ib` was 20 disjoint subnets. The SM at the UFM's port reached 1 of them, and the other
  174 HCA ports sat in `Initialize` with no LID. Meanwhile UFM REST reported them `Active`. The
  generator now builds `spines` (default 2) and uplinks every rail leaf to every spine.
- **Switch GUIDs were 72 bits.** The QM9700's four-byte `guidPrefix` plus identity/v1's five
  derived bytes made a 72-bit GUID. identity/v1 now derives `8 − len(prefix)` bytes, so every GUID
  is 64 bits. Because HKDF output is prefix-stable, the new GUIDs equal the leading 64 bits of the
  old ones.
- **UFM's LIDs were its own.** The ufm adapter assigned LIDs from the topology. With the ibsim
  adapter running and an SM having assigned LIDs, it now serves the SM's, read from ibsim.

### What the simulation is not

- **Port state.** UFM REST's port states are still the model's: every cabled, powered port is
  `Active`, and there is no `Initialize` or `Armed` while the SM has yet to sweep a port.
- **Node GUIDs.** A channel adapter's node GUID is its port GUID minus one, because ibsim answers
  NodeInfo with PortGUID = NodeGUID + port number. A ConnectX reports equal node and port GUIDs.
  Port GUIDs, which UFM, partitions and NICo use, are the model's.
- **Switch ports.** Switch ports share the switch's GUID, as on real InfiniBand. The model's
  per-port GUIDs of switch ports, which UFM REST serves, are not used.
- **Ids and speeds.** Vendor and device ids are 0, since the model has no PCI ids for fabric
  devices. The GB300 trays' ConnectX-8 ports are XDR in the model but are cabled to NDR QM9700
  ports, so their links train at NDR either way. ibsim 0.12 knows no speed above NDR.
- **Power.** A powered-off host or a switch without AC is simulated as its links unlinked. The
  node does not disappear with its ports.
- **Clients and speed.** ibsim takes at most 10 MAD clients at once: OpenSM is one, and each diag
  another while it runs. Its MAD path is a single process on unix sockets, so the first sweep of
  this 215-node fabric takes OpenSM about 14 s here.

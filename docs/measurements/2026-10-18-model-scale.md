# Measurements — model fidelity at 1,000+ BMCs (2026-10-18)

The first measured unit costs ([16 — Performance targets](../spec/16-testing.md#performance-targets),
[09 — Sizing](../spec/09-runtime.md#sizing)). Host: an Apple-silicon laptop (arm64, macOS),
loopback substrate. Estate: 50 `generic/cpu-rack` racks of 20 `generic/server-2u` servers
(1,000 server BMCs plus 100 PDU BMCs, 52 model switches), generated DMTF Redfish tree, twin DHCP
through the twin relay. Adapters: `redfish`, `dhcp`.

| Target | Spec value | Measured |
|---|---|---|
| DHCP lease storm | 1,000 BMCs lease within 60 s of `up` through the twin relay | **1,100 BMCs leased 2.8 s after start**, every lease relayed with its own option-82 circuit id |
| Redfish p99 under the stack's poll | ≤ 250 ms at 1,000 `model` BMCs polled every 30 s | **p50 0.60 ms, p90 2.3 ms, p99 6.6 ms, max 25 ms** (4,000 requests over two minutes, 0 errors) |
| Redfish burst | — | every BMC at once, 64 concurrent: 1,000 requests in 32 ms, p99 5.6 ms |
| Memory per `model` BMC endpoint | estimated 10–20 MB (09) | **≈0.17 MB**: 191 MB RSS for the whole engine with 1,152 devices and 2,200 endpoints (Redfish over TLS and DHCP) |

Also measured, by the adapter suites:

| What | Measured | Where |
|---|---|---|
| Leases from a real ISC Kea 2.6.2 through the twin relay | 1,036 BMCs in 2.0 s with start-up spread over 2 s; 4.6–12.9 s when every BMC starts at once (Kea drops part of the burst and the clients' retransmissions recover it) | `internal/adapter/dhcp` (`testdata/kea/run-in-container.sh`) |
| A simulated week on the virtual clock | 168 h of the 344-device mixed hall in about 45 s; two runs from one seed give an identical fault and state history | `test/e2e` `TestVirtualWeekReplaysExactly` |
| Redfish on a captured GB300 tray tree | p50 0.2–0.5 ms per resource | `internal/adapter/redfish` `TestLatency` |
| Kubernetes projection | 50 GPU nodes registered 216 ms after `up`; an Xid 79 turns its node NotReady and cordoned in 121 ms (measured before D43: a failed GPU now leaves the node Ready, with `GpuHealthy=False` and one GPU withheld) | `internal/adapter/kwok` |

Not yet measured: event delivery to the console at 200 events/s (the console is not wired to the
control API yet), SONiC-VS and `vm` host costs (no such substrate on this host).

Reproduce: generate the estate as described, then `dctwin up <estate> --adapters redfish,dhcp`,
`dctwin credentials '?class=server'` and the 30 s poller in `scripts/` (see
`docs/implementation.md`).

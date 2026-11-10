# The scenario library

Stack-neutral scenarios by capability ([spec 17 — The scenario library](../../../docs/spec/17-critic.md#the-scenario-library)).
They read a stack only through the standard aliases (`stack.aliases`), which the installed stack
maps in its `critic` section; a scenario whose alias no stack maps is `not-run`, naming the alias.
Every run is judged by the critic, so a scenario asserts what the situation requires and the
policies judge everything else.

| Group | Scenario | Aliases | Estate |
|---|---|---|---|
| ingest | `discovery` | machine | an NVL72 rack |
| leak | `tray-detector`, `rack-point`, `isolation` (run last: it isolates the rack) | machine, alerts | NVL72, DSX Exchange BMS |
| power | `feed-loss` | machine | NVL72 |
| thermal | `cdu-flow-loss` | machine | NVL72 with a CDU loop |
| fabric | `switch-tray-failure` | machine | NVL72 |
| bmc | `unreachable` | machine | NVL72 |
| accelerator | `xid-79` | node | NVL72 |
| lifecycle | `release-sanitise` | allocation, machine | NVL72 |
| storage | `drive-failure` | none (twin-only) | a storage rack |
| compound | `leak-and-bmc` | machine | NVL72 |

The reference stack maps `machine` and `alerts`: on `nvl72-refstack` every scenario but
`accelerator/xid-79` and `lifecycle/release-sanitise` runs, and those two are `not-run`.

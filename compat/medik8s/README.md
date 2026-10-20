# medik8s against the twin

medik8s' fleet-health and remediation pair, built from released tags and run **unmodified**:
[Node Healthcheck Operator](https://github.com/medik8s/node-healthcheck-operator) (NHC) `v0.12.0`
watches the twin's Nodes, and when one stays unhealthy creates a remediation from a template;
[Fence Agents Remediation](https://github.com/medik8s/fence-agents-remediation) (FAR) `v0.8.0`
fences that host through its BMC with the ClusterLabs
[fence agents](https://github.com/ClusterLabs/fence-agents) `v4.17.0`, built from source:
`fence_ipmilan` over IPMI-over-LAN and `fence_redfish` over Redfish, against the twin's `ipmi` and
`redfish` adapters. The twin's event log shows what each agent did to the host.

> **Impersonated.** The Nodes are the twin's `kwok` projection ([D24](../../docs/spec/11-decisions.md)):
> no kubelet runs, so "the Node returns Ready" means the twin's model reached `On` and booted.
> The BMCs are the twin's adapters; no workload runs on the hosts that are fenced.

## What runs

| Process | Source | Role |
|---|---|---|
| `etcd`, `kube-apiserver` 1.37.0 | envtest's binaries (`setup-envtest`) | the cluster |
| `kube-controller-manager` 1.37.0 | `kubernetes/kubernetes` at `v1.37.0`, built from source | node lifecycle (`not-ready`, `unreachable` taints), pod GC for `out-of-service` |
| `dctwin up examples/estates/lab-model.yaml --adapters redfish,ipmi,kwok` | this repository | 50 Nodes; IPMI and Redfish BMCs on loopback ports |
| NHC manager | `medik8s/node-healthcheck-operator` at `v0.12.0` (`a49ec14`), `go build ./cmd`, as a process | the NodeHealthChecks and their webhook |
| FAR manager and the fence agents | `medik8s/fence-agents-remediation` at `v0.8.0` (`78139f2`) for linux, and `ClusterLabs/fence-agents` at `v4.17.0` (`d30fb54`) built with its own autotools (`./autogen.sh && ./configure --with-agents="ipmilan redfish" && make install`) into one image, `far.Containerfile` | the remediations, their webhook, the agents as child processes of the manager |

FAR runs in a container, `dctwin-medik8s-far-<pid>`, because that is how FAR ships: its image is
the manager and the fence-agent packages, the manager runs the agent as a child process, and its
template webhook looks for the agent in `/usr/sbin` of its own filesystem. The container reaches
the apiserver and the twin's BMCs on this machine's loopback: `host.docker.internal` under Docker
Desktop or colima, `host.containers.internal` with pasta's `--map-host-loopback` under rootless
Podman. NHC runs as a process; its webhook server is fixed to `:9443` (`cmd/main.go`), which must
be free.

Both webhooks are registered, from each project's `config/webhook/manifests.yaml` laid out as its
kustomization would (namespace `medik8s`, name prefix), through an `ExternalName` Service for
localhost on the port each server listens on (`../kueue/lib/render.py`), with a certificate made
here and its CA in `caBundle`, as OLM or cert-manager would inject it. CRDs are each project's
`config/crd/bases`.

`manifests/` holds the namespaces and three NodeHealthChecks over disjoint pairs of the twin's
8-GPU servers (unhealthy after 20 s; `minHealthy` 50%), each with its FenceAgentsRemediationTemplate,
which `run.sh` renders with each BMC's endpoint (`--ip`, `--ipport` per node) and a Secret per node
with the BMC credentials the twin reports (`dctwin credentials`):

| NHC | Hosts | Template |
|---|---|---|
| `ipmi-onoff` | b01 (Dell XE9680) | `fence_ipmilan --lanplus`, reboot = power off then on (the agent's default), `ResourceDeletion` |
| `ipmi-cycle` | b05 (Lenovo SR670 V2) | `fence_ipmilan --lanplus --method=cycle` (IPMI chassis power cycle), `ResourceDeletion` |
| `redfish` | b02 (Supermicro SYS-821GE-TNHR) | `fence_redfish --ssl-insecure`, `OutOfServiceTaint` |

Each NHC sets `healthyDelay: 30s` (see Findings: the twin's hosts boot the instant they power on).

## Running it

```bash
make build
DCTWIN=$PWD/bin/dctwin compat/medik8s/run.sh                      # PASS/FAIL table; artefacts under $WORK
WORK=/tmp/medik8s DCTWIN=$PWD/bin/dctwin compat/medik8s/run.sh    # reuse builds and the image
KCM=/path/kube-controller-manager ...                             # prebuilt at v1.37.0
BUILD_ONLY=1 ... ; KEEP=1 ... ; REMOVE_IMAGE=1 ...
```

Needs Go, git, python3, openssl, envtest's binaries and Docker or rootless Podman. The image
`dctwin-medik8s-far:<FAR tag>-fa<fence-agents tag>-<arch>` is built once (Debian bookworm-slim,
the agents built with Debian's toolchain, which is then removed; 240 MB) and kept; `REMOVE_IMAGE=1` removes it on exit.
`$WORK/evidence` keeps the rendered templates and the twin's event log for each remediation.

## Result (2026-10-20, NHC v0.12.0 `a49ec14`, FAR v0.8.0 `78139f2`, fence-agents v4.17.0 `d30fb54`, kube-controller-manager v1.37.0)

On a Linux x86_64 box under rootless Podman, the harness pinned to two cores, the lab estate at its
own time scale (1/20); 8 minutes including the five remediations. Times are seconds after the twin
broke the host, from one run.

| # | Check | Observed | Result |
|---|---|---|---|
| U1, U2 | Nodes; creation taint lifted | 50 Ready; 0 `not-ready` taints after 8.5 s | PASS |
| U3 | NHC running; FAR container running; agents built from source | `fence_ipmilan` and `fence_redfish` report `4.17.0` | PASS |
| U4 | Templates through FAR's admission webhook (in its container) | the three templates admitted; one naming `fence_nosuchagent` refused: `unsupported fence agent` (FAR looks in its image's `/usr/sbin`) | PASS |
| U5 | NodeHealthChecks through NHC's webhook | each Enabled, 2 of 2 Nodes healthy | PASS |
| F1 | `fence_ipmilan -o status` and `fence_redfish -o status` from the FAR container against two of the twin's BMCs | `Status: ON` from both | PASS |
| M1a–d | **IPMI, a host that dies** (`dctwin power off b01/server-02`, a workload bound to it) | NHC creates the remediation at 21 s; FAR taints the Node and runs `fence_ipmilan`; the twin logs `ipmi power.on` and Off → PoweringOn → On; `FenceAgentActionSucceeded` at 25 s; FAR deletes the workload (`ResourceDeletion`), `Succeeded`; Ready at 27 s; NHC ends the remediation, FAR removes its taint and finalizer | PASS |
| M2a–d | **Redfish, a host that dies** (`b02/server-02`) | remediation at 21 s; `fence_redfish` → the twin logs `redfish power.on` (ComputerSystem.Reset); `node.kubernetes.io/out-of-service:NoExecute` added; kube-controller-manager's pod GC deletes the workload; Ready at 23 s; remediation ends | PASS |
| M3a, b, d | **IPMI, a running host whose GPU failed** (Xid 48 on `b01/server-01/gpu-0`; NHC watches `GpuHealthy=False`) | remediation at 21 s; `fence_ipmilan` reboots it: the twin logs `ipmi power.off.force` then `ipmi power.on`; success at 26 s | PASS |
| M3c | Is the GPU reset by that power-off and power-on? | **no**: GpuHealthy stays False, the Xid 48 fault is still on gpu-0, NHC keeps the host unhealthy (the harness then clears the fault; M3d) | FINDING |
| M4a–d | **IPMI chassis power cycle** (same fault on `b05/server-01`, template `--method=cycle`) | the twin logs `ipmi power.cycle`; `device.reset` fires and clears Xid 48; GpuHealthy True; remediation ends at 25 s | PASS |
| M5a–d | **A host that dies with its BMC** (`b05/server-02` off, then `generic/bmc/unreachable`) | remediation at 21 s; `fence_ipmilan` gets no answer: `FenceAgentActionSucceeded=False (FenceAgentFailed)` at 92 s, the twin logs no power request; the same remediation held, host unhealthy, at all 36 samples over 141 s; after the BMC returns and the host is powered on, Ready and the remediation ends | PASS |

26 checks: 25 PASS, 1 FINDING. `$WORK/evidence` keeps each remediation's twin events
(`M1-twin-events.json` …) and the rendered templates; `nhc.log` and `far.log` are under `$WORK/medik8s`.

## Findings

1. **Twin bug, fixed (`internal/adapter/kwok`): a stale Ready transition time after the
   controller manager marked a Node Unknown.** The projection compared each condition with what
   it had last posted itself. A powered-off host posts nothing and renews no lease, so after the
   controller manager's 50 s grace its Node goes `Ready=Unknown`; at power-on the twin posted
   `Ready=False (PoweringOn)` with the transition time it had cached from the power-off. NHC read
   the powering-on host as False since the outage began (`Node matches unhealthy condition … False`
   the instant it powered on), where a real kubelet's status would start a new episode. The
   projection now reads the Node back before the first status it writes for a host that was down
   (`seedConditions`), and NHC logs `Node is going to match unhealthy condition … False` with the
   duration left (M5, compare `nhc.log`). Test: `TestTransitionAfterTheControllerMarkedUnknown`
   (fails without the fix).
2. **Twin, fixed after this run (`internal/engine/power.go`): a power-off then power-on was not a
   reset.** A power-on from Off now fires `device.reset` (a cold boot), so M3c's default reboot
   clears Xid 48 on a re-run. As found:
   `device.reset` fires only when `powerOn` is called with `reset=true`, i.e. for a `power.restart`
   or `power.cycle` request (and an AC cycle's restore); `power.off.force` followed by `power.on`
   fires `device.powerOn` only. Spec 14 defines `device.reset` as "a reset or power cycle completes
   (the On that follows)", and in hardware an off/on is a cold power cycle that resets every GPU.
   So the faults that clear on `device.reset` (nvidia `xid-48`, `xid-94`, `lane-down`; AMD RAS;
   Gaudi HBM) survive FAR's default `reboot` (M3c), Redfish `ForceOff`+`On`, or IPMI `power off`+`power
   on`, and clear on `chassis power cycle` (M4c). A fix: fire `device.reset` on a power-on from Off,
   or let those faults also clear on `device.powerOn`.
3. **Twin, fixed after this run (engine): hosts booted the instant they reached power On.** The
   engine now runs each host's `boot.postSeconds` (scaled) as POST before its OS boots: Redfish
   BootProgress shows it and the Node is Ready only after it, so `HEALTHY_DELAY` need no longer
   stand in for the boot. As found: The profiles' `boot:
   { postSeconds: 150 }` is not read by the engine (no code reads `postSeconds`); `boot()` sets
   `booted: os` at once, so the Node is `Ready` the moment chassis power is On, where a real
   server's kubelet takes minutes. NHC then calls the host healthy and ends the remediation while
   `fence_ipmilan` is still confirming the power-on, and FAR kills the agent: with
   `HEALTHY_DELAY=0s`, M1's agent ends `signal: killed` with no output, FAR never records success
   and the workload is never deleted (22 PASS, 3 FAIL). The harness sets NHC's `healthyDelay: 30s`
   in place of the boot.
4. **Twin, fixed after this run (IPMI adapter): 45 s of PoweringOn read as chassis off.** IPMI now
   reads chassis power on from PoweringOn, as a BMC's rails do. As found: At
   `TIME_SCALE=1` these profiles take `power.onSeconds: 45` to reach On, and the IPMI adapter
   reports chassis power off until then (its fidelity report says so). `fence_ipmilan`'s default
   `--power-timeout` is 20 s: it logs `Timed out waiting to power ON` and still reports `Success:
   Rebooted` (a reboot fences once the host was off). A run at `TIME_SCALE=1`, made before M5 and
   finding 1's fix, passed all its rows but M3c; a real BMC reports chassis power on within
   seconds and spends the minutes in POST (finding 3).
5. **Stack behaviour, for the record.** FAR v0.8.0's remediation taint is
   `remediation.medik8s.io/fence-agents-remediation:NoSchedule` (not NoExecute); NHC v0.12.0 names a
   remediation `<node>-<random>` and carries the node in `remediation.medik8s.io/node-name`; NHC
   treats a condition that changes status mid-outage (the projection's `False` at power-off, the
   controller manager's `Unknown` 50 s later) as one episode ("going to match"), so the outage
   held (M5c) both before and after finding 1's fix.

node-problem-detector was not run (below).

## What this does not show

- node-problem-detector, the other vendor-neutral health source the spec names: it runs on the
  node and reads the kernel log, which on the twin needs the in-host path of `vm` hosts. It was not
  run.
- Self Node Remediation, Machine Deletion Remediation and NHC's escalating remediations; NHC on
  OpenShift (MachineHealthCheck, console plugin).
- That a fenced host's workloads were safe to restart elsewhere: no workload runs.

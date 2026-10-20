# Prometheus against the twin's exporters

[Prometheus](https://github.com/prometheus/prometheus), the official release binary at `v3.15.0`
(downloaded by `run.sh` and checked against the release's `sha256sums.txt`), run **unmodified**,
scrapes the twin's per-host accelerator exporters for the lab estate, evaluates ordinary GPU-fleet
alert rules, and sees faults and power events the harness injects through `dctwin`.

> **Impersonated.** At `model` fidelity no host runs, so DCGM-exporter, the AMD Device Metrics
> Exporter and the Intel Gaudi metric exporter cannot run either; the twin's `metrics` adapter
> answers in their place, with each exporter's metric names, label names and label order at a
> pinned release ([D24](../../docs/spec/11-decisions.md), `internal/adapter/metrics`). Every value
> is the model's and nothing is measured.

## What runs

| Process | Role |
|---|---|
| `dctwin up examples/estates/lab-model.yaml --adapters redfish,kwok,metrics` | 46 exporter endpoints: 42 DCGM (GB300 and GB200 trays, HGX H100, A100), 2 AMD (MI355X), 2 Gaudi 3. `kwok` stays inactive: no kubeconfig is given |
| `prometheus` 3.15.0 | `prometheus.yml`, `rules/accelerators.yml` |

Targets are the exporters' **site names**, `<node>:<exporter port>` (`a01-compute-tray-07:9400`,
`b03-server-01:5000`, `b04-server-01:41611`), as a stack's Prometheus would address them, written
by `targets.py` from `dctwin endpoints` into file-SD files. Prometheus reaches them through the
twin's site gateway (`proxy_url`), which routes only to estate endpoints. A host that is off has no
exporter: the gateway's upstream connection is dropped and the target goes `up == 0`.

`rules/accelerators.yml` knows nothing about dc-twin: Xid (`DCGM_FI_DEV_XID_ERRORS > 0`, `== 79`),
double-bit ECC, GPU temperature, a GPU missing telemetry while its host still answers, AMD
`gpu_health == 0` and uncorrectable ECC, Gaudi HBM pending rows and NIC ports down, and
`up == 0`. `promtool check config` passes; the run keeps its output.

## Running it

```bash
make build
DCTWIN=$PWD/bin/dctwin compat/prometheus/run.sh                     # downloads Prometheus once into $WORK
WORK=/tmp/prom DCTWIN=$PWD/bin/dctwin compat/prometheus/run.sh
PROMETHEUS=/path/to/prometheus DCTWIN=$PWD/bin/dctwin compat/prometheus/run.sh
KEEP=1 ...                                                          # leave the twin and Prometheus up
```

Needs curl, python3 and `shasum`. Prometheus listens on a free loopback port; `$WORK/evidence`
keeps the targets, the PromQL results and the firing alerts at each step.

## Result (2026-10-18, Prometheus 3.15.0 darwin-arm64)

| # | Check | Observed | Result |
|---|---|---|---|
| P1 | Targets from `dctwin endpoints` | dcgm 42, amd 2, gaudi 2 | PASS |
| P2 | `sum(up)` | 46 | PASS |
| P3 | `DCGM_FI_DEV_GPU_TEMP`, one series per GPU | 192: by `modelName` NVIDIA GB300 72, NVIDIA GB200 72, NVIDIA H100 80GB HBM3 32, NVIDIA A100-SXM4-80GB 16 | PASS |
| P3b | `modelName` written as GPU Feature Discovery writes `nvidia.com/gpu.product` | NVIDIA-A100-SXM4-80GB, NVIDIA-GB200, NVIDIA-GB300, NVIDIA-H100-80GB-HBM3: the kwok Nodes' products | PASS |
| P4 | AMD `gpu_junction_temperature`, `gpu_nodes_total` | 16 series; 16 GPUs | PASS |
| P5 | Gaudi `habanalabs_temperature_onchip`, `habanalabs_nic_port_status` | 16 devices; 384 ports (16 × 24) | PASS |
| P6 | Recording rule `instance:dcgm_gpu_temp:max` | 42 series | PASS |
| P7 | No alert on the healthy estate | GPUXidError, AMDGPUUnhealthy, GaudiHBMUncorrectable, ExporterDown: 0 | PASS |
| F1 | `dctwin fault inject a01/compute-tray-07/gpu-2 nvidia/xid/xid-79` | `DCGM_FI_DEV_XID_ERRORS{gpu="2"} 79`, `err_msg="GPU has fallen off the bus"`, within one scrape (4.7 s) | PASS |
| F2 | Alerts `GPUXidError`, `GPUFellOffTheBus` | both firing | PASS |
| F3 | GPU off the bus stops reporting | 3 of 4 temperature series on the tray; `GPUMissingTelemetry` firing | PASS |
| F4 | AMD RAS uncorrectable ECC on `b03/server-01/gpu-3` | `gpu_health{gpu_id="3"} 0`, `gpu_ecc_uncorrect_total 1`, `AMDGPUUnhealthy` firing | PASS |
| F5 | Gaudi HBM uncorrectable on `b04/server-01/hpu-5` | `habanalabs_pending_rows_with_double_bit_ecc_errors{id="5"} 1`, alert firing | PASS |
| F6 | Gaudi RoCE ports down on `b04/server-02/hpu-1` | 21 internal ports of 24 at 0, `GaudiPortsDown` firing | PASS |
| F7 | `dctwin power off b01/server-02` | `up{instance="b01-server-02:9400"} 0`, `ExporterDown` firing | PASS |
| R1 | `dctwin fault clear` (Xid 79) | `DCGM_FI_DEV_XID_ERRORS{gpu="2"} 0`, `GPUXidError` resolved | PASS |
| R2 | `dctwin power on b01/server-02` | `up` back to 1 once the host has booted (3–4 s) | PASS |

17 checks: 17 PASS (the rerun after D43; the first run had 16 checks, without P3b). Times are from one run on a laptop and vary from run to run.

```text
# PromQL after the Xid 79 injection
DCGM_FI_DEV_XID_ERRORS{UUID="GPU-ddd21035-…", device="nvidia2", err_code="79", err_msg="GPU has fallen off the bus",
  gpu="2", hostname="a01-compute-tray-07", instance="a01-compute-tray-07:9400", job="dcgm-exporter",
  modelName="NVIDIA GB300", pci_bus_id="00000000:38:00.0"} 79
ALERTS{alertname="GPUMissingTelemetry", alertstate="firing", instance="a01-compute-tray-07:9400"} 1
ALERTS{alertname="ExporterDown", alertstate="firing", instance="b01-server-02:9400", job="dcgm-exporter"} 1
```

## What this does not show

- Any measured value. Every accelerator on a host reports the host's `accelerator-max` sensor
  (62.5 °C for every GB300 GPU above), power is a share of the profile's curve, and fields the
  model has no source for (clocks, energy, PCIe, NVLink traffic, profiling metrics) are absent. The
  `metrics` adapter's fidelity report lists these.
- DCGM's own Xid semantics: the twin returns the Xid series to 0 when the fault clears; DCGM keeps
  the last Xid until the driver reloads.
- Exporter deployment: no DaemonSet, ServiceMonitor or pod-attribution labels (the exporters'
  `pod`/`namespace` labels are empty because no pod runs).

## Twin issue the first run showed, fixed

The first run found `modelName` disagreeing with the Node's `nvidia.com/gpu.product` on three
profiles (`NVIDIA B300` / `NVIDIA-GB300`, `NVIDIA B200` / `NVIDIA-GB200`, `NVIDIA A100 80GB` /
`NVIDIA-A100-SXM4-80GB`). On a host both are NVML's device name, GPU Feature Discovery writing its
spaces as dashes, so a join of DCGM series to Nodes by product found nothing for those hosts.
Since [D43](../../docs/spec/11-decisions.md) an NVIDIA accelerator's `model` is its NVML name and
the kwok projection derives the GFD labels from it; P3b checks the two agree.


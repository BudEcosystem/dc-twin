#!/usr/bin/env bash
# Runs Prometheus, the official release binary at a pinned version, unmodified, against the twin's
# in-host exporters: DCGM-exporter, the AMD Device Metrics Exporter and the Intel Gaudi metric
# exporter, one per model host of the lab estate (internal/adapter/metrics, IMPERSONATED: D24 —
# no host runs, so the twin answers in each exporter's place with its metric and label names).
# Prometheus scrapes them by site name through the twin's site gateway, evaluates the alert rules
# in rules/, and the harness injects faults and power events and reads back PromQL and alerts.
#
#   compat/prometheus/run.sh                       # PASS/FAIL table on stdout, artefacts under $WORK
#   WORK=/tmp/prom DCTWIN=bin/dctwin compat/prometheus/run.sh
#   PROMETHEUS=/path/to/prometheus compat/prometheus/run.sh     # skip the download
#   KEEP=1 compat/prometheus/run.sh                # leave the twin and Prometheus running
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
PROM_VERSION=${PROM_VERSION:-3.15.0}
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
ADAPTERS=${ADAPTERS:-redfish,kwok,metrics}   # kwok stays inactive: no kubeconfig is given here
ESTATE=${ESTATE:-$root/examples/estates/lab-model.yaml}   # the checks name its racks and hosts
mkdir -p "$WORK/evidence"
WORK=$(cd "$WORK" && pwd)

. "$here/../kai/lib/common.sh"
. "$here/../kai/lib/twin.sh"
command -v "$DCTWIN" >/dev/null || { echo "dctwin not found: set DCTWIN (make build; DCTWIN=bin/dctwin)" >&2; exit 1; }
DCTWIN=$(command -v "$DCTWIN")

# ---------------------------------------------------------------- Prometheus, the release binary

if [ -z "${PROMETHEUS:-}" ]; then
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	case "$(uname -m)" in arm64 | aarch64) arch=arm64 ;; x86_64) arch=amd64 ;; *) arch=$(uname -m) ;; esac
	name="prometheus-$PROM_VERSION.$os-$arch"
	dl="$WORK/download"
	if [ ! -x "$WORK/$name/prometheus" ]; then
		mkdir -p "$dl"
		base="https://github.com/prometheus/prometheus/releases/download/v$PROM_VERSION"
		curl -fsSL -o "$dl/$name.tar.gz" "$base/$name.tar.gz"
		curl -fsSL -o "$dl/sha256sums.txt" "$base/sha256sums.txt"
		want=$(awk -v f="$name.tar.gz" '$2 == f { print $1 }' "$dl/sha256sums.txt")
		got=$(shasum -a 256 "$dl/$name.tar.gz" | awk '{ print $1 }')
		[ -n "$want" ] && [ "$want" = "$got" ] || { echo "checksum mismatch for $name.tar.gz: want $want got $got" >&2; exit 1; }
		tar -xzf "$dl/$name.tar.gz" -C "$WORK"
	fi
	PROMETHEUS="$WORK/$name/prometheus"
fi
PROMTOOL=${PROMTOOL:-$(dirname "$PROMETHEUS")/promtool}
PROM_VERSION_LINE=$("$PROMETHEUS" --version 2>&1 | head -1)

# ---------------------------------------------------------------- bring-up

PROM_PID=
cleanup() {
	set +e
	if [ "${KEEP:-}" = 1 ]; then
		echo "KEEP=1: left running. Prometheus $PROM_URL (pid $PROM_PID); stop with: kill $PROM_PID; DCTWIN_STATE=$TWIN_STATE HOME=$TWIN_HOME $DCTWIN down" >&2
		return
	fi
	[ -n "$PROM_PID" ] && kill "$PROM_PID" 2>/dev/null && wait "$PROM_PID" 2>/dev/null
	twin_down
}
trap cleanup EXIT
trap 'exit 130' INT TERM

rm -rf "$WORK/twin" "$WORK/prom"
twin_up "$ESTATE" "$WORK/twin" "$ADAPTERS"
twin endpoints >"$WORK/evidence/endpoints.json"
gateway=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["gateway"])' "$WORK/evidence/endpoints.json")

mkdir -p "$WORK/prom"
cp -R "$here/rules" "$WORK/prom/"
sed "s|__GATEWAY__|$gateway|" "$here/prometheus.yml" >"$WORK/prom/prometheus.yml"
python3 -I "$here/targets.py" "$WORK/prom/targets" <"$WORK/evidence/endpoints.json" >"$WORK/evidence/targets.txt"
"$PROMTOOL" check config "$WORK/prom/prometheus.yml" >"$WORK/evidence/promtool-check.txt" 2>&1
port=$(free_port)
PROM_URL="http://127.0.0.1:$port"
"$PROMETHEUS" --config.file="$WORK/prom/prometheus.yml" --storage.tsdb.path="$WORK/prom/data" \
	--web.listen-address="127.0.0.1:$port" >"$WORK/prom/prometheus.log" 2>&1 &
PROM_PID=$!
ready() { curl -sf "$PROM_URL/-/ready" >/dev/null; }
wait_until 60 ready >/dev/null || { tail -20 "$WORK/prom/prometheus.log" >&2; exit 1; }
echo "$PROM_VERSION_LINE at $PROM_URL; site gateway $gateway" >&2

pq() { python3 -I "$here/promq.py" "$PROM_URL" "$@"; }
# evidence <file> <promql>: keep what a query returned
evidence() { { echo "# $2"; pq query "$2"; } >>"$WORK/evidence/$1" 2>&1 || true; }
value_is() { [ "$(pq value "$1")" = "$2" ]; }
count_is() { [ "$(pq count "$1")" = "$2" ]; }
firing() { [ "$(pq alerts "$1" | head -1)" -ge "${2:-1}" ]; }
not_firing() { [ "$(pq alerts "$1" | head -1)" = 0 ]; }

# ---------------------------------------------------------------- P: scraping a healthy estate

t=$(tr '\n' ' ' <"$WORK/evidence/targets.txt")
record P1 "Scrape targets from dctwin endpoints (site names, via the site gateway)" "dcgm 42, amd 2, gaudi 2" "${t% }" "$(grep -q 'dcgm: 42' "$WORK/evidence/targets.txt" && grep -q 'amd: 2' "$WORK/evidence/targets.txt" && grep -q 'gaudi: 2' "$WORK/evidence/targets.txt" && echo PASS || echo FAIL)"

ms=$(wait_until 60 value_is 'sum(up)' 46) || true
record P2 "sum(up) over every exporter" "46" "$(pq value 'sum(up)') after ${ms} ms" "$( [ "$(pq value 'sum(up)')" = 46 ] && echo PASS || echo FAIL)"
evidence P2-up.txt 'sum by (job) (up)'

n=$(pq count 'DCGM_FI_DEV_GPU_TEMP')
by=$(pq query 'count by (modelName) (DCGM_FI_DEV_GPU_TEMP)' | sed -E 's/.*modelName="([^"]*)"\} ([0-9]+)/\1: \2/' | sort | tr '\n' ',' | sed 's/,$//; s/,/, /g')
record P3 "DCGM_FI_DEV_GPU_TEMP, one series per GPU" "192 (72 GB300 + 72 GB200 tray GPUs, 32 H100, 16 A100)" "$n ($by)" "$( [ "$n" = 192 ] && echo PASS || echo FAIL)"
evidence P3-dcgm-temp.txt 'DCGM_FI_DEV_GPU_TEMP{instance="a01-compute-tray-07:9400"}'
evidence P3-dcgm-temp.txt 'count by (modelName) (DCGM_FI_DEV_GPU_TEMP)'

# On a host, modelName and GPU Feature Discovery's nvidia.com/gpu.product are one NVML name (GFD
# writes its spaces as dashes): a join of DCGM series to Nodes by product depends on it.
names=$(pq query 'count by (modelName) (DCGM_FI_DEV_GPU_TEMP)' | sed -E 's/.*modelName="([^"]*)".*/\1/' | tr ' ' '-' | sort | tr '\n' ' ')
want="NVIDIA-A100-SXM4-80GB NVIDIA-GB200 NVIDIA-GB300 NVIDIA-H100-80GB-HBM3 "
record P3b "modelName, written as GFD writes nvidia.com/gpu.product" "${want% } (the kwok Nodes' products)" "${names% }" "$( [ "$names" = "$want" ] && echo PASS || echo FAIL)"

a=$(pq count 'gpu_junction_temperature') g=$(pq value 'sum(gpu_nodes_total)')
record P4 "AMD exporter: gpu_junction_temperature per GPU, gpu_nodes_total per host" "16 series; sum(gpu_nodes_total) 16" "$a series; sum(gpu_nodes_total) $g" "$( [ "$a" = 16 ] && [ "$g" = 16 ] && echo PASS || echo FAIL)"
evidence P4-amd.txt 'gpu_junction_temperature{instance="b03-server-01:5000"}'
evidence P4-amd.txt 'gpu_health'

h=$(pq count 'habanalabs_temperature_onchip') p=$(pq count 'habanalabs_nic_port_status')
record P5 "Gaudi exporter: habanalabs_temperature_onchip per device, NIC port status" "16 devices; 384 ports (16 x 24)" "$h devices; $p ports" "$( [ "$h" = 16 ] && [ "$p" = 384 ] && echo PASS || echo FAIL)"
evidence P5-gaudi.txt 'habanalabs_temperature_onchip{instance="b04-server-01:41611"}'

ms=$(wait_until 30 count_is 'instance:dcgm_gpu_temp:max' 42) || true
record P6 "Recording rule instance:dcgm_gpu_temp:max" "42 series (one per DCGM host)" "$(pq count 'instance:dcgm_gpu_temp:max') after ${ms} ms" "$( [ "$(pq count 'instance:dcgm_gpu_temp:max')" = 42 ] && echo PASS || echo FAIL)"
record P7 "No GPU alert firing on the healthy estate" "GPUXidError, AMDGPUUnhealthy, GaudiHBMUncorrectable, ExporterDown: 0" "$(pq alerts GPUXidError | head -1), $(pq alerts AMDGPUUnhealthy | head -1), $(pq alerts GaudiHBMUncorrectable | head -1), $(pq alerts ExporterDown | head -1)" "$(not_firing GPUXidError && not_firing AMDGPUUnhealthy && not_firing GaudiHBMUncorrectable && not_firing ExporterDown && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- F: faults injected in the twin

XID='DCGM_FI_DEV_XID_ERRORS{instance="a01-compute-tray-07:9400",gpu="2"}'
twin fault inject 'a01/compute-tray-07/gpu-2' nvidia/xid/xid-79 >"$WORK/evidence/F1-inject.json"
ms=$(wait_until 30 value_is "$XID" 79) || true
msg=$(pq query "$XID" | grep -o 'err_msg="[^"]*"' || true)
record F1 "dctwin fault inject a01/compute-tray-07/gpu-2 nvidia/xid/xid-79" "$XID = 79, err_msg \"GPU has fallen off the bus\"" "$(pq value "$XID"), $msg, after ${ms} ms" "$( [ "$(pq value "$XID")" = 79 ] && echo PASS || echo FAIL)"
evidence F1-xid.txt "$XID"
ms=$(wait_until 30 firing GPUXidError) || true
record F2 "Alert GPUXidError (DCGM_FI_DEV_XID_ERRORS > 0) and GPUFellOffTheBus (== 79)" "both firing for a01-compute-tray-07 gpu 2" "GPUXidError $(pq alerts GPUXidError | head -1), GPUFellOffTheBus $(pq alerts GPUFellOffTheBus | head -1) firing, after ${ms} ms" "$(firing GPUXidError && firing GPUFellOffTheBus && echo PASS || echo FAIL)"
pq alerts GPUXidError >"$WORK/evidence/F2-alerts.txt" 2>&1 || true
pq alerts GPUFellOffTheBus >>"$WORK/evidence/F2-alerts.txt" 2>&1 || true
wait_until 20 firing GPUMissingTelemetry >/dev/null || true
record F3 "GPU off the bus stops reporting telemetry" "3 DCGM_FI_DEV_GPU_TEMP series on the tray (of 4); GPUMissingTelemetry firing" "$(pq count 'DCGM_FI_DEV_GPU_TEMP{instance="a01-compute-tray-07:9400"}') series; GPUMissingTelemetry $(pq alerts GPUMissingTelemetry | head -1)" "$( [ "$(pq count 'DCGM_FI_DEV_GPU_TEMP{instance="a01-compute-tray-07:9400"}')" = 3 ] && firing GPUMissingTelemetry && echo PASS || echo FAIL)"

AMD='gpu_health{instance="b03-server-01:5000",gpu_id="3"}'
twin fault inject 'b03/server-01/gpu-3' amd/ras/uncorrectable-ecc >/dev/null
ms=$(wait_until 30 value_is "$AMD" 0) || true
wait_until 20 firing AMDGPUUnhealthy >/dev/null || true
record F4 "dctwin fault inject b03/server-01/gpu-3 amd/ras/uncorrectable-ecc" "$AMD = 0; gpu_ecc_uncorrect_total 1; AMDGPUUnhealthy firing" "$(pq value "$AMD") after ${ms} ms; uncorrect $(pq value 'gpu_ecc_uncorrect_total{instance="b03-server-01:5000",gpu_id="3"}'); AMDGPUUnhealthy $(pq alerts AMDGPUUnhealthy | head -1)" "$( [ "$(pq value "$AMD")" = 0 ] && firing AMDGPUUnhealthy && echo PASS || echo FAIL)"
evidence F4-amd.txt 'gpu_ecc_uncorrect_total > 0'

HBM='habanalabs_pending_rows_with_double_bit_ecc_errors{instance="b04-server-01:41611",id="5"}'
twin fault inject 'b04/server-01/hpu-5' intel/gaudi/hbm-uncorrectable >/dev/null
ms=$(wait_until 30 value_is "$HBM" 1) || true
wait_until 20 firing GaudiHBMUncorrectable >/dev/null || true
record F5 "dctwin fault inject b04/server-01/hpu-5 intel/gaudi/hbm-uncorrectable" "$HBM = 1; GaudiHBMUncorrectable firing" "$(pq value "$HBM") after ${ms} ms; GaudiHBMUncorrectable $(pq alerts GaudiHBMUncorrectable | head -1)" "$( [ "$(pq value "$HBM")" = 1 ] && firing GaudiHBMUncorrectable && echo PASS || echo FAIL)"

twin fault inject 'b04/server-02/hpu-1' intel/gaudi/roce-port-down >/dev/null
PORTS='count by (instance, id) (habanalabs_nic_port_status{instance="b04-server-02:41611",id="1"} == 0)'
ms=$(wait_until 30 value_is "$PORTS" 21) || true
wait_until 20 firing GaudiPortsDown >/dev/null || true
record F6 "dctwin fault inject b04/server-02/hpu-1 intel/gaudi/roce-port-down" "21 internal (scale-up) ports of 24 at 0; GaudiPortsDown firing" "$(pq value "$PORTS") ports down after ${ms} ms; GaudiPortsDown $(pq alerts GaudiPortsDown | head -1)" "$( [ "$(pq value "$PORTS")" = 21 ] && firing GaudiPortsDown && echo PASS || echo FAIL)"

UP='up{instance="b01-server-02:9400"}'
twin power off 'b01/server-02' >/dev/null
ms=$(wait_until 30 value_is "$UP" 0) || true
wait_until 20 firing ExporterDown >/dev/null || true
record F7 "dctwin power off b01/server-02" "$UP = 0 (the exporter does not answer); ExporterDown firing" "$(pq value "$UP") after ${ms} ms; ExporterDown $(pq alerts ExporterDown | head -1)" "$( [ "$(pq value "$UP")" = 0 ] && firing ExporterDown && echo PASS || echo FAIL)"
pq query 'ALERTS{alertstate="firing"}' >"$WORK/evidence/F-alerts-firing.txt" 2>&1 || true

# ---------------------------------------------------------------- R: recovery

twin fault clear 'a01/compute-tray-07/gpu-2' nvidia/xid/xid-79 >/dev/null
ms=$(wait_until 30 value_is "$XID" 0) || true
wait_until 20 not_firing GPUXidError >/dev/null || true
record R1 "dctwin fault clear (Xid 79)" "$XID back to 0; GPUXidError no longer firing" "$(pq value "$XID") after ${ms} ms; GPUXidError $(pq alerts GPUXidError | head -1) firing" "$( [ "$(pq value "$XID")" = 0 ] && not_firing GPUXidError && echo PASS || echo FAIL)"
twin power on 'b01/server-02' >/dev/null
ms=$(wait_until 90 value_is "$UP" 1) || true
record R2 "dctwin power on b01/server-02" "$UP back to 1 once the host has booted" "$(pq value "$UP") after ${ms} ms" "$( [ "$(pq value "$UP")" = 1 ] && echo PASS || echo FAIL)"

twin report >"$WORK/evidence/fidelity-report.txt" 2>&1 || true
status=0
print_table "$PROM_VERSION_LINE against dc-twin ($(basename "$ESTATE")). Exporters are the twin's metrics projection: IMPERSONATED (D24); nothing is measured." || status=$?
echo
echo "artefacts: $WORK/evidence (PromQL results, alerts, targets), Prometheus config and log: $WORK/prom"
exit $status

#!/usr/bin/env bash
# Runs NVIDIA KAI Scheduler, unmodified and built from a released tag, against the twin's
# Kubernetes view of the lab estate: a real kube-apiserver and etcd (envtest's binaries), the real
# kube-controller-manager, and dc-twin's kwok projection standing in for 50 kubelets
# (IMPERSONATED, D24). KAI binds GPU, AMD and Gaudi pods onto those Nodes, places an NVL72 gang in
# one NVLink domain, and avoids trays and servers the twin faults or powers off.
#
#   compat/kai/run.sh                      # PASS/FAIL table on stdout, artefacts under $WORK
#   WORK=/tmp/kai DCTWIN=bin/dctwin compat/kai/run.sh
#   KCM=none compat/kai/run.sh             # no kube-controller-manager (see README: Modes)
#   KCM=/path/kube-controller-manager compat/kai/run.sh
#   KEEP=1 compat/kai/run.sh               # leave everything running; prints how to stop it
#
# Nothing here reads ~/.kube/config: every client is given the scratch cluster's kubeconfig.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
KAI_REF=${KAI_REF:-v0.18.6}           # github.com/kai-scheduler/KAI-Scheduler (was NVIDIA/KAI-Scheduler)
K8S_REF=${K8S_REF:-v1.37.0}           # kube-controller-manager, at the envtest apiserver's version
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
KCM=${KCM:-build}                     # build | none | <path to kube-controller-manager>
ADAPTERS=${ADAPTERS:-redfish,kwok,metrics}
ESTATE=${ESTATE:-$root/examples/estates/lab-model.yaml}   # the checks name its racks and hosts
mkdir -p "$WORK/bin" "$WORK/src" "$WORK/evidence"
WORK=$(cd "$WORK" && pwd)

. "$here/lib/common.sh"
. "$here/lib/cluster.sh"
. "$here/lib/twin.sh"

ENVTEST_DIR=$(envtest_dir)
[ -x "$ENVTEST_DIR/kube-apiserver" ] || { echo "no envtest binaries: set KUBEBUILDER_ASSETS (setup-envtest use -p path)" >&2; exit 1; }
KUBECTL=${KUBECTL:-$ENVTEST_DIR/kubectl}
export ENVTEST_DIR KUBECTL
command -v "$DCTWIN" >/dev/null || { echo "dctwin not found: set DCTWIN (make build; DCTWIN=bin/dctwin)" >&2; exit 1; }
DCTWIN=$(command -v "$DCTWIN")

# ---------------------------------------------------------------- build from source

KAI_COMPONENTS="scheduler binder podgrouper queuecontroller podgroupcontroller"
if [ ! -d "$WORK/src/kai-scheduler/.git" ]; then
	git clone -q --depth 1 --branch "$KAI_REF" https://github.com/kai-scheduler/KAI-Scheduler.git "$WORK/src/kai-scheduler"
fi
for c in $KAI_COMPONENTS; do
	if [ ! -x "$WORK/bin/kai-$c" ]; then
		echo "building KAI $c at $KAI_REF" >&2
		(cd "$WORK/src/kai-scheduler" && go build -o "$WORK/bin/kai-$c" "./cmd/$c")
	fi
done
KAI_COMMIT=$(git -C "$WORK/src/kai-scheduler" rev-parse HEAD)

case "$KCM" in
none) KCM_BIN= ;;
build)
	KCM_BIN="$WORK/bin/kube-controller-manager"
	if [ ! -x "$KCM_BIN" ]; then
		if [ ! -d "$WORK/src/kubernetes/.git" ]; then
			git clone -q --depth 1 --branch "$K8S_REF" https://github.com/kubernetes/kubernetes.git "$WORK/src/kubernetes"
		fi
		echo "building kube-controller-manager $K8S_REF (once; about ten minutes)" >&2
		(cd "$WORK/src/kubernetes" && go build -o "$KCM_BIN" ./cmd/kube-controller-manager)
	fi
	;;
*) KCM_BIN=$KCM ;;
esac

# ---------------------------------------------------------------- bring-up

PIDS=""
cleanup() {
	set +e
	if [ "${KEEP:-}" = 1 ]; then
		echo "KEEP=1: left running. Stop with: kill $PIDS; DCTWIN_STATE=$TWIN_STATE HOME=$TWIN_HOME $DCTWIN down; kill $CLUSTER_PIDS" >&2
		return
	fi
	local p
	for p in $PIDS; do kill "$p" 2>/dev/null; done
	for p in $PIDS; do wait "$p" 2>/dev/null; done
	twin_down
	cluster_down
}
trap cleanup EXIT
trap 'exit 130' INT TERM

rm -rf "$WORK/cluster" "$WORK/twin" "$WORK/kai"
cluster_up "$WORK/cluster" "$KCM_BIN"
echo "kube-apiserver $APISERVER_URL (envtest $(basename "$ENVTEST_DIR")); kube-controller-manager: ${KCM_BIN:-none}" >&2
kc apply --server-side -f "$WORK/src/kai-scheduler/deployments/kai-scheduler/crds/" >/dev/null
crds_established() { # every KAI CRD Established (a CRD has no conditions for its first moments)
	[ "$(kc get crd -o 'jsonpath={range .items[*]}{.status.conditions[?(@.type=="Established")].status}{"\n"}{end}' | grep -c True)" -ge 6 ]
}
wait_until 60 crds_established >/dev/null || { echo "KAI CRDs not established" >&2; exit 1; }
kc apply -f "$here/manifests/00-namespaces.yaml" -f "$here/manifests/01-queues.yaml" \
	-f "$here/manifests/02-topology.yaml" -f "$here/manifests/03-priorityclasses.yaml" >/dev/null

twin_up "$ESTATE" "$WORK/twin" "$ADAPTERS" "$KUBECONFIG_FILE"
echo "twin up: $(grep -o 'control API: [^ ]*' "$WORK/twin/up.log"), state $TWIN_STATE" >&2

# KAI's components as processes, each with the scratch kubeconfig and its own loopback ports.
mkdir -p "$WORK/kai"
start() { # start <name> <args...>
	local name=$1
	shift
	KUBECONFIG="$KUBECONFIG_FILE" HOME="$WORK/twin/home" "$WORK/bin/kai-$name" "$@" >"$WORK/kai/$name.log" 2>&1 &
	PIDS="$PIDS $!"
}
start scheduler --scheduler-name kai-scheduler --namespace kai-scheduler \
	--listen-address "127.0.0.1:$(free_port)" --plugin-server-port "$(free_port)"
start binder --scheduler-name kai-scheduler --resource-reservation-namespace kai-resource-reservation \
	--metrics-bind-address "127.0.0.1:$(free_port)" --health-probe-bind-address "127.0.0.1:$(free_port)"
start podgrouper --scheduler-name kai-scheduler \
	--metrics-bind-address "127.0.0.1:$(free_port)" --health-probe-bind-address "127.0.0.1:$(free_port)"
start queuecontroller --enable-webhook=false --metrics-listen-address "127.0.0.1:$(free_port)"
# KAI v0.18.6's podgroupcontroller accepts --metrics-bind-address and ignores it: it binds :8080
# on every interface whatever it is given (found by compat/grove), which would take a port this
# machine's own services may use. It only maintains PodGroup status, so it runs only when asked
# (KAI_PODGROUPCONTROLLER=1) and only while nothing listens on 8080.
KAI_PODGROUPCONTROLLER=${KAI_PODGROUPCONTROLLER:-0}
KAI_RUNNING=4
if [ "$KAI_PODGROUPCONTROLLER" = 1 ]; then
	if (exec 3<>/dev/tcp/127.0.0.1/8080) 2>/dev/null; then
		echo "KAI_PODGROUPCONTROLLER=1, but something listens on 127.0.0.1:8080 and KAI's podgroupcontroller binds :8080 regardless of its flags; not starting it" >&2
	else
		start podgroupcontroller --scheduler-name kai-scheduler --enable-podgroup-webhook=false \
			--metrics-bind-address "127.0.0.1:$(free_port)" --health-probe-bind-address "127.0.0.1:$(free_port)"
		KAI_RUNNING=5
	fi
fi

# ---------------------------------------------------------------- helpers

NS=dc-twin-kai
render_pod() { # name workload queue resource count selector [annotations]
	sed -e "s|@NAME@|$1|" -e "s|@WORKLOAD@|$2|" -e "s|@QUEUE@|$3|" -e "s|@RESOURCE@|$4|" \
		-e "s|@COUNT@|$5|" -e "s|@SELECTOR@|$6|" -e "s|@ANNOTATIONS@|${7:-}|" \
		"$here/manifests/workloads/pod.yaml.tmpl"
}
submit_pod() { # name queue resource count selector
	render_pod "$1" "$1" "$2" "$3" "$4" "$5" | tee "$WORK/evidence/pod-$1.yaml" | kc apply -f - >/dev/null
}
submit_gang() { # name size gpus-per-pod selector
	local name=$1 n=$2 g=$3 sel=$4 i f="$WORK/evidence/gang-$1.yaml"
	sed -e "s|@NAME@|$name|" -e "s|@QUEUE@|training|" -e "s|@MIN@|$n|" "$here/manifests/workloads/podgroup.yaml.tmpl" >"$f"
	for i in $(seq -w 1 "$n"); do
		echo "---" >>"$f"
		render_pod "$name-$i" "$name" training nvidia.com/gpu "$g" "$sel" \
			"pod-group-name: $name, kai.scheduler/skip-podgrouper: \"true\"" >>"$f"
	done
	kc apply -f "$f" >/dev/null
}
# Pods bound to a Node with no kubelet never finish terminating; delete them as a kubelet that
# confirmed the kill would.
delete_workload() { kc delete pods -n "$NS" -l "dc-twin.io/workload=$1" --force --grace-period=0 >/dev/null 2>&1 || true; }
node_of() { kc get pod -n "$NS" "$1" -o jsonpath='{.spec.nodeName}'; }
bound_nodes() { kc get pods -n "$NS" -l "dc-twin.io/workload=$1" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | grep . || true; }
bound_count() { bound_nodes "$1" | grep -c . || true; }
label_of() { kc get node "$1" -o "jsonpath={.metadata.labels.$2}"; }
ready_of() { kc get node "$1" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].reason}'; }
alloc_of() { kc get node "$1" -o "jsonpath={.status.allocatable.$2}"; }
has_count() { [ "$(bound_count "$1")" = "$2" ]; }
is_bound() { [ -n "$(node_of "$1")" ]; }
cliques_of() { local n; for n in $(bound_nodes "$1"); do label_of "$n" 'nvidia\.com/gpu\.clique'; echo; done | sort -u | grep -c . || true; }
taints_of() { kc get node "$1" -o 'jsonpath={range .spec.taints[*]}{.key}:{.effect} {end}'; }
podgroup_reason() { kc get podgroup -n "$NS" "$1" -o 'jsonpath={.status.schedulingConditions[0].reasons[0].message}' 2>/dev/null; }
ok() { "$@" >/dev/null 2>&1 && echo 0 || echo 1; }

# ---------------------------------------------------------------- U: the twin's Nodes, as KAI sees them

hosts=50
node_count_is() { [ "$(kc get nodes -o name | grep -c .)" -eq "$1" ]; }
ms=$(wait_until 60 node_count_is $hosts) || true
ready=$(kc get nodes -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c True || true)
imp=$(kc get nodes -o jsonpath='{range .items[*]}{.metadata.annotations.dc-twin\.io/impersonated}{"\n"}{end}' | grep -c kwok || true)
record U1 "Nodes the twin registers (kwok projection)" "$hosts Ready, all dc-twin.io/impersonated=kwok" "$ready Ready, $imp impersonated, after ${ms} ms" "$( [ "$ready" = $hosts ] && [ "$imp" = $hosts ] && echo PASS || echo FAIL)"
kc get nodes -L nvidia.com/gpu.product,amd.com/gpu.product-name,habana.ai/gaudi.product,nvidia.com/gpu.clique >"$WORK/evidence/nodes.txt"

sum_res() { kc get nodes -o "jsonpath={range .items[*]}{.status.allocatable.$1}{\"\\n\"}{end}" | awk '{s+=$1} END {print s+0}'; }
nv=$(sum_res 'nvidia\.com/gpu') amd=$(sum_res 'amd\.com/gpu') hpu=$(sum_res 'habana\.ai/gaudi')
record U2 "Accelerator allocatable across Nodes" "nvidia.com/gpu 192, amd.com/gpu 16, habana.ai/gaudi 16" "nvidia.com/gpu $nv, amd.com/gpu $amd, habana.ai/gaudi $hpu" "$( [ "$nv" = 192 ] && [ "$amd" = 16 ] && [ "$hpu" = 16 ] && echo PASS || echo FAIL)"

notready_taints() { kc get nodes -o jsonpath='{range .items[*]}{.spec.taints[*].key}{"\n"}{end}' | grep -c 'node.kubernetes.io/not-ready' || true; }
if [ -n "$KCM_BIN" ]; then
	no_notready_taints() { [ "$(notready_taints)" = 0 ]; }
	ms=$(wait_until 60 no_notready_taints) || true
	n=$(notready_taints)
	record U3 "not-ready:NoSchedule taint (TaintNodesByCondition) lifted from Ready Nodes" "0 Nodes keep it (the node-lifecycle controller lifts it)" "$n Nodes keep it, after ${ms} ms" "$( [ "$n" = 0 ] && echo PASS || echo FAIL)"
else
	# No node-lifecycle controller: do its one job for Ready Nodes, and say so.
	for nd in $(kc get nodes -o jsonpath='{.items[*].metadata.name}'); do kc taint node "$nd" node.kubernetes.io/not-ready:NoSchedule- >/dev/null 2>&1 || true; done
	record U3 "not-ready:NoSchedule taint on Ready Nodes" "removed by this harness (KCM=none: no node-lifecycle controller)" "$(notready_taints) Nodes keep it" CANNOT
fi

alive=0
for p in $PIDS; do kill -0 "$p" 2>/dev/null && alive=$((alive + 1)); done
record U4 "KAI $KAI_REF ($KAI_COMMIT) components running" "$KAI_RUNNING of $KAI_RUNNING (scheduler, binder, podgrouper, queuecontroller$( [ "$KAI_RUNNING" = 5 ] && echo ', podgroupcontroller'); podgroupcontroller only with KAI_PODGROUPCONTROLLER=1, it binds :8080)" "$alive of $KAI_RUNNING" "$( [ $alive = "$KAI_RUNNING" ] && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- A: placement on a healthy estate

submit_gang nvl72-a 18 4 "nvidia.com/gpu.product: NVIDIA-GB300"
ms=$(wait_until 60 has_count nvl72-a 18) || true
nodes=$(bound_nodes nvl72-a | sort -u | grep -c . || true)
cl=$(cliques_of nvl72-a)
gb300=$(for n in $(bound_nodes nvl72-a); do label_of "$n" 'nvidia\.com/gpu\.product'; echo; done | grep -c NVIDIA-GB300 || true)
record A1 "Gang of 18 pods x 4 nvidia.com/gpu, required in one nvidia.com/gpu.clique, GB300" "18 of 18 bound, 18 Nodes, 1 clique" "$(bound_count nvl72-a) bound after ${ms} ms on $nodes Nodes ($gb300 GB300) in $cl clique(s)" "$( [ "$(bound_count nvl72-a)" = 18 ] && [ "$nodes" = 18 ] && [ "$cl" = 1 ] && echo PASS || echo FAIL)"
kc get pods -n "$NS" -l dc-twin.io/workload=nvl72-a -o wide >"$WORK/evidence/A1-gang.txt"

# single pods, one per accelerator, each with the product label a stack would select on
submit_pod gb200-x1 inference nvidia.com/gpu 1 "nvidia.com/gpu.product: NVIDIA-GB200"
submit_pod h100-x8 inference nvidia.com/gpu 8 "nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3"
submit_pod a100-x8 inference nvidia.com/gpu 8 "nvidia.com/gpu.product: NVIDIA-A100-SXM4-80GB"
submit_pod mi355x-x4 inference amd.com/gpu 4 "amd.com/gpu.product-name: AMD_Instinct_MI355X"
submit_pod gaudi3-x4 inference habana.ai/gaudi 4 "kubernetes.io/os: linux"
i=2
for spec in "gb200-x1 nvidia\\.com/gpu\\.product NVIDIA-GB200" "h100-x8 nvidia\\.com/gpu\\.product NVIDIA-H100-80GB-HBM3" \
	"a100-x8 nvidia\\.com/gpu\\.product NVIDIA-A100-SXM4-80GB" "mi355x-x4 amd\\.com/gpu\\.product-name AMD_Instinct_MI355X" \
	"gaudi3-x4 habana\\.ai/gaudi\\.product HL-325L"; do
	set -- $spec
	ms=$(wait_until 30 is_bound "$1") || true
	n=$(node_of "$1")
	got=$([ -n "$n" ] && label_of "$n" "$2" || true)
	what=$(kc get pod -n "$NS" "$1" -o jsonpath='{.spec.containers[0].resources.limits}' | tr -d '{}"')
	record "A$i" "Pod $1 ($what)" "bound to a Node labelled $3" "${n:-unbound} ($got) after ${ms} ms" "$( [ "$got" = "$3" ] && echo PASS || echo FAIL)"
	i=$((i + 1))
done

# amd.com/gpu with no node selector: where does KAI put it?
submit_pod amd-anywhere-x2 inference amd.com/gpu 2 "kubernetes.io/os: linux"
wait_until 30 is_bound amd-anywhere-x2 >/dev/null || true
n=$(node_of amd-anywhere-x2)
has=$([ -n "$n" ] && alloc_of "$n" 'amd\.com/gpu' || true)
if [ -n "$has" ] && [ "$has" != 0 ]; then res=PASS; else res=FINDING; fi
record A7 "Pod amd-anywhere-x2 (amd.com/gpu 2, no node selector)" "bound to a Node that advertises amd.com/gpu (a kubelet admits nothing else)" "${n:-unbound}, whose allocatable amd.com/gpu is '${has:-none}' ($(label_of "${n:-x}" 'nvidia\.com/gpu\.product' 2>/dev/null || true))" "$res"
kc get pod -n "$NS" amd-anywhere-x2 -o wide >"$WORK/evidence/A7-amd-anywhere.txt" 2>&1 || true
delete_workload amd-anywhere-x2

queue_counted() { [ -n "$(kc get queue training -o 'jsonpath={.status.allocated.nvidia\.com/gpu}')" ]; }
wait_until 30 queue_counted >/dev/null || true
qt=$(kc get queue training -o jsonpath='{.status.allocated}' | tr -d '{}"')
qi=$(kc get queue inference -o jsonpath='{.status.allocated}' | tr -d '{}"')
if [ "$KAI_RUNNING" = 5 ]; then
	record A8 "Queue accounting (queuecontroller)" "training nvidia.com/gpu:72; inference holds the single pods" "training {$qt}; inference {$qi}" "$( echo "$qt" | grep -q 'nvidia.com/gpu:72' && echo PASS || echo FAIL)"
else
	# the queuecontroller sums PodGroup status, which only the podgroupcontroller writes
	record A8 "Queue accounting (queuecontroller)" "needs KAI's podgroupcontroller (KAI_PODGROUPCONTROLLER=1; it binds :8080)" "training {$qt}; inference {$qi}" CANNOT
fi
kc get queues -o yaml >"$WORK/evidence/A8-queues.yaml"

# ---------------------------------------------------------------- B: faults and power, twin side

delete_workload nvl72-a
workload_gone() { [ -z "$(kc get pods -n "$NS" -l "dc-twin.io/workload=$1" -o name)" ]; }
wait_until 30 workload_gone nvl72-a >/dev/null || true

TRAY=a01-compute-tray-07
twin fault inject 'a01/compute-tray-07/gpu-2' nvidia/xid/xid-79 >"$WORK/evidence/B1-inject.json"
# A kubelet stays Ready after an Xid: the GPU's condition and allocatable carry it, so the
# controller manager adds no not-ready:NoExecute taint to evict the pods on the healthy GPUs.
cond_of() { kc get node "$1" -o "jsonpath={.status.conditions[?(@.type==\"$2\")].status}"; }
xid_settled() { [ "$(ready_of $TRAY | cut -d/ -f1)" = True ] && [ "$(cond_of $TRAY GpuHealthy)" = False ] && [ "$(kc get node $TRAY -o jsonpath='{.spec.unschedulable}')" = true ] && [ "$(alloc_of $TRAY 'nvidia\.com/gpu')" = 3 ] && ! taints_of $TRAY | grep -q not-ready; }
ms=$(wait_until 20 xid_settled) || true
gh=$(kc get node $TRAY -o 'jsonpath={.status.conditions[?(@.type=="GpuHealthy")].status}')
record B1 "dctwin fault inject a01/compute-tray-07/gpu-2 nvidia/xid/xid-79" "$TRAY Ready, GpuHealthy=False, cordoned, allocatable nvidia.com/gpu 3, no not-ready taint" "Ready=$(ready_of $TRAY), unschedulable=$(kc get node $TRAY -o jsonpath='{.spec.unschedulable}'), GpuHealthy=$gh, allocatable $(alloc_of $TRAY 'nvidia\.com/gpu'), taints [$(taints_of $TRAY)] after ${ms} ms" "$(pass_if "$(ok xid_settled)")"
kc describe node $TRAY >"$WORK/evidence/B1-describe-node.txt"

OFF_TRAY=a02-compute-tray-03
twin power off 'a02/compute-tray-03' >/dev/null
off_settled() { [ "$(ready_of "$1")" = "False/PoweredOff" ]; }
ms=$(wait_until 30 off_settled $OFF_TRAY) || true
tainted_not_ready() { taints_of "$1" | grep -q not-ready; }
if [ -n "$KCM_BIN" ]; then wait_until 20 tainted_not_ready $OFF_TRAY >/dev/null || true; fi
record B2 "dctwin power off a02/compute-tray-03" "$OFF_TRAY Ready=False (PoweredOff)$( [ -n "$KCM_BIN" ] && echo '; node-lifecycle controller taints it not-ready')" "Ready=$(ready_of $OFF_TRAY) after ${ms} ms, taints [$(taints_of $OFF_TRAY)]" "$( off_settled $OFF_TRAY && { [ -z "$KCM_BIN" ] || taints_of $OFF_TRAY | grep -q not-ready; } && echo PASS || echo FAIL)"

OFF_SRV=b01-server-02
twin power off 'b01/server-02' >/dev/null
ms=$(wait_until 30 off_settled $OFF_SRV) || true
record B3 "dctwin power off b01/server-02 (HGX H100)" "$OFF_SRV Ready=False (PoweredOff)" "Ready=$(ready_of $OFF_SRV) after ${ms} ms, taints [$(taints_of $OFF_SRV)]" "$(pass_if "$(ok off_settled $OFF_SRV)")"

twin fault inject 'b03/server-01/gpu-3' amd/ras/uncorrectable-ecc >/dev/null
amd_settled() { [ "$(ready_of b03-server-01 | cut -d/ -f1)" = True ] && [ "$(cond_of b03-server-01 GpuHealthy)" = False ] && [ "$(alloc_of b03-server-01 'amd\.com/gpu')" = 7 ]; }
ms=$(wait_until 20 amd_settled) || true
record B4 "dctwin fault inject b03/server-01/gpu-3 amd/ras/uncorrectable-ecc" "b03-server-01 Ready, GpuHealthy=False, cordoned, allocatable amd.com/gpu 7" "Ready=$(ready_of b03-server-01), unschedulable=$(kc get node b03-server-01 -o jsonpath='{.spec.unschedulable}'), allocatable $(alloc_of b03-server-01 'amd\.com/gpu') after ${ms} ms" "$(pass_if "$(ok amd_settled)")"

twin fault inject 'b04/server-01/hpu-5' intel/gaudi/hbm-uncorrectable >/dev/null
hpu_settled() { [ "$(ready_of b04-server-01 | cut -d/ -f1)" = True ] && [ "$(cond_of b04-server-01 HpuHealthy)" = False ] && [ "$(alloc_of b04-server-01 'habana\.ai/gaudi')" = 7 ]; }
ms=$(wait_until 20 hpu_settled) || true
record B5 "dctwin fault inject b04/server-01/hpu-5 intel/gaudi/hbm-uncorrectable" "b04-server-01 Ready, HpuHealthy=False, cordoned, allocatable habana.ai/gaudi 7" "Ready=$(ready_of b04-server-01), unschedulable=$(kc get node b04-server-01 -o jsonpath='{.spec.unschedulable}'), allocatable $(alloc_of b04-server-01 'habana\.ai/gaudi') after ${ms} ms" "$(pass_if "$(ok hpu_settled)")"
kc get nodes "$TRAY" "$OFF_TRAY" "$OFF_SRV" b03-server-01 b04-server-01 >"$WORK/evidence/B-nodes-after-faults.txt"

# ---------------------------------------------------------------- C: new pods avoid what the twin broke

# Each NVLink domain now has a tray out (a01's Xid tray, a02's powered-off tray, and gb200-x1
# holds one GPU on another a02 tray): no clique has 18 trays with 4 free GPUs.
submit_gang nvl72-b 18 4 "kubernetes.io/os: linux"
sleep 15
why=$(podgroup_reason nvl72-b | tr '\n|' '  ' | cut -c1-160)
record C1 "Gang of 18 x 4, one clique, no product selector, after the faults" "0 of 18 bound (gang is all-or-nothing); PodGroup Unschedulable" "$(bound_count nvl72-b) bound after 15 s; PodGroup: $why" "$( [ "$(bound_count nvl72-b)" = 0 ] && [ -n "$why" ] && echo PASS || echo FAIL)"
kc get podgroup -n "$NS" nvl72-b -o yaml >"$WORK/evidence/C1-podgroup.yaml"
kc delete podgroup -n "$NS" nvl72-b >/dev/null
delete_workload nvl72-b

submit_gang nvl72-c 17 4 "kubernetes.io/os: linux"
ms=$(wait_until 60 has_count nvl72-c 17) || true
bad=$(bound_nodes nvl72-c | grep -c -e "^$TRAY\$" -e "^$OFF_TRAY\$" || true)
cl=$(cliques_of nvl72-c)
racks=$(for n in $(bound_nodes nvl72-c); do label_of "$n" 'dc-twin\.io/rack'; echo; done | sort -u | tr '\n' ' ')
record C2 "Gang of 17 x 4, one clique, no product selector" "17 of 17 bound in one clique, none on $TRAY or $OFF_TRAY" "$(bound_count nvl72-c) bound after ${ms} ms, $cl clique (rack ${racks% }), $bad on a faulted tray" "$( [ "$(bound_count nvl72-c)" = 17 ] && [ "$cl" = 1 ] && [ "$bad" = 0 ] && echo PASS || echo FAIL)"
kc get pods -n "$NS" -l dc-twin.io/workload=nvl72-c -o wide >"$WORK/evidence/C2-gang.txt"

# H100: four servers, one powered off, one holding h100-x8.
free_h100=0
for n in b01-server-01 b01-server-02 b02-server-01 b02-server-02; do
	[ "$(ready_of $n | cut -d/ -f1)" = True ] && [ "$n" != "$(node_of h100-x8)" ] && free_h100=$((free_h100 + 1))
done
for k in 1 2 3; do submit_pod "h100-after-$k" inference nvidia.com/gpu 8 "nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3"; done
sleep 10
placed=0 on_off=0 where=""
for k in 1 2 3; do
	n=$(node_of "h100-after-$k")
	[ -n "$n" ] && placed=$((placed + 1)) && where="$where $n"
	[ "$n" = "$OFF_SRV" ] && on_off=$((on_off + 1))
done
record C3 "3 pods x 8 H100 GPUs after b01-server-02 is off" "$free_h100 bound (the Ready H100 servers with 8 free), none on $OFF_SRV, the rest Pending" "$placed bound (${where# }), $on_off on $OFF_SRV" "$( [ "$placed" = "$free_h100" ] && [ "$on_off" = 0 ] && echo PASS || echo FAIL)"

submit_pod mi355x-after-x4 inference amd.com/gpu 4 "amd.com/gpu.product-name: AMD_Instinct_MI355X"
submit_pod gaudi3-after-x4 inference habana.ai/gaudi 4 "kubernetes.io/os: linux"
wait_until 30 is_bound mi355x-after-x4 >/dev/null || true
wait_until 30 is_bound gaudi3-after-x4 >/dev/null || true
record C4 "Pod mi355x-after-x4 after the RAS error on b03-server-01" "bound to b03-server-02" "$(node_of mi355x-after-x4 || true)" "$( [ "$(node_of mi355x-after-x4)" = b03-server-02 ] && echo PASS || echo FAIL)"
record C5 "Pod gaudi3-after-x4 after the HBM error on b04-server-01" "bound to b04-server-02" "$(node_of gaudi3-after-x4 || true)" "$( [ "$(node_of gaudi3-after-x4)" = b04-server-02 ] && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- D: recovery

pending=""
for k in 1 2 3; do [ -z "$(node_of "h100-after-$k")" ] && pending="h100-after-$k" && break; done
twin power on 'b01/server-02' >/dev/null
pod_on() { [ "$(node_of "$1")" = "$2" ]; }
ms=$(wait_until 90 pod_on "${pending:-none}" $OFF_SRV) || true
record D1 "dctwin power on b01/server-02 with ${pending:-no} pod Pending" "$OFF_SRV Ready again; KAI binds the Pending pod to it" "Ready=$(ready_of $OFF_SRV); ${pending:-none} on $(node_of "${pending:-none}" 2>/dev/null || echo -) after ${ms} ms" "$( [ -n "$pending" ] && [ "$(node_of "$pending")" = $OFF_SRV ] && echo PASS || echo FAIL)"

twin fault clear 'a01/compute-tray-07/gpu-2' nvidia/xid/xid-79 >/dev/null
cleared() { [ "$(ready_of $TRAY)" = "True/KubeletReady" ] && [ -z "$(kc get node $TRAY -o jsonpath='{.spec.unschedulable}')" ] && [ "$(alloc_of $TRAY 'nvidia\.com/gpu')" = 4 ]; }
ms=$(wait_until 20 cleared) || true
record D2 "dctwin fault clear (Xid 79)" "$TRAY Ready, uncordoned, allocatable nvidia.com/gpu 4" "Ready=$(ready_of $TRAY), unschedulable='$(kc get node $TRAY -o jsonpath='{.spec.unschedulable}')', allocatable $(alloc_of $TRAY 'nvidia\.com/gpu') after ${ms} ms" "$(pass_if "$(ok cleared)")"

kc get pods -n "$NS" -o wide >"$WORK/evidence/pods-final.txt"
kc get podgroups -n "$NS" >"$WORK/evidence/podgroups-final.txt"
kc get bindrequests -n "$NS" >"$WORK/evidence/bindrequests-final.txt" 2>&1 || true
twin report >"$WORK/evidence/fidelity-report.txt" 2>&1 || true

status=0
print_table "KAI Scheduler $KAI_REF against dc-twin ($(basename "$ESTATE"), kube-controller-manager: $( [ -n "$KCM_BIN" ] && echo "$K8S_REF" || echo none)). Nodes are the kwok projection: IMPERSONATED (D24); no pod runs." || status=$?
echo
echo "artefacts: $WORK/evidence (kubectl output, rendered workloads), logs: $WORK/kai, $WORK/cluster/logs, $WORK/twin/up.log"
exit $status

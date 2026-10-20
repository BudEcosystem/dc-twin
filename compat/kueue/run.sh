#!/usr/bin/env bash
# Runs Kueue (kubernetes-sigs/kueue), unmodified and built from a released tag, against the twin's
# Kubernetes view of the lab estate: a real kube-apiserver and etcd (envtest's binaries), the real
# kube-controller-manager (Jobs, node lifecycle, taint eviction) and kube-scheduler (binding), and
# dc-twin's kwok projection standing in for 50 kubelets (IMPERSONATED, D24). Kueue admits Jobs by
# quota into flavors keyed on the twin's node labels, places NVL72 gangs inside one NVLink domain
# with topology-aware scheduling, and reacts to the twin's faults and power changes.
#
#   compat/kueue/run.sh                      # PASS/FAIL table on stdout, artefacts under $WORK
#   WORK=/tmp/kueue DCTWIN=bin/dctwin compat/kueue/run.sh
#   KCM=/path/kube-controller-manager KUBE_SCHEDULER=/path/kube-scheduler compat/kueue/run.sh
#   BUILD_ONLY=1 compat/kueue/run.sh        # clone and build, then stop
#   KEEP=1 compat/kueue/run.sh               # leave everything running; prints how to stop it
#
# Nothing here reads ~/.kube/config: every client is given the scratch cluster's kubeconfig.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
KUEUE_REF=${KUEUE_REF:-v0.20.2}       # github.com/kubernetes-sigs/kueue
K8S_REF=${K8S_REF:-v1.37.0}           # kube-controller-manager and kube-scheduler, at the envtest apiserver's version
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
KCM=${KCM:-build}                     # build | <path to kube-controller-manager>
KUBE_SCHEDULER=${KUBE_SCHEDULER:-build}   # build | <path to kube-scheduler>
ADAPTERS=${ADAPTERS:-redfish,kwok,metrics}
ESTATE=${ESTATE:-$root/examples/estates/lab-model.yaml}   # the checks name its racks and hosts
mkdir -p "$WORK/bin" "$WORK/src"
WORK=$(cd "$WORK" && pwd)
rm -rf "$WORK/evidence" && mkdir -p "$WORK/evidence"

. "$here/../kai/lib/common.sh"
. "$here/../kai/lib/cluster.sh"
. "$here/../kai/lib/twin.sh"
. "$here/lib/secret-volume.sh"

ENVTEST_DIR=$(envtest_dir)
[ -x "$ENVTEST_DIR/kube-apiserver" ] || { echo "no envtest binaries: set KUBEBUILDER_ASSETS (setup-envtest use -p path)" >&2; exit 1; }
KUBECTL=${KUBECTL:-$ENVTEST_DIR/kubectl}
export ENVTEST_DIR KUBECTL
command -v "$DCTWIN" >/dev/null || { echo "dctwin not found: set DCTWIN (make build; DCTWIN=bin/dctwin)" >&2; exit 1; }
DCTWIN=$(command -v "$DCTWIN")

# ---------------------------------------------------------------- build from source

if [ ! -d "$WORK/src/kueue/.git" ]; then
	git -c advice.detachedHead=false clone -q --depth 1 --branch "$KUEUE_REF" https://github.com/kubernetes-sigs/kueue.git "$WORK/src/kueue" 2>&1 | grep -v 'is not a commit' >&2 || true
fi
KUEUE_COMMIT=$(git -C "$WORK/src/kueue" rev-parse HEAD)
if [ ! -x "$WORK/bin/kueue" ]; then
	echo "building Kueue $KUEUE_REF (about fifteen minutes cold)" >&2
	(cd "$WORK/src/kueue" && go build -o "$WORK/bin/kueue" ./cmd/kueue)
fi

k8s_build() { # k8s_build <binary>: from kubernetes/kubernetes at K8S_REF, once
	if [ ! -d "$WORK/src/kubernetes/.git" ]; then
		git -c advice.detachedHead=false clone -q --depth 1 --branch "$K8S_REF" https://github.com/kubernetes/kubernetes.git "$WORK/src/kubernetes" 2>&1 | grep -v 'is not a commit' >&2 || true
	fi
	if [ ! -x "$WORK/bin/$1" ]; then
		echo "building $1 $K8S_REF (once)" >&2
		(cd "$WORK/src/kubernetes" && go build -o "$WORK/bin/$1" "./cmd/$1")
	fi
	echo "$WORK/bin/$1"
}
[ "$KCM" = build ] && KCM=$(k8s_build kube-controller-manager)
[ "$KUBE_SCHEDULER" = build ] && KUBE_SCHEDULER=$(k8s_build kube-scheduler)
[ "${BUILD_ONLY:-}" = 1 ] && { echo "BUILD_ONLY=1: sources and builds in $WORK" >&2; exit 0; }

# Kueue's install manifests as its release builds them (config/default, the kustomization its
# manifests.yaml is made from), rendered offline by kubectl's built-in kustomize: the CRDs with
# their conversion webhooks, the webhook configurations and the empty webhook-certificate Secret.
"$KUBECTL" kustomize "$WORK/src/kueue/config/default" >"$WORK/kueue-manifests.yaml"

# ---------------------------------------------------------------- bring-up

PIDS=""
cleanup() {
	set +e
	if [ "${KEEP:-}" = 1 ]; then
		echo "KEEP=1: left running. Stop with: kill $PIDS; HOME=$TWIN_HOME $DCTWIN down --state $TWIN_STATE; kill $CLUSTER_PIDS" >&2
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

rm -rf "$WORK/cluster" "$WORK/twin" "$WORK/kueue"
mkdir -p "$WORK/kueue"
cluster_up "$WORK/cluster" "$KCM"
echo "kube-apiserver $APISERVER_URL (envtest $(basename "$ENVTEST_DIR")); kube-controller-manager and kube-scheduler $K8S_REF" >&2

"$KUBE_SCHEDULER" --kubeconfig "$KUBECONFIG_FILE" --leader-elect=false --secure-port 0 >"$WORK/kueue/kube-scheduler.log" 2>&1 &
PIDS="$PIDS $!"

# The webhook Service is an ExternalName for localhost, and the webhook configurations and CRD
# conversion stanzas carry the loopback port Kueue serves on: kube-apiserver dials
# localhost:<port> and verifies the certificate Kueue issued for kueue-webhook-service.kueue-system.svc
# (lib/render.py). Everything else in those objects is Kueue's.
WEBHOOK_PORT=$(free_port)
kc create namespace kueue-system >/dev/null
kc create service externalname kueue-webhook-service -n kueue-system --external-name localhost >/dev/null
python3 -I "$here/lib/render.py" kinds "$WORK/kueue-manifests.yaml" CustomResourceDefinition,MutatingWebhookConfiguration,ValidatingWebhookConfiguration,Secret >"$WORK/kueue/selected.yaml"
python3 -I "$here/lib/render.py" service-port kueue-webhook-service "$WEBHOOK_PORT" "$WORK/kueue/selected.yaml" >"$WORK/kueue/install.yaml"
kc apply --server-side -f "$WORK/kueue/install.yaml" >/dev/null
crds_established() { [ "$(kc get crd -o 'jsonpath={range .items[*]}{.status.conditions[?(@.type=="Established")].status}{"\n"}{end}' | grep -c True)" -ge 11 ]; }
wait_until 60 crds_established >/dev/null || { echo "Kueue CRDs not established" >&2; exit 1; }

sed -e "s|@HEALTH_PORT@|$(free_port)|" -e "s|@WEBHOOK_PORT@|$WEBHOOK_PORT|" -e "s|@CERT_DIR@|$WORK/kueue/certs|" \
	-e "s|@VISIBILITY_PORT@|$(free_port)|" "$here/manifests/kueue-config.yaml.tmpl" >"$WORK/kueue/config.yaml"
secret_volume kueue-system kueue-webhook-server-cert "$WORK/kueue/certs" &
PIDS="$PIDS $!"
# VisibilityOnDemand is off: its API server writes its certificates to /visibility, a path a
# process outside a container cannot create (pkg/visibility/server.go).
(cd "$WORK/kueue" && KUBECONFIG="$KUBECONFIG_FILE" HOME="$WORK/kueue" exec "$WORK/bin/kueue" --config "$WORK/kueue/config.yaml" \
	--feature-gates=VisibilityOnDemand=false >"$WORK/kueue/kueue.log" 2>&1) &
PIDS="$PIDS $!"
kueue_started() { grep -q '"logger":"job".*"Starting workers"' "$WORK/kueue/kueue.log" && grep -q '"logger":"clusterqueue-reconciler".*"Starting workers"' "$WORK/kueue/kueue.log"; }
wait_until 120 kueue_started >/dev/null || { tail -20 "$WORK/kueue/kueue.log" >&2; echo "Kueue did not start" >&2; exit 1; }

twin_up "$ESTATE" "$WORK/twin" "$ADAPTERS" "$KUBECONFIG_FILE"
echo "twin up: $(grep -o 'control API: [^ ]*' "$WORK/twin/up.log"), state $TWIN_STATE" >&2

kc apply -f "$here/manifests/00-namespace.yaml" -f "$here/manifests/01-flavors.yaml" -f "$here/manifests/02-queues.yaml" >"$WORK/evidence/queues-applied.txt"

# ---------------------------------------------------------------- helpers

NS=dc-twin-kueue
submit() { # name queue resource per-pod count [pod annotations]
	sed -e "s|@NAME@|$1|g" -e "s|@QUEUE@|$2|" -e "s|@RESOURCE@|$3|" -e "s|@PER_POD@|$4|" -e "s|@COUNT@|$5|g" \
		-e "s|@ANNOTATIONS@|${6:-}|" "$here/manifests/job.yaml.tmpl" | tee "$WORK/evidence/job-$1.yaml" | kc apply -f - >/dev/null
}
TAS='kueue.x-k8s.io/podset-required-topology: nvidia.com/gpu.clique'
# A Job's pods bound to a Node with no kubelet never finish terminating; delete them as a kubelet
# that confirmed the kill would.
delete_job() {
	kc delete job -n "$NS" "$1" --wait=false >/dev/null 2>&1 || true
	kc delete pods -n "$NS" -l "dc-twin.io/workload=$1" --force --grace-period=0 >/dev/null 2>&1 || true
}
wl_of() { kc get workloads -n "$NS" -l "kueue.x-k8s.io/job-uid=$(kc get job -n "$NS" "$1" -o jsonpath='{.metadata.uid}')" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null; }
wl_cond() { kc get workload -n "$NS" "$(wl_of "$1")" -o "jsonpath={.status.conditions[?(@.type==\"$2\")].status}" 2>/dev/null; }
wl_msg() { kc get workload -n "$NS" "$(wl_of "$1")" -o "jsonpath={.status.conditions[?(@.type==\"$2\")].message}" 2>/dev/null; }
wl_flavors() { kc get workload -n "$NS" "$(wl_of "$1")" -o 'jsonpath={.status.admission.podSetAssignments[0].flavors}' 2>/dev/null | tr -d '{}"'; }
wl_hosts() { kc get workload -n "$NS" "$(wl_of "$1")" -o 'jsonpath={.status.admission.podSetAssignments[0].topologyAssignment.slices[*].valuesPerLevel[0].individual.prefix}{" "}{.status.admission.podSetAssignments[0].topologyAssignment.slices[*].valuesPerLevel[0].individual.roots}' 2>/dev/null; }
admitted() { [ "$(wl_cond "$1" Admitted)" = True ]; }
bound_nodes() { kc get pods -n "$NS" -l "dc-twin.io/workload=$1" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | grep . || true; }
bound_count() { bound_nodes "$1" | grep -c . || true; }
has_bound() { [ "$(bound_count "$1")" = "$2" ]; }
pods_count() { kc get pods -n "$NS" -l "dc-twin.io/workload=$1" -o name | grep -c . || true; }
label_of() { kc get node "$1" -o "jsonpath={.metadata.labels.$2}"; }
labels_of() { local n; for n in $(bound_nodes "$1"); do label_of "$n" "$2"; echo; done | sort | uniq -c | awk '{printf "%s%s x%s", (NR>1?", ":""), $2, $1}'; }
cliques_of() { local n; for n in $(bound_nodes "$1"); do label_of "$n" 'nvidia\.com/gpu\.clique'; echo; done | sort -u | grep -c . || true; }
ready_of() { kc get node "$1" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].reason}'; }
cond_of() { kc get node "$1" -o "jsonpath={.status.conditions[?(@.type==\"$2\")].status}"; }
alloc_of() { kc get node "$1" -o "jsonpath={.status.allocatable.$2}"; }
taints_of() { kc get node "$1" -o 'jsonpath={range .spec.taints[*]}{.key}:{.effect} {end}'; }
pod_at() { # the Node of the live (not terminating) pod with this completion index
	kc get pods -n "$NS" -l "dc-twin.io/workload=$1,batch.kubernetes.io/job-completion-index=$2" \
		-o 'jsonpath={range .items[*]}{.metadata.deletionTimestamp}|{.spec.nodeName}{"\n"}{end}' | awk -F'|' '$1 == "" && $2 != "" { print $2 }' | tail -1
}
suspended() { [ "$(kc get job -n "$NS" "$1" -o jsonpath='{.spec.suspend}')" = true ]; }
cq_usage() { kc get clusterqueue "$1" -o 'jsonpath={range .status.flavorsUsage[*]}{.name}={.resources[0].total} {end}'; }
ok() { "$@" >/dev/null 2>&1 && echo 0 || echo 1; }

# ---------------------------------------------------------------- U: the twin's Nodes, the stack

hosts=50
node_count_is() { [ "$(kc get nodes -o name | grep -c .)" -eq "$1" ]; }
ms=$(wait_until 60 node_count_is $hosts) || true
ready=$(kc get nodes -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c True || true)
imp=$(kc get nodes -o jsonpath='{range .items[*]}{.metadata.annotations.dc-twin\.io/impersonated}{"\n"}{end}' | grep -c kwok || true)
record U1 "Nodes the twin registers (kwok projection)" "$hosts Ready, all dc-twin.io/impersonated=kwok" "$ready Ready, $imp impersonated, after ${ms} ms" "$( [ "$ready" = $hosts ] && [ "$imp" = $hosts ] && echo PASS || echo FAIL)"
kc get nodes -L nvidia.com/gpu.product,amd.com/gpu.product-name,habana.ai/gaudi.product,nvidia.com/gpu.clique >"$WORK/evidence/nodes.txt"

notready_taints() { kc get nodes -o jsonpath='{range .items[*]}{.spec.taints[*].key}{"\n"}{end}' | grep -c 'node.kubernetes.io/not-ready' || true; }
no_notready_taints() { [ "$(notready_taints)" = 0 ]; }
ms=$(wait_until 60 no_notready_taints) || true
record U2 "not-ready:NoSchedule taint (TaintNodesByCondition) lifted from Ready Nodes" "0 Nodes keep it (the node-lifecycle controller lifts it)" "$(notready_taints) Nodes keep it, after ${ms} ms" "$( no_notready_taints && echo PASS || echo FAIL)"

alive=0
for p in $PIDS; do kill -0 "$p" 2>/dev/null && alive=$((alive + 1)); done
cqs_active() { [ "$(kc get clusterqueue -o 'jsonpath={range .items[*]}{.status.conditions[?(@.type=="Active")].status}{"\n"}{end}' | grep -c True)" = 2 ]; }
wait_until 30 cqs_active >/dev/null || true
cab=$(for w in mutating validating; do kc get "${w}webhookconfiguration" "kueue-$w-webhook-configuration" -o 'jsonpath={.webhooks[0].clientConfig.caBundle}' | head -c 8 | grep -c . ; done | grep -c 1 || true)
record U3 "Kueue $KUEUE_REF ($KUEUE_COMMIT) and kube-scheduler running; webhooks served" "processes up; CA injected into both webhook configurations by Kueue's cert rotator; ClusterQueues nvl72 and accel Active (their creation went through Kueue's validating webhook)" "$alive of 3 processes (kube-scheduler, Kueue, the secret projection) up; CA bundle in $cab of 2 configurations; Active: $(kc get clusterqueue -o 'jsonpath={range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="Active")].status} {end}')" "$( [ "$alive" -ge 3 ] && [ "$cab" = 2 ] && cqs_active && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- A: admission by quota, placement by flavor

submit h100-a inference nvidia.com/gpu 8 2
ms=$(wait_until 60 has_bound h100-a 2) || true
record A1 "Job h100-a: 2 pods x 8 nvidia.com/gpu, LocalQueue inference" "admitted to flavor h100; 2 pods bound to 2 H100 Nodes" "admitted=$(wl_cond h100-a Admitted) flavor {$(wl_flavors h100-a)}; bound: $(labels_of h100-a 'nvidia\.com/gpu\.product') on $(bound_nodes h100-a | sort -u | grep -c .) Nodes, after ${ms} ms" "$( admitted h100-a && [ "$(labels_of h100-a 'nvidia\.com/gpu\.product')" = "NVIDIA-H100-80GB-HBM3 x2" ] && [ "$(bound_nodes h100-a | sort -u | grep -c .)" = 2 ] && echo PASS || echo FAIL)"
kc get job -n "$NS" h100-a -o jsonpath='{.spec.template.spec.nodeSelector}' >"$WORK/evidence/A1-injected-node-selector.json"

submit mi355x-a inference amd.com/gpu 8 1
submit gaudi3-a inference habana.ai/gaudi 8 1
wait_until 60 has_bound mi355x-a 1 >/dev/null || true
wait_until 60 has_bound gaudi3-a 1 >/dev/null || true
record A2 "Job mi355x-a: 1 pod x 8 amd.com/gpu" "flavor mi355x; bound to an AMD MI355X Node" "flavor {$(wl_flavors mi355x-a)}; on $(bound_nodes mi355x-a) ($(labels_of mi355x-a 'amd\.com/gpu\.product-name'))" "$( [ "$(labels_of mi355x-a 'amd\.com/gpu\.product-name')" = "AMD_Instinct_MI355X x1" ] && echo PASS || echo FAIL)"
record A3 "Job gaudi3-a: 1 pod x 8 habana.ai/gaudi" "flavor gaudi3; bound to a Gaudi 3 Node" "flavor {$(wl_flavors gaudi3-a)}; on $(bound_nodes gaudi3-a) ($(labels_of gaudi3-a 'habana\.ai/gaudi\.product'))" "$( [ "$(labels_of gaudi3-a 'habana\.ai/gaudi\.product')" = "HL-325L x1" ] && echo PASS || echo FAIL)"

submit h100-b inference nvidia.com/gpu 8 2
wait_until 60 has_bound h100-b 2 >/dev/null || true
submit gpu-c inference nvidia.com/gpu 8 2
wait_until 60 has_bound gpu-c 2 >/dev/null || true
record A4 "Jobs h100-b (2 x 8) then gpu-c (2 x 8) on the same queue" "h100-b takes the rest of the h100 quota (32); gpu-c falls through to the next flavor, a100, and binds to the A100 Nodes" "h100-b {$(wl_flavors h100-b)} on $(labels_of h100-b 'nvidia\.com/gpu\.product'); gpu-c {$(wl_flavors gpu-c)} on $(labels_of gpu-c 'nvidia\.com/gpu\.product')" "$( [ "$(labels_of h100-b 'nvidia\.com/gpu\.product')" = "NVIDIA-H100-80GB-HBM3 x2" ] && [ "$(labels_of gpu-c 'nvidia\.com/gpu\.product')" = "NVIDIA-A100-SXM4-80GB x2" ] && echo PASS || echo FAIL)"

submit gpu-d inference nvidia.com/gpu 8 1
sleep 5
record A5 "Job gpu-d (1 x 8) with h100 and a100 quota used up" "not admitted; the Job stays suspended (Kueue's webhook suspended it at creation) and creates no pods" "QuotaReserved=$(wl_cond gpu-d QuotaReserved): $(wl_msg gpu-d QuotaReserved | cut -c1-140); suspend=$(kc get job -n "$NS" gpu-d -o jsonpath='{.spec.suspend}'), $(pods_count gpu-d) pods" "$( ! admitted gpu-d && suspended gpu-d && [ "$(pods_count gpu-d)" = 0 ] && echo PASS || echo FAIL)"

submit nvl72-a training nvidia.com/gpu 4 18 "$TAS"
ms=$(wait_until 60 has_bound nvl72-a 18) || true
record A6 "Job nvl72-a: 18 pods x 4 nvidia.com/gpu, required in one nvidia.com/gpu.clique (TAS)" "admitted to gb300 with a topology assignment of 18 hosts; 18 pods bound to 18 Nodes in 1 clique" "flavor {$(wl_flavors nvl72-a)}; $(bound_count nvl72-a) bound after ${ms} ms on $(bound_nodes nvl72-a | sort -u | grep -c .) Nodes in $(cliques_of nvl72-a) clique(s) ($(labels_of nvl72-a 'nvidia\.com/gpu\.product'))" "$( [ "$(bound_count nvl72-a)" = 18 ] && [ "$(bound_nodes nvl72-a | sort -u | grep -c .)" = 18 ] && [ "$(cliques_of nvl72-a)" = 1 ] && echo PASS || echo FAIL)"
kc get workload -n "$NS" "$(wl_of nvl72-a)" -o yaml >"$WORK/evidence/A6-workload.yaml"
record A7 "ClusterQueue usage" "accel h100=32, a100=16 (and mi355x, gaudi3 8 each); nvl72 gb300=72" "accel: $(cq_usage accel); nvl72: $(cq_usage nvl72)" "$( cq_usage accel | grep -q 'h100=32' && cq_usage accel | grep -q 'a100=16' && cq_usage nvl72 | grep -q 'gb300=72' && echo PASS || echo FAIL)"
kc get clusterqueues -o yaml >"$WORK/evidence/A7-clusterqueues.yaml"

# ---------------------------------------------------------------- B: faults and power, twin side

# gpu-d first: it is still pending, and would be admitted into the quota the others free.
delete_job gpu-d
delete_job nvl72-a
delete_job h100-b
delete_job gpu-c
TRAY=a01-compute-tray-07
twin fault inject 'a01/compute-tray-07/gpu-2' nvidia/xid/xid-79 >"$WORK/evidence/B1-inject.json"
xid_settled() { [ "$(cond_of $TRAY GpuHealthy)" = False ] && [ "$(kc get node $TRAY -o jsonpath='{.spec.unschedulable}')" = true ] && [ "$(alloc_of $TRAY 'nvidia\.com/gpu')" = 3 ]; }
ms=$(wait_until 20 xid_settled) || true
record B1 "dctwin fault inject a01/compute-tray-07/gpu-2 nvidia/xid/xid-79" "$TRAY Ready, GpuHealthy=False, cordoned, allocatable nvidia.com/gpu 3" "Ready=$(ready_of $TRAY), GpuHealthy=$(cond_of $TRAY GpuHealthy), allocatable $(alloc_of $TRAY 'nvidia\.com/gpu'), taints [$(taints_of $TRAY)] after ${ms} ms" "$(pass_if "$(ok xid_settled)")"
OFF_TRAY=a02-compute-tray-03
twin power off 'a02/compute-tray-03' >/dev/null
off_tainted() { [ "$(ready_of "$1")" = "False/PoweredOff" ] && taints_of "$1" | grep -q not-ready; }
ms=$(wait_until 30 off_tainted $OFF_TRAY) || true
record B2 "dctwin power off a02/compute-tray-03" "$OFF_TRAY Ready=False (PoweredOff), not-ready taints from the node-lifecycle controller" "Ready=$(ready_of $OFF_TRAY), taints [$(taints_of $OFF_TRAY)] after ${ms} ms" "$(pass_if "$(ok off_tainted $OFF_TRAY)")"
# The H100 server to power off is one h100-a does not hold, so h100-a keeps running.
OFF_SRV=$(kc get nodes -l nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3 -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep -v -x -F "$(bound_nodes h100-a)" | head -1)
OFF_SRV_PATH=$(kc get node "$OFF_SRV" -o jsonpath='{.metadata.annotations.dc-twin\.io/device-path}')
twin power off "$OFF_SRV_PATH" >/dev/null
ms=$(wait_until 30 off_tainted "$OFF_SRV") || true
record B3 "dctwin power off $OFF_SRV_PATH (HGX H100, not one h100-a holds)" "$OFF_SRV Ready=False (PoweredOff), tainted not-ready" "Ready=$(ready_of "$OFF_SRV"), taints [$(taints_of "$OFF_SRV")] after ${ms} ms" "$(pass_if "$(ok off_tainted "$OFF_SRV")")"

# ---------------------------------------------------------------- C: admission and placement after the faults

# TAS reads the Nodes: with one tray out of each NVLink domain, an 18-tray gang fits nowhere.
submit nvl72-b training nvidia.com/gpu 4 18 "$TAS"
tas_says() { wl_msg nvl72-b QuotaReserved | grep -q 'allows to fit only'; }
wait_until 30 tas_says >/dev/null || true
record C1 "Job nvl72-b: 18 x 4, one clique, after the faults (TAS)" "not admitted: TAS finds 17 usable trays per NVLink domain; the Job stays suspended, no pods" "QuotaReserved=$(wl_cond nvl72-b QuotaReserved): $(wl_msg nvl72-b QuotaReserved | cut -c1-170); $(pods_count nvl72-b) pods" "$( ! admitted nvl72-b && tas_says && [ "$(pods_count nvl72-b)" = 0 ] && echo PASS || echo FAIL)"
kc get workload -n "$NS" "$(wl_of nvl72-b)" -o yaml >"$WORK/evidence/C1-workload.yaml"

# Two 15-tray gangs: one per NVLink domain, each avoiding its domain's faulted tray, two spare.
submit tas-c training nvidia.com/gpu 4 15 "$TAS"
wait_until 60 has_bound tas-c 15 >/dev/null || true
submit tas-d training nvidia.com/gpu 4 15 "$TAS"
wait_until 60 has_bound tas-d 15 >/dev/null || true
bad=$( (bound_nodes tas-c; bound_nodes tas-d) | grep -c -e "^$TRAY\$" -e "^$OFF_TRAY\$" || true)
record C2 "Jobs tas-c and tas-d: 15 x 4 each, one clique (TAS)" "both admitted; each in one clique, gb300 then gb200; none on $TRAY or $OFF_TRAY" "tas-c {$(wl_flavors tas-c)} $(bound_count tas-c) bound in $(cliques_of tas-c) clique; tas-d {$(wl_flavors tas-d)} $(bound_count tas-d) bound in $(cliques_of tas-d) clique; $bad on a faulted tray" "$( [ "$(bound_count tas-c)" = 15 ] && [ "$(bound_count tas-d)" = 15 ] && [ "$(cliques_of tas-c)" = 1 ] && [ "$(cliques_of tas-d)" = 1 ] && [ "$bad" = 0 ] && echo PASS || echo FAIL)"
kc get pods -n "$NS" -o wide >"$WORK/evidence/C2-pods.txt"

# Quota is nominal for flavors without TAS: Kueue admits against the ClusterQueue, not the Nodes.
submit h100-c inference nvidia.com/gpu 8 2
sleep 10
pend=$(kc get pods -n "$NS" -l dc-twin.io/workload=h100-c -o jsonpath='{range .items[*]}{.spec.nodeName}|{.status.conditions[?(@.type=="PodScheduled")].message}{"\n"}{end}' | awk -F'|' '$1 == "" { print $2 }' | head -1 | cut -c1-150)
record C3 "Job h100-c: 2 x 8 H100 with $OFF_SRV off and h100-a holding two servers" "admitted by quota (h100 has 16 free); kube-scheduler binds 1 pod to the one free Ready H100 Node, the other stays Pending (no Ready Node fits)" "admitted=$(wl_cond h100-c Admitted); bound: $(bound_nodes h100-c | tr '\n' ' ')($(bound_count h100-c) of 2); Pending pod: ${pend:-none}" "$( admitted h100-c && [ "$(bound_count h100-c)" = 1 ] && ! bound_nodes h100-c | grep -q "^$OFF_SRV\$" && echo PASS || echo FAIL)"

# TAS failed-node replacement. A cordon alone leaves a running pod where it is; when the pod goes
# (drained, as an operator or a health controller would), Kueue moves that host's share of the
# assignment to a spare tray in the same domain and the Job's replacement pod is bound there.
C_NODE=$(pod_at tas-c 0)
C_PATH=$(kc get node "$C_NODE" -o jsonpath='{.metadata.annotations.dc-twin\.io/device-path}')
twin fault inject "$C_PATH/gpu-1" nvidia/xid/xid-79 >/dev/null
cordoned() { [ "$(kc get node "$1" -o jsonpath='{.spec.unschedulable}')" = true ]; }
wait_until 20 cordoned "$C_NODE" >/dev/null || true
sleep 5
still=$(pod_at tas-c 0)
kc delete pod -n "$NS" -l "dc-twin.io/workload=tas-c,batch.kubernetes.io/job-completion-index=0" --force --grace-period=0 >/dev/null 2>&1 || true
moved() { local n; n=$(pod_at tas-c 0); [ -n "$n" ] && [ "$n" != "$C_NODE" ]; }
ms=$(wait_until 60 moved) || true
newc=$(pod_at tas-c 0)
record C4 "Xid 79 on $C_NODE, a host of tas-c, then its pod drained" "the pod stays on the cordoned tray until drained; then Kueue replaces the host in the TAS assignment with a spare in the same clique, and the new pod is bound there" "before drain: pod on ${still:-none}; after: on ${newc:-none} ($(label_of "${newc:-x}" 'nvidia\.com/gpu\.clique' 2>/dev/null | cut -c1-8)…, same clique: $( [ -n "$newc" ] && [ "$(label_of "$newc" 'nvidia\.com/gpu\.clique')" = "$(label_of "$C_NODE" 'nvidia\.com/gpu\.clique')" ] && echo yes || echo no)) after ${ms} ms; assignment: $(wl_hosts tas-c | cut -c1-120)" "$( [ "$still" = "$C_NODE" ] && [ -n "$newc" ] && [ "$newc" != "$C_NODE" ] && [ "$(label_of "$newc" 'nvidia\.com/gpu\.clique')" = "$(label_of "$C_NODE" 'nvidia\.com/gpu\.clique')" ] && ! wl_hosts tas-c | grep -q "\"${C_NODE##*-}\"" && echo PASS || echo FAIL)"

# A host powered off under a running gang: the node-lifecycle controller evicts the pod once its
# not-ready toleration (20 s, the Job template's) runs out, and Kueue replaces the host.
D_NODE=$(pod_at tas-d 0)
D_PATH=$(kc get node "$D_NODE" -o jsonpath='{.metadata.annotations.dc-twin\.io/device-path}')
twin power off "$D_PATH" >/dev/null
movedd() { local n; n=$(pod_at tas-d 0); [ -n "$n" ] && [ "$n" != "$D_NODE" ]; }
ms=$(wait_until 120 movedd) || true
newd=$(pod_at tas-d 0)
record C5 "dctwin power off $D_NODE, a host of tas-d" "after 20 s the controller manager evicts the pod (TaintManagerEviction); Kueue replaces the host with a spare in the same clique and the new pod is bound there" "pod now on ${newd:-none} after ${ms} ms (same clique: $( [ -n "$newd" ] && [ "$(label_of "$newd" 'nvidia\.com/gpu\.clique')" = "$(label_of "$D_NODE" 'nvidia\.com/gpu\.clique')" ] && echo yes || echo no)); events: $(kc get events -n "$NS" --field-selector reason=TaintManagerEviction -o name | grep -c . || true) TaintManagerEviction" "$( [ -n "$newd" ] && [ "$newd" != "$D_NODE" ] && [ "$(label_of "$newd" 'nvidia\.com/gpu\.clique')" = "$(label_of "$D_NODE" 'nvidia\.com/gpu\.clique')" ] && echo PASS || echo FAIL)"
kc get workloads -n "$NS" -o yaml >"$WORK/evidence/C-workloads.yaml"
kc get events -n "$NS" --sort-by=.lastTimestamp >"$WORK/evidence/C-events.txt"

# ---------------------------------------------------------------- D: recovery

twin power on "$OFF_SRV_PATH" >/dev/null
h100c_full() { [ "$(bound_count h100-c)" = 2 ] && bound_nodes h100-c | grep -q "^$OFF_SRV\$"; }
ms=$(wait_until 120 h100c_full) || true
record D1 "dctwin power on $OFF_SRV_PATH with h100-c's second pod Pending" "kube-scheduler binds the Pending pod to $OFF_SRV once it is Ready again" "Ready=$(ready_of $OFF_SRV); h100-c on $(bound_nodes h100-c | tr '\n' ' ')after ${ms} ms" "$(pass_if "$(ok h100c_full)")"

twin fault clear 'a01/compute-tray-07/gpu-2' nvidia/xid/xid-79 >/dev/null
twin fault clear "$C_PATH/gpu-1" nvidia/xid/xid-79 >/dev/null
twin power on 'a02/compute-tray-03' >/dev/null
twin power on "$D_PATH" >/dev/null
delete_job tas-c
delete_job tas-d
# nvl72-b (C1) has waited in the queue since the faults; it is admitted when a domain has 18 trays.
ms=$(wait_until 120 has_bound nvl72-b 18) || true
record D2 "Faults cleared, trays powered on, tas-c and tas-d deleted" "nvl72-b, pending since C1, is admitted and its 18 pods bound in one clique" "admitted=$(wl_cond nvl72-b Admitted) {$(wl_flavors nvl72-b)}; $(bound_count nvl72-b) bound in $(cliques_of nvl72-b) clique after ${ms} ms" "$( [ "$(bound_count nvl72-b)" = 18 ] && [ "$(cliques_of nvl72-b)" = 1 ] && echo PASS || echo FAIL)"

kc get pods -n "$NS" -o wide >"$WORK/evidence/pods-final.txt"
kc get workloads -n "$NS" >"$WORK/evidence/workloads-final.txt"
twin report >"$WORK/evidence/fidelity-report.txt" 2>&1 || true

status=0
print_table "Kueue $KUEUE_REF against dc-twin ($(basename "$ESTATE"), kube-controller-manager and kube-scheduler $K8S_REF). Nodes are the kwok projection: IMPERSONATED (D24); no pod runs." || status=$?
echo
echo "artefacts: $WORK/evidence (kubectl output, rendered Jobs), logs: $WORK/kueue, $WORK/cluster/logs, $WORK/twin/up.log"
exit $status

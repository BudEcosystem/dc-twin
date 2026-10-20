#!/usr/bin/env bash
# Runs Volcano (volcano-sh/volcano), unmodified and built from a released tag, against the twin's
# Kubernetes view of the lab estate: a real kube-apiserver and etcd (envtest's binaries), the real
# kube-controller-manager (node lifecycle, taints), and dc-twin's kwok projection standing in for
# 50 kubelets (IMPERSONATED, D24). Volcano's admission webhooks, controllers and scheduler run as
# processes: HyperNodes are discovered from the twin's NVLink clique labels, gangs are bound
# all-or-nothing inside one NVLink domain, and the scheduler steers around the twin's faults.
#
#   compat/volcano/run.sh                    # PASS/FAIL table on stdout, artefacts under $WORK
#   WORK=/tmp/volcano DCTWIN=bin/dctwin compat/volcano/run.sh
#   KCM=/path/kube-controller-manager compat/volcano/run.sh
#   BUILD_ONLY=1 compat/volcano/run.sh        # clone and build, then stop
#   KEEP=1 compat/volcano/run.sh             # leave everything running; prints how to stop it
#
# Nothing here reads ~/.kube/config: every client is given the scratch cluster's kubeconfig.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
VOLCANO_REF=${VOLCANO_REF:-v1.16.0}   # github.com/volcano-sh/volcano
K8S_REF=${K8S_REF:-v1.37.0}           # kube-controller-manager, at the envtest apiserver's version
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
KCM=${KCM:-build}                     # build | <path to kube-controller-manager>
ADAPTERS=${ADAPTERS:-redfish,kwok,metrics}
ESTATE=${ESTATE:-$root/examples/estates/lab-model.yaml}   # the checks name its racks and hosts
mkdir -p "$WORK/bin" "$WORK/src"
WORK=$(cd "$WORK" && pwd)
rm -rf "$WORK/evidence" && mkdir -p "$WORK/evidence"

. "$here/../kai/lib/common.sh"
. "$here/../kai/lib/cluster.sh"
. "$here/../kai/lib/twin.sh"

ENVTEST_DIR=$(envtest_dir)
[ -x "$ENVTEST_DIR/kube-apiserver" ] || { echo "no envtest binaries: set KUBEBUILDER_ASSETS (setup-envtest use -p path)" >&2; exit 1; }
KUBECTL=${KUBECTL:-$ENVTEST_DIR/kubectl}
export ENVTEST_DIR KUBECTL
command -v "$DCTWIN" >/dev/null || { echo "dctwin not found: set DCTWIN (make build; DCTWIN=bin/dctwin)" >&2; exit 1; }
DCTWIN=$(command -v "$DCTWIN")

# ---------------------------------------------------------------- build from source

VC_COMPONENTS="scheduler controller-manager webhook-manager"
if [ ! -d "$WORK/src/volcano/.git" ]; then
	git -c advice.detachedHead=false clone -q --depth 1 --branch "$VOLCANO_REF" https://github.com/volcano-sh/volcano.git "$WORK/src/volcano" 2>&1 | grep -v 'is not a commit' >&2 || true
fi
VOLCANO_COMMIT=$(git -C "$WORK/src/volcano" rev-parse HEAD)
for c in $VC_COMPONENTS; do
	if [ ! -x "$WORK/bin/vc-$c" ]; then
		echo "building Volcano $c at $VOLCANO_REF" >&2
		(cd "$WORK/src/volcano" && go build -o "$WORK/bin/vc-$c" "./cmd/$c")
	fi
done
if [ "$KCM" = build ]; then
	if [ ! -x "$WORK/bin/kube-controller-manager" ]; then
		if [ ! -d "$WORK/src/kubernetes/.git" ]; then
			git -c advice.detachedHead=false clone -q --depth 1 --branch "$K8S_REF" https://github.com/kubernetes/kubernetes.git "$WORK/src/kubernetes" 2>&1 | grep -v 'is not a commit' >&2 || true
		fi
		echo "building kube-controller-manager $K8S_REF (once; about ten minutes)" >&2
		(cd "$WORK/src/kubernetes" && go build -o "$WORK/bin/kube-controller-manager" ./cmd/kube-controller-manager)
	fi
	KCM="$WORK/bin/kube-controller-manager"
fi
# The release's install manifest at the tag (the chart rendered by the project):
# CRDs, admission webhook configurations, the scheduler's and admission ConfigMaps.
VC_MANIFEST="$WORK/src/volcano/installer/volcano-development.yaml"
[ "${BUILD_ONLY:-}" = 1 ] && { echo "BUILD_ONLY=1: sources and builds in $WORK" >&2; exit 0; }

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

rm -rf "$WORK/cluster" "$WORK/twin" "$WORK/volcano"
VC="$WORK/volcano"
mkdir -p "$VC/certs" "$VC/conf"
cluster_up "$WORK/cluster" "$KCM"
echo "kube-apiserver $APISERVER_URL (envtest $(basename "$ENVTEST_DIR")); kube-controller-manager $K8S_REF" >&2

# Admission: the manifest's webhook configurations name volcano-admission-service on port 443.
# The Service is an ExternalName for localhost and the configurations carry the loopback port
# vc-webhook-manager serves on (compat/kueue/lib/render.py); the certificate is made as the
# chart's admission-init Job makes it (installer/dockerfile/webhook-manager/
# gen-admission-secret.sh: the same openssl steps and names, written under $WORK), and
# vc-webhook-manager patches its CA into the configurations itself.
WEBHOOK_PORT=$(free_port)
kc apply -f "$here/manifests/00-namespace.yaml" >/dev/null
kc create service externalname volcano-admission-service -n volcano-system --external-name localhost >/dev/null
python3 -I "$here/../kueue/lib/render.py" kinds "$VC_MANIFEST" CustomResourceDefinition,MutatingWebhookConfiguration,ValidatingWebhookConfiguration,ConfigMap >"$VC/manifest-selected.yaml"
python3 -I "$here/../kueue/lib/render.py" service-port volcano-admission-service "$WEBHOOK_PORT" "$VC/manifest-selected.yaml" >"$VC/install.yaml"
kc apply --server-side -f "$VC/install.yaml" >/dev/null
# The scheduler's ConfigMap with network-topology-aware enabled, and HyperNode discovery from labels.
kc apply --server-side --force-conflicts -f "$here/manifests/01-config.yaml" >/dev/null
crds_established() { [ "$(kc get crd -o 'jsonpath={range .items[*]}{.status.conditions[?(@.type=="Established")].status}{"\n"}{end}' | grep -c True)" -ge 12 ]; }
wait_until 60 crds_established >/dev/null || { echo "Volcano CRDs not established" >&2; exit 1; }

SVC=volcano-admission-service
cat >"$VC/certs/csr.conf" <<EOF
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[ v3_req ]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names
[alt_names]
DNS.1 = ${SVC}
DNS.2 = ${SVC}.volcano-system
DNS.3 = ${SVC}.volcano-system.svc
EOF
(
	cd "$VC/certs"
	openssl genrsa -out ca.key 2048 2>/dev/null
	openssl req -x509 -new -nodes -key ca.key -subj "/CN=${SVC}.volcano-system.svc" -days 3650 -out ca.crt
	openssl genrsa -out server.key 2048 2>/dev/null
	openssl req -new -key server.key -subj "/CN=${SVC}.volcano-system.svc" -out server.csr -config csr.conf
	openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out server.crt -extensions v3_req -extfile csr.conf -days 3650 2>/dev/null
)
kc create secret generic volcano-admission-secret -n volcano-system --from-file=tls.key="$VC/certs/server.key" \
	--from-file=tls.crt="$VC/certs/server.crt" --from-file=ca.crt="$VC/certs/ca.crt" >/dev/null
# ConfigMap keys to files, as a ConfigMap volume would mount them.
cm_file() { kc get cm -n volcano-system "$1" -o json | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["data"][sys.argv[1]], end="")' "$2" >"$VC/conf/$2"; }
cm_file volcano-scheduler-configmap volcano-scheduler.conf
cm_file volcano-admission-configmap volcano-admission.conf

# The three deployments' containers, with the chart's arguments (volcano-development.yaml) and
# loopback addresses; KUBE_POD_NAMESPACE as the chart's env sets it.
start() { # start <name> <args...>
	local name=$1
	shift
	(cd "$VC" && KUBE_POD_NAMESPACE=volcano-system HOME="$VC" exec "$WORK/bin/vc-$name" --kubeconfig "$KUBECONFIG_FILE" --logtostderr "$@" >"$VC/$name.log" 2>&1) &
	PIDS="$PIDS $!"
}
start webhook-manager \
	--enabled-admission=/jobs/mutate,/jobs/validate,/podgroups/validate,/queues/mutate,/queues/validate,/hypernodes/validate,/cronjobs/validate \
	--tls-cert-file="$VC/certs/server.crt" --tls-private-key-file="$VC/certs/server.key" --ca-cert-file="$VC/certs/ca.crt" \
	--admission-conf="$VC/conf/volcano-admission.conf" --webhook-namespace=volcano-system --webhook-service-name=$SVC \
	--listen-address=127.0.0.1 --port="$WEBHOOK_PORT" --enable-healthz=true --healthz-address="127.0.0.1:$(free_port)"
start controller-manager --enable-healthz=true --healthz-address="127.0.0.1:$(free_port)" --enable-metrics=true \
	--listen-address="127.0.0.1:$(free_port)" --leader-elect=false --kube-api-qps=50 --kube-api-burst=100 --worker-threads=3 \
	--worker-threads-for-gc=5 --worker-threads-for-podgroup=5 --sharding-configmap=volcano-sharding-configmap \
	--sharding-configmap-namespace=volcano-system
start scheduler --scheduler-conf="$VC/conf/volcano-scheduler.conf" --enable-healthz=true --healthz-address="127.0.0.1:$(free_port)" \
	--enable-metrics=true --listen-address="127.0.0.1:$(free_port)" --leader-elect=false --kube-api-qps=2000 --kube-api-burst=2000 \
	--schedule-period=1s --node-worker-threads=20 --plugins-dir="$VC/plugins"
ca_patched() { [ -n "$(kc get mutatingwebhookconfiguration volcano-admission-service-jobs-mutate -o jsonpath='{.webhooks[0].clientConfig.caBundle}')" ]; }
wait_until 60 ca_patched >/dev/null || { tail -20 "$VC/webhook-manager.log" >&2; echo "vc-webhook-manager did not patch its CA" >&2; exit 1; }
queues_made() { kc get queue default >/dev/null 2>&1 && kc get queue root >/dev/null 2>&1; }
wait_until 60 queues_made >/dev/null || true
kc apply -f "$here/manifests/02-queue.yaml" >"$WORK/evidence/queue-applied.txt"

twin_up "$ESTATE" "$WORK/twin" "$ADAPTERS" "$KUBECONFIG_FILE"
echo "twin up: $(grep -o 'control API: [^ ]*' "$WORK/twin/up.log"), state $TWIN_STATE" >&2

# ---------------------------------------------------------------- helpers

NS=dc-twin-volcano
TOPO='networkTopology: { mode: hard, highestTierAllowed: 1 }'
vcjob() { # name minAvailable replicas resource per-pod selector [topology]
	sed -e "s|@NAME@|$1|g" -e "s|@MIN@|$2|" -e "s|@REPLICAS@|$3|" -e "s|@RESOURCE@|$4|" -e "s|@PER_POD@|$5|" \
		-e "s|@SELECTOR@|$6|" -e "s|@TOPOLOGY@|${7:-}|" "$here/manifests/vcjob.yaml.tmpl" | tee "$WORK/evidence/vcjob-$1.yaml" | kc apply -f - >/dev/null
}
pod() { # name resource count selector
	sed -e "s|@NAME@|$1|g" -e "s|@RESOURCE@|$2|" -e "s|@PER_POD@|$3|" -e "s|@SELECTOR@|$4|" "$here/manifests/pod.yaml.tmpl" \
		| tee "$WORK/evidence/pod-$1.yaml" | kc apply -f - >/dev/null
}
# Pods bound to a Node with no kubelet never finish terminating; delete them as a kubelet that
# confirmed the kill would.
delete_workload() {
	kc delete jobs.batch.volcano.sh -n "$NS" "$1" --wait=false >/dev/null 2>&1 || true
	kc delete pods -n "$NS" -l "dc-twin.io/workload=$1" --force --grace-period=0 >/dev/null 2>&1 || true
}
bound_nodes() { kc get pods -n "$NS" -l "dc-twin.io/workload=$1" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | grep . || true; }
bound_count() { bound_nodes "$1" | grep -c . || true; }
has_bound() { [ "$(bound_count "$1")" = "$2" ]; }
node_of() { kc get pod -n "$NS" "$1" -o jsonpath='{.spec.nodeName}' 2>/dev/null; }
is_bound() { [ -n "$(node_of "$1")" ]; }
label_of() { kc get node "$1" -o "jsonpath={.metadata.labels.$2}"; }
labels_of() { local n; for n in $(bound_nodes "$1"); do label_of "$n" "$2"; echo; done | sort | uniq -c | awk '{printf "%s%s x%s", (NR>1?", ":""), $2, $1}'; }
cliques_of() { local n; for n in $(bound_nodes "$1"); do label_of "$n" 'nvidia\.com/gpu\.clique'; echo; done | sort -u | grep -c . || true; }
ready_of() { kc get node "$1" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].reason}'; }
cond_of() { kc get node "$1" -o "jsonpath={.status.conditions[?(@.type==\"$2\")].status}"; }
alloc_of() { kc get node "$1" -o "jsonpath={.status.allocatable.$2}"; }
taints_of() { kc get node "$1" -o 'jsonpath={range .spec.taints[*]}{.key}:{.effect} {end}'; }
pg_of() { kc get podgroup -n "$NS" -o name | grep "/$1-" | head -1; }
pg_why() { kc get "$(pg_of "$1")" -n "$NS" -o 'jsonpath={.status.conditions[?(@.type=="Unschedulable")].message}' 2>/dev/null; }
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
cab=0
for w in $(kc get mutatingwebhookconfiguration,validatingwebhookconfiguration -o name | grep volcano-admission); do
	[ -n "$(kc get "$w" -o jsonpath='{.webhooks[0].clientConfig.caBundle}')" ] && cab=$((cab + 1))
done
qstate=$(kc get queue dc-twin -o jsonpath='{.status.state}' 2>/dev/null)
record U3 "Volcano $VOLCANO_REF ($VOLCANO_COMMIT): vc-webhook-manager, vc-controller-manager, vc-scheduler" "3 of 3 running; the CA patched into the 7 webhook configurations; queues root and default made by vc-scheduler; Queue dc-twin admitted through the queue webhooks and Open" "$alive of 3 running; CA in $cab of 7 configurations; queues: $(kc get queues -o jsonpath='{range .items[*]}{.metadata.name}{" "}{end}'); dc-twin state $qstate" "$( [ "$alive" = 3 ] && [ "$cab" = 7 ] && queues_made && [ "$qstate" = Open ] && echo PASS || echo FAIL)"

hn_ready() { [ "$(kc get hypernodes -o jsonpath='{range .items[?(@.spec.tier==1)]}{.status.nodeCount}{"\n"}{end}' | grep -c '^18$')" = 2 ]; }
ms=$(wait_until 60 hn_ready) || true
hns=$(kc get hypernodes -o jsonpath='{range .items[*]}{.metadata.name}(tier {.spec.tier}, {.status.nodeCount} nodes) {end}')
record U4 "HyperNodes discovered from the twin's nvidia.com/gpu.clique labels (label discovery)" "2 tier-1 HyperNodes of 18 Nodes, one per NVL72 rack" "$hns after ${ms} ms" "$(pass_if "$(ok hn_ready)")"
kc get hypernodes -o yaml >"$WORK/evidence/U4-hypernodes.yaml"

# ---------------------------------------------------------------- A: placement on a healthy estate

vcjob nvl72-a 18 18 nvidia.com/gpu 4 'nvidia.com/gpu.product: NVIDIA-GB300' "$TOPO"
ms=$(wait_until 60 has_bound nvl72-a 18) || true
record A1 "Volcano Job nvl72-a: 18 x 4 nvidia.com/gpu, minAvailable 18, networkTopology hard tier 1, GB300" "18 of 18 bound to 18 Nodes in 1 clique" "$(bound_count nvl72-a) bound after ${ms} ms on $(bound_nodes nvl72-a | sort -u | grep -c .) Nodes in $(cliques_of nvl72-a) clique(s) ($(labels_of nvl72-a 'nvidia\.com/gpu\.product'))" "$( [ "$(bound_count nvl72-a)" = 18 ] && [ "$(bound_nodes nvl72-a | sort -u | grep -c .)" = 18 ] && [ "$(cliques_of nvl72-a)" = 1 ] && echo PASS || echo FAIL)"
kc get pods -n "$NS" -l dc-twin.io/workload=nvl72-a -o wide >"$WORK/evidence/A1-gang.txt"

pod h100-x8 nvidia.com/gpu 8 "nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3"
pod a100-x8 nvidia.com/gpu 8 "nvidia.com/gpu.product: NVIDIA-A100-SXM4-80GB"
pod mi355x-x4 amd.com/gpu 4 "amd.com/gpu.product-name: AMD_Instinct_MI355X"
pod gaudi3-x4 habana.ai/gaudi 4 "kubernetes.io/os: linux"
i=2
for spec in "h100-x8 nvidia\\.com/gpu\\.product NVIDIA-H100-80GB-HBM3" "a100-x8 nvidia\\.com/gpu\\.product NVIDIA-A100-SXM4-80GB" \
	"mi355x-x4 amd\\.com/gpu\\.product-name AMD_Instinct_MI355X" "gaudi3-x4 habana\\.ai/gaudi\\.product HL-325L"; do
	set -- $spec
	ms=$(wait_until 30 is_bound "$1") || true
	n=$(node_of "$1")
	got=$([ -n "$n" ] && label_of "$n" "$2" || true)
	what=$(kc get pod -n "$NS" "$1" -o jsonpath='{.spec.containers[0].resources.limits}' | tr -d '{}"')
	record "A$i" "Pod $1 ($what), a PodGroup of one" "bound to a Node labelled $3" "${n:-unbound} ($got) after ${ms} ms" "$( [ "$got" = "$3" ] && echo PASS || echo FAIL)"
	i=$((i + 1))
done

pod amd-anywhere-x2 amd.com/gpu 2 "kubernetes.io/os: linux"
wait_until 30 is_bound amd-anywhere-x2 >/dev/null || true
n=$(node_of amd-anywhere-x2)
has=$([ -n "$n" ] && alloc_of "$n" 'amd\.com/gpu' || true)
record A6 "Pod amd-anywhere-x2 (amd.com/gpu 2, no node selector)" "bound to a Node that advertises amd.com/gpu (compat/kai's A7 is the contrast)" "${n:-unbound}, allocatable amd.com/gpu '${has:-none}'" "$( [ -n "$has" ] && [ "$has" != 0 ] && echo PASS || echo FAIL)"

alloc_q() { kc get queue dc-twin -o "jsonpath={.status.allocated.$1}"; }
q_counted() { [ "$(alloc_q 'nvidia\.com/gpu')" = 88 ]; }
wait_until 30 q_counted >/dev/null || true
record A7 "Queue dc-twin accounting" "nvidia.com/gpu 88 (72 + 8 + 8), amd.com/gpu 6, habana.ai/gaudi 4" "$(kc get queue dc-twin -o jsonpath='{.status.allocated}' | tr -d '{}"')" "$( q_counted && [ "$(alloc_q 'amd\.com/gpu')" = 6 ] && [ "$(alloc_q 'habana\.ai/gaudi')" = 4 ] && echo PASS || echo FAIL)"
kc get queue dc-twin -o yaml >"$WORK/evidence/A7-queue.yaml"

# ---------------------------------------------------------------- B: faults and power, twin side

delete_workload nvl72-a
workload_gone() { [ -z "$(kc get pods -n "$NS" -l "dc-twin.io/workload=$1" -o name)" ]; }
wait_until 30 workload_gone nvl72-a >/dev/null || true

TRAY=a01-compute-tray-07
twin fault inject 'a01/compute-tray-07/gpu-2' nvidia/xid/xid-79 >"$WORK/evidence/B1-inject.json"
xid_settled() { [ "$(ready_of $TRAY | cut -d/ -f1)" = True ] && [ "$(cond_of $TRAY GpuHealthy)" = False ] && [ "$(kc get node $TRAY -o jsonpath='{.spec.unschedulable}')" = true ] && [ "$(alloc_of $TRAY 'nvidia\.com/gpu')" = 3 ]; }
ms=$(wait_until 20 xid_settled) || true
record B1 "dctwin fault inject a01/compute-tray-07/gpu-2 nvidia/xid/xid-79" "$TRAY Ready, GpuHealthy=False, cordoned, allocatable nvidia.com/gpu 3" "Ready=$(ready_of $TRAY), GpuHealthy=$(cond_of $TRAY GpuHealthy), allocatable $(alloc_of $TRAY 'nvidia\.com/gpu'), taints [$(taints_of $TRAY)] after ${ms} ms" "$(pass_if "$(ok xid_settled)")"
OFF_TRAY=a02-compute-tray-03
twin power off 'a02/compute-tray-03' >/dev/null
off_tainted() { [ "$(ready_of "$1")" = "False/PoweredOff" ] && taints_of "$1" | grep -q not-ready; }
ms=$(wait_until 30 off_tainted $OFF_TRAY) || true
record B2 "dctwin power off a02/compute-tray-03" "$OFF_TRAY Ready=False (PoweredOff), not-ready taint from the node-lifecycle controller" "Ready=$(ready_of $OFF_TRAY), taints [$(taints_of $OFF_TRAY)] after ${ms} ms" "$(pass_if "$(ok off_tainted $OFF_TRAY)")"
# the H100 server to power off is one h100-x8 does not hold
OFF_SRV=$(kc get nodes -l nvidia.com/gpu.product=NVIDIA-H100-80GB-HBM3 -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep -v -x -F "$(node_of h100-x8)" | head -1)
OFF_SRV_PATH=$(kc get node "$OFF_SRV" -o jsonpath='{.metadata.annotations.dc-twin\.io/device-path}')
twin power off "$OFF_SRV_PATH" >/dev/null
ms=$(wait_until 30 off_tainted "$OFF_SRV") || true
record B3 "dctwin power off $OFF_SRV_PATH (HGX H100, not h100-x8's)" "$OFF_SRV Ready=False (PoweredOff), tainted not-ready" "Ready=$(ready_of "$OFF_SRV"), taints [$(taints_of "$OFF_SRV")] after ${ms} ms" "$(pass_if "$(ok off_tainted "$OFF_SRV")")"
kc get nodes "$TRAY" "$OFF_TRAY" "$OFF_SRV" >"$WORK/evidence/B-nodes-after-faults.txt"

# ---------------------------------------------------------------- C: gangs and placement after the faults

vcjob nvl72-b 18 18 nvidia.com/gpu 4 "kubernetes.io/os: linux" "$TOPO"
pg_says() { pg_why nvl72-b | grep -q 'tasks in gang unschedulable'; }
wait_until 30 pg_says >/dev/null || true
sleep 5
record C1 "Volcano Job nvl72-b: 18 x 4, minAvailable 18, one HyperNode, no product selector, after the faults" "0 of 18 bound (all or nothing); PodGroup Unschedulable naming the HyperNodes and the faulted Nodes" "$(bound_count nvl72-b) bound; PodGroup: $(pg_why nvl72-b | tr '\n|' '  ' | cut -c1-230)" "$( [ "$(bound_count nvl72-b)" = 0 ] && pg_says && echo PASS || echo FAIL)"
kc get "$(pg_of nvl72-b)" -n "$NS" -o yaml >"$WORK/evidence/C1-podgroup.yaml"

vcjob nvl72-c 17 17 nvidia.com/gpu 4 "kubernetes.io/os: linux" "$TOPO"
ms=$(wait_until 60 has_bound nvl72-c 17) || true
bad=$(bound_nodes nvl72-c | grep -c -e "^$TRAY\$" -e "^$OFF_TRAY\$" || true)
racks=$(for n in $(bound_nodes nvl72-c); do label_of "$n" 'dc-twin\.io/rack'; echo; done | sort -u | tr '\n' ' ')
record C2 "Volcano Job nvl72-c: 17 x 4, one HyperNode, no product selector" "17 of 17 bound in one clique, none on $TRAY or $OFF_TRAY" "$(bound_count nvl72-c) bound after ${ms} ms, $(cliques_of nvl72-c) clique (rack ${racks% }), $bad on a faulted tray" "$( [ "$(bound_count nvl72-c)" = 17 ] && [ "$(cliques_of nvl72-c)" = 1 ] && [ "$bad" = 0 ] && echo PASS || echo FAIL)"
kc get pods -n "$NS" -l dc-twin.io/workload=nvl72-c -o wide >"$WORK/evidence/C2-gang.txt"

# H100: four servers, one off, one holding h100-x8: a gang of three cannot start.
vcjob h100-g 3 3 nvidia.com/gpu 8 "nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3"
pg_h100() { pg_why h100-g | grep -q 'tasks in gang unschedulable'; }
wait_until 30 pg_h100 >/dev/null || true
sleep 5
record C3 "Volcano Job h100-g: 3 x 8 H100, minAvailable 3, with $OFF_SRV off and h100-x8 on another" "0 of 3 bound (two H100 Nodes fit, the gang needs three); PodGroup Unschedulable" "$(bound_count h100-g) bound; PodGroup: $(pg_why h100-g | cut -c1-200)" "$( [ "$(bound_count h100-g)" = 0 ] && pg_h100 && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- D: recovery

twin power on "$OFF_SRV_PATH" >/dev/null
ms=$(wait_until 90 has_bound h100-g 3) || true
record D1 "dctwin power on $OFF_SRV_PATH with h100-g waiting" "the whole gang binds once $OFF_SRV is Ready: 3 of 3, one on $OFF_SRV" "Ready=$(ready_of "$OFF_SRV"); h100-g on $(bound_nodes h100-g | tr '\n' ' ')after ${ms} ms" "$( has_bound h100-g 3 && bound_nodes h100-g | grep -q "^$OFF_SRV\$" && echo PASS || echo FAIL)"

twin fault clear 'a01/compute-tray-07/gpu-2' nvidia/xid/xid-79 >/dev/null
twin power on 'a02/compute-tray-03' >/dev/null
cleared() { [ "$(ready_of $TRAY)" = "True/KubeletReady" ] && [ -z "$(kc get node $TRAY -o jsonpath='{.spec.unschedulable}')" ] && [ "$(alloc_of $TRAY 'nvidia\.com/gpu')" = 4 ] && [ "$(ready_of $OFF_TRAY)" = "True/KubeletReady" ]; }
ms=$(wait_until 60 cleared) || true
record D2 "dctwin fault clear (Xid 79) and power on a02/compute-tray-03" "$TRAY Ready, uncordoned, allocatable 4; $OFF_TRAY Ready" "$TRAY Ready=$(ready_of $TRAY), unschedulable='$(kc get node $TRAY -o jsonpath='{.spec.unschedulable}')', allocatable $(alloc_of $TRAY 'nvidia\.com/gpu'); $OFF_TRAY Ready=$(ready_of $OFF_TRAY) after ${ms} ms" "$(pass_if "$(ok cleared)")"

delete_workload nvl72-c
# nvl72-b (C1) has waited since the faults; a domain with 18 free trays lets it bind.
ms=$(wait_until 90 has_bound nvl72-b 18) || true
record D3 "nvl72-c deleted, the estate healed" "nvl72-b, pending since C1, binds 18 of 18 in one clique" "$(bound_count nvl72-b) bound in $(cliques_of nvl72-b) clique after ${ms} ms" "$( [ "$(bound_count nvl72-b)" = 18 ] && [ "$(cliques_of nvl72-b)" = 1 ] && echo PASS || echo FAIL)"

kc get pods -n "$NS" -o wide >"$WORK/evidence/pods-final.txt"
kc get podgroups -n "$NS" >"$WORK/evidence/podgroups-final.txt"
kc get jobs.batch.volcano.sh -n "$NS" >"$WORK/evidence/vcjobs-final.txt"
twin report >"$WORK/evidence/fidelity-report.txt" 2>&1 || true

status=0
print_table "Volcano $VOLCANO_REF against dc-twin ($(basename "$ESTATE"), kube-controller-manager $K8S_REF). Nodes are the kwok projection: IMPERSONATED (D24); no pod runs." || status=$?
echo
echo "artefacts: $WORK/evidence (kubectl output, rendered workloads), logs: $WORK/volcano, $WORK/cluster/logs, $WORK/twin/up.log"
exit $status

#!/usr/bin/env bash
# Runs NVIDIA Grove and the NVIDIA Dynamo operator, unmodified and built from released tags, with
# NVIDIA KAI Scheduler as the scheduler, against the twin's Kubernetes view of two GB300 NVL72
# racks: a real kube-apiserver and etcd (envtest's binaries), the real kube-controller-manager, and
# dc-twin's kwok projection standing in for 36 kubelets (IMPERSONATED, D24). A disaggregated
# inference PodCliqueSet is packed into one NVLink domain; the twin then faults and powers off trays
# under it and the placement reacts. A DynamoGraphDeployment is rendered by the Dynamo operator into
# Grove's objects and placed the same way. No pod runs: what is tested is placement and control flow.
#
#   compat/grove/run.sh                       # PASS/FAIL table on stdout, artefacts under $WORK
#   WORK=/tmp/grove DCTWIN=bin/dctwin compat/grove/run.sh
#   KAI_BIN=<dir with kai-scheduler, kai-binder, ...> compat/grove/run.sh   # reuse compat/kai's build
#   KCM=none | KCM=/path/kube-controller-manager compat/grove/run.sh
#   DYNAMO=none compat/grove/run.sh           # Grove only (the Dynamo operator needs Docker)
#   KEEP=1 compat/grove/run.sh                # leave everything running; prints how to stop it
#
# Nothing here reads ~/.kube/config: every client is given the scratch cluster's kubeconfig.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
kailib="$root/compat/kai/lib"
GROVE_REF=${GROVE_REF:-v0.1.0-alpha.16}   # github.com/ai-dynamo/grove (NVIDIA/grove redirects there)
DYNAMO_REF=${DYNAMO_REF:-v1.5.1}          # github.com/ai-dynamo/dynamo, deploy/operator
KAI_REF=${KAI_REF:-v0.18.6}               # github.com/kai-scheduler/KAI-Scheduler
K8S_REF=${K8S_REF:-v1.37.0}               # kube-controller-manager, at the envtest apiserver's version
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
KCM=${KCM:-build}                         # build | none | <path to kube-controller-manager>
KAI_BIN=${KAI_BIN:-}                      # a directory of prebuilt kai-<component> binaries at KAI_REF
DYNAMO=${DYNAMO:-container}               # container | none
DYNAMO_NETWORK=${DYNAMO_NETWORK:-auto}    # auto | host (rootless Podman on this machine) | publish (a Docker VM)
ESTATE=${ESTATE:-$here/estate.yaml}       # the checks name its racks a01 and a02
mkdir -p "$WORK/bin" "$WORK/src" "$WORK/evidence"
WORK=$(cd "$WORK" && pwd)

. "$kailib/common.sh"
. "$kailib/cluster.sh"
. "$kailib/twin.sh"
# has <grep args>: grep that reads its whole input, so with pipefail a writer is never cut off by
# SIGPIPE (grep -q stops at the first match, and the pipeline then fails).
has() { grep "$@" >/dev/null; }

ENVTEST_DIR=$(envtest_dir)
[ -x "$ENVTEST_DIR/kube-apiserver" ] || { echo "no envtest binaries: set KUBEBUILDER_ASSETS (setup-envtest use -p path)" >&2; exit 1; }
KUBECTL=${KUBECTL:-$ENVTEST_DIR/kubectl}
export ENVTEST_DIR KUBECTL
command -v "$DCTWIN" >/dev/null || { echo "dctwin not found: set DCTWIN (make build; DCTWIN=bin/dctwin)" >&2; exit 1; }
DCTWIN=$(command -v "$DCTWIN")
command -v helm >/dev/null || { echo "helm not found: the charts' webhook configurations are rendered with helm template" >&2; exit 1; }
if [ "$DYNAMO" = container ] && ! docker info >/dev/null 2>&1; then
	echo "no Docker: run with DYNAMO=none, or start Docker (the Dynamo operator runs in a container; see README)" >&2
	exit 1
fi

# ---------------------------------------------------------------- build from source

clone() { # clone <url> <ref> <dir> [sparse paths...]
	local url=$1 ref=$2 dir=$3
	shift 3
	[ -d "$dir/.git" ] && return 0
	if [ $# -gt 0 ]; then
		git clone -q --depth 1 --branch "$ref" --filter=blob:none --sparse "$url" "$dir" 2>/dev/null
		git -C "$dir" sparse-checkout set "$@"
	else
		git clone -q --depth 1 --branch "$ref" "$url" "$dir" 2>/dev/null
	fi
}

clone https://github.com/ai-dynamo/grove.git "$GROVE_REF" "$WORK/src/grove"
GROVE_COMMIT=$(git -C "$WORK/src/grove" rev-parse HEAD)
if [ ! -x "$WORK/bin/grove-operator" ]; then
	echo "building Grove operator at $GROVE_REF" >&2
	(cd "$WORK/src/grove/operator" && go build -o "$WORK/bin/grove-operator" ./cmd)
fi

clone https://github.com/kai-scheduler/KAI-Scheduler.git "$KAI_REF" "$WORK/src/kai-scheduler"
KAI_COMMIT=$(git -C "$WORK/src/kai-scheduler" rev-parse HEAD)
# KAI's podgroupcontroller parses --metrics-bind-address but, at v0.18.6, builds its manager without
# it (cmd/podgroupcontroller/app/app.go), so its metrics server takes controller-runtime's default
# :8080 on every interface whatever the flag says. It is built and started only when its source uses
# the flag. Nothing here needs it: it writes PodGroup resource status, which no check reads.
KAI_COMPONENTS="scheduler binder podgrouper queuecontroller"
if grep -q 'options.MetricsAddr' "$WORK/src/kai-scheduler/cmd/podgroupcontroller/app/app.go"; then
	PGC=started KAI_COMPONENTS="$KAI_COMPONENTS podgroupcontroller"
else
	PGC=skipped
fi
KAI_DIR=${KAI_BIN:-$WORK/bin}
for c in $KAI_COMPONENTS; do
	if [ ! -x "$KAI_DIR/kai-$c" ]; then
		[ -z "$KAI_BIN" ] || { echo "KAI_BIN=$KAI_BIN has no kai-$c" >&2; exit 1; }
		echo "building KAI $c at $KAI_REF" >&2
		(cd "$WORK/src/kai-scheduler" && go build -o "$KAI_DIR/kai-$c" "./cmd/$c")
	fi
done

case "$KCM" in
none) KCM_BIN= ;;
build)
	KCM_BIN="$WORK/bin/kube-controller-manager"
	if [ ! -x "$KCM_BIN" ]; then
		clone https://github.com/kubernetes/kubernetes.git "$K8S_REF" "$WORK/src/kubernetes"
		echo "building kube-controller-manager $K8S_REF (once; about ten minutes)" >&2
		(cd "$WORK/src/kubernetes" && go build -o "$KCM_BIN" ./cmd/kube-controller-manager)
	fi
	;;
*) KCM_BIN=$KCM ;;
esac

# The Dynamo operator reads its namespace from the in-cluster service-account file
# (/var/run/secrets/kubernetes.io/serviceaccount/namespace, internal/cert/cert.go) and refuses to
# start without it, so it runs as a Linux container of this machine's architecture, built here from
# source into an image holding only the static binary and that one file.
DYNAMO_IMAGE=dctwin-dynamo-operator:${DYNAMO_REF#v}
if [ "$DYNAMO" = container ]; then
	clone https://github.com/ai-dynamo/dynamo.git "$DYNAMO_REF" "$WORK/src/dynamo" deploy/operator deploy/helm
	DYNAMO_COMMIT=$(git -C "$WORK/src/dynamo" rev-parse HEAD)
	darch=$(docker info --format '{{.Architecture}}' 2>/dev/null || true)
	[ -n "$darch" ] && [ "$darch" != "<no value>" ] || darch=$(docker info --format '{{.Host.Arch}}' 2>/dev/null || uname -m)
	case "$darch" in aarch64 | arm64) darch=arm64 ;; x86_64 | amd64) darch=amd64 ;; esac
	img="$WORK/dynamo-image"
	if [ ! -x "$img/rootfs/manager" ]; then
		echo "building the Dynamo operator at $DYNAMO_REF (linux/$darch)" >&2
		mkdir -p "$img/rootfs/var/run/secrets/kubernetes.io/serviceaccount" "$img/rootfs/etc/dynamo" "$img/rootfs/tmp"
		(cd "$WORK/src/dynamo/deploy/operator" && CGO_ENABLED=0 GOOS=linux GOARCH=$darch go build -o "$img/rootfs/manager" ./cmd)
		printf dynamo-system >"$img/rootfs/var/run/secrets/kubernetes.io/serviceaccount/namespace"
		printf 'FROM scratch\nCOPY rootfs/ /\nENV KUBECONFIG=/etc/dynamo/kubeconfig HOME=/tmp\nUSER 65532:65532\nENTRYPOINT ["/manager"]\n' >"$img/Dockerfile"
	fi
	docker build -q -t "$DYNAMO_IMAGE" "$img" >/dev/null
fi

# ---------------------------------------------------------------- bring-up

PIDS=""
DYNAMO_CTR=dctwin-dynamo-operator
cleanup() {
	set +e
	if [ "${KEEP:-}" = 1 ]; then
		echo "KEEP=1: left running. Stop with: kill $PIDS; docker rm -f $DYNAMO_CTR; HOME=$TWIN_HOME $DCTWIN down --state $TWIN_STATE; kill $CLUSTER_PIDS" >&2
		return
	fi
	if [ "$DYNAMO" = container ]; then
		docker logs "$DYNAMO_CTR" >"$WORK/serving/dynamo-operator.log" 2>&1
		docker rm -f "$DYNAMO_CTR" >/dev/null 2>&1
		[ "${KEEP_IMAGE:-}" = 1 ] || docker rmi "$DYNAMO_IMAGE" >/dev/null 2>&1
	fi
	local p
	for p in $PIDS; do kill "$p" 2>/dev/null; done
	for p in $PIDS; do wait "$p" 2>/dev/null; done
	twin_down
	cluster_down
}
trap cleanup EXIT
trap 'exit 130' INT TERM

rm -rf "$WORK/cluster" "$WORK/twin" "$WORK/serving" "$WORK/evidence"
mkdir -p "$WORK/serving" "$WORK/evidence"
cluster_up "$WORK/cluster" "$KCM_BIN"
echo "kube-apiserver $APISERVER_URL (envtest $(basename "$ENVTEST_DIR")); kube-controller-manager: ${KCM_BIN:-none}" >&2
kc apply --server-side -f "$WORK/src/kai-scheduler/deployments/kai-scheduler/crds/" \
	-f "$WORK/src/grove/operator/api/core/v1alpha1/crds/" -f "$WORK/src/grove/scheduler/api/core/v1alpha1/crds/" >/dev/null
crds_established() {
	[ "$(kc get crd -o 'jsonpath={range .items[*]}{.status.conditions[?(@.type=="Established")].status}{"\n"}{end}' | grep -c True)" -ge 12 ]
}
wait_until 60 crds_established >/dev/null || { echo "CRDs not established" >&2; exit 1; }
kc apply -f "$here/manifests/00-namespaces.yaml" -f "$here/manifests/01-queues.yaml" -f "$root/compat/kai/manifests/03-priorityclasses.yaml" >/dev/null

twin_up "$ESTATE" "$WORK/twin" kwok "$KUBECONFIG_FILE"
echo "twin up: $(grep -o 'control API: [^ ]*' "$WORK/twin/up.log"), state $TWIN_STATE" >&2

start() { # start <log name> <command...>
	local name=$1
	shift
	KUBECONFIG="$KUBECONFIG_FILE" HOME="$WORK/twin/home" "$@" >"$WORK/serving/$name.log" 2>&1 &
	PIDS="$PIDS $!"
}
start kai-scheduler "$KAI_DIR/kai-scheduler" --scheduler-name kai-scheduler --namespace kai-scheduler \
	--listen-address "127.0.0.1:$(free_port)" --plugin-server-port "$(free_port)"
start kai-binder "$KAI_DIR/kai-binder" --scheduler-name kai-scheduler --resource-reservation-namespace kai-resource-reservation \
	--metrics-bind-address "127.0.0.1:$(free_port)" --health-probe-bind-address "127.0.0.1:$(free_port)"
start kai-podgrouper "$KAI_DIR/kai-podgrouper" --scheduler-name kai-scheduler \
	--metrics-bind-address "127.0.0.1:$(free_port)" --health-probe-bind-address "127.0.0.1:$(free_port)"
start kai-queuecontroller "$KAI_DIR/kai-queuecontroller" --enable-webhook=false --metrics-listen-address "127.0.0.1:$(free_port)"
if [ "$PGC" = started ]; then
	start kai-podgroupcontroller "$KAI_DIR/kai-podgroupcontroller" --scheduler-name kai-scheduler --enable-podgroup-webhook=false \
		--metrics-bind-address "127.0.0.1:$(free_port)" --health-probe-bind-address "127.0.0.1:$(free_port)"
fi

# A scratch CA and a serving certificate for 127.0.0.1, for both operators' webhooks.
P=$WORK/serving/pki
mkdir -p "$P/grove" "$P/dynamo"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=dctwin-serving-ca -keyout "$P/ca.key" -out "$P/ca.crt" 2>/dev/null
printf 'subjectAltName=IP:127.0.0.1\n' >"$P/san"
for d in grove dynamo; do
	openssl req -newkey rsa:2048 -nodes -subj /CN=127.0.0.1 -keyout "$P/$d/tls.key" -out "$P/$d.csr" 2>/dev/null
	openssl x509 -req -in "$P/$d.csr" -CA "$P/ca.crt" -CAkey "$P/ca.key" -CAcreateserial -days 2 -extfile "$P/san" -out "$P/$d/tls.crt" 2>/dev/null
done

# Grove: its chart's operator configuration with KAI as the default scheduler profile and
# topology-aware scheduling on, manual certificates, loopback ports; the chart's webhook
# configurations (helm template) pointed at the process by URL.
GWP=$(free_port)
cat >"$WORK/serving/grove-config.yaml" <<CFG
apiVersion: operator.config.grove.io/v1alpha1
kind: OperatorConfiguration
runtimeClientConnection: { qps: 100, burst: 150 }
leaderElection: { enabled: false, leaseDuration: 15s, renewDeadline: 10s, retryPeriod: 2s, resourceLock: leases, resourceName: grove-operator-leader-election, resourceNamespace: grove-system }
server:
  webhooks: { bindAddress: 127.0.0.1, port: $GWP, serverCertDir: $P/grove, secretName: grove-webhook-server-cert, certProvisionMode: manual }
  healthProbes: { bindAddress: 127.0.0.1, port: $(free_port) }
  metrics: { bindAddress: 127.0.0.1, port: $(free_port) }
controllers:
  podCliqueSet: { concurrentSyncs: 3 }
  podClique: { concurrentSyncs: 3 }
  podCliqueScalingGroup: { concurrentSyncs: 3 }
scheduler:
  defaultProfileName: kai-scheduler
  profiles: [ { name: default-scheduler }, { name: kai-scheduler } ]
logLevel: info
logFormat: json
topologyAwareScheduling: { enabled: true }
network: { autoMNNVLEnabled: false }
CFG
HOME="$WORK/twin/home" helm template grove "$WORK/src/grove/operator/charts" -n grove-system \
	--set config.server.webhooks.certProvisionMode=manual --set config.topologyAwareScheduling.enabled=true >"$WORK/serving/grove-chart.yaml"
python3 -I "$here/lib/webhooks.py" "$WORK/serving/grove-chart.yaml" "$WORK/serving/grove-webhooks.yaml" "$GWP" "$P/ca.crt"
start grove-operator env GROVE_INIT_CONTAINER_IMAGE=registry.invalid/dc-twin/grove-initc "$WORK/bin/grove-operator" --config "$WORK/serving/grove-config.yaml"
webhook_up() { curl -sk -o /dev/null "https://127.0.0.1:$1/"; }
wait_until 60 webhook_up "$GWP" >/dev/null || { echo "Grove's webhook server did not start; see $WORK/serving/grove-operator.log" >&2; exit 1; }
kc apply -f "$WORK/serving/grove-webhooks.yaml" >/dev/null

# ---------------------------------------------------------------- helpers

NS=dc-twin-serving
pods_of() { kc get pods -n "$NS" -l "app.kubernetes.io/part-of=$1" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}'; }
replica_pods() { pods_of "$1" | grep "^$1-$2-" || true; }   # <pcs> <replica index>
bound_in() { replica_pods "$1" "$2" | awk 'NF == 2' | grep -c . || true; }
total_in() { replica_pods "$1" "$2" | grep -c . || true; }
nodes_in() { replica_pods "$1" "$2" | awk 'NF == 2 {print $2}'; }
label_of() { kc get node "$1" -o "jsonpath={.metadata.labels.$2}"; }
cliques_in() { local n; for n in $(nodes_in "$1" "$2" | sort -u); do label_of "$n" 'nvidia\.com/gpu\.clique'; echo; done | sort -u | grep . || true; }
racks_in() { local n; for n in $(nodes_in "$1" "$2" | sort -u); do label_of "$n" 'dc-twin\.io/rack'; echo; done | sort -u | tr '\n' ' ' | sed 's/ $//'; }
gpu_trays_in() { # distinct Nodes holding the replica's GPU pods
	replica_pods "$1" "$2" | awk 'NF == 2 && $1 !~ /-frontend-/ {print $2}' | sort -u | grep -c . || true
}
all_bound() { local n; n=$(total_in "$1" "$2"); [ "$n" -gt 0 ] && [ "$(bound_in "$1" "$2")" = "$n" ] && [ "$n" = "${3:-$n}" ]; }
node_of() { kc get pod -n "$NS" "$1" -o jsonpath='{.spec.nodeName}' 2>/dev/null; }
pod_on() { pods_of "$1" | awk -v n="$2" '$2 == n {print $1}'; }
first_pod() { pods_of "$1" | awk -v p="$2" 'NF == 2 && index($1, p) == 1 {print $1, $2; exit}'; }   # <pcs> <name prefix>
newest_pod() { # <pcs> <name prefix>: the most recently created pod of a PodClique (Grove's replacement)
	kc get pods -n "$NS" -l "app.kubernetes.io/part-of=$1" --sort-by=.metadata.creationTimestamp \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | awk -v p="$2" 'index($1, p) == 1' | tail -1
}
replaced() { # <pcs> <prefix> <old pod>: a newer pod of the PodClique exists and is bound
	local r
	r=$(newest_pod "$1" "$2")
	[ -n "$r" ] && [ "$r" != "$3" ] && [ -n "$(node_of "$r")" ]
}
recreated() { local r; r=$(newest_pod "$1" "$2"); [ -n "$r" ] && [ "$r" != "$3" ]; }
gone() { [ -z "$(kc get pod -n "$NS" "$1" -o name 2>/dev/null)" ]; }
ready_of() { kc get node "$1" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].reason}'; }
cond_of() { kc get node "$1" -o "jsonpath={.status.conditions[?(@.type==\"$2\")].status}"; }
alloc_of() { kc get node "$1" -o 'jsonpath={.status.allocatable.nvidia\.com/gpu}'; }
cordoned() { [ "$(kc get node "$1" -o jsonpath='{.spec.unschedulable}')" = true ]; }
taints_of() { kc get node "$1" -o 'jsonpath={range .spec.taints[*]}{.key}:{.effect} {end}'; }
selector_of() { echo "${1%%-*}/${1#*-}"; }   # a01-compute-tray-07 -> a01/compute-tray-07
podgroups_of() { kc get podgroups -n "$NS" -l "app.kubernetes.io/part-of=$1" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'; }
pg_reason() { kc get podgroup -n "$NS" "$1" -o 'jsonpath={.status.schedulingConditions[0].reasons[0].message}' 2>/dev/null; }
free_trays() { # Ready, schedulable trays of a rack with all four GPUs unrequested
	local rack=$1 n used
	for n in $(kc get nodes -l "dc-twin.io/rack=$rack" -o jsonpath='{.items[*].metadata.name}'); do
		[ "$(ready_of "$n" | cut -d/ -f1)" = True ] || continue
		cordoned "$n" && continue
		used=$(kc get pods -A --field-selector "spec.nodeName=$n" -o jsonpath='{range .items[*]}{.spec.containers[0].resources.limits.nvidia\.com/gpu}{"\n"}{end}' | awk '{s+=$1} END {print s+0}')
		[ "$used" = 0 ] && echo "$n"
	done
}
render_pcs() { # name replicas prefill decode prefill-min decode-min
	sed -e "s|@NAME@|$1|" -e "s|@REPLICAS@|$2|" -e "s|@PREFILL@|$3|" -e "s|@DECODE@|$4|" \
		-e "s|@PREFILL_MIN@|$5|" -e "s|@DECODE_MIN@|$6|" "$here/manifests/workloads/disagg-pcs.yaml.tmpl"
}
delete_pcs() { # Grove deletes the pods; with no kubelet to confirm, bound ones are force-deleted as a kubelet would confirm
	kc delete pcs -n "$NS" "$1" --wait=false >/dev/null 2>&1 || true
	local i
	for i in $(seq 1 40); do
		kc delete pods -n "$NS" -l "app.kubernetes.io/part-of=$1" --force --grace-period=0 >/dev/null 2>&1 || true
		[ -z "$(pods_of "$1")" ] && [ -z "$(kc get pcs -n "$NS" "$1" -o name 2>/dev/null)" ] && return 0
		sleep 0.5
	done
}
ok() { "$@" >/dev/null 2>&1 && echo 0 || echo 1; }
yes_no() { if [ "$1" -eq 0 ]; then echo PASS; else echo FAIL; fi; }

# ---------------------------------------------------------------- U: the twin's Nodes, KAI and Grove

hosts=36
node_count_is() { [ "$(kc get nodes -o name | grep -c .)" -eq "$1" ]; }
ms=$(wait_until 60 node_count_is $hosts) || true
ready=$(kc get nodes -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c True || true)
imp=$(kc get nodes -o jsonpath='{range .items[*]}{.metadata.annotations.dc-twin\.io/impersonated}{"\n"}{end}' | grep -c kwok || true)
record U1 "Nodes the twin registers (kwok projection)" "$hosts Ready GB300 trays, all dc-twin.io/impersonated=kwok" "$ready Ready, $imp impersonated, after ${ms} ms" "$( [ "$ready" = $hosts ] && [ "$imp" = $hosts ] && echo PASS || echo FAIL)"
kc get nodes -L nvidia.com/gpu.product,nvidia.com/gpu.clique,dc-twin.io/rack >"$WORK/evidence/nodes.txt"
gpus=$(kc get nodes -o 'jsonpath={range .items[*]}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}' | awk '{s+=$1} END {print s+0}')
ncl=$(kc get nodes -o 'jsonpath={range .items[*]}{.metadata.labels.nvidia\.com/gpu\.clique}{"\n"}{end}' | sort -u | grep -c . || true)
record U2 "GPU allocatable and NVLink cliques" "nvidia.com/gpu 144; 2 distinct nvidia.com/gpu.clique (one per NVL72 rack)" "nvidia.com/gpu $gpus; $ncl cliques" "$( [ "$gpus" = 144 ] && [ "$ncl" = 2 ] && echo PASS || echo FAIL)"

notready_taints() { kc get nodes -o jsonpath='{range .items[*]}{.spec.taints[*].key}{"\n"}{end}' | grep -c 'node.kubernetes.io/not-ready' || true; }
if [ -n "$KCM_BIN" ]; then
	no_notready_taints() { [ "$(notready_taints)" = 0 ]; }
	ms=$(wait_until 60 no_notready_taints) || true
	record U3 "not-ready:NoSchedule creation taint lifted by the node-lifecycle controller" "0 Nodes keep it" "$(notready_taints) keep it, after ${ms} ms" "$( [ "$(notready_taints)" = 0 ] && echo PASS || echo FAIL)"
else
	for nd in $(kc get nodes -o jsonpath='{.items[*].metadata.name}'); do kc taint node "$nd" node.kubernetes.io/not-ready:NoSchedule- >/dev/null 2>&1 || true; done
	record U3 "not-ready:NoSchedule taint on Ready Nodes" "removed by this harness (KCM=none)" "$(notready_taints) Nodes keep it" CANNOT
fi

alive=0 want=0
for p in $PIDS; do want=$((want + 1)); kill -0 "$p" 2>/dev/null && alive=$((alive + 1)); done
record U4 "KAI $KAI_REF ($KAI_COMMIT) and Grove $GROVE_REF ($GROVE_COMMIT) running" "$want of $want processes (KAI scheduler, binder, podgrouper, queuecontroller$( [ "$PGC" = started ] && echo ", podgroupcontroller"); grove-operator with its webhooks)" "$alive of $want" "$( [ $alive = $want ] && echo PASS || echo FAIL)"
if [ "$PGC" = skipped ]; then
	record U6 "KAI podgroupcontroller's --metrics-bind-address" "honoured, so it can run beside other services" "not started: $KAI_REF never passes the flag to its manager (cmd/podgroupcontroller/app/app.go), whose metrics server binds :8080 on all interfaces" FINDING
fi

kc apply -f "$here/manifests/02-clustertopologybinding.yaml" >/dev/null
topo_synced() {
	[ "$(kc get clustertopologybinding nvlink -o 'jsonpath={.status.schedulerTopologyStatuses[0].inSync}')" = true ] &&
		[ "$(kc get topologies.kai.scheduler nvlink -o 'jsonpath={.spec.levels[*].nodeLabel}')" = "topology.kubernetes.io/zone nvidia.com/gpu.clique kubernetes.io/hostname" ]
}
ms=$(wait_until 30 topo_synced) || true
record U5 "ClusterTopologyBinding nvlink (zone, nvlink-domain, host) synced to a KAI Topology by Grove" "KAI Topology nvlink with levels zone, nvidia.com/gpu.clique, hostname; binding inSync" "levels [$(kc get topologies.kai.scheduler nvlink -o 'jsonpath={.spec.levels[*].nodeLabel}' 2>/dev/null)], inSync=$(kc get clustertopologybinding nvlink -o 'jsonpath={.status.schedulerTopologyStatuses[0].inSync}') after ${ms} ms" "$(yes_no "$(ok topo_synced)")"
kc get topologies.kai.scheduler nvlink -o yaml >"$WORK/evidence/U5-kai-topology.yaml"

# ---------------------------------------------------------------- G0: a scaled PodGang is constrained on its own

# Six a01 trays are taken (other tenants' pods), so a01 has 12 free and a02 18. A replica whose
# prefill and decode scaling groups keep only one instance in the base PodGang (minAvailable 1)
# needs 8 trays for that gang and 4 for each of two scaled gangs: 16 in one domain would fit in a02.
for i in 01 02 03 04 05 06; do
	sed -e "s|@NAME@|filler-a01-$i|" -e "s|@NODE@|a01-compute-tray-$i|" "$here/manifests/workloads/filler-pod.yaml.tmpl"
	echo "---"
done >"$WORK/evidence/G0-fillers.yaml"
kc apply -f "$WORK/evidence/G0-fillers.yaml" >/dev/null
fillers_bound() { [ "$(kc get pods -n "$NS" -l dc-twin.io/workload=filler -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | grep -c .)" = 6 ]; }
wait_until 30 fillers_bound >/dev/null || true
render_pcs split 1 2 2 1 1 | tee "$WORK/evidence/G0-pcs.yaml" | kc apply -f - >/dev/null 2>&1
ms=$(wait_until 90 all_bound split 0 17) || true
ngangs=$(podgroups_of split | grep -c . || true)
cl=$(cliques_in split 0 | grep -c . || true)
record G0 "PodCliqueSet replica (pack.required nvlink-domain) whose scaling groups have minAvailable 1, with 12 trays free in a01 and 18 in a02" "the whole replica in one NVLink domain" "$(bound_in split 0)/17 bound after ${ms} ms as $ngangs KAI PodGroups, across $cl cliques (racks $(racks_in split 0)): each scaled PodGang is packed on its own (Grove's GREP-244 lists it as a limitation)" "$( [ "$cl" = 1 ] && echo PASS || echo FINDING)"
kc get pods -n "$NS" -l app.kubernetes.io/part-of=split -o wide >"$WORK/evidence/G0-pods.txt"
kc get podgroups -n "$NS" -l app.kubernetes.io/part-of=split -o yaml >"$WORK/evidence/G0-podgroups.yaml"
delete_pcs split
kc delete pods -n "$NS" -l dc-twin.io/workload=filler --force --grace-period=0 >/dev/null 2>&1 || true

# ---------------------------------------------------------------- A: a disaggregated replica in one NVLink domain

render_pcs disagg 1 2 2 2 2 | tee "$WORK/evidence/A-pcs.yaml" | kc apply -f - >/dev/null 2>&1
ms=$(wait_until 90 all_bound disagg 0 17) || true
gang=$(podgroups_of disagg | head -1)
cl=$(cliques_in disagg 0 | grep -c . || true)
trays=$(gpu_trays_in disagg 0)
gb300=$(for n in $(nodes_in disagg 0 | sort -u); do label_of "$n" 'nvidia\.com/gpu\.product'; echo; done | grep -c NVIDIA-GB300 || true)
record A1 "PodCliqueSet disagg: frontend + 2 prefill + 2 decode instances (leader and 3 workers, 4 GPUs each), pack.required nvlink-domain" "17 of 17 bound, the 16 GPU pods on 16 GB300 trays, 1 clique" "$(bound_in disagg 0)/17 bound after ${ms} ms, GPU pods on $trays trays, $gb300 GB300 Nodes, $cl clique (rack $(racks_in disagg 0))" "$( [ "$(bound_in disagg 0)" = 17 ] && [ "$trays" = 16 ] && [ "$cl" = 1 ] && echo PASS || echo FAIL)"
pgq=$(kc get podgroup -n "$NS" "$gang" -o jsonpath='{.spec.queue}')
pgmin=$(kc get podgroup -n "$NS" "$gang" -o jsonpath='{.spec.minMember}')
pgsub=$(kc get podgroup -n "$NS" "$gang" -o 'jsonpath={range .spec.subGroups[*]}{.name}{"\n"}{end}' | grep -c . || true)
pgtop=$(kc get podgroup -n "$NS" "$gang" -o 'jsonpath={.spec.topologyConstraint.topology}/{.spec.topologyConstraint.requiredTopologyLevel}')
record A2 "PodGang to KAI PodGroup (Grove's KAI backend)" "one PodGroup, queue serving, required level nvidia.com/gpu.clique of topology nvlink, a subgroup per PodClique" "$(podgroups_of disagg | grep -c .) PodGroup: queue $pgq, minMember $pgmin, $pgsub subgroups, topology $pgtop" "$( [ "$(podgroups_of disagg | grep -c .)" = 1 ] && [ "$pgq" = serving ] && [ "$pgtop" = nvlink/nvidia.com/gpu.clique ] && [ "$pgsub" = 9 ] && echo PASS || echo FAIL)"
kc get pods -n "$NS" -l app.kubernetes.io/part-of=disagg -o wide >"$WORK/evidence/A1-pods.txt"
kc get pcs,pclq,pcsg,podgang -n "$NS" >"$WORK/evidence/A1-grove-objects.txt"
kc get podgroup -n "$NS" "$gang" -o yaml >"$WORK/evidence/A2-podgroup.yaml"
D0=$(racks_in disagg 0)
D1=$( [ "$D0" = a01 ] && echo a02 || echo a01)

kc patch pcs -n "$NS" disagg --type merge -p '{"spec":{"replicas":2}}' >/dev/null 2>&1
ms=$(wait_until 90 all_bound disagg 1 17) || true
c0=$(cliques_in disagg 0) c1=$(cliques_in disagg 1)
record A3 "Scale to 2 replicas" "replica 1: 17 of 17 bound in the other NVLink domain" "$(bound_in disagg 1)/17 bound after ${ms} ms in $(cliques_in disagg 1 | grep -c . || true) clique (rack $(racks_in disagg 1)); replica 0 in rack $D0" "$( [ "$(bound_in disagg 1)" = 17 ] && [ "$(echo "$c1" | grep -c .)" = 1 ] && [ "$c0" != "$c1" ] && echo PASS || echo FAIL)"

kc patch pcs -n "$NS" disagg --type merge -p '{"spec":{"replicas":3}}' >/dev/null 2>&1
created3() { [ "$(total_in disagg 2)" = 17 ]; }
wait_until 30 created3 >/dev/null || true
sleep 15
gang2=$(podgroups_of disagg | grep '^disagg-2-' | head -1)
why=$(pg_reason "$gang2" | tr '\n|' '  ' | cut -c1-170)
record A4 "Scale to 3 replicas (each domain has 2 free trays)" "replica 2: 0 of 17 bound (gang is all-or-nothing), PodGroup Unschedulable on the clique requirement" "$(bound_in disagg 2)/17 bound after 15 s; PodGroup: $why" "$( [ "$(bound_in disagg 2)" = 0 ] && echo "$why" | has 'nvidia.com/gpu.clique' && echo PASS || echo FAIL)"
kc get podgroup -n "$NS" "$gang2" -o yaml >"$WORK/evidence/A4-podgroup.yaml" 2>&1 || true
kc patch pcs -n "$NS" disagg --type merge -p '{"spec":{"replicas":2}}' >/dev/null 2>&1
gone3() { [ "$(total_in disagg 2)" = 0 ]; }
wait_until 30 gone3 >/dev/null || true

# ---------------------------------------------------------------- B: twin faults under replica 0

read -r XPOD XNODE <<<"$(first_pod disagg disagg-0-decode-0-dworker-)"
twin fault inject "$(selector_of "$XNODE")/gpu-1" nvidia/xid/xid-79 >"$WORK/evidence/B1-inject.json"
xid_settled() { [ "$(ready_of "$1" | cut -d/ -f1)" = True ] && [ "$(cond_of "$1" GpuHealthy)" = False ] && cordoned "$1" && [ "$(alloc_of "$1")" = 3 ]; }
ms=$(wait_until 20 xid_settled "$XNODE") || true
record B1 "dctwin fault inject $(selector_of "$XNODE")/gpu-1 nvidia/xid/xid-79, under decode worker $XPOD" "$XNODE Ready, GpuHealthy=False, cordoned, allocatable 3; the pod is not evicted (D43)" "Ready=$(ready_of "$XNODE"), GpuHealthy=$(cond_of "$XNODE" GpuHealthy), unschedulable=$(kc get node "$XNODE" -o jsonpath='{.spec.unschedulable}'), allocatable $(alloc_of "$XNODE") after ${ms} ms; $XPOD on $(node_of "$XPOD")" "$( xid_settled "$XNODE" && [ "$(node_of "$XPOD")" = "$XNODE" ] && echo PASS || echo FAIL)"

# The remediation NVSentinel or an operator would take: drain the pod off the faulted tray.
spare0=$(free_trays "$D0" | tr '\n' ' ')
kc delete pod -n "$NS" "$XPOD" --force --grace-period=0 >/dev/null 2>&1
ms=$(wait_until 60 replaced disagg disagg-0-decode-0-dworker- "$XPOD") || true
RPOD=$(newest_pod disagg disagg-0-decode-0-dworker-)
RNODE=$(node_of "$RPOD")
record B2 "Drain the pod off the Xid tray: Grove recreates it, KAI places the replacement" "bound in rack $D0 (the gang's clique) on a spare healthy tray (spare: ${spare0% }), not $XNODE" "$RPOD on ${RNODE:-nothing} (rack $( [ -n "$RNODE" ] && label_of "$RNODE" 'dc-twin\.io/rack'), clique same: $( [ -n "$RNODE" ] && [ "$(label_of "$RNODE" 'nvidia\.com/gpu\.clique')" = "$(echo "$c0")" ] && echo yes || echo no)) after ${ms} ms" "$( [ -n "$RNODE" ] && [ "$RNODE" != "$XNODE" ] && [ "$(label_of "$RNODE" 'nvidia\.com/gpu\.clique')" = "$c0" ] && echo PASS || echo FAIL)"

read -r OPOD ONODE <<<"$(first_pod disagg disagg-0-prefill-0-pworker-)"
spare0=$(free_trays "$D0" | tr '\n' ' ')
t0=$(now_ms)
twin power off "$(selector_of "$ONODE")" >/dev/null
# The controller taints a Node whose Ready is False not-ready and one whose Ready is Unknown (its
# lease lapsed, as a powered-off host's does) unreachable; either NoExecute taint evicts.
not_ready_tainted() { [ "$(ready_of "$1" | cut -d/ -f1)" != True ] && taints_of "$1" | has -E '(not-ready|unreachable):NoExecute'; }
taint_ms=$(wait_until 60 not_ready_tainted "$ONODE") || true
tainted=$(ok not_ready_tainted "$ONODE")
wait_until 60 gone "$OPOD" >/dev/null || true
evict_s=$(( ($(now_ms) - t0) / 1000 ))
ms=$(wait_until 60 replaced disagg disagg-0-prefill-0-pworker- "$OPOD") || true
OREP=$(newest_pod disagg disagg-0-prefill-0-pworker-)
ORNODE=$(node_of "$OREP")
record B3 "dctwin power off $(selector_of "$ONODE"), under prefill worker $OPOD" "Ready not True, a not-ready or unreachable NoExecute taint; kube-controller-manager evicts the pod after its 10 s toleration; the replacement is bound in rack $D0 on the last spare (${spare0% })" "Ready=$(ready_of "$ONODE"), taints [$(taints_of "$ONODE")] ${taint_ms} ms after power-off; $OPOD evicted ${evict_s} s after power-off; replacement $OREP on ${ORNODE:-nothing} after ${ms} ms" "$( [ -n "$KCM_BIN" ] && [ "$tainted" = 0 ] && gone "$OPOD" && [ -n "$ORNODE" ] && [ "$ORNODE" != "$ONODE" ] && [ "$(label_of "$ORNODE" 'nvidia\.com/gpu\.clique')" = "$c0" ] && echo PASS || echo FAIL)"

read -r ZPOD ZNODE <<<"$(first_pod disagg disagg-0-decode-1-dworker-)"
twin fault inject "$(selector_of "$ZNODE")/gpu-0" nvidia/xid/xid-79 >/dev/null
wait_until 20 xid_settled "$ZNODE" >/dev/null || true
spare1=$(free_trays "$D1" | tr '\n' ' ')
kc delete pod -n "$NS" "$ZPOD" --force --grace-period=0 >/dev/null 2>&1
wait_until 30 recreated disagg disagg-0-decode-1-dworker- "$ZPOD" >/dev/null || true
sleep 15
ZREP=$(newest_pod disagg disagg-0-decode-1-dworker-)
zmsg=$(kc get pod -n "$NS" "${ZREP:-none}" -o 'jsonpath={.status.conditions[?(@.type=="PodScheduled")].message}' 2>/dev/null | tr '\n' ' ' | sed 's/  */ /g' | cut -c1-150)
record B4 "A third tray of rack $D0 faulted (Xid 79 on $ZNODE) and its decode worker drained; rack $D1 has free trays (${spare1% })" "the replacement stays Pending: the gang's pods stay in its clique, none goes to $D1" "${ZREP:-no pod} unbound after 15 s: $zmsg" "$( [ -n "$ZREP" ] && [ -z "$(node_of "$ZREP")" ] && echo PASS || echo FAIL)"
kc get pods -n "$NS" -l app.kubernetes.io/part-of=disagg -o wide >"$WORK/evidence/B-pods-after-faults.txt"
kc describe node "$XNODE" >"$WORK/evidence/B1-describe-node.txt"

# ---------------------------------------------------------------- D: recovery

twin fault clear "$(selector_of "$ZNODE")/gpu-0" nvidia/xid/xid-79 >/dev/null
ms=$(wait_until 60 replaced disagg disagg-0-decode-1-dworker- "$ZPOD") || true
record D1 "dctwin fault clear on $ZNODE with $ZREP Pending" "$ZNODE uncordoned, allocatable 4; KAI binds the Pending pod there" "$ZREP on $(node_of "${ZREP:-none}" || true) after ${ms} ms; unschedulable='$(kc get node "$ZNODE" -o jsonpath='{.spec.unschedulable}')', allocatable $(alloc_of "$ZNODE")" "$( [ -n "$ZREP" ] && [ "$(node_of "$ZREP")" = "$ZNODE" ] && echo PASS || echo FAIL)"

twin power on "$(selector_of "$ONODE")" >/dev/null
back() { [ "$(ready_of "$1")" = True/KubeletReady ] && ! taints_of "$1" | has -E 'not-ready|unreachable'; }
ms=$(wait_until 60 back "$ONODE") || true
record D2 "dctwin power on $(selector_of "$ONODE")" "Ready, the controller's not-ready and unreachable taints lifted" "Ready=$(ready_of "$ONODE"), taints [$(taints_of "$ONODE")] after ${ms} ms" "$(yes_no "$(ok back "$ONODE")")"

twin fault clear "$(selector_of "$XNODE")/gpu-1" nvidia/xid/xid-79 >/dev/null
cleared() { [ "$(ready_of "$1")" = True/KubeletReady ] && ! cordoned "$1" && [ "$(alloc_of "$1")" = 4 ]; }
ms=$(wait_until 20 cleared "$XNODE") || true
record D3 "dctwin fault clear on $XNODE" "Ready, uncordoned, allocatable 4" "Ready=$(ready_of "$XNODE"), unschedulable='$(kc get node "$XNODE" -o jsonpath='{.spec.unschedulable}')', allocatable $(alloc_of "$XNODE") after ${ms} ms" "$(yes_no "$(ok cleared "$XNODE")")"
kc get pods -n "$NS" -l app.kubernetes.io/part-of=disagg -o wide >"$WORK/evidence/D-pods-after-recovery.txt"
record D4 "PodCliqueSet replicas Available" "not reached: no kubelet makes a pod Ready (Grove's PodGang Ready condition stays False)" "available $(kc get pcs -n "$NS" disagg -o jsonpath='{.status.availableReplicas}'); PodGang Ready=$(kc get podgang -n "$NS" -o 'jsonpath={.items[0].status.conditions[?(@.type=="Ready")].status}')" CANNOT
delete_pcs disagg

# ---------------------------------------------------------------- E: the Dynamo operator, through Grove

if [ "$DYNAMO" = container ]; then
	DWP=$(free_port) DMP=$(free_port) DHP=$(free_port)
	AP=${APISERVER_URL##*:}
	# With rootless Podman the container shares this machine's network namespace (--network host:
	# no port is published and no firewall rule is added) and reaches the apiserver on 127.0.0.1,
	# serving its webhooks there on loopback ports. With a Docker VM (Colima, Docker Desktop) it
	# reaches this machine's loopback through the VM's host address (Lima's host.lima.internal,
	# 192.168.5.2; Docker Desktop's host.docker.internal) and its webhook port is published on
	# 127.0.0.1.
	if [ "$DYNAMO_NETWORK" = auto ]; then
		if docker --version 2>/dev/null | has -i podman; then DYNAMO_NETWORK=host; else DYNAMO_NETWORK=publish; fi
	fi
	if [ "$DYNAMO_NETWORK" = host ]; then
		DHOST=127.0.0.1 DWHOST=127.0.0.1 DNET="--network host"
	else
		DHOST=${DYNAMO_APISERVER_HOST:-192.168.5.2} DWHOST=0.0.0.0 DNET="-p 127.0.0.1:$DWP:$DWP"
	fi
	DE="$WORK/serving/dynamo-etc"
	mkdir -p "$DE/certs"
	cp "$P/dynamo/tls.crt" "$P/dynamo/tls.key" "$DE/certs/"
	sed "s#server: https://127.0.0.1:$AP#server: https://$DHOST:$AP#" "$KUBECONFIG_FILE" >"$DE/kubeconfig"
	# The chart's operator configuration (helm template of components/operator with Grove and KAI
	# enabled), with manual certificates, loopback metrics and probes, and no leader election.
	HOME="$WORK/twin/home" helm template dynamo "$WORK/src/dynamo/deploy/helm/charts/platform/components/operator" -n dynamo-system \
		--set discoveryBackend=kubernetes --set global.grove.enabled=true --set global.kai-scheduler.enabled=true >"$WORK/serving/dynamo-chart.yaml"
	cat >"$DE/config.yaml" <<CFG
apiVersion: operator.config.dynamo.nvidia.com/v1alpha1
kind: OperatorConfiguration
server:
  metrics: { bindAddress: "127.0.0.1", port: $DMP, secure: false }
  healthProbe: { bindAddress: "127.0.0.1", port: $DHP }
  webhook: { host: "$DWHOST", port: $DWP, certDir: /etc/dynamo/certs, certProvisionMode: "manual", secretName: "webhook-server-cert", serviceName: dynamo-operator-webhook-service }
leaderElection: { enabled: false, id: "dynamo.nvidia.com", namespace: "dynamo-system" }
orchestrators:
  kaiScheduler: { enabled: true }
  grove: { enabled: true }
rbac:
  plannerClusterRoleName: dynamo-dynamo-operator-planner
  dgdrProfilingClusterRoleName: dynamo-dynamo-operator-dgdr-profiling
  eppClusterRoleName: dynamo-dynamo-operator-epp
mpi: { sshSecretName: "mpi-run-ssh-secret", sshSecretNamespace: "dynamo-system" }
CFG
	chmod -R a+rX "$DE"
	kc create namespace dynamo-system >/dev/null
	kc -n dynamo-system create secret generic webhook-server-cert --from-file=ca.crt="$P/ca.crt" \
		--from-file=tls.crt="$P/dynamo/tls.crt" --from-file=tls.key="$P/dynamo/tls.key" >/dev/null
	python3 -I -c '
import re, sys
docs = open(sys.argv[1]).read().split("\n---\n")
keep = [d for d in docs if re.search(r"^kind: (ClusterRole|ClusterRoleBinding|Role|RoleBinding|ServiceAccount)\s*$", d, re.M)]
open(sys.argv[2], "w").write("\n---\n".join(keep) + "\n")' "$WORK/serving/dynamo-chart.yaml" "$WORK/serving/dynamo-rbac.yaml"
	kc apply -n dynamo-system -f "$WORK/serving/dynamo-rbac.yaml" >/dev/null
	# CRDs as shipped: their conversion webhook names the chart's Service, which the operator checks
	# for at start-up before it injects the CA.
	kc apply --server-side -f "$WORK/src/dynamo/deploy/operator/config/crd/bases/" >/dev/null
	python3 -I "$here/lib/webhooks.py" "$WORK/serving/dynamo-chart.yaml" "$WORK/serving/dynamo-webhooks.yaml" "$DWP" "$P/ca.crt"
	kc apply -f "$WORK/serving/dynamo-webhooks.yaml" >/dev/null

	docker rm -f "$DYNAMO_CTR" >/dev/null 2>&1 || true
	# 1 CPU and 512 MiB; without them where rootless Podman has no cgroup controllers delegated.
	# shellcheck disable=SC2086
	docker create --name "$DYNAMO_CTR" --memory 512m --cpus 1 $DNET "$DYNAMO_IMAGE" \
		--config=/etc/dynamo/config.yaml --operator-version="${DYNAMO_REF#v}" >/dev/null 2>"$WORK/serving/dynamo-create.err" ||
		docker create --name "$DYNAMO_CTR" $DNET "$DYNAMO_IMAGE" \
			--config=/etc/dynamo/config.yaml --operator-version="${DYNAMO_REF#v}" >/dev/null
	docker cp "$DE/." "$DYNAMO_CTR:/etc/dynamo/" >/dev/null
	docker start "$DYNAMO_CTR" >/dev/null
	injected() { docker logs "$DYNAMO_CTR" 2>&1 | has 'CRD conversion webhook CA bundle injected'; }
	wait_until 90 injected >/dev/null || true
	# No Service can carry the apiserver to a port on this machine's loopback (Endpoints refuse
	# 127.0.0.0/8), so once the operator has injected its CA the conversion webhooks are pointed at it
	# by URL with that CA; without this the operator's v1alpha1 informers never sync.
	cab=$(base64 <"$P/ca.crt" | tr -d '\n')
	for c in dynamographdeployments dynamocomponentdeployments dynamographdeploymentrequests dynamographdeploymentscalingadapters; do
		kc patch crd "$c.nvidia.com" --type json -p "[{\"op\":\"replace\",\"path\":\"/spec/conversion/webhook/clientConfig\",\"value\":{\"url\":\"https://127.0.0.1:$DWP/convert\",\"caBundle\":\"$cab\"}}]" >/dev/null
	done
	workers_started() { docker logs "$DYNAMO_CTR" 2>&1 | has '"controller": "DynamoGraphDeployment".*Starting workers\|Starting workers.*"controller": "DynamoGraphDeployment"'; }
	ms=$(wait_until 120 workers_started) || true
	gates=$(docker logs "$DYNAMO_CTR" 2>&1 | grep -o 'Resolved operator feature gates.*' | head -1 | grep -o '"grove": [a-z]*, "lws": [a-z]*, "kaiScheduler": [a-z]*' || true)
	record E1 "Dynamo operator $DYNAMO_REF ($DYNAMO_COMMIT) in container $DYNAMO_CTR" "running; Grove and KAI detected from the API (grove: true, kaiScheduler: true); DynamoGraphDeployment controller started" "$(docker inspect -f '{{.State.Status}}' "$DYNAMO_CTR" 2>/dev/null); gates {$gates}; controller started after ${ms} ms" "$( [ "$(docker inspect -f '{{.State.Status}}' "$DYNAMO_CTR")" = running ] && echo "$gates" | has '"grove": true.*"kaiScheduler": true' && workers_started && echo PASS || echo FAIL)"

	sed "s|@NAME@|vllm-disagg|" "$here/manifests/workloads/dgd.yaml.tmpl" | tee "$WORK/evidence/E-dgd.yaml" | kc apply -f - >"$WORK/evidence/E-dgd-apply.txt" 2>&1 || true
	pcs_made() { [ -n "$(kc get pcs -n "$NS" vllm-disagg -o name 2>/dev/null)" ] && [ "$(kc get pclq -n "$NS" -l app.kubernetes.io/part-of=vllm-disagg -o name | grep -c .)" = 5 ]; }
	ms=$(wait_until 90 pcs_made) || true
	ptop=$(kc get pcs -n "$NS" vllm-disagg -o 'jsonpath={.spec.template.topologyConstraint.topologyName}/{.spec.template.topologyConstraint.pack.required}' 2>/dev/null)
	pcsgs=$(kc get pcsg -n "$NS" -l app.kubernetes.io/part-of=vllm-disagg -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)
	pclqs=$(kc get pclq -n "$NS" -l app.kubernetes.io/part-of=vllm-disagg -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | sed 's/vllm-disagg-0-//g')
	tla=$(kc get dgd -n "$NS" vllm-disagg -o 'jsonpath={.status.conditions[?(@.type=="TopologyLevelsAvailable")].status}' 2>/dev/null)
	record E2 "DynamoGraphDeployment vllm-disagg (Frontend, 2-node prefill, 2-node decode, topologyConstraint nvlink/nvlink-domain)" "a PodCliqueSet packed on nvlink/nvlink-domain, PodCliqueScalingGroups prefill and decode, leader and worker PodCliques; TopologyLevelsAvailable" "after ${ms} ms: PCS topology $ptop; PCSGs [$pcsgs]; PodCliques [$pclqs]; TopologyLevelsAvailable=$tla" "$( [ "$ptop" = nvlink/nvlink-domain ] && pcs_made && [ "$tla" = True ] && echo PASS || echo FAIL)"
	ms=$(wait_until 90 all_bound vllm-disagg 0 5) || true
	dgang=$(podgroups_of vllm-disagg | head -1)
	dq=$(kc get podgroup -n "$NS" "${dgang:-none}" -o jsonpath='{.spec.queue}' 2>/dev/null)
	dtop=$(kc get podgroup -n "$NS" "${dgang:-none}" -o 'jsonpath={.spec.topologyConstraint.requiredTopologyLevel}' 2>/dev/null)
	dcl=$(cliques_in vllm-disagg 0 | grep -c . || true)
	record E3 "Its pods placed by KAI" "5 of 5 bound, the 4 GPU pods on 4 GB300 trays, 1 clique; PodGroup queue serving (from nvidia.com/kai-scheduler-queue), required nvidia.com/gpu.clique" "$(bound_in vllm-disagg 0)/5 bound after ${ms} ms, GPU pods on $(gpu_trays_in vllm-disagg 0) trays, $dcl clique (rack $(racks_in vllm-disagg 0)); PodGroup queue $dq, required $dtop" "$( [ "$(bound_in vllm-disagg 0)" = 5 ] && [ "$(gpu_trays_in vllm-disagg 0)" = 4 ] && [ "$dcl" = 1 ] && [ "$dq" = serving ] && [ "$dtop" = nvidia.com/gpu.clique ] && echo PASS || echo FAIL)"
	kc get pods -n "$NS" -l app.kubernetes.io/part-of=vllm-disagg -o wide >"$WORK/evidence/E3-pods.txt"
	kc get pcs -n "$NS" vllm-disagg -o yaml >"$WORK/evidence/E2-pcs.yaml"

	# Dynamo gives its workers a 60 s termination grace period. On a powered-off tray no kubelet
	# confirms the kill, so the evicted pod stays Terminating, as it would behind a dead node, until
	# the node is declared out of service (Kubernetes' non-graceful node shutdown: the
	# node.kubernetes.io/out-of-service taint, after which kube-controller-manager's pod GC
	# force-deletes it).
	read -r WPOD WNODE <<<"$(first_pod vllm-disagg vllm-disagg-0-decode-0-decode-wkr-)"
	dcl0=$(cliques_in vllm-disagg 0)
	twin power off "$(selector_of "$WNODE")" >/dev/null
	terminating() { [ -n "$(kc get pod -n "$NS" "$1" -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null)" ]; }
	ms=$(wait_until 90 terminating "$WPOD") || true
	sleep 5
	still=$(terminating "$WPOD" && echo Terminating || echo gone)
	record E4 "dctwin power off $(selector_of "$WNODE"), under Dynamo's decode worker $WPOD (grace period 60 s)" "kube-controller-manager evicts it after its 10 s toleration; with no kubelet to confirm, it stays Terminating" "deletion requested after ${ms} ms; 5 s later: $still on $(node_of "$WPOD")" "$( [ "$still" = Terminating ] && echo PASS || echo FAIL)"
	kc taint node "$WNODE" node.kubernetes.io/out-of-service=nodeshutdown:NoExecute >/dev/null
	ms=$(wait_until 60 gone "$WPOD") || true
	ms2=$(wait_until 60 replaced vllm-disagg vllm-disagg-0-decode-0-decode-wkr- "$WPOD") || true
	WREP=$(newest_pod vllm-disagg vllm-disagg-0-decode-0-decode-wkr-)
	WRNODE=$(node_of "${WREP:-none}")
	record E5 "Taint $WNODE node.kubernetes.io/out-of-service=nodeshutdown:NoExecute (non-graceful node shutdown)" "pod GC force-deletes the pod; Grove recreates it; KAI binds the replacement in the same clique, on another tray" "deleted after ${ms} ms; $WREP on ${WRNODE:-nothing} (same clique: $( [ -n "$WRNODE" ] && [ "$(label_of "$WRNODE" 'nvidia\.com/gpu\.clique')" = "$dcl0" ] && echo yes || echo no)) ${ms2} ms later" "$( [ -n "$WRNODE" ] && [ "$WRNODE" != "$WNODE" ] && [ "$(label_of "$WRNODE" 'nvidia\.com/gpu\.clique')" = "$dcl0" ] && echo PASS || echo FAIL)"
	twin power on "$(selector_of "$WNODE")" >/dev/null
	kc taint node "$WNODE" node.kubernetes.io/out-of-service- >/dev/null 2>&1 || true
	state=$(kc get dgd -n "$NS" vllm-disagg -o jsonpath='{.status.state}')
	record E6 "DynamoGraphDeployment Ready" "not reached: no kubelet runs the frontend or workers" "state $state, Ready=$(kc get dgd -n "$NS" vllm-disagg -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}'): $(kc get dgd -n "$NS" vllm-disagg -o 'jsonpath={.status.conditions[?(@.type=="Ready")].message}' | cut -c1-110)" CANNOT
	kc get dgd -n "$NS" vllm-disagg -o yaml >"$WORK/evidence/E-dgd-status.yaml"
fi

twin report >"$WORK/evidence/fidelity-report.txt" 2>&1 || true

status=0
print_table "Grove $GROVE_REF$( [ "$DYNAMO" = container ] && echo " and the Dynamo operator $DYNAMO_REF") with KAI Scheduler $KAI_REF against dc-twin ($(basename "$ESTATE"), kube-controller-manager: $( [ -n "$KCM_BIN" ] && echo "$K8S_REF" || echo none)). Nodes are the kwok projection: IMPERSONATED (D24); no pod runs." || status=$?
echo
echo "artefacts: $WORK/evidence; logs: $WORK/serving, $WORK/cluster/logs, $WORK/twin/up.log"
exit $status

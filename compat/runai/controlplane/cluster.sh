#!/usr/bin/env bash
# Runs inside the control-plane guest after install-cp.sh: Run:ai's cluster side over the twin's
# Nodes (lib/bringup.sh, as run.sh), plus Run:ai's agent and cluster-sync connected to the control
# plane with the cluster's own credentials, then node pools, a project with quota and a workload
# made through the control plane's API, checked on the twin's Nodes.
#
#   cluster.sh <cp dir> <work dir>
#
# <cp dir> is install-cp.sh's output (cp.env). Binaries come from ~/bin (dctwin, kai-*, krm-*,
# runai-*, karta-operator, kube-controller-manager, helm), sources from ~/src (krm, kai-scheduler,
# run-ai, karta, workload-map), envtest from KUBEBUILDER_ASSETS. It uses sudo (the guest is its
# own): to write the agent's secret file and to give two services network namespaces.
set -euo pipefail
CPD=${1:?cp dir} WORK=${2:?work dir}
here=$(cd "$(dirname "$0")/.." && pwd)
root=$(cd "$here/../.." && pwd)
kailib="$root/compat/kai/lib"
# shellcheck disable=SC1091
. "$CPD/cp.env"
BIN=${BIN:-$HOME/bin} SRC=${SRC:-$HOME/src}
mkdir -p "$WORK/bin" "$WORK/src"
WORK=$(cd "$WORK" && pwd)
. "$kailib/common.sh"
. "$kailib/cluster.sh"
. "$kailib/twin.sh"
has() { grep "$@" >/dev/null; }
ENVTEST_DIR=$(envtest_dir)
KUBECTL=${KUBECTL:-$ENVTEST_DIR/kubectl}
export ENVTEST_DIR KUBECTL
DCTWIN=$BIN/dctwin KAI_DIR=$BIN KCM_BIN=$BIN/kube-controller-manager
ESTATE=${ESTATE:-$root/examples/estates/lab-model.yaml}
RUNAI_WORKLOADS=build RUNAI_REF=${RUNAI_REF:-3.0.20}
ADAPTERS=${ADAPTERS:-kwok}   # only the Nodes are needed here; the BMC dialects need assets the guest has not fetched
for b in "$BIN"/krm-* "$BIN"/runai-*; do ln -sf "$b" "$WORK/bin/"; done
ln -sfn "$SRC/krm" "$WORK/src/krm"
ln -sfn "$SRC/kai-scheduler" "$WORK/src/kai-scheduler"
ln -sfn "$SRC/run-ai" "$WORK/src/run-ai"
KARTA_DIR=$SRC/karta
KARTA_CRDS=$SRC/workload-map/charts/karta/crds/ HARNESS_KARTA=0   # the Karta CRD of the chart's karta 0.2.6
RUNAI_PROFILE=1   # KAI and KRM with the names and switches a Run:ai cluster gives them (lib/bringup.sh)
RUNAI_COMMIT=$(git -C "$SRC/run-ai" rev-parse HEAD 2>/dev/null || echo "$RUNAI_REF")
KRM_REF=v0.18.3 KAI_REF=v0.18.6
KRM_COMMIT=$(git -C "$SRC/krm" rev-parse HEAD 2>/dev/null || echo "$KRM_REF")
KAI_COMMIT=$(git -C "$SRC/kai-scheduler" rev-parse HEAD 2>/dev/null || echo "$KAI_REF")

. "$here/lib/bringup.sh"

# ---------------------------------------------------------------- Run:ai's API
api() { # api METHOD PATH [json]: as the control plane's admin user
	local t
	t=$(curl -sf --max-time 60 -X POST "$CP_URL/auth/realms/runai/protocol/openid-connect/token" --data-urlencode grant_type=password \
		--data-urlencode client_id=runai --data-urlencode "username=$CP_ADMIN_EMAIL" --data-urlencode "password=$CP_ADMIN_PASSWORD" \
		--data-urlencode scope=openid | jq -r .access_token)
	if [ $# -gt 2 ]; then
		curl -s --max-time 60 -X "$1" -H "Authorization: Bearer $t" -H 'Content-Type: application/json' -d "$3" "$CP_URL$2"
	else
		curl -s --max-time 60 -X "$1" -H "Authorization: Bearer $t" "$CP_URL$2"
	fi
}
E=$WORK/evidence
mkdir -p "$E"
# The cluster install-cp.sh registered, while it has never connected; after that a new one per run,
# so every node pool, project and workload below is made by this run.
CLUSTER_NAME=$CP_CLUSTER_NAME UUID=$CP_CLUSTER_UUID
if [ "$(api GET /v1/k8s/clusters | jq -r --arg u "$UUID" '.[] | select(.uuid == $u) | .status.state')" != WaitingToConnect ]; then
	CLUSTER_NAME=$CP_CLUSTER_NAME-$(date +%H%M%S)
	UUID=$(api POST /v1/k8s/clusters "{\"name\": \"$CLUSTER_NAME\"}" | jq -r .uuid)
fi
echo "cluster $CLUSTER_NAME $UUID" >&2

# ---------------------------------------------------------------- the cluster's credentials and Run:ai's cluster CRDs
# What the cluster chart's install hook does (helm-hooks-handler, control_plane_details_downloader.go):
# with the installer's client credentials, read the cluster's control-plane config, which carries
# the agent's client.
it=$(curl -sf -X POST "$CP_URL/auth/realms/runai/protocol/openid-connect/token" --data-urlencode grant_type=client_credentials \
	--data-urlencode "client_id=$CP_INSTALLER_CLIENT_ID" --data-urlencode "client_secret=$CP_INSTALLER_SECRET" | jq -r .access_token)
curl -sf -H "Authorization: Bearer $it" "$CP_URL/api/v1/clusters/$UUID/config/control-plane" >"$WORK/runai/cp-config.json"
AGENT_CLIENT=$(jq -r .agent.client.name "$WORK/runai/cp-config.json")
AGENT_SECRET=$(jq -r .agent.client.secret "$WORK/runai/cp-config.json")
CHART_CRDS="$SRC/run-ai/runai-cluster/cluster/runai-chart"
kc apply --server-side --force-conflicts -f "$CHART_CRDS/charts/crds/crds/" -f "$CHART_CRDS/crds/" >/dev/null 2>"$E/crds.err" || true
# The cluster chart's namespace for Run:ai's own objects (the agent's ConfigMaps, runai-public).
kc create namespace runai >/dev/null 2>&1 || true

# ---------------------------------------------------------------- network namespaces
# The agent, cluster-sync and external-workload-integrator all start controller-runtime's metrics
# server at its default :8080, with no setting to move it (each cmd/main.go passes no Metrics
# options); in a cluster each has a pod, and so a network namespace, of its own. Here the agent
# keeps the guest's and the other two get one each, as their pods would: a namespace whose loopback
# carries the API server's port and the control plane's 443, each bridged over a Unix socket to the
# guest's (socat), with the control plane's name resolving to that loopback (ip netns's
# /etc/netns/<ns>/hosts). Out of a pod each reads the kubeconfig KUBECONFIG names (cluster-sync:
# KUBE_CONFIG_FILE_PATH, its out-of-cluster fallback in cmd/main.go).
command -v socat >/dev/null || sudo apt-get install -y -qq socat >/dev/null
SOCK=$WORK/runai/sock NETNS=""
aport=${APISERVER_URL##*:}
mkdir -p "$SOCK"
rm -f "$SOCK"/*.sock
bridge() { # bridge <ns> <name> <port in the namespace> <guest address:port>
	socat "UNIX-LISTEN:$SOCK/$1-$2.sock,fork" "TCP:$4" >"$WORK/runai/socat-$1-$2.log" 2>&1 &
	PIDS="$PIDS $!"
	sudo ip netns exec "$1" socat "TCP-LISTEN:$3,bind=127.0.0.1,fork,reuseaddr" "UNIX-CONNECT:$SOCK/$1-$2.sock" >>"$WORK/runai/socat-$1-$2.log" 2>&1 &
	PIDS="$PIDS $!"
}
ns_up() { # ns_up <ns>
	sudo ip netns del "$1" 2>/dev/null || true
	sudo ip netns add "$1"
	sudo ip -n "$1" link set lo up
	sudo mkdir -p "/etc/netns/$1"
	printf '127.0.0.1 localhost %s\n' "$CP_DOMAIN" | sudo tee "/etc/netns/$1/hosts" >/dev/null
	NETNS="$NETNS $1"
	bridge "$1" apiserver "$aport" "127.0.0.1:$aport"
	bridge "$1" controlplane 443 "$CP_DOMAIN:443"
	sleep 1
}
# Run:ai's own services are restarted when they exit, as their Deployments' pods would be; each
# exit is counted in <log name>.restarts.
supervise() { # supervise <log name> <command...>
	local name=$1
	shift
	(
		trap 'kill "$c" 2>/dev/null; exit 0' TERM INT
		n=0
		while :; do
			KUBECONFIG="$KUBECONFIG_FILE" HOME="$WORK/twin/home" "$@" >>"$WORK/runai/$name.log" 2>&1 &
			c=$!
			rc=0
			wait "$c" || rc=$?
			n=$((n + 1))
			echo "$n exit=$rc $(date -u +%T)" >>"$WORK/runai/$name.restarts"
			sleep 5
		done
	) &
	PIDS="$PIDS $!"
}
start_in() { # start_in <ns> <log name> <env assignments and command...>
	local ns=$1 name=$2
	shift 2
	supervise "$name" sudo ip netns exec "$ns" sudo -u "$(id -un)" env KUBECONFIG="$KUBECONFIG_FILE" KUBE_CONFIG_FILE_PATH="$KUBECONFIG_FILE" \
		HOME="$WORK/twin/home" "$@"
}
ns_down() { # what runs in a namespace runs as root (ip netns exec), so it is stopped through it
	local ns
	if [ "${KEEP:-}" = 1 ]; then
		echo "KEEP=1: also, for ns in$NETNS: sudo ip netns pids \$ns | xargs -r sudo kill; sudo ip netns del \$ns" >&2
		return
	fi
	for ns in $NETNS; do sudo ip netns pids "$ns" 2>/dev/null | xargs -r sudo kill 2>/dev/null; done
	sleep 1
	for ns in $NETNS; do sudo ip netns del "$ns" 2>/dev/null; done
}
trap 'cleanup; ns_down' EXIT

# ---------------------------------------------------------------- scheduler shards
# Here node pools, the default one included, arrive from the control plane through the agent, at
# any time; the harness does the KAI operator's part continuously: a scheduler for every
# SchedulingShard KRM creates (lib/bringup.sh start_shard).
(
	trap 'for p in $SHARD_PIDS; do kill "$p" 2>/dev/null; done; exit 0' TERM INT
	while :; do
		sync_shards >/dev/null 2>&1 || true
		sleep 3
	done
) &
PIDS="$PIDS $!"

# ---------------------------------------------------------------- workload types
# A workload type reaches the cluster as a Karta the agent writes from the control plane's list.
# The workload-map operator the cluster chart deploys (karta 0.2.6) validates it and marks its kind
# present (CRDExists: a CRD, or a built-in kind such as batch/v1 Job); Run:ai's
# external-workload-integrator then grants the kind's RBAC and marks it SyncEnabled; cluster-sync
# reports that phase to the control plane, which accepts workloads only of a Ready type. The
# operator runs with the chart's arguments, the integrator with its Deployment's environment.
start karta-operator "$BIN/karta-operator" --metrics-bind-address "127.0.0.1:$(free_port)" \
	--health-probe-bind-address "127.0.0.1:$(free_port)"
ns_up dctwin-ewi
start_in dctwin-ewi runai-external-workload-integrator METRICS_PORT="$(free_port)" "$BIN/runai-external-workload-integrator"

# go-operator, the cluster's operator, reports the cluster's version and status to cluster-sync in
# the runai-public ConfigMap (operands/runai-public), from the RunaiConfig it reconciles and the
# operands it deploys. It does not run here (its operands are Deployments, which need kubelets), so
# the harness writes runai-public in its format: the chart's version, the platform and the API
# server's version, no operand status, and the two dependencies go-operator always reports. Neither
# exists behind the twin's Nodes (no GPU Operator, no Prometheus), so both say so; the control plane
# rejects a report without them. IMPERSONATED: go-operator's, written by the harness.
KUBE_VERSION=$(kc version -o json 2>/dev/null | jq -r .serverVersion.gitVersion)
cat >"$WORK/runai/runai-public.yaml" <<RP
version: $RUNAI_REF
runaiConfigStatus:
  operands: {}
dependencies:
  required:
    gpu-stack: { available: false, reason: "no GPU Operator behind dc-twin's Nodes (written by compat/runai)" }
    prometheus: { available: false, reason: "no Prometheus in the twin's cluster (written by compat/runai)" }
platform:
  type: kubernetes
  kubeVersion: $KUBE_VERSION
RP
kc -n runai create configmap runai-public --from-literal=cluster-version="$RUNAI_REF" \
	--from-file=runai-public="$WORK/runai/runai-public.yaml" >/dev/null

# ---------------------------------------------------------------- the agent and cluster-sync
# Run:ai's agent (desired state down) and cluster-sync (state up), configured by environment as
# go-operator's Deployments configure them (operands/agent, operands/cluster-sync), with the
# cluster's ID and the agent client. The twin's Nodes carry GPU Feature Discovery's labels, not
# Node Feature Discovery's, so cluster-sync is told which label marks a GPU node.
# Both read the client and the cluster ID from /etc/secret/secret.json, the "whisper" secret
# go-operator writes per Deployment (internal/secrets/vault.go) and mounts there
# (common/constants/path.go). The harness writes that file, as the kubelet's mount would.
sudo mkdir -p /etc/secret
jq -n --arg n "$AGENT_CLIENT" --arg s "$AGENT_SECRET" --arg c "$UUID" '{CLIENT_NAME: $n, CLIENT_SECRET: $s, CLUSTER_ID: $c}' |
	sudo tee /etc/secret/secret.json >/dev/null
sudo chmod 644 /etc/secret/secret.json
common_env=(ENDPOINT="$CP_URL" CLUSTER_DOMAIN="$CP_URL" CLUSTER_VERSION="$RUNAI_REF")
[ -s "$WORK/runai/ads.key" ] || openssl rand -hex 16 >"$WORK/runai/ads.key"
supervise runai-agent env "${common_env[@]}" METRICS_PORT="$(free_port)" WEBHOOK_PORT="$(free_port)" CERT_DIR="$P" \
	TLS_REJECT_UNAUTHORIZED=true RUNAI_ADS_ENCRYPTION_KEY="$(cat "$WORK/runai/ads.key")" "$BIN/runai-agent"
ns_up dctwin-cs
start_in dctwin-cs runai-cluster-sync "${common_env[@]}" METRICS_PORT="$(free_port)" SSL_CERTIFICATE_VERIFICATION=true \
	NVIDIA_GPU_LABEL=nvidia.com/gpu.present "$BIN/runai-cluster-sync"

TR_NODES=50
# ---------------------------------------------------------------- C: through the control plane
# Connected, or MissingPrerequisites: connected, with the required dependencies reported unavailable.
connected() { case "$(api GET /v1/k8s/clusters | jq -r --arg u "$UUID" '.[] | select(.uuid == $u) | .status.state')" in Connected | MissingPrerequisites) ;; *) return 1 ;; esac; }
ms=$(wait_until 300 connected) || true
api GET /v1/k8s/clusters >"$E/C1-clusters.json"
record C1 "Cluster $CLUSTER_NAME in the control plane, after Run:ai's agent and cluster-sync start" "state Connected (MissingPrerequisites while its GPU stack and Prometheus are reported absent)" "state $(jq -r --arg u "$UUID" '.[] | select(.uuid == $u) | .status.state' "$E/C1-clusters.json"), version $(jq -r --arg u "$UUID" '.[] | select(.uuid == $u) | .version' "$E/C1-clusters.json") after ${ms} ms" "$(connected && echo PASS || echo FAIL)"

nodes_seen() { [ "$(api GET "/api/v1/nodes?filterBy=clusterUuid==$UUID&limit=500" | jq '.nodes | length')" -ge $TR_NODES ]; }
ms=$(wait_until 300 nodes_seen) || true
api GET "/api/v1/nodes?filterBy=clusterUuid==$UUID&limit=500" >"$E/C2-nodes.json"
record C2 "The twin's Nodes in the control plane (cluster-sync)" "$TR_NODES nodes with their GPUs" "$(jq '.nodes | length' "$E/C2-nodes.json") nodes, $(jq '[.nodes[].gpuInfo.gpuCount // 0] | add' "$E/C2-nodes.json") GPUs, GB300 nodes $(jq '[.nodes[] | select(.gpuInfo.gpuType == "NVIDIA-GB300")] | length' "$E/C2-nodes.json") after ${ms} ms" "$(nodes_seen && echo PASS || echo FAIL)"

api POST /api/v1/node-pools "{\"name\": \"gb300\", \"clusterId\": \"$UUID\", \"labelKey\": \"nvidia.com/gpu.product\", \"labelValue\": \"NVIDIA-GB300\"}" >"$E/C3-nodepool.json"
pool_in_cluster() { kc get nodepools.kai.resources gb300 >/dev/null 2>&1 && [ "$(kc get nodes -l "$NODEPOOL_LABEL=gb300" -o name | grep -c .)" = 18 ]; }
ms=$(wait_until 180 pool_in_cluster) || true
record C3 "Node pool gb300 (nvidia.com/gpu.product=NVIDIA-GB300) created through the API" "the agent writes the NodePool; KRM labels the 18 GB300 trays and adds a shard" "NodePool in cluster: $(kc get nodepools.kai.resources gb300 -o name 2>/dev/null || echo none); $(kc get nodes -l "$NODEPOOL_LABEL=gb300" -o name | grep -c . || true) Nodes labelled $NODEPOOL_LABEL=gb300; shards $(kc get schedulingshards -o jsonpath='{.items[*].metadata.name}' 2>/dev/null) after ${ms} ms" "$(pool_in_cluster && echo PASS || echo FAIL)"

POOL_ID=$(api GET "/api/v1/node-pools?filterBy=clusterId==$UUID" | jq -r '.nodepools[]? | select(.name == "gb300") | .id' | head -1)
api POST /api/v1/org-unit/projects "{\"name\": \"llm-train\", \"clusterId\": \"$UUID\", \"defaultNodePools\": [\"gb300\"], \"resources\": [{\"nodePool\": {\"id\": \"$POOL_ID\", \"name\": \"gb300\"}, \"gpu\": {\"deserved\": 72, \"limit\": 72}}]}" >"$E/C4-project.json"
PROJECT_ID=$(jq -r '.id // empty' "$E/C4-project.json")
project_in_cluster() { kc get projects.kai.resources llm-train >/dev/null 2>&1 && kc get queues -o name 2>/dev/null | has llm-train; }
ms=$(wait_until 180 project_in_cluster) || true
record C4 "Project llm-train with 72 GB300 GPUs deserved and gb300 its default node pool, created through the API" "the agent writes the Project; KRM makes its namespace and its gb300 queue" "project $PROJECT_ID; in cluster: $(kc get projects.kai.resources -o name 2>/dev/null | tr '\n' ' ')queues $(kc get queues -o jsonpath='{range .items[*]}{.metadata.name}={.spec.resources.gpu.quota} {end}' 2>/dev/null) after ${ms} ms" "$(project_in_cluster && echo PASS || echo FAIL)"
kc get projects.kai.resources,queues -o yaml >"$E/C4-cluster-objects.yaml" 2>&1 || true
NSP=$(kc get projects.kai.resources llm-train -o jsonpath='{.status.namespace}' 2>/dev/null || true)
NSP=${NSP:-$(kc get ns -l runai/queue=llm-train -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)}

job=$(sed -e "s|@NAME@|pretrain|g" -e "s|@PODS@|18|g" -e "s|@GPUS@|4|g" "$here/manifests/workloads/training-job.yaml.tmpl" |
	python3 -I -c 'import sys, json, yaml; print(json.dumps(yaml.safe_load(sys.stdin)))')
# The control plane accepts a workload only once its type is Ready in the cluster (the Karta chain
# above); until then it answers 400, and the submission is retried.
submitted() {
	api POST /api/v2/workloads "{\"metadata\": {\"name\": \"pretrain\", \"projectId\": \"$PROJECT_ID\"}, \"manifest\": $job}" >"$E/C5-workload.json"
	! jq -e '(type == "array" and (.[0].code // null) != null) or (type == "object" and (.code // null) != null)' "$E/C5-workload.json" >/dev/null
}
ms_submit=$(wait_until 180 submitted) || true
bound() { [ "$(kc get pods -n "${NSP:-none}" -l dc-twin.io/workload=pretrain -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | grep -c .)" = 18 ]; }
ms=$(wait_until 300 bound) || true
nodes=$(kc get pods -n "${NSP:-none}" -l dc-twin.io/workload=pretrain -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | sort -u)
cl=$(for n in $nodes; do kc get node "$n" -o 'jsonpath={.metadata.labels.nvidia\.com/gpu\.clique}'; echo; done | sort -u | grep -c . || true)
record C5 "Workload pretrain (a Job of 18 x 4 GPUs, one clique) submitted through POST /api/v2/workloads" "the agent applies it in the project's namespace; KAI binds 18 pods on GB300 trays in one clique" "namespace ${NSP:-none}: WorkloadRunner $(kc get workloadrunners -n "${NSP:-none}" -o name 2>/dev/null | tr '\n' ' ')Job $(kc get jobs -n "${NSP:-none}" -o name 2>/dev/null | tr '\n' ' ')$(echo "$nodes" | grep -c . || true) Nodes, $cl clique; accepted after ${ms_submit} ms, bound ${ms} ms later" "$(bound && [ "$cl" = 1 ] && echo PASS || echo FAIL)"
kc get pods -A -o wide >"$E/C5-pods.txt" 2>&1 || true

# The way back: external-workload-integrator records the Job as an ExternalWorkload in its
# project's namespace, and cluster-sync reports it and its pods to the control plane.
WID=$(jq -r '.metadata.id // empty' "$E/C5-workload.json" 2>/dev/null || true)
wl_phase() { api GET "/api/v1/workloads?filterBy=clusterId==$UUID" | jq -r '.workloads[]? | select(.name == "pretrain") | .phase'; }
pods_reported() { [ "$(api GET "/api/v1/workloads/$WID/pods" | jq '[.pods[]? | select((.nodeName // .node // "") != "")] | length')" = 18 ]; }
ms=$(wait_until 180 pods_reported) || true
api GET "/api/v1/workloads?filterBy=clusterId==$UUID" >"$E/C6-workloads.json" 2>&1 || true
api GET "/api/v1/workloads/$WID/pods" >"$E/C6-pods.json" 2>&1 || true
kc get externalworkloads -A -o yaml >"$E/C6-externalworkloads.yaml" 2>&1 || true
record C6 "Workload pretrain as the control plane sees it (external-workload-integrator, cluster-sync)" "its 18 pods reported, each with its Node" "ExternalWorkloads $(kc get externalworkloads -n "${NSP:-none}" -o name 2>/dev/null | tr '\n' ' ')phase $(wl_phase); $(jq '.pods | length' "$E/C6-pods.json" 2>/dev/null) pods ($(jq -r '[.pods[]?.k8sPhase] | group_by(.) | map("\(.[0]) \(length)") | join(", ")' "$E/C6-pods.json" 2>/dev/null)) on $(jq -r '[.pods[]? | (.nodeName // .node // empty)] | unique | length' "$E/C6-pods.json" 2>/dev/null) Nodes after ${ms} ms" "$(pods_reported && echo PASS || echo FAIL)"

# A restart of Run:ai's own services is a finding: in a cluster the pod would restart too, but it
# says the service exited.
i=0
for svc in runai-agent runai-cluster-sync runai-external-workload-integrator; do
	i=$((i + 1))
	[ -s "$WORK/runai/$svc.restarts" ] || continue
	why=$(grep -m1 -E '^panic:|"level":"fatal"|FATAL' "$WORK/runai/$svc.log" | cut -c1-160 || true)
	record "R$i" "$svc exits" "runs without exiting" "restarted $(wc -l <"$WORK/runai/$svc.restarts") times (first: ${why:-see its log})" FINDING
done
cp "$WORK"/runai/*.restarts "$E/" 2>/dev/null || true

status=0
print_table "Run:ai $RUNAI_REF control plane ($CP_URL) with its cluster side over dc-twin's Nodes (IMPERSONATED, D24)" || status=$?
exit $status

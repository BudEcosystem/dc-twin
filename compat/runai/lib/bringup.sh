# shellcheck shell=bash
# Run:ai's cluster side as processes over the twin's Nodes, sourced by run.sh and
# controlplane/cluster.sh after the binaries exist. It brings up envtest's kube-apiserver with the
# real kube-controller-manager, the twin, KAI's cluster-wide components, KRM's three services with
# their webhooks, the chart's default NodePool and Run:ai's anyworkload-controller, and defines
# the shard starter (the KAI operator's part) and the helpers the checks use.
#
# Needs: here (compat/runai), root, WORK (with src/krm, src/kai-scheduler and, for the workload
# controller, src/run-ai and KARTA_DIR), WORK/bin (krm-*, runai-*), KAI_DIR, KCM_BIN, ESTATE,
# RUNAI_WORKLOADS, ENVTEST_DIR and KUBECTL, and compat/kai/lib's common.sh, cluster.sh and twin.sh.
# ---------------------------------------------------------------- bring-up

PIDS="" SHARD_PIDS=""
cleanup() {
	set +e
	if [ "${KEEP:-}" = 1 ]; then
		echo "KEEP=1: left running. Stop with: kill $PIDS $SHARD_PIDS; HOME=$TWIN_HOME $DCTWIN down --state $TWIN_STATE; kill $CLUSTER_PIDS" >&2
		return
	fi
	local p
	for p in $PIDS $SHARD_PIDS; do kill "$p" 2>/dev/null; done
	for p in $PIDS $SHARD_PIDS; do wait "$p" 2>/dev/null; done
	twin_down
	cluster_down
}
trap cleanup EXIT
trap 'exit 130' INT TERM

rm -rf "$WORK/cluster" "$WORK/twin" "$WORK/runai" "$WORK/evidence"
mkdir -p "$WORK/runai" "$WORK/evidence"
cluster_up "$WORK/cluster" "$KCM_BIN"
echo "kube-apiserver $APISERVER_URL (envtest $(basename "$ENVTEST_DIR")); kube-controller-manager: $KCM_BIN" >&2
kc apply --server-side -f "$WORK/src/kai-scheduler/deployments/kai-scheduler/crds/" \
	-f "$WORK/src/krm/deployments/kai-resource-management-chart/crds/" >/dev/null
crds_established() {
	[ "$(kc get crd -o 'jsonpath={range .items[*]}{.status.conditions[?(@.type=="Established")].status}{"\n"}{end}' | grep -c True)" -ge 11 ]
}
wait_until 60 crds_established >/dev/null || { echo "CRDs not established" >&2; exit 1; }
kc apply -f "$here/manifests/00-namespaces.yaml" -f "$root/compat/kai/manifests/03-priorityclasses.yaml" \
	-f "$root/compat/kai/manifests/02-topology.yaml" >/dev/null

twin_up "$ESTATE" "$WORK/twin" "${ADAPTERS:-redfish,kwok,metrics}" "$KUBECONFIG_FILE"
echo "twin up: $(grep -o 'control API: [^ ]*' "$WORK/twin/up.log"), state $TWIN_STATE" >&2

start() { # start <log name> <command...>
	local name=$1
	shift
	KUBECONFIG="$KUBECONFIG_FILE" HOME="$WORK/twin/home" "$@" >"$WORK/runai/$name.log" 2>&1 &
	PIDS="$PIDS $!"
}
# Names: KAI's and KRM's own defaults, or, with RUNAI_PROFILE=1 (controlplane/cluster.sh), the ones a
# Run:ai cluster gives them: go-operator configures KRM and KAI with Run:ai's scheduler name and
# labels (go-operator operands/krm-operator/resources/krm_configs.go convertKRMGlobal,
# operands/kai-operator/resources/global.go), and the cluster chart's kai-resource-management values
# turn off KRM's node pool, project and department webhooks and KRM's own default NodePool (the agent
# syncs it from the control plane). Run:ai's components find a project's namespace, its pods and
# their scheduler by those names.
npc_args=() pc_args=() pga_args=() krm_values=()
if [ "${RUNAI_PROFILE:-0}" = 1 ]; then
	SCHED=runai-scheduler QUEUE_LABEL=runai/queue NODEPOOL_LABEL=runai/node-pool
	npc_args=(--finalizer-domain run.ai)
	pc_args=(--finalizer-domain run.ai --namespace-project-label-key runai/queue --project-label-key project
		--enforce-scheduler-annotation-key runai/enforce-scheduler-name)
	pga_args=(--namespace-project-label-key runai/queue --project-label-key project
		--enforce-scheduler-annotation-key runai/enforce-scheduler-name)
	krm_values=(--set commonArgs.finalizerDomain=run.ai --set commonArgs.namespaceProjectLabelKey=runai/queue
		--set commonArgs.projectLabelKey=project --set commonArgs.enforceSchedulerAnnotationKey=runai/enforce-scheduler-name
		--set commonArgs.schedulerName=runai-scheduler --set nodePoolController.webhook.nodepool=false
		--set projectController.webhook.project=false --set projectController.webhook.department=false
		--set defaultNodePool.enabled=false)
else
	SCHED=kai-scheduler QUEUE_LABEL=kai.scheduler/queue NODEPOOL_LABEL=kai.scheduler/node-pool
fi

# KAI's cluster-wide components. Its podgroupcontroller is not run: at v0.18.6 it ignores
# --metrics-bind-address and binds :8080 (see compat/grove/README.md); nothing here reads what it writes.
start kai-binder "$KAI_DIR/kai-binder" --scheduler-name "$SCHED" --resource-reservation-namespace kai-resource-reservation \
	--metrics-bind-address "127.0.0.1:$(free_port)" --health-probe-bind-address "127.0.0.1:$(free_port)"
start kai-podgrouper "$KAI_DIR/kai-podgrouper" --scheduler-name "$SCHED" --queue-label-key "$QUEUE_LABEL" \
	--nodepool-label-key "$NODEPOOL_LABEL" --metrics-bind-address "127.0.0.1:$(free_port)" --health-probe-bind-address "127.0.0.1:$(free_port)"
start kai-queuecontroller "$KAI_DIR/kai-queuecontroller" --enable-webhook=false --metrics-listen-address "127.0.0.1:$(free_port)"

# KRM: the three services its operator would deploy, with the arguments it would give them
# (pkg/operator/operands/*/resources.go at the chart's defaults), each serving its admission webhook
# on its own port with a certificate for 127.0.0.1 from a scratch CA. The chart's webhook
# configurations (helm template) are pointed at them by URL.
P=$WORK/runai/pki
mkdir -p "$P"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=dctwin-runai-ca -keyout "$P/ca.key" -out "$P/ca.crt" 2>/dev/null
printf 'subjectAltName=IP:127.0.0.1\n' >"$P/san"
openssl req -newkey rsa:2048 -nodes -subj /CN=127.0.0.1 -keyout "$P/tls.key" -out "$P/tls.csr" 2>/dev/null
openssl x509 -req -in "$P/tls.csr" -CA "$P/ca.crt" -CAkey "$P/ca.key" -CAcreateserial -days 2 -extfile "$P/san" -out "$P/tls.crt" 2>/dev/null
PGA_PORT=$(free_port) PC_PORT=$(free_port) NPC_PORT=$(free_port)
NS_KRM=kai-resource-management
start krm-nodepool-controller "$WORK/bin/krm-nodepool-controller" --metrics-port "$(free_port)" --scheduler-namespace kai-scheduler \
	--enable-nodepool-validation-webhook=true --default-nodepool-name default --webhook-port "$NPC_PORT" --cert-dir "$P" \
	--scheduler-name "$SCHED" --nodepool-label-key "$NODEPOOL_LABEL" ${npc_args[@]+"${npc_args[@]}"}
start krm-project-controller "$WORK/bin/krm-project-controller" --metrics-port "$(free_port)" --profiler-api-port "$(free_port)" \
	--rolebindings-configmap-namespace $NS_KRM --project-delete-blockers-namespace $NS_KRM --install-namespace $NS_KRM \
	--default-nodepool-name default --enable-project-validation-webhook=true --enable-department-validation-webhook=true \
	--webhook-port "$PC_PORT" --cert-dir "$P" --namespaces --role-bindings \
	--queue-label-key "$QUEUE_LABEL" --nodepool-label-key "$NODEPOOL_LABEL" ${pc_args[@]+"${pc_args[@]}"}
start krm-pod-group-assigner "$WORK/bin/krm-pod-group-assigner" --webhook-port "$PGA_PORT" --cert-dir "$P" \
	--enable-pod-webhook=true --default-nodepool-name default --profiler-api-port "$(free_port)" \
	--scheduler-name "$SCHED" --queue-label-key "$QUEUE_LABEL" --nodepool-label-key "$NODEPOOL_LABEL" ${pga_args[@]+"${pga_args[@]}"}
webhook_up() { curl -sk -o /dev/null "https://127.0.0.1:$1/"; }
for p in "$PGA_PORT" "$PC_PORT" "$NPC_PORT"; do
	wait_until 60 webhook_up "$p" >/dev/null || { echo "a KRM webhook server did not start on $p; see $WORK/runai/krm-*.log" >&2; exit 1; }
done
chart="$WORK/runai/krm-chart"
rm -rf "$chart" && cp -R "$WORK/src/krm/deployments/kai-resource-management-chart" "$chart"
# The bundled kai-scheduler subchart is not fetched: KAI runs as processes here, and only KRM's own
# templates are rendered.
awk '/^dependencies:/ {exit} {print}' "$WORK/src/krm/deployments/kai-resource-management-chart/Chart.yaml" >"$chart/Chart.yaml"
HOME="$WORK/twin/home" helm template krm "$chart" -n $NS_KRM --set openshift=false ${krm_values[@]+"${krm_values[@]}"} >"$WORK/runai/krm-rendered.yaml"
python3 -I "$root/compat/grove/lib/webhooks.py" "$WORK/runai/krm-rendered.yaml" "$WORK/runai/krm-webhooks.yaml" \
	"pod-group-assigner=$PGA_PORT,project-controller=$PC_PORT,nodepool-controller=$NPC_PORT" "$P/ca.crt"
kc apply -f "$WORK/runai/krm-webhooks.yaml" >/dev/null
python3 -I -c '
import re, sys
docs = open(sys.argv[1]).read().split("\n---\n")
open(sys.argv[2], "w").write("\n---\n".join(d for d in docs if re.search(r"^kind: NodePool\s*$", d, re.M)) + "\n")' \
	"$WORK/runai/krm-rendered.yaml" "$WORK/runai/default-nodepool.yaml"
if has '^kind: NodePool' "$WORK/runai/default-nodepool.yaml"; then kc apply -f "$WORK/runai/default-nodepool.yaml" >/dev/null; fi

# Run:ai's anyworkload-controller, configured by environment as its Deployment is, with the
# WorkloadRunner webhooks go-operator defines. It renders a WorkloadRunner's template through the
# Karta for its kind, which in a Run:ai cluster the agent syncs from the control plane's workload
# types (labelled run.ai/karta-group, -kind, -version) and the workload-map operator marks Ready.
# Neither runs here, so the harness applies the Karta for batch/v1 Job from the karta module the
# controller requires (docs/samples/batch-job.yaml), with those labels and a Ready condition.
# IMPERSONATED: the labels and the condition are the harness's.
if [ "$RUNAI_WORKLOADS" = build ]; then
	# The WorkloadRunner and RestartRequest CRDs as runai-chart ships them, and Karta's.
	kc apply --server-side -f "$WORK/src/run-ai/runai-cluster/cluster/runai-chart/charts/crds/crds/run.ai_workloadrunner.yaml" \
		-f "$WORK/src/run-ai/runai-cluster/cluster/runai-chart/charts/crds/crds/run.ai_restartrequest.yaml" -f "${KARTA_CRDS:-$KARTA_DIR/charts/karta/crds/}" >/dev/null
	karta_established() { kc get crd workloadrunners.run.ai kartas.run.ai restartrequests.run.ai -o 'jsonpath={range .items[*]}{.status.conditions[?(@.type=="Established")].status}{"\n"}{end}' | grep -c True | has 3; }
	wait_until 30 karta_established >/dev/null || true
	# controlplane/cluster.sh sets HARNESS_KARTA=0: there the agent syncs the Kartas and the
	# workload-map operator and external-workload-integrator mark them.
	if [ "${HARNESS_KARTA:-1}" = 1 ]; then
		kc apply -f "$KARTA_DIR/docs/samples/batch-job.yaml" >/dev/null
		kc label karta batch-v1-job run.ai/karta-group=batch run.ai/karta-kind=Job run.ai/karta-version=v1 --overwrite >/dev/null
		kc patch karta batch-v1-job --subresource=status --type merge -p "{\"status\":{\"conditions\":[{\"type\":\"Ready\",\"status\":\"True\",\"reason\":\"Validated\",\"message\":\"set by compat/runai\",\"lastTransitionTime\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}]}}" >/dev/null
	fi
	AWC_PORT=$(free_port)
	start runai-anyworkload-controller env METRICS_PORT="$(free_port)" WEBHOOK_PORT="$AWC_PORT" WEBHOOK_CERT_DIR="$P" \
		"$WORK/bin/runai-anyworkload-controller"
	wait_until 60 webhook_up "$AWC_PORT" >/dev/null || { echo "anyworkload-controller's webhook did not start; see $WORK/runai/runai-anyworkload-controller.log" >&2; exit 1; }
	sed -e "s|@URL@|https://127.0.0.1:$AWC_PORT|g" -e "s|@CA@|$(base64 <"$P/ca.crt" | tr -d '\n')|g" "$here/manifests/04-anyworkload-webhooks.yaml.tmpl" >"$WORK/runai/anyworkload-webhooks.yaml"
	kc apply -f "$WORK/runai/anyworkload-webhooks.yaml" >/dev/null
fi

# The KAI operator turns each SchedulingShard KRM creates into a scheduler Deployment and reports it
# Deployed and Available. No Deployment can run here, so the harness does the operator's part: it
# starts one kai-scheduler process per shard with the arguments the operator builds
# (pkg/operator/operands/scheduler/resources_for_shard.go buildArgsList) and then writes those two
# conditions, which KRM reads before it calls a node pool Ready. IMPERSONATED: the conditions are
# the harness's, not the KAI operator's.
started_shards=""
start_shard() { # start_shard <SchedulingShard name>
	local s=$1 part args a
	part=$(kc get schedulingshard "$s" -o jsonpath='{.spec.partitionLabelValue}')
	args="--scheduler-name=$SCHED --namespace=kai-scheduler --nodepool-label-key=$NODEPOOL_LABEL --partition-label-value=$part --queue-label-key=$QUEUE_LABEL"
	for a in $(kc get schedulingshard "$s" -o go-template='{{range $k, $v := .spec.args}}{{$k}}={{$v}} {{end}}'); do
		"$KAI_DIR/kai-scheduler" --help 2>&1 | has -- "--${a%%=*} " && args="$args --$a"
	done
	# shellcheck disable=SC2086
	KUBECONFIG="$KUBECONFIG_FILE" HOME="$WORK/twin/home" "$KAI_DIR/kai-scheduler" $args \
		--listen-address "127.0.0.1:$(free_port)" --plugin-server-port "$(free_port)" >"$WORK/runai/kai-scheduler-$s.log" 2>&1 &
	SHARD_PIDS="$SHARD_PIDS $!"
	echo "$s $args" >>"$WORK/runai/shards.txt"
	local now
	now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	kc patch schedulingshard "$s" --subresource=status --type merge -p "{\"status\":{\"conditions\":[
		{\"type\":\"Deployed\",\"status\":\"True\",\"reason\":\"Deployed\",\"message\":\"scheduler process started by compat/runai\",\"lastTransitionTime\":\"$now\"},
		{\"type\":\"Available\",\"status\":\"True\",\"reason\":\"Available\",\"message\":\"scheduler process started by compat/runai\",\"lastTransitionTime\":\"$now\"}]}}" >/dev/null
	started_shards="$started_shards $s"
}
sync_shards() {
	local s
	for s in $(kc get schedulingshards -o jsonpath='{.items[*].metadata.name}'); do
		case " $started_shards " in *" $s "*) ;; *) start_shard "$s" ;; esac
	done
}

# ---------------------------------------------------------------- helpers

ready_of() { kc get node "$1" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].reason}'; }
cond_of() { kc get node "$1" -o "jsonpath={.status.conditions[?(@.type==\"$2\")].status}"; }
alloc_of() { kc get node "$1" -o 'jsonpath={.status.allocatable.nvidia\.com/gpu}'; }
label_of() { kc get node "$1" -o "jsonpath={.metadata.labels.$2}"; }
cordoned() { [ "$(kc get node "$1" -o jsonpath='{.spec.unschedulable}')" = true ]; }
pool_of() { label_of "$1" 'kai\.scheduler/node-pool'; }
np_phase() { kc get nodepool "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
pods_of() { # <namespace> <workload>: "pod node" lines
	kc get pods -n "$1" -l "dc-twin.io/workload=$2" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.nodeName}{"\n"}{end}'
}
bound_count() { pods_of "$1" "$2" | awk 'NF == 2' | grep -c . || true; }
pod_count() { pods_of "$1" "$2" | grep -c . || true; }
has_bound() { [ "$(bound_count "$1" "$2")" = "$3" ]; }
nodes_of() { pods_of "$1" "$2" | awk 'NF == 2 {print $2}'; }
labels_over() { local n; for n in $(nodes_of "$1" "$2" | sort -u); do label_of "$n" "$3"; echo; done | sort -u | grep . || true; }
pg_of_pod() { kc get pod -n "$1" "$2" -o 'jsonpath={.metadata.annotations.pod-group-name}'; }
first_pod() { pods_of "$1" "$2" | awk 'NF == 2 {print $1; exit}'; }
yes_no() { if [ "$1" -eq 0 ]; then echo PASS; else echo FAIL; fi; }
ok() { "$@" >/dev/null 2>&1 && echo 0 || echo 1; }
render_job() { sed -e "s|@NAME@|$1|g" -e "s|@PODS@|$2|g" -e "s|@GPUS@|$3|g" "$here/manifests/workloads/training-job.yaml.tmpl"; }
render_deploy() { # name replicas gpus annotations affinity
	sed -e "s|@NAME@|$1|g" -e "s|@REPLICAS@|$2|g" -e "s|@GPUS@|$3|g" -e "s|@ANNOTATIONS@|$4|g" -e "s|@AFFINITY@|$5|g" \
		"$here/manifests/workloads/inference-deployment.yaml.tmpl"
}

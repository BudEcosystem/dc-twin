#!/usr/bin/env bash
# Runs the cluster side of NVIDIA Run:ai, as its release pins it, against the twin's Kubernetes view
# of the lab estate: Run:ai 3.0.20's scheduling and resource-management layer, KAI Resource
# Management (KRM: nodepool-controller, project-controller, pod-group-assigner, built from the tag
# Run:ai pins), with KAI Scheduler, one scheduler per node pool, on a real kube-apiserver and etcd
# (envtest's binaries) and the real kube-controller-manager, over dc-twin's kwok projection standing
# in for 50 kubelets (IMPERSONATED, D24). Node pools are cut by GPU product, a department and two
# projects carry quota, and training and inference workloads are placed by product and NVLink
# clique; the twin then faults and powers off hosts and the placement reacts. No pod runs.
#
#   compat/runai/run.sh                       # PASS/FAIL table on stdout, artefacts under $WORK
#   WORK=/tmp/runai DCTWIN=bin/dctwin compat/runai/run.sh
#   KAI_BIN=<dir with kai-scheduler, kai-binder, ...> compat/runai/run.sh   # reuse compat/kai's build
#   KCM=/path/kube-controller-manager compat/runai/run.sh
#   RUNAI_WORKLOADS=none compat/runai/run.sh  # without Run:ai's workload controller
#   KEEP=1 compat/runai/run.sh                # leave everything running; prints how to stop it
#
# Nothing here reads ~/.kube/config: every client is given the scratch cluster's kubeconfig.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
kailib="$root/compat/kai/lib"
RUNAI_REF=${RUNAI_REF:-3.0.20}            # github.com/dsx-ai-factory/run-ai (release tag)
KRM_REF=${KRM_REF:-v0.18.3}               # github.com/kai-scheduler/kai-resource-management, Run:ai 3.0.20's pin
KAI_REF=${KAI_REF:-v0.18.6}               # KAI Scheduler; KRM v0.18.3 bundles v0.18.3 (see README)
K8S_REF=${K8S_REF:-v1.37.0}
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
KCM=${KCM:-build}                         # build | <path to kube-controller-manager> (the Job and Deployment controllers are needed)
KAI_BIN=${KAI_BIN:-}
RUNAI_WORKLOADS=${RUNAI_WORKLOADS:-build}   # build | none: Run:ai's anyworkload-controller (WorkloadRunner)
ESTATE=${ESTATE:-$root/examples/estates/lab-model.yaml}   # the checks name its racks and hosts
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
command -v helm >/dev/null || { echo "helm not found: KRM's webhook configurations are rendered with helm template" >&2; exit 1; }
[ "$KCM" != none ] || { echo "KCM=none is not supported: the Job and Deployment controllers create the workloads' pods" >&2; exit 1; }

# ---------------------------------------------------------------- build from source

clone() { # clone <url> <ref> <dir>
	[ -d "$3/.git" ] || git clone -q --depth 1 --branch "$2" "$1" "$3" 2>/dev/null
}

# Run:ai's release names the KRM it installs (runai-cluster/kai-scheduler/kai-version.yaml); read it
# from the tag rather than trusting KRM_REF's default.
if [ ! -f "$WORK/src/runai-kai-version.yaml" ]; then
	curl -fsSL "https://raw.githubusercontent.com/dsx-ai-factory/run-ai/$RUNAI_REF/runai-cluster/kai-scheduler/kai-version.yaml" \
		-o "$WORK/src/runai-kai-version.yaml" || true
fi
pinned_krm=$(awk '/^krm:/ {k=1} k && /chart-version:/ {print $2; exit}' "$WORK/src/runai-kai-version.yaml" 2>/dev/null || true)
pinned_kai=$(awk '/^kai:/ {k=1} k && /human-readable-version:/ {print $2; exit}' "$WORK/src/runai-kai-version.yaml" 2>/dev/null || true)
[ -z "$pinned_krm" ] || [ "$pinned_krm" = "$KRM_REF" ] || echo "note: Run:ai $RUNAI_REF pins KRM $pinned_krm; running KRM $KRM_REF" >&2

clone https://github.com/kai-scheduler/kai-resource-management.git "$KRM_REF" "$WORK/src/krm"
KRM_COMMIT=$(git -C "$WORK/src/krm" rev-parse HEAD)
for c in nodepool-controller project-controller pod-group-assigner; do
	if [ ! -x "$WORK/bin/krm-$c" ]; then
		echo "building KRM $c at $KRM_REF" >&2
		(cd "$WORK/src/krm" && go build -ldflags='-s -w' -o "$WORK/bin/krm-$c" "./cmd/$c")
	fi
done

# Run:ai's own workload controller, anyworkload-controller (the dromos library with Run:ai's plugins),
# from the release tag: a sparse, blob-filtered checkout of the monorepo's cluster modules.
AWC=runai-cluster/cluster/anyworkload-controller
if [ "$RUNAI_WORKLOADS" = build ]; then
	if [ ! -d "$WORK/src/run-ai/.git" ]; then
		git clone -q --depth 1 --branch "$RUNAI_REF" --filter=blob:none --sparse https://github.com/dsx-ai-factory/run-ai.git "$WORK/src/run-ai" 2>/dev/null
		git -C "$WORK/src/run-ai" sparse-checkout set $AWC runai-cluster/cluster/dromos runai-cluster/cluster/common runai-cluster/cluster/sdk \
			runai-cluster/cluster/workload-common runai-cluster/common runai-common-packages control-plane/cluster-service/pkg/client \
			runai-cluster/cluster/runai-chart/charts/crds
	fi
	RUNAI_COMMIT=$(git -C "$WORK/src/run-ai" rev-parse HEAD)
	if [ ! -x "$WORK/bin/runai-anyworkload-controller" ]; then
		echo "building Run:ai anyworkload-controller at $RUNAI_REF" >&2
		(cd "$WORK/src/run-ai/$AWC" && go build -ldflags='-s -w' -o "$WORK/bin/runai-anyworkload-controller" ./cmd)
	fi
	# Karta (the workload map the controller renders templates through), at the version it requires.
	karta_ver=$(awk '$1 == "github.com/run-ai/karta" {print $2; exit}' "$WORK/src/run-ai/$AWC/go.mod")
	KARTA_DIR=$(cd "$WORK/src/run-ai/$AWC" && go mod download -json "github.com/run-ai/karta@$karta_ver" | awk -F'"' '$2 == "Dir" {print $4}')
fi

KAI_COMPONENTS="scheduler binder podgrouper queuecontroller"
clone https://github.com/kai-scheduler/KAI-Scheduler.git "$KAI_REF" "$WORK/src/kai-scheduler"
KAI_COMMIT=$(git -C "$WORK/src/kai-scheduler" rev-parse HEAD)
KAI_DIR=${KAI_BIN:-$WORK/bin}
for c in $KAI_COMPONENTS; do
	if [ ! -x "$KAI_DIR/kai-$c" ]; then
		[ -z "$KAI_BIN" ] || { echo "KAI_BIN=$KAI_BIN has no kai-$c" >&2; exit 1; }
		echo "building KAI $c at $KAI_REF" >&2
		(cd "$WORK/src/kai-scheduler" && go build -o "$KAI_DIR/kai-$c" "./cmd/$c")
	fi
done

case "$KCM" in
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

. "$here/lib/bringup.sh"

# ---------------------------------------------------------------- U: the twin's Nodes and the components

hosts=50
node_count_is() { [ "$(kc get nodes -o name | grep -c .)" -eq "$1" ]; }
ms=$(wait_until 60 node_count_is $hosts) || true
ready=$(kc get nodes -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c True || true)
imp=$(kc get nodes -o jsonpath='{range .items[*]}{.metadata.annotations.dc-twin\.io/impersonated}{"\n"}{end}' | grep -c kwok || true)
record U1 "Nodes the twin registers (kwok projection)" "$hosts Ready, all dc-twin.io/impersonated=kwok" "$ready Ready, $imp impersonated, after ${ms} ms" "$( [ "$ready" = $hosts ] && [ "$imp" = $hosts ] && echo PASS || echo FAIL)"
no_notready_taints() { ! kc get nodes -o jsonpath='{range .items[*]}{.spec.taints[*].key}{"\n"}{end}' | has 'node.kubernetes.io/not-ready'; }
wait_until 60 no_notready_taints >/dev/null || true
alive=0 want=0
for p in $PIDS; do want=$((want + 1)); kill -0 "$p" 2>/dev/null && alive=$((alive + 1)); done
record U2 "KRM $KRM_REF ($KRM_COMMIT), KAI $KAI_REF ($KAI_COMMIT)$( [ "$RUNAI_WORKLOADS" = build ] && echo " and Run:ai $RUNAI_REF anyworkload-controller ($RUNAI_COMMIT)") running" "$want of $want (KRM nodepool-controller, project-controller, pod-group-assigner with their webhooks; KAI binder, podgrouper, queuecontroller$( [ "$RUNAI_WORKLOADS" = build ] && echo "; anyworkload-controller with its webhooks"))" "$alive of $want; Run:ai $RUNAI_REF pins KRM ${pinned_krm:-?} and KAI ${pinned_kai:-?}" "$( [ $alive = $want ] && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- N: node pools by product

kc apply -f "$here/manifests/01-nodepools.yaml" >/dev/null
shards_exist() { [ "$(kc get schedulingshards -o name | grep -c .)" -ge 4 ]; }
wait_until 60 shards_exist >/dev/null || true
sync_shards
pools_ready() { local p; for p in gb300 gb200 h100 default; do [ "$(np_phase "$p")" = Ready ] || return 1; done; }
ms=$(wait_until 60 pools_ready) || true
count_pool() { kc get nodes -l "kai.scheduler/node-pool=$1" -o name | grep -c . || true; }
defcount=$(kc get nodes -l '!kai.scheduler/node-pool' -o name | grep -c . || true)
record N1 "NodePools gb300, gb200, h100 by nvidia.com/gpu.product, and default" "all Ready; Nodes labelled kai.scheduler/node-pool: gb300 18, gb200 18, h100 4; the other 10 unlabelled (default)" "phases $(for p in gb300 gb200 h100 default; do printf '%s=%s ' "$p" "$(np_phase "$p")"; done)after ${ms} ms; labelled gb300 $(count_pool gb300), gb200 $(count_pool gb200), h100 $(count_pool h100), default $defcount" "$( pools_ready && [ "$(count_pool gb300)" = 18 ] && [ "$(count_pool gb200)" = 18 ] && [ "$(count_pool h100)" = 4 ] && [ "$defcount" = 10 ] && echo PASS || echo FAIL)"
record N2 "A KAI scheduler shard per node pool" "4 SchedulingShards from KRM, each scheduling only its pool's Nodes (partition label value)" "$(kc get schedulingshards -o jsonpath='{range .items[*]}{.metadata.name}={.spec.partitionLabelValue} {end}'); $(echo "$SHARD_PIDS" | wc -w | tr -d ' ') kai-scheduler processes started by the harness (IMPERSONATED: the KAI operator's Deployments and status)" "$( [ "$(kc get schedulingshards -o name | grep -c .)" = 4 ] && echo PASS || echo FAIL)"
kc get nodepools -o yaml >"$WORK/evidence/N1-nodepools.yaml"
kc get schedulingshards -o yaml >"$WORK/evidence/N2-schedulingshards.yaml"
kc get nodes -L kai.scheduler/node-pool,nvidia.com/gpu.product,nvidia.com/gpu.clique >"$WORK/evidence/N1-nodes.txt"

# ---------------------------------------------------------------- P: department, projects, queues

kc apply -f "$here/manifests/02-department.yaml" >/dev/null 2>"$WORK/evidence/P-apply.err" || true
kc apply -f "$here/manifests/03-projects.yaml" >/dev/null 2>>"$WORK/evidence/P-apply.err" || true
queues_made() {
	local q
	for q in ai ai-gb300 ai-gb200 ai-h100 llm-train llm-train-gb300 llm-train-gb200 serving serving-h100 serving-gb200; do
		kc get queue "$q" >/dev/null 2>&1 || return 1
	done
}
ms=$(wait_until 60 queues_made) || true
qp() { kc get queue "$1" -o 'jsonpath={.spec.parentQueue}' 2>/dev/null; }
ql() { kc get queue "$1" -o 'jsonpath={.spec.resources.gpu.quota}/{.spec.resources.gpu.limit}' 2>/dev/null; }
record P1 "Department ai and Projects llm-train, serving" "KAI Queues per (owner, node pool), project queues parented to the department's queue of the same pool, GPU quota/limit as written" "after ${ms} ms: llm-train-gb300 → $(qp llm-train-gb300) $(ql llm-train-gb300); llm-train-gb200 → $(qp llm-train-gb200) $(ql llm-train-gb200); serving-h100 → $(qp serving-h100) $(ql serving-h100); serving-gb200 → $(qp serving-gb200) $(ql serving-gb200)" "$( queues_made && [ "$(qp llm-train-gb300)" = ai-gb300 ] && [ "$(ql llm-train-gb300)" = 72/72 ] && [ "$(qp serving-h100)" = ai-h100 ] && [ "$(ql serving-h100)" = 8/16 ] && echo PASS || echo FAIL)"
ns_made() { kc get ns kai-llm-train kai-serving -o name >/dev/null 2>&1; }
wait_until 30 ns_made >/dev/null || true
record P2 "Project namespaces" "kai-llm-train and kai-serving, labelled with their project" "$(kc get ns kai-llm-train kai-serving -o 'jsonpath={range .items[*]}{.metadata.name}({.metadata.labels.kai/project}) {end}' 2>/dev/null)" "$( [ "$(kc get ns kai-llm-train -o 'jsonpath={.metadata.labels.kai/project}' 2>/dev/null)" = llm-train ] && [ "$(kc get ns kai-serving -o 'jsonpath={.metadata.labels.kai/project}' 2>/dev/null)" = serving ] && echo PASS || echo FAIL)"
kc get queues -o yaml >"$WORK/evidence/P1-queues.yaml"
kc get projects,departments -o yaml >"$WORK/evidence/P1-projects.yaml"

# ---------------------------------------------------------------- W: workloads placed by product, clique and quota

TR=kai-llm-train SV=kai-serving
render_job pretrain 18 4 | tee "$WORK/evidence/W1-job.yaml" | kc apply -f - >/dev/null
ms=$(wait_until 90 has_bound $TR pretrain 18) || true
p1=$(first_pod $TR pretrain)
pg=$(pg_of_pod $TR "${p1:-none}" 2>/dev/null || true)
pgq=$(kc get podgroup -n $TR "${pg:-none}" -o jsonpath='{.spec.queue}' 2>/dev/null)
pgnp=$(kc get podgroup -n $TR "${pg:-none}" -o 'jsonpath={.metadata.labels.kai\.scheduler/node-pool}' 2>/dev/null)
products=$(labels_over $TR pretrain 'nvidia\.com/gpu\.product' | tr '\n' ' ')
cl=$(labels_over $TR pretrain 'nvidia\.com/gpu\.clique' | grep -c . || true)
record W1 "Training Job pretrain in kai-llm-train: 18 pods x 4 GPUs, one gang, required in one nvidia.com/gpu.clique; no scheduler, queue or pool named" "18 of 18 bound on GB300 trays in 1 clique; PodGroup assigned node pool gb300, queue llm-train-gb300" "$(bound_count $TR pretrain)/18 bound after ${ms} ms on [${products% }], $cl clique; PodGroup $pg: node pool $pgnp, queue $pgq" "$( [ "$(bound_count $TR pretrain)" = 18 ] && [ "${products% }" = NVIDIA-GB300 ] && [ "$cl" = 1 ] && [ "$pgq" = llm-train-gb300 ] && [ "$pgnp" = gb300 ] && echo PASS || echo FAIL)"
sn=$(kc get pod -n $TR "${p1:-none}" -o 'jsonpath={.spec.schedulerName}' 2>/dev/null)
plabel=$(kc get pod -n $TR "${p1:-none}" -o 'jsonpath={.metadata.labels.project}' 2>/dev/null)
aff=$(kc get pod -n $TR "${p1:-none}" -o 'jsonpath={range .spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[*]}{range .matchExpressions[*]}{.key} {.operator} {.values} | {end}{end}' 2>/dev/null | sed 's/ | $//')
record W2 "KRM pod admission on a pretrain pod" "schedulerName kai-scheduler, label project=llm-train, required node affinity on the label pairs of the project's default pools (gb300, gb200)" "schedulerName $sn, project=$plabel, affinity [$aff]" "$( [ "$sn" = kai-scheduler ] && [ "$plabel" = llm-train ] && echo "$aff" | has NVIDIA-GB300 && echo "$aff" | has NVIDIA-GB200 && echo PASS || echo FAIL)"
kc get pods -n $TR -o wide >"$WORK/evidence/W1-pods.txt"
kc get podgroup -n $TR "${pg:-none}" -o yaml >"$WORK/evidence/W1-podgroup.yaml" 2>&1 || true

render_deploy llm-serve 3 8 "" "{}" | tee "$WORK/evidence/W3-deployment.yaml" | kc apply -f - >/dev/null
created3() { [ "$(pod_count $SV llm-serve)" = 3 ]; }
wait_until 60 created3 >/dev/null || true
ms=$(wait_until 60 has_bound $SV llm-serve 1) || true
pend_reason() { local q; q=$(pods_of $SV llm-serve | awk 'NF == 1 {print $1; exit}'); kc get pod -n $SV "${q:-none}" -o 'jsonpath={.status.conditions[?(@.type=="PodScheduled")].message}' 2>/dev/null | has .; }
wait_until 60 pend_reason >/dev/null || true
products=$(labels_over $SV llm-serve 'nvidia\.com/gpu\.product' | tr '\n' ' ')
pend=$(pods_of $SV llm-serve | awk 'NF == 1 {print $1; exit}')
why=$(kc get pod -n $SV "${pend:-none}" -o 'jsonpath={.status.conditions[?(@.type=="PodScheduled")].message}' 2>/dev/null | tr '\n' ' ' | cut -c1-190)
record W3 "Inference Deployment llm-serve in kai-serving: 3 replicas x 8 GPUs; queue serving-h100 deserved 8, limit 16; the h100 pool has 32 free" "1 bound on an H100 server; 2 Pending: inference is non-preemptible, and KAI holds non-preemptible work to the queue's deserved quota" "$(bound_count $SV llm-serve)/3 bound after ${ms} ms on [${products% }]; ${pend:-none}: $why" "$( [ "$(bound_count $SV llm-serve)" = 1 ] && [ "${products% }" = NVIDIA-H100-80GB-HBM3 ] && echo "$why" | has NonPreemptibleOverQuota && echo PASS || echo FAIL)"
kc get pods -n $SV -o wide >"$WORK/evidence/W3-pods.txt"
kc get queue serving-h100 -o yaml >"$WORK/evidence/W3-queue.yaml"

# W4: a Run:ai workload as a WorkloadRunner, while pretrain holds every GB300 tray: the project's
# first pool is full, so the gang should be moved to its second, gb200, and packed in one clique.
if [ "$RUNAI_WORKLOADS" = build ]; then
	sed -e "s|@NAME@|finetune|g" -e "s|@PODS@|4|g" -e "s|@GPUS@|4|g" "$here/manifests/workloads/workloadrunner-job.yaml.tmpl" \
		| tee "$WORK/evidence/W4-workloadrunner.yaml" | kc apply -f - >"$WORK/evidence/W4-apply.txt" 2>&1 || true
	job_made() { [ "$(kc get job -n $TR finetune -o 'jsonpath={.metadata.ownerReferences[0].kind}' 2>/dev/null)" = WorkloadRunner ]; }
	ms0=$(wait_until 60 job_made) || true
	ms=$(wait_until 120 has_bound $TR finetune 4) || true
	p5=$(first_pod $TR finetune)
	pg5=$(pg_of_pod $TR "${p5:-none}" 2>/dev/null || true)
	record W4 "WorkloadRunner finetune (a Job of 4 pods x 4 GPUs, one clique) in kai-llm-train while pretrain holds every GB300 tray" "anyworkload-controller creates the Job (owned by the WorkloadRunner); gb300 is full, so the gang is moved to gb200: queue llm-train-gb200, 4 GB200 trays in 1 clique" "Job $(job_made && echo "owned by WorkloadRunner after ${ms0} ms" || echo "not created"); $(bound_count $TR finetune)/4 bound after ${ms} ms on [$(labels_over $TR finetune 'nvidia\.com/gpu\.product' | tr '\n' ' ' | sed 's/ $//')], $(labels_over $TR finetune 'nvidia\.com/gpu\.clique' | grep -c . || true) clique; PodGroup node pool $(kc get podgroup -n $TR "${pg5:-none}" -o 'jsonpath={.metadata.labels.kai\.scheduler/node-pool}' 2>/dev/null), queue $(kc get podgroup -n $TR "${pg5:-none}" -o jsonpath='{.spec.queue}' 2>/dev/null); runner phase $(kc get workloadrunner -n $TR finetune -o jsonpath='{.status.phase}' 2>/dev/null)" "$( job_made && [ "$(bound_count $TR finetune)" = 4 ] && [ "$(labels_over $TR finetune 'nvidia\.com/gpu\.product')" = NVIDIA-GB200 ] && [ "$(labels_over $TR finetune 'nvidia\.com/gpu\.clique' | grep -c .)" = 1 ] && [ "$(kc get podgroup -n $TR "${pg5:-none}" -o jsonpath='{.spec.queue}' 2>/dev/null)" = llm-train-gb200 ] && echo PASS || echo FAIL)"
	kc get workloadrunner -n $TR finetune -o yaml >"$WORK/evidence/W4-workloadrunner-status.yaml" 2>&1 || true
	kc get pods -n $TR -l dc-twin.io/workload=finetune -o wide >"$WORK/evidence/W4-pods.txt" 2>&1 || true
fi

# ---------------------------------------------------------------- F: the twin's faults and power

# F1: an Xid 79 under a pretrain pod; its drain (what NVSentinel or an operator would do) leaves the
# gang one tray short in its clique, and the Job controller's replacement waits.
XPOD=$(first_pod $TR pretrain)
XNODE=$(kc get pod -n $TR "$XPOD" -o jsonpath='{.spec.nodeName}')
twin fault inject "${XNODE%%-*}/${XNODE#*-}/gpu-2" nvidia/xid/xid-79 >"$WORK/evidence/F1-inject.json"
xid_settled() { [ "$(ready_of "$XNODE" | cut -d/ -f1)" = True ] && [ "$(cond_of "$XNODE" GpuHealthy)" = False ] && cordoned "$XNODE" && [ "$(alloc_of "$XNODE")" = 3 ]; }
ms=$(wait_until 20 xid_settled) || true
kc delete pod -n $TR "$XPOD" --force --grace-period=0 >/dev/null 2>&1
recreated() { [ "$(pod_count $TR pretrain)" = 18 ] && ! pods_of $TR pretrain | has "^$XPOD "; }
wait_until 30 recreated >/dev/null || true
sleep 15
newp=$(pods_of $TR pretrain | awk 'NF == 1 {print $1; exit}')
record F1 "dctwin fault inject $XNODE gpu-2 nvidia/xid/xid-79 under pretrain pod $XPOD, which is then drained" "$XNODE cordoned with allocatable 3; the Job controller's replacement stays Pending: the gang's clique has no other free GB300 tray" "fault settled after ${ms} ms; replacement ${newp:-none} unbound after 15 s; $(bound_count $TR pretrain)/18 bound" "$( xid_settled && [ -n "$newp" ] && echo PASS || echo FAIL)"
twin fault clear "${XNODE%%-*}/${XNODE#*-}/gpu-2" nvidia/xid/xid-79 >/dev/null
ms=$(wait_until 60 has_bound $TR pretrain 18) || true
record F2 "dctwin fault clear on $XNODE" "the replacement binds there; 18 of 18 again, 1 clique" "$(kc get pod -n $TR "${newp:-none}" -o jsonpath='{.spec.nodeName}' 2>/dev/null) after ${ms} ms; $(bound_count $TR pretrain)/18 bound, $(labels_over $TR pretrain 'nvidia\.com/gpu\.clique' | grep -c . || true) clique" "$( [ "$(bound_count $TR pretrain)" = 18 ] && [ "$(kc get pod -n $TR "${newp:-none}" -o jsonpath='{.spec.nodeName}' 2>/dev/null)" = "$XNODE" ] && echo PASS || echo FAIL)"

# F3-F5: every H100 server powered off. KRM documents a node pool with nodes but none ready as
# Unschedulable; a service that accepts h100 or gb200 should end on gb200, charged to gb200's queue.
H100="b01/server-01 b01/server-02 b02/server-01 b02/server-02"
H100N="b01-server-01 b01-server-02 b02-server-01 b02-server-02"
for h in $H100; do twin power off "$h" >/dev/null; done
all_down() { local n; for n in $H100N; do [ "$(ready_of "$n" | cut -d/ -f1)" != True ] || return 1; done; }
ms=$(wait_until 60 all_down) || true
record F3 "dctwin power off the four H100 servers ($H100)" "their Nodes not Ready" "$(for n in $H100N; do printf '%s=%s ' "$n" "$(ready_of "$n")"; done)after ${ms} ms" "$(yes_no "$(ok all_down)")"
pool_unsched() { [ "$(np_phase h100)" = Unschedulable ]; }
ms=$(wait_until 30 pool_unsched) || true
record F4 "NodePool h100 phase with none of its Nodes Ready" "Unschedulable (KRM docs/concepts/node-pools.md)" "phase $(np_phase h100) after ${ms} ms; status lists $(kc get nodepool h100 -o 'jsonpath={range .status.nodes[*]}{.name}={.status} {end}'): KRM's node watch passes only changes of status.phase (which kubelets never set), spec.unschedulable and labels, so a Ready condition going False never re-evaluates the pool" "$(pool_unsched && echo PASS || echo FINDING)"
# Node affinity on the pools' own label pairs (the form KRM's admission recognises as a node-pool
# preference) and the kai.scheduler/node-pools annotation giving the order.
aff='{ nodeAffinity: { requiredDuringSchedulingIgnoredDuringExecution: { nodeSelectorTerms: [ { matchExpressions: [ { key: nvidia.com/gpu.product, operator: In, values: [NVIDIA-H100-80GB-HBM3, NVIDIA-GB200] } ] } ] } } }'
render_deploy llm-serve-b 2 4 '"kai.scheduler/node-pools": "h100 gb200"' "$aff" | tee "$WORK/evidence/F5-deployment.yaml" | kc apply -f - >/dev/null
ms=$(wait_until 180 has_bound $SV llm-serve-b 2) || true
p4=$(first_pod $SV llm-serve-b)
pg4=$(pg_of_pod $SV "${p4:-none}" 2>/dev/null || true)
moves=$(grep -c "Successfully assigned pod group <$SV/${pg4:-none}>" "$WORK/runai/krm-pod-group-assigner.log" 2>/dev/null || true)
record F5 "Deployment llm-serve-b (2 x 4 GPUs) asking for node pools \"h100 gb200\" with every H100 server off" "pod-group-assigner tries h100, the h100 shard cannot place it, it moves to gb200: queue serving-gb200, bound on GB200 trays" "$(bound_count $SV llm-serve-b)/2 bound after ${ms} ms on [$(labels_over $SV llm-serve-b 'nvidia\.com/gpu\.product' | tr '\n' ' ' | sed 's/ $//')]; PodGroup node pool $(kc get podgroup -n $SV "${pg4:-none}" -o 'jsonpath={.metadata.labels.kai\.scheduler/node-pool}' 2>/dev/null), queue $(kc get podgroup -n $SV "${pg4:-none}" -o jsonpath='{.spec.queue}' 2>/dev/null) after $moves assignments" "$( [ "$(bound_count $SV llm-serve-b)" = 2 ] && [ "$(labels_over $SV llm-serve-b 'nvidia\.com/gpu\.product')" = NVIDIA-GB200 ] && [ "$(kc get podgroup -n $SV "${pg4:-none}" -o jsonpath='{.spec.queue}' 2>/dev/null)" = serving-gb200 ] && echo PASS || echo FAIL)"
kc get pods -n $SV -o wide >"$WORK/evidence/F5-pods.txt"
grep "$SV/${pg4:-none}" "$WORK/runai/krm-pod-group-assigner.log" | grep 'Successfully assigned' >"$WORK/evidence/F5-assignments.log" 2>/dev/null || true

for h in $H100; do twin power on "$h" >/dev/null; done
all_up() { local n; for n in $H100N; do [ "$(ready_of "$n")" = True/KubeletReady ] || return 1; done; }
ms=$(wait_until 60 all_up) || true
record F6 "dctwin power on the four H100 servers" "Ready again" "$(for n in $H100N; do printf '%s=%s ' "$n" "$(ready_of "$n")"; done)after ${ms} ms; NodePool h100 $(np_phase h100)" "$(yes_no "$(ok all_up)")"
record F7 "Workloads running, Run:ai workload status" "not reached: no kubelet runs a pod, and Run:ai's control plane and status-updater are not run" "pods bound and Pending; Job pretrain active $(kc get job -n $TR pretrain -o jsonpath='{.status.active}'), ready $(kc get job -n $TR pretrain -o jsonpath='{.status.ready}')" CANNOT

twin report >"$WORK/evidence/fidelity-report.txt" 2>&1 || true
kc get podgroups -A -o wide >"$WORK/evidence/podgroups-final.txt" 2>&1 || true

status=0
print_table "Run:ai $RUNAI_REF cluster side (KRM $KRM_REF, KAI $KAI_REF, kube-controller-manager $K8S_REF) against dc-twin ($(basename "$ESTATE")). Nodes are the kwok projection: IMPERSONATED (D24); no pod runs." || status=$?
echo
echo "artefacts: $WORK/evidence; logs: $WORK/runai, $WORK/cluster/logs, $WORK/twin/up.log"
exit $status

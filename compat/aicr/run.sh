#!/usr/bin/env bash
# Runs NVIDIA AI Cluster Runtime's CLI (aicr), built from its v1.0.0 tag and unmodified, against
# the twin's Kubernetes view: a real kube-apiserver and etcd (envtest's binaries) and, by default,
# the real kube-controller-manager, with dc-twin's kwok projection standing in for the GPU nodes'
# kubelets (IMPERSONATED, D24). It records what aicr recognises about the twin's nodes (snapshot,
# fingerprint, recipe) and what cannot work on a twin with no kubelet, OS or GPU (validator and
# snapshot-agent Jobs, OS readiness constraints, PCI discovery).
#
#   compat/aicr/run.sh                     # PASS/FAIL/CANNOT table on stdout, artefacts under $WORK
#   WORK=/tmp/aicr DCTWIN=bin/dctwin KCM=/path/kube-controller-manager compat/aicr/run.sh
#   KCM=none compat/aicr/run.sh            # no controller-manager: the Job steps show no pod at all
#
# Nothing here reads ~/.kube/config: aicr runs with HOME and KUBECONFIG pointing at the work
# directory (its local collectors read $KUBECONFIG, not --kubeconfig: see README).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
AICR_REF=${AICR_REF:-v1.0.0}          # github.com/NVIDIA/aicr
K8S_REF=${K8S_REF:-v1.37.0}
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
KCM=${KCM:-build}                     # build | none | <path to kube-controller-manager>
ADAPTERS=${ADAPTERS:-redfish,kwok,metrics}
LAB=${LAB:-$root/examples/estates/lab-model.yaml}           # every accelerator: 50 Nodes
GB300=${GB300:-$root/examples/estates/nvl72-refstack.yaml}  # one GB300 NVL72 rack: 18 Nodes
mkdir -p "$WORK/bin" "$WORK/src" "$WORK/evidence"
WORK=$(cd "$WORK" && pwd)

. "$here/../kai/lib/common.sh"
. "$here/../kai/lib/cluster.sh"
. "$here/../kai/lib/twin.sh"

ENVTEST_DIR=$(envtest_dir)
[ -x "$ENVTEST_DIR/kube-apiserver" ] || { echo "no envtest binaries: set KUBEBUILDER_ASSETS" >&2; exit 1; }
KUBECTL=${KUBECTL:-$ENVTEST_DIR/kubectl}
export ENVTEST_DIR KUBECTL
command -v "$DCTWIN" >/dev/null || { echo "dctwin not found: set DCTWIN" >&2; exit 1; }
DCTWIN=$(command -v "$DCTWIN")

# ---------------------------------------------------------------- build from source

if [ ! -d "$WORK/src/aicr/.git" ]; then
	git clone -q --depth 1 --branch "$AICR_REF" https://github.com/NVIDIA/aicr.git "$WORK/src/aicr"
fi
AICR_COMMIT=$(git -C "$WORK/src/aicr" rev-parse HEAD)
if [ ! -x "$WORK/bin/aicr" ]; then
	echo "building aicr $AICR_REF" >&2
	# the release's version stamp (.goreleaser.yaml ldflags), so the CLI names its own release
	(cd "$WORK/src/aicr" && go build -ldflags "-X github.com/NVIDIA/aicr/pkg/cli.version=${AICR_REF#v} -X github.com/NVIDIA/aicr/pkg/cli.commit=$AICR_COMMIT" \
		-o "$WORK/bin/aicr" ./cmd/aicr)
fi
case "$KCM" in
none) KCM_BIN= ;;
build)
	KCM_BIN="$WORK/bin/kube-controller-manager"
	if [ ! -x "$KCM_BIN" ]; then
		[ -d "$WORK/src/kubernetes/.git" ] || git clone -q --depth 1 --branch "$K8S_REF" https://github.com/kubernetes/kubernetes.git "$WORK/src/kubernetes"
		echo "building kube-controller-manager $K8S_REF (once; about ten minutes)" >&2
		(cd "$WORK/src/kubernetes" && go build -o "$KCM_BIN" ./cmd/kube-controller-manager)
	fi
	;;
*) KCM_BIN=$KCM ;;
esac

cleanup() {
	set +e
	twin_down
	cluster_down
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# aicr against the scratch cluster only: its own HOME, KUBECONFIG and --kubeconfig.
aicr() { (cd "$WORK" && HOME="$WORK/home" KUBECONFIG="$KUBECONFIG_FILE" "$WORK/bin/aicr" "$@"); }
mkdir -p "$WORK/home"
# fp <snapshot.json> <dimension>: a fingerprint dimension's value
fp() { python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["fingerprint"].get(sys.argv[2], {}).get("value", ""))' "$1" "$2"; }
fpnote() { python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["fingerprint"].get(sys.argv[2], {}).get("note", ""))' "$1" "$2"; }
# labels <snapshot.json> <label>: "<value>=<node count>" for each value of a node label
labels() {
	python3 -I - "$1" "$2" <<'PY'
import json, sys
snap, key = json.load(open(sys.argv[1])), sys.argv[2]
out = []
for m in snap["measurements"]:
    if m.get("type") != "NodeTopology":
        continue
    for st in m.get("subtypes") or []:
        if st.get("subtype") != "label":
            continue
        for k, v in (st.get("data") or {}).items():
            if k == key or k.startswith(key + "."):
                val, _, nodes = str(v).partition("|")
                out.append(f"{val}={len([n for n in nodes.split(',') if n])}")
print(", ".join(sorted(out)))
PY
}
node_count_ge() { [ "$(kc get nodes -o name | grep -c .)" -ge "$1" ]; }

# ---------------------------------------------------------------- the lab estate (every accelerator)

cluster_up "$WORK/cluster-lab" "$KCM_BIN"
twin_up "$LAB" "$WORK/twin-lab" "$ADAPTERS" "$KUBECONFIG_FILE"
wait_until 60 node_count_ge 50 >/dev/null || true

S="$WORK/evidence/snapshot-lab.json"
set +e
AICR_AGENT_MODE=true aicr snapshot --kubeconfig "$KUBECONFIG_FILE" --format json -o "$S" >"$WORK/evidence/snapshot-lab.log" 2>&1
rc=$?
set -e
record X1 "aicr snapshot, local collectors (AICR_AGENT_MODE=true), lab estate" "exit 0; nodeCount 50, gpuNodeCount 42, k8sVersion 1.37.0, region site-1" "exit $rc; nodeCount $(fp "$S" nodeCount), gpuNodeCount $(fp "$S" gpuNodeCount), k8sVersion $(fp "$S" k8sVersion), region $(fp "$S" region)" "$( [ $rc = 0 ] && [ "$(fp "$S" nodeCount)" = 50 ] && [ "$(fp "$S" gpuNodeCount)" = 42 ] && [ "$(fp "$S" k8sVersion)" = 1.37.0 ] && echo PASS || echo FAIL)"
p=$(labels "$S" nvidia.com/gpu.product)
record X2 "GPU products aicr reads from nvidia.com/gpu.product" "NVIDIA-GB300=18, NVIDIA-GB200=18, NVIDIA-H100-80GB-HBM3=4, NVIDIA-A100-SXM4-80GB=2" "$p" "$(echo "$p" | grep -q 'NVIDIA-GB300=18' && echo "$p" | grep -q 'NVIDIA-GB200=18' && echo "$p" | grep -q 'NVIDIA-H100-80GB-HBM3=4' && echo "$p" | grep -q 'NVIDIA-A100-SXM4-80GB=2' && echo PASS || echo FAIL)"
o="AMD $(labels "$S" amd.com/gpu.product-name); Gaudi $(labels "$S" habana.ai/gaudi.product); cliques $(labels "$S" nvidia.com/gpu.clique | sed -E 's/[0-9a-f-]{36}\.1/<uuid>.1/g')"
record X3 "Other accelerator and NVLink labels in the snapshot's node topology" "AMD_Instinct_MI355X=2, HL-325L=2, 2 cliques of 18" "$o" "$(echo "$o" | grep -q 'AMD_Instinct_MI355X=2' && echo "$o" | grep -q 'HL-325L=2' && [ "$(labels "$S" nvidia.com/gpu.clique | grep -o '=18' | grep -c .)" = 2 ] && echo PASS || echo FAIL)"
acc=$(fp "$S" accelerator) note=$(fpnote "$S" accelerator)
record X4 "Fingerprint accelerator on a mixed estate" "empty with note multi-gpu (more than one nvidia.com/gpu.product)" "'$acc' note '$note'" "$( [ -z "$acc" ] && [ "$note" = multi-gpu ] && echo PASS || echo FAIL)"
hostfail=$(grep -c -E 'GPU hardware detection failed|failed to collect (grub|kmod|release)|D-Bus not available' "$WORK/evidence/snapshot-lab.log" || true)
record X5 "Host collectors (PCI GPU discovery, OS release, kernel modules, systemd)" "nothing: they read the host aicr runs on, and no twin Node has an OS or PCI bus" "$hostfail host collectors skipped; GPU measurement has $(python3 -I -c 'import json,sys; print(sum(len(m.get("subtypes") or []) for m in json.load(open(sys.argv[1]))["measurements"] if m["type"]=="GPU"))' "$S") subtypes" CANNOT

set +e
aicr recipe --snapshot "$S" -o "$WORK/evidence/recipe-lab.yaml" >"$WORK/evidence/recipe-lab.log" 2>&1
rc=$?
set -e
comps=$(grep -o 'components=[0-9]*' "$WORK/evidence/recipe-lab.log" | head -1)
crit=$(awk '/^criteria:/{f=1;next} f&&/^[^ ]/{f=0} f' "$WORK/evidence/recipe-lab.yaml" 2>/dev/null | tr -s ' \n' ' ')
record X6 "aicr recipe --snapshot (lab estate)" "exit 0; accelerator any; a recipe for 50 nodes" "exit $rc; ${comps:-no recipe}; criteria${crit:+:}${crit% }" "$( [ $rc = 0 ] && echo "$crit" | grep -q 'accelerator: any' && echo PASS || echo FAIL)"

set +e
aicr validate --recipe "$WORK/evidence/recipe-lab.yaml" --snapshot "$S" --no-cluster --output "$WORK/evidence/ctrf-lab.json" >"$WORK/evidence/validate-lab.log" 2>&1
rc=$?
set -e
sum=$(python3 -I -c 'import json,sys; s=json.load(open(sys.argv[1]))["results"]["summary"]; print("{tests} tests, {passed} passed, {failed} failed, {skipped} skipped".format(**s))' "$WORK/evidence/ctrf-lab.json" 2>/dev/null || echo "no report")
ready=$(grep -o 'readiness constraint passed: name=[^ ]* expected=[^a]*actual=[^ ]*' "$WORK/evidence/validate-lab.log" | head -1)
record X7 "aicr validate --no-cluster (lab recipe, lab snapshot)" "exit 0; readiness K8s.server.version passes; every check skipped (dry run)" "exit $rc; ${ready:-no readiness line}; $sum" "$( [ $rc = 0 ] && [ -n "$ready" ] && echo PASS || echo FAIL)"

# The snapshot agent as a Job (aicr's default mode): it needs a scheduler and a kubelet.
set +e
aicr snapshot --kubeconfig "$KUBECONFIG_FILE" --namespace aicr-snapshot --timeout 30s --no-cleanup -o "$WORK/evidence/snapshot-job.yaml" >"$WORK/evidence/snapshot-job.log" 2>&1
rc=$?
set -e
pod=$(kc get pods -n aicr-snapshot -o jsonpath='{range .items[*]}{.metadata.name} sched={.spec.schedulerName} node={.spec.nodeName} phase={.status.phase} image={.spec.containers[0].image}{end}' 2>/dev/null || true)
jobs=$(kc get jobs -n aicr-snapshot -o name 2>/dev/null | grep -c . || true)
record X8 "aicr snapshot as an agent Job (default mode), 30 s timeout" "exit 5 (TIMEOUT): no kube-scheduler binds the agent pod and no kubelet would run it" "exit $rc; $jobs Job; pod: ${pod:-none}" "$( [ $rc = 5 ] && echo CANNOT || echo FAIL)"
kc get jobs,pods -n aicr-snapshot -o wide >"$WORK/evidence/snapshot-job-objects.txt" 2>&1 || true

twin_down
cluster_down

# ---------------------------------------------------------------- one GB300 NVL72 rack (a single GPU product)

cluster_up "$WORK/cluster-gb300" "$KCM_BIN"
twin_up "$GB300" "$WORK/twin-gb300" kwok "$KUBECONFIG_FILE"
wait_until 60 node_count_ge 18 >/dev/null || true
S="$WORK/evidence/snapshot-gb300.json"
set +e
AICR_AGENT_MODE=true aicr snapshot --kubeconfig "$KUBECONFIG_FILE" --format json -o "$S" >"$WORK/evidence/snapshot-gb300.log" 2>&1
rc=$?
set -e
record X9 "aicr snapshot, one GB300 NVL72 rack (nvl72-refstack estate)" "fingerprint accelerator gb300 (from nvidia.com/gpu.product NVIDIA-GB300), gpuNodeCount 18" "exit $rc; accelerator '$(fp "$S" accelerator)' (source nodeTopology.label.nvidia.com/gpu.product), gpuNodeCount $(fp "$S" gpuNodeCount), gpuModel '$(fp "$S" gpuModel)'" "$( [ "$(fp "$S" accelerator)" = gb300 ] && [ "$(fp "$S" gpuNodeCount)" = 18 ] && echo PASS || echo FAIL)"

set +e
aicr recipe --snapshot "$S" -o "$WORK/evidence/recipe-gb300-any.yaml" >"$WORK/evidence/recipe-gb300-any.log" 2>&1
rc=$?
set -e
ov=$(awk '/appliedOverlays:/{f=1;next} f&&/^ *- /{print $2;next} f{exit}' "$WORK/evidence/recipe-gb300-any.yaml" 2>/dev/null | tr '\n' ' ')
record X10 "aicr recipe --snapshot (GB300 rack)" "exit 0; criteria accelerator gb300; overlay gb300-any applied" "exit $rc; overlays ${ov% }" "$( [ $rc = 0 ] && echo "$ov" | grep -q gb300-any && echo PASS || echo FAIL)"

set +e
aicr recipe --snapshot "$S" --service generic --intent training --os ubuntu -o "$WORK/evidence/recipe-gb300-snap-training.yaml" >"$WORK/evidence/recipe-gb300-snap-training.log" 2>&1
rc=$?
set -e
record X11 "aicr recipe --snapshot --service generic --intent training --os ubuntu (GB300 rack)" "fails: the generic GB300 training overlay's os-ubuntu constraints (OS.release.ID, VERSION_ID, kernel) have no OS to read" "exit $rc; $(grep -o 'failed_constraints=[0-9]* excluded=\[[^]]*\]' "$WORK/evidence/recipe-gb300-snap-training.log" | head -1)" "$( [ $rc != 0 ] && echo CANNOT || echo FAIL)"

set +e
aicr recipe --service generic --accelerator gb300 --intent training --os ubuntu -o "$WORK/evidence/recipe-gb300-training.yaml" >"$WORK/evidence/recipe-gb300-training.log" 2>&1
rc1=$?
aicr validate --recipe "$WORK/evidence/recipe-gb300-training.yaml" --snapshot "$S" --no-cluster --output "$WORK/evidence/ctrf-gb300.json" >"$WORK/evidence/validate-gb300.log" 2>&1
rc=$?
set -e
why=$(grep -o 'readiness check could not evaluate: [^:]*' "$WORK/evidence/validate-gb300.log" | head -1)
record X12 "aicr validate --no-cluster (generic GB300 training recipe, GB300 snapshot)" "readiness fails closed (exit 2): no OS release in a twin snapshot" "recipe exit $rc1; validate exit $rc; ${why:-?}" "$( [ $rc1 = 0 ] && [ $rc = 2 ] && echo CANNOT || echo FAIL)"

status=0
print_table "aicr $AICR_REF ($AICR_COMMIT) against dc-twin (kube-controller-manager: $( [ -n "$KCM_BIN" ] && echo "$K8S_REF" || echo none)). Nodes are the kwok projection: IMPERSONATED (D24)." || status=$?
echo
echo "artefacts: $WORK/evidence (snapshots as JSON, recipes, CTRF reports, logs)"
exit $status

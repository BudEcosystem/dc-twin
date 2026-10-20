#!/usr/bin/env bash
# Runs NVIDIA's Kubernetes GPU stack — the device plugin, GPU Feature Discovery, the DRA driver,
# dcgm-exporter and Node Feature Discovery, unmodified and pinned — on REAL kubelets whose GPUs
# are the twin's: CONTAINER HOSTS (D49). Each selected twin host is one Kind node named for it, and
# NVIDIA's Mokka (the nvml-mock driver tree and mock libnvidia-ml.so) runs on it with a GPU profile
# dc-twin renders from the model (internal/mokka, cmd/dctwin-mokka): the twin's NVML names,
# counts, identity/v1 UUIDs and serials, PCI addresses, memory, VBIOS, NVLink domain and rack
# placement. While the twin runs, `dctwin-mokka sync` keeps each node's Mokka override document
# equal to the model, so an Xid 79 the twin injects makes that GPU fall off the bus in NVML.
#
# Every result is compared with the model and with the twin's own Kubernetes projection of the
# same hosts (the kwok adapter, impersonated, D24, on an envtest kube-apiserver) and its
# dcgm-exporter impersonation: where the real software and the twin's impersonation disagree, the
# disagreement is reported as a FINDING on one side or the other.
#
#   compat/gpu-operator/run.sh                  # PASS/FAIL/FINDING/CANNOT table on stdout, evidence under $WORK
#   HOSTS='a01/compute-tray-01 b01/server-01'   # the twin hosts made container hosts (default)
#   KEEP=1 compat/gpu-operator/run.sh           # leave the cluster and the twin running
#   KEEP=wait compat/gpu-operator/run.sh        # the same until $WORK/release exists, then tear down
#
# Needs a rootful Docker or Podman (Kind's nodes are containers, and Mokka makes device nodes in
# them; on a host whose engine is rootless, guest.sh runs this inside a KVM guest), helm 3.8+,
# kubectl, go, envtest's binaries (KUBEBUILDER_ASSETS) and python3. Nothing here reads ~/.kube/config or the user's helm
# repositories: every client is given the scratch cluster's kubeconfig and a scratch helm home.
# The Kind cluster is named dctwin-gpuop; it is deleted at the end, with the Kind node image when
# this run pulled it.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)

# ---------------------------------------------------------------- pins
KIND_REF=${KIND_REF:-v0.31.0}   # Mokka's CI (hack/e2e-ci-cluster.sh at v0.5.0) and its default node image
KIND_NODE=kindest/node:v1.35.0@sha256:452d707d4862f52530247495d180205e029056831160e22870e37e3f6c1ac31f
MOKKA_CHART=oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock MOKKA_VER=0.5.0 \
	MOKKA_SHA=f9c290aedd2cf0f100fd30c5d9db72595fc48150e3d6ccc262345af4a122ebb3   # image ghcr.io/nvidia/nvml-mock:0.5.0
DP_REPO=https://nvidia.github.io/k8s-device-plugin DP_VER=0.19.3 \
	DP_SHA=8f01067500e712508fe55fe060b6f895814dc7efc1cb96323db2ce4f777f6e96       # nvcr.io/nvidia/k8s-device-plugin:v0.19.3
NFD_REPO=https://kubernetes-sigs.github.io/node-feature-discovery/charts NFD_VER=0.19.0 \
	NFD_SHA=9e93b360e6167b782759026de40ba9d68d44c3e8b0b53b735592ad48fd3339ad      # registry.k8s.io/nfd/node-feature-discovery:v0.19.0
DRA_REPO=https://helm.ngc.nvidia.com/nvidia DRA_VER=0.5.0 \
	DRA_SHA=f9fd285657b70139fb72b3d0dfa97fbfdb6a5902cd03ba73cb5a9fc90cba7bfa      # nvcr.io/nvidia/dra-driver-nvidia-gpu:v0.5.0
WORKLOAD_IMAGE=debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587

CLUSTER=${CLUSTER:-dctwin-gpuop}
WORK=${WORK:-$(mktemp -d)}
ESTATE=${ESTATE:-$root/examples/estates/lab-model.yaml}
HOSTS=${HOSTS:-a01/compute-tray-01 b01/server-01}
FAULT_GPU=${FAULT_GPU:-a01/compute-tray-01/gpu-2}
mkdir -p "$WORK/bin" "$WORK/charts" "$WORK/evidence" "$WORK/helm"
WORK=$(cd "$WORK" && pwd)
EV=$WORK/evidence

. "$root/compat/kai/lib/common.sh"
. "$root/compat/kai/lib/cluster.sh"
. "$root/compat/kai/lib/twin.sh"

ENVTEST_DIR=$(envtest_dir)
[ -x "$ENVTEST_DIR/kube-apiserver" ] || { echo "no envtest binaries: set KUBEBUILDER_ASSETS (setup-envtest use -p path)" >&2; exit 1; }
KUBECTL=${KUBECTL:-$(command -v kubectl || echo "$ENVTEST_DIR/kubectl")}
export ENVTEST_DIR KUBECTL
for t in docker helm go python3 curl; do command -v "$t" >/dev/null || { echo "$t not found" >&2; exit 1; }; done
# Mokka's node agent makes its driver tree's character devices with mknod, which the kernel refuses
# in every user namespace but the first: on a rootless Docker or Podman, Kind's nodes are in one and
# Mokka never starts. guest.sh runs this script inside a KVM guest with a rootful Docker instead.
if docker info 2>/dev/null | grep -qiE '^ *rootless: *true|name=rootless'; then
	echo "the container engine is rootless: Mokka needs mknod, which no user namespace allows; run compat/gpu-operator/guest.sh" >&2
	exit 2
fi
export HELM_CONFIG_HOME=$WORK/helm/config HELM_CACHE_HOME=$WORK/helm/cache HELM_DATA_HOME=$WORK/helm/data

# ---------------------------------------------------------------- build and fetch
(cd "$root" && go build -o "$WORK/bin/dctwin" ./cmd/dctwin && go build -o "$WORK/bin/dctwin-mokka" ./cmd/dctwin-mokka)
DCTWIN=$WORK/bin/dctwin
KIND=${KIND:-$WORK/bin/kind}
[ -x "$KIND" ] || (cd "$WORK" && GOBIN="$WORK/bin" GOFLAGS=-mod=mod go install "sigs.k8s.io/kind@$KIND_REF")

sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }
fetch() { # fetch <file> <sha256> <helm pull args...>
	local f=$1 sha=$2
	shift 2
	[ -f "$WORK/charts/$f" ] || (cd "$WORK/charts" && helm pull "$@" >/dev/null)
	[ "$(sha256 "$WORK/charts/$f")" = "$sha" ] || { echo "$f: sha256 differs from the pin" >&2; exit 1; }
}
fetch "nvml-mock-$MOKKA_VER.tgz" "$MOKKA_SHA" "$MOKKA_CHART" --version "$MOKKA_VER"
fetch "nvidia-device-plugin-$DP_VER.tgz" "$DP_SHA" nvidia-device-plugin --repo "$DP_REPO" --version "$DP_VER"
fetch "node-feature-discovery-chart-$NFD_VER.tgz" "$NFD_SHA" node-feature-discovery --repo "$NFD_REPO" --version "$NFD_VER"
fetch "dra-driver-nvidia-gpu-$DRA_VER.tgz" "$DRA_SHA" dra-driver-nvidia-gpu --repo "$DRA_REPO" --version "$DRA_VER"
mkdir -p "$WORK/charts/mokka" && tar -xzf "$WORK/charts/nvml-mock-$MOKKA_VER.tgz" -C "$WORK/charts/mokka"

# ---------------------------------------------------------------- render the twin's hosts
SEL=$(echo "$HOSTS" | tr ' ' '\n' | sed '/^$/d' | paste -sd' ' -)
"$WORK/bin/dctwin-mokka" render "$ESTATE" --select "$SEL" --base "$WORK/charts/mokka/nvml-mock/profiles" --out "$WORK/render" >"$EV/render.txt"
cat "$EV/render.txt" >&2
NODES=$(python3 -I -c 'import json,sys; print(" ".join(h["Node"] for h in json.load(open(sys.argv[1]))))' "$WORK/render/hosts.json")

# ---------------------------------------------------------------- the cluster: one Kind node per twin host
KUBECONFIG_KIND=$WORK/kind/kubeconfig
export KUBECONFIG_KIND
kk() { "$KUBECTL" --kubeconfig "$KUBECONFIG_KIND" --cache-dir "$WORK/kind/kcache" "$@"; }
hh() { helm --kubeconfig "$KUBECONFIG_KIND" "$@"; }
mkdir -p "$WORK/kind"
python3 -I - "$WORK/render/hosts.json" "$KIND_NODE" "$CLUSTER" >"$WORK/kind/kind.yaml" <<'PY'
import json, re, sys
hosts, image, name = json.load(open(sys.argv[1])), sys.argv[2], sys.argv[3]
def lv(s):  # kwok's labelValue for a path
    v = re.sub(r"[^A-Za-z0-9._-]+", "-", s.replace("/", ".")).strip("-._")
    return v[:63]
print(f"""# generated by compat/gpu-operator/run.sh: one worker per twin host, named for it
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: {name}
containerdConfigPatches:
  - |-
    [plugins."io.containerd.nri.v1.nri"]
      disable = false
      disable_connections = false
      socket_path = "/var/run/nri/nri.sock"
# A Docker host shared with other work can starve a control plane of CPU for minutes: one control
# plane needs no leader election (losing a lease under load restarts the controller manager, whose
# bootstrap signer a joining node waits for), and the control plane and a joining node may take
# many minutes to come up.
kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    apiServer:
      timeoutForControlPlane: 30m0s
    controllerManager:
      extraArgs: {{ leader-elect: "false" }}
    scheduler:
      extraArgs: {{ leader-elect: "false" }}
  - |
    kind: JoinConfiguration
    discovery:
      timeout: 30m0s
nodes:
  - role: control-plane
    image: {image}""")
for h in hosts:
    print(f"""  - role: worker
    image: {image}
    labels: {{ dc-twin.io/host: container, dc-twin.io/path: "{lv(h['Path'])}" }}
    kubeadmConfigPatches:
      - |
        kind: JoinConfiguration
        nodeRegistration:
          name: {h['Node']}""")
PY
KIND_IMAGE_PULLED=0
KIND_IMAGE_REF=${KIND_NODE%%:*}@${KIND_NODE#*@}   # by digest: Docker and Podman both resolve it
docker image inspect "$KIND_IMAGE_REF" >/dev/null 2>&1 || KIND_IMAGE_PULLED=1
TWIN_PID= SYNC_PID= CLUSTER_PIDS=
cleanup() {
	set +e
	if [ "${KEEP:-}" = wait ]; then
		# Everything stays up, as with KEEP=1, until $WORK/release exists (at most four hours);
		# then it is torn down here, so a caller holding a lock around this script holds it for
		# the cluster's whole life.
		echo "KEEP=wait: inspect with KUBECONFIG=$KUBECONFIG_KIND; touch $WORK/release to tear down" >&2
		local i
		for i in $(seq 1 1440); do [ -e "$WORK/release" ] && break; sleep 10; done
		rm -f "$WORK/release"
	elif [ "${KEEP:-}" = 1 ]; then
		echo "KEEP=1: left running. Stop with: kill ${SYNC_PID:-}; DCTWIN_STATE=${TWIN_STATE:-} HOME=${TWIN_HOME:-} $DCTWIN down; kill ${CLUSTER_PIDS:-}; $KIND delete cluster --name $CLUSTER" >&2
		return
	fi
	[ -n "$SYNC_PID" ] && kill "$SYNC_PID" 2>/dev/null && wait "$SYNC_PID" 2>/dev/null
	twin_down
	cluster_down
	"$KIND" delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG_KIND" >/dev/null 2>&1
	if [ "$KIND_IMAGE_PULLED" = 1 ] && [ "${KEEP_IMAGES:-}" != 1 ]; then docker image rm "$KIND_IMAGE_REF" >/dev/null 2>&1; fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM
"$KIND" delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG_KIND" >/dev/null 2>&1 || true
# NODE_MEMORY caps each node container (e.g. 1300m) from the moment Kind creates it, before
# kubeadm starts anything in it: on a Docker host shared with other work, what runs here is then
# OOM-killed inside its own node rather than pressing the host into killing someone else's.
CAPPER=
rm -f "$WORK"/kind/capped-*
if [ -n "${NODE_MEMORY:-}" ]; then
	(
		end=$((SECONDS + 900))
		while [ $SECONDS -lt $end ]; do
			for c in $(docker ps --filter "label=io.x-k8s.kind.cluster=$CLUSTER" --format '{{.Names}}'); do
				[ -e "$WORK/kind/capped-$c" ] && continue
				docker update --memory "$NODE_MEMORY" --memory-swap "$NODE_MEMORY" "$c" >/dev/null 2>&1 && touch "$WORK/kind/capped-$c"
			done
			sleep 1
		done
	) &
	CAPPER=$!
fi
"$KIND" create cluster --config "$WORK/kind/kind.yaml" --kubeconfig "$KUBECONFIG_KIND" --wait 600s >&2
[ -n "$CAPPER" ] && kill "$CAPPER" 2>/dev/null
kk get nodes -o wide >"$EV/nodes-wide.txt"

# ---------------------------------------------------------------- the twin, and its own projection of the same hosts
rm -rf "$WORK/cluster" "$WORK/twin"
cluster_up "$WORK/cluster"
twin_up "$ESTATE" "$WORK/twin" "kwok,metrics" "$KUBECONFIG_FILE"
GATEWAY=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["gateway"])' "$TWIN_STATE/control.json")
echo "twin up: $(grep -o 'control API: [^ ]*' "$WORK/twin/up.log"); kwok on $APISERVER_URL; gateway $GATEWAY" >&2

# ---------------------------------------------------------------- Mokka: one release per container host, its profile the twin's
kk create namespace mokka >/dev/null
kk label namespace mokka pod-security.kubernetes.io/enforce=privileged >/dev/null
for n in $NODES; do
	hh install "nvml-mock-$n" "$WORK/charts/nvml-mock-$MOKKA_VER.tgz" -n mokka \
		--set-file gpu.customConfig="$WORK/render/$n.yaml" \
		--set "nodeSelector.kubernetes\.io/hostname=$n" \
		--set-string updateStrategy.rollingUpdate.maxUnavailable=100% \
		--wait --timeout 15m >"$EV/helm-mokka-$n.txt"
done
"$WORK/bin/dctwin-mokka" sync "$ESTATE" --select "$SEL" --control "$TWIN_STATE/control.json" -- "$here/put-override.sh" '{node}' \
	>"$WORK/sync.log" 2>&1 &
SYNC_PID=$!

# ---------------------------------------------------------------- NFD, the device plugin with GFD, the DRA driver, dcgm-exporter
hh install nfd "$WORK/charts/node-feature-discovery-chart-$NFD_VER.tgz" -n nfd --create-namespace \
	-f "$here/values/nfd.yaml" --wait --timeout 15m >"$EV/helm-nfd.txt"
hh install nvdp "$WORK/charts/nvidia-device-plugin-$DP_VER.tgz" -n nvidia-device-plugin --create-namespace \
	-f "$here/values/device-plugin.yaml" --wait --timeout 15m >"$EV/helm-device-plugin.txt"
# GFD reads the machine type from DMI, which a Kind node does not own; Mokka serves one through
# the CDI mount that carries its config (Mokka docs/helm-chart.md, "Machine type"). The device
# plugin's chart has no value for GFD's environment, so it is set on the DaemonSet.
kk -n nvidia-device-plugin set env ds/nvdp-nvidia-device-plugin-gpu-feature-discovery GFD_MACHINE_TYPE_FILE=/etc/nvml-mock/machine-type >/dev/null
kk -n nvidia-device-plugin rollout status ds/nvdp-nvidia-device-plugin-gpu-feature-discovery --timeout 10m >/dev/null
hh install dra "$WORK/charts/dra-driver-nvidia-gpu-$DRA_VER.tgz" -n nvidia-dra --create-namespace \
	-f "$here/values/dra.yaml" --wait --timeout 15m >"$EV/helm-dra.txt"
kk apply -f "$here/manifests/dcgm-exporter.yaml" >/dev/null
kk -n dc-twin-dcgm rollout status ds/dcgm-exporter --timeout 10m >/dev/null || true
kk get pods -A -o wide >"$EV/pods.txt"

# ---------------------------------------------------------------- checks
CMP="python3 -I $here/compare.py"
recordall() { while IFS= read -r l; do IFS='|' read -r a b c d e <<<"$l"; record "$a" "$b" "$c" "$d" "$e"; done; }
kwokc() { kc "$@"; }   # the twin's projection (envtest)

gfd_labelled() { for n in $NODES; do kk get node "$n" -o 'jsonpath={.metadata.labels.nvidia\.com/gpu\.product}' | grep -q . || return 1; done; }
alloc() { kk get node "$1" -o 'jsonpath={.status.allocatable.nvidia\.com/gpu}'; }
allocated() { for n in $NODES; do [ -n "$(alloc "$n")" ] || return 1; done; }
wait_until 300 allocated >/dev/null || true
wait_until 300 gfd_labelled >/dev/null || true
kwok_up() { for n in $NODES; do kwokc get node "$n" >/dev/null 2>&1 || return 1; done; }
wait_until 60 kwok_up >/dev/null || true
kk get nodes -o json >"$EV/nodes.json"
kwokc get nodes -o json >"$EV/kwok-nodes.json"

# 1. the nodes are the twin's hosts
recordall < <($CMP nodes "$WORK/render/hosts.json" "$EV/nodes.json")
# 2. what NVML says on each node, through the real nvidia-smi
for n in $NODES; do
	pod=$(kk -n mokka get pod -l app.kubernetes.io/name=nvml-mock --field-selector "spec.nodeName=$n" -o 'jsonpath={.items[0].metadata.name}')
	kk -n mokka exec "$pod" -c node-agent -- nvidia-smi \
		--query-gpu=index,name,uuid,serial,pci.bus_id,memory.total,vbios_version,compute_cap --format=csv,noheader,nounits >"$EV/smi-$n.csv" 2>&1 || true
	recordall < <($CMP smi "$WORK/render/hosts.json" "$n" "$EV/smi-$n.csv")
done
# 3. allocatable and GFD's labels against the model and the twin's projection
recordall < <($CMP labels "$WORK/render/hosts.json" "$EV/nodes.json" "$EV/kwok-nodes.json")

# 4. a pod asking the device plugin for a GPU, and what nvidia-smi shows inside it
kk create namespace dc-twin-gpu >/dev/null
for n in $NODES; do
	sed -e "s|@NAME@|gpu-$n|" -e "s|@NODE@|$n|" -e "s|@IMAGE@|$WORKLOAD_IMAGE|" "$here/manifests/gpu-pod.yaml.tmpl" | kk apply -f - >/dev/null
done
for n in $NODES; do
	if kk -n dc-twin-gpu wait --for=condition=Ready "pod/gpu-$n" --timeout=600s >/dev/null 2>&1; then
		kk -n dc-twin-gpu exec "gpu-$n" -- nvidia-smi -L >"$EV/pod-smi-$n.txt" 2>&1 || true
	else
		kk -n dc-twin-gpu describe pod "gpu-$n" >"$EV/pod-smi-$n.txt" 2>&1
	fi
	recordall < <($CMP podsmi "$WORK/render/hosts.json" "$n" "$EV/pod-smi-$n.txt" "device plugin")
done

# 5. DRA: ResourceSlices and a claim
kk get resourceslices -o json >"$EV/resourceslices.json" 2>/dev/null || echo '{"items":[]}' >"$EV/resourceslices.json"
recordall < <($CMP slices "$WORK/render/hosts.json" "$EV/resourceslices.json")
n1=${NODES%% *}
sed -e "s|@NAME@|claim-$n1|" -e "s|@NODE@|$n1|" -e "s|@IMAGE@|$WORKLOAD_IMAGE|" "$here/manifests/dra-claim.yaml.tmpl" | kk apply -f - >/dev/null
if kk -n dc-twin-gpu wait --for=condition=Ready "pod/claim-$n1" --timeout=300s >/dev/null 2>&1; then
	kk -n dc-twin-gpu exec "claim-$n1" -- nvidia-smi -L >"$EV/claim-smi-$n1.txt" 2>&1 || true
else
	kk -n dc-twin-gpu describe pod "claim-$n1" >"$EV/claim-smi-$n1.txt" 2>&1
fi
kk -n dc-twin-gpu get resourceclaims -o json >"$EV/resourceclaims.json"
recordall < <($CMP claim "$WORK/render/hosts.json" "$n1" "$EV/resourceclaims.json" "$EV/resourceslices.json" "$EV/claim-smi-$n1.txt")

# 6. dcgm-exporter on Mokka against the twin's dcgm-exporter impersonation
scrape_real() { # node -> the real exporter's /metrics through the API server's pod proxy
	local pod
	pod=$(kk -n dc-twin-dcgm get pod -l app=dcgm-exporter --field-selector "spec.nodeName=$1" -o 'jsonpath={.items[0].metadata.name}')
	[ -n "$pod" ] && kk get --raw "/api/v1/namespaces/dc-twin-dcgm/pods/$pod:9400/proxy/metrics"
}
scrape_twin() { curl -s --max-time 10 --proxytunnel -x "http://$GATEWAY" "http://$1:9400/metrics"; }
has_xid_series() { scrape_real "$1" | grep -q '^DCGM_FI_DEV_XID_ERRORS'; }
for n in $NODES; do
	wait_until 120 has_xid_series "$n" >/dev/null || true
	scrape_real "$n" >"$EV/dcgm-real-$n.txt" 2>&1 || true
	scrape_twin "$n" >"$EV/dcgm-twin-$n.txt" 2>&1 || true
	recordall < <($CMP dcgm "$WORK/render/hosts.json" "$n" "$EV/dcgm-real-$n.txt" "$EV/dcgm-twin-$n.txt")
done

# 7. a twin fault: Xid 79 on one GPU, then its clearing
fault_host=$(python3 -I -c 'import json,sys
for h in json.load(open(sys.argv[1])):
    for g in h["GPUs"]:
        if g["Path"].endswith("/"+sys.argv[2]): print(h["Node"], g["Index"], g["UUID"])' "$WORK/render/hosts.json" "$FAULT_GPU")
read -r fnode fidx fuuid <<<"$fault_host"
before=$(alloc "$fnode")
kk -n dc-twin-gpu delete pod "gpu-$fnode" "claim-$fnode" --ignore-not-found --wait=true >/dev/null 2>&1 || true
applied_lines() { grep -c "applied to $fnode" "$WORK/sync.log" || true; }
a0=$(applied_lines)
twin fault inject "$FAULT_GPU" nvidia/xid/xid-79 >"$EV/fault-inject.json"
fid=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))[0]["ID"])' "$EV/fault-inject.json" 2>/dev/null || true)
doc_failed() { [ "$(applied_lines)" -gt "$a0" ] && tail -n 20 "$WORK/sync.log" | grep -q fallen_off_bus; }
ms_doc=$(wait_until 60 doc_failed) && ok=0 || ok=1
record F1 "Xid 79 on $FAULT_GPU reaches $fnode's Mokka override document" "failure {mode: fallen_off_bus, xid 79} on GPU $fidx" \
	"$( [ $ok = 0 ] && echo "applied ${ms_doc} ms after the twin committed it" || echo "not applied in 60 s")" "$(pass_if $ok)"
pod=$(kk -n mokka get pod -l app.kubernetes.io/name=nvml-mock --field-selector "spec.nodeName=$fnode" -o 'jsonpath={.items[0].metadata.name}')
sleep 2   # one override TTL
kk -n mokka exec "$pod" -c node-agent -- nvml-mock-ctl status >"$EV/fault-ctl-status.txt" 2>&1 || true
kk -n mokka exec "$pod" -c node-agent -- nvidia-smi --query-gpu=index,uuid,pci.bus_id,temperature.gpu --format=csv,noheader >"$EV/fault-smi.txt" 2>&1 || true
recordall < <($CMP faultsmi "$fidx" "$fuuid" "$EV/fault-smi.txt")
dropped() { [ "$(alloc "$fnode")" -lt "$before" ]; }
ms_alloc=$(wait_until 120 dropped) && ok=0 || ok=1
after=$(alloc "$fnode")
kwok_after=$(kwokc get node "$fnode" -o 'jsonpath={.status.allocatable.nvidia\.com/gpu}')
kk get node "$fnode" -o json >"$EV/fault-node.json"
kk logs -n nvidia-device-plugin "$(kk -n nvidia-device-plugin get pod --field-selector "spec.nodeName=$fnode" -o name | grep -v gpu-feature-discovery | head -1)" --tail=200 >"$EV/fault-device-plugin.log" 2>&1 || true
recordall < <($CMP faultalloc "$before" "$after" "$kwok_after" "$ok" "$ms_alloc" "$EV/fault-device-plugin.log")
kk get resourceslices -o json >"$EV/fault-resourceslices.json"
recordall < <($CMP faultslices "$fnode" "$fuuid" "$EV/resourceslices.json" "$EV/fault-resourceslices.json")
sleep 6   # one dcgm-exporter collect interval
scrape_real "$fnode" >"$EV/fault-dcgm-real.txt" 2>&1 || true
scrape_twin "$fnode" >"$EV/fault-dcgm-twin.txt" 2>&1 || true
recordall < <($CMP faultdcgm "$fidx" "$EV/fault-dcgm-real.txt" "$EV/fault-dcgm-twin.txt")

# clearing: the document empties, NVML answers again, and allocatable is what the device plugin makes of it
a1=$(applied_lines)
twin fault clear "${fid:-$FAULT_GPU}" >"$EV/fault-clear.json" 2>&1 || true
doc_healthy() { [ "$(applied_lines)" -gt "$a1" ] && tail -n 5 "$WORK/sync.log" | grep -q 'every GPU healthy'; }
ms_doc=$(wait_until 60 doc_healthy) && ok=0 || ok=1
sleep 2
kk -n mokka exec "$pod" -c node-agent -- nvidia-smi --query-gpu=index,uuid,pci.bus_id,temperature.gpu --format=csv,noheader >"$EV/clear-smi.txt" 2>&1 || true
record F6 "the cleared fault empties $fnode's override document; NVML answers for GPU $fidx again" "document healthy; nvidia-smi reads GPU $fidx" \
	"$( [ $ok = 0 ] && echo "applied ${ms_doc} ms after the clear; $(grep -c "$fuuid" "$EV/clear-smi.txt") nvidia-smi row for $fuuid" || echo "not applied in 60 s")" \
	"$( [ $ok = 0 ] && grep -q "$fuuid" "$EV/clear-smi.txt" && echo PASS || echo FAIL)"
recovered() { [ "$(alloc "$fnode")" -ge "$before" ]; }
ms_rec=$(wait_until 90 recovered) && ok=0 || ok=1
kwok_clear=$(kwokc get node "$fnode" -o 'jsonpath={.status.allocatable.nvidia\.com/gpu}')
if [ $ok = 0 ]; then
	record F7 "allocatable on $fnode recovers when the twin clears the fault" "$before (twin projection: $kwok_clear)" "$(alloc "$fnode") after ${ms_rec} ms" PASS
else
	# The device plugin never re-admits a device its health checker marked unhealthy; a real host
	# gets it back when the plugin restarts (a reboot, which is what clears Xid 79 on hardware).
	stuck=$(alloc "$fnode")
	dp=$(kk -n nvidia-device-plugin get pod --field-selector "spec.nodeName=$fnode" -o name | grep -v gpu-feature-discovery | head -1)
	kk -n nvidia-device-plugin delete "$dp" --wait=true >/dev/null 2>&1 || true
	ms_rec=$(wait_until 120 recovered) && ok=0 || ok=1
	record F7 "allocatable on $fnode recovers when the twin clears the fault" "$before (twin projection: $kwok_clear)" \
		"stays $stuck for 90 s with NVML healthy: the device plugin never re-admits a device it marked unhealthy; $( [ $ok = 0 ] && echo "$(alloc "$fnode") ${ms_rec} ms after the plugin's pod restarts" || echo "not even after a restart")" FINDING
fi

# 8. what this run cannot show
record C1 "the host's power and AC state reach the node" "a twin power-off stops the kubelet" "a container host keeps running: power is not carried (D49)" CANNOT
record C2 "an Xid line on the node's kernel log" "NVRM: Xid … 79 in dmesg" "not written: a Kind node's kernel ring buffer is the Docker host's, shared by every container on it (Mokka nodeAgent.kernelLog off)" CANNOT

cp "$WORK/sync.log" "$EV/sync.log"
cp -r "$WORK/render" "$EV/render"
print_table "NVIDIA's Kubernetes GPU stack on container hosts driven by the twin (compat/gpu-operator)" | tee "$WORK/results.md"

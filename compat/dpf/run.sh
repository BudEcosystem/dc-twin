#!/usr/bin/env bash
# Runs NVIDIA DOCA Platform Framework (DPF), unmodified and built from a released tag, in
# zero-trust mode against the twin's BlueField-3 DPUs: DPF discovers the DPU BMCs on the
# management network through the site gateway, takes them over (factory reset, credentials, BMC
# firmware, mutual TLS with its own CA), and provisions them with the HBN guide's BFB and flavor
# over Redfish, as far as a twin with no DPU OS lets it (Q1). The twin's side is the redfish engine
# with the `bluefield` adapter in front of every BlueField BMC; the DPF side is DPF's provisioning
# controller and static cluster manager, cert-manager, and two envtest control planes (the host
# cluster DPF runs in, and the DPU cluster DPUs would join).
#
#   compat/dpf/run.sh                       # PASS/FAIL table on stdout, artefacts under $WORK
#   WORK=/tmp/dpf DCTWIN=bin/dctwin compat/dpf/run.sh
#   BFB=test compat/dpf/run.sh              # DPF's own 9 KB test BFB instead of the 1.7 GB one
#   KEEP=1 compat/dpf/run.sh                # leave everything running; prints how to stop it
#   DPU=vm compat/dpf/run.sh                # server-01's DPU at vm: an Arm the BFB push installs
#
# DPU=vm (D52) puts one DPU at vm fidelity: its Arm is a QEMU aarch64 machine (4 vCPUs; 4 GB for the
# DPU OS, 8 GB backed by a file for the BFB's installer; TCG on an x86_64 host, slow) that the BMC's
# BFB push installs and boots (lib/install-phase.sh: the installer runs with the stack down), cabled
# to the management network, so DPF's first boot can fetch its DPU agent and the agent can join the
# DPU cluster. The stack's services are then at site addresses the twin routes (siteServices):
# the host cluster's and the DPU cluster's API servers and the bfb-registry, which serves DPF's
# dpu-agent package at /deb (built from DPF's source, lib/agent-deb.sh; DEB_DIR reuses one). The DPU
# cluster becomes what a kubeadm-made control plane is to a joining node: a controller manager
# (bootstrap token signing, CSR approval and signing), a scheduler, and kubeadm's init phases that
# upload its configuration and publish cluster-info (KUBEADM, KCM and SCHEDULER name the v1.37.0
# binaries). The host cluster gets a controller manager too: it signs the DPU agent's client
# certificate, which DPF's provisioning controller approves.
#
# Linux only: DPF's controllers are Linux programs, run here as ordinary processes. The
# provisioning controller keeps BFBs under /bfb, an absolute path it does not let you change; it
# runs with $WORK/bfb mounted there in a mount namespace of its own (lib/nsroot.sh), so nothing is
# created at the host's root. Nothing reads ~/.kube/config: every client is given a scratch
# kubeconfig.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
DPF_REF=${DPF_REF:-v26.8.0}           # github.com/NVIDIA/doca-platform
CM_REF=${CM_REF:-v1.21.2}             # github.com/cert-manager/cert-manager
TAG=$DPF_REF
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
ESTATE=${ESTATE:-$here/estate.yaml}
ADAPTERS=${ADAPTERS:-redfish,dhcp,bluefield}
BFB=${BFB:-real}                      # real: the HBN guide's BFB; test: DPF's test-bfb.bfb
GUIDE_BFB_URL="https://content.mellanox.com/BlueField/BFBs/Ubuntu24.04/bf-bundle-3.5.0-89_26.07_ubuntu-24.04_64k_prod.bfb"
GUIDE_BFB_BYTES=1677035416
# the BMC firmware DPF pushes to a BF3 BMC older than BF-24.10-17 (Dockerfile.dpf-system at the tag)
BMC_FWPKG_URL="https://linux.mellanox.com/public/repo/bluefield/BMC/BF3/bmc/24.10-17/bf3-bmc-24.10-17_opn.fwpkg"
BMC_FWPKG_SHA256=4f5efcd101b1489dd83efb6160cf4e940864c07c0c77ac4b8717138531d63620
BMC_ROOT_PASSWORD=${BMC_ROOT_PASSWORD:-DcTwin-Dpf-$(openssl rand -hex 4)}
DPU=${DPU:-model}                     # model: BMCs only; vm: server-01's DPU has an Arm (D52)
case $DPU in
model) RUN_SECONDS=${RUN_SECONDS:-900} ;; # how long DPF is given to provision before the checks
vm) RUN_SECONDS=${RUN_SECONDS:-3600} ;;   # an aarch64 installer and boot under TCG
*) echo "DPU is model or vm" >&2; exit 1 ;;
esac
AGENT_SECONDS=${AGENT_SECONDS:-3600} # DPU=vm: how long the Arm's OS and DPU agent are given
# DPU=vm: AGENT_SKIP=connectx tells DPF's DPU agent, through the DPU flavor, to skip what needs a
# ConnectX (below); unset, the guide's flavor is used as it is
AGENT_SKIP=${AGENT_SKIP:-}
case $AGENT_SKIP in '' | connectx) ;; *) echo "AGENT_SKIP is connectx or unset" >&2; exit 1 ;; esac
[ -z "$AGENT_SKIP" ] || [ "$DPU" = vm ] || { echo "AGENT_SKIP needs DPU=vm" >&2; exit 1; }
# DPU=vm: where the stack's services are on the site (the twin routes them, siteServices)
SITE_HOST_API=10.90.0.10:443 SITE_DPU_API=10.90.0.11:443 SITE_REGISTRY=10.90.0.12:443
VM_DPU=w01/server-01/bf3-0
mkdir -p "$WORK/bin" "$WORK/src" "$WORK/evidence" "$WORK/dl"
WORK=$(cd "$WORK" && pwd)

. "$root/compat/kai/lib/common.sh"
. "$root/compat/kai/lib/twin.sh"
. "$here/lib/cluster.sh"
. "$here/lib/twin.sh"

ENVTEST_DIR=$(envtest_dir)
[ -x "$ENVTEST_DIR/kube-apiserver" ] || { echo "no envtest binaries: set KUBEBUILDER_ASSETS (setup-envtest use -p path)" >&2; exit 1; }
KUBECTL=${KUBECTL:-$ENVTEST_DIR/kubectl}
export ENVTEST_DIR KUBECTL
command -v "$DCTWIN" >/dev/null || { echo "dctwin not found: set DCTWIN (make build; DCTWIN=bin/dctwin)" >&2; exit 1; }
DCTWIN=$(command -v "$DCTWIN")
[ "$(uname -s)" = Linux ] || { echo "compat/dpf runs on Linux (DPF's controllers are Linux programs; see the header)" >&2; exit 1; }
command -v podman >/dev/null || { echo "rootless podman is needed for the provisioning controller's mount namespace (lib/nsroot.sh)" >&2; exit 1; }
# DPF's webhook server listens on :9443, which no flag changes; refuse rather than collide
if (exec 3<>/dev/tcp/127.0.0.1/9443) 2>/dev/null; then echo "port 9443 is in use: DPF's provisioning controller needs it for its webhook server" >&2; exit 1; fi

if [ "$DPU" = vm ]; then
	KUBEADM=${KUBEADM:-$(command -v kubeadm || true)} KCM=${KCM:-$(command -v kube-controller-manager || true)}
	SCHEDULER=${SCHEDULER:-$(command -v kube-scheduler || true)}
	for b in KUBEADM KCM SCHEDULER; do
		[ -x "${!b}" ] || { echo "DPU=vm needs $b (a v1.37.0 binary, as envtest's API server)" >&2; exit 1; }
	done
	for t in qemu-system-aarch64 xz; do command -v "$t" >/dev/null || { echo "DPU=vm needs $t" >&2; exit 1; }; done
fi
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi; }
free_gb() { df -k "$WORK" | awk 'NR==2 {printf "%d", $4/1048576}'; }

# ---------------------------------------------------------------- sources and builds

if [ ! -d "$WORK/src/doca-platform/.git" ]; then
	git clone -q --depth 1 --branch "$DPF_REF" https://github.com/NVIDIA/doca-platform.git "$WORK/src/doca-platform"
fi
DPF_COMMIT=$(git -C "$WORK/src/doca-platform" rev-parse HEAD)
if [ ! -d "$WORK/src/cert-manager/.git" ]; then
	git clone -q --depth 1 --branch "$CM_REF" https://github.com/cert-manager/cert-manager.git "$WORK/src/cert-manager"
fi
for c in provisioning static-cluster-manager; do
	if [ ! -x "$WORK/bin/$c" ]; then
		echo "building DPF $c at $DPF_REF" >&2
		(cd "$WORK/src/doca-platform" && CGO_ENABLED=0 go build -o "$WORK/bin/$c" "./cmd/$c")
	fi
done
if [ ! -x "$WORK/bin/cert-manager-controller" ]; then
	echo "building cert-manager controller at $CM_REF" >&2
	(cd "$WORK/src/cert-manager/cmd/controller" && go build -o "$WORK/bin/cert-manager-controller" .)
fi
if [ ! -x "$WORK/bin/bfbregistry" ]; then
	(cd "$root" && go build -o "$WORK/bin/bfbregistry" ./compat/dpf/bfbregistry)
fi
if [ "$DPU" = vm ]; then
	DEB_DIR=${DEB_DIR:-$WORK/deb}
	if [ ! -f "$DEB_DIR/Packages" ]; then
		echo "building DPF's dpu-agent package (arm64) at $DPF_REF" >&2
		DPF_REF=$DPF_REF "$here/lib/agent-deb.sh" "$WORK/src/doca-platform" "$DEB_DIR" >"$WORK/agent-deb.log" 2>&1 || { tail -20 "$WORK/agent-deb.log" >&2; exit 1; }
	fi
fi
if [ ! -f "$WORK/dl/bf3-bmc.fwpkg" ]; then
	echo "downloading the BF3 BMC firmware package DPF pushes (64 MB, NVIDIA)" >&2
	curl -sSfL -o "$WORK/dl/bf3-bmc.fwpkg" "$BMC_FWPKG_URL"
fi
[ "$(sha256 "$WORK/dl/bf3-bmc.fwpkg" | awk '{print $1}')" = "$BMC_FWPKG_SHA256" ] || { echo "bf3-bmc.fwpkg: sha256 mismatch" >&2; exit 1; }

# ---------------------------------------------------------------- bring-up

PIDS="" CLUSTER_DIRS="" BFBSRV_PID=""
cleanup() {
	set +e
	if [ "${KEEP:-}" = 1 ]; then
		echo "KEEP=1: left running. Stop with: pkill -f $WORK/bin/; HOME=${TWIN_HOME:-} $DCTWIN down --state ${TWIN_STATE:-}; kill $PIDS $BFBSRV_PID \$(cat $WORK/host/pids $WORK/dpu/pids)" >&2
		return
	fi
	local p
	for p in $PIDS $BFBSRV_PID; do kill "$p" 2>/dev/null; done
	[ -z "${SCHED_PID:-}" ] || kill "$SCHED_PID" 2>/dev/null
	# the provisioning controller runs below podman unshare, chroot and env: stop it by its binary
	pkill -f "$WORK/bin/provisioning" 2>/dev/null
	for p in $PIDS $BFBSRV_PID; do wait "$p" 2>/dev/null; done
	twin_down
	dpf_clusters_down
	# a check stopped part-way (INT, TERM) leaves the subshell of its $(wait_until …) polling,
	# holding whatever the harness inherited (a job lock's descriptor among them)
	pkill -TERM -P $$ 2>/dev/null
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# The stack's processes are started through these, so that DPU=vm can stop them for the BFB
# installer (which needs most of the memory a job may use, lib/install-phase.sh) and start them
# again on the same clusters. Their logs are appended to across restarts.
SCHED_PID="" CM_PID="" SCM_PID="" PROV_PID=""
start_scheduler() { # the DPU cluster's scheduler: what places a DPU service's pods on a DPU Node
	"$SCHEDULER" --kubeconfig "$DPU_KUBECONFIG" --leader-elect=false --secure-port 0 >>"$WORK/dpu/logs/kube-scheduler.log" 2>&1 &
	SCHED_PID=$!
}
start_cert_manager() {
	"$WORK/bin/cert-manager-controller" --kubeconfig "$HOST_KUBECONFIG" --leader-elect=false \
		--metrics-listen-address "127.0.0.1:$(free_port)" \
		--cluster-resource-namespace dpf-operator-system --v=2 >>"$WORK/logs/cert-manager.log" 2>&1 &
	CM_PID=$!
	PIDS="$PIDS $CM_PID"
}
start_static_cluster_manager() {
	KUBECONFIG="$HOST_KUBECONFIG" POD_NAMESPACE=dpf-operator-system HTTPS_PROXY="http://$GATEWAY" NO_PROXY="$no_proxy" \
		"$WORK/bin/static-cluster-manager" --leader-elect=false \
		--metrics-bind-address=0 --health-probe-bind-address="127.0.0.1:$(free_port)" >>"$WORK/logs/static-cluster-manager.log" 2>&1 &
	SCM_PID=$!
	PIDS="$PIDS $SCM_PID"
}
start_provisioning() {
	NSROOT_BINDS="$WORK/bfb:/bfb $WORK/certs/serviceaccount:/var/run/secrets/kubernetes.io/serviceaccount" \
		KUBECONFIG="$HOST_KUBECONFIG" POD_NAMESPACE=dpf-operator-system BMC_FW_FILE="$WORK/dl/bf3-bmc.fwpkg" TMPDIR="$WORK/tmp" \
		HTTPS_PROXY="http://$GATEWAY" NO_PROXY="$no_proxy" \
		"$here/lib/nsroot.sh" "$WORK/bin/provisioning" --dpu-install-interface=redfish --deployment-mode=zero-trust --leader-elect=false \
		--metrics-bind-address=0 --health-probe-bind-address="127.0.0.1:$(free_port)" \
		--bfb-registry-load-balancer-address="$REGISTRY_URL" \
		--redfish-client-cert-dir="$WORK/certs/redfish-client-cert" >>"$WORK/logs/provisioning.log" 2>&1 &
	PROV_PID=$!
	PIDS="$PIDS $PROV_PID"
}
# stack_stop / stack_start: DPF's controllers, cert-manager and both clusters (their data stays);
# the twin and the bfb-registry stay up
stack_stop() {
	# a process may be gone already (the provisioning controller's wrapper ends with it), and
	# wait returns a killed process's status: neither stops the harness
	local p
	pkill -f "$WORK/bin/provisioning" 2>/dev/null || true
	for p in $PROV_PID $SCM_PID $CM_PID $SCHED_PID; do kill "$p" 2>/dev/null || true; done
	for p in $PROV_PID $SCM_PID $CM_PID $SCHED_PID; do wait "$p" 2>/dev/null || true; done
	dpf_cluster_stop "$WORK/dpu"
	dpf_cluster_stop "$WORK/host"
}
stack_start() {
	dpf_cluster_start "$WORK/host"
	dpf_cluster_start "$WORK/dpu"
	[ "$DPU" != vm ] || start_scheduler
	start_cert_manager
	start_static_cluster_manager
	start_provisioning
}

rm -rf "$WORK/host" "$WORK/dpu" "$WORK/twin" "$WORK/logs" "$WORK/certs" "$WORK/manifests" "$WORK/evidence" "$WORK/bfbsrv" "$WORK/tmp"
# DPF downloads a BFB only when its file is missing: keep the guide's BFB from an earlier run
# (KEEP_BFB=1, when it is whole) and nothing else
keep=""
if [ "${KEEP_BFB:-}" = 1 ] && [ "$BFB" = real ] && [ -f "$WORK/bfb/dpf-operator-system-bf-bundle-$TAG.bfb" ] &&
	[ "$(wc -c <"$WORK/bfb/dpf-operator-system-bf-bundle-$TAG.bfb")" = "$GUIDE_BFB_BYTES" ]; then
	keep="$WORK/bfb.kept"
	mv "$WORK/bfb/dpf-operator-system-bf-bundle-$TAG.bfb" "$keep"
fi
rm -rf "$WORK/bfb"
mkdir -p "$WORK/bfb"
[ -z "$keep" ] || mv "$keep" "$WORK/bfb/dpf-operator-system-bf-bundle-$TAG.bfb"
mkdir -p "$WORK/evidence" "$WORK/logs"
if [ "$DPU" = vm ]; then
	# both with a controller manager: the DPU agent's client certificate in the host cluster is a
	# CSR DPF's controller approves and the host cluster's signer signs (kube-apiserver-client);
	# a DPU's kubelet joins the DPU cluster the same way
	dpf_cluster_up "$WORK/host" host "$KCM" "$SITE_HOST_API"
	dpf_cluster_up "$WORK/dpu" dpu "$KCM" "$SITE_DPU_API"
	start_scheduler
else
	dpf_cluster_up "$WORK/host" host
	dpf_cluster_up "$WORK/dpu" dpu
fi
hk() { "$KUBECTL" --kubeconfig "$HOST_KUBECONFIG" --cache-dir "$WORK/host/kcache" "$@"; }
dk() { "$KUBECTL" --kubeconfig "$DPU_KUBECONFIG" --cache-dir "$WORK/dpu/kcache" "$@"; }
echo "host cluster :$HOST_PORT, DPU cluster :$DPU_PORT (envtest $(basename "$ENVTEST_DIR"))" >&2
REGISTRY_PORT=$(free_port)

# CRDs: cert-manager's, DPF's provisioning API and its operator config
hk apply --server-side -f "$WORK/src/cert-manager/deploy/crds/" >/dev/null
hk apply --server-side -f "$WORK/src/doca-platform/config/provisioning/crd/bases/" \
	-f "$WORK/src/doca-platform/config/operator-crds/operator.dpu.nvidia.com_dpfoperatorconfigs.yaml" >/dev/null
# what DPF's install grants the DPU agent's bootstrap token: creating its client certificate's CSR
# (config/provisioning/rbac/dpuagent_bootstrap.yaml), which DPF's CSR controller then approves
hk apply -f "$WORK/src/doca-platform/config/provisioning/rbac/dpuagent_bootstrap.yaml" >/dev/null
crds_established() { [ "$(hk get crd -o 'jsonpath={range .items[*]}{.status.conditions[?(@.type=="Established")].status}{"\n"}{end}' | grep -c True)" -ge "$(hk get crd -o name | grep -c .)" ]; }
wait_until 60 crds_established >/dev/null

# cert-manager's controller, on this host
start_cert_manager

certs="$here/manifests/01-certificates.yaml"
if [ "$DPU" = vm ]; then
	# the registry is reached at its site address too (by the BMC and the DPU)
	mkdir -p "$WORK/manifests"
	sed "s|ipAddresses: \[\"127.0.0.1\"\]|ipAddresses: [\"127.0.0.1\", \"${SITE_REGISTRY%%:*}\"]|" "$certs" >"$WORK/manifests/01-certificates.yaml"
	certs="$WORK/manifests/01-certificates.yaml"
fi
hk apply -f "$here/manifests/00-namespace.yaml" -f "$certs" >/dev/null
secret_ready() { [ -n "$(hk get secret -n dpf-operator-system "$1" -o 'jsonpath={.data.tls\.crt}' 2>/dev/null)" ]; }
ms=$(wait_until 120 secret_ready dpf-provisioning-redfish-client-secret) || true
wait_until 60 secret_ready bfb-registry-server-cert >/dev/null || true
record S1 "cert-manager $CM_REF issues DPF's CA, the Redfish client certificate (CN root) and the bfb-registry certificate" "three secrets" \
	"$(for s in dpf-provisioning-ca-secret dpf-provisioning-redfish-client-secret bfb-registry-server-cert; do secret_ready $s && printf '%s ' $s; done)after ${ms} ms" \
	"$(secret_ready dpf-provisioning-redfish-client-secret && secret_ready bfb-registry-server-cert && echo PASS || echo FAIL)"

# The CA trust bundle DPF's operator keeps (internal/operator/controllers/ca_trust_bundle.go):
# the provisioning CA's certificate under ca.crt. No operator runs, so the harness writes it.
hk get secret -n dpf-operator-system dpf-provisioning-ca-secret -o 'jsonpath={.data.tls\.crt}' | base64 -d >"$WORK/dpf-ca.crt"
# bundle-hash: sha256 over the sorted, newline-joined sha256 fingerprints of the bundle's certificates
bundle_hash=$(openssl x509 -in "$WORK/dpf-ca.crt" -outform DER | sha256 | awk '{printf "%s", $1}' | sha256 | awk '{print $1}')
hk create configmap dpf-ca-trust-bundle -n dpf-operator-system --from-file=ca.crt="$WORK/dpf-ca.crt" \
	--from-literal=bundle-hash="$bundle_hash" >/dev/null
hk label configmap dpf-ca-trust-bundle -n dpf-operator-system dpu.nvidia.com/component=dpf-operator >/dev/null
mkdir -p "$WORK/certs/redfish-client" "$WORK/certs/registry"
for k in tls.crt tls.key; do
	hk get secret -n dpf-operator-system dpf-provisioning-redfish-client-secret -o "jsonpath={.data.${k//./\\.}}" | base64 -d >"$WORK/certs/redfish-client/$k"
	hk get secret -n dpf-operator-system bfb-registry-server-cert -o "jsonpath={.data.${k//./\\.}}" | base64 -d >"$WORK/certs/registry/$k"
done

# ---------------------------------------------------------------- the twin

if [ "$DPU" = vm ]; then
	# the estate with server-01's DPU at vm (its Arm runs the OS DPF installs: os: stack) and the
	# stack's services at their site addresses
	SITE_SERVICES="host-api@$SITE_HOST_API=127.0.0.1:$HOST_PORT dpu-api@$SITE_DPU_API=127.0.0.1:$DPU_PORT bfb-registry@$SITE_REGISTRY=127.0.0.1:$REGISTRY_PORT"
	mkdir -p "$WORK/twin"
	{
		awk '/^  stacks:/ { exit } { print }' "$ESTATE"
		printf '  stacks: [{ name: nvidia-dsx-os@0.1.0, outputs: { siteServices: "%s" } }]\n' "$SITE_SERVICES"
	} | sed "/^  packs:/i\\  fidelity: { default: model, overrides: [{ select: \"$VM_DPU\", level: vm, os: stack }] }" >"$WORK/twin/estate-vm.yaml"
	dpf_twin_up "$WORK/twin/estate-vm.yaml" "$WORK/twin" "$ADAPTERS" --allow-tcg
else
	twin_up "$ESTATE" "$WORK/twin" "$ADAPTERS"
fi
GATEWAY=$(grep -o 'site gateway (HTTP CONNECT): [0-9.:]*' "$WORK/twin/up.log" | awk '{print $NF}')
echo "twin up: gateway $GATEWAY, state $TWIN_STATE" >&2
GATEWAY_PORT=${GATEWAY##*:}
leases() { twin get '**' 2>/dev/null | awk '$1 ~ /\// && $6 ~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ {print $2, $6, $1}'; }
dpus_leased() { [ "$(leases | awk '$1 == "dpu"' | grep -c .)" -ge 2 ]; }
wait_until 60 dpus_leased >/dev/null || true
leases >"$WORK/evidence/leases.txt"
IP_RANGE_START=$(awk '{print $2}' "$WORK/evidence/leases.txt" | sort -t. -k4 -n | head -1)
IP_RANGE_END=$(awk '{print $2}' "$WORK/evidence/leases.txt" | sort -t. -k4 -n | tail -1)
DPU_BMCS=$(awk '$1 == "dpu" {print $3}' "$WORK/evidence/leases.txt")
record T1 "The twin's BMCs lease on the management network (twin site DHCP)" "2 BlueField BMCs among the BMCs" \
	"$(grep -c . "$WORK/evidence/leases.txt") BMCs in $IP_RANGE_START–$IP_RANGE_END, $(awk '$1 == "dpu"' "$WORK/evidence/leases.txt" | grep -c .) BlueField" \
	"$([ "$(awk '$1 == "dpu"' "$WORK/evidence/leases.txt" | grep -c .)" = 2 ] && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- the BFB

# What DPF downloads: the HBN guide's BFB (1.7 GB, NVIDIA's licence, into $WORK/bfb) when the
# disk allows it with 50 GB to spare, else DPF's own test BFB, which is not a boot stream (the BMC
# refuses it at the transfer).
if [ "$BFB" = real ] && [ ! -f "$WORK/bfb/dpf-operator-system-bf-bundle-$TAG.bfb" ] && [ "$(free_gb)" -lt $((GUIDE_BFB_BYTES / 1073741824 + 52)) ]; then
	echo "only $(free_gb) GB free: using DPF's test BFB instead of the 1.7 GB guide BFB (BFB=test)" >&2
	BFB=test
fi
case $BFB in
real) BFB_URL=$GUIDE_BFB_URL ;;
test)
	mkdir -p "$WORK/bfbsrv"
	cp "$WORK/src/doca-platform/internal/provisioning/controllers/testdata/test-bfb.bfb" "$WORK/bfbsrv/"
	BFBSRV_PORT=$(free_port)
	python3 -I -m http.server --bind 127.0.0.1 "$BFBSRV_PORT" --directory "$WORK/bfbsrv" >"$WORK/logs/bfbsrv.log" 2>&1 &
	BFBSRV_PID=$!
	BFB_URL="http://127.0.0.1:$BFBSRV_PORT/test-bfb.bfb"
	;;
*) echo "BFB is real or test" >&2; exit 1 ;;
esac

# ---------------------------------------------------------------- DPF

guide="$WORK/src/doca-platform/docs/public/user-guides/zero-trust/use-cases/hbn/manifests"
# where DPF tells a DPU the host cluster's API server is (DPFOperatorConfig overrides), and the
# DPU cluster's kubeconfig DPF reads and hands DPUs (its join endpoint): loopback at model, the
# site addresses at vm
API_HOST=127.0.0.1 API_PORT=$HOST_PORT DPU_KC=$DPU_KUBECONFIG REGISTRY_URL="https://127.0.0.1:$REGISTRY_PORT"
if [ "$DPU" = vm ]; then
	API_HOST=${SITE_HOST_API%%:*} API_PORT=${SITE_HOST_API##*:} DPU_KC=$WORK/dpu/kubeconfig-site REGISTRY_URL="https://$SITE_REGISTRY"
fi
subst() {
	sed -e "s|\$TAG|$TAG|g" -e "s|\$BFB_URL|$BFB_URL|g" -e "s|\$IP_RANGE_START|$IP_RANGE_START|g" -e "s|\$IP_RANGE_END|$IP_RANGE_END|g" \
		-e "s|\$TARGETCLUSTER_API_SERVER_HOST|$API_HOST|g" -e "s|\$TARGETCLUSTER_API_SERVER_PORT|$API_PORT|g" \
		-e "s|@TAG@|$TAG|g" -e "s|@DPU_KUBECONFIG_B64@|$(base64 <"$DPU_KC" | tr -d '\n')|g" "$@"
}
mkdir -p "$WORK/manifests"
subst "$guide/02-dpf-system-installation/operatorconfig.yaml" >"$WORK/manifests/operatorconfig.yaml"
subst "$guide/02-dpf-system-installation/dpudiscovery.yaml" >"$WORK/manifests/dpudiscovery.yaml"
subst "$guide/03.1-dpudeployment-installation-pf/bfb.yaml" >"$WORK/manifests/bfb.yaml"
subst "$guide/03.1-dpudeployment-installation-pf/hbn-dpuflavor.yaml" >"$WORK/manifests/hbn-dpuflavor.yaml"
if [ "$AGENT_SKIP" = connectx ]; then
	# The agent's operations after the DPU mode all need the ConnectX the Arm does not have:
	# nvconfig resolves the NIC's physical functions through devlink (p0/p1), the reboot it then
	# orders applies that NVConfig, and the SFs, VF MACs and OVS-DOCA bridges are the NIC's. The
	# agent skips an operation only by a command-line flag; a DPU flavor sets them with a systemd
	# drop-in among its configFiles, which DPF's cloud-init writes before the agent first starts —
	# the mechanism DPF's own e2e flavor uses (test/objects/infrastructure/dpuflavor.yaml, for
	# --skip-reboot-method-discovery). Without the reboot the flavor's kernel parameters would
	# never be in /proc/cmdline, so they are skipped too. Everything else is the guide's flavor,
	# and the agent still configures and starts its kubelet and joins with DPF's kubeadm join.
	cat >"$WORK/manifests/agent-skip-connectx.yaml" <<'AS'
  - operation: override
    path: /etc/systemd/system/dpu-agent.service.d/20-dctwin-no-connectx.conf
    permissions: "0644"
    raw: |
      [Service]
      ExecStart=
      ExecStart=/bin/bash -c '/opt/dpf/dpuagent $(cat /opt/dpf/dpuagent.conf) --skip-nvconfig --skip-reboot --skip-kernel-cmd-line --skip-sf-config --skip-vf-mac --skip-ovs-raw-script'
AS
	sed -i "/^  configFiles:\$/r $WORK/manifests/agent-skip-connectx.yaml" "$WORK/manifests/hbn-dpuflavor.yaml"
	grep -q 20-dctwin-no-connectx "$WORK/manifests/hbn-dpuflavor.yaml" || { echo "AGENT_SKIP: the flavor has no configFiles to add to" >&2; exit 1; }
fi
subst "$here/manifests/03-dpucluster.yaml" >"$WORK/manifests/dpucluster.yaml"
subst "$here/manifests/05-dpuset.yaml" >"$WORK/manifests/dpuset.yaml"
hk apply -f "$WORK/manifests/operatorconfig.yaml" -f "$WORK/manifests/dpucluster.yaml" >/dev/null
hk create secret generic -n dpf-operator-system bmc-shared-password --from-literal=password="$BMC_ROOT_PASSWORD" >/dev/null

# the bfb-registry stand-in, serving DPF's /bfb on this host's loopback with the certificate DPF's
# issuer signed for it
"$WORK/bin/bfbregistry" -root "$WORK/bfb" -listen "127.0.0.1:$REGISTRY_PORT" ${DEB_DIR:+-deb "$DEB_DIR"} \
	-cert "$WORK/certs/registry/tls.crt" -key "$WORK/certs/registry/tls.key" >"$WORK/logs/bfb-registry.log" 2>&1 &
PIDS="$PIDS $!"

if [ "$DPU" = vm ]; then
	# The DPU cluster as a kubeadm-made control plane presents itself to a joining node: its
	# ClusterConfiguration and KubeletConfiguration uploaded, cluster-info published in kube-public
	# (signed by the controller manager's bootstrap signer) and the bootstrap-token RBAC. kubeadm
	# reaches the API server at its site address, through the site gateway, so cluster-info names
	# the address a DPU joins at.
	cat >"$WORK/dpu/kubeadm.yaml" <<KA
apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
bootstrapTokens: [{ ttl: 1h0m0s }]
localAPIEndpoint: { advertiseAddress: ${SITE_DPU_API%%:*}, bindPort: ${SITE_DPU_API##*:} }
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
kubernetesVersion: v1.37.0
clusterName: dpu-cluster-1
controlPlaneEndpoint: $SITE_DPU_API
networking: { serviceSubnet: 10.0.0.0/24, podSubnet: 10.244.0.0/16, dnsDomain: cluster.local }
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: systemd
clusterDNS: [10.0.0.10]
KA
	for ph in "upload-config kubeadm" "upload-config kubelet" "bootstrap-token --skip-token-print"; do
		# shellcheck disable=SC2086
		HTTPS_PROXY="http://$GATEWAY" NO_PROXY=localhost,127.0.0.1 "$KUBEADM" init phase $ph --config "$WORK/dpu/kubeadm.yaml" \
			--kubeconfig "$WORK/dpu/kubeconfig-site" >>"$WORK/logs/kubeadm-init-phases.log" 2>&1 || true
	done
	ci=$(dk get configmap -n kube-public cluster-info -o 'jsonpath={.data.kubeconfig}' 2>/dev/null | grep -o 'server: .*' || true)
	cfgs=$(dk get configmap -n kube-system kubeadm-config kubelet-config -o name 2>/dev/null | grep -c . || true)
	record K1 "The DPU cluster presents itself as kubeadm's control plane (DPU=vm)" "kubeadm-config, kubelet-config, cluster-info at the site address" \
		"$cfgs configs uploaded; cluster-info ${ci:-missing}" "$([ "$cfgs" = 2 ] && [ "$ci" = "server: https://$SITE_DPU_API" ] && echo PASS || echo FAIL)"
fi

# DPF's static cluster manager and provisioning controller, as processes. Their Redfish clients
# reach the BMCs through the twin's site gateway (HTTPS_PROXY); Go never proxies loopback, so the
# API servers and the registry are direct, and the BFB's host is listed in NO_PROXY.
no_proxy="localhost,127.0.0.1,$(echo "$BFB_URL" | awk -F/ '{print $3}' | cut -d: -f1)"
start_static_cluster_manager
# the webhook server's certificate (os.TempDir()/k8s-webhook-server/serving-certs): DPF's webhooks
# are not registered with the API server (no operator installs their configurations), but the
# manager will not start without one
mkdir -p "$WORK/tmp/k8s-webhook-server/serving-certs"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 \
	-subj "/CN=dpf-provisioning-webhook-service.dpf-operator-system.svc" \
	-keyout "$WORK/tmp/k8s-webhook-server/serving-certs/tls.key" -out "$WORK/tmp/k8s-webhook-server/serving-certs/tls.crt" 2>/dev/null
# what a pod's service account volume holds: DPF puts the host cluster's CA in each DPU's bf.cfg
# (for the DPU agent) from /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
mkdir -p "$WORK/certs/serviceaccount"
cp "$WORK/host/pki/ca.crt" "$WORK/certs/serviceaccount/ca.crt"
printf dpf-operator-system >"$WORK/certs/serviceaccount/namespace"
cp -R "$WORK/certs/redfish-client" "$WORK/certs/redfish-client-cert"
start_provisioning

hk apply -f "$WORK/manifests/dpudiscovery.yaml" >/dev/null
echo "DPF $DPF_REF ($DPF_COMMIT) running; discovery over $IP_RANGE_START–$IP_RANGE_END; BFB $BFB_URL" >&2

. "$here/lib/checks.sh"

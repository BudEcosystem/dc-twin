#!/usr/bin/env bash
# Runs Metal3's bare-metal operator (BMO), built from a released tag and unmodified, against
# dc-twin's emulated BMCs: BMO on a real kube-apiserver and etcd (envtest's binaries, optionally
# with the real kube-controller-manager), driving a real OpenStack Ironic configured as Metal3's
# ironic-image configures it. BareMetalHosts are created for the twin's BMCs over Redfish (the
# generic, iDRAC and iLO 5 address schemes, through the twin's site gateway at each BMC's leased
# address) and over IPMI (at the BMC's loopback port), and flow.py checks every effect in the twin's
# model and event log as well as in BMO's status and Ironic's record.
#
#   compat/metal3/run.sh                                  # PASS/FAIL table; artefacts under $WORK
#   WORK=/tmp/m3 DCTWIN=./bin/dctwin IRONIC_VENV=/tmp/ironic/venv compat/metal3/run.sh
#
# KUBEBUILDER_ASSETS  directory with etcd and kube-apiserver (default: the newest setup-envtest
#                     installed; see compat/kai/lib/common.sh)
# KCM                 a kube-controller-manager binary (optional): garbage collection of the
#                     objects BMO owns, and namespace deletion. Without it those are not exercised
# IRONIC_VENV         a venv with ironic 39.0.0 (compat/ironic/run.sh makes one); default: made in
#                     $WORK/venv with the same pins
# BMO_BIN, BMO_PLUGIN_DIR  a baremetal-operator binary and the directory holding the
#                     ironic-provisioner.so built with it, at $BMO_REF with the same Go toolchain
#                     (skips the build, about four minutes)
# IPMITOOL            ipmitool for Ironic's ipmi hardware type (default: the one on PATH)
# KUBECTL             default: envtest's kubectl beside kube-apiserver, else the one on PATH
#
# Everything it starts listens on loopback ports the kernel chooses; the only kubeconfig any
# process is given is the scratch one under $WORK, and BMO and Ironic run with HOME under $WORK.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
# shellcheck source=../kai/lib/common.sh
. "$root/compat/kai/lib/common.sh"
# shellcheck source=../kai/lib/cluster.sh
. "$root/compat/kai/lib/cluster.sh"
# shellcheck source=../kai/lib/twin.sh
. "$root/compat/kai/lib/twin.sh"

BMO_REF=v0.14.2                     # released 2026-10-15
BMO_COMMIT=f3d404e900df6dac91cce68e459fbda2413c7562
IRONIC_VERSION=39.0.0               # what Metal3's ironic-image v39.0.0 ships (stable/2026.2)
IRONICCLIENT_VERSION=6.3.0
CONSTRAINTS=${CONSTRAINTS:-https://releases.openstack.org/constraints/upper/2026.2}
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
KCM=${KCM:-}
mkdir -p "$WORK"/{bin,plugins,logs,home}
WORK=$(cd "$WORK" && pwd)
DCTWIN=$(command -v "$DCTWIN")
ENVTEST_DIR=$(envtest_dir)
[ -x "$ENVTEST_DIR/kube-apiserver" ] || { echo "no envtest binaries: set KUBEBUILDER_ASSETS (setup-envtest use -p path 1.37.0)" >&2; exit 1; }
if [ -z "${KUBECTL:-}" ] && [ -x "$ENVTEST_DIR/kubectl" ]; then KUBECTL="$ENVTEST_DIR/kubectl"; fi
KUBECTL=$(command -v "${KUBECTL:-kubectl}")
IPMITOOL=${IPMITOOL:-$(command -v ipmitool || true)}
[ -n "$IPMITOOL" ] || { echo "no ipmitool: Ironic's ipmi hardware type needs it; set IPMITOOL" >&2; exit 1; }

pids=()
bmo_pid= ironic_pid= http_pid=
stop() { # stop <pid>: TERM, wait up to 10 s, then KILL
	[ -n "$1" ] || return 0
	kill "$1" 2>/dev/null || return 0
	local i
	for i in $(seq 1 40); do kill -0 "$1" 2>/dev/null || break; sleep 0.25; done
	kill -9 "$1" 2>/dev/null || true
	wait "$1" 2>/dev/null || true
}
cleanup() {
	set +eu
	stop "$bmo_pid"
	stop "$ironic_pid"
	[ -n "${IR:-}" ] && pkill -f "$IR/ironic.conf" 2>/dev/null
	stop "$http_pid"
	twin_down
	cluster_down
	echo "stopped BMO, Ironic, the twin, kube-apiserver and etcd; artefacts in $WORK" >&2
}
trap cleanup EXIT

# ---------------------------------------------------------------- BMO at the tag
src="$WORK/src/baremetal-operator"
if [ ! -d "$src/.git" ]; then
	git -c advice.detachedHead=false clone -q --depth 1 --branch "$BMO_REF" https://github.com/metal3-io/baremetal-operator "$src" 2>&1 |
		grep -v 'is not a commit' >&2 || true # an annotated tag; the commit is checked below
fi
got=$(git -C "$src" rev-parse HEAD)
[ "$got" = "$BMO_COMMIT" ] || { echo "baremetal-operator $BMO_REF is $got, expected $BMO_COMMIT" >&2; exit 1; }
if [ -z "${BMO_BIN:-}" ]; then
	# Since v0.14 the Ironic provisioner is a Go plugin BMO loads at start: the binary and the
	# plugin must come from one toolchain and one module graph, so both are built here (as the
	# project's Makefile builds them: `manager`, `ironic-plugin`).
	echo "building baremetal-operator $BMO_REF and its Ironic provisioner plugin ..." >&2
	go -C "$src" build -o "$WORK/bin/baremetal-operator" .
	CGO_ENABLED=1 go -C "$src" build -buildmode=plugin -o "$WORK/plugins/ironic-provisioner.so" ./pkg/provisioner/ironic/plugin/
	BMO_BIN="$WORK/bin/baremetal-operator"
	BMO_PLUGIN_DIR="$WORK/plugins"
fi
: "${BMO_PLUGIN_DIR:?set BMO_PLUGIN_DIR with BMO_BIN}"

# ---------------------------------------------------------------- Ironic's venv
if [ -z "${IRONIC_VENV:-}" ]; then
	IRONIC_VENV="$WORK/venv"
	if [ ! -x "$IRONIC_VENV/bin/ironic" ]; then
		for p in python3.13 python3.12 python3.11; do command -v "$p" >/dev/null && { PYTHON=$p; break; }; done
		: "${PYTHON:?no Python 3.11+ found; set IRONIC_VENV}"
		echo "installing ironic $IRONIC_VERSION (constraints $CONSTRAINTS)" >&2
		"$PYTHON" -m venv "$IRONIC_VENV"
		"$IRONIC_VENV/bin/pip" install -q --timeout 30 --retries 10 -c "$CONSTRAINTS" \
			"ironic==$IRONIC_VERSION" "python-ironicclient==$IRONICCLIENT_VERSION"
	fi
fi
"$IRONIC_VENV/bin/python" -I -c "import importlib.metadata as m, sys; v = m.version('ironic'); sys.exit(v != '$IRONIC_VERSION' and f'ironic {v} in $IRONIC_VENV, expected $IRONIC_VERSION')"
if [ "$(uname -s)" = Darwin ] && "$IRONIC_VENV/bin/python" -I -c 'import setproctitle' 2>/dev/null; then
	# macOS only, as in compat/ironic: setproctitle is not fork-safe there and every conductor
	# process cotyledon forks dies with SIGSEGV; cotyledon treats the module as optional
	"$IRONIC_VENV/bin/pip" uninstall -y -q setproctitle
fi
PY="$IRONIC_VENV/bin/python"

# ---------------------------------------------------------------- kube-apiserver, etcd (and KCM)
cluster_up "$WORK/k8s" "$KCM"
echo "kube-apiserver $(kc version -o json | "$PY" -I -c 'import json,sys; print(json.load(sys.stdin)["serverVersion"]["gitVersion"])') at $APISERVER_URL${KCM:+, with kube-controller-manager}" >&2
# every CRD BMO's manager watches, from the source tree at the tag (config/base/crds/bases)
kc apply --server-side -f "$src/config/base/crds/bases" >"$WORK/logs/crds.txt"
kc wait --for condition=Established --timeout 60s crd --all >/dev/null
kc create namespace metal3 >/dev/null

# ---------------------------------------------------------------- the twin
twin_up "$root/examples/estates/lab-model.yaml" "$WORK/twin" redfish,ipmi,dhcp
GATEWAY=$("$PY" -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["gateway"])' "$TWIN_STATE/control.json")
echo "twin up (lab-model; redfish, ipmi, dhcp), site gateway $GATEWAY" >&2

# ---------------------------------------------------------------- Ironic, as ironic-image runs it
IR="$WORK/ironic"
rm -rf "$IR"
mkdir -p "$IR"/{log,state,tmp,locks,certs,tftpboot/master_images,html/images,html/tmp,html/master_images}
api_port=$(free_port) http_port=$(free_port)
# Metal3's ironic-image serves this directory with httpd (runhttpd) and fills images/ with IPA
# (ipa-downloader). No host here can run IPA, so the deploy kernel and ramdisk are placeholders:
# Ironic downloads and caches them and writes the iPXE configuration, and no host ever runs them.
printf 'placeholder: not an IPA kernel (compat/metal3)\n' >"$IR/html/images/ironic-python-agent.kernel"
printf 'placeholder: not an IPA ramdisk (compat/metal3)\n' >"$IR/html/images/ironic-python-agent.initramfs"
"$PY" -I -m http.server --bind 127.0.0.1 --directory "$IR/html" "$http_port" >"$WORK/logs/httpd.log" 2>&1 &
http_pid=$!
sed -e "s|@WORK@|$IR|g" -e "s|@API_PORT@|$api_port|g" -e "s|@HTTP_PORT@|$http_port|g" "$here/ironic.conf.in" >"$IR/ironic.conf"
"$IRONIC_VENV/bin/ironic-dbsync" --config-file "$IR/ironic.conf" create_schema >"$IR/dbsync.log" 2>&1
# Ironic reaches Redfish BMCs as it would on a site network, through the twin's site gateway (an
# HTTP CONNECT proxy, which sushy honours from HTTPS_PROXY); IPMI is UDP to the BMC's loopback port.
(cd "$IR" && exec env -i HOME="$WORK/home" PATH="$(dirname "$IPMITOOL"):/usr/bin:/bin" \
	HTTPS_PROXY="http://$GATEWAY" https_proxy="http://$GATEWAY" NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost \
	"$IRONIC_VENV/bin/ironic" --config-file "$IR/ironic.conf" >"$IR/ironic.stdout" 2>&1) &
ironic_pid=$!
echo "ironic $IRONIC_VERSION starting (pid $ironic_pid, API http://127.0.0.1:$api_port, httpd :$http_port)" >&2
for _ in $(seq 1 300); do
	curl -s -o /dev/null "http://127.0.0.1:$api_port/v1" && break
	kill -0 "$ironic_pid" 2>/dev/null || { tail -20 "$IR/ironic.stdout" "$IR/log/ironic.log" >&2; exit 1; }
	sleep 1
done
curl -s -o /dev/null "http://127.0.0.1:$api_port/v1" || { echo "Ironic API did not come up" >&2; exit 1; }
# the conductor registers its drivers a little after the API answers
for _ in $(seq 1 120); do
	curl -s "http://127.0.0.1:$api_port/v1/drivers" | grep -q '"idrac"' && break
	sleep 1
done

# ---------------------------------------------------------------- BMO
health_port=$(free_port)
# The variables are BMO's own (pkg/provisioner/ironic/factory.go, clients/auth.go). The TLS and
# auth paths point at files that do not exist, which is how BMO is told: plain HTTP, no auth
# (otherwise it would look under /opt/metal3).
env -i HOME="$WORK/home" PATH=/usr/bin:/bin KUBECONFIG="$KUBECONFIG_FILE" \
	PROVISIONER_PLUGIN_DIR="$BMO_PLUGIN_DIR" POD_NAMESPACE=metal3 \
	IRONIC_ENDPOINT="http://127.0.0.1:$api_port/v1/" \
	DEPLOY_KERNEL_URL="http://127.0.0.1:$http_port/images/ironic-python-agent.kernel" \
	DEPLOY_RAMDISK_URL="http://127.0.0.1:$http_port/images/ironic-python-agent.initramfs" \
	IRONIC_CACERT_FILE="$WORK/none/ca.crt" IRONIC_CLIENT_CERT_FILE="$WORK/none/tls.crt" \
	IRONIC_CLIENT_PRIVATE_KEY_FILE="$WORK/none/tls.key" METAL3_AUTH_ROOT_DIR="$WORK/none" \
	"$BMO_BIN" -namespace metal3 -provisioner ironic -dev -webhook-port 0 -metrics-addr 0 \
	-health-addr "127.0.0.1:$health_port" >"$WORK/logs/bmo.log" 2>&1 &
bmo_pid=$!
for _ in $(seq 1 120); do grep -q 'Starting workers' "$WORK/logs/bmo.log" 2>/dev/null && break; kill -0 "$bmo_pid" 2>/dev/null || break; sleep 0.5; done
grep -q 'Starting workers' "$WORK/logs/bmo.log" || { tail -30 "$WORK/logs/bmo.log" >&2; exit 1; }
echo "baremetal-operator $BMO_REF running (pid $bmo_pid)" >&2

# HOLD=1 keeps everything running for a person (or another flow) to drive: the environment is
# written to $WORK/env.sh, and the run stops on Ctrl-C or when BMO exits
if [ "${HOLD:-0}" = 1 ]; then
	cat >"$WORK/env.sh" <<ENV
export KUBECONFIG_FILE='$KUBECONFIG_FILE' IRONIC_URL='http://127.0.0.1:$api_port' IRONIC_LOG='$IR/log/ironic.log'
export BMO_LOG='$WORK/logs/bmo.log' TWIN_HOME='$TWIN_HOME' DCTWIN_STATE='$TWIN_STATE' DCTWIN_ASSETS='$DCTWIN_ASSETS' PY='$PY'
ENV
	echo "holding; environment in $WORK/env.sh" >&2
	wait "$bmo_pid"
	exit 0
fi

# ---------------------------------------------------------------- the flow
set +e
HOME="$TWIN_HOME" DCTWIN_STATE="$TWIN_STATE" DCTWIN_ASSETS="$DCTWIN_ASSETS" "$PY" -I "$here/flow.py" \
	--kubeconfig "$KUBECONFIG_FILE" --kubectl "$KUBECTL" --dctwin "$DCTWIN" --work "$WORK" \
	--ironic "http://127.0.0.1:$api_port" --ironic-log "$IR/log/ironic.log" --bmo-log "$WORK/logs/bmo.log" \
	${KCM:+--kcm} | tee "$WORK/flow.txt"
status=${PIPESTATUS[0]}
set -e
exit "$status"

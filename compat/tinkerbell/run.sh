#!/usr/bin/env bash
# Runs Tinkerbell's BMC controller, Rufio, unmodified and at a released tag, against dc-twin's
# emulated BMCs, on a real kube-apiserver and etcd (envtest's binaries) -- no cluster, no
# container runtime. Rufio was archived as its own repository after v0.6.5 and now ships inside
# the Tinkerbell monorepo (github.com/tinkerbell/tinkerbell, rufio/), built into the single
# `tinkerbell` binary; this harness builds that binary at the tag and runs it with every service
# but Rufio switched off by its own flags.
#
#   KUBEBUILDER_ASSETS=$(setup-envtest use -p path 1.37.0) compat/tinkerbell/run.sh
#   WORK=/tmp/x DCTWIN=./bin/dctwin TINKERBELL_BIN=/tmp/x/bin/tinkerbell compat/tinkerbell/run.sh
#
# KUBEBUILDER_ASSETS  directory holding etcd and kube-apiserver (required)
# TINKERBELL_BIN      a tinkerbell binary already built at $TINKERBELL_REF (skips the build,
#                     which is a large module graph: about 15 minutes cold)
# INTERVAL            Rufio's --rufio-power-check-interval (default 10s; Tinkerbell's own is 30m)
# IPMITOOL            ipmitool binary for bmclib's ipmitool provider (default: the one on PATH);
#                     without one the IPMI Machines are not run and the table says so
#
# Everything it starts (etcd, kube-apiserver, the twin, Rufio) listens on loopback ports chosen
# at run time, uses a kubeconfig under $WORK and is stopped on exit.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
TINKERBELL_REF=v0.25.0
TINKERBELL_COMMIT=63a025498de1d4b74e5a24c2fcf66a197ac1a960
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
KUBECTL=${KUBECTL:-kubectl}
INTERVAL=${INTERVAL:-10s}
: "${KUBEBUILDER_ASSETS:?set KUBEBUILDER_ASSETS to a directory with etcd and kube-apiserver (setup-envtest use -p path 1.37.0)}"
mkdir -p "$WORK/bin" "$WORK/k8s" "$WORK/logs"
WORK=$(cd "$WORK" && pwd)
DCTWIN=$(command -v "$DCTWIN")
KUBECTL=$(command -v "$KUBECTL")

pids=()
cleanup() {
  set +eu
  [ -n "${rufio_pid:-}" ] && kill "$rufio_pid" 2>/dev/null && wait "$rufio_pid" 2>/dev/null
  [ -n "${DCTWIN_STATE:-}" ] && [ -f "$DCTWIN_STATE/control.json" ] && "$DCTWIN" down >/dev/null 2>&1
  for ((i = ${#pids[@]} - 1; i >= 0; i--)); do kill "${pids[$i]}" 2>/dev/null; wait "${pids[$i]}" 2>/dev/null; done
  echo "stopped Rufio, the twin, kube-apiserver and etcd; artefacts in $WORK" >&2
}
trap cleanup EXIT
freeport() { python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }

# ---------------------------------------------------------------- Rufio at the tag
src="$WORK/src/tinkerbell"
if [ ! -d "$src/.git" ]; then
  git -c advice.detachedHead=false clone -q --depth 1 --branch "$TINKERBELL_REF" https://github.com/tinkerbell/tinkerbell "$src" 2>&1 \
    | grep -v 'is not a commit' >&2 || true   # an annotated tag; the commit is checked below
fi
got=$(git -C "$src" rev-parse HEAD)
[ "$got" = "$TINKERBELL_COMMIT" ] || { echo "tinkerbell $TINKERBELL_REF is $got, expected $TINKERBELL_COMMIT" >&2; exit 1; }
if [ -z "${TINKERBELL_BIN:-}" ]; then
  echo "building tinkerbell $TINKERBELL_REF (Rufio and bmclib) ..." >&2
  go -C "$src" build -o "$WORK/bin/tinkerbell" ./cmd/tinkerbell
  TINKERBELL_BIN="$WORK/bin/tinkerbell"
fi
go version -m "$TINKERBELL_BIN" | grep -E '^[[:space:]]+(mod|dep)[[:space:]]+github.com/(tinkerbell/tinkerbell|bmc-toolbox/bmclib/v2|stmcginnis/gofish)[[:space:]]' >&2 || true

# ---------------------------------------------------------------- kube-apiserver and etcd
k8s="$WORK/k8s"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=dctwin-compat-ca' -keyout "$k8s/ca.key" -out "$k8s/ca.crt" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj '/O=system:masters/CN=compat-admin' -keyout "$k8s/admin.key" -out "$k8s/admin.csr" 2>/dev/null
openssl x509 -req -in "$k8s/admin.csr" -CA "$k8s/ca.crt" -CAkey "$k8s/ca.key" -CAcreateserial -days 1 -out "$k8s/admin.crt" 2>/dev/null
openssl genrsa -out "$k8s/sa.key" 2048 2>/dev/null
openssl rsa -in "$k8s/sa.key" -pubout -out "$k8s/sa.pub" 2>/dev/null
etcd_port=$(freeport); peer_port=$(freeport); api_port=$(freeport)
rm -rf "$k8s/etcd-data"
"$KUBEBUILDER_ASSETS/etcd" --data-dir "$k8s/etcd-data" --unsafe-no-fsync \
  --listen-client-urls "http://127.0.0.1:$etcd_port" --advertise-client-urls "http://127.0.0.1:$etcd_port" \
  --listen-peer-urls "http://127.0.0.1:$peer_port" --initial-advertise-peer-urls "http://127.0.0.1:$peer_port" \
  --initial-cluster "default=http://127.0.0.1:$peer_port" > "$WORK/logs/etcd.log" 2>&1 &
pids+=($!)
"$KUBEBUILDER_ASSETS/kube-apiserver" --etcd-servers "http://127.0.0.1:$etcd_port" \
  --bind-address 127.0.0.1 --advertise-address 127.0.0.1 --endpoint-reconciler-type none --secure-port "$api_port" \
  --cert-dir "$k8s/serving" --client-ca-file "$k8s/ca.crt" --authorization-mode RBAC \
  --service-account-key-file "$k8s/sa.pub" --service-account-signing-key-file "$k8s/sa.key" \
  --service-account-issuer https://kubernetes.default.svc --service-cluster-ip-range 10.0.0.0/24 > "$WORK/logs/kube-apiserver.log" 2>&1 &
pids+=($!)
kubeconfig="$k8s/kubeconfig"
cat > "$kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: compat
  cluster: {server: "https://127.0.0.1:$api_port", insecure-skip-tls-verify: true}
users:
- name: compat-admin
  user: {client-certificate: "$k8s/admin.crt", client-key: "$k8s/admin.key"}
contexts:
- name: compat
  context: {cluster: compat, user: compat-admin, namespace: default}
current-context: compat
EOF
k() { "$KUBECTL" --kubeconfig "$kubeconfig" "$@"; }
for _ in $(seq 1 120); do k get --raw /readyz >/dev/null 2>&1 && break; sleep 0.5; done
k get --raw /readyz >/dev/null || { tail -20 "$WORK/logs/kube-apiserver.log"; exit 1; }
echo "kube-apiserver $(k version -o json | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["serverVersion"]["gitVersion"])') on 127.0.0.1:$api_port" >&2

# Rufio's three CRDs, from the source tree at the tag (the v1alpha1 set its controller serves)
crds="$src/crd/bases/v1alpha1"
k apply --server-side -f "$crds/bmc.tinkerbell.org_machines.yaml" -f "$crds/bmc.tinkerbell.org_jobs.yaml" -f "$crds/bmc.tinkerbell.org_tasks.yaml" >&2
k wait --for condition=Established --timeout 60s crd/machines.bmc.tinkerbell.org crd/jobs.bmc.tinkerbell.org crd/tasks.bmc.tinkerbell.org >&2

# ---------------------------------------------------------------- the twin
export DCTWIN_STATE="$WORK/state"
rm -rf "$DCTWIN_STATE"
prev_current=$(cat "$HOME/.dc-twin/current" 2>/dev/null || true)
"$DCTWIN" up "$root/examples/estates/lab-model.yaml" --state "$DCTWIN_STATE" --adapters redfish,ipmi > "$WORK/logs/dctwin-up.log" 2>&1 &
pids+=($!)
for _ in $(seq 1 120); do [ -f "$DCTWIN_STATE/control.json" ] && break; sleep 0.25; done
[ -f "$DCTWIN_STATE/control.json" ] || { cat "$WORK/logs/dctwin-up.log"; exit 1; }
# `dctwin up` records itself as the operator's current estate; this run addresses its estate by
# DCTWIN_STATE, so the operator's own pointer is put back
if [ -n "$prev_current" ]; then printf '%s' "$prev_current" > "$HOME/.dc-twin/current"; fi

# ---------------------------------------------------------------- Rufio
protocols=redfish,ipmi
IPMITOOL=${IPMITOOL:-$(command -v ipmitool || true)}
if [ -z "$IPMITOOL" ]; then
  echo "no ipmitool: bmclib's ipmitool provider cannot run, so the IPMI Machines are not run" >&2
  protocols=redfish
fi
rufio_path=/usr/bin:/bin
[ -n "$IPMITOOL" ] && rufio_path="$(dirname "$IPMITOOL"):$rufio_path"
http_port=$(freeport); https_port=$(freeport)
env -i HOME="$WORK" PATH="$rufio_path" KUBECONFIG="$kubeconfig" "$TINKERBELL_BIN" \
  --backend kube --backend-kube-config "$kubeconfig" \
  --enable-smee=false --enable-tootles=false --enable-tink-server=false --enable-tink-controller=false \
  --enable-secondstar=false --enable-ui=false --enable-rufio-controller=true --enable-crd-migrations=false \
  --bind-address 127.0.0.1 --http-port "$http_port" --https-port "$https_port" \
  --rufio-power-check-interval "$INTERVAL" --rufio-max-concurrent-reconciles 8 > "$WORK/logs/rufio.log" 2>&1 &
rufio_pid=$!
for _ in $(seq 1 60); do grep -q '"Starting workers".*"controller":"machine"' "$WORK/logs/rufio.log" 2>/dev/null && break; sleep 0.5; done
grep -q '"Starting workers"' "$WORK/logs/rufio.log" || { tail -20 "$WORK/logs/rufio.log"; exit 1; }
echo "Rufio running (pid $rufio_pid), power check every $INTERVAL" >&2

# ---------------------------------------------------------------- the flow
set +e
python3 -I "$here/flow.py" --kubeconfig "$kubeconfig" --kubectl "$KUBECTL" --dctwin "$DCTWIN" \
  --work "$WORK" --protocols "$protocols" | tee "$WORK/flow.txt"
status=${PIPESTATUS[0]}
exit "$status"

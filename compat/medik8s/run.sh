#!/usr/bin/env bash
# Runs medik8s' fleet-health and remediation pair against the twin, unmodified and built from
# released tags: Node Healthcheck Operator (NHC) watches the twin's Nodes, and Fence Agents
# Remediation (FAR) fences an unhealthy one through its BMC with the ClusterLabs fence agents
# (fence_ipmilan over IPMI-over-LAN, fence_redfish over Redfish), built from their source tag.
# The cluster is a real kube-apiserver and etcd (envtest's binaries) with the real
# kube-controller-manager; the Nodes are dc-twin's kwok projection (IMPERSONATED, D24), and the
# BMCs the agents talk to are the twin's ipmi and redfish adapters.
#
# NHC runs as a process. FAR runs in a container built here, as FAR ships (its manager and the
# fence agents in one image; the agent is a child process of the manager): a dctwin-medik8s-far-*
# container that reaches the apiserver and the BMCs on this machine's loopback (host.docker.internal
# or, under rootless Podman, host.containers.internal). Both serve their admission webhooks.
#
#   compat/medik8s/run.sh                    # PASS/FAIL table on stdout, artefacts under $WORK
#   WORK=/tmp/medik8s DCTWIN=bin/dctwin compat/medik8s/run.sh
#   KCM=/path/kube-controller-manager compat/medik8s/run.sh
#   BUILD_ONLY=1 compat/medik8s/run.sh        # clone and build, then stop
#   HEALTHY_DELAY=0s TIME_SCALE=1 ...          # the NHCs' healthyDelay; the estate's timeScale
#   KEEP=1 compat/medik8s/run.sh             # leave everything running; prints how to stop it
#
# Needs Go, git, python3, envtest's binaries and a container engine whose containers can reach the
# host's loopback: Docker Desktop or colima (host.docker.internal), or rootless Podman (pasta's
# --map-host-loopback, host.containers.internal). The image
# (dctwin-medik8s-far:<tags>) is kept for the next run; REMOVE_IMAGE=1 removes it on exit.
# Nothing here reads ~/.kube/config: every client is given the scratch cluster's kubeconfig.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
NHC_REF=${NHC_REF:-v0.12.0}           # github.com/medik8s/node-healthcheck-operator
FAR_REF=${FAR_REF:-v0.8.0}            # github.com/medik8s/fence-agents-remediation
FA_REF=${FA_REF:-v4.17.0}             # github.com/ClusterLabs/fence-agents
K8S_REF=${K8S_REF:-v1.37.0}           # kube-controller-manager, at the envtest apiserver's version
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
KCM=${KCM:-build}                     # build | <path to kube-controller-manager>
DOCKER=${DOCKER:-docker}              # docker, or podman (rootless) under either name
ADAPTERS=${ADAPTERS:-redfish,ipmi,kwok}
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
"$DOCKER" info >/dev/null 2>&1 || { echo "no container engine: FAR runs in a container (see the header)" >&2; exit 1; }
# How the FAR container reaches this machine's loopback (the apiserver, the twin's BMCs):
# Docker Desktop and colima resolve host.docker.internal to it; rootless Podman maps it with
# pasta's --map-host-loopback and names it host.containers.internal.
if "$DOCKER" --version 2>/dev/null | grep -qi podman; then
	ARCH=$("$DOCKER" info --format '{{.Host.Arch}}')
	NET_ARGS=(--network=pasta:--map-host-loopback,169.254.1.2)
	HOST_FROM_CONTAINER=${HOST_FROM_CONTAINER:-host.containers.internal}
else
	ARCH=$("$DOCKER" version --format '{{.Server.Arch}}')
	NET_ARGS=()
	HOST_FROM_CONTAINER=${HOST_FROM_CONTAINER:-host.docker.internal}
fi
# NHC serves its webhook on :9443, a port controller-runtime fixes for it; it must be free here.
python3 -I -c 'import socket; s=socket.socket(); s.bind(("", 9443))' 2>/dev/null || { echo "port 9443 is in use: NHC's webhook server binds it (cmd/main.go)" >&2; exit 1; }

# ---------------------------------------------------------------- build from source

clone() { # clone <repo> <ref> <dir>
	[ -d "$3/.git" ] || git -c advice.detachedHead=false clone -q --depth 1 --branch "$2" "https://github.com/$1.git" "$3" 2>&1 | grep -v 'is not a commit' >&2 || true
}
clone medik8s/node-healthcheck-operator "$NHC_REF" "$WORK/src/nhc"
clone medik8s/fence-agents-remediation "$FAR_REF" "$WORK/src/far"
clone ClusterLabs/fence-agents "$FA_REF" "$WORK/src/fence-agents"
NHC_COMMIT=$(git -C "$WORK/src/nhc" rev-parse HEAD)
FAR_COMMIT=$(git -C "$WORK/src/far" rev-parse HEAD)
FA_COMMIT=$(git -C "$WORK/src/fence-agents" rev-parse HEAD)
if [ ! -x "$WORK/bin/nhc-manager" ]; then
	echo "building NHC $NHC_REF" >&2
	(cd "$WORK/src/nhc" && go build -o "$WORK/bin/nhc-manager" ./cmd)
fi
if [ ! -x "$WORK/bin/far-manager-linux-$ARCH" ]; then
	echo "building FAR $FAR_REF for linux/$ARCH" >&2
	(cd "$WORK/src/far" && GOOS=linux GOARCH="$ARCH" CGO_ENABLED=0 go build -o "$WORK/bin/far-manager-linux-$ARCH" ./cmd)
fi
if [ "$KCM" = build ]; then
	if [ ! -x "$WORK/bin/kube-controller-manager" ]; then
		clone kubernetes/kubernetes "$K8S_REF" "$WORK/src/kubernetes"
		echo "building kube-controller-manager $K8S_REF (once; about ten minutes)" >&2
		(cd "$WORK/src/kubernetes" && go build -o "$WORK/bin/kube-controller-manager" ./cmd/kube-controller-manager)
	fi
	KCM="$WORK/bin/kube-controller-manager"
fi

# The FAR image: debian:bookworm-slim, the fence agents built from the ClusterLabs tag with their
# own autotools build, and FAR's manager (far.Containerfile). Native architecture only.
IMAGE="dctwin-medik8s-far:${FAR_REF}-fa${FA_REF#v}-${ARCH}"
if ! "$DOCKER" image inspect "$IMAGE" >/dev/null 2>&1; then
	ctx="$WORK/far-image"
	rm -rf "$ctx" && mkdir -p "$ctx"
	git -C "$WORK/src/fence-agents" archive --format=tar --prefix=fence-agents/ HEAD | tar -x -C "$ctx"
	echo "${FA_REF#v}" >"$ctx/fence-agents/.tarball-version"
	cp "$WORK/bin/far-manager-linux-$ARCH" "$ctx/manager"
	cp "$here/far.Containerfile" "$ctx/Containerfile"
	echo "building $IMAGE (the fence agents from source; a few minutes once)" >&2
	"$DOCKER" build --platform "linux/$ARCH" -t "$IMAGE" -f "$ctx/Containerfile" "$ctx" >"$WORK/far-image-build.log" 2>&1 \
		|| { tail -30 "$WORK/far-image-build.log" >&2; exit 1; }
fi
[ "${BUILD_ONLY:-}" = 1 ] && { echo "BUILD_ONLY=1: sources and builds in $WORK" >&2; exit 0; }

# ---------------------------------------------------------------- bring-up

PIDS=""
FAR_CONTAINER="dctwin-medik8s-far-$$"
cleanup() {
	set +e
	if [ "${KEEP:-}" = 1 ]; then
		echo "KEEP=1: left running. Stop with: kill $PIDS; $DOCKER rm -f $FAR_CONTAINER; HOME=$TWIN_HOME $DCTWIN down --state $TWIN_STATE; kill $CLUSTER_PIDS" >&2
		return
	fi
	"$DOCKER" logs "$FAR_CONTAINER" >"$WORK/medik8s/far.log" 2>&1
	"$DOCKER" rm -f "$FAR_CONTAINER" >/dev/null 2>&1
	[ "${REMOVE_IMAGE:-}" = 1 ] && "$DOCKER" rmi "$IMAGE" >/dev/null 2>&1
	local p
	for p in $PIDS; do kill "$p" 2>/dev/null; done
	for p in $PIDS; do wait "$p" 2>/dev/null; done
	twin_down
	cluster_down
}
trap cleanup EXIT
trap 'exit 130' INT TERM

rm -rf "$WORK/cluster" "$WORK/twin" "$WORK/medik8s"
M="$WORK/medik8s"
mkdir -p "$M/far" "$M/nhc-tmp/k8s-webhook-server/serving-certs"
cluster_up "$WORK/cluster" "$KCM"
echo "kube-apiserver $APISERVER_URL (envtest $(basename "$ENVTEST_DIR")); kube-controller-manager $K8S_REF" >&2

kc apply -f "$here/manifests/00-namespaces.yaml" >/dev/null
kc apply --server-side -f "$WORK/src/nhc/config/crd/bases/" -f "$WORK/src/far/config/crd/bases/" >/dev/null
crds_established() { [ "$(kc get crd -o 'jsonpath={range .items[*]}{.status.conditions[?(@.type=="Established")].status}{"\n"}{end}' | grep -c True)" -ge 3 ]; }
wait_until 60 crds_established >/dev/null || { echo "NHC and FAR CRDs not established" >&2; exit 1; }

# Webhooks. Each project's config/webhook/manifests.yaml as its kustomization lays it out
# (namespace, name prefix), served through an ExternalName Service for localhost on the port each
# webhook server listens on (compat/kueue/lib/render.py), with a certificate made here for
# <service>.medik8s.svc and its CA put in caBundle, as OLM or cert-manager would.
webhook_cert() { # webhook_cert <service> <dir>: tls.crt, tls.key; the cert is its own CA
	mkdir -p "$2"
	openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=$1.medik8s.svc" \
		-addext "subjectAltName=DNS:$1.medik8s.svc,DNS:$1.medik8s,DNS:$1" -keyout "$2/tls.key" -out "$2/tls.crt" 2>/dev/null
	chmod 0644 "$2/tls.key" "$2/tls.crt"   # readable by the FAR image's non-root user; a scratch certificate
}
install_webhooks() { # install_webhooks <manifests.yaml> <prefix> <port> <cert dir>
	local svc="$2webhook-service"
	kc create service externalname "$svc" -n medik8s --external-name localhost >/dev/null
	sed -e "s|namespace: system|namespace: medik8s|" -e "s|name: webhook-service|name: $svc|" \
		-e "s|name: mutating-webhook-configuration|name: $2mutating-webhook-configuration|" \
		-e "s|name: validating-webhook-configuration|name: $2validating-webhook-configuration|" "$1" >"$M/$2webhooks-ns.yaml"
	python3 -I "$here/../kueue/lib/render.py" service-port "$svc" "$3" "$M/$2webhooks-ns.yaml" >"$M/$2webhooks.yaml"
	python3 -I "$here/../kueue/lib/render.py" ca-bundle "$4/tls.crt" "$M/$2webhooks.yaml" >"$M/$2webhooks-ca.yaml"
	kc apply -f "$M/$2webhooks-ca.yaml" >/dev/null
}
webhook_cert node-healthcheck-webhook-service "$M/nhc-tmp/k8s-webhook-server/serving-certs"
FAR_WEBHOOK_PORT=$(free_port)
webhook_cert fence-agents-remediation-webhook-service "$M/far/certs"

# NHC, a process here; DEPLOYMENT_NAMESPACE as its Deployment sets it. Its webhook server reads
# its certificate from $TMPDIR/k8s-webhook-server/serving-certs (controller-runtime's default).
(cd "$M" && DEPLOYMENT_NAMESPACE=medik8s TMPDIR="$M/nhc-tmp" HOME="$M" KUBECONFIG="$KUBECONFIG_FILE" exec "$WORK/bin/nhc-manager" \
	--leader-elect=false --metrics-bind-address=0 --health-probe-bind-address="127.0.0.1:$(free_port)" >"$M/nhc.log" 2>&1) &
PIDS="$PIDS $!"

# FAR, in its container: the scratch kubeconfig with the apiserver at $HOST_FROM_CONTAINER, its
# webhook certificate where controller-runtime looks, and the webhook port published on loopback.
sed -e "s|server: https://127.0.0.1:|server: https://$HOST_FROM_CONTAINER:|" "$KUBECONFIG_FILE" >"$M/far/kubeconfig"
chmod 0644 "$M/far/kubeconfig"
"$DOCKER" create --name "$FAR_CONTAINER" --label dc-twin.io/harness=compat-medik8s ${NET_ARGS[@]+"${NET_ARGS[@]}"} -p "127.0.0.1:$FAR_WEBHOOK_PORT:9443" \
	-e KUBECONFIG=/tmp/far/kubeconfig "$IMAGE" --leader-elect=false --metrics-bind-address=0 --health-probe-bind-address=:8081 >/dev/null
mkdir -p "$M/far/copy/far" "$M/far/copy/k8s-webhook-server/serving-certs"
cp "$M/far/kubeconfig" "$M/far/copy/far/"
cp "$M/far/certs/tls.crt" "$M/far/certs/tls.key" "$M/far/copy/k8s-webhook-server/serving-certs/"
chmod -R a+rX "$M/far/copy"
"$DOCKER" cp "$M/far/copy/." "$FAR_CONTAINER:/tmp/"
"$DOCKER" start "$FAR_CONTAINER" >/dev/null

install_webhooks "$WORK/src/nhc/config/webhook/manifests.yaml" node-healthcheck- 9443 "$M/nhc-tmp/k8s-webhook-server/serving-certs"
install_webhooks "$WORK/src/far/config/webhook/manifests.yaml" fence-agents-remediation- "$FAR_WEBHOOK_PORT" "$M/far/certs"
nhc_started() { grep -q 'Starting workers.*nodehealthcheck' "$M/nhc.log"; }
far_started() { "$DOCKER" logs "$FAR_CONTAINER" 2>&1 | grep -q 'Starting workers.*FenceAgentsRemediation"'; }
wait_until 90 nhc_started >/dev/null || { tail -20 "$M/nhc.log" >&2; echo "NHC did not start" >&2; exit 1; }
wait_until 90 far_started >/dev/null || { "$DOCKER" logs "$FAR_CONTAINER" 2>&1 | tail -20 >&2; echo "FAR did not start" >&2; exit 1; }

# TIME_SCALE overrides the estate's timeScale (the lab estate runs its behaviours at 1/20).
estate_file=$ESTATE
if [ -n "${TIME_SCALE:-}" ]; then
	estate_file="$M/$(basename "$ESTATE")"
	sed -e "s|^\(  timeScale:\).*|\1 $TIME_SCALE|" "$ESTATE" >"$estate_file"
fi
TIME_SCALE=$(sed -n 's/^  timeScale: *//p' "$estate_file")
twin_up "$estate_file" "$WORK/twin" "$ADAPTERS" "$KUBECONFIG_FILE"
echo "twin up: $(grep -o 'control API: [^ ]*' "$WORK/twin/up.log"), state $TWIN_STATE" >&2

# ---------------------------------------------------------------- helpers

ready_of() { kc get node "$1" -o 'jsonpath={.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].reason}'; }
cond_of() { kc get node "$1" -o "jsonpath={.status.conditions[?(@.type==\"$2\")].status}"; }
taints_of() { kc get node "$1" -o 'jsonpath={range .spec.taints[*]}{.key}:{.effect} {end}'; }
has_taint() { taints_of "$1" | grep -q "$2"; }
path_of() { kc get node "$1" -o jsonpath='{.metadata.annotations.dc-twin\.io/device-path}'; }
# NHC names a remediation <node>-<random> and puts the node in an annotation, which FAR reads.
far_of() { kc get fenceagentsremediations -n medik8s -o jsonpath='{range .items[*]}{.metadata.annotations.remediation\.medik8s\.io/node-name} {.metadata.name}{"\n"}{end}' | awk -v n="$1" '$1 == n { print $2 }' | head -1; }
far_get() { local f; f=$(far_of "$1"); [ -n "$f" ] && kc get fenceagentsremediation -n medik8s "$f" "${@:2}" 2>/dev/null; }
far_cond() { far_get "$1" -o "jsonpath={.status.conditions[?(@.type==\"$2\")].status}"; }
far_reason() { far_get "$1" -o "jsonpath={.status.conditions[?(@.type==\"$2\")].reason}"; }
far_exists() { [ -n "$(far_of "$1")" ]; }
far_gone() { ! far_exists "$1"; }
far_fenced() { [ "$(far_cond "$1" FenceAgentActionSucceeded)" = True ]; }
far_succeeded() { [ "$(far_cond "$1" Succeeded)" = True ]; }
nhc_unhealthy() { kc get nodehealthcheck "$1" -o jsonpath='{range .status.unhealthyNodes[*]}{.name}{" "}{end}'; }
nhc_healthy() { kc get nodehealthcheck "$1" -o jsonpath='{.status.healthyNodes}/{.status.observedNodes} {.status.phase}'; }
seq_now() { twin status --json | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["seq"])'; }
twin_events() { twin events --select "$1" --since "$2" --json; }
events_summary() { twin_events "$1" "$2" | python3 -I "$here/lib/events.py" summary "$3"; }
events_have() { twin_events "$1" "$2" | python3 -I "$here/lib/events.py" has "$3" "$4"; }
bound_pod() { # bound_pod <name> <node>: a workload already on the Node, as if it had run there
	kc apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: { name: $1, namespace: dc-twin-medik8s, labels: { dc-twin.io/workload: $1 } }
spec:
  nodeName: $2
  terminationGracePeriodSeconds: 0
  containers: [{ name: main, image: registry.invalid/dc-twin/never-pulled:0 }]
EOF
}
pod_gone() { ! kc get pod -n dc-twin-medik8s "$1" >/dev/null 2>&1; }
since() { echo $(( ($(now_ms) - $1) / 1000 )); }
ok() { "$@" >/dev/null 2>&1 && echo 0 || echo 1; }

# ---------------------------------------------------------------- U: the twin's Nodes, the stack

hosts=50
node_count_is() { [ "$(kc get nodes -o name | grep -c .)" -eq "$1" ]; }
ms=$(wait_until 60 node_count_is $hosts) || true
ready=$(kc get nodes -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c True || true)
record U1 "Nodes the twin registers (kwok projection)" "$hosts Ready" "$ready Ready after ${ms} ms" "$( [ "$ready" = $hosts ] && echo PASS || echo FAIL)"
notready_taints() { kc get nodes -o jsonpath='{range .items[*]}{.spec.taints[*].key}{"\n"}{end}' | grep -c 'node.kubernetes.io/not-ready' || true; }
no_notready_taints() { [ "$(notready_taints)" = 0 ]; }
ms=$(wait_until 60 no_notready_taints) || true
record U2 "not-ready:NoSchedule taint lifted from Ready Nodes (node-lifecycle controller)" "0 Nodes keep it" "$(notready_taints) after ${ms} ms" "$( no_notready_taints && echo PASS || echo FAIL)"

fav=$("$DOCKER" exec "$FAR_CONTAINER" fence_ipmilan --version 2>&1 | tr -d '\r' | head -1)
frv=$("$DOCKER" exec "$FAR_CONTAINER" fence_redfish --version 2>&1 | tr -d '\r' | head -1)
running=$("$DOCKER" inspect -f '{{.State.Running}}' "$FAR_CONTAINER")
record U3 "NHC $NHC_REF ($NHC_COMMIT) as a process; FAR $FAR_REF ($FAR_COMMIT) in $IMAGE with fence-agents $FA_REF ($FA_COMMIT)" "NHC up; the FAR container running; fence_ipmilan and fence_redfish report ${FA_REF#v}" "NHC pid $(kill -0 ${PIDS##* } 2>/dev/null && echo alive || echo dead); container running=$running; fence_ipmilan $fav, fence_redfish $frv" "$( kill -0 ${PIDS##* } 2>/dev/null && [ "$running" = true ] && [ "$fav" = "${FA_REF#v}" ] && [ "$frv" = "${FA_REF#v}" ] && echo PASS || echo FAIL)"

# BMC endpoints and credentials, from the twin, as an operator would enter them per node.
twin endpoints >"$M/endpoints.json"
NODES="b01-server-01 b01-server-02 b05-server-01 b05-server-02 b02-server-01 b02-server-02"
bmc_port() { python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["endpoints"][sys.argv[2]][sys.argv[3]].rsplit(":",1)[1])' "$M/endpoints.json" "$(path_of "$1")" "$2"; }
for n in $NODES; do
	twin credentials "$(path_of "$n")" | python3 -I -c '
import json, sys
c = json.load(sys.stdin)[0]
print("--username=" + c["user"]); print("--password=" + c["password"])' >"$M/creds-$n"
	kc create secret generic "bmc-$n" -n medik8s --from-literal=--username="$(sed -n 's/^--username=//p' "$M/creds-$n")" \
		--from-literal=--password="$(sed -n 's/^--password=//p' "$M/creds-$n")" >/dev/null
	rm -f "$M/creds-$n"
done
template() { # template <name> <agent> <protocol> <strategy> <rack> [extra shared parameter lines]
	local n
	{
		cat <<EOF
apiVersion: fence-agents-remediation.medik8s.io/v1alpha1
kind: FenceAgentsRemediationTemplate
metadata: { name: $1, namespace: medik8s }
spec:
  template:
    spec:
      agent: $2
      retrycount: 3
      retryinterval: 5s
      timeout: 60s
      remediationStrategy: $4
      sharedparameters:
        --ip: $HOST_FROM_CONTAINER
${6:-}
      nodeparameters:
        --ipport:
EOF
		for n in $NODES; do case $n in "$5"-*) echo "          $n: \"$(bmc_port "$n" "$3")\"" ;; esac; done
		echo "      nodeSecretNames:"
		for n in $NODES; do case $n in "$5"-*) echo "        $n: bmc-$n" ;; esac; done
	} >"$WORK/evidence/template-$1.yaml"
	apply_retry "$WORK/evidence/template-$1.yaml"
}
# apply_retry <file>: kubectl apply, retried: on a loaded machine a webhook in the FAR container
# can miss kube-apiserver's 10 s webhook timeout; the output of the last attempt is printed.
apply_retry() {
	local i out
	for i in 1 2 3 4 5; do
		if out=$(kc apply -f "$1" 2>&1); then echo "$out"; return 0; fi
		echo "attempt $i: $out" >>"$M/apply-retries.log"
		sleep 3
	done
	echo "$out"
	return 1
}
t_ipmi=$(template fence-ipmilan fence_ipmilan ipmi ResourceDeletion b01 '        --lanplus: ""' 2>&1) || true
t_cycle=$(template fence-ipmilan-cycle fence_ipmilan ipmi ResourceDeletion b05 '        --lanplus: ""
        --method: cycle' 2>&1) || true
t_redfish=$(template fence-redfish fence_redfish redfish OutOfServiceTaint b02 '        --ssl-insecure: ""' 2>&1) || true
# FAR's validating webhook looks for the agent in /usr/sbin of its own image.
sed -e 's/name: fence-ipmilan$/name: no-such-agent/' -e 's/agent: fence_ipmilan/agent: fence_nosuchagent/' "$WORK/evidence/template-fence-ipmilan.yaml" >"$M/template-no-such-agent.yaml"
for i in 1 2 3 4 5; do   # retried only while the webhook call itself times out
	bad=$(kc apply -f "$M/template-no-such-agent.yaml" 2>&1 || true)
	echo "$bad" | grep -q 'context deadline exceeded' || break
	sleep 3
done
record U4 "FenceAgentsRemediationTemplates through FAR's admission webhook (in its container)" "fence_ipmilan (b01, onoff), fence_ipmilan --method=cycle (b05) and fence_redfish (b02) admitted; a template naming fence_nosuchagent refused (no such agent in the image)" "$(echo "$t_ipmi; $t_cycle; $t_redfish" | tr '\n' ' ' | cut -c1-200); bad: $(echo "$bad" | tr '\n' ' ' | grep -o 'unsupported fence agent[^"]*' | head -1 | cut -c1-80)" "$( kc get fenceagentsremediationtemplate -n medik8s fence-ipmilan fence-ipmilan-cycle fence-redfish >/dev/null 2>&1 && echo "$bad" | grep -q 'unsupported fence agent' && ! kc get fenceagentsremediationtemplate -n medik8s no-such-agent >/dev/null 2>&1 && echo PASS || echo FAIL)"

# HEALTHY_DELAY replaces the NodeHealthChecks' healthyDelay (30 s; README, Findings).
sed -e "s|healthyDelay: 30s|healthyDelay: ${HEALTHY_DELAY:-30s}|" "$here/manifests/02-nodehealthchecks.yaml" >"$M/nodehealthchecks.yaml"
nhcs=$(apply_retry "$M/nodehealthchecks.yaml" | tr '\n' ' ') || true
nhc_on() { [ "$(kc get nodehealthcheck -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' | grep -c Enabled)" = 3 ] && [ "$(kc get nodehealthcheck -o jsonpath='{range .items[*]}{.status.healthyNodes}{"\n"}{end}' | grep -c '^2$')" = 3 ]; }
ms=$(wait_until 60 nhc_on) || true
record U5 "NodeHealthChecks ipmi-onoff, ipmi-cycle, redfish through NHC's admission webhook" "admitted; each Enabled with 2 of 2 Nodes healthy" "$(kc get nodehealthcheck -o jsonpath='{range .items[*]}{.metadata.name}: {.status.phase} {.status.healthyNodes}/{.status.observedNodes}; {end}') after ${ms} ms" "$(pass_if "$(ok nhc_on)")"

# F: the agents against the twin's BMCs directly, from the FAR container
agent_status() { # agent_status <agent> <node> <protocol> [extra args]: the agent's -o status, credentials on stdin
	local a=$1 n=$2 p=$3
	shift 3
	{ twin credentials "$(path_of "$n")" | python3 -I -c '
import json, sys
c = json.load(sys.stdin)[0]
print("username=" + c["user"]); print("password=" + c["password"])'
		printf 'ip=%s\nipport=%s\naction=status\n' "$HOST_FROM_CONTAINER" "$(bmc_port "$n" "$p")"
		for x in "$@"; do echo "$x"; done
	} | "$DOCKER" exec -i "$FAR_CONTAINER" "$a" 2>&1 | tr -d '\r' | tail -1
}
s1=$(agent_status fence_ipmilan b01-server-01 ipmi lanplus=1)
s2=$(agent_status fence_redfish b02-server-01 redfish ssl_insecure=1)
record F1 "fence_ipmilan and fence_redfish -o status, run in the FAR container against the twin's BMCs" "Status: ON from b01-server-01 over IPMI and from b02-server-01 over Redfish" "fence_ipmilan: $s1; fence_redfish: $s2" "$( [ "$s1" = "Status: ON" ] && [ "$s2" = "Status: ON" ] && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- M: remediation through the BMCs

# remediate <id> <nhc> <node> <protocol> <expected requests> <how the twin breaks it> [check after fence]
# Watches NHC create the FenceAgentsRemediation, FAR fence the host through its BMC, the twin log
# the power requests over that protocol, the Node come back, and NHC end the remediation.
remediate_watch() {
	local id=$1 nhc=$2 node=$3 proto=$4 want=$5 seq0=$6 t0=$7
	local p ms_cr ms_fence ms_ready ms_done
	p=$(path_of "$node")
	far_created() { far_exists "$node"; }
	far_tainted() { has_taint "$node" remediation.medik8s.io/fence-agents-remediation; }
	ms_cr=$(wait_until 120 far_created) || true
	record "${id}a" "NHC $nhc: $node unhealthy for 20 s" "NHC creates a FenceAgentsRemediation for $node from its template" "$(far_exists "$node" && echo "$(far_of "$node") created" || echo absent) $(since "$t0") s after the twin broke the host; NHC unhealthy: $(nhc_unhealthy "$nhc")" "$(pass_if "$(ok far_exists "$node")")"
	wait_until 30 far_tainted >/dev/null || true
	tainted=$(taints_of "$node")
	ms_fence=$(wait_until 180 far_fenced "$node") || true
	fenced_at=$(since "$t0")
	wait_until 60 events_have "$p" "$seq0" "$proto" "$want" >/dev/null || true
	record "${id}b" "FAR runs the fence agent against $node's BMC ($proto)" "FenceAgentActionSucceeded=True; the twin logs $want accepted over $proto; the Node carries FAR's remediation taint meanwhile" "FenceAgentActionSucceeded=$(far_cond "$node" FenceAgentActionSucceeded) ($(far_reason "$node" FenceAgentActionSucceeded)) $fenced_at s after the break; twin: $(events_summary "$p" "$seq0" "$proto"); taints while remediating: [$tainted]" "$( far_fenced "$node" && events_have "$p" "$seq0" "$proto" "$want" && echo "$tainted" | grep -q 'remediation.medik8s.io/fence-agents-remediation:' && echo PASS || echo FAIL)"
}
remediate_finish() { # <id> <nhc> <node> <t0>
	local id=$1 nhc=$2 node=$3 t0=$4
	node_ready() { [ "$(ready_of "$node")" = True/KubeletReady ]; }
	done_() { far_gone "$node" && ! has_taint "$node" remediation.medik8s.io && [ -z "$(nhc_unhealthy "$nhc")" ]; }
	wait_until 120 node_ready >/dev/null || true
	ready_at=$(since "$t0")
	wait_until 120 done_ >/dev/null || true
	record "${id}d" "$node back; remediation ends" "Node Ready (KubeletReady); NHC deletes the FenceAgentsRemediation, FAR removes its taints and finalizer, NHC reports no unhealthy Node" "Ready=$(ready_of "$node") $ready_at s after the break; FAR CR $(far_exists "$node" && echo present || echo deleted), taints [$(taints_of "$node")], NHC $nhc $(nhc_healthy "$nhc") unhealthy [$(nhc_unhealthy "$nhc")] at $(since "$t0") s" "$( node_ready && done_ && echo PASS || echo FAIL)"
}

# M1: IPMI. A host that dies (power lost to it, out of band) with a workload on it.
N1=b01-server-02
bound_pod job-on-b01-02 $N1
seq0=$(seq_now); t0=$(now_ms)
twin power off "$(path_of $N1)" >/dev/null
remediate_watch M1 ipmi-onoff $N1 ipmi power.on "$seq0" "$t0"
wait_until 30 pod_gone job-on-b01-02 >/dev/null || true
record M1c "FAR's ResourceDeletion strategy" "the workload bound to $N1 is deleted once the host is fenced; FenceAgentsRemediation Succeeded=True" "pod job-on-b01-02 $(pod_gone job-on-b01-02 && echo deleted || echo present); Succeeded=$(far_cond $N1 Succeeded) ($(far_reason $N1 Succeeded))" "$( pod_gone job-on-b01-02 && { far_succeeded $N1 || far_gone $N1; } && echo PASS || echo FAIL)"
remediate_finish M1 ipmi-onoff $N1 "$t0"
twin_events "$(path_of $N1)" "$seq0" >"$WORK/evidence/M1-twin-events.json"

# M2: Redfish, out-of-service taint strategy.
N2=b02-server-02
bound_pod job-on-b02-02 $N2
seq0=$(seq_now); t0=$(now_ms)
twin power off "$(path_of $N2)" >/dev/null
remediate_watch M2 redfish $N2 redfish power.on "$seq0" "$t0"
oos() { has_taint $N2 node.kubernetes.io/out-of-service; }
wait_until 30 oos >/dev/null || true
oos_t=$(taints_of $N2)
wait_until 60 pod_gone job-on-b02-02 >/dev/null || true
record M2c "FAR's OutOfServiceTaint strategy" "node.kubernetes.io/out-of-service:NoExecute on $N2 after fencing; kube-controller-manager's pod GC force-deletes the workload bound to it" "taints [$oos_t]; pod job-on-b02-02 $(pod_gone job-on-b02-02 && echo deleted || echo present); Succeeded=$(far_cond $N2 Succeeded)" "$( echo "$oos_t" | grep -q 'out-of-service:NoExecute' && pod_gone job-on-b02-02 && echo PASS || echo FAIL)"
remediate_finish M2 redfish $N2 "$t0"
twin_events "$(path_of $N2)" "$seq0" >"$WORK/evidence/M2-twin-events.json"

# M3: IPMI on a running host whose GPU failed: Xid 48 (double-bit ECC), whose catalogue entry
# clears on device.reset. fence_ipmilan's reboot is power off then power on.
N3=b01-server-01
P3=$(path_of $N3)
seq0=$(seq_now); t0=$(now_ms)
twin fault inject "$P3/gpu-0" nvidia/xid/xid-48 >/dev/null
remediate_watch M3 ipmi-onoff $N3 ipmi power.off.force,power.on "$seq0" "$t0"
node_ready3() { [ "$(ready_of $N3)" = True/KubeletReady ]; }
wait_until 120 node_ready3 >/dev/null || true
sleep 25   # longer than NHC's 20 s: a GPU still failed is unhealthy again
faults3=$(twin get "$P3" --json 2>/dev/null | python3 -I -c 'import json,sys; v=json.load(sys.stdin); v=v[0] if isinstance(v,list) else v; print(",".join(f["fault"] + " on " + f["target"].rsplit("/",1)[-1] for f in (v.get("faults") or [])) or "none")' 2>/dev/null || echo "?")
if [ "$(cond_of $N3 GpuHealthy)" = True ]; then r=PASS; else r=FINDING; fi
record M3c "After FAR's off/on through IPMI: is the Xid 48 GPU reset?" "a host power cycle resets its GPUs: GpuHealthy=True and NHC ends the remediation" "Ready=$(ready_of $N3), GpuHealthy=$(cond_of $N3 GpuHealthy), faults: $faults3; FAR CR $(far_exists $N3 && echo "present (Succeeded=$(far_cond $N3 Succeeded))" || echo deleted); NHC unhealthy [$(nhc_unhealthy ipmi-onoff)]" "$r"
twin_events "$P3" "$seq0" >"$WORK/evidence/M3-twin-events.json"
twin fault clear "$P3/gpu-0" nvidia/xid/xid-48 >/dev/null 2>&1 || true
remediate_finish M3 ipmi-onoff $N3 "$t0"

# M4: the same fault on b05, whose template fences with --method=cycle (IPMI chassis power cycle).
N4=b05-server-01
P4=$(path_of $N4)
seq0=$(seq_now); t0=$(now_ms)
twin fault inject "$P4/gpu-0" nvidia/xid/xid-48 >/dev/null
remediate_watch M4 ipmi-cycle $N4 ipmi power.cycle "$seq0" "$t0"
gpu_ok4() { [ "$(cond_of $N4 GpuHealthy)" = True ]; }
wait_until 120 gpu_ok4 >/dev/null || true
record M4c "After FAR's chassis power cycle through IPMI" "the twin fires device.reset; Xid 48 clears; GpuHealthy=True" "GpuHealthy=$(cond_of $N4 GpuHealthy); twin: $(events_summary "$P4" "$seq0" ipmi)" "$(pass_if "$(ok gpu_ok4)")"
remediate_finish M4 ipmi-cycle $N4 "$t0"
twin_events "$P4" "$seq0" >"$WORK/evidence/M4-twin-events.json"

# M5: a host that dies with its BMC: powered off, then the BMC unreachable. Fencing cannot work;
# FAR must say so, and NHC must hold the remediation for the whole outage (one episode, as a real
# Node's Ready stays Unknown), until the BMC returns and the host is powered on.
N5=b05-server-02
P5=$(path_of $N5)
seq0=$(seq_now); t0=$(now_ms)
twin power off "$P5" >/dev/null
twin fault inject "$P5" generic/bmc/unreachable >/dev/null
far_created5() { far_exists $N5; }
wait_until 60 far_created5 >/dev/null || true
cr5=$(far_of $N5)
record M5a "NHC ipmi-cycle: $N5 off, its BMC unreachable" "NHC creates a FenceAgentsRemediation for $N5" "${cr5:-none} $(since "$t0") s after the break" "$( [ -n "$cr5" ] && echo PASS || echo FAIL)"
# Watch the episode: FAR's agent fails against a silent BMC (3 tries, 60 s timeout each at most);
# meanwhile the remediation must neither be deleted nor replaced.
failed5() { [ "$(far_cond $N5 FenceAgentActionSucceeded)" = False ]; }
# NHC marks a remediation it is about to end (the node looked healthy) with its healthy-delay
# annotation; a real outage never shows one.
held5() {
	[ "$(far_of $N5)" = "$cr5" ] && nhc_unhealthy ipmi-cycle | grep -qw $N5 &&
		[ -z "$(far_get $N5 -o 'jsonpath={.metadata.annotations.remediation\.medik8s\.io/healthy-delay}')" ]
}
breaks=0 samples=0 deadline=$(( $(now_ms) + 240000 ))
while [ "$(now_ms)" -lt "$deadline" ]; do
	samples=$((samples + 1))
	held5 || breaks=$((breaks + 1))
	failed5 && break
	sleep 3
done
fail_at=$(since "$t0")
# past the controller manager's 50 s grace and NHC's 20 s duration again: a "healthy" verdict in
# the middle of the outage would show here
for i in $(seq 1 15); do samples=$((samples + 1)); held5 || breaks=$((breaks + 1)); sleep 3; done
record M5b "FAR's fence_ipmilan against an unreachable BMC" "FenceAgentActionSucceeded=False (FenceAgentFailed); the twin logs no power request from the stack" "FenceAgentActionSucceeded=$(far_cond $N5 FenceAgentActionSucceeded) ($(far_reason $N5 FenceAgentActionSucceeded)) $fail_at s after the break; twin: $(events_summary "$P5" "$seq0" ipmi)" "$( failed5 && ! events_have "$P5" "$seq0" ipmi power.on && echo PASS || echo FAIL)"
record M5c "NHC across the outage" "one remediation for the whole outage: at every sample the same FenceAgentsRemediation, $N5 unhealthy, no healthy-delay annotation" "$((samples - breaks)) of $samples samples ($(since "$t0") s) held; remediation now $(far_of $N5)" "$( [ "$breaks" = 0 ] && echo PASS || echo FAIL)"
twin fault clear "$P5" generic/bmc/unreachable >/dev/null
twin power on "$P5" >/dev/null
remediate_finish M5 ipmi-cycle $N5 "$t0"
twin_events "$P5" "$seq0" >"$WORK/evidence/M5-twin-events.json"

kc get nodehealthchecks -o yaml >"$WORK/evidence/nodehealthchecks-final.yaml"
kc get events -A --sort-by=.lastTimestamp >"$WORK/evidence/events.txt" 2>&1 || true
twin report >"$WORK/evidence/fidelity-report.txt" 2>&1 || true

status=0
print_table "medik8s against dc-twin ($(basename "$ESTATE") at timeScale $TIME_SCALE, NHC healthyDelay ${HEALTHY_DELAY:-30s}): NHC $NHC_REF, FAR $FAR_REF, fence-agents $FA_REF, kube-controller-manager $K8S_REF. Nodes are the kwok projection: IMPERSONATED (D24); the BMCs are the twin's ipmi and redfish adapters." || status=$?
echo
echo "artefacts: $WORK/evidence (templates, twin events per remediation), logs: $M (nhc.log, far.log), $WORK/cluster/logs, $WORK/twin/up.log"
exit $status

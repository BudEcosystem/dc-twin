#!/usr/bin/env bash
# Runs NVIDIA NVSentinel, unmodified and built from a released tag, against a GPU server the twin
# runs as a real virtual machine (compat/nvsentinel/estate.yaml: a Dell XE9680 at `vm` fidelity,
# `os: twin`). An Xid 79 injected into the twin is printk'd into the guest's kernel by the in-host
# agent's kernel module (dctwin-kmsg, D50), read from the node's journal by NVSentinel's
# syslog-health-monitor, and followed by NVSentinel through quarantine, drain and a reboot request,
# which a stand-in for the site's lifecycle manager carries out through the server's Redfish BMC;
# the twin observes the reboot, the guest comes back with a new boot ID, and NVSentinel completes
# the remediation.
#
# With BASE (a Debian 13 genericcloud qcow2) the node has GPUs as well: a second guest
# (lib/gpu.sh) in which NVIDIA's Mokka presents the server's GPUs, as the twin's model holds them,
# to NVML, and NVSentinel's gpu-health-monitor watches them through an in-process DCGM host
# engine. The same Xid 79 reaches it twice, as a driver's would: NVML loses the GPU (dctwin-mokka
# sync), and the kernel line is printk'd into that guest's kernel too (kmsg-relay.py, through the
# same kernel module), where DCGM's kmsg reader finds it. Since the GPU stays off the bus until an
# AC cycle, the run then shows what the monitors make of a warm reboot that cannot fix it, cycles
# the server's power, and watches the quarantine lift.
#
#   compat/nvsentinel/run.sh                 # PASS/FAIL table on stdout, artefacts under $WORK
#   BASE=/path/debian-13-genericcloud-amd64.qcow2 compat/nvsentinel/run.sh   # with the GPU guest
#   WORK=/path DCTWIN=bin/dctwin KCM=/path/kube-controller-manager compat/nvsentinel/run.sh
#   KEEP=1 compat/nvsentinel/run.sh          # leave everything running; prints how to stop it
#   BUILD_ONLY=1 compat/nvsentinel/run.sh    # build NVSentinel, its images, the module, helm (and
#                                            # with BASE the prepared GPU guest), then stop
#   KMOD=0 compat/nvsentinel/run.sh          # without the kernel module: the agent writes through
#                                            # /dev/kmsg (facility user) and marks the journal entries
#
# Needs a Linux host with KVM for a guest of its own architecture, Go, git, python3,
# openssl, curl, a Docker engine or rootless Podman (as `docker`), envtest's binaries and the
# twin's guest image (fetched into $WORK if missing); with BASE, x86_64, qemu-system-x86_64 and
# qemu-img, and 3 GiB more memory for the GPU guest. NVSentinel's modules run in containers
# named dctwin-nvs-* on this host's network, each on a free port; they, their volumes and
# PostgreSQL's container are removed at the end. Nothing here reads ~/.kube/config: every client
# is given the scratch cluster's kubeconfig.
set -euo pipefail
{ # one compound command, read whole before any of it runs: a sync of the repository while the
# guests run cannot change what this process does next
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
NVS_REF=${NVS_REF:-v1.27.0}          # github.com/NVIDIA/NVSentinel
K8S_REF=${K8S_REF:-v1.37.0}
HELM_VERSION=${HELM_VERSION:-v4.2.4}
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
KCM=${KCM:-build}                    # build | none | <path to kube-controller-manager>
ESTATE=${ESTATE:-$here/estate.yaml}
HOST=${HOST:-b01/server-01}          # the estate's `vm` GPU server (os: twin)
GPU=${GPU:-gpu-2}                    # Xid 79
GPU48=${GPU48:-gpu-5}                # Xid 48
ADAPTERS=${ADAPTERS:-redfish,ipmi,dhcp,vm,inhost}
BASE=${BASE:-}
KMOD=${KMOD:-1}
mkdir -p "$WORK/bin" "$WORK/src" "$WORK/evidence" "$WORK/dl"
WORK=$(cd "$WORK" && pwd)
EV=$WORK/evidence
rm -f "$EV"/* 2>/dev/null || true

. "$root/compat/kai/lib/common.sh"
. "$root/compat/kai/lib/cluster.sh" # kc, cluster_down
. "$root/compat/kai/lib/twin.sh"
. "$here/lib/cluster.sh"
. "$here/lib/postgres.sh"
. "$here/lib/build.sh"
. "$here/lib/gpu.sh"

ENVTEST_DIR=$(envtest_dir)
[ -x "$ENVTEST_DIR/kube-apiserver" ] || { echo "no envtest binaries: set KUBEBUILDER_ASSETS (setup-envtest use -p path)" >&2; exit 1; }
KUBECTL=${KUBECTL:-$ENVTEST_DIR/kubectl}
export ENVTEST_DIR KUBECTL
command -v "$DCTWIN" >/dev/null || { echo "dctwin not found: set DCTWIN (make build; DCTWIN=bin/dctwin)" >&2; exit 1; }
DCTWIN=$(command -v "$DCTWIN")
for t in docker openssl python3 curl git go; do command -v $t >/dev/null || { echo "$t is needed" >&2; exit 1; }; done
[ "$(uname -s)" = Linux ] || { echo "compat/nvsentinel runs NVSentinel's containers on this host's network: it needs a Linux host" >&2; exit 1; }
PODMAN=; docker --version 2>/dev/null | grep -qi podman && PODMAN=1
if [ -n "$BASE" ]; then
	[ -r "$BASE" ] || { echo "BASE: $BASE is not readable" >&2; exit 1; }
	[ "$(uname -m)" = x86_64 ] && [ -w /dev/kvm ] || { echo "the GPU guest is x86_64 under KVM" >&2; exit 1; }
	for t in qemu-system-x86_64 qemu-img ssh ssh-keygen; do command -v $t >/dev/null || { echo "$t is needed for the GPU guest" >&2; exit 1; }; done
	BASE=$(cd "$(dirname "$BASE")" && pwd)/$(basename "$BASE")
fi

# ---------------------------------------------------------------- build from source

ARCH=$(docker_arch)
NVS_COMMIT=$(nvs_source "$NVS_REF")
nvs_build "$ARCH"
IMAGE=${IMAGE:-dctwin-nvsentinel:$NVS_REF-$ARCH}
nvs_image "$IMAGE" "$ARCH"
SRC=$WORK/src/nvsentinel

case "$KCM" in
none) KCM_BIN= ;;
build)
	KCM_BIN="$WORK/bin/kube-controller-manager"
	if [ ! -x "$KCM_BIN" ]; then
		[ -d "$WORK/src/kubernetes/.git" ] || git clone -q --depth 1 --branch "$K8S_REF" https://github.com/kubernetes/kubernetes.git "$WORK/src/kubernetes"
		echo "building kube-controller-manager $K8S_REF (once; about ten minutes)" >&2
		(cd "$WORK/src/kubernetes" && GOFLAGS=${GOFLAGS:--p=2} go build -o "$KCM_BIN" ./cmd/kube-controller-manager)
	fi
	;;
*) KCM_BIN=$KCM ;;
esac

# helm renders NVSentinel's chart; the release binary, checked against the digest helm publishes
HELM=$(command -v helm || true)
if [ -z "$HELM" ]; then
	HELM=$WORK/bin/helm
	if [ ! -x "$HELM" ]; then
		t=helm-$HELM_VERSION-linux-$ARCH.tar.gz
		curl -sfL -o "$WORK/bin/$t" "https://get.helm.sh/$t"
		want=$(curl -sfL "https://get.helm.sh/$t.sha256sum" | awk '{print $1}')
		[ "$(sha256sum "$WORK/bin/$t" | awk '{print $1}')" = "$want" ] || { echo "$t: digest mismatch" >&2; exit 1; }
		tar -xzf "$WORK/bin/$t" -C "$WORK/bin" --strip-components 1 "linux-$ARCH/helm" && rm "$WORK/bin/$t"
	fi
fi

# The twin's assets: the user's cache, except that the guest image's directories are this run's
# own, because the twin writes its initrd (its /init with the in-host agent, and the kernel
# module) beside the image; whatever is missing (the guest image for this host's architecture) is
# fetched into it.
OWN_ASSETS=
if [ -z "${DCTWIN_ASSETS:-}" ]; then
	cache=$HOME/.cache/dc-twin/assets
	mkdir -p "$WORK/assets"
	for a in "$cache"/*; do
		[ -e "$a" ] || continue
		case "$(basename "$a")" in
		alpine-netboot@*)
			mkdir -p "$WORK/assets/$(basename "$a")"
			for f in "$a"/vmlinuz-* "$a"/initramfs-* "$a"/modloop-*; do [ -e "$f" ] && ln -sf "$f" "$WORK/assets/$(basename "$a")/"; done ;;
		*) ln -sfn "$a" "$WORK/assets/$(basename "$a")" ;;
		esac
	done
	export DCTWIN_ASSETS=$WORK/assets
	OWN_ASSETS=1
	HOME="$WORK" "$DCTWIN" assets fetch >"$WORK/assets-fetch.log" 2>&1 || { cat "$WORK/assets-fetch.log" >&2; exit 1; }
fi
# The in-host agent's kernel module (internal/inhost/kmod, D50), built for the twin's guest kernel
# and put beside its image, where the vm adapter adds it to the guest's initramfs and the agent
# loads it: a fault's line is then printk'd, a kernel record with facility kern. KMOD=0 leaves it
# out (of this run's own copy of the image), and the agent writes through /dev/kmsg instead.
GUEST_DIR=$DCTWIN_ASSETS/alpine-netboot@3.24.2-$(uname -m)
if [ "$KMOD" = 1 ]; then
	"$root/internal/inhost/kmod/build.sh" "$GUEST_DIR" 2>"$EV/kmod-build.txt" || { cat "$EV/kmod-build.txt" >&2; exit 1; }
elif [ -n "$OWN_ASSETS" ]; then
	rm -f "$GUEST_DIR/dctwin-kmsg.ko" "$GUEST_DIR/dctwin-kmsg.ko.txt"
fi

# The GPU guest: Mokka, the twin's GPUs rendered for it, and the guest prepared once
trap 'gpu_guest_stop' EXIT
if [ -n "$BASE" ]; then
	(cd "$root" && CGO_ENABLED=0 go build -o "$WORK/bin/dctwin-mokka" ./cmd/dctwin-mokka)
	gpu_mokka
	GHM_IMAGE=dctwin-nvs-gpu-health-monitor:$NVS_REF
	"$WORK/bin/dctwin-mokka" render "$ESTATE" --select "$HOST" --base "$WORK/dl/mokka-profiles" --out "$WORK/render" >"$EV/render.txt"
	read -r GNODE NGPU < <(python3 -I -c 'import json,sys; h=json.load(open(sys.argv[1]))[0]; print(h["Node"], len(h["GPUs"]))' "$WORK/render/hosts.json")
	DRIVER_VERSION=$(sed -n 's/^ *driver_version: *"\{0,1\}\([0-9.]*\)"\{0,1\} *$/\1/p' "$WORK/render/$GNODE.yaml" | head -1)
	gpu_guest_prepare
fi

[ "${BUILD_ONLY:-}" = 1 ] && { echo "built: NVSentinel $NVS_REF ($NVS_COMMIT) for linux/$ARCH, image $IMAGE, helm $HELM${BASE:+, the GPU guest prepared ($WORK/guest/prepared.qcow2)}" >&2; exit 0; }

# ---------------------------------------------------------------- bring-up

PIDS=""
COMPONENTS="platform-connectors syslog-health-monitor fault-quarantine node-drainer fault-remediation janitor-provider janitor"
gpu_up() { [ -n "$BASE" ] && [ -n "${GPU_QEMU_PID:-}" ] && kill -0 "$GPU_QEMU_PID" 2>/dev/null; }
# collect keeps what a run leaves behind, however it ends
collect() {
	local c
	for c in $COMPONENTS; do docker logs "dctwin-nvs-$c" >"$EV/$c.log" 2>&1 || true; done
	cp "$WORK/journal-remote.log" "$WORK/sync.log" "$WORK/relay.log" "$WORK/forward.log" "$EV/" 2>/dev/null || true
	[ -z "${INHOST_DIR:-}" ] || cp "$INHOST_DIR/kmsg.log" "$EV/guest-kmsg.log" 2>/dev/null || true
	[ -z "${TWIN_STATE:-}" ] || { twin events >"$EV/twin-events.txt" 2>/dev/null; twin report >"$EV/fidelity-report.txt" 2>/dev/null; } || true
	kc get rebootnodes -o yaml >"$EV/rebootnodes.yaml" 2>/dev/null || true
	[ -z "${NODE:-}" ] || kc get node "$NODE" -o yaml >"$EV/node-final.yaml" 2>/dev/null || true
	psqlq "select document from health_events order by created_at" >"$EV/health-events.jsonl" 2>/dev/null || true
	if gpu_up; then
		gpu_gssh 'sudo docker logs dctwin-nvs-gpu-health-monitor' >"$EV/gpu-health-monitor.log" 2>&1 || true
		gpu_gssh 'sudo dmesg -r' >"$EV/gpu-guest-dmesg.txt" 2>&1 || true
		gpu_gssh 'sudo journalctl -b --no-pager -u dctwin-nvml-mock -u docker' >"$EV/gpu-guest-journal.txt" 2>&1 || true
	fi
	cp "$WORK/guest/console.log" "$EV/gpu-guest-console.log" 2>/dev/null || true
}
cleanup() {
	set +e
	collect
	if [ "${KEEP:-}" = 1 ]; then
		echo "KEEP=1: left running. Stop with: kill $PIDS; docker rm -f \$(docker ps -aq --filter label=dc-twin.io/harness=nvsentinel); docker volume rm dctwin-nvs-run dctwin-nvs-journal dctwin-nvs-syslog-state; DCTWIN_STATE=$TWIN_STATE HOME=$TWIN_HOME $DCTWIN down; kill $CLUSTER_PIDS${GPU_QEMU_PID:+; kill $GPU_QEMU_PID ${GPU_SEED_PID:-} (the GPU guest: ssh -i $WORK/guest/id_ed25519 -p $(cat "$WORK/guest/ssh.port" 2>/dev/null) debian@127.0.0.1)}" >&2
		return
	fi
	local p
	for p in $PIDS; do pkill -P "$p" 2>/dev/null; kill "$p" 2>/dev/null; done
	docker rm -f $(docker ps -aq --filter label=dc-twin.io/harness=nvsentinel) >/dev/null 2>&1
	docker volume rm dctwin-nvs-run dctwin-nvs-journal dctwin-nvs-syslog-state >/dev/null 2>&1
	gpu_guest_stop
	rm -f "$WORK/guest/run.qcow2"
	twin_down
	nvs_postgres_down
	cluster_down
}
trap cleanup EXIT
trap 'exit 130' INT TERM

rm -rf "$WORK/cluster" "$WORK/twin" "$WORK/nvs" "$WORK/sync.log" "$WORK/relay.log" "$WORK/forward.log" "$WORK/standin.busy"
nvs_cluster_up "$WORK/cluster" 127.0.0.1 "$KCM_BIN"
nvs_postgres_up "$WORK/pg"
echo "kube-apiserver :$APISERVER_PORT, kube-controller-manager ${KCM_BIN:-none}, PostgreSQL :$PG_PORT ($PG_MODE)" >&2

# NVSentinel's chart, rendered at the tag with values.yaml; its ConfigMaps go into the cluster as an
# install would put them, and each container is given the files its pod would mount from them.
kc create namespace nvsentinel >/dev/null
kc create namespace dc-twin-training >/dev/null
CHART=$SRC/distros/kubernetes/nvsentinel
HELM_HOME=$WORK/helm
mkdir -p "$HELM_HOME"
HOME=$HELM_HOME XDG_CACHE_HOME=$HELM_HOME/cache XDG_CONFIG_HOME=$HELM_HOME/config XDG_DATA_HOME=$HELM_HOME/data \
	"$HELM" template nvsentinel "$CHART" -n nvsentinel -f "$here/values.yaml" \
	--set global.datastore.connection.host=127.0.0.1 --set global.datastore.connection.port="$PG_PORT" >"$EV/chart-rendered.yaml"
awk 'BEGIN { RS = "\n---\n" } /\nkind: ConfigMap\n/ { print "---"; print }' "$EV/chart-rendered.yaml" >"$WORK/configmaps.yaml"
kc apply -n nvsentinel -f "$WORK/configmaps.yaml" >/dev/null
kc apply --server-side -f "$CHART/charts/janitor/crds/" >/dev/null
cm() { kc get configmap -n nvsentinel "$1" -o json | python3 -I -c 'import json, sys; print(json.load(sys.stdin)["data"][sys.argv[1]], end="")' "$2"; }

twin_up "$ESTATE" "$WORK/twin" "$ADAPTERS" "$KUBECONFIG_FILE"
echo "twin up: $(grep -o 'control API: [^ ]*' "$WORK/twin/up.log"), state $TWIN_STATE" >&2
CREDS=$(twin credentials "$HOST")
HOST_PATH=$(echo "$CREDS" | python3 -I -c 'import json, sys; print(json.load(sys.stdin)[0]["path"])')
NODE=$(echo "$HOST" | tr '/' '-' | tr 'A-Z' 'a-z')
INHOST_DIR=$TWIN_STATE/inhost/$(echo "$HOST_PATH" | tr '/' '_')

# ---------------------------------------------------------------- helpers

ok() { "$@" >/dev/null 2>&1 && echo 0 || echo 1; }
# grep_q: grep -q that reads its whole input: under pipefail, grep -q's early exit fails the writer
# (SIGPIPE) and with it the pipeline, so a long log that holds the match would read as a miss
grep_q() { grep "$@" >/dev/null; }
jp() { kc get node "$NODE" -o "jsonpath=$1" 2>/dev/null; }
cond() { jp "{.status.conditions[?(@.type==\"$1\")].status}"; }
boot_id() { jp '{.status.nodeInfo.bootID}'; }
label() { jp "{.metadata.labels.$1}"; }
nvs_state() { label 'dgxc\.nvidia\.com/nvsentinel-state'; }
events() { twin events --select "$HOST_PATH" "$@" 2>/dev/null; }
since_seq() { twin events 2>/dev/null | tail -1 | awk '{print substr($1, 2)}'; }
pg_now() { psqlq 'select now()' 2>/dev/null; }
# hev [events.py filters]: this node's health events (since $SINCE when set), oldest first
hev() { psqlq "select document from health_events where node_name = '$NODE'${SINCE:+ and created_at > '$SINCE'} order by created_at" 2>/dev/null | python3 -I "$here/lib/events.py" "$@"; }
quarantined() { [ "$(jp '{.spec.unschedulable}')" = true ] && [ -n "$(jp '{.metadata.annotations.quarantineHealthEvent}')" ]; }
uncordoned() { [ "$(jp '{.spec.unschedulable}')" != true ] && [ -z "$(jp '{.metadata.annotations.quarantineHealthEvent}')" ]; }
rebootnodes() { kc get rebootnodes.janitor.dgxc.nvidia.com -o name 2>/dev/null | grep -c "$NODE" || true; }

# Files a pod would have: the in-cluster ServiceAccount directory (the cluster's admin token, the
# harness's CA) and its ConfigMap mounts, staged per container and copied in before it starts.
SA=$WORK/nvs/serviceaccount
mkdir -p "$SA"
printf '%s' "$ADMIN_TOKEN" >"$SA/token"
cp "$CLUSTER_CA" "$SA/ca.crt"
printf nvsentinel >"$SA/namespace"
kc get configmap -n nvsentinel nvsentinel-datastore-config -o json |
	python3 -I -c 'import json, sys; d = json.load(sys.stdin)["data"]; print("\n".join(f"{k}={v}" for k, v in d.items() if "\n" not in v))' >"$WORK/nvs/datastore.env"
docker volume create dctwin-nvs-run >/dev/null
docker volume create dctwin-nvs-journal >/dev/null
docker volume create dctwin-nvs-syslog-state >/dev/null

# pod <name> <files-dir> <create args...> -- <command...>: a module's container, on this host's
# network as a pod with hostNetwork would be, so every listener is given a free port.
pod() {
	local name=$1 files=$2
	shift 2
	local opts=()
	while [ "$1" != -- ]; do opts+=("$1"); shift; done
	shift
	docker create --name "dctwin-nvs-$name" --label dc-twin.io/harness=nvsentinel --network host --memory "${POD_MEMORY:-384m}" \
		-e POD_NAME="$name" -e POD_NAMESPACE=nvsentinel -e LOG_LEVEL=info \
		-e KUBERNETES_SERVICE_HOST=127.0.0.1 -e KUBERNETES_SERVICE_PORT="$APISERVER_PORT" \
		-v "$SA:/var/run/secrets/kubernetes.io/serviceaccount:ro" \
		--env-file "$WORK/nvs/datastore.env" ${opts[@]+"${opts[@]}"} "$IMAGE" "$@" >/dev/null
	[ -z "$files" ] || docker cp "$files/." "dctwin-nvs-$name:/" >/dev/null
	docker start "dctwin-nvs-$name" >/dev/null
}
stage() { mkdir -p "$WORK/nvs/$1/$(dirname "$2")" && cat >"$WORK/nvs/$1/$2"; }

# ---------------------------------------------------------------- the node

node_ready() { [ "$(cond Ready)" = True ] && [ -n "$(boot_id)" ]; }
ms=$(wait_until 150 node_ready) || true
if ! node_ready; then
	# the machine did not come up (its power-on is a twin incident in the event log): power it on
	# once more, as an operator would, and say so
	twin events --kind twin >"$EV/U1-incidents.txt" 2>/dev/null || true
	echo "$(date -u +%FT%TZ) $NODE not Ready after ${ms} ms; powering $HOST on again: $(tail -1 "$EV/U1-incidents.txt" | cut -c1-200)" | tee -a "$EV/notes.txt" >&2
	twin power on "$HOST" >/dev/null 2>&1 || true
	ms=$(wait_until 150 node_ready) || true
fi
BOOT0=$(boot_id) || true
attached_obs=$(events --kind observe | grep 'inhost.agent' | grep attached | tail -1) || true
agent_boot=$(echo "$attached_obs" | grep -o 'boot [0-9a-f-]*' | cut -d' ' -f2) || true
TMOD=$(echo "$attached_obs" | grep -o 'kernel module [a-z]*' | cut -d' ' -f3) || true
imp=$(jp '{.metadata.annotations.dc-twin\.io/impersonated}') || true
record U1 "The server's guest boots and its Node registers (in-host agent)" "Node $NODE Ready, boot ID the guest's own, annotated impersonated" \
	"Ready=$(cond Ready), bootID $BOOT0, agent's boot $agent_boot, impersonated=$imp, kernel $(jp '{.status.nodeInfo.kernelVersion}') $(jp '{.status.nodeInfo.architecture}'), after ${ms} ms" \
	"$( [ "$(cond Ready)" = True ] && [ -n "$BOOT0" ] && [ "$BOOT0" = "$agent_boot" ] && [ "$imp" = inhost ] && echo PASS || echo FAIL)"
kc get node "$NODE" -o yaml >"$EV/U1-node.yaml"
if [ "$KMOD" = 1 ]; then
	record K1 "The in-host agent's kernel module in the twin's guest (dctwin-kmsg, D50)" "built for the guest's kernel, vermagic and symbol versions equal to its own modules', loaded by the agent" \
		"$(grep -E '^(guest kernel|symbol versions)' "$EV/kmod-build.txt" | tr -s ' ' | tr '\n' ';' | cut -c1-220) agent: kernel module ${TMOD:-?}" \
		"$( [ "$TMOD" = loaded ] && echo PASS || echo FAIL)"
else
	record K1 "The in-host agent's kernel module in the twin's guest (dctwin-kmsg, D50)" "KMOD=0: left out; the agent writes through /dev/kmsg" "agent: kernel module ${TMOD:-?}" \
		"$( [ "$TMOD" = absent ] && echo PASS || echo FAIL)"
fi

# The node's journal: systemd-journal-remote, fed the twin's export of the guest's kernel log.
EXPORT=$INHOST_DIR/journal.export
(tail -c +1 -F "$EXPORT" 2>/dev/null | docker run -i --rm --name dctwin-nvs-journal --label dc-twin.io/harness=nvsentinel --network none --memory 256m \
	-v dctwin-nvs-journal:/nvsentinel/var/log/journal "$IMAGE" \
	/lib/systemd/systemd-journal-remote --split-mode=none --output=/nvsentinel/var/log/journal/node.journal - >"$WORK/journal-remote.log" 2>&1) &
PIDS="$PIDS $!"

# platform-connectors, node-local, on the socket the node's monitors share (a hostPath on a node)
cm platform-connectors config.json | stage platform-connectors etc/config/config.json
for f in metadata.toml overrides.toml dedup.toml; do cm platform-connectors $f | stage platform-connectors etc/config/$f; done
pod platform-connectors "$WORK/nvs/platform-connectors" -e NODE_NAME="$NODE" -e APP_NAME=platform-connector \
	-v dctwin-nvs-run:/var/run/nvsentinel -- \
	platform-connectors --config=/etc/config/config.json --metrics-port="$(free_port)" --socket=/var/run/nvsentinel/nvsentinel.sock
# syslog-health-monitor, with the chart's arguments. It reads the node's boot ID from
# /proc/sys/kernel/random/boot_id; the node's kernel is the guest's, so the guest's boot ID is shown
# there (a bind mount in the container's own mount namespace, hence CAP_SYS_ADMIN). Its state
# file (the boot ID it last saw, its journal cursors) is on a volume, as the chart puts it on the
# node's disk, so it survives the restart that follows a reboot.
cm syslog-health-monitor-config nic-driver.toml | stage syslog etc/syslog-health-monitor/nic-driver.toml
cm syslog-health-monitor-config cancellations.toml | stage syslog etc/syslog-health-monitor/cancellations.toml
SYSLOG_OPTS=(--cap-add SYS_ADMIN)
[ -n "$PODMAN" ] || SYSLOG_OPTS+=(--security-opt apparmor=unconfined)
start_monitor() {
	printf '%s\n' "$1" | stage syslog etc/nvs/boot_id
	if docker inspect dctwin-nvs-syslog-health-monitor >/dev/null 2>&1; then
		docker cp "$WORK/nvs/syslog/etc/nvs/boot_id" dctwin-nvs-syslog-health-monitor:/etc/nvs/boot_id >/dev/null
		docker restart dctwin-nvs-syslog-health-monitor >/dev/null
		return
	fi
	pod syslog-health-monitor "$WORK/nvs/syslog" -e NODE_NAME="$NODE" "${SYSLOG_OPTS[@]}" \
		-v dctwin-nvs-run:/var/run/nvsentinel -v dctwin-nvs-journal:/nvsentinel/var/log/journal:ro \
		-v dctwin-nvs-syslog-state:/var/run/syslog_monitor -- \
		sh -c 'mount --bind /etc/nvs/boot_id /proc/sys/kernel/random/boot_id && exec syslog-health-monitor "$@"' syslog-health-monitor \
		--polling-interval 15s --metrics-port "$SYSLOG_METRICS" --kata-enabled false \
		--checks SysLogsXIDError,SysLogsSXIDError,SysLogsGPUFallenOff --metadata-path /var/lib/nvsentinel/gpu_metadata.json \
		--processing-strategy EXECUTE_REMEDIATION --nic-driver-config /etc/syslog-health-monitor/nic-driver.toml \
		--sysfs-root /nvsentinel/sys --boot-lookback-window 2h --cancellations-config /etc/syslog-health-monitor/cancellations.toml \
		--platform-connector-socket unix:///var/run/nvsentinel/nvsentinel.sock
}
SYSLOG_METRICS=$(free_port)

# ---------------------------------------------------------------- the node's GPUs (BASE)

if [ -n "$BASE" ]; then
	[ "$GNODE" = "$NODE" ] || { echo "dctwin-mokka names the host $GNODE, the in-host Node is $NODE" >&2; exit 1; }
	gpu_guest_up
	gpu_guest_configure "$WORK/render/$NODE.yaml" "$NODE" "$NGPU" "$DRIVER_VERSION" >"$EV/G0-configure.txt" 2>&1 || { cat "$EV/G0-configure.txt" >&2; exit 1; }
	export NVS_GUEST_KEY=$WORK/guest/id_ed25519 NVS_GUEST_PORT=$(cat "$WORK/guest/ssh.port") NVS_GUEST_NODE=$NODE
	MOKKA_SYNC=("$WORK/bin/dctwin-mokka" sync "$ESTATE" --select "$HOST" --control "$TWIN_STATE/control.json")
	"${MOKKA_SYNC[@]}" -- "$here/put-override.sh" '{node}' >"$WORK/sync.log" 2>&1 &
	PIDS="$PIDS $!"
	GSSH=(ssh -q -i "$WORK/guest/id_ed25519" -p "$(cat "$WORK/guest/ssh.port")" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=10)
	python3 -I "$root/compat/fleet-intelligence/kmsg-relay.py" "$INHOST_DIR/kmsg.log" "$WORK/relay.log" -- "${GSSH[@]}" debian@127.0.0.1 &
	PIDS="$PIDS $!"
	# The node's monitors share one platform-connectors socket (a hostPath on a node). The GPU
	# guest's monitor reaches the node's one platform-connectors through it, forwarded into the
	# guest's /run/nvsentinel over ssh (OpenSSH's Unix-socket forwarding), again after every boot.
	PC_SOCK=$(docker volume inspect dctwin-nvs-run --format '{{.Mountpoint}}')/nvsentinel.sock
	(set +e # a failed connection (the guest rebooting) must not end the loop
	while :; do
		"${GSSH[@]}" debian@127.0.0.1 'rm -f /run/nvsentinel/nvsentinel.sock' 2>/dev/null
		"${GSSH[@]}" -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=5 -o ServerAliveCountMax=3 \
			-R "/run/nvsentinel/nvsentinel.sock:$PC_SOCK" debian@127.0.0.1 2>>"$WORK/forward.log"
		echo "$(date -u +%FT%TZ) forward ended; again" >>"$WORK/forward.log"
		sleep 2
	done) &
	PIDS="$PIDS $!"
	# the chart's gpu-health-monitor: its ConfigMap's files and its DaemonSet's arguments (dcgm-4.x)
	ghm_cm=$(kc get configmap -n nvsentinel -o name | sed 's|.*/||' | grep 'gpu-health-monitor' | head -1)
	mkdir -p "$WORK/nvs/dcgmhealth"
	cm "$ghm_cm" config.ini >"$WORK/nvs/dcgmhealth/config.ini"
	cm "$ghm_cm" dcgmerrorsmapping.csv >"$WORK/nvs/dcgmhealth/dcgmerrorsmapping.csv"
	python3 -I - "$EV/chart-rendered.yaml" >"$WORK/nvs/ghm.args" <<'EOF'
import sys
docs = open(sys.argv[1]).read().split("\n---\n")
doc = next(d for d in docs if "\nkind: DaemonSet\n" in d and "gpu_health_monitor" in d and "-dcgm-4.x" in d)
lines = doc.splitlines()
i = next(n for n, l in enumerate(lines) if l.strip() == "args:")
ind = len(lines[i]) - len(lines[i].lstrip())
for l in lines[i + 1:]:
    s = l.strip()
    if not s.startswith("- ") or len(l) - len(l.lstrip()) < ind:
        break
    v = s[2:].strip()
    if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
        v = v[1:-1]
    print(v)
EOF
	gpu_info=$(gpu_gssh 'echo "$(. /etc/os-release; echo $PRETTY_NAME), kernel $(uname -r), $(nproc) vCPU, $(free -m | awk "/^Mem:/ {print \$2}") MiB; kernel module dctwin-kmsg $(cat /var/lib/dctwin-kmod.txt); Mokka node agent $(systemctl is-active dctwin-nvml-mock)"' 2>&1) || true
	GMOD=$(gpu_gssh 'cut -d" " -f1 /var/lib/dctwin-kmod.txt' 2>/dev/null) || true
	record G0 "The node's GPU guest (BASE): Debian, Mokka's NVML, the kernel module" "Mokka's node agent active on the profile rendered from the model; NVML the driver tree's; dctwin-kmsg loaded" \
		"$gpu_info; $(tail -1 "$EV/G0-configure.txt" | cut -c1-120)" "$( [ "$GMOD" = loaded ] && grep -q 'configured, NVML /var/lib/nvml-mock/driver/' "$EV/G0-configure.txt" && echo PASS || echo FAIL)"
	gpu_gssh 'nvidia-smi --query-gpu=index,uuid,pci.bus_id --format=csv,noheader' >"$EV/G1-smi.csv" 2>&1 || true
	g1=$(python3 -I - "$WORK/render/hosts.json" "$EV/G1-smi.csv" <<'EOF'
import json, sys
want = {g["Index"]: (g["UUID"], g["BusID"].upper()) for g in json.load(open(sys.argv[1]))[0]["GPUs"]}
got = {}
for l in open(sys.argv[2]):
    f = [x.strip() for x in l.split(",")]
    if len(f) == 3 and f[0].isdigit():
        got[int(f[0])] = (f[1], f[2].upper().replace("00000000:", "0000:"))
same = sum(1 for i in want if got.get(i) == want[i])
print("%d %d %d" % (len(want), len(got), same))
EOF
) || true
	read -r g1w g1g g1s <<<"${g1:-0 0 0}"
	record G1 "The GPU guest's NVML presents the twin's GPUs" "$NGPU GPUs; each one's UUID and PCI bus ID the model's" \
		"nvidia-smi: $g1g GPUs, $g1s of the model's $g1w equal (UUID and bus ID); $(head -1 "$EV/G1-smi.csv" | cut -c1-90)" \
		"$( [ "$g1w" -gt 0 ] && [ "$g1s" = "$g1w" ] && [ "$g1g" = "$g1w" ] && echo PASS || echo FAIL)"
fi

# ---------------------------------------------------------------- the control plane

cm fault-quarantine config.toml | stage fault-quarantine etc/config/config.toml
pod fault-quarantine "$WORK/nvs/fault-quarantine" -e APP_NAME=fault-quarantine -- \
	fault-quarantine --metrics-port="$(free_port)" --dry-run=false --circuit-breaker-enabled=false \
	--gpu-node-label=nvidia.com/gpu.present=true --kube-api-qps=5 --kube-api-burst=10 --config-path=/etc/config/config.toml
cm node-drainer config.toml | stage node-drainer etc/config/config.toml
pod node-drainer "$WORK/nvs/node-drainer" -e APP_NAME=node-drainer -- \
	node-drainer --metrics-port="$(free_port)" --config-path=/etc/config/config.toml --dry-run=false --kube-api-qps=5 --kube-api-burst=10 \
	--requeue-backoff-base=10s --tls-enabled=false
for f in config.toml nvidia-reboot.yaml terminate-node.yaml; do cm fault-remediation $f | stage fault-remediation etc/config/$f; done
pod fault-remediation "$WORK/nvs/fault-remediation" -e APP_NAME=fault-remediation -- \
	fault-remediation --dry-run=false --enable-log-collector=false --leader-elect=false --config-path=/etc/config/config.toml \
	--metrics-address="127.0.0.1:$(free_port)" --health-address="127.0.0.1:$(free_port)"
# janitor-provider with the label provider; its gRPC TLS and token audience need cert-manager and
# projected tokens, so they are off (the chart sets TLS_* and AUTH_AUDIENCES), and the janitor
# reaches it insecurely
JP_PORT=$(free_port)
pod janitor-provider "" -e CSP=label -e JANITOR_PROVIDER_PORT="$JP_PORT" -e METRICS_PORT="$(free_port)" \
	-e LABEL_REBOOT_KEY=nke.nvidia.com/reboot=requested-by-nvsentinel -e LABEL_TERMINATE_KEY=nke.nvidia.com/terminate=requested-by-nvsentinel -- \
	janitor-provider
cm janitor config.yaml | python3 -I -c '
import sys
out = []
for line in sys.stdin.read().splitlines():
    s = line.strip()
    if s.startswith("cspProviderHost:"):
        out.append(line.split("cspProviderHost:")[0] + "cspProviderHost: 127.0.0.1:" + sys.argv[1])
        out.append(line.split("cspProviderHost:")[0] + "cspProviderInsecure: true")
    elif s.startswith("cspProviderCAPath:") or s.startswith("cspProviderTokenPath:"):
        continue
    else:
        out.append(line)
print("\n".join(out))' "$JP_PORT" | stage janitor etc/nvsentinel/janitor/config.yaml
mkdir -p "$WORK/nvs/janitor/tmp/k8s-webhook-server/serving-certs"
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=janitor-webhook" \
	-keyout "$WORK/nvs/janitor/tmp/k8s-webhook-server/serving-certs/tls.key" -out "$WORK/nvs/janitor/tmp/k8s-webhook-server/serving-certs/tls.crt" 2>/dev/null
# (its admission webhook server listens on controller-runtime's :9443, which has no flag; no
# ValidatingWebhookConfiguration is installed, so nothing calls it)
pod janitor "$WORK/nvs/janitor" -- \
	janitor --metrics-bind-address="127.0.0.1:$(free_port)" --health-probe-bind-address="127.0.0.1:$(free_port)" \
	--config-bind-address="127.0.0.1:$(free_port)" --config=/etc/nvsentinel/janitor/config.yaml \
	--webhook-cert-path=/tmp/k8s-webhook-server/serving-certs --webhook-cert-name=tls.crt --webhook-cert-key=tls.key \
	--enable-ttl=true --default-ttl=336h
# the monitors last: fault-quarantine creates the datastore's tables
tables() { [ -n "$(psqlq "select 1 from information_schema.tables where table_name = 'health_events'" 2>/dev/null)" ]; }
wait_until 90 tables >/dev/null || true
start_monitor "$BOOT0"
# gpu_monitor: the GPU guest's gpu-health-monitor, (re)started as the kubelet starts the node's pod
gpu_monitor() { gpu_guest_monitor "$WORK/nvs/dcgmhealth" "$WORK/nvs/ghm.args" >>"$EV/gpu-monitor-starts.txt" 2>&1; }
ghm_running() { [ "$(gpu_gssh 'sudo docker inspect -f "{{.State.Running}}" dctwin-nvs-gpu-health-monitor' 2>/dev/null)" = true ]; }
[ -z "$BASE" ] || gpu_monitor || true

running() { local c n=0; for c in $COMPONENTS; do [ "$(docker inspect -f '{{.State.Running}}' "dctwin-nvs-$c" 2>/dev/null)" = true ] && n=$((n + 1)); done; echo $n; }
all_running() { [ "$(running)" = 7 ] && [ "$(docker inspect -f '{{.State.Running}}' dctwin-nvs-journal 2>/dev/null)" = true ] && { [ -z "$BASE" ] || ghm_running; }; }
sleep 10
wait_until 60 all_running >/dev/null || true
record U2 "NVSentinel $NVS_REF ($NVS_COMMIT) running, built from source" "7 of 7 modules and the node's journal (systemd-journal-remote)${BASE:+, and gpu-health-monitor in the GPU guest}" \
	"$(running) of 7 ($COMPONENTS); journal $(docker inspect -f '{{.State.Running}}' dctwin-nvs-journal 2>/dev/null)${BASE:+; gpu-health-monitor $(ghm_running && echo running || echo "not running")}" "$(pass_if "$(ok all_running)")"
record U3 "PostgreSQL datastore, schema made by NVSentinel" "health_events table exists" "$(psqlq "select count(*) from information_schema.tables where table_schema = 'public'" 2>/dev/null) tables" "$(pass_if "$(ok tables)")"
# syslog-health-monitor starts a fresh node's journal at its tail, on its first pass (one polling
# interval after it starts): a fault before that pass is history it does not read, as on a node
first_pass() { docker logs dctwin-nvs-syslog-health-monitor 2>&1 | grep_q "run cycle completed successfully"; }
ms=$(wait_until 90 first_pass) || true
record U4 "syslog-health-monitor's first pass over the node's journal" "done (it starts at the journal's tail)" "$(first_pass && echo done || echo "not done") after ${ms} ms" "$(pass_if "$(ok first_pass)")"
if [ -n "$BASE" ]; then
	# the monitor's DCGM, and the healthy baseline it publishes: one healthy event per GPU and watch
	healthy_gpus() { [ "$(hev --agent gpu-health-monitor --healthy | grep -o 'entities=GPU=[0-9]*' | sort -u | wc -l)" -ge "$NGPU" ]; }
	ms=$(wait_until 240 healthy_gpus) || true
	gpu_gssh 'sudo docker exec dctwin-nvs-gpu-health-monitor sh -c "nv-hostengine --version 2>/dev/null | head -3; dcgmi discovery -l --host localhost:5555 2>&1 | head -40"' >"$EV/G2-dcgm.txt" 2>&1 || true
	hev --agent gpu-health-monitor >"$EV/G2-events.txt" || true
	sup=$(gpu_gssh 'sudo docker logs dctwin-nvs-gpu-health-monitor 2>&1 | grep -o "supported gpus are \[[0-9, ]*\]" | tail -1' 2>/dev/null) || true
	record G2 "NVSentinel's gpu-health-monitor on DCGM (in-process host engine, the chart's embedded-mode) sees the node's GPUs healthy" \
		"DCGM's group holds the $NGPU GPUs; a healthy event for each, published through the node's platform-connectors" \
		"${sup:-no group}; healthy events for $(grep -o 'entities=GPU=[0-9]*' "$EV/G2-events.txt" | sort -u | wc -l) GPUs ($(sort -u "$EV/G2-events.txt" | grep -o 'check=[A-Za-z]*' | sort -u | tr '\n' ' ')) after ${ms} ms" \
		"$(pass_if "$(ok healthy_gpus)")"
fi

# a workload on the server, in the namespace the drain policy evicts at once
sed -e "s|@NODE@|$NODE|" "$here/manifests/workload.yaml" | tee "$EV/workload.yaml" | kc apply -f - >/dev/null

# ---------------------------------------------------------------- the node's reboots, and the stand-in for the site's lifecycle manager

BMC=$(echo "$CREDS" | python3 -I -c 'import json, sys; print(json.load(sys.stdin)[0]["redfish"])')
BMC_USER=$(echo "$CREDS" | python3 -I -c 'import json, sys; d = json.load(sys.stdin)[0]; print(d["user"] + ":" + d["password"])')
rf() { curl -sk -u "$BMC_USER" "$@"; }
reset_target() {
	local sys
	sys=$(rf "https://$BMC/redfish/v1/Systems" | python3 -I -c 'import json, sys; print(json.load(sys.stdin)["Members"][0]["@odata.id"])')
	rf "https://$BMC$sys" | python3 -I -c 'import json, sys; print(json.load(sys.stdin)["Actions"]["#ComputerSystem.Reset"]["target"])'
}
force_restart() {
	rf -X POST -H 'Content-Type: application/json' -d '{"ResetType":"ForceRestart"}' -o /dev/null -w "Redfish answered %{http_code}" "https://$BMC$(reset_target)"
}
# node_reboot <command...>: the machine is reset by <command> (a Redfish request, an AC cycle);
# the GPU guest, which stands for the same machine's GPUs, is reset with it. Returns once the Node
# is back with a new boot ID and Ready, and the GPU guest is up (with the GPU state the model holds
# applied to Mokka before anything reads it). Prints what happened.
node_reboot() {
	local b0 gpid= r g=
	b0=$(boot_id) || true
	pg_now >"$WORK/last-reset.pg" || true
	r=$("$@" 2>&1 | tr -s '\n\t ' '   ' | cut -c1-160) || true
	[ -z "$BASE" ] || { gpu_guest_reset >"$WORK/gpu-reset.out" 2>&1 & gpid=$!; }
	node_back() { [ -n "$(boot_id)" ] && [ "$(boot_id)" != "$b0" ] && [ "$(cond Ready)" = True ]; }
	wait_until 400 node_back >/dev/null || true
	if [ -n "$gpid" ]; then
		wait "$gpid" && g=$(tail -1 "$WORK/gpu-reset.out") || g="not back: $(tail -1 "$WORK/gpu-reset.out")"
		"${MOKKA_SYNC[@]}" --once -- "$here/put-override.sh" '{node}' >>"$WORK/sync-once.log" 2>&1 || true
	fi
	echo "$r; boot $b0 → $(boot_id), Ready=$(cond Ready)${BASE:+; GPU guest boot $g}"
}
# node_pods: what the kubelet does after its node reboots: the node's DaemonSet pods restart
node_pods() {
	start_monitor "$(boot_id)"
	[ -z "$BASE" ] || gpu_monitor || true
}

# NVSentinel's label provider asks for a reboot by labelling the Node and considers it done once the
# label is gone and the Node's boot ID has changed; on bare metal the site's lifecycle manager (the
# role NICo or NKE plays) does it. This stand-in does it the way such a manager does, through the
# server's BMC (Redfish ComputerSystem.Reset, ForceRestart, at the target the system's own
# resource names), and, once the Node is back with a new boot ID, restarts the node's monitors as
# the kubelet restarts a DaemonSet's pods after its node reboots. All it does is logged to
# $EV/standin.log.
standin() {
	set +e # a failed request must not end the stand-in
	while :; do
		if [ -n "$(label 'nke\.nvidia\.com/reboot')" ]; then
			touch "$WORK/standin.busy"
			echo "$(date -u +%FT%TZ) reboot requested on $NODE (label nke.nvidia.com/reboot=$(label 'nke\.nvidia\.com/reboot'), boot $(boot_id)): POST $(reset_target) {ResetType: ForceRestart} to $BMC" >>"$EV/standin.log"
			echo "$(date -u +%FT%TZ) $(node_reboot force_restart)" >>"$EV/standin.log"
			echo "$(date -u +%FT%TZ) restarting the node's monitors (as the kubelet would) and removing the label" >>"$EV/standin.log"
			node_pods
			kc label node "$NODE" nke.nvidia.com/reboot- >>"$EV/standin.log" 2>&1
			rm -f "$WORK/standin.busy"
		fi
		sleep 2
	done
}
standin &
PIDS="$PIDS $!"

# ---------------------------------------------------------------- X: the fault, inside the host

SEQ0=$(since_seq) || true
SINCE=$(pg_now)
twin fault inject "$HOST/$GPU" nvidia/xid/xid-79 >"$EV/X1-inject.json"
recorded() { events --kind observe --since "$SEQ0" | grep_q 'kernelLog: .* → recorded'; }
ms=$(wait_until 60 recorded) || true
line=$(events --kind observe --since "$SEQ0" | grep 'kernelLog: .* → recorded' | grep ': 79,' | head -1) || true
how=$(echo "$line" | grep -o "printk'd by the in-host agent's kernel module\|written through /dev/kmsg[^:]*") || true
record X1 "dctwin fault inject $HOST/$GPU nvidia/xid/xid-79" \
	"the in-host agent enters the catalogue's lines into the guest's kernel ($( [ "$KMOD" = 1 ] && echo "printk'd by its kernel module" || echo "through /dev/kmsg")) and sees them in the kernel log" \
	"$(echo "$line" | sed 's/.*holds the fault.s line //' | cut -c1-160), ${how:-?}, after ${ms} ms" "$( recorded && { [ "$KMOD" != 1 ] || [ "${how#printk}" != "$how" ]; } && echo PASS || echo FAIL)"
grep -a 'NVRM: Xid' "$TWIN_STATE"/vm/*/console.log | tail -1 >"$EV/X1-console.txt" || true
in_journal() { docker exec dctwin-nvs-syslog-health-monitor sh -c 'journalctl -D /nvsentinel/var/log/journal _TRANSPORT=kernel -g "NVRM: Xid" -o verbose --no-pager' 2>/dev/null | grep_q 'MESSAGE=NVRM: Xid'; }
ms=$(wait_until 60 in_journal) || true
docker exec dctwin-nvs-syslog-health-monitor sh -c 'journalctl -D /nvsentinel/var/log/journal _TRANSPORT=kernel -g "NVRM: Xid" -o verbose --no-pager' >"$EV/X2-journal.txt" 2>&1 || true
fields=$(grep -E '^ *(PRIORITY|SYSLOG_FACILITY|SYSLOG_IDENTIFIER|DCTWIN_FAULT|DCTWIN_KMSG_FACILITY|_BOOT_ID)=' "$EV/X2-journal.txt" | tr -d ' ' | sort -u | tr '\n' ' ') || true
if [ "$KMOD" = 1 ]; then
	x2want="a _TRANSPORT=kernel entry, SYSLOG_FACILITY=0, SYSLOG_IDENTIFIER=kernel, MESSAGE the driver's line whole; no DCTWIN_FAULT marking (the record is a printk)"
	x2ok() { in_journal && grep -q '^ *SYSLOG_FACILITY=0$' "$EV/X2-journal.txt" && ! grep -q 'DCTWIN_FAULT' "$EV/X2-journal.txt"; }
else
	x2want="a _TRANSPORT=kernel entry, SYSLOG_IDENTIFIER=kernel, MESSAGE the driver's line, marked DCTWIN_FAULT (written through /dev/kmsg, facility user)"
	x2ok() { in_journal && grep -q 'DCTWIN_FAULT=' "$EV/X2-journal.txt"; }
fi
record X2 "The line in the node's journal" "$x2want" "${fields}after ${ms} ms" "$(pass_if "$(ok x2ok)")"

# ---------------------------------------------------------------- G: the same fault, through DCGM

if [ -n "$BASE" ]; then
	IDX79=${GPU#gpu-}
	inguest79() { gpu_gssh 'sudo dmesg -r' 2>/dev/null | grep_q 'NVRM: Xid .*: 79,'; }
	ms=$(wait_until 90 inguest79) || true
	gpu_gssh 'sudo dmesg -r' 2>/dev/null | grep 'NVRM:' >"$EV/G3-guest-kmsg.txt" || true
	gfac=$(grep 'NVRM: Xid' "$EV/G3-guest-kmsg.txt" | head -1 | sed -n 's/^<\([0-9]*\)>.*/\1/p') || true
	via=$(python3 -I -c 'import json,sys; print(" ".join(sorted({json.loads(l).get("via") or "?" for l in open(sys.argv[1])})))' "$WORK/relay.log" 2>/dev/null) || true
	record G3 "STAND-IN: the twin guest's NVRM records entered into the GPU guest's kernel (kmsg-relay.py, through its dctwin-kmsg)" \
		"the same lines, facility kern (<4>: kern, warning), in the kernel log DCGM reads" \
		"$(grep -c 'NVRM:' "$EV/G3-guest-kmsg.txt") NVRM records via ${via:-nothing}, the Xid's priority <${gfac:-?}> after ${ms} ms: $(grep 'NVRM: Xid' "$EV/G3-guest-kmsg.txt" | head -1 | cut -c1-110)" \
		"$( inguest79 && [ "$gfac" = 4 ] && echo PASS || echo FAIL)"
	dcgm79() { [ -n "$(hev --agent gpu-health-monitor --unhealthy --code DCGM_FR_FALLEN_OFF_BUS --first)" ]; }
	ms=$(wait_until 60 dcgm79) || true
	ev=$(hev --agent gpu-health-monitor --unhealthy --first) || true
	# what the monitor's in-process DCGM holds: the GPUs it still has, and GPU 2's Xid field
	# (DCGM_FI_DEV_XID_ERRORS, 230), as DCGM's own client reads them from the engine's listener
	dcgmi() { gpu_gssh "sudo docker exec dctwin-nvs-gpu-health-monitor dcgmi $* --host localhost:5555" 2>&1; }
	{ dcgmi discovery -l | grep -E 'GPUs found|^\| [0-9] '; dcgmi dmon -e 230 -i "$IDX79" -c 1; } >"$EV/G4-dcgm.txt" 2>&1 || true
	gpu_gssh 'sudo docker logs dctwin-nvs-gpu-health-monitor 2>&1 | grep -i -E "xid|fallen|lost|incident|supported gpus" | tail -8' >"$EV/G4-monitor.txt" 2>&1 || true
	if dcgm79; then res=PASS; else res=FINDING; fi
	record G4 "gpu-health-monitor raises its DCGM health event for the Xid 79" "an unhealthy, fatal GpuAllWatch event: DCGM_FR_FALLEN_OFF_BUS on GPU $IDX79, RESTART_BM (the chart's mapping)" \
		"${ev:-none} after ${ms} ms; DCGM: $(grep 'GPUs found' "$EV/G4-dcgm.txt" | head -1 | tr -s ' |' ' '), XID field of GPU $IDX79: $(grep -E '^ *GPU '"$IDX79"' ' "$EV/G4-dcgm.txt" | tr -s ' ' | head -1)" "$res"
fi

# ---------------------------------------------------------------- D: detection

# the syslog monitor's event for an Xid (default 79), as the datastore holds it
xid_event() { SINCE= hev --agent syslog-health-monitor --check SysLogsXIDError --unhealthy --fatal --code "${1:-79}" --first; }
detected() { [ -n "$(xid_event)" ]; }
ms=$(wait_until 120 detected) || true
psqlq "select document from health_events order by created_at" >"$EV/D1-health-events.jsonl" 2>/dev/null || true
ev=$(xid_event) || true
record D1 "syslog-health-monitor reports it through platform-connectors" "a fatal SysLogsXIDError health event for $NODE, error 79, in the datastore" "${ev:-none} after ${ms} ms" "$(pass_if "$(ok detected)")"
has_cond() { [ "$(cond SysLogsXIDError)" = True ]; }
ms=$(wait_until 60 has_cond) || true
record D2 "platform-connectors' Kubernetes connector" "Node condition SysLogsXIDError=True${BASE:+ (GpuAllWatch as G4 found it)}" \
	"SysLogsXIDError=$(cond SysLogsXIDError): $(jp '{.status.conditions[?(@.type=="SysLogsXIDError")].message}' | cut -c1-120)${BASE:+; GpuAllWatch=$(cond GpuAllWatch): $(jp '{.status.conditions[?(@.type=="GpuAllWatch")].message}' | cut -c1-100)}" \
	"$(pass_if "$(ok has_cond)")"

# ---------------------------------------------------------------- Q, N: quarantine and drain

ms=$(wait_until 120 quarantined) || true
kc get node "$NODE" -o yaml >"$EV/Q1-node.yaml"
record Q1 "fault-quarantine cordons the Node" "unschedulable, cordon-by NVSentinel, quarantineHealthEvent annotation" \
	"unschedulable=$(jp '{.spec.unschedulable}'), cordon-by=$(label 'k8saas\.nvidia\.com/cordon-by'), cordon-reason=$(label 'k8saas\.nvidia\.com/cordon-reason'), taints [$(jp '{range .spec.taints[*]}{.key}:{.effect} {end}')] after ${ms} ms" \
	"$(pass_if "$(ok quarantined)")"
drained() { case "$(nvs_state)" in drain-succeeded | remediating | remediation-succeeded) return 0 ;; esac; return 1; }
pod_gone() { [ -z "$(kc get pod -n dc-twin-training training-0 -o name 2>/dev/null)" ]; }
# evicted: gone, or Terminating; no kubelet confirms the kill, and node-drainer counts a pod
# Terminating past its grace period as drained (its stuck-terminating rule)
terminating() { kc get pod -n dc-twin-training training-0 -o 'jsonpath={.metadata.deletionTimestamp}' 2>/dev/null; }
evicted() { pod_gone || [ -n "$(terminating)" ]; }
ms=$(wait_until 240 drained) || true
record N1 "node-drainer drains it" "the workload evicted (Immediate), nvsentinel-state drain-succeeded" \
	"pod training-0 $(pod_gone && echo gone || echo "Terminating since $(terminating)"), nvsentinel-state=$(nvs_state) after ${ms} ms" "$(drained && evicted && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- R: remediation, through the server's BMC

rn() { kc get rebootnodes.janitor.dgxc.nvidia.com -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep "$NODE" | head -1; }
have_rn() { [ -n "$(rn)" ]; }
ms=$(wait_until 180 have_rn) || true
RN=$(rn) || true
record R1 "fault-remediation asks for a reboot (RESTART_BM)" "a RebootNode for $NODE" "${RN:-none}, nvsentinel-state=$(nvs_state) after ${ms} ms" "$(pass_if "$(ok have_rn)")"
signalled() { [ -n "$RN" ] && [ "$(kc get rebootnode "$RN" -o 'jsonpath={.status.conditions[?(@.type=="SignalSent")].status}')" = True ]; }
ms=$(wait_until 180 signalled) || true
labelled() { [ -s "$EV/standin.log" ]; }
wait_until 30 labelled >/dev/null || true
record R2 "janitor sends it through janitor-provider (label provider)" "SignalSent=True; Node labelled nke.nvidia.com/reboot" \
	"SignalSent=$(kc get rebootnode "${RN:-x}" -o 'jsonpath={.status.conditions[?(@.type=="SignalSent")].status}' 2>/dev/null); stand-in: $(head -1 "$EV/standin.log" 2>/dev/null | cut -c1-160)" "$( signalled && labelled && echo PASS || echo FAIL)"
rebooted() { [ -n "$(boot_id)" ] && [ "$(boot_id)" != "$BOOT0" ] && [ "$(cond Ready)" = True ]; }
ms=$(wait_until 400 rebooted) || true
standin_idle() { [ ! -e "$WORK/standin.busy" ]; }
wait_until 400 standin_idle >/dev/null || true
events --since "$SEQ0" >"$EV/R3-twin-events.txt" || true
rfreq=$(grep -c 'power.restart redfish' "$EV/R3-twin-events.txt" || true)
qmp=$(grep -c 'QMP RESET' "$EV/R3-twin-events.txt" || true)
record R3 "The reboot, as the twin sees it" "a Redfish reset of the server, QEMU resets the machine, the guest boots again, the Node's boot ID changes${BASE:+; the GPU guest reset with it}" \
	"Redfish power.restart requests $rfreq, QMP RESET $qmp, boot $BOOT0 → $(boot_id), Ready=$(cond Ready) after ${ms} ms${BASE:+; stand-in: $(grep -o 'GPU guest boot .*' "$EV/standin.log" | head -1)}" \
	"$( [ "$rfreq" -ge 1 ] && [ "$qmp" -ge 1 ] && rebooted && { [ -z "$BASE" ] || grep_q 'GPU guest boot [0-9a-f]' "$EV/standin.log"; } && echo PASS || echo FAIL)"
completed() { [ "$(kc get rebootnode "${RN:-x}" -o 'jsonpath={.status.conditions[?(@.type=="NodeReady")].status}' 2>/dev/null)" = True ]; }
ms=$(wait_until 300 completed) || true
kc get rebootnode "${RN:-x}" -o yaml >"$EV/R4-rebootnode.yaml" 2>&1 || true
record R4 "janitor sees the reboot through" "RebootNode NodeReady=True (label removed, boot ID changed, Ready)" \
	"NodeReady=$(kc get rebootnode "${RN:-x}" -o 'jsonpath={.status.conditions[?(@.type=="NodeReady")].status}' 2>/dev/null), nvsentinel-state=$(nvs_state) after ${ms} ms" "$(pass_if "$(ok completed)")"
B1=$(boot_id) || true

# ---------------------------------------------------------------- C: after a warm reboot, the GPU is still off the bus

syslog_cleared() { [ "$(cond SysLogsXIDError)" = False ]; }
ms=$(wait_until 120 syslog_cleared) || true
record C1 "After the reboot the restarted syslog-health-monitor clears its check" "SysLogsXIDError=False (a healthy event for the new boot)" \
	"SysLogsXIDError=$(cond SysLogsXIDError): $(jp '{.status.conditions[?(@.type=="SysLogsXIDError")].message}' | cut -c1-100) after ${ms} ms" "$(pass_if "$(ok syslog_cleared)")"
still=$(twin fault list "$HOST" 2>/dev/null | grep -c 'xid-79' || true)
if [ -n "$BASE" ]; then
	# the warm reboot did not bring the GPU back (the twin keeps Xid 79 until an AC cycle): NVML in
	# the rebooted GPU guest says so, and the question is whether NVSentinel's monitors do
	# what the monitors published since the machine's last reset: the GPU guest's monitor is a new
	# process then, with no memory of the fault; a fatal event of its own is detection, while a
	# quarantine still held by the condition set before the reboot is not
	SINCE=$(cat "$WORK/last-reset.pg" 2>/dev/null) || true
	gpu_gssh 'nvidia-smi --query-gpu=index,uuid,pci.bus_id --format=csv,noheader' >"$EV/C2-smi.csv" 2>&1 || true
	# the restarted monitor's first polls come 15 s apart, after its DCGM has started: three minutes
	# is many of them
	seen_again() { [ -n "$(hev --agent gpu-health-monitor --unhealthy --fatal --first)" ]; }
	ms=$(wait_until 180 seen_again) || true
	! seen_again || wait_until 60 quarantined >/dev/null || true
	if seen_again && quarantined; then res=PASS; else res=FINDING; fi
	ev=$(hev --agent gpu-health-monitor --unhealthy --first) || true
	hev --agent gpu-health-monitor >"$EV/C2-events.txt" || true
	evh=$(grep -c 'healthy=True' "$EV/C2-events.txt" || true)
	SINCE=
	gpu_gssh 'sudo docker logs dctwin-nvs-gpu-health-monitor 2>&1 | grep -i -E "supported gpus|lost|fallen|xid|incident" | tail -10' >"$EV/C2-monitor.txt" 2>&1 || true
	{ dcgmi discovery -l | grep -E 'GPUs found|^\| [0-9] '; } >"$EV/C2-dcgm.txt" 2>&1 || true
	g2h=$(grep -E "entities=GPU=${IDX79}( |,|$)" "$EV/C2-events.txt" | grep -c 'healthy=True' || true)
	record C2 "After NVSentinel's warm reboot gpu-$IDX79 is still dead in the model (Xid 79 holds until an AC cycle)" \
		"gpu-health-monitor (DCGM reading Mokka's NVML from the model) sees gpu-$IDX79 unhealthy; the node stays or is re-quarantined" \
		"active xid-79 faults: $still; DCGM: $(grep 'GPUs found' "$EV/C2-dcgm.txt" | head -1 | tr -s ' |' ' ')(gpu-$IDX79 lost); the restarted monitor's group: $(grep -o 'supported gpus are \[[0-9, ]*\]' "$EV/C2-monitor.txt" | tail -1 | sed 's/supported gpus are //'); since the reset: unhealthy ${ev:-none}, healthy events $evh, $g2h of them for gpu-$IDX79; unschedulable=$(jp '{.spec.unschedulable}'), GpuAllWatch=$(cond GpuAllWatch), RebootNodes $(rebootnodes) after ${ms} ms" "$res"
else
	record C2 "The GPU after a warm reboot" "the twin keeps Xid 79 (only an AC cycle clears it)" \
		"active xid-79 faults on the server: $still; without BASE no GPU monitor runs (the twin's guest has no NVML)" CANNOT
fi

# ---------------------------------------------------------------- A: the AC cycle that brings the GPU back

# The site's operator cycles the server's input power (dctwin power ac-cycle: the act a technician
# does at the PDU or by reseating the server), once no reboot is in progress. Xid 79 clears on it;
# the node boots, its monitors restart, and NVSentinel is expected to lift the quarantine.
nolabel() { [ -z "$(label 'nke\.nvidia\.com/reboot')" ] && standin_idle; }
wait_until 600 nolabel >/dev/null || true
SINCE=$(pg_now)
acr=$(node_reboot twin power ac-cycle "$HOST") || true
echo "$(date -u +%FT%TZ) AC cycle: $acr" >>"$EV/standin.log"
node_pods
cleared79() { [ "$(twin fault list "$HOST" 2>/dev/null | grep -c 'xid-79')" = 0 ]; }
# with BASE, also the restarted monitor's healthy events for every GPU, GPU 2 among them
all_healthy() { [ -z "$BASE" ] || [ "$(hev --agent gpu-health-monitor --healthy | grep -o 'entities=GPU=[0-9]*' | sort -u | wc -l)" -ge "$NGPU" ]; }
back_in_service() { uncordoned && all_healthy; }
ms=$(wait_until 420 back_in_service) || true
[ -z "$BASE" ] || { dcgmi discovery -l | grep -E 'GPUs found|^\| [0-9] '; } >"$EV/A1-dcgm.txt" 2>&1 || true
kc get node "$NODE" -o yaml >"$EV/A1-node.yaml"
record A1 "An AC cycle of the server (dctwin power ac-cycle) brings the GPU back; NVSentinel lifts the quarantine" \
	"Xid 79 cleared; the node's monitors report healthy; uncordoned, quarantine annotation gone" \
	"$acr; active xid-79: $(cleared79 && echo none || echo still); ${BASE:+DCGM: $(grep 'GPUs found' "$EV/A1-dcgm.txt" 2>/dev/null | head -1 | tr -s ' |' ' '), healthy events since from gpu-health-monitor for $(hev --agent gpu-health-monitor --healthy | grep -o 'entities=GPU=[0-9]*' | sort -u | wc -l) GPUs; }unschedulable=$(jp '{.spec.unschedulable}'), SysLogsXIDError=$(cond SysLogsXIDError)${BASE:+, GpuAllWatch=$(cond GpuAllWatch)}, uncordon-by=$(label 'k8saas\.nvidia\.com/uncordon-by') after ${ms} ms" \
	"$( cleared79 && back_in_service && echo PASS || echo FAIL)"
SINCE=

# ---------------------------------------------------------------- C3, G5: Xid 48

# A fault logged between a boot and the monitor's start: syslog-health-monitor's post-reboot scan
# reads the current boot's journal from its start, "so entries emitted between boot and monitor
# startup are not missed". The harness reboots the server through its BMC as an operator would,
# injects an Xid 48 on another GPU while the syslog monitor is down, then starts the monitors as
# the kubelet would. With BASE, the gpu-health-monitor sees the same Xid through DCGM.
wait_until 600 nolabel >/dev/null || true
B2=$(boot_id) || true
SEQ2=$(since_seq) || true
SINCE=$(pg_now)
c3r=$(node_reboot force_restart) || true
echo "$(date -u +%FT%TZ) C3 reboot: $c3r" >>"$EV/standin.log"
twin fault inject "$HOST/$GPU48" nvidia/xid/xid-48 >"$EV/C3-inject.json"
recorded48() { events --kind observe --since "$SEQ2" | grep_q 'kernelLog: .* → recorded.*: 48,'; }
wait_until 60 recorded48 >/dev/null || true
node_pods
seen48() { [ -n "$(xid_event 48)" ]; }
ms=$(wait_until 90 seen48) || true
skipped=$(docker logs dctwin-nvs-syslog-health-monitor 2>&1 | grep -c '"msg":"Finished processing boot-filtered journal entries"' || true)
if seen48; then res=PASS; else res=FINDING; fi
record C3 "An Xid 48 logged after a reboot, before the monitor starts" "reported by syslog-health-monitor's post-reboot scan of the current boot" \
	"boot $B2 → $(boot_id); line in the kernel log: $(recorded48 && echo yes || echo no); event: $(xid_event 48 | cut -c1-120)${ms:+ after ${ms} ms}; boot-filtered scans finished $skipped" "$res"
if [ -n "$BASE" ]; then
	IDX48=${GPU48#gpu-}
	dcgm48() { [ -n "$(hev --agent gpu-health-monitor --unhealthy --first)" ]; }
	ms=$(wait_until 240 dcgm48) || true
	hev --agent gpu-health-monitor --unhealthy >"$EV/G5-events.txt" || true
	gpu_gssh 'sudo docker logs dctwin-nvs-gpu-health-monitor 2>&1 | grep -i -E "xid|dbe|ecc|incident" | tail -8' >"$EV/G5-monitor.txt" 2>&1 || true
	record G5 "gpu-health-monitor raises its DCGM health event for the Xid 48 (GPU $IDX48)" \
		"an unhealthy event for GPU $IDX48: DCGM_FR_XID_ERROR (Xid 48) and/or DCGM_FR_VOLATILE_DBE_DETECTED (Mokka's ECC counters)" \
		"$(head -3 "$EV/G5-events.txt" | cut -c1-200 | tr '\n' ' ')$( [ -s "$EV/G5-events.txt" ] || echo none) after ${ms} ms" \
		"$( grep -q "entities=GPU=$IDX48" "$EV/G5-events.txt" && echo PASS || echo FAIL)"
	ms=$(wait_until 120 quarantined) || true
	# DCGM_FR_XID_ERROR is NONE in the chart's mapping (not fatal); a fatal event is what quarantines
	fatal48=$(grep -c 'fatal=True' "$EV/G5-events.txt" || true)
	if quarantined; then res=PASS; elif [ "$fatal48" = 0 ]; then res=FINDING; else res=FAIL; fi
	record Q2 "fault-quarantine acts on it as before" "the Node cordoned again (on a fatal event)" \
		"unschedulable=$(jp '{.spec.unschedulable}'), quarantineHealthEvent $( [ -n "$(jp '{.metadata.annotations.quarantineHealthEvent}')" ] && echo set || echo absent), fatal DCGM events $fatal48, nvsentinel-state=$(nvs_state) after ${ms} ms" "$res"
fi
SINCE=
twin fault clear "$HOST/$GPU" nvidia/xid/xid-79 >/dev/null 2>&1 || true
twin fault clear "$HOST/$GPU48" nvidia/xid/xid-48 >/dev/null 2>&1 || true

# ---------------------------------------------------------------- result

print_table "NVSentinel $NVS_REF ($NVS_COMMIT) against the twin's $HOST (vm, os: twin; Node impersonated, D24)${BASE:+; GPUs through Mokka in a second guest}" | tee "$WORK/results.md"
exit
}

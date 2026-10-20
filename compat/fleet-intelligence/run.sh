#!/usr/bin/env bash
# Runs NVIDIA's Fleet Intelligence agent (fleetint), unmodified and installed from its released
# package, against GPUs and GPU faults that are the twin's.
#
# Two guests, both real kernels under KVM:
#
#   - the twin's own: the estate's Dell XE9680 (b01/server-01, 8 × H100) at `vm` fidelity with
#     `os: twin` (compat/fleet-intelligence/estate.yaml), whose in-host agent (internal/inhost)
#     printk's a fault catalogue's kernel-log lines into that guest's kernel through its kernel
#     module (dctwin-kmsg, built here for that kernel: facility kern, D50) and carries the records
#     back out (state/inhost/<host>/kmsg.log). It is Alpine's netboot initramfs (musl), with no way
#     in for a cgo, glibc agent.
#   - the fleetint guest: Debian 13 (glibc), started here with QEMU user-mode networking, in which
#     Mokka (NVIDIA's nvml-mock 0.5.0) presents the same server's GPUs to NVML, from a profile
#     `dctwin-mokka render` writes from the model, and fleetint 1.5.0 runs from its .deb under its
#     own systemd unit, in offline mode, unenrolled (guest-setup.sh).
#
# A fault the twin injects reaches fleetint twice: through NVML, because `dctwin-mokka sync` keeps
# the guest's Mokka override document equal to the model (an Xid 79 makes the GPU fall off the bus
# in NVML); and through the kernel log, because the records the twin's guest kernel recorded are
# entered into the fleetint guest's kernel through the same module, built there for that kernel
# (kmsg-relay.py, the one stand-in: its last hop). Where a kernel has no module, the line goes
# through that kernel's /dev/kmsg (facility user), and the results say so (KMOD=0 forces it for
# the twin's guest).
#
#   BASE=/path/debian-13-genericcloud-amd64.qcow2 compat/fleet-intelligence/run.sh
#   KEEP=1 …     leave both guests, the twin, the sync and the relay running (prints how to stop them)
#   KEEP=wait …  the same until $WORK/release exists, then tear down (for a caller holding a lock)
#
#   BASE        a Debian 13 genericcloud qcow2 (cloud.debian.org, checked against its SHA512SUMS by
#               whoever fetched it); only read: the guest writes a qcow2 overlay in $WORK/guest
#   WORK        scratch (default mktemp -d); evidence under $WORK/evidence, the table in
#               $WORK/results.md
#   CPUS=2 MEM=3072 DISK=12G   the fleetint guest's size (the twin's guest is the estate's, 1 GiB)
#   HOST=b01/server-01 GPU79=gpu-2 GPU48=gpu-5   the twin host and the GPUs the faults go on
#   MOKKA_DIR   an extracted nvml-mock image (bin/, lib/ of its /usr/local), instead of extracting
#               it here with Podman or Docker
#
# Needs Linux with KVM, qemu-system-x86_64 and qemu-img, OVMF and swtpm (the twin's vm adapter),
# Go, python3, curl, ssh, gpg (optional: the release's signature), and Podman or Docker unless
# MOKKA_DIR is given. Nothing on the host's network or firewall changes: the fleetint guest has
# QEMU's user-mode network with one loopback port forwarded to its ssh; the twin's guest is on the
# twin's own loopback substrate. Run it under whatever lock the host's owner asks for: the guests
# hold about 4.5 GiB for the run's whole life.
set -euo pipefail
{ # the whole script is one compound command, read before any of it runs: a sync of the repository
# while the guests run cannot change what this process does next
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)

# ---------------------------------------------------------------- pins
FLEETINT_VER=1.5.0
FLEETINT_COMMIT=a83abe929ed8fa937a474b104fe197c0dbd116fc   # github.com/dsx-ai-factory/fleet-intelligence-agent v1.5.0
FLEETINT_REL=https://github.com/dsx-ai-factory/fleet-intelligence-agent/releases/download/v$FLEETINT_VER
FLEETINT_DEB=fleetint_${FLEETINT_VER}_amd64.deb
FLEETINT_SHA=cd5136e1747a82197dac8906edf4e6b76fea6981d6e28f282225d34c725fa298
# the release's signing key (fleet-intelligence.pub.asc): "NVIDIA Fleet Intelligence <fleetint@exchange.nvidia.com>"
FLEETINT_KEY=FE0C8B74CA66357C13BE197DCCE3C963087199B3
# nvml-mock 0.5.0 (k8s-test-infra v0.5.0, e2950d58), the multi-architecture index
MOKKA_IMAGE=ghcr.io/nvidia/nvml-mock@sha256:9583cc6e9ce53ff54bda4f34072fd64460364c907a4af17419fffec36dac6238
MOKKA_COMMIT=e2950d58bdace638f3a522005978cc278ffa95d7
MOKKA_PROFILES=https://raw.githubusercontent.com/NVIDIA/k8s-test-infra/$MOKKA_COMMIT/deployments/nvml-mock/helm/nvml-mock/profiles
declare -A MOKKA_PROFILE_SHA=([h100]=5121352b0b340ef19bdacdbb8322b268d7121f3b70c3690b717c6cab458840b2)

: "${BASE:?BASE: a Debian 13 genericcloud qcow2}"
WORK=${WORK:-$(mktemp -d)}
ESTATE=${ESTATE:-$here/estate.yaml}
HOST=${HOST:-b01/server-01}
GPU79=${GPU79:-gpu-2}
GPU48=${GPU48:-gpu-5}
CPUS=${CPUS:-2} MEM=${MEM:-3072} DISK=${DISK:-12G}
ADAPTERS=${ADAPTERS:-vm,inhost}
# what a previous run in the same WORK left is kept only where it is a cache (bin, dl, assets)
rm -rf "$WORK/evidence" "$WORK/stage" "$WORK/render" "$WORK/relay.log" "$WORK/sync.log" "$WORK/results.md"
mkdir -p "$WORK/bin" "$WORK/dl" "$WORK/evidence" "$WORK/guest" "$WORK/stage"
WORK=$(cd "$WORK" && pwd)
EV=$WORK/evidence G=$WORK/guest

. "$root/compat/kai/lib/common.sh"
. "$root/compat/kai/lib/twin.sh"

[ "$(uname -s)" = Linux ] && [ -w /dev/kvm ] || { echo "needs Linux with KVM (both guests are x86_64 under KVM)" >&2; exit 1; }
for t in qemu-system-x86_64 qemu-img go python3 curl ssh ssh-keygen sha256sum tar; do
	command -v "$t" >/dev/null || { echo "$t not found" >&2; exit 1; }
done
ENGINE=
if [ -z "${MOKKA_DIR:-}" ]; then
	for e in podman docker; do command -v $e >/dev/null && { ENGINE=$e; break; }; done
	[ -n "$ENGINE" ] || { echo "Podman or Docker is needed to extract nvml-mock's image (or set MOKKA_DIR)" >&2; exit 1; }
fi
# no port this harness opens may be one of these (other work on a shared host)
AVOID=" ${AVOID_PORTS:-5556 8080 5173 6443 5432 18443 16385 11079 22 10250 2379 2380 9446} "
port() { local p; while :; do p=$(free_port); case "$AVOID" in *" $p "*) ;; *) echo "$p"; return ;; esac; done; }

# ---------------------------------------------------------------- build and fetch
(cd "$root" && CGO_ENABLED=0 go build -o "$WORK/bin/dctwin" ./cmd/dctwin && CGO_ENABLED=0 go build -o "$WORK/bin/dctwin-mokka" ./cmd/dctwin-mokka)
DCTWIN=$WORK/bin/dctwin

# fleetint's package, by its pinned digest; the release's checksums.txt and its signature, when gpg is here
FD=$WORK/dl/fleetint-$FLEETINT_VER
mkdir -p "$FD"
for f in "$FLEETINT_DEB" checksums.txt checksums.txt.asc fleet-intelligence.pub.asc; do
	[ -s "$FD/$f" ] || curl -sfL -o "$FD/$f" "$FLEETINT_REL/$f"
done
[ "$(sha256sum "$FD/$FLEETINT_DEB" | cut -d' ' -f1)" = "$FLEETINT_SHA" ] || { echo "$FLEETINT_DEB: sha256 differs from the pin" >&2; exit 1; }
grep -q "^$FLEETINT_SHA  $FLEETINT_DEB\$" "$FD/checksums.txt" || { echo "the release's checksums.txt does not list the pinned $FLEETINT_DEB" >&2; exit 1; }
SIG="gpg absent: the package checked against the pinned sha256 and the release's checksums.txt only"
if command -v gpg >/dev/null; then
	gh=$(mktemp -d)
	GNUPGHOME=$gh gpg -q --import "$FD/fleet-intelligence.pub.asc" 2>/dev/null
	if GNUPGHOME=$gh gpg --status-fd 1 --verify "$FD/checksums.txt.asc" "$FD/checksums.txt" 2>/dev/null | grep -q "VALIDSIG $FLEETINT_KEY"; then
		SIG="checksums.txt signed by $FLEETINT_KEY (the release's key)"
	else
		echo "checksums.txt: no valid signature by $FLEETINT_KEY" >&2; rm -rf "$gh"; exit 1
	fi
	rm -rf "$gh"
fi

# Mokka: its released image's /usr/local (node agent, nvml-mock-ctl, nvidia-smi, the mock libnvidia-ml)
MOKKA_PULLED=0
if [ -z "${MOKKA_DIR:-}" ]; then
	MOKKA_DIR=$WORK/dl/mokka
	if [ ! -x "$MOKKA_DIR/bin/node-agent" ]; then
		$ENGINE image inspect "$MOKKA_IMAGE" >/dev/null 2>&1 || { $ENGINE pull -q --platform linux/amd64 "$MOKKA_IMAGE" >/dev/null; MOKKA_PULLED=1; }
		rm -rf "$MOKKA_DIR" && mkdir -p "$MOKKA_DIR"
		$ENGINE rm -f dctwin-fleetint-mokka >/dev/null 2>&1 || true
		$ENGINE create --name dctwin-fleetint-mokka "$MOKKA_IMAGE" true >/dev/null
		$ENGINE cp dctwin-fleetint-mokka:/usr/local/bin "$MOKKA_DIR/bin"
		$ENGINE cp dctwin-fleetint-mokka:/usr/local/lib "$MOKKA_DIR/lib"
		$ENGINE rm dctwin-fleetint-mokka >/dev/null
		[ "$MOKKA_PULLED" = 1 ] && [ "${KEEP_IMAGES:-}" != 1 ] && $ENGINE image rm "$MOKKA_IMAGE" >/dev/null 2>&1 || true
	fi
fi
mkdir -p "$WORK/dl/mokka-profiles"
for b in "${!MOKKA_PROFILE_SHA[@]}"; do
	pf=$WORK/dl/mokka-profiles/$b.yaml
	[ -s "$pf" ] || curl -sfL -o "$pf" "$MOKKA_PROFILES/$b.yaml"
	[ "$(sha256sum "$pf" | cut -d' ' -f1)" = "${MOKKA_PROFILE_SHA[$b]}" ] || { echo "Mokka profile $b: sha256 differs from the pin" >&2; exit 1; }
done

# ---------------------------------------------------------------- the twin's host, rendered for Mokka
"$WORK/bin/dctwin-mokka" render "$ESTATE" --select "$HOST" --base "$WORK/dl/mokka-profiles" --out "$WORK/render" >"$EV/render.txt"
read -r NODE HOST_PATH NGPU < <(python3 -I -c 'import json,sys; h=json.load(open(sys.argv[1]))[0]; print(h["Node"], h["Path"], len(h["GPUs"]))' "$WORK/render/hosts.json")
gpu_of() { python3 -I -c 'import json,sys
for g in json.load(open(sys.argv[1]))[0]["GPUs"]:
    if g["Path"].endswith("/"+sys.argv[2]): print(g["Index"], g["UUID"], g["BusID"].lower())' "$WORK/render/hosts.json" "$1"; }
read -r IDX79 UUID79 BUS79 < <(gpu_of "$GPU79")
read -r IDX48 UUID48 BUS48 < <(gpu_of "$GPU48")
DRIVER_VERSION=$(sed -n 's/^ *driver_version: *"\{0,1\}\([0-9.]*\)"\{0,1\} *$/\1/p' "$WORK/render/$NODE.yaml" | head -1)
S=$WORK/stage
mkdir -p "$S/mokka/bin" "$S/mokka/lib"
cp "$MOKKA_DIR/bin/node-agent" "$MOKKA_DIR/bin/nvml-mock-ctl" "$MOKKA_DIR/bin/nvidia-smi" "$S/mokka/bin/"
for l in libnvidia-ml libcuda; do cp "$(ls "$MOKKA_DIR"/lib/$l.so.*.*.* | grep -v '\.h$' | head -1)" "$S/mokka/lib/"; done
cp "$WORK/render/$NODE.yaml" "$S/config.yaml"
cp "$FD/$FLEETINT_DEB" "$here/guest-setup.sh" "$S/"
mkdir -p "$S/kmod" && cp "$root/internal/inhost/kmod/Kbuild" "$root/internal/inhost/kmod/dctwin_kmsg.c" "$root/internal/inhost/kmod/guest-load.sh" "$S/kmod/"
printf 'NODE_NAME=%s\nGPU_COUNT=%s\nDRIVER_VERSION=%s\n' "$NODE" "$NGPU" "$DRIVER_VERSION" >"$S/guest.env"

# ---------------------------------------------------------------- the twin, with the XE9680's own guest
# The twin's assets: the user's cache, except that the guest image's directories are this run's
# own, because the twin writes its initrd (its /init with the in-host agent) beside the image;
# whatever is missing (the guest image for x86_64) is fetched into it (compat/nvsentinel does the same).
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
	HOME="$WORK" "$DCTWIN" assets fetch >"$WORK/assets-fetch.log" 2>&1 || { cat "$WORK/assets-fetch.log" >&2; exit 1; }
fi
# The in-host agent's kernel module (internal/inhost/kmod, D50), built for the twin's guest kernel
# and put beside its image, where the vm adapter adds it to the guest's initramfs: the agent then
# printk's a fault's lines (facility kern). KMOD=0 leaves it out, and the agent writes through
# /dev/kmsg instead (facility user, its journal entries marked), which the results say.
GUEST_DIR=$DCTWIN_ASSETS/alpine-netboot@3.24.2-x86_64
if [ "${KMOD:-1}" = 1 ]; then
	ENGINE=$ENGINE "$root/internal/inhost/kmod/build.sh" "$GUEST_DIR" 2>"$EV/kmod-build.txt" || { cat "$EV/kmod-build.txt" >&2; exit 1; }
elif [ "$DCTWIN_ASSETS" = "$WORK/assets" ]; then
	rm -f "$GUEST_DIR/dctwin-kmsg.ko" "$GUEST_DIR/dctwin-kmsg.ko.txt"
fi

TWIN_PID= SYNC_PID= RELAY_PID= QEMU_PID= SEED_PID=
gssh() { ssh -q -i "$G/id_ed25519" -p "$(cat "$G/ssh.port")" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
	-o BatchMode=yes -o ConnectTimeout=10 debian@127.0.0.1 "$@"; }
collect() {
	set +e
	[ -n "${TWIN_STATE:-}" ] && { twin events --select "$HOST_PATH" >"$EV/twin-events.txt" 2>/dev/null; twin report >"$EV/fidelity-report.txt" 2>/dev/null
		cp "$TWIN_STATE/inhost/${HOST_PATH//\//_}/kmsg.log" "$EV/twin-guest-kmsg.log" 2>/dev/null; }
	cp "$WORK/sync.log" "$WORK/relay.log" "$EV/" 2>/dev/null
	cp "$G/console.log" "$EV/fleetint-guest-console.log" 2>/dev/null
	if [ -n "$QEMU_PID" ] && kill -0 "$QEMU_PID" 2>/dev/null; then
		gssh 'sudo journalctl -b --no-pager -u fleetintd -u dctwin-nvml-mock' >"$EV/guest-journal.txt" 2>&1
		gssh 'sudo dmesg' >"$EV/guest-dmesg.txt" 2>&1
		gssh 'sudo tar -C /var/lib -cf - fleetint-offline 2>/dev/null' | tar -C "$EV" -xf - 2>/dev/null
	fi
}
shutdown_guest() {
	if [ -n "$QEMU_PID" ] && kill -0 "$QEMU_PID" 2>/dev/null; then
		gssh sudo systemctl poweroff >/dev/null 2>&1
		for _ in $(seq 1 60); do kill -0 "$QEMU_PID" 2>/dev/null || break; sleep 1; done
		kill "$QEMU_PID" 2>/dev/null
	fi
	[ -n "$SEED_PID" ] && kill "$SEED_PID" 2>/dev/null
	rm -f "$G/overlay.qcow2"
}
cleanup() {
	set +e
	collect
	if [ "${KEEP:-}" = wait ]; then
		echo "KEEP=wait: ssh -i $G/id_ed25519 -p $(cat "$G/ssh.port" 2>/dev/null) debian@127.0.0.1; DCTWIN_STATE=${TWIN_STATE:-} HOME=${TWIN_HOME:-} $DCTWIN events; touch $WORK/release to tear down" >&2
		local i
		for i in $(seq 1 1440); do [ -e "$WORK/release" ] && break; sleep 10; done
		rm -f "$WORK/release"
		collect
	elif [ "${KEEP:-}" = 1 ]; then
		echo "KEEP=1: left running. Stop with: kill ${SYNC_PID:-} ${RELAY_PID:-} ${SEED_PID:-}; DCTWIN_STATE=${TWIN_STATE:-} HOME=${TWIN_HOME:-} $DCTWIN down; kill ${QEMU_PID:-}; rm $G/overlay.qcow2" >&2
		return
	fi
	for p in $SYNC_PID $RELAY_PID; do kill "$p" 2>/dev/null && wait "$p" 2>/dev/null; done
	twin_down
	shutdown_guest
}
trap cleanup EXIT
trap 'exit 130' INT TERM

rm -rf "$WORK/twin"
twin_up "$ESTATE" "$WORK/twin" "$ADAPTERS"
INHOST_DIR=$TWIN_STATE/inhost/${HOST_PATH//\//_}
events() { twin events --select "$HOST_PATH" "$@" 2>/dev/null; }
since_seq() { twin events 2>/dev/null | tail -1 | awk '{print substr($1, 2)}'; }
echo "twin up: $(grep -o 'control API: [^ ]*' "$WORK/twin/up.log"), state $TWIN_STATE" >&2

# ---------------------------------------------------------------- the fleetint guest
[ -f "$G/id_ed25519" ] || ssh-keygen -q -t ed25519 -N '' -C dctwin-fleetint -f "$G/id_ed25519"
mkdir -p "$G/seed"
printf 'instance-id: dctwin-fleetint-%s\nlocal-hostname: %s\n' "$(date +%s)" "$NODE" >"$G/seed/meta-data"
cat >"$G/seed/user-data" <<EOF
#cloud-config
users:
  - name: debian
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys: ["$(cat "$G/id_ed25519.pub")"]
growpart: { mode: auto, devices: ["/"] }
runcmd:
  - [touch, /var/lib/dctwin-ready]
EOF
rm -f "$G/overlay.qcow2"
qemu-img create -q -f qcow2 -b "$BASE" -F qcow2 "$G/overlay.qcow2" "$DISK"
port >"$G/ssh.port"
HTTP_PORT=$(port)
python3 -I -m http.server --bind 127.0.0.1 --directory "$G/seed" "$HTTP_PORT" >"$G/seed.log" 2>&1 &
SEED_PID=$!
# NoCloud from the host's loopback, which user-mode networking presents to the guest at 10.0.2.2
qemu-system-x86_64 -name dctwin-fleetint-guest -machine q35,accel=kvm -cpu host -smp "$CPUS" -m "$MEM" \
	-drive "file=$G/overlay.qcow2,if=virtio,format=qcow2,discard=unmap" \
	-nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$(cat "$G/ssh.port")-:22" \
	-smbios "type=1,serial=ds=nocloud;s=http://10.0.2.2:$HTTP_PORT/" \
	-display none -serial "file:$G/console.log" &
QEMU_PID=$!
echo "fleetint guest booting (ssh 127.0.0.1:$(cat "$G/ssh.port"), console $G/console.log)" >&2
ready() { gssh test -e /var/lib/dctwin-ready; }
wait_until 600 ready >/dev/null || { echo "the fleetint guest did not finish cloud-init; see $G/console.log" >&2; exit 1; }
kill "$SEED_PID" 2>/dev/null; SEED_PID=
gssh 'sudo rm -rf ~/stage; mkdir -p ~/stage' && tar -C "$S" -cf - . | gssh 'tar -C ~/stage -xf -'
gssh 'sudo sh ~/stage/guest-setup.sh /home/debian/stage' >"$EV/guest-setup.log" 2>&1 || { cat "$EV/guest-setup.log" >&2; exit 1; }
gssh 'echo "$(. /etc/os-release; echo $PRETTY_NAME), kernel $(uname -r), glibc $(ldd --version | head -1 | grep -o "[0-9.]*$"), $(nproc) vCPU, $(free -m | awk "/^Mem:/ {print \$2}") MiB"' >"$EV/guest.txt" 2>&1
export FI_GUEST_KEY=$G/id_ed25519 FI_GUEST_PORT=$(cat "$G/ssh.port") FI_GUEST_NODE=$NODE

# ---------------------------------------------------------------- the twin's paths into it
attached() { events --kind observe | grep 'inhost.agent' | grep -q attached; }
wait_until 240 attached >/dev/null || true
"$WORK/bin/dctwin-mokka" sync "$ESTATE" --select "$HOST" --control "$TWIN_STATE/control.json" -- "$here/put-override.sh" '{node}' \
	>"$WORK/sync.log" 2>&1 &
SYNC_PID=$!
python3 -I "$here/kmsg-relay.py" "$INHOST_DIR/kmsg.log" "$WORK/relay.log" -- \
	ssh -q -i "$G/id_ed25519" -p "$(cat "$G/ssh.port")" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes debian@127.0.0.1 &
RELAY_PID=$!
applied_lines() { grep -c "applied to $NODE" "$WORK/sync.log" || true; }
first_applied() { [ "$(applied_lines)" -ge 1 ]; }
wait_until 60 first_applied >/dev/null || true

# ---------------------------------------------------------------- helpers
fget() { gssh sudo fleetint-get "$1"; }
CHK="python3 -I $here/check.py"
recordall() { while IFS= read -r l; do IFS='|' read -r a b c d e <<<"$l"; record "$a" "$b" "$c" "$d" "$e"; done; }
XIDC=accelerator-nvidia-error-xid NVMLC=accelerator-nvidia-nvml
# state <component> <file>: fleetint's state of one component now, saved; prints health, reason,
# suggested actions, incidents and time, separated by \x1f (check.py)
state() { fget "/v1/states?components=$1" >"$EV/$2" 2>/dev/null; $CHK state "$EV/$2" "$1"; }
health() { state "$1" "${2:-.state-poll.json}" | cut -d$'\x1f' -f1; }
xid_health_is() { [ "$(health $XIDC)" = "$1" ]; }
smi() { gssh 'nvidia-smi --query-gpu=index,name,uuid,serial,pci.bus_id,memory.total,vbios_version --format=csv,noheader,nounits' 2>&1; }
api_up() { fget /healthz >/dev/null; }
boot_id() { gssh cat /proc/sys/kernel/random/boot_id 2>/dev/null; }
relayed() { [ -s "$WORK/relay.log" ] && python3 -I -c 'import json,sys
n=sum(1 for l in open(sys.argv[1]) if json.loads(l)["ok"] and (": %s, " % sys.argv[2]) in json.loads(l)["wrote"])
sys.exit(0 if n >= 1 else 1)' "$WORK/relay.log" "$1"; }

# ---------------------------------------------------------------- B: the GPUs, and a healthy baseline
gssh 'fleetint --version 2>/dev/null' | grep -m1 '^fleetint v' >"$EV/fleetint-version.txt" || true
smi >"$EV/B1-smi.csv" || true
recordall < <($CHK smi "$WORK/render/hosts.json" "$EV/B1-smi.csv")
wait_until 180 api_up >/dev/null || true
ok=1; api_up && [ "$(gssh systemctl is-active fleetintd)" = active ] && ok=0
record B2 "fleetint $FLEETINT_VER ($FLEETINT_COMMIT) from its .deb, under its own unit, offline mode" "fleetintd active; local API on /run/fleetint/fleetint.sock" \
	"fleetintd $(gssh systemctl is-active fleetintd); $(head -1 "$EV/fleetint-version.txt"); $(cat "$EV/guest.txt"); $SIG" "$(pass_if $ok)"
gssh 'sudo timeout 180 fleetint precheck' >"$EV/precheck.txt" 2>&1 || true
gssh 'sudo fleetint machine-info 2>/dev/null' >"$EV/B3-machine-info.txt" || true
recordall < <($CHK gpus "$WORK/render/hosts.json" "$EV/B3-machine-info.txt")
present() { [ "$(health $XIDC)" != absent ]; }
wait_until 120 present >/dev/null || true
fget /v1/states >"$EV/B5-states.json" 2>&1 || true
$CHK components "$EV/B5-states.json" >"$EV/B5-components.txt" 2>&1 || true
IFS=$'\x1f' read -r h r _ _ < <(state $XIDC B5-xid.json) || true
IFS=$'\x1f' read -r hn rn _ _ < <(state $NVMLC B5-nvml.json) || true
record B5 "fleetint's Xid and NVML components on the healthy twin" "Healthy; NVML loaded, the Xid check on (not \"NVIDIA NVML library is not loaded\")" \
	"$XIDC: $h, \"$r\"; $NVMLC: $hn, \"$rn\"" "$( [ "$h" = Healthy ] && [ "$hn" = Healthy ] && [ "${r#*not loaded}" = "$r" ] && echo PASS || echo FAIL)"
IFS=$'\x1f' read -r h r _ _ < <(state library B6-library.json) || true
record B6 "fleetint's library component: the driver's user-space libraries" "Healthy: libnvidia-ml.so.1 and libcuda.so.1 (Mokka's) where a driver install puts them" "$h, \"$r\"" "$( [ "$h" = Healthy ] && echo PASS || echo FAIL)"
gssh 'sudo timeout 180 fleetint scan' >"$EV/B7-scan.txt" 2>&1 || true

# ---------------------------------------------------------------- X: Xid 79 on one GPU
SEQ0=$(since_seq) || true
a0=$(applied_lines)
twin fault inject "$HOST/$GPU79" nvidia/xid/xid-79 >"$EV/X-inject.json"
FID79=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))[0]["ID"])' "$EV/X-inject.json" 2>/dev/null || true)
recorded79() { events --kind observe --since "$SEQ0" | grep 'kernelLog: .* → recorded' | grep -q ': 79,'; }
ms=$(wait_until 60 recorded79) || true
events --kind observe --since "$SEQ0" | grep 'kernelLog' >"$EV/X1-twin-kernellog.txt" || true
record X1 "dctwin fault inject $HOST/$GPU79 nvidia/xid/xid-79: the in-host agent writes the catalogue's lines into the twin's guest kernel" \
	"both lines (GPU at …, Xid 79) recorded by that kernel" "$(grep -c '→ recorded' "$EV/X1-twin-kernellog.txt") recorded after ${ms} ms: $(grep '→ recorded' "$EV/X1-twin-kernellog.txt" | sed 's/.*holds the fault.s line //' | tr '\n' ' ' | cut -c1-300)" \
	"$(pass_if "$(recorded79 && echo 0 || echo 1)")"
ms=$(wait_until 60 relayed 79) || true
gssh 'sudo dmesg -r' | grep 'NVRM:' >"$EV/X2-guest-kmsg.txt" 2>&1 || true
via=$(python3 -I -c 'import json,sys; print(" ".join(sorted({json.loads(l).get("via") or "?" for l in open(sys.argv[1])})))' "$WORK/relay.log" 2>/dev/null) || true
record X2 "STAND-IN: the twin guest's records entered into the fleetint guest's kernel (kmsg-relay.py, through its dctwin-kmsg)" "the same text, same level, in the fleetint guest's kernel log" \
	"$(grep -c 'NVRM:' "$EV/X2-guest-kmsg.txt") NVRM records in its ring buffer ${ms} ms after, written to ${via:-nothing}; $(tail -1 "$EV/X2-guest-kmsg.txt" | cut -c1-200)" \
	"$(pass_if "$(relayed 79 && grep -q ": 79, " "$EV/X2-guest-kmsg.txt" && echo 0 || echo 1)")"
doc79() { [ "$(applied_lines)" -gt "$a0" ] && tail -n 20 "$WORK/sync.log" | grep -q fallen_off_bus; }
ms=$(wait_until 60 doc79) || true
sleep 2   # one override TTL
smi >"$EV/X3-smi.csv" || true
record X3 "the same fault reaches the guest's NVML (dctwin-mokka sync → Mokka's override document)" "{mode: fallen_off_bus, xid 79} on GPU $IDX79; nvidia-smi: GPU $IDX79 lost, the others answer" \
	"applied ${ms} ms after; nvidia-smi: $(grep -ci 'lost' "$EV/X3-smi.csv") of $(grep -c 'GPU-' "$EV/X3-smi.csv") rows lost: $(grep -i 'lost' "$EV/X3-smi.csv" | head -1)" \
	"$( doc79 && grep "^$IDX79," "$EV/X3-smi.csv" | grep -qi 'lost' && [ "$(grep -ci 'lost' "$EV/X3-smi.csv")" = 1 ] && echo PASS || echo FAIL)"
ms=$(wait_until 90 eval '! xid_health_is Healthy') || true
IFS=$'\x1f' read -r h r a _ < <(state $XIDC X4-xid.json) || true
record X4 "fleetint's Xid component (accelerator-nvidia-error-xid) reports it" "Unhealthy, Xid 79 on PCI:${BUS79%.0} = $UUID79 (the model's $GPU79)" \
	"$h after ${ms} ms: \"$r\"; suggested: ${a:-none}" "$( [ "$h" != Healthy ] && [ "${r#*79}" != "$r" ] && [ "${r#*$UUID79}" != "$r" ] && echo PASS || echo FAIL)"
fget "/v1/events?components=$XIDC" >"$EV/X5-events.json" 2>&1 || true
IFS=$'\x1f' read -r et ed em ek < <($CHK xidevent "$EV/X5-events.json" 79) || true
record X5 "fleetint's error_xid event" "Xid 79 for PCI:${BUS79%.0}, from the kernel line" "type ${et:-none}, device ${ed:-none}: \"${em:-}\"; raw kmsg \"${ek:-}\"" \
	"$( [ -n "$et" ] && [ "${ek#*: 79, }" != "$ek" ] && echo PASS || echo FAIL)"
# fleetint's NVML component reports the errors of its machine-info collection, which its exporter
# refreshes at most every five minutes (internal/exporter/collector/machine_info_provider.go), longer
# than this fault lasts; and that collection reads identity of every GPU (UUID, serial, PCI, VBIOS,
# name, index) and memory of the first GPU that answers, while Mokka's failure guards cover
# measurement getters (memory, temperature, power, clocks, ECC) and not identity
IFS=$'\x1f' read -r h r _ inc t < <(state $NVMLC X6-nvml.json) || true
if [ "$h" = Unhealthy ] && [ "${inc#*$UUID79}" != "$inc" ]; then res=PASS; else res=FINDING; fi
record X6 "fleetint's NVML component (collection errors of its machine-info) while the GPU is lost" "collection errors naming $UUID79" \
	"$h, \"$r\", last evaluated $t${inc:+; ${inc:0:200}}: machine info is refreshed at most every 5 min, and reads identity of every GPU, which Mokka 0.5.0 answers for a lost GPU, and memory of the first that answers" "$res"
gssh 'sudo timeout 180 fleetint scan' >"$EV/X7-scan.txt" 2>&1 || true
found=$(grep -a -o 'matched [0-9]* xid errors from [0-9]* kmsg(s)' "$EV/X7-scan.txt" | head -1 || true)
record X7 "fleetint scan (its one-shot check of the host) while the fault is active" "the Xid 79 found in the kernel log, Fatal" \
	"${found:-no xid line}: $(grep -a '| 79 ' "$EV/X7-scan.txt" | head -1 | tr -s ' ' | sed 's/^ *| *//; s/ *| *$//; s/ *| */, /g' | cut -c1-160)" \
	"$( grep -aq '| 79 .*Fatal' "$EV/X7-scan.txt" && echo PASS || echo FAIL)"
fget /v1/states >"$EV/X-states.json" 2>&1 || true

# ---------------------------------------------------------------- M: Mokka's node agent restarts with the fault still active
# The node agent clears the override document when it starts (a node pod restart, here a service
# restart): the document is replaced behind the twin's back, and the sync's resync must restore it.
gssh 'sudo systemctl restart dctwin-nvml-mock' >/dev/null 2>&1 || true
gssh 'sudo cat /var/lib/nvml-mock/driver/config/overrides.yaml 2>/dev/null' >"$EV/M1-after-restart.yaml" || true
restored() { gssh 'sudo cat /var/lib/nvml-mock/driver/config/overrides.yaml 2>/dev/null' | grep -q fallen_off_bus; }
ms=$(wait_until 40 restored) && ok=0 || ok=1
sleep 2   # one override TTL
smi >"$EV/M1-smi.csv" || true
lost=$(grep "^$IDX79," "$EV/M1-smi.csv" | grep -ci lost || true)
record M1 "Mokka's node agent restarts while the twin holds Xid 79 (it clears the override document)" "the sync's resync (10 s) restores {fallen_off_bus, xid 79}; GPU $IDX79 lost again in NVML" \
	"$( [ $ok = 0 ] && echo "restored ${ms} ms after the restart; GPU $IDX79 lost in nvidia-smi: $( [ "$lost" = 1 ] && echo yes || echo no)" || echo "not restored in 40 s: NVML answers for GPU $IDX79 while the model holds it lost")" \
	"$( [ $ok = 0 ] && [ "$lost" = 1 ] && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- Y: Xid 48 on another GPU
SEQ1=$(since_seq) || true
a1=$(applied_lines)
twin fault inject "$HOST/$GPU48" nvidia/xid/xid-48 >"$EV/Y-inject.json"
FID48=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))[0]["ID"])' "$EV/Y-inject.json" 2>/dev/null || true)
recorded48() { events --kind observe --since "$SEQ1" | grep 'kernelLog: .* → recorded' | grep -q ': 48,'; }
wait_until 60 recorded48 >/dev/null || true
ms=$(wait_until 60 relayed 48) || true
doc48() { [ "$(applied_lines)" -gt "$a1" ] && tail -n 20 "$WORK/sync.log" | grep -q ecc_uncorrectable; }
wait_until 60 doc48 >/dev/null || true
seen48() { fget "/v1/events?components=$XIDC" >"$EV/Y-events.json" 2>/dev/null && [ -n "$($CHK xidevent "$EV/Y-events.json" 48)" ]; }
ms=$(wait_until 90 seen48) || true
IFS=$'\x1f' read -r et ed em ek < <($CHK xidevent "$EV/Y-events.json" 48) || true
sleep 2
smi >"$EV/Y-smi.csv" || true
gssh 'nvidia-smi -q -i '"$IDX48"' -d ECC' >"$EV/Y-smi-ecc.txt" 2>&1 || true
IFS=$'\x1f' read -r h r a _ < <(state $XIDC Y-xid.json) || true
record Y1 "dctwin fault inject $HOST/$GPU48 nvidia/xid/xid-48: lines recorded, relayed (stand-in), Mokka document" \
	"Xid 48 lines in both kernels; {ecc_uncorrectable, xid 48} on GPU $IDX48" \
	"recorded: $(recorded48 && echo yes || echo no); relayed: $(relayed 48 && echo yes || echo no); document: $(doc48 && echo ecc_uncorrectable || echo not applied)" \
	"$( recorded48 && relayed 48 && doc48 && echo PASS || echo FAIL)"
record Y2 "fleetint reports Xid 48" "an error_xid event, Xid 48 on PCI:${BUS48%.0} = $UUID48 (the model's $GPU48)" \
	"type ${et:-none} after ${ms} ms, device ${ed:-none}: \"${em:-}\"; component: $h, \"$r\"" \
	"$( [ -n "$et" ] && [ "${em#*$UUID48}" != "$em" ] && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- R: clearing, then the reboot fleetint asks for
a2=$(applied_lines)
twin fault clear "${FID79:-$HOST/$GPU79}" >"$EV/R-clear79.json" 2>&1 || true
twin fault clear "${FID48:-$HOST/$GPU48}" >"$EV/R-clear48.json" 2>&1 || true
doc_healthy() { [ "$(applied_lines)" -gt "$a2" ] && tail -n 4 "$WORK/sync.log" | grep -q 'every GPU healthy'; }
ms=$(wait_until 60 doc_healthy) || true
sleep 2
smi >"$EV/R1-smi.csv" || true
record R1 "the twin clears both faults: Mokka's document empties, NVML answers for every GPU" "$NGPU GPUs answer" \
	"document healthy ${ms} ms after; nvidia-smi: $(grep -c 'GPU-' "$EV/R1-smi.csv") GPUs" "$( doc_healthy && [ "$(grep -c 'GPU-' "$EV/R1-smi.csv")" = "$NGPU" ] && echo PASS || echo FAIL)"
sleep 20   # more than one of fleetint's check intervals
IFS=$'\x1f' read -r h r a _ < <(state $XIDC R3-xid.json) || true
record R3 "fleetint's Xid component after the clear" "what fleetint does: an Xid read from the kernel log is history, kept until a reboot (SetHealthy, which would purge it, has no caller in 1.5.0)" \
	"$h: \"$r\"; suggested: ${a:-none}" "$( [ "$h" = Healthy ] && echo PASS || echo FINDING)"
B0=$(boot_id || true)
gssh 'sudo systemctl reboot' >/dev/null 2>&1 || true
back() { b=$(boot_id) && [ -n "$b" ] && [ "$b" != "$B0" ] && api_up; }
ms=$(wait_until 300 back) || true
healthy_again() { xid_health_is Healthy; }
ms2=$(wait_until 120 healthy_again) || true
IFS=$'\x1f' read -r h r a _ < <(state $XIDC R4-xid.json) || true
fget "/v1/events?components=$XIDC" >"$EV/R4-events.json" 2>&1 || true
smi >"$EV/R4-smi.csv" || true
record R4 "after the reboot fleetint suggests (the fleetint guest rebooted by the harness)" "fleetint records the reboot; its Xid component Healthy; NVML $NGPU GPUs" \
	"boot $B0 → $(boot_id) after ${ms} ms; $XIDC: $h, \"$r\" ${ms2} ms after its API answered; nvidia-smi: $(grep -c 'GPU-' "$EV/R4-smi.csv") GPUs" \
	"$( [ "$h" = Healthy ] && [ "$(grep -c 'GPU-' "$EV/R4-smi.csv")" = "$NGPU" ] && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- what this run cannot show
dcgm=$(grep '^accelerator-nvidia-dcgm' "$EV/B5-components.txt" | head -3 | tr '\t' ' ' | tr '\n' ';' | cut -c1-240 || true)
record C1 "fleetint's DCGM components (clock, mem, power, thermal, nvlink, …)" "readings from a DCGM host engine" \
	"no host engine: DCGM (datacenter-gpu-manager-4) is a Depends not installed, and the twin serves none; ${dcgm:-not listed}" CANNOT
record C2 "enrollment, attestation, OTLP export to Fleet Intelligence's backend" "the managed service" "not run: no backend; offline mode only (Q9)" CANNOT
# the facility both kernels gave the Xid lines: the twin guest's record prefix (kmsg.log), and the
# fleetint guest's (dmesg -r prints <facility*8+level>)
twinfac=$(grep -a 'NVRM: Xid' "$INHOST_DIR/kmsg.log" 2>/dev/null | head -1 | awk '{split($2, p, ","); print int(p[1] / 8)}') || true
gfac=$(grep -a 'NVRM: Xid' "$EV/X2-guest-kmsg.txt" | head -1 | sed -n 's/^<\([0-9]*\)>.*/\1/p' | awk '{print int($1 / 8)}') || true
gmod=$(gssh cat /var/lib/dctwin-kmod.txt 2>/dev/null) || true
tmod=$(events --kind observe | grep 'inhost.agent' | grep attached | tail -1 | grep -o 'kernel module [a-z]*' | cut -d' ' -f3) || true
if [ "$twinfac" = 0 ] && [ "$gfac" = 0 ]; then res=PASS; elif [ "$tmod" != loaded ] || [ "${gmod%% *}" != loaded ]; then res=CANNOT; else res=FAIL; fi
record C3 "the kernel records have the driver's facility" "facility kern in both kernels: printk'd by the in-host agent's kernel module (dctwin-kmsg, D50), as nvidia.ko's are, not written to /dev/kmsg" \
	"the twin's guest: facility ${twinfac:-?} (module ${tmod:-?}); the fleetint guest: facility ${gfac:-?} (module ${gmod:-?}); the printk is dctwin-kmsg's, not nvidia.ko's, which does not run" "$res"
mi() { sed -n "s/^| *$1 *| *\(.*[^ ]\) *|\$/\1/p" "$EV/B3-machine-info.txt" | head -1; }
record C4 "the server around the GPUs, as fleetint reports it (CPU, memory, DMI, power)" "the model's XE9680" \
	"the fleetint guest's: $(mi 'CPU Type') × $(mi 'CPU Logical Cores'), $(mi 'Memory Total'), QEMU's DMI (no dmidecode); the twin's XE9680 machine is the other guest, so a twin power action does not reach this one" CANNOT

print_table "NVIDIA Fleet Intelligence agent $FLEETINT_VER against the twin's $HOST (compat/fleet-intelligence)" | tee "$WORK/results.md"
exit
}

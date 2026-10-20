#!/usr/bin/env bash
# guest.sh: runs run.sh inside a KVM guest with a rootful Docker, for a host whose own container
# engine is rootless.
#
# Mokka's node agent creates its driver tree's character devices with mknod (/dev/nvidia<N>,
# nvidiactl, nvidia-uvm, the MIG capability nodes), and the kernel refuses mknod of a device in
# every user namespace but the first, privileged container or not. A Kind node of rootless Docker
# or Podman is in one, so Mokka cannot stage there and never starts. This boots a Debian cloud
# image under QEMU/KVM with user-mode networking (the host's network and firewall are not
# touched), installs docker.io inside the guest, copies in the toolchain and this repository, and
# runs compat/gpu-operator/run.sh there: Kind, Mokka and everything else run in the guest, whose
# kernel is the one their device nodes are made in.
#
#   BASE=/path/debian-13-genericcloud-amd64.qcow2 GUEST_DIR=/path/scratch compat/gpu-operator/guest.sh
#
#   BASE        a Debian 13 genericcloud qcow2 (cloud.debian.org; checked against its SHA512SUMS by
#               whoever fetched it). It is only read: the guest writes a qcow2 overlay in GUEST_DIR.
#   GUEST_DIR   scratch for the overlay, the cloud-init seed, the guest's ssh key, console and
#               evidence (copied back to $GUEST_DIR/evidence). Deleted overlay at the end.
#   CPUS=4 MEM=6144 DISK=25G      the guest's size
#   GOROOT_HOST=$(go env GOROOT)  a linux/amd64 Go toolchain to copy in (run.sh builds dctwin)
#   KIND, HELM, KUBECTL           linux/amd64 binaries to copy in (default: from PATH)
#   KUBEBUILDER_ASSETS            envtest's kube-apiserver and etcd, copied in
#   RUN_ENV                       extra environment for run.sh in the guest, e.g. "NODE_MEMORY=1500m"
#   RUN_TESTS=1                   then run `go test ./internal/...` in the guest ($GUEST_DIR/gotest.log)
#   HOLD=1                        keep the guest up after the run, until $GUEST_DIR/shutdown
#                                 exists (`guest.sh ssh …` runs a command in it meanwhile)
#
# Run it under whatever lock the host's owner asks for: the guest holds MEM for its whole life.
set -euo pipefail
{ # the whole script is one compound command, read before any of it runs: a sync of the repository
# while the guest runs cannot change what this process does next
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
: "${BASE:?BASE: a Debian 13 genericcloud qcow2}" "${GUEST_DIR:?GUEST_DIR: a scratch directory}"
CPUS=${CPUS:-4} MEM=${MEM:-6144} DISK=${DISK:-25G}
mkdir -p "$GUEST_DIR"
GUEST_DIR=$(cd "$GUEST_DIR" && pwd)
G=$GUEST_DIR
free_port() { python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }
gssh() { ssh -q -i "$G/id_ed25519" -p "$(cat "$G/ssh.port")" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
	-o BatchMode=yes -o ConnectTimeout=10 debian@127.0.0.1 "$@"; }
grsync() { rsync -a --delete -e "ssh -q -i $G/id_ed25519 -p $(cat "$G/ssh.port") -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes" "$@"; }
if [ "${1:-}" = ssh ]; then shift; gssh "$@"; exit; fi

# ---------------------------------------------------------------- the guest
[ -f "$G/id_ed25519" ] || ssh-keygen -q -t ed25519 -N '' -C dctwin-guest -f "$G/id_ed25519"
mkdir -p "$G/seed"
cat >"$G/seed/meta-data" <<EOF
instance-id: dctwin-gpuop-$(date +%s)
local-hostname: dctwin-gpuop-guest
EOF
cat >"$G/seed/user-data" <<EOF
#cloud-config
users:
  - name: debian
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys: ["$(cat "$G/id_ed25519.pub")"]
growpart: { mode: auto, devices: ["/"] }
write_files:
  - path: /etc/sysctl.d/90-kind.conf
    content: |
      fs.inotify.max_user_instances = 8192
      fs.inotify.max_user_watches = 1048576
package_update: true
packages: [docker.io, rsync, curl, python3, ca-certificates]
runcmd:
  - [sysctl, --system]
  - [usermod, -aG, docker, debian]
  - [systemctl, enable, --now, docker]
  - [touch, /var/lib/dctwin-ready]
EOF
rm -f "$G/overlay.qcow2"
qemu-img create -q -f qcow2 -b "$BASE" -F qcow2 "$G/overlay.qcow2" "$DISK"
free_port >"$G/ssh.port"
HTTP_PORT=$(free_port)
python3 -I -m http.server --bind 127.0.0.1 --directory "$G/seed" "$HTTP_PORT" >"$G/seed.log" 2>&1 &
SEED_PID=$!
QEMU_PID=
shutdown_guest() {
	set +e
	if [ -n "$QEMU_PID" ] && kill -0 "$QEMU_PID" 2>/dev/null; then
		gssh sudo systemctl poweroff >/dev/null 2>&1
		for _ in $(seq 1 60); do kill -0 "$QEMU_PID" 2>/dev/null || break; sleep 2; done
		kill "$QEMU_PID" 2>/dev/null
	fi
	kill "$SEED_PID" 2>/dev/null
	rm -f "$G/overlay.qcow2"
}
trap shutdown_guest EXIT
trap 'exit 130' INT TERM
# NoCloud from the host's loopback, which user-mode networking presents to the guest at 10.0.2.2.
qemu-system-x86_64 -name dctwin-gpuop-guest -machine q35,accel=kvm -cpu host -smp "$CPUS" -m "$MEM" \
	-drive "file=$G/overlay.qcow2,if=virtio,format=qcow2,discard=unmap" \
	-nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$(cat "$G/ssh.port")-:22" \
	-smbios "type=1,serial=ds=nocloud;s=http://10.0.2.2:$HTTP_PORT/" \
	-display none -serial "file:$G/console.log" -pidfile "$G/qemu.pid" &
QEMU_PID=$!
echo "guest booting (ssh 127.0.0.1:$(cat "$G/ssh.port"), console $G/console.log)" >&2
for _ in $(seq 1 180); do gssh test -e /var/lib/dctwin-ready 2>/dev/null && break; sleep 5; done
gssh test -e /var/lib/dctwin-ready || { echo "the guest did not finish cloud-init; see $G/console.log" >&2; exit 1; }
gssh 'docker version --format "docker {{.Server.Version}}"; nproc; free -m | sed -n 2p; df -h / | tail -1' >&2

# ---------------------------------------------------------------- the toolchain and the repository
GOROOT_HOST=${GOROOT_HOST:-$(go env GOROOT)}
KIND=${KIND:-$(command -v kind)} HELM=${HELM:-$(command -v helm)} KUBECTL=${KUBECTL:-$(command -v kubectl)}
: "${KUBEBUILDER_ASSETS:?KUBEBUILDER_ASSETS: envtest binaries (kube-apiserver, etcd)}"
gssh 'mkdir -p ~/go-root ~/bin ~/envtest ~/dc-twin ~/work'
grsync "$GOROOT_HOST/" "debian@127.0.0.1:go-root/"
grsync "$KUBEBUILDER_ASSETS/" "debian@127.0.0.1:envtest/"
grsync "$KIND" "$HELM" "$KUBECTL" "debian@127.0.0.1:bin/"
sync_repo() { grsync --exclude /bin/ --exclude .DS_Store --exclude __pycache__ "$root/" "debian@127.0.0.1:dc-twin/"; }
sync_repo

run() {
	sync_repo
	gssh "export PATH=\$HOME/bin:\$HOME/go-root/bin:\$PATH GOROOT=\$HOME/go-root KUBEBUILDER_ASSETS=\$HOME/envtest KUBECTL=\$HOME/bin/kubectl KIND=\$HOME/bin/kind WORK=\$HOME/work GOFLAGS=-p=$CPUS ${RUN_ENV:-}; cd ~/dc-twin && compat/gpu-operator/run.sh" \
		>"$G/run.out" 2>"$G/run.err" || true
	mkdir -p "$G/evidence"
	grsync "debian@127.0.0.1:work/evidence/" "$G/evidence/" || true
	grsync "debian@127.0.0.1:work/results.md" "$G/results.md" 2>/dev/null || true
	cat "$G/run.out"
}
run
if [ "${RUN_TESTS:-}" = 1 ]; then
	# The repository's own tests, in the same guest: a Linux machine with Go and envtest's binaries.
	gssh "export PATH=\$HOME/go-root/bin:\$PATH GOROOT=\$HOME/go-root KUBEBUILDER_ASSETS=\$HOME/envtest GOFLAGS=-p=$CPUS; cd ~/dc-twin && go test ./internal/... 2>&1" \
		>"$G/gotest.log" 2>&1 || true
	grep -E '^(ok|FAIL|---|panic)|no test files' "$G/gotest.log" | grep -v 'no test files' >&2 || true
fi
if [ "${HOLD:-}" = 1 ]; then
	echo "HOLD=1: the guest stays up until $G/shutdown exists; '$0 ssh <cmd>' runs in it; touch $G/rerun to run again" >&2
	rm -f "$G/shutdown" "$G/rerun"
	while [ ! -e "$G/shutdown" ]; do
		if [ -e "$G/rerun" ]; then rm -f "$G/rerun"; run; fi
		sleep 5
	done
	rm -f "$G/shutdown"
fi
exit
}

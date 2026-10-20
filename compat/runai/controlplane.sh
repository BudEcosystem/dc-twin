#!/usr/bin/env bash
# controlplane.sh: Run:ai's control plane against the twin, in one KVM guest.
#
# Run:ai's control plane is about thirty services on Kubernetes (Keycloak, PostgreSQL, NATS and
# Run:ai's own), so it needs kubelets; the twin's Nodes have none. This boots a Debian 13 cloud
# image under QEMU/KVM with user-mode networking (the host's network and firewall are untouched),
# installs k3s in it and Run:ai 3.0.20's published control-plane chart (controlplane/install-cp.sh),
# registers a cluster through Run:ai's API, and then runs the cluster side in the same guest
# (controlplane/cluster.sh): the twin's Nodes on envtest's kube-apiserver with KRM, per-pool KAI
# shards and Run:ai's workload controller as in run.sh, plus Run:ai's agent and cluster-sync
# connected to the control plane. Node pools, a project with quota and a workload are then made
# through the control plane's API, and the harness checks they reach the twin's Nodes.
#
#   BASE=<debian-13-genericcloud-amd64.qcow2> GUEST_DIR=<scratch> BIN=<dir> SRC=<dir> compat/runai/controlplane.sh
#
#   BASE       a Debian 13 genericcloud qcow2 (only read; the guest writes an overlay in GUEST_DIR)
#   GUEST_DIR  scratch: overlay, cloud-init seed, ssh key, console, evidence (copied back)
#   BIN        linux/amd64 binaries for the cluster side: dctwin, kai-{scheduler,binder,podgrouper,
#              queuecontroller}, krm-{nodepool-controller,project-controller,pod-group-assigner},
#              runai-{anyworkload-controller,agent,cluster-sync,external-workload-integrator},
#              karta-operator (workload-map v0.2.6 operator/), kube-controller-manager, helm
#   SRC        what cluster.sh reads its CRDs and charts from: krm (v0.18.3), kai-scheduler (v0.18.6),
#              run-ai (3.0.20), karta (the module the workload controller requires), workload-map
#              (v0.2.6, the Karta chart the 3.0.20 cluster chart deploys); symbolic links are followed
#   KUBEBUILDER_ASSETS  envtest's kube-apiserver, etcd and kubectl
#   CPUS=6 MEM=7168 DISK=80G   the guest's size (Run:ai documents 10 cores and 12 GB for the
#              control plane's system nodes). The guest reports the pages it frees to the host
#              (virtio-balloon free page reporting), so its page cache of pulled images does not
#              stay resident on the host.
#   HOLD=1     keep the guest up after the run until $GUEST_DIR/shutdown exists
#              (`controlplane.sh ssh <cmd>` runs a command in it meanwhile)
#
# Run it under the host's lock for big jobs: the guest holds MEM for its whole life.
set -euo pipefail
{ # one compound command, read before any of it runs: a sync of the repository cannot change it
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
: "${GUEST_DIR:?GUEST_DIR: a scratch directory}"
CPUS=${CPUS:-6} MEM=${MEM:-7168} DISK=${DISK:-80G}
mkdir -p "$GUEST_DIR"
G=$(cd "$GUEST_DIR" && pwd)
free_port() { python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }
gssh() { ssh -q -i "$G/id_ed25519" -p "$(cat "$G/ssh.port")" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
	-o BatchMode=yes -o ConnectTimeout=10 -o ServerAliveInterval=30 debian@127.0.0.1 "$@"; }
grsync() { rsync -a --delete -e "ssh -q -i $G/id_ed25519 -p $(cat "$G/ssh.port") -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes" "$@"; }
if [ "${1:-}" = ssh ]; then shift; gssh "$@"; exit; fi
: "${BASE:?BASE: a Debian 13 genericcloud qcow2}" "${BIN:?BIN: the cluster side binaries}" "${SRC:?SRC: the cluster side sources}"
: "${KUBEBUILDER_ASSETS:?KUBEBUILDER_ASSETS: envtest binaries}"

# ---------------------------------------------------------------- the guest
[ -f "$G/id_ed25519" ] || ssh-keygen -q -t ed25519 -N '' -C dctwin-runai-cp -f "$G/id_ed25519"
mkdir -p "$G/seed"
cat >"$G/seed/meta-data" <<EOF
instance-id: dctwin-runai-cp-$(date +%s)
local-hostname: dctwin-runai-cp
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
  - path: /etc/sysctl.d/90-k3s.conf
    content: |
      fs.inotify.max_user_instances = 8192
      fs.inotify.max_user_watches = 1048576
  # The page cache of ~5 GB of pulled images would otherwise fill the guest's memory and, with it,
  # the host's: drop it every 30 s, and the freed pages go back to the host (free page reporting).
  - path: /etc/systemd/system/dctwin-dropcaches.service
    content: |
      [Service]
      ExecStart=/bin/sh -c 'while true; do sync; echo 1 > /proc/sys/vm/drop_caches; sleep 30; done'
      [Install]
      WantedBy=multi-user.target
package_update: true
packages: [rsync, curl, python3, python3-yaml, ca-certificates, git, openssl, jq, socat]
runcmd:
  - [sysctl, --system]
  - [systemctl, enable, --now, dctwin-dropcaches.service]
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
qemu-system-x86_64 -name dctwin-runai-cp -machine q35,accel=kvm -cpu host -smp "$CPUS" -m "$MEM" \
	-drive "file=$G/overlay.qcow2,if=virtio,format=qcow2,discard=unmap" \
	-device virtio-balloon-pci,free-page-reporting=on \
	-nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$(cat "$G/ssh.port")-:22" \
	-smbios "type=1,serial=ds=nocloud;s=http://10.0.2.2:$HTTP_PORT/" \
	-display none -serial "file:$G/console.log" -pidfile "$G/qemu.pid" &
QEMU_PID=$!
echo "guest booting (ssh 127.0.0.1:$(cat "$G/ssh.port"), console $G/console.log)" >&2
for _ in $(seq 1 180); do gssh test -e /var/lib/dctwin-ready 2>/dev/null && break; sleep 5; done
gssh test -e /var/lib/dctwin-ready || { echo "the guest did not finish cloud-init; see $G/console.log" >&2; exit 1; }
gssh 'nproc; free -m | sed -n 2p; df -h / | tail -1' >&2

# ---------------------------------------------------------------- binaries, repository, run
gssh 'mkdir -p ~/bin ~/envtest ~/src ~/dc-twin ~/work'
grsync -L "$BIN/" "debian@127.0.0.1:bin/"
grsync -L --exclude .git "$SRC/" "debian@127.0.0.1:src/"
grsync "$KUBEBUILDER_ASSETS/" "debian@127.0.0.1:envtest/"
grsync --exclude /bin/ --exclude .DS_Store --exclude __pycache__ "$root/" "debian@127.0.0.1:dc-twin/"
run() {
	gssh 'export PATH=$HOME/bin:$PATH; cd ~/dc-twin && compat/runai/controlplane/install-cp.sh ~/work/cp' >"$G/install.out" 2>"$G/install.err" || true
	gssh 'export PATH=$HOME/bin:$PATH KUBEBUILDER_ASSETS=$HOME/envtest; cd ~/dc-twin && compat/runai/controlplane/cluster.sh ~/work/cp ~/work/cluster' \
		>"$G/run.out" 2>"$G/run.err" || true
	mkdir -p "$G/evidence"
	grsync "debian@127.0.0.1:work/cluster/evidence/" "$G/evidence/" || true
	cat "$G/run.out"
}
run
if [ "${HOLD:-}" = 1 ]; then
	echo "HOLD=1: the guest stays up until $G/shutdown exists; '$0 ssh <cmd>' runs in it" >&2
	rm -f "$G/shutdown"
	while [ ! -e "$G/shutdown" ]; do sleep 5; done
	rm -f "$G/shutdown"
fi
exit
}

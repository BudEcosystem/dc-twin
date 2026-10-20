#!/usr/bin/env bash
# Runs MAAS as Canonical ships it -- the snap, at a pinned channel and revision -- in an Ubuntu
# 24.04 cloud-image KVM guest with real systemd and snapd, with the twin inside the same guest, and
# checks MAAS's own power control of the twin's Redfish and IPMI BMCs through MAAS's API
# (flow.py --stage maas): query, off, on, a power cycle, a twin-side change read back by MAAS's
# power polling, and an unreachable BMC.
#
#   compat/maas/run-snap.sh                               # artefacts under $WORK
#   WORK=/tmp/maas-snap KEEP=1 compat/maas/run-snap.sh    # leave the guest running (ssh: $WORK/ssh.sh)
#
# Needs a Linux host with /dev/kvm, qemu-system-x86_64, qemu-img, curl, ssh, python3 and Go. The
# guest has QEMU user-mode networking only (no bridge, no libvirt, nothing changed on the host);
# its SSH port is forwarded to a loopback port the kernel chooses.
#
# MAAS_CHANNEL (3.7/stable) and MAAS_REVISION (43264, 3.7.4): the run stops if the channel carries
#   another revision. TESTDB_CHANNEL (3.7/stable): maas-test-db, Canonical's PostgreSQL snap for MAAS.
# UBUNTU_RELEASE (release-20260926): cloud-images.ubuntu.com/releases/noble/<release>, checked
#   against that release's SHA256SUMS; BASE_IMAGE names a copy already downloaded.
# GUEST_CPUS (4), GUEST_MEM (6144 MiB), GUEST_DISK (40G, a qcow2 overlay in $WORK).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
MAAS_CHANNEL=${MAAS_CHANNEL:-3.7/stable}
MAAS_REVISION=${MAAS_REVISION:-43264}
TESTDB_CHANNEL=${TESTDB_CHANNEL:-3.7/stable}
UBUNTU_RELEASE=${UBUNTU_RELEASE:-release-20260926}
IMAGE_NAME=ubuntu-24.04-server-cloudimg-amd64.img
GUEST_CPUS=${GUEST_CPUS:-4}
GUEST_MEM=${GUEST_MEM:-6144}
GUEST_DISK=${GUEST_DISK:-40G}
WORK=${WORK:-$(mktemp -d)}
DCTWIN_ASSETS=${DCTWIN_ASSETS:-$HOME/.cache/dc-twin/assets}
mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd)
[ -r /dev/kvm ] && [ -w /dev/kvm ] || { echo "no access to /dev/kvm" >&2; exit 1; }
# the guest's memory plus 1 GiB must be available, or the run does not start (MemAvailable, KiB)
avail=$(awk '/^MemAvailable:/ {print int($2 / 1024)}' /proc/meminfo)
[ "$avail" -ge $((GUEST_MEM + 1024)) ] || { echo "only $avail MiB available; the guest needs $GUEST_MEM MiB" >&2; exit 1; }
free_port() { python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }

# ---------------------------------------------------------------- the base image, checked
base=${BASE_IMAGE:-$WORK/$IMAGE_NAME}
if [ ! -f "$base" ]; then
	url=https://cloud-images.ubuntu.com/releases/noble/$UBUNTU_RELEASE
	echo "downloading $url/$IMAGE_NAME ..." >&2
	curl -fsSL -o "$WORK/SHA256SUMS" "$url/SHA256SUMS"
	curl -fsSL -o "$base.part" "$url/$IMAGE_NAME"
	want=$(awk -v f="*$IMAGE_NAME" '$2 == f || $2 == substr(f, 2) {print $1}' "$WORK/SHA256SUMS")
	got=$(sha256sum "$base.part" | cut -d' ' -f1)
	[ -n "$want" ] && [ "$want" = "$got" ] || { echo "$IMAGE_NAME: sha256 $got, SHA256SUMS says '$want'" >&2; exit 1; }
	mv "$base.part" "$base"
fi
echo "base image $base ($UBUNTU_RELEASE, sha256 checked)" >&2

# ---------------------------------------------------------------- the twin, static, for the guest
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/dctwin" ./cmd/dctwin)
assets=$(ls -d "$DCTWIN_ASSETS"/libredfish@* 2>/dev/null | head -1)
[ -n "$assets" ] || { echo "no captured BMC trees under $DCTWIN_ASSETS: run \`dctwin assets fetch\`" >&2; exit 1; }
tar -C "$(dirname "$assets")" -czf "$WORK/assets.tgz" "$(basename "$assets")"

# ---------------------------------------------------------------- the guest
G="$WORK/guest"
rm -rf "$G" && mkdir -p "$G/seed"
qemu-img create -q -f qcow2 -F qcow2 -b "$base" "$G/overlay.qcow2" "$GUEST_DISK"
ssh-keygen -q -t ed25519 -N '' -C dctwin-maas -f "$G/key"
cat >"$G/seed/user-data" <<EOF
#cloud-config
hostname: maas-twin
users:
  - name: twin
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys: ["$(cat "$G/key.pub")"]
ssh_pwauth: false
EOF
printf 'instance-id: dctwin-maas-%s\nlocal-hostname: maas-twin\n' "$$" >"$G/seed/meta-data"
seed_port=$(free_port) ssh_port=$(free_port)
# cloud-init's NoCloud datasource reads the seed over HTTP from the host (10.0.2.2 in user-mode
# networking is the host's loopback), named in the SMBIOS serial number
python3 -I -m http.server --bind 127.0.0.1 --directory "$G/seed" "$seed_port" >"$G/seed.log" 2>&1 &
seed_pid=$!
qemu_pid=
cleanup() {
	set +e
	kill "$seed_pid" 2>/dev/null
	if [ "${KEEP:-0}" = 1 ]; then
		echo "guest kept (pid $(cat "$G/qemu.pid" 2>/dev/null)); ssh: $WORK/ssh.sh; stop it with: kill \$(cat $G/qemu.pid)" >&2
		return
	fi
	if [ -f "$G/qemu.pid" ]; then
		qp=$(cat "$G/qemu.pid")
		gssh 'sudo poweroff' >/dev/null 2>&1
		for _ in $(seq 1 60); do kill -0 "$qp" 2>/dev/null || break; sleep 1; done
		kill "$qp" 2>/dev/null
	fi
	rm -f "$G/overlay.qcow2"
	echo "guest stopped and its overlay removed; artefacts in $WORK" >&2
}
trap cleanup EXIT
qemu-system-x86_64 -enable-kvm -cpu host -smp "$GUEST_CPUS" -m "$GUEST_MEM" -name dctwin-maas \
	-drive "file=$G/overlay.qcow2,if=virtio,format=qcow2" \
	-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$ssh_port-:22" -device virtio-net-pci,netdev=n0 \
	-smbios "type=1,serial=ds=nocloud;s=http://10.0.2.2:$seed_port/" \
	-display none -serial "file:$G/console.log" -daemonize -pidfile "$G/qemu.pid"
cat >"$WORK/ssh.sh" <<EOF
#!/bin/sh
exec ssh -q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$G/key" -p $ssh_port twin@127.0.0.1 "\$@"
EOF
chmod +x "$WORK/ssh.sh"
gssh() { "$WORK/ssh.sh" "$@"; }
gscp() { scp -q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$G/key" -P "$ssh_port" "$@"; }
for _ in $(seq 1 120); do gssh true 2>/dev/null && break; sleep 5; done
gssh 'cloud-init status --wait >/dev/null; uname -r; systemctl is-system-running || true' >&2
echo "guest up: $GUEST_CPUS vCPUs, $GUEST_MEM MiB, ssh 127.0.0.1:$ssh_port" >&2

# ---------------------------------------------------------------- MAAS, the snap
gscp "$WORK/dctwin" "$WORK/assets.tgz" "$root/examples/estates/lab-model.yaml" "$here/flow.py" "$here/guest-setup.sh" twin@127.0.0.1:
gssh "sudo MAAS_CHANNEL=$MAAS_CHANNEL MAAS_REVISION=$MAAS_REVISION TESTDB_CHANNEL=$TESTDB_CHANNEL bash ./guest-setup.sh" 2>&1 | tee "$WORK/guest-setup.txt" >&2
gssh 'test -f /opt/dctwin/ready' || { echo "MAAS did not come up in the guest (guest-setup.txt)" >&2; exit 1; }

# ---------------------------------------------------------------- the checks
set +e
gssh 'sudo env DCTWIN_STATE=/opt/dctwin/state PATH=/opt/dctwin:/snap/bin:/usr/sbin:/usr/bin:/sbin:/bin python3 -I /opt/dctwin/flow.py --stage maas --work /opt/dctwin/out' | tee "$WORK/maas.txt"
status=${PIPESTATUS[0]}
set -e
gssh 'sudo sh -c "DCTWIN_STATE=/opt/dctwin/state /opt/dctwin/dctwin events --json > /opt/dctwin/out/events.json; snap list maas maas-test-db > /opt/dctwin/out/snaps.txt; tar -C /var/snap/maas/common -czf /opt/dctwin/out/maas-logs.tgz log; journalctl --no-pager -t maas-agent -t maas-regiond -t maas-rackd -t maas-temporal-worker -t maas-temporal > /opt/dctwin/out/journal.txt; chmod -R a+r /opt/dctwin/out"'
gscp "twin@127.0.0.1:/opt/dctwin/out/*" "$WORK/"
exit "$status"

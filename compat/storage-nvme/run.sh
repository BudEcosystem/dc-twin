#!/usr/bin/env bash
# NICo's scout NVMe cleanup, command for command, on the drives the twin gives a vm host
# (docs/reviews/2026-11-08-storage-deep-dive.md S-15): a Debian guest under KVM with two NVMe drives
# configured exactly as internal/substrate/qemu.DriveArgs configures them, nvme-cli from Debian, and
# the sequence of crates/scout/src/deprovision/scrabbing.rs clean_this_nvme at NICo's pin
# 02b92f18: nvme id-ctrl -o json (tnvmcap, cntlid, oacs, sn, mn, fr), list-ns -a, format -s2 -f on
# every namespace, and where OACS bit 3 says the drive manages namespaces: delete-ns, id-ns on the
# broadcast NSID (the LBA formats), create-ns over tnvmcap/512 sectors, attach-ns to the controller.
# A drive passes when every step succeeds, the namespace comes back at the drive's full size, and
# the marker written before the cleanup reads back as zeros.
#
#   QEMU_DIR=<scripts/qemu/build.sh prefix>/bin BASE=<debian-13-genericcloud-amd64.qcow2> compat/storage-nvme/run.sh
#
# Needs Linux with KVM, python3, and internet access from the guest (it installs nvme-cli). With an
# unpatched QEMU the controllers advertise namespace management they do not implement and report
# no capacity, and the run shows the failure NICo would hit (NVMECleanFailed).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
WORK=${WORK:-$(mktemp -d)}
QEMU_DIR=${QEMU_DIR:-}
Q=${QEMU_DIR:+$QEMU_DIR/}qemu-system-x86_64
BASE=${BASE:?BASE=<debian-13-genericcloud-amd64.qcow2>}
mkdir -p "$WORK/seed"
port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }
HTTP_PORT=$(port)

(cd "$root" && go build -o "$WORK/driveargs" ./compat/storage-nvme/driveargs)
truncate -s 4G "$WORK/d0.raw"
truncate -s 2G "$WORK/d1.raw"
mapfile -t DRIVES < <("$WORK/driveargs" "$Q" \
	"$WORK/d0.raw:S6X1NE0R700001:SAMSUNG MZQL23T8HCLS-00A07:GDC5902Q:02dc7e0000000001" \
	"$WORK/d1.raw:S6X1NE0R700002:SAMSUNG MZQL23T8HCLS-00A07:GDC5902Q:02dc7e0000000002")
printf '%s\n' "${DRIVES[@]}" > "$WORK/drive-args.txt"

cat > "$WORK/seed/meta-data" <<EOF
instance-id: dctwin-storage-nvme
local-hostname: storage-nvme
EOF
cat > "$WORK/seed/user-data" <<'EOF'
#cloud-config
package_update: true
packages: [nvme-cli]
write_files:
  - path: /root/scout-clean.sh
    permissions: "0755"
    content: |
      #!/bin/bash
      exec > /dev/ttyS0 2>&1
      echo "@@T begin"
      for dev in $(ls /dev/nvme[0-9]* | grep -E "^/dev/nvme[0-9]+$"); do
        J=$(nvme id-ctrl $dev -o json)
        read OACS TNV CNT SN MN FR <<< $(python3 -c "import json,sys;d=json.loads(sys.argv[1]);print(d[\"oacs\"],d[\"tnvmcap\"],d[\"cntlid\"],d[\"sn\"].strip().replace(\" \",\"_\"),d[\"mn\"].strip().replace(\" \",\"_\"),d[\"fr\"].strip())" "$J")
        echo "@@T $dev id-ctrl oacs=$OACS tnvmcap=$TNV cntlid=$CNT sn=$SN mn=$MN fr=$FR"
        NS=$(ls /sys/block | grep -E "^nvme[0-9]+n1$" | while read b; do [ "$(cat /sys/block/$b/device/serial 2>/dev/null | xargs)" = "$SN" ] && echo $b; done)
        echo DCTWIN-MARKER | dd of=/dev/$NS bs=512 count=1 conv=sync,fsync status=none
        ok=1
        for ns in $(nvme list-ns $dev -a | sed -n "s/.*:\(0x[0-9]*\)/\1/p"); do
          nvme format $dev -s2 -f -n $ns >/dev/null && echo "@@T $dev format $ns ok" || { echo "@@T $dev format $ns FAILED"; ok=0; }
          if (( OACS & 8 )); then nvme delete-ns $dev -n $ns >/dev/null && echo "@@T $dev delete-ns $ns ok" || { echo "@@T $dev delete-ns $ns FAILED"; ok=0; }; fi
        done
        if (( OACS & 8 )); then
          L=$(nvme id-ns $dev -n 0xffffffff -o json | python3 -c "import json,sys;d=json.load(sys.stdin);print(sum(1 for l in d[\"lbafs\"] if l[\"ds\"]==9 and l[\"ms\"]==0))")
          echo "@@T $dev id-ns broadcast 512B-formats=$L"
          S=$((TNV/512))
          OUT=$(nvme create-ns $dev --nsze=$S --ncap=$S --flbas 0 --dps=0 2>&1); echo "@@T $dev create-ns: $OUT"
          NSID=$(echo "$OUT" | sed -n "s/.*nsid:\([0-9]*\).*/\1/p")
          [ -n "$NSID" ] || ok=0
          nvme attach-ns $dev -n "$NSID" -c $CNT >/dev/null && echo "@@T $dev attach-ns $NSID ok" || { echo "@@T $dev attach-ns FAILED"; ok=0; }
          sleep 3; nvme ns-rescan $dev; sleep 1
        fi
        NS=$(ls /sys/block | grep -E "^nvme[0-9]+n1$" | while read b; do [ "$(cat /sys/block/$b/device/serial 2>/dev/null | xargs)" = "$SN" ] && echo $b; done)
        SIZE=$(blockdev --getsize64 /dev/$NS 2>/dev/null || echo 0)
        HEAD=$(dd if=/dev/$NS bs=13 count=1 status=none 2>/dev/null | od -An -c | tr -d " \n")
        [ "$SIZE" = "$TNV" ] || ok=0
        case "$HEAD" in *D*) ok=0;; esac
        echo "@@T $dev result ok=$ok size=$SIZE tnvmcap=$TNV head=$HEAD"
      done
      echo "@@T done"
runcmd:
  - [/root/scout-clean.sh]
EOF

qemu-img create -q -f qcow2 -b "$BASE" -F qcow2 "$WORK/overlay.qcow2" 8G
python3 -I -m http.server --bind 127.0.0.1 --directory "$WORK/seed" "$HTTP_PORT" >"$WORK/seed.log" 2>&1 &
SEED=$!
"$Q" -name dctwin-storage-nvme -machine q35 -accel kvm -cpu host -m 2048 -smp 2 -display none \
	-smbios "type=1,serial=ds=nocloud;s=http://10.0.2.2:$HTTP_PORT/" \
	-nic user,model=virtio-net-pci \
	-drive "file=$WORK/overlay.qcow2,if=virtio,format=qcow2" \
	"${DRIVES[@]}" -serial "file:$WORK/console.log" &
QEMU=$!
trap 'kill $QEMU $SEED 2>/dev/null || true' EXIT
for _ in $(seq 1 900); do grep -aq "@@T done" "$WORK/console.log" 2>/dev/null && break; sleep 1; done
grep -a "@@T" "$WORK/console.log" | tee "$WORK/result.txt"
pass=$(grep -c "result ok=1" "$WORK/result.txt" || true)
echo "storage-nvme: $pass of 2 drives cleaned as NICo's scout cleans them (artefacts: $WORK)"
[ "$pass" = 2 ]

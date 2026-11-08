#!/usr/bin/env bash
# A storage system's data path at fidelity vm (docs/reviews/2026-11-08-storage-deep-dive.md P4,
# S-Q2) used by real, unmodified clients: the storage-vm-system estate's two Alpine storage nodes
# serve the model's objects, and a Debian guest under KVM, outside the twin, reaches each node's
# published targets on this machine (QEMU user networking's 10.0.2.2):
#   - the Linux NFS client mounts a file system (NFSv4.1), writes and reads it;
#   - nvme-cli connects to a volume over NVMe/TCP as the host the model maps it to, and the
#     namespace has the volume's size and NGUID; data written reads back;
#   - curl signs S3 requests (SigV4) to put and get an object in a bucket;
#   - k3s (pinned) with csi-driver-nfs (pinned) provisions a PersistentVolumeClaim on the file
#     system's export, binds it, and a pod writes into it: the twin observes the bytes.
# A host is refused by a volume it is not mapped to.
#
#   QEMU_DIR=<scripts/qemu/build.sh prefix>/bin BASE=<debian-13-genericcloud-amd64.qcow2> compat/storage-datapath/run.sh
#
# Needs Linux with KVM, the twin's guest image with its storage packages (dctwin assets fetch),
# python3, and internet access from the Debian guest (it installs nfs-common, nvme-cli, k3s and
# the CSI driver's images).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
K3S_VERSION=${K3S_VERSION:-v1.34.2+k3s1}
CSI_NFS_VERSION=${CSI_NFS_VERSION:-v4.13.0}
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
QEMU_DIR=${QEMU_DIR:-}
Q=${QEMU_DIR:+$QEMU_DIR/}qemu-system-x86_64
BASE=${BASE:?BASE=<debian-13-genericcloud-amd64.qcow2>}
HOSTNQN=nqn.2014-08.org.nvmexpress:uuid:d7c5a0e2-0b1e-4f4a-9a52-5d2c0e1f0a01
mkdir -p "$WORK/seed"
port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }

# the twin: the estate with this QEMU
export DCTWIN_STATE="$WORK/state"
rm -rf "$DCTWIN_STATE"
sed "s|    vm: { memoryMB: 1536, cpus: 2 }|    vm: { memoryMB: 1536, cpus: 2${QEMU_DIR:+, qemu: $QEMU_DIR} }|" "$root/examples/estates/storage-vm-system.yaml" > "$WORK/estate.yaml"
"$DCTWIN" up "$WORK/estate.yaml" --state "$DCTWIN_STATE" --adapters vm,inhost,storage > "$WORK/up.log" 2>&1 &
for _ in $(seq 1 240); do [ -f "$DCTWIN_STATE/control.json" ] && break; sleep 0.5; done
[ -f "$DCTWIN_STATE/control.json" ] || { cat "$WORK/up.log"; exit 1; }
QEMU=
trap '[ -n "$QEMU" ] && kill $QEMU 2>/dev/null; kill ${SEED:-} 2>/dev/null; "$DCTWIN" down >/dev/null 2>&1 || true' EXIT
echo "waiting for both storage nodes to serve"
for _ in $(seq 1 180); do
  n=$("$DCTWIN" report 2>/dev/null | grep -c "serving nfs,nvme-tcp,s3" || true)
  [ "$n" = 2 ] && break
  sleep 2
done
[ "$n" = 2 ] || { "$DCTWIN" report | grep "data path"; exit 1; }

"$DCTWIN" storage create filesystem lab-fs models --size 10G --export "10.50.0.0/16:root" > /dev/null
"$DCTWIN" storage create volume lab-fs vol1 --size 1Gi > /dev/null
"$DCTWIN" storage map lab-fs vol1 "$HOSTNQN" > /dev/null
"$DCTWIN" storage create volume lab-fs vol2 --size 1Gi > /dev/null    # mapped to no host
"$DCTWIN" storage create bucket lab-fs datasets > /dev/null
"$DCTWIN" storage get lab-fs > "$WORK/system.json"
sleep 8   # a reconcile pass puts them on their nodes
"$DCTWIN" storage credentials > "$WORK/credentials.json"
read -r NFS NVME NVME2 S3 NQN NQN2 NGUID AK SK < <(python3 - "$WORK/system.json" "$WORK/credentials.json" <<'EOF'
import json, sys
v = json.load(open(sys.argv[1])); c = json.load(open(sys.argv[2]))[0]
node = lambda p: "-".join(p.split("/")[-2:])
port = lambda home, proto: c["endpoints"][node(home) + "/" + proto].rsplit(":", 1)[1]
fs = [o for o in v["FileSystems"] if o["Name"] == "models"][0]
v1 = [o for o in v["Volumes"] if o["Name"] == "vol1"][0]
v2 = [o for o in v["Volumes"] if o["Name"] == "vol2"][0]
b = [o for o in v["Buckets"] if o["Name"] == "datasets"][0]
print(port(fs["Home"], "nfs"), port(v1["Home"], "nvme-tcp"), port(v2["Home"], "nvme-tcp"), port(b["Home"], "s3"),
      v1["NQN"], v2["NQN"], v1["NGUID"], c["accessKey"], c["secretKey"])
EOF
)
echo "published: nfs :$NFS, nvme-tcp :$NVME (vol1) :$NVME2 (vol2), s3 :$S3"

HTTP_PORT=$(port)
cat > "$WORK/seed/meta-data" <<EOF
instance-id: dctwin-storage-datapath
local-hostname: storage-client
EOF
cat > "$WORK/seed/user-data" <<EOF
#cloud-config
package_update: true
packages: [nfs-common, nvme-cli, curl]
write_files:
  - path: /etc/nvme/hostnqn
    content: "$HOSTNQN\n"
  - path: /root/client.sh
    permissions: "0755"
    content: |
      #!/bin/bash
      exec > /dev/ttyS0 2>&1
      T() { echo "@@T \$*"; }
      T begin
      modprobe nvme-tcp
      # NFS
      mkdir -p /mnt/models
      if mount -t nfs4 -o vers=4.1,port=$NFS 10.0.2.2:/models /mnt/models; then
        echo "weights v1" > /mnt/models/hello.txt && sync && [ "\$(cat /mnt/models/hello.txt)" = "weights v1" ] && T "nfs ok \$(stat -f -c %T /mnt/models)" || T "nfs FAILED io"
        dd if=/dev/urandom of=/mnt/models/blob bs=1M count=16 conv=fsync status=none
        umount /mnt/models
      else T "nfs FAILED mount"; fi
      # NVMe/TCP
      if nvme connect -t tcp -a 10.0.2.2 -s $NVME -n $NQN --hostnqn $HOSTNQN; then
        sleep 2
        # the namespace's head (nvmeXnY), not a multipath path (nvmeXcYnZ): the subsystem's own
        dev=\$(for s in /sys/class/nvme-subsystem/*; do grep -qx "$NQN" \$s/subsysnqn 2>/dev/null && ls -d \$s/nvme*n* | grep -v "c[0-9]*n" | head -1; done | xargs -r basename)
        dev=/dev/\$dev
        size=\$(blockdev --getsize64 \$dev)
        nguid=\$(nvme id-ns \$dev -o json | python3 -c 'import json,sys;print(json.load(sys.stdin)["nguid"])')
        head -c 1048576 /dev/urandom > /tmp/p; dd if=/tmp/p of=\$dev bs=1M count=1 oflag=direct status=none
        same=\$(dd if=\$dev bs=1M count=1 iflag=direct status=none | cmp - /tmp/p && echo yes || echo no)
        T "nvme ok dev=\$dev size=\$size nguid=\$nguid readback=\$same"
        nvme disconnect -n $NQN >/dev/null
      else T "nvme FAILED connect"; fi
      if nvme connect -t tcp -a 10.0.2.2 -s $NVME2 -n $NQN2 --hostnqn $HOSTNQN 2>/dev/null; then T "nvme-unmapped FAILED connected"; nvme disconnect -n $NQN2; else T "nvme-unmapped ok refused"; fi
      # S3
      echo "weights v1" > /tmp/obj
      curl -sS --fail-with-body --aws-sigv4 aws:amz:us-east-1:s3 --user $AK:$SK -T /tmp/obj http://10.0.2.2:$S3/datasets/hello.txt &&
        [ "\$(curl -sS --aws-sigv4 aws:amz:us-east-1:s3 --user $AK:$SK http://10.0.2.2:$S3/datasets/hello.txt)" = "weights v1" ] && T "s3 ok" || T "s3 FAILED"
      # Kubernetes: k3s and csi-driver-nfs, a claim on the file system's export
      curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION=$K3S_VERSION INSTALL_K3S_EXEC="--disable traefik --disable metrics-server" sh - >/dev/null 2>&1 || T "k3s FAILED install"
      export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
      for i in \$(seq 1 120); do kubectl get nodes 2>/dev/null | grep -q " Ready" && kubectl get serviceaccount default >/dev/null 2>&1 && break; sleep 2; done
      curl -skSL https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/$CSI_NFS_VERSION/deploy/install-driver.sh | bash -s $CSI_NFS_VERSION -- >/dev/null 2>&1 || T "csi FAILED install"
      cat > /root/claim.yaml <<'K'
      apiVersion: storage.k8s.io/v1
      kind: StorageClass
      metadata: { name: dctwin-nfs, annotations: { storageclass.kubernetes.io/is-default-class: "true" } }
      provisioner: nfs.csi.k8s.io
      parameters: { server: 10.0.2.2, share: /models }
      mountOptions: [ "nfsvers=4.1", "port=$NFS" ]
      reclaimPolicy: Delete
      volumeBindingMode: Immediate
      ---
      apiVersion: v1
      kind: PersistentVolumeClaim
      metadata: { name: weights }
      spec: { accessModes: [ReadWriteMany], resources: { requests: { storage: 1Gi } } }
      ---
      apiVersion: v1
      kind: Pod
      metadata: { name: writer }
      spec:
        restartPolicy: Never
        containers:
          - name: w
            image: busybox:1.37
            command: ["sh", "-c", "dd if=/dev/urandom of=/data/model.bin bs=1M count=24 && sync && ls -l /data"]
            volumeMounts: [{ name: d, mountPath: /data }]
        volumes: [{ name: d, persistentVolumeClaim: { claimName: weights } }]
      K
      for i in \$(seq 1 30); do kubectl apply -f /root/claim.yaml >/dev/null 2>&1 && kubectl get pod writer >/dev/null 2>&1 && break; sleep 4; done
      for i in \$(seq 1 300); do p=\$(kubectl get pod writer -o jsonpath='{.status.phase}' 2>/dev/null); [ "\$p" = Succeeded -o "\$p" = Failed ] && break; sleep 2; done
      T "csi pvc=\$(kubectl get pvc weights -o jsonpath='{.status.phase}') pv=\$(kubectl get pvc weights -o jsonpath='{.spec.volumeName}') pod=\$p"
      kubectl logs writer 2>/dev/null | tail -3 | sed 's/^/@@T pod /'
      kubectl get events -A --sort-by=.lastTimestamp 2>/dev/null | tail -5 | sed 's/^/@@T event /'
      T done
runcmd:
  - [/root/client.sh]
EOF
qemu-img create -q -f qcow2 -b "$BASE" -F qcow2 "$WORK/overlay.qcow2" 16G
python3 -I -m http.server --bind 127.0.0.1 --directory "$WORK/seed" "$HTTP_PORT" >"$WORK/seed.log" 2>&1 &
SEED=$!
"$Q" -name dctwin-storage-client -machine q35 -accel kvm -cpu host -m 4096 -smp 2 -display none \
  -smbios "type=1,serial=ds=nocloud;s=http://10.0.2.2:$HTTP_PORT/" \
  -nic user,model=virtio-net-pci -drive "file=$WORK/overlay.qcow2,if=virtio,format=qcow2" \
  -serial "file:$WORK/console.log" &
QEMU=$!
for _ in $(seq 1 1500); do grep -aq "@@T done" "$WORK/console.log" 2>/dev/null && break; sleep 1; done
grep -a "@@T" "$WORK/console.log" | tee "$WORK/result.txt"
status=0
for want in "nfs ok" "nvme ok" "nvme-unmapped ok" "s3 ok" "csi pvc=Bound"; do
  grep -q "@@T $want" "$WORK/result.txt" || { echo "FAIL: $want"; status=1; }
done
grep -q "readback=yes" "$WORK/result.txt" || { echo "FAIL: NVMe data did not read back"; status=1; }
grep -q "size=1073741824 nguid=$NGUID" "$WORK/result.txt" || { echo "FAIL: the namespace is not the volume (size 1 GiB, NGUID $NGUID)"; status=1; }
grep -q "pod=Succeeded" "$WORK/result.txt" || { echo "FAIL: the pod on the claim did not succeed"; status=1; }
# the twin's model observed what the clients wrote
sleep 8
used=$("$DCTWIN" storage get lab-fs | python3 -c 'import json,sys;v=json.load(sys.stdin);print([f["UsedBytes"] for f in v["FileSystems"] if f["Name"]=="models"][0])')
echo "the model's file system models holds $used bytes"
[ "$used" -ge $((40 << 20)) ] || { echo "FAIL: the model did not observe the clients' 40 MiB"; status=1; }
"$DCTWIN" report | grep -E "data path|stand-in" | sed 's/^/report: /'
echo "storage-datapath: $([ $status = 0 ] && echo PASS || echo FAIL) (artefacts: $WORK)"
exit $status

# shellcheck shell=bash
# The GPU side of compat/nvsentinel's node: a Debian guest (KVM, QEMU user-mode networking, a qcow2
# overlay of the image BASE names) whose NVML is NVIDIA's Mokka presenting the twin's GPUs, kept equal
# to the model by `dctwin-mokka sync`, under NVSentinel's DCGM-based gpu-health-monitor (an
# in-process DCGM host engine, the chart's embedded-mode), which publishes to the node's one
# platform-connectors through its socket, forwarded into the guest over ssh. Its kernel log gets
# the twin guest's NVRM records through the in-host agent's kernel module, built there
# (kmsg-relay.py), where DCGM's own kmsg reader finds an Xid as it finds the driver's. Sourced by
# run.sh after compat/kai/lib/common.sh. The twin's own `os: twin` guest of the same server is
# Alpine's netboot initramfs (musl): neither NVML nor DCGM can run there, so the GPUs are this
# second guest's, as in compat/fleet-intelligence. It stands for the same machine: when the twin's
# machine boots anew, this guest is reset too (gpu_guest_reset, through its QMP socket).
#
# The guest is prepared once ($WORK/guest/prepared.qcow2: Mokka, Docker, the monitor's image built
# from the tag's source, the kernel module) and every run boots an overlay of it, so a run spends
# no time on packages.

# nvml-mock 0.5.0 (NVIDIA/k8s-test-infra v0.5.0, e2950d58), the multi-architecture index, and its h100 profile
MOKKA_IMAGE=ghcr.io/nvidia/nvml-mock@sha256:9583cc6e9ce53ff54bda4f34072fd64460364c907a4af17419fffec36dac6238
MOKKA_COMMIT=e2950d58bdace638f3a522005978cc278ffa95d7
MOKKA_H100_SHA=5121352b0b340ef19bdacdbb8322b268d7121f3b70c3690b717c6cab458840b2
GPU_CPUS=${GPU_CPUS:-4} GPU_MEM=${GPU_MEM:-3072} GPU_DISK=${GPU_DISK:-24G}

gpu_gssh() {
	ssh -q -i "$WORK/guest/id_ed25519" -p "$(cat "$WORK/guest/ssh.port")" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
		-o BatchMode=yes -o ConnectTimeout=10 debian@127.0.0.1 "$@"
}

# gpu_mokka: the released nvml-mock image's /usr/local (node agent, nvml-mock-ctl, nvidia-smi, the
# mock libnvidia-ml), extracted once into $WORK/dl/mokka, and its h100 profile.
gpu_mokka() {
	local m=$WORK/dl/mokka
	if [ ! -x "$m/bin/node-agent" ]; then
		docker pull -q --platform linux/amd64 "$MOKKA_IMAGE" >/dev/null
		rm -rf "$m" && mkdir -p "$m"
		docker rm -f dctwin-nvs-mokka >/dev/null 2>&1 || true
		docker create --name dctwin-nvs-mokka "$MOKKA_IMAGE" true >/dev/null
		docker cp dctwin-nvs-mokka:/usr/local/bin "$m/bin"
		docker cp dctwin-nvs-mokka:/usr/local/lib "$m/lib"
		docker rm dctwin-nvs-mokka >/dev/null
		docker image rm "$MOKKA_IMAGE" >/dev/null 2>&1 || true
	fi
	mkdir -p "$WORK/dl/mokka-profiles"
	local p=$WORK/dl/mokka-profiles/h100.yaml
	[ -s "$p" ] || curl -sfL -o "$p" "https://raw.githubusercontent.com/NVIDIA/k8s-test-infra/$MOKKA_COMMIT/deployments/nvml-mock/helm/nvml-mock/profiles/h100.yaml"
	[ "$(sha256sum "$p" | cut -d' ' -f1)" = "$MOKKA_H100_SHA" ] || { echo "Mokka profile h100: sha256 differs from the pin" >&2; return 1; }
}

# gpu_guest_boot <disk.qcow2>: boots a guest on the disk with a NoCloud seed (a user with this run's
# key) served from this host's loopback, which user-mode networking shows the guest as 10.0.2.2.
gpu_guest_boot() {
	local disk=$1 G=$WORK/guest http
	[ -f "$G/id_ed25519" ] || ssh-keygen -q -t ed25519 -N '' -C dctwin-nvsentinel -f "$G/id_ed25519"
	mkdir -p "$G/seed"
	printf 'instance-id: dctwin-nvs-%s\nlocal-hostname: %s\n' "$(date +%s%N)" "${NODE:-dctwin-gpu}" >"$G/seed/meta-data"
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
	free_port >"$G/ssh.port"
	http=$(free_port)
	# the seed server stays up while the guest runs: cloud-init looks for its seed on every boot
	python3 -I -m http.server --bind 127.0.0.1 --directory "$G/seed" "$http" >"$G/seed.log" 2>&1 &
	GPU_SEED_PID=$!
	rm -f "$G/qmp.sock"
	qemu-system-x86_64 -name dctwin-nvs-gpu-guest -machine q35,accel=kvm -cpu host -smp "$GPU_CPUS" -m "$GPU_MEM" \
		-drive "file=$disk,if=virtio,format=qcow2,discard=unmap" \
		-nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$(cat "$G/ssh.port")-:22" \
		-smbios "type=1,serial=ds=nocloud;s=http://10.0.2.2:$http/" \
		-qmp "unix:$G/qmp.sock,server=on,wait=off" \
		-display none -serial "file:$G/console.log" &
	GPU_QEMU_PID=$!
	ready() { gpu_gssh test -e /var/lib/dctwin-ready; }
	wait_until 600 ready >/dev/null || { echo "the GPU guest did not finish cloud-init; see $G/console.log" >&2; return 1; }
	export GPU_QEMU_PID GPU_SEED_PID
}

# gpu_guest_reset: the machine's reset button (QMP system_reset), then waits until the guest is
# back with a new boot ID, its Docker and Mokka's node agent up. Prints the new boot ID.
gpu_guest_reset() {
	local b0 b
	b0=$(gpu_gssh cat /proc/sys/kernel/random/boot_id 2>/dev/null) || true
	python3 -I -c '
import json, socket, sys
s = socket.socket(socket.AF_UNIX); s.settimeout(10); s.connect(sys.argv[1]); f = s.makefile("rw")
f.readline()
for c in ("qmp_capabilities", "system_reset"):
    f.write(json.dumps({"execute": c}) + "\n"); f.flush()
    while True:
        r = json.loads(f.readline())
        if "return" in r or "error" in r:
            break
' "$WORK/guest/qmp.sock" || return 1
	gpu_back() {
		b=$(gpu_gssh cat /proc/sys/kernel/random/boot_id 2>/dev/null) && [ -n "$b" ] && [ "$b" != "$b0" ] &&
			gpu_gssh 'systemctl is-active -q docker && systemctl is-active -q dctwin-nvml-mock' 2>/dev/null
	}
	wait_until 300 gpu_back >/dev/null || return 1
	echo "$b"
}

gpu_guest_stop() {
	[ -z "${GPU_SEED_PID:-}" ] || { kill "$GPU_SEED_PID" 2>/dev/null; GPU_SEED_PID=; }
	[ -n "${GPU_QEMU_PID:-}" ] || return 0
	gpu_gssh sudo systemctl poweroff >/dev/null 2>&1 || true
	local i
	for i in $(seq 1 60); do kill -0 "$GPU_QEMU_PID" 2>/dev/null || break; sleep 0.5; done
	kill "$GPU_QEMU_PID" 2>/dev/null || true
	wait "$GPU_QEMU_PID" 2>/dev/null || true
	GPU_QEMU_PID=
}

# gpu_prepare_key <stage>: what the prepared disk is made from: gpu-guest-setup.sh's install step
# (its other steps run on every boot of an overlay), the staged files and the base image.
gpu_prepare_key() {
	{ sed -n '/^install)$/,/^\t;;$/p' "$here/gpu-guest-setup.sh" | sha256sum; sha256sum "$1/build.env" "$1"/mokka/bin/* "$1"/mokka/lib/* "$1"/kmod/*; echo "$BASE"; } |
		sha256sum | cut -c1-16
}

# gpu_guest_prepare: the prepared disk, built once per key (Mokka, Docker, the monitor's image, the
# kernel module), and this run's stage directory.
gpu_guest_prepare() {
	local G=$WORK/guest key S l
	mkdir -p "$G"
	S=$WORK/guest/stage
	rm -rf "$S" && mkdir -p "$S/mokka/bin" "$S/mokka/lib"
	cp "$WORK/dl/mokka/bin/node-agent" "$WORK/dl/mokka/bin/nvml-mock-ctl" "$WORK/dl/mokka/bin/nvidia-smi" "$S/mokka/bin/"
	for l in libnvidia-ml libcuda; do cp "$(ls "$WORK/dl/mokka/lib/$l".so.*.*.* | head -1)" "$S/mokka/lib/"; done
	tar -C "$SRC" -cf "$S/gpu-health-monitor.tar" health-monitors/gpu-health-monitor
	printf 'GHM_IMAGE=%s\nNVS_REF=%s\nNVS_COMMIT=%s\n' "$GHM_IMAGE" "$NVS_REF" "$NVS_COMMIT" >"$S/build.env"
	cp "$here/gpu-guest-setup.sh" "$S/"
	mkdir -p "$S/kmod" && cp "$root/internal/inhost/kmod/Kbuild" "$root/internal/inhost/kmod/dctwin_kmsg.c" "$root/internal/inhost/kmod/guest-load.sh" "$S/kmod/"
	key=$(gpu_prepare_key "$S")
	if [ ! -f "$G/prepared.qcow2" ] || [ "$(cat "$G/prepared.key" 2>/dev/null)" != "$key" ]; then
		echo "preparing the GPU guest (once: Docker, Mokka, the kernel module, gpu-health-monitor built from $NVS_REF)" >&2
		rm -f "$G/prepared.qcow2" "$G/prepared.key"
		qemu-img create -q -f qcow2 -b "$BASE" -F qcow2 "$G/prepared.qcow2" "$GPU_DISK"
		gpu_guest_boot "$G/prepared.qcow2"
		gpu_gssh 'rm -rf ~/stage; mkdir -p ~/stage' && tar -C "$S" -cf - . | gpu_gssh 'tar -C ~/stage -xf -'
		gpu_gssh 'sudo sh ~/stage/gpu-guest-setup.sh install /home/debian/stage' >"$G/prepare.log" 2>&1 || { tail -20 "$G/prepare.log" >&2; gpu_guest_stop; return 1; }
		gpu_gssh 'sudo cloud-init clean --logs >/dev/null 2>&1; sudo rm -f /var/lib/dctwin-ready; sudo fstrim -a >/dev/null 2>&1' || true
		gpu_guest_stop
		echo "$key" >"$G/prepared.key"
	fi
}

# gpu_guest_up: this run's overlay of the prepared disk, booted.
gpu_guest_up() {
	local G=$WORK/guest
	rm -f "$G/run.qcow2"
	qemu-img create -q -f qcow2 -b "$G/prepared.qcow2" -F qcow2 "$G/run.qcow2"
	gpu_guest_boot "$G/run.qcow2"
}

# gpu_guest_configure <rendered profile> <node> <gpu count> <driver version>
gpu_guest_configure() {
	local S=$WORK/guest/stage
	cp "$1" "$S/config.yaml"
	printf 'NODE_NAME=%s\nGPU_COUNT=%s\nDRIVER_VERSION=%s\n' "$2" "$3" "$4" >"$S/guest.env"
	gpu_gssh 'rm -rf ~/stage; mkdir -p ~/stage' && tar -C "$S" -cf - --exclude gpu-health-monitor.tar . | gpu_gssh 'tar -C ~/stage -xf -'
	gpu_gssh 'sudo sh ~/stage/gpu-guest-setup.sh configure /home/debian/stage'
}

# gpu_guest_monitor <dcgmhealth dir> <args file>: (re)starts the gpu-health-monitor in the guest
# with the chart's ConfigMap files and arguments, as the kubelet starts the DaemonSet's pod.
gpu_guest_monitor() {
	local S=$WORK/guest/stage
	rm -rf "$S/dcgmhealth" && cp -r "$1" "$S/dcgmhealth" && cp "$2" "$S/ghm.args"
	tar -C "$S" -cf - dcgmhealth ghm.args guest.env build.env gpu-guest-setup.sh | gpu_gssh 'tar -C ~/stage -xf -' &&
		gpu_gssh 'sudo sh ~/stage/gpu-guest-setup.sh monitor /home/debian/stage'
}

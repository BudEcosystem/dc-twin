#!/usr/bin/env bash
# Q1 (docs/spec/11-decisions.md): does a BlueField BFB's own installer and DPU OS boot under QEMU
# aarch64, well enough to run a DPU agent or HBN? This boots the BFB DPF provisions, unmodified (the
# one exception, a kernel argument changed only when asked, is below):
#
#   1. lists the boot stream and extracts its Linux Image and installer initramfs (lib/bfb.py);
#   2. boots them on QEMU's `virt` machine with UEFI (edk2 for aarch64) and an empty NVMe disk, and
#      lets the BFB's installer run (on its defaults, or with a bf.cfg: BFCFG below);
#   3. boots the installed disk with the same UEFI variables, and records how far the DPU OS gets.
#
# The installer is done when IT says so ("INFO: Installation finished", "INFO: Rebooting...", then a
# reboot, which `-no-reboot` turns into QEMU exiting). grub-install prints "Installation finished.
# No error reported." twelve minutes in, while the installer is still writing grub.cfg: a first
# version of this script stopped there and booted a disk with no GRUB configuration, which lands at
# a bare `grub>` prompt. A failing installer says "ERR Installation failed" and reboots.
#
# BFCFG=<file> hands the installer a bf.cfg. On a BlueField it arrives behind the boot stream (DPF
# appends the one it generates, which is what the twin's BMC receives at P6); the installer sources
# it as /etc/bf.cfg from its initramfs root. QEMU has no boot FIFO, so this appends a second cpio
# archive holding etc/bf.cfg to a copy of the BFB's initramfs, which Linux unpacks over the first.
# Without it the installer runs on its defaults. BOOT_ONLY=1 boots the disk and UEFI variables an
# earlier run left in $Q1WORK again, without the installer.
#
# The installed DPU OS boots through the installer's UEFI entry, shim and GRUB, unmodified, and on
# QEMU's `virt` machine its kernel prints nothing at all: GRUB's command line carries the BFB's
# `earlycon=pl011,0x01000000` (the installer rewrites the address to BlueField-3's 0x13010000 only
# when it finds BlueField-3's ACPI table), and on `virt` that address is inside the firmware flash.
# QEMU's pflash trace shows it: the kernel writes "[" and then " " (0x20) to the UART's data
# register, the flash takes 0x20 for the first cycle of a CFI block erase and from then on answers
# every read with its status, 0xA0, and bit 5 of the UART's flag register is "transmit FIFO full":
# the boot CPU waits on the second character of its first line forever (39 million reads a minute).
# BOOT_EARLYCON=pl011,0x9000000 (`virt`'s PL011) boots the installed kernel and initramfs directly,
# read from the disk with debugfs, with GRUB's command line and that ONE argument changed. A boot
# whose console says nothing for $BOOT_SILENCE seconds is stopped.
#
# Timings (host seconds, *.milestones) and the serial logs land in $Q1WORK. Accelerated where the
# host can run the guest (HVF on Apple silicon, KVM on an aarch64 Linux host), else TCG (`-cpu
# neoverse-n1`), which is slow and said so. Apple silicon has no 64 KB translation granule: a
# 64K-page BFB (DPF's HBN guide's) does not boot under HVF at all; use the 4K-page build there.
#
#   Q1WORK=/tmp/q1 BFB_FILE=…/bf-bundle-…_64k_prod.bfb compat/dpf/q1.sh
#   BFB_URL=https://content.mellanox.com/… compat/dpf/q1.sh       # downloads it (1.7 GB)
#   Q1WORK=/tmp/q1 BOOT_ONLY=1 BOOT_EARLYCON=pl011,0x9000000 compat/dpf/q1.sh
#
# It needs about 3 GB for the extracted images and up to 20 GB for the installed disk, and guest
# memory for the installer, which unpacks its 1.6 GB initramfs into a RAM file system limited to
# half the guest's memory: 6 GB is enough for the 4K-page BFB (4 GB is not), the 64K-page one needs
# 8 GB (5 and 6 GB fail with "Initramfs unpacking failed: write error").
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
Q1WORK=${Q1WORK:-$(mktemp -d)}
GUIDE_BFB_URL="https://content.mellanox.com/BlueField/BFBs/Ubuntu24.04/bf-bundle-3.5.0-89_26.07_ubuntu-24.04_64k_prod.bfb"
BFB_URL=${BFB_URL:-$GUIDE_BFB_URL}
INSTALL_TIMEOUT=${INSTALL_TIMEOUT:-14400}
BOOT_TIMEOUT=${BOOT_TIMEOUT:-7200}
BOOT_SETTLE=${BOOT_SETTLE:-900}
BOOT_SILENCE=${BOOT_SILENCE:-900}
MEM=${MEM:-8192}
SMP=${SMP:-4}
# Under TCG: Neoverse N1, an ARMv8.2 core like BlueField-3's Cortex-A78 (with the 64 KB granule the
# guide's BFB needs). `-cpu max` leaves edk2 (AAVMF 2025.11) printing nothing past its banner on
# QEMU 10.2's TCG, with or without pauth-impdef; cortex-a57 and neoverse-n1 boot it.
TCG_CPU=${TCG_CPU:-neoverse-n1}
# TCG's translation cache, in MiB: QEMU's default on a 64-bit host is up to 1 GiB, which a long
# installer fills and which counts against a memory cap as much as guest RAM does
TB_MB=${TB_MB:-256}
# INSTALL_ONLY=1 stops after the installer (the install phase measured on its own)
INSTALL_ONLY=${INSTALL_ONLY:-}
# RAM_FILE=<path> backs the guest's RAM with that file (QEMU memory-backend-file, shared), so a
# host that caps the job's memory can write guest pages back to disk instead of killing QEMU
RAM_FILE=${RAM_FILE:-}
mkdir -p "$Q1WORK/images" "$Q1WORK/logs"
Q1WORK=$(cd "$Q1WORK" && pwd)
out="$Q1WORK/results.txt"
if [ -n "${BOOT_ONLY:-}" ]; then echo "--- boot only, $(date -u +%FT%TZ)" >>"$out"; else : >"$out"; fi
note() { echo "$*" | tee -a "$out" >&2; }
now() { date +%s; }

if [ -n "${BOOT_ONLY:-}" ]; then
	:
elif [ -z "${BFB_FILE:-}" ]; then
	BFB_FILE="$Q1WORK/dl/$(basename "$BFB_URL")"
	if [ ! -s "$BFB_FILE" ]; then
		mkdir -p "$Q1WORK/dl"
		note "downloading $BFB_URL"
		curl -sSfL -C - -o "$BFB_FILE" "$BFB_URL"
	fi
fi
if [ -z "${BOOT_ONLY:-}" ]; then
	note "BFB: $BFB_FILE ($(wc -c <"$BFB_FILE") bytes)"
	python3 -I "$here/lib/bfb.py" "$BFB_FILE" "$Q1WORK/images" 62,63 | tee "$Q1WORK/logs/bfb-images.txt" >&2
	grep -E "page size|image |initramfs" "$Q1WORK/logs/bfb-images.txt" >>"$out"
fi

arch=$(uname -m)
case "$(uname -s)/$arch" in
Darwin/arm64) accel=(-accel hvf -cpu host) ;;
Linux/aarch64) if [ -w /dev/kvm ]; then accel=(-accel kvm -cpu host); else accel=(-accel "tcg,thread=multi,tb-size=$TB_MB" -cpu "$TCG_CPU"); fi ;;
*) accel=(-accel "tcg,thread=multi,tb-size=$TB_MB" -cpu "$TCG_CPU") ;;
esac
note "host $(uname -s) $arch: QEMU ${accel[*]}"
code="" vars=""
for c in /usr/share/AAVMF/AAVMF_CODE.no-secboot.fd /usr/share/AAVMF/AAVMF_CODE.fd /opt/homebrew/share/qemu/edk2-aarch64-code.fd /usr/share/qemu/edk2-aarch64-code.fd; do
	[ -f "$c" ] && { code=$c; break; }
done
for v in /usr/share/AAVMF/AAVMF_VARS.fd /opt/homebrew/share/qemu/edk2-arm-vars.fd /usr/share/qemu/edk2-arm-vars.fd; do
	[ -f "$v" ] && { vars=$v; break; }
done
[ -n "$code" ] && [ -n "$vars" ] || { echo "no edk2 aarch64 firmware found" >&2; exit 1; }
disk="$Q1WORK/dpu-nvme.qcow2"
if [ -n "${BOOT_ONLY:-}" ]; then
	for f in code.fd vars.fd dpu-nvme.qcow2; do
		[ -s "$Q1WORK/$f" ] || { echo "BOOT_ONLY: no $Q1WORK/$f from an earlier run" >&2; exit 1; }
	done
else
	cp "$code" "$Q1WORK/code.fd"
	cp "$vars" "$Q1WORK/vars.fd"
	size=$(wc -c <"$Q1WORK/code.fd")
	for f in code.fd vars.fd; do truncate -s "$size" "$Q1WORK/$f" 2>/dev/null || true; done
	qemu-img create -q -f qcow2 "$disk" 40G
fi

machine=(-M virt "${accel[@]}" -smp "$SMP" -m "$MEM" -nographic -no-reboot -monitor none -display none
	-drive "if=pflash,format=raw,readonly=on,file=$Q1WORK/code.fd" -drive "if=pflash,format=raw,file=$Q1WORK/vars.fd"
	-drive "if=none,id=d0,file=$disk,format=qcow2" -device nvme,serial=dpuq1,drive=d0
	-netdev user,id=n0,restrict=on -device virtio-net-pci,netdev=n0)
if [ -n "$RAM_FILE" ]; then
	machine+=(-object "memory-backend-file,id=ram0,size=${MEM}M,mem-path=$RAM_FILE,share=on" -machine memory-backend=ram0)
	note "guest RAM: ${MEM} MB backed by $RAM_FILE"
fi

# run_until <log> <timeout> <stop-regex> <qemu args...>: runs QEMU until the regex appears in the
# serial log, QEMU exits, or the timeout; prints the seconds taken and why it stopped. After a
# match QEMU gets a minute to exit by itself (the installer reboots, and -no-reboot makes that an
# exit) before it is killed, or $settle seconds if the log matches $settle_on (a booted OS keeps
# writing to its console after the login prompt: cloud-init, late units). Each regex in $milestones is timed, in host seconds, the first time it
# appears: "<seconds> <regex>" lines in <log>.milestones.
milestones=()
settle=60 settle_on='^$' silence=0
run_until() {
	local log=$1 limit=$2 stop=$3
	shift 3
	: >"$log"
	: >"$log.milestones"
	rm -f "$log.hwm" "$log.anon"
	local t0 qpid why=timeout m i size=0 grew
	local -a seen=()
	t0=$(now)
	grew=$t0
	qemu-system-aarch64 "$@" -serial "file:$log" >"$log.qemu" 2>&1 &
	qpid=$!
	tick() {
		for i in "${!milestones[@]}"; do
			m=${milestones[$i]}
			if [ -z "${seen[$i]:-}" ] && grep -a -q -E "$m" "$log"; then
				seen[$i]=1
				echo "$(($(now) - t0)) $m" >>"$log.milestones"
			fi
		done
		# QEMU's peak resident memory so far (Linux), and the most of it that was anonymous (what
		# a memory cap cannot write back to a file)
		local hwm anon
		hwm=$(awk '/^VmHWM:/ {print $2}' "/proc/$qpid/status" 2>/dev/null || true)
		anon=$(awk '/^RssAnon:/ {print $2}' "/proc/$qpid/status" 2>/dev/null || true)
		[ -n "$hwm" ] && echo "$hwm" >"$log.hwm"
		if [ -n "$anon" ] && [ "$anon" -gt "$(cat "$log.anon" 2>/dev/null || echo 0)" ]; then echo "$anon" >"$log.anon"; fi
	}
	while kill -0 "$qpid" 2>/dev/null; do
		tick
		if grep -a -q -E "$stop" "$log"; then why="matched"; break; fi
		if [ $(($(now) - t0)) -ge "$limit" ]; then break; fi
		if [ "$(wc -c <"$log")" -ne "$size" ]; then size=$(wc -c <"$log"); grew=$(now); fi
		if [ "$silence" -gt 0 ] && [ $(($(now) - grew)) -ge "$silence" ]; then
			why="console silent for ${silence} s"
			break
		fi
		sleep 5
	done
	if [ "$why" = matched ]; then
		local wait_s=60
		grep -a -q -E "$settle_on" "$log" && wait_s=$settle
		for _ in $(seq $((wait_s / 5))); do kill -0 "$qpid" 2>/dev/null || break; sleep 5; tick; done
	fi
	if kill -0 "$qpid" 2>/dev/null; then
		kill "$qpid" 2>/dev/null || true
		[ "$why" = matched ] && why="matched; killed"
	elif [ "$why" = matched ]; then
		why="matched; qemu exited"
	else
		why="qemu exited"
	fi
	wait "$qpid" 2>/dev/null || true
	if [ -s "$log.hwm" ]; then
		echo "QEMU peak RSS $(( $(cat "$log.hwm") / 1024 )) MiB, of it anonymous at most $(( $(cat "$log.anon" 2>/dev/null || echo 0) / 1024 )) MiB" >>"$log.milestones"
	fi
	echo "$(($(now) - t0))|$why"
}

# ---------------------------------------------------------------- the installer

if [ -z "${BOOT_ONLY:-}" ]; then
	initrd="$Q1WORK/images/initramfs.bin"
	if [ -n "${BFCFG:-}" ]; then
		rm -rf "$Q1WORK/cfgroot" && mkdir -p "$Q1WORK/cfgroot/etc"
		cp "$BFCFG" "$Q1WORK/cfgroot/etc/bf.cfg"
		(cd "$Q1WORK/cfgroot" && printf 'etc\netc/bf.cfg\n' | cpio -o -H newc --quiet) >"$Q1WORK/images/bfcfg.cpio"
		cat "$initrd" "$Q1WORK/images/bfcfg.cpio" >"$Q1WORK/images/initramfs-bfcfg.bin"
		initrd="$Q1WORK/images/initramfs-bfcfg.bin"
		note "bf.cfg: $(basename "$BFCFG") ($(wc -c <"$BFCFG") bytes) as /etc/bf.cfg in the installer's root"
	else
		note "bf.cfg: none; the installer runs on its defaults"
	fi
	installed="INFO: [A-Za-z]+ installation completed"
	note "installer: the BFB's Image and initramfs, UEFI, ${MEM} MB, NVMe 40G"
	milestones=("Linux version" "Run /init" "Installing ubuntu" "OS installation target" "Extracting /" "Updating .* initramfs"
		"Creating GRUB configuration" "$installed" "INFO: Installation finished" "ERR Installation failed")
	r=$(run_until "$Q1WORK/logs/install.log" "$INSTALL_TIMEOUT" "INFO: Installation finished|Installation finished\\. Rebooting|ERR Installation failed|Failed to install ubuntu|Kernel panic" \
		"${machine[@]}" -kernel "$Q1WORK/images/image.bin" -initrd "$initrd" \
		-append "console=ttyAMA0 earlycon=pl011,0x9000000 initrd=initramfs")
	note "installer: ${r%%|*} s (${r#*|})"
	sed 's/^/  at /' "$Q1WORK/logs/install.log.milestones" | tee -a "$out" >&2
	for m in "Linux version" "OS installation target" "$installed" "INFO: Installation finished" "ERR " "Failed to install" "Kernel panic"; do
		t=$(grep -a -m1 -E "$m" "$Q1WORK/logs/install.log" | tr -d '\r' | cut -c1-160 || true)
		[ -n "$t" ] && note "  $t"
	done
	grep -a -E "ERROR|ERR |WARNING|No such|not found" "$Q1WORK/logs/install.log" | tr -d '\r' | sort | uniq -c | sort -rn | head -20 | sed 's/^/  /' >>"$out"
	note "disk after install: $(qemu-img info "$disk" | awk '/disk size/ {print $3, $4}')"
	rm -f "$Q1WORK/images/initramfs-bfcfg.bin"

	# The OS image is on the disk once the installer logs "installation completed" (it has synced);
	# what the installer does after that is firmware (ATF/UEFI, BMC, NIC), which may fail here without
	# failing the DPU OS. Boot whatever was installed.
	if ! grep -a -q -E "$installed" "$Q1WORK/logs/install.log"; then
		note "the installer did not complete the OS image; the installed-OS boot is not attempted"
		exit 0
	fi
	if [ -n "$INSTALL_ONLY" ]; then
		note "INSTALL_ONLY: the installed-OS boot is not attempted"
		exit 0
	fi
fi

# ---------------------------------------------------------------- the DPU OS

boot=()
if [ -n "${BOOT_EARLYCON:-}" ]; then
	# The installed kernel, initramfs and GRUB command line, read from the disk's first Linux
	# partition without mounting it (a sparse raw copy, then debugfs).
	ins="$Q1WORK/inspect"
	mkdir -p "$ins"
	qemu-img convert -U -f qcow2 -O raw "$disk" "$ins/disk.raw"
	off=$(python3 -I - "$ins/disk.raw" <<'PY'
import struct, sys
b = open(sys.argv[1], "rb").read(512 * 64)
lba, n, size = struct.unpack_from("<QII", b, 512 + 72)
linux = bytes.fromhex("af3dc60f838472478e793d69d8477de4")  # 0FC63DAF-8483-4772-8E79-3D69D8477DE4
for i in range(n):
    e = b[lba * 512 + i * size:lba * 512 + (i + 1) * size]
    if e[:16] == linux:
        print(struct.unpack_from("<Q", e, 32)[0] * 512)
        break
PY
)
	fs="$ins/disk.raw?offset=$off"
	debugfs=$(command -v debugfs || echo /sbin/debugfs)
	"$debugfs" -R "cat /boot/grub/grub.cfg" "$fs" 2>/dev/null >"$ins/grub.cfg"
	line=$(awk '$1 == "linux" { sub(/^[ \t]*linux[ \t]+/, ""); print; exit }' "$ins/grub.cfg")
	initrd=$(awk '$1 == "initrd" { print $2; exit }' "$ins/grub.cfg")
	[ -n "$line" ] && [ -n "$initrd" ] || { echo "no linux/initrd line in the installed grub.cfg" >&2; exit 1; }
	"$debugfs" -R "dump ${line%% *} $ins/vmlinuz" "$fs" 2>/dev/null
	"$debugfs" -R "dump $initrd $ins/initrd" "$fs" 2>/dev/null
	rm -f "$ins/disk.raw"
	if [ "$(head -c 2 "$ins/vmlinuz" | od -An -tx1 | tr -d ' ')" = 1f8b ]; then
		gzip -dc <"$ins/vmlinuz" >"$ins/vmlinuz.img" && mv "$ins/vmlinuz.img" "$ins/vmlinuz"
	fi
	args=$(echo "${line#* }" | sed -E 's/[$][A-Za-z_]+//g; s/earlycon=[^ ]*/earlycon='"$BOOT_EARLYCON"'/; s/ +$//')
	note "DPU OS: GRUB's entry, ${line%% *}, booted directly with earlycon=$BOOT_EARLYCON (the one change)"
	note "  command line: $args"
	boot=(-kernel "$ins/vmlinuz" -initrd "$ins/initrd" -append "$args")
else
	note "DPU OS: boot the NVMe disk through UEFI (the installer's boot entry)"
fi
milestones=("BdsDxe: starting" "GNU GRUB" "Linux version" "Run /init" "systemd\\[1\\]|Welcome to" "Reached target.*Basic System"
	"Reached target.*Multi-User" "cloud-init.*finished|Cloud-init.*finished" "login:" "Kernel panic" "grub>")
settle=$BOOT_SETTLE settle_on="login:" silence=$BOOT_SILENCE
r=$(run_until "$Q1WORK/logs/boot.log" "$BOOT_TIMEOUT" "login:|Kernel panic|^grub>|grub> *$" "${machine[@]}" ${boot[@]+"${boot[@]}"})
note "DPU OS boot: ${r%%|*} s (${r#*|})"
sed 's/^/  at /' "$Q1WORK/logs/boot.log.milestones" | tee -a "$out" >&2
for m in "Linux version" "Reached target.*Multi-User|Reached target.*Graphical" "cloud-init.*finished|Cloud-init.*finished" \
	"containerd|kubelet" "openvswitch|ovs-vswitchd" "mlx5|rshim|tmfifo|oob_net0" "login:" "Kernel panic"; do
	grep -a -E "$m" "$Q1WORK/logs/boot.log" | head -3 | cut -c1-160 | sed 's/^/  /' >>"$out" || true
done
grep -a -E "Failed to start|FAILED" "$Q1WORK/logs/boot.log" | sed 's/\x1b\[[0-9;]*m//g' | sort -u | head -20 | sed 's/^/  /' >>"$out" || true
note "done: $out"

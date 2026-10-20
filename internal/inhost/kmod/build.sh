#!/usr/bin/env bash
# Builds dctwin-kmsg.ko, the kernel half of the in-host agent (dctwin_kmsg.c, D50), for the kernel
# of a pinned os: twin guest image, and puts it beside the image, where the vm adapter adds it to the
# guest's initramfs and the agent loads it:
#
#   internal/inhost/kmod/build.sh <guest-image-dir>      # e.g. $DCTWIN_ASSETS/alpine-netboot@3.24.2-x86_64
#
# The build runs in a throwaway Alpine container (Docker or Podman; ENGINE overrides) of the
# engine's own architecture, which must be the guest's. A module already beside the image, built
# from the same sources for the same kernel, is kept. A module must be built against its kernel's headers, and
# Alpine's mirror keeps only the newest linux-virt-dev of a branch: the netboot image's kernel
# (6.18.52 for 3.24.2) has none any more. So the module is built against the branch's current
# headers (the same stable series), carrying the guest kernel's release string, and is accepted
# only if the kernel would accept it: its vermagic must equal that of the guest's own modules (from
# the image's modloop), and the version (CRC) of every kernel symbol it uses must equal the one the
# guest's own modules were linked against. A symbol no guest module uses is reported, not trusted;
# the kernel still checks it when the agent loads the module, and the agent falls back to
# /dev/kmsg if it is refused. Writes dctwin-kmsg.ko and dctwin-kmsg.ko.txt (how it was built and
# checked) into <guest-image-dir>.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
dir=${1:?usage: build.sh <guest-image-dir>}
dir=$(cd "$dir" && pwd)
[ -f "$dir/modloop-virt" ] || { echo "$dir has no modloop-virt (an Alpine netboot guest image)" >&2; exit 1; }
IMAGE=${KMOD_BUILD_IMAGE:-docker.io/library/alpine:3.24}
ENGINE=${ENGINE:-$(command -v docker || command -v podman || true)}
[ -n "$ENGINE" ] || { echo "Docker or Podman is needed to build the module" >&2; exit 1; }
src=$(cat "$here/Kbuild" "$here/dctwin_kmsg.c" "$0" | sha256sum | cut -c1-16)
if [ -s "$dir/dctwin-kmsg.ko" ] && grep -qx "sources: $src" "$dir/dctwin-kmsg.ko.txt" 2>/dev/null; then
	cat "$dir/dctwin-kmsg.ko.txt" >&2
	exit 0
fi
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT

tar -C "$here" -cf - Kbuild dctwin_kmsg.c |
	"$ENGINE" run -i --rm --name "dctwin-kmod-build-$$" --memory 1g --network host -v "$dir/modloop-virt:/g/modloop-virt:ro" "$IMAGE" sh -ec '
		mkdir -p /src && tar -xf - -C /src
		apk add -q linux-virt-dev gcc make musl-dev squashfs-tools kmod >/dev/null
		unsquashfs -q -d /m /g/modloop-virt >/dev/null
		rel=$(ls /m/modules | grep -v firmware | head -1)
		hdr=$(ls -d /usr/src/linux-headers-*-virt | head -1)
		hrel=${hdr#/usr/src/linux-headers-}
		cp -a "$hdr" /k
		if [ "$hrel" != "$rel" ]; then
			sed -i "s/\"$hrel\"/\"$rel\"/" /k/include/generated/utsrelease.h
			[ -f /k/include/config/kernel.release ] && echo "$rel" >/k/include/config/kernel.release
		fi
		make -s -C /k M=/src modules >&2
		ko=/src/dctwin-kmsg.ko
		strip --strip-debug "$ko"
		ref=$(find /m/modules/$rel -name "*.ko*" | head -1)
		vm=$(modinfo -F vermagic "$ko"); vg=$(modinfo -F vermagic "$ref")
		{
			echo "built against: linux-virt-dev ($hrel headers), $(gcc --version | head -1)"
			echo "guest kernel:  $rel"
			echo "vermagic:      $vm"
		} >/src/info
		[ "$vm" = "$vg" ] || { echo "vermagic \"$vm\" differs from the guest modules: \"$vg\"" >&2; exit 1; }
		find /m/modules/$rel -name "*.ko*" -exec modprobe --dump-modversions {} \; 2>/dev/null | sort -u -k2,2 >/tmp/guest.crc
		bad=0; ok=0; unv=""
		modprobe --dump-modversions "$ko" >/tmp/mine.crc
		while read -r crc sym; do
			g=$(awk -v s="$sym" "\$2 == s { print \$1; exit }" /tmp/guest.crc)
			if [ -z "$g" ]; then unv="$unv $sym"
			elif [ "$g" = "$crc" ]; then ok=$((ok + 1))
			else echo "symbol $sym: version $crc, the guest kernel has $g" >&2; bad=$((bad + 1)); fi
		done </tmp/mine.crc
		echo "symbol versions: $ok equal to those the guest modules use, $bad different, unverified:${unv:- none}" >>/src/info
		[ "$bad" = 0 ] || exit 1
		tar -C /src -cf - dctwin-kmsg.ko info
	' | tar -C "$out" -xf -
[ -s "$out/dctwin-kmsg.ko" ] || { echo "the module was not built" >&2; exit 1; }
cp "$out/dctwin-kmsg.ko" "$dir/dctwin-kmsg.ko.partial" && mv "$dir/dctwin-kmsg.ko.partial" "$dir/dctwin-kmsg.ko"
{ cat "$out/info"; echo "sources: $src"; } >"$dir/dctwin-kmsg.ko.txt"
cat "$dir/dctwin-kmsg.ko.txt" >&2

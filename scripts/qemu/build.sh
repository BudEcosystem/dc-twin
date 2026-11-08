#!/usr/bin/env bash
# Builds the QEMU the twin's `vm` hosts need for their drives (docs/reviews/2026-11-08-storage-deep-dive.md
# S-15, S-16): QEMU 11.1 or later, whose NVMe device takes the drive's model and firmware strings,
# with 0001-hw-nvme-namespace-management.patch applied. Unpatched, QEMU's NVMe controller
# advertises namespace management (OACS bit 3) and implements neither create nor delete, and reports
# no total capacity (TNVMCAP 0), so a stack that rebuilds namespaces when it sanitises a drive
# (NICo's scout: format -s2, delete-ns, create-ns, attach-ns) fails. The patch implements Namespace
# Management over the subsystem's own backing, reports TNVMCAP and UNVMCAP, answers Identify
# Namespace for the broadcast NSID, attaches (and announces) a namespace plugged into a controller
# that is already running, and adds the controller property `ns-mgmt` (default on), by which the
# twin recognises a patched build (`-device nvme,help`).
#
#   scripts/qemu/build.sh [prefix]          # default: ./qemu-11.1.2-dctwin; x86_64 and aarch64
#   TARGETS=x86_64-softmmu scripts/qemu/build.sh /opt/dctwin-qemu
#
# Then point an estate at it: spec.adapters.vm.qemu: <prefix>/bin. Needs QEMU's build
# dependencies (ninja, meson, flex, bison, glib, pixman, slirp, libfdt); on a host without them,
# run it in a container that has them, with the prefix mounted at the same path.
set -euo pipefail
VER=${QEMU_VERSION:-11.1.2}
PREFIX=${1:-$PWD/qemu-$VER-dctwin}
TARGETS=${TARGETS:-x86_64-softmmu,aarch64-softmmu}
HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"
curl -fsSLO "https://download.qemu.org/qemu-$VER.tar.xz"
tar xf "qemu-$VER.tar.xz"
cd "qemu-$VER"
patch -p1 < "$HERE/0001-hw-nvme-namespace-management.patch"
./configure --prefix="$PREFIX" --target-list="$TARGETS" --enable-slirp --disable-docs --disable-werror \
	--disable-gtk --disable-sdl --disable-opengl --disable-vnc
make -j"$(nproc 2>/dev/null || sysctl -n hw.ncpu)"
make install
"$PREFIX/bin/qemu-system-${TARGETS%%-*}" -device nvme,help | grep -q 'ns-mgmt=' && echo "built $PREFIX (nvme ns-mgmt)"

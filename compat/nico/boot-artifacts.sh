#!/usr/bin/env bash
# compat/nico/boot-artifacts.sh — build, from NICo's source at the pin, what NICo serves a host that
# network-boots (its iPXE and a scout boot image) and the NICo programs for Linux the Ready path
# runs, in one container of NICo's build distribution (dctwin-nico-build, ubuntu:24.04). The
# architecture is this host's (x86_64 or aarch64), which is the vm host's under KVM or HVF.
# README.md (the Ready path) explains why each piece is built this way.
#
#   compat/nico/boot-artifacts.sh all        up ipxe rust image down
#   compat/nico/boot-artifacts.sh <step>...
#
#   up      the build container and its volume (the cargo home, the target directory)
#   ipxe    NICo's iPXE: upstream iPXE at the submodule commit NICo pins, NICo's config headers,
#           patches and embedded script, NICo's make target for the architecture
#           (pxe/Makefile.toml: bin-x86_64-efi/snponly.efi DEBUG=tls, or
#           bin-arm64-efi/snponly.efi DEBUG=snp,tls) → $OUT/ipxe.efi
#   rust    RUST_PACKAGES with NICo's pinned toolchain, unmodified, in the container; their
#           binaries (RUST_BINS) → $OUT. By default carbide-pxe (`carbide`) and carbide-scout
#           (`forge-scout`); run.sh adds nico-api, the dhcp-server and the admin CLI on Linux
#   rust-cross  carbide-pxe and forge-scout cross-compiled for linux/arm64 on a macOS host
#           (cargo-zigbuild, glibc 2.39), the target's libudev and tpm2-tss from the build
#           container as a sysroot (pkg-config-sysroot.py): for a Docker VM too loaded to build in
#   image   the host's boot image $OUT/scout.efi: a UKI of Ubuntu noble (NICo's scout image
#           distribution) with a 4K-page kernel (on x86_64 the one NICo's scout-oss-x86_64 profile
#           pins), forge-scout in /opt/forge and NICo's own scout unit, drop-ins and scripts from
#           pxe/ and crates/scout/misc, as one initramfs. Not NICo's image: README.md says what
#           differs and why.
#   down    remove the containers and the volume
#
# Environment: NICO_SRC (required: the extracted infra-controller tree at the pin), OUT (default
# $WORK/boot), WORK (default ${TMPDIR:-/tmp}/dctwin-nico), JOBS [4], BUILD_MEMORY (a memory limit
# for the build container; none by default), BUILD_CPUS (a CPU limit for it), RUST_PACKAGES, RUST_BINS, RUST_FEATURES_ARGS.
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
WORK=${WORK:-${TMPDIR:-/tmp}/dctwin-nico}
WORK=${WORK%/}
OUT=${OUT:-$WORK/boot}
JOBS=${JOBS:-4}
BUILD_MEMORY=${BUILD_MEMORY:-}
BUILD_CPUS=${BUILD_CPUS:-}   # a CPU limit for the build container (docker --cpus); none by default
C=dctwin-nico-build
IMG=dctwin-nico-image
VOL=dctwin-nico-build
# the iPXE commit NICo pins as its pxe/ipxe/upstream submodule at NICO_SHA (read from GitHub's
# tree for that commit; the source tarball does not carry submodules)
IPXE_SHA=${IPXE_SHA:-bbd7821bd42da5456ee068a471ef73d525ea26a1}
RUST_TOOLCHAIN=${RUST_TOOLCHAIN:-1.97.1}   # NICo's rust-toolchain.toml
UBUNTU=${UBUNTU:-noble}
BASE=${BASE:-docker.io/library/ubuntu:24.04}   # fully qualified: podman resolves no short names
RUST_PACKAGES=${RUST_PACKAGES:-carbide-pxe carbide-scout}
RUST_BINS=${RUST_BINS:-carbide forge-scout}
case $(uname -m) in
  x86_64 | amd64) ARCH=x86_64 DEB=amd64 IPXE_BIN=bin-x86_64-efi IPXE_DEBUG=tls
    # NICo's scout-oss-x86_64 profile pins this kernel (4K pages)
    KERNEL=${KERNEL:-"linux-image-6.8.0-111-generic linux-modules-6.8.0-111-generic"} ;;
  arm64 | aarch64) ARCH=aarch64 DEB=arm64 IPXE_BIN=bin-arm64-efi IPXE_DEBUG=snp,tls
    # NICo's scout-oss-aarch64 profile pins linux-image-6.17.0-1032-nvidia-64k, a 64K-page kernel
    # Hypervisor.framework cannot run; Ubuntu's generic arm64 kernel has 4K pages
    KERNEL=${KERNEL:-linux-image-virtual} ;;
  *) echo "boot-artifacts.sh: no build for $(uname -m)" >&2; exit 1 ;;
esac

die() { echo "boot-artifacts.sh: $*" >&2; exit 1; }
say() { printf '\n== %s\n' "$*"; }
[ -n "${NICO_SRC:-}" ] && [ -d "$NICO_SRC/crates/pxe" ] || die "NICO_SRC must name the infra-controller source tree"
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
NICO_SRC=$(cd "$NICO_SRC" && pwd)
inside() { docker exec -i "$C" bash -euo pipefail -c "$1"; }
# The Docker host may be a VM that shares only some host directories (colima: the home directory),
# so the source goes in, and the artifacts come out, by docker cp.
fetch_out() { local f; for f in "$@"; do docker cp "$C:/work/out/$f" "$OUT/$f" >/dev/null || die "copy $f out"; done; }

step_up() {
  say "build container $C (ubuntu:24.04 $DEB${BUILD_MEMORY:+, memory limit $BUILD_MEMORY}), volume $VOL"
  docker container inspect "$C" >/dev/null 2>&1 && { echo "  $C is running"; return; }
  docker volume create "$VOL" >/dev/null
  docker run -d --name "$C" --platform "linux/$DEB" ${BUILD_MEMORY:+--memory "$BUILD_MEMORY" --memory-swap "$BUILD_MEMORY"} ${BUILD_CPUS:+--cpus "$BUILD_CPUS"} \
    -v "$VOL:/work" "$BASE" sleep infinity >/dev/null || die "docker run"
  # NICo's source, without build output, read from /src in the container
  inside 'mkdir -p /src /work/out'
  # (COPYFILE_DISABLE: macOS's tar would add AppleDouble ._ files, which sqlx reads as migrations)
  COPYFILE_DISABLE=1 tar -C "$NICO_SRC" --exclude ./target --exclude ./.git --exclude '._*' -cf - . | docker exec -i "$C" tar -C /src -xf - || die "copy the source in"
  inside 'export DEBIAN_FRONTEND=noninteractive; apt-get update -qq &&
    apt-get install -y -qq --no-install-recommends build-essential pkg-config cmake clang libclang-dev git curl ca-certificates \
      libudev-dev libtss2-dev protobuf-compiler libprotobuf-dev libssl-dev liblzma-dev perl systemd-ukify systemd-boot-efi \
      zstd cpio file binutils >/dev/null && apt-get clean && rm -rf /var/lib/apt/lists/*' || die "packages"
}

step_ipxe() {
  say "NICo's iPXE for $ARCH EFI (ipxe $IPXE_SHA with NICo's headers, patches and embedded script)"
  # the patches NICo's Makefile applies for the architecture (ipxe-x86_64 / ipxe-aarch64 tasks)
  local patches="0001-efi-Add-TPM-measurement-API-via-TCG-v1-and-TCG-v2-EF.patch 0003-efi-prevent-load-image-watchdog-timeout.patch 0004-http-ipv6-source.patch"
  [ "$ARCH" = aarch64 ] && patches="0001-fix-update-to-allow-iPXE-to-boot-on-BlueField-NICs.patch 0004-http-ipv6-source.patch"
  inside "
    mkdir -p /work/ipxe && cd /work/ipxe
    if [ ! -d upstream/src ]; then
      curl -fsSL https://codeload.github.com/ipxe/ipxe/tar.gz/$IPXE_SHA -o ipxe.tgz
      mkdir upstream && tar xzf ipxe.tgz -C upstream --strip-components 1 && rm ipxe.tgz
    fi
    cd upstream
    cp /src/pxe/ipxe/local/*.h src/config/local/
    for p in $patches; do
      if patch --batch -p1 --forward --dry-run -i /src/pxe/ipxe/local/\$p >/dev/null; then patch --batch -p1 --forward -i /src/pxe/ipxe/local/\$p >/dev/null
      elif ! patch --batch -p1 -R --dry-run -i /src/pxe/ipxe/local/\$p >/dev/null; then echo \"patch \$p does not apply\"; exit 1; fi
    done
    cd src && make -s -j$JOBS $IPXE_BIN/snponly.efi VERSION=nico-02b92f1 EMBED=/src/pxe/ipxe/local/embed.ipxe DEBUG=$IPXE_DEBUG
    cp $IPXE_BIN/snponly.efi /work/out/ipxe.efi
  " || die "ipxe"
  fetch_out ipxe.efi
  ls -l "$OUT/ipxe.efi" | awk '{print "  ipxe.efi", $5, "bytes"}'
}

step_rust() {
  say "$RUST_PACKAGES for linux/$DEB (rust $RUST_TOOLCHAIN, -j$JOBS)"
  local pk="" p
  for p in $RUST_PACKAGES; do pk="$pk -p $p"; done
  inside "
    export CARGO_HOME=/work/cargo RUSTUP_HOME=/work/rustup PATH=/work/cargo/bin:\$PATH
    [ -x /work/cargo/bin/cargo ] || curl -fsSL https://sh.rustup.rs | sh -s -- -y -q --profile minimal --default-toolchain $RUST_TOOLCHAIN --no-modify-path
    export CARGO_TARGET_DIR=/work/target CARGO_PROFILE_DEV_DEBUG=0 CARGO_INCREMENTAL=0 CARGO_NET_GIT_FETCH_WITH_CLI=true
    cd /src && cargo build --locked -j$JOBS $pk ${RUST_FEATURES_ARGS:-} 2>&1 | grep -vE '^ +(Compiling|Downloaded|Downloading)' | tail -20
    test \${PIPESTATUS[0]} -eq 0
    for b in $RUST_BINS; do cp /work/target/debug/\$b /work/out/ && strip /work/out/\$b; done
  " || die "rust"
  fetch_out $RUST_BINS
  for p in $RUST_BINS; do ls -l "$OUT/$p" | awk '{print "  " $NF, $5, "bytes"}'; done
}

step_rust_cross() {
  say "carbide-pxe and forge-scout for linux/arm64, cross-compiled here (rust $RUST_TOOLCHAIN, cargo-zigbuild)"
  local sysroot=$WORK/sysroot-arm64 tools=${TOOLS:-}
  [ -n "$tools" ] && export PATH=$tools/zig:$tools/czb/bin:$PATH
  command -v zig >/dev/null && command -v cargo-zigbuild >/dev/null || die "rust-cross needs zig and cargo-zigbuild"
  if [ ! -f "$sysroot/usr/include/libudev.h" ]; then
    # the target's native libraries scout and carbide-pxe link, as Ubuntu noble arm64 has them
    inside 'cd / && tar cf /work/out/sysroot.tar usr/include/libudev.h usr/include/tss2 usr/lib/aarch64-linux-gnu/pkgconfig \
      $(ls -d usr/lib/aarch64-linux-gnu/lib{udev,tss2,cap,crypto,ssl,json-c,curl,uuid}* 2>/dev/null)' || die "sysroot"
    mkdir -p "$sysroot" && docker cp "$C:/work/out/sysroot.tar" "$WORK/sysroot.tar" >/dev/null && tar xf "$WORK/sysroot.tar" -C "$sysroot" && rm -f "$WORK/sysroot.tar"
    inside 'rm -f /work/out/sysroot.tar'
  fi
  local target=${XTARGET_DIR:-$WORK/xtarget}
  ( cd "$NICO_SRC" &&
    RUSTUP_TOOLCHAIN=$RUST_TOOLCHAIN PKG_CONFIG="$here/pkg-config-sysroot.py" PKG_CONFIG_ALLOW_CROSS=1 \
    PKG_CONFIG_LIBDIR=$sysroot/usr/lib/aarch64-linux-gnu/pkgconfig PKG_CONFIG_SYSROOT_DIR=$sysroot \
    CARGO_TARGET_DIR=$target CARGO_PROFILE_DEV_DEBUG=0 CARGO_INCREMENTAL=0 CARGO_NET_GIT_FETCH_WITH_CLI=true \
    cargo zigbuild --locked -j"$JOBS" -p carbide-pxe -p carbide-scout --target aarch64-unknown-linux-gnu.2.39 2>&1 |
    grep -vE '^ +(Compiling|Downloaded|Downloading)' | tail -20 ) || die "cargo zigbuild"
  cp "$target/aarch64-unknown-linux-gnu/debug/carbide" "$target/aarch64-unknown-linux-gnu/debug/forge-scout" "$OUT/" || die "the binaries"
}

# The image is assembled in a container of NICo's scout distribution (ubuntu:24.04, Ubuntu noble)
# with apt, then exported as one initramfs: no chroot or loop device, so no privileges.
# NICo's scout-oss package list (pxe/mkosi.profiles/scout-oss-*/mkosi.conf), less what serves
# hardware the VM does not have (the NVIDIA driver, DCGM, MFT, InfiniBand, ipmitool: no GPU, HCA or
# IPMI device is in the guest) and what scout never runs (compilers, cloud-init, a container
# runtime, debugging and benchmarking tools).
IMAGE_PACKAGES=${IMAGE_PACKAGES:-"systemd systemd-sysv systemd-resolved udev dbus kmod iproute2 iputils-ping curl ca-certificates \
  util-linux dmidecode nvme-cli hdparm sg3-utils pciutils tpm2-tools efibootmgr lldpd netcat-openbsd openssh-client zstd"}
step_image() {
  say "the boot image: Ubuntu $UBUNTU $DEB (ubuntu:24.04), kernel $KERNEL, forge-scout and NICo's scout units → scout.efi"
  [ -x "$OUT/forge-scout" ] || die "build forge-scout first (step rust)"
  docker rm -f "$IMG" >/dev/null 2>&1 || true
  docker run -d --name "$IMG" --platform "linux/$DEB" "$BASE" sleep infinity >/dev/null || die "image container"
  docker exec -i "$IMG" bash -euo pipefail -c "export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq && apt-get install -y -qq --no-install-recommends $IMAGE_PACKAGES $KERNEL >/dev/null 2>&1
    apt-get clean && rm -rf /var/lib/apt/lists/*" || die "image packages"
  # forge-scout, and NICo's own files for it: the unit its package installs (crates/scout/misc),
  # the profile's drop-ins and scripts (mkosi.extra) and common files (postinst.chroot copies)
  local p=$NICO_SRC/pxe/mkosi.profiles/scout-oss-$ARCH/mkosi.extra
  docker exec "$IMG" mkdir -p /opt/forge /etc/systemd/system/multi-user.target.wants
  docker cp "$OUT/forge-scout" "$IMG:/opt/forge/forge-scout" >/dev/null
  docker cp "$NICO_SRC/crates/scout/misc/forge-scout.service" "$IMG:/etc/systemd/system/forge-scout.service" >/dev/null
  (cd "$p" && tar -cf - etc/systemd etc/modprobe.d opt/forge/forge-scout-pre.sh opt/forge/forge-scout.env.template) \
    | docker exec -i "$IMG" tar -C / -xf - || die "NICo's scout files"
  docker cp "$NICO_SRC/pxe/common_files/forge-scout-network.sh" "$IMG:/opt/forge/forge-scout-network.sh" >/dev/null
  docker cp "$NICO_SRC/pxe/common_files/load_modules.sh" "$IMG:/opt/forge/load_modules.sh" >/dev/null
  docker exec -i "$IMG" bash -euo pipefail -c '
    chmod 0755 /opt/forge/forge-scout /opt/forge/*.sh
    # postinst.chroot: systemctl enable forge-scout (and lldpd); the image is not booted here, so
    # the links are made by hand. networkd and resolved are the image'"'"'s network (Ubuntu
    # enables them through netplan, which is not installed).
    ln -sf /etc/systemd/system/forge-scout.service /etc/systemd/system/multi-user.target.wants/forge-scout.service
    for u in systemd-networkd.service systemd-resolved.service lldpd.service; do
      systemctl enable "$u" >/dev/null 2>&1 || true
    done
    systemctl set-default multi-user.target >/dev/null
    echo "dc-twin (NICo scout-oss userland, $(ls /lib/modules))" >/etc/scout-image-version
    if ldd /opt/forge/forge-scout | grep "not found"; then exit 1; fi
    : >/etc/machine-id  # a new machine id each boot, as an image booted fresh has
    # the whole root is the initramfs; systemd runs it as the system (no /etc/initrd-release)
    ln -sf /usr/lib/systemd/systemd /init
    k=$(ls /boot/vmlinuz-* | head -1); cp "$k" /vmlinuz
    rm -rf /usr/share/doc/* /usr/share/man/* /usr/share/locale/* /var/cache/apt/* /var/log/* /boot/initrd.img* /boot/System.map-*' || die "the image"
  # the root, as the container's filesystem without its runtime mounts (resolv.conf, hosts,
  # hostname), archived in the build container: the image gets its own of those three (a second
  # archive, which the kernel unpacks over the first)
  docker export "$IMG" | docker exec -i "$C" bash -euo pipefail -c 'rm -rf /work/root && mkdir -p /work/root && tar -C /work/root -xf -' || die "export the image"
  inside 'cd /work/root && rm -f .dockerenv run/.containerenv etc/resolv.conf etc/hosts etc/hostname && mv vmlinuz /work/out/vmlinuz
    rm -rf /work/stage && mkdir -p /work/stage/etc && ln -s ../run/systemd/resolve/stub-resolv.conf /work/stage/etc/resolv.conf
    printf "127.0.0.1 localhost\n::1 localhost\n" >/work/stage/etc/hosts
    echo scout >/work/stage/etc/hostname
    { find . ! -path "./boot/*" -print | cpio -o -H newc --quiet; cd /work/stage && find . -print | cpio -o -H newc --quiet; } | zstd -q -T2 -10 -o /work/out/scout-initrd.zst
    rm -rf /work/root /work/stage' || die "the initramfs"
  docker exec "$IMG" cat /etc/scout-image-version | sed 's/^/  /'
  docker exec "$IMG" sh -c 'echo "kernel $(ls /lib/modules)"' >"$OUT/kernel"
  docker rm -f "$IMG" >/dev/null
  # a UKI (systemd-stub) of the kernel and the initramfs, with no embedded command line: the
  # firmware is not Secure Boot, so the arguments iPXE passes (NICo's scout command line) are the
  # kernel's, as with NICo's own scout.efi
  inside 'cd /work/out && ukify build --linux vmlinuz --initrd scout-initrd.zst --output scout.efi >/dev/null' || die "ukify"
  fetch_out scout.efi
  inside 'rm -f /work/out/vmlinuz /work/out/scout-initrd.zst /work/out/scout.efi'
  ls -l "$OUT/scout.efi" | awk '{print "  " $NF, $5, "bytes"}'
}

step_down() {
  say "remove $C, $IMG and $VOL"
  docker rm -f "$C" "$IMG" >/dev/null 2>&1 || true
  docker volume rm "$VOL" >/dev/null 2>&1 || true
}

steps=("$@")
[ ${#steps[@]} -gt 0 ] || { sed -n '2,/^set -uo/p' "$0" | sed '$d'; exit 0; }
[ "${steps[0]}" = all ] && steps=(up ipxe rust image down)
for s in "${steps[@]}"; do
  case $s in up | ipxe | rust | image | down) "step_$s" ;; rust-cross) step_rust_cross ;; *) die "unknown step $s" ;; esac
done

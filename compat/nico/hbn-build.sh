#!/usr/bin/env bash
# compat/nico/hbn-build.sh — build, from NICo's source at the pin, the two NICo programs an emulated
# BlueField's HBN runs (D71): forge-dpu-agent (crate carbide-agent) and forge-dhcp-server (crate
# carbide-dhcp-server), for the host's architecture, on Debian bookworm, the distribution of the
# emulated HBN's image (test/linuxnet/nos-hbn), so they link against its glibc. One container
# (dctwin-nico-hbn-build) with a volume for the toolchain, the cargo home and the target directory;
# `down` removes both.
#
#   NICO_SRC=/path/to/infra-controller OUT=/path/to/out compat/nico/hbn-build.sh up build
#   compat/nico/hbn-build.sh down
#
# Environment: NICO_SRC (the infra-controller tree at the pin), OUT (where the binaries go,
# default ./out), JOBS [8], BUILD_MEMORY (a memory limit for the container), RUST_TOOLCHAIN
# [1.97.1, NICo's rust-toolchain.toml].
set -euo pipefail
OUT=${OUT:-./out}
JOBS=${JOBS:-8}
BUILD_MEMORY=${BUILD_MEMORY:-}
RUST_TOOLCHAIN=${RUST_TOOLCHAIN:-1.97.1}
BASE=${BASE:-docker.io/library/debian:bookworm}
C=dctwin-nico-hbn-build
VOL=dctwin-nico-hbn-build
die() { echo "hbn-build.sh: $*" >&2; exit 1; }
say() { printf '\n== %s\n' "$*"; }
inside() { docker exec -i "$C" bash -euo pipefail -c "$1"; }

step_up() {
  [ -n "${NICO_SRC:-}" ] && [ -d "$NICO_SRC/crates/agent" ] || die "NICO_SRC must name the infra-controller source tree"
  say "build container $C ($BASE), volume $VOL"
  if ! docker container inspect "$C" >/dev/null 2>&1; then
    docker volume create "$VOL" >/dev/null
    docker run -d --name "$C" ${BUILD_MEMORY:+--memory "$BUILD_MEMORY" --memory-swap "$BUILD_MEMORY"} -v "$VOL:/work" "$BASE" sleep infinity >/dev/null
  fi
  inside 'export DEBIAN_FRONTEND=noninteractive; apt-get update -qq && apt-get install -y -qq --no-install-recommends \
    build-essential pkg-config cmake clang libclang-dev git curl ca-certificates libudev-dev libtss2-dev \
    protobuf-compiler libprotobuf-dev libssl-dev liblzma-dev perl >/dev/null'
  docker exec "$C" mkdir -p /src /work/out
  COPYFILE_DISABLE=1 tar -C "$NICO_SRC" --exclude ./target --exclude ./.git -cf - . | docker exec -i "$C" tar -C /src -xf -
}

step_build() {
  say "forge-dpu-agent and forge-dhcp-server (rust $RUST_TOOLCHAIN, -j$JOBS, bookworm)"
  inside "
    export CARGO_HOME=/work/cargo RUSTUP_HOME=/work/rustup PATH=/work/cargo/bin:\$PATH
    [ -x /work/cargo/bin/cargo ] || curl -fsSL https://sh.rustup.rs | sh -s -- -y -q --profile minimal --default-toolchain $RUST_TOOLCHAIN --no-modify-path
    export CARGO_TARGET_DIR=/work/target CARGO_PROFILE_DEV_DEBUG=0 CARGO_INCREMENTAL=0 CARGO_NET_GIT_FETCH_WITH_CLI=true
    cd /src && cargo build --locked -j$JOBS -p carbide-agent --bin forge-dpu-agent -p carbide-dhcp-server --bin forge-dhcp-server 2>&1 \
      | grep -vE '^ +(Compiling|Downloaded|Downloading)' | tail -30
    test \${PIPESTATUS[0]} -eq 0
    for b in forge-dpu-agent forge-dhcp-server; do cp /work/target/debug/\$b /work/out/ && strip /work/out/\$b; done
  "
  mkdir -p "$OUT"
  for b in forge-dpu-agent forge-dhcp-server; do
    docker cp "$C:/work/out/$b" "$OUT/$b" >/dev/null
    ls -l "$OUT/$b" | awk '{print "  " $NF, $5, "bytes"}'
  done
}

step_down() {
  say "remove $C and $VOL"
  docker rm -f "$C" >/dev/null 2>&1 || true
  docker volume rm "$VOL" >/dev/null 2>&1 || true
}

[ $# -gt 0 ] || { sed -n '2,/^set -euo/p' "$0" | sed '$d'; exit 0; }
for s in "$@"; do
  case $s in up | build | down) "step_$s" ;; *) die "unknown step $s" ;; esac
done

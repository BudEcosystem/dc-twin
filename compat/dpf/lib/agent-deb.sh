#!/usr/bin/env bash
# DPF's DPU agent as DPF ships it to a DPU: an arm64 Debian package in the bfb-registry's apt
# repository, which DPF's cloud-init on the DPU installs from `<registry>/deb` (dpf.list,
# `apt-get install dpu-agent`). Built the way DPF's own registry image builds it
# (Dockerfile.bfb-registry, stage dpuagent-packager): `make binary-dpuagent deb-dpuagent` at the tag,
# with the nfpm the Makefile pins, then `dpkg-scanpackages -m . > Packages && gzip -k Packages`.
# The build runs in a rootless container from the Go toolchain DPF's go.mod pins, so nothing is
# installed on the host; the repository lands in <out> for bfbregistry's -deb.
#
#   lib/agent-deb.sh <doca-platform checkout at the tag> <out dir>
#
# Only dpu-agent is packaged: the registry image also carries the dpu_hw SPIRE plugin and SPIRE's
# own packages for DPF's SPIFFE identity mode, which the bootstrap-token mode run here does not
# install (`apt-get install --no-install-recommends dpu-agent`).
set -euo pipefail
src=$(cd "$1" && pwd)
mkdir -p "$2"
out=$(cd "$2" && pwd)
tag=${DPF_REF:-v26.8.0}
go=$(awk '/^toolchain /{sub("go", "", $2); print $2}' "$src/go.mod")
[ -n "$go" ] || { echo "no toolchain line in $src/go.mod" >&2; exit 1; }
image=docker.io/library/golang:$go
cache=${AGENT_CACHE:-$out/.cache}
mkdir -p "$cache/mod" "$cache/build"
podman run --rm --name "dctwin-dpf-agent-build-$$" \
	-v "$src:/workspace" -v "$cache/mod:/go/pkg/mod" -v "$cache/build:/root/.cache/go-build" -v "$out:/deb" \
	-w /workspace -e GOTOOLCHAIN=local -e GOFLAGS=-p=2 "$image" bash -ec '
		apt-get -qq update >/dev/null && apt-get -qq install -y --no-install-recommends dpkg-dev >/dev/null
		make binary-dpuagent deb-dpuagent TAG='"$tag"'
		rm -f /deb/*.deb /deb/Packages /deb/Packages.gz
		cp bin/dpu-agent_*_arm64.deb /deb/
		cd /deb && dpkg-scanpackages -m . >Packages 2>/dev/null && gzip -kf Packages
		dpkg-deb --info /deb/dpu-agent_*_arm64.deb | sed -n "/Package:/,/Architecture:/p"'
chmod -R u+w "$cache" 2>/dev/null # Go keeps its module cache read-only
rm -rf "$cache"
ls -l "$out"

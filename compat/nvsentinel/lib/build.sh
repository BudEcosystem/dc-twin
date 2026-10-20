# shellcheck shell=bash
# NVSentinel from source at a released tag. Sourced by compat/nvsentinel/run.sh after
# compat/kai/lib/common.sh.
#
# The control-plane modules are pure Go and are built here with the host's Go (each is its own
# module, `go 1.27.0`, with `replace` directives to its siblings). syslog-health-monitor reads the
# systemd journal through go-systemd's sdjournal (cgo, libsystemd's headers) and is built with the
# `systemd` tag, as its own Dockerfile builds it, inside a throwaway Debian container whose
# compiler, Go and caches live in tmpfs, so the host needs no libsystemd-dev. The source goes in and
# the binary comes out on the container's stdio; Go's module download cache, when there is one, is
# mounted read-only as a file proxy. Binaries land in $WORK/bin/linux-<arch>; nothing is built
# into the repository. GOFLAGS (default -p=2) caps the build on a shared machine.

NVS_REPO=${NVS_REPO:-https://github.com/NVIDIA/NVSentinel.git}
NVS_GO_MODULES="platform-connectors fault-quarantine node-drainer fault-remediation janitor janitor-provider"
# Go for the cgo build, pinned by digest (https://go.dev/dl/?mode=json&include=all)
NVS_GO_VERSION=go1.27.0
NVS_GO_SHA256_arm64=51798d2c42d0e1c6ed7fd9f48728b4193abac9e8aad6dbac2fe96a81f5909bda
NVS_GO_SHA256_amd64=${NVS_GO_SHA256_amd64:-}
NVS_BASE_IMAGE=${NVS_BASE_IMAGE:-docker.io/library/debian:bookworm-slim}

# docker_arch prints the container engine's architecture in Go's spelling (Docker's info, or
# Podman's when `docker` is Podman).
docker_arch() {
	local a
	a=$(docker info --format '{{.Architecture}}' 2>/dev/null) || a=
	case "$a" in "" | *"<no value>"*) a=$(docker info --format '{{.Host.Arch}}' 2>/dev/null) ;; esac
	case "$a" in
	aarch64 | arm64) echo arm64 ;;
	x86_64 | amd64) echo amd64 ;;
	*) echo "$a" ;;
	esac
}

# nvs_source <ref>: a shallow clone at the tag, once. Prints the commit.
nvs_source() {
	local ref=$1 dir=$WORK/src/nvsentinel
	if [ ! -d "$dir/.git" ]; then
		git clone -q --depth 1 --branch "$ref" "$NVS_REPO" "$dir" 2>/dev/null
	fi
	git -C "$dir" rev-parse HEAD
}

# nvs_build <arch>: every binary the harness runs, into $WORK/bin/linux-<arch>.
nvs_build() {
	local arch=$1 src=$WORK/src/nvsentinel out=$WORK/bin/linux-$1 c
	mkdir -p "$out"
	for c in $NVS_GO_MODULES; do
		[ -x "$out/$c" ] && continue
		echo "building $c (linux/$arch)" >&2
		(cd "$src/$c" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch GOTOOLCHAIN=local GOFLAGS=${GOFLAGS:--p=2} go build -trimpath -ldflags "-s -w" -o "$out/$c" .) || return 1
	done
	[ -x "$out/syslog-health-monitor" ] && return 0
	local sum proxy=() goproxy=https://proxy.golang.org,direct cache
	eval "sum=\${NVS_GO_SHA256_$arch}"
	cache=$(go env GOMODCACHE)/cache/download
	if [ -d "$cache" ]; then
		proxy=(-v "$cache:/goproxy:ro")
		goproxy=file:///goproxy,$goproxy
	fi
	echo "building syslog-health-monitor (linux/$arch, cgo, -tags systemd) in a throwaway $NVS_BASE_IMAGE container" >&2
	tar -C "$src" -cf - --exclude .git health-monitors/syslog-health-monitor commons data-models |
		docker run -i --rm --name dctwin-nvs-build --memory 3500m \
			--tmpfs /opt/go:exec,size=700m --tmpfs /root/.cache:exec,size=3g --tmpfs /gomod:exec,size=3g --tmpfs /src:exec,size=500m \
			${proxy[@]+"${proxy[@]}"} -e GOMODCACHE=/gomod -e GOPROXY="$goproxy" -e GOTOOLCHAIN=local -e CGO_ENABLED=1 \
			"$NVS_BASE_IMAGE" bash -ec "
				tar -xf - -C /src
				export DEBIAN_FRONTEND=noninteractive
				apt-get update -qq >/dev/null
				apt-get install -y -qq --no-install-recommends gcc libc6-dev libsystemd-dev pkg-config ca-certificates curl >/dev/null 2>&1
				curl -sfL -o /opt/go/go.tgz https://go.dev/dl/$NVS_GO_VERSION.linux-$arch.tar.gz
				[ -z '$sum' ] || echo '$sum  /opt/go/go.tgz' | sha256sum -c - >&2
				tar -C /opt/go -xzf /opt/go/go.tgz && rm /opt/go/go.tgz
				cd /src/health-monitors/syslog-health-monitor
				GOFLAGS=-p=2 /opt/go/go/bin/go build -tags systemd -trimpath -ldflags '-s -w' -o /src/out . >&2
				cat /src/out
			" >"$out/syslog-health-monitor.partial" || { rm -f "$out/syslog-health-monitor.partial"; return 1; }
	chmod +x "$out/syslog-health-monitor.partial" && mv "$out/syslog-health-monitor.partial" "$out/syslog-health-monitor"
}

# nvs_image <tag> <arch>: the image every NVSentinel container runs from. Debian with the systemd
# journal's reader (libsystemd0, which go-systemd loads) and systemd-journal-remote (it turns the
# twin's journal export stream into the journal file syslog-health-monitor reads), and the
# binaries built above in /nvsentinel/bin. Built once per tag.
nvs_image() {
	local tag=$1 arch=$2 ctx
	if docker image inspect "$tag" >/dev/null 2>&1; then
		return 0
	fi
	ctx=$WORK/image
	rm -rf "$ctx" && mkdir -p "$ctx/bin"
	cp "$here/image/Dockerfile" "$ctx/"
	cp "$WORK/bin/linux-$arch"/* "$ctx/bin/"
	docker build -q -t "$tag" --build-arg BASE="$NVS_BASE_IMAGE" "$ctx" >/dev/null && rm -rf "$ctx"
}

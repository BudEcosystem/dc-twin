#!/usr/bin/env bash
# Runs the real OpenSM subnet manager and infiniband-diags against the twin's InfiniBand fabric,
# simulated by ibsim from the model (internal/adapter/ibsim; the vendor-neutral "Scale-out fabric:
# OpenSM" slot of docs/spec/06-packs-and-stacks.md).
#
# ibsim and its preload library are Linux software, so everything runs in one Linux container
# native to the Docker host (dctwin-opensm:trixie, built from ./Containerfile: OpenSM,
# infiniband-diags, ibsim-utils and libumad2sim0 from Debian trixie): the twin, cross-compiled
# here and copied in, runs the lab estate with the ufm and ibsim adapters; the ibsim adapter
# starts ibsim on the rendered topology and applies the model's link state through its console;
# in-container.sh starts OpenSM at the UFM appliance's HCA and checks the fabric it brings up
# against the model and against UFM REST, then pulls and replaces a cable, powers a tray off and
# on and AC-cycles a leaf in the twin, and reads what OpenSM and the diags see. The container has
# no network (everything is on its loopback) and is removed at the end.
#
#   compat/opensm/run.sh                    # PASS/FAIL/FINDING table, evidence under $WORK
#   WORK=/tmp/osm compat/opensm/run.sh
#   KEEP=1 compat/opensm/run.sh             # leave the container (stopped) for inspection
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
WORK=${WORK:-$(mktemp -d "${TMPDIR:-/tmp}/dctwin-opensm.XXXXXX")}
mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd)
image=dctwin-opensm:trixie
name=dctwin-opensm-$$

cleanup() {
	if [ "${KEEP:-}" = 1 ]; then
		echo "KEEP=1: container $name left; remove with: docker rm -f $name" >&2
		return
	fi
	docker rm -f "$name" >/dev/null 2>&1 || true
}
trap cleanup EXIT

case $(docker info --format '{{.Architecture}}') in
	aarch64 | arm64) goarch=arm64 ;;
	x86_64 | amd64) goarch=amd64 ;;
	*) echo "FAIL: unsupported Docker architecture" >&2; exit 1 ;;
esac

docker build -q -t "$image" -f "$here/Containerfile" "$here" >/dev/null
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -o "$WORK/dctwin" ./cmd/dctwin)

docker create --name "$name" --network none "$image" bash /work/compat/opensm/in-container.sh >/dev/null
docker cp "$WORK/dctwin" "$name:/work/dctwin"
tmp=$(mktemp -d "$WORK/ctx.XXXXXX")
mkdir -p "$tmp/examples/estates" "$tmp/compat/opensm" "$tmp/compat/kai/lib"
cp "$root/examples/estates/lab-model.yaml" "$tmp/examples/estates/"
cp "$here/in-container.sh" "$here/ibnet.py" "$tmp/compat/opensm/"
cp "$root/compat/kai/lib/common.sh" "$root/compat/kai/lib/twin.sh" "$tmp/compat/kai/lib/"
docker cp "$tmp/examples" "$name:/work/"
docker cp "$tmp/compat" "$name:/work/"
rm -rf "$tmp"

set +e
docker start -a "$name" 2>&1 | tee "$WORK/run.log"
rc=${PIPESTATUS[0]}
set -e
rm -rf "$WORK/out"
docker cp "$name:/work/out" "$WORK/out" >/dev/null 2>&1 || true
echo
echo "evidence: $WORK/out/evidence; table: $WORK/out/results.md"
if [ "$rc" -eq 0 ]; then
	echo "PASS: compat/opensm (FINDING rows are the twin's, recorded, not failures)"
else
	echo "FAIL: compat/opensm"
fi
exit "$rc"

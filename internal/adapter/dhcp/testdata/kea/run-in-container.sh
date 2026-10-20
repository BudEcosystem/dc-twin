#!/bin/sh
# Runs the dhcp adapter's Kea end-to-end tests inside a Linux container that has ISC Kea.
#
# Kea answers a relay at giaddr, on the relay's UDP source port when the relay sends RFC 8357's
# sub-option. The twin relay's giaddr is its own loopback address, so Kea must share its network
# namespace: no port mapping or NAT can sit between them (and docker on macOS forwards no UDP).
# This script cross-compiles the package's test binary for the container's architecture, copies
# it and the lab estate into a container built from ./Containerfile, and runs it there with
# DCTWIN_KEA=auto, so the test starts kea-dhcp4 itself.
#
#   testdata/kea/run-in-container.sh                  # Kea tests and the 1,000-BMC scale run
#   RUN=TestKeaStack testdata/kea/run-in-container.sh # a subset
set -eu
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../../.." && pwd)
image=dctwin-kea-e2e:2.6
run=${RUN:-'Kea|Scale'}
work=$(mktemp -d "${TMPDIR:-/tmp}/dctwin-kea.XXXXXX")
trap 'rm -rf "$work"' EXIT

case $(docker info --format '{{.Architecture}}') in
  aarch64|arm64) goarch=arm64 ;;
  x86_64|amd64) goarch=amd64 ;;
  *) echo "unsupported docker architecture" >&2; exit 1 ;;
esac

docker build -q -t "$image" -f "$here/Containerfile" "$here" >/dev/null
(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go test -c -o "$work/dhcp.test" ./internal/adapter/dhcp/)

name=dctwin-kea-e2e-$$
docker create --name "$name" -e DCTWIN_KEA=auto "$image" \
  /work/dhcp.test -test.v -test.count=1 -test.timeout=10m -test.run "$run" >/dev/null
trap 'docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
docker cp "$work/dhcp.test" "$name:/work/dhcp.test"
docker cp "$repo/examples/estates/lab-model.yaml" "$name:/work/examples/estates/lab-model.yaml"
docker start -a "$name"

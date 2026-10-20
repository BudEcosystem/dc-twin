#!/usr/bin/env bash
# Runs Canonical MAAS 3.7.3 against dc-twin's BMCs. MAAS ships as a snap or as Debian packages for
# Ubuntu; this harness builds a container (Containerfile) from the MAAS 3.7 PPA's packages on
# Ubuntu 24.04 for the Docker host's own architecture, and runs the twin inside the same container,
# so MAAS reaches each BMC at its loopback port (Redfish over TCP, IPMI over UDP; Docker on macOS
# forwards no UDP). Two stages (flow.py):
#
#   drivers  `maas.power`, the command maas-agent runs for every power action, from the packages
#            (their power drivers are checked to be byte-identical to the 3.7.3 source tag), with
#            the parameters MAAS stores for a machine, against Redfish and IPMI BMCs
#   maas     MAAS itself -- regiond, the API server, Temporal and its worker, rackd, maas-agent,
#            PostgreSQL (start-maas.sh) -- with machines created through its CLI. In a container
#            without systemd maas-agent never completes its own configuration and runs no power
#            action (README); run-snap.sh runs MAAS itself, as the snap, in a KVM guest instead
#
#   compat/maas/run.sh                                   # the drivers stage; artefacts under $WORK
#   STAGES="drivers maas" compat/maas/run.sh             # and MAAS's services in the container
#   WORK=/tmp/maas KEEP=1 compat/maas/run.sh             # keep the container for inspection
#
# The image is tagged dctwin-maas:3.7.3 and kept for the next run (REBUILD=1 rebuilds it); the
# container, dctwin-maas-<pid>, is removed on exit unless KEEP=1.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
MAAS_VERSION=3.7.3
MAAS_COMMIT=9122eee6854592c9ca713c0e83725993911d368a   # tag 3.7.3, the PPA's 1:3.7.3-18030-g.9122eee68
IMAGE=${IMAGE:-dctwin-maas:$MAAS_VERSION}
STAGES=${STAGES:-drivers}
WORK=${WORK:-$(mktemp -d)}
DCTWIN_ASSETS=${DCTWIN_ASSETS:-$HOME/.cache/dc-twin/assets}
mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd)

# the container's architecture: Docker's daemon (a VM's on macOS), else Podman's host, else this one
arch=$(docker info --format '{{.Architecture}}' 2>/dev/null || docker info --format '{{.Host.Arch}}' 2>/dev/null || uname -m)
case $arch in
aarch64 | arm64) goarch=arm64 ;;
x86_64 | amd64) goarch=amd64 ;;
*) echo "unsupported Docker architecture" >&2; exit 1 ;;
esac

# ---------------------------------------------------------------- the image (native architecture)
if [ "${REBUILD:-0}" = 1 ] || ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
	echo "building $IMAGE from the MAAS 3.7 PPA (about 15 minutes) ..." >&2
	docker build -t "$IMAGE" -f "$here/Containerfile" "$here" >"$WORK/build.log" 2>&1 || { tail -30 "$WORK/build.log" >&2; exit 1; }
fi

# ---------------------------------------------------------------- the twin, for Linux
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -o "$WORK/dctwin-linux" ./cmd/dctwin)
assets=$(ls -d "$DCTWIN_ASSETS"/libredfish@* 2>/dev/null | head -1)
[ -n "$assets" ] || { echo "no captured BMC trees under $DCTWIN_ASSETS: run \`dctwin assets fetch\`" >&2; exit 1; }

# ---------------------------------------------------------------- the container
name=dctwin-maas-$$
cleanup() {
	set +e
	docker exec "$name" sh -c 'tar -C /var/log -czf /tmp/maas-logs.tgz maas; dctwin events --state /work/state --json > /tmp/events.json' >/dev/null 2>&1
	docker cp "$name:/tmp/maas-logs.tgz" "$WORK/" >/dev/null 2>&1
	docker cp "$name:/tmp/events.json" "$WORK/" >/dev/null 2>&1
	if [ "${KEEP:-0}" = 1 ]; then echo "container $name kept" >&2; else docker rm -f "$name" >/dev/null 2>&1; fi
	echo "artefacts in $WORK" >&2
}
trap cleanup EXIT
# NET_ADMIN applies to the container's own network namespace only: the BMC network below
docker create --name "$name" --hostname maas-twin --cap-add NET_ADMIN "$IMAGE" sleep infinity >/dev/null
docker cp "$WORK/dctwin-linux" "$name:/usr/local/bin/dctwin"
docker cp "$root/examples/estates/lab-model.yaml" "$name:/tmp/lab-model.yaml"
docker cp "$here/flow.py" "$name:/tmp/flow.py"
docker cp "$here/start-maas.sh" "$name:/usr/local/bin/start-maas.sh"
docker start "$name" >/dev/null
docker exec "$name" mkdir -p /work/out /root/.cache/dc-twin/assets
docker cp "$assets" "$name:/root/.cache/dc-twin/assets/"
dexec() { docker exec -e DCTWIN_STATE=/work/state "$name" "$@"; }
dexec sh -c 'cp /tmp/lab-model.yaml /tmp/flow.py /work/ && dpkg-query -W maas-rack-controller maas-region-controller maas-agent temporal freeipmi-tools' | tee "$WORK/packages.txt" >&2
# The BMC network, inside the container: an interface with 10.250.0.1/24, which the rack controller
# reports to the region (so MAAS routes power actions for BMCs on that subnet to its agent), and the
# whole /24 local, so that flow.py can give each BMC an address there with the standard ports.
dexec sh -c 'ip link add dctwin-bmc type dummy && ip addr add 10.250.0.1/24 dev dctwin-bmc &&
	ip link set dctwin-bmc up && ip route replace local 10.250.0.0/24 dev dctwin-bmc table local' 

# ---------------------------------------------------------------- provenance of the drivers
src="$WORK/src/maas"
if [ ! -d "$src/.git" ]; then
	git -c advice.detachedHead=false clone -q --depth 1 --branch "$MAAS_VERSION" https://git.launchpad.net/maas "$src" 2>&1 | grep -v 'is not a commit' >&2 || true
fi
[ "$(git -C "$src" rev-parse HEAD)" = "$MAAS_COMMIT" ] || { echo "maas $MAAS_VERSION is not $MAAS_COMMIT" >&2; exit 1; }
rm -rf "$WORK/packaged" && mkdir -p "$WORK/packaged"
docker cp "$name:/usr/lib/python3/dist-packages/provisioningserver/drivers/power" "$WORK/packaged/power"
docker cp "$name:/usr/lib/python3/dist-packages/provisioningserver/power_driver_command.py" "$WORK/packaged/"
if diff -r -x __pycache__ -x tests "$WORK/packaged/power" "$src/src/provisioningserver/drivers/power" >"$WORK/drivers.diff" &&
	diff "$WORK/packaged/power_driver_command.py" "$src/src/provisioningserver/power_driver_command.py" >>"$WORK/drivers.diff"; then
	echo "the packaged power drivers and maas.power are identical to MAAS $MAAS_VERSION ($MAAS_COMMIT)" | tee "$WORK/provenance.txt" >&2
else
	echo "the packaged power drivers differ from the $MAAS_VERSION tag: $WORK/drivers.diff" >&2; exit 1
fi

# ---------------------------------------------------------------- the twin, inside
docker exec -d "$name" sh -c 'dctwin up /work/lab-model.yaml --state /work/state --adapters redfish,ipmi > /work/up.log 2>&1'
for _ in $(seq 1 240); do dexec grep -q 'control API' /work/up.log 2>/dev/null && break; sleep 0.5; done
dexec grep -q 'control API' /work/up.log || { dexec cat /work/up.log >&2; exit 1; }
echo "twin up in $name (lab-model; redfish, ipmi)" >&2

status=0
for stage in $STAGES; do
	if [ "$stage" = maas ]; then
		echo "starting MAAS (start-maas.sh) ..." >&2
		if ! dexec /usr/local/bin/start-maas.sh 2>&1 | tee "$WORK/start-maas.txt" >&2; then
			echo "MAAS did not come up; the maas stage is not run (start-maas.txt, maas-logs.tgz)" | tee -a "$WORK/maas.txt" >&2
			status=1
			continue
		fi
		dexec bash -c 'maas createadmin --username admin --password "$(openssl rand -hex 12)" --email admin@example.invalid >/dev/null &&
			maas login admin http://127.0.0.1:5240/MAAS/ "$(maas apikey --username admin)" >/dev/null'
	fi
	set +e
	dexec python3 -I /work/flow.py --stage "$stage" --work /work/out | tee "$WORK/$stage.txt"
	rc=${PIPESTATUS[0]}
	set -e
	[ "$rc" = 0 ] || status=1
	docker cp "$name:/work/out/." "$WORK/" >/dev/null 2>&1 || true
done
exit "$status"

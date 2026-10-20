#!/usr/bin/env bash
# End-to-end: the TAP substrate (D20) and a real NOS (D4), on Linux.
#
#   (a) TestTapModelSwitches: model switches. Every BMC's management port is its own TAP with
#       the model's MAC, bridged per rack segment; BMCs lease over real L2 from the twin site
#       DHCP and from Kea through the twin relay; Redfish answers on each BMC's own IP:443; the
#       probe's ARP table and the bridge's FDB show the model MACs on their own ports; a pulled
#       cable takes a BMC off the segment and a replaced one brings it back.
#   (b, c) TestRealNOS: generic/frr-dhcrelay (FRR + ISC dhcrelay) on the OOB fabric. The twin
#       starts a container per switch and wires BMC TAPs and fabric cables into them; dhcrelay
#       relays every BMC by giaddr to Kea in its own container; eBGP between the FRR instances
#       carries the routes; the NOS observer reports ports and sessions; cable pulls show there.
#   (d) TestSonicVS: SONiC-VS on the OOB fabric (D44), the image SONiC publishes (linux/amd64:
#       native on an x86_64 host, emulated under binfmt_misc on an aarch64 one). The twin renders
#       each switch's HWSKU, config_db.json and bgpd configuration and starts SONiC's own
#       dhcp_relay container beside it; every BMC is relayed by SONiC's dhcrelay with its
#       hostname:port circuit id; eBGP between the SONiC switches; the observer reads APPL_DB.
#       Opt-in (RUN=TestSonicVS): it fetches ~1.3 GB of images, and each switch boots in a minute
#       or two natively, several emulated.
#
# The twin starts the NOS containers itself and opens their network namespaces
# (/proc/<pid>/ns/net), so the test runs as root where it sees the engine's processes. Two ways:
#
#   MODE=container (the default; any Docker host, a Mac's Linux VM included): the test binary runs
#     in a privileged container with the Docker Engine's socket and the host's PID namespace.
#   MODE=native (a Linux host with passwordless sudo): the test binary runs as root on the host in
#     a network namespace of its own (unshare --net), so nothing it listens on is the host's. With
#     Podman the engine is rootful Podman's Docker-compatible API, served for the run on a socket in
#     the work directory (`podman system service`); images are built rootless and loaded into
#     root's store, so no rootful build or container ever gets a network (every NOS and Kea
#     container is --network none). With Docker it is the rootful Docker Engine. The model-switch
#     suite's Kea, a process in the test's namespace, is the host's kea-dhcp4 or else the Kea
#     image's own, run by the image's musl loader.
#
# Everything a run creates is named dctwin-* (containers, volumes, network namespaces) and removed
# at the end; nothing else is touched: no firewall rule, route or bridge outside its namespaces.
#
#   scripts/linux-substrate-e2e.sh                         # (a), (b) and (c)
#   RUN=TestRealNOS scripts/linux-substrate-e2e.sh
#   RUN=TestSonicVS MODE=native scripts/linux-substrate-e2e.sh
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
run=${RUN:-'TestTapModelSwitches|TestRealNOS'}
mode=${MODE:-container}
work=$(mktemp -d "${TMPDIR:-/tmp}/dctwin-linuxnet.XXXXXX")
log="$work/linux-substrate-e2e.log"
name=dctwin-linuxnet-$$
estates='linuxnet-nos linuxnet-model linuxnet-sonic'

case $mode in
  container)
    eng=(docker)
    rootless=
    ;;
  native)
    if [ "$(uname -s)" != Linux ] || ! sudo -n true 2>/dev/null; then
      echo "FAIL: MODE=native needs a Linux host and passwordless sudo" >&2; exit 2
    fi
    if command -v podman >/dev/null; then
      eng=(sudo podman)
      rootless=podman # builds run rootless, their images are loaded into root's store
    elif command -v docker >/dev/null; then
      eng=(sudo docker)
      rootless=
    else
      echo "FAIL: MODE=native needs podman or docker" >&2; exit 2
    fi
    ;;
  *) echo "FAIL: MODE is container or native" >&2; exit 2 ;;
esac
svc= # the rootful Podman API service a native run starts

cleanup() {
  [ "$mode" = container ] && { docker rm -f "$name" >/dev/null 2>&1 || true; }
  # anything a crashed run left: the switch containers and their volumes (labelled by estate), the
  # test's Kea, and in a native run the namespaces the twin and the tests name
  for f in $(for e in $estates; do echo "label=dc-twin.estate=$e"; done) label=dc-twin.test=linuxnet; do
    "${eng[@]}" ps -aq --filter "$f" 2>/dev/null | while read -r id; do
      case $("${eng[@]}" inspect -f '{{.Name}}' "$id") in /dctwin-*|dctwin-*) "${eng[@]}" rm -f "$id" >/dev/null ;; esac
    done
    "${eng[@]}" volume ls -q --filter "$f" 2>/dev/null | while read -r v; do
      case $v in dctwin-*) "${eng[@]}" volume rm -f "$v" >/dev/null ;; esac
    done
  done
  if [ "$mode" = native ]; then
    for ns in $(ip netns list 2>/dev/null | awk '{print $1}' | grep -E '^dctwin-(linuxnet-|probe-)' || true); do
      sudo ip netns del "$ns" || true
    done
    [ -n "$svc" ] && { sudo kill "$svc" 2>/dev/null || true; }
    rm -rf "$work/kea-root"
  fi
  true
}
trap cleanup EXIT

if [ "$mode" = native ]; then
  case $(uname -m) in aarch64|arm64) goarch=arm64 ;; x86_64|amd64) goarch=amd64 ;; *) echo "FAIL: unsupported architecture" >&2; exit 1 ;; esac
  if [ "${eng[1]}" = podman ]; then
    sock="$work/podman.sock"
    sudo podman system service --time=0 "unix://$sock" >"$work/podman-service.log" 2>&1 &
    svc=$!
    for _ in $(seq 1 100); do sudo test -S "$sock" && break; sleep 0.1; done
    sudo test -S "$sock" || { echo "FAIL: the Podman API service did not start (see $work/podman-service.log)" >&2; exit 1; }
    engine_host="unix://$sock"
  else
    engine_host=unix:///var/run/docker.sock
  fi
else
  case $(docker info --format '{{.Architecture}}') in
    aarch64|arm64) goarch=arm64 ;;
    x86_64|amd64) goarch=amd64 ;;
    *) echo "FAIL: unsupported Docker architecture" >&2; exit 1 ;;
  esac
fi

sha256() { (sha256sum "$1" 2>/dev/null || shasum -a 256 "$1") | cut -d' ' -f1; }
image() { # tag, Containerfile, context: build only what is missing
  if ! "${eng[@]}" image inspect "$1" >/dev/null 2>&1; then
    echo "building $1"
    if [ -n "$rootless" ]; then
      podman build -q -t "$1" -f "$2" "$3" >/dev/null
      podman save "$1" | "${eng[@]}" load -q >/dev/null
    else
      "${eng[@]}" build -q -t "$1" -f "$2" "$3" >/dev/null
    fi
  fi
}
image dctwin-kea-e2e:2.6 "$root/internal/adapter/dhcp/testdata/kea/Containerfile" "$root/internal/adapter/dhcp/testdata/kea"

# SONiC (internal/nos: SonicVSImage, SonicRelay*; a test there keeps these in step). Only when the
# run asks for it. The switch image is pulled by digest, so no tag of the user's moves; the relay
# image SONiC publishes only as a build artifact, so it is downloaded at a pinned build, checked
# against its sha256, loaded and tagged dctwin-sonic-dhcp-relay:<build>.
sonic_vs=sonicdev-microsoft.azurecr.io/docker-sonic-vs@sha256:0de1c335c5ce02b1f1102068eeb44c8272147535647acd6a3153e0cb857b44ed
sonic_relay=dctwin-sonic-dhcp-relay:master.1250767-408ffc98c
sonic_relay_url='https://sonic-build.azurewebsites.net/api/sonic/artifacts?branchName=master&buildId=1250767&platform=vs&target=target/docker-dhcp-relay.gz'
sonic_relay_sha256=939cbdb454b76849284d3e8f51e26da41f63a87a3e763047bf61ecad091fc980
case $run in *Sonic*)
  if ! "${eng[@]}" image inspect "$sonic_vs" >/dev/null 2>&1; then
    echo "pulling $sonic_vs (linux/amd64)"
    "${eng[@]}" pull -q --platform linux/amd64 "$sonic_vs" >/dev/null
  fi
  if ! "${eng[@]}" image inspect "$sonic_relay" >/dev/null 2>&1; then
    echo "fetching SONiC's dhcp_relay image ($sonic_relay_url)"
    curl -fsSL -o "$work/docker-dhcp-relay.gz" "$sonic_relay_url"
    got=$(sha256 "$work/docker-dhcp-relay.gz")
    if [ "$got" != "$sonic_relay_sha256" ]; then
      echo "FAIL: docker-dhcp-relay.gz has sha256 $got, not the pinned $sonic_relay_sha256" >&2; exit 1
    fi
    # the tarball is tagged docker-dhcp-relay:latest; a tag of that name already here is put back
    prior=$("${eng[@]}" image inspect -f '{{.Id}}' docker-dhcp-relay:latest 2>/dev/null || true)
    "${eng[@]}" load -q -i "$work/docker-dhcp-relay.gz" >/dev/null
    "${eng[@]}" tag docker-dhcp-relay:latest "$sonic_relay"
    if [ -n "$prior" ]; then "${eng[@]}" tag "$prior" docker-dhcp-relay:latest; else "${eng[@]}" rmi docker-dhcp-relay:latest >/dev/null; fi
    rm -f "$work/docker-dhcp-relay.gz"
  fi
;; esac
case $run in TestSonicVS) ;; *) image dctwin-nos-frr-dhcrelay:bookworm "$root/test/linuxnet/nos/Containerfile" "$root/test/linuxnet/nos" ;; esac

(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go test -c -o "$work/linuxnet.test" ./test/linuxnet/)

if [ "$mode" = native ]; then
  testpath=$PATH
  if ! command -v kea-dhcp4 >/dev/null; then
    # the model-switch suite runs Kea as a process in its own namespace (the runner image has it):
    # here it is the Kea image's own binary, run by the image's musl loader with its libraries
    mkdir -p "$work/kea-root" "$work/bin"
    "${eng[@]}" create --name "dctwin-kea-extract-$$" --label dc-twin.test=linuxnet dctwin-kea-e2e:2.6 >/dev/null
    "${eng[@]}" export "dctwin-kea-extract-$$" | tar -x -C "$work/kea-root" lib usr/lib usr/sbin
    "${eng[@]}" rm "dctwin-kea-extract-$$" >/dev/null
    loader=$(ls "$work"/kea-root/lib/ld-musl-*.so.1)
    printf '#!/bin/sh\nexec "%s" --library-path "%s" "%s" "$@"\n' "$loader" "$work/kea-root/usr/lib:$work/kea-root/lib" "$work/kea-root/usr/sbin/kea-dhcp4" >"$work/bin/kea-dhcp4"
    chmod +x "$work/bin/kea-dhcp4"
    testpath=$work/bin:$PATH
  fi
fi

set +e
if [ "$mode" = native ]; then
  mkdir -p "$work/test/linuxnet"
  cp -R "$root/test/linuxnet/testdata" "$work/test/linuxnet/"
  # its own network namespace, loopback up: the twin's loopback endpoints are not the host's
  sudo env DCTWIN_LINUXNET=1 DOCKER_HOST="$engine_host" PATH="$testpath" unshare --net sh -c \
    'ip link set lo up && cd "$1" && exec "$2" -test.v -test.count=1 -test.timeout=45m -test.run "$3"' \
    sh "$work/test/linuxnet" "$work/linuxnet.test" "$run" 2>&1 | tee "$log"
  rc=${PIPESTATUS[0]}
else
  # the runner is tagged by its Containerfile's hash and built only when that changes: even a
  # cached build queues behind every other build on a busy Docker host (it took an hour on one)
  runner=dctwin-linuxnet-e2e:cf-$(sha256 "$root/test/linuxnet/Containerfile" | cut -c1-12)
  image "$runner" "$root/test/linuxnet/Containerfile" "$root/test/linuxnet"
  docker create --name "$name" --privileged --pid=host -v /var/run/docker.sock:/var/run/docker.sock \
    -e DCTWIN_LINUXNET=1 "$runner" \
    /work/linuxnet.test -test.v -test.count=1 -test.timeout=45m -test.run "$run" >/dev/null
  docker cp "$work/linuxnet.test" "$name:/work/linuxnet.test"
  docker cp "$root/test/linuxnet/testdata" "$name:/work/test/linuxnet/testdata"
  docker start -a "$name" 2>&1 | tee "$log"
  rc=${PIPESTATUS[0]}
fi
set -e

echo
echo "--- summary (full log: $log)"
grep -E '^(--- |    --- )(PASS|FAIL|SKIP)' "$log" || true
if [ "$rc" -eq 0 ] && ! grep -q -- '--- SKIP' "$log" && grep -q -- '^--- PASS' "$log"; then
  echo "PASS: linux substrate e2e"
elif [ "$rc" -eq 0 ]; then
  echo "FAIL: a suite was skipped or not run (see the log); nothing skipped counts as a pass"
  rc=1
else
  echo "FAIL: linux substrate e2e"
fi
exit "$rc"

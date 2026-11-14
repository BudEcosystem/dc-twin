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
#   (e) TestNVUE: generic/nvue-frr on the OOB fabric (D56): FRR, ifupdown2 and ISC dhcrelay
#       configured through the twin's NVUE, which applies each switch's startup configuration;
#       every BMC leases through the relay it configured; from the probe, a client reaches each
#       switch's NVUE at its loopback and builds an EVPN VNI across two racks, which each leaf's
#       bridge carries with the other's VTEP learned from EVPN; a VRF applies where the kernel can.
#   (d) TestSonicVS: SONiC-VS on the OOB fabric (D44), the image SONiC publishes (linux/amd64:
#       native on an x86_64 host, emulated under binfmt_misc on an aarch64 one). The twin renders
#       each switch's HWSKU and config_db.json and starts SONiC's own dhcp_relay, bgp and gnmi
#       containers beside it (D55); every BMC is relayed by SONiC's dhcrelay with its
#       hostname:port circuit id; eBGP between the SONiC switches, FRR configured from CONFIG_DB;
#       gNMI at each switch's loopback reads CONFIG_DB and creates a VRF; an EVPN VNI becomes a
#       VXLAN device; the observer reads APPL_DB. Opt-in (RUN=TestSonicVS): it fetches ~1.6 GB of
#       images, and each switch boots in a minute or two natively, several emulated.
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
#   ENGINE=podman-rootless (with MODE=native): the engine is the invoking user's rootless Podman,
#     its Docker-compatible API served for the run on a socket in the work directory; the test
#     runs as root, opens the containers' namespaces and moves their ports in, and no rootful
#     engine is touched (a shared host that allows only rootless containers).
#   (f) TestVMHostsOnRealNOS: two x86_64 vm hosts (os: twin, KVM) on generic/nvue-frr leaves (D57):
#       their NICs spliced onto TAPs in the leaves; DHCP from Kea through the leaf's relay, static
#       frontend addresses routed across the fabric, a pulled cable, and a tenant VRF with an L3 VNI
#       through NVUE. Opt-in (RUN=TestVMHostsOnRealNOS MODE=native): it needs /dev/kvm, QEMU and the
#       twin's x86_64 guest image (dctwin assets fetch; DCTWIN_ASSETS, default ~/.cache/dc-twin/assets).
#   (g) TestDPUIsolation: three x86_64 vm hosts behind BlueFields whose HBN is emulated (D71): each
#       BlueField a container running the twin's NVUE over FRR with HBN's interface names, its
#       host's NIC its host PF on pf0hpf_if, its uplinks unnumbered eBGP leaf ports with EVPN; each
#       configured through NVUE as NICo's agent configures it (FNN VPCs, an ACL, an ETV segment):
#       VPC isolation on a shared leaf and across racks, uplink failover, events. Opt-in
#       (RUN=TestDPUIsolation MODE=native), with what (f) needs.
#   (h) TestHBNAppliesNICoConfigs: every NVUE configuration NICo's agent is tested to produce,
#       applied to one emulated BlueField container through its NVUE; FRR, ifupdown2 and nftables
#       accept each (D71's P1 exit). Opt-in (RUN=TestHBNAppliesNICoConfigs); no KVM needed.
#   (i) TestDPUAgent: (g)'s estate with NICo's own forge-dpu-agent and forge-dhcp-server in each
#       BlueField (HBN_BINARIES: compat/nico/hbn-build.sh's output), configured by a Forge API test
#       harness: the agent registers, renders and pushes its FNN configuration through the emulated
#       NVUE, acknowledges it, the host leases its address from the DPU's DHCP server, VPCs isolate,
#       a re-tenanted host is reconfigured and a pulled uplink is reported. Opt-in
#       (RUN=TestDPUAgent MODE=native HBN_BINARIES=...).
#   NOS_MEMORY=512m: every container the twin and the tests create gets that memory limit, with no
#     swap beyond it (spec.adapters.network.nos.memory), for a host with a memory budget.
#
# Everything a run creates is named dctwin-* (containers, volumes, network namespaces) and removed
# at the end; nothing else is touched: no firewall rule, route or bridge outside its namespaces.
#
#   scripts/linux-substrate-e2e.sh                         # (a), (b), (c) and (e)
#   RUN=TestRealNOS scripts/linux-substrate-e2e.sh
#   RUN=TestSonicVS MODE=native scripts/linux-substrate-e2e.sh
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
run=${RUN:-'TestTapModelSwitches|TestRealNOS|TestNVUE'}
mode=${MODE:-container}
work=$(mktemp -d "${TMPDIR:-/tmp}/dctwin-linuxnet.XXXXXX")
log="$work/linux-substrate-e2e.log"
name=dctwin-linuxnet-$$
# SCOPE (estate names) limits cleanup to one run's estates, their probes and Kea containers, so
# runs of different suites can share a host side by side; unset, cleanup takes every suite's
scope=${SCOPE:-}
estates='linuxnet-nos linuxnet-model linuxnet-sonic linuxnet-nvue linuxnet-vm-nos linuxnet-dpu linuxnet-dpu-agent linuxnet-day0-cumulus linuxnet-day0-sonic linuxnet-day0-onie linuxnet-day0-api linuxnet-day0-cabling'

case $mode in
  container)
    eng=(docker)
    rootless=
    ;;
  native)
    if [ "$(uname -s)" != Linux ] || ! sudo -n true 2>/dev/null; then
      echo "FAIL: MODE=native needs a Linux host and passwordless sudo" >&2; exit 2
    fi
    if [ "${ENGINE:-}" = podman-rootless ]; then
      command -v podman >/dev/null || { echo "FAIL: ENGINE=podman-rootless needs podman" >&2; exit 2; }
      eng=(podman)
      rootless=
    elif command -v podman >/dev/null; then
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
  [ -n "$scope" ] && estates=$scope
  testlabel=label=dc-twin.test=linuxnet
  [ -n "$scope" ] && testlabel=
  # twice: a switch container cannot go while the sidecars in its namespace are there, and the
  # first pass may meet it before them
  for pass in 1 2; do
  for f in $(for e in $estates; do echo "label=dc-twin.estate=$e"; done) $testlabel; do
    "${eng[@]}" ps -aq --filter "$f" 2>/dev/null | while read -r id; do
      case $("${eng[@]}" inspect -f '{{.Name}}' "$id") in /dctwin-*|dctwin-*) "${eng[@]}" rm -f "$id" >/dev/null ;; esac
    done
    "${eng[@]}" volume ls -q --filter "$f" 2>/dev/null | while read -r v; do
      case $v in dctwin-*) "${eng[@]}" volume rm -f "$v" >/dev/null ;; esac
    done
  done
  done
  if [ "$mode" = native ]; then
    pat='^dctwin-(linuxnet-|probe-)'
    if [ -n "$scope" ]; then
      pat='^dctwin-('$(for e in $scope; do printf '%s|probe-%s|' "$e" "${e#linuxnet-}"; done | sed 's/|$//')')$'
      # the scoped run's own Kea containers (named for their estate)
      for e in $scope; do
        "${eng[@]}" ps -aq --filter label=dc-twin.test=linuxnet --filter name="dctwin-kea-$e-" 2>/dev/null | while read -r id; do "${eng[@]}" rm -f "$id" >/dev/null; done
      done
    fi
    for ns in $(ip netns list 2>/dev/null | awk '{print $1}' | grep -E "$pat" || true); do
      sudo ip netns del "$ns" || true
    done
    [ -n "$svc" ] && { kill "$svc" 2>/dev/null || sudo kill "$svc" 2>/dev/null || true; }
    rm -rf "$work/kea-root"
  fi
  true
}
trap cleanup EXIT

if [ "$mode" = native ]; then
  case $(uname -m) in aarch64|arm64) goarch=arm64 ;; x86_64|amd64) goarch=amd64 ;; *) echo "FAIL: unsupported architecture" >&2; exit 1 ;; esac
  if [ "${eng[0]}" = podman ]; then
    sock="$work/podman.sock"
    podman system service --time=0 "unix://$sock" >"$work/podman-service.log" 2>&1 &
    svc=$!
    for _ in $(seq 1 600); do test -S "$sock" && break; sleep 0.1; done
    test -S "$sock" || { echo "FAIL: the Podman API service did not start (see $work/podman-service.log)" >&2; exit 1; }
    engine_host="unix://$sock"
  elif [ "${eng[1]:-}" = podman ]; then
    sock="$work/podman.sock"
    sudo podman system service --time=0 "unix://$sock" >"$work/podman-service.log" 2>&1 &
    svc=$!
    for _ in $(seq 1 600); do sudo test -S "$sock" && break; sleep 0.1; done
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

# SONiC (internal/nos: SonicVSImage, SonicRelay*, SonicBGP*, SonicGNMI*; a test there keeps these
# in step). Only when the run asks for it. The switch image is pulled by digest, so no tag of the
# user's moves; the relay, bgp and gnmi images SONiC publishes only as build artifacts, so each is
# downloaded at a pinned build, checked against its sha256, loaded and tagged dctwin-sonic-*:<build>.
sonic_vs=sonicdev-microsoft.azurecr.io/docker-sonic-vs@sha256:0de1c335c5ce02b1f1102068eeb44c8272147535647acd6a3153e0cb857b44ed
sonic_relay=dctwin-sonic-dhcp-relay:master.1250767-408ffc98c
sonic_relay_url='https://sonic-build.azurewebsites.net/api/sonic/artifacts?branchName=master&buildId=1250767&platform=vs&target=target/docker-dhcp-relay.gz'
sonic_relay_sha256=939cbdb454b76849284d3e8f51e26da41f63a87a3e763047bf61ecad091fc980
sonic_bgp=dctwin-sonic-fpm-frr:master.1250767-408ffc98c
sonic_bgp_url='https://sonic-build.azurewebsites.net/api/sonic/artifacts?branchName=master&buildId=1250767&platform=vs&target=target/docker-fpm-frr.gz'
sonic_bgp_sha256=c1daf6372d1eca20eeac6da7a7f52678a1964e2d215ff30edc6cb3a1b794116d
sonic_gnmi=dctwin-sonic-gnmi:master.1250767-408ffc98c
sonic_gnmi_url='https://sonic-build.azurewebsites.net/api/sonic/artifacts?branchName=master&buildId=1250767&platform=vs&target=target/docker-sonic-gnmi.gz'
sonic_gnmi_sha256=864ed2431d7f6b963fff005dbb458d84e0b8ae12779b20fbd96ccd29027a9244
sonic_lldp=dctwin-sonic-lldp:master.1250767-408ffc98c
sonic_lldp_url='https://sonic-build.azurewebsites.net/api/sonic/artifacts?branchName=master&buildId=1250767&platform=vs&target=target/docker-lldp.gz'
sonic_lldp_sha256=728214e5726229075248a3227e59c4f9b5639502c2b543e479281dd8ada4a022
# sonic_artifact <local tag> <url> <sha256> <the tarball's own repository>
sonic_artifact() {
  "${eng[@]}" image inspect "$1" >/dev/null 2>&1 && return
  echo "fetching SONiC's $4 image ($2)"
  curl -fsSL -o "$work/$4.gz" "$2"
  got=$(sha256 "$work/$4.gz")
  if [ "$got" != "$3" ]; then
    echo "FAIL: $4.gz has sha256 $got, not the pinned $3" >&2; exit 1
  fi
  # the tarball is tagged <repository>:latest; a tag of that name already here is put back
  prior=$("${eng[@]}" image inspect -f '{{.Id}}' "$4:latest" 2>/dev/null || true)
  "${eng[@]}" load -q -i "$work/$4.gz" >/dev/null
  "${eng[@]}" tag "$4:latest" "$1"
  if [ -n "$prior" ]; then "${eng[@]}" tag "$prior" "$4:latest"; else "${eng[@]}" rmi "$4:latest" >/dev/null; fi
  rm -f "$work/$4.gz"
}
case $run in *Sonic*|*TestDay0*)
  if ! "${eng[@]}" image inspect "$sonic_vs" >/dev/null 2>&1; then
    echo "pulling $sonic_vs (linux/amd64)"
    "${eng[@]}" pull -q --platform linux/amd64 "$sonic_vs" >/dev/null
  fi
  sonic_artifact "$sonic_relay" "$sonic_relay_url" "$sonic_relay_sha256" docker-dhcp-relay
  sonic_artifact "$sonic_bgp" "$sonic_bgp_url" "$sonic_bgp_sha256" docker-fpm-frr
  sonic_artifact "$sonic_gnmi" "$sonic_gnmi_url" "$sonic_gnmi_sha256" docker-sonic-gnmi
  case $run in *TestDay0*) sonic_artifact "$sonic_lldp" "$sonic_lldp_url" "$sonic_lldp_sha256" docker-lldp ;; esac
;; esac
case $run in TestSonicVS) ;; *) image dctwin-nos-frr-dhcrelay:bookworm "$root/test/linuxnet/nos/Containerfile" "$root/test/linuxnet/nos" ;; esac
case $run in *TestNVUE*|*TestVMHosts*|*TestDPU*|*TestHBN*|*TestDay0*) image dctwin-nos-nvue-frr:bookworm "$root/test/linuxnet/nos-nvue/Containerfile" "$root/test/linuxnet/nos-nvue" ;; esac
# the NetBox + Ansible day-0 integration (compat/netbox-ansible): NetBox 4.2.9 with PostgreSQL and
# Valkey, from quay.io and ECR Public (no Docker Hub), and its controller (net-configurator nc-v8.0.0)
case $run in *TestDay0NetBox*|TestDay0)
  for i in quay.io/netboxcommunity/netbox:v4.2.9-3.2.1 public.ecr.aws/docker/library/postgres:17-alpine public.ecr.aws/valkey/valkey:8.0-alpine; do
    "${eng[@]}" image inspect "$i" >/dev/null 2>&1 || "${eng[@]}" pull -q "$i" >/dev/null
  done
  image dctwin-netbox-ansible:test "$root/compat/netbox-ansible/Containerfile" "$root/compat/netbox-ansible" ;;
esac
# NVIDIA's nvidia.nvue Ansible collection from Galaxy, the client TestDay0Ansible drives NVUE with
case $run in *TestDay0*) image dctwin-ansible-nvue:test "$root/test/linuxnet/ansible/Containerfile" "$root/test/linuxnet/ansible" ;; esac
case $run in *TestDPU*|*TestHBN*) image dctwin-nos-hbn:bookworm "$root/test/linuxnet/nos-hbn/Containerfile" "$root/test/linuxnet/nos-hbn" ;; esac

(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go test -c -o "$work/linuxnet.test" ./test/linuxnet/)
# the twin's day-0 programs a factory switch mounts (18: ZTP, nv, ONIE's discovery, a DHCP client)
mkdir -p "$work/day0"
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -o "$work/day0/dctwin-day0" ./cmd/dctwin-day0)
chmod 755 "$work" "$work/day0"

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
  cp -R "$root/internal/nos/nvue/testdata/nico" "$work/test/linuxnet/testdata/"
  cp -R "$root/internal/nos/nvue/testdata/nvcm-1.4.1" "$work/test/linuxnet/testdata/"
  # its own network namespace, loopback up: the twin's loopback endpoints are not the host's
  sudo env DCTWIN_LINUXNET=1 DCTWIN_HOLD="${HOLD:-}" DCTWIN_HBN_BINARIES="${HBN_BINARIES:-}" DCTWIN_DAY0_BINARIES="$work/day0" DCTWIN_REFERENCE_DAY0="$root/stacks/reference-day0" DCTWIN_COMPAT_NETBOX="$root/compat/netbox-ansible" DCTWIN_COMPAT_NVCM="$root/compat/nvcm" DCTWIN_NVCM_SRC="${NVCM_SRC:-}" DCTWIN_FOUNDRY="${FOUNDRY:-}" DCTWIN_RUN="${DCTWIN_RUN:-}" DCTWIN_NOS_MEMORY="${NOS_MEMORY:-}" DCTWIN_ASSETS="${DCTWIN_ASSETS:-$HOME/.cache/dc-twin/assets}" DOCKER_HOST="$engine_host" PATH="$testpath" unshare --net sh -c \
    'ip link set lo up && cd "$1" && exec "$2" -test.v -test.count=1 -test.timeout=45m -test.run "$3"' \
    sh "$work/test/linuxnet" "$work/linuxnet.test" "$run" 2>&1 | tee "$log"
  rc=${PIPESTATUS[0]}
else
  # the runner is tagged by its Containerfile's hash and built only when that changes: even a
  # cached build queues behind every other build on a busy Docker host (it took an hour on one)
  runner=dctwin-linuxnet-e2e:cf-$(sha256 "$root/test/linuxnet/Containerfile" | cut -c1-12)
  image "$runner" "$root/test/linuxnet/Containerfile" "$root/test/linuxnet"
  docker create --name "$name" --privileged --pid=host -v /var/run/docker.sock:/var/run/docker.sock \
    -e DCTWIN_LINUXNET=1 -e DCTWIN_NOS_MEMORY="${NOS_MEMORY:-}" -e DCTWIN_DAY0_BINARIES=/work/day0 "$runner" \
    /work/linuxnet.test -test.v -test.count=1 -test.timeout=45m -test.run "$run" >/dev/null
  docker cp "$work/linuxnet.test" "$name:/work/linuxnet.test"
  docker cp "$work/day0" "$name:/work/day0"
  docker cp "$root/test/linuxnet/testdata" "$name:/work/test/linuxnet/testdata"
  docker cp "$root/internal/nos/nvue/testdata/nico" "$name:/work/test/linuxnet/testdata/nico"
  docker cp "$root/internal/nos/nvue/testdata/nvcm-1.4.1" "$name:/work/test/linuxnet/testdata/nvcm-1.4.1"
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

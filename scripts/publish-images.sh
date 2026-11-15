#!/usr/bin/env bash
# Publishes the twin's own images to a registry, so a host pulls them rather than building or
# fetching them: the twin (spec.adapters.network.nos.registry, DCTWIN_REGISTRY) and
# scripts/linux-substrate-e2e.sh (REGISTRY) pull <registry>/<name>:<tag> and tag it as the twin
# names it. .github/workflows/images.yml runs it; by hand it needs docker with buildx (and QEMU's
# binfmt for the other architecture), skopeo, and a login to the registry.
#
#   REGISTRY=registry.bud.studio/gpu-foundry scripts/publish-images.sh [built|sonic|twin|all]   (all: the default)
#   DRY_RUN=1 …     prints what it would build and push, and reads nothing from the registry
#
# What it publishes:
#   built  the images scripts/linux-substrate-e2e.sh builds from this repository, for
#          linux/amd64 and linux/arm64 (PLATFORMS), each labelled dc-twin.context with the git
#          tree of what it is built from; generic/nvue-frr's first, as emulated/hbn is built on it
#   sonic  SONiC's dhcp_relay, bgp, gnmi and lldp images, which SONiC publishes only as build
#          artifacts that expire: fetched at the pinned build, checked against their sha256 and
#          pushed under the twin's tags (linux/amd64, all SONiC builds); and its switch image,
#          copied by digest (the copy keeps its digest) under its build's version, for safekeeping
#   twin   the twin itself, as charts/dc-twin runs it (the repository's Dockerfile, which
#          cross-compiles, so neither architecture builds under emulation): every commit's as
#          dc-twin:sha-<commit>, and the chart's appVersion (charts/dc-twin/Chart.yaml) once, from
#          the first commit that publishes it; bump appVersion to publish another
#
# A tag is published once. A host that has a tag never pulls it again, so a published tag whose
# content changed would leave hosts running different images under one name: a built image whose
# context differs from its published tag's fails here, saying to give it a new tag where it is
# named (internal/nos, scripts/linux-substrate-e2e.sh and this list). One already published with
# the same context is left as it is, so a run publishes only what changed. What is built is the
# checkout's committed content: a context with uncommitted changes is refused.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
reg=${REGISTRY:?REGISTRY=<host>/<path>, e.g. registry.bud.studio/gpu-foundry}
reg=${reg%/}
platforms=${PLATFORMS:-linux/amd64,linux/arm64}
what=${1:-all}
dry=${DRY_RUN:-}
work=$(mktemp -d "${TMPDIR:-/tmp}/dctwin-publish.XXXXXX")
trap 'rm -rf "$work"' EXIT

sha256() { (sha256sum "$1" 2>/dev/null || shasum -a 256 "$1") | cut -d' ' -f1; }

# The images scripts/linux-substrate-e2e.sh builds: <tag> <context> [<base tag> <base context>]
# (a test in internal/nos keeps this and the script's in step). The context is a directory holding
# the Containerfile, or the Containerfile itself when the image is built from it alone (the
# runner, whose tag the script derives from its Containerfile).
built=(
  "dctwin-kea-e2e:2.6 internal/adapter/dhcp/testdata/kea"
  "dctwin-linuxnet-e2e:cf-$(sha256 "$root/test/linuxnet/Containerfile" | cut -c1-12) test/linuxnet/Containerfile dctwin-kea-e2e:2.6 internal/adapter/dhcp/testdata/kea"
  "dctwin-nos-frr-dhcrelay:bookworm test/linuxnet/nos"
  "dctwin-nos-nvue-frr:bookworm test/linuxnet/nos-nvue"
  "dctwin-nos-hbn:bookworm test/linuxnet/nos-hbn dctwin-nos-nvue-frr:bookworm test/linuxnet/nos-nvue"
  "dctwin-ansible-nvue:test test/linuxnet/ansible"
  "dctwin-netbox-ansible:test compat/netbox-ansible"
)

# SONiC, as internal/nos pins it: <tag> <artifact URL> <sha256> <the tarball's own repository>
sonic_vs=sonicdev-microsoft.azurecr.io/docker-sonic-vs@sha256:0de1c335c5ce02b1f1102068eeb44c8272147535647acd6a3153e0cb857b44ed
sonic_vs_version=master.1112479-83775acca
sonic=(
  "dctwin-sonic-dhcp-relay:master.1250767-408ffc98c https://sonic-build.azurewebsites.net/api/sonic/artifacts?branchName=master&buildId=1250767&platform=vs&target=target/docker-dhcp-relay.gz 939cbdb454b76849284d3e8f51e26da41f63a87a3e763047bf61ecad091fc980 docker-dhcp-relay"
  "dctwin-sonic-fpm-frr:master.1250767-408ffc98c https://sonic-build.azurewebsites.net/api/sonic/artifacts?branchName=master&buildId=1250767&platform=vs&target=target/docker-fpm-frr.gz c1daf6372d1eca20eeac6da7a7f52678a1964e2d215ff30edc6cb3a1b794116d docker-fpm-frr"
  "dctwin-sonic-gnmi:master.1250767-408ffc98c https://sonic-build.azurewebsites.net/api/sonic/artifacts?branchName=master&buildId=1250767&platform=vs&target=target/docker-sonic-gnmi.gz 864ed2431d7f6b963fff005dbb458d84e0b8ae12779b20fbd96ccd29027a9244 docker-sonic-gnmi"
  "dctwin-sonic-lldp:master.1250767-408ffc98c https://sonic-build.azurewebsites.net/api/sonic/artifacts?branchName=master&buildId=1250767&platform=vs&target=target/docker-lldp.gz 728214e5726229075248a3227e59c4f9b5639502c2b543e479281dd8ada4a022 docker-lldp"
)

run() { # a command that changes something: printed, and run unless this is a dry run
  echo "+ $*"
  [ -n "$dry" ] || "$@"
}

# identity <context>…: what an image is built from, as git sees it (the contexts' tree ids,
# committed content only: a CI checkout is clean)
identity() {
  local ids=() c
  for c in "$@"; do ids+=("$(git -C "$root" rev-parse "HEAD:$c")"); done
  local IFS=+
  echo "${ids[*]}"
}

# published <ref>: whether the registry has it
published() {
  [ -z "$dry" ] && skopeo inspect --raw "docker://$1" >/dev/null 2>&1
}

# published_platforms <ref>: the os/arch of each image published under it (an index's entries, or
# the one image's own)
published_platforms() {
  local raw
  raw=$(skopeo inspect --raw "docker://$1" 2>/dev/null) || return 0
  if grep -q '"manifests"' <<<"$raw"; then
    python3 -c '
import json, sys
for m in json.loads(sys.argv[1]).get("manifests", []):
    p = m.get("platform") or {}
    if p.get("architecture") not in (None, "unknown"):  # buildx attestations are unknown/unknown
        print(p.get("os", "linux") + "/" + p["architecture"])
' "$raw"
  else
    skopeo inspect --format '{{.Os}}/{{.Architecture}}' "docker://$1" 2>/dev/null
  fi
}

# published_identity <ref>: its dc-twin.context label ("" when it has none)
published_identity() {
  skopeo inspect --override-os linux --override-arch amd64 --format '{{index .Labels "dc-twin.context"}}' "docker://$1" 2>/dev/null | sed 's/<no value>//'
}

# clean <path>…: the paths hold only committed content, which is what an image's label says it was
# built from (a dry run only warns)
clean() {
  local dirty
  dirty=$(git -C "$root" status --porcelain -- "$@")
  [ -z "$dirty" ] && return 0
  echo "${dry:+warning: }$* has uncommitted changes; publish from a clean checkout:" >&2
  echo "$dirty" | sed 's/^/  /' >&2
  [ -n "$dry" ]
}

publish_built() { # <tag> <context> [<base tag> <base context>]
  local tag=$1 ctx=$2 base=${3:-} basectx=${4:-}
  local id
  clean "$ctx" ${basectx:+"$basectx"} || return 1
  id=$(identity "$ctx" ${basectx:+"$basectx"})
  if published "$reg/$tag"; then
    local have
    have=$(published_identity "$reg/$tag")
    if [ "$have" = "$id" ]; then
      # the same content: published, unless a platform is missing (one published from a single
      # host has its own only), which is added by publishing it again for every platform
      local have_platforms missing="" p
      have_platforms=$(published_platforms "$reg/$tag")
      for p in ${platforms//,/ }; do grep -qx "$p" <<<"$have_platforms" || missing+=" $p"; done
      if [ -z "$missing" ]; then
        echo "$tag: published, from the same $ctx"
        return
      fi
      echo "$tag: published from the same $ctx, but not for$missing: publishing it again for $platforms"
    else
      echo "FAIL: $reg/$tag is published from another $ctx${basectx:+ and $basectx} (${have:-no dc-twin.context label}; now $id)." >&2
      echo "  A host that has the tag never pulls it again: give the image a new tag where it is named" >&2
      echo "  (internal/nos, scripts/linux-substrate-e2e.sh and scripts/publish-images.sh)." >&2
      return 1
    fi
  else
    echo "$tag: building from $ctx for $platforms"
  fi
  local file=$ctx/Containerfile dir=$ctx
  if [ -f "$root/$ctx" ]; then file=$ctx dir=$(dirname "$ctx"); fi
  run docker buildx build --platform "$platforms" --label "dc-twin.context=$id" \
    ${base:+--build-arg "BASE=$reg/$base"} -t "$reg/$tag" -f "$root/$file" --push "$root/$dir"
}

publish_sonic() { # <tag> <url> <sha256> <repository>
  local tag=$1 url=$2 sum=$3 repo=$4
  if published "$reg/$tag"; then
    echo "$tag: published (pinned by build and sha256)"
    return
  fi
  echo "$tag: fetching SONiC's $repo ($url)"
  run curl -fsSL -o "$work/$repo.gz" "$url"
  if [ -z "$dry" ]; then
    local got
    got=$( (sha256sum "$work/$repo.gz" 2>/dev/null || shasum -a 256 "$work/$repo.gz") | cut -d' ' -f1)
    if [ "$got" != "$sum" ]; then
      echo "FAIL: $repo.gz has sha256 $got, not the pinned $sum" >&2
      return 1
    fi
  fi
  # the tarball is tagged <repository>:latest
  run docker load -q -i "$work/$repo.gz"
  run docker tag "$repo:latest" "$reg/$tag"
  run docker push -q "$reg/$tag"
  run docker rmi "$repo:latest" "$reg/$tag"
  rm -f "$work/$repo.gz"
}

publish_twin() {
  local commit version tags=()
  clean Dockerfile go.mod go.sum cmd internal packs stacks console examples LICENSE charts/dc-twin/Chart.yaml || return 1
  commit=$(git -C "$root" rev-parse --short=12 HEAD)
  version=$(sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' "$root/charts/dc-twin/Chart.yaml")
  if [ -z "$version" ]; then
    echo "FAIL: no appVersion in charts/dc-twin/Chart.yaml" >&2
    return 1
  fi
  if published "$reg/dc-twin:sha-$commit"; then
    echo "dc-twin:sha-$commit: published"
  else
    tags+=(-t "$reg/dc-twin:sha-$commit")
  fi
  if published "$reg/dc-twin:$version"; then
    echo "dc-twin:$version: published (bump appVersion in charts/dc-twin/Chart.yaml to publish another)"
  else
    tags+=(-t "$reg/dc-twin:$version")
  fi
  [ ${#tags[@]} -gt 0 ] || return 0
  echo "dc-twin: building at $commit for $platforms"
  run docker buildx build --platform "$platforms" --label "dc-twin.context=$(git -C "$root" rev-parse HEAD)" \
    "${tags[@]}" -f "$root/Dockerfile" --push "$root"
}

publish_sonic_vs() {
  local to="$reg/docker-sonic-vs:$sonic_vs_version"
  if published "$to"; then
    echo "docker-sonic-vs:$sonic_vs_version: published"
    return
  fi
  echo "docker-sonic-vs:$sonic_vs_version: copying $sonic_vs, by digest"
  run skopeo copy --all "docker://$sonic_vs" "docker://$to"
}

case $what in built|sonic|twin|all) ;; *) echo "usage: publish-images.sh [built|sonic|twin|all]" >&2; exit 2 ;; esac
failed=0
case $what in built|all)
  unpublished=" " # tags that failed here: an image built on one is not built on what the registry still holds
  for b in "${built[@]}"; do
    read -r tag ctx base basectx <<<"$b"
    if [ -n "${base:-}" ] && [[ $unpublished == *" $base "* ]]; then
      echo "FAIL: $tag is built on $base, which was not published: not built" >&2
      failed=1 unpublished+="$tag "
      continue
    fi
    publish_built "$tag" "$ctx" "${base:-}" "${basectx:-}" || { failed=1; unpublished+="$tag "; }
  done ;;
esac
case $what in sonic|all)
  for s in "${sonic[@]}"; do
    read -r tag url sum repo <<<"$s"
    publish_sonic "$tag" "$url" "$sum" "$repo" || failed=1
  done
  publish_sonic_vs || failed=1 ;;
esac
case $what in twin|all) publish_twin || failed=1 ;; esac
exit $failed

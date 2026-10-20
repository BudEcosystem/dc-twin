#!/usr/bin/env bash
# Runs NICo's own BMC classifier, bmc-explorer-cli (crates/bmc-explorer-cli: the site explorer's
# exploration report for one BMC), against one twin BMC of each kind, once through each of NICo's
# Redfish clients (libredfish, and nv-redfish — the site explorer's default), and prints the
# vendor and hardware class NICo assigns beside the twin profile.
#
#   DCTWIN_STATE=<state of a running estate> classify.sh <bmc-explorer-cli> <out-dir> <selector>...
#
# It reaches each BMC at its loopback endpoint (dctwin credentials), with the factory account,
# as the CLI's operator would on a bench.
set -euo pipefail
cli=$1; out=$2; shift 2
dctwin=${DCTWIN:-dctwin}
mkdir -p "$out"
explore() { # explore <mode> <name> <ep> <user> <pass> -> "vendor class result"
  local mode=$1 name=$2 ep=$3 user=$4 pass=$5 f="$out/$2.$1"
  if "$cli" --username "$user" --password "$pass" --mode "$mode" --bmc-port "${ep##*:}" "${ep%:*}" >"$f.json" 2>"$f.log"; then
    local err; err=$(jq -r '.LastExplorationError // empty | tostring' "$f.json")
    printf '%s|%s|%s' "$(jq -r '.Vendor // "-"' "$f.json")" "$(jq -r '.HardwareClass // "-"' "$f.json")" "${err:-ok}"
  else
    printf '%s|%s|%s' - - "error: $(grep -o 'Error: .*' "$f.log" | tail -1 | cut -c1-110)"
  fi
}
printf '%-26s %-27s | %-28s | %s\n' "twin BMC" "twin profile" "libredfish: vendor / class" "nv-redfish (site explorer default): vendor / class / result"
for sel in "$@"; do
  creds=$("$dctwin" credentials "$sel")
  path=$(jq -r '.[0].path' <<<"$creds"); ep=$(jq -r '.[0].redfish' <<<"$creds")
  user=$(jq -r '.[0].user' <<<"$creds"); pass=$(jq -r '.[0].password' <<<"$creds")
  profile=$("$dctwin" get "$sel" --json | jq -r 'if type=="array" then .[0] else . end | .profile // "?"')
  short=${path#*/*/}; name=$(tr '/' '_' <<<"$short")
  IFS='|' read -r lv lc lr <<<"$(explore libredfish "$name" "$ep" "$user" "$pass")"
  IFS='|' read -r nv nc nr <<<"$(explore nv-redfish "$name" "$ep" "$user" "$pass")"
  l="$lv / $lc"; [ "$lr" = ok ] || l="$l ($lr)"
  printf '%-26s %-27s | %-28s | %s / %s / %s\n' "$short" "$profile" "$l" "$nv" "$nc" "$nr"
done

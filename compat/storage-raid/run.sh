#!/usr/bin/env bash
# Storage controllers and RAID (docs/reviews/2026-11-08-storage-deep-dive.md P2) driven by real,
# unmodified clients against dc-twin's storage-lab estate:
#   - OpenStack sushy (pinned) making Ironic's redfish RAID and inspection calls (raid_check.py)
#     on a generic storage node's RAID controller, each write read back from the twin's model;
#   - DMTF's Redfish-Interop-Validator (pinned) with OpenStack Ironic's own interop profile
#     (OpenStackIronicProfile.v1_2_0.json, Apache-2.0, fetched at a pinned commit) on the storage
#     node's and a generic server's generated trees.
#
#   compat/storage-raid/run.sh                 # artefacts under $WORK
#   WORK=/tmp/x DCTWIN=./bin/dctwin compat/storage-raid/run.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
SUSHY_VERSION=5.14.0
VALIDATOR_VERSION=3.0.5
IRONIC_COMMIT=${IRONIC_COMMIT:-master}
PROFILE_URL="https://opendev.org/openstack/ironic/raw/branch/$IRONIC_COMMIT/redfish-interop-profiles/OpenStackIronicProfile.v1_2_0.json"
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
NODE=${NODE:-s01/node-01}
SERVER=${SERVER:-c01/server-02}
mkdir -p "$WORK"
if [ -z "${PYTHON:-}" ]; then
  for p in python3.13 python3.12 python3.11 python3; do
    if command -v "$p" >/dev/null && "$p" -c 'import sys; sys.exit(sys.version_info < (3, 11))'; then PYTHON=$p; break; fi
  done
fi
: "${PYTHON:?no Python 3.11+ found; set PYTHON}"
if [ ! -x "$WORK/venv/bin/rf_interop_validator" ]; then
  "$PYTHON" -m venv "$WORK/venv"
  "$WORK/venv/bin/pip" install -q "sushy==$SUSHY_VERSION" "redfish_interop_validator==$VALIDATOR_VERSION"
fi
[ -f "$WORK/ironic-profile.json" ] || curl -fsSL "$PROFILE_URL" -o "$WORK/ironic-profile.json"

export DCTWIN_STATE="$WORK/state"
rm -rf "$DCTWIN_STATE"
"$DCTWIN" up "$root/examples/estates/storage-lab.yaml" --state "$DCTWIN_STATE" --adapters redfish > "$WORK/up.log" 2>&1 &
for _ in $(seq 1 120); do [ -f "$DCTWIN_STATE/control.json" ] && break; sleep 0.25; done
[ -f "$DCTWIN_STATE/control.json" ] || { cat "$WORK/up.log"; exit 1; }
trap '"$DCTWIN" down >/dev/null 2>&1 || true' EXIT
export DCTWIN
"$DCTWIN" credentials "$NODE $SERVER" > "$WORK/credentials.json"

status=0
"$WORK/venv/bin/python" -I "$here/raid_check.py" "$WORK/credentials.json" "site-1/hall-a/$NODE" | tee "$WORK/raid.txt" || status=1

# Ironic's interop profile on each tree. The profile asks every VolumeCollection to take POST with
# its apply times; a host's directly attached NVMe drives (Storage "1") have no RAID controller, so
# theirs is read-only (Allow: GET, HEAD) and its two findings are expected; raid.py skips that
# Storage too (SupportedRAIDTypes []). Any other FAIL, or a missing required resource, fails the run.
for dev in "$NODE" "$SERVER"; do
  read -r addr user pass < <(python3 -c 'import json,sys;c=[x for x in json.load(open(sys.argv[1])) if x["path"].endswith("/"+sys.argv[2])][0];print(c["redfish"],c["user"],c["password"])' "$WORK/credentials.json" "$dev")
  out="$WORK/interop-${dev//\//_}"
  mkdir -p "$out"
  "$WORK/venv/bin/rf_interop_validator" -u "$user" -p "$pass" -r "https://$addr" --logdir "$out" "$WORK/ironic-profile.json" > "$out/stdout.txt" 2>&1 || true
  read -r pass_n warn_n fail_n < <(awk -F'|' '/PASS.*WARN.*FAIL/{getline; getline; gsub(/ /,""); print $2, $3, $4}' "$out/stdout.txt")
  per=$(awk '/^Validating /{r=$2; sub(/\.\.\.$/,"",r)} /- Pass:/{n=$0; sub(/.*Fail: /,"",n); sub(/,.*/,"",n); if (n+0>0) print n, r}' "$out/stdout.txt")
  unexpected=$(printf '%s\n' "$per" | awk 'NF && $2 !~ /\/Storage\/1\/Volumes$/' || true)
  per_sum=$(printf '%s\n' "$per" | awk '{s+=$1} END{print s+0}')
  echo "interop $dev: ${pass_n:-?} PASS, ${warn_n:-?} WARN, ${fail_n:-?} FAIL"
  [ -n "$per" ] && printf '  FAIL %s\n' "$per"
  if [ -z "${fail_n:-}" ] || [ -n "$unexpected" ] || [ "$fail_n" -gt "$per_sum" ]; then
    echo "  unexpected: ${unexpected:-a required resource is missing (see $out/stdout.txt)}"
    status=1
  fi
done
exit $status

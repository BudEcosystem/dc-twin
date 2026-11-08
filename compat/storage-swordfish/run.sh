#!/usr/bin/env bash
# A shared storage system's management API (docs/reviews/2026-11-08-storage-deep-dive.md P4, S-Q3),
# SNIA Swordfish served by the storage adapter, against real, unmodified, pinned DMTF and SNIA
# tooling on the storage-lab estate's system:
#   - DMTF's python-redfish-library provisions through a session (swordfish_check.py): a file
#     system and its export, a volume mapped to a host by an NVMe-oF connection, a quota change and
#     the deletes, each read back from the twin's model;
#   - DMTF's Redfish Service Validator checks every resource against the published DMTF and
#     Swordfish CSDL, with the objects provisioned;
#   - DMTF's Redfish Interop Validator checks SNIA's Swordfish profiles (Discovery, File
#     Provisioning, Block Provisioning) from SNIA/Swordfish-Publications at a pinned commit;
#   - Prometheus's promtool (the pinned release compat/prometheus uses) lints the system's
#     exporter, after a pool-full fault and an object filled past its quota.
#
#   compat/storage-swordfish/run.sh                 # artefacts under $WORK
#   WORK=/tmp/x DCTWIN=./bin/dctwin compat/storage-swordfish/run.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
REDFISH_LIB_VERSION=3.3.4
SERVICE_VALIDATOR_VERSION=3.1.8
INTEROP_VALIDATOR_VERSION=3.0.5
PROM_VERSION=${PROM_VERSION:-3.15.0}
SWORDFISH_COMMIT=${SWORDFISH_COMMIT:-main}
PROFILES="SwordfishDiscovery SwordfishFileProvisioning SwordfishBlockProvisioning"
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
mkdir -p "$WORK/profiles"
if [ -z "${PYTHON:-}" ]; then
  for p in python3.13 python3.12 python3.11 python3; do
    if command -v "$p" >/dev/null && "$p" -c 'import sys; sys.exit(sys.version_info < (3, 11))'; then PYTHON=$p; break; fi
  done
fi
: "${PYTHON:?no Python 3.11+ found; set PYTHON}"
if [ ! -x "$WORK/venv/bin/rf_interop_validator" ]; then
  "$PYTHON" -m venv "$WORK/venv"
  "$WORK/venv/bin/pip" install -q "redfish==$REDFISH_LIB_VERSION" "redfish_service_validator==$SERVICE_VALIDATOR_VERSION" "redfish_interop_validator==$INTEROP_VALIDATOR_VERSION"
fi
for p in $PROFILES; do
  [ -f "$WORK/profiles/$p.json" ] || curl -fsSL "https://raw.githubusercontent.com/SNIA/Swordfish-Publications/$SWORDFISH_COMMIT/profiles/$p.json" -o "$WORK/profiles/$p.json"
  # a profile names the profiles it requires by name and version; the validator looks for
  # <name>.v<major>_<minor>_<patch>.json beside it
  v=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["ProfileVersion"].replace(".","_"))' "$WORK/profiles/$p.json")
  cp "$WORK/profiles/$p.json" "$WORK/profiles/$p.v$v.json"
  # The provisioning profiles require "SwordfishDiscovery or CoSDiscovery" (conditional
  # alternatives); validator 3.0.5 loads every required profile however it is conditioned, and
  # CoSDiscovery (classes of service) is not served. Discovery is validated on its own run, so
  # each profile is also checked by its own requirements alone: a copy without RequiredProfiles.
  # SwordfishFileProvisioning 1.1.2 names its file-system resource "Filesystem", which no
  # resource type is (the schema's is FileSystem), so the copy names it FileSystem.
  python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));d.pop("RequiredProfiles",None);r=d.get("Resources",{});"Filesystem" in r and r.setdefault("FileSystem",r.pop("Filesystem"));json.dump(d,open(sys.argv[2],"w"),indent=1)' \
    "$WORK/profiles/$p.json" "$WORK/profiles/$p.own.json"
done

export DCTWIN_STATE="$WORK/state"
rm -rf "$DCTWIN_STATE"
"$DCTWIN" up "$root/examples/estates/storage-lab.yaml" --state "$DCTWIN_STATE" --adapters storage > "$WORK/up.log" 2>&1 &
for _ in $(seq 1 120); do [ -f "$DCTWIN_STATE/control.json" ] && break; sleep 0.25; done
[ -f "$DCTWIN_STATE/control.json" ] || { cat "$WORK/up.log"; exit 1; }
trap '"$DCTWIN" down >/dev/null 2>&1 || true' EXIT
export DCTWIN
"$DCTWIN" storage credentials > "$WORK/credentials.json"
read -r api user pass < <(python3 -c 'import json,sys;c=json.load(open(sys.argv[1]))[0];print(c["api"],c["user"],c["password"])' "$WORK/credentials.json")

status=0
"$WORK/venv/bin/python" -I "$here/swordfish_check.py" "$WORK/credentials.json" | tee "$WORK/swordfish.txt" || status=1

# leave objects for the validators to walk
"$DCTWIN" storage create filesystem hall-a-fs home --size 1Ti --export 10.50.0.0/16 > /dev/null
"$DCTWIN" storage create volume hall-a-fs scratch --size 100G > /dev/null
"$DCTWIN" storage map hall-a-fs scratch nqn.2014-08.org.nvmexpress:uuid:validator-host > /dev/null

out="$WORK/service-validator"
mkdir -p "$out"
"$WORK/venv/bin/rf_service_validator" -u "$user" -p "$pass" -r "$api" --logdir "$out" > "$out/stdout.txt" 2>&1 || true
table() { awk -F'|' '/PASS.*WARN.*FAIL/{getline; getline; gsub(/ /,""); print $2, $3, $4}' "$1"; }
failing() { awk '/^Validating /{r=$2} /- Pass:.*Fail: [1-9]/{print "  FAIL " r}' "$1"; }
read -r pn wn fn < <(table "$out/stdout.txt" || true) || true
echo "service validator: ${pn:-?} PASS, ${wn:-?} WARN, ${fn:-?} FAIL"
failing "$out/stdout.txt"
[ "${fn:-1}" = 0 ] || status=1

for p in $PROFILES; do
  o="$WORK/interop-$p"
  mkdir -p "$o"
  "$WORK/venv/bin/rf_interop_validator" -u "$user" -p "$pass" -r "$api" --logdir "$o" "$WORK/profiles/$p.own.json" > "$o/stdout.txt" 2>&1 || true
  pn= wn= fn=
  read -r pn wn fn < <(table "$o/stdout.txt" || true) || true
  echo "interop $p: ${pn:-?} PASS, ${wn:-?} WARN, ${fn:-?} FAIL"
  failing "$o/stdout.txt"
  [ -n "$fn" ] || grep -E "Abort|not found" "$o/stdout.txt" | head -3
  [ "${fn:-1}" = 0 ] || status=1
done
# the exporter, linted by promtool, with alerts raised
"$DCTWIN" fault inject storage/hall-a-fs generic/storage-system/pool-near-full --param percent=95 > /dev/null
"$DCTWIN" storage fill hall-a-fs home 120% > /dev/null
if [ -z "${PROMTOOL:-}" ]; then
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$(uname -m)" in arm64 | aarch64) arch=arm64 ;; x86_64) arch=amd64 ;; *) arch=$(uname -m) ;; esac
  name="prometheus-$PROM_VERSION.$os-$arch"
  if [ ! -x "$WORK/$name/promtool" ]; then
    base="https://github.com/prometheus/prometheus/releases/download/v$PROM_VERSION"
    curl -fsSL -o "$WORK/$name.tar.gz" "$base/$name.tar.gz"
    curl -fsSL -o "$WORK/sha256sums.txt" "$base/sha256sums.txt"
    want=$(awk -v f="$name.tar.gz" '$2 == f { print $1 }' "$WORK/sha256sums.txt")
    got=$(shasum -a 256 "$WORK/$name.tar.gz" | awk '{ print $1 }')
    [ -n "$want" ] && [ "$want" = "$got" ] || { echo "checksum mismatch for $name.tar.gz" >&2; exit 1; }
    tar -xzf "$WORK/$name.tar.gz" -C "$WORK"
  fi
  PROMTOOL="$WORK/$name/promtool"
fi
metrics=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))[0]["metrics"])' "$WORK/credentials.json")
curl -fsS "$metrics" > "$WORK/metrics.txt"
if lint=$("$PROMTOOL" check metrics < "$WORK/metrics.txt" 2>&1); then
  echo "promtool check metrics: clean ($(grep -c '^storage_system' "$WORK/metrics.txt") samples)"
else
  echo "promtool check metrics: $lint"; status=1
fi
for want in 'storage_system_alerts{system="hall-a-fs",alert="PoolNearlyFull",severity="Warning"} 1' 'storage_system_alerts{system="hall-a-fs",alert="QuotaExceeded",severity="Warning"} 1' 'storage_system_health{system="hall-a-fs"} 1'; do
  grep -qF "$want" "$WORK/metrics.txt" && echo "PASS  exporter: $want" || { echo "FAIL  exporter lacks: $want"; status=1; }
done
exit $status

#!/usr/bin/env bash
# Runs DMTF's Redfish Service Validator, unmodified and pinned, against dc-twin BMCs of the lab
# estate: the generated DMTF tree (a generic server), the generated PowerEquipment tree (a PDU)
# and a captured tree (a GB300 compute tray). A captured target is validated a second time as
# captured -- the same mockup served raw by capture_server.py -- so every failure is attributed
# either to the captured vendor data or to what the twin adds on top of it.
#
#   compat/dmtf-validator/run.sh              # summaries on stdout and in $WORK/summary.txt
#   DCTWIN_STATE=<dir> compat/dmtf-validator/run.sh   # reuse a running estate instead of starting one
#   TARGETS="c02/server-01" FAULTS="" WORK=/tmp/v DCTWIN=./dctwin compat/dmtf-validator/run.sh
#
# The validator downloads the current DMTF schema bundle (DSP8010) into $WORK/SchemaFiles on its
# first run; the bundle version is printed in each debug log.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
VALIDATOR_VERSION=3.1.8
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
TARGETS=${TARGETS:-"c02/server-01 b01/pdu-a a01/compute-tray-01"}
# the captures behind captured dialects: the libredfish asset the packs declare
LIBREDFISH=${DCTWIN_ASSETS:-$HOME/.cache/dc-twin/assets}/libredfish@a51ffb1ffeae15d8f9471224cb799d54007098a2
capture_of() { # dialect -> capture directory (empty for a generated tree)
  case "$1" in
    nvidia/gb-bmc|nvidia/gb300-bmc) echo nvidia_gb200 ;;   # gb300-bmc overlays the GB200 capture
    nvidia/nvswitch-tray) echo nvidia_gbswitch ;;
    nvidia/bluefield) echo nvidia_dpu ;;
    nvidia/dgx) echo nvidia_viking ;;
    nvidia/gh200-bmc) echo nvidia_gh200 ;;
    oem-dell/idrac) echo dell ;;
    oem-dell/idrac-multi-dpu) echo dell_multi_dpu ;;
    oem-supermicro/redfish) echo supermicro ;;
    oem-hpe/ilo) echo hpe ;;
    oem-lenovo/xcc) echo lenovo ;;
    liteon/power-shelf) echo liteon_powershelf ;;
    delta/power-shelf) echo delta_powershelf ;;
  esac
}
mkdir -p "$WORK"

if [ -z "${PYTHON:-}" ]; then
  for p in python3.13 python3.12 python3.11 python3; do
    if command -v "$p" >/dev/null && "$p" -c 'import sys; sys.exit(sys.version_info < (3, 9))'; then PYTHON=$p; break; fi
  done
fi
: "${PYTHON:?no Python 3.9+ found; set PYTHON}"
if [ ! -x "$WORK/venv/bin/rf_service_validator" ]; then
  "$PYTHON" -m venv "$WORK/venv"
  "$WORK/venv/bin/pip" install -q "redfish_service_validator==$VALIDATOR_VERSION"
fi
py="$WORK/venv/bin/python"

if [ -n "${DCTWIN_STATE:-}" ] && [ -f "$DCTWIN_STATE/control.json" ]; then
  echo "using the running estate at $DCTWIN_STATE" >&2
else
  export DCTWIN_STATE="$WORK/state"
  rm -rf "$DCTWIN_STATE"
  "$DCTWIN" up "$root/examples/estates/lab-model.yaml" --state "$DCTWIN_STATE" --adapters redfish > "$WORK/up.log" 2>&1 &
  for _ in $(seq 1 120); do [ -f "$DCTWIN_STATE/control.json" ] && break; sleep 0.25; done
  [ -f "$DCTWIN_STATE/control.json" ] || { cat "$WORK/up.log"; exit 1; }
  trap '"$DCTWIN" down >/dev/null 2>&1 || true' EXIT
fi

# faults injected first, so the twin's live bindings are validated too: log entries the twin
# writes, a component gone Critical, an outlet switched off. FAULTS="" validates the estate as started.
FAULTS=${FAULTS-"c02/server-01/psu-1=generic/power/psu-failure b01/pdu-a=generic/power/outlet-off a01/compute-tray-01/gpu-1=nvidia/xid/xid-79"}
for f in $FAULTS; do
  "$DCTWIN" fault inject "${f%%=*}" "${f#*=}" > /dev/null && echo "injected ${f#*=} at ${f%%=*}" >&2
done

validate() { # name url user password
  local out="$WORK/$1"
  rm -rf "$out"
  "$WORK/venv/bin/rf_service_validator" --ip "$2" -u "$3" -p "$4" --authtype Basic \
    --logdir "$out" --schema_directory "$WORK/SchemaFiles" > "$out.out" 2>&1 || true
  ls -t "$out"/*/RedfishServiceValidatorDebug_*.log | head -1
}

rm -f "$WORK/failed"
for t in $TARGETS; do
  name=$(echo "$t" | tr '/' '_')
  pick='import json,sys; c=[x for x in json.load(sys.stdin) if x["path"].endswith("/"+sys.argv[1])][0]'
  read -r addr user pass < <("$DCTWIN" credentials "$t" | "$py" -I -c "$pick"'; print(c["redfish"], c["user"], c["password"])' "$t")
  echo "validating $t at https://$addr" >&2
  log=$(validate "$name" "https://$addr" "$user" "$pass")
  dialect=$("$DCTWIN" get "$t" --json | "$py" -I -c "$pick"'; print(c["dialect"])' "$t")
  capture=$(capture_of "$dialect")
  if [ -n "$capture" ]; then
    echo "validating the $capture capture as captured" >&2
    srvout="$WORK/$name.capture-server.out"
    "$py" -I "$here/capture_server.py" "$LIBREDFISH/$capture" > "$srvout" 2>&1 &
    srv=$!
    for _ in $(seq 1 40); do grep -q '^port ' "$srvout" 2>/dev/null && break; sleep 0.25; done
    port=$(awk '/^port /{print $2}' "$srvout")
    clog=$(validate "$name.capture" "http://127.0.0.1:$port" capture capture)
    kill "$srv" 2>/dev/null || true
    # the capture's own failures are the baseline, not a verdict on the twin
    "$py" -I "$here/summarise.py" "$capture (as captured)" "$clog" --json "$WORK/$name.capture.json" > /dev/null || true
    "$py" -I "$here/summarise.py" "$t" "$log" --json "$WORK/$name.json" --baseline "$WORK/$name.capture.json" || touch "$WORK/failed"
  else
    "$py" -I "$here/summarise.py" "$t" "$log" --json "$WORK/$name.json" --warnings || touch "$WORK/failed"
  fi
  echo
done | tee "$WORK/summary.txt"
# fails when the twin is at fault: any failure on a generated tree, or one the twin adds to a capture
[ ! -e "$WORK/failed" ]

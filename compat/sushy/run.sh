#!/usr/bin/env bash
# Runs OpenStack sushy, unmodified and pinned, against dc-twin's lab estate: the Redfish library
# Ironic's redfish hardware type (and so Metal3's bare-metal operator) drives every BMC with.
# check.py makes the calls Ironic's redfish driver makes -- power, boot device, BIOS, secure
# boot, inspection, firmware, sensors -- and checks each write in the twin's model and event log.
#
#   compat/sushy/run.sh                       # PASS/FAIL table on stdout, artefacts under $WORK
#   WORK=/tmp/x DCTWIN=./dctwin PYTHON=python3.12 compat/sushy/run.sh
#   DCTWIN_STATE=<dir> compat/sushy/run.sh    # reuse a running estate (its BMCs are power-cycled)
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
SUSHY_VERSION=5.14.0
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
# the BMCs checked: generated DMTF tree, then captured iDRAC, Supermicro, iLO, XCC and GB300 trays
DEVICES=${DEVICES:-"c02/server-01 b01/server-01 b02/server-01 c01/server-02 b05/server-01 a01/compute-tray-01"}
mkdir -p "$WORK"

# sushy 5.14 needs Python 3.11 or later
if [ -z "${PYTHON:-}" ]; then
  for p in python3.13 python3.12 python3.11 python3; do
    if command -v "$p" >/dev/null && "$p" -c 'import sys; sys.exit(sys.version_info < (3, 11))'; then PYTHON=$p; break; fi
  done
fi
: "${PYTHON:?no Python 3.11+ found; set PYTHON}"
if [ ! -x "$WORK/venv/bin/python" ]; then
  "$PYTHON" -m venv "$WORK/venv"
  "$WORK/venv/bin/pip" install -q "sushy==$SUSHY_VERSION"
fi

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
export DCTWIN
"$DCTWIN" credentials "$DEVICES" > "$WORK/credentials.json"
# the GB300 tray serves two systems (the host and the HGX baseboard): Ironic is given the host's
# redfish_system_id, as an operator would
"$WORK/venv/bin/python" -I "$here/check.py" "$WORK/credentials.json" \
  --system-id a01/compute-tray-01=/redfish/v1/Systems/System_0 \
  --json-out "$WORK/sushy.json" | tee "$WORK/sushy.txt"

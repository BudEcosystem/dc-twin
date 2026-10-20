#!/usr/bin/env bash
# Runs OpenStack Ironic -- the bare-metal service Metal3 and OpenStack use, unmodified and pinned --
# standalone against dc-twin's lab estate, and drives it as an operator does: enroll, validate,
# manage, power, boot device, out-of-band inspection, a BIOS change by manual cleaning, provide.
# Then it changes the datacenter under Ironic (power cut from the twin, a failed PSU, a hung BMC)
# and watches Ironic notice. flow.py checks every step on the twin's side as well as in Ironic.
#
#   compat/ironic/run.sh                        # PASS/FAIL table on stdout, artefacts under $WORK
#   WORK=/tmp/x DCTWIN=./bin/dctwin PYTHON=python3.12 compat/ironic/run.sh
#   ADDRESSING=hostname compat/ironic/run.sh    # BMCs by host name, TLS verified with the estate CA
#   ADDRESSING=loopback compat/ironic/run.sh    # BMCs at their loopback host:port, no gateway
#   DCTWIN_STATE=<dir> compat/ironic/run.sh     # reuse a running estate (its BMCs are power-cycled,
#                                               # faulted and get a BIOS change)
#   CLI=0 compat/ironic/run.sh                  # skip the closing `baremetal` CLI listing
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
IRONIC_VERSION=39.0.0           # released 2026-09, the 2026.2 series
IRONICCLIENT_VERSION=6.3.0      # the `baremetal` CLI
CONSTRAINTS=${CONSTRAINTS:-https://releases.openstack.org/constraints/upper/2026.2}
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
API_PORT=${API_PORT:-16385}
ADDRESSING=${ADDRESSING:-ip}
CLI=${CLI:-1}
# the BMCs driven: generated DMTF tree, then captured iDRAC, Supermicro, iLO, XCC and GB300 tray
DEVICES=${DEVICES:-"c02/server-01 b01/server-01 b02/server-01 c01/server-02 b05/server-01 a01/compute-tray-01"}
mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd)

# Ironic 39 needs Python 3.11 or later
if [ -z "${PYTHON:-}" ]; then
  for p in python3.13 python3.12 python3.11 python3; do
    if command -v "$p" >/dev/null && "$p" -c 'import sys; sys.exit(sys.version_info < (3, 11))'; then PYTHON=$p; break; fi
  done
fi
: "${PYTHON:?no Python 3.11+ found; set PYTHON}"
if [ ! -x "$WORK/venv/bin/ironic" ]; then
  echo "installing ironic $IRONIC_VERSION and python-ironicclient $IRONICCLIENT_VERSION (constraints $CONSTRAINTS)" >&2
  "$PYTHON" -m venv "$WORK/venv"
  "$WORK/venv/bin/pip" install -q --timeout 30 --retries 10 -c "$CONSTRAINTS" \
    "ironic==$IRONIC_VERSION" "python-ironicclient==$IRONICCLIENT_VERSION"
  if [ "$(uname -s)" = Darwin ]; then
    # macOS only: setproctitle reaches LaunchServices, which is not fork-safe, so every conductor
    # process cotyledon forks dies with SIGSEGV in set_process_title. cotyledon treats the module
    # as optional; without it processes keep their default titles. Ironic itself is untouched.
    "$WORK/venv/bin/pip" uninstall -y -q setproctitle
  fi
  "$WORK/venv/bin/pip" freeze > "$WORK/pip-freeze.txt"
fi

# the estate
started_twin=0
if [ -n "${DCTWIN_STATE:-}" ] && [ -f "$DCTWIN_STATE/control.json" ]; then
  echo "using the running estate at $DCTWIN_STATE" >&2
else
  export DCTWIN_STATE="$WORK/state"
  rm -rf "$DCTWIN_STATE"
  "$DCTWIN" up "$root/examples/estates/lab-model.yaml" --state "$DCTWIN_STATE" --adapters redfish,dhcp > "$WORK/up.log" 2>&1 &
  for _ in $(seq 1 120); do [ -f "$DCTWIN_STATE/control.json" ] && break; sleep 0.25; done
  [ -f "$DCTWIN_STATE/control.json" ] || { cat "$WORK/up.log"; exit 1; }
  started_twin=1
fi
export DCTWIN DCTWIN_STATE
GATEWAY=$("$WORK/venv/bin/python" -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["gateway"])' "$DCTWIN_STATE/control.json")

# Ironic: one process, sqlite, no Keystone, no message bus (ironic.conf.in)
if curl -s -o /dev/null "http://127.0.0.1:$API_PORT/"; then
  echo "port $API_PORT is in use; set API_PORT" >&2; exit 1
fi
IR="$WORK/ironic"
rm -rf "$IR"
mkdir -p "$IR"/{log,state,tmp,locks,httpboot,tftpboot,images}
sed -e "s|@WORK@|$IR|g" -e "s|@API_PORT@|$API_PORT|g" -e "s|@HOST@|twin-conductor|g" \
  "$here/ironic.conf.in" > "$IR/ironic.conf"
"$WORK/venv/bin/ironic-dbsync" --config-file "$IR/ironic.conf" create_schema > "$IR/dbsync.log" 2>&1

ironic_pid=
cleanup() {
  if [ -n "$ironic_pid" ]; then
    kill "$ironic_pid" 2>/dev/null || true
    for _ in $(seq 1 40); do kill -0 "$ironic_pid" 2>/dev/null || break; sleep 0.25; done
    pkill -f "$IR/ironic.conf" 2>/dev/null || true
  fi
  if [ "$started_twin" = 1 ]; then "$DCTWIN" down >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

# Ironic reaches the BMCs as it would on a site network: through the twin's site gateway (an HTTP
# CONNECT proxy, which requests -- and so sushy -- honours from HTTPS_PROXY) at each BMC's leased
# address or host name. The API stays on loopback.
(cd "$IR" && exec env HTTPS_PROXY="http://$GATEWAY" https_proxy="http://$GATEWAY" \
  NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost \
  "$WORK/venv/bin/ironic" --config-file "$IR/ironic.conf" > "$IR/ironic.stdout" 2>&1) &
ironic_pid=$!
echo "ironic $IRONIC_VERSION starting (pid $ironic_pid, API http://127.0.0.1:$API_PORT, BMCs via gateway $GATEWAY)" >&2
for _ in $(seq 1 300); do
  curl -s -o /dev/null "http://127.0.0.1:$API_PORT/v1" && break
  kill -0 "$ironic_pid" 2>/dev/null || { tail -20 "$IR/ironic.stdout" "$IR/log/ironic.log"; exit 1; }
  sleep 1
done
curl -s -o /dev/null "http://127.0.0.1:$API_PORT/v1" || { echo "Ironic API did not come up" >&2; tail -20 "$IR/log/ironic.log"; exit 1; }

"$DCTWIN" credentials "$DEVICES" > "$WORK/credentials.json"
# the GB300 tray serves two systems (the host and the HGX baseboard): Ironic is given the host's
# redfish_system_id, as an operator would
set +e
"$WORK/venv/bin/python" -I "$here/flow.py" "$WORK/credentials.json" \
  --api "http://127.0.0.1:$API_PORT" --addressing "$ADDRESSING" --ca "$DCTWIN_STATE/estate-ca.pem" \
  --system-id a01/compute-tray-01=/redfish/v1/Systems/System_0 \
  --ironic-log "$IR/log/ironic.log" --json-out "$WORK/ironic.json" --evidence "$WORK/evidence.txt" \
  | tee "$WORK/ironic.txt"
status=${PIPESTATUS[0]}
set -e

if [ "$CLI" = 1 ]; then
  # the real CLI against the same service, for the record
  export OS_AUTH_TYPE=none OS_ENDPOINT="http://127.0.0.1:$API_PORT" OS_BAREMETAL_API_VERSION=latest NO_PROXY=127.0.0.1
  {
    echo "\$ baremetal node list --fields name provision_state power_state maintenance fault health"
    "$WORK/venv/bin/baremetal" node list --fields name provision_state power_state maintenance fault health
    echo "\$ baremetal port list --fields node_uuid address"
    "$WORK/venv/bin/baremetal" port list --fields node_uuid address
  } 2>&1 | tee "$WORK/cli.txt"
fi
echo "artefacts: $WORK (ironic.txt, evidence.txt, ironic.json, ironic/log/ironic.log, credentials.json)" >&2
exit "$status"

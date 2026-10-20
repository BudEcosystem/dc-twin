#!/usr/bin/env bash
# End-to-end: the reference stack (cmd/refstack) against the NVL72 estate. The stack discovers
# BMCs from relayed DHCP, ingests them over Redfish through the site gateway, consumes BMS points
# over MQTT, and the scenarios assert on the hardware side, the event log, the BMS publisher's
# log and the stack's accessors.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
work=${WORK:-$(mktemp -d)}
bin="$work/bin"; mkdir -p "$bin"
( cd "$root" && go build -o "$bin/dctwin" ./cmd/dctwin && go build -o "$bin/refstack" ./cmd/refstack )
state="$work/state"; rm -rf "$state"; mkdir -p "$state"
"$bin/refstack" --gateway 127.0.0.1:18443 --expected "$state/stack-inputs/expected-machines.json" > "$work/refstack.log" 2>&1 &
ref=$!
sleep 1
"$bin/dctwin" up "$root/examples/estates/nvl72-refstack.yaml" --state "$state" --gateway 127.0.0.1:18443 \
  --adapters redfish,dhcp,bms,snmp,ipmi,ufm,nvue > "$work/up.log" 2>&1 &
for _ in $(seq 1 80); do [ -f "$state/control.json" ] && break; sleep 0.25; done
cleanup() { DCTWIN_STATE="$state" "$bin/dctwin" down >/dev/null 2>&1 || true; kill $ref 2>/dev/null || true; }
trap cleanup EXIT
export DCTWIN_STATE="$state"
rc=0
for sc in bmc-discovery nvl72-leak-response tray-leak-isolation; do
  "$bin/dctwin" scenario run "$root/examples/scenarios/$sc.yaml" | tee "$work/$sc.result" || rc=1
  grep -q '^result: PASSED' "$work/$sc.result" || rc=1
done
echo "--- reference stack log"; grep -E 'ingest|leak|isolat|unrelayed' "$work/refstack.log" | tail -12
echo "--- machines ingested:"; curl -s http://127.0.0.1:18080/v1/machines | python3 -c 'import json,sys; from collections import Counter; ms=json.load(sys.stdin); print(len(ms), dict(Counter(m["hardware_type"] for m in ms)))'
exit $rc

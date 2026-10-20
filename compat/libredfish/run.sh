#!/usr/bin/env bash
# Runs NICo's libredfish integration suite, unmodified, against dc-twin's emulated BMCs.
# The suite normally starts a Python mockup server per vendor; we swap that one fixture file
# for a forwarder to the twin BMC of the same vendor (forwarder.py). Every assertion in
# tests/integration_test.rs is libredfish's own.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
REF=a51ffb1ffeae15d8f9471224cb799d54007098a2
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
mkdir -p "$WORK"
if [ ! -d "$WORK/libredfish/.git" ]; then
  git clone -q https://github.com/dsx-ai-factory/libredfish "$WORK/libredfish"
fi
git -C "$WORK/libredfish" checkout -q "$REF"
git -C "$WORK/libredfish" checkout -q -- tests/redfishMockupServer.py
cp "$here/forwarder.py" "$WORK/libredfish/tests/redfishMockupServer.py"

state="$WORK/state"
rm -rf "$state"
"$DCTWIN" up "$root/examples/estates/oracle-libredfish.yaml" --state "$state" --adapters redfish > "$WORK/up.log" 2>&1 &
for _ in $(seq 1 80); do [ -f "$state/control.json" ] && break; sleep 0.25; done
trap 'DCTWIN_STATE="$state" "$DCTWIN" down >/dev/null 2>&1 || true' EXIT
DCTWIN_STATE="$state" "$DCTWIN" endpoints > "$WORK/endpoints.json"
python3 - "$WORK/endpoints.json" "$WORK/map.json" <<'PY'
import json, sys
ep = json.load(open(sys.argv[1]))["endpoints"]
site = "site-1/hall-a/"
devices = {
    "nvidia_gb200": "a01/compute-tray-04",      # tray index 3, as the capture's CBC_0 reports
    "nvidia_gbswitch": "a01/switch-tray-01",
    "nvidia_dpu": "a01/compute-tray-01/bf3-0",
    "liteon_powershelf": "a01/power-shelf-01",
    "dell": "b01/server-01",
    "supermicro": "b02/server-01",
    "lenovo": "b03/server-01",
    "nvidia_viking": "b04/server-01",
    "hpe": "c01/server-01",
    "nvidia_gh200": "b06/server-01",
    "delta_powershelf": "a02/power-shelf-01",
    "dell_multi_dpu": "c03/server-01",
}
m = {v: ep[site + p]["redfish"] for v, p in devices.items()}
json.dump(m, open(sys.argv[2], "w"), indent=2)
print(json.dumps(m, indent=2))
PY
cd "$WORK/libredfish"
export DCTWIN_LIBREDFISH_MAP="$WORK/map.json"
RUSTUP_TOOLCHAIN=${RUSTUP_TOOLCHAIN:-stable} cargo test --test integration_test -- --test-threads=1 ${TESTS:-} 2>&1 | tee "$WORK/cargo-test.log"

#!/usr/bin/env bash
# Drives the twin's NMX-C and Rack Management Service with NVIDIA's own client crates:
#   - NICo's libnmxc (crates/libnmxc), the NMX-C client NICo's nvlink-manager uses, and the
#     NmxControllerClient it generates (for Subscribe), from NICo at the commit the twin's NMX-C
#     interface asset is pinned to;
#   - librms from dsx-ai-factory/nv-rms-client at the commit the twin's RMS bindings were
#     generated from, the RMS client NICo's component manager uses.
# Every request and every check is in src/bin/*.rs; none of the twin's code is in the clients.
# Needs cargo and protoc (PROTOC=…); NICO_SRC may point at an existing NICo checkout, ESTATE at
# another copy of the lab estate (for instance one with its own spec.adapters.network.basePort).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
NICO_REF=02b92f18fed776c8ef0056803338707c5e6bcb6b
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
ESTATE=${ESTATE:-$root/examples/estates/lab-model.yaml}
mkdir -p "$WORK"
if [ -z "${NICO_SRC:-}" ]; then
  NICO_SRC="$WORK/infra-controller"
  if [ ! -d "$NICO_SRC/crates/libnmxc" ]; then
    curl -sSL "https://codeload.github.com/dsx-ai-factory/infra-controller/tar.gz/$NICO_REF" | tar -xz -C "$WORK"
    mv "$WORK/infra-controller-$NICO_REF" "$NICO_SRC"
  fi
fi

crate="$WORK/check"
mkdir -p "$crate"
sed -e "s#@NICO_SRC@#$NICO_SRC#" -e "s#@NICO_REF@#${NICO_REF:0:7}#" "$here/Cargo.toml.in" > "$crate/Cargo.toml"
rm -rf "$crate/src" && cp -R "$here/src" "$crate/src"
[ -f "$crate/Cargo.lock" ] || cp "$NICO_SRC/Cargo.lock" "$crate/Cargo.lock" # NICo's pins where they apply
target=${CARGO_TARGET_DIR:-$WORK/target}
(cd "$crate" && RUSTUP_TOOLCHAIN=${RUSTUP_TOOLCHAIN:-stable} CARGO_TARGET_DIR="$target" CARGO_NET_GIT_FETCH_WITH_CLI=true cargo build --bins)
bin="$target/debug"

state="$WORK/state"
rm -rf "$state"
"$DCTWIN" assets fetch
current="$HOME/.dc-twin/current" # `up` points it at this estate; it is given back on exit
previous=$(cat "$current" 2>/dev/null || true)
"$DCTWIN" up "$ESTATE" --state "$state" --adapters dhcp,nmxc,rms > "$WORK/up.log" 2>&1 &
for _ in $(seq 1 120); do [ -f "$state/control.json" ] && break; sleep 0.25; done
export DCTWIN_STATE="$state"
trap '"$DCTWIN" down >/dev/null 2>&1 || true; [ -n "$previous" ] && printf %s "$previous" > "$current"' EXIT
sleep 3 # BMC leases

nmxc() { # <rack> <inject>
  eval "$(python3 - "$state/nmxc-endpoints.json" "$1" <<'PY'
import json, sys
e = next(e for e in json.load(open(sys.argv[1])) if e["device"].endswith(sys.argv[2] + "/switch-tray-01"))
print(f'export NMXC_URL={e["url"]} NMXC_CA={e["ca"]} NMXC_AUTHORITY={e["authority"]}')
PY
)"
  NMXC_INJECT="$2" "$bin/nmxc_check"
}
nmxc a01 "\"$DCTWIN\" fault inject a01/switch-tray-03 nvidia/nvlink/switch-tray-failure" | tee "$WORK/nmxc-a01.log"
nmxc a02 "\"$DCTWIN\" fault inject a02/compute-tray-07/gpu-2 nvidia/xid/xid-79" | tee "$WORK/nmxc-a02.log"

"$DCTWIN" get 'a01/**' --json > "$WORK/a01-devices.json"
"$DCTWIN" credentials 'a01/**' > "$WORK/a01-credentials.json"
python3 - "$WORK/a01-devices.json" "$WORK/a01-credentials.json" > "$WORK/rms-inventory.json" <<'PY'
import json, sys
creds = {c["path"]: c for c in json.load(open(sys.argv[2]))}
out = []
for d in json.load(open(sys.argv[1])):
    if d["class"] not in ("compute-tray", "switch-tray", "power-shelf") or d["path"] not in creds:
        continue
    c = creds[d["path"]]
    out.append({"path": d["path"], "rack": d["rack"], "class": d["class"], "bmcMAC": d["identity"]["bmcMAC"],
                "siteIP": d["endpoint"]["siteIP"], "user": c["user"], "password": c["password"]})
json.dump(out, sys.stdout, indent=1)
PY
eval "$(python3 - "$state/rms.json" <<'PY'
import json, sys
s = json.load(open(sys.argv[1]))
print(f'export RMS_URL={s["url"]} RMS_CA={s["ca"]}')
PY
)"
RMS_INVENTORY="$WORK/rms-inventory.json" RMS_TRAY=site-1/hall-a/a01/compute-tray-05 "$bin/rms_check" | tee "$WORK/rms.log"

"$DCTWIN" events --select 'a01/compute-tray-05' > "$WORK/events-tray05.log" || true
grep -E 'rms' "$WORK/events-tray05.log" | tail -20 || true

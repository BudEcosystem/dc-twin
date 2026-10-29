#!/usr/bin/env bash
# compat/nico/run.sh — run NVIDIA Infra Controller (NICo), unmodified, against the twin, and print
# a PASS/FAIL table of what it did. README.md explains every step and what it showed.
#
#   compat/nico/run.sh all            fetch build classify up nico load dhcp discover health
#                                     faults consumer report down
#   compat/nico/run.sh <step>...      any of those steps, in order
#   compat/nico/run.sh ready          the Ready path (README), Linux only: fetch boot build up pxe nico
#                                     load dhcp host report down, on examples/estates/dgx-nico-ready.yaml,
#                                     where a DGX H100 is a real x86_64 VM that network-boots NICo's
#                                     iPXE and scout and is driven by NICo to Ready
#   compat/nico/run.sh trays          the trays path (README), Linux only: fetch boot build up pxe nico
#                                     load dhcp trays report down, on
#                                     examples/estates/nvl72-nico-impersonated.yaml, where NICo's DPU
#                                     software and scout are impersonated on the model machines (D55)
#                                     and NICo takes the GB300 trays through DPU init and host
#                                     discovery towards Ready, and the rack through its maintenance
#
# What runs, natively on this host (macOS or Linux):
#   NICo at NICO_SHA built from source: nico-api (carbide-api, --no-default-features as NICo's
#   dev/mac-local-dev does) with its site explorer and state controllers, forge-dhcp-server
#   (controller mode), forge-hw-health, carbide-dsx-exchange-consumer, nico-admin-cli,
#   bmc-explorer-cli, and NICo Flow (rest-api/flow, Go) with a Temporal dev server.
#   Postgres and Vault as containers dctwin-nico-pg and dctwin-nico-vault.
#   The twin: examples/estates/nvl72-nico.yaml (adapters redfish, dhcp, bms); its BMC network is
#   `dhcp: stack`, relayed to NICo's dhcp-server; NICo's HTTPS reaches each leased BMC address
#   through the twin's site gateway (HTTPS_PROXY).
#
# Environment (defaults in brackets):
#   WORK [${TMPDIR:-/tmp}/dctwin-nico]  everything this script creates; delete it to start over
#   NICO_SHA [pinned below]             infra-controller commit
#   NICO_SRC                            an extracted source tree to use instead of downloading
#   RUSTUP_TOOLCHAIN                    e.g. `stable` when NICo's pinned toolchain is not installed
#   NICO_DHCP_PATCH                     macOS only: the forge-dhcp-server patch README.md shows
#   VAULT_BINARY                        a linux vault binary to run in alpine:3.21 instead of
#                                       pulling hashicorp/vault:1.20.2
#   TEMPORAL_BIN                        a temporal CLI to use instead of downloading one
#   Binaries already in $WORK/bin are not rebuilt.
#   API_PORT [11079] METRICS_PORT [11080] PG_PORT [55432] VAULT_PORT [18201] DHCP_PORT [26767]
#   GATEWAY [127.0.0.1:18443] FLOW_PORT [15051] TEMPORAL_PORT [17233]
#   CONSUMER_METRICS_PORT [19009] HEALTH_METRICS_PORT [19010] DHCP_METRICS_PORT [16768]
#   EXPLORE_MODE [compare-result] DPU_POLICY [manage]
#   COMPONENT_BACKEND [rms]             NVLink switch and power shelf backend: the twin's RMS, or mock
#   BMS_MIRRORS [nico-legacy,nico-api]  the twin BMS's mirrors (stack output bmsConsumerMirror):
#                                       nico-legacy for the dsx-exchange-consumer, nico-api for
#                                       nico-api's own DSX Exchange integration (README, twin gap 4)
# The Ready path also reads:
#   READY_ESTATE [examples/estates/dgx-nico-ready.yaml]  READY_HOST [b01/server-01]
#   BOOT_ARTIFACTS [$WORK/boot]         ipxe.efi, scout.efi and NICo's Linux programs (carbide-pxe,
#                                       nico-api, the dhcp-server, the admin CLI); built by
#                                       boot-artifacts.sh when absent
#   PXE_LINK_PORT [17800]               the twin's routed site network link to svcnet
#   SVC_NS [dctwin-nico-svc]            the network namespace NICo's PXE service and API run in (sudo)
#   READY_WAIT [5400]                   seconds to wait for the host to reach Ready
#   READY_STALL [1800]                  give up once one state has held this long
# The trays path also reads:
#   TRAYS_ESTATE [examples/estates/nvl72-nico-impersonated.yaml]
#   TRAYS_WAIT [7200]                   seconds to wait for every tray to reach Ready
#   TRAYS_STALL [1800]                  give up once no tray has changed state for this long
#   IPXE_AARCH64                        NICo's aarch64 iPXE, on a host of another architecture
# On the trays path the twin runs redfish, dhcp, bms, nvue, rms, nmxc and nicoagent; NICo's PXE
# service and API run in the namespace as on the Ready path, with the BMC and admin networks
# routed into it, NICo's RMS backend is the twin's RMS (forwarded in), and NVLink partitioning is
# on, against the twin's NMX-C at each switch tray's NVOS address.
# On the Ready path the twin also runs ipmi and vm (and no rms: a DGX H100 rack has no rack
# manager); Vault issues machine certificates under the harness CA; the estate CA is loaded as a
# TPM CA; and a second svcnet relays the DGX BMC's IPMI at its lease into the namespace.
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
NICO_SHA=${NICO_SHA:-02b92f18fed776c8ef0056803338707c5e6bcb6b}
TEMPORAL_VERSION=${TEMPORAL_VERSION:-1.9.1}
WORK=${WORK:-${TMPDIR:-/tmp}/dctwin-nico}
WORK=${WORK%/}
API_PORT=${API_PORT:-11079}
METRICS_PORT=${METRICS_PORT:-11080}
PG_PORT=${PG_PORT:-55432}
VAULT_PORT=${VAULT_PORT:-18201}
DHCP_PORT=${DHCP_PORT:-26767}
DHCP_METRICS_PORT=${DHCP_METRICS_PORT:-16768}
GATEWAY=${GATEWAY:-127.0.0.1:18443}
FLOW_PORT=${FLOW_PORT:-15051}
TEMPORAL_PORT=${TEMPORAL_PORT:-17233}
CONSUMER_METRICS_PORT=${CONSUMER_METRICS_PORT:-19009}
HEALTH_METRICS_PORT=${HEALTH_METRICS_PORT:-19010}
EXPLORE_MODE=${EXPLORE_MODE:-compare-result}
DPU_POLICY=${DPU_POLICY:-manage}
COMPONENT_BACKEND=${COMPONENT_BACKEND:-rms}
BMS_MIRRORS=${BMS_MIRRORS:-nico-legacy,nico-api}
ESTATE=${ESTATE:-$repo/examples/estates/nvl72-nico.yaml}
READY_ESTATE=${READY_ESTATE:-$repo/examples/estates/dgx-nico-ready.yaml}
TRAYS_ESTATE=${TRAYS_ESTATE:-$repo/examples/estates/nvl72-nico-impersonated.yaml}
TRAYS_WAIT=${TRAYS_WAIT:-7200}
READY_HOST=${READY_HOST:-b01/server-01}
SVC_NS=${SVC_NS:-dctwin-nico-svc}
READY_WAIT=${READY_WAIT:-5400}
PXE_LINK_PORT=${PXE_LINK_PORT:-17800}
CLASSIFY_ESTATE=${CLASSIFY_ESTATE:-$repo/examples/estates/lab-model.yaml}
PG=dctwin-nico-pg
VAULT=dctwin-nico-vault
SITE_PASSWORD=vault-password   # the site-wide BMC root password NICo rotates BMCs to

SRC=${NICO_SRC:-$WORK/src/infra-controller}
BIN=$WORK/bin
STATE=$WORK/state
LOGS=$WORK/logs
OUT=$WORK/out
CERTS=$WORK/certs
RESULTS=$OUT/results.tsv
BOOT=${BOOT_ARTIFACTS:-$WORK/boot}
DCTWIN=${DCTWIN:-$WORK/bin/dctwin}
mkdir -p "$WORK" "$BIN" "$LOGS" "$OUT" "$CERTS"
touch "$RESULTS"

say() { printf '\n== %s\n' "$*"; }
die() { echo "run.sh: $*" >&2; exit 1; }
need() { for c in "$@"; do command -v "$c" >/dev/null || die "$c is required"; done; }
# result <check> <PASS|FAIL|SKIP|KNOWN> <evidence>
result() {
  { grep -v "^$1	" "$RESULTS" || true; } >"$RESULTS.tmp"
  printf '%s\t%s\t%s\n' "$1" "$2" "$3" >>"$RESULTS.tmp"
  mv "$RESULTS.tmp" "$RESULTS"
  printf '  [%s] %s — %s\n' "$2" "$1" "$3"
}
# wait_for <seconds> <description> <command...>
wait_for() {
  local t=$1 what=$2; shift 2
  local end=$((SECONDS + t))
  until "$@" >/dev/null 2>&1; do
    [ $SECONDS -lt $end ] || { echo "  timed out after ${t}s waiting for $what"; return 1; }
    sleep 3
  done
}
pidfile() { echo "$WORK/$1.pid"; }
start_bg() { # start_bg <name> <cmd...>: background, logs to $LOGS/<name>.log, pid in $WORK
  local name=$1; shift
  stop_bg "$name"
  nohup "$@" >"$LOGS/$name.log" 2>&1 </dev/null &
  echo $! >"$(pidfile "$name")"
}
stop_bg() { # stop_bg <name>: SIGTERM, then wait for the process to exit (SIGKILL after 30 s)
  local f pid i; f=$(pidfile "$1")
  [ -f "$f" ] || return 0
  pid=$(cat "$f"); rm -f "$f"
  # a process started under sudo (the Ready path's namespace) is stopped with sudo
  kill "$pid" 2>/dev/null || sudo -n kill "$pid" 2>/dev/null || return 0
  # A nico-api in its graceful shutdown has already stopped its BMS worker but keeps its broker
  # subscription and its log file until it exits; a replacement started meanwhile would share
  # both, and the old one logs "BMS DSX Exchange command dropped because the worker stopped" for
  # every BMS message the broker still delivers to it (README, NICo-side findings).
  for i in $(seq 60); do ps -p "$pid" >/dev/null 2>&1 || return 0; sleep 0.5; done
  kill -9 "$pid" 2>/dev/null || sudo -n kill -9 "$pid" 2>/dev/null || true
}

export DCTWIN_STATE=$STATE
twin() { "$DCTWIN" "$@"; }
admin() {
  "$BIN/nico-admin-cli" --api-url "https://localhost:$API_PORT" --root-ca-path "$CERTS/ca.crt" \
    --client-cert-path "$CERTS/client.crt" --client-key-path "$CERTS/client.key" "$@" 2>/dev/null
}
last_seq() { twin events 2>/dev/null | tail -1 | awk '{print substr($1,2)}'; }
uname_s=$(uname -s)
arch=$(uname -m); case $arch in x86_64) goarch=amd64;; aarch64|arm64) goarch=arm64;; *) goarch=$arch;; esac
os=$(echo "$uname_s" | tr '[:upper:]' '[:lower:]')
export API_PORT CERTS BIN DCTWIN

# ------------------------------------------------------------------------------------- fetch
step_fetch() {
  say "fetch NICo $NICO_SHA and the Temporal CLI $TEMPORAL_VERSION"
  need curl tar shasum
  if [ -d "$SRC/crates" ]; then echo "  NICo source present at $SRC"
  else
    mkdir -p "$WORK/src" "$SRC"
    curl -fsSL -o "$WORK/src/nico.tgz" "https://codeload.github.com/dsx-ai-factory/infra-controller/tar.gz/$NICO_SHA" ||
      die "download failed"
    tar xzf "$WORK/src/nico.tgz" -C "$SRC" --strip-components 1
    echo "  extracted $(ls "$SRC/crates" | wc -l | tr -d ' ') crates"
  fi
  if netns; then :  # the Ready and trays paths run no Flow, so no Temporal
  elif [ -n "${TEMPORAL_BIN:-}" ]; then cp "$TEMPORAL_BIN" "$BIN/temporal"
  elif [ ! -x "$BIN/temporal" ]; then
    local f="temporal_cli_${TEMPORAL_VERSION}_${os}_${goarch}.tar.gz" u="https://github.com/temporalio/cli/releases/download/v$TEMPORAL_VERSION"
    curl -fsSL -o "$WORK/src/$f" "$u/$f" && curl -fsSL -o "$WORK/src/temporal-checksums.txt" "$u/checksums.txt"
    (cd "$WORK/src" && grep " $f\$" temporal-checksums.txt | shasum -a 256 -c -) || die "temporal checksum"
    tar xzf "$WORK/src/$f" -C "$BIN" temporal
  fi
  result fetch PASS "NICo $NICO_SHA; temporal $TEMPORAL_VERSION"
}

# ------------------------------------------------------------------------------------- build
cargo_env() {
  export CARGO_TARGET_DIR=${CARGO_TARGET_DIR:-$WORK/target}
  export CARGO_PROFILE_DEV_DEBUG=${CARGO_PROFILE_DEV_DEBUG:-0}
  export CARGO_INCREMENTAL=0
  export CARGO_NET_GIT_FETCH_WITH_CLI=true
  # NICo's .cargo/config.toml denies every warning; a compiler newer than its pin can warn where
  # the pin did not, so the same flags apply without -D warnings.
  export RUSTFLAGS=${RUSTFLAGS:-"-D unreachable-pub --cfg tokio_unstable -A dependency-on-unit-never-type-fallback -A unknown_lints"}
}
built() { local b; for b in "$@"; do [ -x "$BIN/$b" ] || return 1; done; }
step_build() {
  if netns; then
    # the Ready and trays paths' NICo programs were built for Linux by boot-artifacts.sh (step boot)
    say "NICo's programs from $BOOT, and the twin"
    local b
    for b in carbide-api forge-dhcp-server nico-admin-cli; do
      [ -x "$BOOT/$b" ] || die "$BOOT/$b is missing (step boot)"
      cp "$BOOT/$b" "$BIN/$b"
    done
    if [ "$DCTWIN" = "$WORK/bin/dctwin" ] || [ ! -x "$DCTWIN" ]; then (cd "$repo" && go build -o "$DCTWIN" ./cmd/dctwin) || die "go build dctwin"; fi
    (cd "$repo" && go build -o "$BIN/nicoinputs" ./compat/nico/nicoinputs) || die "go build nicoinputs"
    result build PASS "nico-api, forge-dhcp-server, admin-cli and carbide-pxe for linux/$goarch at $NICO_SHA (boot-artifacts.sh)"
    return
  fi
  say "build NICo (the first build takes a long time)"
  need cargo go
  cargo_env
  if ! built bmc-explorer-cli nico-admin-cli carbide-dsx-exchange-consumer forge-hw-health; then
    (cd "$SRC" && cargo build --locked -p bmc-explorer-cli -p nico-admin-cli -p carbide-dsx-exchange-consumer -p carbide-health) || die "cargo build"
    for b in bmc-explorer-cli nico-admin-cli carbide-dsx-exchange-consumer forge-hw-health; do cp "$CARGO_TARGET_DIR/debug/$b" "$BIN/"; done
  fi
  if ! built carbide-api; then
    (cd "$SRC" && cargo build --locked -p carbide-api --no-default-features) || die "cargo build carbide-api"
    cp "$CARGO_TARGET_DIR/debug/carbide-api" "$BIN/"
  fi
  if ! built forge-dhcp-server; then
    local dsrc=$SRC
    if [ "$uname_s" = Darwin ]; then
      # socket2 has no SO_BINDTODEVICE on macOS, so NICo's dhcp-server does not compile there
      # unmodified (README: the patch, and why).
      [ -n "${NICO_DHCP_PATCH:-}" ] || die "macOS: forge-dhcp-server needs NICO_DHCP_PATCH (README.md)"
      dsrc=$WORK/src/infra-controller-macos-dhcp
      rm -rf "$dsrc"; cp -R "$SRC" "$dsrc"
      (cd "$dsrc" && patch -p1 <"$NICO_DHCP_PATCH") || die "patch"
    fi
    (cd "$dsrc" && cargo build --locked -p carbide-dhcp-server) || die "cargo build dhcp-server"
    cp "$CARGO_TARGET_DIR/debug/forge-dhcp-server" "$BIN/"
  fi
  if ! built flow; then
    (cd "$SRC/rest-api" && GOFLAGS=-mod=readonly go build -o "$BIN/flow" ./flow) || die "go build flow"
  fi
  # the twin is this checkout's (a few seconds to build), so the BMS mirrors the run selects exist
  # in it; a DCTWIN given from outside is used as it is
  if [ "$DCTWIN" = "$WORK/bin/dctwin" ] || [ ! -x "$DCTWIN" ]; then (cd "$repo" && go build -o "$DCTWIN" ./cmd/dctwin); fi
  (cd "$repo" && go build -o "$BIN/nicoinputs" ./compat/nico/nicoinputs)
  if built carbide-api forge-dhcp-server forge-hw-health carbide-dsx-exchange-consumer nico-admin-cli bmc-explorer-cli flow; then
    result build PASS "nico-api, forge-dhcp-server, forge-hw-health, dsx-exchange-consumer, admin-cli, bmc-explorer-cli, flow"
  else result build FAIL "missing binaries in $BIN"; fi
}

# --------------------------------------------------------------------------------- the twin
twin_up() { # twin_up <name> <estate> <state> <adapters> [gateway]
  local name=$1 estate=$2 state=$3 adapters=$4 gw=${5:-}
  local prev=""; [ -f "$HOME/.dc-twin/current" ] && prev=$(cat "$HOME/.dc-twin/current")
  rm -rf "$state"
  start_bg "$name" "$DCTWIN" up "$estate" --state "$state" --adapters "$adapters" ${gw:+--gateway "$gw"}
  wait_for 90 "the twin ($name)" test -s "$state/endpoints.json" || return 1
  sleep 2
  # dctwin up records itself as the current estate; leave whatever was current before
  if [ -n "$prev" ]; then echo "$prev" >"$HOME/.dc-twin/current"; fi
  head -5 "$LOGS/$name.log" | sed 's/^/  /'
}

step_classify() {
  say "1. bmc-explorer-cli (NICo's BMC classifier) against one twin BMC of each kind"
  local st=$WORK/state-classify
  twin_up twin-classify "$CLASSIFY_ESTATE" "$st" redfish || { result classify FAIL "twin did not start"; return; }
  DCTWIN_STATE=$st DCTWIN=$DCTWIN "$here/classify.sh" "$BIN/bmc-explorer-cli" "$OUT/classify" \
    a01/compute-tray-01 a02/compute-tray-01 a01/switch-tray-01 a01/compute-tray-01/bf3-0 a01/power-shelf-01 \
    b01/server-01 c01/server-01 b02/server-01 c01/server-02 b05/server-01 b03/server-01 b04/server-01 c02/server-01 \
    | tee "$OUT/classify.txt"
  stop_bg twin-classify
  local lr nv
  lr=$(awk -F'|' 'NR>1 && $2 !~ /error/ && $2 !~ /^ *- \/ -/' "$OUT/classify.txt" | wc -l | tr -d ' ')
  nv=$(awk -F'|' 'NR>1 && $3 ~ /\/ ok *$/' "$OUT/classify.txt" | wc -l | tr -d ' ')
  result classify PASS "libredfish classified $lr of 13 BMCs; nv-redfish (the site explorer's default) explored $nv of 13 (out/classify.txt)"
}

# ------------------------------------------------------------------------- NICo's services
vault_token() { cat "$WORK/vault-root-token"; }
step_up() {
  say "start Postgres ($PG :$PG_PORT, TLS), Vault ($VAULT :$VAULT_PORT) and the twin"
  need docker jq openssl curl
  if [ ! -f "$CERTS/client.crt" ]; then
    cp "$SRC/dev/certs/localhost/gen-certs.sh" "$CERTS/"
    (cd "$CERTS" && ./gen-certs.sh >/dev/null 2>&1) || die "gen-certs"
  fi
  # SPIFFE identities: nico-api admits its services, and issues BMC credentials, by them
  local svc
  for svc in nico-hardware-health nico-flow nico-dsx-exchange-consumer nico-pxe; do
    [ -f "$CERTS/$svc.crt" ] && continue
    openssl ecparam -name prime256v1 -genkey -noout -out "$CERTS/$svc.key" 2>/dev/null
    printf '[v3]\nbasicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=clientAuth\nsubjectAltName=URI:spiffe://nico.local/forge-system/sa/%s\n' "$svc" >"$CERTS/$svc.cnf"
    openssl req -new -key "$CERTS/$svc.key" -subj "/CN=$svc" -out "$CERTS/$svc.csr" 2>/dev/null
    openssl x509 -req -in "$CERTS/$svc.csr" -CA "$CERTS/ca.crt" -CAkey "$CERTS/ca.key" -CAcreateserial -days 30 -sha256 \
      -extfile "$CERTS/$svc.cnf" -extensions v3 -out "$CERTS/$svc.crt" 2>/dev/null
    rm -f "$CERTS/$svc.csr" "$CERTS/$svc.cnf" "$CERTS/ca.srl"
  done
  # nico-api's own certificate also names its site address on the service network, where a host
  # booted on the twin's segments reaches it (the Ready path: scout's server_uri)
  if [ ! -f "$CERTS/nico-api-site.crt" ]; then
    openssl ecparam -name prime256v1 -genkey -noout -out "$CERTS/nico-api-site.key" 2>/dev/null
    printf '[v3]\nbasicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth,clientAuth\nsubjectAltName=DNS:localhost,IP:127.0.0.1,IP:10.90.0.20,DNS:carbide-api.forge\n' >"$CERTS/nico-api-site.cnf"
    openssl req -new -key "$CERTS/nico-api-site.key" -subj "/CN=localhost" -out "$CERTS/nico-api-site.csr" 2>/dev/null
    openssl x509 -req -in "$CERTS/nico-api-site.csr" -CA "$CERTS/ca.crt" -CAkey "$CERTS/ca.key" -CAcreateserial -days 30 -sha256 \
      -extfile "$CERTS/nico-api-site.cnf" -extensions v3 -out "$CERTS/nico-api-site.crt" 2>/dev/null
    rm -f "$CERTS/nico-api-site.csr" "$CERTS/nico-api-site.cnf" "$CERTS/ca.srl"
  fi
  docker rm -f "$PG" "$VAULT" >/dev/null 2>&1 || true
  # nico-api connects to Postgres over TLS only (NICo's own script mounts its localhost cert)
  docker create --name "$PG" -e POSTGRES_PASSWORD=admin -e POSTGRES_HOST_AUTH_METHOD=trust \
    -p "127.0.0.1:$PG_PORT:5432" --entrypoint sh docker.io/library/postgres:16-alpine -c \
    'mkdir -p /pgcerts && cp /certs-in/localhost.crt /pgcerts/server.crt && cp /certs-in/localhost.key /pgcerts/server.key && chown -R postgres:postgres /pgcerts && chmod 600 /pgcerts/server.key && exec docker-entrypoint.sh postgres -c ssl=on -c ssl_cert_file=/pgcerts/server.crt -c ssl_key_file=/pgcerts/server.key -c max_connections=300' >/dev/null
  # (a directory copied to a path the container lacks becomes that path, with Docker and Podman)
  docker cp "$CERTS" "$PG:/certs-in" && docker start "$PG" >/dev/null || die "start $PG"
  (umask 077; openssl rand -hex 16 >"$WORK/vault-root-token")
  if [ -n "${VAULT_BINARY:-}" ]; then
    docker create --name "$VAULT" --cap-add=IPC_LOCK -p "127.0.0.1:$VAULT_PORT:8200" docker.io/library/alpine:3.21 \
      /usr/local/bin/vault server -dev -dev-root-token-id="$(vault_token)" -dev-listen-address=0.0.0.0:8200 >/dev/null
    docker cp "$VAULT_BINARY" "$VAULT:/usr/local/bin/vault"
    docker start "$VAULT" >/dev/null
  else
    docker run --detach --name "$VAULT" --cap-add=IPC_LOCK -p "127.0.0.1:$VAULT_PORT:8200" docker.io/hashicorp/vault:1.20.2 \
      server -dev -dev-root-token-id="$(vault_token)" -dev-listen-address=0.0.0.0:8200 >/dev/null
  fi
  wait_for 60 vault curl -fs "http://127.0.0.1:$VAULT_PORT/v1/sys/health" || die "vault"
  # The Ready path's machine certificates: nico-api issues a host's certificate from Vault's PKI
  # when scout registers it, and scout then calls nico-api with it (mTLS), which verifies it against
  # the site CA ([tls] root_cafile_path). NICo's mac-local-dev script gives Vault a root of its own
  # ("myvault.com"), so every scout call after registration was refused (UnknownIssuer) and the
  # host waited in cleanup; as a site's Vault issues under the site's CA, the Ready path's issues
  # under the harness CA.
  local pki_root='vault write certs/root/generate/internal common_name=myvault.com ttl=87600h'
  if netns; then
    cat "$CERTS/ca.crt" "$CERTS/ca.key" >"$WORK/vault-ca-bundle.pem"
    docker cp "$WORK/vault-ca-bundle.pem" "$VAULT:/tmp/ca-bundle.pem" >/dev/null
    pki_root='vault write certs/config/ca pem_bundle=@/tmp/ca-bundle.pem'
  fi
  # the secrets NICo's dev/mac-local-dev/run-nico-api.sh writes
  docker exec -e VAULT_ADDR=http://127.0.0.1:8200 -e VAULT_TOKEN="$(vault_token)" -e SITE="$SITE_PASSWORD" -e PKI_ROOT="$pki_root" "$VAULT" sh -c '
    vault secrets enable -path=secrets -version=2 kv
    for p in machines/bmc/site/root machines/all_dpus/site_default/uefi-metadata-items/auth machines/all_hosts/site_default/uefi-metadata-items/auth; do
      echo "{\"UsernamePassword\": {\"username\": \"root\", \"password\": \"$SITE\" }}" | vault kv put /secrets/$p -
    done
    vault secrets enable -path=certs pki
    eval "$PKI_ROOT"
    vault write certs/roles/role allowed_domains=example.com allow_subdomains=true max_ttl=72h require_cn=false allowed_uri_sans="spiffe://nico.local/*"
  ' >"$LOGS/vault-init.log" 2>&1
  wait_for 60 postgres sh -c "docker logs $PG 2>&1 | grep -q 'PostgreSQL init process complete'" || die "postgres"
  sleep 3
  # the twin: its BMC network is relayed to NICo's dhcp-server on DHCP_PORT, and its BMS mirrors
  # the shapes NICo's consumer and nico-api parse (quoted: a bare comma ends a flow-mapping entry)
  # the Ready path's estate also names nico-api's port, and the routed site network's link
  local est=$ESTATE; ready && est=$READY_ESTATE; trays && est=$TRAYS_ESTATE
  sed -E -e "s|server: \"127.0.0.1:26767\"|server: \"127.0.0.1:$DHCP_PORT\"|" \
      -e "s#bmsConsumerMirror: (\"[^\"]*\"|[^,} ]*)#bmsConsumerMirror: \"$BMS_MIRRORS\"#" \
      -e "s|nico-api@10.90.0.20:11079=127.0.0.1:11079|nico-api@10.90.0.20:$API_PORT=127.0.0.1:$API_PORT|" \
      -e "s|link=127.0.0.1:17800|link=127.0.0.1:$PXE_LINK_PORT|" "$est" >"$WORK/estate.yaml"
  local adapters=redfish,dhcp,bms,nvue
  ready && adapters=redfish,dhcp,bms,vm,ipmi
  trays && adapters=redfish,dhcp,bms,nvue,nmxc,nicoagent
  [ "$COMPONENT_BACKEND" = rms ] && ! ready && adapters=$adapters,rms
  twin_up twin "$WORK/estate.yaml" "$STATE" "$adapters" "$GATEWAY" || die "twin"
  if [ "$COMPONENT_BACKEND" = rms ] && ! ready; then
    jq -r .url "$STATE/rms.json" >"$WORK/rms-url" && jq -r .ca "$STATE/rms.json" >"$WORK/rms-ca" || die "the twin's RMS (rms.json)"
  fi
  jq -r '.endpoints["site-1/networks/oob"]["dhcp-relay-upstream"]' "$STATE/endpoints.json" >"$WORK/relay-upstream"
  jq -r '.endpoints["site-1/facility/bms"].mqtt' "$STATE/endpoints.json" >"$WORK/broker"
  echo "  twin relay upstream $(cat "$WORK/relay-upstream"), DSX Exchange broker $(cat "$WORK/broker"), site gateway $GATEWAY"
  result up PASS "postgres (TLS), vault, twin with $(jq '.endpoints|length' "$STATE/endpoints.json") endpoints"
  if ready; then
    # the host is prepared as a site prepares one before NICo ingests it (step host's firmware)
    twin credentials >"$WORK/credentials.json"
    ready_firmware
  fi
}

render() { # render <template> <out>
  local broker; broker=$(cat "$WORK/broker")
  sed -e "s|@API_PORT@|$API_PORT|g" -e "s|@METRICS_PORT@|$METRICS_PORT|g" -e "s|@PG_PORT@|$PG_PORT|g" \
      -e "s|@WORK@|$WORK|g" -e "s|@EXPLORE_INTERVAL@|${EXPLORE_INTERVAL:-15s}|g" -e "s|@EXPLORE_MODE@|$EXPLORE_MODE|g" \
      -e "s|@DPU_POLICY@|$DPU_POLICY|g" -e "s|@DSX_BUS@|${DSX_BUS:-true}|g" -e "s|@BROKER_PORT@|${broker##*:}|g" \
      -e "s|@CONSUMER_METRICS_PORT@|$CONSUMER_METRICS_PORT|g" -e "s|@HEALTH_METRICS_PORT@|$HEALTH_METRICS_PORT|g" \
      -e "s|@COMPONENT_BACKEND@|$COMPONENT_BACKEND|g" -e "s|@RMS_URL@|$(cat "$WORK/rms-url" 2>/dev/null || echo https://rms.invalid)|g" \
      -e "s|@RMS_CA@|$(cat "$WORK/rms-ca" 2>/dev/null || echo "$WORK/certs/ca.crt")|g" "$1" >"$2"
}
vault_env() {
  echo VAULT_ADDR="http://127.0.0.1:$VAULT_PORT" VAULT_TOKEN="$(vault_token)" VAULT_CACERT="$CERTS/ca.crt" \
    VAULT_KV_MOUNT_LOCATION=secrets VAULT_PKI_MOUNT_LOCATION=certs VAULT_PKI_ROLE_NAME=role
}
proxy_env() { echo HTTPS_PROXY="http://$GATEWAY" https_proxy="http://$GATEWAY" NO_PROXY=localhost,127.0.0.1,::1 no_proxy=localhost,127.0.0.1,::1; }

step_nico() {
  say "start nico-api (site explorer through the twin's site gateway) and NICo Flow"
  mkdir -p "$WORK/firmware" "$WORK/firmware-cache"
  "$BIN/nicoinputs" -estate "$WORK/estate.yaml" -expected "$STATE/stack-inputs/expected-machines.json" -out "$WORK/inputs" | sed 's/^/  /'
  render "$here/nico-api-config.toml.in" "$WORK/nico-api-config.toml"
  cat "$WORK/inputs/nico-site.toml" >>"$WORK/nico-api-config.toml"
  ready && ready_config "$WORK/nico-api-config.toml"
  trays && trays_config "$WORK/nico-api-config.toml"
  local db="postgresql://postgres:admin@127.0.0.1:$PG_PORT"
  DATABASE_URL=$db "$BIN/carbide-api" migrate >"$LOGS/migrate.log" 2>&1 || { tail "$LOGS/migrate.log"; die "migrations"; }
  # HTTPS_PROXY: every BMC address NICo leases is reached through the twin's site gateway (an HTTP
  # CONNECT proxy from a leased site address to that BMC's endpoint). Vault (http) and Postgres
  # are local and never proxied.
  # on the Ready path nico-api runs in the site network's namespace (step pxe), as the user, where
  # its database, Vault and the site gateway are forwarded in at their loopback addresses
  local inns=""
  netns && inns="sudo -n ip netns exec $SVC_NS sudo -n -u $(id -un)"
  start_bg nico-api $inns env RUST_BACKTRACE=1 RUST_LOG="${NICO_RUST_LOG:-info}" $(proxy_env) $(vault_env) \
    CARBIDE_WEB_AUTH_TYPE=none NICO_WEB_AUTH_TYPE=none DATABASE_URL="$db" \
    "$BIN/carbide-api" run --config-path "$WORK/nico-api-config.toml"
  local at="127.0.0.1:$API_PORT"
  netns && at="10.90.0.20:$API_PORT in $SVC_NS (and 127.0.0.1:$API_PORT through svcnet)"
  if wait_for 180 nico-api admin version; then
    result nico-api PASS "listening on $at; $(admin network-segment show | grep -cE '\| (oob|provisioning)-') segments, one per twin rack and network"
  else tail -20 "$LOGS/nico-api.log"; result nico-api FAIL "did not answer (logs/nico-api.log)"; return 1; fi
  netns && return 0  # the Ready and trays paths need no Flow
  # NICo Flow: the policy engine that powers off leaking trays (Temporal runs its tasks)
  docker exec "$PG" psql -U postgres -qc 'create database flow' >/dev/null 2>&1
  start_bg temporal "$BIN/temporal" server start-dev --namespace flow --ip 127.0.0.1 --port "$TEMPORAL_PORT" \
    --http-port $((TEMPORAL_PORT + 1)) --metrics-port $((TEMPORAL_PORT + 2)) --headless
  local fenv="DB_HOST=127.0.0.1 DB_PORT=$PG_PORT DB_USER=postgres DB_PASSWORD=admin DB_NAME=flow"
  env $fenv "$BIN/flow" db migrate >"$LOGS/flow-migrate.log" 2>&1 || { result flow FAIL "migrations"; return; }
  mkdir -p "$WORK/flow-certs"
  cp "$CERTS/ca.crt" "$WORK/flow-certs/ca.crt"; cp "$CERTS/nico-flow.crt" "$WORK/flow-certs/tls.crt"; cp "$CERTS/nico-flow.key" "$WORK/flow-certs/tls.key"
  printf 'component_managers:\n  compute: nicolegacy\n  nvswitch: nico\n  powershelf: nico\nproviders:\n  nico:\n    timeout: "1m"\n' >"$WORK/flow-components.yaml"
  printf 'inventory_run_frequency: 30s\nleak_detection_interval: 15s\n' >"$WORK/flow-config.yaml"
  wait_for 60 temporal grep -q 'Temporal Server' "$LOGS/temporal.log"
  sleep 3
  start_bg flow env $fenv TEMPORAL_HOST=127.0.0.1 TEMPORAL_PORT="$TEMPORAL_PORT" TEMPORAL_NAMESPACE=flow \
    NICO_CORE_API_URL="localhost:$API_PORT" CERTDIR="$WORK/flow-certs" COMPONENT_MANAGER_CONFIG="$WORK/flow-components.yaml" \
    FLOW_CONFIG_FILE="$WORK/flow-config.yaml" FLOW_AUTHORIZATION_MODE=AUDIT \
    "$BIN/flow" serve --dev-mode --listen-port "$FLOW_PORT" --metrics-port $((FLOW_PORT + 1))
  if wait_for 60 flow grep -q 'gRPC server is running' "$LOGS/flow.log"; then result flow PASS "NICo Flow up; leak-detection job every 15 s"
  else result flow FAIL "did not start (logs/flow.log)"; fi
}

step_load() {
  say "load the expected racks, machines, power shelves and switches (admin CLI), as an operator would"
  local i=$WORK/inputs r
  admin expected-rack-group replace-all --filename "$i/expected-rack-groups.json" >>"$LOGS/load.log"
  for r in $(jq -r '(.expected_racks // [])[].rack_id' "$i/expected-racks.json"); do
    admin expected-rack add "$r" --meta-name "$r" >>"$LOGS/load.log"
  done
  admin expected-machine replace-all --filename "$i/expected-machines.json" >>"$LOGS/load.log"
  admin expected-power-shelf replace-all --filename "$i/expected-power-shelves.json" >>"$LOGS/load.log"
  admin expected-switch replace-all --filename "$i/expected-switches.json" >>"$LOGS/load.log"
  admin expected-machine show >"$OUT/expected-machines.txt"
  local n racks
  n=$(grep -c 'dc-twin site-1' "$OUT/expected-machines.txt" || true)
  racks=$(admin expected-rack show | grep -c '_NVIDIA' || true)
  local shelves switches
  shelves=$(jq '.expected_power_shelves | length' "$i/expected-power-shelves.json")
  switches=$(jq '.expected_switches | length' "$i/expected-switches.json")
  if [ "$n" -gt 0 ]; then result load PASS "$n expected machines, $racks expected NVL rack, $shelves shelves, $switches switches (with NVOS MACs)"
  else result load FAIL "expected machines not loaded (logs/load.log)"; fi
  # the Ready path's TPM: the vm host's EK certificates are signed by the estate CA, its TPM
  # vendor's CA as far as NICo is concerned, which an operator loads as NICo loads a vendor's (the
  # trays path's impersonated scout reports EK certificates the estate CA signs too)
  if netns; then
    openssl x509 -in "$STATE/estate-ca.pem" -outform der -out "$WORK/estate-ca.der" &&
      admin tpm-ca add --filename "$WORK/estate-ca.der" >>"$LOGS/load.log" &&
      admin tpm-ca show >"$OUT/tpm-ca.txt" && grep -q 'dc-twin' "$OUT/tpm-ca.txt" &&
      result tpm-ca PASS "the estate CA, which signed the vm host's EK certificates, loaded as a TPM CA (tpm-ca add)" ||
      result tpm-ca FAIL "tpm-ca add of the estate CA (logs/load.log)"
  fi
}

leased() { twin report 2>/dev/null | grep -Eo '[0-9]+ of [0-9]+ BMC ports leased' | head -1; }
all_leased() { local l; l=$(leased); [ -n "$l" ] && [ "${l%% *}" = "$(echo "$l" | awk '{print $3}')" ]; }
step_dhcp() {
  say "start NICo's dhcp-server (controller mode) behind the twin relay"
  render "$here/dhcp-server.yaml.in" "$WORK/dhcp-server.yaml"
  local up iface=lo; up=$(cat "$WORK/relay-upstream"); [ "$uname_s" = Darwin ] && iface=lo0
  # The twin relay sends from one upstream socket and carries its port in RFC 8357's relay
  # source port sub-option; NICo's server answers giaddr at a fixed --relay-response-port.
  start_bg dhcp-server env RUST_LOG="${NICO_RUST_LOG:-info}" "$BIN/forge-dhcp-server" --mode controller \
    --interfaces "$iface" --listen-addr "127.0.0.1:$DHCP_PORT" --relay-response-port "${up##*:}" \
    --dhcp-config "$WORK/dhcp-server.yaml" --forge-root-ca-path "$CERTS/ca.crt" \
    --client-cert-path "$CERTS/client.crt" --client-key-path "$CERTS/client.key" --metrics-listen-addr "127.0.0.1:$DHCP_METRICS_PORT"
  wait_for "${LEASE_WAIT:-300}" "every BMC to lease from NICo" all_leased
  twin credentials >"$WORK/credentials.json"
  local l; l=$(leased)
  if all_leased; then result dhcp PASS "$l by NICo's forge-dhcp-server through the twin relay (option 82, link selection)"
  else result dhcp FAIL "${l:-no leases}"; fi
  if ready; then ready_ipmi; fi
}

report_json() { admin -f json site-explorer get-report all >"$OUT/site-explorer-report.json"; }
machine_count() { admin machine show | grep -c 'fm100' || true; }
step_discover() {
  say "site explorer: explored endpoints, hardware types, credential rotation, machines"
  local end=$((SECONDS + ${EXPLORE_WAIT:-900})) mh=0
  while [ $SECONDS -lt $end ]; do
    report_json
    mh=$(jq '.managed_hosts | length' "$OUT/site-explorer-report.json" 2>/dev/null || echo 0)
    [ "${mh:-0}" -gt 0 ] && [ "$(machine_count)" -gt 0 ] && break
    sleep 20
  done
  sleep 30; report_json
  python3 -I "$here/nico-report.py" "$OUT/site-explorer-report.json" "$WORK/credentials.json" "$STATE/stack-inputs/expected-machines.json" >"$OUT/explored-vs-twin.txt"
  admin site-explorer get-report endpoint >"$OUT/explored-endpoints.txt"
  admin site-explorer get-report managed-host >"$OUT/managed-hosts.txt"
  admin machine show >"$OUT/machines.txt"
  admin power-shelf show >"$OUT/power-shelves.txt"
  admin switch show >"$OUT/switches.txt"
  admin rack show >"$OUT/racks.txt"
  python3 -I "$here/twin-requests.py" "$STATE" --writes >"$OUT/twin-writes.txt"
  local ok match total rot machines nongb gb
  ok=$(grep -c ' ok$' "$OUT/explored-vs-twin.txt" || true); total=$(grep -cE '^[a-z][0-9]+/' "$OUT/explored-vs-twin.txt" || true)
  match=$(awk '$6=="yes"' "$OUT/explored-vs-twin.txt" | wc -l | tr -d ' ')
  nongb=$(awk '$NF=="ok" && $1 !~ /compute-tray-[0-9]+$/' "$OUT/explored-vs-twin.txt" | wc -l | tr -d ' ')
  gb=$(awk '$NF=="ok" && $1 ~ /compute-tray-[0-9]+$/ && $6=="no"' "$OUT/explored-vs-twin.txt" | wc -l | tr -d ' ')
  rot=$(awk '$1=="account.password" {print $2}' "$OUT/twin-writes.txt"); rot=${rot:-0}
  machines=$(machine_count)
  if [ "$ok" -gt 0 ] && [ "$match" -ge "$nongb" ]; then result discover PASS "$ok of $total leased BMCs explored (10 PDUs refused as non-BMCs); serial = the twin's on all $nongb non-GB BMCs (out/explored-vs-twin.txt)"
  else result discover FAIL "$ok explored, $match of $nongb non-GB serials match"; fi
  [ "$gb" -gt 0 ] && result discover-gb-serial KNOWN "$gb GB300 trays report the HGX baseboard's serial, not the tray's (libredfish fallback; README)"
  if [ "$rot" -gt 0 ]; then result credential-rotation PASS "$rot BMC root passwords rotated to the site password (account.password on the twin)"
  else result credential-rotation FAIL "no account.password requests"; fi
  if [ "$machines" -gt 0 ]; then result machines PASS "$mh managed hosts identified, $machines machines created (out/machines.txt)"
  else result machines FAIL "$mh managed hosts identified, no machine created"; fi
  # GB300 trays NICo created as managed hosts, each paired with its DPU (dpu_policy manage)
  local gbm gbx states
  gbx=$(awk '$NF=="ok" && $1 ~ /compute-tray-[0-9]+$/' "$OUT/explored-vs-twin.txt" | wc -l | tr -d ' ')
  gbm=$(grep -oE 'dc-twin\.path:[^"]*/compute-tray-[0-9]+"' "$OUT/machines.txt" | sort -u | wc -l | tr -d ' ')
  states=$(awk -F'|' '$3 ~ /fm100p/ {gsub(/ /,"",$4); print $4}' "$OUT/machines.txt" | sort | uniq -c | awk '{printf "%s%s %s", (NR>1?", ":""), $1, $2}')
  if [ "$gbx" -gt 0 ] && [ "$gbm" -ge "$gbx" ]; then
    result machines-gb-trays PASS "$gbm of $gbx GB300 trays are managed hosts paired with their DPU (${states:-no host states})"
  elif grep -q 'serial number missing' "$LOGS/nico-api.log"; then
    result machines-gb-trays KNOWN "$gbm of $gbx GB300 trays created; the rest: 'serial number missing (product, board and chassis)' (README)"
  fi
  return 0
}

step_health() {
  say "start NICo's hardware-health service (sensors and in-tray leak detectors)"
  render "$here/hw-health.toml.in" "$WORK/hw-health.toml"
  start_bg hw-health env RUST_LOG="${NICO_RUST_LOG:-info}" $(proxy_env) "$BIN/forge-hw-health" "$WORK/hw-health.toml"
  if wait_for 180 "leak detectors polled" grep -q 'report_source=bmc-leak-detectors success_count=[1-9]' "$LOGS/hw-health.log"; then
    result health PASS "forge-hw-health polls $(grep 'Discovered and sharded BMC endpoints' "$LOGS/hw-health.log" | tail -1 | grep -o 'endpoint_count=[0-9]*' | cut -d= -f2) BMCs' sensors and leak detectors (the NVSwitch trays and power shelves NICo created)"
  else result health FAIL "no leak-detector readings (logs/hw-health.log)"; fi
}

# Each check captures its command's output before matching it: under pipefail a `| grep -q`
# that stops reading early fails the whole pipeline.
switch_id() { local o; o=$(admin switch show); grep "a03-$1 " <<<"$o" | head -1 | awk -F'|' '{gsub(/ /,"",$2); print $2}'; }
has_tray_leak() { local o; o=$(admin switch health-report show "$1"); grep -q TrayLeak <<<"$o"; }
rack_alert() { local o; o=$(admin rack health-history a03); head -5 <<<"$o" | grep -q "Alerting | $1"; }
endpoint_error() { local o; o=$(admin site-explorer get-report endpoint "$1"); grep -q "$2" <<<"$o"; }
# rms_powered_off <since> <device> [show]: a power-off request over RMS took the device Off
rms_powered_off() {
  local o; o=$(twin events --since "$1" --no-reads --select "$2")
  grep -qE 'power\.off\.[a-z]+ rms ' <<<"$o" && grep -q 'power: On → Off' <<<"$o" || return 1
  [ "${3:-}" = show ] && grep -oE -m1 'power\.off\.[a-z]+ rms [A-Za-z]+' <<<"$o"
  return 0
}
powered_on_by_nico() { local o; o=$(twin events --since "$1" --no-reads --select "$2"); grep -q 'request stack .* power.on' <<<"$o"; }
# a host (server or compute tray) NICo explored without error but holds no machine for, other than
# the one the unreachable-BMC fault uses
explored_not_ingested() {
  local p
  admin machine show >"$WORK/machines-now.txt" 2>/dev/null || cp "$OUT/machines.txt" "$WORK/machines-now.txt"
  for p in $(awk '$NF=="ok" && ($1 ~ /server-[0-9]+$/ || $1 ~ /compute-tray-[0-9]+$/) {print $1}' "$OUT/explored-vs-twin.txt"); do
    [ "$p" = b04/server-02 ] && continue
    grep -q "dc-twin.path:[^\"]*/$p\"" "$WORK/machines-now.txt" || { echo "$p"; return; }
  done
}
# nico-api's own DSX Exchange integration, through the BMS's nico-api mirror (README, twin gap 4)
nico_api_mirror() { [[ ",$BMS_MIRRORS," == *",nico-api,"* ]]; }
# nico-api publishes HeartbeatTimestampIntegration every 5 s once it has parsed the BMS's metadata
# for it; the twin's report counts what arrived
nico_api_heartbeats() { local o; o=$(twin report 2>/dev/null); grep -Eo '[0-9]+ values received on BMS/v1/[^ ,;]*HeartbeatTimestampIntegration[^ ,;]*' <<<"$o" | head -1; }
nico_api_beating() { local h; h=$(nico_api_heartbeats); [ -n "$h" ] && [ "${h%% *}" -gt 0 ]; }
# nico_api_requested <seq> <pointType>: nico-api published a03's request on the mirror's topic
nico_api_requested() { local o; o=$(twin events --since "$1" --no-reads); grep -q "request stack .*PUBLISH BMS/v1/[^ ]*/Rack/$2/[^ ]*/a03/[^ ]*/nico-api" <<<"$o"; }
# a03_isolation <seq> <electrical|liquid> <true|false>: since <seq>, a03's isolation went to that
a03_isolation() { local o from=true; [ "$3" = true ] && from=false; o=$(twin events --since "$1" --no-reads); grep -q "a03  isolation\.$2: $from → $3" <<<"$o"; }
step_faults() {
  say "3. faults: a BMC that stops answering, a power-off, two tray leaks and the rack isolation they lead to"
  local s sw3 hpe
  # a BMC that stops answering
  hpe=$(jq -r '.[] | select(.path|endswith("b04/server-02")) | .siteIP' "$WORK/credentials.json")
  twin fault inject b04/server-02 generic/bmc/unreachable --id bmc-down >/dev/null
  if wait_for 180 "endpoint error" endpoint_error "$hpe" SITEEXPLORER-102; then
    result bmc-unreachable PASS "b04/server-02 ($hpe): NICO-SITEEXPLORER-102, the endpoint was not reachable"
  else result bmc-unreachable FAIL "no endpoint error for $hpe"; fi
  twin fault clear bmc-down >/dev/null
  # a power-off of a host NICo has explored but not ingested: its site explorer powers such a host
  # back on (an ingested one is its state machine's to drive). Before the leaks: once NICo has the
  # BMS isolate a03, a03 has no power.
  local off; off=$(explored_not_ingested)
  if [ -z "$off" ]; then result power-off KNOWN "every explored host is ingested; NICo's site explorer powers on only hosts it has not ingested"
  else
    s=$(last_seq)
    twin power off "$off" >/dev/null
    if wait_for 180 "NICo to power it on" powered_on_by_nico "$s" "$off"; then
      result power-off PASS "$off off on the twin; NICo's site explorer powered it on ($(twin events --since "$s" --no-reads --select "$off" | grep 'power.on' | head -1 | awk '{print $6, $8, $9}'))"
    else result power-off FAIL "$off: no power.on from NICo"; fi
  fi
  # two tray leaks
  sw3=$(switch_id switch-tray-03)
  local leak_s; leak_s=$(last_seq)
  twin fault inject a03/switch-tray-03 generic/leak/tray-detector --id leak-sw3 >/dev/null
  if wait_for 180 "NICo's TrayLeak report" has_tray_leak "$sw3"; then
    result tray-leak PASS "switch-tray-03: TrayLeak, SensorCritical, PreventAllocations, Hardware (hardware-health.tray-leak-detection)"
  else result tray-leak FAIL "no TrayLeak report on $sw3"; fi
  if wait_for 180 "Flow's power-off" grep -q 'Power-off task submitted for leaking component' "$LOGS/flow.log"; then
    # with COMPONENT_BACKEND=rms the power-off reaches the twin: NICo core -> the twin's RMS
    if [ "$COMPONENT_BACKEND" = rms ] && wait_for 120 "the switch tray off" rms_powered_off "$leak_s" a03/switch-tray-03; then
      result tray-leak-flow PASS "Flow -> NICo core -> the twin's RMS ($(rms_powered_off "$leak_s" a03/switch-tray-03 show)): switch-tray-03 powered off"
    else
      sleep 20
      local why; why=$(grep -o 'power control failed for [^:]*: [A-Za-z ]*' "$LOGS/flow.log" | head -1 | sed 's/.*: //')
      if [ "$COMPONENT_BACKEND" = rms ]; then result tray-leak-flow FAIL "Flow submitted a power-off; the twin's RMS powered nothing off${why:+ (NICo core: \"$why\")}"
      else result tray-leak-flow PASS "Flow submitted a force power-off for the leaking switch tray (backend mock: not sent to the twin)${why:+ (NICo core: \"$why\")}"; fi
    fi
  else result tray-leak-flow FAIL "no power-off task (logs/flow.log)"; fi
  s=$(last_seq)
  twin fault inject a03/switch-tray-05 generic/leak/tray-detector --id leak-sw5 >/dev/null
  if wait_for 180 "rack leak" rack_alert BmcLeakDetection; then
    result rack-leak-from-trays PASS "two leaking trays: rack a03 Alerting (RackLeak at the threshold of 2)"
  else result rack-leak-from-trays FAIL "rack a03 not alerting"; fi
  # at the threshold nico-api asks the BMS over DSX Exchange to isolate a03, electrically and
  # liquid, on the topics the nico-api mirror's metadata named; the twin's BMS opens a03's power
  # and closes its liquid supply after the dialect's delay, and publishes the isolation status
  if nico_api_mirror; then
    if nico_api_beating; then result nico-api-bms PASS "nico-api reads the BMS's metadata: $(nico_api_heartbeats) (its HeartbeatTimestampIntegration, every 5 s)"
    else result nico-api-bms FAIL "no HeartbeatTimestampIntegration from nico-api on the twin's BMS (dctwin report)"; fi
    if wait_for 120 "nico-api's isolation request" nico_api_requested "$s" RackElectricalIsolationRequest &&
      wait_for 60 "a03's electrical isolation" a03_isolation "$s" electrical true; then
      local liq=""; nico_api_requested "$s" RackLiquidIsolationRequest && a03_isolation "$s" liquid true && liq=" and closed its liquid supply"
      result nico-api-isolation PASS "two leaking trays → nico-api asked the BMS to isolate a03 (RackElectricalIsolationRequest=1 on the nico-api mirror's topic) → the twin's BMS opened a03's power${liq}"
    else result nico-api-isolation FAIL "no isolation of a03 requested by nico-api on the nico-api mirror's topics (dctwin events)"; fi
  elif grep -q 'Failed to parse BMS metadata' "$LOGS/nico-api.log"; then
    result nico-api-bms KNOWN "BMS_MIRRORS has no nico-api: nico-api's own DSX Exchange integration rejects the twin's AsyncAPI metadata (README)"
  fi
  s=$(last_seq)
  twin fault clear leak-sw3 >/dev/null; twin fault clear leak-sw5 >/dev/null
  if nico_api_mirror && a03_isolation 0 electrical true; then
    if wait_for 180 "a03's release" a03_isolation "$s" electrical false; then
      result nico-api-release PASS "tray leaks cleared → nico-api's RackElectricalIsolationRequest=0 → the twin's BMS closed a03's power again"
    else result nico-api-release KNOWN "a03 still isolated 180 s after its tray leaks were cleared on the twin: nico-api sent no RackElectricalIsolationRequest=0 (README)"; fi
  fi
}

step_consumer() {
  say "4. NICo's dsx-exchange-consumer on the twin's BMS broker (nico-legacy mirror)"
  render "$here/dsx-exchange-consumer.toml.in" "$WORK/dsx-consumer.toml"
  start_bg dsx-consumer env RUST_LOG="${NICO_RUST_LOG:-info}" $(vault_env) "$BIN/carbide-dsx-exchange-consumer" "$WORK/dsx-consumer.toml"
  wait_for 60 consumer grep -q 'MQTT consumer connected' "$LOGS/dsx-consumer.log" || { result consumer-rack-leak FAIL "not connected"; return; }
  sleep 5
  twin fault inject a03/leak-point-01 generic/leak/rack-point --id rack-leak >/dev/null
  if wait_for 180 "the consumer's alert" grep -q 'Leak alert detected.*leak-point-01' "$LOGS/dsx-consumer.log"; then
    # NICo's consumer keeps one override per rack and removes it on any point's Clear it has not
    # yet recorded, including the periodic re-publication of a sibling point (README), so the
    # alert lasts until the next such Clear: look for it in the rack's health history meanwhile.
    if wait_for 100 "rack alert" rack_alert BmsLeakDetectRack; then
      result consumer-rack-leak PASS "BMS LeakDetectRack on a03 → consumer → rack a03 Alerting BmsLeakDetectRack"
    else
      result consumer-rack-leak PASS "BMS LeakDetectRack on a03 → consumer inserted the rack a03 override (removed again before the rack's health history recorded it, see consumer-sibling-clear)"
    fi
  else result consumer-rack-leak FAIL "the consumer saw no leak (logs/dsx-consumer.log)"; fi
  local after; after=$(grep -A50 'Leak alert detected.*leak-point-01' "$LOGS/dsx-consumer.log")
  if [ -n "$(grep 'Leak cleared' <<<"$after" | grep -v 'LeakDetectRack/site-1/hall-a/a03/leak-point-01/')" ]; then
    result consumer-sibling-clear KNOWN "a Clear re-published for another point of rack a03 removed the leak override while leak-point-01 was still leaking (NICo consumer, README)"
  fi
  twin fault clear rack-leak >/dev/null
  if wait_for 60 clear grep -q 'Leak cleared.*LeakDetectRack/site-1/hall-a/a03/leak-point-01/' "$LOGS/dsx-consumer.log"; then
    result consumer-clear PASS "cleared → the consumer removed the rack health override"
  else result consumer-clear FAIL "no removal"; fi
}

# ---------------------------------------------------------------------------- the Ready path
# The DGX H100 of examples/estates/dgx-nico-ready.yaml is a real x86_64 VM (`vm`, `os: stack`,
# KVM) whose NICs are on the twin's segments (D48): its firmware's DHCP is relayed to NICo's
# dhcp-server, it HTTP-boots NICo's iPXE from NICo's PXE service, which serves it NICo's scout
# (README: what the boot image is), and scout reports to nico-api, which drives the host through
# its state machine to Ready. NICo's PXE service and nico-api run in a Linux network namespace
# (SVC_NS) the twin routes the guest into at L3, because both identify a host by its packets'
# source address: the namespace's TAP is svcnet's, at the far end of the twin's routed site network
# link; nothing is added to the host's interfaces, routes or firewall. Linux only (sudo for the
# namespace).
ready() { [ "${READY:-}" = 1 ]; }
trays() { [ "${TRAYS:-}" = 1 ]; }
netns() { ready || trays; }  # NICo's PXE service and API run in the site network's namespace

# trays_config <config>: the trays path's changes to nico-api's configuration. As on the Ready
# path, nico-api listens at its site address in the namespace with its site certificate, and a
# failed boot is retried after 15 minutes. Unlike it, the switches and shelves stay on the twin's
# RMS (forwarded into the namespace, step pxe), and NVLink partitioning is on: nvlink-manager
# reaches the twin's NMX-C at each switch tray's NVOS address on 9370, verified against the
# estate CA, which signs NMX-C's certificates.
trays_config() {
  local f=$1
  sed -i.bak -e "s|^listen = \"127.0.0.1:$API_PORT\"|listen = \"10.90.0.20:$API_PORT\"|" \
    -e "s|certs/localhost.crt|certs/nico-api-site.crt|" -e "s|certs/localhost.key|certs/nico-api-site.key|" "$f" && rm -f "$f.bak"
  awk '{print} /^\[machine_state_controller\]$/ {print "failure_retry_time = \"15m\""}' "$f" >"$f.tmp" && mv "$f.tmp" "$f"
  printf '\n[nvlink_config]\nenabled = true\nallow_insecure = false\nnmx_c_tls_ca_cert_path = "%s"\n' "$STATE/estate-ca.pem" >>"$f"
  # The BlueField bundle NICo's DPU image installs carries the DPUs' BMC, ERoT and NIC firmware,
  # and an operator keeps nico-api's dpu_models catalog in step with it (NICo's
  # docs/operations/firmware-updates/configuration.md). On this path the image is impersonated
  # and installs nothing, so the bundle's firmware is what the estate's BlueFields run: the
  # catalog names the versions the twin's BlueField BMCs serve (read from the first one), and
  # NICo's DPU firmware check (DPUInitializing/WaitingForPlatformConfiguration) finds them in place.
  local dpu v_bmc v_cec v_nic v_uefi
  dpu=$(jq -r '[.[] | select(.path | test("/bf3-[0-9]+$")) | .path][0] // empty' "$WORK/credentials.json" 2>/dev/null)
  [ -n "$dpu" ] || dpu=$(twin get '?class=dpu' --json 2>/dev/null | jq -r '.[0].path // empty')
  fwv() { twin inspect "$dpu" "/redfish/v1/UpdateService/FirmwareInventory/$1" 2>/dev/null | jq -r '.Version // empty'; }
  v_bmc=$(fwv BMC_Firmware) v_cec=$(fwv Bluefield_FW_ERoT) v_nic=$(fwv DPU_NIC) v_uefi=$(fwv DPU_UEFI)
  if [ -n "$v_bmc" ] && [ -n "$v_nic" ]; then
    {
      printf '\n[dpu_config]\ndpu_nic_firmware_update_versions = ["%s"]\n' "$v_nic"
      printf '\n[dpu_config.dpu_models.bluefield3]\nvendor = "Nvidia"\nmodel = "Bluefield 3 SmartNIC Main Card"\nordering = ["bmc", "cec"]\n'
      local c id v
      for c in bmc:BMC_Firmware:$v_bmc cec:Bluefield_FW_ERoT:$v_cec nic:DPU_NIC:$v_nic uefi:DPU_UEFI:$v_uefi; do
        IFS=: read -r c id v <<<"$c"
        [ -n "$v" ] || continue
        printf '\n[dpu_config.dpu_models.bluefield3.components.%s]\ncurrent_version_reported_as = "%s"\n\n[[dpu_config.dpu_models.bluefield3.components.%s.known_firmware]]\nversion = "%s"\ndefault = true\n' "$c" "$id" "$c" "$v"
      done
    } >>"$f"
    echo "  dpu_models.bluefield3: the twin's BlueField firmware (bmc $v_bmc, cec $v_cec, nic $v_nic, uefi $v_uefi) as the bundle's"
  else
    echo "  dpu_models: could not read the twin's BlueField firmware from $dpu; NICo keeps its own baseline"
  fi
}

# ready_config <config>: the Ready path's changes to nico-api's configuration
ready_config() {
  local f=$1
  # nico-api listens at its site address in the namespace (svcnet exposes it on this host's
  # loopback for the dhcp-server and the admin CLI); retry pacing at test speed: a failed boot is
  # retried after 15 minutes, not 90; no switch or shelf on the estate, so NICo's own mock backend;
  # and no rack manager: a DGX H100 rack has no RMS, and with an RMS URL configured NICo's site
  # explorer requires a rack profile of every expected machine's rack to look up its slot and
  # tray, and creates no host without one (run 1: "rack b01 has no rack_profile_id for RMS slot
  # and tray lookup"); an empty URL is NICo's way of saying there is none
  sed -i.bak -e "s|^listen = \"127.0.0.1:$API_PORT\"|listen = \"10.90.0.20:$API_PORT\"|" \
    -e "s|certs/localhost.crt|certs/nico-api-site.crt|" -e "s|certs/localhost.key|certs/nico-api-site.key|" \
    -e 's|^nv_switch_backend = .*|nv_switch_backend = "mock"|' -e 's|^power_shelf_backend = .*|power_shelf_backend = "mock"|' \
    -e '/^\[rms\]$/,/^\[/ s|^api_url = .*|api_url = ""|' "$f" && rm -f "$f.bak"
  awk '{print} /^\[machine_state_controller\]$/ {print "failure_retry_time = \"15m\""}' "$f" >"$f.tmp" && mv "$f.tmp" "$f"
}

# ready_ipmi: the host's BMC over IPMI at its lease, in the namespace. NICo restarts a DGX H100
# through IPMI rather than Redfish (`ipmitool -I lanplus -C 17 … chassis power reset` at the BMC's
# address, port 623: needs_ipmi_restart in crates/machine-controller/src/handler.rs), and a UDP
# client goes through no proxy, so a second svcnet puts the BMC's lease on the namespace's loopback
# and relays its port 623 to the twin's IPMI endpoint for that BMC, as the site gateway carries the
# BMC's HTTPS. Without it (the second run, 2026-10-20) NICo's every restart of the host failed with
# "Could not open socket!" and the host stayed in WaitingForPlatformConfiguration.
ready_ipmi() {
  local ip ep user pass p ok=""
  ip=$(jq -r --arg d "$(host_path)" '.[] | select(.path==$d) | .siteIP // empty' "$WORK/credentials.json")
  ep=$(jq -r --arg d "$(host_path)" '.endpoints[$d].ipmi // empty' "$STATE/endpoints.json")
  user=$(jq -r --arg d "$(host_path)" '.[] | select(.path==$d) | .user' "$WORK/credentials.json")
  pass=$(jq -r --arg d "$(host_path)" '.[] | select(.path==$d) | .password' "$WORK/credentials.json")
  if [ -z "$ip" ] || [ -z "$ep" ]; then result ipmi FAIL "no lease or no IPMI endpoint for $(host_path)"; return 1; fi
  start_bg ipmi-relay sudo -n "$BOOT/svcnet" -netns "$SVC_NS" -tap "" -lo "$ip/32" -udp "$ip:623=$ep"
  wait_for 30 "the IPMI relay" grep -q 'relaying udp' "$LOGS/ipmi-relay.log" || { result ipmi FAIL "svcnet's IPMI relay did not start (logs/ipmi-relay.log)"; return 1; }
  for p in "$SITE_PASSWORD" "$pass"; do
    ok=$(sudo -n ip netns exec "$SVC_NS" sudo -n -u "$(id -un)" ipmitool -I lanplus -C 17 -H "$ip" -U "$user" -P "$p" chassis power status 2>/dev/null) && break
  done
  if [ -n "$ok" ]; then result ipmi PASS "the DGX's BMC answers IPMI (lanplus, cipher suite 17) at its lease $ip:623 in $SVC_NS, where NICo's ipmitool reaches it: \"$ok\""
  else result ipmi FAIL "no IPMI answer at $ip:623 in $SVC_NS (logs/ipmi-relay.log)"; fi
}

step_boot() {
  say "NICo's boot artifacts and Linux programs (boot-artifacts.sh)"
  [ "$uname_s" = Linux ] || die "the Ready and trays paths run on Linux (README: on macOS no native process sees a guest's own address)"
  local b missing=""
  # the trays path's machines run impersonated software (D55): no scout image, no forge-scout
  local want="ipxe.efi scout.efi carbide forge-scout carbide-api forge-dhcp-server nico-admin-cli"
  local pkgs="carbide-api carbide-dhcp-server nico-admin-cli carbide-pxe carbide-scout" bins="carbide-api forge-dhcp-server nico-admin-cli carbide forge-scout" steps="up ipxe rust image down"
  if trays; then
    want="ipxe.efi carbide carbide-api forge-dhcp-server nico-admin-cli"
    pkgs="carbide-api carbide-dhcp-server nico-admin-cli carbide-pxe" bins="carbide-api forge-dhcp-server nico-admin-cli carbide" steps="up ipxe rust down"
  fi
  for b in $want; do [ -s "$BOOT/$b" ] || missing="$missing $b"; done
  if [ -n "$missing" ]; then
    echo "  building:$missing"
    NICO_SRC=$SRC OUT=$BOOT WORK=$WORK JOBS=${JOBS:-4} RUST_PACKAGES="$pkgs" RUST_BINS="$bins" \
      "$here/boot-artifacts.sh" $steps || { result boot FAIL "boot-artifacts.sh"; return 1; }
  else echo "  present in $BOOT"; fi
  (cd "$repo" && GOOS=linux GOARCH=$goarch CGO_ENABLED=0 go build -o "$BOOT/svcnet" ./compat/nico/svcnet) || die "go build svcnet"
  if trays; then
    result boot PASS "NICo's iPXE ($(wc -c <"$BOOT/ipxe.efi" | tr -d ' ') B, its embedded script), carbide-pxe, nico-api, dhcp-server, admin CLI for linux/$goarch at ${NICO_SHA:0:8}"
    return
  fi
  result boot PASS "NICo's iPXE ($(wc -c <"$BOOT/ipxe.efi" | tr -d ' ') B), scout.efi ($(($(wc -c <"$BOOT/scout.efi") / 1048576)) MiB: forge-scout and NICo's scout units, Ubuntu noble, $(cat "$BOOT/kernel" 2>/dev/null || echo "a 4K-page kernel")), carbide-pxe, nico-api, dhcp-server, admin CLI for linux/$goarch at ${NICO_SHA:0:8}"
}

# the namespace, svcnet in it, and NICo's PXE service at 10.90.0.17 (ports 80 and 8080, as NICo's
# deployment exposes it: its embedded iPXE script fetches from port 80, its DHCP hands out 8080)
step_pxe() {
  say "the site network's namespace $SVC_NS: svcnet, NICo's PXE service at 10.90.0.17 (nico-api comes at 10.90.0.20)"
  [ "$uname_s" = Linux ] || die "the Ready and trays paths run on Linux"
  stop_bg pxe-80; stop_bg pxe-8080; stop_bg svcnet
  sudo -n ip netns del "$SVC_NS" 2>/dev/null || true
  sudo -n ip netns add "$SVC_NS" || die "ip netns add $SVC_NS (needs sudo)"
  # a service in the namespace may bind port 80 as the user (this namespace's sysctl only)
  sudo -n ip netns exec "$SVC_NS" sysctl -qw net.ipv4.ip_unprivileged_port_start=0
  local broker; broker=$(cat "$WORK/broker" 2>/dev/null)
  local fw="-forward 127.0.0.1:$PG_PORT=127.0.0.1:$PG_PORT -forward 127.0.0.1:$VAULT_PORT=127.0.0.1:$VAULT_PORT -forward $GATEWAY=$GATEWAY"
  [ -n "$broker" ] && fw="$fw -forward $broker=$broker"
  # the trays path: the DPUs' out-of-band addresses (the BMC network) and the trays' admin
  # addresses are routed in too, and NICo's RMS backend is the twin's RMS on this host
  local routes="-route 10.40.0.0/16"
  if trays; then
    routes="$routes -route 10.30.0.0/16 -route 192.168.252.0/24"
    local rms; rms=$(sed -E 's|^https?://||; s|/.*$||' "$WORK/rms-url" 2>/dev/null)
    [ -n "$rms" ] && fw="$fw -forward $rms=$rms"
  fi
  start_bg svcnet sudo -n "$BOOT/svcnet" -netns "$SVC_NS" -tap svc0 -addr 10.90.0.17/24 -addr 10.90.0.20/24 -gateway 10.90.0.1 \
    $routes -listen "127.0.0.1:$PXE_LINK_PORT" $fw -expose "127.0.0.1:$API_PORT=10.90.0.20:$API_PORT"
  wait_for 60 "svcnet" grep -q 'svcnet: uplink from' "$LOGS/svcnet.log" || { tail "$LOGS/svcnet.log"; result pxe FAIL "the twin's routed site network did not link (logs/svcnet.log)"; return 1; }
  local static=$WORK/forge-boot-artifacts
  rm -rf "$static" && mkdir -p "$static/blobs/internal/$arch"
  if trays; then
    # NICo's iPXE, which the impersonated firmware fetches and whose embedded script it runs; the
    # images NICo's boot scripts name (its DPU image and scout) run nowhere on this path, so a
    # labelled stand-in file is served where each would be, for the impersonator to fetch (D55)
    # (the GB300 trays and their DPUs are aarch64: IPXE_AARCH64 names NICo's aarch64 iPXE when
    # this host is not, boot-artifacts.sh building it on an aarch64 Docker host)
    cp "$BOOT/ipxe.efi" "$static/blobs/internal/$arch/"
    if [ "$arch" != aarch64 ] && [ -n "${IPXE_AARCH64:-}" ]; then
      mkdir -p "$static/blobs/internal/aarch64" && cp "$IPXE_AARCH64" "$static/blobs/internal/aarch64/ipxe.efi"
    fi
    local f d
    for d in "$static"/blobs/internal/*; do
      for f in carbide.efi carbide.root scout.efi; do
        echo "dc-twin: NICo's $f is impersonated on this path (D55); this file stands where it would be served" >"$d/$f"
      done
    done
  else
    cp "$BOOT/ipxe.efi" "$BOOT/scout.efi" "$static/blobs/internal/$arch/"
  fi
  local p
  for p in 80 8080; do
    start_bg "pxe-$p" sudo -n ip netns exec "$SVC_NS" sudo -n -u "$(id -un)" env PXE_BIND_PORT=$p RUST_LOG="${NICO_RUST_LOG:-info}" \
      FORGE_ROOT_CAFILE_PATH="$CERTS/ca.crt" FORGE_CLIENT_CERT_PATH="$CERTS/nico-pxe.crt" FORGE_CLIENT_KEY_PATH="$CERTS/nico-pxe.key" \
      FORGE_BOOTSTRAP_ROOT_CAFILE_PATH="$CERTS/ca.crt" CARBIDE_PXE_TEMPLATE_DIRECTORY="$SRC/pxe/templates" \
      CARBIDE_API_INTERNAL_URL="https://10.90.0.20:$API_PORT" CARBIDE_API_URL="https://10.90.0.20:$API_PORT" \
      CARBIDE_PXE_URL=http://10.90.0.17:8080 CARBIDE_STATIC_PXE_URL=http://10.90.0.17:8080 \
      "$BOOT/carbide" -s "$static"
  done
  if wait_for 60 "carbide-pxe" sudo -n ip netns exec "$SVC_NS" curl -fsS -o /dev/null "http://10.90.0.17:8080/public/blobs/internal/$arch/ipxe.efi"; then
    result pxe PASS "carbide-pxe (linux/$goarch) at 10.90.0.17:80 and :8080 in namespace $SVC_NS, linked to the twin's routed site network (logs/svcnet.log)"
  else tail -5 "$LOGS/pxe-8080.log"; result pxe FAIL "carbide-pxe does not serve ipxe.efi (logs/pxe-8080.log)"; fi
}

host_path() { echo "site-1/hall-a/$READY_HOST"; }
# the vm host's machine id in NICo (by its expected machine's dc-twin.path label)
host_id() { local j; j=$(admin -f json machine show 2>/dev/null); python3 -I "$here/ready.py" id "$(host_path)" "$WORK/inputs/expected-machines.json" <<<"$j"; }
has_host() { [ -n "$(host_id)" ]; }
host_state() { local j; j=$(admin -f json machine show "$1" 2>/dev/null); python3 -I "$here/ready.py" state <<<"$j"; }
# what NICo's state controller last made of the host: wait (and why), transition, error
host_outcome() { docker exec "$PG" psql -U postgres -d postgres -Atc "select controller_state_outcome from machines where id = '$1'" 2>/dev/null | jq -r '"\(.outcome // ""): \(.reason // .error // "")"' 2>/dev/null; }

# bmc_curl <device> [curl options]: Redfish (the resource RF_PATH) on a device's BMC, at its
# loopback endpoint, with whichever password works: the site password NICo rotates it to, or the
# factory one
bmc_curl() {
  local ep user pass p
  ep=$(jq -r --arg d "$1" '.endpoints[$d].redfish' "$STATE/endpoints.json")
  user=$(jq -r --arg d "$1" '.[] | select(.path==$d) | .user' "$WORK/credentials.json")
  pass=$(jq -r --arg d "$1" '.[] | select(.path==$d) | .password' "$WORK/credentials.json")
  shift
  # the site password first, quietly (a 401 means NICo has not rotated it yet), then the factory one
  curl -fsk -u "$user:$SITE_PASSWORD" --cacert "$STATE/estate-ca.pem" "$@" "https://$ep${RF_PATH}" ||
    curl -fsSk -u "$user:$pass" --cacert "$STATE/estate-ca.pem" "$@" "https://$ep${RF_PATH}"
}
# The operator's firmware step. NICo's site explorer pairs a DGX H100 ("Viking") only when its
# CPLDMB_0 firmware is 0.2.1.9 (crates/site-explorer/src/lib.rs, "A DC power cycle may be
# needed"); the captured DGX's is 0.2.1.8. A site updates it before NICo can ingest the host:
# here, a Redfish SimpleUpdate of CPLDMB_0 through the host's BMC, staged by the BMC's update task
# and applied at the host's next power-on, then a DC power cycle.
host_reset() { RF_PATH=/redfish/v1/Systems/DGX/Actions/ComputerSystem.Reset bmc_curl "$(host_path)" -X POST -H 'Content-Type: application/json' -d "{\"ResetType\":\"$1\"}" >/dev/null; }
host_power() { [ "$(RF_PATH=/redfish/v1/Systems/DGX bmc_curl "$(host_path)" 2>/dev/null | jq -r .PowerState)" = "$1" ]; }
cpld_version() { RF_PATH=/redfish/v1/UpdateService/FirmwareInventory/CPLDMB_0 bmc_curl "$(host_path)" 2>/dev/null | jq -r '.Version // empty'; }
cpld_current() { [ "$(cpld_version)" = 0.2.1.9 ]; }
ready_firmware() {
  local v
  v=$(cpld_version)
  [ -n "$v" ] || return 0
  if [ "$v" = 0.2.1.9 ]; then result operator-cpld PASS "CPLDMB_0 is at 0.2.1.9, the version NICo's site explorer pairs a DGX H100 at"; return 0; fi
  RF_PATH=/redfish/v1/UpdateService/Actions/UpdateService.SimpleUpdate bmc_curl "$(host_path)" -X POST -H 'Content-Type: application/json' \
    -d '{"ImageURI":"http://10.90.0.17:8080/public/blobs/firmware/0.2.1.9.bin","Targets":["/redfish/v1/UpdateService/FirmwareInventory/CPLDMB_0"]}' >/dev/null ||
    { result operator-cpld FAIL "SimpleUpdate of CPLDMB_0 refused"; return 0; }
  sleep 10 # the update task completes, and the image is staged for the host's next power-on
  # a DC power cycle, as the DGX's BMC offers it (ForceOff, then On: its ResetActionInfo has no
  # PowerCycle)
  host_reset ForceOff
  wait_for 60 "the host off" host_power Off
  host_reset On
  if wait_for 120 "CPLDMB_0 at 0.2.1.9" cpld_current; then
    result operator-cpld PASS "the operator updated the DGX H100's CPLDMB_0 $v → 0.2.1.9 (Redfish SimpleUpdate through its BMC, applied at a DC power cycle: ForceOff, On), the version NICo's site explorer pairs a DGX at"
  else result operator-cpld FAIL "CPLDMB_0 still $(cpld_version) after the update and the restart"; fi
}

step_host() {
  say "the vm host: DHCP through the twin relay to NICo, HTTP boot of NICo's iPXE and scout, NICo's state machine to Ready"
  local start=$SECONDS id st last="" o lasto=""
  # NICo creates the host as a zero-DPU managed host once its BMC is explored and preingested
  wait_for 1500 "NICo to create the vm host" has_host
  id=$(host_id)
  if [ -z "$id" ]; then
    admin machine show >"$OUT/machines-ready.txt"
    result ready-machine FAIL "NICo created no machine for $(host_path) in 25 minutes (out/machines-ready.txt)"
    ready_evidence ""
    return 1
  fi
  result ready-machine PASS "$(host_path) is NICo machine $id, created as a zero-DPU host (its expected machine: dpu_policy ignore, the boot NIC primary on the HostInband segment)"
  # follow the host's state until Ready, recording each state; give up after READY_WAIT, or once
  # one state has held for READY_STALL (longer than NICo's retry after a failure, 15 minutes here)
  local end=$((SECONDS + READY_WAIT)) since=$SECONDS
  : >"$OUT/host-states.txt"
  while [ $SECONDS -lt $end ]; do
    st=$(host_state "$id")
    if [ -n "$st" ] && [ "$st" != "$last" ]; then
      printf '%s +%ds %s\n' "$(date -u +%H:%M:%S)" $((SECONDS - start)) "$st" | tee -a "$OUT/host-states.txt" | sed 's/^/  state: /'
      last=$st since=$SECONDS
      admin -f json machine show "$id" >"$OUT/host-$(wc -l <"$OUT/host-states.txt" | tr -d ' ').json" 2>/dev/null
    fi
    case $st in Ready | Ready/* | READY*) break ;; esac
    if [ $((SECONDS - since)) -gt "${READY_STALL:-1800}" ]; then
      echo "  state $last unchanged for $(((SECONDS - since) / 60)) min; stopping"
      break
    fi
    o=$(host_outcome "$id")
    if [ -n "$o" ] && [ "$o" != "$lasto" ]; then
      printf '%s +%ds   %s\n' "$(date -u +%H:%M:%S)" $((SECONDS - start)) "$o" >>"$OUT/host-outcomes.txt"
      lasto=$o
    fi
    id=$(host_id) # a predicted host's id becomes the stable one at discovery
    sleep 15
  done
  ready_evidence "$id"
  # the path is NICo's own record of the host's states (every transition, with its time; the
  # machine's events in `machine show`), not the polled states
  local path
  python3 -I "$here/ready.py" history "$OUT/host.json" >"$OUT/host-state-history.txt"
  path=$(cut -d'|' -f2 "$OUT/host-state-history.txt" 2>/dev/null | grep -v '^created$' | uniq | paste -sd' ' - | sed 's/ / → /g')
  [ -n "$path" ] || path=$(awk '{print $3}' "$OUT/host-states.txt" | paste -sd' ' - | sed 's/ / → /g')
  case $last in
    Ready | Ready/* | READY*) result ready-state PASS "$(host_path) reached Ready in $(((SECONDS - start) / 60)) min: $path (out/host-state-history.txt)" ;;
    *) result ready-state FAIL "$(host_path) stopped at $last after $(((SECONDS - start) / 60)) min: $path; NICo: $(host_outcome "$id") (out/host-state-history.txt, out/host-outcomes.txt, out/host.json)" ;;
  esac
}

# host_state_history: NICo's machine state history (its own table), one line per transition:
# time|machine id|state/substate
host_state_history() {
  docker exec "$PG" psql -U postgres -d postgres -Atc \
    "select to_char(timestamp at time zone 'UTC', 'HH24:MI:SS') || '|' || object_id || '|' || state::text from machine_state_history order by id" 2>/dev/null |
    while IFS='|' read -r t m st; do
      printf '%s|%s|%s\n' "$t" "$m" "$(jq -r '.state as $s | ([to_entries[] | select(.value | type == "object") | .value.state? // empty | strings] | first) as $sub | if $sub then "\($s)/\($sub)" else $s end' <<<"$st" 2>/dev/null)"
    done
}

# ready_evidence <machine id>: what the twin and NICo recorded of the host's boot and discovery,
# checked by ready.py into result rows
ready_evidence() {
  [ -n "$1" ] && admin -f json machine show "$1" >"$OUT/host.json" 2>/dev/null
  host_state_history >"$OUT/machine-state-history.txt" # NICo's table, with the predicted host's rows
  twin events --no-reads --select "$(host_path)" >"$OUT/host-events.txt" 2>/dev/null
  twin console "$(host_path)" >"$OUT/host-console.txt" 2>/dev/null
  cat "$LOGS/pxe-80.log" "$LOGS/pxe-8080.log" >"$LOGS/pxe.log" 2>/dev/null
  python3 -I "$here/ready.py" evidence "$OUT/host.json" "$OUT/host-events.txt" "$LOGS/pxe.log" "$LOGS/dhcp-server.log" \
    "$LOGS/nico-api.log" "$WORK/inputs/expected-machines.json" "$(host_path)" "$STATE" >"$OUT/host-evidence.tsv" 2>"$OUT/host-evidence.err"
  local check status evidence
  while IFS=$'\t' read -r check status evidence; do
    [ -n "$check" ] && result "$check" "$status" "$evidence"
  done <"$OUT/host-evidence.tsv"
}

# step_trays: NICo's machines on the trays path, followed until every GB300 tray is Ready (or no
# machine has changed state for TRAYS_STALL), then the rack's state and what NVLink partitioning
# did. The machines' software is the twin's impersonators (D55): the fidelity report says how far
# each machine's got, and the run is labelled impersonated.
tray_hosts() { jq '[.expected_machines[] | select((.metadata.labels // []) | any(.key == "dc-twin.path" and (.value | test("compute-tray"))))] | length' "$WORK/inputs/expected-machines.json" 2>/dev/null || echo 18; }
step_trays() {
  say "the GB300 trays: DPU init with impersonated DPU software, host discovery with impersonated scout, towards Ready"
  local start=$SECONDS end=$((SECONDS + TRAYS_WAIT)) since=$SECONDS last="" now j want
  want=$(tray_hosts)
  : >"$OUT/trays-states.txt"
  while [ $SECONDS -lt $end ]; do
    j=$(admin -f json machine show 2>/dev/null)
    now=$(python3 -I "$here/trays.py" summary <<<"$j" | paste -sd';' -)
    if [ -n "$now" ] && [ "$now" != "$last" ]; then
      printf '%s +%ds %s\n' "$(date -u +%H:%M:%S)" $((SECONDS - start)) "$now" | tee -a "$OUT/trays-states.txt" | sed 's/^/  /'
      last=$now since=$SECONDS
    fi
    [ "$(python3 -I "$here/trays.py" states <<<"$j" | awk '$2=="host" && $3 ~ /^Ready/' | wc -l | tr -d ' ')" -ge "$want" ] && break
    if [ $((SECONDS - since)) -gt "${TRAYS_STALL:-1800}" ]; then
      echo "  no machine changed state for $(((SECONDS - since) / 60)) min; stopping"
      break
    fi
    sleep 30
  done
  admin -f json machine show >"$OUT/machines.json" 2>/dev/null
  python3 -I "$here/trays.py" states <"$OUT/machines.json" >"$OUT/machine-states.txt"
  host_state_history >"$OUT/machine-state-history.txt"
  local dpus dinit hosts ready
  dpus=$(awk '$2=="dpu"' "$OUT/machine-states.txt" | wc -l | tr -d ' ')
  hosts=$(awk '$2=="host"' "$OUT/machine-states.txt" | wc -l | tr -d ' ')
  dinit=$(awk '$2=="host" || $2=="predicted"' "$OUT/machine-states.txt" | grep -ci 'DPUInit' || true)
  ready=$(awk '$2=="host" && $3 ~ /^Ready/' "$OUT/machine-states.txt" | wc -l | tr -d ' ')
  if [ "$dpus" -gt 0 ] && [ "$dinit" -eq 0 ]; then result trays-dpu-init PASS "$dpus DPUs discovered by NICo; no managed host left in DPUInitializing (impersonated DPU software, D55)"
  else result trays-dpu-init FAIL "$dinit managed hosts still in DPUInitializing, $dpus DPUs (out/machine-states.txt)"; fi
  if [ "$ready" -ge "$want" ]; then result trays-ready PASS "$ready of $want GB300 trays Ready in $(((SECONDS - start) / 60)) min (impersonated scout, D55)"
  else result trays-ready FAIL "$ready of $want GB300 trays Ready; $hosts hosts: $(awk '$2=="host"{print $3}' "$OUT/machine-states.txt" | sort | uniq -c | awk '{printf "%s %s, ", $1, $2}') (out/machine-states.txt, out/trays-states.txt)"; fi
  # the rack: once its trays are Ready NICo runs its maintenance (ConfigureNmxCluster on the
  # twin's RMS) and validation; nvlink-manager then reads the NVLink domain from NMX-C at the
  # configured switch's NVOS address. Both are waited for (TRAYS_RACK_WAIT [1800]).
  local rack nvl rend=$((SECONDS + ${TRAYS_RACK_WAIT:-1800}))
  while :; do
    admin -f json rack show >"$OUT/racks.json" 2>/dev/null
    rack=$(python3 -I "$here/trays.py" racks <"$OUT/racks.json" 2>/dev/null | grep -i a03 | head -1)
    nvl=$(grep -a nvl_partition_monitor "$LOGS/nico-api.log" | tail -1 | grep -aoE "machines_scanned: [0-9]+|nmx_c_unreachable_chassis: \{[^}]*\}|nmxc_num_partitions: [0-9]+|nmxc_num_gpus: [0-9]+" | paste -sd' ' -)
    grep -qiE ' Ready' <<<"$rack" && grep -qE 'nmxc_num_gpus: [1-9]' <<<"$nvl" && break
    [ $SECONDS -lt $rend ] || break
    sleep 30
  done
  if grep -qiE ' Ready' <<<"$rack"; then result trays-rack PASS "rack $rack, after its maintenance (ConfigureNmxCluster on the twin's RMS) and validation (out/racks.json)"
  else result trays-rack FAIL "rack ${rack:-a03 not listed} (out/racks.json)"; fi
  # NICo's own record of the domain: each tray's nvlink_info, its domain UUID from NMX-C
  local dom want_dom
  dom=$(docker exec "$PG" psql -U postgres -d postgres -Atc "select distinct nvlink_info->>'domain_uuid' from machines where nvlink_info is not null" 2>/dev/null | paste -sd, -)
  want_dom=$(jq -r '.[0].domainUuid // empty' "$STATE/nmxc-endpoints.json" 2>/dev/null)
  if grep -qE 'nmxc_num_gpus: [1-9]' <<<"$nvl" && [ -n "$want_dom" ] && [ "$dom" = "$want_dom" ]; then
    result trays-nvlink PASS "nvlink-manager read the NVLink domain from the twin's NMX-C ($nvl); every tray's domain_uuid is the twin's, $want_dom"
  else result trays-nvlink FAIL "nvlink-manager: ${nvl:-no partition monitor pass}; trays' domain_uuid ${dom:-none}, the twin's ${want_dom:-unknown} (logs/nico-api.log)"; fi
  twin report >"$OUT/twin-report.txt" 2>/dev/null
  grep -A3 'nicoagent' "$OUT/twin-report.txt" | grep -m1 'reached:' | sed 's/^ *//' | while read -r l; do result trays-impersonated KNOWN "$l (out/twin-report.txt)"; done
}

step_report() {
  say "results"
  if command -v column >/dev/null; then column -t -s $'\t' "$RESULTS" | sed 's/^/  /'; else sed 's/^/  /' "$RESULTS"; fi
  echo "  evidence: $OUT, logs: $LOGS"
}

step_down() {
  say "stop everything this script started"
  local p
  for p in "$WORK"/*.pid; do [ -f "$p" ] && stop_bg "$(basename "$p" .pid)"; done
  docker rm -f "$PG" "$VAULT" >/dev/null 2>&1 || true
  if { [ -f "$WORK/ready-mode" ] || [ -f "$WORK/trays-mode" ]; } && [ "$uname_s" = Linux ]; then sudo -n ip netns del "$SVC_NS" 2>/dev/null || true; fi
  echo "  stopped processes; removed containers $PG $VAULT${READY:+ and the namespace $SVC_NS}"
}

steps=("$@")
[ ${#steps[@]} -gt 0 ] || steps=(help)
if [ "${steps[0]}" = all ]; then
  steps=(fetch build classify up nico load dhcp discover health faults consumer report down)
fi
if [ "${steps[0]}" = ready ]; then
  READY=1
  touch "$WORK/ready-mode"   # a WORK the Ready path used stays on it, step by step
  steps=(fetch boot build up pxe nico load dhcp host report down)
fi
# (a pipeline name alone runs the whole path; among other steps, `trays` is the one step)
if [ "${steps[0]}" = trays ] && [ ${#steps[@]} -eq 1 ]; then
  TRAYS=1
  touch "$WORK/trays-mode"
  steps=(fetch boot build up pxe nico load dhcp trays report down)
fi
[ -f "$WORK/ready-mode" ] && READY=1
[ -f "$WORK/trays-mode" ] && TRAYS=1
for s in "${steps[@]}"; do
  case $s in
    help) sed -n '2,/^set -uo pipefail/p' "$0" | sed '$d'; exit 0;;
    fetch|build|classify|up|nico|load|dhcp|discover|health|faults|consumer|report|down) "step_$s";;
    trays) TRAYS=1; touch "$WORK/trays-mode"; step_trays;;
    boot|pxe) trays || { READY=1; touch "$WORK/ready-mode"; }; "step_$s";;
    host) READY=1; touch "$WORK/ready-mode"; "step_$s";;
    *) die "unknown step $s";;
  esac
done

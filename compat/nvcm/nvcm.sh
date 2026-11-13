#!/bin/sh
# NVIDIA Config Manager 1.4.1 (github.com/dsx-ai-factory/nv-config-manager, 2f8bf14), run
# unmodified from its own images as plain containers, as its Helm chart runs them (18 — Z7c, D80):
#
#   backend, on a bridge network <name>-net (each by its container name):
#     <name>-postgres  PostgreSQL 17: nautobot, config_store, kea_dhcp
#     <name>-redis     Valkey 8 (Redis' protocol)
#     <name>-nats      NATS with JetStream; the streams nv-config-manager and nautobot
#     <name>-nautobot  Nautobot 2.4 with NVCM's apps and jobs (uWSGI on 8003), and its Celery worker
#     <name>-config-store, <name>-render, <name>-render-dcim, <name>-render-device, <name>-ztp-api
#   edge, <name>-edge: Kea (supervisord: kea-dhcp4, its control agent on 8000, stork), on the bridge
#     as eth1; its eth0 is the site's management network, which the twin attaches (spec.adapters.
#     network.attach, interface eth0: NVCM's Kea configuration names eth0). In its network namespace:
#     <name>-dhcp-api, <name>-dhcp-sync (NVCM's sync of Kea's configuration), <name>-ztp (the
#     device-facing ZTP API on 80, as the chart's http-lb) and <name>-sftp (2222).
#
# Usage: nvcm.sh up <name> <work>        the containers, ready but for the edge's eth0
#        nvcm.sh load <name> <work> <blueprint dir> <ztp address>
#                                         the blueprint (stack-inputs/day0/nvcm) into Nautobot,
#                                         every device rendered, Kea's configuration made and set
#        nvcm.sh down <name>
# Environment: NVCM_SRC (NVCM's source at 1.4.1: its mock_topology job), DOCKER_HOST (a Podman
# service socket, unix://…; else the local podman), and the images below.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
APP=${NVCM_APP:-localhost/nvcm-app:1.4.1}
NAUTOBOT=${NVCM_NAUTOBOT:-localhost/nvcm-nautobot:1.4.1}
KEA=${NVCM_KEA:-localhost/nvcm-kea:1.4.1}
KEA_ADMIN=${NVCM_KEA_ADMIN:-localhost/nvcm-kea-admin:1.4.1}
PG=${NVCM_POSTGRES:-public.ecr.aws/docker/library/postgres:17-alpine}
REDIS=${NVCM_REDIS:-public.ecr.aws/valkey/valkey:8.0-alpine}
NATS=${NVCM_NATS:-public.ecr.aws/docker/library/nats:2.10-alpine}
NGINX=${NVCM_NGINX:-public.ecr.aws/nginx/nginx-unprivileged:1.27-alpine}
TOKEN=0123456789abcdef0123456789abcdef01234567
lbl="--label=dc-twin.test=nvcm"
PW=nvcm-secret-1
pm() { if [ -n "${DOCKER_HOST:-}" ]; then podman --url "$DOCKER_HOST" "$@"; else podman "$@"; fi; }
log() { printf '%s nvcm: %s\n' "$(date -u +%T)" "$*" >&2; }
die() { log "$*"; exit 1; }

wait_for() { # wait_for <what> <seconds> <command...>
  what=$1 secs=$2; shift 2
  n=0
  until "$@" >/dev/null 2>&1; do
    n=$((n+2)); [ $n -lt "$secs" ] || die "$what: not ready after ${secs}s"
    sleep 2
  done
  log "$what ready"
}

ini() { # the one INI every NVCM service reads (the chart's kubernetes-secrets.yaml)
  cat <<EOF
[auth]
required = false
accept_request_headers = true
[nautobot]
server = http://$1-nautobot:8080
public_url = http://$1-nautobot:8080
token = $TOKEN
version = 2.4.43
verify = false
cache_refresh_interval = 300
cache_ttl = 3600
[dcim]
provider = nautobot-2x
server = http://$1-nautobot:8080
public_url = http://$1-nautobot:8080
token = $TOKEN
verify = false
cache_refresh_interval = 300
cache_ttl = 3600
[dcim.options]
[nats]
server = nats://$1-nats:4222
queue = nv-config-manager
auth_method = password
user = nv-config-manager
password = $PW
local = true
config_manager_stream = nv-config-manager
config_manager_api_prefix = \$JS.API
config_manager_consumer_name = nv-config-manager-device
config_manager_subjects = nv-config-manager.nautobotchange,nv-config-manager.devicechange,nv-config-manager.workflow.result
render_change_stream = nv-config-manager
render_change_subject = nv-config-manager.nautobotchange
device_change_stream = nv-config-manager
device_change_subject = nv-config-manager.devicechange
archive_stream = nv-config-manager
archive_subject = nv-config-manager.workflow.result
archive_consumer_name = nv-config-manager-archive
archive_deliver_subject = nv-config-manager.archive.delivery
nautobot_stream = nautobot
nautobot_api_prefix = \$JS.API
nautobot_consumer_name = nv-config-manager-nautobot
nautobot_subjects = nautobot
nautobot_subject = nautobot
dcim_change_stream = nautobot
dcim_change_subject = nautobot
local_stream_max_bytes = 52428800
[redis]
host = $1-redis
port = 6379
db = 0
lock_db = 0
ssl = false
socket_timeout = 5
socket_connect_timeout = 5
password = $PW
[config_store]
database_host = $1-postgres
database_port = 5432
database = config_store
database_user = config_store
database_password = nvcm-db
[config_store.api]
cors_origins = http://localhost
[config_store.client]
api_service = http://$1-config-store:9000
api_url = http://$1-config-store:9000
ui_url = http://localhost
use_internal_endpoint = true
verify = false
[render]
api_service = http://$1-render:9000
api_url = http://$1-render:9000
use_internal_endpoint = true
[aggregate]
is_aggregate_environment = false
[dhcp]
api_service = http://$1-edge:9000
api_url = http://$1-edge:9000
cors_origins = http://localhost
[dhcp.kea]
server = 127.0.0.1
port = 8000
[dhcp.lease_db]
local = false
host = $1-postgres
port = 5432
database = kea_dhcp
user = dhcp
password = nvcm-db
[ztp]
api_service = http://$1-ztp-api:9000
api_url = http://$1-ztp-api:9000
user_domain = dctwin.lab
use_internal_endpoint = true
storage_type = file
file_store_path = /mnt/images
http_stream_chunk_bytes = 16777216
http_max_concurrent_downloads = 2
sftp_read_ahead_bytes = 16777216
sftp_max_concurrent_downloads = 8
sftp_metrics_port = 9100
EOF
}

nautobot_env() {
  echo "NAUTOBOT_DB_ENGINE=django.db.backends.postgresql NAUTOBOT_DB_HOST=$1-postgres NAUTOBOT_DB_PORT=5432 NAUTOBOT_DB_NAME=nautobot" \
    "NAUTOBOT_DB_USER=nautobot NAUTOBOT_DB_PASSWORD=nvcm-db NAUTOBOT_DB_TIMEOUT=300 NAUTOBOT_REDIS_HOST=$1-redis NAUTOBOT_REDIS_PORT=6379" \
    "NAUTOBOT_REDIS_SSL=false NAUTOBOT_REDIS_PASSWORD=$PW NAUTOBOT_CACHE_REDIS_DB=1 NAUTOBOT_ALLOWED_HOSTS=* NAUTOBOT_SECRET_KEY=dctwin-nvcm-nautobot-secret-key-of-fifty-characters-or-more-0123" \
    "NAUTOBOT_CREATE_SUPERUSER=true NAUTOBOT_SUPERUSER_NAME=admin NAUTOBOT_SUPERUSER_EMAIL=admin@dctwin.lab NAUTOBOT_SUPERUSER_PASSWORD=$PW" \
    "NAUTOBOT_SUPERUSER_API_TOKEN=$TOKEN NATS_HOST=nats://$1-nats:4222 NATS_USER=nautobot NATS_PASSWORD=$PW NV_CONFIG_MANAGER_DEPLOYMENT_TYPE=all" \
    "NAUTOBOT_METRICS_ENABLED=false NAUTOBOT_DEBUG=false NAUTOBOT_LOG_LEVEL=INFO NAUTOBOT_JOBS_ROOT=/opt/nautobot/jobs" \
    "PYTHONPATH=/opt/nautobot/jobs:/usr/local/lib/python3.11/dist-packages:/app PYTHONIOENCODING=utf-8 LC_ALL=C.UTF-8 LANG=C.UTF-8" \
    "NV_CONFIG_MANAGER_INI=/etc/nv-config-manager/nv-config-manager.ini"
}

envflags() { for kv in "$@"; do printf -- '-e\n%s\n' "$kv"; done; }

up() {
  name=$1 work=$2
  [ -n "${NVCM_SRC:-}" ] && [ -d "$NVCM_SRC/development/mock_topology" ] || die "NVCM_SRC must name NVCM's source at 1.4.1"
  mkdir -p "$work/etc" "$work/custom" "$work/static" "$work/media" "$work/prom" "$work/images" "$work/initdb" "$work/jetstream"
  ini "$name" > "$work/etc/nv-config-manager.ini"
  # the render containers' secrets (installer accounts.py): per site, the switches' passwords and
  # the hash salt, the site named as NVCM slugs it
  site=$(printf '%s' "${NVCM_SITE:-dctwin}" | tr 'A-Z ' 'a-z-')
  printf '[site.%s]\nroot_password_r1 = Day0-NVCM-1\napi_user_key_r1 = Day0-NVCM-1\nbgp_password_r1 = bgp-nvcm\nisis_password_r1 = isis-nvcm\ntacacs_key_r1 = tacacs-nvcm\nhash_salt = W585uq6K\n' "$site" > "$work/etc/config-secrets.ini"
  cp "$here/nats.conf" "$here/nginx.conf" "$here/uwsgi.ini" "$here/streams.py" "$here/load.py" "$work/etc/"
  cp "$here/initdb.sql" "$work/initdb/"
  [ -s "$work/images/manifest.json" ] || echo '{"images": []}' > "$work/images/manifest.json"
  # NVCM's jobs volume (pvc_updater.py): mock_topology, its blueprints with it
  rm -rf "$work/custom/mock_topology"
  cp -R "$NVCM_SRC/development/mock_topology" "$work/custom/mock_topology"
  find "$work/custom" \( -name __pycache__ -o -name tests -o -name .venv \) -prune -exec rm -rf {} + 2>/dev/null || true
  : > "$work/custom/__init__.py"
  chmod -R a+rwX "$work"
  pm network rm -f "$name-net" >/dev/null 2>&1 || true
  pm network create "$name-net" >/dev/null
  run() { pm run -d $lbl --network "$name-net" --name "$@" >/dev/null; }

  for c in postgres redis nats nautobot-nginx nautobot nautobot-init nautobot-worker config-store config-store-migrate config-store-cache render render-dcim render-device ztp-api edge kea-admin dhcp-api dhcp-sync ztp sftp; do
    pm rm -f "$name-$c" >/dev/null 2>&1 || true
  done
  run "$name-postgres" -e POSTGRES_PASSWORD=nvcm-root -v "$work/initdb:/docker-entrypoint-initdb.d:ro,Z" "$PG"
  run "$name-redis" "$REDIS" valkey-server --requirepass "$PW"
  run "$name-nats" -e SYS_PASSWORD=$PW -e NV_CONFIG_MANAGER_PASSWORD=$PW -e NAUTOBOT_PASSWORD=$PW \
    -v "$work/etc/nats.conf:/etc/nats/nats.conf:ro,Z" -v "$work/jetstream:/data/jetstream:Z" "$NATS" -c /etc/nats/nats.conf
  wait_for postgres 120 pm exec "$name-postgres" pg_isready -U postgres -d kea_dhcp

  nb() { # nb <container> <cmd...>: a Nautobot container, NVCM's jobs and INI mounted
    c=$1; shift
    # shellcheck disable=SC2046
    pm run -d $lbl --network "$name-net" --name "$name-$c" $(envflags $(nautobot_env "$name") ${NB_EXTRA:-}) \
      -v "$work/etc/nv-config-manager.ini:/etc/nv-config-manager/nv-config-manager.ini:ro,Z" \
      -v "$work/etc/uwsgi.ini:/opt/nautobot/uwsgi.ini:ro,Z" -v "$work/custom:/opt/nautobot/jobs/custom:Z" \
      -v "$work/static:/opt/nautobot/static:Z" -v "$work/media:/opt/nautobot/media:Z" -v "$work/prom:/prom_cache:Z" \
      -v "$work/etc:/dctwin:ro,Z" "$NAUTOBOT" "$@" >/dev/null
  }
  log "Nautobot: migrations, superuser, bootstrap data (migration_runner)"
  NB_EXTRA=NAUTOBOT_RUN_BOOTSTRAP=true nb nautobot-init python /opt/nautobot/jobs/nv_config_manager_jobs/migration_runner.py
  rc=$(pm wait "$name-nautobot-init")
  [ "$rc" = 0 ] || { pm logs --tail 60 "$name-nautobot-init" >&2; die "Nautobot's migrations: exit $rc"; }
  nb nautobot nautobot-server start --ini /opt/nautobot/uwsgi.ini
  nb nautobot-worker nautobot-server celery worker --loglevel INFO --queues default --events --concurrency 2
  # the chart's nginx in front of uWSGI, in Nautobot's network namespace
  pm run -d $lbl --network "container:$name-nautobot" --name "$name-nautobot-nginx" \
    -v "$work/etc/nginx.conf:/etc/nginx/conf.d/default.conf:ro,Z" -v "$work/static:/opt/nautobot/static:ro,Z" "$NGINX" >/dev/null
  wait_for nautobot 300 pm exec "$name-nautobot" python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/health', timeout=5)"
  pm exec "$name-nautobot" python /dctwin/streams.py "nats://$name-nats:4222" nv-config-manager "$PW" >&2

  app() { # app <container> <net> <cmd...>: an NVCM service, its INI mounted
    c=$1 net=$2; shift 2
    pm run -d $lbl --network "$net" --name "$name-$c" -e NV_CONFIG_MANAGER_INI=/etc/nv-config-manager/nv-config-manager.ini \
      -e LOG_LEVEL=INFO -e ENVIRONMENT=local -e NV_CONFIG_MANAGER_DEV_SALT=W585uq6K \
      -e NV_CONFIG_MANAGER_CONFIG_SECRET_PATH=/etc/nv-config-manager/secrets/config-secrets.ini \
      -v "$work/etc/nv-config-manager.ini:/etc/nv-config-manager/nv-config-manager.ini:ro,Z" \
      -v "$work/etc/config-secrets.ini:/etc/nv-config-manager/secrets/config-secrets.ini:ro,Z" \
      -v "$work/images:/mnt/images:Z" "$APP" "$@" >/dev/null
  }
  log "config-store: migrations, API, cache refresh"
  app config-store-migrate "$name-net" alembic -c db/alembic.ini upgrade head
  rc=$(pm wait "$name-config-store-migrate")
  [ "$rc" = 0 ] || { pm logs --tail 40 "$name-config-store-migrate" >&2; die "config-store's migrations: exit $rc"; }
  pm run -d $lbl --network "$name-net" --name "$name-config-store" -e NV_CONFIG_MANAGER_SERVICE=config_store.api -e RETENTION_DAYS=365 \
    -e NV_CONFIG_MANAGER_INI=/etc/nv-config-manager/nv-config-manager.ini -e LOG_LEVEL=INFO -e ENVIRONMENT=local \
    -v "$work/etc/nv-config-manager.ini:/etc/nv-config-manager/nv-config-manager.ini:ro,Z" "$APP" nv-config-manager-config-store-api >/dev/null
  app config-store-cache "$name-net" nv-config-manager-config-store-cache-refresh
  log "render: API and its two consumers"
  pm run -d $lbl --network "$name-net" --name "$name-render" -e NV_CONFIG_MANAGER_SERVICE=render -e NV_CONFIG_MANAGER_DEV_SALT=W585uq6K \
    -e NV_CONFIG_MANAGER_CONFIG_SECRET_PATH=/etc/nv-config-manager/secrets/config-secrets.ini -e NV_CONFIG_MANAGER_INI=/etc/nv-config-manager/nv-config-manager.ini \
    -v "$work/etc/nv-config-manager.ini:/etc/nv-config-manager/nv-config-manager.ini:ro,Z" \
    -v "$work/etc/config-secrets.ini:/etc/nv-config-manager/secrets/config-secrets.ini:ro,Z" "$APP" nv-config-manager-render-api >/dev/null
  for k in dcim device; do
    pm run -d $lbl --network "$name-net" --name "$name-render-$k" -e NATS_CONSUMER=$k -e NV_CONFIG_MANAGER_DEV_SALT=W585uq6K \
      -e NV_CONFIG_MANAGER_CONFIG_SECRET_PATH=/etc/nv-config-manager/secrets/config-secrets.ini -e NV_CONFIG_MANAGER_INI=/etc/nv-config-manager/nv-config-manager.ini \
      -v "$work/etc/nv-config-manager.ini:/etc/nv-config-manager/nv-config-manager.ini:ro,Z" \
      -v "$work/etc/config-secrets.ini:/etc/nv-config-manager/secrets/config-secrets.ini:ro,Z" "$APP" nv-config-manager-render-consumer >/dev/null
  done
  pm run -d $lbl --network "$name-net" --name "$name-ztp-api" -e NV_CONFIG_MANAGER_SERVICE=ztp -e NV_CONFIG_MANAGER_INI=/etc/nv-config-manager/nv-config-manager.ini \
    -v "$work/etc/nv-config-manager.ini:/etc/nv-config-manager/nv-config-manager.ini:ro,Z" -v "$work/images:/mnt/images:Z" "$APP" nv-config-manager-ztp-api >/dev/null

  log "DHCP: Kea's lease database, the edge (Kea), the DHCP API and the configuration sync"
  pm run $lbl --rm --network "$name-net" --name "$name-kea-admin" -e INI_FILE=/etc/vault/nv-config-manager.ini \
    -v "$work/etc/nv-config-manager.ini:/etc/vault/nv-config-manager.ini:ro,Z" "$KEA_ADMIN" >&2 || die "kea-admin db-init"
  # the edge: on the bridge as eth1, its eth0 the twin's; ports below 1024 for NVCM's user (the
  # chart publishes the ZTP API's 8000 as 80)
  pm run -d $lbl --network "$name-net:interface_name=eth1" --name "$name-edge" --hostname "$name-edge" \
    --cap-add NET_RAW --cap-add NET_BIND_SERVICE --sysctl net.ipv4.ip_unprivileged_port_start=0 \
    -v "$work/etc/nv-config-manager.ini:/etc/vault/nv-config-manager.ini:ro,Z" "$KEA" >/dev/null
  edge() { # edge <container> <cmd...>: an NVCM service in the edge's network namespace
    c=$1; shift
    pm run -d $lbl --network "container:$name-edge" --name "$name-$c" -e NV_CONFIG_MANAGER_INI=/etc/nv-config-manager/nv-config-manager.ini \
      -e NV_CONFIG_MANAGER_KEA_SERVER=127.0.0.1 -e NV_CONFIG_MANAGER_KEA_PORT=8000 \
      -v "$work/etc/nv-config-manager.ini:/etc/nv-config-manager/nv-config-manager.ini:ro,Z" -v "$work/images:/mnt/images:Z" "$@" >/dev/null
  }
  edge dhcp-api -e NV_CONFIG_MANAGER_SERVICE=dhcp "$APP" nv-config-manager-dhcp-api
  edge dhcp-sync "$APP" nv-config-manager-dhcp-confgen sync-kea-configuration --ip-version 4 --refresh-interval 10
  edge ztp -e NV_CONFIG_MANAGER_REJECT_IDENTITY_HEADERS=true "$APP" nv-config-manager-ztp-api --port 80
  edge sftp "$APP" nv-config-manager-ztp-sftp --host 0.0.0.0 --port 2222 --level INFO
  log "up: attach $name-edge's eth0 to the management network"
}

load() {
  name=$1 work=$2 bp=$3 ztp=$4 bmcdhcp=${5:-}
  [ -d "$bp/context" ] || die "$bp is not a blueprint (context/<name>/…)"
  for d in "$bp"/context/*/; do
    b=$(basename "$d")
    rm -rf "$work/custom/mock_topology/context/$b"
    cp -R "$d" "$work/custom/mock_topology/context/$b"
    find "$work/custom/mock_topology/context/$b" -type f -exec sed -i "s/@ZTP_SERVER@/$ztp/g" {} +
    # the OOB-Leafs' BMC relay (18 — Z7d): the site's BMC DHCP, when it has one
    if [ -n "$bmcdhcp" ]; then
      find "$work/custom/mock_topology/context/$b" -type f -exec sed -i "s/@BMC_DHCP@/$bmcdhcp/g" {} +
    fi
    chmod -R a+rwX "$work/custom/mock_topology/context/$b"
    # the render's secrets for each site the blueprint names (the location of type Site, slugged
    # as NVCM's vault filter does: lower case, spaces to dashes)
    awk '/location_type: Site/ { s = 1 } s && /name:/ { sub(/.*name: */, ""); print; s = 0 }' "$work/custom/mock_topology/context/$b/locations.yaml" |
      tr 'A-Z ' 'a-z-' | while read -r site; do
        grep -q "^\[site\.$site\]" "$work/etc/config-secrets.ini" ||
          printf '[site.%s]\nroot_password_r1 = Day0-NVCM-1\napi_user_key_r1 = Day0-NVCM-1\nbgp_password_r1 = bgp-nvcm\nisis_password_r1 = isis-nvcm\ntacacs_key_r1 = tacacs-nvcm\nhash_salt = W585uq6K\n' "$site" >> "$work/etc/config-secrets.ini"
      done
    log "blueprint $b into Nautobot (MockTopologyDesign)"
    pm exec "$name-nautobot" python /dctwin/load.py "$TOKEN" "$b" dctwin >&2 || die "MockTopologyDesign $b failed"
  done
  log "render: every device"
  pm exec "$name-nautobot" python -c "
import json, urllib.request
r = urllib.request.urlopen(urllib.request.Request('http://$name-render:9000/v1/render/all', method='POST'), timeout=120)
print('render/all', r.status, r.read()[:300])" >&2
  # Kea's own configuration names eth0, which exists once the site's network is attached: until
  # then supervisord gave kea-dhcp4 up as exiting too quickly, as a pod started before its network
  # would be restarted, so it is started again
  log "Kea: kea-dhcp4 on eth0"
  pm exec "$name-edge" sh -c 'n=0; until [ -e /sys/class/net/eth0 ]; do n=$((n+1)); [ $n -gt 300 ] && exit 1; sleep 1; done; supervisorctl start kea-dhcp4 >/dev/null 2>&1; sleep 3; supervisorctl status kea-dhcp4' >&2 ||
    die "the edge has no eth0: attach it to the management network first"
  log "DHCP: Kea's configuration from Nautobot (refresh, then the sync sets it)"
  pm run $lbl --rm --network "container:$name-edge" --name "$name-dhcp-refresh" -e NV_CONFIG_MANAGER_INI=/etc/nv-config-manager/nv-config-manager.ini \
    -e NV_CONFIG_MANAGER_KEA_SERVER=127.0.0.1 -e NV_CONFIG_MANAGER_KEA_PORT=8000 \
    -v "$work/etc/nv-config-manager.ini:/etc/nv-config-manager/nv-config-manager.ini:ro,Z" "$APP" \
    nv-config-manager-dhcp-confgen refresh-kea-configuration --ip-version 4 >&2 || die "Kea's configuration"
}

down() {
  name=$1
  pm ps -a --format '{{.Names}}' | grep "^$name-" | xargs -r podman ${DOCKER_HOST:+--url "$DOCKER_HOST"} rm -f >/dev/null 2>&1 || true
  pm network rm -f "$name-net" >/dev/null 2>&1 || true
}

cmd=${1:-}; [ $# -gt 0 ] && shift
case $cmd in
  up) [ $# -eq 2 ] || die "nvcm.sh up <name> <work>"; up "$@" ;;
  load) [ $# -eq 4 ] || [ $# -eq 5 ] || die "nvcm.sh load <name> <work> <blueprint dir> <ztp address> [<BMC DHCP address>]"; load "$@" ;;
  down) [ $# -eq 1 ] || die "nvcm.sh down <name>"; down "$@" ;;
  *) die "usage: nvcm.sh up|load|down …" ;;
esac

# shellcheck shell=bash
# NVSentinel's datastore: PostgreSQL, one of the two it supports (MongoDB needs a replica set for
# change streams; PostgreSQL's LISTEN/NOTIFY needs nothing), on a free loopback port with trust
# authentication: this host's PostgreSQL when its initdb and pg_ctl are on PATH, else the official
# image as a container (`dctwin-nvs-postgres`, this host's network). NVSentinel's own modules
# create its tables (store-client's datastore.go); the harness creates only the database. Sourced
# after compat/kai/lib/common.sh.

NVS_POSTGRES_IMAGE=${NVS_POSTGRES_IMAGE:-docker.io/library/postgres:17.6-bookworm}

# nvs_postgres_up <dir>: exports PG_PORT, PG_DIR and PG_MODE (host | container).
nvs_postgres_up() {
	local dir=$1 bin i
	PG_PORT=$(free_port)
	rm -rf "$dir" && mkdir -p "$dir"
	PG_DIR=$dir
	if command -v pg_ctl >/dev/null && command -v initdb >/dev/null; then
		PG_MODE=host
		bin=$(dirname "$(command -v pg_ctl)")
		LC_ALL=C "$bin/initdb" -D "$dir/data" -U postgres -A trust -E UTF8 --no-locale >"$dir/initdb.log" 2>&1 || { cat "$dir/initdb.log" >&2; return 1; }
		LC_ALL=C "$bin/pg_ctl" -D "$dir/data" -l "$dir/postgres.log" -w \
			-o "-p $PG_PORT -c listen_addresses=127.0.0.1 -c unix_socket_directories=" start >/dev/null || { cat "$dir/postgres.log" >&2; return 1; }
	else
		PG_MODE=container
		docker rm -f dctwin-nvs-postgres >/dev/null 2>&1 || true
		docker run -d --name dctwin-nvs-postgres --label dc-twin.io/harness=nvsentinel --network host --memory 512m \
			-e POSTGRES_HOST_AUTH_METHOD=trust -e POSTGRES_USER=postgres -e POSTGRES_DB=postgres \
			"$NVS_POSTGRES_IMAGE" -c listen_addresses=127.0.0.1 -c port="$PG_PORT" >/dev/null
		for i in $(seq 1 120); do
			psqlq_db postgres 'select 1' >/dev/null 2>&1 && break
			sleep 0.5
		done
	fi
	export PG_PORT PG_DIR PG_MODE
	psqlq_db postgres 'CREATE DATABASE nvsentinel' >/dev/null
}

# psqlq_db <database> <sql> and psqlq <sql>: one query, unaligned, no headers.
psqlq_db() {
	if [ "${PG_MODE:-}" = container ]; then
		docker exec dctwin-nvs-postgres psql -h 127.0.0.1 -p "$PG_PORT" -U postgres -d "$1" -At -c "$2"
	else
		"$(dirname "$(command -v pg_ctl)")/psql" -h 127.0.0.1 -p "$PG_PORT" -U postgres -d "$1" -At -c "$2"
	fi
}
psqlq() { psqlq_db nvsentinel "$1"; }

nvs_postgres_down() {
	case "${PG_MODE:-}" in
	host) LC_ALL=C "$(dirname "$(command -v pg_ctl)")/pg_ctl" -D "$PG_DIR/data" -m fast -w stop >/dev/null 2>&1 || true ;;
	container) docker rm -f dctwin-nvs-postgres >/dev/null 2>&1 || true ;;
	esac
}

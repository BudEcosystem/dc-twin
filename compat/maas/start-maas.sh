#!/bin/bash
# Starts MAAS 3.7.3 in a container that has no systemd: each service as its systemd unit in the
# packages starts it (/usr/lib/systemd/system/maas-*.service: the same binary, user, environment
# and order), with its output in /var/log/maas/<service>.out. Writes /run/maas-ready when the
# region answers, the rack controller is registered and maas-agent runs; exits non-zero naming the
# service that did not come up.
#
#   start-maas.sh            MAAS_URL (default http://127.0.0.1:5240/MAAS), MAAS_WAIT (seconds per
#                            service, default 900: on a loaded host regiond takes minutes)
#
# One deviation from a default install: regiond runs one worker process (regiond.conf
# num_workers: 1, a documented option; the default is 4), to keep the container small.
set -u
URL=${MAAS_URL:-http://127.0.0.1:5240/MAAS}
WAIT=${MAAS_WAIT:-900}
LOG=/var/log/maas
mkdir -p "$LOG" /run/maas
chown maas:maas "$LOG" /run/maas
rm -f /run/maas-ready

say() { echo "start-maas: $*" >&2; }
# what each unit's User=maas Group=maas does
AS_MAAS=(setpriv --reuid=maas --regid=maas --init-groups env HOME=/var/lib/maas)
start() { # start <name> <command...>: in the background, from /var/lib/maas
	local name=$1
	shift
	(cd /var/lib/maas && exec "$@" >"$LOG/$name.out" 2>&1) &
	echo $! >"/run/maas-$name.pid"
}
running() { kill -0 "$(cat "/run/maas-$1.pid" 2>/dev/null)" 2>/dev/null; }
wait_for() { # wait_for <what> <command...>
	local what=$1 i
	shift
	for i in $(seq 1 "$WAIT"); do
		"$@" >/dev/null 2>&1 && { say "$what after ${i}s"; return 0; }
		sleep 1
	done
	say "gave up waiting for $what"
	return 1
}

pg_ctlcluster 16 main start
say "postgresql up"
# the URL the region tells racks and machines (the package set it from the build host's address)
maas-region local_config_set --maas-url "$URL"
grep -q '^num_workers:' /etc/maas/regiond.conf || echo 'num_workers: 1' >>/etc/maas/regiond.conf
maas-rack config --region-url "$URL"

# maas-regiond.service
start regiond "${AS_MAAS[@]}" DJANGO_SETTINGS_MODULE=maasserver.djangosettings.settings \
	prometheus_multiproc_dir=/var/lib/maas/prometheus /usr/sbin/regiond
# maas-http.service: nginx, once regiond has written the configuration it includes
wait_for "regiond's nginx configuration" test -s /var/lib/maas/http/regiond.nginx.conf || exit 1
/usr/sbin/nginx -t -q -g 'daemon on; master_process on;' -c /var/lib/maas/http/nginx.conf &&
	/usr/sbin/nginx -g 'daemon on; master_process on;' -c /var/lib/maas/http/nginx.conf
wait_for "the region API" curl -sf -m 30 -o /dev/null "$URL/api/2.0/version/" || exit 1
# the v3 API server reads the cluster certificates regiond creates on its first start (its unit
# restarts it until they exist; here it starts after them)
start apiserver "${AS_MAAS[@]}" /usr/sbin/maas-apiserver

# maas-temporal.service and maas-temporal-worker.service (Temporal's persistence is the region's
# PostgreSQL; on a loaded host its start can exceed its own deadlines, so it is retried)
for attempt in 1 2 3 4 5; do
	start temporal "${AS_MAAS[@]}" /usr/sbin/temporal-server -e production -r /var/lib/maas/temporal/ -c "" --allow-no-auth start
	sleep 60
	running temporal && break
	say "temporal-server exited (attempt $attempt): $(tail -1 "$LOG/temporal.out")"
done
running temporal || { say "temporal-server does not stay up"; exit 1; }
start temporal-worker "${AS_MAAS[@]}" /usr/sbin/maas-temporal-worker

# maas-rackd.service, then maas-agent.service once the rack has its agent configuration
rm -f /var/lib/maas/dhcpd.sock /var/lib/maas/dhcpd.conf /var/lib/maas/dhcpd6.conf
# the unit grants AmbientCapabilities=CAP_NET_BIND_SERVICE: rackd binds TFTP on port 69
start rackd setpriv --reuid=maas --regid=maas --init-groups --inh-caps=+net_bind_service \
	--ambient-caps=+net_bind_service env HOME=/var/lib/maas prometheus_multiproc_dir=/var/lib/maas/prometheus /usr/sbin/rackd
wait_for "the rack's agent configuration" test -s /etc/maas/agent.yaml || exit 1
start agent "${AS_MAAS[@]}" /usr/sbin/maas-agent
sleep 5
for s in regiond apiserver temporal temporal-worker rackd agent; do
	running "$s" || { say "$s is not running: $(tail -3 "$LOG/$s.out")"; exit 1; }
done
touch /run/maas-ready
say "MAAS is up: $URL"

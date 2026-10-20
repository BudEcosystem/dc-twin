#!/bin/bash
# Runs as root inside the run-snap.sh guest (Ubuntu 24.04, systemd, snapd): installs MAAS and its
# test database as snaps, gives the guest a BMC network, initialises MAAS as region+rack, starts the
# twin, and writes /opt/dctwin/ready when MAAS's rack controller and agent are up.
# MAAS_CHANNEL, MAAS_REVISION, TESTDB_CHANNEL come from run-snap.sh.
set -euxo pipefail
home=/home/twin
mkdir -p /opt/dctwin/out /root/.cache/dc-twin/assets
install -m 0755 "$home/dctwin" /opt/dctwin/dctwin
cp "$home/lab-model.yaml" "$home/flow.py" /opt/dctwin/
tar -C /root/.cache/dc-twin/assets -xzf "$home/assets.tgz"

snap wait system seed.loaded
snap install maas-test-db --channel="$TESTDB_CHANNEL"
snap install maas --channel="$MAAS_CHANNEL"
rev=$(snap list maas | awk 'NR == 2 {print $3}')
if [ -n "${MAAS_REVISION:-}" ] && [ "$rev" != "$MAAS_REVISION" ]; then
	echo "maas $MAAS_CHANNEL is revision $rev, expected $MAAS_REVISION" >&2
	exit 1
fi
snap list maas maas-test-db

# The BMC network: an interface with 10.250.0.1/24, made before MAAS starts so that the rack
# controller reports it (MAAS sends a machine's power actions to an agent on its BMC's subnet), and
# the whole /24 local, so that flow.py can give each BMC an address there with the standard ports.
ip link add dctwin-bmc type dummy
ip addr add 10.250.0.1/24 dev dctwin-bmc
ip link set dctwin-bmc up
ip route replace local 10.250.0.0/24 dev dctwin-bmc table local

addr=$(ip -4 -o route get 1.1.1.1 | sed -n 's/.* src \([0-9.]*\).*/\1/p')
maas init region+rack --database-uri maas-test-db:/// --maas-url "http://$addr:5240/MAAS"
maas createadmin --username admin --password "$(openssl rand -hex 12)" --email admin@example.invalid
for _ in $(seq 1 60); do
	key=$(maas apikey --username admin 2>/dev/null) && [ -n "$key" ] && break
	sleep 5
done
# the region's HTTP front (nginx) answers 502 for a while after `maas init`
for _ in $(seq 1 120); do
	curl -sf -o /dev/null http://127.0.0.1:5240/MAAS/api/2.0/version/ && break
	sleep 5
done
for _ in $(seq 1 30); do
	maas login admin http://127.0.0.1:5240/MAAS/ "$key" >/dev/null && break
	sleep 10
done
maas admin version read

# the twin, beside MAAS
# (detached, every descriptor redirected: anything left holding the ssh session's output would keep
# run-snap.sh's ssh waiting)
(cd /opt/dctwin && exec setsid nohup ./dctwin up lab-model.yaml --state /opt/dctwin/state --adapters redfish,ipmi \
	</dev/null >/opt/dctwin/up.log 2>&1) &
for _ in $(seq 1 120); do grep -q 'control API' /opt/dctwin/up.log && break; sleep 0.5; done
grep 'control API' /opt/dctwin/up.log

# MAAS is ready for power work when its rack controller reports the BMC network and its agent has
# configured the power service
for _ in $(seq 1 120); do
	maas admin rack-controllers read | grep -q '10.250.0.0/24' && break
	sleep 5
done
maas admin rack-controllers read | python3 -c '
import json, sys
for r in json.load(sys.stdin):
    print(r["hostname"], r["system_id"], r["version"],
          [(i["name"], [l.get("subnet", {}).get("cidr") for l in i["links"]]) for i in r["interface_set"]],
          [(s["name"], s["status"]) for s in r["service_set"]])'
# (the snap's pebble runs each service under systemd-cat: maas-agent logs to the journal)
for _ in $(seq 1 60); do
	journalctl -t maas-agent --no-pager | grep -q 'Started power-service' && break
	sleep 5
done
journalctl -t maas-agent --no-pager | grep -E 'Started power-service|TaskQueue=agent:power' | tail -4 || true
touch /opt/dctwin/ready

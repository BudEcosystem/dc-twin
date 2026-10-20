#!/usr/bin/env bash
# The checks of compat/opensm, run inside its container by run.sh: the twin brings the lab estate
# up with the ufm and ibsim adapters (the ibsim adapter runs ibsim on the model's InfiniBand
# fabric and follows the model's link state through ibsim's console); OpenSM, unmodified from
# Debian's archive, runs as the subnet manager at the UFM appliance's HCA, and infiniband-diags
# read the fabric, all through ibsim's preload library (libumad2sim). Results go to /work/out.
set -uo pipefail
W=/work
OUT=$W/out
EV=$OUT/evidence
mkdir -p "$EV"
. "$W/compat/kai/lib/common.sh"
. "$W/compat/kai/lib/twin.sh"
DCTWIN=$W/dctwin
ESTATE=$W/examples/estates/lab-model.yaml
ibnet() { python3 -I "$W/compat/opensm/ibnet.py" "$@"; }
pkgver() { dpkg-query -W -f '${Version}' "$1"; }
# j <json> <python expression over r>: one value from a helper's JSON
j() { python3 -I -c "import json,sys; r=json.loads(sys.argv[1]); print($2)" "$1"; }
brief() { j "$1" '"%d switches, %d CA ports, %d links; equal to the model: %s %s" % (r["switches"], r["cas"], r["links"], r["equal"], "; ".join(r["mismatches"][:2]))'; }

VERSIONS="OpenSM $(pkgver opensm), infiniband-diags $(pkgver infiniband-diags), ibsim $(pkgver ibsim-utils) (libumad2sim $(pkgver libumad2sim0)), Debian $(cat /etc/debian_version), $(uname -m)"
echo "$VERSIONS" >"$EV/versions.txt"
echo "$VERSIONS" >&2

OSM_PID=
cleanup() {
	[ -n "$OSM_PID" ] && kill "$OSM_PID" 2>/dev/null
	twin_down
}
trap cleanup EXIT

# ---------------------------------------------------------------- the twin, and the ibsim it runs

twin_up "$ESTATE" "$W/twin" "ufm,ibsim" || { echo "FAIL: the twin did not come up" >&2; exit 1; }
cp "$TWIN_STATE/ibsim.json" "$EV/ibsim.json"
. "$TWIN_STATE/ibsim-backend-ib.env" # IBSIM_SOCKNAME, SIM_HOST (the SM's), IBSIM_PRELOAD, IBSIM_TOPOLOGY
NET=$IBSIM_TOPOLOGY
SM_HOST=$SIM_HOST
TRANSCRIPT=$TWIN_STATE/ibsim-backend-ib.log
cp "$NET" "$EV/topology-rendered.net"
UFMCREDS=$TWIN_STATE/ufm-credentials.json
SWITCHES=22 CAS=193 LINKS=233 ISL=40 # the lab's backend-ib: 20 rail leaves and 2 spines

# mad <node id> <command...>: an infiniband-diags or OpenSM process attached at a node
mad() { local h=$1; shift; SIM_HOST=$h LD_PRELOAD=$IBSIM_PRELOAD "$@"; }

read -r TRAY7 PG7 SWP7 <<<"$(ibnet node "$NET" "a01-compute-tray-07 HCA-1")"
read -r TRAY1 _ _ <<<"$(ibnet node "$NET" "a01-compute-tray-01 HCA-1")"
read -r LEAF1 _ _ <<<"$(ibnet node "$NET" "c03-backend-ib-rail-a01-1")"
SMPG=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))[0]["smPortGuid"][2:])' "$TWIN_STATE/ibsim.json")
read -r R_RUN R_VER R_PID R_SW R_CA R_LINKS <<<"$(python3 -I -c 'import json,sys; i=json.load(open(sys.argv[1]))[0]; print(i["running"], i["version"], i["pid"], i["switches"], i["channelAdapters"], i["links"])' "$TWIN_STATE/ibsim.json")"
parsed=$(grep -o 'parsed [0-9]* lines' "$TRANSCRIPT" | head -1)
warns=$(grep -ciE 'ibwarn|warn|fatal|panic' "$TRANSCRIPT" || true)
record B1 "The twin runs ibsim on the model's InfiniBand fabric (ibsim adapter)" \
	"ibsim running, topology parsed with no warnings: $SWITCHES switches, $CAS CA ports, $LINKS links" \
	"running=$R_RUN ibsim $R_VER pid $R_PID; $parsed, $warns warnings; $R_SW switches, $R_CA CA ports, $R_LINKS links" \
	"$([ "$R_RUN" = True ] && kill -0 "$R_PID" && grep -q 'Network simulator ready' "$TRANSCRIPT" && [ "$warns" = 0 ] && [ "$R_SW/$R_CA/$R_LINKS" = $SWITCHES/$CAS/$LINKS ] && echo PASS || echo FAIL)"

twin report >"$EV/fidelity-report.txt" 2>&1
line=$(grep -o 'backend-ib: SIMULATED by ibsim[^;]*' "$EV/fidelity-report.txt" | head -1)
record B2 "The fidelity report labels the fabric" "backend-ib: SIMULATED by ibsim …; substrate-owned: port state, LIDs, forwarding tables" \
	"${line:-missing}; $(grep -c 'held by ibsim and set by the stack' "$EV/fidelity-report.txt") substrate-owned line" \
	"$([ -n "$line" ] && grep -q 'held by ibsim and set by the stack' "$EV/fidelity-report.txt" && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- OpenSM at the UFM appliance's HCA

OSMDIR=$W/osm
OSM_LOG=$OSMDIR/osm.log
LFTS=$OSMDIR/opensm-lfts.dump
mkdir -p "$OSMDIR"
# OpenSM's defaults, but its log flushed per message (force_log_flush; the checks below read it as
# it is written) and log flags 0x43: errors, info and routing, which also dumps the LFTs and the
# subnet (opensm-lfts.dump, opensm-subnet.lst) at every sweep
printf 'force_log_flush TRUE\nlog_flags 0x43\n' >"$OSMDIR/opensm.conf"
t0=$(now_ms)
(cd "$OSMDIR" && SIM_HOST=$SM_HOST OSM_CACHE_DIR=$OSMDIR OSM_TMP_DIR=$OSMDIR LD_PRELOAD=$IBSIM_PRELOAD \
	exec opensm -F "$OSMDIR/opensm.conf" -f "$OSM_LOG" >"$OSMDIR/opensm.out" 2>&1) &
OSM_PID=$!
osm_up() { grep -q 'SUBNET UP' "$OSM_LOG" 2>/dev/null; }
wait_until 60 osm_up >/dev/null
up_ms=$(($(now_ms) - t0))
record S1 "OpenSM (SIM_HOST $SM_HOST, the UFM appliance's ib0) becomes the master SM" "Entering MASTER state; SUBNET UP" \
	"$(grep -o 'Entering MASTER state' "$OSM_LOG" | head -1); $(grep -c 'SUBNET UP' "$OSM_LOG") SUBNET UP, ${up_ms} ms after start" \
	"$(grep -q 'Entering MASTER state' "$OSM_LOG" && osm_up && echo PASS || echo FAIL)"

mad "$TRAY7" sminfo >"$EV/S2-sminfo.txt" 2>&1
SMLID=$(sed -n 's/.*sm lid \([0-9]*\).*/\1/p' "$EV/S2-sminfo.txt")
record S2 "sminfo from a01-compute-tray-07 HCA-1" "state 3 SMINFO_MASTER, sm guid = the UFM appliance's ib0 (0x$SMPG)" "$(head -1 "$EV/S2-sminfo.txt")" \
	"$(grep -q "sm guid 0x$SMPG.*SMINFO_MASTER" "$EV/S2-sminfo.txt" && echo PASS || echo FAIL)"

mad "$TRAY7" ibstat >"$EV/S3-ibstat-tray07.txt" 2>&1
st=$(awk -F': ' '/State:/{s=$2} /Physical state:/{p=$2} /Base lid:/{l=$2} /SM lid:/{m=$2} /Port GUID:/{g=$2} END{print s"/"p", lid "l", SM lid "m", port GUID "g}' "$EV/S3-ibstat-tray07.txt")
LID7=$(awk -F': ' '/Base lid:/{print $2}' "$EV/S3-ibstat-tray07.txt")
record S3 "ibstat at a01-compute-tray-07 HCA-1" "Active/LinkUp, a LID, SM lid $SMLID, port GUID 0x$PG7 (the model's cx8-0)" "$st" \
	"$(grep -q 'State: Active' "$EV/S3-ibstat-tray07.txt" && [ "${LID7:-0}" -gt 0 ] && grep -q "SM lid: $SMLID" "$EV/S3-ibstat-tray07.txt" && grep -q "Port GUID: 0x$PG7" "$EV/S3-ibstat-tray07.txt" && echo PASS || echo FAIL)"

# one CA on every rail leaf: the master SM configured it
ibnet islands "$NET" >"$EV/islands.txt"
n=0 good=0
for leaf in $(grep -o '"c03-backend-ib-rail-[a-z0-9-]*"' "$NET" | sort -u | tr -d '"'); do
	host=$(python3 -I - "$NET" "$leaf" "$W/compat/opensm/ibnet.py" <<'EOF'
import importlib.util, sys
spec = importlib.util.spec_from_file_location("ibnet", sys.argv[3]); m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
nodes = m.parse(sys.argv[1])
leaf = next(nid for nid, n in nodes.items() if n["desc"] == sys.argv[2])
print(next(peer for p, (peer, _) in sorted(nodes[leaf]["ports"].items()) if nodes[peer]["kind"] == "Ca"))
EOF
	)
	n=$((n + 1))
	{ echo "== $leaf ($host)"; mad "$host" sminfo; mad "$host" ibstat; } >>"$EV/S4-every-rail.txt" 2>&1
	mad "$host" sminfo 2>/dev/null | grep -q "sm lid $SMLID sm guid 0x$SMPG.*SMINFO_MASTER" &&
		mad "$host" ibstat 2>/dev/null | grep -q 'State: Active' && good=$((good + 1))
done
record S4 "One subnet: a CA on every rail leaf" "$(wc -l <"$EV/islands.txt") connected fabric; on each of the 20 rail leaves a CA Active, configured by the master (sminfo: SM lid $SMLID, the UFM's GUID)" \
	"$(wc -l <"$EV/islands.txt") connected component ($(awk '{print $2" switches, "$3" CA ports, "$4" links"}' "$EV/islands.txt")); $good of $n rail leaves" \
	"$([ "$(wc -l <"$EV/islands.txt")" = 1 ] && [ "$n" = 20 ] && [ "$good" = 20 ] && echo PASS || echo FAIL)"

mad "$TRAY7" iblinkinfo --switches-only >"$EV/S5-iblinkinfo.txt" 2>&1
act=$(grep -c 'Active/  LinkUp' "$EV/S5-iblinkinfo.txt")
record S5 "iblinkinfo: every link of the fabric" "$((CAS + 2 * ISL)) switch ports Active/LinkUp ($CAS HCA links, $ISL leaf-spine links at both ends), none Initialize" \
	"$act Active/LinkUp, $(grep -c 'Initialize' "$EV/S5-iblinkinfo.txt") Initialize; $(grep -m1 -o '4X *[0-9.]* Gbps' "$EV/S5-iblinkinfo.txt")" \
	"$([ "$act" = $((CAS + 2 * ISL)) ] && ! grep -q Initialize "$EV/S5-iblinkinfo.txt" && echo PASS || echo FAIL)"

mad "$TRAY7" ibnetdiscover >"$EV/S6-ibnetdiscover.txt" 2>"$EV/S6-ibnetdiscover.err"
cmp=$(ibnet compare "$NET" "$EV/S6-ibnetdiscover.txt")
echo "$cmp" >"$EV/S6-compare.json"
record S6 "ibnetdiscover of the whole fabric against the model's topology" "$SWITCHES switches, $CAS CA ports, $LINKS links; node and port GUIDs, port numbers, descriptions equal" \
	"$(brief "$cmp")" "$(echo "$cmp" | grep -q '"equal": true' && echo PASS || echo FAIL)"
DISC0=$EV/S6-ibnetdiscover.txt

lids=$(ibnet lids "$DISC0")
cp "$LFTS" "$OSMDIR"/opensm-subnet.lst "$EV/" 2>/dev/null
lft=$(ibnet lft "$NET" "$LFTS" --focus c03-backend-ib-rail-a01-1)
echo "$lft" >"$EV/S7-lft.json"
record S7 "LIDs and routing: OpenSM's LFT dump against the model's cabling" "$((SWITCHES + CAS)) LIDs, one per node; every switch holds all of them and routes every CA LID on a minimum-hop path ($((SWITCHES * CAS)) routes)" \
	"$lids distinct LIDs; $(j "$lft" 'r["switches"]') switches holding $(j "$lft" 'r["lidsPerSwitch"]') LIDs each; $(j "$lft" 'r["minhop"]') of $(j "$lft" 'r["caRoutes"]') CA routes minimum-hop $(j "$lft" 'r["wrong"][:1]')" \
	"$([ "$lids" = $((SWITCHES + CAS)) ] && [ "$(j "$lft" 'r["lidsPerSwitch"]')" = "[$((SWITCHES + CAS))]" ] && [ "$(j "$lft" 'r["minhop"]')" = $((SWITCHES * CAS)) ] && [ "$(j "$lft" 'r["caRoutes"]')" = $((SWITCHES * CAS)) ] && echo PASS || echo FAIL)"

# through the spines: leaf a01-1 spreads the CAs it does not cable over its two uplinks, and a
# path from a01-1 to a server on rail b05-4 crosses a spine
DEST=$(ibnet node "$NET" "b05-server-02 HCA-8" | awk '{print $2}')
DESTLID=$(ibnet lid "$DISC0" "$DEST")
mad "$TRAY1" ibtracert "$LID7" "$DESTLID" >"$EV/S8-ibtracert.txt" 2>&1
hops=$(grep -o '"c03-backend-ib-[a-z0-9-]*"' "$EV/S8-ibtracert.txt" | tr -d '"' | sed 's/c03-backend-ib-//' | tr '\n' ' ')
record S8 "Routing through the spines" "leaf a01-1 sends the 174 CA LIDs it does not cable out of uplinks 33 and 34 (spines 1 and 2), split; ibtracert tray-07 -> b05-server-02 HCA-8: rail-a01-1, a spine, rail-b05-4" \
	"leaf a01-1 uplinks (port: CA LIDs) $(j "$lft" 'r["focus"]'); ibtracert lid $LID7 -> $DESTLID via: $hops" \
	"$([ "$(j "$lft" 'sorted(r["focus"])')" = "['33', '34']" ] && [ "$(j "$lft" 'sum(r["focus"].values())')" = 174 ] && echo "$hops" | grep -qE '^rail-a01-1 spine-[12] rail-b05-4 $' && echo PASS || echo FAIL)"

mad "$TRAY1" saquery NR "$LID7" >"$EV/S9-saquery-NR-tray07.txt" 2>&1
mad "$TRAY7" smpquery portinfo "$LID7" 1 >"$EV/S9-smpquery-portinfo.txt" 2>&1
record S9 "The SA and PortInfo" "the SA's NodeRecord for LID $LID7 is tray-07's; PortInfo LinkState Active" \
	"SA: $(grep -o 'port_guid\.*0x[0-9a-f]*' "$EV/S9-saquery-NR-tray07.txt") $(grep -o 'NodeDescription\.*.*' "$EV/S9-saquery-NR-tray07.txt" | sed 's/\.\.*/ /'); $(grep -m1 -o 'LinkState:\.*[A-Za-z]*' "$EV/S9-smpquery-portinfo.txt")" \
	"$(grep -q "port_guid\.*0x$PG7" "$EV/S9-saquery-NR-tray07.txt" && grep -q 'LinkState:\.*Active' "$EV/S9-smpquery-portinfo.txt" && echo PASS || echo FAIL)"
mad "$TRAY7" saquery NR >"$EV/S9b-saquery-NR-table.txt" 2>&1
nrec=$(grep -ci 'NodeRecord dump' "$EV/S9b-saquery-NR-table.txt")
record S9b "An SA table query: saquery NR with no filter" "$((SWITCHES + CAS)) NodeRecords (every node)" \
	"$nrec NodeRecord(s): libumad2sim carries no RMPP (ibsim's TODO), so an SA response longer than one MAD reaches the client as its first segment" \
	"$([ "$nrec" = $((SWITCHES + CAS)) ] && echo PASS || { [ "$nrec" -ge 1 ] && echo CANNOT || echo FAIL; })"

mad "$TRAY1" perfquery "$LID7" 1 >"$EV/S10-perfquery.txt" 2>&1
record S10 "perfquery of tray-07's port (PerfMgr MADs answered by ibsim)" "PortCounters answered (ibsim's counters, not the model's)" \
	"$(grep -c '^Port\|^Symbol\|^Link' "$EV/S10-perfquery.txt") counter lines, e.g. $(grep -m1 -o 'PortXmitData:\.*[0-9]*' "$EV/S10-perfquery.txt")" \
	"$(grep -q 'PortXmitData' "$EV/S10-perfquery.txt" && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- UFM REST against what OpenSM did

sleep 1 # UFM reads ibsim at most twice a second
ufm=$(ibnet ufm "$UFMCREDS" "$NET" "$DISC0")
echo "$ufm" >"$EV/U-ufm-vs-discovered.json"
u() { j "$ufm" "$1"; }
record U1 "UFM REST against the discovered fabric: channel adapter ports" "$CAS computer ports, each discovered with the same port GUID, node description and system GUID" \
	"$(u 'r["ufmComputerPorts"]') UFM computer ports; $(u 'r["caDiscovered"]') discovered; descriptions equal $(u 'r["caDescEqual"]'), system GUIDs equal $(u 'r["caSysEqual"]')" \
	"$([ "$(u 'r["ufmComputerPorts"]')/$(u 'r["caDiscovered"]')/$(u 'r["caDescEqual"]')/$(u 'r["caSysEqual"]')" = $CAS/$CAS/$CAS/$CAS ] && echo PASS || echo FAIL)"
record U2 "UFM REST against the discovered fabric: switch GUIDs" "64-bit GUIDs (16 hex digits), each a discovered switch's node GUID" \
	"systemID lengths (hex digits: ports) $(u 'r["switchSysGuidLengths"]'); a discovered switch's GUID for $(u 'r["switchDiscovered"]') of $(u 'r["ufmSwitchPorts"]') switch ports" \
	"$([ "$(u 'r["switchSysGuidLengths"]')" = "{'16': $((CAS + 2 * ISL))}" ] && [ "$(u 'r["switchDiscovered"]')" = $((CAS + 2 * ISL)) ] && echo PASS || echo FAIL)"
record U3 "UFM REST LIDs against OpenSM's (the ufm adapter reads the SM's LIDs from ibsim)" "every port reports the LID the SM assigned: $CAS CA ports, $((CAS + 2 * ISL)) switch ports" \
	"CA ports $(u 'r["caLidEqual"]') of $(u 'r["caDiscovered"]'); switch ports $(u 'r["switchLidEqual"]') of $(u 'r["switchDiscovered"]'); e.g. $(u 'r["examples"]["lid"]'); $(u 'r["examples"]["switchLid"]')" \
	"$([ "$(u 'r["caLidEqual"]')/$(u 'r["switchLidEqual"]')" = "$CAS/$((CAS + 2 * ISL))" ] && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- the model changes, the SM sees it

osm_mark() { wc -l <"$OSM_LOG"; }
osm_has() { tail -n +$(($1 + 1)) "$OSM_LOG" | grep -q "$2"; }
tr_mark() { wc -l <"$TRANSCRIPT"; }
tr_has() { tail -n +$(($1 + 1)) "$TRANSCRIPT" | grep -qF "$2"; }
tr_count() { [ "$(tail -n +$(($1 + 1)) "$TRANSCRIPT" | grep -c "$2")" -ge "$3" ]; }
# lid_routed <lid>: how many switches' LFTs hold it
lid_routed() { grep -ci "^0x$(printf %04x "$1") " "$LFTS"; }
lid_everywhere() { [ "$(lid_routed "$1")" = "$SWITCHES" ]; }
lid_nowhere() { [ "$(lid_routed "$1")" = 0 ]; }
leaf_routes() { python3 -I - "$LFTS" "$1" "$2" <<'EOF'
import re, sys
# leaf_routes <dump> <switch desc> <lid>: the port that switch routes the LID out of
cur = None
for line in open(sys.argv[1]):
    m = re.match(r"Unicast lids .*\('([^']*)'\)", line)
    if m:
        cur = m.group(1)
    elif cur == sys.argv[2] and line.lower().startswith("0x%04x " % int(sys.argv[3])):
        print(int(line.split()[1]))
EOF
}
ufm_port() {
	python3 -I - "$UFMCREDS" "$1" <<'EOF'
import base64, json, ssl, sys, urllib.request
c = json.load(open(sys.argv[1]))[0]
ctx = ssl.create_default_context(); ctx.check_hostname = False; ctx.verify_mode = ssl.CERT_NONE
req = urllib.request.Request(c["url"] + "/ufmRest/resources/ports")
req.add_header("Authorization", "Basic " + base64.b64encode(f"{c['user']}:{c['password']}".encode()).decode())
for p in json.load(urllib.request.urlopen(req, context=ctx)):
    if p["guid"] == sys.argv[2]:
        print(p["logical_state"] + "/" + p["physical_state"] + " lid " + str(p["lid"]))
EOF
}

om=$(osm_mark)
tm=$(tr_mark)
twin cable pull site-1/hall-a/a01/compute-tray-07:cx8-0 >"$EV/L1-pull.json" 2>&1
ms=$(wait_until 20 osm_has "$om" "Removed port with GUID:0x$PG7") || true
wait_until 20 lid_nowhere "$LID7" >/dev/null
tail -n +$((om + 1)) "$OSM_LOG" >"$EV/L1-osm.log"
record L1 "dctwin cable pull site-1/hall-a/a01/compute-tray-07:cx8-0" \
	"console Unlink at the leaf's port $SWP7; OpenSM gets trap 128, removes the port, SUBNET UP; LID $LID7 leaves every switch's LFT" \
	"$(grep -o '> Unlink [^ ]*' "$TRANSCRIPT" | tail -1); $(grep -o 'num:128 (Link state change)' "$EV/L1-osm.log" | head -1); $(grep -o "Removed port with GUID:0x$PG7 LID range \[[0-9, ]*\]" "$EV/L1-osm.log" | head -1); $(grep -c 'SUBNET UP' "$EV/L1-osm.log") SUBNET UP, ${ms} ms after the pull; LID $LID7 in $(lid_routed "$LID7") LFTs" \
	"$(tr_has "$tm" "Unlink \"$LEAF1\"[$SWP7]" && grep -q 'num:128' "$EV/L1-osm.log" && grep -q "Removed port with GUID:0x$PG7" "$EV/L1-osm.log" && grep -q 'SUBNET UP' "$EV/L1-osm.log" && lid_nowhere "$LID7" && echo PASS || echo FAIL)"

mad "$TRAY1" iblinkinfo --switches-only >"$EV/L2-iblinkinfo.txt" 2>&1
mad "$TRAY1" ibnetdiscover >"$EV/L2-ibnetdiscover.txt" 2>/dev/null
cas=$(grep -c '^Ca' "$EV/L2-ibnetdiscover.txt")
LEAF1LID=$(grep -E "^Switch.*\"$LEAF1\"" "$DISC0" | grep -o 'lid [0-9]*' | awk '{print $2}')
portline=$(grep -E "^ +$LEAF1LID +$SWP7\[" "$EV/L2-iblinkinfo.txt" | head -1 | sed -E 's/ +/ /g')
sleep 1
us=$(ufm_port "$PG7")
record L2 "After the pull: the fabric as OpenSM and diags see it, and UFM" \
	"leaf a01-1 port $SWP7 Down/Polling; $((CAS - 1)) CAs discovered; UFM Down/Link Down" \
	"port:${portline}; $cas CAs; UFM $us" \
	"$(echo "$portline" | grep -q 'Down/ Polling' && [ "$cas" = $((CAS - 1)) ] && echo "$us" | grep -q '^Down/Link Down' && echo PASS || echo FAIL)"

om=$(osm_mark)
tm=$(tr_mark)
twin cable replace site-1/hall-a/a01/compute-tray-07:cx8-0 >"$EV/L3-replace.json" 2>&1
ms=$(wait_until 20 lid_everywhere "$LID7") || true
tail -n +$((om + 1)) "$OSM_LOG" >"$EV/L3-osm.log"
mad "$TRAY7" ibstat >"$EV/L3-ibstat.txt" 2>&1
mad "$TRAY7" ibnetdiscover >"$EV/L3-ibnetdiscover.txt" 2>/dev/null
lid=$(awk -F': ' '/Base lid:/{print $2}' "$EV/L3-ibstat.txt")
sleep 1
us=$(ufm_port "$PG7")
record L3 "dctwin cable replace (the same port)" "console Link; trap 128; Active with the same LID $LID7 (guid2lid), in every switch's LFT again, out of port $SWP7 at the leaf; $CAS CAs; UFM Active, lid $LID7" \
	"$(grep -o '> Link [^ ]* [^ ]*' "$TRANSCRIPT" | tail -1); $(grep -c 'num:128' "$EV/L3-osm.log") trap(s); $(grep -o 'State: [A-Za-z]*' "$EV/L3-ibstat.txt" | head -1), LID $lid; in $(lid_routed "$LID7") LFTs after ${ms} ms, leaf port $(leaf_routes c03-backend-ib-rail-a01-1 "$LID7"); $(grep -c '^Ca' "$EV/L3-ibnetdiscover.txt") CAs; UFM $us" \
	"$(tr_has "$tm" "Link \"$LEAF1\"[$SWP7] \"$TRAY7\"[1]" && grep -q 'State: Active' "$EV/L3-ibstat.txt" && [ "$lid" = "$LID7" ] && lid_everywhere "$LID7" && [ "$(leaf_routes c03-backend-ib-rail-a01-1 "$LID7")" = "$SWP7" ] && [ "$(grep -c '^Ca' "$EV/L3-ibnetdiscover.txt")" = $CAS ] && [ "$us" = "Active/Link Up lid $LID7" ] && echo PASS || echo FAIL)"

# a host powered off: its four HCAs, one per rail, all in the master's subnet
PG8S=""
for r in 1 2 3 4; do PG8S="$PG8S $(ibnet node "$NET" "a01-compute-tray-08 HCA-$r" | awk '{print $2}')"; done
removed() { local n=0 g; for g in $PG8S; do osm_has "$1" "Removed port with GUID:0x$g" && n=$((n + 1)); done; echo $n; }
all_removed() { [ "$(removed "$1")" = 4 ]; }
LIDS8=""
for g in $PG8S; do LIDS8="$LIDS8 $(ibnet lid "$DISC0" "$g")"; done
om=$(osm_mark)
tm=$(tr_mark)
twin power off site-1/hall-a/a01/compute-tray-08 >"$EV/L4-power-off.json" 2>&1
wait_until 30 tr_count "$tm" '> Unlink' 4 >/dev/null || true
wait_until 30 all_removed "$om" >/dev/null || true
unl=$(tail -n +$((tm + 1)) "$TRANSCRIPT" | grep -c '> Unlink')
gone=0
for l in $LIDS8; do wait_until 10 lid_nowhere "$l" >/dev/null && gone=$((gone + 1)); done
record L4 "dctwin power off site-1/hall-a/a01/compute-tray-08" "4 console Unlinks (one per rail); OpenSM removes all four HCAs; their LIDs ($LIDS8 ) leave every LFT" \
	"$unl Unlinks; $(removed "$om") of 4 HCAs removed; $gone of 4 LIDs gone from every LFT" \
	"$([ "$unl" = 4 ] && [ "$(removed "$om")" = 4 ] && [ "$gone" = 4 ] && echo PASS || echo FAIL)"

om=$(osm_mark)
tm=$(tr_mark)
twin power on site-1/hall-a/a01/compute-tray-08 >"$EV/L5-power-on.json" 2>&1
wait_until 30 tr_count "$tm" '> Link' 4 >/dev/null || true
back=0 same=0
for r in 1 2 3 4; do
	read -r h g _ <<<"$(ibnet node "$NET" "a01-compute-tray-08 HCA-$r")"
	want=$(ibnet lid "$DISC0" "$g")
	wait_until 30 lid_everywhere "$want" >/dev/null && back=$((back + 1))
	mad "$h" ibstat 2>/dev/null | grep -q 'State: Active' && [ "$(mad "$h" ibstat 2>/dev/null | awk -F': ' '/Base lid:/{print $2}')" = "$want" ] && same=$((same + 1))
done
lnk=$(tail -n +$((tm + 1)) "$TRANSCRIPT" | grep -c '> Link')
record L5 "dctwin power on site-1/hall-a/a01/compute-tray-08" "4 console Links; all four HCAs Active with the LIDs they had, routed by every switch" \
	"$lnk Links; $same of 4 Active with their LIDs; $back of 4 in every LFT" \
	"$([ "$lnk" = 4 ] && [ "$same" = 4 ] && [ "$back" = 4 ] && echo PASS || echo FAIL)"

# rerouting: leaf a01-1 loses its uplink to spine 1
EX=$(j "$lft" 'r["example"]["33"]') # a CA LID leaf a01-1 sent through spine 1
mad "$TRAY1" ibtracert "$LID7" "$EX" >"$EV/L6-ibtracert-before.txt" 2>&1
om=$(osm_mark)
tm=$(tr_mark)
twin cable pull site-1/hall-a/c03/backend-ib-rail-a01-1:ib33 >"$EV/L6-pull.json" 2>&1
via34() { [ "$(leaf_routes c03-backend-ib-rail-a01-1 "$EX")" = 34 ]; }
ms=$(wait_until 30 via34) || true
sleep 1
lft6=$(ibnet lft "$NET" "$LFTS" --focus c03-backend-ib-rail-a01-1 --without c03-backend-ib-rail-a01-1:33)
echo "$lft6" >"$EV/L6-lft-uplink-pulled.json"
cp "$LFTS" "$EV/L6-lfts-uplink-pulled.dump"
mad "$TRAY1" ibtracert "$LID7" "$EX" >"$EV/L6-ibtracert-after.txt" 2>&1
before=$(grep -o '"c03-backend-ib-[a-z0-9-]*"' "$EV/L6-ibtracert-before.txt" | tr -d '"' | sed 's/c03-backend-ib-//' | tr '\n' ' ')
after=$(grep -o '"c03-backend-ib-[a-z0-9-]*"' "$EV/L6-ibtracert-after.txt" | tr -d '"' | sed 's/c03-backend-ib-//' | tr '\n' ' ')
record L6 "Rerouting: dctwin cable pull site-1/hall-a/c03/backend-ib-rail-a01-1:ib33 (leaf a01-1's uplink to spine 1)" \
	"console Unlink; OpenSM reroutes: leaf a01-1 sends every CA it does not cable out of port 34; every route minimum-hop on the fabric without that cable; LID $EX's path moves to spine 2" \
	"$(grep -o '> Unlink [^ ]*' "$TRANSCRIPT" | tail -1); rerouted ${ms} ms after the pull; leaf a01-1 uplinks $(j "$lft6" 'r["focus"]'); $(j "$lft6" 'r["minhop"]') of $(j "$lft6" 'r["caRoutes"]') routes minimum-hop $(j "$lft6" 'r["wrong"][:1]'); lid $LID7 -> $EX: before ${before}, after ${after}" \
	"$(tr_has "$tm" "Unlink \"$LEAF1\"[33]" && [ "$(j "$lft6" 'r["focus"]')" = "{'34': 174}" ] && [ "$(j "$lft6" 'r["minhop"]')" = $((SWITCHES * CAS)) ] && echo "$before" | grep -q 'spine-1' && echo "$after" | grep -q 'spine-2' && ! echo "$after" | grep -q 'spine-1' && echo PASS || echo FAIL)"

tm=$(tr_mark)
twin cable replace site-1/hall-a/c03/backend-ib-rail-a01-1:ib33 >"$EV/L7-replace.json" 2>&1
split() { [ "$(ibnet lft "$NET" "$LFTS" --focus c03-backend-ib-rail-a01-1 | python3 -I -c 'import json,sys; print(sorted(json.load(sys.stdin)["focus"]))')" = "['33', '34']" ]; }
ms=$(wait_until 30 split) || true
lft7=$(ibnet lft "$NET" "$LFTS" --focus c03-backend-ib-rail-a01-1)
record L7 "dctwin cable replace of the uplink" "console Link; both uplinks carry routes again; every route minimum-hop on the whole fabric" \
	"$(grep -o '> Link [^ ]* [^ ]*' "$TRANSCRIPT" | tail -1); after ${ms} ms leaf a01-1 uplinks $(j "$lft7" 'r["focus"]'); $(j "$lft7" 'r["minhop"]') of $(j "$lft7" 'r["caRoutes"]') minimum-hop" \
	"$(tr_has "$tm" "Link \"$LEAF1\"[33]" && split && [ "$(j "$lft7" 'r["minhop"]')" = $((SWITCHES * CAS)) ] && echo PASS || echo FAIL)"

# the leaf the UFM appliance hangs off loses AC (the lab's time scale makes it a 100 ms outage):
# its 18 trays, the SM's own port and its two uplinks go down and come back
om=$(osm_mark)
tm=$(tr_mark)
twin power ac-cycle site-1/hall-a/c03/backend-ib-rail-a01-1 >"$EV/L8-ac-cycle.json" 2>&1
wait_until 30 tr_count "$tm" '> Link' 21 >/dev/null || true
fabric_whole() { [ "$(mad "$TRAY7" iblinkinfo --switches-only 2>/dev/null | grep -c 'Active/  LinkUp')" = $((CAS + 2 * ISL)) ]; }
ms=$(wait_until 60 fabric_whole) || true
tail -n +$((om + 1)) "$OSM_LOG" >"$EV/L8-osm.log"
tail -n +$((tm + 1)) "$TRANSCRIPT" >"$EV/L8-transcript.log"
unl=$(grep -c '> Unlink' "$EV/L8-transcript.log")
lnk=$(grep -c '> Link' "$EV/L8-transcript.log")
mad "$TRAY7" ibnetdiscover >"$EV/L8-ibnetdiscover.txt" 2>/dev/null
cmp=$(ibnet compare "$NET" "$EV/L8-ibnetdiscover.txt")
kept=$(ibnet lidsame "$DISC0" "$EV/L8-ibnetdiscover.txt")
record L8 "dctwin power ac-cycle site-1/hall-a/c03/backend-ib-rail-a01-1 (the SM's own leaf)" "21 Unlinks then 21 Links (18 trays, the UFM, 2 uplinks); OpenSM loses its port and re-sweeps; the fabric whole again, equal to the model, every LID kept" \
	"$unl Unlinks, $lnk Links; $(grep -c 'num:128' "$EV/L8-osm.log") trap(s), $(grep -c 'Removed port' "$EV/L8-osm.log") ports removed; $(grep -o 'SM port is down' "$EV/L8-osm.log" | head -1), then $(grep -o 'SM port is up' "$EV/L8-osm.log" | head -1), $(grep -c 'Entering MASTER state' "$EV/L8-osm.log") MASTER, $(grep -c 'SUBNET UP' "$EV/L8-osm.log") SUBNET UP; whole after ${ms} ms; $(brief "$cmp" | sed 's/^[^;]*; //'); LIDs kept: $kept" \
	"$([ "$unl" = 21 ] && [ "$lnk" = 21 ] && echo "$cmp" | grep -q '"equal": true' && [ "$kept" = "$((SWITCHES + CAS)) of $((SWITCHES + CAS))" ] && fabric_whole && echo PASS || echo FAIL)"

twin report >"$EV/fidelity-report-end.txt" 2>&1
applied=$(grep -o '[0-9]* link change(s) applied through the console[^;]*' "$EV/fidelity-report-end.txt" | head -1)
ufmline=$(grep -o 'backend-ib: UFM REST[^;]*' "$EV/fidelity-report-end.txt" | grep -o 'LIDs the subnet manager assigned, read from ibsim' | head -1)
record R1 "The fidelity report after the run" "54 link changes applied (1 + 1 + 4 + 4 + 1 + 1 + 42), none refused; UFM REST serving the SM's LIDs" \
	"${applied:-missing}; ufm: ${ufmline:-its own LIDs}" \
	"$([ "$applied" = "54 link change(s) applied through the console" ] && [ -n "$ufmline" ] && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- down

kill "$OSM_PID" 2>/dev/null
wait "$OSM_PID" 2>/dev/null
OSM_PID=
twin_down
sleep 0.5
record C1 "dctwin down stops the ibsim it started" "no ibsim process" "$(pgrep -c ibsim || true) ibsim process(es); transcript ends: $(tail -c 40 "$TRANSCRIPT" | tr '\n' ' ')" \
	"$(pgrep ibsim >/dev/null && echo FAIL || echo PASS)"

cp "$TRANSCRIPT" "$EV/ibsim-transcript.log"
cp "$OSM_LOG" "$EV/opensm-master.log"
cp "$W/twin/up.log" "$EV/twin-up.log" 2>/dev/null
print_table "compat/opensm — $VERSIONS" >"$OUT/results.md"
rc=$?
cat "$OUT/results.md"
exit "$rc"

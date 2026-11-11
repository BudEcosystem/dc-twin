#!/usr/bin/env bash
# The critic through the CLI, end to end (docs/spec/17-critic.md): the reference stack with a
# seeded defect (MUTANT, default wrong-tray) against the NVL72 estate.
#   1. a scenario run judged by the critic, with JSON, JUnit and Markdown written
#   2. an open session: a fault injected by hand, the stack's reaction judged from the events
#   3. the critic as an MCP server, asked by an agent for the judgement and an explanation
#   4. the estate down, the recorded log re-judged offline, also under a stricter policy
# Addresses are chosen free, so it runs beside other twins.
set -uo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
work=${WORK:-$(mktemp -d)}
mutant=${MUTANT-wrong-tray}
bin="$work/bin"; mkdir -p "$bin"
( cd "$root" && go build -o "$bin/dctwin" ./cmd/dctwin && go build -o "$bin/refstack" ./cmd/refstack ) || exit 1
free() { python3 -c 'import socket,sys; s=socket.socket(socket.AF_INET, socket.SOCK_DGRAM if sys.argv[1]=="udp" else socket.SOCK_STREAM); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])' "$1"; }
gw=$(free tcp); api=$(free tcp); broker=$(free tcp); dhcp=$(free udp)
state="$work/state"; rm -rf "$state"; mkdir -p "$state"
sed -e "s/127.0.0.1:18080/127.0.0.1:$api/g; s/127.0.0.1:18830/127.0.0.1:$broker/g; s/127.0.0.1:16767/127.0.0.1:$dhcp/g" \
  "$root/examples/estates/nvl72-refstack.yaml" > "$work/estate.yaml"
"$bin/refstack" --gateway "127.0.0.1:$gw" --api "127.0.0.1:$api" --broker "127.0.0.1:$broker" --dhcp "127.0.0.1:$dhcp" \
  --expected "$state/stack-inputs/expected-machines.json" ${mutant:+--mutant "$mutant"} > "$work/refstack.log" 2>&1 &
ref=$!
sleep 1
"$bin/dctwin" up "$work/estate.yaml" --state "$state" --gateway "127.0.0.1:$gw" --adapters redfish,dhcp,bms,snmp,ipmi,ufm,nvue > "$work/up.log" 2>&1 &
for _ in $(seq 1 80); do [ -f "$state/control.json" ] && break; sleep 0.25; done
cleanup() { DCTWIN_STATE="$state" "$bin/dctwin" down >/dev/null 2>&1 || true; kill $ref 2>/dev/null || true; }
trap cleanup EXIT
export DCTWIN_STATE="$state"
d="$bin/dctwin"

echo "=== 1. a judged scenario run (mutant: ${mutant:-none})"
"$d" scenario run "$root/examples/scenarios/nvl72-leak-response.yaml" --subject "refstack+${mutant:-none}" \
  --judgement "$work/leak.json" --junit "$work/leak.junit.xml" --markdown "$work/leak.md"
echo "exit code: $?"
echo "--- the first violation, explained"
"$d" critic explain "$work/leak.json" 1
echo "--- JUnit: $(grep -c '<testcase' "$work/leak.junit.xml") test cases, $(grep -c '<failure' "$work/leak.junit.xml") failures"

echo "=== 2. an open session: a tray leak injected by hand"
sid=$("$d" critic start --subject manual | sed -n 's/^critic session \([^ ]*\) started.*/\1/p')
echo "session $sid"
"$d" fault inject 'a03/compute-tray-12' generic/leak/tray-detector --id manual >/dev/null
sleep 20
"$d" fault clear manual >/dev/null
sleep 3
"$d" critic stop "$sid" --judgement "$work/session.json"
echo "exit code: $?"

echo "=== 3. the critic as an MCP server"
run=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["run"]["id"])' "$work/leak.json")
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"explain_violation\",\"arguments\":{\"run\":\"$run\",\"index\":1}}}" \
  | "$d" critic mcp > "$work/mcp.out"
python3 - "$work/mcp.out" <<'PY'
import json,sys
for line in open(sys.argv[1]):
    r=json.loads(line)
    if r["id"]==2: print("tools:", ", ".join(t["name"] for t in r["result"]["tools"]))
    if r["id"]==3: print(r["result"]["content"][0]["text"])
PY

echo "=== 4. offline: the recorded log re-judged with the estate down"
cleanup; trap - EXIT; sleep 1
"$d" critic judge "$state" --all --quiet --judgement "$work/offline.json"
echo "exit code: $?"
python3 - "$work/offline.json" "$work/leak.json" <<'PY'
import json,sys
off=json.load(open(sys.argv[1])); live=json.load(open(sys.argv[2]))
off = off if isinstance(off, list) else [off]
for j in off: print(f'  {j["run"]["id"]:44} {j["verdict"]:8} violations {sorted(set(v["rule"] for v in j["violations"]))}')
m=[j for j in off if j["run"]["id"]==live["run"]["id"]][0]
same = sorted(v["rule"] for v in m["violations"])==sorted(v["rule"] for v in live["violations"]) and m["verdict"]==live["verdict"]
print("  offline re-judgement of the scenario run equals the live one:", same)
PY
cat > "$work/strict.yaml" <<'YAML'
apiVersion: dc-twin/v1alpha1
kind: Policy
metadata: { name: demo/strict, version: 0.1.0 }
spec:
  extends: [reference/default]
  objectives:
    - { name: leak-seen-in-1s, measure: fault.exposed, where: 'fault.fault == "generic/leak/tray-detector"', pass: "<=1s", key: true }
YAML
echo "--- the same log under a stricter policy (a tray leak must be read within 1 s, key)"
"$d" critic judge "$state" --run "$run" --policy "$work/strict.yaml" | sed -n '1,4p;/objectives:/,/uncovered\|n\/a/p'
echo "exit code: ${PIPESTATUS[0]}"
echo "work: $work"

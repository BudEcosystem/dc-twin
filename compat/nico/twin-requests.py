#!/usr/bin/env python3
"""Summarise what the software under test asked the twin: requests from the stack in a twin
state directory's event log (events.jsonl), counted by canonical request name and device kind.

    twin-requests.py <state-dir> [--since SEQ] [--writes]
"""
import json
import re
import sys
from collections import Counter, defaultdict

args = sys.argv[1:]
if not args:
    sys.exit(__doc__)
state, since, writes = args[0], 0, "--writes" in args
if "--since" in args:
    since = int(args[args.index("--since") + 1])
by_name, by_kind, devices = Counter(), defaultdict(Counter), defaultdict(set)
first = last = None
with open(state + "/events.jsonl") as f:
    for line in f:
        try:
            e = json.loads(line)
        except ValueError:
            continue
        if e.get("seq", 0) <= since or e.get("kind") != "request":
            continue
        if (e.get("source") or {}).get("class") != "stack":
            continue
        r = e.get("request") or {}
        name = r.get("canonical") or "%s %s" % (r.get("method"), r.get("target"))
        if name == "other":
            # no canonical name: show the method and the resource, ids generalised
            target = (r.get("target") or "").split("?")[0]
            target = re.sub(r"/Managers/[^/]+$", "/Managers/{id}", target)
            name = "%s %s" % (r.get("method"), target)
        if writes and r.get("method") in ("GET", "HEAD"):
            continue
        path = e.get("path", "")
        kind = re.sub(r"-\d+$", "", path.rsplit("/", 1)[-1])
        by_name[name] += 1
        by_kind[name][kind] += 1
        devices[name].add(path)
        first = first or e.get("wall")
        last = e.get("wall")
total = sum(by_name.values())
print("%d requests from the stack%s (%s .. %s)" % (total, " (writes only)" if writes else "", first, last))
for name, n in by_name.most_common():
    kinds = ", ".join("%s %d" % kv for kv in by_kind[name].most_common())
    print("  %-34s %6d  on %d devices: %s" % (name, n, len(devices[name]), kinds))

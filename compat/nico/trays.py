#!/usr/bin/env python3
"""compat/nico/trays.py — read NICo's machines and racks for the trays path (run.sh trays).

  trays.py states              stdin: `nico-admin-cli -f json machine show`; prints one line per
                               machine: <id> <kind> <state>, kind host|dpu|predicted
  trays.py summary             stdin: the same; prints "<n> <kind> <state>" counts, one per line
  trays.py racks               stdin: `nico-admin-cli -f json rack show`; prints <rack> <state>

Only the standard library; run with python3 -I.
"""
import json
import sys

sys.path.insert(0, __file__.rsplit("/", 1)[0])
from ready import load, machines, machine_id, state_of  # noqa: E402


def kind(mid):
    # NICo's machine ids name their type: fm100h… a host, fm100d… a DPU, fm100p… a predicted host
    t = mid[5:6] if mid.startswith("fm100") else ""
    return {"h": "host", "d": "dpu", "p": "predicted"}.get(t, "other")


def rows(doc):
    for m in machines(doc):
        mid = machine_id(m)
        if mid:
            yield mid, kind(mid), state_of(m).replace(" ", "")


def main():
    cmd = sys.argv[1] if len(sys.argv) > 1 else ""
    doc = load(sys.stdin.read())
    if cmd == "states":
        for r in rows(doc):
            print(*r)
    elif cmd == "summary":
        counts = {}
        for _, k, s in rows(doc):
            counts[(k, s)] = counts.get((k, s), 0) + 1
        for (k, s), n in sorted(counts.items()):
            print(n, k, s)
    elif cmd == "racks":
        items = doc if isinstance(doc, list) else (doc or {}).get("racks") or (doc or {}).get("items") or []
        for r in items:
            if not isinstance(r, dict):
                continue
            rid = r.get("id")
            if isinstance(rid, dict):
                rid = rid.get("id") or rid.get("value")
            st = r.get("state") or r.get("status")
            if isinstance(st, dict):
                st = json.dumps(st, separators=(",", ":"))
            print(rid or r.get("name", "?"), st)
    else:
        print(__doc__)
        sys.exit(2)


if __name__ == "__main__":
    main()

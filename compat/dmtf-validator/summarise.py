#!/usr/bin/env python3
# Summarises one Redfish Service Validator debug log: the PASS/WARN/FAIL/NOT TESTED totals and
# every distinct failure (property + message, numeric path segments folded to #), how often it occurred and
# on which resources. With --baseline (the summary of the same capture served as captured, by
# capture_server.py), each failure is attributed: present in the capture itself, or added by the
# twin.
# Usage: summarise.py LABEL DEBUG.log [--json OUT] [--baseline BASELINE.json] [--warnings]
# Exit status 1 when the twin is at fault: any failure (no baseline), or one the capture lacks.
import collections
import json
import re
import sys

ANSI = re.compile(r"\x1b\[[0-9;]*m")
LINE = re.compile(r"^\S+ \S+ - redfish - (ERROR|WARNING) - (FAIL|WARN) - (.*)$")
VALIDATING = re.compile(r"^\S+ \S+ - redfish - INFO - Validating (.*?)\.\.\.$")
TOTALS = re.compile(r"\|\s*(\d+)\s*\|\s*(\d+)\s*\|\s*(\d+)\s*\|\s*(\d+)\s*\|")


def arg(name):
    return sys.argv[sys.argv.index(name) + 1] if name in sys.argv else None


def parse(path):
    resource, totals, resources = "", None, 0
    found = {"FAIL": collections.OrderedDict(), "WARN": collections.OrderedDict()}
    for raw in open(path, encoding="utf-8", errors="replace"):
        line = ANSI.sub("", raw.rstrip("\n"))
        m = VALIDATING.match(line)
        if m:
            resource, resources = m.group(1).rstrip("/") or "/", resources + 1
            continue
        m = LINE.match(line)
        if m:
            kind, msg = m.group(2), m.group(3)
            # "/Prop/3/Sub ([value]): Error text" -> "/Prop/#/Sub: Error text" (the value dropped,
            # array indices folded, member-specific names in the text kept)
            pm = re.match(r"^(\S*) \((.*)\): (.*)$", msg)
            fold = lambda x: re.sub(r"/[0-9]+(?=/|$|')", "/#", x)
            key = f"{fold(pm.group(1))}: {fold(pm.group(3))}" if pm else fold(msg)
            e = found[kind].setdefault(key, {"count": 0, "resources": []})
            e["count"] += 1
            if resource not in e["resources"]:
                e["resources"].append(resource)
            continue
        m = TOTALS.search(line)
        if m:
            totals = dict(zip(("pass", "warn", "fail", "not_tested"), map(int, m.groups())))
    return {"resources": resources, "totals": totals, "fail": found["FAIL"], "warn": found["WARN"]}


def where(rs):
    return ", ".join(rs[:3]) + (f" (+{len(rs) - 3} more)" if len(rs) > 3 else "")


def main():
    label, path = sys.argv[1], sys.argv[2]
    s = parse(path)
    t = s["totals"]
    print(f"== {label}: {s['resources']} resources validated; " +
          (", ".join(f"{k.upper().replace('_', ' ')} {v}" for k, v in t.items()) if t else "no totals (run incomplete?)"))
    base = json.load(open(arg("--baseline"))) if arg("--baseline") else None
    fails, twin = s["fail"], {}
    if base is None:
        print(f"  distinct FAIL: {len(fails)}")
        for key, e in fails.items():
            print(f"    [{e['count']:>3}x] {key}\n           at {where(e['resources'])}")
    else:
        bt = base["totals"] or {}
        print(f"  the capture served as captured: {base['resources']} resources; " +
              ", ".join(f"{k.upper().replace('_', ' ')} {v}" for k, v in bt.items()))
        twin, captured = collections.OrderedDict(), collections.OrderedDict()
        for key, e in fails.items():
            seen = set(base["fail"].get(key, {}).get("resources", []))
            mine = [r for r in e["resources"] if r not in seen]
            theirs = [r for r in e["resources"] if r in seen]
            if mine:
                twin[key] = mine
            if theirs:
                captured[key] = theirs
        gone = [k for k in base["fail"] if k not in fails]
        print(f"  distinct FAIL added by the twin: {len(twin)}")
        for key, rs in twin.items():
            print(f"    {key}\n           at {where(rs)}")
        print(f"  distinct FAIL already in the capture: {len(captured)}")
        for key, rs in captured.items():
            print(f"    {key}\n           at {where(rs)}")
        if gone:
            print(f"  distinct FAIL in the capture that the twin does not show: {len(gone)}")
            for key in gone:
                print(f"    {key}\n           at {where(base['fail'][key]['resources'])}")
    warns = s["warn"]
    if "--warnings" in sys.argv:
        print(f"  distinct WARN: {len(warns)}")
        for key, e in warns.items():
            print(f"    [{e['count']:>3}x] {key}\n           at {where(e['resources'])}")
    else:
        kinds = collections.Counter()
        for key, e in warns.items():
            kinds[key.split(": ", 1)[-1].split(":")[0]] += e["count"]
        print(f"  WARN by kind: " + "; ".join(f"{k} x{n}" for k, n in kinds.most_common()))
    if arg("--json"):
        json.dump({"label": label, **s}, open(arg("--json"), "w"), indent=2)
    if t is None or (twin if base is not None else fails):
        sys.exit(1)


if __name__ == "__main__":
    main()

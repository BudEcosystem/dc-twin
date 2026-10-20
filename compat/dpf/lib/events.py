"""Summarise what DPF asked of one DPU BMC, from the twin's event log (events.jsonl).

Each request is shown by canonical name with its method, resource and answer, consecutive
repeats folded into one row with a count; the state the BMC changed (the engine's and the
bluefield front's state events) is shown where it happened. Run with python3 -I.

    python3 -I events.py <events.jsonl> <device path> [markdown|text]
"""
import json
import sys


def main():
    path, device = sys.argv[1], sys.argv[2]
    fmt = sys.argv[3] if len(sys.argv) > 3 else "markdown"
    rows = []
    with open(path) as f:
        for line in f:
            try:
                e = json.loads(line)
            except ValueError:
                continue  # the line the twin is writing now
            if e.get("path") != device:
                continue
            src = e.get("source") or {}
            if e.get("kind") == "request" and e.get("request"):
                r = e["request"]
                if src.get("class") not in ("stack",):
                    who = "%s/%s" % (src.get("class", ""), src.get("principal", ""))
                else:
                    who = src.get("principal", "")
                method, target = r.get("method", ""), r.get("target", "")
                if not method:
                    # a request the BMC makes of its own engine (the bluefield front's ResetToDefaults,
                    # the Arm reset into a pushed BFB): no HTTP method
                    method, target = "(%s)" % r.get("protocol", ""), target or ""
                key = ("req", method, target, r.get("canonical", ""), str(r.get("status", "")), who)
            elif e.get("kind") == "state" and e.get("state"):
                s = e["state"]
                field = s.get("field", "")
                if field.startswith(("sensor.", "patch:")):
                    continue
                to = s.get("to")
                if field.startswith("account.") and field.endswith(".password"):
                    to = "[rotated]"
                key = ("state", field, str(s.get("from", "")), str(to), (e.get("message") or "")[:160], "%s/%s" % (src.get("class", ""), src.get("principal", "")))
            else:
                continue
            if rows and rows[-1][0] == key:
                rows[-1][1] += 1
                rows[-1][3] = e["seq"]
            else:
                rows.append([key, 1, e["seq"], e["seq"]])
    if fmt == "markdown":
        print("| seq | request or state | canonical / field | answer / change | by | n |")
        print("|---|---|---|---|---|---|")
    for key, n, first, last in rows:
        seq = str(first) if first == last else "%d–%d" % (first, last)
        if key[0] == "req":
            _, m, target, canon, status, who = key
            cells = [seq, "%s %s" % (m, target), canon, status, who, str(n)]
        else:
            _, field, frm, to, msg, who = key
            change = "%s → %s" % (frm or "·", to)
            if msg:
                change += " (" + msg + ")"
            cells = [seq, "state", field, change, who, str(n)]
        if fmt == "markdown":
            print("| " + " | ".join(c.replace("|", "\\|") for c in cells) + " |")
        else:
            print("  ".join(cells))


main()

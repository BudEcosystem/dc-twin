#!/usr/bin/env python3
"""Join NICo's site-explorer report with the twin's model: for every BMC the twin leased an
address to from NICo's DHCP server, what NICo explored there.

    nico-report.py <site-explorer get-report all JSON> <dctwin credentials JSON> [<expected-machines.json>]

Columns: twin device, the address NICo leased, NICo's endpoint type and vendor, the serial NICo
read (system, else the first chassis that has one), whether it is the twin's serial for that
device, and NICo's exploration error code and text.
"""
import json
import re
import sys
from collections import Counter

if len(sys.argv) < 3:
    sys.exit(__doc__)
report = json.load(open(sys.argv[1]))
creds = json.load(open(sys.argv[2]))
serials = {}
if len(sys.argv) > 3:
    for e in json.load(open(sys.argv[3])):
        serials[e["path"]] = e["serial"]
by_ip = {c["siteIP"]: c for c in creds if c.get("siteIP")}
eps = {e["address"]: e for e in report.get("endpoints", [])}


def short(path):
    return "/".join(path.split("/")[2:])


def nico_serial(r):
    for s in r.get("systems") or []:
        if s.get("serial_number"):
            return s["serial_number"]
    for c in r.get("chassis") or []:
        if c.get("serial_number"):
            return c["serial_number"]
    return ""


def err(e):
    r = e["report"]
    schema = r.get("last_exploration_error_schema") or {}
    code = schema.get("error_code", "")
    text = r.get("last_exploration_error") or ""
    try:
        j = json.loads(text)
        text = "%s: %s" % (j.get("Type"), j.get("Details") or j.get("details") or "")
    except (ValueError, TypeError):
        pass
    text = re.sub(r"\s+", " ", str(text))
    return (code + " " + text).strip()[:110]


rows, counts = [], Counter()
for ip, c in sorted(by_ip.items(), key=lambda kv: kv[1]["path"]):
    e = eps.get(ip)
    path = c["path"]
    if not e:
        rows.append((short(path), ip, "-", "-", "-", "-", "not explored (yet)"))
        counts["not explored"] += 1
        continue
    r = e["report"]
    s = nico_serial(r)
    twin_serial = serials.get(path, "")
    match = "-" if not s else ("yes" if s == twin_serial else ("no (twin %s)" % twin_serial if twin_serial else "?"))
    error = err(e) if r.get("last_exploration_error") else ""
    rows.append((short(path), ip, r.get("endpoint_type") or "-", r.get("vendor") or "-", s or "-", match, error or "ok"))
    counts["explored ok" if not error else "exploration error"] += 1
w = [max(len(str(r[i])) for r in rows + [("twin device", "NICo lease", "type", "vendor", "NICo serial", "= twin", "result")]) for i in range(6)]
hdr = ("twin device", "NICo lease", "type", "vendor", "NICo serial", "= twin", "result")
print("  ".join(h.ljust(w[i]) for i, h in enumerate(hdr[:6])) + "  " + hdr[6])
for r in rows:
    print("  ".join(str(x).ljust(w[i]) for i, x in enumerate(r[:6])) + "  " + r[6])
mh = report.get("managed_hosts") or []
print("\n%d BMCs leased by NICo; %s; %d managed hosts identified" % (len(by_ip), ", ".join("%s %d" % kv for kv in counts.most_common()), len(mh)))

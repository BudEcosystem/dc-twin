#!/usr/bin/env python3
"""The reference day-0 tooling's cabling check (stacks/reference-day0, docs/spec/18-day0.md Z3).

It reads what a site's tooling has: stack-inputs/day0's cabling.csv (the cabling plan) and
expected-switches.json (each switch's management MAC), and its DHCP server's leases (a CSV of
mac,address), and asks each switch's NVUE for the LLDP neighbours of the ports the plan cables
(GET /nvue_v1/interface/<port>/lldp/neighbor, as `nv show interface <port> lldp neighbor` does).
Every cable between two switches the tooling leased is checked at both ends: the neighbour seen
must be the far device and port the plan names. It prints one line per cable end and, last, a
JSON summary, and exits 1
when any cable is not as planned, as a site's validation (NetQ's `netq check topology`, PTM)
fails a fabric before its BGP sessions are trusted. Python's standard library only.

  check-cabling.py <stack-inputs/day0> --leases <leases.csv> --password <pw> [--user cumulus]
"""
import argparse
import base64
import csv
import json
import os
import ssl
import sys
import urllib.error
import urllib.request


def neighbours(addr, port, user, password, api_port):
    url = "https://%s:%d/nvue_v1/interface/%s/lldp/neighbor" % (addr, api_port, port)
    req = urllib.request.Request(url)
    req.add_header("Authorization", "Basic " + base64.b64encode(("%s:%s" % (user, password)).encode()).decode())
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    with urllib.request.urlopen(req, context=ctx, timeout=30) as r:
        return json.load(r)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("inputs", help="stack-inputs/day0")
    ap.add_argument("--leases", required=True, help="the DHCP server's leases: a CSV of mac,address")
    ap.add_argument("--user", default="cumulus")
    ap.add_argument("--password", required=True)
    ap.add_argument("--api-port", type=int, default=8765)
    a = ap.parse_args()
    with open(os.path.join(a.inputs, "expected-switches.json")) as f:
        expected = json.load(f)
    leases = {}
    with open(a.leases) as f:
        for row in csv.reader(f):
            if len(row) >= 2:
                leases[row[0].strip().lower()] = row[1].strip()
    # the switches this tooling brought up: those its DHCP server leased (the OOB switches the
    # site configures otherwise, and their cables to the management ports, are not its to check)
    addr = {e["hostname"]: leases[e["mgmtMAC"].lower()] for e in expected if leases.get(e.get("mgmtMAC", "").lower())}
    with open(os.path.join(a.inputs, "cabling.csv")) as f:
        cables = [c for c in csv.DictReader(f) if c["a_interface"] and c["b_interface"]
                  and c["a_device"] in addr and c["b_device"] in addr]
    bad = []
    checked = 0
    for c in cables:
        for near, nif, far, fif in ((c["a_device"], c["a_interface"], c["b_device"], c["b_interface"]),
                                    (c["b_device"], c["b_interface"], c["a_device"], c["a_interface"])):
            checked += 1
            if not addr.get(near):
                bad.append(c["id"])
                print("NO-ADDRESS %s %s:%s: no lease for the switch" % (c["id"], near, nif))
                continue
            try:
                seen = neighbours(addr[near], nif, a.user, a.password, a.api_port)
            except (urllib.error.URLError, OSError, ValueError) as e:
                bad.append(c["id"])
                print("UNREACHABLE %s %s:%s: %s" % (c["id"], near, nif, e))
                continue
            got = sorted("%s:%s" % (n.get("chassis", {}).get("system-name", k), n.get("port", {}).get("name", "?"))
                         for k, n in (seen or {}).items())
            want = "%s:%s" % (far, fif)
            if got == [want]:
                print("OK %s %s:%s sees %s" % (c["id"], near, nif, want))
            else:
                bad.append(c["id"])
                print("MISMATCH %s %s:%s sees %s, the plan says %s" % (c["id"], near, nif, ", ".join(got) or "nothing", want))
    bad = sorted(set(bad))
    print(json.dumps({"cables": len(cables), "ends": checked, "notAsPlanned": bad}))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Put each access port's untagged VLAN into NetBox, as a site's operator does after the design's
CSV import.

The import carries an access port's `mode` but not the VLAN it is untagged in: NetBox's interface
import form has no `untagged_vlan` column (netbox/dcim/forms/bulk_import.py at 4.2), so the design
cannot say it in CSV. This reads the ports from the design itself (stack-inputs/day0/plan.json: each
switch's `vlans`, their `ports` and `name`) and sets, for every one of them, the interface's mode to
access and its untagged VLAN to that VLAN at the switch's site. A tool reading the design from
NetBox then sees the segments' ports as the design has them.

Through NetBox's REST API with a token; Python's standard library only.

  access-vlans.py <netbox url> <stack-inputs/day0> --token <token>
"""
import argparse
import json
import os
import sys
import urllib.parse
import urllib.request


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("url")
    ap.add_argument("inputs")
    ap.add_argument("--token", required=True)
    a = ap.parse_args()
    base = a.url.rstrip("/") + "/api"

    def api(method, path, body=None, **query):
        url = base + path + ("?" + urllib.parse.urlencode(query) if query else "")
        req = urllib.request.Request(url, method=method, data=json.dumps(body).encode() if body is not None else None,
                                     headers={"Authorization": "Token " + a.token, "Content-Type": "application/json",
                                              "Accept": "application/json"})
        with urllib.request.urlopen(req, timeout=60) as r:
            return json.load(r)

    def one(path, **query):
        res = api("GET", path, **query)["results"]
        return res[0] if res else None

    with open(os.path.join(a.inputs, "plan.json")) as f:
        plan = json.load(f)
    done = 0
    for sw in plan.get("switches", []):
        host = sw["hostname"]
        for v in sw.get("vlans") or []:
            ports = v.get("ports") or []
            if not ports:
                continue
            dev = one("/dcim/devices/", name=host)
            if dev is None:
                print("no device %s in NetBox" % host, file=sys.stderr)
                return 1
            # the VLAN at the switch's own site, as vlans.csv imported it
            site = (dev.get("site") or {}).get("id")
            vid = int(str(v["name"]).lower().removeprefix("vlan")) if not v.get("id") else int(v["id"])
            vlan = one("/ipam/vlans/", vid=vid, site_id=site) or one("/ipam/vlans/", vid=vid)
            if vlan is None:
                print("%s: no VLAN %d in NetBox for %s" % (host, vid, v["name"]), file=sys.stderr)
                return 1
            for port in ports:
                ifc = one("/dcim/interfaces/", device_id=dev["id"], name=port)
                if ifc is None:
                    print("%s: no interface %s in NetBox" % (host, port), file=sys.stderr)
                    return 1
                api("PATCH", "/dcim/interfaces/%d/" % ifc["id"], {"mode": "access", "untagged_vlan": vlan["id"]})
                print("%s %s: access, untagged in VLAN %d" % (host, port, vid))
                done += 1
    print("%d access ports given their untagged VLAN" % done)
    return 0


if __name__ == "__main__":
    sys.exit(main())

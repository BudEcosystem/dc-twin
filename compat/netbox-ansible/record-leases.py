#!/usr/bin/env python3
"""Record each switch's management lease in NetBox, as a site's tooling does once its DHCP
server has handed the factory switches their addresses.

For every expected switch (stack-inputs/day0/expected-switches.json: hostname and management MAC)
with a lease (a CSV of mac,address, the DHCP server's), the address is created in NetBox in the
management VRF with its prefix's length, assigned to the device's eth0, and made the device's
out-of-band address (oob_ip), which Ansible's netbox.netbox inventory then gives as the host's
address. Through NetBox's REST API with a token; Python's standard library only.

  record-leases.py <netbox url> <stack-inputs/day0> --leases <leases.csv> --token <token>
"""
import argparse
import csv
import ipaddress
import json
import os
import sys
import urllib.parse
import urllib.request


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("url")
    ap.add_argument("inputs")
    ap.add_argument("--leases", required=True)
    ap.add_argument("--token", required=True)
    ap.add_argument("--vrf", default="mgmt")
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

    with open(os.path.join(a.inputs, "expected-switches.json")) as f:
        expected = json.load(f)
    leases = {}
    with open(a.leases) as f:
        for row in csv.reader(f):
            if len(row) >= 2:
                leases[row[0].strip().lower()] = row[1].strip()
    vrf = one("/ipam/vrfs/", name=a.vrf)
    if vrf is None:
        print("no VRF %s in NetBox" % a.vrf, file=sys.stderr)
        return 1
    done = 0
    for e in expected:
        ip = leases.get(e.get("mgmtMAC", "").lower())
        if not ip:
            continue
        dev = one("/dcim/devices/", name=e["hostname"])
        ifc = dev and one("/dcim/interfaces/", device_id=dev["id"], name="eth0")
        pfx = one("/ipam/prefixes/", contains=ip, vrf_id=vrf["id"])
        if not dev or not ifc or not pfx:
            print("%s: device %s, eth0 %s, management prefix for %s %s in NetBox" % (
                e["hostname"], bool(dev), bool(ifc), ip, bool(pfx)), file=sys.stderr)
            return 1
        addr = "%s/%d" % (ip, ipaddress.ip_network(pfx["prefix"]).prefixlen)
        rec = one("/ipam/ip-addresses/", address=addr, vrf_id=vrf["id"])
        body = {"address": addr, "vrf": vrf["id"], "status": "dhcp", "assigned_object_type": "dcim.interface",
                "assigned_object_id": ifc["id"], "description": "management lease"}
        rec = api("PATCH", "/ipam/ip-addresses/%d/" % rec["id"], body) if rec else api("POST", "/ipam/ip-addresses/", body)
        api("PATCH", "/dcim/devices/%d/" % dev["id"], {"oob_ip": rec["id"]})
        print("%s: %s on eth0, its out-of-band address" % (e["hostname"], addr))
        done += 1
    print("%d management leases recorded" % done)
    return 0 if done else 1


if __name__ == "__main__":
    sys.exit(main())

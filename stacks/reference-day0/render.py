#!/usr/bin/env python3
"""The reference day-0 tooling's generator (stacks/reference-day0, docs/spec/18-day0.md).

It reads what a site hands its day-0 tooling, the twin's stack-inputs/day0 (D77):
expected-switches.json (each switch by serial and management MAC) and plan.json (the address
plan), and writes, as a site's own generator does:

  <out>/kea-dhcp4.json            ISC Kea: the management segments, and a reservation per switch
                                  by its management MAC handing it its ZTP URL: option 239 for a
                                  Cumulus switch (its provisioning script), option 67 for a SONiC
                                  switch (its ZTP document); ONIE's default URL (option 114) for
                                  a switch with no NOS yet (vendor class onie_vendor:)
  <out>/www/ztp/<serial>.sh       a Cumulus ZTP script: `nv set ...; nv config apply -y`, with the
                                  CUMULUS-AUTOPROVISIONING marker Cumulus's ZTP requires
  <out>/www/ztp/<serial>.json     a SONiC ZTP document whose configdb-json section loads
  <out>/www/configdb/<serial>.json  the switch's config_db.json

Serve <out>/www with any HTTP server (python3 -m http.server --directory <out>/www 8080) at the
address given as --server, and run Kea with <out>/kea-dhcp4.json. Nothing here is the twin's:
it reads only the files a site has and speaks only DHCP and HTTP.
"""
import argparse
import json
import os
import sys


def cumulus_script(s, password):
    lines = ["#!/bin/bash", "# CUMULUS-AUTOPROVISIONING", "set -e",
             "nv set system hostname %s" % s["hostname"]]
    if s.get("loopback"):
        lines.append("nv set interface lo ip address %s" % s["loopback"])
    for it in s.get("interfaces", []):
        if it.get("kind") == "fabric":
            lines.append("nv set interface %s ip address %s" % (it["name"], it["address"]))
            lines.append("nv set interface %s link state up" % it["name"])
        elif it.get("kind") == "unnumbered":
            lines.append("nv set interface %s link state up" % it["name"])
    if s.get("asn"):
        lines.append("nv set router bgp autonomous-system %d" % s["asn"])
        lines.append("nv set router bgp router-id %s" % s["routerId"])
        lines.append("nv set vrf default router bgp address-family ipv4-unicast redistribute connected enable on")
        for it in s.get("interfaces", []):
            if it.get("kind") == "fabric":
                lines.append("nv set vrf default router bgp neighbor %s remote-as %d" % (it["peerAddress"], it["peerAsn"]))
            elif it.get("kind") == "unnumbered":
                lines.append("nv set vrf default router bgp neighbor %s remote-as external" % it["name"])
            if s.get("evpn") and it.get("kind") in ("fabric", "unnumbered"):
                peer = it.get("peerAddress") or it["name"]
                lines.append("nv set vrf default router bgp neighbor %s address-family l2vpn-evpn enable on" % peer)
        if s.get("evpn"):
            lines.append("nv set evpn enable on")
    for v in s.get("vlans", []):
        lines.append("nv set bridge domain br_default vlan %d" % v["id"])
        lines.append("nv set interface vlan%d ip address %s" % (v["id"], v["gateway"]))
        for p in v.get("ports", []):
            lines.append("nv set interface %s bridge domain br_default access %d" % (p, v["id"]))
            lines.append("nv set interface %s link state up" % p)
        for r in v.get("relay", []):
            lines.append("nv set service dhcp-relay default server %s" % r)
            lines.append("nv set service dhcp-relay default interface vlan%d" % v["id"])
    if password:
        lines.append("nv set system aaa user cumulus password '%s'" % password)
    lines += ["nv config apply -y", "exit 0", ""]
    return "\n".join(lines)


def sonic_configdb(s):
    db = {}

    def put(table, key, v):
        db.setdefault(table, {})[key] = v

    asn = str(s.get("asn", ""))
    put("DEVICE_METADATA", "localhost", {"hostname": s["hostname"], "bgp_asn": asn,
                                         "docker_routing_config_mode": "unified", "frr_mgmt_framework_config": "true"})
    if s.get("loopback"):
        put("LOOPBACK_INTERFACE", "Loopback0", {})
        put("LOOPBACK_INTERFACE", "Loopback0|" + s["loopback"], {})
    if asn:
        put("BGP_GLOBALS", "default", {"local_asn": asn, "router_id": s["routerId"], "keepalive": "3", "holdtime": "9"})
        put("BGP_GLOBALS_AF", "default|ipv4_unicast", {"redistribute_connected": "true"})
        put("ROUTE_REDISTRIBUTE", "default|connected|bgp|ipv4", {})
        put("ROUTE_MAP", "DAY0_PERMIT|10", {"route_operation": "permit"})
    for it in s.get("interfaces", []):
        if it.get("kind") != "fabric":
            continue
        put("INTERFACE", it["name"], {})
        put("INTERFACE", it["name"] + "|" + it["address"], {})
        put("BGP_NEIGHBOR", "default|" + it["peerAddress"], {"asn": str(it["peerAsn"]), "local_addr": it["address"].split("/")[0],
                                                              "admin_status": "up", "keepalive": "3", "holdtime": "9"})
        put("BGP_NEIGHBOR_AF", "default|%s|ipv4_unicast" % it["peerAddress"],
            {"admin_status": "true", "route_map_in": ["DAY0_PERMIT"], "route_map_out": ["DAY0_PERMIT"]})
    for v in s.get("vlans", []):
        vlan = {"vlanid": str(v["id"])}
        if v.get("relay"):
            vlan["dhcp_servers"] = v["relay"]
        put("VLAN", v["name"], vlan)
        for p in v.get("ports", []):
            put("VLAN_MEMBER", "%s|%s" % (v["name"], p), {"tagging_mode": "untagged"})
        put("VLAN_INTERFACE", v["name"], {})
        put("VLAN_INTERFACE", "%s|%s" % (v["name"], v["gateway"]), {})
    return db


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("inputs", help="stack-inputs/day0")
    ap.add_argument("out")
    ap.add_argument("--server", required=True, help="the HTTP server's address, host[:port]")
    ap.add_argument("--interface", default="svc0", help="the interface Kea listens on")
    ap.add_argument("--password", default="", help="a new password for user cumulus (Cumulus ZTP scripts)")
    a = ap.parse_args()
    with open(os.path.join(a.inputs, "expected-switches.json")) as f:
        expected = json.load(f)
    with open(os.path.join(a.inputs, "plan.json")) as f:
        plan = json.load(f)
    by_serial = {s["serial"]: s for s in plan["switches"]}
    base = "http://" + a.server
    os.makedirs(os.path.join(a.out, "www", "ztp"), exist_ok=True)
    os.makedirs(os.path.join(a.out, "www", "configdb"), exist_ok=True)
    reservations = []
    for e in expected:
        if not e.get("factory"):
            continue
        s = by_serial.get(e["serial"])
        if s is None:
            print("no address plan for %s (%s)" % (e["hostname"], e["serial"]), file=sys.stderr)
            continue
        opts = []
        if e["nos"] == "sonic-vs":
            with open(os.path.join(a.out, "www", "configdb", e["serial"] + ".json"), "w") as f:
                json.dump(sonic_configdb(s), f, indent=2, sort_keys=True)
            doc = {"ztp": {"01-configdb-json": {"url": {"source": "%s/configdb/%s.json" % (base, e["serial"])},
                                                "clear-config": False, "overwrite-device-metadata": True}}}
            with open(os.path.join(a.out, "www", "ztp", e["serial"] + ".json"), "w") as f:
                json.dump(doc, f, indent=2)
            opts.append({"name": "boot-file-name", "data": "%s/ztp/%s.json" % (base, e["serial"])})
        else:
            path = os.path.join(a.out, "www", "ztp", e["serial"] + ".sh")
            with open(path, "w") as f:
                f.write(cumulus_script(s, a.password))
            os.chmod(path, 0o755)
            opts.append({"name": "cumulus-provision-url", "data": "%s/ztp/%s.sh" % (base, e["serial"]), "always-send": True})
        if e.get("mgmtMAC"):
            reservations.append({"hw-address": e["mgmtMAC"].lower(), "option-data": opts})
    subnets = []
    for i, m in enumerate(plan.get("managementNetworks", [])):
        if not m.get("prefix"):
            continue
        net = m["prefix"].split("/")[0].rsplit(".", 1)[0]
        subnets.append({"id": i + 1, "subnet": m["prefix"], "pools": [{"pool": "%s.10 - %s.250" % (net, net)}],
                        "option-data": [{"name": "routers", "data": m["gateway"]}], "reservations": reservations})
    kea = {"Dhcp4": {
        "interfaces-config": {"interfaces": [a.interface], "dhcp-socket-type": "udp"},
        "lease-database": {"type": "memfile", "persist": True, "name": "/var/lib/kea/kea-leases4.csv"},
        "valid-lifetime": 3600,
        "reservations-global": False, "reservations-in-subnet": True,
        # Cumulus's 239 is no standard option; ONIE's default-url is code 114, which Kea 2.6 names
        # v4-captive-portal (RFC 8910 gave the code to the captive portal's URL)
        "option-def": [{"name": "cumulus-provision-url", "code": 239, "type": "string", "space": "dhcp4"}],
        "client-classes": [{"name": "onie", "test": "substring(option[60].text,0,12) == 'onie_vendor:'",
                            "option-data": [{"name": "v4-captive-portal", "data": base + "/onie/onie-installer", "always-send": True}]}],
        "subnet4": subnets}}
    with open(os.path.join(a.out, "kea-dhcp4.json"), "w") as f:
        json.dump(kea, f, indent=2)
    print("rendered %d switches' ZTP into %s" % (len(reservations), a.out))


if __name__ == "__main__":
    main()

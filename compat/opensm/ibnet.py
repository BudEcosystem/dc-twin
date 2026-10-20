#!/usr/bin/env python3
"""Reads topologies in ibnetdiscover's output format: the one the twin's ibsim adapter renders
from the model, and what ibnetdiscover discovers against ibsim once a subnet manager has run.

  ibnet.py islands NET                   connected components: SIM_HOST, switches, CAs, links
  ibnet.py node NET DESC                 node id, port GUID and switch port of a node description
  ibnet.py compare NET DISC...           the union of the discovered files equals NET?
  ibnet.py compare-part NET DISC...      ... equals the islands of NET it reaches?
  ibnet.py lft NET DUMP [--focus SWDESC] [--without SWDESC:PORT]
                                         OpenSM's LFT dump: every switch holds every LID and routes
                                         every CA LID on a minimum-hop path of the model's cabling
                                         (less a pulled cable); --focus: one switch's ports for CAs
                                         it does not cable
  ibnet.py lidsame DISC DISC             how many nodes kept their LID between two discoveries
  ibnet.py lids DISC                     distinct non-zero LIDs in a discovery
  ibnet.py ufm CREDS NET DISC...         UFM REST's ports against the topology and discovery
  ibnet.py lid DISC PORTGUID             a channel adapter port's LID in a discovered file
  ibnet.py desc NET PORTGUID             the node description of a port GUID

Only the standard library; run with python3 -I.
"""
import json
import re
import ssl
import sys
import urllib.request

HEADER = re.compile(r'^(Switch|Ca)\s+(\d+)\s+"([^"]+)"\s+#\s+"([^"]*)"(.*)$')
PORT = re.compile(r'^\[(\d+)\](?:\(([0-9a-f]+)\))?\s+"([^"]+)"\[(\d+)\](?:\(([0-9a-f]+)\))?(.*)$')


def parse(path):
    """nodes: id -> {kind, guid, sys, desc, nports, lid, portguid, ports: {n: (peer, peerport)}}"""
    nodes, cur, guid, sysg = {}, None, None, None
    with open(path) as f:
        for line in f:
            line = line.rstrip("\n")
            if not line.strip():
                cur = None
                continue
            if line.startswith("#"):
                continue
            if line.startswith(("switchguid=", "caguid=")):
                guid = int(line.split("=", 1)[1].split("(")[0], 16)
                continue
            if line.startswith("sysimgguid="):
                sysg = int(line.split("=", 1)[1], 16)
                continue
            m = HEADER.match(line)
            if m:
                kind, nports, nid, desc, rest = m.groups()
                lid = re.search(r"lid (\d+)", rest)
                cur = nodes[nid] = {"kind": kind, "guid": guid, "sys": sysg if sysg is not None else guid, "desc": desc, "nports": int(nports),
                                    "lid": int(lid.group(1)) if lid else None, "portguid": guid if kind == "Switch" else None,
                                    "ports": {}}
                guid = sysg = None
                continue
            m = PORT.match(line)
            if m and cur is not None:
                n, lpg, peer, pport, _rpg, rest = m.groups()
                cur["ports"][int(n)] = (peer, int(pport))
                if cur["kind"] == "Ca":
                    cur["portguid"] = int(lpg, 16) if lpg else cur["portguid"]
                    lid = re.search(r"# lid (\d+)", rest)
                    if lid:
                        cur["lid"] = int(lid.group(1))
    return nodes


def links(nodes):
    out = set()
    for nid, n in nodes.items():
        for p, (peer, pp) in n["ports"].items():
            out.add(tuple(sorted([(nid, p), (peer, pp)])))
    return out


def islands(nodes):
    seen, comps = set(), []
    order = list(nodes)  # file order: the SM's CA first
    for start in order:
        if start in seen:
            continue
        comp, todo = [], [start]
        seen.add(start)
        while todo:
            x = todo.pop()
            comp.append(x)
            for peer, _ in nodes[x]["ports"].values():
                if peer in nodes and peer not in seen:
                    seen.add(peer)
                    todo.append(peer)
        comps.append(sorted(comp, key=order.index))
    return comps


def merge(paths):
    out = {}
    for p in paths:
        for nid, n in parse(p).items():
            out.setdefault(nid, n)
    return out


def compare(net, disc, partial=False):
    want, got = parse(net), merge(disc)
    if partial:  # only the islands of the model the discovery reached
        keep = set()
        for comp in islands(want):
            if any(x in got for x in comp):
                keep |= set(comp)
        want = {k: v for k, v in want.items() if k in keep}
    bad = []
    for nid in sorted(set(want) | set(got)):
        w, g = want.get(nid), got.get(nid)
        if w is None or g is None:
            bad.append(f"{nid}: {'not in the model' if w is None else 'not discovered'}")
            continue
        for k in ("kind", "guid", "desc", "nports", "portguid", "sys"):
            if w[k] != g[k]:
                bad.append(f"{nid} {k}: model {w[k]!r} discovered {g[k]!r}")
        if w["ports"] != g["ports"]:
            bad.append(f"{nid} ports: model {sorted(w['ports'].items())[:3]}… discovered {sorted(g['ports'].items())[:3]}…")
    sw = sum(1 for n in got.values() if n["kind"] == "Switch")
    ca = sum(1 for n in got.values() if n["kind"] == "Ca")
    lw, lg = links(want), links(got)
    print(json.dumps({"switches": sw, "cas": ca, "links": len(lg), "modelLinks": len(lw), "equal": not bad and lw == lg,
                      "mismatches": bad[:20]}))
    return 0 if not bad and lw == lg else 1


def ufm(creds, net, disc):
    c = json.load(open(creds))[0]
    ctx = ssl.create_default_context()
    ctx.check_hostname, ctx.verify_mode = False, ssl.CERT_NONE
    req = urllib.request.Request(c["url"] + "/ufmRest/resources/ports")
    import base64
    req.add_header("Authorization", "Basic " + base64.b64encode(f"{c['user']}:{c['password']}".encode()).decode())
    ports = json.load(urllib.request.urlopen(req, context=ctx))
    want, got = parse(net), merge(disc) if disc else {}
    ca_by_pg = {f"{n['portguid']:016x}": (nid, n) for nid, n in want.items() if n["kind"] == "Ca"}
    sw_by_g = {f"{n['guid']:016x}": (nid, n) for nid, n in want.items() if n["kind"] == "Switch"}
    disc_pg = {f"{n['portguid']:016x}": n for n in got.values() if n["kind"] == "Ca" and n["portguid"]}
    disc_sw = {f"{n['guid']:016x}": n for n in got.values() if n["kind"] == "Switch"}
    r = {"ufmComputerPorts": 0, "ufmSwitchPorts": 0, "caGuidInModel": 0, "caDescEqual": 0, "caSysEqual": 0,
         "caDiscovered": 0, "caLidEqual": 0, "caActiveUFM": 0, "caActiveUFMNotDiscovered": 0,
         "switchSysGuidLengths": {}, "switchSysGuidPrefixEqual": 0, "switchDiscovered": 0, "switchLidEqual": 0,
         "examples": {}}
    for p in ports:
        if p["description"] == "Computer IB Port":
            r["ufmComputerPorts"] += 1
            hit = ca_by_pg.get(p["guid"])
            if hit:
                r["caGuidInModel"] += 1
                r["caDescEqual"] += hit[1]["desc"] == p["node_description"]
                r["caSysEqual"] += f"{hit[1]['sys']:016x}" == p["systemID"]
            d = disc_pg.get(p["guid"])
            active = p["logical_state"] == "Active"
            r["caActiveUFM"] += active
            if d:
                r["caDiscovered"] += 1
                r["caLidEqual"] += d["lid"] == p["lid"]
                r["examples"].setdefault("lid", f"{p['guid']}: UFM lid {p['lid']}, OpenSM lid {d['lid']}")
            elif active:
                r["caActiveUFMNotDiscovered"] += 1
                r["examples"].setdefault("activeNotDiscovered", f"{p['node_description']} ({p['guid']}) UFM {p['logical_state']}")
        else:
            r["ufmSwitchPorts"] += 1
            sid = p["systemID"]
            r["switchSysGuidLengths"][str(len(sid))] = r["switchSysGuidLengths"].get(str(len(sid)), 0) + 1
            r["switchSysGuidPrefixEqual"] += sid[:16] in sw_by_g
            r["switchDiscovered"] += sid[:16] in disc_sw
            if sid[:16] in disc_sw:
                r["switchLidEqual"] += disc_sw[sid[:16]]["lid"] == p["lid"]
                r["examples"].setdefault("switchLid", f"{p['system_name']} port {p['number']}: UFM lid {p['lid']}, OpenSM lid {disc_sw[sid[:16]]['lid']}")
            r["examples"].setdefault("switchGuid", f"UFM systemID {sid}, OpenSM/ibsim node GUID {sid[:16]}")
    print(json.dumps(r))
    return 0


def lft(net, dump, focus=None, without=None):
    """OpenSM's opensm-lfts.dump against the model's cabling: every switch's table holds every
    LID, and routes every channel adapter's LID out of the port its cable is on (at its leaf) or
    out of a port toward a switch one hop nearer that leaf (minhop, OpenSM's default engine)."""
    want = parse(net)
    sw = {nid for nid, n in want.items() if n["kind"] == "Switch"}
    by_desc = {want[nid]["desc"]: nid for nid in sw}
    adj = {nid: {} for nid in sw}  # switch -> {port: peer switch}
    attach = {}  # CA port GUID -> (leaf, port)
    for nid in sw:
        for p, (peer, _) in want[nid]["ports"].items():
            if peer in sw:
                adj[nid][p] = peer
            else:
                attach[want[peer]["portguid"]] = (nid, p)
    if without:
        d, port = without.rsplit(":", 1)
        a, port = by_desc[d], int(port)
        b = adj[a].pop(port)
        for q, x in list(adj[b].items()):
            if x == a and want[b]["ports"][q] == (a, port):
                del adj[b][q]
    dist = {}
    for t in sw:  # hops from every switch to t
        d, todo = {t: 0}, [t]
        while todo:
            x = todo.pop(0)
            for y in adj[x].values():
                if y not in d:
                    d[y] = d[x] + 1
                    todo.append(y)
        dist[t] = d
    head = re.compile(r"Unicast lids \[\d+-\d+\] of switch Lid \d+ guid 0x([0-9a-f]+)")
    entry = re.compile(r"^0x([0-9a-f]+) (\d+) # (.*)$")
    ca = re.compile(r"Channel Adapter portguid 0x([0-9a-f]+)")
    r = {"switches": 0, "lidsPerSwitch": [], "caRoutes": 0, "minhop": 0, "wrong": [], "focus": {}}
    cur, n = None, 0
    for line in open(dump):
        m = head.match(line)
        if m:
            if cur is not None:
                r["lidsPerSwitch"].append(n)
            cur, n = f"S-{int(m.group(1), 16):016x}", 0
            r["switches"] += 1
            continue
        m = entry.match(line)
        if not m or cur is None:
            continue
        n += 1
        c = ca.search(m.group(3))
        if not c:
            continue
        r["caRoutes"] += 1
        port = int(m.group(2))
        leaf, lp = attach.get(int(c.group(1), 16), (None, None))
        if leaf is None:
            r["wrong"].append(f"{cur}: {line.strip()} (no such CA in the model)")
            continue
        if cur == leaf:
            ok = port == lp
        else:
            here = dist[leaf].get(cur)
            ok = here is not None and port in adj[cur] and dist[leaf].get(adj[cur][port]) == here - 1
            if focus and want[cur]["desc"] == focus:
                r["focus"][str(port)] = r["focus"].get(str(port), 0) + 1
                r.setdefault("example", {}).setdefault(str(port), int(m.group(1), 16))
        if ok:
            r["minhop"] += 1
        else:
            r["wrong"].append(f"{want[cur]['desc']}: {line.strip()}")
    if cur is not None:
        r["lidsPerSwitch"].append(n)
    r["lidsPerSwitch"] = sorted(set(r["lidsPerSwitch"]))
    r["wrong"] = r["wrong"][:5]
    print(json.dumps(r))
    return 0 if r["caRoutes"] == r["minhop"] else 1


def lidsame(a, b):
    x, y = parse(a), parse(b)
    same = sum(1 for k, v in x.items() if k in y and v["lid"] == y[k]["lid"] and v["lid"])
    print(same, "of", len(x))
    return 0


def main(argv):
    cmd = argv[1]
    if cmd == "islands":
        nodes = parse(argv[2])
        for comp in islands(nodes):
            cas = [x for x in comp if nodes[x]["kind"] == "Ca"]
            sws = [x for x in comp if nodes[x]["kind"] == "Switch"]
            nl = len({l for l in links(nodes) if l[0][0] in comp})
            print(cas[0], len(sws), len(cas), nl, ",".join(nodes[s]["desc"] for s in sws))
        return 0
    if cmd == "compare":
        return compare(argv[2], argv[3:])
    if cmd == "compare-part":
        return compare(argv[2], argv[3:], partial=True)
    if cmd == "lft":
        opts = dict(zip(argv[4::2], argv[5::2]))
        return lft(argv[2], argv[3], opts.get("--focus"), opts.get("--without"))
    if cmd == "lidsame":
        return lidsame(argv[2], argv[3])
    if cmd == "lids":
        print(len({n["lid"] for n in parse(argv[2]).values() if n["lid"]}))
        return 0
    if cmd == "node":
        nodes = parse(argv[2])
        for nid, n in nodes.items():
            if n["desc"] == argv[3]:
                peer = next(iter(n["ports"].values()), ("", 0))
                print(nid, f"{n['portguid']:016x}", peer[1])
                return 0
        return 1
    if cmd == "ufm":
        return ufm(argv[2], argv[3], argv[4:])
    if cmd == "lid":
        for n in parse(argv[2]).values():
            if n["kind"] == "Ca" and n["portguid"] == int(argv[3], 16):
                print(n["lid"])
                return 0
        return 1
    if cmd == "desc":
        for n in parse(argv[2]).values():
            if n["portguid"] == int(argv[3], 16):
                print(n["desc"])
                return 0
        return 1
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))

"""Loads a blueprint into NVCM's Nautobot the way NVCM's installer does (run_after_deploy,
installer/.../nautobot_jobs.py at 1.4.1): MockTopologyDesign found, enabled and run with the
blueprint, its result waited for. Run in the Nautobot container, against Nautobot at localhost.

Then the site's underlay peer group (NVCM's templates' UNDERLAY) on every BGP endpoint the design
made without one: MockTopologyDesign's peerings carry no peer group, and NVCM's render falls back
to the peer's role in upper case (CONVERGED-SPINE), a peer group its templates never define (NVCM's
goldens have UNDERLAY from their render data). A site's source of truth holds it, as here. An
endpoint on a loopback (an OOB-Leaf's session with the switch it learns VRF OOB from, 18 — Z7d)
is NVCM's EVPN peer group instead, which its templates define (multihop, update-source lo).

Last, the relay helpers of the blueprint's helpers.json: each VLAN related to its relay's servers
through NVCM's vlan_to_helper_address relationship (VLAN to IP address), which its render reads
for the relay (helper_addresses_by_vrf) and the blueprint format cannot carry.
argv: <token> <blueprint> <deployment name> [helpers] (helpers: only the relay helpers, the design
already loaded)"""
import json
import sys
import time
import urllib.error
import urllib.request

BASE = "http://127.0.0.1:8003"
token, blueprint, deployment = sys.argv[1:4]
only_helpers = sys.argv[4:5] == ["helpers"]


def call(method, path, body=None):
    """A request to Nautobot, tried again on a server error or no connection: a moment of no name
    resolution on the container network (its DNS reloading) answers 500, which a retry outlasts."""
    for attempt in range(20):
        req = urllib.request.Request(BASE + path, method=method, data=json.dumps(body).encode() if body is not None else None,
                                     headers={"Authorization": "Token " + token, "Content-Type": "application/json",
                                              "Accept": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=120) as r:
                return json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            if e.code < 500 or attempt == 19:
                raise
            print("Nautobot answered %d to %s %s; again" % (e.code, method, path), flush=True)
        except urllib.error.URLError:
            if attempt == 19:
                raise
        time.sleep(3)


def design():
    """MockTopologyDesign with the blueprint, then the peer groups."""
    jobs = call("GET", "/api/extras/jobs/?module_name=custom.mock_topology.jobs.mock_topology_design&job_class_name=MockTopologyDesign")
    if not jobs.get("results"):
        sys.exit("MockTopologyDesign is not registered: " + json.dumps(jobs)[:500])
    job = jobs["results"][0]
    call("PATCH", "/api/extras/jobs/%s/" % job["id"], {"enabled": True})
    res = call("POST", "/api/extras/jobs/%s/run/" % job["id"], {"data": {"blueprint": blueprint, "deployment_name": deployment}})
    rid = (res.get("job_result") or res).get("id")
    print("MockTopologyDesign", blueprint, "result", rid, flush=True)
    for _ in range(600):
        r = call("GET", "/api/extras/job-results/%s/" % rid)
        status = r["status"]["value"] if isinstance(r.get("status"), dict) else r.get("status")
        if status in ("SUCCESS", "FAILURE", "REVOKED"):
            break
        time.sleep(2)
    logs = call("GET", "/api/extras/job-logs/?job_result=%s&limit=500" % rid)
    for entry in logs.get("results", []):
        if entry.get("log_level") in ("warning", "error", "failure", "critical"):
            print(entry.get("log_level"), entry.get("message"), flush=True)
    print("status", status, flush=True)
    if status != "SUCCESS":
        # the job's own account of its failure: its result and traceback, and every log line
        print("result:", json.dumps(r.get("result"))[:2000], flush=True)
        print("traceback:", (r.get("traceback") or "")[-4000:], flush=True)
        for entry in logs.get("results", []):
            print(entry.get("log_level"), (entry.get("message") or "")[:500], flush=True)
        sys.exit(1)

    groups = {}  # (routing instance, peer group name) -> its id
    for name in ("UNDERLAY", "EVPN"):
        for pg in call("GET", "/api/plugins/bgp/peer-groups/?limit=1000&name=" + name).get("results", []):
            groups[((pg.get("routing_instance") or {}).get("id"), name)] = pg["id"]
    fixed = {"UNDERLAY": 0, "EVPN": 0}
    for ep in call("GET", "/api/plugins/bgp/peer-endpoints/?limit=1000&depth=1").get("results", []):
        if ep.get("peer_group"):
            continue
        ri = (ep.get("routing_instance") or {}).get("id")
        si = ep.get("source_interface") or {}
        name = "EVPN" if (si.get("name") or "").lower() == "lo" else "UNDERLAY"
        if (ri, name) not in groups:
            groups[(ri, name)] = call("POST", "/api/plugins/bgp/peer-groups/", {"name": name, "routing_instance": ri, "enabled": True})["id"]
        call("PATCH", "/api/plugins/bgp/peer-endpoints/%s/" % ep["id"], {"peer_group": groups[(ri, name)]})
        fixed[name] += 1
    print("UNDERLAY on", fixed["UNDERLAY"], "BGP endpoints; EVPN on", fixed["EVPN"], flush=True)


if not only_helpers:
    design()

path = "/opt/nautobot/jobs/custom/mock_topology/context/%s/helpers.json" % blueprint
try:
    with open(path) as f:
        helpers = json.load(f).get("vlans", [])
except FileNotFoundError:
    helpers = []
if helpers:
    rels = call("GET", "/api/extras/relationships/?key=vlan_to_helper_address").get("results", [])
    if not rels:
        sys.exit("NVCM's vlan_to_helper_address relationship is not in Nautobot")
    rel = rels[0]["id"]
    ns = call("GET", "/api/ipam/namespaces/?name=Global")["results"][0]["id"]
    active = call("GET", "/api/extras/statuses/?name=Active")["results"][0]["id"]
for h in helpers:
    vlans = call("GET", "/api/ipam/vlans/?vid=%d&limit=100" % h["vid"]).get("results", [])
    if not vlans:
        sys.exit("no VLAN %d in Nautobot for its relay helpers" % h["vid"])
    for host in h["helpers"]:
        ips = call("GET", "/api/ipam/ip-addresses/?address=%s" % host).get("results", [])
        if ips:
            ip = ips[0]["id"]
        else:
            net = ".".join(host.split(".")[:3]) + ".0/24"
            if not call("GET", "/api/ipam/prefixes/?prefix=%s&namespace=%s" % (net, ns)).get("results"):
                call("POST", "/api/ipam/prefixes/", {"prefix": net, "namespace": ns, "status": active, "type": "network"})
            ip = call("POST", "/api/ipam/ip-addresses/", {"address": host + "/24", "namespace": ns, "status": active, "type": "host"})["id"]
        for v in vlans:
            have = call("GET", "/api/extras/relationship-associations/?relationship=vlan_to_helper_address&source_id=%s&destination_id=%s" % (v["id"], ip)).get("results", [])
            if not have:
                call("POST", "/api/extras/relationship-associations/", {"relationship": rel, "source_type": "ipam.vlan", "source_id": v["id"],
                                                                         "destination_type": "ipam.ipaddress", "destination_id": ip})
        print("VLAN %d: relay helper %s (%d VLANs)" % (h["vid"], host, len(vlans)), flush=True)

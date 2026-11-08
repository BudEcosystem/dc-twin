#!/usr/bin/env python3
# A storage system's Swordfish service driven by DMTF's python-redfish-library (the `redfish`
# package, unmodified), as a standards-based storage client provisions: a session login, a file
# system and its NFS export, a volume, a host endpoint and the connection mapping it, a quota
# change, then every delete. After each write the twin's model is read back (`dctwin storage get`).
#
# Usage: swordfish_check.py CREDENTIALS.json
import json
import os
import subprocess
import sys

import redfish

DCTWIN = os.environ.get("DCTWIN", "dctwin")
results = []


def record(op, ok, detail):
    results.append({"op": op, "result": "PASS" if ok else "FAIL", "detail": detail})
    print(f"{'PASS' if ok else 'FAIL'}  {op:<28} {detail}", flush=True)


def model(name):
    out = subprocess.run([DCTWIN, "storage", "get", name], capture_output=True, text=True, check=True).stdout
    return json.loads(out)


def main():
    c = json.load(open(sys.argv[1]))[0]
    name = c["name"]
    cl = redfish.redfish_client(base_url=c["api"], username=c["user"], password=c["password"], default_prefix="/redfish/v1")
    cl.login(auth="session")
    sys_uri = "/redfish/v1/Storage/" + name
    root = cl.get("/redfish/v1").dict
    record("discover", "Storage" in root and cl.get(sys_uri).status == 200, f"service root links Storage; {sys_uri}")

    pool = cl.get(sys_uri + "/StoragePools/pool").dict
    m = model(name)
    record("pool.capacity", pool["Capacity"]["Data"]["AllocatedBytes"] == m["UsableBytes"],
           f"pool {pool['Capacity']['Data']['AllocatedBytes']} bytes, model usable {m['UsableBytes']}")

    r = cl.post(sys_uri + "/FileSystems", body={"Name": "models", "Capacity": {"Data": {"AllocatedBytes": 10 * 2**40}}})
    fs = r.getheader("Location")
    m = model(name)
    record("filesystem.create", r.status == 201 and any(f["Name"] == "models" and f["CapacityBytes"] == 10 * 2**40 for f in m["FileSystems"]),
           f"{r.status} {fs}")
    r = cl.post(fs + "/ExportedFileShares", body={"FileSharingProtocols": ["NFSv4_1"], "DefaultAccessCapabilities": ["Read", "Write"],
                                                  "RootAccess": False, "Oem": {"DcTwin": {"Clients": "10.50.0.0/16"}}})
    m = model(name)
    ex = [f["Exports"] for f in m["FileSystems"] if f["Name"] == "models"][0]
    record("export.create", r.status == 201 and ex == [{"Clients": "10.50.0.0/16", "Access": "rw", "RootSquash": True}], f"{r.status} {ex}")

    r = cl.patch(fs, body={"Capacity": {"Data": {"AllocatedBytes": 20 * 2**40}}})
    m = model(name)
    record("filesystem.quota", r.status == 200 and [f for f in m["FileSystems"] if f["Name"] == "models"][0]["CapacityBytes"] == 20 * 2**40, f"{r.status}")

    r = cl.post(sys_uri + "/Volumes", body={"Name": "db-1", "CapacityBytes": 2**40})
    vol = r.getheader("Location")
    m = model(name)
    v = [x for x in m["Volumes"] if x["Name"] == "db-1"]
    record("volume.create", r.status == 201 and v and v[0]["CapacityBytes"] == 2**40, f"{r.status} {vol} nqn {v[0]['NQN'] if v else None}")
    nqn = "nqn.2014-08.org.nvmexpress:uuid:5f6c0d2e-1b3a-4c55-9d1e-0c1f2a3b4c5d"
    r = cl.post("/redfish/v1/Fabrics/NVMeoF/Endpoints", body={"Identifiers": [{"DurableName": nqn, "DurableNameFormat": "NQN"}],
                                                               "ConnectedEntities": [{"EntityRole": "Initiator", "EntityType": "StorageInitiator"}]})
    ep = r.getheader("Location")
    record("host.register", r.status == 201 and nqn in model(name)["Hosts"], f"{r.status} {ep}")
    r = cl.post("/redfish/v1/Fabrics/NVMeoF/Connections", body={"VolumeInfo": [{"Volume": {"@odata.id": vol}, "AccessCapabilities": ["Read", "Write"]}],
                                                                 "Links": {"InitiatorEndpoints": [{"@odata.id": ep}]}})
    conn = r.getheader("Location")
    m = model(name)
    record("volume.map", r.status == 201 and [x for x in m["Volumes"] if x["Name"] == "db-1"][0]["Hosts"] == [nqn], f"{r.status} {conn}")
    tgt = cl.get(conn).dict["Links"]["TargetEndpoints"]
    tdoc = cl.get(tgt[0]["@odata.id"]).dict if tgt else {}
    addr = (tdoc.get("IPTransportDetails") or [{}])[0].get("IPv4Address", {}).get("Address")
    record("connection.target", bool(tgt) and addr == m["Addresses"].get("data"), f"target {tgt[0]['@odata.id'] if tgt else None} at {addr}:4420")

    for what, uri in (("connection.delete", conn), ("volume.delete", vol), ("host.delete", ep), ("filesystem.delete", fs)):
        r = cl.delete(uri)
        record(what, r.status == 204, f"{r.status} {uri}")
    m = model(name)
    record("model.empty", not m["FileSystems"] and not m["Volumes"] and not m["Hosts"], "the model holds nothing after the deletes")
    cl.logout()


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        record("error", False, repr(e))
    fails = [r for r in results if r["result"] == "FAIL"]
    print(f"\nstorage-swordfish: {len(results) - len(fails)} PASS, {len(fails)} FAIL")
    sys.exit(1 if fails else 0)

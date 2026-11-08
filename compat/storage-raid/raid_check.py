#!/usr/bin/env python3
# Ironic's redfish RAID interface and its out-of-band inspection, made with OpenStack sushy,
# unmodified, against dc-twin's storage-lab estate (docs/reviews/2026-11-08-storage-deep-dive.md
# P2). Each step is the sushy call sequence of ironic/drivers/modules/redfish/raid.py and
# inspect.py at Ironic master (2026-11): get_physical_disks (a Storage whose first controller's
# SupportedRAIDTypes is an explicit empty list is skipped), create_virtual_disk (the apply time
# Immediate when the collection supports it, else OnReset; a 202 is a task monitor polled until it
# ends, after a reboot for OnReset), _submit_delete_configuration (only volumes with a RAIDType), and
# _detect_local_gb. After every write the twin's model is read back (`dctwin get`).
#
# Usage: raid_check.py CREDENTIALS.json DEVICE
import json
import os
import subprocess
import sys
import time

import sushy
import urllib3

urllib3.disable_warnings()
DCTWIN = os.environ.get("DCTWIN", "dctwin")
GiB = 1024 ** 3
results = []


def twin_device(path):
    out = subprocess.run([DCTWIN, "get", path, "--json"], capture_output=True, text=True, check=True).stdout
    for v in json.loads(out) or []:
        if v["path"] == path:
            return v
    raise SystemExit(f"twin has no device {path}")


def record(op, ok, detail):
    results.append({"op": op, "result": "PASS" if ok else "FAIL", "detail": detail})
    print(f"{'PASS' if ok else 'FAIL'}  {op:<28} {detail}", flush=True)


def first_controller(storage):
    # ironic.drivers.modules.redfish.utils.get_first_controller
    try:
        ctrls = storage.controllers.get_members()
        if ctrls:
            return ctrls[0]
    except sushy.exceptions.MissingAttributeError:
        pass
    sc = storage.storage_controllers
    return sc[0] if sc else None


def fresh_volumes(system, storage_id):
    # as Ironic reads them (raid.py update_raid_config, through redfish_utils.get_system): a system
    # object of its own, its Storage, then its Volumes
    system = system._root.get_system(system.path)
    for st in system.storage.get_members():
        if st.identity == storage_id:
            return st.volumes
    raise RuntimeError(f"no Storage {storage_id}")


def wait_task(root, uri, what, timeout=120):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        tm = root.get_task_monitor(uri)
        if not tm.is_processing:
            t = tm.get_task()
            return t.task_state, t.task_status
        time.sleep(1)
    raise RuntimeError(f"{what}: task {uri} still processing after {timeout}s")


def main():
    creds = json.load(open(sys.argv[1]))
    path = sys.argv[2]
    c = next(x for x in creds if x["path"] == path)
    root = sushy.Sushy(f"https://{c['redfish']}/redfish/v1", username=c["user"], password=c["password"], verify=False)
    system = root.get_system(root.get_system_collection().members_identities[0])
    system._root = root

    # inspection: ironic/drivers/modules/redfish/inspect.py _detect_local_gb
    sizes = []
    try:
        sizes = [s for s in system.simple_storage.disks_sizes_bytes if s >= 4 * GiB]
    except sushy.exceptions.MissingAttributeError:
        pass
    if not sizes:
        try:
            st = system.storage
            sizes = sorted(s for s in (st.volumes_sizes_bytes or st.drives_sizes_bytes) if s >= 4 * GiB)
        except sushy.exceptions.MissingAttributeError:
            sizes = []
    local_gb = int(sizes[0] / GiB) - 1 if sizes else 0
    model = twin_device(path)
    drives = [x for x in model["components"] if x["kind"] == "drive"]
    smallest = min(d["drive"]["capacityBytes"] for d in drives)
    record("inspect.local_gb", local_gb == int(smallest / GiB) - 1, f"local_gb {local_gb} (smallest drive {smallest} bytes)")

    # get_physical_disks
    disks, disk_storage = [], {}
    for storage in system.storage.get_members():
        ctl = first_controller(storage)
        if ctl is not None and ctl.raid_types == []:
            continue
        for d in storage.drives:
            disks.append(d)
            disk_storage[d.identity] = storage
    raid_drives = [x for x in drives if x["drive"]["controller"]]
    record("raid.physical_disks", len(disks) == len(raid_drives),
           f"{len(disks)} disks behind RAID controllers ({len(raid_drives)} in the model); the direct NVMe Storage skipped")
    if not disks:
        return
    storage = disk_storage[disks[0].identity]
    ctl = first_controller(storage)
    record("raid.controller", ctl is not None and "RAID1" in [t.value if hasattr(t, "value") else t for t in ctl.raid_types],
           f"{storage.identity}: SupportedRAIDTypes {[getattr(t, 'value', t) for t in ctl.raid_types]}")

    # create_virtual_disk, as Ironic chooses the apply time
    vc = storage.volumes
    supported = vc.operation_apply_time_support.mapped_supported_values if vc.operation_apply_time_support else []
    apply_time = sushy.APPLY_TIME_IMMEDIATE if sushy.APPLY_TIME_IMMEDIATE in supported else (
        sushy.APPLY_TIME_ON_RESET if sushy.APPLY_TIME_ON_RESET in supported else None)
    payload = {"RAIDType": "RAID1", "Links": {"Drives": [{"@odata.id": d._path} for d in disks[:2]]}, "Name": "ironic-root"}
    r = vc.create(payload, apply_time=apply_time)
    vols = fresh_volumes(system, storage.identity).get_members()
    model = twin_device(path)
    record("raid.create.immediate", any(v.name == "ironic-root" and str(getattr(v.raid_type, "value", v.raid_type)) == "RAID1" for v in vols)
           and any(v["name"] == "ironic-root" for v in model["volumes"]),
           f"apply time {getattr(apply_time, 'value', apply_time)}; {len(vols)} volume(s); returned {type(r).__name__}")

    # a controller that applies on reset (Ironic's path for one that offers only OnReset)
    payload = {"RAIDType": "RAID5", "Links": {"Drives": [{"@odata.id": d._path} for d in disks[2:5]]}, "Name": "ironic-data"}
    tm = vc.create(payload, apply_time=sushy.APPLY_TIME_ON_RESET)
    pending = tm is not None and tm.is_processing
    record("raid.create.onreset.pending", pending and not any(v["name"] == "ironic-data" and not v["pending"] for v in twin_device(path)["volumes"]),
           f"task monitor {getattr(tm, 'task_monitor_uri', None)} processing={pending}")
    system.reset_system(sushy.RESET_FORCE_RESTART)
    state, status = wait_task(root, tm.task_monitor_uri, "RAID5 on reset")
    model = twin_device(path)
    record("raid.create.onreset.applied", str(getattr(state, "value", state)) == "Completed"
           and any(v["name"] == "ironic-data" and v["raidType"] == "RAID5" and not v["pending"] for v in model["volumes"]),
           f"after the reset: task {getattr(state, 'value', state)}/{getattr(status, 'value', status)}")

    # _submit_delete_configuration: only volumes with a RAIDType, at Ironic's apply time
    deleted = 0
    for v in fresh_volumes(system, storage.identity).get_members():
        if v.raid_type:
            v.delete(apply_time=sushy.APPLY_TIME_IMMEDIATE)
            deleted += 1
    model = twin_device(path)
    record("raid.delete", deleted == 2 and not model["volumes"], f"deleted {deleted}; the model holds {len(model['volumes'])}")


if __name__ == "__main__":
    try:
        main()
    except Exception as e:  # report, then fail
        record("error", False, repr(e))
    fails = [r for r in results if r["result"] == "FAIL"]
    print(f"\nstorage-raid: {len(results) - len(fails)} PASS, {len(fails)} FAIL")
    sys.exit(1 if fails else 0)

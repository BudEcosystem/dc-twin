#!/usr/bin/env python3
# Drives OpenStack Ironic -- the real service, unmodified, standalone -- against dc-twin's emulated
# BMCs, through Ironic's REST API (the calls the `baremetal` CLI makes), and confirms every action
# on the twin's side: the canonical request events Ironic's Redfish traffic produced, and the
# model's state (`dctwin get` / `dctwin events`), not only what Ironic says back.
#
# Usage: flow.py CREDENTIALS.json --api URL [--system-id DEVICE=ID ...] [--addressing MODE]
#                [--ca FILE] [--ironic-log FILE] [--json-out FILE] [--evidence FILE]
#   CREDENTIALS.json is `dctwin credentials <selector>`: [{path, user, password, redfish, siteIP}].
#   --addressing: "ip" (default) enrolls each BMC at its leased site address, https://<siteIP>,
#   with certificate verification off (as Metal3's disableCertificateVerification); "hostname"
#   at https://<rack>-<device>-bmc, verified against --ca (the estate CA); both need Ironic to
#   run with HTTPS_PROXY set to the twin's site gateway. "loopback" uses the host:port directly.
#   DCTWIN (default "dctwin") and DCTWIN_STATE locate the running estate.
# Exit status 1 when any step FAILs.
import argparse
import json
import os
import re
import subprocess
import sys
import threading
import time

import requests

WAIT = float(os.environ.get("FLOW_WAIT", "180"))
# a little over [conductor]sync_power_state_interval: long enough for one power sync to run
SYNC = float(os.environ.get("FLOW_SYNC", "12"))
# a BIOS step with its reboot takes 10-80 s here; a node still in clean wait after this is stuck
CLEAN_WAIT = float(os.environ.get("FLOW_CLEAN_WAIT", "180"))

# the lifecycle, run node by node
LIFECYCLE = ["enroll", "validate", "manage", "bios.cached", "firmware.cached", "power.off", "power.on",
             "boot.pxe", "reboot", "boot.consumed", "inspect", "bios.clean", "bios.applied", "provide"]
# Ironic reacting to the twin, run on every node at once (they wait on Ironic's periodic tasks)
REACTIONS = ["sync.twin-off", "sync.twin-on", "health.degraded", "health.recovered",
             "bmc.unreachable", "bmc.maintenance", "bmc.recovered"]
OPS = LIFECYCLE + REACTIONS
WRITES = ("power.", "boot.", "bios.", "secureboot.", "firmware.", "account.", "vmedia.")


class Fail(Exception):
    pass


class XFail(Exception):
    """Ironic fails here exactly as it would on the hardware the BMC was captured from."""


class Blocked(Exception):
    pass


class Skip(Exception):
    pass


def await_(cond, what, timeout=WAIT, every=1.0):
    end = time.monotonic() + timeout
    while True:
        v = cond()
        if v:
            return v
        if time.monotonic() > end:
            raise Fail(f"timed out after {timeout:.0f}s waiting for {what}")
        time.sleep(every)


# ------------------------------------------------------------------------------------------ twin

class Twin:
    def __init__(self, exe):
        self.exe = exe

    def _run(self, *args):
        out = subprocess.run([self.exe, *args], capture_output=True, text=True, check=True).stdout
        return json.loads(out) if out.strip() else None

    def device(self, path):
        for v in self._run("get", path, "--json") or []:
            if v["path"] == path:
                return v
        raise Fail(f"twin has no device {path}")

    def seq(self):
        return self._run("status", "--json")["seq"]

    def events(self, path, since, reads=False):
        args = ["events", "--select", path, "--since", str(since), "--json"] + ([] if reads else ["--no-reads"])
        return [e for e in (self._run(*args) or []) if e.get("path") == path]

    def inspect(self, path, rf):
        """What the BMC serves at a Redfish path, rendered in-process (never in the request log)."""
        return self._run("inspect", path, rf)

    def power(self, verb, path):
        return self._run("power", verb, path)

    def fault(self, path, fault, fid):
        return self._run("fault", "inject", path, fault, "--id", fid)

    def clear(self, fid):
        return self._run("fault", "clear", fid)


def redfish_requests(evs, canonical=None):
    return [e for e in evs if e.get("kind") == "request" and (e.get("request") or {}).get("protocol") == "redfish"
            and (canonical is None or e["request"].get("canonical") == canonical)]


def writes_of(evs):
    return [e for e in redfish_requests(evs) if e["request"].get("method") in ("POST", "PATCH", "PUT", "DELETE")
            and (e["request"].get("canonical") or "").startswith(WRITES)]


def states_of(evs, field, to=None):
    return [e for e in evs if e.get("kind") == "state" and (e.get("state") or {}).get("field") == field
            and (to is None or e["state"].get("to") == to)]


def walk(evs, field="power"):
    ps = states_of(evs, field)
    return " -> ".join([ps[0]["state"]["from"]] + [e["state"]["to"] for e in ps]) if ps else "(none)"


def canon(evs):
    """The canonical names of the Redfish writes, in order, for a note."""
    return ", ".join(f"{e['request']['canonical']}" for e in writes_of(evs)) or "(no writes)"


# ---------------------------------------------------------------------------------------- ironic

class Ironic:
    def __init__(self, url):
        self.url = url.rstrip("/")
        self.s = requests.Session()
        self.s.trust_env = False  # the API is local; HTTPS_PROXY is for Ironic's BMC traffic only
        root = self.s.get(self.url + "/v1", timeout=30).json()
        self.version = root["version"]["version"]
        self.s.headers.update({"X-OpenStack-Ironic-API-Version": self.version})

    def call(self, method, path, body=None, ok=(200, 201, 202, 204)):
        # Ironic on SQLite serialises writers; while a power sync waits on a dead BMC an API read
        # can wait on the database for a minute or more, so reads get a long timeout and a retry
        for attempt in range(3 if method == "GET" else 1):
            try:
                r = self.s.request(method, self.url + path, json=body, timeout=180)
                break
            except requests.exceptions.ReadTimeout:
                if attempt == 2 or method != "GET":
                    raise
        if r.status_code not in ok:
            raise Fail(f"{method} {path} -> {r.status_code}: {r.text[:300]}")
        return r.json() if r.content else None

    def node(self, name):
        return self.call("GET", f"/v1/nodes/{name}")

    def provision(self, name, target, **extra):
        self.call("PUT", f"/v1/nodes/{name}/states/provision", dict(target=target, **extra))

    def power(self, name, target):
        self.call("PUT", f"/v1/nodes/{name}/states/power", {"target": target})

    def history(self, name):
        return self.call("GET", f"/v1/nodes/{name}/history?detail=True")["history"]

    def wait_state(self, name, want, failed=(), timeout=WAIT):
        def done():
            n = self.node(name)
            if n["provision_state"] in failed:
                return n
            if n["provision_state"] == want and n["target_provision_state"] is None:
                return n
            return None
        n = await_(done, f"provision state {want}", timeout)
        if n["provision_state"] != want:
            raise Fail(f"provision state {n['provision_state']}: {n['last_error']}")
        return n

    def wait_power(self, name, want, timeout=WAIT):
        return await_(lambda: (lambda n: n if n["power_state"] == want and n["target_power_state"] is None else None)(self.node(name)),
                      f"Ironic power_state {want!r}", timeout)


def log_lines(logfile, uuid, *patterns, limit=3):
    """Ironic conductor log lines about one node that match every pattern (evidence for a note)."""
    if not logfile or not os.path.exists(logfile):
        return []
    out = []
    with open(logfile, errors="replace") as f:
        for line in f:
            if uuid in line and all(p in line for p in patterns):
                # drop the timestamp's date, the pid and the request context, keep level and message
                m = re.match(r"\S+ (\S+) \d+ (\w+) (\S+) \[[^\]]*\] (.*)", line.rstrip())
                if m:
                    # DEBUG lines end with the function and the source file: drop them
                    msg = re.sub(r" \w+ /\S+\.py:\d+$", "", m.group(4))
                    out.append(f"{m.group(1)} {m.group(2)} {msg}")
                else:
                    out.append(line.rstrip())
    return out[-limit:]


# ------------------------------------------------------------------------------------------ node

class Node:
    def __init__(self, twin, ironic, cred, system_id, addressing, ca, logfile):
        self.twin, self.ir, self.cred, self.system_id = twin, ironic, cred, system_id
        self.path = cred["path"]
        self.short = self.path.split("/", 2)[-1]
        self.name = self.short.replace("/", "-")
        self.addressing, self.ca, self.logfile = addressing, ca, logfile
        self.results, self.evidence = {}, {}
        self.uuid = None
        self.bios_change = None
        self.bios_path = None

    def record(self, op, fn):
        t0 = time.monotonic()
        try:
            note = fn()
            self.results[op] = ("PASS", note or "")
        except XFail as e:
            self.results[op] = ("XFAIL", str(e))
        except Skip as e:
            self.results[op] = ("SKIP", str(e))
        except Blocked as e:
            self.results[op] = ("BLOCK", str(e))
        except Fail as e:
            self.results[op] = ("FAIL", str(e))
        except Exception as e:  # HTTP errors, protocol surprises
            self.results[op] = ("FAIL", f"{type(e).__name__}: {e}"[:400])
        self.results[op] = self.results[op] + (round(time.monotonic() - t0, 1),)

    def ok(self, op):
        return self.results.get(op, ("",))[0] == "PASS"

    def needs(self, *ops):
        for op in ops:
            if not self.ok(op):
                raise Blocked(f"needs {op}")

    def evid(self, op, *lines):
        self.evidence.setdefault(op, []).extend(lines)

    # what the BMC serves, read in-process
    def system_rf(self):
        if self.system_id:
            return self.system_id
        members = self.twin.inspect(self.path, "/redfish/v1/Systems")["Members"]
        return members[0]["@odata.id"]

    # ---------------------------------------------------------------------------- lifecycle

    def op_enroll(self):
        verify = False
        if self.addressing == "ip":
            addr = f"https://{self.cred['siteIP']}"
        elif self.addressing == "hostname":
            addr, verify = f"https://{self.name}-bmc", self.ca or True
        else:
            addr = f"https://{self.cred['redfish']}"
        info = {"redfish_address": addr, "redfish_username": self.cred["user"],
                "redfish_password": self.cred["password"], "redfish_verify_ca": verify}
        if self.system_id:
            info["redfish_system_id"] = self.system_id
        refused = ""
        if self.addressing == "hostname" and self.ca and not getattr(self, "tls_checked", False):
            refused = self.untrusted(dict(info, redfish_verify_ca=True))
        n = self.ir.call("POST", "/v1/nodes", {"name": self.name, "driver": "redfish", "driver_info": info})
        self.uuid = n["uuid"]
        if n["provision_state"] != "enroll":
            raise Fail(f"enrolled in {n['provision_state']}")
        ifaces = ", ".join(f"{k.split('_')[0]}={n[k]}" for k in ("power_interface", "management_interface", "inspect_interface",
                                                                 "bios_interface", "firmware_interface", "boot_interface"))
        tls = "TLS verified against the estate CA" if verify else "TLS unverified"
        if refused:
            tls += f"; the same BMC trusted with the system CAs only is refused: {refused}"
        return f"{self.uuid} at {addr}{' system ' + self.system_id if self.system_id else ''} ({tls}); {ifaces}"

    def untrusted(self, info):
        """The estate CA is what makes the BMC's certificate valid: the same enrollment trusting
        only the system CAs must fail verification when Ironic first talks to the BMC."""
        self.tls_checked = True
        name = f"{self.name}-untrusted"
        self.ir.call("POST", "/v1/nodes", {"name": name, "driver": "redfish", "driver_info": info})
        try:
            self.ir.provision(name, "manage")
            n = await_(lambda: (lambda x: x if x["target_provision_state"] is None else None)(self.ir.node(name)),
                       "the untrusted enrollment to settle", 120)
            if n["provision_state"] != "enroll" or "CERTIFICATE_VERIFY_FAILED" not in (n["last_error"] or ""):
                raise Fail(f"an enrollment without the estate CA reached {n['provision_state']}: {n['last_error']}")
            return "CERTIFICATE_VERIFY_FAILED"
        finally:
            self.ir.call("DELETE", f"/v1/nodes/{name}")

    def op_validate(self):
        v = self.ir.call("GET", f"/v1/nodes/{self.name}/validate")
        need = ["power", "management", "inspect", "bios", "firmware"]
        bad = [f"{k}: {v[k].get('reason')}" for k in need if not v.get(k, {}).get("result")]
        if bad:
            raise Fail("; ".join(bad))
        other = [k for k in ("boot", "deploy") if not v[k]["result"]]
        return ", ".join(f"{k} true" for k in need) + (f"; {'/'.join(other)} false (no deploy image given: expected)" if other else "")

    def op_manage(self):
        since = self.twin.seq()
        self.ir.provision(self.name, "manage")
        n = self.ir.wait_state(self.name, "manageable", failed=("enroll",))
        dev = self.twin.device(self.path)
        want = {"On": "power on", "Off": "power off"}[dev["power"]]
        if n["power_state"] != want:
            raise Fail(f"Ironic power_state {n['power_state']!r} but the model has {dev['power']}")
        evs = self.twin.events(self.path, since, reads=True)
        reqs = redfish_requests(evs)
        users = sorted({(e.get("source") or {}).get("principal") for e in reqs} - {None, "anonymous", "login"})
        if self.cred["user"] not in users:
            raise Fail(f"no authenticated Redfish request as {self.cred['user']!r} ({len(reqs)} requests, principals {users})")
        if writes_of(evs):
            raise Fail(f"verify wrote to the BMC: {canon(evs)}")
        # Ironic caches one Redfish session per (address, user): a new session only on first contact
        sess = "a new session" if redfish_requests(evs, "session.create") else "its cached session"
        return (f"manageable, power_state {n['power_state']!r} = model {dev['power']}, health {n.get('health')!r}, "
                f"vendor {n['properties'].get('vendor')!r}; twin: {len(reqs)} Redfish reads as {users!r} on {sess}, 0 writes")

    def op_bios_cached(self):
        self.needs("manage")
        settings = self.ir.call("GET", f"/v1/nodes/{self.name}/bios?detail=True")["bios"]
        if not settings:
            raise Fail("Ironic cached no BIOS settings")
        sysj = self.twin.inspect(self.path, self.system_rf())
        if "Bios" not in sysj:
            raise Skip("the system links no Bios")
        self.bios_path = sysj["Bios"]["@odata.id"]
        attrs = self.twin.inspect(self.path, self.bios_path).get("Attributes") or {}
        # Ironic stores every setting as a string, booleans as "1"/"0"
        norm = lambda v: str(int(v)) if isinstance(v, bool) else str(v)
        diff = [s["name"] for s in settings if norm(attrs.get(s["name"])) != str(s["value"])]
        if diff or len(settings) != len(attrs):
            raise Fail(f"Ironic cached {len(settings)} settings, the BMC serves {len(attrs)}; differing: {diff[:5]}")
        reg = sum(1 for s in settings if s.get("attribute_type"))
        return f"{len(settings)} settings cached, all equal to {self.bios_path} Attributes; {reg} with registry metadata"

    def op_firmware_cached(self):
        self.needs("manage")
        comps = {c["component"]: c["current_version"] for c in self.ir.call("GET", f"/v1/nodes/{self.name}/firmware")["firmware"]}
        if not comps:
            raise Fail("Ironic cached no firmware components")
        model = {k: v["version"] for k, v in (self.twin.device(self.path).get("firmware") or {}).items()}
        bad = [f"{k}: Ironic {v!r}, model {model[k]!r}" for k, v in comps.items() if k in model and model[k] != v]
        if bad:
            raise Fail("; ".join(bad))
        return ", ".join(f"{k} {v}" for k, v in sorted(comps.items())) + " (equal to the model's firmware)"

    def _power(self, target, model_to, canonical):
        since = self.twin.seq()
        self.ir.power(self.name, target)
        n = self.ir.wait_power(self.name, target)
        if n["last_error"]:
            raise Fail(f"last_error {n['last_error']}")
        # Ironic maps PoweringOn to "power on" (GET_POWER_STATE_MAP), so it can report success while
        # the host is still on its way; the model must get there within a few seconds
        dev = await_(lambda: (lambda d: d if d["power"] == model_to else None)(self.twin.device(self.path)),
                     f"the model to reach {model_to}", 20)
        evs = await_(lambda: (lambda e: e if states_of(e, "power", model_to) else None)(self.twin.events(self.path, since)),
                     f"a power -> {model_to} state event", 20)
        if not redfish_requests(evs, canonical):
            raise Fail(f"no '{canonical}' from protocol redfish (writes: {canon(evs)})")
        return evs, dev

    def op_power_off(self):
        self.needs("manage")
        evs, dev = self._power("power off", "Off", "power.off.force")
        return f"Ironic 'power off'; twin: {canon(evs)} (redfish), power {walk(evs)}, model {dev['power']}"

    def op_power_on(self):
        self.needs("manage")
        evs, dev = self._power("power on", "On", "power.on")
        return f"Ironic 'power on'; twin: {canon(evs)} (redfish), power {walk(evs)}, model {dev['power']}"

    def op_boot_pxe(self):
        self.needs("manage")
        since = self.twin.seq()
        self.ir.call("PUT", f"/v1/nodes/{self.name}/management/boot_device", {"boot_device": "pxe", "persistent": False})
        got = self.ir.call("GET", f"/v1/nodes/{self.name}/management/boot_device")
        dev = self.twin.device(self.path)
        evs = self.twin.events(self.path, since)
        b = dev["boot"]
        if b.get("overrideTarget") != "Pxe" or b.get("overrideEnabled") != "Once":
            raise Fail(f"model boot {b}; twin logged {canon(evs)}")
        if not redfish_requests(evs, "boot.override"):
            raise Fail(f"no boot.override from protocol redfish (writes: {canon(evs)})")
        if got.get("boot_device") != "pxe":
            raise Fail(f"Ironic reads back {got}")
        tgt = [e["request"]["method"] + " " + e["request"]["target"] for e in redfish_requests(evs, "boot.override")]
        return f"Ironic boot_device {got}; twin: boot.override ({tgt[0]}), model Pxe/Once"

    def op_reboot(self):
        self.needs("power.on")
        self.reboot_since = since = self.twin.seq()
        self.ir.power(self.name, "rebooting")
        n = self.ir.wait_power(self.name, "power on")
        if n["last_error"]:
            raise Fail(f"last_error {n['last_error']}")

        def cycled():
            e = self.twin.events(self.path, since)
            ps = states_of(e, "power")
            return e if any(x["state"]["from"] == "On" for x in ps) and ps[-1]["state"]["to"] == "On" else None
        evs = await_(cycled, "the host to leave On and come back On", 30)
        names = [e["request"]["canonical"] for e in writes_of(evs) if e["request"]["canonical"].startswith("power.")]
        if not names:
            raise Fail("no power request from protocol redfish")
        return f"Ironic 'rebooting' -> 'power on'; twin: {' then '.join(names)} (redfish), power {walk(evs)}"

    def op_boot_consumed(self):
        self.needs("boot.pxe", "reboot")
        dev = self.twin.device(self.path)
        evs = self.twin.events(self.path, self.reboot_since)
        if dev["boot"].get("overrideEnabled") != "Disabled" or not states_of(evs, "boot.overrideEnabled", "Disabled"):
            raise Fail(f"model boot after the reboot is {dev['boot']}")
        got = self.ir.call("GET", f"/v1/nodes/{self.name}/management/boot_device")
        return f"the one-time PXE override was used by Ironic's reboot (model booted {dev.get('booted')!r}, override Disabled); Ironic now reads {got}"

    def op_inspect(self):
        self.needs("manage")
        since = self.inspect_since = self.twin.seq()
        self.ir.provision(self.name, "inspect")
        n = self.ir.wait_state(self.name, "manageable", failed=("inspect failed",))
        p = n["properties"]
        evs = self.twin.events(self.path, since)
        ports = self.ir.call("GET", f"/v1/ports?node={self.uuid}&detail=True")["ports"]
        inv = self.ir.call("GET", f"/v1/nodes/{self.name}/inventory")["inventory"]
        if writes_of(evs):
            raise Fail(f"inspection wrote to the BMC: {canon(evs)}")
        # what the BMC serves: memory and the system's NIC MACs
        sysj = self.twin.inspect(self.path, self.system_rf())
        gib = (sysj.get("MemorySummary") or {}).get("TotalSystemMemoryGiB")
        macs = set()
        if "EthernetInterfaces" in sysj:
            for m in self.twin.inspect(self.path, sysj["EthernetInterfaces"]["@odata.id"]).get("Members", []):
                mac = (self.twin.inspect(self.path, m["@odata.id"]).get("MACAddress") or "").lower()
                if re.fullmatch(r"[0-9a-f]{2}(:[0-9a-f]{2}){5}", mac):
                    macs.add(mac)
        got = {pt["address"].lower() for pt in ports}
        problems = []
        if gib and int(p.get("memory_mb", 0)) != int(gib * 1024):
            problems.append(f"memory_mb {p.get('memory_mb')} but the BMC says {gib} GiB")
        if got != macs:
            problems.append(f"ports {sorted(got)} but the BMC's EthernetInterfaces have {sorted(macs)}")
        if problems:
            raise Fail("; ".join(problems))
        vend = inv.get("system_vendor") or {}
        dev = self.twin.device(self.path)
        serial = vend.get("serial_number")
        ident = "serial = model" if serial and serial == dev["identity"]["serial"] else f"serial {serial!r} (model {dev['identity']['serial']!r})"
        self.evid("inspect", f"properties {json.dumps({k: p.get(k) for k in ('memory_mb', 'cpus', 'cpu_arch', 'local_gb', 'capabilities', 'vendor')})}",
                  f"ports {sorted(got)}",
                  f"inventory cpu {json.dumps(inv.get('cpu'))}, system_vendor {json.dumps(vend)}")
        return (f"memory_mb {p.get('memory_mb')}, cpus {p.get('cpus', inv.get('cpu', {}).get('count'))}, cpu_arch {p.get('cpu_arch')!r}, "
                f"local_gb {p.get('local_gb')}, {len(ports)} ports = the BMC's NIC MACs, {ident}; twin: 0 writes")

    def inspect_failed(self):
        """GB trays: the captured host system has no EthernetInterfaces, so Ironic's
        validate-interfaces hook fails inspection, as it would on the captured hardware."""
        n = self.ir.node(self.name)
        sysj = self.twin.inspect(self.path, self.system_rf())
        if n["provision_state"] == "inspect failed" and "EthernetInterfaces" not in sysj and "interfaces" in (n["last_error"] or "").lower():
            # Ironic's inspection error handler powers the node off; then back to manageable and
            # on again for the steps that follow
            evs = self.twin.events(self.path, self.inspect_since)
            off = [e["request"]["canonical"] for e in writes_of(evs)]
            self.ir.provision(self.name, "manage")
            self.ir.wait_state(self.name, "manageable", failed=("enroll", "inspect failed"))
            self.ir.power(self.name, "power on")
            self.ir.wait_power(self.name, "power on")
            err = n["last_error"].split("Reason: ")[-1]
            raise XFail(f"{err} -- the captured {self.system_rf()} has no EthernetInterfaces; Ironic then powered the node "
                        f"off (twin: {', '.join(off) or 'no write'}); returned to manageable and powered on")

    def _pick_bios(self):
        settings = self.ir.call("GET", f"/v1/nodes/{self.name}/bios?detail=True")["bios"]
        flips = {"Enabled": "Disabled", "Disabled": "Enabled"}
        cand = [s for s in settings if s["value"] in flips and not s.get("read_only")
                and (not s.get("allowable_values") or flips[s["value"]] in s["allowable_values"])]
        cand.sort(key=lambda s: (0 if any(t in s["name"].lower() for t in ("sriov", "virtual", "vtd", "vt-d")) else 1, s["name"]))
        if not cand:
            raise Skip("no writable Enabled/Disabled BIOS attribute")
        s = cand[0]
        return s["name"], s["value"], flips[s["value"]]

    def op_bios_clean(self):
        self.needs("manage")
        if self.bios_path is None:
            self.bios_path = self.twin.inspect(self.path, self.system_rf()).get("Bios", {}).get("@odata.id")
        name, old, new = self._pick_bios()
        since = self.twin.seq()
        step = {"interface": "bios", "step": "apply_configuration", "args": {"settings": [{"name": name, "value": new}]}}
        self.ir.provision(self.name, "clean", clean_steps=[step], disable_ramdisk=True)
        try:
            n = self.ir.wait_state(self.name, "manageable", failed=("clean failed",), timeout=CLEAN_WAIT)
        except Fail:
            n = self.ir.node(self.name)
            if n["provision_state"] != "clean wait":
                raise
            self.bios_stuck(name, new, since)
        evs = self.twin.events(self.path, since)
        sets = redfish_requests(evs, "bios.set")
        if not sets or not states_of(evs, "bios.pending"):
            raise Fail(f"no bios.set request / bios.pending state in the model; writes: {canon(evs)}")
        self.bios_change = (name, old, new, since)
        tgt = f"{sets[0]['request']['method']} {sets[0]['request']['target']}"
        self.evid("bios.clean", f"clean step bios.apply_configuration {name}={new} with disable_ramdisk; Ironic: {n['provision_state']}",
                  f"twin writes in order: {canon(evs)}", f"twin power: {walk(evs)}")
        return f"{name}: {old} -> {new}, manual clean (disable_ramdisk) -> manageable; twin: {canon(evs)} ({tgt}), power {walk(evs)}"

    def bios_stuck(self, name, new, since):
        """Ironic 39 finishes a BIOS step after the reboot only once the system's BootProgress
        reaches OSBootStarted/OSRunning (when the BMC reports BootProgress at all), and it keeps
        the step alive while it is reported. Say what the BMC and the model show, then recover as
        an operator must -- the step is not abortable: maintenance, delete, enroll, manage."""
        sysj = self.twin.inspect(self.path, self.system_rf())
        bp = (sysj.get("BootProgress") or {}).get("LastState")
        dev = self.twin.device(self.path)
        evs = self.twin.events(self.path, since)
        applied = states_of(evs, "bios")
        waits = log_lines(self.logfile, self.uuid, "Waiting for boot progress to reach OS started", limit=1)
        self.evid("bios.clean", f"twin writes: {canon(evs)}; power {walk(evs)}; model booted {dev.get('booted')!r}",
                  f"BMC BootProgress.LastState {bp!r} (the capture's value)", *waits)
        self.ir.call("PUT", f"/v1/nodes/{self.name}/maintenance", {"reason": "BIOS step waiting on BootProgress"})
        self.ir.call("DELETE", f"/v1/nodes/{self.name}")
        self.op_enroll()
        self.ir.provision(self.name, "manage")
        self.ir.wait_state(self.name, "manageable", failed=("enroll",))
        raise Fail(f"{name}={new}: Ironic still in clean wait after {CLEAN_WAIT:.0f}s, polling BootProgress "
                   f"(log: 'boot progress: ... Waiting for boot progress to reach OS started'); the BMC serves "
                   f"BootProgress.LastState {bp!r} although the model {'applied the setting and ' if applied else ''}"
                   f"booted {dev.get('booted')!r} (twin bug: BootProgress is the capture's, not the model's). "
                   f"Recovered: maintenance, delete, re-enroll ({self.uuid}), manage")

    def op_bios_applied(self):
        self.needs("bios.clean")
        name, old, new, since = self.bios_change
        evs = self.twin.events(self.path, since)
        applied = states_of(evs, "bios")
        if not applied:
            raise Fail("the model applied no pending BIOS settings")
        attrs = self.twin.inspect(self.path, self.bios_path).get("Attributes") or {}
        cached = self.ir.call("GET", f"/v1/nodes/{self.name}/bios/{name}")
        cached = cached.get(name, cached)
        if attrs.get(name) != new:
            raise Fail(f"the BMC serves {name}={attrs.get(name)} after the reboot")
        if cached.get("value") != new:
            raise Fail(f"Ironic's cached setting is {cached.get('value')!r}")
        # the bios state event follows a power transition: the setting applied on Ironic's reboot
        pw = [e["seq"] for e in states_of(evs, "power", "On")]
        order = "after the reboot's power On" if pw and applied[0]["seq"] >= min(pw) else f"(bios event seq {applied[0]['seq']}, power On seqs {pw})"
        return f"model applied {name}={new} {order}; BMC Attributes and Ironic's cache both read {new}"

    def op_provide(self):
        self.needs("manage")
        since = self.twin.seq()
        self.ir.provision(self.name, "provide")
        n = self.ir.wait_state(self.name, "available", failed=("clean failed",))
        evs = self.twin.events(self.path, since)
        if writes_of(evs):
            raise Fail(f"provide wrote to the BMC: {canon(evs)}")
        return f"available (automated_clean false: no cleaning), power_state {n['power_state']!r}; twin: 0 writes"

    def run_lifecycle(self):
        for op in LIFECYCLE:
            if op != "enroll" and not self.ok("enroll"):
                self.results[op] = ("BLOCK", "not enrolled", 0)
                continue
            fn = getattr(self, "op_" + op.replace(".", "_").replace("-", "_"))
            if op == "inspect":
                def fn(f=fn):
                    try:
                        return f()
                    except Fail:
                        self.inspect_failed()
                        raise
            self.record(op, fn)
            print(f"  {self.short:20} {op:18} {self.results[op][0]}", file=sys.stderr, flush=True)


# ------------------------------------------------------------------------------------- reactions

def react(nodes, op, fn):
    """Run fn(node) on every node at once, each recording op."""
    threads = []
    for n in nodes:
        if not n.ok("enroll"):
            n.results[op] = ("BLOCK", "not enrolled", 0)
            continue
        t = threading.Thread(target=n.record, args=(op, lambda n=n: fn(n)))
        t.start()
        threads.append(t)
    for t in threads:
        t.join()
    for n in nodes:
        print(f"  {n.short:20} {op:18} {n.results[op][0]}", file=sys.stderr, flush=True)


def r_twin_off(n):
    n.needs("manage")
    if n.twin.device(n.path)["power"] != "On":
        n.ir.power(n.name, "power on")
        n.ir.wait_power(n.name, "power on")
    since = n.twin.seq()
    t0 = time.monotonic()
    n.twin.power("off", n.path)
    node = n.ir.wait_power(n.name, "power off", timeout=90)
    dt = time.monotonic() - t0
    evs = n.twin.events(n.path, since)
    acts = [e for e in evs if e.get("kind") == "act"]
    if writes_of(evs):
        raise Fail(f"Ironic wrote to the BMC after the twin's act: {canon(evs)}")
    lines = log_lines(n.logfile, n.uuid, "sync_power_state", "Updating recorded state to 'power off'")
    if not lines:
        raise Fail("no sync_power_state line in the conductor log")
    n.evid("sync.twin-off", f"twin act: {acts[0]['request']['canonical']} via {acts[0]['request']['protocol']}" if acts else "twin act", *lines)
    # Ironic counts a recorded change as a sync attempt until a later sync confirms it; a second
    # change before that (with power_state_sync_max_retries = 1) is "max retries exceeded" and
    # maintenance. An operator's next act comes after a confirming sync, so wait for one.
    time.sleep(SYNC)
    return (f"twin `power off` ({acts[0]['request']['canonical'] if acts else '?'}, protocol control) -> Ironic power_state "
            f"'power off' after {dt:.0f}s via its power sync; no Redfish write from Ironic; provision {node['provision_state']}")


def r_twin_on(n):
    n.needs("sync.twin-off")
    since = n.twin.seq()
    t0 = time.monotonic()
    n.twin.power("on", n.path)
    n.ir.wait_power(n.name, "power on", timeout=90)
    dt = time.monotonic() - t0
    evs = n.twin.events(n.path, since)
    if writes_of(evs):
        raise Fail(f"Ironic wrote to the BMC: {canon(evs)}")
    lines = log_lines(n.logfile, n.uuid, "sync_power_state", "Updating recorded state to 'power on'")
    n.evid("sync.twin-on", *lines)
    return f"twin `power on` -> Ironic 'power on' after {dt:.0f}s; no Redfish write from Ironic"


def health_target(n):
    dev = n.twin.device(n.path)
    psus = [c["name"] for c in dev["components"] if c["kind"] == "psu"]
    if psus:
        return f"{n.path}/{psus[0]}", "generic/power/psu-failure"
    gpus = [c["name"] for c in dev["components"] if c["kind"] == "accelerator"]
    if gpus:
        return f"{n.path}/{gpus[0]}", "nvidia/xid/xid-79"
    raise Skip("no PSU or GPU to fail")


def r_health_degraded(n):
    n.needs("enroll")
    target, fault = health_target(n)
    fid = f"ironic-health-{n.name}"
    n.twin.fault(target, fault, fid)
    n.health_fault = fid
    sysj = n.twin.inspect(n.path, n.system_rf())
    want = (sysj.get("Status") or {}).get("Health")
    t0 = time.monotonic()
    if want in (None, "OK"):
        time.sleep(25)  # two power syncs
        node = n.ir.node(n.name)
        raise Fail(f"{fault} on {target.split('/')[-1]}: the BMC's {n.system_rf()} Status.Health stays {want!r} "
                   f"(model health {n.twin.device(n.path)['health']}); Ironic health {node.get('health')!r}")
    node = await_(lambda: (lambda x: x if x.get("health") == want else None)(n.ir.node(n.name)),
                  f"Ironic health {want!r}", 90)
    dt = time.monotonic() - t0
    hist = [h["event"] for h in n.ir.history(n.name) if "health" in h["event"]]
    n.evid("health.degraded", f"twin fault {fault} on {target}", *(hist[-1:]))
    return f"{fault} on {target.split('/')[-1]} -> BMC Status.Health {want} -> Ironic health {node['health']!r} after {dt:.0f}s; history: {hist[-1] if hist else '-'}"


def r_health_recovered(n):
    n.needs("health.degraded")
    n.twin.clear(n.health_fault)
    t0 = time.monotonic()
    node = await_(lambda: (lambda x: x if x.get("health") == "OK" else None)(n.ir.node(n.name)), "Ironic health 'OK'", 90)
    hist = [h["event"] for h in n.ir.history(n.name) if "health" in h["event"]]
    return f"fault cleared -> Ironic health {node['health']!r} after {time.monotonic() - t0:.0f}s; history: {hist[-1] if hist else '-'}"


def r_bmc_unreachable(n):
    n.needs("enroll")
    pre = n.ir.node(n.name)
    if pre["maintenance"]:
        raise Fail(f"already in maintenance before the fault: {pre['maintenance_reason']}")
    fid = f"ironic-bmc-{n.name}"
    n.bmc_fault = fid
    n.unreach_t0 = time.monotonic()
    n.twin.fault(n.path, "generic/bmc/unreachable", fid)

    def warned():
        return log_lines(n.logfile, n.uuid, "During sync_power_state, could not get power state", limit=5)
    lines = await_(warned, "a failed power sync in the conductor log", 120)
    dt = time.monotonic() - n.unreach_t0
    n.evid("bmc.unreachable", *lines[:1])
    m = re.search(r"Error: (.*)", lines[0])
    return f"BMC drops every connection -> Ironic's power sync fails after {dt:.0f}s: {(m.group(1) if m else lines[0])[:160]}"


def r_bmc_maintenance(n):
    n.needs("bmc.unreachable")
    node = await_(lambda: (lambda x: x if x["maintenance"] else None)(n.ir.node(n.name)), "maintenance", 180)
    dt = time.monotonic() - n.unreach_t0
    if node["fault"] != "power failure":
        raise Fail(f"maintenance with fault {node['fault']!r}: {node['maintenance_reason']}")
    attempts = log_lines(n.logfile, n.uuid, "could not get power state", limit=10)
    errs = [h["event"] for h in n.ir.history(n.name) if h["severity"] == "ERROR"]
    n.evid("bmc.maintenance", *attempts[-1:], *log_lines(n.logfile, n.uuid, "Switching node to maintenance mode", limit=1))
    return (f"after {len(attempts)} failed sync(s) and the retry ({dt:.0f}s): maintenance true, fault 'power failure', "
            f"power_state {node['power_state']!r}, reason {node['maintenance_reason'][:110]!r}...; history ERROR events: {len(errs)}")


def r_bmc_recovered(n):
    n.needs("bmc.maintenance")
    since = n.twin.seq()
    n.twin.clear(n.bmc_fault)
    t0 = time.monotonic()
    node = await_(lambda: (lambda x: x if not x["maintenance"] and x["fault"] is None else None)(n.ir.node(n.name)),
                  "maintenance cleared by Ironic's power failure recovery", 120)
    dt = time.monotonic() - t0
    evs = n.twin.events(n.path, since, reads=True)
    lines = log_lines(n.logfile, n.uuid, "recovered", limit=2) or log_lines(n.logfile, n.uuid, "power failure", limit=2)
    n.evid("bmc.recovered", *lines)
    dev = n.twin.device(n.path)
    want = {"On": "power on", "Off": "power off"}[dev["power"]]
    if node["power_state"] != want:
        raise Fail(f"Ironic power_state {node['power_state']!r}, model {dev['power']}")
    return (f"fault cleared -> Ironic's power failure recovery lifted maintenance after {dt:.0f}s; power_state "
            f"{node['power_state']!r} = model; twin: {len(redfish_requests(evs))} Redfish reads, {len(writes_of(evs))} writes")


# ------------------------------------------------------------------------------------------ main

def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("credentials")
    ap.add_argument("--api", required=True)
    ap.add_argument("--system-id", action="append", default=[], help="DEVICE=SYSTEM_ID (Ironic's redfish_system_id)")
    ap.add_argument("--addressing", choices=("ip", "hostname", "loopback"), default="ip")
    ap.add_argument("--ca", help="CA bundle for --addressing hostname (the estate CA)")
    ap.add_argument("--ironic-log")
    ap.add_argument("--json-out")
    ap.add_argument("--evidence")
    a = ap.parse_args()
    twin = Twin(os.environ.get("DCTWIN", "dctwin"))
    ironic = Ironic(a.api)
    creds = json.load(open(a.credentials))
    sids = dict(x.split("=", 1) for x in a.system_id)
    nodes = []
    for c in creds:
        short = c["path"].split("/", 2)[-1]
        nodes.append(Node(twin, ironic, c, sids.get(c["path"]) or sids.get(short), a.addressing, a.ca, a.ironic_log))
    t0 = time.monotonic()
    for n in nodes:
        print(f"lifecycle {n.short} ({twin.device(n.path)['dialect']})", file=sys.stderr, flush=True)
        n.run_lifecycle()
    print("reactions (every node at once)", file=sys.stderr, flush=True)
    react(nodes, "sync.twin-off", r_twin_off)
    react(nodes, "sync.twin-on", r_twin_on)
    react(nodes, "health.degraded", r_health_degraded)
    react(nodes, "health.recovered", r_health_recovered)
    react(nodes, "bmc.unreachable", r_bmc_unreachable)
    react(nodes, "bmc.maintenance", r_bmc_maintenance)
    react(nodes, "bmc.recovered", r_bmc_recovered)
    elapsed = time.monotonic() - t0

    dialects = {n.short: twin.device(n.path)["dialect"] for n in nodes}
    cols = [n.short for n in nodes]
    w0 = max(len(o) for o in OPS + ["step"])
    ws = [max(len(c), len(dialects[c]), 5) for c in cols]
    line = lambda cells: "| " + " | ".join(x.ljust(w) for x, w in zip(cells, [w0] + ws)) + " |"
    print(f"Ironic {ironic_version()} (API {ironic.version}), BMCs addressed by {a.addressing}{' through the site gateway' if a.addressing != 'loopback' else ''}, {elapsed:.0f}s")
    print(line(["step"] + cols))
    print(line([""] + [dialects[c] for c in cols]))
    print("|" + "|".join("-" * (w + 2) for w in [w0] + ws) + "|")
    for op in OPS:
        print(line([op] + [n.results.get(op, ("-",))[0] for n in nodes]))
    kinds = ("PASS", "FAIL", "XFAIL", "BLOCK", "SKIP")
    totals = {k: sum(1 for n in nodes for op in OPS if n.results.get(op, ("",))[0] == k) for k in kinds}
    print("\n" + ", ".join(f"{v} {k}" for k, v in totals.items()) +
          "  (XFAIL: fails as on the captured hardware; BLOCK: a prerequisite failed; SKIP: not exposed)\n")
    for n in nodes:
        print(f"{n.short} ({dialects[n.short]}) node {n.name} {n.uuid}")
        for op in OPS:
            st, note, dt = n.results.get(op, ("-", "", 0))
            print(f"  {st:5} {op:{w0}} {dt:6.1f}s  {note}")
    if a.evidence:
        with open(a.evidence, "w") as f:
            for n in nodes:
                f.write(f"== {n.short} ({dialects[n.short]}) {n.uuid}\n")
                for op, lines in n.evidence.items():
                    for l in lines:
                        f.write(f"  [{op}] {l}\n")
                for h in ironic.history(n.name):
                    f.write(f"  [history] {h['created_at'][11:19]} {h['severity']:5} {h['event']}\n")
    if a.json_out:
        json.dump({n.short: {"dialect": dialects[n.short], "uuid": n.uuid,
                             "results": {op: list(v) for op, v in n.results.items()}, "evidence": n.evidence} for n in nodes},
                  open(a.json_out, "w"), indent=2)
    sys.exit(1 if totals["FAIL"] else 0)


def ironic_version():
    try:
        import importlib.metadata
        return importlib.metadata.version("ironic")
    except Exception:
        return "?"


if __name__ == "__main__":
    main()

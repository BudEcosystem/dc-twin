#!/usr/bin/env python3
# Drives Metal3's bare-metal operator (BMO) the way a Metal3 user does -- Secrets and
# BareMetalHosts in Kubernetes, `online`, the reboot annotation, deletion -- against dc-twin's
# emulated BMCs, and checks each effect three ways: in the twin's model and event log (`dctwin get`,
# `dctwin events`, `dctwin inspect`), in what Ironic recorded (read-only REST calls), and in BMO's
# status and the objects it creates (HardwareData, HostFirmwareSettings, FirmwareSchema,
# HostFirmwareComponents). Nothing here calls Ironic to change anything: BMO does that.
#
# BMO, Ironic, kube-apiserver and the twin are already running (run.sh starts them). This script:
#   1. creates a credentials Secret (from `dctwin credentials`) and a BareMetalHost per BMC, over
#      Redfish (redfish://, idrac-redfish://, ilo5-redfish://, at the BMC's leased site address) and
#      over IPMI (ipmi://, at the BMC's loopback port), and follows registration and inspection;
#   2. powers hosts off and on through `spec.online`, reboots an externally provisioned host with
#      the reboot annotation;
#   3. changes the twin underneath them: a power-off by the twin's operator, a failed PSU, an
#      unreachable BMC; then deletes hosts.
#
# Usage: flow.py --kubeconfig F --kubectl BIN --dctwin BIN --work DIR --ironic URL
#                [--ironic-log F] [--bmo-log F] [--kcm]
#   DCTWIN_STATE (and HOME, DCTWIN_ASSETS) locate the running estate. Prints a table and a note
#   per check; exit status 1 when any check FAILs.
import argparse
import json
import os
import re
import subprocess
import sys
import threading
import time
import urllib.request

NS = "metal3"
SITE = "site-1/hall-a/"
# Every host BMO is given. scheme is the BareMetalHost bmc.address scheme; inspection is
# spec.inspectionMode ("fast" is out-of-band inspection through the BMC, "agent" boots IPA).
HOSTS = [
    dict(name="generic-c02", device="c02/server-01", what="generic 2U, generated DMTF tree", scheme="redfish", inspection="fast"),
    dict(name="dell-b01", device="b01/server-01", what="Dell XE9680, captured iDRAC (Ironic's idrac hardware type)",
         scheme="idrac-redfish", inspection="disabled"),
    dict(name="smc-b02", device="b02/server-01", what="Supermicro SYS-821GE-TNHR, captured", scheme="redfish", inspection="fast"),
    dict(name="xcc-b05", device="b05/server-01", what="Lenovo SR670 V2, captured XCC", scheme="redfish", inspection="fast"),
    dict(name="ilo-c01", device="c01/server-02", what="HPE DL385 Gen10 Plus, captured iLO 5", scheme="ilo5-redfish", inspection="fast"),
    dict(name="gb300-a01", device="a01/compute-tray-01", what="NVIDIA GB300 tray, captured", scheme="redfish", inspection="fast",
         system="System_0"),
    dict(name="dell-c01-ext", device="c01/server-01", what="Dell R760, captured iDRAC; externally provisioned", scheme="redfish",
         inspection="fast", external=True),
    dict(name="generic-c02-ipmi", device="c02/server-02", what="generic 2U over IPMI, inspection disabled", scheme="ipmi",
         inspection="disabled"),
    dict(name="dell-b01-ipmi", device="b01/server-02", what="Dell XE9680 over IPMI, agent inspection", scheme="ipmi",
         inspection="agent"),
    dict(name="smc-b02-creds", device="b02/server-02", what="Supermicro, first given a wrong password", scheme="redfish",
         inspection="fast", badcreds=True),
    # the same iDRAC with out-of-band inspection, which BMO maps to an inspect interface Ironic's idrac
    # hardware type does not have (a finding about BMO and Ironic; no request reaches the BMC)
    dict(name="dell-b01-oob", device="b01/server-01", what="Dell XE9680 iDRAC, idrac-redfish with fast inspection",
         scheme="idrac-redfish", inspection="fast", finding="idrac-fast"),
]
STEPS = ["register", "credentials.bad", "credentials.fixed", "inspect", "firmware.settings", "firmware.components",
         "power.off", "power.on", "reboot", "twin.off", "health.critical", "health.ok", "bmc.lost", "bmc.back", "delete"]
WAIT = float(os.environ.get("FLOW_WAIT", "300"))
# BMO looks at a host in a steady state every 60 s (manageHostPower's steadyStateResult)
BMO_POLL = 60
# BMO's firmware settings and components controllers read Ironic again every 5 minutes; a first
# read that comes before Ironic has cached the BIOS settings leaves the list empty until the next
HFS_WAIT = 360


class Fail(Exception):
    pass


class XFail(Exception):
    """Fails exactly as it does on the hardware the BMC was captured from."""


class Cannot(Exception):
    """Cannot pass on a twin whose hosts are at `model` fidelity: nothing runs on them."""


class Blocked(Exception):
    pass


class Finding(Exception):
    """Behaviour of the software under test (BMO, Ironic), not of the twin."""


def await_(cond, what, timeout=WAIT, every=1.0):
    end = time.monotonic() + timeout
    while True:
        v = cond()
        if v:
            return v
        if time.monotonic() > end:
            raise Fail(f"timed out after {timeout:.0f}s waiting for {what}")
        time.sleep(every)


class Env:
    def __init__(self, a):
        self.a = a
        self.lock = threading.Lock()
        os.makedirs(os.path.join(a.work, "rendered"), exist_ok=True)
        os.makedirs(os.path.join(a.work, "evidence"), exist_ok=True)
        root = self.ironic_get("/v1")
        self.ironic_version = root["version"]["version"]

    # ------------------------------------------------------------------ kubernetes
    def kubectl(self, *args, stdin=None, check=True):
        p = subprocess.run([self.a.kubectl, "--kubeconfig", self.a.kubeconfig, *args], input=stdin,
                           capture_output=True, text=True)
        if check and p.returncode != 0:
            raise Fail(f"kubectl {' '.join(args)}: {p.stderr.strip()}")
        return p.stdout

    def get(self, kind, name):
        out = self.kubectl("get", kind, name, "-n", NS, "-o", "json", check=False)
        return json.loads(out) if out.strip() else None

    def apply(self, obj, fname):
        path = os.path.join(self.a.work, "rendered", fname)
        with open(path, "w") as f:
            json.dump(obj, f, indent=2)
        os.chmod(path, 0o600)
        return self.kubectl("apply", "-f", path).strip()

    def patch(self, kind, name, patch):
        return self.kubectl("patch", kind, name, "-n", NS, "--type", "merge", "-p", json.dumps(patch)).strip()

    # ------------------------------------------------------------------ ironic (reads only)
    def ironic_get(self, path):
        req = urllib.request.Request(self.a.ironic.rstrip("/") + path)
        if getattr(self, "ironic_version", None):
            req.add_header("X-OpenStack-Ironic-API-Version", self.ironic_version)
        # Ironic on SQLite serialises writers: while its power sync waits on dead BMCs a read can
        # time out on the database lock and answer 500 (compat/ironic saw the same), so reads retry
        for attempt in range(8):
            try:
                with urllib.request.urlopen(req, timeout=180) as r:
                    return json.loads(r.read() or b"null")
            except urllib.error.HTTPError as e:
                if e.code == 404:
                    return None
                if e.code < 500 or attempt == 7:
                    raise
                time.sleep(3)
            except OSError:
                if attempt == 7:
                    raise
                time.sleep(3)

    def node(self, name):
        return self.ironic_get(f"/v1/nodes/{NS}~{name}")

    # ------------------------------------------------------------------ twin
    def dctwin(self, *args):
        p = subprocess.run([self.a.dctwin, *args], capture_output=True, text=True)
        if p.returncode != 0:
            raise Fail(f"dctwin {' '.join(args)}: {p.stderr.strip() or p.stdout.strip()}")
        out = p.stdout.strip()
        try:
            return json.loads(out) if out else None
        except json.JSONDecodeError:
            return out

    def device(self, dev):
        return self.dctwin("get", dev, "--json")[0]

    def seq(self):
        return int(self.dctwin("status", "--json")["seq"])

    def events(self, dev, since, reads=False):
        args = ["events", "--select", dev, "--since", str(since), "--json"] + ([] if reads else ["--no-reads"])
        return [e for e in (self.dctwin(*args) or []) if e.get("path") == SITE + dev]

    def inspect(self, dev, rf):
        return self.dctwin("inspect", dev, rf)

    # ------------------------------------------------------------------ logs
    def log_lines(self, which, *patterns, limit=3):
        f = self.a.ironic_log if which == "ironic" else self.a.bmo_log
        if not f or not os.path.exists(f):
            return []
        out = []
        with open(f, errors="replace") as fh:
            for line in fh:
                if all(p in line for p in patterns):
                    out.append(line.rstrip())
        return out[-limit:]


def requests_of(evs, proto=None, prefix=None):
    return [e for e in evs if e.get("kind") == "request" and e.get("request")
            and (proto is None or e["request"].get("protocol") == proto)
            and (prefix is None or (e["request"].get("canonical") or "").startswith(prefix))]


WRITE_PREFIXES = ("power.", "boot.", "bios.", "firmware.", "account.", "vmedia.", "secureboot.")


def writes_of(evs, proto=None):
    return [e for e in requests_of(evs, proto) if (e["request"].get("canonical") or "").startswith(WRITE_PREFIXES)
            and not e["request"]["canonical"].endswith(".read")]


def canon(evs):
    return ", ".join(e["request"]["canonical"] for e in evs) or "(none)"


def short_ironic(line):
    """An Ironic or BMO log line without its timestamp, pid and request context."""
    m = re.match(r"\S+ \S+ \d+ (\w+) \S+ \[[^\]]*\] (.*)", line)
    if m:
        msg = re.sub(r" \w+ /\S+\.py:\d+$", "", m.group(2))
        return f"{m.group(1)} {msg}"[:400]
    return line[:400]


class Host:
    def __init__(self, env, spec, creds, endpoints):
        self.env = env
        self.spec = spec
        self.name = spec["name"]
        self.dev = spec["device"]
        self.proto = "ipmi" if spec["scheme"] == "ipmi" else "redfish"
        self.cred = creds[SITE + self.dev]
        self.ep = endpoints[SITE + self.dev]
        self.results = {}
        self.evidence = {}
        self.faults = []
        # the host's ComputerSystem in the twin's Redfish tree (also read for IPMI hosts, in-process)
        members = [m["@odata.id"] for m in env.inspect(self.dev, "/redfish/v1/Systems")["Members"]]
        want = spec.get("system")
        self.system = next((m for m in members if want and m.endswith("/" + want)), members[0])
        if self.proto == "redfish":
            self.address = f"{spec['scheme']}://{self.cred['siteIP']}{self.system}"
        else:
            self.address = f"ipmi://{self.ep['ipmi']}"

    # ------------------------------------------------------------------ objects
    def manifests(self, password=None):
        secret = {"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
                  "metadata": {"name": f"{self.name}-bmc", "namespace": NS},
                  "stringData": {"username": self.cred["user"], "password": password or self.cred["password"]}}
        spec = {"online": True, "bootMode": "UEFI", "automatedCleaningMode": "disabled",
                "bmc": {"address": self.address, "credentialsName": f"{self.name}-bmc"},
                "inspectionMode": self.spec["inspection"]}
        if self.proto == "redfish":
            # self-signed BMC certificates and no CA to trust at a leased address (compat/ironic)
            spec["bmc"]["disableCertificateVerification"] = True
        if self.spec["inspection"] == "disabled":
            # with inspection off BMO needs the MAC the host boots from (the webhook's rule too)
            spec["bootMACAddress"] = self.boot_mac()
        if self.spec.get("external"):
            spec["externallyProvisioned"] = True
        bmh = {"apiVersion": "metal3.io/v1alpha1", "kind": "BareMetalHost",
               "metadata": {"name": self.name, "namespace": NS}, "spec": spec}
        return secret, bmh

    def boot_mac(self):
        # the host's first NIC, as an operator reads it off the machine; here from the Redfish tree
        # the twin renders for the same host (in-process, not through the BMC's request log)
        eths = self.env.inspect(self.dev, self.system + "/EthernetInterfaces")["Members"]
        return self.env.inspect(self.dev, eths[0]["@odata.id"])["MACAddress"].lower()

    def bmh(self):
        return self.env.get("baremetalhost", self.name) or {}

    def status(self):
        return self.bmh().get("status") or {}

    def state(self):
        return (self.status().get("provisioning") or {}).get("state", "")

    def node(self):
        return self.env.node(self.name)

    def model(self):
        return self.env.device(self.dev)

    def condition(self, ctype):
        return next((c for c in self.status().get("conditions") or [] if c.get("type") == ctype), {})

    def evid(self, step, *lines):
        self.evidence.setdefault(step, []).extend(l for l in lines if l)

    def needs(self, *steps):
        for s in steps:
            if self.results.get(s, ("-",))[0] not in ("PASS", "XFAIL"):
                raise Blocked(f"needs {s}")

    def summary(self):
        st = self.status()
        return (f"state {self.state() or '-'}, operationalStatus {st.get('operationalStatus') or '-'}, "
                f"errorType {st.get('errorType') or '-'}, poweredOn {st.get('poweredOn')}")


# ---------------------------------------------------------------------------------- the checks

def driver_of(scheme):
    return {"redfish": "redfish", "ilo5-redfish": "redfish", "idrac-redfish": "idrac", "ipmi": "ipmi"}[scheme]


def s_register(h, t0seq):
    if h.spec.get("finding") == "idrac-fast":
        return register_idrac_fast(h, t0seq)
    if h.spec.get("badcreds"):
        raise Blocked("registered with a wrong password first: credentials.bad, credentials.fixed")
    st = await_(lambda: h.state() not in ("", "registering") or h.status().get("errorType"), "registration", WAIT)
    st = h.status()
    if st.get("errorType"):
        raise Fail(f"{h.summary()}: {st.get('errorMessage')}")
    n = h.node()
    if not n:
        raise Fail("no Ironic node")
    want = driver_of(h.spec["scheme"])
    if n["driver"] != want:
        raise Fail(f"Ironic driver {n['driver']}, expected {want}")
    di = n["driver_info"]
    if h.proto == "redfish":
        addr_ok = di.get("redfish_address") == f"https://{h.cred['siteIP']}" and di.get("redfish_system_id") == h.system
        addr = f"{di.get('redfish_address')} {di.get('redfish_system_id')}"
    else:
        host, port = h.ep["ipmi"].split(":")
        addr_ok = di.get("ipmi_address") == host and str(di.get("ipmi_port")) == port
        addr = f"{di.get('ipmi_address')}:{di.get('ipmi_port')}"
    if not addr_ok:
        raise Fail(f"Ironic driver_info address {addr}, expected {h.address}")
    good = ((st.get("goodCredentials") or {}).get("credentials") or {}).get("name")
    evs = h.env.events(h.dev, t0seq, reads=True)
    reqs = requests_of(evs, h.proto)
    # the twin names the user on a session's creation and on every write; reads carry no principal
    # (Ironic keeps a Redfish session per BMC for its lifetime, so a BMC it already knows may show
    # no session.create here)
    users = sorted({(e.get("source") or {}).get("principal") for e in reqs} - {None, "anonymous", "login"})
    if users and users != [h.cred["user"]]:
        raise Fail(f"{h.proto} requests by {users}, expected only {h.cred['user']!r}")
    if not reqs:
        raise Fail(f"no {h.proto} request in the twin's log")
    mine = reqs
    ifaces = {k: n.get(f"{k}_interface") for k in ("power", "management", "boot", "inspect", "bios", "firmware", "deploy")}
    # anything else Ironic posted while managing the node (the idrac verify step clears the job queue)
    other = [f"{e['request']['method']} {e['request']['target'].rsplit('/', 1)[-1]} -> {e['request'].get('status')}"
             for e in reqs if e["request"].get("method") in ("POST", "PATCH", "PUT", "DELETE")
             and e["request"].get("canonical") not in ("session.create", "session.delete")
             and not e["request"]["canonical"].startswith(WRITE_PREFIXES)]
    return (f"BMH {h.state()} (goodCredentials {good}); Ironic node {n['name']} driver {n['driver']} at {addr}, "
            f"provision {n['provision_state']}, interfaces {ifaces}; twin: {len(mine)} {h.proto} requests "
            f"(user {', '.join(users) or 'via an existing session'}), writes during registration: {canon(writes_of(evs))}"
            + (f"; other requests: {', '.join(other)}" if other else ""))


def register_idrac_fast(h, t0seq):
    want = "is not supported by hardware type IDRACHardware"
    lines = await_(lambda: h.env.log_lines("bmo", f'"name": "{h.name}"', want, limit=1), "BMO's registration error", 120)
    st = h.status()
    m = re.search(r"faultstring\W+(.*?)\\", lines[0])
    evs = h.env.events(h.dev, t0seq, reads=True)
    h.evid("register", short_ironic(lines[0])[:600])
    note = (f"BMO v0.14.2 asks Ironic for inspect_interface 'redfish' on an idrac-redfish:// host with inspectionMode fast "
            f"(bmc/redfish.go: 'FIXME ... this should be using idracRedfish'); Ironic 39 answers 400 "
            f"({(m.group(1) if m else want)[:140]}); BMH stays {st.get('provisioning', {}).get('state')!r} with no "
            f"errorType, retrying; no Ironic node is created, so nothing reaches the BMC")
    h.env.kubectl("delete", "baremetalhost", h.name, "-n", NS, "--wait=false")
    raise Finding(note)


def s_credentials_bad(h, t0seq):
    if not h.spec.get("badcreds"):
        return None
    st = await_(lambda: (lambda s: s if s.get("errorType") else None)(h.status()), "a registration error", WAIT)
    if st["errorType"] != "registration error":
        raise Fail(f"{h.summary()}: {st.get('errorMessage')}")
    evs = h.env.events(h.dev, t0seq, reads=True)
    denied = [e for e in requests_of(evs, h.proto) if (e["request"].get("status") or 0) in (401, 403)]
    h.evid("credentials.bad", f"BMH errorMessage: {st.get('errorMessage')}",
           *[f"twin #{e['seq']} {e['request'].get('method')} {e['request'].get('target')} -> {e['request'].get('status')}"
             for e in denied[:3]])
    if not denied:
        raise Fail(f"the twin's log shows no refused request; BMH {st['errorType']}: {st.get('errorMessage')}")
    n = h.node()
    return (f"wrong password -> the BMC answered {len(denied)} request(s) {denied[0]['request'].get('status')} -> "
            f"BMH {h.state()}, errorType {st['errorType']!r}: {st.get('errorMessage', '')[:200]}; "
            f"Ironic {n['provision_state'] if n else '-'}")


def s_credentials_fixed(h, t0seq):
    if not h.spec.get("badcreds"):
        return None
    h.needs("credentials.bad")
    secret, _ = h.manifests()
    h.env.apply(secret, f"{h.name}-secret.json")
    t = time.monotonic()
    await_(lambda: h.state() not in ("", "registering") and not h.status().get("errorType"), "registration after the fix", WAIT)
    n = h.node()
    return (f"Secret corrected -> BMH {h.state()} after {time.monotonic() - t:.0f}s, no error; Ironic {n['provision_state']}")


def s_inspect(h, t0seq):
    if h.spec.get("finding"):
        return None
    mode = h.spec["inspection"]
    if h.spec.get("badcreds"):
        h.needs("credentials.fixed")
    elif not h.spec.get("external"):
        h.needs("register")
    if h.spec.get("external"):
        await_(lambda: h.state() == "externally provisioned" or h.status().get("errorType"), "externally provisioned", WAIT)
        n = h.node()
        if h.status().get("errorType"):
            raise Fail(h.summary() + ": " + h.status().get("errorMessage", ""))
        return (f"externallyProvisioned: no inspection; BMO adopted the node: BMH {h.state()}, Ironic {n['provision_state']} "
                f"(deploy {n['deploy_interface']})")
    if mode == "disabled":
        st = await_(lambda: h.state() in ("available", "preparing") or h.status().get("errorType"), "available", WAIT)
        if h.status().get("errorType"):
            raise Fail(h.summary())
        return f"inspectionMode disabled: BMH {h.state()} without inspection; port {h.bmh()['spec']['bootMACAddress']}"
    if mode == "agent":
        return inspect_agent(h, t0seq)
    # fast: Ironic's redfish inspect interface, out of band
    await_(lambda: h.state() in ("preparing", "available") or h.status().get("errorType"), "inspection", WAIT)
    st = h.status()
    if st.get("errorType"):
        msg = st.get("errorMessage", "")
        if h.spec.get("system") == "System_0" and "EthernetInterfaces" in msg:
            raise XFail(f"BMH {st['errorType']}: {msg[:220]} (Ironic, as on the captured GB300: no EthernetInterfaces link)")
        raise Fail(f"{h.summary()}: {msg}")
    hd = await_(lambda: h.env.get("hardwaredata", h.name), "HardwareData", 60)
    hw = (hd.get("spec") or {}).get("hardware") or {}
    sysj = h.env.inspect(h.dev, h.system)
    problems = []
    ram = (sysj.get("MemorySummary") or {}).get("TotalSystemMemoryGiB")
    if ram is not None and hw.get("ramMebibytes") != int(ram * 1024):
        problems.append(f"ramMebibytes {hw.get('ramMebibytes')} != {int(ram * 1024)}")
    eth = sysj.get("EthernetInterfaces", {}).get("@odata.id")
    macs = set()
    if eth:
        for m in h.env.inspect(h.dev, eth)["Members"]:
            mac = h.env.inspect(h.dev, m["@odata.id"]).get("MACAddress")
            if mac:
                macs.add(mac.lower())
    nics = {(n.get("mac") or "").lower() for n in hw.get("nics") or []}
    if nics != macs:
        problems.append(f"nics {sorted(nics)} != EthernetInterfaces {sorted(macs)}")
    sv = hw.get("systemVendor") or {}
    for k, rf in (("manufacturer", "Manufacturer"), ("productName", "Model"), ("serialNumber", "SerialNumber")):
        if (sv.get(k) or "") != (sysj.get(rf) or ""):
            problems.append(f"systemVendor.{k} {sv.get(k)!r} != {rf} {sysj.get(rf)!r}")
    # HardwareDetails.firmware comes from IPA's inventory; Ironic's out-of-band inspection leaves it
    # empty (BMO reads BIOS and BMC versions through HostFirmwareComponents instead, checked below)
    fw = ((hw.get("firmware") or {}).get("bios") or {}).get("version")
    cpu = hw.get("cpu") or {}
    disks = hw.get("storage") or []
    note = (f"BMH {h.state()}; HardwareData: {hw.get('ramMebibytes')} MiB, cpu {cpu.get('arch')!r} x{cpu.get('count')} "
            f"{cpu.get('model', '')!r}, {len(nics)} NICs, {len(disks)} disks, vendor {sv.get('manufacturer')!r} "
            f"{sv.get('productName')!r} {sv.get('serialNumber')!r}, firmware.bios {fw or 'empty'}")
    if problems:
        raise Fail(note + "; " + "; ".join(problems))
    evs = h.env.events(h.dev, t0seq)
    if writes_of(evs, "redfish"):
        note += f"; twin writes: {canon(writes_of(evs, 'redfish'))}"
    return note


def inspect_agent(h, t0seq):
    # Ironic's agent inspection: boot IPA from the network and wait for it to call back
    n = await_(lambda: (lambda x: x if x and x["provision_state"] in ("inspect wait", "inspect failed") else None)(h.node()),
               "Ironic inspect wait", WAIT)
    evs = await_(lambda: (lambda e: e if requests_of(e, "ipmi", "power.on") or requests_of(e, "ipmi", "power.restart")
                          or requests_of(e, "ipmi", "power.cycle") else None)(h.env.events(h.dev, t0seq)),
                 "Ironic powering the host on to boot", 120)
    boot = requests_of(evs, "ipmi", "boot.override")
    pw = [e for e in requests_of(evs, "ipmi", "power.")]
    walked = [e for e in h.env.events(h.dev, t0seq) if e.get("kind") == "state" and (e.get("state") or {}).get("field") == "boot.walk"]
    walk = (str(walked[-1]["state"].get("to")) if walked else "(no walk)")
    reqs = [f"#{e['seq']} {e['request']['canonical']} {e['request'].get('target', '')}" for e in boot + pw]
    h.evid("inspect", *reqs[:6], f"walker: {walk}")
    t = time.monotonic()
    st = await_(lambda: (lambda s: s if s.get("errorType") else None)(h.status()), "the inspection to time out", 600)
    n = h.node()
    if not walked:
        walk = "(no walk)"
        w = [e for e in h.env.events(h.dev, t0seq) if e.get("kind") == "state" and (e.get("state") or {}).get("field") == "boot.walk"]
        if w:
            walk = str(w[-1]["state"].get("to"))
    raise Cannot(f"Ironic {n['provision_state']!r} ({(n.get('last_error') or '')[:90]}) after {time.monotonic() - t:.0f}s "
                 f"more: it set a one-time PXE boot and powered the host on over IPMI ({canon(boot + pw)}); the host is "
                 f"`model` fidelity: the twin's boot walk ended at {walk!r} (nothing serves PXE on the provisioning "
                 f"network here), and no IPA calls back -> BMH {st['errorType']!r}: {st.get('errorMessage', '')[:120]}")


def s_firmware_settings(h, t0seq):
    if h.proto != "redfish" or h.spec.get("finding"):
        return None
    h.needs("inspect") if not h.spec.get("external") else None
    hfs = await_(lambda: (lambda o: o if o and (o.get("status") or {}).get("settings") else None)(
        h.env.get("hostfirmwaresettings", h.name)), "HostFirmwareSettings with settings", HFS_WAIT)
    got = hfs["status"]["settings"]
    bios_ref = (h.env.inspect(h.dev, h.system).get("Bios") or {}).get("@odata.id")
    attrs = h.env.inspect(h.dev, bios_ref).get("Attributes", {}) if bios_ref else {}

    def same(v, s):
        if v is None:
            return s == ""
        if isinstance(v, bool):
            return s in (("True", "true", "1") if v else ("False", "false", "0"))
        return str(v) == s
    # BMO keeps no setting whose name contains "Password" (hostfirmwaresettings_controller.go); every
    # other attribute the BMC serves must be there with its value (a null as "")
    want = {k: v for k, v in attrs.items() if "Password" not in k}
    dropped = len(attrs) - len(want)
    diff = [k for k, v in want.items() if k not in got or not same(v, got[k])] + [k for k in got if k not in want]
    schema = (hfs["status"].get("schema") or {}).get("name")
    fs = h.env.get("firmwareschema", schema) if schema else None
    nschema = len((fs or {}).get("spec", {}).get("schema") or {})
    note = (f"HostFirmwareSettings: {len(got)} settings = the BMC's Bios.Attributes ({len(attrs)}, less {dropped} "
            f"password), values equal; FirmwareSchema {schema or '-'} ({nschema} entries)")
    if diff:
        raise Fail(note + f"; differ: {diff[:5]} (e.g. {diff[0]}: BMC {attrs.get(diff[0])!r}, BMO {got.get(diff[0])!r})")
    return note


def s_firmware_components(h, t0seq):
    if h.proto != "redfish" or h.spec.get("finding"):
        return None
    hfc = await_(lambda: (lambda o: o if o and (o.get("status") or {}).get("components") else None)(
        h.env.get("hostfirmwarecomponents", h.name)), "HostFirmwareComponents with components", HFS_WAIT)
    comps = {c["component"]: c.get("currentVersion") for c in hfc["status"]["components"]}
    # what the BMC serves (the system's BiosVersion, the manager's FirmwareVersion) and the model
    sysj = h.env.inspect(h.dev, h.system)
    mgr = h.env.inspect(h.dev, "/redfish/v1/Managers")["Members"][0]["@odata.id"]
    served = {"bios": sysj.get("BiosVersion"), "bmc": h.env.inspect(h.dev, mgr).get("FirmwareVersion")}
    fw = h.model()["firmware"]
    model = {k: (fw.get(k) or {}).get("version") for k in ("bios", "bmc")}
    bad = [f"{k} {comps.get(k)!r} != served {v!r}" for k, v in served.items() if v and comps.get(k) != v]
    bad += [f"{k} {comps.get(k)!r} != model {v!r}" for k, v in model.items() if v and comps.get(k) != v]
    others = len([k for k in comps if k not in ("bios", "bmc")])
    note = (f"HostFirmwareComponents bios {comps.get('bios')!r}, bmc {comps.get('bmc')!r} (+{others} NICs) = the BMC's "
            f"{served}; model {model}")
    gaps = [k for k in ("bios", "bmc") if served[k] and not model[k]]
    if gaps:
        note += f"; the model has no {'/'.join(gaps)} version: the BMC serves the capture's, unbound to the model"
    if bad:
        raise Fail(note + "; " + "; ".join(bad))
    if not comps:
        raise Fail("no components")
    return note


def set_online(h, online):
    h.env.patch("baremetalhost", h.name, {"spec": {"online": online}})


def s_power_off(h, t0seq):
    h.needs("inspect")
    if h.results["inspect"][0] != "PASS":
        # a host whose inspection failed stays in BMO's inspecting state, where BMO manages no power
        raise Blocked(f"inspection {h.results['inspect'][0]}: BMO leaves the host in {h.state()!r} and manages no power")
    since = h.env.seq()
    t = time.monotonic()
    set_online(h, False)
    await_(lambda: h.status().get("poweredOn") is False and h.model()["power"] == "Off", "poweredOn false and model Off", WAIT)
    dt = time.monotonic() - t
    evs = h.env.events(h.dev, since)
    offs = requests_of(evs, h.proto, "power.off")
    if not offs:
        raise Fail(f"no power.off request over {h.proto}: {canon(writes_of(evs))}")
    n = h.node()
    return (f"online: false -> {canon(writes_of(evs, h.proto))} over {h.proto} -> model Off, BMH poweredOn false after "
            f"{dt:.0f}s; Ironic {n['power_state']!r}")


def s_power_on(h, t0seq):
    h.needs("power.off")
    since = h.env.seq()
    t = time.monotonic()
    set_online(h, True)
    await_(lambda: h.status().get("poweredOn") is True and h.model()["power"] == "On", "poweredOn true and model On", WAIT)
    dt = time.monotonic() - t
    evs = h.env.events(h.dev, since)
    if not requests_of(evs, h.proto, "power.on"):
        raise Fail(f"no power.on request over {h.proto}: {canon(writes_of(evs))}")
    return f"online: true -> {canon(writes_of(evs, h.proto))} -> model On, poweredOn true after {dt:.0f}s"


def s_reboot(h, t0seq):
    if not h.spec.get("external"):
        return None
    h.needs("inspect")
    since = h.env.seq()
    t = time.monotonic()
    h.env.kubectl("annotate", "baremetalhost", h.name, "-n", NS, "reboot.metal3.io=")

    def done():
        evs = h.env.events(h.dev, since)
        ann = (h.bmh().get("metadata") or {}).get("annotations") or {}
        if requests_of(evs, h.proto, "power.off") and requests_of(evs, h.proto, "power.on") and \
                "reboot.metal3.io" not in ann and h.status().get("poweredOn") and h.model()["power"] == "On":
            return evs
        return None
    evs = await_(done, "power off, power on and the annotation removed", WAIT)
    walk = [f"{e['state']['from']}->{e['state']['to']}" for e in evs if e.get("kind") == "state" and e["state"].get("field") == "power"]
    return (f"reboot.metal3.io -> {canon(writes_of(evs, h.proto))} (power {' '.join(walk)}), annotation removed, "
            f"poweredOn true after {time.monotonic() - t:.0f}s")


def s_twin_off(h, t0seq):
    h.needs("power.on") if not h.spec.get("external") else h.needs("reboot")
    since = h.env.seq()
    t = time.monotonic()
    h.env.dctwin("power", "off", h.dev)

    def back():
        evs = h.env.events(h.dev, since)
        return evs if requests_of(evs, h.proto, "power.on") and h.model()["power"] == "On" else None
    evs = await_(back, "the host powered back on", 180)
    dt = time.monotonic() - t
    n = h.node()
    lines = [l for l in h.env.log_lines("ironic", n["uuid"], "Changing hardware state to 'power on'", limit=1)]
    h.evid("twin.off", *map(short_ironic, lines))
    await_(lambda: h.status().get("poweredOn") is True, "BMH poweredOn true", 30)
    who = "Ironic's power sync (force_power_state_during_sync)" if lines else "BMO"
    return (f"twin `power off` -> {who} sent {canon(writes_of(evs, h.proto))} over {h.proto}; model On again after "
            f"{dt:.0f}s; BMH poweredOn true")


def health_target(h):
    dev = h.model()
    psus = [c["name"] for c in dev["components"] if c["kind"] == "psu"]
    if psus:
        return f"{h.dev}/{psus[0]}", "generic/power/psu-failure"
    gpus = [c["name"] for c in dev["components"] if c["kind"] == "accelerator"]
    if gpus:
        return f"{h.dev}/{gpus[0]}", "nvidia/xid/xid-79"
    raise Fail("no PSU or GPU to fail")


def s_health_critical(h, t0seq):
    if h.spec.get("finding"):
        return None
    if h.proto != "redfish":
        c = h.condition("Healthy")
        if c:
            h.evid("health.critical", f"Healthy condition over IPMI: {c.get('status')} {c.get('reason')}")
        return None
    h.needs("register") if not h.spec.get("badcreds") else h.needs("credentials.fixed")
    if h.state() not in ("available", "externally provisioned"):
        raise Blocked(f"BMH {h.state()!r} ({h.status().get('errorType') or 'no error'}): BMO refreshes the Healthy "
                      f"condition of a host it manages (in the first run it stayed Unknown here)")
    target, fault = health_target(h)
    # a fault id must not be another's prefix plus "-": `dctwin fault clear X` also clears X-*
    fid = f"m3-{h.name}.health"
    h.env.dctwin("fault", "inject", target, fault, "--id", fid)
    h.faults.append(fid)
    want = (h.env.inspect(h.dev, h.system).get("Status") or {}).get("Health")
    t = time.monotonic()
    if want in (None, "OK"):
        raise Fail(f"{fault}: the BMC's Status.Health stays {want!r}")
    c = await_(lambda: (lambda c: c if c.get("status") == "False" else None)(h.condition("Healthy")),
               "Healthy condition False", BMO_POLL * 2 + 60)
    n = h.node()
    return (f"{fault} on {target.split('/')[-1]} -> BMC Status.Health {want} -> Ironic health {n.get('health')!r} -> "
            f"BMH condition Healthy={c['status']} reason {c.get('reason')!r} after {time.monotonic() - t:.0f}s")


def s_health_ok(h, t0seq):
    if h.proto != "redfish":
        return None
    h.needs("health.critical")
    fid = f"m3-{h.name}.health"
    h.env.dctwin("fault", "clear", fid)
    h.faults.remove(fid)
    t = time.monotonic()
    c = await_(lambda: (lambda c: c if c.get("status") == "True" else None)(h.condition("Healthy")),
               "Healthy condition True", BMO_POLL * 2 + 60)
    return f"fault cleared -> BMH Healthy={c['status']} reason {c.get('reason')!r} after {time.monotonic() - t:.0f}s"


def s_bmc_lost(h, t0seq):
    h.needs("twin.off")
    fid = f"m3-{h.name}.bmc"
    h.env.dctwin("fault", "inject", h.dev, "generic/bmc/unreachable", "--id", fid)
    h.faults.append(fid)
    h.lost_t0 = time.monotonic()
    n = await_(lambda: (lambda x: x if x["maintenance"] else None)(h.node()), "Ironic maintenance", 420, every=3)
    dt = time.monotonic() - h.lost_t0
    if n["fault"] != "power failure":
        raise Fail(f"maintenance with fault {n['fault']!r}: {n['maintenance_reason']}")
    lines = h.env.log_lines("ironic", n["uuid"], "Switching node to maintenance mode", limit=1)
    h.evid("bmc.lost", *map(short_ironic, lines))
    # what a Metal3 user sees meanwhile: give BMO one steady-state poll
    time.sleep(BMO_POLL + 5)
    c = h.condition("Healthy")
    h.lost_view = f"{h.summary()}, Healthy={c.get('status')}/{c.get('reason')}"
    return (f"BMC drops every connection -> Ironic: maintenance, fault 'power failure', power_state "
            f"{n['power_state']!r} after {dt:.0f}s; BMH a poll later: {h.lost_view}")


def s_bmc_back(h, t0seq):
    fid = f"m3-{h.name}.bmc"
    since = h.env.seq()
    if fid in h.faults:  # cleared even when bmc.lost failed, so that later steps have a BMC
        h.env.dctwin("fault", "clear", fid)
        h.faults.remove(fid)
    h.needs("bmc.lost")
    t = time.monotonic()
    n = await_(lambda: (lambda x: x if not x["maintenance"] and not x["fault"] else None)(h.node()),
               "Ironic's power-failure recovery", 180, every=2)
    model = h.model()["power"]
    want = {"On": "power on", "Off": "power off"}[model]
    if n["power_state"] != want:
        raise Fail(f"Ironic power_state {n['power_state']!r}, model {model}")
    await_(lambda: h.status().get("poweredOn") == (model == "On") and not h.status().get("errorType"), "BMH consistent", 120)
    evs = h.env.events(h.dev, since)
    return (f"fault cleared -> Ironic lifted maintenance after {time.monotonic() - t:.0f}s, power_state {n['power_state']!r} "
            f"= model; BMH {h.summary()}; writes {canon(writes_of(evs))}")


def s_delete(h, t0seq):
    if h.name not in ("generic-c02-ipmi", "dell-b01-ipmi", "smc-b02-creds"):
        return None
    if h.name == "smc-b02-creds":
        h.needs("credentials.fixed")
    uuid = (h.node() or {}).get("uuid")
    since = h.env.seq()
    t = time.monotonic()
    h.env.kubectl("delete", "baremetalhost", h.name, "-n", NS, "--wait=false")
    await_(lambda: h.env.get("baremetalhost", h.name) is None, "the BareMetalHost gone", WAIT, every=2)
    dt = time.monotonic() - t
    if uuid and h.env.ironic_get(f"/v1/nodes/{uuid}") is not None:
        raise Fail("the Ironic node is still there")
    evs = h.env.events(h.dev, since)
    left = []
    for kind in ("hardwaredata", "hostfirmwaresettings", "hostfirmwarecomponents"):
        if h.env.a.kcm:
            try:
                await_(lambda: h.env.get(kind, h.name) is None, f"{kind} collected", 60, every=2)
            except Fail:
                left.append(kind)
        elif h.env.get(kind, h.name) is not None:
            left.append(kind + " (no garbage collector running)")
    note = (f"deleted -> BMO {canon(writes_of(evs, h.proto)) or 'sent nothing'} over {h.proto}, removed the Ironic node, "
            f"BareMetalHost gone after {dt:.0f}s; model power {h.model()['power']}")
    if left:
        if h.env.a.kcm:
            raise Fail(note + f"; left behind: {left}")
        note += f"; left: {left}"
    return note


CHECKS = {
    "register": s_register, "credentials.bad": s_credentials_bad, "credentials.fixed": s_credentials_fixed,
    "inspect": s_inspect, "firmware.settings": s_firmware_settings, "firmware.components": s_firmware_components,
    "power.off": s_power_off, "power.on": s_power_on, "reboot": s_reboot, "twin.off": s_twin_off,
    "health.critical": s_health_critical, "health.ok": s_health_ok, "bmc.lost": s_bmc_lost, "bmc.back": s_bmc_back,
    "delete": s_delete,
}


def run_step(hosts, step, t0seq, only=None):
    def one(h):
        if only and not only(h):
            return
        if h.spec.get("finding") and step != "register":
            return
        t = time.monotonic()
        try:
            note = CHECKS[step](h, t0seq)
            if note is None:
                return
            h.results[step] = ("PASS", note, time.monotonic() - t)
        except XFail as e:
            h.results[step] = ("XFAIL", str(e), time.monotonic() - t)
        except Cannot as e:
            h.results[step] = ("CANNOT", str(e), time.monotonic() - t)
        except Finding as e:
            h.results[step] = ("FINDING", str(e), time.monotonic() - t)
        except Blocked as e:
            h.results[step] = ("BLOCK", str(e), time.monotonic() - t)
        except Exception as e:  # noqa: BLE001 -- a check that cannot complete is a FAIL with its reason
            h.results[step] = ("FAIL", f"{type(e).__name__}: {e}; {h.summary()}", time.monotonic() - t)
        r = h.results[step]
        print(f"  {r[0]:6} {step:19} {h.name:17} {r[2]:6.1f}s  {r[1][:300]}", file=sys.stderr, flush=True)
    threads = [threading.Thread(target=one, args=(h,)) for h in hosts]
    for th in threads:
        th.start()
    for th in threads:
        th.join()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--kubeconfig", required=True)
    ap.add_argument("--kubectl", default="kubectl")
    ap.add_argument("--dctwin", default="dctwin")
    ap.add_argument("--work", required=True)
    ap.add_argument("--ironic", required=True)
    ap.add_argument("--ironic-log")
    ap.add_argument("--bmo-log")
    ap.add_argument("--kcm", action="store_true", help="kube-controller-manager runs (garbage collection)")
    ap.add_argument("--hosts", help="comma-separated subset of host names")
    a = ap.parse_args()
    env = Env(a)
    creds = {c["path"]: c for c in env.dctwin("credentials", " ".join(h["device"] for h in HOSTS))}
    endpoints = env.dctwin("endpoints")["endpoints"]
    specs = [s for s in HOSTS if not a.hosts or s["name"] in a.hosts.split(",")]
    hosts = [Host(env, s, creds, endpoints) for s in specs]
    t0 = time.monotonic()
    t0seq = env.seq()
    print(f"creating {len(hosts)} BareMetalHosts", file=sys.stderr, flush=True)
    for h in hosts:
        secret, bmh = h.manifests(password="not-the-password" if h.spec.get("badcreds") else None)
        env.apply(secret, f"{h.name}-secret.json")
        env.apply(bmh, f"{h.name}-bmh.json")
    for step in ("register", "credentials.bad", "credentials.fixed", "inspect", "firmware.settings", "firmware.components"):
        run_step(hosts, step, t0seq)
    run_step(hosts, "power.off", t0seq, only=lambda h: not h.spec.get("external"))
    run_step(hosts, "power.on", t0seq, only=lambda h: not h.spec.get("external"))
    run_step(hosts, "reboot", t0seq)
    run_step(hosts, "twin.off", t0seq)
    run_step(hosts, "health.critical", t0seq)
    run_step(hosts, "health.ok", t0seq)
    run_step(hosts, "bmc.lost", t0seq, only=lambda h: h.name in ("generic-c02", "dell-b01", "generic-c02-ipmi", "dell-c01-ext"))
    run_step(hosts, "bmc.back", t0seq, only=lambda h: h.name in ("generic-c02", "dell-b01", "generic-c02-ipmi", "dell-c01-ext"))
    run_step(hosts, "delete", t0seq)
    for h in hosts:
        for fid in list(h.faults):
            env.dctwin("fault", "clear", fid)
    elapsed = time.monotonic() - t0

    # ------------------------------------------------------------------ the table
    cols = [h.name for h in hosts]
    w0 = max(len(s) for s in STEPS + ["check"])
    ws = [max(len(c), 6) for c in cols]
    line = lambda cells: "| " + " | ".join(x.ljust(w) for x, w in zip(cells, [w0] + ws)) + " |"
    print(f"Metal3 baremetal-operator against dc-twin; Ironic API {env.ironic_version}; {elapsed:.0f}s\n")
    print(line(["check"] + cols))
    print("|" + "|".join("-" * (w + 2) for w in [w0] + ws) + "|")
    for s in STEPS:
        print(line([s] + [h.results.get(s, ("-",))[0] for h in hosts]))
    kinds = ("PASS", "FAIL", "XFAIL", "CANNOT", "FINDING", "BLOCK")
    totals = {k: sum(1 for h in hosts for s in STEPS if h.results.get(s, ("",))[0] == k) for k in kinds}
    print("\n" + ", ".join(f"{v} {k}" for k, v in totals.items()) +
          "  (XFAIL: fails as on the captured hardware; CANNOT: needs software running on the host, which a `model` "
          "host has none of; FINDING: behaviour of BMO or Ironic, not of the twin; BLOCK: a prerequisite failed; -: does not apply)\n")
    for h in hosts:
        print(f"{h.name}: {h.dev} ({h.spec['what']}), {h.address}")
        for s in STEPS:
            if s in h.results:
                st, note, dt = h.results[s]
                print(f"  {st:6} {s:{w0}} {dt:6.1f}s  {note}")
                for l in h.evidence.get(s, []):
                    print(f"         {'':{w0}}          | {l}")
    with open(os.path.join(a.work, "results.json"), "w") as f:
        json.dump({h.name: {"device": h.dev, "address": h.address, "results": {k: list(v) for k, v in h.results.items()},
                            "evidence": h.evidence} for h in hosts}, f, indent=2)
    sys.exit(1 if totals["FAIL"] else 0)


if __name__ == "__main__":
    main()

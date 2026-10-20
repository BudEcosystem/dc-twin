#!/usr/bin/env python3
# Runs inside the dctwin-maas container (compat/maas/run.sh), beside a twin estate. Two stages:
#
#   drivers  MAAS's power drivers, as MAAS runs them: maas-agent turns every power action into one
#            `maas.power <action> <driver> --<param> <value> ...` process (src/maasagent/internal/
#            power/service.go, powerCommand), and this stage runs that same command, from the 3.7.3
#            packages, with the parameters MAAS stores for a machine. IPMI goes through FreeIPMI
#            (ipmipower, ipmi-chassis-config), as MAAS's ipmi driver does.
#   maas     MAAS itself (region, rack, Temporal, maas-agent; start-maas.sh): machines created
#            through the MAAS CLI with the same power parameters, then power queried, switched off
#            and on through the API, and the twin changed underneath.
#
# MAAS stores a BMC's address as an IP address (its power types extract one from `power_address`)
# and reaches Redfish on 443 and IPMI on 623, through a rack controller attached to the BMC's
# subnet. The twin serves each BMC on its own loopback port, so this script gives each BMC an
# address of its own on the container's BMC network (10.250.0.0/24, set up by run.sh), and relays
# TCP 443 and UDP 623 there to the BMC's ports (BMCNet): the management network as MAAS expects it.
#
# Every effect is checked in the twin (`dctwin get`, `dctwin events`), not only in MAAS's answer.
# Usage: flow.py --stage drivers|maas --work DIR [--hosts a,b]; DCTWIN_STATE locates the estate.
import argparse
import asyncio
import json
import os
import re
import subprocess
import sys
import threading
import time

SITE = "site-1/hall-a/"
HOSTS = [
    dict(name="generic-c02", device="c02/server-01", what="generic 2U, generated DMTF tree", driver="redfish"),
    dict(name="dell-b01", device="b01/server-01", what="Dell XE9680, captured iDRAC", driver="redfish"),
    dict(name="smc-b02", device="b02/server-01", what="Supermicro SYS-821GE-TNHR, captured", driver="redfish",
         cycle=True),
    dict(name="xcc-b05", device="b05/server-01", what="Lenovo SR670 V2, captured XCC", driver="redfish"),
    dict(name="ilo-c01", device="c01/server-02", what="HPE DL385 Gen10 Plus, captured iLO 5", driver="redfish"),
    dict(name="gb300-a01", device="a01/compute-tray-01", what="NVIDIA GB300 tray, captured", driver="redfish",
         node_id="System_0"),
    dict(name="generic-c02-ipmi", device="c02/server-02", what="generic 2U over IPMI", driver="ipmi"),
    dict(name="dell-b01-ipmi", device="b01/server-02", what="Dell XE9680 over IPMI", driver="ipmi"),
    dict(name="gb300-a01-ipmi", device="a01/compute-tray-02", what="NVIDIA GB300 tray over IPMI", driver="ipmi",
         cycle=True),
]
# (cycle=True: in the maas stage, the machine MAAS power-cycles by commissioning it; the drivers
# stage drives every BMC alike)
DRIVER_STEPS = ["status", "off", "on", "cycle", "reset", "twin.off", "bad.password", "bmc.lost"]
MAAS_STEPS = ["create", "query", "power.off", "power.on", "cycle", "poll.off", "poll.on", "bmc.lost"]
# the image MAAS imports so that it accepts machines (it validates a machine's architecture against
# its imported boot images): Ubuntu 24.04, the GA kernel only, about 470 MB from images.maas.io
IMAGE_SUBARCH = "ga-24.04"
# the BMC network run.sh makes in the container (10.250.0.1 is the rack controller's address)
BMC_NET = "10.250.0."


class BMCNet:
    """Each BMC at an address of its own with the standard ports: TCP 443 and UDP 623 on
    10.250.0.<n> relayed to the BMC's loopback ports (the twin's endpoints)."""

    def __init__(self, routes):
        self.routes = routes  # [(ip, "host:port" for Redfish or None, "host:port" for IPMI or None)]
        self.loop = asyncio.new_event_loop()
        self.ready = threading.Event()
        self.error = None
        threading.Thread(target=self._run, daemon=True).start()
        if not self.ready.wait(30) or self.error:
            raise Fail(f"BMC relays did not start: {self.error}")

    def _run(self):
        asyncio.set_event_loop(self.loop)
        try:
            self.loop.run_until_complete(self._start())
        except Exception as e:  # noqa: BLE001
            self.error = e
        self.ready.set()
        self.loop.run_forever()

    async def _start(self):
        for ip, tcp, udp in self.routes:
            if tcp:
                await asyncio.start_server(lambda r, w, t=tcp: self._pipe(r, w, t), ip, 443)
            if udp:
                await self.loop.create_datagram_endpoint(lambda t=udp: _UDPRelay(self.loop, t), local_addr=(ip, 623))

    @staticmethod
    async def _pipe(r, w, target):
        host, port = target.rsplit(":", 1)
        try:
            ur, uw = await asyncio.open_connection(host, int(port))
        except OSError:
            w.close()
            return

        async def copy(a, b):
            try:
                while True:
                    data = await a.read(65536)
                    if not data:
                        break
                    b.write(data)
                    await b.drain()
            except Exception:  # noqa: BLE001 -- either side going away ends the relay
                pass
            finally:
                b.close()
        await asyncio.gather(copy(r, uw), copy(ur, w))


class _UDPRelay(asyncio.DatagramProtocol):
    """One upstream socket per client address, so each RMCP+ client keeps its own session."""

    def __init__(self, loop, target):
        self.loop, self.target, self.up = loop, target, {}

    def connection_made(self, transport):
        self.transport = transport

    def datagram_received(self, data, addr):
        up = self.up.get(addr)
        if up is None:
            relay = self

            class Back(asyncio.DatagramProtocol):
                def datagram_received(self, d, _):
                    relay.transport.sendto(d, addr)
            host, port = self.target.rsplit(":", 1)
            self.up[addr] = [None, [data]]
            fut = asyncio.ensure_future(self.loop.create_datagram_endpoint(Back, remote_addr=(host, int(port))))
            fut.add_done_callback(lambda f: self._opened(addr, f))
        elif up[0] is None:
            up[1].append(data)
        else:
            up[0].sendto(data)

    def _opened(self, addr, fut):
        transport, _ = fut.result()
        pending = self.up[addr][1]
        self.up[addr] = [transport, []]
        for d in pending:
            transport.sendto(d)


class Fail(Exception):
    pass


class Cannot(Exception):
    pass


class Finding(Exception):
    pass


def await_(cond, what, timeout, every=1.0):
    end = time.monotonic() + timeout
    while True:
        v = cond()
        if v:
            return v
        if time.monotonic() > end:
            raise Fail(f"timed out after {timeout:.0f}s waiting for {what}")
        time.sleep(every)


def run(*args, timeout=600, check=True):
    p = subprocess.run(list(args), capture_output=True, text=True, timeout=timeout)
    if check and p.returncode != 0:
        raise Fail(f"{' '.join(args[:3])} ...: exit {p.returncode}: {(p.stderr or p.stdout).strip()[-400:]}")
    return p


def dctwin(*args):
    p = run("dctwin", *args)
    out = p.stdout.strip()
    try:
        return json.loads(out) if out else None
    except json.JSONDecodeError:
        return out


def device(dev):
    return dctwin("get", dev, "--json")[0]


def seq():
    return int(dctwin("status", "--json")["seq"])


def events(dev, since, reads=False):
    args = ["events", "--select", dev, "--since", str(since), "--json"] + ([] if reads else ["--no-reads"])
    return [e for e in (dctwin(*args) or []) if e.get("path") == SITE + dev]


def reqs(evs, proto=None, prefix=""):
    return [e for e in evs if e.get("kind") == "request" and e.get("request")
            and (proto is None or e["request"].get("protocol") == proto)
            and (e["request"].get("canonical") or "").startswith(prefix)]


WRITES = ("power.", "boot.")


def writes(evs, proto=None):
    return [e for e in reqs(evs, proto) if e["request"]["canonical"].startswith(WRITES)
            and not e["request"]["canonical"].endswith(".read")]


def canon(evs):
    return ", ".join(e["request"]["canonical"] + (f" ({e['request']['target'].split(': ', 1)[1]})"
                     if e["request"]["canonical"] == "boot.override" and ": " in e["request"].get("target", "") else "")
                     for e in evs) or "(none)"


class Host:
    def __init__(self, spec, cred, ep, ip):
        self.spec, self.name, self.dev = spec, spec["name"], spec["device"]
        self.proto = spec["driver"]
        self.cred, self.ep, self.ip = cred, ep, ip
        self.results = {}
        if self.proto == "redfish":
            # the parameters MAAS stores for a Redfish machine (RedfishPowerDriver.settings); the
            # driver prefixes https://
            self.params = {"power_address": ip, "power_user": cred["user"], "power_pass": cred["password"]}
            if spec.get("node_id"):
                self.params["node_id"] = spec["node_id"]
        else:
            # IPMIPowerDriver.settings with MAAS's defaults, which the region stores and maas-agent
            # passes: LAN_2_0, boot type auto, cipher suite 3, operator privilege, the opensesspriv
            # workaround, hard power off
            self.params = {"power_driver": "LAN_2_0", "power_boot_type": "auto", "power_address": ip,
                           "power_user": cred["user"], "power_pass": cred["password"], "cipher_suite_id": "3",
                           "privilege_level": "OPERATOR", "workaround_flags": "opensesspriv", "power_off_mode": "hard"}

    def argv(self, action, params=None):
        out = ["maas.power", action, self.proto]
        for k, v in (params or self.params).items():
            out += [f"--{k.replace('_', '-')}", str(v)]
        return out

    def power(self, action, params=None, timeout=600):
        t = time.monotonic()
        p = run(*self.argv(action, params), timeout=timeout, check=False)
        return p.returncode, p.stdout.strip().splitlines()[-1] if p.stdout.strip() else "", p.stderr.strip(), time.monotonic() - t

    def model_power(self):
        return {"On": "on", "Off": "off"}.get(device(self.dev)["power"], device(self.dev)["power"])


# ------------------------------------------------------------------------------------ drivers

def d_status(h):
    rc, out, err, dt = h.power("status")
    want = h.model_power()
    if rc != 0 or out != want:
        raise Fail(f"status -> {out!r} (exit {rc}), model {want}: {err[-200:]}")
    return f"`maas.power status {h.proto}` -> {out!r} = model, {dt:.0f}s"


def d_switch(h, action, want):
    since = seq()
    rc, out, err, dt = h.power(action)
    evs = events(h.dev, since)
    model = h.model_power()
    if rc != 0 or out != want or model != want:
        raise Fail(f"{action} -> {out!r} (exit {rc}), model {model}; twin: {canon(writes(evs))}; {err[-240:]}")
    ov = device(h.dev)["boot"]
    return f"-> {out!r}, model {model}, {dt:.0f}s; twin: {canon(writes(evs, h.proto))}; boot override now {ov['overrideTarget']}/{ov['overrideEnabled']}"


def d_off(h):
    return d_switch(h, "off", "off")


def d_on(h):
    note = d_switch(h, "on", "on")
    return note


def d_cycle(h):
    return d_switch(h, "cycle", "on")


def d_reset(h):
    if h.proto == "ipmi":
        rc, out, err, dt = h.power("reset")
        if rc == 0:
            raise Fail(f"reset over IPMI answered {out!r}")
        return None  # MAAS's ipmi driver has no reset (power_reset raises NotImplementedError)
    since = seq()
    note = d_switch(h, "reset", "on")
    evs = events(h.dev, since)
    resets = reqs(evs, "redfish", "power.restart")
    if len(resets) != 1:
        # MAAS issues GracefulRestart, waits and reads PowerState, and resets again while it does
        # not read On (perform_power); a BMC whose host stays On through a warm restart is reset once
        walk = " ".join(e["state"]["to"] for e in evs if e.get("kind") == "state" and e["state"].get("field") == "power")
        raise Fail(f"{len(resets)} resets for one `maas.power reset`: the model's power went {walk} "
                   f"for each restart, MAAS read 'off' and reset again; {note}")
    return note


def d_twin_off(h):
    dctwin("power", "off", h.dev)
    rc, out, err, dt = h.power("status")
    dctwin("power", "on", h.dev)
    await_(lambda: h.model_power() == "on", "the host back on", 60)
    if rc != 0 or out != "off":
        raise Fail(f"after the twin's power off, status -> {out!r} (exit {rc}): {err[-200:]}")
    return f"twin `power off` -> `maas.power status` -> 'off'; twin powered it back on"


def d_bad_password(h):
    p = dict(h.params, power_pass="not-the-password")
    rc, out, err, dt = h.power("status", p, timeout=900)
    if rc == 0:
        raise Fail(f"a wrong password answered {out!r}")
    msg = err.strip().splitlines()[-1] if err.strip() else out
    return f"wrong password -> exit {rc} after {dt:.0f}s: {msg[:220]}"


def d_bmc_lost(h):
    fid = f"maas-{h.name}.bmc"
    dctwin("fault", "inject", h.dev, "generic/bmc/unreachable", "--id", fid)
    try:
        rc, out, err, dt = h.power("status", timeout=1200)
    finally:
        dctwin("fault", "clear", fid)
    if rc == 0 and out in ("on", "off"):
        raise Fail(f"an unreachable BMC answered {out!r}")
    msg = (err.strip().splitlines() or [out])[-1]
    return f"BMC unreachable -> exit {rc} ({out or 'no state'}) after {dt:.0f}s: {msg[:220]}"


DRIVER_CHECKS = {"status": d_status, "off": d_off, "on": d_on, "cycle": d_cycle, "reset": d_reset,
                 "twin.off": d_twin_off, "bad.password": d_bad_password, "bmc.lost": d_bmc_lost}


# ------------------------------------------------------------------------------------ MAAS itself

class Maas:
    def __init__(self, profile="admin"):
        self.profile = profile
        self.arch = run("dpkg", "--print-architecture").stdout.strip()
        self.version = (self.cli("version", "read") or {}).get("version", "?")

    def import_image(self):
        """What a MAAS operator does first: import a boot image (here one, the smallest)."""
        sel = self.cli("boot-source-selections", "read", "1")[0]
        self.cli("boot-source-selection", "update", "1", str(sel["id"]), "os=ubuntu", "release=noble",
                 f"arches={self.arch}", f"subarches={IMAGE_SUBARCH}", "labels=*")
        self.cli("boot-resources", "import")
        t = time.monotonic()
        try:  # the import is asynchronous: see it start, then finish
            await_(lambda: self.cli("boot-resources", "is-importing") is True, "the import to start", 60, every=2)
        except Fail:
            pass
        await_(lambda: self.cli("boot-resources", "is-importing") is False and any(
            r["name"] == "ubuntu/noble" and r["architecture"] == f"{self.arch}/{IMAGE_SUBARCH}"
            for r in self.cli("boot-resources", "read")), "the image import", 1800, every=10)
        return f"imported ubuntu/noble {self.arch}/{IMAGE_SUBARCH} in {time.monotonic() - t:.0f}s"

    def cli(self, *args, timeout=600):
        p = run("maas", self.profile, *args, timeout=timeout, check=False)
        if p.returncode != 0:
            raise Fail(f"maas {' '.join(args[:3])}: {(p.stdout + p.stderr).strip()[-300:]}")
        out = p.stdout.strip()
        try:
            return json.loads(out) if out else None
        except json.JSONDecodeError:
            return out


def m_create(m, h):
    mac = boot_mac(h)
    args = ["machines", "create", f"hostname={h.name}", f"architecture={m.arch}/generic", f"mac_addresses={mac}",
            f"power_type={h.proto}"]
    # deployed=true: a machine already in service (no commissioning); the cycle hosts are created
    # New (commission=false) and commissioned by the `cycle` check
    args.append("commission=false" if h.spec.get("cycle") else "deployed=true")
    args += [f"power_parameters_{k}={v}" for k, v in h.params.items()]
    mach = m.cli(*args)
    h.system_id = mach["system_id"]
    got = m.cli("machine", "power-parameters", h.system_id)
    return f"machine {h.system_id} ({mach.get('status_name')}), power_type {mach.get('power_type')}, parameters {sorted(got)}"


def boot_mac(h):
    sysid = dctwin("inspect", h.dev, "/redfish/v1/Systems")["Members"][0]["@odata.id"]
    if h.spec.get("node_id"):
        sysid = "/redfish/v1/Systems/" + h.spec["node_id"]
    eth = dctwin("inspect", h.dev, sysid).get("EthernetInterfaces")
    if eth:
        members = dctwin("inspect", h.dev, eth["@odata.id"])["Members"]
        if members:
            return dctwin("inspect", h.dev, members[0]["@odata.id"])["MACAddress"].lower()
    return device(h.dev)["identity"]["bmcMAC"]


def mread(m, h):
    return m.cli("machine", "read", h.system_id)


def m_query(m, h):
    since = seq()
    st = m.cli("machine", "query-power-state", h.system_id, timeout=300)
    want = h.model_power()
    if st.get("state") != want:
        raise Fail(f"query-power-state {st}, model {want}")
    got = reqs(events(h.dev, since, reads=True), h.proto)
    if not got:
        raise Fail(f"MAAS answered {st['state']!r} with no {h.proto} request in the twin's log")
    return (f"query-power-state -> {st['state']!r} = model; twin: {len(got)} {h.proto} requests "
            f"({', '.join(sorted({e['request']['canonical'] for e in got}))})")


def m_power(m, h, verb, want, prefix):
    if h.spec.get("cycle"):
        return None
    since = seq()
    t = time.monotonic()
    m.cli("machine", verb, h.system_id, timeout=300)
    await_(lambda: h.model_power() == want, f"model {want}", 300, every=2)
    mach = await_(lambda: (lambda x: x if x.get("power_state") == want else None)(mread(m, h)),
                  f"MAAS power_state {want}", 300, every=5)
    evs = events(h.dev, since)
    if not reqs(evs, h.proto, prefix):
        raise Fail(f"no {prefix} request over {h.proto}: {canon(writes(evs))}")
    return (f"`machine {verb}` -> twin: {canon(writes(evs, h.proto))}; model {want}, MAAS power_state "
            f"{mach['power_state']!r} after {time.monotonic() - t:.0f}s")


def m_cycle(m, h):
    # MAAS power-cycles a machine that is on when it starts commissioning it (Node._start with
    # allow_power_cycle): the CYCLE power workflow
    if not h.spec.get("cycle"):
        return None
    if h.model_power() != "on":
        raise Fail(f"model {h.model_power()} before the cycle")
    since = seq()
    t = time.monotonic()
    m.cli("machine", "commission", h.system_id, timeout=300)

    def cycled():
        evs = events(h.dev, since)
        offs = reqs(evs, h.proto, "power.off")
        ons = [e for e in reqs(evs, h.proto, "power.on") if offs and e["seq"] > offs[0]["seq"]]
        return evs if ons and h.model_power() == "on" else None
    evs = await_(cycled, "power off then on in the twin's log", 300, every=2)
    dt = time.monotonic() - t
    status = mread(m, h).get("status_name")
    walk = [e["state"]["to"] for e in evs if e.get("kind") == "state" and e["state"].get("field") == "power"]
    # commissioning goes on to wait for the ephemeral image to boot and call back, which a `model`
    # host never does: abort it, and MAAS powers the machine off
    since2 = seq()
    m.cli("machine", "abort", h.system_id, timeout=300)
    await_(lambda: h.model_power() == "off", "the machine off after the abort", 300, every=2)
    after = mread(m, h).get("status_name")
    return (f"`machine commission` of a machine that is on -> MAAS's power cycle: twin "
            f"{canon(writes(evs, h.proto))} (power {' -> '.join(walk)}) in {dt:.0f}s, status {status}; "
            f"`machine abort` -> {canon(writes(events(h.dev, since2), h.proto))}, status {after}")


MAAS_CHECKS = {"create": m_create, "query": m_query,
               "power.off": lambda m, h: m_power(m, h, "power-off", "off", "power.off"),
               "power.on": lambda m, h: m_power(m, h, "power-on", "on", "power.on"),
               "cycle": m_cycle}

# the rack controller polls the power of every machine not queried in the last five minutes, every
# 15 s (NodePowerMonitorService; the region's list_cluster_nodes_power_parameters)
POLL_WAIT = 420


def m_poll(m, h, verb, want):
    if h.spec.get("cycle"):
        return None
    since = seq()
    dctwin("power", verb, h.dev)
    await_(lambda: h.model_power() == want, f"model {want}", 120, every=2)
    t = time.monotonic()
    mach = await_(lambda: (lambda x: x if x.get("power_state") == want else None)(mread(m, h)),
                  f"MAAS's own power polling to record {want!r}", POLL_WAIT, every=5)
    dt = time.monotonic() - t
    evs = events(h.dev, since)
    if writes(evs, h.proto):
        raise Fail(f"MAAS wrote to the BMC: {canon(writes(evs, h.proto))}")
    polled = reqs(events(h.dev, since, reads=True), h.proto)
    return (f"twin `power {verb}`, nobody asks MAAS -> MAAS power_state {mach['power_state']!r} after {dt:.0f}s "
            f"(its polling: {len(polled)} {h.proto} reads, no write)")


def m_bmc_lost(m, h):
    if h.spec.get("cycle"):
        return None
    fid = f"maas-{h.name}.lost"
    dctwin("fault", "inject", h.dev, "generic/bmc/unreachable", "--id", fid)
    t = time.monotonic()
    try:
        try:
            st = m.cli("machine", "query-power-state", h.system_id, timeout=900)
            got = f"query-power-state answered {st}"
        except Fail as e:
            got = f"query-power-state failed: {str(e)[-260:]}"
        dt = time.monotonic() - t
        mach = await_(lambda: (lambda x: x if x.get("power_state") not in ("on", "off") else None)(mread(m, h)),
                      "MAAS's power_state to leave on/off", POLL_WAIT, every=5)
    finally:
        dctwin("fault", "clear", fid)
    if "answered" in got and st.get("state") in ("on", "off"):
        raise Fail(f"an unreachable BMC: {got}")
    back = await_(lambda: (lambda x: x if x.get("power_state") == h.model_power() else None)(
        m.cli("machine", "query-power-state", h.system_id, timeout=300) and mread(m, h)), "recovery", 300, every=5)
    return (f"BMC unreachable -> {got} after {dt:.0f}s; MAAS power_state {mach['power_state']!r}; fault cleared -> "
            f"query-power-state {back['power_state']!r}")


MAAS_CHECKS.update({"poll.off": lambda m, h: m_poll(m, h, "off", "off"),
                    "poll.on": lambda m, h: m_poll(m, h, "on", "on"), "bmc.lost": m_bmc_lost})


# ------------------------------------------------------------------------------------ main

def record(h, step, fn, *args):
    t = time.monotonic()
    try:
        note = fn(*args)
        if note is None:
            return
        h.results[step] = ("PASS", note, time.monotonic() - t)
    except Cannot as e:
        h.results[step] = ("CANNOT", str(e), time.monotonic() - t)
    except Finding as e:
        h.results[step] = ("FINDING", str(e), time.monotonic() - t)
    except Exception as e:  # noqa: BLE001
        h.results[step] = ("FAIL", f"{type(e).__name__}: {e}", time.monotonic() - t)
    r = h.results[step]
    print(f"  {r[0]:6} {step:13} {h.name:16} {r[2]:6.1f}s  {r[1][:260]}", file=sys.stderr, flush=True)


def table(title, hosts, steps):
    cols = [h.name for h in hosts]
    w0 = max(len(s) for s in steps + ["check"])
    ws = [max(len(c), 6) for c in cols]
    line = lambda cells: "| " + " | ".join(x.ljust(w) for x, w in zip(cells, [w0] + ws)) + " |"
    print(f"\n{title}\n")
    print(line(["check"] + cols))
    print("|" + "|".join("-" * (w + 2) for w in [w0] + ws) + "|")
    for s in steps:
        print(line([s] + [h.results.get(s, ("-",))[0] for h in hosts]))
    kinds = ("PASS", "FAIL", "CANNOT", "FINDING")
    tot = {k: sum(1 for h in hosts for s in steps if h.results.get(s, ("",))[0] == k) for k in kinds}
    print("\n" + ", ".join(f"{v} {k}" for k, v in tot.items()) + "\n")
    for h in hosts:
        print(f"{h.name}: {h.dev} ({h.spec['what']}), {h.proto} {h.params['power_address']}")
        for s in steps:
            if s in h.results:
                st, note, dt = h.results[s]
                print(f"  {st:6} {s:{w0}} {dt:6.1f}s  {note}")
    return tot["FAIL"]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--stage", choices=("drivers", "maas"), required=True)
    ap.add_argument("--work", required=True)
    ap.add_argument("--hosts")
    a = ap.parse_args()
    creds = {c["path"]: c for c in dctwin("credentials", " ".join(s["device"] for s in HOSTS))}
    eps = dctwin("endpoints")["endpoints"]
    specs = [s for s in HOSTS if not a.hosts or s["name"] in a.hosts.split(",")]
    hosts = [Host(s, creds[SITE + s["device"]], eps[SITE + s["device"]], f"{BMC_NET}{10 + i}")
             for i, s in enumerate(HOSTS) if s in specs]
    BMCNet([(h.ip, h.ep["redfish"] if h.proto == "redfish" else None, h.ep["ipmi"] if h.proto == "ipmi" else None)
            for h in hosts])
    t0 = time.monotonic()
    if a.stage == "drivers":
        for h in hosts:
            for step in DRIVER_STEPS:
                record(h, step, DRIVER_CHECKS[step], h)
        fails = table(f"MAAS 3.7.3 power drivers (`maas.power`, as maas-agent runs them) against dc-twin; "
                      f"{time.monotonic() - t0:.0f}s", hosts, DRIVER_STEPS)
    else:
        m = Maas()
        print(m.import_image(), file=sys.stderr, flush=True)
        for step in MAAS_STEPS:
            # a machine MAAS cannot query gets no further checks
            gate = "create" if step in ("create", "query") else "query"
            todo = [h for h in hosts if step == "create" or h.results.get(gate, ("",))[0] == "PASS"]
            ths = [threading.Thread(target=record, args=(h, step, MAAS_CHECKS[step], m, h)) for h in todo]
            for th in ths:
                th.start()
            for th in ths:
                th.join()
        fails = table(f"MAAS {m.version} (region, rack, Temporal, maas-agent) managing power through dc-twin's BMCs; "
                      f"{time.monotonic() - t0:.0f}s", hosts, MAAS_STEPS)
    with open(os.path.join(a.work, f"results-{a.stage}.json"), "w") as f:
        json.dump({h.name: {"device": h.dev, "results": {k: list(v) for k, v in h.results.items()}} for h in hosts}, f, indent=2)
    sys.exit(1 if fails else 0)


if __name__ == "__main__":
    main()

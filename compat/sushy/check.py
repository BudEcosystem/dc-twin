#!/usr/bin/env python3
# Drives dc-twin's emulated BMCs with OpenStack's sushy, unmodified: the library Ironic's redfish
# hardware type (and through it Metal3) uses to manage every machine. Each operation below is a
# call sequence Ironic's redfish driver makes (ironic/drivers/modules/redfish/{power,management,
# bios,inspect,firmware}.py); after every write the effect is checked in the twin's model
# and event log (`dctwin get` / `dctwin events`), not only in what the BMC answers back.
#
# Usage: check.py CREDENTIALS.json [--system-id DEVICE=SYSTEM_ID ...] [--json-out FILE]
#   CREDENTIALS.json is `dctwin credentials <selector>`: [{path, user, password, redfish, siteIP}].
#   DCTWIN (default "dctwin") and DCTWIN_STATE locate the running estate.
# Exit status 1 when any operation FAILs.
import argparse
import importlib.metadata
import json
import os
import re
import subprocess
import sys
import time

import sushy
import urllib3

urllib3.disable_warnings()

OPS = [
    "connect", "system.get", "inspect", "power.off", "power.on", "boot.pxe.once", "bios.read",
    "bios.stage", "reboot", "bios.applied", "boot.once.consumed", "secureboot.read", "managers",
    "firmware.inventory", "firmware.versions", "chassis.power", "chassis.thermal",
]
WAIT = float(os.environ.get("CHECK_WAIT", "60"))
MAC = re.compile(r"[0-9A-Fa-f]{2}([:-][0-9A-Fa-f]{2}){5}")
# ironic.drivers.modules.redfish.inspect.PROCESSOR_INSTRUCTION_SET_MAP
INSTRUCTION_SET = {sushy.InstructionSet.ARM_A32: "arm", sushy.InstructionSet.ARM_A64: "aarch64",
                   sushy.InstructionSet.IA_64: "ia64", sushy.InstructionSet.MIPS32: "mips",
                   sushy.InstructionSet.MIPS64: "mips64", sushy.InstructionSet.X86: "i686",
                   sushy.InstructionSet.X86_64: "x86_64"}


class Skip(Exception):
    """The BMC does not expose the resource the operation needs (Ironic treats it the same)."""


class Fail(Exception):
    pass


class Blocked(Exception):
    """Not attempted: an operation it depends on failed (reported as BLOCK, not as a second FAIL)."""


class Twin:
    def __init__(self, exe):
        self.exe = exe

    def _run(self, *args):
        out = subprocess.run([self.exe, *args], capture_output=True, text=True, check=True).stdout
        return json.loads(out) if out.strip() else None

    def device(self, path):
        views = self._run("get", path, "--json") or []
        for v in views:
            if v["path"] == path:
                return v
        raise Fail(f"twin has no device {path}")

    def seq(self):
        return self._run("status", "--json")["seq"]

    def events(self, path, since):
        evs = self._run("events", "--select", path, "--no-reads", "--since", str(since), "--json") or []
        return [e for e in evs if e.get("path") == path]


def requests_of(evs, canonical):
    return [e for e in evs if e.get("kind") == "request" and (e.get("request") or {}).get("canonical") == canonical
            and e["request"].get("protocol") == "redfish"]


def states_of(evs, field, to=None):
    return [e for e in evs if e.get("kind") == "state" and (e.get("state") or {}).get("field") == field
            and (to is None or e["state"].get("to") == to)]


def logged(evs):
    """The requests the twin logged, for a failure message."""
    return [f"{e['request'].get('method')} {e['request'].get('target')} -> {e['request'].get('canonical')}"
            for e in evs if e.get("kind") == "request" and e.get("request")]


def await_(cond, what, timeout=WAIT):
    end = time.monotonic() + timeout
    while True:
        v = cond()
        if v:
            return v
        if time.monotonic() > end:
            raise Fail(f"timed out after {timeout:.0f}s waiting for {what}")
        time.sleep(0.25)


def power_of(system):
    system.refresh(force=True)
    return system.power_state


class Check:
    def __init__(self, twin, cred, system_id):
        self.twin, self.cred, self.system_id = twin, cred, system_id
        self.path = cred["path"]
        self.results = {}
        self.conn = self.system = None
        self.bios_change = None
        self.reboot_seq = None

    def record(self, op, fn):
        try:
            note = fn()
            self.results[op] = ("PASS", note or "")
        except Skip as e:
            self.results[op] = ("SKIP", str(e))
        except Blocked as e:
            self.results[op] = ("BLOCK", str(e))
        except Fail as e:
            self.results[op] = ("FAIL", str(e))
        except sushy.exceptions.MissingAttributeError as e:
            self.results[op] = ("SKIP", f"not exposed: {e}")
        except Exception as e:  # sushy errors, HTTP errors, protocol surprises
            self.results[op] = ("FAIL", f"{type(e).__name__}: {e}"[:400])

    def run(self):
        for op in OPS:
            if op != "connect" and self.results.get("connect", ("FAIL",))[0] != "PASS":
                self.results[op] = ("BLOCK", "not connected")
                continue
            if op != "connect" and op != "system.get" and self.system is None:
                skipped = self.results.get("system.get", ("FAIL",))[0] == "SKIP"
                self.results[op] = ("SKIP", "no ComputerSystem: not an Ironic node") if skipped else ("BLOCK", "no system")
                continue
            self.record(op, getattr(self, "op_" + op.replace(".", "_")))
        try:
            if self.conn:
                self.conn.close()  # deletes the Redfish session, as Ironic's connection cache does
        except Exception:
            pass
        return self.results

    # -------------------------------------------------------------------------- read paths

    def op_connect(self):
        # Ironic's default redfish_auth_type is "auto": try a session, fall back to Basic.
        self.auth = sushy.auth.SessionOrBasicAuth(username=self.cred["user"], password=self.cred["password"])
        self.conn = sushy.Sushy(f"https://{self.cred['redfish']}/redfish/v1", auth=self.auth, verify=False)
        mode = "session" if self.auth.get_session_key() else "basic"
        return f"Redfish {self.conn.redfish_version}, product {self.conn.product!r}, auth={mode}"

    def op_system_get(self):
        # ironic redfish_utils.get_system: one system needs no id, several need redfish_system_id
        members = self.conn.get_system_collection().members_identities
        sid = self.system_id or (members[0] if len(members) == 1 else None)
        if sid is None:
            raise Fail(f"{len(members)} systems and no --system-id: {list(members)}")
        self.system = s = self.conn.get_system(sid)
        dev = self.twin.device(self.path)
        if s.power_state is None or s.power_state.value != dev["power"]:
            raise Fail(f"PowerState {s.power_state} but the model has {dev['power']}")
        boot = s.boot
        ident = "UUID matches the model" if (s.uuid or "").lower() == dev["identity"]["uuid"].lower() else f"UUID {s.uuid} (model {dev['identity']['uuid']})"
        return (f"{sid}: {s.manufacturer} {s.model}, PowerState {s.power_state.value}, boot "
                f"{getattr(boot.target, 'value', boot.target)}/{getattr(boot.enabled, 'value', boot.enabled)}/"
                f"{getattr(boot.mode, 'value', boot.mode)}; {ident}")

    def op_inspect(self):
        # ironic redfish inspect_hardware: memory_mb and one system NIC with a valid MAC are
        # essential (the default validate-interfaces hook fails without one); CPU count and
        # architecture and local_gb are recorded as found, empty when the BMC does not say
        s = self.system
        mem = s.memory_summary.size_gib if s.memory_summary else None
        cpus, arch = 0, ""
        if s.processors:
            cpus = s.processors.summary.count
            procs = s.processors.get_members()
            arch = INSTRUCTION_SET.get(procs[0].instruction_set) or "" if procs else ""
        macs = []
        try:
            nics = s.ethernet_interfaces.get_members() if s.ethernet_interfaces else []
        except sushy.exceptions.MissingAttributeError:
            nics = []
        macs = [n.mac_address for n in nics if n.mac_address and MAC.fullmatch(n.mac_address)]
        sizes = []
        for get in (lambda: s.simple_storage.disks_sizes_bytes, lambda: s.storage.volumes_sizes_bytes,
                    lambda: s.storage.drives_sizes_bytes):
            try:
                sizes += [x for x in (get() or []) if x >= 4 << 30]
            except (sushy.exceptions.SushyError, AttributeError):
                pass
        local_gb = max(0, int(min(sizes) / (1 << 30) - 1)) if sizes else 0
        missing = []
        if not mem:
            missing.append("no MemorySummary.TotalSystemMemoryGiB (memory_mb is essential)")
        if not macs:
            missing.append(f"no system EthernetInterface with a valid MAC ({len(nics)} interfaces; the validate-interfaces hook needs one)")
        if missing:
            raise Fail("Ironic inspection would fail: " + "; ".join(missing))
        note = f"memory_mb {int(mem * 1024)}, cpus {cpus}, cpu_arch {arch or repr('')}, local_gb {local_gb}, {len(macs)} NIC MACs"
        gaps = ([] if cpus else ["cpus 0: no Processor TotalThreads"]) + ([] if arch else ["cpu_arch empty: no Processor InstructionSet"])
        return note + (f" [gaps: {'; '.join(gaps)}]" if gaps else "")

    # -------------------------------------------------------------------------- power

    def _power(self, reset, want_model, canonical):
        since = self.twin.seq()
        self.system.reset_system(reset)
        target = sushy.POWER_STATE_ON if want_model == "On" else sushy.POWER_STATE_OFF
        await_(lambda: power_of(self.system) == target, f"the BMC to report PowerState {target.value}")
        dev = self.twin.device(self.path)
        if dev["power"] != want_model:
            raise Fail(f"BMC reports {target.value} but the model has {dev['power']}")
        evs = await_(lambda: (lambda e: e if states_of(e, "power", want_model) else None)(self.twin.events(self.path, since)),
                     f"a power -> {want_model} state event")
        reqs = requests_of(evs, canonical)
        if not reqs:
            raise Fail(f"no '{canonical}' request event from protocol redfish (saw {[(e.get('request') or {}).get('canonical') for e in evs if e.get('kind') == 'request']})")
        st = states_of(evs, "power", want_model)[-1]["state"]
        return f"{reset.value} -> model {dev['power']}; events: {canonical} (redfish), power {st['from']} -> {st['to']}"

    def op_power_off(self):
        # ironic redfish power: POWER_OFF is RESET_FORCE_OFF, then poll until Off
        if self.system.power_state != sushy.POWER_STATE_ON:
            raise Fail(f"expected the host On before power off, it is {self.system.power_state}")
        return self._power(sushy.RESET_FORCE_OFF, "Off", "power.off.force")

    def op_power_on(self):
        return self._power(sushy.RESET_ON, "On", "power.on")

    def op_reboot(self):
        # ironic SET_POWER_STATE_MAP[REBOOT] is RESET_FORCE_RESTART
        since = self.reboot_seq = self.twin.seq()
        self.system.reset_system(sushy.RESET_FORCE_RESTART)

        def back_on():
            evs = self.twin.events(self.path, since)
            ps = states_of(evs, "power")
            left = any(e["state"]["from"] == "On" for e in ps)
            return evs if left and ps[-1]["state"]["to"] == "On" else None

        evs = await_(back_on, "the host to leave On and come back On")
        await_(lambda: power_of(self.system) == sushy.POWER_STATE_ON, "the BMC to report PowerState On")
        if not requests_of(evs, "power.restart"):
            raise Fail("no 'power.restart' request event from protocol redfish")
        dev = self.twin.device(self.path)
        if dev["power"] != "On":
            raise Fail(f"model power {dev['power']} after the restart")
        walk = " -> ".join([states_of(evs, "power")[0]["state"]["from"]] + [e["state"]["to"] for e in states_of(evs, "power")])
        return f"ForceRestart -> model On; events: power.restart (redfish), power {walk}"

    # -------------------------------------------------------------------------- boot

    def op_boot_pxe_once(self):
        # ironic _set_boot_device(task, system, PXE, persistent=False)
        since = self.twin.seq()
        self.system.set_system_boot_options(target=sushy.BOOT_SOURCE_TARGET_PXE, enabled=sushy.BOOT_SOURCE_ENABLED_ONCE)
        self.system.refresh(force=True)
        b = self.system.boot
        dev = self.twin.device(self.path)
        model_ok = dev["boot"].get("overrideTarget") == "Pxe" and dev["boot"].get("overrideEnabled") == "Once"
        if b.target != sushy.BOOT_SOURCE_TARGET_PXE or b.enabled != sushy.BOOT_SOURCE_ENABLED_ONCE or not model_ok:
            raise Fail(f"BMC reads back {getattr(b.target, 'value', b.target)}/{getattr(b.enabled, 'value', b.enabled)}, model boot "
                       f"{dev['boot'].get('overrideTarget')}/{dev['boot'].get('overrideEnabled')}; twin logged {logged(self.twin.events(self.path, since))}")
        evs = self.twin.events(self.path, since)
        if not requests_of(evs, "boot.override"):
            raise Fail("no 'boot.override' request event from protocol redfish")
        return "BMC and model both Pxe/Once; event boot.override (redfish)"

    def op_boot_once_consumed(self):
        if self.reboot_seq is None or self.results.get("boot.pxe.once", ("",))[0] != "PASS":
            raise Blocked("needs boot.pxe.once and reboot to have passed")
        dev = self.twin.device(self.path)
        evs = self.twin.events(self.path, self.reboot_seq)
        self.system.refresh(force=True)
        if dev["boot"].get("overrideEnabled") != "Disabled" or not states_of(evs, "boot.overrideEnabled", "Disabled"):
            raise Fail(f"model boot after reboot is {dev['boot']}")
        if self.system.boot.enabled != sushy.BOOT_SOURCE_ENABLED_DISABLED:
            raise Fail(f"BMC still reports BootSourceOverrideEnabled {self.system.boot.enabled}")
        return f"the one-time override was used by the reboot (model booted '{dev.get('booted')}') and reads Disabled"

    # -------------------------------------------------------------------------- BIOS

    def _bios(self):
        try:
            return self.system.bios
        except sushy.exceptions.MissingAttributeError:
            raise Skip("no Bios resource")

    def op_bios_read(self):
        # ironic redfish bios cache_bios_settings
        attrs = self._bios().attributes or {}
        if not attrs:
            raise Fail("Bios has no Attributes")
        return f"{len(attrs)} attributes, registry {self._bios().json.get('AttributeRegistry')}"

    def op_bios_stage(self):
        # ironic redfish bios apply_configuration: set_attributes (OnReset when supported), reboot
        bios = self._bios()
        attrs = bios.attributes or {}
        flips = {"Enabled": "Disabled", "Disabled": "Enabled"}
        names = sorted(k for k, v in attrs.items() if v in flips)
        names.sort(key=lambda k: 0 if any(t in k.lower() for t in ("sriov", "virtual", "vtd", "vt-d")) else 1)
        if not names:
            raise Fail("no Enabled/Disabled attribute to stage")
        name = names[0]
        new = flips[attrs[name]]
        apply_time = None
        if bios.supported_apply_times and sushy.APPLY_TIME_ON_RESET in bios.supported_apply_times:
            apply_time = sushy.APPLY_TIME_ON_RESET
        since = self.twin.seq()
        bios.set_attributes({name: new}, apply_time=apply_time)
        bios.refresh(force=True)
        pending = bios.pending_attributes or {}
        if pending.get(name) != new:
            raise Fail(f"pending attributes after the PATCH: {pending}")
        self.bios_change = (name, new)
        evs = self.twin.events(self.path, since)
        if not requests_of(evs, "bios.set") or not states_of(evs, "bios.pending"):
            raise Fail(f"the BMC shows {name}={new} pending, but the model staged nothing: no bios.set request or "
                       f"bios.pending state; twin logged {logged(evs)}")
        return f"{name}: {attrs[name]} -> {new} staged{' (OnReset)' if apply_time else ''}; event bios.set (redfish)"

    def op_bios_applied(self):
        if not self.bios_change or self.reboot_seq is None:
            if self.results.get("bios.stage", ("",))[0] == "SKIP":
                raise Skip("no Bios resource")
            raise Blocked("needs a staged BIOS change and a reboot")
        name, new = self.bios_change
        bios = self.system.bios
        bios.refresh(force=True)
        evs = self.twin.events(self.path, self.reboot_seq)
        if not states_of(evs, "bios"):
            raise Fail("the model applied no pending BIOS settings on the reboot")
        if (bios.attributes or {}).get(name) != new:
            raise Fail(f"after the reboot {name} reads {(bios.attributes or {}).get(name)}")
        # iLO and XCC settings objects mirror every attribute, so a name is still listed after a
        # reset; it is pending only if the staged value differs from the applied one
        pending = (bios.pending_attributes or {}).get(name)
        if pending is not None and pending != new:
            raise Fail(f"{name} still pending ({pending}) after the reboot")
        return f"{name} reads {new} after the reboot and nothing is pending"

    # -------------------------------------------------------------------------- the rest

    def op_secureboot_read(self):
        # ironic redfish management get_secure_boot_state
        try:
            sb = self.system.secure_boot
        except sushy.exceptions.MissingAttributeError:
            raise Skip("no SecureBoot resource")
        dev = self.twin.device(self.path)
        if bool(sb.enabled) != bool(dev["secureBoot"]):
            raise Fail(f"SecureBootEnable {sb.enabled} but the model has {dev['secureBoot']}")
        return f"enabled {sb.enabled}, current boot {getattr(sb.current_boot, 'value', sb.current_boot)}, mode {getattr(sb.mode, 'value', sb.mode)}"

    def op_managers(self):
        mgrs = self.conn.get_manager_collection().get_members()
        if not mgrs:
            raise Fail("no managers")
        managed_by = [m.identity for m in self.system.managers]
        out = [f"{m.identity} {getattr(m.manager_type, 'value', m.manager_type)} fw {m.firmware_version}" for m in mgrs]
        return f"{len(mgrs)}: " + "; ".join(out[:3]) + (" ..." if len(out) > 3 else "") + f"; system ManagedBy {managed_by}"

    def op_firmware_inventory(self):
        # the update service and its inventory, which Ironic's firmware updates target
        us = self.conn.get_update_service()
        inv = us.firmware_inventory.get_members()
        if not inv:
            raise Fail("empty FirmwareInventory")
        listed = ", ".join(f"{i.identity}={i.version}" for i in inv[:3]) + (" ..." if len(inv) > 3 else "")
        return f"{len(inv)} items ({listed})"

    def op_firmware_versions(self):
        # ironic redfish firmware cache_firmware_components: System.BiosVersion and the managing
        # Manager's FirmwareVersion, compared here with the versions the twin's model holds
        dev = self.twin.device(self.path)
        model = {k: v["version"] for k, v in (dev.get("firmware") or {}).items()}
        mgr = self.system.managers[0] if self.system.managers else None
        seen = {"bios": self.system.bios_version, "bmc": mgr.firmware_version if mgr else None}
        out, bad = [], []
        for comp, v in seen.items():
            if v is None:
                out.append(f"{comp}: not reported")
                continue
            if comp in model and model[comp] != v:
                bad.append(f"{comp}: BMC says {v!r}, model has {model[comp]!r}")
            else:
                out.append(f"{comp} {v}")
        if bad:
            raise Fail("; ".join(bad))
        return ", ".join(out) + " (agree with the model)"

    def _chassis(self):
        chs = self.system.chassis
        if not chs:
            raise Skip("system links no chassis")
        return chs

    def op_chassis_power(self):
        # ironic redfish management get_sensors_data: _get_sensors_power per linked chassis
        seen = []
        for ch in self._chassis():
            try:
                p = ch.power
            except sushy.exceptions.MissingAttributeError:
                continue
            psus = [(x.identity, getattr(x.status, "health", None) and x.status.health.value) for x in (p.power_supplies or [])]
            seen.append(f"{ch.identity}: {len(psus)} PSUs")
        if not seen:
            raise Skip("no linked chassis has Power")
        return "; ".join(seen[:3]) + (" ..." if len(seen) > 3 else "")

    def op_chassis_thermal(self):
        seen = []
        for ch in self._chassis():
            try:
                t = ch.thermal
            except sushy.exceptions.MissingAttributeError:
                continue
            temps = [x.reading_celsius for x in (t.temperatures or [])]
            seen.append(f"{ch.identity}: {len(temps)} temperatures, {len(t.fans or [])} fans")
        if not seen:
            raise Skip("no linked chassis has Thermal")
        return "; ".join(seen[:3]) + (" ..." if len(seen) > 3 else "")


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("credentials")
    ap.add_argument("--system-id", action="append", default=[], help="DEVICE=SYSTEM_ID (Ironic's redfish_system_id)")
    ap.add_argument("--json-out")
    a = ap.parse_args()
    twin = Twin(os.environ.get("DCTWIN", "dctwin"))
    creds = json.load(open(a.credentials))
    sids = dict(x.split("=", 1) for x in a.system_id)
    report = {}
    for c in creds:
        short = c["path"].split("/", 2)[-1]
        dialect = twin.device(c["path"])["dialect"]
        sid = sids.get(c["path"]) or sids.get(short)
        t0 = time.monotonic()
        res = Check(twin, c, sid).run()
        report[short] = {"dialect": dialect, "results": res, "seconds": round(time.monotonic() - t0, 1)}
        print(f"checked {short} ({dialect}) in {report[short]['seconds']}s", file=sys.stderr, flush=True)

    cols = list(report)
    w0 = max(len(o) for o in OPS + ["operation"])
    ws = [max(len(c), len(report[c]["dialect"]), 4) for c in cols]
    line = lambda cells: "| " + " | ".join(x.ljust(w) for x, w in zip(cells, [w0] + ws)) + " |"
    print(f"sushy {importlib.metadata.version('sushy')}")
    print(line(["operation"] + cols))
    print(line([""] + [report[c]["dialect"] for c in cols]))
    print("|" + "|".join("-" * (w + 2) for w in [w0] + ws) + "|")
    for op in OPS:
        print(line([op] + [report[c]["results"][op][0] for c in cols]))
    totals = {k: sum(1 for c in cols for op in OPS if report[c]["results"][op][0] == k) for k in ("PASS", "FAIL", "BLOCK", "SKIP")}
    print("\n" + ", ".join(f"{n} {k}" for k, n in totals.items()) +
          "  (SKIP: the BMC does not expose it; BLOCK: not attempted, a prerequisite failed)\n")
    for c in cols:
        print(f"{c} ({report[c]['dialect']})")
        for op in OPS:
            st, note = report[c]["results"][op]
            print(f"  {st:5} {op:{w0}}  {note}")
    if a.json_out:
        json.dump(report, open(a.json_out, "w"), indent=2)
    sys.exit(1 if totals["FAIL"] else 0)


if __name__ == "__main__":
    main()

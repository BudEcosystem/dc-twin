#!/usr/bin/env python3
# Drives Tinkerbell's BMC controller, Rufio, through the Kubernetes API only -- Machine, Job and
# Task objects, exactly as tink-controller or an operator would -- against dc-twin's emulated BMCs,
# and checks every effect in the twin's model and event log (`dctwin get`, `dctwin events`,
# `dctwin inspect`), not only in what Rufio writes back to its own status.
#
# Rufio, kube-apiserver and the twin are already running (run.sh starts them). This script:
#   1. renders a Secret and two Machines per BMC (Redfish via bmclib's gofish provider, IPMI via
#      its ipmitool provider) from the manifests beside it, applies them, and waits for Rufio to
#      report Contactable and the twin's power state;
#   2. per protocol, runs Jobs -- power off, boot device pxe (EFI), power on, reboot, and the
#      three-task netboot Job tink-controller creates -- and checks each in the twin;
#   3. changes the twin underneath Rufio -- power off from the twin, a BMC made unreachable and
#      cleared -- and checks Rufio's Machine status follows on its next poll.
#
# Usage: flow.py --kubeconfig F --kubectl BIN --dctwin BIN --work DIR [--timeout S] [--protocols redfish,ipmi]
#   DCTWIN_STATE locates the running estate. Prints a PASS/FAIL table; exit 1 when any row FAILs.
import argparse
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
NS = "dctwin"
SITE = "site-1/hall-a/"
# machine prefix, twin device, what it is, Redfish systemName (only where a BMC serves two systems)
DEVICES = [
    ("dell-b01", "b01/server-01", "Dell XE9680, captured iDRAC tree", ""),
    ("smc-b02", "b02/server-01", "Supermicro SYS-821GE-TNHR, captured tree", ""),
    ("generic-c02", "c02/server-01", "generic 2U server, generated DMTF tree", ""),
    ("gb300-a01", "a01/compute-tray-01", "NVIDIA GB300 compute tray, captured tree", "System_0"),
]
PROTOCOLS = ["redfish", "ipmi"]
STEPS = ["power-off", "pxe-efi", "power-on", "reboot", "netboot"]
RACE = "already exists"


class Run:
    def __init__(self, a):
        self.a = a
        self.rows = []
        self.evidence = os.path.join(a.work, "evidence")
        os.makedirs(self.evidence, exist_ok=True)
        os.makedirs(os.path.join(a.work, "rendered"), exist_ok=True)

    # ------------------------------------------------------------------ tools
    def kubectl(self, *args, check=True):
        p = subprocess.run([self.a.kubectl, "--kubeconfig", self.a.kubeconfig, *args], capture_output=True, text=True)
        if check and p.returncode != 0:
            raise RuntimeError(f"kubectl {' '.join(args)}: {p.stderr.strip()}")
        return p.stdout

    def kget(self, kind, name):
        out = self.kubectl("get", f"{kind}.bmc.tinkerbell.org", name, "-n", NS, "-o", "json", check=False)
        return json.loads(out) if out.strip() else {}

    def dctwin(self, *args):
        p = subprocess.run([self.a.dctwin, *args], capture_output=True, text=True)
        if p.returncode != 0:
            raise RuntimeError(f"dctwin {' '.join(args)}: {p.stderr.strip() or p.stdout.strip()}")
        out = p.stdout.strip()
        try:
            return json.loads(out) if out else None
        except json.JSONDecodeError:
            return out

    def device(self, path):
        return self.dctwin("get", path, "--json")[0]

    def seq(self):
        return int(self.dctwin("status", "--json")["seq"])

    def events(self, path, since):
        evs = self.dctwin("events", "--select", path, "--no-reads", "--since", str(since), "--json") or []
        return [e for e in evs if e.get("path") == path]

    def system_boot(self, path, system_name):
        """The Boot object of the host's ComputerSystem, rendered in-process (never logged)."""
        members = self.dctwin("inspect", path, "/redfish/v1/Systems")["Members"]
        ids = [m["@odata.id"] for m in members]
        sys_id = next((i for i in ids if system_name and i.endswith("/" + system_name)), ids[0])
        return self.dctwin("inspect", path, sys_id).get("Boot", {})

    # ------------------------------------------------------------------ helpers
    def row(self, phase, subject, protocol, check, ok, detail):
        self.rows.append({"phase": phase, "subject": subject, "protocol": protocol, "check": check,
                          "verdict": "PASS" if ok else "FAIL", "detail": detail})
        print(f"  {'PASS' if ok else 'FAIL'}  {phase:<10} {subject:<20} {protocol:<8} {check}: {detail}", flush=True)

    @staticmethod
    def wait(fn, timeout, every=0.5):
        start = time.time()
        last = None
        while True:
            ok, last = fn()
            if ok or time.time() - start > timeout:
                return ok, last, time.time() - start
            time.sleep(every)

    @staticmethod
    def render(template, **kv):
        with open(os.path.join(HERE, "manifests", template)) as f:
            s = f.read()
        for k, v in kv.items():
            s = s.replace("@" + k + "@", str(v))
        return s

    def apply(self, name, text):
        path = os.path.join(self.a.work, "rendered", name)
        with open(path, "w") as f:
            f.write(text)
        os.chmod(path, 0o600)
        return self.kubectl("apply", "-f", path).strip()

    def machine_state(self, name):
        m = self.kget("machines", name)
        st = m.get("status", {})
        cond = next((c for c in st.get("conditions", []) if c.get("type") == "Contactable"), {})
        return st.get("powerState", ""), cond.get("status", ""), cond.get("message", "")

    @staticmethod
    def requests(evs, canonical, protocol):
        """Requests of these canonical names over this protocol that the BMC accepted: an HTTP status
        below 400; over IPMI, no completion code. (The twin logs an IPMI boot override with status 200,
        an HTTP status, where other IPMI requests carry the completion code or nothing; both read as
        accepted here.)"""
        def accepted(r):
            s = r.get("status")
            return s is None or (protocol == "redfish" and s < 400) or (protocol == "ipmi" and s in (0, 200))
        return [e for e in evs if e.get("kind") == "request" and (e.get("request") or {}).get("protocol") == protocol
                and e["request"].get("canonical") in canonical and accepted(e["request"])]

    @staticmethod
    def describe(evs, protocol):
        req = [e for e in evs if e.get("kind") == "request" and (e.get("request") or {}).get("protocol") == protocol
               and not e["request"].get("canonical", "").startswith("session.") and e["request"].get("canonical") != "other"]
        return ", ".join(f"#{e['seq']} {e['request']['canonical']} ({e['request']['method']} {e['request']['target']})"
                         for e in req) or "no actions logged"

    # ------------------------------------------------------------------ phases
    def setup(self):
        creds = {c["path"]: c for c in self.dctwin("credentials", " ".join(d for _, d, _, _ in DEVICES))}
        ep = self.dctwin("endpoints")["endpoints"]
        self.kubectl("apply", "-f", os.path.join(HERE, "manifests", "namespace.yaml"))
        self.machines = []  # (machine, device path, protocol, prefix, systemName)
        for prefix, sel, what, system in DEVICES:
            path = SITE + sel
            c = creds[path]
            secret = "bmc-" + prefix
            docs = [self.render("secret.yaml", SECRET=secret, USERNAME=c["user"], PASSWORD=c["password"])]
            for proto in PROTOCOLS:
                addr = ep[path].get(proto)
                if not addr:
                    self.row("setup", prefix, proto, "endpoint", False, f"the twin serves no {proto} for {path}")
                    continue
                host, port = addr.rsplit(":", 1)
                name = f"{prefix}-{proto}"
                docs.append(self.render(f"machine-{proto}.yaml", MACHINE=name, SECRET=secret, DEVICE=path,
                                        HOST=host, PORT=port, SYSTEM_NAME=system))
                self.machines.append((name, path, proto, prefix, system))
            print(self.apply(f"{prefix}.yaml", "---\n".join(docs)), flush=True)

    def machine_status(self):
        for name, path, proto, prefix, _ in self.machines:
            twin = self.device(path)["power"].lower()

            def ready():
                p, c, m = self.machine_state(name)
                return (c == "True" and p == twin), (p, c, m)
            ok, (p, c, m), t = self.wait(ready, self.a.timeout)
            self.row("machine", prefix, proto, "status", ok,
                     f"Contactable={c or '-'} powerState={p or '-'} (twin power {twin}) after {t:.1f}s" + (f" msg={m}" if m and not ok else ""))

    def run_jobs(self, proto, step):
        machines = [m for m in self.machines if m[2] == proto]
        seq0 = self.seq()
        for name, *_ in machines:
            self.submit(name, step, f"{name}-{step}")
        for name, path, _, prefix, system in machines:
            job = f"{name}-{step}"
            completed, detail, t = self.await_job(job)
            if not completed and RACE in detail:
                # Rufio's Job controller lists a Job's Tasks from its informer cache; when the Job is
                # reconciled again before the cache holds the Task it has just created, it creates the
                # same Task again, gets AlreadyExists and marks the Job Failed, so the Job's later Tasks
                # are never created (rufio/internal/controller/job.go). The Task that was created still
                # runs. This is Rufio's own race, not an answer from the twin: the Job is submitted
                # once more under a new name, the row says so, and the twin is checked for everything
                # Rufio sent since the first submission.
                first = detail
                job = f"{name}-{step}-again"
                self.submit(name, step, job)
                completed, detail, t = self.await_job(job)
                detail = f"{detail}; resubmitted: the first Job hit Rufio's Task-creation race ({first[:110]})"
            self.row(step, prefix, proto, "Rufio Job", completed, detail)
            self.check_twin(step, prefix, path, proto, system, seq0)

    def submit(self, machine, step, job):
        print(self.apply(f"job-{job}.yaml", self.render(f"job-{step}.yaml", JOB=job, MACHINE=machine)), flush=True)

    def await_job(self, job):
        def done():
            j = self.kget("jobs", job)
            conds = {c["type"]: c for c in j.get("status", {}).get("conditions", [])}
            fin = any(conds.get(t, {}).get("status") == "True" for t in ("Completed", "Failed"))
            return fin, conds
        _, conds, t = self.wait(done, self.a.timeout)
        completed = conds.get("Completed", {}).get("status") == "True"
        tasks = json.loads(self.kubectl("get", "tasks.bmc.tinkerbell.org", "-n", NS, "-l", f"owner-name={job}", "-o", "json"))["items"]
        tstat = "; ".join(f"{x['metadata']['name']} " + ",".join(f"{c['type']}={c['status']}" for c in x.get("status", {}).get("conditions", []))
                          for x in sorted(tasks, key=lambda x: x["metadata"]["name"]))
        fail = conds.get("Failed", {}).get("message", "")
        return completed, f"{'Completed' if completed else ('Failed: ' + fail if fail else 'not finished')} in {t:.1f}s [{tstat}]", t

    def check_twin(self, step, prefix, path, proto, system, seq0):
        def power_is(want):
            def f():
                p = self.device(path)["power"]
                return p == want, p
            return f
        if step in ("power-off", "power-on", "reboot", "netboot"):
            want = "Off" if step == "power-off" else "On"
            ok, p, _ = self.wait(power_is(want), 30)
            self.row(step, prefix, proto, "twin model", ok, f"power {p}")
        evs = self.events(path, seq0)
        if step == "power-off":
            hit = self.requests(evs, ("power.off.force",), proto)
            self.row(step, prefix, proto, "twin event", bool(hit), self.describe(evs, proto))
        elif step == "power-on":
            hit = self.requests(evs, ("power.on",), proto)
            self.row(step, prefix, proto, "twin event", bool(hit), self.describe(evs, proto))
        elif step == "reboot":
            hit = self.requests(evs, ("power.cycle", "power.restart"), proto)
            fallback = self.requests(evs, ("power.off.graceful", "power.off.force"), proto) and self.requests(evs, ("power.on",), proto)
            self.row(step, prefix, proto, "twin event", bool(hit or fallback), self.describe(evs, proto))
        elif step == "pxe-efi":
            boot = self.system_boot(path, system)
            ok = boot.get("BootSourceOverrideTarget") == "Pxe" and boot.get("BootSourceOverrideMode") == "UEFI" \
                and boot.get("BootSourceOverrideEnabled") == "Once"
            self.row(step, prefix, proto, "twin model", ok,
                     "Boot " + " ".join(f"{k.replace('BootSourceOverride', '')}={boot.get(k)}" for k in
                                        ("BootSourceOverrideTarget", "BootSourceOverrideEnabled", "BootSourceOverrideMode")))
            hit = self.requests(evs, ("boot.override",), proto)
            self.row(step, prefix, proto, "twin event", bool(hit), self.describe(evs, proto))
        elif step == "netboot":
            order = [e["request"]["canonical"] for e in evs if e.get("kind") == "request"
                     and (e.get("request") or {}).get("protocol") == proto
                     and e["request"].get("canonical") in ("power.off.force", "boot.override", "boot.order", "power.on")]
            want = ["power.off.force", "boot.override", "power.on"]
            # the same canonical repeated (ipmitool retries, idempotent re-sends) collapses
            seen = [c for i, c in enumerate(order) if i == 0 or order[i - 1] != c]
            self.row(step, prefix, proto, "twin event order", seen == want, " -> ".join(seen) or "none")

    def machine_follows(self, proto, step, want):
        for name, path, p, prefix, _ in self.machines:
            if p != proto:
                continue

            def f():
                ps, c, _ = self.machine_state(name)
                return ps == want and c == "True", ps
            ok, ps, t = self.wait(f, self.a.timeout)
            self.row(step, prefix, proto, "Machine status", ok, f"powerState={ps} after {t:.1f}s (Rufio's own poll)")

    def twin_side(self):
        # 1. power the generic server off from the twin, as a technician or a PDU would
        prefix, sel = "generic-c02", "c02/server-01"
        path = SITE + sel
        names = [m for m in self.machines if m[3] == prefix]
        for act, want in (("off", "off"), ("on", "on")):
            r = self.dctwin("power", act, sel)
            twin = self.device(path)["power"]
            for name, _, proto, _, _ in names:
                def f():
                    ps, c, _ = self.machine_state(name)
                    return ps == want, ps
                ok, ps, t = self.wait(f, self.a.timeout)
                self.row(f"twin {act}", prefix, proto, "Machine status", ok,
                         f"`dctwin power {act}` ({r['canonical']}, twin {twin}) -> powerState={ps} after {t:.1f}s")
        # 2. make a BMC unreachable, then clear it
        prefix, sel = "dell-b01", "b01/server-01"
        names = [m for m in self.machines if m[3] == prefix]
        fault = self.dctwin("fault", "inject", sel, "generic/bmc/unreachable")
        fid = fault[0]["ID"] if isinstance(fault, list) and fault else "?"
        for name, _, proto, _, _ in names:
            def gone():
                ps, c, m = self.machine_state(name)
                return c == "False", (ps, c, m)
            ok, (ps, c, m), t = self.wait(gone, self.a.timeout + 60)
            # bmclib reports every provider it tried; keep the first line and the one for this protocol
            provider = "gofish" if proto == "redfish" else "ipmitool"
            lines = [x.strip(" *\t") for x in m.splitlines() if x.strip()]
            why = "; ".join(dict.fromkeys(lines[:1] + [x for x in lines if f"provider: {provider}" in x]))
            self.row("unreachable", prefix, proto, "Machine status", ok,
                     f"fault {fid} -> Contactable={c} powerState={ps} after {t:.1f}s ({why[:220]})")
        self.dctwin("fault", "clear", fid)
        for name, _, proto, _, _ in names:
            def back():
                ps, c, m = self.machine_state(name)
                return c == "True" and ps == "on", (ps, c, m)
            ok, (ps, c, m), t = self.wait(back, self.a.timeout + 60)
            self.row("cleared", prefix, proto, "Machine status", ok, f"Contactable={c} powerState={ps} after {t:.1f}s")

    def collect(self):
        cols = ("NAME:.metadata.name,HOST:.spec.connection.host,REDFISH:.spec.connection.providerOptions.redfish.port,"
                "IPMI:.spec.connection.providerOptions.ipmitool.port,POWER:.status.powerState,"
                "CONTACTABLE:.status.conditions[?(@.type==\"Contactable\")].status")
        out = {
            "machines.txt": self.kubectl("get", "machines.bmc.tinkerbell.org", "-n", NS, "-o", "custom-columns=" + cols),
            "jobs.txt": self.kubectl("get", "jobs.bmc.tinkerbell.org", "-n", NS, "-o",
                                     "custom-columns=NAME:.metadata.name,MACHINE:.spec.machineRef.name,TASKS:.spec.tasks[*],"
                                     "COMPLETED:.status.conditions[?(@.type==\"Completed\")].status,"
                                     "FAILED:.status.conditions[?(@.type==\"Failed\")].status"),
            "tasks.txt": self.kubectl("get", "tasks.bmc.tinkerbell.org", "-n", NS, "-o",
                                      "custom-columns=NAME:.metadata.name,TASK:.spec.task,START:.status.startTime,"
                                      "DONE:.status.completionTime,COMPLETED:.status.conditions[?(@.type==\"Completed\")].status"),
            "machines.yaml": self.kubectl("get", "machines.bmc.tinkerbell.org", "-n", NS, "-o", "yaml"),
            "jobs.yaml": self.kubectl("get", "jobs.bmc.tinkerbell.org", "-n", NS, "-o", "yaml"),
            "k8s-events.txt": self.kubectl("get", "events", "-n", NS, check=False),
        }
        for _, sel, _, _ in DEVICES:
            p = subprocess.run([self.a.dctwin, "events", "--select", SITE + sel, "--no-reads"], capture_output=True, text=True)
            out[f"twin-events-{sel.replace('/', '_')}.txt"] = p.stdout
        for name, text in out.items():
            with open(os.path.join(self.evidence, name), "w") as f:
                f.write(text)

    def table(self):
        print("\n%-4s  %-12s %-12s %-8s %-18s %s" % ("", "phase", "subject", "proto", "check", "detail"))
        for r in self.rows:
            print("%-4s  %-12s %-12s %-8s %-18s %s" % (r["verdict"], r["phase"], r["subject"], r["protocol"], r["check"], r["detail"]))
        n = len(self.rows)
        f = sum(1 for r in self.rows if r["verdict"] == "FAIL")
        print(f"\n{n - f} of {n} checks pass, {f} fail")
        with open(os.path.join(self.a.work, "results.json"), "w") as fh:
            json.dump(self.rows, fh, indent=2)
        return f


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--kubeconfig", required=True)
    ap.add_argument("--kubectl", default="kubectl")
    ap.add_argument("--dctwin", default=os.environ.get("DCTWIN", "dctwin"))
    ap.add_argument("--work", required=True)
    ap.add_argument("--timeout", type=float, default=120, help="seconds to wait for Rufio to act")
    ap.add_argument("--protocols", default=",".join(PROTOCOLS), help="redfish,ipmi or a subset")
    a = ap.parse_args()
    PROTOCOLS[:] = [p for p in a.protocols.split(",") if p]
    r = Run(a)
    try:
        print("== Machines and credential Secrets", flush=True)
        r.setup()
        r.machine_status()
        for proto in PROTOCOLS:
            for step in STEPS:
                print(f"== {proto}: Job {step}", flush=True)
                r.run_jobs(proto, step)
                if step == "power-off":
                    r.machine_follows(proto, step, "off")
        print("== The twin changes underneath Rufio", flush=True)
        r.twin_side()
    finally:
        r.collect()
    sys.exit(1 if r.table() else 0)


if __name__ == "__main__":
    main()

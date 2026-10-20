#!/usr/bin/env python3
"""compat/nico/ready.py — read NICo's and the twin's records of the Ready path's vm host.

  ready.py id <dc-twin path> [expected-machines.json]
                                  stdin: `nico-admin-cli -f json machine show`; prints the host's id
  ready.py state                  stdin: `... machine show <id>`; prints its state
  ready.py history <host.json>    NICo's record of the host's states (`machine show <id>`'s events)
  ready.py evidence <host.json> <host-events.txt> <pxe.log> <dhcp-server.log> <nico-api.log>
                    <expected-machines.json (nicoinputs)> <dc-twin path> <twin state dir>
                                  prints result rows (check, PASS|FAIL|KNOWN, evidence), tab-separated

Only the standard library; run with python3 -I.
"""
import json
import re
import sys


def load(text):
    try:
        return json.loads(text)
    except ValueError:
        return None


def machines(doc):
    """The machine objects in an admin CLI JSON document, however it nests them."""
    if isinstance(doc, list):
        return [m for m in doc if isinstance(m, dict)]
    if isinstance(doc, dict):
        for k in ("machines", "items", "Machines"):
            if isinstance(doc.get(k), list):
                return doc[k]
        return [doc]
    return []


def machine_id(m):
    v = m.get("id")
    if isinstance(v, dict):
        v = v.get("id")
    return v or ""


def labels(m):
    meta = m.get("metadata") or {}
    out = {}
    for l in meta.get("labels") or []:
        if isinstance(l, dict):
            out[l.get("key")] = l.get("value")
    return out


def state_of(m):
    s = m.get("state")
    if isinstance(s, dict):
        s = s.get("state") or json.dumps(s)
    return str(s or "")


def walk(o):
    if isinstance(o, dict):
        yield o
        for v in o.values():
            yield from walk(v)
    elif isinstance(o, list):
        for v in o:
            yield from walk(v)


def find_key(o, key):
    for d in walk(o):
        if key in d:
            yield d[key]


def names(m, path, macs=()):
    """Whether a machine is the one the twin path names: its dc-twin.path label (from its
    expected machine), wherever the CLI's JSON carries it, or else one of its MACs (its BMC's or
    its boot NIC's, from the expected machine), since a machine record need not carry the expected
    machine's metadata."""
    if labels(m).get("dc-twin.path") == path:
        return True
    text = json.dumps(m)
    if "dc-twin.path" in text and json.dumps(path) in text:
        return True
    return any(mac and mac.lower() in text.lower() for mac in macs)


def expected_macs(expected, path):
    """The BMC and interface MACs of the expected machine the twin path names."""
    try:
        with open(expected) as f:
            doc = json.load(f)
    except (OSError, ValueError):
        return []
    out = []
    for e in doc.get("expected_machines", []):
        if labels(e).get("dc-twin.path") == path:
            out.append(e.get("bmc_mac_address", ""))
            out += [i.get("mac_address", "") for i in e.get("interfaces") or []]
    return [m for m in out if m]


def cmd_id(path, expected=""):
    doc = load(sys.stdin.read())
    macs = expected_macs(expected, path) if expected else []
    ms = [m for m in machines(doc) if names(m, path, macs)]
    # a host has its stable id (fm100h…) once scout has discovered it, and a predicted one
    # (fm100p…) before; its DPUs (fm100d…) are not it
    for prefix in ("fm100h", "fm100p"):
        for m in ms:
            if machine_id(m).startswith(prefix):
                print(machine_id(m))
                return
    for m in ms:
        if machine_id(m) and not machine_id(m).startswith("fm100d"):
            print(machine_id(m))
            return


def cmd_state():
    doc = load(sys.stdin.read())
    ms = machines(doc)
    if ms:
        print(state_of(ms[0]))


def short_state(st):
    """A state document as NICo's admin CLI writes it, as state/substate."""
    if not isinstance(st, dict):
        return str(st)
    top = st.get("state", "")
    for k, v in st.items():
        if k == "state" or not isinstance(v, dict):
            continue
        sub = v.get("state") or v.get("validation_type")
        if isinstance(sub, str):
            return "%s/%s" % (top, sub)
        if len(v) == 1 and next(iter(v))[:1].isupper():  # an enum variant: {"MatchingSku": {...}}
            return "%s/%s" % (top, next(iter(v)))
    return top


def cmd_history(host_json):
    """NICo's own record of the host's states (the machine's events), one line each:
    time|state/substate."""
    try:
        with open(host_json) as f:
            doc = load(f.read())
    except OSError:
        return
    ms = machines(doc or {})
    for e in (ms[0].get("events") or []) if ms else []:
        st = load(e.get("event", "")) if isinstance(e.get("event"), str) else e.get("event")
        print("%s|%s" % (str(e.get("time", ""))[11:19], short_state(st)))


def row(check, status, evidence):
    print("%s\t%s\t%s" % (check, status, evidence.replace("\t", " ").replace("\n", " ")))


def cmd_evidence(host_json, events, pxe_log, dhcp_log, api_log, expected, path, state_dir):
    def read(p):
        try:
            with open(p, errors="replace") as f:
                return f.read()
        except OSError:
            return ""

    ev, pxe, dhcp, api = read(events), read(pxe_log), read(dhcp_log), read(api_log)
    host = machines(load(read(host_json)) or {})
    host = host[0] if host else {}
    exp = load(read(expected)) or {}
    mac, serial = "", ""
    for e in exp.get("expected_machines", []):
        if labels(e).get("dc-twin.path") == path:
            serial = e.get("chassis_serial_number", "")
            for i in e.get("interfaces") or []:
                if i.get("primary"):
                    mac = i.get("mac_address", "").lower()
    # 1. DHCP: the guest's firmware leased through the twin relay from NICo's server (the twin's
    # switch port observed the ACK: an observe event, field lease, on the port's path)
    leases = []
    for line in read(state_dir + "/events.jsonl").splitlines():
        e = load(line) or {}
        st = e.get("state") or {}
        if st.get("field") == "lease" and (e.get("source") or {}).get("principal") == "vm-network" and e.get("path", "").startswith(path + ":"):
            leases.append(e)
    nico = [e for e in leases if (e.get("data") or {}).get("server") == "10.90.0.16"]
    if nico:
        ip = nico[0]["data"]["ip"]
        # the boot file NICo named (to the firmware's HTTP boot client), from whichever ACK had one
        boot = next((e["data"]["bootFile"] for e in nico if e["data"].get("bootFile")), "")
        relayed = [l for l in dhcp.splitlines() if mac and mac in l.lower()]
        circuit = ""
        for l in relayed:
            m = re.search(r'circuit_id="Some\(\\?"([^"\\]+)', l)
            if m:
                circuit = m.group(1)
                break
        # who asked, by DHCP vendor class: the firmware's PXE client and iPXE (PXEClient), its HTTP
        # boot client (HTTPClient), scout's OS (none)
        classes = {}
        for l in relayed:
            m = re.search(r'vendor_id=(\S*)', l)
            if m and "Decoded DHCP packet" not in l:
                classes[m.group(1) or "none"] = classes.get(m.group(1) or "none", 0) + 1
        asked = ", ".join("%s %d" % kv for kv in sorted(classes.items()))
        row("ready-dhcp", "PASS", "the guest (%s) leased %s from NICo's dhcp-server (server id 10.90.0.16) through the twin relay%s, boot file %s; its answers by the client's DHCP vendor class: %s; %d lease events"
            % (mac, ip, " (circuit id %s)" % circuit if circuit else "", boot or "none", asked or "?", len(leases)))
    else:
        row("ready-dhcp", "FAIL", "no lease from NICo on the host's boot NIC %s in the twin's events (%d leases observed)" % (mac, len(leases)))
    # 2. HTTP boot: NICo's iPXE, its script and scout, fetched from NICo's PXE service
    def fetched(pattern):
        return [l for l in pxe.splitlines() if re.search(pattern, l)]
    ipxe = fetched(r"internal/(x86_64|aarch64)/ipxe\.efi")
    script = fetched(r"/api/v0/pxe/boot")
    scout = fetched(r"internal/(x86_64|aarch64)/scout\.efi")
    ca = fetched(r"/api/v0/tls/root_ca")
    peers = sorted(set(re.findall(r"10\.40\.\d+\.\d+", "\n".join(ipxe + script + scout + ca))))
    if ipxe and script and scout:
        row("ready-boot", "PASS", "carbide-pxe served ipxe.efi (%d), the iPXE boot script (%d) and scout.efi (%d), and scout fetched the root CA (%d), to %s (the guest's own lease as the peer)"
            % (len(ipxe), len(script), len(scout), len(ca), ", ".join(peers) or "?"))
    else:
        row("ready-boot", "FAIL", "carbide-pxe log: ipxe.efi %d, boot script %d, scout.efi %d requests" % (len(ipxe), len(script), len(scout)))
    # 3. scout's discovery: the hardware NICo recorded, against the twin's model
    hw = {}
    for v in find_key(host, "dmi_data"):
        if isinstance(v, dict):
            hw = v
            break
    macs = set()
    for v in find_key(host, "mac_address"):
        if isinstance(v, str):
            macs.add(v.lower())
    if hw:
        ser = {k: hw.get(k, "") for k in ("product_serial", "board_serial", "chassis_serial")}
        same = all(v == serial for v in ser.values())
        ek = any(isinstance(v, str) and v for v in find_key(host, "tpm_ek_certificate"))
        ca = re.search(r'CA verification status for EK certificate.*found_signing_ca=(\w+) issuer="([^"]*)"', api)
        tpm = "TPM EK certificate %s" % ("reported" if ek else "NOT reported")
        if ca:
            tpm += ", its CA found: %s (%s)" % (ca.group(1), ca.group(2))
        row("ready-discovery", "PASS" if same and mac in macs and ek else "FAIL",
            "scout's DiscoverMachine: SMBIOS product/board/chassis serial %s (the BMC's Redfish serial, the twin's: %s), %s %s; boot NIC %s %s; %s"
            % ("/".join(ser.values()), serial, hw.get("sys_vendor", ""), hw.get("product_name", ""), mac,
               "among the NICs scout reported" if mac in macs else "NOT among %s" % sorted(macs), tpm))
    elif re.search(r"(?i)discover_?machine", api):
        row("ready-discovery", "FAIL", "nico-api logged DiscoverMachine but the host's record has no DMI data (out/host.json)")
    else:
        row("ready-discovery", "FAIL", "no DiscoverMachine from scout in nico-api's log")
    # 4. machine validation, as NICo's configuration has it (disabled: NICo's default)
    if "Skipping Machine Validation" in api:
        row("ready-validation", "KNOWN", "NICo entered machine validation (Validation/MachineValidation) and skipped it: [machine_validation_config] enabled = false, NICo's default")
    elif re.search(r"(?i)machine.?validation.*(fail|error)", api):
        row("ready-validation", "FAIL", "machine validation failed (nico-api's log)")


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(2)
    c = sys.argv[1]
    if c == "id":
        cmd_id(*sys.argv[2:4])
    elif c == "state":
        cmd_state()
    elif c == "history":
        cmd_history(sys.argv[2])
    elif c == "evidence":
        cmd_evidence(*sys.argv[2:10])
    else:
        print(__doc__)
        sys.exit(2)


if __name__ == "__main__":
    main()

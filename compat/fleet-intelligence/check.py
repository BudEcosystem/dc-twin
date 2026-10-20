"""Readers and comparisons for compat/fleet-intelligence/run.sh. Run with python3 -I.

  check.py gpus <hosts.json> <machine-info.txt>    fleetint's GPUs (`fleetint machine-info`) against the model's
  check.py smi <hosts.json> <smi.csv>              Mokka's nvidia-smi against the model's
  check.py state <states.json> <component>         health, reason, suggested actions, incidents, time
  check.py xidevent <events.json> <xid>            the newest error_xid event for that Xid: type,
                                                   device, message, raw kmsg
  (both separated by US, \x1f)
  check.py components <states.json>                every component's health and reason, one per line

`gpus` and `smi` print check lines, "id|check|expected|observed|result", which run.sh records.
hosts.json is what `dctwin-mokka render` wrote: each host's GPUs as the twin derives them.
"""
import json
import sys

# the field separator of state and xidevent: not whitespace, so that bash's read keeps empty fields
US = "\x1f"


def out(i, check, expected, observed, result):
    print("|".join(str(x).replace("|", "/").replace("\n", " ") for x in (i, check, expected, observed, result)))


def load(path):
    with open(path) as f:
        return json.load(f)


def walk(o):
    yield o
    if isinstance(o, dict):
        for v in o.values():
            yield from walk(v)
    elif isinstance(o, list):
        for v in o:
            yield from walk(v)


def bus(s):
    # NVML's busId has an eight-digit domain (00000000:38:00.0), go-nvlib's four (0000:38:00.0)
    s = (s or "").lower()
    return s[-12:] if len(s) >= 12 else s


def model_gpus(hj):
    h = load(hj)[0]
    return h, sorted(h["GPUs"], key=lambda g: g["Index"])


def cmd_gpus(hj, txt):
    # `fleetint machine-info` prints tables: "| GPU Product | NVIDIA-H100-80GB-HBM3 |", and one row per
    # GPU, "| GPU UUID | GPU INDEX | GPU BUS ID | SN | MINOR ID | BOARD ID | VBIOS VERSION | CHASSIS SN |"
    h, gs = model_gpus(hj)
    seen, product, memory = {}, None, None
    for line in open(txt, errors="replace"):
        f = [x.strip() for x in line.strip().strip("|").split("|")]
        if len(f) >= 7 and f[0].startswith("GPU-"):
            seen[f[0]] = {"index": f[1], "bus": f[2], "sn": f[3], "vbios": f[6]}
        elif len(f) == 2 and f[0] == "GPU Product":
            product = f[1]
        elif len(f) == 2 and f[0] == "GPU Memory":
            memory = f[1]
    bad = []
    for g in gs:
        r = seen.get(g["UUID"])
        if r is None:
            bad.append("%s absent" % g["UUID"])
            continue
        for k, want, got in (("serial", g["Serial"], r["sn"]), ("bus id", bus(g["BusID"]), bus(r["bus"])),
                             ("index", str(g["Index"]), r["index"])):
            if want != got:
                bad.append("%s %s %s≠%s" % (g["Path"].rsplit("/", 1)[-1], k, got, want))
    extra = set(seen) - {g["UUID"] for g in gs}
    if extra:
        bad.append("not in the model: " + ", ".join(sorted(extra)))
    g2 = next((g for g in gs if g["Index"] == 2), gs[0])
    out("B3", "fleetint machine-info lists the twin's GPUs (UUID, index, PCI bus id, serial)",
        "%d GPUs of %s, e.g. %s = %s at %s, serial %s" % (len(gs), h["Path"], g2["Path"].rsplit("/", 1)[-1], g2["UUID"], bus(g2["BusID"]), g2["Serial"]),
        "%d GPUs; %s" % (len(seen), "every field equal" if not bad else "; ".join(bad)),
        "PASS" if not bad and len(seen) == len(gs) else "FAIL")
    want, mem = gs[0]["Name"].replace(" ", "-"), str(gs[0]["MemoryMiB"] << 20)
    out("B4", "fleetint's GPU product and memory", "%s (the model's NVML name, D43, as fleetint writes it), %s bytes (%d MiB)" % (want, mem, gs[0]["MemoryMiB"]),
        "%s, %s bytes" % (product, memory), "PASS" if product == want and memory == mem else "FAIL")


def cmd_smi(hj, csv):
    h, gs = model_gpus(hj)
    rows = {}
    for line in open(csv):
        f = [x.strip() for x in line.split(",")]
        if len(f) >= 6 and f[0].isdigit():
            rows[int(f[0])] = f
    bad = []
    for g in gs:
        r = rows.get(g["Index"])
        if r is None:
            bad.append("GPU %d absent" % g["Index"])
            continue
        for k, want, got in (("name", g["Name"], r[1]), ("uuid", g["UUID"], r[2]), ("serial", g["Serial"], r[3]),
                             ("bus id", bus(g["BusID"]), bus(r[4])), ("memory", str(g["MemoryMiB"]), r[5])):
            if want != got:
                bad.append("GPU %d %s %s≠%s" % (g["Index"], k, got, want))
    out("B1", "Mokka's NVML (real nvidia-smi) in the fleetint guest reports the twin's GPUs",
        "%d × %s; name, UUID, serial, bus id, memory of each equal to the model" % (len(gs), gs[0]["Name"]),
        "%d rows; %s" % (len(rows), "every field equal" if not bad else "; ".join(bad)),
        "PASS" if not bad and len(rows) == len(gs) else "FAIL")


def states_of(sj, comp):
    # GET /v1/states answers {"<component>": [<state>, …], …}
    try:
        d = load(sj)
    except ValueError:
        return []
    if isinstance(d, dict) and isinstance(d.get(comp), list):
        return d[comp]
    for o in walk(d):
        if isinstance(o, dict) and o.get("component") == comp and isinstance(o.get("states"), list):
            return o["states"]
    return []


def cmd_state(sj, comp):
    st = states_of(sj, comp)
    if not st:
        print(US.join(["absent", "", "", "", ""]))
        return
    s = st[0]
    acts = (s.get("suggested_actions") or {}).get("repair_actions") or []
    inc = "; ".join("%s: %s" % (i.get("entity_id", ""), i.get("message", "")) for i in s.get("incidents") or [])
    print(US.join([s.get("health", ""), s.get("reason", ""), ",".join(acts), inc, str(s.get("time") or "")]))


def cmd_components(sj):
    d = load(sj)
    for comp in sorted(d) if isinstance(d, dict) else []:
        for s in d[comp] if isinstance(d[comp], list) else []:
            print("%s\t%s\t%s" % (comp, s.get("health", ""), (s.get("reason") or "").replace("\n", " ")[:160]))


def cmd_xidevent(ej, xid):
    best = None
    for o in walk(load(ej)):
        if not (isinstance(o, dict) and o.get("name") == "error_xid"):
            continue
        ex = o.get("extra_info") or {}
        try:
            d = json.loads(ex.get("data", "{}"))
        except ValueError:
            d = {}
        if str(d.get("xid")) != str(xid):
            continue
        if best is None or o.get("time", "") >= best[0].get("time", ""):
            best = (o, d)
    if best is None:
        return
    o, d = best
    print(US.join([o.get("type", ""), d.get("device_uuid", ""), o.get("message") or "", d.get("raw_kmsg", "")]))


if __name__ == "__main__":
    cmd, args = sys.argv[1], sys.argv[2:]
    {"gpus": cmd_gpus, "smi": cmd_smi, "state": cmd_state, "xidevent": cmd_xidevent, "components": cmd_components}[cmd](*args)

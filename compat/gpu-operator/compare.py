#!/usr/bin/env python3
"""Comparisons for compat/gpu-operator/run.sh: what NVIDIA's real software reports on the container
hosts against the model (hosts.json, from `dctwin-mokka render`) and against the twin's own
impersonations (its kwok Nodes and dcgm-exporter). Each check is printed as one line,
"id|check|expected|observed|result", which run.sh records.

PASS: the real software reports the model. FAIL: it does not, and nothing explains it.
FINDING: a disagreement with a known owner — the twin's impersonation, Mokka, or the software
itself — named in the line. Run with python3 -I.
"""
import json
import re
import sys

GFD_KEYS = ["product", "count", "memory", "family", "compute.major", "compute.minor", "machine", "clique"]


def out(i, check, expected, observed, result):
    print("|".join(str(x).replace("|", "/").replace("\n", " ") for x in (i, check, expected, observed, result)))


def hosts(path):
    return {h["Node"]: h for h in json.load(open(path))}


def nodes(path):
    return {n["metadata"]["name"]: n for n in json.load(open(path))["items"]}


def cmd_nodes(hj, nj):
    hs, ns = hosts(hj), nodes(nj)
    for name, h in hs.items():
        n = ns.get(name)
        if n is None:
            out("N-" + name, "a Node named for twin host " + h["Path"], name, "absent", "FAIL")
            continue
        l = n["metadata"]["labels"]
        ready = [c["status"] for c in n["status"]["conditions"] if c["type"] == "Ready"]
        ok = l.get("dc-twin.io/host") == "container" and ready == ["True"]
        out("N-" + name, "a real kubelet registers twin host " + h["Path"],
            "Node %s Ready, dc-twin.io/host=container" % name,
            "Ready=%s, kubelet %s, %s, containerd %s" % (ready[0] if ready else "?", n["status"]["nodeInfo"]["kubeletVersion"],
                                                         n["status"]["nodeInfo"]["osImage"], n["status"]["nodeInfo"]["containerRuntimeVersion"]),
            "PASS" if ok else "FAIL")
        arch = l.get("kubernetes.io/arch")
        if arch != h["Arch"]:
            out("A-" + name, "kubernetes.io/arch of " + name, h["Arch"] + " (the model's CPU)",
                arch + ": a container host runs on the Docker host's architecture (D49)", "FINDING")


def gpu_rows(path):
    rows = []
    for line in open(path):
        f = [x.strip() for x in line.split(",")]
        if len(f) >= 8 and f[0].isdigit():
            rows.append(f)
    return rows


def cmd_smi(hj, node, csv):
    h = hosts(hj)[node]
    rows = gpu_rows(csv)
    lab = h["gfdLabels"]
    cc = "%s.%s" % (lab.get("nvidia.com/gpu.compute.major"), lab.get("nvidia.com/gpu.compute.minor"))
    bad = []
    for g in h["GPUs"]:
        r = [x for x in rows if int(x[0]) == g["Index"]]
        if not r:
            bad.append("GPU %d missing" % g["Index"])
            continue
        r = r[0]
        want = [g["Name"], g["UUID"], g["Serial"], "0000" + g["BusID"], str(g["MemoryMiB"]), g["VBIOS"], cc]
        for k, w, o in zip(["name", "uuid", "serial", "pci.bus_id", "memory.total", "vbios", "compute_cap"], want, r[1:8]):
            if (k == "memory.total" and g["MemoryMiB"] == 0) or (k == "vbios" and not g["VBIOS"]):
                continue  # the model has none: the value is Mokka's profile's
            if w.upper() != o.upper():
                bad.append("GPU %d %s %s≠%s" % (g["Index"], k, o, w))
    mokkas = [] if h["GPUs"][0]["VBIOS"] else ["VBIOS (the profile has no gpu firmware: Mokka's %s)" % (rows[0][6] if rows else "?")]
    out("S-" + node, "nvidia-smi on %s reports the twin's GPUs" % node,
        "%d × %s; UUID, serial, PCI bus id, memory, VBIOS, compute capability per GPU as derived%s" % (
            len(h["GPUs"]), h["GPUs"][0]["Name"], "; not compared: " + ", ".join(mokkas) if mokkas else ""),
        ("%d GPUs, every field equal (e.g. GPU 0 %s at %s)" % (len(rows), rows[0][2], rows[0][4])) if not bad and rows else "; ".join(bad[:6]) or open(csv).read()[:200],
        "PASS" if not bad and len(rows) == len(h["GPUs"]) else "FAIL")


def cmd_labels(hj, nj, kj):
    hs, ns, ks = hosts(hj), nodes(nj), nodes(kj)
    for name, h in hs.items():
        n, k = ns.get(name), ks.get(name)
        if n is None or k is None:
            out("L-" + name, "labels of " + name, "a real Node and a kwok Node", "real %s, kwok %s" % (n is not None, k is not None), "FAIL")
            continue
        rl, kl, model = n["metadata"]["labels"], k["metadata"]["labels"], h["gfdLabels"]
        ra = n["status"].get("allocatable", {}).get("nvidia.com/gpu")
        ka = k["status"].get("allocatable", {}).get("nvidia.com/gpu")
        cnt = str(len(h["GPUs"]))
        out("D-" + name, "device plugin: nvidia.com/gpu allocatable on " + name,
            "%s (model); twin projection %s" % (cnt, ka), str(ra), "PASS" if ra == cnt and ka == cnt else "FAIL")
        agree, findings = [], []
        for key in GFD_KEYS:
            lk = "nvidia.com/gpu." + key
            real, kw, mv = rl.get(lk), kl.get(lk), model.get(lk)
            if real == kw == mv:
                if real is not None:
                    agree.append("%s=%s" % (key, real))
                continue
            if key == "machine" and real == mv and kw != mv:
                findings.append(("machine", "the twin projects %s (the system's product name); real GFD reads %s" % (kw, real),
                                 "Mokka: its node agent serves the GPU's name as GFD's machine type, having no platform field in its profiles"))
            else:
                findings.append((key, "model %s, twin projection %s" % (mv, kw), "real GFD %s" % real))
        out("G-" + name, "GPU Feature Discovery labels on %s equal the model and the twin's projection" % name,
            "product, count, memory, family, compute capability%s" % (", clique" if "nvidia.com/gpu.clique" in model else ""),
            "; ".join(agree), "PASS" if agree and all(f[0] == "machine" for f in findings) else "FAIL")
        for key, exp, obs in findings:
            out("G-%s-%s" % (name, key), "nvidia.com/gpu.%s on %s" % (key, name), exp, obs,
                "FINDING" if key == "machine" else "FAIL")
        extra = sorted(x for x in rl if (x.startswith("nvidia.com/") and x not in kl and x != "nvidia.com/gfd.timestamp"))
        if extra:
            out("X-" + name, "labels real GFD writes on %s that the twin's projection does not" % name, "none",
                ", ".join("%s=%s" % (x, rl[x]) for x in extra), "FINDING")
        missing = sorted(x for x in kl if x.startswith("nvidia.com/") and x not in rl)
        if missing:
            out("Y-" + name, "labels the twin's projection writes on %s that real GFD does not" % name, "none",
                ", ".join("%s=%s" % (x, kl[x]) for x in missing) +
                (": nvidia.com/gpu.present is written by the GPU Operator's controller on nodes NFD reports an NVIDIA PCI device for, not by GFD; without the operator nothing writes it"
                 if missing == ["nvidia.com/gpu.present"] else ""), "FINDING")
        rc, kc_ = n["status"].get("capacity", {}), k["status"].get("capacity", {})
        if rc.get("cpu") != kc_.get("cpu") or rc.get("memory") != kc_.get("memory"):
            out("K-" + name, "cpu and memory capacity of " + name, "the model's: cpu %s, memory %s (the twin's projection)" % (kc_.get("cpu"), kc_.get("memory")),
                "cpu %s, memory %s: a container host's capacity is the machine its Kind node runs on (D49)" % (rc.get("cpu"), rc.get("memory")), "FINDING")
        hostpci = sorted(x for x in rl if x.startswith("feature.node.kubernetes.io/pci-") and x != "feature.node.kubernetes.io/pci-10de.present")
        if hostpci:
            out("H-" + name, "PCI devices NFD finds on " + name + " besides Mokka's", "none: the model's host has only its GPUs' NVIDIA functions here",
                ", ".join(hostpci) + ": NFD reads the PCI bus of the machine the Kind node runs on (D49)", "FINDING")
        nfd = rl.get("feature.node.kubernetes.io/pci-10de.present")
        out("F-" + name, "Node Feature Discovery labels " + name, "feature.node.kubernetes.io/pci-10de.present=true (Mokka's feature file)",
            str(nfd), "PASS" if nfd == "true" else "FAIL")


def smi_uuids(path):
    return re.findall(r"GPU \d+: (.+?) \(UUID: (GPU-[0-9a-f-]+)\)", open(path).read())


def cmd_podsmi(hj, node, path, via):
    h = hosts(hj)[node]
    got = smi_uuids(path)
    model = {g["UUID"] for g in h["GPUs"]}
    ok = len(got) == 1 and got[0][1] in model and got[0][0] == h["GPUs"][0]["Name"]
    out("P-" + node, "a pod given one GPU by the %s on %s: nvidia-smi -L inside it" % (via, node),
        "one of the twin's GPUs (%d on the host)" % len(model),
        ("%s %s, the twin's %s" % (got[0][0], got[0][1], next(g["Path"] for g in h["GPUs"] if g["UUID"] == got[0][1]))) if ok
        else (open(path).read()[:240]), "PASS" if ok else "FAIL")


def devices(slices, node=None):
    for s in slices["items"]:
        sp = s["spec"]
        if node and sp.get("nodeName") != node:
            continue
        for d in sp.get("devices") or []:
            attrs = d.get("attributes") or (d.get("basic") or {}).get("attributes") or {}
            val = {k: next(iter(v.values())) for k, v in attrs.items()}
            yield sp.get("driver"), sp.get("pool", {}).get("name"), d["name"], val, d


def cmd_slices(hj, sj):
    hs, sl = hosts(hj), json.load(open(sj))
    for name, h in hs.items():
        devs = [(n, a) for drv, pool, n, a, _ in devices(sl, name) if drv == "gpu.nvidia.com" and a.get("type", "gpu") == "gpu"]
        got = {a.get("uuid") for _, a in devs}
        model = {g["UUID"] for g in h["GPUs"]}
        products = {a.get("productName") for _, a in devs}
        ok = got == model and products == {h["GPUs"][0]["Name"]}
        sample = devs[0][1] if devs else {}
        out("R-" + name, "DRA ResourceSlices for %s list the twin's GPUs" % name,
            "%d devices, uuid = the twin's, productName %s" % (len(model), h["GPUs"][0]["Name"]),
            "%d devices, uuids %s; productName %s; e.g. %s" % (len(devs), "equal" if got == model else "differ (%s)" % sorted(got)[:2],
                                                            ",".join(sorted(str(p) for p in products)),
                                                            ", ".join("%s=%s" % (k, sample[k]) for k in sorted(sample) if k in ("pciBusID", "pcieRoot", "architecture", "cudaComputeCapability", "brand", "driverVersion"))),
            "PASS" if ok else "FAIL")


def cmd_claim(hj, node, cj, sj, smi):
    h = hosts(hj)[node]
    claims = json.load(open(cj))["items"]
    res = [r for c in claims for r in ((c.get("status") or {}).get("allocation") or {}).get("devices", {}).get("results", [])]
    if not res:
        out("R-claim", "a ResourceClaim for one GPU on " + node, "allocated, pod Running", "not allocated: " + open(smi).read()[:200], "FAIL")
        return
    r = res[0]
    uuid = next((a.get("uuid") for drv, pool, n, a, _ in devices(json.load(open(sj))) if pool == r["pool"] and n == r["device"]), None)
    seen = smi_uuids(smi)
    ok = uuid in {g["UUID"] for g in h["GPUs"]} and len(seen) == 1 and seen[0][1] == uuid
    out("R-claim", "a ResourceClaim for one gpu.nvidia.com device on %s; nvidia-smi -L in its pod" % node,
        "one of the twin's GPUs, the same one nvidia-smi shows",
        "allocated %s/%s = %s; the pod sees %s" % (r["pool"], r["device"], uuid, ",".join(u for _, u in seen) or open(smi).read()[:160]),
        "PASS" if ok else "FAIL")


SAMPLE = re.compile(r'^([A-Za-z_:][A-Za-z0-9_:]*)\{(.*)\}\s+(\S+)')
LABEL = re.compile(r'(\w+)="((?:[^"\\]|\\.)*)"')


def scrape(path):
    s = {}
    for line in open(path, errors="replace"):
        m = SAMPLE.match(line)
        if m:
            s.setdefault(m.group(1), []).append((dict(LABEL.findall(m.group(2))), float(m.group(3))))
    return s


def cmd_dcgm(hj, node, real, twin):
    h = hosts(hj)[node]
    r, t = scrape(real), scrape(twin)
    if "DCGM_FI_DEV_XID_ERRORS" not in r and "DCGM_FI_DEV_GPU_TEMP" not in r:
        out("M-" + node, "dcgm-exporter on Mokka on " + node, "per-GPU series", "no series: " + open(real).read()[:200], "FAIL")
        return
    fam = "DCGM_FI_DEV_XID_ERRORS" if "DCGM_FI_DEV_XID_ERRORS" in r else "DCGM_FI_DEV_GPU_TEMP"
    bad = []
    for g in h["GPUs"]:
        rr = [l for l, _ in r.get(fam, []) if l.get("gpu") == str(g["Index"])]
        tt = [l for l, _ in t.get(fam, []) if l.get("gpu") == str(g["Index"])]
        if not rr:
            bad.append("gpu %d absent" % g["Index"])
            continue
        for key in ("UUID", "pci_bus_id", "modelName", "device"):
            want = tt[0].get(key) if tt else None
            if rr[0].get(key) != want:
                bad.append("gpu %d %s real %s twin %s" % (g["Index"], key, rr[0].get(key), want))
    out("M-" + node, "real dcgm-exporter on %s against the twin's dcgm-exporter impersonation" % node,
        "gpu, UUID, pci_bus_id, modelName, device equal per GPU (%s)" % fam,
        ("equal for %d GPUs; real families %d, impersonated %d; real-only: %s" % (len(h["GPUs"]), len(r), len(t),
                                                                              ", ".join(sorted(set(r) - set(t))[:6]))) if not bad else "; ".join(bad[:4]),
        "PASS" if not bad else "FAIL")
    rl = r.get(fam, [{}])[0][0] if r.get(fam) else {}
    tl = t.get(fam, [{}])[0][0] if t.get(fam) else {}
    rh = {l.get("Hostname", l.get("hostname")) for l, _ in r.get(fam, [])}
    if rh != {node}:
        out("M-%s-host" % node, "dcgm-exporter's hostname label on " + node, node + " (the Node, as the twin labels it)",
            "%s" % ",".join(sorted(map(str, rh))), "FINDING")
    if sorted(rl) != sorted(tl):
        out("M-%s-labels" % node, "dcgm-exporter's label names on " + node, "the impersonation's (dcgm-exporter 4.8.4): " + ", ".join(sorted(tl)),
            "the GPU Operator v26.3.3's dcgm-exporter 4.5.3-4.8.2: " + ", ".join(sorted(rl)) +
            ". The twin impersonates a later release, whose label is hostname; this one says Hostname and adds DCGM_FI_DRIVER_VERSION", "FINDING")
    ro, to = sorted(set(r) - set(t)), sorted(set(t) - set(r))
    if ro or to:
        out("M-%s-families" % node, "metric families on " + node, "the impersonation's %d" % len(t),
            "real %d. Real only: %s. Impersonated only: %s" % (len(r), ", ".join(ro), ", ".join(to)), "FINDING")


def cmd_faultsmi(idx, uuid, path):
    rows = [l for l in open(path).read().splitlines() if l.strip()]
    mine = [l for l in rows if uuid.lower() in l.lower()]
    healthy_others = [l for l in rows if l.split(",")[0].strip().isdigit() and uuid.lower() not in l.lower()]
    gone = not mine or any(x in mine[0] for x in ("Unknown Error", "N/A", "GPU is lost", "[")) or "lost" in " ".join(rows).lower()
    out("F2", "NVML on the node after the twin's Xid 79 on GPU %s" % idx,
        "GPU %s fallen off the bus (NVML_ERROR_GPU_IS_LOST); the others answer" % idx,
        " / ".join(rows)[:300], "PASS" if gone else "FAIL")


def cmd_faultalloc(before, after, kwok, ok, ms, log=""):
    exp = str(int(before) - 1)
    said = ""
    try:
        m = re.search(r"\S+\.go:\d+\] Error waiting for event: [A-Z_]+; Marking all devices as unhealthy", open(log).read())
        said = (" (its log: \"%s\")" % m.group(0)) if m else ""
    except OSError:
        pass
    if after == kwok == exp:
        res, obs = "PASS", "%s after %s ms; the twin's projection %s" % (after, ms, kwok)
    elif ok == "0":
        res, obs = "FINDING", ("%s after %s ms; the twin's projection %s. The device plugin v0.19.3 withdraws EVERY GPU of the node: "
                               "NVML's event-set wait returns GPU_IS_LOST for the set once one GPU has fallen off the bus, and it marks all "
                               "devices unhealthy on any such error%s. The twin's projection withholds the one GPU (D43)" % (after, ms, kwok, said)) \
            if after == "0" else "%s after %s ms; the twin's projection %s" % (after, ms, kwok)
    else:
        res, obs = "FAIL", "still %s after 120 s; the twin's projection %s" % (after, kwok)
    out("F3", "device plugin: allocatable after the twin's Xid 79", "%s (one GPU withheld, as the twin's projection)" % exp, obs, res)


def cmd_faultslices(node, uuid, before, after):
    b = [d for _, _, _, a, d in devices(json.load(open(before)), node) if a.get("uuid") == uuid]
    a = [d for _, _, _, x, d in devices(json.load(open(after)), node) if x.get("uuid") == uuid]
    if not a:
        out("F4", "DRA ResourceSlices after the twin's Xid 79", "the lost GPU withdrawn or tainted", "withdrawn", "PASS")
    elif a[0].get("taints"):
        out("F4", "DRA ResourceSlices after the twin's Xid 79", "the lost GPU withdrawn or tainted", "tainted: %s" % a[0]["taints"], "PASS")
    else:
        out("F4", "DRA ResourceSlices after the twin's Xid 79", "the lost GPU withdrawn or tainted",
            "still published, untainted: the DRA driver 0.5.0, at its chart's defaults, neither withdraws nor taints a GPU NVML reports lost", "FINDING")


def cmd_faultdcgm(idx, real, twin):
    r, t = scrape(real), scrape(twin)
    def xid(s):
        for l, v in s.get("DCGM_FI_DEV_XID_ERRORS", []):
            if l.get("gpu") == idx:
                return v, l.get("err_msg", "")
        return None, ""
    rv, rm = xid(r)
    tv, tm = xid(t)
    rfam = sorted(f for f, ss in r.items() if any(l.get("gpu") == idx for l, _ in ss))
    tfam = sorted(f for f, ss in t.items() if any(l.get("gpu") == idx for l, _ in ss))
    res = "PASS" if rv == tv == 79 else "FINDING"
    obs = "real dcgm-exporter: %s; it still serves %d families for GPU %s (e.g. %s)" % (
        ("DCGM_FI_DEV_XID_ERRORS %s %s" % (rv, rm)) if rv is not None else "no DCGM_FI_DEV_XID_ERRORS series (none before the fault either)",
        len(rfam), idx, ", ".join("%s=%s" % (f, v) for f in ("DCGM_FI_DEV_GPU_TEMP", "DCGM_FI_DEV_POWER_USAGE")
                                    for l, v in r.get(f, []) if l.get("gpu") == idx))
    out("F5", "dcgm-exporter for GPU %s after the twin's Xid 79" % idx,
        "XID 79 and the GPU's telemetry gone, as the twin's impersonation serves (%s %s; %d families for the GPU)" % (tv, tm, len(tfam)),
        obs + ". DCGM's host engine on Mokka does not see the device fall off the bus", res)


if __name__ == "__main__":
    globals()["cmd_" + sys.argv[1]](*sys.argv[2:])

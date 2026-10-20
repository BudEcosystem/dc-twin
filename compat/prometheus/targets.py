"""Write Prometheus file_sd targets from `dctwin endpoints` (JSON on stdin).

Each exporter route is a site name "<node>:<port>"; the port says which exporter it is
(internal/adapter/metrics/exporters.go: dcgm 9400, amd 5000, gaudi 41611). One file per exporter.
Usage: dctwin endpoints | python3 -I targets.py <out-dir>
"""
import json
import os
import sys

PORTS = {"9400": "dcgm", "5000": "amd", "41611": "gaudi"}

def main() -> None:
    out = sys.argv[1]
    os.makedirs(out, exist_ok=True)
    routes = json.load(sys.stdin)["routes"]
    groups = {name: [] for name in PORTS.values()}
    for site in sorted(routes):
        node, _, port = site.rpartition(":")
        if port in PORTS:
            groups[PORTS[port]].append(site)
    for name, targets in groups.items():
        with open(os.path.join(out, name + ".json"), "w") as f:
            json.dump([{"targets": targets, "labels": {"exporter": name}}], f, indent=1)
        print(f"{name}: {len(targets)} targets")

if __name__ == "__main__":
    main()

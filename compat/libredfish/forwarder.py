#!/usr/bin/env python3
# Stands in for libredfish's tests/redfishMockupServer.py: instead of serving a captured
# mockup, it forwards the port the integration test connects to onto the dc-twin BMC that
# emulates the same vendor. TLS passes through untouched; the twin's BMC answers.
# Usage (as the test invokes the mockup server): --port P --dir mockups/<vendor>/ [--ssl ...]
import argparse, json, os, socket, sys, threading

ap = argparse.ArgumentParser()
ap.add_argument("--port", type=int, required=True)
ap.add_argument("--dir", required=True)
ap.add_argument("--ssl", action="store_true")
ap.add_argument("--cert")
ap.add_argument("--key")
a = ap.parse_args()
vendor = os.path.basename(os.path.normpath(a.dir))
mapping = json.load(open(os.environ["DCTWIN_LIBREDFISH_MAP"]))
if vendor not in mapping:
    print(f"forwarder: no twin BMC for {vendor}", file=sys.stderr)
    sys.exit(1)
host, port = mapping[vendor].rsplit(":", 1)
port = int(port)

def pipe(src, dst):
    try:
        while True:
            b = src.recv(65536)
            if not b:
                break
            dst.sendall(b)
    except OSError:
        pass
    finally:
        for s in (src, dst):
            try:
                s.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass

srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", a.port))
srv.listen(64)
print(f"forwarder: 127.0.0.1:{a.port} ({vendor}) -> twin BMC {host}:{port}", file=sys.stderr, flush=True)
while True:
    c, _ = srv.accept()
    try:
        u = socket.create_connection((host, port))
    except OSError:
        c.close()
        continue
    threading.Thread(target=pipe, args=(c, u), daemon=True).start()
    threading.Thread(target=pipe, args=(u, c), daemon=True).start()

#!/usr/bin/env python3
# Serves a captured Redfish mockup exactly as captured (read-only, plain HTTP, no auth), so the
# validator can be run against the capture itself. Errors it reports there belong to the captured
# vendor data; errors the twin adds on top belong to the twin's bindings.
# Lookups are exact (case-sensitive, trailing "/" ignored), as the twin's own tree lookups are.
# Usage: capture_server.py MOCKUP_DIR  -- prints "port N" once listening on 127.0.0.1
import http.server
import json
import os
import sys

root = os.path.abspath(sys.argv[1])
docs = {}
for d, _, files in os.walk(root):
    rel = "/" + os.path.relpath(d, root).replace(os.sep, "/")
    for f in files:
        if f in ("index.json", "index.xml"):
            docs[(rel.rstrip("/") or "/", f)] = os.path.join(d, f)


class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, code, body, ctype):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("OData-Version", "4.0")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def do_GET(self):
        p = self.path.split("?", 1)[0].rstrip("/") or "/"
        if p == "/redfish" and ("/redfish", "index.json") not in docs:
            return self._send(200, json.dumps({"v1": "/redfish/v1/"}).encode(), "application/json")
        for name, ctype in (("index.json", "application/json"), ("index.xml", "application/xml")):
            f = docs.get((p, name))
            if f:
                return self._send(200, open(f, "rb").read(), ctype)
        err = {"error": {"code": "Base.1.8.ResourceNotFound", "message": f"{p} is not in the capture"}}
        self._send(404, json.dumps(err).encode(), "application/json")

    do_HEAD = do_GET

    def do_POST(self):
        self._send(405, b"{}", "application/json")

    do_PATCH = do_PUT = do_DELETE = do_POST


srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
print(f"port {srv.server_address[1]}", flush=True)
srv.serve_forever()

#!/usr/bin/env python3
"""Load stack-inputs/day0/netbox into a NetBox as a site does: through NetBox's own CSV import.

Logs in to NetBox's web UI and, for each file in the README's order, posts it to that object
type's Import form (`POST /<app>/<model>/import/`, `import_method=direct`, `format=csv`), the
form a person pastes a CSV into. NetBox imports each file in one transaction: a 302 to the list
is success; any other answer is the form back with its errors, which are printed, and the import
stops. The REST API takes no CSV, so this is the only way NetBox itself parses these files.
Python's standard library only.

  import-csv.py <netbox url> <stack-inputs/day0/netbox> --user <u> --password <pw>
"""
import argparse
import html
import http.cookiejar
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

# each file's import form, by NetBox 4.2's URL names (the app's <model>_bulk_import view)
IMPORT = {
    "custom_fields.csv": "/extras/custom-fields/import/",
    "sites.csv": "/dcim/sites/import/",
    "racks.csv": "/dcim/racks/import/",
    "manufacturers.csv": "/dcim/manufacturers/import/",
    "platforms.csv": "/dcim/platforms/import/",
    "device_types.csv": "/dcim/device-types/import/",
    "device_roles.csv": "/dcim/device-roles/import/",
    "devices.csv": "/dcim/devices/import/",
    "vrfs.csv": "/ipam/vrfs/import/",
    "interfaces.csv": "/dcim/interfaces/import/",
    "mac_addresses.csv": "/dcim/mac-addresses/import/",
    "cables.csv": "/dcim/cables/import/",
    "rirs.csv": "/ipam/rirs/import/",
    "asns.csv": "/ipam/asns/import/",
    "vlans.csv": "/ipam/vlans/import/",
    "prefixes.csv": "/ipam/prefixes/import/",
    "ip_addresses.csv": "/ipam/ip-addresses/import/",
}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


def order(readme):
    """The import order the README gives (its numbered list of files)."""
    with open(readme) as f:
        return re.findall(r"^\s*\d+\. `([a-z_]+\.csv)`", f.read(), re.M)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("url")
    ap.add_argument("dir")
    ap.add_argument("--user", required=True)
    ap.add_argument("--password", required=True)
    a = ap.parse_args()
    base = a.url.rstrip("/")
    jar = http.cookiejar.CookieJar()
    web = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar), NoRedirect)

    def csrf():
        return next((c.value for c in jar if c.name == "csrftoken"), "")

    def post(path, fields):
        body = urllib.parse.urlencode(dict(fields, csrfmiddlewaretoken=csrf())).encode()
        req = urllib.request.Request(base + path, data=body, headers={"Referer": base + path})
        try:
            with web.open(req, timeout=300) as r:
                return r.status, r.read().decode(errors="replace")
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode(errors="replace")

    web.open(base + "/login/", timeout=60).read()
    st, _ = post("/login/", {"username": a.user, "password": a.password, "next": "/"})
    if st != 302:
        print("login as %s: HTTP %d" % (a.user, st), file=sys.stderr)
        return 1
    for name in order(os.path.join(a.dir, "README.md")):
        with open(os.path.join(a.dir, name)) as f:
            data = f.read()
        rows = len(data.strip().splitlines()) - 1
        if rows <= 0:
            print("%s: no rows, skipped" % name)
            continue
        st, page = post(IMPORT[name], {"import_method": "direct", "data": data, "format": "csv", "csv_delimiter": ","})
        if st != 302:
            # the form back with its errors: each with the text around it, where NetBox names the
            # record and the field
            text = re.sub(r"\s+", " ", html.unescape(re.sub(r"<[^>]+>", " ", re.sub(r"(?s)<(script|style).*?</\1>", "", page))))
            # NetBox's bulk import names a bad row "Record <n> <field>: <error>" on the data field
            rec = re.findall(r"Record \d+[^|]{0,240}", text)
            ctx = " | ".join(r.strip() for r in rec[:8]) or text[:3000]
            print("%s: HTTP %d, not imported: %s" % (name, st, ctx), file=sys.stderr)
            return 1
        print("%s: %d rows imported" % (name, rows))
    return 0


if __name__ == "__main__":
    sys.exit(main())

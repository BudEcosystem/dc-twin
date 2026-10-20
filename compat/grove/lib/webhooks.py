"""Point a chart's admission webhooks (and CRD conversion webhooks) at a loopback URL.

An operator run as a process (or a container with a published port) has no in-cluster Service in
front of it, so the `clientConfig.service` its chart renders is replaced by `url:
https://127.0.0.1:<port><path>` and the scratch CA's bundle. Everything else in the rendered
objects is kept as the chart ships it. Standard library only.

  python3 -I webhooks.py <rendered.yaml> <out.yaml> <port> <ca.crt>
  python3 -I webhooks.py <rendered.yaml> <out.yaml> <service>=<port>[,<service>=<port>...] <ca.crt>

With one port every webhook goes to it; with a map each goes to the port of the Service it names.

Only the documents of kind MutatingWebhookConfiguration, ValidatingWebhookConfiguration and
CustomResourceDefinition (those with a conversion webhook) are written out.
"""
import base64
import re
import sys

src, out, ports, ca = sys.argv[1:5]
cab = base64.b64encode(open(ca, "rb").read()).decode()
portmap = dict(p.split("=", 1) for p in ports.split(",")) if "=" in ports else None

# clientConfig:
#   [caBundle: ...]
#   service:
#     name: x
#     namespace: y
#     path: /p
#     [port: n]
PATTERN = re.compile(
    r"clientConfig:\n"
    r"(?:(?P<ind0>[ \t]+)caBundle: [^\n]*\n)?"
    r"(?:[ \t]*#[^\n]*\n)*"
    r"(?P<ind>[ \t]+)service:\n"
    r"(?P<body>(?:(?P=ind)[ \t]+[^\n]*\n)+)"
)


def repl(m):
    ind = m.group("ind")
    path = re.search(r"path: ([^\n]+)", m.group("body"))
    p = path.group(1).strip().strip('"') if path else "/"
    port = ports
    if portmap is not None:
        name = re.search(r"name: ([^\n]+)", m.group("body")).group(1).strip().strip('"')
        port = portmap[name]
    return "clientConfig:\n%scaBundle: %s\n%surl: https://127.0.0.1:%s%s\n" % (ind, cab, ind, port, p)


docs = open(src).read().split("\n---\n")
keep = []
for d in docs:
    kind = re.search(r"^kind: (\S+)", d, re.M)
    if not kind:
        continue
    k = kind.group(1)
    if k in ("MutatingWebhookConfiguration", "ValidatingWebhookConfiguration") or (
        k == "CustomResourceDefinition" and "strategy: Webhook" in d
    ):
        keep.append(PATTERN.sub(repl, d))
    elif k == "CustomResourceDefinition":
        keep.append(d)
open(out, "w").write("\n---\n".join(keep) + "\n")

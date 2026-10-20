"""Small, dependency-free edits to a project's own rendered install manifests (Kueue's Helm chart,
Volcano's volcano-development.yaml), used by compat/kueue and compat/volcano. No YAML library:
the inputs are generated files at a pinned tag, and the edits are line-level.

  render.py kinds <file> <Kind>[,<Kind>...]        the documents of these kinds, '---'-separated
  render.py service-port <service> <port> <file>   every webhook clientConfig.service block that
                                                   names <service> gets `port: <port>` (replacing
                                                   any port it had)
  render.py ca-bundle <ca.crt> <file>              every webhook clientConfig gets caBundle: the CA,
                                                   where no cert-injector (OLM, cert-manager) runs

The webhook configurations and CRD conversion stanzas refer to a Service (port 443 by default).
The harness serves the webhook on a loopback port and makes that Service an ExternalName for
localhost, which kube-apiserver resolves to localhost:<the port in the clientConfig>; the TLS
server name stays <service>.<namespace>.svc, so each project's own certificate and CA bundle
handling is unchanged. Only the port is written in.
"""
import re
import sys


def documents(text):
    doc = []
    for line in text.splitlines():
        if line.startswith("---"):
            if doc:
                yield doc
            doc = []
            continue
        doc.append(line)
    if doc:
        yield doc


def kind_of(doc):
    for line in doc:
        m = re.match(r"^kind:\s*(\S+)\s*$", line)
        if m:
            return m.group(1)
    return None


def kinds(path, wanted):
    want = set(wanted.split(","))
    out = []
    with open(path) as f:
        for doc in documents(f.read()):
            if kind_of(doc) in want:
                out.append("\n".join(doc))
    print("\n---\n".join(out))


def service_port(service, port, path):
    with open(path) as f:
        lines = f.read().splitlines()
    out = []
    i = 0
    n = 0
    while i < len(lines):
        line = lines[i]
        out.append(line)
        m = re.match(r"^(\s*)service:\s*$", line)
        if m:
            indent = None
            block = []
            j = i + 1
            # the block: lines indented deeper than `service:`
            while j < len(lines):
                l = lines[j]
                lead = len(l) - len(l.lstrip(" "))
                if l.strip() and lead <= len(m.group(1)):
                    break
                if indent is None and l.strip():
                    indent = l[:lead]
                block.append(l)
                j += 1
            names = [b for b in block if re.match(r"^\s*name:\s*['\"]?" + re.escape(service) + r"['\"]?\s*$", b)]
            if names and indent is not None:
                # a `port:` already there (Volcano's manifests say 443) is replaced, else added
                block = [b for b in block if not re.match(r"^\s*port:", b)]
                block.append(f"{indent}port: {port}")
                n += 1
            out.extend(block)
            i = j
            continue
        i += 1
    if n == 0:
        sys.exit(f"render.py: no clientConfig.service block names {service} in {path}")
    print("\n".join(out))


def ca_bundle(ca_path, path):
    import base64
    with open(ca_path, "rb") as f:
        ca = base64.b64encode(f.read()).decode()
    with open(path) as f:
        lines = f.read().splitlines()
    out = []
    n = 0
    for k, line in enumerate(lines):
        m = re.match(r"^(\s*)service:\s*$", line)
        if m and k > 0 and lines[k - 1].strip() == "clientConfig:":
            out.append(f"{m.group(1)}caBundle: {ca}")
            n += 1
        out.append(line)
    if n == 0:
        sys.exit(f"render.py: no clientConfig.service block in {path}")
    print("\n".join(out))


if __name__ == "__main__":
    if len(sys.argv) == 4 and sys.argv[1] == "kinds":
        kinds(sys.argv[2], sys.argv[3])
    elif len(sys.argv) == 5 and sys.argv[1] == "service-port":
        service_port(sys.argv[2], sys.argv[3], sys.argv[4])
    elif len(sys.argv) == 4 and sys.argv[1] == "ca-bundle":
        ca_bundle(sys.argv[2], sys.argv[3])
    else:
        sys.exit(__doc__)

#!/usr/bin/env python3
"""sonic-ztp-reference.py <sonic-ztp checkout> <doc.json>...: ZTP JSON documents to check against.

Collects every ZTP JSON document sonic-ztp's own tests hold (tests/*.py: string literals with a
top-level "ztp" object, read with ast, never importing or running sonic-ztp) and the documents
given (the ones the day-0 tooling writes), and prints them with the checkout's commit.
internal/day0/ztp's reference test reads every document with the twin's ZTP engine and checks it
agrees with sonic-ztp's rules (ZTPSections.py, ztp-engine.py at that commit; the test cites them):
testdata/sonic-ztp-reference.json.
"""
import ast
import glob
import json
import os
import subprocess
import sys

root, given = sys.argv[1], sys.argv[2:]
docs = []
for path in given:
    with open(path) as f:
        docs.append({"name": "tooling/" + os.path.basename(path), "doc": json.load(f)})
for path in sorted(glob.glob(os.path.join(root, "tests", "*.py"))):
    with open(path) as f:
        tree = ast.parse(f.read(), path)
    n = 0
    for node in ast.walk(tree):
        if isinstance(node, ast.Constant) and isinstance(node.value, str) and '"ztp"' in node.value:
            try:
                d = json.loads(node.value)
            except ValueError:
                continue
            if isinstance(d, dict) and isinstance(d.get("ztp"), dict):
                n += 1
                docs.append({"name": "%s#%d" % (os.path.basename(path), n), "doc": d})

commit = subprocess.run(["git", "-C", root, "rev-parse", "HEAD"], capture_output=True, text=True).stdout.strip()
json.dump({"source": "https://github.com/sonic-net/sonic-ztp", "commit": commit, "docs": docs}, sys.stdout, indent=1, sort_keys=True)
print()

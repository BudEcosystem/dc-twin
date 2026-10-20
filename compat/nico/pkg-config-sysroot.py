#!/usr/bin/env python3
"""compat/nico/pkg-config-sysroot.py — a minimal pkg-config for boot-artifacts.sh's cross build (rust-cross).

Reads .pc files from PKG_CONFIG_LIBDIR (colon-separated) and prefixes their -I and -L paths with
PKG_CONFIG_SYSROOT_DIR, answering what the Rust pkg-config crate asks (--libs, --cflags,
--modversion, --atleast-version, --variable, --static), so libudev-sys and tss-esapi-sys find the
target's libraries in a sysroot copied from the build container. Only the standard library.
"""
import os, re, sys
libdirs = [d for d in os.environ.get("PKG_CONFIG_LIBDIR", "").split(":") if d]
sysroot = os.environ.get("PKG_CONFIG_SYSROOT_DIR", "")
def load(name):
    for d in libdirs:
        p = os.path.join(d, name + ".pc")
        if os.path.exists(p):
            vars, fields = {}, {}
            for line in open(p):
                line = line.split("#", 1)[0].strip()
                if not line:
                    continue
                m = re.match(r"^([A-Za-z0-9_.]+)\s*=\s*(.*)$", line)
                if m:
                    vars[m.group(1)] = m.group(2)
                    continue
                m = re.match(r"^([A-Za-z0-9_.]+):\s*(.*)$", line)
                if m:
                    fields[m.group(1)] = m.group(2)
            def sub(s):
                for _ in range(10):
                    s2 = re.sub(r"\$\{([^}]+)\}", lambda m: vars.get(m.group(1), ""), s)
                    if s2 == s:
                        break
                    s = s2
                return s
            vars = {k: sub(v) for k, v in vars.items()}
            return {k: sub(v) for k, v in fields.items()}, vars
    return None, None
def vcmp(a, b):
    pa = [int(x) if x.isdigit() else 0 for x in re.split(r"[.\-]", a)]
    pb = [int(x) if x.isdigit() else 0 for x in re.split(r"[.\-]", b)]
    return (pa > pb) - (pa < pb)
def flags(s):
    out = []
    for f in s.split():
        if sysroot and (f.startswith("-I/") or f.startswith("-L/")):
            f = f[:2] + sysroot + f[2:]
        out.append(f)
    return out
args = sys.argv[1:]
want, pkgs, atleast, var = set(), [], None, None
i = 0
while i < len(args):
    a = args[i]
    if a.startswith("--atleast-version"):
        if "=" in a:
            atleast = a.split("=", 1)[1]
        else:
            i += 1; atleast = args[i]
    elif a.startswith("--variable="):
        var = a.split("=", 1)[1]
    elif a.startswith("--"):
        want.add(a)
    else:
        pkgs.append(a)
    i += 1
if "--version" in want:
    print("0.29.2"); sys.exit(0)
out = []
for p in pkgs:
    f, v = load(p)
    if f is None:
        sys.stderr.write("Package %s was not found in the pkg-config search path.\n" % p); sys.exit(1)
    if atleast and vcmp(f.get("Version", "0"), atleast) < 0:
        sys.stderr.write("Requested '%s >= %s' but version of %s is %s\n" % (p, atleast, p, f.get("Version"))); sys.exit(1)
    if var:
        out.append(v.get(var, "")); continue
    if "--modversion" in want:
        out.append(f.get("Version", "")); continue
    if "--cflags" in want:
        out += flags(f.get("Cflags", ""))
    if "--libs" in want:
        out += flags(f.get("Libs", ""))
        if "--static" in want:
            out += flags(f.get("Libs.private", ""))
seen, res = set(), []
for o in out:
    if o not in seen or not o.startswith("-"):
        seen.add(o); res.append(o)
print(" ".join(res) if not ("--modversion" in want or var) else "\n".join(out))

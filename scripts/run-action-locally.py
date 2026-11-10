#!/usr/bin/env python3
"""Run a composite GitHub Action's own steps on this machine, as the runner would: each `run:`
step in bash with RUNNER_TEMP, GITHUB_OUTPUT and GITHUB_STEP_SUMMARY, its `${{ inputs.* }}`,
`${{ runner.temp }}`, `${{ github.* }}` and `${{ steps.<id>.outputs.* }}` substituted, its `if:`
evaluated for the forms the action uses. `uses:` steps (setup-go, upload-artifact) are reported
and skipped: this machine has Go, and artifacts stay in RUNNER_TEMP.

  scripts/run-action-locally.py .github/actions/dctwin-critic/action.yml campaign=examples/campaigns/refstack-smoke.yaml
"""
import os, re, subprocess, sys, tempfile, yaml

action = yaml.safe_load(open(sys.argv[1]))
inputs = {k: str(v.get("default", "")) for k, v in action.get("inputs", {}).items()}
for a in sys.argv[2:]:
    k, v = a.split("=", 1)
    inputs[k] = v
event = os.environ.get("GITHUB_EVENT_NAME", "push")
github = {"sha": os.environ.get("GITHUB_SHA", "local"), "event_name": event, "token": ""}
for k, spec in action.get("inputs", {}).items():
    if spec.get("required") and not inputs.get(k):
        sys.exit(f"input {k} is required")
temp = tempfile.mkdtemp(prefix="runner-temp-")
outputs = {}

def expr(e):
    e = e.strip()
    m = re.fullmatch(r"inputs\.(\w+)", e)
    if m: return inputs.get(m.group(1), "")
    if e == "runner.temp": return temp
    m = re.fullmatch(r"github\.([\w.]+)", e)
    if m: return github.get(m.group(1), "")
    m = re.fullmatch(r"steps\.(\w+)\.outputs\.(\w+)", e)
    if m: return outputs.get(m.group(1), {}).get(m.group(2), "")
    raise SystemExit(f"unsupported expression: {e}")

def subst(s):
    return re.sub(r"\$\{\{(.*?)\}\}", lambda m: expr(m.group(1)), s)

def condition(c, failed):
    if c is None: return not failed
    c = c.replace("${{", "").replace("}}", "")
    parts = [p.strip() for p in c.split("&&")]
    ok = True
    for p in parts:
        if p == "always()": continue
        m = re.fullmatch(r"(.+?)\s*==\s*'(.*)'", p)
        if not m: raise SystemExit(f"unsupported if: {p}")
        ok = ok and expr(m.group(1)) == m.group(2)
    return ok and ("always()" in c or not failed)

failed = False
for i, st in enumerate(action["runs"]["steps"], 1):
    name = st.get("name") or st.get("uses")
    if "uses" in st:
        print(f"[{i}] skip uses: {st['uses']}")
        continue
    if not condition(st.get("if"), failed):
        print(f"[{i}] skip (if: {st.get('if')}): {name}")
        continue
    out = os.path.join(temp, f"output-{i}")
    open(out, "w").close()
    env = dict(os.environ, RUNNER_TEMP=temp, GITHUB_OUTPUT=out, GITHUB_STEP_SUMMARY=os.path.join(temp, "summary.md"))
    env.update({k: subst(str(v)) for k, v in (st.get("env") or {}).items()})
    print(f"[{i}] run: {name}", flush=True)
    rc = subprocess.call(["bash", "-e", "-c", subst(st["run"])], env=env)
    if st.get("id"):
        outputs[st["id"]] = dict(l.rstrip("\n").split("=", 1) for l in open(out) if "=" in l)
    print(f"[{i}] exit {rc}", flush=True)
    if rc != 0:
        failed = True
        break
print("outputs:", outputs)
print("summary:", os.path.join(temp, "summary.md"))
sys.exit(1 if failed else 0)

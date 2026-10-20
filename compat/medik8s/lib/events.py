"""Summarise what the twin logged for one device since a sequence number: the requests that came
in over a BMC protocol and the power transitions they caused. Reads `dctwin events --json` on
stdin. Used by compat/medik8s/run.sh.

  events.py summary <protocol>     "ipmi power.off.force #812, ipmi power.on #815; power On>Off>PoweringOn>On"
  events.py has <protocol> <canonical>[,<canonical>...]   exit 0 when every canonical was accepted over <protocol>
"""
import json
import sys


def load():
    data = json.load(sys.stdin) or []
    return data


def requests(evs, protocol):
    out = []
    for e in evs:
        r = e.get("request") or {}
        if e.get("kind") != "request" or r.get("protocol") != protocol:
            continue
        c = r.get("canonical", "")
        if c.startswith("session.") or c.endswith(".read") or c == "other":
            continue
        s = r.get("status")
        accepted = s is None or s in (0, 200, 202, 204)
        out.append((e.get("seq"), c, accepted))
    return out


def power_path(evs):
    states = []
    for e in evs:
        st = e.get("state") or {}
        if e.get("kind") == "state" and st.get("field") == "power":
            if not states:
                states.append(st.get("from"))
            states.append(st.get("to"))
    return ">".join(states)


def main():
    if len(sys.argv) == 3 and sys.argv[1] == "summary":
        evs = load()
        reqs = requests(evs, sys.argv[2])
        parts = [f"{sys.argv[2]} {c} #{s}" + ("" if ok else " (refused)") for s, c, ok in reqs]
        print((", ".join(parts) or f"no {sys.argv[2]} power requests") + "; power " + (power_path(evs) or "unchanged"))
        return 0
    if len(sys.argv) == 4 and sys.argv[1] == "has":
        evs = load()
        got = {c for _, c, ok in requests(evs, sys.argv[2]) if ok}
        return 0 if all(c in got for c in sys.argv[3].split(",")) else 1
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main())

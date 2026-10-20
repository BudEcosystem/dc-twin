"""events.py [filters]: NVSentinel's health events as its datastore holds them, one JSON document
per line on stdin (health_events.document, oldest first), filtered, one summary line each.

  --agent A        the monitor that published it (syslog-health-monitor, gpu-health-monitor)
  --check C        its check name (SysLogsXIDError, GpuAllWatch, …)
  --healthy        only healthy events; --unhealthy only unhealthy ones
  --fatal          only fatal ones
  --code X         only those whose errorCode list holds X (an Xid number, a DCGM_FR_* name)
  --first          the first match only
  --count          print the number of matches instead

A document's keys are read case-blind (the datastore keeps them as the protobuf's JSON names).
"""
import json
import sys


# RecommendedAction (data-models/protobufs/health_event.proto at the tag): the datastore keeps the number
ACTIONS = {0: "NONE", 2: "COMPONENT_RESET", 5: "CONTACT_SUPPORT", 6: "RUN_FIELDDIAG", 15: "RESTART_VM", 24: "RESTART_BM",
           25: "REPLACE_VM", 26: "RUN_DCGMEUD", 27: "CUSTOM", 99: "UNKNOWN"}


def low(o):
    if isinstance(o, dict):
        return {k.lower(): low(v) for k, v in o.items()}
    if isinstance(o, list):
        return [low(v) for v in o]
    return o


def main(argv):
    want = {}
    flags = set()
    i = 0
    while i < len(argv):
        a = argv[i]
        if a in ("--agent", "--check", "--code"):
            want[a[2:]] = argv[i + 1]
            i += 2
            continue
        if a in ("--healthy", "--unhealthy", "--fatal", "--first", "--count"):
            flags.add(a[2:])
            i += 1
            continue
        sys.exit(__doc__)
    n = 0
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            d = low(json.loads(line))
        except ValueError:
            continue
        d = d.get("healthevent", d)
        if "agent" in want and d.get("agent") != want["agent"]:
            continue
        if "check" in want and d.get("checkname") != want["check"]:
            continue
        healthy = bool(d.get("ishealthy"))
        if "healthy" in flags and not healthy or "unhealthy" in flags and healthy:
            continue
        if "fatal" in flags and not d.get("isfatal"):
            continue
        codes = [str(c) for c in d.get("errorcode") or []]
        if "code" in want and want["code"] not in codes:
            continue
        n += 1
        if "count" in flags:
            continue
        ents = ",".join("%s=%s" % (e.get("entitytype"), e.get("entityvalue")) for e in d.get("entitiesimpacted") or [])
        print("agent=%s check=%s healthy=%s fatal=%s errorCode=%s action=%s entities=%s node=%s message=%s" % (
            d.get("agent"), d.get("checkname"), healthy, bool(d.get("isfatal")), codes,
            ACTIONS.get(d.get("recommendedaction") or 0, d.get("recommendedaction")),
            ents, d.get("nodename"), " ".join(str(d.get("message") or "").split())[:160]))
        if "first" in flags:
            break
    if "count" in flags:
        print(n)


if __name__ == "__main__":
    main(sys.argv[1:])

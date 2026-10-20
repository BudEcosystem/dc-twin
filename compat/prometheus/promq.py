"""A small client for Prometheus's HTTP API (/api/v1/query, /api/v1/alerts), for run.sh.

  promq.py <base-url> query <promql>            one line per series: {labels} value
  promq.py <base-url> count <promql>            number of series
  promq.py <base-url> value <promql>            the first series' value (empty if none)
  promq.py <base-url> alerts <alertname>        firing alerts of that name: count, then one line each
"""
import json
import sys
import urllib.parse
import urllib.request


def get(base: str, path: str, params: dict) -> dict:
    url = base.rstrip("/") + path + ("?" + urllib.parse.urlencode(params) if params else "")
    with urllib.request.urlopen(url, timeout=10) as r:
        d = json.load(r)
    if d.get("status") != "success":
        raise SystemExit(f"prometheus: {d}")
    return d["data"]


def labels(m: dict) -> str:
    name = m.get("__name__", "")
    rest = ", ".join(f'{k}="{v}"' for k, v in sorted(m.items()) if k != "__name__")
    return f"{name}{{{rest}}}"


def main() -> None:
    base, mode, arg = sys.argv[1], sys.argv[2], sys.argv[3]
    if mode == "alerts":
        firing = [a for a in get(base, "/api/v1/alerts", {})["alerts"]
                  if a["labels"].get("alertname") == arg and a["state"] == "firing"]
        print(len(firing))
        for a in firing:
            print(labels({k: v for k, v in a["labels"].items()}), a.get("value", ""), a.get("activeAt", ""))
        return
    res = get(base, "/api/v1/query", {"query": arg})["result"]
    if mode == "count":
        print(len(res))
    elif mode == "value":
        print(res[0]["value"][1] if res else "")
    else:
        for r in res:
            print(labels(r["metric"]), r["value"][1])


if __name__ == "__main__":
    main()

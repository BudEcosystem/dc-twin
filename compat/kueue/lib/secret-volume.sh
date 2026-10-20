# shellcheck shell=bash
# secret_volume <namespace> <secret> <dir> &: a stand-in for the kubelet's secret volume, for a
# controller run as a process instead of a pod. Kueue's (and cert-controller's) certificate
# rotation writes the webhook certificate to a Secret and expects to find it as files in its
# certificate directory, which in a cluster is that Secret mounted into the pod; this writes each
# key of the Secret to <dir>/<key> every two seconds, replacing a file only when it changed.
# Needs kc (compat/kai/lib/cluster.sh).
secret_volume() {
	local ns=$1 name=$2 dir=$3
	mkdir -p "$dir"
	while :; do
		kc get secret -n "$ns" "$name" -o json 2>/dev/null | python3 -I -c '
import base64, json, os, sys
d = sys.argv[1]
try:
    data = json.load(sys.stdin).get("data") or {}
except ValueError:
    sys.exit(0)
for k, v in data.items():
    b = base64.b64decode(v)
    p = os.path.join(d, k)
    try:
        if open(p, "rb").read() == b:
            continue
    except OSError:
        pass
    with open(p + ".tmp", "wb") as f:
        f.write(b)
    os.chmod(p + ".tmp", 0o600)
    os.replace(p + ".tmp", p)
' "$dir"
		sleep 2
	done
}

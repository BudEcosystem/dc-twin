#!/usr/bin/env bash
# A VAST cluster's management API, the twin's vast/vms dialect (docs/reviews/2026-11-08-storage-deep-dive.md
# P6), driven by VAST's own CSI driver: the published vastdataorg/csi image (pinned), unmodified, as
# the CSI controller for NFS volumes against the storage-vast estate's system, and Kubernetes'
# csi-sanity (pinned, built from source) calling it: CreateVolume, DeleteVolume,
# ValidateVolumeCapabilities, ControllerExpandVolume, CreateSnapshot, DeleteSnapshot, ListSnapshots
# and the identity service. Every volume csi-sanity makes is read back from the twin's model, and
# none is left when it is done. The node service (it mounts) is not run: at model nothing mounts.
#
#   compat/storage-vast/run.sh               # artefacts under $WORK
#   WORK=/tmp/x DCTWIN=./bin/dctwin DOCKER=podman compat/storage-vast/run.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
VAST_CSI_IMAGE=${VAST_CSI_IMAGE:-docker.io/vastdataorg/csi:v2.7.2}
CSI_TEST_VERSION=${CSI_TEST_VERSION:-v5.5.0}
WORK=${WORK:-$(mktemp -d)}
DCTWIN=${DCTWIN:-dctwin}
DOCKER=${DOCKER:-docker}
NAME=dctwin-vast-csi-$$
mkdir -p "$WORK/csi" "$WORK/vms-auth"

if [ ! -x "$WORK/csi-sanity" ]; then
  rm -rf "$WORK/csi-test"
  git clone -q --branch "$CSI_TEST_VERSION" --depth 1 https://github.com/kubernetes-csi/csi-test "$WORK/csi-test"
  (cd "$WORK/csi-test/cmd/csi-sanity" && go build -o "$WORK/csi-sanity" .)
fi
"$DOCKER" image inspect "$VAST_CSI_IMAGE" > /dev/null 2>&1 || "$DOCKER" pull -q "$VAST_CSI_IMAGE" > /dev/null

export DCTWIN_STATE="$WORK/state"
rm -rf "$DCTWIN_STATE"
"$DCTWIN" up "$root/examples/estates/storage-vast.yaml" --state "$DCTWIN_STATE" --adapters storage > "$WORK/up.log" 2>&1 &
for _ in $(seq 1 120); do [ -f "$DCTWIN_STATE/control.json" ] && break; sleep 0.25; done
[ -f "$DCTWIN_STATE/control.json" ] || { cat "$WORK/up.log"; exit 1; }
trap '"$DOCKER" rm -f $NAME >/dev/null 2>&1 || true; "$DCTWIN" down >/dev/null 2>&1 || true' EXIT
"$DCTWIN" storage credentials > "$WORK/credentials.json"
read -r api user pass < <(python3 -c 'import json,sys;c=json.load(open(sys.argv[1]))[0];print(c["api"].replace("https://",""),c["user"],c["password"])' "$WORK/credentials.json")
printf %s "$user" > "$WORK/vms-auth/username"
printf %s "$pass" > "$WORK/vms-auth/password"
rm -f "$WORK/csi/csi.sock"

# VAST's controller, as its Helm chart runs it (controller mode, NFS plugin), on this host's network
"$DOCKER" run -d --name $NAME --network host --memory 1g \
  -v "$WORK/vms-auth:/opt/vms-auth:ro" -v "$WORK/csi:/csi" \
  -e CSI_ENDPOINT=unix:///csi/csi.sock -e X_CSI_MODE=controller_and_node -e X_CSI_NODE_ID=dctwin-sanity -e X_CSI_VMS_HOST="$api" \
  -e X_CSI_DISABLE_USAGE_STATS=true -e X_CSI_ENABLE_VMS_SSL_VERIFICATION=false -e X_CSI_LOG_LEVEL=debug \
  "$VAST_CSI_IMAGE" serve --plugin nfs > /dev/null
for _ in $(seq 1 120); do [ -S "$WORK/csi/csi.sock" ] && break; sleep 0.5; done
[ -S "$WORK/csi/csi.sock" ] || { "$DOCKER" logs $NAME 2>&1 | tail -30; exit 1; }

cat > "$WORK/volume-params.yaml" <<'EOF'
root_export: /k8s
view_policy: default
vip_pool_name: vippool-1
EOF
cat > "$WORK/snapshot-params.yaml" <<'EOF'
csi.storage.k8s.io/volumesnapshot/name: sanity-snapshot
csi.storage.k8s.io/volumesnapshot/namespace: default
EOF
# Skipped, each independent of the management API: the node service and a volume's lifecycle
# (they mount; at model nothing does); ControllerPublishVolume (vast-csi's NFS plugin reads the
# volume context's root_export before it validates the volume, so csi-sanity's unknown volume
# and node give KeyError, not NotFound); an unknown source snapshot (vast-csi raises on VMS's
# 404 for it instead of answering NotFound); and a second CreateVolume of a name with another
# capacity (vast-csi raises a plain exception, gRPC Unknown, not AlreadyExists). Each was seen
# failing in a first run with only the node service and lifecycle skipped.
SKIP="Node Service|volume lifecycle|ControllerPublishVolume|source snapshot is not found|existing name and different capacity"
status=0
"$WORK/csi-sanity" --csi.endpoint="$WORK/csi/csi.sock" \
  --csi.testvolumeparameters="$WORK/volume-params.yaml" --csi.testsnapshotparameters="$WORK/snapshot-params.yaml" \
  --csi.testvolumesize=1073741824 --csi.testvolumeexpandsize=2147483648 \
  --ginkgo.focus="Controller Service|Identity Service" --ginkgo.skip="$SKIP" \
  --ginkgo.no-color > "$WORK/sanity.txt" 2>&1 || status=1
grep -E "^(Ran |SUCCESS|FAIL|PASS|Test Suite)|Passed \|" "$WORK/sanity.txt" | tail -4
grep -E "^\s*\[FAIL\]" "$WORK/sanity.txt" | head -20 || true
"$DOCKER" logs $NAME > "$WORK/vast-csi.log" 2>&1 || true
reqs=$(grep -c ">>> \[" "$WORK/vast-csi.log" || true)
echo "vast-csi made $reqs VMS requests ($(grep -o ">>> \[[A-Z]*\] https://[^ ]*/api/v1/[a-z_]*" "$WORK/vast-csi.log" | sed 's|https://[^/]*/api/v1/||' | sort | uniq -c | sort -rn | awk '{printf "%s %s %s; ", $1, $3, $4}'))"
left=$("$DCTWIN" storage get vast-1 | python3 -c 'import json,sys;v=json.load(sys.stdin);print(len(v["FileSystems"] or []), len(v["Snapshots"] or []))')
echo "left in the model after csi-sanity: file systems and snapshots $left"
[ "$left" = "0 0" ] || status=1
echo "storage-vast: $([ $status = 0 ] && echo PASS || echo FAIL) (artefacts: $WORK)"
exit $status

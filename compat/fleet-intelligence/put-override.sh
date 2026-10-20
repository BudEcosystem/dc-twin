#!/usr/bin/env bash
# put-override.sh <node>: places the override document on stdin in the fleetint guest, as
# `dctwin-mokka sync` hands it over (internal/mokka, ExecSink). The document goes where
# nvml-mock-ctl writes it, beside the engine's config.yaml in the driver tree Mokka's node agent
# staged (/var/lib/nvml-mock/driver/config), replaced atomically (a temporary file renamed over the
# old one); every loaded mock library re-reads it within its one-second TTL. Then nvml-mock-ctl
# reads it back, which fails on a document the mock would refuse. compat/gpu-operator's
# put-override.sh does the same through `kubectl exec`; here the node is a guest reached over ssh.
#
# Needs FI_GUEST_KEY and FI_GUEST_PORT (run.sh exports them). <node> is the twin's node name of the
# one host the guest stands for; any other is refused.
set -euo pipefail
node=$1
[ "$node" = "${FI_GUEST_NODE:?}" ] || { echo "the guest is $FI_GUEST_NODE, not $node" >&2; exit 1; }
ssh -q -i "${FI_GUEST_KEY:?}" -p "${FI_GUEST_PORT:?}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
	-o BatchMode=yes -o ConnectTimeout=10 debian@127.0.0.1 \
	'sudo sh -c '\''d=/var/lib/nvml-mock/driver/config; cat >"$d/.overrides.yaml.dctwin" && mv -f "$d/.overrides.yaml.dctwin" "$d/overrides.yaml" && MOCK_NVML_CONFIG=$d/config.yaml nvml-mock-ctl status >/dev/null'\'

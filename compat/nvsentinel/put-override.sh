#!/usr/bin/env bash
# put-override.sh <node>: places the Mokka override document on stdin in the GPU guest, as
# `dctwin-mokka sync` hands it over (internal/mokka, ExecSink): beside the engine's config.yaml in the
# driver tree Mokka's node agent staged (/var/lib/nvml-mock/driver/config), replaced atomically;
# every loaded mock library re-reads it within its one-second TTL. Then nvml-mock-ctl reads it back,
# which fails on a document the mock would refuse. compat/fleet-intelligence's put-override.sh does
# the same for its guest.
#
# Needs NVS_GUEST_KEY, NVS_GUEST_PORT and NVS_GUEST_NODE (run.sh exports them). <node> is the
# twin's node name of the one host the guest stands for; any other is refused.
set -euo pipefail
node=$1
[ "$node" = "${NVS_GUEST_NODE:?}" ] || { echo "the guest is $NVS_GUEST_NODE, not $node" >&2; exit 1; }
ssh -q -i "${NVS_GUEST_KEY:?}" -p "${NVS_GUEST_PORT:?}" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
	-o BatchMode=yes -o ConnectTimeout=10 debian@127.0.0.1 \
	'sudo sh -c '\''d=/var/lib/nvml-mock/driver/config; cat >"$d/.overrides.yaml.dctwin" && mv -f "$d/.overrides.yaml.dctwin" "$d/overrides.yaml" && MOCK_NVML_CONFIG=$d/config.yaml nvml-mock-ctl status >/dev/null'\'

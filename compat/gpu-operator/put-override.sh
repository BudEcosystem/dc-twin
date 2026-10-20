#!/usr/bin/env bash
# put-override.sh <node>: places the override document on stdin on a container host, as
# `dctwin-mokka sync` hands it over (internal/mokka, ExecSink). The document goes where
# nvml-mock-ctl writes it, next to the node's config.yaml in Mokka's driver tree, replaced
# atomically (a temporary file renamed over the old one), through the nvml-mock pod on that node;
# then nvml-mock-ctl reads it back, which fails on a document the mock would refuse.
#
# Needs KUBECONFIG_KIND (the scratch cluster's kubeconfig) and KUBECTL in the environment.
set -euo pipefail
node=$1
k() { "${KUBECTL:-kubectl}" --kubeconfig "$KUBECONFIG_KIND" "$@"; }
pod=$(k -n mokka get pod -l app.kubernetes.io/name=nvml-mock --field-selector "spec.nodeName=$node" \
	-o 'jsonpath={.items[0].metadata.name}')
[ -n "$pod" ] || { echo "no nvml-mock pod on node $node" >&2; exit 1; }
k -n mokka exec -i "$pod" -c node-agent -- sh -c \
	'd=/var/lib/nvml-mock/driver/config; cat >"$d/.overrides.yaml.dctwin" && mv -f "$d/.overrides.yaml.dctwin" "$d/overrides.yaml" && nvml-mock-ctl status >/dev/null'

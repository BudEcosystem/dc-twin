# shellcheck shell=bash
# A Kubernetes control plane from envtest's binaries, as ordinary processes: etcd, kube-apiserver
# and, when one is given, kube-controller-manager. No kubelet, no scheduler, no container runtime.
# Sourced by compat/kai/run.sh and compat/aicr/run.sh, after lib/common.sh (free_port).
#
# Everything is written under $1 (a scratch directory). The kubeconfig it writes is the only one
# any process here is given: nothing reads ~/.kube/config or a user's context.

# cluster_up <dir> [kube-controller-manager binary]
# Needs ENVTEST_DIR (kube-apiserver, etcd) and KUBECTL. Ports are the kernel's free ones, so a
# second twin, envtest or dev stack on this machine is never collided with.
# Exports KUBECONFIG_FILE, APISERVER_URL and CLUSTER_PIDS.
cluster_up() {
	local dir=$1 kcm=${2:-}
	local aport eport pport
	aport=$(free_port) eport=$(free_port) pport=$(free_port)
	mkdir -p "$dir/pki" "$dir/etcd" "$dir/logs"
	local token
	token=$(openssl rand -hex 24)
	# A static token in system:masters: the apiserver's admin, for this scratch cluster only.
	printf '%s,dc-twin-compat,dc-twin-compat,"system:masters"\n' "$token" >"$dir/tokens.csv"
	openssl genrsa -out "$dir/pki/sa.key" 2048 2>/dev/null
	openssl rsa -in "$dir/pki/sa.key" -pubout -out "$dir/pki/sa.pub" 2>/dev/null

	"$ENVTEST_DIR/etcd" --name default --data-dir "$dir/etcd" \
		--listen-client-urls "http://127.0.0.1:$eport" --advertise-client-urls "http://127.0.0.1:$eport" \
		--listen-peer-urls "http://127.0.0.1:$pport" --initial-advertise-peer-urls "http://127.0.0.1:$pport" \
		--initial-cluster "default=http://127.0.0.1:$pport" --unsafe-no-fsync \
		>"$dir/logs/etcd.log" 2>&1 &
	CLUSTER_PIDS="$!"
	local i
	for i in $(seq 1 60); do
		if curl -sf "http://127.0.0.1:$eport/health" >/dev/null 2>&1; then break; fi
		sleep 0.25
	done
	curl -sf "http://127.0.0.1:$eport/health" >/dev/null || { echo "etcd did not start; see $dir/logs/etcd.log" >&2; return 1; }

	# envtest's own flags (controller-runtime pkg/internal/testing/controlplane), with a token file
	# in place of its client-certificate user. The ServiceAccount admission plugin is disabled, as
	# envtest disables it: no token controller need exist for a pod to be admitted.
	"$ENVTEST_DIR/kube-apiserver" \
		--etcd-servers "http://127.0.0.1:$eport" \
		--bind-address 127.0.0.1 --secure-port "$aport" --cert-dir "$dir/pki" \
		--token-auth-file "$dir/tokens.csv" --authorization-mode RBAC --allow-privileged=true \
		--disable-admission-plugins ServiceAccount \
		--service-account-issuer "https://127.0.0.1:$aport/" \
		--service-account-key-file "$dir/pki/sa.pub" --service-account-signing-key-file "$dir/pki/sa.key" \
		--service-cluster-ip-range 10.0.0.0/24 \
		>"$dir/logs/kube-apiserver.log" 2>&1 &
	CLUSTER_PIDS="$CLUSTER_PIDS $!"

	KUBECONFIG_FILE="$dir/kubeconfig"
	cat >"$KUBECONFIG_FILE" <<KC
apiVersion: v1
kind: Config
clusters:
- name: dc-twin-compat
  cluster:
    server: https://127.0.0.1:$aport
    insecure-skip-tls-verify: true
users:
- name: dc-twin-compat
  user:
    token: $token
contexts:
- name: dc-twin-compat
  context: { cluster: dc-twin-compat, user: dc-twin-compat }
current-context: dc-twin-compat
KC
	chmod 600 "$KUBECONFIG_FILE"
	APISERVER_URL="https://127.0.0.1:$aport"
	for i in $(seq 1 120); do
		if kc get --raw /readyz >/dev/null 2>&1; then break; fi
		sleep 0.5
	done
	kc get --raw /readyz >/dev/null || { echo "kube-apiserver did not become ready; see $dir/logs" >&2; return 1; }

	if [ -n "$kcm" ]; then
		# The real node-lifecycle, taint-eviction, Job, garbage-collector controllers and the rest.
		# Leader election off (one replica), no serving port, its own credential the same token.
		"$kcm" --kubeconfig "$KUBECONFIG_FILE" --leader-elect=false --secure-port 0 \
			--service-account-private-key-file "$dir/pki/sa.key" --use-service-account-credentials=false \
			--controllers '*' \
			>"$dir/logs/kube-controller-manager.log" 2>&1 &
		CLUSTER_PIDS="$CLUSTER_PIDS $!"
	fi
	export KUBECONFIG_FILE APISERVER_URL CLUSTER_PIDS
}

# kc runs kubectl against the scratch cluster only: an explicit kubeconfig and cache directory.
kc() {
	"$KUBECTL" --kubeconfig "$KUBECONFIG_FILE" --cache-dir "$(dirname "$KUBECONFIG_FILE")/kcache" "$@"
}

cluster_down() {
	# Stop in reverse order (controller-manager, apiserver, then etcd): an apiserver whose etcd
	# went first waits on it instead of exiting. Anything still up after 10 s is killed.
	local p pids="" i
	for p in $CLUSTER_PIDS; do pids="$p $pids"; done
	for p in $pids; do
		kill "$p" 2>/dev/null || continue
		for i in $(seq 1 40); do kill -0 "$p" 2>/dev/null || break; sleep 0.25; done
		kill -9 "$p" 2>/dev/null || true
		wait "$p" 2>/dev/null || true
	done
}

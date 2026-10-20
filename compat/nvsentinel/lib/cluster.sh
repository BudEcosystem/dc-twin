# shellcheck shell=bash
# The cluster for compat/nvsentinel: envtest's etcd and kube-apiserver as ordinary processes on
# this host, and the real kube-controller-manager, as compat/kai/lib/cluster.sh runs them, except
# that the apiserver serves a certificate from a CA of the harness's own: NVSentinel's modules run
# in containers as their pods would, with an in-cluster ServiceAccount directory whose ca.crt is
# that CA, and verify the apiserver by it. Sourced after compat/kai/lib/common.sh (free_port).
#
# Everything is written under $1. Nothing reads ~/.kube/config.

# nvs_cluster_up <dir> <address containers reach this host by> [kube-controller-manager]
# Exports KUBECONFIG_FILE (for this host), KUBECONFIG_CONTAINER (the same cluster at that address),
# APISERVER_PORT, ADMIN_TOKEN, CLUSTER_PIDS and CLUSTER_CA.
nvs_cluster_up() {
	local dir=$1 hostaddr=$2 kcm=${3:-}
	local aport eport pport i
	aport=$(free_port) eport=$(free_port) pport=$(free_port)
	mkdir -p "$dir/pki" "$dir/etcd" "$dir/logs"
	ADMIN_TOKEN=$(openssl rand -hex 24)
	printf '%s,dc-twin-compat,dc-twin-compat,"system:masters"\n' "$ADMIN_TOKEN" >"$dir/tokens.csv"
	openssl genrsa -out "$dir/pki/sa.key" 2048 2>/dev/null
	openssl rsa -in "$dir/pki/sa.key" -pubout -out "$dir/pki/sa.pub" 2>/dev/null
	# a CA and a serving certificate for every name the apiserver is reached by
	openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=dc-twin-compat-ca" \
		-keyout "$dir/pki/ca.key" -out "$dir/pki/ca.crt" 2>/dev/null
	printf 'subjectAltName=IP:127.0.0.1,DNS:localhost,IP:%s,IP:10.0.2.2,DNS:host.docker.internal\nextendedKeyUsage=serverAuth\n' "$hostaddr" >"$dir/pki/san.ext"
	openssl req -newkey rsa:2048 -nodes -subj "/CN=kube-apiserver" -keyout "$dir/pki/apiserver.key" -out "$dir/pki/apiserver.csr" 2>/dev/null
	openssl x509 -req -in "$dir/pki/apiserver.csr" -CA "$dir/pki/ca.crt" -CAkey "$dir/pki/ca.key" -CAcreateserial \
		-days 2 -extfile "$dir/pki/san.ext" -out "$dir/pki/apiserver.crt" 2>/dev/null
	CLUSTER_CA="$dir/pki/ca.crt"

	"$ENVTEST_DIR/etcd" --name default --data-dir "$dir/etcd" \
		--listen-client-urls "http://127.0.0.1:$eport" --advertise-client-urls "http://127.0.0.1:$eport" \
		--listen-peer-urls "http://127.0.0.1:$pport" --initial-advertise-peer-urls "http://127.0.0.1:$pport" \
		--initial-cluster "default=http://127.0.0.1:$pport" --unsafe-no-fsync \
		>"$dir/logs/etcd.log" 2>&1 &
	CLUSTER_PIDS="$!"
	for i in $(seq 1 60); do
		curl -sf "http://127.0.0.1:$eport/health" >/dev/null 2>&1 && break
		sleep 0.25
	done
	"$ENVTEST_DIR/kube-apiserver" \
		--etcd-servers "http://127.0.0.1:$eport" \
		--bind-address 127.0.0.1 --secure-port "$aport" \
		--tls-cert-file "$dir/pki/apiserver.crt" --tls-private-key-file "$dir/pki/apiserver.key" \
		--token-auth-file "$dir/tokens.csv" --authorization-mode RBAC --allow-privileged=true \
		--disable-admission-plugins ServiceAccount \
		--service-account-issuer "https://127.0.0.1:$aport/" \
		--service-account-key-file "$dir/pki/sa.pub" --service-account-signing-key-file "$dir/pki/sa.key" \
		--service-cluster-ip-range 10.0.0.0/24 \
		>"$dir/logs/kube-apiserver.log" 2>&1 &
	CLUSTER_PIDS="$CLUSTER_PIDS $!"
	APISERVER_PORT=$aport
	KUBECONFIG_FILE="$dir/kubeconfig"
	KUBECONFIG_CONTAINER="$dir/kubeconfig.container"
	local f server
	for f in "$KUBECONFIG_FILE:127.0.0.1" "$KUBECONFIG_CONTAINER:$hostaddr"; do
		server=${f#*:}
		cat >"${f%%:*}" <<KC
apiVersion: v1
kind: Config
clusters:
- name: dc-twin-compat
  cluster:
    server: https://$server:$aport
    certificate-authority-data: $(base64 <"$CLUSTER_CA" | tr -d '\n')
users:
- name: dc-twin-compat
  user:
    token: $ADMIN_TOKEN
contexts:
- name: dc-twin-compat
  context: { cluster: dc-twin-compat, user: dc-twin-compat }
current-context: dc-twin-compat
KC
		chmod 600 "${f%%:*}"
	done
	for i in $(seq 1 120); do
		kc get --raw /readyz >/dev/null 2>&1 && break
		sleep 0.5
	done
	kc get --raw /readyz >/dev/null || { echo "kube-apiserver did not become ready; see $dir/logs" >&2; return 1; }
	if [ -n "$kcm" ]; then
		"$kcm" --kubeconfig "$KUBECONFIG_FILE" --leader-elect=false --secure-port 0 \
			--service-account-private-key-file "$dir/pki/sa.key" --use-service-account-credentials=false \
			--controllers '*' >"$dir/logs/kube-controller-manager.log" 2>&1 &
		CLUSTER_PIDS="$CLUSTER_PIDS $!"
	fi
	export KUBECONFIG_FILE KUBECONFIG_CONTAINER APISERVER_PORT ADMIN_TOKEN CLUSTER_PIDS CLUSTER_CA
}

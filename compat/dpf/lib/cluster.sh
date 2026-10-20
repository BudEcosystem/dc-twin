# shellcheck shell=bash
# Kubernetes control planes for compat/dpf: envtest's etcd and kube-apiserver as ordinary
# processes, as compat/kai/lib/cluster.sh starts them, with two differences DPF needs:
#
#   - the apiserver serves a certificate from a CA of this run, and the kubeconfig carries that
#     CA: DPF computes a DPU cluster's `kubeadm join --discovery-token-ca-cert-hash` from the CA
#     in the cluster's kubeconfig, and refuses a kubeconfig that has none.
#   - its admin kubeconfig authenticates with a client certificate of that CA in system:masters,
#     as kubeadm's super-admin.conf does: DPF hands ArgoCD a DPU cluster's credentials as the
#     kubeconfig's client certificate and key (argocd.NewArgoCDClusterSecret), never a token.
#   - it takes bootstrap tokens and client certificates from that CA, as a kubeadm-made control
#     plane does: DPF's DPU agent authenticates to the host cluster with a bootstrap token, and a
#     DPU's kubelet joins the DPU cluster with one (`kubeadm join`) and then with the client
#     certificate the cluster's controller manager signs for it.
#
# With a site address (a stack service the twin routes, DPU=vm), the certificate names it too and
# <dir>/kubeconfig-site addresses the apiserver there: what DPF hands a DPU, and what DPF itself
# uses through the twin's site gateway, so DPF and the DPU name the cluster by one address.
#
# Everything is under $1 (a scratch directory); nothing reads ~/.kube/config.

# dpf_cluster_up <dir> <name> [kube-controller-manager binary] [site address ip:port]
# Needs ENVTEST_DIR. Sets <NAME>_KUBECONFIG and <NAME>_PORT (upper-case name) and adds <dir> to
# CLUSTER_DIRS (dpf_clusters_down stops them). The controller manager, when given, runs every controller including the bootstrap
# token signer and cleaner, and signs kubelet certificates with the cluster's CA. The cluster's
# ports, certificates and etcd data stay in <dir>, so dpf_cluster_stop and dpf_cluster_start stop
# it and bring the same cluster back (DPU=vm runs the BFB installer with the clusters down).
dpf_cluster_up() {
	local dir=$1 name=$2 kcm=${3:-} site=${4:-}
	local aport eport pport token
	aport=$(free_port) eport=$(free_port) pport=$(free_port)
	mkdir -p "$dir/pki" "$dir/etcd" "$dir/logs"
	token=$(openssl rand -hex 24)
	printf '%s,dc-twin-compat,dc-twin-compat,"system:masters"\n' "$token" >"$dir/tokens.csv"
	openssl genrsa -out "$dir/pki/sa.key" 2048 2>/dev/null
	openssl rsa -in "$dir/pki/sa.key" -pubout -out "$dir/pki/sa.pub" 2>/dev/null
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 \
		-subj "/CN=dc-twin compat-dpf $name CA" -keyout "$dir/pki/ca.key" -out "$dir/pki/ca.crt" 2>/dev/null
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj "/CN=kube-apiserver" \
		-keyout "$dir/pki/apiserver.key" -out "$dir/pki/apiserver.csr" 2>/dev/null
	local san="IP:127.0.0.1,DNS:localhost,DNS:kubernetes,DNS:kubernetes.default"
	[ -z "$site" ] || san="$san,IP:${site%%:*}"
	printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\n' "$san" >"$dir/pki/apiserver.ext"
	openssl x509 -req -in "$dir/pki/apiserver.csr" -CA "$dir/pki/ca.crt" -CAkey "$dir/pki/ca.key" -CAcreateserial \
		-days 2 -extfile "$dir/pki/apiserver.ext" -out "$dir/pki/apiserver.crt" 2>/dev/null
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj "/O=system:masters/CN=kubernetes-super-admin" \
		-keyout "$dir/pki/admin.key" -out "$dir/pki/admin.csr" 2>/dev/null
	printf 'extendedKeyUsage=clientAuth\n' >"$dir/pki/admin.ext"
	openssl x509 -req -in "$dir/pki/admin.csr" -CA "$dir/pki/ca.crt" -CAkey "$dir/pki/ca.key" -CAcreateserial \
		-days 2 -extfile "$dir/pki/admin.ext" -out "$dir/pki/admin.crt" 2>/dev/null
	local ca cert key
	ca=$(base64 <"$dir/pki/ca.crt" | tr -d '\n')
	cert=$(base64 <"$dir/pki/admin.crt" | tr -d '\n')
	key=$(base64 <"$dir/pki/admin.key" | tr -d '\n')
	cat >"$dir/kubeconfig" <<KC
apiVersion: v1
kind: Config
clusters:
- name: $name
  cluster:
    server: https://127.0.0.1:$aport
    certificate-authority-data: $ca
users:
- name: $name-admin
  user:
    client-certificate-data: $cert
    client-key-data: $key
contexts:
- name: $name
  context: { cluster: $name, user: $name-admin }
current-context: $name
KC
	chmod 600 "$dir/kubeconfig"
	if [ -n "$site" ]; then
		sed "s|server: https://127.0.0.1:$aport|server: https://$site|" "$dir/kubeconfig" >"$dir/kubeconfig-site"
		chmod 600 "$dir/kubeconfig-site"
	fi
	printf 'name=%s\naport=%s\neport=%s\npport=%s\nkcm=%s\n' "$name" "$aport" "$eport" "$pport" "$kcm" >"$dir/cluster.env"
	dpf_cluster_start "$dir"
}

# dpf_cluster_start <dir>: etcd, kube-apiserver and (when the cluster has one) its controller
# manager, from what dpf_cluster_up left in <dir>.
dpf_cluster_start() {
	local dir=$1 name aport eport pport kcm var i
	# shellcheck disable=SC1091
	. "$dir/cluster.env"
	: >"$dir/pids"
	"$ENVTEST_DIR/etcd" --name default --data-dir "$dir/etcd" \
		--listen-client-urls "http://127.0.0.1:$eport" --advertise-client-urls "http://127.0.0.1:$eport" \
		--listen-peer-urls "http://127.0.0.1:$pport" --initial-advertise-peer-urls "http://127.0.0.1:$pport" \
		--initial-cluster "default=http://127.0.0.1:$pport" --unsafe-no-fsync \
		>>"$dir/logs/etcd.log" 2>&1 &
	echo $! >>"$dir/pids"
	for i in $(seq 1 60); do
		curl -sf "http://127.0.0.1:$eport/health" >/dev/null 2>&1 && break
		sleep 0.25
	done
	# envtest's flags (compat/kai/lib/cluster.sh), with this run's serving certificate, on loopback
	"$ENVTEST_DIR/kube-apiserver" \
		--etcd-servers "http://127.0.0.1:$eport" \
		--bind-address 127.0.0.1 --secure-port "$aport" \
		--tls-cert-file "$dir/pki/apiserver.crt" --tls-private-key-file "$dir/pki/apiserver.key" \
		--token-auth-file "$dir/tokens.csv" --authorization-mode Node,RBAC --allow-privileged=true \
		--client-ca-file "$dir/pki/ca.crt" --enable-bootstrap-token-auth \
		--disable-admission-plugins ServiceAccount \
		--service-account-issuer "https://127.0.0.1:$aport/" \
		--service-account-key-file "$dir/pki/sa.pub" --service-account-signing-key-file "$dir/pki/sa.key" \
		--service-cluster-ip-range 10.0.0.0/24 \
		>>"$dir/logs/kube-apiserver.log" 2>&1 &
	echo $! >>"$dir/pids"
	for i in $(seq 1 120); do
		"$KUBECTL" --kubeconfig "$dir/kubeconfig" get --raw /readyz >/dev/null 2>&1 && break
		sleep 0.5
	done
	"$KUBECTL" --kubeconfig "$dir/kubeconfig" get --raw /readyz >/dev/null || { echo "kube-apiserver ($name) did not become ready; see $dir/logs" >&2; return 1; }
	if [ -n "$kcm" ]; then
		"$kcm" --kubeconfig "$dir/kubeconfig" --leader-elect=false --secure-port 0 \
			--service-account-private-key-file "$dir/pki/sa.key" --use-service-account-credentials=false \
			--root-ca-file "$dir/pki/ca.crt" --cluster-signing-cert-file "$dir/pki/ca.crt" --cluster-signing-key-file "$dir/pki/ca.key" \
			--controllers '*,bootstrapsigner,tokencleaner' >>"$dir/logs/kube-controller-manager.log" 2>&1 &
		echo $! >>"$dir/pids"
	fi
	CLUSTER_DIRS="${CLUSTER_DIRS:-} $dir"
	var=$(echo "$name" | tr '[:lower:]-' '[:upper:]_')
	eval "${var}_KUBECONFIG=\$dir/kubeconfig ${var}_PORT=\$aport"
	export CLUSTER_DIRS
}

# dpf_cluster_stop <dir>: the cluster's processes, newest first; its data stays.
dpf_cluster_stop() {
	local dir=$1 p pids="" i
	[ -f "$dir/pids" ] || return 0
	for p in $(cat "$dir/pids"); do pids="$p $pids"; done
	for p in $pids; do
		kill "$p" 2>/dev/null || continue
		for i in $(seq 1 40); do kill -0 "$p" 2>/dev/null || break; sleep 0.25; done
		kill -9 "$p" 2>/dev/null || true
		wait "$p" 2>/dev/null || true
	done
	: >"$dir/pids"
}

dpf_clusters_down() {
	local d
	for d in ${CLUSTER_DIRS:-}; do dpf_cluster_stop "$d"; done
	CLUSTER_DIRS=
}

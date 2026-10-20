#!/usr/bin/env bash
# Runs inside the control-plane guest (see ../controlplane.sh): installs k3s, then Run:ai 3.0.20's
# control plane from its published chart and images (helm.ngc.nvidia.com/nvidia/runai-oss and
# nvcr.io/nvidia/runai-oss, both anonymous), the way Run:ai's own local-dev.sh does on Kind: a
# self-signed CA and certificate for the domain, the runai-backend-tls and runai-ca-cert secrets,
# the domain resolved to the ingress inside and outside the cluster. Then it waits for the
# control plane, registers a cluster through its API and writes the cluster's credentials.
#
#   install-cp.sh <out dir>      writes <out>/cp.env (URL, admin, cluster UUID, credentials)
set -euo pipefail
OUT=${1:?out dir}
mkdir -p "$OUT"
DOMAIN=${DOMAIN:-runai.dctwin.test}
RUNAI_REF=${RUNAI_REF:-3.0.20}
K3S_VERSION=${K3S_VERSION:-v1.35.9+k3s1}   # Run:ai 3.0 supports upstream Kubernetes 1.35-1.37
ADMIN_EMAIL=${ADMIN_EMAIL:-test@run.ai}
CLUSTER_NAME=${CLUSTER_NAME:-dctwin}
here=$(cd "$(dirname "$0")" && pwd)
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
log() { printf '%s %s\n' "$(date +%T)" "$*" >&2; }

# ---------------------------------------------------------------- k3s
if ! command -v k3s >/dev/null; then
	log "installing k3s $K3S_VERSION"
	curl -sfL https://get.k3s.io | sudo INSTALL_K3S_VERSION="$K3S_VERSION" \
		INSTALL_K3S_EXEC="--disable metrics-server --write-kubeconfig-mode 644" sh - >"$OUT/k3s-install.log" 2>&1
fi
for _ in $(seq 1 120); do kubectl get --raw /readyz >/dev/null 2>&1 && break; sleep 2; done
for _ in $(seq 1 120); do kubectl -n kube-system get svc traefik >/dev/null 2>&1 && break; sleep 5; done
NODE_IP=$(kubectl get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')

# ---------------------------------------------------------------- TLS and names
C=$OUT/certs
mkdir -p "$C"
if [ ! -f "$C/tls.crt" ]; then
	openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=dctwin-runai-ca" -keyout "$C/ca.key" -out "$C/ca.crt" 2>/dev/null
	openssl req -newkey rsa:2048 -nodes -subj "/CN=$DOMAIN" -keyout "$C/tls.key" -out "$C/tls.csr" 2>/dev/null
	printf 'subjectAltName=DNS:%s,DNS:*.%s\n' "$DOMAIN" "$DOMAIN" >"$C/san"
	openssl x509 -req -in "$C/tls.csr" -CA "$C/ca.crt" -CAkey "$C/ca.key" -CAcreateserial -days 30 -extfile "$C/san" -out "$C/tls.crt" 2>/dev/null
	sudo cp "$C/ca.crt" /usr/local/share/ca-certificates/dctwin-runai-ca.crt
	sudo update-ca-certificates >/dev/null 2>&1
fi
# The guest resolves the domain to its own address, where k3s's servicelb exposes Traefik (443);
# pods resolve it to Traefik's ClusterIP (CoreDNS, as local-dev.sh does for ingress-nginx).
grep -q " $DOMAIN\$" /etc/hosts || echo "$NODE_IP $DOMAIN" | sudo tee -a /etc/hosts >/dev/null
TRAEFIK_IP=$(kubectl -n kube-system get svc traefik -o jsonpath='{.spec.clusterIP}')
kubectl -n kube-system apply -f - >/dev/null <<CM
apiVersion: v1
kind: ConfigMap
metadata: { name: coredns-custom, namespace: kube-system }
data:
  runai.server: |
    $DOMAIN:53 {
        hosts {
            $TRAEFIK_IP $DOMAIN
            fallthrough
        }
        cache 30
    }
CM
kubectl -n kube-system rollout restart deployment/coredns >/dev/null
kubectl create namespace runai-backend --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n runai-backend create secret tls runai-backend-tls --cert="$C/tls.crt" --key="$C/tls.key" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n runai-backend create secret generic runai-ca-cert --from-file=runai-ca.pem="$C/ca.crt" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# ---------------------------------------------------------------- the control plane
CHART=$OUT/control-plane-$RUNAI_REF.tgz
[ -s "$CHART" ] || curl -fsSL -o "$CHART" "https://helm.ngc.nvidia.com/nvidia/runai-oss/charts/control-plane-$RUNAI_REF.tgz"
# The chart requires a special character in the admin password.
[ -s "$OUT/admin-password" ] || printf '%s' "$(openssl rand -hex 10)-Aa1" >"$OUT/admin-password"
ADMIN_PASSWORD=$(cat "$OUT/admin-password")
log "installing the control plane ($CHART)"
t0=$(date +%s)
helm upgrade --install runai-backend "$CHART" -n runai-backend -f "$here/values-cp.yaml" \
	--set global.domain="$DOMAIN" --set global.management.user="$ADMIN_EMAIL" \
	--set tenantsManager.config.adminUsername="$ADMIN_EMAIL" \
	--set-string global.management.password="$ADMIN_PASSWORD" --set-string tenantsManager.config.adminPassword="$ADMIN_PASSWORD" \
	--timeout 40m >"$OUT/helm-install.log" 2>&1 || { log "helm install failed; see $OUT/helm-install.log"; tail -20 "$OUT/helm-install.log" >&2; }
log "helm returned after $(( $(date +%s) - t0 )) s"

token() {
	curl -sf --max-time 60 -X POST "https://$DOMAIN/auth/realms/runai/protocol/openid-connect/token" \
		--data-urlencode grant_type=password --data-urlencode client_id=runai --data-urlencode "username=$ADMIN_EMAIL" \
		--data-urlencode "password=$ADMIN_PASSWORD" --data-urlencode scope=openid | jq -r .access_token
}
ready=0
for i in $(seq 1 90); do
	if T=$(token) && [ -n "$T" ] && [ "$T" != null ] && curl -sf --max-time 30 -H "Authorization: Bearer $T" "https://$DOMAIN/_version" >"$OUT/version.json"; then
		ready=1
		break
	fi
	sleep 10
done
[ "$ready" = 1 ] || { log "the control plane did not answer /_version"; exit 1; }
log "control plane healthy $(( $(date +%s) - t0 )) s after install began: $(cat "$OUT/version.json")"

# ---------------------------------------------------------------- the cluster, through Run:ai's API
# The cluster service can answer later than /_version; look the name up, else register it, until one works.
UUID=""
for _ in $(seq 1 36); do
	T=$(token)
	UUID=$(curl -sf -H "Authorization: Bearer $T" "https://$DOMAIN/v1/k8s/clusters" | jq -r --arg n "$CLUSTER_NAME" '.[] | select(.name == $n) | .uuid' | head -1) || UUID=""
	[ -n "$UUID" ] && break
	UUID=$(curl -sf -X POST -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
		-d "{\"name\": \"$CLUSTER_NAME\"}" "https://$DOMAIN/v1/k8s/clusters" | jq -r .uuid) && [ -n "$UUID" ] && [ "$UUID" != null ] && break
	UUID=""
	sleep 5
done
[ -n "$UUID" ] || { log "could not register cluster $CLUSTER_NAME"; exit 1; }
INSTALLER_SECRET=""
for _ in $(seq 1 30); do
	T=$(token)
	INSTALLER_SECRET=$(curl -sf -H "Authorization: Bearer $T" "https://$DOMAIN/api/v1/service-accounts/installer" | jq -r .clientSecret) &&
		[ -n "$INSTALLER_SECRET" ] && [ "$INSTALLER_SECRET" != null ] && break
	sleep 5
done
cat >"$OUT/cp.env" <<ENV
CP_URL=https://$DOMAIN
CP_DOMAIN=$DOMAIN
CP_ADMIN_EMAIL=$ADMIN_EMAIL
CP_ADMIN_PASSWORD=$ADMIN_PASSWORD
CP_CLUSTER_NAME=$CLUSTER_NAME
CP_CLUSTER_UUID=$UUID
CP_INSTALLER_CLIENT_ID=runai-internal.installer
CP_INSTALLER_SECRET=$INSTALLER_SECRET
CP_CA=$C/ca.crt
ENV
chmod 600 "$OUT/cp.env"
log "cluster $CLUSTER_NAME registered: $UUID"

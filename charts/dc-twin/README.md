# dc-twin Helm chart

Runs one dc-twin estate per release, in one pod: the engine, every adapter, the console and the
site gateway. The twin runs on the loopback substrate ([D34](../../docs/spec/11-decisions.md)):
each device endpoint is a port on the pod's loopback, and software under test reaches a device
at its site address through the **site gateway**, an HTTP CONNECT proxy exposed as a Service. The
pod runs unprivileged, as a non-root user with a read-only root filesystem.

## Install

The chart runs the image the repository's `Dockerfile` builds. No image is published yet, so
build it where your cluster can pull it:

```bash
make image                                   # ghcr.io/budecosystem/dc-twin:<appVersion>
```

That is enough for a cluster that runs on the same Docker Engine (Docker Desktop, Colima, k3s
with Docker). Load it into kind with `kind load docker-image ghcr.io/budecosystem/dc-twin:0.1.0`.
For any other cluster, push it to a registry and set `image.repository`:

```bash
docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/dc-twin:0.1.0 --push .
```

Install it:

```bash
helm install dc-twin charts/dc-twin -n dc-twin --create-namespace
kubectl -n dc-twin rollout status deploy/dc-twin
```

The first start fetches the packs' assets (about 360 MB) in an init container: the captured BMC
trees, NVIDIA's NMX-C interface definition and the `os: twin` guest image. The image does not
carry them, because some may not be redistributed ([D13](../../docs/spec/11-decisions.md)). The
init container needs egress to `github.com`, `codeload.github.com`, `raw.githubusercontent.com`
and `dl-cdn.alpinelinux.org`. The assets are kept on a persistent volume, so a later pod starts
in seconds.

## Open the console

The estate token is in a Secret the chart makes at install and keeps across upgrades, so a
bookmarked address keeps working (`control.token.existingSecret` names one of yours instead):

```bash
export DCTWIN_TOKEN=$(kubectl -n dc-twin get secret dc-twin-token -o jsonpath='{.data.token}' | base64 -d)
```

By default the control API and the console listen on the pod's loopback only
([15](../../docs/spec/15-security.md)). Reach them through a port-forward, then open
`http://127.0.0.1:8765/#token=$DCTWIN_TOKEN`:

```bash
kubectl -n dc-twin port-forward deploy/dc-twin 8765
```

Every `dctwin` verb runs in the pod, for example `dctwin status`, `dctwin fault inject …`,
`dctwin events --follow` or `dctwin report`:

```bash
kubectl -n dc-twin exec deploy/dc-twin -- dctwin status
```

### At a URL, through an Ingress

```yaml
ingress:
  enabled: true
  className: nginx
  hosts:
    - host: dc-twin.example.com
      paths: [{ path: /, pathType: Prefix }]
  tls:
    - secretName: dc-twin-tls
      hosts: [dc-twin.example.com]
networkPolicy:
  enabled: true
  controlFrom:
    - namespaceSelector:
        matchLabels: { kubernetes.io/metadata.name: ingress-nginx }
```

Open `https://dc-twin.example.com/#token=$DCTWIN_TOKEN`, or open the URL and paste the token. The
token after the `#` never reaches the server, so no proxy logs it, and the console sends it only
in a header. An Ingress (or `control.service.enabled`) makes the twin listen on the pod's address
too (`--allow-remote-control`, [D54](../../docs/spec/11-decisions.md)), and **the token is then the
only authentication**: whoever has it can inject faults, power devices off and apply a changed
estate. So:

- **Serve it over TLS** (`ingress.tls`), and only over TLS: with Traefik,
  `traefik.ingress.kubernetes.io/router.entrypoints: websecure` in `ingress.annotations` leaves
  plain HTTP unrouted.
- **Enable the NetworkPolicy**, with `controlFrom` naming your Ingress controller's pods. Otherwise
  any pod in the cluster, the software under test among them, can reach the control port. The
  site gateway stays open to the cluster.
- **Put a login or an allow-list in front** where more than a trusted team can reach the URL, with
  your Ingress controller's annotations (`ingress.annotations`; oauth2-proxy and
  `whitelist-source-range` for ingress-nginx are in [values.yaml](values.yaml)).

The event stream sends `X-Accel-Buffering: no` and a keep-alive every 20 seconds, so a proxy
neither buffers it nor closes it when the twin is quiet.

## Point software under test at the twin

Software in the cluster reaches the devices through the gateway Service, at the site addresses
and host names a site would hand it:

```bash
kubectl -n dc-twin exec deploy/dc-twin -- dctwin credentials    # each BMC's site address and login
```

- **An HTTP client** (a lifecycle manager, Ironic, a Redfish library) takes the gateway as its
  proxy: `HTTPS_PROXY=http://dc-twin-gateway.dc-twin.svc:3128`, then `https://10.30.0.11/redfish/v1`.
- **Prometheus** scrapes the twin's exporters with `proxy_url: http://dc-twin-gateway.dc-twin.svc:3128`
  on the scrape job ([compat/prometheus](../../compat/prometheus/prometheus.yml)).

The gateway routes only to estate endpoints and the stack's `siteServices`. It never routes to
the control API.

## Choose the estate

The twin runs the first of these that is set:

| Value | The estate |
|---|---|
| `estate.existingConfigMap` | Your ConfigMap, the file under `estate.key` (default `estate.yaml`). Changing its contents does not restart the pod: `kubectl rollout restart` |
| `estate.file` | The file itself: `--set-file estate.file=my-hall.yaml`. A changed file starts a new pod on it |
| `estate.example` | An estate shipped in the image, `examples/estates/<example>.yaml`. Default `mixed-hall` |

A new pod is a new twin, started from the chart's estate. An **Apply** from the console's Design
restarts the estate inside the pod and lasts until the pod restarts, so **Export** a design you
want to keep and install it with `estate.file`.

## Values

| Value | Default | |
|---|---|---|
| `image.repository`, `image.tag` | `ghcr.io/budecosystem/dc-twin`, the appVersion | |
| `adapters` | `[]` (every adapter) | `dctwin up --adapters`, e.g. `[redfish, bms]` |
| `console` | `true` | Serve the console beside the control API |
| `control.port` | `8765` | The control API's and the console's port |
| `control.token.existingSecret`, `.key` | the chart's Secret, `token` | The estate token. Use your own Secret with a tool that renders the chart offline (Argo CD), which cannot keep the chart's |
| `control.service.enabled`, `.type`, `.port` | `false`, `ClusterIP`, `8765` | A Service for the control API (beyond loopback, D54) |
| `ingress.enabled`, `.className`, `.annotations`, `.hosts`, `.tls` | `false` | The console and the control API at a URL (beyond loopback, D54) |
| `networkPolicy.enabled`, `.controlFrom` | `false`, `[]` | The gateway open to the cluster, the control port only from `controlFrom` |
| `gateway.port`, `gateway.service.type`, `gateway.service.port` | `3128`, `ClusterIP`, `3128` | `NodePort` or `LoadBalancer` reach it from outside the cluster |
| `assets.fetch` | `true` | Run `dctwin assets fetch` in an init container |
| `assets.persistence.enabled`, `.size`, `.storageClass`, `.existingClaim` | `true`, `1Gi`, default class | Where the assets are kept; disabled, they are fetched on every start |
| `state.sizeLimit` | `2Gi` | The state directory's emptyDir: the event log and an apply's file |
| `allowTCG`, `extraArgs`, `extraEnv` | | More for `dctwin up` |
| `resources` | requests 100m, 256Mi | `mixed-hall` (354 devices) idles at about 140 MiB |
| `startupProbe`, `readinessProbe`, `livenessProbe` | | Each runs `dctwin status` in the container |

The pod and container security contexts, the service account (it mounts no token: the twin calls
no Kubernetes API), node selector, tolerations and affinity are in [values.yaml](values.yaml).

## What it does not run

- **`vm` devices, the TAP substrate and real NOSes.** They need `/dev/kvm` and QEMU, root, and a
  container engine. `up` refuses an estate with `vm` devices and says why; run those on a Linux
  host ([09](../../docs/spec/09-runtime.md)).
- **IPMI, SNMP and DHCP from outside the pod.** They are UDP, and the gateway proxies TCP. Their
  endpoints answer inside the pod (`kubectl exec … -- dctwin endpoints`).
- **Users and roles.** A console at a URL has one shared token until the control API has OIDC
  ([15 — Users and the control API](../../docs/spec/15-security.md)).
- **Two pods of one twin.** One engine per estate: the Deployment has one replica and replaces
  its pod with `Recreate`.

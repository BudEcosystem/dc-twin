# dc-twin Helm chart

Runs one dc-twin estate per release, in one pod: the engine, every adapter, the console and the
site gateway. By default the twin runs on the loopback substrate
([D34](../../docs/spec/11-decisions.md)): each device endpoint is a port on the pod's loopback,
and software under test reaches a device at its site address through the **site gateway**, an
HTTP CONNECT proxy exposed as a Service. The pod runs unprivileged, as a non-root user with a
read-only root filesystem. For real switch operating systems and day 0, see
[The tap substrate](#the-tap-substrate-real-switches-and-day-0).

## Install

The chart runs the image the repository's `Dockerfile` builds, which `.github/workflows/images.yml`
publishes to `registry.bud.studio/gpu-foundry/dc-twin` for amd64 and arm64: the chart's
`appVersion` (its default tag) once, and every commit on `main` as `sha-<commit>` (set `image.tag`
to pin one). The project is private, so give the cluster a pull secret for an account that can
read it, and name it in `imagePullSecrets`:

```bash
kubectl -n dc-twin create secret docker-registry bud-registry --docker-server=registry.bud.studio \
  --docker-username='<robot account>' --docker-password='<its secret>'
helm install dc-twin charts/dc-twin -n dc-twin --set 'imagePullSecrets[0].name=bud-registry'
```

Before the workflow has published it, or to run your own change, build it where your cluster can
pull it:

```bash
make image                                   # registry.bud.studio/gpu-foundry/dc-twin:<appVersion>
```

That is enough for a cluster that runs on the same Docker Engine (Docker Desktop, Colima, k3s
with Docker). Load it into kind with `kind load docker-image registry.bud.studio/gpu-foundry/dc-twin:0.1.0`.
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

## The tap substrate: real switches and day 0

An estate whose fabrics run a real `nos`, or are brought up by the software under test
(`fabric.bringUp: factory`, [18](../../docs/spec/18-day0.md)), needs the tap substrate
(`spec.adapters.network.substrate: tap`): namespaces, TAPs and bridges, and the switches as
containers. `substrate: tap` runs it in the pod
([D82](../../docs/spec/11-decisions.md#d82--the-chart-runs-the-tap-substrate-in-a-privileged-pod-with-its-engine-beside-the-twin)):

- **The pod is privileged**, and the twin and its engine run as root. Their network is the pod's
  own, but the kernel is the node's: give the twin a node you would give a container host.
- **An engine sidecar** (Podman's Docker-compatible API) runs the switches. It shares the pod's
  process namespace, so the twin opens each switch's network namespace itself, as on a host. Its
  image store is a volume (`tap.engine.persistence`), so switch images are pulled once.
- **The twin's switch images** are pulled from `tap.registry`, logging in with
  `tap.registryAuthSecret` (a `kubernetes.io/dockerconfigjson` Secret; by default the first of
  `imagePullSecrets`).
- **The day-0 programs** (`dctwin-day0`) are copied out of the twin's image by an init container,
  so an estate leaves `spec.adapters.network.nos.day0Binaries` unset.
- **If the engine restarts on its own**, the switches and tooling it ran are gone with it, so once
  it is back it stops the twin, and the pod starts the twin again on a fresh estate. On a new pod
  the engine first removes what an earlier pod left in its store.
- **The site's day-0 servers** are containers the engine starts before the twin
  (`tap.containers`); the estate's `spec.adapters.network.attach` cables each to a switch port by
  its `container` name. A DHCP and ZTP server has to sit on the switches' management network, so it
  runs here, not as a pod of its own.
- **Software in the cluster reaches the site's networks through the site gateway** when one
  attach is the estate's `siteGateway`: a CONNECT to an address in that host's subnet or its
  `routes` that no estate endpoint answers is dialled from the host's network namespace, as a
  management server on the OOB network reaches it. So a switch's SSH, NVUE and gNMI at its lease
  or its loopback, and a tool's API in a slot (Kea's control agent), are reached as a BMC's Redfish
  is: `CONNECT 10.30.0.21:22` to `dc-twin-gateway:3128`. Anything off those networks is still
  refused.

```bash
helm install day0 charts/dc-twin -n dc-twin-day0 \
  --set substrate=tap \
  --set imagePullSecrets[0].name=bud-registry \
  --set-file estate.file=my-day0.yaml \
  -f day0-containers.yaml   # tap.containers: the attach targets
```

### The site's day-0 tooling in the slots

What a site has before anyone runs day 0 is the twin's; everything that takes the switches from
factory state to the design is the software under test's:

| The twin's: bootstrap | The software under test's: day 0 |
|---|---|
| The switches, cabled, in factory state; the aggregation switch it configures, relaying to the tooling's addresses (the estate) | DHCP and ZTP servers, their images and configuration (`tap.containers`, `tap.networks`, `tap.files`) |
| The management node's NICs: each attach's address, `routes` and `siteGateway` (the estate), and its bare host (`tap.hosts`) | The design, every switch's configuration, the BMCs' DHCP, the tooling's backends (pods in the cluster) |
| The gateway, standing in for the site's routing | |

So the twin's values name the hosts (`tap.hosts`) and the tooling's values list only the
containers it runs (`tap.containers`), each by the host's name; Helm replaces a list a later
values file sets, so the two never overwrite each other. Upgrade with both files. A host with no
container stays bare and cabled. Until the tooling serves them, a management network with
`dhcp: stack` leaves its devices without addresses, and the gateway routes no BMC by its host
name either, as on a site.

The software under test brings its own DHCP and ZTP servers, as NVIDIA Config Manager's edge
(Kea with its DHCP API, configuration sync, ZTP API and SFTP; `compat/nvcm/nvcm.sh`) or NICo's DHCP
server. Each runs as it would on a site's management server, with two networks:

- **The switch network**, as `eth0` (or the attach's `interface`): the twin's port, at the
  attach's address. Give the attach `routes`, the site's networks (the management network, the
  service networks, the switches' loopbacks), so only those go through the switch.
- **Its backend**, as `eth1`: an engine network (`tap.networks`, `network:
  "<net>:interface_name=eth1"`), whose traffic leaves through the pod. Its default route is here,
  so it reaches the cluster: its backend's Services by name, as a pod does.

Further containers share the edge's network namespace (`network: "container:nvcm-edge"`).
Configuration files come from your Secrets and ConfigMaps (`tap.files`, `mounts`); capabilities,
sysctls and the hostname are `options`. A named volume (`options: [-v, <name>:<path>]`) is kept in
the engine's store across pods; containers are not: each pod starts the tooling again, and the
twin cables it again at `up`. values.yaml has NVIDIA Config Manager's edge as an example.

## Values

| Value | Default | |
|---|---|---|
| `image.repository`, `image.tag` | `registry.bud.studio/gpu-foundry/dc-twin`, the appVersion | |
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
| `substrate` | `loopback` | `tap` for real NOSes and day 0: a privileged pod ([above](#the-tap-substrate-real-switches-and-day-0)) |
| `tap.engine.image`, `.resources` | `quay.io/podman/stable:v5.2.2` | The engine sidecar |
| `tap.engine.persistence.enabled`, `.size`, `.storageClass`, `.existingClaim` | `true`, `20Gi`, default class | The engine's image store |
| `tap.registry`, `tap.registryAuthSecret` | `registry.bud.studio/gpu-foundry`, the first pull secret | Where the twin's switch images come from (`DCTWIN_REGISTRY`) |
| `tap.containers` | `[]` | `name`, `image`, `args`, `env`, `network` (default `none`), `mounts`, `options`: started in order before the twin, for `attach` |
| `tap.hosts`, `tap.hostImage` | `[]`, busybox | The management node's hosts the estate attaches by name: bare (`tap.hostImage`, idle) unless `tap.containers` has one of that name |
| `tap.networks` | `[]` | Engine networks the containers join by name |
| `tap.files` | `[]` | `name`, and a `secret` or a `configMap`: file sets the containers mount |

The pod and container security contexts, the service account (it mounts no token: the twin calls
no Kubernetes API), node selector, tolerations and affinity are in [values.yaml](values.yaml).

## What it does not run

- **`vm` devices.** They need `/dev/kvm` and QEMU. `up` refuses an estate with `vm` devices and
  says why; run those on a Linux host ([09](../../docs/spec/09-runtime.md)).
- **Real NOSes and day 0 without `substrate: tap`.** On the default loopback substrate every
  switch is a model.
- **An `attach` by `netns`** with `substrate: tap`: attach a container from `tap.containers`.
- **IPMI, SNMP and DHCP from outside the pod.** They are UDP, and the gateway proxies TCP. Their
  endpoints answer inside the pod (`kubectl exec … -- dctwin endpoints`).
- **Users and roles.** A console at a URL has one shared token until the control API has OIDC
  ([15 — Users and the control API](../../docs/spec/15-security.md)).
- **Two pods of one twin.** One engine per estate: the Deployment has one replica and replaces
  its pod with `Recreate`.

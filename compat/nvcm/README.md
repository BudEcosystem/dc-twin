# NVIDIA Config Manager day 0 (D80)

DSX OS's switch day 0 on the twin (18 — DSX OS: NVIDIA Config Manager's ZTP, Z7c). A factory
Cumulus fabric is brought up by NVIDIA Config Manager (NVCM), unmodified, from the design the
twin hands a site:

| Step | What runs | Changed? |
|---|---|---|
| Images | NVCM 1.4.1 (`github.com/dsx-ai-factory/nv-config-manager`, `2f8bf14`) built from its tag with its own Dockerfiles: `nvcm-app`, `nvcm-nautobot`, `nvcm-kea`, `nvcm-kea-admin` | Nautobot's Dockerfile's `--chown=root:root` is built as `--chown=0:0`: buildah cannot look `root` up in a distroless base. The same files and owner |
| Services | Postgres, Valkey (Redis), NATS with JetStream, Nautobot with uWSGI behind nginx, Nautobot's Celery worker, config-store, render with its two consumers, ZTP's API, Kea with its control agent, the DHCP API and sync, the device-facing ZTP API on 80, SFTP on 2222. Each run as the Helm chart runs it, with its commands, environment and one INI (`nvcm.sh`) | no |
| Source of truth | `stack-inputs/day0/nvcm`: the design as a Design Builder blueprint (`internal/day0/nvcm.go`), loaded by NVCM's own `MockTopologyDesign` job, the way NVCM's installer loads its blueprints | glue (`load.py`) |
| Rendering | NVCM's render service, from its Cumulus 5.14.0 templates: `In-Band-Leaf` for the leaves, `Converged-Spine` for the spines | no |
| DHCP | NVCM's DHCP service makes Kea's configuration from Nautobot: per switch, a reservation by its management MAC, with option 239 naming its boot script | no |
| ZTP | NVCM's boot script on each switch: release checked, serial validated, startup fetched over SFTP, `nv config replace` and apply, `provisioned` | no |
| Judging | The twin's day-0 oracle and the critic's `day0/fabric` policy | — |

## Run

Build NVCM's images once, on an x86_64 Linux host with rootless Podman:

```bash
git clone https://github.com/dsx-ai-factory/nv-config-manager && cd nv-config-manager && git checkout 1.4.1
podman build --build-context scripts=build/ -t localhost/nvcm-app:1.4.1 -f build/nv-config-manager.Dockerfile .
sed 's/--chown=root:root/--chown=0:0/' build/nautobot.Dockerfile > /tmp/nautobot.Dockerfile
podman build --build-context scripts=build/ -t localhost/nvcm-nautobot:1.4.1 -f /tmp/nautobot.Dockerfile components/nautobot/
podman build --build-context scripts=build/ -t localhost/nvcm-kea:1.4.1 -f build/kea.Dockerfile .
podman build --build-context scripts=build/ -t localhost/nvcm-kea-admin:1.4.1 -f build/kea-admin.Dockerfile .
```

Then run the test (`test/linuxnet/nvcm_real_linux_test.go`):

```bash
RUN=TestDay0NVCM NVCM_SRC=/path/to/nv-config-manager MODE=native ENGINE=podman-rootless scripts/linux-substrate-e2e.sh
```

`nvcm.sh` also runs NVCM by hand: `up <name> <work>`, then attach `<name>-edge`'s `eth0` to the
management network, then `load <name> <work> <blueprint dir> <ztp address>`, and `down <name>`.

## How it is laid out, and why

- **A bridge network.** The backend sits on its own bridge network and finds its peers by name. Four of its services listen on the same fixed ports: config-store and render on 9000, both render consumers on 8000.
- **The edge.** Kea and the device-facing services share one network namespace, the edge. The bridge is its `eth1`. Its `eth0` is the site's management network, attached by the twin at 10.90.0.10. NVCM's Kea configuration listens on `eth0`, and the ZTP service needs to see each switch's own source address.
- **Kea starts late.** `kea-dhcp4` starts once `eth0` exists. Until then supervisord gives it up as exiting too quickly, so `load` starts it again.
- **nginx in front of Nautobot.** Nautobot is reached through nginx, as the chart does. uWSGI's `http-socket` does not keep connections alive, and the render service's client reuses them.
- **Site secrets.** Each site the blueprint names gets its secrets in `config-secrets.ini`: the switches' passwords and the hash salt, by the site's slug.
- **Not run.** Temporal and NVCM's UI. Temporal is needed only for the backup that `provisioned` starts, which NVCM logs and skips when Temporal is not there.

Results are in [docs/implementation.md](../../docs/implementation.md#day-0).

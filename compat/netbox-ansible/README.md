# NetBox + Ansible day 0 (D79)

The day-0 integration of [18 — Day 0](../../docs/spec/18-day0.md) (Z6). A factory fabric is brought
up from the design the twin hands a site (`stack-inputs/day0/`), by open-source tools only, with
NVIDIA's own configuration:

| Step | What runs | Changed? |
|---|---|---|
| Source of truth | NetBox 4.2.9 (`quay.io/netboxcommunity/netbox:v4.2.9-3.2.1`), PostgreSQL 17 and Valkey 8 (ECR Public) | no |
| Load | `import-csv.py`: `stack-inputs/day0/netbox/*.csv` through NetBox's own CSV import (the UI's Import form, in the README's order) | glue |
| Leases | `record-leases.py`: each switch's management lease (the site's Kea) recorded on its `eth0` and made its out-of-band address | glue |
| First login | NVIDIA's `net-configurator` nc-v8.0.0 `push-switch-configs.yml --tags bootstrap-password`: the factory password changed through PAM's prompt | no |
| Inventory | Ansible's `netbox.netbox` 3.23.1 `nb_inventory` (`netbox.yml`) | no |
| Configuration | `templates/fabric_nvue.yaml.j2`: `net-configurator`'s spine template (MIT) ported onto NetBox's data, as NVUE's object tree | ported |
| Push | `push.yml`: NVIDIA's `nvidia.nvue` 1.2.9 `api` module over NVUE's REST API, one revision per switch, applied with `force` and `wait` | no |
| Judging | the twin's day-0 oracle (`internal/day0`) and the critic's `day0/fabric` policy | — |

The controller image (`Containerfile`) is `net-configurator` at its tag (checked by commit) with
its own pinned Python requirements (ansible 14.6, ansible-core 2.21), plus `nvidia.nvue`,
`pynetbox`, `sshpass` and OpenSSH. Nothing pulls from Docker Hub.

Run on an x86_64 Linux host with rootless Podman:

```bash
RUN=TestDay0NetBox MODE=native scripts/linux-substrate-e2e.sh
```

The test (`test/linuxnet/netbox_linux_test.go`) runs NetBox, its database and the controller in
the network namespace of the site's Kea on the OOB network, with the switches emulating Cumulus
Linux 5.18.3's NVUE (`spec.adapters.network.nos.cumulusVersion`), the release `net-configurator`
targets.

## What the port changes, and why

- **Form.** NVUE's object tree, not `nv set` lines, because the push goes through `nvidia.nvue`
  over REST. Every setting is one of the template's lines.
- **Values.** The hostname, loopback and AS come from NetBox: the device, its `lo` address, and
  its `bgp_asn` custom field (`custom_fields.csv`). NetBox relates ASNs to sites, not devices.
- **Underlay.** The design's fabric links are numbered /31s. Each cabled `swp` with an address
  gets a numbered neighbour, at its cable's far address, in the template's `underlay` peer group.
  The workbook's spines instead use unnumbered neighbours on breakout sub-ports.
- **Route-map names.** `LOOPBACK-BGP` and `WEIGHTED-ECMP` instead of `LOOPBACK_BGP` and
  `WEIGHTED_ECMP`. `nvidia.nvue`'s httpapi plugin rewrites `_` to `-` in every key but not in
  values, so a route map named with `_` would be created under one name and referred to by
  another.
- **Left out.** Breakout, OOB uplinks, overlay peers, LDAP, ACLs and banners: the design has no
  values for them.

Results are in [docs/implementation.md](../../docs/implementation.md#day-0).

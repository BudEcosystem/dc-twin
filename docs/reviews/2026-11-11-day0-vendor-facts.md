# Day 0 vendor facts — 2026-11-11

Research notes for emulating switch "day 0" bring-up (ONIE install, then NOS ZTP, then LLDP) in the twin. Every fact below cites its source. When the source is code, the file path and the commit read are given. Anything not confirmed in a primary source is marked **UNCONFIRMED** and repeated in the list at the end.

Sources read (snapshot 2026-11-11):

| Source | Revision |
|---|---|
| `github.com/opencomputeproject/onie` | `ac03c31f5ffd` (2026-11-10) |
| `github.com/sonic-net/sonic-ztp` | `036c8eaad345` (2026-04-15) |
| `github.com/sonic-net/SONiC` `doc/ztp/ztp.md` | `21fa0352a1c7` (2026-11-10) |
| `github.com/sonic-net/sonic-buildimage` | `master`, read through the GitHub API on 2026-11-11 |
| `github.com/lldpd/lldpd` | `62e946403635` (2026-11-09) |
| NVIDIA Cumulus Linux docs | 5.0, 5.11, 5.13, 5.14 and 5.18 pages, as cited below. The latest 5.x is 5.18. |

Short link names used below:

- `CL-ZTP` = <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-514/Installation-Management/Zero-Touch-Provisioning-ZTP/>
- `CL50-ZTP` = <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-50/Installation-Management/Zero-Touch-Provisioning-ZTP/>
- `CL-QSG` = <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-514/Quick-Start-Guide/> (5.18 version: <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-518/Quick-Start-Guide/>)
- `CL-NVUE-CLI` = <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux/System-Configuration/NVIDIA-User-Experience-NVUE/NVUE-CLI/> (renders as v5.18)
- `CL-NVUE-API` = <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-514/System-Configuration/NVIDIA-User-Experience-NVUE/NVUE-API/>
- `NVUE-REF-CONFIG` = <https://docs.nvidia.com/networking-ethernet-software/nvue-reference/Config-Commands/>
- `NVUE-CHEAT` = <https://docs.nvidia.com/networking-ethernet-software/knowledge-base/Setup-and-Getting-Started/NVUE-Cheat-Sheet/>
- `ONIE-DISCO` = <https://opencomputeproject.github.io/onie/design-spec/discovery.html>
- `SONIC-HLD` = <https://github.com/sonic-net/SONiC/blob/master/doc/ztp/ztp.md>

---

## A. Cumulus Linux 5.x ZTP

### A.1 Order of provisioning sources
ZTP tries these sources in order (`CL-ZTP`; the same order appears in `CL50-ZTP`):
1. A local file in `/var/lib/cumulus/ztp`. ZTP searches for it once, at boot.
2. A USB drive ("ZTP-USB"). ZTP tries once to find an inserted, unmounted drive. The drive must use FAT32, FAT16 or VFAT and must be inserted before power-up. It is mounted under `/tmp`, and scripts see the mount point as `$ZTP_USB_MOUNTPOINT`.
3. DHCP. ZTP checks DHCP every 10 seconds for up to 5 minutes when no local or USB script is found (`CL-ZTP`).

USB/local file names follow an ONIE-style waterfall, most specific first (`CL-ZTP`):
`cumulus-ztp-<arch>-<vendor>_<model>-r<revision>`, then `cumulus-ztp-<arch>-<vendor>_<model>`, then `cumulus-ztp-<vendor>_<model>`, then `cumulus-ztp-<arch>`, then `cumulus-ztp`.

### A.2 DHCP
- Option **239**, named `cumulus-provision-url` (type `text`), carries the script URL. The docs give this server-side ISC example (`CL-ZTP`):
  ```
  option cumulus-provision-url code 239 = text;
  subnet 192.0.2.0 netmask 255.255.255.0 {
    range 192.0.2.100 192.0.2.200;
    option cumulus-provision-url "http://192.0.2.1/demo.sh";
  }
  ```
- The switch sends option **60** (vendor class) with the value `cumulus-linux x86_64`. The doc says this value has two spaces between the words (`CL-ZTP`).
- The switch puts its serial number in option **61** (client identifier) (`CL-QSG`, 5.18).
- `CL-ZTP` does not mention options 114 or 67, and documents no default/fallback URL (`ztp -s` shows `URL None` when no URL is set). **UNCONFIRMED** whether the switch-side dhclient config requests anything beyond 239. The docs do not publish the switch's dhclient.conf.
- DHCP-triggered ZTP runs at boot, when a cable is plugged into or unplugged from eth0, or when power is restored, and only if the switch has not been provisioned yet (`CL-ZTP`).

### A.3 HTTP request headers sent when fetching the script
These are documented identically in 5.0 (`CL50-ZTP`) and 5.14 (`CL-ZTP`). Header names and example values:

| Header | Example value |
|---|---|
| `User-Agent` | `CumulusLinux-AutoProvision/0.4` |
| `CUMULUS-ARCH` | `x86_64` |
| `CUMULUS-BUILD` | `5.1.0` (5.14 page) / `5.0.0` (5.0 page) |
| `CUMULUS-MANUFACTURER` | `odm` |
| `CUMULUS-PRODUCTNAME` | `switch_model` |
| `CUMULUS-SERIAL` | `XYZ123004` |
| `CUMULUS-BASE-MAC` | `44:38:39:FF:40:94` (upper-case, colon-separated) |
| `CUMULUS-MGMT-MAC` | `44:38:39:FF:00:00` |
| `CUMULUS-VERSION` | `5.1.0` / `5.0.0` |
| `CUMULUS-PROV-COUNT` | `0` |
| `CUMULUS-PROV-MAX` | `32` |

- `CUMULUS-LICENSE-INSTALLED` and `CUMULUS-PROD-ID` are **not** in the 5.x tables. Treat them as absent in 5.x (**UNCONFIRMED** whether they ever existed in older releases).
- The docs describe PROV-COUNT and PROV-MAX only through their example values. Their semantics (attempt counter and maximum) are **UNCONFIRMED**.

### A.4 Script marker, exit codes, retries
- The script must contain the string `CUMULUS-AUTOPROVISIONING`, conventionally as the comment `# CUMULUS-AUTOPROVISIONING`. It does not need to be printed to stdout. `CUMULUS-AUTOPROVISIONING-CASCADE` is an additional directive that lets provisioning continue past the script (`CL-ZTP`). Log line when found: `ZTP Manual: Found Marker CUMULUS-AUTOPROVISIONING`.
- Exit code: `0` means success, and ZTP marks provisioning complete. Any non-zero code is a failure, logged as "Script returned failure" (and, in the DHCP case, e.g. `ZTP DHCP: Payload returned code 1`) (`CL-ZTP`).
- Retries: the sample log shows "Retrying" lines before `ZTP script failed. Exiting...`. **The retry count is UNCONFIRMED.** The DHCP discovery window is 10 s × up to 5 min. The docs' `ping_until_reachable` helper makes 30 attempts with a 1 s sleep (`CL-ZTP`).
- The script runs locally as root. The docs advise against redirecting stderr (`&> /dev/null`) because it can disrupt NVUE (`CL-ZTP`).
- During first boot, front-panel and management interfaces stay Oper UP until configuration completes, ZTP finishes, or ZTP terminates. `onie-install -z <URL>` can hand ONIE an empty ZTP script (`CL-ZTP`).

### A.5 `ztp` CLI (`/usr/sbin/ztp`)
Flags from `CL-ZTP` and `CL50-ZTP`:

| Flag | Meaning |
|---|---|
| `-e` | Enable ZTP and activate provisioning |
| `-d` | Disable ZTP and deactivate provisioning (does not stop a running script) |
| `-r [URL\|path]` / `--run <URL>` | Run ZTP manually, optionally from a URL or local path. The 5.0 docs describe it as "force provisioning, ignoring the status in the configuration file" |
| `-R` | Reset ZTP to its original state by removing the `ztp` directory, so ZTP runs again at next reboot |
| `-s` | Show current ZTP state |
| `-v` | Verbose output, used with `-r` for debugging |
| `-b` | Appears only as the systemd `ExecStart=/usr/sbin/ztp -b` (boot mode, inferred). **Meaning UNCONFIRMED** |
| `-i` | **Not documented. UNCONFIRMED / likely nonexistent** |

`ztp -s` output, verbatim from `CL-ZTP`:
```
ZTP INFO:
State          disabled
Version        1.0
Result         success
Date           Mon May 20 21:51:04 2019 UTC
Method         Switch manually configured
URL            None
```
NVUE equivalents: `nv action enable|disable|run|abort system ztp` (`run` takes an optional `url`, and all four take an optional `force`). `nv show system ztp` prints (`CL-ZTP`):
```
        operational
-------  -----------
service  enabled
status   enabled
version  1.0
```

### A.6 Logs and state files
- ZTP logs to `/var/log/syslog` (find it with `grep -i ztp`). The sample script writes its own output to `/var/log/autoprovision` (`exec >> /var/log/autoprovision 2>&1`). The sample log shows `/var/lib/cumulus/ztp/ztp_state.log` and the lock `/var/run/ztp.lock` being created (`CL-ZTP`).

### A.7 When ZTP runs
- At first boot, until provisioned. A manual run is skipped if ZTP already ran on a previous boot **or if manual configuration changes were made**: ZTP then treats the switch as provisioned and exits, as reflected in `Method  Switch manually configured` (`CL-ZTP`).

### A.8 ZTP with NVUE
- The docs' scripts use `nv set ...` followed by `nv config apply`, with no `-y` in the ZTP page examples. A startup file from USB is applied with `nv config patch ${ZTP_USB_MOUNTPOINT}/startup.yaml` then `nv config apply` (`CL-ZTP`).
- Non-interactive apply is `nv config apply --y` / `--assume-yes`. The NVUE reference documents `--y`, not `-y` (`NVUE-REF-CONFIG`). Whether a single-dash `-y` is accepted is **UNCONFIRMED** from docs.
- From 5.9, auto-save is on by default, so `nv config apply` also writes `/etc/nvue.d/startup.yaml`. On 5.8 and earlier you need `nv config save` or `nv set system config auto-save enable on` (`NVUE-REF-CONFIG`; <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-59/System-Configuration/NVIDIA-User-Experience-NVUE/NVUE-CLI/>).
- `nv config replace <file>` replaces the whole pending config. From 5.13 the file can be YAML or a text file of `nv set`/`nv unset` lines (`NVUE-REF-CONFIG`).

---

## B. Cumulus Linux 5.x factory defaults

| Item | Default | Source |
|---|---|---|
| Hostname | `cumulus`, set by the default `/etc/nvue.d/startup.yaml`. Because of that, the switch **does not accept the DHCP `host-name` option** | `CL-NVUE-CLI` ("Default Startup File") |
| Default startup file | `/etc/nvue.d/startup.yaml`. It holds the `cumulus` user account and the factory configuration | `CL-NVUE-CLI` |
| eth0 | "The management interface uses DHCPv4 for addressing by default." | `CL-QSG` |
| Management VRF | "Cumulus Linux enables Management VRF by default". eth0 (eth1 on some platforms) is the only supported OOB interface | <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-514/Layer-3/VRFs/Management-VRF/> |
| Login | User `cumulus`, password `cumulus`. On first login you are prompted to change the password, then the SSH session disconnects and you must reconnect. Root is locked. `cumulus` is in the sudo group | `CL-QSG`; <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-514/System-Configuration/Authentication-Authorization-and-Accounting/User-Accounts/> |
| Exact password-change prompt text | not documented. **UNCONFIRMED** | — |
| Data-plane ports (swp*) | **admin down**: "By default, Cumulus Linux disables all data plane ports (every Ethernet port except the management interface, eth0)." | `CL-QSG` |
| Bridge | "Cumulus Linux does not put all ports into a bridge by default." `br_default` exists only once you add ports to it | `CL-QSG` |
| SSH | Enabled. "All accounts except root can use remote SSH login" | `CL-QSG` |
| MTU | 9216 ("ifupdown2 assigns 9216 as the default MTU"; the driver's initial value is 9238) | <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-514/Layer-1-and-Switch-Ports/Interface-Configuration-and-Management/Switch-Port-Attributes/>; `NVUE-CHEAT` |
| Auto-negotiation / FEC | NVUE enables auto-negotiation by default. Auto FEC is the default on Spectrum | same Switch-Port-Attributes page |
| Time zone | `Etc/UTC` (shown in `nv show system date-time`; not labelled as "default") | <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-518/System-Configuration/Date-and-Time/Setting-Date-and-Time/> |
| NTP | `server 0..3.cumulusnetworks.pool.ntp.org iburst` in `/etc/ntpsec/ntp.conf`. Which VRF ntpd runs in by default is **UNCONFIRMED** | <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-514/System-Configuration/Date-and-Time/Network-Time-Protocol-NTP/> |
| NVUE REST API | **Enabled by default** on port **8765**. With no `listening-address` configured, nginx "listens on all addresses for the target port" (5.14). Configure with `nv set system api port <n>`, `nv set system api listening-address <ip>`, `nv set system api state disabled` | `CL-NVUE-API` |
| LLDP | "Cumulus Linux enables the `lldp` service by default." `tx-interval 30`, `tx-hold-multiplier 4` (TTL = 4 × 30 = 120 s, by calculation), port ID subtype default `ifname`, 802.1 TLVs off, LLDP-MED inventory off, mode `default`. 802.3 Link Aggregation and Max Frame Size TLVs are sent by default | <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-514/Layer-2/Link-Layer-Discovery-Protocol/> |
| PTM | `ptmd` exists in 5.x. It needs `/etc/ptm.d/topology.dot` to start. NVUE controls only the FRR check-link-state hook (`nv set router ptm enable` / `nv unset router ptm enable`). Whether ptmd is enabled by default is **UNCONFIRMED** | <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-514/Layer-1-and-Switch-Ports/Prescriptive-Topology-Manager-PTM/> |

Notes:
- Whether the REST API listened on localhost only in early 5.x releases is **UNCONFIRMED**. The 5.14 doc says it listens on all addresses unless you set `listening-address`.
- `topology.dot` format is an undirected Graphviz graph. Physical ports only, no bonds or subinterfaces. The docs recommend the same file on every switch:
  ```
  graph G {
      "spine1":"swp1" -- "leaf1":"swp1";
      "spine1":"swp2" -- "leaf2":"swp1";
      "leaf1":"swp5s0" -- "server1":"eth1";
  }
  ```
  `ptmctl` columns: `port, cbl status, BFD status, BFD peer, BFD local, BFD type`. `-d` adds `exp nbr, act nbr, sysname, portID, portDescr, match on, last upd, …`; `-l` shows the LLDP view. `cbl status` ∈ {`pass`, `fail`, `N/A`}.
- `nv show service lldp` sample:
  ```
                          operational  applied
  ----------------------  -----------  -------
  tx-interval             30           30
  tx-hold-multiplier      4            4
  dot1-tlv                off          off
  lldp-med-inventory-tlv  off          off
  mode                    default      default
  state                   enabled      disabled
  ```

---

## C. NVUE REST API and `nv` CLI

### C.1 REST API (`CL-NVUE-API`)
- Base URL is `https://<switch>:8765/nvue_v1`. Auth is HTTP **basic**, with the same users as the CLI. The examples use `-k`/`--insecure` because of the self-signed certificate.
- Revision workflow:
  1. `POST /nvue_v1/revision` returns `{"<rev>": {"state": "pending", "transition": {"issue": {}, "progress": ""}}}`
  2. `PATCH /nvue_v1/<path>?rev=<rev>` with a JSON body, e.g.
     `curl -u 'cumulus:cumulus' -d '{"99.99.99.99/32": {}}' -H 'Content-Type: application/json' -k -X PATCH https://127.0.0.1:8765/nvue_v1/interface/lo/ip/address?rev=2`
  3. Apply with `PATCH /nvue_v1/revision/<rev>` and body `{"state": "apply", "auto-prompt": {"ays": "ays_yes"}}`.
- Create example: `curl -u 'cumulus:cumulus' --insecure -X POST https://127.0.0.1:8765/nvue_v1/revision`.

### C.2 `nv` CLI (`CL-NVUE-CLI`, `NVUE-REF-CONFIG`)

| Command | Behaviour / output |
|---|---|
| `nv set <path> <value>` / `nv unset <path>` | Stage changes in the pending revision |
| `nv show <path> [-o auto\|json\|yaml\|native]` (`--output`) | Default `auto` is a table with `operational` / `applied` (and `pending`) columns. `--pending`, `--applied` and `--operational` select the view, `--rev <n>` selects a revision |
| `nv config apply [--y\|--assume-yes\|--assume-no] [--confirm [<time>]] [<rev>]` | Apply pending. `--confirm` auto-rolls back after 10 min unless confirmed. `<rev>` ∈ `applied`, `startup`, `empty` or a number. Auto-saves from 5.9 on |
| `nv config diff [<base> <target>]` | YAML diff, new config only by default. `--verbose` shows both sides. `-o commands` shows the diff as commands |
| `nv config replace <file>` | Replace the whole pending config (YAML, or set/unset text from 5.13) |
| `nv config patch <file>` | Merge a file into pending. "A patch contains a single request to the NVUE service"; ordering inside the patch is not guaranteed |
| `nv config save` | Write applied → `/etc/nvue.d/startup.yaml` (needed only with auto-save off) |
| `nv config history [<rev>]` | Table of revision ID, date/time, user and change type |
| `nv config show [--all] [-r <rev>] [-o commands]` | Applied config as YAML (header line e.g. `version: Cumulus Linux 5.18.0`). `-o commands` prints it as `nv set` lines (`CL-NVUE-CLI`; not listed in `NVUE-REF-CONFIG`) |
| `nv config detach` | The two docs disagree. `NVUE-REF-CONFIG` says it **discards** changes since the last apply. `CL-NVUE-CLI` says it detaches pending and saves it under an integer ID (e.g. `4`). **Exact semantics UNCONFIRMED** |
| `nv config revision` | Lists revisions (`CL-NVUE-CLI`) |

### C.3 NVUE paths

| Purpose | Command | Source / note |
|---|---|---|
| Hostname | `nv set system hostname leaf01` | `NVUE-CHEAT`; `CL-QSG` |
| Time zone | `nv set system date-time timezone US/Eastern` (5.14+ / 5.18 docs). Older form: `nv set system timezone US/Eastern` | 5.18 Setting-Date-and-Time page; `NVUE-CHEAT`. **The release where it changed is UNCONFIRMED** |
| NTP | `nv set service ntp <vrf> server <host> iburst on`, e.g. `nv set service ntp default server 4.cumulusnetworks.pool.ntp.org iburst on`. For mgmt: `nv set service ntp mgmt server <host> iburst on` | NTP page (5.14) shows the `default` VRF form and `nv show service ntp mgmt server`. **The mgmt `set` form is inferred from the `<vrf>` grammar, UNCONFIRMED verbatim** |
| DNS | `nv set service dns mgmt server 198.51.100.31` | `NVUE-CHEAT` |
| Syslog | 5.13+: `nv set system syslog server 192.168.0.254 port 601`, `… protocol tcp`, `… vrf mgmt`. Older: `nv set service syslog <vrf> server <ip> port 514` | <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-513/Monitoring-and-Troubleshooting/Syslog/>; `NVUE-CHEAT` |
| LLDP | `nv set service lldp state enabled\|disabled`, `tx-interval <s>`, `tx-hold-multiplier <n>`, `dot1-tlv on`, `mode default\|force-send-lldp\|force-send-cdpv1\|force-send-cdpv2`, `lldp-med-inventory-tlv on`. Per port: `nv set interface swp1 lldp state …` | LLDP page (5.14) |
| User | `nv set system aaa user admin2 role system-admin`, `… password [<plain>]`, `… hashed-password <hash>`, `… full-name "FIRST LAST"`. Roles: `system-admin`, `nvue-admin`, `nvue-monitor` | User-Accounts page (5.14) |
| Breakout | `nv set interface swp1 link breakout 4x` (5.14 page). The cheat sheet shows `4x25G`. Then `nv set interface swp1s0-3 link state up` | Switch-Port-Attributes (5.14); `NVUE-CHEAT` |
| MTU | `nv set interface swp1 link mtu 1500` (default 9216) | same |
| Speed | `nv set interface swp1 link speed 400G` | same |
| Link state | `nv set interface swp1s0-3 link state up` (also `down`) | same |
| Auto-neg | `nv set interface swp1 link auto-negotiate on\|off` | same |
| RoCE | `nv set qos roce` ≡ `nv set qos roce mode lossless` (default mode lossless). `nv set qos roce mode lossy` | <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-514/Layer-1-and-Switch-Ports/Quality-of-Service/RDMA-over-Converged-Ethernet-RoCE/> |
| BGP ASN / RID | `nv set router bgp autonomous-system 65101`; `nv set router bgp router-id 10.10.10.1` | <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-514/Layer-3/Border-Gateway-Protocol-BGP/Basic-BGP-Configuration/> |
| BGP unnumbered neighbor | `nv set vrf default router bgp neighbor swp51 remote-as external` | same |
| Network | `nv set vrf default router bgp address-family ipv4-unicast network 10.10.10.1/32` | same |
| Peer group | `nv set vrf default router bgp peer-group SPINE remote-as external`; `nv set vrf default router bgp neighbor swp51 peer-group SPINE` | <https://docs.nvidia.com/networking-ethernet-software/cumulus-linux-514/Layer-3/Border-Gateway-Protocol-BGP/Optional-BGP-Configuration/> |
| PTM | `nv set router ptm enable` | PTM page (5.14) |
| Management VRF | built-in `mgmt` | Management-VRF page |

---

## D. ONIE

Source code is `opencomputeproject/onie@ac03c31f5ffd`. The design spec is `ONIE-DISCO`.

### D.1 DHCPv4 client identity
Built in `rootconf/default/lib/onie/functions`, `udhcpc_args()`:
- Option **60** vendor class is `onie_vendor:${onie_platform}` (`udhcpc -V onie_vendor:<platform>`), e.g. `onie_vendor:x86_64-mlnx_msn2700-r0`.
- Option **77** user class is the literal string `onie_dhcp_user_class`.
- Option **61** client id is `00` + hex(serial number from `onie-sysinfo -s`).
- Option **125** (VIVSO) uses enterprise number **42623** (`onie_iana_enterprise=42623`, "Open Compute Project"). The client sends sub-options 3 = machine, 4 = CPU arch, 5 = machine revision. The server may return sub-option **1 = installer URL** and **2 = updater URL** (`import_vendoropts()`); updater URL is used only in update/embed boot mode.
- `onie_platform` is `${onie_arch}-${onie_machine}-r${onie_machine_rev}` (`rootconf/default/etc/init.d/gen-config.sh`; build side `build-config/Makefile`: `PLATFORM = $(ARCH)-$(MACHINE_PREFIX)`, `MACHINE_PREFIX = $(MACHINE)-r$(MACHINE_REV)`).

### D.2 DHCP options requested and used
- `rootconf/default/bin/discover`, `sd_dhcp4()`, requests `-O 7 43 54 66 67 72 114 125` on top of busybox udhcpc defaults (`-t 2 -T 2 -n`), trying every interface until one succeeds. The design spec's PRL list is 1, 3, 6, 7, 12, 15, 42, 54, 66, 67, 72, 114, 150 (`ONIE-DISCO`).
- Busybox env-name mapping comes from the ONIE patch `patches/busybox/dhcp-additional-options.patch`: 114 → `url`, 72 → `wwwsrv`, 125 → `vivso`, 150 → `tftpsiaddr`. Exported to the installer as `onie_disco_<name>`.
- Semantics (`ONIE-DISCO`): **114** is an exact URL (HTTP intended, other schemes accepted). **72** is an HTTP server IP, tried with the default file names at server root. **66** plus **67** form an exact TFTP URL. **67** alone is also tried as a URL. **150** is the TFTP server IP.
- Option 43 is requested, but no parsing of it was found in `rootconf/default`. **Use of option 43 is UNCONFIRMED.**
- DHCPv6 (`sd_dhcp6`), mDNS (`sd_mdns`) and fallback (`sd_fallback`) are **TODO stubs that return 1** in `bin/discover`.

### D.3 Discovery and execution order
`bin/discover` `service_discovery()` runs: static URL (from boot cmdline; if set, return), then local filesystems, then UBI filesystems, then DHCPv6 (stub), then DHCPv4, then mDNS (stub), then fallback (stub). After that, `neigh_discovery()` pings `ff02::1` on each interface (`ping6 -I $i`) and collects `ip -4 neigh` and `ip -6 neigh` into `onie_neighs`.

`bin/exec_installer` then tries, in order:
1. CLI static URL (`onie-nos-install <url>`). If it fails, it stops.
2. CLI static updater URL.
3. `onie_static_url` from the kernel cmdline.
4. Pending firmware updates.
5. Local partitions (`local_fs_run`), trying every default file name.
6. Exact URLs: `onie_disco_onie_url` (VIVSO sub-opt 1/2), then `onie_disco_url` (opt 114).
7. `http_download()`, over a unique-ordered server list: `onie-server` (DNS name), opt 72 `wwwsrv`, BOOTP `siaddr`, opt 54 `serverid`, opt 150 `tftpsiaddr`, opt 66 `tftp`, then the link-local neighbours. For each server that answers `nc -w 10 <server> 80`, it tries `http://<server>/<default file name>` for every name. Then it tries opt 67 bootfile as a URL.
8. `tftp_download()`, over `onie-server`, `siaddr`, opt 66, opt 150, opt 54, with bootfile from opt 67 / BOOTP `boot_file`.
9. `waterfall()` (TFTP only), over `onie-server`, `siaddr`, opt 66, opt 150, with paths in this order: `<MAC with dashes>/<default_filename>`, then `<IP hex 8 chars>/…` down to a 1-char prefix, then all default file names at root. It stops on a TFTP timeout.
10. On failure it sleeps 20 s (`delay=20`) and loops forever.

The default server name is `onie_server_name="onie-server"` (`lib/onie/functions`). If DNS resolves it, ONIE tries `http://onie-server/` and `tftp://onie-server/` (`ONIE-DISCO`).

### D.4 Default installer file names (waterfall), from `lib/onie/functions`
Prefix is `onie-installer` (`onie_operation=os-install`), or `onie-updater` in update/embed mode (`onie_operation=onie-update`). For each name, the bare name is tried first, then the name + `.bin` (`onie_image_suffixes=".bin"`):
1. `onie-installer-<arch>-<machine>-r<rev>` (= `onie-installer-${onie_platform}`; machine is `<vendor>_<model>`)
2. `onie-installer-<arch>-<machine>`
3. `onie-installer-<machine>`
4. `onie-installer-<arch>-<switch_asic>` (e.g. `x86_64-mlnx`)
5. `onie-installer-<arch>`
6. `onie-installer`

Each one is followed by its `.bin` variant: `onie-installer-x86_64-mlnx_msn2700-r0`, `onie-installer-x86_64-mlnx_msn2700-r0.bin`, …. The `.bin` suffix was added in 2017.08 and the asic form in 2016.05 (`ONIE-DISCO`).

### D.5 HTTP headers
From `rootconf/default/bin/exec_installer`, `wget_run()`, using `wget -T 3`:
```
User-Agent: onie/1.0 (<uname -s>-<uname -r><onie_version>; <busybox version>)
ONIE-SERIAL-NUMBER: <onie_serial_num>
ONIE-ETH-ADDR: <onie_eth_addr>         (mgmt MAC, aa:bb:cc:dd:ee:ff form from onie-sysinfo -e)
ONIE-VENDOR-ID: <onie_vendor_id>       (IANA enterprise number, decimal; Mellanox = 33049)
ONIE-MACHINE: <onie_machine>           (e.g. mlnx_msn2700)
ONIE-MACHINE-REV: <onie_machine_rev>   (e.g. 0)
ONIE-ARCH: <onie_arch>                 (e.g. x86_64)
ONIE-SECURITY-KEY: <onie_sec_key>
ONIE-OPERATION: os-install | onie-update
ONIE-VERSION: <onie_version>
```
`ONIE-VERSION` is sent by the code but is **missing from the design-spec header table** (`ONIE-DISCO` lists the first eight). The exact case of the `ONIE-ETH-ADDR` hex digits is **UNCONFIRMED**, since it depends on `onie-sysinfo -e`. The format of the `onie_sec_key` value is **UNCONFIRMED**.

Installer contract: the downloaded image is run, and `/var/run/install.rc` = `0` means success. The updater image must contain `ONIE-UPDATER-COOKIE` within its first 100 lines (`lib/onie/functions`).

### D.6 NVIDIA/Mellanox platform strings
- In ONIE, `machine/mellanox/mlnx_x86/machine.make` sets `ONIE_ARCH ?= x86_64`, `SWITCH_ASIC_VENDOR = mlnx`, `VENDOR_ID = 33049`, and build machine `mlnx_x86`, giving the build-time platform `x86_64-mlnx_x86-r0`.
- At runtime, `machine/mellanox/mlnx_x86/rootconf/sysroot-lib-onie/gen-config-platform` reads EEPROM TLV **0x21** (product name), lowercases it, and maps legacy codenames: panther → `mlnx_msn2700`, spider → `mlnx_msn2410`, neptune → `mlnx_msx6710`, scorpion → `mlnx_msb7700`, tarantula → `mlnx_msx1410`. Otherwise it uses `mlnx_<part before '-'><part after '-'>`. It then sets `onie_platform=x86_64-<live_machine>-r<rev>`.
- SONiC platform directories, which equal the ONIE platform string, are in `sonic-buildimage/device/mellanox/`. Mellanox-era names: `x86_64-mlnx_msn2010-r0`, `x86_64-mlnx_msn2100-r0`, `x86_64-mlnx_msn2410-r0`, `x86_64-mlnx_msn2700-r0`, `x86_64-mlnx_msn2740-r0`, `x86_64-mlnx_msn3420-r0`, `x86_64-mlnx_msn3700-r0`, `x86_64-mlnx_msn3700c-r0`, `x86_64-mlnx_msn3800-r0`, `x86_64-mlnx_msn4410-r0`, `x86_64-mlnx_msn4600-r0`, `x86_64-mlnx_msn4600c-r0`, `x86_64-mlnx_msn4700-r0`. NVIDIA-era names: `x86_64-nvidia_sn2201-r0`, `x86_64-nvidia_sn4280-r0`, `x86_64-nvidia_sn4800-r0`, `x86_64-nvidia_sn5400-r0`, **`x86_64-nvidia_sn5600-r0`**, `x86_64-nvidia_sn5610n-r0`, `x86_64-nvidia_sn5640-r0`, plus `_simx` variants and `x86_64-nvidia_sn6600_ld-r0` / `x86_64-nvidia_sn6810_ld-r0` (listing of <https://github.com/sonic-net/sonic-buildimage/tree/master/device/mellanox>).
  - The ONIE mapping above would yield `mlnx_*`. How NVIDIA-era ONIE produces `nvidia_sn5600` is in vendor-shipped ONIE, not upstream, so **SN5600's ONIE-side string is UNCONFIRMED from upstream ONIE source**. That it is `x86_64-nvidia_sn5600-r0` is inferred from the SONiC device directory name.

### D.7 `onie-syseeprom` TLV codes
From `patches/busybox/feature-onie-syseeprom-command.patch` → `include/onie_tlvinfo.h`. The header ID string is `"TlvInfo"` and the version is `0x01`.

| Code | Name | Code | Name |
|---|---|---|---|
| 0x21 | Product Name | 0x29 | ONIE Version |
| 0x22 | Part Number | 0x2A | MAC Addresses (count) |
| 0x23 | Serial Number | 0x2B | Manufacturer |
| 0x24 | Base MAC Address | 0x2C | Country Code |
| 0x25 | Manufacture Date | 0x2D | Vendor Name |
| 0x26 | Device Version | 0x2E | Diag Version |
| 0x27 | Label Revision | 0x2F | Service Tag |
| 0x28 | Platform Name | 0xFD | Vendor Extension |
| | | 0xFE | CRC-32 |

---

## E. SONiC ZTP (`sonic-ztp`)

### E.1 DHCP options
Switch-side dhclient config is `sonic-buildimage/files/dhcp/dhclient.conf.j2` (<https://github.com/sonic-net/sonic-buildimage/blob/master/files/dhcp/dhclient.conf.j2>):
```
option snmp-community code 224 = text;
option minigraph-url code 225 = text;
option acl-url code 226 = text;
option tftp-server-name code 66 = text;
option bootfile-name code 67 = text;
option user-class code 77 = text;
option provisioning-script-url code 239 = text;
option dhcp6.user-class code 15 = text;
option dhcp6.provisioning-script-url code 239 = text;
option dhcp6.boot-file-url code 59 = text;
```
When ZTP is enabled, it also requests `bootfile-name, provisioning-script-url, tftp-server-name, dhcp6.provisioning-script-url, dhcp6.boot-file-url`, and sends `user-class "SONiC-ZTP"`, `dhcp6.user-class "SONiC-ZTP"`, and `dhcp-client-identifier "SONiC##<product-name>##<serial-no>"`, with `retry 60`.

The exit hook `src/usr/lib/ztp/dhcp/ztp` (installed as `/etc/dhcp/dhclient-exit-hooks.d/ztp`) writes the URLs to `/var/run/ztp/`:
- `dhcp_67-ztp_data_url`, from `new_bootfile_name`
- `dhcp_66-ztp_tftp_server`
- `dhcp6_59-ztp_data_url`
- `dhcp_239-provisioning-script_url`
- `dhcp6_239-provisioning-script_url`
- `dhcp_graph_url` / `dhcp_acl_url` (225/226)

It does nothing if `ztp status -c` = `0:DISABLED`.

Precedence, from `src/usr/lib/ztp/ztp-engine.py` discovery and `SONIC-HLD` §3.5:
1. If `/etc/sonic/minigraph.xml` exists, or `/etc/sonic/config_db.json` exists with `monitor-startup-config` true, the mode is `MANUAL_CONFIG` and ZTP is skipped.
2. Local `/host/ztp/ztp_data_local.json` (`local-fs`)
3. opt 67 (prefixed `tftp://<opt66>/` when option 66 is present) (`dhcp-opt67`)
4. DHCPv6 opt 59 (`dhcp6-opt59`)
5. opt 239 (`dhcp-opt239`)
6. DHCPv6 opt 239 (`dhcp6-opt239`)
7. opt 225/226 graph/ACL (`dhcp-opt225-graph-url`)

Option 67 takes precedence over 239. Options 224/225/226 are deprecated (`SONIC-HLD`). For option 239, the engine synthesises `{"ztp": {"provisioning-script": {"plugin": {"url": "file:///host/ztp/provisioning-script", "ignore-section-data": true}}, "restart-ztp-no-config": false}}` (`__createProvScriptJson`). The script's exit code 0 means success and anything else means failure. ZTP does not re-run automatically after a 239 script (`SONIC-HLD`).

### E.2 Defaults (`src/usr/lib/python3/dist-packages/ztp/defaults.py`; overridable in `/host/ztp/ztp_cfg.json`)

| Key | Default |
|---|---|
| `admin-mode` | `True` |
| `ztp-json` | `/host/ztp/ztp_data.json` |
| `ztp-json-local` | `/host/ztp/ztp_data_local.json` |
| `ztp-json-shadow` | `/host/ztp/ztp_data_shadow.json` |
| `provisioning-script` | `/host/ztp/provisioning-script` |
| `log-file` | `/var/log/ztp.log` |
| `log-level` | `INFO` |
| `http-user-agent` | `SONiC-ZTP/0.1` |
| `include-http-headers` | `True` |
| `curl-retries` / `curl-timeout` | `3` / `30` |
| `discovery-interval` | `10` |
| `restart-ztp-interval` | `300` |
| `https-secure` | `True` |
| `feat-inband` / `feat-ipv4` / `feat-ipv6` / `feat-console-logging` | `True` |
| `ignore-result`, `halt-on-failure`, `reboot-on-success`, `reboot-on-failure`, `restart-ztp-on-failure` | `False` |
| `restart-ztp-no-config`, `restart-ztp-on-invalid-data`, `monitor-startup-config` | `True` |
| `config-fallback` | `False` |
| `ztp-json-version` | `"1.0"` |
| per-section dirs | `/var/lib/ztp/sections/<section>/input.json` |

### E.3 HTTP headers (`src/usr/lib/python3/dist-packages/ztp/Downloader.py`)
These are sent with curl `-H` when `include-http-headers` is true. Values come from the system EEPROM (`DecodeSysEeprom.py`):
```
User-Agent: SONiC-ZTP/0.1
PRODUCT-NAME: <eeprom product name>
SERIAL-NUMBER: <eeprom serial>
BASE-MAC-ADDRESS: <eeprom base MAC>
SONiC-VERSION: <sonic version>
```

### E.4 URL identity: no `$SERIAL_NUMBER` / `$MAC` substitution
There is no shell-style variable substitution in URLs. Identity goes into URLs through `dynamic-url` (`src/usr/lib/python3/dist-packages/ztp/ZTPObjects.py`, classes `DynamicURL` and `Identifier`):
```json
"dynamic-url": { "source": { "prefix": "http://srv/", "identifier": "serial-number", "suffix": "_config_db.json" },
                 "destination": "/etc/sonic/config_db.json" }
```
- The resulting source is `prefix + identifier + suffix`.
- `identifier` ∈ `hostname`, `hostname-fqdn`, `serial-number`, `product-name`, `sonic-version`, any other literal string, or `{"url": {...}}`. In the last case a script is downloaded and run, and its output is the identifier.
- **There is no `mac` identifier.**
- `url` objects have keys `source` (mandatory), `destination`, `secure`, `encrypted`, `timeout`, `include-http-headers`, `curl-arguments`.
- `plugin.dynamic-url` > `plugin.url` > `plugin.name` (`SONIC-HLD` §3.2.1).

### E.5 ztp.json schema
- Top-level object is `"ztp"`. Leaf keys inside it (non-dict values, e.g. `status`, `ztp-json-version`, `restart-ztp-no-config`, `config-fallback`) are top-level settings. Every dict-valued key is a **configuration section** (`ZTPSections.py`: `self.section_names = sorted(self.ztpDict.keys())`, with non-dict entries removed).
- Sections run in **lexical order of name**. A leading `NNN-` is stripped to find the built-in plugin, so `01-configdb-json` runs the `configdb-json` plugin (`ZTPSections.py` `re.split("^[0-9]+-", …)`; `SONIC-HLD`).
- Per-section keys (`SONIC-HLD` §3.1; `ZTPSections.py` `__buildDefaults`):
  - `plugin`: string name, or an object with `name` / `url` / `dynamic-url` / `args` / `ignore-section-data`
  - `ignore-result`, `halt-on-failure`, `reboot-on-success`, `reboot-on-failure`: bool, default false, filled in by the engine
  - `status`
  - `timestamp`, `start-timestamp` (set by the engine)
  - `description`
  - `exit-code` (set by the engine)
  - `suspend-exit-code`: positive int; invalid values are dropped
- **Status values:** `BOOT` (default, not yet applied), `IN-PROGRESS`, `SUCCESS`, `FAILED`, `SUSPEND`, `DISABLED`. Any other value is coerced to `DISABLED`. `ztp status` prints `BOOT` as `Not Started`.
- Plugin exit codes: `0` → SUCCESS, `== suspend-exit-code` → SUSPEND (retried after the other sections), anything else → FAILED (`SONIC-HLD` §3.2.2; `ztp-engine.py` around line 518).
- Timestamp format: `%Y-%m-%d %H:%M:%S %Z`, e.g. `2019-09-11 19:12:24 UTC` (`src/usr/bin/ztp` `timeDiff`).

### E.6 Built-in plugins (`src/usr/lib/ztp/plugins/`)

| Plugin | Keys (defaults) |
|---|---|
| `configdb-json` | `url` / `dynamic-url` (download to `/etc/sonic/config_db.json`); `clear-config` (default **true**); `save-config` (default false, runs `config save -y`); `overwrite-device-metadata` (default false). Does a `config reload` |
| `firmware` | `install` {`url`/`dynamic-url`, `version`, `set-default` (true), `set-next-boot` (false), `skip-reboot` (false), `pre-check`}; `remove` {`version`}; `upgrade-docker` {`url`/`dynamic-url`, `container-name`, `cleanup-image` (false), `enforce-check` (false), `tag`}; `reboot-on-success` |
| `snmp` | `community-ro`, `snmp-location` (at least one is required), `restart-agent` (false). Writes `/etc/sonic/snmp.yml` (`snmp_rocommunity:`, `snmp_location:`) |
| `connectivity-check` | `ping-hosts`, `ping6-hosts`, `retry-count` (12), `retry-interval` (5), `ping-count` (3), `deadline`, `timeout` |
| `graphservice` | `minigraph-url`, `acl-url` (each a `url`/`dynamic-url`). Writes `/etc/sonic/minigraph.xml`, `/etc/sonic/acl.json` |
| `download` | `files` (list of url objects), `curl-arguments` |
| `provisioning-script` | Not a file in `plugins/`. In practice it is a section whose `plugin` is a `url` to a script (see E.1) |

### E.7 `ztp` CLI (`src/usr/bin/ztp`)
- Commands: `disable`, `enable`, `erase`, `features`, `run` ("Restart ZTP"), `status`.
- Flags: `-c/--status-code`, `-v/--verbose`, `-y/--yes`, `-C/--config-json FILE`.
- The SONiC CLI wrappers are `show ztp status [--verbose]` and `config ztp enable|disable|run` (`SONIC-HLD`).
- `ztp status -c` prints one of: `0:DISABLED`, `1:INACTIVE`, `2:ACTIVE-DISCOVERY`, `3:NOT-STARTED`, `4:IN-PROGRESS`, `5:SUCCESS`, `6:FAILED`.
- `ztp status` (terse) example from `SONIC-HLD`:
  ```
  ZTP Admin Mode : True
  ZTP Service    : Inactive
  ZTP Status     : SUCCESS
  ZTP Source     : dhcp-opt67 (eth0)
  Runtime        : 05m 31s
  Timestamp      : 2019-09-11 19:12:24 UTC

  ZTP Service is not running

  01-configdb-json: SUCCESS
  02-connectivity-check: SUCCESS
  ```
  `ZTP Service` ∈ `Inactive`, `Processing`, `Active Discovery`.
- `--verbose` adds a `ZTP` banner plus `Error`, `Ignore Result`, `ZTP JSON Version`, and, per section, `Description`, `Status`, `Runtime`, `Timestamp`, `Exit Code`, `Error`, `Ignore Result`, `Halt on Failure`.

### E.8 Defaults on SONiC images
- Default user/password: `DEFAULT_USERNAME = admin`, `DEFAULT_PASSWORD = YourPaSsWoRd`. `CHANGE_DEFAULT_PASSWORD ?= n`, so no forced change by default (`sonic-buildimage/rules/config`).
- **ZTP is not built by default**: `# ENABLE_ZTP = y` is commented out in `rules/config`. The twin should treat ZTP presence as an image property.

---

## F. LLDP basics for emulation

Primary source is the lldpd source (`lldpd/lldpd@62e946403635`) plus its usage docs <https://lldpd.github.io/usage.html>. IEEE 802.1AB itself is paywalled and was not read directly.

- Destination MAC `01:80:c2:00:00:0e` (nearest bridge; `src/daemon/lldp-tlv.h` `LLDP_ADDR_NEAREST_BRIDGE`). The alternatives are `01:80:c2:00:00:03` (nearest non-TPMR bridge) and `01:80:c2:00:00:00` (nearest customer bridge). lldpd docs: "By default, the 01:80:c2:00:00:0e MAC address is used".
- EtherType `0x88cc` (`src/daemon/lldpd.h` `ETH_P_LLDP`).
- Timers: `LLDPD_TX_INTERVAL 30` and `LLDPD_TX_HOLD 4`, so `LLDPD_TTL` = 120 s (`src/daemon/lldpd.h`). lldpd docs: "The default value is 4 and therefore the default TTL is 120 seconds." That these equal the 802.1AB `msgTxInterval`/`msgTxHold` defaults is **UNCONFIRMED against the IEEE text**. Cumulus shows the same values (B).
- TLV types (`src/daemon/lldp-tlv.h`): 0 End, 1 Chassis ID, 2 Port ID, 3 TTL, 4 Port Description, 5 System Name, 6 System Description, 7 System Capabilities, 8 Management Address, 127 Org-specific. OUIs are 802.1 `00-80-C2`, 802.3 `00-12-0F`, MED `00-12-BB`, DCBX `00-1B-21`.
  - 802.1 subtypes: 1 PVID, 2 PPVID, 3 VLAN name, 4 Protocol identity.
  - 802.3 subtypes: 1 MAC/PHY, 2 Power, 3 Link aggregation, 4 Max frame size.
- Chassis ID subtypes (`src/lldp-const.h`): 1 chassis component, 2 interface alias, 3 port component, **4 MAC address**, 5 network address, 6 interface name, 7 locally assigned. lldpd uses the first interface MAC, so subtype 4 (usage docs).
- Port ID subtypes: 1 interface alias, 2 port component, 3 MAC address, 4 network address, **5 interface name**, 6 agent circuit ID, **7 locally assigned**. lldpd defaults to MAC (subtype 3), but Cumulus defaults `portidsubtype ifname` (subtype 5) (B).
- Mandatory TLVs: Chassis ID, Port ID, TTL, End. This is standard 802.1AB knowledge; **the IEEE text was not read, so it is UNCONFIRMED here**. lldpd's usage page does not list mandatory TLVs.
- For a Cumulus-like neighbour, send: Chassis ID (sub 4, base MAC), Port ID (sub 5, `swpN`), TTL 120, Port Description, System Name (hostname), System Description, System Capabilities, Management Address (eth0 IP), plus 802.3 Link Aggregation and Max Frame Size (on by default per B), and End.

---

## UNCONFIRMED list

1. Cumulus: which DHCP options the switch-side dhclient requests beyond 239 (114/67 are not mentioned). No default URL is documented.
2. Cumulus: semantics of `CUMULUS-PROV-COUNT` / `CUMULUS-PROV-MAX`, which are documented by example values only.
3. Cumulus: `CUMULUS-LICENSE-INSTALLED` and `CUMULUS-PROD-ID` are absent from the 5.0 and 5.14 header tables; whether they existed in 4.x or earlier was not checked.
4. Cumulus: ZTP script retry count. The log shows "Retrying" but no number is documented.
5. Cumulus: meaning of `ztp -b` (seen only in the systemd `ExecStart`). `ztp -i` is undocumented and may not exist.
6. Cumulus: whether single-dash `nv config apply -y` is accepted. The reference documents `--y` / `--assume-yes`.
7. Cumulus: exact first-login password-change prompt text.
8. Cumulus: whether the NVUE REST API listened on localhost only in early 5.x releases. 5.14 says all addresses unless `listening-address` is set.
9. Cumulus: whether ptmd is enabled by default in 5.x.
10. Cumulus: the default VRF for ntpd, and the verbatim `nv set service ntp mgmt server …` form (inferred from the `<vrf>` grammar).
11. Cumulus: the exact release in which `nv set system timezone` became `nv set system date-time timezone`, and in which `nv set service syslog <vrf>` became `nv set system syslog server` (5.13 already uses the new syslog form).
12. Cumulus: exact semantics of `nv config detach`. The two NVIDIA docs disagree (discard vs. save under an integer ID).
13. ONIE: use of DHCP option 43 (requested but not parsed in `rootconf/default`).
14. ONIE: the format of the `ONIE-SECURITY-KEY` value, and the hex case of `ONIE-ETH-ADDR`.
15. ONIE: the ONIE-side platform string on NVIDIA-era Spectrum (SN5600). `x86_64-nvidia_sn5600-r0` is inferred from the SONiC device directory, since upstream ONIE `mlnx_x86` would derive `mlnx_*`.
16. LLDP: 802.1AB `msgTxInterval=30` / `msgTxHold=4` defaults and the mandatory-TLV set, confirmed only via lldpd source and docs, not the IEEE text.

## Addendum (2026-11-12): the UNCONFIRMED points checked again

A second pass over NVIDIA's documentation, the Cumulus Linux 5.4 to 5.15 pages and NVIDIA's own
`enterprise-ras` net-configurator nc-v8.0.0 (MIT), whose code is NVIDIA's real use. The verdicts:

| # | Point | Verdict | Evidence |
|---|---|---|---|
| 1 | DHCP options beyond 239 | still unconfirmed (partial) | CL 5.14/4.4 ZTP pages: option 60 `cumulus-linux x86_64` is sent; 114/43/67 not mentioned |
| 2 | PROV-COUNT / PROV-MAX semantics | still unconfirmed | only example values (0/32) in the header tables |
| 3 | LICENSE-INSTALLED / PROD-ID in 4.x | refuted for 4.4 | absent from 4.4's header table too (3.x not checked) |
| 4 | ZTP retry count | still unconfirmed | only "Retrying" log lines |
| 5 | `ztp -b` / `-i` | still unconfirmed | `-b` only in the unit's ExecStart; `-i` not documented |
| 6 | `nv config apply -y` | confirmed in NVIDIA's code | the docs list `--y`/`--assume-yes`; net-configurator's ZTP template runs `-y`, its push `--assume-yes --confirm` then `--confirm-yes` |
| 7 | the password-change prompt's text | still unconfirmed | 5.13 Quick Start says only that it prompts and then disconnects |
| 8 | NVUE API localhost-only in early 5.x | confirmed | ≤5.5 `listen localhost:8765 ssl;` (edit `nvue.conf`); 5.6+ all addresses |
| 9 | ptmd enabled by default | still unconfirmed | 5.14 PTM page: starts only with `topology.dot` present |
| 10 | NTP's VRF form | form confirmed, default VRF unconfirmed | `nv set service ntp <vrf-id> server <id>` (≤5.14) |
| 11 | timezone and syslog releases | confirmed, corrected | timezone moved to `system date-time` in **5.15** (5.14 still `system timezone`; this review's "5.14+" was wrong); syslog to `system syslog` in **5.13** |
| 12 | `nv config detach` | confirmed | pending changes move to a numbered detached config (integer ids since 5.5) |
| 13 | ONIE option 43 | still unconfirmed | the ONIE user guide names only options 60 and 125 |
| 14 | ONIE-SECURITY-KEY format, ETH-ADDR case | still unconfirmed | not on the user-guide page |
| 15 | the SN5600's ONIE platform string | partly confirmed (third party) | a Dell KB shows a search for `onie-installer-x86_64-nvidia_sn5600-r0`, but its banner says `x86_64-mlnx_x86-r0` |
| 16 | LLDP defaults, mandatory TLVs | still unconfirmed | IEEE 802.1AB not read (tcpdump's dissector accepts the twin's frames) |

Also confirmed from net-configurator: **NVUE changed substantially in 5.15**. Feature switches
became `state enabled|disabled`, interface addressing moved to `ipv4`/`ipv6`, and `service ntp`
and `service dns` became `system ntp` and `system dns`. NVIDIA's templates do not work on 5.14
or earlier. The twin now emulates both grammars, selected by
`spec.adapters.network.nos.cumulusVersion`.

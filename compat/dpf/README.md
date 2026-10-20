# DOCA Platform Framework (DPF) against the twin

NVIDIA's [DOCA Platform Framework](https://github.com/NVIDIA/doca-platform), the Kubernetes
operator of NVIDIA DSX OS that provisions BlueField DPUs and runs DPU services such as HBN, built
from its released tag `v26.8.0` (`f1b50e8`) and run **unmodified** in **zero-trust mode**: DPF
reaches nothing but the DPUs' own BMCs, over Redfish. It discovers the twin's two BlueField-3 BMCs
on the management network, takes them over (factory reset, credentials, BMC firmware, mutual TLS
with its own CA), and provisions them with the HBN guide's BFB and DPU flavor, up to the point
where a real DPU's operating system would have to boot. At `model` fidelity no DPU OS runs
([Q1](../../docs/spec/11-decisions.md#q1)), so DPF stops there, waiting for its DPU agent, and this
harness records that as `CANNOT`. With `DPU=vm` one DPU's Arm is a QEMU machine
([D52](../../docs/spec/11-decisions.md#d52)): the BMC's push installs the BFB on it, its DPU OS
boots and runs DPF's first boot, and DPF's own DPU agent reports to DPF. With the guide's flavor
the agent stops at its first operation that needs the DPU's ConnectX; told by the flavor to skip
those, it joins the DPU cluster and DPF puts the DPU in service, Ready
([below](#result-with-dpuvm-2026-10-20)). HBN is not deployed. `q1.sh` boots the same BFB under
QEMU apart from the twin.

> **Not impersonated.** Nothing here stands in for a DPF component or a DPU agent. The twin serves
> the BMCs (and, at `vm`, runs the Arm's machine, on which only NVIDIA's software and DPF's runs);
> everything on the other side of Redfish is DPF's own code or what DPF's operator would have
> deployed (named in the table below).

## What runs

| Process | Source | Role |
|---|---|---|
| `etcd`, `kube-apiserver` 1.37.0, twice | envtest's binaries | the host cluster DPF runs in, and the DPU cluster DPUs would join (DPF's `static` DPUCluster). Each serves a certificate from a CA of the run, because DPF computes the DPUs' `kubeadm join --discovery-token-ca-cert-hash` from the CA in the DPU cluster's kubeconfig |
| `dctwin up estate.yaml --adapters redfish,dhcp,bluefield` | this repository | one rack of two GH200 servers, each with a BlueField-3 whose BMC leases on the management network from the twin site DHCP; the `bluefield` adapter in front of both BlueField BMCs |
| DPF `provisioning` controller | `cmd/provisioning` at `v26.8.0`, `go build` | discovery, the DPUDevice, DPUNode, DPUSet, DPU, BFB, DPUCluster and DPUNodeMaintenance controllers, with `--dpu-install-interface=redfish --deployment-mode=zero-trust`. Its Redfish clients reach the BMCs through the twin's site gateway (`HTTPS_PROXY`) |
| DPF `static-cluster-manager` | `cmd/static-cluster-manager` at `v26.8.0` | makes the DPUCluster Ready from its admin kubeconfig |
| cert-manager controller | `cert-manager/cert-manager` `v1.21.2`, `cmd/controller` | issues what DPF's operator asks it for: DPF's CA, the Redfish client certificate (CommonName `root`), the bfb-registry certificate, and the **BMC server certificates from the CSRs the BMCs generate** |
| `bfbregistry` | `bfbregistry/` here | DPF's bfb-registry is a pod (nginx with the http-concat module, `Dockerfile.bfb-registry`); with no kubelet the pod never runs, so this serves DPF's `/bfb` with the same URL scheme (`/bfb/??<bfb>,bfcfg/<dpu>.cfg?/bfb-to-install`, the BFB with the DPU's bf.cfg appended) and the certificate DPF's issuer signed for it |
| `kube-controller-manager` (both clusters), `kube-scheduler` and `kubeadm init phase` (DPU cluster), 1.37.0 | Kubernetes' release binaries; `DPU=vm` only | what a joining DPU needs of its clusters: the host cluster signs the DPU agent's client certificate; the DPU cluster signs bootstrap tokens, approves and signs kubelet certificates, and carries kubeadm's uploaded configuration and `cluster-info` |
| `dpu-agent` 26.8.0, arm64 | DPF's `make deb-dpuagent` at the tag, in a rootless `golang` container (`lib/agent-deb.sh`); `DPU=vm` only | the package DPF's registry image carries, served at the registry's `/deb` with an apt index |
| The DPU's Arm | QEMU aarch64 run by the twin's `bluefield` adapter (D52); `DPU=vm` only | server-01's BlueField: installed by its BMC's BFB push with the BFB's own installer, then booting the DPU OS that installer wrote |

What DPF's operator would have done and the harness does instead, each from the operator's own
manifests or code at the tag: the provisioning controller's Issuers and Certificates
(`manifests/01-certificates.yaml`, from `internal/operator/inventory/manifests/provisioning-controller.yaml`),
the `dpf-ca-trust-bundle` ConfigMap with its `bundle-hash` (as `ca_trust_bundle.go` computes it), a
pod's service-account CA at `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` (DPF writes it
into each DPU's bf.cfg for the DPU agent), and the webhook server's certificate (DPF's webhooks are
not registered: no operator installs their configurations). The DPF objects are the HBN guide's,
taken from the clone (`docs/public/user-guides/zero-trust/use-cases/hbn/manifests`):
`operatorconfig.yaml`, `dpudiscovery.yaml`, `bfb.yaml` and `hbn-dpuflavor.yaml`, plus the DPUSet
the guide's DPUDeployment makes (`manifests/05-dpuset.yaml`: the DPUDeployment's services and chains
need DPUs that have joined the DPU cluster, so DPF's dpuservice controller is not run) and a static
DPUCluster (`manifests/03-dpucluster.yaml`, as DPF's own e2e objects define one). The guide's one
manual step before OS installation, releasing the node-effect hold
(`kubectl annotate dpunodemaintenances … wait-for-external-nodeeffect=false`), is run by the harness.

DPF's provisioning controller keeps BFBs and bf.cfg files under `/bfb`, an absolute path no flag
changes. It runs as an ordinary process with `$WORK/bfb` (and the service-account directory)
mounted in a mount namespace of its own (`lib/nsroot.sh`, entered through rootless podman's user
namespace), so nothing is created at the host's root. DPF's webhook server listens on `:9443`,
which no flag changes either; the harness refuses to start if the port is taken.

## Running it

```bash
make compat-dpf                              # on Linux: needs Go, git, python3, openssl, rootless podman, envtest
KUBEBUILDER_ASSETS=…/k8s/1.37.0-linux-amd64 WORK=/var/tmp/dpf DCTWIN=$PWD/bin/dctwin compat/dpf/run.sh
BFB=test compat/dpf/run.sh                   # DPF's own 9 KB test-bfb.bfb instead of the guide's 1.7 GB BFB
KEEP_BFB=1 compat/dpf/run.sh                 # reuse the BFB an earlier run downloaded into $WORK/bfb
KEEP=1 compat/dpf/run.sh                     # leave everything running
DPU=vm KUBEADM=… KCM=… SCHEDULER=… compat/dpf/run.sh   # server-01's DPU at vm: its Arm runs the OS DPF installs
DPU=vm AGENT_SKIP=connectx … compat/dpf/run.sh          # and the DPU agent told, by the flavor, to skip the ConnectX's steps
```

`DPU=vm` ([D52](../../docs/spec/11-decisions.md#d52)) puts one DPU at `vm` fidelity: its Arm is a
QEMU aarch64 machine (4 vCPUs, TCG on an x86_64 host; 8 GB for the BFB's installer, 4 GB for the
DPU OS, a 256 MiB translation cache) that the BMC's BFB push installs and boots, so DPF's own DPU
OS, first boot and DPU agent run. On a host whose jobs are capped (the lab's 9G), the installer
runs with the clusters and DPF's controllers stopped (`lib/install-phase.sh`: when the BMC starts
streaming the BFB, they stop with their data kept; the twin and the registry stay; once the
installer has finished they start again on the same clusters), and the job's memory is logged by
phase (`evidence/memory.log`).

What the installer needs on such a host, measured with `q1.sh` (`INSTALL_ONLY=1`), the guide's 64K
BFB alone in a 9G scope (`systemd-run --scope -p MemoryMax=9G -p MemorySwapMax=0`), TCG on x86_64
with a 256 MiB translation cache (2026-10-20):

| Guest RAM | Result | QEMU's peak (anonymous) | Scope's peak |
|---|---|---|---|
| 8192 MB, anonymous | killed by the cap while extracting the DPU OS | 9,203 MiB | 9G |
| 7680 MB, anonymous | killed by the cap while extracting the DPU OS | 9,206 MiB (9,186 MiB) | 9G |
| 8192 MB, backed by a file on disk (`RAM_FILE`, QEMU `memory-backend-file`) | `INFO: Installation finished` in 1,040 s | 5,665 MiB (2,066 MiB) | 6.7G |

QEMU holds the guest's RAM (the installer's page cache fills it while it extracts the 1.5 GB DPU
OS), its own copy of the 1.6 GB initramfs `-initrd` loads, and the translation cache: about 2 GiB
over the guest's size, so anonymous guest RAM cannot reach the 8 GB the installer installs with
under a 9G cap. File-backed, the guest's pages are written back to the file under the cap instead;
the Arm's installer runs that way (D52). It needs `qemu-system-aarch64` with
edk2, `xz` and e2fsprogs' `debugfs`, and Kubernetes v1.37.0's `kubeadm`, `kube-controller-manager`
and `kube-scheduler` (the envtest API server's version). It builds DPF's `dpu-agent` package with
`lib/agent-deb.sh` (rootless podman; `DEB_DIR` reuses one) and serves it at the registry's `/deb`.
The stack's services then have site addresses the twin routes (`siteServices`): the host
cluster's API at 10.90.0.10:443, the DPU cluster's at 10.90.0.11:443 and the bfb-registry at
10.90.0.12:443, which DPF writes into the DPU's bf.cfg and reaches itself through the site
gateway. The harness applies DPF's `config/provisioning/rbac/dpuagent_bootstrap.yaml` with DPF's
CRDs, as DPF's operator does: without it the agent's bootstrap identity
(`system:bootstrappers:dpf:dpu-agent`) may not create the certificate signing request it
authenticates with, which a first run found.

Linux only: DPF's controllers are Linux programs (its DPU agent does not build elsewhere, and the
provisioning controller needs `/bfb`). `BFB=real` (the default) has DPF download the HBN guide's
BFB, `bf-bundle-3.5.0-89_26.07_ubuntu-24.04_64k_prod.bfb` (1,677,035,416 bytes, NVIDIA's licence;
never committed, D13), when the disk has 50 GB to spare after it; otherwise it falls back to
`BFB=test`. The BMC firmware package DPF pushes to a BlueField-3 BMC older than `BF-24.10-17`
(`bf3-bmc-24.10-17_opn.fwpkg`, 67,105,977 bytes, the one DPF's own `dpf-system` image bakes in) is
downloaded and its sha256 checked. `$WORK/evidence` keeps each DPU BMC's request log
(`twin-events-*.md`, below), the DPUDevice and DPU objects, and the twin's fidelity report.

## Result (2026-10-19, on an x86_64 Linux host)

DPF `v26.8.0` (`f1b50e85`), cert-manager `v1.21.2`, envtest 1.37.0, `BFB=real`:

| # | Check | Observed | Result |
|---|---|---|---|
| S1 | cert-manager issues DPF's CA, the Redfish client certificate and the bfb-registry certificate | three secrets in 0.3 s | PASS |
| T1 | The twin's BMCs lease on the management network | 6 BMCs in 10.30.0.11–10.30.0.16, 2 of them BlueField | PASS |
| D1 | DPUDiscovery crawls the range through the site gateway | a DPUDevice and a DPUNode per BlueField BMC, named by the serial the twin derived (`mt1565361tbu`, `mt4936658cqq`), in 2.6 s | PASS |
| D2 | The host BMCs and PDUs in the range | 4 other BMCs probed (DPF tries the factory password on each, is refused, and moves on); none adopted | PASS |
| D3 | BMC factory reset on initialization (DPF's default policy) | `ResetToDefaults` answered by both BMCs; each reboots and comes back on its factory password with `PasswordChangeRequired` (403 on Managers), which DPF treats as the reset having landed: `BMCFactoryResetReady True/FactoryResetCompleted` | PASS |
| D4 | The factory password replaced by the shared secret's | `BMCCredentialsReady True`; 4 password PATCHes (before and after the reset) | PASS |
| D5 | BMC firmware older than DPF's minimum | DPF pushes NVIDIA's `bf3-bmc-24.10-17_opn.fwpkg` (octet-stream POST to UpdateService) to each BMC; the BMC reads it as a PLDM package, its task completes and `BMC_Firmware` reports `BF-24.10-17` (was the capture's `BF-24.10-10`) | PASS |
| D6 | Mutual TLS | each BMC: DPF's CA installed in its Truststore, a CSR from a key the BMC generated, signed by cert-manager, installed with `ReplaceCertificate` and served; TLS client authentication on; DPF's verified mTLS client (CA-pinned, the BMC's address as identity, client certificate CN `root`) then reaches Managers: `Initialized True` | PASS |
| D7 | Discovery over mTLS | `Discovered True`: BlueField3, OPN `900-9D3B6-00CV-AA0`, PSID `MT_0000000884`, PF0 MAC, DPU mode | PASS |
| D8 | Server certificate and CA trust bundle reconciled over mTLS | `BMCServerCertificateReady`, `CATrustBundleReady True` | PASS |
| P1 | DPF downloads the BFB and reads its versions | Ready in 515 s; DOCA 3.5.0 | PASS |
| P2 | The DPUSet's DPUs hold at Node Effect (`nodeEffect.hold`, as the guide's DPUDeployment sets) | 2 DPUs, a DPUNodeMaintenance each; released as the guide releases it | PASS |
| P3 | InitializeInterface over mTLS | DPU mode, Secure Boot, DPU type and the capacity check of the HBN flavor (16 cores, 32 GiB): `InterfaceInitialized True` | PASS |
| P4 | ConfigFWParameters | host rshim disabled (`HostRshim.Set`), the BMC's rshim enabled (`Oem/Nvidia` PATCH) and read back: `FWConfigured True` | PASS |
| P5 | PrepareBFB | a bf.cfg per DPU with a `kubeadm join` for the DPU cluster; a bootstrap token per DPU in the DPU cluster | PASS |
| P6 | OS Installing: `SimpleUpdate` of the BFB to `DPU_OS` | each BMC fetches `https://127.0.0.1:…/bfb/??…bfb,bfcfg/…?/bfb-to-install` from the registry, reads all 49 images of the 1.7 GB boot stream with their CRCs and the DPU's bf.cfg after it, its task reaches 100%, and it resets the DPU's Arm into the stream: `BFBTransferred True` | PASS |
| P7 | OS installed: the DPU agent on the new DPU OS reports | `OS Installing`, `OSInstalled False/OSNotRunning: Waiting for DPU OS to finish booting and start dpu-agent` | CANNOT |
| P8 | The DPU joins the DPU cluster; HBN deployed there | 0 Nodes in the DPU cluster | CANNOT |

18 checks: 16 PASS, 0 FAIL, 2 CANNOT. Since this run a pushed BMC image runs only after the BMC's
reset, which DPF requests once the task completes (D5 in the `DPU=vm` runs below). `BFB=test` (DPF's
test BFB, which is a JSON inventory and not a boot stream): the same up to P5; at P6 each BMC
refuses the stream at its first bytes (`not a BlueField boot stream`), the task goes to Exception,
DPF retries and puts the DPUs in Error.
Times are from one run and vary.

### What DPF asked of one DPU BMC

From `evidence/twin-events-…server-01_bf3-0.md` (repeated reads folded; `inventory.read` of the
service root, Managers and Systems left out):

| seq | request | canonical | answer |
|---|---|---|---|
| 35 | `PATCH AccountService/Accounts/root` (factory password, from discovery) | `account.password` | 200 |
| 37 | `GET Chassis/Card1` (serial, OPN) | `inventory.read` | 200 |
| 48 | `POST Managers/Bluefield_BMC/Actions/Manager.ResetToDefaults` | `bmc.defaults` | 200; the BMC resets its Truststore, certificate and password and reboots |
| 73 | `GET Managers` as root / factory password | `account.read` | 403 PasswordChangeRequired |
| 78 | `PATCH AccountService/Accounts/root` | `account.password` | 200 |
| 81 | `POST UpdateService` (the 64 MB fwpkg) | `firmware.update.push` | 202; task 1; `firmware.bmc` BF-24.10-10 → BF-24.10-17 |
| 124–130 | `GET`/`POST Managers/Bluefield_BMC/Truststore/Certificates`, `POST CertificateService.GenerateCSR` | `certificate.read` · `certificate.install` · `certificate.csr` | 200 |
| 142 | `POST CertificateService.ReplaceCertificate` (cert-manager's certificate for the CSR) | `certificate.replace` | 200; `certificate.https` factory → installed |
| 144 | `PATCH AccountService` (`Oem.OpenBMC.AuthMethods.TLS`) | `account.policy` | 200 |
| 148–151 | over mTLS: `Systems/Bluefield/Oem/Nvidia`, `Chassis/Card1`, `FirmwareInventory/DPU_BOARD`, `NetworkDeviceFunctions/eth0f0` | `inventory.read` · `firmware.inventory.read` · `network.read` | 200 |
| 155–183 | `NetworkProtocol/HTTPS/Certificates/1`, the Truststore, `SecureBoot` (rotation and trust-bundle reconcilers) | `certificate.read` · `secureboot.read` | 200 |
| 326 | `POST Systems/Bluefield/Oem/Nvidia/Actions/HostRshim.Set` | `dpu.rshim.set` | 200 |
| 328 | `PATCH Managers/Bluefield_BMC/Oem/Nvidia` (`BmcRShim`) | `dpu.rshim.set` | 200 |
| 335 | `POST UpdateService/Actions/UpdateService.SimpleUpdate` (DPU_OS) | `firmware.update.simple` | 202; task 1000 |
| 340–356 | `GET TaskService/Tasks/1000` | `task.read` | 200; 0 → 100% as the stream arrives; `dpu.bfb` transferring → transferred; the Arm reset (`power.restart` over rshim) |

## Result with `DPU=vm` (2026-10-20)

The same host, each run whole in a 9G memory cap (`systemd-run --scope -p MemoryMax=9G -p
MemorySwapMax=0`), server-01's DPU at `vm` and server-02's at `model`. S1–P5 as above, all PASS
(D5 now with DPF's BMC reset, below). Two DPU flavors: the guide's as it is, and
`AGENT_SKIP=connectx`, the guide's with one more of its `configFiles`, a systemd drop-in that
starts DPF's DPU agent with `--skip-nvconfig --skip-reboot --skip-kernel-cmd-line --skip-sf-config
--skip-vf-mac --skip-ovs-raw-script` (flags of the agent's own; DPF's e2e flavor adds one the same
way). Then:

| # | Check | The guide's flavor | `AGENT_SKIP=connectx` |
|---|---|---|---|
| I1 | The BFB's installer with the stack down (the memory rule): when the BMC starts streaming, the clusters, cert-manager and DPF's controllers stop; the twin and the registry stay | PASS: `INFO: Installation finished` and the stack back on the same clusters 1,478 s later; job 7,595 MiB of 9,216, the Arm's QEMU (8 GB, file-backed) 6,109 MiB | PASS: 1,488 s; job 7,287 MiB, QEMU 6,866 MiB |
| P6 | OS Installing: the BFB transferred | PASS (DPF polled the BMC's task once it was back) | PASS |
| A1 | The BMC's push installs the DPU OS on the Arm with DPF's bf.cfg: the stream's own installer, `/etc/bf.cfg` from what followed the last image | PASS: `Running bfb_modify_os from bf.cfg`, `INFO: Installation finished`; `oob_net0` named by a link file for the MAC the BMC serves for the Arm's `oob0` | PASS, and the BFB's boot firmware staged at its ARM capsule (UEFI `4.16.0-48-g3e1cb60aa0`, ATF `4.16.0-8-gda930bca9`, BSP `4.16.0.14075`) |
| A2 | The DPU OS boots: GRUB's entry, `virt`'s early console ahead of its command line | PASS: `dpu-node-mt1565361tbu-mt1565361tbu login:` 138 s after the reset | PASS: 145 s |
| A3 | DPF's first boot installs the DPU agent | PASS: `oob_net0` leased 10.30.0.9 on its BMC's segment (the address stand-in D52 first had), DPF's CA trusted, `dpu-agent_26.8.0_arm64.deb` fetched from `https://10.90.0.12:443/deb` through the segment's gateway | PASS |
| A4 | The Arm's `oob_net0` and its BMC are two DHCP clients behind one switch port: the Arm leases from the twin site DHCP through the twin relay, the BMC keeps its lease (D52, amended) | not run (the check came with the amendment) | PASS (2026-10-21): `oob_net0` (`5c:25:73:7d:8a:49`) leased 10.30.0.200 from the twin site DHCP (10.30.0.2) through the relay of `w01-mgmt-switch:eth4`; the BMC on 10.30.0.14 at T1 and at the end; the relay delivered 21 replies on the shared port by chaddr, 16 to the Arm and 5 to the BMC (its renewals among them) |
| P7 | The DPU agent reports to DPF: its bootstrap CSR approved by DPF and signed by the host cluster, `lastStartupTime` set | PASS: OS Installing → DPU Config, 345 s after the Arm's reset into its OS | PASS: 345 s |
| P8 | The DPU joins the DPU cluster as a Node | FAIL: no Node. The agent's 17th operation, `nvconfig`, fails (`no physical ports from devlink (p0/p1)`) and is retried for good; DPF waits at `DPUConfig: waiting for DPU agent to report reboot method` | PASS: DPF's `kubeadm join` at 10.90.0.11:443; `dpu-node-mt1565361tbu-mt1565361tbu` Ready, kubelet v1.34.11, containerd 2.2.1, Ubuntu 24.04.4, `6.8.0-1030-bluefield-64k` |
| R1 | DPF puts the DPU in service: DPU Cluster Config, Service Readiness, Node Effect Removal | CANNOT | PASS: phase **Ready**, 403 s after the Arm's reset into its OS |
| H1 | DPF deploys HBN as a DPUService on the DPU Node | CANNOT: no Node | CANNOT (below) |
| M1 | Memory with the DPU OS (4 GB) up beside the twin, both clusters, cert-manager and DPF's controllers | PASS: job 6,023 MiB, QEMU 3,569 MiB | PASS: job 6,060 MiB, QEMU 3,531 MiB |

`AGENT_SKIP=connectx`: 26 checks, 25 PASS, 1 CANNOT; re-run on 2026-10-21 after D52's amendment
(the Arm leasing through the twin relay, the site gateway routing the stack's services), 27 checks
with A4, 26 PASS, 1 CANNOT (H1), phase Ready, the job's peak 6,023 MiB with the DPU OS up. Memory is sampled every 5 s; the cgroup's
own peaks, which include page cache the kernel reclaims at the cap, were 8,752 MiB and 7,291 MiB,
and neither run met the cap (`memory.events`: `max 0`, no OOM kill). The guide's-flavor column
is two runs: the later ran without intervention and was stopped once the agent stood at
`nvconfig`, its harness rows ending at A1 (its A2 waited on a login prompt a kernel message had
split, which the adapter now matches) and the rest read from DPF's objects; the earlier recorded
A2 and P7 itself and left the agent at `nvconfig` for ten minutes, but needed DPF's DPU agent
bootstrap RBAC applied by hand, which `run.sh` applies since.

**Where the guide's flavor stops.** The agent runs, in DPF's order, every operation that needs only
an operating system and a network: the kernel module, the netplan (`oob_net0`) and its check, the
startup report (P7), the DPU object, DNS, the static files, the built-in kubelet removed, sysctl
set and checked, the kernel command line written to GRUB, containerd, the DPU mode (13 conditions
True). The next, `nvconfig`, resolves the ConnectX's physical functions through devlink to apply
the flavor's NVConfig, and the Arm has no ConnectX. After it come the reboot that applies the
NVConfig, the kernel command line checked, the SFs, VF MACs and OVS-DOCA bridges, which need the
ConnectX again, and only then the kubelet and the join. The agent skips an operation only when a
flag tells it to, and the guide's flavor sets none.

**With the ConnectX's steps skipped**, the agent runs the rest a minute after it starts (the reboot
it reports as `NoAction`, the kubelet configured and started with DPF's `kubeadm join`, the node
labels, the host OS init release); the kubelet joins with its bootstrap token and gets its client
certificate from the DPU cluster. DPF then reads the DPU's UEFI and BSP versions from the BMC and
compares them with the BFB's: a first run stopped there (`DPU UEFI version 4.9.0-46-g7e3911bd4d
was not updated, expected 4.16.0-48-g3e1cb60aa0`, the BMC reporting the capture's boot firmware),
and the twin now models the installer's ARM capsule as firmware (D52). DPF labels the Node, deletes
the join tokens and the agent's bootstrap token, and the DPU is Ready. What this skips is exactly
what a DPU does with its NIC: the NVConfig and the reboot that applies it, the flavor's kernel
parameters (among them the hugepages OVS-DOCA uses), SFs, VF MACs and the OVS bridges.

**HBN.** The guide's `doca-hbn` 3.5.0 chart (`helm.ngc.nvidia.com/nvidia/doca`, 4,709 bytes) and
its image (`nvcr.io/nvidia/doca/doca_hbn:3.5.0-doca3.5.0`, arm64, 67 layers, 296 MB compressed)
are both fetched without an NGC account. DPF deploys it through its DPUService controller and Argo
CD v3.3.0, which DPF's operator installs and this harness does not run, so no DPUService exists.
Had they run, the pod could not have been placed: the guide's DPUServiceTemplate requests
`memory: 6Gi` and `nvidia.com/bf_sf: 4`; the Ready Node allocates 3,433,728 Ki and advertises no
`nvidia.com/bf_sf` (DPF's SF device plugin advertises the ConnectX's scalable functions). Its image
would also have to come from a registry the Arm can reach, and the twin routes the Arm to the
stack's site services and the estate's endpoints only. Memory for it: beside the Arm the stack
took about 2.5 GiB of the cap, so a DPU OS with HBN's 6Gi on top would need a larger guest,
file-backed as the installer's is.

Found along the way, none of them DPF failing: DPF resets a BlueField BMC when its firmware task
completes and reads the version again within about 100 ms; the BMC has not gone down yet, the
version is the old one, and DPF pushes the package a second time (one of the two BMCs in each
`AGENT_SKIP` run, the runs since the image waits for the BMC's reset: `3 pushes accepted, 2 BMC
resets` in D5), which a BMC still up accepts. The BFB's DPU OS asks `192.168.100.1:53`, tmfifo's
host address on a BlueField, for DNS (refused: there is no rshim network). And the discovery range
is taken from the leases at the moment both BlueField BMCs have one, so a host BMC that leases later
can fall outside it (D2 then counts one BMC fewer).

## What the twin added

The `bluefield` adapter (`internal/adapter/bluefield`): what a BlueField BMC (OpenBMC's bmcweb)
does that the Redfish engine's generic service did not, found by running DPF:

- **Status codes.** bmcweb answers a successful write with 200 and a Base Success message where the
  engine answered 204; DPF requires 200 for EnableMTLS, InstallCert, ReplaceCertificate,
  HostRshim.Set and the rshim PATCH.
- **ResetToDefaults** resets what the BMC holds and reboots it; the engine logged the request and
  changed nothing, which left DPF waiting forever for a factory password that never came back.
- **Certificates and mutual TLS:** a Truststore (ten slots), CSRs from keys the BMC generates (a
  pending key dies with a BMC reboot, as DPF expects), ReplaceCertificate checked against the held
  key and then served on the BMC's HTTPS, and client certificates verified against the Truststore
  and mapped to an account by CommonName.
- **The BFB push:** SimpleUpdate to `DPU_OS` fetches the image, checks the boot stream image by
  image as it streams, reports the transfer as task progress, refuses a second update while one
  runs (`Another update is in progress`), and resets the Arm into the stream. **BMC firmware**
  pushed to the HttpPushUri (or multipart) is read as a DSP0267 PLDM package and staged by the
  engine under the version its BMC image carries.
- **OEM state:** HostRshim, BmcRShim and Mode written through the BMC read back.
- **The Arm at `vm`** (D52): a QEMU aarch64 machine that the BMC's push installs with the BFB's
  own installer (the bf.cfg handed over where the boot FIFO would put it) and that boots GRUB's
  entry with `virt`'s UART as its early console; its out-of-band NIC on the BMC's switch port,
  named `oob_net0`, a second DHCP client behind that port that leases from the twin site DHCP
  through the twin relay (the relay delivers each reply by chaddr within the port, so the BMC
  keeps its own lease); the stack's site services routed by the site gateway, which routes them for
  every stack process; and the BFB's boot firmware (UEFI, ATF, BSP) staged when the installer reports
  its ARM capsule written and reported by the BMC after the Arm's next reset. A BMC upload still
  arriving when the BMC goes down for a reset is cut off with it.

The engine changed in one place: a BMC component staged `applyOn: reset` is applied when the BMC
itself reboots (DPF resets the BMC once the update task completes), where it had been applied only
at a reset of the host. Until it was, the BlueField-3 profile applied its BMC image at the task's
completion; it is `applyOn: reset` again.

## Where DPF stops, and what it would take

After the transfer DPF waits for its DPU agent (`cmd/dpuagent`, a Linux program installed by the
DPU's bf.cfg) to report from the new DPU OS; only then does it configure the DPU, wait for it to
join the DPU cluster as a Node and deploy DPU services such as HBN there. At `model` the twin runs
no DPU OS, so DPF stops at the transfer. At `vm` (`DPU=vm`, [above](#result-with-dpuvm-2026-10-20))
the DPU OS runs and the agent reports; what stops it then is the ConnectX the Arm does not have.
With the ConnectX's steps skipped the DPU is Ready, and HBN would then need DPF's DPUService
controller and Argo CD, more memory than the Arm's 4 GB, and the ConnectX's SFs. `q1.sh` boots the very BFB DPF provisioned under QEMU, apart
from the twin, which is how the `vm` Arm was worked out;
[below](#q1-the-bfb-dpf-provisioned-under-qemu) is what it found, and
[Q1 in 11](../../docs/spec/11-decisions.md#q1) what is still missing.

## Q1: the BFB DPF provisioned, under QEMU

`q1.sh` takes the boot stream DPF pushed at P6 (the guide's 64K-page BFB) and runs it on QEMU's
aarch64 `virt` machine with UEFI (edk2), apart from the twin: first the BFB's own Linux and
installer initramfs against an empty 40 GB NVMe disk, then the installed disk. Nothing in the BFB
is changed; booting the installed DPU OS needs one kernel argument changed (below), which the
script does only when asked and says so.

```bash
Q1WORK=/var/tmp/q1 BFB_FILE=$WORK/bfb/dpf-operator-system-bf-bundle-v26.8.0.bfb compat/dpf/q1.sh
BOOT_ONLY=1 BOOT_EARLYCON=pl011,0x9000000 Q1WORK=/var/tmp/q1 compat/dpf/q1.sh   # boot what it installed
BFCFG=$WORK/bfb/bfcfg/<dpu> BOOT_EARLYCON=pl011,0x9000000 Q1WORK=… BFB_FILE=… compat/dpf/q1.sh
```

On x86_64 that is TCG (QEMU 10.2, `-cpu neoverse-n1`, 4 vCPUs, 8 GB): slow, and the timings below
are wall-clock seconds on a 4-core laptop-class CPU (2026-10-20). On Apple silicon HVF runs the
4K-page build of the same bundle (its kernel reaches its root mount in 0.4 s) but not the guide's
64K-page one: Apple silicon has no 64 KB translation granule.

| Stage | Observed | Seconds |
|---|---|---|
| edk2 → the BFB's kernel (`6.8.0-1030-bluefield-64k`) | EFI stub, Linux | 10 |
| initramfs unpacked, `/init` | needs 8 GB of guest memory: the installer unpacks its 1.6 GB initramfs into a RAM file system of half the memory; 5 and 6 GB fail with `Initramfs unpacking failed: write error` | 25 |
| `install-ubuntu.service` probes the hardware | no IPMB on `i2c-1`, no MST device, no eMMC, no `RshimMacAddr` EFI variable, no boot FIFO (`mlx-mkbfb` finds no stream to dump): each fails and the installer carries on | 25–50 |
| the target: `/dev/nvme0n1`, partitioned (EFI + ext4), DPU OS extracted | 1.5 GB `image.tar.xz` | 55–597 |
| the DPU OS's initramfs rebuilt, GRUB (shim) installed for arm64-efi, `grub.cfg`, boot entry `ubuntu0` written to the UEFI variables | | 597–747 |
| after the OS: ATF/UEFI capsule (`bad block device /dev/mmcblk0`, reported as passed), BMC components (skipped: no BMC credentials in the default bf.cfg), NIC firmware ("the same as provided"), then `INFO: Installation finished`, reboot | | 747–777 |
| the installed disk | 6.5 GiB | |

The installed DPU OS, booted from that disk (UEFI variables as the installer left them):

| Stage | Observed | Seconds |
|---|---|---|
| unmodified: edk2 → the installer's `ubuntu0` entry → shim → GRUB (2 s countdown) → the kernel's EFI stub exits boot services | then nothing, with one vCPU busy, until stopped | 10, then silent |
| why | GRUB's command line has the BFB's `earlycon=pl011,0x01000000`, inside `virt`'s firmware flash; the pflash trace shows the kernel's first space taken for a CFI block erase, after which every read of the UART's flag register returns `0xA0`, "transmit FIFO full" | |
| `BOOT_EARLYCON=pl011,0x9000000`: GRUB's kernel, initramfs and command line with that one argument changed | Linux | 5 |
| the DPU OS's initramfs, its systemd, the root file system | | 15–25 |
| `Ubuntu 24.04.4 LTS localhost.localdomain ttyAMA0`, `login:` | | 111 |
| the BFB's own cloud-init (its default user-data): enables containerd and kubelet, imports the bundled images (`doca_telemetry`, `pause`) | finished at 351 s of uptime | 361 |
| failed units | `mlx_ipmid` (IPMI towards the BMC), `systemd-networkd-wait-online`, SPDK's `vhost`; `bfvcheck` finds no firmware package and no eMMC; the only network device is QEMU's `enp0s2`: no `oob_net0`, no `tmfifo_net0`, no ConnectX `p0`/`p1` | |

With **DPF's bf.cfg** (`BFCFG=`, the one DPF rendered at P5 for `dpu-node-mt1565361tbu`, handed to
the installer as `/etc/bf.cfg` in a second initramfs archive, where a BlueField's boot FIFO would
put it), the same installer runs DPF's hooks, and the DPU OS it installs runs DPF's first boot:

| Stage | Observed | Seconds |
|---|---|---|
| installer: the same steps, then DPF's `bfb_modify_os` | DPF's cloud-init (hostname, `oob_net0` by DHCP, DPF's CA, the DPU agent's configuration and bootstrap kubeconfig, `install-dpu-agent.sh`) written into the new OS | 747 |
| installer: DPF's `pre_bmc_components_update` and the BMC firmware flow | `ipmitool` finds no `/dev/ipmi0`; the BMC flow stops at `mlxbf_gige driver is not loaded` (it reaches the BMC over the DPU's out-of-band port); `INFO: Installation finished` | 753–763 |
| the DPU OS boots (`earlycon` changed as above) | `dpu-node-mt1565361tbu-mt1565361tbu login:` | 101 |
| DPF's cloud-init | DPF's CA added to the trust store; `install-dpu-agent.sh`: `apt-get install dpu-agent` from `https://127.0.0.1:46147/deb`, the registry address DPF wrote (the harness's registry, on the host's loopback), refused 60 times | 120–1232 |
| the end | `failed to install dpu-agent after 60 attempts`; `cloud-final.service` failed; no DPU agent, so nothing reports to DPF | 1240 |

So the next thing a DPU needed was concrete: the bfb-registry reachable from the DPU (through
`oob_net0`, which DPF's netplan expects and QEMU's `virtio-net` is not), a `/deb` repository holding
DPF's arm64 `dpu-agent` (DPF builds it from its source, `make deb-dpuagent`, into its registry
image), and the host cluster's API for the agent's bootstrap kubeconfig. `DPU=vm` supplies all
three.

Worth knowing before reading an older log: grub-install prints `Installation finished. No error
reported.` twelve minutes in, and a first version of this script stopped there, leaving a disk whose
GRUB has no configuration (a bare `grub>`); and edk2 for aarch64 on QEMU 10.2's TCG prints nothing
past its banner with `-cpu max`, which is why the script uses `neoverse-n1`.

## What this does not show

- At `model`: the DPU agent, the DPU cluster join and HBN (P7, P8). At `vm`: anything a DPU does
  with its ConnectX. The Arm is QEMU's `virt` machine, so the agent stops at `nvconfig` with the
  guide's flavor, and `AGENT_SKIP=connectx` reaches a Ready DPU only by skipping the NVConfig, the
  reboot that applies it, the kernel parameters, SFs, VF MACs and OVS-DOCA bridges.
- HBN, or any DPU service: no DPUService is deployed (H1).
- That the DPU OS boots on a BlueField as it does here: the Arm boots with `virt`'s UART ahead of
  GRUB's command line, gets its bf.cfg in an initramfs archive rather than through the boot FIFO,
  and runs QEMU's edk2 whatever boot firmware the BMC reports.
- Host-trusted mode: DPF's host agent runs as a pod on each host, which needs kubelets.
- DPF's operator, webhooks, Kamaji DPU clusters, the dpuservice controller and ArgoCD.
- That BlueField firmware is safe to update: the twin reads the BMC's package and the BFB's
  versions, and flashes nothing.
- Anything about BlueField-4 (DPF's BF4 path uses ISOs, virtual media and other OEM resources).

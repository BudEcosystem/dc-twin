# shellcheck shell=bash
# The checks of compat/dpf/run.sh, in the order DPF provisions a zero-trust DPU. Sourced by
# run.sh once DPF is running; uses its hk/dk, twin, record and wait_until.

NS=dpf-operator-system
cond() { # kind name type -> status/reason
	hk get "$1" -n $NS "$2" -o "jsonpath={.status.conditions[?(@.type==\"$3\")].status}/{.status.conditions[?(@.type==\"$3\")].reason}" 2>/dev/null
}
cond_msg() { hk get "$1" -n $NS "$2" -o "jsonpath={.status.conditions[?(@.type==\"$3\")].message}" 2>/dev/null; }
devices() { hk get dpudevices -n $NS -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null; }
dpus() { hk get dpus -n $NS -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null; }
all_devices() { # type status
	local d n=0 ok=0
	for d in $(devices); do
		n=$((n + 1)) || true
		[ "$(cond dpudevice "$d" "$1" | cut -d/ -f1)" = "$2" ] && ok=$((ok + 1))
	done
	[ "$n" -ge 2 ] && [ "$ok" = "$n" ]
}
all_dpus() { # condition type status
	local d n=0 ok=0
	for d in $(dpus); do
		n=$((n + 1)) || true
		[ "$(cond dpu "$d" "$1" | cut -d/ -f1)" = "$2" ] && ok=$((ok + 1))
	done
	[ "$n" -ge 2 ] && [ "$ok" = "$n" ]
}
phases() { hk get dpus -n $NS -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' 2>/dev/null | sort | uniq -c | awk '{$1=$1; print}' | paste -sd, -; }
events() { python3 -I "$here/lib/events.py" "$TWIN_STATE/events.jsonl" "$1" "${2:-markdown}"; }
dpu_events() { local p; for p in $DPU_BMCS; do events "$p" text; done; }
twin_serials() { twin get '**/bf3-*' 2>/dev/null | awk '$1 ~ /\// {print tolower($NF)}' | sort | paste -sd' ' -; }

# ---------------------------------------------------------------- D: the DPU BMCs, taken over

two_devices() { [ "$(devices | grep -c .)" -ge 2 ]; }
ms=$(wait_until 180 two_devices) || true
got=$(devices | sort | paste -sd' ' -) || true
want=$(twin_serials) || true
record D1 "DPUDiscovery crawls $IP_RANGE_START–$IP_RANGE_END through the site gateway" \
	"a DPUDevice and DPUNode per BlueField BMC, named by its serial ($want); no other BMC adopted" \
	"DPUDevices [$got], DPUNodes $(hk get dpunodes -n $NS -o name 2>/dev/null | grep -c .), after ${ms} ms" \
	"$([ "$got" = "$want" ] && echo PASS || echo FAIL)"
other=$(for p in $(awk '$1 != "dpu" {print $3}' "$WORK/evidence/leases.txt"); do events "$p" text; done | grep -c "account.password" || true) || true
record D2 "The host BMCs and PDUs in the range" "DPF asks each for its factory password and leaves it (none is a BlueField)" \
	"$(awk '$1 != "dpu"' "$WORK/evidence/leases.txt" | grep -c .) other BMCs probed; $other password changes accepted on them" \
	"$([ "$other" = 0 ] && echo PASS || echo FAIL)"

ready_devices() { all_devices Ready True; }
ms=$(wait_until 300 ready_devices) || true
for d in $(devices); do hk get dpudevice -n $NS "$d" -o yaml >"$WORK/evidence/dpudevice-$d.yaml"; done
first=$(devices | head -1) || true
fr=$(cond dpudevice "$first" BMCFactoryResetReady) || true
record D3 "BMC factory reset on initialization (DPF's default policy)" "BMCFactoryResetReady True/FactoryResetCompleted on both; the BMC comes back on its factory password, which must be changed" \
	"$first: $fr; twin: $(dpu_events | grep -c 'bmc.defaults  200') ResetToDefaults answered, $(dpu_events | grep -c 'GET /redfish/v1/Managers  account.read  403') PasswordChangeRequired answers" \
	"$(all_devices BMCFactoryResetReady True && echo PASS || echo FAIL)"
record D4 "BMC credentials: the factory password replaced by the shared secret's" "BMCCredentialsReady True on both" \
	"$(cond dpudevice "$first" BMCCredentialsReady) ($(dpu_events | grep -c 'PATCH /redfish/v1/AccountService/Accounts/root  account.password  200') password PATCHes)" \
	"$(all_devices BMCCredentialsReady True && echo PASS || echo FAIL)"
bmc_fw() { # the version a BMC reports, as a stack reads it through the site gateway
	curl -sk -m 10 --proxy "http://$GATEWAY" -u "root:$BMC_ROOT_PASSWORD" "https://$1/redfish/v1/UpdateService/FirmwareInventory/BMC_Firmware" |
		python3 -I -c 'import json,sys; print(json.load(sys.stdin).get("Version","?"))' 2>/dev/null || echo "?"
}
fw=$(for ip in $(awk '$1 == "dpu" {print $2}' "$WORK/evidence/leases.txt"); do bmc_fw "$ip"; done | sort -u | paste -sd, -) || true
record D5 "BMC firmware older than DPF's minimum (BF-24.10-17): DPF pushes NVIDIA's bf3-bmc-24.10-17 package" \
	"the BMC reads the PLDM package and its task completes; DPF resets the BMC, which boots the image: BMC_Firmware reports BF-24.10-17" \
	"$(dpu_events | grep -c 'POST /redfish/v1/UpdateService  firmware.update.push  202') pushes accepted, $(dpu_events | grep -c 'Actions/Manager.Reset  bmc.reset') BMC resets; BMC firmware now $fw" \
	"$([ "$fw" = BF-24.10-17 ] && echo PASS || echo FAIL)"
installed=$(dpu_events | grep -c 'certificate.https  factory → installed' || true) || true
record D6 "Mutual TLS: DPF's CA in the BMC Truststore, a CSR from the BMC's own key signed by cert-manager, ReplaceCertificate, TLS auth on" \
	"Initialized True on both (DPF's verified mTLS client reaches Managers); each BMC serves the certificate DPF's issuer signed" \
	"Initialized $(cond dpudevice "$first" Initialized); $installed HTTPS certificates installed; $(dpu_events | grep -c 'certificate.csr  200') CSRs" \
	"$(all_devices Initialized True && [ "$installed" -ge 2 ] && echo PASS || echo FAIL)"
psid=$(hk get dpudevice -n $NS "$first" -o jsonpath='{.status.psid}' 2>/dev/null) || true
record D7 "Discovery over mTLS: chassis, DPU type, PSID, PF0 MAC" "Discovered True on both" \
	"$(cond dpudevice "$first" Discovered); dpuType $(hk get dpudevice -n $NS "$first" -o jsonpath='{.status.dpuType}'), PSID ${psid:-none}" \
	"$(all_devices Discovered True && echo PASS || echo FAIL)"
record D8 "BMC server certificate and CA trust bundle reconciled over mTLS" "BMCServerCertificateReady and CATrustBundleReady True" \
	"$(cond dpudevice "$first" BMCServerCertificateReady), $(cond dpudevice "$first" CATrustBundleReady)" \
	"$(all_devices BMCServerCertificateReady True && all_devices CATrustBundleReady True && echo PASS || echo FAIL)"

# ---------------------------------------------------------------- P: provisioning the DPUs

hk apply -f "$WORK/manifests/bfb.yaml" -f "$WORK/manifests/hbn-dpuflavor.yaml" -f "$WORK/manifests/dpuset.yaml" >/dev/null
bfb_ready() { [ "$(hk get bfb -n $NS "bf-bundle-$TAG" -o jsonpath='{.status.phase}' 2>/dev/null)" = Ready ]; }
ms=$(wait_until 1800 bfb_ready) || true
record P1 "BFB downloaded by DPF into its volume, versions read from the BFB" "BFB Ready" \
	"$(hk get bfb -n $NS "bf-bundle-$TAG" -o jsonpath='{.status.phase} {.status.fileName} DOCA {.status.versions.doca}' 2>/dev/null) after ${ms} ms" \
	"$(bfb_ready && echo PASS || echo FAIL)"
two_dpus() { [ "$(dpus | grep -c .)" -ge 2 ]; }
wait_until 120 two_dpus >/dev/null || true
held() { [ "$(hk get dpunodemaintenances -n $NS -o name 2>/dev/null | grep -c .)" -ge 2 ]; }
ms=$(wait_until 180 held) || true
record P2 "The DPUSet's DPUs reach Node Effect and hold (nodeEffect.hold, as the guide's DPUDeployment sets)" \
	"a DPUNodeMaintenance per DPU, waiting for wait-for-external-nodeeffect" \
	"$(dpus | grep -c .) DPUs ($(phases)); $(hk get dpunodemaintenances -n $NS -o name 2>/dev/null | grep -c .) DPUNodeMaintenances after ${ms} ms" \
	"$(held && echo PASS || echo FAIL)"
# the guide's next step, which an administrator runs: release the hold
hk annotate --overwrite dpunodemaintenances -n $NS --all provisioning.dpu.nvidia.com/wait-for-external-nodeeffect=false >/dev/null
iface() { all_dpus InterfaceInitialized True; }
ms=$(wait_until 300 iface) || true
d1=$(dpus | head -1) || true
record P3 "InitializeInterface over mTLS: mode, Secure Boot, DPU type, capacity for the HBN flavor (16 cores, 32 GiB)" "InterfaceInitialized True on both" \
	"$(cond dpu "$d1" InterfaceInitialized) after ${ms} ms" "$(iface && echo PASS || echo FAIL)"
fwc() { all_dpus FWConfigured True; }
ms=$(wait_until 300 fwc) || true
record P4 "ConfigFWParameters: host rshim disabled, the BMC's rshim enabled (the BFB push path)" "FWConfigured True on both" \
	"$(cond dpu "$d1" FWConfigured); twin: $(dpu_events | grep -c 'dpu.rshim.host') host-rshim and $(dpu_events | grep -c 'dpu.rshim.bmc') BMC-rshim changes" \
	"$(fwc && echo PASS || echo FAIL)"
prepared() { all_dpus BFBPrepared True; }
ms=$(wait_until 300 prepared) || true
tokens=$(dk get secrets -n kube-system -o jsonpath='{range .items[*]}{.type}{"\n"}{end}' 2>/dev/null | grep -c bootstrap.kubernetes.io/token || true) || true
record P5 "PrepareBFB: a bf.cfg per DPU with a kubeadm join for the DPU cluster" "BFBPrepared True on both; a bootstrap token per DPU in the DPU cluster" \
	"$(cond dpu "$d1" BFBPrepared); $tokens bootstrap tokens in the DPU cluster" "$(prepared && [ "$tokens" -ge 2 ] && echo PASS || echo FAIL)"

[ "$DPU" != vm ] || . "$here/lib/install-phase.sh"
transferred() { all_dpus BFBTransferred True; }
failed_transfer() { dpu_events | grep -q 'dpu.bfb  transferring → failed'; }
errored() { [ "$(hk get dpus -n $NS -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' 2>/dev/null | grep -c Error)" -ge 2 ]; }
settled() { transferred || errored; }
ms=$(wait_until "$RUN_SECONDS" settled) || true
for d in $(dpus); do hk get dpu -n $NS "$d" -o yaml >"$WORK/evidence/dpu-$d.yaml"; done
summary=$(dpu_events | grep -o 'dpu.bfb  transferring → [a-z]* ([^)]*' | head -1) || true
if [ "$BFB" = real ]; then
	record P6 "OS Installing: UpdateService.SimpleUpdate pushes the BFB (+ the DPU's bf.cfg from DPF's registry) to DPU_OS" \
		"the BMC fetches it, checks the boot stream image by image, its task reaches 100%: BFBTransferred True on both" \
		"$(cond dpu "$d1" BFBTransferred) after ${ms} ms; twin: ${summary:-no transfer}" "$(transferred && echo PASS || echo FAIL)"
else
	record P6 "OS Installing with DPF's test BFB (BFB=test: not a boot stream)" \
		"the BMC refuses it at the transfer (task Exception), DPF retries and stops" \
		"$(cond dpu "$d1" BFBTransferred): $(cond_msg dpu "$d1" BFBTransferred | cut -c1-160); twin: ${summary:-no transfer}" \
		"$(failed_transfer && echo PASS || echo FAIL)"
fi
if [ "$DPU" = vm ]; then
	. "$here/lib/checks-vm.sh"
else
	waiting=$(cond_msg dpu "$d1" OSInstalled) || true
	record P7 "OS installed: DPF waits for the DPU agent on the new DPU OS" "the DPU OS boots and its agent reports (no DPU OS is modelled: Q1)" \
		"DPUs $(phases); OSInstalled $(cond dpu "$d1" OSInstalled): ${waiting:-none}" CANNOT
	record P8 "DPU joins the DPU cluster as a Node; DPU services (HBN) deployed there" "Nodes in the DPU cluster, DPUs Ready" \
		"DPU cluster Nodes: $(dk get nodes -o name 2>/dev/null | grep -c .)" CANNOT
fi

# ---------------------------------------------------------------- evidence

hk get dpudevices,dpunodes,dpus,dpunodemaintenances,bfbs,dpuclusters,dpusets -n $NS -o wide >"$WORK/evidence/objects.txt" 2>&1 || true
for p in $DPU_BMCS; do
	n=$(echo "$p" | tr / _) || true
	events "$p" markdown >"$WORK/evidence/twin-events-$n.md"
done
twin report >"$WORK/evidence/fidelity-report.txt" 2>&1 || true

print_table "DPF $DPF_REF ($DPF_COMMIT) zero-trust provisioning against dc-twin (BFB=$BFB, DPU=$DPU${AGENT_SKIP:+, AGENT_SKIP=$AGENT_SKIP})"

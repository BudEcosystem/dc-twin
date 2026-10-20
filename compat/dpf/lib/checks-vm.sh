# shellcheck shell=bash
# The checks of DPU=vm after the BFB transfer (sourced by lib/checks.sh): one DPU's Arm (D52)
# installs the BFB DPF pushed, boots its DPU OS and runs DPF's first boot; DPF's DPU agent reports
# from it and joins the DPU cluster. The other DPU stays at model fidelity, where P7 and P8 cannot
# happen.

# vm_path, vm_serial, arm_dir, arm_events, arm_has and the memory sampler: lib/install-phase.sh
vm_dpu=$(dpus | grep -- "$vm_serial" | sed -n 1p) || true
agent_status() { hk get dpu -n $NS "$vm_dpu" -o 'jsonpath={.status.agentStatus.lastStartupTime}' 2>/dev/null; }
agent_reported() { [ -n "$(agent_status)" ]; }
installed() { arm_has "arm → installed" || arm_has "arm → install-failed"; }
logged_in() { arm_has "event → os.login"; }
fetched_agent() { grep -q 'GET /deb/.*dpu-agent_.*arm64.deb' "$WORK/logs/bfb-registry.log"; }
node_joined() { [ "$(dk get nodes -o name 2>/dev/null | grep -c .)" -ge 1 ]; }
# node_or_stuck: a Node joined, or the agent has stood at the same failing operation (a condition
# False with the same message) for AGENT_STUCK_SECONDS: it retries an operation the DPU cannot do
# (on the Arm, one that needs the ConnectX), and waiting longer shows nothing. Asks at most every
# 10 s (wait_until polls every 0.2 s).
stuck_key="" stuck_since=0 last_ask=0
node_or_stuck() {
	local now k
	now=$(date +%s)
	[ $((now - last_ask)) -ge 10 ] || return 1
	last_ask=$now
	node_joined && return 0
	k=$(hk get dpu -n $NS "$vm_dpu" -o 'jsonpath={range .status.agentStatus.conditions[?(@.status=="False")]}{.type}: {.message}{"\n"}{end}' 2>/dev/null | sed -n 1p) || true
	if [ -n "$k" ] && [ "$k" = "$stuck_key" ]; then
		[ $((now - stuck_since)) -ge "${AGENT_STUCK_SECONDS:-600}" ] && return 0
	else
		stuck_key=$k stuck_since=$now
	fi
	return 1
}

ms=$(wait_until "$AGENT_SECONDS" installed) || true
record A1 "The BMC's BFB push installs the DPU OS on $VM_DPU's Arm (DPU=vm, D52): the BFB's own installer, with DPF's bf.cfg" \
	"the installer partitions the Arm's disk, writes the DPU OS, runs DPF's bfb_modify_os, reboots" \
	"$(arm_events | grep -E 'arm → install|installer.(started|bf.cfg|finished|failed)' | sed 's/^[0-9]* //' | paste -sd';' - | cut -c1-400) after ${ms} ms" \
	"$(arm_has 'arm → installed' && echo PASS || echo FAIL)"
ms=$(wait_until 1800 logged_in) || true
record A2 "The DPU OS boots on the Arm (GRUB's entry, with virt's early console ahead of its command line)" \
	"a login prompt; DPF's cloud-init runs (oob_net0 by DHCP, DPF's CA, the dpu-agent install)" \
	"$(arm_events | grep -E 'os\.(kernel|login|cloud-init)' | sed 's/^[0-9]* //' | cut -d: -f1 | paste -sd';' -) after ${ms} ms" \
	"$(logged_in && echo PASS || echo FAIL)"
ms=$(wait_until 1800 fetched_agent) || true
record A3 "DPF's first boot installs dpu-agent from the bfb-registry's /deb, at its site address through the Arm's segment gateway" \
	"apt fetches the package DPF built (lib/agent-deb.sh) from https://$SITE_REGISTRY/deb" \
	"$(grep -c 'GET /deb/' "$WORK/logs/bfb-registry.log" || true) /deb requests; $(grep -o 'GET /deb/[^ ]*' "$WORK/logs/bfb-registry.log" | sort -u | paste -sd' ' - | cut -c1-200)" \
	"$(fetched_agent && echo PASS || echo FAIL)"
ms=$(wait_until "$AGENT_SECONDS" agent_reported) || true
hk get dpu -n $NS "$vm_dpu" -o yaml >"$WORK/evidence/dpu-$vm_dpu.yaml" 2>/dev/null || true
record P7 "OS installed: the DPU agent on $VM_DPU's DPU OS reports to DPF (the other DPU is at model: no OS)" \
	"agentStatus.lastStartupTime set; DPF moves the DPU past OS Installing" \
	"$vm_dpu: lastStartupTime $(agent_status); phase $(hk get dpu -n $NS "$vm_dpu" -o jsonpath='{.status.phase}' 2>/dev/null); OSInstalled $(cond dpu "$vm_dpu" OSInstalled) after ${ms} ms" \
	"$(agent_reported && echo PASS || echo FAIL)"
ms=$(wait_until "$AGENT_SECONDS" node_or_stuck) || true
# a joined Node: its kubelet's Ready, and DPF's next phase (DPU Cluster Config waits for a Ready Node)
node_state() { dk get nodes -o 'jsonpath={range .items[*]}{.metadata.name} Ready={.status.conditions[?(@.type=="Ready")].status} ({.status.conditions[?(@.type=="Ready")].message}); kubelet {.status.nodeInfo.kubeletVersion}, allocatable memory {.status.allocatable.memory}{"\n"}{end}' 2>/dev/null | cut -c1-300; }
node_ready() { dk get nodes -o 'jsonpath={.items[*].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null | grep -q True; }
! node_joined || wait_until 300 node_ready >/dev/null || true
# the agent's operations in order, and where it stands: the first that has not succeeded
agent_conds=$(hk get dpu -n $NS "$vm_dpu" -o 'jsonpath={range .status.agentStatus.conditions[*]}{.type}={.status} {end}' 2>/dev/null) || true
stuck=$(hk get dpu -n $NS "$vm_dpu" -o 'jsonpath={range .status.agentStatus.conditions[?(@.status=="False")]}{.type}: {.message}{"\n"}{end}' 2>/dev/null | head -1 | cut -c1-300) || true
record P8 "The DPU joins the DPU cluster as a Node (the agent's kubeadm join, at the cluster's site address)" "a Node in the DPU cluster; the DPU Ready" \
	"DPU cluster Nodes: $(node_state | paste -sd' ' -); DPU phase $(hk get dpu -n $NS "$vm_dpu" -o jsonpath='{.status.phase}' 2>/dev/null) ($(hk get dpu -n $NS "$vm_dpu" -o 'jsonpath={.status.conditions[?(@.type=="DPUClusterReady")].message}' 2>/dev/null)); agent: ${stuck:-$agent_conds}" \
	"$(node_joined && echo PASS || echo FAIL)"
# R1: with a Node, DPF carries the DPU on to Ready: DPU Cluster Config (the BMC's DPU_UEFI and
# DPU_BSP must be the BFB's, the Node Ready), Service Readiness and Node Effect Removal. Asks at
# most every 10 s.
dpu_phase() { hk get dpu -n $NS "$vm_dpu" -o jsonpath='{.status.phase}' 2>/dev/null; }
last_phase_ask=0
dpu_ready() {
	local now
	now=$(date +%s)
	[ $((now - last_phase_ask)) -ge 10 ] || return 1
	last_phase_ask=$now
	[ "$(dpu_phase)" = Ready ]
}
if node_joined; then
	ms=$(wait_until "${READY_SECONDS:-900}" dpu_ready) || true
	blocking=$(hk get dpu -n $NS "$vm_dpu" -o 'jsonpath={range .status.conditions[?(@.status=="False")]}{.type}: {.message}{"\n"}{end}' 2>/dev/null | head -1 | cut -c1-300) || true
	record R1 "DPF puts the DPU in service: DPU Cluster Config (the DPU's UEFI and BSP the BFB's, its Node Ready), Service Readiness, Node Effect Removal" \
		"phase Ready" "phase $(dpu_phase)${blocking:+; $blocking} after ${ms} ms" "$([ "$(dpu_phase)" = Ready ] && echo PASS || echo FAIL)"
else
	record R1 "DPF puts the DPU in service" "phase Ready" "no DPU Node (P8)" CANNOT
fi
hk get dpu -n $NS "$vm_dpu" -o yaml >"$WORK/evidence/dpu-$vm_dpu.yaml" 2>/dev/null || true
if node_joined; then
	h_obs="a DPU Node exists ($(node_state | sed -n 1p)); the harness runs neither DPF's DPUService controller nor Argo CD v3.3.0, which deploy HBN (DPF's operator installs them); nor could the Node take HBN's pod: it advertises no nvidia.com/bf_sf and allocates less memory than the 6Gi HBN requests"
else
	h_obs="no DPU Node to schedule it to: the agent stands at ${stuck:-an operation that has not succeeded}, before it starts its kubelet and joins"
fi
record H1 "DPF deploys HBN as a DPUService on the DPU Node (the guide's doca-hbn 3.5.0: chart and image anonymous from NGC)" \
	"the HBN pod placed on the DPU Node (it requests 6Gi, more than the Arm's 4 GB, and nvidia.com/bf_sf: 4, the SFs DPF's device plugin advertises from the ConnectX)" "$h_obs" CANNOT
hk get dpu -n $NS "$vm_dpu" -o yaml >"$WORK/evidence/dpu-$vm_dpu.yaml" 2>/dev/null || true
dk get nodes -o wide >"$WORK/evidence/dpu-cluster-nodes.txt" 2>&1 || true
arm_events >"$WORK/evidence/arm-events.txt" 2>&1 || true
cp "$arm_dir/console.log" "$WORK/evidence/arm-console.log" 2>/dev/null || true
read -r os_scope os_qemu <<<"$(mem_peaks os)"
cap_mib=$(awk '{ if ($1 ~ /^[0-9]+$/) printf "%d", $1 / 1048576; else print 0 }' "$mem_cgroup/memory.max" 2>/dev/null) || true
record M1 "Memory with the DPU OS up beside the twin, both clusters, cert-manager and DPF's controllers" \
	"the whole job within its cap (the memory rule: 9G)" \
	"peak: job ${os_scope:-?} MiB of $(mem_limit), the Arm's QEMU (the profile's 4 GB guest) ${os_qemu:-?} MiB" \
	"$([ "${cap_mib:-0}" -gt 0 ] && [ "${os_scope:-0}" -gt 0 ] && [ "$os_scope" -lt "$cap_mib" ] && echo PASS || echo FAIL)"

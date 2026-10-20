# shellcheck shell=bash
# DPU=vm's install phase, sourced by lib/checks.sh once the BFBs are prepared (P5).
#
# The lab's memory rule caps a job at 9G for everything it runs, and the BFB's installer needs an
# 8 GB guest (Q1: it unpacks its 1.6 GB initramfs into a RAM file system of half the guest's
# memory). With the clusters and DPF's controllers alive beside it that does not fit, so the
# installer runs with the stack down: when the BMC starts streaming the BFB to the vm DPU, the
# clusters (their etcd data stays), cert-manager and DPF's controllers are stopped; the twin, which
# holds the BMCs' state (its certificates and Truststore are the front's, in memory), and the
# bfb-registry the BMC is streaming from stay up. The BMC finishes the transfer and resets the Arm
# into the installer; when the installer has finished (the Arm then boots its DPU OS, with the
# profile's 4 GB), the stack is started again on the same clusters and DPF carries on: it polls the
# BMC's task, sees the transfer done, and waits for its DPU agent.
#
# mem_sampler logs, every 5 s, the job scope's memory and the Arm's QEMU resident memory by phase
# (evidence/memory.log), so the peak of each phase is measured, not estimated.

# (awk reads to the end: a reader that quits early would leave `twin get` a SIGPIPE, which
# pipefail and set -e turn into the harness's exit)
vm_path=$(twin get "**/$VM_DPU" 2>/dev/null | awk '$1 ~ /\// && !f {print $1; f = 1}') || true
vm_serial=$(twin get "**/$VM_DPU" 2>/dev/null | awk '$1 ~ /\// && !f {print tolower($NF); f = 1}') || true
arm_dir="$TWIN_STATE/bluefield/$(echo "$vm_path" | tr / _)"
# arm_events: the Arm's observations in the twin's log (field → to: message)
arm_events() {
	python3 -I - "$TWIN_STATE/events.jsonl" "$vm_path" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    try:
        e = json.loads(line)
    except ValueError:
        continue
    st = e.get("state") or {}
    src = (e.get("source") or {}).get("principal", "")
    if e.get("path") == sys.argv[2] and src == "bluefield-arm":
        print(f'{e.get("seq")} {st.get("field")} → {st.get("to")}: {e.get("message", "")[:160]}')
PY
}
arm_has() { arm_events | grep -q -- "$1"; }

# the job's cgroup (systemd-run --scope), when it runs in one
mem_cgroup=/sys/fs/cgroup$(awk -F: '$1 == "0" {print $3}' /proc/self/cgroup 2>/dev/null)
mem_sampler() {
	[ -r "$mem_cgroup/memory.current" ] || return 0
	local q p
	while :; do
		q=0
		for p in $(cat "$mem_cgroup/cgroup.procs" 2>/dev/null); do
			[ "$(cat "/proc/$p/comm" 2>/dev/null)" = qemu-system-aar ] || continue
			q=$((q + $(awk '/^VmRSS:/ {print $2}' "/proc/$p/status" 2>/dev/null || echo 0))) || true
		done
		echo "$(date +%s) $(cat "$WORK/phase" 2>/dev/null || echo -) $(($(cat "$mem_cgroup/memory.current") / 1048576)) $((q / 1024))" >>"$WORK/evidence/memory.log"
		sleep 5
	done
}
# mem_peaks <phase>: "<scope MiB> <QEMU MiB>", the largest seen in that phase
mem_peaks() {
	awk -v ph="$1" '$2 == ph { if ($3 > s) s = $3; if ($4 > q) q = $4 } END { printf "%d %d", s, q }' "$WORK/evidence/memory.log" 2>/dev/null
}
mem_limit() { cat "$mem_cgroup/memory.max" 2>/dev/null | awk '{ if ($1 ~ /^[0-9]+$/) printf "%d MiB", $1 / 1048576; else print $1 }'; }

echo provision >"$WORK/phase"
mem_sampler &
MEM_SAMPLER=$!
PIDS="$PIDS $MEM_SAMPLER"

push_started() { grep -q -- "GET /bfb/??.*$vm_serial" "$WORK/logs/bfb-registry.log" 2>/dev/null; }
ms=$(wait_until "$RUN_SECONDS" push_started) || true
if push_started; then
	# the provisioning phase's peak is the scope's so far (cgroup v2's memory.peak), not the
	# sampler's: the BMC's push starts within seconds of P5, before the sampler's first sample
	prov_scope=$(($(cat "$mem_cgroup/memory.peak" 2>/dev/null || echo 0) / 1048576)) || true
	echo install >"$WORK/phase"
	t0=$(date +%s)
	stack_stop
	echo "stack down for the install phase ($(date +%T)); the twin and the bfb-registry stay up" >&2
	install_done() { arm_has "arm → installed" || arm_has "arm → install-failed" || arm_has "power → Off: the Arm's machine ended"; }
	wait_until "$AGENT_SECONDS" install_done >/dev/null || true
	t_install=$(($(date +%s) - t0)) || true
	read -r inst_scope inst_qemu <<<"$(mem_peaks install)"
	verdict=$(arm_events | grep -E 'arm → install|power → Off' | tail -1 | sed 's/^[0-9]* //') || true
	echo os >"$WORK/phase"
	stack_start
	echo "stack up again ($(date +%T))" >&2
	record I1 "The BFB installer with the stack down (memory rule: 9G for the job; the installer needs an 8 GB guest)" \
		"the installer finishes within the cap: the twin, the bfb-registry and the Arm's 8 GB QEMU (TCG cache 256 MiB) alone" \
		"${verdict:-no verdict} after ${t_install} s; peak: job ${inst_scope} MiB of $(mem_limit), the Arm's QEMU ${inst_qemu} MiB (the provisioning phase before it: ${prov_scope} MiB)" \
		"$(arm_has 'arm → installed' && echo PASS || echo FAIL)"
else
	record I1 "The BFB installer with the stack down" "the BMC starts streaming the BFB to the vm DPU" "no push for $vm_serial after ${ms} ms" FAIL
fi

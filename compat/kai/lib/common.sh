# shellcheck shell=bash
# Shared by compat/kai, compat/aicr and compat/prometheus: a results table and waiting.

RESULTS=()

# record <id> <check> <expected> <observed> <result>
# result is PASS or FAIL; FINDING (a behaviour of the software under test, not of the twin) and
# CANNOT (a step that cannot pass on a twin with no kubelet, OS or GPU; expected) do not fail a run.
record() {
	RESULTS+=("$1|$2|$3|$4|$5")
	printf '%-5s %-8s %s — %s\n' "$1" "$5" "$2" "$4" >&2
}

# pass_if <condition-exit-status>: PASS when 0, else FAIL
pass_if() { if [ "$1" -eq 0 ]; then echo PASS; else echo FAIL; fi; }

print_table() {
	local title=$1 r id check expected observed result fails=0
	printf '\n%s\n\n' "$title"
	printf '| # | Check | Expected | Observed | Result |\n|---|---|---|---|---|\n'
	for r in "${RESULTS[@]}"; do
		IFS='|' read -r id check expected observed result <<<"$r"
		printf '| %s | %s | %s | %s | %s |\n' "$id" "$check" "$expected" "$observed" "$result"
		[ "$result" = FAIL ] && fails=$((fails + 1))
	done
	local n=${#RESULTS[@]}
	printf '\n%d checks: %d PASS, %d FAIL, %d FINDING, %d CANNOT\n' "$n" \
		"$(printf '%s\n' "${RESULTS[@]}" | grep -c '|PASS$' || true)" "$fails" \
		"$(printf '%s\n' "${RESULTS[@]}" | grep -c '|FINDING$' || true)" \
		"$(printf '%s\n' "${RESULTS[@]}" | grep -c '|CANNOT$' || true)"
	return "$fails"
}

now_ms() { python3 -I -c 'import time; print(int(time.time() * 1000))'; }

# wait_until <seconds> <command...>: polls every 0.2 s; prints the elapsed milliseconds and
# returns 0 when the command succeeds, 1 on timeout.
wait_until() {
	local limit=$1 t0 deadline
	shift
	t0=$(now_ms)
	deadline=$((t0 + limit * 1000))
	while :; do
		if "$@" >/dev/null 2>&1; then
			echo $(($(now_ms) - t0))
			return 0
		fi
		if [ "$(now_ms)" -ge "$deadline" ]; then
			echo $(($(now_ms) - t0))
			return 1
		fi
		sleep 0.2
	done
}

# free_port prints a loopback TCP port nothing is listening on (the kernel's choice).
free_port() {
	python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'
}

# envtest_dir finds a kube-apiserver and etcd: $KUBEBUILDER_ASSETS, else the newest version
# setup-envtest installed in its default directory (as internal/adapter/kwok/e2e_test.go does).
envtest_dir() {
	if [ -n "${KUBEBUILDER_ASSETS:-}" ]; then echo "$KUBEBUILDER_ASSETS"; return; fi
	local base d best=""
	for base in "$HOME/Library/Application Support/io.kubebuilder.envtest/k8s" "$HOME/.local/share/kubebuilder-envtest/k8s"; do
		for d in "$base"/*; do
			[ -x "$d/kube-apiserver" ] && [ -x "$d/etcd" ] && best=$d
		done
	done
	echo "$best"
}

# shellcheck shell=bash
# Bring a dc-twin estate up and down for a harness. Sourced by compat/kai, compat/aicr and
# compat/prometheus after lib/common.sh.
#
# `dctwin up` records the running estate in ~/.dc-twin/current; the harness runs it with HOME set
# to its work directory so a user's own running estate stays the current one. The captured BMC
# assets are still read from the user's cache (DCTWIN_ASSETS).

# twin_up <estate.yaml> <work-dir> <adapters> [kubeconfig]
# With a kubeconfig, the estate is copied into the work directory with that path as the stack's
# kubeconfig output (the kwok adapter's credential): everything from the estate's `stacks:` key to
# the end of the file is replaced, so the key must be the file's last. Exports TWIN_STATE.
twin_up() {
	local estate=$1 dir=$2 adapters=$3 kubeconfig=${4:-}
	mkdir -p "$dir/home"
	TWIN_HOME="$dir/home"
	DCTWIN_ASSETS=${DCTWIN_ASSETS:-$HOME/.cache/dc-twin/assets}
	TWIN_STATE="$dir/state"
	rm -rf "$TWIN_STATE"
	local file=$estate
	if [ -n "$kubeconfig" ]; then
		file="$dir/$(basename "$estate")"
		awk '/^  stacks:/ { exit } { print }' "$estate" >"$file"
		printf '  stacks: [{ name: nvidia-dsx-os@0.1.0, outputs: { kubeconfig: "%s" } }]\n' "$kubeconfig" >>"$file"
	fi
	HOME="$TWIN_HOME" DCTWIN_ASSETS="$DCTWIN_ASSETS" "$DCTWIN" up "$file" --state "$TWIN_STATE" --adapters "$adapters" \
		>"$dir/up.log" 2>&1 &
	TWIN_PID=$!
	local i
	for i in $(seq 1 240); do
		[ -f "$TWIN_STATE/control.json" ] && grep -q 'control API' "$dir/up.log" && break
		kill -0 "$TWIN_PID" 2>/dev/null || break
		sleep 0.25
	done
	grep -q 'control API' "$dir/up.log" || { cat "$dir/up.log" >&2; return 1; }
	export TWIN_STATE TWIN_HOME DCTWIN_ASSETS TWIN_PID
}

# twin <verb> [args...]: the dctwin CLI against this harness's estate.
twin() {
	HOME="$TWIN_HOME" DCTWIN_ASSETS="$DCTWIN_ASSETS" "$DCTWIN" "$@" --state "$TWIN_STATE"
}

twin_down() {
	[ -n "${TWIN_PID:-}" ] || return 0
	twin down >/dev/null 2>&1 || kill "$TWIN_PID" 2>/dev/null || true
	local i
	for i in $(seq 1 40); do kill -0 "$TWIN_PID" 2>/dev/null || break; sleep 0.25; done
	kill -9 "$TWIN_PID" 2>/dev/null || true
	wait "$TWIN_PID" 2>/dev/null || true
	TWIN_PID=
}

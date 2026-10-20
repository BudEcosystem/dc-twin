# shellcheck shell=bash
# dpf_twin_up <estate.yaml> <work-dir> <adapters> [dctwin up flags...]: compat/kai/lib/twin.sh's
# twin_up for an estate this harness has rendered already, with flags for `dctwin up` (DPU=vm
# passes --allow-tcg: the Arm is an aarch64 guest, which runs under TCG on an x86_64 host). Sets
# what twin_up sets, so that lib's `twin` and `twin_down` work on it.
dpf_twin_up() {
	local estate=$1 dir=$2 adapters=$3
	shift 3
	mkdir -p "$dir/home"
	TWIN_HOME="$dir/home"
	DCTWIN_ASSETS=${DCTWIN_ASSETS:-$HOME/.cache/dc-twin/assets}
	TWIN_STATE="$dir/state"
	rm -rf "$TWIN_STATE"
	HOME="$TWIN_HOME" DCTWIN_ASSETS="$DCTWIN_ASSETS" "$DCTWIN" up "$estate" --state "$TWIN_STATE" --adapters "$adapters" "$@" \
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

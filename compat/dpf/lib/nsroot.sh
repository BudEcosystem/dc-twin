#!/usr/bin/env bash
# nsroot.sh <command> [args...]
#
# Runs a command as an ordinary process on this Linux host, with the host's filesystem and
# network, plus the binds NSROOT_BINDS names ("<host dir>:<path> …"), in a mount namespace of its
# own. DPF's provisioning controller keeps BFBs and bf.cfg files under /bfb, an absolute path it
# does not let you change (internal/provisioning/controllers/util: BFBBaseDir), and reads the
# host cluster's CA from a pod's service-account volume
# (/var/run/secrets/kubernetes.io/serviceaccount); a harness must not create either on a shared
# host, and in the process's own mount namespace nothing else sees them.
#
# The new root is a tmpfs holding a recursive bind of every top-level directory of the host's; a
# directory a bind is grafted under is itself a tmpfs of binds of the host directory's entries,
# level by level, so the rest of it stays the host's. The namespace is entered through rootless
# podman's user namespace (`podman unshare`), because hosts that restrict unprivileged user
# namespaces (Ubuntu's AppArmor policy) allow podman's. The process is uid 0 there and the host's
# uid outside; what it writes belongs to the user.
set -euo pipefail
inner='
set -e
# the host directories that become tmpfs images: every existing ancestor of a bind target
expand=" "
for b in $NSROOT_BINDS; do
	dst=$(realpath -m "${b#*:}")
	cur=""
	IFS=/ read -ra parts <<<"${dst#/}"
	for ((i = 0; i < ${#parts[@]} - 1; i++)); do
		cur="$cur/${parts[i]}"
		if [ -d "$cur" ] && [ ! -L "$cur" ]; then expand="$expand$cur "; fi
	done
done
place() { # host path, path under the new root
	case "$expand" in
	*" $1 "*)
		mkdir "$2"
		mount -t tmpfs tmpfs "$2"
		local e
		for e in "$1"/* "$1"/.[!.]*; do
			[ -e "$e" ] || [ -L "$e" ] || continue
			place "$e" "$2/${e##*/}"
		done
		return ;;
	esac
	if [ -L "$1" ]; then ln -s "$(readlink "$1")" "$2"
	elif [ -d "$1" ]; then mkdir "$2"; mount --rbind "$1" "$2"
	elif [ -e "$1" ]; then : >"$2"; mount --bind "$1" "$2"
	fi
}
r=$(mktemp -d)
mount -t tmpfs tmpfs "$r"
for d in /*; do place "$d" "$r$d"; done
for b in $NSROOT_BINDS; do
	dst=$(realpath -m "${b#*:}")
	mkdir -p "$r$dst"
	mount --bind "${b%%:*}" "$r$dst"
done
exec chroot "$r" /usr/bin/env -C "$CWD" "$@"
'
exec env NSROOT_BINDS="${NSROOT_BINDS:-}" CWD="$PWD" podman unshare unshare --mount --propagation private bash -c "$inner" nsroot "$@"

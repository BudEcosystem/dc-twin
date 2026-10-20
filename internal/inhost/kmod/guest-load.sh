#!/bin/sh
# guest-load.sh <dir>: builds dctwin-kmsg.ko (D50) from <dir> (Kbuild and dctwin_kmsg.c, this
# directory's) inside a Debian guest, for the guest's running kernel, installs it where the
# kernel's own modules are found, loads it, and has it loaded again on every boot
# (modules-load.d), as a driver's module is. Run as root. The second guests the compat harnesses
# boot beside the twin (compat/nvsentinel's GPU guest, compat/fleet-intelligence's) use it, so
# that a line the twin rendered for a fault is printk'd into their kernels too, a kernel record
# with facility kern, rather than written through /dev/kmsg (facility user).
#
# The build needs the kernel's build tree, which Debian ships as linux-headers-<release>, and its
# compiler; both are installed when missing. A kernel whose headers the archive no longer has, or
# one that refuses the module, leaves the guest without it: the script says so and still exits 0,
# and the caller writes through /dev/kmsg and labels it.
#
# Prints one line: "loaded <release> <sha256 of the module>" or "absent: <why>".
set -u
src=${1:?usage: guest-load.sh <dir with Kbuild and dctwin_kmsg.c>}
rel=$(uname -r)
say() { echo "$*"; exit 0; }
if [ -w /proc/dctwin-kmsg ]; then
	say "loaded $rel $(sha256sum "/lib/modules/$rel/updates/dctwin-kmsg.ko" 2>/dev/null | cut -c1-64)"
fi
if [ ! -d "/lib/modules/$rel/build" ]; then
	command -v apt-get >/dev/null || say "absent: no build tree for $rel and no apt-get"
	DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends "linux-headers-$rel" make >/dev/null 2>&1 ||
		{ apt-get update -qq >/dev/null 2>&1 && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends "linux-headers-$rel" make >/dev/null 2>&1; } ||
		say "absent: linux-headers-$rel is not in the guest's package sources"
fi
command -v make >/dev/null || { DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends make >/dev/null 2>&1 || say "absent: no make"; }
b=$(mktemp -d)
cp "$src/Kbuild" "$src/dctwin_kmsg.c" "$b/"
make -s -C "/lib/modules/$rel/build" M="$b" modules >"$b/build.log" 2>&1 || say "absent: the build failed: $(tail -3 "$b/build.log" | tr '\n' ' ')"
install -D -m 0644 "$b/dctwin-kmsg.ko" "/lib/modules/$rel/updates/dctwin-kmsg.ko"
depmod -a "$rel"
echo dctwin-kmsg >/etc/modules-load.d/dctwin-kmsg.conf
rm -rf "$b"
modprobe dctwin-kmsg 2>/tmp/dctwin-kmsg.err || say "absent: the kernel refused it: $(cat /tmp/dctwin-kmsg.err)"
[ -w /proc/dctwin-kmsg ] || say "absent: loaded, but /proc/dctwin-kmsg is missing"
say "loaded $rel $(sha256sum "/lib/modules/$rel/updates/dctwin-kmsg.ko" | cut -c1-64)"

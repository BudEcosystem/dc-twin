package nvue

import (
	"sort"
	"strings"
)

// ApplyScript is what applying runs in the switch's container, as `sh -c ApplyScript sh <args>`
// with ApplyArgs: write /etc/network/interfaces and /etc/frr/frr.conf, reload both with the
// switch's own tools (ifreload -a, which creates, changes and removes interfaces to match;
// frr-reload.py, which applies the difference to the running FRR, after the VRFs' own stanzas
// through vtysh), restart dhcrelay when its command line changed, and replace the twin's
// nftables tables with the ACLs' (nft -f, atomically), then the system's files, accounts and
// daemons (SystemScript: hostname, time zone, users, NTP, DNS, syslog, LLDP, the login messages,
// SNMP). It prints what each
// printed and exits non-zero when one failed; ifupdown2 carries on past an interface it cannot
// create, as it does on a switch.
const ApplyScript = `set -u
umask 022
mkdir -p /run/network /etc/network/interfaces.d
printf '%s' "$1" > /etc/network/interfaces.dctwin && mv /etc/network/interfaces.dctwin /etc/network/interfaces
printf '%s' "$2" > /etc/frr/frr.conf.dctwin && chown frr:frr /etc/frr/frr.conf.dctwin && mv /etc/frr/frr.conf.dctwin /etc/frr/frr.conf
rc=0
# what the running VRF stanzas have that the new ones do not (a static route, an L3 VNI) is
# removed through vtysh first, while the VRF still exists (ifreload may remove it)
` + VRFStanzas + ` < /etc/frr/frr.conf > /run/dctwin-vrfs.conf
vtysh -c 'show running-config' 2>/dev/null | ` + VRFStanzas + ` > /run/dctwin-vrfs.running
` + VRFRemovals + ` /run/dctwin-vrfs.conf /run/dctwin-vrfs.running > /run/dctwin-vrfs.del
if [ -s /run/dctwin-vrfs.del ]; then
  echo "== vtysh -f (what the VRFs' stanzas no longer have)"
  vtysh -f /run/dctwin-vrfs.del 2>&1 || rc=1
fi
# a VLAN whose VNI changed (a VRF given another L3 VNI on the same reserved VLAN): its SVI and
# VXLAN devices are made again, as zebra keeps an existing SVI's L3 VNI and VRF (FRR 8.4)
for d in $5; do
  echo "== ip link del $d (its VNI changed)"
  ip link del "$d" 2>&1 || true
done
echo "== ifreload -a"
ifreload -a 2>&1 || rc=1
# each VRF's stanza (its L3 VNI, its static routes) next, through vtysh: frr-reload's
# line-by-line diff can send a "vrf" line into a router bgp context, where bgpd reads it as
# vrf-policy (D57); with the removals above, frr-reload finds the stanzas equal
if [ -s /run/dctwin-vrfs.conf ]; then
  echo "== vtysh -f (the VRFs' stanzas)"
  vtysh -f /run/dctwin-vrfs.conf 2>&1 || rc=1
fi
# a new AS is a new BGP: FRR starts again from the new configuration, as NVUE restarts BGP for
# it (frr-reload cannot change a router bgp's AS, and deleting the old instance while VNIs exist
# crashes bgpd 10 in bgp_evpn_local_vni_add)
new_as=$(awk '/^router bgp [0-9]+$/ { print $3; exit }' /etc/frr/frr.conf)
old_as=$(vtysh -c 'show running-config' 2>/dev/null | awk '/^router bgp [0-9]+$/ { print $3; exit }')
if [ -n "$old_as" ] && [ -z "$new_as" ]; then
  # BGP removed (a factory reset, a configuration without it): FRR's own command removes it;
  # a restart is for an AS that changes, which frr-reload cannot do. Its VRFs' instances first:
  # FRR refuses to delete the default instance while they exist
  echo "== router bgp $old_as removed"
  for v in $(vtysh -c 'show running-config' 2>/dev/null | awk '/^router bgp [0-9]+ vrf [^ ]+$/ { print $3 " " $5 }' | tr ' ' ':'); do
    vtysh -c 'configure terminal' -c "no router bgp ${v%%:*} vrf ${v#*:}" 2>&1 || rc=1
  done
  vtysh -c 'configure terminal' -c "no router bgp $old_as" 2>&1 || rc=1
elif [ -n "$old_as" ] && [ "$old_as" != "$new_as" ]; then
  echo "== router bgp $old_as is now $new_as: FRR restarts"
  /usr/lib/frr/frrinit.sh restart 2>&1 || rc=1
fi
echo "== frr-reload.py --reload"
out=$(/usr/lib/frr/frr-reload.py --reload --stdout /etc/frr/frr.conf 2>&1) || rc=1
printf '%s\n' "$out" | ` + ReloadSummary + `
# frr-reload can finish without FRR running what it was given: the BGP the file has must run
if [ -n "$new_as" ] && ! vtysh -c 'show running-config' 2>/dev/null | grep -q "^router bgp $new_as\$"; then
  echo "FRR does not run router bgp $new_as after the reload"; rc=1
fi
echo "== dhcrelay"
# the switch's start script keeps dhcrelay running with the arguments in /run/dctwin-relay (a
# process started from here would end with this exec); a change is the file and a restart
old=$(cat /run/dctwin-relay 2>/dev/null || true)
if [ "$3" != "$old" ]; then
  printf '%s' "$3" > /run/dctwin-relay
  pkill -x dhcrelay 2>/dev/null || true
  if [ -n "$3" ]; then printf '%s\n' "$3" | sed 's/^/dhcrelay in vrf /'; else echo "no relay"; fi
else
  echo "unchanged"
fi
# a relay that cannot run (an interface it cannot open) fails the apply, saying why
if [ -n "$3" ]; then
  want=$(printf '%s\n' "$3" | grep -c .)
  n=0
  until [ "$(pgrep -x dhcrelay 2>/dev/null | grep -c .)" -ge "$want" ]; do
    n=$((n+1))
    if [ $n -gt 10 ]; then echo "dhcrelay is not running (want $want):"; tail -n 5 /var/log/dhcrelay.log; rc=1; break; fi
    sleep 0.5
  done
fi
echo "== nft -f (ACLs)"
printf '%s' "$4" > /run/dctwin-acl.nft
if command -v nft >/dev/null 2>&1; then
  nft -f /run/dctwin-acl.nft 2>&1 || rc=1
elif grep -q 'jump acl_' /run/dctwin-acl.nft; then
  echo "ACLs are configured and the switch has no nft"; rc=1
else
  echo "no ACLs"
fi
` + SystemScript + `exit $rc
`

// VRFRemovals is, from the new VRF stanzas (its first file) and the running ones (its second),
// the commands that remove each line a running stanza has and the new one does not.
const VRFRemovals = `awk 'FILENAME == ARGV[1] { if ($0 ~ /^vrf /) v = $0; else if ($0 !~ /^exit-vrf/) want[v SUBSEP $0] = 1; next }
/^vrf / { v = $0; next }
/^exit-vrf/ || /^ *!?$/ { next }
!((v SUBSEP $0) in want) { sub(/^ +/, ""); printf "%s\n no %s\nexit-vrf\n", v, $0 }'`

// VRFStanzas filters a frr.conf (on stdin) to its VRFs' stanzas, vrf ... exit-vrf, whole.
const VRFStanzas = `awk '/^vrf / { p = 1 } p { print } /^exit-vrf/ { p = 0 }'`

// ReloadSummary filters frr-reload's output (on stdin) to what an apply reports: every line
// that reports an error, however early (the first is what failed the batch; frr-reload then
// retries line by line and prints far more), then the last 20 lines.
const ReloadSummary = `awk 'NF { l[++n] = $0; if ($0 ~ /^% |[Ff]ailure|[Ee]rror|Can.t |WARNING/) e[++m] = $0 }
END { if (m) { print "(errors)"; for (i = 1; i <= m; i++) print e[i]; print "(last lines)" }
	s = n - 19; if (s < 1) s = 1; for (i = s; i <= n; i++) print l[i] }'`

// ApplyArgs are ApplyScript's arguments for rendered files.
func ApplyArgs(f Files) []string {
	y := f.System
	return []string{f.Interfaces, f.FRR, strings.Join(f.Relay, "\n"), f.ACL, strings.Join(f.Recreate, " "),
		y.Hostname, y.Timezone, y.Users, y.NTP, y.NTPVRF, y.DNS, y.Syslog, y.LLDP, y.Banner, y.MOTD, y.SNMP}
}

// vniVLANs reads a rendered interfaces file's VXLAN devices: VLAN -> VNI.
func vniVLANs(interfaces string) map[string]string {
	out := map[string]string{}
	var vni string
	for _, l := range strings.Split(interfaces, "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) == 2 && f[0] == "iface" && strings.HasPrefix(f[1], "vni"):
			vni = strings.TrimPrefix(f[1], "vni")
		case len(f) == 2 && f[0] == "iface":
			vni = ""
		case len(f) == 2 && f[0] == "bridge-access" && vni != "":
			out[f[1]] = vni
		}
	}
	return out
}

// recreate is the devices applying to must make again: the SVI and both VXLAN devices of every
// VLAN whose VNI the new files change.
func recreate(old, new Files) []string {
	was, is := vniVLANs(old.Interfaces), vniVLANs(new.Interfaces)
	var out []string
	for vlan, v := range is {
		if o, ok := was[vlan]; ok && o != v {
			out = append(out, "vlan"+vlan, "vni"+o, "vni"+v)
		}
	}
	sort.Strings(out)
	return out
}

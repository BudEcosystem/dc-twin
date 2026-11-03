package nvue

import "strings"

// ApplyScript is what applying runs in the switch's container, as `sh -c ApplyScript sh <args>`
// with ApplyArgs: write /etc/network/interfaces and /etc/frr/frr.conf, reload both with the
// switch's own tools (ifreload -a, which creates, changes and removes interfaces to match;
// frr-reload.py, which applies the difference to the running FRR, after the VRFs' own stanzas
// through vtysh), and restart dhcrelay when its
// command line changed. It prints what each printed and exits non-zero when one failed; ifupdown2
// carries on past an interface it cannot create, as it does on a switch.
const ApplyScript = `set -u
umask 022
mkdir -p /run/network /etc/network/interfaces.d
printf '%s' "$1" > /etc/network/interfaces.dctwin && mv /etc/network/interfaces.dctwin /etc/network/interfaces
printf '%s' "$2" > /etc/frr/frr.conf.dctwin && chown frr:frr /etc/frr/frr.conf.dctwin && mv /etc/frr/frr.conf.dctwin /etc/frr/frr.conf
rc=0
echo "== ifreload -a"
ifreload -a 2>&1 || rc=1
# each VRF's stanza (its L3 VNI) first, through vtysh: frr-reload's line-by-line diff can send a
# new "vrf" line into a router bgp context, where bgpd reads it as vrf-policy (D57)
` + VRFStanzas + ` < /etc/frr/frr.conf > /run/dctwin-vrfs.conf
if [ -s /run/dctwin-vrfs.conf ]; then
  echo "== vtysh -f (the VRFs' stanzas)"
  vtysh -f /run/dctwin-vrfs.conf 2>&1 || rc=1
fi
echo "== frr-reload.py --reload"
out=$(/usr/lib/frr/frr-reload.py --reload --stdout /etc/frr/frr.conf 2>&1) || rc=1
printf '%s\n' "$out" | ` + ReloadSummary + `
echo "== dhcrelay"
# the switch's start script keeps dhcrelay running with the arguments in /run/dctwin-relay (a
# process started from here would end with this exec); a change is the file and a restart
old=$(cat /run/dctwin-relay 2>/dev/null || true)
if [ "$3" != "$old" ]; then
  printf '%s' "$3" > /run/dctwin-relay
  pkill -x dhcrelay 2>/dev/null || true
  if [ -n "$3" ]; then echo "dhcrelay $3"; else echo "no relay"; fi
else
  echo "unchanged"
fi
# a relay that cannot run (an interface it cannot open) fails the apply, saying why
if [ -n "$3" ]; then
  n=0
  until pgrep -x dhcrelay >/dev/null 2>&1; do
    n=$((n+1))
    if [ $n -gt 10 ]; then echo "dhcrelay is not running:"; tail -n 5 /var/log/dhcrelay.log; rc=1; break; fi
    sleep 0.5
  done
fi
exit $rc
`

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
	return []string{f.Interfaces, f.FRR, strings.Join(f.Relay, " ")}
}

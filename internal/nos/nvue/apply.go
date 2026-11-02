package nvue

import "strings"

// ApplyScript is what applying runs in the switch's container, as `sh -c ApplyScript sh <args>`
// with ApplyArgs: write /etc/network/interfaces and /etc/frr/frr.conf, reload both with the
// switch's own tools (ifreload -a, which creates, changes and removes interfaces to match;
// frr-reload.py, which applies the difference to the running FRR), and restart dhcrelay when its
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
echo "== frr-reload.py --reload"
/usr/lib/frr/frr-reload.py --reload --stdout /etc/frr/frr.conf 2>&1 | grep -vE '^$' | tail -n 20 || true
vtysh -c 'show running-config' >/dev/null 2>&1 || rc=1
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
exit $rc
`

// ApplyArgs are ApplyScript's arguments for rendered files.
func ApplyArgs(f Files) []string {
	return []string{f.Interfaces, f.FRR, strings.Join(f.Relay, " ")}
}

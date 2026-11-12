package nvue

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// System is what a configuration's system and service branches render to besides the network's
// files (18 — The first push, D76): the files and accounts a day-0 push gives a Cumulus switch,
// which ApplyScript writes and the daemons it (re)starts. An empty field is one NVUE does not
// manage: a daemon NVUE started for it before is stopped, a file it wrote is reset, and on a
// switch the twin configures itself (not a factory one) nothing is touched that its tree does
// not name, so the twin's own switches run as they did before day 0 existed.
type System struct {
	// Hostname is /etc/hostname, set with hostname(1) too, as Cumulus sets it from
	// `system hostname`; "" leaves it.
	Hostname string
	// Timezone is /etc/timezone, and /etc/localtime's link into /usr/share/zoneinfo; "" leaves
	// them.
	Timezone string
	// Users are the NVUE-managed accounts (`system aaa user`), one per line, space-separated:
	// name, hashed password or -, the role's groups (comma-separated; - when the configuration
	// names no role, and an existing account keeps its groups), the groups a new account is
	// created with, and the full name.
	Users string
	// NTP is the NTP servers' lines (`server <host> [iburst]`, which chrony and ntpd both read),
	// and NTPVRF the VRF they are reached in.
	NTP, NTPVRF string
	// DNS is /etc/resolv.conf.
	DNS string
	// Syslog is rsyslog's forwarding configuration; non-empty also means rsyslogd runs.
	Syslog string
	// LLDP is lldpd's configuration; non-empty also means lldpd runs.
	LLDP string
	// Banner is /etc/issue.net, which sshd shows before the password (the image's sshd_config
	// names it as its Banner), and MOTD /etc/motd, shown after a login: `system message
	// pre-login` and `post-login`.
	Banner, MOTD string
	// SNMP is snmpd's configuration (system snmp-server); non-empty also means snmpd runs.
	SNMP string
}

// renderSystem renders a configuration's system and service branches. A factory switch (D72)
// runs what Cumulus runs from the factory whatever the tree says (rsyslog, lldpd, vendor facts
// B) and has Cumulus's default time zone, Etc/UTC; a switch the twin configures runs only what
// its tree names.
func renderSystem(t Tree, sw nos.Switch) (System, error) {
	factory := sw.Factory != nil
	var s System
	s.Hostname = str(t, "system", "hostname")
	s.Timezone = str(t, "system", "date-time", "timezone")
	if s.Timezone == "" {
		s.Timezone = str(t, "system", "timezone")
	}
	if s.Timezone == "" && factory {
		s.Timezone = "Etc/UTC"
	}

	users := sub(t, "system", "aaa", "user")
	var lines []string
	for _, name := range keys(users) {
		u := sub(users, name)
		role := str(u, "role")
		groups, create := "-", strings.Join(Roles[DefaultRole], ",")
		if role != "" {
			g, ok := roleGroups(t, role)
			if !ok {
				return System{}, fmt.Errorf("system aaa user %s: role %s is neither built in (%s) nor a system aaa role", name, role, strings.Join(keys(rolesAny()), ", "))
			}
			if len(g) == 0 {
				return System{}, fmt.Errorf("system aaa user %s: role %s allows nothing (its classes deny, or have no command-path)", name, role)
			}
			groups, create = strings.Join(g, ","), strings.Join(g, ",")
		}
		hash := str(u, "hashed-password")
		if p := str(u, "password"); p != "" && hash == "" {
			hash = hashStable(name, p)
		}
		if hash == "" {
			hash = "-"
		}
		// a disabled user (state disabled) keeps its account, its password locked (!), as
		// passwd -l locks one; how Cumulus disables it is UNCONFIRMED
		if str(u, "state") == "disabled" {
			hash = "!" + strings.TrimPrefix(hash, "-")
		}
		if strings.ContainsAny(hash, " \t\n:") {
			return System{}, fmt.Errorf("system aaa user %s: hashed-password is not a crypt(3) hash", name)
		}
		full := strings.Map(func(r rune) rune {
			if r == '\n' || r == ':' || r == ',' {
				return ' '
			}
			return r
		}, str(u, "full-name"))
		lines = append(lines, strings.TrimRight(fmt.Sprintf("%s %s %s %s %s", name, hash, groups, create, full), " "))
	}
	s.Users = strings.Join(lines, "\n")

	// NTP: Cumulus runs ntpd in one VRF (vendor facts B: which one by default is UNCONFIRMED);
	// the twin's chrony does too, bound to the management VRF's device for mgmt
	ntp := sub(t, "service", "ntp")
	var ntpLines []string
	for _, vrf := range keys(ntp) {
		servers := sub(ntp, vrf, "server")
		if len(servers) == 0 {
			continue
		}
		if vrf != "default" && vrf != "mgmt" {
			return System{}, fmt.Errorf("service ntp %s: the twin's NTP runs in the default VRF or mgmt", vrf)
		}
		if s.NTPVRF != "" && s.NTPVRF != vrf {
			return System{}, fmt.Errorf("service ntp: servers in %s and %s: the twin's NTP runs in one VRF, as Cumulus's ntpd does", s.NTPVRF, vrf)
		}
		s.NTPVRF = vrf
		for _, h := range keys(servers) {
			// 5.15's association-type pool is chrony's (and ntpd's) pool: every address the
			// name resolves to
			l := "server " + h
			if str(servers, h, "association-type") == "pool" {
				l = "pool " + h
			}
			if on(servers, h, "iburst") {
				l += " iburst"
			}
			ntpLines = append(ntpLines, l)
		}
	}
	s.NTP = strings.Join(ntpLines, "\n")

	// DNS: one resolv.conf; a server in the management VRF is marked as Cumulus marks it
	// ("# vrf mgmt"), and ApplyScript routes queries to it through the VRF's table
	dns := sub(t, "service", "dns")
	var dnsLines []string
	for _, vrf := range keys(dns) {
		if vrf != "default" && vrf != "mgmt" {
			return System{}, fmt.Errorf("service dns %s: the twin's resolver is in the default VRF or mgmt", vrf)
		}
		for _, a := range keys(sub(dns, vrf, "server")) {
			if _, err := netip.ParseAddr(a); err != nil {
				return System{}, fmt.Errorf("service dns %s server %s: not an address", vrf, a)
			}
			l := "nameserver " + a
			if vrf == "mgmt" {
				l += " # vrf mgmt"
			}
			dnsLines = append(dnsLines, l)
		}
	}
	if len(dnsLines) > 0 {
		s.DNS = "# rendered by the twin's NVUE (service dns)\n" + strings.Join(dnsLines, "\n") + "\n"
	}

	// syslog: rsyslog's forwarding (omfwd), its Device the VRF's when not the default
	syslog := sub(t, "system", "syslog", "server")
	if len(syslog) > 0 || factory {
		var b strings.Builder
		b.WriteString("# rendered by the twin's NVUE (system syslog)\n")
		for _, h := range keys(syslog) {
			srv := sub(syslog, h)
			port := str(srv, "port")
			if port == "" {
				port = "514"
			}
			proto := str(srv, "protocol")
			if proto == "" {
				proto = "udp"
			}
			fmt.Fprintf(&b, `*.* action(type="omfwd" target="%s" port="%s" protocol="%s"`, h, port, proto)
			if vrf := str(srv, "vrf"); vrf != "" && vrf != "default" {
				if vrf != "mgmt" && sub(t, "vrf", vrf) == nil {
					return System{}, fmt.Errorf("system syslog server %s: vrf %s does not exist", h, vrf)
				}
				fmt.Fprintf(&b, ` Device="%s"`, vrf)
			}
			b.WriteString(")\n")
		}
		s.Syslog = b.String()
	}

	// LLDP: lldpd with Cumulus's settings (vendor facts B: tx-interval 30, tx-hold-multiplier
	// 4, port ID the interface's name), on the front-panel ports and eth0
	lldp := sub(t, "service", "lldp")
	if (factory || lldp != nil) && str(lldp, "state") != "disabled" {
		interval, hold := str(lldp, "tx-interval"), str(lldp, "tx-hold-multiplier")
		if interval == "" {
			interval = "30"
		}
		if hold == "" {
			hold = "4"
		}
		var b strings.Builder
		b.WriteString("# rendered by the twin's NVUE (service lldp)\n")
		if s.Hostname != "" {
			fmt.Fprintf(&b, "configure system hostname %s\n", s.Hostname)
		}
		fmt.Fprintf(&b, "configure system interface pattern swp*,%s\n", nos.MgmtIf)
		fmt.Fprintf(&b, "configure lldp portidsubtype ifname\n")
		fmt.Fprintf(&b, "configure lldp tx-interval %s\n", interval)
		fmt.Fprintf(&b, "configure lldp tx-hold %s\n", hold)
		s.LLDP = b.String()
	}

	s.Banner = str(t, "system", "message", "pre-login")
	s.MOTD = str(t, "system", "message", "post-login")

	snmp, err := renderSNMP(sub(t, "system", "snmp-server"))
	if err != nil {
		return System{}, err
	}
	s.SNMP = snmp
	return s, nil
}

// renderSNMP is snmpd's configuration for system snmp-server once it is on: its listening
// addresses (localhost is the loopback's; none is localhost only, UNCONFIRMED as Cumulus's
// default) and its read-only communities, each from its access prefixes (any: every source).
// An address in a VRF other than the default is listened for on every address: a socket of the
// default VRF cannot bind a VRF's address, and takes the VRF's datagrams (udp_l3mdev_accept, which
// the twin sets on a factory switch, as for sshd).
func renderSNMP(snmp map[string]any) (string, error) {
	if str(snmp, "enable") != "on" {
		return "", nil
	}
	var agents []string
	seen := map[string]bool{}
	add := func(a string) {
		if !seen[a] {
			seen[a] = true
			agents = append(agents, a)
		}
	}
	for _, a := range keys(sub(snmp, "listening-address")) {
		vrf := str(snmp, "listening-address", a, "vrf")
		switch {
		case vrf != "" && vrf != "default":
			add("udp:161")
		case a == "localhost":
			add("udp:127.0.0.1:161")
		default:
			ip, err := netip.ParseAddr(a)
			if err != nil {
				return "", fmt.Errorf("system snmp-server listening-address %s: an address, or localhost", a)
			}
			if ip.Is6() {
				add("udp6:[" + a + "]:161")
			} else {
				add("udp:" + a + ":161")
			}
		}
	}
	if len(agents) == 0 {
		agents = []string{"udp:127.0.0.1:161"}
	}
	var b strings.Builder
	b.WriteString("# rendered by the twin's NVUE (system snmp-server)\n")
	fmt.Fprintf(&b, "agentaddress %s\n", strings.Join(agents, ","))
	for _, c := range keys(sub(snmp, "readonly-community")) {
		if strings.ContainsAny(c, " \t\n#") {
			return "", fmt.Errorf("system snmp-server readonly-community %q: not a community snmpd can read", c)
		}
		access := keys(sub(snmp, "readonly-community", c, "access"))
		if len(access) == 0 {
			access = []string{"any"}
		}
		for _, a := range access {
			if a == "any" {
				a = "default"
			}
			fmt.Fprintf(&b, "rocommunity %s %s\n", c, a)
		}
	}
	return b.String(), nil
}

// roleGroups is a role's Linux groups, which the switch authorises NVUE's users by (auth.go): a
// role `system aaa role` defines is its classes', a class allowing ro being nvshow, rw nvshow and
// nvset, act or all nvshow, nvset and nvapply, and Cumulus's class sudo (as net-configurator
// restates it, system-admin's) also the sudo group; a class the configuration does not define
// is Cumulus's built-in one of its name (nvshow, nvapply, sudo; UNCONFIRMED beyond the names
// net-configurator restates). So net-configurator's system-admin, nvue-admin and nvue-monitor
// are Roles' own. A built-in role the configuration does not define is Roles'.
func roleGroups(t Tree, role string) ([]string, bool) {
	r := sub(t, "system", "aaa", "role", role)
	if r == nil {
		g, ok := Roles[role]
		return g, ok
	}
	rank := map[string]int{"ro": 1, "rw": 2, "act": 3, "all": 3}
	builtin := map[string]int{"nvshow": 1, "nvset": 2, "nvapply": 3, "sudo": 3}
	level, sudo := 0, false
	for _, class := range keys(sub(r, "class")) {
		c := sub(t, "system", "aaa", "class", class)
		l := 0
		switch {
		case c == nil:
			l = builtin[class]
		case str(c, "action") == "deny":
			continue
		default:
			for _, p := range keys(sub(c, "command-path")) {
				l = max(l, rank[str(c, "command-path", p, "permission")])
			}
		}
		if class == "sudo" && l > 0 {
			sudo = true
		}
		level = max(level, l)
	}
	var out []string
	if sudo {
		out = append(out, "sudo")
	}
	out = append(out, []string{"nvshow", "nvset", "nvapply"}[:level]...)
	return out, true
}

// rolesAny is Roles as a tree's keys, for naming the built-in roles.
func rolesAny() map[string]any {
	out := map[string]any{}
	for r := range Roles {
		out[r] = true
	}
	return out
}

// SystemScript is ApplyScript's part for System, its arguments $6 to $16 (ApplyArgs): the
// hostname, the time zone, the users, NTP, DNS, syslog, lldpd, the login messages and snmpd. A switch container runs no
// init, so the daemons are started (and restarted on a change) here, as each daemonises.
// /etc/hostname, /etc/hosts and /etc/resolv.conf are Docker's bind mounts, so they are written in
// place, never renamed over.
const SystemScript = `# stop a daemon NVUE runs and wait for it to be gone, so one started next does not meet it
dctwin_stop() {
  pkill -x "$1" 2>/dev/null || return 0
  n=0
  while pgrep -x "$1" >/dev/null 2>&1 && [ $n -lt 50 ]; do sleep 0.1; n=$((n+1)); done
}
echo "== system"
if [ -n "$6" ]; then
  printf '%s\n' "$6" > /etc/hostname
  grep -v '^127\.0\.1\.1[[:space:]]' /etc/hosts > /run/dctwin-hosts 2>/dev/null || true
  { cat /run/dctwin-hosts; printf '127.0.1.1\t%s\n' "$6"; } > /etc/hosts
  [ "$(hostname 2>/dev/null)" = "$6" ] || hostname "$6" 2>&1 || echo "hostname $6: not set in the container's UTS namespace (/etc/hostname is)"
fi
if [ -n "$7" ]; then
  if [ -e "/usr/share/zoneinfo/$7" ]; then
    printf '%s\n' "$7" > /etc/timezone && ln -sf "/usr/share/zoneinfo/$7" /etc/localtime
  elif [ "$7" != Etc/UTC ]; then
    echo "time zone $7: not in /usr/share/zoneinfo"; rc=1
  fi
fi
echo "== users"
mkdir -p /var/lib/dctwin
touch /var/lib/dctwin/nvue-users
if [ -n "$8" ]; then
  for g in nvshow nvset nvapply; do getent group $g >/dev/null 2>&1 || groupadd -r $g 2>&1 || rc=1; done
fi
: > /run/dctwin-users.new
while read -r name hash groups create gecos; do
  [ -n "$name" ] || continue
  echo "$name" >> /run/dctwin-users.new
  if ! id -u "$name" >/dev/null 2>&1; then
    echo "useradd $name"
    useradd -m -s /bin/bash -c "$gecos" -G "$create" "$name" 2>&1 || { rc=1; continue; }
  fi
  if [ "$groups" != - ]; then usermod -G "$groups" "$name" 2>&1 || rc=1; fi
  if [ -n "$gecos" ]; then usermod -c "$gecos" "$name" 2>&1 || rc=1; fi
  if [ "$hash" != - ] && [ "$(getent shadow "$name" | cut -d: -f2)" != "$hash" ]; then
    echo "password of $name"
    usermod -p "$hash" "$name" 2>&1 && chage -d "$(date +%F)" "$name" 2>&1 || rc=1
  fi
done <<EOF
$8
EOF
while read -r name; do
  # root and the factory account are the image's, never removed by NVUE
  [ -n "$name" ] && [ "$name" != root ] && [ "$name" != cumulus ] || continue
  grep -qx "$name" /run/dctwin-users.new && continue
  echo "userdel $name"
  userdel -r "$name" 2>&1 || true
done < /var/lib/dctwin/nvue-users
mv /run/dctwin-users.new /var/lib/dctwin/nvue-users
echo "== ntp"
if [ -n "$9" ]; then
  if command -v chronyd >/dev/null 2>&1; then
    mkdir -p /etc/chrony /var/lib/chrony
    {
      echo "# rendered by the twin's NVUE (service ntp ${10})"
      if [ "${10}" = mgmt ]; then echo "bindacqdevice mgmt"; fi
      printf '%s\n' "$9"
      echo "driftfile /var/lib/chrony/chrony.drift"
      echo "makestep 1 3"
    } > /run/dctwin-chrony.conf
    if ! cmp -s /run/dctwin-chrony.conf /etc/chrony/chrony.conf || ! pgrep -x chronyd >/dev/null 2>&1; then
      mv /run/dctwin-chrony.conf /etc/chrony/chrony.conf
      dctwin_stop chronyd
      # -x: the container's clock is the host's; chronyd keeps time without setting it
      chronyd -x -f /etc/chrony/chrony.conf 2>&1 || rc=1
      echo "chronyd started"
    else
      echo "unchanged"
    fi
  elif command -v ntpd >/dev/null 2>&1; then
    conf=/etc/ntp.conf; [ -d /etc/ntpsec ] && conf=/etc/ntpsec/ntp.conf
    { echo "# rendered by the twin's NVUE (service ntp ${10})"; printf '%s\n' "$9"; } > "$conf"
    dctwin_stop ntpd
    ntpd -c "$conf" 2>&1 || rc=1
  else
    echo "NTP servers are configured and the switch has no chronyd or ntpd"; rc=1
  fi
  touch /run/dctwin-ntp
elif [ -e /run/dctwin-ntp ]; then
  dctwin_stop chronyd; dctwin_stop ntpd; rm -f /run/dctwin-ntp
  echo "stopped"
fi
echo "== dns"
while ip rule del pref 990 2>/dev/null; do :; done
if [ -n "${11}" ]; then
  printf '%s' "${11}" > /etc/resolv.conf
  touch /run/dctwin-dns
  # a server in the management VRF: queries from the default VRF's sockets go by the VRF's table
  t=$(ip -d link show mgmt 2>/dev/null | sed -n 's/.*vrf table \([0-9]*\).*/\1/p')
  for a in $(sed -n 's/^nameserver \([^ ]*\) # vrf mgmt$/\1/p' /etc/resolv.conf); do
    if [ -n "$t" ]; then ip rule add pref 990 to "$a" table "$t" 2>&1 || rc=1; else echo "nameserver $a: vrf mgmt does not exist"; fi
  done
elif [ -e /run/dctwin-dns ]; then
  echo "# rendered by the twin's NVUE: no DNS servers" > /etc/resolv.conf
  rm -f /run/dctwin-dns
fi
echo "== syslog"
if [ -n "${12}" ]; then
  mkdir -p /etc/rsyslog.d
  printf '%s' "${12}" > /run/dctwin-rsyslog.conf
  if command -v rsyslogd >/dev/null 2>&1; then
    if ! cmp -s /run/dctwin-rsyslog.conf /etc/rsyslog.d/50-dctwin-nvue.conf || ! pgrep -x rsyslogd >/dev/null 2>&1; then
      mv /run/dctwin-rsyslog.conf /etc/rsyslog.d/50-dctwin-nvue.conf
      dctwin_stop rsyslogd
      rm -f /run/rsyslogd.pid
      rsyslogd 2>&1 || rc=1
      echo "rsyslogd started"
    else
      echo "unchanged"
    fi
  elif grep -q omfwd /run/dctwin-rsyslog.conf; then
    echo "syslog servers are configured and the switch has no rsyslogd"; rc=1
  fi
  touch /run/dctwin-syslog
elif [ -e /run/dctwin-syslog ]; then
  rm -f /etc/rsyslog.d/50-dctwin-nvue.conf /run/dctwin-syslog
  dctwin_stop rsyslogd
  echo "stopped"
fi
echo "== lldp"
if [ -n "${13}" ]; then
  printf '%s' "${13}" > /run/dctwin-lldpd.conf
  if command -v lldpd >/dev/null 2>&1; then
    if ! cmp -s /run/dctwin-lldpd.conf /etc/lldpd.conf || ! pgrep -x lldpd >/dev/null 2>&1; then
      mv /run/dctwin-lldpd.conf /etc/lldpd.conf
      dctwin_stop lldpd
      mkdir -p /run/lldpd
      lldpd 2>&1 || rc=1
      echo "lldpd started"
    else
      echo "unchanged"
    fi
  else
    echo "service lldp is on and the switch has no lldpd: no LLDP"
  fi
  touch /run/dctwin-lldp
elif [ -e /run/dctwin-lldp ]; then
  dctwin_stop lldpd; rm -f /run/dctwin-lldp
  echo "stopped"
fi
echo "== messages"
# pre-login: sshd's Banner, which the image's sshd_config names (/etc/issue.net); post-login:
# /etc/motd. A message NVUE no longer sets is emptied
if [ -n "${14}" ]; then
  printf '%s\n' "${14}" > /etc/issue.net && touch /run/dctwin-issue
elif [ -e /run/dctwin-issue ]; then
  : > /etc/issue.net; rm -f /run/dctwin-issue
fi
if [ -n "${15}" ]; then
  printf '%s\n' "${15}" > /etc/motd && touch /run/dctwin-motd
elif [ -e /run/dctwin-motd ]; then
  : > /etc/motd; rm -f /run/dctwin-motd
fi
echo "== snmp"
if [ -n "${16}" ]; then
  printf '%s' "${16}" > /run/dctwin-snmpd.conf
  if command -v snmpd >/dev/null 2>&1; then
    if ! cmp -s /run/dctwin-snmpd.conf /etc/snmp/snmpd.conf || ! pgrep -x snmpd >/dev/null 2>&1; then
      mkdir -p /etc/snmp && mv /run/dctwin-snmpd.conf /etc/snmp/snmpd.conf
      dctwin_stop snmpd
      # -C: this file only, not the package's defaults
      snmpd -C -c /etc/snmp/snmpd.conf -Lf /var/log/snmpd.log -p /run/snmpd.pid 2>&1 || rc=1
      echo "snmpd started"
    else
      echo "unchanged"
    fi
  else
    echo "system snmp-server is on and the switch has no snmpd: no SNMP"
  fi
  touch /run/dctwin-snmp
elif [ -e /run/dctwin-snmp ]; then
  dctwin_stop snmpd; rm -f /run/dctwin-snmp
  echo "stopped"
fi
`

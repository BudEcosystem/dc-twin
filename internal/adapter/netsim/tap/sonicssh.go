package tap

import "strings"

// sonicCommands are SONiC CLI commands that change the switch, strongest first, with their
// canonical names: a reset to the factory (config-setup factory, ztp run), a configuration loaded
// or saved, CONFIG_DB written directly, a file staged on the switch, the rest of what a day-0
// tool's bootstrap changes (accounts, the NOS image, a reboot).
var sonicCommands = []struct{ match, canonical string }{
	{"config-setup factory", "network.config.reset"},
	{"ztp run", "network.config.reset"},
	{"config reload", "network.config.apply"},
	{"config load", "network.config.apply"},
	{"config save", "network.config.save"},
	{"sonic-installer install", "network.firmware.update"},
	{"reboot", "network.reboot"},
	{"sonic-db-cli", "network.config.change"},
	{"redis-cli", "network.config.change"},
	{"config ", "network.config.change"},
	{"useradd", "network.account.write"},
	{"userdel", "network.account.write"},
	{"chpasswd", "network.account.write"},
	{"cat >", "network.config.stage"},
	{"tee ", "network.config.stage"},
}

// sonicCanonical names a SONiC CLI command as a canonical request: the strongest change it makes
// (a compound command, `a && b`, is its strongest part), else a read of what it reads: LLDP's
// neighbours, FRR's state, or the switch's configuration and state.
func sonicCanonical(cmd string) (canonical, method string) {
	c := strings.ToLower(cmd)
	for _, x := range sonicCommands {
		if !strings.Contains(c, x.match) {
			continue
		}
		if x.match == "sonic-db-cli" || x.match == "redis-cli" {
			w := false
			for _, verb := range []string{" del ", " hset ", " hdel ", " set ", " hmset ", " flushdb"} {
				w = w || strings.Contains(c+" ", verb)
			}
			if !w {
				continue
			}
		}
		if x.match == "config " && !strings.Contains(" "+c, " config ") {
			continue
		}
		return x.canonical, strings.TrimSpace(x.match)
	}
	switch {
	case strings.Contains(c, "lldpctl") || strings.Contains(c, "lldpcli") || strings.Contains(c, "show lldp"):
		return "network.lldp.read", "lldpctl"
	case strings.Contains(c, "vtysh"):
		return "network.bgp.read", "vtysh"
	}
	f := strings.Fields(c)
	m := ""
	for _, w := range f {
		if w != "sudo" && !strings.HasPrefix(w, "-") && w != "''" {
			m = w
			break
		}
	}
	return "network.config.read", m
}

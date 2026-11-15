package tap

import "testing"

// Each command a day-0 tool runs on SONiC over SSH, as the canonical request the twin records.
func TestSonicCanonical(t *testing.T) {
	for cmd, want := range map[string]string{
		"sudo -S -p '' config load -y /tmp/c.json":                                  "network.config.apply",
		"sudo -S -p '' config save -y":                                              "network.config.save",
		"sonic-db-cli CONFIG_DB DEL 'BGP_NEIGHBOR|10.0.0.1'":                        "network.config.change",
		"cat > /tmp/foundry-config.json":                                            "network.config.stage",
		"sudo -S -p '' config-setup factory && sudo ztp enable && sudo ztp run -y":  "network.config.reset",
		"sudo -S -p '' config vrf add mgmt && sudo config save -y":                  "network.config.save",
		"sudo -S -p '' docker exec -i lldp lldpctl -f json":                         "network.lldp.read",
		"sudo -S -p '' docker exec -i bgp vtysh -c 'show bgp vrf all summary json'": "network.bgp.read",
		"sonic-cfggen -d --var-json PORT":                                           "network.config.read",
		"sonic-db-cli CONFIG_DB HGET 'DEVICE_METADATA|localhost' hostname":          "network.config.read",
		"ip -j link show": "network.config.read",
		"sudo useradd -m -s /bin/bash -G sudo,docker foundry": "network.account.write",
		// read by its words, not its substrings
		"show reboot-cause":                              "network.config.read",
		"last reboot":                                    "network.config.read",
		"show runningconfiguration all":                  "network.config.read",
		"ls /etc/sonic 2>/dev/null":                      "network.config.read",
		"sudo sonic-cfggen -j /tmp/c.json --write-to-db": "network.config.change",
		"scp -t /etc/sonic/config_db.json":               "network.config.stage",
		"scp -f /etc/sonic/config_db.json":               "network.config.read",
		"echo 'nameserver 10.0.0.1' > /etc/resolv.conf":  "network.config.stage",
		"sudo reboot":                                    "network.reboot",
		"sudo sh -c 'config save -y'":                    "network.config.save",
		"docker exec bgp vtysh -c 'configure terminal' -c 'router bgp 65000'": "network.config.change",
		"sonic-db-cli CONFIG_DB HGET 'X|config save' y":                       "network.config.read",
	} {
		if got, _ := sonicCanonical(cmd); got != want {
			t.Errorf("%q: %s, want %s", cmd, got, want)
		}
	}
}

// An SSH command's record is taken from dctwin-ssh-command run by sshd (directly, or through the
// account's shell), not from a process that runs it from a shell of its own.
func TestSSHCommandPeer(t *testing.T) {
	type p struct {
		comm string
		ppid int
	}
	procs := map[int]p{
		10: {"sshd-session", 1}, 11: {"bash", 10}, 12: {"dctwin-ssh-com", 11}, // a ForceCommand through bash
		13: {"dctwin-ssh-com", 10},                   // run directly
		14: {"bash", 12}, 15: {"dctwin-ssh-com", 14}, // run again from the login shell it started
		16: {"python3", 1}, 17: {"dctwin-ssh-com", 16}, // from a script
		18: {"sudo", 11}, // not it at all
	}
	proc := func(pid int) (string, int, bool) { x, ok := procs[pid]; return x.comm, x.ppid, ok }
	for pid, want := range map[int]bool{12: true, 13: true, 15: false, 17: false, 18: false, 99: false} {
		if got := sshCommandPeer(pid, proc); got != want {
			t.Errorf("pid %d: %v, want %v", pid, got, want)
		}
	}
}

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
	} {
		if got, _ := sonicCanonical(cmd); got != want {
			t.Errorf("%q: %s, want %s", cmd, got, want)
		}
	}
}

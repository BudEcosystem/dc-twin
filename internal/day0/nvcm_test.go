package day0

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// The design as NVCM's blueprint: each Cumulus leaf and spine with its NVCM role, serial,
// management MAC and reserved address, loopback, fabric /31s cabled to their peers, AS; the site's
// option 239 by reservation, and the aggregates covering every address.
func TestNVCMBlueprint(t *testing.T) {
	d := Design{Estate: "lab", Plan: "given", Release: "5.14.0",
		Mgmt: []MgmtNet{{Network: "oob", Rack: "site-1/hall-a/n01", Prefix: "10.30.3.0/24", Gateway: "10.30.3.1"}},
		Switches: []Switch{
			{Path: "s/n01/leaf-1", Hostname: "n01-leaf-1", Role: "leaf", Rack: "n01", Serial: "SN1", MgmtMAC: "44:38:39:00:00:01", NOS: "generic/nvue-frr", Factory: true,
				ASN: 65101, Loopback: "10.255.255.1/32", Interfaces: []Interface{{Name: "swp64", Kind: "fabric", Address: "10.255.0.1/31", Peer: "n01-spine-1:swp1"}}},
			{Path: "s/n01/spine-1", Hostname: "n01-spine-1", Role: "spine", Rack: "n01", Serial: "SN2", MgmtMAC: "44:38:39:00:00:02", NOS: "generic/nvue-frr", Factory: true,
				ASN: 65001, Loopback: "10.255.255.9/32", Interfaces: []Interface{{Name: "swp1", Kind: "fabric", Address: "10.255.0.0/31", Peer: "n01-leaf-1:swp64"}}},
			{Path: "s/n01/sonic", Hostname: "n01-sonic", Role: "leaf", Rack: "n01", NOS: "sonic-vs", Factory: true},
			{Path: "s/n01/oob", Hostname: "n01-oob", Role: "oob", Rack: "n01", NOS: "generic/nvue-frr", MgmtMAC: "44:38:39:00:00:09"},
		}}
	files, err := nvcmFiles(d, d.Release)
	if err != nil {
		t.Fatal(err)
	}
	bp := filepath.Join("context", NVCMBlueprint)
	var dev struct {
		Data struct {
			Device map[string]any `json:"device"`
		} `json:"data"`
	}
	if err := json.Unmarshal(files[filepath.Join(bp, "devices", "n01-leaf-1.json")], &dev); err != nil {
		t.Fatal(err)
	}
	dv := dev.Data.Device
	if dv["serial"] != "SN1" || dv["role"].(map[string]any)["name"] != "In-Band-Leaf" || dv["primary_ip4_interface"] != "lo" {
		t.Errorf("leaf: %v", dv)
	}
	b, _ := json.Marshal(dv)
	for _, want := range []string{`"mac_address":"44:38:39:00:00:01"`, `"address":"10.30.3.200/24"`, `"address":"10.255.255.1/32"`,
		`"connected_interface":{"device":{"name":"n01-spine-1"},"name":"swp1"}`, `"version":"5.14.0"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("leaf has no %s: %s", want, b)
		}
	}
	for _, n := range []string{"n01-sonic", "n01-oob"} {
		if _, ok := files[filepath.Join(bp, "devices", n+".json")]; ok {
			t.Errorf("%s (SONiC, or configured by the twin) in NVCM's blueprint", n)
		}
	}
	if got := NVCMMgmtAddrs(d)["n01-spine-1"]; got != "10.30.3.201/24" {
		t.Errorf("spine's reservation %q", got)
	}
	var loc map[string]any
	if err := yaml.Unmarshal(files[filepath.Join(bp, "locations.yaml")], &loc); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(files[filepath.Join(bp, "locations.yaml")]), "cumulus-provision-url: http://{{ztp_server}}/v1/device/{{device_id}}/boot-script") {
		t.Errorf("no option 239 reservation:\n%s", files[filepath.Join(bp, "locations.yaml")])
	}
	if bgp := string(files[filepath.Join(bp, "bgp_routing_instances.yaml")]); strings.Count(bgp, "peer_device:") != 1 ||
		!strings.Contains(bgp, "source_interface: swp64") {
		t.Errorf("one peering for the one link:\n%s", bgp)
	}
	for _, p := range []string{"10.30.3.0/24", "10.255.255.0/24", "10.255.0.0/24"} {
		if !strings.Contains(string(files[filepath.Join(bp, "prefixes.yaml")]), p) {
			t.Errorf("no aggregate %s:\n%s", p, files[filepath.Join(bp, "prefixes.yaml")])
		}
	}
}

// oobLeaves is n factory OOB-Leafs, one to a rack, each with its rack's management network and
// BMC segment.
func oobLeaves(n int) Design {
	d := Design{Estate: "lab", Plan: "given"}
	for i := 1; i <= n; i++ {
		rack := fmt.Sprintf("r%02d", i)
		d.Mgmt = append(d.Mgmt, MgmtNet{Network: "oob", Rack: "site-1/hall-a/" + rack, Prefix: fmt.Sprintf("10.20.%d.0/24", i)})
		d.Switches = append(d.Switches, Switch{Path: "site-1/hall-a/" + rack + "/mgmt-switch", Hostname: rack + "-mgmt-switch", Role: "oob", Rack: rack,
			NOS: "generic/nvue-frr", Factory: true, MgmtMAC: fmt.Sprintf("44:38:39:00:01:%02x", i),
			Vlans: []Vlan{{Name: "Vlan1000", ID: 1000, Network: "oob", Gateway: fmt.Sprintf("10.30.%d.1/24", i), Ports: []string{"swp1"}}}})
	}
	return d
}

// The BMC DHCP leases none of VLAN 201's addresses the OOB-Leafs hold: at least NICo's ten are
// reserved, and more when the leaves' SVIs (from the third address) reach past them.
func TestNVCMBMCReserve(t *testing.T) {
	for _, c := range []struct{ leaves, reserve int }{{2, 10}, {7, 10}, {9, 12}} {
		files, err := nvcmFiles(oobLeaves(c.leaves), "")
		if err != nil {
			t.Fatal(err)
		}
		var h struct {
			Vlans []struct {
				Prefix  string `json:"prefix"`
				Reserve int    `json:"reserve"`
			} `json:"vlans"`
		}
		if err := json.Unmarshal(files[filepath.Join("context", NVCMBlueprint, "helpers.json")], &h); err != nil || len(h.Vlans) != 1 {
			t.Fatalf("%d leaves: helpers.json: %v", c.leaves, err)
		}
		if h.Vlans[0].Reserve != c.reserve {
			t.Errorf("%d leaves: reserve %d, want %d", c.leaves, h.Vlans[0].Reserve, c.reserve)
		}
		net := netip.MustParsePrefix(h.Vlans[0].Prefix)
		for name, b := range files {
			if !strings.HasPrefix(name, filepath.Join("context", NVCMBlueprint, "devices")) {
				continue
			}
			var dev struct {
				Data struct {
					Device struct {
						Interfaces []struct {
							Name string `json:"name"`
							IPs  []struct {
								Host string `json:"host"`
							} `json:"ip_addresses"`
						} `json:"interfaces"`
					} `json:"device"`
				} `json:"data"`
			}
			if err := json.Unmarshal(b, &dev); err != nil {
				t.Fatal(err)
			}
			for _, it := range dev.Data.Device.Interfaces {
				if it.Name != fmt.Sprintf("vlan%d", NVCMBMCVLAN) {
					continue
				}
				a := netip.MustParseAddr(it.IPs[0].Host)
				first := net.Addr()
				for k := 0; k < c.reserve; k++ {
					first = first.Next()
				}
				if !a.Less(first) {
					t.Errorf("%d leaves: %s's vlan%d %s is in the BMC pool (from %s)", c.leaves, name, NVCMBMCVLAN, a, first)
				}
			}
		}
	}
}

// Racks of one name in two halls: each switch's reservation is from its own rack's network.
func TestNVCMMgmtAddrsByRackPath(t *testing.T) {
	d := Design{Estate: "lab", Plan: "given",
		Mgmt: []MgmtNet{{Rack: "site-1/hall-a/r01", Prefix: "10.20.1.0/24"}, {Rack: "site-1/hall-b/r01", Prefix: "10.20.2.0/24"}},
		Switches: []Switch{
			{Path: "site-1/hall-a/r01/mgmt-switch", Hostname: "a-r01-mgmt", Role: "oob", Rack: "r01", NOS: "generic/nvue-frr", Factory: true},
			{Path: "site-1/hall-b/r01/mgmt-switch", Hostname: "b-r01-mgmt", Role: "oob", Rack: "r01", NOS: "generic/nvue-frr", Factory: true},
		}}
	got := NVCMMgmtAddrs(d)
	if got["a-r01-mgmt"] != "10.20.1.200/24" || got["b-r01-mgmt"] != "10.20.2.200/24" {
		t.Errorf("reservations %v", got)
	}
}

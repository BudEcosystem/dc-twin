package day0

import (
	"encoding/json"
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

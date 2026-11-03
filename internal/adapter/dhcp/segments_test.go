package dhcp

import (
	"net/netip"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/compile"
)

// A network without DHCP still has a rack's segment when it has an IPv4 CIDR: a real switch
// carries it as a VLAN with its gateway (D57), numbered as a DHCP network's would be. A network
// without a CIDR has none, and DHCP networks are what PlanSegments gives.
func TestNetworkSegments(t *testing.T) {
	est, _, err := compile.LoadEstate("../../../test/linuxnet/testdata/tap-vm-nos.yaml")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: "x86_64"}) // its vm hosts are x86_64, run under KVM
	if err != nil {
		t.Fatal(err)
	}
	m := res.Model
	fe := NetworkSegments(m, "frontend")
	if len(fe) != 2 {
		t.Fatalf("frontend: %d segments, want one per server rack: %+v", len(fe), fe)
	}
	for i, s := range fe {
		if s.Network != "frontend" || s.Index != i || s.Prefix != netip.MustParsePrefix([]string{"10.50.0.0/24", "10.50.1.0/24"}[i]) ||
			s.Gateway != s.Prefix.Addr().Next() || len(s.Switches) != 1 {
			t.Errorf("frontend segment %d: %+v", i, s)
		}
	}
	var prov []Segment
	for _, s := range PlanSegments(m) {
		if s.Network == "provisioning" {
			prov = append(prov, s)
		}
		if s.Network == "frontend" {
			t.Errorf("PlanSegments gives frontend, which has no DHCP: %+v", s)
		}
	}
	got := NetworkSegments(m, "provisioning")
	if len(got) != len(prov) || len(got) != 2 || got[0].Prefix != prov[0].Prefix || got[1].Gateway != prov[1].Gateway {
		t.Errorf("provisioning: NetworkSegments %+v, PlanSegments %+v", got, prov)
	}
	m.Networks["frontend"].CIDR = ""
	if s := NetworkSegments(m, "frontend"); len(s) != 0 {
		t.Errorf("no cidr: %+v", s)
	}
	if s := NetworkSegments(m, "nonesuch"); s != nil {
		t.Errorf("no network: %+v", s)
	}
}

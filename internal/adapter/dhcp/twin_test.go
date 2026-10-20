package dhcp

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// The unmodified lab estate addresses its OOB network by `dhcp: twin`: every BMC, sub-device
// BMCs (BlueField DPUs) included, leases from the twin site DHCP through its rack's twin relay,
// and the lease is in the model.
func TestTwinDHCPLeasesEveryLabBMC(t *testing.T) {
	tw, a := up(t, loadLab(t), func(a *Adapter) { a.leaseTime = 6 * time.Second })
	if len(a.endpoints) < 150 {
		t.Fatalf("only %d BMC endpoints", len(a.endpoints))
	}
	took := waitLeased(t, a, 60*time.Second)
	t.Logf("%d BMC endpoints leased through the twin relays in %s", len(a.endpoints), took.Round(time.Millisecond))
	ls := leases(tw, a)
	checkRelayFacts(t, tw, a, ls)

	// the twin site DHCP hands each port the address its cabling implies
	ns := a.nets["oob"]
	if ns.plan.scheme != schemeCompact {
		t.Errorf("lab oob (/20, rows of 6, 6, 4) scheme %s, want compact", ns.plan.scheme)
	}
	for path, want := range map[string]string{
		"site-1/hall-a/a01/compute-tray-01":       "10.30.0.11", // a01 = segment 0, mgmt-switch-1:eth1
		"site-1/hall-a/a01/compute-tray-01/bf3-0": "10.30.0.12", // its DPU's BMC, eth2
		"site-1/hall-a/b01/server-01":             "10.30.6.13", // b01 = row 1 × 6 + 0, eth3
		"site-1/hall-a/c03/pdu-a":                 "10.30.14.11",
	} {
		if got := ls[path].IP; got != want {
			t.Errorf("%s leased %s, want %s", path, got, want)
		}
		if ls[path].Server != ns.plan.serverID().String() {
			t.Errorf("%s: server %s, want the twin site DHCP %s", path, ls[path].Server, ns.plan.serverID())
		}
	}
	if got := ls["site-1/hall-a/a01/compute-tray-01"].VendorClass; got != "NVIDIA/OpenBMC" {
		t.Errorf("GB300 tray vendor class %q, want the dialect's NVIDIA/OpenBMC", got)
	}
	if got := ls["site-1/hall-a/a01/compute-tray-01/bf3-0"].VendorClass; got != "NVIDIA/BF/BMC" {
		t.Errorf("BlueField-3 BMC vendor class %q, want the dialect's NVIDIA/BF/BMC, which NICo's DHCP recognises", got)
	}
	if got := ls["site-1/hall-a/a01/compute-tray-01"].Circuit; got != "a01-mgmt-switch-1:eth1" {
		t.Errorf("circuit id %q", got)
	}

	// committed: one lease event per BMC, sourced as the twin's
	evs := tw.Engine.Events(0, func(e schema.Event) bool { return e.State != nil && e.State.Field == "lease" })
	if len(evs) < len(a.endpoints) {
		t.Errorf("%d lease events for %d endpoints", len(evs), len(a.endpoints))
	}
	for _, e := range evs[:1] {
		if e.Source.Class != "twin" || e.Data["circuitId"] == nil {
			t.Errorf("lease event %+v", e)
		}
	}

	// renewals at T1 (3 s of a 6 s lease) keep every address
	time.Sleep(4 * time.Second)
	if n := a.renewals.Load(); n < int64(len(a.endpoints)) {
		t.Errorf("%d renewals after T1 for %d endpoints", n, len(a.endpoints))
	}
	after := leases(tw, a)
	for p, l := range ls {
		if after[p].IP != l.IP {
			t.Errorf("%s: %s renewed as %s", p, l.IP, after[p].IP)
		}
	}

	// a BMC reset: the controller goes down and comes back (OnBMCUp), and the client asks for
	// its address again
	dev := "site-1/hall-a/b02/server-01"
	ep := a.endpoints[dev]
	if err := tw.Engine.ResetBMC(engine.Request{Path: dev, Canonical: "bmc.reset", Protocol: "test",
		Source: schema.EventSource{Class: "scenario", Principal: "test"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "an INIT-REBOOT after the BMC reset", func() bool { return ep.reboots.Load() > 0 })
	evs = tw.Engine.Events(0, func(e schema.Event) bool {
		return e.Path == dev && e.State != nil && e.State.Field == "bmc"
	})
	if len(evs) < 2 {
		t.Errorf("the BMC reset left %d bmc state events", len(evs))
	}
	if got := leases(tw, a)[dev].IP; got != ls[dev].IP {
		t.Errorf("after a BMC reset %s leased %s, want its address %s again", dev, got, ls[dev].IP)
	}

	// a pulled cable silences the port; replacing it brings the client back for its address
	dev2 := "site-1/hall-a/c01/server-01"
	ep2 := a.endpoints[dev2]
	link := ep2.port.Link.ID
	if err := tw.Engine.SetLink(link, false, schema.EventSource{Class: "scenario", Principal: "test"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(4 * time.Second) // past a T1: the renewal finds no carrier
	acks := ep2.acks.Load()
	time.Sleep(3 * time.Second)
	if ep2.acks.Load() != acks {
		t.Errorf("%s was ACKed with its cable pulled", dev2)
	}
	if err := tw.Engine.SetLink(link, true, schema.EventSource{Class: "scenario", Principal: "test"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "a lease after the cable is replaced", func() bool { return ep2.acks.Load() > acks })

	rep := a.Report()
	t.Logf("report: served %d", rep.Served)
	for _, l := range rep.Lines {
		t.Logf("  %s", l)
	}
	joined := strings.Join(rep.Lines, "\n")
	for _, want := range []string{"TWIN-PROVIDED", "circuit id", "link selection", "renewals at T1", fmt.Sprintf("%d of %d BMC ports leased", len(a.endpoints), len(a.endpoints))} {
		if !strings.Contains(joined, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	if len(rep.IsNot) == 0 {
		t.Error("report has no isNot")
	}
}

// Segment addressing is a pure function of the hall layout: adding a rack renumbers nothing.
func TestSegmentsDoNotMoveWhenARackIsAdded(t *testing.T) {
	est := loadLab(t)
	before := segmentsOf(t, est)
	est2 := loadLab(t)
	rows := est2.Spec.Sites[0].Halls[0].Rows
	rows[2].Racks = append(rows[2].Racks, schema.Rack{Name: "c04", Position: 4, Format: "generic/cpu-rack",
		Params: map[string]any{"server": "generic/server-2u", "servers": 2}})
	after := segmentsOf(t, est2)
	for k, p := range before {
		if after[k] != p {
			t.Errorf("%s moved from %s to %s", k, p, after[k])
		}
	}
	if after["oob site-1/hall-a/c04"] != netip.MustParsePrefix("10.30.15.0/24") {
		t.Errorf("c04 got %s", after["oob site-1/hall-a/c04"])
	}
}

func segmentsOf(t *testing.T, est *schema.Estate) map[string]netip.Prefix {
	t.Helper()
	m := compileEstate(t, est)
	out := map[string]netip.Prefix{}
	for _, s := range PlanSegments(m) {
		out[s.Network+" "+s.Rack] = s.Prefix
	}
	return out
}

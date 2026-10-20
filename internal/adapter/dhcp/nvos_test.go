package dhcp

import (
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
	"github.com/insomniacslk/dhcp/dhcpv4"
)

// Every NVLink switch tray's NVOS management port (mgmt0) is a DHCP client of its own, relayed
// with its own port's circuit id, while the tray is powered on: it leases an address in its
// rack's segment that is not the BMC's, the lease is committed as a state event on the port, a
// power-off silences the client at once (while the BMC, on standby power, keeps renewing), and
// a power-on brings NVOS back for the address it had (INIT-REBOOT).
func TestNVOSLeasesWhileTrayIsOn(t *testing.T) {
	tw, a := up(t, loadLab(t), func(a *Adapter) { a.leaseTime = 6 * time.Second })
	if len(a.nosEPs) != 18 {
		t.Fatalf("%d NVOS endpoints, want the lab's 18 switch trays' mgmt0", len(a.nosEPs))
	}
	waitFor(t, 30*time.Second, "every NVOS management port leased", func() bool { return len(a.NVOSLeases()) == len(a.nosEPs) })
	bmcLeases := leases(tw, a)
	nos := a.NVOSLeases()
	seen := map[string]string{}
	for ref, ep := range a.nosEPs {
		ip := nos[ref]
		addr, err := netip.ParseAddr(ip)
		if err != nil || !ep.seg.Prefix.Contains(addr) {
			t.Errorf("%s: NVOS leased %s, outside its segment %s", ref, ip, ep.seg.Prefix)
		}
		bmc := bmcLeases[ep.dev.Path]
		if bmc.IP == ip || bmc.Circuit == ep.circuit {
			t.Errorf("%s: NVOS (%s, %s) shares the BMC's address or port (%s, %s)", ref, ip, ep.circuit, bmc.IP, bmc.Circuit)
		}
		if ep.port.Name != "mgmt0" || ep.circuit != circuitID(ep.sw, ep.swPort) {
			t.Errorf("%s: port %s, circuit %q", ref, ep.port.Name, ep.circuit)
		}
		if prev, dup := seen[ip]; dup {
			t.Errorf("%s and %s share %s", prev, ref, ip)
		}
		seen[ip] = ref
	}

	// what NVOS sends: its host name and Debian dhclient's request list, no vendor class, no
	// client identifier (a BMC's client sends both)
	tray := "site-1/hall-a/a01/switch-tray-03"
	ref := tray + ":mgmt0"
	ep := a.nosEPs[ref]
	m := ep.message(dhcpv4.MessageTypeDiscover, ep.xid())
	if m.ClassIdentifier() != "" || m.Options.Has(dhcpv4.OptionClientIdentifier) {
		t.Errorf("NVOS sent option 60 %q / option 61", m.ClassIdentifier())
	}
	if want := "nvos-" + strings.ToLower(ep.dev.Identity.Serial); m.HostName() != want {
		t.Errorf("NVOS host name %q, want %q", m.HostName(), want)
	}
	if !m.IsOptionRequested(dhcpv4.OptionClasslessStaticRoute) || !m.IsOptionRequested(dhcpv4.OptionNTPServers) {
		t.Errorf("NVOS request list %v", m.ParameterRequestList())
	}

	// committed as a state event on the port's path, not as the device's lease
	evs := tw.Engine.Events(0, func(e schema.Event) bool { return e.Path == ref && e.State != nil && e.State.Field == "lease" })
	if len(evs) != 1 || evs[0].Kind != "state" || evs[0].State.To != nos[ref] || evs[0].Data["os"] != "nvos" ||
		evs[0].Data["circuitId"] != ep.circuit || evs[0].Data["remoteId"] != switchMAC(ep.sw).String() || evs[0].Source.Class != "twin" {
		t.Fatalf("NVOS lease events %+v", evs)
	}
	t.Logf("NVOS lease event: %s (%v)", evs[0].Message, evs[0].Data)
	var devLease string
	tw.Engine.Read(func(m *model.Model) { devLease = m.DeviceBy[tray].State.Lease.IP })
	if devLease != bmcLeases[tray].IP {
		t.Errorf("the tray's model lease is %s, want the BMC's %s", devLease, bmcLeases[tray].IP)
	}

	// power off: NVOS's client stops at once and stays silent past T1; the BMC renews on
	src := schema.EventSource{Class: "scenario", Principal: "test"}
	if err := tw.Engine.Power(engine.Request{Path: tray, Canonical: "power.off.force", Protocol: "test", Source: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "NVOS's client to stop with its tray", func() bool { return !ep.running() })
	if a.nvosUp(ep.dev) {
		t.Fatal("NVOS is up on a tray that is off")
	}
	nvosAcks, bmcAcks := ep.acks.Load(), a.endpoints[tray].acks.Load()
	other := a.nosEPs["site-1/hall-a/a01/switch-tray-04:mgmt0"]
	otherAcks := other.acks.Load()
	time.Sleep(4 * time.Second) // past T1 (3 s of a 6 s lease)
	if ep.acks.Load() != nvosAcks {
		t.Errorf("NVOS was ACKed %d times with its tray off", ep.acks.Load()-nvosAcks)
	}
	if a.endpoints[tray].acks.Load() == bmcAcks {
		t.Error("the tray's BMC stopped renewing when the tray powered off (it runs on standby power)")
	}
	if other.acks.Load() == otherAcks {
		t.Error("another tray's NVOS stopped renewing")
	}
	if a.NVOSLeases()[ref] != nos[ref] {
		t.Errorf("the lease record moved to %q while NVOS was off", a.NVOSLeases()[ref])
	}

	// power on: NVOS asks for its address again and gets it; no new lease event (it did not move)
	if err := tw.Engine.Power(engine.Request{Path: tray, Canonical: "power.on", Protocol: "test", Source: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "NVOS's INIT-REBOOT after the tray powered on", func() bool { return ep.reboots.Load() > 0 })
	if !ep.running() || a.NVOSLeases()[ref] != nos[ref] {
		t.Errorf("after power-on: running %v, lease %s, want %s", ep.running(), a.NVOSLeases()[ref], nos[ref])
	}
	if evs := tw.Engine.Events(0, func(e schema.Event) bool { return e.Path == ref && e.State != nil && e.State.Field == "lease" }); len(evs) != 1 {
		t.Errorf("%d lease events after a power cycle at the same address", len(evs))
	}

	rep := a.Report()
	joined := strings.Join(rep.Lines, "\n")
	for _, want := range []string{"18 of 18 NVOS management ports leased", "NVOS: 18 NVLink switch tray NVOS management ports"} {
		if !strings.Contains(joined, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	for _, l := range rep.Lines {
		if strings.Contains(l, "NVOS") {
			t.Logf("report: %s", l)
		}
	}
}

// A tray that is off when the twin starts has no NVOS client running until it is powered on.
func TestNVOSWaitsForPowerOn(t *testing.T) {
	est := loadLab(t)
	est.Spec.InitialState = "off"
	tw, a := up(t, est, nil)
	tray := "site-1/hall-a/a02/switch-tray-01"
	ep := a.nosEPs[tray+":mgmt0"]
	if ep == nil {
		t.Fatal("no NVOS endpoint")
	}
	time.Sleep(3 * time.Second) // past the start window
	if ep.running() || len(a.NVOSLeases()) != 0 {
		t.Fatalf("NVOS DHCPs on trays that are off: running %v, leases %v", ep.running(), a.NVOSLeases())
	}
	if len(a.Leases()) == 0 {
		t.Error("no BMC leased: BMCs run on standby power")
	}
	// the tray's precondition is its rack's power shelves delivering
	src := schema.EventSource{Class: "scenario", Principal: "test"}
	var shelves []*model.Device
	tw.Engine.Read(func(m *model.Model) {
		for _, d := range m.RackBy["site-1/hall-a/a02"].Devices {
			if d.BaseClass == "power-shelf" {
				shelves = append(shelves, d)
			}
		}
	})
	for _, d := range shelves {
		if err := tw.Engine.Power(engine.Request{Path: d.Path, Canonical: "power.on", Protocol: "test", Source: src}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 20*time.Second, "a02's power shelves on", func() bool {
		on := true
		tw.Engine.Read(func(*model.Model) {
			for _, d := range shelves {
				on = on && d.State.Power == model.On
			}
		})
		return on
	})
	if len(a.NVOSLeases()) != 0 {
		t.Fatal("NVOS leased while its tray was off")
	}
	if err := tw.Engine.Power(engine.Request{Path: tray, Canonical: "power.on", Protocol: "test", Source: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "NVOS's lease after power-on", func() bool { return a.NVOSLeases()[tray+":mgmt0"] != "" })
	if n := len(a.NVOSLeases()); n != 1 {
		t.Errorf("%d NVOS leases, want only the powered-on tray's", n)
	}
}

// The NVOS port rule: a switch tray's management ports other than its BMC's.
func TestNVOSPorts(t *testing.T) {
	m := compileEstate(t, loadLab(t))
	trays := 0
	for _, d := range m.Devices {
		ps := twin.NVOSPorts(d)
		if d.BaseClass != "switch-tray" {
			if len(ps) != 0 {
				t.Errorf("%s (%s): NVOS ports %v", d.Path, d.BaseClass, ps)
			}
			continue
		}
		trays++
		if len(ps) != 1 || ps[0].Name != "mgmt0" || ps[0].MAC == d.Identity.BMCMAC || ps[0].Link == nil {
			t.Errorf("%s: NVOS ports %v", d.Path, ps)
		}
	}
	if trays != 18 {
		t.Errorf("%d switch trays", trays)
	}
}

// The relay's remote id is the switch's MAC as text, which NICo reads as UTF-8.
func TestRelayRemoteIDIsText(t *testing.T) {
	mac, _ := net.ParseMAC("b8:3f:d2:13:04:d4")
	so := subOptions(raiBytes("a03-mgmt-switch-1:eth16", mac, net.IPv4(10, 30, 2, 1), 24333))
	if got := string(so[subRemoteID]); got != "b8:3f:d2:13:04:d4" {
		t.Errorf("remote id %q", got)
	}
	if string(so[subCircuitID]) != "a03-mgmt-switch-1:eth16" || !net.IP(so[subLinkSelection]).Equal(net.IPv4(10, 30, 2, 1)) {
		t.Errorf("sub-options %q", so)
	}
}

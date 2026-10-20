package dhcp

import (
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// Two DHCP clients behind one switch port, as a BlueField's out-of-band port carries its BMC and
// its Arm (D52): the Arm, plugged into the BMC's port by its own MAC, leases from the twin site
// DHCP through the twin relay, from the dynamic range (the port's cabled address is the BMC's),
// and the BMC keeps its lease and its renewals: no reply for one client is delivered to the other.
func TestTwoClientsBehindOnePort(t *testing.T) {
	tw, a := up(t, loadLab(t), func(a *Adapter) { a.leaseTime = 6 * time.Second })
	waitLeased(t, a, 60*time.Second)
	const bmc = "site-1/hall-a/a01/compute-tray-01/bf3-0"
	ep := a.endpoints[bmc]
	if ep == nil {
		t.Fatalf("no BMC endpoint for %s", bmc)
	}
	bmcIP := leases(tw, a)[bmc].IP
	if bmcIP != "10.30.0.12" {
		t.Fatalf("the BlueField's BMC leased %s, want its port's 10.30.0.12", bmcIP)
	}
	armMAC := append(net.HardwareAddr(nil), ep.mac...)
	armMAC[5]++

	r, err := tw.Net.PlugRelay(ep.port.Ref, armMAC)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if gw, _ := r.Gateway(); gw.String() != "10.30.0.1/24" {
		t.Fatalf("the Arm's segment gateway %s, want the BMC's segment's 10.30.0.1/24", gw)
	}
	replies := make(chan *dhcpv4.DHCPv4, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Receive(buf)
			if err != nil {
				close(replies)
				return
			}
			if m, err := dhcpv4.FromBytes(buf[:n]); err == nil {
				replies <- m
			}
		}
	}()
	foreign := 0
	ask := func(m *dhcpv4.DHCPv4) *dhcpv4.DHCPv4 {
		t.Helper()
		if err := r.Send(m.ToBytes()); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(5 * time.Second)
		for {
			select {
			case reply := <-replies:
				if reply.ClientHWAddr.String() != armMAC.String() {
					foreign++ // the BMC's reply delivered to the Arm
					continue
				}
				if reply.TransactionID == m.TransactionID {
					return reply
				}
			case <-deadline:
				t.Fatalf("no reply to the Arm's %s", m.MessageType())
			}
		}
	}

	acksBefore := ep.acks.Load()
	disc, _ := dhcpv4.NewDiscovery(armMAC)
	offer := ask(disc)
	armIP, _ := netip.AddrFromSlice(offer.YourIPAddr.To4())
	if offer.MessageType() != dhcpv4.MessageTypeOffer || armIP != netip.MustParseAddr("10.30.0.200") {
		t.Fatalf("the Arm was offered %s (%s), want the dynamic range's first, 10.30.0.200", armIP, offer.MessageType())
	}
	req, _ := dhcpv4.NewRequestFromOffer(offer)
	if ack := ask(req); ack.MessageType() != dhcpv4.MessageTypeAck || !ack.YourIPAddr.Equal(offer.YourIPAddr) ||
		!ack.Router()[0].Equal(net.ParseIP("10.30.0.1")) {
		t.Fatalf("the Arm's request: %v", ack)
	}

	// past the BMC's T1 (3 s of 6): its renewals still reach it, and it keeps its address
	time.Sleep(4 * time.Second)
	if ep.acks.Load() <= acksBefore {
		t.Errorf("the BMC was ACKed %d times before the Arm plugged in and %d after its T1: its renewals went elsewhere", acksBefore, ep.acks.Load())
	}
	if got := leases(tw, a)[bmc].IP; got != bmcIP {
		t.Errorf("the BMC's lease moved from %s to %s", bmcIP, got)
	}
	// the Arm renews (RENEWING: ciaddr, no requested address) and keeps its own
	renew, _ := dhcpv4.New(dhcpv4.WithHwAddr(armMAC), dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
		dhcpv4.WithClientIP(offer.YourIPAddr), dhcpv4.WithBroadcast(false))
	if ack := ask(renew); ack.MessageType() != dhcpv4.MessageTypeAck || !ack.YourIPAddr.Equal(offer.YourIPAddr) {
		t.Fatalf("the Arm's renewal: %v", ack)
	}
	if foreign > 0 {
		t.Errorf("%d replies for another client were delivered to the Arm's port", foreign)
	}

	joined := strings.Join(a.Report().Lines, "\n")
	for _, want := range []string{"1 switch ports have more than one DHCP client behind them",
		"(a01-mgmt-switch-1:eth2: ", " to " + ep.mac.String(), " to " + armMAC.String(),
		"1 second DHCP clients behind a port another client holds"} {
		if !strings.Contains(joined, want) {
			t.Errorf("report lacks %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "vm host ports plugged") {
		t.Errorf("a second client was reported as a vm host port:\n%s", joined)
	}
}

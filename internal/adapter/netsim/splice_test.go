package netsim

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

var (
	spGuestMAC  = net.HardwareAddr{0x02, 0, 0, 0, 0, 0x01}
	spSwitchMAC = net.HardwareAddr{0x02, 0, 0, 0, 0, 0xfe}
)

// startSplice splices two in-memory cables: the guest's NIC (guest) and the switch port's TAP
// (sw). A frame the guest sends is put on guest.toPort; what the splice delivers to the switch
// arrives on sw.toGuest; what the switch sends is put on sw.toPort and arrives on guest.toGuest.
func startSplice(t *testing.T) (guest, sw *memCable, sp *Splice, dhcp chan string, flows chan Flow) {
	t.Helper()
	guest, sw = newMemCable(), newMemCable()
	dhcp, flows = make(chan string, 16), make(chan Flow, 16)
	sp = NewSplice(guest, sw, SpliceConfig{Ref: "r01/server-01:eth0",
		OnDHCP:  func(dir string, payload []byte) { dhcp <- dir },
		Observe: func(f Flow) { flows <- f }})
	t.Cleanup(func() { guest.close(); sw.close(); sp.Close() })
	return
}

func recv(t *testing.T, ch chan []byte, what string) []byte {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(2 * time.Second):
		t.Fatalf("no frame %s", what)
		return nil
	}
}

func none[T any](t *testing.T, ch chan T, what string) {
	t.Helper()
	select {
	case x := <-ch:
		t.Fatalf("%s: %v", what, x)
	case <-time.After(100 * time.Millisecond):
	}
}

// tcpFrame is an IPv4 TCP segment with the flags given (no payload; checksums are not read).
func tcpFrame(src, dst netip.AddrPort, flags byte) []byte {
	b := make([]byte, ethHeader+ipv4Header+20)
	copy(b[0:6], spSwitchMAC)
	copy(b[6:12], spGuestMAC)
	binary.BigEndian.PutUint16(b[12:14], 0x0800)
	ip := b[ethHeader:]
	ip[0], ip[8], ip[9] = 0x45, 64, 6
	binary.BigEndian.PutUint16(ip[2:4], uint16(ipv4Header+20))
	s, d := src.Addr().As4(), dst.Addr().As4()
	copy(ip[12:16], s[:])
	copy(ip[16:20], d[:])
	tcp := ip[ipv4Header:]
	binary.BigEndian.PutUint16(tcp[0:2], src.Port())
	binary.BigEndian.PutUint16(tcp[2:4], dst.Port())
	tcp[12], tcp[13] = 5<<4, flags
	return b
}

// Every frame crosses the splice unchanged both ways, whatever it carries: the guest is on the
// switch's segment, and the switch answers its ARP, IPv6 and LLDP as it would a cabled host's.
func TestSpliceCarriesEveryFrameUnchanged(t *testing.T) {
	guest, sw, sp, _, _ := startSplice(t)
	arp := append(append(append([]byte{}, BroadcastMAC...), spGuestMAC...), 0x08, 0x06)
	arp = append(arp, bytes.Repeat([]byte{0xa5}, 28)...)
	ipv6 := append(append(append([]byte{}, spSwitchMAC...), spGuestMAC...), 0x86, 0xdd)
	ipv6 = append(ipv6, bytes.Repeat([]byte{0x60}, 40)...)
	jumbo := append(append(append([]byte{}, spSwitchMAC...), spGuestMAC...), 0x08, 0x00)
	jumbo = append(jumbo, bytes.Repeat([]byte{0x45}, 1486)...)
	for _, f := range [][]byte{arp, ipv6, jumbo} {
		guest.toPort <- f
		if got := recv(t, sw.toGuest, "at the switch"); !bytes.Equal(got, f) {
			t.Errorf("guest → switch changed a %x frame", f[12:14])
		}
		sw.toPort <- f
		if got := recv(t, guest.toGuest, "at the guest"); !bytes.Equal(got, f) {
			t.Errorf("switch → guest changed a %x frame", f[12:14])
		}
	}
	if sp.FromGuest.Load() != 3 || sp.ToGuest.Load() != 3 {
		t.Errorf("counted %d from and %d to the guest, want 3 and 3", sp.FromGuest.Load(), sp.ToGuest.Load())
	}
}

// The guest's DHCP is observed as it passes (D48's observer contract): a request on its way to
// the switch's relay, a reply on its way back. The splice relays nothing itself.
func TestSpliceObservesDHCP(t *testing.T) {
	guest, sw, _, dhcp, flows := startSplice(t)
	discover := UDPFrame{SrcMAC: spGuestMAC, DstMAC: BroadcastMAC, SrcIP: netip.IPv4Unspecified(), DstIP: netip.MustParseAddr("255.255.255.255"),
		SrcPort: 68, DstPort: 67, Payload: bootp(1, true, netip.Addr{})}.Marshal()
	guest.toPort <- discover
	if got := recv(t, sw.toGuest, "DISCOVER at the switch"); !bytes.Equal(got, discover) {
		t.Fatal("the DISCOVER was changed")
	}
	if d := <-dhcp; d != "request" {
		t.Errorf("DISCOVER observed as %q", d)
	}
	offer := UDPFrame{SrcMAC: spSwitchMAC, DstMAC: spGuestMAC, SrcIP: netip.MustParseAddr("10.40.0.1"), DstIP: netip.MustParseAddr("10.40.0.10"),
		SrcPort: 67, DstPort: 68, Payload: bootp(2, false, netip.MustParseAddr("10.40.0.10"))}.Marshal()
	sw.toPort <- offer
	recv(t, guest.toGuest, "OFFER at the guest")
	if d := <-dhcp; d != "reply" {
		t.Errorf("OFFER observed as %q", d)
	}
	none(t, flows, "DHCP observed as a flow")
}

// A connection the guest opens is observed once, as routed by the switch: a TCP SYN (not its
// retransmission, not the far end's SYN-ACK or a segment of an open connection), and the first
// UDP datagram from a source to a destination.
func TestSpliceObservesTheGuestsFlows(t *testing.T) {
	guest, sw, _, dhcp, flows := startSplice(t)
	src, dst := netip.MustParseAddrPort("10.50.0.10:40000"), netip.MustParseAddrPort("10.50.1.10:22")
	syn := tcpFrame(src, dst, 0x02)
	for i := 0; i < 2; i++ { // the SYN and its retransmission
		guest.toPort <- syn
		recv(t, sw.toGuest, "SYN at the switch")
	}
	select {
	case f := <-flows:
		if f.Proto != "tcp" || f.Src != src || f.Dst != dst || f.Service != SplicedService || f.Refused != "" {
			t.Errorf("flow %+v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the SYN was not observed")
	}
	sw.toPort <- tcpFrame(dst, src, 0x12) // SYN-ACK
	recv(t, guest.toGuest, "SYN-ACK at the guest")
	guest.toPort <- tcpFrame(src, dst, 0x10) // ACK
	recv(t, sw.toGuest, "ACK at the switch")
	none(t, flows, "a second flow for one connection")

	u := UDPFrame{SrcMAC: spGuestMAC, DstMAC: spSwitchMAC, SrcIP: src.Addr(), DstIP: netip.MustParseAddr("10.50.1.10"), SrcPort: 5000, DstPort: 53, Payload: []byte("q")}.Marshal()
	guest.toPort <- u
	guest.toPort <- u
	recv(t, sw.toGuest, "UDP at the switch")
	recv(t, sw.toGuest, "UDP at the switch")
	if f := <-flows; f.Proto != "udp" || f.Dst != netip.MustParseAddrPort("10.50.1.10:53") {
		t.Errorf("UDP flow %+v", f)
	}
	none(t, flows, "a second flow for one UDP source and destination")
	none(t, dhcp, "a flow observed as DHCP")
}

// failCable refuses every write, as a guest's cable does while no machine runs.
type failCable struct {
	*memCable
	mu     sync.Mutex
	writes int
}

func (c *failCable) WriteFrame(b []byte) error {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return errors.New("no link")
}

// A frame the far side cannot take (the guest is off) is dropped and counted; the splice keeps
// carrying the other way.
func TestSpliceDropsWhatTheGuestCannotTake(t *testing.T) {
	guest, sw := &failCable{memCable: newMemCable()}, newMemCable()
	sp := NewSplice(guest, sw, SpliceConfig{Ref: "x"})
	t.Cleanup(func() { guest.close(); sw.close(); sp.Close() })
	f := append(append(append([]byte{}, spGuestMAC...), spSwitchMAC...), 0x88, 0xcc, 0)
	sw.toPort <- f
	deadline := time.Now().Add(2 * time.Second)
	for sp.Dropped.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sp.Dropped.Load() != 1 || sp.ToGuest.Load() != 0 {
		t.Errorf("dropped %d, delivered %d", sp.Dropped.Load(), sp.ToGuest.Load())
	}
	guest.toPort <- f
	recv(t, sw.toGuest, "a frame from the guest after a drop")
}

// Close stops the splice: what arrives after it is not carried.
func TestSpliceClose(t *testing.T) {
	guest, sw, sp, _, _ := startSplice(t)
	sp.Close()
	guest.toPort <- tcpFrame(netip.MustParseAddrPort("10.0.0.1:1"), netip.MustParseAddrPort("10.0.0.2:2"), 0x02)
	none(t, sw.toGuest, "a frame carried after Close")
}

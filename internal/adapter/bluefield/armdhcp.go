package bluefield

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// The Arm's out-of-band address. A BlueField has one RJ45 out-of-band port shared by its BMC and
// its Arm (oob_net0, the mlxbf_gige driver's port), each with its own MAC, so on a site two DHCP
// clients sit behind the one switch port. The twin relay serves one client per switch port: its
// upstream finds a reply's endpoint by the port's circuit id, then by the port's own MAC, so a
// second client plugged into the same port (netsim.PortRelay, the vm adapter's path, D48) would
// take over the BMC's replies, and the BMC would lose its lease at its next renewal. Until the
// twin relay serves both clients of a shared port (docs/implementation.md, Not built), the Arm's
// port answers the Arm's DHCP itself: the BMC's segment and gateway, from the BMC's own lease, and
// a fixed address in it that the twin site DHCP never hands out (it allocates from .10 up; the
// gateway is .1). Everything else of the port is netsim.HostPort's: the segment gateway routes the
// Arm's TCP to the stack's site services and the estate's endpoints, and to nothing else.
//
// This is not the site's DHCP server: it answers one MAC, on one cable, with one address, and the
// fidelity report says so. A network whose DHCP the stack serves (dhcp: stack) would have the
// stack answer the Arm; the Arm is refused there rather than given an address the stack never
// chose.

// armHostOffset is the Arm's host part in its segment: below the twin site DHCP's lowest (.10).
const armHostOffset = 9

// armRelay is the Arm's switch port as netsim.HostPort sees it (netsim.PortRelay).
type armRelay struct {
	mac     net.HardwareAddr
	addr    netip.Addr
	gw      netip.Prefix // the gateway's address and the segment's prefix length
	gwMAC   net.HardwareAddr
	lease   time.Duration
	replies chan []byte

	mu        sync.Mutex
	closed    bool
	requests  int
	acks      int
	lastState string
}

// newArmRelay derives the Arm's address from its BMC's lease: the BMC's segment (a /24 of the
// network, or the whole network when it is smaller), its gateway, and armHostOffset in it.
func newArmRelay(mac net.HardwareAddr, bmcIP, gateway string, network netip.Prefix, gwMAC net.HardwareAddr) (*armRelay, error) {
	ip, err := netip.ParseAddr(bmcIP)
	if err != nil || !ip.Is4() {
		return nil, fmt.Errorf("the BMC has no IPv4 lease to share a segment with (%q)", bmcIP)
	}
	bits := 24
	if network.IsValid() && network.Bits() > 24 {
		bits = network.Bits()
	}
	seg := netip.PrefixFrom(ip, bits).Masked()
	gw, err := netip.ParseAddr(gateway)
	if err != nil || !seg.Contains(gw) {
		gw = seg.Addr().Next()
	}
	a := seg.Addr()
	for i := 0; i < armHostOffset; i++ {
		a = a.Next()
	}
	if !seg.Contains(a) || a == gw || a == ip {
		return nil, fmt.Errorf("segment %s has no free host .%d for the Arm", seg, armHostOffset)
	}
	return &armRelay{mac: mac, addr: a, gw: netip.PrefixFrom(gw, bits), gwMAC: gwMAC, lease: 24 * time.Hour,
		replies: make(chan []byte, 16)}, nil
}

// Send answers a DHCP client message from the Arm's NIC. Messages from any other MAC (there is
// none on this cable) and anything but DISCOVER and REQUEST are ignored.
func (r *armRelay) Send(payload []byte) error {
	m, err := dhcpv4.FromBytes(payload)
	if err != nil || m.OpCode != dhcpv4.OpcodeBootRequest {
		return nil
	}
	if m.ClientHWAddr.String() != r.mac.String() {
		return nil
	}
	r.mu.Lock()
	r.requests++
	r.mu.Unlock()
	var typ dhcpv4.MessageType
	switch m.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		typ = dhcpv4.MessageTypeOffer
	case dhcpv4.MessageTypeRequest:
		want := m.RequestedIPAddress()
		if want == nil || want.IsUnspecified() {
			want = m.ClientIPAddr
		}
		if want != nil && !want.IsUnspecified() && !want.Equal(r.addr.AsSlice()) {
			typ = dhcpv4.MessageTypeNak
		} else {
			typ = dhcpv4.MessageTypeAck
		}
	default:
		return nil
	}
	mask := net.CIDRMask(r.gw.Bits(), 32)
	mods := []dhcpv4.Modifier{
		dhcpv4.WithMessageType(typ),
		dhcpv4.WithServerIP(r.gw.Addr().AsSlice()),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(r.gw.Addr().AsSlice())),
	}
	if typ != dhcpv4.MessageTypeNak {
		mods = append(mods,
			dhcpv4.WithYourIP(r.addr.AsSlice()),
			dhcpv4.WithNetmask(mask),
			dhcpv4.WithRouter(r.gw.Addr().AsSlice()),
			dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(r.lease)),
			dhcpv4.WithOption(dhcpv4.OptRenewTimeValue(r.lease/2)),
			dhcpv4.WithOption(dhcpv4.OptRebindingTimeValue(r.lease*7/8)))
	}
	reply, err := dhcpv4.NewReplyFromRequest(m, mods...)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errClosedRelay
	}
	if typ == dhcpv4.MessageTypeAck {
		r.acks++
	}
	r.lastState = typ.String()
	select {
	case r.replies <- reply.ToBytes():
	default:
	}
	return nil
}

var errClosedRelay = errors.New("the Arm's port is closed")

// Receive blocks for the next reply to deliver out of the port.
func (r *armRelay) Receive(b []byte) (int, error) {
	m, ok := <-r.replies
	if !ok {
		return 0, errClosedRelay
	}
	return copy(b, m), nil
}

// Gateway is the segment gateway and the MAC it answers with.
func (r *armRelay) Gateway() (netip.Prefix, net.HardwareAddr) { return r.gw, r.gwMAC }

// Server says what answers the port's DHCP, for reports.
func (r *armRelay) Server() string {
	return fmt.Sprintf("the bluefield adapter, for the Arm's MAC only (%s at %s): the twin relay serves one client per switch port", r.addr, r.gw)
}

func (r *armRelay) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.replies)
	}
	return nil
}

func (r *armRelay) stats() (requests, acks int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests, r.acks
}

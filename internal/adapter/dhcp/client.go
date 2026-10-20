package dhcp

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/insomniacslk/dhcp/dhcpv4"
)

// endpoint is one modelled port's DHCP client, cabled to one port of a switch. On the loopback
// substrate it is a userspace client on its own socket, and the model switch's relay knows its
// ingress port by that socket. On the TAP substrate (D20) a BMC's client is on its own NIC and
// speaks raw frames there, as a BMC's DHCP client does on a packet socket: it broadcasts on its
// segment, and whatever relays that segment (the twin relay on a model switch's bridge, or a
// real NOS's relay) hears it on the port it is cabled to.
type endpoint struct {
	a       *Adapter
	device  string // device path the lease is recorded on
	dev     *model.Device
	bmc     bool // a BMC endpoint (else NVOS's, or a host's provisioning port, driven by the walker)
	nvos    bool // a switch tray's NVOS management port: up while the tray is on (nvos.go)
	port    *model.Port
	mac     net.HardwareAddr
	seg     *segment
	sw      *model.Device
	swPort  *model.Port
	circuit string
	remote  net.HardwareAddr
	offset  int    // the twin site DHCP's static host part for this port (0: none)
	class   string // option 60, as sent

	// options every message of this client carries (vendor class, hostname, client id, PRL)
	opts []dhcpv4.Modifier

	conn *net.UDPConn
	addr netip.AddrPort

	// l2 is the endpoint's own NIC on the TAP substrate (nil on loopback); nos names the real
	// NOS whose relay serves its segment (its seg.relay is then nil).
	l2  netsim.L2Port
	nos string

	// acks counts this endpoint's ACKs; reboots those that answered an INIT-REBOOT
	acks, reboots atomic.Int64

	mu      sync.Mutex
	waiters map[dhcpv4.TransactionID]chan *dhcpv4.DHCPv4
	cancel  context.CancelFunc // the running client's; nil when none runs
	gen     int                // bumped by every start and halt, so a client that ends knows it is current
	lastIP  net.IP             // the address to ask for again after a BMC reboot (INIT-REBOOT)
	rng     *rand.Rand
}

// running reports whether the endpoint's client runs.
func (ep *endpoint) running() bool {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return ep.cancel != nil
}

// halt stops the endpoint's client at once, mid-exchange or mid-lease: an OS's DHCP client dies
// with its OS. The address it held is kept for the INIT-REBOOT of its next start.
func (ep *endpoint) halt() {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	if ep.cancel != nil {
		ep.cancel()
		ep.cancel = nil
	}
	ep.gen++
}

var (
	errDown = errors.New("the endpoint's controller or link is down")
	errNAK  = errors.New("the server answered NAK")
)

// open gives the endpoint its socket (or, on the TAP substrate, its NIC) and plugs it into the
// relay of its segment.
func (ep *endpoint) open(kind string) error {
	ep.waiters = map[dhcpv4.TransactionID]chan *dhcpv4.DHCPv4{}
	h := fnv.New64a()
	h.Write(ep.mac)
	ep.rng = rand.New(rand.NewSource(int64(h.Sum64())))
	if ep.bmc && ep.a.rt.Net.L2 != nil {
		if p := ep.a.rt.Net.L2.Port(ep.port.Ref); p != nil {
			ep.l2 = p
			if ep.seg.relay != nil {
				ep.seg.relay.attach(ep)
			}
			go ep.readL2()
			return nil
		}
	}
	if ep.seg.relay == nil {
		return fmt.Errorf("%s: no relay serves its segment", ep.port.Ref)
	}
	c, err := ep.a.rt.Net.ListenUDP(ep.device, kind)
	if err != nil {
		return err
	}
	ep.conn = c
	ep.addr = c.LocalAddr().(*net.UDPAddr).AddrPort()
	ep.seg.relay.attach(ep)
	go ep.read()
	return nil
}

func (ep *endpoint) close() {
	if ep.seg.relay != nil {
		ep.seg.relay.detach(ep)
	}
	if ep.conn != nil {
		ep.conn.Close()
	}
}

// twinServed reports whether the twin site DHCP answers this endpoint (else the stack's).
func (ep *endpoint) twinServed() bool {
	return ep.seg.relay != nil && ep.seg.relay.up.twin
}

// readL2 takes the DHCP replies off the endpoint's NIC: UDP from port 67 to port 68, for its
// own hardware address (a broadcast reply reaches every client on the segment).
func (ep *endpoint) readL2() {
	buf := make([]byte, 9216)
	for {
		n, err := ep.l2.ReadFrame(buf)
		if err != nil {
			return
		}
		f, ok := netsim.ParseUDPFrame(buf[:n])
		if !ok || f.DstPort != 68 || f.SrcPort != 67 {
			continue
		}
		m, err := dhcpv4.FromBytes(f.Payload)
		if err != nil || m.OpCode != dhcpv4.OpcodeBootReply || m.ClientHWAddr.String() != ep.mac.String() {
			continue
		}
		ep.deliver(m)
	}
}

func (ep *endpoint) deliver(m *dhcpv4.DHCPv4) {
	ep.mu.Lock()
	ch := ep.waiters[m.TransactionID]
	ep.mu.Unlock()
	if ch != nil {
		select {
		case ch <- m:
		default:
		}
	}
}

// send transmits a client message: to the relay's socket on loopback, or as a broadcast frame
// out of the endpoint's own NIC (from 0.0.0.0, or from ciaddr while it renews).
func (ep *endpoint) send(m *dhcpv4.DHCPv4) error {
	if ep.l2 == nil {
		_, err := ep.conn.WriteToUDPAddrPort(marshal(m), ep.seg.relay.addr())
		return err
	}
	src := netip.IPv4Unspecified()
	if a, ok := netip.AddrFromSlice(m.ClientIPAddr.To4()); ok && !a.IsUnspecified() {
		src = a
	}
	return ep.l2.WriteFrame(netsim.UDPFrame{SrcMAC: ep.mac, DstMAC: netsim.BroadcastMAC, SrcIP: src,
		DstIP: netip.AddrFrom4([4]byte{255, 255, 255, 255}), SrcPort: 68, DstPort: 67, Payload: marshal(m)}.Marshal())
}

// read hands each reply to the exchange waiting on its transaction id.
func (ep *endpoint) read() {
	buf := make([]byte, 4096)
	for {
		n, err := ep.conn.Read(buf)
		if err != nil {
			return
		}
		m, err := dhcpv4.FromBytes(buf[:n])
		if err != nil || m.OpCode != dhcpv4.OpcodeBootReply {
			continue
		}
		ep.deliver(m)
	}
}

// up reports whether the endpoint may transmit: its controller is up (BMC endpoints) and its
// port's link is up.
func (ep *endpoint) up() bool {
	return ep.a.endpointUp(ep)
}

// message builds a client message carrying the endpoint's options.
func (ep *endpoint) message(t dhcpv4.MessageType, xid dhcpv4.TransactionID, extra ...dhcpv4.Modifier) *dhcpv4.DHCPv4 {
	mods := []dhcpv4.Modifier{dhcpv4.WithTransactionID(xid), dhcpv4.WithHwAddr(ep.mac), dhcpv4.WithMessageType(t)}
	mods = append(mods, ep.opts...)
	mods = append(mods, extra...)
	m, _ := dhcpv4.New(mods...)
	return m
}

func (ep *endpoint) xid() dhcpv4.TransactionID {
	var x dhcpv4.TransactionID
	ep.mu.Lock()
	ep.rng.Read(x[:])
	ep.mu.Unlock()
	return x
}

func (ep *endpoint) jitter(d time.Duration) time.Duration {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return time.Duration(ep.rng.Int63n(int64(d/4)+1)) - d/8
}

// exchange sends a message and waits for a reply of a wanted type, retransmitting with RFC
// 2131's backoff (the base doubling to 64 s, with jitter of an eighth either way) up to tries
// times (0: until ctx ends). Before every transmission the endpoint must be up: a BMC that went
// down stops talking.
func (ep *endpoint) exchange(ctx context.Context, m *dhcpv4.DHCPv4, tries int, want ...dhcpv4.MessageType) (*dhcpv4.DHCPv4, error) {
	ch := make(chan *dhcpv4.DHCPv4, 8)
	ep.mu.Lock()
	ep.waiters[m.TransactionID] = ch
	ep.mu.Unlock()
	defer func() {
		ep.mu.Lock()
		delete(ep.waiters, m.TransactionID)
		ep.mu.Unlock()
	}()
	wait := ep.a.retransmit
	start := time.Now()
	for i := 0; tries == 0 || i < tries; i++ {
		if !ep.up() {
			return nil, errDown
		}
		m.NumSeconds = uint16(time.Since(start) / time.Second)
		if err := ep.send(m); err != nil {
			if ep.l2 != nil && !ep.up() {
				return nil, errDown // the substrate took the link down under it
			}
			return nil, err
		}
		ep.a.sent.Add(1)
		t := time.NewTimer(wait + ep.jitter(wait))
	recv:
		for {
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-t.C:
				break recv
			case r := <-ch:
				mt := r.MessageType()
				if mt == dhcpv4.MessageTypeNak {
					t.Stop()
					return r, errNAK
				}
				for _, w := range want {
					if mt == w {
						t.Stop()
						return r, nil
					}
				}
			}
		}
		if wait < 64*time.Second {
			wait *= 2
		}
	}
	return nil, fmt.Errorf("no answer after %d tries", tries)
}

// acquire runs DISCOVER, OFFER, REQUEST, ACK. It returns the offer too: a PXE client takes its
// boot parameters from whichever carries them.
func (ep *endpoint) acquire(ctx context.Context, tries int, extra ...dhcpv4.Modifier) (offer, ack *dhcpv4.DHCPv4, err error) {
	for {
		xid := ep.xid()
		offer, err = ep.exchange(ctx, ep.message(dhcpv4.MessageTypeDiscover, xid, extra...), tries, dhcpv4.MessageTypeOffer)
		if err != nil {
			return nil, nil, err
		}
		req := ep.message(dhcpv4.MessageTypeRequest, xid, append(extra,
			dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(offer.YourIPAddr)),
			dhcpv4.WithOptionCopied(offer, dhcpv4.OptionServerIdentifier))...)
		ack, err = ep.exchange(ctx, req, 4, dhcpv4.MessageTypeAck)
		if err == nil {
			return offer, ack, nil
		}
		if ctx.Err() != nil || errors.Is(err, errDown) {
			return nil, nil, err
		}
		// a NAK or a lost REQUEST: back to INIT (RFC 2131 §4.4.1)
		if tries != 0 {
			return offer, nil, err
		}
	}
}

// extend is RENEWING and then REBINDING: a REQUEST with ciaddr, retransmitted until the lease
// ends. Both go through the relay. On a site a RENEWING client unicasts to the server and the
// relay never sees it; the loopback substrate has no route from a leased site address to the
// server, so the twin's renewals are relayed, which a server accepts the same way. On the TAP
// substrate they are broadcast from ciaddr, as a REBINDING client's are, and relayed the same.
func (ep *endpoint) extend(ctx context.Context, ack *dhcpv4.DHCPv4, until time.Time) (*dhcpv4.DHCPv4, error) {
	ctx, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	req := ep.message(dhcpv4.MessageTypeRequest, ep.xid(), dhcpv4.WithClientIP(ack.YourIPAddr))
	return ep.exchange(ctx, req, 0, dhcpv4.MessageTypeAck)
}

// reboot is INIT-REBOOT: a rebooted client asks for the address it had (option 50, no server
// id); no answer within two tries falls back to DISCOVER.
func (ep *endpoint) reboot(ctx context.Context, ip net.IP) (*dhcpv4.DHCPv4, error) {
	req := ep.message(dhcpv4.MessageTypeRequest, ep.xid(), dhcpv4.WithOption(dhcpv4.OptRequestedIPAddress(ip)))
	return ep.exchange(ctx, req, 2, dhcpv4.MessageTypeAck)
}

// leaseTimes reads T1, T2 and the lease time from an ACK, with RFC 2131's defaults.
func leaseTimes(ack *dhcpv4.DHCPv4) (t1, t2, lease time.Duration) {
	lease = ack.IPAddressLeaseTime(time.Hour)
	t1 = ack.IPAddressRenewalTime(lease / 2)
	t2 = ack.IPAddressRebindingTime(lease * 7 / 8)
	return
}

// run is a BMC's DHCP client from INIT to BOUND and on through its renewals. When its
// controller goes down it falls silent and waits for the engine's OnBMCUp, which restarts it;
// when only its link is down it waits for carrier, as a NIC's link-up restarts a client, and
// then asks for its previous address again (INIT-REBOOT). NVOS's client runs the same way, with
// NVOS as its controller: its tray leaving On halts it, reaching On restarts it (nvos.go).
func (ep *endpoint) run(ctx context.Context, stagger time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(stagger):
	}
	for ctx.Err() == nil {
		ack, err := ep.obtain(ctx)
		for err == nil && ack != nil {
			ep.bound(ack)
			ack, err = ep.keep(ctx, ack)
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errDown) && !ep.waitCarrier(ctx) {
			return // the controller is down: OnBMCUp restarts the client
		}
	}
}

// obtain is INIT-REBOOT when the client had an address, else (or when that goes unanswered)
// INIT: DISCOVER until an offer is accepted.
func (ep *endpoint) obtain(ctx context.Context) (*dhcpv4.DHCPv4, error) {
	ep.mu.Lock()
	prev := ep.lastIP
	ep.mu.Unlock()
	if prev != nil {
		ack, err := ep.reboot(ctx, prev)
		if err == nil {
			ep.reboots.Add(1)
			return ack, nil
		}
		if errors.Is(err, errDown) || ctx.Err() != nil {
			return nil, err
		}
		ep.mu.Lock()
		ep.lastIP = nil
		ep.mu.Unlock()
	}
	_, ack, err := ep.acquire(ctx, 0)
	return ack, err
}

// keep waits for T1 and extends the lease: RENEWING until T2, REBINDING until the lease ends.
// It returns the next ACK, or nil when the lease is lost and the client must start over.
func (ep *endpoint) keep(ctx context.Context, ack *dhcpv4.DHCPv4) (*dhcpv4.DHCPv4, error) {
	t1, t2, lease := leaseTimes(ack)
	at := time.Now()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(t1):
	}
	ep.a.renewals.Add(1)
	next, err := ep.extend(ctx, ack, at.Add(t2))
	if err != nil && ctx.Err() == nil && !errors.Is(err, errNAK) && !errors.Is(err, errDown) {
		next, err = ep.extend(ctx, ack, at.Add(lease)) // REBINDING
	}
	if errors.Is(err, errDown) || ctx.Err() != nil {
		return nil, err
	}
	if err != nil { // NAK or expiry: back to INIT
		ep.mu.Lock()
		ep.lastIP = nil
		ep.mu.Unlock()
		return nil, nil
	}
	return next, nil
}

// waitCarrier waits while the controller is up but the port's link is down; false means the
// controller itself is down (or the adapter stopped). NVOS is an NVOS endpoint's controller:
// the tray's power-on restarts it (watchNVOS), as OnBMCUp restarts a BMC's.
func (ep *endpoint) waitCarrier(ctx context.Context) bool {
	for {
		if ep.a.stopped.Load() || ctx.Err() != nil {
			return false
		}
		if ep.bmc {
			if up, _, _ := ep.a.rt.Engine.BMCBehaviour(ep.dev); !up {
				return false
			}
		}
		if ep.nvos && !ep.a.nvosUp(ep.dev) {
			return false
		}
		if ep.a.rt.Engine.PortUp(ep.port) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
}

// bound records an ACK as the device's lease (the engine commits it; the Redfish adapter routes
// the address through the site gateway); an NVOS endpoint's as a state event on its port, which
// the NVUE adapter routes (nvosBound).
func (ep *endpoint) bound(ack *dhcpv4.DHCPv4) {
	ep.mu.Lock()
	ep.lastIP = ack.YourIPAddr
	ep.mu.Unlock()
	t1, _, _ := leaseTimes(ack)
	server := ""
	if sid := ack.ServerIdentifier(); sid != nil {
		server = sid.String()
	}
	gw := ""
	if r := ack.Router(); len(r) > 0 {
		gw = r[0].String()
	}
	src := "twin"
	if !ep.twinServed() {
		src = "stack"
	}
	if ep.l2 != nil {
		// the endpoint takes the address on its own NIC: ARP and TCP answer there from now on
		bits := 24
		if mask := ack.SubnetMask(); mask != nil {
			bits, _ = mask.Size()
		}
		var gwAddr netip.Addr
		if r := ack.Router(); len(r) > 0 {
			gwAddr, _ = netip.AddrFromSlice(r[0].To4())
		}
		if a, ok := netip.AddrFromSlice(ack.YourIPAddr.To4()); ok {
			if err := ep.l2.SetAddress(netip.PrefixFrom(a, bits), gwAddr); err != nil {
				ep.a.rt.Engine.Incident("dhcp", fmt.Sprintf("%s: the substrate refused the leased address %s: %v", ep.port.Ref, a, err))
			}
		}
	}
	ep.a.acks.Add(1)
	ep.acks.Add(1)
	if ep.nvos {
		ep.a.nvosBound(ep, ack.YourIPAddr.String(), server, gw, src)
		return
	}
	ep.a.rt.Engine.SetLease(ep.device, &model.Lease{IP: ack.YourIPAddr.String(), Server: server, Circuit: ep.circuit,
		Remote: ep.remote.String(), At: ep.a.rt.Engine.Now(), Renew: t1, Gateway: gw, VendorClass: ep.class},
		sourceOf(src, server))
	ep.a.leased(ep)
}

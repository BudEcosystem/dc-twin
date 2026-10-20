package netsim

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A routed site network (D48). A stack service that identifies a host by the address its packets
// come from (NICo's PXE service does, and refuses any proxy's word for it) cannot sit behind the
// gateway's TCP proxy, which on macOS can only connect from this host's loopback address. Such
// services run in a Linux network namespace (a container) whose interface is the far end of an
// Uplink: the twin's router holds the network's gateway address there and forwards IPv4 between
// the vm hosts' ports and the namespace without NAT, as a site's routers do: the namespace sees
// the guest's lease as the source, and its answers come back to the guest through the gateway.
//
// The link is a TCP connection carrying Ethernet frames in QEMU's stream framing (a 32-bit
// big-endian length before each frame); a helper in the namespace connects it to a TAP. The twin
// dials it and redials while it is down.

// SiteNetwork is a routed site network as a stack's `siteNetwork` output names it.
type SiteNetwork struct {
	Prefix  netip.Prefix
	Gateway netip.Addr // the twin's address on it
	Link    string     // host:port of the namespace's end of the link
}

// ParseSiteNetwork reads `prefix=<cidr> gateway=<ip> link=<host:port>` (fields in any order,
// separated by white space or commas).
func ParseSiteNetwork(s string) (SiteNetwork, error) {
	var n SiteNetwork
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return n, fmt.Errorf("siteNetwork field %q: want key=value", f)
		}
		var err error
		switch k {
		case "prefix":
			n.Prefix, err = netip.ParsePrefix(v)
		case "gateway":
			n.Gateway, err = netip.ParseAddr(v)
		case "link":
			_, _, err = net.SplitHostPort(v)
			n.Link = v
		default:
			err = fmt.Errorf("unknown field")
		}
		if err != nil {
			return n, fmt.Errorf("siteNetwork field %q: %v", f, err)
		}
	}
	if !n.Prefix.IsValid() || !n.Prefix.Addr().Is4() || !n.Gateway.IsValid() || !n.Prefix.Contains(n.Gateway) || n.Link == "" {
		return n, fmt.Errorf("siteNetwork %q: want prefix=<IPv4 cidr> gateway=<an address in it> link=<host:port>", s)
	}
	return n, nil
}

// Uplink is the router's interface on a routed site network.
type Uplink struct {
	Net SiteNetwork
	MAC net.HardwareAddr
	log *slog.Logger

	mu     sync.Mutex
	conn   net.Conn
	arp    map[netip.Addr]net.HardwareAddr
	hosts  map[netip.Addr]*HostPort // a guest's lease -> its port
	closed chan struct{}
	once   sync.Once
	wmu    sync.Mutex

	ToSite, FromSite, Unresolved, NoHost, Connects atomic.Int64
}

// NewUplink starts the router's interface on a site network and dials its link.
func NewUplink(n SiteNetwork, mac net.HardwareAddr, log *slog.Logger) *Uplink {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	u := &Uplink{Net: n, MAC: mac, log: log, arp: map[netip.Addr]net.HardwareAddr{}, hosts: map[netip.Addr]*HostPort{}, closed: make(chan struct{})}
	go u.dial()
	return u
}

// Linked reports whether the namespace's end is connected.
func (u *Uplink) Linked() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.conn != nil
}

func (u *Uplink) dial() {
	for {
		select {
		case <-u.closed:
			return
		default:
		}
		c, err := net.DialTimeout("tcp", u.Net.Link, 3*time.Second)
		if err != nil {
			select {
			case <-u.closed:
				return
			case <-time.After(time.Second):
			}
			continue
		}
		u.mu.Lock()
		u.conn = c
		u.mu.Unlock()
		u.Connects.Add(1)
		u.log.Info("site network linked", "prefix", u.Net.Prefix, "link", u.Net.Link)
		u.read(c)
		u.mu.Lock()
		if u.conn == c {
			u.conn = nil
		}
		u.mu.Unlock()
		c.Close()
	}
}

func (u *Uplink) read(c net.Conn) {
	var h [4]byte
	for {
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(h[:])
		if n < ethHeader || n > 65535 {
			return
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		u.frame(b)
	}
}

func (u *Uplink) write(frame []byte) {
	u.mu.Lock()
	c := u.conn
	u.mu.Unlock()
	if c == nil {
		return
	}
	b := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(frame)), uint32(len(frame)))
	b = append(b, frame...)
	u.wmu.Lock()
	_, _ = c.Write(b)
	u.wmu.Unlock()
}

// frame takes one frame from the namespace: ARP for the gateway, ARP replies, and IPv4 for a
// guest, routed to its port.
func (u *Uplink) frame(b []byte) {
	switch binary.BigEndian.Uint16(b[12:14]) {
	case 0x0806:
		u.arpIn(b[ethHeader:])
	case 0x0800:
		ip := b[ethHeader:]
		if len(ip) < ipv4Header {
			return
		}
		src := netip.AddrFrom4([4]byte(ip[12:16]))
		if u.Net.Prefix.Contains(src) {
			u.learn(src, net.HardwareAddr(b[6:12]))
		}
		dst := netip.AddrFrom4([4]byte(ip[16:20]))
		u.mu.Lock()
		hp := u.hosts[dst]
		u.mu.Unlock()
		if hp == nil {
			u.NoHost.Add(1)
			return
		}
		pkt := append([]byte(nil), ip...)
		if !decTTL(pkt) {
			return
		}
		u.FromSite.Add(1)
		hp.deliverIPv4(pkt)
	}
}

func (u *Uplink) learn(ip netip.Addr, mac net.HardwareAddr) {
	u.mu.Lock()
	u.arp[ip] = append(net.HardwareAddr(nil), mac...)
	u.mu.Unlock()
}

// arpIn answers who-has for the gateway and learns from every ARP message.
func (u *Uplink) arpIn(a []byte) {
	if len(a) < 28 || binary.BigEndian.Uint16(a[0:2]) != 1 || binary.BigEndian.Uint16(a[2:4]) != 0x0800 {
		return
	}
	op := binary.BigEndian.Uint16(a[6:8])
	sha, spa, tpa := net.HardwareAddr(a[8:14]), netip.AddrFrom4([4]byte(a[14:18])), netip.AddrFrom4([4]byte(a[24:28]))
	if u.Net.Prefix.Contains(spa) && !spa.IsUnspecified() {
		u.learn(spa, sha)
	}
	if op == 1 && tpa == u.Net.Gateway {
		u.write(arpFrame(2, u.MAC, u.Net.Gateway, sha, spa))
	}
}

func arpFrame(op uint16, sha net.HardwareAddr, spa netip.Addr, tha net.HardwareAddr, tpa netip.Addr) []byte {
	b := make([]byte, ethHeader+28)
	dst := tha
	if op == 1 {
		dst, tha = BroadcastMAC, net.HardwareAddr{0, 0, 0, 0, 0, 0}
	}
	copy(b[0:6], dst)
	copy(b[6:12], sha)
	binary.BigEndian.PutUint16(b[12:14], 0x0806)
	a := b[ethHeader:]
	binary.BigEndian.PutUint16(a[0:2], 1)
	binary.BigEndian.PutUint16(a[2:4], 0x0800)
	a[4], a[5] = 6, 4
	binary.BigEndian.PutUint16(a[6:8], op)
	s, t := spa.As4(), tpa.As4()
	copy(a[8:14], sha)
	copy(a[14:18], s[:])
	copy(a[18:24], tha)
	copy(a[24:28], t[:])
	return b
}

// attach routes a guest's lease to its port.
func (u *Uplink) attach(ip netip.Addr, hp *HostPort) {
	u.mu.Lock()
	for a, h := range u.hosts {
		if h == hp {
			delete(u.hosts, a) // a new lease replaces the port's old one
		}
	}
	u.hosts[ip] = hp
	u.mu.Unlock()
}

func (u *Uplink) detach(hp *HostPort) {
	u.mu.Lock()
	for a, h := range u.hosts {
		if h == hp {
			delete(u.hosts, a)
		}
	}
	u.mu.Unlock()
}

// Routes reports whether the router forwards a destination to this site network.
func (u *Uplink) Routes(dst netip.Addr) bool {
	return u != nil && u.Net.Prefix.Contains(dst) && dst != u.Net.Gateway
}

// forward sends a guest's IPv4 packet into the site network: the next hop is the destination
// itself (the network is on-link), resolved by ARP; a packet for an unresolved neighbour is
// dropped while the request goes out, and the sender retransmits, as a router's first packet to
// a neighbour often is.
func (u *Uplink) forward(ip []byte) {
	dst := netip.AddrFrom4([4]byte(ip[16:20]))
	pkt := append([]byte(nil), ip...)
	if !decTTL(pkt) {
		return
	}
	u.mu.Lock()
	mac := u.arp[dst]
	u.mu.Unlock()
	if mac == nil {
		u.Unresolved.Add(1)
		u.write(arpFrame(1, u.MAC, u.Net.Gateway, nil, dst))
		return
	}
	f := make([]byte, ethHeader+len(pkt))
	copy(f[0:6], mac)
	copy(f[6:12], u.MAC)
	binary.BigEndian.PutUint16(f[12:14], 0x0800)
	copy(f[ethHeader:], pkt)
	u.ToSite.Add(1)
	u.write(f)
}

// decTTL decrements an IPv4 packet's TTL and updates its header checksum (RFC 1624); a packet
// whose TTL runs out is dropped.
func decTTL(ip []byte) bool {
	if len(ip) < ipv4Header || ip[8] <= 1 {
		return false
	}
	ip[8]--
	ihl := int(ip[0]&0x0f) * 4
	if ihl < ipv4Header || ihl > len(ip) {
		return false
	}
	binary.BigEndian.PutUint16(ip[10:12], 0)
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip[:ihl], 0))
	return true
}

// Close stops the interface.
func (u *Uplink) Close() error {
	u.once.Do(func() {
		close(u.closed)
		u.mu.Lock()
		if u.conn != nil {
			u.conn.Close()
		}
		u.mu.Unlock()
	})
	return nil
}

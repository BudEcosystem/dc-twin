package netsim

import (
	"errors"
	"net"
	"net/netip"
	"sync"
)

// L2 is a substrate that gives modelled ports a link layer: the TAP substrate (D20), Linux
// only. Every BMC's management port is its own NIC with the model's MAC, on its own veth, served
// by a userspace network stack inside the twin process; the veth's far end is a port of its
// segment's bridge (a model switch) or the matching port of a real NOS. The loopback substrate
// has none (Net.L2 is nil), and everything below is then unused.
type L2 interface {
	// Name is the substrate's name in reports ("tap").
	Name() string
	// Port is the endpoint behind a modelled port (its ref, "device:port"), or nil when the
	// substrate gave that port none.
	Port(ref string) L2Port
	// DevicePort is the endpoint of a device's management (BMC) port, or nil.
	DevicePort(device string) L2Port
	// Gateway puts a model switch segment's twin relay on the segment: a NIC on the segment's
	// bridge holding the segment gateway, with the switch's MAC. It is nil (and no error) when
	// the substrate bridged no endpoint for that network and rack.
	Gateway(network, rack string, gw netip.Prefix, mac net.HardwareAddr) (L2Gateway, error)
	// NOS names the real NOS serving the switch at the far end of a port ref ("" for a model
	// switch).
	NOS(ref string) string
	// Report is the substrate's part of the fidelity report.
	Report() L2Report
	Close() error
}

// L2Report is what the substrate says about itself in the fidelity report (07).
type L2Report struct {
	Lines []string
	IsNot []string
}

// FrameConn reads and writes raw Ethernet frames on one NIC, as a DHCP client or relay does with
// a packet socket.
type FrameConn interface {
	MAC() net.HardwareAddr
	WriteFrame(b []byte) error
	// ReadFrame blocks for the next frame the NIC received (to its MAC or a group address). It
	// returns ErrClosed once the NIC is gone.
	ReadFrame(b []byte) (int, error)
}

// L2Port is one modelled port's endpoint.
type L2Port interface {
	FrameConn
	Ref() string
	// SetAddress gives the endpoint its address (a lease), with the segment's gateway; the
	// userspace stack then answers ARP and TCP on it. A zero prefix removes it.
	SetAddress(p netip.Prefix, gw netip.Addr) error
	Address() netip.Prefix
	// ListenTCP listens on the endpoint at a TCP port, on whatever address it holds.
	ListenTCP(port uint16) (net.Listener, error)
}

// L2Gateway is a model switch segment's twin relay presence.
type L2Gateway interface {
	FrameConn
	// Ingress is the modelled port the segment's bridge learned a source MAC on (its forwarding
	// database), as a switch knows the port a frame arrived on.
	Ingress(src net.HardwareAddr) (ref string, ok bool)
}

// ErrClosed is returned by a FrameConn whose NIC is gone.
var ErrClosed = errors.New("netsim: endpoint closed")

// l2Ports are the endpoint kinds served on a device's own L2 address as well as its loopback
// port, and the TCP port each answers on there.
var l2Ports = map[string]uint16{"redfish": 443}

// multiListener accepts from several listeners: a device endpoint's loopback port (the site
// gateway's route to it) and its own address on the L2 substrate. Addr is the first one's.
type multiListener struct {
	ls     []net.Listener
	conns  chan net.Conn
	done   chan struct{}
	once   sync.Once
	errMu  sync.Mutex
	errors int
}

func newMultiListener(ls ...net.Listener) *multiListener {
	m := &multiListener{ls: ls, conns: make(chan net.Conn), done: make(chan struct{})}
	for _, l := range ls {
		go m.loop(l)
	}
	return m
}

func (m *multiListener) loop(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			select {
			case <-m.done:
				return
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			m.errMu.Lock()
			m.errors++
			m.errMu.Unlock()
			return
		}
		select {
		case m.conns <- c:
		case <-m.done:
			c.Close()
			return
		}
	}
}

func (m *multiListener) Accept() (net.Conn, error) {
	select {
	case c := <-m.conns:
		return c, nil
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *multiListener) Close() error {
	m.once.Do(func() {
		close(m.done)
		for _, l := range m.ls {
			l.Close()
		}
	})
	return nil
}

func (m *multiListener) Addr() net.Addr { return m.ls[0].Addr() }

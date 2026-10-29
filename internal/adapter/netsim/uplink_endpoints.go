package netsim

import (
	"context"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// The routed site network's way to estate endpoints. A service in the namespace reaches a
// machine at its lease through the router (forward, frame), and an estate endpoint at its site
// address the same way: the namespace routes the estate's networks to the twin's gateway, as a
// site routes them, and the router terminates such a TCP connection (a userspace stack in
// promiscuous mode that answers for any address, as a guest's segment gateway does) and opens a
// connection to where the twin serves that address on this host, by the substrate's site routes.
// So NICo's nvlink-manager, which runs in the namespace, reaches a switch tray's NMX-C at the
// tray's NVOS lease on 9370. Only TCP is routed; the endpoint sees this host's loopback address
// as its peer, which no estate endpoint checks.

// SetSiteRoutes gives the router the substrate's site routes (estate endpoints at their site
// addresses, "ip:port" -> where the twin serves them), so the namespace reaches them.
func (u *Uplink) SetSiteRoutes(f func() map[string]string) {
	if u == nil || f == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.ep != nil {
		return
	}
	u.siteRoutes = f
	u.ep = newEndpointRouter(u)
}

type endpointRouter struct {
	u      *Uplink
	st     *stack.Stack
	link   *channel.Endpoint
	cancel context.CancelFunc
	once   sync.Once
}

const endpointNIC tcpip.NICID = 1

func newEndpointRouter(u *Uplink) *endpointRouter {
	r := &endpointRouter{u: u}
	r.st = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	r.link = channel.New(512, 1500, "")
	_ = r.st.CreateNIC(endpointNIC, r.link)
	r.st.SetPromiscuousMode(endpointNIC, true)
	r.st.SetSpoofing(endpointNIC, true)
	r.st.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: endpointNIC}})
	fwd := tcp.NewForwarder(r.st, 0, 256, r.forwardTCP)
	r.st.SetTransportProtocolHandler(tcp.ProtocolNumber, fwd.HandlePacket)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go r.out(ctx)
	return r
}

// routes says whether an estate endpoint is served at dst (on any port).
func (r *endpointRouter) routes(dst netip.Addr) bool {
	prefix := dst.String() + ":"
	for k := range r.u.siteRoutes() {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

func (r *endpointRouter) inject(ip []byte) {
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), ip...))})
	r.link.InjectInbound(ipv4.ProtocolNumber, pkt)
	pkt.DecRef()
}

// out sends the stack's answers back into the site network.
func (r *endpointRouter) out(ctx context.Context) {
	for {
		pkt := r.link.ReadContext(ctx)
		if pkt == nil {
			return
		}
		var b []byte
		for _, s := range pkt.AsSlices() {
			b = append(b, s...)
		}
		pkt.DecRef()
		r.u.forward(b)
	}
}

func (r *endpointRouter) forwardTCP(fr *tcp.ForwarderRequest) {
	id := fr.ID()
	dst := addrPort(id.LocalAddress, id.LocalPort)
	local, ok := r.u.siteRoutes()[dst.String()]
	if !ok {
		r.u.EndpointRefused.Add(1)
		fr.Complete(true)
		return
	}
	up, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		r.u.EndpointRefused.Add(1)
		fr.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, terr := fr.CreateEndpoint(&wq)
	if terr != nil {
		up.Close()
		fr.Complete(true)
		return
	}
	fr.Complete(false)
	r.u.EndpointFlows.Add(1)
	g := gonet.NewTCPConn(&wq, ep)
	go func() {
		defer up.Close()
		defer g.Close()
		done := make(chan struct{}, 2)
		go func() { io.Copy(up, g); done <- struct{}{} }()
		go func() { io.Copy(g, up); done <- struct{}{} }()
		<-done
	}()
}

func (r *endpointRouter) close() {
	r.once.Do(func() {
		r.cancel()
		r.st.Close()
	})
}

//go:build linux

package tap

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/link/tun"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/raw"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

func init() { twin.RegisterNetworkSubstrate("tap", New) }

// Substrate is the running TAP substrate.
type Substrate struct {
	plan   *Plan
	eng    *engine.Engine
	log    *slog.Logger
	nsName string
	ns     netns.NsHandle
	nlMu   sync.Mutex
	h      *netlink.Handle
	st     *stack.Stack

	mu       sync.Mutex
	nextNIC  tcpip.NICID
	ports    map[string]*port // by port ref
	byDev    map[string]*port
	bridges  map[string]*bridge // "network|rack path"
	gws      []*gateway
	switches []*nosSwitch
	byPath   map[string]*nosSwitch
	cables   []*cable
	docker   *nos.Docker
	volumes  []string // the switches' shared volumes, removed after their containers
	attached []string

	obs    *observer
	ctx    context.Context
	cancel context.CancelFunc
	closed atomic.Bool
	wg     sync.WaitGroup
}

type bridge struct {
	plan  *BridgePlan
	index int
	ports map[int]string // bridge port ifindex -> endpoint port ref
}

// nic is one NIC of the userspace stack on a TAP's file descriptor.
type nic struct {
	s       *Substrate
	id      tcpip.NICID
	ifname  string
	mac     net.HardwareAddr
	fd      int
	carrier int32          // 1 on, 0 off (the cable)
	pe      tcpip.Endpoint // the packet endpoint frames are read and written on
	wq      waiter.Queue
	notify  chan struct{}
	done    chan struct{}
	extra   []tcpip.Endpoint

	mu   sync.Mutex
	addr netip.Prefix
	gw   netip.Addr
}

// port is a BMC management port's endpoint.
type port struct {
	*nic
	plan *EndpointPlan
}

type gateway struct {
	*nic
	br *bridge

	cacheMu sync.Mutex
	cache   map[string]string
	cacheAt time.Time
}

type nosSwitch struct {
	plan *SwitchPlan
	c    *nos.Container
	ns   netns.NsHandle
	h    *netlink.Handle
	// volumes are the switch's shared directories (nos.Entry.Shared), in order; relay is the
	// container its DHCP relay runs in, beside it (nos.Entry.RelayImage).
	volumes []string
	relay   *nos.Container
}

// cable is a fabric link between two real switches: the twin holds its middle (a two-port
// bridge between two veths), so pulling it drops carrier at both switches.
type cable struct {
	plan     *LinkPlan
	a, b     string // twin-side veths
	br       string
	appliedA int32
	appliedB int32
}

// New builds the substrate from the running estate (twin.RegisterNetworkSubstrate).
func New(ctx context.Context, a twin.NetworkArgs) (netsim.L2, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("the tap substrate needs root (CAP_NET_ADMIN and CAP_SYS_ADMIN) to create network namespaces, TAPs and bridges")
	}
	var plan *Plan
	var err error
	a.Engine.Read(func(m *model.Model) { plan, err = BuildPlan(m, a.Estate, a.Outputs, compile.HostArch()) })
	if err != nil {
		return nil, err
	}
	log := a.Log
	if log == nil {
		log = slog.Default()
	}
	s := &Substrate{plan: plan, eng: a.Engine, log: log, ns: netns.None(), nextNIC: 1, ports: map[string]*port{}, byDev: map[string]*port{},
		bridges: map[string]*bridge{}, byPath: map[string]*nosSwitch{}}
	s.ctx, s.cancel = context.WithCancel(ctx)
	if err := s.start(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Substrate) start() error {
	s.nsName = unsafeName.ReplaceAllString("dctwin-"+s.plan.Estate, "-")
	if err := s.makeNetns(); err != nil {
		return err
	}
	h, err := netlink.NewHandleAt(s.ns)
	if err != nil {
		return fmt.Errorf("netlink in %s: %w", s.nsName, err)
	}
	s.h = h
	if lo, err := h.LinkByName("lo"); err == nil {
		_ = h.LinkSetUp(lo)
	}
	s.st = stack.New(stack.Options{
		NetworkProtocols:         []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols:       []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
		RawFactory:               raw.EndpointFactory{},
		AllowPacketEndpointWrite: true,
	})
	if err := s.startSwitches(); err != nil {
		return err
	}
	for _, b := range s.plan.Bridges {
		if err := s.addBridge(b.Name); err != nil {
			return fmt.Errorf("bridge %s (%s on %s): %w", b.Name, b.Rack, b.Network, err)
		}
		l, err := s.h.LinkByName(b.Name)
		if err != nil {
			return err
		}
		s.bridges[b.Network+"|"+b.Rack] = &bridge{plan: b, index: l.Attrs().Index, ports: map[int]string{}}
	}
	for _, ep := range s.plan.Endpoints {
		if err := s.addEndpoint(ep); err != nil {
			return fmt.Errorf("endpoint %s: %w", ep.Ref, err)
		}
	}
	for i, lp := range s.plan.Links {
		if err := s.addCable(i, lp); err != nil {
			return fmt.Errorf("fabric link %s: %w", lp.Link.ID, err)
		}
	}
	for _, at := range s.plan.Attach {
		if err := s.attach(at); err != nil {
			return fmt.Errorf("attach %s%s to %s:%s: %w", at.Container, at.Netns, at.Switch.Config.Hostname, at.Port, err)
		}
	}
	s.syncLinks()
	s.wg.Add(1)
	go s.linkLoop()
	if len(s.switches) > 0 {
		s.obs = newObserver(s)
		if err := s.waitSwitches(s.startTimeout()); err != nil {
			return err
		}
		s.wg.Add(1)
		go s.obs.loop(s.ctx, &s.wg)
	}
	s.log.Info("tap substrate up", "netns", s.nsName, "endpoints", len(s.ports), "bridges", len(s.bridges),
		"nos", len(s.switches), "cables", len(s.cables), "attached", len(s.attached))
	return nil
}

// inNS runs f on a thread inside the twin's namespace (sockets and /proc/sys writes made there
// stay there).
func inNS(ns netns.NsHandle, f func() error) error {
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		return err
	}
	defer orig.Close()
	if err := netns.Set(ns); err != nil {
		runtime.UnlockOSThread()
		return err
	}
	ferr := f()
	if err := netns.Set(orig); err != nil {
		return fmt.Errorf("could not return to the twin's own namespace: %w", err) // the thread stays locked and dies
	}
	runtime.UnlockOSThread()
	return ferr
}

func (s *Substrate) makeNetns() error {
	_ = netns.DeleteNamed(s.nsName) // a leftover of a crashed run
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := netns.Get()
		if err != nil {
			runtime.UnlockOSThread()
			errc <- err
			return
		}
		defer orig.Close()
		ns, err := netns.NewNamed(s.nsName)
		if err != nil {
			_ = netns.Set(orig)
			runtime.UnlockOSThread()
			errc <- fmt.Errorf("create network namespace %s: %w", s.nsName, err)
			return
		}
		s.ns = ns
		// no IPv6 on any twin interface: the kernel's side of an endpoint's TAP must never speak
		// on the segment (no router solicitations, no MLD)
		for _, k := range []string{"all", "default"} {
			_ = os.WriteFile("/proc/sys/net/ipv6/conf/"+k+"/disable_ipv6", []byte("1"), 0o644)
		}
		if err := netns.Set(orig); err != nil {
			errc <- err // the thread stays locked and is discarded
			return
		}
		runtime.UnlockOSThread()
		errc <- nil
	}()
	return <-errc
}

func (s *Substrate) nl(f func(h *netlink.Handle) error) error {
	s.nlMu.Lock()
	defer s.nlMu.Unlock()
	return f(s.h)
}

func (s *Substrate) addBridge(name string) error {
	return s.nl(func(h *netlink.Handle) error {
		off := false
		br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name, MTU: 1500}, MulticastSnooping: &off}
		if err := h.LinkAdd(br); err != nil {
			return err
		}
		return h.LinkSetUp(br)
	})
}

// addEndpoint creates a BMC port's TAP and its NIC: the TAP's device is the switch port (a port
// of the segment's bridge, or moved into the NOS's namespace under the model port's name, with
// the switch port's MAC); the NIC behind its file descriptor carries the BMC's MAC.
func (s *Substrate) addEndpoint(ep *EndpointPlan) error {
	var devMAC net.HardwareAddr
	if ep.Peer != nil && ep.Peer.MAC != "" {
		devMAC, _ = net.ParseMAC(ep.Peer.MAC)
	}
	n, err := s.addNIC(ep.TapIf, ep.MAC, devMAC)
	if err != nil {
		return err
	}
	if err := s.nl(func(h *netlink.Handle) error {
		l, err := h.LinkByName(ep.TapIf)
		if err != nil {
			return err
		}
		if ep.Switch != nil {
			sw := s.byPath[ep.Switch.Device.Path]
			if err := h.LinkSetNsFd(l, int(sw.ns)); err != nil {
				return fmt.Errorf("move into %s: %w", sw.c.Name, err)
			}
			moved, err := sw.h.LinkByName(ep.TapIf)
			if err != nil {
				return err
			}
			return sw.h.LinkSetName(moved, ep.SwitchIf) // the NOS brings it up and into its VLAN
		}
		br := s.bridges[ep.Bridge.Network+"|"+ep.Bridge.Rack]
		if err := h.LinkSetMasterByIndex(l, br.index); err != nil {
			return err
		}
		br.ports[l.Attrs().Index] = ep.Ref
		return h.LinkSetUp(l)
	}); err != nil {
		n.close()
		return err
	}
	// a bound client keeps port 68 open, so a unicast reply never draws a port unreachable
	if e, err := n.udpSink(68); err == nil {
		n.extra = append(n.extra, e)
	}
	p := &port{nic: n, plan: ep}
	s.mu.Lock()
	s.ports[ep.Ref] = p
	s.byDev[ep.Device] = p
	s.mu.Unlock()
	return nil
}

// addNIC creates a TAP in the twin's namespace (its device gets devMAC when given) and makes its
// file descriptor a NIC of the userspace stack with the endpoint's MAC, with a packet endpoint
// for raw frames.
func (s *Substrate) addNIC(ifname string, mac, devMAC net.HardwareAddr) (*nic, error) {
	fd := -1
	if err := inNS(s.ns, func() error {
		var err error
		fd, err = tun.OpenTAP(ifname)
		return err
	}); err != nil {
		return nil, fmt.Errorf("TAP %s: %w", ifname, err)
	}
	if err := macFilter(fd, mac); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("TAP %s: %w", ifname, err)
	}
	if len(devMAC) == 6 {
		if err := s.nl(func(h *netlink.Handle) error {
			l, err := h.LinkByName(ifname)
			if err != nil {
				return err
			}
			return h.LinkSetHardwareAddr(l, devMAC)
		}); err != nil {
			unix.Close(fd)
			return nil, err
		}
	}
	link, err := fdbased.New(&fdbased.Options{FDs: []int{fd}, MTU: 1500, EthernetHeader: true, Address: tcpip.LinkAddress(mac),
		RXChecksumOffload: true, ProcessorsPerChannel: 1,
		ClosedFunc: func(terr tcpip.Error) {
			if !s.closed.Load() {
				s.log.Error("tap: a NIC stopped reading its TAP", "tap", ifname, "err", terr)
				s.eng.Incident("tap", fmt.Sprintf("the NIC on TAP %s stopped reading it: %v", ifname, terr))
			}
		}})
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	s.mu.Lock()
	id := s.nextNIC
	s.nextNIC++
	s.mu.Unlock()
	if terr := s.st.CreateNICWithOptions(id, link, stack.NICOptions{Name: ifname, DeliverLinkPackets: true}); terr != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("NIC %s: %s", ifname, terr)
	}
	n := &nic{s: s, id: id, ifname: ifname, mac: mac, fd: fd, done: make(chan struct{}), carrier: 1}
	pe, terr := s.st.NewPacketEndpoint(false, 0, &n.wq)
	if terr != nil {
		n.close()
		return nil, fmt.Errorf("packet endpoint on %s: %s", ifname, terr)
	}
	pe.SocketOptions().SetReceiveBufferSize(1<<20, true)
	if terr := pe.Bind(tcpip.FullAddress{NIC: id, Port: uint16(header.IPv4ProtocolNumber)}); terr != nil {
		pe.Close()
		n.close()
		return nil, fmt.Errorf("bind the packet endpoint on %s: %s", ifname, terr)
	}
	entry, ch := waiter.NewChannelEntry(waiter.ReadableEvents)
	n.wq.EventRegister(&entry)
	n.pe, n.notify = pe, ch
	return n, nil
}

// macFilter gives the TAP the receive filter a NIC has in hardware: frames to its own MAC and to
// any group address (broadcast, multicast) reach the stack; unicast for another MAC (a bridge
// flooding an unknown destination) is dropped in the kernel (TUNSETTXFILTER, TUN_FLT_ALLMULTI).
// Besides being what a NIC does, it is load-bearing: gVisor's readv dispatcher, which a TAP's
// descriptor gets, stops reading for good on the first frame not addressed to it.
func macFilter(fd int, mac net.HardwareAddr) error {
	if len(mac) != 6 {
		return fmt.Errorf("MAC %s is not 48 bits", mac)
	}
	var f [10]byte                           // struct tun_filter: u16 flags, u16 count, then count addresses
	binary.NativeEndian.PutUint16(f[0:2], 1) // TUN_FLT_ALLMULTI
	binary.NativeEndian.PutUint16(f[2:4], 1)
	copy(f[4:], mac)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TUNSETTXFILTER, uintptr(unsafe.Pointer(&f[0]))); errno != 0 {
		return fmt.Errorf("MAC filter: %w", errno)
	}
	return nil
}

// setCarrier is the cable: carrier off on the TAP is what the switch port sees when the cable is
// pulled (its admin state untouched); frames the stack writes meanwhile reach a port that is down.
func (n *nic) setCarrier(up bool) error {
	v := int32(0)
	if up {
		v = 1
	}
	if atomic.LoadInt32(&n.carrier) == v {
		return nil
	}
	if err := unix.IoctlSetPointerInt(n.fd, unix.TUNSETCARRIER, int(v)); err != nil {
		return fmt.Errorf("carrier on %s: %w", n.ifname, err)
	}
	atomic.StoreInt32(&n.carrier, v)
	return nil
}

func (n *nic) udpSink(port uint16) (tcpip.Endpoint, error) {
	var wq waiter.Queue
	e, terr := n.s.st.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if terr != nil {
		return nil, errors.New(terr.String())
	}
	e.SocketOptions().SetReceiveBufferSize(4096, false)
	if terr := e.SocketOptions().SetBindToDevice(int32(n.id)); terr != nil {
		e.Close()
		return nil, errors.New(terr.String())
	}
	if terr := e.Bind(tcpip.FullAddress{NIC: n.id, Port: port}); terr != nil {
		e.Close()
		return nil, errors.New(terr.String())
	}
	return e, nil
}

// ---------------------------------------------------------------- netsim.FrameConn

func (n *nic) MAC() net.HardwareAddr { return n.mac }

func (n *nic) WriteFrame(b []byte) error {
	if _, terr := n.pe.Write(bytes.NewReader(b), tcpip.WriteOptions{}); terr != nil {
		return fmt.Errorf("%s: %s", n.ifname, terr)
	}
	return nil
}

func (n *nic) ReadFrame(b []byte) (int, error) {
	for {
		w := tcpip.SliceWriter(b)
		res, terr := n.pe.Read(&w, tcpip.ReadOptions{})
		if terr == nil {
			return res.Count, nil
		}
		if _, ok := terr.(*tcpip.ErrWouldBlock); !ok {
			return 0, netsim.ErrClosed
		}
		select {
		case <-n.notify:
		case <-n.done:
			return 0, netsim.ErrClosed
		}
	}
}

func (n *nic) close() {
	select {
	case <-n.done:
		return
	default:
	}
	close(n.done)
	if n.pe != nil {
		n.pe.Close()
	}
	for _, e := range n.extra {
		e.Close()
	}
	_ = n.s.st.RemoveNIC(n.id)
	if n.fd >= 0 {
		unix.Close(n.fd)
	}
}

// ---------------------------------------------------------------- netsim.L2Port

func (p *port) Ref() string { return p.plan.Ref }

func (p *port) Address() netip.Prefix {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addr
}

func (n *nic) SetAddress(pfx netip.Prefix, gw netip.Addr) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if pfx == n.addr && gw == n.gw {
		return nil
	}
	st := n.s.st
	if n.addr.IsValid() {
		_ = st.RemoveAddress(n.id, tcpip.AddrFrom4(n.addr.Addr().As4()))
		st.RemoveRoutes(func(r tcpip.Route) bool { return r.NIC == n.id })
	}
	n.addr, n.gw = netip.Prefix{}, netip.Addr{}
	if !pfx.IsValid() {
		return nil
	}
	if !pfx.Addr().Is4() {
		return fmt.Errorf("%s is not IPv4", pfx)
	}
	awp := tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(pfx.Addr().As4()), PrefixLen: pfx.Bits()}
	if terr := st.AddProtocolAddress(n.id, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: awp}, stack.AddressProperties{}); terr != nil {
		return errors.New(terr.String())
	}
	st.AddRoute(tcpip.Route{Destination: awp.Subnet(), NIC: n.id})
	if gw.IsValid() && gw.Is4() && gw != pfx.Addr() {
		st.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(gw.As4()), NIC: n.id})
	}
	n.addr, n.gw = pfx, gw
	return nil
}

// ListenTCP listens on the endpoint's NIC (SO_BINDTODEVICE), on whatever address it holds.
func (n *nic) ListenTCP(port uint16) (net.Listener, error) {
	var wq waiter.Queue
	e, terr := n.s.st.NewEndpoint(tcp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if terr != nil {
		return nil, errors.New(terr.String())
	}
	e.SocketOptions().SetReuseAddress(true)
	if terr := e.SocketOptions().SetBindToDevice(int32(n.id)); terr != nil {
		e.Close()
		return nil, errors.New(terr.String())
	}
	if terr := e.Bind(tcpip.FullAddress{NIC: n.id, Port: port}); terr != nil {
		e.Close()
		return nil, fmt.Errorf("bind :%d on %s: %s", port, n.ifname, terr)
	}
	if terr := e.Listen(1024); terr != nil {
		e.Close()
		return nil, errors.New(terr.String())
	}
	return gonet.NewTCPListener(n.s.st, &wq, e), nil
}

// ---------------------------------------------------------------- netsim.L2

func (s *Substrate) Name() string { return "tap" }

func (s *Substrate) Port(ref string) netsim.L2Port {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.ports[ref]; p != nil {
		return p
	}
	return nil
}

func (s *Substrate) DevicePort(device string) netsim.L2Port {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.byDev[device]; p != nil {
		return p
	}
	return nil
}

func (s *Substrate) NOS(ref string) string {
	dev, _, _ := strings.Cut(ref, ":")
	s.mu.Lock()
	defer s.mu.Unlock()
	if sw := s.byPath[dev]; sw != nil {
		return sw.plan.NOS.Name
	}
	return ""
}

// Gateway puts the twin relay on a model switch segment's bridge, at the segment's gateway.
func (s *Substrate) Gateway(network, rack string, gw netip.Prefix, mac net.HardwareAddr) (netsim.L2Gateway, error) {
	s.mu.Lock()
	br := s.bridges[network+"|"+rack]
	s.mu.Unlock()
	if br == nil {
		return nil, nil
	}
	ifname := fmt.Sprintf("q%04x", br.plan.Index)
	n, err := s.addNIC(ifname, mac, nil)
	if err != nil {
		return nil, err
	}
	if err := s.nl(func(h *netlink.Handle) error {
		l, err := h.LinkByName(ifname)
		if err != nil {
			return err
		}
		if err := h.LinkSetMasterByIndex(l, br.index); err != nil {
			return err
		}
		return h.LinkSetUp(l)
	}); err != nil {
		n.close()
		return nil, err
	}
	if err := n.SetAddress(gw, netip.Addr{}); err != nil {
		return nil, err
	}
	// the relay listens on 67 the way a switch's DHCP relay does, so a client's broadcast does
	// not draw a port unreachable from the gateway
	if e, err := n.udpSink(67); err == nil {
		n.extra = append(n.extra, e)
	}
	g := &gateway{nic: n, br: br, cache: map[string]string{}}
	s.mu.Lock()
	s.gws = append(s.gws, g)
	s.mu.Unlock()
	return g, nil
}

// Ingress looks a source MAC up in the segment bridge's forwarding database: the bridge port it
// was learned on is the modelled switch port the frame came in on.
func (g *gateway) Ingress(src net.HardwareAddr) (string, bool) {
	key := src.String()
	g.cacheMu.Lock()
	defer g.cacheMu.Unlock()
	if ref, ok := g.cache[key]; ok && time.Since(g.cacheAt) < 250*time.Millisecond {
		return ref, true
	}
	var neighs []netlink.Neigh
	if err := g.s.nl(func(h *netlink.Handle) error {
		var err error
		neighs, err = h.NeighList(0, unix.AF_BRIDGE)
		return err
	}); err != nil {
		return "", false
	}
	g.cache, g.cacheAt = map[string]string{}, time.Now()
	for _, n := range neighs {
		if n.MasterIndex != g.br.index {
			continue
		}
		if ref, ok := g.br.ports[n.LinkIndex]; ok {
			g.cache[n.HardwareAddr.String()] = ref
		}
	}
	ref, ok := g.cache[key]
	return ref, ok
}

// ---------------------------------------------------------------- real switches

func (s *Substrate) startSwitches() error {
	if len(s.plan.Switches) == 0 {
		return nil
	}
	d, err := nos.NewDocker()
	if err != nil {
		return fmt.Errorf("a real NOS needs a NOS runtime: %w", err)
	}
	s.docker = d
	// a loaded Docker host takes minutes over a container create: this bounds the images' checks
	// and the switches' creation, not their start-up (waitSwitches)
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Minute)
	defer cancel()
	canRun := map[string]error{}
	for _, sp := range s.plan.Switches {
		e := sp.NOS
		images := []string{sp.Image}
		if e.RelayImage != "" && sp.Config.Relaying() {
			images = append(images, e.RelayImage)
		}
		for _, img := range images {
			if !d.HasImage(ctx, img) {
				return fmt.Errorf("switch %s: the image %s for nos %s is not present on the Docker host: %s", sp.Device.Path, img, e.Name, e.Obtain)
			}
		}
		if sp.Emulated {
			err, done := canRun[sp.Image]
			if !done {
				err = d.CanRun(ctx, unsafeName.ReplaceAllString("dctwin-"+s.plan.Estate+"-platform-check", "-"), sp.Image, e.Platform)
				canRun[sp.Image] = err
			}
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("switch %s: the Docker Engine did not run nos %s's %s image in time to show it can: %w", sp.Device.Path, e.Name, e.Platform, err)
			case err != nil:
				return fmt.Errorf("switch %s: nos %s's image is %s, which this %s Docker host cannot run (it runs another architecture's images only through binfmt_misc: Rosetta or QEMU user mode): %w", sp.Device.Path, e.Name, e.Platform, compile.HostArch(), err)
			}
		}
		script, err := e.StartScript(sp.Config)
		if err != nil {
			return err
		}
		labels := map[string]string{"dc-twin.estate": s.plan.Estate, "dc-twin.switch": sp.Device.Path, "dc-twin.nos": e.Name}
		var vols, binds []string
		for _, m := range e.Shared {
			v := unsafeName.ReplaceAllString(sp.Container+"-"+strings.Trim(m.Path, "/"), "-")
			if err := d.CreateVolume(ctx, v, labels); err != nil {
				return fmt.Errorf("switch %s: volume %s: %w", sp.Device.Path, v, err)
			}
			s.volumes = append(s.volumes, v)
			vols, binds = append(vols, v), append(binds, v+":"+m.Path)
		}
		c, err := d.Run(ctx, nos.Spec{Name: sp.Container, Image: sp.Image, Hostname: sp.Config.Hostname,
			Entrypoint: nos.Entrypoint, Cmd: []string{script}, Labels: labels, Sysctls: e.Sysctls,
			Platform: e.Platform, Privileged: e.Privileged, Binds: binds})
		if err != nil {
			return err
		}
		sw := &nosSwitch{plan: sp, c: c, ns: netns.None(), volumes: vols}
		s.switches = append(s.switches, sw) // removed on Close even if wiring fails
		s.byPath[sp.Device.Path] = sw
		ns, err := netns.GetFromPath(fmt.Sprintf("/proc/%d/ns/net", c.Pid))
		if err != nil {
			return fmt.Errorf("switch %s: cannot open the network namespace of container %s (pid %d): the twin must see the Docker host's processes (run it on the host, or in a container with --pid=host): %w", sp.Device.Path, c.Name, c.Pid, err)
		}
		sw.ns = ns
		if sw.h, err = netlink.NewHandleAt(ns); err != nil {
			return err
		}
	}
	return nil
}

// startTimeout is how long `up` waits for the slowest NOS to start.
func (s *Substrate) startTimeout() time.Duration {
	t := 90 * time.Second
	for _, sw := range s.switches {
		if d := sw.plan.NOS.StartTimeout; d > t {
			t = d
		}
	}
	return t
}

// startRelay starts a switch's DHCP relay container beside it, as SONiC runs dhcp_relay beside
// swss: in the switch's network namespace (so with its hostname, which the relay's circuit id
// carries), with its shared directories (CONFIG_DB's socket and database configuration, /etc/sonic
// read-only), tmpfs where SONiC gives it tmpfs, and the capabilities a relay needs. It renders
// its own configuration from CONFIG_DB, so it starts once the switch has loaded it.
func (s *Substrate) startRelay(sw *nosSwitch) error {
	e := sw.plan.NOS
	var binds []string
	for i, m := range e.Shared {
		b := sw.volumes[i] + ":" + m.Path
		if m.RelayReadOnly {
			b += ":ro"
		}
		binds = append(binds, b)
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Minute) // a loaded Docker host is slow to create
	defer cancel()
	c, err := s.docker.Run(ctx, nos.Spec{Name: sw.c.Name + "-dhcp-relay", Image: e.RelayImage, Platform: e.Platform,
		NetworkMode: "container:" + sw.c.ID, Env: []string{"RUNTIME_OWNER=local"}, Binds: binds,
		Tmpfs: map[string]string{"/tmp": "", "/var/tmp": ""}, CapAdd: []string{"NET_ADMIN", "NET_RAW"},
		Labels: map[string]string{"dc-twin.estate": s.plan.Estate, "dc-twin.switch": sw.plan.Device.Path, "dc-twin.nos": e.Name, "dc-twin.role": "dhcp-relay"}})
	if err != nil {
		return fmt.Errorf("switch %s: its DHCP relay container: %w", sw.plan.Device.Path, err)
	}
	sw.relay = c
	return nil
}

func (s *Substrate) addCable(i int, lp *LinkPlan) error {
	a, b := s.byPath[lp.A.Device.Path], s.byPath[lp.B.Device.Path]
	c := &cable{plan: lp, a: fmt.Sprintf("la%04x", i), b: fmt.Sprintf("lb%04x", i), br: fmt.Sprintf("l%04x", i)}
	if err := s.addBridge(c.br); err != nil { // up from the start; its two ends carry the state
		return err
	}
	if err := s.nl(func(h *netlink.Handle) error {
		brl, err := h.LinkByName(c.br)
		if err != nil {
			return err
		}
		for _, end := range []struct {
			twin, port string
			ns         netns.NsHandle
		}{{c.a, lp.APort, a.ns}, {c.b, lp.BPort, b.ns}} {
			if err := h.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: end.twin, MTU: 1500}, PeerName: end.port, PeerNamespace: netlink.NsFd(int(end.ns))}); err != nil {
				return err
			}
			l, err := h.LinkByName(end.twin)
			if err != nil {
				return err
			}
			if err := h.LinkSetMasterByIndex(l, brl.Attrs().Index); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if lp.A.NOS.SoftwareChecksum || lp.B.NOS.SoftwareChecksum {
		// a SONiC-VS port takes its peer's frames as they are; its peer must checksum them
		for _, end := range []struct {
			ns   netns.NsHandle
			port string
		}{{a.ns, lp.APort}, {b.ns, lp.BPort}} {
			if err := txChecksumOff(end.ns, end.port); err != nil {
				return err
			}
		}
	}
	s.cables = append(s.cables, c)
	return nil
}

// txChecksumOff turns transmit checksum offload off on an interface in a network namespace
// (`ethtool -K <if> tx off`; TSO goes with it), so its kernel checksums every frame it sends.
// SONiC-VS needs it of every host cabled to one of its ports (nos.Entry.SoftwareChecksum): its
// SAI copies frames from a port to the port's host interface with a packet socket, which hands a
// frame whose checksum was left to offload over as it is, and the switch's kernel then drops it
// (Kea's replies reached the relay with `bad udp cksum` and went no further).
func txChecksumOff(ns netns.NsHandle, ifname string) error {
	return inNS(ns, func() error {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		const ethtoolSTXCSUM = 0x17 // ETHTOOL_STXCSUM, <linux/ethtool.h>
		val := struct{ cmd, data uint32 }{cmd: ethtoolSTXCSUM}
		var ifr struct { // struct ifreq with ifr_data
			name [unix.IFNAMSIZ]byte
			data unsafe.Pointer
			_    [24 - unsafe.Sizeof(uintptr(0))]byte
		}
		copy(ifr.name[:], ifname)
		ifr.data = unsafe.Pointer(&val)
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCETHTOOL, uintptr(unsafe.Pointer(&ifr))); errno != 0 {
			return fmt.Errorf("transmit checksum offload off on %s: %w", ifname, errno)
		}
		return nil
	})
}

// attach cables a host outside the twin to a real switch's port: a veth from the switch's
// namespace into the host's, its address and a default route through the switch.
func (s *Substrate) attach(at *AttachPlan) error {
	sw := s.byPath[at.Switch.Device.Path]
	var target netns.NsHandle
	var err error
	if at.Container != "" {
		if s.docker == nil {
			return errors.New("no NOS runtime")
		}
		pid, err := s.docker.Pid(s.ctx, at.Container)
		if err != nil {
			return err
		}
		target, err = netns.GetFromPath(fmt.Sprintf("/proc/%d/ns/net", pid))
		if err != nil {
			return fmt.Errorf("the network namespace of %s: %w", at.Container, err)
		}
	} else if target, err = netns.GetFromName(at.Netns); err != nil {
		return fmt.Errorf("network namespace %s: %w", at.Netns, err)
	}
	defer target.Close()
	if err := s.nl(func(h *netlink.Handle) error {
		return h.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: at.SwitchIf, MTU: 1500, Namespace: netlink.NsFd(int(sw.ns))},
			PeerName: at.Interface, PeerNamespace: netlink.NsFd(int(target))})
	}); err != nil {
		return err
	}
	if at.Switch.NOS.SoftwareChecksum {
		if err := txChecksumOff(target, at.Interface); err != nil {
			return err
		}
	}
	th, err := netlink.NewHandleAt(target)
	if err != nil {
		return err
	}
	defer th.Close()
	l, err := th.LinkByName(at.Interface)
	if err != nil {
		return err
	}
	if lo, err := th.LinkByName("lo"); err == nil {
		_ = th.LinkSetUp(lo)
	}
	if err := th.AddrAdd(l, &netlink.Addr{IPNet: &net.IPNet{IP: at.Address.Addr().AsSlice(), Mask: net.CIDRMask(at.Address.Bits(), 32)}}); err != nil {
		return err
	}
	if err := th.LinkSetUp(l); err != nil {
		return err
	}
	if err := th.RouteReplace(&netlink.Route{LinkIndex: l.Attrs().Index, Gw: at.Gateway.AsSlice()}); err != nil {
		return fmt.Errorf("default route via %s: %w", at.Gateway, err)
	}
	name := at.Container
	if name == "" {
		name = "netns " + at.Netns
	}
	s.attached = append(s.attached, fmt.Sprintf("%s (%s, %s) at %s:%s", name, at.Interface, at.Address, at.Switch.Config.Hostname, at.Port))
	return nil
}

// waitSwitches waits until every switch has run its start-up script, its relay container (where
// its NOS has one) is relaying, and every BGP session it configured is established: `up` returns
// on a converged fabric, as a site's day starts.
func (s *Substrate) waitSwitches(timeout time.Duration) error {
	start := time.Now()
	deadline := start.Add(timeout)
	for _, sw := range s.switches {
		for {
			logs, _ := s.docker.Logs(s.ctx, sw.c.ID)
			if nos.Started(logs) {
				break
			}
			if _, err := s.docker.Pid(s.ctx, sw.c.ID); err != nil {
				return fmt.Errorf("switch %s (%s) stopped during start-up: %v\n%s", sw.plan.Device.Path, sw.c.Name, err, logs)
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("switch %s (%s) did not finish its start-up in %s:\n%s", sw.plan.Device.Path, sw.c.Name, timeout, logs)
			}
			time.Sleep(200 * time.Millisecond)
		}
		s.log.Info("tap: switch started", "switch", sw.plan.Device.Path, "nos", sw.plan.NOS.Name, "emulated", sw.plan.Emulated, "after", time.Since(start).Round(time.Second))
		if sw.plan.NOS.RelayImage != "" && sw.plan.Config.Relaying() {
			if err := s.startRelay(sw); err != nil {
				return err
			}
		}
	}
	for _, sw := range s.switches {
		if sw.relay == nil {
			continue
		}
		for {
			out, _, _ := s.docker.Exec(s.ctx, sw.relay.ID, nos.RelayStatusCommand...)
			pending := nos.RelaysPending(sw.plan.Config, out)
			if len(pending) == 0 {
				break
			}
			if _, err := s.docker.Pid(s.ctx, sw.relay.ID); err != nil {
				logs, _ := s.docker.Logs(s.ctx, sw.relay.ID)
				return fmt.Errorf("switch %s: its relay container %s stopped: %v\n%s", sw.plan.Device.Path, sw.relay.Name, err, logs)
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("switch %s: its relay container %s did not start %s in %s:\n%s", sw.plan.Device.Path, sw.relay.Name, strings.Join(pending, ", "), timeout, out)
			}
			time.Sleep(time.Second)
		}
	}
	for {
		s.obs.poll(s.ctx)
		down := append(s.obs.sessionsDown(), s.unrouted()...)
		if len(down) == 0 {
			s.log.Info("tap: fabric converged", "after", time.Since(start).Round(time.Second))
			return nil
		}
		if time.Now().After(deadline) {
			s.eng.Incident("tap", fmt.Sprintf("the fabric had not converged %s after up: %s", timeout, strings.Join(down, ", ")))
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// unrouted lists the relay targets a switch has no route to yet: a session can be established
// before its routes are in the kernel, and a relay forwarding then gets "network unreachable".
func (s *Substrate) unrouted() []string {
	var out []string
	for _, sw := range s.switches {
		for _, v := range sw.plan.Config.Vlans {
			for _, srv := range v.Relay {
				if _, err := sw.h.RouteGet(srv.AsSlice()); err != nil {
					out = append(out, fmt.Sprintf("%s has no route to %s", sw.plan.Config.Hostname, srv))
				}
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- link state from the model

// linkLoop keeps every link's state with the model: an endpoint's TAP has carrier while its
// port is up (cabled, not forced down, powered) and, for a BMC, its controller is up; a fabric
// cable is up while both its ports are.
func (s *Substrate) linkLoop() {
	defer s.wg.Done()
	id, ch := s.eng.Subscribe(4096)
	defer func() { s.eng.Unsubscribe(id) }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case _, ok := <-ch:
			if !ok {
				id, ch = s.eng.Subscribe(4096)
			}
			// coalesce a burst of events
			drain := time.After(20 * time.Millisecond)
		coalesce:
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						id, ch = s.eng.Subscribe(4096)
					}
				case <-drain:
					break coalesce
				}
			}
			s.syncLinks()
		case <-tick.C:
			s.syncLinks()
		}
	}
}

func (s *Substrate) syncLinks() {
	if s.closed.Load() {
		return
	}
	s.mu.Lock()
	ports := make([]*port, 0, len(s.ports))
	for _, p := range s.ports {
		ports = append(ports, p)
	}
	s.mu.Unlock()
	for _, p := range ports {
		want := s.eng.PortUp(p.plan.Port)
		if want && p.plan.Dev.HasBMC() {
			want, _, _ = s.eng.BMCBehaviour(p.plan.Dev)
		}
		if err := p.setCarrier(want); err != nil {
			s.log.Warn("tap: link state", "port", p.plan.Ref, "err", err)
		}
	}
	for _, c := range s.cables {
		want := s.eng.PortUp(c.plan.Link.A) && s.eng.PortUp(c.plan.Link.B)
		s.setLink(c.a, want, &c.appliedA)
		s.setLink(c.b, want, &c.appliedB)
	}
}

func (s *Substrate) setLink(name string, up bool, applied *int32) {
	state := int32(2)
	if up {
		state = 1
	}
	if atomic.LoadInt32(applied) == state {
		return
	}
	err := s.nl(func(h *netlink.Handle) error {
		l, err := h.LinkByName(name)
		if err != nil {
			return err
		}
		if up {
			return h.LinkSetUp(l)
		}
		return h.LinkSetDown(l)
	})
	if err == nil {
		atomic.StoreInt32(applied, state)
	}
}

// ---------------------------------------------------------------- inspection (tests, the CLI)

// Plan is the wiring the substrate built.
func (s *Substrate) Plan() *Plan { return s.plan }

// Netns is the twin's network namespace (under /var/run/netns).
func (s *Substrate) Netns() string { return s.nsName }

func (s *Substrate) realSwitch(path string) *nosSwitch {
	if sw := s.byPath[path]; sw != nil {
		return sw
	}
	for p, x := range s.byPath {
		if strings.HasSuffix(p, "/"+path) {
			return x
		}
	}
	return nil
}

// Exec runs a command in a real switch's container.
func (s *Substrate) Exec(ctx context.Context, switchPath string, cmd ...string) (string, error) {
	sw := s.realSwitch(switchPath)
	if sw == nil {
		return "", fmt.Errorf("no real switch %s", switchPath)
	}
	out, code, err := s.docker.Exec(ctx, sw.c.ID, cmd...)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	return out, err
}

// NICStats is every NIC's received and sent frame counts, by interface name.
func (s *Substrate) NICStats() map[string][2]uint64 {
	out := map[string][2]uint64{}
	for _, info := range s.st.NICInfo() {
		out[info.Name] = [2]uint64{info.Stats.Rx.Packets.Value(), info.Stats.Tx.Packets.Value()}
	}
	return out
}

// ExecDetached starts a command in a real switch's container and returns at once.
func (s *Substrate) ExecDetached(ctx context.Context, switchPath string, cmd ...string) error {
	sw := s.realSwitch(switchPath)
	if sw == nil {
		return fmt.Errorf("no real switch %s", switchPath)
	}
	return s.docker.ExecDetached(ctx, sw.c.ID, cmd...)
}

// RelayExec runs a command in a real switch's relay container.
func (s *Substrate) RelayExec(ctx context.Context, switchPath string, cmd ...string) (string, error) {
	sw := s.realSwitch(switchPath)
	if sw == nil || sw.relay == nil {
		return "", fmt.Errorf("no relay container for %s", switchPath)
	}
	out, code, err := s.docker.Exec(ctx, sw.relay.ID, cmd...)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	return out, err
}

// Relays lists the relay containers: switch device path -> container name.
func (s *Substrate) Relays() map[string]string {
	out := map[string]string{}
	for _, sw := range s.switches {
		if sw.relay != nil {
			out[sw.plan.Device.Path] = sw.relay.Name
		}
	}
	return out
}

// Switches lists the real switches: device path -> container name.
func (s *Substrate) Switches() map[string]string {
	out := map[string]string{}
	for _, sw := range s.switches {
		out[sw.plan.Device.Path] = sw.c.Name
	}
	return out
}

// Observed is what the NOS observer last read: "<port ref>" -> operational state, and
// "<switch path> bgp <peer>" -> session state.
func (s *Substrate) Observed() map[string]string {
	if s.obs == nil {
		return nil
	}
	return s.obs.snapshot()
}

// Report is the substrate's part of the fidelity report.
func (s *Substrate) Report() netsim.L2Report {
	var r netsim.L2Report
	nosNames := map[string]int{}
	for _, sw := range s.switches {
		nosNames[sw.plan.NOS.Name]++
	}
	var real []string
	for n, c := range nosNames {
		real = append(real, fmt.Sprintf("%d %s", c, n))
	}
	sort.Strings(real)
	r.Lines = append(r.Lines, fmt.Sprintf("Substrate tap (D20): %d BMC ports are their own NICs (the model's MAC, on their own TAP) in a userspace stack (gVisor netstack); TAPs, bridges and cables live in network namespace %s; %d model-switch segments are Linux bridges with the twin relay at their gateway; real switches: %s; %d fabric cables; %d hosts attached.",
		len(s.ports), s.nsName, len(s.bridges), orNone(strings.Join(real, ", ")), len(s.cables), len(s.attached)))
	emulated, relays := map[string]int{}, 0
	for _, sw := range s.switches {
		if sw.plan.Emulated {
			emulated[sw.plan.NOS.Name]++
		}
		if sw.relay != nil {
			relays++
		}
	}
	var names []string
	for n := range emulated {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		e, _ := nos.Lookup(n)
		r.Lines = append(r.Lines, fmt.Sprintf("tap: %d %s switches run emulated %s: the image is built for %s only and runs on this %s host under the Docker host's binary translation (binfmt_misc), unmodified and slower.",
			emulated[n], n, strings.TrimPrefix(e.Platform, "linux/"), strings.Join(e.Arch, ", "), compile.HostArch()))
	}
	if relays > 0 {
		r.Lines = append(r.Lines, fmt.Sprintf("tap: %d switches relay DHCP from a relay container of their NOS's own beside them (SONiC's dhcp_relay), in their network namespace.", relays))
	}
	for why, n := range s.plan.Skipped {
		r.Lines = append(r.Lines, fmt.Sprintf("tap: %d BMC ports have no endpoint: %s.", n, why))
	}
	if s.obs != nil {
		r.Lines = append(r.Lines, s.obs.summary()...)
	}
	r.IsNot = append(r.IsNot,
		"Only BMC management ports have a link layer: IPMI, SNMP and the other UDP endpoints, the boot walker's host ports and every service the site gateway routes to still answer on loopback ports.",
		"Endpoints send no LLDP, gratuitous ARP or IPv6; a renewing BMC broadcasts its REQUEST (REBINDING-style) instead of unicasting to the server.",
		"Packets flow; nothing is measured. Each NIC's TAP filters unicast to its own MAC in the kernel (as NIC hardware does), so a port never sees another BMC's flooded unicast.",
	)
	seen := map[string]bool{}
	for _, sw := range s.switches {
		if !seen[sw.plan.NOS.Name] {
			seen[sw.plan.NOS.Name] = true
			r.IsNot = append(r.IsNot, sw.plan.NOS.IsNot...)
		}
	}
	if len(s.switches) > 0 {
		r.IsNot = append(r.IsNot, "Real switches run as containers the twin wires itself (TAPs and veths moved into their namespaces), not through containerlab; a rack's management switch keeps its profile's fidelity in the model although its NOS serves it (D21 derives only generated switches' fidelity).")
	}
	return r
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Close removes everything the substrate made: NICs and their TAPs (closing a TAP's descriptor
// deletes it, in whichever namespace it is), the twin's namespace (its bridges and cables with
// it) and then the switch containers.
func (s *Substrate) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	s.mu.Lock()
	for _, p := range s.ports {
		p.close()
	}
	for _, g := range s.gws {
		g.close()
	}
	s.mu.Unlock()
	if s.st != nil {
		s.st.Close()
		s.st.Wait()
	}
	if s.docker != nil {
		// a loaded Docker host takes its time to kill an emulated SONiC: remove the switches side
		// by side, each relay before its switch, and the volumes after them all
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		var wg sync.WaitGroup
		for _, sw := range s.switches {
			if sw.h != nil {
				sw.h.Close()
			}
			if sw.ns.IsOpen() {
				sw.ns.Close()
			}
			wg.Add(1)
			go func(sw *nosSwitch) {
				defer wg.Done()
				if sw.relay != nil {
					if err := s.docker.Remove(ctx, sw.relay.ID); err != nil {
						s.log.Warn("removing a relay container", "container", sw.relay.Name, "err", err)
					}
				}
				if err := s.docker.Remove(ctx, sw.c.ID); err != nil {
					s.log.Warn("removing a switch container", "container", sw.c.Name, "err", err)
				}
			}(sw)
		}
		wg.Wait()
		for _, v := range s.volumes {
			if err := s.docker.RemoveVolume(ctx, v); err != nil {
				s.log.Warn("removing a switch volume", "volume", v, "err", err)
			}
		}
		cancel()
	}
	if s.h != nil {
		s.h.Close()
	}
	if s.ns.IsOpen() {
		s.ns.Close()
	}
	if s.nsName != "" {
		_ = netns.DeleteNamed(s.nsName)
	}
	return nil
}

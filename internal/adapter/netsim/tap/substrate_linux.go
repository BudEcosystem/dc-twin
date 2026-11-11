//go:build linux

package tap

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

func init() { twin.RegisterNetworkSubstrate("tap", New) }

// Substrate is the running TAP substrate.
type Substrate struct {
	plan *Plan
	eng  *engine.Engine
	// agents are NICo's DPU agents running in emulated BlueFields (D71), by DPU path
	agents map[string]*hbnAgent
	log    *slog.Logger
	nsName string
	ns     netns.NsHandle
	nlMu   sync.Mutex
	h      *netlink.Handle
	st     *stack.Stack

	mu       sync.Mutex
	nextNIC  tcpip.NICID
	ports    map[string]*port     // by port ref
	hosts    map[string]*hostPort // vm hosts' spliced data ports, by port ref (D57)
	byDev    map[string]*port
	bridges  map[string]*bridge // "network|rack path"
	gws      []*gateway
	switches []*nosSwitch
	byPath   map[string]*nosSwitch
	cables   []*cable
	docker   *nos.Docker
	volumes  []string // the switches' shared volumes, removed after their containers
	attached []string
	// ca issues the TLS material of real switches' management interfaces (the estate CA); nil
	// leaves them without (a gNMI server then listens on its loopback only)
	ca       *netsim.Net
	stateDir string
	// kernel is what up found the Docker host's kernel to support for a NOS's overlays: "vrf",
	// "vxlan" -> nil, or why not
	kernel map[string]error

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

// hostPort is a vm host's data port on a real NOS (D57): a TAP whose device is the switch's port,
// read and written raw. The vm adapter splices the guest's NIC onto it (netsim.Splice); nothing of
// the twin's is behind it.
type hostPort struct {
	plan    *HostPortPlan
	f       *os.File
	carrier int32 // 1 on, 0 off (the cable, the host's power)
}

// errNoCarrier is a host port's write while its cable is down: the frame is not delivered.
var errNoCarrier = errors.New("tap: the port has no carrier")

func (h *hostPort) ReadFrame(b []byte) (int, error) {
	for {
		n, err := h.f.Read(b)
		if err != nil {
			return 0, netsim.ErrClosed
		}
		if atomic.LoadInt32(&h.carrier) == 1 {
			return n, nil
		}
		// sent before the carrier went: a pulled cable carries nothing
	}
}

func (h *hostPort) WriteFrame(b []byte) error {
	if atomic.LoadInt32(&h.carrier) == 0 {
		return errNoCarrier
	}
	_, err := h.f.Write(b)
	return err
}

func (h *hostPort) setCarrier(up bool) error {
	v := int32(0)
	if up {
		v = 1
	}
	if atomic.LoadInt32(&h.carrier) == v {
		return nil
	}
	rc, err := h.f.SyscallConn()
	if err != nil {
		return err
	}
	var ierr error
	if err := rc.Control(func(fd uintptr) { ierr = unix.IoctlSetPointerInt(int(fd), unix.TUNSETCARRIER, int(v)) }); err != nil {
		return err
	}
	if ierr != nil {
		return fmt.Errorf("carrier on %s: %w", h.plan.SwitchIf, ierr)
	}
	atomic.StoreInt32(&h.carrier, v)
	return nil
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
	// container its DHCP relay runs in, beside it (nos.Entry.RelayImage); sidecars its other
	// containers beside it, by role (nos.Entry.Sidecars).
	volumes  []string
	relay    *nos.Container
	sidecars map[string]*nos.Container
	// nvue is the switch's NVUE when its NOS is configured through it (nos.Entry.API), served at
	// its loopback in its namespace by nvueHTTP; nvuePassword is user cumulus's.
	nvue         *nvue.Server
	nvueHTTP     *http.Server
	nvuePassword string
}

// routing is the container a switch's routing daemons run in: its bgp sidecar, or the switch's.
func (sw *nosSwitch) routing() *nos.Container {
	if c := sw.sidecars["bgp"]; c != nil {
		return c
	}
	return sw.c
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
		bridges: map[string]*bridge{}, hosts: map[string]*hostPort{}, byPath: map[string]*nosSwitch{}, ca: a.Net, stateDir: a.StateDir, kernel: map[string]error{}}
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
	for _, hp := range s.plan.HostPorts {
		if err := s.addHostPort(hp); err != nil {
			return fmt.Errorf("host port %s: %w", hp.Ref, err)
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

// addHostPort creates a vm host port's TAP in the twin's namespace, gives its device the switch
// port's MAC and moves it into the NOS's namespace under the port's name there, as a BMC's
// endpoint (D45, D57). Its carrier starts off: syncLinks turns it on while the cable is in and
// the host is powered.
func (s *Substrate) addHostPort(hp *HostPortPlan) error {
	fd := -1
	if err := inNS(s.ns, func() error {
		var err error
		fd, err = tun.OpenTAP(hp.TapIf) // non-blocking: the runtime's poller reads it
		return err
	}); err != nil {
		return fmt.Errorf("TAP %s: %w", hp.TapIf, err)
	}
	h := &hostPort{plan: hp, f: os.NewFile(uintptr(fd), hp.TapIf), carrier: 1}
	if err := h.setCarrier(false); err != nil {
		h.f.Close()
		return err
	}
	sw := s.byPath[hp.Switch.Device.Path]
	if err := s.nl(func(nh *netlink.Handle) error {
		l, err := nh.LinkByName(hp.TapIf)
		if err != nil {
			return err
		}
		// the switch port's MAC; a BlueField's host PF representor (D71) has no model port behind
		// it and keeps the TAP's
		if hp.Peer != nil && hp.Peer.MAC != "" {
			if mac, err := net.ParseMAC(hp.Peer.MAC); err == nil {
				if err := nh.LinkSetHardwareAddr(l, mac); err != nil {
					return err
				}
			}
		}
		if err := nh.LinkSetNsFd(l, int(sw.ns)); err != nil {
			return fmt.Errorf("move into %s: %w", sw.c.Name, err)
		}
		moved, err := sw.h.LinkByName(hp.TapIf)
		if err != nil {
			return err
		}
		return sw.h.LinkSetName(moved, hp.SwitchIf) // the NOS brings it up and into its VLAN
	}); err != nil {
		h.f.Close()
		return err
	}
	s.mu.Lock()
	s.hosts[hp.Ref] = h
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

// HostPort is the switch port a vm host's data port is spliced onto (D57), or nil.
func (s *Substrate) HostPort(ref string) netsim.Cable {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h := s.hosts[ref]; h != nil {
		return h
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
	d.Memory = s.plan.NOSMemory
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
		for _, sc := range e.Sidecars {
			images = append(images, sc.Image)
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
		if _, ok := e.Sidecar("gnmi"); ok && s.ca != nil && sp.Config.Loopback.IsValid() {
			cert, err := s.ca.Certificate(sp.Config.Hostname, []string{sp.Config.Hostname}, []string{sp.Config.Loopback.String()})
			if err != nil {
				return fmt.Errorf("switch %s: its gNMI certificate: %w", sp.Device.Path, err)
			}
			crt, key, err := netsim.PEM(cert)
			if err != nil {
				return err
			}
			sp.Config.GNMI = &nos.TLSFiles{CA: string(s.ca.CAPEM), Cert: crt, Key: key}
		}
		script, err := e.StartScript(sp.Config)
		if err != nil {
			return err
		}
		labels := map[string]string{"dc-twin.estate": s.plan.Estate, "dc-twin.switch": sp.Device.Path, "dc-twin.nos": e.Name}
		var vols []string
		binds := append([]string{}, sp.Binds...)
		for _, m := range e.Shared {
			v := unsafeName.ReplaceAllString(sp.Container+"-"+strings.Trim(m.Path, "/"), "-")
			if err := d.CreateVolume(ctx, v, labels); err != nil {
				return fmt.Errorf("switch %s: volume %s: %w", sp.Device.Path, v, err)
			}
			s.volumes = append(s.volumes, v)
			vols, binds = append(vols, v), append(binds, v+":"+m.Path)
		}
		sysctls := e.Sysctls
		if len(sp.Config.Unnumbered) > 0 {
			// an unnumbered session runs over the port's IPv6 link-local address (RFC 5549)
			sysctls = map[string]string{}
			for k, v := range e.Sysctls {
				sysctls[k] = v
			}
			sysctls["net.ipv6.conf.all.disable_ipv6"], sysctls["net.ipv6.conf.default.disable_ipv6"] = "0", "0"
			sysctls["net.ipv6.conf.all.forwarding"] = "1"
			sysctls["net.ipv6.conf.all.ignore_routes_with_linkdown"], sysctls["net.ipv6.conf.default.ignore_routes_with_linkdown"] = "1", "1"
		}
		c, err := d.Run(ctx, nos.Spec{Name: sp.Container, Image: sp.Image, Hostname: sp.Config.Hostname,
			Entrypoint: nos.Entrypoint, Cmd: []string{script}, Labels: labels, Sysctls: sysctls,
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
	c, err := s.runBeside(sw, "dhcp-relay", sw.plan.NOS.RelayImage, []string{"NET_ADMIN", "NET_RAW"})
	if err != nil {
		return fmt.Errorf("switch %s: its DHCP relay container: %w", sw.plan.Device.Path, err)
	}
	sw.relay = c
	return nil
}

// runBeside starts one of a NOS's containers beside its switch container, as SONiC runs its
// containers beside swss: in the switch's network namespace, with its shared directories (where
// the relay's is read-only, every one's is), tmpfs where SONiC gives it tmpfs.
func (s *Substrate) runBeside(sw *nosSwitch, role, image string, caps []string) (*nos.Container, error) {
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
	return s.docker.Run(ctx, nos.Spec{Name: sw.c.Name + "-" + role, Image: image, Platform: e.Platform,
		NetworkMode: "container:" + sw.c.ID, Env: []string{"RUNTIME_OWNER=local"}, Binds: binds,
		Tmpfs: map[string]string{"/tmp": "", "/var/tmp": ""}, CapAdd: caps,
		Labels: map[string]string{"dc-twin.estate": s.plan.Estate, "dc-twin.switch": sw.plan.Device.Path, "dc-twin.nos": e.Name, "dc-twin.role": role}})
}

// startSidecars starts a switch's other containers (nos.Entry.Sidecars) once it has started.
func (s *Substrate) startSidecars(sw *nosSwitch) error {
	for _, sc := range sw.plan.NOS.Sidecars {
		c, err := s.runBeside(sw, sc.Role, sc.Image, sc.CapAdd)
		if err != nil {
			return fmt.Errorf("switch %s: its %s container: %w", sw.plan.Device.Path, sc.Role, err)
		}
		if sw.sidecars == nil {
			sw.sidecars = map[string]*nos.Container{}
		}
		sw.sidecars[sc.Role] = c
	}
	return nil
}

// waitSidecars waits until every switch's sidecars say they are ready, running the repair a
// sidecar names while it is not.
func (s *Substrate) waitSidecars(deadline time.Time) error {
	for _, sw := range s.switches {
		for _, sc := range sw.plan.NOS.Sidecars {
			c := sw.sidecars[sc.Role]
			if c == nil {
				continue
			}
			for {
				out, _, _ := s.docker.Exec(s.ctx, c.ID, sc.Check...)
				waiting, repair := sc.Pending(sw.plan.Config, out)
				if waiting == "" {
					break
				}
				if _, err := s.docker.Pid(s.ctx, c.ID); err != nil {
					logs, _ := s.docker.Logs(s.ctx, c.ID)
					return fmt.Errorf("switch %s: its %s container %s stopped: %v\n%s", sw.plan.Device.Path, sc.Role, c.Name, err, logs)
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("switch %s: its %s container %s has not got %s:\n%s", sw.plan.Device.Path, sc.Role, c.Name, waiting, out)
				}
				if repair != nil {
					s.log.Info("tap: repairing a sidecar", "switch", sw.plan.Device.Path, "role", sc.Role, "waiting for", waiting, "cmd", strings.Join(repair, " "))
					_, _, _ = s.docker.Exec(s.ctx, c.ID, repair...)
				}
				time.Sleep(2 * time.Second)
			}
		}
	}
	return nil
}

// startNVUE configures a switch whose NOS is configured through NVUE (D56): its NVUE applies the
// startup configuration from the model (the switch's ifreload and frr-reload.py doing the work
// in its container), then serves its REST API at the switch's Loopback0, port 8765, from a socket
// made in the switch's own network namespace, so a client reaches it in band, across the fabric,
// as it reaches a Cumulus switch's.
func (s *Substrate) startNVUE(sw *nosSwitch) error {
	pw := make([]byte, 12)
	if _, err := rand.Read(pw); err != nil {
		return err
	}
	sw.nvuePassword = hex.EncodeToString(pw)
	id := sw.c.ID
	apply := func(ctx context.Context, f nvue.Files) (string, error) {
		out, code, err := s.docker.Exec(ctx, id, append([]string{"sh", "-c", nvue.ApplyScript, "sh"}, nvue.ApplyArgs(f)...)...)
		if err != nil {
			return out, err
		}
		if code != 0 {
			return out, fmt.Errorf("exit %d:\n%s", code, strings.TrimSpace(out))
		}
		return out, nil
	}
	sw.nvue = nvue.NewServer(sw.plan.Config, sw.nvuePassword, apply)
	sw.nvue.SetRunner(func(ctx context.Context, args ...string) (string, error) {
		out, code, err := s.docker.Exec(ctx, id, args...)
		if err == nil && code != 0 {
			err = fmt.Errorf("%s: exit %d: %s", strings.Join(args, " "), code, strings.TrimSpace(out))
		}
		return out, err
	})
	// every write a client makes is a request event: a VPC applied to a DPU, a port moved on a
	// switch (D71, I-07)
	path := sw.plan.Device.Path
	sw.nvue.OnWrite(func(ev nvue.Event) {
		if s.eng == nil {
			return
		}
		canonical := map[string]string{"revision.create": "network.config.revision", "revision.change": "network.config.change",
			"revision.delete": "network.config.discard", "revision.apply": "network.config.apply"}[ev.Op]
		status := 200
		if ev.Op == "revision.apply" && ev.State != "applied" {
			status = 500
		}
		target := "/nvue_v1/revision/" + ev.Revision
		if ev.Path != "" {
			target = "/nvue_v1" + ev.Path + "?rev=" + ev.Revision
		}
		user, addr, _ := strings.Cut(ev.Client, "@")
		s.eng.LogRead(engine.Request{Path: path, Canonical: canonical, Protocol: "nvue", Method: ev.Op, Target: target,
			Source: schema.EventSource{Class: "stack", Principal: user, Address: addr}}, status)
	})
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	defer cancel()
	if err := sw.nvue.ApplyStartup(ctx); err != nil {
		return fmt.Errorf("switch %s: NVUE: %w", sw.plan.Device.Path, err)
	}
	s.log.Info("tap: NVUE applied the startup configuration", "switch", sw.plan.Device.Path)
	if sw.plan.Config.Native {
		var reps []string
		for _, p := range sw.plan.Config.Panel {
			if n := p.If(); strings.HasPrefix(n, "pf") && !strings.HasPrefix(n, "pf0dpu") {
				reps = append(reps, n)
			}
		}
		if err := trapDHCP(sw.ns, reps); err != nil {
			return fmt.Errorf("switch %s: %w", sw.plan.Device.Path, err)
		}
	}
	lo := sw.plan.Config.Loopback
	if sw.plan.Config.Native {
		// a BlueField's HBN: NVUE answers in the DPU's own namespace, where NICo's agent runs
		// beside it (NVUE_HTTPS_ADDRESS=127.0.0.1:8765, D71)
		lo = netip.MustParseAddr("127.0.0.1")
	}
	if s.ca == nil || !lo.IsValid() {
		return nil
	}
	cert, err := s.ca.Certificate(sw.plan.Config.Hostname, []string{sw.plan.Config.Hostname}, []string{lo.String()})
	if err != nil {
		return err
	}
	var ln net.Listener
	if err := inNS(sw.ns, func() error {
		var err error
		ln, err = net.Listen("tcp", netip.AddrPortFrom(lo, nvue.APIPort).String())
		return err
	}); err != nil {
		return fmt.Errorf("switch %s: NVUE at %s: %w", sw.plan.Device.Path, lo, err)
	}
	sw.nvueHTTP = &http.Server{Handler: sw.nvue, ReadHeaderTimeout: 30 * time.Second}
	go func() {
		_ = sw.nvueHTTP.Serve(tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}))
	}()
	return nil
}

// SwitchNetns is a real switch's network namespace (its container's), for a test or a stack that
// cables something into it; the handle stays the substrate's.
func (s *Substrate) SwitchNetns(switchPath string) (netns.NsHandle, bool) {
	sw := s.realSwitch(switchPath)
	if sw == nil {
		return netns.None(), false
	}
	return sw.ns, true
}

// DialIn dials a TCP address from inside a real switch's network namespace: a client beside the
// NOS, as NICo's agent is beside an emulated HBN's NVUE (D71).
func (s *Substrate) DialIn(switchPath string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		sw := s.realSwitch(switchPath)
		if sw == nil {
			return nil, fmt.Errorf("%s is not a real switch", switchPath)
		}
		var c net.Conn
		err := inNS(sw.ns, func() error {
			var err error
			c, err = (&net.Dialer{}).DialContext(ctx, network, addr)
			return err
		})
		return c, err
	}
}

// NVUE is a real switch's NVUE, nil when its NOS has none.
func (s *Substrate) NVUE(switchPath string) *nvue.Server {
	if sw := s.realSwitch(switchPath); sw != nil {
		return sw.nvue
	}
	return nil
}

// probeKernel finds whether the Docker host's kernel has the devices a VRF and a VNI need, in a
// switch's namespace (every container shares the host's kernel): SONiC's managers create them
// when a VRF or a VXLAN tunnel is written to CONFIG_DB, and fail where the kernel has none.
func (s *Substrate) probeKernel() {
	for _, sw := range s.switches {
		if !slices.Contains(sw.plan.NOS.Provides, "switch/vrf") {
			continue
		}
		for _, p := range kernelProbes {
			dev := strings.Fields(p.add)[3]
			out, code, err := s.docker.Exec(s.ctx, sw.c.ID, "sh", "-c", p.add+" 2>&1 && ip link del "+dev)
			switch {
			case err != nil:
				s.kernel[p.feature] = err
			case code != 0:
				s.kernel[p.feature] = errors.New(strings.TrimSpace(out))
			default:
				s.kernel[p.feature] = nil
			}
		}
		return // one switch answers for the host
	}
}

// SwitchesFile is what the substrate hands a stack in stack-inputs about its real switches'
// management interfaces, beside the client certificate they accept (D55).
const SwitchesFile = "switches.json"

// SwitchInput is one real switch in stack-inputs/switches.json.
type SwitchInput struct {
	Path     string `json:"path"`
	Hostname string `json:"hostname"`
	NOS      string `json:"nos"`
	Loopback string `json:"loopback"`
	// GNMI is host:port of its gNMI server, absent where it has none.
	GNMI string `json:"gnmi,omitempty"`
	// NVUE is host:port of its NVUE REST API (https://<NVUE>/nvue_v1/), with the user and
	// password it accepts; absent where it has none.
	NVUE         string `json:"nvue,omitempty"`
	NVUEUser     string `json:"nvueUser,omitempty"`
	NVUEPassword string `json:"nvuePassword,omitempty"`
	ASN          uint32 `json:"asn"`
	// Container is the NOS's container, for an emulated BlueField HBN whose NVUE answers on
	// its own loopback, in its namespace, as HBN's does to the agent beside it (D71).
	Container string `json:"container,omitempty"`
}

// writeSwitchInputs writes stack-inputs/switches.json and, when a switch serves gNMI, a client
// certificate the estate CA issued, the CA, and the client's key: what a site hands the software
// that configures its switches.
func (s *Substrate) writeSwitchInputs() error {
	if s.stateDir == "" || len(s.switches) == 0 {
		return nil
	}
	dir := filepath.Join(s.stateDir, "stack-inputs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var list []SwitchInput
	gnmi := false
	for _, sw := range s.switches {
		c := sw.plan.Config
		in := SwitchInput{Path: sw.plan.Device.Path, Hostname: c.Hostname, NOS: sw.plan.NOS.Name, ASN: c.ASN}
		if c.Loopback.IsValid() {
			in.Loopback = c.Loopback.String()
		}
		if c.GNMI != nil && sw.sidecars["gnmi"] != nil {
			in.GNMI = netip.AddrPortFrom(c.Loopback, nos.SonicGNMIPort).String()
			gnmi = true
		}
		if sw.nvueHTTP != nil {
			in.NVUE, in.NVUEUser, in.NVUEPassword = netip.AddrPortFrom(c.Loopback, nvue.APIPort).String(), nvue.User, sw.nvuePassword
			if c.Native {
				in.NVUE = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), nvue.APIPort).String()
				in.Container = sw.plan.Container
			}
		}
		list = append(list, in)
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, SwitchesFile), b, 0o600); err != nil {
		return err
	}
	if !gnmi {
		return nil
	}
	cert, err := s.ca.ClientCertificate("dctwin-stack")
	if err != nil {
		return err
	}
	crt, key, err := netsim.PEM(cert)
	if err != nil {
		return err
	}
	for name, body := range map[string]string{"switch-client.crt": crt, "switch-client.key": key, "switch-ca.crt": string(s.ca.CAPEM)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
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
	if len(at.Routes) == 0 {
		if err := th.RouteReplace(&netlink.Route{LinkIndex: l.Attrs().Index, Gw: at.Gateway.AsSlice()}); err != nil {
			return fmt.Errorf("default route via %s: %w", at.Gateway, err)
		}
	}
	for _, r := range at.Routes {
		dst := &net.IPNet{IP: r.Addr().AsSlice(), Mask: net.CIDRMask(r.Bits(), 32)}
		if err := th.RouteReplace(&netlink.Route{LinkIndex: l.Attrs().Index, Dst: dst, Gw: at.Gateway.AsSlice()}); err != nil {
			return fmt.Errorf("route %s via %s: %w", r, at.Gateway, err)
		}
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
		if err := s.startSidecars(sw); err != nil {
			return err
		}
		if sw.plan.NOS.API == "nvue" {
			if err := s.startNVUE(sw); err != nil {
				return err
			}
		}
	}
	if err := s.waitSidecars(deadline); err != nil {
		return err
	}
	s.probeKernel()
	if err := s.writeSwitchInputs(); err != nil {
		return fmt.Errorf("stack-inputs/%s: %w", SwitchesFile, err)
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
	s.mu.Lock()
	hosts := make([]*hostPort, 0, len(s.hosts))
	for _, h := range s.hosts {
		hosts = append(hosts, h)
	}
	s.mu.Unlock()
	for _, h := range hosts {
		// the host's port is up while its cable is in and the host is on (engine.PortUp)
		if err := h.setCarrier(s.eng.PortUp(h.plan.Port)); err != nil {
			s.log.Warn("tap: link state", "port", h.plan.Ref, "err", err)
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

// RoutingExec runs a command where a real switch's routing daemons run (vtysh): its bgp
// sidecar, or the switch's own container.
func (s *Substrate) RoutingExec(ctx context.Context, switchPath string, cmd ...string) (string, error) {
	sw := s.realSwitch(switchPath)
	if sw == nil {
		return "", fmt.Errorf("no real switch %s", switchPath)
	}
	out, code, err := s.docker.Exec(ctx, sw.routing().ID, cmd...)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	return out, err
}

// SidecarExec runs a command in one of a real switch's sidecar containers (its role: "bgp",
// "gnmi").
func (s *Substrate) SidecarExec(ctx context.Context, switchPath, role string, cmd ...string) (string, error) {
	sw := s.realSwitch(switchPath)
	if sw == nil || sw.sidecars[role] == nil {
		return "", fmt.Errorf("no %s container for %s", role, switchPath)
	}
	out, code, err := s.docker.Exec(ctx, sw.sidecars[role].ID, cmd...)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	return out, err
}

// Kernel is what up found the Docker host's kernel to support for a NOS's overlays: "vrf",
// "vxlan" -> nil, or why not. Empty when no NOS that provides VRFs runs.
func (s *Substrate) Kernel() map[string]error {
	out := map[string]error{}
	for k, v := range s.kernel {
		out[k] = v
	}
	return out
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
	if len(s.plan.HostPorts) > 0 {
		r.Lines = append(r.Lines, fmt.Sprintf("tap: %d vm host data ports are their real switches' ports (D57): each a TAP moved into its NOS, the guest's NIC spliced onto it frame for frame, so the switch's VLAN, relay and routing carry the guest; the twin relays and routes nothing for them.", len(s.plan.HostPorts)))
	}
	hbn := 0
	for _, sw := range s.switches {
		if sw.plan.NOS.Name == nos.HBN {
			hbn++
		}
	}
	if hbn > 0 {
		r.Lines = append(r.Lines, fmt.Sprintf("tap: %d BlueFields' HBN is EMULATED (D71): each a container of the twin's NVUE over FRR, ifupdown2 and nftables with HBN's interface names, its host's NIC its host PF on pf0hpf_if, its uplinks unnumbered eBGP leaf ports; not NVIDIA's HBN, no ConnectX and no offload (the kernel forwards), the BlueField itself at model fidelity.", hbn))
	}
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
	var gnmi []string
	routing := 0
	for _, sw := range s.switches {
		if sw.sidecars["bgp"] != nil {
			routing++
		}
		if sw.sidecars["gnmi"] != nil && sw.plan.Config.GNMI != nil {
			gnmi = append(gnmi, fmt.Sprintf("%s %s", sw.plan.Config.Hostname, netip.AddrPortFrom(sw.plan.Config.Loopback, nos.SonicGNMIPort)))
		}
	}
	if routing > 0 {
		r.Lines = append(r.Lines, fmt.Sprintf("tap: %d switches route from their NOS's bgp container beside them (SONiC's docker-fpm-frr, unified mode: FRR configured from CONFIG_DB by frrcfgd).", routing))
	}
	if len(gnmi) > 0 {
		r.Lines = append(r.Lines, fmt.Sprintf("tap: gNMI (SONiC's gnmi container, TLS, clients by an estate-CA certificate: stack-inputs/switch-client.crt) at %s.", strings.Join(gnmi, ", ")))
	}
	var nv []string
	for _, sw := range s.switches {
		if sw.nvueHTTP != nil {
			nv = append(nv, fmt.Sprintf("%s %s", sw.plan.Config.Hostname, netip.AddrPortFrom(sw.plan.Config.Loopback, nvue.APIPort)))
		}
	}
	if len(nv) > 0 {
		r.Lines = append(r.Lines, fmt.Sprintf("tap: NVUE (the twin's, D56: revisions rendered to ifupdown2 and FRR in the switch; TLS, user cumulus, its password in stack-inputs/switches.json) at %s.", strings.Join(nv, ", ")))
	}
	for _, p := range kernelProbes {
		err, probed := s.kernel[p.feature]
		switch {
		case !probed:
		case err == nil:
			r.Lines = append(r.Lines, fmt.Sprintf("tap: the Docker host's kernel creates %s devices: a switch can carry them.", p.feature))
		default:
			r.Lines = append(r.Lines, fmt.Sprintf("tap: the Docker host's kernel cannot create %s devices (%v): a switch's NOS fails to create one written to its configuration.", p.feature, err))
		}
	}
	for why, n := range s.plan.Skipped {
		r.Lines = append(r.Lines, fmt.Sprintf("tap: %d BMC ports have no endpoint: %s.", n, why))
	}
	for why, n := range s.plan.HostSkipped {
		r.Lines = append(r.Lines, fmt.Sprintf("tap: %d vm host data ports are not spliced onto a real switch: %s.", n, why))
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
	s.mu.Lock()
	for _, h := range s.agents {
		h.close()
	}
	s.agents = nil
	s.mu.Unlock()
	s.wg.Wait()
	s.mu.Lock()
	for _, p := range s.ports {
		p.close()
	}
	for _, g := range s.gws {
		g.close()
	}
	for _, h := range s.hosts {
		h.f.Close()
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
			if sw.nvueHTTP != nil {
				_ = sw.nvueHTTP.Close()
			}
			if sw.h != nil {
				sw.h.Close()
			}
			if sw.ns.IsOpen() {
				sw.ns.Close()
			}
			wg.Add(1)
			go func(sw *nosSwitch) {
				defer wg.Done()
				for role, c := range sw.sidecars {
					if err := s.docker.Remove(ctx, c.ID); err != nil {
						s.log.Warn("removing a switch's container", "role", role, "container", c.Name, "err", err)
					}
				}
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

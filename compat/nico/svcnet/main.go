//go:build linux

// Command svcnet is the far end of the twin's routed site network (netsim.Uplink, D48) in a Linux
// network namespace, where stack services that identify a host by the address its packets come
// from run, so that they see the guest's own lease. NICo's PXE service is one: it looks a booting
// host up by its TCP peer address and takes no proxy's word for it.
//
// It makes a TAP in the namespace with the service addresses and a route to the guests' networks
// through the twin's gateway, and carries the TAP's frames over one TCP connection the twin dials
// (each frame after its length as a 32-bit big-endian integer, QEMU's stream framing). It also
// forwards TCP ports between the namespace and the host it runs on: from the namespace to services
// that run on the host (`-forward`: NICo's database and Vault, the twin's site gateway), and from
// the host into the namespace (`-expose`: a service that runs in the namespace, such as NICo's API,
// for the stack's tools on the host). It also relays UDP into the namespace at an estate address
// (`-udp`, with `-lo` putting that address on the namespace's loopback): a BMC's IPMI at the site
// address the stack leased it, which a UDP client such as ipmitool can reach through no proxy. It
// needs CAP_NET_ADMIN.
//
// Two ways to run it:
//
//   - in a container (the macOS harness: Docker in a VM): the container is the namespace, and the
//     uplink listener and the forwards' far ends are in it too (the host is reached through the
//     container's own network; -expose is not used);
//
//   - with -netns <name> on a Linux host (as root): the TAP, its addresses and routes and the
//     forwards' listeners are made in the named namespace (`ip netns add`), while the uplink
//     listener and the forwards' dials stay in the namespace svcnet was started in, the host's.
//     Nothing is added to the host's interfaces, routes or firewall.
//
//     svcnet [-netns dctwin-nico-svc] -tap svc0 -addr 10.90.0.17/24 -gateway 10.90.0.1 \
//     -route 10.40.0.0/16 -listen 127.0.0.1:17800 -forward 127.0.0.1:55432=127.0.0.1:55432 \
//     -expose 127.0.0.1:11079=10.90.0.20:11079
//
// With -tap "" it makes no TAP and no uplink, only the forwards: a second svcnet adds forwards
// to a namespace the first one made, once their addresses are known (a BMC's lease):
//
//	svcnet -netns dctwin-nico-svc -tap "" -lo 10.30.16.10/32 -udp 10.30.16.10:623=127.0.0.1:24161
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(s string) error { *l = append(*l, s); return nil }

// inNS runs f with the calling thread in the named namespace ("" : the current one). File
// descriptors and sockets f makes stay in that namespace after the thread returns.
func inNS(name string, f func() error) error {
	if name == "" {
		return f()
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	orig, err := netns.Get()
	if err != nil {
		return err
	}
	defer orig.Close()
	ns, err := netns.GetFromName(name)
	if err != nil {
		return fmt.Errorf("namespace %s: %w", name, err)
	}
	defer ns.Close()
	if err := netns.Set(ns); err != nil {
		return err
	}
	defer netns.Set(orig)
	return f()
}

func main() {
	nsName := flag.String("netns", "", "make the TAP and the forwards' listeners in this network namespace")
	tapName := flag.String("tap", "svc0", "the TAP's name")
	gateway := flag.String("gateway", "", "the twin's gateway on the site network")
	listen := flag.String("listen", ":17800", "where the twin's uplink connects")
	var addrs, routes, forwards, exposes, udps, los list
	flag.Var(&addrs, "addr", "an address (cidr) of the namespace on the site network (repeatable)")
	flag.Var(&udps, "udp", "local=remote UDP relay: listen in the namespace, send from svcnet's own (repeatable)")
	flag.Var(&los, "lo", "an address (cidr) put on the namespace's loopback, for a -udp listener at an estate address (repeatable)")
	flag.Var(&routes, "route", "a guests' network routed through the gateway (repeatable)")
	flag.Var(&forwards, "forward", "local=remote TCP forward: listen in the namespace, dial from svcnet's own (repeatable)")
	flag.Var(&exposes, "expose", "local=remote TCP forward: listen in svcnet's own namespace, dial in the namespace (repeatable)")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	var tap *os.File
	var relays []*net.UDPConn
	err := inNS(*nsName, func() error {
		if *tapName != "" {
			var err error
			if tap, err = openTAP(*tapName); err != nil {
				return err
			}
			if err := configure(*tapName, addrs, *gateway, routes); err != nil {
				return err
			}
		}
		if err := loopback(los); err != nil {
			return err
		}
		for _, u := range udps {
			local, _, ok := strings.Cut(u, "=")
			if !ok {
				return fmt.Errorf("-udp %q: want local=remote", u)
			}
			ua, err := net.ResolveUDPAddr("udp", local)
			if err != nil {
				return err
			}
			c, err := net.ListenUDP("udp", ua)
			if err != nil {
				return fmt.Errorf("udp %s: %w", local, err)
			}
			relays = append(relays, c)
		}
		for _, f := range forwards {
			local, remote, ok := strings.Cut(f, "=")
			if !ok {
				return fmt.Errorf("-forward %q: want local=remote", f)
			}
			l, err := net.Listen("tcp", local)
			if err != nil {
				return fmt.Errorf("forward %s: %w", local, err)
			}
			go forward(l, remote, "")
		}
		return nil
	})
	if err != nil {
		log.Fatalf("svcnet: %v", err)
	}
	for i, u := range udps {
		_, remote, _ := strings.Cut(u, "=")
		go relayUDP(relays[i], remote)
	}
	for _, f := range exposes {
		local, remote, ok := strings.Cut(f, "=")
		if !ok {
			log.Fatalf("-expose %q: want local=remote", f)
		}
		l, err := net.Listen("tcp", local)
		if err != nil {
			log.Fatalf("expose %s: %v", local, err)
		}
		go forward(l, remote, *nsName)
	}
	if *tapName == "" {
		log.Printf("svcnet: forwards only in %s (udp %v, loopback %v)", orDefault(*nsName), udps, los)
		select {}
	}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("svcnet: %s %v in %s, routes %v via %s; uplink at %s", *tapName, addrs, orDefault(*nsName), routes, *gateway, *listen)
	var mu sync.Mutex
	var cur net.Conn
	// the TAP's frames go to whichever uplink connection is current
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := tap.Read(buf)
			if err != nil {
				log.Fatalf("tap read: %v", err)
			}
			mu.Lock()
			c := cur
			mu.Unlock()
			if c == nil {
				continue
			}
			out := binary.BigEndian.AppendUint32(make([]byte, 0, 4+n), uint32(n))
			if _, err := c.Write(append(out, buf[:n]...)); err != nil {
				c.Close()
			}
		}
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("svcnet: uplink from %s", c.RemoteAddr())
		mu.Lock()
		if cur != nil {
			cur.Close()
		}
		cur = c
		mu.Unlock()
		go func(c net.Conn) {
			var h [4]byte
			for {
				if _, err := io.ReadFull(c, h[:]); err != nil {
					break
				}
				b := make([]byte, binary.BigEndian.Uint32(h[:]))
				if _, err := io.ReadFull(c, b); err != nil {
					break
				}
				if _, err := tap.Write(b); err != nil {
					log.Printf("tap write: %v", err)
				}
			}
			log.Printf("svcnet: uplink from %s closed", c.RemoteAddr())
			c.Close()
		}(c)
	}
}

func orDefault(ns string) string {
	if ns == "" {
		return "this namespace"
	}
	return "namespace " + ns
}

// openTAP creates a TAP (no packet information header) and returns its descriptor.
func openTAP(name string) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var req [unix.IFNAMSIZ + 64]byte
	copy(req[:unix.IFNAMSIZ-1], name)
	*(*uint16)(unsafe.Pointer(&req[unix.IFNAMSIZ])) = unix.IFF_TAP | unix.IFF_NO_PI
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(&req[0]))); e != 0 {
		unix.Close(fd)
		return nil, fmt.Errorf("TUNSETIFF: %v", e)
	}
	return os.NewFile(uintptr(fd), "/dev/net/tun"), nil
}

func configure(name string, addrs []string, gateway string, routes []string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		ad, err := netlink.ParseAddr(a)
		if err != nil {
			return err
		}
		if err := netlink.AddrAdd(l, ad); err != nil {
			return fmt.Errorf("address %s: %w", a, err)
		}
	}
	if err := netlink.LinkSetUp(l); err != nil {
		return err
	}
	if lo, err := netlink.LinkByName("lo"); err == nil {
		_ = netlink.LinkSetUp(lo) // a new namespace's loopback starts down
	}
	gw := net.ParseIP(gateway)
	for _, r := range routes {
		_, dst, err := net.ParseCIDR(r)
		if err != nil {
			return err
		}
		if err := netlink.RouteAdd(&netlink.Route{LinkIndex: l.Attrs().Index, Dst: dst, Gw: gw}); err != nil {
			return fmt.Errorf("route %s via %s: %w", r, gateway, err)
		}
	}
	return nil
}

// forward carries each connection to l on to remote, dialled in the namespace dialNS ("": svcnet's
// own).
func forward(l net.Listener, remote, dialNS string) {
	log.Printf("svcnet: forwarding %s to %s", l.Addr(), remote)
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			var u net.Conn
			err := inNS(dialNS, func() error {
				var err error
				u, err = net.Dial("tcp", remote)
				return err
			})
			if err != nil {
				log.Printf("forward %s: %v", remote, err)
				c.Close()
				return
			}
			go func() { io.Copy(u, c); u.Close() }()
			io.Copy(c, u)
			c.Close()
		}()
	}
}

// loopback puts addresses on the namespace's loopback (and brings it up).
func loopback(addrs []string) error {
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		return err
	}
	for _, a := range addrs {
		ad, err := netlink.ParseAddr(a)
		if err != nil {
			return err
		}
		if err := netlink.AddrAdd(lo, ad); err != nil && !os.IsExist(err) {
			return fmt.Errorf("loopback address %s: %w", a, err)
		}
	}
	return nil
}

// relayUDP carries each client's datagrams to c on to remote, from a socket of svcnet's own
// namespace per client, and the replies back to that client from c's address, so the client sees
// its peer at the address it sent to. A client's socket is closed after two idle minutes.
func relayUDP(c *net.UDPConn, remote string) {
	ra, err := net.ResolveUDPAddr("udp", remote)
	if err != nil {
		log.Printf("udp relay %s: %v", remote, err)
		return
	}
	log.Printf("svcnet: relaying udp %s to %s", c.LocalAddr(), remote)
	var mu sync.Mutex
	clients := map[string]*net.UDPConn{}
	buf := make([]byte, 65536)
	for {
		n, from, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		mu.Lock()
		u := clients[from.String()]
		if u == nil {
			if u, err = net.DialUDP("udp", nil, ra); err != nil {
				mu.Unlock()
				log.Printf("udp relay %s: %v", remote, err)
				continue
			}
			clients[from.String()] = u
			go func(u *net.UDPConn, from *net.UDPAddr) {
				b := make([]byte, 65536)
				for {
					u.SetReadDeadline(time.Now().Add(2 * time.Minute))
					n, err := u.Read(b)
					if err != nil {
						break
					}
					c.WriteToUDP(b[:n], from)
				}
				mu.Lock()
				delete(clients, from.String())
				mu.Unlock()
				u.Close()
			}(u, from)
		}
		mu.Unlock()
		u.Write(buf[:n])
	}
}

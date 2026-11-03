package netsim

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
)

// A vm host's NIC on a real NOS's port (D57). On the TAP substrate a `vm` host's data port that
// the model cables to a real switch is that switch's port: a TAP whose device the twin moved into
// the NOS's namespace, as a BMC's is (D45). The guest's NIC is a QEMU stream cable (D48), and the
// splice carries its frames to the TAP and back, unchanged. The switch is the segment: its VLAN
// holds the gateway, its relay forwards the guest's DHCP, its routing carries the guest's
// traffic, and the twin routes and relays nothing for the port.
//
// The splice observes what it carries, as the host port does (D48's observer contract): each
// DHCP message, and each connection the guest opens, as a flow the switch routed. It never
// changes, answers or drops a frame for what it holds (only what the far side cannot take, while
// the guest is off).

// SplicedService is a spliced port's flow's Service: the switch routed it, not the twin.
const SplicedService = "routed by the switch"

// SpliceConfig is a spliced port's observers.
type SpliceConfig struct {
	Ref string
	// OnDHCP is told about each DHCP message: "request" from the guest, "reply" to it.
	OnDHCP func(dir string, payload []byte)
	// Observe is told about each connection the guest opens: a TCP SYN, the first UDP datagram
	// from a source to a destination.
	Observe func(Flow)
	Log     *slog.Logger
}

// Splice carries frames between a vm NIC's cable and its switch port's TAP.
type Splice struct {
	cfg    SpliceConfig
	closed atomic.Bool
	// FromGuest and ToGuest count the frames carried; Dropped what the far side could not take.
	FromGuest, ToGuest, Dropped atomic.Int64

	mu   sync.Mutex
	seen map[flowKey]bool
}

type flowKey struct {
	proto    byte
	src, dst netip.AddrPort
}

// maxSeenFlows bounds what a splice remembers to observe a flow once; past it, it forgets and a
// flow may be observed again.
const maxSeenFlows = 4096

// NewSplice starts carrying frames. Its pumps end when a side's ReadFrame fails (the cable or the
// TAP closed); Close stops it carrying before then.
func NewSplice(guest, port Cable, cfg SpliceConfig) *Splice {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Splice{cfg: cfg, seen: map[flowKey]bool{}}
	go s.pump(guest, port, true)
	go s.pump(port, guest, false)
	return s
}

// Close stops the splice carrying frames; the owner closes the cable and the TAP.
func (s *Splice) Close() { s.closed.Store(true) }

func (s *Splice) pump(from, to Cable, fromGuest bool) {
	buf := make([]byte, 65536)
	for {
		n, err := from.ReadFrame(buf)
		if err != nil {
			return
		}
		if s.closed.Load() {
			continue
		}
		frame := buf[:n]
		if n < ethHeader {
			s.Dropped.Add(1)
			continue
		}
		s.observe(frame, fromGuest)
		if err := to.WriteFrame(frame); err != nil {
			s.Dropped.Add(1)
			continue
		}
		if fromGuest {
			s.FromGuest.Add(1)
		} else {
			s.ToGuest.Add(1)
		}
	}
}

// observe tells the observers what a frame is: DHCP either way, a new flow from the guest.
func (s *Splice) observe(frame []byte, fromGuest bool) {
	if f, ok := ParseUDPFrame(frame); ok {
		switch {
		case fromGuest && f.DstPort == 67:
			if s.cfg.OnDHCP != nil {
				s.cfg.OnDHCP("request", append([]byte(nil), f.Payload...))
			}
			return
		case !fromGuest && f.DstPort == 68:
			if s.cfg.OnDHCP != nil {
				s.cfg.OnDHCP("reply", append([]byte(nil), f.Payload...))
			}
			return
		case fromGuest:
			s.flow(17, netip.AddrPortFrom(f.SrcIP, f.SrcPort), netip.AddrPortFrom(f.DstIP, f.DstPort))
		}
		return
	}
	if !fromGuest || binary.BigEndian.Uint16(frame[12:14]) != 0x0800 || len(frame) < ethHeader+ipv4Header {
		return
	}
	ip := frame[ethHeader:]
	ihl := int(ip[0]&0x0f) * 4
	if ip[0]>>4 != 4 || ip[9] != 6 || ihl < ipv4Header || len(ip) < ihl+20 || binary.BigEndian.Uint16(ip[6:8])&0x1fff != 0 {
		return
	}
	tcp := ip[ihl:]
	if flags := tcp[13]; flags&0x02 == 0 || flags&0x10 != 0 { // a SYN, not a SYN-ACK
		return
	}
	src := netip.AddrPortFrom(netip.AddrFrom4([4]byte(ip[12:16])), binary.BigEndian.Uint16(tcp[0:2]))
	dst := netip.AddrPortFrom(netip.AddrFrom4([4]byte(ip[16:20])), binary.BigEndian.Uint16(tcp[2:4]))
	s.flow(6, src, dst)
}

func (s *Splice) flow(proto byte, src, dst netip.AddrPort) {
	k := flowKey{proto, src, dst}
	s.mu.Lock()
	if s.seen[k] {
		s.mu.Unlock()
		return
	}
	if len(s.seen) >= maxSeenFlows {
		s.seen = map[flowKey]bool{}
	}
	s.seen[k] = true
	s.mu.Unlock()
	if s.cfg.Observe == nil {
		return
	}
	name := "tcp"
	if proto == 17 {
		name = "udp"
	}
	s.cfg.Observe(Flow{Proto: name, Src: src, Dst: dst, Service: SplicedService})
}

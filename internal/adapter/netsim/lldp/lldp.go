// Package lldp is the twin's LLDP for model endpoints (18 — LLDP and cabling, D78): real NOSes run
// their own LLDP agents (lldpd in generic/nvue-frr, SONiC's lldp container), and the twin speaks
// for the endpoints it models (model hosts, BMC ports, model switches) on their TAPs, only while
// they are powered on and have carrier, so a real switch's `lldpcli show neighbors` (and from it a
// day-0 tool's cabling check) sees every cable's far end, model or real.
//
// Frame builds an IEEE 802.1AB LLDPDU in an Ethernet frame and Parse reads one, the twin's own or
// one a real NOS sends to a model endpoint. Sender transmits periodically as lldpd does. The
// package is pure Go with no system calls: the TAP substrate supplies the write.
//
// The layout follows docs/reviews/2026-11-11-day0-vendor-facts.md section F, read from lldpd
// (lldpd/lldpd@62e946403635: src/daemon/lldp-tlv.h, src/daemon/lldpd.h, src/lldp-const.h) and its
// usage page; 802.1AB itself was not read (UNCONFIRMED 16). Every TLV is a 16-bit header, a 7-bit
// type then a 9-bit length, followed by that many bytes. A frame carries, in order, Chassis ID,
// Port ID and TTL (mandatory), then Port Description, System Name, System Description, System
// Capabilities and Management Address when set, 802.3 Maximum Frame Size when set, and End: what
// facts F lists for a Cumulus-like neighbour.
package lldp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// DestMAC is the nearest-bridge group address lldpd sends to by default (LLDP_ADDR_NEAREST_BRIDGE).
var DestMAC = net.HardwareAddr{0x01, 0x80, 0xc2, 0x00, 0x00, 0x0e}

// The other two LLDP group addresses (nearest non-TPMR bridge, nearest customer bridge), which
// Parse accepts too.
var (
	NonTPMRBridgeMAC  = net.HardwareAddr{0x01, 0x80, 0xc2, 0x00, 0x00, 0x03}
	CustomerBridgeMAC = net.HardwareAddr{0x01, 0x80, 0xc2, 0x00, 0x00, 0x00}
)

// EtherType is LLDP's (ETH_P_LLDP).
const EtherType = 0x88cc

// lldpd's timers (src/daemon/lldpd.h): LLDPD_TX_INTERVAL 30 s and LLDPD_TX_HOLD 4, so a TTL of
// 120 s. Cumulus runs the same.
const (
	DefaultInterval = 30
	TxHold          = 4
	DefaultTTL      = DefaultInterval * TxHold
)

// TLV types (src/daemon/lldp-tlv.h).
const (
	TLVEnd          = 0
	TLVChassisID    = 1
	TLVPortID       = 2
	TLVTTL          = 3
	TLVPortDesc     = 4
	TLVSysName      = 5
	TLVSysDesc      = 6
	TLVSysCap       = 7
	TLVMgmtAddr     = 8
	TLVOrgSpecific  = 127
	maxTLVLength    = 511
	maxStringLength = 255
)

// Chassis ID subtypes (src/lldp-const.h).
const (
	ChassisMAC     = 4
	ChassisIfName  = 6
	ChassisLocal   = 7
	chassisNetAddr = 5
)

// Port ID subtypes (src/lldp-const.h): lldpd defaults to the MAC, Cumulus to the interface name.
const (
	PortIfAlias = 1
	PortMAC     = 3
	PortIfName  = 5
	PortLocal   = 7
)

// System capability bits (802.1AB's table, as src/lldp-const.h has them).
const (
	CapOther    = 0x0001
	CapRepeater = 0x0002
	CapBridge   = 0x0004
	CapWLAN     = 0x0008
	CapRouter   = 0x0010
	CapPhone    = 0x0020
	CapDOCSIS   = 0x0040
	CapStation  = 0x0080
)

// The 802.3 OUI and its Maximum Frame Size subtype (facts F).
var oui8023 = [3]byte{0x00, 0x12, 0x0f}

const dot3MaxFrameSize = 4

// Info is what one endpoint says about itself in an LLDPDU.
type Info struct {
	// ChassisMAC is the Chassis ID, subtype 4 (lldpd uses the first interface's MAC; the twin
	// the device's). A parsed frame with another chassis subtype leaves it nil and sets
	// ChassisSubtype and ChassisID.
	ChassisMAC     net.HardwareAddr
	ChassisSubtype byte
	ChassisID      string
	// PortID is the port as the device names it. PortIDSubtype is 5, interface name, when 0; 7
	// (locally assigned) and 3 (PortID a MAC in its text form) are also sent.
	PortID        string
	PortIDSubtype byte
	// PortMAC is the frame's source address when Frame is given none: the port's own MAC, else
	// the chassis's.
	PortMAC net.HardwareAddr
	// PortDesc, SysName and SysDesc are sent when set.
	PortDesc, SysName, SysDesc string
	// MgmtAddr is sent when valid, with MgmtIfIndex as its ifIndex interface number (lldpd's
	// LLDP_MGMT_IFACE_IFINDEX).
	MgmtAddr    netip.Addr
	MgmtIfIndex uint32
	// TTL is how long a receiver keeps this information, in seconds: DefaultTTL when 0. A
	// shutdown frame (Shutdown) carries 0.
	TTL uint16
	// Bridge and Router are the System Capabilities, available and enabled. With neither the
	// endpoint is a station, as lldpd reports a host. Station is set by Parse.
	Bridge, Router, Station bool
	// MaxFrameSize, when set, is sent as 802.3's Maximum Frame Size TLV (Cumulus sends it).
	MaxFrameSize uint16
}

// Frame is an Ethernet frame to DestMAC carrying in's LLDPDU, padded to Ethernet's 60-byte
// minimum. src is the source MAC; nil uses in.PortMAC, else in.ChassisMAC.
func Frame(src net.HardwareAddr, in Info) []byte {
	ttl := in.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	return frame(src, in, ttl)
}

// Shutdown is in's shutdown frame: the mandatory TLVs with a TTL of 0, which tells a receiver
// to forget the endpoint at once (sent by Sender.Stop, as lldpd sends on exit).
func Shutdown(src net.HardwareAddr, in Info) []byte {
	return frame(src, Info{ChassisMAC: in.ChassisMAC, ChassisSubtype: in.ChassisSubtype, ChassisID: in.ChassisID,
		PortID: in.PortID, PortIDSubtype: in.PortIDSubtype, PortMAC: in.PortMAC}, 0)
}

func frame(src net.HardwareAddr, in Info, ttl uint16) []byte {
	if src == nil {
		src = in.PortMAC
	}
	if src == nil {
		src = in.ChassisMAC
	}
	b := make([]byte, 0, 128)
	b = append(b, DestMAC...)
	b = append(b, pad6(src)...)
	b = binary.BigEndian.AppendUint16(b, EtherType)

	if in.ChassisMAC != nil {
		b = tlv(b, TLVChassisID, append([]byte{ChassisMAC}, in.ChassisMAC...))
	} else {
		st := in.ChassisSubtype
		if st == 0 {
			st = ChassisLocal
		}
		b = tlv(b, TLVChassisID, append([]byte{st}, in.ChassisID...))
	}
	st := in.PortIDSubtype
	if st == 0 {
		st = PortIfName
	}
	port := []byte(in.PortID)
	if st == PortMAC {
		if mac, err := net.ParseMAC(in.PortID); err == nil {
			port = mac
		}
	}
	b = tlv(b, TLVPortID, append([]byte{st}, port...))
	b = tlv(b, TLVTTL, binary.BigEndian.AppendUint16(nil, ttl))
	for _, s := range []struct {
		t int
		v string
	}{{TLVPortDesc, in.PortDesc}, {TLVSysName, in.SysName}, {TLVSysDesc, in.SysDesc}} {
		if s.v != "" {
			b = tlv(b, s.t, []byte(trunc(s.v)))
		}
	}
	if ttl != 0 {
		var caps uint16
		if in.Bridge {
			caps |= CapBridge
		}
		if in.Router {
			caps |= CapRouter
		}
		if caps == 0 {
			caps = CapStation
		}
		b = tlv(b, TLVSysCap, binary.BigEndian.AppendUint32(nil, uint32(caps)<<16|uint32(caps)))
	}
	if in.MgmtAddr.IsValid() {
		// address string length (subtype + address), IANA address family (1 IPv4, 2 IPv6), the
		// address, interface numbering subtype 2 (ifIndex), the interface number, OID length 0
		a := in.MgmtAddr.Unmap()
		fam := byte(1)
		if a.Is6() {
			fam = 2
		}
		raw := a.AsSlice()
		v := append([]byte{byte(1 + len(raw)), fam}, raw...)
		v = append(v, 2)
		v = binary.BigEndian.AppendUint32(v, in.MgmtIfIndex)
		v = append(v, 0)
		b = tlv(b, TLVMgmtAddr, v)
	}
	if in.MaxFrameSize != 0 && ttl != 0 {
		v := append(oui8023[:], dot3MaxFrameSize)
		b = tlv(b, TLVOrgSpecific, binary.BigEndian.AppendUint16(v, in.MaxFrameSize))
	}
	b = tlv(b, TLVEnd, nil)
	for len(b) < 60 {
		b = append(b, 0)
	}
	return b
}

// tlv appends one TLV: the type in the header's top 7 bits, the length in its low 9.
func tlv(b []byte, typ int, v []byte) []byte {
	if len(v) > maxTLVLength {
		v = v[:maxTLVLength]
	}
	b = binary.BigEndian.AppendUint16(b, uint16(typ)<<9|uint16(len(v)))
	return append(b, v...)
}

func trunc(s string) string {
	if len(s) > maxStringLength {
		return s[:maxStringLength]
	}
	return s
}

func pad6(mac net.HardwareAddr) []byte {
	out := make([]byte, 6)
	copy(out, mac)
	return out
}

// Parse reads an LLDP Ethernet frame (the destination one of the three LLDP group addresses,
// optionally 802.1Q-tagged) into an Info. Chassis ID, Port ID and TTL are required; unknown and
// organisation-specific TLVs other than 802.3's Maximum Frame Size are skipped.
func Parse(f []byte) (Info, error) {
	var in Info
	if len(f) < 14 {
		return in, errors.New("lldp: frame too short")
	}
	dst := net.HardwareAddr(f[:6])
	if !equalMAC(dst, DestMAC) && !equalMAC(dst, NonTPMRBridgeMAC) && !equalMAC(dst, CustomerBridgeMAC) {
		return in, fmt.Errorf("lldp: destination %s is not an LLDP group address", dst)
	}
	et, p := binary.BigEndian.Uint16(f[12:]), f[14:]
	if et == 0x8100 && len(p) >= 4 {
		et, p = binary.BigEndian.Uint16(p[2:]), p[4:]
	}
	if et != EtherType {
		return in, fmt.Errorf("lldp: ethertype %#04x", et)
	}
	in.PortMAC = append(net.HardwareAddr(nil), f[6:12]...)
	var seen [4]bool
	for {
		if len(p) < 2 {
			return in, errors.New("lldp: LLDPDU ends without an End TLV")
		}
		h := binary.BigEndian.Uint16(p)
		typ, n := int(h>>9), int(h&0x1ff)
		p = p[2:]
		if n > len(p) {
			return in, fmt.Errorf("lldp: TLV %d length %d past the frame", typ, n)
		}
		v := p[:n]
		p = p[n:]
		switch typ {
		case TLVEnd:
			if !seen[TLVChassisID] || !seen[TLVPortID] || !seen[TLVTTL] {
				return in, errors.New("lldp: missing a mandatory TLV (Chassis ID, Port ID, TTL)")
			}
			return in, nil
		case TLVChassisID:
			if n < 2 {
				return in, errors.New("lldp: short Chassis ID")
			}
			if v[0] == ChassisMAC && n == 7 {
				in.ChassisMAC = append(net.HardwareAddr(nil), v[1:]...)
			} else {
				in.ChassisSubtype, in.ChassisID = v[0], string(v[1:])
				if v[0] == chassisNetAddr && n >= 3 {
					in.ChassisID = netAddr(v[1:])
				}
			}
		case TLVPortID:
			if n < 2 {
				return in, errors.New("lldp: short Port ID")
			}
			in.PortIDSubtype, in.PortID = v[0], string(v[1:])
			if v[0] == PortMAC && n == 7 {
				in.PortID = net.HardwareAddr(v[1:]).String()
			}
		case TLVTTL:
			if n < 2 {
				return in, errors.New("lldp: short TTL")
			}
			in.TTL = binary.BigEndian.Uint16(v)
		case TLVPortDesc:
			in.PortDesc = string(v)
		case TLVSysName:
			in.SysName = string(v)
		case TLVSysDesc:
			in.SysDesc = string(v)
		case TLVSysCap:
			if n == 4 {
				en := binary.BigEndian.Uint16(v[2:])
				in.Bridge, in.Router, in.Station = en&CapBridge != 0, en&CapRouter != 0, en&CapStation != 0
			}
		case TLVMgmtAddr:
			if n >= 2 && int(v[0]) >= 2 && 1+int(v[0])+5 <= n && !in.MgmtAddr.IsValid() {
				if a, ok := netip.AddrFromSlice(v[2 : 1+int(v[0])]); ok {
					in.MgmtAddr = a
					in.MgmtIfIndex = binary.BigEndian.Uint32(v[1+int(v[0])+1:])
				}
			}
		case TLVOrgSpecific:
			if n == 6 && [3]byte(v[:3]) == oui8023 && v[3] == dot3MaxFrameSize {
				in.MaxFrameSize = binary.BigEndian.Uint16(v[4:])
			}
		}
		if typ < len(seen) {
			seen[typ] = true
		}
	}
}

// netAddr is a network-address chassis ID (IANA family, address) in text form.
func netAddr(v []byte) string {
	if a, ok := netip.AddrFromSlice(v[1:]); ok {
		return a.String()
	}
	return string(v)
}

func equalMAC(a, b net.HardwareAddr) bool { return string(a) == string(b) }

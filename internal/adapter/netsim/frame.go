package netsim

import (
	"encoding/binary"
	"net"
	"net/netip"
)

// UDPFrame is an Ethernet II / IPv4 / UDP datagram as a raw socket sees it: what a DHCP client
// sends before it has an address, and what a relay sends back to a client that has none yet.
type UDPFrame struct {
	SrcMAC, DstMAC   net.HardwareAddr
	SrcIP, DstIP     netip.Addr
	SrcPort, DstPort uint16
	TTL              uint8 // 0: 64
	Payload          []byte
}

// BroadcastMAC is ff:ff:ff:ff:ff:ff.
var BroadcastMAC = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

const (
	ethHeader  = 14
	ipv4Header = 20
	udpHeader  = 8
)

// Marshal encodes the frame with valid IPv4 and UDP checksums.
func (f UDPFrame) Marshal() []byte {
	n := ethHeader + ipv4Header + udpHeader + len(f.Payload)
	b := make([]byte, n)
	copy(b[0:6], f.DstMAC)
	copy(b[6:12], f.SrcMAC)
	binary.BigEndian.PutUint16(b[12:14], 0x0800)
	ip := b[ethHeader : ethHeader+ipv4Header]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(ipv4Header+udpHeader+len(f.Payload)))
	ttl := f.TTL
	if ttl == 0 {
		ttl = 64
	}
	ip[8] = ttl
	ip[9] = 17
	src, dst := f.SrcIP.As4(), f.DstIP.As4()
	copy(ip[12:16], src[:])
	copy(ip[16:20], dst[:])
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip, 0))
	u := b[ethHeader+ipv4Header:]
	binary.BigEndian.PutUint16(u[0:2], f.SrcPort)
	binary.BigEndian.PutUint16(u[2:4], f.DstPort)
	binary.BigEndian.PutUint16(u[4:6], uint16(udpHeader+len(f.Payload)))
	copy(u[udpHeader:], f.Payload)
	// pseudo-header: source, destination, protocol, UDP length
	var ph uint32
	ph += uint32(binary.BigEndian.Uint16(src[0:2])) + uint32(binary.BigEndian.Uint16(src[2:4]))
	ph += uint32(binary.BigEndian.Uint16(dst[0:2])) + uint32(binary.BigEndian.Uint16(dst[2:4]))
	ph += 17 + uint32(udpHeader+len(f.Payload))
	cs := checksum(u, ph)
	if cs == 0 {
		cs = 0xffff // RFC 768: a computed zero is sent as all ones
	}
	binary.BigEndian.PutUint16(u[6:8], cs)
	return b
}

// ParseUDPFrame decodes an Ethernet II / IPv4 / UDP frame; anything else (another protocol, a
// fragment, IP options it cannot skip) is not ok.
func ParseUDPFrame(b []byte) (UDPFrame, bool) {
	var f UDPFrame
	if len(b) < ethHeader+ipv4Header+udpHeader || binary.BigEndian.Uint16(b[12:14]) != 0x0800 {
		return f, false
	}
	ip := b[ethHeader:]
	ihl := int(ip[0]&0x0f) * 4
	if ip[0]>>4 != 4 || ihl < ipv4Header || len(ip) < ihl+udpHeader || ip[9] != 17 {
		return f, false
	}
	if binary.BigEndian.Uint16(ip[6:8])&0x3fff != 0 { // a fragment
		return f, false
	}
	total := int(binary.BigEndian.Uint16(ip[2:4]))
	if total > len(ip) || total < ihl+udpHeader {
		return f, false
	}
	u := ip[ihl:total]
	ulen := int(binary.BigEndian.Uint16(u[4:6]))
	if ulen < udpHeader || ulen > len(u) {
		return f, false
	}
	f.DstMAC = net.HardwareAddr(append([]byte(nil), b[0:6]...))
	f.SrcMAC = net.HardwareAddr(append([]byte(nil), b[6:12]...))
	f.SrcIP = netip.AddrFrom4([4]byte(ip[12:16]))
	f.DstIP = netip.AddrFrom4([4]byte(ip[16:20]))
	f.TTL = ip[8]
	f.SrcPort = binary.BigEndian.Uint16(u[0:2])
	f.DstPort = binary.BigEndian.Uint16(u[2:4])
	f.Payload = append([]byte(nil), u[udpHeader:ulen]...)
	return f, true
}

// checksum is the Internet checksum (RFC 1071) of b, starting from a partial sum.
func checksum(b []byte, sum uint32) uint16 {
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

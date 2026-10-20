package netsim

import (
	"bytes"
	"net"
	"net/netip"
	"testing"
)

// A frame survives a round trip with valid IPv4 and UDP checksums: re-summing a header that
// carries its checksum yields zero.
func TestUDPFrameRoundTrip(t *testing.T) {
	mac, _ := net.ParseMAC("02:dc:10:00:00:05")
	f := UDPFrame{SrcMAC: mac, DstMAC: BroadcastMAC, SrcIP: netip.IPv4Unspecified(), DstIP: netip.MustParseAddr("255.255.255.255"),
		SrcPort: 68, DstPort: 67, Payload: []byte("a DHCPDISCOVER, more or less")}
	b := f.Marshal()
	g, ok := ParseUDPFrame(b)
	if !ok {
		t.Fatal("did not parse")
	}
	if g.SrcMAC.String() != mac.String() || g.DstIP != f.DstIP || g.SrcPort != 68 || g.DstPort != 67 || !bytes.Equal(g.Payload, f.Payload) {
		t.Fatalf("round trip: %+v", g)
	}
	if c := checksum(b[14:34], 0); c != 0 {
		t.Errorf("IPv4 header checksum does not verify (%#x)", c)
	}
	u := b[34:]
	src, dst := f.SrcIP.As4(), f.DstIP.As4()
	ph := uint32(src[0])<<8 | uint32(src[1])
	ph += uint32(src[2])<<8 | uint32(src[3])
	ph += uint32(dst[0])<<8 | uint32(dst[1])
	ph += uint32(dst[2])<<8 | uint32(dst[3])
	ph += 17 + uint32(len(u))
	if c := checksum(u, ph); c != 0 {
		t.Errorf("UDP checksum does not verify (%#x)", c)
	}
	if _, ok := ParseUDPFrame(b[:30]); ok {
		t.Error("a truncated frame parsed")
	}
}

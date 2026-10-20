package dhcp

import (
	"bytes"
	"net"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

// Option 82 sub-options the twin relay writes (RFC 3046, RFC 3527, RFC 8357).
const (
	subCircuitID     = 1
	subRemoteID      = 2
	subLinkSelection = 5
	subRelayPort     = 19
)

// optRAI is option 82, the relay agent information option.
var optRAI = dhcpv4.OptionRelayAgentInformation

// raiBytes encodes option 82's sub-options in the order the relay inserts them. relayPort is
// the relay's UDP source port when it is not 67 (0 otherwise).
//
// The remote id is the switch's MAC as text ("b8:3f:d2:13:04:d4"), not its six octets. RFC 3046
// leaves the sub-option opaque, but NICo reads both ids as UTF-8 text: its dhcp-server
// (crates/dhcp-server/src/util.rs, u8_to_hex_string is std::str::from_utf8) recorded the binary
// MAC as remote_id=None, and its Kea hook (crates/dhcp/src/discovery.rs,
// discovery_set_remote_id) refuses a remote id that is not valid UTF-8, which makes the hook drop
// the request (crates/dhcp/src/kea/callouts.cc). A text MAC is what NICo logs and caches by.
func raiBytes(circuit string, remote net.HardwareAddr, link net.IP, relayPort int) []byte {
	var b bytes.Buffer
	tlv := func(code byte, v []byte) {
		b.WriteByte(code)
		b.WriteByte(byte(len(v)))
		b.Write(v)
	}
	tlv(subCircuitID, []byte(circuit))
	if len(remote) > 0 {
		tlv(subRemoteID, []byte(remote.String()))
	}
	if ip4 := link.To4(); ip4 != nil {
		tlv(subLinkSelection, ip4)
	}
	if relayPort != 0 {
		// RFC 8357: a relay that does not use port 67 says so with sub-option 19, and the server
		// answers the UDP source port of the relayed message. ISC dhcrelay sends it empty; Kea
		// 2.6 parses it as a 16-bit integer and drops the whole message as "OptionInt 19
		// truncated" when it is empty, so the relay carries its source port in it, which both
		// readings accept (the server still answers the packet's source port).
		tlv(subRelayPort, []byte{byte(relayPort >> 8), byte(relayPort)})
	}
	return b.Bytes()
}

// subOptions parses option 82's value into its sub-options.
func subOptions(v []byte) map[byte][]byte {
	out := map[byte][]byte{}
	for len(v) >= 2 {
		code, n := v[0], int(v[1])
		if len(v) < 2+n {
			break
		}
		out[code] = append(out[code], v[2:2+n]...)
		v = v[2+n:]
	}
	return out
}

// marshal serialises a message with option 82, when present, as the last option before End:
// RFC 3046 §2.1 says a relay agent SHALL add it last, and some servers parse it that way. The
// library sorts options by code, so the option is spliced in by hand.
func marshal(m *dhcpv4.DHCPv4) []byte {
	rai := m.Options.Get(optRAI)
	if rai == nil {
		return m.ToBytes()
	}
	c := *m
	c.Options = dhcpv4.Options{}
	for k, v := range m.Options {
		if k != optRAI.Code() {
			c.Options[k] = v
		}
	}
	b := c.ToBytes()
	// find End after the 236-byte header and the 4-byte cookie, walking TLVs
	i := 240
	for i < len(b) {
		switch b[i] {
		case 0: // pad
			i++
			continue
		case 255:
		default:
			if i+1 >= len(b) {
				return m.ToBytes()
			}
			i += 2 + int(b[i+1])
			continue
		}
		break
	}
	if i >= len(b) {
		return m.ToBytes()
	}
	out := make([]byte, 0, len(b)+2+len(rai))
	out = append(out, b[:i]...)
	for len(rai) > 0 { // RFC 3396 split, never needed for what the relay writes
		n := len(rai)
		if n > 255 {
			n = 255
		}
		out = append(out, optRAI.Code(), byte(n))
		out = append(out, rai[:n]...)
		rai = rai[n:]
	}
	out = append(out, 255)
	for len(out) < 300 {
		out = append(out, 0)
	}
	return out
}

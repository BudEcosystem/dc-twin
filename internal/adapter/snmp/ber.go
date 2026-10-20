package snmp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The BER subset an SNMPv1/v2c agent needs (X.690 definite lengths, RFC 1157 and RFC 3416
// PDUs, RFC 2578 application types). It is written here rather than borrowed from gosnmp,
// whose decoder is a manager's (it validates a client configuration first and logs every
// value), so the agent controls exactly what it accepts and what it echoes.

// Universal, application and context tags.
const (
	tagInteger        byte = 0x02
	tagOctetString    byte = 0x04
	tagNull           byte = 0x05
	tagOID            byte = 0x06
	tagSequence       byte = 0x30
	tagIPAddress      byte = 0x40
	tagCounter32      byte = 0x41
	tagGauge32        byte = 0x42
	tagTimeTicks      byte = 0x43
	tagOpaque         byte = 0x44
	tagCounter64      byte = 0x46
	tagNoSuchObject   byte = 0x80
	tagNoSuchInstance byte = 0x81
	tagEndOfMibView   byte = 0x82
)

// PDU types.
const (
	pduGet      byte = 0xa0
	pduGetNext  byte = 0xa1
	pduResponse byte = 0xa2
	pduSet      byte = 0xa3
	pduTrapV1   byte = 0xa4
	pduGetBulk  byte = 0xa5
	pduInform   byte = 0xa6
	pduTrapV2   byte = 0xa7
	pduReport   byte = 0xa8
)

// Message versions.
const (
	versionV1  = 0
	versionV2c = 1
)

// Error statuses (RFC 3416 §3; 0–5 are also SNMPv1's).
const (
	errNoError             = 0
	errTooBig              = 1
	errNoSuchName          = 2
	errBadValue            = 3
	errReadOnly            = 4
	errGenErr              = 5
	errNoAccess            = 6
	errWrongType           = 7
	errWrongLength         = 8
	errWrongEncoding       = 9
	errWrongValue          = 10
	errNoCreation          = 11
	errInconsistentValue   = 12
	errResourceUnavailable = 13
	errCommitFailed        = 14
	errUndoFailed          = 15
	errAuthorizationError  = 16
	errNotWritable         = 17
	errInconsistentName    = 18
)

// v1Error maps an SNMPv2 error status onto SNMPv1's (RFC 3584 §4.4).
func v1Error(status int) int {
	switch status {
	case errWrongType, errWrongLength, errWrongEncoding, errWrongValue, errInconsistentValue:
		return errBadValue
	case errNoAccess, errNotWritable, errNoCreation, errInconsistentName, errAuthorizationError:
		return errNoSuchName
	case errResourceUnavailable, errCommitFailed, errUndoFailed:
		return errGenErr
	}
	return status
}

// OID is an object identifier.
type OID []uint32

// ParseOID reads dotted decimal, with or without a leading dot.
func ParseOID(s string) (OID, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), ".")
	if s == "" {
		return nil, errors.New("empty OID")
	}
	parts := strings.Split(s, ".")
	o := make(OID, len(parts))
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("OID %q: arc %q: %w", s, p, err)
		}
		o[i] = uint32(n)
	}
	return o, nil
}

func mustOID(s string) OID {
	o, err := ParseOID(s)
	if err != nil {
		panic(err)
	}
	return o
}

func (o OID) String() string {
	var b strings.Builder
	for i, a := range o {
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(strconv.FormatUint(uint64(a), 10))
	}
	return b.String()
}

// Compare orders OIDs lexicographically by arc value, a prefix first (RFC 3416 §4.2.2).
func (o OID) Compare(p OID) int {
	for i := 0; i < len(o) && i < len(p); i++ {
		if o[i] != p[i] {
			if o[i] < p[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(o) < len(p):
		return -1
	case len(o) > len(p):
		return 1
	}
	return 0
}

// HasPrefix reports whether p is a prefix of o (or equal to it).
func (o OID) HasPrefix(p OID) bool {
	if len(p) > len(o) {
		return false
	}
	for i := range p {
		if o[i] != p[i] {
			return false
		}
	}
	return true
}

// Append returns a new OID with arcs added.
func (o OID) Append(arcs ...uint32) OID {
	out := make(OID, 0, len(o)+len(arcs))
	return append(append(out, o...), arcs...)
}

// value is a varbind value: the tag says which field holds it.
type value struct {
	tag byte
	i   int64  // INTEGER
	u   uint64 // Counter32, Gauge32, TimeTicks, Counter64
	b   []byte // OCTET STRING, IpAddress, Opaque, and the raw content of anything else
	oid OID    // OBJECT IDENTIFIER
}

func intValue(i int64) value    { return value{tag: tagInteger, i: i} }
func strValue(s string) value   { return value{tag: tagOctetString, b: []byte(s)} }
func oidValue(o OID) value      { return value{tag: tagOID, oid: o} }
func ticksValue(t uint64) value { return value{tag: tagTimeTicks, u: t} }
func exception(tag byte) value  { return value{tag: tag} }

type varbind struct {
	oid OID
	val value
}

// message is an SNMPv1/v2c message. For GetBulk, errStatus and errIndex carry non-repeaters
// and max-repetitions.
type message struct {
	version   int64
	community []byte
	pdu       byte
	requestID int64
	errStatus int64
	errIndex  int64
	vbs       []varbind
}

var errBER = errors.New("malformed BER")

// readTLV reads one definite-length TLV with a single-octet tag.
func readTLV(b []byte) (tag byte, content, rest []byte, err error) {
	if len(b) < 2 {
		return 0, nil, nil, errBER
	}
	tag = b[0]
	if tag&0x1f == 0x1f {
		return 0, nil, nil, fmt.Errorf("%w: multi-octet tag", errBER)
	}
	l, off := int(b[1]), 2
	if l&0x80 != 0 {
		n := l & 0x7f
		if n == 0 || n > 4 || len(b) < 2+n {
			return 0, nil, nil, fmt.Errorf("%w: length", errBER)
		}
		l = 0
		for _, c := range b[2 : 2+n] {
			l = l<<8 | int(c)
		}
		off = 2 + n
	}
	if l < 0 || len(b)-off < l {
		return 0, nil, nil, fmt.Errorf("%w: truncated", errBER)
	}
	return tag, b[off : off+l], b[off+l:], nil
}

func expect(b []byte, want byte) (content, rest []byte, err error) {
	tag, c, r, err := readTLV(b)
	if err != nil {
		return nil, nil, err
	}
	if tag != want {
		return nil, nil, fmt.Errorf("%w: tag 0x%02x, want 0x%02x", errBER, tag, want)
	}
	return c, r, nil
}

func decodeInt(c []byte) (int64, error) {
	if len(c) == 0 || len(c) > 8 {
		return 0, fmt.Errorf("%w: integer length %d", errBER, len(c))
	}
	v := int64(int8(c[0]))
	for _, x := range c[1:] {
		v = v<<8 | int64(x)
	}
	return v, nil
}

func decodeUint(c []byte) (uint64, error) {
	if len(c) == 0 || len(c) > 9 || (len(c) == 9 && c[0] != 0) {
		return 0, fmt.Errorf("%w: unsigned length %d", errBER, len(c))
	}
	var v uint64
	for _, x := range c {
		v = v<<8 | uint64(x)
	}
	return v, nil
}

func decodeOID(c []byte) (OID, error) {
	if len(c) == 0 {
		return OID{0, 0}, nil
	}
	var arcs []uint64
	var cur uint64
	for i, x := range c {
		cur = cur<<7 | uint64(x&0x7f)
		if cur > 0xffffffff+80 {
			return nil, fmt.Errorf("%w: OID arc too large", errBER)
		}
		if x&0x80 == 0 {
			arcs = append(arcs, cur)
			cur = 0
		} else if i == len(c)-1 {
			return nil, fmt.Errorf("%w: OID truncated", errBER)
		}
	}
	if len(arcs) > 127 {
		return nil, fmt.Errorf("%w: OID longer than 128 arcs", errBER)
	}
	first := arcs[0]
	var o OID
	switch {
	case first < 40:
		o = OID{0, uint32(first)}
	case first < 80:
		o = OID{1, uint32(first - 40)}
	default:
		if first-80 > 0xffffffff {
			return nil, fmt.Errorf("%w: OID arc too large", errBER)
		}
		o = OID{2, uint32(first - 80)}
	}
	for _, a := range arcs[1:] {
		if a > 0xffffffff {
			return nil, fmt.Errorf("%w: OID arc too large", errBER)
		}
		o = append(o, uint32(a))
	}
	return o, nil
}

func decodeValue(tag byte, c []byte) (value, error) {
	v := value{tag: tag}
	var err error
	switch tag {
	case tagInteger:
		v.i, err = decodeInt(c)
	case tagCounter32, tagGauge32, tagTimeTicks, tagCounter64:
		v.u, err = decodeUint(c)
	case tagOID:
		v.oid, err = decodeOID(c)
	case tagNull, tagNoSuchObject, tagNoSuchInstance, tagEndOfMibView:
		if len(c) != 0 {
			err = fmt.Errorf("%w: non-empty null", errBER)
		}
	default:
		// OCTET STRING, IpAddress, Opaque and anything unknown keep their raw content, so a
		// SET of an unexpected type can be refused with wrongType rather than dropped.
		v.b = append([]byte(nil), c...)
	}
	return v, err
}

// decodeMessage parses an SNMPv1/v2c message. A v3 message fails here (its second field is a
// header SEQUENCE, not a community), which the agent treats as a drop.
func decodeMessage(b []byte) (*message, error) {
	body, _, err := expect(b, tagSequence)
	if err != nil {
		return nil, err
	}
	m := &message{}
	c, body, err := expect(body, tagInteger)
	if err != nil {
		return nil, err
	}
	if m.version, err = decodeInt(c); err != nil {
		return nil, err
	}
	if m.community, body, err = expect(body, tagOctetString); err != nil {
		return nil, err
	}
	pduTag, pdu, _, err := readTLV(body)
	if err != nil {
		return nil, err
	}
	m.pdu = pduTag
	for _, dst := range []*int64{&m.requestID, &m.errStatus, &m.errIndex} {
		if c, pdu, err = expect(pdu, tagInteger); err != nil {
			return nil, err
		}
		if *dst, err = decodeInt(c); err != nil {
			return nil, err
		}
	}
	vbl, _, err := expect(pdu, tagSequence)
	if err != nil {
		return nil, err
	}
	for len(vbl) > 0 {
		var vb []byte
		if vb, vbl, err = expect(vbl, tagSequence); err != nil {
			return nil, err
		}
		oc, rest, err := expect(vb, tagOID)
		if err != nil {
			return nil, err
		}
		o, err := decodeOID(oc)
		if err != nil {
			return nil, err
		}
		vt, vc, _, err := readTLV(rest)
		if err != nil {
			return nil, err
		}
		v, err := decodeValue(vt, vc)
		if err != nil {
			return nil, err
		}
		m.vbs = append(m.vbs, varbind{oid: o, val: v})
	}
	return m, nil
}

// ---------------------------------------------------------------- encoding

func appendLength(dst []byte, l int) []byte {
	switch {
	case l < 0x80:
		return append(dst, byte(l))
	case l <= 0xff:
		return append(dst, 0x81, byte(l))
	case l <= 0xffff:
		return append(dst, 0x82, byte(l>>8), byte(l))
	case l <= 0xffffff:
		return append(dst, 0x83, byte(l>>16), byte(l>>8), byte(l))
	}
	return append(dst, 0x84, byte(l>>24), byte(l>>16), byte(l>>8), byte(l))
}

// tlvLen is the encoded size of a TLV whose content is n octets.
func tlvLen(n int) int { return 1 + len(appendLength(nil, n)) + n }

func tlv(tag byte, content []byte) []byte {
	out := make([]byte, 0, tlvLen(len(content)))
	out = append(out, tag)
	out = appendLength(out, len(content))
	return append(out, content...)
}

func encodeInt(i int64) []byte {
	n := 1
	for n < 8 {
		// the shortest two's complement that round-trips
		shifted := i >> (8*n - 1)
		if shifted == 0 || shifted == -1 {
			break
		}
		n++
	}
	out := make([]byte, n)
	for k := n - 1; k >= 0; k-- {
		out[k] = byte(i)
		i >>= 8
	}
	return out
}

func encodeUint(u uint64) []byte {
	var out []byte
	for {
		out = append([]byte{byte(u)}, out...)
		u >>= 8
		if u == 0 {
			break
		}
	}
	if out[0]&0x80 != 0 {
		out = append([]byte{0}, out...)
	}
	return out
}

func appendBase128(dst []byte, v uint64) []byte {
	var tmp [10]byte
	n := 0
	for {
		tmp[n] = byte(v & 0x7f)
		n++
		v >>= 7
		if v == 0 {
			break
		}
	}
	for k := n - 1; k >= 0; k-- {
		b := tmp[k]
		if k > 0 {
			b |= 0x80
		}
		dst = append(dst, b)
	}
	return dst
}

func encodeOID(o OID) []byte {
	for len(o) < 2 {
		o = append(o, 0)
	}
	out := appendBase128(nil, uint64(o[0])*40+uint64(o[1]))
	for _, a := range o[2:] {
		out = appendBase128(out, uint64(a))
	}
	return out
}

func encodeValue(v value) []byte {
	switch v.tag {
	case tagInteger:
		return tlv(tagInteger, encodeInt(v.i))
	case tagCounter32, tagGauge32, tagTimeTicks, tagCounter64:
		return tlv(v.tag, encodeUint(v.u))
	case tagOID:
		return tlv(tagOID, encodeOID(v.oid))
	case tagNull, tagNoSuchObject, tagNoSuchInstance, tagEndOfMibView:
		return tlv(v.tag, nil)
	}
	return tlv(v.tag, v.b)
}

func encodeVarbind(vb varbind) []byte {
	return tlv(tagSequence, append(tlv(tagOID, encodeOID(vb.oid)), encodeValue(vb.val)...))
}

// encode serialises the message.
func (m *message) encode() []byte {
	var vbl []byte
	for _, vb := range m.vbs {
		vbl = append(vbl, encodeVarbind(vb)...)
	}
	pdu := tlv(tagInteger, encodeInt(m.requestID))
	pdu = append(pdu, tlv(tagInteger, encodeInt(m.errStatus))...)
	pdu = append(pdu, tlv(tagInteger, encodeInt(m.errIndex))...)
	pdu = append(pdu, tlv(tagSequence, vbl)...)
	body := tlv(tagInteger, encodeInt(m.version))
	body = append(body, tlv(tagOctetString, m.community)...)
	body = append(body, tlv(m.pdu, pdu)...)
	return tlv(tagSequence, body)
}

// sizeWith is the encoded size of m if its varbind list held vbl octets of varbinds.
func (m *message) sizeWith(vbl int) int {
	pdu := tlvLen(len(encodeInt(m.requestID))) + tlvLen(len(encodeInt(m.errStatus))) +
		tlvLen(len(encodeInt(m.errIndex))) + tlvLen(vbl)
	body := tlvLen(len(encodeInt(m.version))) + tlvLen(len(m.community)) + tlvLen(pdu)
	return tlvLen(body)
}

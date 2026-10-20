package ipmi

// The wire formats of IPMI over LAN (IPMI v2.0 rev 1.1, sections 13 and 22): the RMCP header,
// the ASF presence ping, the IPMI v1.5 session wrapper (only ever sessionless here), the RMCP+
// session header with its integrity trailer and confidentiality envelope, and the IPMI LAN
// message inside them. Everything multi-byte in a session header is least significant byte
// first; the ASF fields are network order.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
)

const (
	rmcpVersion   = 0x06
	rmcpNoAck     = 0xFF
	classASF      = 0x06
	classIPMI     = 0x07
	asfIANA       = 4542 // 0x000011BE, ASF's enterprise number
	asfPing       = 0x80
	asfPong       = 0x40
	authTypeNone  = 0x00
	authTypeRMCPP = 0x06

	// RMCP+ payload types (13.27.3).
	payloadIPMI        = 0x00
	payloadSOL         = 0x01
	payloadOEM         = 0x02
	payloadOpenReq     = 0x10
	payloadOpenResp    = 0x11
	payloadRAKP1       = 0x12
	payloadRAKP2       = 0x13
	payloadRAKP3       = 0x14
	payloadRAKP4       = 0x15
	payloadEncrypted   = 0x80
	payloadAuthentic   = 0x40
	payloadTypeMask    = 0x3F
	nextHeaderRMCP     = 0x07
	bmcSlaveAddr       = 0x20
	integrityPadByte   = 0xFF
	sessionHeaderBytes = 12 // auth type, payload type, session id, sequence, length

	// Algorithms (13.28).
	authNone         = 0x00
	authHMACSHA1     = 0x01
	authHMACSHA256   = 0x03
	integNone        = 0x00
	integHMACSHA1    = 0x01 // HMAC-SHA1-96
	integHMACSHA256  = 0x04 // HMAC-SHA256-128
	confNone         = 0x00
	confAESCBC128    = 0x01
	kuidBytes        = 20 // a user key is the password, zero padded to 20 bytes (13.31)
	maxUserNameBytes = 16
)

// RMCP+ status codes (Table 13-15).
const (
	rmcpOK                 = 0x00
	rmcpNoResources        = 0x01
	rmcpInvalidSessionID   = 0x02
	rmcpInvalidRole        = 0x09
	rmcpUnauthorizedRole   = 0x0A
	rmcpInvalidNameLength  = 0x0C
	rmcpUnauthorizedName   = 0x0D
	rmcpInvalidIntegrity   = 0x0F
	rmcpNoCipherSuiteMatch = 0x11
	rmcpIllegalParameter   = 0x12
)

// cipherSuite is one supported RMCP+ cipher suite (Table 22-20).
type cipherSuite struct {
	id                uint8
	auth, integ, conf uint8
}

// suites are the cipher suites this BMC offers: 3 and 17, the two every current BMC and
// `ipmitool -I lanplus` agree on. Suite 0 (no authentication) is deliberately absent, as it is on
// any BMC that is not misconfigured.
var suites = []cipherSuite{
	{id: 3, auth: authHMACSHA1, integ: integHMACSHA1, conf: confAESCBC128},
	{id: 17, auth: authHMACSHA256, integ: integHMACSHA256, conf: confAESCBC128},
}

func suiteFor(auth, integ, conf uint8) (cipherSuite, bool) {
	for _, s := range suites {
		if s.auth == auth && s.integ == integ && s.conf == conf {
			return s, true
		}
	}
	return cipherSuite{}, false
}

var errShort = errors.New("short packet")

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func put16(v uint16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, v)
	return b
}
func put32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func rmcpHeader(class byte) []byte { return []byte{rmcpVersion, 0x00, rmcpNoAck, class} }

// ---------------------------------------------------------------- ASF

// asfPongFor answers an ASF presence ping (ASF 2.0 3.2.4.3): IPMI is supported.
func asfPongFor(body []byte) []byte {
	if len(body) < 8 || binary.BigEndian.Uint32(body[0:4]) != asfIANA || body[4] != asfPing {
		return nil
	}
	tag := body[5]
	out := rmcpHeader(classASF)
	out = binary.BigEndian.AppendUint32(out, asfIANA)
	out = append(out, asfPong, tag, 0x00, 0x10)
	out = binary.BigEndian.AppendUint32(out, asfIANA) // IANA enterprise number of the entity
	out = append(out, 0, 0, 0, 0)                     // OEM defined
	out = append(out, 0x81)                           // supported entities: IPMI, ASF 1.0
	out = append(out, 0x00)                           // supported interactions
	out = append(out, 0, 0, 0, 0, 0, 0)               // reserved
	return out
}

// ---------------------------------------------------------------- IPMI LAN messages

// request is an IPMI LAN request message (13.8).
type request struct {
	rsAddr, netFn, rsLUN, rqAddr, rqSeq, rqLUN, cmd byte
	data                                            []byte
}

func checksum(b []byte) byte {
	var s byte
	for _, x := range b {
		s += x
	}
	return -s
}

func parseRequest(m []byte) (request, error) {
	if len(m) < 7 {
		return request{}, errShort
	}
	if checksum(m[0:2]) != m[2] || checksum(m[3:len(m)-1]) != m[len(m)-1] {
		return request{}, errors.New("bad checksum")
	}
	return request{rsAddr: m[0], netFn: m[1] >> 2, rsLUN: m[1] & 3, rqAddr: m[3], rqSeq: m[4] >> 2, rqLUN: m[4] & 3,
		cmd: m[5], data: append([]byte(nil), m[6:len(m)-1]...)}, nil
}

// response encodes the answer to r with a completion code.
func (r request) response(cc byte, data []byte) []byte {
	m := []byte{r.rqAddr, (r.netFn|1)<<2 | r.rqLUN, 0, r.rsAddr, r.rqSeq<<2 | r.rsLUN, r.cmd, cc}
	m[2] = checksum(m[0:2])
	m = append(m, data...)
	return append(m, checksum(m[3:]))
}

// ---------------------------------------------------------------- IPMI v1.5 session wrapper

// v15Message unwraps a sessionless IPMI v1.5 packet body (after the RMCP header).
func v15Message(body []byte) ([]byte, bool) {
	if len(body) < 10 || body[0] != authTypeNone || le32(body[5:9]) != 0 {
		return nil, false // authenticated IPMI v1.5 sessions are not offered
	}
	n := int(body[9])
	if len(body) < 10+n {
		return nil, false
	}
	return body[10 : 10+n], true
}

func v15Packet(msg []byte) []byte {
	out := rmcpHeader(classIPMI)
	out = append(out, authTypeNone, 0, 0, 0, 0, 0, 0, 0, 0, byte(len(msg)))
	return append(out, msg...)
}

// ---------------------------------------------------------------- RMCP+

// v2Header is a parsed RMCP+ session header.
type v2Header struct {
	ptype     byte // with the encrypted and authenticated bits
	sessionID uint32
	seq       uint32
	payload   []byte
	// covered is the integrity-protected range (auth type .. next header) and authCode the
	// trailing AuthCode, when the payload is authenticated.
	covered, authCode []byte
}

func (h v2Header) kind() byte      { return h.ptype & payloadTypeMask }
func (h v2Header) encrypted() bool { return h.ptype&payloadEncrypted != 0 }
func (h v2Header) authentic() bool { return h.ptype&payloadAuthentic != 0 }

// parseV2 reads an RMCP+ packet body (after the RMCP header). authLen is the AuthCode length
// the session negotiated, needed to find the trailer of an authenticated packet.
func parseV2(body []byte, authLen func(sessionID uint32) int) (v2Header, error) {
	if len(body) < sessionHeaderBytes || body[0] != authTypeRMCPP {
		return v2Header{}, errShort
	}
	h := v2Header{ptype: body[1]}
	off := 2
	if h.kind() == payloadOEM {
		return v2Header{}, errors.New("OEM explicit payloads are not supported")
	}
	h.sessionID, h.seq = le32(body[off:]), le32(body[off+4:])
	n := int(le16(body[off+8:]))
	off += 10
	if len(body) < off+n {
		return v2Header{}, errShort
	}
	h.payload = body[off : off+n]
	if h.authentic() {
		al := authLen(h.sessionID)
		if al == 0 || len(body) < off+n+2+al {
			return v2Header{}, errShort
		}
		h.covered, h.authCode = body[:len(body)-al], body[len(body)-al:]
		if h.covered[len(h.covered)-1] != nextHeaderRMCP {
			return v2Header{}, errors.New("bad next header")
		}
	}
	return h, nil
}

// v2Packet builds an RMCP+ packet. With keys, the payload is encrypted with AES-CBC-128 and
// the packet carries an integrity trailer.
func v2Packet(ptype byte, sessionID, seq uint32, payload []byte, k *sessionKeys) []byte {
	if k != nil && k.conf == confAESCBC128 {
		payload = k.encrypt(payload)
		ptype |= payloadEncrypted
	}
	if k != nil && k.integ != integNone {
		ptype |= payloadAuthentic
	}
	body := []byte{authTypeRMCPP, ptype}
	body = append(body, put32(sessionID)...)
	body = append(body, put32(seq)...)
	body = append(body, put16(uint16(len(payload)))...)
	body = append(body, payload...)
	if k != nil && k.integ != integNone {
		pad := (4 - (len(body)+2)%4) % 4
		for i := 0; i < pad; i++ {
			body = append(body, integrityPadByte)
		}
		body = append(body, byte(pad), nextHeaderRMCP)
		body = append(body, k.authCode(body)...)
	}
	return append(rmcpHeader(classIPMI), body...)
}

// ---------------------------------------------------------------- keys and algorithms

func hashFor(auth uint8) func() hash.Hash {
	if auth == authHMACSHA256 {
		return sha256.New
	}
	return sha1.New
}

func hmacOf(h func() hash.Hash, key []byte, parts ...[]byte) []byte {
	m := hmac.New(h, key)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

// rakpICVLen is the length of RAKP message 4's integrity check value: the authentication
// algorithm's HMAC truncated as its integrity counterpart is (HMAC-SHA1-96, HMAC-SHA256-128).
func rakpICVLen(auth uint8) int {
	if auth == authHMACSHA256 {
		return 16
	}
	return 12
}

// sessionKeys are the keys an active session derived from its SIK (13.32).
type sessionKeys struct {
	auth, integ, conf uint8
	sik, k1, k2       []byte
}

func deriveKeys(auth, integ, conf uint8, sik []byte) *sessionKeys {
	h := hashFor(auth)
	c1, c2 := make([]byte, 20), make([]byte, 20)
	for i := range c1 {
		c1[i], c2[i] = 0x01, 0x02
	}
	return &sessionKeys{auth: auth, integ: integ, conf: conf, sik: sik, k1: hmacOf(h, sik, c1), k2: hmacOf(h, sik, c2)}
}

func (k *sessionKeys) authLen() int {
	switch k.integ {
	case integHMACSHA1:
		return 12
	case integHMACSHA256:
		return 16
	}
	return 0
}

func (k *sessionKeys) authCode(covered []byte) []byte {
	h := sha1.New
	if k.integ == integHMACSHA256 {
		h = sha256.New
	}
	return hmacOf(h, k.k1, covered)[:k.authLen()]
}

func (k *sessionKeys) verify(h v2Header) bool {
	return h.authentic() && hmac.Equal(k.authCode(h.covered), h.authCode)
}

// encrypt is AES-CBC-128 with K2's first 16 bytes (13.29): a random IV, then the payload padded
// with 1, 2, 3 … and the pad length, to a multiple of the block size.
func (k *sessionKeys) encrypt(p []byte) []byte {
	block, _ := aes.NewCipher(k.k2[:16])
	pad := (aes.BlockSize - (len(p)+1)%aes.BlockSize) % aes.BlockSize
	plain := append([]byte(nil), p...)
	for i := 1; i <= pad; i++ {
		plain = append(plain, byte(i))
	}
	plain = append(plain, byte(pad))
	iv := randomBytes(aes.BlockSize)
	out := make([]byte, aes.BlockSize+len(plain))
	copy(out, iv)
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out[aes.BlockSize:], plain)
	return out
}

func (k *sessionKeys) decrypt(c []byte) ([]byte, error) {
	if len(c) < 2*aes.BlockSize || len(c)%aes.BlockSize != 0 {
		return nil, errors.New("bad ciphertext length")
	}
	block, _ := aes.NewCipher(k.k2[:16])
	plain := make([]byte, len(c)-aes.BlockSize)
	cipher.NewCBCDecrypter(block, c[:aes.BlockSize]).CryptBlocks(plain, c[aes.BlockSize:])
	pad := int(plain[len(plain)-1])
	if pad >= aes.BlockSize || pad+1 > len(plain) {
		return nil, errors.New("bad confidentiality pad")
	}
	for i := 0; i < pad; i++ {
		if plain[len(plain)-1-pad+i] != byte(i+1) {
			return nil, errors.New("bad confidentiality pad")
		}
	}
	return plain[:len(plain)-1-pad], nil
}

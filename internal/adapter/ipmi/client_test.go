package ipmi

// A minimal RMCP+ console for the tests that must run where ipmitool is absent. It is written
// from the specification, not from the server's code paths, except for the shared primitives
// (HMAC, AES-CBC, packet framing); ipmitool_test.go is the independent oracle.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const (
	dell    = "site-1/hall-a/b01/server-01"
	gb300   = "site-1/hall-a/a01/compute-tray-01"
	generic = "site-1/hall-a/c02/server-01"
)

// lab brings the lab estate up with only this adapter.
func lab(t *testing.T) (*twin.Twin, *Adapter) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../../examples/estates/lab-model.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, "", reg, twin.Options{Adapters: []string{"ipmi"}, StateDir: t.TempDir(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	return tw, tw.Adapter("ipmi").(*Adapter)
}

type creds struct{ user, pass string }

func credsOf(tw *twin.Twin, path string) creds {
	var c creds
	tw.Engine.Read(func(m *model.Model) {
		d := m.DeviceBy[path]
		c = creds{d.Identity.BMCUser, d.Identity.BMCPassword}
	})
	return c
}

func view(tw *twin.Twin, path string) map[string]any {
	var d *model.Device
	tw.Engine.Read(func(m *model.Model) { d = m.DeviceBy[path] })
	return tw.Engine.DeviceView(d)
}

// requests returns the committed IPMI request events for a device and canonical name.
func requests(tw *twin.Twin, path, canonical string) []schema.Event {
	return tw.Engine.Events(0, func(ev schema.Event) bool {
		return ev.Path == path && ev.Request != nil && ev.Request.Protocol == "ipmi" && ev.Request.Canonical == canonical
	})
}

func waitFor(t *testing.T, within time.Duration, what string, ok func() bool) {
	t.Helper()
	end := time.Now().Add(within)
	for time.Now().Before(end) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("not within %s: %s", within, what)
}

// ---------------------------------------------------------------- console

type console struct {
	conn      *net.UDPConn
	consoleID uint32
	bmcID     uint32
	keys      *sessionKeys
	seq       uint32
	rqSeq     byte
	guid      []byte
	lastSent  []byte
}

func dial(t *testing.T, addr string) *console {
	t.Helper()
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.DialUDP("udp", nil, ua)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &console{conn: c, consoleID: 0xA0A1A2A3}
}

var errTimeout = errors.New("no answer")

func (c *console) roundTrip(pkt []byte) ([]byte, error) {
	c.lastSent = pkt
	if _, err := c.conn.Write(pkt); err != nil {
		return nil, err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	buf := make([]byte, 4096)
	n, err := c.conn.Read(buf)
	if err != nil {
		return nil, errTimeout
	}
	return buf[:n], nil
}

func (c *console) ping() error {
	p := rmcpHeader(classASF)
	p = append(p, 0x00, 0x00, 0x11, 0xBE, asfPing, 0x17, 0x00, 0x00)
	r, err := c.roundTrip(p)
	if err != nil {
		return err
	}
	if len(r) < 12 || r[3] != classASF || r[8] != asfPong || r[9] != 0x17 || r[20]&0x80 == 0 {
		return fmt.Errorf("bad pong % x", r)
	}
	return nil
}

func (c *console) v2(r []byte) (v2Header, error) {
	if len(r) < 4 || r[0] != rmcpVersion || r[3] != classIPMI {
		return v2Header{}, errShort
	}
	return parseV2(r[4:], func(uint32) int {
		if c.keys == nil {
			return 0
		}
		return c.keys.authLen()
	})
}

// rakpError is an RMCP+ status a step of session set-up returned.
type rakpError struct {
	step   string
	status byte
}

func (e rakpError) Error() string { return fmt.Sprintf("%s: RMCP+ status 0x%02x", e.step, e.status) }

// open establishes a session; role is the privilege requested in RAKP 1.
func (c *console) open(user, pass string, suite cipherSuite, role byte) error {
	req := []byte{0x01, role, 0, 0}
	req = append(req, put32(c.consoleID)...)
	req = append(req, 0x00, 0, 0, 0x08, suite.auth, 0, 0, 0, 0x01, 0, 0, 0x08, suite.integ, 0, 0, 0, 0x02, 0, 0, 0x08, suite.conf, 0, 0, 0)
	r, err := c.roundTrip(v2Packet(payloadOpenReq, 0, 0, req, nil))
	if err != nil {
		return err
	}
	h, err := c.v2(r)
	if err != nil || h.kind() != payloadOpenResp {
		return fmt.Errorf("open session response: %v", err)
	}
	if h.payload[1] != rmcpOK {
		return rakpError{"open session", h.payload[1]}
	}
	c.bmcID = le32(h.payload[8:12])

	rm := randomBytes(16)
	roleByte := role | 0x10 // name-only lookup, as ipmitool sends
	r1 := []byte{0x02, 0, 0, 0}
	r1 = append(r1, put32(c.bmcID)...)
	r1 = append(r1, rm...)
	r1 = append(r1, roleByte, 0, 0, byte(len(user)))
	r1 = append(r1, user...)
	if r, err = c.roundTrip(v2Packet(payloadRAKP1, 0, 0, r1, nil)); err != nil {
		return err
	}
	if h, err = c.v2(r); err != nil || h.kind() != payloadRAKP2 {
		return fmt.Errorf("rakp 2: %v", err)
	}
	p := h.payload
	if p[1] != rmcpOK {
		return rakpError{"rakp 2", p[1]}
	}
	rc, guid, code := p[8:24], p[24:40], p[40:]
	c.guid = append([]byte(nil), guid...)
	kuid := make([]byte, kuidBytes)
	copy(kuid, pass)
	hf := hashFor(suite.auth)
	nameBytes := []byte{roleByte, byte(len(user))}
	wrong := !bytes.Equal(code, hmacOf(hf, kuid, put32(c.consoleID), put32(c.bmcID), rm, rc, guid, nameBytes, []byte(user)))

	r3 := []byte{0x03, 0, 0, 0}
	r3 = append(r3, put32(c.bmcID)...)
	r3 = append(r3, hmacOf(hf, kuid, rc, put32(c.consoleID), nameBytes, []byte(user))...)
	if r, err = c.roundTrip(v2Packet(payloadRAKP3, 0, 0, r3, nil)); err != nil {
		if wrong {
			return rakpError{"rakp 2 code (wrong password)", rmcpInvalidIntegrity}
		}
		return err
	}
	if h, err = c.v2(r); err != nil || h.kind() != payloadRAKP4 {
		return fmt.Errorf("rakp 4: %v", err)
	}
	if h.payload[1] != rmcpOK {
		return rakpError{"rakp 4", h.payload[1]}
	}
	if wrong {
		return errors.New("rakp 2 code did not verify but the BMC accepted RAKP 3")
	}
	sik := hmacOf(hf, kuid, rm, rc, nameBytes, []byte(user))
	icv := hmacOf(hf, sik, rm, put32(c.bmcID), guid)[:rakpICVLen(suite.auth)]
	if !bytes.Equal(icv, h.payload[8:]) {
		return errors.New("rakp 4 integrity check value did not verify")
	}
	c.keys = deriveKeys(suite.auth, suite.integ, suite.conf, sik)
	return nil
}

func (c *console) message(netFn, cmd byte, data []byte) []byte {
	c.rqSeq = (c.rqSeq + 1) & 0x3F
	m := []byte{bmcSlaveAddr, netFn << 2, 0, 0x81, c.rqSeq << 2, cmd}
	m[2] = checksum(m[0:2])
	m = append(m, data...)
	return append(m, checksum(m[3:]))
}

// sessionless sends a command outside a session (session id 0, RMCP+ framing).
func (c *console) sessionless(netFn, cmd byte, data ...byte) (byte, []byte, error) {
	r, err := c.roundTrip(v2Packet(payloadIPMI, 0, 0, c.message(netFn, cmd, data), nil))
	if err != nil {
		return 0, nil, err
	}
	h, err := c.v2(r)
	if err != nil {
		return 0, nil, err
	}
	return decodeResponse(h.payload, netFn, cmd)
}

// cmd sends a command in the session.
func (c *console) cmd(netFn, cmd byte, data ...byte) (byte, []byte, error) {
	c.seq++
	r, err := c.roundTrip(v2Packet(payloadIPMI, c.bmcID, c.seq, c.message(netFn, cmd, data), c.keys))
	if err != nil {
		return 0, nil, err
	}
	return c.decode(r, netFn, cmd)
}

func (c *console) decode(r []byte, netFn, cmd byte) (byte, []byte, error) {
	h, err := c.v2(r)
	if err != nil {
		return 0, nil, err
	}
	if h.sessionID != c.consoleID {
		return 0, nil, fmt.Errorf("answer for session %08x", h.sessionID)
	}
	if !c.keys.verify(h) || !h.encrypted() {
		return 0, nil, errors.New("answer not authenticated and encrypted")
	}
	plain, err := c.keys.decrypt(h.payload)
	if err != nil {
		return 0, nil, err
	}
	return decodeResponse(plain, netFn, cmd)
}

func decodeResponse(m []byte, netFn, cmd byte) (byte, []byte, error) {
	if len(m) < 8 || checksum(m[0:2]) != m[2] || checksum(m[3:len(m)-1]) != m[len(m)-1] {
		return 0, nil, fmt.Errorf("bad response % x", m)
	}
	if m[1]>>2 != netFn|1 || m[5] != cmd {
		return 0, nil, fmt.Errorf("response to netfn %02x cmd %02x", m[1]>>2, m[5])
	}
	return m[6], m[7 : len(m)-1], nil
}

func (c *console) mustCmd(t *testing.T, netFn, cmd byte, data ...byte) []byte {
	t.Helper()
	cc, out, err := c.cmd(netFn, cmd, data...)
	if err != nil {
		t.Fatalf("netfn %02x cmd %02x: %v", netFn, cmd, err)
	}
	if cc != ccOK {
		t.Fatalf("netfn %02x cmd %02x: completion code %02x", netFn, cmd, cc)
	}
	return out
}

func (c *console) close(t *testing.T) {
	t.Helper()
	c.mustCmd(t, netFnApp, cmdCloseSession, put32(c.bmcID)...)
}

// openAdmin opens an administrator session on a device's BMC.
func openAdmin(t *testing.T, tw *twin.Twin, a *Adapter, path string, suite cipherSuite) *console {
	t.Helper()
	cr := credsOf(tw, path)
	c := dial(t, a.Endpoint(path))
	if err := c.open(cr.user, cr.pass, suite, privAdmin); err != nil {
		t.Fatalf("%s: open session (suite %d): %v", path, suite.id, err)
	}
	return c
}

var stackSrc = schema.EventSource{Class: "user", Principal: "test"}

func addAccount(t *testing.T, tw *twin.Twin, path, user, pass, role string) {
	t.Helper()
	if _, err := tw.Engine.CreateAccount(engine.Request{Path: path, Canonical: "account.create", Source: stackSrc}, user, pass, role); err != nil {
		t.Fatal(err)
	}
}

func reqFor(path string) engine.Request {
	return engine.Request{Path: path, Canonical: "account.password", Source: stackSrc}
}

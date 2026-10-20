// Package ipmi is the IPMI-over-LAN adapter (docs/spec/05-interfaces.md — Management): an IPMI
// 2.0 BMC speaking RMCP and RMCP+, as `ipmitool -I lanplus` does, for every device whose profile
// sets bmc.ipmi. It projects the model (chassis power, boot flags, the SEL, sensors through the
// SDR repository, the FRU inventory) and translates requests into engine calls; it never changes
// model state itself. Session state (RAKP keys, sequence windows, reservations) is protocol state
// and lives here, as it lives in a real BMC's IPMI stack rather than in its Redfish service.
package ipmi

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

func init() { twin.Register("ipmi", func() adapter.Adapter { return New() }) }

const (
	// sessionIdle is how long an idle session lives (IPMI's customary 60 s inactivity timeout).
	sessionIdle = 60 * time.Second
	// setupIdle is how long a session that never completed RAKP holds a slot. A client that
	// finds RAKP 2's code wrong (a wrong password) just stops, so the BMC never hears of it.
	setupIdle = 10 * time.Second
	// maxSessions is the per-BMC session table size.
	maxSessions = 32
	protocol    = "ipmi"
)

// Adapter serves one IPMI LAN endpoint per IPMI-capable BMC.
type Adapter struct {
	mu   sync.Mutex
	rt   *adapter.Runtime
	e    *engine.Engine
	bmcs []*bmc
	// Endpoints maps a device path to the UDP address (host:port) its BMC answers IPMI on.
	Endpoints map[string]string
	noMfr     []string // dialects that carry no IPMI manufacturer id
	wg        sync.WaitGroup
	cancel    context.CancelFunc
}

// New returns an unstarted adapter.
func New() *Adapter { return &Adapter{Endpoints: map[string]string{}} }

func (a *Adapter) Name() string { return protocol }

// Endpoint is the UDP address a device's BMC serves IPMI on, or "".
func (a *Adapter) Endpoint(path string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Endpoints[path]
}

// Start opens an endpoint for every device whose profile sets bmc.ipmi.
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt, a.e = rt, rt.Engine
	type target struct {
		d          *model.Device
		serial     string
		uuid       string
		dialect    string
		bmcVersion string
	}
	var targets []target
	rt.Engine.Read(func(m *model.Model) {
		for _, d := range m.Devices {
			if !d.HasBMC() || !d.Profile.Spec.BMC.IPMI || d.Fidelity == "hardware" {
				continue
			}
			t := target{d: d, serial: d.Identity.Serial, uuid: d.Identity.UUID, dialect: d.Dialect}
			targets = append(targets, t)
		}
	})
	ctx, a.cancel = context.WithCancel(ctx)
	noMfr := map[string]bool{}
	for _, t := range targets {
		conn, err := rt.Net.ListenUDP(t.d.Path, protocol)
		if err != nil {
			a.Stop()
			return err
		}
		var mfr uint32
		if rt.Reg != nil {
			if dl, _ := rt.Reg.Dialect(t.dialect); dl != nil {
				mfr = uint32(dl.Spec.Identity.IPMIManufacturer) & 0x0FFFFF
			}
		}
		if mfr == 0 {
			noMfr[t.dialect] = true
		}
		b := &bmc{a: a, d: t.d, conn: conn, mfr: mfr, product: productID(t.d.ProfileName), guid: guidOf(t.uuid),
			sensors: sensorsFor(t.d.Profile), fru: fruImage(t.d.Profile.Spec.Identity, t.serial),
			sessions: map[uint32]*session{}, sdrRes: 1, selRes: 1}
		addr := conn.LocalAddr().String()
		a.mu.Lock()
		a.bmcs = append(a.bmcs, b)
		a.Endpoints[t.d.Path] = addr
		a.mu.Unlock()
		rt.Engine.SetEndpoint(t.d.Path, protocol, addr)
	}
	for k := range noMfr {
		a.noMfr = append(a.noMfr, k)
	}
	sort.Strings(a.noMfr)
	for _, b := range a.bmcs {
		a.wg.Add(1)
		go func(b *bmc) {
			defer a.wg.Done()
			b.serve(ctx)
		}(b)
	}
	if rt.Log != nil {
		rt.Log.Info("ipmi: serving", "bmcs", len(a.bmcs))
	}
	return nil
}

// Stop closes every endpoint and waits for the servers to return.
func (a *Adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Lock()
	for _, b := range a.bmcs {
		b.conn.Close()
	}
	a.mu.Unlock()
	a.wg.Wait()
	return nil
}

// Report is the adapter's fidelity-report contribution.
func (a *Adapter) Report() adapter.Report {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := adapter.Report{Adapter: protocol, Served: len(a.bmcs),
		Lines: []string{fmt.Sprintf("IPMI 2.0 over LAN (RMCP+, cipher suites 3 and 17) on %d BMCs, each its own loopback UDP port", len(a.bmcs))},
		IsNot: []string{
			"No IPMI v1.5 sessions (MD5, straight password): only RMCP+; cipher suite 0 is not offered.",
			"No Serial over LAN payload, no OEM payloads or OEM commands, no IPMB bridging, no PEF or alerting, no user or channel configuration commands.",
			"Sensors are 8-bit linear SDR readings of the model's derived values (a power sensor's step is about maxW/212 W); leak and discrete sensors are not in the SDR.",
			"The SEL is the model's log mapped to an IPMI event class by message id; the Redfish message text is not carried.",
			"The FRU board and product serial are the device serial; the manufacturing date is unspecified; the product id is a hash of the profile name, not a vendor's.",
			"Chassis power is binary: it reads on from the moment power is applied (PoweringOn) until the model reaches Off.",
			"A stack reaches IPMI only on the loopback port: the site gateway is HTTP CONNECT (TCP), so a BMC's site address does not route UDP.",
		}}
	if len(a.noMfr) > 0 {
		r.Lines = append(r.Lines, "manufacturer id 0 (unknown): dialects without identity.ipmiManufacturer: "+strings.Join(a.noMfr, ", "))
	}
	return r
}

// ---------------------------------------------------------------- one BMC

type bmc struct {
	a       *Adapter
	d       *model.Device
	conn    *net.UDPConn
	mfr     uint32
	product uint16
	guid    []byte
	sensors []*sensor
	fru     []byte

	// owned by the serve goroutine
	sessions map[uint32]*session
	sdrRes   uint16
	selRes   uint16
	requests uint64
	bmcSince time.Time
	// boot option parameter 3, BMC boot flag valid bit clearing: what a stack last set (volatile,
	// as on a BMC; the model's boot override has no 60-second expiry for it to suppress)
	bootValidClear byte
}

// productID is a stable 16-bit id per profile: no vendor product id is in the model.
func productID(profile string) uint16 {
	h := fnv.New32a()
	h.Write([]byte(profile))
	s := h.Sum32()
	return uint16(s>>16) ^ uint16(s)
}

// guidOf encodes the system UUID as IPMI and SMBIOS do: the first three fields least
// significant byte first, the rest as written.
func guidOf(u string) []byte {
	hex := strings.ReplaceAll(u, "-", "")
	raw := make([]byte, 16)
	for i := 0; i < 16 && 2*i+2 <= len(hex); i++ {
		v, _ := strconv.ParseUint(hex[2*i:2*i+2], 16, 8)
		raw[i] = byte(v)
	}
	g := append([]byte(nil), raw...)
	g[0], g[1], g[2], g[3] = raw[3], raw[2], raw[1], raw[0]
	g[4], g[5] = raw[5], raw[4]
	g[6], g[7] = raw[7], raw[6]
	return g
}

func (b *bmc) serve(ctx context.Context) {
	buf := make([]byte, 4096)
	for {
		n, from, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || strings.Contains(err.Error(), "closed") {
				return
			}
			continue
		}
		pkt := append([]byte(nil), buf[:n]...)
		up, latency, errPct := b.a.e.BMCBehaviour(b.d)
		if !up {
			// a BMC that is down answers nothing, and a reboot loses its sessions
			b.sessions = map[uint32]*session{}
			continue
		}
		b.checkReboot()
		if latency > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(latency):
			}
		}
		if out := b.handle(pkt, from, errPct); out != nil {
			_, _ = b.conn.WriteToUDP(out, from)
		}
	}
}

// checkReboot drops sessions when the BMC rebooted since the last packet.
func (b *bmc) checkReboot() {
	var since time.Time
	b.a.e.Read(func(*model.Model) { since = b.d.State.BMCSince })
	if !since.Equal(b.bmcSince) {
		b.bmcSince = since
		b.sessions = map[uint32]*session{}
	}
}

func (b *bmc) handle(p []byte, from *net.UDPAddr, errPct int) []byte {
	if len(p) < 5 || p[0] != rmcpVersion || p[3]&0x80 != 0 {
		return nil
	}
	body := p[4:]
	switch p[3] & 0x1F {
	case classASF:
		out := asfPongFor(body)
		if out != nil {
			b.log(from, "", "ASF Presence Ping", "asf ping", "other", 0)
		}
		return out
	case classIPMI:
		if body[0] == authTypeRMCPP {
			return b.rmcpPlus(body, from, errPct)
		}
		msg, ok := v15Message(body)
		if !ok {
			return nil
		}
		req, err := parseRequest(msg)
		if err != nil {
			return nil
		}
		cc, data := b.sessionless(req, from)
		return v15Packet(req.response(cc, data))
	}
	return nil
}

// sessionless answers the commands IPMI allows outside a session (22.13, 22.15, 22.14).
func (b *bmc) sessionless(r request, from *net.UDPAddr) (byte, []byte) {
	if r.netFn == netFnApp {
		switch r.cmd {
		case cmdGetChannelAuthCaps, cmdGetChannelCipherSuites, cmdGetSystemGUID:
			return b.command(nil, r, from, 0)
		}
	}
	b.log(from, "", commandName(r), target(r, ""), "other", ccInsufficientPrivilege)
	return ccInsufficientPrivilege, nil
}

func (b *bmc) authLen(id uint32) int {
	if s := b.sessions[id]; s != nil && s.keys != nil {
		return s.keys.authLen()
	}
	return 0
}

func (b *bmc) rmcpPlus(body []byte, from *net.UDPAddr, errPct int) []byte {
	h, err := parseV2(body, b.authLen)
	if err != nil {
		return nil
	}
	b.expire()
	switch h.kind() {
	case payloadOpenReq:
		return b.openSession(h, from)
	case payloadRAKP1:
		return b.rakp1(h, from)
	case payloadRAKP3:
		return b.rakp3(h, from)
	case payloadIPMI:
	default:
		return nil // SOL and other payloads are not offered
	}
	if h.sessionID == 0 {
		if h.encrypted() || h.authentic() {
			return nil
		}
		req, err := parseRequest(h.payload)
		if err != nil {
			return nil
		}
		cc, data := b.sessionless(req, from)
		return v2Packet(payloadIPMI, 0, 0, req.response(cc, data), nil)
	}
	s := b.sessions[h.sessionID]
	if s == nil || s.state != stateActive {
		return nil
	}
	// a session that negotiated integrity and confidentiality accepts nothing less
	if s.keys.integ != integNone && !s.keys.verify(h) {
		return nil
	}
	if s.keys.conf != confNone && !h.encrypted() {
		return nil
	}
	if cached, dup, ok := s.accept(h.seq); !ok {
		return nil
	} else if dup {
		return cached
	}
	payload := h.payload
	if h.encrypted() {
		if payload, err = s.keys.decrypt(payload); err != nil {
			return nil
		}
	}
	req, err := parseRequest(payload)
	if err != nil {
		return nil
	}
	s.last = time.Now()
	cc, data := b.command(s, req, from, errPct)
	s.outSeq++
	if s.outSeq == 0 {
		s.outSeq = 1
	}
	out := v2Packet(payloadIPMI, s.consoleID, s.outSeq, req.response(cc, data), s.keys)
	s.remember(h.seq, out)
	if s.closing {
		delete(b.sessions, s.bmcID)
	}
	return out
}

// expire drops idle sessions and abandoned session set-ups.
func (b *bmc) expire() {
	now := time.Now()
	for id, s := range b.sessions {
		idle := sessionIdle
		if s.state != stateActive {
			idle = setupIdle
		}
		if now.Sub(s.last) > idle {
			delete(b.sessions, id)
		}
	}
}

// log records a request in the engine's event log (14 — canonical request names). Requests the
// engine executes (power, boot, log clear) are logged by the engine itself.
func (b *bmc) log(from *net.UDPAddr, user, method, tgt, canonical string, status byte) {
	b.a.e.LogRead(b.req(from, user, method, tgt, canonical), int(status))
}

func (b *bmc) req(from *net.UDPAddr, user, method, tgt, canonical string) engine.Request {
	src := schema.EventSource{Class: "stack", Principal: user}
	if from != nil {
		src.Address = from.String()
	}
	return engine.Request{Path: b.d.Path, Canonical: canonical, Source: src, Protocol: protocol, Method: method, Target: tgt}
}

// ---------------------------------------------------------------- sessions

const (
	stateOpen = iota
	stateRAKP2
	stateActive
)

type session struct {
	consoleID, bmcID uint32
	suite            cipherSuite
	maxPriv, priv    byte
	state            int
	rm, rc           []byte
	role             byte // RAKP 1's requested role byte, as sent
	user, password   string
	kuid             []byte
	keys             *sessionKeys
	inSeq, inMask    uint32
	outSeq           uint32
	last             time.Time
	closing          bool
	replies          map[uint32][]byte
	replyOrder       []uint32
}

// accept applies the inbound sequence window (13.12): a sliding window of 32 below the highest
// number seen. A repeat returns the cached reply, so a retransmission is answered, not re-run.
func (s *session) accept(seq uint32) (cached []byte, dup, ok bool) {
	if seq == 0 {
		return nil, false, false
	}
	switch {
	case s.inSeq == 0 || seq > s.inSeq:
		shift := seq - s.inSeq
		if s.inSeq == 0 || shift >= 32 {
			s.inMask = 0
		} else {
			s.inMask <<= shift
		}
		s.inMask |= 1
		s.inSeq = seq
		return nil, false, true
	default:
		d := s.inSeq - seq
		if d >= 32 {
			return nil, false, false
		}
		if s.inMask&(1<<d) != 0 {
			r, ok := s.replies[seq]
			return r, true, ok
		}
		s.inMask |= 1 << d
		return nil, false, true
	}
}

func (s *session) remember(seq uint32, out []byte) {
	if s.replies == nil {
		s.replies = map[uint32][]byte{}
	}
	s.replies[seq] = out
	s.replyOrder = append(s.replyOrder, seq)
	if len(s.replyOrder) > 8 {
		delete(s.replies, s.replyOrder[0])
		s.replyOrder = s.replyOrder[1:]
	}
}

func (b *bmc) newSessionID() uint32 {
	for {
		id := le32(randomBytes(4))
		if id != 0 && b.sessions[id] == nil {
			return id
		}
	}
}

// openSession answers an RMCP+ Open Session Request (13.17).
func (b *bmc) openSession(h v2Header, from *net.UDPAddr) []byte {
	p := h.payload
	if len(p) < 32 || h.sessionID != 0 {
		return nil
	}
	tag, reqPriv, consoleID := p[0], p[1]&0x0F, le32(p[4:8])
	fail := func(status byte) []byte {
		b.log(from, "", "RMCP+ Open Session", "open session", "session.create", status)
		out := []byte{tag, status, 0, 0}
		out = append(out, put32(consoleID)...)
		return v2Packet(payloadOpenResp, 0, 0, out, nil)
	}
	if consoleID == 0 {
		return fail(rmcpInvalidSessionID)
	}
	if p[8] != 0x00 || p[16] != 0x01 || p[24] != 0x02 {
		return fail(rmcpIllegalParameter)
	}
	suite, ok := suiteFor(p[12]&0x3F, p[20]&0x3F, p[28]&0x3F)
	if !ok {
		return fail(rmcpNoCipherSuiteMatch)
	}
	if reqPriv > privAdmin {
		return fail(rmcpInvalidRole)
	}
	if len(b.sessions) >= maxSessions {
		return fail(rmcpNoResources)
	}
	if reqPriv == 0 {
		reqPriv = privAdmin
	}
	s := &session{consoleID: consoleID, bmcID: b.newSessionID(), suite: suite, maxPriv: reqPriv, state: stateOpen, last: time.Now()}
	b.sessions[s.bmcID] = s
	out := []byte{tag, rmcpOK, reqPriv, 0}
	out = append(out, put32(consoleID)...)
	out = append(out, put32(s.bmcID)...)
	out = append(out, 0x00, 0, 0, 0x08, suite.auth, 0, 0, 0)
	out = append(out, 0x01, 0, 0, 0x08, suite.integ, 0, 0, 0)
	out = append(out, 0x02, 0, 0, 0x08, suite.conf, 0, 0, 0)
	return v2Packet(payloadOpenResp, 0, 0, out, nil)
}

// rakp1 answers RAKP Message 1 with Message 2 (13.20, 13.21): the user is looked up by name in
// the model's BMC accounts, and the key exchange code proves this BMC knows the password.
func (b *bmc) rakp1(h v2Header, from *net.UDPAddr) []byte {
	p := h.payload
	if len(p) < 28 {
		return nil
	}
	tag, id := p[0], le32(p[4:8])
	s := b.sessions[id]
	fail := func(status byte, user string) []byte {
		b.log(from, user, "RMCP+ RAKP 1", "rakp", "session.create", status)
		var console uint32
		if s != nil {
			console = s.consoleID
			delete(b.sessions, id)
		}
		out := []byte{tag, status, 0, 0}
		out = append(out, put32(console)...)
		return v2Packet(payloadRAKP2, 0, 0, out, nil)
	}
	if s == nil || s.state == stateActive {
		return fail(rmcpInvalidSessionID, "")
	}
	role, n := p[24], int(p[27])
	if n > maxUserNameBytes || len(p) < 28+n {
		return fail(rmcpInvalidNameLength, "")
	}
	user := string(p[28 : 28+n])
	want := role & 0x0F
	if want == 0 || want > privAdmin {
		return fail(rmcpInvalidRole, user)
	}
	acct, ok := b.account(user)
	if !ok || user == "" {
		return fail(rmcpUnauthorizedName, user)
	}
	if want > privilegeOf(acct.Role) || want > s.maxPriv {
		return fail(rmcpUnauthorizedRole, user)
	}
	s.rm, s.rc, s.role, s.user, s.password = append([]byte(nil), p[8:24]...), randomBytes(16), role, user, acct.Password
	s.kuid = make([]byte, kuidBytes)
	copy(s.kuid, acct.Password)
	s.state = stateRAKP2
	s.last = time.Now()
	code := hmacOf(hashFor(s.suite.auth), s.kuid, put32(s.consoleID), put32(s.bmcID), s.rm, s.rc, b.guid,
		[]byte{s.role, byte(len(user))}, []byte(user))
	out := []byte{tag, rmcpOK, 0, 0}
	out = append(out, put32(s.consoleID)...)
	out = append(out, s.rc...)
	out = append(out, b.guid...)
	out = append(out, code...)
	return v2Packet(payloadRAKP2, 0, 0, out, nil)
}

// rakp3 checks RAKP Message 3 and answers with Message 4 (13.22, 13.23); on success the session
// is active with keys derived from the SIK.
func (b *bmc) rakp3(h v2Header, from *net.UDPAddr) []byte {
	p := h.payload
	if len(p) < 8 {
		return nil
	}
	tag, status, id := p[0], p[1], le32(p[4:8])
	s := b.sessions[id]
	if s == nil || s.state != stateRAKP2 {
		return nil
	}
	reply := func(status byte, icv []byte) []byte {
		out := []byte{tag, status, 0, 0}
		out = append(out, put32(s.consoleID)...)
		return v2Packet(payloadRAKP4, 0, 0, append(out, icv...), nil)
	}
	if status != rmcpOK {
		// the console refused RAKP 2: it holds a different password for this user
		delete(b.sessions, id)
		b.log(from, s.user, "RMCP+ RAKP 3", "rakp", "session.create", status)
		return nil
	}
	h1 := hashFor(s.suite.auth)
	want := hmacOf(h1, s.kuid, s.rc, put32(s.consoleID), []byte{s.role, byte(len(s.user))}, []byte(s.user))
	if !bytes.Equal(want, p[8:]) {
		delete(b.sessions, id)
		b.log(from, s.user, "RMCP+ RAKP 3", "rakp", "session.create", rmcpInvalidIntegrity)
		return reply(rmcpInvalidIntegrity, nil)
	}
	// the engine makes the authentication decision; the HMAC proves the console holds the key
	acct, ok := b.a.e.Authenticate(b.d, s.user, s.password)
	if !ok {
		delete(b.sessions, id)
		b.log(from, s.user, "RMCP+ RAKP 3", "rakp", "session.create", rmcpUnauthorizedName)
		return reply(rmcpUnauthorizedName, nil)
	}
	sik := hmacOf(h1, s.kuid, s.rm, s.rc, []byte{s.role, byte(len(s.user))}, []byte(s.user))
	s.keys = deriveKeys(s.suite.auth, s.suite.integ, s.suite.conf, sik)
	icv := hmacOf(h1, sik, s.rm, put32(s.bmcID), b.guid)[:rakpICVLen(s.suite.auth)]
	s.maxPriv = minByte(s.role&0x0F, privilegeOf(acct.Role))
	s.priv = s.maxPriv
	s.state = stateActive
	s.last = time.Now()
	b.log(from, s.user, "RMCP+ RAKP 3", fmt.Sprintf("cipher suite %d", s.suite.id), "session.create", rmcpOK)
	return reply(rmcpOK, icv)
}

func minByte(a, b byte) byte {
	if a < b {
		return a
	}
	return b
}

// account finds an enabled BMC account by name in the model.
func (b *bmc) account(user string) (model.Account, bool) {
	var out model.Account
	ok := false
	b.a.e.Read(func(*model.Model) {
		for _, a := range b.d.State.Accounts {
			if a.User == user && a.Enabled && !a.Locked {
				out, ok = a, true
			}
		}
	})
	return out, ok
}

// IPMI privilege levels.
const (
	privCallback = 1
	privUser     = 2
	privOperator = 3
	privAdmin    = 4
)

// privilegeOf maps a model account role (Redfish's role ids, or a capture's) to IPMI.
func privilegeOf(role string) byte {
	r := strings.ToLower(role)
	switch {
	case strings.Contains(r, "admin"):
		return privAdmin
	case strings.Contains(r, "operator"):
		return privOperator
	case strings.Contains(r, "callback"):
		return privCallback
	}
	return privUser
}

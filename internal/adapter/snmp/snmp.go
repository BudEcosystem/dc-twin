// Package snmp is the facility adapter for PDUs over SNMP (docs/spec/05-interfaces.md —
// Facility): one SNMPv1/v2c agent per device whose profile has `spec.snmp`, on its own netsim
// UDP endpoint, serving the MIB-II system group and the objects of the profile's MIB dialect.
// Reads project committed state (relay states, faults, the PDU's derived output power and
// current); an outlet command is translated into engine.SetOutlet and never changes state here.
package snmp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

func init() { twin.Register("snmp", func() adapter.Adapter { return New() }) }

// Config holds the agent's twin-side knobs.
type Config struct {
	// ReadCommunity may read; WriteCommunity may read and write. Any other community is
	// dropped without a response, as RFC 3584 agents do.
	ReadCommunity  string
	WriteCommunity string
	// Contact is sysContact.0.
	Contact string
	// MaxMessageSize bounds a response: a GET that would exceed it answers tooBig, a GETBULK
	// is truncated to fit.
	MaxMessageSize int
	// RebootDuration is how long immediateReboot holds an outlet off, before the estate's
	// time scale. PowerNet's default outlet reboot duration is 5 s.
	RebootDuration time.Duration
}

// DefaultConfig is net-snmp's conventional pair of communities.
func DefaultConfig() Config {
	return Config{ReadCommunity: "public", WriteCommunity: "private", Contact: "dc-twin",
		MaxMessageSize: 1472, RebootDuration: 5 * time.Second}
}

// Adapter serves every SNMP PDU of an estate.
type Adapter struct {
	cfg       Config
	rt        *adapter.Runtime
	mu        sync.Mutex
	agents    []*agent
	endpoints map[string]string
	dialects  map[string]*binding
	stop      chan struct{}
	stopOnce  sync.Once
	wg        sync.WaitGroup
}

// New returns an adapter with DefaultConfig.
func New() *Adapter { return NewWithConfig(DefaultConfig()) }

// NewWithConfig returns an adapter with the given configuration; zero fields take defaults.
func NewWithConfig(c Config) *Adapter {
	d := DefaultConfig()
	if c.ReadCommunity == "" {
		c.ReadCommunity = d.ReadCommunity
	}
	if c.WriteCommunity == "" {
		c.WriteCommunity = d.WriteCommunity
	}
	if c.Contact == "" {
		c.Contact = d.Contact
	}
	if c.MaxMessageSize <= 0 {
		c.MaxMessageSize = d.MaxMessageSize
	}
	if c.RebootDuration <= 0 {
		c.RebootDuration = d.RebootDuration
	}
	return &Adapter{cfg: c, endpoints: map[string]string{}, dialects: map[string]*binding{}, stop: make(chan struct{})}
}

func (a *Adapter) Name() string { return "snmp" }

// Endpoint is the host:port a device's SNMP agent listens on, or "" when it has none. The
// engine's device view reads model.Endpoint.SNMP, which only the engine may set; until it
// offers a call for that, this is where the endpoint is published (and in netsim's
// endpoints.json, under kind "snmp").
func (a *Adapter) Endpoint(path string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.endpoints[path]
}

// Endpoints is a copy of every device path's SNMP endpoint.
func (a *Adapter) Endpoints() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]string, len(a.endpoints))
	for k, v := range a.endpoints {
		out[k] = v
	}
	return out
}

// Start opens one agent per SNMP device.
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt = rt
	type plan struct {
		d       *model.Device
		mib     string
		outlets int
		served  map[int]string
		rack    string
	}
	var plans []plan
	rt.Engine.Read(func(m *model.Model) {
		for _, d := range m.Devices {
			if d.Profile == nil || d.Profile.Spec.SNMP == nil {
				continue
			}
			p := plan{d: d, mib: d.Profile.Spec.SNMP.MIB, outlets: d.Profile.Spec.SNMP.Outlets, served: map[int]string{}}
			r := d.RackOf()
			if r != nil {
				p.rack = r.Path
				// served device i is on outlet i+1 of every PDU of the input (engine.recomputePower)
				for _, in := range r.Inputs {
					if in.Kind != "pdu" || !containsDevice(in.Sources, d) {
						continue
					}
					for i, s := range in.Serves {
						p.served[i+1] = s.Name
					}
				}
			}
			plans = append(plans, p)
		}
	})
	for _, p := range plans {
		b, err := a.binding(p.mib)
		if err != nil {
			a.Stop()
			return fmt.Errorf("%s: %w", p.d.Path, err)
		}
		conn, err := rt.Net.ListenUDP(p.d.Path, "snmp")
		if err != nil {
			a.Stop()
			return err
		}
		ag := &agent{a: a, dev: p.d, path: p.d.Path, b: b, outlets: p.outlets, served: p.served, rack: p.rack,
			conn: conn, pending: map[int]uint64{}}
		ag.powerSensor = sensorName(p.d, "output-power", "power")
		ag.currentSensor = sensorName(p.d, "output-current", "current")
		a.mu.Lock()
		a.agents = append(a.agents, ag)
		a.endpoints[p.d.Path] = conn.LocalAddr().String()
		a.mu.Unlock()
		a.wg.Add(1)
		go ag.serve()
	}
	go func() {
		select {
		case <-ctx.Done():
			a.Stop()
		case <-a.stop:
		}
	}()
	if rt.Log != nil {
		rt.Log.Info("snmp agents up", "devices", len(plans))
	}
	return nil
}

func (a *Adapter) binding(name string) (*binding, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b, ok := a.dialects[name]; ok {
		return b, nil
	}
	d := a.rt.Reg.MIBs[name]
	if d == nil {
		return nil, fmt.Errorf("MIB dialect %q is not in any loaded pack", name)
	}
	b, err := bind(name, d)
	if err != nil {
		return nil, err
	}
	a.dialects[name] = b
	return b, nil
}

func containsDevice(list []*model.Device, d *model.Device) bool {
	for _, x := range list {
		if x == d {
			return true
		}
	}
	return false
}

// sensorName finds the profile's sensor with a source, or else one with a name.
func sensorName(d *model.Device, source, name string) string {
	for _, s := range d.Profile.Spec.Sensors {
		if s.Source == source {
			return s.Name
		}
	}
	for _, s := range d.Profile.Spec.Sensors {
		if s.Name == name {
			return s.Name
		}
	}
	return ""
}

// Stop closes every agent and waits for pending outlet reboots to give up.
func (a *Adapter) Stop() error {
	a.stopOnce.Do(func() {
		close(a.stop)
		a.mu.Lock()
		for _, ag := range a.agents {
			ag.conn.Close()
		}
		a.mu.Unlock()
	})
	a.wg.Wait()
	return nil
}

// Report is the adapter's fidelity-report contribution.
func (a *Adapter) Report() adapter.Report {
	a.mu.Lock()
	defer a.mu.Unlock()
	rep := adapter.Report{Adapter: "snmp", Served: len(a.agents)}
	names := make([]string, 0, len(a.dialects))
	for n := range a.dialects {
		names = append(names, n)
	}
	sort.Strings(names)
	rep.Lines = append(rep.Lines,
		fmt.Sprintf("PDUs over SNMPv1/v2c: %d agents, MIB dialects %s", len(a.agents), strings.Join(names, ", ")),
		"communities: one read-only, one read-write; an unknown community is dropped without a response",
		"an agent answers only while its PDU's management controller is available; a BMC latency fault delays answers and an error fault answers genErr")
	for _, n := range names {
		b := a.dialects[n]
		rep.IsNot = append(rep.IsNot, b.spec.IsNot.All...)
		rep.IsNot = append(rep.IsNot, b.spec.IsNot.Model...)
		if len(b.unknown) > 0 {
			rep.IsNot = append(rep.IsNot, fmt.Sprintf("MIB dialect %s: objects %s have no binding in the snmp adapter and are not served", n, strings.Join(b.unknown, ", ")))
		}
	}
	rep.IsNot = append(rep.IsNot,
		"SNMPv1 and v2c only: SNMPv3 (USM), traps and informs are not implemented.",
		"Only the MIB-II system group and the dialect's bound objects are served; the system group is read-only.",
		fmt.Sprintf("Outlet commands are immediateOn, immediateOff and immediateReboot; delayed commands and cancelPendingCommand answer wrongValue. immediateReboot holds the outlet off for a fixed %s (behaviour-scaled), not a per-outlet reboot duration, and its second half is timed by the adapter against the engine clock.", a.cfg.RebootDuration),
		"Loopback UDP endpoints: the site gateway carries TCP only, so SNMP is not reachable at a PDU's site address.")
	return rep
}

// ---------------------------------------------------------------- one device's agent

type agent struct {
	a             *Adapter
	dev           *model.Device
	path          string
	b             *binding
	outlets       int
	served        map[int]string // outlet -> served device name
	rack          string
	conn          *net.UDPConn
	powerSensor   string
	currentSensor string

	// cmdMu serialises outlet commands with the completion of a pending reboot; pending is
	// the generation of each outlet's pending reboot, so a newer command cancels it.
	cmdMu   sync.Mutex
	pending map[int]uint64
	gen     uint64
}

func (ag *agent) serve() {
	defer ag.a.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, raddr, err := ag.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-ag.a.stop:
				return
			default:
				continue
			}
		}
		if out := ag.handle(append([]byte(nil), buf[:n]...), raddr); out != nil {
			_, _ = ag.conn.WriteToUDP(out, raddr)
		}
	}
}

func pduName(t byte) string {
	switch t {
	case pduGet:
		return "GET"
	case pduGetNext:
		return "GETNEXT"
	case pduGetBulk:
		return "GETBULK"
	case pduSet:
		return "SET"
	}
	return fmt.Sprintf("PDU-0x%02x", t)
}

// handle answers one datagram, or returns nil to drop it.
func (ag *agent) handle(pkt []byte, raddr *net.UDPAddr) []byte {
	e := ag.a.rt.Engine
	req, err := decodeMessage(pkt)
	if err != nil || (req.version != versionV1 && req.version != versionV2c) {
		return nil // unparseable, or SNMPv3: snmpInASNParseErrs / snmpInBadVersions
	}
	switch req.pdu {
	case pduGet, pduGetNext, pduSet:
	case pduGetBulk:
		if req.version == versionV1 {
			return nil // not an SNMPv1 PDU
		}
	default:
		return nil // responses, traps, informs and reports are not requests to an agent
	}
	up, latency, errPct := e.BMCBehaviour(ag.dev)
	if !up {
		return nil // the management card is down: the PDU does not answer
	}
	if latency > 0 {
		time.Sleep(latency)
	}
	community := string(req.community)
	src := schema.EventSource{Class: "stack", Principal: community, Address: raddr.String()}
	target := ""
	if len(req.vbs) > 0 {
		target = req.vbs[0].oid.String()
	}
	rw := community == ag.a.cfg.WriteCommunity
	if !rw && community != ag.a.cfg.ReadCommunity {
		e.Commit(schema.Event{Kind: "request", Source: schema.EventSource{Class: "stack", Address: raddr.String()}, Path: ag.path,
			Request: &schema.EventRequest{Protocol: "snmp", Method: pduName(req.pdu), Target: target, Canonical: "other", Status: errAuthorizationError},
			Message: "unknown SNMP community: dropped without a response"})
		return nil
	}
	if errPct > 0 && rand.IntN(100) < errPct {
		resp := ag.response(req, errGenErr, 1, req.vbs)
		e.LogRead(engine.Request{Path: ag.path, Canonical: "other", Source: src, Protocol: "snmp", Method: pduName(req.pdu), Target: target}, errGenErr)
		return ag.fit(req, resp)
	}
	var resp *message
	switch req.pdu {
	case pduSet:
		resp = ag.set(req, rw, src)
	default:
		var canonical string
		resp, canonical = ag.read(req)
		e.LogRead(engine.Request{Path: ag.path, Canonical: canonical, Source: src, Protocol: "snmp", Method: pduName(req.pdu), Target: target}, int(resp.errStatus))
	}
	return ag.fit(req, resp)
}

// response builds a Response-PDU, mapping the status for SNMPv1.
func (ag *agent) response(req *message, status, index int, vbs []varbind) *message {
	if req.version == versionV1 {
		status = v1Error(status)
	}
	if status == errNoError {
		index = 0
	}
	return &message{version: req.version, community: req.community, pdu: pduResponse, requestID: req.requestID,
		errStatus: int64(status), errIndex: int64(index), vbs: vbs}
}

// fit encodes a response, replacing it with tooBig when it exceeds the maximum message size
// (RFC 3416 §4.2.1: an empty varbind list; RFC 1157 §4.1.2: the request's, for SNMPv1).
func (ag *agent) fit(req, resp *message) []byte {
	out := resp.encode()
	if len(out) <= ag.a.cfg.MaxMessageSize {
		return out
	}
	var vbs []varbind
	if req.version == versionV1 {
		vbs = req.vbs
	}
	return ag.response(req, errTooBig, 0, vbs).encode()
}

// read answers GET, GETNEXT and GETBULK from one snapshot of the device.
func (ag *agent) read(req *message) (*message, string) {
	v := ag.snapshot()
	health := false
	note := func(en entry) {
		if en.read == "health.read" {
			health = true
		}
	}
	canonical := func() string {
		if health {
			return "health.read"
		}
		return "inventory.read"
	}
	out := make([]varbind, 0, len(req.vbs))
	switch req.pdu {
	case pduGet:
		for i, vb := range req.vbs {
			en, exc := v.get(vb.oid)
			if exc != 0 {
				if req.version == versionV1 {
					return ag.response(req, errNoSuchName, i+1, req.vbs), canonical()
				}
				out = append(out, varbind{oid: vb.oid, val: exception(exc)})
				continue
			}
			note(en)
			out = append(out, varbind{oid: en.oid, val: en.val})
		}
	case pduGetNext:
		for i, vb := range req.vbs {
			en, ok := v.next(vb.oid)
			if !ok {
				if req.version == versionV1 {
					return ag.response(req, errNoSuchName, i+1, req.vbs), canonical()
				}
				out = append(out, varbind{oid: vb.oid, val: exception(tagEndOfMibView)})
				continue
			}
			note(en)
			out = append(out, varbind{oid: en.oid, val: en.val})
		}
	case pduGetBulk:
		out = ag.bulk(req, v, note)
	}
	return ag.response(req, errNoError, 0, out), canonical()
}

// maxBulkVarbinds bounds a GETBULK before it is trimmed to the message size.
const maxBulkVarbinds = 2048

// bulk answers a GETBULK (RFC 3416 §4.2.3), stopping once every repeater has reached the end
// of the MIB view and trimming whole varbinds from the end to fit the message size.
func (ag *agent) bulk(req *message, v *view, note func(entry)) []varbind {
	n := int(min(max(req.errStatus, 0), int64(len(req.vbs))))
	m := int(min(max(req.errIndex, 0), maxBulkVarbinds))
	var out []varbind
	for _, vb := range req.vbs[:n] {
		if en, ok := v.next(vb.oid); ok {
			note(en)
			out = append(out, varbind{oid: en.oid, val: en.val})
		} else {
			out = append(out, varbind{oid: vb.oid, val: exception(tagEndOfMibView)})
		}
	}
	reps := req.vbs[n:]
	cur := make([]OID, len(reps))
	ended := make([]bool, len(reps))
	for j, vb := range reps {
		cur[j] = vb.oid
	}
	for r := 0; r < m && len(reps) > 0 && len(out) < maxBulkVarbinds; r++ {
		live := false
		for j := range reps {
			if !ended[j] {
				if en, ok := v.next(cur[j]); ok {
					note(en)
					cur[j] = en.oid
					out = append(out, varbind{oid: en.oid, val: en.val})
					live = true
					continue
				}
				ended[j] = true
			}
			out = append(out, varbind{oid: cur[j], val: exception(tagEndOfMibView)})
		}
		if !live {
			break
		}
	}
	// trim to the message size
	probe := &message{version: req.version, community: req.community, requestID: req.requestID}
	total, keep := 0, 0
	for i, vb := range out {
		total += len(encodeVarbind(vb))
		if probe.sizeWith(total) > ag.a.cfg.MaxMessageSize {
			break
		}
		keep = i + 1
	}
	return out[:keep]
}

// ---------------------------------------------------------------- SET

// set validates every varbind before applying any (RFC 3416 §4.2.5), then translates each
// outlet command into engine.SetOutlet.
func (ag *agent) set(req *message, rw bool, src schema.EventSource) *message {
	e := ag.a.rt.Engine
	refuse := func(status, index int) *message {
		r := engine.Request{Path: ag.path, Canonical: "other", Source: src, Protocol: "snmp", Method: "SET"}
		if index >= 1 && index <= len(req.vbs) {
			vb := req.vbs[index-1]
			r.Target = vb.oid.String()
			if _, ok := ag.b.outletCommandIndex(vb.oid); ok && vb.val.tag == tagInteger {
				r.Canonical = commandCanonical(vb.val.i)
			}
		}
		resp := ag.response(req, status, index, req.vbs)
		e.LogRead(r, int(resp.errStatus))
		return resp
	}
	if len(req.vbs) == 0 {
		return ag.response(req, errNoError, 0, nil)
	}
	if !rw {
		// the read-only community's write view is empty: net-snmp answers noAccess (v2c) and
		// noSuchName (v1) on the first varbind
		return refuse(errNoAccess, 1)
	}
	type op struct {
		oid    OID
		outlet int
		cmd    int64
	}
	var ops []op
	col, hasCmd := ag.b.columns[objOutletCommand]
	for i, vb := range req.vbs {
		if !hasCmd || !vb.oid.HasPrefix(col) || len(vb.oid) == len(col) {
			return refuse(errNotWritable, i+1)
		}
		if vb.val.tag != tagInteger {
			return refuse(errWrongType, i+1)
		}
		if vb.val.i < cmdImmediateOn || vb.val.i > cmdImmediateReboot {
			return refuse(errWrongValue, i+1)
		}
		idx, ok := ag.b.outletCommandIndex(vb.oid)
		if !ok || idx < 1 || idx > int64(ag.outlets) {
			return refuse(errNoCreation, i+1)
		}
		ops = append(ops, op{oid: vb.oid, outlet: int(idx), cmd: vb.val.i})
	}
	for i, o := range ops {
		r := engine.Request{Path: ag.path, Canonical: commandCanonical(o.cmd), Source: src, Protocol: "snmp", Method: "SET", Target: o.oid.String()}
		var err error
		switch o.cmd {
		case cmdImmediateOn, cmdImmediateOff:
			ag.cmdMu.Lock()
			ag.cancelPending(o.outlet)
			err = e.SetOutlet(r, o.outlet, o.cmd == cmdImmediateOn)
			ag.cmdMu.Unlock()
		case cmdImmediateReboot:
			err = ag.reboot(r, o.outlet)
		}
		if err != nil {
			// earlier varbinds of this SET are applied and there is no undo
			return ag.response(req, errCommitFailed, i+1, req.vbs)
		}
	}
	return ag.response(req, errNoError, 0, req.vbs)
}

func commandCanonical(cmd int64) string {
	switch cmd {
	case cmdImmediateOn:
		return "power.on"
	case cmdImmediateOff:
		return "power.off.force"
	case cmdImmediateReboot:
		return "power.cycle"
	}
	return "other"
}

func (ag *agent) cancelPending(outlet int) { delete(ag.pending, outlet) }

// reboot switches the outlet off now and on again after the reboot duration on the engine's
// clock, unless a newer command for the outlet arrives first.
func (ag *agent) reboot(r engine.Request, outlet int) error {
	e := ag.a.rt.Engine
	ag.cmdMu.Lock()
	ag.gen++
	gen := ag.gen
	ag.pending[outlet] = gen
	err := e.SetOutlet(r, outlet, false)
	ag.cmdMu.Unlock()
	if err != nil {
		return err
	}
	due := e.Now().Add(time.Duration(float64(ag.a.cfg.RebootDuration) * e.Scale()))
	ag.a.wg.Add(1)
	go func() {
		defer ag.a.wg.Done()
		t := time.NewTicker(20 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ag.a.stop:
				return
			case <-t.C:
			}
			if e.Now().Before(due) {
				continue
			}
			ag.cmdMu.Lock()
			if ag.pending[outlet] == gen {
				delete(ag.pending, outlet)
				done := r
				done.Source = schema.EventSource{Class: "behaviour", Principal: "snmp"}
				done.Method = "immediateReboot"
				_ = e.SetOutlet(done, outlet, true)
			}
			ag.cmdMu.Unlock()
			return
		}
	}()
	return nil
}

// ---------------------------------------------------------------- the device's MIB now

// snapshot builds the device's view from committed state. Each engine accessor takes its own
// read lock, so none is called inside Read.
func (ag *agent) snapshot() *view {
	e := ag.a.rt.Engine
	var (
		name     string
		relay    = map[int]bool{}
		bmcSince time.Time
	)
	e.Read(func(m *model.Model) {
		name = ag.dev.Name
		for k, v := range ag.dev.State.Outlets {
			relay[k] = v
		}
		bmcSince = ag.dev.State.BMCSince
	})
	sets := e.FaultSets(ag.dev)
	now, since := e.Now(), e.Started()
	if bmcSince.After(since) {
		since = bmcSince
	}
	ticks := uint64(0)
	if up := now.Sub(since); up > 0 {
		ticks = uint64(up/(10*time.Millisecond)) % (1 << 32)
	}

	v := &view{}
	b := ag.b
	inst0 := OID{0}
	v.add(sysDescr, inst0, strValue(b.spec.SysDescr), "inventory.read")
	v.add(sysObjectID, inst0, oidValue(b.sysObjectID), "inventory.read")
	v.add(sysUpTime, inst0, ticksValue(ticks), "health.read")
	v.add(sysContact, inst0, strValue(ag.a.cfg.Contact), "inventory.read")
	v.add(sysName, inst0, strValue(name), "inventory.read")
	v.add(sysLocation, inst0, strValue(ag.rack), "inventory.read")
	v.add(sysServices, inst0, intValue(72), "inventory.read")

	scalar := func(key string, val value, read string) {
		if o, ok := b.scalars[key]; ok {
			v.add(o[:len(o)-1], o[len(o)-1:], val, read)
		}
	}
	scalar(objDeviceName, strValue(name), "inventory.read")
	if ag.powerSensor != "" {
		if w, _, ok := e.Sensor(ag.dev, ag.powerSensor); ok {
			scalar(objDevicePower, intValue(int64(math.Round(w/10))), "health.read")
		}
	}
	if ag.currentSensor != "" {
		if amps, st, ok := e.Sensor(ag.dev, ag.currentSensor); ok {
			load := int64(loadNormal)
			switch st {
			case model.Warning:
				load = loadNearOverload
			case model.Critical:
				load = loadOverload
			}
			scalar(objDeviceLoad, intValue(load), "health.read")
			scalar(objPhaseCurrent, intValue(int64(math.Round(amps*10))), "health.read")
		}
	}
	for n := 1; n <= ag.outlets; n++ {
		on := relay[n]
		if f, ok := sets[fmt.Sprintf("outlet.%d", n)]; ok && fmt.Sprint(f) == "off" {
			on = false
		}
		label := ag.served[n]
		if label == "" {
			label = fmt.Sprintf("Outlet %d", n)
		}
		state, command := int64(outletOff), int64(cmdImmediateOff)
		if on {
			state, command = outletOn, cmdImmediateOn
		}
		idx := OID{uint32(n)}
		if o, ok := b.columns[objOutletName]; ok {
			v.add(o, idx, strValue(label), "inventory.read")
		}
		if o, ok := b.columns[objOutletState]; ok {
			v.add(o, idx, intValue(state), "health.read")
		}
		if o, ok := b.columns[objOutletCommand]; ok {
			// reading the command returns the outlet's state as a command (PowerNet-MIB)
			v.add(o, idx, intValue(command), "health.read")
		}
	}
	v.sort()
	return v
}

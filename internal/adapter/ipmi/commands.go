package ipmi

// The IPMI commands this BMC answers: the call list of `ipmitool`'s mc, chassis, sel, sdr,
// sensor and fru commands and of lifecycle managers that drive power and boot over IPMI.
// Anything else is answered "invalid command" (C1h) and logged as `other`.

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

const (
	netFnChassis = 0x00
	netFnSensor  = 0x04
	netFnApp     = 0x06
	netFnStorage = 0x0A

	cmdGetDeviceID             = 0x01
	cmdGetSelfTestResults      = 0x04
	cmdGetACPIPowerState       = 0x07
	cmdGetSystemGUID           = 0x37
	cmdGetChannelAuthCaps      = 0x38
	cmdSetSessionPrivilege     = 0x3B
	cmdCloseSession            = 0x3C
	cmdGetChannelCipherSuites  = 0x54
	cmdGetChassisStatus        = 0x01
	cmdChassisControl          = 0x02
	cmdSetSystemBootOptions    = 0x08
	cmdGetSystemBootOptions    = 0x09
	cmdGetSensorHysteresis     = 0x25
	cmdGetSensorThresholds     = 0x27
	cmdGetSensorReading        = 0x2D
	cmdGetFRUInventoryAreaInfo = 0x10
	cmdReadFRUData             = 0x11
	cmdGetSDRRepositoryInfo    = 0x20
	cmdReserveSDRRepository    = 0x22
	cmdGetSDR                  = 0x23
	cmdGetSELInfo              = 0x40
	cmdGetSELAllocationInfo    = 0x41
	cmdReserveSEL              = 0x42
	cmdGetSELEntry             = 0x43
	cmdClearSEL                = 0x47

	ccOK                    = 0x00
	ccParamNotSupported     = 0x80
	ccInvalidSessionID      = 0x87
	ccPrivilegeExceedsLimit = 0x81
	ccInvalidCommand        = 0xC1
	ccReservationCanceled   = 0xC5
	ccReqDataLengthInvalid  = 0xC7
	ccParamOutOfRange       = 0xC9
	ccNotPresent            = 0xCB
	ccInvalidDataField      = 0xCC
	ccInsufficientPrivilege = 0xD4
	ccNotSupportedInState   = 0xD5
	ccUnspecified           = 0xFF
)

// call is one command being answered.
type call struct {
	b         *bmc
	s         *session // nil outside a session
	r         request
	from      *net.UDPAddr
	user      string
	canonical string
	target    string
	logged    bool // the engine logged the request itself
}

type handler struct {
	name      string
	canonical string
	priv      byte
	fn        func(c *call) (byte, []byte)
}

type cmdKey struct{ netFn, cmd byte }

var handlers map[cmdKey]handler

func init() {
	handlers = map[cmdKey]handler{
		{netFnApp, cmdGetDeviceID}:            {"Get Device ID", "inventory.read", privUser, getDeviceID},
		{netFnApp, cmdGetSelfTestResults}:     {"Get Self Test Results", "health.read", privUser, getSelfTest},
		{netFnApp, cmdGetACPIPowerState}:      {"Get ACPI Power State", "health.read", privUser, getACPIPowerState},
		{netFnApp, cmdGetSystemGUID}:          {"Get System GUID", "inventory.read", privUser, getSystemGUID},
		{netFnApp, cmdGetChannelAuthCaps}:     {"Get Channel Authentication Capabilities", "other", privCallback, getChannelAuthCaps},
		{netFnApp, cmdGetChannelCipherSuites}: {"Get Channel Cipher Suites", "other", privCallback, getChannelCipherSuites},
		{netFnApp, cmdSetSessionPrivilege}:    {"Set Session Privilege Level", "other", privCallback, setSessionPrivilege},
		{netFnApp, cmdCloseSession}:           {"Close Session", "session.delete", privCallback, closeSession},

		{netFnChassis, cmdGetChassisStatus}:     {"Get Chassis Status", "health.read", privUser, getChassisStatus},
		{netFnChassis, cmdChassisControl}:       {"Chassis Control", "other", privOperator, chassisControl},
		{netFnChassis, cmdSetSystemBootOptions}: {"Set System Boot Options", "other", privOperator, setBootOptions},
		{netFnChassis, cmdGetSystemBootOptions}: {"Get System Boot Options", "inventory.read", privUser, getBootOptions},

		{netFnSensor, cmdGetSensorReading}:    {"Get Sensor Reading", "health.read", privUser, getSensorReading},
		{netFnSensor, cmdGetSensorThresholds}: {"Get Sensor Thresholds", "health.read", privUser, getSensorThresholds},
		{netFnSensor, cmdGetSensorHysteresis}: {"Get Sensor Hysteresis", "health.read", privUser, getSensorHysteresis},

		{netFnStorage, cmdGetFRUInventoryAreaInfo}: {"Get FRU Inventory Area Info", "inventory.read", privUser, getFRUInfo},
		{netFnStorage, cmdReadFRUData}:             {"Read FRU Data", "inventory.read", privUser, readFRU},
		{netFnStorage, cmdGetSDRRepositoryInfo}:    {"Get SDR Repository Info", "health.read", privUser, getSDRInfo},
		{netFnStorage, cmdReserveSDRRepository}:    {"Reserve SDR Repository", "health.read", privUser, reserveSDR},
		{netFnStorage, cmdGetSDR}:                  {"Get SDR", "health.read", privUser, getSDR},
		{netFnStorage, cmdGetSELInfo}:              {"Get SEL Info", "log.read", privUser, getSELInfo},
		{netFnStorage, cmdGetSELAllocationInfo}:    {"Get SEL Allocation Info", "log.read", privUser, getSELAllocInfo},
		{netFnStorage, cmdReserveSEL}:              {"Reserve SEL", "log.read", privUser, reserveSEL},
		{netFnStorage, cmdGetSELEntry}:             {"Get SEL Entry", "log.read", privUser, getSELEntry},
		{netFnStorage, cmdClearSEL}:                {"Clear SEL", "log.clear", privOperator, clearSEL},
	}
}

func commandName(r request) string {
	if h, ok := handlers[cmdKey{r.netFn, r.cmd}]; ok {
		return h.name
	}
	return fmt.Sprintf("NetFn 0x%02x Cmd 0x%02x", r.netFn, r.cmd)
}

// target is the logged request target: the command's address and a qualifier.
func target(r request, q string) string {
	t := fmt.Sprintf("netfn 0x%02x cmd 0x%02x", r.netFn, r.cmd)
	if q != "" {
		t += ": " + q
	}
	return t
}

// failShare reports whether the nth request is one of a fault's share of failed answers.
func failShare(n uint64, pct int) bool {
	return (n*uint64(pct))/100 != ((n-1)*uint64(pct))/100
}

// command answers one IPMI request, inside a session (s) or outside one (s == nil).
func (b *bmc) command(s *session, r request, from *net.UDPAddr, errPct int) (byte, []byte) {
	c := &call{b: b, s: s, r: r, from: from}
	if s != nil {
		c.user = s.user
	}
	h, ok := handlers[cmdKey{r.netFn, r.cmd}]
	if !ok {
		b.log(from, c.user, commandName(r), target(r, ""), "other", ccInvalidCommand)
		return ccInvalidCommand, nil
	}
	c.canonical = h.canonical
	if s != nil && s.priv < h.priv {
		b.log(from, c.user, h.name, target(r, ""), h.canonical, ccInsufficientPrivilege)
		return ccInsufficientPrivilege, nil
	}
	if s != nil && errPct > 0 && r.netFn != netFnApp {
		b.requests++
		if failShare(b.requests, errPct) {
			b.log(from, c.user, h.name, target(r, "fault: bmc.errorPercent"), h.canonical, ccUnspecified)
			return ccUnspecified, nil
		}
	}
	cc, data := h.fn(c)
	if !c.logged {
		b.log(from, c.user, h.name, target(r, c.target), c.canonical, cc)
	}
	return cc, data
}

func (c *call) request(canonical string) engine.Request {
	return c.b.req(c.from, c.user, commandName(c.r), target(c.r, c.target), canonical)
}

// ---------------------------------------------------------------- application

var digits = regexp.MustCompile(`\d+`)

// fwRev encodes a firmware version as Get Device ID does: the major in binary, the minor in
// BCD, and up to four further numbers as the auxiliary revision.
func fwRev(v string) (major, minor byte, aux []byte) {
	nums := digits.FindAllString(v, -1)
	n := func(i int) int {
		if i >= len(nums) {
			return 0
		}
		x, _ := strconv.Atoi(nums[i])
		return x
	}
	major = byte(min(n(0), 0x7F))
	m := n(1) % 100
	minor = byte(m/10<<4 | m%10)
	aux = make([]byte, 4)
	for i := 0; i < 4; i++ {
		aux[i] = byte(n(2 + i))
	}
	return
}

func getDeviceID(c *call) (byte, []byte) {
	b := c.b
	var fw string
	b.a.e.Read(func(*model.Model) {
		if f := b.d.State.Firmware["bmc"]; f != nil {
			fw = f.Version
		}
	})
	major, minor, aux := fwRev(fw)
	data := []byte{0x20, 0x01, major, minor, 0x02,
		0x8F, // chassis, FRU inventory, SEL, SDR repository and sensor device; no device SDRs
		byte(b.mfr), byte(b.mfr >> 8), byte(b.mfr >> 16)}
	data = append(data, put16(b.product)...)
	return ccOK, append(data, aux...)
}

func getSelfTest(c *call) (byte, []byte) { return ccOK, []byte{0x55, 0x00} }

func getSystemGUID(c *call) (byte, []byte) { return ccOK, c.b.guid }

func getACPIPowerState(c *call) (byte, []byte) {
	if c.b.poweredOn() {
		return ccOK, []byte{0x00, 0x00} // S0/G0, D0
	}
	return ccOK, []byte{0x05, 0x03} // S5/G2 soft off, D3
}

func getChannelAuthCaps(c *call) (byte, []byte) {
	d := c.r.data
	if len(d) < 2 {
		return ccReqDataLengthInvalid, nil
	}
	if ch := d[0] & 0x0F; ch != 0x0E && ch != 0x01 {
		return ccInvalidDataField, nil
	}
	return ccOK, []byte{0x01,
		0x80, // IPMI v2.0 extended capabilities; no IPMI v1.5 authentication types
		0x04, // non-null user names enabled; per-message and user-level authentication on
		0x02, // IPMI v2.0 connections only
		0, 0, 0, 0}
}

func getChannelCipherSuites(c *call) (byte, []byte) {
	d := c.r.data
	if len(d) < 3 {
		return ccReqDataLengthInvalid, nil
	}
	if ch := d[0] & 0x0F; ch != 0x0E && ch != 0x01 {
		return ccInvalidDataField, nil
	}
	if d[1] != payloadIPMI {
		return ccInvalidDataField, nil
	}
	var list []byte
	if d[2]&0x80 != 0 {
		for _, s := range suites {
			list = append(list, 0xC0, s.id, s.auth, 0x40|s.integ, 0x80|s.conf)
		}
	} else {
		list = []byte{authHMACSHA1, authHMACSHA256, 0x40 | integHMACSHA1, 0x40 | integHMACSHA256, 0x80 | confAESCBC128}
	}
	idx := int(d[2]&0x3F) * 16
	out := []byte{0x01}
	if idx < len(list) {
		out = append(out, list[idx:min(idx+16, len(list))]...)
	}
	return ccOK, out
}

func setSessionPrivilege(c *call) (byte, []byte) {
	if c.s == nil {
		return ccInsufficientPrivilege, nil
	}
	if len(c.r.data) < 1 {
		return ccReqDataLengthInvalid, nil
	}
	want := c.r.data[0] & 0x0F
	c.target = fmt.Sprintf("privilege %d", want)
	if want == 0 {
		return ccOK, []byte{c.s.priv}
	}
	if want > c.s.maxPriv || want > privAdmin {
		return ccPrivilegeExceedsLimit, nil
	}
	c.s.priv = want
	return ccOK, []byte{want}
}

func closeSession(c *call) (byte, []byte) {
	if c.s == nil {
		return ccInsufficientPrivilege, nil
	}
	if len(c.r.data) < 4 {
		return ccReqDataLengthInvalid, nil
	}
	id := le32(c.r.data)
	switch {
	case id == c.s.bmcID:
		c.s.closing = true
	case id != 0 && c.b.sessions[id] != nil && c.s.priv >= privAdmin:
		delete(c.b.sessions, id)
	default:
		return ccInvalidSessionID, nil
	}
	return ccOK, nil
}

// ---------------------------------------------------------------- chassis

// poweredOn is IPMI's binary view of the model's power: on from the moment power is applied
// (PoweringOn) until the model reaches Off, as a BMC's chassis status reads its power rails; the
// host's POST and boot after that are BootProgress's, not chassis power's. (Reading off through
// a whole power-on window made ipmitool and fence_ipmilan time out waiting for "on".)
func (b *bmc) poweredOn() bool {
	var p string
	b.a.e.Read(func(*model.Model) { p = b.d.State.Power })
	return p != model.Off
}

func getChassisStatus(c *call) (byte, []byte) {
	b := c.b
	power := byte(0x00) // power restore policy: stay off (a returning feed leaves hosts off)
	if b.poweredOn() {
		power |= 0x01
	}
	misc := byte(0)
	for _, comp := range b.d.Components {
		h, _ := b.a.e.ComponentHealth(comp)
		if h == model.OK {
			continue
		}
		switch comp.Kind {
		case "fan":
			misc |= 0x08 // cooling/fan fault
		case "drive":
			misc |= 0x04 // drive fault
		}
	}
	return ccOK, []byte{power, 0x00, misc}
}

var controls = map[byte]struct{ name, canonical string }{
	0x00: {"power down", "power.off.force"},
	0x01: {"power up", "power.on"},
	0x02: {"power cycle", "power.cycle"},
	0x03: {"hard reset", "power.restart"},
	0x05: {"soft shutdown", "power.off.graceful"},
}

func chassisControl(c *call) (byte, []byte) {
	if len(c.r.data) < 1 {
		return ccReqDataLengthInvalid, nil
	}
	op, ok := controls[c.r.data[0]&0x0F]
	if !ok {
		c.target = fmt.Sprintf("control 0x%02x (not modelled)", c.r.data[0]&0x0F)
		return ccInvalidDataField, nil
	}
	c.target, c.canonical, c.logged = op.name, op.canonical, true
	err := c.b.a.e.Power(c.request(op.canonical))
	switch {
	case err == nil:
		return ccOK, nil
	case errors.Is(err, engine.ErrNoInput), errors.Is(err, engine.ErrPrecondition), errors.Is(err, engine.ErrBMCDown):
		return ccNotSupportedInState, nil
	}
	return ccUnspecified, nil
}

// boot device selectors (Table 28-14, data 2 bits 5:2) and the model's override targets.
var bootDevices = []struct {
	sel    byte
	target string
}{
	{0x1, "Pxe"}, {0x2, "Hdd"}, {0x3, "Hdd"}, {0x4, "Diags"}, {0x5, "Cd"}, {0x6, "BiosSetup"},
	{0x7, "Floppy"}, {0x8, "RemoteDrive"}, {0x9, "Cd"}, {0xB, "RemoteDrive"}, {0xF, "Floppy"},
}

func bootSelector(t string) byte {
	if t == "UefiHttp" {
		return 0x1 // IPMI has no HTTP boot: the nearest is a network boot
	}
	for _, d := range bootDevices {
		if d.target == t {
			return d.sel
		}
	}
	return 0
}

func setBootOptions(c *call) (byte, []byte) {
	d := c.r.data
	if len(d) < 1 {
		return ccReqDataLengthInvalid, nil
	}
	param := d[0] & 0x7F
	c.target = fmt.Sprintf("parameter %d", param)
	switch param {
	case 0, 4: // set in progress, boot info acknowledge: accepted; the model applies writes at once
		if len(d) < 2 {
			return ccReqDataLengthInvalid, nil
		}
		return ccOK, nil
	case 3:
		// BMC boot flag valid bit clearing (IPMI v2.0 table 28-14). Ironic sends 0x08 (don't clear
		// the valid bit on the 60-second timeout) before every boot device change, as pyghmi does;
		// a BMC that refuses it fails Ironic's set_boot_device.
		if len(d) < 2 {
			return ccReqDataLengthInvalid, nil
		}
		c.b.bootValidClear = d[1] & 0x1F
		c.target += fmt.Sprintf(": valid bit clearing 0x%02x", c.b.bootValidClear)
		return ccOK, nil
	case 5:
	default:
		return ccParamNotSupported, nil
	}
	if len(d) < 3 {
		return ccReqDataLengthInvalid, nil
	}
	flags, dev := d[1], (d[2]>>2)&0x0F
	var p engine.BootPatch
	str := func(s string) *string { return &s }
	switch {
	case flags&0x80 == 0:
		p.Enabled = str("Disabled")
		c.target += ": override invalid"
	case dev == 0:
		p.Enabled, p.Target = str("Disabled"), str("None")
		c.target += ": no override"
	default:
		target := ""
		for _, bd := range bootDevices {
			if bd.sel == dev {
				target = bd.target
				break
			}
		}
		if target == "" {
			return ccInvalidDataField, nil
		}
		enabled, mode := "Once", "Legacy"
		if flags&0x40 != 0 {
			enabled = "Continuous"
		}
		if flags&0x20 != 0 {
			mode = "UEFI"
		}
		p.Target, p.Enabled, p.Mode = str(target), str(enabled), str(mode)
		c.target += fmt.Sprintf(": %s %s %s", target, enabled, mode)
	}
	c.canonical, c.logged = "boot.override", true
	if err := c.b.a.e.SetBoot(c.request("boot.override"), p); err != nil {
		return ccUnspecified, nil
	}
	return ccOK, nil
}

func getBootOptions(c *call) (byte, []byte) {
	d := c.r.data
	if len(d) < 1 {
		return ccReqDataLengthInvalid, nil
	}
	param := d[0] & 0x7F
	c.target = fmt.Sprintf("parameter %d", param)
	out := []byte{0x01, param}
	switch param {
	case 0:
		return ccOK, append(out, 0x00) // set complete
	case 3:
		return ccOK, append(out, c.b.bootValidClear)
	case 4:
		return ccOK, append(out, 0x00, 0x00)
	case 5:
	default:
		return ccParamNotSupported, nil
	}
	var bs model.BootState
	c.b.a.e.Read(func(*model.Model) { bs = c.b.d.State.Boot })
	flags, dev := byte(0), byte(0)
	if bs.OverrideEnabled == "Once" || bs.OverrideEnabled == "Continuous" {
		flags = 0x80
		if bs.OverrideEnabled == "Continuous" {
			flags |= 0x40
		}
		if bs.OverrideMode == "UEFI" {
			flags |= 0x20
		}
		dev = bootSelector(bs.OverrideTarget)
	}
	return ccOK, append(out, flags, dev<<2, 0x00, 0x00, 0x00)
}

// ---------------------------------------------------------------- sensors

func (b *bmc) sensor(num byte) *sensor {
	for _, s := range b.sensors {
		if s.num == num {
			return s
		}
	}
	return nil
}

func getSensorReading(c *call) (byte, []byte) {
	if len(c.r.data) < 1 {
		return ccReqDataLengthInvalid, nil
	}
	s := c.b.sensor(c.r.data[0])
	if s == nil {
		return ccNotPresent, nil
	}
	c.target = "sensor " + s.spec.Name
	v, state, ok := c.b.a.e.Sensor(c.b.d, s.spec.Name)
	if !ok {
		return ccOK, []byte{0x00, 0xE0, 0x00} // reading unavailable
	}
	return ccOK, []byte{s.raw(v), 0xC0, thresholdBits(state)}
}

func getSensorThresholds(c *call) (byte, []byte) {
	if len(c.r.data) < 1 {
		return ccReqDataLengthInvalid, nil
	}
	s := c.b.sensor(c.r.data[0])
	if s == nil {
		return ccNotPresent, nil
	}
	c.target = "sensor " + s.spec.Name
	return ccOK, []byte{s.read, 0, 0, 0, s.unc, s.ucr, 0}
}

func getSensorHysteresis(c *call) (byte, []byte) {
	if len(c.r.data) < 1 {
		return ccReqDataLengthInvalid, nil
	}
	if c.b.sensor(c.r.data[0]) == nil {
		return ccNotPresent, nil
	}
	return ccOK, []byte{0x00, 0x00}
}

func getSDRInfo(c *call) (byte, []byte) {
	out := []byte{sdrVersion}
	out = append(out, put16(uint16(len(c.b.sensors)))...)
	out = append(out, 0xFF, 0xFF) // free space unspecified: the repository is the model's
	out = append(out, put32(unixOr(c.b.a.e.Started()))...)
	out = append(out, put32(0xFFFFFFFF)...)
	return ccOK, append(out, 0x22) // non-modal update, Reserve SDR Repository supported
}

func reserveSDR(c *call) (byte, []byte) {
	c.b.sdrRes++
	if c.b.sdrRes == 0 {
		c.b.sdrRes = 1
	}
	return ccOK, put16(c.b.sdrRes)
}

// readRecord serves a Get SDR / Get SEL Entry read: a record by id (0 first, FFFFh last), from
// an offset, with the next record's id.
func readRecord(records [][]byte, data []byte, reservation uint16) (byte, []byte, int) {
	if len(data) < 6 {
		return ccReqDataLengthInvalid, nil, -1
	}
	res, id, off, n := le16(data[0:2]), le16(data[2:4]), int(data[4]), int(data[5])
	if len(records) == 0 {
		return ccNotPresent, nil, -1
	}
	i := -1
	switch id {
	case 0x0000:
		i = 0
	case 0xFFFF:
		i = len(records) - 1
	default:
		if int(id) <= len(records) {
			i = int(id) - 1
		}
	}
	if i < 0 {
		return ccNotPresent, nil, -1
	}
	if off > 0 && res != reservation {
		return ccReservationCanceled, nil, i
	}
	rec := records[i]
	if off > len(rec) {
		return ccParamOutOfRange, nil, i
	}
	end := len(rec)
	if n != 0xFF && off+n < end {
		end = off + n
	}
	next := uint16(0xFFFF)
	if i+1 < len(records) {
		next = uint16(i + 2)
	}
	return ccOK, append(put16(next), rec[off:end]...), i
}

func getSDR(c *call) (byte, []byte) {
	recs := make([][]byte, len(c.b.sensors))
	for i, s := range c.b.sensors {
		recs[i] = s.record
	}
	cc, out, i := readRecord(recs, c.r.data, c.b.sdrRes)
	if i >= 0 {
		c.target = "record " + c.b.sensors[i].spec.Name
	}
	return cc, out
}

// ---------------------------------------------------------------- SEL

func (b *bmc) sel() []model.LogEntry {
	var out []model.LogEntry
	b.a.e.Read(func(*model.Model) {
		for _, l := range b.d.State.Log {
			if l.Service == "SEL" {
				out = append(out, l)
			}
		}
	})
	return out
}

// lastErase is when the SEL was last cleared, from the committed event log.
func (b *bmc) lastErase() time.Time {
	var t time.Time
	for _, ev := range b.a.e.Events(0, func(ev schema.Event) bool {
		return ev.Path == b.d.Path && ev.Request != nil && ev.Request.Canonical == "log.clear"
	}) {
		if x, err := time.Parse(time.RFC3339Nano, ev.T); err == nil {
			t = x
		}
	}
	return t
}

func getSELInfo(c *call) (byte, []byte) {
	sel := c.b.sel()
	n := len(sel)
	free, ops := 0, byte(0x03) // Reserve SEL and Get SEL Allocation Info supported
	if n < selCapacity {
		free = (selCapacity - n) * selRecordBytes
	} else {
		ops |= 0x80 // overflow
	}
	var added time.Time
	if n > 0 {
		added = sel[n-1].Created
	}
	out := []byte{sdrVersion}
	out = append(out, put16(uint16(min(n, 0xFFFE)))...)
	out = append(out, put16(uint16(free))...)
	out = append(out, put32(unixOr(added))...)
	out = append(out, put32(unixOr(c.b.lastErase()))...)
	return ccOK, append(out, ops)
}

func getSELAllocInfo(c *call) (byte, []byte) {
	freeUnits := max(0, selCapacity-len(c.b.sel()))
	out := put16(selCapacity)
	out = append(out, put16(selRecordBytes)...)
	out = append(out, put16(uint16(freeUnits))...)
	out = append(out, put16(uint16(freeUnits))...)
	return ccOK, append(out, 0x01)
}

func reserveSEL(c *call) (byte, []byte) {
	c.b.selRes++
	if c.b.selRes == 0 {
		c.b.selRes = 1
	}
	return ccOK, put16(c.b.selRes)
}

func getSELEntry(c *call) (byte, []byte) {
	sel := c.b.sel()
	recs := make([][]byte, len(sel))
	for i, l := range sel {
		recs[i] = selRecord(uint16(i+1), l)
	}
	cc, out, i := readRecord(recs, c.r.data, c.b.selRes)
	if i >= 0 {
		c.target = fmt.Sprintf("record %d", i+1)
	}
	return cc, out
}

func clearSEL(c *call) (byte, []byte) {
	d := c.r.data
	if len(d) < 6 {
		return ccReqDataLengthInvalid, nil
	}
	if d[2] != 'C' || d[3] != 'L' || d[4] != 'R' {
		return ccInvalidDataField, nil
	}
	if le16(d[0:2]) != c.b.selRes {
		return ccReservationCanceled, nil
	}
	switch d[5] {
	case 0x00: // get erasure status
		c.canonical, c.target = "log.read", "erasure status"
		return ccOK, []byte{0x01}
	case 0xAA:
		c.target, c.logged = "initiate erase", true
		if err := c.b.a.e.ClearLog(c.request("log.clear"), "SEL"); err != nil {
			return ccUnspecified, nil
		}
		c.b.selRes++              // a clear cancels reservations
		return ccOK, []byte{0x01} // erasure completed
	}
	return ccInvalidDataField, nil
}

// ---------------------------------------------------------------- FRU

func getFRUInfo(c *call) (byte, []byte) {
	if len(c.r.data) < 1 {
		return ccReqDataLengthInvalid, nil
	}
	if c.r.data[0] != 0 {
		return ccNotPresent, nil
	}
	return ccOK, append(put16(uint16(len(c.b.fru))), 0x00) // accessed by bytes
}

func readFRU(c *call) (byte, []byte) {
	d := c.r.data
	if len(d) < 4 {
		return ccReqDataLengthInvalid, nil
	}
	if d[0] != 0 {
		return ccNotPresent, nil
	}
	off, n := int(le16(d[1:3])), int(d[3])
	c.target = fmt.Sprintf("offset %d count %d", off, n)
	if off >= len(c.b.fru) {
		return ccParamOutOfRange, nil
	}
	end := min(off+n, len(c.b.fru))
	return ccOK, append([]byte{byte(end - off)}, c.b.fru[off:end]...)
}

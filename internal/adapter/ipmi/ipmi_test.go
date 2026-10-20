package ipmi

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

func TestServesEveryIPMIBMC(t *testing.T) {
	tw, a := lab(t)
	want := 0
	tw.Engine.Read(func(m *model.Model) {
		for _, d := range m.Devices {
			if d.HasBMC() && d.Profile.Spec.BMC.IPMI {
				want++
			} else if a.Endpoints[d.Path] != "" {
				t.Errorf("%s has no IPMI BMC but is served", d.Path)
			}
		}
	})
	r := a.Report()
	if r.Served != want || want == 0 {
		t.Fatalf("served %d, want %d", r.Served, want)
	}
	for _, p := range []string{dell, gb300, generic} {
		ep := a.Endpoint(p)
		if ep == "" || view(tw, p)["endpoint"].(map[string]any)["ipmi"] != ep {
			t.Fatalf("%s: endpoint %q not recorded in the model", p, ep)
		}
		if tw.Net.Endpoint(p, "ipmi") != ep {
			t.Fatalf("%s: endpoint not in the substrate directory", p)
		}
	}
	if !strings.Contains(strings.Join(r.Lines, "\n"), "oem-dell/idrac") {
		t.Errorf("report should name dialects without an IPMI manufacturer id: %v", r.Lines)
	}
}

func TestSessionsAndPowerBothCipherSuites(t *testing.T) {
	tw, a := lab(t)
	for _, s := range suites {
		c := openAdmin(t, tw, a, dell, s)
		id := c.mustCmd(t, netFnApp, cmdGetDeviceID)
		if id[2] != 7 || id[3] != 0x10 || id[4] != 0x02 {
			t.Fatalf("suite %d: device id firmware/version bytes % x, want 7.10 and IPMI 2.0", s.id, id[2:5])
		}
		if st := c.mustCmd(t, netFnChassis, cmdGetChassisStatus); st[0]&1 != 1 {
			t.Fatalf("suite %d: chassis should be on: % x", s.id, st)
		}
		c.mustCmd(t, netFnChassis, cmdChassisControl, 0x00)
		if p := view(tw, dell)["power"]; p != model.Off {
			t.Fatalf("suite %d: power down via IPMI left the model %v", s.id, p)
		}
		if st := c.mustCmd(t, netFnChassis, cmdGetChassisStatus); st[0]&1 != 0 {
			t.Fatalf("suite %d: chassis should read off", s.id)
		}
		c.mustCmd(t, netFnChassis, cmdChassisControl, 0x01)
		waitFor(t, 6*time.Second, "power on after the scaled onSeconds", func() bool { return view(tw, dell)["power"] == model.On })
		c.close(t)
		if _, _, err := c.cmd(netFnApp, cmdGetDeviceID); !errors.Is(err, errTimeout) {
			t.Fatalf("suite %d: a closed session must not be answered: %v", s.id, err)
		}
	}
	evs := requests(tw, dell, "power.off.force")
	if len(evs) != 2 || evs[0].Request.Method != "Chassis Control" || evs[0].Source.Class != "stack" || evs[0].Source.Principal != "root" {
		t.Fatalf("power.off.force requests: %+v", evs)
	}
	if n := len(requests(tw, dell, "session.create")); n != 2 {
		t.Fatalf("session.create events: %d", n)
	}
}

func TestAuthenticationIsTheModels(t *testing.T) {
	tw, a := lab(t)
	cr := credsOf(tw, gb300)
	var re rakpError

	c := dial(t, a.Endpoint(gb300))
	if err := c.open(cr.user, "not-the-password", suites[0], privAdmin); !errors.As(err, &re) || re.status != rmcpInvalidIntegrity {
		t.Fatalf("wrong password: %v", err)
	}
	c = dial(t, a.Endpoint(gb300))
	if err := c.open("nobody", "x", suites[1], privAdmin); !errors.As(err, &re) || re.status != rmcpUnauthorizedName {
		t.Fatalf("unknown user: %v", err)
	}
	evs := requests(tw, gb300, "session.create")
	if len(evs) != 2 || evs[0].Request.Status != rmcpInvalidIntegrity || evs[1].Request.Status != rmcpUnauthorizedName {
		t.Fatalf("failed logins should be logged with their RMCP+ status: %+v", evs)
	}

	// a read-only account: its role caps the session, and chassis control needs operator
	addAccount(t, tw, gb300, "viewer", "viewer-pass", "ReadOnly")
	c = dial(t, a.Endpoint(gb300))
	if err := c.open("viewer", "viewer-pass", suites[1], privAdmin); !errors.As(err, &re) || re.status != rmcpUnauthorizedRole {
		t.Fatalf("read-only user asking for administrator: %v", err)
	}
	c = dial(t, a.Endpoint(gb300))
	if err := c.open("viewer", "viewer-pass", suites[1], privUser); err != nil {
		t.Fatal(err)
	}
	c.mustCmd(t, netFnChassis, cmdGetChassisStatus)
	if cc, _, _ := c.cmd(netFnChassis, cmdChassisControl, 0x00); cc != ccInsufficientPrivilege {
		t.Fatalf("chassis control as user: cc %02x", cc)
	}
	if cc, _, _ := c.cmd(netFnApp, cmdSetSessionPrivilege, privAdmin); cc != ccPrivilegeExceedsLimit {
		t.Fatalf("raising a user session to administrator: cc %02x", cc)
	}
	if view(tw, gb300)["power"] != model.On {
		t.Fatal("a refused chassis control changed the model")
	}

	// an account rotated by the engine takes effect on the next session
	if err := tw.Engine.UpdateAccount(reqFor(gb300), "1", map[string]any{"Password": "rotated-pass"}); err != nil {
		t.Fatal(err)
	}
	c = dial(t, a.Endpoint(gb300))
	if err := c.open(cr.user, cr.pass, suites[0], privAdmin); err == nil {
		t.Fatal("the old password still opens a session")
	}
	c = dial(t, a.Endpoint(gb300))
	if err := c.open(cr.user, "rotated-pass", suites[0], privAdmin); err != nil {
		t.Fatal(err)
	}
}

func TestBMCDownAnswersNothing(t *testing.T) {
	tw, a := lab(t)
	c := openAdmin(t, tw, a, generic, suites[0])
	if err := c.ping(); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Engine.Inject(generic, "generic/bmc/unreachable", nil, "", stackSrc); err != nil {
		t.Fatal(err)
	}
	if err := c.ping(); !errors.Is(err, errTimeout) {
		t.Fatalf("ping to an unreachable BMC: %v", err)
	}
	if _, _, err := c.cmd(netFnChassis, cmdGetChassisStatus); !errors.Is(err, errTimeout) {
		t.Fatalf("command to an unreachable BMC: %v", err)
	}
	if _, err := tw.Engine.Clear(generic, "", stackSrc); err != nil {
		t.Fatal(err)
	}
	if err := c.ping(); err != nil {
		t.Fatal(err)
	}
	// the BMC kept no session across being unavailable
	if _, _, err := c.cmd(netFnChassis, cmdGetChassisStatus); !errors.Is(err, errTimeout) {
		t.Fatalf("session survived the BMC going away: %v", err)
	}
	if _, err := tw.Engine.Inject(generic, "generic/bmc/errors", map[string]any{"rate": 50}, "", stackSrc); err != nil {
		t.Fatal(err)
	}
	c = openAdmin(t, tw, a, generic, suites[0])
	fails := 0
	for i := 0; i < 10; i++ {
		cc, _, err := c.cmd(netFnChassis, cmdGetChassisStatus)
		if err != nil {
			t.Fatal(err)
		}
		if cc == ccUnspecified {
			fails++
		}
	}
	if fails != 5 {
		t.Fatalf("errorPercent 50 failed %d of 10", fails)
	}
}

func TestRetransmissionIsAnsweredNotRerun(t *testing.T) {
	tw, a := lab(t)
	c := openAdmin(t, tw, a, dell, suites[1])
	first := c.mustCmd(t, netFnStorage, cmdReserveSEL)
	again, err := c.roundTrip(c.lastSent)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := c.decode(again, netFnStorage, cmdReserveSEL)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("retransmission: % x then % x (%v)", first, second, err)
	}
	if third := c.mustCmd(t, netFnStorage, cmdReserveSEL); bytes.Equal(third, first) {
		t.Fatal("a new request must get a new reservation")
	}
	// a packet whose integrity code is wrong is dropped
	c.seq++
	pkt := v2Packet(payloadIPMI, c.bmcID, c.seq, c.message(netFnApp, cmdGetDeviceID, nil), c.keys)
	pkt[len(pkt)-1] ^= 0xFF
	if _, err := c.roundTrip(pkt); !errors.Is(err, errTimeout) {
		t.Fatalf("tampered packet answered: %v", err)
	}
	// sessionless commands are limited to discovery
	if cc, _, err := c.sessionless(netFnChassis, cmdGetChassisStatus); err != nil || cc != ccInsufficientPrivilege {
		t.Fatalf("chassis status outside a session: %02x %v", cc, err)
	}
	if cc, out, err := c.sessionless(netFnApp, cmdGetChannelCipherSuites, 0x0E, 0x00, 0x80); err != nil || cc != ccOK ||
		!bytes.Equal(out, []byte{0x01, 0xC0, 3, 1, 0x41, 0x81, 0xC0, 17, 3, 0x44, 0x81}) {
		t.Fatalf("cipher suites: %02x % x %v", cc, out, err)
	}
}

func TestBootFlagsRoundTrip(t *testing.T) {
	tw, a := lab(t)
	c := openAdmin(t, tw, a, gb300, suites[1])
	// PXE, persistent, EFI
	c.mustCmd(t, netFnChassis, cmdSetSystemBootOptions, 0x05, 0xE0, 0x04, 0, 0, 0)
	b := view(tw, gb300)["boot"].(map[string]any)
	if b["overrideTarget"] != "Pxe" || b["overrideEnabled"] != "Continuous" {
		t.Fatalf("boot after IPMI set: %v", b)
	}
	out := c.mustCmd(t, netFnChassis, cmdGetSystemBootOptions, 0x05, 0, 0)
	if !bytes.Equal(out, []byte{0x01, 0x05, 0xE0, 0x04, 0, 0, 0}) {
		t.Fatalf("boot flags read back % x", out)
	}
	if n := len(requests(tw, gb300, "boot.override")); n != 1 {
		t.Fatalf("boot.override requests: %d", n)
	}
	if cc, _, _ := c.cmd(netFnChassis, cmdGetSystemBootOptions, 0x07, 0, 0); cc != ccParamNotSupported {
		t.Fatalf("unsupported parameter: cc %02x", cc)
	}
}

// Ironic's ipmitool management interface sends `raw 0x00 0x08 0x03 0x08` (boot option parameter
// 3, BMC boot flag valid bit clearing) before every boot device change; a refusal fails the change.
func TestBootFlagValidBitClearing(t *testing.T) {
	tw, a := lab(t)
	c := openAdmin(t, tw, a, dell, suites[1])
	if out := c.mustCmd(t, netFnChassis, cmdGetSystemBootOptions, 0x03, 0, 0); !bytes.Equal(out, []byte{0x01, 0x03, 0x00}) {
		t.Fatalf("parameter 3 before any set: % x", out)
	}
	c.mustCmd(t, netFnChassis, cmdSetSystemBootOptions, 0x03, 0x08)
	if out := c.mustCmd(t, netFnChassis, cmdGetSystemBootOptions, 0x03, 0, 0); !bytes.Equal(out, []byte{0x01, 0x03, 0x08}) {
		t.Fatalf("parameter 3 read back: % x", out)
	}
	if cc, _, _ := c.cmd(netFnChassis, cmdSetSystemBootOptions, 0x03); cc != ccReqDataLengthInvalid {
		t.Fatalf("parameter 3 without data: cc %02x", cc)
	}
	// then the boot device, as Ironic sets it next: PXE, once, EFI
	c.mustCmd(t, netFnChassis, cmdSetSystemBootOptions, 0x05, 0xA0, 0x04, 0, 0, 0)
	if b := view(tw, dell)["boot"].(map[string]any); b["overrideTarget"] != "Pxe" || b["overrideEnabled"] != "Once" {
		t.Fatalf("boot after parameter 3 and 5: %v", b)
	}
}

func TestSELIsTheModelLog(t *testing.T) {
	tw, a := lab(t)
	if _, err := tw.Engine.Inject(dell+"/psu-1", "generic/power/psu-failure", nil, "", stackSrc); err != nil {
		t.Fatal(err)
	}
	c := openAdmin(t, tw, a, dell, suites[0])
	info := c.mustCmd(t, netFnStorage, cmdGetSELInfo)
	if le16(info[1:3]) != 1 {
		t.Fatalf("SEL entries % x", info)
	}
	e := c.mustCmd(t, netFnStorage, cmdGetSELEntry, 0, 0, 0, 0, 0, 0xFF)
	if le16(e[0:2]) != 0xFFFF || len(e) != 18 || e[2+10] != 0x08 || e[2+13]&0x0F != 0x01 {
		t.Fatalf("SEL entry: % x", e)
	}
	res := c.mustCmd(t, netFnStorage, cmdReserveSEL)
	if cc, _, _ := c.cmd(netFnStorage, cmdClearSEL, 0x00, 0x00, 'C', 'L', 'R', 0xAA); cc != ccReservationCanceled {
		t.Fatalf("clear with a stale reservation: cc %02x", cc)
	}
	c.mustCmd(t, netFnStorage, cmdClearSEL, res[0], res[1], 'C', 'L', 'R', 0xAA)
	if info := c.mustCmd(t, netFnStorage, cmdGetSELInfo); le16(info[1:3]) != 0 || le32(info[9:13]) == 0xFFFFFFFF {
		t.Fatalf("after clear: % x", info)
	}
	var n int
	tw.Engine.Read(func(m *model.Model) { n = len(m.DeviceBy[dell].State.Log) })
	if n != 0 {
		t.Fatalf("model log after IPMI clear: %d entries", n)
	}
	// both clears are logged: the refused one with its completion code
	if evs := requests(tw, dell, "log.clear"); len(evs) != 2 || evs[0].Request.Status != ccReservationCanceled || evs[1].Request.Status != 0 {
		t.Fatalf("log.clear requests: %+v", evs)
	}
}

func TestSDRSensorsAndFRU(t *testing.T) {
	tw, a := lab(t)
	c := openAdmin(t, tw, a, dell, suites[1])
	info := c.mustCmd(t, netFnStorage, cmdGetSDRRepositoryInfo)
	if info[0] != sdrVersion || le16(info[1:3]) != 4 {
		t.Fatalf("SDR info % x", info)
	}
	res := c.mustCmd(t, netFnStorage, cmdReserveSDRRepository)
	var names []string
	for id := uint16(0); id != 0xFFFF; {
		r := c.mustCmd(t, netFnStorage, cmdGetSDR, res[0], res[1], byte(id), byte(id>>8), 0, 0xFF)
		rec := r[2:]
		if rec[3] != sdrFullSensor || int(rec[4]) != len(rec)-5 {
			t.Fatalf("record % x", rec)
		}
		names = append(names, string(rec[48:]))
		num, m := rec[7], int(rec[24])|int(rec[25]>>6)<<8
		rd := c.mustCmd(t, netFnSensor, cmdGetSensorReading, num)
		var d *model.Device
		tw.Engine.Read(func(mm *model.Model) { d = mm.DeviceBy[dell] })
		v, _, _ := tw.Engine.Sensor(d, string(rec[48:]))
		if got := float64(int(rd[0]) * m); got < v-float64(m) || got > v+float64(m) {
			t.Fatalf("%s: IPMI reads %v, model %v", rec[48:], got, v)
		}
		id = le16(r[0:2])
	}
	if strings.Join(names, ",") != "inlet,gpu-max,cpu-max,power" {
		t.Fatalf("SDR names %v", names)
	}
	fi := c.mustCmd(t, netFnStorage, cmdGetFRUInventoryAreaInfo, 0)
	size := int(le16(fi[0:2]))
	var fru []byte
	for off := 0; off < size; off += 24 {
		r := c.mustCmd(t, netFnStorage, cmdReadFRUData, 0, byte(off), byte(off>>8), 24)
		fru = append(fru, r[1:1+int(r[0])]...)
	}
	if checksum(fru[:7]) != fru[7] {
		t.Fatal("FRU common header checksum")
	}
	var serial string
	tw.Engine.Read(func(m *model.Model) { serial = m.DeviceBy[dell].Identity.Serial })
	if !bytes.Contains(fru, []byte(serial)) || !bytes.Contains(fru, []byte("PowerEdge XE9680")) {
		t.Fatalf("FRU lacks the identity: %q", fru)
	}
}

func TestEncodings(t *testing.T) {
	for v, want := range map[string][3]byte{"7.10.30.00": {7, 0x10, 30}, "25.09.12": {25, 0x09, 12}, "1.12.0": {1, 0x12, 0}, "": {0, 0, 0}} {
		maj, min, aux := fwRev(v)
		if maj != want[0] || min != want[1] || aux[0] != want[2] {
			t.Errorf("fwRev(%q) = %d %02x % x", v, maj, min, aux)
		}
	}
	g := guidOf("00112233-4455-6677-8899-aabbccddeeff")
	if !bytes.Equal(g, []byte{0x33, 0x22, 0x11, 0x00, 0x55, 0x44, 0x77, 0x66, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}) {
		t.Errorf("guid % x", g)
	}
	n := 0
	for i := uint64(1); i <= 100; i++ {
		if failShare(i, 30) {
			n++
		}
	}
	if n != 30 {
		t.Errorf("failShare 30%% gave %d", n)
	}
	s := &session{}
	for _, c := range []struct {
		seq     uint32
		ok, dup bool
	}{{1, true, false}, {3, true, false}, {2, true, false}, {2, false, true}, {40, true, false}, {5, false, false}, {0, false, false}} {
		_, dup, ok := s.accept(c.seq)
		if ok != c.ok || dup != c.dup {
			t.Errorf("accept(%d) = ok %v dup %v", c.seq, ok, dup)
		}
	}
	k := deriveKeys(authHMACSHA256, integHMACSHA256, confAESCBC128, randomBytes(32))
	for _, n := range []int{0, 1, 15, 16, 17, 40} {
		p := randomBytes(n)
		got, err := k.decrypt(k.encrypt(p))
		if err != nil || !bytes.Equal(got, p) {
			t.Errorf("AES round trip of %d bytes: %v", n, err)
		}
	}
}

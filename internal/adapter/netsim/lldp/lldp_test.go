package lldp

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var mac = net.HardwareAddr{0x1c, 0x34, 0xda, 0x00, 0x00, 0x01}

// TLV headers: the type in the top 7 bits, the length in the low 9.
func TestTLVHeader(t *testing.T) {
	for _, c := range []struct {
		typ, n int
		want   []byte
	}{
		{TLVEnd, 0, []byte{0x00, 0x00}},
		{TLVChassisID, 7, []byte{0x02, 0x07}},
		{TLVTTL, 2, []byte{0x06, 0x02}},
		{TLVMgmtAddr, 256, []byte{0x11, 0x00}}, // the length's ninth bit lands in the first byte
		{TLVOrgSpecific, 511, []byte{0xff, 0xff}},
		{TLVOrgSpecific, 600, []byte{0xff, 0xff}}, // capped at 511
	} {
		got := tlv(nil, c.typ, make([]byte, c.n))
		if !bytes.Equal(got[:2], c.want) {
			t.Errorf("type %d length %d: header %x, want %x", c.typ, c.n, got[:2], c.want)
		}
		if n := min(c.n, 511); len(got) != 2+n {
			t.Errorf("type %d length %d: %d bytes", c.typ, c.n, len(got))
		}
	}
}

// A frame laid out by hand from 802.1AB's TLV format, as a Cumulus-like neighbour sends it.
func TestGolden(t *testing.T) {
	golden := "0180c200000e" + "1c34da000001" + "88cc" +
		"0207" + "04" + "1c34da000001" + // Chassis ID, MAC
		"0405" + "05" + hex.EncodeToString([]byte("swp1")) + // Port ID, interface name
		"0602" + "0078" + // TTL 120
		"0a06" + hex.EncodeToString([]byte("leaf01")) + // System Name
		"0e04" + "0014" + "0014" + // capabilities bridge+router, enabled bridge+router
		"100c" + "05" + "01" + "0a1e0039" + "02" + "00000002" + "00" + // Management Address
		"0000" // End
	want, _ := hex.DecodeString(golden)
	in := Info{ChassisMAC: mac, PortID: "swp1", SysName: "leaf01", Bridge: true, Router: true,
		MgmtAddr: netip.MustParseAddr("10.30.0.57"), MgmtIfIndex: 2}
	got := Frame(nil, in)
	if !bytes.Equal(got, want) {
		t.Fatalf("frame\n got %x\nwant %x", got, want)
	}
	p, err := Parse(want)
	if err != nil {
		t.Fatal(err)
	}
	in.TTL, in.PortIDSubtype, in.PortMAC = 120, PortIfName, mac
	if !reflect.DeepEqual(p, in) {
		t.Errorf("parsed %+v\nwant %+v", p, in)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, in := range []Info{
		{ChassisMAC: mac, PortID: "eth1", PortIDSubtype: PortIfName, PortDesc: "to leaf01 swp7", SysName: "gpu-node-17",
			SysDesc: "Ubuntu 24.04 LTS Linux 6.8.0", TTL: 120, Station: true,
			MgmtAddr: netip.MustParseAddr("fd00::17"), MgmtIfIndex: 3, PortMAC: net.HardwareAddr{2, 0, 0, 0, 0, 9}},
		{ChassisMAC: mac, PortID: "Ethernet0", PortIDSubtype: PortLocal, SysName: "sonic", TTL: 4, Bridge: true, Router: true,
			MaxFrameSize: 9216, PortMAC: mac},
		{ChassisMAC: mac, PortID: "02:00:00:00:00:09", PortIDSubtype: PortMAC, TTL: 65535, Router: true, PortMAC: mac},
		{ChassisSubtype: ChassisLocal, ChassisID: "bmc-r12-u3", PortID: "bmc", PortIDSubtype: PortIfName, TTL: 120, Station: true,
			PortMAC: mac},
	} {
		f := Frame(nil, in)
		if len(f) < 60 {
			t.Errorf("%d-byte frame", len(f))
		}
		got, err := Parse(f)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, in) {
			t.Errorf("round trip\n got %+v\nwant %+v", got, in)
		}
	}
}

func TestDefaultsAndShutdown(t *testing.T) {
	in := Info{ChassisMAC: mac, PortID: "swp1", SysName: "leaf01", Bridge: true}
	p, err := Parse(Frame(nil, in))
	if err != nil || p.TTL != DefaultTTL || p.PortIDSubtype != PortIfName {
		t.Errorf("%+v %v", p, err)
	}
	src := net.HardwareAddr{2, 0, 0, 0, 0, 1}
	f := Shutdown(src, in)
	if !bytes.Equal(f[6:12], src) {
		t.Errorf("source %x", f[6:12])
	}
	p, err = Parse(f)
	if err != nil || p.TTL != 0 || p.SysName != "" || p.PortID != "swp1" || !bytes.Equal(p.ChassisMAC, mac) {
		t.Errorf("shutdown %+v %v", p, err)
	}
	// long strings are cut to 255
	p, _ = Parse(Frame(nil, Info{ChassisMAC: mac, PortID: "swp1", SysDesc: strings.Repeat("x", 300)}))
	if len(p.SysDesc) != 255 {
		t.Errorf("sysdesc %d", len(p.SysDesc))
	}
}

func TestParseErrors(t *testing.T) {
	good := Frame(nil, Info{ChassisMAC: mac, PortID: "swp1"})
	mut := func(f func([]byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	for name, f := range map[string][]byte{
		"short":     good[:10],
		"dst":       mut(func(b []byte) []byte { b[5] = 0x01; return b }),
		"ethertype": mut(func(b []byte) []byte { b[13] = 0x00; return b }),
		"no end":    good[:14+9+7+4],
		"overrun":   mut(func(b []byte) []byte { b[15] = 0x7f; return b }),
		"no ttl":    append(append([]byte(nil), good[:14+9+7]...), 0, 0),
	} {
		if _, err := Parse(f); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	// 802.1Q-tagged frames are read
	tagged := append(append(append([]byte(nil), good[:12]...), 0x81, 0x00, 0x00, 0x0a), good[12:]...)
	if p, err := Parse(tagged); err != nil || p.PortID != "swp1" {
		t.Errorf("tagged: %+v %v", p, err)
	}
}

// ---------------------------------------------------------------- the sender

type fakeLink struct {
	mu     sync.Mutex
	frames []Info
	up     bool
	fail   bool
	info   Info
}

func (l *fakeLink) write(f []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fail {
		return errors.New("no carrier")
	}
	p, err := Parse(f)
	if err != nil {
		return err
	}
	l.frames = append(l.frames, p)
	return nil
}

func (l *fakeLink) get() (Info, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.info, l.up
}

func (l *fakeLink) set(f func(*fakeLink)) {
	l.mu.Lock()
	f(l)
	l.mu.Unlock()
}

func (l *fakeLink) sent() []Info {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Info(nil), l.frames...)
}

func waitFor(t *testing.T, l *fakeLink, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(l.sent()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d frames, want %d", len(l.sent()), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A fake clock: the sender sends at start, then once per tick, only while the endpoint is up.
func TestSenderTicks(t *testing.T) {
	l := &fakeLink{up: true, info: Info{ChassisMAC: mac, PortID: "eth0", SysName: "host1"}}
	s := NewSender(0, l.write, l.get)
	ticks := make(chan time.Time)
	var interval time.Duration
	s.ticker = func(d time.Duration) (<-chan time.Time, func()) { interval = d; return ticks, func() {} }
	go s.Run(context.Background())
	waitFor(t, l, 1)
	if interval != 30*time.Second || s.TTL() != 120 {
		t.Errorf("interval %s, TTL %d", interval, s.TTL())
	}
	ticks <- time.Time{}
	waitFor(t, l, 2)
	l.set(func(l *fakeLink) { l.up = false }) // powered off: nothing sent
	ticks <- time.Time{}
	ticks <- time.Time{} // the second send returns only after the first tick was handled
	if n := len(l.sent()); n != 2 {
		t.Errorf("sent %d while down", n)
	}
	l.set(func(l *fakeLink) { l.up = true; l.info.SysName = "host1-renamed" })
	s.Trigger()
	waitFor(t, l, 3)
	l.set(func(l *fakeLink) { l.fail = true })
	ticks <- time.Time{}
	ticks <- time.Time{}
	if s.Errors() == 0 {
		t.Errorf("no write error counted")
	}
	l.set(func(l *fakeLink) { l.fail = false })
	s.Stop()
	<-s.Done()
	got := l.sent()
	if len(got) != 4 {
		t.Fatalf("%d frames", len(got))
	}
	for i, f := range got[:3] {
		if f.TTL != 120 {
			t.Errorf("frame %d TTL %d", i, f.TTL)
		}
	}
	if got[2].SysName != "host1-renamed" {
		t.Errorf("triggered frame %+v", got[2])
	}
	if got[3].TTL != 0 || got[3].PortID != "eth0" {
		t.Errorf("shutdown frame %+v", got[3])
	}
	s.Stop() // twice is safe
}

// With a real clock and a short interval, TTL is interval × 4 (at least 1), and a cancelled
// context ends Run without a shutdown frame.
func TestSenderInterval(t *testing.T) {
	l := &fakeLink{up: true, info: Info{ChassisMAC: mac, PortID: "eth0"}}
	s := NewSender(10*time.Millisecond, l.write, l.get)
	ctx, cancel := context.WithCancel(context.Background())
	go s.Run(ctx)
	waitFor(t, l, 4)
	cancel()
	<-s.Done()
	for _, f := range l.sent() {
		if f.TTL != 1 {
			t.Errorf("TTL %d", f.TTL)
		}
	}
	if NewSender(2*time.Second, nil, nil).TTL() != 8 || NewSender(5*time.Hour, nil, nil).TTL() != 0xffff {
		t.Errorf("TTL scaling")
	}
}

// Stopped while down: no shutdown frame (a powered-off device sends nothing).
func TestSenderStopWhileDown(t *testing.T) {
	l := &fakeLink{up: true, info: Info{ChassisMAC: mac, PortID: "eth0"}}
	s := NewSender(time.Hour, l.write, l.get)
	go s.Run(context.Background())
	waitFor(t, l, 1)
	l.set(func(l *fakeLink) { l.up = false })
	s.Stop()
	<-s.Done()
	if n := len(l.sent()); n != 1 {
		t.Errorf("%d frames", n)
	}
}

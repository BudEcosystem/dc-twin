package snmp

import (
	"io"
	"log"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
	"github.com/gosnmp/gosnmp"
)

const labEstate = "../../../examples/estates/lab-model.yaml"

// The rPDU2 OIDs of the generic/apc-rpdu2 dialect, as a stack would hard-code them.
const (
	oidSysDescr     = ".1.3.6.1.2.1.1.1.0"
	oidSysObjectID  = ".1.3.6.1.2.1.1.2.0"
	oidSysUpTime    = ".1.3.6.1.2.1.1.3.0"
	oidSysName      = ".1.3.6.1.2.1.1.5.0"
	oidSysLocation  = ".1.3.6.1.2.1.1.6.0"
	oidDeviceName   = ".1.3.6.1.4.1.318.1.1.26.2.1.3.1"
	oidDevicePower  = ".1.3.6.1.4.1.318.1.1.26.4.3.1.5.1"
	oidDeviceLoad   = ".1.3.6.1.4.1.318.1.1.26.4.3.1.4.1"
	oidPhaseCurrent = ".1.3.6.1.4.1.318.1.1.26.6.3.1.5.1"
	oidOutletName   = ".1.3.6.1.4.1.318.1.1.26.9.2.3.1.3"
	oidOutletState  = ".1.3.6.1.4.1.318.1.1.26.9.2.3.1.5"
	oidOutletCmd    = ".1.3.6.1.4.1.318.1.1.26.9.2.4.1.5"
)

type lab struct {
	t  *testing.T
	tw *twin.Twin
	a  *Adapter
}

func upLab(t *testing.T) *lab {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{"snmp"}, StateDir: t.TempDir(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	a, ok := tw.Adapter("snmp").(*Adapter)
	if !ok {
		t.Fatal("no snmp adapter")
	}
	return &lab{t: t, tw: tw, a: a}
}

// pdu finds a rack's device by name.
func (l *lab) device(rack, name string) *model.Device {
	l.t.Helper()
	var d *model.Device
	l.tw.Engine.Read(func(m *model.Model) {
		for _, r := range m.Racks {
			if r.Name != rack {
				continue
			}
			for _, x := range r.Devices {
				if x.Name == name {
					d = x
				}
			}
		}
	})
	if d == nil {
		l.t.Fatalf("no %s in rack %s", name, rack)
	}
	return d
}

// onOutlet is the device the rack's PDUs feed from an outlet (engine.recomputePower's rule).
func (l *lab) onOutlet(pdu *model.Device, outlet int) *model.Device {
	l.t.Helper()
	var d *model.Device
	l.tw.Engine.Read(func(m *model.Model) {
		for _, in := range pdu.RackOf().Inputs {
			if in.Kind == "pdu" && containsDevice(in.Sources, pdu) && outlet-1 < len(in.Serves) {
				d = in.Serves[outlet-1]
			}
		}
	})
	if d == nil {
		l.t.Fatalf("nothing on outlet %d of %s", outlet, pdu.Path)
	}
	return d
}

func (l *lab) ac(d *model.Device) bool {
	return l.tw.Engine.DeviceView(d)["ac"].(bool)
}

func (l *lab) client(pdu *model.Device, community string, v gosnmp.SnmpVersion) *gosnmp.GoSNMP {
	l.t.Helper()
	ep := l.a.Endpoint(pdu.Path)
	if ep == "" {
		l.t.Fatalf("%s has no SNMP endpoint", pdu.Path)
	}
	host, port, _ := net.SplitHostPort(ep)
	p, _ := strconv.Atoi(port)
	g := &gosnmp.GoSNMP{Target: host, Port: uint16(p), Community: community, Version: v, Timeout: 2 * time.Second, Retries: 0,
		MaxRepetitions: 10, Logger: gosnmp.NewLogger(log.New(io.Discard, "", 0))}
	if err := g.Connect(); err != nil {
		l.t.Fatal(err)
	}
	l.t.Cleanup(func() { g.Conn.Close() })
	return g
}

func get(t *testing.T, g *gosnmp.GoSNMP, oids ...string) []gosnmp.SnmpPDU {
	t.Helper()
	r, err := g.Get(oids)
	if err != nil {
		t.Fatal(err)
	}
	if r.Error != gosnmp.NoError {
		t.Fatalf("GET %v: %v at %d", oids, r.Error, r.ErrorIndex)
	}
	return r.Variables
}

func setInt(t *testing.T, g *gosnmp.GoSNMP, oid string, v int) *gosnmp.SnmpPacket {
	t.Helper()
	r, err := g.Set([]gosnmp.SnmpPDU{{Name: oid, Type: gosnmp.Integer, Value: v}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEveryPDUHasAnAgent(t *testing.T) {
	l := upLab(t)
	var want []string
	l.tw.Engine.Read(func(m *model.Model) {
		for _, d := range m.Devices {
			if d.Profile.Spec.SNMP != nil {
				want = append(want, d.Path)
			}
		}
	})
	eps := l.a.Endpoints()
	if len(want) == 0 || len(eps) != len(want) {
		t.Fatalf("%d SNMP devices, %d endpoints", len(want), len(eps))
	}
	for _, p := range want {
		if eps[p] == "" || l.tw.Net.Endpoint(p, "snmp") != eps[p] {
			t.Fatalf("%s: endpoint %q, netsim %q", p, eps[p], l.tw.Net.Endpoint(p, "snmp"))
		}
	}
	rep := l.a.Report()
	if rep.Served != len(want) || len(rep.IsNot) == 0 || !strings.Contains(strings.Join(rep.IsNot, "\n"), "PowerNet-MIB rPDU2") {
		t.Fatalf("report: %+v", rep)
	}
	t.Logf("%d PDU agents; report lines %q", rep.Served, rep.Lines)
}

func TestReadsProjectTheModel(t *testing.T) {
	l := upLab(t)
	pdu := l.device("b01", "pdu-a")
	g := l.client(pdu, "public", gosnmp.Version2c)
	vs := get(t, g, oidSysDescr, oidSysObjectID, oidSysUpTime, oidSysName, oidSysLocation, oidDeviceName,
		oidDevicePower, oidDeviceLoad, oidPhaseCurrent, oidOutletName+".1", oidOutletState+".1", oidOutletCmd+".1")
	if s := string(vs[0].Value.([]byte)); !strings.Contains(s, "dc-twin model PDU") {
		t.Fatalf("sysDescr %q", s)
	}
	if vs[1].Value.(string) != ".1.3.6.1.4.1.318.1.3.4.6" {
		t.Fatalf("sysObjectID %v", vs[1].Value)
	}
	if vs[2].Type != gosnmp.TimeTicks {
		t.Fatalf("sysUpTime type %v", vs[2].Type)
	}
	if string(vs[3].Value.([]byte)) != "pdu-a" || string(vs[5].Value.([]byte)) != "pdu-a" {
		t.Fatalf("names %q %q", vs[3].Value, vs[5].Value)
	}
	if string(vs[4].Value.([]byte)) != pdu.RackOf().Path {
		t.Fatalf("sysLocation %q", vs[4].Value)
	}
	w, _, _ := l.tw.Engine.Sensor(pdu, "power")
	if got := vs[6].Value.(int); w <= 0 || got != int(w/10+0.5) {
		t.Fatalf("power: %d hundredths of kW, sensor %.1f W", got, w)
	}
	if vs[7].Value.(int) != loadNormal {
		t.Fatalf("load state %v", vs[7].Value)
	}
	if vs[8].Value.(int) <= 0 {
		t.Fatalf("phase current %v", vs[8].Value)
	}
	if string(vs[9].Value.([]byte)) != l.onOutlet(pdu, 1).Name {
		t.Fatalf("outlet 1 is named %q", vs[9].Value)
	}
	if vs[10].Value.(int) != outletOn || vs[11].Value.(int) != cmdImmediateOn {
		t.Fatalf("outlet 1 state %v command %v", vs[10].Value, vs[11].Value)
	}
	t.Logf("pdu-a: %.1f W -> %d; %v tenths of A; outlet 1 feeds %s", w, vs[6].Value, vs[8].Value, vs[9].Value)

	// absent things, as a v2c manager sees them
	vs = get(t, g, ".1.3.6.1.2.1.1.1", oidOutletState+".99", ".1.3.6.1.2.1.2.1.0")
	if vs[0].Type != gosnmp.NoSuchInstance || vs[1].Type != gosnmp.NoSuchInstance || vs[2].Type != gosnmp.NoSuchObject {
		t.Fatalf("exceptions: %v %v %v", vs[0].Type, vs[1].Type, vs[2].Type)
	}
	r, err := g.GetNext([]string{oidOutletCmd + ".24"})
	if err != nil || r.Variables[0].Type != gosnmp.EndOfMibView {
		t.Fatalf("GETNEXT past the last object: %v %+v", err, r)
	}
	// and as a v1 manager does
	g1 := l.client(pdu, "public", gosnmp.Version1)
	r, err = g1.Get([]string{oidSysName, ".1.3.6.1.2.1.2.1.0"})
	if err != nil || r.Error != gosnmp.NoSuchName || r.ErrorIndex != 2 {
		t.Fatalf("v1 GET of a missing object: %v %+v", err, r)
	}
	r, err = g1.GetNext([]string{oidOutletCmd + ".24"})
	if err != nil || r.Error != gosnmp.NoSuchName || r.ErrorIndex != 1 {
		t.Fatalf("v1 GETNEXT past the end: %v %+v", err, r)
	}
}

func TestWalksAreOrderedAndAgree(t *testing.T) {
	l := upLab(t)
	pdu := l.device("b01", "pdu-a")
	g := l.client(pdu, "public", gosnmp.Version2c)
	walk, err := g.WalkAll(".1.3") // gosnmp fails a walk whose OIDs do not increase (and cannot encode ".1")
	if err != nil {
		t.Fatal(err)
	}
	bulk, err := g.BulkWalkAll(".1.3")
	if err != nil {
		t.Fatal(err)
	}
	if len(walk) != 7+4+3*24 || len(bulk) != len(walk) {
		t.Fatalf("walk %d, bulk walk %d varbinds", len(walk), len(bulk))
	}
	for i := range walk {
		if walk[i].Name != bulk[i].Name {
			t.Fatalf("walk and bulk walk disagree at %d: %s %s", i, walk[i].Name, bulk[i].Name)
		}
	}
	// a GETBULK asking for far more than fits is trimmed to the message size, not refused
	r, err := g.GetBulk([]string{".1.3"}, 0, 500)
	if err != nil || r.Error != gosnmp.NoError || len(r.Variables) == 0 || len(r.Variables) >= len(walk) {
		t.Fatalf("large GETBULK: %v, %d varbinds", err, len(r.Variables))
	}
	// non-repeaters, and repeaters running off the end
	r, err = g.GetBulk([]string{oidSysDescr, oidOutletCmd + ".23"}, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, v := range r.Variables {
		got = append(got, v.Name+"="+v.Type.String())
	}
	if len(r.Variables) != 3 || r.Variables[0].Name != ".1.3.6.1.2.1.1.2.0" || r.Variables[1].Name != oidOutletCmd+".24" ||
		r.Variables[2].Type != gosnmp.EndOfMibView {
		t.Fatalf("GETBULK n=1 m=3: %v", got)
	}
}

func TestCommunities(t *testing.T) {
	l := upLab(t)
	pdu := l.device("b01", "pdu-a")
	before := l.tw.Engine.Seq()

	bad := l.client(pdu, "guess", gosnmp.Version2c)
	bad.Timeout = 300 * time.Millisecond
	if _, err := bad.Get([]string{oidSysName}); err == nil {
		t.Fatal("an unknown community was answered")
	}
	ro := l.client(pdu, "public", gosnmp.Version2c)
	r := setInt(t, ro, oidOutletCmd+".1", cmdImmediateOff)
	if r.Error != gosnmp.NoAccess || r.ErrorIndex != 1 {
		t.Fatalf("SET with the read-only community: %v at %d", r.Error, r.ErrorIndex)
	}
	ro1 := l.client(pdu, "public", gosnmp.Version1)
	r = setInt(t, ro1, oidOutletCmd+".1", cmdImmediateOff)
	if r.Error != gosnmp.NoSuchName {
		t.Fatalf("v1 SET with the read-only community: %v", r.Error)
	}
	if v := get(t, ro, oidOutletState+".1"); v[0].Value.(int) != outletOn {
		t.Fatal("a refused SET switched the outlet")
	}
	refused := l.tw.Engine.Events(before, func(ev schema.Event) bool {
		return ev.Request != nil && ev.Request.Protocol == "snmp" && ev.Request.Method == "SET"
	})
	if len(refused) != 2 || refused[0].Request.Canonical != "power.off.force" || refused[0].Request.Status != errNoAccess ||
		refused[1].Request.Status != errNoSuchName {
		t.Fatalf("refused SETs logged as %+v", refused)
	}
}

func TestSetValidation(t *testing.T) {
	l := upLab(t)
	pdu := l.device("b01", "pdu-a")
	rw := l.client(pdu, "private", gosnmp.Version2c)
	cases := []struct {
		pdu  gosnmp.SnmpPDU
		want gosnmp.SNMPError
	}{
		{gosnmp.SnmpPDU{Name: oidSysName, Type: gosnmp.OctetString, Value: "x"}, gosnmp.NotWritable},
		{gosnmp.SnmpPDU{Name: oidOutletState + ".1", Type: gosnmp.Integer, Value: 1}, gosnmp.NotWritable},
		{gosnmp.SnmpPDU{Name: oidOutletCmd + ".1", Type: gosnmp.OctetString, Value: "off"}, gosnmp.WrongType},
		{gosnmp.SnmpPDU{Name: oidOutletCmd + ".1", Type: gosnmp.Integer, Value: 5}, gosnmp.WrongValue},
		{gosnmp.SnmpPDU{Name: oidOutletCmd + ".25", Type: gosnmp.Integer, Value: 2}, gosnmp.NoCreation},
		{gosnmp.SnmpPDU{Name: oidOutletCmd + ".1.1", Type: gosnmp.Integer, Value: 2}, gosnmp.NoCreation},
	}
	for _, c := range cases {
		r, err := rw.Set([]gosnmp.SnmpPDU{c.pdu})
		if err != nil {
			t.Fatal(err)
		}
		if r.Error != c.want || r.ErrorIndex != 1 {
			t.Fatalf("SET %s %v: %v at %d, want %v", c.pdu.Name, c.pdu.Value, r.Error, r.ErrorIndex, c.want)
		}
	}
	// a SET is all or nothing: a bad second varbind leaves the first unapplied
	r, err := rw.Set([]gosnmp.SnmpPDU{{Name: oidOutletCmd + ".2", Type: gosnmp.Integer, Value: 2}, {Name: oidOutletCmd + ".2", Type: gosnmp.Integer, Value: 9}})
	if err != nil || r.Error != gosnmp.WrongValue || r.ErrorIndex != 2 {
		t.Fatalf("mixed SET: %v %+v", err, r)
	}
	if v := get(t, rw, oidOutletState+".2"); v[0].Value.(int) != outletOn {
		t.Fatal("a refused SET applied its first varbind")
	}
}

func TestOutletCommandsDriveThePowerModel(t *testing.T) {
	l := upLab(t)
	a, b := l.device("b01", "pdu-a"), l.device("b01", "pdu-b")
	ga, gb := l.client(a, "private", gosnmp.Version2c), l.client(b, "private", gosnmp.Version2c)
	fed := l.onOutlet(a, 2)
	if !l.ac(fed) {
		t.Fatalf("%s starts without AC", fed.Path)
	}
	if r := setInt(t, ga, oidOutletCmd+".2", cmdImmediateOff); r.Error != gosnmp.NoError {
		t.Fatalf("off on pdu-a: %v", r.Error)
	}
	if !l.ac(fed) {
		t.Fatal("one PDU's outlet off should leave the redundant feed")
	}
	if r := setInt(t, gb, oidOutletCmd+".2", cmdImmediateOff); r.Error != gosnmp.NoError {
		t.Fatalf("off on pdu-b: %v", r.Error)
	}
	if l.ac(fed) {
		t.Fatal("both outlets off: the device should have lost AC")
	}
	if v := get(t, ga, oidOutletState+".2", oidOutletCmd+".2"); v[0].Value.(int) != outletOff || v[1].Value.(int) != cmdImmediateOff {
		t.Fatalf("outlet 2 reads %v %v", v[0].Value, v[1].Value)
	}
	// immediateReboot: off now, on again after the (scaled) reboot duration
	setInt(t, gb, oidOutletCmd+".2", cmdImmediateOn)
	if !l.ac(fed) {
		t.Fatal("pdu-b's outlet back on should restore AC")
	}
	if r := setInt(t, gb, oidOutletCmd+".2", cmdImmediateReboot); r.Error != gosnmp.NoError {
		t.Fatalf("reboot: %v", r.Error)
	}
	if l.ac(fed) {
		t.Fatal("rebooting the only live outlet should drop AC")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !l.ac(fed) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !l.ac(fed) {
		t.Fatal("AC did not return after immediateReboot")
	}
	evs := l.tw.Engine.Events(0, func(ev schema.Event) bool {
		return ev.Request != nil && ev.Request.Protocol == "snmp" && ev.Path == b.Path && ev.Request.Canonical == "power.cycle"
	})
	if len(evs) != 2 || evs[0].Source.Class != "stack" || evs[1].Source.Class != "behaviour" {
		t.Fatalf("reboot events: %+v", evs)
	}
}

func TestFaultsAndFeedsReachTheAgent(t *testing.T) {
	l := upLab(t)
	a, b := l.device("b01", "pdu-a"), l.device("b01", "pdu-b")
	ga := l.client(a, "public", gosnmp.Version2c)
	if _, err := l.tw.Engine.Inject(a.Path, "generic/power/outlet-off", map[string]any{"outlet": 3}, "", schema.EventSource{Class: "user", Principal: "test"}); err != nil {
		t.Fatal(err)
	}
	if v := get(t, ga, oidOutletState+".3"); v[0].Value.(int) != outletOff {
		t.Fatalf("outlet-off fault: outlet 3 reads %v", v[0].Value)
	}
	// feed A down takes pdu-a's management card with it: no answer; pdu-b still answers
	if err := l.tw.Engine.SetFeed("A", false, schema.EventSource{Class: "user", Principal: "test"}); err != nil {
		t.Fatal(err)
	}
	ga.Timeout = 300 * time.Millisecond
	if _, err := ga.Get([]string{oidSysName}); err == nil {
		t.Fatal("a PDU without AC answered")
	}
	gb := l.client(b, "public", gosnmp.Version2c)
	get(t, gb, oidSysName)
	if err := l.tw.Engine.SetFeed("A", true, schema.EventSource{Class: "user", Principal: "test"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var r *gosnmp.SnmpPacket
	var err error
	for time.Now().Before(deadline) {
		if r, err = ga.Get([]string{oidSysUpTime}); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal("pdu-a did not answer again after its feed returned")
	}
	if up := r.Variables[0].Value.(uint32); up > 500 {
		t.Fatalf("sysUpTime %d after the card rebooted: it should count from the reboot", up)
	}
}

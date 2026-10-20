package snmp

import (
	"bytes"
	"io"
	"log"
	"math"
	"strings"
	"testing"

	"github.com/gosnmp/gosnmp"
)

func TestOIDOrderIsNumericNotTextual(t *testing.T) {
	ordered := []string{
		"1.3.6.1.2.1.1",
		"1.3.6.1.2.1.1.1.0",
		"1.3.6.1.2.1.1.2.0",
		"1.3.6.1.2.1.1.10.0", // after .2 numerically, before it textually
		"1.3.6.1.4.1.318.1.1.26.9.2.3.1.3.2",
		"1.3.6.1.4.1.318.1.1.26.9.2.3.1.3.10",
		"1.3.6.1.4.1.318.1.1.26.9.2.3.1.5.1",
		"1.3.6.1.4.1.4294967295",
	}
	for i := 0; i+1 < len(ordered); i++ {
		a, b := mustOID(ordered[i]), mustOID(ordered[i+1])
		if a.Compare(b) >= 0 || b.Compare(a) <= 0 {
			t.Fatalf("%s should sort before %s", a, b)
		}
	}
	if mustOID(".1.3.6").Compare(mustOID("1.3.6")) != 0 {
		t.Fatal("a leading dot is not part of the OID")
	}
}

func TestViewGetAndNext(t *testing.T) {
	v := &view{}
	col := mustOID("1.3.6.1.4.1.318.1.1.26.9.2.3.1.5")
	for _, n := range []uint32{10, 2, 1} {
		v.add(col, OID{n}, intValue(int64(n)), "health.read")
	}
	v.add(sysDescr, OID{0}, strValue("d"), "inventory.read")
	v.sort()
	if _, exc := v.get(sysDescr); exc != tagNoSuchInstance {
		t.Fatalf("sysDescr without .0 is the object, not an instance: got 0x%x", exc)
	}
	if _, exc := v.get(col.Append(3)); exc != tagNoSuchInstance {
		t.Fatalf("a missing row of a served column: got 0x%x", exc)
	}
	if _, exc := v.get(mustOID("1.3.6.1.2.1.2.1.0")); exc != tagNoSuchObject {
		t.Fatalf("an unserved object: got 0x%x", exc)
	}
	var walked []string
	for o := mustOID("1"); ; {
		en, ok := v.next(o)
		if !ok {
			break
		}
		walked = append(walked, en.oid.String())
		o = en.oid
	}
	want := "1.3.6.1.2.1.1.1.0 1.3.6.1.4.1.318.1.1.26.9.2.3.1.5.1 1.3.6.1.4.1.318.1.1.26.9.2.3.1.5.2 1.3.6.1.4.1.318.1.1.26.9.2.3.1.5.10"
	if got := strings.Join(walked, " "); got != want {
		t.Fatalf("walk order\n got %s\nwant %s", got, want)
	}
}

// gosnmp is an independent BER implementation: what it encodes we must decode, and what we
// encode it must decode to the same values.
func TestBERAgainstGosnmp(t *testing.T) {
	long := strings.Repeat("x", 300) // forces a long-form length
	req := &gosnmp.SnmpPacket{Version: gosnmp.Version2c, Community: "private", PDUType: gosnmp.SetRequest, RequestID: 0x7ffffff0,
		Variables: []gosnmp.SnmpPDU{
			{Name: ".1.3.6.1.4.1.318.1.1.26.9.2.4.1.5.24", Type: gosnmp.Integer, Value: 2},
			{Name: ".1.3.6.1.2.1.1.4.0", Type: gosnmp.OctetString, Value: long},
			{Name: ".1.3.6.1.4.1.4294967295.0", Type: gosnmp.Integer, Value: -129},
		}}
	raw, err := req.MarshalMsg()
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if m.version != versionV2c || string(m.community) != "private" || m.pdu != pduSet || m.requestID != 0x7ffffff0 || len(m.vbs) != 3 {
		t.Fatalf("header: %+v", m)
	}
	if m.vbs[0].oid.String() != "1.3.6.1.4.1.318.1.1.26.9.2.4.1.5.24" || m.vbs[0].val.tag != tagInteger || m.vbs[0].val.i != 2 {
		t.Fatalf("varbind 0: %+v", m.vbs[0])
	}
	if m.vbs[1].val.tag != tagOctetString || string(m.vbs[1].val.b) != long {
		t.Fatalf("varbind 1: tag 0x%x len %d", m.vbs[1].val.tag, len(m.vbs[1].val.b))
	}
	if m.vbs[2].oid.String() != "1.3.6.1.4.1.4294967295.0" || m.vbs[2].val.i != -129 {
		t.Fatalf("varbind 2: %+v", m.vbs[2])
	}

	resp := &message{version: versionV2c, community: []byte("public"), pdu: pduResponse, requestID: 12345, vbs: []varbind{
		{oid: mustOID("1.3.6.1.2.1.1.3.0"), val: ticksValue(math.MaxUint32)},
		{oid: mustOID("1.3.6.1.2.1.1.2.0"), val: oidValue(mustOID("1.3.6.1.4.1.318.1.3.4.6"))},
		{oid: mustOID("1.3.6.1.2.1.1.5.0"), val: strValue(long)},
		{oid: mustOID("1.3.6.1.2.1.1.9.0"), val: exception(tagNoSuchObject)},
		{oid: mustOID("1.3.6.1.2.1.1.9.1"), val: exception(tagNoSuchInstance)},
		{oid: mustOID("1.9"), val: exception(tagEndOfMibView)},
		{oid: mustOID("1.3.6.1.4.1.1.0"), val: intValue(math.MinInt32)},
		{oid: mustOID("1.3.6.1.4.1.2.0"), val: intValue(128)},
	}}
	out := resp.encode()
	var vbl int
	for _, vb := range resp.vbs {
		vbl += len(encodeVarbind(vb))
	}
	if resp.sizeWith(vbl) != len(out) {
		t.Fatalf("sizeWith %d != encoded %d", resp.sizeWith(vbl), len(out))
	}
	g := &gosnmp.GoSNMP{Version: gosnmp.Version2c, Community: "public", Logger: gosnmp.NewLogger(log.New(io.Discard, "", 0))}
	p, err := g.SnmpDecodePacket(out)
	if err != nil {
		t.Fatal(err)
	}
	if p.PDUType != gosnmp.GetResponse || p.RequestID != 12345 {
		t.Fatalf("header as gosnmp reads it: %v %d", p.PDUType, p.RequestID)
	}
	wantTypes := []gosnmp.Asn1BER{gosnmp.TimeTicks, gosnmp.ObjectIdentifier, gosnmp.OctetString, gosnmp.NoSuchObject, gosnmp.NoSuchInstance, gosnmp.EndOfMibView, gosnmp.Integer, gosnmp.Integer}
	if len(p.Variables) != len(wantTypes) {
		t.Fatalf("gosnmp decoded %d varbinds", len(p.Variables))
	}
	for i, w := range wantTypes {
		if p.Variables[i].Type != w {
			t.Fatalf("varbind %d: type %v, want %v", i, p.Variables[i].Type, w)
		}
	}
	if p.Variables[0].Value.(uint32) != math.MaxUint32 {
		t.Fatalf("timeticks: %v", p.Variables[0].Value)
	}
	if p.Variables[1].Value.(string) != ".1.3.6.1.4.1.318.1.3.4.6" {
		t.Fatalf("oid value: %v", p.Variables[1].Value)
	}
	if !bytes.Equal(p.Variables[2].Value.([]byte), []byte(long)) {
		t.Fatal("long string did not survive")
	}
	if p.Variables[6].Value.(int) != math.MinInt32 || p.Variables[7].Value.(int) != 128 {
		t.Fatalf("integers: %v %v", p.Variables[6].Value, p.Variables[7].Value)
	}
}

func TestMalformedIsRefused(t *testing.T) {
	for _, b := range [][]byte{
		nil,
		{0x30},
		{0x30, 0x80, 0x00, 0x00},             // indefinite length
		{0x30, 0x05, 0x02, 0x01, 0x01, 0x04}, // truncated
		{0x30, 0x03, 0x02, 0x01, 0x03},       // v3-shaped: no community
		{0x30, 0x84, 0xff, 0xff, 0xff, 0xff}, // absurd length
	} {
		if _, err := decodeMessage(b); err == nil {
			t.Fatalf("% x decoded", b)
		}
	}
}

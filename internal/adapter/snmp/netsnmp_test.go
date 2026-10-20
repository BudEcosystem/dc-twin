package snmp

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// TestNetSNMPEndToEnd drives the lab estate's PDUs with the real net-snmp command-line tools,
// as an operator or a stack's poller would: numeric OIDs, because the PowerNet MIB is not
// installed on the host.
func TestNetSNMPEndToEnd(t *testing.T) {
	for _, tool := range []string{"snmpget", "snmpwalk", "snmpbulkwalk", "snmpset"} {
		if _, err := os.Stat("/usr/bin/" + tool); err != nil {
			t.Skipf("net-snmp CLI not installed: /usr/bin/%s (%v)", tool, err)
		}
	}
	l := upLab(t)
	a, b := l.device("b01", "pdu-a"), l.device("b01", "pdu-b")
	epA, epB := l.a.Endpoint(a.Path), l.a.Endpoint(b.Path)
	t.Logf("b01 pdu-a at udp:%s, pdu-b at udp:%s", epA, epB)

	run := func(tool string, args ...string) (string, error) {
		t.Helper()
		out, err := exec.Command("/usr/bin/"+tool, args...).CombinedOutput()
		t.Logf("$ %s %s\n%s", tool, strings.Join(args, " "), strings.TrimRight(string(out), "\n"))
		return string(out), err
	}
	must := func(tool string, args ...string) string {
		t.Helper()
		out, err := run(tool, args...)
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return out
	}
	// lines counts varbinds; net-snmp ends a walk that reaches endOfMibView with a line saying so
	lines := func(s string) int {
		n := 0
		for _, ln := range strings.Split(s, "\n") {
			if strings.Contains(ln, " = ") && !strings.Contains(ln, "No more variables left in this MIB View") {
				n++
			}
		}
		return n
	}

	// identity
	out := must("snmpget", "-v2c", "-c", "public", "-On", epA, oidSysDescr, oidSysObjectID, oidSysUpTime, oidSysName, oidSysLocation)
	for _, want := range []string{"dc-twin model PDU", "= OID: .1.3.6.1.4.1.318.1.3.4.6", ".1.3.6.1.2.1.1.5.0 = STRING: pdu-a\n", a.RackOf().Path} {
		if !strings.Contains(out, want) {
			t.Fatalf("snmpget: no %q", want)
		}
	}
	// the PowerNet subtree: four device objects and three outlet columns of 24
	out = must("snmpwalk", "-v2c", "-c", "public", "-On", epA, ".1.3.6.1.4.1.318.1.1.26")
	if n := lines(out); n != 4+3*24 {
		t.Fatalf("PowerNet walk: %d lines", n)
	}
	// the whole agent, three ways; net-snmp refuses a walk whose OIDs do not increase
	full := must("snmpwalk", "-v2c", "-c", "public", "-On", epA, ".1")
	bulk := must("snmpbulkwalk", "-v2c", "-c", "public", "-On", epA, ".1")
	v1 := must("snmpwalk", "-v1", "-c", "public", "-On", epA, ".1")
	for name, w := range map[string]string{"snmpwalk": full, "snmpbulkwalk": bulk, "snmpwalk -v1": v1} {
		if strings.Contains(w, "OID not increasing") || lines(w) != 7+4+3*24 {
			t.Fatalf("%s: %d lines, or OIDs out of order", name, lines(w))
		}
	}

	// the read-only community cannot switch an outlet; an unknown community gets nothing
	out, err := run("snmpset", "-v2c", "-c", "public", "-On", epA, oidOutletCmd+".1", "i", "2")
	if err == nil || !strings.Contains(out, "noAccess") {
		t.Fatalf("snmpset with public: err %v", err)
	}
	out, err = run("snmpget", "-v2c", "-c", "not-a-community", "-t", "1", "-r", "0", "-On", epA, oidSysName)
	if err == nil || !strings.Contains(out, "Timeout") {
		t.Fatalf("snmpget with an unknown community: err %v", err)
	}

	outletState := func(ep string, n int) string {
		t.Helper()
		return strings.TrimSpace(must("snmpget", "-v2c", "-c", "public", "-Oqv", ep, fmt.Sprintf("%s.%d", oidOutletState, n)))
	}
	set := func(ep string, n, cmd int) {
		t.Helper()
		must("snmpset", "-v2c", "-c", "private", "-On", ep, fmt.Sprintf("%s.%d", oidOutletCmd, n), "i", fmt.Sprint(cmd))
	}
	acOf := func(d *model.Device) bool {
		ac := l.ac(d)
		t.Logf("engine view: %s ac=%v power=%v", d.Path, ac, l.tw.Engine.DeviceView(d)["power"])
		return ac
	}

	// outlet 1 of each PDU, then the outlet of the first server: both PDUs must be off to drop AC
	server := l.device("b01", "server-01")
	for _, n := range []int{1, outletOf(t, l, a, server)} {
		fed := l.onOutlet(a, n)
		t.Logf("--- outlet %d of b01 feeds %s", n, fed.Path)
		since := l.tw.Engine.Seq()
		if s := outletState(epA, n); s != "2" {
			t.Fatalf("outlet %d starts %s", n, s)
		}
		set(epA, n, cmdImmediateOff)
		if outletState(epA, n) != "1" || !acOf(fed) {
			t.Fatalf("pdu-a outlet %d off: %s should keep AC through pdu-b", n, fed.Name)
		}
		set(epB, n, cmdImmediateOff)
		if outletState(epB, n) != "1" || acOf(fed) {
			t.Fatalf("both outlets %d off: %s should have lost AC", n, fed.Name)
		}
		offs := l.tw.Engine.Events(since, func(ev schema.Event) bool {
			return ev.Request != nil && ev.Request.Protocol == "snmp" && ev.Request.Canonical == "power.off.force"
		})
		if len(offs) != 2 || offs[0].Path != a.Path || offs[1].Path != b.Path {
			t.Fatalf("event log: %d power.off.force requests from snmp", len(offs))
		}
		for _, ev := range offs {
			t.Logf("event %d: kind=%s path=%s source=%s/%s protocol=%s method=%s target=%s canonical=%s",
				ev.Seq, ev.Kind, ev.Path, ev.Source.Class, ev.Source.Principal, ev.Request.Protocol, ev.Request.Method, ev.Request.Target, ev.Request.Canonical)
		}
		lost := l.tw.Engine.Events(since, func(ev schema.Event) bool {
			return ev.Path == fed.Path && ev.State != nil && ev.State.Field == "ac"
		})
		if len(lost) != 1 || lost[0].State.To != false {
			t.Fatalf("expected one ac -> false on %s, got %+v", fed.Path, lost)
		}
		t.Logf("event %d: %s ac %v -> %v (cause %d)", lost[0].Seq, lost[0].Path, lost[0].State.From, lost[0].State.To, lost[0].Cause)
		set(epA, n, cmdImmediateOn)
		set(epB, n, cmdImmediateOn)
		if outletState(epA, n) != "2" || outletState(epB, n) != "2" || !acOf(fed) {
			t.Fatalf("outlets %d on: %s should have AC again", n, fed.Name)
		}
	}
}

// outletOf is the outlet that feeds a device.
func outletOf(t *testing.T, l *lab, pdu, d *model.Device) int {
	t.Helper()
	for n := 1; n <= 24; n++ {
		if l.onOutlet(pdu, n) == d {
			return n
		}
	}
	t.Fatalf("%s is on no outlet of %s", d.Path, pdu.Path)
	return 0
}

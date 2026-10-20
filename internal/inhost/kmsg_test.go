package inhost

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

func TestParseRecord(t *testing.T) {
	r, err := ParseRecord("12,295,1404946,-;dc-twin guest: up")
	if err != nil {
		t.Fatal(err)
	}
	if r.Prio != 12 || r.Seq != 295 || r.Usec != 1404946 || r.Flags != "-" || r.Text != "dc-twin guest: up" || r.Level() != 4 || r.Facility() != 1 {
		t.Fatalf("%+v", r)
	}
	// the header may carry more fields after the flags (kernels since 3.6 may add them)
	if r, err := ParseRecord("6,0,0,-,caller=T0;Booting Linux"); err != nil || r.Text != "Booting Linux" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"no semicolon", "x,1,2,-;m", "6,1;m", "1000,1,2,-;m"} {
		if _, err := ParseRecord(bad); err == nil {
			t.Errorf("ParseRecord(%q) accepted", bad)
		}
	}
}

// TestJournalEntry checks the mapping against journald's (journald-kmsg.c) for the three cases
// the twin's guest produces: a kernel printk, a userspace line through /dev/kmsg, and a fault's
// line the twin wrote, which is entered as a driver's printk would be and says it is the twin's.
func TestJournalEntry(t *testing.T) {
	now := time.Unix(1760000000, 123456000)
	boot := "e046e23f-78ca-4e11-9eea-1a2206b4b8db"

	k, _ := ParseRecord("6,290,352530,-;input: Power Button as /devices/LNXSYSTM:00")
	e := JournalEntry(k, boot, "a01-compute-tray-01", now, false, "", "")
	for f, want := range map[string]string{
		"_TRANSPORT": "kernel", "PRIORITY": "6", "SYSLOG_FACILITY": "0", "SYSLOG_IDENTIFIER": "kernel",
		"MESSAGE": "input: Power Button as /devices/LNXSYSTM:00", "_SOURCE_MONOTONIC_TIMESTAMP": "352530",
		"_BOOT_ID": "e046e23f78ca4e119eea1a2206b4b8db", "_HOSTNAME": "a01-compute-tray-01", "DCTWIN_FAULT": "",
	} {
		if got := e.Field(f); got != want {
			t.Errorf("kernel record: %s=%q, want %q", f, got, want)
		}
	}

	// A userspace writer's "identifier[pid]:" becomes SYSLOG_IDENTIFIER and SYSLOG_PID.
	u, _ := ParseRecord("30,301,9000000,-;systemd[1]: Started journald.")
	e = JournalEntry(u, boot, "n", now, false, "", "")
	if e.Field("SYSLOG_FACILITY") != "3" || e.Field("SYSLOG_IDENTIFIER") != "systemd" || e.Field("SYSLOG_PID") != "1" || e.Field("MESSAGE") != "Started journald." {
		t.Fatalf("userspace record: %+v", e.Fields)
	}
	// ... which is why a driver's line written from userspace would lose its "NVRM:" to journald
	x, _ := ParseRecord("12,297,30186828,-;NVRM: Xid (PCI:0000:38:00.0): 79, pid=*, GPU has fallen off the bus.")
	if e := JournalEntry(x, boot, "n", now, false, "", ""); e.Field("SYSLOG_IDENTIFIER") != "NVRM" || strings.HasPrefix(e.Field("MESSAGE"), "NVRM:") {
		t.Fatalf("journald's own reading of a userspace NVRM line: %+v", e.Fields)
	}
	// and why the twin enters the line it wrote for a fault as the driver's printk
	e = JournalEntry(x, boot, "n", now, true, "nvidia/xid/xid-79", "f1")
	for f, want := range map[string]string{
		"SYSLOG_FACILITY": "0", "SYSLOG_IDENTIFIER": "kernel", "PRIORITY": "4",
		"MESSAGE":      "NVRM: Xid (PCI:0000:38:00.0): 79, pid=*, GPU has fallen off the bus.",
		"DCTWIN_FAULT": "nvidia/xid/xid-79", "DCTWIN_FAULT_ID": "f1", "DCTWIN_KMSG_FACILITY": "1",
	} {
		if got := e.Field(f); got != want {
			t.Errorf("fault line: %s=%q, want %q", f, got, want)
		}
	}

	esc, _ := ParseRecord(`6,1,1,-;tab\x09and\x0anewline \x zz`)
	if got := JournalEntry(esc, boot, "n", now, false, "", "").Field("MESSAGE"); got != "tab\tand\nnewline \\x zz" {
		t.Fatalf("unescape: %q", got)
	}
}

func TestExportFormat(t *testing.T) {
	e := Entry{Realtime: time.UnixMicro(1760000000123456), Monotonic: 42, BootID: "b",
		Fields: [][2]string{{"MESSAGE", "one\ntwo"}, {"PRIORITY", "4"}}}
	got := e.Export()
	var want bytes.Buffer
	want.WriteString("__REALTIME_TIMESTAMP=1760000000123456\n__MONOTONIC_TIMESTAMP=42\nMESSAGE\n")
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], 7)
	want.Write(n[:])
	want.WriteString("one\ntwo\nPRIORITY=4\n\n")
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("export:\n%q\nwant\n%q", got, want.Bytes())
	}
}

func TestRender(t *testing.T) {
	line := "NVRM: Xid (PCI:{pci}): 79, pid=*, GPU has fallen off the bus."
	if got := render(line, nil, "gpu-2", 2, "GPU-x"); got != "NVRM: Xid (PCI:0000:38:00.0): 79, pid=*, GPU has fallen off the bus." {
		t.Fatalf("got %q", got)
	}
	if got := render("{component} lane {lane} {unknown}", map[string]any{"lane": 3}, "gpu-0", 0, ""); got != "gpu-0 lane 3 {unknown}" {
		t.Fatalf("got %q", got)
	}
	// the bus numbering every projection shares (the dcgm-exporter's pci_bus_id is 00000000:18:00.0 for gpu 0)
	if PCIAddress(0) != "0000:18:00.0" || PCIAddress(7) != "0000:88:00.0" {
		t.Fatalf("%s %s", PCIAddress(0), PCIAddress(7))
	}
}

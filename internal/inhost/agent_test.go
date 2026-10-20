package inhost

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFrame(t *testing.T) {
	rec := "4,297,30186828,-;NVRM: Xid (PCI:0000:38:00.0): 79, pid=*, GPU has fallen off the bus."
	enc := base64.StdEncoding.EncodeToString([]byte(rec))
	cases := []struct {
		line string
		ok   bool
		kind string
	}{
		{"@@dc-twin hello boot=e046e23f-78ca-4e11-9eea-1a2206b4b8db kernel=6.18.52-0-virt arch=aarch64 cpus=2 memKiB=985756", true, "hello"},
		{"@@dc-twin alive boot=e046e23f\r", true, "alive"},
		{"@@dc-twin kmsg " + enc, true, "kmsg"},
		// a kernel message the console printed ahead of the agent's frame on the same line
		{"[   30.18] NVRM: something@@dc-twin kmsg " + enc, true, "kmsg"},
		{"@@dc-twin kmsg not-base64!", false, ""},
		{"@@dc-twin attach", false, ""}, // the twin's own line
		{"[    0.000000] Booting Linux on physical CPU 0x0", false, ""},
	}
	for _, c := range cases {
		f, ok := ParseFrame(c.line)
		if ok != c.ok || f.Kind != c.kind {
			t.Errorf("ParseFrame(%q) = %+v, %v; want kind %q, %v", c.line, f, ok, c.kind, c.ok)
		}
	}
	f, _ := ParseFrame("@@dc-twin kmsg " + enc)
	if f.Record != rec {
		t.Fatalf("kmsg record %q, want %q", f.Record, rec)
	}
	h, _ := ParseFrame(cases[0].line)
	if h.Fields["boot"] != "e046e23f-78ca-4e11-9eea-1a2206b4b8db" || h.Fields["memKiB"] != "985756" {
		t.Fatalf("hello fields %v", h.Fields)
	}
	// The boot log carried again must never read as the guest's banner or ready line: the vm
	// adapter's boot observer watches the same console for those.
	for _, s := range []string{"Booting Linux on physical CPU", "] Linux version ", "dc-twin guest: up", "TPMEventLog="} {
		if strings.Contains("@@dc-twin kmsg "+base64.StdEncoding.EncodeToString([]byte("6,0,0,-;"+s)), s) {
			t.Fatalf("a framed record still contains %q", s)
		}
	}
}

func TestKmsgWriteLine(t *testing.T) {
	got := KmsgWriteLine(4, "NVRM: Xid (PCI:0000:38:00.0): 79\n\x1b[31mevil")
	want := "@@dc-twin kmsg-write <4>NVRM: Xid (PCI:0000:38:00.0): 79??[31mevil\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := KmsgWriteLine(9, "x"); got != "@@dc-twin kmsg-write <4>x\n" {
		t.Fatalf("an out-of-range level: %q", got)
	}
	if AttachLine() != "@@dc-twin attach\n" {
		t.Fatalf("attach line %q", AttachLine())
	}
}

// TestGuestAgentScript checks the guest's half parses as a POSIX shell script (busybox ash runs
// it) and finds its console as the kernel names it.
func TestGuestAgentScript(t *testing.T) {
	s := GuestAgent()
	for _, want := range []string{"/sys/class/tty/console/active", "\"@@dc-twin attach\")", "\"@@dc-twin kmsg-write \"*)", "> /dev/kmsg", "cat /dev/kmsg", "base64", "sleep 5", Listening, "inhost_agent &", "insmod /" + KernelModule, KernelModuleFile, "kmod=$kmod"} {
		if !strings.Contains(s, want) {
			t.Errorf("the guest agent has no %q", want)
		}
	}
	if strings.Contains(s, "@M@") || strings.Contains(s, "@HB@") {
		t.Fatal("an unreplaced placeholder")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to check the script's syntax")
	}
	p := filepath.Join(t.TempDir(), "agent.sh")
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sh, "-n", p).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
}

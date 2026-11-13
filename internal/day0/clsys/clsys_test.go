package clsys

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func inDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	pf, rf := PendingFile, RebootFile
	PendingFile, RebootFile = filepath.Join(d, "onie", "pending.json"), filepath.Join(d, "reboot.json")
	t.Cleanup(func() { PendingFile, RebootFile = pf, rf })
	return d
}

var now = func() time.Time { return time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC) }

// NVIDIA Config Manager's call (onie-install -fa -i <url>) leaves the install for the next boot;
// what the twin does not emulate is refused by name.
func TestOnieInstall(t *testing.T) {
	inDir(t)
	var out, errb bytes.Buffer
	if rc := onieInstall([]string{"-fa", "-i", "http://192.0.2.10/v1/device/x/firmware"}, &out, &errb, now); rc != 0 {
		t.Fatalf("onie-install = %d: %s", rc, errb.String())
	}
	var p Pending
	b, err := os.ReadFile(PendingFile)
	if err != nil || json.Unmarshal(b, &p) != nil || p.URL != "http://192.0.2.10/v1/device/x/firmware" {
		t.Fatalf("pending: %s %v", b, err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-f", "-i", "http://a/b"}, "-a is needed"},
		{[]string{"-fa"}, "only an install from an image URL"},
		{[]string{"-fa", "-i", "ftp://a/b"}, "not an http or https URL"},
		{[]string{"-fa", "-i", "http://a/b", "-z", "nope"}, "not an http or https URL"},
		{[]string{"-fa", "-i", "http://a/b", "extra"}, "unexpected"},
	} {
		errb.Reset()
		if rc := onieInstall(c.args, &out, &errb, now); rc == 0 || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%v: %d %q, want %q", c.args, rc, errb.String(), c.want)
		}
	}
}

// reboot asks the twin and waits for it; it fails when the twin never acts.
func TestReboot(t *testing.T) {
	inDir(t)
	RebootWait = 3 * time.Second
	defer func() { RebootWait = 10 * time.Minute }()
	var out, errb bytes.Buffer
	slept := 0
	if rc := reboot(nil, &out, &errb, now, func(time.Duration) { slept++ }); rc != 1 || slept != 3 || !strings.Contains(errb.String(), "did not reboot") {
		t.Fatalf("reboot = %d after %d s: %s", rc, slept, errb.String())
	}
	var r Reboot
	b, _ := os.ReadFile(RebootFile)
	if json.Unmarshal(b, &r) != nil || r.Install {
		t.Fatalf("request without an install pending: %s", b)
	}
	if rc := onieInstall([]string{"-fa", "-i", "http://a/b"}, &out, &errb, now); rc != 0 {
		t.Fatal(errb.String())
	}
	_ = reboot([]string{"-f"}, &out, &errb, now, func(time.Duration) {})
	b, _ = os.ReadFile(RebootFile)
	if json.Unmarshal(b, &r) != nil || !r.Install {
		t.Fatalf("request with an install pending: %s", b)
	}
	if rc := reboot([]string{"--halt"}, &out, &errb, now, func(time.Duration) {}); rc != 2 {
		t.Errorf("reboot --halt = %d", rc)
	}
}

// The release os-release says: ONIE's installed image's, else the switch's identity's.
func TestRelease(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"etc/dctwin", "var/lib/dctwin/onie", "etc"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(root, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("etc/dctwin/identity.json", `{"version": "5.13.1"}`)
	if v := Release(root); v != "5.13.1" {
		t.Errorf("preinstalled: %q", v)
	}
	write("var/lib/dctwin/onie/status.json", `{"phase": "installing", "version": "5.14.0"}`)
	if v := Release(root); v != "5.13.1" {
		t.Errorf("still installing: %q", v)
	}
	write("var/lib/dctwin/onie/status.json", `{"phase": "installed", "version": "5.14.0"}`)
	if v := Release(root); v != "5.14.0" {
		t.Errorf("installed: %q", v)
	}
	if rc := OSRelease([]string{"-root", root}); rc != 0 {
		t.Fatal(rc)
	}
	b, _ := os.ReadFile(filepath.Join(root, "etc/os-release"))
	if !strings.Contains(string(b), "VERSION_ID=5.14.0\n") || !strings.Contains(string(b), `NAME="Cumulus Linux"`) {
		t.Errorf("os-release:\n%s", b)
	}
}

func TestDecodeSyseeprom(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "identity.json"), []byte(`{"serial":"MT2245X03839","baseMAC":"44:38:39:00:01:01","model":"SN5600","manufacturer":"NVIDIA"}`), 0o644)
	var out, errb bytes.Buffer
	if rc := decodeSyseeprom([]string{"-s"}, d, &out, &errb); rc != 0 || out.String() != "MT2245X03839\n" {
		t.Errorf("-s: %d %q", rc, out.String())
	}
	out.Reset()
	if rc := decodeSyseeprom(nil, d, &out, &errb); rc != 0 || !strings.Contains(out.String(), "Serial Number        0x23  12 MT2245X03839") {
		t.Errorf("table: %d\n%s", rc, out.String())
	}
	if rc := decodeSyseeprom([]string{"-x"}, d, &out, &errb); rc != 2 {
		t.Errorf("-x: %d", rc)
	}
}

func TestTimedatectl(t *testing.T) {
	var out, errb bytes.Buffer
	if rc := timedatectl([]string{"show", "-p", "NTPSynchronized"}, "Reference ID    : 0A5A010A\nLeap status     : Normal\n", nil, &out, &errb); rc != 0 || out.String() != "NTPSynchronized=yes\n" {
		t.Errorf("synced: %d %q", rc, out.String())
	}
	out.Reset()
	if rc := timedatectl([]string{"show", "-p", "NTPSynchronized", "--value"}, "Leap status     : Not synchronised\n", nil, &out, &errb); rc != 0 || out.String() != "no\n" {
		t.Errorf("not synced: %d %q", rc, out.String())
	}
	if rc := timedatectl([]string{"set-ntp", "true"}, "", nil, &out, &errb); rc != 2 {
		t.Errorf("set-ntp: %d", rc)
	}
}

// docker exec asks the twin to run the command in the named service container and passes its
// output and exit code through; other docker commands are not emulated.
func TestDockerExec(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "x.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			var q ExecRequest
			json.NewDecoder(c).Decode(&q)
			r := ExecReply{Output: q.Container + ": " + strings.Join(q.Cmd, " ") + "\n", Code: 3}
			if q.Container != "lldp" {
				r = ExecReply{Code: 1, Error: "Error: No such container: " + q.Container}
			}
			json.NewEncoder(c).Encode(r)
			c.Close()
		}
	}()
	var out, errb bytes.Buffer
	if rc := docker([]string{"exec", "-i", "lldp", "lldpctl", "-f", "json"}, sock, &out, &errb); rc != 3 || out.String() != "lldp: lldpctl -f json\n" {
		t.Errorf("exec: %d %q %q", rc, out.String(), errb.String())
	}
	out.Reset()
	errb.Reset()
	if rc := docker([]string{"exec", "nope", "true"}, sock, &out, &errb); rc != 1 || !strings.Contains(errb.String(), "No such container") {
		t.Errorf("no container: %d %q", rc, errb.String())
	}
	if rc := docker([]string{"ps"}, sock, &out, &errb); rc != 1 {
		t.Errorf("ps: %d", rc)
	}
}

// sudo takes `sudo -S -p ” cmd` with the password on stdin's first line, which the command does
// not see; -u is root's only; an option it does not model is refused.
func TestSudoOptions(t *testing.T) {
	var got []string
	var rest string
	run := func(cmd []string, in io.Reader) int {
		got = cmd
		b, _ := io.ReadAll(in)
		rest = string(b)
		return 7
	}
	var errb bytes.Buffer
	if rc := sudo([]string{"-S", "-p", "", "config", "load", "-y", "/tmp/c.json"}, strings.NewReader("secret\n{\"x\":1}\n"), &errb, run); rc != 7 ||
		strings.Join(got, " ") != "config load -y /tmp/c.json" || rest != "{\"x\":1}\n" {
		t.Errorf("-S -p: rc %d cmd %v stdin %q", rc, got, rest)
	}
	if rc := sudo([]string{"-u", "admin", "id"}, strings.NewReader(""), &errb, run); rc != 1 {
		t.Errorf("-u admin: %d", rc)
	}
	if rc := sudo([]string{"-Z", "id"}, strings.NewReader(""), &errb, run); rc != 1 {
		t.Errorf("-Z: %d", rc)
	}
}

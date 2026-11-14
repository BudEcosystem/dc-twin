package ztp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// testConfig is a config rooted in a temporary directory, with discovery's timers shrunk.
func testConfig(t *testing.T) (*config, string) {
	t.Helper()
	d := t.TempDir()
	p := func(s string) string { return filepath.Join(d, s) }
	c := defaultConfig()
	c.identityFile, c.factoryFile = p("etc/dctwin/identity.json"), p("etc/dctwin/factory.json")
	c.dhcpEnv, c.userApplied = p("run/dctwin/dhcp-eth0.env"), p("run/dctwin/nvue-user-applied")
	c.statusFile, c.daemonFile = p("var/lib/dctwin/ztp/status.json"), p("var/lib/dctwin/ztp/daemon.json")
	c.lockFile, c.tmpDir = p("run/ztp.lock"), p("tmp")
	c.faultsDir, c.onieStatus = p("etc/dctwin/faults"), p("var/lib/dctwin/onie/status.json")
	c.vrfExists = func(string) bool { return false }
	c.clDir, c.autoprovLog = p("var/lib/cumulus/ztp"), p("var/log/autoprovision")
	c.ztpDir, c.ztpLog, c.sectionsDir, c.sonicDir = p("host/ztp"), p("var/log/ztp.log"), p("var/lib/ztp/sections"), p("etc/sonic")
	c.interval, c.window = 5*time.Millisecond, 150*time.Millisecond
	c.dumpMetaCmd = ""
	c.hostname = func() (string, error) { return "leaf01.example", nil }
	c.stdin = strings.NewReader("")
	c.stdout, c.stderr = &bytes.Buffer{}, &bytes.Buffer{}
	c.quiet = true
	id := nos.Identity{Serial: "XYZ123004", BaseMAC: "44:38:39:ff:40:94", MgmtMAC: "44:38:39:ff:00:00",
		Manufacturer: "Mellanox", Model: "MSN2700", Platform: "x86_64-mlnx_msn2700-r0", Arch: "x86_64",
		NOS: "cumulus", Version: "5.11.0"}
	if err := writeJSON(c.identityFile, id); err != nil {
		t.Fatal(err)
	}
	return c, d
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func status(t *testing.T, c *config) twinStatus {
	t.Helper()
	return c.readStatus()
}

func ctxT(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestParseEnv(t *testing.T) {
	l := parseEnv(`reason='BOUND'
new_ip_address='192.0.2.10'
export new_routers="192.0.2.1"
new_domain_name_servers='192.0.2.53 192.0.2.54'
new_cumulus_provision_url='http://192.0.2.1/ztp.sh?a=1&b=2'
new_host_name='it'\''s'
new_bootfile_name=bare\ word # trailing comment
new_filename="a \"quoted\" \$x"
not an assignment
1bad=x
`)
	want := map[string]string{
		"reason": "BOUND", "new_ip_address": "192.0.2.10", "new_routers": "192.0.2.1",
		"new_domain_name_servers": "192.0.2.53 192.0.2.54", "new_cumulus_provision_url": "http://192.0.2.1/ztp.sh?a=1&b=2",
		"new_host_name": "it's", "new_bootfile_name": "bare word", "new_filename": `a "quoted" $x`,
	}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("%s = %q, want %q", k, l[k], v)
		}
	}
	if len(l) != len(want) {
		t.Errorf("parsed %d keys, want %d: %v", len(l), len(want), l)
	}
	if got := l.option(opt239); got != want["new_cumulus_provision_url"] {
		t.Errorf("option 239 = %q", got)
	}
	l["reason"] = "EXPIRE"
	if l.option(opt239) != "" {
		t.Error("an expired lease still carries option 239")
	}
}

// clServer serves a Cumulus script and records the requests' headers.
type clServer struct {
	*httptest.Server
	mu      sync.Mutex
	headers []http.Header
	fails   atomic.Int32 // the next n requests get a 500
	body    atomic.Value
}

func newCLServer(t *testing.T, body string) *clServer {
	s := &clServer{}
	s.body.Store(body)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		if s.fails.Add(-1) >= 0 {
			http.Error(w, "not yet", 500)
			return
		}
		fmt.Fprint(w, s.body.Load().(string))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestCumulusRunHeadersAndScript(t *testing.T) {
	c, d := testConfig(t)
	done := filepath.Join(d, "ran")
	srv := newCLServer(t, "#!/bin/sh\n# CUMULUS-AUTOPROVISIONING\necho \"$PATH\" > "+done+"\necho hello from the script\n")
	if rc := run(context.Background(), c, []string{"-r", srv.URL + "/ztp.sh"}); rc != 0 {
		t.Fatalf("ztp -r = %d: %s", rc, c.stderr)
	}
	if got := readFile(t, done); !strings.HasPrefix(got, nos.ScriptBin+":"+nos.Day0Mount+":") || !strings.Contains(got, "/usr/sbin") {
		t.Errorf("script PATH = %q", got)
	}
	h := srv.headers[0]
	for k, v := range map[string]string{
		"User-Agent": "CumulusLinux-AutoProvision/0.4", "CUMULUS-ARCH": "x86_64", "CUMULUS-BUILD": "5.11.0",
		"CUMULUS-MANUFACTURER": "Mellanox", "CUMULUS-PRODUCTNAME": "MSN2700", "CUMULUS-SERIAL": "XYZ123004",
		"CUMULUS-BASE-MAC": "44:38:39:FF:40:94", "CUMULUS-MGMT-MAC": "44:38:39:FF:00:00", "CUMULUS-VERSION": "5.11.0",
		"CUMULUS-PROV-COUNT": "0", "CUMULUS-PROV-MAX": "32",
	} {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	log := readFile(t, filepath.Join(c.clDir, "ztp_state.log"))
	for _, s := range []string{"ZTP Manual: Found Marker CUMULUS-AUTOPROVISIONING", "hello from the script"} {
		if !strings.Contains(log, s) {
			t.Errorf("ztp_state.log lacks %q:\n%s", s, log)
		}
	}
	if !strings.Contains(readFile(t, c.autoprovLog), "hello from the script") {
		t.Error("no copy in /var/log/autoprovision")
	}
	st := status(t, c)
	if st.State != stateSuccess || st.ExitCode != 0 || st.Attempts != 1 || st.Source != "manual" || st.Flavour != "cumulus" {
		t.Errorf("status = %+v", st)
	}
	if _, err := time.Parse(time.RFC3339, st.Updated); err != nil {
		t.Errorf("updated %q: %v", st.Updated, err)
	}

	out := &bytes.Buffer{}
	c.stdout = out
	run(context.Background(), c, []string{"-s"})
	lines := strings.Split(out.String(), "\n")
	want := []string{"ZTP INFO:", "State          disabled", "Version        1.0", "Result         success"}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("ztp -s line %d = %q, want %q", i, lines[i], w)
		}
	}
	if !strings.HasPrefix(lines[4], "Date           ") || !strings.HasSuffix(lines[4], " UTC") {
		t.Errorf("Date line %q", lines[4])
	}
	if lines[5] != "Method         ZTP Manual" || lines[6] != "URL            "+srv.URL+"/ztp.sh" {
		t.Errorf("ztp -s:\n%s", out)
	}
}

func TestCumulusMarkerRefused(t *testing.T) {
	c, d := testConfig(t)
	done := filepath.Join(d, "ran")
	srv := newCLServer(t, "#!/bin/sh\ntouch "+done+"\n")
	if rc := run(context.Background(), c, []string{"-r", srv.URL}); rc == 0 {
		t.Fatal("a script without the marker succeeded")
	}
	if exists(done) {
		t.Fatal("a script without the marker ran")
	}
	if st := status(t, c); st.State != stateFailed || !strings.Contains(st.Message, "marker") {
		t.Errorf("status = %+v", st)
	}
	if !strings.Contains(readFile(t, filepath.Join(c.clDir, "ztp_state.log")), "Marker CUMULUS-AUTOPROVISIONING not found") {
		t.Error("refusal not logged")
	}
}

func TestCumulusDaemonDHCPRetry(t *testing.T) {
	c, _ := testConfig(t)
	srv := newCLServer(t, "# CUMULUS-AUTOPROVISIONING\nexit 0\n")
	srv.fails.Store(2)
	writeFile(t, c.dhcpEnv, "reason='BOUND'\nnew_cumulus_provision_url='"+srv.URL+"/s'\n", 0o644)
	if rc := newCumulus(c, false).daemon(ctxT(t, 30*time.Second)); rc != 0 {
		t.Fatalf("daemon = %d", rc)
	}
	st := status(t, c)
	if st.State != stateSuccess || st.Attempts != 3 || st.Source != "dhcp-239" {
		t.Errorf("status = %+v", st)
	}
	// the retries are one run: PROV-COUNT is the runs before it
	for i, h := range srv.headers {
		if got := h.Get("CUMULUS-PROV-COUNT"); got != "0" {
			t.Errorf("attempt %d: PROV-COUNT %s", i, got)
		}
	}
	if n := newCumulus(c, false).load().Count; n != 1 {
		t.Errorf("PROV-COUNT after the run: %d", n)
	}
	if !strings.Contains(readFile(t, filepath.Join(c.clDir, "ztp_state.log")), "Retrying") {
		t.Error("no Retrying in the log")
	}
	// provisioned: the next boot does nothing
	n := len(srv.headers)
	if rc := newCumulus(c, false).daemon(ctxT(t, time.Second)); rc != 0 || len(srv.headers) != n {
		t.Errorf("a provisioned switch ran ZTP again (rc %d)", rc)
	}
	if st := status(t, c); st.State != stateSuccess || !strings.Contains(st.Message, "already provisioned") {
		t.Errorf("status = %+v", st)
	}
	// reset re-arms
	if rc := run(context.Background(), c, []string{"-R"}); rc != 0 {
		t.Fatal(rc)
	}
	if st := newCumulus(c, false).load(); st.State != "enabled" || st.Count != 0 || st.Result != "" {
		t.Errorf("after -R: %+v", st)
	}
}

func TestCumulusFailureGivesUp(t *testing.T) {
	c, _ := testConfig(t)
	c.provMax = 3
	srv := newCLServer(t, "# CUMULUS-AUTOPROVISIONING\nexit 3\n")
	writeFile(t, c.dhcpEnv, "new_cumulus_provision_url='"+srv.URL+"'\n", 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	newCumulus(c, false).daemon(ctx)
	// one run: retried until its window closed, counted once
	if len(srv.headers) < 2 {
		t.Errorf("%d attempts in the window", len(srv.headers))
	}
	if n := newCumulus(c, false).load().Count; n != 1 {
		t.Errorf("PROV-COUNT %d after one failed run", n)
	}
	st := status(t, c)
	if st.State != stateFailed || st.ExitCode != 3 {
		t.Errorf("status = %+v", st)
	}
	log := readFile(t, filepath.Join(c.clDir, "ztp_state.log"))
	for _, s := range []string{"ZTP DHCP: Payload returned code 3", "Script returned failure", "ZTP script failed. Exiting..."} {
		if !strings.Contains(log, s) {
			t.Errorf("log lacks %q", s)
		}
	}
	if r := newCumulus(c, false).load(); r.Result != "failed" || r.State != "enabled" {
		t.Errorf("state %+v", r)
	}
	// at CUMULUS-PROV-MAX runs, ZTP fetches nothing more until ztp -R
	z := newCumulus(c, false)
	st2 := z.load()
	st2.Count = c.provMax
	z.store(st2)
	n := len(srv.headers)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	newCumulus(c, false).daemon(ctx2)
	if len(srv.headers) != n || !strings.Contains(status(t, c).Message, "reached CUMULUS-PROV-MAX 3") {
		t.Errorf("at PROV-MAX: %d more fetches, status %+v", len(srv.headers)-n, status(t, c))
	}
}

func TestCumulusNoOptionThenLease(t *testing.T) {
	c, d := testConfig(t)
	srv := newCLServer(t, "# CUMULUS-AUTOPROVISIONING\ntouch "+filepath.Join(d, "ran")+"\n")
	writeFile(t, c.dhcpEnv, "reason='BOUND'\nnew_ip_address='192.0.2.9'\n", 0o644)
	rc := make(chan int, 1)
	go func() { rc <- newCumulus(c, false).daemon(ctxT(t, 30*time.Second)) }()
	deadline := time.Now().Add(3 * time.Second)
	for status(t, c).State != stateFailed {
		if time.Now().After(deadline) {
			t.Fatalf("no failure after the window: %+v", status(t, c))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(status(t, c).Message, "no DHCP option 239") {
		t.Errorf("message %q", status(t, c).Message)
	}
	writeFile(t, c.dhcpEnv, "reason='BOUND'\nnew_cumulus_provision_url='"+srv.URL+"'\n", 0o644)
	if r := <-rc; r != 0 || !exists(filepath.Join(d, "ran")) {
		t.Fatalf("daemon = %d, status %+v", r, status(t, c))
	}
}

func TestCumulusLocalWaterfall(t *testing.T) {
	c, d := testConfig(t)
	z := newCumulus(c, false)
	want := []string{"cumulus-ztp-x86_64-mlnx_msn2700-r0", "cumulus-ztp-x86_64-mlnx_msn2700", "cumulus-ztp-mlnx_msn2700",
		"cumulus-ztp-x86_64", "cumulus-ztp"}
	if got := z.localNames(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("names = %v", got)
	}
	writeFile(t, filepath.Join(c.clDir, "cumulus-ztp"), "# CUMULUS-AUTOPROVISIONING\ntouch "+filepath.Join(d, "generic")+"\n", 0o644)
	writeFile(t, filepath.Join(c.clDir, "cumulus-ztp-x86_64"), "# CUMULUS-AUTOPROVISIONING\ntouch "+filepath.Join(d, "arch")+"\n", 0o644)
	if rc := z.daemon(ctxT(t, 2*time.Second)); rc != 0 {
		t.Fatal(rc)
	}
	if !exists(filepath.Join(d, "arch")) || exists(filepath.Join(d, "generic")) {
		t.Error("the most specific local script did not run alone")
	}
	if st := status(t, c); st.Source != "local" || st.State != stateSuccess {
		t.Errorf("status %+v", st)
	}
}

func TestCumulusManualConfiguration(t *testing.T) {
	c, _ := testConfig(t)
	writeFile(t, c.userApplied, "", 0o644)
	srv := newCLServer(t, "# CUMULUS-AUTOPROVISIONING\n")
	writeFile(t, c.dhcpEnv, "new_cumulus_provision_url='"+srv.URL+"'\n", 0o644)
	if rc := newCumulus(c, false).daemon(ctxT(t, time.Second)); rc != 0 {
		t.Fatal(rc)
	}
	if len(srv.headers) != 0 {
		t.Error("ZTP fetched on a manually configured switch")
	}
	if st := status(t, c); st.State != stateDisabled {
		t.Errorf("status %+v", st)
	}
	out := &bytes.Buffer{}
	c.stdout = out
	run(context.Background(), c, []string{"-s"})
	if !strings.Contains(out.String(), "Method         Switch manually configured") || !strings.Contains(out.String(), "State          disabled") {
		t.Errorf("ztp -s:\n%s", out)
	}
}

func TestCumulusEnableDisable(t *testing.T) {
	c, _ := testConfig(t)
	out := &bytes.Buffer{}
	c.stdout = out
	run(context.Background(), c, []string{"-s"})
	if want := "ZTP INFO:\nState          enabled\nVersion        1.0\nResult         None\nDate           None\nMethod         None\nURL            None\n"; out.String() != want {
		t.Errorf("fresh ztp -s:\n%s", out)
	}
	run(context.Background(), c, []string{"-d"})
	if newCumulus(c, false).load().State != "disabled" || status(t, c).State != stateDisabled {
		t.Error("-d did not disable")
	}
	if rc := newCumulus(c, false).daemon(ctxT(t, time.Second)); rc != 0 || status(t, c).State != stateDisabled {
		t.Error("a disabled ZTP ran")
	}
	run(context.Background(), c, []string{"-e"})
	if newCumulus(c, false).load().State != "enabled" {
		t.Error("-e did not enable")
	}
	if rc := run(context.Background(), c, []string{"-x"}); rc != 2 {
		t.Errorf("unknown flag rc %d", rc)
	}
}

func TestFetchSchemes(t *testing.T) {
	c, d := testConfig(t)
	f := c.newFetcher(nil, false)
	if _, err := f.get(context.Background(), "tftp://192.0.2.1/x"); err == nil || !strings.Contains(err.Error(), "tftp is not supported") {
		t.Errorf("tftp: %v", err)
	}
	p := filepath.Join(d, "f")
	writeFile(t, p, "x", 0o644)
	for _, u := range []string{p, "file://" + p} {
		if b, err := f.get(context.Background(), u); err != nil || string(b) != "x" {
			t.Errorf("%s: %q %v", u, b, err)
		}
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if _, err := f.get(context.Background(), srv.URL); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: %v", err)
	}
}

// --- SONiC

// fakeCmd writes an executable that records its arguments to log, one call per line.
func fakeCmd(t *testing.T, dir, name, log string, exit int) string {
	p := filepath.Join(dir, name)
	writeFile(t, p, fmt.Sprintf("#!/bin/sh\necho %s \"$@\" >> %s\nexit %d\n", name, log, exit), 0o755)
	return p
}

func sonicServer(t *testing.T, files map[string]string) (*httptest.Server, *[]*http.Request) {
	var mu sync.Mutex
	var reqs []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r)
		mu.Unlock()
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, b)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func loadDoc(t *testing.T, c *config) doc {
	t.Helper()
	d, err := newSonic(c, false).loadDoc()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func secStatus(d doc, name string) string { return statusOf(d.top()[name].(map[string]any)) }

func TestSonicSectionsOrderAndFlags(t *testing.T) {
	c, d := testConfig(t)
	order := filepath.Join(d, "order")
	script := func(name string, code int) string {
		return fmt.Sprintf("#!/bin/sh\necho %s >> %s\nexit %d\n", name, order, code)
	}
	srv, _ := sonicServer(t, map[string]string{
		"/a.sh": script("a", 0), "/b.sh": script("b", 4), "/c.sh": script("c", 0), "/d.sh": script("d", 1), "/e.sh": script("e", 0),
	})
	sec := func(path string, extra string) string {
		return fmt.Sprintf(`{"plugin": {"url": %q, "ignore-section-data": true}%s}`, srv.URL+path, extra)
	}
	writeFile(t, filepath.Join(c.ztpDir, "ztp_data_local.json"), `{"ztp": {
		"10-c": `+sec("/c.sh", `, "status": "SUCCESS"`)+`,
		"02-b": `+sec("/b.sh", `, "ignore-result": true`)+`,
		"01-a": `+sec("/a.sh", "")+`,
		"20-d": `+sec("/d.sh", `, "halt-on-failure": true, "reboot-on-failure": true`)+`,
		"30-e": `+sec("/e.sh", "")+`,
		"ztp-json-version": "1.0"
	}}`, 0o644)
	if rc := newSonic(c, false).daemon(ctxT(t, 30*time.Second)); rc != 1 {
		t.Fatalf("daemon = %d", rc)
	}
	if got := strings.Fields(readFile(t, order)); strings.Join(got, ",") != "a,b,d" {
		t.Errorf("ran %v, want a,b,d (c already SUCCESS, e after a halt)", got)
	}
	doc := loadDoc(t, c)
	for n, want := range map[string]string{"01-a": stSuccess, "02-b": stSuccess, "10-c": stSuccess, "20-d": stFailed, "30-e": stBoot} {
		if got := secStatus(doc, n); got != want {
			t.Errorf("%s = %s, want %s", n, got, want)
		}
	}
	b := doc.top()["02-b"].(map[string]any)
	if b["exit-code"] != float64(4) || b["timestamp"] == nil || b["start-timestamp"] == nil || b["halt-on-failure"] != false {
		t.Errorf("02-b = %v", b)
	}
	if _, err := time.Parse(tsFormat, b["timestamp"].(string)); err != nil {
		t.Errorf("timestamp: %v", err)
	}
	if doc.top()["status"] != stFailed {
		t.Errorf("document status %v", doc.top()["status"])
	}
	st := status(t, c)
	if st.State != stateFailed || st.Source != "local" || st.Flavour != "sonic" {
		t.Errorf("status %+v", st)
	}
	if !strings.Contains(readFile(t, c.ztpLog), "reboot-on-failure: the twin cannot reboot") {
		t.Error("reboot-on-failure not logged")
	}
}

func TestSonicOpt67ConfigDBDynamicURL(t *testing.T) {
	c, d := testConfig(t)
	calls := filepath.Join(d, "calls")
	c.reloadCmd = fakeCmd(t, d, "reload", calls, 0)
	c.loadCmd = fakeCmd(t, d, "load", calls, 0)
	c.saveCmd = fakeCmd(t, d, "save", calls, 0)
	c.dumpMetaCmd = `echo '{"localhost": {"mac": "44:38:39:ff:40:94", "platform": "x86_64-kvm_x86_64-r0"}}'`
	srv, reqs := sonicServer(t, map[string]string{
		"/XYZ123004_config_db.json": `{"DEVICE_METADATA": {"localhost": {"hostname": "leaf01", "mac": "00:00:00:00:00:01"}}, "PORT": {}}`,
		"/b.json":                   `{"PORT": {}}`,
	})
	ztpDoc := fmt.Sprintf(`{"ztp": {
		"01-configdb-json": {"dynamic-url": {"source": {"prefix": %q, "identifier": "serial-number", "suffix": "_config_db.json"}}, "save-config": true},
		"02-configdb-json": {"url": {"source": %q, "destination": %q}, "clear-config": false, "overwrite-device-metadata": true}
	}}`, srv.URL+"/", srv.URL+"/b.json", filepath.Join(d, "b.json"))
	files := map[string]string{"/ztp.json": ztpDoc}
	srv2, reqs2 := sonicServer(t, files)
	writeFile(t, c.dhcpEnv, "reason='BOUND'\nnew_bootfile_name='"+srv2.URL+"/ztp.json'\nnew_provisioning_script_url='"+srv2.URL+"/never'\n", 0o644)
	if rc := newSonic(c, false).daemon(ctxT(t, 30*time.Second)); rc != 0 {
		t.Fatalf("daemon = %d; log:\n%s", rc, readFile(t, c.ztpLog))
	}
	h := (*reqs2)[0].Header
	for k, v := range map[string]string{"User-Agent": "SONiC-ZTP/0.1", "PRODUCT-NAME": "MSN2700", "SERIAL-NUMBER": "XYZ123004",
		"BASE-MAC-ADDRESS": "44:38:39:FF:40:94", "SONiC-VERSION": "5.11.0"} {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(*reqs) != 2 {
		t.Errorf("config server saw %d requests", len(*reqs))
	}
	db := filepath.Join(c.sonicDir, "config_db.json")
	want := fmt.Sprintf("reload %s\nsave\nload %s\n", db, filepath.Join(d, "b.json"))
	if got := readFile(t, calls); got != want {
		t.Errorf("commands:\n%s\nwant:\n%s", got, want)
	}
	var cfg struct {
		Meta map[string]map[string]string `json:"DEVICE_METADATA"`
	}
	if err := json.Unmarshal([]byte(readFile(t, db)), &cfg); err != nil {
		t.Fatal(err)
	}
	if l := cfg.Meta["localhost"]; l["hostname"] != "leaf01" || l["mac"] != "44:38:39:ff:40:94" || l["platform"] != "x86_64-kvm_x86_64-r0" {
		t.Errorf("DEVICE_METADATA not merged: %v", l)
	}
	if st := status(t, c); st.State != stateSuccess || st.Source != "dhcp-67" {
		t.Errorf("status %+v", st)
	}
	// status output
	out := &bytes.Buffer{}
	c.stdout = out
	run(context.Background(), c, []string{"status"})
	got := out.String()
	for _, s := range []string{"ZTP Admin Mode : True\n", "ZTP Service    : Inactive\n", "ZTP Status     : SUCCESS\n",
		"ZTP Source     : dhcp-opt67 (eth0)\n", "Runtime        : 00m ", "Timestamp      : ",
		"\nZTP Service is not running\n\n01-configdb-json: SUCCESS\n02-configdb-json: SUCCESS\n"} {
		if !strings.Contains(got, s) {
			t.Errorf("ztp status lacks %q:\n%s", s, got)
		}
	}
	out.Reset()
	run(context.Background(), c, []string{"status", "-c"})
	if out.String() != "5:SUCCESS\n" {
		t.Errorf("status -c = %q", out)
	}
	// a second boot does not run again
	if rc := newSonic(c, false).daemon(ctxT(t, time.Second)); rc != 0 || strings.Count(readFile(t, calls), "reload") != 1 {
		t.Error("ZTP ran again after SUCCESS")
	}
}

func TestSonicOpt239(t *testing.T) {
	c, d := testConfig(t)
	srv, _ := sonicServer(t, map[string]string{"/prov.sh": "#!/bin/sh\necho \"$#\" > " + filepath.Join(d, "argc") + "\nexit 0\n"})
	writeFile(t, c.dhcpEnv, "new_provisioning_script_url='"+srv.URL+"/prov.sh'\n", 0o644)
	if rc := newSonic(c, false).daemon(ctxT(t, 30*time.Second)); rc != 0 {
		t.Fatalf("daemon = %d", rc)
	}
	if strings.TrimSpace(readFile(t, filepath.Join(d, "argc"))) != "0" {
		t.Error("ignore-section-data: the script was given input.json")
	}
	doc := loadDoc(t, c)
	if secStatus(doc, "provisioning-script") != stSuccess || doc.top()["restart-ztp-no-config"] != false {
		t.Errorf("doc %v", doc)
	}
	if st := status(t, c); st.Source != "dhcp-239" || st.State != stateSuccess {
		t.Errorf("status %+v", st)
	}
}

func TestSonicManualConfigSkips(t *testing.T) {
	for name, setup := range map[string]func(c *config){
		"hostname": func(c *config) {
			writeFile(t, filepath.Join(c.sonicDir, "config_db.json"), `{"DEVICE_METADATA": {"localhost": {"hostname": "leaf9"}}}`, 0o644)
		},
		"marker": func(c *config) { writeFile(t, filepath.Join(c.sonicDir, "ztp-skip"), "", 0o644) },
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := testConfig(t)
			setup(c)
			if rc := newSonic(c, false).daemon(ctxT(t, time.Second)); rc != 0 {
				t.Fatal(rc)
			}
			if st := status(t, c); st.State != stateDisabled || !strings.Contains(st.Message, "MANUAL_CONFIG") {
				t.Errorf("status %+v", st)
			}
		})
	}
	c, _ := testConfig(t)
	writeFile(t, filepath.Join(c.sonicDir, "config_db.json"), `{"DEVICE_METADATA": {"localhost": {"hostname": "sonic"}}}`, 0o644)
	if why := newSonic(c, false).manualConfig(); why != "" {
		t.Errorf("the factory config_db.json reads as manual: %s", why)
	}
}

func TestSonicPluginsRefusedAndSNMP(t *testing.T) {
	c, d := testConfig(t)
	calls := filepath.Join(d, "calls")
	c.cfgdbSet = fakeCmd(t, d, "hset", calls, 0)
	writeFile(t, filepath.Join(c.sonicDir, "snmp.yml"), "snmp_rocommunity: old\nother: kept\n", 0o644)
	writeFile(t, filepath.Join(c.ztpDir, "ztp_data_local.json"), `{"ztp": {
		"01-snmp": {"communities-ro": ["public"], "snmp-location": "row 3"},
		"02-firmware": {"install": {"url": "http://x/y.bin"}},
		"03-firmware": {"install": {"url": "http://x/y.bin"}, "ignore-result": true},
		"04-graphservice": {"minigraph-url": "http://x/m.xml"},
		"05-nonesuch": {}
	}}`, 0o644)
	newSonic(c, false).daemon(ctxT(t, 30*time.Second))
	doc := loadDoc(t, c)
	for n, want := range map[string]string{"01-snmp": stSuccess, "02-firmware": stFailed, "03-firmware": stSuccess,
		"04-graphservice": stFailed, "05-nonesuch": stFailed} {
		if got := secStatus(doc, n); got != want {
			t.Errorf("%s = %s, want %s", n, got, want)
		}
	}
	if got := readFile(t, filepath.Join(c.sonicDir, "snmp.yml")); got != "other: kept\nsnmp_rocommunity: public\nsnmp_location: row 3\n" {
		t.Errorf("snmp.yml:\n%s", got)
	}
	if got := readFile(t, calls); got != "hset SNMP_COMMUNITY|public TYPE RO\nhset SNMP|LOCATION Location row 3\n" {
		t.Errorf("cfgdb:\n%s", got)
	}
	log := readFile(t, c.ztpLog)
	for _, s := range []string{"an image install is modelled (D75)", "graphservice: refused by name"} {
		if !strings.Contains(log, s) {
			t.Errorf("log lacks %q", s)
		}
	}
}

func TestSonicConnectivityCheck(t *testing.T) {
	c, d := testConfig(t)
	n := filepath.Join(d, "n")
	// fails twice, then answers
	c.pingCmd = filepath.Join(d, "ping")
	writeFile(t, c.pingCmd, "#!/bin/sh\necho \"$@\" >> "+filepath.Join(d, "args")+"\necho x >> "+n+
		"\n[ $(wc -l < "+n+") -ge 3 ]\n", 0o755)
	z := newSonic(c, false)
	code, err := z.connectivity(context.Background(), "c", map[string]any{"ping-hosts": []any{"192.0.2.1"}, "retry-count": float64(5), "retry-interval": float64(0)})
	if code != 0 || err != nil {
		t.Fatalf("connectivity = %d %v", code, err)
	}
	if got := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(d, "args"))), "\n"); len(got) != 3 || got[0] != "-c 3 -W 1 192.0.2.1" {
		t.Errorf("pings %q", got)
	}
	code, _ = z.connectivity(context.Background(), "c", map[string]any{"ping-hosts": "192.0.2.2", "retry-count": float64(1)})
	if code != 0 {
		t.Errorf("second check %d", code)
	}
	os.Remove(n)
	code, err = z.connectivity(context.Background(), "c", map[string]any{"ping-hosts": []any{"192.0.2.3"}, "retry-count": float64(2), "retry-interval": float64(0)})
	if code == 0 || err == nil {
		t.Error("an unreachable host passed")
	}
}

func TestSonicDynamicIdentifiers(t *testing.T) {
	c, _ := testConfig(t)
	z := newSonic(c, false)
	for id, want := range map[string]string{"hostname": "leaf01", "hostname-fqdn": "leaf01.example", "serial-number": "XYZ123004",
		"product-name": "MSN2700", "sonic-version": "5.11.0", "literal": "literal"} {
		s, err := z.dynamicURL(context.Background(), map[string]any{"source": map[string]any{"prefix": "http://s/", "identifier": id, "suffix": ".json"}, "destination": "/x"})
		if err != nil || s.source != "http://s/"+want+".json" || s.destination != "/x" {
			t.Errorf("%s: %+v %v", id, s, err)
		}
	}
	srv, _ := sonicServer(t, map[string]string{"/id.sh": "#!/bin/sh\necho rack7\n"})
	s, err := z.dynamicURL(context.Background(), map[string]any{"source": map[string]any{"prefix": "p-", "identifier": map[string]any{"url": srv.URL + "/id.sh"}}})
	if err != nil || s.source != "p-rack7" {
		t.Errorf("script identifier: %+v %v", s, err)
	}
}

func TestSonicCLI(t *testing.T) {
	c, _ := testConfig(t)
	out := &bytes.Buffer{}
	c.stdout = out
	run(context.Background(), c, []string{"status", "-c"})
	if out.String() != "3:NOT-STARTED\n" {
		t.Errorf("fresh -c %q", out)
	}
	out.Reset()
	c.stdin = strings.NewReader("n\n")
	run(context.Background(), c, []string{"disable"})
	if !newSonic(c, false).adminMode() {
		t.Error("disable without confirmation disabled")
	}
	run(context.Background(), c, []string{"disable", "-y"})
	out.Reset()
	run(context.Background(), c, []string{"status", "-c"})
	if out.String() != "0:DISABLED\n" || status(t, c).State != stateDisabled {
		t.Errorf("after disable: %q %+v", out, status(t, c))
	}
	if rc := run(context.Background(), c, []string{"run", "-y"}); rc != 1 {
		t.Error("run while disabled")
	}
	run(context.Background(), c, []string{"enable"})
	out.Reset()
	run(context.Background(), c, []string{"status"})
	if want := "ZTP Admin Mode : True\nZTP Service    : Inactive\nZTP Status     : Not Started\n\nZTP Service is not running\n"; out.String() != want {
		t.Errorf("status:\n%q\nwant\n%q", out, want)
	}
	// run in the foreground
	writeFile(t, filepath.Join(c.ztpDir, "ztp_data_local.json"), `{"ztp": {"snmp": {"snmp-location": "here"}}}`, 0o644)
	if rc := run(ctxT(t, 30*time.Second), c, []string{"run", "-y"}); rc != 0 {
		t.Fatalf("run = %d", rc)
	}
	out.Reset()
	run(context.Background(), c, []string{"status", "-v"})
	for _, s := range []string{"ZTP Status       : SUCCESS", "ZTP JSON Version : 1.0", "snmp\n", "Exit Code        : 0", "Halt on Failure  : False"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("verbose lacks %q:\n%s", s, out)
		}
	}
}

func TestRuntimeFormat(t *testing.T) {
	now := time.Now()
	if got := runtime("2019-09-11 19:06:53 UTC", "2019-09-11 19:12:24 UTC", now); got != "05m 31s" {
		t.Errorf("%q", got)
	}
	if got := runtime("2019-09-10 18:00:00 UTC", "2019-09-11 19:12:24 UTC", now); got != "1d 01h 12m 24s" {
		t.Errorf("%q", got)
	}
}

func TestDaemonFlags(t *testing.T) {
	c, _ := testConfig(t)
	if rc := run(context.Background(), c, []string{"daemon"}); rc != 2 {
		t.Errorf("no --nos: %d", rc)
	}
	writeFile(t, c.factoryFile, `{"install":"preinstalled","ztp":false,"nos":"sonic-vs"}`, 0o644)
	if rc := run(ctxT(t, time.Second), c, []string{"daemon", "--nos", "sonic", "--reload-cmd", "my-reload", "--cfgdb-set", "redis-cli -n 4 hset"}); rc != 0 {
		t.Fatal(rc)
	}
	if st := status(t, c); st.State != stateDisabled {
		t.Errorf("factory ztp false: %+v", st)
	}
	c2 := *c
	c2.reloadCmd = ""
	c2.loadDaemonOptions()
	if c2.reloadCmd != "my-reload" || c2.cfgdbSet != "redis-cli -n 4 hset" {
		t.Errorf("daemon options not kept: %q %q", c2.reloadCmd, c2.cfgdbSet)
	}
}

// A script option 239 names runs in eth0's VRF, mgmt, through `ip vrf exec` (clsys), as Cumulus's
// DHCP hook runs it there; one ztp -r names, or on a switch with no mgmt, runs as ztp was started.
func TestCumulusDHCPScriptInMgmtVRF(t *testing.T) {
	c, d := testConfig(t)
	z := newCumulus(c, false)
	dhcp, manual := clSource{"ZTP DHCP", "dhcp-239", "http://x/ztp.sh"}, clSource{"ZTP Manual", "manual", "http://x/ztp.sh"}
	if v := z.scriptVRF(dhcp); v != "" {
		t.Errorf("no mgmt VRF: %q", v)
	}
	c.vrfExists = func(v string) bool { return v == "mgmt" }
	if v := z.scriptVRF(dhcp); v != "mgmt" {
		t.Errorf("a DHCP script's VRF: %q", v)
	}
	if v := z.scriptVRF(manual); v != "" {
		t.Errorf("ztp -r's VRF: %q", v)
	}
	ipLog := filepath.Join(d, "ip.log")
	ip := filepath.Join(d, "ip")
	if err := os.WriteFile(ip, []byte("#!/bin/sh\necho \"$*\" >> "+ipLog+"\nshift 3\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c.ipCmd = ip
	done := filepath.Join(d, "ran")
	code, err := c.runScriptIn(context.Background(), "mgmt", []byte("#!/bin/sh\ntouch "+done+"\n"), io.Discard, nil)
	if err != nil || code != 0 {
		t.Fatalf("run: %d %v", code, err)
	}
	if got := readFile(t, ipLog); !strings.HasPrefix(got, "vrf exec mgmt ") {
		t.Errorf("the script's ip: %q", got)
	}
	if _, err := os.Stat(done); err != nil {
		t.Errorf("the script did not run: %v", err)
	}
}

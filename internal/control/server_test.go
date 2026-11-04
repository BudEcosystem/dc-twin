package control

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

func upMixedHall(t *testing.T) *twin.Twin {
	t.Helper()
	tw, err := twin.Up(mixedHall, twin.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	return tw
}

func get(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestControlAPIIsLoopbackUnlessAllowedBeyond(t *testing.T) {
	tw := upMixedHall(t)
	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0"} {
		if _, err := Serve(tw, addr, nil); err == nil || !strings.Contains(err.Error(), "loopback only") {
			t.Errorf("%s: served beyond loopback without --allow-remote-control (err %v)", addr, err)
		}
	}
	srv, err := ServeWith(tw, ":0", nil, ServeOptions{Remote: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	// the CLI and the chart's probes on the same host reach it at its loopback
	if !strings.HasPrefix(srv.Info.Addr, "127.0.0.1:") {
		t.Fatalf("addr %s: want the loopback address of an API listening on every address", srv.Info.Addr)
	}
	if r := get(t, "http://"+srv.Info.Addr+"/v1/status", ""); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", r.StatusCode)
	}
	if r := get(t, "http://"+srv.Info.Addr+"/v1/status", srv.Info.Token+"x"); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong token: %d, want 401", r.StatusCode)
	}
	if r := get(t, "http://"+srv.Info.Addr+"/v1/status", srv.Info.Token); r.StatusCode != http.StatusOK {
		t.Errorf("the token: %d, want 200", r.StatusCode)
	}
}

func TestControlAPITakesTheOperatorsToken(t *testing.T) {
	tw := upMixedHall(t)
	for _, bad := range []string{"short", "a token with spaces in it"} {
		if _, err := ServeWith(tw, "127.0.0.1:0", nil, ServeOptions{Token: bad}); err == nil {
			t.Errorf("token %q accepted", bad)
		}
	}
	const token = "chart-made-token-0123456789abcdef"
	srv, err := ServeWith(tw, "127.0.0.1:0", nil, ServeOptions{Token: token})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if srv.Info.Token != token {
		t.Fatalf("token %q, want the operator's", srv.Info.Token)
	}
	if r := get(t, "http://"+srv.Info.Addr+"/v1/status", token); r.StatusCode != http.StatusOK {
		t.Errorf("the operator's token: %d, want 200", r.StatusCode)
	}
}

func TestEventStreamKeepsAQuietStreamOpen(t *testing.T) {
	tw := upMixedHall(t)
	defer func(d time.Duration) { streamKeepAlive = d }(streamKeepAlive)
	streamKeepAlive = 50 * time.Millisecond
	srv, err := Serve(tw, "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	r := get(t, "http://"+srv.Info.Addr+"/v1/events/stream", srv.Info.Token)
	defer r.Body.Close()
	if r.Header.Get("X-Accel-Buffering") != "no" {
		t.Errorf("X-Accel-Buffering %q: a proxy would buffer the stream", r.Header.Get("X-Accel-Buffering"))
	}
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("the stream ended")
			}
			if l == ": keep-alive" {
				return
			}
		case <-deadline:
			t.Fatal("no keep-alive on a quiet stream")
		}
	}
}

func TestResponsesAreCompressedForAClientThatAcceptsIt(t *testing.T) {
	tw := upMixedHall(t)
	srv, err := Serve(tw, "127.0.0.1:0", http.FileServer(http.Dir("../../console")))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	raw := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	fetch := func(path, enc string, hdr map[string]string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://"+srv.Info.Addr+path, nil)
		req.Header.Set("Authorization", "Bearer "+srv.Info.Token)
		if enc != "" {
			req.Header.Set("Accept-Encoding", enc)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := raw.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	plain, want := fetch("/v1/devices?select=**", "", nil)
	if plain.Header.Get("Content-Encoding") != "" {
		t.Fatal("compressed for a client that does not accept it")
	}
	resp, body := fetch("/v1/devices?select=**", "gzip, deflate, br", nil)
	if resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("encoding %q, type %q", resp.Header.Get("Content-Encoding"), resp.Header.Get("Content-Type"))
	}
	zr, err := gzip.NewReader(strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != string(want) || !json.Valid(got) {
		t.Fatal("the compressed device list is not the plain one")
	}
	t.Logf("device list: %d bytes, %d compressed", len(want), len(body))
	if r, _ := fetch("/v1/devices?select=**", "gzip;q=0", nil); r.Header.Get("Content-Encoding") != "" {
		t.Error("compressed for gzip;q=0")
	}
	// the console's files too, and a revalidation that finds nothing changed has no body
	page, zpage := fetch("/index.html", "gzip", nil)
	file, _ := os.ReadFile("../../console/index.html")
	if zr, err := gzip.NewReader(strings.NewReader(string(zpage))); page.Header.Get("Content-Encoding") != "gzip" || err != nil {
		t.Errorf("index.html: encoding %q (%v)", page.Header.Get("Content-Encoding"), err)
	} else if got, _ := io.ReadAll(zr); string(got) != string(file) {
		t.Error("index.html compressed is not the file")
	}
	if l := page.Header.Get("Content-Length"); l != "" && l != strconv.Itoa(len(zpage)) {
		t.Errorf("index.html: Content-Length %s for a %d-byte compressed body", l, len(zpage))
	}
	if r, b := fetch("/index.html", "gzip", map[string]string{"If-Modified-Since": page.Header.Get("Last-Modified")}); r.StatusCode != http.StatusNotModified || len(b) != 0 || r.Header.Get("Content-Encoding") != "" {
		t.Errorf("revalidation: %d, %d bytes, encoding %q", r.StatusCode, len(b), r.Header.Get("Content-Encoding"))
	}
	// the event stream flushes each event: never compressed
	req, _ := http.NewRequest("GET", "http://"+srv.Info.Addr+"/v1/events/stream", nil)
	req.Header.Set("Authorization", "Bearer "+srv.Info.Token)
	req.Header.Set("Accept-Encoding", "gzip")
	r, err := raw.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.Header.Get("Content-Encoding") != "" {
		t.Error("the event stream was compressed")
	}
}

// /credentials reports the BMC account that logs in now: after a stack changes the factory
// account's password through AccountService (NICo rotates every BMC's), the new password, with the
// factory one alongside; an unchanged BMC's account as before.
func TestCredentialsFollowAPasswordChange(t *testing.T) {
	tw := upMixedHall(t)
	srv, err := Serve(tw, "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	var changed, unchanged *model.Device
	for _, d := range tw.Engine.M.Devices {
		if d.HasBMC() && len(d.State.Accounts) > 0 {
			if changed == nil {
				changed = d
			} else if unchanged == nil {
				unchanged = d
				break
			}
		}
	}
	if changed == nil || unchanged == nil {
		t.Fatal("no two BMCs with accounts")
	}
	factory := changed.Identity.BMCPassword
	acct := changed.State.Accounts[0]
	if err := tw.Engine.UpdateAccount(engine.Request{Path: changed.Path, Canonical: "account.password", Protocol: "test",
		Source: schema.EventSource{Class: "stack", Principal: "test"}}, acct.ID, map[string]any{"Password": "rotated-by-the-stack"}); err != nil {
		t.Fatal(err)
	}
	creds := func(path string) map[string]string {
		r := get(t, "http://"+srv.Info.Addr+"/v1/credentials?select="+path, srv.Info.Token)
		defer r.Body.Close()
		var out []map[string]string
		if err := json.NewDecoder(r.Body).Decode(&out); err != nil || len(out) != 1 {
			t.Fatalf("%s: %v %v", path, out, err)
		}
		return out[0]
	}
	c := creds(changed.Path)
	if c["user"] != acct.User || c["password"] != "rotated-by-the-stack" || c["factoryPassword"] != factory {
		t.Errorf("after a password change: %v", c)
	}
	if _, ok := tw.Engine.Authenticate(changed, c["user"], c["password"]); !ok {
		t.Errorf("the reported account does not log in")
	}
	u := creds(unchanged.Path)
	if u["password"] != unchanged.Identity.BMCPassword || u["factoryPassword"] != "" {
		t.Errorf("an unchanged BMC: %v", u)
	}
}

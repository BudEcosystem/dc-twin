package nvue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// Drepper's own test vectors for SHA-512 crypt.
func TestSHA512Crypt(t *testing.T) {
	for _, c := range []struct {
		key, salt string
		rounds    int
		custom    bool
		want      string
	}{
		{"Hello world!", "saltstring", 5000, false, "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"},
		{"Hello world!", "saltstringsaltstring", 10000, true, "$6$rounds=10000$saltstringsaltst$OW1/O6BYHV6BcXZu8QVeXbDWra3Oeqh0sbHbbMCVNSnCM/UrjmM0Dp8vOuZeHBy/YTBmSK6H9qs/y3RnOaw5v."},
		{"This is just a test", "toolongsaltstring", 5000, true, "$6$rounds=5000$toolongsaltstrin$lQ8jolhgVRVhY4b5pZKaysCLi0QBxGoNeKQzQ3glMhwllF7oGDZxUhx1yxdYcz/e1JSbq3y6JMxxl8audkUEm0"},
	} {
		if got := sha512Crypt(c.key, c.salt, c.rounds, c.custom); got != c.want {
			t.Errorf("%q %q: %s", c.key, c.salt, got)
		}
		if m, ok := verifySHA512Crypt(c.key, c.want); !m || !ok {
			t.Errorf("verify %s: %v %v", c.want, m, ok)
		}
		if m, _ := verifySHA512Crypt(c.key+"x", c.want); m {
			t.Errorf("a wrong password verified")
		}
	}
	h := HashPassword("secret")
	if m, ok := verifySHA512Crypt("secret", h); !m || !ok || h == HashPassword("secret") {
		t.Errorf("HashPassword: %s", h)
	}
}

// nv's words become a PATCH's path and value, or an unset's path, by the schema.
func TestCLIPath(t *testing.T) {
	for _, c := range []struct {
		words string
		path  string
		value string // JSON
		err   string
	}{
		{"system hostname leaf01", "system", `{"hostname":"leaf01"}`, ""},
		{"interface swp1 ip address 10.0.0.1/31", "interface swp1 ip address", `{"10.0.0.1/31":{}}`, ""},
		{"interface eth0 ip address dhcp", "interface eth0 ip address", `{"dhcp":{}}`, ""},
		{"vrf default router bgp neighbor swp51 remote-as external", "vrf default router bgp neighbor swp51", `{"remote-as":"external"}`, ""},
		{"vrf default router bgp neighbor swp51 remote-as 65200", "vrf default router bgp neighbor swp51", `{"remote-as":65200}`, ""},
		{"vrf default router bgp peer-group SPINE remote-as external", "vrf default router bgp peer-group SPINE", `{"remote-as":"external"}`, ""},
		{"vrf default router bgp neighbor swp51 peer-group SPINE", "vrf default router bgp neighbor swp51", `{"peer-group":"SPINE"}`, ""},
		{"router bgp autonomous-system 65101", "router bgp", `{"autonomous-system":65101}`, ""},
		{"router bgp router-id 10.10.10.1", "router bgp", `{"router-id":"10.10.10.1"}`, ""},
		{"system aaa user admin2 password secret", "system aaa user admin2", `{"password":"secret"}`, ""},
		{"system aaa user admin2 role system-admin", "system aaa user admin2", `{"role":"system-admin"}`, ""},
		{"system date-time timezone US/Eastern", "system date-time", `{"timezone":"US/Eastern"}`, ""},
		{"service ntp mgmt server 4.cumulusnetworks.pool.ntp.org iburst on", "service ntp mgmt server 4.cumulusnetworks.pool.ntp.org", `{"iburst":"on"}`, ""},
		{"service dns mgmt server 198.51.100.31", "service dns mgmt server", `{"198.51.100.31":{}}`, ""},
		{"system syslog server 192.168.0.254 vrf mgmt", "system syslog server 192.168.0.254", `{"vrf":"mgmt"}`, ""},
		{"service lldp tx-interval 10", "service lldp", `{"tx-interval":10}`, ""},
		{"router ptm enable on", "router ptm", `{"enable":"on"}`, ""},
		{"interface swp1 link state up", "interface swp1 link state", `{"up":{}}`, ""},
		{"interface swp1 link mtu 1500", "interface swp1 link", `{"mtu":1500}`, ""},
		{"interface swp1 link speed 400G", "interface swp1 link", `{"speed":"400G"}`, ""},
		{"interface swp1 link breakout 4x", "interface swp1 link", `{"breakout":"4x"}`, ""},
		{"qos roce", "qos", `{"roce":{}}`, ""},
		{"qos roce mode lossy", "qos roce", `{"mode":"lossy"}`, ""},
		{"vrf default router bgp address-family ipv4-unicast network 10.10.10.1/32", "vrf default router bgp address-family ipv4-unicast network", `{"10.10.10.1/32":{}}`, ""},
		{"router ospf enable on", "", "", "not part of the NVUE schema"},
		{"interface swp1 link breakout 3y", "", "", "not a breakout"},
		{"qos roce mode fast", "", "", "not one of lossless, lossy"},
		{"system aaa user root password x", "", "", "root"},
		{"router bgp autonomous-system", "", "", "a value is missing"},
		{"system hostname a b", "", "", "one value"},
	} {
		path, v, err := CLIPath(strings.Fields(c.words), true)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: %v, want an error with %q", c.words, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.words, err)
			continue
		}
		b, _ := json.Marshal(v)
		if strings.Join(path, " ") != c.path || string(b) != c.value {
			t.Errorf("%s: %v %s, want %s %s", c.words, path, b, c.path, c.value)
		}
	}
	for _, c := range []struct{ words, path string }{
		{"interface swp1 ip address 10.0.0.1/31", "interface swp1 ip address 10.0.0.1/31"},
		{"system hostname leaf01", "system hostname"},
		{"vrf default router bgp neighbor swp51", "vrf default router bgp neighbor swp51"},
	} {
		path, v, err := CLIPath(strings.Fields(c.words), false)
		if err != nil || v != nil || strings.Join(path, " ") != c.path {
			t.Errorf("unset %s: %v %v %v", c.words, path, v, err)
		}
	}
}

// A tree's nv set lines make the tree again.
func TestCommandsRoundTrip(t *testing.T) {
	tree, err := Startup(leaf())
	if err != nil {
		t.Fatal(err)
	}
	Patch(tree, []string{"system", "aaa", "user", "ops"}, map[string]any{"full-name": "Ops Team", "role": "nvue-admin"})
	lines := Commands(tree)
	again := Tree{}
	for _, l := range lines {
		w := strings.Fields(strings.TrimPrefix(l, "nv set "))
		for i := range w {
			w[i] = strings.Trim(w[i], "'")
		}
		if strings.Contains(l, "Ops Team") {
			w = append(w[:len(w)-2], "Ops Team")
		}
		path, v, err := CLIPath(w, true)
		if err != nil {
			t.Fatalf("%s: %v", l, err)
		}
		Patch(again, path, v)
	}
	if !reflect.DeepEqual(Clone(again), Clone(tree)) {
		a, _ := json.Marshal(again)
		b, _ := json.Marshal(tree)
		t.Errorf("round trip:\n%s\n%s", a, b)
	}
	if got := Commands(map[string]any{"interface": map[string]any{"swp1": map[string]any{"ip": map[string]any{"address": map[string]any{"10.0.0.1/31": nil}}}}}); len(got) != 1 || got[0] != "nv unset interface swp1 ip address 10.0.0.1/31" {
		t.Errorf("unset lines: %v", got)
	}
}

func factorySwitch() nos.Switch {
	sw := leaf()
	sw.Factory = &nos.Factory{Install: "preinstalled"}
	sw.Mgmt = true
	return sw
}

// A day-0 push's system and service paths render to the switch's files and accounts.
func TestDay0SystemRenders(t *testing.T) {
	sw := factorySwitch()
	tree := Factory(sw)
	set := func(words string) {
		t.Helper()
		path, v, err := CLIPath(strings.Fields(words), true)
		if err != nil {
			t.Fatal(err)
		}
		Patch(tree, path, v)
	}
	for _, c := range []string{
		"system hostname leaf01",
		"system date-time timezone US/Eastern",
		"system aaa user admin2 hashed-password $6$saltsalt$abc",
		"system aaa user admin2 role system-admin",
		"system aaa user viewer role nvue-monitor",
		"system aaa user cumulus password newpass",
		"service ntp mgmt server 192.0.2.10 iburst on",
		"service ntp mgmt server 192.0.2.11",
		"service dns mgmt server 198.51.100.31",
		"service dns default server 198.51.100.32",
		"system syslog server 192.168.0.254 vrf mgmt",
		"system syslog server 192.168.0.254 port 601",
		"system syslog server 192.168.0.254 protocol tcp",
		"service lldp tx-interval 10",
		"router ptm enable on",
		"qos roce mode lossless",
		"qos roce enable on",
		"interface swp49 link breakout 4x",
		"interface swp49 link speed 100G",
		"interface swp49 link mtu 9000",
		"interface swp49 link state up",
		"interface swp49 ip address 10.255.0.0/31",
		"system api listening-address 10.0.0.5",
	} {
		set(c)
	}
	f, err := Render(tree, sw)
	if err != nil {
		t.Fatal(err)
	}
	y := f.System
	if y.Hostname != "leaf01" || y.Timezone != "US/Eastern" || y.NTPVRF != "mgmt" {
		t.Errorf("system: %+v", y)
	}
	// a role sets the account's groups; none (cumulus, the factory account) leaves them
	users := strings.Split(y.Users, "\n")
	if len(users) != 3 || users[0] != "admin2 $6$saltsalt$abc sudo,nvshow,nvset,nvapply sudo,nvshow,nvset,nvapply" ||
		!strings.HasPrefix(users[1], "cumulus $6$") || !strings.HasSuffix(users[1], " - nvshow") || users[2] != "viewer - nvshow nvshow" {
		t.Errorf("users %q", y.Users)
	}
	if want := "server 192.0.2.10 iburst\nserver 192.0.2.11"; y.NTP != want {
		t.Errorf("ntp %q", y.NTP)
	}
	for _, want := range []string{"nameserver 198.51.100.32\n", "nameserver 198.51.100.31 # vrf mgmt\n"} {
		if !strings.Contains(y.DNS, want) {
			t.Errorf("resolv.conf lacks %q:\n%s", want, y.DNS)
		}
	}
	if want := `*.* action(type="omfwd" target="192.168.0.254" port="601" protocol="tcp" Device="mgmt")`; !strings.Contains(y.Syslog, want) {
		t.Errorf("rsyslog:\n%s", y.Syslog)
	}
	for _, want := range []string{"configure system hostname leaf01\n", "configure lldp tx-interval 10\n", "configure lldp tx-hold 4\n", "configure lldp portidsubtype ifname\n"} {
		if !strings.Contains(y.LLDP, want) {
			t.Errorf("lldpd.conf lacks %q:\n%s", want, y.LLDP)
		}
	}
	// breakout and speed are recorded, not rendered; the MTU and state are
	if want := "auto swp49\niface swp49\n    address 10.255.0.0/31\n    mtu 9000\n\n"; !strings.Contains(f.Interfaces, want) {
		t.Errorf("interfaces lack %q:\n%s", want, f.Interfaces)
	}
	if strings.Contains(f.Interfaces, "breakout") || strings.Contains(f.Interfaces, "speed") || strings.Contains(f.FRR, "ptm") {
		t.Errorf("breakout, speed or ptm rendered:\n%s\n%s", f.Interfaces, f.FRR)
	}
	// a breakout's subport does not exist on the twin
	bad := Clone(tree)
	Patch(bad, []string{"interface", "swp49s0", "link", "state"}, map[string]any{"up": map[string]any{}})
	if _, err := Render(bad, sw); err == nil || !strings.Contains(err.Error(), "cannot split a port") {
		t.Errorf("a subport: %v", err)
	}
	// NTP in two VRFs, a DNS server in a tenant VRF: refused
	bad = Clone(tree)
	Patch(bad, []string{"service", "ntp", "default", "server", "192.0.2.12"}, map[string]any{})
	if _, err := Render(bad, sw); err == nil || !strings.Contains(err.Error(), "one VRF") {
		t.Errorf("NTP in two VRFs: %v", err)
	}
	// a plain password is hashed, never rendered
	plain := Clone(tree)
	Patch(plain, []string{"system", "aaa", "user", "ops"}, map[string]any{"password": "s3cret"})
	f2, err := Render(plain, sw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f2.System.Users, "s3cret") || !strings.Contains(f2.System.Users, "ops $6$") {
		t.Errorf("plain password: %q", f2.System.Users)
	}
	args := ApplyArgs(f)
	if len(args) != 16 || args[5] != "leaf01" || args[7] != y.Users || args[12] != y.LLDP {
		t.Errorf("apply args: %q", args)
	}
}

// A factory switch's front-panel ports are admin down until configured; a switch the twin
// configures keeps them as they were.
func TestFactoryPortsAdminDown(t *testing.T) {
	sw := factorySwitch()
	tree := Factory(sw)
	f, err := Render(tree, sw)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("factory interfaces:\n%s", f.Interfaces)
	for _, p := range []string{"swp1", "swp4", "swp49"} {
		if want := fmt.Sprintf("auto %s\niface %s\n    link-down yes\n", p, p); !strings.Contains(f.Interfaces, want) {
			t.Errorf("factory: %s is not down", p)
		}
	}
	for _, want := range []string{"auto mgmt\niface mgmt\n    address 127.0.0.1/8\n    vrf-table auto\n", "auto eth0\niface eth0\n    post-up kill -0", "dhclient -4 -nw -pf /run/dhclient.eth0.pid", "    vrf mgmt\n"} {
		if !strings.Contains(f.Interfaces, want) {
			t.Errorf("factory interfaces lack %q", want)
		}
	}
	if f.System.Hostname != "cumulus" || f.System.Timezone != "Etc/UTC" || f.System.LLDP == "" || f.System.Syslog == "" {
		t.Errorf("factory system: %+v", f.System)
	}
	// a port configured, or named as an unnumbered neighbour, is up
	Patch(tree, []string{"interface", "swp49", "ip", "address", "10.255.0.0/31"}, map[string]any{})
	Patch(tree, nil, map[string]any{"router": map[string]any{"bgp": map[string]any{"enable": "on", "autonomous-system": 65101.0, "router-id": "10.255.255.253"}},
		"vrf": map[string]any{"default": map[string]any{"router": map[string]any{"bgp": map[string]any{"enable": "on", "neighbor": map[string]any{"swp4": map[string]any{"remote-as": "external"}}}}}}})
	f, err = Render(tree, sw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.Interfaces, "iface swp49\n    link-down") || strings.Contains(f.Interfaces, "iface swp4\n    link-down") || !strings.Contains(f.Interfaces, "iface swp1\n    link-down yes") {
		t.Errorf("configured ports:\n%s", f.Interfaces)
	}
	// on a switch, an unnumbered neighbour is FRR's interface neighbour
	if want := " neighbor swp4 interface remote-as external\n"; !strings.Contains(f.FRR, want) {
		t.Errorf("frr.conf lacks %q:\n%s", want, f.FRR)
	}
	twin, err := Startup(leaf())
	if err != nil {
		t.Fatal(err)
	}
	f, _ = Render(twin, leaf())
	if strings.Contains(f.Interfaces, "link-down") || f.System.LLDP != "" || f.System.Syslog != "" || f.System.Timezone != "" {
		t.Errorf("a twin-configured switch changed: %+v\n%s", f.System, f.Interfaces)
	}
}

// Unnumbered sessions with a peer group, as a day-0 push writes them for a switch's uplinks.
func TestUnnumberedPeerGroupOnASwitch(t *testing.T) {
	sw := factorySwitch()
	tree := Factory(sw)
	for _, c := range []string{
		"router bgp autonomous-system 65101", "router bgp router-id 10.10.10.1", "router bgp enable on",
		"vrf default router bgp enable on",
		"vrf default router bgp peer-group SPINE remote-as external",
		"vrf default router bgp neighbor swp49 peer-group SPINE",
		"vrf default router bgp neighbor swp50 peer-group SPINE",
		"vrf default router bgp address-family ipv4-unicast enable on",
		"vrf default router bgp address-family ipv4-unicast network 10.10.10.1/32",
		"interface lo ip address 10.10.10.1/32",
	} {
		path, v, err := CLIPath(strings.Fields(c), true)
		if err != nil {
			t.Fatal(err)
		}
		Patch(tree, path, v)
	}
	sw.Routed = append(sw.Routed, nos.Routed{Port: "up2"})
	f, err := Render(tree, sw)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"router bgp 65101\n bgp router-id 10.10.10.1\n", " neighbor SPINE peer-group\n neighbor SPINE remote-as external\n",
		" neighbor swp49 interface peer-group SPINE\n", " neighbor swp50 interface peer-group SPINE\n", "  network 10.10.10.1/32\n"} {
		if !strings.Contains(f.FRR, want) {
			t.Errorf("frr.conf lacks %q:\n%s", want, f.FRR)
		}
	}
	if strings.Contains(f.Interfaces, "iface swp49\n    link-down") || strings.Contains(f.Interfaces, "iface swp50\n    link-down") {
		t.Errorf("an unnumbered neighbour's port is down:\n%s", f.Interfaces)
	}
}

// fakeSwitch answers the commands NVUE runs in a factory switch: getent shadow, id -nG, python3.
type fakeSwitch struct {
	mu     sync.Mutex
	shadow map[string]string // user -> shadow line
	groups map[string]string
	calls  []string
}

func (f *fakeSwitch) run(ctx context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args[:min(len(args), 3)], " "))
	switch {
	case len(args) == 3 && args[0] == "getent" && args[1] == "shadow":
		if l, ok := f.shadow[args[2]]; ok {
			return l + "\n", nil
		}
		return "", errors.New("exit 2")
	case len(args) == 3 && args[0] == "id" && args[1] == "-nG":
		if g, ok := f.groups[args[2]]; ok {
			return g + "\n", nil
		}
		return "", errors.New("id: no such user")
	case len(args) > 0 && args[0] == "python3":
		return "*\n", nil
	}
	return "", nil
}

func newFactoryServer(t *testing.T) (*Server, *fakeSwitch, *[]Event, *httptest.Server) {
	t.Helper()
	sw := &fakeSwitch{
		shadow: map[string]string{
			"cumulus": "cumulus:" + sha512Crypt("cumulus", "factorysalt", 5000, false) + ":0:0:99999:7:::",
			"admin2":  "admin2:" + sha512Crypt("pw2", "s2", 5000, false) + ":19000:0:99999:7:::",
			"viewer":  "viewer:" + sha512Crypt("pw3", "s3", 5000, false) + ":19000:0:99999:7:::",
			"locked":  "locked:!:19000:0:99999:7:::",
			"yes":     "yes:$y$j9T$abc$def:19000:0:99999:7:::",
		},
		groups: map[string]string{"cumulus": "cumulus sudo nvshow nvset nvapply", "admin2": "admin2 sudo nvshow nvset nvapply",
			"viewer": "viewer nvshow", "nobody2": "nobody2", "root": "root"},
	}
	srv := NewServer(factorySwitch(), FactoryPassword, func(ctx context.Context, f Files) (string, error) { return "ok", nil })
	srv.SetRunner(sw.run)
	srv.SetLocalToken("tok")
	var events []Event
	var mu sync.Mutex
	srv.OnWrite(func(e Event) { mu.Lock(); events = append(events, e); mu.Unlock() })
	if err := srv.ApplyFactory(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, sw, &events, ts
}

type caller struct {
	t  *testing.T
	ts *httptest.Server
}

func (c caller) do(method, path string, body any, hdr func(*http.Request)) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.ts.URL+path, rd)
	if hdr != nil {
		hdr(req)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return res.StatusCode, out
}

func basic(u, p string) func(*http.Request) { return func(r *http.Request) { r.SetBasicAuth(u, p) } }
func local(u, tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(HeaderNvUser, u); r.Header.Set(HeaderNvToken, tok) }
}

// A factory switch authenticates its own accounts as Cumulus's PAM does, and its local nv by
// the token.
func TestFactoryAuthentication(t *testing.T) {
	srv, fs, events, ts := newFactoryServer(t)
	c := caller{t, ts}
	for _, x := range []struct {
		name string
		hdr  func(*http.Request)
		want int
	}{
		{"cumulus with its expired factory password", basic("cumulus", "cumulus"), http.StatusForbidden},
		{"cumulus with a wrong password", basic("cumulus", "nope"), http.StatusUnauthorized},
		{"admin2", basic("admin2", "pw2"), http.StatusOK},
		{"a locked account", basic("locked", ""), http.StatusUnauthorized},
		{"a yescrypt hash the switch's libcrypt does not match", basic("yes", "x"), http.StatusUnauthorized},
		{"the twin's single password does not apply to a factory switch", basic(User, FactoryPassword), http.StatusForbidden},
		{"no credentials", nil, http.StatusUnauthorized},
		{"nv as cumulus", local("cumulus", "tok"), http.StatusOK},
		{"nv as root", local("root", "tok"), http.StatusOK},
		{"nv with a wrong token", local("cumulus", "bad"), http.StatusUnauthorized},
		{"nv as a user that does not exist", local("ghost", "tok"), http.StatusUnauthorized},
		{"nv as a user in no NVUE group", local("nobody2", "tok"), http.StatusForbidden},
	} {
		st, out := c.do("GET", "/nvue_v1/system", nil, x.hdr)
		if st != x.want {
			t.Errorf("%s: %d %v, want %d", x.name, st, out, x.want)
		}
		if x.want == http.StatusForbidden && strings.HasPrefix(x.name, "cumulus") && !strings.Contains(fmt.Sprint(out["detail"]), "must be changed") {
			t.Errorf("%s: %v", x.name, out)
		}
	}
	// a success is kept: the second request does not read the shadow again
	fs.mu.Lock()
	fs.calls = nil
	fs.mu.Unlock()
	c.do("GET", "/nvue_v1/system", nil, basic("admin2", "pw2"))
	fs.mu.Lock()
	for _, call := range fs.calls {
		if strings.HasPrefix(call, "getent") {
			t.Errorf("not cached: %v", fs.calls)
		}
	}
	fs.mu.Unlock()
	// a monitor reads and may not change
	if st, _ := c.do("GET", "/nvue_v1/interface", nil, basic("viewer", "pw3")); st != 200 {
		t.Errorf("viewer read: %d", st)
	}
	if st, _ := c.do("POST", "/nvue_v1/revision", nil, basic("viewer", "pw3")); st != http.StatusForbidden {
		t.Errorf("viewer created a revision: %d", st)
	}
	// writes carry the user
	st, out := c.do("POST", "/nvue_v1/revision", nil, basic("admin2", "pw2"))
	if st != 200 {
		t.Fatalf("admin2 revision: %d %v", st, out)
	}
	*events = nil
	c.do("POST", "/nvue_v1/dctwin/pending", nil, local("cumulus", "tok"))
	if len(*events) != 1 || (*events)[0].User != "cumulus" || (*events)[0].Via != "nv" || !strings.HasPrefix((*events)[0].Client, "cumulus@") {
		t.Errorf("events: %+v", *events)
	}
	_ = srv
}

// The single-password mode of a switch the twin configures is unchanged.
func TestTwinModeAuthUnchanged(t *testing.T) {
	srv := NewServer(leaf(), "secret", func(ctx context.Context, f Files) (string, error) { return "", nil })
	srv.SetRunner((&fakeSwitch{}).run)
	if err := srv.ApplyStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c := caller{t, ts}
	if st, _ := c.do("GET", "/nvue_v1/system", nil, basic(User, "secret")); st != 200 {
		t.Errorf("twin password: %d", st)
	}
	if st, _ := c.do("GET", "/nvue_v1/system", nil, local("root", "")); st != http.StatusUnauthorized {
		t.Errorf("local path without a token: %d", st)
	}
}

// Each user has one pending revision: nv's changes accumulate in it and apply together; another
// user's are their own.
func TestPerUserPendingRevisions(t *testing.T) {
	srv, _, _, ts := newFactoryServer(t)
	var applied []string
	srv.OnApplied(func() { applied = append(applied, strings.Join(srv.ListeningAddresses(), ",")) })
	c := caller{t, ts}
	pend := func(u string, create bool) string {
		m := "GET"
		if create {
			m = "POST"
		}
		_, out := c.do(m, "/nvue_v1/dctwin/pending", nil, local(u, "tok"))
		for id := range out {
			return id
		}
		return ""
	}
	if id := pend("cumulus", false); id != "" {
		t.Errorf("pending before any set: %s", id)
	}
	a := pend("cumulus", true)
	if a == "" || pend("cumulus", true) != a || pend("cumulus", false) != a {
		t.Fatalf("cumulus's pending revision is not one: %s", a)
	}
	b := pend("admin2", true)
	if b == a {
		t.Fatalf("two users share a pending revision")
	}
	if st, out := c.do("PATCH", "/nvue_v1/system?rev="+a, map[string]any{"hostname": "leaf01"}, local("cumulus", "tok")); st != 200 {
		t.Fatalf("patch: %d %v", st, out)
	}
	c.do("PATCH", "/nvue_v1/interface/swp49/ip/address?rev="+a, map[string]any{"10.255.0.0/31": map[string]any{}}, local("cumulus", "tok"))
	// an address with its prefix length in the path is one segment
	if st, out := c.do("DELETE", "/nvue_v1/interface/swp49/ip/address/10.255.0.0%2F31?rev="+a, nil, local("cumulus", "tok")); st != 200 {
		t.Errorf("delete an escaped segment: %d %v", st, out)
	}
	c.do("PATCH", "/nvue_v1/interface/swp49/ip/address?rev="+a, map[string]any{"10.255.0.2/31": map[string]any{}}, local("cumulus", "tok"))
	// link state is one of up and down
	c.do("PATCH", "/nvue_v1/interface/swp49/link/state?rev="+a, map[string]any{"down": map[string]any{}}, local("cumulus", "tok"))
	c.do("PATCH", "/nvue_v1/interface/swp49/link/state?rev="+a, map[string]any{"up": map[string]any{}}, local("cumulus", "tok"))
	c.do("PATCH", "/nvue_v1/system/api/listening-address?rev="+a, map[string]any{"10.0.0.5": map[string]any{}}, local("cumulus", "tok"))
	// a plain password is stored hashed
	c.do("PATCH", "/nvue_v1/system/aaa/user/ops?rev="+a, map[string]any{"password": "s3cret", "role": "nvue-admin"}, local("cumulus", "tok"))
	if _, out := c.do("GET", "/nvue_v1/interface/swp49/link/state?rev="+a, nil, local("cumulus", "tok")); out["down"] != nil || out["up"] == nil {
		t.Errorf("link state: %v", out)
	}
	if _, out := c.do("GET", "/nvue_v1/system/aaa/user/ops?rev="+a, nil, local("cumulus", "tok")); out["password"] != nil || !strings.HasPrefix(fmt.Sprint(out["hashed-password"]), "$6$") {
		t.Errorf("user: %v", out)
	}
	if _, out := c.do("GET", "/nvue_v1/system?rev="+b, nil, local("admin2", "tok")); out["hostname"] != FactoryHostname {
		t.Errorf("admin2's revision has cumulus's change: %v", out)
	}
	if st, out := c.do("PATCH", "/nvue_v1/revision/"+a, map[string]any{"state": "apply"}, local("cumulus", "tok")); st != 200 || out["state"] != "applied" {
		t.Fatalf("apply: %d %v", st, out)
	}
	if pend("cumulus", false) != "" {
		t.Errorf("an applied revision is still pending")
	}
	if fmt.Sprint(srv.ListeningAddresses()) != "[10.0.0.5]" || len(applied) != 1 || applied[0] != "10.0.0.5" {
		t.Errorf("listening addresses: %v, applied hook %v", srv.ListeningAddresses(), applied)
	}
	if _, out := c.do("GET", "/nvue_v1/interface/swp49/ip/address", nil, local("cumulus", "tok")); out["10.255.0.2/31"] == nil || out["10.255.0.0/31"] != nil {
		t.Errorf("applied addresses: %v", out)
	}
	// history: the factory revision and cumulus's
	req, _ := http.NewRequest("GET", ts.URL+"/nvue_v1/dctwin/history", nil)
	local("cumulus", "tok")(req)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var hist []map[string]any
	_ = json.NewDecoder(res.Body).Decode(&hist)
	res.Body.Close()
	if len(hist) != 2 || hist[1]["user"] != "cumulus" || hist[1]["rev-id"] != a {
		t.Errorf("history: %v", hist)
	}
	// detach keeps the revision, no longer pending for its user
	if _, out := c.do("POST", "/nvue_v1/dctwin/detach", nil, local("admin2", "tok")); out[b] == nil || pend("admin2", false) != "" {
		t.Errorf("detach: %v", out)
	}
	if st, _ := c.do("GET", "/nvue_v1/revision/"+b, nil, local("admin2", "tok")); st != 200 {
		t.Errorf("a detached revision is gone: %d", st)
	}
	// save: the applied configuration is the startup one
	if st, out := c.do("POST", "/nvue_v1/dctwin/save", nil, local("cumulus", "tok")); st != 200 {
		t.Errorf("save: %d %v", st, out)
	}
	if _, out := c.do("GET", "/nvue_v1/system?rev=startup", nil, local("cumulus", "tok")); out["hostname"] != "leaf01" {
		t.Errorf("startup after save: %v", out)
	}
}

func TestStartupYAML(t *testing.T) {
	b, err := StartupYAML(Tree{"router": map[string]any{"bgp": map[string]any{"autonomous-system": 65101.0}}}, DefaultBuild)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- header:\n", "nvue-api-version: nvue_v1", "version: Cumulus Linux 5.6.0", "- set:\n", "autonomous-system: 65101\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("startup.yaml lacks %q:\n%s", want, b)
		}
	}
}

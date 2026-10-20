package nvue

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	_ "github.com/BudEcosystem/dc-twin/internal/adapter/dhcp" // NVOS's DHCP client
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const labEstate = "../../../examples/estates/lab-model.yaml"

var actor = schema.EventSource{Class: "scenario", Principal: "nvue-test"}

type client struct {
	t          *testing.T
	base       string
	user, pass string
	hc         *http.Client
}

func (c *client) do(method, path string) (int, http.Header, []byte, error) {
	req, _ := http.NewRequest(method, c.base+path, nil)
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b, nil
}

func (c *client) get(path string, v any) {
	c.t.Helper()
	st, _, b, err := c.do(http.MethodGet, path)
	if err != nil || st != http.StatusOK {
		c.t.Fatalf("GET %s: %v %d %s", path, err, st, b)
	}
	if err := json.Unmarshal(b, v); err != nil {
		c.t.Fatalf("GET %s: %v", path, err)
	}
}

// interfaceStates decodes /nvue_v1/interface as NICo's health collector does and counts NVLink
// ports by link state.
func (c *client) interfaceStates() map[string]int {
	var ifs map[string]struct {
		Type string `json:"type"`
		Link struct {
			State map[string]json.RawMessage `json:"state"`
		} `json:"link"`
	}
	c.get("/nvue_v1/interface?rev=operational&filter_=type%3Dnvl", &ifs)
	out := map[string]int{}
	for n, i := range ifs {
		if i.Type != "nvl" {
			continue
		}
		if len(i.Link.State) != 1 {
			c.t.Fatalf("%s: link state %v", n, i.Link.State)
		}
		for k := range i.Link.State {
			out[k]++
		}
	}
	return out
}

func waitPower(t *testing.T, e *engine.Engine, d *model.Device, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var p string
		e.Read(func(*model.Model) { p = d.State.Power })
		if p == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never reached %s", d.Path, want)
}

func TestNVUEOnTheLab(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{"nvue"}, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	a := tw.Adapter("nvue").(*Adapter)
	if r := a.Report(); r.Served != 18 {
		t.Fatalf("served %d trays", r.Served)
	}
	var tray, host *model.Device
	tw.Engine.Read(func(m *model.Model) {
		tray = m.DeviceBy["site-1/hall-a/a02/switch-tray-04"]
		host = m.DeviceBy["site-1/hall-a/a02/compute-tray-08"]
	})
	c := &client{t: t, base: "https://" + a.Endpoints()[tray.Path], user: tray.Identity.BMCUser, pass: tray.Identity.BMCPassword,
		hc: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}}

	t.Run("nginx front and NVUE errors", func(t *testing.T) {
		anon := *c
		anon.user = ""
		st, h, _, _ := anon.do(http.MethodGet, "/nvue_v1/system")
		if st != http.StatusUnauthorized || h.Get("WWW-Authenticate") != `Basic realm="nvueapi"` {
			t.Fatalf("no credentials: %d %v", st, h)
		}
		bad := *c
		bad.pass = "nope"
		if st, _, _, _ := bad.do(http.MethodGet, "/nvue_v1/system"); st != http.StatusUnauthorized {
			t.Fatalf("wrong password: %d", st)
		}
		if st, h, _, _ := c.do(http.MethodHead, "/nvue_v1/system"); st != http.StatusForbidden || h.Get("Server") != "nginx" {
			t.Fatalf("HEAD: %d", st)
		}
		if st, h, _, _ := c.do(http.MethodGet, "/nvue_v1/vrf"); st != http.StatusNotFound || h.Get("Content-Type") != "application/problem+json" {
			t.Fatalf("unknown tree: %d", st)
		}
		if st, h, _, _ := c.do(http.MethodPatch, "/nvue_v1/system"); st != http.StatusMethodNotAllowed || h.Get("Allow") != "HEAD, OPTIONS, GET" {
			t.Fatalf("a write: %d", st)
		}
	})

	t.Run("a BMC account is an NVOS user", func(t *testing.T) {
		if _, err := tw.Engine.CreateAccount(engine.Request{Path: tray.Path, Canonical: "account.create", Source: actor}, "nico", "s3cret-pass", ""); err != nil {
			t.Fatal(err)
		}
		other := *c
		other.user, other.pass = "nico", "s3cret-pass"
		var sys map[string]any
		other.get("/nvue_v1/system", &sys)
	})

	t.Run("trees bound to the model", func(t *testing.T) {
		var sys, platform, version, health, leak, temps, apps, parts map[string]any
		c.get("/nvue_v1/system", &sys)
		c.get("/nvue_v1/platform", &platform)
		c.get("/nvue_v1/system/version", &version)
		c.get("/nvue_v1/system/health", &health)
		c.get("/nvue_v1/platform/environment/leakage", &leak)
		c.get("/nvue_v1/platform/environment/temperature", &temps)
		c.get("/nvue_v1/cluster/apps", &apps)
		c.get("/nvue_v1/sdn/partition", &parts)
		if sys["hostname"] != hostname(tray) || sys["product-release"] != "25.02.2553" || platform["serial-number"] != tray.Identity.Serial ||
			platform["system-type"] != "N5110_LD" || platform["port-layout"] != "144 x 400G" || version["product-release"] != "25.02.2553" {
			t.Fatalf("identity: %v %v %v", sys, platform, version)
		}
		if health["status"] != "OK" || leak["LEAKAGE-1"].(map[string]any)["state"] != "ok" || temps["ASIC2"] == nil {
			t.Fatalf("health %v leak %v temps %v", health, leak, temps)
		}
		p := parts["32766"].(map[string]any)
		if p["num-gpus"] != "72" || p["health"] != "healthy" || apps["nmx-controller"] == nil {
			t.Fatalf("partitions %v apps %v", parts, apps)
		}
		t.Logf("system: %v", sys)
		t.Logf("temperature: %v", temps)
	})

	t.Run("a powered-off compute tray takes its GPUs' lanes down", func(t *testing.T) {
		if got := c.interfaceStates(); got["up"] != 144 {
			t.Fatalf("before: %v", got)
		}
		if err := tw.Engine.Power(engine.Request{Path: host.Path, Canonical: "power.off.force", Source: actor}); err != nil {
			t.Fatal(err)
		}
		waitPower(t, tw.Engine, host, model.Off)
		// four GPUs of 18 lanes each, spread over nine switch trays: 8 lanes on this tray
		got := c.interfaceStates()
		if got["up"] != 136 || got["down"] != 8 {
			t.Fatalf("with %s off: %v", host.Name, got)
		}
		var parts map[string]map[string]any
		c.get("/nvue_v1/sdn/partition", &parts)
		if parts["32766"]["health"] != "degraded" {
			t.Fatalf("partition with a tray off: %v", parts)
		}
		t.Logf("with %s off: %v; default partition %v", host.Name, got, parts["32766"]["health"])
	})

	t.Run("NVOS answers nothing while its tray is off", func(t *testing.T) {
		if err := tw.Engine.Power(engine.Request{Path: tray.Path, Canonical: "power.off.force", Source: actor}); err != nil {
			t.Fatal(err)
		}
		waitPower(t, tw.Engine, tray, model.Off)
		if st, _, _, err := c.do(http.MethodGet, "/nvue_v1/system"); err == nil {
			t.Fatalf("a powered-off tray answered %d", st)
		}
	})
}

// NVUE answers at NVOS's own address, the lease of the tray's mgmt0 (not the BMC's), through the
// site gateway, as NICo reaches it (https://<NVOS IP>), with a certificate naming that address;
// and at nvos-<serial>. While the tray is off neither answers; powered on again, NVOS answers at
// the address it held.
func TestNVUEAtNVOSAddress(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{"dhcp", "nvue"}, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	a := tw.Adapter("nvue").(*Adapter)
	var tray *model.Device
	var bmcIP string
	tw.Engine.Read(func(m *model.Model) { tray = m.DeviceBy["site-1/hall-a/a01/switch-tray-02"] })
	waitSite := func(what string, ok func(ip string) bool) string {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			ip := a.SiteAddresses()[tray.Path]
			if ok(ip) {
				return ip
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (site address %q)", what, ip)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	ip := waitSite("NVOS's lease", func(ip string) bool { return ip != "" })
	evs := tw.Engine.Events(0, func(e schema.Event) bool {
		return e.Path == tray.Path+":mgmt0" && e.State != nil && e.State.Field == "lease"
	})
	if len(evs) == 0 || evs[len(evs)-1].State.To != ip {
		t.Fatalf("NVUE serves at %s; NVOS lease events %+v", ip, evs)
	}
	tw.Engine.Read(func(*model.Model) {
		if tray.State.Lease != nil {
			bmcIP = tray.State.Lease.IP
		}
	})
	if bmcIP == "" || bmcIP == ip {
		t.Fatalf("NVOS at %s, the BMC at %q: want two addresses", ip, bmcIP)
	}
	deadline := time.Now().Add(30 * time.Second)
	for len(a.SiteAddresses()) < 18 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond) // the rest of the start window
	}
	if got := a.Report().Lines[2]; !strings.Contains(got, "18 of 18") || !strings.Contains(got, "18 answer NVUE") {
		t.Errorf("report: %s", got)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(tw.Net.CAPEM)
	gw, _ := url.Parse("http://" + tw.Net.Gateway)
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(gw),
		TLSClientConfig: &tls.Config{RootCAs: pool}}} // verified: the certificate names NVOS's address
	c := &client{t: t, base: "https://" + ip, user: tray.Identity.BMCUser, pass: tray.Identity.BMCPassword, hc: hc}
	var sys map[string]any
	c.get("/nvue_v1/system", &sys)
	if sys["hostname"] != hostname(tray) {
		t.Fatalf("system at %s: %v", ip, sys)
	}
	byName := *c
	byName.base = "https://" + hostname(tray)
	byName.get("/nvue_v1/system", &sys)
	t.Logf("NVUE at NVOS's lease https://%s (BMC at %s) and https://%s through the site gateway: %v", ip, bmcIP, hostname(tray), sys["hostname"])

	// off: nothing answers at NVOS's address or name; the BMC's route is untouched
	if err := tw.Engine.Power(engine.Request{Path: tray.Path, Canonical: "power.off.force", Source: actor}); err != nil {
		t.Fatal(err)
	}
	waitPower(t, tw.Engine, tray, model.Off)
	waitSite("the route to go with NVOS", func(ip string) bool { return ip == "" })
	if st, _, _, err := c.do(http.MethodGet, "/nvue_v1/system"); err == nil {
		t.Fatalf("NVOS's address answered %d with its tray off", st)
	}
	if st, _, _, err := byName.do(http.MethodGet, "/nvue_v1/system"); err == nil {
		t.Fatalf("NVOS's name answered %d with its tray off", st)
	}
	if _, ok := tw.Net.Routes()[ip+":443"]; ok {
		t.Error("NVOS's address is still routed")
	}

	// on again: NVOS answers at the address it held
	if err := tw.Engine.Power(engine.Request{Path: tray.Path, Canonical: "power.on", Source: actor}); err != nil {
		t.Fatal(err)
	}
	waitPower(t, tw.Engine, tray, model.On)
	if got := waitSite("NVOS back", func(ip string) bool { return ip != "" }); got != ip {
		t.Errorf("NVOS came back at %s, want %s", got, ip)
	}
	c.get("/nvue_v1/system", &sys)
}

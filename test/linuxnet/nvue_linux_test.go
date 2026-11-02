//go:build linux

package linuxnet

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim/tap"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
)

// nvueClient is a client of one switch's NVUE, from the probe's namespace: across the fabric, as
// the software that configures a site's switches reaches them.
type nvueClient struct {
	t    *testing.T
	in   tap.SwitchInput
	http *http.Client
	pw   string
}

func (c *nvueClient) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://"+c.in.NVUE+"/nvue_v1/"+strings.TrimPrefix(path, "/"), rd)
	req.SetBasicAuth(c.in.NVUEUser, c.pw)
	res, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s at %s: %v", method, path, c.in.Hostname, err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// apply makes a revision, patches each path in it and applies it, as `nv set …; nv config apply`.
func (c *nvueClient) apply(patches map[string]any) map[string]any {
	c.t.Helper()
	st, out := c.do("POST", "revision", nil)
	var rev string
	for id := range out {
		rev = id
	}
	if st != 200 || rev == "" {
		c.t.Fatalf("%s: POST revision: %d %v", c.in.Hostname, st, out)
	}
	for path, v := range patches {
		if st, out := c.do("PATCH", path+"?rev="+rev, v); st != 200 {
			c.t.Fatalf("%s: PATCH %s: %d %v", c.in.Hostname, path, st, out)
		}
	}
	_, out = c.do("PATCH", "revision/"+rev, map[string]any{"state": "apply", "auto-prompt": map[string]any{"ays": "ays_yes"}})
	return out
}

// generic/nvue-frr on the OOB fabric (D56): two racks' management switches and the aggregation
// switch run FRR, ifupdown2 and ISC dhcrelay, configured through the twin's NVUE. Up applies each
// switch's startup configuration from the model through NVUE, and every BMC is leased by Kea
// through the relay it configured. From the probe, across the fabric, a client reaches each
// switch's NVUE at its loopback with the credentials stack-inputs gives: it reads the applied
// configuration, is refused a path outside the emulated schema and a wrong password, and builds a
// tenant segment the way an operator does on Cumulus: an EVPN VNI on a VLAN on both racks' switches,
// with the aggregation switch carrying EVPN. Each leaf then has the VNI's VXLAN device in its
// bridge and learns the other's VTEP from EVPN (a type-3 route, the flood entry in its bridge). A
// VRF with an L3 VNI is applied where the host's kernel has VRF devices, and fails, saying why,
// where it has none.
func TestNVUE(t *testing.T) {
	need(t)
	d, err := nos.NewDocker()
	if err != nil {
		t.Skipf("no Docker Engine for the NOS runtime: %v", err)
	}
	ctx := context.Background()
	for _, img := range []string{nos.NVUEFRRImage, keaImage} {
		if !d.HasImage(ctx, img) {
			t.Skipf("image %s is not present (scripts/linux-substrate-e2e.sh builds it)", img)
		}
	}
	suffix := strconv.Itoa(os.Getpid())
	keaName, probeName := "dctwin-kea-"+suffix, "dctwin-probe-nvue"
	p := newProbe(t, probeName)
	startKeaContainer(t, d, keaName)
	est := loadEstate(t, "tap-nvue.yaml", map[string]string{"KEA_CONTAINER": keaName, "PROBE_NETNS": probeName})
	start := time.Now()
	tw, sub, a := up(t, est)
	plan := sub.Plan()
	t.Logf("up returned after %s: every switch configured through NVUE and its sessions established", time.Since(start).Round(time.Second))
	logBGP(t, sub, "after up")
	took := waitLeased(t, a, len(plan.Endpoints), 120*time.Second)
	t.Logf("%d BMCs leased by Kea through the relay NVUE configured, in %s", len(plan.Endpoints), took.Round(time.Millisecond))

	var inputs []tap.SwitchInput
	b, err := os.ReadFile(filepath.Join(tw.Runtime.StateDir, "stack-inputs", tap.SwitchesFile))
	if err != nil || json.Unmarshal(b, &inputs) != nil {
		t.Fatalf("stack-inputs/%s: %v", tap.SwitchesFile, err)
	}
	t.Logf("stack-inputs/%s:\n%s", tap.SwitchesFile, indent(string(b)))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(tw.Net.CAPEM)
	client := func(in tap.SwitchInput) *nvueClient {
		return &nvueClient{t: t, in: in, pw: in.NVUEPassword, http: &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{DialContext: p.dial,
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: in.Hostname}}}}
	}
	var leaves []*nvueClient
	var agg *nvueClient
	for _, in := range inputs {
		if in.NVUE == "" || in.NVUEUser != nvue.User {
			t.Fatalf("%s: no NVUE in stack-inputs: %+v", in.Path, in)
		}
		if strings.Contains(in.Path, "oob-agg") {
			agg = client(in)
		} else {
			leaves = append(leaves, client(in))
		}
	}
	if agg == nil || len(leaves) != 2 {
		t.Fatalf("%d leaves, aggregation %v", len(leaves), agg)
	}

	// the applied configuration, as the client reads it
	l0 := leaves[0]
	if st, out := l0.do("GET", "system", nil); st != 200 || out["hostname"] != l0.in.Hostname {
		t.Errorf("GET system: %d %v", st, out)
	}
	_, ifs := l0.do("GET", "interface", nil)
	bi, _ := json.MarshalIndent(ifs, "", "  ")
	t.Logf("%s: GET /nvue_v1/interface (applied):\n%s", l0.in.Hostname, indent(string(bi)))
	if _, ok := ifs["vlan1000"]; !ok {
		t.Errorf("no vlan1000 SVI in %s's applied configuration", l0.in.Hostname)
	}
	bad := *l0
	bad.pw = "wrong"
	if st, _ := bad.do("GET", "system", nil); st != http.StatusUnauthorized {
		t.Errorf("a wrong password: %d", st)
	}
	_, out := l0.do("POST", "revision", nil)
	for rev := range out {
		if st, out := l0.do("PATCH", "router/ospf?rev="+rev, map[string]any{"enable": "on"}); st != 400 {
			t.Errorf("PATCH router ospf: %d %v", st, out)
		} else {
			t.Logf("a path outside the emulated schema is refused: %v", out["detail"])
		}
	}

	// a tenant segment: VLAN 2001 is VNI 10001 on both racks, EVPN across the aggregation switch
	evpnNeighbours := func(c *nvueClient) map[string]any {
		_, ns := c.do("GET", "vrf/default/router/bgp/neighbor", nil)
		out := map[string]any{}
		for n := range ns {
			out[n] = map[string]any{"address-family": map[string]any{"l2vpn-evpn": map[string]any{"enable": "on"}}}
		}
		return out
	}
	if out := agg.apply(map[string]any{
		"evpn":                            map[string]any{"enable": "on"},
		"vrf/default/router/bgp/neighbor": evpnNeighbours(agg),
	}); out["state"] != "applied" {
		t.Fatalf("%s: EVPN: %v", agg.in.Hostname, out)
	}
	for _, c := range leaves {
		out := c.apply(map[string]any{
			"evpn":                            map[string]any{"enable": "on"},
			"nve/vxlan":                       map[string]any{"enable": "on", "source": map[string]any{"address": c.in.Loopback}},
			"bridge/domain/br_default/vlan":   map[string]any{"2001": map[string]any{"vni": map[string]any{"10001": map[string]any{}}}},
			"vrf/default/router/bgp/neighbor": evpnNeighbours(c),
		})
		if out["state"] != "applied" {
			t.Fatalf("%s: VNI 10001: %v", c.in.Hostname, out)
		}
		t.Logf("%s: revision applied: %v", c.in.Hostname, out["transition"])
	}
	for i, c := range leaves {
		other := leaves[1-i].in.Loopback
		out, _ := sub.Exec(ctx, c.in.Path, "ip", "-d", "link", "show", "vni10001")
		if !strings.Contains(out, "vxlan id 10001 local "+c.in.Loopback) || !strings.Contains(out, "master br_default") {
			t.Errorf("%s: vni10001 is not a VXLAN device in br_default:\n%s", c.in.Hostname, out)
		}
		waitFor(t, 90*time.Second, c.in.Hostname+" to learn "+other+"'s VTEP from EVPN", func() bool {
			out, _ := sub.Exec(ctx, c.in.Path, "bridge", "fdb", "show", "dev", "vni10001")
			return strings.Contains(out, "dst "+other)
		})
		out, _ = sub.Exec(ctx, c.in.Path, "sh", "-c", "vtysh -c 'show evpn vni 10001'; vtysh -c 'show bgp l2vpn evpn route type multicast'; bridge fdb show dev vni10001")
		t.Logf("%s: VNI 10001 (FRR's EVPN view, the type-3 routes, the bridge's flood entries):\n%s", c.in.Hostname, indent(out))
		if out, _ := sub.Exec(ctx, c.in.Path, "cat", "/etc/network/interfaces"); i == 0 {
			t.Logf("%s: /etc/network/interfaces as NVUE rendered it:\n%s", c.in.Hostname, indent(out))
		}
	}

	// a VRF with an L3 VNI and the segment's SVI in it
	out = l0.apply(map[string]any{
		"vrf/tenant-a": map[string]any{"evpn": map[string]any{"vni": map[string]any{"104001": map[string]any{}}},
			"router": map[string]any{"bgp": map[string]any{"enable": "on", "address-family": map[string]any{"ipv4-unicast": map[string]any{"redistribute": map[string]any{"connected": map[string]any{"enable": "on"}}}}}}},
		"interface/vlan2001": map[string]any{"ip": map[string]any{"address": map[string]any{"10.40.1.1/24": map[string]any{}}, "vrf": "tenant-a"}},
	})
	t.Logf("%s: the VRF revision: %v", l0.in.Hostname, out)
	if sub.Kernel()["vrf"] == nil {
		if out["state"] != "applied" {
			t.Errorf("%s: a VRF on a kernel with VRF devices: %v", l0.in.Hostname, out)
		}
		k, _ := sub.Exec(ctx, l0.in.Path, "ip", "-d", "link", "show", "tenant-a")
		f, _ := sub.RoutingExec(ctx, l0.in.Path, "vtysh", "-c", "show running-config")
		if !strings.Contains(k, "vrf table") || !strings.Contains(f, "vrf tenant-a\n vni 104001") {
			t.Errorf("%s: tenant-a is not in the kernel and FRR:\n%s\n%s", l0.in.Hostname, k, f)
		}
	} else {
		if out["state"] != "apply_fail" || !strings.Contains(fmt.Sprint(out["transition"]), "tenant-a") {
			t.Errorf("%s: a VRF on a kernel without VRF devices should fail naming it: %v", l0.in.Hostname, out)
		}
		t.Logf("the Docker host's kernel has no VRF devices (%v): the revision failed, saying why", sub.Kernel()["vrf"])
	}

	for _, l := range sub.Report().Lines {
		t.Logf("report substrate: %s", l)
	}
}

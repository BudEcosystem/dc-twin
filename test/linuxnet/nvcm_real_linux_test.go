//go:build linux

package linuxnet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim/tap"
	"github.com/BudEcosystem/dc-twin/internal/day0"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
)

// NVIDIA Config Manager 1.4.1 as the day-0 tool (compat/nvcm, 18 — Z7c, D80), its own images run
// unmodified: Nautobot loaded from the design the twin hands the site (stack-inputs/day0/nvcm, a
// Design Builder blueprint, NVCM's own MockTopologyDesign job), every switch rendered by NVCM's
// render service from its Cumulus 5.14.0 templates (In-Band-Leaf, Converged-Spine), NVCM's Kea
// configured from Nautobot by its DHCP service (a reservation per switch with option 239), and its
// ZTP service serving each switch its boot script, its serial check, its startup.yaml over SFTP and
// its provisioned callback. The twin's factory switches do nothing but what Cumulus does: lease,
// fetch option 239's script, run it. The fabric converges against the design and the critic's
// day0/fabric policy judges the run.
func TestDay0NVCM(t *testing.T) { runNVCM(t, false) }

// NVCM's firmware path, with NVCM's own boot script and image store: the switches run Cumulus
// 5.13.1 and NVCM intends 5.14.0 (their intended-firmware). Its boot script finds the release
// wrong, runs onie-install -fa -i …/v1/device/<id>/firmware and reboots; ONIE fetches NVCM's image
// from its store (manifest.json, cumulus-linux/5.14.0), the twin recognises it by its sha256 and the
// NOS comes back at 5.14.0 in factory state; its ZTP runs NVCM's script again, which now finds the
// release it wants, and the fabric converges.
func TestDay0NVCMUpgrade(t *testing.T) { runNVCM(t, true) }

// nvcmStack is NVCM 1.4.1 from its own images (compat/nvcm): its name, its work directory and a
// runner of nvcm.sh, up before the twin (its edge is what the OOB network attaches at
// 10.90.0.10), taken down when the test ends.
func nvcmStack(t *testing.T) (d *nos.Docker, name, work string, nvcm func(time.Duration, ...string)) {
	t.Helper()
	compat, src := os.Getenv("DCTWIN_COMPAT_NVCM"), os.Getenv("DCTWIN_NVCM_SRC")
	if compat == "" || src == "" {
		t.Skip("DCTWIN_COMPAT_NVCM and DCTWIN_NVCM_SRC are not set (scripts/linux-substrate-e2e.sh sets them; NVCM_SRC names NVCM's source at 1.4.1)")
	}
	d, err := nosRuntime(t)
	if err != nil {
		t.Skipf("no Docker Engine: %v", err)
	}
	for _, img := range []string{"localhost/nvcm-app:1.4.1", "localhost/nvcm-nautobot:1.4.1", "localhost/nvcm-kea:1.4.1", "localhost/nvcm-kea-admin:1.4.1"} {
		if !d.HasImage(context.Background(), img) {
			t.Skipf("image %s is not present (build it from NVCM's tag: compat/nvcm/README.md)", img)
		}
	}
	run := os.Getenv("DCTWIN_RUN")
	name = "dctwin-nvcm-" + strconv.Itoa(os.Getpid())
	if run != "" {
		name = "dctwin-nvcm-" + run
	}
	work = filepath.Join(filepath.Dir(day0Binaries(t)), name)
	if err := os.MkdirAll(work, 0o777); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(compat, "nvcm.sh")
	nvcm = func(timeout time.Duration, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", append([]string{script}, args...)...)
		cmd.Env = append(os.Environ(), "NVCM_SRC="+src)
		start := time.Now()
		out, err := cmd.CombinedOutput()
		t.Logf("nvcm.sh %s (%s):\n%s", args[0], time.Since(start).Round(time.Second), indent(lastLines(string(out), 30)))
		if err != nil {
			t.Fatalf("nvcm.sh %s: %v", args[0], err)
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			for _, c := range []string{"edge", "ztp", "sftp", "dhcp-sync", "render-device", "nautobot"} {
				out, _ := d.Logs(context.Background(), name+"-"+c)
				t.Logf("%s-%s's log (tail):\n%s", name, c, indent(lastLines(out, 25)))
			}
			hold(t)
		}
		cmd := exec.Command("sh", script, "down", name)
		cmd.Env = os.Environ()
		_ = cmd.Run()
	})
	nvcm(25*time.Minute, "up", name, work)
	return d, name, work, nvcm
}

func runNVCM(t *testing.T, firmware bool) {
	d, name, work, nvcm := nvcmStack(t)
	c := day0Case{estate: "linuxnet-day0-nvcm", nos: nos.NVUEFRR, install: "preinstalled", ztp: "enabled",
		cumulus: day0.NVCMRelease, boot: "40m", dhcp: name + "-edge", dhcpIface: "eth0"}
	// the image NVCM serves as its 5.14.0 firmware (a placeholder the twin recognises by its
	// sha256, D75: the install is modelled)
	image := []byte("#!/bin/sh\n# NVCM's cumulus-linux 5.14.0 image, as the twin knows it\nexit 0\n")
	if firmware {
		c.estate, c.cumulus = "linuxnet-day0-nvcm-fw", "5.13.1"
		c.images = fmt.Sprintf(`[{ match: { sha256: "%x" }, nos: %s, version: "%s" }]`, sha256.Sum256(image), nos.NVUEFRR, day0.NVCMRelease)
	}
	r := startDay0(t, c)
	bp := filepath.Join(r.tw.Runtime.StateDir, "stack-inputs", "day0", "nvcm")
	if _, err := os.Stat(filepath.Join(bp, "context", day0.NVCMBlueprint, "locations.yaml")); err != nil {
		t.Fatalf("the design as NVCM's blueprint: %v", err)
	}
	if firmware {
		bp = nvcmIntend(t, bp, filepath.Join(work, "bp-firmware"), c.cumulus, day0.NVCMRelease)
		dir := filepath.Join(work, "images", "cumulus-linux", day0.NVCMRelease)
		file := "cumulus-linux-" + day0.NVCMRelease + "-mlx-amd64.bin"
		if err := os.MkdirAll(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), image, 0o644); err != nil {
			t.Fatal(err)
		}
		manifest, _ := json.Marshal(map[string]any{"images": []any{map[string]any{"platform": "cumulus-linux", "version": day0.NVCMRelease,
			"filename": file, "path": "cumulus-linux/" + day0.NVCMRelease + "/" + file, "sha256": fmt.Sprintf("%x", sha256.Sum256(image)), "tags": []string{}}}})
		if err := os.WriteFile(filepath.Join(work, "images", "manifest.json"), manifest, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	// the site's tooling fills its ZTP server's address in (the edge's, on the OOB network)
	nvcm(15*time.Minute, "load", name, work, bp, "10.90.0.10")
	took := r.waitConverged(25 * time.Minute)
	t.Logf("every factory switch converged %s after NVCM was loaded, through NVCM's DHCP and ZTP alone", took.Round(time.Second))
	for _, e := range r.factory() {
		out, err := r.sub.Exec(context.Background(), e.Path, "sh", "-c", "ztp -s; grep VERSION_ID /etc/os-release; id nvConfigManager; tail -5 /var/log/autoprovision")
		if err != nil || !strings.Contains(strings.ToLower(out), "success") || !strings.Contains(out, "nvConfigManager") ||
			!strings.Contains(out, "VERSION_ID="+day0.NVCMRelease) {
			t.Errorf("%s after NVCM's ZTP (%v):\n%s", e.Hostname, err, indent(out))
		}
	}
	// NVCM recorded each switch provisioned (its /provisioned callback) in Nautobot
	out, _, err := d.Exec(context.Background(), name+"-nautobot", "python", "-c", fmt.Sprintf(`
import json, urllib.request
req = urllib.request.Request("http://127.0.0.1:8080/api/dcim/devices/?limit=50&depth=1", headers={"Authorization": "Token %s", "Accept": "application/json"})
for dv in json.load(urllib.request.urlopen(req))["results"]:
    print(dv["name"], dv["status"]["name"])`, nvcmToken))
	t.Logf("NVCM's Nautobot devices:\n%s", indent(out))
	for _, e := range r.factory() {
		if err != nil || !strings.Contains(out, e.Hostname+" Provisioned") {
			t.Errorf("%s is not Provisioned in NVCM's Nautobot (%v)", e.Hostname, err)
		}
	}
	r.checkPhases()
	if firmware {
		r.judge()
		return
	}

	// NVCM's reprovision (its Cumulus client's execute_ztp: POST system/factory-default @reset over
	// NVUE, as the account NVCM rendered, with the password its site secrets give it): the switch
	// returns to the factory and NVCM's DHCP and ZTP bring it back, unasked
	e := r.factory()[0]
	ip := nvcmAddr(t, d, name, e.Hostname)
	req, _ := http.NewRequest("POST", fmt.Sprintf("https://%s:%d/nvue_v1/system/factory-default", ip, nvue.APIPort),
		strings.NewReader(`{"@reset": {"state": "start", "parameters": {"force": true}}}`))
	req.SetBasicAuth(nvue.User, "Day0-NVCM-1")
	res, err := r.httpClient().Do(req)
	if err != nil {
		t.Fatalf("%s: @reset: %v", e.Hostname, err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("%s: @reset: %d", e.Hostname, res.StatusCode)
	}
	waitFor(t, 3*time.Minute, e.Hostname+" back in the factory", func() bool {
		v, _ := r.sub.Day0()
		for _, x := range v {
			if x.Path == e.Path && !x.Converged {
				return true
			}
		}
		return false
	})
	took = r.waitConverged(15 * time.Minute)
	t.Logf("%s reprovisioned (NVCM's @reset): through NVCM's ZTP again, converged %s later", e.Hostname, took.Round(time.Second))
	r.judge()
}

// nvcmAddr is a switch's management address in NVCM's Nautobot (its eth0 reservation).
func nvcmAddr(t *testing.T, d *nos.Docker, name, host string) string {
	t.Helper()
	out, _, err := d.Exec(context.Background(), name+"-nautobot", "python", "-c", fmt.Sprintf(`
import json, urllib.request
req = urllib.request.Request("http://127.0.0.1:8080/api/ipam/ip-addresses/?device=%s&limit=50&depth=1", headers={"Authorization": "Token %s", "Accept": "application/json"})
for a in json.load(urllib.request.urlopen(req))["results"]:
    if a.get("mask_length") != 32 and a.get("mask_length") != 31: print(a["host"])`, host, nvcmToken))
	ip := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	if err != nil || ip == "" {
		t.Fatalf("%s's management address in NVCM's Nautobot: %v %q", host, err, out)
	}
	return ip
}

// nvcmToken is compat/nvcm/nvcm.sh's Nautobot API token.
const nvcmToken = "0123456789abcdef0123456789abcdef01234567"

// nvcmIntend is a copy of the blueprint whose switches' intended firmware is another release (the
// site intends what the switches do not run yet).
func nvcmIntend(t *testing.T, from, to, runs, intends string) string {
	t.Helper()
	if err := copyTree(from, to); err != nil {
		t.Fatal(err)
	}
	devs, _ := filepath.Glob(filepath.Join(to, "context", "*", "devices", "*.json"))
	for _, f := range devs {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		b = bytes.ReplaceAll(b, []byte(`"version": "`+runs+`"`), []byte(`"version": "`+intends+`"`))
		if err := os.WriteFile(f, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return to
}

// A DSX site's BMCs on the fabric NVCM brought up (18 — Z7d): the OOB fabric's server-rack
// management switches leave the factory and are NVCM's OOB-Leafs, configured by NVCM 1.4.1
// unmodified through its own DHCP and ZTP, leased through the OOB aggregation switch the twin
// configures (tap-day0-dsx.yaml). The aggregation switch is configured, as the site's first
// switch is by a person, to carry VRF OOB (L3 VNI 2000 on VLAN 4002) with the BMCs' DHCP server
// on a port in it, and EVPN with each leaf's loopback. NVCM renders on each leaf VLAN 201 in VRF
// OOB over the BMCs' ports and a relay from it to the BMC DHCP server (its vlan_to_helper_address
// relationship, which compat/nvcm's loader writes from the blueprint's helpers.json); every BMC
// leases through that relay, upstream over the leaf's L3 VNI. The BMC DHCP server here is a Kea
// standing in for NICo's forge-dhcp-server, with one subnet per rack, answering the relay's giaddr.
func TestDay0NVCMBMC(t *testing.T) {
	d, name, work, nvcm := nvcmStack(t)
	bmcName := "dctwin-bmcdhcp-" + strconv.Itoa(os.Getpid())
	bmc := startKeaContainer(t, d, bmcName)
	c := day0Case{estate: "linuxnet-day0-nvcm-bmc", nos: nos.NVUEFRR, install: "preinstalled", ztp: "enabled",
		cumulus: day0.NVCMRelease, boot: "40m", dhcp: name + "-edge", dhcpIface: "eth0", file: "tap-day0-dsx.yaml", bmcDHCP: bmcName}
	r := startDay0(t, c)
	var leaves []day0.Switch
	var aggLo string
	for _, s := range r.design.Switches {
		switch {
		case s.Factory && s.Role == "oob":
			leaves = append(leaves, s)
		case strings.Contains(s.Hostname, "oob-agg"):
			aggLo = strings.TrimSuffix(s.Loopback, "/32")
		}
	}
	if len(leaves) != 2 || aggLo == "" {
		t.Fatalf("the design: %d factory OOB leaves, aggregation switch's loopback %q", len(leaves), aggLo)
	}

	aggregationOOB(t, r, leaves, aggLo, true)

	bp := filepath.Join(r.tw.Runtime.StateDir, "stack-inputs", "day0", "nvcm")
	var helpers struct {
		Vlans []struct {
			Vid     int    `json:"vid"`
			Prefix  string `json:"prefix"`
			Gateway string `json:"gateway"`
		} `json:"vlans"`
	}
	hb, err := os.ReadFile(filepath.Join(bp, "context", day0.NVCMBlueprint, "helpers.json"))
	if err != nil || json.Unmarshal(hb, &helpers) != nil || len(helpers.Vlans) != 1 {
		t.Fatalf("the blueprint's relay helpers: %v\n%s", err, hb)
	}
	// the BMC DHCP serves VLAN 201's one subnet, which every OOB-Leaf's relay names by its own
	// address in it (giaddr)
	bmcNet := netip.MustParsePrefix(helpers.Vlans[0].Prefix)
	last := bmcNet.Addr()
	for n := 1; n < 1<<(32-bmcNet.Bits())-1; n++ {
		last = last.Next()
	}
	first := bmcNet.Addr()
	for n := 0; n < 10; n++ {
		first = first.Next()
	}
	kc, _ := json.Marshal(map[string]any{"Dhcp4": map[string]any{
		"interfaces-config": map[string]any{"interfaces": []any{"svc0"}, "dhcp-socket-type": "udp"},
		"lease-database":    map[string]any{"type": "memfile", "persist": true, "name": "/var/lib/kea/kea-leases4.csv", "lfc-interval": 0},
		"valid-lifetime":    3600,
		"subnet4": []any{map[string]any{"id": 1, "subnet": bmcNet.String(), "pools": []any{map[string]any{"pool": first.String() + " - " + last.Prev().String()}},
			"option-data": []any{map[string]any{"name": "routers", "data": helpers.Vlans[0].Gateway}}}},
		"loggers": []any{map[string]any{"name": "kea-dhcp4", "severity": "INFO", "output-options": []any{map[string]any{"output": "/var/log/kea-dhcp4.log"}}}},
	}})
	if out, code, err := d.Exec(context.Background(), bmc.ID, "sh", "-c", "printf '%s' \"$1\" > /etc/kea/kea-dhcp4.json && kill -HUP 1", "sh", string(kc)); err != nil || code != 0 {
		t.Fatalf("the BMC DHCP's configuration: %v %d %s", err, code, out)
	}
	nvcm(15*time.Minute, "load", name, work, bp, "10.90.0.10", "10.90.2.10")
	took := r.waitConverged(25 * time.Minute)
	t.Logf("both factory OOB leaves converged %s after NVCM was loaded, through NVCM's DHCP and ZTP", took.Round(time.Second))
	for _, l := range leaves {
		out, _ := r.sub.Exec(context.Background(), l.Path, "sh", "-c", "pgrep -a dhcrelay; ip -br addr show vlan201; ip route show vrf "+day0.NVCMOOBVRF)
		t.Logf("%s: NVCM's relay and VRF OOB:\n%s", l.Hostname, indent(out))
	}

	// every BMC leases from the BMC DHCP through the relay NVCM rendered
	var macs []string
	for _, ep := range r.sub.Plan().Endpoints {
		// the BMCs behind the factory leaves (rack n01's are on the twin's management switches,
		// leased by the site's management DHCP)
		if ep.Switch != nil && ep.Switch.Config.Factory != nil {
			macs = append(macs, strings.ToLower(ep.MAC.String()))
		}
	}
	if len(macs) == 0 {
		t.Fatal("no BMC behind a factory leaf")
	}
	var leases string
	waitFor(t, 5*time.Minute, "every BMC leased through NVCM's relays", func() bool {
		leases, _, _ = d.Exec(context.Background(), bmc.ID, "cat", "/var/lib/kea/kea-leases4.csv")
		for _, m := range macs {
			if !strings.Contains(strings.ToLower(leases), m) {
				return false
			}
		}
		return true
	})
	t.Logf("the BMC DHCP's leases (%d BMCs):\n%s", len(macs), indent(leases))
	for _, l := range strings.Split(strings.TrimSpace(leases), "\n")[1:] {
		if a, err := netip.ParseAddr(strings.Split(l, ",")[0]); err != nil || !bmcNet.Contains(a) {
			t.Errorf("a lease outside VLAN 201's subnet %s: %s", bmcNet, l)
		}
	}
	r.judge()
}

// aggregationOOB configures a DSX estate's aggregation switch as the site's first switch is, through
// its NVUE in one change: VRF OOB (L3 VNI 2000 on VLAN 4002) with the BMC DHCP's port (swp125) in
// it and its connected routes exported to EVPN, and EVPN with each factory OOB leaf: multihop,
// loopback to loopback, as NVCM renders its leaves (peer group EVPN), or else on the numbered
// underlay session over the leaf's uplink, as Foundry renders its own.
func aggregationOOB(t *testing.T, r *day0Run, leaves []day0.Switch, aggLo string, multihop bool) {
	t.Helper()
	var inputs []tap.SwitchInput
	b, err := os.ReadFile(filepath.Join(r.tw.Runtime.StateDir, "stack-inputs", tap.SwitchesFile))
	if err != nil || json.Unmarshal(b, &inputs) != nil {
		t.Fatalf("stack-inputs/%s: %v", tap.SwitchesFile, err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(r.tw.Net.CAPEM)
	var agg *nvueClient
	for _, in := range inputs {
		if strings.Contains(in.Path, "oob-agg") {
			agg = &nvueClient{t: t, in: in, pw: in.NVUEPassword, http: &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{DialContext: r.p.dial,
				TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: in.Hostname}}}}
		}
	}
	if agg == nil {
		t.Fatal("no aggregation switch in stack-inputs")
	}
	evpnPeers := map[string]any{}
	for _, l := range leaves {
		if multihop {
			evpnPeers[strings.TrimSuffix(l.Loopback, "/32")] = map[string]any{"type": "numbered", "remote-as": "external", "multihop-ttl": 2, "update-source": "lo",
				"address-family": map[string]any{"ipv4-unicast": map[string]any{"enable": "off"}, "l2vpn-evpn": map[string]any{"enable": "on"}}}
			continue
		}
		// EVPN on the underlay session the twin made with the leaf's uplink
		for _, it := range l.Interfaces {
			if it.Kind == "fabric" && strings.Contains(it.Peer, "oob-agg") && it.Address != "" {
				addr, _, _ := strings.Cut(it.Address, "/")
				evpnPeers[addr] = map[string]any{"address-family": map[string]any{"l2vpn-evpn": map[string]any{"enable": "on"}}}
			}
		}
	}
	if len(evpnPeers) != len(leaves) {
		t.Fatalf("EVPN peers on the aggregation switch: %v", evpnPeers)
	}
	vrf := map[string]any{
		"evpn":     map[string]any{"enable": "on", "vlan": fmt.Sprint(day0.NVCMOOBL3VLAN), "vni": map[string]any{fmt.Sprint(day0.NVCMOOBL3VNI): map[string]any{}}},
		"loopback": map[string]any{"ip": map[string]any{"address": map[string]any{"10.254.240.250/32": map[string]any{}}}},
		"router": map[string]any{"bgp": map[string]any{"enable": "on", "address-family": map[string]any{
			"ipv4-unicast": map[string]any{"enable": "on", "redistribute": map[string]any{"connected": map[string]any{"enable": "on"}},
				"route-export": map[string]any{"to-evpn": map[string]any{"enable": "on"}}},
			"l2vpn-evpn": map[string]any{"enable": "on"}}}},
	}
	// one change, as a person's `nv config patch` of a file is (a PATCH of the root)
	patch := map[string]any{
		"evpn":      map[string]any{"enable": "on"},
		"nve":       map[string]any{"vxlan": map[string]any{"enable": "on", "source": map[string]any{"address": aggLo}}},
		"interface": map[string]any{"swp125": map[string]any{"ip": map[string]any{"vrf": day0.NVCMOOBVRF}}},
		// the twin's relay of the default VRF no longer listens on the port moved into OOB
		"service": map[string]any{"dhcp-relay": map[string]any{"default": map[string]any{"interface": map[string]any{"swp125": nil}}}},
		"vrf": map[string]any{
			day0.NVCMOOBVRF: vrf,
			"default": map[string]any{"router": map[string]any{"bgp": map[string]any{
				"address-family": map[string]any{"l2vpn-evpn": map[string]any{"enable": "on"}}, "neighbor": evpnPeers}}},
		},
	}
	out := agg.apply(map[string]any{"": patch})
	t.Logf("the aggregation switch: VRF OOB, the BMC DHCP's port in it, EVPN with the leaves: %v", out)
}

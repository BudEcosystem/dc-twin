package nvue

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// The Cumulus Linux 5.15+ grammar (version.go) against what NVIDIA's own day-0 tool configures:
// every line of net-configurator's spine template (enterprise-ras, nc-v8.0.0,
// roles/spine/templates/spine_nvue_cli.j2, which targets Cumulus 5.18.3) as rendered for a
// two-tier eBGP fabric with numbered fabric links. Interface ranges and lists (swp1-4,
// swp1,swp2) are expanded as the twin's `nv` expands them (internal/day0/nvcli) before CLIPath.
var spineCommands = []string{
	"nv set evpn state enabled",
	"nv set interface eth0 vrf mgmt",
	"nv set interface eth0 type eth",
	"nv set interface lo ipv4 address 10.255.255.1/32",
	"nv set interface lo type loopback",
	"nv set interface swp1 link breakout 2x",
	"nv set interface swp2 link breakout 2x",
	"nv set interface swp3 link speed 100G",
	"nv set interface swp1 link state up",
	"nv set interface swp2 link state up",
	"nv set interface swp3 link state up",
	"nv set interface swp4 link state up",
	"nv set interface swp1 ipv4 address 10.255.0.0/31",
	"nv set interface swp1 type swp",
	"nv set interface swp5 type swp",
	"nv set qos roce state enabled",
	"nv set qos roce mode lossless",
	"nv set qos traffic-pool default-lossy memory-percent 10",
	"nv set qos traffic-pool roce-lossless memory-percent 90",
	"nv set router bfd state enabled",
	"nv set router bfd profile underlay detect-multiplier 3",
	"nv set router bfd profile underlay min-rx-interval 300",
	"nv set router bfd profile underlay min-tx-interval 300",
	"nv set router bgp autonomous-system 65000",
	"nv set router bgp state enabled",
	"nv set router bgp router-id 10.255.255.1",
	"nv set router bgp graceful-restart mode full",
	"nv set router bgp graceful-restart restart-time 180",
	"nv set router bgp graceful-restart path-selection-deferral-time 180",
	"nv set router bgp graceful-restart stale-routes-time 180",
	"nv set router policy route-map LOOPBACK_BGP rule 10 action permit",
	"nv set router policy route-map LOOPBACK_BGP rule 10 description permit_loopback_interface_routes",
	"nv set router policy route-map LOOPBACK_BGP rule 10 match interface lo",
	"nv set router policy route-map LOOPBACK_BGP rule 10 match type ipv4",
	"nv set router policy route-map WEIGHTED_ECMP rule 10 action permit",
	"nv set router policy route-map WEIGHTED_ECMP rule 10 description enable_w_ecmp_adjustment",
	"nv set router policy route-map WEIGHTED_ECMP rule 10 set ext-community-bw multipaths",
	"nv set system ntp server 0.cumulusnetworks.pool.ntp.org association-type server",
	"nv set system ntp vrf mgmt",
	"nv set system dns server 198.51.100.31",
	"nv set system aaa authentication order local",
	"nv set system aaa class nvapply action allow",
	"nv set system aaa class nvapply command-path / permission all",
	"nv set system aaa class nvshow action allow",
	"nv set system aaa class nvshow command-path / permission ro",
	"nv set system aaa class sudo action allow",
	"nv set system aaa class sudo command-path / permission all",
	"nv set system aaa role nvue-admin class nvapply",
	"nv set system aaa role nvue-monitor class nvshow",
	"nv set system aaa role system-admin class nvapply",
	"nv set system aaa role system-admin class sudo",
	"nv set system aaa user cumulus full-name cumulus,,,",
	"nv set system aaa user cumulus role system-admin",
	"nv set system aaa user cumulus state enabled",
	"nv set system aaa user cumulus password 'Sp1ne!secret'",
	"nv set system api state enabled",
	"nv set system config auto-save state enabled",
	"nv set system hostname spine-1",
	"nv set system message pre-login 'Authorized access only'",
	"nv set system message post-login 'Welcome to spine-1'",
	"nv set system ssh-server state enabled",
	"nv set system snmp-server state enabled",
	"nv set system snmp-server listening-address localhost vrf default",
	"nv set system snmp-server readonly-community public access 10.0.0.0/8",
	"nv set system syslog server 192.0.2.10 port 514",
	"nv set system syslog server 192.0.2.10 protocol udp",
	"nv set system syslog server 192.0.2.10 vrf mgmt",
	"nv set system date-time timezone Etc/UTC",
	"nv set system wjh channel forwarding trigger l2",
	"nv set system wjh channel forwarding trigger l3",
	"nv set system wjh channel forwarding trigger tunnel",
	"nv set system wjh state enabled",
	"nv set vrf default router bgp address-family ipv4-unicast state enabled",
	"nv set vrf default router bgp address-family ipv4-unicast redistribute connected state enabled",
	"nv set vrf default router bgp address-family ipv4-unicast redistribute connected route-map LOOPBACK_BGP",
	"nv set vrf default router bgp address-family l2vpn-evpn state enabled",
	"nv set vrf default router bgp state enabled",
	"nv set vrf default router bgp neighbor 10.255.0.1 peer-group underlay",
	"nv set vrf default router bgp neighbor 10.255.0.1 type numbered",
	"nv set vrf default router bgp neighbor swp5 peer-group underlay",
	"nv set vrf default router bgp neighbor swp5 type unnumbered",
	"nv set vrf default router bgp path-selection multipath aspath-ignore enabled",
	"nv set vrf default router bgp address-family ipv4-unicast multipaths ebgp 128",
	"nv set vrf default router bgp peer-group overlay address-family ipv4-unicast state disabled",
	"nv set vrf default router bgp peer-group overlay address-family l2vpn-evpn state enabled",
	"nv set vrf default router bgp peer-group overlay bfd profile overlay",
	"nv set vrf default router bgp peer-group overlay multihop-ttl 2",
	"nv set vrf default router bgp peer-group overlay remote-as external",
	"nv set vrf default router bgp peer-group overlay update-source lo",
	"nv set vrf default router bgp peer-group underlay address-family ipv4-unicast state enabled",
	"nv set vrf default router bgp peer-group underlay address-family ipv4-unicast policy outbound route-map WEIGHTED_ECMP",
	"nv set vrf default router bgp peer-group underlay bfd profile underlay",
	"nv set vrf default router bgp peer-group underlay remote-as external",
	"nv set vrf default router bgp router-id 10.255.255.1",
}

var v5183 = Version{5, 18, 3}

// cliWordsOf splits an nv line as a shell does, for the single quotes the template uses.
func cliWordsOf(t *testing.T, line string) []string {
	t.Helper()
	var out []string
	var cur strings.Builder
	in, has := false, false
	for _, r := range line {
		switch {
		case r == '\'':
			in, has = !in, true
		case r == ' ' && !in:
			if has {
				out = append(out, cur.String())
			}
			cur.Reset()
			has = false
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if has {
		out = append(out, cur.String())
	}
	if len(out) < 3 || out[0] != "nv" || out[1] != "set" {
		t.Fatalf("not an nv set line: %q", line)
	}
	return out[2:]
}

// a factory spine running Cumulus Linux 5.18.3, its fabric ports swp1 to swp5 cabled
func spine() nos.Switch {
	sw := nos.Switch{Path: "site-1/hall-a/spine-1", Hostname: "spine-1", Build: Build(v5183), Mgmt: true,
		Factory: &nos.Factory{Install: "preinstalled"}}
	for i := 1; i <= 8; i++ {
		sw.Panel = append(sw.Panel, nos.PanelPort{Name: fmt.Sprintf("p%d", i), Speed: "400G"})
	}
	sw.Cabled = []string{"p1", "p2", "p3", "p4", "p5"}
	return sw
}

// stage is what the switch's NVUE does with one nv set: the path and value it PATCHes, checked
// in the switch's release, with its choices and passwords as a PATCH makes them.
func stage(t *testing.T, ver Version, tree Tree, line string) {
	t.Helper()
	path, v, err := CLIPathAt(ver, cliWordsOf(t, line), true)
	if err != nil {
		t.Errorf("%s: %v", line, err)
		return
	}
	if err := ValidateAt(ver, path, v); err != nil {
		t.Errorf("%s: %v", line, err)
		return
	}
	path, v = withChoices(path, v)
	path, v = hashPasswords(path, v)
	Patch(tree, path, v)
}

// net-configurator's whole spine template applies to a 5.18.3 switch and renders: FRR has its
// BFD profile, graceful restart, peer groups, route maps, neighbours, multipath; the system
// files have its users, NTP, DNS, banners and SNMP.
func TestNetConfiguratorSpine518(t *testing.T) {
	sw := spine()
	tree := Factory(sw)
	if err := ValidateAt(v5183, nil, tree); err != nil {
		t.Fatalf("the 5.18.3 factory tree: %v", err)
	}
	for _, l := range spineCommands {
		stage(t, v5183, tree, l)
	}
	if err := ValidateAt(v5183, nil, tree); err != nil {
		t.Fatalf("the staged tree: %v", err)
	}
	f, err := Render(tree, sw)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"bfd\n profile underlay\n  detect-multiplier 3\n  receive-interval 300\n  transmit-interval 300\n exit\n",
		"route-map LOOPBACK_BGP permit 10\n description permit_loopback_interface_routes\n match interface lo\nexit\n",
		"route-map WEIGHTED_ECMP permit 10\n description enable_w_ecmp_adjustment\n set extcommunity bandwidth num-multipaths\nexit\n",
		"router bgp 65000\n bgp router-id 10.255.255.1\n",
		" bgp bestpath as-path multipath-relax\n",
		" bgp graceful-restart\n",
		" bgp graceful-restart restart-time 180\n",
		" bgp graceful-restart select-defer-time 180\n",
		" bgp graceful-restart stalepath-time 180\n",
		" neighbor overlay peer-group\n neighbor overlay remote-as external\n neighbor overlay ebgp-multihop 2\n neighbor overlay update-source lo\n",
		" neighbor overlay bfd profile overlay\n",
		" neighbor underlay peer-group\n neighbor underlay remote-as external\n",
		" neighbor underlay bfd profile underlay\n",
		" neighbor 10.255.0.1 peer-group underlay\n",
		" neighbor swp5 interface peer-group underlay\n",
		" address-family ipv4 unicast\n  redistribute connected route-map LOOPBACK_BGP\n  maximum-paths 128\n",
		"  no neighbor overlay activate\n",
		"  neighbor underlay activate\n  neighbor underlay route-map WEIGHTED_ECMP out\n",
		" address-family l2vpn evpn\n  neighbor overlay activate\n",
		"  advertise-all-vni\n",
	} {
		if !strings.Contains(f.FRR, want) {
			t.Errorf("frr.conf has no %q:\n%s", want, f.FRR)
		}
	}
	for _, want := range []string{
		"iface lo inet loopback\n    address 10.255.255.1/32\n",
		"auto swp1\niface swp1\n    address 10.255.0.0/31\n",
		"    vrf mgmt\n",
		"auto swp5\niface swp5\n\n",
	} {
		if !strings.Contains(f.Interfaces, want) {
			t.Errorf("interfaces has no %q:\n%s", want, f.Interfaces)
		}
	}
	y := f.System
	if y.Hostname != "spine-1" || y.Timezone != "Etc/UTC" {
		t.Errorf("hostname, time zone: %q %q", y.Hostname, y.Timezone)
	}
	if !strings.HasPrefix(y.Users, "cumulus $6$") || !strings.Contains(y.Users, " sudo,nvshow,nvset,nvapply sudo,nvshow,nvset,nvapply cumulus") || strings.Contains(y.Users, "Sp1ne") {
		t.Errorf("users: %q", y.Users)
	}
	if y.NTP != "server 0.cumulusnetworks.pool.ntp.org" || y.NTPVRF != "mgmt" {
		t.Errorf("ntp: %q in %q", y.NTP, y.NTPVRF)
	}
	if !strings.Contains(y.DNS, "nameserver 198.51.100.31\n") {
		t.Errorf("dns: %q", y.DNS)
	}
	if !strings.Contains(y.Syslog, `target="192.0.2.10" port="514" protocol="udp" Device="mgmt"`) {
		t.Errorf("syslog: %q", y.Syslog)
	}
	if y.Banner != "Authorized access only" || y.MOTD != "Welcome to spine-1" {
		t.Errorf("messages: %q %q", y.Banner, y.MOTD)
	}
	if !strings.Contains(y.SNMP, "agentaddress udp:127.0.0.1:161\n") || !strings.Contains(y.SNMP, "rocommunity public 10.0.0.0/8\n") {
		t.Errorf("snmp: %q", y.SNMP)
	}
	// the tree keeps 5.15's grammar: what the switch serves is what was set
	if str(tree, "router", "bgp", "state") != "enabled" || sub(tree, "interface", "lo", "ipv4", "address") == nil || sub(tree, "interface", "lo", "ip") != nil {
		t.Errorf("the tree is not in 5.15's grammar: %v %v", sub(tree, "router", "bgp"), sub(tree, "interface", "lo"))
	}
}

// What 5.15 removed is refused on a 5.18.3 switch, naming its replacement; what 5.15 (or 5.11,
// 5.9) introduced is refused on the default release, naming the release; any release's grammar
// is accepted by CLIPath without one.
func TestGrammarByVersion(t *testing.T) {
	for _, c := range []struct {
		ver   Version
		words string
		want  string // "" accepted
	}{
		{v5183, "router bgp enable on", `"enable" was replaced in Cumulus Linux 5.15 and this switch runs 5.18.3: use state enabled|disabled`},
		{v5183, "evpn enable on", "use state enabled|disabled"},
		{v5183, "vrf default router bgp address-family ipv4-unicast redistribute connected enable on", "use state enabled|disabled"},
		{v5183, "interface swp1 ip address 10.0.0.1/31", `"ip" was replaced in Cumulus Linux 5.15 and this switch runs 5.18.3: use ipv4`},
		{v5183, "interface eth0 ip vrf mgmt", "interface <id> vrf"},
		{v5183, "service ntp mgmt server 4.cumulusnetworks.pool.ntp.org iburst on", "use system ntp server <host>"},
		{v5183, "service dns mgmt server 198.51.100.31", "use system dns server <address>"},
		{v5183, "service lldp tx-interval 30", "use system lldp"},
		{v5183, "system timezone US/Eastern", "use system date-time timezone"},
		{v5183, "service dhcp-relay default server 10.0.0.1", ""},
		{v5183, "router ptm enable on", ""},
		{v5183, "vrf default router bgp path-selection multipath aspath-ignore on", ""},
		{v5183, "system lldp tx-interval 30", ""},
		{DefaultVersion, "router bgp state enabled", `"state" is Cumulus Linux 5.15's NVUE and this switch runs 5.6.0: before 5.15, enable on|off`},
		{DefaultVersion, "interface swp1 ipv4 address 10.0.0.1/31", `"ipv4" is Cumulus Linux 5.15's NVUE and this switch runs 5.6.0: before 5.15, ip`},
		{DefaultVersion, "interface eth0 vrf mgmt", "before 5.15, ip vrf"},
		{DefaultVersion, "system ntp server 0.cumulusnetworks.pool.ntp.org", "before 5.15, service ntp <vrf> server <host>"},
		{DefaultVersion, "system dns server 198.51.100.31", "before 5.15, service dns <vrf> server <address>"},
		{DefaultVersion, "system snmp-server readonly-community public access any", `"snmp-server" is Cumulus Linux 5.11's NVUE`},
		{DefaultVersion, "system config auto-save enable off", `"auto-save" is Cumulus Linux 5.9's NVUE`},
		{DefaultVersion, "vrf default router bgp path-selection multipath aspath-ignore enabled", "5.15's spelling"},
		{DefaultVersion, "router bgp enable on", ""},
		{DefaultVersion, "system timezone US/Eastern", ""},
		{Version{5, 13, 0}, "system snmp-server enable on", ""},
		{Version{5, 13, 0}, "system snmp-server state enabled", "5.15"},
		{Version{}, "router bgp enable on", ""},
		{Version{}, "router bgp state enabled", ""},
		{Version{}, "interface swp1 ipv4 address 10.0.0.1/31", ""},
	} {
		_, _, err := CLIPathAt(c.ver, strings.Fields(c.words), true)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s at %v: %v", c.words, c.ver, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s at %v: %v, want an error with %q", c.words, c.ver, err, c.want)
		}
	}
	// a removed node is not offered among a refusal's alternatives
	_, _, err := CLIPathAt(v5183, strings.Fields("interface swp1 ospf"), true)
	if err == nil || strings.Contains(err.Error(), " ip,") || !strings.Contains(err.Error(), "ipv4") {
		t.Errorf("the alternatives at 5.18.3: %v", err)
	}
}

// A 5.14 tree and its 5.15 form (upgrade) render the same files; normalize undoes upgrade.
func TestUpgradeNormalize(t *testing.T) {
	old := leaf()
	tree, err := Startup(old)
	if err != nil {
		t.Fatal(err)
	}
	Patch(tree, []string{"service", "ntp", "mgmt", "server", "192.0.2.10"}, map[string]any{"iburst": "on"})
	Patch(tree, []string{"service", "dns", "default", "server", "198.51.100.31"}, map[string]any{})
	Patch(tree, []string{"service", "lldp", "tx-interval"}, 10.0)
	Patch(tree, []string{"system", "timezone"}, "US/Eastern")
	Patch(tree, []string{"interface", "vlan1000", "ip", "vrr"}, map[string]any{"enable": "on", "mac-address": "00:00:5e:00:01:01",
		"address": map[string]any{"10.30.0.254/24": map[string]any{}}, "state": map[string]any{"up": map[string]any{}}})
	up := upgrade(tree)
	if err := ValidateAt(v5183, nil, up); err != nil {
		t.Fatalf("the upgraded tree at 5.18.3: %v", err)
	}
	if err := ValidateAt(DefaultVersion, nil, up); err == nil {
		t.Errorf("the upgraded tree is valid at 5.6.0")
	}
	if str(up, "vrf", "default", "router", "bgp", "address-family", "ipv4-unicast", "redistribute", "connected", "state") != "enabled" ||
		str(up, "interface", "vlan1000", "ipv4", "vrr", "state") != "enabled" || sub(up, "interface", "vlan1000", "ipv4", "vrr", "vrr-state", "up") == nil ||
		str(up, "system", "ntp", "vrf") != "mgmt" || str(up, "system", "date-time", "timezone") != "US/Eastern" || sub(up, "service") == nil {
		t.Errorf("upgraded: %v", up)
	}
	// what normalize leaves alone: the time zone is read in either spelling
	want := normalize(tree)
	Delete(want, []string{"system", "timezone"})
	Patch(want, []string{"system", "date-time", "timezone"}, "US/Eastern")
	if back := normalize(up); !reflect.DeepEqual(back, want) {
		t.Errorf("normalize(upgrade(t)) is not t:\n%v\n%v", back, tree)
	}
	a, err := Render(tree, old)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(up, old)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the 5.14 and 5.15 forms render differently:\n%+v\n%+v", a, b)
	}
	// Startup gives a 5.18.3 switch its configuration in 5.15's grammar, rendering as before
	sw := leaf()
	sw.Build = Build(v5183)
	t2, err := Startup(sw)
	if err != nil {
		t.Fatal(err)
	}
	if sub(t2, "interface", "lo", "ipv4") == nil || str(t2, "router", "bgp", "state") != "enabled" {
		t.Errorf("Startup at 5.18.3: %v", t2)
	}
	s0, _ := Startup(leaf())
	f0, _ := Render(s0, leaf())
	f1, err := Render(t2, sw)
	if err != nil || f0.FRR != f1.FRR || f0.Interfaces != f1.Interfaces {
		t.Errorf("Startup at 5.18.3 renders differently: %v", err)
	}
}

// NVUE's REST revision workflow on a 5.18.3 switch: its build, the whole spine template PATCHed
// into a revision and applied, an old form refused with its replacement.
func TestRESTWorkflow518(t *testing.T) {
	var applied []Files
	srv := NewServer(spine(), FactoryPassword, func(ctx context.Context, f Files) (string, error) {
		applied = append(applied, f)
		return "ok", nil
	})
	sw := &fakeSwitch{shadow: map[string]string{"cumulus": "cumulus:" + sha512Crypt("cumulus", "s", 5000, false) + ":19000:0:99999:7:::"},
		groups: map[string]string{"cumulus": "cumulus sudo nvshow nvset nvapply"}}
	srv.SetRunner(sw.run)
	if err := srv.ApplyFactory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if srv.Version() != v5183 {
		t.Fatalf("version %v", srv.Version())
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c := caller{t, ts}
	auth := basic("cumulus", "cumulus")
	if st, out := c.do("GET", "/nvue_v1/system", nil, auth); st != 200 || out["build"] != "Cumulus Linux 5.18.3" {
		t.Fatalf("GET /system: %d %v", st, out)
	}
	if st, out := c.do("GET", "/nvue_v1/interface/eth0", nil, auth); st != 200 || out["vrf"] != "mgmt" || out["ipv4"] == nil {
		t.Errorf("the factory eth0 at 5.18.3: %d %v", st, out)
	}
	st, out := c.do("POST", "/nvue_v1/revision", nil, auth)
	if st != 200 || len(out) != 1 {
		t.Fatalf("POST revision: %d %v", st, out)
	}
	var rev string
	for k := range out {
		rev = k
	}
	for _, l := range spineCommands {
		path, v, err := CLIPathAt(v5183, cliWordsOf(t, l), true)
		if err != nil {
			t.Fatalf("%s: %v", l, err)
		}
		var esc []string
		for _, p := range path {
			esc = append(esc, url.PathEscape(p))
		}
		if st, out := c.do("PATCH", "/nvue_v1/"+strings.Join(esc, "/")+"?rev="+rev, v, auth); st != 200 {
			t.Fatalf("%s: %d %v", l, st, out)
		}
	}
	// 5.14's switch in a tree is taken on 5.18.3 and held as 5.15's (Q30: NVCM's 5.16.1
	// configurations and Cumulus 5.16's own startup.yaml write enable: on); the CLI refuses it
	st, out = c.do("PATCH", "/nvue_v1/router/bgp?rev="+rev, map[string]any{"enable": "on"}, auth)
	if st != 200 || out["state"] != "enabled" || out["enable"] != nil {
		t.Errorf("an old form in a 5.18.3 revision: %d %v", st, out)
	}
	if _, _, err := CLIPathAt(v5183, []string{"router", "bgp", "enable", "on"}, true); err == nil || !strings.Contains(err.Error(), "use state enabled|disabled") {
		t.Errorf("nv set router bgp enable on at 5.18.3: %v", err)
	}
	if st, out := c.do("GET", "/nvue_v1/service/ntp", nil, auth); st != 404 || !strings.Contains(fmt.Sprint(out["detail"]), "use system ntp server") {
		t.Errorf("GET an old path at 5.18.3: %d %v", st, out)
	}
	if st, out := c.do("PATCH", "/nvue_v1/revision/"+rev, map[string]any{"state": "apply", "auto-prompt": map[string]any{"ays": "ays_yes"}}, auth); st != 200 || out["state"] != "applied" {
		t.Fatalf("apply: %d %v", st, out)
	}
	last := applied[len(applied)-1]
	if !strings.Contains(last.FRR, " neighbor underlay bfd profile underlay\n") || last.System.Hostname != "spine-1" {
		t.Errorf("the applied files:\n%s", last.FRR)
	}
	if st, out := c.do("GET", "/nvue_v1/router/bgp", nil, auth); st != 200 || out["state"] != "enabled" || out["enable"] != nil {
		t.Errorf("GET router bgp: %d %v", st, out)
	}
}

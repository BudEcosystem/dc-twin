package ufm

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/adapter/nvue"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const labEstate = "../../../examples/estates/lab-model.yaml"

var actor = schema.EventSource{Class: "scenario", Principal: "ufm-e2e"}

func upLab(t *testing.T, adapters ...string) *twin.Twin {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: adapters, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	return tw
}

// modelComputerGUIDs is what UFM must report for sys_type=Computer: every InfiniBand port on a
// fabric link that is not on a switch, by normalised GUID, with its port ref.
func modelComputerGUIDs(tw *twin.Twin, fabric string) map[string]string {
	out := map[string]string{}
	tw.Engine.Read(func(m *model.Model) {
		for _, l := range m.Links {
			if l.Fabric != fabric {
				continue
			}
			for _, p := range []*model.Port{l.A, l.B} {
				if p.Kind == "infiniband" && p.Device.BaseClass != "switch" {
					out[strings.TrimPrefix(strings.ToLower(p.GUID), "0x")] = p.Ref
				}
			}
		}
	})
	return out
}

func guidOf(tw *twin.Twin, ref string) string {
	var g string
	tw.Engine.Read(func(m *model.Model) {
		if p := m.Ports[ref]; p != nil {
			g = strings.TrimPrefix(strings.ToLower(p.GUID), "0x")
		}
	})
	return g
}

func portByGUID(ports []nicoPort, guid string) *nicoPort {
	for i := range ports {
		if ports[i].GUID == guid {
			return &ports[i]
		}
	}
	return nil
}

// curl runs curl -k against the twin and logs what it printed.
func curl(t *testing.T, args ...string) string {
	t.Helper()
	path, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is not installed")
	}
	cmd := exec.Command(path, append([]string{"-sk", "-w", "\nHTTP %{http_code}\n"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("curl %v: %v\n%s", args, err, out)
	}
	shown := string(out)
	if i := strings.LastIndex(shown, "\nHTTP"); i > 700 {
		shown = shown[:700] + " …(clipped)" + shown[i:]
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = a
		if strings.ContainsAny(a, " &?*") {
			quoted[i] = "'" + a + "'"
		}
	}
	t.Logf("$ curl -sk %s\n%s", strings.Join(quoted, " "), shown)
	return string(out)
}

func statusOf(err error) int {
	if e, ok := err.(*ufmError); ok {
		return e.status
	}
	return 0
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestNICoAgainstTheLab brings the lab estate up with the UFM and NVUE adapters and drives them
// as NICo does: its UFM client's request sequence over both authentication schemes, a cable
// pull and its repair, a partition's life, the token flow; and NVUE reads before and after a
// switch tray fails.
func TestNICoAgainstTheLab(t *testing.T) {
	tw := upLab(t, "ufm", "nvue")
	a, _ := tw.Adapter("ufm").(*Adapter)
	if a == nil {
		t.Fatal("ufm adapter not running")
	}
	creds := a.Credentials()
	if len(creds) != 1 || creds[0].Fabric != "backend-ib" || creds[0].Device != "site-1/hall-a/c03/ufm-1" {
		t.Fatalf("credentials: %+v", creds)
	}
	cred := creds[0]
	if fromFile, err := ReadCredentials(tw.Runtime.StateDir); err != nil || len(fromFile) != 1 || fromFile[0].Token != cred.Token {
		t.Fatalf("credentials file: %v %+v", err, fromFile)
	}
	t.Logf("UFM backend-ib at %s user=%s (password and token from the adapter's Credentials())", cred.URL, cred.User)
	r := a.Report()
	t.Logf("report: served=%d lines=%q", r.Served, r.Lines)

	v3, err := newNicoClient(cred.URL, "", "", cred.Token)
	if err != nil {
		t.Fatal(err)
	}
	var trace []string
	v3.trace = func(method, u string, status int) {
		trace = append(trace, method+" "+strings.TrimPrefix(u, "https://"+strings.TrimPrefix(cred.URL, "https://"))+" -> "+http.StatusText(status))
	}
	basic, err := newNicoClient(cred.URL, cred.User, cred.Password, "")
	if err != nil {
		t.Fatal(err)
	}

	want := modelComputerGUIDs(tw, "backend-ib")

	t.Run("monitor iteration over /ufmRestV3 with the token", func(t *testing.T) {
		fc, err := v3.monitorIteration()
		if err != nil {
			t.Fatal(err)
		}
		if fc.version != DefaultVersion {
			t.Fatalf("version %q", fc.version)
		}
		if fc.insecure || fc.defaultMembership != "limited" {
			t.Fatalf("fabric configuration: insecure=%v default membership=%q sm=%+v", fc.insecure, fc.defaultMembership, fc.sm)
		}
		if len(fc.computerPortsByGUI) != len(want) {
			t.Fatalf("UFM reports %d Computer ports, the model has %d", len(fc.computerPortsByGUI), len(want))
		}
		for g, ref := range want {
			if _, ok := fc.computerPortsByGUI[g]; !ok {
				t.Fatalf("model port %s (GUID %s) is not in UFM", ref, g)
			}
		}
		if fc.portsByState["Active"] != len(want) {
			t.Fatalf("ports by state %v", fc.portsByState)
		}
		if p, ok := fc.partitions[DefaultPKey]; !ok || p.Name != "management" || p.QoS == nil || len(p.GUIDs) != 0 {
			t.Fatalf("default partition %+v", p)
		}
		t.Logf("version=%s sm=%+v insecure=%v default-membership=%s ports=%v partitions=%d", fc.version, fc.sm, fc.insecure,
			fc.defaultMembership, fc.portsByState, len(fc.partitions))
		t.Logf("request sequence:\n  %s", strings.Join(trace, "\n  "))
	})

	t.Run("the same calls over /ufmRest with Basic credentials", func(t *testing.T) {
		fc, err := basic.monitorIteration()
		if err != nil {
			t.Fatal(err)
		}
		if len(fc.computerPortsByGUI) != len(want) {
			t.Fatalf("ports %d", len(fc.computerPortsByGUI))
		}
		bad, _ := newNicoClient(cred.URL, cred.User, "wrong", "")
		if _, err := bad.version(); err == nil || statusOf(err) != http.StatusUnauthorized {
			t.Fatalf("a wrong password: %v", err)
		}
		// /ufmRestV3 takes a token, not user:password
		asToken, _ := newNicoClient(cred.URL, "", "", strings.TrimPrefix(basic.auth, "Basic "))
		if _, err := asToken.version(); err == nil || statusOf(err) != http.StatusUnauthorized {
			t.Fatalf("Basic user:password on /ufmRestV3: %v", err)
		}
	})

	t.Run("a pulled InfiniBand cable shows down, and up again when cleared", func(t *testing.T) {
		ref := "site-1/hall-a/a01/compute-tray-07:cx8-0"
		guid := guidOf(tw, ref)
		fv, err := tw.Engine.Inject("a01/compute-tray-07:cx8-0", "generic/link/cable-pulled", nil, "", actor)
		if err != nil || len(fv) != 1 {
			t.Fatalf("inject: %v %+v", err, fv)
		}
		ports, err := v3.listPort(nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		p := portByGUID(ports, guid)
		if p == nil || p.LogicalState != "Down" || p.PhysicalState != "Link Down" {
			t.Fatalf("after the pull: %+v", p)
		}
		active, _ := v3.listPort(nil, nil, "Active")
		if len(active) != len(want)-1 || portByGUID(active, guid) != nil {
			t.Fatalf("active ports after the pull: %d", len(active))
		}
		t.Logf("pulled %s: %+v", ref, *p)
		if _, err := tw.Engine.Clear(fv[0].ID, "", actor); err != nil {
			t.Fatal(err)
		}
		ports, _ = v3.listPort(nil, nil, "")
		if p := portByGUID(ports, guid); p == nil || p.LogicalState != "Active" || p.PhysicalState != "Link Up" {
			t.Fatalf("after clearing: %+v", p)
		}
		t.Logf("cleared: %s is Active / Link Up again", guid)
	})

	t.Run("a partition's life: bind two trays, QoS, read back, unbind until gone", func(t *testing.T) {
		var guids []string
		for _, tray := range []string{"compute-tray-01", "compute-tray-02"} {
			for _, port := range []string{"cx8-0", "cx8-1", "cx8-2", "cx8-3"} {
				guids = append(guids, guidOf(tw, "site-1/hall-a/a01/"+tray+":"+port))
			}
		}
		const pkey = 0x10
		var pcs []nicoPortConfig
		for _, g := range guids {
			pcs = append(pcs, nicoPortConfig{GUID: g, Index0: true, Membership: "full"})
		}
		if err := v3.bindPorts(nicoPartition{Name: "tenant-a", PKey: pkey, IPoIB: true}, pcs); err != nil {
			t.Fatal(err)
		}
		if err := v3.updatePartitionQoS(pkey, nicoQoS{MTULimit: 4, ServiceLevel: 5, RateLimit: 200}); err != nil {
			t.Fatal(err)
		}
		p, err := v3.getPartition(pkey, true, true)
		if err != nil {
			t.Fatal(err)
		}
		if p.Name != "api_pkey_0x10" || !p.IPoIB || len(p.GUIDs) != len(guids) || p.QoS == nil || *p.QoS != (nicoQoS{4, 5, 200}) {
			t.Fatalf("partition read back: %+v qos=%+v", p, p.QoS)
		}
		for _, g := range guids {
			if !p.GUIDs[g] {
				t.Fatalf("%s is not a member", g)
			}
		}
		k := uint16(pkey)
		members, err := v3.listPort(&k, nil, "Active")
		if err != nil || len(members) != len(guids) {
			t.Fatalf("list_port filtered by pkey: %v %d", err, len(members))
		}
		all, err := v3.listPartitions(true, false)
		if err != nil || !all[pkey].GUIDs[guids[0]] {
			t.Fatalf("list with guids_data: %v %+v", err, all[pkey])
		}
		fc, err := v3.monitorIteration()
		if err != nil || len(fc.partitions) != 2 {
			t.Fatalf("monitor sees %d partitions: %v", len(fc.partitions), err)
		}
		t.Logf("partition 0x10: name=%s ipoib=%v qos=%+v members=%d", p.Name, p.IPoIB, *p.QoS, len(p.GUIDs))
		if err := v3.unbindPorts(pkey, guids[:4]); err != nil {
			t.Fatal(err)
		}
		if p, err := v3.getPartition(pkey, true, false); err != nil || len(p.GUIDs) != 4 {
			t.Fatalf("after unbinding one tray: %v %+v", err, p)
		}
		if err := v3.unbindPorts(pkey, guids[4:]); err != nil {
			t.Fatal(err)
		}
		if _, err := v3.getPartition(pkey, true, true); !isNotFound(err) {
			t.Fatalf("an emptied partition must read as not found ({}), got %v", err)
		}
		t.Logf("unbound every GUID: GET /resources/pkeys/0x10 answers {} (NotFound)")

		// UFM's own delete, and its refusals
		if err := v3.bindPorts(nicoPartition{PKey: 0x11, IPoIB: false}, pcs[:1]); err != nil {
			t.Fatal(err)
		}
		if _, _, err := v3.execute(http.MethodDelete, "/resources/pkeys/0x11", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := v3.getPartition(0x11, false, false); !isNotFound(err) {
			t.Fatalf("deleted pkey: %v", err)
		}
		if _, _, err := v3.execute(http.MethodDelete, "/resources/pkeys/0x7fff", ""); err == nil {
			t.Fatal("the management partition was deleted")
		}
		if err := v3.bindPorts(nicoPartition{PKey: 0x12}, []nicoPortConfig{{GUID: "0123456789abcdef", Membership: "full"}}); err == nil {
			t.Fatal("a GUID that is not on the fabric was bound")
		}
		if err := v3.updatePartitionQoS(DefaultPKey, nicoQoS{MTULimit: 8, RateLimit: 2.5}); err == nil {
			t.Fatal("an MTU UFM does not support was accepted")
		}
		body, _, err := v3.execute(http.MethodGet, "/resources/pkeys", "")
		if err != nil || strings.TrimSpace(body) != `["0x7fff"]` {
			t.Fatalf("pkeys without parameters (a list): %v %s", err, body)
		}
	})

	t.Run("token flow: mint over /ufmRest, use on /ufmRestV3, revoke", func(t *testing.T) {
		hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
		req, _ := http.NewRequest(http.MethodPost, cred.URL+"/ufmRest/app/tokens", nil)
		req.SetBasicAuth(cred.User, cred.Password)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var tok struct {
			AccessToken string `json:"access_token"`
			Revoked     bool   `json:"revoked"`
			IssuedAt    int64  `json:"issued_at"`
			ExpiresIn   int64  `json:"expires_in"`
			Username    string `json:"username"`
		}
		if err := serdeDecode(string(b), &tok); err != nil || resp.StatusCode != http.StatusOK || len(tok.AccessToken) != 30 || tok.Username != cred.User {
			t.Fatalf("POST /ufmRest/app/tokens: %d %s %v", resp.StatusCode, b, err)
		}
		c, _ := newNicoClient(cred.URL, "", "", tok.AccessToken)
		if v, err := c.version(); err != nil || v != DefaultVersion {
			t.Fatalf("the minted token on /ufmRestV3: %v", err)
		}
		var form bytes.Buffer
		mw := multipart.NewWriter(&form)
		_ = mw.WriteField("token", tok.AccessToken)
		mw.Close()
		req, _ = http.NewRequest(http.MethodPost, cred.URL+"/ufmRest/app/tokens/revoke", &form)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.SetBasicAuth(cred.User, cred.Password)
		resp, err = hc.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("revoke: %v %v", err, resp)
		}
		resp.Body.Close()
		if _, err := c.version(); err == nil || statusOf(err) != http.StatusUnauthorized {
			t.Fatalf("a revoked token still works: %v", err)
		}
		if _, err := v3.version(); err != nil {
			t.Fatalf("the twin-issued token stopped working: %v", err)
		}
		t.Logf("minted %s…, used on /ufmRestV3, revoked; the twin-issued token still answers", tok.AccessToken[:6])
	})

	t.Run("requests are logged by canonical name", func(t *testing.T) {
		evs := tw.Engine.Events(0, func(e schema.Event) bool {
			return e.Request != nil && e.Request.Protocol == Protocol && e.Path == cred.Device
		})
		counts := map[string]int{}
		for _, e := range evs {
			counts[e.Request.Canonical]++
		}
		if counts["network.read"] == 0 || counts["inventory.read"] == 0 || counts["other"] == 0 || counts["session.create"] == 0 {
			t.Fatalf("canonical counts %v", counts)
		}
		t.Logf("ufm-rest requests by canonical name: %v", counts)
	})

	t.Run("curl smoke: both authentication schemes", func(t *testing.T) {
		out := curl(t, "-u", cred.User+":"+cred.Password, cred.URL+"/ufmRest/app/ufm_version")
		if !strings.Contains(out, `"ufm_release_version":"`+DefaultVersion+`"`) {
			t.Fatal(out)
		}
		out = curl(t, "-H", "Authorization: Basic "+cred.Token, cred.URL+"/ufmRestV3/app/smconf")
		if !strings.Contains(out, "m_key_per_port") {
			t.Fatal(out)
		}
		out = curl(t, "-H", "Authorization: Basic "+cred.Token, cred.URL+"/ufmRestV3/resources/ports?sys_type=Computer&system=a01-compute-tray-07")
		var ports []map[string]any
		body := out[:strings.LastIndex(out, "\nHTTP")]
		if err := json.Unmarshal([]byte(body), &ports); err != nil || len(ports) != 4 {
			t.Fatalf("ports of one system: %v", err)
		}
		out = curl(t, "-u", cred.User+":"+cred.Password, cred.URL+"/ufmRest/resources/pkeys/0x7fff?guids_data=true&qos_conf=true")
		if !strings.Contains(out, `"membership":"limited"`) {
			t.Fatal(out)
		}
		out = curl(t, "-u", cred.User+":"+cred.Password, cred.URL+"/ufmRest/resources/systems?type=switch")
		if !strings.Contains(out, `"type":"switch"`) || !strings.Contains(out, "HTTP 200") {
			t.Fatal(clip(out, 400))
		}
		out = curl(t, "-H", "Authorization: Basic not-a-token", cred.URL+"/ufmRestV3/app/ufm_version")
		if !strings.Contains(out, "HTTP 401") {
			t.Fatal(out)
		}
	})

	t.Run("NVUE on switch trays", func(t *testing.T) {
		na, _ := tw.Adapter("nvue").(*nvue.Adapter)
		if na == nil {
			t.Fatal("nvue adapter not running")
		}
		eps := na.Endpoints()
		const tray = "site-1/hall-a/a01/switch-tray-01"
		addr := eps[tray]
		if addr == "" || len(eps) != 18 {
			t.Fatalf("NVUE endpoints: %d, %s=%q", len(eps), tray, addr)
		}
		var user, pass string
		tw.Engine.Read(func(m *model.Model) {
			d := m.DeviceBy[tray]
			user, pass = d.Identity.BMCUser, d.Identity.BMCPassword
		})
		base := "https://" + addr
		out := curl(t, "-u", user+":"+pass, base+"/nvue_v1/system?rev=operational")
		if !strings.Contains(out, `"product-release":"25.02.2553"`) || !strings.Contains(out, `"hostname":"nvos-`) {
			t.Fatal(out)
		}
		upCount := func() (up, down int, sample string) {
			body := curl(t, "-u", user+":"+pass, base+"/nvue_v1/interface?rev=operational&filter_=type%3Dnvl&include=%2F*%2Ftype&include=%2F*%2Flink%2Fstate&include=%2F*%2Flink%2Fdiagnostics")
			// decoded as NICo's health collector does: link.state is an object with at most one key
			var ifs map[string]struct {
				Type string `json:"type"`
				Link struct {
					State       map[string]json.RawMessage `json:"state"`
					Diagnostics map[string]struct {
						Status string `json:"status"`
					} `json:"diagnostics"`
				} `json:"link"`
			}
			if err := json.Unmarshal([]byte(body[:strings.LastIndex(body, "\nHTTP")]), &ifs); err != nil {
				t.Fatal(err)
			}
			names := make([]string, 0, len(ifs))
			for n := range ifs {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				i := ifs[n]
				if i.Type != "nvl" {
					continue
				}
				if len(i.Link.State) != 1 {
					t.Fatalf("%s: link state %v", n, i.Link.State)
				}
				if _, ok := i.Link.State["up"]; ok {
					up++
				} else {
					down++
					if sample == "" {
						for code, d := range i.Link.Diagnostics {
							sample = n + " diagnostics " + code + ": " + d.Status
						}
					}
				}
			}
			return
		}
		if up, down, _ := upCount(); up != 144 || down != 0 {
			t.Fatalf("NVLink ports before the failure: %d up, %d down", up, down)
		}
		fv, err := tw.Engine.Inject("a01/switch-tray-01", "nvidia/nvlink/switch-tray-failure", nil, "", actor)
		if err != nil {
			t.Fatal(err)
		}
		up, down, sample := upCount()
		if up != 0 || down != 144 {
			t.Fatalf("NVLink ports after the failure: %d up, %d down", up, down)
		}
		t.Logf("after switch-tray-failure: %d up, %d down (%s)", up, down, sample)
		out = curl(t, "-u", user+":"+pass, base+"/nvue_v1/system/health")
		if !strings.Contains(out, `"status":"Not OK"`) {
			t.Fatal(out)
		}
		if _, err := tw.Engine.Clear(fv[0].ID, "", actor); err != nil {
			t.Fatal(err)
		}
		if up, _, _ := upCount(); up != 144 {
			t.Fatalf("after clearing: %d up", up)
		}
		out = curl(t, base+"/nvue_v1/system")
		if !strings.Contains(out, "HTTP 401") || !strings.Contains(out, "nginx") {
			t.Fatal(out)
		}
		out = curl(t, "-u", user+":"+pass, "-X", "PATCH", base+"/nvue_v1/system")
		if !strings.Contains(out, "HTTP 405") {
			t.Fatal(out)
		}
	})
}

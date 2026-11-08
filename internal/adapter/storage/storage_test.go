package storage_test

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	sto "github.com/BudEcosystem/dc-twin/internal/adapter/storage"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

func upLab(t *testing.T) (*twin.Twin, sto.Credential) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../../examples/estates/storage-lab.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tw, err := twin.UpEstate(est, "", reg, twin.Options{StateDir: dir, Adapters: []string{"storage"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	creds, err := sto.ReadCredentials(dir)
	if err != nil || len(creds) != 1 {
		t.Fatalf("credentials: %v %v", creds, err)
	}
	return tw, creds[0]
}

type client struct {
	t     *testing.T
	c     sto.Credential
	token string
}

func (c *client) do(method, path, body string) (int, http.Header, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.c.API+path, strings.NewReader(body))
	if c.token != "" {
		req.Header.Set("X-Auth-Token", c.token)
	} else {
		req.SetBasicAuth(c.c.User, c.c.Password)
	}
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, resp.Header, m
}

func members(doc map[string]any) []string {
	var out []string
	ms, _ := doc["Members"].([]any)
	for _, m := range ms {
		out = append(out, m.(map[string]any)["@odata.id"].(string))
	}
	return out
}

// Swordfish provisioning, as a standards-based client makes it, is the engine's: a file system
// with an export, a volume mapped to a host through an NVMe-oF connection, read back through the
// model, the API and the exporter.
func TestSwordfishProvisioning(t *testing.T) {
	tw, cred := upLab(t)
	c := &client{t: t, c: cred}
	if st, _, _ := (&client{t: t, c: sto.Credential{API: cred.API}}).do("GET", "/redfish/v1/Storage", ""); st != http.StatusUnauthorized {
		t.Fatalf("unauthenticated read: %d", st)
	}
	// a session, as python-redfish-library and sushy log in
	st, hdr, _ := c.do("POST", "/redfish/v1/SessionService/Sessions", `{"UserName":"`+cred.User+`","Password":"`+cred.Password+`"}`)
	if st != http.StatusCreated || hdr.Get("X-Auth-Token") == "" {
		t.Fatalf("session: %d", st)
	}
	c.token = hdr.Get("X-Auth-Token")
	_, _, root := c.do("GET", "/redfish/v1", "")
	if root["Storage"] == nil || root["Fabrics"] == nil {
		t.Fatalf("service root: %v", root)
	}
	sys := "/redfish/v1/Storage/hall-a-fs"
	_, _, stor := c.do("GET", sys, "")
	if n, _ := stor["Drives@odata.count"].(float64); n != 120 {
		t.Fatalf("pool drives: %v", stor["Drives@odata.count"])
	}
	_, _, pool := c.do("GET", sys+"/StoragePools/pool", "")
	data := pool["Capacity"].(map[string]any)["Data"].(map[string]any)
	if data["AllocatedBytes"].(float64) != float64(int64(8*12*15_360_000_000_000+24*30_720_000_000_000)/10*8) {
		t.Fatalf("pool capacity: %v", data)
	}
	// a file system and an export rule
	st, hdr, fs := c.do("POST", sys+"/FileSystems", `{"Name":"models","Capacity":{"Data":{"AllocatedBytes":10995116277760}}}`)
	if st != http.StatusCreated || hdr.Get("Location") == "" {
		t.Fatalf("create file system: %d %v", st, fs)
	}
	fsURI := hdr.Get("Location")
	st, _, share := c.do("POST", fsURI+"/ExportedFileShares", `{"FileSharingProtocols":["NFSv4_1"],"DefaultAccessCapabilities":["Read","Write"],"RootAccess":true,"Oem":{"DcTwin":{"Clients":"10.50.0.0/16"}}}`)
	if st != http.StatusCreated || share["RootAccess"] != true {
		t.Fatalf("export: %d %v", st, share)
	}
	// a share's URI is the rule's: deleting another leaves it naming the same rule
	var shares []string
	for _, cl := range []string{"10.60.0.0/16", "10.70.0.0/16"} {
		st, hdr, _ := c.do("POST", fsURI+"/ExportedFileShares", `{"DefaultAccessCapabilities":["Read"],"Oem":{"DcTwin":{"Clients":"`+cl+`"}}}`)
		if st != http.StatusCreated {
			t.Fatalf("export to %s: %d", cl, st)
		}
		shares = append(shares, hdr.Get("Location"))
	}
	if st, _, _ := c.do("DELETE", shares[0], ""); st/100 != 2 {
		t.Fatalf("delete share %s: %d", shares[0], st)
	}
	if st, _, sh := c.do("GET", shares[1], ""); st != http.StatusOK || !strings.Contains(fmt.Sprint(sh["Description"]), "10.70.0.0/16") {
		t.Fatalf("the other share after a delete: %d %v", st, sh)
	}
	if st, _, _ := c.do("DELETE", shares[1], ""); st/100 != 2 {
		t.Fatalf("delete share %s: %d", shares[1], st)
	}
	// a volume, a host, a connection
	st, hdr, vol := c.do("POST", sys+"/Volumes", `{"Name":"db-1","CapacityBytes":1099511627776}`)
	if st != http.StatusCreated {
		t.Fatalf("create volume: %d %v", st, vol)
	}
	volURI := hdr.Get("Location")
	st, hdr, _ = c.do("POST", "/redfish/v1/Fabrics/NVMeoF/Endpoints", `{"Identifiers":[{"DurableName":"nqn.2014-08.org.nvmexpress:uuid:0c5a1b6e-0000-4000-8000-000000000001","DurableNameFormat":"NQN"}]}`)
	if st != http.StatusCreated {
		t.Fatalf("host endpoint: %d", st)
	}
	hostEP := hdr.Get("Location")
	st, _, conn := c.do("POST", "/redfish/v1/Fabrics/NVMeoF/Connections", `{"VolumeInfo":[{"Volume":{"@odata.id":"`+volURI+`"},"AccessCapabilities":["Read","Write"]}],"Links":{"InitiatorEndpoints":[{"@odata.id":"`+hostEP+`"}]}}`)
	if st != http.StatusCreated || len(conn["Links"].(map[string]any)["TargetEndpoints"].([]any)) != 1 {
		t.Fatalf("connection: %d %v", st, conn)
	}
	v, err := tw.Engine.StorageSystem("hall-a-fs")
	if err != nil || len(v.FileSystems) != 1 || v.FileSystems[0].Exports[0].Clients != "10.50.0.0/16" || len(v.Volumes) != 1 || len(v.Volumes[0].Hosts) != 1 {
		t.Fatalf("the model: %+v %v", v, err)
	}
	// what each collection allows
	if _, hdr, _ := c.do("GET", sys+"/Volumes", ""); !strings.Contains(hdr.Get("Allow"), "POST") {
		t.Fatalf("Allow on Volumes: %q", hdr.Get("Allow"))
	}
	// a quota change, an alert, the exporter
	if st, _, _ := c.do("PATCH", fsURI, `{"Capacity":{"Data":{"AllocatedBytes":1099511627776}}}`); st != http.StatusOK {
		t.Fatalf("patch quota: %d", st)
	}
	pct := 150.0
	if err := tw.Engine.StorageAct("hall-a-fs", schema.StorageAct{Fill: "models", UsedPercent: &pct}, schema.EventSource{Class: "user", Principal: "test"}); err != nil {
		t.Fatal(err)
	}
	_, _, stor = c.do("GET", sys, "")
	conds := stor["Status"].(map[string]any)["Conditions"].([]any)
	if stor["Status"].(map[string]any)["Health"] != "Warning" || len(conds) != 1 || !strings.Contains(conds[0].(map[string]any)["Message"].(string), "over its quota") {
		t.Fatalf("status after an over-quota fill: %v", stor["Status"])
	}
	resp, err := http.Get(cred.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`storage_system_health{system="hall-a-fs"} 1`, `storage_system_filesystem_used_bytes{system="hall-a-fs",filesystem="models"} 1649267441664`,
		`storage_system_volume_hosts{system="hall-a-fs",volume="db-1"} 1`, `storage_system_alerts{system="hall-a-fs",alert="QuotaExceeded",severity="Warning"} 1`,
		`storage_system_node_up{system="hall-a-fs",node="s01-jbof-01",role="enclosure"} 1`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("metrics lack %q:\n%s", want, raw)
		}
	}
	// deletes
	if st, _, _ := c.do("DELETE", "/redfish/v1/Fabrics/NVMeoF/Connections/"+v.Volumes[0].ID, ""); st != http.StatusNoContent {
		t.Fatalf("delete connection: %d", st)
	}
	if st, _, _ := c.do("DELETE", volURI, ""); st != http.StatusNoContent {
		t.Fatalf("delete volume: %d", st)
	}
	if st, _, _ := c.do("DELETE", fsURI, ""); st != http.StatusNoContent {
		t.Fatalf("delete file system: %d", st)
	}
	_, _, fss := c.do("GET", sys+"/FileSystems", "")
	_, _, vols := c.do("GET", sys+"/Volumes", "")
	if len(members(fss)) != 0 || len(members(vols)) != 0 {
		t.Fatalf("after deletes: %v %v", fss, vols)
	}
	// a node down shows in its controller and in the system
	if err := tw.Engine.Power(engineReq("site-1/hall-a/s02/node-04")); err != nil {
		t.Fatal(err)
	}
	_, _, ctl := c.do("GET", sys+"/Controllers/s02-node-04", "")
	if ctl["Status"].(map[string]any)["State"] != "UnavailableOffline" {
		t.Fatalf("controller of a powered-off node: %v", ctl["Status"])
	}
}

func engineReq(path string) engine.Request {
	return engine.Request{Path: path, Canonical: "power.off.force", Source: schema.EventSource{Class: "user", Principal: "test"}}
}

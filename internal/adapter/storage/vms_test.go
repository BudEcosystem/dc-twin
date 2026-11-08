package storage_test

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	sto "github.com/BudEcosystem/dc-twin/internal/adapter/storage"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// vast-csi's NFS controller call sequence (CreateVolume, ControllerExpandVolume, CreateSnapshot,
// DeleteSnapshot, DeleteVolume), as its session makes it, against the vast/vms dialect: every
// object lands in the model.
func TestVMSAsVastCSICallsIt(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../../examples/estates/storage-vast.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tw, err := twin.UpEstate(est, "", reg, twin.Options{StateDir: dir, Adapters: []string{"storage"}})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	creds, _ := sto.ReadCredentials(dir)
	c := creds[0]
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	token := ""
	call := func(method, p string, body any) (int, any) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		req, _ := http.NewRequest(method, c.API+"/api/v1/"+p, rd)
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if st, _ := call("GET", "views/", nil); st != 401 {
		t.Fatalf("unauthenticated: %d", st)
	}
	_, tok := call("POST", "token/", map[string]any{"username": c.User, "password": c.Password})
	token, _ = tok.(map[string]any)["access"].(string)
	if token == "" {
		t.Fatalf("token: %v", tok)
	}
	_, vers := call("GET", "versions/?status=success", nil)
	if vers.([]any)[0].(map[string]any)["sys_version"] == "" {
		t.Fatal("no version")
	}
	// CreateVolume: views.ensure (view policy by name, view by path), quotas.ensure
	_, pol := call("GET", "viewpolicies/?name=default", nil)
	polID := pol.([]any)[0].(map[string]any)["id"]
	path := "/k8s/csi:6c1b:default:pvc-0aa1"
	if _, v := call("GET", "views/?path="+path+"&policy__name=default", nil); len(v.([]any)) != 0 {
		t.Fatalf("a view before it is made: %v", v)
	}
	st, view := call("POST", "views/", map[string]any{"path": path, "protocols": []string{"NFS"}, "policy_id": polID, "tenant_id": 1, "create_dir": true})
	if st != 201 {
		t.Fatalf("create view: %d %v", st, view)
	}
	st, quota := call("POST", "quotas/", map[string]any{"name": "csi:6c1b:default:pvc-0aa1", "path": path, "tenant_id": 1, "hard_limit": 1 << 30})
	if st != 201 || quota.(map[string]any)["hard_limit"].(float64) != 1<<30 {
		t.Fatalf("create quota: %d %v", st, quota)
	}
	qid := int(quota.(map[string]any)["id"].(float64))
	// the model holds it: a file system at the path with an export and a quota
	v, _ := tw.Engine.StorageSystem("vast-1")
	if len(v.FileSystems) != 1 || v.FileSystems[0].Path != path || v.FileSystems[0].CapacityBytes != 1<<30 || len(v.FileSystems[0].Exports) != 1 {
		t.Fatalf("the model: %+v", v.FileSystems)
	}
	// ValidateVolumeCapabilities / expand: quotas.one(name=…) is path__endswith
	_, q := call("GET", "quotas/?path__endswith=pvc-0aa1", nil)
	if len(q.([]any)) != 1 {
		t.Fatalf("quota by name: %v", q)
	}
	if st, _ := call("PATCH", "quotas/"+itoa(qid)+"/", map[string]any{"hard_limit": 2 << 30}); st != 200 {
		t.Fatalf("expand: %d", st)
	}
	// snapshots
	st, snap := call("POST", "snapshots/", map[string]any{"name": "csi:snap-1", "path": path, "tenant_id": 1})
	if st != 201 || snap.(map[string]any)["created"] == "" {
		t.Fatalf("snapshot: %d %v", st, snap)
	}
	if st, _ := call("POST", "snapshots/", map[string]any{"name": "csi:snap-1", "path": path, "tenant_id": 1}); st != 400 {
		t.Fatalf("a second snapshot of the name: %d", st)
	}
	_, has := call("GET", "snapshots/?path__contains="+strings.TrimSuffix(path, "/")+"&page_size=10", nil)
	if len(has.([]any)) != 1 {
		t.Fatalf("has_snapshots: %v", has)
	}
	sid := itoa(int(snap.(map[string]any)["id"].(float64)))
	if st, _ := call("DELETE", "snapshots/"+sid+"/", nil); st != 204 {
		t.Fatalf("delete snapshot: %d", st)
	}
	// DeleteVolume: stream check, trash API, view by path, quota by id (gone with the folder)
	if _, s := call("GET", "globalsnapstreams/?name=strm-x", nil); len(s.([]any)) != 0 {
		t.Fatal("snapshot streams")
	}
	if st, _ := call("DELETE", "folders/delete_folder/", map[string]any{"path": path, "tenant_id": 1}); st != 200 {
		t.Fatalf("trash: %d", st)
	}
	if _, v := call("GET", "views/?path="+path, nil); len(v.([]any)) != 0 {
		t.Fatalf("view after its folder went: %v", v)
	}
	if st, _ := call("DELETE", "quotas/"+itoa(qid)+"/", nil); st != 404 {
		t.Fatalf("quota after its folder went: %d", st)
	}
	if v, _ = tw.Engine.StorageSystem("vast-1"); len(v.FileSystems) != 0 {
		t.Fatalf("the model after DeleteVolume: %+v", v.FileSystems)
	}
	_, pools := call("GET", "vippools/?name=vippool-1", nil)
	if len(pools.([]any)) != 1 {
		t.Fatalf("vip pool: %v", pools)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

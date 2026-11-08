package redfish_test

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	rfa "github.com/BudEcosystem/dc-twin/internal/adapter/redfish"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

var actor = schema.EventSource{Class: "user", Principal: "test"}

// upStorage is the lab estate with c02's second server a generic storage node (a RAID controller
// in front of SAS drives, direct NVMe and an M.2 boot pair).
func upStorage(t *testing.T) (*twin.Twin, *rfa.Adapter) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../../examples/estates/lab-model.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for hi := range est.Spec.Sites[0].Halls {
		for ri := range est.Spec.Sites[0].Halls[hi].Rows {
			row := &est.Spec.Sites[0].Halls[hi].Rows[ri]
			for k := range row.Racks {
				if row.Racks[k].Name == "c02" {
					row.Racks[k].Slots = map[string]schema.SlotOverride{"server-02": {Profile: "generic/storage-node-2u"}}
				}
			}
		}
	}
	tw, err := twin.UpEstate(est, "", reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{"redfish"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	return tw, tw.Adapter("redfish").(*rfa.Adapter)
}

func send(t *testing.T, a *rfa.Adapter, d *model.Device, method, path, body string) (int, http.Header, map[string]any) {
	t.Helper()
	addr, _, _ := a.BMC(d.Path)
	req, _ := http.NewRequest(method, "https://"+addr+path, strings.NewReader(body))
	req.SetBasicAuth(d.Identity.BMCUser, d.Identity.BMCPassword)
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, resp.Header, m
}

func waitTask(t *testing.T, a *rfa.Adapter, d *model.Device, loc, want string) map[string]any {
	t.Helper()
	var task map[string]any
	for i := 0; i < 100; i++ {
		var st int
		st, _, task = send(t, a, d, "GET", loc, "")
		if st != http.StatusOK && st != http.StatusAccepted {
			t.Fatalf("GET %s: %d", loc, st)
		}
		// a monitor answers 202 exactly while its task is under way
		if (st == http.StatusAccepted) == (want == "Pending" || want == "Running") && task["TaskState"] == want {
			return task
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("task %s never reached %s: %v", loc, want, task)
	return nil
}

func members(doc map[string]any, key string) []string {
	var out []string
	arr, _ := doc[key].([]any)
	for _, x := range arr {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m["@odata.id"].(string))
		}
	}
	return out
}

// The generated tree's drives are the model's: serial, size, identifiers and the SecureErase
// action; the host's direct NVMe drives sit in a Storage whose controller does no RAID.
func TestGeneratedTreeDrives(t *testing.T) {
	tw, a := upStorage(t)
	d := dev(tw, "site-1/hall-a/c02/server-01")
	var coll map[string]any
	getJSON(t, a, d, "/redfish/v1/Systems/1/Storage", &coll)
	if got := members(coll, "Members"); len(got) != 1 || got[0] != "/redfish/v1/Systems/1/Storage/1" {
		t.Fatalf("storage members: %v", got)
	}
	var ctl map[string]any
	getJSON(t, a, d, "/redfish/v1/Systems/1/Storage/1/Controllers/0", &ctl)
	if raid, ok := ctl["SupportedRAIDTypes"].([]any); !ok || len(raid) != 0 {
		t.Fatalf("direct NVMe controller SupportedRAIDTypes: %v (want an explicit empty list)", ctl["SupportedRAIDTypes"])
	}
	var drive map[string]any
	getJSON(t, a, d, "/redfish/v1/Systems/1/Storage/1/Drives/NVME_1", &drive)
	c := d.ComponentBy("nvme-1")
	if drive["SerialNumber"] != c.Drive.Serial || drive["CapacityBytes"].(float64) != 1.92e12 || drive["Protocol"] != "NVMe" {
		t.Fatalf("drive: %v (model serial %s)", drive, c.Drive.Serial)
	}
	ids, _ := drive["Identifiers"].([]any)
	if len(ids) != 1 || !strings.EqualFold(strings.ReplaceAll(ids[0].(map[string]any)["DurableName"].(string), ":", ""), strings.TrimPrefix(c.Drive.EUI64, "0x")) {
		t.Fatalf("identifiers: %v (eui64 %s)", ids, c.Drive.EUI64)
	}
	if _, ok := drive["Actions"].(map[string]any)["#Drive.SecureErase"]; !ok {
		t.Fatalf("no SecureErase action: %v", drive["Actions"])
	}
}

// Drive.SecureErase is a task that leaves the drive erased; an erase-fails fault makes the next one
// end in an exception with the drive's contents in place.
func TestSecureEraseErasesTheDrive(t *testing.T) {
	tw, a := upStorage(t)
	d := dev(tw, "site-1/hall-a/c02/server-01")
	c := d.ComponentBy("nvme-1")
	path := "/redfish/v1/Systems/1/Storage/1/Drives/NVME_1/Actions/Drive.SecureErase"
	st, hdr, _ := send(t, a, d, "POST", path, `{"SanitizationType":"CryptographicErase"}`)
	if st != http.StatusAccepted || hdr.Get("Location") == "" {
		t.Fatalf("SecureErase: %d %v", st, hdr)
	}
	waitTask(t, a, d, hdr.Get("Location"), "Completed")
	if got := tw.Engine.DriveStatus(c).Contents; got != engine.ContentsErased {
		t.Fatalf("after the erase the drive holds %q", got)
	}
	c0 := d.ComponentBy("nvme-0")
	if _, err := tw.Engine.Inject(c0.Path, "generic/storage/erase-fails", nil, "", actor); err != nil {
		t.Fatal(err)
	}
	st, hdr, _ = send(t, a, d, "POST", strings.ReplaceAll(path, "NVME_1", "NVME_0"), `{}`)
	if st != http.StatusAccepted {
		t.Fatalf("SecureErase: %d", st)
	}
	waitTask(t, a, d, hdr.Get("Location"), "Exception")
	if got := tw.Engine.DriveStatus(c0).Contents; got != engine.ContentsInstalled {
		t.Fatalf("a failed erase left %q", got)
	}
	// a failed drive refuses an erase
	if _, err := tw.Engine.Inject(c.Path, "generic/storage/drive-failure", nil, "", actor); err != nil {
		t.Fatal(err)
	}
	var drive map[string]any
	if st, _, _ := send(t, a, d, "GET", "/redfish/v1/Systems/1/Storage/1/Drives/NVME_1", ""); st != http.StatusOK {
		t.Fatalf("a failed drive is still listed: %d", st)
	}
	getJSON(t, a, d, "/redfish/v1/Systems/1/Storage/1/Drives/NVME_1", &drive)
	if drive["Status"].(map[string]any)["State"] != "UnavailableOffline" || drive["Status"].(map[string]any)["Health"] != "Critical" {
		t.Fatalf("failed drive status: %v", drive["Status"])
	}
	if st, _, _ := send(t, a, d, "POST", path, `{}`); st != http.StatusConflict {
		t.Fatalf("erasing a failed drive: %d, want 409", st)
	}
}

// A RAID controller's volumes, as Ironic's redfish RAID interface makes them: OnReset waits for the
// host's reset behind a task; Immediate is there at once; a member's failure degrades a RAID1
// volume, and replacing it rebuilds.
func TestVolumesAsIronicMakesThem(t *testing.T) {
	tw, a := upStorage(t)
	d := dev(tw, "site-1/hall-a/c02/server-02")
	base := "/redfish/v1/Systems/1/Storage/RAID_0"
	var vc map[string]any
	getJSON(t, a, d, base+"/Volumes", &vc)
	ats, _ := vc["@Redfish.OperationApplyTimeSupport"].(map[string]any)
	if ats == nil || !strings.Contains(strings.Join(anyStrings(ats["SupportedValues"]), ","), "OnReset") {
		t.Fatalf("volume collection apply times: %v", vc)
	}
	// what each collection allows (Ironic's interop profile reads the Allow header)
	if _, hdr, _ := send(t, a, d, "GET", base+"/Volumes", ""); !strings.Contains(hdr.Get("Allow"), "POST") {
		t.Fatalf("RAID volume collection Allow: %q", hdr.Get("Allow"))
	}
	if _, hdr, _ := send(t, a, d, "GET", "/redfish/v1/Systems/1/Storage/1/Volumes", ""); hdr.Get("Allow") != "GET, HEAD" {
		t.Fatalf("direct drives' volume collection Allow: %q", hdr.Get("Allow"))
	}
	var stor map[string]any
	getJSON(t, a, d, base, &stor)
	drives := members(stor, "Drives")
	if len(drives) != 8 {
		t.Fatalf("RAID controller drives: %v", drives)
	}
	body := `{"Name":"data","RAIDType":"RAID1","Links":{"Drives":[{"@odata.id":"` + drives[0] + `"},{"@odata.id":"` + drives[1] + `"}]},"@Redfish.OperationApplyTime":"OnReset"}`
	st, hdr, _ := send(t, a, d, "POST", base+"/Volumes", body)
	if st != http.StatusAccepted {
		t.Fatalf("OnReset volume: %d", st)
	}
	task := hdr.Get("Location")
	waitTask(t, a, d, task, "Pending")
	if err := tw.Engine.Power(engine.Request{Path: d.Path, Canonical: "power.restart", Source: actor}); err != nil {
		t.Fatal(err)
	}
	waitTask(t, a, d, task, "Completed")
	getJSON(t, a, d, base+"/Volumes", &vc)
	vols := members(vc, "Members")
	if len(vols) != 1 {
		t.Fatalf("volumes after the reset: %v", vols)
	}
	var vol map[string]any
	getJSON(t, a, d, vols[0], &vol)
	if vol["RAIDType"] != "RAID1" || vol["CapacityBytes"].(float64) != 20e12 {
		t.Fatalf("volume: %v", vol)
	}
	// RAID5 at once over three more
	body = `{"RAIDType":"RAID5","Links":{"Drives":[{"@odata.id":"` + drives[2] + `"},{"@odata.id":"` + drives[3] + `"},{"@odata.id":"` + drives[4] + `"}]}}`
	if st, _, _ := send(t, a, d, "POST", base+"/Volumes", body); st != http.StatusCreated {
		t.Fatalf("Immediate RAID5: %d", st)
	}
	// RAID1 on a used drive is refused
	if st, _, _ := send(t, a, d, "POST", base+"/Volumes", `{"RAIDType":"RAID1","Links":{"Drives":[{"@odata.id":"`+drives[0]+`"},{"@odata.id":"`+drives[5]+`"}]}}`); st != http.StatusConflict {
		t.Fatalf("a drive in two volumes: %d", st)
	}
	// nor a mirror over one drive named twice
	if st, _, _ := send(t, a, d, "POST", base+"/Volumes", `{"RAIDType":"RAID1","Links":{"Drives":[{"@odata.id":"`+drives[5]+`"},{"@odata.id":"`+drives[5]+`"}]}}`); st != http.StatusConflict {
		t.Fatalf("a drive twice in one volume: %d", st)
	}
	// a member fails: degraded; replaced: rebuilding, then optimal
	hdd0 := d.ComponentBy("hdd-0")
	if _, err := tw.Engine.Inject(hdd0.Path, "generic/storage/drive-failure", nil, "", actor); err != nil {
		t.Fatal(err)
	}
	getJSON(t, a, d, vols[0], &vol)
	if vol["Status"].(map[string]any)["State"] != "Degraded" {
		t.Fatalf("RAID1 with a failed member: %v", vol["Status"])
	}
	if err := tw.Engine.ReplaceComponent(hdd0.Path, actor); err != nil {
		t.Fatal(err)
	}
	getJSON(t, a, d, vols[0], &vol)
	if vol["Status"].(map[string]any)["State"] != "Updating" {
		t.Fatalf("after the replacement: %v", vol["Status"])
	}
	deadline := time.Now().Add(60 * time.Second)
	for vol["Status"].(map[string]any)["State"] != "Enabled" && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		getJSON(t, a, d, vols[0], &vol)
	}
	if vol["Status"].(map[string]any)["State"] != "Enabled" {
		t.Fatalf("rebuild never finished: %v", vol)
	}
	// delete at once
	if st, _, _ := send(t, a, d, "DELETE", vols[0], `{}`); st != http.StatusNoContent {
		t.Fatalf("delete: %d", st)
	}
}

func anyStrings(v any) []string {
	var out []string
	if arr, ok := v.([]any); ok {
		for _, x := range arr {
			out = append(out, x.(string))
		}
	}
	return out
}

// A captured tree's drives are the model's: the Supermicro capture's 7.68 TB drives are served
// as the profile's 3.84 TB ones, with the model's serials; a pulled drive leaves the collections.
func TestCapturedDrivesAreTheModels(t *testing.T) {
	tw, a := up(t)
	d := dev(tw, "site-1/hall-a/b02/server-01")
	var coll map[string]any
	getJSON(t, a, d, "/redfish/v1/Systems/1/Storage", &coll)
	var all []string
	for _, s := range members(coll, "Members") {
		var stor map[string]any
		getJSON(t, a, d, s, &stor)
		all = append(all, members(stor, "Drives")...)
	}
	if len(all) == 0 {
		t.Fatal("no drives in the captured tree")
	}
	serials := map[string]bool{}
	for _, c := range d.Drives() {
		serials[c.Drive.Serial] = true
	}
	for _, p := range all {
		var drive map[string]any
		getJSON(t, a, d, p, &drive)
		if drive["CapacityBytes"].(float64) != 3.84e12 || !serials[drive["SerialNumber"].(string)] {
			t.Fatalf("%s: %v %v (the model's drives are 3.84 TB)", p, drive["CapacityBytes"], drive["SerialNumber"])
		}
	}
	if err := tw.Engine.PullDrive(d.Drives()[0].Path, actor); err != nil {
		t.Fatal(err)
	}
	var after []string
	getJSON(t, a, d, "/redfish/v1/Systems/1/Storage", &coll)
	for _, s := range members(coll, "Members") {
		var stor map[string]any
		getJSON(t, a, d, s, &stor)
		after = append(after, members(stor, "Drives")...)
	}
	if len(after) != len(all)-1 {
		t.Fatalf("a pulled drive: %d drives listed, want %d", len(after), len(all)-1)
	}
}

// NICo's Dell BOSS cleanup as libredfish makes it (dell.rs): find the BOSS by its id, wait for the
// Lifecycle Controller, decommission it (a JID job), then create a RAID1 over its drives.
func TestDellBOSSAsLibredfishDrivesIt(t *testing.T) {
	tw, a := up(t)
	d := dev(tw, "site-1/hall-a/c01/server-01")
	var coll map[string]any
	getJSON(t, a, d, "/redfish/v1/Systems/System.Embedded.1/Storage", &coll)
	boss := ""
	for _, m := range members(coll, "Members") {
		if strings.Contains(m, "BOSS") {
			boss = m[strings.LastIndex(m, "/")+1:]
		}
	}
	if boss != "BOSS.SL.14-1" {
		t.Fatalf("no BOSS controller among %v", members(coll, "Members"))
	}
	st, _, lc := send(t, a, d, "POST", "/redfish/v1/Managers/iDRAC.Embedded.1/Oem/Dell/DellLCService/Actions/DellLCService.GetRemoteServicesAPIStatus", `{}`)
	if st != http.StatusOK || lc["LCStatus"] != "Ready" {
		t.Fatalf("LC status: %d %v", st, lc)
	}
	st, hdr, _ := send(t, a, d, "POST", "/redfish/v1/Systems/System.Embedded.1/Storage/"+boss+"/Actions/Oem/DellStorage.ControllerDrivesDecommission", `{"@Redfish.OperationApplyTime":"Immediate"}`)
	if st != http.StatusAccepted || !strings.Contains(hdr.Get("Location"), "JID_") {
		t.Fatalf("decommission: %d %v", st, hdr)
	}
	waitTask(t, a, d, hdr.Get("Location"), "Completed")
	for _, c := range d.Drives() {
		if c.Drive.Controller != nil {
			if got := tw.Engine.DriveStatus(c).Contents; got != engine.ContentsErased {
				t.Fatalf("BOSS drive %s after decommission: %s", c.Name, got)
			}
		}
	}
	var stor map[string]any
	getJSON(t, a, d, "/redfish/v1/Systems/System.Embedded.1/Storage/"+boss, &stor)
	drives, _ := stor["Drives"].([]any)
	if len(drives) != 2 {
		t.Fatalf("BOSS drives: %v", stor["Drives"])
	}
	body, _ := json.Marshal(map[string]any{"Name": "VD_0", "RAIDType": "RAID1", "Links": map[string]any{"Drives": drives}})
	st, hdr, _ = send(t, a, d, "POST", "/redfish/v1/Systems/System.Embedded.1/Storage/"+boss+"/Volumes", string(body))
	if st != http.StatusAccepted || !strings.Contains(hdr.Get("Location"), "JID_") {
		t.Fatalf("create volume: %d %v", st, hdr)
	}
	var vc map[string]any
	getJSON(t, a, d, "/redfish/v1/Systems/System.Embedded.1/Storage/"+boss+"/Volumes", &vc)
	vols := members(vc, "Members")
	if len(vols) != 1 {
		t.Fatalf("BOSS volumes: %v", vols)
	}
	var vol map[string]any
	getJSON(t, a, d, vols[0], &vol)
	if vol["Name"] != "VD_0" || vol["RAIDType"] != "RAID1" {
		t.Fatalf("BOSS volume: %v", vol)
	}
}

func TestGeneratedVirtualMedia(t *testing.T) {
	tw, a := upStorage(t)
	d := dev(tw, "site-1/hall-a/c02/server-02")
	var mgr, cd map[string]any
	getJSON(t, a, d, "/redfish/v1/Managers/1", &mgr)
	vm, _ := mgr["VirtualMedia"].(map[string]any)
	if vm == nil {
		t.Fatalf("manager has no VirtualMedia: %v", mgr)
	}
	cdPath := "/redfish/v1/Managers/1/VirtualMedia/CD1"
	if st, _, _ := send(t, a, d, "POST", cdPath+"/Actions/VirtualMedia.InsertMedia", `{"Image":"http://images/ipa.iso","Inserted":true}`); st != http.StatusNoContent {
		t.Fatalf("InsertMedia: %d", st)
	}
	getJSON(t, a, d, cdPath, &cd)
	if cd["Inserted"] != true || cd["Image"] != "http://images/ipa.iso" || cd["ConnectedVia"] != "URI" {
		t.Fatalf("after InsertMedia: %v", cd)
	}
	send(t, a, d, "POST", cdPath+"/Actions/VirtualMedia.EjectMedia", `{}`)
	getJSON(t, a, d, cdPath, &cd)
	if cd["Inserted"] != false || cd["Image"] != nil || cd["ConnectedVia"] != "NotConnected" {
		t.Fatalf("after EjectMedia: %v", cd)
	}
}

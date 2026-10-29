package redfish_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	rfa "github.com/BudEcosystem/dc-twin/internal/adapter/redfish"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
	bmclib "github.com/bmc-toolbox/bmclib/v2"
	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/redfish"
)

func up(t *testing.T) (*twin.Twin, *rfa.Adapter) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../../examples/estates/lab-model.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, "", reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{"redfish"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	return tw, tw.Adapter("redfish").(*rfa.Adapter)
}

func dev(tw *twin.Twin, path string) *model.Device { return tw.Engine.M.DeviceBy[path] }

func connect(t *testing.T, a *rfa.Adapter, d *model.Device) *gofish.APIClient {
	t.Helper()
	addr, _, ok := a.BMC(d.Path)
	if !ok {
		t.Fatalf("no BMC for %s", d.Path)
	}
	c, err := gofish.Connect(gofish.ClientConfig{Endpoint: "https://" + addr, Username: d.Identity.BMCUser, Password: d.Identity.BMCPassword, Insecure: true, BasicAuth: false})
	if err != nil {
		t.Fatalf("gofish connect %s: %v", d.Path, err)
	}
	t.Cleanup(c.Logout)
	return c
}

func TestGofishAgainstGB300Tray(t *testing.T) {
	tw, a := up(t)
	d := dev(tw, "site-1/hall-a/a01/compute-tray-07")
	c := connect(t, a, d)
	svc := c.Service
	if svc.Vendor != "NVIDIA" || svc.Product != "GB BMC" {
		t.Fatalf("service root identity: %q %q", svc.Vendor, svc.Product)
	}
	chassis, err := svc.Chassis()
	if err != nil {
		t.Fatal(err)
	}
	var c0 *redfish.Chassis
	for _, ch := range chassis {
		if ch.ID == "Chassis_0" {
			c0 = ch
		}
	}
	if c0 == nil || c0.Model != "NVIDIA GB300" || c0.SerialNumber != d.Identity.Serial {
		t.Fatalf("Chassis_0: %+v (want model NVIDIA GB300, serial %s)", c0, d.Identity.Serial)
	}
	systems, err := svc.Systems()
	if err != nil {
		t.Fatal(err)
	}
	var sys *redfish.ComputerSystem
	for _, s := range systems {
		if s.ID == "System_0" {
			sys = s
		}
	}
	if sys == nil || sys.UUID != d.Identity.UUID || sys.PowerState != redfish.OnPowerState {
		t.Fatalf("System_0: %+v", sys)
	}
	mgrs, _ := svc.Managers()
	found := false
	for _, m := range mgrs {
		eths, _ := m.EthernetInterfaces()
		for _, e := range eths {
			if strings.EqualFold(e.MACAddress, d.Identity.BMCMAC) {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no manager interface carries the BMC MAC %s", d.Identity.BMCMAC)
	}
	// power off through Redfish: the engine changes, and the request is logged by canonical name
	since := tw.Engine.Seq()
	if err := sys.Reset(redfish.ForceOffResetType); err != nil {
		t.Fatal(err)
	}
	if got := tw.Engine.DeviceView(d)["power"]; got != model.Off {
		t.Fatalf("engine power after ForceOff: %v", got)
	}
	evs := tw.Engine.Events(since, func(e schema.Event) bool {
		return e.Request != nil && e.Request.Canonical == "power.off.force" && e.Request.Protocol == "redfish" && e.Path == d.Path
	})
	if len(evs) != 1 {
		t.Fatalf("want one power.off.force request event, got %d", len(evs))
	}
	sys2, _ := redfish.GetComputerSystem(c, sys.ODataID)
	if sys2.PowerState != redfish.OffPowerState {
		t.Fatalf("Redfish PowerState after ForceOff: %s", sys2.PowerState)
	}
	// a leak at the tray reaches its Redfish leak detectors
	if _, err := tw.Engine.Inject(d.Path, "generic/leak/tray-detector", nil, "", schema.EventSource{Class: "user"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	var ld map[string]any
	getJSON(t, a, d, "/redfish/v1/Chassis/Chassis_0/ThermalSubsystem/LeakDetection/LeakDetectors/Chassis_0_LeakDetector_0_Manifold", &ld)
	if ld["DetectorState"] != "Critical" {
		t.Fatalf("leak detector: %v", ld["DetectorState"])
	}
	var root map[string]any
	getJSON(t, a, d, "/redfish/v1/Chassis/Chassis_0", &root)
	if root["Status"].(map[string]any)["HealthRollup"] != "Critical" {
		t.Fatalf("chassis health rollup: %v", root["Status"])
	}
}

func getJSON(t *testing.T, a *rfa.Adapter, d *model.Device, path string, v any) {
	t.Helper()
	addr, _, _ := a.BMC(d.Path)
	req, _ := http.NewRequest("GET", "https://"+addr+path, nil)
	req.SetBasicAuth(d.Identity.BMCUser, d.Identity.BMCPassword)
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatal(err)
	}
}

func TestEveryDialectAnswersWithItsIdentity(t *testing.T) {
	tw, a := up(t)
	cases := map[string][2]string{ // device → vendor, product (as libredfish keys on them)
		"site-1/hall-a/a01/compute-tray-01":       {"NVIDIA", "GB BMC"},
		"site-1/hall-a/a02/compute-tray-01":       {"NVIDIA", "GB200 NVL"},
		"site-1/hall-a/a01/switch-tray-01":        {"NVIDIA", "P3809"},
		"site-1/hall-a/a01/compute-tray-01/bf3-0": {"Nvidia", ""},
		"site-1/hall-a/b01/server-01":             {"Dell", "Integrated Dell Remote Access Controller"},
		"site-1/hall-a/b02/server-01":             {"", ""},
		"site-1/hall-a/c01/server-02":             {"HPE", "ProLiant DL385 Gen10 Plus v2"},
		"site-1/hall-a/b05/server-01":             {"Lenovo", ""},
		"site-1/hall-a/c02/server-01":             {"dc-twin", "Generic BMC"},
	}
	for path, want := range cases {
		d := dev(tw, path)
		if d == nil {
			t.Fatalf("no device %s", path)
		}
		c := connect(t, a, d)
		if c.Service.Vendor != want[0] || (want[1] != "" && c.Service.Product != want[1]) {
			t.Errorf("%s: vendor %q product %q, want %v", path, c.Service.Vendor, c.Service.Product, want)
		}
		systems, err := c.Service.Systems()
		if err != nil || len(systems) == 0 {
			t.Errorf("%s: systems: %v", path, err)
			continue
		}
		t.Logf("%-42s %-8s %-40s system %s serial %q", path, c.Service.Vendor, c.Service.Product, systems[0].ID, systems[0].SerialNumber)
	}
	// the power shelf answers as Lite-On by its chassis
	shelf := dev(tw, "site-1/hall-a/a01/power-shelf-01")
	c := connect(t, a, shelf)
	ch, _ := c.Service.Chassis()
	if len(ch) == 0 || ch[0].Manufacturer != "LITE-ON TECHNOLOGY CORP." || ch[0].SerialNumber != shelf.Identity.Serial {
		t.Fatalf("liteon shelf chassis: %+v", ch)
	}
}

func TestBmclibPowerAcrossVendors(t *testing.T) {
	tw, a := up(t)
	for _, path := range []string{"site-1/hall-a/b01/server-01", "site-1/hall-a/b02/server-02", "site-1/hall-a/c02/server-02", "site-1/hall-a/b03/server-01"} {
		d := dev(tw, path)
		addr, _, _ := a.BMC(path)
		host, port, _ := strings.Cut(addr, ":")
		cl := bmclib.NewClient(host, d.Identity.BMCUser, d.Identity.BMCPassword, bmclib.WithRedfishPort(port), bmclib.WithRedfishUseBasicAuth(true))
		cl.Registry.Drivers = cl.Registry.Using("redfish")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := cl.Open(ctx); err != nil {
			cancel()
			t.Fatalf("%s: bmclib open: %v", path, err)
		}
		st, err := cl.GetPowerState(ctx)
		if err != nil || !strings.EqualFold(st, "on") {
			t.Fatalf("%s: power state %q %v", path, st, err)
		}
		if ok, err := cl.SetPowerState(ctx, "off"); !ok || err != nil {
			t.Fatalf("%s: power off: %v %v", path, ok, err)
		}
		if got := tw.Engine.DeviceView(d)["power"]; got != model.Off && got != model.PoweringOff {
			t.Fatalf("%s: engine power %v", path, got)
		}
		if ok, err := cl.SetBootDevice(ctx, "pxe", false, true); !ok || err != nil {
			t.Fatalf("%s: boot device: %v %v", path, ok, err)
		}
		bt := tw.Engine.DeviceView(d)["boot"].(map[string]any)
		if bt["overrideTarget"] != "Pxe" {
			t.Fatalf("%s: boot override %v", path, bt)
		}
		cl.Close(ctx)
		cancel()
		t.Logf("bmclib %s: power on→off, bootdev pxe → engine %v / %v", path, tw.Engine.DeviceView(d)["power"], bt["overrideTarget"])
	}
}

func TestSiteGatewayReachesABMCByHostName(t *testing.T) {
	tw, a := up(t)
	d := dev(tw, "site-1/hall-a/a01/compute-tray-02")
	_, host, _ := a.BMC(d.Path)
	proxy, _ := url.Parse("http://" + tw.Net.Gateway)
	cl := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, _ := http.NewRequest("GET", "https://"+host+"/redfish/v1/Systems/System_0", nil)
	req.SetBasicAuth(d.Identity.BMCUser, d.Identity.BMCPassword)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("through the gateway: %s", resp.Status)
	}
	req2, _ := http.NewRequest("GET", "https://10.99.99.99/redfish/v1", nil)
	if resp2, err := cl.Do(req2); err == nil {
		resp2.Body.Close()
		t.Fatal("the gateway must refuse addresses that are not estate endpoints")
	}
}

// BootProgress follows the model: None while off, OSRunning once booted, with the time the model
// changed. Ironic waits for it after a BIOS reboot; a frozen captured value stalls the step.
func TestBootProgressFollowsPower(t *testing.T) {
	tw, a := up(t)
	type progress struct {
		BootProgress struct{ LastState, LastStateTime string }
		PowerState   string
	}
	for path, sys := range map[string]string{
		"site-1/hall-a/c02/server-01":       "/redfish/v1/Systems/1",        // generated tree
		"site-1/hall-a/b02/server-01":       "/redfish/v1/Systems/1",        // Supermicro capture
		"site-1/hall-a/a01/compute-tray-07": "/redfish/v1/Systems/System_0", // GB300 capture
	} {
		d := dev(tw, path)
		read := func() progress {
			var p progress
			getJSON(t, a, d, sys, &p)
			return p
		}
		waitPower := func(want string) progress {
			deadline := time.Now().Add(2 * time.Minute)
			for {
				p := read()
				if p.PowerState == want || time.Now().After(deadline) {
					return p
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		if p := waitPower("On"); p.BootProgress.LastState != "OSRunning" {
			t.Fatalf("%s on: BootProgress %+v", path, p.BootProgress)
		}
		src := schema.EventSource{Class: "operator", Principal: "test"}
		if err := tw.Engine.Power(engine.Request{Path: d.Path, Canonical: "power.off.force", Source: src}); err != nil {
			t.Fatal(err)
		}
		off := waitPower("Off")
		if off.BootProgress.LastState != "None" {
			t.Fatalf("%s off: BootProgress %+v", path, off.BootProgress)
		}
		offAt, err := time.Parse(time.RFC3339, off.BootProgress.LastStateTime)
		if err != nil {
			t.Fatalf("%s: LastStateTime %q: %v", path, off.BootProgress.LastStateTime, err)
		}
		if err := tw.Engine.Power(engine.Request{Path: d.Path, Canonical: "power.on", Source: src}); err != nil {
			t.Fatal(err)
		}
		// On, then the profile's POST (boot.postSeconds, scaled), then the OS
		seen := map[string]bool{}
		on := waitPower("On")
		for deadline := time.Now().Add(2 * time.Minute); on.BootProgress.LastState != "OSRunning" && time.Now().Before(deadline); {
			seen[on.BootProgress.LastState] = true
			time.Sleep(100 * time.Millisecond)
			on = read()
		}
		onAt, _ := time.Parse(time.RFC3339, on.BootProgress.LastStateTime)
		if on.BootProgress.LastState != "OSRunning" || onAt.Before(offAt) {
			t.Fatalf("%s on again: BootProgress %+v (off at %s), seen %v", path, on.BootProgress, off.BootProgress.LastStateTime, seen)
		}
		if behaviourPost(d) > 0 && !seen["MemoryInitializationStarted"] {
			t.Fatalf("%s: BootProgress never showed POST on the way to OSRunning: %v", path, seen)
		}
	}
}

// The generated tree carries what an inspector records: CPU architecture and cores, and drives.
func TestGeneratedTreeInventory(t *testing.T) {
	tw, a := up(t)
	d := dev(tw, "site-1/hall-a/c02/server-01")
	var cpu struct {
		ProcessorArchitecture, InstructionSet string
		TotalCores, TotalThreads              int
	}
	getJSON(t, a, d, "/redfish/v1/Systems/1/Processors/CPU_0", &cpu)
	if cpu.ProcessorArchitecture != "x86" || cpu.InstructionSet != "x86-64" || cpu.TotalCores != 32 || cpu.TotalThreads != 64 {
		t.Fatalf("CPU_0: %+v", cpu)
	}
	var coll struct {
		Members []struct {
			ID string `json:"@odata.id"`
		}
	}
	getJSON(t, a, d, "/redfish/v1/Systems/1/Storage", &coll)
	if len(coll.Members) != 1 {
		t.Fatalf("Storage members: %+v", coll.Members)
	}
	var st struct {
		Drives []struct {
			ID string `json:"@odata.id"`
		}
	}
	getJSON(t, a, d, coll.Members[0].ID, &st)
	if len(st.Drives) != 2 {
		t.Fatalf("drives: %+v", st.Drives)
	}
	var drive struct {
		CapacityBytes int64
		Protocol      string
	}
	getJSON(t, a, d, st.Drives[0].ID, &drive)
	if drive.CapacityBytes != 1920000000000 || drive.Protocol != "NVMe" {
		t.Fatalf("drive: %+v", drive)
	}
}

// A BMC's clock runs with the twin's, in its Manager's own offset; a stack that sets it (NICo
// does when it finds drift) sets where it runs from, not a value that then stands still.
func TestBMCClockRuns(t *testing.T) {
	tw, a := up(t)
	for path, mgr := range map[string]string{
		"site-1/hall-a/b01/server-01": "/redfish/v1/Managers/iDRAC.Embedded.1", // Dell capture
		"site-1/hall-a/c02/server-01": "/redfish/v1/Managers/1",                // generated tree
	} {
		d := dev(tw, path)
		var m struct{ DateTime, DateTimeLocalOffset string }
		getJSON(t, a, d, mgr, &m)
		got, err := time.Parse(time.RFC3339, m.DateTime)
		if err != nil {
			t.Fatalf("%s DateTime %q: %v", path, m.DateTime, err)
		}
		if drift := got.Sub(tw.Engine.Now()); drift > 5*time.Second || drift < -5*time.Second {
			t.Fatalf("%s DateTime %s is %v from the twin's clock", path, m.DateTime, drift)
		}
		if _, off := got.Zone(); m.DateTimeLocalOffset != "" && got.Format("-07:00") != m.DateTimeLocalOffset && !(off == 0 && m.DateTimeLocalOffset == "Z") {
			t.Fatalf("%s DateTime %s is not in its offset %s", path, m.DateTime, m.DateTimeLocalOffset)
		}

		set := tw.Engine.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		addr, _, _ := a.BMC(d.Path)
		req, _ := http.NewRequest("PATCH", "https://"+addr+mgr, strings.NewReader(`{"DateTime":"`+set+`"}`))
		req.SetBasicAuth(d.Identity.BMCUser, d.Identity.BMCPassword)
		req.Header.Set("Content-Type", "application/json")
		cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s PATCH DateTime: %d", path, resp.StatusCode)
		}
		getJSON(t, a, d, mgr, &m)
		got, _ = time.Parse(time.RFC3339, m.DateTime)
		if drift := got.Sub(tw.Engine.Now().Add(time.Hour)); drift > 5*time.Second || drift < -5*time.Second {
			t.Fatalf("%s after setting %s: DateTime %s", path, set, m.DateTime)
		}
	}
}

// A host's captured DPU resources follow the model's DPUs: a GB300 tray (one BlueField-3) shows
// one riser, a GB200 tray two, each with that DPU's own serial (what its BMC serves), and the
// Supermicro capture's BlueField-2, which the model's server lacks, is absent.
func TestHostDPUsAreTheModelsDPUs(t *testing.T) {
	tw, a := up(t)
	status := func(d *model.Device, path string) int {
		addr, _, _ := a.BMC(d.Path)
		req, _ := http.NewRequest("GET", "https://"+addr+path, nil)
		req.SetBasicAuth(d.Identity.BMCUser, d.Identity.BMCPassword)
		cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	members := func(d *model.Device, path string) string {
		var c struct{ Members []map[string]string }
		getJSON(t, a, d, path, &c)
		var ids []string
		for _, m := range c.Members {
			ids = append(ids, m["@odata.id"])
		}
		return strings.Join(ids, " ")
	}
	for tray, n := range map[string]int{"site-1/hall-a/a01/compute-tray-07": 1, "site-1/hall-a/a02/compute-tray-01": 2} {
		d := dev(tw, tray)
		chassis := members(d, "/redfish/v1/Chassis")
		for i := 1; i <= 2; i++ {
			riser := fmt.Sprintf("/redfish/v1/Chassis/Riser_Slot%d_BlueField_3_DPU", i)
			if i > n {
				if strings.Contains(chassis, riser) || status(d, riser) != http.StatusNotFound || status(d, riser+"/NetworkAdapters") != http.StatusNotFound {
					t.Fatalf("%s has %d DPU(s) but serves %s", tray, n, riser)
				}
				continue
			}
			var r struct{ SerialNumber string }
			getJSON(t, a, d, riser, &r)
			want := dev(tw, fmt.Sprintf("%s/bf3-%d", tray, i-1)).Identity.Serial
			if !strings.Contains(chassis, riser) || r.SerialNumber != want {
				t.Fatalf("%s %s: serial %q, want the DPU's %q (in collection: %v)", tray, riser, r.SerialNumber, want, strings.Contains(chassis, riser))
			}
			var na struct{ SerialNumber string }
			getJSON(t, a, d, riser+"/NetworkAdapters/"+strings.TrimPrefix(riser, "/redfish/v1/Chassis/"), &na)
			if na.SerialNumber != want {
				t.Fatalf("%s %s network adapter serial %q, want %q", tray, riser, na.SerialNumber, want)
			}
		}
	}
	smc := dev(tw, "site-1/hall-a/b02/server-01")
	for _, p := range []string{"/redfish/v1/Chassis/1/NetworkAdapters/10", "/redfish/v1/Systems/1/EthernetInterfaces/11"} {
		if status(smc, p) != http.StatusNotFound {
			t.Fatalf("Supermicro serves %s, a DPU its model lacks", p)
		}
	}
	if m := members(smc, "/redfish/v1/Chassis/1/NetworkAdapters"); strings.Contains(m, "/NetworkAdapters/10") || !strings.Contains(m, "/NetworkAdapters/9") {
		t.Fatalf("Supermicro NetworkAdapters: %s", m)
	}
}

// iDRAC schedules a job for a PATCH of Bios/Settings whatever attributes it carries: 202 and the
// job's Location, which libredfish's Dell setup reads (NICo's machine_setup).
func TestDellBiosPatchSchedulesAJob(t *testing.T) {
	tw, a := up(t)
	d := dev(tw, "site-1/hall-a/b01/server-01")
	addr, _, _ := a.BMC(d.Path)
	body := `{"@Redfish.SettingsApplyTime":{"ApplyTime":"OnReset"},"Attributes":{"ProcVirtualization":"Disabled"}}`
	req, _ := http.NewRequest("PATCH", "https://"+addr+"/redfish/v1/Systems/System.Embedded.1/Bios/Settings", strings.NewReader(body))
	req.SetBasicAuth(d.Identity.BMCUser, d.Identity.BMCPassword)
	req.Header.Set("Content-Type", "application/json")
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusAccepted || !strings.Contains(loc, "JID_") {
		t.Fatalf("PATCH Bios/Settings: %d, Location %q", resp.StatusCode, loc)
	}
	var job struct{ Id string }
	getJSON(t, a, d, loc, &job)
	if !strings.HasSuffix(loc, "/"+job.Id) {
		t.Fatalf("job at %s answers Id %q", loc, job.Id)
	}
}

// A BlueField's host PF MAC (its OEM BaseMAC, written without separators) is its own, not the
// capture's, so a stack keying machines on it (NICo's HostPFMacAddress) sees one per DPU; and a
// UEFI device path names a NIC by the MAC the rest of the tree gives that NIC.
func TestDPUMACsAreTheirOwn(t *testing.T) {
	tw, a := up(t)
	seen := map[string]string{}
	for _, p := range []string{"site-1/hall-a/a01/compute-tray-01/bf3-0", "site-1/hall-a/a01/compute-tray-02/bf3-0"} {
		d := dev(tw, p)
		var oem struct{ BaseMAC string }
		getJSON(t, a, d, "/redfish/v1/Systems/Bluefield/Oem/Nvidia", &oem)
		if len(oem.BaseMAC) != 12 || oem.BaseMAC == "5c257308f352" {
			t.Fatalf("%s BaseMAC %q is the capture's or malformed", p, oem.BaseMAC)
		}
		if other, dup := seen[oem.BaseMAC]; dup {
			t.Fatalf("%s and %s share BaseMAC %s", p, other, oem.BaseMAC)
		}
		seen[oem.BaseMAC] = p
		var eth struct{ MACAddress string }
		getJSON(t, a, d, "/redfish/v1/Systems/Bluefield/EthernetInterfaces/eth0", &eth)
		var boot struct{ UefiDevicePath string }
		getJSON(t, a, d, "/redfish/v1/Systems/Bluefield/BootOptions/Boot0000", &boot)
		want := "MAC(" + strings.ToUpper(strings.ReplaceAll(eth.MACAddress, ":", "")) + ","
		if eth.MACAddress == "5c:25:73:08:f3:62" || !strings.Contains(boot.UefiDevicePath, want) {
			t.Fatalf("%s: eth0 %s, Boot0000 %s (want %s)", p, eth.MACAddress, boot.UefiDevicePath, want)
		}
	}
}

// A GB tray boots from its DPU by the DPU's host PF MAC: the tray's UEFI network boot options
// for its BlueField name the BaseMAC that DPU's own BMC serves, in the option's name and in its
// device path alike (NICo's set_boot_order_dpu_first matches "UEFI HTTPv4 (MAC:<BaseMAC>)").
func TestTrayBootsFromItsDPUByTheDPUsMAC(t *testing.T) {
	tw, a := up(t)
	for _, p := range []string{"site-1/hall-a/a01/compute-tray-01", "site-1/hall-a/a01/compute-tray-02"} {
		var oem struct{ BaseMAC string }
		getJSON(t, a, dev(tw, p+"/bf3-0"), "/redfish/v1/Systems/Bluefield/Oem/Nvidia", &oem)
		want := strings.ToUpper(oem.BaseMAC)
		d := dev(tw, p)
		found := false
		for _, id := range []string{"Boot000B", "Boot000C", "Boot000D", "Boot000E"} {
			var bo struct{ DisplayName, UefiDevicePath string }
			getJSON(t, a, d, "/redfish/v1/Systems/System_0/BootOptions/"+id, &bo)
			if !strings.Contains(bo.DisplayName, "(MAC:"+want+")") || !strings.Contains(bo.UefiDevicePath, "MAC("+want+",") {
				t.Errorf("%s %s: %q %q, want its DPU's host PF %s in both", p, id, bo.DisplayName, bo.UefiDevicePath, want)
			}
			found = found || strings.HasPrefix(bo.DisplayName, "UEFI HTTPv4")
		}
		if !found {
			t.Errorf("%s: no UEFI HTTPv4 option for its DPU", p)
		}
		// the tray's other NICs keep one derived MAC in name and path
		var usb struct{ DisplayName, UefiDevicePath string }
		getJSON(t, a, d, "/redfish/v1/Systems/System_0/BootOptions/Boot0005", &usb)
		i := strings.Index(usb.DisplayName, "(MAC:")
		if i < 0 || !strings.Contains(usb.UefiDevicePath, "MAC("+usb.DisplayName[i+5:i+17]+",") || strings.Contains(usb.DisplayName, "EEA3AB6B5303") {
			t.Errorf("%s Boot0005: %q %q, want the same derived MAC in both", p, usb.DisplayName, usb.UefiDevicePath)
		}
	}
}

// behaviourPost is a profile's boot.postSeconds (0 when it has none).
func behaviourPost(d *model.Device) float64 {
	if b, ok := d.Profile.Spec.Behaviours["boot"]; ok {
		switch v := b["postSeconds"].(type) {
		case float64:
			return v
		case int:
			return float64(v)
		}
	}
	return 0
}

// A firmware update whose task is still running when its BMC is reset is lost with it (D53):
// once the BMC answers again, TaskService reports the task ended in Exception with TaskAborted,
// and nothing was staged.
func TestFirmwareTaskAbortedByBMCReset(t *testing.T) {
	tw, a := up(t)
	d := dev(tw, "site-1/hall-a/c02/server-01") // generated tree
	addr, _, _ := a.BMC(d.Path)
	cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, _ := http.NewRequest("POST", "https://"+addr+"/redfish/v1/UpdateService/Actions/UpdateService.SimpleUpdate",
		strings.NewReader(`{"ImageURI":"http://images.example/bmc-9.9.9.bin"}`))
	req.SetBasicAuth(d.Identity.BMCUser, d.Identity.BMCPassword)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusAccepted || loc == "" {
		t.Fatalf("SimpleUpdate: %d, Location %q", resp.StatusCode, loc)
	}
	// the task takes 20 s at the estate's timeScale (1 s); the BMC goes down 1 s (50 ms) after
	if err := tw.Engine.ResetBMC(engine.Request{Path: d.Path, Canonical: "bmc.reset", Source: schema.EventSource{Class: "stack", Principal: "test"}}); err != nil {
		t.Fatal(err)
	}
	type taskDoc struct {
		TaskState, TaskStatus string
		Messages              []struct{ MessageId string }
	}
	var task taskDoc
	went, back := false, false
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline) && !back; time.Sleep(100 * time.Millisecond) {
		up, _, _ := tw.Engine.BMCBehaviour(d)
		if !up {
			went = true
			continue
		}
		if !went {
			continue
		}
		req, _ := http.NewRequest("GET", "https://"+addr+loc, nil)
		req.SetBasicAuth(d.Identity.BMCUser, d.Identity.BMCPassword)
		resp, err := cl.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && json.Unmarshal(body, &task) == nil {
			back = true
		}
	}
	if !back {
		t.Fatalf("the BMC did not go down and come back (went down %v)", went)
	}
	if task.TaskState != "Exception" || task.TaskStatus != "Critical" || len(task.Messages) == 0 || task.Messages[0].MessageId != "TaskEvent.1.0.TaskAborted" {
		t.Fatalf("after the BMC's reset: task %+v, want Exception/Critical/TaskAborted", task)
	}
	time.Sleep(2 * time.Second) // past the task's own completion time
	getJSON(t, a, d, loc, &task)
	if task.TaskState != "Exception" {
		t.Fatalf("the lost task later reports %s", task.TaskState)
	}
	tw.Engine.Read(func(*model.Model) {
		for comp, fw := range d.State.Firmware {
			if fw.Staged != "" || fw.Version == "bmc-9.9.9" {
				t.Errorf("%s: version %q staged %q after a lost update", comp, fw.Version, fw.Staged)
			}
		}
	})
}

// A device serves the firmware levels its profile states (D35): a dialect over its own product's
// capture serves the capture's levels, which every profile on it states as its baselines, and a
// dialect presenting another product's capture (the GB300 BMC over the GB200 capture) serves the
// profile's. So a profile's firmware list is what a reader of the device's FirmwareInventory finds.
func TestDevicesServeTheirProfilesFirmware(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	for _, estate := range []string{"lab-model.yaml", "nvl72-nico.yaml", "mixed-hall.yaml"} {
		est, _, err := compile.LoadEstate("../../../examples/estates/" + estate)
		if err != nil {
			t.Fatal(err)
		}
		tw, err := twin.UpEstate(est, "", reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{"redfish"}})
		if err != nil {
			t.Fatal(err)
		}
		a := tw.Adapter("redfish").(*rfa.Adapter)
		checked := map[string]bool{} // one device per profile
		for _, d := range tw.Engine.M.Devices {
			if !d.HasBMC() || checked[d.ProfileName] {
				continue
			}
			spec, _ := reg.Dialect(d.Dialect)
			if spec == nil || spec.Spec.Backend != "mockup" {
				continue
			}
			checked[d.ProfileName] = true
			baseline := map[string]string{}
			for _, fw := range d.Profile.Spec.Firmware {
				baseline[fw.Component] = fw.Baseline
			}
			for id, comp := range spec.Spec.Firmware {
				want, listed := baseline[comp]
				doc, ok := a.Get(d.Path, "/redfish/v1/UpdateService/FirmwareInventory/"+id)
				if !listed || !ok {
					continue
				}
				if got, _ := doc["Version"].(string); got != want {
					t.Errorf("%s (%s, dialect %s): %s serves %q, its profile's %s baseline is %q", estate, d.ProfileName, d.Dialect, id, got, comp, want)
				}
			}
		}
		tw.Down()
	}
}

// A GB tray's GPU firmware inventory reports the model's GPUs (the capture has it Absent).
func TestGBTrayGPUFirmwareFollowsItsGPUs(t *testing.T) {
	tw, a := up(t)
	d := dev(tw, "site-1/hall-a/a01/compute-tray-01")
	for i := 0; i < 4; i++ {
		for _, id := range []string{"HGX_FW_GPU_", "HGX_InfoROM_GPU_"} {
			doc, ok := a.Get(d.Path, "/redfish/v1/UpdateService/FirmwareInventory/"+id+strconv.Itoa(i))
			st, _ := doc["Status"].(map[string]any)
			if !ok || st["State"] != "Enabled" || st["Health"] != "OK" {
				t.Errorf("%s%d: status %v", id, i, st)
			}
		}
	}
}

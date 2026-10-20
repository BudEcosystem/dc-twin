package rms

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/BudEcosystem/dc-twin/internal/adapter/rms/rmspb"
	"github.com/BudEcosystem/dc-twin/internal/adapter/rms/rmspb/rmsv2pb"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const labEstate = "../../../examples/estates/lab-model.yaml"

func up(t *testing.T) (*twin.Twin, rmspb.RackManagerClient, rmsv2pb.RackManagerV2Client) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{Name}, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	a := tw.Adapter(Name).(*Adapter)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(tw.Net.CAPEM)
	cc, err := grpc.NewClient(a.Address(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: "rms"})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return tw, rmspb.NewRackManagerClient(cc), rmsv2pb.NewRackManagerV2Client(cc)
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func dev(tw *twin.Twin, path string) *model.Device {
	var d *model.Device
	tw.Engine.Read(func(m *model.Model) { d = m.DeviceBy[path] })
	return d
}

// nodeInfo is a node as NICo sends it before ingestion: its BMC MAC and address, with credentials.
func nodeInfo(tw *twin.Twin, path, id string) *rmspb.NodeInfo {
	d := dev(tw, path)
	var ip string
	tw.Engine.Read(func(*model.Model) { ip = d.Endpoint.SiteIP })
	return &rmspb.NodeInfo{NodeId: id, RackId: d.Rack.Name, BmcEndpoint: &rmspb.Endpoint{
		Interface: &rmspb.NetworkInterface{IpAddress: ip, MacAddress: d.Identity.BMCMAC}, Port: 443,
		Credentials: &rmspb.Credentials{Auth: &rmspb.Credentials_UserPass{UserPass: &rmspb.UsernamePassword{Username: d.Identity.BMCUser, Password: d.Identity.BMCPassword}}}}}
}

func wait(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func jobDone(t *testing.T, c rmspb.RackManagerClient, id string) *rmspb.JobStatus {
	t.Helper()
	var last *rmspb.JobStatus
	wait(t, "job "+id, func() bool {
		r, err := c.GetJobStatus(ctx(t), &rmspb.GetJobStatusRequest{JobId: id, IncludeChildJobStates: true})
		if err != nil {
			t.Fatal(err)
		}
		last = r.JobStates[0]
		return last.ExecutionState != rmspb.JobExecutionState_JOB_EXECUTION_STATE_RUNNING
	})
	return last
}

const site = "site-1/hall-a/"

func TestRMSOnTheLab(t *testing.T) {
	tw, c, c2 := up(t)
	if r := tw.Adapter(Name).Report(); r.Served != 1 {
		t.Fatalf("report: %+v", r)
	}

	t.Run("version and racks", func(t *testing.T) {
		v, err := c.GetVersion(ctx(t), &rmspb.GetVersionRequest{})
		if err != nil || v.Version == "" {
			t.Fatalf("version: %v %v", v, err)
		}
		r, err := c.ListRacks(ctx(t), &rmspb.ListRacksRequest{})
		if err != nil || strings.Join(r.RackIds, ",") != "a01,a02" {
			t.Fatalf("racks: %v %v", r, err)
		}
		if _, err := c.CreateNodes(ctx(t), &rmspb.CreateNodesRequest{}); status.Code(err) != codes.Unimplemented {
			t.Fatalf("an unserved RPC: %v", err)
		}
	})

	t.Run("device info", func(t *testing.T) {
		unknown := &rmspb.NodeInfo{NodeId: "ghost", BmcEndpoint: &rmspb.Endpoint{Interface: &rmspb.NetworkInterface{MacAddress: "02:00:00:00:00:01"}}}
		r, err := c.BatchGetNodeDeviceInfo(ctx(t), &rmspb.BatchGetNodeDeviceInfoRequest{Nodes: &rmspb.NodeSet{Nodes: []*rmspb.NodeInfo{
			nodeInfo(tw, site+"a01/compute-tray-07", "m-7"), nodeInfo(tw, site+"a01/switch-tray-03", "s-3"), unknown}}})
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != rmspb.ReturnCode_RETURN_CODE_FAILURE || r.Stats.FailedNodes != 1 || len(r.NodeDeviceDetails) != 2 || !strings.Contains(r.Message, "ghost") {
			t.Fatalf("batch: %v", r)
		}
		d := dev(tw, site+"a01/compute-tray-07")
		got := r.NodeDeviceDetails[0]
		if got.NodeId != "m-7" || got.GetTrayIndex() != 6 || got.GetSlotNumber() != uint32(d.U) || got.ChassisSn == nil {
			t.Fatalf("compute tray 7: %v (U %d)", got, d.U)
		}
		if r.NodeDeviceDetails[1].GetChassisSn() != got.GetChassisSn() {
			t.Fatal("two trays of one rack report different chassis serials")
		}
	})

	t.Run("power through the engine", func(t *testing.T) {
		tray := site + "a01/compute-tray-05"
		n := nodeInfo(tw, tray, "m-5")
		set := func(op rmspb.PowerOperation) *rmspb.NodeBatchResponse {
			r, err := c.BatchSetPowerState(ctx(t), &rmspb.BatchSetPowerStateRequest{Nodes: &rmspb.NodeSet{Nodes: []*rmspb.NodeInfo{n}}, Operation: op})
			if err != nil {
				t.Fatal(err)
			}
			return r.Response
		}
		pstate := func() string {
			r, err := c.BatchGetPowerState(ctx(t), &rmspb.BatchGetPowerStateRequest{Nodes: &rmspb.NodeSet{Nodes: []*rmspb.NodeInfo{n}}})
			if err != nil || len(r.NodePowerStates) != 1 {
				t.Fatalf("power state: %v %v", r, err)
			}
			return r.NodePowerStates[0].Pstate
		}
		if pstate() != "ON" {
			t.Fatal("tray not on at start")
		}
		if r := set(rmspb.PowerOperation_POWER_OPERATION_OFF); r.Status != rmspb.ReturnCode_RETURN_CODE_SUCCESS {
			t.Fatalf("off: %v", r)
		}
		wait(t, "the tray off", func() bool { return pstate() == "OFF" })
		if r := set(rmspb.PowerOperation_POWER_OPERATION_ON); r.Status != rmspb.ReturnCode_RETURN_CODE_SUCCESS {
			t.Fatalf("on: %v", r)
		}
		wait(t, "the tray on", func() bool { return pstate() == "ON" })
		seen := map[string]bool{}
		for _, ev := range tw.Engine.Events(0, nil) {
			if ev.Path == tray && ev.Request != nil && ev.Request.Protocol == Protocol && ev.Source.Class == "stack" {
				seen[ev.Request.Canonical] = true
			}
		}
		if !seen["power.off.graceful"] || !seen["power.on"] {
			t.Fatalf("power requests logged from rms: %v", seen)
		}
		bad := nodeInfo(tw, tray, "m-5")
		bad.BmcEndpoint.Credentials.GetUserPass().Password = "wrong"
		r, _ := c.BatchSetPowerState(ctx(t), &rmspb.BatchSetPowerStateRequest{Nodes: &rmspb.NodeSet{Nodes: []*rmspb.NodeInfo{bad}}, Operation: rmspb.PowerOperation_POWER_OPERATION_OFF})
		if r.Response.Status != rmspb.ReturnCode_RETURN_CODE_FAILURE || !strings.Contains(r.Response.NodeResults[0].ErrorMessage, "credentials") {
			t.Fatalf("wrong credentials: %v", r.Response)
		}
		if _, err := c.BatchSetPowerState(ctx(t), &rmspb.BatchSetPowerStateRequest{Nodes: &rmspb.NodeSet{Nodes: []*rmspb.NodeInfo{n}}}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("an unspecified operation: %v", err)
		}
	})

	var switches []*rmspb.NodeInfo
	for _, s := range []string{"switch-tray-01", "switch-tray-02", "switch-tray-03"} {
		switches = append(switches, nodeInfo(tw, site+"a01/"+s, "sw-"+s[len(s)-2:]))
	}

	t.Run("scale-up fabric", func(t *testing.T) {
		id := "sw-02"
		r, err := c2.ConfigureScaleUpFabricManager(ctx(t), &rmsv2pb.ConfigureScaleUpFabricManagerRequest{Nodes: &rmspb.NodeSet{Nodes: switches}, PrimarySwitchNodeId: &id,
			Config: &rmsv2pb.ScaleUpFabricConfig{TopologyType: "gb300_nvl72r1_c2g4_topology", ExtraStaticConfigs: []*rmspb.ScaleUpFabricStaticConfig{{ConfigFileName: "fm.cfg", Key: "RESILIENCY", Value: "1"}}}})
		if err != nil {
			t.Fatal(err)
		}
		js := jobDone(t, c, r.JobId)
		if js.ExecutionState != rmspb.JobExecutionState_JOB_EXECUTION_STATE_COMPLETED {
			t.Fatalf("job: %v", js)
		}
		var fs struct {
			Topology string `json:"topology_type"`
			Switches []struct {
				Node    string `json:"node_id"`
				Enabled bool   `json:"enabled"`
			} `json:"switches"`
		}
		if err := json.Unmarshal([]byte(js.ResultJson), &fs); err != nil || fs.Topology != "gb300_nvl72r1_c2g4_topology" || len(fs.Switches) != 3 {
			t.Fatalf("result: %s %v", js.ResultJson, err)
		}
		st, err := c.GetScaleUpFabricStatus(ctx(t), &rmspb.GetScaleUpFabricStatusRequest{Nodes: &rmspb.NodeSet{Nodes: switches}})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range st.FabricStatus.Switches {
			if s.Enabled != (s.NodeId == "sw-02") || s.FabricManagerStatus != "ok" {
				t.Fatalf("switch %v", s)
			}
		}
		svc, err := c.BatchGetScaleUpFabricServiceStatus(ctx(t), &rmspb.BatchGetScaleUpFabricServiceStatusRequest{Nodes: &rmspb.NodeSet{Nodes: switches}})
		if err != nil || !strings.Contains(svc.ServiceStatuses["sw-02"].StatusJson, "CONTROL_PLANE_STATE_CONFIGURED") || strings.Contains(svc.ServiceStatuses["sw-01"].StatusJson, "addition-info") {
			t.Fatalf("service status: %v %v", svc, err)
		}
		if _, err := c2.ConfigureScaleUpFabricManager(ctx(t), &rmsv2pb.ConfigureScaleUpFabricManagerRequest{Nodes: &rmspb.NodeSet{Nodes: switches}, Config: &rmsv2pb.ScaleUpFabricConfig{}}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("an empty topology: %v", err)
		}
	})

	t.Run("firmware objects", func(t *testing.T) {
		l, err := c.ListFirmwareObjects(ctx(t), &rmspb.ListFirmwareObjectsRequest{})
		if err != nil || len(l.Objects) != 2 {
			t.Fatalf("catalogue: %v %v", l, err)
		}
		tray := site + "a01/compute-tray-02"
		r, err := c.ApplyFirmwareObject(ctx(t), &rmspb.ApplyFirmwareObjectRequest{RackId: "a01", ConfigJson: `{"Id":"fw-2026.10","Version":"25.10.40"}`, FirmwareType: "prod",
			Nodes:            &rmspb.NodeSet{Nodes: []*rmspb.NodeInfo{nodeInfo(tw, tray, "m-2")}},
			ComponentFilters: map[int32]*rmspb.FirmwareObjectComponentFilter{int32(rmspb.NodeType_COMPUTE_GB300_NVIDIA): {Components: []string{"BMC", "BIOS"}}}})
		if err != nil || r.Response.Status != rmspb.ReturnCode_RETURN_CODE_SUCCESS || len(r.Jobs) != 1 || r.ObjectId != "fw-2026.10" {
			t.Fatalf("apply: %v %v", r, err)
		}
		wait(t, "the firmware job", func() bool {
			s, err := c.GetFirmwareJobStatus(ctx(t), &rmspb.GetFirmwareJobStatusRequest{JobId: r.Jobs[0].JobId})
			if err != nil || s.Status != rmspb.ReturnCode_RETURN_CODE_SUCCESS {
				t.Fatalf("status: %v %v", s, err)
			}
			return s.JobState == rmspb.FirmwareJobState_FIRMWARE_JOB_STATE_COMPLETED
		})
		d := dev(tw, tray)
		tw.Engine.Read(func(*model.Model) {
			if d.State.Firmware["bmc"].Staged != "25.10.40" || d.State.Firmware["uefi"].Staged != "25.10.40" || d.State.Firmware["gpu"].Staged != "" {
				t.Fatalf("staged: bmc %q uefi %q gpu %q", d.State.Firmware["bmc"].Staged, d.State.Firmware["uefi"].Staged, d.State.Firmware["gpu"].Staged)
			}
		})
		if s, _ := c.GetFirmwareJobStatus(ctx(t), &rmspb.GetFirmwareJobStatusRequest{JobId: "nope"}); s.Status != rmspb.ReturnCode_RETURN_CODE_FAILURE {
			t.Fatalf("an unknown job: %v", s)
		}
		img, err := c.ApplySwitchSystemImage(ctx(t), &rmspb.ApplySwitchSystemImageRequest{RackId: "a01", ConfigJson: `{"Id":"nvos-25.02.3000","Version":"25.02.3000"}`,
			Nodes: &rmspb.NodeSet{Nodes: switches[:1]}})
		if err != nil || len(img.Jobs) != 1 {
			t.Fatalf("image: %v %v", img, err)
		}
		wait(t, "the image job", func() bool {
			s, _ := c.GetSwitchSystemImageJobStatus(ctx(t), &rmspb.GetSwitchSystemImageJobStatusRequest{JobId: img.Response.JobId})
			return s.State == "completed"
		})
		sw := dev(tw, site+"a01/switch-tray-01")
		tw.Engine.Read(func(*model.Model) {
			if sw.State.Firmware["nvos"].Staged != "25.02.3000" {
				t.Fatalf("nvos staged %q", sw.State.Firmware["nvos"].Staged)
			}
		})
	})

	t.Run("switch lifecycle", func(t *testing.T) {
		cert, err := c.ConfigureSwitchCertificate(ctx(t), &rmspb.ConfigureSwitchCertificateRequest{Nodes: &rmspb.NodeSet{Nodes: switches},
			Services: []rmspb.SwitchService{rmspb.SwitchService_SWITCH_SERVICE_NVUE_API, rmspb.SwitchService_SWITCH_SERVICE_SCALE_UP_FABRIC_MANAGER}})
		if err != nil || len(cert.Jobs) != 3 {
			t.Fatalf("certificates: %v %v", cert, err)
		}
		wait(t, "the certificate job", func() bool {
			s, _ := c.GetConfigureSwitchCertificateJobStatus(ctx(t), &rmspb.GetConfigureSwitchCertificateJobStatusRequest{JobId: cert.Response.JobId})
			return s.State == "completed"
		})
		sw := dev(tw, site+"a01/switch-tray-03")
		pw, err := c.UpdateSwitchSystemPassword(ctx(t), &rmspb.UpdateSwitchSystemPasswordRequest{Nodes: &rmspb.NodeSet{Nodes: switches[2:]}, Username: sw.Identity.BMCUser, Password: "Rotated-by-RMS-1"})
		if err != nil {
			t.Fatal(err)
		}
		if js := jobDone(t, c, pw.Response.JobId); js.ExecutionState != rmspb.JobExecutionState_JOB_EXECUTION_STATE_COMPLETED {
			t.Fatalf("password job: %v", js)
		}
		if _, ok := tw.Engine.Authenticate(sw, sw.Identity.BMCUser, "Rotated-by-RMS-1"); !ok {
			t.Fatal("the switch's password did not change")
		}
		reset, err := c.BatchResetSwitchFactoryDefault(ctx(t), &rmspb.BatchResetSwitchFactoryDefaultRequest{Nodes: &rmspb.NodeSet{Nodes: switches[1:2]}})
		if err != nil {
			t.Fatal(err)
		}
		if js := jobDone(t, c, reset.Response.JobId); js.ExecutionState != rmspb.JobExecutionState_JOB_EXECUTION_STATE_COMPLETED {
			t.Fatalf("reset job: %v", js)
		}
		// the rack re-elects: the switch at the lowest position (EIA U) of those named
		lowest, lowU := "", 1<<30
		for _, s := range []string{"01", "02", "03"} {
			if u := dev(tw, site+"a01/switch-tray-"+s).U; u < lowU {
				lowest, lowU = "sw-"+s, u
			}
		}
		st, _ := c.GetScaleUpFabricStatus(ctx(t), &rmspb.GetScaleUpFabricStatusRequest{Nodes: &rmspb.NodeSet{Nodes: switches}})
		for _, s := range st.FabricStatus.Switches {
			if s.Enabled != (s.NodeId == lowest) {
				t.Fatalf("after the primary's factory reset %s (lowest) is elected: %v", lowest, st.FabricStatus.Switches)
			}
		}
	})

	t.Run("cold reboot", func(t *testing.T) {
		if testing.Short() {
			t.Skip("skipped in -short: the reboot sequence takes about ten seconds at the lab's time scale")
		}
		var nodes []*rmspb.NodeInfo
		for _, p := range []string{"power-shelf-01", "power-shelf-02", "power-shelf-03", "power-shelf-04", "power-shelf-05", "power-shelf-06", "power-shelf-07", "power-shelf-08",
			"compute-tray-01", "compute-tray-02", "switch-tray-01", "switch-tray-02"} {
			nodes = append(nodes, nodeInfo(tw, site+"a02/"+p, p))
		}
		short := uint32(10)
		r, err := c.ExecuteColdReboot(ctx(t), &rmspb.ExecuteColdRebootRequest{Nodes: &rmspb.NodeSet{Nodes: nodes},
			DischargeFloorSeconds: &short, BmcUpFloorSeconds: &short, ComputeBootWaitSeconds: &short})
		if err != nil {
			t.Fatal(err)
		}
		js := jobDone(t, c, r.Response.JobId)
		if js.ExecutionState != rmspb.JobExecutionState_JOB_EXECUTION_STATE_COMPLETED {
			t.Fatalf("cold reboot: %v", js)
		}
		var timeline struct {
			Stages []struct {
				Status string `json:"status"`
			} `json:"stages"`
		}
		if err := json.Unmarshal([]byte(js.ResultJson), &timeline); err != nil || len(timeline.Stages) != 5 {
			t.Fatalf("timeline: %s", js.ResultJson)
		}
		lostAC := 0
		for _, ev := range tw.Engine.Events(0, nil) {
			if strings.HasPrefix(ev.Path, site+"a02/") && ev.State != nil && ev.State.Field == "ac" && ev.State.To == false {
				lostAC++
			}
		}
		if lostAC == 0 {
			t.Fatal("switching the shelves off took no tray's power")
		}
		if _, err := c.ExecuteColdReboot(ctx(t), &rmspb.ExecuteColdRebootRequest{Nodes: &rmspb.NodeSet{Nodes: nodes[8:]}}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("a reboot without power shelves: %v", err)
		}
	})
}

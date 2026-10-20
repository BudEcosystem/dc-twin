package rms

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/BudEcosystem/dc-twin/internal/adapter/nvl"
	"github.com/BudEcosystem/dc-twin/internal/adapter/rms/rmspb"
	"github.com/BudEcosystem/dc-twin/internal/adapter/rms/rmspb/rmsv2pb"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Behaviour durations of the jobs the twin runs itself, before spec.timeScale.
const (
	certificateSeconds  = 20
	fabricSeconds       = 45
	passwordSeconds     = 10
	factoryResetSeconds = 120
)

// canonical is the canonical name (14) each served RPC is logged under. RPCs whose effects the
// engine logs per device (power, firmware) are not logged again here.
var canonical = map[string]string{
	"GetVersion": "inventory.read", "ListRacks": "inventory.read", "GetNodeDeviceInfo": "inventory.read",
	"ListNodeDeviceInfoByNodeType": "inventory.read", "BatchGetNodeDeviceInfo": "inventory.read",
	"GetPowerState": "health.read", "BatchGetPowerState": "health.read",
	"ListFirmwareObjects": "firmware.inventory.read", "GetFirmwareObject": "firmware.inventory.read",
	"GetFirmwareJobStatus": "task.read", "GetSwitchSystemImageJobStatus": "task.read", "GetJobStatus": "task.read",
	"GetConfigureSwitchCertificateJobStatus": "task.read",
	"GetScaleUpFabricStatus":                 "network.read", "BatchGetScaleUpFabricServiceStatus": "network.read",
	"ConfigureSwitchCertificate": "other", "ConfigureScaleUpFabricManager": "other", "UpdateSwitchSystemPassword": "account.password",
	"BatchResetSwitchFactoryDefault": "other", "ExecuteColdReboot": "other",
}

func served() []string {
	out := []string{"SetPowerState", "BatchSetPowerState", "ApplyFirmwareObject", "ApplySwitchSystemImage", "V2 ConfigureScaleUpFabricManager"}
	for k := range canonical {
		if k != "ConfigureScaleUpFabricManager" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// intercept settles jobs before every RPC and logs the RPC by its canonical name.
func (a *Adapter) intercept(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	a.jobs.settle()
	resp, err := h(ctx, req)
	method := info.FullMethod[strings.LastIndex(info.FullMethod, "/")+1:]
	c, ok := canonical[method]
	if strings.HasPrefix(info.FullMethod, "/rack_manager.RackManager/ConfigureScaleUpFabricManager") {
		ok = false // V1's spelling is not served
	}
	if !ok && status.Code(err) != codes.Unimplemented {
		return resp, err
	}
	if !ok {
		c = "other"
	}
	st := 200
	if err != nil {
		st = httpStatus(status.Code(err))
	}
	a.rt.Engine.Commit(schema.Event{Kind: "request", Path: a.site, Source: schema.EventSource{Class: "stack", Principal: "rms-client", Address: peerAddr(ctx)},
		Request: &schema.EventRequest{Protocol: Protocol, Method: method, Target: info.FullMethod, Canonical: c, Status: st}})
	return resp, err
}

func httpStatus(c codes.Code) int {
	switch c {
	case codes.OK:
		return 200
	case codes.InvalidArgument:
		return 400
	case codes.NotFound:
		return 404
	case codes.Unimplemented:
		return 501
	case codes.Unavailable:
		return 503
	}
	return 500
}

func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}

// ---- shared shapes

// target is one requested node and what it matched.
type target struct {
	req *rmspb.NodeInfo
	n   *node
	err error
}

func (t target) id() string { return t.req.GetNodeId() }

func (a *Adapter) resolve(set *rmspb.NodeSet) []target {
	var out []target
	for _, r := range set.GetNodes() {
		n, err := a.match(r)
		out = append(out, target{req: r, n: n, err: err})
	}
	return out
}

// batch is a NodeBatchResponse from per-node outcomes: it succeeds only when every node did.
func batch(ts []target, errs map[int]error, jobID string) *rmspb.NodeBatchResponse {
	r := &rmspb.NodeBatchResponse{JobId: jobID, Stats: &rmspb.NodeOperationStats{TotalNodes: uint32(len(ts))}}
	var bad []string
	for i, t := range ts {
		err := t.err
		if err == nil {
			err = errs[i]
		}
		nr := &rmspb.NodeOperationResult{NodeId: t.id(), Status: rmspb.ReturnCode_RETURN_CODE_SUCCESS}
		if err != nil {
			nr.Status, nr.ErrorMessage = rmspb.ReturnCode_RETURN_CODE_FAILURE, err.Error()
			bad = append(bad, fmt.Sprintf("%s: %v", t.id(), err))
		}
		r.NodeResults = append(r.NodeResults, nr)
	}
	r.Stats.FailedNodes = uint32(len(bad))
	r.Stats.SuccessfulNodes = r.Stats.TotalNodes - r.Stats.FailedNodes
	r.Status = rmspb.ReturnCode_RETURN_CODE_SUCCESS
	if len(bad) > 0 {
		r.Status = rmspb.ReturnCode_RETURN_CODE_FAILURE
		r.Message = fmt.Sprintf("%d of %d nodes failed: %s", len(bad), len(ts), strings.Join(bad, "; "))
	}
	return r
}

func outcome(ts []target, errs map[int]error) (rmspb.ReturnCode, string, *rmspb.NodeOperationStats) {
	b := batch(ts, errs, "")
	return b.Status, b.Message, b.Stats
}

// principal is the user a node's credentials name, else the RMS client.
func principal(ep *rmspb.Endpoint) string {
	if up, ok := ep.GetCredentials().GetAuth().(*rmspb.Credentials_UserPass); ok && up.UserPass.GetUsername() != "" {
		return up.UserPass.GetUsername()
	}
	return "rms-client"
}

// checkCredentials checks credentials a request carries against the device's accounts (a
// switch's NVOS users are the tray's accounts in the twin); none given is RMS's stored ones.
func (a *Adapter) checkCredentials(n *node, ep *rmspb.Endpoint) error {
	up, ok := ep.GetCredentials().GetAuth().(*rmspb.Credentials_UserPass)
	if !ok || up.UserPass == nil {
		return nil
	}
	if _, ok := a.rt.Engine.Authenticate(n.dev, up.UserPass.GetUsername(), up.UserPass.GetPassword()); !ok {
		return fmt.Errorf("the device rejected the credentials of %s", up.UserPass.GetUsername())
	}
	return nil
}

func (a *Adapter) bmcUp(n *node) error {
	if up, _, _ := a.rt.Engine.BMCBehaviour(n.dev); !up {
		return fmt.Errorf("the BMC of %s is not reachable", n.dev.Path)
	}
	return nil
}

// nvosUp reports whether a switch tray's NVOS answers (the tray is powered).
func (a *Adapter) nvosUp(n *node) error {
	on := false
	a.rt.Engine.Read(func(*model.Model) { on = n.dev.State.AC && n.dev.State.Power == model.On })
	if !on {
		return fmt.Errorf("NVOS on %s is not reachable (the tray is not powered)", n.dev.Path)
	}
	return nil
}

func (a *Adapter) request(ctx context.Context, n *node, canonical, method, who string) engine.Request {
	return engine.Request{Path: n.dev.Path, Canonical: canonical, Protocol: Protocol, Method: method, Target: "/rack_manager.RackManager/" + method,
		Source: schema.EventSource{Class: "stack", Principal: who, Address: peerAddr(ctx)}}
}

func (a *Adapter) scaled(seconds float64) time.Duration {
	return time.Duration(seconds * a.rt.Engine.Scale() * float64(time.Second))
}

// ---- utility and inventory

func (a *Adapter) GetVersion(context.Context, *rmspb.GetVersionRequest) (*rmspb.GetVersionResponse, error) {
	return &rmspb.GetVersionResponse{Version: a.version}, nil
}

func (a *Adapter) ListRacks(context.Context, *rmspb.ListRacksRequest) (*rmspb.ListRacksResponse, error) {
	return &rmspb.ListRacksResponse{RackIds: a.Service().Racks}, nil
}

// deviceInfo is a node's placement: the rack chassis serial, its rack unit and tray index.
func (a *Adapter) deviceInfo(id string, n *node) *rmspb.NodeDeviceInfo {
	var seed int64
	a.rt.Engine.Read(func(m *model.Model) { seed = m.Seed })
	info := &rmspb.NodeDeviceInfo{NodeId: id}
	if sn, err := strconv.ParseInt(nvl.ChassisSerial(seed, n.rack), 10, 64); err == nil {
		info.ChassisSn = &sn
	}
	slot, tray := nvl.Slot(n.dev), nvl.TrayIndex(n.dev)
	info.SlotNumber, info.TrayIndex = &slot, &tray
	return info
}

func (a *Adapter) GetNodeDeviceInfo(_ context.Context, r *rmspb.GetNodeDeviceInfoRequest) (*rmspb.GetNodeDeviceInfoResponse, error) {
	n := a.byID(r.GetNodeId())
	if n == nil {
		return &rmspb.GetNodeDeviceInfoResponse{Status: rmspb.ReturnCode_RETURN_CODE_FAILURE, Message: fmt.Sprintf("node %q is not in the inventory", r.GetNodeId())}, nil
	}
	return &rmspb.GetNodeDeviceInfoResponse{Status: rmspb.ReturnCode_RETURN_CODE_SUCCESS, DeviceInfo: a.deviceInfo(r.GetNodeId(), n)}, nil
}

func (a *Adapter) ListNodeDeviceInfoByNodeType(_ context.Context, r *rmspb.ListNodeDeviceInfoByNodeTypeRequest) (*rmspb.ListNodeDeviceInfoByNodeTypeResponse, error) {
	resp := &rmspb.ListNodeDeviceInfoByNodeTypeResponse{Status: rmspb.ReturnCode_RETURN_CODE_SUCCESS, Stats: &rmspb.NodeOperationStats{}}
	for _, n := range a.nodes {
		if a.rackIDs[n.rack] != r.GetRackId() || n.typ != r.GetNodeType() {
			continue
		}
		resp.NodeDeviceDetails = append(resp.NodeDeviceDetails, a.deviceInfo(strings.TrimPrefix(n.dev.Path, a.site+"/"), n))
	}
	resp.Stats.TotalNodes = uint32(len(resp.NodeDeviceDetails))
	resp.Stats.SuccessfulNodes = resp.Stats.TotalNodes
	return resp, nil
}

func (a *Adapter) BatchGetNodeDeviceInfo(_ context.Context, r *rmspb.BatchGetNodeDeviceInfoRequest) (*rmspb.BatchGetNodeDeviceInfoResponse, error) {
	ts := a.resolve(r.GetNodes())
	resp := &rmspb.BatchGetNodeDeviceInfoResponse{}
	for _, t := range ts {
		if t.n != nil {
			resp.NodeDeviceDetails = append(resp.NodeDeviceDetails, a.deviceInfo(t.id(), t.n))
		}
	}
	resp.Status, resp.Message, resp.Stats = outcome(ts, nil)
	return resp, nil
}

// ---- power

func powerCanonical(op rmspb.PowerOperation) (string, map[string]any, error) {
	switch op {
	case rmspb.PowerOperation_POWER_OPERATION_ON, rmspb.PowerOperation_POWER_OPERATION_FORCE_ON:
		return "power.on", nil, nil
	case rmspb.PowerOperation_POWER_OPERATION_OFF, rmspb.PowerOperation_POWER_OPERATION_GRACEFUL_SHUTDOWN:
		return "power.off.graceful", nil, nil
	case rmspb.PowerOperation_POWER_OPERATION_FORCE_OFF:
		return "power.off.force", nil, nil
	case rmspb.PowerOperation_POWER_OPERATION_RESET:
		return "power.cycle", nil, nil
	case rmspb.PowerOperation_POWER_OPERATION_GRACEFUL_RESTART:
		return "power.restart", map[string]any{"graceful": true}, nil
	case rmspb.PowerOperation_POWER_OPERATION_FORCE_RESTART:
		return "power.restart", nil, nil
	}
	return "", nil, status.Errorf(codes.InvalidArgument, "power operation %s is not one RMS performs", op)
}

func (a *Adapter) setPower(ctx context.Context, n *node, ep *rmspb.Endpoint, op rmspb.PowerOperation, method string) error {
	canonical, params, err := powerCanonical(op)
	if err != nil {
		return err
	}
	if err := a.bmcUp(n); err != nil {
		return err
	}
	if err := a.checkCredentials(n, ep); err != nil {
		return err
	}
	r := a.request(ctx, n, canonical, method, principal(ep))
	r.Params = params
	return a.rt.Engine.Power(r)
}

func (a *Adapter) SetPowerState(ctx context.Context, r *rmspb.SetPowerStateRequest) (*rmspb.SetPowerStateResponse, error) {
	if _, _, err := powerCanonical(r.GetOperation()); err != nil {
		return nil, err
	}
	n := a.byID(r.GetNodeId())
	if n == nil {
		return &rmspb.SetPowerStateResponse{Status: rmspb.ReturnCode_RETURN_CODE_FAILURE}, nil
	}
	if err := a.setPower(ctx, n, nil, r.GetOperation(), "SetPowerState"); err != nil {
		return &rmspb.SetPowerStateResponse{Status: rmspb.ReturnCode_RETURN_CODE_FAILURE}, nil
	}
	return &rmspb.SetPowerStateResponse{Status: rmspb.ReturnCode_RETURN_CODE_SUCCESS}, nil
}

func (a *Adapter) BatchSetPowerState(ctx context.Context, r *rmspb.BatchSetPowerStateRequest) (*rmspb.BatchSetPowerStateResponse, error) {
	if _, _, err := powerCanonical(r.GetOperation()); err != nil {
		return nil, err
	}
	ts := a.resolve(r.GetNodes())
	errs := map[int]error{}
	for i, t := range ts {
		if t.n != nil {
			errs[i] = a.setPower(ctx, t.n, t.req.GetBmcEndpoint(), r.GetOperation(), "BatchSetPowerState")
		}
	}
	return &rmspb.BatchSetPowerStateResponse{Response: batch(ts, errs, "")}, nil
}

// pstate is a node's power as its BMC reports it: ON only when on; a node mid-transition reads
// OFF.
func (a *Adapter) pstate(n *node) string {
	p := ""
	a.rt.Engine.Read(func(*model.Model) { p = n.dev.State.Power })
	if p == model.On {
		return "ON"
	}
	return "OFF"
}

func (a *Adapter) GetPowerState(_ context.Context, r *rmspb.GetPowerStateRequest) (*rmspb.GetPowerStateResponse, error) {
	resp := &rmspb.GetPowerStateResponse{NodeId: r.GetNodeId(), RackId: r.GetRackId(), Pstate: "UNKNOWN", Status: rmspb.ReturnCode_RETURN_CODE_FAILURE}
	if n := a.byID(r.GetNodeId()); n != nil && a.bmcUp(n) == nil {
		resp.Pstate, resp.Status = a.pstate(n), rmspb.ReturnCode_RETURN_CODE_SUCCESS
	}
	return resp, nil
}

func (a *Adapter) BatchGetPowerState(_ context.Context, r *rmspb.BatchGetPowerStateRequest) (*rmspb.BatchGetPowerStateResponse, error) {
	ts := a.resolve(r.GetNodes())
	errs := map[int]error{}
	resp := &rmspb.BatchGetPowerStateResponse{}
	for i, t := range ts {
		if t.n == nil {
			continue
		}
		if err := a.bmcUp(t.n); err != nil {
			errs[i] = err
			continue
		}
		if err := a.checkCredentials(t.n, t.req.GetBmcEndpoint()); err != nil {
			errs[i] = err
			continue
		}
		resp.NodePowerStates = append(resp.NodePowerStates, &rmspb.NodePowerState{NodeId: t.id(), Pstate: a.pstate(t.n)})
	}
	resp.Response = batch(ts, errs, "")
	return resp, nil
}

// ---- jobs: status

func (a *Adapter) GetJobStatus(_ context.Context, r *rmspb.GetJobStatusRequest) (*rmspb.GetJobStatusResponse, error) {
	if r.GetJobId() == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}
	j := a.jobs.get(r.GetJobId())
	if j == nil {
		return nil, status.Errorf(codes.NotFound, "job %s not found", r.GetJobId())
	}
	resp := &rmspb.GetJobStatusResponse{JobStates: []*rmspb.JobStatus{a.jobs.status(j)}}
	if r.GetIncludeChildJobStates() {
		for _, c := range j.children {
			resp.JobStates = append(resp.JobStates, a.jobs.status(c))
		}
	}
	return resp, nil
}

// lookup finds a job a kind-specific status RPC names.
func (a *Adapter) lookup(id string, kinds ...string) (*job, *rmspb.JobStatus, error) {
	if id == "" {
		return nil, nil, status.Error(codes.InvalidArgument, "job_id is required")
	}
	j := a.jobs.get(id)
	if j == nil {
		return nil, nil, nil
	}
	ok := false
	for _, k := range kinds {
		ok = ok || j.kind == k
	}
	if !ok {
		return nil, nil, nil
	}
	return j, a.jobs.status(j), nil
}

func (a *Adapter) GetFirmwareJobStatus(_ context.Context, r *rmspb.GetFirmwareJobStatusRequest) (*rmspb.GetFirmwareJobStatusResponse, error) {
	j, js, err := a.lookup(r.GetJobId(), kindFirmware, kindImage)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return &rmspb.GetFirmwareJobStatusResponse{Status: rmspb.ReturnCode_RETURN_CODE_FAILURE, JobId: r.GetJobId(), ErrorMessage: fmt.Sprintf("job %s not found", r.GetJobId())}, nil
	}
	resp := &rmspb.GetFirmwareJobStatusResponse{Status: rmspb.ReturnCode_RETURN_CODE_SUCCESS, JobId: j.id, StateDescription: js.StateDescription,
		RackId: js.GetRackId(), NodeId: js.GetNodeId(), ErrorMessage: js.ErrorMessage, ResultJson: js.ResultJson, CreatedAt: js.CreatedAt, UpdatedAt: js.UpdatedAt}
	switch js.ExecutionState {
	case rmspb.JobExecutionState_JOB_EXECUTION_STATE_COMPLETED:
		resp.JobState, resp.ErrorCode = rmspb.FirmwareJobState_FIRMWARE_JOB_STATE_COMPLETED, rmspb.FirmwareUpdateError_FIRMWARE_UPDATE_ERROR_SUCCESS
	case rmspb.JobExecutionState_JOB_EXECUTION_STATE_FAILED:
		resp.JobState, resp.ErrorCode = rmspb.FirmwareJobState_FIRMWARE_JOB_STATE_FAILED, rmspb.FirmwareUpdateError_FIRMWARE_UPDATE_ERROR_TASK_FAILED
	default:
		resp.JobState = rmspb.FirmwareJobState_FIRMWARE_JOB_STATE_RUNNING
	}
	return resp, nil
}

func wireState(js *rmspb.JobStatus) string {
	switch js.ExecutionState {
	case rmspb.JobExecutionState_JOB_EXECUTION_STATE_COMPLETED:
		return "completed"
	case rmspb.JobExecutionState_JOB_EXECUTION_STATE_FAILED:
		return "failed"
	case rmspb.JobExecutionState_JOB_EXECUTION_STATE_QUEUED:
		return "queued"
	}
	return "running"
}

func (a *Adapter) GetSwitchSystemImageJobStatus(_ context.Context, r *rmspb.GetSwitchSystemImageJobStatusRequest) (*rmspb.GetSwitchSystemImageJobStatusResponse, error) {
	j, js, err := a.lookup(r.GetJobId(), kindImage)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return &rmspb.GetSwitchSystemImageJobStatusResponse{Status: rmspb.ReturnCode_RETURN_CODE_FAILURE, JobId: r.GetJobId(), ErrorMessage: fmt.Sprintf("job %s not found", r.GetJobId())}, nil
	}
	return &rmspb.GetSwitchSystemImageJobStatusResponse{Status: rmspb.ReturnCode_RETURN_CODE_SUCCESS, JobId: j.id, State: wireState(js), Message: js.StateDescription,
		RackId: js.GetRackId(), NodeId: js.GetNodeId(), ErrorMessage: js.ErrorMessage, ResultJson: js.ResultJson, CreatedAt: js.CreatedAt, UpdatedAt: js.UpdatedAt}, nil
}

func (a *Adapter) GetConfigureSwitchCertificateJobStatus(_ context.Context, r *rmspb.GetConfigureSwitchCertificateJobStatusRequest) (*rmspb.GetConfigureSwitchCertificateJobStatusResponse, error) {
	j, js, err := a.lookup(r.GetJobId(), kindCertificate)
	if err != nil {
		return nil, err
	}
	if j == nil {
		return &rmspb.GetConfigureSwitchCertificateJobStatusResponse{Status: rmspb.ReturnCode_RETURN_CODE_FAILURE, JobId: r.GetJobId(), ErrorMessage: fmt.Sprintf("job %s not found", r.GetJobId())}, nil
	}
	return &rmspb.GetConfigureSwitchCertificateJobStatusResponse{Status: rmspb.ReturnCode_RETURN_CODE_SUCCESS, JobId: j.id, State: wireState(js), Message: js.StateDescription,
		RackId: js.GetRackId(), NodeId: js.GetNodeId(), ErrorMessage: js.ErrorMessage, ResultJson: js.ResultJson, CreatedAt: js.CreatedAt, UpdatedAt: js.UpdatedAt}, nil
}

// Job kinds.
const (
	kindFirmware    = "firmware update"
	kindImage       = "switch system image update"
	kindCertificate = "switch certificate configuration"
	kindFabric      = "scale-up fabric configuration"
	kindPassword    = "switch password update"
	kindReset       = "switch factory reset"
	kindReboot      = "rack cold reboot"
)

// ---- firmware objects

// sotDoc is the part of a SOT firmware-object document the twin reads: its Id, a Version for
// every component, or per-component versions (flat, or by SOT device type label).
type sotDoc struct {
	ID         string                     `json:"Id"`
	Version    string                     `json:"Version"`
	Image      string                     `json:"ImageFilename"`
	Components map[string]json.RawMessage `json:"Components"`
}

func parseSOT(js string) (*sotDoc, error) {
	if strings.TrimSpace(js) == "" {
		return nil, status.Error(codes.InvalidArgument, "config_json is required: the SOT JSON names the firmware object to apply")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(js), &raw); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "config_json must be a JSON object describing one firmware object: %v", err)
	}
	var d sotDoc
	_ = json.Unmarshal([]byte(js), &d)
	d.ID = strings.TrimSpace(d.ID)
	return &d, nil
}

// deviceLabel is the SOT device type label of a node kind.
func deviceLabel(kind string) string {
	return map[string]string{"compute": "Compute Node", "switch": "Switch Tray", "powershelf": "Power Shelf"}[kind]
}

// version is the version a document gives a node's component.
func (d *sotDoc) version(kind, component string) string {
	if raw, ok := d.Components[deviceLabel(kind)]; ok {
		var m map[string]string
		if json.Unmarshal(raw, &m) == nil {
			for k, v := range m {
				if strings.EqualFold(k, component) {
					return v
				}
			}
		}
	}
	for k, raw := range d.Components {
		var v string
		if strings.EqualFold(k, component) && json.Unmarshal(raw, &v) == nil {
			return v
		}
	}
	if d.Version != "" {
		return d.Version
	}
	return d.ID
}

// aliases maps the component names RMS callers send (NICo sends BMC, CPLD, BIOS for switches,
// PowerShelfFW for shelves) onto the profile's firmware components.
var aliases = map[string][]string{"bios": {"uefi", "bios"}, "sbios": {"uefi"}, "uefi": {"uefi"}, "hgx": {"gpu"}, "gpu": {"gpu"}, "fpga": {"cpld"}}

// components selects a node's firmware components by the caller's filter: all when none is
// given; a name the profile has no component for is skipped and reported.
func (a *Adapter) components(n *node, names []string) (sel, skipped []string) {
	have := map[string]bool{}
	a.rt.Engine.Read(func(*model.Model) {
		for c := range n.dev.State.Firmware {
			have[c] = true
		}
	})
	all := make([]string, 0, len(have))
	for c := range have {
		all = append(all, c)
	}
	sort.Strings(all)
	if len(names) == 0 {
		return all, nil
	}
	got := map[string]bool{}
	for _, name := range names {
		l := strings.ToLower(name)
		if l == "powershelffw" {
			for _, c := range all {
				got[c] = true
			}
			continue
		}
		cands := append([]string{l}, aliases[l]...)
		hit := false
		for _, c := range cands {
			if have[c] {
				got[c], hit = true, true
			}
		}
		if !hit {
			skipped = append(skipped, name)
		}
	}
	for _, c := range all {
		if got[c] {
			sel = append(sel, c)
		}
	}
	return sel, skipped
}

// catalogue is the firmware objects RMS lists: one per NVL rack format, at its profiles'
// baselines, plus ids a stack's output names.
func (a *Adapter) catalogue() []*rmspb.FirmwareObject {
	var out []*rmspb.FirmwareObject
	seen := map[string]bool{}
	for _, r := range a.racks {
		if seen[r.FormatName] {
			continue
		}
		seen[r.FormatName] = true
		id := "dctwin-" + strings.ReplaceAll(r.FormatName, "/", "-") + "-baseline"
		doc := map[string]any{"Id": id}
		comps := map[string]map[string]string{}
		meta := &rmspb.FirmwareObjectMetadata{}
		types := map[rmspb.NodeType]*rmspb.FirmwareObjectDeviceComponents{}
		var order []rmspb.NodeType
		for _, n := range a.nodes {
			if n.rack.FormatName != r.FormatName || n.dev.Profile == nil {
				continue
			}
			dc := types[n.typ]
			if dc == nil {
				dc = &rmspb.FirmwareObjectDeviceComponents{NodeType: n.typ, DeviceType: deviceLabel(n.kind)}
				types[n.typ] = dc
				order = append(order, n.typ)
				for _, fw := range n.dev.Profile.Spec.Firmware {
					dc.Components = append(dc.Components, &rmspb.FirmwareObjectComponent{Name: fw.Component, Version: fw.Baseline, SourceComponent: fw.Component,
						Artifacts: []*rmspb.FirmwareObjectArtifact{{FirmwareType: "prod", Filename: fw.Component + "-" + fw.Baseline + ".fwpkg", Target: fw.Component}}})
					if comps[deviceLabel(n.kind)] == nil {
						comps[deviceLabel(n.kind)] = map[string]string{}
					}
					comps[deviceLabel(n.kind)][fw.Component] = fw.Baseline
					if n.kind == "switch" && fw.Component == "nvos" {
						meta.SwitchSystemImages = append(meta.SwitchSystemImages, &rmspb.FirmwareObjectSwitchSystemImage{SoftwareType: "prod", Version: fw.Baseline,
							PackageName: "nvos", ImageFilename: "nvos-" + fw.Baseline + ".bin", Required: true})
					}
				}
			}
		}
		sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
		for _, t := range order {
			meta.DeviceComponents = append(meta.DeviceComponents, types[t])
		}
		doc["Components"] = comps
		b, _ := json.Marshal(doc)
		started := timestamppb.New(a.rt.Engine.Started())
		out = append(out, &rmspb.FirmwareObject{Id: id, ConfigJson: string(b), Available: true, Created: started, Updated: started, Metadata: meta,
			HardwareType: r.FormatName, IsDefault: true})
	}
	for _, id := range strings.Split(a.rt.Outputs["rms.firmwareObjects"], ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, &rmspb.FirmwareObject{Id: id, ConfigJson: fmt.Sprintf(`{"Id":%q}`, id), Available: true})
		}
	}
	return out
}

func (a *Adapter) ListFirmwareObjects(_ context.Context, r *rmspb.ListFirmwareObjectsRequest) (*rmspb.ListFirmwareObjectsResponse, error) {
	resp := &rmspb.ListFirmwareObjectsResponse{}
	for _, o := range a.catalogue() {
		if r.GetHardwareType() != "" && o.HardwareType != "" && o.HardwareType != r.GetHardwareType() {
			continue
		}
		if r.GetOnlyAvailable() && !o.Available {
			continue
		}
		resp.Objects = append(resp.Objects, o)
	}
	return resp, nil
}

func (a *Adapter) GetFirmwareObject(_ context.Context, r *rmspb.GetFirmwareObjectRequest) (*rmspb.GetFirmwareObjectResponse, error) {
	for _, o := range a.catalogue() {
		if o.Id == r.GetId() {
			return &rmspb.GetFirmwareObjectResponse{Object: o}, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "firmware object %q not found", r.GetId())
}

// update starts the engine's update tasks for a node's components at the versions a document
// gives them, grouped by version.
func (a *Adapter) update(ctx context.Context, n *node, doc *sotDoc, comps []string, method string) (*job, error) {
	if err := a.bmcUp(n); err != nil {
		return nil, err
	}
	byVersion := map[string][]string{}
	var versions []string
	for _, c := range comps {
		v := doc.version(n.kind, c)
		if byVersion[v] == nil {
			versions = append(versions, v)
		}
		byVersion[v] = append(byVersion[v], c)
	}
	j := &job{dev: n.dev}
	for _, v := range versions {
		t, err := a.rt.Engine.UpdateFirmware(a.request(ctx, n, "firmware.update.simple", method, "rms-client"), byVersion[v], v)
		if err != nil {
			return nil, err
		}
		j.tasks = append(j.tasks, taskRef{dev: n.dev, id: t.ID})
	}
	staged := map[string]string{}
	for v, cs := range byVersion {
		for _, c := range cs {
			staged[c] = v
		}
	}
	j.done = func(j *job) error {
		b, _ := json.Marshal(map[string]any{"node": n.dev.Path, "staged": staged, "note": "staged; applied as each component's applyOn says (reset or AC cycle)"})
		j.result = string(b)
		return nil
	}
	return j, nil
}

func (a *Adapter) ApplyFirmwareObject(ctx context.Context, r *rmspb.ApplyFirmwareObjectRequest) (*rmspb.ApplyFirmwareObjectResponse, error) {
	doc, err := parseSOT(r.GetConfigJson())
	if err != nil {
		return nil, err
	}
	objectID := doc.ID
	if objectID == "" {
		if cat := a.catalogue(); len(cat) > 0 {
			objectID = cat[0].Id
		}
	}
	ts := a.resolve(r.GetNodes())
	errs := map[int]error{}
	parent := &job{kind: kindFirmware, rackID: r.GetRackId()}
	resp := &rmspb.ApplyFirmwareObjectResponse{ObjectId: objectID}
	var kids []*job
	var kidNodes []string
	for i, t := range ts {
		if t.n == nil {
			continue
		}
		var names []string
		if f := r.GetComponentFilters()[int32(t.n.typ)]; f != nil {
			names = f.GetComponents()
		} else if len(r.GetComponentFilters()) > 0 {
			continue // filters name other node types only: this one is not updated
		}
		if err := a.checkCredentials(t.n, t.req.GetBmcEndpoint()); err != nil {
			errs[i] = err
			continue
		}
		comps, skipped := a.components(t.n, names)
		j, err := a.update(ctx, t.n, doc, comps, "ApplyFirmwareObject")
		if err != nil {
			errs[i] = err
			continue
		}
		j.kind, j.rackID, j.nodeID = kindFirmware, t.req.GetRackId(), t.id()
		if len(skipped) > 0 {
			inner := j.done
			j.done = func(j *job) error {
				_ = inner(j)
				var m map[string]any
				_ = json.Unmarshal([]byte(j.result), &m)
				m["skipped"] = skipped
				b, _ := json.Marshal(m)
				j.result = string(b)
				return nil
			}
		}
		kids = append(kids, j)
		kidNodes = append(kidNodes, t.id())
	}
	parent.children = kids
	a.jobs.add(parent)
	for i, k := range kids {
		resp.Jobs = append(resp.Jobs, &rmspb.NodeFirmwareJobInfo{NodeId: kidNodes[i], JobId: k.id})
	}
	resp.Response = batch(ts, errs, parent.id)
	return resp, nil
}

func (a *Adapter) ApplySwitchSystemImage(ctx context.Context, r *rmspb.ApplySwitchSystemImageRequest) (*rmspb.ApplySwitchSystemImageResponse, error) {
	doc, err := parseSOT(r.GetConfigJson())
	if err != nil {
		return nil, err
	}
	ts := a.resolve(r.GetNodes())
	parent := &job{kind: kindImage, rackID: r.GetRackId()}
	errs := map[int]error{}
	resp := &rmspb.ApplySwitchSystemImageResponse{ObjectId: doc.ID}
	type kid struct {
		j  *job
		id string
	}
	var kids []kid
	for i, t := range ts {
		j := &job{kind: kindImage, rackID: t.req.GetRackId(), nodeID: t.id()}
		switch {
		case t.n == nil:
			j.fail = t.err.Error()
		case t.n.kind != "switch":
			j.fail = "not a switch tray"
		default:
			if err := a.checkCredentials(t.n, t.req.GetHostEndpoint()); err != nil {
				j.fail = err.Error()
				break
			}
			u, err := a.update(ctx, t.n, doc, []string{"nvos"}, "ApplySwitchSystemImage")
			if err != nil {
				j.fail = err.Error()
				break
			}
			u.kind, u.rackID, u.nodeID = j.kind, j.rackID, j.nodeID
			j = u
			if resp.ImageFilename == "" {
				resp.ImageFilename = doc.Image
				if resp.ImageFilename == "" {
					resp.ImageFilename = "nvos-" + doc.version("switch", "nvos") + ".bin"
				}
			}
		}
		if j.fail != "" {
			errs[i] = fmt.Errorf("%s", j.fail)
		}
		kids = append(kids, kid{j, t.id()})
		parent.children = append(parent.children, j)
	}
	a.jobs.add(parent)
	for _, k := range kids {
		resp.Jobs = append(resp.Jobs, &rmspb.SwitchSystemImageUpdateJobInfo{NodeId: k.id, JobId: k.j.id})
	}
	resp.Response = batch(ts, errs, parent.id)
	return resp, nil
}

// ---- switch lifecycle

func (a *Adapter) ConfigureSwitchCertificate(ctx context.Context, r *rmspb.ConfigureSwitchCertificateRequest) (*rmspb.ConfigureSwitchCertificateResponse, error) {
	ts := a.resolve(r.GetNodes())
	var services []string
	for _, s := range r.GetServices() {
		services = append(services, s.String())
	}
	errs := map[int]error{}
	parent := &job{kind: kindCertificate}
	resp := &rmspb.ConfigureSwitchCertificateResponse{}
	var ids []string
	for i, t := range ts {
		if t.n == nil {
			continue // a per-node failure with no job, as the callers read per-node results
		}
		if t.n.kind != "switch" {
			errs[i] = fmt.Errorf("not a switch tray")
			continue
		}
		if err := a.nvosUp(t.n); err != nil {
			errs[i] = err
			continue
		}
		n := t.n
		j := &job{kind: kindCertificate, rackID: t.req.GetRackId(), nodeID: t.id(), dev: n.dev, ends: a.rt.Engine.Now().Add(a.scaled(certificateSeconds)),
			desc: "installing certificates"}
		j.done = func(j *job) error {
			a.fab.mu.Lock()
			a.fab.rack(n.rack.Path).Certs[n.dev.Path] = services
			a.fab.saveLocked()
			a.fab.mu.Unlock()
			b, _ := json.Marshal(map[string]any{"node": n.dev.Path, "services": services, "testHello": r.GetTestHello()})
			j.result = string(b)
			return nil
		}
		parent.children = append(parent.children, j)
		ids = append(ids, t.id())
		if parent.rackID == "" {
			parent.rackID = t.req.GetRackId()
		}
	}
	a.jobs.add(parent)
	for i, k := range parent.children {
		resp.Jobs = append(resp.Jobs, &rmspb.ConfigureSwitchCertificateJobInfo{NodeId: ids[i], JobId: k.id})
	}
	resp.Response = batch(ts, errs, parent.id)
	return resp, nil
}

// switchJobs issues a parent with a child per switch, each running for a behaviour duration and
// applying effect when done; a node that matches no switch gets a child that fails, as callers
// of these RPCs learn the outcome from the jobs alone.
func (a *Adapter) switchJobs(ts []target, kind string, seconds float64, desc string, check func(*node, *rmspb.NodeInfo) error, effect func(*node) error) (*job, map[int]error) {
	parent := &job{kind: kind}
	errs := map[int]error{}
	for i, t := range ts {
		j := &job{kind: kind, rackID: t.req.GetRackId(), nodeID: t.id(), desc: desc, ends: a.rt.Engine.Now().Add(a.scaled(seconds))}
		switch {
		case t.n == nil:
			j.fail = t.err.Error()
		case t.n.kind != "switch":
			j.fail = "not a switch tray"
		default:
			if err := check(t.n, t.req); err != nil {
				j.fail = err.Error()
				break
			}
			n := t.n
			j.dev = n.dev
			j.done = func(*job) error { return effect(n) }
		}
		if j.fail != "" {
			errs[i] = fmt.Errorf("%s", j.fail)
		}
		parent.children = append(parent.children, j)
		if parent.rackID == "" {
			parent.rackID = t.req.GetRackId()
		}
	}
	a.jobs.add(parent)
	return parent, errs
}

func (a *Adapter) UpdateSwitchSystemPassword(ctx context.Context, r *rmspb.UpdateSwitchSystemPasswordRequest) (*rmspb.UpdateSwitchSystemPasswordResponse, error) {
	if r.GetUsername() == "" || r.GetPassword() == "" {
		return nil, status.Error(codes.InvalidArgument, "username and password are mandatory")
	}
	ts := a.resolve(r.GetNodes())
	who := peerAddr(ctx)
	parent, errs := a.switchJobs(ts, kindPassword, passwordSeconds, "updating the NVOS password",
		func(n *node, req *rmspb.NodeInfo) error {
			if err := a.nvosUp(n); err != nil {
				return err
			}
			return a.checkCredentials(n, req.GetHostEndpoint())
		},
		func(n *node) error {
			return a.rt.Engine.UpdateAccount(engine.Request{Path: n.dev.Path, Canonical: "account.password", Protocol: Protocol, Method: "UpdateSwitchSystemPassword",
				Target: "/rack_manager.RackManager/UpdateSwitchSystemPassword", Source: schema.EventSource{Class: "stack", Principal: "rms-client", Address: who}},
				r.GetUsername(), map[string]any{"Password": r.GetPassword()})
		})
	return &rmspb.UpdateSwitchSystemPasswordResponse{Response: batch(ts, errs, parent.id)}, nil
}

func (a *Adapter) BatchResetSwitchFactoryDefault(ctx context.Context, r *rmspb.BatchResetSwitchFactoryDefaultRequest) (*rmspb.BatchResetSwitchFactoryDefaultResponse, error) {
	ts := a.resolve(r.GetNodes())
	parent, errs := a.switchJobs(ts, kindReset, factoryResetSeconds, "resetting NVOS to factory defaults",
		func(n *node, _ *rmspb.NodeInfo) error { return a.nvosUp(n) },
		func(n *node) error {
			a.fab.mu.Lock()
			defer a.fab.mu.Unlock()
			rf := a.fab.rack(n.rack.Path)
			if rf.Primary == n.dev.Path {
				rf.Primary, rf.Configured = "", false
			}
			delete(rf.Certs, n.dev.Path)
			a.fab.saveLocked()
			return nil
		})
	return &rmspb.BatchResetSwitchFactoryDefaultResponse{Response: batch(ts, errs, parent.id)}, nil
}

// ---- scale-up fabric

// switches are the matched switch trays of a request, with the rack of the first.
func switches(ts []target) (cands []*node, rack *model.Rack) {
	for _, t := range ts {
		if t.n != nil && t.n.kind == "switch" {
			cands = append(cands, t.n)
			if rack == nil {
				rack = t.n.rack
			}
		}
	}
	return cands, rack
}

func (a *Adapter) GetScaleUpFabricStatus(_ context.Context, r *rmspb.GetScaleUpFabricStatusRequest) (*rmspb.GetScaleUpFabricStatusResponse, error) {
	ts := a.resolve(r.GetNodes())
	cands, rack := switches(ts)
	fs := &rmspb.ScaleUpFabricStatus{}
	primary := ""
	if rack != nil {
		primary = a.fab.ensurePrimary(rack.Path, cands)
		a.fab.mu.Lock()
		rf := a.fab.rack(rack.Path)
		fs.TopologyType = rf.Topology
		for _, s := range rf.Static {
			fs.ExtraStaticConfigs = append(fs.ExtraStaticConfigs, &rmspb.ScaleUpFabricStaticConfig{ConfigFileName: s.File, Key: s.Key, Value: s.Value})
		}
		a.fab.mu.Unlock()
	}
	for _, t := range ts {
		sw := &rmspb.ScaleUpFabricSwitchStatus{NodeId: t.id()}
		switch {
		case t.n == nil:
			sw.ErrorMessage = t.err.Error()
		case t.n.kind != "switch":
			sw.ErrorMessage = "not a switch tray"
		case a.nvosUp(t.n) != nil:
			sw.ErrorMessage = a.nvosUp(t.n).Error()
		default:
			sw.Enabled = t.n.dev.Path == primary
			sw.FabricManagerStatus = "ok"
		}
		fs.Switches = append(fs.Switches, sw)
	}
	return &rmspb.GetScaleUpFabricStatusResponse{Status: rmspb.ReturnCode_RETURN_CODE_SUCCESS, FabricStatus: fs}, nil
}

// domainState is the engine's state of the scale-up domain a switch tray serves.
func (a *Adapter) domainState(n *node) string {
	for _, d := range n.rack.Domains {
		if d.Kind == "scale-up" {
			return a.rt.Engine.DomainState(d.Path)
		}
	}
	return ""
}

func (a *Adapter) BatchGetScaleUpFabricServiceStatus(_ context.Context, r *rmspb.BatchGetScaleUpFabricServiceStatusRequest) (*rmspb.BatchGetScaleUpFabricServiceStatusResponse, error) {
	ts := a.resolve(r.GetNodes())
	cands, rack := switches(ts)
	primary := ""
	if rack != nil {
		primary = a.fab.ensurePrimary(rack.Path, cands)
	}
	resp := &rmspb.BatchGetScaleUpFabricServiceStatusResponse{ServiceStatuses: map[string]*rmspb.ScaleUpFabricServiceStatusEntry{}}
	errs := map[int]error{}
	for i, t := range ts {
		e := &rmspb.ScaleUpFabricServiceStatusEntry{}
		var err error
		switch {
		case t.n == nil:
			err = t.err
		case t.n.kind != "switch":
			err = fmt.Errorf("not a switch tray")
		default:
			err = a.nvosUp(t.n)
		}
		if err != nil {
			e.ErrorMessage, errs[i] = err.Error(), err
		} else {
			// what NVOS's cluster apps report for nmx-controller: the primary's control plane
			// follows the domain (the NVUE adapter shows the same)
			st := map[string]string{"status": "ok"}
			if t.n.dev.Path == primary {
				st["addition-info"] = "CONTROL_PLANE_STATE_CONFIGURED"
				if a.domainState(t.n) == "unformed" {
					st["status"], st["addition-info"] = "not ok", "CONTROL_PLANE_STATE_NOT_CONFIGURED"
				}
			}
			b, _ := json.Marshal(st)
			e.StatusJson = string(b)
		}
		resp.ServiceStatuses[t.id()] = e
	}
	resp.Status, _, resp.Stats = outcome(ts, errs)
	return resp, nil
}

// ConfigureScaleUpFabricManager (V1) is not served: NICo calls the V2 spelling.
func (a *Adapter) ConfigureScaleUpFabricManager(context.Context, *rmspb.ConfigureScaleUpFabricManagerRequest) (*rmspb.ConfigureScaleUpFabricManagerResponse, error) {
	return nil, status.Error(codes.Unimplemented, "the dc-twin RMS serves ConfigureScaleUpFabricManager in rack_manager_v2 only")
}

// ConfigureScaleUpFabricManager (V2) starts one reconciliation job for the rack: it elects the
// primary (the requested switch when it is one of the rack's, else the lowest) and records the
// topology and static configuration once the job's time is up. A request whose configuration
// already holds completes at once.
func (v *v2) ConfigureScaleUpFabricManager(ctx context.Context, r *rmsv2pb.ConfigureScaleUpFabricManagerRequest) (*rmsv2pb.ConfigureScaleUpFabricManagerResponse, error) {
	a := v.a
	if len(r.GetNodes().GetNodes()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "nodes is required: name the switches that form the scale-up fabric")
	}
	topo := r.GetConfig().GetTopologyType()
	if topo == "" {
		return nil, status.Error(codes.InvalidArgument, "config.topology_type is required")
	}
	var static []staticConfig
	seen := map[string]bool{}
	for _, s := range r.GetConfig().GetExtraStaticConfigs() {
		k := s.GetConfigFileName() + "\x00" + s.GetKey()
		if seen[k] {
			return nil, status.Errorf(codes.InvalidArgument, "static config %s/%s is given twice", s.GetConfigFileName(), s.GetKey())
		}
		seen[k] = true
		static = append(static, staticConfig{File: s.GetConfigFileName(), Key: s.GetKey(), Value: s.GetValue()})
	}
	ts := a.resolve(r.GetNodes())
	cands, rack := switches(ts)
	j := &job{kind: kindFabric, rackID: ts[0].req.GetRackId(), desc: "reconciling the scale-up fabric", ends: a.rt.Engine.Now().Add(a.scaled(fabricSeconds))}
	if rack == nil {
		_, msg, _ := outcome(ts, nil)
		j.fail = "no switch in the request matches a switch tray: " + msg
		a.jobs.add(j)
		return &rmsv2pb.ConfigureScaleUpFabricManagerResponse{JobId: j.id}, nil
	}
	var primary *node
	if want := r.GetPrimarySwitchNodeId(); want != "" {
		for _, t := range ts {
			if t.id() == want && t.n != nil && t.n.kind == "switch" {
				primary = t.n
			}
		}
	}
	if primary == nil {
		primary = lowest(append([]*node{}, cands...))
	}
	ids := map[*model.Device]string{}
	for _, t := range ts {
		if t.n != nil {
			ids[t.n.dev] = t.id()
		}
	}
	a.fab.mu.Lock()
	rf := a.fab.rack(rack.Path)
	same := rf.Configured && rf.Primary == primary.dev.Path && rf.Topology == topo && fmt.Sprint(rf.Static) == fmt.Sprint(static)
	a.fab.mu.Unlock()
	if same {
		j.ends = a.rt.Engine.Now()
		j.desc = "the observed configuration already matches"
	}
	j.done = func(j *job) error {
		for _, n := range cands {
			if err := a.nvosUp(n); err != nil && n == primary {
				return err
			}
		}
		a.fab.mu.Lock()
		rf := a.fab.rack(rack.Path)
		rf.Primary, rf.Topology, rf.Static, rf.Configured = primary.dev.Path, topo, static, true
		a.fab.saveLocked()
		a.fab.mu.Unlock()
		fs := &rmspb.ScaleUpFabricStatus{TopologyType: topo}
		for _, s := range static {
			fs.ExtraStaticConfigs = append(fs.ExtraStaticConfigs, &rmspb.ScaleUpFabricStaticConfig{ConfigFileName: s.File, Key: s.Key, Value: s.Value})
		}
		for _, n := range cands {
			fs.Switches = append(fs.Switches, &rmspb.ScaleUpFabricSwitchStatus{NodeId: ids[n.dev], Enabled: n == primary, FabricManagerStatus: "ok"})
		}
		b, _ := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(fs)
		j.result = string(b)
		return nil
	}
	a.jobs.add(j)
	return &rmsv2pb.ConfigureScaleUpFabricManagerResponse{JobId: j.id}, nil
}

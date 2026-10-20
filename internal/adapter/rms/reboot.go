package rms

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/BudEcosystem/dc-twin/internal/adapter/rms/rmspb"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// ExecuteColdReboot runs the rack cold-reboot sequence the RPC documents over the supplied
// devices, through the engine: compute trays shut down gracefully, the power shelves are switched
// off and the rack discharges (every tray's BMC loses power), the shelves come back and the BMCs
// boot, the switch trays power on (NVOS ready), then the compute trays power on and boot. Each
// stage waits at least its floor and at most its poll maximum, on the engine clock and scaled by
// spec.timeScale like every behaviour; a stage that runs out of time fails the job, with no
// rollback. The coordinator job reports the runbook step it is on and writes the stage timeline
// to result_json.
func (a *Adapter) ExecuteColdReboot(ctx context.Context, r *rmspb.ExecuteColdRebootRequest) (*rmspb.ExecuteColdRebootResponse, error) {
	ts := a.resolve(r.GetNodes())
	var bad []string
	var rack *model.Rack
	group := map[string][]*node{}
	for _, t := range ts {
		if t.n == nil {
			bad = append(bad, fmt.Sprintf("%s: %v", t.id(), t.err))
			continue
		}
		if rack == nil {
			rack = t.n.rack
		}
		if t.n.rack != rack {
			bad = append(bad, fmt.Sprintf("%s: not in rack %s", t.id(), rack.Name))
			continue
		}
		group[t.n.kind] = append(group[t.n.kind], t.n)
	}
	if len(bad) > 0 {
		return nil, status.Errorf(codes.InvalidArgument, "cold reboot rejected before any power action: %s", strings.Join(bad, "; "))
	}
	if len(group["powershelf"]) == 0 || len(group["compute"]) == 0 || len(group["switch"]) == 0 {
		return nil, status.Error(codes.InvalidArgument, "cold reboot rejected before any power action: the nodes must include at least one power shelf, one compute tray and one switch tray of one rack")
	}
	secs := func(v *uint32, def float64) time.Duration {
		if v != nil && *v > 0 {
			return a.scaled(float64(*v))
		}
		return a.scaled(def)
	}
	type stage struct {
		name           string
		floor, max     time.Duration
		start          func()
		ready          func() bool
		started, ended time.Time
		status         string
	}
	who := peerAddr(ctx)
	power := func(ns []*node, canonical string) func() {
		return func() {
			for _, n := range ns {
				req := a.request(ctx, n, canonical, "ExecuteColdReboot", "rms-client")
				req.Source.Address = who
				_ = a.rt.Engine.Power(req)
			}
		}
	}
	all := func(ns []*node, f func(*model.Device) bool) func() bool {
		return func() bool {
			ok := true
			a.rt.Engine.Read(func(*model.Model) {
				for _, n := range ns {
					ok = ok && f(n.dev)
				}
			})
			return ok
		}
	}
	trays := append(append([]*node{}, group["compute"]...), group["switch"]...)
	stages := []*stage{
		{name: "Rack reboot step 1-2: shutting down the compute trays", floor: secs(r.PrestageShutdownFloorSeconds, 0), max: secs(r.PrestageShutdownPollMaxSeconds, 300),
			start: power(group["compute"], "power.off.graceful"), ready: all(group["compute"], func(d *model.Device) bool { return d.State.Power == model.Off })},
		{name: "Rack reboot step 3-4: power shelves off, waiting for the rack to discharge", floor: secs(r.DischargeFloorSeconds, 180), max: secs(r.DischargePollMaxSeconds, 600),
			start: power(group["powershelf"], "power.off.force"), ready: all(trays, func(d *model.Device) bool { return !d.State.BMCUp })},
		{name: "Rack reboot step 5-6: power shelves on, waiting for the tray BMCs", floor: secs(r.BmcUpFloorSeconds, 120), max: secs(r.BmcUpPollMaxSeconds, 600),
			start: power(group["powershelf"], "power.on"), ready: all(trays, func(d *model.Device) bool { return d.State.AC && d.State.BMCUp })},
		{name: "Rack reboot step 7-8: switch trays on, waiting for NVOS", floor: 0, max: secs(r.BmcUpPollMaxSeconds, 600),
			start: power(group["switch"], "power.on"), ready: all(group["switch"], func(d *model.Device) bool { return d.State.Power == model.On })},
		{name: "Rack reboot step 10-12: compute trays on, waiting for the operating system to boot", floor: secs(r.ComputeBootWaitSeconds, 300), max: secs(r.ComputeBootWaitSeconds, 300) + secs(r.BmcUpPollMaxSeconds, 600),
			start: power(group["compute"], "power.on"), ready: all(group["compute"], func(d *model.Device) bool { return d.State.Power == model.On })},
	}
	cur := 0
	timeline := func() string {
		var out []map[string]any
		for _, s := range stages {
			if s.started.IsZero() {
				continue
			}
			e := map[string]any{"stage": s.name, "started": s.started.Format(time.RFC3339Nano), "status": s.status}
			if !s.ended.IsZero() {
				e["ended"] = s.ended.Format(time.RFC3339Nano)
			}
			out = append(out, e)
		}
		b, _ := json.Marshal(map[string]any{"rack": rack.Path, "stages": out})
		return string(b)
	}
	j := &job{kind: kindReboot, rackID: ts[0].req.GetRackId()}
	j.step = func(j *job, now time.Time) (jobState, string) {
		for cur < len(stages) {
			s := stages[cur]
			if s.started.IsZero() {
				s.started, s.status = now, "running"
				s.start()
			}
			elapsed := now.Sub(s.started)
			if elapsed >= s.floor && s.ready() {
				s.ended, s.status = now, "completed"
				cur++
				continue
			}
			if elapsed > s.max {
				s.ended, s.status = now, "failed"
				j.result = timeline()
				return failed, fmt.Sprintf("%s: not done after %s", s.name, s.max)
			}
			return running, s.name
		}
		j.result = timeline()
		return completed, ""
	}
	a.jobs.add(j)
	resp := batch(ts, nil, j.id)
	resp.Message = "cold reboot started"
	return &rmspb.ExecuteColdRebootResponse{Response: resp}, nil
}

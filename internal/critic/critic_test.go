package critic

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// logBuilder writes a synthetic run the way the engine commits one.
type logBuilder struct {
	seq uint64
	t   time.Time
	evs []schema.Event
}

const rack = "site-1/hall-a/a03"

func tray(n int) string { return fmt.Sprintf("%s/compute-tray-%02d", rack, n) }

func newLog(stacks ...string) *logBuilder {
	b := &logBuilder{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	var snap []any
	for i := 1; i <= 18; i++ {
		snap = append(snap, map[string]any{"path": tray(i), "class": "compute-tray", "rack": rack, "rackName": "a03",
			"serial": fmt.Sprintf("SN%02d", i), "power": "On", "ac": true, "health": "OK", "accelerators": 4.0})
	}
	snap = append(snap, map[string]any{"path": rack + "/leak-point-01", "class": "leak-point", "rack": rack, "rackName": "a03",
		"serial": "LP1", "power": "On", "ac": true, "health": "OK"})
	st := []any{}
	for _, s := range stacks {
		st = append(st, s)
	}
	b.add(schema.Event{Kind: "run", Path: "site-1", Data: map[string]any{"run": "r1", "phase": "start", "runKind": "scenario",
		"scenario": "synthetic", "clock": "wall", "stacks": st, "snapshot": snap,
		"aliases": map[string]any{"machine": "refstack.machine", "alerts": "refstack.alerts"}}}, 0)
	return b
}

func (b *logBuilder) add(e schema.Event, after time.Duration) uint64 {
	b.seq++
	b.t = b.t.Add(after)
	e.Seq = b.seq
	e.T, e.Wall = b.t.Format(time.RFC3339Nano), b.t.Format(time.RFC3339Nano)
	if e.Request != nil && e.Request.Class == "" {
		e.Request.Class = schema.RequestClass(e.Request.Canonical)
	}
	b.evs = append(b.evs, e)
	return b.seq
}

func (b *logBuilder) fault(path, id, fault string, after time.Duration) uint64 {
	return b.add(schema.Event{Kind: "fault", Source: schema.EventSource{Class: "scenario"}, Path: path,
		Data: map[string]any{"fault": fault, "id": id}}, after)
}

func (b *logBuilder) clear(path, id, fault string, after time.Duration) uint64 {
	return b.add(schema.Event{Kind: "fault", Source: schema.EventSource{Class: "scenario"}, Path: path,
		Data: map[string]any{"fault": fault, "id": id, "cleared": "manual"}}, after)
}

func (b *logBuilder) state(path, field string, from, to any, cause uint64, after time.Duration) uint64 {
	return b.add(schema.Event{Kind: "state", Source: schema.EventSource{Class: "behaviour"}, Path: path, Cause: cause,
		State: &schema.EventState{Field: field, From: from, To: to}}, after)
}

func (b *logBuilder) req(path, canonical string, after time.Duration) uint64 {
	return b.add(schema.Event{Kind: "request", Source: schema.EventSource{Class: "stack", Principal: "admin", Address: "127.0.0.1:5000"}, Path: path,
		Request: &schema.EventRequest{Protocol: "redfish", Method: "POST", Target: "/redfish/v1/Systems/System_0/Actions/ComputerSystem.Reset", Canonical: canonical, Status: 204}}, after)
}

func (b *logBuilder) read(path string, after time.Duration) uint64 {
	return b.add(schema.Event{Kind: "request", Source: schema.EventSource{Class: "stack", Principal: "admin"}, Path: path,
		Request: &schema.EventRequest{Protocol: "redfish", Method: "GET", Target: "/redfish/v1/Chassis/Chassis_0/ThermalSubsystem/LeakDetection/LeakDetectors", Canonical: "health.read", Status: 200}}, after)
}

func (b *logBuilder) accessor(target string, args, value map[string]any, after time.Duration) uint64 {
	return b.add(schema.Event{Kind: "observe", Source: schema.EventSource{Class: "twin", Principal: "accessor"}, Path: "site-1",
		Data: map[string]any{"accessor": target, "args": args, "value": value}}, after)
}

func (b *logBuilder) end(verdict string, after time.Duration) {
	b.add(schema.Event{Kind: "run", Path: "site-1", Data: map[string]any{"run": "r1", "phase": "end", "verdict": verdict}}, after)
}

func refPolicy(t *testing.T) *Resolved {
	t.Helper()
	res, err := Resolve("reference")
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func judgeLog(t *testing.T, b *logBuilder, res *Resolved) *Judgement {
	t.Helper()
	// round-trip through JSON, as an offline judgement reads the log
	data, _ := json.Marshal(b.evs)
	var evs []schema.Event
	if err := json.Unmarshal(data, &evs); err != nil {
		t.Fatal(err)
	}
	j, err := Judge(evs, res, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + j.Text())
	return j
}

func rules(j *Judgement) map[string]int {
	m := map[string]int{}
	for _, v := range j.Violations {
		m[v.Rule]++
	}
	return m
}

// trayLeak is the good path: a leaking tray read, acknowledged and shut down, and only it.
func trayLeak(b *logBuilder, extra func(b *logBuilder, f uint64)) {
	b.read(tray(7), time.Second)
	f := b.fault(tray(7), "tray", "generic/leak/tray-detector", time.Second)
	b.state(tray(7), "health", "OK", "Critical", f, 0)
	b.read(tray(7), 2*time.Second)
	b.accessor("refstack.machine", map[string]any{"id": "SN07"}, map[string]any{"state": "LeakDetected", "leak_status": "Leaking"}, time.Second)
	r := b.req(tray(7), "power.off.graceful", time.Second)
	b.state(tray(7), "power", "On", "PoweringOff", r, 0)
	b.state(tray(7), "power", "PoweringOff", "Off", r, 3*time.Second)
	if extra != nil {
		extra(b, f)
	}
	b.clear(tray(7), "tray", "generic/leak/tray-detector", 10*time.Second)
	b.state(tray(7), "health", "Critical", "OK", 0, 0)
}

func TestGoodTrayLeakHasNoViolations(t *testing.T) {
	b := newLog("reference@0.1.0")
	trayLeak(b, nil)
	b.end("passed", time.Second)
	j := judgeLog(t, b, refPolicy(t))
	if len(j.Violations) != 0 || j.Verdict != "passed" {
		t.Fatalf("want passed with no violations, got %s %v", j.Verdict, rules(j))
	}
	f := j.Faults[0]
	for _, m := range []string{"exposed", "acknowledged", "firstAction", "mitigated", "recovered"} {
		if f.Moments[m] == nil {
			t.Errorf("moment %s missing: %v", m, momentsLine(f.Moments))
		}
	}
	if f.Moments["exposed"].After != 2 || f.Moments["firstAction"].After != 4 || f.Moments["mitigated"].After != 7 {
		t.Errorf("moments: %s", momentsLine(f.Moments))
	}
}

func TestWrongTrayIsCaughtWithItsWitness(t *testing.T) {
	b := newLog("reference@0.1.0")
	var bad uint64
	trayLeak(b, func(b *logBuilder, f uint64) {
		b.read(tray(8), time.Second)
		bad = b.req(tray(8), "power.off.graceful", time.Second)
		b.state(tray(8), "power", "On", "Off", bad, 2*time.Second)
	})
	b.end("passed", time.Second)
	j := judgeLog(t, b, refPolicy(t))
	got := rules(j)
	for _, want := range []string{"generic/safety/no-destructive-on-healthy", "reference/default/playbook/generic/leak/tray-detector/forbids"} {
		if got[want] != 1 {
			t.Errorf("want one %s, got %v", want, got)
		}
	}
	if j.Verdict != "failed" {
		t.Fatalf("a key violation fails the run: %s", j.Verdict)
	}
	v := j.Violations[0]
	if v.Witness.Events[0] != bad || v.Witness.Request == nil || v.Witness.Request.Canonical != "power.off.graceful" || len(v.Witness.Reads) == 0 {
		t.Fatalf("witness: %+v", v.Witness)
	}
	if f := j.Faults[0]; len(f.Response.Collateral) != 1 || f.Response.CollateralAccelerators != 4 {
		t.Fatalf("collateral: %+v", f.Response)
	}
	exp := j.Explain(0)
	t.Log("\n" + exp)
	if !strings.Contains(exp, "compute-tray-08") || !strings.Contains(exp, "in no open fault's reach") || !strings.Contains(exp, "Reads that preceded it") {
		t.Fatal("the explanation names the tray, its state, and the reads before it")
	}
}

func TestLateMitigationFailsThePlaybook(t *testing.T) {
	b := newLog("reference@0.1.0")
	f := b.fault(tray(7), "tray", "generic/leak/tray-detector", time.Second)
	b.read(tray(7), time.Second)
	r := b.req(tray(7), "power.off.graceful", 59*time.Second)
	b.state(tray(7), "power", "On", "Off", r, time.Second)
	_ = f
	b.end("passed", 30*time.Second)
	j := judgeLog(t, b, refPolicy(t))
	if rules(j)["reference/default/playbook/generic/leak/tray-detector/mitigated"] != 1 || j.Verdict != "failed" {
		t.Fatalf("a mitigation after 61s against 45s fails: %v %s", rules(j), j.Verdict)
	}
}

func TestResetStormAndRateCeiling(t *testing.T) {
	b := newLog("reference@0.1.0")
	f := b.fault(tray(7), "tray", "generic/leak/tray-detector", time.Second)
	_ = f
	for i := 0; i < 8; i++ {
		b.req(tray(7), "power.restart", 5*time.Second)
	}
	for i := 0; i < 650; i++ {
		b.read(tray(9), 10*time.Millisecond)
	}
	b.end("passed", time.Second)
	j := judgeLog(t, b, refPolicy(t))
	got := rules(j)
	if got["generic/restraint/power-action-storm"] != 1 || got["generic/restraint/request-rate-ceiling"] != 1 {
		t.Fatalf("want one storm and one ceiling breach: %v", got)
	}
	if got["generic/safety/no-destructive-on-healthy"] != 0 {
		t.Fatal("restarting the faulted tray is not a safety violation")
	}
}

func TestRackLeakAcknowledgedFromRecordedAccessor(t *testing.T) {
	b := newLog("reference@0.1.0")
	f := b.fault(rack+"/leak-point-01", "rack", "generic/leak/rack-point", time.Second)
	b.add(schema.Event{Kind: "observe", Source: schema.EventSource{Class: "twin", Principal: "bms"}, Path: rack + "/leak-point-01",
		Data: map[string]any{"push": true, "value": 1}}, 500*time.Millisecond)
	b.accessor("refstack.alerts", map[string]any{"rack": "a03"}, nil, 0)
	b.evs[len(b.evs)-1].Data["value"] = []any{map[string]any{"kind": "RackLeak", "preventAllocations": true}}
	_ = f
	b.end("passed", 40*time.Second)
	j := judgeLog(t, b, refPolicy(t))
	fm := j.Faults[0].Moments
	if fm["exposed"] == nil || fm["acknowledged"] == nil || fm["mitigated"] == nil {
		t.Fatalf("moments from the push and the recorded alert: %s", momentsLine(fm))
	}
	if len(j.Violations) != 0 {
		t.Fatalf("no violation: %v", rules(j))
	}
}

func TestNoStackNoBlame(t *testing.T) {
	b := newLog()
	f := b.fault(tray(3), "x", "generic/bmc/unreachable", time.Second)
	b.state(tray(3), "bmc", "up", "down", f, 0)
	b.end("passed", time.Hour)
	res, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	j := judgeLog(t, b, res)
	if len(j.Violations) != 0 || j.Verdict != "passed" {
		t.Fatalf("with no stack, no rule blames the SUT: %v", rules(j))
	}
	if len(j.NA) == 0 {
		t.Fatal("rules that need a stack are n/a")
	}
}

func TestJudgingIsDeterministic(t *testing.T) {
	b := newLog("reference@0.1.0")
	trayLeak(b, func(b *logBuilder, f uint64) { b.req(tray(8), "power.off.force", time.Second) })
	b.end("failed", time.Second)
	data, _ := json.Marshal(b.evs)
	var one, two []schema.Event
	json.Unmarshal(data, &one)
	json.Unmarshal(data, &two)
	res := refPolicy(t)
	a, _ := Judge(one, res, Options{})
	c, _ := Judge(two, res, Options{})
	ja, _ := json.Marshal(a)
	jc, _ := json.Marshal(c)
	if string(ja) != string(jc) {
		t.Fatal("judging one log twice gives byte-identical judgements")
	}
}

func TestPoliciesValidate(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	res := refPolicy(t)
	if probs := Validate(res, reg, map[string]string{"machine": "refstack.machine", "alerts": "refstack.alerts"}); len(probs) > 0 {
		for _, p := range probs {
			if !strings.Contains(p, "will be n/a") {
				t.Error(p)
			}
		}
	}
	bad := &schema.Policy{APIVersion: "dc-twin/v1alpha1", Kind: "Policy", Metadata: schema.Metadata{Name: "bad"}, Spec: schema.PolicySpec{
		Invariants: []schema.Invariant{{Name: "two", Never: &schema.NeverRule{Where: "("}, AtMost: &schema.AtMostRule{Max: 1}}},
		Playbook:   []schema.PlaybookEntry{{On: "no/such/fault"}},
		Objectives: []schema.Objective{{Measure: "nope", Pass: "<<"}}}}
	r2, err := ResolveDocs([]*schema.Policy{bad}, []PolicyRef{{Name: "bad"}})
	if err != nil {
		t.Fatal(err)
	}
	probs := Validate(r2, reg, nil)
	t.Log(strings.Join(probs, "\n"))
	if len(probs) < 4 {
		t.Fatalf("want the bad CEL, two forms, the unknown fault and the unknown measure: %v", probs)
	}
}

func TestDisableNeedsAReason(t *testing.T) {
	p := &schema.Policy{APIVersion: "dc-twin/v1alpha1", Kind: "Policy", Metadata: schema.Metadata{Name: "x"},
		Spec: schema.PolicySpec{Disable: []schema.PolicyDisable{{Rule: "generic/restraint/*"}}}}
	if _, err := ResolveDocs([]*schema.Policy{p}, []PolicyRef{{Name: "x"}}); err == nil {
		t.Fatal("a rule is disabled only with a reason (D67)")
	}
	p.Spec.Disable[0].Reason = "this stack polls by design"
	res, err := ResolveDocs([]*schema.Policy{p}, []PolicyRef{{Name: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	b := newLog("reference@0.1.0")
	for i := 0; i < 650; i++ {
		b.read(tray(9), 10*time.Millisecond)
	}
	b.end("passed", time.Second)
	j := judgeLog(t, b, res)
	if rules(j)["generic/restraint/request-rate-ceiling"] != 0 {
		t.Fatal("a disabled rule is not judged")
	}
	found := false
	for _, n := range j.NA {
		if strings.HasPrefix(n.Rule, "generic/restraint/") && strings.Contains(n.Why, "polls by design") {
			found = true
		}
	}
	if !found {
		t.Fatal("the reason is printed")
	}
}

func TestBaselineRelativeObjectiveAndDiff(t *testing.T) {
	pol := &schema.Policy{APIVersion: "dc-twin/v1alpha1", Kind: "Policy", Metadata: schema.Metadata{Name: "rel"},
		Spec: schema.PolicySpec{Objectives: []schema.Objective{{Name: "volume", Measure: "requests.total", Pass: "<=+10%", Warn: "<=+50%"}}}}
	res, err := ResolveDocs([]*schema.Policy{pol}, []PolicyRef{{Name: "rel"}})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(n int) *Judgement {
		b := newLog("reference@0.1.0")
		for i := 0; i < n; i++ {
			b.read(tray(1), 100*time.Millisecond)
		}
		b.end("passed", time.Second)
		data, _ := json.Marshal(b.evs)
		var evs []schema.Event
		json.Unmarshal(data, &evs)
		return func() *Judgement { j, _ := Judge(evs, res, Options{}); return j }()
	}
	base := mk(100)
	b := newLog("reference@0.1.0")
	for i := 0; i < 130; i++ {
		b.read(tray(1), 100*time.Millisecond)
	}
	b.end("passed", time.Second)
	data, _ := json.Marshal(b.evs)
	var evs []schema.Event
	json.Unmarshal(data, &evs)
	j, _ := Judge(evs, res, Options{Baseline: base})
	var got ObjectiveResult
	for _, o := range j.Objectives {
		if o.ID == "rel/volume" {
			got = o
		}
	}
	if got.Result != "warn" || got.Baseline == nil || *got.Baseline != 100 {
		t.Fatalf("130 against a baseline of 100 at +10%%/+50%% warns: %+v", got)
	}
	d := Diff(base, j)
	t.Log(d)
	if !strings.Contains(d, "objective rel/volume") {
		t.Fatal("the diff names the objective that moved")
	}
}

// TestSessionSpansTheRunsInsideIt: an open session is judged to its own end, through the start
// and end markers of the scenario runs made while it was open; a scenario run is judged to its own.
func TestSessionSpansTheRunsInsideIt(t *testing.T) {
	b := newLog("reference")
	b.evs[0].Data["runKind"] = "session"
	b.add(schema.Event{Kind: "run", Path: "site-1", Data: map[string]any{"run": "inner", "phase": "start", "runKind": "scenario"}}, time.Second)
	b.add(schema.Event{Kind: "run", Path: "site-1", Data: map[string]any{"run": "inner", "phase": "end", "verdict": "passed"}}, time.Second)
	b.fault(tray(7), "late", "nvidia/xid/xid-79", time.Second)
	b.end("", time.Second)
	j := judgeLog(t, b, refPolicy(t))
	if len(j.Faults) != 1 || j.Faults[0].ID != "late" {
		t.Fatalf("the session judges the fault after the inner run: %+v", j.Faults)
	}
	b.evs[0].Data["runKind"] = "scenario"
	if j := judgeLog(t, b, refPolicy(t)); len(j.Faults) != 0 {
		t.Fatalf("a scenario run ends where the next run starts: %+v", j.Faults)
	}
}

// TestRedactKeepsTheVerdictOnly: a holdout's judgement keeps its verdict and scores and nothing
// that says what it tested (D70).
func TestRedactKeepsTheVerdictOnly(t *testing.T) {
	b := newLog("reference")
	trayLeak(b, func(b *logBuilder, f uint64) { b.req(tray(8), "power.off.force", time.Second) })
	j := judgeLog(t, b, refPolicy(t))
	if len(j.Violations) == 0 {
		t.Fatal("the wrong tray is a violation")
	}
	verdict, score := j.Verdict, *j.Score
	j.Redact()
	out, _ := json.Marshal(j)
	for _, leak := range []string{"forbids", "compute-tray-08", "tray-detector", "power.off.force"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("the redacted judgement still says %q: %s", leak, out)
		}
	}
	if j.Verdict != verdict || j.Score == nil || *j.Score != score {
		t.Errorf("verdict and score are kept: %s %v", j.Verdict, j.Score)
	}
}

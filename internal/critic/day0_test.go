package critic

import (
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// The day-0 policy (policies/day0/fabric.yaml, 18 — Judging) against synthetic runs of a
// two-leaf factory fabric, its events in the form the NOS observer and the engine commit them.

const (
	leaf1 = "site-1/hall-a/a10/leaf-1"
	leaf2 = "site-1/hall-a/a10/leaf-2"
)

func day0Log() *logBuilder {
	b := &logBuilder{t: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	var snap []any
	for i, p := range []string{leaf1, leaf2} {
		snap = append(snap, map[string]any{"path": p, "class": "switch", "rack": "site-1/hall-a/a10", "rackName": "a10",
			"serial": []string{"LF1", "LF2"}[i], "power": "On", "ac": true, "health": "OK"})
	}
	b.add(schema.Event{Kind: "run", Path: "site-1", Data: map[string]any{"run": "r1", "phase": "start", "runKind": "scenario",
		"scenario": "day0", "clock": "wall", "stacks": []any{"day0tool"}, "snapshot": snap}}, 0)
	return b
}

func (b *logBuilder) observe(path, field string, from, to any, after time.Duration) uint64 {
	return b.add(schema.Event{Kind: "state", Source: schema.EventSource{Class: "observer", Principal: "nos/x"}, Path: path,
		State: &schema.EventState{Field: field, From: from, To: to}}, after)
}

func (b *logBuilder) nvue(path string, after time.Duration) uint64 {
	return b.add(schema.Event{Kind: "request", Source: schema.EventSource{Class: "stack", Principal: "cumulus"}, Path: path,
		Request: &schema.EventRequest{Protocol: "nvue", Method: "revision.apply", Target: "/nvue_v1/revision/1", Canonical: "network.config.apply", Status: 200}}, after)
}

// bringUp is one switch's good day 0: booted, leased, pushed, renamed, password changed, converged.
func bringUp(b *logBuilder, path, host string) {
	b.observe(path, "day0.phase", nil, "factory", time.Second)
	b.observe(path, "day0.phase", "factory", "nos-booted", 90*time.Second)
	b.observe(path, "mgmt.lease", nil, "10.20.0.11", time.Second)
	b.observe(path, "credentials", nil, "factory", 0)
	b.observe(path, "hostname", nil, "cumulus", 0)
	b.add(schema.Event{Kind: "request", Source: schema.EventSource{Class: "stack", Principal: "cumulus"}, Path: path,
		Request: &schema.EventRequest{Protocol: "nvue", Method: "GET", Target: "/nvue_v1/interface/swp63/lldp", Canonical: "network.lldp.read", Status: 200}}, 10*time.Second)
	b.nvue(path, 5*time.Second)
	b.observe(path, "hostname", "cumulus", host, time.Second)
	b.observe(path, "credentials", "factory", "changed", time.Second)
	b.observe(path, "day0.phase", "nos-booted", "configured", time.Second)
	b.observe(path, "day0.phase", "configured", "converged", 30*time.Second)
}

func day0Policy(t *testing.T) *Resolved {
	t.Helper()
	res, err := Resolve("day0/fabric")
	if err != nil {
		t.Fatal(err)
	}
	if probs := Validate(res, nil, nil); len(probs) > 0 {
		t.Fatalf("policy problems: %v", probs)
	}
	return res
}

func day0Rules(j *Judgement) map[string]int {
	out := map[string]int{}
	for r, n := range rules(j) {
		if len(r) > 11 && r[:11] == "day0/fabric" {
			out[r] = n
		}
	}
	return out
}

func TestDay0GoodBringUpPasses(t *testing.T) {
	b := day0Log()
	bringUp(b, leaf1, "a10-leaf-1")
	bringUp(b, leaf2, "a10-leaf-2")
	b.end("passed", time.Hour)
	j := judgeLog(t, b, day0Policy(t))
	if v := day0Rules(j); len(v) != 0 {
		t.Fatalf("want no day-0 violations, got %v", v)
	}
	for _, c := range j.Coverage.Sometimes {
		if c.ID == "day0/fabric/cabling-checked" && !c.Exercised {
			t.Error("the LLDP read was not counted")
		}
	}
}

func TestDay0Violations(t *testing.T) {
	cases := []struct {
		name string
		run  func(b *logBuilder)
		want string
	}{
		{"never converges", func(b *logBuilder) {
			b.observe(leaf1, "day0.phase", nil, "factory", time.Second)
			b.observe(leaf1, "day0.phase", "factory", "nos-booted", 90*time.Second)
		}, "day0/fabric/switch-converges"},
		{"another switch's hostname", func(b *logBuilder) {
			bringUp(b, leaf1, "a10-leaf-1")
			bringUp(b, leaf2, "a10-leaf-1")
		}, "day0/fabric/no-identity-of-another-switch"},
		{"factory credentials at convergence", func(b *logBuilder) {
			b.observe(leaf1, "day0.phase", nil, "nos-booted", time.Second)
			b.observe(leaf1, "credentials", nil, "factory", 0)
			b.observe(leaf1, "day0.phase", "nos-booted", "converged", time.Minute)
		}, "day0/fabric/no-factory-credentials-once-converged"},
		{"lease lost after the first write", func(b *logBuilder) {
			bringUp(b, leaf1, "a10-leaf-1")
			b.nvue(leaf1, time.Second)
			b.observe(leaf1, "mgmt.lease", "10.20.0.11", "", time.Second)
		}, "day0/fabric/management-lease-kept"},
		{"converged over a swapped cable", func(b *logBuilder) {
			f := b.fault(leaf1+":swp63", "swap", "generic/cabling/cable-swap", time.Second)
			b.state(leaf1+":swp63", "asBuilt", "spine-1:swp1", "spine-1:swp2", f, 0)
			bringUp(b, leaf1, "a10-leaf-1")
		}, "day0/fabric/no-convergence-over-a-swapped-cable"},
		{"too many boots", func(b *logBuilder) {
			for i := 0; i < 5; i++ {
				b.observe(leaf1, "day0.phase", "ztp-done", "nos-booted", time.Minute)
			}
			b.observe(leaf1, "day0.phase", "configured", "converged", time.Minute)
		}, "day0/fabric/reboots-during-day0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := day0Log()
			c.run(b)
			b.end("failed", time.Hour)
			j := judgeLog(t, b, day0Policy(t))
			v := day0Rules(j)
			if v[c.want] == 0 || len(v) != 1 {
				t.Errorf("want only %s, got %v", c.want, v)
			}
		})
	}
}

// A lease lost across a reboot that comes back is kept.
func TestDay0LeaseBackAfterRebootIsKept(t *testing.T) {
	b := day0Log()
	bringUp(b, leaf1, "a10-leaf-1")
	b.observe(leaf1, "mgmt.lease", "10.20.0.11", "", time.Second)
	b.observe(leaf1, "day0.phase", "converged", "nos-booted", 60*time.Second)
	b.observe(leaf1, "mgmt.lease", "", "10.20.0.11", time.Second)
	b.observe(leaf1, "day0.phase", "nos-booted", "converged", 30*time.Second)
	b.end("passed", time.Hour)
	if v := day0Rules(judgeLog(t, b, day0Policy(t))); len(v) != 0 {
		t.Fatalf("want no day-0 violations, got %v", v)
	}
}

// day0/fabric is found by name but is not one of the policies every run is judged by.
func TestDay0PolicyIsOptIn(t *testing.T) {
	res, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Policies {
		if p.Name == "day0/fabric" {
			t.Fatal("day0/fabric applied by default")
		}
	}
}

// The day-0 measures (18 — Measures) of two good bring-ups and a refused change: each switch's
// times from its first day-0 observation, its applies, and the fabric's median and maximum.
func TestDay0Measures(t *testing.T) {
	b := day0Log()
	bringUp(b, leaf1, "a10-leaf-1")
	bringUp(b, leaf2, "a10-leaf-2")
	b.add(schema.Event{Kind: "request", Source: schema.EventSource{Class: "stack", Principal: "cumulus"}, Path: leaf1,
		Request: &schema.EventRequest{Protocol: "nvue", Method: "revision.refuse", Target: "/nvue_v1/router/ospf?rev=2", Canonical: "network.config.change", Status: 400}}, time.Second)
	b.end("passed", time.Hour)
	j := judgeLog(t, b, day0Policy(t))
	got := map[string]float64{}
	for _, m := range j.Measures {
		if strings.HasPrefix(m.Name, "day0.") {
			got[m.Name+" "+m.Subject] = m.Value
		}
	}
	// bringUp: factory, then nos-booted 90 s later, the lease 1 s after, converged 48 s after that
	for k, want := range map[string]float64{
		"day0.toLease " + leaf1: 91, "day0.toConverged " + leaf1: 139, "day0.toConverged " + leaf2: 139,
		"day0.applies " + leaf1: 1, "day0.appliesFailed " + leaf1: 0, "day0.ztpFailures " + leaf2: 0,
		"day0.convergedMedian ": 139, "day0.convergedMax ": 139, "day0.refused ": 1,
	} {
		if v, ok := got[k]; !ok || v != want {
			t.Errorf("%s = %v (present %v), want %v", k, v, ok, want)
		}
	}
	if _, ok := got["day0.toZTP "+leaf1]; ok {
		t.Errorf("a switch with no ZTP has a day0.toZTP measure")
	}
}

package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/stmcginnis/gofish/redfish"
)

// Mutants are seeded defects, one behind each name, for verifying the critic
// (docs/spec/17-critic.md — Verifying the critic): each must produce its violation, and the
// unmutated stack none. They are never on by default.
var mutantNames = map[string]string{
	"wrong-tray":         "shut down a healthy neighbour of a leaking tray as well",
	"rack-leak-poweroff": "power a tray off on a BMS rack leak point, which NICo's policy forbids",
	"slow":               "shut a leaking tray down only after a minute",
	"ignore-leak":        "never shut a leaking tray down",
	"chatty":             "poll every compute tray's BMC without pause, beside the health loop",
	"session-leak":       "open a Redfish session on every BMC every few seconds and never close it",
	"reset-storm":        "restart a leaking tray eight times before shutting it down",
	"eager-isolation":    "ask the BMS to isolate the rack at the first leaking tray",
	"no-isolation":       "never ask the BMS to isolate a rack",
}

type mutants map[string]bool

func parseMutants(s string) (mutants, error) {
	m := mutants{}
	for _, n := range strings.Split(s, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, ok := mutantNames[n]; !ok {
			var known []string
			for k := range mutantNames {
				known = append(known, k)
			}
			sort.Strings(known)
			return nil, fmt.Errorf("unknown mutant %q (known: %s)", n, strings.Join(known, ", "))
		}
		m[n] = true
	}
	return m, nil
}

func (m mutants) on(name string) bool { return m[name] }

// neighbour is another Ready tray of the same type and rack, powered on: the next by address.
func (s *stack) neighbour(of *Machine) *Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cands []*Machine
	for _, m := range s.machines {
		if m != of && m.client != nil && m.Type == of.Type && m.Rack == of.Rack && m.Leak != "Leaking" && m.Power == "On" {
			cands = append(cands, m)
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].BMCIP < cands[j].BMCIP })
	for _, c := range cands {
		if c.BMCIP > of.BMCIP {
			return c
		}
	}
	if len(cands) > 0 {
		return cands[0]
	}
	return nil
}

// resetSystem resets a machine's system through its own BMC client.
func (s *stack) resetSystem(m *Machine, t redfish.ResetType) error {
	systems, err := m.client.Service.Systems()
	if err != nil {
		return err
	}
	for _, sy := range systems {
		if sy.ID == "System_0" || len(systems) == 1 {
			return sy.Reset(t)
		}
	}
	return fmt.Errorf("no system")
}

// leakSession opens a Redfish session and keeps none of it: the session-leak mutant.
func (s *stack) leakSession(m *Machine) {
	if !s.mut.on("session-leak") || time.Since(m.lastSession) < 5*time.Second {
		return
	}
	m.lastSession = time.Now()
	resp, err := m.client.Post("/redfish/v1/SessionService/Sessions", map[string]string{"UserName": m.user, "Password": m.pw})
	if err == nil {
		resp.Body.Close()
	}
}

// rackLeakPowerOff is the rack-leak-poweroff mutant: one tray of the rack forced off.
func (s *stack) rackLeakPowerOff(rack string) {
	s.mu.Lock()
	var pick *Machine
	var trays []*Machine
	for _, m := range s.machines {
		if m.Rack == rack && m.client != nil && (m.Type == "gb300-tray" || m.Type == "gb200-tray") && m.Power == "On" {
			trays = append(trays, m)
		}
	}
	sort.Slice(trays, func(i, j int) bool { return trays[i].BMCIP < trays[j].BMCIP })
	if len(trays) > 0 {
		pick = trays[0]
	}
	s.mu.Unlock()
	if pick != nil {
		s.log.Printf("mutant rack-leak-poweroff: forcing %s off", pick.Serial)
		_ = s.resetSystem(pick, redfish.ForceOffResetType)
	}
}

// chatter is the chatty mutant: beside the health loop, every machine read again and again with
// nothing done with what it says, so the stack's own handling is not slowed.
func (s *stack) chatter() {
	started := map[*Machine]bool{}
	for {
		s.mu.Lock()
		for _, m := range s.machines {
			if m.client != nil && !started[m] && (m.Type == "gb300-tray" || m.Type == "gb200-tray") {
				started[m] = true
				go func(m *Machine) {
					for {
						s.readOnly(m)
						time.Sleep(50 * time.Millisecond)
					}
				}(m)
			}
		}
		s.mu.Unlock()
		time.Sleep(time.Second)
	}
}

// readOnly reads what checkMachine reads, and does nothing with it: the chatty mutant's extra
// polls.
func (s *stack) readOnly(m *Machine) {
	systems, err := m.client.Service.Systems()
	if err != nil {
		return
	}
	for _, sy := range systems {
		_ = sy.PowerState
	}
	if chassis, err := m.client.Service.Chassis(); err == nil {
		for _, ch := range chassis {
			var coll struct{ Members []struct{} }
			_ = get(m.client, ch.ODataID+"/ThermalSubsystem/LeakDetection/LeakDetectors", &coll)
		}
	}
}

package nicoagent

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// hbnSubstrate is a substrate that records the DPUs handed to it (netsim.HBNAgents).
type hbnSubstrate struct {
	mu      sync.Mutex
	started map[string]netsim.HBNAgent
	stopped map[string]int
}

func (h *hbnSubstrate) StartHBNAgent(dpu string, a netsim.HBNAgent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started[dpu] = a
	return nil
}

func (h *hbnSubstrate) StopHBNAgent(dpu string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped[dpu]++
}

// A BlueField whose HBN is emulated (D71) is booted by the stand-in as any other, and then handed
// over: once NICo's image has registered it, the installed DPU OS starts NICo's own agent in the
// emulated HBN with what the image installed (nico-api's URL, the site's root CA, the DPU's
// DiscoveryInfo as the agent reads it) and the DPU's own out-of-band path to NICo, and the
// stand-in's own agent loop does not run; a reset stops the agent and starts it again.
func TestHBNBlueFieldIsHandedOff(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	nico := newFakeNICo(t, forgeOrSkip(t, reg))
	tw, a := upDPF(t, nico)
	p := a.order[0]
	b := a.dpus[p]
	sub := &hbnSubstrate{started: map[string]netsim.HBNAgent{}, stopped: map[string]int{}}
	a.hbn = sub
	srcEv := schema.EventSource{Class: "scenario", Principal: "test"}
	tw.Engine.Read(func(m *model.Model) { m.DeviceBy[p].HBN = true })
	b.dev.HBN = true
	// a reset: its firmware boots the network (NICo asks), NICo's image, then the hand-off
	if err := tw.Engine.Power(engine.Request{Path: p, Canonical: "power.restart", Protocol: "test", Source: srcEv}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the hand-off", 90*time.Second, func() bool {
		sub.mu.Lock()
		defer sub.mu.Unlock()
		_, ok := sub.started[p]
		return ok
	})
	sub.mu.Lock()
	ha := sub.started[p]
	sub.mu.Unlock()
	in := b.loadInstalled()
	if in == nil || ha.APIURL != in.APIURL || string(ha.RootCA) != string(in.RootCA) || len(ha.RootCA) == 0 || ha.Dial == nil {
		t.Fatalf("handed over %+v, installed %+v", ha, in)
	}
	var hw map[string]any
	if err := json.Unmarshal(ha.Discovery, &hw); err != nil {
		t.Fatal(err)
	}
	if hw["machine_arch"] != float64(0) || hw["gpus"] == nil || str(hw, "dpu_info", "factory_mac_address") != b.id.hostMAC ||
		!strings.Contains(strings.ToLower(str(hw, "dmi_data", "board_name")), "bluefield") {
		t.Errorf("hw_output.json %s", ha.Discovery)
	}
	// the DPU's own path reaches NICo: a connection to nico-api over it
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	host := strings.TrimPrefix(strings.TrimPrefix(in.APIURL, "https://"), "http://")
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "443")
	}
	c, err := ha.Dial(ctx, "tcp", host)
	if err != nil {
		t.Fatalf("the DPU's own path to nico-api %s: %v", host, err)
	}
	c.Close()
	// the stand-in's agent no longer reports for it
	nico.mu.Lock()
	before := 0
	for _, s := range nico.status {
		if str(s, "dpu_machine_id", "id") == in.MachineID {
			before++
		}
	}
	nico.mu.Unlock()
	time.Sleep(3 * a.networkEvery)
	nico.mu.Lock()
	after := 0
	for _, s := range nico.status {
		if str(s, "dpu_machine_id", "id") == in.MachineID {
			after++
		}
	}
	nico.mu.Unlock()
	if after != before {
		t.Errorf("the stand-in's agent still reports for the handed-over DPU (%d reports, then %d)", before, after)
	}
	if !b.agentUp.Load() {
		t.Errorf("the DPU OS is not up after the hand-off")
	}
	// forge-scout's control loop still answers NICo
	nico.mu.Lock()
	ctl := nico.mtls["ForgeAgentControl"]
	nico.mu.Unlock()
	time.Sleep(2 * a.controlEvery)
	nico.mu.Lock()
	if nico.mtls["ForgeAgentControl"] <= ctl {
		t.Errorf("ForgeAgentControl stopped at the hand-off (%d calls)", nico.mtls["ForgeAgentControl"])
	}
	nico.mu.Unlock()
	// a reset stops NICo's agent; the DPU boots its disk and starts it again
	if err := tw.Engine.Power(engine.Request{Path: p, Canonical: "power.restart", Protocol: "test", Source: srcEv}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the agent stopped by the reset", 30*time.Second, func() bool {
		sub.mu.Lock()
		defer sub.mu.Unlock()
		return sub.stopped[p] >= 1
	})
}

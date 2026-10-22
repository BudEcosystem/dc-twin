package engine

import (
	"fmt"
	"math"
	"sort"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

// chain.go — the power chain between the feeds and the racks (facility.power.topology): which
// of its UPS, busway and panel nodes deliver, a UPS's battery on the twin clock, and what each
// node carries. recomputePower decides the chain first; the rack sources drawing through a node
// have power while it delivers.

// nodeOpen reports whether a fault has cut a node's output: a failed UPS, or an open breaker.
func (e *Engine) nodeOpen(n *model.PowerNode) bool {
	for _, f := range e.faultsOn(n.Device) {
		if v, ok := f.Sets["output"]; ok && fmt.Sprint(v) == "off" {
			return true
		}
		if v, ok := f.Sets["breaker"]; ok && fmt.Sprint(v) == "open" {
			return true
		}
	}
	return false
}

// chargeNow is a UPS's battery now, 0..1, without settling it (for readers).
func (e *Engine) chargeNow(n *model.PowerNode) float64 {
	if n.ChargeAt.IsZero() {
		return n.Charge
	}
	return math.Max(0, math.Min(1, n.Charge-n.DrainPerS*e.now.Sub(n.ChargeAt).Seconds()/e.scale))
}

// settle brings a UPS's battery up to now.
func (e *Engine) settle(n *model.PowerNode) {
	n.Charge, n.ChargeAt = e.chargeNow(n), e.now
}

// recomputeChain decides which nodes deliver, each after the node it is fed from. A UPS
// delivers while its input does (online), or from its battery while it has charge (battery);
// a busway or panel delivers while its input does and its breaker is closed.
func (e *Engine) recomputeChain(cause uint64) {
	for _, n := range e.M.Power {
		in := n.Feed != nil && n.Feed.Up
		if n.Parent != nil {
			in = n.Parent.Up
		}
		open := e.nodeOpen(n)
		mode := "closed"
		if n.Kind == "ups" {
			e.settle(n)
			switch {
			case open:
				mode = "fault"
			case in:
				mode = "online"
			case n.Charge > 0 && n.RuntimeS > 0:
				mode = "battery"
			default:
				mode = "depleted"
			}
		} else if open {
			mode = "open"
		} else if !in {
			mode = "dead"
		}
		if mode != n.Mode && n.Mode != "" {
			e.stateEvent(n.Device.Path, "mode", n.Mode, mode, behaviourSrc, cause, chainHealth(mode))
		}
		n.Mode = mode
		n.Up = mode == "online" || mode == "battery" || mode == "closed"
	}
}

// rearmBatteries sets each UPS draining (on battery, at the load it carries now), recharging
// or idle, and schedules the moment a draining battery is empty. It runs after the sources and
// their racks have settled, since the drain depends on the load.
func (e *Engine) rearmBatteries(cause uint64) {
	for _, n := range e.M.Power {
		if n.Kind == "ups" {
			e.rearm(n, cause)
		}
	}
}

func (e *Engine) rearm(n *model.PowerNode, cause uint64) {
	e.settle(n)
	e.cancel(n.Timer)
	n.Timer = 0
	switch {
	case n.Mode == "battery":
		load := e.nodeLoad(n)
		frac := 1.0
		if n.CapacityW > 0 {
			frac = math.Max(load, 0.05*n.CapacityW) / n.CapacityW
		}
		n.DrainPerS = frac / n.RuntimeS
		if e.chainLoad == nil {
			e.chainLoad = map[string]float64{}
		}
		e.chainLoad[n.Name] = load
		n.Timer = e.after(secs(n.Charge/n.DrainPerS), "ups-depleted", func() {
			n.Timer = 0
			e.settle(n)
			n.Charge = 0
			e.recomputePower(cause)
		})
	case n.Mode == "online" && n.Charge < 1 && n.RuntimeS > 0:
		n.DrainPerS = -1 / (10 * n.RuntimeS) // full again in ten times its ride-through
	default:
		n.DrainPerS = 0
	}
}

// tickChain follows the load of a UPS on battery: when what it carries changes (hosts power
// off, a rack is isolated), the battery lasts longer or shorter.
func (e *Engine) tickChain() {
	for _, n := range e.M.Power {
		if n.Kind != "ups" || n.Mode != "battery" {
			continue
		}
		was, load := e.chainLoad[n.Name], e.nodeLoad(n)
		if math.Abs(load-was) > 0.02*math.Max(was, 1) {
			e.rearm(n, 0)
		}
	}
}

// nodeLoad is what a node carries now: the output of the rack sources drawing through it and
// through the nodes below it.
func (e *Engine) nodeLoad(n *model.PowerNode) float64 {
	w := 0.0
	for _, s := range n.Sources {
		w += e.sourceOutput(s)
	}
	for _, ch := range n.Children {
		w += e.nodeLoad(ch)
	}
	return w
}

// upsRuntimeLeft is how long a UPS's battery would carry its load now, in behaviour seconds.
func (e *Engine) upsRuntimeLeft(n *model.PowerNode) float64 {
	if n.RuntimeS <= 0 {
		return 0
	}
	frac := 1.0
	if n.CapacityW > 0 {
		frac = math.Max(e.nodeLoad(n), 0.05*n.CapacityW) / n.CapacityW
	}
	return e.chargeNow(n) * n.RuntimeS / frac
}

func chainHealth(mode string) string {
	switch mode {
	case "battery", "dead":
		return model.Warning
	case "depleted", "fault", "open":
		return model.Critical
	}
	return model.OK
}

// powerChainView is the chain for /v1/facility: each node's state, battery and load against
// its capacity.
func (e *Engine) powerChainView() []any {
	out := []any{}
	for _, n := range e.M.Power {
		from := ""
		switch {
		case n.Parent != nil:
			from = n.Parent.Name
		case n.Feed != nil:
			from = n.Feed.Name
		}
		racks := []string{}
		for _, r := range n.Racks {
			racks = append(racks, r.Name)
		}
		sort.Strings(racks)
		v := map[string]any{"name": n.Name, "kind": n.Kind, "path": n.Device.Path, "from": from, "capacityW": n.CapacityW,
			"up": n.Up, "mode": n.Mode, "health": e.deviceHealth(n.Device), "loadW": round(e.nodeLoad(n)), "racks": racks, "sources": len(n.Sources)}
		if n.Feed != nil {
			v["feed"] = n.Feed.Name
		}
		if n.Kind == "ups" {
			v["runtimeS"], v["charge"], v["runtimeLeftS"] = n.RuntimeS, round(e.chargeNow(n)*100), round(e.upsRuntimeLeft(n))
		}
		out = append(out, v)
	}
	return out
}

//go:build linux

package tap

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// observer is the NOS observer (02 — Observers; D19): what a real switch decides on its own
// flows back into the twin as committed events. It reads each switch's port operational state as
// the NOS reports it — SONiC's from APPL_DB's PORT_TABLE (orchagent's record of the SAI's port
// state notifications, which `show interfaces status` prints), generic/frr-dhcrelay's from the
// switch's own namespace (netlink, as `ip link` shows it) — and its BGP sessions from FRR (`vtysh
// -c 'show bgp summary json'`, the same in both), and commits every change as a `state` event
// from source class `observer`, on the port named as the model names it. The model keeps no
// observed field for ports yet: the events and this observer's snapshot are where the values
// live.
type observer struct {
	s       *Substrate
	mu      sync.Mutex
	state   map[string]string
	detail  map[string]string
	lastBGP time.Time
}

func newObserver(s *Substrate) *observer {
	return &observer{s: s, state: map[string]string{}, detail: map[string]string{}}
}

func (o *observer) loop(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.poll(ctx)
		}
	}
}

type bgpSummary struct {
	IPv4Unicast struct {
		Peers map[string]struct {
			State    string `json:"state"`
			PfxRcd   int    `json:"pfxRcd"`
			Desc     string `json:"desc"`
			RemoteAs int    `json:"remoteAs"`
		} `json:"peers"`
	} `json:"ipv4Unicast"`
}

func (o *observer) poll(ctx context.Context) {
	for _, sw := range o.s.switches {
		host := sw.plan.Config.Hostname
		e := sw.plan.NOS
		if cmd := e.PortStateCommand(); cmd != nil {
			if o.s.docker != nil {
				cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				out, _, err := o.s.docker.Exec(cctx, sw.c.ID, cmd...)
				cancel()
				if err == nil {
					for port, st := range e.ParsePortStates(sw.plan.Config, out) {
						o.set(host, sw.plan.Device.Path+":"+port, "operState", st, "")
					}
				}
			}
		} else if links, err := sw.h.LinkList(); err == nil {
			wanted := map[string]string{} // the port's name in the switch -> the model's
			for _, p := range sw.plan.Config.Ports() {
				wanted[e.Interface(sw.plan.Config, p)] = p
			}
			for _, l := range links {
				a := l.Attrs()
				port, ok := wanted[a.Name]
				if !ok {
					continue
				}
				// the port's state as a switch's CLI gives it: admin down, or up with or without
				// carrier (a TAP's kernel operstate stays "unknown" until its carrier first changes)
				st := "down"
				switch {
				case a.Flags&net.FlagUp == 0:
					st = "admin-down"
				case a.RawFlags&unix.IFF_LOWER_UP != 0:
					st = "up"
				}
				o.set(host, sw.plan.Device.Path+":"+port, "operState", st, "")
			}
		}
		if len(sw.plan.Config.Routed) == 0 || o.s.docker == nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, _, err := o.s.docker.Exec(cctx, sw.c.ID, "vtysh", "-c", "show bgp summary json")
		cancel()
		if err != nil {
			continue
		}
		if i := strings.Index(out, "{"); i >= 0 {
			out = out[i:]
		}
		var sum bgpSummary
		if json.Unmarshal([]byte(out), &sum) != nil {
			continue
		}
		for _, r := range sw.plan.Config.Routed {
			peer := r.Peer.String()
			st, pfx := "Idle", 0
			if p, ok := sum.IPv4Unicast.Peers[peer]; ok {
				st, pfx = p.State, p.PfxRcd
			}
			o.set(host, sw.plan.Device.Path+" bgp "+peer, "bgp "+peer, st, fmt.Sprintf("AS%d %s, %d prefixes received", r.PeerASN, r.PeerName, pfx))
		}
	}
}

// set records an observed value and commits a change.
func (o *observer) set(host, key, field, value, detail string) {
	o.mu.Lock()
	old, seen := o.state[key]
	o.state[key] = value
	if detail != "" {
		o.detail[key] = detail
	}
	o.mu.Unlock()
	if seen && old == value {
		return
	}
	path := key
	if i := strings.Index(key, " bgp "); i > 0 {
		path = key[:i]
	}
	ev := schema.Event{Kind: "state", Source: schema.EventSource{Class: "observer", Principal: "nos/" + host}, Path: path,
		State: &schema.EventState{Field: field, From: old, To: value}}
	if detail != "" {
		ev.Data = map[string]any{"detail": detail}
	}
	o.s.eng.Commit(ev)
}

func (o *observer) snapshot() map[string]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make(map[string]string, len(o.state))
	for k, v := range o.state {
		out[k] = v
	}
	return out
}

// sessionsDown lists the configured BGP sessions not established.
func (o *observer) sessionsDown() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var down []string
	for _, sw := range o.s.switches {
		for _, r := range sw.plan.Config.Routed {
			key := sw.plan.Device.Path + " bgp " + r.Peer.String()
			if o.state[key] != "Established" {
				down = append(down, fmt.Sprintf("%s→%s (%s)", sw.plan.Config.Hostname, r.PeerName, orNone(o.state[key])))
			}
		}
	}
	sort.Strings(down)
	return down
}

func (o *observer) summary() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	up, ports := 0, 0
	var sessions []string
	for k, v := range o.state {
		if strings.Contains(k, " bgp ") {
			sessions = append(sessions, fmt.Sprintf("%s %s (%s)", k, v, o.detail[k]))
			continue
		}
		ports++
		if v == "up" {
			up++
		}
	}
	sort.Strings(sessions)
	lines := []string{fmt.Sprintf("NOS observer (observed, D19): %d of %d switch ports operationally up, as each NOS reports them (SONiC's APPL_DB PORT_TABLE, an FRR switch's namespace)", up, ports)}
	if len(sessions) > 0 {
		lines = append(lines, "NOS observer: BGP "+strings.Join(sessions, "; "))
	}
	return lines
}

package nvue

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// DefaultBuild is what GET /system reports as a switch's build: the Cumulus Linux release the
// emulated NVUE's schema follows (D56).
const DefaultBuild = "Cumulus Linux 5.6.0"

// operational answers the reads NVUE serves from the switch's running state, not its
// configuration: the system (its build), a VRF's BGP and its neighbours with their sessions'
// state (FRR's), a bridge domain's MAC table (the kernel's) and a port's LLDP neighbours
// (lldpd's). ok is false for any other path,
// which is configuration. NVUE's include filters (?include=/*/state) are honoured.
func (s *Server) operational(r *http.Request, p []string) (any, bool, error) {
	rev := r.URL.Query().Get("rev")
	if rev != "" && rev != "applied" && rev != "operational" {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var v any
	var err error
	switch {
	case len(p) == 1 && p[0] == "system":
		v = s.system()
	case len(p) == 4 && p[0] == "bridge" && p[1] == "domain" && p[3] == "mac-table":
		v, err = s.macTable(ctx, p[2])
	case len(p) >= 3 && len(p) <= 5 && p[0] == "interface" && p[2] == "lldp":
		var l map[string]any
		l, err = s.lldp(ctx, p[1])
		s.emit(r, Event{Op: "lldp.read", Path: "/" + strings.Join(p, "/")})
		if err == nil {
			var found bool
			if v, found = Get(l, p[3:]); !found {
				return nil, false, nil
			}
		}
	case len(p) >= 4 && len(p) <= 6 && p[0] == "vrf" && p[2] == "router" && p[3] == "bgp":
		if len(p) >= 5 && p[4] != "neighbor" {
			return nil, false, nil
		}
		var bgp map[string]any
		bgp, err = s.bgp(ctx, p[1])
		if err == nil {
			var found bool
			v, found = Get(bgp, p[4:])
			if !found {
				return nil, false, nil
			}
		}
	default:
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	if inc := r.URL.Query()["include"]; len(inc) > 0 {
		v = include(v, inc)
	}
	return v, true, nil
}

// system is GET /system: the applied system configuration with the switch's build and hostname.
func (s *Server) system() map[string]any {
	s.mu.Lock()
	out, _ := Get(s.applied, []string{"system"})
	s.mu.Unlock()
	m, _ := out.(map[string]any)
	m = Clone(m)
	m["build"] = s.build()
	if m["hostname"] == nil {
		m["hostname"] = s.sw.Hostname
	}
	// the time zone configured (date-time timezone since 5.14, timezone before), else Cumulus's
	// default (vendor facts B)
	tz := str(m, "date-time", "timezone")
	if tz == "" {
		tz = str(m, "timezone")
	}
	if tz == "" {
		tz = "Etc/UTC"
	}
	m["timezone"] = tz
	return m
}

// bgp is a VRF's BGP: its applied configuration, its neighbours' sessions as FRR reports them,
// and the counts NVUE adds.
func (s *Server) bgp(ctx context.Context, vrf string) (map[string]any, error) {
	s.mu.Lock()
	cfg, _ := Get(s.applied, []string{"vrf", vrf, "router", "bgp"})
	s.mu.Unlock()
	m, _ := cfg.(map[string]any)
	m = Clone(m)
	neighbors, _ := m["neighbor"].(map[string]any)
	if neighbors == nil {
		neighbors = map[string]any{}
	}
	established := 0
	// FRR not answering (restarting for a new AS) is no session established: the configuration
	// is still served, its neighbours idle
	var peers map[string]map[string]any
	if s.run != nil && switchOn(m) {
		if out, err := s.run(ctx, "vtysh", "-c", "show bgp vrf "+vrf+" neighbors json"); err == nil {
			_ = json.Unmarshal([]byte(out), &peers)
		}
	}
	for name, p := range peers {
		n, _ := neighbors[name].(map[string]any)
		if n == nil {
			// a neighbour FRR has that the configuration does not name (a peer group's
			// dynamic member): NVUE shows it too
			n = map[string]any{}
			neighbors[name] = n
		}
		state := strings.ToLower(fmt.Sprint(p["bgpState"]))
		n["state"] = state
		if state == "established" {
			established++
		}
		for nv, frr := range map[string]string{"remote-router-id": "remoteRouterId", "remote-hostname": "hostname", "local-ip": "hostLocal", "remote-ip": "hostForeign"} {
			if x, ok := p[frr]; ok {
				n[nv] = x
			}
		}
		if up, ok := p["bgpTimerUpMsec"].(float64); ok {
			n["uptime-msec"] = up
		}
	}
	for _, n := range neighbors {
		if nm, ok := n.(map[string]any); ok && nm["state"] == nil {
			nm["state"] = "idle"
		}
	}
	m["neighbor"] = neighbors
	m["configured-neighbors"] = float64(len(neighbors))
	m["established-neighbors"] = float64(established)
	return m, nil
}

// lldp is GET /interface/<port>/lldp: the port's LLDP neighbours as lldpd has them (`lldpcli -f
// json0 show neighbors ports <port> details`), keyed by the neighbour's system name, each with
// its chassis (chassis-id, system-name, system-description, management-address-ipv4) and port
// (name, description, type, ttl), the form `nv show interface <port> lldp neighbor -o json`
// gives on Cumulus 5.x as the twin reads NVIDIA's documentation (the key set beyond these is not
// modelled). No lldpd answering is no neighbour.
func (s *Server) lldp(ctx context.Context, port string) (map[string]any, error) {
	neighbors := map[string]any{}
	out := map[string]any{"neighbor": neighbors}
	if s.run == nil {
		return out, nil
	}
	raw, err := s.run(ctx, "lldpcli", "-f", "json0", "show", "neighbors", "ports", port, "details")
	if err != nil {
		return out, nil
	}
	var doc struct {
		LLDP []struct {
			Interface []struct {
				Name    string `json:"name"`
				Age     string `json:"age"`
				Chassis []struct {
					ID    []struct{ Type, Value string } `json:"id"`
					Name  []struct{ Value string }       `json:"name"`
					Descr []struct{ Value string }       `json:"descr"`
					Mgmt  []struct{ Value string }       `json:"mgmt-ip"`
				} `json:"chassis"`
				Port []struct {
					ID    []struct{ Type, Value string } `json:"id"`
					Descr []struct{ Value string }       `json:"descr"`
					TTL   []struct{ Value string }       `json:"ttl"`
				} `json:"port"`
			} `json:"interface"`
		} `json:"lldp"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return out, nil
	}
	first := func(vs []struct{ Value string }) string {
		if len(vs) > 0 {
			return vs[0].Value
		}
		return ""
	}
	for _, l := range doc.LLDP {
		for _, it := range l.Interface {
			if it.Name != port || len(it.Chassis) == 0 {
				continue
			}
			c := it.Chassis[0]
			name := first(c.Name)
			chassis := map[string]any{"system-name": name, "system-description": first(c.Descr)}
			if len(c.ID) > 0 {
				chassis["chassis-id"] = c.ID[0].Value
			}
			if m := first(c.Mgmt); m != "" {
				chassis["management-address-ipv4"] = m
			}
			np := map[string]any{}
			if len(it.Port) > 0 {
				pt := it.Port[0]
				if len(pt.ID) > 0 {
					np["name"], np["type"] = pt.ID[0].Value, pt.ID[0].Type
				}
				np["description"] = first(pt.Descr)
				if ttl, err := strconv.Atoi(first(pt.TTL)); err == nil {
					np["ttl"] = float64(ttl)
				}
			}
			if name == "" {
				name = fmt.Sprint(chassis["chassis-id"])
			}
			neighbors[name] = map[string]any{"chassis": chassis, "port": np}
		}
	}
	return out, nil
}

// macTable is a bridge domain's MAC table from the kernel's FDB, as NVUE numbers its entries.
func (s *Server) macTable(ctx context.Context, domain string) (map[string]any, error) {
	if domain != Bridge {
		return nil, fmt.Errorf("bridge domain %s: only %s is emulated", domain, Bridge)
	}
	out := map[string]any{}
	if s.run == nil {
		return out, nil
	}
	js, err := s.run(ctx, "bridge", "-j", "fdb", "show", "br", Bridge)
	if err != nil {
		return nil, fmt.Errorf("the bridge's FDB: %w", err)
	}
	var fdb []struct {
		MAC    string   `json:"mac"`
		Ifname string   `json:"ifname"`
		Vlan   *int     `json:"vlan"`
		State  string   `json:"state"`
		Flags  []string `json:"flags"`
		Dst    string   `json:"dst"`
	}
	if err := json.Unmarshal([]byte(js), &fdb); err != nil {
		return nil, fmt.Errorf("the bridge's FDB: %v", err)
	}
	i := 0
	for _, e := range fdb {
		typ := "dynamic"
		switch e.State {
		case "permanent":
			typ = "permanent"
		case "static":
			typ = "static"
		}
		entry := map[string]any{"mac": e.MAC, "interface": e.Ifname, "entry-type": typ}
		if e.Vlan != nil {
			entry["vlan"] = float64(*e.Vlan)
		}
		if e.Dst != "" {
			entry["remote-dst"] = e.Dst
		}
		i++
		out[strconv.Itoa(i)] = entry
	}
	return out, nil
}

// include keeps the parts of v that an include filter names: JSON pointers whose segments may
// be shell patterns (/*/state).
func include(v any, patterns []string) any {
	out := map[string]any{}
	for _, p := range patterns {
		segs := strings.Split(strings.Trim(p, "/"), "/")
		pick(v, segs, out)
	}
	return out
}

func pick(v any, segs []string, out map[string]any) {
	m, ok := v.(map[string]any)
	if !ok || len(segs) == 0 {
		return
	}
	for k, c := range m {
		if match, _ := path.Match(segs[0], k); !match {
			continue
		}
		if len(segs) == 1 {
			out[k] = c
			continue
		}
		sub, _ := out[k].(map[string]any)
		if sub == nil {
			sub = map[string]any{}
		}
		pick(c, segs[1:], sub)
		if len(sub) > 0 {
			out[k] = sub
		}
	}
}

// diff is GET ?diff=<base>&rev=<id>: what the revision changes from the base, at a path; {}
// when nothing. NICo's agent asks it before applying, and applies nothing on {}.
func (s *Server) diff(w http.ResponseWriter, r *http.Request, base string, p []string) {
	s.mu.Lock()
	tree := func(id string) (Tree, bool) {
		switch id {
		case "", "applied", "operational":
			return s.applied, true
		case "startup":
			return s.startup, true
		}
		rv := s.revs[id]
		if rv == nil {
			return nil, false
		}
		return rv.tree, true
	}
	a, okA := tree(base)
	b, okB := tree(r.URL.Query().Get("rev"))
	var va, vb any
	if okA && okB {
		va, _ = Get(Clone(a), p)
		vb, _ = Get(Clone(b), p)
	}
	s.mu.Unlock()
	if !okA || !okB {
		problem(w, http.StatusNotFound, "Not Found", "no such revision")
		return
	}
	writeJSON(w, http.StatusOK, treeDiff(va, vb))
}

// treeDiff is what b changes from a: b's values that differ, and null for what b removed, as a
// merge patch; {} when they are equal.
func treeDiff(a, b any) map[string]any {
	out := map[string]any{}
	am, _ := a.(map[string]any)
	bm, _ := b.(map[string]any)
	for k, bv := range bm {
		av, ok := am[k]
		if !ok {
			out[k] = bv
			continue
		}
		_, aObj := av.(map[string]any)
		_, bObj := bv.(map[string]any)
		if aObj && bObj {
			if d := treeDiff(av, bv); len(d) > 0 {
				out[k] = d
			}
			continue
		}
		if !reflect.DeepEqual(norm(av), norm(bv)) {
			out[k] = bv
		}
	}
	for k := range am {
		if _, ok := bm[k]; !ok {
			out[k] = nil
		}
	}
	return out
}

// norm makes a leaf comparable whatever JSON gave it (a number or its string).
func norm(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

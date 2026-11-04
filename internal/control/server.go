// Package control is the control API (docs/spec/09-runtime.md — Control API): the only way into
// the engine for the CLI, the console and scripts. It binds to loopback, requires the estate's
// token, and is on no network the software under test can reach (15); `up --allow-remote-control`
// serves it beyond loopback, for a twin behind a Kubernetes Ingress (D54).
package control

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/report"
	"github.com/BudEcosystem/dc-twin/internal/scenario"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/selector"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Info is what `up` writes to the state directory for clients.
type Info struct {
	Addr     string `json:"addr"`
	Token    string `json:"token"`
	Estate   string `json:"estate"`
	StateDir string `json:"stateDir"`
	PID      int    `json:"pid"`
	Gateway  string `json:"gateway"`
}

type Server struct {
	T      *twin.Twin
	Info   Info
	srv    *http.Server
	Static http.Handler // the console bundle, when served
	// Down is closed when a client asks the estate to come down.
	Down chan struct{}
	// Restart, when set, lets POST /v1/estate/apply bring the estate down and up again on a
	// changed file. T is swapped under mu; requests hold mu for reading.
	Restart Restarter
	mu      sync.RWMutex

	genMu      sync.Mutex
	gen        chan struct{} // closed when a restart begins: event streams end
	restarting bool
	lastApply  *ApplyResult
}

// Twin is the running twin (it changes when an apply restarts the estate).
func (s *Server) Twin() *twin.Twin {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.T
}

// Serve starts the API on addr (loopback) and writes control.json to the state directory.
func Serve(t *twin.Twin, addr string, static http.Handler) (*Server, error) {
	return ServeWith(t, addr, static, ServeOptions{})
}

// ServeOptions change how the control API is served.
type ServeOptions struct {
	// Token is the estate token, for a token the operator keeps (a Kubernetes Secret); empty
	// makes a new one.
	Token string
	// Remote lets the API listen on an address other than loopback (D54): a twin in a cluster,
	// reached through an Ingress, where the token is the only authentication.
	Remote bool
}

func ServeWith(t *twin.Twin, addr string, static http.Handler, o ServeOptions) (*Server, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	host, _, _ := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	if !o.Remote && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("the control API binds to loopback only (15); %s is not (--allow-remote-control serves it there, D54)", addr)
	}
	token := o.Token
	if token == "" {
		tok := make([]byte, 16)
		_, _ = rand.Read(tok)
		token = hex.EncodeToString(tok)
	} else if len(token) < 16 || strings.ContainsAny(token, " \t\r\n") {
		return nil, fmt.Errorf("the estate token must be at least 16 characters, without spaces")
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	// clients on this host (the CLI, a probe) reach an API on every address at its loopback
	at := l.Addr().String()
	if host == "" || (ip != nil && ip.IsUnspecified()) {
		_, port, _ := net.SplitHostPort(at)
		at = net.JoinHostPort("127.0.0.1", port)
	}
	s := &Server{T: t, Static: static, Info: Info{Addr: at, Token: token, Estate: t.Estate.Metadata.Name,
		StateDir: t.Runtime.StateDir, PID: os.Getpid(), Gateway: t.Net.Gateway}, Down: make(chan struct{}), gen: make(chan struct{})}
	b, _ := json.MarshalIndent(s.Info, "", "  ")
	if err := os.WriteFile(filepath.Join(t.Runtime.StateDir, "control.json"), b, 0o600); err != nil {
		return nil, err
	}
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go s.srv.Serve(l)
	return s, nil
}

func (s *Server) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// compressed for a client that accepts it, but the event stream, which flushes event by event
	if r.URL.Path != "/v1/events/stream" && r.Header.Get("Range") == "" && acceptsGzip(r) {
		gw := &gzipResponse{ResponseWriter: w}
		defer gw.Close()
		w = gw
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		if s.Static != nil {
			// revalidate on every load (a 304 when nothing changed), so an edited console is
			// never served stale from the browser's cache
			w.Header().Set("Cache-Control", "no-cache")
			s.Static.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.Info.Token)) != 1 {
		jsonErr(w, http.StatusUnauthorized, "a valid estate token is required")
		return
	}
	s.genMu.Lock()
	restarting, gen, last := s.restarting, s.gen, s.lastApply
	s.genMu.Unlock()
	if restarting {
		if r.URL.Path == "/v1/status" {
			writeJSON(w, map[string]any{"restarting": true, "apply": last})
			return
		}
		jsonErr(w, http.StatusServiceUnavailable, "the estate is restarting to apply a change")
		return
	}
	if r.URL.Path == "/v1/events/stream" && r.Method == http.MethodGet {
		s.stream(w, r, s.Twin(), gen, eventFilter(r.URL.Query()))
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	eng := s.T.Engine
	user := schema.EventSource{Class: "user", Principal: "control-api"}
	p := strings.TrimPrefix(r.URL.Path, "/v1")
	q := r.URL.Query()
	var body map[string]any
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		data, _ := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if strings.Contains(r.Header.Get("Content-Type"), "yaml") {
			body = map[string]any{"_raw": string(data)}
		} else {
			_ = json.Unmarshal(data, &body)
		}
		if body == nil {
			body = map[string]any{}
		}
	}
	str := func(k string) string { v, _ := body[k].(string); return v }
	switch {
	case p == "/down" && r.Method == http.MethodPost:
		writeJSON(w, map[string]any{"down": true})
		select {
		case <-s.Down:
		default:
			close(s.Down)
		}
	case p == "/status" && r.Method == http.MethodGet:
		st := eng.Status()
		st["gateway"] = s.T.Net.Gateway
		st["warnings"] = s.T.Warnings
		var adapters []string
		for _, a := range s.T.Adapters {
			adapters = append(adapters, a.Name())
		}
		st["adapters"] = adapters
		st["estateFile"] = s.T.File
		st["etag"] = fileEtag(s.T)
		st["canApply"] = s.Restart != nil && s.T.File != ""
		if last != nil {
			st["apply"] = last
		}
		writeJSON(w, st)
	case p == "/devices" && r.Method == http.MethodGet:
		views, err := eng.Select(q.Get("select"))
		if err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
		writeJSON(w, views)
	case strings.HasPrefix(p, "/devices/") && r.Method == http.MethodGet:
		path := strings.TrimPrefix(p, "/devices/")
		ds, err := eng.SelectDevices(path)
		if err != nil || len(ds) == 0 {
			jsonErr(w, 404, "no device matches "+path)
			return
		}
		writeJSON(w, eng.DeviceView(ds[0]))
	case p == "/events" && r.Method == http.MethodGet:
		since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
		filter := eventFilter(q)
		evs := eng.Events(since, filter)
		if n, _ := strconv.Atoi(q.Get("limit")); n > 0 && len(evs) > n {
			evs = evs[len(evs)-n:]
		}
		writeJSON(w, evs)
	case p == "/estate" && r.Method == http.MethodGet:
		// the file the running estate came from: as text, or with ?format=json as a tree
		b, err := os.ReadFile(s.T.File)
		if err != nil {
			jsonErr(w, 404, "the running estate has no file: "+err.Error())
			return
		}
		w.Header().Set("ETag", etagOf(b))
		if q.Get("format") == "json" {
			var tree any
			if err := schema.DecodeAny(b, &tree); err != nil {
				jsonErr(w, 500, err.Error())
				return
			}
			writeJSON(w, map[string]any{"etag": etagOf(b), "file": s.T.File, "tree": tree})
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(b)
	case (p == "/estate/plan" || p == "/estate/apply") && r.Method == http.MethodPost:
		// a draft: ops on the estate file (or, from the first API, racks to add and remove)
		var req struct {
			Etag   string    `json:"etag"`
			Ops    []Op      `json:"ops"`
			Racks  []RackAdd `json:"racks"`
			Remove []string  `json:"remove"`
			DryRun bool      `json:"dryRun"`
		}
		b, _ := json.Marshal(body)
		if err := json.Unmarshal(b, &req); err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
		ops := req.Ops
		if len(ops) == 0 {
			ops = legacyOps(req.Racks, req.Remove)
		}
		res, code := s.apply(ops, req.Etag, req.DryRun || p == "/estate/plan")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(res)
	case p == "/host" && r.Method == http.MethodGet:
		writeJSON(w, hostInfo())
	case p == "/power" && r.Method == http.MethodPost:
		ds, err := eng.SelectDevices(str("select"))
		if err != nil || len(ds) == 0 {
			jsonErr(w, 400, "select matches no device")
			return
		}
		canonical := map[string]string{"on": "power.on", "off": "power.off.force", "graceful": "power.off.graceful",
			"restart": "power.restart", "cycle": "power.cycle", "ac-cycle": "ac.cycle"}[str("action")]
		if canonical == "" {
			jsonErr(w, 400, "action: on, off, graceful, restart, cycle or ac-cycle")
			return
		}
		var errs []string
		for _, d := range ds {
			if err := eng.Power(engine.Request{Path: d.Path, Canonical: canonical, Source: user, Protocol: "control", Method: "POST", Target: "/v1/power"}); err != nil {
				errs = append(errs, d.Path+": "+err.Error())
			}
		}
		writeJSON(w, map[string]any{"devices": len(ds), "canonical": canonical, "errors": errs})
	case p == "/faults" && r.Method == http.MethodGet:
		writeJSON(w, eng.Faults(q.Get("select")))
	case p == "/faults" && r.Method == http.MethodPost:
		params, _ := body["params"].(map[string]any)
		fs, err := eng.Inject(str("select"), str("fault"), params, str("id"), user)
		if err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
		writeJSON(w, fs)
	case p == "/faults/clear" && r.Method == http.MethodPost:
		n, err := eng.Clear(str("target"), str("fault"), user)
		if err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"cleared": n})
	case p == "/chaos" && r.Method == http.MethodPost:
		rate, _ := body["rate"].(float64)
		var faults []string
		if fl, ok := body["faults"].([]any); ok {
			for _, f := range fl {
				faults = append(faults, fmt.Sprint(f))
			}
		}
		eng.SetChaos(rate, faults)
		writeJSON(w, map[string]any{"rate": rate})
	case p == "/clock/advance" && r.Method == http.MethodPost:
		d, err := time.ParseDuration(str("duration"))
		if err == nil {
			err = eng.Advance(d)
		}
		if err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"now": eng.Now()})
	case p == "/feeds" && r.Method == http.MethodPost:
		up, _ := body["up"].(bool)
		if err := eng.SetFeed(str("feed"), up, user); err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"feed": str("feed"), "up": up})
	case p == "/cable" && r.Method == http.MethodPost:
		if err := eng.SetLink(str("link"), str("state") == "replace", user); err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"link": str("link"), "state": str("state")})
	case p == "/reseat" && r.Method == http.MethodPost:
		ds, _ := eng.SelectDevices(str("select"))
		for _, d := range ds {
			_ = eng.Reseat(d.Path, user)
		}
		writeJSON(w, map[string]any{"devices": len(ds)})
	case p == "/bmc/reset" && r.Method == http.MethodPost:
		// a technician's BMC reset: the management controller reboots, the host keeps running
		ds, err := eng.SelectDevices(str("select"))
		if err != nil || len(ds) == 0 {
			jsonErr(w, 400, "select matches no device")
			return
		}
		var errs []string
		n := 0
		for _, d := range ds {
			if !d.HasBMC() {
				continue
			}
			n++
			if err := eng.ResetBMC(engine.Request{Path: d.Path, Canonical: "bmc.reset", Source: user, Protocol: "control", Method: "POST", Target: "/v1/bmc/reset"}); err != nil {
				errs = append(errs, d.Path+": "+err.Error())
			}
		}
		writeJSON(w, map[string]any{"devices": n, "errors": errs})
	case p == "/utilisation" && r.Method == http.MethodPost:
		v, _ := body["value"].(float64)
		ds, _ := eng.SelectDevices(str("select"))
		for _, d := range ds {
			_ = eng.SetUtilisation(d.Path, v, user)
		}
		writeJSON(w, map[string]any{"devices": len(ds), "value": v})
	case p == "/layout" && r.Method == http.MethodGet:
		writeJSON(w, s.layout())
	case p == "/links" && r.Method == http.MethodGet:
		links, err := eng.Links(q.Get("select"))
		if err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
		writeJSON(w, links)
	case p == "/facility" && r.Method == http.MethodGet:
		writeJSON(w, eng.Facility())
	case p == "/domains" && r.Method == http.MethodGet:
		writeJSON(w, eng.Domains())
	case p == "/endpoints" && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{"gateway": s.T.Net.Gateway, "endpoints": s.T.Net.Endpoints, "routes": s.T.Net.Routes()})
	case p == "/report" && r.Method == http.MethodGet:
		rep := report.Build(s.T, nil)
		if q.Get("format") == "text" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			io.WriteString(w, rep.Text())
			return
		}
		writeJSON(w, rep)
	case p == "/scenarios/run" && r.Method == http.MethodPost:
		raw, _ := body["_raw"].(string)
		var sc schema.Scenario
		if err := schema.Decode([]byte(raw), &sc); err != nil {
			jsonErr(w, 400, err.Error())
			return
		}
		res := scenario.Run(r.Context(), s.T, &sc, scenario.Options{})
		writeJSON(w, res)
	case p == "/catalogue" && r.Method == http.MethodGet:
		if q.Get("detail") == "full" {
			writeJSON(w, s.catalogueFull())
			return
		}
		writeJSON(w, s.catalogue())
	case p == "/credentials" && r.Method == http.MethodGet:
		// the BMC credentials of devices, for the operator's own tools (redacted from events): the
		// account that logs in now, which a stack may have changed (NICo rotates every BMC's
		// password), and the factory account when it differs (what a site's inventory holds)
		ds, _ := eng.SelectDevices(q.Get("select"))
		out := []map[string]string{}
		eng.Read(func(*model.Model) {
			for _, d := range ds {
				if !d.HasBMC() {
					continue
				}
				user, pass := currentAccount(d)
				c := map[string]string{"path": d.Path, "user": user, "password": pass, "redfish": d.Endpoint.ListenTLS, "siteIP": d.Endpoint.SiteIP}
				if user != d.Identity.BMCUser || pass != d.Identity.BMCPassword {
					c["factoryUser"], c["factoryPassword"] = d.Identity.BMCUser, d.Identity.BMCPassword
				}
				out = append(out, c)
			}
		})
		writeJSON(w, out)
	case p == "/inspect/redfish" && r.Method == http.MethodGet:
		s.inspectRedfish(w, q.Get("device"), q.Get("path"))
	case p == "/console" && r.Method == http.MethodGet:
		// a device's console: a vm's serial port, the stand-in for Serial over LAN (05)
		ds, err := eng.SelectDevices(q.Get("device"))
		if err != nil || len(ds) != 1 {
			jsonErr(w, 400, "device must select exactly one device")
			return
		}
		from, _ := strconv.ParseInt(q.Get("from"), 10, 64)
		for _, a := range s.T.Adapters {
			if c, ok := a.(adapter.Console); ok {
				if b, next, err := c.ConsoleLog(ds[0].Path, from); err == nil {
					w.Header().Set("Content-Type", "text/plain; charset=utf-8")
					w.Header().Set("X-Console-Next", strconv.FormatInt(next, 10))
					w.Write(b)
					return
				}
			}
		}
		jsonErr(w, 404, ds[0].Path+" has no console here: only vm devices have one")
	default:
		jsonErr(w, 404, "no such operation: "+r.Method+" "+r.URL.Path)
	}
}

func eventFilter(q map[string][]string) func(schema.Event) bool {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	var sel *selector.Selector
	if s := get("select"); s != "" {
		sel, _ = selector.Parse(s)
	}
	kind, source, canonical := get("kind"), get("source"), get("canonical")
	hideReads := get("reads") == "false"
	return func(e schema.Event) bool {
		if sel != nil && !sel.MatchPath(e.Path) && !sel.MatchPort(e.Path) {
			return false
		}
		if kind != "" && e.Kind != kind {
			return false
		}
		if source != "" && e.Source.Class != source {
			return false
		}
		if canonical != "" && (e.Request == nil || e.Request.Canonical != canonical) {
			return false
		}
		if hideReads && e.Request != nil && strings.HasSuffix(e.Request.Canonical, ".read") {
			return false
		}
		return true
	}
}

// streamKeepAlive is how often a quiet event stream sends a comment line.
var streamKeepAlive = 20 * time.Second

// stream sends the twin's events as server-sent events until the client goes, or until an apply
// restarts the estate (the client reconnects to the new one).
func (s *Server) stream(w http.ResponseWriter, r *http.Request, t *twin.Twin, gen chan struct{}, filter func(schema.Event) bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		jsonErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // a proxy in front (an Ingress) passes each event on as it comes
	id, ch := t.Engine.Subscribe(4096)
	defer t.Engine.Unsubscribe(id)
	fl.Flush()
	// a comment line now and then keeps a quiet stream open through a proxy's idle timeout
	keep := time.NewTicker(streamKeepAlive)
	defer keep.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-gen:
			return
		case <-keep.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			fl.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if !filter(ev) {
				continue
			}
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, b)
			fl.Flush()
		}
	}
}

// layout is what the console draws from (12, D18): halls, rows with their positions, racks with
// their format's height, and every device's slot, U and state.
func (s *Server) layout() map[string]any {
	var out map[string]any
	s.T.Engine.Read(func(m *model.Model) { out = layoutOf(m, s.T.Estate.Metadata.Name) })
	return out
}

func (s *Server) catalogue() map[string]any {
	reg := s.T.Reg
	profiles, formats, faults, dialects := []any{}, []any{}, []any{}, []any{}
	for n, p := range reg.Profiles {
		profiles = append(profiles, map[string]any{"name": n, "class": p.Spec.Class, "units": p.Spec.Form.Units})
	}
	for n, f := range reg.Formats {
		formats = append(formats, map[string]any{"name": n, "archetype": f.Spec.Archetype, "params": f.Spec.Params})
	}
	for n, f := range reg.Faults {
		faults = append(faults, map[string]any{"id": n, "title": f.Spec.Title, "target": f.Spec.Target, "params": f.Spec.Params, "clears": f.Spec.Clears})
	}
	for n, d := range reg.Redfish {
		dialects = append(dialects, map[string]any{"name": n, "backend": d.Spec.Backend, "mockup": d.Spec.Mockup})
	}
	return map[string]any{"profiles": profiles, "formats": formats, "faults": faults, "dialects": dialects}
}

// inspectRedfish reads a device's Redfish resource as the stack sees it, through the adapter.
func (s *Server) inspectRedfish(w http.ResponseWriter, device, path string) {
	ds, _ := s.T.Engine.SelectDevices(device)
	if len(ds) == 0 || !ds[0].HasBMC() {
		jsonErr(w, 404, "no BMC device matches "+device)
		return
	}
	d := ds[0]
	if path == "" {
		path = "/redfish/v1"
	}
	// render in-process, as the scenario runner observes: the console's reads are not requests
	// from the software under test and must not appear in the device's request log
	for _, a := range s.T.Adapters {
		if g, ok := a.(interface {
			Get(devicePath, resourcePath string) (map[string]any, bool)
		}); ok {
			doc, found := g.Get(d.Path, path)
			if !found || doc == nil {
				jsonErr(w, 404, "no resource "+path+" on "+d.Path)
				return
			}
			writeJSON(w, doc)
			return
		}
	}
	jsonErr(w, 503, "no Redfish adapter is running")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// currentAccount is the BMC account that logs in now: the factory user's, as it stands (a stack
// may have changed its password), else the first enabled administrator, else the factory account.
func currentAccount(d *model.Device) (user, pass string) {
	var admin *model.Account
	for i := range d.State.Accounts {
		a := &d.State.Accounts[i]
		if !a.Enabled || a.Locked || a.User == "" {
			continue
		}
		if a.User == d.Identity.BMCUser {
			return a.User, a.Password
		}
		if admin == nil && a.Role == "Administrator" {
			admin = a
		}
	}
	if admin != nil {
		return admin.User, admin.Password
	}
	return d.Identity.BMCUser, d.Identity.BMCPassword
}

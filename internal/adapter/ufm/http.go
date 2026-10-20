package ufm

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/adapter/ibsim/smstate"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// The two REST surfaces NICo uses. /ufmRestV2 (session cookies) is not served.
const (
	basePath = "/ufmRest"
	v3Path   = "/ufmRestV3"
)

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ServeHTTP authenticates per surface, routes, and logs every request with the engine by its
// canonical name.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	prefix, rest := splitPrefix(r.URL.Path)
	user := ""
	defer func() {
		s.a.rt.Engine.LogRead(engine.Request{Path: s.dev.Path, Canonical: canonical(r.Method, rest), Protocol: Protocol,
			Method: r.Method, Target: r.URL.RequestURI(),
			Source: schema.EventSource{Class: "stack", Principal: user, Address: r.RemoteAddr}}, rec.status)
	}()
	var on bool
	s.a.rt.Engine.Read(func(*model.Model) { on = s.dev.State.AC && s.dev.State.Power == model.On })
	if !on {
		// UFM runs on the appliance: with it off nothing answers on its address.
		rec.status = 0
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				return
			}
		}
		rec.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if prefix == "" {
		writeError(rec, http.StatusNotFound, "not found")
		return
	}
	u, ok := s.authenticate(prefix, r)
	if !ok {
		rec.Header().Set("WWW-Authenticate", `Basic realm="UFM"`)
		writeError(rec, http.StatusUnauthorized, "invalid UFM authorization")
		return
	}
	user = u
	s.route(rec, r, rest, user)
}

// splitPrefix separates the surface ("/ufmRest" or "/ufmRestV3") from the resource path,
// which is returned without leading or trailing slashes.
func splitPrefix(p string) (string, string) {
	seg, rest, _ := strings.Cut(strings.TrimPrefix(p, "/"), "/")
	switch "/" + seg {
	case basePath, v3Path:
		return "/" + seg, strings.Trim(rest, "/")
	}
	return "", ""
}

// authenticate: /ufmRest takes Basic user:password (and, as UFM's own examples show, an
// access token); /ufmRestV3 takes only an access token, presented as "Basic <token>", which is
// what NICo's client sends.
func (s *server) authenticate(prefix string, r *http.Request) (string, bool) {
	scheme, value, _ := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !strings.EqualFold(scheme, "Basic") {
		return "", false
	}
	value = strings.TrimSpace(value)
	if prefix == basePath {
		if raw, err := base64.StdEncoding.DecodeString(value); err == nil {
			if u, p, ok := strings.Cut(string(raw), ":"); ok && u == s.user && p == s.password {
				return u, true
			}
		}
	}
	return s.tokenUser(value)
}

// canonical maps a UFM request onto the canonical request registry (14): reads of the
// fabric's ports, partitions and SM configuration are network.read, of the UFM and its
// systems inventory.read; tokens are sessions; partition writes have no canonical name and are
// logged as other.
func canonical(method, rest string) string {
	switch {
	case rest == "app/tokens" && method == http.MethodPost:
		return "session.create"
	case rest == "app/tokens/revoke":
		return "session.delete"
	case rest == "app/tokens":
		return "account.read"
	case method != http.MethodGet:
		return "other"
	case rest == "app/ufm_version", strings.HasPrefix(rest, "resources/systems"):
		return "inventory.read"
	case rest == "app/smconf", strings.HasPrefix(rest, "resources/ports"), strings.HasPrefix(rest, "resources/pkeys"):
		return "network.read"
	}
	return "other"
}

func (s *server) route(w http.ResponseWriter, r *http.Request, rest, user string) {
	parts := strings.Split(rest, "/")
	m := r.Method
	switch {
	case rest == "app/ufm_version" && m == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"ufm_release_version": s.a.cfg.Version})
	case rest == "app/smconf" && m == http.MethodGet:
		writeJSON(w, http.StatusOK, s.sm)
	case rest == "app/tokens" && m == http.MethodGet:
		s.listTokens(w, user)
	case rest == "app/tokens" && m == http.MethodPost:
		s.createToken(w, user)
	case rest == "app/tokens/revoke" && m == http.MethodPost:
		s.revokeToken(w, r)
	case rest == "resources/ports" && m == http.MethodGet:
		s.ports(w, r, nil)
	case len(parts) == 3 && parts[0] == "resources" && parts[1] == "ports" && m == http.MethodGet:
		s.ports(w, r, strings.Split(parts[2], ","))
	case rest == "resources/systems" && m == http.MethodGet:
		s.systems(w, r, nil)
	case len(parts) == 3 && parts[0] == "resources" && parts[1] == "systems" && m == http.MethodGet:
		s.systems(w, r, strings.Split(parts[2], ","))
	case rest == "resources/pkeys" && m == http.MethodGet:
		s.listPKeys(w, r)
	case rest == "resources/pkeys" && m == http.MethodPost:
		s.addGUIDs(w, r, false)
	case rest == "resources/pkeys" && m == http.MethodPut:
		s.addGUIDs(w, r, true)
	case rest == "resources/pkeys/add" && m == http.MethodPost:
		s.createPKey(w, r)
	case rest == "resources/pkeys/last_updated" && m == http.MethodGet:
		s.lastUpdated(w)
	case rest == "resources/pkeys/qos_conf" && m == http.MethodPut:
		s.updateQoS(w, r)
	case len(parts) == 3 && parts[0] == "resources" && parts[1] == "pkeys" && m == http.MethodGet:
		s.getPKey(w, r, parts[2])
	case len(parts) == 3 && parts[0] == "resources" && parts[1] == "pkeys" && m == http.MethodDelete:
		s.deletePKey(w, parts[2])
	case len(parts) == 5 && parts[0] == "resources" && parts[1] == "pkeys" && parts[3] == "guids" && m == http.MethodDelete:
		s.removeGUIDs(w, parts[2], strings.Split(parts[4], ","))
	case rest == "actions/remove_guids_from_pkey" && m == http.MethodPost:
		var b struct {
			PKey  any      `json:"pkey"`
			GUIDs []string `json:"guids"`
		}
		if err := decode(r, &b); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		key, err := parsePKey(b.PKey)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.removeGUIDs(w, pkeyString(key), b.GUIDs)
	default:
		writeError(w, http.StatusNotFound, "no such UFM resource: "+r.Method+" /"+rest)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	b, _ := json.Marshal(map[string]string{"error": msg})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// writeOK is the answer to a write: 200 with an empty object, as UFM sends.
func writeOK(w http.ResponseWriter) { writeJSON(w, http.StatusOK, map[string]any{}) }

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}
	return nil
}

func boolParam(r *http.Request, k string) bool {
	return strings.EqualFold(r.URL.Query().Get(k), "true")
}

// ---- tokens

func (s *server) listTokens(w http.ResponseWriter, user string) {
	s.mu.Lock()
	out := []token{}
	for _, t := range s.st.Tokens {
		if t.Username == user {
			x := *t
			x.TwinIssued = false
			out = append(out, x)
		}
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *server) createToken(w http.ResponseWriter, user string) {
	s.mu.Lock()
	t := *s.ensureToken(newToken(), user, false)
	_ = s.saveLocked()
	s.mu.Unlock()
	t.TwinIssued = false
	writeJSON(w, http.StatusOK, t)
}

// revokeToken takes the token as a form field (UFM documents multipart/form-data).
func (s *server) revokeToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseMultipartForm(1 << 20)
	tok := r.FormValue("token")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.st.Tokens {
		if t.AccessToken == tok && tok != "" {
			t.Revoked = true
			_ = s.saveLocked()
			writeOK(w)
			return
		}
	}
	writeError(w, http.StatusNotFound, "token not found")
}

// ---- ports and systems

// lidTable is the LID of every port (a switch's is its port 0's). When the fabric runs under
// ibsim and a subnet manager has assigned LIDs in it, they are the SM's, as a UFM reports what
// its embedded OpenSM assigned (D47): read from ibsim through smstate, a port the SM has not
// reached reporting 0. Otherwise the twin assigns them from the topology and keeps them as
// OpenSM keeps guid2lid.
func (s *server) lidTable(sn *snap) map[string]int {
	if src := smstate.Lookup(s.a.rt.Engine); src != nil {
		if ports, ok := src.Ports(s.fabric); ok {
			out := make(map[string]int, len(sn.ports))
			for _, ip := range sn.ports {
				k := lidKey(ip)
				out[k] = ports[k].LID
			}
			s.mu.Lock()
			s.smLIDs = true
			s.mu.Unlock()
			return out
		}
	}
	s.mu.Lock()
	s.smLIDs = false
	s.mu.Unlock()
	keys := make([]string, 0, len(sn.ports))
	for _, ip := range sn.ports {
		keys = append(keys, lidKey(ip))
	}
	return s.lids(keys)
}

func (s *server) health(sn *snap) map[*model.Device]string {
	out := map[*model.Device]string{}
	for _, sy := range sn.systems {
		out[sy.dev] = s.a.rt.Engine.Health(sy.dev)
	}
	return out
}

// ports serves /resources/ports with the filters NICo and UFM's guide use: sys_type
// (Computer, Switch), system (a system name or GUID), active=true; or the named ports.
func (s *server) ports(w http.ResponseWriter, r *http.Request, names []string) {
	sn := s.snapshot()
	st := s.states(sn)
	lid := s.lidTable(sn)
	health := s.health(sn)
	q := r.URL.Query()
	sysType, sysName, active := q.Get("sys_type"), q.Get("system"), boolParam(r, "active")
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := []portJSON{}
	for _, ip := range sn.ports {
		if len(names) > 0 && !want[ip.name] {
			continue
		}
		delete(want, ip.name)
		if sysType != "" && !strings.EqualFold(sysType, map[bool]string{true: "Switch", false: "Computer"}[ip.sw]) {
			continue
		}
		if sysName != "" && sysName != ip.sys.hostname && normGUID(sysName) != ip.sys.guid {
			continue
		}
		if active && !st[ip].up {
			continue
		}
		out = append(out, s.renderPort(ip, st[ip], lid, health, sn))
	}
	if len(want) > 0 {
		missing := make([]string, 0, len(want))
		for n := range want {
			missing = append(missing, n)
		}
		sort.Strings(missing)
		writeError(w, http.StatusNotFound, "no such port: "+strings.Join(missing, ","))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// systems serves /resources/systems with the type, ip, model and role filters, or the named
// systems.
func (s *server) systems(w http.ResponseWriter, r *http.Request, names []string) {
	sn := s.snapshot()
	st := s.states(sn)
	health := s.health(sn)
	q := r.URL.Query()
	out := []systemJSON{}
	var missing []string
	if len(names) > 0 {
		for _, n := range names {
			sy := sn.sysBy[n]
			if sy == nil {
				sy = sn.sysBy[normGUID(n)]
			}
			if sy == nil {
				missing = append(missing, n)
				continue
			}
			out = append(out, s.renderSystem(sy, st, health))
		}
		if len(missing) > 0 {
			writeError(w, http.StatusNotFound, "no such system: "+strings.Join(missing, ","))
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	for _, sy := range sn.systems {
		sj := s.renderSystem(sy, st, health)
		if t := q.Get("type"); t != "" && !strings.EqualFold(t, sj.Type) {
			continue
		}
		if v := q.Get("ip"); v != "" && v != sj.IP {
			continue
		}
		if v := q.Get("model"); v != "" && v != sj.Model {
			continue
		}
		if v := q.Get("role"); v != "" && !strings.EqualFold(v, sj.Role) {
			continue
		}
		out = append(out, sj)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- partition keys

// listPKeys: without guids_data or qos_conf UFM answers a bare list of pkeys (the "different
// structure" NICo's client avoids); with either, an object keyed by pkey.
func (s *server) listPKeys(w http.ResponseWriter, r *http.Request) {
	guids, withQoS := boolParam(r, "guids_data"), boolParam(r, "qos_conf")
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]uint16, 0, len(s.st.Partitions))
	for k := range s.st.Partitions {
		n, _ := parsePKey(k)
		keys = append(keys, n)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	if !guids && !withQoS {
		list := make([]string, 0, len(keys))
		for _, k := range keys {
			list = append(list, pkeyString(k))
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	out := make(map[string]partitionData, len(keys))
	for _, k := range keys {
		out[pkeyString(k)] = s.render(k, s.st.Partitions[pkeyString(k)], guids, withQoS)
	}
	writeJSON(w, http.StatusOK, out)
}

// getPKey answers an unknown pkey with 200 and {}, as UFM does (NICo's client maps that body
// to not-found).
func (s *server) getPKey(w http.ResponseWriter, r *http.Request, raw string) {
	key, err := parsePKey(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.st.Partitions[pkeyString(key)]
	if p == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, s.render(key, p, boolParam(r, "guids_data"), boolParam(r, "qos_conf")))
}

type pkeyBody struct {
	PKey         any      `json:"pkey"`
	GUIDs        []string `json:"guids"`
	IPoverIB     *bool    `json:"ip_over_ib"`
	Index0       *bool    `json:"index0"`
	Membership   *string  `json:"membership"`
	Memberships  []string `json:"memberships"`
	MTULimit     *uint16  `json:"mtu_limit"`
	ServiceLevel *uint8   `json:"service_level"`
	RateLimit    *float64 `json:"rate_limit"`
}

func (b *pkeyBody) qos(base qos) (qos, error) {
	q := base
	if b.MTULimit != nil {
		q.MTULimit = *b.MTULimit
	}
	if b.ServiceLevel != nil {
		q.ServiceLevel = *b.ServiceLevel
	}
	if b.RateLimit != nil {
		q.RateLimit = *b.RateLimit
	}
	return q, q.validate()
}

// checkGUIDs normalises GUIDs and refuses any that is malformed or not a port of the fabric.
func (s *server) checkGUIDs(guids []string) ([]string, error) {
	sn := s.snapshot()
	out := make([]string, 0, len(guids))
	for _, g := range guids {
		n := normGUID(g)
		if !validGUID(n) {
			return nil, fmt.Errorf("invalid GUID %q: 16 to 20 hex digits", g)
		}
		if sn.byGUID[n] == nil {
			return nil, fmt.Errorf("InfiniBand port %s does not exist", n)
		}
		out = append(out, n)
	}
	return out, nil
}

// addGUIDs is POST /resources/pkeys (add GUIDs, creating the pkey named api_pkey_<pkey> when it
// does not exist: the call NICo's bind makes) and PUT /resources/pkeys (set: the list replaces
// the pkey's GUIDs).
func (s *server) addGUIDs(w http.ResponseWriter, r *http.Request, set bool) {
	var b pkeyBody
	if err := decode(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key, err := parsePKey(b.PKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if b.GUIDs == nil {
		writeError(w, http.StatusBadRequest, "guids is required")
		return
	}
	if b.Memberships != nil && b.Membership != nil {
		writeError(w, http.StatusBadRequest, "membership and memberships are exclusive")
		return
	}
	if b.Memberships != nil && len(b.Memberships) != len(b.GUIDs) {
		writeError(w, http.StatusBadRequest, "memberships must name one membership per GUID")
		return
	}
	membership := "full"
	if b.Membership != nil {
		membership = *b.Membership
	}
	for _, m := range append([]string{membership}, b.Memberships...) {
		if m != "full" && m != "limited" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("membership %q: full or limited", m))
			return
		}
	}
	guids, err := s.checkGUIDs(b.GUIDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ks := pkeyString(key)
	p := s.st.Partitions[ks]
	if p == nil {
		p = &partition{Name: "api_pkey_" + ks, IPoIB: true, QoS: defaultQoS, Members: map[string]*member{}}
	}
	q, err := b.qos(p.QoS)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.QoS = q
	if b.IPoverIB != nil {
		p.IPoIB = *b.IPoverIB
	}
	index0 := b.Index0 != nil && *b.Index0
	if set {
		p.Members = map[string]*member{}
	}
	for i, g := range guids {
		m := membership
		if b.Memberships != nil {
			m = b.Memberships[i]
		}
		p.Members[g] = &member{Membership: m, Index0: index0}
	}
	s.st.Partitions[ks] = p
	s.touchLocked()
	writeOK(w)
}

// createPKey is POST /resources/pkeys/add: an empty pkey with its attributes.
func (s *server) createPKey(w http.ResponseWriter, r *http.Request) {
	var b pkeyBody
	if err := decode(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key, err := parsePKey(b.PKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ks := pkeyString(key)
	p := s.st.Partitions[ks]
	if p == nil {
		p = &partition{Name: "api_pkey_" + ks, IPoIB: true, QoS: defaultQoS, Members: map[string]*member{}}
	}
	q, err := b.qos(p.QoS)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.QoS = q
	if b.IPoverIB != nil {
		p.IPoIB = *b.IPoverIB
	}
	if b.Index0 != nil {
		p.Index0 = *b.Index0
	}
	s.st.Partitions[ks] = p
	s.touchLocked()
	writeOK(w)
}

// removeGUIDs removes GUIDs from a pkey; a pkey other than the default one that has no GUID
// left is removed, so a following GET answers {} (the not-found NICo's partition deletion
// waits for).
func (s *server) removeGUIDs(w http.ResponseWriter, raw string, guids []string) {
	key, err := parsePKey(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ks := pkeyString(key)
	p := s.st.Partitions[ks]
	if p == nil {
		writeError(w, http.StatusBadRequest, "partition "+ks+" does not exist")
		return
	}
	for _, g := range guids {
		delete(p.Members, normGUID(g))
	}
	if key != DefaultPKey && len(p.Members) == 0 {
		delete(s.st.Partitions, ks)
	}
	s.touchLocked()
	writeOK(w)
}

func (s *server) deletePKey(w http.ResponseWriter, raw string) {
	key, err := parsePKey(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if key == DefaultPKey {
		writeError(w, http.StatusBadRequest, "the management partition cannot be deleted")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ks := pkeyString(key)
	if s.st.Partitions[ks] == nil {
		writeError(w, http.StatusNotFound, "partition "+ks+" does not exist")
		return
	}
	delete(s.st.Partitions, ks)
	s.touchLocked()
	writeOK(w)
}

// updateQoS is PUT /resources/pkeys/qos_conf.
func (s *server) updateQoS(w http.ResponseWriter, r *http.Request) {
	var b pkeyBody
	if err := decode(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	key, err := parsePKey(b.PKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if b.MTULimit == nil || b.ServiceLevel == nil || b.RateLimit == nil {
		writeError(w, http.StatusBadRequest, "mtu_limit, service_level and rate_limit are required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.st.Partitions[pkeyString(key)]
	if p == nil {
		writeError(w, http.StatusBadRequest, "partition "+pkeyString(key)+" does not exist")
		return
	}
	q, err := b.qos(p.QoS)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.QoS = q
	s.touchLocked()
	writeOK(w)
}

func (s *server) lastUpdated(w http.ResponseWriter) {
	s.mu.Lock()
	v := s.st.LastUpdated
	s.mu.Unlock()
	if v == "" {
		writeJSON(w, http.StatusOK, map[string]any{"last_updated": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"last_updated": v})
}

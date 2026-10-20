package redfish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// ServeHTTP is one BMC's Redfish service.
func (b *bmc) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	up, latency, errPct := b.a.eng.BMCBehaviour(b.dev)
	if !up {
		// an unavailable BMC does not answer: drop the connection as a hung BMC does
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				return
			}
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if latency > 0 {
		time.Sleep(latency)
	}
	path := normalise(r.URL.Path)
	if errPct > 0 && rand.Intn(100) < errPct {
		b.logRead(r, path, http.StatusInternalServerError)
		writeError(w, http.StatusInternalServerError, "Base.1.8.InternalError", "The request failed due to an internal service error.")
		return
	}
	w.Header().Set("OData-Version", "4.0")
	w.Header().Set("Server", "dc-twin")
	user, ok := b.authenticate(w, r, path)
	if !ok {
		return
	}
	src := schema.EventSource{Class: "stack", Principal: user, Address: r.RemoteAddr}
	// reads are logged with the user too (found by compat/maas: only writes named it)
	r = r.WithContext(context.WithValue(r.Context(), userKey{}, user))
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		b.get(w, r, path)
	case http.MethodPost:
		b.post(w, r, path, src)
	case http.MethodPatch, http.MethodPut:
		b.patch(w, r, path, src)
	case http.MethodDelete:
		b.delete(w, r, path, src)
	default:
		writeError(w, http.StatusMethodNotAllowed, "Base.1.8.OperationNotAllowed", "The HTTP method is not allowed on this resource.")
	}
}

func normalise(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "/"
	}
	return p
}

var anonymous = map[string]bool{"/redfish": true, "/redfish/v1": true, "/redfish/v1/$metadata": true, "/redfish/v1/odata": true}

func (b *bmc) authenticate(w http.ResponseWriter, r *http.Request, path string) (string, bool) {
	if anonymous[path] && r.Method == http.MethodGet {
		return "anonymous", true
	}
	if path == "/redfish/v1/SessionService/Sessions" && r.Method == http.MethodPost {
		return "login", true
	}
	if tok := r.Header.Get("X-Auth-Token"); tok != "" {
		if s, ok := b.a.eng.SessionByToken(b.dev, tok); ok {
			return s.User, true
		}
	}
	if u, p, ok := r.BasicAuth(); ok {
		if a, ok := b.a.eng.Authenticate(b.dev, u, p); ok {
			if a.PasswordChangeRequired && !(r.Method == http.MethodPatch && strings.HasPrefix(path, "/redfish/v1/AccountService/Accounts/")) {
				writePasswordChange(w, a.ID)
				return "", false
			}
			return u, true
		}
	}
	if b.a.authOptional {
		// oracle mode: an unauthenticated request acts as the factory account, so a factory
		// password that must be changed still answers 403 PasswordChangeRequired
		if a, ok := b.a.eng.Authenticate(b.dev, b.dev.Identity.BMCUser, b.dev.Identity.BMCPassword); ok && a.PasswordChangeRequired &&
			!(r.Method == http.MethodPatch && strings.HasPrefix(path, "/redfish/v1/AccountService/Accounts/")) {
			writePasswordChange(w, a.ID)
			return "", false
		}
		return "anonymous", true
	}
	b.logRead(r, path, http.StatusUnauthorized)
	w.Header().Set("WWW-Authenticate", `Basic realm="Redfish"`)
	writeError(w, http.StatusUnauthorized, "Base.1.8.NoValidSession", "There is no valid session established with the implementation.")
	return "", false
}

// writePasswordChange answers as DMTF's Base registry says a BMC must when an account's
// password has to be changed first: 403 with PasswordChangeRequired naming the account.
func writePasswordChange(w http.ResponseWriter, accountID string) {
	uri := "/redfish/v1/AccountService/Accounts/" + accountID
	msg := "The password provided for this account must be changed before access is granted. PATCH the Password property for this account located at the target URI '" + uri + "' to complete this process."
	writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": "Base.1.8.PasswordChangeRequired", "message": msg,
		"@Message.ExtendedInfo": []any{map[string]any{"@odata.type": "#Message.v1_1_1.Message", "MessageId": "Base.1.8.PasswordChangeRequired",
			"Message": msg, "MessageArgs": []any{uri}, "Severity": "Critical", "Resolution": "Change the password for this account using a PATCH to the Password property at the URI provided."}}}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, id, msg string) { writeErrorMsg(w, status, id, msg) }

func writeErrorMsg(w http.ResponseWriter, status int, id, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": id, "message": msg,
		"@Message.ExtendedInfo": []any{map[string]any{"@odata.type": "#Message.v1_1_1.Message", "MessageId": id, "Message": msg, "Severity": "Critical", "Resolution": "None."}},
	}})
}

func (b *bmc) request(r *http.Request, path, canonical string, src schema.EventSource) engine.Request {
	return engine.Request{Path: b.dev.Path, Canonical: canonical, Source: src, Protocol: "redfish", Method: r.Method, Target: path}
}

// userKey carries the authenticated user of a request to the read log.
type userKey struct{}

func (b *bmc) logRead(r *http.Request, path string, status int) {
	user, _ := r.Context().Value(userKey{}).(string)
	b.a.eng.LogRead(engine.Request{Path: b.dev.Path, Canonical: b.canonical(r.Method, path), Source: schema.EventSource{Class: "stack", Principal: user, Address: r.RemoteAddr},
		Protocol: "redfish", Method: r.Method, Target: path}, status)
}

// ---------------------------------------------------------------- GET

func (b *bmc) get(w http.ResponseWriter, r *http.Request, path string) {
	if path == "/redfish" {
		b.logRead(r, path, 200)
		writeJSON(w, 200, map[string]any{"v1": "/redfish/v1/"})
		return
	}
	if sse := b.sseURI(); sse != "" && path == sse {
		b.serveSSE(w, r)
		return
	}
	if x, ok := b.xmlDoc(path); ok {
		b.logRead(r, path, 200)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(200)
		w.Write(x)
		return
	}
	rd := &renderer{b: b, snap: b.snapshot()}
	doc, ok := b.backend.render(rd, path)
	if !ok || doc == nil {
		b.logRead(r, path, 404)
		writeError(w, http.StatusNotFound, "Base.1.8.ResourceNotFound", "The requested resource of type unknown named "+path+" was not found.")
		return
	}
	b.logRead(r, path, 200)
	if r.Method == http.MethodHead {
		w.WriteHeader(200)
		return
	}
	if ex := r.URL.Query().Get("$expand"); ex != "" {
		b.expandQuery(rd, doc, ex)
	}
	w.Header().Set("ETag", fmt.Sprintf(`W/"%d"`, b.a.eng.Seq()))
	writeJSON(w, 200, doc)
}

// expand implements $expand (".", "*", "~", with $levels): references that are only an
// @odata.id are replaced by the resource; "." skips Links, "~" expands only Links.
func (b *bmc) expandQuery(rd *renderer, doc map[string]any, q string) {
	typ, levels := q[:1], 1
	if i := strings.Index(q, "$levels="); i >= 0 {
		fmt.Sscanf(q[i+len("$levels="):], "%d", &levels)
	}
	if typ != "." && typ != "*" && typ != "~" {
		return
	}
	only := func(v any) (string, bool) {
		m, ok := v.(map[string]any)
		if !ok || len(m) != 1 {
			return "", false
		}
		id, ok := m["@odata.id"].(string)
		return id, ok
	}
	type frame struct {
		node  any
		level int
		links bool
	}
	stack := []frame{{doc, levels, false}}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.level < 1 {
			continue
		}
		m, ok := f.node.(map[string]any)
		if !ok {
			continue
		}
		for k, v := range m {
			inLinks := f.links || k == "Links"
			if typ == "." && k == "Links" {
				continue
			}
			switch x := v.(type) {
			case map[string]any:
				if id, ok := only(x); ok && (typ != "~" || inLinks) {
					if sub, ok := b.backend.render(rd, normalise(id)); ok && sub != nil {
						m[k] = sub
						stack = append(stack, frame{sub, f.level - 1, inLinks})
						continue
					}
				}
				stack = append(stack, frame{x, f.level, inLinks})
			case []any:
				for i, el := range x {
					if id, ok := only(el); ok && (typ != "~" || inLinks) {
						if sub, ok := b.backend.render(rd, normalise(id)); ok && sub != nil {
							x[i] = sub
							stack = append(stack, frame{sub, f.level - 1, inLinks})
							continue
						}
					}
					stack = append(stack, frame{el, f.level, inLinks})
				}
			}
		}
	}
}

// ---------------------------------------------------------------- POST

func readBody(r *http.Request) map[string]any {
	body, _ := readBodyRaw(r)
	return body
}

func readBodyRaw(r *http.Request) (map[string]any, []byte) {
	body := map[string]any{}
	if r.Body == nil {
		return body, nil
	}
	data, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		body["__multipart__"] = len(data)
		return body, data
	}
	_ = json.Unmarshal(data, &body)
	return body, data
}

// replay answers a write the way the captured BMC answered it (status, headers, body), and
// records the job or task it scheduled so a later GET of its Location resolves.
func (b *bmc) replay(w http.ResponseWriter, r *http.Request, path, canonical string, cr capturedResponse, src schema.EventSource) {
	for k, v := range cr.Headers {
		if k == "Location" {
			if i := strings.LastIndex(v, "/"); i >= 0 {
				// the captured job's id, bumped past the ones this capture's answer already made
				// (jobs scheduled otherwise, such as settingsJobs, are numbered apart)
				id := v[i+1:]
				stem := strings.TrimRight(id, "0123456789")
				if len(id)-len(stem) > 3 {
					stem = id[:len(id)-3]
				}
				n := 0
				for _, t := range b.snapshot().tasks {
					if strings.HasPrefix(t.ID, stem) {
						n++
					}
				}
				if n > 0 {
					id = bumpID(id, n)
				}
				v = v[:i+1] + id
				_, _ = b.a.eng.CreateTask(b.request(r, path, canonical, src), id, "job scheduled by "+canonical, 30*time.Second)
			}
		}
		w.Header().Set(k, v)
	}
	status := cr.Status
	if status == 0 {
		status = http.StatusNoContent
	}
	w.WriteHeader(status)
	io.WriteString(w, cr.Body)
}

// bumpID turns JID_900000000001 into JID_900000000001+n, keeping the vendor's id shape.
func bumpID(id string, n int) string {
	end := len(id)
	start := end
	for start > 0 && id[start-1] >= '0' && id[start-1] <= '9' {
		start--
	}
	if start == end {
		return fmt.Sprintf("%s_%d", id, n)
	}
	var num int64
	fmt.Sscan(id[start:], &num)
	return fmt.Sprintf("%s%0*d", id[:start], end-start, num+int64(n))
}

var resetMap = map[string]string{
	"On": "power.on", "ForceOn": "power.on", "ForceOff": "power.off.force", "GracefulShutdown": "power.off.graceful",
	"GracefulRestart": "power.restart", "ForceRestart": "power.restart", "PowerCycle": "power.cycle", "PushPowerButton": "power.toggle",
	"Nmi": "other", "AuxPowerCycle": "ac.cycle",
}

func (b *bmc) post(w http.ResponseWriter, r *http.Request, path string, src schema.EventSource) {
	body := readBody(r)
	eng := b.a.eng
	fail := func(err error) {
		status, id := http.StatusBadRequest, "Base.1.8.ActionNotSupported"
		switch {
		case errors.Is(err, engine.ErrNoInput), errors.Is(err, engine.ErrPrecondition):
			status, id = http.StatusConflict, "Base.1.8.ResourceInStandby"
		case errors.Is(err, engine.ErrBMCDown):
			status, id = http.StatusServiceUnavailable, "Base.1.8.ServiceTemporarilyUnavailable"
		}
		writeError(w, status, id, err.Error())
	}
	// dialect-bound actions first (OEM resets and the like)
	for _, ab := range b.dialect.Spec.Actions {
		if ok, _ := matchPath(ab.Path, path); !ok {
			continue
		}
		canonical := ab.Canonical
		if ab.ByField != "" {
			canonical = ab.Map[fmt.Sprint(body[ab.ByField])]
		}
		if ab.Behaviour == "ac-cycle" {
			canonical = "ac.cycle"
		}
		if canonical == "" {
			canonical = "other"
		}
		if strings.HasPrefix(canonical, "power.") || canonical == "ac.cycle" {
			req := b.request(r, path, canonical, src)
			if canonical == "ac.cycle" {
				req.Canonical = "ac.cycle"
			}
			if err := eng.Power(req); err != nil {
				fail(err)
				return
			}
		} else {
			eng.LogRead(b.request(r, path, canonical, src), 204)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch {
	case strings.HasSuffix(path, "/Actions/ComputerSystem.Reset") || strings.HasSuffix(path, "/Actions/Chassis.Reset"):
		rt := fmt.Sprint(body["ResetType"])
		canonical, ok := resetMap[rt]
		if !ok {
			writeError(w, http.StatusBadRequest, "Base.1.8.ActionParameterValueNotInList", "ResetType "+rt+" is not supported.")
			return
		}
		if canonical == "power.toggle" {
			canonical = "power.on"
			var p string
			eng.Read(func(*model.Model) { p = b.dev.State.Power })
			if p == model.On {
				canonical = "power.off.graceful"
			}
		}
		req := b.request(r, path, canonical, src)
		if rt == "GracefulRestart" {
			req.Params = map[string]any{"graceful": true}
		}
		if canonical == "other" {
			eng.LogRead(req, 204)
		} else if err := eng.Power(req); err != nil {
			fail(err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/Actions/Manager.Reset"):
		if err := eng.ResetBMC(b.request(r, path, "bmc.reset", src)); err != nil {
			fail(err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/Actions/Manager.ResetToDefaults"):
		eng.LogRead(b.request(r, path, "bmc.defaults", src), 204)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/Actions/Bios.ResetBios"):
		if err := eng.ResetBios(b.request(r, path, "bios.reset", src)); err != nil {
			fail(err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/Actions/Bios.ChangePassword"):
		eng.LogRead(b.request(r, path, "account.password", src), 204)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/Actions/LogService.ClearLog"):
		base := strings.TrimSuffix(path, "/Actions/LogService.ClearLog") + "/Entries"
		_ = eng.ClearLog(b.request(r, path, "log.clear", src), "SEL")
		_ = eng.Patch(b.request(r, path, "log.clear", src), "twin:cleared:"+base, map[string]any{"cleared": true})
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/Actions/UpdateService.SimpleUpdate") || strings.Contains(path, "/UpdateService/upload") || strings.Contains(path, "MultipartUpload") || path == b.multipartURI():
		canonical := "firmware.update.simple"
		if _, mp := body["__multipart__"]; mp {
			canonical = "firmware.update.multipart"
		}
		version := "twin-" + time.Now().UTC().Format("20060102.150405")
		if uri, ok := body["ImageURI"].(string); ok && uri != "" {
			version = versionFromURI(uri)
		}
		var comps []string
		if targets, ok := body["Targets"].([]any); ok {
			for _, t := range targets {
				id := fmt.Sprint(t)
				id = id[strings.LastIndex(id, "/")+1:]
				if c, ok := b.dialect.Spec.Firmware[id]; ok {
					comps = append(comps, c)
				}
			}
		}
		task, err := eng.UpdateFirmware(b.request(r, path, canonical, src), comps, version)
		if err != nil {
			fail(err)
			return
		}
		loc := "/redfish/v1/TaskService/Tasks/" + task.ID
		w.Header().Set("Location", loc)
		writeJSON(w, http.StatusAccepted, map[string]any{"@odata.id": loc, "@odata.type": "#Task.v1_7_1.Task", "Id": task.ID, "TaskState": "Running", "TaskStatus": "OK"})
	case strings.HasSuffix(path, "/Actions/SecureBoot.ResetKeys"):
		eng.LogRead(b.request(r, path, "secureboot.set", src), 204)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/Actions/VirtualMedia.InsertMedia"), strings.HasSuffix(path, "/Actions/VirtualMedia.EjectMedia"):
		canonical := "media.insert"
		if strings.HasSuffix(path, "EjectMedia") {
			canonical = "media.eject"
		}
		res := strings.SplitN(path, "/Actions/", 2)[0]
		props := map[string]any{"Inserted": canonical == "media.insert", "Image": body["Image"]}
		_ = eng.Patch(b.request(r, path, canonical, src), res, props)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/Actions/EventService.SubmitTestEvent"):
		eng.LogRead(b.request(r, path, "event.subscribe", src), 204)
		b.a.broadcastTest(b)
		w.WriteHeader(http.StatusNoContent)
	case path == "/redfish/v1/SessionService/Sessions":
		user, _ := body["UserName"].(string)
		pass, _ := body["Password"].(string)
		s, err := eng.CreateSession(b.request(r, path, "session.create", schema.EventSource{Class: "stack", Principal: user, Address: r.RemoteAddr}), user, pass)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "Base.1.8.NoValidSession", "Invalid username or password.")
			return
		}
		loc := "/redfish/v1/SessionService/Sessions/" + s.ID
		w.Header().Set("X-Auth-Token", s.Token)
		w.Header().Set("Location", loc)
		writeJSON(w, http.StatusCreated, map[string]any{"@odata.id": loc, "@odata.type": "#Session.v1_7_0.Session", "Id": s.ID, "Name": "User Session", "UserName": user})
	case path == "/redfish/v1/AccountService/Accounts":
		user, _ := body["UserName"].(string)
		pass, _ := body["Password"].(string)
		role, _ := body["RoleId"].(string)
		a, err := eng.CreateAccount(b.request(r, path, "account.create", src), user, pass, role)
		if err != nil {
			writeError(w, http.StatusConflict, "Base.1.8.ResourceAlreadyExists", err.Error())
			return
		}
		loc := path + "/" + a.ID
		w.Header().Set("Location", loc)
		writeJSON(w, http.StatusCreated, map[string]any{"@odata.id": loc, "Id": a.ID, "UserName": a.User, "RoleId": a.Role, "Enabled": a.Enabled})
	case path == "/redfish/v1/EventService/Subscriptions":
		dest, _ := body["Destination"].(string)
		ctx, _ := body["Context"].(string)
		s, err := eng.AddSubscription(b.request(r, path, "event.subscribe", src), model.Subscription{Destination: dest, Context: ctx, Protocol: "Redfish"})
		if err != nil {
			fail(err)
			return
		}
		loc := path + "/" + s.ID
		w.Header().Set("Location", loc)
		writeJSON(w, http.StatusCreated, map[string]any{"@odata.id": loc, "Id": s.ID, "Destination": dest})
	default:
		// an action the binding has no behaviour for: accepted when its resource exists, as the
		// captured BMC would accept it (logged under its canonical name, or `other`)
		res := strings.SplitN(path, "/Actions/", 2)[0]
		rd := &renderer{b: b, snap: b.snapshot()}
		if doc, ok := b.backend.render(rd, res); !ok || doc == nil {
			writeError(w, http.StatusNotFound, "Base.1.8.ResourceNotFound", "No resource at "+res)
			return
		}
		canonical := b.canonical(r.Method, path)
		eng.LogRead(b.request(r, path, canonical, src), 204)
		if strings.Contains(path, "/Actions/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = eng.Patch(b.request(r, path, canonical, src), path+"#posted", map[string]any{"last": body})
		w.WriteHeader(http.StatusNoContent)
	}
}

func versionFromURI(uri string) string {
	base := uri[strings.LastIndex(uri, "/")+1:]
	for _, ext := range []string{".fwpkg", ".bin", ".tar.gz", ".tar", ".pldm", ".exe", ".efi"} {
		base = strings.TrimSuffix(base, ext)
	}
	if base == "" {
		return "twin-update"
	}
	return base
}

// ---------------------------------------------------------------- PATCH

func (b *bmc) patch(w http.ResponseWriter, r *http.Request, path string, src schema.EventSource) {
	body, raw := readBodyRaw(r)
	if b.tree != nil {
		if cr, ok := b.tree.response(r.Method, path, raw); ok {
			b.patchSemantics(r, path, body, src)
			b.replay(w, r, path, b.canonical(r.Method, path), cr, src)
			return
		}
	}
	if status, id, msg, ok := b.patchSemantics(r, path, body, src); !ok {
		writeError(w, status, id, msg)
		return
	}
	if tmpl := b.dialect.Spec.SettingsJobs; tmpl != "" && b.isSettingsObject(path) {
		n := 700000000001 + int64(b.a.eng.TaskCount(b.dev.Path))
		loc := strings.ReplaceAll(tmpl, "{n}", fmt.Sprintf("%012d", n))
		_, _ = b.a.eng.CreateTask(b.request(r, path, b.canonical(r.Method, path), src), loc[strings.LastIndex(loc, "/")+1:],
			"job scheduled by a settings PATCH", 30*time.Second)
		w.Header().Set("Location", loc)
		writeJSON(w, http.StatusAccepted, map[string]any{"@Message.ExtendedInfo": []any{map[string]any{
			"Message": "Successfully scheduled the job.", "MessageArgs": []any{}, "MessageId": "IDRAC.2.9.JCP001", "Severity": "OK"}}})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// isSettingsObject is whether path is a settings object a PATCH stages for the next reset (the
// system's or its BIOS's @Redfish.Settings target, or a /Settings resource).
func (b *bmc) isSettingsObject(path string) bool {
	if strings.HasSuffix(path, "/Settings") {
		return true
	}
	return b.settingsOwner(&renderer{b: b, snap: b.snapshot()}, path) != ""
}

// patchSemantics applies a PATCH to the model: boot, BIOS, Secure Boot, accounts, lockdown,
// and everything else as persisted properties.
func (b *bmc) patchSemantics(r *http.Request, path string, body map[string]any, src schema.EventSource) (int, string, string, bool) {
	eng := b.a.eng
	rd := &renderer{b: b, snap: b.snapshot()}
	// a PATCH to a resource's settings object (@Redfish.Settings) is a change to that resource,
	// applied at the next reset: the system's (boot overrides on GB trays) or its BIOS's
	// (/Bios/Settings, /Bios/SD on iDRAC, /Bios/Pending on XCC)
	owner := b.settingsOwner(rd, path)
	if doc, ok := b.backend.render(rd, path); (!ok || doc == nil) && owner == "" {
		if !(strings.HasSuffix(path, "/Settings") || strings.HasSuffix(path, "/SD") || strings.HasSuffix(path, "/Pending")) {
			return http.StatusNotFound, "Base.1.8.ResourceNotFound", "No resource at " + path, false
		}
	}
	isSystem := func(p string) bool {
		return p == b.dialect.Spec.System || (strings.HasPrefix(p, "/redfish/v1/Systems/") && strings.Count(p, "/") == 4)
	}
	switch {
	case isSystem(path) || isSystem(owner):
		if boot, ok := body["Boot"].(map[string]any); ok {
			var p engine.BootPatch
			str := func(k string) *string {
				if v, ok := boot[k].(string); ok {
					return &v
				}
				return nil
			}
			p.Target, p.Enabled, p.Mode, p.URI = str("BootSourceOverrideTarget"), str("BootSourceOverrideEnabled"), str("BootSourceOverrideMode"), str("HttpBootUri")
			if order, ok := boot["BootOrder"].([]any); ok {
				for _, o := range order {
					p.Order = append(p.Order, fmt.Sprint(o))
				}
			}
			// clients such as gofish send the whole Boot object: an override is an override even
			// when the unchanged BootOrder comes with it
			canonical := "boot.override"
			if p.Target == nil && p.Enabled == nil && p.Mode == nil && p.URI == nil && p.Order != nil {
				canonical = "boot.order"
			}
			if err := eng.SetBoot(b.request(r, path, canonical, src), p); err != nil {
				return 400, "Base.1.8.PropertyValueNotInList", err.Error(), false
			}
			delete(body, "Boot")
		}
		if len(body) > 0 {
			_ = eng.Patch(b.request(r, path, "other", src), path, body)
		}
	case strings.HasSuffix(path, "/Bios/Settings") || strings.HasSuffix(path, "/Bios/SD") || strings.HasSuffix(path, "/Bios/Pending") ||
		strings.HasSuffix(path, "/Bios") || strings.HasSuffix(strings.ToLower(owner), "/bios"):
		attrs, _ := body["Attributes"].(map[string]any)
		if err := eng.SetBiosPending(b.request(r, path, "bios.set", src), attrs); err != nil {
			return 400, "Base.1.8.GeneralError", err.Error(), false
		}
	case strings.HasSuffix(path, "/SecureBoot"):
		if v, ok := body["SecureBootEnable"].(bool); ok {
			_ = eng.SetFlag(b.request(r, path, "secureboot.set", src), "secureBoot", v)
			delete(body, "SecureBootEnable")
		}
		if len(body) > 0 {
			_ = eng.Patch(b.request(r, path, "secureboot.set", src), path, body)
		}
	case isManager(path) && body["DateTime"] != nil:
		// the BMC's clock: set, it keeps running (bmc.clock); anything else in the body, such as
		// DateTimeLocalOffset, is the Manager's own property
		v, _ := body["DateTime"].(string)
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return 400, "Base.1.8.PropertyValueFormatError", fmt.Sprintf("DateTime %q is not an RFC 3339 date-time", v), false
		}
		if err := eng.SetBMCClock(b.request(r, path, "bmc.clock", src), t); err != nil {
			return http.StatusNotFound, "Base.1.8.ResourceNotFound", err.Error(), false
		}
		delete(body, "DateTime")
		if len(body) > 0 {
			_ = eng.Patch(b.request(r, path, "other", src), path, body)
		}
	case strings.HasPrefix(path, "/redfish/v1/AccountService/Accounts/"):
		id := strings.TrimPrefix(path, "/redfish/v1/AccountService/Accounts/")
		canonical := "account.update"
		if _, ok := body["Password"]; ok {
			canonical = "account.password"
		}
		if err := eng.UpdateAccount(b.request(r, path, canonical, src), id, body); err != nil {
			return http.StatusNotFound, "Base.1.8.ResourceNotFound", err.Error(), false
		}
	default:
		canonical := b.canonical(r.Method, path)
		if lockdown, on := lockdownIn(body); lockdown {
			_ = eng.SetFlag(b.request(r, path, "lockdown.set", src), "lockdown", on)
			canonical = "lockdown.set"
		}
		_ = eng.Patch(b.request(r, path, canonical, src), path, body)
	}
	return 0, "", "", true
}

// settingsOwner returns the resource whose settings object path is: the system's or its BIOS's.
func (b *bmc) settingsOwner(rd *renderer, path string) string {
	sys := b.dialect.Spec.System
	if sys == "" {
		return ""
	}
	for _, cand := range []string{sys, sys + "/Bios"} {
		doc, ok := b.backend.render(rd, cand)
		if !ok || doc == nil {
			continue
		}
		set, _ := doc["@Redfish.Settings"].(map[string]any)
		so, _ := set["SettingsObject"].(map[string]any)
		if id, _ := so["@odata.id"].(string); id != "" && strings.EqualFold(normalise(id), path) {
			return cand
		}
	}
	return ""
}

// lockdownIn recognises the lockdown settings stacks write (Dell's system lockdown attribute,
// the KCS/host-interface switches NICo flips on NVIDIA BMCs).
func lockdownIn(body map[string]any) (bool, bool) {
	var found, on bool
	var walk func(v any)
	walk = func(v any) {
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		for k, val := range m {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "lockdown") {
				found = true
				s := strings.ToLower(fmt.Sprint(val))
				on = s == "enabled" || s == "true"
			}
			walk(val)
		}
	}
	walk(body)
	return found, on
}

// ---------------------------------------------------------------- DELETE

func (b *bmc) delete(w http.ResponseWriter, r *http.Request, path string, src schema.EventSource) {
	eng := b.a.eng
	var err error
	switch {
	case strings.HasPrefix(path, "/redfish/v1/SessionService/Sessions/"):
		err = eng.DeleteSession(b.request(r, path, "session.delete", src), strings.TrimPrefix(path, "/redfish/v1/SessionService/Sessions/"))
	case strings.HasPrefix(path, "/redfish/v1/AccountService/Accounts/"):
		err = eng.DeleteAccount(b.request(r, path, "account.delete", src), strings.TrimPrefix(path, "/redfish/v1/AccountService/Accounts/"))
	case strings.HasPrefix(path, "/redfish/v1/EventService/Subscriptions/"):
		err = eng.DeleteSubscription(b.request(r, path, "event.subscribe", src), strings.TrimPrefix(path, "/redfish/v1/EventService/Subscriptions/"))
	default:
		writeError(w, http.StatusMethodNotAllowed, "Base.1.8.OperationNotAllowed", "DELETE is not allowed on "+path)
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "Base.1.8.ResourceNotFound", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- canonical names

// canonical maps a request to a canonical name (docs/spec/14-expressions.md); the dialect's
// request list wins, then the generic Redfish rules.
func (b *bmc) canonical(method, path string) string {
	for _, rq := range b.dialect.Spec.Requests {
		if rq.Method != "" && rq.Method != method {
			continue
		}
		if ok, _ := matchPath(rq.Path, path); ok {
			return rq.Canonical
		}
	}
	p := strings.ToLower(path)
	if method == http.MethodGet || method == http.MethodHead {
		switch {
		case strings.Contains(p, "/sse") || strings.HasSuffix(p, "/eventservice/sse"):
			return "event.sse"
		case strings.Contains(p, "lockdown"):
			return "lockdown.read"
		case strings.Contains(p, "/secureboot"):
			return "secureboot.read"
		case strings.Contains(p, "/bios"):
			return "bios.read"
		case strings.Contains(p, "/firmwareinventory") || strings.Contains(p, "/softwareinventory") || strings.Contains(p, "/updateservice"):
			return "firmware.inventory.read"
		case strings.Contains(p, "/taskservice") || strings.Contains(p, "/jobs"):
			return "task.read"
		case strings.Contains(p, "/logservices"):
			return "log.read"
		case strings.Contains(p, "/accountservice") || strings.Contains(p, "/sessionservice"):
			return "account.read"
		case strings.Contains(p, "/sensors") || strings.Contains(p, "/thermal") || strings.Contains(p, "/power") || strings.Contains(p, "environmentmetrics") || strings.Contains(p, "leakdetect"):
			return "health.read"
		case strings.Contains(p, "/ethernetinterfaces") || strings.Contains(p, "/networkadapters") || strings.Contains(p, "/networkdevicefunctions") || strings.Contains(p, "/ports") || strings.Contains(p, "/networkprotocol"):
			return "network.read"
		case strings.Contains(p, "/serialinterfaces"):
			return "console.setup"
		}
		return "inventory.read"
	}
	switch {
	case strings.Contains(p, "serial") || strings.Contains(p, "networkprotocol"):
		return "console.setup"
	case strings.Contains(p, "accountservice"):
		return "account.policy"
	case strings.Contains(p, "lockdown"):
		return "lockdown.set"
	case strings.Contains(p, "tpm"):
		return "tpm.clear"
	case strings.Contains(p, "secureerase") || strings.Contains(p, "drive.secureerase"):
		return "erase.start"
	case strings.Contains(p, "virtualmedia"):
		return "media.insert"
	}
	return "other"
}

// isManager is whether path is a Manager resource (/redfish/v1/Managers/<id>).
func isManager(path string) bool {
	return strings.HasPrefix(path, "/redfish/v1/Managers/") && strings.Count(path, "/") == 4
}

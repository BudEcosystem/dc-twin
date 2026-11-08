package storage

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// VAST's management API (VMS) as a storage system's manager vast/vms
// (docs/reviews/2026-11-08-storage-deep-dive.md P6, S-Q6): the subset VAST's CSI driver
// (vast-csi, Apache-2.0) calls for NFS volumes, read from its source (vast_csi/session and
// plugins/nfs.py at vastcsi 2.7): a JWT from /api/v1/token/, the cluster's version, a tenant, a
// view policy, a VIP pool, views (an NFS export of a path), quotas (a path's hard limit), the
// trash folder API, snapshots and global snapshot streams. Every object is the engine's: a view
// is a file system at its path with an export, a quota that file system's quota, a snapshot its
// snapshot. The VIP pool's addresses are the system's data addresses.

const (
	vmsVersion = "5.3.1.0"
	tenantID   = 1
	tenantName = "default"
)

func (s *server) vmsAuth(r *http.Request) bool {
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		s.mu.Lock()
		_, ok := s.sessions[strings.TrimPrefix(a, "Bearer ")]
		s.mu.Unlock()
		return ok
	}
	_, ok := s.authenticate(r)
	return ok
}

func vmsJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func vmsError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, engine.ErrNoStorageObject):
		status = http.StatusNotFound
	case errors.Is(err, engine.ErrExists):
		status = http.StatusConflict
	case errors.Is(err, engine.ErrPrecondition) && strings.Contains(err.Error(), "is up"):
		status = http.StatusServiceUnavailable
	}
	vmsJSON(w, status, map[string]any{"detail": err.Error()})
}

// serveVMS answers /api/… for a system whose manager is vast/vms.
func (s *server) serveVMS(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(p, "/")
	if len(parts) < 2 || parts[0] != "api" {
		vmsJSON(w, http.StatusNotFound, map[string]any{"detail": "Not found."})
		return
	}
	parts = parts[1:]
	if parts[0] == "v1" || parts[0] == "latest" || (len(parts[0]) > 1 && parts[0][0] == 'v' && isDigits(parts[0][1:])) {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		vmsJSON(w, http.StatusNotFound, map[string]any{"detail": "Not found."})
		return
	}
	res, rest := parts[0], parts[1:]
	if res == "token" {
		s.vmsToken(w, r, rest)
		return
	}
	if !s.vmsAuth(r) {
		vmsJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Authentication credentials were not provided."})
		return
	}
	var body map[string]any
	if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodDelete || r.Method == http.MethodPut) {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if body == nil {
		body = map[string]any{}
	}
	src := schema.EventSource{Class: "stack", Principal: s.user, Address: r.RemoteAddr}
	q := r.URL.Query()
	switch res {
	case "versions":
		vmsJSON(w, 200, filter([]map[string]any{{"id": 1, "name": "vast", "sys_version": vmsVersion, "status": "success", "os_version": "dc-twin"}}, q))
	case "clusters":
		vmsJSON(w, 200, filter([]map[string]any{{"id": 1, "name": s.name, "guid": uuidOf(s.path), "sw_version": vmsVersion, "state": "ONLINE"}}, q))
	case "tenants":
		vmsJSON(w, 200, s.vmsOne(filter([]map[string]any{{"id": tenantID, "name": tenantName, "guid": uuidOf(s.path + "/tenant")}}, q), rest))
	case "viewpolicies":
		vmsJSON(w, 200, s.vmsOne(filter(s.viewPolicies(), q), rest))
	case "qospolicies":
		vmsJSON(w, 200, s.vmsOne(filter([]map[string]any{}, q), rest))
	case "vippools":
		vmsJSON(w, 200, s.vmsOne(filter(s.vipPools(), q), rest))
	case "globalsnapstreams":
		s.vmsStreams(w, r, rest, q)
	case "plugins":
		vmsJSON(w, 200, map[string]any{}) // usage reports, accepted and dropped
	case "views":
		s.vmsViews(w, r, rest, q, body, src)
	case "quotas":
		s.vmsQuotas(w, r, rest, q, body, src)
	case "folders":
		s.vmsFolders(w, r, rest, body, src)
	case "snapshots":
		s.vmsSnapshots(w, r, rest, q, body, src)
	default:
		vmsJSON(w, http.StatusNotFound, map[string]any{"detail": "Not found."})
	}
}

func isDigits(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// vmsOne answers GET /<res>/<id>/ from a list, or the list itself.
func (s *server) vmsOne(list []map[string]any, rest []string) any {
	if len(rest) == 0 || rest[0] == "" {
		return list
	}
	for _, o := range list {
		if fmt.Sprint(o["id"]) == rest[0] {
			return o
		}
	}
	return map[string]any{"detail": "Not found."}
}

// filter applies VMS's query filters: field=value, field__endswith, field__contains, and
// page_size/page (one page holds everything).
func filter(list []map[string]any, q url.Values) []map[string]any {
	out := []map[string]any{}
	for _, o := range list {
		ok := true
		for k, vs := range q {
			v := vs[0]
			switch {
			case k == "page_size" || k == "page" || k == "fields":
			case strings.HasSuffix(k, "__endswith"):
				ok = ok && strings.HasSuffix(fmt.Sprint(field(o, strings.TrimSuffix(k, "__endswith"))), v)
			case strings.HasSuffix(k, "__startswith"):
				ok = ok && strings.HasPrefix(fmt.Sprint(field(o, strings.TrimSuffix(k, "__startswith"))), v)
			case strings.HasSuffix(k, "__contains"):
				ok = ok && strings.Contains(fmt.Sprint(field(o, strings.TrimSuffix(k, "__contains"))), v)
			default:
				ok = ok && fmt.Sprint(field(o, k)) == v
			}
		}
		if ok {
			out = append(out, o)
		}
	}
	return out
}

// field reads a filter's field, policy__name being the view's policy's name.
func field(o map[string]any, k string) any {
	if strings.Contains(k, "__") {
		a, b, _ := strings.Cut(k, "__")
		if m, ok := o[a].(map[string]any); ok {
			return m[b]
		}
		if v, ok := o[a].(string); ok && b == "name" { // a view's policy is its policy's name
			return v
		}
		return o[a+"_"+b]
	}
	if k == "path" {
		if p, ok := o[k].(string); ok {
			return strings.TrimSuffix(p, "/")
		}
	}
	return o[k]
}

func (s *server) vmsToken(w http.ResponseWriter, r *http.Request, rest []string) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	u, _ := body["username"].(string)
	p, _ := body["password"].(string)
	if len(rest) > 0 && rest[0] == "refresh" {
		// a refresh token is a session token: answer with a new access token
		rt, _ := body["refresh"].(string)
		s.mu.Lock()
		_, ok := s.sessions[rt]
		s.mu.Unlock()
		if !ok {
			vmsJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Token is invalid or expired"})
			return
		}
		u, p = s.user, s.password
	}
	if subtle.ConstantTimeCompare([]byte(u), []byte(s.user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(s.password)) != 1 {
		vmsJSON(w, http.StatusUnauthorized, map[string]any{"detail": "No active account found with the given credentials"})
		return
	}
	access, refresh := jwt(s.user), jwt(s.user)
	s.mu.Lock()
	s.sessSeq++
	s.sessions[access] = "vms-" + strconv.Itoa(s.sessSeq)
	s.sessions[refresh] = "vms-" + strconv.Itoa(s.sessSeq)
	s.mu.Unlock()
	vmsJSON(w, 200, map[string]any{"access": access, "refresh": refresh})
}

// jwt is an opaque bearer token in a JWT's shape (the client does not read it).
func jwt(user string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	enc := func(v any) string {
		j, _ := json.Marshal(v)
		return strings.TrimRight(base64URL(j), "=")
	}
	return enc(map[string]any{"alg": "HS256", "typ": "JWT"}) + "." +
		enc(map[string]any{"user": user, "exp": time.Now().Add(time.Hour).Unix(), "jti": hex.EncodeToString(b)}) + "." + hex.EncodeToString(b)
}

func base64URL(b []byte) string {
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var n uint32
		k := 0
		for j := 0; j < 3; j++ {
			n <<= 8
			if i+j < len(b) {
				n |= uint32(b[i+j])
				k++
			}
		}
		for j := 0; j <= k; j++ {
			out.WriteByte(enc[(n>>(18-6*j))&63])
		}
	}
	return out.String()
}

func (s *server) viewPolicies() []map[string]any {
	pol := func(id int, name string, protocols []string) map[string]any {
		return map[string]any{"id": id, "name": name, "tenant_id": tenantID, "tenant_name": tenantName, "flavor": "NFS",
			"protocols": protocols, "nfs_read_write": []string{"*"}, "nfs_no_squash": []string{"*"}, "nfs_root_squash": []string{}}
	}
	return []map[string]any{pol(1, "default", []string{"NFS", "NFS4"}), pol(2, "s3_default_policy", []string{"S3"})}
}

func (s *server) vipPools() []map[string]any {
	v := s.view()
	var ips []string
	for _, n := range v.Nodes {
		if a := v.Addresses["node:"+n.Path]; a != "" {
			ips = append(ips, a)
		}
	}
	if len(ips) == 0 && v.Addresses["data"] != "" {
		ips = []string{v.Addresses["data"]}
	}
	sort.Strings(ips)
	var ranges []any
	for _, ip := range ips {
		ranges = append(ranges, []any{ip, ip})
	}
	if ranges == nil {
		ranges = []any{}
	}
	return []map[string]any{{"id": 1, "name": "vippool-1", "role": "PROTOCOLS", "tenant_id": tenantID, "ip_ranges": ranges}}
}

// a view is a file system that has an export; its id the file system's.
func (s *server) viewObj(o engine.StorageObjectView) map[string]any {
	id, _ := strconv.Atoi(o.ID)
	return map[string]any{"id": id, "guid": uuidOf(s.path + "/view/" + o.ID), "name": o.Name, "path": o.Path, "protocols": []string{"NFS", "NFS4"},
		"policy_id": 1, "policy": "default", "tenant_id": tenantID, "tenant_name": tenantName, "created": o.Created.UTC().Format(time.RFC3339)}
}

func (s *server) quotaObj(o engine.StorageObjectView) map[string]any {
	id, _ := strconv.Atoi(o.ID)
	var hard any
	if o.CapacityBytes > 0 {
		hard = o.CapacityBytes
	}
	return map[string]any{"id": id, "guid": uuidOf(s.path + "/quota/" + o.ID), "name": path.Base(o.Path), "path": o.Path, "hard_limit": hard,
		"tenant_id": tenantID, "tenant_name": tenantName, "used_effective_capacity": o.UsedBytes, "used_capacity": o.UsedBytes, "state": "OK"}
}

func (s *server) fsAt(p string) *engine.StorageObjectView {
	p = "/" + strings.Trim(p, "/")
	v := s.view()
	for i := range v.FileSystems {
		if v.FileSystems[i].Path == p {
			return &v.FileSystems[i]
		}
	}
	return nil
}

func (s *server) fsByID(id string) *engine.StorageObjectView {
	v := s.view()
	for i := range v.FileSystems {
		if v.FileSystems[i].ID == id {
			return &v.FileSystems[i]
		}
	}
	return nil
}

// fsName is a file system's name for a VMS path: its segments joined, made a valid name.
func fsName(p string) string {
	n := strings.ToLower(strings.ReplaceAll(strings.Trim(p, "/"), "/", "-"))
	var b strings.Builder
	for _, c := range n {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	n = strings.TrimLeft(b.String(), "-.")
	if len(n) > 63 || n == "" {
		sum := sha256.Sum256([]byte(p))
		n = strings.TrimRight(n[:min(len(n), 50)], "-.") + "-" + hex.EncodeToString(sum[:6])
	}
	return n
}

var fullExport = []model.Export{{Clients: "*", Access: "rw", RootSquash: false}}

// ensureFS makes the file system at a path if there is none (a view's create_dir, a quota's path).
func (s *server) ensureFS(r *http.Request, p string, exports []model.Export, src schema.EventSource) (*engine.StorageObjectView, error) {
	if o := s.fsAt(p); o != nil {
		return o, nil
	}
	if exports == nil {
		exports = []model.Export{}
	}
	f, err := s.a.rt.Engine.CreateFileSystem(s.req(r, "storage.filesystem.create", src), engine.FileSystemSpec{Name: fsName(p), Path: "/" + strings.Trim(p, "/"), Exports: exports})
	if err != nil {
		return nil, err
	}
	return s.fsByID(f.ID), nil
}

func (s *server) vmsViews(w http.ResponseWriter, r *http.Request, rest []string, q url.Values, body map[string]any, src schema.EventSource) {
	eng := s.a.rt.Engine
	var views []map[string]any
	for _, o := range s.view().FileSystems {
		if len(o.Exports) > 0 {
			views = append(views, s.viewObj(o))
		}
	}
	switch {
	case r.Method == http.MethodGet:
		out := s.vmsOne(filter(views, q), rest)
		if m, ok := out.(map[string]any); ok && m["detail"] != nil {
			vmsJSON(w, 404, out)
			return
		}
		vmsJSON(w, 200, out)
	case r.Method == http.MethodPost:
		p, _ := body["path"].(string)
		if p == "" {
			vmsJSON(w, 400, map[string]any{"path": []string{"This field is required."}})
			return
		}
		if o := s.fsAt(p); o != nil && len(o.Exports) > 0 {
			vmsJSON(w, 400, map[string]any{"path": []string{"View with this path already exists."}})
			return
		}
		o, err := s.ensureFS(r, p, fullExport, src)
		if err == nil && len(o.Exports) == 0 {
			_, err = eng.UpdateFileSystem(s.req(r, "storage.export.create", src), o.ID, nil, fullExport)
			o = s.fsByID(o.ID)
		}
		if err != nil {
			vmsError(w, err)
			return
		}
		vmsJSON(w, 201, s.viewObj(*o))
	case r.Method == http.MethodPatch && len(rest) > 0:
		o := s.fsByID(rest[0])
		if o == nil {
			vmsJSON(w, 404, map[string]any{"detail": "Not found."})
			return
		}
		vmsJSON(w, 200, s.viewObj(*o)) // protocols are NFS and NFS4 already
	case r.Method == http.MethodDelete && len(rest) > 0:
		// a view's deletion stops its export; its directory (the file system) stays
		if _, err := eng.UpdateFileSystem(s.req(r, "storage.export.delete", src), rest[0], nil, []model.Export{}); err != nil {
			vmsError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		vmsJSON(w, 405, map[string]any{"detail": "Method not allowed."})
	}
}

func (s *server) vmsQuotas(w http.ResponseWriter, r *http.Request, rest []string, q url.Values, body map[string]any, src schema.EventSource) {
	eng := s.a.rt.Engine
	var quotas []map[string]any
	for _, o := range s.view().FileSystems {
		if o.Quota {
			quotas = append(quotas, s.quotaObj(o))
		}
	}
	limit := func() int64 {
		n, _ := body["hard_limit"].(float64)
		return int64(n)
	}
	switch {
	case r.Method == http.MethodGet:
		out := s.vmsOne(filter(quotas, q), rest)
		if m, ok := out.(map[string]any); ok && m["detail"] != nil {
			vmsJSON(w, 404, out)
			return
		}
		vmsJSON(w, 200, out)
	case r.Method == http.MethodPost:
		p, _ := body["path"].(string)
		o, err := s.ensureFS(r, p, nil, src)
		if err == nil && o.Quota {
			err = fmt.Errorf("%w: a quota on %s", engine.ErrExists, p)
		}
		if err == nil {
			_, err = eng.SetQuota(s.req(r, "storage.quota.create", src), o.ID, limit(), false)
		}
		if err != nil {
			vmsError(w, err)
			return
		}
		vmsJSON(w, 201, s.quotaObj(*s.fsByID(o.ID)))
	case (r.Method == http.MethodPatch || r.Method == http.MethodPut) && len(rest) > 0:
		if _, err := eng.SetQuota(s.req(r, "storage.quota.update", src), rest[0], limit(), false); err != nil {
			vmsError(w, err)
			return
		}
		vmsJSON(w, 200, s.quotaObj(*s.fsByID(rest[0])))
	case r.Method == http.MethodDelete && len(rest) > 0:
		o := s.fsByID(rest[0])
		if o == nil || !o.Quota {
			vmsJSON(w, 404, map[string]any{"detail": "Not found."})
			return
		}
		if _, err := eng.SetQuota(s.req(r, "storage.quota.delete", src), rest[0], 0, true); err != nil {
			vmsError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		vmsJSON(w, 405, map[string]any{"detail": "Method not allowed."})
	}
}

// vmsFolders is the trash API: DELETE /folders/delete_folder/ removes a path's directory, which
// is its file system, with its data.
func (s *server) vmsFolders(w http.ResponseWriter, r *http.Request, rest []string, body map[string]any, src schema.EventSource) {
	if r.Method != http.MethodDelete || len(rest) == 0 || rest[0] != "delete_folder" {
		vmsJSON(w, 405, map[string]any{"detail": "Method not allowed."})
		return
	}
	p, _ := body["path"].(string)
	o := s.fsAt(p)
	if o == nil {
		vmsJSON(w, 400, map[string]any{"detail": "no such directory " + p})
		return
	}
	if err := s.a.rt.Engine.DeleteStorageObject(s.req(r, "storage.filesystem.delete", src), "filesystem", o.ID); err != nil {
		vmsError(w, err)
		return
	}
	vmsJSON(w, 200, map[string]any{"async_task": nil})
}

func (s *server) vmsSnapshots(w http.ResponseWriter, r *http.Request, rest []string, q url.Values, body map[string]any, src schema.EventSource) {
	eng := s.a.rt.Engine
	v := s.view()
	pathOf := map[string]string{}
	for _, o := range v.FileSystems {
		pathOf[o.ID] = o.Path
	}
	var snaps []map[string]any
	for _, sn := range v.Snapshots {
		id, _ := strconv.Atoi(sn.ID)
		snaps = append(snaps, map[string]any{"id": id, "name": sn.Name, "path": pathOf[sn.Of], "tenant_id": tenantID, "tenant_name": tenantName,
			"created": sn.Created.UTC().Format("2006-01-02T15:04:05.000000Z"), "state": "SUCCESS", "locked": false, "indestructible": false})
	}
	switch {
	case r.Method == http.MethodGet:
		out := s.vmsOne(filter(snaps, q), rest)
		if m, ok := out.(map[string]any); ok && m["detail"] != nil {
			vmsJSON(w, 404, out)
			return
		}
		vmsJSON(w, 200, out)
	case r.Method == http.MethodPost && len(rest) >= 2 && rest[1] == "clone":
		s.vmsClone(w, r, rest[0], v, body, src)
	case r.Method == http.MethodPost:
		name, _ := body["name"].(string)
		p, _ := body["path"].(string)
		for _, sn := range snaps {
			if sn["name"] == name {
				vmsJSON(w, 400, map[string]any{"name": []string{"This field must be unique."}})
				return
			}
		}
		o := s.fsAt(p)
		if o == nil {
			vmsJSON(w, 400, map[string]any{"path": []string{"no such directory " + p}})
			return
		}
		sn, err := eng.CreateSnapshot(s.req(r, "storage.snapshot.create", src), o.ID, name)
		if err != nil {
			vmsError(w, err)
			return
		}
		if exp, _ := body["expiration_time"].(string); exp != "" {
			// a snapshot that expires goes when the stream cloned from it is deleted (VAST's
			// clone of a volume: vast-csi's VolumeFromVolumeBuilder)
			s.mu.Lock()
			if s.expiring == nil {
				s.expiring = map[string]bool{}
			}
			s.expiring[sn.ID] = true
			s.mu.Unlock()
		}
		id, _ := strconv.Atoi(sn.ID)
		vmsJSON(w, 201, map[string]any{"id": id, "name": sn.Name, "path": o.Path, "tenant_id": tenantID, "tenant_name": tenantName,
			"created": sn.Created.UTC().Format("2006-01-02T15:04:05.000000Z"), "state": "SUCCESS"})
	case r.Method == http.MethodDelete && len(rest) > 0:
		if err := eng.DeleteStorageObject(s.req(r, "storage.snapshot.delete", src), "snapshot", rest[0]); err != nil {
			vmsError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		vmsJSON(w, 405, map[string]any{"detail": "Method not allowed."})
	}
}

// vmsClone is POST /snapshots/<id>/clone/: a global snapshot stream that fills a new path with
// the snapshot's contents. The new path is a file system holding what its source held; the stream
// is finished at once.
func (s *server) vmsClone(w http.ResponseWriter, r *http.Request, snapID string, v engine.StorageSystemView, body map[string]any, src schema.EventSource) {
	var of string
	for _, sn := range v.Snapshots {
		if sn.ID == snapID {
			of = sn.Of
		}
	}
	if of == "" {
		vmsJSON(w, 404, map[string]any{"detail": "Not found."})
		return
	}
	dest, _ := body["loanee_root_path"].(string)
	name, _ := body["name"].(string)
	if dest == "" {
		vmsJSON(w, 400, map[string]any{"loanee_root_path": []string{"This field is required."}})
		return
	}
	o, err := s.ensureFS(r, dest, nil, src)
	if err != nil {
		vmsError(w, err)
		return
	}
	if from := s.fsByID(of); from != nil {
		_ = s.a.rt.Engine.ObserveStorageUse(s.path, "filesystem", o.ID, from.UsedBytes, src)
	}
	sid, _ := strconv.Atoi(snapID)
	s.mu.Lock()
	if s.streams == nil {
		s.streams = map[int]map[string]any{}
	}
	s.streamSeq++
	st := map[string]any{"id": s.streamSeq, "name": name, "loanee_root_path": "/" + strings.Trim(dest, "/"), "loanee_tenant_id": tenantID,
		"loanee_snapshot": map[string]any{"id": sid}, "loanee_snapshot_id": sid, "enabled": true,
		"state": "Completed", "status": map[string]any{"state": "Finished", "progress": 100}}
	s.streams[s.streamSeq] = st
	s.mu.Unlock()
	vmsJSON(w, 201, st)
}

// vmsStreams lists, reads, stops and deletes global snapshot streams (clones in progress: every
// one here is finished).
func (s *server) vmsStreams(w http.ResponseWriter, r *http.Request, rest []string, q url.Values) {
	s.mu.Lock()
	var list []map[string]any
	for _, st := range s.streams {
		list = append(list, st)
	}
	s.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i]["id"].(int) < list[j]["id"].(int) })
	switch r.Method {
	case http.MethodGet:
		out := s.vmsOne(filter(list, q), rest)
		if m, ok := out.(map[string]any); ok && m["detail"] != nil {
			vmsJSON(w, 404, out)
			return
		}
		vmsJSON(w, 200, out)
	case http.MethodPatch: // /<id>/stop
		vmsJSON(w, 200, map[string]any{"id": 0, "state": "COMPLETED"})
	case http.MethodDelete:
		if len(rest) > 0 {
			id, _ := strconv.Atoi(rest[0])
			s.mu.Lock()
			st, ok := s.streams[id]
			delete(s.streams, id)
			snap := ""
			if ok {
				snap = fmt.Sprint(st["loanee_snapshot_id"])
				if !s.expiring[snap] {
					snap = ""
				}
				delete(s.expiring, snap)
			}
			s.mu.Unlock()
			if !ok {
				vmsJSON(w, 404, map[string]any{"detail": "Not found."})
				return
			}
			if snap != "" {
				_ = s.a.rt.Engine.DeleteStorageObject(s.req(r, "storage.snapshot.expire", schema.EventSource{Class: "behaviour", Principal: "vms"}), "snapshot", snap)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		vmsJSON(w, 405, map[string]any{"detail": "Method not allowed."})
	}
}

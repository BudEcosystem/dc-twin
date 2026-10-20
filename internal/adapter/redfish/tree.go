package redfish

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// tree is a captured Redfish service (a DMTF mockup): resource path -> raw document. One tree
// is shared by every device of a dialect; per-device state is applied at render time.
type tree struct {
	dir  string
	docs map[string]string // resource path -> file, read on demand (the OS caches them)
	// folded maps a lower-cased path to the captured one: BMCs such as iLO resolve URIs
	// case-insensitively and link a resource with a case its capture did not store it under
	folded map[string]string
	xml    map[string][]byte
	mu     sync.Mutex
	hot    map[string][]byte
	// responses are captured answers to writes (patch_response.json, post_response.json):
	// status, headers and body the real BMC returned.
	responses map[string]capturedResponse
	orig      map[string]string // bound originals: "serial", "uuid", "bmcMAC" -> captured value
}

var (
	treeMu    sync.Mutex
	treeCache = map[string]*tree{}
)

func loadTree(dir string) (*tree, error) {
	treeMu.Lock()
	defer treeMu.Unlock()
	if t, ok := treeCache[dir]; ok {
		return t, nil
	}
	t := &tree{dir: dir, docs: map[string]string{}, xml: map[string][]byte{}, orig: map[string]string{}, hot: map[string][]byte{}, responses: map[string]capturedResponse{}, folded: map[string]string{}}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		base := filepath.Base(p)
		if base == "patch_response.json" || base == "post_response.json" {
			rel, _ := filepath.Rel(dir, filepath.Dir(p))
			if data, err := os.ReadFile(p); err == nil {
				var cr capturedResponse
				if json.Unmarshal(data, &cr) == nil {
					t.responses[strings.TrimSuffix(base, "_response.json")+" /"+filepath.ToSlash(rel)] = cr
				}
			}
			return nil
		}
		if base != "index.json" && base != "index.xml" {
			return nil
		}
		rel, _ := filepath.Rel(dir, filepath.Dir(p))
		key := "/" + filepath.ToSlash(rel)
		if base == "index.xml" {
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			t.xml[key] = data
		} else {
			t.docs[key] = p
			t.folded[strings.ToLower(key)] = key
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, ok := t.docs["/redfish/v1"]; !ok {
		return nil, fmt.Errorf("%s: not a Redfish mockup (no redfish/v1/index.json)", dir)
	}
	treeCache[dir] = t
	return t, nil
}

type capturedResponse struct {
	Match   string            `json:"match_request_body_contains"`
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// response returns the captured answer to a write, if the capture has one for this request: one
// recorded with match_request_body_contains is the BMC's answer to that body only (the Dell
// multi-DPU capture's is a 400 for a read-only attribute), so another body gets the twin's own
// semantics (for a settings object, the dialect's settingsJobs).
func (t *tree) response(method, path string, body []byte) (capturedResponse, bool) {
	cr, ok := t.responses[strings.ToLower(method)+" "+path]
	if !ok || (cr.Match != "" && !strings.Contains(string(body), cr.Match)) {
		return capturedResponse{}, false
	}
	return cr, true
}

func (t *tree) get(path string) (map[string]any, bool) {
	file, ok := t.docs[path]
	if !ok {
		if canon, found := t.folded[strings.ToLower(path)]; found {
			path, file, ok = canon, t.docs[canon], true
		}
	}
	if !ok {
		return nil, false
	}
	t.mu.Lock()
	raw, cached := t.hot[path]
	t.mu.Unlock()
	if !cached {
		var err error
		if raw, err = os.ReadFile(file); err != nil {
			return nil, false
		}
		t.mu.Lock()
		if len(t.hot) < 4096 {
			t.hot[path] = raw
		}
		t.mu.Unlock()
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}

// value reads "<path>#<dotted property>" from the tree.
func (t *tree) value(ref string) string {
	path, prop, ok := strings.Cut(ref, "#")
	if !ok {
		return ""
	}
	doc, ok := t.get(path)
	if !ok {
		return ""
	}
	v := getDotted(doc, prop)
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func getDotted(m map[string]any, prop string) any {
	cur := any(m)
	for _, p := range strings.Split(prop, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

// setDotted sets a dotted property, creating maps; when onlyIfPresent, it changes nothing that
// does not already exist.
func setDotted(m map[string]any, prop string, v any, onlyIfPresent bool) {
	parts := strings.Split(prop, ".")
	cur := m
	for i, p := range parts {
		if i == len(parts)-1 {
			if _, ok := cur[p]; ok || !onlyIfPresent {
				cur[p] = v
			}
			return
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			if onlyIfPresent {
				return
			}
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
}

// matchPath matches a resource path against a pattern in which `*` matches one segment or part
// of one, and {i}/{n} stand for a component index (0-based / 1-based).
func matchPath(pattern, path string) (bool, int) {
	ps, xs := strings.Split(pattern, "/"), strings.Split(path, "/")
	if strings.HasSuffix(pattern, "**") {
		prefix := strings.TrimSuffix(pattern, "**")
		return strings.HasPrefix(path, prefix), -1
	}
	if len(ps) != len(xs) {
		return false, -1
	}
	idx := -1
	for i := range ps {
		p, x := ps[i], xs[i]
		if strings.Contains(p, "{i}") || strings.Contains(p, "{n}") {
			pre, post, one := p, "", 0
			if j := strings.Index(p, "{i}"); j >= 0 {
				pre, post = p[:j], p[j+3:]
			} else {
				j := strings.Index(p, "{n}")
				pre, post, one = p[:j], p[j+3:], 1
			}
			if !strings.HasPrefix(x, pre) || !strings.HasSuffix(x, post) || len(x) <= len(pre)+len(post) {
				return false, -1
			}
			var n int
			if _, err := fmt.Sscanf(x[len(pre):len(x)-len(post)], "%d", &n); err != nil {
				return false, -1
			}
			idx = n - one
			continue
		}
		if !globSeg(p, x) {
			return false, -1
		}
	}
	return true, idx
}

func globSeg(p, s string) bool {
	if p == "*" {
		return true
	}
	parts := strings.Split(p, "*")
	if len(parts) == 1 {
		return p == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		k := strings.Index(s, parts[i])
		if k < 0 {
			return false
		}
		s = s[k+len(parts[i]):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

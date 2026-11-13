package nvcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	yaml "go.yaml.in/yaml/v3"

	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
)

// errShown is an error already printed.
var errShown = errors.New("shown")

func (c *client) run(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(c.out, usage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	var err error
	switch args[0] {
	case "set":
		err = c.set(args[1:], true)
	case "unset":
		err = c.set(args[1:], false)
	case "show":
		err = c.show(args[1:])
	case "config":
		if len(args) < 2 {
			fmt.Fprint(c.errw, usage)
			return 2
		}
		err = c.config(args[1], args[2:])
	case "action":
		err = c.action(args[1:])
	default:
		err = fmt.Errorf("nv %s: not emulated by the twin's nv (set, unset, show, config, action)", args[0])
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errShown):
		return 1
	default:
		fmt.Fprintf(c.errw, "Error: %v\n", err)
		return 1
	}
}

// opts are a command's flags, taken out of its words wherever they are.
type opts struct {
	output, rev string
	yes, no     bool
	words       []string
}

func parse(args []string, valued map[string]string, bools map[string]*bool) (opts, error) {
	var o opts
	for i := 0; i < len(args); i++ {
		a := args[i]
		if name, val, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "--") {
			if k, known := valued[name]; known {
				o.set(k, val)
				continue
			}
		}
		if k, ok := valued[a]; ok {
			if i+1 >= len(args) {
				return o, fmt.Errorf("%s takes a value", a)
			}
			o.set(k, args[i+1])
			i++
			continue
		}
		if b, ok := bools[a]; ok {
			*b = true
			continue
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			return o, fmt.Errorf("%s: not an option the twin's nv has", a)
		}
		o.words = append(o.words, a)
	}
	return o, nil
}

func (o *opts) set(k, v string) {
	if k == "output" {
		o.output = v
	} else {
		o.rev = v
	}
}

// set is `nv set` and `nv unset`: each interface of a range is a change in the user's pending
// revision, made in it at once, as NVUE stages it.
func (c *client) set(words []string, set bool) error {
	verb := "unset"
	if set {
		verb = "set"
	}
	if len(words) == 0 {
		return fmt.Errorf("nv %s: a path", verb)
	}
	for _, w := range words {
		if w == "-h" || w == "--help" {
			fmt.Fprintf(c.out, "usage: nv %s <path> [<value>]\n", verb)
			return nil
		}
	}
	all := expand(words)
	type op struct {
		path []string
		v    any
	}
	var ops []op
	for _, w := range all {
		path, v, err := nvue.CLIPath(w, set)
		if err != nil {
			return err
		}
		ops = append(ops, op{path, v})
	}
	id, err := c.pending(true)
	if err != nil {
		return err
	}
	for _, o := range ops {
		if set {
			_, err = c.call(http.MethodPatch, o.path, url.Values{"rev": {id}}, o.v)
		} else {
			_, err = c.call(http.MethodDelete, o.path, url.Values{"rev": {id}}, nil)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

var rangeRE = regexp.MustCompile(`^(.*[a-z])([0-9]+)-([0-9]+)$`)

// expand takes an interface name's ranges and lists (swp1-4, swp1s0-3, swp1,swp3) to one path
// per interface, as NVUE's CLI does.
func expand(words []string) [][]string {
	if len(words) < 2 || words[0] != "interface" {
		return [][]string{words}
	}
	var names []string
	for _, part := range strings.Split(words[1], ",") {
		m := rangeRE.FindStringSubmatch(part)
		if m == nil {
			names = append(names, part)
			continue
		}
		a, _ := strconv.Atoi(m[2])
		b, _ := strconv.Atoi(m[3])
		if b < a || b-a > 512 {
			names = append(names, part)
			continue
		}
		for i := a; i <= b; i++ {
			names = append(names, m[1]+strconv.Itoa(i))
		}
	}
	var out [][]string
	for _, n := range names {
		w := append([]string{"interface", n}, words[2:]...)
		out = append(out, w)
	}
	return out
}

// show is `nv show`: the tree at a path, applied unless a revision is asked for, with the
// switch's operational state where NVUE serves it (a VRF's BGP, the system).
func (c *client) show(args []string) error {
	var pending, applied, operational bool
	o, err := parse(args, map[string]string{"-o": "output", "--output": "output", "--rev": "rev", "-r": "rev"},
		map[string]*bool{"--pending": &pending, "--applied": &applied, "--operational": &operational})
	if err != nil {
		return err
	}
	rev := o.rev
	switch {
	case pending:
		rev = "pending"
	case applied:
		rev = "applied"
	case operational:
		rev = "operational"
	}
	var path []string
	if len(o.words) > 0 {
		var err error
		if w := o.words; len(w) >= 3 && w[0] == "interface" && w[2] == "lldp" || w[0] == "platform" {
			// a port's LLDP neighbours, the platform: operational state, which NVUE serves and
			// its configuration schema does not name
			path = w
		} else if path, _, err = nvue.CLIPath(o.words, false); err != nil {
			return err
		}
	}
	q := url.Values{}
	if rev == "pending" {
		id, err := c.pending(false)
		if err != nil {
			return err
		}
		rev = id // none pending: what is applied
	}
	if rev != "" {
		q.Set("rev", rev)
	}
	b, err := c.call(http.MethodGet, path, q, nil)
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("NVUE answered: %v", err)
	}
	if o.output == "" || o.output == "auto" {
		// NVUE's table: what the switch runs beside what is applied (a named revision's in
		// place of applied when one is asked for)
		var op any
		if b, err := c.call(http.MethodGet, path, url.Values{"rev": {"operational"}}, nil); err == nil {
			_ = json.Unmarshal(b, &op)
		}
		head := "applied"
		if rev != "" && rev != "applied" && rev != "operational" {
			head = "pending"
		}
		return c.table(op, v, head)
	}
	return c.print(v, o.output)
}

// table is `nv show`'s default output: a row per attribute of the object shown, with the switch's
// operational value and the applied (or pending) one, as Cumulus's columns are; an attribute that
// is an object of named entries (addresses, servers, neighbours) shows their names, one per row.
// The columns are Cumulus's; the layout of the rows is the twin's (UNCONFIRMED against a switch).
func (c *client) table(op, cfg any, head string) error {
	om, _ := op.(map[string]any)
	cm, ok := cfg.(map[string]any)
	if !ok {
		fmt.Fprintf(c.out, "%v\n", cfg)
		return nil
	}
	keys := map[string]bool{}
	for k := range cm {
		keys[k] = true
	}
	for k := range om {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	cell := func(v any) []string {
		switch v := v.(type) {
		case nil:
			return []string{""}
		case map[string]any:
			if len(v) == 0 {
				return []string{""}
			}
			var out []string
			for k, x := range v {
				if sub, ok := x.(map[string]any); ok && len(sub) > 0 {
					out = append(out, k+" [+]")
				} else {
					out = append(out, k)
				}
			}
			sort.Strings(out)
			return out
		default:
			return []string{fmt.Sprint(v)}
		}
	}
	type row struct{ k, o, a string }
	var rows []row
	w1 := len("operational")
	for _, k := range names {
		o, a := cell(om[k]), cell(cm[k])
		for i := 0; i < len(o) || i < len(a); i++ {
			r := row{}
			if i == 0 {
				r.k = k
			}
			if i < len(o) {
				r.o = o[i]
			}
			if i < len(a) {
				r.a = a[i]
			}
			rows = append(rows, r)
			w1 = max(w1, len(r.o))
		}
	}
	kw := 0
	for _, r := range rows {
		kw = max(kw, len(r.k))
	}
	fmt.Fprintf(c.out, "%-*s  %-*s  %s\n", kw, "", w1, "operational", head)
	fmt.Fprintf(c.out, "%s  %s  %s\n", strings.Repeat("-", kw), strings.Repeat("-", w1), strings.Repeat("-", max(len(head), 7)))
	for _, r := range rows {
		fmt.Fprintf(c.out, "%-*s  %-*s  %s\n", kw, r.k, w1, r.o, r.a)
	}
	return nil
}

// print writes a value as -o asks: json (NVUE's, indented), yaml, or auto (YAML here).
func (c *client) print(v any, output string) error {
	switch output {
	case "json":
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Fprintf(c.out, "%s\n", b)
	case "", "auto", "yaml":
		b, err := yaml.Marshal(nvue.Yamlable(v))
		if err != nil {
			return err
		}
		c.out.Write(b)
	default:
		return fmt.Errorf("-o %s: the twin's nv prints json, yaml and auto (a table)", output)
	}
	return nil
}

func (c *client) config(cmd string, args []string) error {
	switch cmd {
	case "apply":
		return c.apply(args)
	case "diff":
		return c.diff(args)
	case "replace", "patch":
		o, err := parse(args, nil, nil)
		if err != nil {
			return err
		}
		if len(o.words) != 1 {
			return fmt.Errorf("nv config %s <file>", cmd)
		}
		return c.file(o.words[0], cmd == "replace")
	case "confirm":
		return c.confirm()
	case "save":
		_, err := c.call(http.MethodPost, []string{"dctwin", "save"}, nil, nil)
		return err
	case "show":
		return c.configShow(args)
	case "history":
		return c.history(args)
	case "detach":
		b, err := c.call(http.MethodPost, []string{"dctwin", "detach"}, nil, nil)
		if err != nil {
			return err
		}
		for id := range revs(b) {
			fmt.Fprintf(c.out, "detached [rev_id: %s]\n", id)
		}
		return nil
	case "revision":
		b, err := c.call(http.MethodGet, []string{"revision"}, nil, nil)
		if err != nil {
			return err
		}
		r := revs(b)
		ids := sortedIDs(r)
		fmt.Fprintf(c.out, "%-8s %s\n", "rev-id", "state")
		for _, id := range ids {
			fmt.Fprintf(c.out, "%-8s %v\n", id, r[id]["state"])
		}
		return nil
	}
	return fmt.Errorf("nv config %s: not emulated by the twin's nv (apply, diff, replace, patch, save, show, history, detach, revision)", cmd)
}

// apply is `nv config apply [<rev>|applied|startup|empty]`. The twin's NVUE never prompts, so
// --y, -y, --assume-yes and --assume-no all apply. --confirm [<time>] applies with NVUE's
// commit-confirm: the configuration before it comes back unless the apply is confirmed within the
// time (10 minutes when none is given), which --confirm-yes, or `nv config confirm`, does.
func (c *client) apply(args []string) error {
	var yes, ayes, no, confirm, confirmYes bool
	o, err := parse(args, nil, map[string]*bool{"-y": &yes, "--y": &yes, "--assume-yes": &ayes, "--assume-no": &no,
		"--confirm": &confirm, "--confirm-yes": &confirmYes})
	if err != nil {
		return err
	}
	if confirmYes {
		return c.confirm()
	}
	window := 0
	if confirm {
		window = 600
		if len(o.words) > 0 && regexp.MustCompile(`^[0-9]+[smh]?$`).MatchString(o.words[0]) {
			d, err := time.ParseDuration(o.words[0] + map[bool]string{true: "s", false: ""}[regexp.MustCompile(`^[0-9]+$`).MatchString(o.words[0])])
			if err != nil || d < time.Second {
				return fmt.Errorf("nv config apply --confirm %s: a time (600, 90s, 10m)", o.words[0])
			}
			window = int(d.Seconds())
			o.words = o.words[1:]
		}
	}
	if len(o.words) > 1 {
		return fmt.Errorf("nv config apply [<rev>]")
	}
	target := ""
	if len(o.words) == 1 {
		target = o.words[0]
	}
	var id string
	switch target {
	case "":
		if id, err = c.pending(false); err != nil {
			return err
		}
		if id == "" {
			fmt.Fprintln(c.out, "Warning: there are no pending changes to apply")
			return nil
		}
	case "applied", "startup", "empty":
		var t any = map[string]any{}
		if target != "empty" {
			b, err := c.call(http.MethodGet, nil, url.Values{"rev": {target}}, nil)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(b, &t); err != nil {
				return err
			}
		}
		if id, err = c.replace(t); err != nil {
			return err
		}
	default:
		if _, err := strconv.Atoi(target); err != nil {
			return fmt.Errorf("nv config apply %s: a revision is a number, applied, startup or empty", target)
		}
		id = target
	}
	body := map[string]any{"state": "apply", "auto-prompt": map[string]any{"ays": "ays_yes"}}
	if window > 0 {
		body["state-controls"] = map[string]any{"confirm": window}
	}
	b, err := c.call(http.MethodPatch, []string{"revision", id}, nil, body)
	if err != nil {
		return err
	}
	var st struct {
		State      string `json:"state"`
		Transition struct {
			Issue    map[string]struct{ Message string } `json:"issue"`
			Progress string                              `json:"progress"`
		} `json:"transition"`
	}
	_ = json.Unmarshal(b, &st)
	switch st.State {
	case "applied":
		fmt.Fprintf(c.out, "applied [rev_id: %s]\n", id)
		return nil
	case "ays":
		fmt.Fprintf(c.out, "applied [rev_id: %s]: confirm within %d s (nv config confirm), or it is rolled back\n", id, window)
		return nil
	}
	fmt.Fprintf(c.errw, "Error: revision %s: %s\n", id, st.State)
	for _, k := range sortedKeys(st.Transition.Issue) {
		fmt.Fprintf(c.errw, "  %s\n", st.Transition.Issue[k].Message)
	}
	return errShown
}

// replace makes the user's pending revision a tree, whole: everything removed, then the tree
// merged.
// confirm keeps the revision a commit-confirm applied (`nv config confirm`, `nv config apply
// --confirm-yes`).
func (c *client) confirm() error {
	b, err := c.call(http.MethodGet, []string{"revision"}, nil, nil)
	if err != nil {
		return err
	}
	for _, id := range sortedIDs(revs(b)) {
		if revs(b)[id]["state"] != "ays" {
			continue
		}
		if _, err := c.call(http.MethodPatch, []string{"revision", id}, nil, map[string]any{"state": "apply", "auto-prompt": map[string]any{"ays": "ays_yes"}}); err != nil {
			return err
		}
		fmt.Fprintf(c.out, "confirmed [rev_id: %s]\n", id)
		return nil
	}
	return fmt.Errorf("no apply is waiting for its confirmation")
}

func (c *client) replace(t any) (string, error) {
	id, err := c.pending(true)
	if err != nil {
		return "", err
	}
	if _, err := c.call(http.MethodDelete, nil, url.Values{"rev": {id}}, nil); err != nil {
		return "", err
	}
	if _, err := c.call(http.MethodPatch, nil, url.Values{"rev": {id}}, t); err != nil {
		return "", err
	}
	return id, nil
}

// diff is `nv config diff`: what the target (the pending revision) changes from the base (the
// applied configuration), in the form of a startup file: a set of what it adds or changes, an
// unset of what it removes (null where a value goes).
func (c *client) diff(args []string) error {
	o, err := parse(args, map[string]string{"-o": "output", "--output": "output"}, nil)
	if err != nil {
		return err
	}
	base, target := "applied", ""
	switch len(o.words) {
	case 0:
	case 1:
		base = o.words[0]
	case 2:
		base, target = o.words[0], o.words[1]
	default:
		return fmt.Errorf("nv config diff [<base> [<target>]]")
	}
	if base == "pending" || target == "" || target == "pending" {
		id, err := c.pending(false)
		if err != nil {
			return err
		}
		if id == "" {
			id = "applied"
		}
		if base == "pending" {
			base = id
		}
		if target == "" || target == "pending" {
			target = id
		}
	}
	b, err := c.call(http.MethodGet, nil, url.Values{"diff": {base}, "rev": {target}}, nil)
	if err != nil {
		return err
	}
	var patch map[string]any
	if err := json.Unmarshal(b, &patch); err != nil {
		return err
	}
	if o.output == "commands" {
		for _, l := range nvue.Commands(patch) {
			fmt.Fprintln(c.out, l)
		}
		return nil
	}
	set, unset := split(patch)
	var doc []any
	if len(set) > 0 {
		doc = append(doc, map[string]any{"set": set})
	}
	if len(unset) > 0 {
		doc = append(doc, map[string]any{"unset": unset})
	}
	if doc == nil {
		doc = []any{}
	}
	return c.print(doc, o.output)
}

// split takes a merge patch apart: what it sets, and what it removes (its nulls).
func split(p map[string]any) (set, unset map[string]any) {
	set, unset = map[string]any{}, map[string]any{}
	for k, v := range p {
		switch x := v.(type) {
		case nil:
			unset[k] = nil
		case map[string]any:
			s, u := split(x)
			if len(s) > 0 || len(x) == 0 {
				set[k] = s
			}
			if len(u) > 0 {
				unset[k] = u
			}
		default:
			set[k] = v
		}
	}
	return set, unset
}

// configShow is `nv config show`: the applied configuration (or a revision's) as startup.yaml
// has it, as JSON of the same list, or as nv set lines.
func (c *client) configShow(args []string) error {
	var all bool
	o, err := parse(args, map[string]string{"-o": "output", "--output": "output", "-r": "rev", "--rev": "rev"}, map[string]*bool{"--all": &all})
	if err != nil {
		return err
	}
	q := url.Values{}
	if o.rev == "pending" {
		if o.rev, err = c.pending(false); err != nil {
			return err
		}
	}
	if o.rev != "" {
		q.Set("rev", o.rev)
	}
	b, err := c.call(http.MethodGet, nil, q, nil)
	if err != nil {
		return err
	}
	var t map[string]any
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	build := nvue.DefaultBuild
	if sb, err := c.call(http.MethodGet, []string{"system"}, nil, nil); err == nil {
		var sys map[string]any
		if json.Unmarshal(sb, &sys) == nil {
			if v, ok := sys["build"].(string); ok && v != "" {
				build = v
			}
		}
	}
	switch o.output {
	case "commands":
		for _, l := range nvue.Commands(t) {
			fmt.Fprintln(c.out, l)
		}
		return nil
	case "json":
		doc := []any{map[string]any{"header": map[string]any{"model": "dctwin", "nvue-api-version": "nvue_v1", "rev-id": 1.0, "version": build}}, map[string]any{"set": t}}
		return c.print(doc, "json")
	case "", "auto", "yaml":
		y, err := nvue.StartupYAML(t, build)
		if err != nil {
			return err
		}
		c.out.Write(y)
		return nil
	}
	return fmt.Errorf("-o %s: nv config show prints yaml, json and commands", o.output)
}

func (c *client) history(args []string) error {
	o, err := parse(args, map[string]string{"-o": "output", "--output": "output"}, nil)
	if err != nil {
		return err
	}
	b, err := c.call(http.MethodGet, []string{"dctwin", "history"}, nil, nil)
	if err != nil {
		return err
	}
	var hs []map[string]any
	if err := json.Unmarshal(b, &hs); err != nil {
		return err
	}
	if len(o.words) == 1 {
		var keep []map[string]any
		for _, h := range hs {
			if fmt.Sprint(h["rev-id"]) == o.words[0] {
				keep = append(keep, h)
			}
		}
		hs = keep
	}
	if o.output == "json" || o.output == "yaml" {
		return c.print(hs, o.output)
	}
	fmt.Fprintf(c.out, "%-8s %-20s %-12s %-6s %s\n", "rev-id", "date", "user", "type", "state")
	for _, h := range hs {
		fmt.Fprintf(c.out, "%-8v %-20v %-12v %-6v %v\n", h["rev-id"], h["date"], h["user"], h["type"], h["state"])
	}
	return nil
}

// file is `nv config replace` and `nv config patch` with a file: Cumulus's startup.yaml form (a
// list of set and unset, its header ignored), a configuration tree as YAML or JSON, or (since
// Cumulus Linux 5.13) nv set and nv unset lines.
func (c *client) file(name string, replace bool) error {
	b, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	ops, err := readConfig(b)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if replace {
		t := nvue.Tree{}
		for _, o := range ops {
			if o.del {
				nvue.Delete(t, o.path)
			} else {
				nvue.Patch(t, o.path, o.v)
			}
		}
		if err := nvue.Validate(nil, t); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		_, err := c.replace(t)
		return err
	}
	id, err := c.pending(true)
	if err != nil {
		return err
	}
	// a file is one change to the revision, as NVUE takes it: its sets and unsets as one merge
	// patch (null removes), unless an unset is followed by a set under the same path, which a
	// merge cannot say
	if doc, ok := mergeOps(ops); ok {
		_, err = c.call(http.MethodPatch, nil, url.Values{"rev": {id}}, doc)
		return err
	}
	for _, o := range ops {
		if o.del {
			_, err = c.call(http.MethodDelete, o.path, url.Values{"rev": {id}}, nil)
		} else {
			_, err = c.call(http.MethodPatch, o.path, url.Values{"rev": {id}}, o.v)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// mergeOps is a file's changes as one merge patch from the root, null where a removal is; false
// when a set lands at or under a path an earlier unset removed (the merge would keep what the
// unset took away).
func mergeOps(ops []op) (map[string]any, bool) {
	doc := map[string]any{}
	var removed [][]string
	for _, o := range ops {
		if o.del {
			if len(o.path) == 0 {
				return nil, false
			}
			if under(o.path, removed) {
				continue // its ancestor is already removed
			}
			nvue.Patch(doc, o.path[:len(o.path)-1], map[string]any{})
			parent := doc
			for _, p := range o.path[:len(o.path)-1] {
				parent = parent[p].(map[string]any)
			}
			parent[o.path[len(o.path)-1]] = nil
			removed = append(removed, o.path)
			continue
		}
		if !o.setLeaves(removed) {
			return nil, false
		}
		if len(o.path) == 0 {
			m, ok := o.v.(map[string]any)
			if !ok {
				return nil, false
			}
			nvue.Patch(doc, nil, m)
		} else {
			nvue.Patch(doc, o.path, o.v)
		}
	}
	return doc, true
}

// setLeaves reports whether a set touches nothing an earlier unset removed.
func (o op) setLeaves(removed [][]string) bool {
	for _, p := range leaves(o.path, o.v) {
		if under(p, removed) {
			return false
		}
	}
	return len(o.path) == 0 || !under(o.path, removed)
}

// under reports whether a path is at or below one of the paths given.
func under(p []string, of [][]string) bool {
	for _, r := range of {
		if len(p) >= len(r) && strings.Join(p[:len(r)], "\x00") == strings.Join(r, "\x00") {
			return true
		}
	}
	return false
}

// op is one change a file makes: a merge at a path, or a removal.
type op struct {
	path []string
	v    any
	del  bool
}

func readConfig(b []byte) ([]op, error) {
	text := strings.TrimSpace(string(b))
	if isCommands(text) {
		var ops []op
		for i, l := range strings.Split(text, "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			w, err := shellWords(l)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			if len(w) < 3 || w[0] != "nv" || (w[1] != "set" && w[1] != "unset") {
				return nil, fmt.Errorf("line %d: nv set or nv unset, not %q", i+1, l)
			}
			for _, words := range expand(w[2:]) {
				path, v, err := nvue.CLIPath(words, w[1] == "set")
				if err != nil {
					return nil, fmt.Errorf("line %d: %w", i+1, err)
				}
				ops = append(ops, op{path: path, v: v, del: w[1] == "unset"})
			}
		}
		return ops, nil
	}
	var doc any
	if err := yaml.Unmarshal(b, &doc); err != nil { // JSON is YAML
		return nil, err
	}
	doc = jsonable(doc)
	switch d := doc.(type) {
	case map[string]any:
		return []op{{v: d}}, nil
	case []any:
		var ops []op
		for _, e := range d {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("a startup file is a list of header, set and unset")
			}
			for _, k := range sortedAny(m) {
				switch k {
				case "header":
				case "set":
					s, ok := m[k].(map[string]any)
					if !ok {
						return nil, fmt.Errorf("set: an object")
					}
					ops = append(ops, op{v: s})
				case "unset":
					for _, p := range leaves(nil, m[k]) {
						ops = append(ops, op{path: p, del: true})
					}
				default:
					return nil, fmt.Errorf("%s: a startup file has header, set and unset", k)
				}
			}
		}
		return ops, nil
	case nil:
		return nil, nil
	}
	return nil, fmt.Errorf("not a configuration")
}

func isCommands(text string) bool {
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		return strings.HasPrefix(l, "nv ")
	}
	return false
}

// leaves is the paths an unset's tree names: each value, null or empty object at its end.
func leaves(path []string, v any) [][]string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		if len(path) == 0 {
			return nil
		}
		return [][]string{path}
	}
	var out [][]string
	for _, k := range sortedAny(m) {
		out = append(out, leaves(append(append([]string{}, path...), k), m[k])...)
	}
	return out
}

// jsonable is a YAML document as JSON has it: string keys, numbers as float64.
func jsonable(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, c := range x {
			out[k] = jsonable(c)
		}
		return out
	case map[any]any:
		out := map[string]any{}
		for k, c := range x {
			out[fmt.Sprint(k)] = jsonable(c)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, c := range x {
			out[i] = jsonable(c)
		}
		return out
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case bool:
		// YAML's true and false are NVUE's on and off where a file writes them so
		if x {
			return "on"
		}
		return "off"
	}
	return v
}

// shellWords splits a line as sh does for quotes and backslashes (no expansion).
func shellWords(l string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in, quote, esc := false, rune(0), false
	for _, r := range l {
		switch {
		case esc:
			cur.WriteRune(r)
			esc, in = false, true
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				esc = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			esc = true
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 || esc {
		return nil, fmt.Errorf("an unterminated quote")
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}

// pending is the user's pending revision's id; create: made when it has none, otherwise ""
// then.
func (c *client) pending(create bool) (string, error) {
	method := http.MethodGet
	if create {
		method = http.MethodPost
	}
	b, err := c.call(method, []string{"dctwin", "pending"}, nil, nil)
	if err != nil {
		return "", err
	}
	for id := range revs(b) {
		return id, nil
	}
	return "", nil
}

func revs(b []byte) map[string]map[string]any {
	var m map[string]map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// call makes one request of NVUE as the invoking user, by the local trust path. An error
// carries NVUE's own words (its problem's detail).
func (c *client) call(method string, path []string, q url.Values, body any) ([]byte, error) {
	var segs []string
	for _, p := range path {
		segs = append(segs, url.PathEscape(p))
	}
	u := c.base + "/" + strings.Join(segs, "/")
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(nvue.HeaderNvUser, c.user)
	req.Header.Set(nvue.HeaderNvToken, c.token)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("NVUE is not answering at %s: %v", c.base, err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode/100 != 2 {
		var p struct {
			Detail string `json:"detail"`
			Title  string `json:"title"`
		}
		if json.Unmarshal(b, &p) == nil && p.Detail != "" {
			return nil, errors.New(p.Detail)
		}
		return nil, fmt.Errorf("NVUE: %s", res.Status)
	}
	return b, nil
}

func sortedIDs(m map[string]map[string]any) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, _ := strconv.Atoi(ids[i])
		b, _ := strconv.Atoi(ids[j])
		return a < b
	})
	return ids
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedAny(m map[string]any) []string { return sortedKeys(m) }

// action is `nv action`: of NVUE's actions, the twin's nv carries out `fetch system` (NVIDIA Config
// Manager's boot script fetches its startup.yaml with it from 5.16): `nv action fetch system
// file-path <path> uri <uri> [file-permissions <octal>] [vrf <vrf>]`, the file fetched over HTTP,
// HTTPS or SFTP (Cumulus's schemes) by the switch's curl, in the VRF when one is given (through the
// twin's ip vrf exec, clsys), and written with the permissions (0644 when none).
func (c *client) action(args []string) error {
	if len(args) >= 2 && args[0] == "reboot" && args[1] == "system" {
		// the switch's reboot (clsys's, which the twin carries out)
		for _, a := range args[2:] {
			if a != "no-confirm" && a != "force" {
				return fmt.Errorf("nv action reboot system %s: not emulated (no-confirm, force)", a)
			}
		}
		cmd := exec.Command("reboot")
		cmd.Stdout, cmd.Stderr = c.out, c.errw
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("reboot: %v", err)
		}
		return nil
	}
	if len(args) < 2 || args[0] != "fetch" || args[1] != "system" {
		return fmt.Errorf("nv action %s: not emulated by the twin's nv (fetch system, reboot system)", strings.Join(args, " "))
	}
	kv := map[string]string{}
	rest := args[2:]
	for i := 0; i < len(rest); i += 2 {
		switch rest[i] {
		case "file-path", "uri", "file-permissions", "vrf":
		default:
			return fmt.Errorf("nv action fetch system: %q is not file-path, uri, file-permissions or vrf", rest[i])
		}
		if i+1 >= len(rest) {
			return fmt.Errorf("nv action fetch system %s: a value is missing", rest[i])
		}
		kv[rest[i]] = rest[i+1]
	}
	path, uri := kv["file-path"], kv["uri"]
	if path == "" || uri == "" {
		return fmt.Errorf("nv action fetch system: file-path and uri are needed")
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("nv action fetch system file-path %s: an absolute path", path)
	}
	u, err := url.Parse(uri)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "sftp" && u.Scheme != "scp") || u.Host == "" {
		return fmt.Errorf("nv action fetch system uri %s: an http, https, sftp or scp URI", uri)
	}
	perm := os.FileMode(0o644)
	if p := kv["file-permissions"]; p != "" {
		n, err := strconv.ParseUint(p, 8, 32)
		if err != nil || n > 0o777 {
			return fmt.Errorf("nv action fetch system file-permissions %s: octal permissions", p)
		}
		perm = os.FileMode(n)
	}
	argv := []string{"curl", "--fail", "--silent", "--show-error", "--insecure", "--output", path + ".nvfetch", uri}
	if v := kv["vrf"]; v != "" && v != "default" {
		argv = append([]string{c.ipCmd, "vrf", "exec", v}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = c.errw, c.errw
	if err := cmd.Run(); err != nil {
		_ = os.Remove(path + ".nvfetch")
		return fmt.Errorf("fetching %s: %v", uri, err)
	}
	if err := os.Chmod(path+".nvfetch", perm); err != nil {
		return err
	}
	if err := os.Rename(path+".nvfetch", path); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "Action succeeded\n")
	return nil
}

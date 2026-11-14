package nvue

import (
	"fmt"
	"strconv"
	"strings"
)

// CLIPath reads the words of an `nv set` or `nv unset` (D76; vendor facts C.2) against the
// emulated schema, as NVUE's CLI does: from the root, a word names a child of the node it is at,
// or, where the node is a collection (interfaces, VRFs, addresses), a member by name; a leaf takes
// the word after it as its value. `nv set interface swp1 ip address 10.0.0.1/31` is the
// collection's member 10.0.0.1/31; `nv set router bgp autonomous-system 65101` the leaf's value.
//
// For a set, path is where the value merges (a PATCH at path) and value is an object of one key:
// the leaf with its value typed as NVUE's JSON has it (a number for a number, "on" or "off" for a
// switch), or the member or child set, {} (`nv set interface eth0 ip address dhcp` is PATCH
// /interface/eth0/ip/address {"dhcp": {}}). For an unset (and a show), path is the whole path and
// value nil; a value after a leaf is ignored, as `nv unset system hostname leaf01` unsets the
// hostname. A word the schema does not have is refused by name. Any release's grammar is taken
// (CLIPathAt reads one release's).
func CLIPath(words []string, set bool) (path []string, value any, err error) {
	return CLIPathAt(Version{}, words, set)
}

// CLIPathAt is CLIPath in a release's grammar: a word the release does not have (5.14's enable
// on a 5.15 switch, 5.15's state on an earlier one) is refused by name, with what to use.
func CLIPathAt(ver Version, words []string, set bool) (path []string, value any, err error) {
	n := schema
	var walked []string
	i := 0
	for ; i < len(words) && n.leaf == 0; i++ {
		c, err := n.childCLI(ver, words[i])
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", strings.Join(append(walked, words[i]), " "), err)
		}
		walked = append(walked, words[i])
		n = c
	}
	rest := words[i:]
	if !set {
		if len(rest) > 1 {
			return nil, nil, fmt.Errorf("%s: one value after %s, not %d", strings.Join(words, " "), strings.Join(walked, " "), len(rest))
		}
		return walked, nil, nil
	}
	if len(walked) == 0 {
		return nil, nil, fmt.Errorf("nothing to set")
	}
	last := walked[len(walked)-1]
	parent := walked[:len(walked)-1]
	if n.leaf == 0 {
		return parent, map[string]any{last: map[string]any{}}, nil
	}
	switch len(rest) {
	case 0:
		return nil, nil, fmt.Errorf("%s: a value is missing", strings.Join(walked, " "))
	case 1:
	default:
		return nil, nil, fmt.Errorf("%s: one value, not %d (quote a value with spaces)", strings.Join(walked, " "), len(rest))
	}
	v := typed(n, rest[0])
	if err := ValidateAt(ver, walked, v); err != nil {
		return nil, nil, err
	}
	return parent, map[string]any{last: v}, nil
}

// typed is a leaf's value as NVUE's JSON holds it: a number for a number (an AS number when it
// is one), a string otherwise ("on", "off", "external").
func typed(n *node, s string) any {
	switch n.leaf {
	case kInt:
		if f, err := strconv.ParseUint(s, 10, 32); err == nil {
			return float64(f)
		}
	case kASN:
		if f, err := strconv.ParseUint(s, 10, 32); err == nil {
			return float64(f)
		}
	}
	return s
}

// Commands is a tree, or a merge patch (a diff, whose nulls are what it removes), as the `nv
// set` and `nv unset` lines that make it: `nv config show -o commands` and `nv config diff -o
// commands`. A leaf is one line with its value; an object with nothing in it (an address, a
// member of a collection) is a line ending at its name.
func Commands(t any) []string {
	var out []string
	var walk func(n *node, path []string, v any)
	walk = func(n *node, path []string, v any) {
		if v == nil {
			if len(path) > 0 {
				out = append(out, "nv unset "+cliWords(path))
			}
			return
		}
		m, isObj := v.(map[string]any)
		if !isObj || (n != nil && n.leaf != 0) {
			out = append(out, "nv set "+cliWords(path)+" "+cliWord(str(v)))
			return
		}
		if len(m) == 0 {
			if len(path) > 0 {
				out = append(out, "nv set "+cliWords(path))
			}
			return
		}
		for _, k := range keys(m) {
			var c *node
			if n != nil {
				c, _ = n.child(k)
			}
			walk(c, append(append([]string{}, path...), k), m[k])
		}
	}
	walk(schema, nil, t)
	return out
}

func cliWords(path []string) string {
	q := make([]string, len(path))
	for i, p := range path {
		q[i] = cliWord(p)
	}
	return strings.Join(q, " ")
}

// cliWord quotes a word for a shell when it needs it.
func cliWord(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`*?[]{}()<>|&;#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// withChoices makes a change to a choice (link state, an ACL rule's action) one of its
// alternatives: setting up removes down, as NVUE's enumeration does. A change at a choice's
// alternative itself (PATCH .../link/state/up {}) is made at the choice. It returns the path and
// value to merge.
func withChoices(path []string, v any) ([]string, any) {
	if v == nil {
		return path, v
	}
	n := schema
	var parent *node
	for _, p := range path {
		c, err := n.child(p)
		if err != nil {
			return path, v
		}
		parent, n = n, c
	}
	if parent != nil && parent.choice && len(path) > 0 {
		path, v = path[:len(path)-1], map[string]any{path[len(path)-1]: v}
		n = parent
	}
	return path, choices(n, v)
}

func choices(n *node, v any) any {
	m, ok := v.(map[string]any)
	if !ok || n == nil || n.leaf != 0 {
		return v
	}
	out := map[string]any{}
	for k, c := range m {
		out[k] = c
	}
	if n.choice {
		var set []string
		for k, c := range m {
			if c != nil {
				set = append(set, k)
			}
		}
		if len(set) == 1 {
			for alt := range n.kids {
				if alt != set[0] {
					out[alt] = nil
				}
			}
		}
		return out
	}
	for k, c := range m {
		if cn, err := n.child(k); err == nil {
			out[k] = choices(cn, c)
		}
	}
	return out
}

// hashPasswords takes the plain passwords out of a change (`system aaa user <name> password`)
// and puts their hashes in (hashed-password), as NVUE keeps no plain password (vendor facts
// C.3). It returns the path and value to merge.
func hashPasswords(path []string, v any) ([]string, any) {
	if v == nil {
		return path, v
	}
	if len(path) == 5 && path[0] == "system" && path[1] == "aaa" && path[2] == "user" && path[4] == "password" {
		path, v = path[:4], map[string]any{"password": v}
	}
	return path, hashAt(path, v)
}

// hashAt rewrites the users' passwords in v, the value at path, keeping everything else in it
// (a null that unsets something) as it is.
func hashAt(path []string, v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	want := []string{"system", "aaa", "user"}
	if len(path) > 4 {
		return v
	}
	for i, p := range path {
		if i < 3 && p != want[i] {
			return v
		}
	}
	out := map[string]any{}
	for k, c := range m {
		out[k] = c
	}
	if len(path) == 4 {
		if p, ok := out["password"].(string); ok {
			delete(out, "password")
			out["hashed-password"] = HashPassword(p)
		}
		return out
	}
	for k, c := range m {
		if len(path) < 3 && k != want[len(path)] {
			continue
		}
		out[k] = hashAt(append(append([]string{}, path...), k), c)
	}
	return out
}

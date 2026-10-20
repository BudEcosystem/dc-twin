// Package selector implements the path and selector grammar of docs/spec/03-estate-model.md:
// brace lists and padded ranges, `*` within a segment, `**` across segments, suffix anchoring on
// a segment boundary, and `?key=value&…` attribute filters. There are no character classes.
package selector

import (
	"fmt"
	"strconv"
	"strings"
)

// Expand performs brace expansion: "a{1..3}b{x,y}" yields six strings. Ranges keep zero
// padding when either bound is written with a leading zero.
func Expand(s string) ([]string, error) {
	open := strings.IndexByte(s, '{')
	if open < 0 {
		if strings.ContainsAny(s, "}") {
			return nil, fmt.Errorf("unbalanced brace in %q", s)
		}
		return []string{s}, nil
	}
	depth, close := 0, -1
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				close = i
			}
		}
		if close >= 0 {
			break
		}
	}
	if close < 0 {
		return nil, fmt.Errorf("unbalanced brace in %q", s)
	}
	prefix, body, rest := s[:open], s[open+1:close], s[close+1:]
	var alts []string
	if a, b, ok := strings.Cut(body, ".."); ok && !strings.Contains(body, ",") {
		lo, err1 := strconv.Atoi(a)
		hi, err2 := strconv.Atoi(b)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("bad range {%s} in %q", body, s)
		}
		width := 0
		if (len(a) > 1 && a[0] == '0') || (len(b) > 1 && b[0] == '0') {
			width = max(len(a), len(b))
		}
		step := 1
		if hi < lo {
			step = -1
		}
		for i := lo; ; i += step {
			alts = append(alts, fmt.Sprintf("%0*d", width, i))
			if i == hi {
				break
			}
		}
	} else {
		alts = splitTop(body)
	}
	tails, err := Expand(rest)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, alt := range alts {
		heads, err := Expand(prefix + alt)
		if err != nil {
			return nil, err
		}
		for _, h := range heads {
			for _, t := range tails {
				out = append(out, h+t)
			}
		}
	}
	return out, nil
}

func splitTop(body string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, body[start:i])
				start = i + 1
			}
		}
	}
	return append(out, body[start:])
}

// Selector is a parsed selector.
type Selector struct {
	Raw      string
	patterns [][]string // each pattern's segments, with a leading "**" for suffix anchoring
	ports    []string   // port glob per pattern ("" when the pattern names devices)
	Filter   map[string]string
}

// Parse parses a selector. An empty selector matches everything.
func Parse(s string) (*Selector, error) {
	sel := &Selector{Raw: s, Filter: map[string]string{}}
	fields := strings.Fields(s)
	for _, f := range fields {
		if strings.HasPrefix(f, "?") {
			for _, kv := range strings.Split(f[1:], "&") {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return nil, fmt.Errorf("selector %q: bad filter %q", s, kv)
				}
				if prev, dup := sel.Filter[k]; dup && prev != v {
					// several filters on the same key are alternatives
					sel.Filter[k] = prev + "|" + v
					continue
				}
				sel.Filter[k] = v
			}
			continue
		}
		if strings.ContainsAny(f, "[]") {
			return nil, fmt.Errorf("selector %q: character classes are reserved; write 0{1..4}, not 0[1-4]", s)
		}
		exp, err := Expand(f)
		if err != nil {
			return nil, fmt.Errorf("selector %q: %w", s, err)
		}
		for _, p := range exp {
			path, port, _ := strings.Cut(p, ":")
			segs := append([]string{"**"}, strings.Split(strings.Trim(path, "/"), "/")...)
			sel.patterns = append(sel.patterns, segs)
			sel.ports = append(sel.ports, port)
		}
	}
	return sel, nil
}

// MustParse parses or panics; for literals in code.
func MustParse(s string) *Selector {
	sel, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return sel
}

// HasPatterns reports whether the selector names paths (a pure filter matches every path).
func (s *Selector) HasPatterns() bool { return len(s.patterns) > 0 }

// MatchPath reports whether a device or rack path matches any pattern (ignoring filters).
func (s *Selector) MatchPath(path string) bool {
	if len(s.patterns) == 0 {
		return true
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range s.patterns {
		if s.ports[i] != "" {
			continue
		}
		if matchSegs(p, segs) {
			return true
		}
	}
	return false
}

// MatchPort reports whether a port ref "path:port" matches.
func (s *Selector) MatchPort(ref string) bool {
	path, port, ok := strings.Cut(ref, ":")
	if !ok {
		return false
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for i, p := range s.patterns {
		if s.ports[i] == "" {
			continue
		}
		if matchSegs(p, segs) && Glob(s.ports[i], port) {
			return true
		}
	}
	return false
}

// MatchAttrs applies the filter. attr returns a device's value for a key.
func (s *Selector) MatchAttrs(attr func(key string) []string) bool {
	for k, want := range s.Filter {
		ok := false
		for _, alt := range strings.Split(want, "|") {
			for _, have := range attr(k) {
				if Glob(alt, have) {
					ok = true
				}
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func matchSegs(p, s []string) bool {
	if len(p) == 0 {
		return len(s) == 0
	}
	if p[0] == "**" {
		for i := 0; i <= len(s); i++ {
			if matchSegs(p[1:], s[i:]) {
				return true
			}
		}
		return false
	}
	if len(s) == 0 || !Glob(p[0], s[0]) {
		return false
	}
	return matchSegs(p[1:], s[1:])
}

// Glob matches one segment against a pattern in which `*` matches any run of characters.
func Glob(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

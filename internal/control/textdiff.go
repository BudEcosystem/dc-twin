package control

import (
	"bytes"
	"strings"

	"go.yaml.in/yaml/v3"
)

// matchLines is a Myers diff of two sequences of n and m lines, eq telling whether line i of
// the first equals line j of the second: for each line of the first, the line of the second it
// is kept as, or -1 when it is removed. The match is a longest common subsequence, found in
// O((n+m)·D) for D differences, so a small edit to a long file is cheap.
func matchLines(n, m int, eq func(i, j int) bool) []int {
	mx := make([]int, n)
	for i := range mx {
		mx[i] = -1
	}
	// the common head and tail need no search
	lo := 0
	for lo < n && lo < m && eq(lo, lo) {
		mx[lo] = lo
		lo++
	}
	hi := 0
	for hi < n-lo && hi < m-lo && eq(n-1-hi, m-1-hi) {
		mx[n-1-hi] = m - 1 - hi
		hi++
	}
	a, b := n-lo-hi, m-lo-hi
	if a == 0 || b == 0 {
		return mx
	}
	at := func(i, j int) bool { return eq(lo+i, lo+j) }
	// forward search, keeping each round's frontier (only the diagonals it can reach) to walk back
	max := a + b
	v := make([]int, 2*max+3)
	off := max + 1
	var trace [][]int
	for d := 0; d <= max; d++ {
		trace = append(trace, append([]int(nil), v[off-d-1:off+d+2]...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < a && y < b && at(x, y) {
				x, y = x+1, y+1
			}
			v[off+k] = x
			if x >= a && y >= b {
				// walk back through the rounds, recording the diagonals as matches
				for dd := d; dd >= 0; dd-- {
					w, base := trace[dd], dd+1 // w[base+k] is the frontier on diagonal k before round dd
					kk := x - y
					var pk int
					if kk == -dd || (kk != dd && w[base+kk-1] < w[base+kk+1]) {
						pk = kk + 1
					} else {
						pk = kk - 1
					}
					px := w[base+pk]
					py := px - pk
					for x > px && y > py && x > 0 && y > 0 {
						x, y = x-1, y-1
						mx[lo+x] = lo + y
					}
					if dd > 0 {
						x, y = px, py
					}
				}
				return mx
			}
		}
	}
	return mx
}

func splitLines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// canonLine is a line with its whitespace taken out: how the YAML encoder's rendering of a line
// is recognised in the author's ({ a: 1 } is {a: 1}, a comment's alignment is not kept).
func canonLine(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// keepLayout writes an edited estate file in the author's layout. The editor changes the
// file's YAML node tree and the encoder writes the whole tree back in its own style (no blank
// lines, its spacing in flow mappings, one space before a comment), so every line would look
// changed. keepLayout lines the encoder's rendering of the unedited file up with the source,
// line for line (ignoring whitespace), and diffs it with the rendering of the edited tree:
// every stretch the edit did not change is copied from the source as the author wrote it, and
// only the stretches it changed take the encoder's text. The result must read back as exactly
// the edited tree, or the encoder's own rendering is returned instead.
func keepLayout(src, norm, out []byte) []byte {
	// the edit's lines at the author's indentation and in the author's flow-mapping style
	// ({ a: 1 }) where the source writes them so; failing that, more as the encoder does
	pad := padsBraces(src)
	for _, v := range []struct{ indent, pad bool }{{true, pad}, {true, false}, {false, pad}, {false, false}} {
		if v.pad && !pad {
			continue
		}
		if merged := mergeLayout(src, norm, out, v.indent, v.pad); merged != nil {
			return merged
		}
	}
	return out
}

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }

func mergeLayout(src, norm, out []byte, reindent, pad bool) []byte {
	s, n, o := splitLines(src), splitLines(norm), splitLines(out)
	cs := make([]string, len(s))
	for i, l := range s {
		cs[i] = canonLine(l)
	}
	cn := make([]string, len(n))
	for j, l := range n {
		cn[j] = canonLine(l)
	}
	sn := matchLines(len(s), len(n), func(i, j int) bool { return cs[i] != "" && cs[i] == cn[j] })
	srcOf := make([]int, len(n))
	for j := range srcOf {
		srcOf[j] = -1
	}
	for i, j := range sn {
		if j >= 0 {
			srcOf[j] = i
		}
	}
	// the source's indentation for each of the encoder's: the most usual among the lines that
	// match, so a file indented by four writes the edit's lines indented by four
	depth := map[int]map[int]int{}
	for j, i := range srcOf {
		if i >= 0 {
			if depth[indentOf(n[j])] == nil {
				depth[indentOf(n[j])] = map[int]int{}
			}
			depth[indentOf(n[j])][indentOf(s[i])]++
		}
	}
	style := func(l string) string {
		if reindent {
			at, best, seen := indentOf(l), -1, 0
			for to, c := range depth[at] {
				if c > seen || (c == seen && to < best) {
					best, seen = to, c
				}
			}
			if best >= 0 && best != at {
				l = strings.Repeat(" ", best) + l[at:]
			}
		}
		if pad {
			l = padBraces(l)
		}
		return l
	}
	no := matchLines(len(n), len(o), func(i, j int) bool { return n[i] == o[j] })
	// anchors: lines the edit kept that are also found in the source
	type anchor struct{ s, n, o int }
	anchors := []anchor{{-1, -1, -1}}
	for j := range n {
		if srcOf[j] >= 0 && no[j] >= 0 {
			anchors = append(anchors, anchor{srcOf[j], j, no[j]})
		}
	}
	anchors = append(anchors, anchor{len(s), len(n), len(o)})
	blank := func(l string) bool { return strings.TrimSpace(l) == "" }
	var buf []string
	for k := 0; k+1 < len(anchors); k++ {
		a, b := anchors[k], anchors[k+1]
		changed := false
		kept := 0
		for j := a.n + 1; j < b.n; j++ {
			if no[j] < 0 {
				changed = true
			} else {
				kept++
			}
		}
		if kept != b.o-a.o-1 {
			changed = true // the edit added lines here
		}
		seg := s[a.s+1 : b.s]
		if !changed {
			buf = append(buf, seg...)
		} else {
			// the edit's lines, between the source's blank lines around the stretch; blank lines
			// that end it separate it from what follows, so what the edit adds goes before them
			trail := 0
			for trail < len(seg) && blank(seg[len(seg)-1-trail]) {
				trail++
			}
			lead := 0
			for lead < len(seg)-trail && blank(seg[lead]) {
				lead++
			}
			buf = append(buf, seg[:lead]...)
			for _, l := range o[a.o+1 : b.o] {
				buf = append(buf, style(l))
			}
			buf = append(buf, seg[len(seg)-trail:]...)
		}
		if b.s < len(s) {
			buf = append(buf, s[b.s])
		}
	}
	merged := []byte(strings.Join(buf, "\n") + "\n")
	// it must be the edited tree, comments and all
	var doc yaml.Node
	if err := yaml.Unmarshal(merged, &doc); err != nil {
		return nil
	}
	var re bytes.Buffer
	enc := yaml.NewEncoder(&re)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil || !bytes.Equal(re.Bytes(), out) {
		return nil
	}
	return merged
}

// flowBraces finds the braces of a line that open and close flow mappings: inside a flow
// collection every brace is one; outside, a brace opens one only where a value starts (after
// "key: ", "- " or at the start of the line), so a plain scalar such as a0{1..4} is left alone.
// Quoted text is skipped. It reports the brace positions and whether each opens or closes.
func flowBraces(l string) (at []int, open []bool) {
	depth, quote := 0, byte(0)
	valueStart := func(i int) bool {
		j := i - 1
		for j >= 0 && l[j] == ' ' {
			j--
		}
		if j < 0 {
			return true
		}
		return (l[j] == ':' || l[j] == '-') && j < i-1
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (i == 0 || l[i-1] == ' '):
			return
		case c == '{' && (depth > 0 || valueStart(i)):
			depth++
			at, open = append(at, i), append(open, true)
		case c == '[' && (depth > 0 || valueStart(i)):
			depth++
		case (c == '}' || c == ']') && depth > 0:
			depth--
			if c == '}' {
				at, open = append(at, i), append(open, false)
			}
		}
	}
	return
}

// padsBraces reports whether a file writes most of its flow mappings as { a: 1 }.
func padsBraces(src []byte) bool {
	padded, tight := 0, 0
	for _, l := range splitLines(src) {
		at, open := flowBraces(l)
		for k, i := range at {
			if open[k] && i+1 < len(l) && l[i+1] != '}' {
				if l[i+1] == ' ' {
					padded++
				} else {
					tight++
				}
			}
		}
	}
	return padded > tight
}

// padBraces writes a line's flow mappings as { a: 1 } (an empty one stays {}).
func padBraces(l string) string {
	at, open := flowBraces(l)
	if len(at) == 0 {
		return l
	}
	var b strings.Builder
	last := 0
	for k, i := range at {
		b.WriteString(l[last:i])
		switch {
		case open[k] && i+1 < len(l) && l[i+1] != '}' && l[i+1] != ' ':
			b.WriteString("{ ")
		case !open[k] && i > 0 && l[i-1] != '{' && l[i-1] != ' ':
			b.WriteString(" }")
		default:
			b.WriteByte(l[i])
		}
		last = i + 1
	}
	b.WriteString(l[last:])
	return b.String()
}

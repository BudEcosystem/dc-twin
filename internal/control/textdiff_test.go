package control

import (
	"math/rand"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestMatchLinesIsALongestCommonSubsequence(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for round := 0; round < 400; round++ {
		gen := func() []byte {
			b := make([]byte, rng.Intn(14))
			for i := range b {
				b[i] = "abc"[rng.Intn(3)]
			}
			return b
		}
		x, y := gen(), gen()
		mx := matchLines(len(x), len(y), func(i, j int) bool { return x[i] == y[j] })
		got, last := 0, -1
		for i, j := range mx {
			if j < 0 {
				continue
			}
			if j <= last || x[i] != y[j] {
				t.Fatalf("%q %q: match %v is not an increasing run of equal lines", x, y, mx)
			}
			last, got = j, got+1
		}
		lcs := make([][]int, len(x)+1)
		for i := range lcs {
			lcs[i] = make([]int, len(y)+1)
		}
		for i := len(x) - 1; i >= 0; i-- {
			for j := len(y) - 1; j >= 0; j-- {
				if x[i] == y[j] {
					lcs[i][j] = lcs[i+1][j+1] + 1
				} else {
					lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
				}
			}
		}
		if got != lcs[0][0] {
			t.Fatalf("%q %q: %d lines matched, the longest common subsequence is %d", x, y, got, lcs[0][0])
		}
	}
}

func TestEditsKeepTheAuthorsLayout(t *testing.T) {
	src := readMixedHall(t)
	name := func(n string) map[string]any { return map[string]any{"name": n} }
	out, _, err := editOps(src, []Op{
		{Op: "set", Path: []any{"spec", "sites", name("site-1"), "halls", name("hall-b"), "rows", name("b"), "racks", name("b01"), "params", "servers"}, Value: 2.0},
		{Op: "rack.add", Hall: "hall-b", Row: "c", Name: "c03", Position: 3, Format: "generic/cpu-rack"},
		{Op: "add", Path: []any{"spec", "facility", "power", "feeds", "-"}, Value: map[string]any{"name": "C", "capacityW": 900000.0}},
		{Op: "rack.remove", Name: "a02"},
	})
	if err != nil {
		t.Fatal(err)
	}
	minus, plus := 0, 0
	for _, l := range strings.Split(unifiedDiff(string(src), string(out), 0), "\n") {
		switch {
		case strings.HasPrefix(l, "-"):
			minus++
		case strings.HasPrefix(l, "+"):
			plus++
		}
	}
	// a02 out; b01's servers; c03 in; feed C; a02 out of cdu-a
	if minus != 4 || plus != 4 {
		t.Fatalf("only the edited lines change: %d removed, %d added\n%s", minus, plus, unifiedDiff(string(src), string(out), 0))
	}
	sel, _, err := editOps(src, []Op{{Op: "set", Path: []any{"spec", "fabrics", name("backend-roce"), "select"}, Value: "{b02,b03}"},
		{Op: "set", Path: []any{"spec", "facility", "power", "topology", name("rpp-a1"), "serves"}, Value: []any{"a0*", "b01"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sel), `select: "{b02,b03}"`) || !strings.Contains(string(sel), `serves: ["a0*", b01]`) {
		t.Fatalf("selectors are quoted:\n%s", unifiedDiff(string(src), string(sel), 0))
	}
	for _, want := range []string{
		"                - { name: c03, position: 3, format: generic/cpu-rack, params: { server: oem-dell/r760, servers: 12 } }\n\n  networks:",
		"    - amd@0.1.0                     # after DSX OS (10 — M8)",
		"      feeds: [{ name: A, capacityW: 900000 }, { name: B, capacityW: 900000 }, { name: C, capacityW: 900000 }]",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("the edited file should read %q:\n%s", want, out)
		}
	}
	if strings.Count(string(out), "\n\n") != strings.Count(string(src), "\n\n") {
		t.Fatal("blank lines are kept")
	}
}

func TestPadBraces(t *testing.T) {
	for in, want := range map[string]string{
		`- {name: a, params: {x: 1}}`:   `- { name: a, params: { x: 1 } }`,
		`select: a0{1..4}`:              `select: a0{1..4}`,
		`select: "{b02,b03}"`:           `select: "{b02,b03}"`,
		`racks: [{name: a}, {name: b}]`: `racks: [{ name: a }, { name: b }]`,
		`x: {}`:                         `x: {}`,
		`- {a: '}{'} # a {comment}`:     `- { a: '}{' } # a {comment}`,
		`serves: ["{b01,b02}", "c0*"]`:  `serves: ["{b01,b02}", "c0*"]`,
	} {
		if got := padBraces(in); got != want {
			t.Errorf("padBraces(%q) = %q, want %q", in, got, want)
		}
	}
}

// A file indented by four, where the encoder indents by two: the edit's lines take the file's
// indentation, and the result is exactly the edited tree.
func TestKeepLayoutFollowsTheFilesIndentation(t *testing.T) {
	src := []byte("spec:\n    racks:\n        - { name: a }\n\n    feeds: [A]\n")
	out, _, err := editOps(src, []Op{{Op: "add", Path: []any{"spec", "racks", "-"}, Value: map[string]any{"name": "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "spec:\n    racks:\n        - { name: a }\n        - { name: b }\n\n    feeds: [A]\n" {
		t.Fatalf("the layout is the file's:\n%s", out)
	}
	var got, want any
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	_ = yaml.Unmarshal([]byte("spec: {racks: [{name: a}, {name: b}], feeds: [A]}"), &want)
	if !equalYAML(got, want) {
		t.Fatalf("got %v, want %v:\n%s", got, want, out)
	}
}

func equalYAML(a, b any) bool {
	x, _ := yaml.Marshal(a)
	y, _ := yaml.Marshal(b)
	return string(x) == string(y)
}

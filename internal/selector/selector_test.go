package selector

import (
	"reflect"
	"testing"
)

func TestExpand(t *testing.T) {
	cases := map[string][]string{
		"tray-{01..03}": {"tray-01", "tray-02", "tray-03"},
		"x{a,b}y":       {"xay", "xby"},
		"{1..2}{a,b}":   {"1a", "1b", "2a", "2b"},
		"n-{8..10}":     {"n-8", "n-9", "n-10"},
		"p-{01..8}":     {"p-01", "p-02", "p-03", "p-04", "p-05", "p-06", "p-07", "p-08"},
		"plain":         {"plain"},
		"a{b,{c,d}}":    {"ab", "ac", "ad"},
	}
	for in, want := range cases {
		got, err := Expand(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Expand(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

func TestMatch(t *testing.T) {
	path := "site-1/hall-a/a03/compute-tray-07"
	yes := []string{"a03/compute-tray-*", "compute-tray-07", "**", "a03/**", "site-1/hall-a/a03/compute-tray-07", "a0*/compute-tray-{06..08}"}
	no := []string{"a04/compute-tray-*", "compute-tray-0", "hall-a/compute-tray-07", "a03"}
	for _, s := range yes {
		if !MustParse(s).MatchPath(path) {
			t.Errorf("%q should match %s", s, path)
		}
	}
	for _, s := range no {
		if MustParse(s).MatchPath(path) {
			t.Errorf("%q should not match %s", s, path)
		}
	}
	if _, err := Parse("a0[1-4]"); err == nil {
		t.Error("character classes must be refused")
	}
	sel := MustParse("a0*/** ?class=switch-tray")
	if !sel.MatchAttrs(func(string) []string { return []string{"switch-tray"} }) {
		t.Error("filter")
	}
	if !MustParse("a10/leaf-*:eth{1..4}").MatchPort("site-1/hall-a/a10/leaf-a03-1:eth3") {
		t.Error("port match")
	}
}

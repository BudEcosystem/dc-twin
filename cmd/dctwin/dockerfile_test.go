package main

import (
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// The Dockerfile copies every top-level directory the twin's packages live in, and .dockerignore
// keeps each in the build context. One left out builds in the repository and nowhere else: the
// image's go build fails, as main's did when internal/critic began embedding policies/ and the
// Dockerfile still copied cmd, internal, packs and stacks only.
func TestDockerfileCopiesEveryPackage(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	const mod = "github.com/BudEcosystem/dc-twin/"
	need := map[string][]string{} // top-level directory -> the packages in it
	for _, p := range strings.Fields(string(out)) {
		if rel, ok := strings.CutPrefix(p, mod); ok {
			top, _, _ := strings.Cut(rel, "/")
			need[top] = append(need[top], p)
		}
	}
	if len(need) < 3 {
		t.Fatalf("go list found the twin's packages in %d directories; it did not list them", len(need))
	}
	copied := map[string]bool{} // COPY <directory> <destination>, from the build context
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) == 3 && f[0] == "COPY" {
			copied[strings.TrimSuffix(f[1], "/")] = true
		}
	}
	// .dockerignore leaves everything out (*) but what it names (!<directory>): a directory it
	// leaves out is not in the build context, and its COPY fails
	ig, err := os.ReadFile("../../.dockerignore")
	if err != nil {
		t.Fatal(err)
	}
	inContext := map[string]bool{}
	all := false
	for _, l := range strings.Split(string(ig), "\n") {
		l = strings.TrimSpace(l)
		if l == "*" {
			all = true
		} else if dir, ok := strings.CutPrefix(l, "!"); ok {
			inContext[strings.TrimSuffix(dir, "/")] = true
		}
	}
	if !all {
		t.Fatalf(".dockerignore does not start from * (leave everything out): the test reads it as an allow-list")
	}
	tops := make([]string, 0, len(need))
	for top := range need {
		tops = append(tops, top)
	}
	sort.Strings(tops)
	for _, top := range tops {
		if !copied[top] {
			t.Errorf("the Dockerfile does not copy %s/, where %s live", top, strings.Join(need[top], ", "))
		}
		if !inContext[top] {
			t.Errorf(".dockerignore leaves %s/ out of the build context, where %s live", top, strings.Join(need[top], ", "))
		}
	}
}

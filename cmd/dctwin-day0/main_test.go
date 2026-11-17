package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallToCopiesTheExecutable(t *testing.T) {
	dir := t.TempDir()
	if rc := installTo([]string{dir}); rc != 0 {
		t.Fatalf("install-to: %d", rc)
	}
	self, _ := os.Executable()
	want, _ := os.Stat(self)
	got, err := os.Stat(filepath.Join(dir, "dctwin-day0"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Size() != want.Size() || got.Mode().Perm()&0o111 == 0 {
		t.Errorf("copied %d bytes, mode %v; want %d bytes, executable", got.Size(), got.Mode(), want.Size())
	}
	if rc := installTo(nil); rc != 2 {
		t.Errorf("no directory: %d, want 2", rc)
	}
}

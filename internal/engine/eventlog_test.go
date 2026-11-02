package engine

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// logFiles are the log's files in dir and their total size.
func logFiles(t *testing.T, dir string) ([]string, int64) {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var total int64
	for _, e := range es {
		if strings.HasPrefix(e.Name(), "events") && strings.HasSuffix(e.Name(), ".jsonl") {
			info, _ := e.Info()
			names = append(names, e.Name())
			total += info.Size()
		}
	}
	sort.Strings(names)
	return names, total
}

// The log on disk stays within its total: the active file rolls into sequence-named segments, and
// the oldest go first, an estate restart's archive among them; the active file is never removed.
func TestEventLogStaysWithinItsSize(t *testing.T) {
	dir := t.TempDir()
	// an earlier run's archive, as the control API's estate restart leaves one
	old := filepath.Join(dir, "events.20261101T000000.jsonl")
	os.WriteFile(old, []byte(strings.Repeat("x", 3000)+"\n"), 0o600)
	os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	l, err := openEventLog(filepath.Join(dir, "events.jsonl"), LogRetention{SegmentBytes: 1024, MaxBytes: 4096, MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	line := []byte(`{"kind":"request","message":"` + strings.Repeat("r", 90) + `"}`)
	var removed []string
	for seq := uint64(1); seq <= 400; seq++ {
		removed = append(removed, l.write(line, seq, false)...)
	}
	l.w.Flush()
	names, total := logFiles(t, dir)
	if total > 4096+1024 {
		t.Errorf("the log is %d bytes on disk, over its 4096 and one segment: %v", total, names)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) || removed[0] != filepath.Base(old) {
		t.Errorf("the restart archive was not removed first: removed %v", removed)
	}
	if names[len(names)-1] != "events.jsonl" || len(names) < 3 {
		t.Fatalf("files %v", names)
	}
	// the segments kept are the newest, in sequence order
	segs := names[:len(names)-1]
	if !strings.HasPrefix(segs[0], "events.0000") || segs[len(segs)-1] <= segs[0] {
		t.Errorf("segments %v", segs)
	}
}

// A segment past its age goes whatever the log's size.
func TestEventLogRemovesSegmentsPastTheirAge(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "events.000000000001-000000000009.jsonl")
	fresh := filepath.Join(dir, "events.000000000010-000000000019.jsonl")
	for _, p := range []string{stale, fresh} {
		os.WriteFile(p, []byte("{}\n"), 0o600)
	}
	os.Chtimes(stale, time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour))
	l, err := openEventLog(filepath.Join(dir, "events.jsonl"), LogRetention{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	if removed := l.prune(); len(removed) != 1 || removed[0] != filepath.Base(stale) {
		t.Errorf("removed %v, want the stale segment only", removed)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the fresh segment went: %v", err)
	}
}

// Through the engine: a stack's reads, committed by the thousand, leave the log on disk within its
// bounds, and the log records what it removed.
func TestEngineLogRecordsItsRetention(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../examples/estates/lab-model.yaml")
	if err != nil {
		t.Fatal(err)
	}
	est.Spec.Clock = "virtual"
	res, err := compile.Compile(est, reg, compile.Options{HostArch: "aarch64"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	e, err := New(res.Model, reg, filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	e.SetLogRetention(LogRetention{SegmentBytes: 8 << 10, MaxBytes: 32 << 10})
	var d *model.Device
	for _, x := range e.M.Devices {
		if x.HasBMC() {
			d = x
			break
		}
	}
	for i := 0; i < 2000; i++ {
		e.mu.Lock()
		e.logRequest(Request{Path: d.Path, Canonical: "inventory.read", Protocol: "redfish", Method: "GET", Target: "/redfish/v1/Systems", Source: stack}, 200)
		e.mu.Unlock()
	}
	_, total := logFiles(t, dir)
	if total > 32<<10+8<<10 {
		t.Errorf("the log is %d bytes on disk, over its 32 KiB and one segment", total)
	}
	notes := e.Events(0, func(ev schema.Event) bool {
		return ev.Kind == "twin" && strings.HasPrefix(ev.Message, "event log retention")
	})
	if len(notes) == 0 {
		t.Fatal("the log does not record what retention removed")
	}
}

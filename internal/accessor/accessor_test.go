package accessor

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

type log struct {
	mu  sync.Mutex
	evs []schema.Event
}

func (l *log) Commit(ev schema.Event) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evs = append(l.evs, ev)
	return uint64(len(l.evs))
}

// TestEachRunRecordsItsFirstRead: an unchanged answer is recorded once per run, so a run judged
// from its own events always has the stack's answer (D68).
func TestEachRunRecordsItsFirstRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"state":"Ready"}`))
	}))
	defer srv.Close()
	b := &Binding{Target: "api.machine", Comp: "api", Outputs: map[string]string{"api": srv.URL},
		Def: schema.Accessor{Kind: "http", Endpoint: "{api}/machines/{id}"}}
	l := &log{}
	r := Shared(l, "site-1")
	defer Forget(l)
	args := map[string]any{"id": "SN1"}
	for i := 0; i < 3; i++ {
		if _, err := r.Read(b, "machine", args); err != nil {
			t.Fatal(err)
		}
	}
	if len(l.evs) != 1 {
		t.Fatalf("an unchanged answer is recorded once: %d", len(l.evs))
	}
	ResetShared(l) // the next run starts
	r.Read(b, "machine", args)
	if len(l.evs) != 2 {
		t.Fatalf("the next run records its first read: %d", len(l.evs))
	}
	Forget(l)
	if Shared(l, "site-1") == r {
		t.Fatal("a forgotten engine gets a fresh recorder")
	}
}

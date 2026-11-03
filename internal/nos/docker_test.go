package nos

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
)

// fakeEngine is the Engine API's create, start and inspect on a unix socket, recording each
// create's body.
func fakeEngine(t *testing.T) (*Docker, func() []map[string]any) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "e.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var creates []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		creates = append(creates, body)
		mu.Unlock()
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"Id":"c1"}`))
	})
	mux.HandleFunc("POST /containers/c1/start", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("GET /containers/c1/json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"State":{"Pid":42,"Running":true}}`))
	})
	mux.HandleFunc("DELETE /containers/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	d := &Docker{Socket: sock, c: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return net.Dial("unix", sock) }}}}
	return d, func() []map[string]any { mu.Lock(); defer mu.Unlock(); return creates }
}

// A runtime with a memory limit gives every container it creates that limit and no swap beyond
// it; without one, containers are created unlimited, as before.
func TestRunMemoryLimit(t *testing.T) {
	d, creates := fakeEngine(t)
	if _, err := d.Run(t.Context(), Spec{Name: "dctwin-x", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	d.Memory = 512 << 20
	if _, err := d.Run(t.Context(), Spec{Name: "dctwin-y", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	cs := creates()
	if len(cs) != 2 {
		t.Fatalf("%d creates", len(cs))
	}
	hc0 := cs[0]["HostConfig"].(map[string]any)
	if _, ok := hc0["Memory"]; ok {
		t.Errorf("no limit set, but the container was created with Memory %v", hc0["Memory"])
	}
	hc1 := cs[1]["HostConfig"].(map[string]any)
	if hc1["Memory"] != float64(512<<20) || hc1["MemorySwap"] != float64(512<<20) {
		t.Errorf("Memory %v MemorySwap %v, want both %d", hc1["Memory"], hc1["MemorySwap"], 512<<20)
	}
}

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "512m": 512 << 20, "2g": 2 << 30, "2G": 2 << 30, "65536k": 64 << 20, "8388608": 8 << 20} {
		got, err := ParseMemory(in)
		if err != nil || got != want {
			t.Errorf("ParseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"x", "-1m", "1t", "0", "3m"} {
		if _, err := ParseMemory(in); err == nil {
			t.Errorf("ParseMemory(%q): no error", in)
		}
	}
}

package nos

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// registryEngine is the Engine API's image inspect, pull and tag on a unix socket: it holds the
// images it has, pulls what its registry has (a pull of anything else reports its failure in the
// progress stream, as the Engine does) and records every pull and tag.
func registryEngine(t *testing.T, have, registry []string) (*Docker, func() []string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "e.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	images := map[string]bool{}
	for _, i := range have {
		images[i] = true
	}
	inRegistry := map[string]bool{}
	for _, i := range registry {
		inRegistry[i] = true
	}
	var log []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /images/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")
		mu.Lock()
		defer mu.Unlock()
		if !images[name] {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"message":"no such image"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		ref := r.URL.Query().Get("fromImage") + ":" + r.URL.Query().Get("tag")
		mu.Lock()
		defer mu.Unlock()
		log = append(log, "pull "+ref+" "+r.URL.Query().Get("platform"))
		if !inRegistry[ref] {
			// the Engine's way: 200, and the failure in the stream
			_, _ = w.Write([]byte("{\"status\":\"Pulling from x\"}\n{\"errorDetail\":{\"message\":\"manifest unknown\"},\"error\":\"manifest unknown\"}\n"))
			return
		}
		images[ref] = true
		_, _ = w.Write([]byte("{\"status\":\"Pulling from x\"}\n{\"status\":\"Download complete\"}\n"))
	})
	mux.HandleFunc("POST /images/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(r.PathValue("rest"), "/tag")
		as := r.URL.Query().Get("repo") + ":" + r.URL.Query().Get("tag")
		mu.Lock()
		defer mu.Unlock()
		log = append(log, "tag "+name+" "+as)
		if !images[name] {
			w.WriteHeader(404)
			return
		}
		images[as] = true
		w.WriteHeader(201)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	d := &Docker{Socket: sock, c: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return net.Dial("unix", sock) }}}}
	return d, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), log...) }
}

// An image the host has is used as it is; one of the twin's own that it lacks is pulled from the
// registry and tagged as the twin names it; without a registry, or for an image named with a
// registry of its own, a missing image is still an error, as before.
func TestEnsure(t *testing.T) {
	const reg = "ghcr.io/budecosystem"
	d, log := registryEngine(t,
		[]string{"dctwin-nos-hbn:bookworm"},
		[]string{reg + "/dctwin-nos-nvue-frr:bookworm", reg + "/dctwin-sonic-gnmi:master.1250767-408ffc98c"})
	ctx := t.Context()

	if from, err := d.Ensure(ctx, "dctwin-nos-hbn:bookworm", reg, ""); err != nil || from != "" {
		t.Errorf("an image the host has: pulled %q, %v", from, err)
	}
	from, err := d.Ensure(ctx, "dctwin-nos-nvue-frr:bookworm", reg, "")
	if err != nil || from != reg+"/dctwin-nos-nvue-frr:bookworm" {
		t.Fatalf("an own image the host lacks: pulled %q, %v", from, err)
	}
	if !d.HasImage(ctx, "dctwin-nos-nvue-frr:bookworm") {
		t.Error("pulled, but not present under the name the twin runs it by")
	}
	// for its platform, as SONiC's are amd64's only
	if _, err := d.Ensure(ctx, "dctwin-sonic-gnmi:master.1250767-408ffc98c", reg, "linux/amd64"); err != nil {
		t.Fatal(err)
	}
	// and Podman's name for a local build
	d2, _ := registryEngine(t, nil, []string{reg + "/dctwin-nos-hbn:bookworm"})
	if from, err := d2.Ensure(ctx, "localhost/dctwin-nos-hbn:bookworm", reg, ""); err != nil || from != reg+"/dctwin-nos-hbn:bookworm" {
		t.Errorf("localhost/: pulled %q, %v", from, err)
	}

	// one of the twin's own with no registry: nothing to pull it from, and no pull tried
	if _, err := d.Ensure(ctx, "dctwin-nos-frr-dhcrelay:bookworm", "", ""); !errors.Is(err, ErrNotPresent) {
		t.Errorf("no registry: %v, want ErrNotPresent", err)
	}
	// one named with a registry of its own is pulled from that, never through ours (this fake's
	// registry has none of SONiC's, so the pull fails)
	const vs = "sonicdev-microsoft.azurecr.io/docker-sonic-vs@sha256:0de1"
	if _, err := d.Ensure(ctx, vs, reg, "linux/amd64"); !errors.Is(err, ErrNotPresent) {
		t.Errorf("%s: %v, want ErrNotPresent after its own registry's refusal", vs, err)
	}
	// the registry has no such image: the Engine's 200 with the failure in its stream is a failure
	_, err = d.Ensure(ctx, "dctwin-nos-frr-dhcrelay:bookworm", reg, "")
	if !errors.Is(err, ErrNotPresent) || !strings.Contains(err.Error(), "manifest unknown") {
		t.Errorf("a pull the registry refuses: %v", err)
	}

	got := strings.Join(log(), "\n")
	for _, want := range []string{
		"pull " + reg + "/dctwin-nos-nvue-frr:bookworm ",
		"tag " + reg + "/dctwin-nos-nvue-frr:bookworm dctwin-nos-nvue-frr:bookworm",
		"pull " + reg + "/dctwin-sonic-gnmi:master.1250767-408ffc98c linux/amd64",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in the Engine's log:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "pull sonicdev-microsoft.azurecr.io/docker-sonic-vs:sha256:0de1 linux/amd64") {
		t.Errorf("SONiC's switch image not pulled from its own registry, by digest:\n%s", got)
	}
	if strings.Contains(got, reg+"/sonicdev") || strings.Contains(got, reg+"/docker-sonic-vs") {
		t.Errorf("pulled an image named with its own registry through ours:\n%s", got)
	}
	// frr-dhcrelay was asked for twice, with no registry and with one: pulled once, for the second
	if n := strings.Count(got, "pull "+reg+"/dctwin-nos-frr-dhcrelay:bookworm"); n != 1 {
		t.Errorf("frr-dhcrelay pulled %d times, want once (none with no registry set):\n%s", n, got)
	}
}

func TestSplitRef(t *testing.T) {
	for ref, want := range map[string][2]string{
		"dctwin-nos-hbn:bookworm":                         {"dctwin-nos-hbn", "bookworm"},
		"ghcr.io/budecosystem/dctwin-nos-hbn:frr10":       {"ghcr.io/budecosystem/dctwin-nos-hbn", "frr10"},
		"registry.lab:5000/dc-twin/hbn":                   {"registry.lab:5000/dc-twin/hbn", "latest"},
		"registry.lab:5000/hbn:bookworm":                  {"registry.lab:5000/hbn", "bookworm"},
		"sonicdev.azurecr.io/docker-sonic-vs@sha256:0de1": {"sonicdev.azurecr.io/docker-sonic-vs", "sha256:0de1"},
	} {
		if r, tag := splitRef(ref); r != want[0] || tag != want[1] {
			t.Errorf("splitRef(%q) = %q, %q; want %q, %q", ref, r, tag, want[0], want[1])
		}
	}
}

func TestValidRegistry(t *testing.T) {
	for _, ok := range []string{"", "ghcr.io/budecosystem", "registry.lab:5000/dc-twin", "localhost:5000", "localhost/x", "ghcr.io/budecosystem/"} {
		if err := ValidRegistry(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"https://ghcr.io/budecosystem", "budecosystem", "ghcr.io/x@sha256:1", "a b"} {
		if err := ValidRegistry(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

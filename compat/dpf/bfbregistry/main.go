// Command bfbregistry stands in for DPF's bfb-registry for compat/dpf: it serves DPF's /bfb
// volume over HTTPS with the URL scheme of the nginx-http-concat module DPF's registry image is
// built with, so a DPU BMC fetching `<registry>/bfb/??<bfb>,bfcfg/<dpu>.cfg?/bfb-to-install`
// receives the BFB with the DPU's bf.cfg appended, as from DPF's own registry.
//
// DPF's bfb-registry is a pod (nginx 1.30 with alibaba/nginx-http-concat, Dockerfile.bfb-registry)
// that the provisioning controller schedules on its node; with no kubelet behind the harness's
// cluster the pod never runs, so this process serves the same volume, the same paths and a
// certificate DPF's issuer signed for it (the bfb-registry-server-cert DPF's BFBRegistryRunnable
// would request). It is the harness's, not the twin's: the twin is on the other side, as the BMC.
//
// With -deb it also serves the registry image's other volume, /deb: the apt repository DPF's
// cloud-init on a DPU installs `dpu-agent` from (`deb [trusted=yes] <registry>/deb ./`), built as
// the image builds it (lib/agent-deb.sh).
//
//	bfbregistry -root /bfb -deb /deb -listen :8443 -cert tls.crt -key tls.key
package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	root := flag.String("root", "/bfb", "the BFB volume")
	listen := flag.String("listen", ":8443", "HTTPS address")
	cert := flag.String("cert", "/tls/tls.crt", "server certificate")
	key := flag.String("key", "/tls/tls.key", "server key")
	deb := flag.String("deb", "", "the apt repository volume served at /deb (empty: none)")
	flag.Parse()
	var debFiles http.Handler
	if *deb != "" {
		debFiles = http.StripPrefix("/deb/", http.FileServer(http.Dir(*deb)))
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if debFiles != nil && strings.HasPrefix(r.URL.Path, "/deb/") {
			log.Printf("%s %s from %s", r.Method, r.URL.RequestURI(), r.RemoteAddr)
			debFiles.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		files, err := resolve(*root, r.URL.Path, r.URL.RawQuery)
		if err != nil {
			log.Printf("%s %s: %v", r.Method, r.URL.RequestURI(), err)
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		var total int64
		for _, f := range files {
			st, err := os.Stat(f)
			if err != nil || st.IsDir() {
				log.Printf("%s %s: %s missing", r.Method, r.URL.RequestURI(), f)
				http.NotFound(w, r)
				return
			}
			total += st.Size()
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
		log.Printf("%s %s from %s: %d files, %d bytes", r.Method, r.URL.RequestURI(), r.RemoteAddr, len(files), total)
		if r.Method == http.MethodHead {
			return
		}
		for _, f := range files {
			fh, err := os.Open(f)
			if err != nil {
				return
			}
			_, err = io.Copy(w, fh)
			fh.Close()
			if err != nil {
				log.Printf("%s: %v", r.URL.RequestURI(), err)
				return
			}
		}
	})
	log.Printf("bfbregistry: serving %s (and /deb from %q) on %s", *root, *deb, *listen)
	log.Fatal(http.ListenAndServeTLS(*listen, *cert, *key, h))
}

// resolve maps a request to files under root. A path ending in "/" with a query starting "?"
// is a concat request: "??a,b,c?ignored" lists files relative to the path (nginx-http-concat;
// what follows the second "?" is a version tag the module ignores, which DPF uses to give the
// download a name). Anything else is one file.
func resolve(root, urlPath, rawQuery string) ([]string, error) {
	clean := path.Clean("/" + urlPath)
	if !strings.HasPrefix(clean, "/bfb") {
		return nil, os.ErrNotExist
	}
	rel := strings.TrimPrefix(clean, "/bfb")
	base := filepath.Join(root, filepath.FromSlash(rel))
	if strings.HasSuffix(urlPath, "/") && strings.HasPrefix(rawQuery, "?") {
		list := strings.TrimPrefix(rawQuery, "?")
		if i := strings.Index(list, "?"); i >= 0 {
			list = list[:i]
		}
		var out []string
		for _, f := range strings.Split(list, ",") {
			p := path.Clean("/" + f)
			if f == "" || strings.Contains(f, "..") {
				return nil, os.ErrNotExist
			}
			out = append(out, filepath.Join(base, filepath.FromSlash(p)))
		}
		return out, nil
	}
	return []string{base}, nil
}

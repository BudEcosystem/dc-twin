package control

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

// gzipResponse compresses a response for a client that accepts gzip. The console polls the
// device list every few seconds, about 25 times smaller compressed, which matters once the
// console is reached over a network rather than at loopback (D54).
type gzipResponse struct {
	http.ResponseWriter
	gz    *gzip.Writer
	wrote bool
}

// acceptsGzip reports whether a request accepts a gzip-encoded response.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(part, ";")
		if strings.TrimSpace(name) != "gzip" {
			continue
		}
		if q, ok := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q="); ok {
			v, err := strconv.ParseFloat(q, 64)
			return err == nil && v > 0
		}
		return true
	}
	return false
}

func (g *gzipResponse) WriteHeader(code int) {
	if g.wrote {
		return
	}
	g.wrote = true
	h := g.Header()
	// a response with no body, one already encoded, or one whose type the server would sniff
	// from its (compressed) bytes goes as it is
	if code >= 200 && code != http.StatusNoContent && code != http.StatusNotModified && h.Get("Content-Encoding") == "" && h.Get("Content-Type") != "" {
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		g.gz = gzip.NewWriter(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponse) Write(b []byte) (int, error) {
	if !g.wrote {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.gz == nil {
		return g.ResponseWriter.Write(b)
	}
	return g.gz.Write(b)
}

// Close ends the compressed body.
func (g *gzipResponse) Close() {
	if g.gz != nil {
		_ = g.gz.Close()
	}
}

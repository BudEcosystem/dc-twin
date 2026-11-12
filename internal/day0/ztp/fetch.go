package ztp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// maxFetch bounds what a fetch reads: a ZTP script, a ZTP document or a config_db.json is far
// smaller, and a server that streams forever must not fill the switch's disk.
const maxFetch = 64 << 20

// fetcher fetches what ZTP points at: http and https as both NOSes' curl does, file URLs and
// plain paths (SONiC's own file:///host/ztp/provisioning-script, E.1; Cumulus's `ztp -r <path>`,
// A.5). tftp, which both NOSes accept, is not supported by the twin's client and fails with a
// reason that says so; a site serves ZTP over HTTP.
type fetcher struct {
	// vrf is the device sockets bind to ("" for the default VRF).
	vrf string
	// insecure skips the server certificate's verification. Cumulus's documented fetches use
	// curl -k (the NVUE examples in C.1, and its ZTP's own fetch is not documented as verifying),
	// so the Cumulus flavour sets it; SONiC's https-secure defaults to true (E.2), and a url
	// object's "secure": false clears it per fetch.
	insecure bool
	// corrupt: the twin's ztp-script-error fault holds, and what is fetched arrives cut short
	corrupt func() bool
	headers http.Header
	timeout time.Duration
}

// newFetcher binds fetches to c.vrf when the switch has that device, and to the default VRF
// otherwise (a sonic-vs image without a management VRF, a test).
func (c *config) newFetcher(h http.Header, insecure bool) *fetcher {
	f := &fetcher{insecure: insecure, headers: h, timeout: c.fetchTimeout, corrupt: func() bool { return c.fault("ztp-corrupt") }}
	if c.vrf != "" && c.vrfExists != nil && c.vrfExists(c.vrf) {
		f.vrf = c.vrf
	}
	return f
}

func (f *fetcher) client(insecure bool, timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if f.vrf != "" {
		d.Control = bindToDevice(f.vrf)
		// Names resolve through the same VRF: Go's resolver dials the servers with this dialer.
		inner := *d
		d.Resolver = &net.Resolver{PreferGo: true, Dial: inner.DialContext}
	}
	tr := &http.Transport{
		DialContext:         d.DialContext,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // documented above
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// get fetches raw with the fetcher's headers.
func (f *fetcher) get(ctx context.Context, raw string) ([]byte, error) {
	return f.getWith(ctx, raw, f.insecure, true, f.timeout)
}

// getWith fetches raw, verifying the server's certificate unless insecure and sending the
// identifying headers when withHeaders (sonic-ztp's include-http-headers, E.2).
func (f *fetcher) getWith(ctx context.Context, raw string, insecure, withHeaders bool, timeout time.Duration) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "/") {
		return readLimited(raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	case "file":
		return readLimited(u.Path)
	case "tftp":
		return nil, fmt.Errorf("%s: tftp is not supported by the twin's ZTP client; serve it over http", raw)
	case "":
		return nil, fmt.Errorf("%q has no scheme (http, https or file)", raw)
	default:
		return nil, fmt.Errorf("%s: scheme %q is not supported (http, https or file)", raw, u.Scheme)
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	if withHeaders {
		for k, vs := range f.headers {
			// set directly: Cumulus's and SONiC's header names are sent as they spell them
			req.Header[k] = vs
		}
	}
	resp, err := f.client(insecure, timeout).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %s", raw, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFetch+1))
	if err == nil && f.corrupt != nil && f.corrupt() && len(b) > 1 {
		b = b[:len(b)/2] // an interrupted transfer (ztp-script-error)
	}
	if err != nil {
		return nil, err
	}
	if len(b) > maxFetch {
		return nil, fmt.Errorf("%s: larger than %d bytes", raw, maxFetch)
	}
	return b, nil
}

func readLimited(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, errors.New(path + " is a directory")
	}
	if fi.Size() > maxFetch {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, maxFetch)
	}
	return os.ReadFile(path)
}

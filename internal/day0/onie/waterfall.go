package onie

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/pin/tftp/v3"
)

// ErrTimeout is a TFTP fetch that timed out, which ends the TFTP waterfall on that server as it
// ends ONIE's (facts D.3, step 9). A TFTP func may return it, or a net.Error that times out.
var ErrTimeout = errors.New("timed out")

// wgetTimeout is busybox wget's -T 3 in wget_run (facts D.5): the network timeout for the
// connection, the response and each read of the body.
const wgetTimeout = 3 * time.Second

// waterfall is one pass of exec_installer after discovery (facts D.3, steps 6 to 9): the exact
// URLs, then HTTP over the server list with the default names, then the bootfile as a URL, then
// TFTP with the bootfile, then the TFTP waterfall. The steps before them (a static URL from the
// command line or kernel, firmware updates, local and UBI partitions) have nothing to find in a
// factory switch's container and are not modelled. It reports whether a NOS installed.
func (e *Env) waterfall(ctx context.Context) bool {
	l := e.lease
	// 6. Exact URLs: option 125's installer URL (onie_disco_onie_url), then option 114
	// (onie_disco_url).
	for _, u := range []string{l.InstallerURL, l.URL} {
		if u != "" && e.tryURL(ctx, u) {
			return true
		}
	}
	// 7. http_download: every server in the unique-ordered list that answers on port 80, with
	// every default name at its root.
	for _, s := range e.HTTPServers() {
		if ctx.Err() != nil {
			return false
		}
		probeHost := s
		if s == ServerName {
			probeHost = e.resolved[0].String()
		}
		if !e.Probe(ctx, probeHost, e.HTTPPort) {
			e.logf("http://%s/: no answer on port %d", s, e.HTTPPort)
			continue
		}
		for _, n := range e.plat.InstallerNames() {
			if e.tryURL(ctx, "http://"+e.hostPort(s)+"/"+n) {
				return true
			}
		}
	}
	// ONIE's server list ends with the link-local neighbours of neigh_discovery() (ping6
	// ff02::1, then ip neigh). A factory switch's management segment has the twin's DHCP and
	// the site's servers on it, all of which the lease names, so neighbours are not probed.
	e.logf("link-local neighbours: not probed (the stand-in reaches servers the lease names)")
	// Option 67 alone is also tried as a URL.
	if strings.Contains(l.Bootfile, "://") && e.tryURL(ctx, l.Bootfile) {
		return true
	}
	if e.TFTP == nil {
		e.logf("tftp: no TFTP client, skipped")
		return false
	}
	// 8. tftp_download: the bootfile (option 67, else the BOOTP file field) from each server.
	if file := firstNonEmpty(l.Bootfile, l.BootFile); file != "" && !strings.Contains(file, "://") {
		for _, s := range e.tftpServers(ctx, true) {
			if e.tryURL(ctx, "tftp://"+s+"/"+strings.TrimPrefix(file, "/")) {
				return true
			}
		}
	}
	// 9. waterfall(): directories named for the MAC and for prefixes of the address's hex, then
	// every default name at the root, stopping on a server at its first timeout.
	for _, s := range e.tftpServers(ctx, false) {
		for _, p := range e.TFTPPaths() {
			ok, timedOut := e.tryTFTP(ctx, s, p)
			if ok {
				return true
			}
			if timedOut || ctx.Err() != nil {
				break
			}
		}
	}
	return false
}

// HTTPServers is http_download's server list (facts D.3, step 7), unique and in order:
// onie-server when it resolves, option 72's servers, the BOOTP siaddr, option 54's server id,
// option 150's TFTP servers, and option 66.
func (e *Env) HTTPServers() []string {
	l := e.lease
	var list []string
	if len(e.resolved) > 0 {
		list = append(list, ServerName)
	}
	list = appendIPs(list, l.WWWServers...)
	list = appendIPs(list, l.SIAddr, l.ServerID)
	list = appendIPs(list, l.TFTPServers...)
	if l.TFTP != "" {
		list = append(list, l.TFTP)
	}
	return ulist(list)
}

// tftpServers is tftp_download's list (onie-server, siaddr, option 66, option 150, option 54) or,
// without the server id, waterfall()'s, as addresses.
func (e *Env) tftpServers(ctx context.Context, withServerID bool) []string {
	l := e.lease
	var list []string
	if len(e.resolved) > 0 {
		list = append(list, e.resolved[0].String())
	}
	list = appendIPs(list, l.SIAddr)
	if l.TFTP != "" {
		if ip := net.ParseIP(l.TFTP); ip != nil {
			list = append(list, ip.String())
		} else if ips, _ := e.Resolve(ctx, l.DNS, l.TFTP); len(ips) > 0 {
			list = append(list, ips[0].String())
		}
	}
	list = appendIPs(list, l.TFTPServers...)
	if withServerID {
		list = appendIPs(list, l.ServerID)
	}
	return ulist(list)
}

// TFTPPaths are waterfall()'s paths: <MAC with dashes>/<default name>, then <address as 8
// upper-case hex digits>/<default name> down to a one-digit prefix, then every default name
// (exec_installer: `sed -e 's/:/-/g'` on onie_eth_addr, `printf %02X%02X%02X%02X`).
func (e *Env) TFTPPaths() []string {
	def := e.plat.DefaultName()
	var out []string
	if e.ethAddr != "" {
		out = append(out, strings.ReplaceAll(e.ethAddr, ":", "-")+"/"+def)
	}
	if ip4 := e.lease.Addr.IP.To4(); ip4 != nil {
		h := fmt.Sprintf("%02X%02X%02X%02X", ip4[0], ip4[1], ip4[2], ip4[3])
		for n := 8; n >= 1; n-- {
			out = append(out, h[:n]+"/"+def)
		}
	}
	return append(out, e.plat.InstallerNames()...)
}

// Headers are wget_run's request headers (facts D.5).
//
//   - User-Agent is `onie/1.0 (${os_ver}$onie_version; $bb_ver)`, os_ver being `uname -r -s`
//     with its space made a dash and bb_ver busybox's first line up to its date, its space made a
//     dash. ONIE's own kernel is named, not the container host's: Kernel, by default
//     4.19.143-onie+ (ONIE 2021.x's kernel line; a choice), and BusyBox-v1.25.1 (ONIE's
//     busybox; a choice).
//   - ONIE-ETH-ADDR is the management MAC, lower case (UNCONFIRMED 14).
//   - ONIE-SECURITY-KEY's format is UNCONFIRMED (14): the stand-in sends the first 16 hex digits
//     of the sha256 of the serial number, stable per switch.
//   - ONIE-VENDOR-ID is the vendor's IANA number (Platform.VendorID).
func (e *Env) Headers() http.Header {
	key := sha256.Sum256([]byte(e.id.Serial))
	// The keys are set as wget writes them, upper case, not in Go's canonical form (Set would
	// send Onie-Serial-Number); HTTP header names are case-insensitive, but a capture shows
	// ONIE's.
	h := http.Header{"User-Agent": {fmt.Sprintf("onie/1.0 (Linux-%s%s; BusyBox-v1.25.1)", firstNonEmpty(e.Kernel, "4.19.143-onie+"), e.Version)}}
	for _, kv := range [][2]string{
		{"ONIE-SERIAL-NUMBER", e.id.Serial},
		{"ONIE-ETH-ADDR", e.ethAddr},
		{"ONIE-VENDOR-ID", strconv.Itoa(e.plat.VendorID())},
		{"ONIE-MACHINE", e.plat.Machine},
		{"ONIE-MACHINE-REV", e.plat.Rev},
		{"ONIE-ARCH", e.plat.Arch},
		{"ONIE-SECURITY-KEY", hex.EncodeToString(key[:8])},
		{"ONIE-OPERATION", e.Operation},
		{"ONIE-VERSION", e.Version},
	} {
		h[kv[0]] = []string{kv[1]}
	}
	return h
}

// tryURL fetches one candidate and installs what it fetched. It reports whether a NOS installed.
func (e *Env) tryURL(ctx context.Context, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		e.logf("%s: not a URL: %v", raw, err)
		return false
	}
	switch u.Scheme {
	case "http", "https":
		return e.tryHTTP(ctx, raw)
	case "tftp":
		if e.TFTP == nil {
			e.logf("%s: no TFTP client, skipped", raw)
			return false
		}
		ok, _ := e.tryTFTP(ctx, u.Host, strings.TrimPrefix(u.Path, "/"))
		return ok
	}
	e.logf("%s: scheme %q not supported (http, https, tftp)", raw, u.Scheme)
	return false
}

func (e *Env) tryHTTP(ctx context.Context, raw string) bool {
	e.logf("fetching %s", raw)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, raw, nil)
	if err != nil {
		e.logf("%s: %v", raw, err)
		return false
	}
	req.Header = e.Headers()
	idle := time.AfterFunc(wgetTimeout, cancel)
	defer idle.Stop()
	resp, err := e.HTTP.Do(req)
	if err != nil {
		e.logf("%s: %v", raw, err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.logf("%s: %s", raw, resp.Status)
		return false
	}
	e.st.URL = raw
	e.setPhase(PhaseFetching)
	h := sha256.New()
	n, err := io.Copy(h, &idleReader{r: resp.Body, t: idle})
	if err != nil {
		return e.failInstall("downloading %s: %v", raw, err)
	}
	return e.install(ctx, raw, path.Base(resp.Request.URL.Path), hex.EncodeToString(h.Sum(nil)), n)
}

// tryTFTP fetches one file over TFTP and installs it. It reports whether a NOS installed and
// whether the server timed out.
func (e *Env) tryTFTP(ctx context.Context, server, file string) (ok, timedOut bool) {
	raw := "tftp://" + server + "/" + file
	e.logf("fetching %s", raw)
	h := sha256.New()
	cw := &countWriter{w: h}
	started := false
	cw.first = func() {
		started = true
		e.st.URL = raw
		e.setPhase(PhaseFetching)
	}
	if err := e.TFTP(ctx, server, file, cw); err != nil {
		timedOut = errors.Is(err, ErrTimeout)
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			timedOut = true
		}
		if started {
			return e.failInstall("downloading %s: %v", raw, err), timedOut
		}
		e.logf("%s: %v", raw, err)
		return false, timedOut
	}
	if !started {
		e.st.URL = raw
	}
	return e.install(ctx, raw, path.Base(file), hex.EncodeToString(h.Sum(nil)), cw.n), false
}

// install is what ONIE does with a fetched installer, modelled (D75): the file is recognised,
// not run. A file matching the switch's NOS installs after installTime; anything else fails as
// an installer whose /var/run/install.rc is not 0 fails, and the waterfall goes on.
func (e *Env) install(ctx context.Context, raw, name, sum string, size int64) bool {
	e.st.URL, e.st.SHA256 = raw, sum
	im, reason := e.match(sum, name, size)
	if reason != "" {
		return e.failInstall("%s: %s", raw, reason)
	}
	e.st.NOS, e.st.Version, e.st.Reason = im.NOS, im.Version, ""
	e.setPhase(PhaseInstalling)
	e.logf("installing %s %s from %s (%d bytes, sha256 %s), %s", im.NOS, im.Version, raw, size, sum, e.installDur)
	if e.powerLoss() {
		// onie-install-power-loss: the install is cut off half way; ONIE starts again
		if err := e.Sleep(ctx, e.installTime()/2); err != nil {
			return false
		}
		e.st.NOS, e.st.Version = "", ""
		return e.failInstall("%s: power was lost during the install (fault)", raw)
	}
	if err := e.Sleep(ctx, e.installTime()); err != nil {
		return false
	}
	e.setPhase(PhaseInstalled)
	e.logf("installed %s %s", im.NOS, im.Version)
	if e.DHCP != nil {
		if err := e.DHCP.Release(ctx, e.lease, []dhcpv4.Modifier{dhcpv4.WithGeneric(dhcpv4.OptionClientIdentifier, ClientID(e.id.Serial))}); err != nil {
			e.logf("releasing the lease: %v", err)
		}
	}
	if e.Net != nil {
		if err := e.Net.Flush(); err != nil {
			e.logf("flushing %s: %v", e.Iface, err)
		}
	}
	return true
}

func (e *Env) installTime() time.Duration { return e.installDur }

func (e *Env) failInstall(format string, a ...any) bool {
	e.st.Reason = fmt.Sprintf(format, a...)
	e.setPhase(PhaseFailed)
	e.logf("install failed: %s", e.st.Reason)
	return false
}

func (e *Env) hostPort(host string) string {
	if e.HTTPPort == 80 {
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(e.HTTPPort))
}

// dial is the HTTP transport's: a name is resolved through the lease's DNS servers, as busybox
// wget resolves through the resolv.conf udhcpc wrote.
func (e *Env) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: wgetTimeout}
	if net.ParseIP(host) == nil {
		var ips []net.IP
		if host == ServerName && len(e.resolved) > 0 {
			ips = e.resolved
		} else if e.lease != nil {
			ips, err = e.Resolve(ctx, e.lease.DNS, host)
		}
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("%s: does not resolve (%v)", host, errOr(err, "no address"))
		}
		addr = net.JoinHostPort(ips[0].String(), port)
	}
	return d.DialContext(ctx, network, addr)
}

// TFTPGet is the TFTP client (pin/tftp): an octet-mode read request to port 69, with busybox
// tftp's few retries.
func TFTPGet(_ context.Context, server, file string, w io.Writer) error {
	c, err := tftp.NewClient(net.JoinHostPort(server, "69"))
	if err != nil {
		return err
	}
	c.SetTimeout(2 * time.Second)
	c.SetRetries(2)
	wt, err := c.Receive(file, "octet")
	if err != nil {
		return err
	}
	_, err = wt.WriteTo(w)
	return err
}

type idleReader struct {
	r io.Reader
	t *time.Timer
}

func (r *idleReader) Read(p []byte) (int, error) {
	r.t.Reset(wgetTimeout)
	return r.r.Read(p)
}

type countWriter struct {
	w     io.Writer
	n     int64
	first func()
}

func (c *countWriter) Write(p []byte) (int, error) {
	if c.n == 0 && c.first != nil {
		c.first()
		c.first = nil
	}
	c.n += int64(len(p))
	return c.w.Write(p)
}

func appendIPs(list []string, ips ...net.IP) []string {
	for _, ip := range ips {
		if ip != nil && !ip.IsUnspecified() {
			list = append(list, ip.String())
		}
	}
	return list
}

// ulist is ONIE's ulist: the list with later repeats dropped.
func ulist(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// powerLoss reports whether the twin's onie-install-power-loss fault holds (a file in the
// switch's /etc/dctwin/faults).
func (e *Env) powerLoss() bool {
	_, err := os.Stat(filepath.Join(e.Dir, "faults", "onie-power-loss"))
	return err == nil
}

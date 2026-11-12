// Package onie is the twin's stand-in for ONIE's install environment (18 — ONIE, D75): what a
// factory switch with `install: onie` runs on its management port, eth0, before it has a NOS. The
// switch's start script runs `onie-discover install` and waits for it to exit 0, then boots the
// NOS the twin's registry has for it, in factory state.
//
// Discovery and the fetch are real, as D75 has them: the program does DHCPv4 itself on eth0 with
// ONIE's vendor class, client id, user class and vendor-identifying options and ONIE's parameter
// request list, configures the lease, and walks ONIE's discovery waterfall (exact URLs from the
// lease, then HTTP to the onie-server name and the lease's servers with ONIE's default installer
// names, then TFTP), sending ONIE's request headers. What it fetches it downloads in full and
// hashes. The install is not run: the file's sha256 or its name is matched against the images the
// twin wrote to /etc/dctwin/images.json (spec.adapters.network.nos.images), and a match for the
// switch's NOS "installs" it after installTime. A file that matches nothing, or matches another
// NOS's image, fails as an ONIE install fails, and ONIE goes on with its waterfall, then waits
// 20 s and starts discovery again, for ever, as ONIE's exec_installer loops.
//
// The behaviour follows ONIE's own scripts, read at opencomputeproject/onie@ac03c31f5ffd and
// recorded in docs/reviews/2026-11-11-day0-vendor-facts.md section D: rootconf/default/bin/discover
// (sd_dhcp4), rootconf/default/bin/exec_installer (url_run, http_download, tftp_download,
// waterfall, wget_run) and rootconf/default/lib/onie/functions (udhcpc_args,
// get_default_filenames). Where that section marks a fact UNCONFIRMED, the value chosen here is
// named where it is used.
//
// Everything the program touches outside itself (the files, the interface, the DHCP client, the
// network configuration, the resolver, TFTP, the clock) is a field of Env, so the waterfall, the
// headers, the matching and the status are unit-tested without root (onie_test.go). The real
// DHCP client and netlink configuration are Linux-only (dhcp_linux.go).
package onie

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// RetryDelay is how long ONIE waits between two passes of its waterfall: exec_installer's
// `delay=20` (facts D.3, step 10).
const RetryDelay = 20 * time.Second

// ServerName is the DNS name ONIE tries as an HTTP and TFTP server: lib/onie/functions'
// onie_server_name (facts D.3).
const ServerName = "onie-server"

// IANAEnterprise is ONIE's enterprise number in DHCP option 125, the Open Compute Project's
// (lib/onie/functions onie_iana_enterprise, facts D.1).
const IANAEnterprise = 42623

// DefaultVersion is the ONIE version the stand-in reports (ONIE-VERSION and the User-Agent) when
// the twin does not say otherwise. ONIE builds are dated YYYY.MM; a switch's vendor ONIE carries
// its own, so this is a choice, not a fact.
const DefaultVersion = "2021.08"

// Factory is /etc/dctwin/factory.json, as the TAP substrate writes it (prepareDay0): how the
// switch is installed, whether its ZTP runs, which NOS the twin's registry boots for it, and how
// long ONIE's install takes (a Go duration, already scaled by spec.timeScale).
type Factory struct {
	Install     string `json:"install"`
	ZTP         bool   `json:"ztp"`
	NOS         string `json:"nos"`
	InstallTime string `json:"installTime"`
}

// Env is everything the stand-in reads, writes and calls. Main fills it for a real switch; tests
// fill it with fakes. A zero field takes the default New gives it.
type Env struct {
	// Dir is where the twin's inputs are (nos.Day0Dir); State where the status is written
	// (nos.Day0State + "/onie").
	Dir, State string
	// Iface is the management port ONIE runs on (nos.MgmtIf).
	Iface string
	// DHCP leases on Iface; Net applies a lease to Iface and removes it again.
	DHCP DHCPClient
	Net  NetConfig
	// Resolve looks a name up through the lease's DNS servers. It returns no address, and no
	// error, when there are no servers to ask.
	Resolve func(ctx context.Context, servers []net.IP, name string) ([]net.IP, error)
	// Probe is ONIE's `nc -w 10 <server> 80` liveness check before it tries a server's names.
	Probe func(ctx context.Context, host string, port int) bool
	// HTTP fetches the HTTP candidates. By default its transport resolves names through the
	// lease's DNS (Env.dial) with wget's timeouts.
	HTTP *http.Client
	// HTTPPort is the port the server-list candidates are fetched from: 80, as ONIE's URLs carry
	// no port. Tests set an httptest server's.
	HTTPPort int
	// TFTP fetches one file from a TFTP server into w. Nil skips TFTP, logged.
	TFTP func(ctx context.Context, server, file string, w io.Writer) error
	// Sleep waits, or returns ctx's error.
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time
	// Out receives the human log, one `onie: ` line each.
	Out io.Writer
	// RejectAll is the twin's onie-image-mismatch fault (day0.onieRejectAll): every installer
	// fails its checks. The fault can also be raised while the program runs, by the file
	// <Dir>/onie-reject-all appearing.
	RejectAll bool
	// Retry is the delay between waterfall passes (RetryDelay).
	Retry time.Duration
	// Version is the ONIE version reported (DefaultVersion); Kernel the kernel release the
	// User-Agent names.
	Version, Kernel string
	// Operation is ONIE's onie_operation: os-install (the only one built).
	Operation string

	id         nos.Identity
	plat       Platform
	images     []nos.Image
	factory    Factory
	installDur time.Duration
	st         Status
	lease      *Lease
	ethAddr    string
	resolved   []net.IP     // onie-server's addresses in this pass
	onWrite    func(Status) // tests: every status written
}

// Install runs ONIE's discovery until a NOS installs (nil) or ctx ends.
func (e *Env) Install(ctx context.Context) error {
	if err := e.load(); err != nil {
		return err
	}
	e.defaults()
	e.st = Status{Phase: PhaseDiscovering}
	e.logf("ONIE %s on %s, platform %s, serial %s, waiting for %s", e.Version, e.Iface, e.plat, e.id.Serial, e.factory.NOS)
	for pass := 1; ; pass++ {
		e.st.Attempts = pass
		e.setPhase(PhaseDiscovering)
		if e.discover(ctx) && e.waterfall(ctx) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.logf("no installer found, starting discovery again in %s", e.Retry)
		if err := e.Sleep(ctx, e.Retry); err != nil {
			return err
		}
	}
}

// load reads the twin's inputs: the switch's identity, the images ONIE recognises and the
// factory settings.
func (e *Env) load() error {
	for name, v := range map[string]any{"identity.json": &e.id, "images.json": &e.images, "factory.json": &e.factory} {
		b, err := os.ReadFile(filepath.Join(e.Dir, name))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, v); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	p, err := ParsePlatform(e.id.Platform)
	if err != nil {
		return fmt.Errorf("identity.json: %w", err)
	}
	e.plat = p
	if e.factory.NOS == "" {
		return fmt.Errorf("factory.json names no NOS to install")
	}
	if e.factory.InstallTime != "" {
		if e.installDur, err = time.ParseDuration(e.factory.InstallTime); err != nil {
			return fmt.Errorf("factory.json installTime: %w", err)
		}
	}
	for i, im := range e.images {
		if im.SHA256 == "" && im.Name == "" {
			return fmt.Errorf("images.json: image %d (%s) has neither sha256 nor name", i, im.NOS)
		}
		if _, err := nameRegexp(im.Name); err != nil {
			return fmt.Errorf("images.json: image %d (%s): name: %w", i, im.NOS, err)
		}
	}
	// ONIE-ETH-ADDR and the TFTP waterfall's MAC directory are onie_eth_addr, the management
	// MAC from onie-sysinfo -e. Its hex case is UNCONFIRMED (facts D.5, UNCONFIRMED 14); the
	// stand-in uses lower case, as Linux prints a MAC (sysfs, ip link).
	e.ethAddr = strings.ToLower(firstNonEmpty(e.id.MgmtMAC, e.id.BaseMAC))
	return nil
}

func (e *Env) defaults() {
	if e.Iface == "" {
		e.Iface = nos.MgmtIf
	}
	if e.Retry == 0 {
		e.Retry = RetryDelay
	}
	if e.HTTPPort == 0 {
		e.HTTPPort = 80
	}
	if e.Sleep == nil {
		e.Sleep = sleep
	}
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.Out == nil {
		e.Out = io.Discard
	}
	if e.Version == "" {
		e.Version = DefaultVersion
	}
	if e.Operation == "" {
		e.Operation = "os-install"
	}
	if e.Probe == nil {
		e.Probe = probe
	}
	if e.Resolve == nil {
		e.Resolve = DNSResolve
	}
	if e.HTTP == nil {
		e.HTTP = &http.Client{Transport: &http.Transport{
			DialContext:           e.dial,
			ResponseHeaderTimeout: wgetTimeout,
		}}
	}
}

// discover is ONIE's service discovery for one pass: DHCPv4 on the management port (sd_dhcp4),
// the lease applied to the port, and the onie-server name resolved. DHCPv6, mDNS and the
// fallback are stubs in ONIE itself (facts D.2) and are not attempted.
func (e *Env) discover(ctx context.Context) bool {
	if e.DHCP == nil {
		e.fail("no DHCP client on %s", e.Iface)
		return false
	}
	l, err := e.DHCP.Lease(ctx, Modifiers(e.id, e.plat))
	if err != nil {
		e.st.Reason = fmt.Sprintf("no DHCP lease on %s: %v", e.Iface, err)
		e.write()
		e.logf("%s", e.st.Reason)
		return false
	}
	if e.Net != nil {
		if err := e.Net.Apply(l); err != nil {
			e.st.Reason = fmt.Sprintf("configuring the lease on %s: %v", e.Iface, err)
			e.write()
			e.logf("%s", e.st.Reason)
			return false
		}
	}
	e.lease = l
	e.logf("lease %s from %s%s", l.Addr, l.ServerID, l.describe())
	e.resolved = nil
	if len(l.DNS) == 0 {
		e.logf("%s: the lease names no DNS server, skipped", ServerName)
	} else if ips, err := e.resolveServer(ctx); err != nil || len(ips) == 0 {
		e.logf("%s: does not resolve (%v)", ServerName, errOr(err, "no address"))
	} else {
		e.resolved = ips
	}
	return true
}

// resolveServer looks onie-server up as busybox's resolver would with the lease's resolv.conf:
// the bare name, then under the lease's domain (option 15).
func (e *Env) resolveServer(ctx context.Context) ([]net.IP, error) {
	ips, err := e.Resolve(ctx, e.lease.DNS, ServerName)
	if (err != nil || len(ips) == 0) && e.lease.Domain != "" {
		return e.Resolve(ctx, e.lease.DNS, ServerName+"."+e.lease.Domain)
	}
	return ips, err
}

func (e *Env) logf(format string, a ...any) {
	fmt.Fprintf(e.Out, "onie: "+format+"\n", a...)
}

// DNSResolve looks a name up at the first of the lease's DNS servers, as busybox's resolver
// does with the resolv.conf udhcpc's script writes. The name is asked rooted, so the
// container's own resolv.conf search list plays no part.
func DNSResolve(ctx context.Context, servers []net.IP, name string) ([]net.IP, error) {
	if len(servers) == 0 {
		return nil, nil
	}
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		d := net.Dialer{Timeout: wgetTimeout}
		return d.DialContext(ctx, network, net.JoinHostPort(servers[0].String(), "53"))
	}}
	cctx, cancel := context.WithTimeout(ctx, 2*wgetTimeout)
	defer cancel()
	return r.LookupIP(cctx, "ip4", strings.TrimSuffix(name, ".")+".")
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// probe is `nc -w 10 <server> 80`: a TCP connection within 10 s.
func probe(ctx context.Context, host string, port int) bool {
	d := net.Dialer{Timeout: 10 * time.Second}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(port)))
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func errOr(err error, s string) any {
	if err != nil {
		return err
	}
	return s
}

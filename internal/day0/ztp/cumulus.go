package ztp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// Cumulus Linux 5.x's ZTP, after NVIDIA's Zero-Touch-Provisioning page (5.0 and 5.14, section A
// of docs/reviews/2026-11-11-day0-vendor-facts.md).
//
// Sources, in order (A.1): a script in /var/lib/cumulus/ztp, searched once at boot under the
// ONIE-style names (most specific first); a USB drive, which the twin does not model (logged and
// skipped); and the URL of DHCP option 239 on eth0, looked for every 10 s for up to 5 minutes.
// The first source found is the one run.
//
// The script (A.3, A.4) is fetched with Cumulus's identifying headers, must contain
// CUMULUS-AUTOPROVISIONING or is refused unrun, and runs as root; its output goes to ZTP's log.
// Exit 0 is success: ZTP marks the switch provisioned and disables itself. Anything else is a
// failure, logged as Cumulus logs it, and retried.
//
// Retry, which the docs show only as "Retrying" lines (UNCONFIRMED 4), is the twin's choice: the
// same source is tried again every 10 s (the discovery interval) until it succeeds, the DHCP
// window of 5 minutes from the first attempt closes, or CUMULUS-PROV-COUNT reaches
// CUMULUS-PROV-MAX; then ZTP logs "ZTP script failed. Exiting..." and the result is failed. The
// two headers are read as an attempt counter and its ceiling (UNCONFIRMED 2): the count is of
// fetches, persists across boots in ZTP's state and is cleared by `ztp -R`; the ceiling is the
// documented example value, 32. A manual `ztp -r` makes one attempt and does not retry.
//
// After a failure, or a window with no option 239, the daemon goes on watching eth0's lease and
// looks again when the option appears or changes, as Cumulus's DHCP-triggered ZTP runs again when
// the management cable is plugged in (A.2), until the switch is provisioned.
//
// ZTP does not run once the switch is provisioned, nor once "manual configuration changes were
// made" (A.7), which the twin reads as an NVUE revision applied by a client other than the twin
// itself: the twin touches /run/dctwin/nvue-user-applied when one is, and ZTP then records
// "Switch manually configured" and disables itself, as `ztp -s` shows it in the docs.

const (
	clMarker  = "CUMULUS-AUTOPROVISIONING"
	clCascade = "CUMULUS-AUTOPROVISIONING-CASCADE"
	clAgent   = "CumulusLinux-AutoProvision/0.4"
	// clManual is the Method `ztp -s` shows for a switch configured by hand (A.5).
	clManual = "Switch manually configured"
)

// clState is ZTP's own state, kept in /var/lib/cumulus/ztp/ztp_state.json; `ztp -s` prints it.
type clState struct {
	State  string `json:"state"` // enabled or disabled
	Result string `json:"result,omitempty"`
	Date   string `json:"date,omitempty"` // RFC 3339
	Method string `json:"method,omitempty"`
	URL    string `json:"url,omitempty"`
	// Count is CUMULUS-PROV-COUNT: fetches made since the last reset.
	Count int `json:"provCount"`
}

type cumulus struct {
	reporter
	c  *config
	id nos.Identity
}

// clSource is a script to run: Method as `ztp -s` names it, the source for the twin's status,
// and the URL or path.
type clSource struct{ method, source, url string }

type outcome int

const (
	outSuccess outcome = iota
	outFailed
	outStopped // disabled, manually configured or cancelled
)

func newCumulus(c *config, echo bool) *cumulus {
	z := &cumulus{c: c}
	z.reporter = reporter{c: c, log: newLogger(c, echo, filepath.Join(c.clDir, "ztp_state.log"), c.autoprovLog)}
	id, err := c.identity()
	if err != nil {
		z.log.printf("ZTP: no identity (%v): the CUMULUS-* headers go out empty", err)
	}
	z.id = id
	z.st = twinStatus{Flavour: "cumulus", ExitCode: -1}
	if prev := c.readStatus(); prev.Flavour == "cumulus" {
		z.st = prev
	}
	return z
}

func (z *cumulus) stateFile() string { return filepath.Join(z.c.clDir, "ztp_state.json") }

func (z *cumulus) load() clState {
	var s clState
	if b, err := os.ReadFile(z.stateFile()); err == nil && json.Unmarshal(b, &s) == nil && s.State != "" {
		return s
	}
	s.State = "enabled"
	if !z.c.factoryZTP() {
		s.State = "disabled"
	}
	return s
}

func (z *cumulus) store(s clState) {
	if err := writeJSON(z.stateFile(), s); err != nil {
		z.log.printf("ZTP: cannot save its state: %v", err)
	}
}

// headers are the CUMULUS-* request headers (A.3), from the switch's identity: ARCH its
// architecture (x86_64 when the identity has none), MANUFACTURER and PRODUCTNAME its
// manufacturer and model as the twin's identity spells them (the docs' examples, "odm" and
// "switch_model", say nothing of case), BASE-MAC and MGMT-MAC upper-case and colon-separated as
// the docs show them, and BUILD and VERSION both the NOS version (the docs' examples give the
// two the same value, 5.0.0 or 5.1.0). CUMULUS-LICENSE-INSTALLED and CUMULUS-PROD-ID are not in
// 5.x's table and are not sent.
func (z *cumulus) headers(count int) http.Header {
	arch := z.id.Arch
	if arch == "" {
		arch = "x86_64"
	}
	h := http.Header{}
	set := func(k, v string) { h[k] = []string{v} }
	set("User-Agent", clAgent)
	set("CUMULUS-ARCH", arch)
	set("CUMULUS-BUILD", z.id.Version)
	set("CUMULUS-MANUFACTURER", z.id.Manufacturer)
	set("CUMULUS-PRODUCTNAME", z.id.Model)
	set("CUMULUS-SERIAL", z.id.Serial)
	set("CUMULUS-BASE-MAC", strings.ToUpper(z.id.BaseMAC))
	set("CUMULUS-MGMT-MAC", strings.ToUpper(z.id.MgmtMAC))
	set("CUMULUS-VERSION", z.id.Version)
	set("CUMULUS-PROV-COUNT", fmt.Sprint(count))
	set("CUMULUS-PROV-MAX", fmt.Sprint(z.c.provMax))
	return h
}

var revSuffix = regexp.MustCompile(`-r[0-9]+$`)

// localNames are the file names ZTP looks for in its directory, most specific first (A.1):
// cumulus-ztp-<arch>-<vendor>_<model>-r<revision>, then without the revision, then
// <vendor>_<model> alone, then <arch>, then plain cumulus-ztp; from the identity's ONIE
// platform string, <arch>-<vendor>_<machine>-r<revision>.
func (z *cumulus) localNames() []string {
	var names []string
	add := func(s string) {
		for _, n := range names {
			if n == s {
				return
			}
		}
		names = append(names, s)
	}
	arch := z.id.Arch
	if p := z.id.Platform; p != "" {
		add("cumulus-ztp-" + p)
		noRev := revSuffix.ReplaceAllString(p, "")
		add("cumulus-ztp-" + noRev)
		if a, machine, ok := strings.Cut(noRev, "-"); ok {
			add("cumulus-ztp-" + machine)
			if arch == "" {
				arch = a
			}
		}
	}
	if arch != "" {
		add("cumulus-ztp-" + arch)
	}
	add("cumulus-ztp")
	return names
}

func (z *cumulus) findLocal() string {
	for _, n := range z.localNames() {
		p := filepath.Join(z.c.clDir, n)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

var errNoMarker = errors.New("no " + clMarker + " marker")

// attempt fetches and runs one script. It counts the fetch (CUMULUS-PROV-COUNT is the count
// before it) and returns the script's exit code, or -1 and the reason it did not run.
func (z *cumulus) attempt(ctx context.Context, src clSource) (int, error) {
	st := z.load()
	f := z.c.newFetcher(z.headers(st.Count), true)
	st.Count++
	z.store(st)
	z.st.Attempts, z.st.Source, z.st.URL = st.Count, src.source, src.url
	z.set(stateRunning, fmt.Sprintf("%s: fetching %s (attempt %d)", src.method, src.url, st.Count))
	body, err := f.get(ctx, src.url)
	if err != nil {
		z.log.printf("%s: cannot fetch %s: %v", src.method, src.url, err)
		return -1, err
	}
	if !bytes.Contains(body, []byte(clMarker)) {
		z.log.printf("%s: Marker %s not found in %s: refusing to run it", src.method, clMarker, src.url)
		return -1, errNoMarker
	}
	z.log.printf("%s: Found Marker %s", src.method, clMarker)
	if bytes.Contains(body, []byte(clCascade)) {
		z.log.printf("%s: Found Marker %s: no later source to continue to on the twin", src.method, clCascade)
	}
	z.log.printf("%s: Executing %s", src.method, src.url)
	out := z.log.writer()
	code, err := z.c.runScript(ctx, body, out, nil)
	out.Close()
	z.st.ExitCode = code
	if err != nil {
		z.log.printf("%s: cannot run %s: %v", src.method, src.url, err)
		return -1, err
	}
	if code != 0 {
		z.log.printf("%s: Payload returned code %d", src.method, code)
		z.log.printf("%s: Script returned failure", src.method)
	} else {
		z.log.printf("%s: Script returned success", src.method)
	}
	return code, nil
}

func (z *cumulus) succeed(src clSource) {
	st := z.load()
	st.State, st.Result, st.Method, st.URL = "disabled", "success", src.method, src.url
	st.Date = z.c.now().UTC().Format(time.RFC3339)
	z.store(st)
	z.st.ExitCode = 0
	z.set(stateSuccess, fmt.Sprintf("%s: %s returned success; ZTP is disabled", src.method, src.url))
}

func (z *cumulus) fail(src clSource, msg string) {
	st := z.load()
	st.Result, st.Method, st.URL = "failed", src.method, src.url
	st.Date = z.c.now().UTC().Format(time.RFC3339)
	z.store(st)
	z.set(stateFailed, msg)
}

// halted reports whether ZTP must stop: cancelled, disabled (`ztp -d`) or manually configured.
func (z *cumulus) halted(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	if exists(z.c.userApplied) {
		z.manuallyConfigured()
		return true
	}
	if z.load().State == "disabled" {
		z.set(stateDisabled, "ZTP was disabled (ztp -d)")
		return true
	}
	return false
}

func (z *cumulus) manuallyConfigured() {
	st := z.load()
	st.State, st.Result, st.Method, st.URL = "disabled", "success", clManual, ""
	st.Date = z.c.now().UTC().Format(time.RFC3339)
	z.store(st)
	z.set(stateDisabled, "ZTP Manual: the switch was configured by hand (an NVUE revision applied by another client); ZTP is disabled")
}

// provision runs a source until it succeeds or the retry policy above gives up; resolve names
// the source afresh for each attempt (a lease can change between them).
func (z *cumulus) provision(ctx context.Context, resolve func() (clSource, bool)) outcome {
	deadline := z.c.now().Add(z.c.window)
	for {
		if z.halted(ctx) {
			return outStopped
		}
		src, ok := resolve()
		if !ok {
			z.set(stateFailed, "ZTP DHCP: the lease no longer carries option 239")
			return outFailed
		}
		if n := z.load().Count; n >= z.c.provMax {
			z.fail(src, fmt.Sprintf("%s: CUMULUS-PROV-COUNT %d reached CUMULUS-PROV-MAX %d: not trying again (ztp -R re-arms)", src.method, n, z.c.provMax))
			return outFailed
		}
		code, err := z.attempt(ctx, src)
		if err == nil && code == 0 {
			z.succeed(src)
			return outSuccess
		}
		if ctx.Err() != nil {
			return outStopped
		}
		reason := fmt.Sprintf("exit code %d", code)
		if err != nil {
			reason = err.Error()
		}
		if z.load().Count >= z.c.provMax || !z.c.now().Before(deadline) {
			z.log.printf("ZTP script failed. Exiting...")
			z.fail(src, fmt.Sprintf("%s: %s failed (%s) after %d attempts", src.method, src.url, reason, z.load().Count))
			return outFailed
		}
		z.log.printf("%s: %s failed (%s). Retrying in %s", src.method, src.url, reason, z.c.interval)
		if sleep(ctx, z.c.interval) != nil {
			return outStopped
		}
	}
}

// daemon is ZTP's run at boot.
func (z *cumulus) daemon(ctx context.Context) int {
	st := z.load()
	switch {
	case exists(z.c.userApplied) && !(st.State == "disabled" && st.Result == "success" && st.Method != clManual):
		z.manuallyConfigured()
		return 0
	case st.State == "disabled" && st.Result == "success" && st.Method != clManual:
		z.set(stateSuccess, fmt.Sprintf("already provisioned by %s on %s", st.Method, st.Date))
		return 0
	case st.State == "disabled":
		z.set(stateDisabled, "ZTP is disabled (ztp -e enables it)")
		return 0
	}
	release, held, err := lock(z.c.lockFile)
	if err != nil || held {
		z.log.printf("ZTP: another ZTP run holds %s (%v)", z.c.lockFile, err)
		return 1
	}
	defer release()
	z.st.Attempts = st.Count
	z.set(stateDiscovering, "ZTP: looking for a provisioning script")

	if p := z.findLocal(); p != "" {
		z.log.printf("ZTP Local: found %s", p)
		src := clSource{"ZTP Local", "local", p}
		if z.provision(ctx, func() (clSource, bool) { return src, true }) == outFailed {
			return 1
		}
		return 0
	}
	z.log.printf("ZTP Local: no script in %s (%s)", z.c.clDir, strings.Join(z.localNames(), ", "))
	z.log.printf("ZTP USB: USB drives are not modelled on the twin: skipped")

	for {
		switch z.discoverDHCP(ctx) {
		case outSuccess:
			return 0
		case outStopped:
			return 0
		}
		if !z.waitLeaseChange(ctx) {
			return 0
		}
		z.set(stateDiscovering, "ZTP DHCP: eth0's lease changed: looking again")
	}
}

func (z *cumulus) dhcpSource() (clSource, bool) {
	u := z.c.lease().option(opt239)
	return clSource{"ZTP DHCP", "dhcp-239", u}, u != ""
}

// discoverDHCP looks for option 239 every interval for the window (A.1), and runs it.
func (z *cumulus) discoverDHCP(ctx context.Context) outcome {
	deadline := z.c.now().Add(z.c.window)
	for {
		if z.halted(ctx) {
			return outStopped
		}
		if src, ok := z.dhcpSource(); ok {
			z.log.printf("ZTP DHCP: found option 239: %s", src.url)
			return z.provision(ctx, z.dhcpSource)
		}
		if !z.c.now().Before(deadline) {
			z.set(stateFailed, fmt.Sprintf("ZTP: no provisioning script: none in %s, USB not modelled, no DHCP option 239 on %s in %s",
				z.c.clDir, nos.MgmtIf, z.c.window))
			return outFailed
		}
		if sleep(ctx, z.c.interval) != nil {
			return outStopped
		}
	}
}

// leaseKey is what of the lease would make Cumulus look again: whether it holds, and its 239.
func (z *cumulus) leaseKey() string {
	l := z.c.lease()
	return fmt.Sprintf("%t|%s", l.bound(), l.option(opt239))
}

// waitLeaseChange waits until the lease brings an option 239 it did not carry before; false
// when ZTP is to stop instead.
func (z *cumulus) waitLeaseChange(ctx context.Context) bool {
	was := z.leaseKey()
	for {
		if sleep(ctx, z.c.interval) != nil || z.halted(ctx) {
			return false
		}
		if now := z.leaseKey(); now != was {
			if _, ok := z.dhcpSource(); ok {
				return true
			}
			was = now
		}
	}
}

// cumulusCLI is /usr/sbin/ztp (A.5): -s, -e, -d, -r [URL|path], -R and -v.
func (c *config) cumulusCLI(ctx context.Context, args []string) int {
	var show, enable, disable, reset, runNow, verbose bool
	var target string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-s" || a == "--status":
			show = true
		case a == "-e" || a == "--enable":
			enable = true
		case a == "-d" || a == "--disable":
			disable = true
		case a == "-R" || a == "--reset":
			reset = true
		case a == "-v" || a == "--verbose":
			verbose = true
		case a == "-r" || a == "--run":
			runNow = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				target = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--run="):
			runNow, target = true, strings.TrimPrefix(a, "--run=")
		default:
			fmt.Fprintf(c.stderr, "ztp: unknown option %q\n%s\n", a, usage)
			return 2
		}
	}
	if enable && disable {
		fmt.Fprintln(c.stderr, "ztp: -e and -d are exclusive")
		return 2
	}
	z := newCumulus(c, verbose)
	rc := 0
	if reset {
		// The docs' reset removes the ztp directory; the twin removes ZTP's state and log in it
		// and keeps a script a site placed there, which is input, not state.
		for _, f := range []string{z.stateFile(), filepath.Join(c.clDir, "ztp_state.log"), c.statusFile} {
			if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(c.stderr, "ztp: %v\n", err)
				rc = 1
			}
		}
		z = newCumulus(c, verbose)
		z.st = twinStatus{Flavour: "cumulus", ExitCode: -1}
	}
	if enable || disable {
		st := z.load()
		st.State = map[bool]string{true: "enabled", false: "disabled"}[enable]
		z.store(st)
		if disable {
			if release, held, _ := lock(c.lockFile); !held && release != nil {
				z.set(stateDisabled, "ZTP was disabled (ztp -d)")
				release()
			}
		}
	}
	if runNow {
		rc = z.runNow(ctx, target)
	}
	if show {
		z.show()
	}
	return rc
}

// runNow is `ztp -r`: one attempt, ignoring ZTP's state ("force provisioning, ignoring the
// status in the configuration file", 5.0) and the attempt ceiling.
func (z *cumulus) runNow(ctx context.Context, target string) int {
	release, held, err := lock(z.c.lockFile)
	if err != nil {
		fmt.Fprintf(z.c.stderr, "ztp: %v\n", err)
		return 1
	}
	if held {
		fmt.Fprintln(z.c.stderr, "ztp: ZTP is already running")
		return 1
	}
	defer release()
	var src clSource
	switch {
	case target != "":
		src = clSource{"ZTP Manual", "manual", target}
	case z.findLocal() != "":
		src = clSource{"ZTP Local", "local", z.findLocal()}
	default:
		var ok bool
		if src, ok = z.dhcpSource(); !ok {
			z.set(stateFailed, "ZTP Manual: no script given, none in "+z.c.clDir+" and no DHCP option 239")
			fmt.Fprintln(z.c.stderr, "ztp: no provisioning script found")
			return 1
		}
	}
	code, err := z.attempt(ctx, src)
	if err == nil && code == 0 {
		z.succeed(src)
		fmt.Fprintln(z.c.stdout, "ZTP: provisioning succeeded")
		return 0
	}
	msg := fmt.Sprintf("%s: %s failed (exit code %d)", src.method, src.url, code)
	if err != nil {
		msg = fmt.Sprintf("%s: %s failed: %v", src.method, src.url, err)
	}
	z.fail(src, msg)
	fmt.Fprintln(z.c.stderr, "ztp: "+msg)
	return 1
}

// show is `ztp -s`, as the docs print it (A.5).
func (z *cumulus) show() {
	st := z.load()
	none := func(s string) string {
		if s == "" {
			return "None"
		}
		return s
	}
	date := ""
	if t, err := time.Parse(time.RFC3339, st.Date); err == nil {
		date = t.UTC().Format("Mon Jan 2 15:04:05 2006 MST")
	}
	w := z.c.stdout
	fmt.Fprintln(w, "ZTP INFO:")
	for _, kv := range [][2]string{
		{"State", st.State}, {"Version", "1.0"}, {"Result", none(st.Result)},
		{"Date", none(date)}, {"Method", none(st.Method)}, {"URL", none(st.URL)},
	} {
		fmt.Fprintf(w, "%-15s%s\n", kv[0], kv[1])
	}
}

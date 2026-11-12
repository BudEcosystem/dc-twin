package ztp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// SONiC's ZTP, after sonic-ztp (github.com/sonic-net/sonic-ztp at 036c8eaad345) and SONiC's ZTP
// design (doc/ztp/ztp.md): section E of docs/reviews/2026-11-11-day0-vendor-facts.md.
//
// Precedence (E.1), looked at every discovery-interval (10 s, E.2) while ZTP's admin mode is
// true:
//
//  0. A configuration the user made skips ZTP (sonic-ztp's MANUAL_CONFIG). sonic-ztp's own test
//     is /etc/sonic/minigraph.xml, or /etc/sonic/config_db.json with monitor-startup-config; but
//     a docker-sonic-vs container always has a config_db.json (its factory one), so the twin's
//     test is: /etc/sonic/ztp-skip exists (a marker the twin or the user may create), or
//     minigraph.xml exists, or config_db.json's DEVICE_METADATA|localhost hostname is set and
//     is not the factory "sonic".
//  1. /host/ztp/ztp_data_local.json (local-fs).
//  2. DHCP option 67, the URL of a ZTP JSON document (dhcp-opt67), prefixed tftp://<option 66>/
//     when it has no scheme and option 66 is present (and then refused, tftp being unsupported).
//  3. DHCP option 239, a provisioning script, run through a document sonic-ztp synthesises for it
//     (dhcp-opt239).
//
// DHCPv6's options 59 and 239 and the deprecated 225/226 have no counterpart here: the twin's
// management network leases IPv4 only (D73), and minigraph is refused by name (graphservice).
//
// The document (E.5): its top-level "ztp" object's dict-valued keys are its sections, run in
// lexical order of their names; a leading NNN- is stripped to name the plugin, unless "plugin"
// names it ("plugin": "name", or {"name"}, or a script at {"url"} or {"dynamic-url"}, which take
// precedence in that order, E.4). Each section's ignore-result, halt-on-failure,
// reboot-on-success and reboot-on-failure are honoured, the engine filling in their defaults
// (false) and each section's "status" ("BOOT" when absent; values outside E.5's six are DISABLED),
// "start-timestamp", "timestamp" and "exit-code", and the document's own "status" and timestamps,
// as sonic-ztp does, saving the document to /host/ztp/ztp_data.json after each step. A section
// already SUCCESS (or DISABLED) is not run again, so a daemon started on a document left
// IN-PROGRESS resumes it. A plugin's exit 0 is SUCCESS, its suspend-exit-code SUSPEND (run again
// after the other sections, up to three passes, then FAILED), anything else FAILED. The twin
// reads ignore-result as sonic-ztp's design describes it: a failed section with ignore-result is
// marked SUCCESS (its exit code kept). The document is SUCCESS when every section is (or the
// document's own ignore-result is true), FAILED otherwise; halt-on-failure stops at the first
// failed section, leaving the rest BOOT.
//
// Reboots: reboot-on-success and reboot-on-failure (a section's or the document's) are logged and
// ZTP continues, since the twin cannot reboot a switch's container from inside it; a reboot is
// the twin's to model (18, "Faults during day 0").
//
// Not modelled: restart-ztp-no-config, restart-ztp-on-failure and config-fallback (ZTP ends at
// the document's outcome, as with their defaults on a switch that has a config_db.json), the
// document's encryption and curl-arguments, and ztp_cfg.json's settings other than admin-mode.

const (
	sonicAgent = "SONiC-ZTP/0.1"
	tsFormat   = "2006-01-02 15:04:05 MST"

	stBoot       = "BOOT"
	stInProgress = "IN-PROGRESS"
	stSuccess    = "SUCCESS"
	stFailed     = "FAILED"
	stSuspend    = "SUSPEND"
	stDisabled   = "DISABLED"
)

type sonic struct {
	reporter
	c  *config
	id nos.Identity
}

func newSonic(c *config, echo bool) *sonic {
	z := &sonic{c: c}
	z.reporter = reporter{c: c, log: newLogger(c, echo, c.ztpLog)}
	id, err := c.identity()
	if err != nil {
		z.log.printf("no identity (%v): the identifying headers go out empty", err)
	}
	z.id = id
	z.st = twinStatus{Flavour: "sonic", ExitCode: -1}
	if prev := c.readStatus(); prev.Flavour == "sonic" {
		z.st = prev
	}
	return z
}

func (z *sonic) dataFile() string   { return filepath.Join(z.c.ztpDir, "ztp_data.json") }
func (z *sonic) cfgFile() string    { return filepath.Join(z.c.ztpDir, "ztp_cfg.json") }
func (z *sonic) scriptFile() string { return filepath.Join(z.c.ztpDir, "provisioning-script") }

// adminMode is ztp_cfg.json's admin-mode (E.2, default true; false when the twin's factory.json
// turns ZTP off and nothing has set it since).
func (z *sonic) adminMode() bool {
	var cfg map[string]any
	if b, err := os.ReadFile(z.cfgFile()); err == nil && json.Unmarshal(b, &cfg) == nil {
		if v, ok := cfg["admin-mode"].(bool); ok {
			return v
		}
	}
	return z.c.factoryZTP()
}

func (z *sonic) setAdminMode(on bool) error {
	cfg := map[string]any{}
	if b, err := os.ReadFile(z.cfgFile()); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	cfg["admin-mode"] = on
	return writeJSON(z.cfgFile(), cfg)
}

// headers are sonic-ztp's (E.3), from the switch's identity as its EEPROM would carry it.
func (z *sonic) headers() http.Header {
	h := http.Header{}
	set := func(k, v string) { h[k] = []string{v} }
	set("User-Agent", sonicAgent)
	set("PRODUCT-NAME", z.id.Model)
	set("SERIAL-NUMBER", z.id.Serial)
	set("BASE-MAC-ADDRESS", strings.ToUpper(z.id.BaseMAC))
	set("SONiC-VERSION", z.id.Version)
	return h
}

func (z *sonic) fetcher() *fetcher { return z.c.newFetcher(z.headers(), false) }

// manualConfig is why ZTP is skipped for a configuration the user made (precedence 0 above), or "".
func (z *sonic) manualConfig() string {
	if exists(filepath.Join(z.c.sonicDir, "ztp-skip")) {
		return filepath.Join(z.c.sonicDir, "ztp-skip") + " exists"
	}
	if exists(filepath.Join(z.c.sonicDir, "minigraph.xml")) {
		return filepath.Join(z.c.sonicDir, "minigraph.xml") + " exists"
	}
	var db struct {
		Meta map[string]struct {
			Hostname string `json:"hostname"`
		} `json:"DEVICE_METADATA"`
	}
	p := filepath.Join(z.c.sonicDir, "config_db.json")
	if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &db) == nil {
		if h := db.Meta["localhost"].Hostname; h != "" && h != "sonic" {
			return fmt.Sprintf("%s has hostname %q, a configuration the user made", p, h)
		}
	}
	return ""
}

// doc is a ZTP JSON document, kept as parsed so keys the twin does not know survive its saves.
type doc map[string]any

func (d doc) top() map[string]any {
	m, _ := d["ztp"].(map[string]any)
	return m
}

func parseDoc(b []byte) (doc, error) {
	var d doc
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("not a ZTP JSON document: %w", err)
	}
	if d.top() == nil {
		return nil, errors.New(`not a ZTP JSON document: no top-level "ztp" object`)
	}
	return d, nil
}

// follow is the document a ZTP JSON's top-level dynamic-url, else url, points to, when it has
// one (ZTPJson's constructor): the document downloaded there replaces it.
func (z *sonic) follow(ctx context.Context, d doc) (doc, error) {
	spec, ok, err := z.source(ctx, d.top())
	if err != nil || !ok {
		return d, err
	}
	b, err := z.download(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("the ztp section's url %s: %w", spec.source, err)
	}
	z.log.printf("ZTP JSON's url: the document is %s", spec.source)
	return parseDoc(b)
}

func (z *sonic) loadDoc() (doc, error) {
	b, err := os.ReadFile(z.dataFile())
	if err != nil {
		return nil, err
	}
	return parseDoc(b)
}

func (z *sonic) saveDoc(d doc) {
	if err := writeJSON(z.dataFile(), d); err != nil {
		z.log.printf("cannot save %s: %v", z.dataFile(), err)
	}
}

// sections are the document's section names in the order they run (lexical, E.5).
func sections(top map[string]any) []string {
	var names []string
	for k, v := range top {
		if _, ok := v.(map[string]any); ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return names
}

// boolOf is a field read as sonic-ztp's getField(..., bool, def) reads it (ZTPLib.getValue): true
// or false, or a string that is "true" or "false" in any case; anything else is def.
func boolOf(m map[string]any, k string, def bool) bool {
	switch v := m[k].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(v) {
		case "true":
			return true
		case "false":
			return false
		}
	}
	return def
}

func strOf(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// intOf is a field read as getField(..., int, def) reads it: Python's int() of a number, a
// boolean or a string of digits; anything else is def.
func intOf(m map[string]any, k string, def int) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case bool:
		if v {
			return 1
		}
		return 0
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// statusOf is a section's or document's status, coerced as sonic-ztp does (E.5).
func statusOf(m map[string]any) string {
	switch s := strOf(m, "status"); s {
	case "":
		return stBoot
	case stBoot, stInProgress, stSuccess, stFailed, stSuspend, stDisabled:
		return s
	}
	return stDisabled
}

func (z *sonic) stamp() string { return z.c.now().UTC().Format(tsFormat) }

// daemon is ZTP's service at boot.
func (z *sonic) daemon(ctx context.Context) int {
	if !z.adminMode() {
		z.set(stateDisabled, "ZTP admin mode is False (ztp enable turns it on)")
		return 0
	}
	release, held, err := lock(z.c.lockFile)
	if err != nil || held {
		z.log.printf("another ZTP run holds %s (%v)", z.c.lockFile, err)
		return 1
	}
	defer release()
	if d, err := z.loadDoc(); err == nil {
		switch s := statusOf(d.top()); s {
		case stSuccess:
			z.set(stateSuccess, "ZTP completed before: "+z.dataFile()+" is SUCCESS")
			return 0
		case stFailed:
			z.set(stateFailed, "ZTP completed before: "+z.dataFile()+" is FAILED")
			return 1
		case stDisabled:
			z.set(stateDisabled, z.dataFile()+" is DISABLED")
			return 0
		default:
			z.log.printf("resuming %s (%s)", z.dataFile(), s)
			return z.engine(ctx, d, z.st.Source, z.st.URL)
		}
	}
	if why := z.manualConfig(); why != "" {
		z.set(stateDisabled, "MANUAL_CONFIG: "+why+": ZTP is skipped")
		return 0
	}
	return z.discoverAndRun(ctx)
}

// discoverAndRun looks for a document every interval until one is found, and runs it.
func (z *sonic) discoverAndRun(ctx context.Context) int {
	z.set(stateDiscovering, "ZTP Service is in Active Discovery")
	for {
		if ctx.Err() != nil {
			return 0
		}
		if !z.adminMode() {
			z.set(stateDisabled, "ZTP admin mode is False (ztp disable)")
			return 0
		}
		if d, src, u := z.discover(ctx); d != nil {
			return z.engine(ctx, d, src, u)
		}
		if sleep(ctx, z.c.interval) != nil {
			return 0
		}
	}
}

// discover is one look at the sources, in precedence order; a source that is present but
// cannot be fetched or parsed is logged and discovery goes on (restart-ztp-on-invalid-data).
func (z *sonic) discover(ctx context.Context) (doc, string, string) {
	local := filepath.Join(z.c.ztpDir, "ztp_data_local.json")
	if b, err := os.ReadFile(local); err == nil {
		d, err := parseDoc(b)
		if err == nil {
			d, err = z.follow(ctx, d)
		}
		if err == nil {
			z.log.printf("ZTP JSON found at %s (local-fs)", local)
			return d, "local", local
		}
		z.log.printf("%s: %v", local, err)
	}
	l := z.c.lease()
	if u := l.option(opt67); u != "" {
		if !strings.Contains(u, "://") && !strings.HasPrefix(u, "/") {
			if tftp := l.option(opt66); tftp != "" {
				u = "tftp://" + tftp + "/" + strings.TrimPrefix(u, "/")
			}
		}
		b, err := z.fetcher().get(ctx, u)
		if err == nil {
			d, perr := parseDoc(b)
			if perr == nil {
				d, perr = z.follow(ctx, d)
			}
			if perr == nil {
				z.log.printf("ZTP JSON downloaded from %s (dhcp-opt67)", u)
				return d, "dhcp-67", u
			}
			err = perr
		}
		z.st.Source, z.st.URL = "dhcp-67", u
		z.set(stateDiscovering, fmt.Sprintf("DHCP option 67 %s: %v", u, err))
		return nil, "", ""
	}
	if u := l.option(opt239); u != "" {
		b, err := z.fetcher().get(ctx, u)
		if err == nil {
			err = writeFileAtomic(z.scriptFile(), b, 0o755)
		}
		if err != nil {
			z.st.Source, z.st.URL = "dhcp-239", u
			z.set(stateDiscovering, fmt.Sprintf("DHCP option 239 %s: %v", u, err))
			return nil, "", ""
		}
		z.log.printf("provisioning script downloaded from %s (dhcp-opt239)", u)
		// sonic-ztp's __createProvScriptJson (E.1)
		return doc{"ztp": map[string]any{
			"provisioning-script": map[string]any{
				"plugin": map[string]any{"url": "file://" + z.scriptFile(), "ignore-section-data": true},
			},
			"restart-ztp-no-config": false,
		}}, "dhcp-239", u
	}
	return nil, "", ""
}

// engine runs a document's sections and records its outcome.
func (z *sonic) engine(ctx context.Context, d doc, source, u string) int {
	top := d.top()
	if _, ok := top["ztp-json-version"]; !ok {
		top["ztp-json-version"] = "1.0"
	}
	names := sections(top)
	for _, n := range names {
		sec := top[n].(map[string]any)
		for _, k := range []string{"ignore-result", "halt-on-failure", "reboot-on-success", "reboot-on-failure"} {
			if _, ok := sec[k]; !ok {
				sec[k] = false
			}
		}
		sec["status"] = statusOf(sec)
	}
	if statusOf(top) == stBoot || strOf(top, "start-timestamp") == "" {
		top["start-timestamp"] = z.stamp()
	}
	top["status"] = stInProgress
	top["timestamp"] = z.stamp()
	z.saveDoc(d)
	z.st.Attempts++
	z.st.Source, z.st.URL = source, u
	z.set(stateRunning, fmt.Sprintf("running %d sections from %s", len(names), u))

	var pending []string
	for _, n := range names {
		if s := statusOf(top[n].(map[string]any)); s != stSuccess && s != stDisabled {
			pending = append(pending, n)
		}
	}
	halted := false
	for pass := 0; pass < 3 && len(pending) > 0 && !halted; pass++ {
		if pass > 0 {
			if sleep(ctx, z.c.interval) != nil {
				return 1
			}
		}
		var suspended []string
		for _, n := range pending {
			if ctx.Err() != nil {
				return 1
			}
			sec := top[n].(map[string]any)
			s := z.runSection(ctx, d, n, sec)
			if s == stSuspend {
				suspended = append(suspended, n)
				continue
			}
			if s == stSuccess && boolOf(sec, "reboot-on-success", false) {
				z.log.printf("%s: reboot-on-success: the twin cannot reboot the switch's container: continuing", n)
			}
			if s == stFailed && boolOf(sec, "reboot-on-failure", false) {
				z.log.printf("%s: reboot-on-failure: the twin cannot reboot the switch's container: continuing", n)
			}
			if s == stFailed && boolOf(sec, "halt-on-failure", false) {
				z.log.printf("%s: FAILED with halt-on-failure: the remaining sections are not run", n)
				halted = true
				break
			}
		}
		pending = suspended
	}
	for _, n := range pending {
		if sec := top[n].(map[string]any); !halted && statusOf(sec) == stSuspend {
			sec["status"] = stFailed
			sec["timestamp"] = z.stamp()
			z.log.printf("%s: still SUSPEND after three passes: FAILED", n)
		}
	}
	result := stSuccess
	var failed []string
	for _, n := range names {
		if statusOf(top[n].(map[string]any)) != stSuccess && statusOf(top[n].(map[string]any)) != stDisabled {
			failed = append(failed, n)
		}
	}
	if len(failed) > 0 && !boolOf(top, "ignore-result", false) {
		result = stFailed
	}
	top["status"] = result
	top["timestamp"] = z.stamp()
	z.saveDoc(d)
	if result == stSuccess && boolOf(top, "reboot-on-success", false) || result == stFailed && boolOf(top, "reboot-on-failure", false) {
		z.log.printf("ZTP %s: reboot requested: the twin cannot reboot the switch's container: continuing", result)
	}
	if result == stSuccess {
		z.set(stateSuccess, fmt.Sprintf("ZTP SUCCESS: %d sections", len(names)))
		return 0
	}
	z.set(stateFailed, "ZTP FAILED: "+strings.Join(failed, ", ")+" not SUCCESS")
	return 1
}

// runSection runs one section's plugin and records its status.
func (z *sonic) runSection(ctx context.Context, d doc, name string, sec map[string]any) string {
	start := z.stamp()
	sec["status"] = stInProgress
	sec["start-timestamp"] = start
	sec["timestamp"] = start
	z.saveDoc(d)
	z.log.printf("%s: processing", name)
	code, err := z.plugin(ctx, name, sec)
	if err != nil {
		z.log.printf("%s: %v", name, err)
		if code == 0 {
			code = 1
		}
	}
	z.st.ExitCode = code
	status := stFailed
	switch se := intOf(sec, "suspend-exit-code", 0); {
	case code == 0:
		status = stSuccess
	case se > 0 && code == se:
		status = stSuspend
	}
	if status == stFailed && boolOf(sec, "ignore-result", false) {
		z.log.printf("%s: FAILED (exit code %d) with ignore-result: marked SUCCESS", name, code)
		status = stSuccess
	}
	sec["exit-code"] = code
	sec["status"] = status
	sec["timestamp"] = z.stamp()
	z.saveDoc(d)
	z.log.printf("%s: %s (exit code %d)", name, status, code)
	z.save()
	return status
}

// sonicCLI is sonic-ztp's /usr/bin/ztp (E.7): status [-c] [-v], enable, disable [-y], run [-y].
func (c *config) sonicCLI(ctx context.Context, args []string) int {
	cmd := args[0]
	var code, verbose, yes bool
	for _, a := range args[1:] {
		switch a {
		case "-c", "--status-code":
			code = true
		case "-v", "--verbose":
			verbose = true
		case "-y", "--yes":
			yes = true
		default:
			fmt.Fprintf(c.stderr, "ztp: unknown option %q\n%s\n", a, usage)
			return 2
		}
	}
	z := newSonic(c, verbose && cmd == "run")
	switch cmd {
	case "status":
		if code {
			fmt.Fprintln(c.stdout, z.statusCode())
		} else {
			z.printStatus(verbose)
		}
		return 0
	case "enable":
		if err := z.setAdminMode(true); err != nil {
			fmt.Fprintf(c.stderr, "ztp: %v\n", err)
			return 1
		}
		return 0
	case "disable":
		if !yes && !c.confirm("Active ZTP session will be stopped and disabled, continue?") {
			return 0
		}
		if err := z.setAdminMode(false); err != nil {
			fmt.Fprintf(c.stderr, "ztp: %v\n", err)
			return 1
		}
		if release, held, _ := lock(c.lockFile); !held && release != nil {
			z.set(stateDisabled, "ZTP admin mode is False (ztp disable)")
			release()
		}
		return 0
	case "run":
		if !z.adminMode() {
			fmt.Fprintln(c.stderr, "ZTP is disabled. Use 'ztp enable' first.")
			return 1
		}
		if !yes && !c.confirm("ZTP will be restarted. You may lose switch data and connectivity, continue?") {
			return 0
		}
		return z.restart(ctx)
	}
	fmt.Fprintf(c.stderr, "ztp: unknown command %q\n%s\n", cmd, usage)
	return 2
}

// restart is `ztp run` ("Restart ZTP"): the previous document is forgotten and ZTP discovers
// and runs again in the foreground. An operator's explicit run is not skipped for a
// configuration found on the switch (precedence 0), which is the boot run's test.
func (z *sonic) restart(ctx context.Context) int {
	release, held, err := lock(z.c.lockFile)
	if err != nil || held {
		fmt.Fprintln(z.c.stderr, "ztp: ZTP is already running")
		return 1
	}
	defer release()
	_ = os.Remove(z.dataFile())
	z.st = twinStatus{Flavour: "sonic", ExitCode: -1}
	return z.discoverAndRun(ctx)
}

func (c *config) confirm(prompt string) bool {
	fmt.Fprintf(c.stdout, "%s [y/N]: ", prompt)
	line, _ := bufio.NewReader(c.stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// service is `ZTP Service` (E.7): Inactive unless a run holds ZTP's lock.
func (z *sonic) service() string {
	release, held, _ := lock(z.c.lockFile)
	if release != nil {
		release()
	}
	if !held {
		return "Inactive"
	}
	if z.c.readStatus().State == stateDiscovering {
		return "Active Discovery"
	}
	return "Processing"
}

// statusCode is `ztp status -c` (E.7).
func (z *sonic) statusCode() string {
	if !z.adminMode() {
		return "0:DISABLED"
	}
	switch z.service() {
	case "Active Discovery":
		return "2:ACTIVE-DISCOVERY"
	case "Processing":
		return "4:IN-PROGRESS"
	}
	if d, err := z.loadDoc(); err == nil {
		switch statusOf(d.top()) {
		case stSuccess:
			return "5:SUCCESS"
		case stFailed:
			return "6:FAILED"
		case stBoot:
			return "3:NOT-STARTED"
		}
		return "1:INACTIVE"
	}
	if z.c.readStatus().State == stateDisabled {
		return "1:INACTIVE"
	}
	return "3:NOT-STARTED"
}

func display(status string) string {
	if status == stBoot {
		return "Not Started"
	}
	return status
}

// sourceName is sonic-ztp's name for where the document came from (E.1).
func sourceName(src string) string {
	switch src {
	case "local":
		return "local-fs"
	case "dhcp-67":
		return "dhcp-opt67 (" + nos.MgmtIf + ")"
	case "dhcp-239":
		return "dhcp-opt239 (" + nos.MgmtIf + ")"
	}
	return src
}

// runtime is sonic-ztp's timeDiff: "05m 31s", with hours and days when there are any.
func runtime(start, end string, now time.Time) string {
	s, err := time.Parse(tsFormat, start)
	if err != nil {
		return ""
	}
	e, err := time.Parse(tsFormat, end)
	if err != nil {
		e = now
	}
	d := e.Sub(s)
	if d < 0 {
		d = 0
	}
	secs := int(d.Seconds())
	days, h, m, sec := secs/86400, secs/3600%24, secs/60%60, secs%60
	out := fmt.Sprintf("%02dm %02ds", m, sec)
	if h > 0 || days > 0 {
		out = fmt.Sprintf("%02dh ", h) + out
	}
	if days > 0 {
		out = fmt.Sprintf("%dd ", days) + out
	}
	return out
}

var leadingNum = regexp.MustCompile(`^[0-9]+-`)

// printStatus is `ztp status` (E.7): the terse form from SONiC's design, and with -v the
// verbose one (its banner and per-section fields as E.7 lists them; their layout is the twin's).
func (z *sonic) printStatus(verbose bool) {
	w := z.c.stdout
	pr := func(k, v string) {
		if verbose {
			fmt.Fprintf(w, "%-16s : %s\n", k, v)
		} else {
			fmt.Fprintf(w, "%-14s : %s\n", k, v)
		}
	}
	title := func(s, rule string) {
		fmt.Fprintln(w, strings.Repeat(rule, 40))
		fmt.Fprintln(w, s)
		fmt.Fprintln(w, strings.Repeat(rule, 40))
	}
	if verbose {
		title("ZTP", "=")
	}
	admin := "False"
	if z.adminMode() {
		admin = "True"
	}
	service := z.service()
	d, derr := z.loadDoc()
	status := "Not Started"
	if derr == nil {
		status = display(statusOf(d.top()))
	}
	pr("ZTP Admin Mode", admin)
	pr("ZTP Service", service)
	pr("ZTP Status", status)
	st := z.c.readStatus()
	if derr == nil {
		top := d.top()
		if st.Source != "" {
			pr("ZTP Source", sourceName(st.Source))
		}
		end := strOf(top, "timestamp")
		if statusOf(top) == stInProgress {
			end = ""
		}
		if rt := runtime(strOf(top, "start-timestamp"), end, z.c.now().UTC()); rt != "" {
			pr("Runtime", rt)
		}
		if ts := strOf(top, "timestamp"); ts != "" {
			pr("Timestamp", ts)
		}
		if verbose {
			if st.State == stateFailed && st.Message != "" {
				pr("Error", st.Message)
			}
			pr("Ignore Result", pyBool(boolOf(top, "ignore-result", false)))
			pr("ZTP JSON Version", fmt.Sprint(top["ztp-json-version"]))
		}
	} else if st.State == stateDisabled && st.Message != "" && verbose {
		pr("Error", st.Message)
	}
	fmt.Fprintln(w)
	if service == "Inactive" {
		fmt.Fprintln(w, "ZTP Service is not running")
	}
	if derr != nil {
		return
	}
	top := d.top()
	if !verbose {
		fmt.Fprintln(w)
		for _, n := range sections(top) {
			fmt.Fprintf(w, "%s: %s\n", n, display(statusOf(top[n].(map[string]any))))
		}
		return
	}
	for _, n := range sections(top) {
		sec := top[n].(map[string]any)
		fmt.Fprintln(w)
		title(n, "-")
		if desc := strOf(sec, "description"); desc != "" {
			pr("Description", desc)
		}
		pr("Status", display(statusOf(sec)))
		end := strOf(sec, "timestamp")
		if statusOf(sec) == stInProgress {
			end = ""
		}
		if rt := runtime(strOf(sec, "start-timestamp"), end, z.c.now().UTC()); rt != "" {
			pr("Runtime", rt)
		}
		if ts := strOf(sec, "timestamp"); ts != "" {
			pr("Timestamp", ts)
		}
		if ec, ok := sec["exit-code"]; ok {
			pr("Exit Code", fmt.Sprint(ec))
		}
		pr("Ignore Result", pyBool(boolOf(sec, "ignore-result", false)))
		pr("Halt on Failure", pyBool(boolOf(sec, "halt-on-failure", false)))
	}
}

func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

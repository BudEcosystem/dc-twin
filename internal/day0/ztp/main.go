// Package ztp is the twin's ZTP client for a factory switch (18, "ZTP"; D74): where a NOS image
// does not carry its own ZTP client, this one runs in the switch's container and implements the
// NOS's documented contract, and the fidelity report labels it. It has two flavours, one per NOS
// the twin starts in factory state (nos.Entry.FactoryCapable):
//
//   - cumulus, on generic/nvue-frr (standing in for Cumulus Linux 5.x): a provisioning script
//     found in /var/lib/cumulus/ztp, or at the URL of DHCP option 239, fetched with Cumulus's
//     identifying headers, refused unless it carries the CUMULUS-AUTOPROVISIONING marker, and run
//     as root; exit 0 is success and ZTP disables itself. The contract is section A of
//     docs/reviews/2026-11-11-day0-vendor-facts.md (NVIDIA's Cumulus Linux 5.0 and 5.14 ZTP
//     pages), and the CLI is Cumulus's `ztp -s/-e/-d/-r/-R/-v`.
//   - sonic, on sonic-vs: a ZTP JSON document (local, or at the URL of DHCP option 67) whose
//     sections run in lexical order of their names, each by its plugin (configdb-json,
//     provisioning-script, connectivity-check, snmp, download; firmware recognised and not run,
//     graphservice refused by name), or the provisioning script of option 239. The contract is
//     section E of the same review (github.com/sonic-net/sonic-ztp at 036c8eaad345 and SONiC's
//     doc/ztp/ztp.md), and the CLI is sonic-ztp's `ztp status/enable/disable/run`.
//
// The client does no DHCP itself (D73): the NOS's own dhclient leases eth0, in VRF mgmt on
// Cumulus, and an exit hook the twin installs in the image writes the lease's variables to
// /run/dctwin/dhcp-eth0.env as dhclient's environment (lease.go). Fetches leave through VRF mgmt
// when the switch has one (fetch.go), as `ip vrf exec mgmt curl` would.
//
// Whatever the flavour, the client keeps the twin-facing status file
// /var/lib/dctwin/ztp/status.json (status.go), whose "state" the twin's observer reads
// (internal/adapter/netsim/tap's day-0 observation) to place the switch in its day-0 phase.
//
// The program runs as `dctwin-day0 ztp <args>`, or as `ztp <args>` through a link of that name
// (cmd/dctwin-day0). The twin starts `ztp daemon --nos cumulus|sonic` detached once the switch
// has booted with factory.ztp enabled (18, "Factory state").
package ztp

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// config is every path and knob the client uses, so tests can run it in a temporary directory
// without root. defaultConfig is the switch's.
type config struct {
	// The twin's inputs (18, "The estate"; internal/nos/factory.go): the switch's identity and
	// its factory settings, both written by the twin under nos.Day0Dir.
	identityFile, factoryFile string
	// dhcpEnv is the lease of eth0 as the twin's dhclient exit hook writes it (lease.go).
	dhcpEnv string
	// faultsDir is where the twin puts the day-0 faults it carries out in the switch
	// (packs/generic/faults/day0.yaml): dhcp-strip-ztp, the lease seen without its ZTP options;
	// ztp-corrupt, what is fetched cut short.
	faultsDir string
	// userApplied is the marker the twin touches when an NVUE revision is applied by a client
	// other than the twin itself: Cumulus's "manual configuration changes" (A.7).
	userApplied string
	// statusFile is the twin-facing status (status.go); daemonFile the daemon's command
	// options, kept so a later `ztp run` or `ztp -r` applies a configuration as the daemon would.
	statusFile, daemonFile string
	// lockFile is ZTP's lock (A.6: /var/run/ztp.lock), held by whichever run is in progress.
	lockFile string
	// tmpDir is where fetched scripts are written before they run.
	tmpDir string

	// vrf is the device fetches bind to when vrfExists says the switch has it (mgmt).
	vrf       string
	vrfExists func(string) bool
	// scriptVRF, when set, runs scripts and pings through `ip vrf exec <scriptVRF>`.
	scriptVRF string
	ipCmd     string
	// path is the PATH scripts and commands run with: the day-0 programs first, so a script's
	// `nv` is the twin's (D76).
	path string

	// interval is how often discovery looks again; window how long Cumulus's DHCP discovery
	// lasts (A.1: every 10 s for up to 5 minutes). provMax is CUMULUS-PROV-MAX.
	interval, window time.Duration
	provMax          int
	fetchTimeout     time.Duration

	// Cumulus (A.6): the ZTP directory (local scripts, state and ztp_state.log) and the copy of
	// the log the docs' sample script writes, /var/log/autoprovision.
	clDir, autoprovLog string

	// SONiC (E.2): /host/ztp (ztp_cfg.json, ztp_data.json, ztp_data_local.json, the
	// provisioning script), ZTP's log, its per-section directories and /etc/sonic.
	ztpDir, ztpLog, sectionsDir, sonicDir string
	// The commands the configdb-json and snmp plugins apply a configuration with (sonic.go).
	reloadCmd, loadCmd, saveCmd, cfgdbSet, pingCmd string
	// dumpMetaCmd prints the switch's DEVICE_METADATA table as JSON, for configdb-json's
	// overwrite-device-metadata false.
	dumpMetaCmd string

	hostname func() (string, error)
	now      func() time.Time
	stdin    io.Reader
	stdout   io.Writer
	stderr   io.Writer
	// quiet keeps log lines off stderr (tests, and the CLI without -v).
	quiet bool
}

func defaultConfig() *config {
	return &config{
		identityFile: filepath.Join(nos.Day0Dir, "identity.json"),
		factoryFile:  filepath.Join(nos.Day0Dir, "factory.json"),
		dhcpEnv:      "/run/dctwin/dhcp-" + nos.MgmtIf + ".env",
		faultsDir:    filepath.Join(nos.Day0Dir, "faults"),
		userApplied:  "/run/dctwin/nvue-user-applied",
		statusFile:   filepath.Join(nos.Day0State, "ztp", "status.json"),
		daemonFile:   filepath.Join(nos.Day0State, "ztp", "daemon.json"),
		lockFile:     "/var/run/ztp.lock",
		tmpDir:       os.TempDir(),
		vrf:          "mgmt",
		vrfExists:    linkExists,
		ipCmd:        "ip",
		path:         nos.Day0Mount + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		interval:     10 * time.Second,
		window:       5 * time.Minute,
		provMax:      32,
		fetchTimeout: 30 * time.Second,
		clDir:        "/var/lib/cumulus/ztp",
		autoprovLog:  "/var/log/autoprovision",
		ztpDir:       "/host/ztp",
		ztpLog:       "/var/log/ztp.log",
		sectionsDir:  "/var/lib/ztp/sections",
		sonicDir:     "/etc/sonic",
		reloadCmd:    "config reload -y",
		loadCmd:      "config load -y",
		saveCmd:      "config save -y",
		pingCmd:      "ping",
		dumpMetaCmd:  "sonic-cfggen -d --var-json DEVICE_METADATA",
		hostname:     os.Hostname,
		now:          time.Now,
		stdin:        os.Stdin,
		stdout:       os.Stdout,
		stderr:       os.Stderr,
	}
}

// linkExists reports whether the switch has a network device of that name (a VRF is one).
func linkExists(name string) bool {
	_, err := net.InterfaceByName(name)
	return err == nil
}

// Main is the program's entry point (cmd/dctwin-day0).
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, defaultConfig(), args)
}

// run dispatches on the shape of its arguments: `daemon` is the twin's; a leading flag is
// Cumulus's CLI (`ztp -s`); a leading word is sonic-ztp's (`ztp status`).
func run(ctx context.Context, c *config, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(c.stderr, usage)
		return 2
	}
	switch a := args[0]; {
	case a == "daemon":
		return c.daemon(ctx, args[1:])
	case a == "-b" || a == "--boot":
		// Cumulus's systemd unit runs `ztp -b` at boot (A.5, its meaning inferred): ZTP's boot run.
		return c.daemon(ctx, append([]string{"--nos", "cumulus"}, args[1:]...))
	case a == "-h" || a == "--help" || a == "help":
		fmt.Fprintln(c.stdout, usage)
		return 0
	case strings.HasPrefix(a, "-"):
		return c.loadDaemonOptions().cumulusCLI(ctx, args)
	default:
		return c.loadDaemonOptions().sonicCLI(ctx, args)
	}
}

const usage = `usage:
  ztp daemon --nos cumulus|sonic [options]   the twin's ZTP run at boot
  ztp -s | -e | -d | -R | -r [URL|path] [-v] Cumulus Linux's ZTP CLI
  ztp status [-c] [-v] | enable | disable [-y] | run [-y]   SONiC's ZTP CLI`

// daemonOptions are the daemon's command options that change how a configuration is applied,
// saved by the daemon in daemonFile for the CLI's own runs.
type daemonOptions struct {
	Flavour   string `json:"flavour"`
	ReloadCmd string `json:"reloadCmd,omitempty"`
	LoadCmd   string `json:"loadCmd,omitempty"`
	SaveCmd   string `json:"saveCmd,omitempty"`
	CfgdbSet  string `json:"cfgdbSet,omitempty"`
	DumpMeta  string `json:"dumpMetadataCmd,omitempty"`
	ScriptVRF string `json:"scriptVRF,omitempty"`
	VRF       string `json:"vrf,omitempty"`
}

func (c *config) loadDaemonOptions() *config {
	var o daemonOptions
	if b, err := os.ReadFile(c.daemonFile); err == nil && json.Unmarshal(b, &o) == nil {
		set := func(dst *string, v string) {
			if v != "" {
				*dst = v
			}
		}
		set(&c.reloadCmd, o.ReloadCmd)
		set(&c.loadCmd, o.LoadCmd)
		set(&c.saveCmd, o.SaveCmd)
		set(&c.cfgdbSet, o.CfgdbSet)
		set(&c.dumpMetaCmd, o.DumpMeta)
		set(&c.scriptVRF, o.ScriptVRF)
		set(&c.vrf, o.VRF)
	}
	return c
}

// daemon is `ztp daemon`: ZTP's run at boot, as the NOS's own service starts it. Its options:
//
//	--nos cumulus|sonic   the flavour (required)
//	--reload-cmd CMD      SONiC: how configdb-json applies a config_db.json with clear-config
//	                      true (default "config reload -y"; the file is appended, or replaces {file})
//	--load-cmd CMD        SONiC: the same with clear-config false (default "config load -y")
//	--save-cmd CMD        SONiC: save-config true (default "config save -y")
//	--cfgdb-set CMD       SONiC: the snmp plugin's CONFIG_DB write, run as CMD <key> <field> <value>
//	                      (e.g. "redis-cli -n 4 hset"); empty writes only /etc/sonic/snmp.yml
//	--dump-metadata-cmd CMD  SONiC: prints the switch's DEVICE_METADATA as JSON, kept over a
//	                      downloaded config_db.json's unless overwrite-device-metadata
//	                      (default "sonic-cfggen -d --var-json DEVICE_METADATA")
//	--vrf NAME            the device fetches bind to when it exists (default mgmt)
//	--script-vrf NAME     run scripts and pings through `ip vrf exec NAME` (default: as started)
//	--interval D, --window D, --prov-max N   discovery's period, Cumulus's DHCP window and
//	                      CUMULUS-PROV-MAX (defaults 10s, 5m, 32; the twin scales them)
func (c *config) daemon(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("ztp daemon", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	flavour := fs.String("nos", "", "cumulus or sonic")
	fs.StringVar(&c.reloadCmd, "reload-cmd", c.reloadCmd, "SONiC configdb-json reload command")
	fs.StringVar(&c.loadCmd, "load-cmd", c.loadCmd, "SONiC configdb-json load command (clear-config false)")
	fs.StringVar(&c.saveCmd, "save-cmd", c.saveCmd, "SONiC configdb-json save command")
	fs.StringVar(&c.cfgdbSet, "cfgdb-set", c.cfgdbSet, "SONiC CONFIG_DB hset command for the snmp plugin")
	fs.StringVar(&c.dumpMetaCmd, "dump-metadata-cmd", c.dumpMetaCmd, "SONiC command printing DEVICE_METADATA as JSON")
	fs.StringVar(&c.vrf, "vrf", c.vrf, "device fetches bind to when present")
	fs.StringVar(&c.scriptVRF, "script-vrf", c.scriptVRF, "VRF scripts run in through ip vrf exec")
	fs.DurationVar(&c.interval, "interval", c.interval, "discovery interval")
	fs.DurationVar(&c.window, "window", c.window, "Cumulus DHCP discovery window")
	fs.IntVar(&c.provMax, "prov-max", c.provMax, "CUMULUS-PROV-MAX")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *flavour != "cumulus" && *flavour != "sonic" {
		fmt.Fprintln(c.stderr, "ztp daemon: --nos must be cumulus or sonic")
		return 2
	}
	_ = writeJSON(c.daemonFile, daemonOptions{Flavour: *flavour, ReloadCmd: c.reloadCmd, LoadCmd: c.loadCmd,
		SaveCmd: c.saveCmd, CfgdbSet: c.cfgdbSet, DumpMeta: c.dumpMetaCmd, ScriptVRF: c.scriptVRF, VRF: c.vrf})
	if *flavour == "cumulus" {
		return newCumulus(c, !c.quiet).daemon(ctx)
	}
	return newSonic(c, !c.quiet).daemon(ctx)
}

// writeJSON writes v to path atomically (a temporary file renamed over it), creating its
// directory.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'), 0o644)
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := os.Chmod(f.Name(), mode); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}

// identity is the switch's identity as the twin wrote it (nos.Identity); a missing or
// unreadable file leaves it empty, and the headers that carry it go out empty, logged.
func (c *config) identity() (nos.Identity, error) {
	var id nos.Identity
	b, err := os.ReadFile(c.identityFile)
	if err != nil {
		return id, err
	}
	return id, json.Unmarshal(b, &id)
}

// factoryZTP is factory.json's "ztp": whether the NOS's ZTP is armed at first boot (default
// true when the file is absent, since the twin starts the daemon only when it is).
func (c *config) factoryZTP() bool {
	var f struct {
		ZTP *bool `json:"ztp"`
	}
	if b, err := os.ReadFile(c.factoryFile); err == nil && json.Unmarshal(b, &f) == nil && f.ZTP != nil {
		return *f.ZTP
	}
	return true
}

// sleep waits d, or until ctx ends.
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

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// fault reports whether the twin has a day-0 fault on for the switch (a file in faultsDir).
func (c *config) fault(name string) bool {
	if c.faultsDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(c.faultsDir, name))
	return err == nil
}

// lease is eth0's lease as ZTP sees it: under the twin's ztp-dhcp-no-option fault, without the
// ZTP options (239, 67), as a scope that serves none.
func (c *config) lease() lease {
	l := readLease(c.dhcpEnv)
	if l != nil && c.fault("dhcp-strip-ztp") {
		for _, names := range [][]string{opt239, opt67} {
			for _, n := range names {
				delete(l, n)
			}
		}
	}
	return l
}

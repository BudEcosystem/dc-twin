// Package ibsim is the InfiniBand subnet-management substrate (docs/spec/05-interfaces.md —
// Fabric managers, "InfiniBand: subnet manager"): for every InfiniBand fabric of the model it
// renders the fabric as a topology in ibnetdiscover's output format and runs ibsim
// (github.com/linux-rdma/ibsim) on it, so that a stack's own subnet manager and diagnostics,
// OpenSM, ibnetdiscover, ibstat, iblinkinfo, smpquery, perfquery, unmodified, manage and read
// the twin's fabric through ibsim's LD_PRELOAD library (libumad2sim) in place of /dev/infiniband.
// It fills the vendor-neutral "Scale-out fabric: OpenSM" slot (06 — The same slots).
//
// The topology is a pure function of the model's structure (Render): every switch with the
// system GUID the ufm adapter reports, every host channel adapter port with the model's port
// GUID, every cable with its port numbers. While the twin runs, the operational state of every
// cable (engine.PortUp of both ends: a pulled cable, a port fault, a powered-off host or a switch
// without AC) is applied to the running ibsim through its console, `Unlink` and `Link`, so a
// subnet manager sees the fabric change as it would: ibsim's switch sends it trap 128 and it
// sweeps.
//
// What ibsim then holds, port logical state (Initialize, Armed, Active), LIDs, forwarding
// tables and the SM's own state, is the substrate's and the stack's subnet manager's, not the
// model's (D19). ibsim answers subnet management and performance MADs only: there is no data
// plane, as there is none anywhere in the twin's InfiniBand.
package ibsim

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/adapter/ibsim/smstate"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// InfoFile is where the adapter describes each simulated fabric for a stack in another process
// (a harness, a subnet manager's unit): its topology, the ibsim socket name a MAD client must
// use, where the subnet manager attaches and the preload library.
const InfoFile = "ibsim.json"

// Config is what can be configured about the adapter. Binary can also be set from the
// installed stack's outputs: ibsim.binary (a path, or "none" to render topologies only).
type Config struct {
	// Binary is the ibsim to run; empty: `ibsim` on PATH. When none is found the adapter
	// renders each fabric's topology into the state directory and simulates nothing.
	Binary string

	// newConsole replaces running ibsim (tests): it is called once per fabric after the
	// topology is written.
	newConsole func(fabric, netFile string) (console, error)
}

// Info is one simulated fabric, as InfoFile lists it.
type Info struct {
	Fabric   string `json:"fabric"`
	Topology string `json:"topology"` // the rendered ibnetdiscover-format file
	Running  bool   `json:"running"`
	// SockName is the IBSIM_SOCKNAME every MAD client of this ibsim must have in its
	// environment (ibsim and libumad2sim meet on abstract unix sockets named from it).
	SockName string `json:"sockName,omitempty"`
	// SMHost is the SIM_HOST a subnet manager attaches at (the node id of the fabric manager
	// appliance's HCA); SMPort and SMPortGUID are that port in the model.
	SMHost     string `json:"smHost"`
	SMPort     string `json:"smPort"`
	SMPortGUID string `json:"smPortGuid"`
	// Preload is libumad2sim, for LD_PRELOAD, when the adapter found it.
	Preload    string   `json:"preload,omitempty"`
	Binary     string   `json:"binary,omitempty"`
	Version    string   `json:"version,omitempty"`
	PID        int      `json:"pid,omitempty"`
	Transcript string   `json:"transcript,omitempty"` // ibsim's console and the commands applied
	Switches   int      `json:"switches"`
	CAs        int      `json:"channelAdapters"`
	Links      int      `json:"links"`
	Notes      []string `json:"notes,omitempty"`
	Why        string   `json:"why,omitempty"` // why it is not running
}

// Adapter runs ibsim for every InfiniBand fabric.
type Adapter struct {
	cfg     Config
	rt      *adapter.Runtime
	bin     string
	version string
	preload string
	sims    []*sim
	lines   []string
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// sim is one fabric: its topology and, when it runs, ibsim and the link state applied to it.
type sim struct {
	a       *Adapter
	topo    *Topology
	netFile string
	logFile string
	sock    string
	proc    *process
	con     console
	why     string

	mu      sync.Mutex
	applied map[*Link]bool // the link state ibsim holds: true when linked
	changes int
	errs    int
	lastErr string

	dumpMu sync.Mutex // the last Dump, for Ports
	dump   map[string]smstate.Port
	dumped time.Time
}

func init() {
	twin.Register("ibsim", func() adapter.Adapter { return New(Config{}) })
}

// New returns an adapter with a configuration; an empty Binary is taken from the stack's
// outputs at Start, then from PATH.
func New(cfg Config) *Adapter { return &Adapter{cfg: cfg} }

func (a *Adapter) Name() string { return "ibsim" }

// Start renders every InfiniBand fabric, runs ibsim on each when it can, applies the model's
// current link state and follows it.
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt = rt
	if a.cfg.Binary == "" {
		a.cfg.Binary = rt.Outputs["ibsim.binary"]
	}
	var why string
	switch {
	case a.cfg.newConsole != nil:
	case a.cfg.Binary == "none":
		why = "ibsim.binary is none"
	case a.cfg.Binary != "":
		a.bin = a.cfg.Binary
	default:
		if p, err := exec.LookPath("ibsim"); err == nil {
			a.bin = p
		} else {
			why = "ibsim is not on PATH (Debian and Ubuntu: ibsim-utils and libumad2sim0; or github.com/linux-rdma/ibsim)"
		}
	}
	if a.bin != "" {
		a.version = ibsimVersion(a.bin)
		a.preload = findPreload(a.bin)
	}
	var fabrics []string
	var topos []*Topology
	var errs []string
	rt.Engine.Read(func(m *model.Model) {
		fabrics = Fabrics(m)
		for _, f := range fabrics {
			t, err := Render(m, f)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			topos = append(topos, t)
		}
	})
	for _, e := range errs {
		a.lines = append(a.lines, e+": not simulated")
	}
	if len(fabrics) == 0 {
		a.lines = append(a.lines, "no InfiniBand fabric in the estate")
		return nil
	}
	for _, t := range topos {
		s := &sim{a: a, topo: t, applied: map[*Link]bool{}}
		a.sims = append(a.sims, s)
		if rt.StateDir == "" {
			s.why = "no state directory to write the topology to"
			continue
		}
		s.netFile = filepath.Join(rt.StateDir, "ibsim-"+t.Fabric+".net")
		s.logFile = filepath.Join(rt.StateDir, "ibsim-"+t.Fabric+".log")
		if err := os.WriteFile(s.netFile, t.Text(), 0o644); err != nil {
			a.Stop()
			return fmt.Errorf("ibsim: %w", err)
		}
		if why != "" {
			s.why = why
			continue
		}
		if err := s.start(); err != nil {
			// the rest of the twin runs: a fabric ibsim cannot simulate is an incident and a
			// report line, not a refusal to come up
			s.why = err.Error()
			rt.Engine.Incident("ibsim", fmt.Sprintf("fabric %s: %v", t.Fabric, err))
			continue
		}
		s.reconcile() // the model's current state, before the twin reports itself up
	}
	if err := a.writeInfo(); err != nil {
		rt.Log.Warn("ibsim: info file not written", "err", err)
	}
	for _, s := range a.sims {
		if s.proc != nil {
			// UFM REST reports the LIDs the stack's SM assigned in ibsim (D47)
			smstate.Register(rt.Engine, a)
			break
		}
	}
	ctx, a.cancel = context.WithCancel(ctx)
	for _, s := range a.sims {
		if s.con != nil {
			a.wg.Add(1)
			go func() { defer a.wg.Done(); s.follow(ctx) }()
		}
	}
	return nil
}

func (s *sim) start() error {
	if s.a.cfg.newConsole != nil {
		c, err := s.a.cfg.newConsole(s.topo.Fabric, s.netFile)
		if err != nil {
			return err
		}
		s.con = c
	} else {
		s.sock = sockName(s.a.rt.StateDir, s.topo.Fabric)
		n, sw, ports := s.topo.Sizes()
		p, _, err := startProcess(s.a.bin, s.netFile, s.sock, [3]int{n, sw, ports}, s.logFile)
		if err != nil {
			return err
		}
		s.proc, s.con = p, p
	}
	for _, l := range s.topo.Links {
		s.applied[l] = true // the topology file cables every link
	}
	return nil
}

// sockName is the IBSIM_SOCKNAME of one fabric's ibsim: ibsim's sockets are abstract unix
// sockets in the network namespace, so two twins on one host (two state directories) must not
// share one.
func sockName(stateDir, fabric string) string {
	h := sha256.Sum256([]byte(stateDir + "\x00" + fabric))
	return fmt.Sprintf("dctwin-%s-%x", fabric, h[:3])
}

// follow applies the model's link state as it changes. Any committed event other than a
// request may have changed a port (a cable act, a fault, power, AC), so on one it drains what
// has queued behind it and reconciles at once: the engine commits under its write lock, so the
// reconcile reads the state that event left. Reconciling on a timer instead missed changes the
// time scale makes short (the lab's 0.05 makes a switch's AC cycle a 100 ms outage, and a
// 100 ms tick read it half restored). A transition that begins and ends within one engine
// operation (a virtual clock's Advance across both) is not seen. Every five seconds it
// reconciles regardless, for changes made while a dropped subscription was renewed.
func (s *sim) follow(ctx context.Context) {
	e := s.a.rt.Engine
	full := time.NewTicker(5 * time.Second)
	defer full.Stop()
	var exited <-chan struct{}
	if s.proc != nil {
		exited = s.proc.exited
	}
	for ctx.Err() == nil {
		id, ch := e.Subscribe(4096)
		s.reconcile()
	loop:
		for {
			select {
			case <-ctx.Done():
				e.Unsubscribe(id)
				return
			case <-exited:
				// Stop cancels ctx and waits for this loop before it quits ibsim, so this is a
				// crash: the fabric no longer follows the model
				e.Unsubscribe(id)
				e.Incident("ibsim", fmt.Sprintf("fabric %s: ibsim exited (%v); the simulated fabric is gone, see %s", s.topo.Fabric, s.proc.waitErr, s.logFile))
				return
			case ev, ok := <-ch:
				if !ok {
					break loop // dropped for falling behind: resubscribe and reconcile
				}
				dirty := ev.Kind != "request"
				for more := true; more; {
					select {
					case ev, ok := <-ch:
						if !ok {
							break loop
						}
						dirty = dirty || ev.Kind != "request"
					default:
						more = false
					}
				}
				if dirty {
					s.reconcile()
				}
			case <-full.C:
				s.reconcile()
			}
		}
	}
}

// desired is the state every link should have in ibsim: linked when both of its ports are up
// in the model (admin up, cable in, no port fault, the device powered or, for a switch, with AC).
func (s *sim) desired() map[*Link]bool {
	e := s.a.rt.Engine
	out := make(map[*Link]bool, len(s.topo.Links))
	for _, l := range s.topo.Links {
		out[l] = e.PortUp(l.ma) && e.PortUp(l.mb)
	}
	return out
}

// reconcile sends ibsim the commands that make its links the model's, in the topology's link
// order.
func (s *sim) reconcile() {
	want := s.desired()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc != nil && !s.proc.running() {
		return
	}
	for _, l := range s.topo.Links {
		up := want[l]
		if s.applied[l] == up {
			continue
		}
		cmd := unlinkCommand(l)
		if up {
			cmd = linkCommand(l)
		}
		out, err := s.con.Do(cmd)
		if err == nil {
			err = consoleError(out, up)
		}
		if err != nil {
			s.errs++
			if s.lastErr != err.Error() {
				s.a.rt.Engine.Incident("ibsim", fmt.Sprintf("fabric %s: %s: %v", s.topo.Fabric, cmd, err))
			}
			s.lastErr = err.Error()
			continue
		}
		s.applied[l] = up
		s.changes++
	}
}

// unlinkCommand takes a link down at its switch end (ibsim keeps the peer for a ReLink, marks
// both ports Down/Polling and sends the SM trap 128 from each switch end).
func unlinkCommand(l *Link) string {
	return fmt.Sprintf("Unlink %q[%d]", l.A.Node.ID, l.A.Port)
}

// linkCommand cables a link again: both ports Initialize/LinkUp until the SM arms and
// activates them, and trap 128.
func linkCommand(l *Link) string {
	return fmt.Sprintf("Link %q[%d] %q[%d]", l.A.Node.ID, l.A.Port, l.B.Node.ID, l.B.Port)
}

// consoleError reads ibsim's answer to a command: it reports a refusal as a line starting
// with "#". A link that is already in the wanted state is not an error (the state is right).
func consoleError(out string, up bool) error {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case !up && strings.Contains(line, "is not linked"):
		case up && strings.Contains(line, "already connected"):
		default:
			return fmt.Errorf("ibsim: %s", strings.TrimSpace(strings.TrimPrefix(line, "#")))
		}
	}
	return nil
}

// Stop stops following and quits every ibsim.
func (a *Adapter) Stop() error {
	if a.rt != nil {
		smstate.Unregister(a.rt.Engine, a)
	}
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	for _, s := range a.sims {
		if s.proc != nil {
			s.proc.mu.Lock()
			stopping := s.proc.stopping
			s.proc.mu.Unlock()
			if !stopping {
				s.proc.stop()
			}
		}
	}
	return nil
}

// Infos describes every fabric the adapter rendered.
func (a *Adapter) Infos() []Info {
	var out []Info
	for _, s := range a.sims {
		sw, ca, links := s.topo.Counts()
		in := Info{Fabric: s.topo.Fabric, Topology: s.netFile, Switches: sw, CAs: ca, Links: links, Notes: s.topo.Notes, Why: s.why}
		if s.topo.SM != nil {
			in.SMHost, in.SMPort, in.SMPortGUID = s.topo.SM.ID, s.topo.SM.Port, fmt.Sprintf("0x%016x", s.topo.SM.PortGUID)
		}
		if s.proc != nil {
			in.Running, in.SockName, in.PID, in.Transcript = s.proc.running(), s.sock, s.proc.pid(), s.logFile
			in.Binary, in.Version, in.Preload = a.bin, a.version, a.preload
		} else if s.con != nil {
			in.Running = true
		}
		out = append(out, in)
	}
	return out
}

// writeInfo writes InfoFile and, per running fabric, ibsim-<fabric>.env for a shell to source.
func (a *Adapter) writeInfo() error {
	if a.rt.StateDir == "" {
		return nil
	}
	infos := a.Infos()
	b, _ := json.MarshalIndent(infos, "", "  ")
	if err := os.WriteFile(filepath.Join(a.rt.StateDir, InfoFile), b, 0o644); err != nil {
		return err
	}
	for _, in := range infos {
		if in.SockName == "" {
			continue
		}
		var env strings.Builder
		fmt.Fprintf(&env, "# The simulated InfiniBand fabric %s (dc-twin's ibsim adapter). Source this, then run a\n", in.Fabric)
		env.WriteString("# MAD client under the preload, e.g.  LD_PRELOAD=$IBSIM_PRELOAD opensm -f osm.log\n")
		env.WriteString("# SIM_HOST is the subnet manager's attachment; set it to another node id of the topology to\n")
		env.WriteString("# attach a client elsewhere.\n")
		for _, kv := range [][2]string{{"IBSIM_SOCKNAME", in.SockName}, {"SIM_HOST", in.SMHost}, {"IBSIM_PRELOAD", in.Preload}, {"IBSIM_TOPOLOGY", in.Topology}} {
			fmt.Fprintf(&env, "%s=%s\n", kv[0], shellQuote(kv[1]))
		}
		env.WriteString("export IBSIM_SOCKNAME SIM_HOST IBSIM_PRELOAD IBSIM_TOPOLOGY\n")
		if err := os.WriteFile(filepath.Join(a.rt.StateDir, "ibsim-"+in.Fabric+".env"), []byte(env.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ReadInfo reads the fabrics a running twin's ibsim adapter described in its state directory.
func ReadInfo(stateDir string) ([]Info, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, InfoFile))
	if err != nil {
		return nil, err
	}
	var out []Info
	return out, json.Unmarshal(b, &out)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// findPreload looks for libumad2sim where distributions and ibsim's own `make install` put it.
func findPreload(bin string) string {
	pats := []string{
		"/usr/lib/*/umad2sim/libumad2sim.so*", "/usr/lib64/umad2sim/libumad2sim.so*",
		"/usr/lib/umad2sim/libumad2sim.so*", "/usr/local/lib/umad2sim/libumad2sim.so*",
		"/usr/local/lib64/umad2sim/libumad2sim.so*",
		filepath.Join(filepath.Dir(bin), "..", "lib", "umad2sim", "libumad2sim.so*"),
		filepath.Join(filepath.Dir(bin), "..", "umad2sim", "libumad2sim.so*"),
	}
	for _, p := range pats {
		if m, _ := filepath.Glob(p); len(m) > 0 {
			return filepath.Clean(m[0])
		}
	}
	return ""
}

// Report is the adapter's fidelity-report contribution.
func (a *Adapter) Report() adapter.Report {
	r := adapter.Report{Adapter: "ibsim"}
	for _, s := range a.sims {
		sw, ca, links := s.topo.Counts()
		shape := fmt.Sprintf("%d switches, %d channel adapter ports, %d links", sw, ca, links)
		sm := ""
		if s.topo.SM != nil {
			sm = fmt.Sprintf("; a subnet manager attaches at SIM_HOST=%s (%s)", s.topo.SM.ID, s.topo.SM.Port)
		}
		if s.con == nil {
			where := ""
			if s.netFile != "" {
				where = " in " + s.netFile
			}
			r.Lines = append(r.Lines, fmt.Sprintf("%s: topology rendered (%s)%s; not simulated: %s", s.topo.Fabric, shape, where, s.why))
			continue
		}
		r.Served++
		s.mu.Lock()
		applied := fmt.Sprintf("%d link change(s) applied through the console", s.changes)
		if s.errs > 0 {
			applied += fmt.Sprintf(", %d refused (last: %s)", s.errs, s.lastErr)
		}
		s.mu.Unlock()
		who := "a test console"
		if s.proc != nil {
			who = fmt.Sprintf("ibsim %s (pid %d, IBSIM_SOCKNAME=%s, preload %s)", a.version, s.proc.pid(), s.sock, orNone(a.preload))
		}
		r.Lines = append(r.Lines, fmt.Sprintf("%s: SIMULATED by %s from the model: %s%s; %s; topology %s", s.topo.Fabric, who, shape, sm, applied, s.netFile))
		for _, n := range s.topo.Notes {
			r.Lines = append(r.Lines, s.topo.Fabric+": "+n)
		}
		r.SubstrateOwned = append(r.SubstrateOwned, fmt.Sprintf("%s: InfiniBand port logical state (Initialize, Armed, Active), LIDs, forwarding tables, partitions and subnet manager state, held by ibsim and set by the stack's subnet manager", s.topo.Fabric))
	}
	r.Lines = append(r.Lines, a.lines...)
	r.IsNot = []string{
		"The InfiniBand fabric is simulated by ibsim from the model: it answers subnet management and performance MADs (SMPs, SA, PerfMgr) through libumad2sim in place of the kernel's umad devices. There is no data plane: no packets, RDMA, IPoIB or congestion.",
		"Cabling, link up and down, and power are the model's, applied to ibsim through its console (Unlink and Link); a powered-off or AC-less device is simulated as its links unlinked, not as a node that disappears with its ports.",
		"Port logical state, LIDs, forwarding tables and partitions are what the stack's subnet manager set in ibsim. The ufm adapter reads the LIDs from ibsim (console Dump, at most twice a second) once a subnet manager has assigned any; its port states are still the model's (every cabled, powered port Active), not the SM's.",
		"A channel adapter's node GUID is its port GUID minus one, because ibsim answers PortGUID = NodeGUID + port number; a ConnectX reports equal node and port GUIDs. Port GUIDs are the model's.",
		"Switch ports share the switch's GUID (the GUID of its first InfiniBand port in the model), as on real InfiniBand; the model's per-port GUIDs of switch ports are not used. A model GUID longer than 64 bits would be cut to its leading 64 bits (identity/v1 derives none).",
		"Vendor and device ids are 0 (the model has no PCI ids for fabric devices); speeds above NDR are simulated as NDR; ibsim takes at most 10 MAD clients at once.",
		"libumad2sim carries no RMPP: an SA response longer than one MAD (an unfiltered table query, such as every NodeRecord) reaches the client as its first segment; single-record queries are whole.",
		"Link training, cable and transceiver information, bit errors and port counters are ibsim's (counters are settable from its console), not the model's.",
	}
	return r
}

func orNone(s string) string {
	if s == "" {
		return "not found"
	}
	return s
}

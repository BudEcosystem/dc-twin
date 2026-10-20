// Package inhost is the in-host agent (docs/spec/05-interfaces.md — Inside a booted host): what
// the twin puts inside an `os: twin` `vm` host (D23) so that software running against the host
// sees, from inside it, what the model says happened to it. Today that is the kernel log: a
// fault whose catalogue entry has a `kernelLog` effect (Xid 79's "NVRM: Xid (PCI:…): 79, …") is
// written into the guest's real kernel ring buffer through /dev/kmsg, and the guest's kernel log
// is carried back out, record by record, as /dev/kmsg gives it, into the twin's state directory:
// the records as they are (kmsg.log) and as the journal entries journald would make of them
// (journal.export, in the journal export format systemd-journal-remote reads), for node-local
// consumers that read a host's journal, such as NVSentinel's syslog-health-monitor.
//
// Both halves are here: GuestAgent is the guest's (the vm adapter's /init runs it), and the
// Adapter is the twin's. They speak on the guest's serial console (D50; agent.go says why that and
// not vsock), never on a network the stack can see (D10).
//
// With the stack's kubeconfig among its outputs, the adapter also registers each such host as a
// Node and keeps it as a kubelet would (node.go): Ready while the guest's agent is alive, its
// boot ID the guest's own. No kubelet runs in the guest, so that is IMPERSONATION (D24) and a run
// that relies on it is labelled so.
package inhost

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/identity"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Name is the adapter's registered name.
const Name = "inhost"

// observer is the principal of what the adapter reports into the model.
const observer = "inhost"

// ConsoleSocket locates a `vm` device's serial console socket (QEMU's, server side) while its
// machine runs. The vm adapter, which owns the machine, sets it; nil when the vm adapter is not
// linked in, and then nothing here can reach a guest.
var ConsoleSocket func(stateDir, devicePath string) string

func init() { twin.Register(Name, func() adapter.Adapter { return New() }) }

// Adapter is the twin's half of the in-host agent, for every `os: twin` `vm` host.
type Adapter struct {
	rt       *adapter.Runtime
	guests   []*guest
	byPath   map[string]*guest
	nodes    *nodes
	inactive string
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// New returns an adapter that has not started.
func New() *Adapter { return &Adapter{byPath: map[string]*guest{}} }

func (a *Adapter) Name() string { return Name }

// guest is one host's agent as the twin sees it.
type guest struct {
	a    *Adapter
	d    *model.Device
	sock string
	dir  string

	mu        sync.Mutex
	conn      net.Conn
	listening bool // the agent said it listens, since the machine last started or reset
	attached  bool
	info      Hello
	lastAlive time.Time
	nextSeq   uint64
	seqBoot   string
	queued    []line // fault lines waiting for the agent
	pending   []line // written, waiting for their record
	written   int
	recorded  int
	marked    int // of those, entered with the /dev/kmsg path's marking
	records   int
	kmsg      *os.File
	journal   *os.File
	lastErr   string
}

// Hello is what the agent says of the guest when the twin attaches.
type Hello struct {
	Boot, Kernel, Arch string
	CPUs               int
	MemKiB             int64
	// Kmod is the agent's kernel module in this boot: loaded (a fault's line is printk'd, facility
	// kern), refused (the kernel would not load it) or absent (the image carries none); in the last
	// two the line goes through /dev/kmsg (facility user).
	Kmod string
}

// line is a fault's kernel-log line on its way into the guest.
type line struct {
	level          int
	text           string
	fault, faultID string
	component      string
	cause          uint64
}

// Start finds the `os: twin` `vm` hosts and follows each one's console.
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt = rt
	rt.Engine.Read(func(m *model.Model) {
		for _, d := range m.Devices {
			if d.IsHost() && d.Fidelity == "vm" && d.OS == "twin" {
				g := &guest{a: a, d: d, dir: filepath.Join(rt.StateDir, Name, strings.ReplaceAll(d.Path, "/", "_"))}
				a.guests = append(a.guests, g)
				a.byPath[d.Path] = g
			}
		}
	})
	switch {
	case len(a.guests) == 0:
		a.inactive = "no host is at vm fidelity with os: twin"
		return nil
	case ConsoleSocket == nil:
		a.inactive = "the vm adapter is not linked in, so no guest's console can be reached"
		return nil
	}
	for _, g := range a.guests {
		g.sock = ConsoleSocket(rt.StateDir, g.d.Path)
		if err := os.MkdirAll(g.dir, 0o755); err != nil {
			return err
		}
		var err error
		if g.kmsg, err = os.OpenFile(filepath.Join(g.dir, "kmsg.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
			return err
		}
		if g.journal, err = os.OpenFile(filepath.Join(g.dir, "journal.export"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
			return err
		}
	}
	lctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	if kc := strings.TrimSpace(rt.Outputs[OutputKubeconfig]); kc != "" {
		n, err := newNodes(a, kc)
		if err != nil {
			cancel()
			return fmt.Errorf("inhost: %w", err)
		}
		a.nodes = n
		a.wg.Add(1)
		go n.run(lctx)
	}
	a.wg.Add(1 + 2*len(a.guests))
	go a.watch(lctx)
	for _, g := range a.guests {
		go g.follow(lctx)
		go g.tend(lctx)
	}
	rt.Log.Info("inhost: following os: twin guests", "hosts", len(a.guests))
	return nil
}

// Stop disconnects from every guest. The guests keep running.
func (a *Adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	for _, g := range a.guests {
		g.mu.Lock()
		if g.conn != nil {
			g.conn.Close()
		}
		g.mu.Unlock()
	}
	a.wg.Wait()
	for _, g := range a.guests {
		if g.kmsg != nil {
			g.kmsg.Close()
		}
		if g.journal != nil {
			g.journal.Close()
		}
	}
	return nil
}

// Dir is where a host's kernel log is kept (kmsg.log, journal.export).
func (a *Adapter) Dir(path string) string {
	if g := a.byPath[path]; g != nil {
		return g.dir
	}
	return ""
}

// Guest is what the twin knows of one host's agent.
type Guest struct {
	Path, Node, Socket, Dir string
	Attached                bool
	Hello                   Hello
	Records, Written        int
}

// Guests reports every followed host.
func (a *Adapter) Guests() []Guest {
	var out []Guest
	for _, g := range a.guests {
		g.mu.Lock()
		out = append(out, Guest{Path: g.d.Path, Node: nodeName(g.d), Socket: g.sock, Dir: g.dir, Attached: g.attached, Hello: g.info, Records: g.records, Written: g.written})
		g.mu.Unlock()
	}
	return out
}

// Report is the adapter's fidelity-report contribution.
func (a *Adapter) Report() adapter.Report {
	r := adapter.Report{Adapter: Name}
	if a.inactive != "" {
		r.Lines = []string{"in-host agent: inactive: " + a.inactive}
		return r
	}
	for _, g := range a.guests {
		g.mu.Lock()
		state := "not attached"
		if g.attached {
			state = fmt.Sprintf("attached (boot %s, kernel %s)", g.info.Boot, g.info.Kernel)
			r.Served++
		}
		path := "its kernel module (facility kern)"
		if g.info.Kmod != "loaded" {
			path = fmt.Sprintf("/dev/kmsg (facility user, entries marked; kernel module %s)", g.info.Kmod)
		}
		r.Lines = append(r.Lines, fmt.Sprintf("in-host agent: %s: %s over its serial console; %d fault line(s) written through %s, %d seen back in the kernel log (%d marked); %d kernel records carried to %s",
			g.d.Path, state, g.written, path, g.recorded, g.marked, g.records, filepath.Join(g.dir, "journal.export")))
		if g.lastErr != "" {
			r.Lines = append(r.Lines, "in-host agent: "+g.d.Path+": last error: "+g.lastErr)
		}
		g.mu.Unlock()
	}
	r.IsNot = []string{
		"The kernel log is the guest's own ring buffer; a fault's line is printk'd into it by the in-host agent's kernel module (dctwin-kmsg), a record with facility kern, not by a driver, which does not run. Where the guest image carries no module or the kernel refuses it, the line is written through /dev/kmsg, where the kernel gives it facility user, and its journal entry is entered as a driver's printk and marked DCTWIN_FAULT (D50).",
		"No journald runs in the twin's guest (Alpine's netboot initramfs): journal.export is the twin's mapping of each record to the entry journald makes of it (journald-kmsg.c). Device properties (_KERNEL_*, _UDEV_*) are not carried.",
		"The channel is the guest's serial console, so the agent's lines show on the console beside the guest's own; nothing is sent until the twin attaches, and a second console client cannot connect while the twin holds it.",
		"Lines are written when a fault is injected; a fault that is still active when the guest reboots is not logged again at boot.",
	}
	if a.nodes != nil {
		r.Impersonated = []string{"inhost (kubelet for os: twin vm hosts)"}
		r.Lines = append(r.Lines, a.nodes.reportLine())
		r.IsNot = append(r.IsNot, a.nodes.isNot()...)
	}
	return r
}

// ---------------------------------------------------------------- following a guest

// follow keeps a connection to the guest's console while its machine runs, and reads it.
func (g *guest) follow(ctx context.Context) {
	defer g.a.wg.Done()
	for ctx.Err() == nil {
		c, err := net.DialTimeout("unix", g.sock, time.Second)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
			continue
		}
		g.mu.Lock()
		g.conn = c
		g.mu.Unlock()
		g.read(c)
		c.Close()
		g.mu.Lock()
		g.conn = nil
		g.mu.Unlock()
		g.detach("the machine's console closed (the machine stopped)")
	}
}

func (g *guest) read(c net.Conn) {
	rd := bufio.NewReaderSize(c, 64<<10)
	for {
		s, err := rd.ReadString('\n')
		if s != "" {
			g.line(strings.TrimRight(s, "\r\n"))
		}
		if err != nil {
			return
		}
	}
}

// line handles one line of console output.
func (g *guest) line(s string) {
	f, ok := ParseFrame(s)
	if !ok {
		// the guest's own console output: the kernel's banner is a new boot, and the agent's
		// first kernel-log line says it listens
		switch {
		case strings.Contains(s, "Booting Linux on physical CPU") || strings.Contains(s, "] Linux version "):
			g.detach("the guest's kernel is starting")
		case strings.Contains(s, Listening):
			g.mu.Lock()
			g.listening = true
			g.mu.Unlock()
			g.send(AttachLine())
		}
		return
	}
	switch f.Kind {
	case "hello":
		g.hello(f)
	case "alive":
		g.mu.Lock()
		if g.attached && f.Fields["boot"] == g.info.Boot {
			g.lastAlive = time.Now()
		}
		g.mu.Unlock()
	case "kmsg":
		g.record(f.Record)
	}
}

func (g *guest) hello(f Frame) {
	h := Hello{Boot: f.Fields["boot"], Kernel: f.Fields["kernel"], Arch: f.Fields["arch"]}
	h.CPUs, _ = strconv.Atoi(f.Fields["cpus"])
	h.MemKiB, _ = strconv.ParseInt(f.Fields["memKiB"], 10, 64)
	h.Kmod = f.Fields["kmod"]
	if h.Kmod == "" {
		h.Kmod = "absent"
	}
	g.mu.Lock()
	first := !g.attached || g.info.Boot != h.Boot
	g.attached, g.listening, g.info, g.lastAlive = true, true, h, time.Now()
	queued := g.queued
	g.queued = nil
	g.mu.Unlock()
	if first {
		g.observe("inhost.agent", "attached", fmt.Sprintf("in-host agent attached over the serial console: boot %s, kernel %s %s, %d CPUs, %d KiB, kernel module %s", h.Boot, h.Kernel, h.Arch, h.CPUs, h.MemKiB, h.Kmod), 0,
			map[string]any{"boot": h.Boot, "kernel": h.Kernel, "arch": h.Arch, "cpus": h.CPUs, "memKiB": h.MemKiB, "kmod": h.Kmod})
		if g.a.nodes != nil {
			g.a.nodes.mark(g)
		}
		queued = append(queued, g.a.bootLines(g)...)
	}
	for _, l := range queued {
		g.write(l)
	}
}

// detach forgets the agent: the machine stopped, was reset or is booting again.
func (g *guest) detach(why string) {
	g.mu.Lock()
	was := g.attached
	g.attached, g.listening = false, false
	g.pending = nil
	g.mu.Unlock()
	if was {
		g.observe("inhost.agent", "detached", "in-host agent detached: "+why, 0, nil)
		if g.a.nodes != nil {
			g.a.nodes.mark(g)
		}
	}
}

// tend attaches to an agent that is listening but not attached (the twin connected after the
// agent said so: the model's boot stage says the guest's OS is up), and detaches one that has
// gone quiet.
func (g *guest) tend(ctx context.Context) {
	defer g.a.wg.Done()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var booted, power string
		g.a.rt.Engine.Read(func(*model.Model) { booted, power = g.d.State.Booted, g.d.State.Power })
		g.mu.Lock()
		connected, attached, listening, quiet := g.conn != nil, g.attached, g.listening, time.Since(g.lastAlive) > 3*HeartbeatEvery*time.Second
		g.mu.Unlock()
		switch {
		case attached && quiet:
			g.detach(fmt.Sprintf("no word from the agent for %ds", 3*HeartbeatEvery))
		case connected && !attached && power == model.On && (listening || booted == "os"):
			g.send(AttachLine())
		}
	}
}

func (g *guest) send(s string) {
	g.mu.Lock()
	c := g.conn
	g.mu.Unlock()
	if c == nil {
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte(s)); err != nil {
		g.fail(err)
	}
}

func (g *guest) fail(err error) {
	g.mu.Lock()
	g.lastErr = err.Error()
	g.mu.Unlock()
	g.a.rt.Log.Warn("inhost", "host", g.d.Path, "err", err)
}

func (g *guest) observe(field, to, msg string, cause uint64, data map[string]any) {
	_ = g.a.rt.Engine.Observe(engine.Observation{Path: g.d.Path, Observer: observer, Field: field, To: to, Message: msg, Cause: cause, Data: data})
}

// ---------------------------------------------------------------- the kernel log

// record handles one /dev/kmsg record the agent carried out: new records (by sequence number,
// per boot) go to kmsg.log and, as journal entries, to journal.export.
func (g *guest) record(raw string) {
	r, err := ParseRecord(raw)
	if err != nil {
		return
	}
	now := time.Now()
	g.mu.Lock()
	boot := g.info.Boot
	if boot == "" {
		g.mu.Unlock()
		return
	}
	// the next sequence number expected, per boot, as journald keeps it (kernel-seqnum)
	if g.seqBoot == boot && r.Seq < g.nextSeq {
		g.mu.Unlock()
		return
	}
	g.seqBoot, g.nextSeq = boot, r.Seq+1
	g.records++
	var hit *line
	for i, l := range g.pending {
		if l.text == r.Text && r.Level() == l.level {
			ll := l
			hit = &ll
			g.pending = append(g.pending[:i], g.pending[i+1:]...)
			g.recorded++
			break
		}
	}
	fmt.Fprintf(g.kmsg, "%s %s\n", boot, raw)
	// A fault's line printk'd by the agent's kernel module is a kernel record (facility kern) and is
	// entered as journald enters one; one written through /dev/kmsg (facility user) is entered as
	// a driver's printk would be, and marked (D50).
	var e Entry
	if hit != nil && r.Facility() != 0 {
		e = JournalEntry(r, boot, nodeName(g.d), now, true, hit.fault, hit.faultID)
		g.marked++
	} else {
		e = JournalEntry(r, boot, nodeName(g.d), now, false, "", "")
	}
	if _, err := g.journal.Write(e.Export()); err != nil {
		g.lastErr = err.Error()
	}
	g.mu.Unlock()
	if hit != nil {
		how := "printk'd by the in-host agent's kernel module"
		if r.Facility() != 0 {
			how = "written through /dev/kmsg, marked in the journal"
		}
		g.observe("kernelLog", "recorded", fmt.Sprintf("%s: the guest's kernel log holds the fault's line (record %d, level %d, facility %d, %s): %s", hit.component, r.Seq, r.Level(), r.Facility(), how, r.Text), hit.cause,
			map[string]any{"component": hit.component, "fault": hit.fault, "faultId": hit.faultID, "seq": r.Seq, "usec": r.Usec, "boot": boot, "record": raw,
				"journal": filepath.Join(g.dir, "journal.export")})
	}
}

// write asks the agent to put a fault's line in the kernel log, or queues it until the agent
// attaches.
func (g *guest) write(l line) {
	g.mu.Lock()
	if !g.attached {
		g.queued = append(g.queued, l)
		g.mu.Unlock()
		return
	}
	g.pending = append(g.pending, l)
	g.written++
	into := "the guest's /dev/kmsg"
	if g.info.Kmod == "loaded" {
		into = "its kernel module (" + KernelModuleFile + ")"
	}
	g.mu.Unlock()
	g.send(KmsgWriteLine(l.level, l.text))
	g.observe("kernelLog", "written", fmt.Sprintf("%s: the in-host agent writes the fault's kernel-log line to %s: <%d>%s", l.component, into, l.level, l.text), l.cause,
		map[string]any{"component": l.component, "fault": l.fault, "faultId": l.faultID, "level": l.level, "line": l.text})
}

// ---------------------------------------------------------------- faults

// watch follows the engine's events: an injected fault with a kernelLog effect on one of the
// guests' components becomes lines for that guest; a reset or power-off detaches the agent.
func (a *Adapter) watch(ctx context.Context) {
	defer a.wg.Done()
	id, ch := a.rt.Engine.Subscribe(4096)
	for {
		select {
		case <-ctx.Done():
			a.rt.Engine.Unsubscribe(id)
			return
		case ev, ok := <-ch:
			if !ok {
				id, ch = a.rt.Engine.Subscribe(4096)
				continue
			}
			a.event(ev)
		}
	}
}

func (a *Adapter) event(ev schema.Event) {
	g := a.guestOf(ev.Path)
	if g == nil {
		return
	}
	switch {
	case ev.Kind == "fault":
		if _, cleared := ev.Data["cleared"]; cleared {
			return
		}
		for _, l := range a.kernelLines(g, ev) {
			g.write(l)
		}
	case ev.Kind == "observe" && ev.State != nil && ev.State.Field == "reset" && ev.Source.Principal != observer:
		g.detach(fmt.Sprintf("the machine was reset (%v)", ev.State.To))
	case ev.Kind == "state" && ev.State != nil && ev.State.Field == "power" && ev.State.To == model.Off:
		g.detach("the machine is off")
	}
}

func (a *Adapter) guestOf(p string) *guest {
	if i := strings.IndexByte(p, ':'); i >= 0 {
		p = p[:i]
	}
	for p != "" {
		if g, ok := a.byPath[p]; ok {
			return g
		}
		i := strings.LastIndexByte(p, '/')
		if i < 0 {
			return nil
		}
		p = p[:i]
	}
	return nil
}

// kernelLines renders the kernelLog effects of an injected fault that apply inside a `vm` host:
// the lines a driver writes when the fault happens. Lines marked `when: boot` are not among them.
func (a *Adapter) kernelLines(g *guest, ev schema.Event) []line {
	faultID, _ := ev.Data["fault"].(string)
	params, _ := ev.Data["params"].(map[string]any)
	fid, _ := ev.Data["id"].(string)
	return a.faultLines(g, faultID, fid, ev.Path, params, ev.Seq, false)
}

// bootLines renders, for a guest that has just booted, the `when: boot` kernelLog lines of every
// fault still active on its components: what a driver logs when it probes a device the fault left
// broken (Xid 79's GPU, which reads all ones until an AC cycle, is reported as fallen off the bus
// at every boot).
func (a *Adapter) bootLines(g *guest) []line {
	var out []line
	for _, f := range a.rt.Engine.Faults(g.d.Path) {
		out = append(out, a.faultLines(g, f.Fault, f.ID, f.Target, nil, 0, true)...)
	}
	return out
}

// faultLines renders one fault's kernelLog effects that apply at `vm`: at boot (atBoot) the
// `when: boot` ones, otherwise the rest.
func (a *Adapter) faultLines(g *guest, faultID, fid, target string, params map[string]any, cause uint64, atBoot bool) []line {
	ref := a.rt.Engine.Catalogue(faultID)
	if ref == nil || ref.Spec == nil {
		return nil
	}
	comp, index, uuid := "", 0, ""
	for _, c := range g.d.Components {
		if c.Path == target {
			comp, index = c.Name, c.Index
			uuid = "GPU-" + identity.New(a.rt.Engine.M.Seed).UUID(c.Path, g.d.Identity.ProfileMajor, c.Index)
		}
	}
	var out []line
	for _, eff := range ref.Spec.Effects {
		body, ok := eff["kernelLog"].(map[string]any)
		if !ok {
			continue
		}
		if at, ok := body["appliesAt"].([]any); ok && !hasString(at, "vm") {
			continue
		}
		if (fmt.Sprint(body["when"]) == "boot") != atBoot {
			continue
		}
		text, _ := body["line"].(string)
		if text == "" {
			continue
		}
		level := 4 // the kernel's default message level, for a printk that names none
		if v, ok := body["level"]; ok {
			if n, err := strconv.Atoi(fmt.Sprint(v)); err == nil {
				level = n
			}
		}
		out = append(out, line{level: level, text: Printable(render(text, params, comp, index, uuid), 900), fault: faultID, faultID: fid, component: target, cause: cause})
	}
	return out
}

func hasString(list []any, s string) bool {
	for _, x := range list {
		if fmt.Sprint(x) == s {
			return true
		}
	}
	return false
}

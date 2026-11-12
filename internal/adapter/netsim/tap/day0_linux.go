//go:build linux

package tap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/BudEcosystem/dc-twin/internal/day0"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Day 0 on the TAP substrate (18): a factory switch starts in its NOS's factory state, its
// management port on the OOB network (D73), and the software under test brings it up. The
// substrate does what the switch would do on its own (boot, run its ZTP, lease its management
// address) and watches what it becomes: the day-0 observer reads each factory switch's hostname,
// BGP sessions, routes, management lease, VLANs, LLDP neighbours and its ZTP's and ONIE's status
// from the switch itself, holds them against the design (day0.Evaluate) and commits every change
// as a state event from source class observer, as the NOS observer does (D19).

// day0Interval is how often the day-0 observer reads the factory switches: a poll that takes
// longer delays the next (a ticker drops the ticks it missed).
const day0Interval = 10 * time.Second

// day0Inputs is where a factory estate's design is written for the day-0 tool (D77).
const day0Inputs = "day0"

// factoryBringUp brings a factory switch up on its own time: its start-up script (after ONIE's
// install when it has one), its boot time, the NOS's sidecars, its factory configuration and its
// ZTP. Nothing of the design is applied.
func (s *Substrate) factoryBringUp(sw *nosSwitch) {
	defer s.wg.Done()
	f := sw.plan.Config.Factory
	path := sw.plan.Device.Path
	start := time.Now()
	fail := func(err error) {
		s.log.Warn("tap: a factory switch did not come up", "switch", path, "err", err)
		s.eng.Incident("tap", fmt.Sprintf("factory switch %s did not come up: %v", path, err))
	}
	for {
		logs, _ := s.docker.Logs(s.ctx, sw.c.ID)
		if nos.Started(logs) {
			break
		}
		if s.ctx.Err() != nil {
			return
		}
		if _, err := s.docker.Pid(s.ctx, sw.c.ID); err != nil {
			fail(fmt.Errorf("its container %s stopped: %v\n%s", sw.c.Name, err, logs))
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	// dead on arrival: no boot until the fault clears (switch-doa)
	for !s.mgmtDOAClear(sw) {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(time.Second):
		}
		start = time.Now()
	}
	boot := f.BootTime
	if v, ok := s.eng.FaultSets(sw.plan.Device)["day0.bootFactor"]; ok {
		if k, err := strconv.ParseFloat(fmt.Sprint(v), 64); err == nil && k > 1 {
			boot = time.Duration(float64(boot) * k) // switch-slow-boot
		}
	}
	if wait := boot - time.Since(start); wait > 0 {
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
	s.log.Info("tap: factory switch booted", "switch", path, "nos", sw.plan.NOS.Name, "after", time.Since(start).Round(time.Second))
	if sw.plan.NOS.RelayImage != "" {
		if err := s.startRelay(sw); err != nil {
			fail(err)
			return
		}
	}
	if err := s.startSidecars(sw); err != nil {
		fail(err)
		return
	}
	if err := s.waitSidecarsOf(sw, time.Now().Add(sw.plan.NOS.StartTimeout+5*time.Minute)); err != nil {
		fail(err)
		return
	}
	switch sw.plan.NOS.API {
	case "nvue":
		if err := s.startNVUE(sw); err != nil {
			fail(err)
			return
		}
	default:
		// SONiC: its management port's DHCP client, as SONiC's interfaces-config runs one on eth0
		// (a VS container runs no systemd unit to start it)
		if err := s.docker.ExecDetached(s.ctx, sw.c.ID, nos.SonicFactoryMgmt...); err != nil {
			fail(fmt.Errorf("its management port's DHCP client: %w", err))
			return
		}
	}
	if f.ZTP {
		if err := s.docker.ExecDetached(s.ctx, sw.c.ID, nos.Day0Mount+"/dctwin-day0", "ztp", "daemon", "--nos", sw.plan.NOS.ZTPFlavour()); err != nil {
			fail(fmt.Errorf("its ZTP: %w", err))
			return
		}
	}
	sw.factoryUp.Store(true)
}

// waitSidecarsOf waits for one switch's sidecars, as waitSidecars does for every switch.
func (s *Substrate) waitSidecarsOf(sw *nosSwitch, deadline time.Time) error {
	for _, sc := range sw.plan.NOS.SidecarsOf(sw.plan.Config) {
		c := sw.sidecars[sc.Role]
		if c == nil {
			continue
		}
		for {
			out, _, _ := s.docker.Exec(s.ctx, c.ID, sc.Check...)
			waiting, repair := sc.Pending(sw.plan.Config, out)
			if waiting == "" {
				break
			}
			if s.ctx.Err() != nil {
				return s.ctx.Err()
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("its %s container %s has not got %s", sc.Role, c.Name, waiting)
			}
			if repair != nil {
				_, _, _ = s.docker.Exec(s.ctx, c.ID, repair...)
			}
			time.Sleep(2 * time.Second)
		}
	}
	return nil
}

// execIn runs a command in a switch's container and fails on a non-zero exit.
func (s *Substrate) execIn(sw *nosSwitch, cmd ...string) (string, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	out, code, err := s.docker.Exec(ctx, sw.c.ID, cmd...)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d: %s", code, strings.TrimSpace(out))
	}
	return out, err
}

// writeDay0 writes what a factory switch's day-0 programs read (its identity, ONIE's images) to
// the directory mounted at /etc/dctwin in it, and the design to stack-inputs/day0 (D77).
func (s *Substrate) writeDay0Files() error {
	if !s.plan.Factory() || s.stateDir == "" {
		return nil
	}
	s.eng.Read(func(m *model.Model) { s.design = s.plan.Day0Design(m) })
	if err := day0.WriteInputs(filepath.Join(s.stateDir, "stack-inputs", day0Inputs), s.design); err != nil {
		return fmt.Errorf("stack-inputs/%s: %w", day0Inputs, err)
	}
	return nil
}

// day0Dir is a factory switch's /etc/dctwin on the Docker host, under the twin's state.
func (s *Substrate) day0Dir(sp *SwitchPlan) string {
	return filepath.Join(s.stateDir, "day0", sp.Container)
}

// prepareDay0 writes a factory switch's /etc/dctwin before its container starts, and returns the
// binds that mount it and the twin's day-0 programs.
func (s *Substrate) prepareDay0(sp *SwitchPlan) ([]string, error) {
	f := sp.Config.Factory
	if f == nil {
		return nil, nil
	}
	if s.stateDir == "" {
		return nil, fmt.Errorf("a factory switch needs the twin's state directory")
	}
	dir := s.day0Dir(sp)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	traversable(dir)
	id := f.Identity
	id.NOS = sp.NOS.Name
	if id.Version == "" {
		id.Version = sp.NOS.SwitchVersion(sp.Config)
	}
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	sp.nvueToken = hex.EncodeToString(tok)
	// the token `nv` presents to NVUE as the local user (nvue.SetLocalToken): readable in the
	// switch by every local user, as Cumulus's nv socket is open to its nv groups
	if err := os.WriteFile(filepath.Join(dir, filepath.Base(nvue.TokenFile)), []byte(sp.nvueToken), 0o644); err != nil {
		return nil, err
	}
	files := map[string]any{"identity.json": id, "images.json": f.Images,
		"factory.json": map[string]any{"install": f.Install, "ztp": f.ZTP, "nos": sp.NOS.Name, "installTime": f.InstallTime.String()}}
	for name, v := range files {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644); err != nil {
			return nil, err
		}
	}
	bin := s.plan.Day0Binaries
	if bin == "" {
		return nil, fmt.Errorf("switch %s: the twin's day-0 programs (dctwin-day0) were not found: build them (go build -o <dir>/dctwin-day0 ./cmd/dctwin-day0) and set spec.adapters.network.nos.day0Binaries to <dir>", sp.Device.Path)
	}
	return []string{dir + ":" + nos.Day0Dir + ":ro", bin + ":" + nos.Day0Mount + ":ro"}, nil
}

// ---------------------------------------------------------------- the day-0 observer

// day0Observer reads factory switches and holds them against the design.
type day0Observer struct {
	s    *Substrate
	mu   sync.Mutex
	last map[string]string // "<path>|<field>" -> value
	obs  map[string]day0.Observed
	verd map[string]day0.Verdict
	// lldpAt is when each switch's LLDP neighbours were last read, lldpLast what they were
	lldpAt   map[string]time.Time
	lldpLast map[string]map[string]string
}

// lldpInterval is how often the day-0 observer asks a switch's lldpd for its neighbours: the one
// read that is an exec (lldpd's control socket speaks its own protocol), so a rare one.
const lldpInterval = time.Minute

// observe reads one factory switch, without exec but for LLDP (direct_linux.go): its addresses
// over netlink in its namespace, its ZTP's and ONIE's status files and its hostname from its own
// filesystem, its FRR over the daemons' vty sockets.
func (o *day0Observer) observe(ctx context.Context, sw *nosSwitch) day0.Observed {
	ob := day0.Observed{Sessions: map[string]day0.Session{}, Vlans: map[string]string{}, LLDP: map[string]string{}}
	if links, err := sw.h.LinkList(); err == nil {
		for _, l := range links {
			name := l.Attrs().Name
			if name != nos.MgmtIf && !strings.HasPrefix(strings.ToLower(name), "vlan") {
				continue
			}
			addrs, err := sw.h.AddrList(l, unix.AF_INET)
			if err != nil || len(addrs) == 0 {
				continue
			}
			a := addrs[0].IPNet.String()
			if name == nos.MgmtIf {
				ob.Mgmt = a
			} else {
				ob.Vlans[name] = a
			}
		}
	}
	pid := sw.c.Pid
	var st struct {
		Phase string `json:"phase"`
	}
	if b, err := containerFile(pid, nos.Day0State+"/onie/status.json"); err == nil && json.Unmarshal(b, &st) == nil && st.Phase != "" && st.Phase != "installed" {
		ob.Phase = day0.PhaseONIE
	}
	var zs struct {
		State string `json:"state"`
	}
	if b, err := containerFile(pid, nos.Day0State+"/ztp/status.json"); ob.Phase == "" && err == nil && json.Unmarshal(b, &zs) == nil {
		switch zs.State {
		case "running", "discovering":
			ob.Phase = day0.PhaseZTPRunning
		case "success":
			ob.Phase = day0.PhaseZTPDone
		case "failed":
			ob.Phase = day0.PhaseZTPFailed
		}
	}
	if b, err := containerFile(pid, "/etc/hostname"); err == nil {
		ob.Hostname = strings.TrimSpace(string(b))
	}
	if sw.nvue != nil {
		if h, ok := sw.nvue.Applied()["system"].(map[string]any); ok {
			if name, _ := h["hostname"].(string); name != "" {
				ob.Hostname = name
			}
		}
	} else if h, err := redisHGet(ctx, pid, 4, "DEVICE_METADATA|localhost", "hostname"); err == nil && h != "" {
		ob.Hostname = h
	}
	rpid := sw.routing().Pid
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	sum, err1 := vtyshCommand(cctx, rpid, "bgpd", "show bgp vrf all summary json")
	rib, err2 := vtyshCommand(cctx, rpid, "zebra", "show ip route json")
	cancel()
	if err1 == nil || err2 == nil {
		ob.Sessions, ob.Routes = parseFRR(sum + "\n" + rib)
	}
	ob.LLDP = o.lldp(ctx, sw)
	if !sw.factoryUp.Load() && ob.Phase == "" {
		ob.Phase = day0.PhaseFactory
	}
	return ob
}

// lldp is a switch's LLDP neighbours, read from its lldpd at most every lldpInterval.
func (o *day0Observer) lldp(ctx context.Context, sw *nosSwitch) map[string]string {
	path := sw.plan.Device.Path
	o.mu.Lock()
	if o.lldpAt == nil {
		o.lldpAt, o.lldpLast = map[string]time.Time{}, map[string]map[string]string{}
	}
	last, at := o.lldpLast[path], o.lldpAt[path]
	o.mu.Unlock()
	if time.Since(at) < lldpInterval {
		return last
	}
	c := sw.c
	if x := sw.sidecars["lldp"]; x != nil {
		c = x // SONiC's lldpd is its lldp container's
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	out, _, err := o.s.docker.Exec(cctx, c.ID, "timeout", "10", "lldpcli", "-f", "json0", "show", "neighbors")
	cancel()
	res := last
	if err == nil {
		res = parseLLDP(out[max(0, strings.Index(out, "{")):])
	}
	o.mu.Lock()
	o.lldpAt[path], o.lldpLast[path] = time.Now(), res
	o.mu.Unlock()
	return res
}

// parseFRR reads vtysh's output for `show bgp vrf all summary json` and `show ip route json`,
// printed one after the other.
func parseFRR(out string) (map[string]day0.Session, []string) {
	sessions := map[string]day0.Session{}
	var routes []string
	dec := json.NewDecoder(strings.NewReader(out[max(0, strings.Index(out, "{")):]))
	for {
		var v map[string]any
		if err := dec.Decode(&v); err != nil {
			break
		}
		for k, x := range v {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			// a route: its value is a list of route entries; a VRF: its address families
			if _, isRoute := x.([]any); isRoute {
				continue
			}
			for af, y := range m {
				fam, ok := y.(map[string]any)
				if !ok {
					continue
				}
				peers, ok := fam["peers"].(map[string]any)
				if !ok {
					continue
				}
				for peer, z := range peers {
					p, _ := z.(map[string]any)
					st, _ := p["state"].(string)
					cur := sessions[peer]
					cur.VRF = k
					if strings.HasPrefix(strings.ToLower(af), "l2vpn") {
						cur.EVPN = st == "Established"
						if cur.State == "" {
							cur.State = st
						}
					} else {
						cur.State = st
					}
					sessions[peer] = cur
				}
			}
		}
		for k, x := range v {
			if _, isRoute := x.([]any); isRoute {
				routes = append(routes, k)
			}
		}
	}
	sort.Strings(routes)
	return sessions, routes
}

// parseLLDP reads lldpcli's json0 output: local port -> "<system name>:<port id>".
func parseLLDP(out string) map[string]string {
	res := map[string]string{}
	var v struct {
		LLDP []struct {
			Interface []struct {
				Name    string `json:"name"`
				Chassis []struct {
					Name []struct {
						Value string `json:"value"`
					} `json:"name"`
				} `json:"chassis"`
				Port []struct {
					ID []struct {
						Value string `json:"value"`
					} `json:"id"`
				} `json:"port"`
			} `json:"interface"`
		} `json:"lldp"`
	}
	if json.Unmarshal([]byte(out), &v) != nil {
		return res
	}
	for _, l := range v.LLDP {
		for _, it := range l.Interface {
			sys, port := "", ""
			if len(it.Chassis) > 0 && len(it.Chassis[0].Name) > 0 {
				sys = it.Chassis[0].Name[0].Value
			}
			if len(it.Port) > 0 && len(it.Port[0].ID) > 0 {
				port = it.Port[0].ID[0].Value
			}
			res[it.Name] = sys + ":" + port
		}
	}
	return res
}

// poll reads every factory switch, evaluates the design and commits what changed.
func (o *day0Observer) poll(ctx context.Context) {
	s := o.s
	obs := map[string]day0.Observed{}
	var factory []*nosSwitch
	for _, sw := range s.switches {
		if sw.plan.Config.Factory != nil {
			factory = append(factory, sw)
		}
	}
	if len(factory) == 0 {
		return
	}
	// one switch at a time: every read is a container exec, and a NOS runtime under load queues
	// them (rootless Podman serialises its database; an exec whose caller gave up still runs)
	for _, sw := range factory {
		if ctx.Err() != nil {
			return
		}
		obs[sw.plan.Device.Path] = o.observe(ctx, sw)
	}
	verdicts := day0.Evaluate(s.design, obs)
	o.mu.Lock()
	o.obs = obs
	o.verd = map[string]day0.Verdict{}
	for _, v := range verdicts {
		o.verd[v.Path] = v
	}
	o.mu.Unlock()
	for _, sw := range factory {
		path := sw.plan.Device.Path
		host := sw.plan.Config.Hostname
		ob := obs[path]
		v := o.verd[path]
		o.commit(host, path, "hostname", ob.Hostname, "")
		o.commit(host, path, "mgmt.lease", ob.Mgmt, "")
		for port, n := range ob.LLDP {
			o.commit(host, path, "lldp "+port, n, "")
		}
		detail := ""
		if len(v.Diffs) > 0 {
			detail = strings.Join(v.Diffs, "; ")
		}
		o.commit(host, path, "day0.phase", v.Phase, detail)
	}
}

// commit commits a day-0 observation that changed (a state event from the observer, D19).
func (o *day0Observer) commit(host, path, field, value, detail string) {
	key := path + "|" + field
	o.mu.Lock()
	if o.last == nil {
		o.last = map[string]string{}
	}
	old, seen := o.last[key]
	o.last[key] = value
	o.mu.Unlock()
	if seen && old == value {
		return
	}
	if !seen && value == "" {
		return
	}
	ev := schema.Event{Kind: "state", Source: schema.EventSource{Class: "observer", Principal: "nos/" + host}, Path: path,
		State: &schema.EventState{Field: field, From: old, To: value}}
	if detail != "" {
		ev.Data = map[string]any{"detail": detail}
	}
	o.s.eng.Commit(ev)
}

func (o *day0Observer) loop(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	t := time.NewTicker(day0Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			o.poll(ctx)
		}
	}
}

// Day0 is the day-0 observer's view: each factory switch's verdict against the design, with what
// it observed (dctwin fabric status, the control API, tests).
func (s *Substrate) Day0() ([]day0.Verdict, map[string]day0.Observed) {
	if s.day0 == nil {
		return nil, nil
	}
	s.day0.mu.Lock()
	defer s.day0.mu.Unlock()
	var out []day0.Verdict
	for _, v := range s.day0.verd {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	obs := map[string]day0.Observed{}
	for k, v := range s.day0.obs {
		obs[k] = v
	}
	return out, obs
}

// Design is the day-0 design the substrate wrote to stack-inputs/day0 (D77).
func (s *Substrate) Design() day0.Design { return s.design }

// traversable lets a NOS runtime that is not the twin's user (rootless Podman, whose containers
// are another uid on the host) reach a directory it mounts: every directory on the way that the
// twin owns gets search permission for others (a test's state directory is 0700). Nothing is made
// readable that was not.
func traversable(dir string) {
	for p := filepath.Clean(dir); p != "/" && p != "."; p = filepath.Dir(p) {
		st, err := os.Stat(p)
		if err != nil {
			return
		}
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Geteuid() {
			continue
		}
		if st.Mode().Perm()&0o001 == 0 {
			_ = os.Chmod(p, st.Mode().Perm()|0o011)
		}
	}
}

// mgmtDOAClear is whether a factory switch is not dead on arrival (switch-doa).
func (s *Substrate) mgmtDOAClear(sw *nosSwitch) bool {
	v, ok := s.eng.FaultSets(sw.plan.Device)["day0.doa"]
	return !ok || v == false || v == "false"
}

// FactoryReset returns a factory switch to its NOS's factory state (18 — Factory state, `dctwin
// switch reset --factory`): its configuration wiped as the NOS's own factory reset wipes it
// (NVUE's factory tree over an empty configuration; SONiC's factory config_db.json reloaded), its
// factory login back and expired, its ZTP armed again and run (`ztp -R` on Cumulus, a fresh ZTP
// document state on SONiC). The NOS is not reinstalled: ONIE's install is not undone.
func (s *Substrate) FactoryReset(ctx context.Context, path string) error {
	return s.factoryReset(ctx, path, false)
}

// FactoryUninstall is FactoryReset with ONIE's uninstall (`onie-select -u`, then ONIE's install
// mode): a switch with install: onie is returned to ONIE, whose discovery fetches, matches and
// installs its NOS again, and only then is the NOS's ZTP armed. The install stays modelled (D75):
// the NOS's processes are not stopped while ONIE runs, its configuration is the factory's, and
// what is real is ONIE's discovery and fetch, as at the first install.
func (s *Substrate) FactoryUninstall(ctx context.Context, path string) error {
	return s.factoryReset(ctx, path, true)
}

func (s *Substrate) factoryReset(ctx context.Context, path string, uninstall bool) error {
	sw := s.realSwitch(path)
	switch {
	case sw == nil:
		return fmt.Errorf("no real switch %s", path)
	case sw.plan.Config.Factory == nil:
		return fmt.Errorf("%s is configured by the twin (bringUp: twin): it has no factory state to return to", sw.plan.Device.Path)
	case !sw.factoryUp.Load():
		return fmt.Errorf("%s is still coming up from the factory", sw.plan.Device.Path)
	case uninstall && sw.plan.Config.Factory.Install != "onie":
		return fmt.Errorf("%s was not installed by ONIE (install: %s): there is no ONIE to return to", sw.plan.Device.Path, sw.plan.Config.Factory.Install)
	case uninstall && sw.plan.NOS.Name != nos.NVUEFRR:
		return fmt.Errorf("%s: ONIE's uninstall is modelled for %s only (its NOS's management DHCP client is restarted after the install)", sw.plan.Device.Path, nos.NVUEFRR)
	}
	f := sw.plan.Config.Factory
	exec := func(script string) error { _, err := s.execIn(sw, "sh", "-c", script); return err }
	// the ZTP daemon stops first: it must not act on the configuration being removed
	_ = exec("pkill -f 'dctwin-day0 ztp daemon' || true")
	switch sw.plan.NOS.Name {
	case nos.NVUEFRR:
		if err := sw.nvue.ResetFactory(ctx); err != nil {
			return fmt.Errorf("NVUE's factory configuration: %w", err)
		}
		if err := exec("echo '" + nvue.User + ":" + nvue.FactoryPassword + "' | chpasswd && chage -d 0 " + nvue.User +
			" && rm -f /run/dctwin/nvue-user-applied && ztp -R >/dev/null 2>&1 || true"); err != nil {
			return fmt.Errorf("the factory login: %w", err)
		}
	case nos.SonicVS:
		db, err := nos.ConfigDB(sw.plan.Config)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(db, "", "    ")
		if err != nil {
			return err
		}
		if err := exec("cat > /etc/sonic/config_db.json <<'EOF'\n" + string(b) + "\nEOF\n" +
			"/usr/local/bin/config.sonic reload -y -n -f /etc/sonic/config_db.json && echo 'admin:" + nos.SonicFactoryPassword + "' | chpasswd" +
			" && rm -f /host/ztp/ztp_data.json /host/ztp/ztp_cfg.json " + nos.Day0State + "/ztp/status.json"); err != nil {
			return fmt.Errorf("SONiC's factory configuration: %w", err)
		}
	default:
		return fmt.Errorf("%s: no factory reset for %s", sw.plan.Device.Path, sw.plan.NOS.Name)
	}
	ztp := ""
	if f.ZTP {
		ztp = nos.Day0Mount + "/dctwin-day0 ztp daemon --nos " + sw.plan.NOS.ZTPFlavour()
	}
	canonical := "switch.factory-reset"
	if uninstall {
		// back to ONIE: its status cleared (the observer reports the switch in ONIE again), its
		// discovery run until it installs, as the start script's first install did, then ZTP
		canonical = "switch.onie-uninstall"
		// ONIE's install releases its lease and flushes eth0 before the NOS boots; the NOS's
		// boot starts its own DHCP client again, so its dhclient is restarted (ifupdown's post-up
		// starts it with no pid file)
		script := "rm -f " + nos.Day0State + "/onie/status.json; until " + nos.Day0Mount + "/dctwin-day0 onie-discover install; do sleep 5; done; " +
			"kill $(cat /run/dhclient." + nos.MgmtIf + ".pid 2>/dev/null) 2>/dev/null; rm -f /run/dhclient." + nos.MgmtIf + ".pid; ifreload -a"
		if ztp != "" {
			script += "; exec " + ztp
		}
		if err := s.docker.ExecDetached(s.ctx, sw.c.ID, "sh", "-c", script); err != nil {
			return fmt.Errorf("ONIE's install mode: %w", err)
		}
	} else if ztp != "" {
		if err := s.docker.ExecDetached(s.ctx, sw.c.ID, "sh", "-c", "exec "+ztp); err != nil {
			return fmt.Errorf("its ZTP: %w", err)
		}
	}
	s.eng.LogRead(engine.Request{Path: sw.plan.Device.Path, Canonical: canonical, Protocol: "control", Method: "POST",
		Target: "/v1/switch/factory-reset", Source: schema.EventSource{Class: "operator", Principal: "dctwin"}}, 200)
	return nil
}

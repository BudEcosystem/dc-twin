// Package twin brings an estate up: compile, engine, substrate, adapters; and down again.
package twin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Factory builds an adapter; packages register theirs in init.
type Factory func() adapter.Adapter

var (
	regMu     sync.Mutex
	factories = map[string]Factory{}
)

// Register adds an adapter factory under a name.
func Register(name string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	factories[name] = f
}

// NetworkSubstrate builds a link-layer substrate for an estate (netsim.L2): the tap package
// registers "tap" on Linux (D20). Without one, endpoints are loopback ports (D34).
type NetworkSubstrate func(ctx context.Context, a NetworkArgs) (netsim.L2, error)

// NetworkArgs is what a network substrate is built from.
type NetworkArgs struct {
	Estate   *schema.Estate
	Engine   *engine.Engine
	StateDir string
	Outputs  map[string]string
	Log      *slog.Logger
}

var netSubstrates = map[string]NetworkSubstrate{}

// RegisterNetworkSubstrate adds a network substrate under the name spec.adapters.network.substrate
// selects.
func RegisterNetworkSubstrate(name string, f NetworkSubstrate) {
	regMu.Lock()
	defer regMu.Unlock()
	netSubstrates[name] = f
}

// NOSSwitches are the switches a real NOS serves: those a fabric with a real nos generated, and
// those with a port cabled in such a fabric (a rack's management switch on a real-NOS OOB
// fabric). Their `vm` fidelity is the NOS's (D21), not a QEMU machine's.
func NOSSwitches(m *model.Model) map[*model.Device]string {
	out := map[*model.Device]string{}
	for _, f := range m.Fabrics {
		if f.NOS == "none" {
			continue
		}
		for _, d := range f.Devices {
			out[d] = f.NOS
		}
	}
	for _, l := range m.Links {
		f := m.Fabrics[l.Fabric]
		if f == nil || f.NOS == "none" {
			continue
		}
		for _, p := range []*model.Port{l.A, l.B} {
			if p.Device.BaseClass == "switch" {
				out[p.Device] = f.NOS
			}
		}
	}
	return out
}

// Options for Up.
type Options struct {
	StateDir    string
	AllowTCG    bool
	Adapters    []string // empty: every registered adapter
	GatewayAddr string
	Log         *slog.Logger
	Outputs     map[string]string
}

// Twin is a running estate.
type Twin struct {
	Estate   *schema.Estate
	File     string
	Engine   *engine.Engine
	Reg      *pack.Registry
	Net      *netsim.Net
	Adapters []adapter.Adapter
	Lock     *pack.Lock
	Warnings []string
	Runtime  *adapter.Runtime
	cancel   context.CancelFunc
}

// Up compiles an estate and starts it.
func Up(estatePath string, opt Options) (*Twin, error) {
	reg, err := compile.Registry()
	if err != nil {
		return nil, err
	}
	est, _, err := compile.LoadEstate(estatePath)
	if err != nil {
		return nil, err
	}
	return UpEstate(est, estatePath, reg, opt)
}

// UpEstate starts an already-loaded estate.
func UpEstate(est *schema.Estate, file string, reg *pack.Registry, opt Options) (*Twin, error) {
	if opt.Log == nil {
		opt.Log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: compile.HostArch(), AllowTCG: opt.AllowTCG})
	if err != nil {
		return nil, err
	}
	names := opt.Adapters
	if len(names) == 0 {
		regMu.Lock()
		for k := range factories {
			names = append(names, k)
		}
		regMu.Unlock()
		sort.Strings(names)
	}
	var insts []adapter.Adapter
	for _, name := range names {
		regMu.Lock()
		f := factories[name]
		regMu.Unlock()
		if f == nil {
			return nil, fmt.Errorf("no adapter %q", name)
		}
		insts = append(insts, f())
	}
	nosSwitches := NOSSwitches(res.Model)
	for _, d := range res.Model.Devices {
		if _, ok := nosSwitches[d]; ok && d.Fidelity == "vm" {
			continue // its NOS runs it, on the network substrate (below)
		}
		if d.Fidelity == "vm" {
			// a vm device runs only where an enabled substrate adapter accepts it (09)
			var why error = fmt.Errorf("the vm adapter is not enabled (--adapters)")
			for _, a := range insts {
				if s, ok := a.(adapter.Substrate); ok {
					if why = s.Accepts(est, d, opt.AllowTCG); why == nil {
						break
					}
				}
			}
			if why != nil {
				return nil, fmt.Errorf("%s is at vm fidelity and cannot run here: %v; run it at model fidelity, or where QEMU can run its guest (09)", d.Path, why)
			}
		}
		if d.Fidelity == "hardware" {
			return nil, fmt.Errorf("%s is at hardware fidelity: no hardware bridge is configured on this host (07)", d.Path)
		}
	}
	substrate := est.Spec.Adapters.Network.Substrate
	switch substrate {
	case "", "loopback":
		substrate = ""
	case "tap":
	default:
		return nil, fmt.Errorf("spec.adapters.network.substrate %q: loopback or tap", substrate)
	}
	for _, f := range res.Model.Fabrics {
		if f.NOS != "none" && substrate != "tap" {
			// a real NOS's ports are veths the tap substrate wires into its namespace (D20); the
			// loopback substrate has no link layer to give it (D34)
			return nil, fmt.Errorf("fabric %s runs nos %s: a real NOS needs spec.adapters.network.substrate: tap (a Linux host with a NOS runtime, D20); on the loopback substrate use nos: none (model switches)", f.Name, f.NOS)
		}
	}
	if opt.StateDir == "" {
		opt.StateDir = filepath.Join(os.TempDir(), "dc-twin", est.Metadata.Name)
	}
	if err := os.MkdirAll(opt.StateDir, 0o700); err != nil {
		return nil, err
	}
	warnings := append([]string{}, res.Warnings...)
	if file != "" {
		lockPath := filepath.Join(opt.StateDir, filepath.Base(file)+".lock")
		if prev, err := os.ReadFile(lockPath); err == nil {
			var old pack.Lock
			if schema.Decode(prev, &old) == nil {
				have := map[string]pack.LockEntry{}
				for _, l := range res.Lock.Packs {
					have[l.Name] = l
				}
				for _, l := range old.Packs {
					cur, ok := have[l.Name]
					switch {
					case !ok || cur.Version != l.Version:
						return nil, fmt.Errorf("the lockfile %s pins %s@%s; the estate now resolves %s@%s — remove the lock to accept the change (13 — Names, pins and the lockfile)", lockPath, l.Name, l.Version, l.Name, cur.Version)
					case cur.Digest != l.Digest:
						warnings = append(warnings, fmt.Sprintf("pack %s@%s changed content under the same version (lock %s, now %s)", l.Name, l.Version, l.Digest[:19], cur.Digest[:19]))
					}
				}
			}
		}
		_ = os.WriteFile(lockPath, res.Lock.Marshal(), 0o644)
	}
	eng, err := engine.New(res.Model, reg, filepath.Join(opt.StateDir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	n, err := netsim.New(opt.StateDir, est.Spec.Adapters.Network.BasePort)
	if err != nil {
		return nil, err
	}
	if err := n.StartGateway(opt.GatewayAddr); err != nil {
		return nil, err
	}
	outputs := map[string]string{}
	for _, s := range est.Spec.Stacks {
		for k, v := range s.Outputs {
			outputs[k] = v
		}
	}
	for k, v := range opt.Outputs {
		outputs[k] = v
	}
	rt := &adapter.Runtime{Engine: eng, Reg: reg, Net: n, StateDir: opt.StateDir, Assets: AssetsDir(), Log: opt.Log, Outputs: outputs, AllowTCG: opt.AllowTCG}
	t := &Twin{Estate: est, File: file, Engine: eng, Reg: reg, Net: n, Lock: res.Lock, Warnings: warnings, Runtime: rt}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	if substrate != "" {
		regMu.Lock()
		build := netSubstrates[substrate]
		regMu.Unlock()
		if build == nil {
			t.Down()
			return nil, fmt.Errorf("spec.adapters.network.substrate %s: not available on %s (the TAP substrate is Linux only, D20, and is linked in by importing package tap); use the loopback substrate here", substrate, runtime.GOOS)
		}
		l2, err := build(ctx, NetworkArgs{Estate: est, Engine: eng, StateDir: opt.StateDir, Outputs: outputs, Log: opt.Log})
		if err != nil {
			t.Down()
			return nil, fmt.Errorf("the %s substrate: %w", substrate, err)
		}
		n.L2 = l2
	}
	for i, a := range insts {
		name := names[i]
		if err := a.Start(ctx, rt); err != nil {
			t.Down()
			return nil, fmt.Errorf("adapter %s: %w", name, err)
		}
		t.Adapters = append(t.Adapters, a)
	}
	eng.Start()
	_ = n.Write()
	_ = writeStackInputs(t)
	return t, nil
}

// NVOSPorts are an NVLink switch tray's NVOS management ports: its management-role Ethernet
// ports other than the BMC's (mgmt0 on nvidia/nvlink-switch-tray). NVOS, the tray's own OS, runs
// on the tray's CPU and is reached on them; the BMC, on standby power, is reached on bmc0. The
// rule is shared by the three places that need it: the dhcp adapter runs NVOS's DHCP client on
// these ports, the nvue adapter serves NVUE at their lease, and the stack inputs hand their MACs
// to a stack (D46).
func NVOSPorts(d *model.Device) []*model.Port {
	if d.BaseClass != "switch-tray" {
		return nil
	}
	bmc := compile.BMCPort(d)
	var out []*model.Port
	for _, p := range d.Ports {
		if p.Role == "management" && p.Kind == "ethernet" && p.Component == nil && p != bmc && p.MAC != "" {
			out = append(out, p)
		}
	}
	return out
}

// ExpectedMachine is one entry of stack-inputs/expected-machines.json (D38): what a site's
// inventory lists for a device with a BMC. NVOSMACs is an NVLink switch tray's NVOS management
// MACs (D46), absent on every other device: NICo will not manage a switch without them
// (`No NVOS MAC addresses on expected switch`), and finds the switch's NVOS interface among the
// MACs that DHCP by them.
type ExpectedMachine struct {
	BMCMAC   string   `json:"bmcMAC"`
	Serial   string   `json:"serial"`
	User     string   `json:"user"`
	Password string   `json:"password"`
	Rack     string   `json:"rack"`
	Path     string   `json:"path"`
	NVOSMACs []string `json:"nvosMACs,omitempty"`
}

// ExpectedMachines lists a model's expected machines, in device order. It reads compiled
// identity only (factory credentials, not rotated ones), as a site's inventory holds it.
func ExpectedMachines(m *model.Model) []ExpectedMachine {
	var list []ExpectedMachine
	for _, d := range m.Devices {
		if !d.HasBMC() {
			continue
		}
		rack := ""
		if r := d.RackOf(); r != nil {
			rack = r.Name
		}
		e := ExpectedMachine{BMCMAC: d.Identity.BMCMAC, Serial: d.Identity.Serial, User: d.Identity.BMCUser,
			Password: d.Identity.BMCPassword, Rack: rack, Path: d.Path}
		for _, p := range NVOSPorts(d) {
			e.NVOSMACs = append(e.NVOSMACs, p.MAC)
		}
		list = append(list, e)
	}
	return list
}

// writeStackInputs hands an installed stack what a site hands its lifecycle manager before
// discovery: the expected machines (BMC MAC, serial, factory credentials, rack, and a switch
// tray's NVOS MACs), as NICo's expected-machines and expected-switches imports take them.
// Nothing else of the twin's state is given.
func writeStackInputs(t *Twin) error {
	if len(t.Estate.Spec.Stacks) == 0 {
		return nil
	}
	var list []ExpectedMachine
	t.Engine.Read(func(m *model.Model) { list = ExpectedMachines(m) })
	dir := filepath.Join(t.Runtime.StateDir, "stack-inputs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	return os.WriteFile(filepath.Join(dir, "expected-machines.json"), b, 0o600)
}

// Down stops adapters, the engine and the substrate.
func (t *Twin) Down() {
	if t.cancel != nil {
		t.cancel()
	}
	for i := len(t.Adapters) - 1; i >= 0; i-- {
		_ = t.Adapters[i].Stop()
	}
	if t.Engine != nil {
		t.Engine.Stop()
	}
	if t.Net != nil {
		t.Net.Close()
	}
}

// Adapter returns a running adapter by name.
func (t *Twin) Adapter(name string) adapter.Adapter {
	for _, a := range t.Adapters {
		if a.Name() == name {
			return a
		}
	}
	return nil
}

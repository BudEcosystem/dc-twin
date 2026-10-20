// Package ufm is the UFM REST adapter (docs/spec/05-interfaces.md — Fabric managers): for every
// InfiniBand fabric whose manager is nvidia/ufm it serves the UFM Enterprise REST API on the
// fabric's UFM appliance, projected from the model's InfiniBand ports and links.
//
// Its scope is the call list of the stack under test (D25). For NICo that is the
// `crates/ib-fabric` UFM client: the version, the subnet manager configuration, the ports of
// Computer systems, and partition keys with their GUID membership and QoS, under `/ufmRest` with
// HTTP Basic credentials and under `/ufmRestV3` with an access token presented as
// `Authorization: Basic <token>`. The token is minted the way UFM mints one,
// `POST /ufmRest/app/tokens` with Basic credentials, and the twin also issues one at start so a
// stack can be configured before it runs.
//
// Port state is never stored here: every response asks the engine (engine.PortUp) so a pulled
// cable, a powered-off host or a port fault shows as it happens. Partition keys, tokens and the
// LID table are this adapter's own state (they do not exist in the model) and persist in the
// state directory, as a UFM's database would. When the ibsim adapter runs the fabric and a
// stack's subnet manager has assigned LIDs there, the LIDs served are the SM's instead, read
// from ibsim (package smstate, D47), as a UFM serves what its embedded OpenSM assigned.
package ufm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/identity"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Protocol is the protocol name requests are logged under.
const Protocol = "ufm-rest"

// Manager is the fabric manager this adapter serves.
const Manager = "nvidia/ufm"

// CredentialsFile is where the adapter writes the credentials it issued, in the state
// directory, for the CLI and for harnesses in another process (mode 0600).
const CredentialsFile = "ufm-credentials.json"

// DefaultVersion is the UFM release the adapter reports. NICo reads some partition fields only
// from UFM 6.19 on (the default partition's membership), so the twin reports a 6.19 release.
const DefaultVersion = "6.19.0"

// Config is what can be configured about the served UFMs. Empty fields take derived values.
// Every field can also be set from the installed stack's outputs: ufm.username, ufm.password,
// ufm.token, ufm.version and ufm.smInsecure ("true").
type Config struct {
	// User and Password are the UFM user /ufmRest accepts; empty: "admin" and a password
	// derived from the estate seed and the appliance's path.
	User, Password string
	// Token is the access token the twin issues at start; empty: derived like Password.
	Token string
	// Version is the UFM release reported by /app/ufm_version.
	Version string
	// InsecureSM reports the subnet manager configuration UFM ships with (m_key 0, sm_key and
	// sa_key 1, no per-port m_key), which NICo flags as insecure. Off: derived, secure keys.
	InsecureSM bool
}

// Credential is what a stack needs to reach one served UFM.
type Credential struct {
	Fabric   string `json:"fabric"`
	Device   string `json:"device"`
	URL      string `json:"url"`               // the loopback endpoint on this host
	SiteURL  string `json:"siteUrl,omitempty"` // the appliance's site address, when it has one
	User     string `json:"user"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

// Adapter serves UFM REST for every UFM-managed fabric.
type Adapter struct {
	cfg     Config
	rt      *adapter.Runtime
	mu      sync.Mutex
	servers []*server
	lines   []string
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func init() {
	twin.Register("ufm", func() adapter.Adapter { return New(Config{}) })
}

// New returns an adapter with a configuration; fields left empty are filled from the stack's
// outputs at Start, then derived.
func New(cfg Config) *Adapter { return &Adapter{cfg: cfg} }

func (a *Adapter) Name() string { return "ufm" }

// Start finds every fabric managed by nvidia/ufm, its appliance, and serves UFM REST there.
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt = rt
	a.applyOutputs(rt.Outputs)
	if a.cfg.Version == "" {
		a.cfg.Version = DefaultVersion
	}
	type target struct {
		fabric string
		dev    *model.Device
	}
	var targets []target
	var seed int64
	rt.Engine.Read(func(m *model.Model) {
		seed = m.Seed
		names := make([]string, 0, len(m.Fabrics))
		for n := range m.Fabrics {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			f := m.Fabrics[n]
			if f.Manager != Manager {
				continue
			}
			if f.Technology != "infiniband" {
				a.lines = append(a.lines, fmt.Sprintf("%s: manager %s on a %s fabric; UFM manages InfiniBand only, not served", n, Manager, f.Technology))
				continue
			}
			d := appliance(m, f)
			if d == nil {
				a.lines = append(a.lines, fmt.Sprintf("%s: managed by %s but no UFM appliance is cabled to it; not served", n, Manager))
				continue
			}
			targets = append(targets, target{fabric: n, dev: d})
		}
	})
	ctx, a.cancel = context.WithCancel(ctx)
	ids := identity.New(seed)
	used := map[*model.Device]int{}
	for _, t := range targets {
		kind := "ufm"
		if used[t.dev] > 0 {
			kind = "ufm-" + t.fabric // one appliance managing two subnets: a second endpoint
		}
		used[t.dev]++
		s, err := a.newServer(t.fabric, t.dev, kind, ids)
		if err != nil {
			a.Stop()
			return err
		}
		a.servers = append(a.servers, s)
		a.wg.Add(1)
		go func() { defer a.wg.Done(); s.watchLeases(ctx) }()
	}
	if err := a.writeCredentials(); err != nil {
		rt.Log.Warn("ufm: credentials file not written", "err", err)
	}
	return nil
}

func (a *Adapter) applyOutputs(out map[string]string) {
	set := func(dst *string, key string) {
		if *dst == "" && out[key] != "" {
			*dst = out[key]
		}
	}
	set(&a.cfg.User, "ufm.username")
	set(&a.cfg.Password, "ufm.password")
	set(&a.cfg.Token, "ufm.token")
	set(&a.cfg.Version, "ufm.version")
	if strings.EqualFold(out["ufm.smInsecure"], "true") {
		a.cfg.InsecureSM = true
	}
}

// appliance finds the UFM appliance of a fabric: the appliance whose InfiniBand port is cabled
// into the fabric (the rail generator cables its ib0 to the first rail leaf).
func appliance(m *model.Model, f *model.Fabric) *model.Device {
	for _, d := range m.Devices {
		if d.BaseClass != "appliance" {
			continue
		}
		for _, p := range d.Ports {
			if p.Kind == "infiniband" && p.Link != nil && p.Link.Fabric == f.Name {
				return d
			}
		}
	}
	return nil
}

// Stop closes every endpoint and persists state.
func (a *Adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	for _, s := range a.servers {
		s.close()
	}
	a.wg.Wait()
	return nil
}

// Credentials returns what a stack needs to reach each served UFM: its endpoint, the user and
// password /ufmRest accepts, and the token the twin issued for /ufmRestV3.
func (a *Adapter) Credentials() []Credential {
	var out []Credential
	for _, s := range a.servers {
		out = append(out, s.credential())
	}
	return out
}

func (a *Adapter) writeCredentials() error {
	if a.rt.StateDir == "" {
		return nil
	}
	b, _ := json.MarshalIndent(a.Credentials(), "", "  ")
	return os.WriteFile(filepath.Join(a.rt.StateDir, CredentialsFile), b, 0o600)
}

// ReadCredentials reads the credentials a running twin's UFM adapter wrote to its state
// directory.
func ReadCredentials(stateDir string) ([]Credential, error) {
	b, err := os.ReadFile(filepath.Join(stateDir, CredentialsFile))
	if err != nil {
		return nil, err
	}
	var out []Credential
	return out, json.Unmarshal(b, &out)
}

// Report is the adapter's fidelity-report contribution.
func (a *Adapter) Report() adapter.Report {
	r := adapter.Report{Adapter: "ufm", Served: len(a.servers)}
	for _, s := range a.servers {
		r.Lines = append(r.Lines, s.reportLine())
	}
	r.Lines = append(r.Lines, a.lines...)
	r.IsNot = []string{
		"No subnet manager runs here: no fabric sweep, no SM routing and no packet path. Port state is the model's (engine.PortUp of both cable ends), ports go straight from Down to Active with no Initialize or Armed. LIDs are assigned by the twin from the topology and kept like OpenSM's guid2lid, unless the ibsim adapter runs the fabric and a stack's subnet manager has assigned LIDs in it: then they are the SM's, read from ibsim (D47), and port state is still the model's.",
		"The SM configuration (/app/smconf) is static data: derived secure keys, or UFM's shipped defaults with ufm.smInsecure.",
		"Partition keys, their GUID membership and QoS are this adapter's state; they enforce nothing, because InfiniBand has no data path in the twin.",
		"Switch ports carry the model's per-port GUIDs; on real InfiniBand a switch's ports share its node GUID and differ by port number.",
		"Ports and systems carry UFM's documented field names with values from the model, or N/A; the secondary fields (capabilities, enabled and supported speeds, tier, mirror) are not checked against a UFM capture.",
		"Not served: session authentication (/ufmRestV2), client-certificate authentication, telemetry, monitoring, alarms, events, jobs, cable info, high-BER filters, paging and the plugin (fast_api) PKey API.",
	}
	return r
}

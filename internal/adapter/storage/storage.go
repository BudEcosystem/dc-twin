// Package storage serves the estate's shared storage systems (docs/reviews/2026-11-08-storage-deep-dive.md
// P4): each system's management API, SNIA Swordfish over the Redfish protocol (S-Q3), at its
// management address, and its Prometheus exporter; at fidelity vm it also drives the system's
// data path, real Linux targets on its nodes (datapath.go, S-Q2). What a system holds is the
// engine's (S-Q1): this adapter projects it and turns requests into engine requests.
package storage

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/adapter/dhcp"
	"github.com/BudEcosystem/dc-twin/internal/identity"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// Name is the adapter's registered name.
const Name = "storage"

// CredentialsFile is where the adapter writes each system's API and S3 credentials in the state
// directory (`dctwin storage credentials`).
const CredentialsFile = "storage-credentials.json"

// Site ports a system's services answer at on its management and data addresses.
const (
	APIPort     = 443
	MetricsPort = 9580
	NFSPort     = 2049
	NVMePort    = 4420
	S3Port      = 8333
)

func init() { twin.Register(Name, func() adapter.Adapter { return New() }) }

// Adapter serves every storage system of the estate.
type Adapter struct {
	rt      *adapter.Runtime
	cancel  context.CancelFunc
	servers []*server
	lines   []string
	wg      sync.WaitGroup
	snap    *snap // the NVMe emulation stand-in (snap.go)
}

// New returns an adapter that has not started.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return Name }

// Credential is what a client needs to reach one system.
type Credential struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	API        string `json:"api"`     // https://host:port of the Swordfish service on this machine
	Metrics    string `json:"metrics"` // http://host:port/metrics
	User       string `json:"user"`
	Password   string `json:"password"`
	Management string `json:"management,omitempty"` // its site address
	Data       string `json:"data,omitempty"`
	// S3 keys, and at vm where each protocol is served on this machine
	AccessKey string            `json:"accessKey,omitempty"`
	SecretKey string            `json:"secretKey,omitempty"`
	Endpoints map[string]string `json:"endpoints,omitempty"`
}

type server struct {
	a        *Adapter
	path     string
	name     string
	manager  string // the management API served: generic/swordfish or vast/vms
	index    int
	user     string
	password string
	access   string
	secret   string
	mgmtIP   string
	dataIP   string
	apiAddr  string
	metrics  string
	srv      *http.Server
	msrv     *http.Server
	cert     *tls.Certificate

	mu       sync.Mutex
	sessions map[string]string // token -> session id
	sessSeq  int
	// streams are VMS's global snapshot streams (vms.go): a clone's progress, kept by the dialect
	// since every one is finished when it is made
	streams   map[int]map[string]any
	streamSeq int
	expiring  map[string]bool // snapshots made with an expiration time, by id
	dp        *datapath
}

// Start serves each system.
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt = rt
	ctx, a.cancel = context.WithCancel(ctx)
	var systems []*model.StorageSystem
	var seed int64
	rt.Engine.Read(func(m *model.Model) {
		seed = m.Seed
		systems = append(systems, m.StorageSystems...)
	})
	ids := identity.New(seed)
	for i, sys := range systems {
		s, err := a.newServer(ctx, sys, i, ids)
		if err != nil {
			a.Stop()
			return err
		}
		a.servers = append(a.servers, s)
	}
	if err := a.writeCredentials(); err != nil {
		rt.Log.Warn("storage: credentials file not written", "err", err)
	}
	a.snap = newSnap(a)
	a.wg.Add(1)
	go func() { defer a.wg.Done(); a.runSnap(ctx) }()
	return nil
}

func secret(ids *identity.Deriver, kind, path string, n int) string {
	const alnum = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	var b strings.Builder
	for _, c := range ids.Material(kind, path, "1", 0, n) {
		b.WriteByte(alnum[int(c)%len(alnum)])
	}
	return b.String()
}

func (a *Adapter) newServer(ctx context.Context, sys *model.StorageSystem, i int, ids *identity.Deriver) (*server, error) {
	s := &server{a: a, path: sys.Path, name: sys.Name, manager: sys.Manager, index: i, user: "admin", sessions: map[string]string{},
		password: secret(ids, "storage-password", sys.Path, 16), access: "DCT" + strings.ToUpper(secret(ids, "storage-s3-access", sys.Path, 17)),
		secret: secret(ids, "storage-s3-secret", sys.Path, 40)}
	var m *model.Model
	a.rt.Engine.Read(func(mm *model.Model) { m = mm })
	s.mgmtIP = systemAddress(m, sys.ManagementNetwork, sys.Nodes[0], i)
	s.dataIP = systemAddress(m, sys.DataNetwork, sys.Nodes[0], i)
	if s.mgmtIP != "" {
		a.rt.Engine.SetStorageAddress(sys.Path, "management", s.mgmtIP)
	}
	if s.dataIP != "" {
		a.rt.Engine.SetStorageAddress(sys.Path, "data", s.dataIP)
	}
	var ips []string
	if s.mgmtIP != "" {
		ips = append(ips, s.mgmtIP)
	}
	cert, err := a.rt.Net.Certificate(sys.Name, []string{sys.Name, "localhost"}, ips)
	if err != nil {
		return nil, err
	}
	s.cert = &cert
	ln, err := a.rt.Net.Listen(sys.Path, "swordfish")
	if err != nil {
		return nil, err
	}
	s.apiAddr = ln.Addr().String()
	a.rt.Engine.SetStorageEndpoint(sys.Path, "api", s.apiAddr)
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(discard{}, "", 0)}
	go s.srv.Serve(tls.NewListener(ln, &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}))
	ml, err := a.rt.Net.Listen(sys.Path, "storage-metrics")
	if err != nil {
		return nil, err
	}
	s.metrics = ml.Addr().String()
	a.rt.Engine.SetStorageEndpoint(sys.Path, "metrics", s.metrics)
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.serveMetrics)
	s.msrv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(discard{}, "", 0)}
	go s.msrv.Serve(ml)
	if s.mgmtIP != "" {
		a.rt.Net.Route(fmt.Sprintf("%s:%d", s.mgmtIP, APIPort), s.apiAddr)
		a.rt.Net.Route(fmt.Sprintf("%s:%d", s.mgmtIP, MetricsPort), s.metrics)
	}
	if sys.Fidelity == "vm" {
		s.dp = newDatapath(s, sys)
		a.wg.Add(1)
		go func() { defer a.wg.Done(); s.dp.run(ctx) }()
	}
	return s, nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// systemAddress is a system's site address on a network: in the segment of the rack of its first
// node, the twin's reserved .5 onwards (.2 to .9 of every segment are the twin's, dhcp/segments.go),
// one per system. A network without a CIDR gives none.
func systemAddress(m *model.Model, network string, first *model.Device, i int) string {
	if network == "" || first == nil || first.RackOf() == nil {
		return ""
	}
	for _, sg := range dhcp.NetworkSegments(m, network) {
		if sg.Rack != first.RackOf().Path || !sg.Prefix.IsValid() {
			continue
		}
		b := sg.Prefix.Addr().As4()
		v := binary.BigEndian.Uint32(b[:]) + uint32(5+i)
		binary.BigEndian.PutUint32(b[:], v)
		if a := netip.AddrFrom4(b); sg.Prefix.Contains(a) && i < 5 {
			return a.String()
		}
	}
	return ""
}

// Stop closes every endpoint.
func (a *Adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	for _, s := range a.servers {
		if s.srv != nil {
			s.srv.Close()
		}
		if s.msrv != nil {
			s.msrv.Close()
		}
	}
	a.wg.Wait()
	return nil
}

// Credentials are every system's.
func (a *Adapter) Credentials() []Credential {
	var out []Credential
	for _, s := range a.servers {
		c := Credential{Path: s.path, Name: s.name, API: "https://" + s.apiAddr, Metrics: "http://" + s.metrics + "/metrics",
			User: s.user, Password: s.password, Management: s.mgmtIP, Data: s.dataIP, AccessKey: s.access, SecretKey: s.secret}
		if s.dp != nil {
			c.Endpoints = s.dp.published()
		}
		out = append(out, c)
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

// ReadCredentials reads the credentials a running twin's storage adapter wrote.
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
	r := adapter.Report{Adapter: Name, Served: len(a.servers)}
	for _, s := range a.servers {
		v, err := a.rt.Engine.StorageSystem(s.path)
		if err != nil {
			continue
		}
		line := fmt.Sprintf("%s: %s at %s (Swordfish at %s, metrics at %s), %d nodes, %d enclosures, %s usable of %s raw (%d+%d), protocols %s, fidelity %s",
			s.name, v.Template, orNone(s.mgmtIP), s.apiAddr, s.metrics, len(v.Nodes), len(v.Enclosures), bytesH(v.UsableBytes), bytesH(v.RawBytes),
			v.DataUnits, v.ParityUnits, strings.Join(v.Protocols, ","), v.Fidelity)
		r.Lines = append(r.Lines, line)
		if s.dp != nil {
			r.Lines = append(r.Lines, s.dp.reportLines()...)
			r.SubstrateOwned = append(r.SubstrateOwned, fmt.Sprintf("%s: used bytes of its objects observed from its nodes", s.name))
		}
		for _, n := range v.IsNot {
			r.IsNot = append(r.IsNot, s.name+": "+n)
		}
	}
	r.Lines = append(r.Lines, a.lines...)
	if a.snap != nil {
		r.Lines = append(r.Lines, a.snap.reportLines()...)
	}
	r.IsNot = append(r.IsNot,
		"The management API is SNIA Swordfish's resources over Redfish (Storage, StoragePool, Volume, FileSystem, FileShare, NVMe-oF Fabric, Endpoint and Connection, Chassis and Drive); not served: classes of service, consistency groups, replication, events and tasks (every write completes at once).",
		"S3 buckets are made through the twin's control API (`dctwin storage create bucket`): Swordfish has no bucket resource.")
	sort.Strings(r.IsNot)
	return r
}

func orNone(s string) string {
	if s == "" {
		return "no site address"
	}
	return s
}

func bytesH(n int64) string {
	const tb = 1e12
	if n >= 1e15 {
		return fmt.Sprintf("%.2f PB", float64(n)/1e15)
	}
	return fmt.Sprintf("%.2f TB", float64(n)/tb)
}

// listen is a helper for the data path's published endpoints.
func (s *server) listen(kind string) (net.Listener, error) { return s.a.rt.Net.Listen(s.path, kind) }

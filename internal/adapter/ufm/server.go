package ufm

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/identity"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// server is one served UFM: one fabric, on one appliance.
type server struct {
	a        *Adapter
	fabric   string
	dev      *model.Device
	kind     string
	addr     string
	user     string
	password string
	token    string // the twin-issued token
	sm       smConfig
	srv      *http.Server
	ln       net.Listener

	mu        sync.Mutex
	smLIDs    bool // the last ports response carried the subnet manager's LIDs (lidTable)
	st        *persisted
	statePath string
	siteIP    string
	certs     map[string]*tls.Certificate
}

// smConfig is /app/smconf, as NICo's SmConfig deserializes it.
type smConfig struct {
	SubnetPrefix string `json:"subnet_prefix"`
	MKey         string `json:"m_key"`
	SMKey        string `json:"sm_key"`
	SAKey        string `json:"sa_key"`
	MKeyPerPort  bool   `json:"m_key_per_port"`
}

const alnum = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// derived returns n alphanumerics derived from the estate seed for this appliance.
func derived(ids *identity.Deriver, kind string, d *model.Device, n int) string {
	m := ids.Material(kind, d.Path, d.Identity.ProfileMajor, 0, n)
	var b strings.Builder
	for _, c := range m {
		b.WriteByte(alnum[int(c)%len(alnum)])
	}
	return b.String()
}

func derivedHex(ids *identity.Deriver, kind string, d *model.Device) string {
	return fmt.Sprintf("0x%x", ids.Material(kind, d.Path, d.Identity.ProfileMajor, 0, 8))
}

func (a *Adapter) newServer(fabric string, d *model.Device, kind string, ids *identity.Deriver) (*server, error) {
	s := &server{a: a, fabric: fabric, dev: d, kind: kind, certs: map[string]*tls.Certificate{},
		user: a.cfg.User, password: a.cfg.Password, token: a.cfg.Token}
	if a.rt.StateDir != "" {
		s.statePath = filepath.Join(a.rt.StateDir, "ufm-"+fabric+".json")
	}
	if s.user == "" {
		s.user = "admin"
	}
	if s.password == "" {
		s.password = derived(ids, "ufm-password", d, 16)
	}
	if s.token == "" {
		s.token = derived(ids, "ufm-token", d, 30)
	}
	if a.cfg.InsecureSM {
		s.sm = smConfig{SubnetPrefix: "0xfe80000000000000", MKey: "0x0000000000000000", SMKey: "0x0000000000000001", SAKey: "0x0000000000000001"}
	} else {
		s.sm = smConfig{SubnetPrefix: "0xfe80000000000000", MKey: derivedHex(ids, "ufm-m-key", d),
			SMKey: derivedHex(ids, "ufm-sm-key", d), SAKey: derivedHex(ids, "ufm-sa-key", d), MKeyPerPort: true}
	}
	if err := s.load(); err != nil {
		return nil, fmt.Errorf("ufm %s: %w", fabric, err)
	}
	s.mu.Lock()
	s.ensureToken(s.token, s.user, true)
	_ = s.saveLocked()
	s.mu.Unlock()

	ln, err := a.rt.Net.Listen(d.Path, kind)
	if err != nil {
		return nil, err
	}
	s.addr = ln.Addr().String()
	a.rt.Engine.SetEndpoint(d.Path, kind, s.addr)
	a.rt.Engine.Read(func(*model.Model) { s.siteIP = d.Endpoint.SiteIP })
	if s.siteIP != "" {
		a.rt.Net.Route(s.siteIP+":443", s.addr)
	}
	s.ln = tls.NewListener(ln, &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: s.certificate})
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(discard{}, "", 0)}
	go s.srv.Serve(s.ln)
	return s, nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// certificate issues (once per site address) a certificate from the estate CA naming the
// appliance and its site address.
func (s *server) certificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	ip := s.siteIP
	c := s.certs[ip]
	s.mu.Unlock()
	if c != nil {
		return c, nil
	}
	var ips []string
	if ip != "" {
		ips = []string{ip}
	}
	cert, err := s.a.rt.Net.Certificate(s.dev.Name, []string{s.dev.Name, "ufm", "localhost"}, ips)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.certs[ip] = &cert
	s.mu.Unlock()
	return &cert, nil
}

// watchLeases follows the appliance's site address: when the stack's DHCP (or the twin's)
// leases it an address, port 443 there reaches this UFM through the site gateway.
func (s *server) watchLeases(ctx context.Context) {
	e := s.a.rt.Engine
	for ctx.Err() == nil {
		id, ch := e.Subscribe(256)
		// a lease committed between start and subscribing is picked up here
		var ip string
		e.Read(func(*model.Model) { ip = s.dev.Endpoint.SiteIP })
		s.routeSite(ip)
	loop:
		for {
			select {
			case <-ctx.Done():
				e.Unsubscribe(id)
				return
			case ev, ok := <-ch:
				if !ok {
					break loop // dropped for falling behind: resubscribe
				}
				if ev.Path == s.dev.Path && ev.State != nil && ev.State.Field == "lease" {
					s.routeSite(fmt.Sprint(ev.State.To))
				}
			}
		}
	}
}

func (s *server) routeSite(ip string) {
	s.mu.Lock()
	old := s.siteIP
	s.siteIP = ip
	s.mu.Unlock()
	if ip == old {
		return
	}
	if old != "" {
		s.a.rt.Net.Unroute(old + ":443")
	}
	if ip != "" {
		s.a.rt.Net.Route(ip+":443", s.addr)
	}
}

func (s *server) credential() Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := Credential{Fabric: s.fabric, Device: s.dev.Path, URL: "https://" + s.addr, User: s.user, Password: s.password, Token: s.token}
	if s.siteIP != "" {
		c.SiteURL = "https://" + s.siteIP
	}
	return c
}

func (s *server) reportLine() string {
	s.mu.Lock()
	parts := len(s.st.Partitions)
	lids := "LIDs assigned by the twin"
	if s.smLIDs {
		lids = "LIDs the subnet manager assigned, read from ibsim"
	}
	s.mu.Unlock()
	n := len(s.snapshot().ports)
	return fmt.Sprintf("%s: UFM REST %s on %s at https://%s (/ufmRest Basic, /ufmRestV3 token): %d InfiniBand ports, %d partition keys, %s",
		s.fabric, s.a.cfg.Version, s.dev.Path, s.addr, n, parts, lids)
}

func (s *server) close() {
	if s.srv != nil {
		_ = s.srv.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.siteIP != "" {
		s.a.rt.Net.Unroute(s.siteIP + ":443")
	}
	_ = s.saveLocked()
}

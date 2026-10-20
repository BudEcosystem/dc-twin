package bluefield

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// front is one BlueField BMC as a stack meets it: its TLS identity, the state a BlueField BMC
// keeps itself, and a forwarding hop to the redfish engine's service for everything else.
type front struct {
	a        *Adapter
	dev      *model.Device
	spec     *schema.RedfishDialect
	manager  string // /redfish/v1/Managers/Bluefield_BMC
	system   string // /redfish/v1/Systems/Bluefield
	addr     string
	mu       sync.Mutex
	upstream string
	proxy    *httputil.ReverseProxy

	// TLS identity: the factory certificate (estate CA, naming the BMC's host name and lease),
	// or the one a stack installed with ReplaceCertificate, and a CSR's pending key.
	factory     *tls.Certificate
	factoryFor  string
	installed   *tls.Certificate
	installedPM string
	pendingKey  *ecdsa.PrivateKey
	trust       []trustCert
	trustSeq    int
	tlsAuth     bool // AccountService Oem.OpenBMC.AuthMethods.TLS
	tlsAuthInit bool
	// pwChange is set by ResetToDefaults: the factory account answers 403 PasswordChangeRequired
	// until its password is changed, as a BlueField BMC does after a factory reset.
	pwChange bool

	// BlueField OEM state written through the BMC (nil: what the capture says).
	hostRshim   *string
	bmcRshim    *bool
	mode        *string
	pendingMode string

	// update tasks this BMC runs itself (the BFB push), and whether one is running.
	tasks   []*task
	taskSeq int
	busy    bool
	// bfb is what the last pushed boot stream carried; armInBFB is set once the Arm has been
	// reset into a pushed BFB (the captured OsIsRunning no longer stands for anything).
	bfb      *bfbFacts
	armInBFB bool
	// arm is the DPU's Arm when the BlueField is at vm fidelity (arm.go): a pushed boot stream
	// is saved for it, and the reset that follows boots the stream's installer.
	arm *arm
}

func newFront(a *Adapter, d *model.Device, spec *schema.RedfishDialect) (*front, error) {
	f := &front{a: a, dev: d, spec: spec, manager: spec.Spec.Manager, system: spec.Spec.System, taskSeq: 999}
	if f.manager == "" || f.system == "" {
		return nil, fmt.Errorf("bluefield: dialect %s names no manager or system", spec.Metadata.Name)
	}
	// The capture's AccountService says whether TLS client authentication is on at the factory
	// (it is on the captured BlueField-3 BMC); without the capture, bmcweb's default (on).
	f.tlsAuth, f.tlsAuthInit = true, true
	if dir, err := twin.AssetPath(a.rt.Reg, spec.Spec.Mockup); err == nil {
		if b, err := os.ReadFile(filepath.Join(dir, "redfish", "v1", "AccountService", "index.json")); err == nil {
			var doc map[string]any
			if json.Unmarshal(b, &doc) == nil {
				if v, ok := dig(doc, "Oem", "OpenBMC", "AuthMethods", "TLS").(bool); ok {
					f.tlsAuth, f.tlsAuthInit = v, v
				}
			}
		}
	}
	return f, nil
}

func dig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

// setUpstream records the redfish engine's service for this BMC and builds the forwarding hop.
func (f *front) setUpstream(up string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upstream == up && f.proxy != nil {
		return
	}
	f.upstream = up
	target := &url.URL{Scheme: "https", Host: up}
	rp := httputil.NewSingleHostReverseProxy(target)
	pool := f.a.estateCA
	rp.Transport = &http.Transport{
		Proxy:                 nil,
		ResponseHeaderTimeout: 60 * time.Second,
		// The hop is the twin talking to itself: the engine's certificate is the estate CA's.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true, VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error { //nolint:gosec // verified below against the estate CA
			if len(raw) == 0 {
				return fmt.Errorf("no certificate from the redfish engine")
			}
			c, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			_, err = c.Verify(x509.VerifyOptions{Roots: pool})
			return err
		}},
	}
	rp.FlushInterval = -1 // EventService SSE streams through
	rp.ModifyResponse = f.modifyResponse
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		writeError(w, http.StatusServiceUnavailable, "Base.1.15.ServiceTemporarilyUnavailable", "The BMC service is not available: "+err.Error())
	}
	f.proxy = rp
}

// tlsConfig is the BMC's TLS identity for one handshake: the installed certificate if a stack
// replaced it, else the factory one; a client certificate is requested whenever TLS
// authentication is on, and checked against the Truststore by the handler (bmcweb verifies the
// peer when TLS auth is enabled and falls back to other methods when none is presented).
func (f *front) tlsConfig(*tls.ClientHelloInfo) (*tls.Config, error) {
	cert, err := f.serverCert()
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12}
	f.mu.Lock()
	if f.tlsAuth {
		cfg.ClientAuth = tls.RequestClientCert
	}
	f.mu.Unlock()
	return cfg, nil
}

// siteIP is the BMC's leased address, or "" before its lease.
func (f *front) siteIP() string {
	var ip string
	f.a.eng.Read(func(*model.Model) { ip = f.dev.Endpoint.SiteIP })
	return ip
}

func (f *front) serverCert() (*tls.Certificate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.installed != nil {
		return f.installed, nil
	}
	ip := f.siteIP()
	if f.factory == nil || f.factoryFor != ip {
		host := hostName(f.dev)
		var ips []string
		if ip != "" {
			ips = append(ips, ip)
		}
		c, err := f.a.rt.Net.Certificate(host, []string{host}, ips)
		if err != nil {
			return nil, err
		}
		f.factory, f.factoryFor = &c, ip
	}
	return f.factory, nil
}

// hostName is the BMC's host name as the redfish adapter names it ("<rack>-<device…>-bmc").
func hostName(d *model.Device) string {
	parts := []string{}
	for x := d; x != nil; x = x.Parent {
		parts = append([]string{x.Name}, parts...)
	}
	if r := d.RackOf(); r != nil {
		parts = append([]string{r.Name}, parts...)
	}
	return strings.Join(parts, "-") + "-bmc"
}

func (f *front) leaseMoved() {
	f.mu.Lock()
	f.factory = nil
	f.mu.Unlock()
}

// bmcRebooted drops what a BMC holds only in memory: a CSR's pending key.
func (f *front) bmcRebooted() {
	f.mu.Lock()
	had := f.pendingKey != nil
	f.pendingKey = nil
	f.mu.Unlock()
	if had {
		f.commit("certificate.csr", "pending", nil, "the BMC rebooted; the CSR's pending key pair is gone", 0)
	}
}

// ---------------------------------------------------------------- requests

// principal is who a request is from.
type principal struct {
	user string
	how  string // anonymous, basic, token, certificate, none
	// pass is the account's password, for forwarding a certificate-authenticated request to the
	// engine as that account (the engine knows basic auth and sessions, not certificates).
	pass string
	ok   bool
	// factoryDefault: authenticated as the factory account with its factory password.
	factoryDefault bool
}

func (f *front) identify(r *http.Request) principal {
	eng := f.a.eng
	if tok := r.Header.Get("X-Auth-Token"); tok != "" {
		if s, ok := eng.SessionByToken(f.dev, tok); ok {
			return principal{user: s.User, how: "token", ok: true}
		}
	}
	if u, p, ok := r.BasicAuth(); ok {
		if _, ok := eng.Authenticate(f.dev, u, p); ok {
			return principal{user: u, how: "basic", ok: true,
				factoryDefault: u == f.dev.Identity.BMCUser && p == f.dev.Identity.BMCPassword}
		}
		return principal{user: u, how: "basic"}
	}
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		if user, ok := f.verifyClient(r.TLS.PeerCertificates); ok {
			var acct model.Account
			found := false
			eng.Read(func(*model.Model) {
				for _, a := range f.dev.State.Accounts {
					if a.User == user && a.Enabled && !a.Locked {
						acct, found = a, true
					}
				}
			})
			if found {
				return principal{user: user, how: "certificate", pass: acct.Password, ok: true}
			}
		}
		return principal{how: "certificate"}
	}
	return principal{how: "none"}
}

var anonymous = map[string]bool{"/redfish": true, "/redfish/v1": true, "/redfish/v1/$metadata": true, "/redfish/v1/odata": true}

func normalise(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return "/"
	}
	return p
}

// down drops the connection when the BMC is down (rebooting), as the engine does: a BMC that is
// down does not answer.
func (f *front) down(w http.ResponseWriter) bool {
	if up, _, _ := f.a.eng.BMCBehaviour(f.dev); up {
		return false
	}
	if hj, ok := w.(http.Hijacker); ok {
		if c, _, err := hj.Hijack(); err == nil {
			c.Close()
			return true
		}
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	return true
}

func (f *front) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.down(w) {
		return
	}
	path := normalise(r.URL.Path)
	p := f.identify(r)
	f.mu.Lock()
	mustChange := f.pwChange
	proxy := f.proxy
	f.mu.Unlock()
	if mustChange && p.ok && p.how == "basic" && p.factoryDefault &&
		!(r.Method == http.MethodPatch && strings.HasPrefix(path, "/redfish/v1/AccountService/Accounts/")) &&
		!(anonymous[path] && r.Method == http.MethodGet) {
		f.log(r, p, path, "account.read", http.StatusForbidden)
		writePasswordChange(w, f.dev.Identity.BMCUser)
		return
	}
	if h := f.handler(r.Method, path); h != nil {
		if !p.ok && !(anonymous[path] && r.Method == http.MethodGet) {
			f.log(r, p, path, "other", http.StatusUnauthorized)
			w.Header().Set("WWW-Authenticate", `Basic realm="Redfish"`)
			writeError(w, http.StatusUnauthorized, "Base.1.15.NoValidSession", "There is no valid session established with the implementation.")
			return
		}
		h(w, r, path, p)
		return
	}
	if proxy == nil {
		writeError(w, http.StatusServiceUnavailable, "Base.1.15.ServiceTemporarilyUnavailable", "The BMC service is starting.")
		return
	}
	if p.ok && p.how == "certificate" {
		// mutual TLS: the engine authenticates basic auth and sessions, so the request is
		// forwarded as the account the client certificate names
		r.Header.Del("Authorization")
		r.SetBasicAuth(p.user, p.pass)
	}
	if r.Method == http.MethodPatch && path == "/redfish/v1/AccountService" {
		f.authMethodsPatch(r)
	}
	proxy.ServeHTTP(w, r)
	if r.Method == http.MethodPatch && strings.HasPrefix(path, "/redfish/v1/AccountService/Accounts/") && mustChange {
		f.passwordChanged()
	}
}

// passwordChanged clears the factory-reset password requirement once the factory account's
// password is no longer the factory one.
func (f *front) passwordChanged() {
	still := false
	f.a.eng.Read(func(*model.Model) {
		for _, a := range f.dev.State.Accounts {
			if a.User == f.dev.Identity.BMCUser && a.Password == f.dev.Identity.BMCPassword {
				still = true
			}
		}
	})
	if !still {
		f.mu.Lock()
		f.pwChange = false
		f.mu.Unlock()
	}
}

// authMethodsPatch records AccountService Oem.OpenBMC.AuthMethods.TLS from a PATCH that the
// engine will persist (and render back); the front acts on it at the next handshake.
func (f *front) authMethodsPatch(r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return
	}
	v, ok := dig(doc, "Oem", "OpenBMC", "AuthMethods", "TLS").(bool)
	if !ok {
		return
	}
	f.mu.Lock()
	old := f.tlsAuth
	f.tlsAuth = v
	f.mu.Unlock()
	if old != v {
		f.commit("account.authMethods.tls", old, v, "TLS client authentication", 0)
	}
}

// modifyResponse makes the engine's answers the BlueField BMC's: bmcweb answers a successful
// write with 200 and a Base Success message where the engine answers 204, and the resources whose
// BlueField state the front keeps are rendered with it.
func (f *front) modifyResponse(resp *http.Response) error {
	req := resp.Request
	path := normalise(req.URL.Path)
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		if resp.StatusCode == http.StatusNoContent {
			setJSONBody(resp, http.StatusOK, success())
		}
		return nil
	}
	if resp.StatusCode != http.StatusOK || req.Method != http.MethodGet || !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return nil
	}
	ov := f.overlay(path)
	if ov == nil {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
	ov(doc)
	setJSONBody(resp, http.StatusOK, doc)
	return nil
}

func setJSONBody(resp *http.Response, status int, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	resp.StatusCode = status
	resp.Status = fmt.Sprintf("%d %s", status, http.StatusText(status))
	resp.Body = io.NopCloser(bytes.NewReader(b))
	resp.ContentLength = int64(len(b))
	resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
	resp.Header.Set("Content-Type", "application/json; charset=utf-8")
}

func success() map[string]any {
	return map[string]any{"@Message.ExtendedInfo": []any{map[string]any{
		"@odata.type": "#Message.v1_1_1.Message", "Message": "The request completed successfully.",
		"MessageArgs": []any{}, "MessageId": "Base.1.15.Success", "MessageSeverity": "OK", "Resolution": "None"}}}
}

// overlay returns how the front changes a forwarded GET of path, or nil.
func (f *front) overlay(path string) func(map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch path {
	case f.system + "/Oem/Nvidia":
		hr, mode := f.hostRshim, f.mode
		return func(d map[string]any) {
			if hr != nil {
				d["HostRshim"] = *hr
			}
			if mode != nil {
				d["Mode"] = *mode
			}
		}
	case f.manager + "/Oem/Nvidia":
		br := f.bmcRshim
		return func(d map[string]any) {
			if br != nil {
				d["BmcRShim"] = map[string]any{"BmcRShimEnabled": *br}
			}
		}
	case f.system:
		inBFB := f.armInBFB
		if f.arm != nil {
			// a BlueField at vm: the Arm's own state, observed. OsIsRunning once its DPU OS has
			// reached its login prompt in this boot (DPF's hostless reboot waits for it); else
			// the property is left out rather than given a value the twin did not see
			running := f.arm.osRunning()
			return func(d map[string]any) {
				if bp, ok := d["BootProgress"].(map[string]any); ok {
					if running {
						bp["OemLastState"] = "OsIsRunning"
					} else {
						delete(bp, "OemLastState")
					}
				}
			}
		}
		return func(d map[string]any) {
			if bp, ok := d["BootProgress"].(map[string]any); ok && inBFB {
				// the Arm was reset into a pushed BFB and no installer or DPU OS runs here: the
				// captured OsIsRunning would claim an OS the twin does not run
				delete(bp, "OemLastState")
			}
		}
	case f.manager, "/redfish/v1/UpdateService/FirmwareInventory/BMC_Firmware":
		// the BMC's version is the model's (seeded from the capture, changed by an update): the
		// dialect binds no firmware inventory, so the engine renders the capture's there
		var v string
		f.a.eng.Read(func(*model.Model) {
			if fw := f.dev.State.Firmware["bmc"]; fw != nil {
				v = fw.Version
			}
		})
		prop := "FirmwareVersion"
		if path != f.manager {
			prop = "Version"
		}
		return func(d map[string]any) {
			if _, ok := d[prop]; ok && v != "" {
				d[prop] = v
			}
		}
	case "/redfish/v1/TaskService/Tasks":
		ids := make([]string, 0, len(f.tasks))
		for _, t := range f.tasks {
			ids = append(ids, t.id)
		}
		return func(d map[string]any) {
			members, _ := d["Members"].([]any)
			for _, id := range ids {
				members = append(members, map[string]any{"@odata.id": "/redfish/v1/TaskService/Tasks/" + id})
			}
			d["Members"], d["Members@odata.count"] = members, len(members)
		}
	}
	return nil
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, path string, p principal)

// handler returns the front's own handler for a request, or nil to forward it.
func (f *front) handler(method, path string) handlerFunc {
	m := f.manager
	trust := m + "/Truststore/Certificates"
	https := m + "/NetworkProtocol/HTTPS/Certificates"
	switch {
	case path == trust && method == http.MethodGet:
		return f.getTruststore
	case path == trust && method == http.MethodPost:
		return f.installTrust
	case strings.HasPrefix(path, trust+"/") && method == http.MethodGet:
		return f.getTrustCert
	case strings.HasPrefix(path, trust+"/") && method == http.MethodDelete:
		return f.deleteTrustCert
	case path == https && method == http.MethodGet:
		return f.getHTTPSCollection
	case path == https+"/1" && method == http.MethodGet:
		return f.getHTTPSCert
	case path == "/redfish/v1/CertificateService/Actions/CertificateService.GenerateCSR" && method == http.MethodPost:
		return f.generateCSR
	case path == "/redfish/v1/CertificateService/Actions/CertificateService.ReplaceCertificate" && method == http.MethodPost:
		return f.replaceCertificate
	case path == "/redfish/v1/UpdateService/Actions/UpdateService.SimpleUpdate" && method == http.MethodPost:
		return f.simpleUpdate
	case (path == "/redfish/v1/UpdateService" || path == "/redfish/v1/UpdateService/update") && method == http.MethodPost:
		return f.pushUpdate
	case path == "/redfish/v1/UpdateService/update-multipart" && method == http.MethodPost:
		return f.multipartUpdate
	case strings.HasPrefix(path, "/redfish/v1/TaskService/Tasks/") && method == http.MethodGet && f.hasTask(strings.TrimPrefix(path, "/redfish/v1/TaskService/Tasks/")):
		return f.getTask
	case path == f.system+"/Oem/Nvidia/Actions/HostRshim.Set" && method == http.MethodPost:
		return f.hostRshimSet
	case path == f.system+"/Oem/Nvidia/Actions/Mode.Set" && method == http.MethodPost:
		return f.modeSet
	case path == f.system+"/Oem/Nvidia/SOC.ForceReset" && method == http.MethodPost:
		return f.socForceReset
	case path == m+"/Oem/Nvidia" && method == http.MethodPatch:
		return f.bmcRshimPatch
	case path == m+"/Actions/Manager.ResetToDefaults" && method == http.MethodPost:
		return f.resetToDefaults
	}
	return nil
}

// ---------------------------------------------------------------- OEM

func (f *front) hostRshimSet(w http.ResponseWriter, r *http.Request, path string, p principal) {
	var body struct{ HostRshim string }
	if err := decode(r, &body); err != nil || (body.HostRshim != "Enabled" && body.HostRshim != "Disabled") {
		f.log(r, p, path, "dpu.rshim.set", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.ActionParameterValueNotInList", "HostRshim must be Enabled or Disabled.")
		return
	}
	c := f.log(r, p, path, "dpu.rshim.set", http.StatusOK)
	f.mu.Lock()
	old := "captured"
	if f.hostRshim != nil {
		old = *f.hostRshim
	}
	v := body.HostRshim
	f.hostRshim = &v
	f.mu.Unlock()
	f.commit("dpu.rshim.host", old, v, "the host's rshim access", c)
	writeJSON(w, http.StatusOK, success())
}

func (f *front) bmcRshimPatch(w http.ResponseWriter, r *http.Request, path string, p principal) {
	var body struct {
		BmcRShim *struct{ BmcRShimEnabled *bool }
	}
	if err := decode(r, &body); err != nil || body.BmcRShim == nil || body.BmcRShim.BmcRShimEnabled == nil {
		f.log(r, p, path, "dpu.rshim.set", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.PropertyMissing", "BmcRShim.BmcRShimEnabled is required.")
		return
	}
	c := f.log(r, p, path, "dpu.rshim.set", http.StatusOK)
	v := *body.BmcRShim.BmcRShimEnabled
	f.mu.Lock()
	var old any = "captured"
	if f.bmcRshim != nil {
		old = *f.bmcRshim
	}
	f.bmcRshim = &v
	f.mu.Unlock()
	f.commit("dpu.rshim.bmc", old, v, "the BMC's own rshim (the BFB push path)", c)
	writeJSON(w, http.StatusOK, success())
}

// modeSet stages a DPU/NIC mode change; a BlueField applies it at the next power cycle of the
// host it sits in (the front applies it when the DPU next powers off).
func (f *front) modeSet(w http.ResponseWriter, r *http.Request, path string, p principal) {
	var body struct{ Mode string }
	if err := decode(r, &body); err != nil || (body.Mode != "DpuMode" && body.Mode != "NicMode") {
		f.log(r, p, path, "dpu.mode.set", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.ActionParameterValueNotInList", "Mode must be DpuMode or NicMode.")
		return
	}
	c := f.log(r, p, path, "dpu.mode.set", http.StatusOK)
	f.mu.Lock()
	f.pendingMode = body.Mode
	f.mu.Unlock()
	f.commit("dpu.mode.pending", nil, body.Mode, "applied at the next power cycle", c)
	writeJSON(w, http.StatusOK, success())
}

// applyPendingMode is called when the DPU powers off.
func (f *front) applyPendingMode() {
	f.mu.Lock()
	m := f.pendingMode
	f.pendingMode = ""
	var old any = "captured"
	if f.mode != nil {
		old = *f.mode
	}
	if m != "" {
		f.mode = &m
	}
	f.mu.Unlock()
	if m != "" {
		f.commit("dpu.mode", old, m, "applied at a power cycle", 0)
	}
}

// socForceReset resets the DPU's SoC without waiting for the host (hostless DPUs): the Arm
// restarts, through the engine.
func (f *front) socForceReset(w http.ResponseWriter, r *http.Request, path string, p principal) {
	err := f.a.eng.Power(engine.Request{Path: f.dev.Path, Canonical: "power.restart", Source: src(r, p), Protocol: "redfish", Method: r.Method, Target: path})
	if err != nil {
		writeError(w, http.StatusConflict, "Base.1.15.ResourceInStandby", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, success())
}

// resetToDefaults is ResetToDefaults(ResetAll) as a BlueField BMC does it: the Truststore and
// the installed certificate are wiped, the OEM settings return to the factory's, the factory
// account's password returns to the factory one (and must be changed before anything else),
// and the BMC reboots.
func (f *front) resetToDefaults(w http.ResponseWriter, r *http.Request, path string, p principal) {
	var body struct{ ResetToDefaultsType string }
	_ = decode(r, &body)
	if body.ResetToDefaultsType != "" && body.ResetToDefaultsType != "ResetAll" {
		f.log(r, p, path, "bmc.defaults", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.ActionParameterValueNotInList", "ResetToDefaultsType must be ResetAll.")
		return
	}
	c := f.log(r, p, path, "bmc.defaults", http.StatusOK)
	f.mu.Lock()
	f.trust, f.installed, f.installedPM, f.pendingKey = nil, nil, "", nil
	f.tlsAuth = f.tlsAuthInit
	f.hostRshim, f.bmcRshim, f.mode, f.pendingMode = nil, nil, nil, ""
	f.pwChange = true
	f.mu.Unlock()
	f.commit("bmc.defaults", nil, "factory", "Truststore, installed certificate, OEM settings and the factory account's password reset", c)
	beh := schema.EventSource{Class: "behaviour", Principal: "bluefield"}
	_ = f.a.eng.UpdateAccount(engine.Request{Path: f.dev.Path, Canonical: "bmc.defaults", Source: beh, Protocol: "bmc"},
		f.dev.Identity.BMCUser, map[string]any{"Password": f.dev.Identity.BMCPassword})
	_ = f.a.eng.ResetBMC(engine.Request{Path: f.dev.Path, Canonical: "bmc.reset", Source: beh, Protocol: "bmc", Target: "ResetToDefaults"})
	writeJSON(w, http.StatusOK, success())
}

// ---------------------------------------------------------------- helpers

func decode(r *http.Request, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func src(r *http.Request, p principal) schema.EventSource {
	return schema.EventSource{Class: "stack", Principal: p.user, Address: r.RemoteAddr}
}

// log records a request the front answered, by canonical name, and returns its sequence number
// (the cause of the state it changed).
func (f *front) log(r *http.Request, p principal, path, canonical string, status int) uint64 {
	return f.a.eng.LogRead(engine.Request{Path: f.dev.Path, Canonical: canonical, Source: src(r, p),
		Protocol: "redfish", Method: r.Method, Target: path}, status)
}

// commit records a fact the BMC keeps itself as a state event on the device.
func (f *front) commit(field string, from, to any, msg string, cause uint64) {
	f.a.eng.Commit(schema.Event{Kind: "state", Source: schema.EventSource{Class: "behaviour", Principal: "bluefield"},
		Path: f.dev.Path, State: &schema.EventState{Field: field, From: from, To: to}, Message: msg, Cause: cause})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("OData-Version", "4.0")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, id, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": id, "message": msg,
		"@Message.ExtendedInfo": []any{map[string]any{"@odata.type": "#Message.v1_1_1.Message", "MessageId": id, "Message": msg, "MessageSeverity": "Critical", "Resolution": "None."}},
	}})
}

// writePasswordChange is DMTF's PasswordChangeRequired, as the engine answers it.
func writePasswordChange(w http.ResponseWriter, account string) {
	uri := "/redfish/v1/AccountService/Accounts/" + account
	msg := "The password provided for this account must be changed before access is granted. PATCH the Password property for this account located at the target URI '" + uri + "' to complete this process."
	writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": "Base.1.15.PasswordChangeRequired", "message": msg,
		"@Message.ExtendedInfo": []any{map[string]any{"@odata.type": "#Message.v1_1_1.Message", "MessageId": "Base.1.15.PasswordChangeRequired",
			"Message": msg, "MessageArgs": []any{uri}, "MessageSeverity": "Critical", "Resolution": "Change the password for this account using a PATCH to the Password property at the URI provided."}}}})
}

func (f *front) reportLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	if f.installed != nil {
		leaf, _ := x509.ParseCertificate(f.installed.Certificate[0])
		if leaf != nil {
			out = append(out, fmt.Sprintf("serves a certificate installed by ReplaceCertificate (issuer %q)", leaf.Issuer.CommonName))
		}
	}
	if len(f.trust) > 0 {
		out = append(out, fmt.Sprintf("Truststore holds %d certificate(s); TLS client authentication %v", len(f.trust), f.tlsAuth))
	}
	if f.bfb != nil {
		out = append(out, "last BFB pushed: "+f.bfb.summary())
	}
	return out
}

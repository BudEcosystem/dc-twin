// Package netsim is the portable substrate for endpoints on a host without TAPs (macOS, CI
// containers): every device endpoint gets its own loopback port, allocated in a stable order,
// and a site gateway (HTTP CONNECT) routes a stack's connections to a device's site address
// (the IP its DHCP lease gave it) to that endpoint, and to a stack service at the site address
// the stack's outputs name (`siteServices`), so a stack process and a `vm` guest reach a service
// at one address. The estate CA issues each BMC's certificate.
//
// What this substrate is not: there is no L2 segment, so link-layer facts (ARP, LLDP frames,
// a switch port's MAC table) are not visible to a stack; the fidelity report says so. On Linux
// the TAP substrate (D20, package tap) adds a link layer beside it: Net.L2 gives every BMC's
// management port its own NIC, and Listen serves an endpoint on its own address there as well as
// on its loopback port, so the site gateway keeps working.
package netsim

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const IsNot = "Loopback substrate: each endpoint is its own TCP/UDP port on this host and a stack reaches a device's site address through the site gateway (HTTP CONNECT); there is no L2 segment, so ARP, LLDP frames and MAC learning are not visible (on Linux, spec.adapters.network.substrate: tap adds the link layer, D20)."

// Net allocates endpoints and holds the estate CA.
type Net struct {
	mu        sync.Mutex
	BindIP    string
	next      int
	Endpoints map[string]map[string]string // device path -> kind -> host:port
	routes    map[string]string            // site host[:port] -> local host:port
	services  []Service                    // the stack's site services (siteServices)
	caCert    *x509.Certificate
	caKey     *ecdsa.PrivateKey
	CAPEM     []byte
	gateway   net.Listener
	Gateway   string
	stateDir  string
	listeners []io.Closer

	// L2 is the TAP substrate when spec.adapters.network.substrate is tap (nil otherwise).
	L2 L2
}

// New creates the substrate. basePort is the first port allocated.
func New(stateDir string, basePort int) (*Net, error) {
	if basePort == 0 {
		basePort = 24000
	}
	n := &Net{BindIP: "127.0.0.1", next: basePort, Endpoints: map[string]map[string]string{}, routes: map[string]string{}, stateDir: stateDir}
	if err := n.loadOrCreateCA(); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *Net) loadOrCreateCA() error {
	certPath, keyPath := filepath.Join(n.stateDir, "estate-ca.pem"), filepath.Join(n.stateDir, "estate-ca.key")
	if cb, err := os.ReadFile(certPath); err == nil {
		if kb, err := os.ReadFile(keyPath); err == nil {
			cblk, _ := pem.Decode(cb)
			kblk, _ := pem.Decode(kb)
			if cblk != nil && kblk != nil {
				c, err1 := x509.ParseCertificate(cblk.Bytes)
				k, err2 := x509.ParseECPrivateKey(kblk.Bytes)
				if err1 == nil && err2 == nil {
					n.caCert, n.caKey, n.CAPEM = c, k, cb
					return nil
				}
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dc-twin estate CA", Organization: []string{"dc-twin"}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	n.caCert, _ = x509.ParseCertificate(der)
	n.caKey = key
	n.CAPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if n.stateDir != "" {
		_ = os.MkdirAll(n.stateDir, 0o700)
		kd, _ := x509.MarshalECPrivateKey(key)
		_ = os.WriteFile(certPath, n.CAPEM, 0o644)
		_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), 0o600)
	}
	return nil
}

// Certificate issues a leaf certificate for a device endpoint.
func (n *Net) Certificate(cn string, names []string, ips []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: names}
	for _, ip := range append(ips, "127.0.0.1") {
		if p := net.ParseIP(ip); p != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, p)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, n.caCert, &key.PublicKey, n.caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der, n.caCert.Raw}, PrivateKey: key}, nil
}

// Listen opens the next free TCP port for a device endpoint of a kind. On the TAP substrate an
// endpoint kind with a well-known port (Redfish: 443) also answers on the device's own address,
// on its own NIC; the returned listener accepts from both, and its Addr is the loopback port.
func (n *Net) Listen(device, kind string) (net.Listener, error) {
	l, err := n.listenLoopback(device, kind)
	if err != nil || n.L2 == nil {
		return l, err
	}
	port, ok := l2Ports[kind]
	if !ok {
		return l, nil
	}
	ep := n.L2.DevicePort(device)
	if ep == nil {
		return l, nil
	}
	ll, err := ep.ListenTCP(port)
	if err != nil {
		l.Close()
		return nil, fmt.Errorf("%s %s on its L2 endpoint: %w", device, kind, err)
	}
	n.mu.Lock()
	n.listeners = append(n.listeners, ll)
	if n.Endpoints[device] == nil {
		n.Endpoints[device] = map[string]string{}
	}
	n.Endpoints[device][kind+"-l2"] = fmt.Sprintf("%s:%d", ep.Ref(), port)
	n.mu.Unlock()
	return newMultiListener(l, ll), nil
}

// Substrate is the name of the substrate endpoints are served on.
func (n *Net) Substrate() string {
	if n.L2 != nil {
		return n.L2.Name()
	}
	return "loopback"
}

// SubstrateIsNot is the substrate's part of the fidelity report: on the TAP substrate what it
// wired and observed, then what it is not.
func (n *Net) SubstrateIsNot() []string {
	var out []string
	if n.L2 != nil {
		r := n.L2.Report()
		out = append(append(out, r.Lines...), r.IsNot...)
	} else {
		out = append(out, IsNot)
	}
	n.mu.Lock()
	var svcs []string
	for _, s := range n.services {
		if s.Proto == "tcp" {
			svcs = append(svcs, s.String())
		}
	}
	n.mu.Unlock()
	if len(svcs) > 0 {
		sort.Strings(svcs)
		out = append(out, "The site gateway routes a stack process's TCP to the stack's own services at the site addresses its outputs name (siteServices), as a vm guest's segment gateway does, through the same proxy (a service sees this host's loopback address): "+strings.Join(svcs, ", "))
	}
	return out
}

func (n *Net) listenLoopback(device, kind string) (net.Listener, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for tries := 0; tries < 2000; tries++ {
		addr := fmt.Sprintf("%s:%d", n.BindIP, n.next)
		n.next++
		l, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		n.record(device, kind, addr)
		n.listeners = append(n.listeners, l)
		return l, nil
	}
	return nil, fmt.Errorf("no free port for %s %s", device, kind)
}

// ListenUDP opens the next free UDP port for a device endpoint.
func (n *Net) ListenUDP(device, kind string) (*net.UDPConn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for tries := 0; tries < 2000; tries++ {
		addr := fmt.Sprintf("%s:%d", n.BindIP, n.next)
		n.next++
		ua, _ := net.ResolveUDPAddr("udp", addr)
		c, err := net.ListenUDP("udp", ua)
		if err != nil {
			continue
		}
		n.record(device, kind, addr)
		n.listeners = append(n.listeners, c)
		return c, nil
	}
	return nil, fmt.Errorf("no free UDP port for %s %s", device, kind)
}

func (n *Net) record(device, kind, addr string) {
	if n.Endpoints[device] == nil {
		n.Endpoints[device] = map[string]string{}
	}
	n.Endpoints[device][kind] = addr
}

// Endpoint returns a recorded endpoint.
func (n *Net) Endpoint(device, kind string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.Endpoints[device][kind]
}

// Route makes a site address (ip:port, or a hostname:port) reach a local endpoint through the
// gateway.
func (n *Net) Route(site, local string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.routes[site] = local
}

// Unroute removes a route.
func (n *Net) Unroute(site string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.routes, site)
}

// SetServices gives the substrate the stack's site services (its `siteServices` output): the site
// gateway routes a stack process's TCP to each at its site address, as a `vm` guest's segment
// gateway routes the guest's (HostPort), so every client names a service by one address.
func (n *Net) SetServices(svcs []Service) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.services = append([]Service(nil), svcs...)
}

// Services is a copy of the stack's site services.
func (n *Net) Services() []Service {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Service(nil), n.services...)
}

// Resolve says where the site gateway routes a TCP site address (ip:port, or a hostname[:port]):
// a stack service at its site address, else an estate endpoint at its route; a hostname without
// a port is taken as :443.
func (n *Net) Resolve(site string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.resolveLocked(site)
}

func (n *Net) resolveLocked(site string) (string, bool) {
	for _, s := range n.services {
		if s.Proto == "tcp" && s.Site.String() == site {
			return s.Local, true
		}
	}
	if local, ok := n.routes[site]; ok {
		return local, true
	}
	if !strings.Contains(site, ":") {
		local, ok := n.routes[site+":443"]
		return local, ok
	}
	return "", false
}

// Routes is a copy of the routing table (estate endpoints; the stack's services are Services).
func (n *Net) Routes() map[string]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := map[string]string{}
	for k, v := range n.routes {
		out[k] = v
	}
	return out
}

// StartGateway starts the site gateway: an HTTP CONNECT proxy that routes only to estate
// endpoints and to the stack's site services (SetServices), and refuses everything else, so
// nothing behind it can reach the control plane or the internet through it (15 — isolation).
func (n *Net) StartGateway(addr string) error {
	if addr == "" {
		addr = n.BindIP + ":0"
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	n.gateway, n.Gateway = l, l.Addr().String()
	srv := &http.Server{Handler: http.HandlerFunc(n.serveConnect), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(l)
	return nil
}

func (n *Net) serveConnect(w http.ResponseWriter, r *http.Request) {
	target := ""
	if r.Method == http.MethodConnect {
		target = r.Host
	} else if r.URL.Host != "" {
		// plain HTTP through the proxy: forward the request line to the routed endpoint
		target = r.URL.Host
		if !strings.Contains(target, ":") {
			target += ":80"
		}
	}
	local, ok := n.Resolve(target)
	if !ok {
		http.Error(w, "dc-twin site gateway: no estate endpoint or stack service (siteServices) at "+target, http.StatusBadGateway)
		return
	}
	up, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if r.Method != http.MethodConnect {
		r.URL.Scheme, r.URL.Host = "", ""
		r.RequestURI = ""
		_ = r.Write(up)
		resp, err := http.ReadResponse(bufio.NewReader(up), r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			up.Close()
			return
		}
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		resp.Body.Close()
		up.Close()
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	// Hijack first and write the status line ourselves: a 2xx answer to CONNECT carries no
	// Transfer-Encoding or Content-Length (RFC 9110 §9.3.6), which net/http would add to one
	// written through the ResponseWriter.
	conn, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		up.Close()
		conn.Close()
		return
	}
	if buf.Reader.Buffered() > 0 {
		b, _ := buf.Peek(buf.Reader.Buffered())
		up.Write(b)
	}
	go func() { io.Copy(up, conn); up.Close() }()
	go func() { io.Copy(conn, up); conn.Close() }()
}

// Write saves the endpoint directory, routes and CA for clients (compat harnesses, the CLI).
func (n *Net) Write() error {
	if n.stateDir == "" {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	devs := make([]string, 0, len(n.Endpoints))
	for d := range n.Endpoints {
		devs = append(devs, d)
	}
	sort.Strings(devs)
	out := map[string]any{"gateway": n.Gateway, "endpoints": n.Endpoints, "routes": n.routes, "ca": filepath.Join(n.stateDir, "estate-ca.pem")}
	if len(n.services) > 0 {
		svcs := map[string]string{}
		for _, s := range n.services {
			if s.Proto == "tcp" {
				svcs[s.Site.String()] = s.Local
			}
		}
		out["services"] = svcs
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return os.WriteFile(filepath.Join(n.stateDir, "endpoints.json"), b, 0o644)
}

// Close closes every listener.
func (n *Net) Close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, l := range n.listeners {
		l.Close()
	}
	if n.gateway != nil {
		n.gateway.Close()
	}
	if n.L2 != nil {
		_ = n.L2.Close()
	}
}

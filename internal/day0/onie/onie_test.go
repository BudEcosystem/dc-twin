package onie

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

const (
	testNOS      = "generic/nvue-frr"
	testPlatform = "x86_64-nvidia_sn5600-r0"
)

var testID = nos.Identity{Serial: "MT2245X01234", BaseMAC: "1C:34:DA:00:00:01", MgmtMAC: "1C:34:DA:00:00:01",
	Manufacturer: "NVIDIA", Model: "SN5600", Platform: testPlatform, Arch: "x86_64"}

func TestParsePlatform(t *testing.T) {
	p, err := ParsePlatform("x86_64-mlnx_msn2700-r0")
	if err != nil {
		t.Fatal(err)
	}
	if want := (Platform{Arch: "x86_64", Machine: "mlnx_msn2700", Rev: "0", ASIC: "mlnx"}); p != want {
		t.Fatalf("got %+v, want %+v", p, want)
	}
	if p.VendorID() != 33049 {
		t.Errorf("vendor id %d", p.VendorID())
	}
	p, err = ParsePlatform("arm64-accton_as4610_54-r12")
	if err != nil || p.Arch != "arm64" || p.Machine != "accton_as4610_54" || p.Rev != "12" || p.ASIC != "accton" || p.VendorID() != IANAEnterprise {
		t.Errorf("got %+v %v", p, err)
	}
	for _, bad := range []string{"", "x86_64", "x86_64-mlnx_msn2700", "-mlnx-r0", "x86_64-mlnx_msn2700-r"} {
		if _, err := ParsePlatform(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestInstallerNames(t *testing.T) {
	p, _ := ParsePlatform("x86_64-mlnx_msn2700-r0")
	want := []string{
		"onie-installer-x86_64-mlnx_msn2700-r0", "onie-installer-x86_64-mlnx_msn2700-r0.bin",
		"onie-installer-x86_64-mlnx_msn2700", "onie-installer-x86_64-mlnx_msn2700.bin",
		"onie-installer-mlnx_msn2700", "onie-installer-mlnx_msn2700.bin",
		"onie-installer-x86_64-mlnx", "onie-installer-x86_64-mlnx.bin",
		"onie-installer-x86_64", "onie-installer-x86_64.bin",
		"onie-installer", "onie-installer.bin",
	}
	if got := p.InstallerNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestModifiers(t *testing.T) {
	p, _ := ParsePlatform(testPlatform)
	hw, _ := net.ParseMAC(testID.MgmtMAC)
	d, err := dhcpv4.NewDiscovery(hw, Modifiers(testID, p)...)
	if err != nil {
		t.Fatal(err)
	}
	o := d.Options
	if got := string(o.Get(dhcpv4.OptionClassIdentifier)); got != "onie_vendor:x86_64-nvidia_sn5600-r0" {
		t.Errorf("option 60 %q", got)
	}
	if got := o.Get(dhcpv4.OptionClientIdentifier); !bytes.Equal(got, append([]byte{0}, "MT2245X01234"...)) {
		t.Errorf("option 61 %x", got)
	}
	if got := string(o.Get(dhcpv4.OptionUserClassInformation)); got != "onie_dhcp_user_class" {
		t.Errorf("option 77 %q", got)
	}
	want125 := []byte{0, 0, 0xa6, 0x7f, 26, 3, 13}
	want125 = append(want125, "nvidia_sn5600"...)
	want125 = append(want125, 4, 6)
	want125 = append(want125, "x86_64"...)
	want125 = append(want125, 5, 1, '0')
	if got := o.Get(dhcpv4.OptionVendorIdentifyingVendorSpecific); !bytes.Equal(got, want125) {
		t.Errorf("option 125 %x, want %x", got, want125)
	}
	prl := o.Get(dhcpv4.OptionParameterRequestList)
	for _, c := range []byte{7, 43, 54, 66, 67, 72, 114, 125} {
		if !bytes.Contains(prl, []byte{c}) {
			t.Errorf("parameter request list %v lacks %d", prl, c)
		}
	}
}

func TestLeaseFromACK(t *testing.T) {
	hw, _ := net.ParseMAC(testID.MgmtMAC)
	vivso := []byte{0, 0, 0xa6, 0x7f, 0}
	sub := append([]byte{1, byte(len("http://10.0.0.9/x"))}, "http://10.0.0.9/x"...)
	vivso[4] = byte(len(sub))
	vivso = append(vivso, sub...)
	// another enterprise's block first, which ONIE ignores
	vivso = append([]byte{0, 0, 0, 9, 3, 1, 1, 'z'}, vivso...)
	ack, err := dhcpv4.New(dhcpv4.WithHwAddr(hw), dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
		dhcpv4.WithYourIP(net.IPv4(10, 30, 0, 57)), dhcpv4.WithServerIP(net.IPv4(10, 30, 0, 2)),
		dhcpv4.WithNetmask(net.CIDRMask(24, 32)),
		dhcpv4.WithOption(dhcpv4.OptRouter(net.IPv4(10, 30, 0, 1))),
		dhcpv4.WithOption(dhcpv4.OptDNS(net.IPv4(10, 30, 0, 3))),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.IPv4(10, 30, 0, 2))),
		dhcpv4.WithGeneric(dhcpv4.OptionURL, []byte("http://10.0.0.8/onie")),
		dhcpv4.WithGeneric(dhcpv4.OptionDefaultWorldWideWebServer, []byte{10, 0, 0, 7, 10, 0, 0, 6}),
		dhcpv4.WithGeneric(dhcpv4.GenericOptionCode(150), []byte{10, 0, 0, 5}),
		dhcpv4.WithGeneric(dhcpv4.OptionTFTPServerName, []byte("tftp.example")),
		dhcpv4.WithGeneric(dhcpv4.OptionBootfileName, []byte("installers/nos.bin")),
		dhcpv4.WithGeneric(dhcpv4.OptionVendorIdentifyingVendorSpecific, vivso),
	)
	if err != nil {
		t.Fatal(err)
	}
	l := LeaseFromACK(ack)
	if l.Addr.String() != "10.30.0.57/24" || !l.Router.Equal(net.IPv4(10, 30, 0, 1)) || !l.ServerID.Equal(net.IPv4(10, 30, 0, 2)) ||
		!l.SIAddr.Equal(net.IPv4(10, 30, 0, 2)) || len(l.DNS) != 1 {
		t.Errorf("lease %+v", l)
	}
	if l.URL != "http://10.0.0.8/onie" || l.InstallerURL != "http://10.0.0.9/x" || l.TFTP != "tftp.example" || l.Bootfile != "installers/nos.bin" {
		t.Errorf("lease %+v", l)
	}
	if fmt.Sprint(l.WWWServers) != "[10.0.0.7 10.0.0.6]" || fmt.Sprint(l.TFTPServers) != "[10.0.0.5]" {
		t.Errorf("servers %v %v", l.WWWServers, l.TFTPServers)
	}
}

// ---------------------------------------------------------------- the harness

type fakeDHCP struct {
	mu       sync.Mutex
	lease    func() *Lease
	leases   int
	released int
	mods     []dhcpv4.Modifier
}

func (f *fakeDHCP) Lease(_ context.Context, mods []dhcpv4.Modifier) (*Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases++
	f.mods = mods
	if f.lease == nil {
		return nil, errors.New("no offer")
	}
	return f.lease(), nil
}

func (f *fakeDHCP) Release(context.Context, *Lease, []dhcpv4.Modifier) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released++
	return nil
}

type fakeNet struct{ applied, flushed int }

func (f *fakeNet) Apply(*Lease) error { f.applied++; return nil }
func (f *fakeNet) Flush() error       { f.flushed++; return nil }

// server is an HTTP server that records every request and serves files by path.
type server struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	reqs  []*http.Request
}

func newServer(t *testing.T) *server {
	s := &server{files: map[string][]byte{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r)
		b, ok := s.files[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) serve(path string, b []byte) {
	s.mu.Lock()
	s.files[path] = b
	s.mu.Unlock()
}

// got is the requests seen, as host + path.
func (s *server) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.reqs {
		h, _, _ := net.SplitHostPort(r.Host)
		out = append(out, h+r.URL.Path)
	}
	return out
}

func (s *server) port() int {
	u, _ := url.Parse(s.URL)
	p, _ := strconv.Atoi(u.Port())
	return p
}

type harness struct {
	e      *Env
	dhcp   *fakeDHCP
	net    *fakeNet
	srv    *server
	log    *bytes.Buffer
	phases []string
	sleeps []time.Duration
	tftp   []string
	// onRetry runs at each retry sleep; returning true ends the run.
	onRetry func(pass int) bool
	run     func() error
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func newHarness(t *testing.T, images []nos.Image) *harness {
	t.Helper()
	dir := t.TempDir()
	for name, v := range map[string]any{"identity.json": testID, "images.json": images,
		"factory.json": Factory{Install: "onie", ZTP: true, NOS: testNOS, InstallTime: "5m0s"}} {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := &harness{dhcp: &fakeDHCP{}, net: &fakeNet{}, srv: newServer(t), log: &bytes.Buffer{}}
	h.dhcp.lease = func() *Lease {
		return &Lease{Addr: net.IPNet{IP: net.IPv4(10, 30, 0, 57).To4(), Mask: net.CIDRMask(24, 32)},
			DNS: []net.IP{net.IPv4(10, 30, 0, 3)}, ServerID: net.IPv4(10, 30, 0, 2)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pass := 0
	h.e = &Env{
		Dir: dir, State: filepath.Join(dir, "state"), Iface: "eth0",
		DHCP: h.dhcp, Net: h.net, HTTPPort: h.srv.port(), Out: h.log,
		Now: func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) },
		// every server the lease names is the one httptest server on 127.0.0.1
		Probe: func(_ context.Context, host string, _ int) bool { return host == "127.0.0.1" },
		Resolve: func(_ context.Context, _ []net.IP, name string) ([]net.IP, error) {
			if name == ServerName {
				return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
			}
			return nil, nil
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			h.sleeps = append(h.sleeps, d)
			if d == RetryDelay {
				pass++
				if h.onRetry == nil || h.onRetry(pass) {
					cancel()
					return context.Canceled
				}
			}
			return nil
		},
		TFTP: func(_ context.Context, server, file string, w io.Writer) error {
			h.tftp = append(h.tftp, server+"/"+file)
			return errors.New("file not found")
		},
	}
	h.e.onWrite = func(s Status) {
		if n := len(h.phases); n == 0 || h.phases[n-1] != s.Phase {
			h.phases = append(h.phases, s.Phase)
		}
	}
	h.run = func() error { return h.e.Install(ctx) }
	return h
}

func (h *harness) status(t *testing.T) Status {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.e.State, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// ---------------------------------------------------------------- the waterfall

// The order of one pass, with nothing to find: the exact URLs, then every name on onie-server
// and on the lease's server (the same httptest server, by name and by address), then TFTP with
// the bootfile, then TFTP's waterfall.
func TestWaterfallOrder(t *testing.T) {
	h := newHarness(t, nil)
	port := h.srv.port()
	h.dhcp.lease = func() *Lease {
		return &Lease{Addr: net.IPNet{IP: net.IPv4(10, 30, 0, 57).To4(), Mask: net.CIDRMask(24, 32)},
			DNS: []net.IP{net.IPv4(10, 30, 0, 3)}, ServerID: net.IPv4(127, 0, 0, 1), SIAddr: net.IPv4(127, 0, 0, 1),
			WWWServers:   []net.IP{net.IPv4(10, 9, 9, 9)}, // does not answer
			URL:          fmt.Sprintf("http://127.0.0.1:%d/exact-114", port),
			InstallerURL: fmt.Sprintf("http://127.0.0.1:%d/vivso-1", port),
			Bootfile:     "boot/nos.bin",
		}
	}
	if err := h.run(); !errors.Is(err, context.Canceled) {
		t.Fatalf("run: %v", err)
	}
	p, _ := ParsePlatform(testPlatform)
	want := []string{"127.0.0.1/vivso-1", "127.0.0.1/exact-114"}
	for _, host := range []string{"onie-server", "127.0.0.1"} {
		for _, n := range p.InstallerNames() {
			want = append(want, host+"/"+n)
		}
	}
	if got := h.srv.got(); !reflect.DeepEqual(got, want) {
		t.Fatalf("HTTP requests\n got %q\nwant %q", got, want)
	}
	// tftp_download: onie-server, siaddr (= server id); then waterfall(): onie-server, siaddr
	wantTFTP := []string{"127.0.0.1/boot/nos.bin"}
	def := "onie-installer-" + testPlatform
	paths := []string{"1c-34-da-00-00-01/" + def}
	for _, pre := range []string{"0A1E0039", "0A1E003", "0A1E00", "0A1E0", "0A1E", "0A1", "0A", "0"} {
		paths = append(paths, pre+"/"+def)
	}
	paths = append(paths, p.InstallerNames()...)
	for _, s := range paths {
		wantTFTP = append(wantTFTP, "127.0.0.1/"+s)
	}
	if !reflect.DeepEqual(h.tftp, wantTFTP) {
		t.Fatalf("TFTP requests\n got %q\nwant %q", h.tftp, wantTFTP)
	}
	if !strings.Contains(h.log.String(), "onie: http://10.9.9.9/: no answer") {
		t.Errorf("log:\n%s", h.log)
	}
	if s := h.status(t); s.Phase != PhaseDiscovering || s.Attempts != 1 || s.Updated != "2026-11-11T12:00:00Z" {
		t.Errorf("status %+v", s)
	}
}

// Without DNS servers, onie-server is skipped, and a TFTP timeout ends that server's waterfall.
func TestNoDNSAndTFTPTimeout(t *testing.T) {
	h := newHarness(t, nil)
	h.dhcp.lease = func() *Lease {
		return &Lease{Addr: net.IPNet{IP: net.IPv4(10, 30, 0, 57).To4(), Mask: net.CIDRMask(24, 32)}, SIAddr: net.IPv4(10, 1, 1, 1)}
	}
	h.e.TFTP = func(_ context.Context, server, file string, _ io.Writer) error {
		h.tftp = append(h.tftp, server+"/"+file)
		return ErrTimeout
	}
	h.run()
	if len(h.srv.got()) != 0 {
		t.Errorf("HTTP %q", h.srv.got())
	}
	if len(h.tftp) != 1 || h.tftp[0] != "10.1.1.1/1c-34-da-00-00-01/onie-installer-"+testPlatform {
		t.Errorf("TFTP %q", h.tftp)
	}
	if !strings.Contains(h.log.String(), "onie-server: the lease names no DNS server, skipped") {
		t.Errorf("log:\n%s", h.log)
	}
}

func TestHeaders(t *testing.T) {
	h := newHarness(t, nil)
	h.run()
	if len(h.srv.reqs) == 0 {
		t.Fatal("no request")
	}
	r := h.srv.reqs[0]
	for k, v := range map[string]string{
		"User-Agent":         "onie/1.0 (Linux-4.19.143-onie+2021.08; BusyBox-v1.25.1)",
		"ONIE-SERIAL-NUMBER": "MT2245X01234",
		"ONIE-ETH-ADDR":      "1c:34:da:00:00:01",
		"ONIE-VENDOR-ID":     "33049",
		"ONIE-MACHINE":       "nvidia_sn5600",
		"ONIE-MACHINE-REV":   "0",
		"ONIE-ARCH":          "x86_64",
		"ONIE-OPERATION":     "os-install",
		"ONIE-VERSION":       "2021.08",
	} {
		if got := r.Header.Get(k); got != v {
			t.Errorf("%s: %q, want %q", k, got, v)
		}
	}
	if k := r.Header.Get("ONIE-SECURITY-KEY"); len(k) != 16 {
		t.Errorf("ONIE-SECURITY-KEY %q", k)
	}
	if hk := h.e.Headers(); hk["ONIE-SERIAL-NUMBER"] == nil {
		t.Errorf("header keys not ONIE's case: %v", hk)
	}
	if r.Host != fmt.Sprintf("onie-server:%d", h.srv.port()) {
		t.Errorf("Host %q", r.Host)
	}
}

// ---------------------------------------------------------------- matching and installing

func TestInstall(t *testing.T) {
	img := []byte("#!/bin/sh\n# a placeholder NOS installer\n")
	other := []byte("someone else's installer")
	cases := []struct {
		name     string
		images   []nos.Image
		path     string // served at this path
		body     []byte
		reject   bool
		rejectFS bool
		power    bool   // the twin's onie-install-power-loss fault
		want     string // "" installs, else a substring of the reason
	}{
		{name: "sha256", images: []nos.Image{{SHA256: strings.ToUpper(sum(img)), NOS: testNOS, Version: "5.14.0"}},
			path: "/onie-installer", body: img},
		{name: "name", images: []nos.Image{{Name: `onie-installer-x86_64(\.bin)?`, NOS: testNOS, Version: "5.14.0"}},
			path: "/onie-installer-x86_64.bin", body: img},
		{name: "sha256 before name", images: []nos.Image{{Name: "onie-installer", NOS: "sonic-vs"}, {SHA256: sum(img), NOS: testNOS, Version: "5.14.0"}},
			path: "/onie-installer", body: img},
		{name: "name anchored", images: []nos.Image{{Name: "onie-installer", NOS: testNOS}},
			path: "/onie-installer.bin", body: img, want: "is no installer this switch recognises"},
		{name: "name with wrong sha256", images: []nos.Image{{Name: "onie-installer", SHA256: sum(other), NOS: testNOS}},
			path: "/onie-installer", body: img, want: "its sha256 is " + sum(img)},
		{name: "other NOS", images: []nos.Image{{SHA256: sum(img), NOS: "sonic-vs", Version: "202405"}},
			path: "/onie-installer", body: img, want: "an installer for sonic-vs 202405, not for this switch's generic/nvue-frr"},
		{name: "no match", images: []nos.Image{{SHA256: sum(other), NOS: testNOS}},
			path: "/onie-installer", body: img, want: "is no installer this switch recognises"},
		{name: "reject-all", images: []nos.Image{{SHA256: sum(img), NOS: testNOS}},
			path: "/onie-installer", body: img, reject: true, want: RejectReason},
		{name: "reject file", images: []nos.Image{{SHA256: sum(img), NOS: testNOS}},
			path: "/onie-installer", body: img, rejectFS: true, want: RejectReason},
		{name: "power lost during the install", images: []nos.Image{{SHA256: sum(img), NOS: testNOS}},
			path: "/onie-installer", body: img, power: true, want: "power was lost during the install"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.images)
			h.srv.serve(c.path, c.body)
			h.e.RejectAll = c.reject
			if c.rejectFS {
				os.WriteFile(filepath.Join(h.e.Dir, RejectFile), nil, 0o644)
			}
			if c.power {
				os.MkdirAll(filepath.Join(h.e.Dir, "faults"), 0o755)
				os.WriteFile(filepath.Join(h.e.Dir, "faults", "onie-power-loss"), nil, 0o644)
			}
			err := h.run()
			s := h.status(t)
			if c.want == "" {
				if err != nil {
					t.Fatalf("run: %v\n%s", err, h.log)
				}
				if s.Phase != PhaseInstalled || s.NOS != testNOS || s.Version != "5.14.0" || s.SHA256 != sum(img) ||
					s.URL != fmt.Sprintf("http://onie-server:%d%s", h.srv.port(), c.path) || s.Reason != "" {
					t.Errorf("status %+v", s)
				}
				if want := []string{PhaseDiscovering, PhaseFetching, PhaseInstalling, PhaseInstalled}; !reflect.DeepEqual(h.phases, want) {
					t.Errorf("phases %q, want %q", h.phases, want)
				}
				if !reflect.DeepEqual(h.sleeps, []time.Duration{5 * time.Minute}) {
					t.Errorf("sleeps %v", h.sleeps)
				}
				if h.dhcp.released != 1 || h.net.flushed != 1 || h.net.applied != 1 {
					t.Errorf("released %d, flushed %d, applied %d", h.dhcp.released, h.net.flushed, h.net.applied)
				}
				return
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("run: %v", err)
			}
			if s.Phase != PhaseFailed || !strings.Contains(s.Reason, c.want) || s.NOS != "" {
				t.Errorf("status %+v, want reason %q", s, c.want)
			}
			if h.dhcp.released != 0 || h.net.flushed != 0 {
				t.Errorf("released %d, flushed %d", h.dhcp.released, h.net.flushed)
			}
			// a failed installer does not end the pass: the rest of the waterfall is tried
			if got := h.srv.got(); len(got) != 12 {
				t.Errorf("%d requests: %q", len(got), got)
			}
			if !strings.Contains(h.log.String(), "onie: install failed: ") {
				t.Errorf("log:\n%s", h.log)
			}
		})
	}
}

// Nothing to find on the first pass; ONIE waits 20 s, discovers again, and installs on the
// second.
func TestRetry(t *testing.T) {
	img := []byte("installer")
	h := newHarness(t, []nos.Image{{SHA256: sum(img), NOS: testNOS, Version: "5.14.0"}})
	h.onRetry = func(pass int) bool {
		if pass == 1 {
			h.srv.serve("/onie-installer-x86_64-nvidia_sn5600.bin", img)
			return false
		}
		return true
	}
	if err := h.run(); err != nil {
		t.Fatalf("run: %v\n%s", err, h.log)
	}
	s := h.status(t)
	if s.Phase != PhaseInstalled || s.Attempts != 2 {
		t.Errorf("status %+v", s)
	}
	if h.dhcp.leases != 2 || h.dhcp.released != 1 {
		t.Errorf("leases %d, released %d", h.dhcp.leases, h.dhcp.released)
	}
	if !reflect.DeepEqual(h.sleeps, []time.Duration{RetryDelay, 5 * time.Minute}) {
		t.Errorf("sleeps %v", h.sleeps)
	}
	if want := []string{PhaseDiscovering, PhaseFetching, PhaseInstalling, PhaseInstalled}; !reflect.DeepEqual(h.phases, want) {
		t.Errorf("phases %q", h.phases)
	}
}

// A failed install, then the right one further down the waterfall, in the same pass.
func TestFailedThenInstalled(t *testing.T) {
	good, bad := []byte("good"), []byte("bad")
	h := newHarness(t, []nos.Image{{SHA256: sum(good), NOS: testNOS, Version: "5.14.0"}})
	h.srv.serve("/onie-installer-"+testPlatform, bad)
	h.srv.serve("/onie-installer", good)
	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{PhaseDiscovering, PhaseFetching, PhaseFailed, PhaseFetching, PhaseInstalling, PhaseInstalled}
	if !reflect.DeepEqual(h.phases, want) {
		t.Errorf("phases %q, want %q", h.phases, want)
	}
	if s := h.status(t); s.Attempts != 1 || !strings.HasSuffix(s.URL, "/onie-installer") || s.Reason != "" {
		t.Errorf("status %+v", s)
	}
}

// No lease: the pass ends, the reason is recorded, and ONIE tries again after 20 s.
func TestNoLease(t *testing.T) {
	h := newHarness(t, nil)
	h.dhcp.lease = nil
	h.onRetry = func(pass int) bool { return pass == 3 }
	h.run()
	s := h.status(t)
	if s.Phase != PhaseDiscovering || s.Attempts != 3 || !strings.Contains(s.Reason, "no DHCP lease on eth0: no offer") {
		t.Errorf("status %+v", s)
	}
	if h.dhcp.leases != 3 || len(h.srv.got()) != 0 {
		t.Errorf("leases %d", h.dhcp.leases)
	}
}

// A TFTP-only site: the installer under the address's hex directory.
func TestTFTPInstall(t *testing.T) {
	img := []byte("tftp installer")
	h := newHarness(t, []nos.Image{{SHA256: sum(img), NOS: testNOS, Version: "5.14.0"}})
	h.dhcp.lease = func() *Lease {
		return &Lease{Addr: net.IPNet{IP: net.IPv4(10, 30, 0, 57).To4(), Mask: net.CIDRMask(24, 32)}, SIAddr: net.IPv4(10, 1, 1, 1)}
	}
	h.e.TFTP = func(_ context.Context, server, file string, w io.Writer) error {
		h.tftp = append(h.tftp, server+"/"+file)
		if file == "0A1E00/onie-installer-"+testPlatform {
			_, err := w.Write(img)
			return err
		}
		return errors.New("file not found")
	}
	if err := h.run(); err != nil {
		t.Fatalf("run: %v\n%s", err, h.log)
	}
	if s := h.status(t); s.Phase != PhaseInstalled || s.URL != "tftp://10.1.1.1/0A1E00/onie-installer-"+testPlatform {
		t.Errorf("status %+v", s)
	}
	if len(h.tftp) != 4 {
		t.Errorf("TFTP %q", h.tftp)
	}
}

func TestMainFlags(t *testing.T) {
	var out, errb bytes.Buffer
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"phase":"installed"}`), 0o644)
	if rc := run([]string{"status", "--state", dir}, &out, &errb); rc != 0 || out.String() != `{"phase":"installed"}` {
		t.Errorf("status: %d %q %q", rc, out.String(), errb.String())
	}
	if rc := run([]string{"bogus"}, &out, &errb); rc != 2 {
		t.Errorf("bogus: %d", rc)
	}
	if rc := run([]string{"--no-such-flag"}, &out, &errb); rc != 2 {
		t.Errorf("bad flag: %d", rc)
	}
}

// onie-install -i's URL, from the NOS (a firmware the day-0 tool serves, NVIDIA Config Manager's
// /v1/device/<id>/firmware): ONIE fetches it first, whatever DHCP offers, and installs its image.
func TestInstallStaticURL(t *testing.T) {
	img := []byte("#!/bin/sh\n# the day-0 tool's firmware\n")
	h := newHarness(t, []nos.Image{{Name: "^firmware$", NOS: testNOS, Version: "5.14.0"}})
	h.srv.serve("/v1/device/abc/firmware", img)
	h.e.StaticURL = fmt.Sprintf("http://onie-server:%d/v1/device/abc/firmware", h.srv.port())
	if err := h.run(); err != nil {
		t.Fatalf("run: %v\n%s", err, h.log)
	}
	s := h.status(t)
	if s.Phase != PhaseInstalled || s.Version != "5.14.0" || s.URL != h.e.StaticURL {
		t.Errorf("status %+v", s)
	}
	if got := h.srv.got(); len(got) != 1 || !strings.HasSuffix(got[0], "/v1/device/abc/firmware") {
		t.Errorf("requests: %q", got)
	}
}

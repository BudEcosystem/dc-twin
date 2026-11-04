package dhcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/pin/tftp/v3"
)

// bootStack is the stack side of a network boot, in the shape a provisioning stack serves it:
// a relayed-only DHCP server that answers PXE clients with a TFTP server and iPXE's NBP, UEFI
// HTTP clients with the NBP's URL, and iPXE (user class "iPXE") with the boot script's URL; a
// TFTP server; and an HTTP server for the script, kernel and initrd. Its site address is
// 10.90.0.10 (the service network); the test routes it through the substrate.
type bootStack struct {
	t      *testing.T
	dhcp   *net.UDPConn
	files  map[string][]byte
	mu     sync.Mutex
	served map[string]int64   // path -> bytes written in full
	seen   map[string][]*seen // chaddr -> the requests that reached the server
	next   map[netip.Prefix]int
}

type seen struct {
	class, user string
	arch        []byte
	ndi, uuid   []byte
	features    []byte
	circuit     string
	link        net.IP
}

const bootSite = "10.90.0.10"

const bootScript = `#!ipxe
# the shape of a provisioning stack's rendered script: settings, a platform test, then
# kernel, initrd and boot
set base http://${next-server}/images/${buildarch}
iseq ${platform} efi && goto efi ||
echo legacy BIOS is not served
exit
:efi
isset ${serial} || goto noserial
kernel ${base}/vmlinuz console=ttyS0 serial=${serial} uuid=${uuid} BOOTIF=01-${net0/mac:hexhyp} vendor=${manufacturer}
initrd ${base}/initrd.img
imgfetch --name ks http://${next-server}/ks/${mac:hexraw}.cfg || echo no kickstart for ${mac}
imgfetch http://${next-server}/missing.cfg || echo optional file absent
boot
:noserial
echo no serial
exit
`

func newBootStack(t *testing.T) *bootStack {
	nbp := append([]byte("MZ\x90\x00 iPXE 1.21.1 -- Open Source Network Boot Firmware -- https://ipxe.org "), bytes.Repeat([]byte{0xa5}, 300_000)...)
	return &bootStack{t: t, served: map[string]int64{}, seen: map[string][]*seen{}, next: map[netip.Prefix]int{},
		files: map[string][]byte{
			"/ipxe.efi":                 nbp,
			"/ipxe-arm64.efi":           nbp,
			"/boot.ipxe":                []byte(bootScript),
			"/images/x86_64/vmlinuz":    bytes.Repeat([]byte("K"), 1_200_000),
			"/images/x86_64/initrd.img": bytes.Repeat([]byte("I"), 2_500_000),
			"/images/arm64/vmlinuz":     bytes.Repeat([]byte("k"), 1_000_000),
			"/images/arm64/initrd.img":  bytes.Repeat([]byte("i"), 2_000_000),
		}}
}

func (s *bootStack) count(path string, n int64) {
	s.mu.Lock()
	s.served[path] += n
	s.mu.Unlock()
}

// start opens the three services; it returns the DHCP server's address and a function that
// routes their site addresses through the substrate once the twin is up.
func (s *bootStack) start() (string, func(route func(site, local string))) {
	// TFTP
	tc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		s.t.Fatal(err)
	}
	ts := tftp.NewServer(func(name string, rf io.ReaderFrom) error {
		b, ok := s.files["/"+strings.TrimPrefix(name, "/")]
		if !ok {
			return fmt.Errorf("no file %s", name)
		}
		n, err := rf.ReadFrom(bytes.NewReader(b))
		if err == nil {
			s.count("tftp:/"+strings.TrimPrefix(name, "/"), n)
		}
		return err
	}, nil)
	go ts.Serve(tc)
	s.t.Cleanup(ts.Shutdown)
	// HTTP
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := s.files[r.URL.Path]
		if strings.HasPrefix(r.URL.Path, "/ks/") {
			b, ok = []byte("# kickstart for "+r.URL.Path), true
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		n, err := w.Write(b)
		if err == nil {
			s.count("http:"+r.URL.Path, int64(n))
		}
	}))
	s.t.Cleanup(hs.Close)
	// DHCP
	dc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		s.t.Fatal(err)
	}
	s.dhcp = dc
	s.t.Cleanup(func() { dc.Close() })
	go s.serveDHCP()
	return dc.LocalAddr().String(), func(route func(site, local string)) {
		route(bootSite+":69", tc.LocalAddr().String())
		route(bootSite+":80", strings.TrimPrefix(hs.URL, "http://"))
	}
}

func (s *bootStack) serveDHCP() {
	buf := make([]byte, 4096)
	for {
		n, from, err := s.dhcp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		m, err := dhcpv4.FromBytes(buf[:n])
		if err != nil || m.GatewayIPAddr.IsUnspecified() {
			continue // relayed requests only
		}
		rai := subOptions(m.Options.Get(optRAI))
		link := net.IP(rai[subLinkSelection])
		sn := &seen{class: m.ClassIdentifier(), user: string(m.Options.Get(dhcpv4.OptionUserClassInformation)),
			arch: m.Options.Get(dhcpv4.OptionClientSystemArchitectureType), ndi: m.Options.Get(dhcpv4.OptionClientNetworkInterfaceIdentifier),
			uuid: m.Options.Get(dhcpv4.OptionClientMachineIdentifier), features: m.Options.Get(dhcpv4.OptionEtherboot),
			circuit: string(rai[subCircuitID]), link: link}
		s.mu.Lock()
		s.seen[m.ClientHWAddr.String()] = append(s.seen[m.ClientHWAddr.String()], sn)
		pfx := netip.PrefixFrom(netip.AddrFrom4([4]byte(link.To4())), 24).Masked()
		if s.next[pfx] == 0 {
			s.next[pfx] = 100
		}
		ip, _ := hostAt(pfx, s.next[pfx])
		if m.MessageType() == dhcpv4.MessageTypeDiscover {
			s.next[pfx]++
		} else if req := m.RequestedIPAddress(); req != nil {
			ip, _ = netip.AddrFromSlice(req.To4())
		}
		s.mu.Unlock()
		t := dhcpv4.MessageTypeOffer
		if m.MessageType() == dhcpv4.MessageTypeRequest {
			t = dhcpv4.MessageTypeAck
		}
		mods := []dhcpv4.Modifier{dhcpv4.WithMessageType(t), dhcpv4.WithYourIP(ip.AsSlice()),
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP(bootSite))),
			dhcpv4.WithOption(dhcpv4.OptSubnetMask(net.CIDRMask(24, 32))),
			dhcpv4.WithOption(dhcpv4.OptRouter(link)),
			dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(time.Hour))}
		switch {
		case sn.user == "iPXE":
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptTFTPServerName(bootSite)),
				dhcpv4.WithOption(dhcpv4.OptBootFileName("http://"+bootSite+"/boot.ipxe")))
		case strings.HasPrefix(sn.class, "HTTPClient"):
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier("HTTPClient")),
				dhcpv4.WithOption(dhcpv4.OptBootFileName("http://"+bootSite+"/ipxe.efi")))
		case strings.HasPrefix(sn.class, "PXEClient:Arch:00011"):
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptTFTPServerName(bootSite)), dhcpv4.WithOption(dhcpv4.OptBootFileName("ipxe-arm64.efi")))
		case strings.HasPrefix(sn.class, "PXEClient"):
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptTFTPServerName(bootSite)), dhcpv4.WithOption(dhcpv4.OptBootFileName("ipxe.efi")))
		}
		r, _ := dhcpv4.NewReplyFromRequest(m, mods...)
		// answer the relay at giaddr, on its source port (RFC 8357), as Kea does
		_, _ = s.dhcp.WriteToUDP(r.ToBytes(), &net.UDPAddr{IP: m.GatewayIPAddr, Port: from.Port})
	}
}

// A model host powered on with a PXE (or UEFI HTTP) boot override is boot-walked to `boot`:
// DHCP with the PXE client options, the NBP over TFTP (or HTTP), iPXE's second DHCP, the
// script, every artifact it names fetched in full, and the walk stops at the script's boot.
func TestBootWalkPXEAndHTTPBoot(t *testing.T) {
	stack := newBootStack(t)
	srv, routes := stack.start()
	est := loadLab(t)
	// the provisioning network is the stack's: its DHCP answers relayed requests
	n := est.Spec.Networks["provisioning"]
	n.DHCP, n.Server = "stack", srv
	est.Spec.Networks["provisioning"] = n
	tw, a := up(t, est, func(a *Adapter) { a.retransmit = time.Second })
	routes(tw.Net.Route)

	hosts := map[string]string{"site-1/hall-a/b01/server-01": "Pxe", "site-1/hall-a/b01/server-02": "UefiHttp",
		"site-1/hall-a/c02/server-01": "Pxe"}
	src := schema.EventSource{Class: "scenario", Principal: "test"}
	once := "Once"
	for h, target := range hosts {
		target := target
		if err := tw.Engine.SetBoot(engine.Request{Path: h, Canonical: "boot.override", Protocol: "test", Source: src},
			engine.BootPatch{Target: &target, Enabled: &once}); err != nil {
			t.Fatal(err)
		}
		if err := tw.Engine.Power(engine.Request{Path: h, Canonical: "power.restart", Protocol: "test", Source: src}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		done := 0
		a.mu.Lock()
		for h := range hosts {
			if w := a.walks[h]; w != nil && w.step != "" {
				done++
			}
		}
		a.mu.Unlock()
		if done == len(hosts) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d walks finished", done, len(hosts))
		}
		time.Sleep(50 * time.Millisecond)
	}
	for h, target := range hosts {
		a.mu.Lock()
		w := a.walks[h]
		a.mu.Unlock()
		var booted string
		var d *model.Device
		tw.Engine.Read(func(m *model.Model) { d = m.DeviceBy[h]; booted = d.State.Booted })
		evs := tw.Engine.Events(0, func(e schema.Event) bool { return e.Path == h && e.State != nil && e.State.Field == "boot.walk" })
		var steps []string
		for _, e := range evs {
			steps = append(steps, fmt.Sprint(e.State.To))
		}
		t.Logf("%s (%s): walked %s; artifacts:", h, target, strings.Join(steps, " → "))
		for _, ar := range w.artifacts {
			t.Logf("    %-9s %s (%d bytes)", ar.Kind, ar.URL, ar.Bytes)
		}
		nbpStep := "tftp"
		if target == "UefiHttp" {
			nbpStep = "http"
		}
		want := []string{"dhcp", nbpStep, "ipxe-dhcp", "script", "artifacts", "booted"}
		if strings.Join(steps, ",") != strings.Join(want, ",") || booted != "booted" {
			t.Errorf("%s: steps %v (model %q), want %v", h, steps, booted, want)
		}
		mac := ""
		for _, p := range d.Ports {
			if p.Role == "provisioning" {
				mac = strings.ToLower(p.MAC)
			}
		}
		// what the stack's DHCP saw: the firmware's PXE options, then iPXE's
		stack.mu.Lock()
		reqs := stack.seen[mac]
		stack.mu.Unlock()
		if len(reqs) < 4 {
			t.Fatalf("%s: the stack saw %d requests", h, len(reqs))
		}
		fw, ipxe := reqs[0], reqs[len(reqs)-1]
		wantClass := "PXEClient:Arch:00007:UNDI:003016"
		if target == "UefiHttp" {
			wantClass = "HTTPClient:Arch:00016:UNDI:003016"
		}
		if fw.class != wantClass || !bytes.Equal(fw.ndi, []byte{1, 3, 16}) || len(fw.uuid) != 17 || fw.uuid[0] != 0 {
			t.Errorf("%s firmware DHCP: class %q ndi %v uuid %x", h, fw.class, fw.ndi, fw.uuid)
		}
		if got := fmt.Sprintf("%x", fw.uuid[1:]); got != strings.ReplaceAll(d.Identity.UUID, "-", "") {
			t.Errorf("%s: option 97 %s, want the system UUID %s", h, got, d.Identity.UUID)
		}
		if ipxe.user != "iPXE" || len(ipxe.features) == 0 || !strings.HasSuffix(ipxe.class, "UNDI:003010") && target == "Pxe" {
			t.Errorf("%s iPXE DHCP: user class %q, option 175 %x, class %q", h, ipxe.user, ipxe.features, ipxe.class)
		}
		if fw.circuit == "" || fw.link == nil {
			t.Errorf("%s: the stack saw no relay information", h)
		}
		// the variables reached the URLs the stack served
		if !hasArtifact(w, "/ks/"+strings.ReplaceAll(mac, ":", "")+".cfg") {
			t.Errorf("%s: no kickstart fetched by ${mac:hexraw}", h)
		}
		if !strings.Contains(w.step, "booted") {
			t.Errorf("%s ended at %s", h, w.step)
		}
	}
	// every artifact transferred in full, as the stack's own counters see it
	stack.mu.Lock()
	defer stack.mu.Unlock()
	for path, want := range map[string]int64{
		"tftp:/ipxe.efi":                 2 * int64(len(stack.files["/ipxe.efi"])),
		"http:/ipxe.efi":                 int64(len(stack.files["/ipxe.efi"])),
		"http:/boot.ipxe":                3 * int64(len(bootScript)),
		"http:/images/x86_64/vmlinuz":    3 * int64(len(stack.files["/images/x86_64/vmlinuz"])),
		"http:/images/x86_64/initrd.img": 3 * int64(len(stack.files["/images/x86_64/initrd.img"])),
	} {
		if stack.served[path] != want {
			t.Errorf("%s: %d bytes served, want %d", path, stack.served[path], want)
		}
	}
	for _, l := range a.Report().Lines {
		if strings.HasPrefix(l, "boot:") || strings.Contains(l, "provisioning") {
			t.Logf("report: %s", l)
		}
	}
}

func hasArtifact(w *walkRecord, suffix string) bool {
	for _, a := range w.artifacts {
		if strings.HasSuffix(a.URL, suffix) && a.Bytes > 0 {
			return true
		}
	}
	return false
}

// With no boot server (the twin site DHCP offers addresses only), the walk stops at dhcp and
// says why; a compute tray with no provisioning port stops there too.
func TestBootWalkStopsAndSaysWhy(t *testing.T) {
	tw, a := up(t, loadLab(t), func(a *Adapter) { a.retransmit = time.Second })
	src := schema.EventSource{Class: "scenario", Principal: "test"}
	pxe, once := "Pxe", "Once"
	hosts := []string{"site-1/hall-a/b02/server-01", "site-1/hall-a/a01/compute-tray-03"}
	for _, h := range hosts {
		if err := tw.Engine.SetBoot(engine.Request{Path: h, Canonical: "boot.override", Source: src}, engine.BootPatch{Target: &pxe, Enabled: &once}); err != nil {
			t.Fatal(err)
		}
		if err := tw.Engine.Power(engine.Request{Path: h, Canonical: "power.restart", Source: src}); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{
		hosts[0]: "dhcp (no boot server offered)",
		hosts[1]: "dhcp (no provisioning port on a network with DHCP)",
	}
	deadline := time.Now().Add(30 * time.Second)
	for h, w := range want {
		for {
			var got string
			tw.Engine.Read(func(m *model.Model) { got = m.DeviceBy[h].State.Booted })
			if got == w {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: booted %q, want %q", h, got, w)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	rep := strings.Join(a.Report().Lines, "\n")
	if !strings.Contains(rep, `boot: 2 model hosts boot-walked`) {
		t.Errorf("report:\n%s", rep)
	}
	t.Logf("%s", rep[strings.Index(rep, "boot:"):])
}

// The interpreter alone: iPXE's failure semantics, gotos, settings and their types.
func TestIPXEInterpreter(t *testing.T) {
	base, _ := url.Parse("http://10.0.0.1/s/boot.ipxe")
	var got []string
	fetch := func(ctx context.Context, u *url.URL, keep bool) ([]byte, int64, error) {
		got = append(got, u.String())
		if strings.HasSuffix(u.Path, "/missing") {
			return nil, 0, fmt.Errorf("404")
		}
		if strings.HasSuffix(u.Path, "/next.ipxe") {
			return []byte("#!ipxe\nkernel k2 a=${a}\nboot\n"), 20, nil
		}
		return nil, 10, nil
	}
	vars := map[string]string{"mac": "52:54:00:aa:bb:cc", "platform": "efi"}
	res := runIPXE(context.Background(), []byte(`#!ipxe
set a 1
iseq ${platform} pcbios && goto bios ||
isset ${nothing} && goto bios ||
imgfetch /missing || set a 2
kernel vmlinuz mac=${net0/mac:hexhyp}
chain next.ipxe
:bios
exit
`), base, vars, fetch)
	if !res.Booted || res.Image != "http://10.0.0.1/s/k2" {
		t.Fatalf("result %+v", res)
	}
	if strings.Join(got, " ") != "http://10.0.0.1/missing http://10.0.0.1/s/vmlinuz http://10.0.0.1/s/next.ipxe http://10.0.0.1/s/k2" {
		t.Errorf("fetched %v", got)
	}
	if vars["a"] != "2" {
		t.Errorf("a = %q", vars["a"])
	}
	// NICo's script for a DPU past discovery boots its disk: sanboot of a local drive, an exit
	res = runIPXE(context.Background(), []byte("#!ipxe\nsanboot --no-describe --drive 0x80 ||\nsanboot --no-describe --drive 0x81 ||\nexit\n"), base, map[string]string{}, fetch)
	if res.Booted || !strings.HasPrefix(res.Reason, "exit") {
		t.Errorf("local sanboot: %+v", res)
	}
	// an unhandled failure ends the script, as in iPXE
	res = runIPXE(context.Background(), []byte("#!ipxe\niseq a b && goto x\nboot\n:x\n"), base, map[string]string{}, fetch)
	if res.Booted || res.Step != "script" || !strings.Contains(res.Reason, "failed") {
		t.Errorf("unhandled failure: %+v", res)
	}
	res = runIPXE(context.Background(), []byte("#!ipxe\nparams\nparam a b\nchain --replace http://x/y##params\n"), base, map[string]string{}, fetch)
	if res.Booted || !strings.Contains(res.Reason, "params") {
		t.Errorf("params: %+v", res)
	}
	if vendorClassFor("aarch64", false, "003016") != "PXEClient:Arch:00011:UNDI:003016" ||
		vendorClassFor("x86_64", false, "003016") != "PXEClient:Arch:00007:UNDI:003016" ||
		vendorClassFor("x86_64", true, "003016") != "HTTPClient:Arch:00016:UNDI:003016" || vendorClassFor("aarch64", true, "003016") != "HTTPClient:Arch:00019:UNDI:003016" {
		t.Error("vendor classes")
	}
}

package dhcp

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
	"github.com/pin/tftp/v3"
)

// The boot walker (05 — The boot walk). A `model` host has no firmware, so when the engine
// powers one on with a network boot override (Hooks.OnBoot) the walker does on its behalf what
// UEFI and iPXE would, far enough for the stack's boot service to see a real client, and stops
// before anything executes: DHCP with the PXE (or HTTP boot) client options, the boot server's
// answer, a TFTP (or HTTP) download of the network boot program, iPXE's second DHCP, the boot
// script, every artifact it names, and the script's `boot`. Each step is committed with
// SetBooted, and the walk ends at "booted" or at "<step> (<why it stopped>)".

// Walk steps, in order.
const (
	stepDHCP      = "dhcp"
	stepTFTP      = "tftp"
	stepHTTP      = "http"
	stepIPXEDHCP  = "ipxe-dhcp"
	stepScript    = "script"
	stepArtifacts = "artifacts"
	stepBooted    = "booted"
)

// walkRecord is the last walk of a host, for the report and for tests.
type walkRecord struct {
	host      string
	target    string
	step      string // final: "booted" or "<step> (<reason>)"
	steps     []string
	nbp       string
	nbpBytes  int64
	script    string
	artifacts []artifact
	lease     string
	network   string
}

func (a *Adapter) onBoot(d *model.Device, target string) {
	if a.stopped.Load() || a.rt.BootClaim(d.Path) != "" {
		return // an impersonator boots this host itself (D55)
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.mu.Lock()
	if prev := a.walkStop[d.Path]; prev != nil {
		prev() // a new power-on supersedes a walk in progress
	}
	a.walkStop[d.Path] = cancel
	a.mu.Unlock()
	go a.walk(ctx, d, target)
}

// arch is the host's UEFI architecture: aarch64 for Grace compute trays and any profile whose
// vm fidelity says so, x86_64 otherwise.
func arch(d *model.Device) string {
	if d.BaseClass == "compute-tray" {
		return "aarch64"
	}
	if v, ok := d.Profile.Spec.Fidelity["vm"]["arch"].(string); ok && v == "aarch64" {
		return "aarch64"
	}
	return "x86_64"
}

// pxeArch is option 93's value and option 60's Arch field: RFC 4578 and IANA's processor
// architecture registry, as EDK2 sends them (0x0007 for x64 PXE, 0x000b for AArch64; 0x0010
// and 0x0013 for HTTP boot).
func pxeArch(archName string, httpBoot bool) iana.Arch {
	switch {
	case archName == "aarch64" && httpBoot:
		return iana.EFI_ARM64_HTTP
	case archName == "aarch64":
		return iana.EFI_ARM64
	case httpBoot:
		return iana.EFI_X86_64_HTTP
	}
	return iana.EFI_X86_64
}

// vendorClassFor is option 60 as EDK2 (and iPXE) send it: "<PXEClient|HTTPClient>:Arch:<arch>:UNDI:
// <major><minor>", five fields for HTTP boot as for PXE (EDK2's HttpBootDxe; NICo's DHCP server
// reads any other shape as an unknown client and offers it no boot URI, found on the trays path).
func vendorClassFor(archName string, httpBoot bool, undi string) string {
	if httpBoot {
		return fmt.Sprintf("HTTPClient:Arch:%05d:UNDI:%s", pxeArch(archName, true), undi)
	}
	return fmt.Sprintf("PXEClient:Arch:%05d:UNDI:%s", pxeArch(archName, false), undi)
}

// uuidBytes is the SMBIOS system UUID as option 97 carries it (type 0, then 16 octets).
func uuidBytes(u string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(u, "-", ""))
	if err != nil || len(b) != 16 {
		return make([]byte, 16)
	}
	return b
}

// provisioningPort is the host's port on a provisioning network with DHCP.
func (a *Adapter) provisioningPort(d *model.Device) (*model.Port, *netState, string) {
	why := "no provisioning port on a network with DHCP"
	for _, p := range d.Ports {
		if p.Role != "provisioning" || p.Component != nil {
			continue
		}
		ns := a.nets[p.Network]
		if ns == nil {
			continue
		}
		if ns.idle != "" {
			why = "network " + p.Network + " has no DHCP server"
			continue
		}
		return p, ns, ""
	}
	return nil, nil, why
}

func (a *Adapter) walk(ctx context.Context, d *model.Device, target string) {
	w := &walkRecord{host: d.Path, target: target}
	finish := func(step, reason string) {
		final := step
		if reason != "" {
			final = step + " (" + reason + ")"
		}
		w.step = final
		w.steps = append(w.steps, final)
		a.mu.Lock()
		a.walks[d.Path] = w
		a.mu.Unlock()
		if ctx.Err() != nil && reason != "" {
			return // superseded or stopped: the next walk records its own
		}
		a.rt.Engine.SetBooted(d.Path, final, walkerSrc)
	}
	reached := func(step string) {
		w.steps = append(w.steps, step)
		a.rt.Engine.SetBooted(d.Path, step, walkerSrc)
	}
	if d.Fidelity != "model" {
		return // OVMF boots a vm host for real
	}
	p, ns, why := a.provisioningPort(d)
	if p == nil {
		finish(stepDHCP, why)
		return
	}
	ep, err := a.walkEndpoint(d, p, ns)
	if err != nil {
		finish(stepDHCP, err.Error())
		return
	}
	httpBoot := target == "UefiHttp"
	archName := arch(d)
	common := []dhcpv4.Modifier{
		dhcpv4.WithOption(dhcpv4.OptClientArch(pxeArch(archName, httpBoot))),
		dhcpv4.WithGeneric(dhcpv4.OptionClientNetworkInterfaceIdentifier, []byte{1, 3, 16}),
		dhcpv4.WithGeneric(dhcpv4.OptionClientMachineIdentifier, append([]byte{0}, uuidBytes(d.Identity.UUID)...)),
		dhcpv4.WithOption(dhcpv4.OptMaxMessageSize(1472)),
	}
	// 1. the firmware's DHCP (the options go with each exchange: a superseded walk of the same
	// host may still be finishing on this endpoint)
	fwOpts := append([]dhcpv4.Modifier{
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier(vendorClassFor(archName, httpBoot, "003016"))),
		dhcpv4.WithOption(dhcpv4.OptParameterRequestList(dhcpv4.OptionSubnetMask, dhcpv4.OptionTimeOffset, dhcpv4.OptionRouter,
			dhcpv4.OptionDomainNameServer, dhcpv4.OptionHostName, dhcpv4.OptionDomainName, dhcpv4.OptionRootPath,
			dhcpv4.OptionVendorSpecificInformation, dhcpv4.OptionClassIdentifier, dhcpv4.OptionTFTPServerName,
			dhcpv4.OptionBootfileName, dhcpv4.OptionClientMachineIdentifier)),
	}, common...)
	offer, ack, err := ep.acquire(ctx, 4, fwOpts...)
	if err != nil {
		finish(stepDHCP, dhcpFailure(err))
		return
	}
	w.lease, w.network = ack.YourIPAddr.String(), p.Network
	a.rt.Engine.Commit(schema.Event{Kind: "request", Source: walkerSrc, Path: d.Path,
		Message: fmt.Sprintf("boot walk: %s leased %s on %s from %s", p.Name, ack.YourIPAddr, p.Network, serverOf(ack)),
		Data:    map[string]any{"port": p.Ref, "ip": ack.YourIPAddr.String(), "circuitId": ep.circuit, "vendorClass": vendorClassFor(archName, httpBoot, "003016")}})
	next, file := bootParams(ack, offer)
	if file == "" {
		if strings.HasPrefix(offer.ClassIdentifier(), "PXEClient") || strings.HasPrefix(ack.ClassIdentifier(), "PXEClient") {
			finish(stepDHCP, "the server expects ProxyDHCP on port 4011, which the walker does not implement")
		} else {
			finish(stepDHCP, "no boot server offered")
		}
		return
	}
	reached(stepDHCP)
	// 2–3. the network boot program
	var nbp []byte
	nbpStep := stepTFTP
	if httpBoot {
		nbpStep = stepHTTP
		u, err := url.Parse(file)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			finish(stepHTTP, fmt.Sprintf("the server offered %q, not an HTTP boot URI", file))
			return
		}
		body, _, err := a.fetchURL(ctx, u, next, true)
		if err != nil {
			finish(stepHTTP, err.Error())
			return
		}
		nbp, w.nbp = body, u.String()
	} else {
		if strings.Contains(file, "://") {
			finish(stepTFTP, fmt.Sprintf("the server offered the URL %q to a PXE client, which fetches only by TFTP", file))
			return
		}
		body, err := a.tftpGet(ctx, next, file, true)
		if err != nil {
			finish(stepTFTP, fmt.Sprintf("RRQ %s from %s: %v", file, next, err))
			return
		}
		nbp, w.nbp = body, file
	}
	w.nbpBytes = int64(len(nbp))
	if !bytes.Contains(nbp, []byte("iPXE")) && !strings.Contains(strings.ToLower(w.nbp), "ipxe") {
		finish(nbpStep, fmt.Sprintf("the network boot program %s is not iPXE, and the walker walks iPXE only", baseName(w.nbp)))
		return
	}
	reached(nbpStep)
	// 4. iPXE's own DHCP
	ipxeOpts := append([]dhcpv4.Modifier{
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier(vendorClassFor(archName, httpBoot, "003010"))),
		dhcpv4.WithGeneric(dhcpv4.OptionUserClassInformation, []byte("iPXE")),
		dhcpv4.WithGeneric(dhcpv4.OptionEtherboot, ipxeFeatures()),
		dhcpv4.WithOption(dhcpv4.OptParameterRequestList(dhcpv4.OptionSubnetMask, dhcpv4.OptionRouter, dhcpv4.OptionDomainNameServer,
			dhcpv4.OptionHostName, dhcpv4.OptionDomainName, dhcpv4.OptionRootPath, dhcpv4.OptionVendorSpecificInformation,
			dhcpv4.OptionClassIdentifier, dhcpv4.OptionTFTPServerName, dhcpv4.OptionBootfileName, dhcpv4.OptionEtherboot)),
	}, common...)
	offer2, ack2, err := ep.acquire(ctx, 4, ipxeOpts...)
	if err != nil {
		finish(stepIPXEDHCP, dhcpFailure(err))
		return
	}
	next2, script := bootParams(ack2, offer2)
	if script == "" {
		finish(stepIPXEDHCP, "the server offered iPXE no boot file")
		return
	}
	if script == file {
		finish(stepIPXEDHCP, fmt.Sprintf("the server offered iPXE the network boot program %s again; a real iPXE would chain-load it in a loop", file))
		return
	}
	reached(stepIPXEDHCP)
	// 5. the boot script
	su, err := scriptURL(script, next2)
	if err != nil {
		finish(stepScript, err.Error())
		return
	}
	body, _, err := a.fetchURL(ctx, su, next2, true)
	if err != nil {
		finish(stepScript, err.Error())
		return
	}
	w.script = su.String()
	reached(stepScript)
	if !isIPXEScript(body) {
		// iPXE's autoboot executes whatever it fetched: that is the boot
		w.artifacts = append(w.artifacts, artifact{Kind: "image", URL: su.String(), Bytes: int64(len(body))})
		finish(stepBooted, "")
		return
	}
	// 6–7. interpret it, fetching every artifact, and stop at boot
	vars := a.ipxeVars(d, p, ack2, next2, script)
	fetched := false
	res := runIPXE(ctx, body, su, vars, func(ctx context.Context, u *url.URL, keep bool) ([]byte, int64, error) {
		b, n, err := a.fetchURL(ctx, u, next2, keep)
		if err == nil && !fetched {
			fetched = true
			reached(stepArtifacts)
		}
		return b, n, err
	})
	w.artifacts = append(w.artifacts, res.Artifacts...)
	if !res.Booted {
		finish(res.Step, res.Reason)
		return
	}
	finish(stepBooted, "")
}

func dhcpFailure(err error) string {
	switch {
	case errors.Is(err, errDown):
		return "the provisioning port is down"
	case errors.Is(err, errNAK):
		return "the server answered NAK"
	case errors.Is(err, context.Canceled):
		return "superseded"
	}
	return err.Error()
}

func serverOf(ack *dhcpv4.DHCPv4) string {
	if sid := ack.ServerIdentifier(); sid != nil {
		return sid.String()
	}
	return "an unidentified server"
}

// bootParams reads the boot server and file: options 66 and 67, else siaddr and the file
// field, from the ACK, else from the offer (PXE firmware boots from whichever carries them).
func bootParams(ack, offer *dhcpv4.DHCPv4) (next, file string) {
	for _, m := range []*dhcpv4.DHCPv4{ack, offer} {
		if m == nil {
			continue
		}
		if next == "" {
			next = m.TFTPServerName()
			if next == "" && m.ServerIPAddr != nil && !m.ServerIPAddr.IsUnspecified() {
				next = m.ServerIPAddr.String()
			}
		}
		if file == "" {
			file = m.BootFileNameOption()
			if file == "" {
				file = m.BootFileName
			}
		}
	}
	return strings.TrimRight(next, "\x00"), strings.TrimRight(file, "\x00")
}

// scriptURL makes iPXE's boot file a URL: a bare name is fetched by TFTP from next-server.
func scriptURL(file, next string) (*url.URL, error) {
	if strings.Contains(file, "://") {
		return url.Parse(file)
	}
	if next == "" {
		return nil, fmt.Errorf("the boot file %q names no server and no next-server was offered", file)
	}
	return &url.URL{Scheme: "tftp", Host: next, Path: "/" + strings.TrimPrefix(file, "/")}, nil
}

// ipxeFeatures is option 175 as iPXE sends it: encapsulated feature flags (HTTP, DNS, TFTP,
// EFI and the image formats it runs).
func ipxeFeatures() []byte {
	var b []byte
	for _, code := range []byte{0x13 /* HTTP */, 0x15 /* TFTP */, 0x17 /* DNS */, 0x18 /* bzImage */, 0x24 /* EFI */, 0x27 /* menu */} {
		b = append(b, code, 1, 1)
	}
	return b
}

// ipxeVars are the settings an iPXE script sees on this host.
func (a *Adapter) ipxeVars(d *model.Device, p *model.Port, ack *dhcpv4.DHCPv4, next, file string) map[string]string {
	v := map[string]string{
		"mac": strings.ToLower(p.MAC), "serial": d.Identity.Serial, "uuid": d.Identity.UUID, "asset": "",
		"buildarch": map[string]string{"aarch64": "arm64", "x86_64": "x86_64"}[arch(d)], "platform": "efi",
		"user-class": "iPXE", "next-server": next, "filename": file, "ip": ack.YourIPAddr.String(),
	}
	if id := d.Profile.Spec.Identity; id != nil {
		v["manufacturer"], v["product"] = id.Manufacturer, id.Model
	}
	if m := ack.SubnetMask(); m != nil {
		v["netmask"] = net.IP(m).String()
	}
	if r := ack.Router(); len(r) > 0 {
		v["gateway"] = r[0].String()
	}
	if dns := ack.DNS(); len(dns) > 0 {
		v["dns"] = dns[0].String()
	}
	if h := ack.HostName(); h != "" {
		v["hostname"] = h
	}
	for _, k := range []string{"mac", "ip", "netmask", "gateway", "dns"} {
		v["net0/"+k] = v[k]
	}
	return v
}

// walkEndpoint is the DHCP client of a host's provisioning port, kept across walks.
func (a *Adapter) walkEndpoint(d *model.Device, p *model.Port, ns *netState) (*endpoint, error) {
	a.mu.Lock()
	ep := a.walkEPs[p.Ref]
	a.mu.Unlock()
	if ep != nil {
		return ep, nil
	}
	var why string
	ep = a.portEndpoint(d, p, ns, func(w string) *endpoint { why = w; return nil })
	if ep == nil {
		return nil, errors.New(why)
	}
	ep.dev = d
	if err := ep.open("dhcp-boot:" + p.Name); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.walkEPs[p.Ref] = ep
	a.mu.Unlock()
	return ep, nil
}

// ---------------------------------------------------------------- transfers

type counter struct{ n int64 }

func (c *counter) Write(b []byte) (int, error) { c.n += int64(len(b)); return len(b), nil }

// tftpGet is a TFTP RRQ, transferred fully. host may carry a port; otherwise 69. The site
// address is resolved through the substrate's routes.
func (a *Adapter) tftpGet(ctx context.Context, host, file string, keep bool) ([]byte, error) {
	hp := host
	if _, _, err := net.SplitHostPort(host); err != nil {
		hp = net.JoinHostPort(host, "69")
	}
	c, err := tftp.NewClient(a.resolve(hp))
	if err != nil {
		return nil, err
	}
	c.SetTimeout(2 * time.Second)
	c.SetRetries(3)
	c.RequestTSize(true)
	type res struct {
		b   []byte
		err error
	}
	done := make(chan res, 1)
	go func() {
		wt, err := c.Receive(file, "octet")
		if err != nil {
			done <- res{nil, err}
			return
		}
		var buf bytes.Buffer
		var w io.Writer = &buf
		cnt := &counter{}
		if !keep {
			w = cnt
		}
		if _, err := wt.WriteTo(w); err != nil {
			done <- res{nil, err}
			return
		}
		done <- res{buf.Bytes(), nil}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		return r.b, r.err
	}
}

// fetchURL GETs an http or tftp URL fully. HTTPS stops the walk: the walker cannot present the
// trust roots an iPXE build embeds, nor a client certificate.
func (a *Adapter) fetchURL(ctx context.Context, u *url.URL, next string, keep bool) ([]byte, int64, error) {
	switch u.Scheme {
	case "tftp":
		host := u.Host
		if host == "" {
			host = next
		}
		b, err := a.tftpGet(ctx, host, strings.TrimPrefix(u.Path, "/"), true)
		if err != nil {
			return nil, 0, fmt.Errorf("TFTP %s: %v", u, err)
		}
		if !keep {
			return nil, int64(len(b)), nil
		}
		return b, int64(len(b)), nil
	case "http":
	case "https":
		return nil, 0, &stopErr{stepArtifacts, "HTTPS (" + u.String() + ") is not implemented: the walker cannot present iPXE's embedded trust roots or a client certificate"}
	default:
		return nil, 0, &stopErr{stepArtifacts, "unsupported URI scheme " + u.Scheme + " (" + u.String() + ")"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "iPXE/1.21.1")
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, 0, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	if keep {
		b, err := io.ReadAll(resp.Body)
		return b, int64(len(b)), err
	}
	n, err := io.Copy(io.Discard, resp.Body)
	return nil, n, err
}

func (a *Adapter) httpClient() *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return d.DialContext(ctx, network, a.resolve(addr))
		},
	}}
}

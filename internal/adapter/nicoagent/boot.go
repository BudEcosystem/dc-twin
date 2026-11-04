package nicoagent

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"

	"github.com/BudEcosystem/dc-twin/internal/adapter/dhcp"
)

// A machine's UEFI network boot, done on its own network path (guest.go). NICo boots a
// BlueField it is ingesting over UEFI HTTP (it sets the DPU's boot order to HTTP boot through
// the BMC, then restarts it), and a host it is discovering the same way: the firmware's DHCP as
// an HTTP client of the machine's architecture, the boot program
// the server names (NICo's iPXE), iPXE's own DHCP, and the script iPXE runs, which chains NICo's
// boot service for this machine. The script iPXE runs is the one built into the iPXE it
// downloaded (NICo embeds its own), read out of the image; one the DHCP server names instead is
// run when the image carries none. The scripts are interpreted by the boot walker's interpreter
// (package dhcp), with what the machine's firmware knows of itself (its SMBIOS: manufacturer,
// product, serial) as iPXE's settings. The boot ends at the kernel NICo names, with its command
// line, or at `exit`, which returns to the firmware's next boot option: the machine's own disk.

// bootOutcome is where a boot ended.
type bootOutcome struct {
	lease   *lease
	steps   []string
	kernel  string // the image `boot` ran, when it booted
	cmdline string
	exited  bool   // the script exited to the firmware (boot from disk)
	failed  string // why it stopped otherwise
	nbp     string
	script  string
}

// Steps of a boot, committed as the DPU's boot progress (engine.SetBooted).
const (
	stepDHCP     = "dhcp"
	stepHTTP     = "http"
	stepIPXEDHCP = "ipxe-dhcp"
	stepScript   = "script"
	stepBooted   = "booted"
	stepOS       = "os"
)

// smbios is what a machine's firmware knows of it: what its BMC serves for the system.
type smbios struct {
	manufacturer, product, serial, uuid string
}

func (b *machine) netboot(ctx context.Context, step func(string)) bootOutcome {
	var out bootOutcome
	fail := func(s, why string) bootOutcome {
		out.failed = s + " (" + why + ")"
		out.steps = append(out.steps, out.failed)
		return out
	}
	reached := func(s string) {
		out.steps = append(out.steps, s)
		step(s)
	}
	archType, buildarch := iana.EFI_X86_64_HTTP, "x86_64"
	if b.arch == "aarch64" {
		archType, buildarch = iana.EFI_ARM64_HTTP, "arm64"
	}
	common := []dhcpv4.Modifier{
		dhcpv4.WithOption(dhcpv4.OptClientArch(archType)),
		dhcpv4.WithGeneric(dhcpv4.OptionClientNetworkInterfaceIdentifier, []byte{1, 3, 16}),
		dhcpv4.WithGeneric(dhcpv4.OptionClientMachineIdentifier, append([]byte{0}, uuidBytes(b.sm.uuid)...)),
		dhcpv4.WithOption(dhcpv4.OptMaxMessageSize(1472)),
	}
	// option 60 as EDK2's HTTP boot client sends it, and as iPXE does (its UNDI version): five
	// fields, which NICo's DHCP server requires of a UEFI client before it offers a boot URI
	vendor := fmt.Sprintf("HTTPClient:Arch:%05d:UNDI:003000", archType)
	ipxeVendor := fmt.Sprintf("HTTPClient:Arch:%05d:UNDI:003010", archType)
	fw := append([]dhcpv4.Modifier{
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier(vendor)),
		dhcpv4.WithOption(dhcpv4.OptParameterRequestList(dhcpv4.OptionSubnetMask, dhcpv4.OptionRouter, dhcpv4.OptionDomainNameServer,
			dhcpv4.OptionHostName, dhcpv4.OptionDomainName, dhcpv4.OptionVendorSpecificInformation, dhcpv4.OptionClassIdentifier,
			dhcpv4.OptionBootfileName)),
	}, common...)
	l, err := b.nic.acquire(ctx, 4, fw...)
	if err != nil {
		return fail(stepDHCP, err.Error())
	}
	out.lease = l
	b.leased(l)
	next, file := bootParams(l.ack, l.offer)
	if file == "" {
		return fail(stepDHCP, "the DHCP server offered no boot URI")
	}
	reached(stepDHCP)
	u, err := url.Parse(file)
	if err != nil || u.Scheme != "http" {
		return fail(stepHTTP, fmt.Sprintf("the server offered %q, not an HTTP boot URI", file))
	}
	nbp, err := b.get(ctx, u, true)
	if err != nil {
		return fail(stepHTTP, err.Error())
	}
	out.nbp = u.String()
	embedded := embeddedScript(nbp)
	if embedded == nil && !bytes.Contains(nbp, []byte("iPXE")) {
		return fail(stepHTTP, fmt.Sprintf("the boot program %s is not iPXE", u))
	}
	reached(stepHTTP)
	ipxe := append([]dhcpv4.Modifier{
		dhcpv4.WithOption(dhcpv4.OptClassIdentifier(ipxeVendor)),
		dhcpv4.WithGeneric(dhcpv4.OptionUserClassInformation, []byte("iPXE")),
		dhcpv4.WithOption(dhcpv4.OptParameterRequestList(dhcpv4.OptionSubnetMask, dhcpv4.OptionRouter, dhcpv4.OptionDomainNameServer,
			dhcpv4.OptionHostName, dhcpv4.OptionDomainName, dhcpv4.OptionClassIdentifier, dhcpv4.OptionBootfileName)),
	}, common...)
	l2, err := b.nic.acquire(ctx, 4, ipxe...)
	if err != nil {
		return fail(stepIPXEDHCP, err.Error())
	}
	b.leased(l2)
	next2, file2 := bootParams(l2.ack, l2.offer)
	if next2 == "" {
		next2 = next
	}
	reached(stepIPXEDHCP)
	vars := map[string]string{
		"mac": strings.ToLower(b.nic.mac.String()), "serial": b.sm.serial, "uuid": b.sm.uuid, "asset": "",
		"manufacturer": b.sm.manufacturer, "product": b.sm.product, "buildarch": buildarch, "platform": "efi",
		"user-class": "iPXE", "next-server": next2, "filename": file2, "ip": l2.addr.Addr().String(),
	}
	if l2.router.IsValid() {
		vars["gateway"] = l2.router.String()
	}
	for _, k := range []string{"mac", "ip", "gateway"} {
		vars["net0/"+k] = vars[k]
	}
	fetch := func(ctx context.Context, u *url.URL, keep bool) ([]byte, int64, error) {
		body, err := b.get(ctx, u, keep)
		return body, int64(len(body)), err
	}
	var res dhcp.IPXEResult
	if embedded != nil {
		out.script = out.nbp + " (embedded)"
		res = dhcp.RunIPXE(ctx, embedded, u, vars, fetch)
	} else {
		if file2 == "" {
			return fail(stepIPXEDHCP, "the server offered iPXE no boot file, and the boot program embeds no script")
		}
		su, err := url.Parse(file2)
		if err != nil || su.Scheme != "http" {
			return fail(stepScript, fmt.Sprintf("iPXE was offered %q, not an HTTP URI", file2))
		}
		body, err := b.get(ctx, su, true)
		if err != nil {
			return fail(stepScript, err.Error())
		}
		out.script = su.String()
		if !dhcp.IsIPXEScript(body) {
			return fail(stepScript, su.String()+" is not an iPXE script")
		}
		res = dhcp.RunIPXE(ctx, body, su, vars, fetch)
	}
	reached(stepScript)
	if !res.Booted {
		if strings.HasPrefix(res.Reason, "exit") {
			out.exited = true
			out.steps = append(out.steps, "exit")
			// a script that exits after a download failed (NICo's `chain … || goto failed`)
			// says why in the download, not the exit
			for _, ar := range res.Artifacts {
				if strings.Contains(ar.Kind, "failed") {
					out.failed = stepScript + " (" + ar.Kind + " " + ar.URL + ", then exit)"
				}
			}
			return out
		}
		return fail(res.Step, res.Reason)
	}
	out.kernel, out.cmdline = res.Image, res.Cmdline
	reached(stepBooted)
	return out
}

// embeddedScript is the script built into an iPXE image (its EMBED), which iPXE runs instead of
// asking DHCP for one: the bytes from "#!ipxe" to the string's terminating NUL.
func embeddedScript(img []byte) []byte {
	i := bytes.Index(img, []byte("#!ipxe\n"))
	if i < 0 {
		return nil
	}
	s := img[i:]
	if j := bytes.IndexByte(s, 0); j >= 0 {
		s = s[:j]
	}
	return s
}

// bootParams reads the boot server and file: options 66 and 67, else siaddr and the file
// field, from the ACK, else from the offer.
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

func uuidBytes(u string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(u, "-", ""))
	if err != nil || len(b) != 16 {
		return make([]byte, 16)
	}
	return b
}

// httpClient is an HTTP client whose connections leave from the machine's address.
func (b *machine) httpClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil, DialContext: b.nic.dial, DisableKeepAlives: true}}
}

// get fetches a URL fully from the machine's address, keeping the body when asked.
func (b *machine) get(ctx context.Context, u *url.URL, keep bool) ([]byte, error) {
	if u.Scheme != "http" {
		return nil, fmt.Errorf("%s: only http is implemented (the firmware and iPXE fetch these by http)", u)
	}
	// a setting with spaces (a BlueField's SMBIOS product) expanded into a URL is sent with the
	// spaces encoded; iPXE, which expands a line before splitting it, would cut the URL there
	if strings.Contains(u.RawQuery, " ") {
		c := *u
		c.RawQuery = strings.ReplaceAll(u.RawQuery, " ", "%20")
		u = &c
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "iPXE/1.21.1")
	resp, err := b.httpClient(5 * time.Minute).Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	if keep {
		return io.ReadAll(resp.Body)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	b.fetched(u.String(), n)
	return nil, err
}

// cmdlineArg is a kernel command line's value for key ("machine_id=…").
func cmdlineArg(cmdline, key string) string {
	for _, f := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(f, key+"="); ok {
			return v
		}
	}
	return ""
}

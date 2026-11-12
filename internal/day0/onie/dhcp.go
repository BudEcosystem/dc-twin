package onie

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"

	"github.com/insomniacslk/dhcp/dhcpv4"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// DHCPClient leases an address on the management port and gives it back.
type DHCPClient interface {
	// Lease runs DHCPv4's discover, offer, request and ack with ONIE's options (Modifiers on
	// both the discover and the request), and returns what ONIE reads from the ack.
	Lease(ctx context.Context, mods []dhcpv4.Modifier) (*Lease, error)
	// Release sends DHCPRELEASE for the lease, carrying mods (the client id), as the installed
	// NOS starts with the port to itself and its own DHCP client.
	Release(ctx context.Context, l *Lease, mods []dhcpv4.Modifier) error
}

// NetConfig applies a lease to the management port (busybox udhcpc's default.script: the
// address, the default route) and removes it again (flush, before the NOS boots).
type NetConfig interface {
	Apply(l *Lease) error
	Flush() error
}

// Lease is what ONIE reads from a DHCPv4 ack: busybox udhcpc's environment, which ONIE exports to
// the installer as onie_disco_<name> (patches/busybox/dhcp-additional-options.patch, facts D.2).
type Lease struct {
	Addr   net.IPNet // ip, subnet
	Router net.IP    // router (option 3)
	DNS    []net.IP  // dns (6)
	Domain string    // domain (15)
	// ServerID is serverid (54); SIAddr the BOOTP siaddr.
	ServerID, SIAddr net.IP
	// URL is option 114, default-url: an exact URL (onie_disco_url).
	URL string
	// InstallerURL and UpdaterURL are option 125's ONIE sub-options 1 and 2 (onie_disco_onie_url,
	// import_vendoropts()). Only the installer URL is used for os-install.
	InstallerURL, UpdaterURL string
	// WWWServers are option 72 (wwwsrv), TFTPServers option 150 (tftpsiaddr).
	WWWServers, TFTPServers []net.IP
	// TFTP is option 66 (tftp), a server name or address; Bootfile option 67 (bootfile) and
	// BootFile the BOOTP file field (boot_file).
	TFTP, Bootfile, BootFile string
	// VendorSpecific is option 43, requested by ONIE but parsed by nothing in rootconf/default
	// (UNCONFIRMED 13); it is kept for the log only.
	VendorSpecific []byte

	// ACK is the ack itself, which DHCPRELEASE is built from.
	ACK *dhcpv4.DHCPv4
}

func (l *Lease) describe() string {
	var b strings.Builder
	add := func(k string, v any) { fmt.Fprintf(&b, ", %s %v", k, v) }
	if l.URL != "" {
		add("default-url", l.URL)
	}
	if l.InstallerURL != "" {
		add("installer-url", l.InstallerURL)
	}
	if len(l.WWWServers) > 0 {
		add("www-server", l.WWWServers)
	}
	if l.TFTP != "" {
		add("tftp-server", l.TFTP)
	}
	if l.Bootfile != "" {
		add("bootfile", l.Bootfile)
	}
	if len(l.VendorSpecific) > 0 {
		add("option 43 (unused)", fmt.Sprintf("%d bytes", len(l.VendorSpecific)))
	}
	return b.String()
}

// RequestedOptions is ONIE's parameter request list: busybox udhcpc's defaults (the options its
// common.c flags OPTION_REQ: subnet 1, router 3, dns 6, hostname 12, domain 15, broadcast 28),
// plus what sd_dhcp4 adds with `-O 7 43 54 66 67 72 114 125` (facts D.2).
var RequestedOptions = []uint8{1, 3, 6, 12, 15, 28, 7, 43, 54, 66, 67, 72, 114, 125}

// UserClass is option 77 as udhcpc_args() sends it: the literal string, not RFC 3004's
// length-prefixed form (facts D.1).
const UserClass = "onie_dhcp_user_class"

// Modifiers are the options ONIE's udhcpc_args() puts on its discover and request (facts D.1):
// option 60 `onie_vendor:<platform>`, option 61 `00` then the serial number's bytes, option 77
// the user class, option 125 with OCP's enterprise and the machine, arch and revision
// sub-options, and the parameter request list.
func Modifiers(id nos.Identity, p Platform) []dhcpv4.Modifier {
	codes := make([]dhcpv4.OptionCode, len(RequestedOptions))
	for i, c := range RequestedOptions {
		codes[i] = dhcpv4.GenericOptionCode(c)
	}
	return []dhcpv4.Modifier{
		dhcpv4.WithGeneric(dhcpv4.OptionClassIdentifier, []byte("onie_vendor:"+p.String())),
		dhcpv4.WithGeneric(dhcpv4.OptionClientIdentifier, ClientID(id.Serial)),
		dhcpv4.WithGeneric(dhcpv4.OptionUserClassInformation, []byte(UserClass)),
		dhcpv4.WithGeneric(dhcpv4.OptionVendorIdentifyingVendorSpecific, VIVSO(p)),
		dhcpv4.WithRequestedOptions(codes...),
	}
}

// ClientID is option 61: the type byte 00 followed by the serial number (udhcpc_args() passes
// `00` + hex(onie-sysinfo -s)).
func ClientID(serial string) []byte { return append([]byte{0}, serial...) }

// VIVSO is option 125 as ONIE sends it (RFC 3925): enterprise 42623, then sub-options 3 machine,
// 4 CPU arch and 5 machine revision.
func VIVSO(p Platform) []byte {
	var sub []byte
	for _, s := range []struct {
		code byte
		v    string
	}{{3, p.Machine}, {4, p.Arch}, {5, p.Rev}} {
		sub = append(sub, s.code, byte(len(s.v)))
		sub = append(sub, s.v...)
	}
	b := binary.BigEndian.AppendUint32(nil, IANAEnterprise)
	b = append(b, byte(len(sub)))
	return append(b, sub...)
}

// vivsoSub returns option 125's sub-options for an enterprise.
func vivsoSub(b []byte, enterprise uint32) map[byte]string {
	out := map[byte]string{}
	for len(b) >= 5 {
		ent, n := binary.BigEndian.Uint32(b), int(b[4])
		b = b[5:]
		if n > len(b) {
			return out
		}
		data := b[:n]
		b = b[n:]
		if ent != enterprise {
			continue
		}
		for len(data) >= 2 && int(data[1])+2 <= len(data) {
			out[data[0]] = string(data[2 : 2+int(data[1])])
			data = data[2+int(data[1]):]
		}
	}
	return out
}

func ipList(b []byte) []net.IP {
	var out []net.IP
	for ; len(b) >= 4; b = b[4:] {
		out = append(out, net.IP(append([]byte(nil), b[:4]...)))
	}
	return out
}

// LeaseFromACK reads an ack as udhcpc hands it to ONIE.
func LeaseFromACK(ack *dhcpv4.DHCPv4) *Lease {
	o := ack.Options
	l := &Lease{
		Addr:     net.IPNet{IP: ack.YourIPAddr, Mask: ack.SubnetMask()},
		DNS:      ack.DNS(),
		Domain:   ack.DomainName(),
		ServerID: ack.ServerIdentifier(),
		SIAddr:   ack.ServerIPAddr,
		URL:      string(o.Get(dhcpv4.OptionURL)),
		TFTP:     ack.TFTPServerName(),
		Bootfile: ack.BootFileNameOption(),
		BootFile: ack.BootFileName,
		ACK:      ack,
	}
	if l.Addr.Mask == nil {
		l.Addr.Mask = l.Addr.IP.DefaultMask()
	}
	if r := ack.Router(); len(r) > 0 {
		l.Router = r[0]
	}
	l.WWWServers = ipList(o.Get(dhcpv4.OptionDefaultWorldWideWebServer))
	l.TFTPServers = ipList(o.Get(dhcpv4.GenericOptionCode(150)))
	l.VendorSpecific = o.Get(dhcpv4.OptionVendorSpecificInformation)
	vs := vivsoSub(o.Get(dhcpv4.OptionVendorIdentifyingVendorSpecific), IANAEnterprise)
	l.InstallerURL, l.UpdaterURL = vs[1], vs[2]
	return l
}

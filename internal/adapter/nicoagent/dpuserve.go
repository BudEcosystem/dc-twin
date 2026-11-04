package nicoagent

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
)

// A BlueField's forge-dpu-agent runs NICo's forge-dhcp-server on the DPU, which answers the
// host's DHCP on the host-facing ports from the managed host's network configuration: the
// address NICo gave the host's interface (the admin interface until an instance's tenant
// interfaces replace it), its prefix and gateway, and a boot URI for an HTTP boot client (the
// interface's own, else NICo's iPXE for the host's architecture on NICo's PXE service), as the
// central dhcp-server would. The stand-in's DPU does the same for its host's stand-in: it is the
// netsim.PortRelay of the host's switch port, so the port's gateway is the interface's gateway
// and the routed site network carries the host's address (its DHCPACK attaches it, D48).

// setHostConfig records the network configuration the agent fetched, and NICo's PXE service.
func (b *dpu) setHostConfig(cfg obj, pxeURL string) {
	b.hostMu.Lock()
	b.hostCfg, b.pxeURL = cfg, pxeURL
	b.hostMu.Unlock()
}

// hostIface is the interface the configuration gives the host: admin, else the first tenant one.
func (b *dpu) hostIface() (obj, string, error) {
	b.hostMu.Lock()
	cfg, pxe := b.hostCfg, b.pxeURL
	b.hostMu.Unlock()
	if cfg == nil || !b.agentUp.Load() {
		return nil, "", errors.New("the DPU's agent is not running: no DHCP server answers the host")
	}
	if use, _ := cfg["use_admin_network"].(bool); use {
		if a, ok := cfg["admin_interface"].(map[string]any); ok {
			return a, pxe, nil
		}
		return nil, "", errors.New("the network configuration has no admin interface")
	}
	if ts, ok := cfg["tenant_interfaces"].([]any); ok && len(ts) > 0 {
		if t, ok := ts[0].(map[string]any); ok {
			return t, pxe, nil
		}
	}
	return nil, "", errors.New("the network configuration has no interface for the host")
}

// hostDHCP plugs the host's port into the DPU's DHCP server, or says why it cannot yet (netsim
// retries the plug at the host's next DHCP).
func (b *dpu) hostDHCP() (netsim.PortRelay, error) {
	iface, _, err := b.hostIface()
	if err != nil {
		return nil, err
	}
	_, gw, pfx, err := ifaceV4(iface)
	if err != nil {
		return nil, err
	}
	mac := append(net.HardwareAddr(nil), b.mac...)
	mac[0] |= 0x02 // the DPU's host-facing router port: a locally administered MAC of its own
	mac[5] ^= 0xff
	return &hostServer{b: b, gw: netip.PrefixFrom(gw, pfx.Bits()), gwMAC: mac, replies: make(chan []byte, 16), closed: make(chan struct{})}, nil
}

// ifaceV4 is an interface's IPv4 address, gateway and segment prefix: from its addresses
// entries (an IPv4 one: ip, a gateway CIDR, the segment's prefix), which NICo writes, else from
// the deprecated flat fields older NICo wrote.
func ifaceV4(iface obj) (ip, gw netip.Addr, pfx netip.Prefix, err error) {
	gwStr, ipStr, pStr := str(iface, "gateway"), str(iface, "ip"), firstOf(str(iface, "prefix"), str(iface, "interface_prefix"))
	if as, ok := iface["addresses"].([]any); ok {
		for _, a := range as {
			am, _ := a.(map[string]any)
			if am == nil || (str(am, "address_family") != "" && str(am, "address_family") != "ADDRESS_FAMILY_V4") || str(am, "ip") == "" {
				continue
			}
			gwStr, ipStr, pStr = str(am, "gateway"), str(am, "ip"), firstOf(str(am, "prefix"), str(am, "interface_prefix"))
			break
		}
	}
	if ip, err = netip.ParseAddr(ipStr); err != nil || !ip.Is4() {
		return ip, gw, pfx, fmt.Errorf("the host's interface has no IPv4 address (%q)", ipStr)
	}
	// the gateway is written as a CIDR ("192.168.252.1/24") or a bare address
	if p, err := netip.ParsePrefix(gwStr); err == nil {
		gw, pfx = p.Addr(), p.Masked()
	} else if gw, err = netip.ParseAddr(gwStr); err != nil {
		return ip, gw, pfx, fmt.Errorf("the host's interface has no gateway (%q)", gwStr)
	}
	if p, err := netip.ParsePrefix(pStr); err == nil && p.Bits() < 32 {
		pfx = p.Masked()
	}
	if !pfx.IsValid() {
		pfx = netip.PrefixFrom(gw, 24).Masked()
	}
	return ip, gw, pfx, nil
}

type hostServer struct {
	b       *dpu
	gw      netip.Prefix
	gwMAC   net.HardwareAddr
	replies chan []byte
	closed  chan struct{}
	once    sync.Once
}

func (s *hostServer) Send(payload []byte) error {
	m, err := dhcpv4.FromBytes(payload)
	if err != nil {
		return err
	}
	iface, pxe, err := s.b.hostIface()
	if err != nil {
		return nil // nothing answers: the host's DHCP times out
	}
	ip, _, pfx, err := ifaceV4(iface)
	if err != nil {
		return nil
	}
	var t dhcpv4.MessageType
	switch m.MessageType() {
	case dhcpv4.MessageTypeDiscover:
		t = dhcpv4.MessageTypeOffer
	case dhcpv4.MessageTypeRequest:
		t = dhcpv4.MessageTypeAck
		if req := m.RequestedIPAddress(); req != nil && !req.Equal(net.IP(ip.AsSlice())) {
			t = dhcpv4.MessageTypeNak
		}
	default:
		return nil
	}
	gw := s.gw.Addr()
	mods := []dhcpv4.Modifier{dhcpv4.WithMessageType(t),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.IP(gw.AsSlice())))}
	if t != dhcpv4.MessageTypeNak {
		mods = append(mods, dhcpv4.WithYourIP(net.IP(ip.AsSlice())),
			dhcpv4.WithOption(dhcpv4.OptSubnetMask(net.CIDRMask(pfx.Bits(), 32))),
			dhcpv4.WithOption(dhcpv4.OptRouter(net.IP(gw.AsSlice()))),
			dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(time.Hour)))
		pxeHost := ""
		if u, err := url.Parse(pxe); err == nil {
			pxeHost = u.Hostname()
		}
		if pxeHost != "" {
			mods = append(mods, dhcpv4.WithServerIP(net.ParseIP(pxeHost))) // next-server: iPXE's boot service
		}
		user := string(m.Options.Get(dhcpv4.OptionUserClassInformation))
		if strings.HasPrefix(m.ClassIdentifier(), "HTTPClient") && user != "iPXE" {
			boot := str(iface, "booturl")
			if boot == "" && pxeHost != "" {
				boot = fmt.Sprintf("http://%s:8080/public/blobs/internal/%s/ipxe.efi", pxeHost, s.b.hostArch())
			}
			if boot != "" {
				mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier("HTTPClient")), dhcpv4.WithOption(dhcpv4.OptBootFileName(boot)))
			}
		}
	}
	r, err := dhcpv4.NewReplyFromRequest(m, mods...)
	if err != nil {
		return err
	}
	select {
	case s.replies <- r.ToBytes():
	case <-s.closed:
	}
	return nil
}

// hostArch is the architecture of the DPU's host, for its boot URI.
func (b *dpu) hostArch() string {
	if b.dev.Parent != nil {
		return archOf(b.dev.Parent)
	}
	return "aarch64"
}

func (s *hostServer) Receive(buf []byte) (int, error) {
	select {
	case r := <-s.replies:
		return copy(buf, r), nil
	case <-s.closed:
		return 0, errCableClosed
	}
}

func (s *hostServer) Gateway() (netip.Prefix, net.HardwareAddr) { return s.gw, s.gwMAC }

func (s *hostServer) Server() string {
	return "the DPU's forge-dhcp-server (" + s.b.path + ", impersonated)"
}

func (s *hostServer) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

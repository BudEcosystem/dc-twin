//go:build linux

package onie

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/vishvananda/netlink"
)

// System returns the real DHCP client and network configuration for an interface: nclient4 on a
// raw socket (the port has no address yet, as udhcpc's has none), and netlink for what
// udhcpc's default.script does with the lease.
func System(iface string) (DHCPClient, NetConfig, error) {
	if _, err := net.InterfaceByName(iface); err != nil {
		return nil, nil, err
	}
	return &sysDHCP{iface: iface}, &sysNet{iface: iface}, nil
}

type sysDHCP struct{ iface string }

// Lease is sd_dhcp4's `udhcpc -t 2 -T 2 -n` (facts D.2): two discovers two seconds apart, then
// give up for this pass.
func (d *sysDHCP) Lease(ctx context.Context, mods []dhcpv4.Modifier) (*Lease, error) {
	c, err := nclient4.New(d.iface, nclient4.WithTimeout(2*time.Second), nclient4.WithRetry(2))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	l, err := c.Request(cctx, mods...)
	if err != nil {
		return nil, err
	}
	return LeaseFromACK(l.ACK), nil
}

// Release sends DHCPRELEASE unicast from the leased address to the server, as RFC 2131 4.4.4
// has it, while the address is still on the port.
func (d *sysDHCP) Release(_ context.Context, l *Lease, mods []dhcpv4.Modifier) error {
	if l == nil || l.ACK == nil {
		return nil
	}
	if l.ServerID == nil {
		return fmt.Errorf("the lease names no server")
	}
	req, err := dhcpv4.NewReleaseFromACK(l.ACK, mods...)
	if err != nil {
		return err
	}
	c, err := net.DialUDP("udp4", &net.UDPAddr{IP: l.Addr.IP, Port: nclient4.ClientPort}, &net.UDPAddr{IP: l.ServerID, Port: nclient4.ServerPort})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write(req.ToBytes())
	return err
}

type sysNet struct{ iface string }

// Apply is udhcpc's default.script on bound: the port up, its old addresses gone, the leased
// address with its broadcast, and the default route through the lease's router.
func (n *sysNet) Apply(l *Lease) error {
	link, err := netlink.LinkByName(n.iface)
	if err != nil {
		return err
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return err
	}
	if err := n.Flush(); err != nil {
		return err
	}
	ip4 := l.Addr.IP.To4()
	if ip4 == nil {
		return fmt.Errorf("lease %s is not IPv4", l.Addr.IP)
	}
	brd := make(net.IP, 4)
	for i := range brd {
		brd[i] = ip4[i] | ^l.Addr.Mask[len(l.Addr.Mask)-4+i]
	}
	ipn := l.Addr
	if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: &ipn, Broadcast: brd}); err != nil {
		return fmt.Errorf("address %s: %w", l.Addr.String(), err)
	}
	if l.Router != nil {
		if err := netlink.RouteReplace(&netlink.Route{LinkIndex: link.Attrs().Index, Gw: l.Router}); err != nil {
			return fmt.Errorf("default route via %s: %w", l.Router, err)
		}
	}
	return nil
}

// Flush removes the port's IPv4 routes and addresses, as udhcpc's script does on deconfig, so the
// NOS starts with the port as the factory left it.
func (n *sysNet) Flush() error {
	link, err := netlink.LinkByName(n.iface)
	if err != nil {
		return err
	}
	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for i := range routes {
		_ = netlink.RouteDel(&routes[i])
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for i := range addrs {
		if err := netlink.AddrDel(link, &addrs[i]); err != nil {
			return fmt.Errorf("removing %s: %w", addrs[i].IPNet, err)
		}
	}
	return nil
}

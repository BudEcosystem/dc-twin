//go:build linux

// Package dhcpc is the twin's DHCP client for a switch's management port where its NOS image
// has none: docker-sonic-vs leaves isc-dhcp-client out (Q25, read 2026-11-11 from the pinned
// image), while a SONiC switch's interfaces-config runs dhclient on eth0. It leases as dhclient
// would for SONiC's ZTP (docs/reviews/2026-11-11-day0-vendor-facts.md, E): user class
// SONiC-ZTP, client identifier SONiC##<product>##<serial>, options 67 and 239 requested, and
// writes the lease as dhclient's exit hooks see it (reason and new_* variables, shell-quoted) to
// an env file the ZTP client reads, the same file a generic/nvue-frr switch's dhclient hook
// writes. It keeps the lease: renews at T1, and starts again when the lease is lost or the port
// loses carrier.
package dhcpc

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/vishvananda/netlink"
)

// Main is `dctwin-day0 dhcp [flags]`.
func Main(args []string) int {
	fs := flag.NewFlagSet("dhcp", flag.ContinueOnError)
	iface := fs.String("i", "eth0", "the interface")
	envFile := fs.String("env", "/run/dctwin/dhcp-eth0.env", "where the lease is written, as dhclient's hooks see it")
	identity := fs.String("identity", "/etc/dctwin/identity.json", "the switch's identity (nos.Identity)")
	userClass := fs.String("user-class", "SONiC-ZTP", "option 77")
	metric := fs.Int("metric", 202, "the default route's metric (a fabric's own default wins)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var id struct{ Serial, Model, Platform string }
	if b, err := os.ReadFile(*identity); err == nil {
		_ = json.Unmarshal(b, &id)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	c := &client{iface: *iface, env: *envFile, metric: *metric,
		mods: []dhcpv4.Modifier{
			dhcpv4.WithUserClass(*userClass, false),
			dhcpv4.WithOption(dhcpv4.OptClientIdentifier(append([]byte{0}, []byte("SONiC##"+product(id.Model, id.Platform)+"##"+id.Serial)...))),
			dhcpv4.WithRequestedOptions(dhcpv4.OptionSubnetMask, dhcpv4.OptionRouter, dhcpv4.OptionDomainNameServer, dhcpv4.OptionDomainName,
				dhcpv4.OptionHostName, dhcpv4.OptionNTPServers, dhcpv4.OptionBootfileName, dhcpv4.OptionTFTPServerName, dhcpv4.GenericOptionCode(239)),
		}}
	if err := c.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "dhcp:", err)
		return 1
	}
	return 0
}

// product is SONiC's product name in its client identifier: the platform's, as SONiC reads its
// EEPROM's product name; the model when there is no platform.
func product(model, platform string) string {
	if platform != "" {
		return platform
	}
	return strings.ReplaceAll(model, " ", "-")
}

type client struct {
	iface  string
	env    string
	metric int
	mods   []dhcpv4.Modifier
}

func (c *client) run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := c.waitCarrier(ctx); err != nil {
			return err
		}
		lease, nc, err := c.acquire(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dhcp: no lease yet:", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
			continue
		}
		if err := c.bind(lease.ACK, "BOUND"); err != nil {
			fmt.Fprintln(os.Stderr, "dhcp: bind:", err)
		}
		c.keep(ctx, nc, lease)
		nc.Close()
		c.unbind(lease.ACK)
	}
	return ctx.Err()
}

func (c *client) waitCarrier(ctx context.Context) error {
	for {
		if l, err := netlink.LinkByName(c.iface); err == nil {
			if l.Attrs().Flags&net.FlagUp == 0 {
				_ = netlink.LinkSetUp(l)
			}
			if l.Attrs().RawFlags&syscall.IFF_RUNNING != 0 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (c *client) acquire(ctx context.Context) (*nclient4.Lease, *nclient4.Client, error) {
	nc, err := nclient4.New(c.iface, nclient4.WithTimeout(5*time.Second), nclient4.WithRetry(3))
	if err != nil {
		return nil, nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lease, err := nc.Request(cctx, c.mods...)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	return lease, nc, nil
}

// keep renews the lease at T1 until it is lost, the port loses carrier, or ctx ends.
func (c *client) keep(ctx context.Context, nc *nclient4.Client, lease *nclient4.Lease) {
	for {
		t1 := lease.ACK.IPAddressRenewalTime(0)
		if t1 <= 0 {
			t1 = lease.ACK.IPAddressLeaseTime(time.Hour) / 2
		}
		deadline := time.Now().Add(t1)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				_ = nc.Release(lease)
				return
			case <-time.After(time.Second):
			}
			if l, err := netlink.LinkByName(c.iface); err != nil || l.Attrs().RawFlags&syscall.IFF_RUNNING == 0 {
				return // carrier lost: lease again when it is back (INIT-REBOOT in dhclient's terms)
			}
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		renewed, err := nc.Renew(cctx, lease, c.mods...)
		cancel()
		if err != nil {
			return
		}
		lease = renewed
		_ = c.bind(lease.ACK, "RENEW")
	}
}

// bind puts the lease on the interface and writes the env file.
func (c *client) bind(ack *dhcpv4.DHCPv4, reason string) error {
	l, err := netlink.LinkByName(c.iface)
	if err != nil {
		return err
	}
	mask := ack.SubnetMask()
	if mask == nil {
		mask = net.CIDRMask(24, 32)
	}
	addr := &netlink.Addr{IPNet: &net.IPNet{IP: ack.YourIPAddr, Mask: mask}}
	if err := netlink.AddrReplace(l, addr); err != nil {
		return fmt.Errorf("address %s: %w", addr, err)
	}
	if rs := ack.Router(); len(rs) > 0 {
		_ = netlink.RouteReplace(&netlink.Route{LinkIndex: l.Attrs().Index, Gw: rs[0], Priority: c.metric})
	}
	return writeEnv(c.env, envOf(ack, c.iface, reason))
}

func (c *client) unbind(ack *dhcpv4.DHCPv4) {
	if l, err := netlink.LinkByName(c.iface); err == nil {
		mask := ack.SubnetMask()
		if mask == nil {
			mask = net.CIDRMask(24, 32)
		}
		_ = netlink.AddrDel(l, &netlink.Addr{IPNet: &net.IPNet{IP: ack.YourIPAddr, Mask: mask}})
	}
	_ = writeEnv(c.env, map[string]string{"reason": "EXPIRE", "interface": c.iface})
}

// envOf is the lease as dhclient's exit hooks see it (dhclient-script(8)): reason, interface and
// new_* variables named as dhclient names the options, with option 67 also as SONiC's ZTP
// names it and option 239 under both NOSes' names.
func envOf(ack *dhcpv4.DHCPv4, iface, reason string) map[string]string {
	env := map[string]string{"reason": reason, "interface": iface, "new_ip_address": ack.YourIPAddr.String()}
	if m := ack.SubnetMask(); m != nil {
		env["new_subnet_mask"] = net.IP(m).String()
	}
	join := func(ips []net.IP) string {
		var s []string
		for _, ip := range ips {
			s = append(s, ip.String())
		}
		return strings.Join(s, " ")
	}
	if r := ack.Router(); len(r) > 0 {
		env["new_routers"] = join(r)
	}
	if d := ack.DNS(); len(d) > 0 {
		env["new_domain_name_servers"] = join(d)
	}
	if n := ack.NTPServers(); len(n) > 0 {
		env["new_ntp_servers"] = join(n)
	}
	if h := ack.HostName(); h != "" {
		env["new_host_name"] = h
	}
	if d := ack.DomainName(); d != "" {
		env["new_domain_name"] = d
	}
	if t := ack.IPAddressLeaseTime(0); t > 0 {
		env["new_dhcp_lease_time"] = strconv.Itoa(int(t.Seconds()))
	}
	boot := ack.BootFileNameOption()
	if boot == "" {
		boot = strings.TrimRight(ack.BootFileName, "\x00")
	}
	if boot != "" {
		env["new_filename"], env["new_bootfile_name"], env["new_ztp_json_url"] = boot, boot, boot
	}
	if v := ack.Options.Get(dhcpv4.GenericOptionCode(239)); len(v) > 0 {
		env["new_ztp_provisioning_script_url"], env["new_cumulus_provision_url"] = string(v), string(v)
	}
	if s := ack.ServerIdentifier(); s != nil {
		env["new_dhcp_server_identifier"] = s.String()
	}
	return env
}

func writeEnv(path string, env map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var keys []string
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s='%s'\n", k, strings.ReplaceAll(env[k], "'", `'\''`))
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

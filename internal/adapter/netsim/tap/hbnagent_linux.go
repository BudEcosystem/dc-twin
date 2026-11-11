//go:build linux

package tap

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
)

// The emulated BlueField's management VRF (D71): its DPU OS's, holding oob_net0, through which
// NICo's agent (which binds its sockets to mgmt) reaches NICo.
const (
	mgmtVRF   = "mgmt"
	mgmtTable = 1001
	// hbnDNS is the site DNS the agent is given, on oob_net0, answered by the twin; apiProxy is
	// the address nico-api's name resolves to when its URL names it by name.
	hbnDNS   = "169.254.71.53"
	apiProxy = "169.254.71.80"
	// hbnSupervisor is NICo's programs' supervisord configuration in the emulated HBN
	hbnSupervisor = "/etc/supervisor/conf.d/forge.conf"
)

// hbnAgent is a running hand-off: the listeners on oob_net0 that carry the agent's connections.
type hbnAgent struct {
	mu   sync.Mutex
	lns  []io.Closer
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (h *hbnAgent) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.lns {
		l.Close()
	}
	h.lns = nil
}

var _ netsim.HBNAgents = (*Substrate)(nil)

// StartHBNAgent starts NICo's forge-dpu-agent and forge-dhcp-server in an emulated BlueField (D71),
// as DPF runs them: containerized, configuring HBN through NVUE's REST API, under HBN's
// supervisord, from the binaries the estate mounts (spec.adapters.network.nos.hbnBinaries). The
// DPU's management VRF holds oob_net0 with nico-api's and the PXE service's addresses and a site
// DNS of the twin's; each connection the agent opens there is carried over the DPU's own
// out-of-band path (a.Dial: the stand-in's NIC at the BlueField's switch port, D58), so NICo sees
// the DPU as it would. The agent registers itself (DiscoverMachine) with the DPU's identity.
func (s *Substrate) StartHBNAgent(path string, a netsim.HBNAgent) error {
	sw := s.realSwitch(path)
	if sw == nil || sw.plan.NOS.Name != "emulated/hbn" {
		return fmt.Errorf("%s is not an emulated BlueField", path)
	}
	if out, code, err := s.docker.Exec(s.ctx, sw.c.ID, "test", "-x", "/opt/forge/bin/forge-dpu-agent"); err != nil || code != 0 {
		return fmt.Errorf("%s: NICo's agent is not in /opt/forge/bin (spec.adapters.network.nos.hbnBinaries): %v %s", path, err, out)
	}
	u, err := url.Parse(a.APIURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s: nico-api's URL %q", path, a.APIURL)
	}
	apiHost, apiPort, err := net.SplitHostPort(u.Host)
	if err != nil {
		apiHost, apiPort = u.Host, "443"
	}
	names := map[string]netip.Addr{}
	apiIP, err := netip.ParseAddr(apiHost)
	if err != nil {
		apiIP = netip.MustParseAddr(apiProxy)
		names[strings.ToLower(apiHost)] = apiIP
	}
	names["carbide-api.forge"] = apiIP
	pxeIP, err := netip.ParseAddr(a.PXEHost)
	if err != nil {
		pxeIP = apiIP
	}
	names["carbide-pxe.forge"] = pxeIP
	h := &hbnAgent{dial: a.Dial}
	// a restart: the running agent's listeners go first, as the new ones take their addresses
	s.mu.Lock()
	if old := s.agents[path]; old != nil {
		old.close()
		delete(s.agents, path)
	}
	s.mu.Unlock()
	if err := s.mgmtVRF(sw, []netip.Addr{apiIP, pxeIP, netip.MustParseAddr(hbnDNS)}); err != nil {
		return fmt.Errorf("%s: its management VRF: %w", path, err)
	}
	// nico-api, carried over the DPU's own path to where NICo's image found it
	upstream := net.JoinHostPort(apiHost, apiPort)
	if err := inNS(sw.ns, func() error {
		ln, err := mgmtListen("tcp", netip.AddrPortFrom(apiIP, portOf(apiPort)).String())
		if err != nil {
			return err
		}
		h.lns = append(h.lns, ln)
		go h.forward(s.ctx, ln, upstream)
		pc, err := mgmtListenPacket(net.JoinHostPort(hbnDNS, "53"))
		if err != nil {
			return err
		}
		h.lns = append(h.lns, pc)
		go serveNames(pc, names)
		return nil
	}); err != nil {
		h.close()
		return fmt.Errorf("%s: oob_net0: %w", path, err)
	}
	conf := fmt.Sprintf(`[forge-system]
api-server = "%s"
root-ca = "/opt/forge/forge_root.pem"

[period]
main-loop-active-secs = 5
main-loop-idle-secs = 30
network-config-fetch-secs = 30
version-check-secs = 1800
`, a.APIURL)
	sv := fmt.Sprintf(`[program:forge-dhcp-server]
command=/opt/forge/bin/forge-dhcp-server --mode dpu --grpc-listen-addr 127.0.0.1:50051
autorestart=true
redirect_stderr=true
stdout_logfile=/var/log/forge-dhcp-server.log

[program:forge-dpu-agent]
command=/opt/forge/bin/forge-dpu-agent --config-path /etc/forge/agent.toml run --dhcp-grpc-server http://127.0.0.1:50051 --skip-upgrade-check
environment=AGENT_PLATFORM_TYPE="containerized",HBN_CONFIG_MODE="nvue-rest",NVUE_HTTPS_ADDRESS="127.0.0.1:8765",NVUE_USERNAME="cumulus",NVUE_PASSWORD="%s",RUST_LOG="info"
autorestart=true
redirect_stderr=true
stdout_logfile=/var/log/forge-dpu-agent.log
`, sw.nvuePassword)
	out, code, err := s.docker.Exec(s.ctx, sw.c.ID, "sh", "-c", hbnAgentSetup, "sh", string(a.RootCA), string(a.Discovery), hbnDNS, conf, sv)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d: %s", code, strings.TrimSpace(out))
	}
	if err != nil {
		h.close()
		return fmt.Errorf("%s: starting NICo's agent: %w", path, err)
	}
	s.mu.Lock()
	if s.agents == nil {
		s.agents = map[string]*hbnAgent{}
	}
	s.agents[path] = h
	s.mu.Unlock()
	s.log.Info("tap: NICo's DPU agent runs in the emulated HBN (D71)", "dpu", path, "api", a.APIURL)
	return nil
}

// StopHBNAgent stops NICo's agent and DHCP server in an emulated BlueField.
func (s *Substrate) StopHBNAgent(path string) {
	sw := s.realSwitch(path)
	s.mu.Lock()
	h := s.agents[path]
	delete(s.agents, path)
	s.mu.Unlock()
	if h != nil {
		h.close()
	}
	if sw != nil {
		_, _, _ = s.docker.Exec(s.ctx, sw.c.ID, "sh", "-c", "rm -f "+hbnSupervisor+"; supervisorctl -c /etc/supervisor/supervisord.conf update")
	}
}

// hbnAgentSetup writes what DPF's init container gives the agent (the site's root CA, the DPU's
// DiscoveryInfo, the nameservers), its configuration and its programs for supervisord.
const hbnAgentSetup = `set -e
mkdir -p /opt/forge /data/lldp /etc/forge /var/support/forge-dhcp/conf /var/support/forge-dhcp/logs
printf '%s' "$1" > /opt/forge/forge_root.pem
printf '%s' "$2" > /data/hw_output.json
printf '%s\n' "$3" > /data/nameservers
printf 'nameserver %s\n' "$3" > /etc/resolv.conf
printf '%s' "$4" > /etc/forge/agent.toml
printf '%s' "$5" > ` + hbnSupervisor + `
supervisorctl -c /etc/supervisor/supervisord.conf update
`

// mgmtVRF gives the DPU's namespace its management VRF with oob_net0 holding addresses.
func (s *Substrate) mgmtVRF(sw *nosSwitch, addrs []netip.Addr) error {
	h, err := netlink.NewHandleAt(sw.ns)
	if err != nil {
		return err
	}
	defer h.Close()
	vrf, err := h.LinkByName(mgmtVRF)
	if err != nil {
		if err := h.LinkAdd(&netlink.Vrf{LinkAttrs: netlink.LinkAttrs{Name: mgmtVRF}, Table: mgmtTable}); err != nil {
			return err
		}
		if vrf, err = h.LinkByName(mgmtVRF); err != nil {
			return err
		}
	}
	if err := h.LinkSetUp(vrf); err != nil {
		return err
	}
	oob, err := h.LinkByName("oob_net0")
	if err != nil {
		if err := h.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "oob_net0"}}); err != nil {
			return err
		}
		if oob, err = h.LinkByName("oob_net0"); err != nil {
			return err
		}
	}
	if err := h.LinkSetMasterByIndex(oob, vrf.Attrs().Index); err != nil {
		return err
	}
	for _, a := range addrs {
		ad := &netlink.Addr{IPNet: &net.IPNet{IP: a.AsSlice(), Mask: net.CIDRMask(32, 32)}}
		if err := h.AddrReplace(oob, ad); err != nil {
			return err
		}
	}
	return h.LinkSetUp(oob)
}

func portOf(p string) uint16 {
	var n uint16
	_, _ = fmt.Sscan(p, &n)
	return n
}

// bindMgmt binds a socket to the management VRF.
func bindMgmt(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, mgmtVRF)
	}); err != nil {
		return err
	}
	return serr
}

func mgmtListen(network, addr string) (net.Listener, error) {
	lc := net.ListenConfig{Control: bindMgmt}
	return lc.Listen(context.Background(), network, addr)
}

func mgmtListenPacket(addr string) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: bindMgmt}
	return lc.ListenPacket(context.Background(), "udp", addr)
}

// forward carries each connection made to a listener to upstream over the DPU's own path.
func (h *hbnAgent) forward(ctx context.Context, ln net.Listener, upstream string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			u, err := h.dial(ctx, "tcp", upstream)
			if err != nil {
				return
			}
			defer u.Close()
			done := make(chan struct{}, 2)
			go func() { _, _ = io.Copy(u, c); done <- struct{}{} }()
			go func() { _, _ = io.Copy(c, u); done <- struct{}{} }()
			<-done
		}()
	}
}

// serveNames answers A queries for the names given, NXDOMAIN for others: the site DNS the agent
// resolves carbide-pxe.forge (and nico-api's name) through.
func serveNames(c net.PacketConn, names map[string]netip.Addr) {
	buf := make([]byte, 1500)
	for {
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			return
		}
		if out := dnsAnswer(buf[:n], names); out != nil {
			_, _ = c.WriteTo(out, from)
		}
	}
}

// dnsAnswer is the response to one DNS query, nil for a packet that is not one.
func dnsAnswer(q []byte, names map[string]netip.Addr) []byte {
	if len(q) < 12 || q[2]&0x80 != 0 || binary.BigEndian.Uint16(q[4:6]) != 1 {
		return nil
	}
	var labels []string
	i := 12
	for i < len(q) && q[i] != 0 {
		l := int(q[i])
		if l&0xc0 != 0 || i+1+l > len(q) {
			return nil
		}
		labels = append(labels, string(q[i+1:i+1+l]))
		i += 1 + l
	}
	if i+5 > len(q) {
		return nil
	}
	end := i + 5
	qtype := binary.BigEndian.Uint16(q[i+1 : i+3])
	addr, known := names[strings.ToLower(strings.Join(labels, "."))]
	flags := uint16(0x8180)
	if !known {
		flags |= 3 // NXDOMAIN
	}
	answers := 0
	if known && qtype == 1 && addr.Is4() {
		answers = 1
	}
	out := append([]byte{q[0], q[1]}, 0, 0, 0, 1, 0, byte(answers), 0, 0, 0, 0)
	binary.BigEndian.PutUint16(out[2:4], flags)
	out = append(out, q[12:end]...)
	if answers == 1 {
		a := addr.As4()
		out = append(out, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, a[0], a[1], a[2], a[3])
	}
	return out
}

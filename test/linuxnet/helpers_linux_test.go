//go:build linux

package linuxnet

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"

	"github.com/BudEcosystem/dc-twin/internal/adapter/dhcp"
	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim/tap"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/redfish"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// need skips unless this is a root Linux run that asked for these tests.
func need(t *testing.T) {
	t.Helper()
	if os.Getenv("DCTWIN_LINUXNET") == "" {
		t.Skip("DCTWIN_LINUXNET is not set: run scripts/linux-substrate-e2e.sh (a privileged Linux container)")
	}
	if os.Geteuid() != 0 {
		t.Skip("the TAP substrate needs root")
	}
}

func loadEstate(t *testing.T, file string, subst map[string]string) *schema.Estate {
	t.Helper()
	b, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for k, v := range subst {
		s = strings.ReplaceAll(s, k, v)
	}
	var est schema.Estate
	if err := schema.Decode([]byte(s), &est); err != nil {
		t.Fatal(err)
	}
	return &est
}

// up brings an estate up with the DHCP and Redfish adapters on the TAP substrate.
func up(t *testing.T, est *schema.Estate) (*twin.Twin, *tap.Substrate, *dhcp.Adapter) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	lvl := slog.LevelWarn
	if os.Getenv("DCTWIN_DEBUG") != "" {
		lvl = slog.LevelDebug
	}
	start := time.Now()
	tw, err := twin.UpEstate(est, "", reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{"dhcp", "redfish"},
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	sub, ok := tw.Net.L2.(*tap.Substrate)
	if !ok {
		t.Fatalf("the substrate is %T, not tap", tw.Net.L2)
	}
	t.Logf("up in %s on substrate %s (netns %s)", time.Since(start).Round(time.Millisecond), tw.Net.Substrate(), sub.Netns())
	return tw, sub, tw.Adapter("dhcp").(*dhcp.Adapter)
}

func waitLeased(t *testing.T, a *dhcp.Adapter, want int, timeout time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		if n := len(a.Leases()); n >= want {
			return time.Since(start)
		}
		if time.Since(start) > timeout {
			t.Fatalf("%d of %d BMCs leased after %s", len(a.Leases()), want, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// lease is a BMC's committed lease, as the model holds it.
func leaseOf(tw *twin.Twin, path string) model.Lease {
	var l model.Lease
	tw.Engine.Read(func(m *model.Model) {
		if d := m.DeviceBy[path]; d != nil && d.State.Lease != nil {
			l = *d.State.Lease
		}
	})
	return l
}

func device(tw *twin.Twin, path string) *model.Device {
	var d *model.Device
	tw.Engine.Read(func(m *model.Model) { d = m.DeviceBy[path] })
	return d
}

// probe is a network namespace standing in for a client on a twin network: a kernel stack with
// its own ARP table, separate from everything the twin runs.
type probe struct {
	name string
	ns   netns.NsHandle
	h    *netlink.Handle
}

func newProbe(t *testing.T, name string) *probe {
	t.Helper()
	_ = netns.DeleteNamed(name)
	errc := make(chan error, 1)
	p := &probe{name: name}
	go func() {
		runtime.LockOSThread()
		orig, err := netns.Get()
		if err != nil {
			errc <- err
			return
		}
		ns, err := netns.NewNamed(name)
		if err == nil {
			p.ns = ns
		}
		if serr := netns.Set(orig); serr != nil {
			errc <- serr
			return
		}
		runtime.UnlockOSThread()
		errc <- err
	}()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	h, err := netlink.NewHandleAt(p.ns)
	if err != nil {
		t.Fatal(err)
	}
	p.h = h
	if lo, err := h.LinkByName("lo"); err == nil {
		_ = h.LinkSetUp(lo)
	}
	t.Cleanup(func() {
		h.Close()
		p.ns.Close()
		_ = netns.DeleteNamed(name)
	})
	return p
}

// cableToBridge plugs the probe into a bridge of the twin's namespace with an address.
func (p *probe) cableToBridge(t *testing.T, twinNS, bridge, ifname, addr string) {
	t.Helper()
	tns, err := netns.GetFromName(twinNS)
	if err != nil {
		t.Fatal(err)
	}
	defer tns.Close()
	th, err := netlink.NewHandleAt(tns)
	if err != nil {
		t.Fatal(err)
	}
	defer th.Close()
	peer := "x" + ifname
	if err := th.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: peer, MTU: 1500}, PeerName: ifname, PeerNamespace: netlink.NsFd(int(p.ns))}); err != nil {
		t.Fatal(err)
	}
	br, err := th.LinkByName(bridge)
	if err != nil {
		t.Fatal(err)
	}
	pl, _ := th.LinkByName(peer)
	if err := th.LinkSetMaster(pl, br); err != nil {
		t.Fatal(err)
	}
	_ = th.LinkSetUp(pl)
	l, err := p.h.LinkByName(ifname)
	if err != nil {
		t.Fatal(err)
	}
	a, err := netlink.ParseAddr(addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.h.AddrAdd(l, a); err != nil {
		t.Fatal(err)
	}
	if err := p.h.LinkSetUp(l); err != nil {
		t.Fatal(err)
	}
}

// dial opens a TCP connection from inside the probe's namespace.
func (p *probe) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := netns.Get()
		if err != nil {
			ch <- res{nil, err}
			return
		}
		defer orig.Close()
		if err := netns.Set(p.ns); err != nil {
			ch <- res{nil, err}
			return
		}
		var d net.Dialer
		c, err := d.DialContext(ctx, network, addr)
		if serr := netns.Set(orig); serr == nil {
			runtime.UnlockOSThread()
		}
		ch <- res{c, err}
	}()
	r := <-ch
	return r.c, r.err
}

// redfishRoot GETs /redfish/v1/ at an address from the probe and returns the status and the
// certificate's common name (each BMC's certificate names its host).
func (p *probe) redfishRoot(ip string, timeout time.Duration) (int, string, error) {
	var cn string
	tr := &http.Transport{DialContext: p.dial, TLSClientConfig: &tls.Config{InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) > 0 {
				cn = cs.PeerCertificates[0].Subject.CommonName
			}
			return nil
		}}}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: timeout}
	resp, err := c.Get("https://" + ip + "/redfish/v1/")
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, cn, nil
}

// neigh is the probe's ARP table: IP -> MAC (reachable or stale entries).
func (p *probe) neigh(t *testing.T) map[string]string {
	t.Helper()
	ns, err := p.h.NeighList(0, netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, n := range ns {
		if n.HardwareAddr != nil && n.State&(netlink.NUD_REACHABLE|netlink.NUD_STALE|netlink.NUD_DELAY|netlink.NUD_PROBE) != 0 {
			out[n.IP.String()] = n.HardwareAddr.String()
		}
	}
	return out
}

// sh runs a command and returns its output, for the log (iproute2's view of the same state).
func sh(args ...string) string {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

package vm

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
	"github.com/insomniacslk/dhcp/dhcpv4"
)

// fakeL2 is a TAP substrate that splices some host ports onto switch ports (D57): what the
// splice writes to a port arrives on its out channel, and what is put on its in channel is what
// the switch sent.
type fakeL2 struct{ ports map[string]*fakeSwitchPort }

type fakeSwitchPort struct {
	in, out chan []byte
	done    chan struct{}
}

func (p *fakeSwitchPort) ReadFrame(b []byte) (int, error) {
	select {
	case f := <-p.in:
		return copy(b, f), nil
	case <-p.done:
		return 0, netsim.ErrClosed
	}
}

func (p *fakeSwitchPort) WriteFrame(b []byte) error {
	p.out <- append([]byte(nil), b...)
	return nil
}

func (f *fakeL2) Name() string                    { return "tap" }
func (f *fakeL2) Port(string) netsim.L2Port       { return nil }
func (f *fakeL2) DevicePort(string) netsim.L2Port { return nil }
func (f *fakeL2) Gateway(string, string, netip.Prefix, net.HardwareAddr) (netsim.L2Gateway, error) {
	return nil, nil
}
func (f *fakeL2) NOS(string) string       { return "generic/nvue-frr" }
func (f *fakeL2) Report() netsim.L2Report { return netsim.L2Report{} }
func (f *fakeL2) Close() error            { return nil }
func (f *fakeL2) HostPort(ref string) netsim.Cable {
	if p := f.ports[ref]; p != nil {
		return p
	}
	return nil
}

// qemuNIC is QEMU's end of a NIC's stream cable: frames with a 32-bit big-endian length.
type qemuNIC struct{ c net.Conn }

func (q qemuNIC) send(t *testing.T, b []byte) {
	t.Helper()
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := q.c.Write(append(hdr[:], b...)); err != nil {
		t.Fatal(err)
	}
}

func (q qemuNIC) recv(t *testing.T) []byte {
	t.Helper()
	_ = q.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	var hdr [4]byte
	if _, err := io.ReadFull(q.c, hdr[:]); err != nil {
		t.Fatalf("no frame to the guest: %v", err)
	}
	b := make([]byte, binary.BigEndian.Uint32(hdr[:]))
	if _, err := io.ReadFull(q.c, b); err != nil {
		t.Fatal(err)
	}
	return b
}

// A vm host's port the substrate spliced onto a real switch (D57) carries the guest's frames to
// the switch and back: the switch relays its DHCP and routes it, and the twin only observes, as
// D48's observer contract has it: the guest's lease is committed as a lease event on the port. A
// port the substrate did not splice keeps the twin's switch port and gateway (D48).
func TestSplicedNICCarriesTheGuestToTheSwitch(t *testing.T) {
	m, eng := splicedMachine(t)
	a, sp, host := m.a, m.a.rt.Net.L2.(*fakeL2).ports[m.path+":eth0"], m.path
	defer m.closeNICs()
	if len(m.nics) != 2 {
		t.Fatalf("%d NICs, want eth0 and eth1", len(m.nics))
	}
	eth0, eth1 := m.nics[0], m.nics[1]
	if eth0.port.Name != "eth0" || eth0.splice == nil || eth0.hp != nil {
		t.Fatalf("eth0: spliced %v, twin port %v", eth0.splice != nil, eth0.hp != nil)
	}
	if eth1.splice != nil || eth1.hp == nil {
		t.Fatalf("eth1 (not spliced): spliced %v, twin port %v", eth1.splice != nil, eth1.hp != nil)
	}

	conn, err := net.Dial("unix", eth0.cable.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	q := qemuNIC{conn}
	mac, _ := net.ParseMAC(eth0.port.MAC)
	disc, err := dhcpv4.NewDiscovery(mac, dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient:Arch:00007")))
	if err != nil {
		t.Fatal(err)
	}
	frame := netsim.UDPFrame{SrcMAC: mac, DstMAC: netsim.BroadcastMAC, SrcIP: netip.IPv4Unspecified(), DstIP: netip.MustParseAddr("255.255.255.255"),
		SrcPort: 68, DstPort: 67, Payload: disc.ToBytes()}.Marshal()
	// QEMU's NIC connects before the guest sends; the cable is linked once it has accepted
	deadline := time.Now().Add(2 * time.Second)
	for !eth0.cable.Linked() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	q.send(t, frame)
	select {
	case got := <-sp.out:
		if string(got) != string(frame) {
			t.Fatal("the DISCOVER reached the switch changed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the guest's DISCOVER did not reach the switch")
	}

	ack, err := dhcpv4.NewReplyFromRequest(disc, dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
		dhcpv4.WithYourIP(net.ParseIP("10.40.0.10")), dhcpv4.WithServerIP(net.ParseIP("10.90.0.10")),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP("10.90.0.10"))), dhcpv4.WithRouter(net.ParseIP("10.40.0.1")))
	if err != nil {
		t.Fatal(err)
	}
	leafMAC, _ := net.ParseMAC(eth0.port.Link.Peer(eth0.port).MAC)
	reply := netsim.UDPFrame{SrcMAC: leafMAC, DstMAC: mac, SrcIP: netip.MustParseAddr("10.40.0.1"), DstIP: netip.MustParseAddr("10.40.0.10"),
		SrcPort: 67, DstPort: 68, Payload: ack.ToBytes()}.Marshal()
	sp.in <- reply
	if got := q.recv(t); string(got) != string(reply) {
		t.Fatal("the ACK reached the guest changed")
	}

	var x NIC
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		x = a.NICs(host)[0]
		if x.Lease != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if x.Lease != "10.40.0.10" || x.Classes["PXEClient:Arch:00007"] != 1 {
		t.Errorf("observed lease %q, classes %v", x.Lease, x.Classes)
	}
	if !x.Spliced || x.Switch == "" || x.Gateway != "" {
		t.Errorf("NIC %+v: want spliced onto its switch's port, with no twin gateway", x)
	}
	var ev *schema.Event
	for _, e := range eng.Events(0, func(e schema.Event) bool { return e.Kind == "observe" && e.Path == host+":eth0" }) {
		if e.State != nil && e.State.Field == "lease" {
			e := e
			ev = &e
		}
	}
	if ev == nil || ev.State.To != "10.40.0.10" || !strings.Contains(ev.Message, "relayed by the switch") {
		t.Errorf("lease event %+v", ev)
	}
	lines := strings.Join(a.networkReport(), "\n")
	if !strings.Contains(lines, "spliced onto the real switch port") {
		t.Errorf("report:\n%s", lines)
	}
}

// fakeLinks records set_link as QEMU would apply it.
type fakeLinks struct{ set []string }

func (f *fakeLinks) SetLink(netdev string, up bool) error {
	f.set = append(f.set, fmt.Sprintf("%s=%v", netdev, up))
	return nil
}

// A guest's NIC has the carrier its cable has (D57's cable pulls, D48's too): a pulled cable or a
// port fault takes it down through QMP set_link, a replaced one brings it back, and nothing is sent
// while nothing changed; a new QEMU process is given every NIC's state.
func TestGuestNICCarrierFollowsTheCable(t *testing.T) {
	m, eng := splicedMachine(t)
	defer m.closeNICs()
	q := &fakeLinks{}
	m.syncLinks(q, true)
	if strings.Join(q.set, " ") != "net0=true net1=true" {
		t.Fatalf("a new process: %v", q.set)
	}
	q.set = nil
	m.syncLinks(q, false)
	if len(q.set) != 0 {
		t.Fatalf("nothing changed, but sent %v", q.set)
	}
	eth1 := m.nics[1].port
	src := schema.EventSource{Class: "test", Principal: "test"}
	if err := eng.SetLink(eth1.Ref, false, src); err != nil {
		t.Fatal(err)
	}
	m.syncLinks(q, false)
	if strings.Join(q.set, " ") != "net1=false" {
		t.Fatalf("cable pulled: %v", q.set)
	}
	q.set = nil
	if err := eng.SetLink(eth1.Ref, true, src); err != nil {
		t.Fatal(err)
	}
	m.syncLinks(q, false)
	if strings.Join(q.set, " ") != "net1=true" {
		t.Fatalf("cable replaced: %v", q.set)
	}
}

// splicedMachine is tap-vm-nos's r01 server as the vm adapter plans it, with its eth0 spliced
// onto a fake real switch port and eth1 on the twin's switch port, its NICs cabled.
func splicedMachine(t *testing.T) (*machine, *engine.Engine) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../../test/linuxnet/testdata/tap-vm-nos.yaml")
	if err != nil {
		t.Fatal(err)
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: "x86_64"})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(res.Model, reg, "")
	if err != nil {
		t.Fatal(err)
	}
	const host = "site-1/hall-a/r01/server-01"
	d := res.Model.DeviceBy[host]
	if d == nil || d.Fidelity != "vm" {
		t.Fatalf("%s is not a vm host", host)
	}
	sp := &fakeSwitchPort{in: make(chan []byte, 8), out: make(chan []byte, 8), done: make(chan struct{})}
	t.Cleanup(func() { close(sp.done) })
	n, err := netsim.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	n.L2 = &fakeL2{ports: map[string]*fakeSwitchPort{host + ":eth0": sp}}
	a := New()
	a.rt = &adapter.Runtime{Engine: eng, Net: n, StateDir: t.TempDir(), Assets: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.opts = est.Spec.Adapters.VM
	p, err := planOf(d)
	if err != nil {
		t.Fatal(err)
	}
	run, err := os.MkdirTemp("/tmp", "dctwin-vm-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	m := &machine{a: a, path: host, plan: p, run: run, dir: run, inst: &qemu.Installation{Arch: "x86_64"}}
	a.machines = map[string]*machine{host: m}
	a.order = []string{host}
	if err := m.cableNICs(d, nil); err != nil {
		t.Fatal(err)
	}
	return m, eng
}

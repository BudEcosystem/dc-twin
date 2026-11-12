//go:build linux

package linuxnet

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim/lldp"
	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// The twin's LLDP frames (D78; internal/adapter/netsim/lldp) decoded by an implementation that is
// not the twin's: tcpdump's LLDP dissector, in the generic/nvue-frr image (Debian's tcpdump). The
// frames a model BMC sends are written to a pcap and read back with `tcpdump -v`, which names
// every TLV it decodes per IEEE 802.1AB and marks a malformed or truncated one.
func TestLLDPDecodedByTcpdump(t *testing.T) {
	need(t)
	d, err := nosRuntime(t)
	if err != nil {
		t.Skipf("no Docker Engine: %v", err)
	}
	ctx := context.Background()
	if !d.HasImage(ctx, nos.NVUEFRRImage) {
		t.Skipf("image %s is not present", nos.NVUEFRRImage)
	}
	base := filepath.Dir(day0Binaries(t)) // the run's work directory, which the NOS runtime can read
	dir := filepath.Join(base, "lldp-pcap")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mac, _ := net.ParseMAC("02:dc:10:91:94:94")
	frames := [][]byte{
		lldp.Frame(mac, lldp.Info{ChassisMAC: mac, PortID: "bmc0", PortDesc: "BMC management port", SysName: "r01-server-01",
			SysDesc: "dc-twin model BMC (generic/server-2u)", Station: true, MgmtAddr: netip.MustParseAddr("10.30.0.57")}),
		lldp.Frame(mac, lldp.Info{ChassisMAC: mac, PortID: "eth1", PortDesc: "uplink", SysName: "r01-mgmt-switch",
			SysDesc: "dc-twin model switch", Bridge: true, Router: true, TTL: 120}),
	}
	pcap := filepath.Join(dir, "lldp.pcap")
	if err := os.WriteFile(pcap, writePcap(frames), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := d.Run(ctx, nos.Spec{Name: "dctwin-lldp-tcpdump-" + strconv.Itoa(os.Getpid()), Image: nos.NVUEFRRImage,
		Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{"tcpdump -nn -v -r /pcap/lldp.pcap 2>&1; echo DECODED; sleep 30"},
		Binds: []string{dir + ":/pcap:ro"}, Labels: map[string]string{"dc-twin.test": "linuxnet"}})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Remove(context.Background(), c.ID)
	var out string
	waitFor(t, 2*time.Minute, "tcpdump's decode", func() bool {
		out, _ = d.Logs(ctx, c.ID)
		return strings.Contains(out, "DECODED")
	})
	t.Logf("tcpdump -v:\n%s", indent(out))
	for _, bad := range []string{"[|LLDP]", "malformed", "Invalid", "invalid"} {
		if strings.Contains(out, bad) {
			t.Errorf("tcpdump marks the twin's LLDP %q", bad)
		}
	}
	for _, want := range []string{
		"LLDP, length",
		"Chassis ID TLV (1)", "Subtype MAC address (4): 02:dc:10:91:94:94",
		"Port ID TLV (2)", "bmc0",
		"Time to Live TLV (3)", "TTL 120s",
		"Port Description TLV (4)", "BMC management port",
		"System Name TLV (5)", "r01-server-01",
		"System Description TLV (6)", "dc-twin model BMC",
		"System Capabilities TLV (7)",
		"Management Address TLV (8)", "10.30.0.57",
		"End TLV (0)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tcpdump's decode has no %q", want)
		}
	}
	if n := strings.Count(out, "LLDP, length"); n != len(frames) {
		t.Errorf("tcpdump decoded %d LLDP frames of %d", n, len(frames))
	}
}

// writePcap is a pcap file (libpcap's format, Ethernet link type) holding frames.
func writePcap(frames [][]byte) []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	w(uint32(0xa1b2c3d4))
	w(uint16(2))
	w(uint16(4))
	w(int32(0))
	w(uint32(0))
	w(uint32(65535))
	w(uint32(1)) // LINKTYPE_ETHERNET
	now := time.Now()
	for _, f := range frames {
		w(uint32(now.Unix()))
		w(uint32(now.Nanosecond() / 1000))
		w(uint32(len(f)))
		w(uint32(len(f)))
		b.Write(f)
	}
	return b.Bytes()
}

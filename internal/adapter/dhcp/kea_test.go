package dhcp

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// These tests run the twin relay against a real ISC Kea DHCPv4 server as the stack's DHCP.
//
//	DCTWIN_KEA=auto        start kea-dhcp4 (from PATH, or DCTWIN_KEA_BIN) with a config the
//	                       test writes: one subnet per twin segment, store-extended-info on
//	DCTWIN_KEA=host:port   use a Kea already running there, configured with PlanSegments'
//	                       subnets; DCTWIN_KEA_LEASES names its memfile to check option 82
//
// Kea answers a relay at giaddr, on the relay's source port when the relay sends RFC 8357's
// sub-option, so the relay and Kea must share a network namespace with no NAT between them. On
// macOS with docker (colima), testdata/kea/run-in-container.sh runs this package's test binary
// inside a container that has Kea; on Linux, any kea-dhcp4 on PATH will do.

type keaServer struct {
	addr   string
	leases string
	log    string
}

func startKea(t testing.TB, segs []Segment, network string, lifetime time.Duration) *keaServer {
	t.Helper()
	mode := os.Getenv("DCTWIN_KEA")
	if mode == "" {
		t.Skip("DCTWIN_KEA is not set: set DCTWIN_KEA=auto with kea-dhcp4 on PATH (or DCTWIN_KEA_BIN), DCTWIN_KEA=host:port for a running Kea, or run testdata/kea/run-in-container.sh")
	}
	if mode != "auto" {
		t.Logf("using the Kea at %s; it must serve these subnets, selected by link selection:\n%s", mode, keaHint(segs, network))
		return &keaServer{addr: mode, leases: os.Getenv("DCTWIN_KEA_LEASES")}
	}
	bin := os.Getenv("DCTWIN_KEA_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("kea-dhcp4"); err != nil {
			t.Skip("DCTWIN_KEA=auto but kea-dhcp4 is not on PATH (set DCTWIN_KEA_BIN, or run testdata/kea/run-in-container.sh)")
		}
	}
	dir := t.TempDir()
	iface := os.Getenv("DCTWIN_KEA_IFACE")
	if iface == "" {
		iface = "lo"
		if runtime.GOOS == "darwin" {
			iface = "lo0"
		}
	}
	port := freeUDPPort(t)
	k := &keaServer{addr: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), leases: filepath.Join(dir, "kea-leases4.csv"),
		log: filepath.Join(dir, "kea.log")}
	cfg := keaConfig(segs, network, iface, k.leases, k.log, lifetime)
	if os.Getenv("DCTWIN_KEA_DEBUG") != "" {
		lg := cfg["Dhcp4"].(map[string]any)["loggers"].([]any)[0].(map[string]any)
		lg["severity"], lg["debuglevel"] = "DEBUG", 55
	}
	cfgPath := filepath.Join(dir, "kea-dhcp4.json")
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-c", cfgPath, "-p", strconv.Itoa(port))
	cmd.Env = append(os.Environ(), "KEA_PIDFILE_DIR="+dir, "KEA_LOCKFILE_DIR="+dir)
	out, _ := os.Create(filepath.Join(dir, "kea.stdout"))
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
		if t.Failed() {
			lb, _ := os.ReadFile(k.log)
			lines := strings.Split(string(lb), "\n")
			if len(lines) > 60 {
				lines = lines[len(lines)-60:]
			}
			t.Logf("Kea's log, last lines:\n%s", strings.Join(lines, "\n"))
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for {
		lb, _ := os.ReadFile(k.log)
		if strings.Contains(string(lb), "DHCP4_STARTED") {
			break
		}
		select {
		case err := <-exited:
			so, _ := os.ReadFile(filepath.Join(dir, "kea.stdout"))
			t.Fatalf("kea-dhcp4 exited: %v\n%s\n%s", err, so, lb)
		default:
		}
		if time.Now().After(deadline) {
			so, _ := os.ReadFile(filepath.Join(dir, "kea.stdout"))
			t.Fatalf("kea-dhcp4 did not start in 20 s\n%s\n%s", so, lb)
		}
		time.Sleep(100 * time.Millisecond)
	}
	vb, _ := exec.Command(bin, "-v").Output()
	t.Logf("Kea %s listening on %s (%s), %d subnets, leases in %s", strings.TrimSpace(string(vb)), k.addr, iface, len(cfg["Dhcp4"].(map[string]any)["subnet4"].([]any)), k.leases)
	return k
}

func freeUDPPort(t testing.TB) int {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// keaConfig is the stack side of the contract: one subnet per twin segment, selected by the
// relay's link selection, with the segment's gateway as router; store-extended-info keeps the
// relay agent information in each lease.
func keaConfig(segs []Segment, network, iface, leases, log string, lifetime time.Duration) map[string]any {
	life := int(lifetime / time.Second)
	var subnets []any
	for _, s := range segs {
		if s.Network != network || !s.Prefix.IsValid() {
			continue
		}
		first, _ := hostAt(s.Prefix, 10)
		last, _ := hostAt(s.Prefix, 250)
		subnets = append(subnets, map[string]any{
			"id": s.Index + 1, "subnet": s.Prefix.String(),
			"pools":        []any{map[string]any{"pool": first.String() + " - " + last.String()}},
			"option-data":  []any{map[string]any{"name": "routers", "data": s.Gateway.String()}},
			"user-context": map[string]any{"rack": s.Rack},
		})
	}
	return map[string]any{"Dhcp4": map[string]any{
		"interfaces-config": map[string]any{"interfaces": []any{iface}, "dhcp-socket-type": "udp"},
		"lease-database":    map[string]any{"type": "memfile", "persist": true, "name": leases, "lfc-interval": 0},
		"valid-lifetime":    life, "renew-timer": life / 2, "rebind-timer": life * 7 / 8,
		"store-extended-info": true,
		"subnet4":             subnets,
		"loggers": []any{map[string]any{"name": "kea-dhcp4", "severity": "INFO",
			"output-options": []any{map[string]any{"output": log}}}},
	}}
}

// keaLease is a row of Kea's memfile with the relay agent information it stored.
type keaLease struct {
	addr, hwaddr string
	subnetID     int
	circuit      string
	remote       []byte
	link         net.IP
	context      string
}

// readKeaLeases reads Kea's memfile (CSV, commas inside fields escaped as "&#x2c"), the last
// row of each hardware address winning.
func readKeaLeases(path string) (map[string]keaLease, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]keaLease{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var cols map[string]int
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ",")
		for i := range fields {
			fields[i] = strings.ReplaceAll(fields[i], "&#x2c", ",")
		}
		if cols == nil {
			cols = map[string]int{}
			for i, c := range fields {
				cols[c] = i
			}
			continue
		}
		get := func(c string) string {
			if i, ok := cols[c]; ok && i < len(fields) {
				return fields[i]
			}
			return ""
		}
		l := keaLease{addr: get("address"), hwaddr: get("hwaddr"), context: get("user_context")}
		l.subnetID, _ = strconv.Atoi(get("subnet_id"))
		var uc struct {
			ISC struct {
				RAI json.RawMessage `json:"relay-agent-info"`
			} `json:"ISC"`
		}
		if json.Unmarshal([]byte(l.context), &uc) == nil && len(uc.ISC.RAI) > 0 {
			var hexs string
			var obj struct {
				Sub string `json:"sub-options"`
			}
			if json.Unmarshal(uc.ISC.RAI, &obj) == nil && obj.Sub != "" {
				hexs = obj.Sub
			} else {
				_ = json.Unmarshal(uc.ISC.RAI, &hexs) // Kea before 2.3 stored the hex string itself
			}
			if raw, err := hex.DecodeString(strings.TrimPrefix(strings.ToLower(hexs), "0x")); err == nil {
				so := subOptions(raw)
				l.circuit, l.remote, l.link = string(so[subCircuitID]), so[subRemoteID], net.IP(so[subLinkSelection])
			}
		}
		out[l.hwaddr] = l
	}
	return out, sc.Err()
}

// Kea, the stack's DHCP, leases every BMC of the lab estate through the twin relays: each
// lease in Kea's own store carries its port's circuit id, the switch's MAC as remote id and
// the segment's link selection, and lands in the subnet of the BMC's rack.
func TestKeaStackDHCPLeasesEveryLabBMC(t *testing.T) {
	segs := PlanSegments(compileEstate(t, loadLab(t)))
	kea := startKea(t, segs, "oob", lifetimeLab)
	est := loadLab(t)
	n := est.Spec.Networks["oob"]
	n.DHCP, n.Server = "stack", kea.addr
	est.Spec.Networks["oob"] = n

	tw, a := up(t, est, nil)
	took := waitLeased(t, a, 60*time.Second)
	t.Logf("%d BMC endpoints leased from Kea through the twin relays in %s", len(a.endpoints), took.Round(time.Millisecond))
	ls := leases(tw, a)
	checkRelayFacts(t, tw, a, ls)
	for path, l := range ls {
		if l.Server == "" {
			t.Errorf("%s: no server identifier", path)
		}
	}
	checkKeaStore(t, kea, a, ls)

	// renewals at T1 go to Kea through the relays and keep every address; a BMC reset ends in
	// an INIT-REBOOT that Kea ACKs with the same address
	time.Sleep(lifetimeLab/2 + 2*time.Second)
	if n := a.renewals.Load(); n < int64(len(a.endpoints)) {
		t.Errorf("%d renewals after T1 for %d endpoints", n, len(a.endpoints))
	}
	dev := "site-1/hall-a/a02/compute-tray-05/bf3-0"
	if err := tw.Engine.ResetBMC(engine.Request{Path: dev, Canonical: "bmc.reset", Protocol: "test",
		Source: schema.EventSource{Class: "scenario", Principal: "test"}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 20*time.Second, "Kea's ACK of the INIT-REBOOT after the BMC reset", func() bool { return a.endpoints[dev].reboots.Load() > 0 })
	after := leases(tw, a)
	for p, l := range ls {
		if after[p].IP != l.IP {
			t.Errorf("%s: Kea moved %s to %s", p, l.IP, after[p].IP)
		}
	}
	t.Logf("%d renewals and a BMC reset later, every BMC holds the address Kea first gave it (%d ACKs in all)", a.renewals.Load(), a.acks.Load())
	for _, l := range a.Report().Lines {
		t.Logf("report: %s", l)
	}
}

// lifetimeLab is the lease Kea grants in the lab test: short, so renewals happen during it.
const lifetimeLab = 8 * time.Second

// checkKeaStore asserts Kea's own record of every BMC lease: its address, the circuit id of the
// BMC's switch port, the switch's MAC as remote id, the segment gateway as link selection, and
// the subnet of the BMC's rack.
func checkKeaStore(t testing.TB, kea *keaServer, a *Adapter, ls map[string]model.Lease) {
	t.Helper()
	if kea.leases == "" {
		t.Log("DCTWIN_KEA_LEASES not set: Kea's own lease store was not checked")
		return
	}
	var kl map[string]keaLease
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		kl, err = readKeaLeases(kea.leases)
		if err == nil && len(kl) >= len(a.endpoints) || time.Now().After(deadline) {
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	circuits := map[string]bool{}
	for path, ep := range a.endpoints {
		k, ok := kl[ep.mac.String()]
		if !ok {
			t.Errorf("%s (%s): no lease in Kea's store", path, ep.mac)
			continue
		}
		if k.addr != ls[path].IP {
			t.Errorf("%s: Kea leased %s, the model holds %s", path, k.addr, ls[path].IP)
		}
		if k.circuit != circuitID(ep.sw, ep.swPort) {
			t.Errorf("%s: Kea stored circuit id %q, want %q", path, k.circuit, circuitID(ep.sw, ep.swPort))
		}
		if string(k.remote) != switchMAC(ep.sw).String() {
			t.Errorf("%s: Kea stored remote id %q, want the switch's MAC as text %s", path, k.remote, switchMAC(ep.sw))
		}
		if !k.link.Equal(ep.seg.Gateway.AsSlice()) {
			t.Errorf("%s: Kea stored link selection %s, want %s", path, k.link, ep.seg.Gateway)
		}
		if k.subnetID != ep.seg.Index+1 {
			t.Errorf("%s: Kea chose subnet %d, want %d (%s)", path, k.subnetID, ep.seg.Index+1, ep.seg.Prefix)
		}
		if circuits[k.circuit] {
			t.Errorf("circuit id %q appears twice", k.circuit)
		}
		circuits[k.circuit] = true
	}
	t.Logf("Kea's store holds %d leases, %d distinct circuit ids", len(kl), len(circuits))
	if ep := a.endpoints["site-1/hall-a/a01/compute-tray-01/bf3-0"]; ep != nil {
		k := kl[ep.mac.String()]
		t.Logf("e.g. Kea lease %s %s subnet %d user-context %s", k.addr, k.hwaddr, k.subnetID, k.context)
	}
	if lb, err := os.ReadFile(kea.log); err == nil {
		allocs := strings.Count(string(lb), "DHCP4_LEASE_ALLOC")
		t.Logf("Kea's log: %d DHCP4_LEASE_ALLOC lines", allocs)
		for _, line := range strings.Split(string(lb), "\n") {
			if strings.Contains(line, "DHCP4_LEASE_ALLOC") {
				t.Logf("  %s", line)
				break
			}
		}
	}
}

func waitFor(t testing.TB, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func keaHint(segs []Segment, network string) string {
	var b strings.Builder
	for _, s := range segs {
		if s.Network != network {
			continue
		}
		fmt.Fprintf(&b, "%s %s %s gw %s\n", s.Network, s.Rack, s.Prefix, s.Gateway)
	}
	return b.String()
}

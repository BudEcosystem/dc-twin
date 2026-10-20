package dhcp

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// scaleEstate is a hall of three rows of sixteen positions: 47 CPU racks of 20 servers and two
// PDUs each (22 BMCs a rack) and one network rack, 1,036 BMCs on one OOB network.
func scaleEstate(t testing.TB, dhcp, server string) *schema.Estate {
	var b strings.Builder
	b.WriteString(`apiVersion: dc-twin/v1alpha1
kind: Estate
metadata: { name: scale-1k }
spec:
  seed: 1000
  clock: wall
  timeScale: 0.05
  packs: [generic@0.1.0]
  fidelity: { default: model }
  sites:
    - name: site-1
      halls:
        - name: hall-a
          rows:
`)
	for r, row := range []string{"a", "b", "c"} {
		fmt.Fprintf(&b, "            - name: %s\n              positions: 16\n              racks:\n", row)
		for p := 1; p <= 16; p++ {
			if r == 2 && p == 16 {
				fmt.Fprintf(&b, "                - { name: %s%02d, position: %d, format: generic/network-rack }\n", row, p, p)
				continue
			}
			fmt.Fprintf(&b, "                - { name: %s%02d, position: %d, format: generic/cpu-rack, params: { server: generic/server-2u, servers: 20 } }\n", row, p, p)
		}
	}
	srv := ""
	if server != "" {
		srv = ", server: \"" + server + "\""
	}
	fmt.Fprintf(&b, `  networks:
    oob: { role: management, dhcp: %s, cidr: 10.30.0.0/16, fabric: oob%s }
  fabrics:
    - { name: oob, template: oob-flat, nos: none }
  facility:
    power:
      feeds: [{ name: A, capacityW: 9000000 }, { name: B, capacityW: 9000000 }]
  stacks: []
`, dhcp, srv)
	var est schema.Estate
	if err := schema.Decode([]byte(b.String()), &est); err != nil {
		t.Fatal(err)
	}
	return &est
}

// Spec target (05, 16): 1,000 BMCs leased within 60 s. Through Kea when DCTWIN_KEA is set,
// else through the twin site DHCP; both paths cross the twin relays.
func TestScaleThousandBMCsLease(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: the scale run opens over a thousand sockets")
	}
	for _, burst := range []bool{false, true} {
		name := "spread"
		if burst {
			name = "burst"
		}
		t.Run(name, func(t *testing.T) { scaleRun(t, burst) })
	}
}

// scaleRun leases the scale estate. Spread: first DISCOVERs over the adapter's start window, as
// a hall's BMCs come up. Burst: all at once, the worst case for the server's socket buffer.
func scaleRun(t *testing.T, burst bool) {
	mode, server := "twin", ""
	var kea *keaServer
	if os.Getenv("DCTWIN_KEA") != "" {
		kea = startKea(t, PlanSegments(compileEstate(t, scaleEstate(t, "twin", ""))), "oob", 10*time.Minute)
		mode, server = "stack", kea.addr
	}
	est := scaleEstate(t, mode, server)
	t0 := time.Now()
	tw, a := up(t, est, func(a *Adapter) {
		if burst {
			a.stagger = 0
		}
	})
	startup := time.Since(t0)
	if len(a.endpoints) < 1000 {
		t.Fatalf("%d BMC endpoints, want ≥ 1,000", len(a.endpoints))
	}
	took := waitLeased(t, a, 90*time.Second)
	t.Logf("SCALE: %d BMCs on %d segments leased from %s through the twin relays in %s (estate up and %d sockets opened in %s; first DISCOVERs spread over %s)",
		len(a.endpoints), len(a.nets["oob"].order), a.nets["oob"].up.label, took.Round(time.Millisecond), len(a.endpoints)+len(a.nets["oob"].order)+2,
		startup.Round(time.Millisecond), a.stagger)
	if took > 60*time.Second {
		t.Errorf("leasing took %s, over the 60 s target", took)
	}
	ls := leases(tw, a)
	checkRelayFacts(t, tw, a, ls)
	if kea != nil {
		checkKeaStore(t, kea, a, ls)
	}
	t.Logf("SCALE: %d DHCP messages sent by the endpoints (%d retransmissions)", a.sent.Load(), a.sent.Load()-2*int64(len(a.endpoints)))
	if a.nets["oob"].plan.scheme != schemeOrdinal {
		t.Errorf("a /16 over three rows of 16 should use the rack ordinal, got %s", a.nets["oob"].plan.scheme)
	}
	for _, l := range a.Report().Lines {
		t.Logf("report: %s", l)
	}
}

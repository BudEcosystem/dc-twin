package e2e

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/control"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// TestIsolationScan is the M0 exit's isolation check (15): from where the software under test
// stands — the site gateway — the control API, the engine and the host are unreachable; only
// estate endpoints answer.
func TestIsolationScan(t *testing.T) {
	tw := up(t, "../../examples/estates/lab-model.yaml", []string{"redfish"}, nil)
	srv, err := control.Serve(tw, "127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	connect := func(target string) int {
		c, err := net.DialTimeout("tcp", tw.Net.Gateway, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	for _, target := range []string{srv.Info.Addr, "127.0.0.1:22", "1.1.1.1:443", "localhost:443"} {
		if code := connect(target); code == http.StatusOK {
			t.Errorf("the site gateway reached %s, which is not an estate endpoint", target)
		} else {
			t.Logf("gateway → %-18s refused (%d)", target, code)
		}
	}
	// an estate endpoint does answer through the gateway
	var bmc string
	for path, kinds := range tw.Net.Endpoints {
		if a, ok := kinds["redfish"]; ok && path != "" {
			_ = a
			bmc = path
			break
		}
	}
	host := ""
	for site, local := range tw.Net.Routes() {
		if local == tw.Net.Endpoints[bmc]["redfish"] {
			host = site
		}
	}
	if code := connect(host); code != http.StatusOK {
		t.Fatalf("an estate endpoint (%s) must answer through the gateway, got %d", host, code)
	}
	// and the M0 exit's request-name rule: an operator's power act and a stack's Redfish reset
	// carry the same canonical name
	since := tw.Engine.Seq()
	_ = tw.Engine.Power(engine.Request{Path: bmc, Canonical: "power.off.force", Source: schema.EventSource{Class: "user"}, Protocol: "control"})
	evs := tw.Engine.Events(since, func(e schema.Event) bool { return e.Request != nil && e.Request.Canonical == "power.off.force" })
	if len(evs) != 1 {
		t.Fatalf("a dctwin power act must log the canonical name power.off.force, got %d events", len(evs))
	}
}

package nmxc

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	_ "github.com/BudEcosystem/dc-twin/internal/adapter/dhcp"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// NMX-C runs on NVOS, so a client that dials NVOS's address (its mgmt0 lease, D46) through the
// site gateway reaches the tray's controller, as NICo's nvlink-manager does.
func TestNMXCAtNVOSAddress(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	ps := protoOrSkip(t, reg)
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{"dhcp", Name}, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	const tray = "site-1/hall-a/a01/switch-tray-01"
	port := ""
	for _, p := range twin.NVOSPorts(tw.Engine.M.DeviceBy[tray]) {
		port = p.Ref
	}
	if port == "" {
		t.Fatal("no NVOS port on " + tray)
	}
	lease := ""
	waitFor(t, "NVOS's lease on "+port, func() bool {
		for _, ev := range tw.Engine.Events(0, func(e schema.Event) bool {
			return e.State != nil && e.State.Field == "lease" && e.Path == port
		}) {
			lease = fmt.Sprint(ev.State.To)
		}
		return lease != ""
	})

	target := fmt.Sprintf("%s:%d", lease, Port)
	viaGateway := func(ctx context.Context, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", tw.Net.Gateway)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr)
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil || resp.StatusCode != 200 {
			c.Close()
			return nil, fmt.Errorf("CONNECT %s: %v %v", addr, resp, err)
		}
		return c, nil
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(tw.Net.CAPEM)
	cc, err := grpc.NewClient("passthrough:///"+target, grpc.WithContextDialer(viaGateway),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: lease})))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	c := &client{t: t, ps: ps, cc: cc}
	h := c.call("Hello", obj{"gateway_id": "nmxc-nvos-test", "major_version": "PROTO_MSG_MAJOR_VERSION", "minor_version": "PROTO_MSG_MINOR_VERSION"})
	if rc(h) != "NMX_ST_SUCCESS" {
		t.Fatalf("Hello at NVOS's address %s: %v", target, h)
	}
	t.Logf("NMX-C answers at NVOS's address %s (TLS verified for that address) through the site gateway", target)
}

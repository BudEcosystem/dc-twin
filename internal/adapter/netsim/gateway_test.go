package netsim

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

// The site gateway answers CONNECT with a bare 200 (no Transfer-Encoding or Content-Length,
// RFC 9110 §9.3.6) and then relays bytes to the routed endpoint.
func TestGatewayConnectReply(t *testing.T) {
	n, err := New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	if err := n.StartGateway("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		c, err := echo.Accept()
		if err == nil {
			io.Copy(c, c)
			c.Close()
		}
	}()
	n.Route("10.30.0.13:443", echo.Addr().String())

	c, err := net.Dial("tcp", n.Gateway)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "CONNECT 10.30.0.13:443 HTTP/1.1\r\nHost: 10.30.0.13:443\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || len(resp.TransferEncoding) > 0 || resp.Header.Get("Content-Length") != "" {
		t.Fatalf("CONNECT reply: %d %v %v", resp.StatusCode, resp.TransferEncoding, resp.Header)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		t.Fatalf("relayed %q, %v", got, err)
	}
}

// siteL2 is a substrate whose siteGateway host reaches 10.30.0.0/20: it dials what it reaches
// (here, the echo server) and has no route to anything else.
type siteL2 struct {
	L2
	to     string
	dialed []string
}

func (s *siteL2) Close() error { return nil }

func (s *siteL2) DialSite(addr string, timeout time.Duration) (net.Conn, error) {
	host, _, _ := net.SplitHostPort(addr)
	if !netip.MustParsePrefix("10.30.0.0/20").Contains(netip.MustParseAddr(host)) {
		return nil, ErrNoSiteRoute
	}
	s.dialed = append(s.dialed, addr)
	return net.DialTimeout("tcp", s.to, timeout)
}

// An address no estate endpoint answers (a real switch's SSH at its lease) is reached through
// the substrate's siteGateway host; one off the site's networks is still refused, and an estate
// endpoint is still the gateway's own route.
func TestGatewayReachesTheSiteThroughItsHost(t *testing.T) {
	n, err := New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	if err := n.StartGateway("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	l2 := &siteL2{to: echo.Addr().String()}
	n.L2 = l2
	n.Route("10.30.0.13:443", echo.Addr().String())
	connect := func(target string) int {
		c, err := net.Dial("tcp", n.Gateway)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	if code := connect("10.30.0.21:22"); code != 200 {
		t.Fatalf("a switch's SSH on the site's network: %d", code)
	}
	if code := connect("8.8.8.8:443"); code != http.StatusBadGateway {
		t.Fatalf("an address off the site's networks: %d", code)
	}
	if code := connect("10.30.0.13:443"); code != 200 {
		t.Fatalf("an estate endpoint: %d", code)
	}
	if len(l2.dialed) != 1 || l2.dialed[0] != "10.30.0.21:22" {
		t.Fatalf("dialled through the host: %v (an estate endpoint is the gateway's own)", l2.dialed)
	}
}

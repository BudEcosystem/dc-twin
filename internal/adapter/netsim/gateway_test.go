package netsim

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
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

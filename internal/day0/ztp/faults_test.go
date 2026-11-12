package ztp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The twin's day-0 faults as ZTP sees them: ztp-dhcp-no-option leaves the lease without its ZTP
// options, ztp-script-error cuts what is fetched short.
func TestDay0Faults(t *testing.T) {
	c, _ := testConfig(t)
	writeFile(t, c.dhcpEnv, "reason='BOUND'\nnew_ip_address='10.0.0.5'\nnew_cumulus_provision_url='http://z/ztp.sh'\nnew_ztp_json_url='http://z/ztp.json'\n", 0o644)
	if got := c.lease().option(opt239); got != "http://z/ztp.sh" {
		t.Fatalf("option 239 without the fault: %q", got)
	}
	writeFile(t, filepath.Join(c.faultsDir, "dhcp-strip-ztp"), "", 0o644)
	if got, got67 := c.lease().option(opt239), c.lease().option(opt67); got != "" || got67 != "" {
		t.Fatalf("ztp-dhcp-no-option: the lease still has 239 %q, 67 %q", got, got67)
	}
	if !c.lease().bound() {
		t.Fatal("ztp-dhcp-no-option: the lease itself is lost")
	}
	_ = os.Remove(filepath.Join(c.faultsDir, "dhcp-strip-ztp"))

	body := "#!/bin/sh\n# CUMULUS-AUTOPROVISIONING\nexit 0\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer srv.Close()
	f := c.newFetcher(http.Header{}, true)
	b, err := f.get(context.Background(), srv.URL)
	if err != nil || string(b) != body {
		t.Fatalf("fetch: %q %v", b, err)
	}
	writeFile(t, filepath.Join(c.faultsDir, "ztp-corrupt"), "", 0o644)
	b, err = f.get(context.Background(), srv.URL)
	if err != nil || len(b) != len(body)/2 {
		t.Fatalf("ztp-script-error: %d bytes (%v), want %d", len(b), err, len(body)/2)
	}
}

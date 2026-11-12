//go:build linux

package linuxnet

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// SONiC's first push without ZTP (18 — The first push; ztp disabled): the tooling finds each
// factory sonic-vs switch's management lease in its own DHCP server's leases, logs in over SSH as
// SONiC's factory user (admin, SONiC's default password) and loads the switch's config_db.json
// fragment with SONiC's own CLI (`sudo config load -y`, then `sudo config save -y`), as a site
// without ZTP does; SONiC's managers and frrcfgd take it to the kernel and FRR and the fabric
// converges. gNMI answers at the management lease too: SONiC's gnmi_get reads the pushed
// DEVICE_METADATA there, and gnmi_set writes a VRF through SONiC's YANG models, which CONFIG_DB
// then holds (mutual TLS with the estate CA's client certificate from stack-inputs).
func TestDay0SonicPush(t *testing.T) {
	r := startDay0(t, day0Case{estate: "linuxnet-day0-sonicpush", nos: nos.SonicVS, install: "preinstalled", ztp: "disabled"})
	ctx := context.Background()
	fac := r.factory()
	leases := map[string]string{}
	waitFor(t, 10*time.Minute, "every factory SONiC switch's management lease in Kea", func() bool {
		l := r.leases()
		for _, e := range fac {
			if l[strings.ToLower(e.MgmtMAC)] == "" {
				return false
			}
			leases[e.Serial] = l[strings.ToLower(e.MgmtMAC)]
		}
		return true
	})
	for _, e := range fac {
		ip := leases[e.Serial]
		s, _ := r.planOf(e.Serial)
		var out string
		// sshd and the CLI come up with the switch's services, after its lease
		ok := eventually(10*time.Minute, func() bool {
			cl, err := r.sshAs(ip, "admin", nos.SonicFactoryPassword)
			if err != nil {
				out = err.Error()
				return false
			}
			defer cl.Close()
			sess, err := cl.NewSession()
			if err != nil {
				out = err.Error()
				return false
			}
			defer sess.Close()
			body, _ := json.MarshalIndent(sonicConfigDB(s, e.BaseMAC), "", "  ")
			sess.Stdin = bytes.NewReader(body)
			b, err := sess.CombinedOutput("cat > /tmp/day0-config_db.json && sudo config load -y /tmp/day0-config_db.json && sudo config save -y")
			out = string(b)
			return err == nil
		})
		t.Logf("%s (%s): ssh admin@ config load -y; config save -y:\n%s", e.Hostname, ip, indent(out))
		if !ok {
			t.Fatalf("%s: SONiC's CLI over SSH did not take the configuration", e.Hostname)
		}
	}
	took := r.waitConverged(30 * time.Minute)
	t.Logf("every factory SONiC switch converged %s after its configuration was loaded over SSH", took.Round(time.Second))
	r.checkPhases()

	// gNMI at the management lease, with the estate CA's client certificate a site is handed
	e := fac[0]
	ip := leases[e.Serial]
	work := filepath.Join(filepath.Dir(day0Binaries(t)), "gnmi-"+r.c.estate+"-"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	for _, f := range []string{"switch-client.crt", "switch-client.key", "switch-ca.crt"} {
		b, err := os.ReadFile(filepath.Join(r.tw.Runtime.StateDir, "stack-inputs", f))
		if err == nil {
			err = os.WriteFile(filepath.Join(work, f), b, 0o644)
		}
		if err != nil {
			t.Fatalf("stack-inputs/%s: %v", f, err)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "vrf.json"), []byte(`{"sonic-vrf:VRF_LIST":[{"name":"Vrf-day0","vni":10099}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := r.d.Run(ctx, nos.Spec{Name: "dctwin-gnmi-" + r.c.estate + "-" + strconv.Itoa(os.Getpid()), Image: nos.SonicGNMIImage,
		Platform: "linux/amd64", Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{"sleep infinity"},
		NetworkMode: "container:" + r.kea.ID, Binds: []string{work + ":/c:ro"}, Labels: map[string]string{"dc-twin.test": "linuxnet"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.d.Remove(context.Background(), c.ID) })
	gnmi := func(tool string, args ...string) (string, int) {
		cmd := append([]string{tool, "-target_addr", ip + ":" + strconv.Itoa(nos.SonicGNMIPort), "-target_name", e.Hostname,
			"-ca", "/c/switch-ca.crt", "-cert", "/c/switch-client.crt", "-key", "/c/switch-client.key", "-logtostderr"}, args...)
		out, code, err := r.d.Exec(ctx, c.ID, cmd...)
		if err != nil {
			t.Logf("%s: %v", tool, err)
		}
		return out, code
	}
	out, code := gnmi("gnmi_get", "-xpath_target", "CONFIG_DB", "-xpath", "DEVICE_METADATA/localhost")
	t.Logf("gNMI Get CONFIG_DB/DEVICE_METADATA/localhost at %s's management lease %s (exit %d):\n%s", e.Hostname, ip, code, indent(out))
	if code != 0 || !strings.Contains(out, e.Hostname) {
		t.Errorf("gNMI Get at the management lease does not give the hostname the SSH push set (%s)", e.Hostname)
	}
	out, code = gnmi("gnmi_set", "-update", "/sonic-vrf:sonic-vrf/VRF/VRF_LIST:@/c/vrf.json")
	t.Logf("gNMI Set /sonic-vrf:sonic-vrf/VRF/VRF_LIST (Vrf-day0) at the management lease (exit %d):\n%s", code, indent(out))
	if code != 0 {
		t.Errorf("gNMI Set at the management lease failed")
	}
	if !eventually(time.Minute, func() bool {
		row, _ := r.sub.Exec(ctx, e.Path, "redis-cli", "-n", "4", "--raw", "hgetall", "VRF|Vrf-day0")
		return strings.Contains(row, "10099")
	}) {
		t.Errorf("Vrf-day0 is not in %s's CONFIG_DB a minute after the gNMI Set", e.Hostname)
	}
	r.judge()
}

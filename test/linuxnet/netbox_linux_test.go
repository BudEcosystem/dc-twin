//go:build linux

package linuxnet

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
)

// The NetBox + Ansible day-0 integration (compat/netbox-ansible, 18 Z6, D79), every piece real
// and unmodified but the site's own glue:
//   - NetBox 4.2.9 (netbox-docker 3.2.1's image) with PostgreSQL 17 and Valkey 8, beside the
//     tooling's Kea on the OOB network;
//   - stack-inputs/day0/netbox loaded through NetBox's own CSV import (import-csv.py);
//   - the management leases recorded in NetBox as each switch's out-of-band address
//     (record-leases.py);
//   - NVIDIA's net-configurator nc-v8.0.0 push playbook, as checked out, in its password-only mode
//     (--tags bootstrap-password): the factory password changed at first login over SSH;
//   - Ansible's netbox.netbox inventory reading NetBox, net-configurator's spine template ported
//     onto NetBox's variables as NVUE's object tree, and NVIDIA's nvidia.nvue pushing it over
//     NVUE's REST API, one revision per switch (push.yml).
//
// The switches run Cumulus 5.18.3's NVUE grammar, the version net-configurator targets. The
// fabric converges and the critic's day-0 policy passes.
const (
	netboxImage   = "quay.io/netboxcommunity/netbox:v4.2.9-3.2.1"
	postgresImage = "public.ecr.aws/docker/library/postgres:17-alpine"
	valkeyImage   = "public.ecr.aws/valkey/valkey:8.0-alpine"
	// netboxAnsibleImage is compat/netbox-ansible/Containerfile: net-configurator nc-v8.0.0 with
	// its pinned requirements (ansible 14, ansible-core 2.21, the netbox.netbox collection)
	netboxAnsibleImage = "dctwin-netbox-ansible:test"
	netboxToken        = "0b1d6d7c3f0e4a5b8c9d0e1f2a3b4c5d6e7f8a9b"
	netboxAdminPW      = "Day0-NetBox-1"
)

func TestDay0NetBox(t *testing.T) {
	compat := os.Getenv("DCTWIN_COMPAT_NETBOX")
	if compat == "" {
		t.Skip("DCTWIN_COMPAT_NETBOX is not set (scripts/linux-substrate-e2e.sh points it at compat/netbox-ansible)")
	}
	r := startDay0(t, day0Case{estate: "linuxnet-day0-netbox", nos: nos.NVUEFRR, install: "preinstalled", ztp: "disabled", cumulus: "5.18.3"})
	ctx := context.Background()
	for _, img := range []string{netboxImage, postgresImage, valkeyImage, netboxAnsibleImage} {
		if !r.d.HasImage(ctx, img) {
			t.Skipf("image %s is not present (scripts/linux-substrate-e2e.sh pulls and builds them)", img)
		}
	}
	suffix := r.c.estate + "-" + strconv.Itoa(os.Getpid())
	beside := "container:" + r.kea.ID // Kea's network namespace: the OOB network and localhost
	run := func(name string, s nos.Spec) {
		s.Name, s.NetworkMode = name+"-"+suffix, beside
		s.Labels = map[string]string{"dc-twin.test": "linuxnet"}
		c, err := r.d.Run(ctx, s)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Cleanup(func() {
			if t.Failed() {
				out, _ := r.d.Logs(context.Background(), c.ID)
				t.Logf("%s's log (tail):\n%s", name, indent(lastLines(out, 40)))
			}
			_ = r.d.Remove(context.Background(), c.ID)
		})
	}
	run("dctwin-nb-postgres", nos.Spec{Image: postgresImage, Env: []string{"POSTGRES_DB=netbox", "POSTGRES_USER=netbox", "POSTGRES_PASSWORD=netbox-db"}})
	run("dctwin-nb-valkey", nos.Spec{Image: valkeyImage, Cmd: []string{"valkey-server", "--requirepass", "netbox-cache"}})
	run("dctwin-nb-netbox", nos.Spec{Image: netboxImage, Memory: 2 << 30, Env: []string{
		"DB_HOST=localhost", "DB_NAME=netbox", "DB_USER=netbox", "DB_PASSWORD=netbox-db",
		"REDIS_HOST=localhost", "REDIS_PASSWORD=netbox-cache", "REDIS_DATABASE=0", "REDIS_SSL=false",
		"REDIS_CACHE_HOST=localhost", "REDIS_CACHE_PASSWORD=netbox-cache", "REDIS_CACHE_DATABASE=1", "REDIS_CACHE_SSL=false",
		"SECRET_KEY=dctwin-day0-netbox-secret-key-of-at-least-fifty-characters-0123456789",
		"SKIP_SUPERUSER=false", "SUPERUSER_NAME=admin", "SUPERUSER_EMAIL=admin@example.com",
		"SUPERUSER_PASSWORD=" + netboxAdminPW, "SUPERUSER_API_TOKEN=" + netboxToken,
	}})

	// the tooling's work directory: the NetBox CSVs and the inputs the glue reads, the leases,
	// the harness, and where the scripts are rendered
	work := filepath.Join(filepath.Dir(day0Binaries(t)), "netbox-"+suffix)
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	inputs := filepath.Join(r.tw.Runtime.StateDir, "stack-inputs", "day0")
	if err := copyTree(inputs, filepath.Join(work, "day0")); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(compat, filepath.Join(work, "harness")); err != nil {
		t.Fatal(err)
	}
	// the controller writes what it reads back here (host.json) as its own user, which owns
	// nothing on the host
	if err := os.Chmod(work, 0o777); err != nil {
		t.Fatal(err)
	}
	fac := r.factory()
	waitFor(t, 5*time.Minute, "every factory switch's management lease in Kea", func() bool {
		l := r.leases()
		for _, e := range fac {
			if l[strings.ToLower(e.MgmtMAC)] == "" {
				return false
			}
		}
		return true
	})
	var leases strings.Builder
	for mac, ip := range r.leases() {
		fmt.Fprintf(&leases, "%s,%s\n", mac, ip)
	}
	if err := os.WriteFile(filepath.Join(work, "leases.csv"), []byte(leases.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	ctl := "dctwin-nb-ansible-" + suffix
	c, err := r.d.Run(ctx, nos.Spec{Name: ctl, Image: netboxAnsibleImage, NetworkMode: beside, Entrypoint: []string{"/bin/sh", "-c"},
		Cmd: []string{"sleep infinity"}, Binds: []string{work + ":/work"}, Labels: map[string]string{"dc-twin.test": "linuxnet"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.d.Remove(context.Background(), c.ID) })
	step := func(what, cmd string) string {
		t.Helper()
		start := time.Now()
		out, code, err := r.d.Exec(ctx, c.ID, "/bin/sh", "-c", "cd /work/harness && "+cmd+" 2>&1")
		t.Logf("%s (%s, exit %d):\n%s", what, time.Since(start).Round(time.Second), code, indent(out))
		if err != nil || code != 0 {
			t.Fatalf("%s failed: exit %d, %v", what, code, err)
		}
		return out
	}
	const nb = "http://127.0.0.1:8080"
	waitFor(t, 10*time.Minute, "NetBox answering (its first start migrates the database)", func() bool {
		_, code, err := r.d.Exec(ctx, c.ID, "python3", "-c",
			"import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('"+nb+"/login/',timeout=5).status==200 else 1)")
		return err == nil && code == 0
	})
	step("import-csv.py: stack-inputs/day0/netbox through NetBox's CSV import",
		"python3 import-csv.py "+nb+" /work/day0/netbox --user admin --password "+netboxAdminPW)
	step("record-leases.py: the management leases as the switches' out-of-band addresses",
		"python3 record-leases.py "+nb+" /work/day0 --leases /work/leases.csv --token "+netboxToken)
	env := "NETBOX_API=" + nb + " NETBOX_TOKEN=" + netboxToken + " ANSIBLE_NOCOLOR=1 ANSIBLE_HOST_KEY_CHECKING=False ANSIBLE_INVENTORY_UNPARSED_FAILED=True "
	inv := step("ansible-inventory: the switches as netbox.netbox reads them", env+"ansible-inventory -i netbox.yml --graph")
	// the graph's hosts, one per line under their groups (`  |  |--leaf-1`)
	hosts := map[string]bool{}
	for _, l := range strings.Split(inv, "\n") {
		hosts[strings.TrimLeft(strings.TrimSpace(l), "|- ")] = true
	}
	for _, e := range fac {
		if !hosts[e.Hostname] {
			t.Fatalf("%s is not in the inventory NetBox gives", e.Hostname)
		}
	}
	if _, sw, _ := strings.Cut(inv, "@switches:"); strings.Contains(strings.SplitN(sw, "|--@", 2)[0], "mgmt-switch") {
		t.Fatalf("the inventory's switches include the OOB switches the site configures otherwise")
	}
	// what the template reads of one switch: its interfaces with their addresses and cable peers,
	// its custom fields (the whole of it checked; the log shortened, for when the port does not
	// render)
	step("ansible-inventory --host "+fac[0].Hostname, env+"ansible-inventory -i netbox.yml --host "+fac[0].Hostname+" > /work/host.json && head -c 6000 /work/host.json")
	b, err := os.ReadFile(filepath.Join(work, "host.json"))
	if err != nil {
		t.Fatal(err)
	}
	host := string(b)
	for _, want := range []string{`"interfaces"`, `"custom_fields"`, `"bgp_asn"`, `"link_peers"`, `"ip_addresses"`} {
		if !strings.Contains(host, want) {
			t.Errorf("the inventory's %s has no %s", fac[0].Hostname, want)
		}
	}
	// net-configurator's own playbook, unmodified, in its password-only mode: the factory
	// password changed at first login (PAM's prompt, answered by its expect task)
	step("net-configurator push-switch-configs.yml --tags bootstrap-password (nc-v8.0.0, unmodified)", env+
		"ansible-playbook -i netbox.yml /opt/enterprise-ras/net-configurator/playbooks/push-switch-configs.yml --tags bootstrap-password "+
		"-e switch_ansible_password="+newPassword+" -e switch_bootstrap_password="+nvue.FactoryPassword+" -e ansible_user="+nvue.User)
	step("push.yml: each switch's tree from NetBox with nvidia.nvue over NVUE's REST API", env+
		"ansible-playbook -i netbox.yml push.yml -e switch_ansible_password="+newPassword)
	took := r.waitConverged(10 * time.Minute)
	t.Logf("the fabric converged %s after nvidia.nvue's push from NetBox", took.Round(time.Second))
	r.checkPhases()
	e := fac[0]
	if st := r.nvueStatus(r.leases()[strings.ToLower(e.MgmtMAC)], nvue.User, newPassword); st != http.StatusOK {
		t.Errorf("%s: NVUE with the password the push set: %d", e.Hostname, st)
	}
	r.judge()
}

// copyTree copies a directory's regular files, recursively, readable by the container runtime.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(to, b, 0o644)
	})
}

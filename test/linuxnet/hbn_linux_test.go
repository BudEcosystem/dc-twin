//go:build linux

package linuxnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	yaml "go.yaml.in/yaml/v3"

	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
)

// (h) The emulated HBN against NICo's own oracle (D71, P1 of the DPU isolation design): every
// NVUE configuration NICo's agent's templates are tested to produce (internal/nos/nvue/testdata/
// nico, copied from NICo at its pin) is applied, one after another, to one emulated BlueField's
// container through its NVUE, as the agent applies it (a revision emptied, the configuration
// patched in, applied), and FRR (frr-reload.py), ifupdown2 (ifreload -a) and nftables (nft -f)
// accept every one: the revision is `applied`, FRR runs the configuration's BGP AS and route
// maps, the kernel has its VRFs, and the ACL tables are loaded where it binds ACLs.
func TestHBNAppliesNICoConfigs(t *testing.T) {
	need(t)
	d, err := nosRuntime(t)
	if err != nil {
		t.Skipf("no Docker Engine for the NOS runtime: %v", err)
	}
	ctx := context.Background()
	if !d.HasImage(ctx, nos.HBNImage) {
		t.Skipf("image %s is not present (scripts/linux-substrate-e2e.sh builds it)", nos.HBNImage)
	}
	sw := nvue.HBNSwitch("site-1/hall-a/r01/server-01/bf3-0", "r01-server-01-bf3-0")
	e, _ := nos.Lookup(nos.HBN)
	script, err := e.StartScript(sw)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("dctwin-hbn-oracle-%d", os.Getpid())
	c, err := d.Run(ctx, nos.Spec{Name: name, Image: nos.HBNImage, Hostname: sw.Hostname, Entrypoint: nos.Entrypoint, Cmd: []string{script},
		Labels: map[string]string{"dc-twin.test": "linuxnet"}, Sysctls: e.Sysctls})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Remove(context.Background(), c.ID) })
	// nothing is cabled: the start script makes every HBN interface a dummy
	waitFor(t, 90*time.Second, "the HBN container to start", func() bool {
		logs, _ := d.Logs(ctx, c.ID)
		return nos.Started(logs)
	})
	exec := func(args ...string) string {
		out, _, _ := d.Exec(ctx, c.ID, args...)
		return out
	}
	srv := nvue.NewServer(sw, "secret", func(ctx context.Context, f nvue.Files) (string, error) {
		out, code, err := d.Exec(ctx, c.ID, append([]string{"sh", "-c", nvue.ApplyScript, "sh"}, nvue.ApplyArgs(f)...)...)
		if err == nil && code != 0 {
			err = fmt.Errorf("exit %d:\n%s", code, strings.TrimSpace(out))
		}
		return out, err
	})
	if err := srv.ApplyStartup(ctx); err != nil {
		t.Fatal(err)
	}
	// testdata/nico is internal/nos/nvue's, which the e2e script copies beside the test
	files, _ := filepath.Glob("testdata/nico/*.yaml.expected")
	if len(files) == 0 {
		files, _ = filepath.Glob("../../internal/nos/nvue/testdata/nico/*.yaml.expected")
	}
	if len(files) < 15 {
		t.Fatalf("%d of NICo's configurations", len(files))
	}
	applied := 0
	for _, f := range files {
		base := filepath.Base(f)
		if base == "nvue_startup_fnn_classic_with_empty_nsg_default_deny.yaml.expected" {
			continue // two VPCs with one L3 VNI: NVUE refuses it (internal/nos/nvue's test)
		}
		cfg := readNICoConfig(t, f)
		st, out := applyLikeNICo(t, srv, cfg)
		if st != "applied" {
			t.Errorf("%s: %s: %v\n%s", base, st, out, indent(exec("sh", "-c", "ps -eo pid,args | grep -E '[b]gpd|[z]ebra|[w]atchfrr'; grep -iE 'bgpd|backtrace|assert|signal' /var/log/syslog 2>/dev/null | tail -n 40")))
			continue
		}
		applied++
		asn := strconv.FormatFloat(nvueGet(cfg, "router", "bgp", "autonomous-system").(float64), 'f', -1, 64)
		run := exec("vtysh", "-c", "show running-config")
		if !strings.Contains(run, "router bgp "+asn) {
			t.Errorf("%s: FRR does not run router bgp %s:\n%s", base, asn, indent(run))
		}
		vrfs := exec("ip", "-br", "link", "show", "type", "vrf")
		for v := range asMap(cfg["vrf"]) {
			if v != "default" && !strings.Contains(vrfs, v) {
				t.Errorf("%s: no VRF device %s: %s", base, v, vrfs)
			}
		}
		for rm := range asMap(nvueGet(cfg, "router", "policy", "route-map")) {
			if !strings.Contains(run, "route-map "+rm+" ") {
				t.Errorf("%s: FRR has no route-map %s", base, rm)
			}
		}
		if strings.Contains(fmt.Sprint(cfg["interface"]), "acl:") {
			if nft := exec("nft", "list", "table", "inet", "dctwin_acl"); !strings.Contains(nft, "jump acl_") {
				t.Errorf("%s: the ACLs are not loaded:\n%s", base, indent(nft))
			}
		}
		t.Logf("%s: applied; FRR runs AS %s, %d VRFs", base, asn, len(asMap(cfg["vrf"])))
	}
	if applied < len(files)-1 {
		t.Errorf("%d of %d of NICo's configurations applied", applied, len(files)-1)
	}
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func nvueGet(v any, path ...string) any {
	for _, p := range path {
		v = asMap(v)[p]
	}
	return v
}

func readNICoConfig(t *testing.T, file string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var doc []map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	for _, e := range doc {
		if set, ok := e["set"]; ok {
			j, err := json.Marshal(stringKeys(set))
			if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			var out map[string]any
			_ = json.Unmarshal(j, &out)
			return out
		}
	}
	t.Fatalf("%s: no set", file)
	return nil
}

// applyLikeNICo drives the server's REST API in process as NICo's push_config does.
func applyLikeNICo(t *testing.T, srv *nvue.Server, cfg map[string]any) (string, map[string]any) {
	t.Helper()
	hc := &nvueClient{t: t, in: nvueInProcess(), pw: "secret", http: inProcessClient(srv)}
	return hc.push(cfg)
}

// stringKeys makes YAML's mappings JSON's objects (a numeric key is a string in NVUE's JSON).
func stringKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			x[k] = stringKeys(c)
		}
		return x
	case map[any]any:
		out := map[string]any{}
		for k, c := range x {
			out[fmt.Sprint(k)] = stringKeys(c)
		}
		return out
	case []any:
		for i := range x {
			x[i] = stringKeys(x[i])
		}
	}
	return v
}

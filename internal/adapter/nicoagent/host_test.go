package nicoagent

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/nvl"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// upNVL brings up compat/nico's GB300 NVL72 rack (its network rack for the frontend fabric, no
// OEM racks) with both impersonators, the BMC network relayed to the fake NICo.
func upNVL(t *testing.T, nico *fakeNICo) (*twin.Twin, *Adapter) {
	return upNICoEstate(t, nico, false, DPUAgent, Scout)
}

// upNICoEstate is compat/nico's estate: the GB300 rack, and the OEM racks when oem is set (their
// provisioning network relayed to the fake NICo too), with the impersonators named.
func upNICoEstate(t *testing.T, nico *fakeNICo, oem bool, imps ...string) (*twin.Twin, *Adapter) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../../examples/estates/nvl72-nico.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !oem {
		est.Spec.Sites[0].Halls[0].Rows = est.Spec.Sites[0].Halls[0].Rows[:1]
	}
	var fabrics []schema.Fabric
	for _, f := range est.Spec.Fabrics {
		if f.Name != "backend" {
			fabrics = append(fabrics, f)
		}
	}
	est.Spec.Fabrics = fabrics
	delete(est.Spec.Networks, "backend")
	dhcpAddr, services := nico.start()
	nw := est.Spec.Networks["oob"]
	nw.Server = dhcpAddr
	est.Spec.Networks["oob"] = nw
	if oem {
		pn := est.Spec.Networks["provisioning"]
		pn.DHCP, pn.Server = "stack", dhcpAddr
		est.Spec.Networks["provisioning"] = pn
	}
	est.Spec.Stacks = []schema.StackRef{{Name: "nvidia-dsx-os@0.1.0", Impersonators: imps}}
	tw, err := twin.UpEstate(est, "", reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{"dhcp", "redfish", "nicoagent-test"},
		Outputs: map[string]string{"siteServices": services}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	a := tw.Adapter(Name).(*Adapter)
	t.Cleanup(func() {
		if t.Failed() {
			for i, l := range a.Report().Lines {
				if i < 3 || strings.Contains(l, "/b0") {
					t.Logf("report: %s", l)
				}
			}
			nico.mu.Lock()
			t.Logf("NICo saw: calls %v", nico.calls)
			nico.mu.Unlock()
		}
	})
	return tw, a
}

// Every GB300 tray of a model NVL72 rack, its BlueField impersonated, is discovered by scout as
// NICo discovers one: restarted once its DPU's agent reports, the tray boots through its DPU
// (the DPU's DHCP server answers it with the admin address NICo gave the DPU's host interface,
// and NICo's boot service with scout), and scout reports the tray with the BMC's serial, a TPM EK
// certificate the estate CA signs, the DPU's host PF as its NIC, and its four GPUs with the
// NVLink placement and fabric GUIDs the twin's NMX-C reports; then RebootCompleted, and NICo's
// instructions answered: discovery completed, cleanup completed, validation completed.
func TestModelTraysAreDiscoveredByScoutThroughTheirDPUs(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	nico := newFakeNICo(t, forgeOrSkip(t, reg))
	tw, a := upNVL(t, nico)
	if len(a.dpus) != 18 || len(a.hosts) < 18 {
		t.Fatalf("%d DPUs and %d hosts, want the rack's 18 trays and their DPUs", len(a.dpus), len(a.hosts))
	}
	waitFor(t, "every DPU's agent", 90*time.Second, func() bool {
		for _, b := range a.dpus {
			if !b.agentUp.Load() {
				return false
			}
		}
		return true
	})
	// NICo restarts each host once its DPU is ready
	src := schema.EventSource{Class: "scenario", Principal: "test"}
	var trays []*host
	for _, h := range a.hosts {
		if h.dpu == nil {
			continue
		}
		trays = append(trays, h)
		if err := tw.Engine.Power(engine.Request{Path: h.path, Canonical: "power.restart", Protocol: "test", Source: src}); err != nil {
			t.Fatal(err)
		}
	}
	if len(trays) != 18 {
		t.Fatalf("%d hosts behind an impersonated DPU, want 18", len(trays))
	}
	waitFor(t, "every tray's scout to finish NICo's instructions", 120*time.Second, func() bool {
		nico.mu.Lock()
		defer nico.mu.Unlock()
		for _, h := range trays {
			if nico.hostStep["host-"+h.sm.serial] < 2 {
				return false
			}
		}
		return true
	})
	ca, _ := pem.Decode(tw.Net.CAPEM)
	caCert, err := x509.ParseCertificate(ca.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	seed := tw.Engine.M.Seed
	for _, h := range trays {
		id := "host-" + h.dev.Identity.Serial
		nico.mu.Lock()
		info := nico.scouted[id]
		rebooted := nico.rebooted[id]
		completed := nico.completed[id]
		adminIP := ""
		for dpuID, ip := range nico.admin {
			if dpuID == "dpu-"+h.dpu.id.serial {
				adminIP = ip
			}
		}
		nico.mu.Unlock()
		if info == nil {
			t.Fatalf("%s: no scout discovery as %s", h.path, id)
		}
		if !rebooted || !completed {
			t.Errorf("%s: RebootCompleted %v, DiscoveryCompleted %v", h.path, rebooted, completed)
		}
		// the EK certificate is the estate CA's, and the same on every boot (NICo's machine id)
		der, err := base64.StdEncoding.DecodeString(str(info, "tpm_ek_certificate"))
		if err != nil {
			t.Fatalf("%s: EK certificate: %v", h.path, err)
		}
		ek, err := x509.ParseCertificate(der)
		if err != nil || ek.CheckSignatureFrom(caCert) != nil {
			t.Errorf("%s: EK certificate not signed by the estate CA: %v", h.path, err)
		}
		// its NIC is its DPU's host PF, addressed by the DPU's DHCP server from NICo's config
		ifs, _ := info["network_interfaces"].([]any)
		if len(ifs) != 1 || str(ifs[0].(map[string]any), "mac_address") != h.dpu.id.hostMAC {
			t.Errorf("%s: NICs %v, want the DPU's host PF %s", h.path, info["network_interfaces"], h.dpu.id.hostMAC)
		}
		h.mu.Lock()
		lease := h.lease
		h.mu.Unlock()
		if adminIP == "" || lease != adminIP {
			t.Errorf("%s: leased %q, want the admin address %q NICo gave its DPU's host", h.path, lease, adminIP)
		}
		// GPUs with the placement NMX-C reports for them
		gpus, _ := info["gpus"].([]any)
		var accels []*model.Component
		for _, c := range h.dev.Components {
			if c.Kind == "accelerator" {
				accels = append(accels, c)
			}
		}
		if len(gpus) != len(accels) || len(gpus) == 0 {
			t.Fatalf("%s: %d GPUs reported, the model has %d", h.path, len(gpus), len(accels))
		}
		for i, g := range gpus {
			gm := g.(map[string]any)
			pi, _ := gm["platform_info"].(map[string]any)
			want := fmt.Sprintf("0x%016x", nvl.FabricGUID(seed, accels[i]))
			if str(pi, "fabric_guid") != want || str(pi, "chassis_serial") != nvl.ChassisSerial(seed, h.dev.Rack) {
				t.Errorf("%s GPU %d: platform info %v, want fabric GUID %s on chassis %s", h.path, i, pi, want, nvl.ChassisSerial(seed, h.dev.Rack))
			}
			if !strings.Contains(str(gm, "name"), "GB300") {
				t.Errorf("%s GPU %d: name %q (NICo knows a multi-node NVLink GPU by it)", h.path, i, str(gm, "name"))
			}
		}
		var booted string
		tw.Engine.Read(func(m *model.Model) { booted = m.DeviceBy[h.path].State.Booted })
		if booted != stepOS {
			t.Errorf("%s: boot progress %q, want %q", h.path, booted, stepOS)
		}
	}
	for _, l := range a.Report().Lines[:1] {
		t.Logf("report: %s", l)
	}
}

// A server with no DPU (the R760s and the HPE of compat/nico's CPU rack) is discovered by scout
// on its own provisioning port: its firmware's DHCP through the twin relay to NICo's server, as
// the Ready path's DGX does, NICo's iPXE and boot service, then scout with its BMC's identity.
func TestZeroDPUServerIsDiscoveredOnItsProvisioningPort(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	nico := newFakeNICo(t, forgeOrSkip(t, reg))
	tw, a := upNICoEstate(t, nico, true, Scout)
	var servers []*host
	for _, h := range a.hosts {
		if strings.Contains(h.path, "/b04/") {
			servers = append(servers, h)
		}
	}
	if len(servers) != 2 {
		t.Fatalf("%d servers in b04, want 2", len(servers))
	}
	src := schema.EventSource{Class: "scenario", Principal: "test"}
	for _, h := range servers {
		if err := tw.Engine.Power(engine.Request{Path: h.path, Canonical: "power.on", Protocol: "test", Source: src}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "both servers' scout", 90*time.Second, func() bool {
		nico.mu.Lock()
		defer nico.mu.Unlock()
		for _, h := range servers {
			if nico.hostStep["host-"+h.dev.Identity.Serial] < 2 {
				return false
			}
		}
		return true
	})
	for _, h := range servers {
		p := provisioningPort(h.dev)
		nico.mu.Lock()
		reqs := nico.dhcpSeen[strings.ToLower(p.MAC)]
		info := nico.scouted["host-"+h.dev.Identity.Serial]
		nico.mu.Unlock()
		if len(reqs) == 0 || !strings.HasPrefix(reqs[0].class, "HTTPClient:Arch:00016") || reqs[0].circuit == "" {
			t.Errorf("%s: NICo's DHCP saw %+v from its provisioning port", h.path, reqs)
		}
		ifs, _ := info["network_interfaces"].([]any)
		if len(ifs) != 1 || str(ifs[0].(map[string]any), "mac_address") != strings.ToLower(p.MAC) || str(info, "machine_arch") != "X86_64" {
			t.Errorf("%s: scout reported %v", h.path, info)
		}
	}
}

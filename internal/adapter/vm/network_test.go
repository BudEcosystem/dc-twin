package vm_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	_ "github.com/BudEcosystem/dc-twin/internal/adapter/dhcp"
	"github.com/BudEcosystem/dc-twin/internal/adapter/redfish"
	"github.com/BudEcosystem/dc-twin/internal/adapter/vm"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// vmHost is a vm host of the host's own architecture, accelerated, booting the stack's OS (so
// from its firmware's boot order: the network) on a provisioning network the twin site DHCP
// serves: a GB300 tray on an arm64 host, a DGX H100 on an x86_64 one.
type vmHost struct {
	tw           *twin.Twin
	path, boot   string // the device, and the port it boots from
	leaf, system string // the switch the boot port is cabled to, the BMC's system resource
}

func upVMHost(t *testing.T, adapters ...string) vmHost {
	t.Helper()
	if testing.Short() {
		t.Skip("boots a virtual machine")
	}
	arch := qemu.HostArch()
	if _, err := qemu.Find(arch, "", false); err != nil {
		t.Skipf("needs QEMU for an accelerated %s guest: %v", arch, err)
	}
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	var h vmHost
	file := "../../../examples/estates/gb300-vm.yaml"
	h.path, h.boot, h.leaf, h.system = "site-1/hall-a/a01/compute-tray-01", "bf3-0:p0", "leaf-a01-1:", "/redfish/v1/Systems/System_0"
	if arch == "x86_64" {
		file = "../../../examples/estates/dgx-nico-ready.yaml"
		h.path, h.boot, h.leaf, h.system = "site-1/hall-a/b01/server-01", "eth0", "leaf-b01-1:", "/redfish/v1/Systems/DGX"
	}
	est, _, err := compile.LoadEstate(file)
	if err != nil {
		t.Fatal(err)
	}
	for i := range est.Spec.Fidelity.Overrides {
		if est.Spec.Fidelity.Overrides[i].OS == "twin" {
			est.Spec.Fidelity.Overrides[i].OS = "stack"
		}
	}
	for name, n := range est.Spec.Networks {
		if n.DHCP == "stack" {
			n.DHCP, n.Server = "twin", ""
			est.Spec.Networks[name] = n
		}
	}
	est.Spec.Stacks = nil
	est.Spec.Adapters.VM.TPM = "off"
	h.tw, err = twin.UpEstate(est, "", reg, twin.Options{StateDir: t.TempDir(), Adapters: adapters})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.tw.Down)
	if p := strings.Split(h.boot, ":"); len(p) == 2 {
		h.boot = h.path + "/" + p[0] + ":" + p[1]
	} else {
		h.boot = h.path + ":" + h.boot
	}
	return h
}

// A vm host's NIC is on the twin's segment (D48): the host's UEFI firmware broadcasts its PXE
// DHCP from the NIC with the model's MAC; it reaches the twin relay of the segment the port is
// cabled to, through the twin's switch port at the far end of QEMU's stream socket, and the
// segment's server (the twin site DHCP here) answers back through it.
func TestGuestFirmwareDHCPsThroughTheTwinRelay(t *testing.T) {
	h := upVMHost(t, "dhcp", "vm")
	va := h.tw.Adapter("vm").(*vm.Adapter)
	var x vm.NIC
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range va.NICs(h.path) {
			if n.Port == h.boot {
				x = n
			}
		}
		if x.Replies > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	t.Logf("%+v", x)
	if !x.Linked {
		t.Fatal("QEMU did not connect the NIC's stream socket")
	}
	if x.DHCPRequests == 0 || x.Replies == 0 {
		t.Fatalf("the guest's DHCP through the port: %d requests relayed, %d replies delivered", x.DHCPRequests, x.Replies)
	}
	want := map[string]string{"aarch64": "PXEClient:Arch:00011", "x86_64": "PXEClient:Arch:00007"}[qemu.HostArch()]
	pxe := false
	for c := range x.Classes {
		// EDK2's PXE client: PXEClient:Arch:00011 (AArch64 UEFI) or 00007 (x64 UEFI)
		pxe = pxe || strings.HasPrefix(c, want)
	}
	if !pxe {
		t.Fatalf("no request from the firmware's PXE client (%s): %v", want, x.Classes)
	}
	// the segment's gateway is the .1 of the rack's /24 on the provisioning network (10.40.0.0/16)
	if !strings.HasPrefix(x.Gateway, "10.40.") || !strings.HasSuffix(x.Gateway, ".1/24") || !strings.Contains(x.Server, "twin site DHCP") || !strings.HasPrefix(x.Peer, h.leaf) {
		t.Fatalf("segment: gateway %q, server %q, peer %q", x.Gateway, x.Server, x.Peer)
	}
	rep := strings.Join(h.tw.Adapter("dhcp").Report().Lines, "\n")
	if !strings.Contains(rep, "1 vm host ports plugged into their segment's relay") {
		t.Fatalf("the dhcp report does not show the plugged port:\n%s", rep)
	}
}

// A vm host's boot options are its firmware's (D48): the BMC serves the load options the guest's
// UEFI firmware created for its NICs, with their device paths and what each boots (Alias), and a
// boot order set through the BMC is written into the firmware's variable store before the host
// next starts, so the firmware boots what the BMC says.
func TestBootOptionsAreTheFirmwares(t *testing.T) {
	h := upVMHost(t, "redfish", "dhcp", "vm")
	tw := h.tw
	rf := tw.Adapter("redfish").(*redfish.Adapter)
	va := tw.Adapter("vm").(*vm.Adapter)
	i := strings.LastIndex(h.boot, ":")
	dev, port := h.boot[:i], h.boot[i+1:]
	mac := strings.ToUpper(strings.ReplaceAll(tw.Engine.M.DeviceBy[dev].PortBy[port].MAC, ":", ""))
	want := "UEFI HTTPv4 (MAC:" + mac + ")"
	var http4 string
	var ids []string
	deadline := time.Now().Add(60 * time.Second)
	for http4 == "" && time.Now().Before(deadline) {
		time.Sleep(time.Second)
		coll, ok := rf.Get(h.path, h.system+"/BootOptions")
		if !ok {
			continue
		}
		ids = nil
		for _, m := range coll["Members"].([]any) {
			ref := m.(map[string]any)["@odata.id"].(string)
			opt, _ := rf.Get(h.path, ref)
			ids = append(ids, fmt.Sprint(opt["BootOptionReference"]))
			if opt["DisplayName"] == want {
				// BootOrder names an option by its reference, the firmware's variable ("Boot0004");
				// its Id is the variable's number on a BMC that numbers its options so (the DGX's)
				http4 = opt["BootOptionReference"].(string)
				if id := fmt.Sprint(opt["Id"]); id != http4 && "Boot"+id != http4 {
					t.Fatalf("the boot option's Id %s is neither its reference %s nor its number", id, http4)
				}
				if !strings.HasSuffix(ref, "/"+fmt.Sprint(opt["Id"])) {
					t.Fatalf("the boot option at %s has Id %v", ref, opt["Id"])
				}
				path := fmt.Sprint(opt["UefiDevicePath"])
				if opt["Alias"] != "UefiHttp" || !strings.Contains(path, "MAC("+mac) || !strings.Contains(path, "IPv4(") || !strings.HasSuffix(path, "Uri()") {
					t.Fatalf("the HTTP boot option: alias %v, device path %s", opt["Alias"], path)
				}
				if opt["@odata.etag"] == nil {
					t.Fatal("the boot option has no @odata.etag")
				}
			}
		}
	}
	if http4 == "" {
		t.Fatalf("the BMC serves no boot option %q (options %v)", want, ids)
	}
	// the system's EthernetInterfaces are the NICs the firmware enumerated, its boot NIC among them
	colon := strings.ToUpper(tw.Engine.M.DeviceBy[dev].PortBy[port].MAC)
	nics, _ := rf.Get(h.path, h.system+"/EthernetInterfaces")
	var found bool
	var macs []string
	for _, m := range nics["Members"].([]any) {
		e, _ := rf.Get(h.path, m.(map[string]any)["@odata.id"].(string))
		macs = append(macs, fmt.Sprint(e["MACAddress"]))
		if e["MACAddress"] == colon && e["InterfaceEnabled"] == true && strings.Contains(fmt.Sprint(e["UefiDevicePath"]), "MAC("+mac) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the system's EthernetInterfaces %v do not include the boot NIC %s", macs, colon)
	}
	doc, _ := rf.Get(h.path, h.system)
	order := doc["Boot"].(map[string]any)["BootOrder"].([]any)
	if len(order) != len(ids) {
		t.Fatalf("BootOrder %v is not the firmware's %d options", order, len(ids))
	}
	// a client puts HTTP boot over the boot NIC first, and restarts the host
	next := []string{http4}
	for _, o := range order {
		if o != http4 {
			next = append(next, o.(string))
		}
	}
	src := schema.EventSource{Class: "user", Principal: "test"}
	if err := tw.Engine.SetBoot(engine.Request{Path: h.path, Canonical: "boot.order", Protocol: "redfish", Source: src}, engine.BootPatch{Order: next}); err != nil {
		t.Fatal(err)
	}
	doc, _ = rf.Get(h.path, h.system)
	if got := fmt.Sprint(doc["Boot"].(map[string]any)["BootOrder"]); got != fmt.Sprint(next) {
		t.Fatalf("BootOrder after the PATCH: %s, want %v", got, next)
	}
	// a system with a settings object (the DGX's SD) shows the same order there, where a client
	// such as NICo's DGX implementation reads the order it reorders
	if set, ok := doc["@Redfish.Settings"].(map[string]any); ok {
		so := set["SettingsObject"].(map[string]any)["@odata.id"].(string)
		sd, ok := rf.Get(h.path, so)
		if !ok {
			t.Fatalf("no settings object %s", so)
		}
		if got := fmt.Sprint(sd["Boot"].(map[string]any)["BootOrder"]); got != fmt.Sprint(next) {
			t.Fatalf("BootOrder in %s: %s, want %v", so, got, next)
		}
	}
	starts := va.Machines()[0].Starts
	if err := tw.Engine.Power(engine.Request{Path: h.path, Canonical: "power.restart", Protocol: "redfish", Source: src}); err != nil {
		t.Fatal(err)
	}
	// the restart is a cold start (a boot order is pending): wait for the new process, then count
	// the guest's DHCP requests from there
	for i := 0; i < 300 && va.Machines()[0].Starts == starts; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	seen := len(nicOf(va, h).Requests)
	mi := va.Machines()[0]
	var nv *qemu.NVRAM
	var err error
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		if nv, err = qemu.ReadNVRAM(mi.NVRAM); err == nil && len(nv.BootOrder) > 0 && nv.BootOrder[0] == http4 {
			break
		}
	}
	if nv == nil || len(nv.BootOrder) == 0 || nv.BootOrder[0] != http4 {
		t.Fatalf("the firmware's BootOrder after the restart: %+v, want %s first", nv, http4)
	}
	// and the firmware boots it first: its first DHCP after the restart is its HTTP boot client's
	// (HTTPClient:Arch:00016 on x64 UEFI, 00019 on AArch64), not its PXE client's
	deadline = time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		r := nicOf(va, h).Requests
		if len(r) <= seen {
			continue
		}
		if !strings.HasPrefix(r[seen], "HTTPClient") {
			t.Fatalf("the firmware's first DHCP after the restart is %q, not its HTTP boot client's (the option the BMC put first); requests %v", r[seen], r)
		}
		return
	}
	t.Fatal("the firmware sent no DHCP after the restart")
}

func nicOf(va *vm.Adapter, h vmHost) vm.NIC {
	for _, n := range va.NICs(h.path) {
		if n.Port == h.boot {
			return n
		}
	}
	return vm.NIC{}
}

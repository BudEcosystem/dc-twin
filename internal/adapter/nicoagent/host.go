package nicoagent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/adapter/nvl"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// NICo's discovery agent, scout, for a host at `model` fidelity (D55). NICo boots a host it is
// ingesting into scout over the network: a host behind a DPU through the DPU (its host-facing
// port, answered by the DPU's DHCP server, dpuserve.go), a host without one on its provisioning
// port through the twin relay, as the DGX of the Ready path does. Scout reports the host's
// hardware (DiscoverMachine, reporter SCOUT) and that the host has rebooted into it
// (RebootCompleted), then asks NICo what to do (ForgeAgentControl) until the host is reset:
// finish discovery (DiscoveryCompleted), clean up (CleanupMachineCompleted, every step OK: no
// disk is wiped), or validate (MachineValidationCompleted, no test run). What it reports is the
// host's own: the identity its BMC serves, a TPM endorsement-key certificate kept for the host
// and signed by the estate CA (which a stack loads as its TPM vendors' CA, as the Ready path
// loads it for swtpm's), its boot NIC, and its GPUs with the NVLink placement NVML reports and
// the twin's NMX-C and RMS report for the same GPUs (package nvl): the rack's chassis serial, the
// tray's slot and index, and each GPU's fabric GUID, by which NICo joins NMX-C's partition
// membership to its machines.

type host struct {
	*machine
	dpu *dpu
	ek  []byte // the TPM's EK certificate (DER)
	nicDesc
}

// nicDesc is how scout describes the boot NIC: its PCI identity.
type nicDesc struct{ vendor, device, description string }

func (h *host) runHost(ctx context.Context, why string) {
	if err := h.prepare(); err != nil {
		h.end("not booted: " + err.Error())
		return
	}
	out := h.netbootRetrying(ctx)
	if ctx.Err() != nil {
		return
	}
	if out.kernel != "" {
		h.end("booted " + baseName(out.kernel) + " (" + why + ")")
		h.retry(ctx, func() error { return h.runScout(ctx, out) })
		return
	}
	if out.failed != "" {
		h.note("network boot: " + out.failed)
	}
	// the disk: NICo installs an OS only for an instance, which nothing here runs
	h.end("no OS: the network boot ended (" + firstOf(out.failed, "exit") + ") and the host's disk boots nothing the twin runs")
}

// prepare reads the host's identity from its BMC, keeps its EK certificate, and cables its NIC,
// once.
func (h *host) prepare() error {
	h.prepMu.Lock()
	defer h.prepMu.Unlock()
	if h.nic != nil {
		return nil
	}
	// the host's SMBIOS as the vm adapter gives a vm host's firmware (the profile's manufacturer
	// and product, the device's serial and UUID): what scout reads from /sys/class/dmi
	var sm smbios
	h.a.rt.Engine.Read(func(*model.Model) {
		if id := h.dev.Profile.Spec.Identity; id != nil {
			sm.manufacturer, sm.product = id.Manufacturer, id.Model
		}
		sm.serial, sm.uuid = h.dev.Identity.Serial, h.dev.Identity.UUID
	})
	if sm.serial == "" {
		return fmt.Errorf("%s has no serial", h.path)
	}
	h.sm = sm
	if err := h.loadEK(); err != nil {
		return err
	}
	var ref string
	var plug func() (netsim.PortRelay, error)
	if h.dpu != nil {
		// behind its DPU: the host PF's MAC, out of the DPU's data port, answered by the DPU
		if err := h.dpu.prepare(); err != nil {
			return fmt.Errorf("its DPU %s: %w", h.dpu.path, err)
		}
		mac, err := net.ParseMAC(h.dpu.id.hostMAC)
		if err != nil {
			return fmt.Errorf("its DPU serves no usable host MAC (%q)", h.dpu.id.hostMAC)
		}
		h.mac = mac
		p := dataPort(h.dpu.dev)
		if p == nil {
			return fmt.Errorf("its DPU %s has no cabled data port", h.dpu.path)
		}
		ref, plug = p.Ref, h.dpu.hostDHCP
		h.nicDesc = nicDesc{"Mellanox Technologies", firstOf(h.dpu.id.product, "BlueField-3 SmartNIC Main Card"), "MT43244 BlueField-3 integrated ConnectX-7 network controller"}
	} else {
		p := provisioningPort(h.dev)
		if p == nil {
			return fmt.Errorf("%s has no cabled provisioning port", h.path)
		}
		mac, err := net.ParseMAC(p.MAC)
		if err != nil {
			return fmt.Errorf("%s's provisioning port has no MAC", h.path)
		}
		h.mac = mac
		r := p.Ref
		ref, plug = r, func() (netsim.PortRelay, error) { return h.a.rt.Net.PlugRelay(r, nil) }
		h.nicDesc = nicDesc{"Intel Corporation", "Ethernet Controller", ""}
	}
	n, err := newNIC(h.mac, h.a.portConfig(ref, plug))
	if err != nil {
		return err
	}
	h.nic = n
	return nil
}

// dataPort is a DPU's first cabled Ethernet port that is not its management port (p0).
func dataPort(d *model.Device) *model.Port {
	var first *model.Port
	for _, p := range d.Ports {
		if p.Kind != "ethernet" || p.Role == "management" || p.Link == nil {
			continue
		}
		if p.Role == "provisioning" {
			return p
		}
		if first == nil {
			first = p
		}
	}
	return first
}

// provisioningPort is a host's own cabled provisioning port (a zero-DPU host's boot NIC).
func provisioningPort(d *model.Device) *model.Port {
	for _, p := range d.Ports {
		if p.Role == "provisioning" && p.Kind == "ethernet" && p.Link != nil && p.MAC != "" {
			return p
		}
	}
	return nil
}

// loadEK keeps the host's EK certificate across runs: NICo derives the host's machine id from it.
func (h *host) loadEK() error {
	path := h.statePath + ".ek"
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		h.ek = b
		return nil
	}
	c, err := h.a.rt.Net.Certificate("TPM EK "+h.sm.serial, nil, nil)
	if err != nil {
		return fmt.Errorf("the TPM's EK certificate: %w", err)
	}
	h.ek = c.Certificate[0]
	return os.WriteFile(path, h.ek, 0o600)
}

// runScout is scout: discovery, then NICo's instructions, until the host resets or powers off.
func (h *host) runScout(ctx context.Context, out bootOutcome) error {
	in := &installed{
		InterfaceID: cmdlineArg(out.cmdline, "machine_id"),
		APIURL:      cmdlineArg(out.cmdline, "server_uri"),
		PXEURL:      cmdlineArg(out.cmdline, "pxe_uri"),
	}
	if in.APIURL == "" || strings.HasPrefix(in.APIURL, "[") {
		return fmt.Errorf("the kernel command line names no server_uri NICo filled in: %q", out.cmdline)
	}
	if in.PXEURL != "" && !strings.HasPrefix(in.PXEURL, "[") {
		u, err := url.Parse(strings.TrimRight(in.PXEURL, "/") + "/api/v0/tls/root_ca")
		if err != nil {
			return err
		}
		ca, err := h.get(ctx, u, true)
		if err != nil {
			return fmt.Errorf("the site's root CA: %w", err)
		}
		in.RootCA = ca
	}
	conn, err := h.connect(in)
	if err != nil {
		return err
	}
	req := obj{
		"info":               h.discoveryInfo(),
		"create_machine":     true,
		"discovery_reporter": "MACHINE_DISCOVERY_REPORTER_SCOUT",
	}
	if in.InterfaceID != "" {
		req["machine_interface_id"] = obj{"value": in.InterfaceID}
	}
	res, err := h.forge.call(ctx, conn, "DiscoverMachine", req)
	conn.Close()
	if err != nil {
		return fmt.Errorf("DiscoverMachine: %w", err)
	}
	h.called("DiscoverMachine")
	in.MachineID = str(res, "machine_id", "id")
	if in.MachineID == "" {
		return errors.New("DiscoverMachine returned no machine id")
	}
	if mc, ok := res["machine_certificate"].(map[string]any); ok {
		in.Cert, _ = base64.StdEncoding.DecodeString(fmt.Sprint(mc["public_key"]))
		in.Key, _ = base64.StdEncoding.DecodeString(fmt.Sprint(mc["private_key"]))
	}
	_ = h.saveInstalled(in)
	h.reach("discovered")
	h.note(fmt.Sprintf("discovered as NICo machine %s", in.MachineID))
	conn, err = h.connect(in)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := h.forge.call(ctx, conn, "RebootCompleted", obj{"machine_id": obj{"id": in.MachineID}}); err != nil {
		h.callFailed("RebootCompleted", err)
	} else {
		h.called("RebootCompleted")
	}
	h.step(stepOS)
	h.agentUp.Store(true)
	defer h.agentUp.Store(false)
	t := time.NewTicker(h.a.controlEvery)
	defer t.Stop()
	for {
		h.scoutControl(ctx, conn, in)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (h *host) scoutControl(ctx context.Context, conn *grpc.ClientConn, in *installed) {
	id := obj{"id": in.MachineID}
	res, err := h.forge.call(ctx, conn, "ForgeAgentControl", obj{"machine_id": id})
	if err != nil {
		h.callFailed("ForgeAgentControl", err)
		return
	}
	h.called("ForgeAgentControl")
	legacy, _ := res["legacy_action"].(string)
	var method string
	var req obj
	switch {
	case res["discovery"] != nil || legacy == "DISCOVERY":
		method, req = "DiscoveryCompleted", obj{"machine_id": id}
	case res["reset"] != nil || legacy == "RESET":
		ok := obj{"result": "OK", "message": ""}
		method, req = "CleanupMachineCompleted", obj{"machine_id": id, "nvme": ok, "ram": ok, "mem_overwrite": ok, "ib": ok, "hdd": ok, "result": "OK"}
	case res["machine_validation"] != nil || legacy == "MACHINE_VALIDATION":
		mv, _ := res["machine_validation"].(map[string]any)
		v := str(mv, "validation_id", "value")
		if v == "" {
			return
		}
		method, req = "MachineValidationCompleted", obj{"machine_id": id, "validation_id": obj{"value": v}}
	default:
		return // noop, retry: nothing to do yet
	}
	if _, err := h.forge.call(ctx, conn, method, req); err != nil {
		h.callFailed(method, err)
		return
	}
	h.called(method)
}

// discoveryInfo is the host's hardware as scout reports it.
func (h *host) discoveryInfo() obj {
	arch, rpcArch := "x86_64", "X86_64"
	if h.arch == "aarch64" {
		arch, rpcArch = "aarch64", "AARCH64"
	}
	nicPath := "/devices/pci0000:00/0000:00:00.0/0000:01:00.0/net/enp1s0np0"
	info := obj{
		"machine_type": arch,
		"machine_arch": rpcArch,
		"dmi_data": obj{
			"board_name": h.sm.product, "board_version": "", "bios_version": "", "bios_date": "",
			"product_serial": h.sm.serial, "board_serial": h.sm.serial, "chassis_serial": h.sm.serial,
			"product_name": h.sm.product, "sys_vendor": h.sm.manufacturer,
		},
		"tpm_ek_certificate": base64.StdEncoding.EncodeToString(h.ek),
		"network_interfaces": []any{obj{
			"mac_address": h.mac.String(),
			"pci_properties": obj{"vendor": h.vendor, "device": h.device, "path": nicPath, "numa_node": 0,
				"description": h.description, "slot": "0000:01:00.0"},
		}},
	}
	var gpus, cpus []any
	seed := h.a.rt.Engine.M.Seed
	for _, c := range h.dev.Components {
		switch c.Kind {
		case "accelerator":
			g := obj{"name": gpuName(h.dev, c), "serial": fmt.Sprintf("%s-%d", h.sm.serial, c.Index), "driver_version": "",
				"vbios_version": "", "inforom_version": "", "frequency": "",
				"pci_bus_id": fmt.Sprintf("%08x:06:00.0", 8+c.Index)}
			if c.MemoryMiB > 0 {
				g["total_memory"] = fmt.Sprintf("%d MiB", int64(c.MemoryMiB))
			}
			if r := h.dev.Rack; nvl.RackScale(r) && scaleUp(c) {
				g["platform_info"] = obj{"chassis_serial": nvl.ChassisSerial(seed, r), "slot_number": nvl.Slot(h.dev),
					"tray_index": nvl.TrayIndex(h.dev), "host_id": 1, "module_id": c.Index + 1,
					"fabric_guid": fmt.Sprintf("0x%016x", nvl.FabricGUID(seed, c))}
			}
			gpus = append(gpus, g)
		case "cpu":
			cpus = append(cpus, obj{"model": c.Model, "vendor": c.Vendor, "sockets": 1, "cores": 0, "threads": 0})
		}
	}
	if len(gpus) > 0 {
		info["gpus"] = gpus
	}
	if len(cpus) > 0 {
		info["cpu_info"] = cpus
	}
	return info
}

// scaleUp says whether a GPU has NVLink scale-up ports (it is in the rack's NVLink domain).
func scaleUp(c *model.Component) bool {
	for _, p := range c.Ports {
		if p.Kind == "scale-up" {
			return true
		}
	}
	return false
}

// gpuName is the name NVML gives a GPU: the platform's for a Grace superchip tray (NICo knows a
// multi-node NVLink GPU by it), else the component's model.
func gpuName(d *model.Device, c *model.Component) string {
	for _, s := range []string{d.ProfileName, c.Model} {
		u := strings.ToUpper(s)
		for _, k := range []string{"GB300", "GB200", "GH200"} {
			if strings.Contains(u, k) {
				return "NVIDIA " + k
			}
		}
	}
	return firstOf(c.Model, "NVIDIA GPU")
}

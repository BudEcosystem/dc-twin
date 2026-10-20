// Package mokka drives NVIDIA's Mokka (github.com/NVIDIA/k8s-test-infra: the nvml-mock driver
// tree and its mock libnvidia-ml.so) from the twin's model, so that real Kubernetes GPU software
// (the GPU Operator's device plugin, GPU Feature Discovery and dcgm-exporter, the DRA driver, Node
// Feature Discovery, nvidia-smi) runs on a real kubelet against the twin's GPUs rather than
// against Mokka's sample hardware.
//
// Mokka takes two inputs per node, and this package renders both from the model:
//
//   - the GPU profile (Render): the config.yaml the node agent stages and every loaded copy of the
//     mock library reads. The twin writes the identity of each GPU into it — the NVML name the
//     twin's packs use (D43), the count, identity/v1 UUIDs and serials, the PCI bus numbering the
//     twin's dcgm-exporter impersonation and fault catalogue use, memory, VBIOS, the NVLink
//     domain's cluster UUID and the tray's place in its NVL rack — over a base profile of Mokka's
//     own for the same product, which supplies what the model has no source for (clocks, power
//     envelope, ECC and NVLink shape, PCIe root complexes). Identity fields are baked into a
//     device when the library constructs it (docs/nvml-mock-ctl.md, "v1 scope"), so a profile is
//     rendered once, at install, as the model's structure is fixed while the estate runs.
//   - the runtime override document (Overrides): the overrides.yaml that nvml-mock-ctl writes
//     next to config.yaml and every loaded library re-reads on a one-second TTL, deep-merged over
//     the profile. This is Mokka's one mechanism for runtime change; it has no API or daemon to
//     send a command to (docs/architecture.md, "State changes at runtime"). The twin owns the
//     whole document for the hosts it drives and rewrites it from committed model state whenever
//     a host's accelerators change (Syncer): an accelerator fault whose catalogue entry carries a
//     `shim: {driver: nvidia/mokka, …}` effect becomes that device's `failure` block, so Xid 79
//     makes the GPU fall off the bus in NVML, with Xid 79 queued on the NVML event set, and
//     clearing the fault removes the block.
//
// Fidelity (D49): the hosts this drives are CONTAINER HOSTS — a Kind node, or any kubelet whose
// node runs Mokka — not the twin's `vm` guests. The kubelet, containerd and everything scheduled
// on them are real; the operating system is a container on whatever machine runs it, and the GPUs
// are the twin's model as Mokka's mock NVML presents it. The node's firmware, PCI bus, power and
// kernel are not the twin's, so a host the twin powers off keeps running here (IsNot).
// Mokka's node agent makes its driver tree's character devices with mknod, which the kernel
// refuses in every user namespace but the first, so the node's container engine must be rootful
// (compat/gpu-operator/guest.sh runs the cluster in a KVM guest where the host's is rootless).
//
// What Mokka cannot carry, and so this package does not render, is printed by IsNot and by each
// profile's Notes.
package mokka

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/adapter/nvl"
	"github.com/BudEcosystem/dc-twin/internal/identity"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// Mokka is the release whose config and override schemas this package renders: nvml-mock 0.5.0,
// k8s-test-infra tag v0.5.0 (e2950d58), chart oci://ghcr.io/nvidia/k8s-test-infra/chart/nvml-mock.
const (
	Mokka       = "v0.5.0"
	MokkaCommit = "e2950d58bdace638f3a522005978cc278ffa95d7"
	// ShimDriver is the fault catalogues' name for this shim: a `shim` effect with this driver is
	// the pack's statement of what a fault is to Mokka.
	ShimDriver = "nvidia/mokka"
)

// serialFormat is the identity/v1 serial format of an NVIDIA GPU: thirteen digits, as NVML
// reports a board serial. It is the dcgm exporter dialect's serialFormat
// (internal/adapter/metrics, Exporters["dcgm"]), so the twin derives one serial per GPU whichever
// surface reports it.
const serialFormat = "{digits:13}"

// Host is what the twin knows of one host's NVIDIA GPUs, read from the compiled model: everything
// a profile is rendered from. It is plain data, so a profile can be rendered from a compiled
// estate without a running engine (cmd/dctwin-mokka renders from the same estate file and seed
// the running twin compiled).
type Host struct {
	Path    string // the model path, a01/compute-tray-01
	Node    string // the Node it registers as: kwok.NodeName, "<rack>-<device>"
	Profile string // the twin's device profile
	Machine string // the system's product name (the profile's identity model)
	Arch    string // amd64 or arm64, model.HostArch
	UUID    string // the system's own UUID (SMBIOS, Redfish)
	GPUs    []GPU
	// Fabric is set when the GPUs belong to an NVLink domain with switch trays (an NVL rack):
	// what NVML's GpuFabricInfo reports for them. HGX baseboards and single servers have none.
	Fabric *Fabric
	// Platform is the tray's place in its NVL rack, which NVML's platform info reports; nil
	// outside a rack-scale system.
	Platform *Platform
}

// GPU is one accelerator as NVML reports it.
type GPU struct {
	Index     int    // NVML index: the component's index
	Path      string // a01/compute-tray-01/gpu-2
	Name      string // the NVML name: the component's model (D43)
	UUID      string // GPU-<identity/v1 UUID of the component>
	Serial    string // identity/v1 serial, thirteen digits
	BusID     string // the sysfs BDF, 0000:38:00.0
	MemoryMiB int64  // 0 when the profile does not say
	VBIOS     string // the device's "gpu" firmware version, when the profile has one
}

// NVMLBusID is the address in NVML's busId format (NVML_DEVICE_PCI_BUS_ID_FMT, eight domain
// digits): what dcgm-exporter labels pci_bus_id and nvidia-smi prints.
func (g GPU) NVMLBusID() string { return "0000" + g.BusID }

// Fabric is a GPU's NVLink fabric identity.
type Fabric struct {
	ClusterUUID string // the domain's UUID: nvl.DomainUUID, kwok's {domain} before ".1"
	CliqueID    int    // 1: the model has one NVLink partition per domain (kwok.DomainID)
}

// Platform is a compute tray's placement, as NMX-C and RMS report it (internal/adapter/nvl).
type Platform struct {
	ChassisSerial string // the NVL rack's chassis serial
	Slot          int    // the rack unit
	TrayIndex     int    // among the rack's compute trays, from 0
}

// BusID is the PCI address the twin gives an NVIDIA GPU: bus 0x18 + 0x10 per index, the
// numbering the dcgm-exporter impersonation labels pci_bus_id with and the fault catalogue's
// {pci} names in kernel-log lines (internal/adapter/metrics, renderDCGM).
func BusID(index int) string { return fmt.Sprintf("0000:%02X:00.0", 0x18+index*0x10) }

// HostOf reads a host's GPUs from the model. ok is false when the host has no NVIDIA GPU. Only
// fields the compiler writes once are read (structure, identity, the firmware baselines), so it
// needs no engine lock.
func HostOf(m *model.Model, d *model.Device) (Host, bool) {
	id := identity.New(m.Seed)
	h := Host{Path: d.Path, Node: NodeName(d), Profile: d.ProfileName, Arch: model.HostArch(d), UUID: d.Identity.UUID}
	if d.Profile != nil {
		h.Machine = d.Profile.Spec.Identity.Model
	}
	vbios := ""
	if fw := d.State.Firmware["gpu"]; fw != nil {
		vbios = fw.Version
	}
	paths := map[string]bool{}
	for _, c := range d.Components {
		if c.Kind != "accelerator" || c.Vendor != "nvidia" {
			continue
		}
		paths[c.Path] = true
		h.GPUs = append(h.GPUs, GPU{
			Index:     c.Index,
			Path:      c.Path,
			Name:      c.Model,
			UUID:      "GPU-" + id.UUID(c.Path, d.Identity.ProfileMajor, c.Index),
			Serial:    id.Serial(model.SerialSource(c).Path, d.Identity.ProfileMajor, serialFormat, model.SerialSource(c).Index),
			BusID:     BusID(c.Index),
			MemoryMiB: int64(c.MemoryMiB),
			VBIOS:     vbios,
		})
	}
	if len(h.GPUs) == 0 {
		return h, false
	}
	sort.Slice(h.GPUs, func(i, j int) bool { return h.GPUs[i].Index < h.GPUs[j].Index })
	// An NVL rack's domain is the one NVML reports a fabric for: the GPUs register with the
	// NVLink switch trays' fabric manager. A per-server domain (HGX) has no switch tray, and real
	// HGX H100 GPUs report a completed fabric with a zero cluster UUID, which GPU Feature
	// Discovery turns into no clique.
	for _, dom := range nvl.ScaleUpDomains(m) {
		for _, mem := range dom.Members {
			if paths[mem] {
				h.Fabric = &Fabric{ClusterUUID: nvl.DomainUUID(m.Seed, dom), CliqueID: 1}
				break
			}
		}
		if h.Fabric != nil {
			break
		}
	}
	if r := d.RackOf(); h.Fabric != nil && nvl.RackScale(r) && d.BaseClass == "compute-tray" {
		h.Platform = &Platform{ChassisSerial: nvl.ChassisSerial(m.Seed, r), Slot: int(nvl.Slot(d)), TrayIndex: int(nvl.TrayIndex(d))}
	}
	return h, true
}

// Hosts reads every host a selector-free walk of the model finds with NVIDIA GPUs, in path order.
func Hosts(m *model.Model) []Host {
	var out []Host
	for _, d := range m.Devices {
		if !d.IsHost() {
			continue
		}
		if h, ok := HostOf(m, d); ok {
			out = append(out, h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

var notDNS = regexp.MustCompile(`[^a-z0-9.-]+`)

// NodeName is the Node a host registers as. It must equal kwok.NodeName and metrics.NodeName
// (a test holds the three together): "<rack>-<device>" as a DNS-1123 name. A container host is
// given this name (kubeadm's nodeRegistration.name), so the real kubelet, the twin's kwok
// projection and dcgm-exporter's hostname label all name the host the same way.
func NodeName(d *model.Device) string {
	n := d.Name
	if r := d.RackOf(); r != nil {
		n = r.Name + "-" + d.Name
	}
	n = notDNS.ReplaceAllString(strings.ToLower(n), "-")
	n = strings.Trim(n, "-.")
	if len(n) > 253 {
		n = strings.Trim(n[:253], "-.")
	}
	return n
}

// IsNot is what a run on container hosts driven by this package does not show.
var IsNot = []string{
	"The host's operating system is a container (a Kind node) on whatever machine runs it: kernel, firmware, PCI bus, NUMA and power are that machine's, not the twin's. A host the twin powers off or AC-cycles keeps running.",
	"GPUs are the twin's model as Mokka's mock NVML presents them: no kernel runs, no memory is allocated, and every reading not in the twin's profile (clocks, power, temperature, utilisation, ECC shape, NVLink link state) is Mokka's.",
	"Runtime change reaches the node only as a Mokka failure block: an accelerator fault whose catalogue declares a nvidia/mokka shim effect. Port, power, thermal and NVLink-lane faults the model applies are not carried.",
	"InfiniBand is not rendered: Mokka derives HCA GUIDs from a prefix and cannot carry the twin's port GUIDs (the NIC sysfs tree is the in-host agent's, Q16).",
	"Xid lines on the kernel log are not written: on a Kind node the kernel ring buffer is the Docker host's, shared by every node and by whatever else that host runs.",
}

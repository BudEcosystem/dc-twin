// Package schema holds the v1alpha1 file formats of docs/spec/13-schemas.md: the estate, the
// pack objects (profiles, rack formats, fabric templates, fault catalogues, dialects), scenarios,
// stacks and the committed event. Decoding is strict: an unknown field is an error, because a
// misspelt field that is silently ignored is a test that silently does not run.
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	yaml3 "go.yaml.in/yaml/v3"
	"sigs.k8s.io/yaml"
)

const APIVersion = "dc-twin/v1alpha1"

type Metadata struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// Header is what every file starts with.
type Header struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
}

// ---------------------------------------------------------------- Estate

type Estate struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   Metadata   `json:"metadata"`
	Spec       EstateSpec `json:"spec"`
}

type EstateSpec struct {
	Seed         int64              `json:"seed"`
	Clock        string             `json:"clock,omitempty"`
	TimeScale    float64            `json:"timeScale,omitempty"`
	Packs        []string           `json:"packs"`
	Fidelity     FidelitySpec       `json:"fidelity,omitempty"`
	InitialState string             `json:"initialState,omitempty"`
	Sites        []Site             `json:"sites"`
	Cabling      *Cabling           `json:"cabling,omitempty"`
	Networks     map[string]Network `json:"networks"`
	Fabrics      []Fabric           `json:"fabrics"`
	// StorageSystems are the shared storage systems the storage racks' nodes and enclosures make.
	StorageSystems []StorageSystem `json:"storageSystems,omitempty"`
	Facility       Facility        `json:"facility,omitempty"`
	Stacks         []StackRef      `json:"stacks,omitempty"`
	Hardware       []HardwareSpec  `json:"hardware,omitempty"`
	Adapters       AdapterOptions  `json:"adapters,omitempty"`
}

// AdapterOptions are twin-side knobs the fidelity report always prints when set away from the
// default (an implementation addition, recorded in docs/spec/13-schemas.md).
type AdapterOptions struct {
	Redfish RedfishOptions `json:"redfish,omitempty"`
	Network NetworkOptions `json:"network,omitempty"`
	VM      VMOptions      `json:"vm,omitempty"`
}

// VMOptions size and boot the `vm` substrate's machines (QEMU). The fidelity report prints
// every one that is set: a scaled-down guest is not the profile's machine.
type VMOptions struct {
	// MemoryMB and CPUs replace every guest's size from its profile's fidelity.vm.cost.
	MemoryMB int `json:"memoryMB,omitempty"`
	CPUs     int `json:"cpus,omitempty"`
	// QEMU is a directory searched for qemu-system-<arch> before PATH.
	QEMU string `json:"qemu,omitempty"`
	// Guest is a directory holding the os: twin guest image; default: the pinned image under
	// the assets directory (`dctwin assets fetch`).
	Guest string `json:"guest,omitempty"`
	// TPM is `auto` (default: attach swtpm to hosts whose profile has a TPM, when swtpm is
	// installed) or `off`.
	TPM string `json:"tpm,omitempty"`
	// Drives is `nvme` (default: every drive of a vm host's profile is an NVMe drive in the guest,
	// on a sparse backing in the state directory) or `off` (the guest has no drives).
	Drives string `json:"drives,omitempty"`
}

type RedfishOptions struct {
	// Auth is `required` (default) or `optional`: optional accepts unauthenticated reads and
	// writes, which only an oracle run (a client test suite written against a mockup server)
	// should ever set.
	Auth string `json:"auth,omitempty"`
	// DPUs is `model` (default) or `capture`: capture keeps every DPU resource a captured tree
	// holds whatever DPUs the model's host has, for an oracle run whose suite counts the captured
	// machine's devices; the fidelity report prints it.
	DPUs string `json:"dpus,omitempty"`
	// Drives is `model` (default: a captured tree's drives are bound to the model's, in order, and
	// those it has beyond the model's are absent) or `capture` (the captured machine's drives as
	// captured, for an oracle run whose suite counts them); the fidelity report prints it.
	Drives string `json:"drives,omitempty"`
}

type NetworkOptions struct {
	// Substrate is `loopback` (default on hosts without TAPs: every endpoint on 127.0.0.1 with
	// its own port, plus the site gateway) or `tap` (Linux, D20).
	Substrate string `json:"substrate,omitempty"`
	// BasePort is the first loopback port endpoints are allocated from.
	BasePort int `json:"basePort,omitempty"`
	// NOS configures the real switch operating systems the tap substrate runs (a fabric whose
	// nos is not none).
	NOS NOSOptions `json:"nos,omitempty"`
	// Attach wires hosts outside the twin (a stack's DHCP server, its controller) into the twin
	// networks at a real switch's port, as a site cables its management servers.
	Attach []Attachment `json:"attach,omitempty"`
	// HBN gives every BlueField at model fidelity whose host is at vm an emulated data plane on
	// the tap substrate (D71): its HBN is the twin's NVUE over FRR in a container of its own,
	// its host's NIC is its host PF (pf0hpf_if), and its uplinks are routed, unnumbered leaf ports.
	HBN bool `json:"hbn,omitempty"`
}

// NOSOptions are the tap substrate's knobs for real NOS instances.
type NOSOptions struct {
	// Runtime is where real NOS instances run: `docker` (the default; the Docker Engine's unix
	// socket).
	Runtime string `json:"runtime,omitempty"`
	// Image replaces the registry's image for every switch (a locally built tag).
	Image string `json:"image,omitempty"`
	// Underlay is the IPv4 prefix fabric links' /31s are numbered from (default 10.255.0.0/16).
	Underlay string `json:"underlay,omitempty"`
	// Memory is every NOS container's memory limit, with no swap beyond it (bytes, or k, m, g:
	// "512m"); unset, containers are unlimited.
	Memory string `json:"memory,omitempty"`
	// HBNBinaries is a directory on the Docker host holding NICo's DPU programs built from its
	// source (forge-dpu-agent, forge-dhcp-server: compat/nico/hbn-build.sh), mounted read-only at
	// /opt/forge/bin in every emulated BlueField (D71); unset, none is.
	HBNBinaries string `json:"hbnBinaries,omitempty"`
	// Day0Binaries is a directory on the Docker host holding the twin's day-0 programs for the
	// switches (dctwin-day0: its ZTP client, nv, ONIE's discovery; `go build ./cmd/dctwin-day0`),
	// mounted read-only at /opt/dctwin/bin in every switch of a factory fabric (18); unset, the
	// directory of the twin's own executable is used when it holds one.
	Day0Binaries string `json:"day0Binaries,omitempty"`
	// Images are the NOS installers ONIE's discovery recognises (D75): a fetched file matching
	// one, by sha256 or by name, installs that NOS.
	Images []NOSImage `json:"images,omitempty"`
	// CumulusVersion is the Cumulus Linux release every generic/nvue-frr switch runs ("5.18.3"):
	// the NVUE grammar it speaks (5.15 changed it), the build GET /system reports and the
	// version its ZTP requests carry. Unset, 5.6.0 (the emulation's own, before 5.15).
	CumulusVersion string `json:"cumulusVersion,omitempty"`
}

// NOSImage is one NOS installer ONIE may fetch: what identifies it and the NOS it installs.
type NOSImage struct {
	Match NOSImageMatch `json:"match"`
	NOS   string        `json:"nos"`
	// Version is what the installed NOS reports as its version ("5.13.1"); optional.
	Version string `json:"version,omitempty"`
}

// NOSImageMatch identifies an installer by its content's sha256 (hex) or its file name (a
// regular expression over the last element of the URL ONIE fetched).
type NOSImageMatch struct {
	SHA256 string `json:"sha256,omitempty"`
	Name   string `json:"name,omitempty"`
}

// Attachment cables a running container to a real switch's port.
type Attachment struct {
	// Container is the container's name; the twin moves a veth into its network namespace.
	// Netns instead names a network namespace under /var/run/netns (a host process's).
	Container string `json:"container,omitempty"`
	Netns     string `json:"netns,omitempty"`
	// Port is the switch port it is cabled to, a port ref ("<switch path>:<port>").
	Port string `json:"port"`
	// Address is the host's address and prefix (10.90.0.10/24); Gateway, the switch's address
	// on that port, is also the host's default route.
	Address string `json:"address"`
	Gateway string `json:"gateway"`
	// Interface is the host-side interface name (default svc0).
	Interface string `json:"interface,omitempty"`
	// Routes are prefixes the host reaches through the port, in place of a default route (a
	// namespace whose default route is elsewhere: NICo's, routed to the twin's own router).
	Routes []string `json:"routes,omitempty"`
	// Serves are prefixes the host answers for besides its address (services on another of its
	// interfaces): the switch routes them to it and its BGP carries them across the fabric.
	Serves []string `json:"serves,omitempty"`
}

type FidelitySpec struct {
	Default   string             `json:"default,omitempty"`
	Overrides []FidelityOverride `json:"overrides,omitempty"`
}

type FidelityOverride struct {
	Select string `json:"select"`
	Level  string `json:"level"`
	OS     string `json:"os,omitempty"`
}

type Site struct {
	Name  string `json:"name"`
	Halls []Hall `json:"halls"`
}

type Hall struct {
	Name   string         `json:"name"`
	Layout map[string]any `json:"layout,omitempty"`
	Rows   []Row          `json:"rows"`
}

type Row struct {
	Name      string `json:"name"`
	Positions int    `json:"positions"`
	Racks     []Rack `json:"racks,omitempty"`
}

type Rack struct {
	Name     string                  `json:"name"`
	Position int                     `json:"position"`
	Format   string                  `json:"format"`
	Params   map[string]any          `json:"params,omitempty"`
	Feeds    []string                `json:"feeds,omitempty"`
	Slots    map[string]SlotOverride `json:"slots,omitempty"`
}

type SlotOverride struct {
	Profile string         `json:"profile,omitempty"`
	Params  map[string]any `json:"params,omitempty"`
	Empty   bool           `json:"empty,omitempty"`
}

type Cabling struct {
	Add    []CableSpec `json:"add,omitempty"`
	Remove []string    `json:"remove,omitempty"`
}

type CableSpec struct {
	A    string `json:"a"`
	B    string `json:"b"`
	Kind string `json:"kind,omitempty"`
}

type Network struct {
	Role   string `json:"role"`
	CIDR   string `json:"cidr,omitempty"`
	DHCP   string `json:"dhcp,omitempty"`
	Fabric string `json:"fabric,omitempty"`
	// Server is where relayed DHCP goes when dhcp: stack (host:port); normally a stack output.
	Server string `json:"server,omitempty"`
	// Overlay makes the network a second network on another network's ports, a segment the same
	// NICs and switches carry tagged (GB trays' storage VXLAN on their BlueField's frontend ports,
	// docs/reviews/2026-11-08-storage-deep-dive.md 5.5). Its role need not be its ports'.
	Overlay *NetworkOverlay `json:"overlay,omitempty"`
}

// NetworkOverlay is the network an overlay rides and how it is told apart there.
type NetworkOverlay struct {
	On    string `json:"on"`    // the network whose ports carry it
	Encap string `json:"encap"` // vlan or vxlan
	ID    int    `json:"id"`    // the VLAN id or the VNI
}

type Fabric struct {
	Name       string         `json:"name"`
	Template   string         `json:"template"`
	NOS        string         `json:"nos"`
	Technology string         `json:"technology,omitempty"`
	Manager    string         `json:"manager,omitempty"`
	Params     map[string]any `json:"params,omitempty"`
	Select     string         `json:"select,omitempty"`
	PlaceIn    string         `json:"placeIn,omitempty"`
	// BringUp is how the fabric's real switches start (D72): `twin` (the default: the twin
	// applies each switch's start-up configuration from the model, D55, D56) or `factory` (each
	// switch starts in its NOS's factory state and the software under test does day 0, 18).
	BringUp string `json:"bringUp,omitempty"`
	// Factory tunes a fabric whose bringUp is factory.
	Factory *FabricFactory `json:"factory,omitempty"`
}

// FabricFactory is how a factory fabric's switches leave the factory (18 — The estate).
type FabricFactory struct {
	// Install is `preinstalled` (the default: the NOS is on the switch) or `onie` (the switch has
	// no NOS until ONIE's discovery fetches an installer the twin recognises, D75).
	Install string `json:"install,omitempty"`
	// ZTP is `enabled` (the default: the NOS's ZTP runs at its first boot, D74) or `disabled`.
	ZTP string `json:"ztp,omitempty"`
	// Plan is `given` (the default: stack-inputs/day0/plan.json holds the twin's address plan, the
	// oracle compares values) or `tool` (the software under test allocates; the oracle compares
	// structure, D77).
	Plan string `json:"plan,omitempty"`
	// BootTime is how long a NOS takes from power-on to its management port's DHCP; InstallTime
	// how long ONIE's install takes (durations, "90s", "5m"; shortened under spec.timeScale).
	BootTime    string `json:"bootTime,omitempty"`
	InstallTime string `json:"installTime,omitempty"`
	// Select limits factory state to these switches (a selector); the others are configured by
	// the twin as under bringUp: twin.
	Select string `json:"select,omitempty"`
}

// StorageSystem is one shared storage system (docs/reviews/2026-11-08-storage-deep-dive.md 5.4,
// S-Q4): the storage nodes and drive enclosures of the racks it selects, made one system with
// protocols, a pool, a management API and, at vm, a data path.
type StorageSystem struct {
	Name     string `json:"name"`
	Template string `json:"template"`
	// Select names the racks whose storage nodes and enclosures are the system's.
	Select   string          `json:"select,omitempty"`
	Networks StorageNetworks `json:"networks,omitempty"`
	// Protocols are what the system serves, from its template's: nfs, s3, nvme-tcp.
	Protocols []string       `json:"protocols,omitempty"`
	Params    map[string]any `json:"params,omitempty"`
	// Manager is the management API it serves (default: its template's first).
	Manager string `json:"manager,omitempty"`
	// Fidelity is model (default: the management API, exporters and events from the model) or vm
	// (its nodes are vm hosts that serve the protocols with real Linux targets).
	Fidelity string `json:"fidelity,omitempty"`
}

// StorageNetworks bind a storage system to the estate's networks by name.
type StorageNetworks struct {
	// Data is the network clients reach its protocols on (role storage).
	Data string `json:"data,omitempty"`
	// Management is the network its management API is served on (role management).
	Management string `json:"management,omitempty"`
}

type Facility struct {
	Power   FacilityPower   `json:"power,omitempty"`
	Cooling FacilityCooling `json:"cooling,omitempty"`
	Devices []FacilityDev   `json:"devices,omitempty"`
	BMS     *DialectRef     `json:"bms,omitempty"`
	Grid    *DialectRef     `json:"grid,omitempty"`
}

type FacilityPower struct {
	Feeds    []Feed      `json:"feeds,omitempty"`
	Topology []PowerNode `json:"topology,omitempty"`
}

type Feed struct {
	Name      string  `json:"name"`
	CapacityW float64 `json:"capacityW"`
	Phases    int     `json:"phases,omitempty"`
	Voltage   float64 `json:"voltage,omitempty"`
}

type PowerNode struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	From      string   `json:"from"`
	CapacityW float64  `json:"capacityW"`
	Serves    []string `json:"serves"`
	// RuntimeS is a UPS's battery ride-through at its rated load, in seconds.
	RuntimeS float64 `json:"runtimeS,omitempty"`
	// Profile is the facility profile the node is made from (D generic/<kind>).
	Profile string `json:"profile,omitempty"`
}

type FacilityCooling struct {
	Loops []Loop `json:"loops,omitempty"`
}

type Loop struct {
	Name       string   `json:"name"`
	Profile    string   `json:"profile"`
	Racks      []string `json:"racks"`
	CapacityKW float64  `json:"capacityKW"`
	SupplyC    float64  `json:"supplyC,omitempty"`
}

type FacilityDev struct {
	Name     string `json:"name"`
	Profile  string `json:"profile"`
	At       string `json:"at"`
	Position int    `json:"position,omitempty"`
}

type DialectRef struct {
	Dialect string `json:"dialect"`
	// Broker is the MQTT broker the publisher connects to (host:port); normally a stack output.
	Broker string `json:"broker,omitempty"`
}

type StackRef struct {
	Name          string         `json:"name"`
	Components    []string       `json:"components,omitempty"`
	Options       map[string]any `json:"options,omitempty"`
	Impersonators []string       `json:"impersonators,omitempty"`
	// Outputs are what the installed stack hands back (broker address, DHCP server, accessor
	// endpoints) when the stack was installed outside the twin's service zone.
	Outputs map[string]string `json:"outputs,omitempty"`
}

type HardwareSpec struct {
	Select         string         `json:"select"`
	Bridge         map[string]any `json:"bridge"`
	Identity       any            `json:"identity,omitempty"`
	Controls       []any          `json:"controls,omitempty"`
	CredentialsRef string         `json:"credentialsRef,omitempty"`
}

// ---------------------------------------------------------------- Device profile

type DeviceProfile struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   Metadata    `json:"metadata"`
	Spec       ProfileSpec `json:"spec"`
}

type ProfileSpec struct {
	Class      string                    `json:"class"`
	Form       Form                      `json:"form"`
	Identity   *IdentitySpec             `json:"identity,omitempty"`
	Components []ComponentSpec           `json:"components,omitempty"`
	BMC        *BMCSpec                  `json:"bmc,omitempty"`
	Ports      []PortSpec                `json:"ports"`
	Power      PowerSpec                 `json:"power"`
	Cooling    CoolingSpec               `json:"cooling"`
	Sensors    []SensorSpec              `json:"sensors,omitempty"`
	Firmware   []FirmwareSpec            `json:"firmware,omitempty"`
	Behaviours map[string]map[string]any `json:"behaviours,omitempty"`
	Faults     []string                  `json:"faults"`
	Quirks     []string                  `json:"quirks,omitempty"`
	Kubernetes *KubernetesSpec           `json:"kubernetes,omitempty"`
	BMS        *BMSMapping               `json:"bms,omitempty"`
	SNMP       *SNMPMapping              `json:"snmp,omitempty"`
	Metrics    *MetricsSpec              `json:"metrics,omitempty"`
	Fidelity   map[string]map[string]any `json:"fidelity"`
	Layout     map[string]any            `json:"layout,omitempty"`
	Provides   []Provide                 `json:"provides"`
	IsNot      IsNot                     `json:"isNot"`
	Sources    []Source                  `json:"sources"`
}

type Form struct {
	Mounting string `json:"mounting"`
	Units    int    `json:"units,omitempty"`
}

type IdentitySpec struct {
	SerialFormat  string `json:"serialFormat"`
	MacOUI        string `json:"macOUI,omitempty"`
	GUIDPrefix    string `json:"guidPrefix,omitempty"`
	UUIDNamespace string `json:"uuidNamespace,omitempty"`
	Manufacturer  string `json:"manufacturer,omitempty"`
	Model         string `json:"model,omitempty"`
	PartNumber    string `json:"partNumber,omitempty"`
	// Platform is a switch's ONIE platform string (`<arch>-<vendor>_<machine>-r<revision>`,
	// onie-sysinfo -p), which ONIE's discovery and a NOS's ZTP report (18).
	Platform string `json:"platform,omitempty"`
}

type ComponentSpec struct {
	Name    string     `json:"name"`
	Kind    string     `json:"kind"`
	Count   int        `json:"count"`
	Model   string     `json:"model,omitempty"`
	Vendor  string     `json:"vendor,omitempty"`
	Profile string     `json:"profile,omitempty"`
	Ports   []PortSpec `json:"ports,omitempty"`
	MaxW    float64    `json:"maxW,omitempty"`
	Pad     int        `json:"pad,omitempty"`
	// MemoryGiB is an accelerator's own memory, for what exporters and GPU feature discovery
	// report; when absent it is read from a "<n>GB" in the model string.
	MemoryGiB float64 `json:"memoryGiB,omitempty"`
	// MemoryMiB is the exact figure NVML reports (a few percent under the marketed size), and
	// wins over MemoryGiB.
	MemoryMiB float64 `json:"memoryMiB,omitempty"`
	// SerialShare is how many consecutive accelerators share one board serial (2 on a GB200/GB300
	// tray, whose superchips carry two GPUs each); 0 or 1: each has its own.
	SerialShare int `json:"serialShare,omitempty"`
	// A drive's facts (kind drive). CapacityBytes wins over a size in Model ("NVMe 3.84 TB");
	// Media is ssd or hdd; Protocol nvme, sata or sas (default: nvme when Model says NVMe, else
	// sata); FormFactor m.2, u.2, e1.s, e3.s, 2.5 or 3.5; Role boot, data or cache (default: the
	// first drive boot, the rest data); Firmware the drive's firmware revision; PartNumber; and
	// SerialFormat its serial number's pattern (identity.Serial).
	CapacityBytes int64  `json:"capacityBytes,omitempty"`
	Media         string `json:"media,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
	FormFactor    string `json:"formFactor,omitempty"`
	Role          string `json:"role,omitempty"`
	Firmware      string `json:"firmware,omitempty"`
	PartNumber    string `json:"partNumber,omitempty"`
	SerialFormat  string `json:"serialFormat,omitempty"`
	// Controller names the storage-controller component (its name) a drive sits behind; a drive
	// without one is attached to the host directly (NVMe on PCIe).
	Controller string `json:"controller,omitempty"`
	// RAIDTypes are a storage controller's supported RAID types (kind storage-controller), as
	// Redfish names them (RAID0, RAID1, RAID5, RAID6, RAID10, RAID50, RAID60).
	RAIDTypes []string `json:"raidTypes,omitempty"`
}

type BMCSpec struct {
	Dialect         string `json:"dialect"`
	Mockup          string `json:"mockup,omitempty"`
	IPMI            bool   `json:"ipmi,omitempty"`
	Console         string `json:"console,omitempty"`
	DHCPVendorClass string `json:"dhcpVendorClass,omitempty"`
}

type PortSpec struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Speed      string `json:"speed,omitempty"`
	Role       string `json:"role,omitempty"`
	Count      int    `json:"count,omitempty"`
	Lanes      int    `json:"lanes,omitempty"`
	Owner      string `json:"owner,omitempty"`
	Technology string `json:"technology,omitempty"`
	Source     string `json:"source,omitempty"`
}

type PowerSpec struct {
	StandbyW float64 `json:"standbyW"`
	IdleW    float64 `json:"idleW"`
	MaxW     float64 `json:"maxW"`
	Model    string  `json:"model,omitempty"`
	// OutputW is what a power source (shelf, PDU, UPS) can deliver.
	OutputW float64 `json:"outputW,omitempty"`
}

type CoolingSpec struct {
	Kind string `json:"kind"`
	Loop string `json:"loop,omitempty"`
}

type SensorSpec struct {
	Name     string   `json:"name"`
	Quantity string   `json:"quantity"`
	Unit     string   `json:"unit,omitempty"`
	Warn     *float64 `json:"warn,omitempty"`
	Crit     *float64 `json:"crit,omitempty"`
	Source   string   `json:"source"`
}

type FirmwareSpec struct {
	Component  string `json:"component"`
	Baseline   string `json:"baseline"`
	Updateable *bool  `json:"updateable,omitempty"`
	ApplyOn    string `json:"applyOn,omitempty"`
}

type KubernetesSpec struct {
	Resources map[string]int    `json:"resources,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	DRA       map[string]any    `json:"dra,omitempty"`
}

type BMSMapping struct {
	ObjectType string     `json:"objectType"`
	Points     []BMSPoint `json:"points"`
}

type BMSPoint struct {
	Name      string `json:"name"`
	PointType string `json:"pointType"`
	Sensor    string `json:"sensor"`
}

// SNMPMapping binds a profile to a MIB dialect (a PDU's outlets and phases).
type SNMPMapping struct {
	MIB     string `json:"mib"`
	Outlets int    `json:"outlets,omitempty"`
}

// MetricsSpec says which exporter dialect a host's in-host metrics follow (dcgm, amd, gaudi).
type MetricsSpec struct {
	Exporter string `json:"exporter"`
}

type Provide struct {
	Capability string   `json:"capability"`
	At         []string `json:"at"`
	Via        string   `json:"via,omitempty"`
}

// IsNot is either a plain list (meaning `all`) or a map by level.
type IsNot struct {
	All      []string `json:"all,omitempty"`
	Model    []string `json:"model,omitempty"`
	VM       []string `json:"vm,omitempty"`
	Hardware []string `json:"hardware,omitempty"`
	set      bool
}

func (n *IsNot) UnmarshalJSON(b []byte) error {
	n.set = true
	t := bytes.TrimSpace(b)
	if len(t) > 0 && t[0] == '[' {
		return json.Unmarshal(b, &n.All)
	}
	type plain struct {
		All      []string `json:"all,omitempty"`
		Model    []string `json:"model,omitempty"`
		VM       []string `json:"vm,omitempty"`
		Hardware []string `json:"hardware,omitempty"`
	}
	var p plain
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	n.All, n.Model, n.VM, n.Hardware = p.All, p.Model, p.VM, p.Hardware
	return nil
}

// Declared reports whether the file said anything at all; isNot is required, even if empty.
func (n IsNot) Declared() bool { return n.set }

// At returns what the subject does not model at a fidelity level.
func (n IsNot) At(level string) []string {
	out := append([]string{}, n.All...)
	switch level {
	case "model":
		out = append(out, n.Model...)
	case "vm":
		out = append(out, n.VM...)
	case "hardware":
		out = append(out, n.Hardware...)
	}
	return out
}

type Source struct {
	What string `json:"what"`
	URL  string `json:"url"`
	Date string `json:"date"`
}

// ---------------------------------------------------------------- Rack format

type RackFormat struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   Metadata   `json:"metadata"`
	Spec       FormatSpec `json:"spec"`
}

type FormatSpec struct {
	Archetype   string                    `json:"archetype"`
	Params      map[string]FormatParam    `json:"params,omitempty"`
	Slots       []SlotSpec                `json:"slots"`
	Layout      FormatLayout              `json:"layout"`
	FixedLinks  []FixedLink               `json:"fixedLinks,omitempty"`
	Domains     []DomainSpec              `json:"domains,omitempty"`
	Power       FormatPower               `json:"power"`
	Cooling     map[string]any            `json:"cooling"`
	Propagation map[string][]Propagation  `json:"propagation,omitempty"`
	Management  map[string]string         `json:"management,omitempty"`
	Behaviours  map[string]map[string]any `json:"behaviours,omitempty"`
	Provides    []Provide                 `json:"provides,omitempty"`
	IsNot       IsNot                     `json:"isNot"`
	Sources     []Source                  `json:"sources"`
}

type FormatParam struct {
	Type     string   `json:"type"`
	Default  any      `json:"default,omitempty"`
	Range    []int    `json:"range,omitempty"`
	Allowed  []any    `json:"allowed,omitempty"`
	Class    string   `json:"class,omitempty"`
	Provides []string `json:"provides,omitempty"`
	Label    string   `json:"label,omitempty"`
}

type SlotSpec struct {
	Names   string         `json:"names"`
	Profile string         `json:"profile"`
	Accepts map[string]any `json:"accepts,omitempty"`
	U       int            `json:"u,omitempty"`
	Side    string         `json:"side,omitempty"`
	At      []string       `json:"at,omitempty"`
	// Feed binds the slot's devices to a power input side (A or B) by brace pattern position.
	Inputs []string `json:"inputs,omitempty"`
	// When is a CEL condition over the format's params; the slot is skipped when false.
	When string `json:"when,omitempty"`
}

type FormatLayout struct {
	Height     int            `json:"height"`
	UNumbering string         `json:"uNumbering,omitempty"`
	Front      []LayoutGroup  `json:"front"`
	Back       []string       `json:"back,omitempty"`
	Reserved   []ReservedU    `json:"reserved,omitempty"`
	Extra      map[string]any `json:"extra,omitempty"`
}

type LayoutGroup struct {
	Slots string `json:"slots"`
	Group string `json:"group,omitempty"`
}

type ReservedU struct {
	U     int    `json:"u"`
	Units int    `json:"units"`
	For   string `json:"for"`
}

type FixedLink struct {
	Template string `json:"template,omitempty"`
	A        string `json:"a,omitempty"`
	B        string `json:"b,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

type DomainSpec struct {
	Kind       string `json:"kind"`
	Technology string `json:"technology,omitempty"`
	Members    string `json:"members"`
	Manager    string `json:"manager,omitempty"`
	Formation  string `json:"formation,omitempty"`
	Spans      string `json:"spans,omitempty"`
	Unit       string `json:"unit,omitempty"`
}

type FormatPower struct {
	Inputs    []PowerInput `json:"inputs"`
	CapacityW float64      `json:"capacityW"`
}

type PowerInput struct {
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	CapacityW  float64 `json:"capacityW"`
	Redundancy string  `json:"redundancy,omitempty"`
	MinSources int     `json:"minSources,omitempty"`
	// Sources names the slots whose devices feed this input (power shelves, PDUs).
	Sources string `json:"sources,omitempty"`
	// Feeds says which estate feed each source draws from, by source index modulo len.
	Feeds []string `json:"feeds,omitempty"`
	// Serves names the slots fed by this input; default every slot that is not a source.
	Serves string `json:"serves,omitempty"`
}

type Propagation struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Effect string `json:"effect"`
}

// ---------------------------------------------------------------- Fabric template

type FabricTemplate struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   Metadata     `json:"metadata"`
	Spec       TemplateSpec `json:"spec"`
}

type TemplateSpec struct {
	// Generator names the core generator that implements the template (clos, rail, oob, spine).
	Generator string                 `json:"generator"`
	Params    map[string]FormatParam `json:"params,omitempty"`
	Switch    map[string]string      `json:"switch,omitempty"`
	Provides  map[string][]string    `json:"provides,omitempty"`
	Layout    map[string]any         `json:"layout,omitempty"`
	IsNot     IsNot                  `json:"isNot"`
	Sources   []Source               `json:"sources,omitempty"`
}

// ---------------------------------------------------------------- Storage template

// StorageTemplate is a kind of shared storage system: which devices of the selected racks make
// it, what it can serve and which management APIs it has (the storage analogue of a fabric
// template).
type StorageTemplate struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Metadata   Metadata            `json:"metadata"`
	Spec       StorageTemplateSpec `json:"spec"`
}

type StorageTemplateSpec struct {
	// Nodes and Enclosures are the device classes of the selected racks that are the system's
	// storage nodes (they serve) and drive enclosures (drives behind the nodes).
	Nodes      string `json:"nodes"`
	Enclosures string `json:"enclosures,omitempty"`
	// Media are the drive media its pool is made of (default ssd); boot drives never are.
	Media []string `json:"media,omitempty"`
	// Protocols are what it can serve; Managers the management APIs it can serve, the first by
	// default.
	Protocols []string               `json:"protocols"`
	Managers  []string               `json:"managers"`
	Params    map[string]FormatParam `json:"params,omitempty"`
	// Faults are the catalogues a system of this template accepts (a strict allow-list, as a
	// profile's faults are).
	Faults   []string            `json:"faults,omitempty"`
	Provides map[string][]string `json:"provides,omitempty"`
	IsNot    IsNot               `json:"isNot"`
	Sources  []Source            `json:"sources,omitempty"`
}

// ---------------------------------------------------------------- Fault catalogue

type FaultCatalogue struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Metadata   Metadata      `json:"metadata"`
	Spec       CatalogueSpec `json:"spec"`
}

type CatalogueSpec struct {
	Faults []FaultSpec `json:"faults"`
	IsNot  IsNot       `json:"isNot"`
}

type FaultSpec struct {
	Name    string                 `json:"name"`
	Title   string                 `json:"title"`
	Target  FaultTarget            `json:"target"`
	Params  map[string]FormatParam `json:"params,omitempty"`
	Effects []map[string]any       `json:"effects"`
	Clears  FaultClears            `json:"clears"`
	IsNot   IsNot                  `json:"isNot"`
}

type FaultTarget struct {
	Component  string `json:"component,omitempty"`
	Device     string `json:"device,omitempty"`
	Port       bool   `json:"port,omitempty"`
	DeviceWith string `json:"deviceWith,omitempty"`
	Vendor     string `json:"vendor,omitempty"`
	// StorageSystem targets the storage systems the selector names (storage/<name>).
	StorageSystem bool `json:"storageSystem,omitempty"`
}

type FaultClears struct {
	On     []string `json:"on,omitempty"`
	After  string   `json:"after,omitempty"`
	Manual bool     `json:"manual,omitempty"`
	// OnYAML11 catches `on:` written unquoted, which YAML 1.1 reads as the boolean true.
	OnYAML11 []string `json:"true,omitempty"`
}

// Triggers returns the clearing triggers however the key was spelt.
func (c FaultClears) Triggers() []string { return append(append([]string{}, c.On...), c.OnYAML11...) }

// ---------------------------------------------------------------- Dialects

// RedfishDialect binds a captured mockup (or the generated DMTF tree) to the model.
type RedfishDialect struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Metadata   Metadata    `json:"metadata"`
	Spec       DialectSpec `json:"spec"`
}

type DialectSpec struct {
	// Backend is `mockup` (a captured tree bound by overlays) or `generated` (a DMTF tree the
	// engine builds from the model).
	Backend string `json:"backend"`
	// Mockup names the asset the captured tree comes from (pack.yaml assets).
	Mockup string `json:"mockup,omitempty"`
	// Inherits takes another dialect's binding and applies this one's overlays on top.
	Inherits    string         `json:"inherits,omitempty"`
	ServiceRoot map[string]any `json:"serviceRoot,omitempty"`
	System      string         `json:"system,omitempty"`
	Manager     string         `json:"manager,omitempty"`
	Chassis     string         `json:"chassis,omitempty"`
	BMCPort     string         `json:"bmcInterface,omitempty"`
	// Bind names where the captured tree holds the device's primary identity, as
	// "<resource path>#<property>": serial, uuid, bmcMAC. Those values become the model's.
	Bind map[string]string `json:"bind,omitempty"`
	// Replace lists literal strings of the captured service (its address, its hostname) that are
	// replaced everywhere they occur.
	Replace map[string]string `json:"replace,omitempty"`
	// Accounts seeds the model's BMC accounts from the captured AccountService when true.
	SeedAccounts bool `json:"seedAccounts,omitempty"`
	// DynamicCollections are collection paths whose members the engine renders from the model.
	DynamicCollections map[string]string `json:"dynamicCollections,omitempty"`
	Factory            FactoryLogin      `json:"factory"`
	Scrub              []ScrubRule       `json:"scrub,omitempty"`
	Overlays           []Overlay         `json:"overlays,omitempty"`
	Components         []ComponentBind   `json:"components,omitempty"`
	// DPUs are the resources the capture holds for the host's DPUs, as patterns: {n} (1-based) or
	// {i} (0-based) is the DPU's index, a pattern without one is the first DPU. Each matches the
	// resource and everything under it. They are bound to the model's DPU sub-devices in order:
	// every SerialNumber there becomes that DPU's own (a stack pairs a host with its DPUs by
	// serial), and a DPU the model does not have is absent, from its collections too.
	DPUs []string `json:"dpus,omitempty"`
	// DPUHostMACs are the MACs the capture holds for the host-side PFs of the host's DPUs (in
	// any spelling), in the order of DPUs: wherever the host's tree names one (its NICs, its
	// boot options), the engine names that DPU's host PF instead, the BaseMAC the DPU's own BMC
	// serves, so the host's boot option for its DPU and the DPU agree (a stack sets a host to
	// boot from its DPU by that MAC).
	DPUHostMACs []string `json:"dpuHostMACs,omitempty"`
	// DPUBootName is how the BMC names a vm host's firmware boot option for a NIC that is a DPU's
	// host PF, where it names options by adapter (a Supermicro: "UEFI HTTP IPv4 Mellanox Network
	// Adapter - <MAC>(MAC:<mac>)", which NICo's libredfish looks for to boot a host from its DPU).
	// {proto} is HTTP or PXE, {ip} IPv4 or IPv6, {mac} the MAC with colons, {compact} without.
	// Empty: the firmware's own description.
	DPUBootName string `json:"dpuBootName,omitempty"`
	// FirmwareFromProfile keeps the profile's firmware baselines where the capture would seed
	// them: set by a dialect that presents another product's capture (the GB300 BMC over the
	// GB200 capture), whose captured levels are that other product's. Without it a dialect serves
	// its capture's levels, and the profiles that use it state those levels (D35).
	FirmwareFromProfile bool `json:"firmwareFromProfile,omitempty"`
	// SettingsJobs is the Location of the job a PATCH of a settings object schedules when the
	// capture has no answer for that request, with {n} for a twelve-digit job number: iDRAC
	// answers every Bios/Settings PATCH 202 with a JID_ job. Empty: 204, as most BMCs answer.
	SettingsJobs string `json:"settingsJobs,omitempty"`
	// StorageControllers are storage controllers the dialect serves from the model rather than
	// from its capture: component group -> the Storage resource's id under the system (a Dell
	// BOSS: boss -> BOSS.SL.14-1). Their drives and volumes are the model's, with the dialect's
	// job semantics (settingsJobs) for volume and decommission requests.
	StorageControllers map[string]string `json:"storageControllers,omitempty"`
	Sensors            []SensorBind      `json:"sensors,omitempty"`
	Leak               []string          `json:"leakDetectors,omitempty"`
	Actions            []ActionBind      `json:"actions,omitempty"`
	Firmware           map[string]string `json:"firmware,omitempty"`
	Quirks             []QuirkSpec       `json:"quirks,omitempty"`
	Identity           DialectIdentity   `json:"identity,omitempty"`
	Requests           []RequestMap      `json:"requests,omitempty"`
	IsNot              IsNot             `json:"isNot"`
	Sources            []Source          `json:"sources,omitempty"`
}

type FactoryLogin struct {
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
}

type DialectIdentity struct {
	DHCPVendorClass   string `json:"dhcpVendorClass,omitempty"`
	IPMIManufacturer  int    `json:"ipmiManufacturer,omitempty"`
	LLDPSystemName    string `json:"lldpSystemName,omitempty"`
	CertificateIssuer string `json:"certificateIssuer,omitempty"`
}

type ScrubRule struct {
	Property string `json:"property"`
	Kind     string `json:"kind"`
}

// Overlay sets a property at a path (glob over resource paths) from a literal or a model field.
type Overlay struct {
	Path  string `json:"path"`
	Set   string `json:"set"`
	Value any    `json:"value,omitempty"`
	From  string `json:"from,omitempty"`
}

type ComponentBind struct {
	Component string   `json:"component"`
	Resources []string `json:"resources"`
}

type SensorBind struct {
	Sensor    string   `json:"sensor"`
	Resources []string `json:"resources"`
	Property  string   `json:"property,omitempty"`
}

type ActionBind struct {
	Path      string            `json:"path"`
	Method    string            `json:"method,omitempty"`
	Canonical string            `json:"canonical,omitempty"`
	ByField   string            `json:"byField,omitempty"`
	Map       map[string]string `json:"map,omitempty"`
	Behaviour string            `json:"behaviour,omitempty"`
	Status    int               `json:"status,omitempty"`
}

type QuirkSpec struct {
	Name   string         `json:"name"`
	Title  string         `json:"title,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

type RequestMap struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Canonical string `json:"canonical"`
	Op        string `json:"op,omitempty"`
}

// TopicDialect is a BMS or grid topic scheme over MQTT.
type TopicDialect struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Metadata   Metadata  `json:"metadata"`
	Spec       TopicSpec `json:"spec"`
}

type TopicSpec struct {
	Value      string            `json:"value"`
	Metadata   string            `json:"metadata"`
	Requests   []TopicRequest    `json:"requests,omitempty"`
	Payload    map[string]string `json:"payload,omitempty"`
	Heartbeat  string            `json:"heartbeat,omitempty"`
	IsNot      IsNot             `json:"isNot"`
	Sources    []Source          `json:"sources,omitempty"`
	PointTypes []string          `json:"pointTypes,omitempty"`
}

type TopicRequest struct {
	PointType string `json:"pointType"`
	Effect    string `json:"effect"`
	Status    string `json:"status,omitempty"`
	DelayS    int    `json:"delaySeconds,omitempty"`
}

// MibDialect is a vendor's SNMP MIB for a PDU or UPS.
type MibDialect struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       MibSpec  `json:"spec"`
}

type MibSpec struct {
	SysObjectID string            `json:"sysObjectID"`
	SysDescr    string            `json:"sysDescr"`
	Objects     map[string]string `json:"objects"`
	IsNot       IsNot             `json:"isNot"`
	Sources     []Source          `json:"sources,omitempty"`
}

// ---------------------------------------------------------------- Pack manifest

type PackManifest struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       PackSpec `json:"spec"`
}

type PackSpec struct {
	Title     string   `json:"title,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
	Assets    []Asset  `json:"assets,omitempty"`
	IsNot     IsNot    `json:"isNot"`
}

type Asset struct {
	Name    string `json:"name"`
	Path    string `json:"path,omitempty"`
	Licence string `json:"licence"`
	Obtain  *struct {
		Source string `json:"source"`
		Ref    string `json:"ref"`
		Path   string `json:"path"`
		// Files, when set, are the only files under Path that are obtained, each fetched on its
		// own instead of the repository's tarball (a single interface definition, not a tree).
		Files []string `json:"files,omitempty"`
	} `json:"obtain,omitempty"`
}

// ---------------------------------------------------------------- Scenario

type Scenario struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   Metadata     `json:"metadata"`
	Spec       ScenarioSpec `json:"spec"`
}

type ScenarioSpec struct {
	Estate  ScenarioEstate `json:"estate"`
	Stack   ScenarioStack  `json:"stack"`
	Timeout string         `json:"timeout"`
	Chaos   *Chaos         `json:"chaos,omitempty"`
	Setup   []Step         `json:"setup,omitempty"`
	Steps   []Step         `json:"steps"`
	Report  string         `json:"report,omitempty"`
}

type ScenarioEstate struct {
	Requires struct {
		Formats      []string       `json:"formats,omitempty"`
		Capabilities []string       `json:"capabilities,omitempty"`
		Racks        map[string]int `json:"racks,omitempty"`
		Clock        string         `json:"clock,omitempty"`
	} `json:"requires"`
}

type ScenarioStack struct {
	Components []string          `json:"components,omitempty"`
	Accessors  map[string]string `json:"accessors,omitempty"`
	// Aliases are standard aliases (17 — Standard aliases) the scenario reads, bound to whichever
	// installed stack maps them in its critic section; the scenario is not-run when none does.
	Aliases []string `json:"aliases,omitempty"`
}

type Chaos struct {
	Enabled           bool     `json:"enabled"`
	RatePerDeviceHour float64  `json:"ratePerDeviceHour"`
	Faults            []string `json:"faults,omitempty"`
}

type Step struct {
	Name   string      `json:"name,omitempty"`
	Wait   *WaitStep   `json:"wait,omitempty"`
	Act    *ActStep    `json:"act,omitempty"`
	Fault  *FaultStep  `json:"fault,omitempty"`
	Expect *ExpectStep `json:"expect,omitempty"`
}

type WaitStep struct {
	For    string `json:"for"`
	Within string `json:"within"`
}

type ActStep struct {
	Power   string  `json:"power,omitempty"`
	Select  string  `json:"select,omitempty"`
	Cable   string  `json:"cable,omitempty"`
	Link    string  `json:"link,omitempty"`
	Reseat  bool    `json:"reseat,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	Clock   string  `json:"clock,omitempty"`
	// Feed and Up take a facility feed down or up.
	Feed string `json:"feed,omitempty"`
	Up   *bool  `json:"up,omitempty"`
	// Utilisation sets the load of the selected hosts.
	Utilisation *float64 `json:"utilisation,omitempty"`
	// Drive is a technician's act on the drives Select names: pull, insert, replace or erase.
	Drive string `json:"drive,omitempty"`
	// Storage is an act on the storage system Select names (storage/<name>): its objects' use
	// (`fill`), or an object taken offline or back (`offline`, `online`).
	Storage *StorageAct `json:"storage,omitempty"`
}

// StorageAct changes what a storage system holds without its management API.
type StorageAct struct {
	// Fill sets the used bytes of the named object (a file system, volume or bucket), as clients
	// writing to it would; UsedPercent sets it as a share of the object's quota or capacity.
	Fill        string   `json:"fill,omitempty"`
	UsedBytes   *int64   `json:"usedBytes,omitempty"`
	UsedPercent *float64 `json:"usedPercent,omitempty"`
	// Offline and Online take the named object offline or bring it back.
	Offline string `json:"offline,omitempty"`
	Online  string `json:"online,omitempty"`
}

type FaultStep struct {
	ID     string         `json:"id,omitempty"`
	Select string         `json:"select,omitempty"`
	Inject string         `json:"inject,omitempty"`
	Params map[string]any `json:"params,omitempty"`
	Clear  any            `json:"clear,omitempty"`
}

type ExpectStep struct {
	Within string        `json:"within"`
	All    []Observation `json:"all"`
}

type Observation struct {
	Not      *Observation   `json:"not,omitempty"`
	Redfish  *RedfishObs    `json:"redfish,omitempty"`
	IPMI     map[string]any `json:"ipmi,omitempty"`
	BMS      *BMSObs        `json:"bms,omitempty"`
	Event    *EventObs      `json:"event,omitempty"`
	Domain   *DomainObs     `json:"domain,omitempty"`
	Faults   *FaultsObs     `json:"faults,omitempty"`
	Accessor *AccessorObs   `json:"accessor,omitempty"`
	Model    *ModelObs      `json:"model,omitempty"`
	SNMP     *SNMPObs       `json:"snmp,omitempty"`
	UFM      *UFMObs        `json:"ufm,omitempty"`
	Kube     *KubeObs       `json:"kubernetes,omitempty"`
	DHCP     *DHCPObs       `json:"dhcp,omitempty"`
}

type RedfishObs struct {
	Select   string `json:"select"`
	Property string `json:"property"`
	Equals   any    `json:"equals"`
}

type BMSObs struct {
	Topic string `json:"topic"`
	Value any    `json:"value"`
}

type EventObs struct {
	Canonical string `json:"canonical,omitempty"`
	// Class matches a request's class (read, write, destructive; 14), so a scenario can say "no
	// destructive request" without naming a stack's calls.
	Class string `json:"class,omitempty"`
	Kind  string `json:"kind,omitempty"`
	From  string `json:"from,omitempty"`
	On    string `json:"on,omitempty"`
	Field string `json:"field,omitempty"`
	To    any    `json:"to,omitempty"`
}

type DomainObs struct {
	Path  string `json:"path"`
	State string `json:"state"`
}

type FaultsObs struct {
	Select string `json:"select"`
	Count  int    `json:"count"`
}

type AccessorObs struct {
	Alias  string         `json:"alias"`
	Args   map[string]any `json:"args,omitempty"`
	Equals any            `json:"equals"`
}

// ModelObs asserts a CEL expression over the model is true.
type ModelObs struct {
	Expr string `json:"expr"`
}

type SNMPObs struct {
	Select string `json:"select"`
	OID    string `json:"oid"`
	Equals any    `json:"equals"`
}

type UFMObs struct {
	Port   string `json:"port"`
	Field  string `json:"field"`
	Equals any    `json:"equals"`
}

type KubeObs struct {
	Node      string `json:"node"`
	Condition string `json:"condition,omitempty"`
	Status    string `json:"status,omitempty"`
	Taint     string `json:"taint,omitempty"`
}

type DHCPObs struct {
	Select string `json:"select"`
	Leased bool   `json:"leased"`
}

// ---------------------------------------------------------------- Stack

type Stack struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Metadata   Metadata  `json:"metadata"`
	Spec       StackSpec `json:"spec"`
}

type StackSpec struct {
	Title         string                    `json:"title,omitempty"`
	Options       map[string]map[string]any `json:"options,omitempty"`
	Impersonators []Impersonator            `json:"impersonators,omitempty"`
	Components    map[string]StackComponent `json:"components"`
	Scenarios     []string                  `json:"scenarios"`
	Critic        *StackCritic              `json:"critic,omitempty"`
	IsNot         IsNot                     `json:"isNot"`
}

type Impersonator struct {
	Name       string         `json:"name"`
	Replaces   string         `json:"replaces"`
	Obtain     map[string]any `json:"obtain,omitempty"`
	AppliesTo  string         `json:"appliesTo"`
	Projection string         `json:"projection,omitempty"`
}

type StackComponent struct {
	Obtain        map[string]any            `json:"obtain"`
	ServiceZone   map[string]any            `json:"serviceZone,omitempty"`
	Clusters      []map[string]any          `json:"clusters,omitempty"`
	Install       []map[string]any          `json:"install"`
	Attach        []string                  `json:"attach"`
	Requires      []Requirement             `json:"requires"`
	Inputs        map[string]string         `json:"inputs,omitempty"`
	Outputs       map[string]map[string]any `json:"outputs,omitempty"`
	Accessors     map[string]Accessor       `json:"accessors,omitempty"`
	FabricProfile map[string]any            `json:"fabricProfile,omitempty"`
}

type Requirement struct {
	Capability string         `json:"capability"`
	Scope      map[string]any `json:"scope,omitempty"`
	AnyOf      []string       `json:"anyOf,omitempty"`
	Min        int            `json:"min,omitempty"`
	Preferred  string         `json:"preferred,omitempty"`
	When       string         `json:"when,omitempty"`
	Gate       string         `json:"gate,omitempty"`
}

type Accessor struct {
	Kind       string            `json:"kind"`
	Endpoint   string            `json:"endpoint"`
	Credential string            `json:"credential,omitempty"`
	Join       map[string]string `json:"join,omitempty"`
}

// ---------------------------------------------------------------- Event

type Event struct {
	Seq      uint64         `json:"seq"`
	T        string         `json:"t"`
	Wall     string         `json:"wall"`
	Kind     string         `json:"kind"`
	Source   EventSource    `json:"source"`
	Path     string         `json:"path"`
	Request  *EventRequest  `json:"request,omitempty"`
	State    *EventState    `json:"state,omitempty"`
	Cause    uint64         `json:"cause,omitempty"`
	Severity string         `json:"severity,omitempty"`
	Message  string         `json:"message,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
}

type EventSource struct {
	Class     string `json:"class"`
	Principal string `json:"principal,omitempty"`
	Address   string `json:"address,omitempty"`
}

type EventRequest struct {
	Protocol  string `json:"protocol"`
	Method    string `json:"method,omitempty"`
	Target    string `json:"target,omitempty"`
	Canonical string `json:"canonical"`
	// Class is the canonical request's class (read, write, destructive, other; 17).
	Class  string `json:"class,omitempty"`
	Status int    `json:"status,omitempty"`
	// Detail is what a request carried that its target does not say: an NVUE change's switch
	// spelling as sent and as held ("spelling": "enable->state", 18 — Z7a, Q30); a refused
	// change's reason ("issue")
	Detail map[string]string `json:"detail,omitempty"`
}

type EventState struct {
	Field string `json:"field"`
	From  any    `json:"from,omitempty"`
	To    any    `json:"to,omitempty"`
}

// ---------------------------------------------------------------- decoding

// Decode reads one YAML or JSON document strictly into v. YAML is read with 1.2 semantics
// (go.yaml.in/yaml/v3), so `on:` and `yes:` are keys, not booleans, as the spec writes them;
// the result is then decoded as JSON with unknown fields refused.
func Decode(data []byte, v any) error {
	var doc any
	if err := yaml3.Unmarshal(data, &doc); err != nil {
		return err
	}
	j, err := json.Marshal(normaliseYAML(doc))
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("error unmarshaling JSON: %w", err)
	}
	return nil
}

// DecodeAny reads one YAML document into plain JSON-shaped values (string-keyed maps, slices
// and scalars), with the same 1.2 semantics as Decode: the estate as a tree, for clients.
func DecodeAny(data []byte, v *any) error {
	var doc any
	if err := yaml3.Unmarshal(data, &doc); err != nil {
		return err
	}
	*v = normaliseYAML(doc)
	return nil
}

// normaliseYAML turns yaml.v3's maps into JSON-encodable ones.
func normaliseYAML(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = normaliseYAML(e)
		}
		return x
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[fmt.Sprint(k)] = normaliseYAML(e)
		}
		return m
	case []any:
		for i, e := range x {
			x[i] = normaliseYAML(e)
		}
		return x
	}
	return v
}

// KindOf reads only the header of a document.
func KindOf(data []byte) (Header, error) {
	var h Header
	if err := yaml.Unmarshal(data, &h); err != nil {
		return h, err
	}
	return h, nil
}

// SplitDocuments splits a multi-document YAML stream.
func SplitDocuments(data []byte) [][]byte {
	var out [][]byte
	for _, part := range bytes.Split(data, []byte("\n---")) {
		p := bytes.TrimSpace(part)
		p = bytes.TrimPrefix(p, []byte("---"))
		if len(bytes.TrimSpace(p)) == 0 {
			continue
		}
		out = append(out, p)
	}
	return out
}

// CheckHeader validates apiVersion and kind.
func CheckHeader(h Header, kind string) error {
	if h.APIVersion != APIVersion {
		return fmt.Errorf("apiVersion %q: want %q", h.APIVersion, APIVersion)
	}
	if h.Kind != kind {
		return fmt.Errorf("kind %q: want %q", h.Kind, kind)
	}
	if strings.TrimSpace(h.Metadata.Name) == "" {
		return fmt.Errorf("%s: metadata.name is required", kind)
	}
	return nil
}

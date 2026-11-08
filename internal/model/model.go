// Package model holds the runtime model the compiler builds from an estate file and its packs
// (docs/spec/03-estate-model.md — The runtime model). The engine is its only writer; adapters
// read it under the engine's read lock.
package model

import (
	"sort"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Power states of the power behaviour.
const (
	Off         = "Off"
	PoweringOn  = "PoweringOn"
	On          = "On"
	PoweringOff = "PoweringOff"
)

// Health values, worst last.
const (
	OK       = "OK"
	Warning  = "Warning"
	Critical = "Critical"
)

func HealthRank(h string) int {
	switch h {
	case Warning:
		return 1
	case Critical:
		return 2
	}
	return 0
}

func WorstHealth(hs ...string) string {
	w := OK
	for _, h := range hs {
		if HealthRank(h) > HealthRank(w) {
			w = h
		}
	}
	return w
}

type Model struct {
	Name     string
	Seed     int64
	Site     string
	Estate   *schema.Estate
	Clock    string
	Halls    []*Hall
	Racks    []*Rack
	RackBy   map[string]*Rack
	Devices  []*Device
	DeviceBy map[string]*Device
	Ports    map[string]*Port
	Links    []*Link
	LinkBy   map[string]*Link
	Domains  []*Domain
	Networks map[string]*Network
	Fabrics  map[string]*Fabric
	Feeds    map[string]*Feed
	// Power is the power chain between the feeds and the racks (facility.power.topology), each
	// node after the node it is fed from; PowerBy finds a node by its device's path.
	Power   []*PowerNode
	PowerBy map[string]*PowerNode
	Loops   map[string]*Loop
	// StorageSystems are the estate's shared storage systems, in estate order; StorageBy finds one
	// by its path (<site>/storage/<name>).
	StorageSystems []*StorageSystem
	StorageBy      map[string]*StorageSystem
	// Warnings are what validation reported without refusing.
	Warnings []string
}

type Hall struct {
	Name string
	Path string
	Rows []*Row
}

type Row struct {
	Name      string
	Index     int
	Positions int
	Racks     []*Rack
}

type Rack struct {
	Name       string
	Path       string
	Hall       *Hall
	Row        *Row
	Position   int
	FormatName string
	Format     *schema.RackFormat
	Params     map[string]any
	Devices    []*Device
	Inputs     []*PowerInput
	Domains    []*Domain
	Feeds      []string
	Loop       *Loop
	Isolated   map[string]bool // electrical, liquid: set by a BMS isolation request
}

// PowerInput is one of a rack format's power inputs: its sources (shelves, PDUs) and what it serves.
type PowerInput struct {
	Name       string
	Kind       string
	CapacityW  float64
	Redundancy string
	MinSources int
	Sources    []*Device
	Serves     []*Device
	// SourceFeed is the estate feed each source draws from.
	SourceFeed map[string]string
	// SourceNode is the power-chain node each source draws through, when its feed reaches the
	// rack through one (a panel, a busway or a UPS); a source not in it is fed by the feed itself.
	SourceNode map[string]*PowerNode
}

type Device struct {
	Path           string
	Name           string
	Rack           *Rack
	Hall           *Hall
	Slot           string
	SlotIndex      int
	U              int
	Units          int
	Side           string
	ProfileName    string
	ProfileVersion string
	Profile        *schema.DeviceProfile
	Class          string
	BaseClass      string
	Fidelity       string
	OS             string
	Identity       Identity
	Components     []*Component
	Ports          []*Port
	PortBy         map[string]*Port
	Parent         *Device
	Children       []*Device
	Generated      string // fabric name for template-generated devices
	Dialect        string
	Input          *PowerInput // the rack input that supplies it, if any
	Outlets        map[int]*Device
	State          DeviceState
	Endpoint       Endpoint
}

type Identity struct {
	ProfileMajor string
	Serial       string
	UUID         string
	BMCMAC       string
	MACs         map[string]string
	GUIDs        map[string]string
	BMCUser      string
	BMCPassword  string
}

// Endpoint is where the device's management interfaces are served on this host.
type Endpoint struct {
	SiteIP    string // the address the stack leased to the BMC (dhcp: stack) or the twin assigned
	Listen    string // host:port on this machine
	ListenTLS string
	IPMI      string
	SNMP      string
	SSH       string
}

type Component struct {
	Path   string
	Name   string // "<name>-<index>"
	Group  string
	Kind   string
	Model  string
	Vendor string
	Index  int
	Device *Device
	Ports  []*Port
	MaxW   float64
	// MemoryMiB is an accelerator's own memory (0 when the profile does not say).
	MemoryMiB float64
	// SerialShare is how many consecutive components of the group share one serial.
	SerialShare int
	Sub         *Device
	// Drive is a drive's facts (kind drive); Controller a storage controller's (kind
	// storage-controller). Nil for every other kind.
	Drive      *DriveInfo
	Controller *ControllerInfo
	State      ComponentState
}

// DriveInfo is what a drive is: its size, media, protocol, form, role and identity.
type DriveInfo struct {
	CapacityBytes int64
	Media         string // ssd, hdd
	Protocol      string // nvme, sata, sas
	FormFactor    string
	Role          string // boot, data, cache
	Firmware      string
	PartNumber    string
	Manufacturer  string
	SerialFormat  string
	Serial        string
	// EUI64 is the drive's first namespace's IEEE extended unique identifier (16 hex digits),
	// the WWID a host's /dev/disk/by-id and a root-device hint's wwn match.
	EUI64 string
	// Controller is the storage-controller component the drive sits behind, or nil.
	Controller *Component
	// Bay is the drive's slot number among its device's drives (0-based).
	Bay int
}

// ControllerInfo is a storage controller: the RAID types it supports and its drives.
type ControllerInfo struct {
	RAIDTypes []string
	Drives    []*Component
}

type ComponentState struct {
	Counters map[string]float64
	Replaced int
	// Contents is what a drive holds: empty, installed (a bootable OS), data, erased, or erasing
	// while a secure erase runs (owned at model; at vm the guest's writes are not observed).
	Contents string
	// Pulled is a drive a technician took out of its bay.
	Pulled bool
	// Serial and EUI64 are a replacement drive's own identity (empty: the first drive's, in
	// DriveInfo): a new drive in the bay is a different drive.
	Serial, EUI64 string
}

// Volume is a storage controller's logical volume (a RAID set over drives), owned state.
type Volume struct {
	ID            string
	Name          string
	RAIDType      string
	Controller    string   // the controller component's name
	Drives        []string // its drives' component names
	CapacityBytes int64
	// Pending is "create" or "delete" while the change waits for the host's next reset
	// (OperationApplyTime OnReset); "" once applied.
	Pending string
	// RebuildUntil is when a rebuild onto a replaced member completes (zero: none running).
	RebuildUntil time.Time
	Created      time.Time
}

type Port struct {
	Ref        string
	Name       string
	Device     *Device
	Component  *Component
	Kind       string
	Role       string
	Speed      string
	Technology string
	MAC        string
	GUID       string
	Link       *Link
	Network    string
	AdminUp    bool
}

type Link struct {
	ID     string
	A, B   *Port
	Kind   string
	Fabric string
	Up     bool // false when pulled
}

// Peer returns the far end of the link from p.
func (l *Link) Peer(p *Port) *Port {
	if l.A == p {
		return l.B
	}
	return l.A
}

type Domain struct {
	Path       string
	Kind       string
	Technology string
	Members    []string
	Manager    string
	Formation  string
	Racks      []*Rack
	State      string
}

type Network struct {
	Name   string
	Role   string
	CIDR   string
	DHCP   string
	Fabric string
	Server string
	Ports  []*Port
	// Overlay is the network whose ports carry this one (Encap vlan or vxlan, OverlayID the VLAN id
	// or VNI); its Ports are that network's. Empty for a network with ports of its own role.
	Overlay   string
	Encap     string
	OverlayID int
}

type Fabric struct {
	Name       string
	Template   string
	NOS        string
	Technology string
	Manager    string
	Params     map[string]any
	Racks      []*Rack
	Devices    []*Device
	Links      []*Link
	Provides   []string
}

type Feed struct {
	Name      string
	Path      string
	CapacityW float64
	Up        bool
}

// PowerNode is a node of the power chain between a feed and the racks: a UPS, a busway or a
// power panel. Its state is owned by the engine.
type PowerNode struct {
	Name      string
	Kind      string // ups, busway or pdu-panel
	Device    *Device
	CapacityW float64
	RuntimeS  float64    // a UPS's battery ride-through at its rated load
	Parent    *PowerNode // the node it is fed from; nil when it is fed by Feed itself
	Feed      *Feed      // the feed at the head of its chain
	Children  []*PowerNode
	Racks     []*Rack   // the racks it serves
	Sources   []*Device // the rack power sources (PDUs, shelves) that draw through it
	Up        bool      // delivering
	Mode      string    // a UPS: online, battery, depleted or fault; a busway or panel: closed or open
	Charge    float64   // a UPS's battery, 0..1, as of ChargeAt
	ChargeAt  time.Time
	DrainPerS float64 // the charge lost per behaviour second (negative while recharging)
	Timer     uint64  // the pending battery-depleted timer
}

type Loop struct {
	Name       string
	Profile    string
	Racks      []*Rack
	CapacityKW float64
	SupplyC    float64
	Flow       bool
}

// DeviceState is the device's owned state (D19). At `model` fidelity every field is owned.
type DeviceState struct {
	Power       string
	PowerSince  time.Time
	PowerTarget string // the state a running transition ends in
	AC          bool   // input power present (BMC on standby when Off)
	BMCUp       bool
	BMCSince    time.Time
	// BMCClock is the BMC's clock less the twin's: zero until a stack sets the clock, as an RTC
	// that runs with the twin and keeps what it was set to across BMC resets.
	BMCClock    time.Duration
	Booted      string // boot-walk step reached, or "os" when booted
	BootedSince time.Time
	Boot        BootState
	Bios        map[string]any
	BiosPending map[string]any
	SecureBoot  bool
	Lockdown    bool
	Accounts    []Account
	Sessions    map[string]Session
	Firmware    map[string]*FirmwareState
	Log         []LogEntry
	Tasks       []Task
	Subscribers []Subscription
	// Patches is the PATCH store of a protocol projection, kept in the model so it is owned
	// state and survives an adapter restart (resource path -> merged properties).
	Patches     map[string]map[string]any
	Lease       *Lease
	Outlets     map[int]bool // a PDU's outlet states
	Utilisation float64
	Counters    map[string]float64
	// Volumes are the device's storage controllers' logical volumes.
	Volumes   []Volume
	VolumeSeq int
	// Emulation is a DPU's NVMe emulation (P5): the NVMe functions it presents to its host.
	Emulation *NVMeEmulation
}

type BootState struct {
	OverrideTarget  string
	OverrideEnabled string
	OverrideMode    string
	HTTPBootURI     string
	Order           []string
}

type Account struct {
	ID       string
	User     string
	Password string
	Role     string
	Enabled  bool
	Locked   bool
	// PasswordChangeRequired is set on factory accounts of dialects that force a first change.
	PasswordChangeRequired bool
}

type Session struct {
	ID      string
	Token   string
	User    string
	Created time.Time
}

type FirmwareState struct {
	Component string
	Version   string
	Staged    string
	ApplyOn   string
	Baseline  string
}

type LogEntry struct {
	ID        int
	Created   time.Time
	Severity  string
	Message   string
	MessageID string
	Service   string // SEL, BMC
}

type Task struct {
	ID        string
	State     string
	Started   time.Time
	Ends      time.Time
	Message   string
	Canonical string
	Percent   int
}

type Subscription struct {
	ID          string
	Destination string
	Protocol    string
	Context     string
	SSE         bool
}

type Lease struct {
	IP          string
	Server      string
	Circuit     string
	Remote      string
	At          time.Time
	Renew       time.Duration
	Gateway     string
	VendorClass string
}

// ---- helpers

// Attr returns a device's values for a selector filter key.
func (d *Device) Attr(key string) []string {
	switch key {
	case "class":
		return []string{d.Class, d.BaseClass}
	case "profile":
		return []string{d.ProfileName}
	case "fidelity":
		return []string{d.Fidelity}
	case "os":
		return []string{d.OS}
	case "dialect":
		return []string{d.Dialect}
	case "rack":
		if d.Rack != nil {
			return []string{d.Rack.Name}
		}
	case "vendor":
		var out []string
		for _, c := range d.Components {
			out = append(out, c.Vendor)
		}
		return out
	}
	return nil
}

// HasBMC reports whether the device has a management controller.
func (d *Device) HasBMC() bool { return d.Profile != nil && d.Profile.Spec.BMC != nil }

// IsHost reports whether the device is a host a lifecycle manager ingests.
func (d *Device) IsHost() bool {
	return d.BaseClass == "server" || d.BaseClass == "compute-tray"
}

// Drives are the device's drive components, in bay order.
func (d *Device) Drives() []*Component {
	var out []*Component
	for _, c := range d.Components {
		if c.Kind == "drive" {
			out = append(out, c)
		}
	}
	return out
}

// Controllers are the device's storage-controller components.
func (d *Device) Controllers() []*Component {
	var out []*Component
	for _, c := range d.Components {
		if c.Kind == "storage-controller" {
			out = append(out, c)
		}
	}
	return out
}

// BootDrive is the drive a host boots from: the first drive whose role is boot, else the first
// drive; nil for a host without drives.
func (d *Device) BootDrive() *Component {
	ds := d.Drives()
	for _, c := range ds {
		if c.Drive != nil && c.Drive.Role == "boot" {
			return c
		}
	}
	if len(ds) > 0 {
		return ds[0]
	}
	return nil
}

// ComponentBy finds a component by its name ("gpu-2") or path suffix.
func (d *Device) ComponentBy(name string) *Component {
	for _, c := range d.Components {
		if c.Name == name || strings.HasSuffix(c.Path, "/"+name) {
			return c
		}
	}
	return nil
}

// Sorted returns paths sorted.
func Sorted(m map[string]*Device) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RackOf returns the rack a device sits in, following parents for sub-devices.
func (d *Device) RackOf() *Rack {
	for x := d; x != nil; x = x.Parent {
		if x.Rack != nil {
			return x.Rack
		}
	}
	return nil
}

// StorageSystem is a shared storage system (docs/reviews/2026-11-08-storage-deep-dive.md 5.4): the
// storage nodes and enclosures of its racks made one system. What it is made of is compiled; what
// it holds (State) is owned by the engine; its health, the node serving each object and its alerts
// are derived (engine/storagesys.go).
type StorageSystem struct {
	Name     string
	Path     string // <site>/storage/<name>
	Template string
	Manager  string
	Fidelity string // model or vm
	// Protocols it serves (nfs, s3, nvme-tcp).
	Protocols []string
	Params    map[string]any
	// DataNetwork and ManagementNetwork are the estate networks its protocols and its
	// management API are on.
	DataNetwork       string
	ManagementNetwork string
	Nodes             []*Device
	Enclosures        []*Device
	// Drives are the drives its pool is made of: the nodes' and enclosures' data drives of the
	// template's media.
	Drives []*Component
	// DataUnits and ParityUnits are its protection (data+parity across drives).
	DataUnits, ParityUnits int
	RawBytes               int64
	UsableBytes            int64
	// FullPercent is the pool use at which it raises its pool-full alert.
	FullPercent int
	// Faults are the catalogues it accepts.
	Faults   []string
	Provides []string
	IsNot    []string
	// Endpoints are where this machine serves it (api, metrics, and at vm nfs, s3, nvme-tcp),
	// host:port; Addresses its site addresses (management, data). Both are set by the adapter.
	Endpoints map[string]string
	Addresses map[string]string
	State     StorageState
}

// StorageState is what a storage system holds: owned state, changed by its management API, acts
// and, at vm, observed from its nodes (used bytes).
type StorageState struct {
	FileSystems []*FileSystem
	Volumes     []*BlockVolume
	Buckets     []*Bucket
	Snapshots   []*Snapshot
	// Hosts are the NVMe hosts (NQNs) known to it: registered, or mapped to a volume.
	Hosts []string
	// Seq numbers the objects it creates.
	Seq int
}

// FileSystem is a file system the system exports (NFS); QuotaBytes 0 is unlimited.
type FileSystem struct {
	ID, Name string
	// Path is where it is in the system's namespace, and so its export path ("/<name>" unless a
	// management API made it at a path of its own, as a VAST view is).
	Path       string
	Home       string // the path of the node it lives on
	QuotaBytes int64
	// Quota is whether a quota object exists on it (a VAST quota with no hard limit is one).
	Quota     bool
	UsedBytes int64
	Exports   []Export
	Offline   bool
	Created   time.Time
}

// Export is one export rule of a file system: the clients it admits (an address, a prefix or
// "*"), read-write or read-only, and whether root is squashed.
type Export struct {
	Clients    string
	Access     string // rw or ro
	RootSquash bool
}

// BlockVolume is a volume the system serves as an NVMe namespace over NVMe/TCP; Hosts are the
// host NQNs it is mapped to (none: unmapped).
type BlockVolume struct {
	ID, Name      string
	Home          string
	CapacityBytes int64
	UsedBytes     int64
	NQN           string // the subsystem NQN it is served under
	NGUID         string
	Hosts         []string
	Offline       bool
	Created       time.Time
}

// Bucket is an S3 bucket; QuotaBytes 0 is unlimited.
type Bucket struct {
	Name       string
	Home       string
	QuotaBytes int64
	UsedBytes  int64
	Offline    bool
	Created    time.Time
}

// Snapshot is a point-in-time copy of a file system or volume (Of is its id).
type Snapshot struct {
	ID, Name  string
	Of        string
	UsedBytes int64
	Created   time.Time
}

// NVMeEmulation is a DPU's NVMe emulation (BlueField's NVME_EMULATION_ENABLE, DOCA SNAP;
// docs/reviews/2026-11-08-storage-deep-dive.md 5.3): whether it is on, and the NVMe functions it
// presents to its host, each a storage system's volume the DPU reaches as an NVMe-oF host.
type NVMeEmulation struct {
	Enabled   bool
	Functions []EmulatedFunction
	Seq       int
}

// EmulatedFunction is one NVMe function a DPU presents to its host.
type EmulatedFunction struct {
	ID      int
	System  string // the storage system's path
	Volume  string // the volume's id
	HostNQN string // the NQN the DPU connects to the volume's target as
	Serial  string // the emulated controller's serial number, as the host reads it
	Created time.Time
}

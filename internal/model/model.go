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
	State       ComponentState
}

type ComponentState struct {
	Counters map[string]float64
	Replaced int
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

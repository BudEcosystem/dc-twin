// Package day0 is what the twin hands a day-0 tool and how it judges the tool's result (18,
// D77): the design of a factory fabric, as a site hands it (the expected switches, the cabling
// plan, the address plan, the same design as NetBox's bulk-import CSVs), and the oracle that
// compares what each switch reports about itself with that design.
//
// The design is the plan the twin would have applied itself under bringUp: twin (internal/nos's
// Switch, built by the TAP substrate's plan), so with plan: given the comparison is exact.
package day0

// Design is a factory estate's day-0 design: every real switch (factory or not, since a day-0
// tool configures the fabric around the ones the twin configured) and every cable between them.
type Design struct {
	Estate   string   `json:"estate"`
	Site     string   `json:"site,omitempty"`
	Plan     string   `json:"plan"` // given or tool
	Switches []Switch `json:"switches"`
	Cables   []Cable  `json:"cables"`
	// Mgmt is the management network the switches' eth0 lease on.
	Mgmt []MgmtNet `json:"managementNetworks,omitempty"`
}

// Switch is one switch of the design.
type Switch struct {
	Path     string `json:"path"`
	Hostname string `json:"hostname"` // the design's hostname, <rack>-<name>
	// Role is oob, leaf, spine or super-spine.
	Role    string `json:"role"`
	Fabric  string `json:"fabric"`
	Rack    string `json:"rack,omitempty"`
	U       int    `json:"u,omitempty"`
	Profile string `json:"profile"`
	// Identity, as the switch reports it (ONIE's EEPROM, ZTP's headers).
	Manufacturer string `json:"manufacturer,omitempty"`
	Model        string `json:"model,omitempty"`
	PartNumber   string `json:"partNumber,omitempty"`
	Platform     string `json:"platform,omitempty"`
	Serial       string `json:"serial"`
	BaseMAC      string `json:"baseMAC"`
	MgmtMAC      string `json:"mgmtMAC,omitempty"`
	NOS          string `json:"nos"`
	// Factory: the switch starts in factory state and the tool brings it up; otherwise the twin
	// configured it (bringUp: twin, or outside factory.select).
	Factory bool `json:"factory"`

	// The address plan (plan: given).
	ASN      uint32 `json:"asn,omitempty"`
	RouterID string `json:"routerId,omitempty"`
	Loopback string `json:"loopback,omitempty"` // a /32
	// Interfaces are its cabled front-panel ports as its NOS names them, with the design's use.
	Interfaces []Interface `json:"interfaces,omitempty"`
	Vlans      []Vlan      `json:"vlans,omitempty"`
	// EVPN: its fabric sessions carry L2VPN EVPN.
	EVPN bool `json:"evpn,omitempty"`
}

// Interface is a switch port of the design.
type Interface struct {
	Name     string `json:"name"`              // as the NOS names it (swp1, Ethernet0)
	Model    string `json:"model"`             // as the model names it (eth1)
	Speed    string `json:"speed,omitempty"`   // 400G
	Kind     string `json:"kind"`              // fabric, unnumbered, access, service, management
	Address  string `json:"address,omitempty"` // a fabric /31 with its prefix
	Peer     string `json:"peer,omitempty"`    // the far end, <hostname>:<its port as its NOS names it>
	PeerAddr string `json:"peerAddress,omitempty"`
	PeerASN  uint32 `json:"peerAsn,omitempty"`
	Vlan     string `json:"vlan,omitempty"` // an access port's VLAN, Vlan<id>
}

// Vlan is an access segment of the design: its gateway and the servers its relay forwards to.
type Vlan struct {
	Name    string   `json:"name"` // Vlan<id>
	ID      int      `json:"id"`
	Network string   `json:"network"`
	Gateway string   `json:"gateway"` // the gateway with the segment's prefix length
	Relay   []string `json:"relay,omitempty"`
	Ports   []string `json:"ports,omitempty"` // as the NOS names them
}

// Cable is one cable of the design: both ends' devices and ports.
type Cable struct {
	ID      string `json:"id"`
	Fabric  string `json:"fabric"`
	ADevice string `json:"aDevice"` // the device's hostname (switches) or path
	APort   string `json:"aPort"`   // the model's port name
	BDevice string `json:"bDevice"`
	BPort   string `json:"bPort"`
	Speed   string `json:"speed,omitempty"`
	// AIf, BIf are the ports as the switches' NOSes name them, where an end is a real switch.
	AIf string `json:"aIf,omitempty"`
	BIf string `json:"bIf,omitempty"`
}

// MgmtNet is a management network segment the switches' management ports lease on.
type MgmtNet struct {
	Network string `json:"network"`
	Rack    string `json:"rack"`
	Prefix  string `json:"prefix"`
	Gateway string `json:"gateway"`
}

// Observed is what the twin read from one switch, from the switch itself (the NOS observer).
type Observed struct {
	Hostname string `json:"hostname"`
	// Sessions are its BGP sessions by peer (an address, or an interface for an unnumbered one),
	// in every VRF: FRR's state name (Established, Active, Idle, …) and whether EVPN is up on it.
	Sessions map[string]Session `json:"sessions"`
	// Routes are the prefixes in its default VRF's RIB.
	Routes []string `json:"routes,omitempty"`
	// Mgmt is its management port's lease, in VRF mgmt ("" when it holds none).
	Mgmt string `json:"mgmt,omitempty"`
	// Vlans are its VLAN interfaces with their addresses (Vlan1000 -> 10.30.0.1/24).
	Vlans map[string]string `json:"vlans,omitempty"`
	// LLDP are its LLDP neighbours by local port: "<system name>:<port id>".
	LLDP map[string]string `json:"lldp,omitempty"`
	// Phase is the day-0 phase its ZTP and ONIE report (Phase*); "" when not known.
	Phase string `json:"phase,omitempty"`
	// Credentials is "factory" while the NOS's factory password still logs in.
	Credentials string `json:"credentials,omitempty"`
}

// Session is one observed BGP session.
type Session struct {
	State string `json:"state"`
	EVPN  bool   `json:"evpn,omitempty"`
	VRF   string `json:"vrf,omitempty"`
}

// The day-0 phases a switch moves through (18 — Converged).
const (
	PhaseFactory    = "factory"
	PhaseONIE       = "onie-installing"
	PhaseBooted     = "nos-booted"
	PhaseZTPRunning = "ztp-running"
	PhaseZTPDone    = "ztp-done"
	PhaseZTPFailed  = "ztp-failed"
	PhaseConfigured = "configured"
	PhaseConverged  = "converged"
)

// Verdict is the oracle's view of one switch: its phase and what differs from the design.
type Verdict struct {
	Path      string   `json:"path"`
	Phase     string   `json:"phase"`
	Converged bool     `json:"converged"`
	Diffs     []string `json:"diffs,omitempty"`
}

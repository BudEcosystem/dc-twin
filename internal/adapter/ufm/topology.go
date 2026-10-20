package ufm

import (
	"sort"
	"strconv"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

// ibPort is one InfiniBand port of the fabric as the model cables it. Everything here is
// structural (fixed at compile); operational state is asked of the engine per response.
type ibPort struct {
	p       *model.Port
	peer    *model.Port
	dev     *model.Device
	sys     *system
	sw      bool
	guid    string
	number  int
	dname   string
	name    string
	adminUp bool
}

// system is a device with ports on the fabric: a UFM "system".
type system struct {
	dev      *model.Device
	guid     string // system GUID: the GUID of the device's first InfiniBand port
	hostname string
	sw       bool
	sm       bool // the UFM appliance: the subnet manager's host
	ports    []*ibPort
	siteIP   string
	fw       string // the current version of the device's first firmware component
}

type snap struct {
	ports   []*ibPort
	byGUID  map[string]*ibPort
	byPort  map[*model.Port]*ibPort
	systems []*system
	sysBy   map[string]*system // by system GUID and by hostname
}

// hostname is the name a device reports on the fabric (its node description): rack and slot,
// which is unique in the estate.
func hostname(d *model.Device) string {
	if r := d.RackOf(); r != nil && d.Parent == nil {
		return r.Name + "-" + d.Name
	}
	if d.Parent != nil {
		return hostname(d.Parent) + "-" + d.Name
	}
	return d.Name
}

func isSwitch(d *model.Device) bool { return d.BaseClass == "switch" }

// snapshot reads the fabric's topology from the model under the engine's read lock.
func (s *server) snapshot() *snap {
	sn := &snap{byGUID: map[string]*ibPort{}, byPort: map[*model.Port]*ibPort{}, sysBy: map[string]*system{}}
	sysOf := map[*model.Device]*system{}
	s.a.rt.Engine.Read(func(m *model.Model) {
		add := func(p, peer *model.Port) {
			if _, ok := sn.byPort[p]; ok {
				return
			}
			d := p.Device
			sy := sysOf[d]
			if sy == nil {
				sy = &system{dev: d, hostname: hostname(d), sw: isSwitch(d), sm: d == s.dev, siteIP: d.Endpoint.SiteIP, fw: "N/A"}
				if f := d.Profile.Spec.Firmware; len(f) > 0 {
					if st := d.State.Firmware[f[0].Component]; st != nil {
						sy.fw = st.Version
					}
				}
				for _, x := range d.Ports {
					if x.Kind == "infiniband" && x.GUID != "" {
						sy.guid = normGUID(x.GUID)
						break
					}
				}
				sysOf[d] = sy
			}
			ip := &ibPort{p: p, peer: peer, dev: d, sys: sy, sw: sy.sw, guid: normGUID(p.GUID), adminUp: p.AdminUp}
			if ip.sw {
				ip.number = trailingNumber(p.Name)
				ip.dname = strconv.Itoa(ip.number)
			} else {
				hca := 0
				for _, x := range d.Ports {
					if x.Kind == "infiniband" {
						hca++
					}
					if x == p {
						break
					}
				}
				ip.number = 1
				ip.dname = "HCA-" + strconv.Itoa(hca) + "/1"
			}
			ip.name = ip.guid + "_" + strconv.Itoa(ip.number)
			sn.byPort[p] = ip
			sn.ports = append(sn.ports, ip)
		}
		for _, l := range m.Links {
			if l.Fabric != s.fabric || l.A == nil || l.B == nil || l.A.Kind != "infiniband" || l.B.Kind != "infiniband" {
				continue
			}
			add(l.A, l.B)
			add(l.B, l.A)
		}
	})
	sort.SliceStable(sn.ports, func(i, j int) bool {
		a, b := sn.ports[i], sn.ports[j]
		if a.dev.Path != b.dev.Path {
			return a.dev.Path < b.dev.Path
		}
		return portIndex(a.dev, a.p) < portIndex(b.dev, b.p)
	})
	for _, ip := range sn.ports {
		sn.byGUID[ip.guid] = ip
		ip.sys.ports = append(ip.sys.ports, ip)
	}
	for _, sy := range sysOf {
		sn.systems = append(sn.systems, sy)
		sn.sysBy[sy.guid] = sy
		sn.sysBy[sy.hostname] = sy
	}
	sort.Slice(sn.systems, func(i, j int) bool { return sn.systems[i].dev.Path < sn.systems[j].dev.Path })
	return sn
}

func portIndex(d *model.Device, p *model.Port) int {
	for i, x := range d.Ports {
		if x == p {
			return i
		}
	}
	return -1
}

func trailingNumber(name string) int {
	i := len(name)
	for i > 0 && name[i-1] >= '0' && name[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(name[i:])
	return n
}

// lidKeys lists the LID table keys in topology order: one LID per switch (its port 0), one
// per HCA port.
func lidKey(ip *ibPort) string {
	if ip.sw {
		return ip.sys.guid
	}
	return ip.guid
}

// ---- operational state

type portState struct {
	up       bool
	physical string
	logical  string
}

// states asks the engine for the operational state of every port: a link is up when both of
// its ends are (admin up, cable in, no port fault, device powered).
func (s *server) states(sn *snap) map[*ibPort]portState {
	e := s.a.rt.Engine
	upCache := map[*model.Port]bool{}
	portUp := func(p *model.Port) bool {
		if v, ok := upCache[p]; ok {
			return v
		}
		v := e.PortUp(p)
		upCache[p] = v
		return v
	}
	out := make(map[*ibPort]portState, len(sn.ports))
	for _, ip := range sn.ports {
		var st portState
		switch {
		case !ip.adminUp:
			st = portState{physical: "Disabled", logical: "Down"}
		case ip.peer != nil && portUp(ip.p) && portUp(ip.peer):
			st = portState{up: true, physical: "Link Up", logical: "Active"}
		default:
			st = portState{physical: "Link Down", logical: "Down"}
		}
		out[ip] = st
	}
	return out
}

// ibSpeed names a port speed as InfiniBand does (per 4x port).
func ibSpeed(speed string) string {
	switch strings.ToUpper(speed) {
	case "800G":
		return "XDR"
	case "400G":
		return "NDR"
	case "200G":
		return "HDR"
	case "100G":
		return "EDR"
	case "56G":
		return "FDR"
	}
	if speed == "" {
		return "N/A"
	}
	return speed
}

func severity(health string, portDown bool) string {
	switch health {
	case model.Critical:
		return "Critical"
	case model.Warning:
		return "Warning"
	}
	if portDown {
		return "Warning"
	}
	return "Info"
}

// portJSON is a UFM port object: the fields of UFM's documented example, plus system_name,
// which NICo's client requires and UFM returns.
type portJSON struct {
	Description         string   `json:"description"`
	Number              int      `json:"number"`
	ExternalNumber      int      `json:"external_number"`
	PhysicalState       string   `json:"physical_state"`
	Path                string   `json:"path"`
	Tier                int      `json:"tier"`
	LID                 int      `json:"lid"`
	Mirror              string   `json:"mirror"`
	LogicalState        string   `json:"logical_state"`
	Capabilities        []string `json:"capabilities"`
	MTU                 int      `json:"mtu"`
	PeerPortDName       string   `json:"peer_port_dname"`
	Severity            string   `json:"severity"`
	ActiveSpeed         string   `json:"active_speed"`
	EnabledSpeed        []string `json:"enabled_speed"`
	SupportedSpeed      []string `json:"supported_speed"`
	ActiveWidth         string   `json:"active_width"`
	EnabledWidth        []string `json:"enabled_width"`
	SupportedWidth      []string `json:"supported_width"`
	DName               string   `json:"dname"`
	PeerNodeName        string   `json:"peer_node_name"`
	Peer                string   `json:"peer"`
	PeerNodeGUID        string   `json:"peer_node_guid"`
	SystemID            string   `json:"systemID"`
	SystemName          string   `json:"system_name"`
	NodeDescription     string   `json:"node_description"`
	Name                string   `json:"name"`
	Module              string   `json:"module"`
	PeerLID             int      `json:"peer_lid"`
	PeerGUID            string   `json:"peer_guid"`
	PeerNodeDescription string   `json:"peer_node_description"`
	GUID                string   `json:"guid"`
}

func nodeDescription(ip *ibPort) string {
	if ip.sw {
		return ip.sys.hostname
	}
	return ip.sys.hostname + " " + strings.TrimSuffix(ip.dname, "/1")
}

func (s *server) renderPort(ip *ibPort, st portState, lid map[string]int, health map[*model.Device]string, sn *snap) portJSON {
	kind := "Computer"
	tier := 0
	if ip.sw {
		kind, tier = "Switch", 1
	}
	speed := ibSpeed(ip.p.Speed)
	pj := portJSON{
		Description: kind + " IB Port", Number: ip.number, ExternalNumber: ip.number,
		PhysicalState: st.physical, LogicalState: st.logical, Tier: tier, LID: lid[lidKey(ip)], Mirror: "disable",
		Path:         "default / " + kind + ": " + ip.sys.hostname + " / NA / " + ip.dname,
		Capabilities: []string{}, MTU: 4096, Severity: severity(health[ip.dev], !st.up),
		ActiveSpeed: speed, EnabledSpeed: []string{speed}, SupportedSpeed: []string{speed},
		ActiveWidth: "4x", EnabledWidth: []string{"4x"}, SupportedWidth: []string{"4x"},
		DName: ip.dname, SystemID: ip.sys.guid, SystemName: ip.sys.hostname, NodeDescription: nodeDescription(ip),
		Name: ip.name, Module: "N/A", GUID: ip.guid,
	}
	if ip.sys.sm {
		pj.Capabilities = []string{"sm"}
	}
	if !st.up {
		pj.ActiveSpeed, pj.ActiveWidth = "N/A", "N/A"
	}
	if peer := sn.byPort[ip.peer]; peer != nil && st.up {
		pj.Peer, pj.PeerGUID, pj.PeerLID = peer.name, peer.guid, lid[lidKey(peer)]
		pj.PeerNodeGUID, pj.PeerNodeName, pj.PeerPortDName = peer.sys.guid, peer.sys.hostname, peer.dname
		pj.PeerNodeDescription = nodeDescription(peer)
	}
	return pj
}

// systemJSON is a UFM system object with the fields of UFM's documented example.
type systemJSON struct {
	CPUsNumber          int      `json:"cpus_number"`
	IP                  string   `json:"ip"`
	RAM                 int      `json:"ram"`
	FWVersion           string   `json:"fw_version"`
	MirroringTemplate   bool     `json:"mirroring_template"`
	CPUSpeed            int      `json:"cpu_speed"`
	IsManualIP          bool     `json:"is_manual_ip"`
	Technology          string   `json:"technology"`
	PSID                string   `json:"psid"`
	GUID                string   `json:"guid"`
	Severity            string   `json:"severity"`
	Script              string   `json:"script"`
	Capabilities        []string `json:"capabilities"`
	State               string   `json:"state"`
	Role                string   `json:"role"`
	Type                string   `json:"type"`
	SMMode              string   `json:"sm_mode"`
	Vendor              string   `json:"vendor"`
	Description         string   `json:"description"`
	HasUFMAgent         bool     `json:"has_ufm_agent"`
	ServerOperationMode string   `json:"server_operation_mode"`
	Groups              []string `json:"groups"`
	TotalAlarms         int      `json:"total_alarms"`
	Temperature         string   `json:"temperature"`
	Uptime              string   `json:"uptime"`
	SystemName          string   `json:"system_name"`
	SWVersion           string   `json:"sw_version"`
	SystemGUID          string   `json:"system_guid"`
	Name                string   `json:"name"`
	URL                 string   `json:"url"`
	Modules             []string `json:"modules"`
	CPUType             string   `json:"cpu_type"`
	IsManaged           bool     `json:"is_managed"`
	Model               string   `json:"model"`
	Ports               []string `json:"ports"`
}

func (s *server) renderSystem(sy *system, states map[*ibPort]portState, health map[*model.Device]string) systemJSON {
	d := sy.dev
	id := d.Profile.Spec.Identity
	vendor, mdl := "", ""
	if id != nil {
		vendor, mdl = id.Manufacturer, id.Model
	}
	typ, role, mode := "host", "endpoint", "Compute_Server"
	if sy.sw {
		typ, role, mode = "switch", "tor", "Switch"
	}
	smMode := "noSM"
	if sy.sm {
		smMode = "activeSM"
	}
	ip := sy.siteIP
	if ip == "" {
		ip = "0.0.0.0"
	}
	state, anyDown := "inactive", false
	ports := []string{}
	tech := "N/A"
	for _, p := range sy.ports {
		ports = append(ports, p.name)
		if states[p].up {
			state = "active"
		} else {
			anyDown = true
		}
		tech = ibSpeed(p.p.Speed)
	}
	return systemJSON{IP: ip, FWVersion: sy.fw, Technology: tech, PSID: "N/A", GUID: sy.guid, Severity: severity(health[d], anyDown),
		Script: "N/A", Capabilities: []string{}, State: state, Role: role, Type: typ, SMMode: smMode, Vendor: vendor,
		Description: mdl, ServerOperationMode: mode, Groups: []string{}, Temperature: "N/A", Uptime: "N/A",
		SystemName: sy.hostname, SWVersion: "N/A", SystemGUID: sy.guid, Name: sy.guid, Modules: []string{}, CPUType: "any",
		IsManaged: true, Model: mdl, Ports: ports}
}

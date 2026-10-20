package nmxc

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/nvl"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// The domain as NMX-C sees it: compute nodes (compute trays) with their GPUs, switch nodes
// (switch trays) with their NVLink switch ASICs, and the NVLink lanes between them. The structure
// is fixed when the adapter starts; every value that can change (power, health, lane state, the
// domain's state) is observed from the engine per request, in one snapshot.

// Placement and identity conventions (Report says them too).
const (
	hostID       = 1      // one operating-system instance per compute tray
	nvidiaVendor = 0x10de // PCI vendor id NMX-C reports for GPUs and switches
)

type node struct {
	dev     *model.Device
	chassis int
	gpus    []*gpu
}

type gpu struct {
	comp  *model.Component
	node  *node
	uid   uint64
	id    uint32 // module id, from 1
	lanes []*model.Port
}

type swNode struct {
	dev     *model.Device
	chassis int
	asics   []*asic
}

type asic struct {
	comp  *model.Component
	sw    *swNode
	uid   uint64
	id    uint32 // switch index on the tray, from 0
	ports []*model.Port
}

// end is one end of an NVLink lane.
type end struct {
	gpu  *gpu
	asic *asic
	num  uint32 // port number on the GPU or ASIC, from 0
}

// conn is one NVLink connection: a GPU lane to a switch port, or a switch-to-switch trunk.
type conn struct {
	a, b   end
	pa, pb *model.Port
	trunk  bool
}

// shape is a domain's fixed structure.
type shape struct {
	dom      *model.Domain
	uuid     string
	racks    []*model.Rack
	chassis  []string
	nodes    []*node
	sws      []*swNode
	gpus     []*gpu
	gpuBy    map[uint64]*gpu
	asics    []*asic
	conns    []*conn
	trays    []*model.Device
	prefixes []string
	maxLanes int
	maxPorts int
}

func newShape(m *model.Model, seed int64, dom *model.Domain) *shape {
	s := &shape{dom: dom, uuid: nvl.DomainUUID(seed, dom), racks: dom.Racks, gpuBy: map[uint64]*gpu{}}
	members := map[string]bool{}
	for _, p := range dom.Members {
		members[p] = true
	}
	ends := map[*model.Port]end{}
	for ri, r := range dom.Racks {
		s.chassis = append(s.chassis, nvl.ChassisSerial(seed, r))
		s.prefixes = append(s.prefixes, r.Path+"/")
		var devs []*model.Device
		devs = append(devs, r.Devices...)
		sort.Slice(devs, func(i, j int) bool { return devs[i].Name < devs[j].Name })
		for _, d := range devs {
			switch d.BaseClass {
			case "compute-tray":
				n := &node{dev: d, chassis: ri}
				for _, c := range d.Components {
					if c.Kind != "accelerator" || !members[c.Path] {
						continue
					}
					g := &gpu{comp: c, node: n, uid: nvl.FabricGUID(seed, c), id: uint32(c.Index) + 1}
					for _, p := range c.Ports {
						if p.Kind == "scale-up" {
							ends[p] = end{gpu: g, num: uint32(len(g.lanes))}
							g.lanes = append(g.lanes, p)
						}
					}
					if len(g.lanes) > s.maxLanes {
						s.maxLanes = len(g.lanes)
					}
					n.gpus = append(n.gpus, g)
					s.gpus = append(s.gpus, g)
					s.gpuBy[g.uid] = g
				}
				if len(n.gpus) > 0 {
					s.nodes = append(s.nodes, n)
				}
			case "switch-tray":
				sw := &swNode{dev: d, chassis: ri}
				for _, c := range d.Components {
					if c.Kind != "asic" {
						continue
					}
					a := &asic{comp: c, sw: sw, uid: nvl.FabricGUID(seed, c), id: uint32(c.Index)}
					for _, p := range c.Ports {
						if p.Kind == "scale-up" {
							ends[p] = end{asic: a, num: uint32(len(a.ports))}
							a.ports = append(a.ports, p)
						}
					}
					if len(a.ports) > s.maxPorts {
						s.maxPorts = len(a.ports)
					}
					sw.asics = append(sw.asics, a)
					s.asics = append(s.asics, a)
				}
				s.sws = append(s.sws, sw)
				s.trays = append(s.trays, d)
			}
		}
	}
	// lanes: every GPU lane to its switch port, then switch-to-switch trunks once each
	for _, g := range s.gpus {
		for _, p := range g.lanes {
			if p.Link == nil {
				continue
			}
			q := p.Link.Peer(p)
			if e, ok := ends[q]; ok && e.asic != nil {
				s.conns = append(s.conns, &conn{a: ends[p], b: e, pa: p, pb: q})
			}
		}
	}
	for _, a := range s.asics {
		for _, p := range a.ports {
			if p.Link == nil {
				continue
			}
			q := p.Link.Peer(p)
			e, ok := ends[q]
			if !ok || e.asic == nil || q.Ref < p.Ref {
				continue
			}
			s.conns = append(s.conns, &conn{a: ends[p], b: e, pa: p, pb: q, trunk: true})
		}
	}
	return s
}

// touches reports whether an event path lies within the domain.
func (s *shape) touches(path string) bool {
	if path == s.dom.Path {
		return true
	}
	for _, p := range s.prefixes {
		if strings.HasPrefix(path, p) || path+"/" == p {
			return true
		}
	}
	return false
}

// ---- snapshot

type snapshot struct {
	at      time.Time
	state   string // the engine's domain state: formed, degraded, unformed
	powered map[*model.Device]bool
	up      map[*conn]bool
	upPort  map[*model.Port]bool
	gpu     map[*gpu]string
	node    map[*node]string
	asic    map[*asic]string
	sw      map[*swNode]string
	reason  map[*gpu]string
}

// observe takes one snapshot of everything that can change.
func (s *shape) observe(e *engine.Engine) *snapshot {
	sn := &snapshot{at: e.Now(), state: e.DomainState(s.dom.Path), powered: map[*model.Device]bool{}, up: map[*conn]bool{},
		upPort: map[*model.Port]bool{}, gpu: map[*gpu]string{}, node: map[*node]string{}, asic: map[*asic]string{}, sw: map[*swNode]string{}, reason: map[*gpu]string{}}
	e.Read(func(*model.Model) {
		for _, n := range s.nodes {
			sn.powered[n.dev] = n.dev.State.AC && n.dev.State.Power == model.On
		}
		for _, w := range s.sws {
			sn.powered[w.dev] = w.dev.State.AC && w.dev.State.Power == model.On
		}
	})
	for _, c := range s.conns {
		up := e.PortUp(c.pa) && e.PortUp(c.pb)
		sn.up[c] = up
		sn.upPort[c.pa], sn.upPort[c.pb] = up, up
	}
	for _, g := range s.gpus {
		h, state := e.ComponentHealth(g.comp)
		lanes := 0
		for _, p := range g.lanes {
			if sn.upPort[p] {
				lanes++
			}
		}
		switch {
		case !sn.powered[g.node.dev]:
			sn.gpu[g], sn.reason[g] = "NMX_GPU_HEALTH_NO_NVLINK", "compute tray not powered"
		case h == model.Critical || state == "UnavailableOffline":
			sn.gpu[g], sn.reason[g] = "NMX_GPU_HEALTH_NO_NVLINK", "GPU "+strings.ToLower(h)
		case lanes == 0:
			sn.gpu[g], sn.reason[g] = "NMX_GPU_HEALTH_NO_NVLINK", "no NVLink lane up"
		case lanes < len(g.lanes):
			sn.gpu[g], sn.reason[g] = "NMX_GPU_HEALTH_DEGRADED", fmt.Sprintf("%d of %d NVLink lanes up", lanes, len(g.lanes))
		default:
			sn.gpu[g] = "NMX_GPU_HEALTH_HEALTHY"
		}
	}
	for _, n := range s.nodes {
		no := 0
		for _, g := range n.gpus {
			if sn.gpu[g] == "NMX_GPU_HEALTH_NO_NVLINK" {
				no++
			}
		}
		switch {
		case no == len(n.gpus):
			sn.node[n] = "NMX_COMPUTE_NODE_HEALTH_UNHEALTHY"
		case no > 0:
			sn.node[n] = "NMX_COMPUTE_NODE_HEALTH_DEGRADED"
		default:
			sn.node[n] = "NMX_COMPUTE_NODE_HEALTH_HEALTHY"
		}
	}
	for _, w := range s.sws {
		dh := e.Health(w.dev)
		bad, healthy := 0, 0
		for _, a := range w.asics {
			ch, _ := e.ComponentHealth(a.comp)
			cabled, up := 0, 0
			for _, p := range a.ports {
				if p.Link != nil {
					cabled++
					if sn.upPort[p] {
						up++
					}
				}
			}
			switch {
			case !sn.powered[w.dev] || dh == model.Critical || ch == model.Critical:
				sn.asic[a] = "NMX_SWITCH_HEALTH_UNHEALTHY"
				bad++
			case up < cabled:
				sn.asic[a] = "NMX_SWITCH_HEALTH_MISSING_NVLINK"
			default:
				sn.asic[a] = "NMX_SWITCH_HEALTH_HEALTHY"
				healthy++
			}
		}
		switch {
		case len(w.asics) > 0 && bad == len(w.asics):
			sn.sw[w] = "NMX_SWITCH_NODE_HEALTH_UNHEALTHY"
		case healthy == len(w.asics):
			sn.sw[w] = "NMX_SWITCH_NODE_HEALTH_HEALTHY"
		default:
			sn.sw[w] = "NMX_SWITCH_NODE_HEALTH_MISSING_NVLINK"
		}
	}
	return sn
}

// partitionHealth is a partition's health from its GPUs': healthy when every GPU is; unhealthy
// when none can use NVLink; degraded (routing disabled for some) when some cannot; degraded
// bandwidth when lanes are down.
func (sn *snapshot) partitionHealth(s *shape, p *partition) string {
	if len(p.GPUs) == 0 {
		return "NMX_PARTITION_HEALTH_HEALTHY"
	}
	no, deg := 0, 0
	for _, u := range p.GPUs {
		g := s.gpuBy[u]
		if g == nil {
			no++
			continue
		}
		switch sn.gpu[g] {
		case "NMX_GPU_HEALTH_NO_NVLINK":
			no++
		case "NMX_GPU_HEALTH_DEGRADED", "NMX_GPU_HEALTH_DEGRADED_BW":
			deg++
		}
	}
	switch {
	case no == len(p.GPUs):
		return "NMX_PARTITION_HEALTH_UNHEALTHY"
	case no > 0:
		return "NMX_PARTITION_HEALTH_DEGRADED"
	case deg > 0:
		return "NMX_PARTITION_HEALTH_DEGRADED_BANDWIDTH"
	}
	return "NMX_PARTITION_HEALTH_HEALTHY"
}

// controlPlane maps the engine's domain state onto NMX-C's control plane state and health.
func (sn *snapshot) controlPlane() (state, health, why string) {
	switch sn.state {
	case "formed":
		return "NMX_CONTROL_PLANE_STATE_CONFIGURED", "NMX_CONTROLLER_HEALTH_HEALTHY", "NVLink domain configured"
	case "degraded":
		return "NMX_CONTROL_PLANE_STATE_DEGRADED", "NMX_CONTROLLER_HEALTH_DEGRADED", "NVLink domain degraded: a member GPU is unhealthy or its compute tray is not powered"
	}
	return "NMX_CONTROL_PLANE_STATE_ERROR", "NMX_CONTROLLER_HEALTH_UNHEALTHY", "NVLink domain not formed: a switch tray is failed or not powered"
}

// ---- locations

func (s *shape) location(d *model.Device, chassis int, host uint64) obj {
	return obj{"chassis_id": chassis, "slot_id": nvl.Slot(d), "host_id": host}
}

func (s *shape) locationInfo(d *model.Device, chassis int, host uint64) obj {
	return obj{"chassis_serial_number": s.chassis[chassis], "tray_index": nvl.TrayIndex(d), "location": s.location(d, chassis, host)}
}

func (s *shape) nodeLocation(n *node) obj         { return s.location(n.dev, n.chassis, hostID) }
func (s *shape) nodeLocationInfo(n *node) obj     { return s.locationInfo(n.dev, n.chassis, hostID) }
func (s *shape) switchLocation(w *swNode) obj     { return s.location(w.dev, w.chassis, 0) }
func (s *shape) switchLocationInfo(w *swNode) obj { return s.locationInfo(w.dev, w.chassis, 0) }

// sameLocation compares a requested Location with a device's.
func (s *shape) sameLocation(l *location, d *model.Device, chassis int, host uint64) bool {
	if l == nil {
		return true
	}
	return uint64(l.Chassis) == uint64(chassis) && uint64(l.Slot) == uint64(nvl.Slot(d)) && (uint64(l.Host) == host || l.Host == 0)
}

// gpuAt finds a GPU by location and module id.
func (s *shape) gpuAt(l *location, id uint32) *gpu {
	for _, n := range s.nodes {
		if !s.sameLocation(l, n.dev, n.chassis, hostID) {
			continue
		}
		for _, g := range n.gpus {
			if g.id == id {
				return g
			}
		}
	}
	return nil
}

// endpoint is a lane end as NMX-C's LinkEndPoint.
func (s *shape) endpoint(e end) obj {
	if e.gpu != nil {
		return obj{"loc": s.nodeLocation(e.gpu.node), "switch_orgpu_id": e.gpu.id, "port_num": e.num}
	}
	return obj{"loc": s.switchLocation(e.asic.sw), "switch_orgpu_id": e.asic.id, "port_num": e.num}
}

func (e end) uid() uint64 {
	if e.gpu != nil {
		return e.gpu.uid
	}
	return e.asic.uid
}

// atLocation reports whether a lane end is on the device a requested location names.
func (s *shape) atLocation(l *location, e end) bool {
	if e.gpu != nil {
		return s.sameLocation(l, e.gpu.node.dev, e.gpu.node.chassis, hostID)
	}
	return s.sameLocation(l, e.asic.sw.dev, e.asic.sw.chassis, 0)
}

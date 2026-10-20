package nmxc

import (
	"context"
	"fmt"
	"sort"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/BudEcosystem/dc-twin/internal/adapter/nvl"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// NMX-C reports an operation's outcome in server_header.return_code, not as a gRPC status, and
// NICo's client reads it there (libnmxc's check_server_header_success): a rejected partition
// operation is an OK response carrying the controller's code. A gRPC status is used for what the
// controller cannot parse, and UNIMPLEMENTED for an RPC the twin does not serve.

// Fixed controller facts, as the NVOS capture behind the NVUE adapter reports its nmx-controller
// application ("app-ver", "components-ver", "capabilities", the default partition's
// "mcast-limit").
var (
	appVersion = "4.21.156"
	components = [][2]string{{"sm", "2025.10.18"}, {"gfm", "580.173.04"}, {"fib", "4.21.156"}}
	caps       = []string{"sm", "gfm", "fib", "gw-api"}
)

const multicastGroups = 1024

// handler serves one unary RPC: a response body (without server_header) and either a
// controller return code (*stError) or a gRPC status error.
type handler func(c *call) (obj, error)

// call is one request on one tray's controller.
type call struct {
	s       *trayServer
	method  protoreflect.MethodDescriptor
	in      *dynamicpb.Message
	gateway string
	peer    string
	data    map[string]any // logged with the request
}

func (c *call) decode(into any) error {
	if err := Decode(c.in, into); err != nil {
		return status.Errorf(codes.InvalidArgument, "%s: %v", c.method.Input().FullName(), err)
	}
	return nil
}

// RPCs the twin serves, with the canonical request name each is logged under (14).
var rpcs = map[string]struct {
	canonical string
	fn        func(*call) (obj, error)
}{
	"Hello":                      {"inventory.read", (*call).hello},
	"FactoryReset":               {"other", (*call).factoryReset},
	"GetDomainProperties":        {"inventory.read", (*call).domainProperties},
	"GetDomainStateInfo":         {"health.read", (*call).domainStateInfo},
	"GetTopologyInfo":            {"network.read", (*call).topologyInfo},
	"GetComputeNodeCount":        {"inventory.read", (*call).computeNodeCount},
	"GetComputeNodeLocationList": {"inventory.read", (*call).computeNodeLocationList},
	"GetComputeNodeInfoList":     {"inventory.read", (*call).computeNodeInfoList},
	"GetGpuInfoList":             {"inventory.read", (*call).gpuInfoList},
	"GetSwitchNodeCount":         {"inventory.read", (*call).switchNodeCount},
	"GetSwitchNodeLocationList":  {"inventory.read", (*call).switchNodeLocationList},
	"GetSwitchNodeInfoList":      {"inventory.read", (*call).switchNodeInfoList},
	"GetSwitchInfoList":          {"inventory.read", (*call).switchInfoList},
	"GetPartitionCount":          {"network.read", (*call).partitionCount},
	"GetPartitionIdList":         {"network.read", (*call).partitionIDList},
	"GetPartitionInfoList":       {"network.read", (*call).partitionInfoList},
	"CreatePartition":            {"other", (*call).createPartition},
	"DeletePartition":            {"other", (*call).deletePartition},
	"AddGpusToPartition":         {"other", (*call).addGPUs},
	"RemoveGpusFromPartition":    {"other", (*call).removeGPUs},
	"GetConnCount":               {"network.read", (*call).connCount},
	"GetConnInfoList":            {"network.read", (*call).connInfoList},
	"GetConnInfoCombined":        {"network.read", (*call).connInfoCombined},
	"GetStateReport":             {"health.read", (*call).stateReport},
}

// Served lists the RPCs served, Subscribe included, for the report.
func Served() []string {
	out := []string{"Subscribe"}
	for k := range rpcs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// serviceDesc registers every RPC of the compiled definition: the served ones dispatch to their
// handler, the others answer UNIMPLEMENTED as a controller does for what it does not serve.
func (s *trayServer) serviceDesc() *grpc.ServiceDesc {
	sd := s.a.ps.Service
	desc := &grpc.ServiceDesc{ServiceName: string(sd.FullName()), HandlerType: (*any)(nil), Metadata: ProtoFile}
	ms := sd.Methods()
	for i := 0; i < ms.Len(); i++ {
		m := ms.Get(i)
		if m.IsStreamingClient() || m.IsStreamingServer() {
			desc.Streams = append(desc.Streams, grpc.StreamDesc{StreamName: string(m.Name()), ServerStreams: m.IsStreamingServer(), ClientStreams: m.IsStreamingClient(),
				Handler: func(_ any, stream grpc.ServerStream) error { return s.stream(m, stream) }})
			continue
		}
		desc.Methods = append(desc.Methods, grpc.MethodDesc{MethodName: string(m.Name()),
			Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				in := dynamicpb.NewMessage(m.Input())
				if err := dec(in); err != nil {
					return nil, err
				}
				return s.unary(ctx, m, in)
			}})
	}
	return desc
}

func (s *trayServer) unary(ctx context.Context, m protoreflect.MethodDescriptor, in *dynamicpb.Message) (out any, err error) {
	c := &call{s: s, method: m, in: in, peer: peerAddr(ctx)}
	var hdr struct {
		Gateway string `json:"gateway_id"`
	}
	_ = Decode(in, &hdr)
	c.gateway = hdr.Gateway
	r, ok := rpcs[string(m.Name())]
	canonical := r.canonical
	if !ok {
		canonical = "other"
	}
	rc := "NMX_ST_SUCCESS"
	defer func() { s.log(c, canonical, err, rc) }()
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "the dc-twin NMX-C does not implement %s", m.Name())
	}
	body, herr := r.fn(c)
	var se *stError
	switch {
	case herr == nil:
	case asSt(herr, &se):
		rc = se.code
	default:
		return nil, herr
	}
	if body == nil {
		body = obj{}
	}
	body["server_header"] = s.header(rc)
	msg, err := Build(m.Output(), body)
	if err != nil {
		s.a.rt.Engine.Incident("nmxc", fmt.Sprintf("%s: response does not fit the interface definition: %v", m.Name(), err))
		return nil, status.Errorf(codes.Internal, "dc-twin NMX-C: %v", err)
	}
	return msg, nil
}

func asSt(err error, se **stError) bool {
	e, ok := err.(*stError)
	if ok {
		*se = e
	}
	return ok
}

func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}

// header is the server header every response carries.
func (s *trayServer) header(rc string) obj {
	return obj{"domain_uuid": s.d.uuid, "app_uuid": s.appUUID, "app_ver": appVersion, "return_code": rc}
}

// log records the request with the engine by its canonical name; partition writes carry what
// they did.
func (s *trayServer) log(c *call, canonical string, err error, rc string) {
	st := 200
	if err != nil {
		st = httpStatus(status.Code(err))
	}
	data := c.data
	if rc != "NMX_ST_SUCCESS" {
		if data == nil {
			data = map[string]any{}
		}
		data["returnCode"] = rc
	}
	s.a.rt.Engine.Commit(schema.Event{Kind: "request", Path: s.dev.Path, Data: data,
		Source:  schema.EventSource{Class: "stack", Principal: c.gateway, Address: c.peer},
		Request: &schema.EventRequest{Protocol: Protocol, Method: string(c.method.Name()), Target: "/" + ServiceName + "/" + string(c.method.Name()), Canonical: canonical, Status: st}})
}

func httpStatus(c codes.Code) int {
	switch c {
	case codes.OK:
		return 200
	case codes.InvalidArgument:
		return 400
	case codes.NotFound:
		return 404
	case codes.Unimplemented:
		return 501
	case codes.Unavailable:
		return 503
	case codes.Canceled:
		return 499
	}
	return 500
}

// ---- requests

type location struct {
	Chassis u64 `json:"chassis_id"`
	Slot    u64 `json:"slot_id"`
	Host    u64 `json:"host_id"`
}

type partitionID struct {
	ID uint32 `json:"partition_id"`
}

type gpuLocation struct {
	Loc *location `json:"loc"`
	ID  uint32    `json:"gpu_id"`
}

// ---- handlers

func (c *call) hello() (obj, error) {
	var req struct {
		Major enum `json:"major_version"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	var kv []obj
	for _, p := range components {
		kv = append(kv, obj{"key": p[0], "value": p[1]})
	}
	body := obj{"components_ver": kv, "capabilities": caps, "host_os_details": "NVOS " + c.s.nvos() + " (dc-twin model of " + c.s.dev.Path + ")",
		"major_version": "PROTO_MSG_MAJOR_VERSION", "minor_version": "PROTO_MSG_MINOR_VERSION"}
	if req.Major.set() && req.Major != "PROTO_MSG_MAJOR_VERSION" {
		return body, st("NMX_ST_VERSION_MISMATCH", "protocol major version %s is not supported", req.Major)
	}
	return body, nil
}

func (c *call) factoryReset() (obj, error) {
	d := c.s.d
	d.mu.Lock()
	d.tab = newTable(d.uids(), d.a.cfg.factoryPartition)
	err := d.saveLocked()
	d.mu.Unlock()
	c.data = map[string]any{"partitions": "factory default"}
	d.a.kick(d)
	return obj{}, err
}

func (c *call) domainProperties() (obj, error) {
	d := c.s.d
	perChassis := map[int]int{}
	swPerChassis := map[int]int{}
	gpusPerNode, perSwitch := 0, 0
	for _, n := range d.nodes {
		perChassis[n.chassis]++
		if len(n.gpus) > gpusPerNode {
			gpusPerNode = len(n.gpus)
		}
	}
	for _, w := range d.sws {
		swPerChassis[w.chassis]++
		if len(w.asics) > perSwitch {
			perSwitch = len(w.asics)
		}
	}
	ports := 0
	for _, g := range d.gpus {
		ports += len(g.lanes)
	}
	for _, a := range d.asics {
		ports += len(a.ports)
	}
	return obj{"max_compute_nodes": len(d.nodes), "max_compute_nodes_per_chassis": maxOf(perChassis), "max_gpus_per_compute_node": gpusPerNode,
		"max_gpu_nv_links": d.maxLanes, "max_switch_nodes": len(d.sws), "max_switch_nodes_per_chassis": maxOf(swPerChassis),
		"max_switches_per_switch_node": perSwitch, "max_switch_nv_links": d.maxPorts, "max_num_partitions": len(d.gpus),
		"max_multicast_groups": multicastGroups, "max_num_ports": ports}, nil
}

func maxOf(m map[int]int) int {
	n := 0
	for _, v := range m {
		if v > n {
			n = v
		}
	}
	return n
}

func (c *call) domainStateInfo() (obj, error) {
	return c.s.d.stateInfo(c.s.d.observe()), nil
}

// stateInfo is DomainStateInfo's body for a snapshot (also the Subscribe heartbeat). No
// multicast group is ever allocated (no NVLink traffic is modelled), so all are available; a
// partition's multicast limit caps what it may allocate and reserves nothing.
func (d *domain) stateInfo(sn *snapshot) obj {
	state, health, why := sn.controlPlane()
	return obj{"control_plane_state": state, "available_multicast_groups": multicastGroups, "config_status_description": why, "nmx_controller_health": health}
}

func (c *call) computeNodeCount() (obj, error) {
	var req struct {
		Attr    enum `json:"attr"`
		Chassis *u64 `json:"chassis_id"`
		Health  enum `json:"node_health"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	nodes := c.s.d.selectNodes(c.s.d.observe(), req.Attr, req.Health, req.Chassis, 0)
	return obj{"num_nodes": len(nodes)}, nil
}

func (c *call) computeNodeLocationList() (obj, error) {
	var req struct {
		Attr    enum   `json:"attr"`
		Health  enum   `json:"node_health"`
		Chassis *u64   `json:"chassis_id"`
		Num     uint32 `json:"num_nodes"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	d := c.s.d
	var locs []obj
	for _, n := range d.selectNodes(d.observe(), req.Attr, req.Health, req.Chassis, int(req.Num)) {
		locs = append(locs, d.nodeLocation(n))
	}
	return obj{"loc_list": orEmpty(locs)}, nil
}

// selectNodes filters compute nodes by allocation, health and chassis.
func (d *domain) selectNodes(sn *snapshot, attr, health enum, chassis *u64, limit int) []*node {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*node
	for _, n := range d.nodes {
		in := 0
		for _, g := range n.gpus {
			if d.tab.of(g.uid) != nil {
				in++
			}
		}
		switch attr {
		case "NMX_COMPUTE_NODE_ATTR_FREE":
			if in != 0 {
				continue
			}
		case "NMX_COMPUTE_NODE_ATTR_FULLY_ALLOCATED":
			if in != len(n.gpus) {
				continue
			}
		case "NMX_COMPUTE_NODE_ATTR_PARTIALLY_ALLOCATED":
			if in == 0 || in == len(n.gpus) {
				continue
			}
		}
		if health.set() && enum(sn.node[n]) != health {
			continue
		}
		if chassis != nil && int(*chassis) != n.chassis && *chassis != 0 {
			continue
		}
		out = append(out, n)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

func (c *call) computeNodeInfoList() (obj, error) {
	var req struct {
		Locs []location `json:"loc_list"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	d := c.s.d
	sn := d.observe()
	d.mu.Lock()
	defer d.mu.Unlock()
	var list []obj
	for _, n := range d.nodes {
		if len(req.Locs) > 0 {
			hit := false
			for i := range req.Locs {
				hit = hit || d.sameLocation(&req.Locs[i], n.dev, n.chassis, hostID)
			}
			if !hit {
				continue
			}
		}
		ids := map[uint32]bool{}
		for _, g := range n.gpus {
			if p := d.tab.of(g.uid); p != nil {
				ids[p.ID] = true
			}
		}
		list = append(list, obj{"loc": d.nodeLocationInfo(n), "num_gpus": len(n.gpus), "node_health": sn.node[n], "partition_id_list": partitionIDs(ids)})
	}
	return obj{"node_info_list": orEmpty(list)}, nil
}

func partitionIDs(ids map[uint32]bool) []obj {
	var keys []uint32
	for k := range ids {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := []obj{}
	for _, k := range keys {
		out = append(out, obj{"partition_id": k})
	}
	return out
}

func orEmpty(l []obj) []obj {
	if l == nil {
		return []obj{}
	}
	return l
}

func (c *call) gpuInfoList() (obj, error) {
	var req struct {
		Attr      enum         `json:"attr"`
		Num       uint32       `json:"num_gpus"`
		Loc       *location    `json:"loc"`
		Partition *partitionID `json:"partition_id"`
		Health    enum         `json:"gpu_health"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	d := c.s.d
	sn := d.observe()
	d.mu.Lock()
	defer d.mu.Unlock()
	var rc error
	if req.Partition != nil && d.tab.Partitions[req.Partition.ID] == nil {
		// a filter naming a partition that is not provisioned is an error, not an empty success
		rc = st("NMX_ST_PARTITION_ID_NOT_IN_USE", "partition %d is not provisioned", req.Partition.ID)
	}
	var list []obj
	for _, g := range d.gpus {
		p := d.tab.of(g.uid)
		if req.Partition != nil && (p == nil || p.ID != req.Partition.ID) {
			continue
		}
		if req.Attr == "NMX_GPU_ATTR_LOCATION" && req.Loc != nil && !d.sameLocation(req.Loc, g.node.dev, g.node.chassis, hostID) {
			continue
		}
		if req.Health.set() && enum(sn.gpu[g]) != req.Health {
			continue
		}
		info := obj{"loc": d.nodeLocationInfo(g.node), "gpu_id": g.id, "gpu_uid": g.uid, "gpu_health": sn.gpu[g]}
		if p != nil {
			info["partition_id"] = obj{"partition_id": p.ID}
		}
		list = append(list, info)
		if req.Num > 0 && len(list) == int(req.Num) {
			break
		}
	}
	return obj{"gpu_info_list": orEmpty(list)}, rc
}

func (c *call) switchNodeCount() (obj, error) {
	var req struct {
		Attr   enum `json:"attr"`
		Health enum `json:"node_health"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	return obj{"num_nodes": len(c.s.d.selectSwitches(c.s.d.observe(), req.Attr, req.Health, 0))}, nil
}

func (c *call) switchNodeLocationList() (obj, error) {
	var req struct {
		Attr   enum   `json:"attr"`
		Health enum   `json:"node_health"`
		Num    uint32 `json:"num_nodes"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	d := c.s.d
	var locs []obj
	for _, w := range d.selectSwitches(d.observe(), req.Attr, req.Health, int(req.Num)) {
		locs = append(locs, d.switchLocation(w))
	}
	return obj{"loc_list": orEmpty(locs)}, nil
}

// selectSwitches filters switch nodes. The NVLink fabric of an NVL rack is one tier: every
// switch is L1, none is L2.
func (d *domain) selectSwitches(sn *snapshot, attr, health enum, limit int) []*swNode {
	if attr == "NMX_SWITCH_NODE_ATTR_L2" {
		return nil
	}
	var out []*swNode
	for _, w := range d.sws {
		if health.set() && enum(sn.sw[w]) != health {
			continue
		}
		out = append(out, w)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

func (c *call) switchNodeInfoList() (obj, error) {
	var req struct {
		Locs []location `json:"loc_list"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	d := c.s.d
	sn := d.observe()
	d.mu.Lock()
	ids := map[uint32]bool{}
	for _, p := range d.tab.list() {
		ids[p.ID] = true // an NVLink partition spans the whole switch fabric
	}
	d.mu.Unlock()
	var list []obj
	for _, w := range d.sws {
		if len(req.Locs) > 0 {
			hit := false
			for i := range req.Locs {
				hit = hit || d.sameLocation(&req.Locs[i], w.dev, w.chassis, 0)
			}
			if !hit {
				continue
			}
		}
		list = append(list, obj{"loc": d.switchLocationInfo(w), "num_switches": len(w.asics), "node_health": sn.sw[w], "partition_id_list": partitionIDs(ids)})
	}
	return obj{"node_info_list": orEmpty(list)}, nil
}

func (c *call) switchInfoList() (obj, error) {
	var req struct {
		Loc *location `json:"loc"`
		Num uint32    `json:"num_switches"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	d := c.s.d
	sn := d.observe()
	var list []obj
	for _, a := range d.asics {
		if req.Loc != nil && !d.sameLocation(req.Loc, a.sw.dev, a.sw.chassis, 0) {
			continue
		}
		list = append(list, obj{"loc": d.switchLocation(a.sw), "switch_id": a.id, "switch_uid": a.uid, "num_ports": len(a.ports), "health": sn.asic[a]})
		if req.Num > 0 && len(list) == int(req.Num) {
			break
		}
	}
	return obj{"switch_info_list": orEmpty(list)}, nil
}

// ---- partitions

type partitionFilter struct {
	Attr   enum   `json:"info_attr"`
	GPUs   uint32 `json:"num_gpus"`
	Nodes  uint32 `json:"num_nodes"`
	Num    uint32 `json:"num_partitions"`
	Health enum   `json:"health"`
}

func (d *domain) selectPartitions(sn *snapshot, f partitionFilter) []*partition {
	var out []*partition
	for _, p := range d.tab.list() {
		switch f.Attr {
		case "NMX_PARTITION_INFO_ATTR_NUM_GPUS":
			if uint32(len(p.GPUs)) != f.GPUs {
				continue
			}
		case "NMX_PARTITION_INFO_ATTR_NUM_COMPUTE_NODES":
			if uint32(d.nodesOf(p)) != f.Nodes {
				continue
			}
		}
		if f.Health.set() && enum(sn.partitionHealth(d.shape, p)) != f.Health {
			continue
		}
		out = append(out, p)
		if f.Num > 0 && len(out) == int(f.Num) {
			break
		}
	}
	return out
}

func (d *domain) nodesOf(p *partition) int {
	nodes := map[*node]bool{}
	for _, u := range p.GPUs {
		if g := d.gpuBy[u]; g != nil {
			nodes[g.node] = true
		}
	}
	return len(nodes)
}

func (c *call) partitionCount() (obj, error) {
	var f partitionFilter
	if err := c.decode(&f); err != nil {
		return nil, err
	}
	f.Num = 0
	d := c.s.d
	sn := d.observe()
	d.mu.Lock()
	defer d.mu.Unlock()
	return obj{"num_partitions": len(d.selectPartitions(sn, f))}, nil
}

func (c *call) partitionIDList() (obj, error) {
	var f partitionFilter
	if err := c.decode(&f); err != nil {
		return nil, err
	}
	d := c.s.d
	sn := d.observe()
	d.mu.Lock()
	defer d.mu.Unlock()
	var list []obj
	for _, p := range d.selectPartitions(sn, f) {
		list = append(list, obj{"partition_id": obj{"partition_id": p.ID}, "num_gpus": len(p.GPUs)})
	}
	return obj{"partition_list": orEmpty(list)}, nil
}

func (c *call) partitionInfoList() (obj, error) {
	var req struct {
		IDs   []partitionID `json:"partition_id_list"`
		Names []string      `json:"partition_name_list"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	var ids []uint32
	for _, p := range req.IDs {
		ids = append(ids, p.ID)
	}
	d := c.s.d
	sn := d.observe()
	d.mu.Lock()
	defer d.mu.Unlock()
	var list []obj
	for _, p := range d.tab.find(ids, req.Names) {
		list = append(list, d.partitionInfo(sn, p))
	}
	return obj{"partition_info_list": orEmpty(list)}, nil
}

func (d *domain) partitionInfo(sn *snapshot, p *partition) obj {
	var locs []obj
	uids := []uint64{}
	for _, u := range p.GPUs {
		uids = append(uids, u)
		if g := d.gpuBy[u]; g != nil {
			locs = append(locs, obj{"loc": d.nodeLocation(g.node), "gpu_id": g.id})
		}
	}
	info := obj{"partition_id": obj{"partition_id": p.ID}, "name": p.Name, "num_gpus": len(p.GPUs), "gpu_location_list": orEmpty(locs),
		"gpu_uid_list": uids, "health": sn.partitionHealth(d.shape, p), "partition_type": p.Type, "num_allocated_multicast_groups": 0}
	attr := obj{"multicast_groups_limit": p.McastLimit}
	if p.Resiliency != "" {
		attr["resiliency_mode"] = p.Resiliency
	}
	info["attr"] = attr
	return info
}

// resolveGPUs turns GPU references (uids or locations) into uids; an unknown location is a bad
// parameter, an unknown uid is checked by the table.
func (d *domain) resolveGPUs(uids []uint64, locs []gpuLocation) ([]uint64, error) {
	out := append([]uint64{}, uids...)
	for _, l := range locs {
		g := d.gpuAt(l.Loc, l.ID)
		if g == nil {
			return nil, st("NMX_ST_BADPARAM", "no GPU %d at the given location", l.ID)
		}
		out = append(out, g.uid)
	}
	return out, nil
}

func (c *call) createPartition() (obj, error) {
	var req struct {
		Name string `json:"name"`
		Res  []struct {
			Loc *gpuLocation `json:"gpu_location"`
			UID *u64         `json:"gpu_uid"`
		} `json:"gpu_resource_id"`
		Attr *struct {
			Resiliency enum   `json:"resiliency_mode"`
			Mcast      uint32 `json:"multicast_groups_limit"`
		} `json:"attr"`
		ID *partitionID `json:"partition_id"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	var uids []uint64
	var locs []gpuLocation
	for _, r := range req.Res {
		switch {
		case r.UID != nil:
			uids = append(uids, uint64(*r.UID))
		case r.Loc != nil:
			locs = append(locs, *r.Loc)
		}
	}
	typ := typeUID
	if len(locs) > 0 && len(uids) == 0 {
		typ = typeLocation
	}
	resil, mcast := "", uint32(0)
	if req.Attr != nil {
		if req.Attr.Resiliency.set() {
			resil = string(req.Attr.Resiliency)
		}
		mcast = req.Attr.Mcast
	}
	id := uint32(0)
	if req.ID != nil {
		id = req.ID.ID
	}
	d := c.s.d
	d.mu.Lock()
	all, err := d.resolveGPUs(uids, locs)
	var p *partition
	if err == nil {
		p, err = d.tab.create(req.Name, all, id, typ, resil, mcast)
	}
	if err == nil {
		err = d.saveLocked()
	}
	d.mu.Unlock()
	if err != nil {
		var se *stError
		if asSt(err, &se) {
			return obj{}, err
		}
		return obj{}, st("NMX_ST_NMX_CONTROLLER_DB_ERROR", "%v", err)
	}
	body := obj{"partition_id": obj{"partition_id": p.ID}}
	c.data = map[string]any{"partition": p.ID, "name": p.Name, "gpus": len(p.GPUs)}
	d.partitionChanged(c, "create_partition_response", body)
	return body, nil
}

func (c *call) deletePartition() (obj, error) {
	var req struct {
		ID   *partitionID `json:"partition_id"`
		Name string       `json:"name"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	id := uint32(0)
	if req.ID != nil {
		id = req.ID.ID
	}
	r, err := newRef(id, req.Name)
	if err != nil {
		return obj{}, err
	}
	d := c.s.d
	d.mu.Lock()
	got, err := d.tab.remove(r)
	if err == nil {
		err = d.saveLocked()
	}
	d.mu.Unlock()
	if err != nil {
		return obj{}, err
	}
	body := obj{"partition_id": obj{"partition_id": got}}
	c.data = map[string]any{"partition": got, "deleted": true}
	d.partitionChanged(c, "delete_partition_response", body)
	return body, nil
}

func (c *call) addGPUs() (obj, error)    { return c.update(true) }
func (c *call) removeGPUs() (obj, error) { return c.update(false) }

func (c *call) update(add bool) (obj, error) {
	var req struct {
		ID   *partitionID  `json:"partition_id"`
		Locs []gpuLocation `json:"location_list"`
		UIDs []u64         `json:"gpu_uid"`
		Name string        `json:"name"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	id := uint32(0)
	if req.ID != nil {
		id = req.ID.ID
	}
	r, err := newRef(id, req.Name)
	if err != nil {
		return obj{}, err
	}
	var uids []uint64
	for _, u := range req.UIDs {
		uids = append(uids, uint64(u))
	}
	d := c.s.d
	d.mu.Lock()
	all, err := d.resolveGPUs(uids, req.Locs)
	var got uint32
	if err == nil {
		if add {
			got, err = d.tab.addGPUs(r, all)
		} else {
			got, err = d.tab.removeGPUs(r, all)
		}
	}
	if err == nil {
		err = d.saveLocked()
	}
	d.mu.Unlock()
	if err != nil {
		return obj{}, err
	}
	body := obj{"partition_id": obj{"partition_id": got}}
	verb := "added"
	if !add {
		verb = "removed"
	}
	c.data = map[string]any{"partition": got, verb: len(all)}
	d.partitionChanged(c, "update_partition_response", body)
	return body, nil
}

// ---- connections and topology

type connFilter struct {
	Type enum      `json:"conn_type"`
	Attr enum      `json:"conn_attr"`
	Num  uint32    `json:"num_conns"`
	Loc  *location `json:"loc"`
}

func (d *domain) selectConns(sn *snapshot, f connFilter) []*conn {
	if f.Attr == "NMX_NVLINK_CONN_ATTR_UNEXPECTED" {
		return nil // the twin's lanes are the expected topology
	}
	var out []*conn
	for _, c := range d.conns {
		switch f.Type {
		case "NMX_NVLINK_CONN_TYPE_GPU":
			if c.trunk {
				continue
			}
		case "NMX_NVLINK_CONN_TYPE_SWITCH":
			if !c.trunk {
				continue
			}
		}
		switch f.Attr {
		case "NMX_NVLINK_CONN_ATTR_DISCOVERED", "NMX_NVLINK_CONN_ATTR_EXPECTED_ACTIVE":
			if !sn.up[c] {
				continue
			}
		case "NMX_NVLINK_CONN_ATTR_EXPECTED_INACTIVE":
			if sn.up[c] {
				continue
			}
		}
		if f.Loc != nil && !d.atLocation(f.Loc, c.a) && !d.atLocation(f.Loc, c.b) {
			continue
		}
		out = append(out, c)
		if f.Num > 0 && len(out) == int(f.Num) {
			break
		}
	}
	return out
}

func (d *domain) connInfo(sn *snapshot, c *conn) obj {
	typ, state := "NMX_NVLINK_CONN_TYPE_GPU", "NMX_NVLINK_CONN_STATE_INACTIVE"
	if c.trunk {
		typ = "NMX_NVLINK_CONN_TYPE_SWITCH"
	}
	if sn.up[c] {
		state = "NMX_NVLINK_CONN_STATE_ACTIVE"
	}
	return obj{"end_point_a": d.endpoint(c.a), "end_point_b": d.endpoint(c.b), "conn_type": typ, "conn_state": state}
}

// stamp is the time of the last connection change as NMX-C writes it (UTC, yyyy-mm-dd hh.mm.ss).
func (d *domain) stamp() string {
	d.mu.Lock()
	t := d.topoChanged
	d.mu.Unlock()
	return t.UTC().Format("2006-01-02 15.04.05")
}

func (c *call) connCount() (obj, error) {
	var f connFilter
	if err := c.decode(&f); err != nil {
		return nil, err
	}
	f.Num = 0
	d := c.s.d
	return obj{"num_conns": len(d.selectConns(d.observe(), f)), "time_stamp": d.stamp()}, nil
}

func (c *call) connInfoList() (obj, error) {
	var f connFilter
	if err := c.decode(&f); err != nil {
		return nil, err
	}
	d := c.s.d
	sn := d.observe()
	var list []obj
	for _, x := range d.selectConns(sn, f) {
		list = append(list, d.connInfo(sn, x))
	}
	return obj{"conn_info_list": obj{"conn_info_list": orEmpty(list)}, "time_stamp": d.stamp()}, nil
}

func (c *call) connInfoCombined() (obj, error) {
	return obj{"unexpected_conn_list": obj{"conn_info_list": []obj{}}}, nil
}

func (c *call) topologyInfo() (obj, error) {
	d := c.s.d
	return obj{"device_topo_info": d.topology(d.observe())}, nil
}

func (c *call) stateReport() (obj, error) {
	var req struct {
		Type enum `json:"report_type"`
	}
	if err := c.decode(&req); err != nil {
		return nil, err
	}
	d := c.s.d
	switch req.Type {
	case "NMX_REPORT_TYPE_TOPOLOGY":
		return obj{"device_topo_info": obj{"device_topo_info": d.topology(d.observe())}}, nil
	case "NMX_REPORT_TYPE_PARTITIONS":
		d.mu.Lock()
		defer d.mu.Unlock()
		var parts []obj
		for _, p := range d.tab.list() {
			var gpus []obj
			for _, u := range p.GPUs {
				e := obj{"device_uid": u}
				if g := d.gpuBy[u]; g != nil {
					e["location_info"] = d.nodeLocationInfo(g.node)
				}
				gpus = append(gpus, e)
			}
			parts = append(parts, obj{"partition_id": obj{"partition_id": p.ID}, "gpus": orEmpty(gpus)})
		}
		return obj{"partitions": obj{"partitions": orEmpty(parts)}}, nil
	}
	return obj{}, st("NMX_ST_BADPARAM", "report type %q is not a report NMX-C produces", req.Type)
}

// topology is every GPU and switch ASIC with its ports, as DeviceTopoInfo.
func (d *domain) topology(sn *snapshot) []obj {
	d.mu.Lock()
	all := map[uint32]bool{}
	member := map[uint64]uint32{}
	for _, p := range d.tab.list() {
		all[p.ID] = true
		for _, u := range p.GPUs {
			member[u] = p.ID
		}
	}
	d.mu.Unlock()
	peerOf := map[*model.Port]end{}
	for _, c := range d.conns {
		peerOf[c.pa], peerOf[c.pb] = c.b, c.a
	}
	port := func(p *model.Port, typ string, num uint32, uid uint64, parts []obj) obj {
		up := sn.upPort[p]
		po := obj{"port_type": typ, "port_uid": portUID(uid, num), "port_num": num, "physical_state": "NMX_PORT_PHYS_STATE_DISABLED",
			"logical_state": "NMX_LOGICAL_LINK_STATE_DOWN", "partition_id_list": parts, "port_admin_state": "NMX_ADMIN_STATE_UP"}
		if !p.AdminUp {
			po["port_admin_state"] = "NMX_ADMIN_STATE_DOWN"
		}
		if up {
			po["physical_state"], po["logical_state"] = "NMX_PORT_PHYS_STATE_LINKUP", "NMX_LOGICAL_LINK_STATE_ACTIVE"
		}
		if e, ok := peerOf[p]; ok {
			po["peer_port_device_uid"], po["peer_port_num"] = e.uid(), e.num
		}
		return po
	}
	var out []obj
	for _, a := range d.asics {
		var ports []obj
		for i, p := range a.ports {
			typ := "NMX_PORT_TYPE_SWITCH_ACCESS"
			if e, ok := peerOf[p]; ok && e.asic != nil {
				typ = "NMX_PORT_TYPE_SWITCH_TRUNK"
			}
			ports = append(ports, port(p, typ, uint32(i), a.uid, partitionIDs(all)))
		}
		out = append(out, obj{"switch_topo_info": obj{"loc": d.switchLocationInfo(a.sw), "device_uid": a.uid, "device_id": a.id, "num_ports": len(a.ports),
			"system_uid": nvl.SystemGUID(d.a.seed, a.sw.dev), "vendor_id": nvidiaVendor, "description": a.comp.Model, "partition_id": partitionIDs(all),
			"device_health": sn.asic[a], "port_topo_info": orEmpty(ports)}})
	}
	for _, g := range d.gpus {
		parts := []obj{}
		if id, ok := member[g.uid]; ok {
			parts = partitionIDs(map[uint32]bool{id: true})
		}
		var ports []obj
		for i, p := range g.lanes {
			ports = append(ports, port(p, "NMX_PORT_TYPE_GPU", uint32(i), g.uid, parts))
		}
		out = append(out, obj{"gpu_topo_info": obj{"loc": d.nodeLocationInfo(g.node), "device_uid": g.uid, "device_id": g.id, "num_ports": len(g.lanes),
			"system_uid": nvl.SystemGUID(d.a.seed, g.node.dev), "vendor_id": nvidiaVendor, "description": g.comp.Model, "partition_id": parts,
			"device_health": sn.gpu[g], "port_topo_info": orEmpty(ports)}})
	}
	return out
}

// portUID is a port's unique id: its device's GUID with the port number in the low byte.
func portUID(device uint64, num uint32) uint64 { return device&^0xff | uint64(num&0xff) }

package nmxc

import (
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	v1reflectiongrpc "google.golang.org/grpc/reflection/grpc_reflection_v1"
	v1alphareflectiongrpc "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func registerReflection(s *grpc.Server, opts reflection.ServerOptions) {
	v1reflectiongrpc.RegisterServerReflectionServer(s, reflection.NewServerV1(opts))
	v1alphareflectiongrpc.RegisterServerReflectionServer(s, reflection.NewServer(opts))
}

// subscriber is one Subscribe stream.
type subscriber struct {
	gateway string
	self    bool
	ch      chan obj // ServerNotification bodies
}

// stream serves Subscribe (the service's one server-streaming RPC).
func (s *trayServer) stream(m protoreflect.MethodDescriptor, ss grpc.ServerStream) (err error) {
	c := &call{s: s, method: m, peer: peerAddr(ss.Context())}
	defer func() { s.log(c, "event.subscribe", err, "NMX_ST_SUCCESS") }()
	if m.Name() != "Subscribe" || !m.IsStreamingServer() || m.IsStreamingClient() {
		return status.Errorf(codes.Unimplemented, "the dc-twin NMX-C does not implement %s", m.Name())
	}
	in := dynamicpb.NewMessage(m.Input())
	if err := ss.RecvMsg(in); err != nil {
		return err
	}
	c.in = in
	var req struct {
		Gateway   string `json:"gateway_id"`
		Self      bool   `json:"notify_on_self_change"`
		Heartbeat uint32 `json:"heart_beat_rate"`
	}
	if err := c.decode(&req); err != nil {
		return err
	}
	c.gateway = req.Gateway
	c.data = map[string]any{"heartbeatSeconds": req.Heartbeat, "notifyOnSelfChange": req.Self}
	d := s.d
	sub := &subscriber{gateway: req.Gateway, self: req.Self, ch: make(chan obj, 256)}
	d.mu.Lock()
	d.subs[sub] = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.subs, sub)
		d.mu.Unlock()
	}()
	send := func(n obj) error {
		msg, err := Build(m.Output(), n)
		if err != nil {
			s.a.rt.Engine.Incident(Name, "Subscribe: notification does not fit the interface definition: "+err.Error())
			return status.Errorf(codes.Internal, "dc-twin NMX-C: %v", err)
		}
		return ss.SendMsg(msg)
	}
	if err := send(obj{"subscription_response": obj{"server_header": s.header("NMX_ST_SUCCESS")}}); err != nil {
		return err
	}
	var beat <-chan time.Time
	if req.Heartbeat > 0 {
		t := time.NewTicker(time.Duration(req.Heartbeat) * time.Second)
		defer t.Stop()
		beat = t.C
	}
	for {
		select {
		case <-ss.Context().Done():
			return nil
		case n := <-sub.ch:
			if err := send(n); err != nil {
				return err
			}
		case <-beat:
			info := d.stateInfo(d.observe())
			info["server_header"] = s.header("NMX_ST_SUCCESS")
			if err := send(obj{"domain_state_info": info}); err != nil {
				return err
			}
		}
	}
}

// publish delivers a notification to every subscriber it is for; one that cannot keep up loses
// it rather than holding the controller up. skip is the requesting client's gateway, whose own
// change it is told of only with notify_on_self_change.
func (d *domain) publish(field string, body obj, skip string, skipSet bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for sub := range d.subs {
		if skipSet && sub.gateway == skip && !sub.self {
			continue
		}
		n := obj{field: body}
		select {
		case sub.ch <- n:
		default:
		}
	}
}

// header is the server header a notification carries, from the domain's manager tray.
func (d *domain) header() obj {
	return obj{"domain_uuid": d.uuid, "app_uuid": d.app, "app_ver": appVersion, "return_code": "NMX_ST_SUCCESS"}
}

// partitionChanged tells the other subscribers of a partition write.
func (d *domain) partitionChanged(c *call, field string, body obj) {
	n := obj{}
	for k, v := range body {
		n[k] = v
	}
	n["server_header"] = c.s.header("NMX_ST_SUCCESS")
	d.publish(field, n, c.gateway, true)
	d.a.kick(d)
}

// changed compares a new snapshot with the last and notifies what changed.
func (d *domain) changed(sn *snapshot) {
	d.mu.Lock()
	last := d.last
	d.last = sn
	health, topo, plane := false, false, last.state != sn.state
	for g, h := range sn.gpu {
		health = health || last.gpu[g] != h
	}
	for n, h := range sn.node {
		health = health || last.node[n] != h
	}
	for a, h := range sn.asic {
		health = health || last.asic[a] != h
	}
	for w, h := range sn.sw {
		health = health || last.sw[w] != h
	}
	for c, up := range sn.up {
		topo = topo || last.up[c] != up
	}
	for dev, on := range sn.powered {
		topo = topo || last.powered[dev] != on
	}
	if topo {
		d.topoChanged = sn.at
	}
	var parts []uint32
	for _, p := range d.tab.list() {
		if last.partitionHealth(d.shape, p) != sn.partitionHealth(d.shape, p) {
			parts = append(parts, p.ID)
		}
	}
	d.mu.Unlock()
	ctx := obj{"context": ""}
	if health {
		d.publish("health_state_changed", obj{"server_header": d.header()}, "", false)
	}
	if topo {
		d.publish("fm_event", obj{"server_header": d.header(), "context": ctx, "fm_event_topology_change": obj{"context": ctx}}, "", false)
	}
	if plane {
		d.publish("fm_event", obj{"server_header": d.header(), "context": ctx, "fm_event_control_plane_state_change": obj{"context": ctx}}, "", false)
	}
	for _, id := range parts {
		d.publish("fm_event", obj{"server_header": d.header(), "context": ctx, "fm_event_partition_change": obj{"context": ctx, "partition_id": obj{"partition_id": id}}}, "", false)
	}
}

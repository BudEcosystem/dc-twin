package nmxc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/BudEcosystem/dc-twin/internal/adapter/nvl"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const labEstate = "../../../examples/estates/lab-model.yaml"

var actor = schema.EventSource{Class: "scenario", Principal: "nmxc-test"}

// protoOrSkip compiles the obtained interface definition, or skips: it is NVIDIA proprietary and
// is not in the repository.
func protoOrSkip(t *testing.T, reg *pack.Registry) *Protoset {
	t.Helper()
	ps, err := LoadProto(reg)
	if err != nil {
		t.Skipf("skipped: NMX-C's interface definition (nmx_c.proto, NVIDIA proprietary) is not obtained here: %v", err)
	}
	return ps
}

// client is a dynamic NMX-C client: requests and responses are messages of the compiled
// definition, written and read by their proto field names.
type client struct {
	t  *testing.T
	ps *Protoset
	cc *grpc.ClientConn
}

func dial(t *testing.T, ps *Protoset, tw *twin.Twin, ep Endpoint) *client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(tw.Net.CAPEM)
	cc, err := grpc.NewClient(strings.TrimPrefix(ep.URL, "https://"), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: ep.Authority})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	return &client{t: t, ps: ps, cc: cc}
}

func (c *client) try(method string, req obj) (map[string]any, error) {
	m := c.ps.Method(method)
	if m == nil {
		c.t.Fatalf("no RPC %s", method)
	}
	in, err := Build(m.Input(), req)
	if err != nil {
		c.t.Fatal(err)
	}
	out := dynamicpb.NewMessage(m.Output())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.cc.Invoke(ctx, "/"+ServiceName+"/"+method, in, out); err != nil {
		return nil, err
	}
	b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(out)
	var v map[string]any
	return v, json.Unmarshal(b, &v)
}

func (c *client) call(method string, req obj) map[string]any {
	c.t.Helper()
	v, err := c.try(method, req)
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	return v
}

func rc(v map[string]any) string {
	h, _ := v["server_header"].(map[string]any)
	s, _ := h["return_code"].(string)
	return s
}

func list(v map[string]any, k string) []any {
	l, _ := v[k].([]any)
	return l
}

func upLab(t *testing.T, state string) (*twin.Twin, *Protoset) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	ps := protoOrSkip(t, reg)
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{Name}, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	return tw, ps
}

func endpoint(t *testing.T, a *Adapter, device string) Endpoint {
	t.Helper()
	for _, e := range a.Endpoints() {
		if e.Device == device {
			return e
		}
	}
	t.Fatalf("no NMX-C on %s", device)
	return Endpoint{}
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNMXCOnTheLab(t *testing.T) {
	state := t.TempDir()
	tw, ps := upLab(t, state)
	defer tw.Down()
	a := tw.Adapter(Name).(*Adapter)
	if r := a.Report(); r.Served != 18 {
		t.Fatalf("served %d trays, want 18 (two NVL72 racks)", r.Served)
	}
	const rack = "site-1/hall-a/a01"
	var dom *model.Domain
	var seed int64
	tw.Engine.Read(func(m *model.Model) {
		seed = m.Seed
		for _, d := range m.Domains {
			if d.Path == rack+"/domain/scale-up-nvlink" {
				dom = d
			}
		}
	})
	c := dial(t, ps, tw, endpoint(t, a, rack+"/switch-tray-01"))
	gw := obj{"gateway_id": "nmxc-test"}

	t.Run("hello and domain", func(t *testing.T) {
		h := c.call("Hello", obj{"gateway_id": "nmxc-test", "major_version": "PROTO_MSG_MAJOR_VERSION", "minor_version": "PROTO_MSG_MINOR_VERSION"})
		hdr := h["server_header"].(map[string]any)
		if rc(h) != "NMX_ST_SUCCESS" || hdr["domain_uuid"] != nvl.DomainUUID(seed, dom) {
			t.Fatalf("hello: %v", h)
		}
		p := c.call("GetDomainProperties", gw)
		if p["max_compute_nodes"] != float64(18) || p["max_gpus_per_compute_node"] != float64(4) || p["max_switch_nodes"] != float64(9) || p["max_switches_per_switch_node"] != float64(2) || p["max_gpu_nv_links"] != float64(18) {
			t.Fatalf("properties: %v", p)
		}
		s := c.call("GetDomainStateInfo", gw)
		if s["control_plane_state"] != "NMX_CONTROL_PLANE_STATE_CONFIGURED" || s["nmx_controller_health"] != "NMX_CONTROLLER_HEALTH_HEALTHY" {
			t.Fatalf("state: %v", s)
		}
		if n := c.call("GetComputeNodeCount", obj{"attr": "NMX_COMPUTE_NODE_ATTR_ALL"}); n["num_nodes"] != float64(18) {
			t.Fatalf("compute nodes: %v", n)
		}
		if n := c.call("GetSwitchNodeCount", obj{"attr": "NMX_SWITCH_NODE_ATTR_ALL"}); n["num_nodes"] != float64(9) {
			t.Fatalf("switch nodes: %v", n)
		}
		if l := list(c.call("GetSwitchInfoList", gw), "switch_info_list"); len(l) != 18 {
			t.Fatalf("switches: %d", len(l))
		}
		conns := c.call("GetConnCount", obj{"conn_type": "NMX_NVLINK_CONN_TYPE_ALL", "conn_attr": "NMX_NVLINK_CONN_ATTR_EXPECTED_ACTIVE"})
		if conns["num_conns"] != float64(72*18) {
			t.Fatalf("active lanes: %v", conns)
		}
		if _, err := c.try("GetStaticConfig", gw); err == nil || !strings.Contains(err.Error(), "Unimplemented") {
			t.Fatalf("an unserved RPC: %v", err)
		}
	})

	gpus := list(c.call("GetGpuInfoList", obj{"attr": "NMX_GPU_ATTR_ALL", "gateway_id": "nmxc-test"}), "gpu_info_list")
	t.Run("GPUs", func(t *testing.T) {
		if len(gpus) != 72 {
			t.Fatalf("%d GPUs, want 72", len(gpus))
		}
		seen := map[string]bool{}
		for _, g := range gpus {
			g := g.(map[string]any)
			uid := fmt.Sprint(g["gpu_uid"])
			if seen[uid] || g["gpu_health"] != "NMX_GPU_HEALTH_HEALTHY" || g["partition_id"].(map[string]any)["partition_id"] != float64(DefaultPartitionID) {
				t.Fatalf("GPU %v", g)
			}
			seen[uid] = true
		}
	})
	uid := func(i int) string { return fmt.Sprint(gpus[i].(map[string]any)["gpu_uid"]) }

	t.Run("partitions", func(t *testing.T) {
		if n := c.call("GetPartitionCount", obj{"info_attr": "NMX_PARTITION_INFO_ATTR_ALL"}); n["num_partitions"] != float64(1) {
			t.Fatalf("factory partitions: %v", n)
		}
		if r := c.call("DeletePartition", obj{"partition_id": obj{"partition_id": DefaultPartitionID}}); rc(r) != "NMX_ST_SUCCESS" {
			t.Fatalf("delete factory partition: %v", r)
		}
		var ids []obj
		for i := 0; i < 8; i++ {
			ids = append(ids, obj{"gpu_uid": uid(i)})
		}
		r := c.call("CreatePartition", obj{"name": "tenant-a", "gpu_resource_id": ids, "gateway_id": "nmxc-test"})
		if rc(r) != "NMX_ST_SUCCESS" || r["partition_id"].(map[string]any)["partition_id"] != float64(1) {
			t.Fatalf("create: %v", r)
		}
		if r := c.call("CreatePartition", obj{"name": "tenant-b", "gpu_resource_id": []obj{{"gpu_uid": uid(3)}}}); rc(r) != "NMX_ST_RESOURCE_IN_USE" {
			t.Fatalf("a GPU in two partitions: %v", r)
		}
		if r := c.call("CreatePartition", obj{"name": "tenant-a", "gpu_resource_id": []obj{{"gpu_uid": uid(20)}}}); rc(r) != "NMX_ST_PARTITION_NAME_IN_USE" {
			t.Fatalf("a duplicate name: %v", r)
		}
		loc := gpus[8].(map[string]any)["loc"].(map[string]any)["location"]
		r = c.call("AddGpusToPartition", obj{"name": "tenant-a", "location_list": []obj{{"loc": loc, "gpu_id": gpus[8].(map[string]any)["gpu_id"]}}})
		if rc(r) != "NMX_ST_SUCCESS" {
			t.Fatalf("add by location: %v", r)
		}
		info := list(c.call("GetPartitionInfoList", obj{"partition_name_list": []string{"tenant-a"}}), "partition_info_list")
		if len(info) != 1 || len(list(info[0].(map[string]any), "gpu_uid_list")) != 9 || info[0].(map[string]any)["health"] != "NMX_PARTITION_HEALTH_HEALTHY" {
			t.Fatalf("info: %v", info)
		}
		if r := c.call("RemoveGpusFromPartition", obj{"partition_id": obj{"partition_id": 1}, "gpu_uid": []string{uid(0), uid(0)}}); rc(r) != "NMX_ST_SUCCESS" {
			t.Fatalf("remove: %v", r)
		}
		if g := list(c.call("GetGpuInfoList", obj{"attr": "NMX_GPU_ATTR_PARTITION_ID", "partition_id": obj{"partition_id": 1}}), "gpu_info_list"); len(g) != 8 {
			t.Fatalf("GPUs of partition 1: %d", len(g))
		}
		if r := c.call("GetGpuInfoList", obj{"attr": "NMX_GPU_ATTR_PARTITION_ID", "partition_id": obj{"partition_id": 9}}); rc(r) != "NMX_ST_PARTITION_ID_NOT_IN_USE" {
			t.Fatalf("an unprovisioned partition: %v", r)
		}
		if n := c.call("GetComputeNodeCount", obj{"attr": "NMX_COMPUTE_NODE_ATTR_FREE"}); n["num_nodes"] != float64(15) {
			t.Fatalf("free compute nodes: %v", n)
		}
	})

	t.Run("subscribe", func(t *testing.T) {
		m := ps.Method("Subscribe")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		st, err := c.cc.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/"+ServiceName+"/Subscribe")
		if err != nil {
			t.Fatal(err)
		}
		in, _ := Build(m.Input(), obj{"gateway_id": "watcher", "heart_beat_rate": 1})
		if err := st.SendMsg(in); err != nil {
			t.Fatal(err)
		}
		st.CloseSend()
		got := make(chan map[string]any, 64)
		go func() {
			for {
				out := dynamicpb.NewMessage(m.Output())
				if err := st.RecvMsg(out); err != nil {
					close(got)
					return
				}
				b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(out)
				var v map[string]any
				_ = json.Unmarshal(b, &v)
				got <- v
			}
		}()
		next := func(want string) map[string]any {
			t.Helper()
			timeout := time.After(10 * time.Second)
			for {
				select {
				case v, ok := <-got:
					if !ok {
						t.Fatalf("stream ended waiting for %s", want)
					}
					for k := range v {
						if k == want || strings.Contains(fmt.Sprint(v[k]), want) {
							return v
						}
					}
				case <-timeout:
					t.Fatalf("no %s notification", want)
				}
			}
		}
		next("subscription_response")
		next("domain_state_info") // the heartbeat asked for
		if _, err := tw.Engine.Inject(rack+"/switch-tray-03", "nvidia/nvlink/switch-tray-failure", nil, "", actor); err != nil {
			t.Fatal(err)
		}
		next("health_state_changed")
		next("fm_event_topology_change")
		next("fm_event_control_plane_state_change")
		s := c.call("GetDomainStateInfo", gw)
		if s["control_plane_state"] != "NMX_CONTROL_PLANE_STATE_ERROR" {
			t.Fatalf("after a switch-tray failure: %v", s)
		}
		for _, g := range list(c.call("GetGpuInfoList", obj{"attr": "NMX_GPU_ATTR_ALL"}), "gpu_info_list") {
			if g.(map[string]any)["gpu_health"] != "NMX_GPU_HEALTH_DEGRADED" {
				t.Fatalf("a GPU with lanes to the failed tray: %v", g)
			}
		}
		if n := c.call("GetConnCount", obj{"conn_type": "NMX_NVLINK_CONN_TYPE_GPU", "conn_attr": "NMX_NVLINK_CONN_ATTR_EXPECTED_INACTIVE"}); n["num_conns"] != float64(72*2) {
			t.Fatalf("lanes down: %v", n)
		}
		if _, err := tw.Engine.Clear(rack+"/switch-tray-03", "nvidia/nvlink/switch-tray-failure", actor); err != nil {
			t.Fatal(err)
		}
		next("fm_event_control_plane_state_change")
		if _, err := tw.Engine.Inject(rack+"/compute-tray-07/gpu-2", "nvidia/xid/xid-79", nil, "", actor); err != nil {
			t.Fatal(err)
		}
		next("health_state_changed")
		bad := 0
		for _, g := range list(c.call("GetGpuInfoList", obj{"attr": "NMX_GPU_ATTR_ALL"}), "gpu_info_list") {
			if g.(map[string]any)["gpu_health"] == "NMX_GPU_HEALTH_NO_NVLINK" {
				bad++
			}
		}
		if bad != 1 {
			t.Fatalf("%d GPUs without NVLink after one Xid 79", bad)
		}
		// another client's partition change reaches this subscriber
		other := dial(t, ps, tw, endpoint(t, a, rack+"/switch-tray-05"))
		if r := other.call("CreatePartition", obj{"name": "tenant-c", "gpu_resource_id": []obj{{"gpu_uid": uid(40)}}, "gateway_id": "carbide"}); rc(r) != "NMX_ST_SUCCESS" {
			t.Fatalf("create from another tray: %v", r)
		}
		next("create_partition_response")
	})

	t.Run("an unpowered tray answers nothing", func(t *testing.T) {
		tray := rack + "/switch-tray-09"
		if err := tw.Engine.Power(engine.Request{Path: tray, Canonical: "power.off.force", Source: actor}); err != nil {
			t.Fatal(err)
		}
		var d *model.Device
		tw.Engine.Read(func(m *model.Model) { d = m.DeviceBy[tray] })
		waitFor(t, "the tray to power off", func() bool {
			off := false
			tw.Engine.Read(func(*model.Model) { off = d.State.Power == model.Off })
			return off
		})
		dead := dial(t, ps, tw, endpoint(t, a, tray))
		if _, err := dead.try("Hello", gw); err == nil {
			t.Fatal("an unpowered tray answered")
		}
		if h := c.call("Hello", gw); rc(h) != "NMX_ST_SUCCESS" {
			t.Fatalf("another tray: %v", h)
		}
	})

	t.Run("requests are logged", func(t *testing.T) {
		n := 0
		for _, ev := range tw.Engine.Events(0, nil) {
			if ev.Request != nil && ev.Request.Protocol == Protocol && ev.Request.Method == "CreatePartition" {
				n++
			}
		}
		if n < 3 {
			t.Fatalf("%d CreatePartition requests logged", n)
		}
	})

	// partitions are the controller's database: they survive the twin going down and up
	tw.Down()
	tw2, _ := upLab(t, state)
	defer tw2.Down()
	c2 := dial(t, ps, tw2, endpoint(t, tw2.Adapter(Name).(*Adapter), rack+"/switch-tray-02"))
	names := map[string]bool{}
	for _, p := range list(c2.call("GetPartitionInfoList", gw), "partition_info_list") {
		names[p.(map[string]any)["name"].(string)] = true
	}
	if !names["tenant-a"] || !names["tenant-c"] || names[DefaultPartitionName] {
		t.Fatalf("partitions after down and up: %v", names)
	}
}

func TestRefusesWithoutTheDefinition(t *testing.T) {
	t.Setenv("DCTWIN_ASSETS", t.TempDir())
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	_, err = twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{Name}, StateDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "dctwin assets fetch") {
		t.Fatalf("up without nmx_c.proto: %v", err)
	}
}

func TestPartitionTable(t *testing.T) {
	tab := newTable([]uint64{0x10, 0x11, 0x12, 0x13}, false)
	if _, err := tab.create("taken", []uint64{0x13}, 7, "", "", 0); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		uids []uint64
		id   uint32
		want string
	}{
		{"p", []uint64{0x10, 0x10}, 0, "NMX_ST_SUCCESS"},
		{"q", []uint64{0x99}, 0, "NMX_ST_BADPARAM"},
		{"r", []uint64{0x13}, 0, "NMX_ST_RESOURCE_IN_USE"},
		{"taken", []uint64{0x11}, 0, "NMX_ST_PARTITION_NAME_IN_USE"},
		{"s", []uint64{0x11}, 7, "NMX_ST_PARTITION_ID_IN_USE"},
		{"", []uint64{0x11}, 0, "NMX_ST_SUCCESS"},
	}
	for _, c := range cases {
		_, err := tab.create(c.name, c.uids, c.id, "", "", 0)
		if got := returnCode(err); got != c.want {
			t.Errorf("create(%q, %x, %d) = %s, want %s", c.name, c.uids, c.id, got, c.want)
		}
	}
	if p := tab.Partitions[1]; p == nil || p.Name != "p" || len(p.GPUs) != 1 {
		t.Fatalf("ids are allocated from 1 and GPUs de-duplicated: %+v", p)
	}
	if p := tab.Partitions[2]; p == nil || p.Name != "partition-2" {
		t.Fatalf("an unnamed partition: %+v", p)
	}
	if _, err := tab.remove(ref{id: 42}); err != nil {
		t.Fatalf("deleting an id that is not provisioned succeeds: %v", err)
	}
	if _, err := tab.remove(ref{name: "nope"}); returnCode(err) != "NMX_ST_PARTITION_NAME_NOT_IN_USE" {
		t.Fatalf("deleting an unknown name: %v", err)
	}
	if _, err := tab.removeGPUs(ref{id: 7}, []uint64{0x99}); err != nil {
		t.Fatalf("removing a GPU that is not a member: %v", err)
	}
}

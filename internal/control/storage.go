package control

import (
	"fmt"
	"net/http"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// storageOp is `dctwin storage create|delete|map|unmap|register|fill|offline|online`: an
// operator's request to a storage system, as its management API would make it, or an act on it.
func (s *Server) storageOp(w http.ResponseWriter, r *http.Request, body map[string]any, user schema.EventSource) {
	eng := s.T.Engine
	str := func(k string) string { v, _ := body[k].(string); return v }
	num := func(k string) (int64, bool) {
		v, ok := body[k].(float64)
		return int64(v), ok
	}
	sys := str("system")
	req := engine.Request{Path: sys, Canonical: "storage." + str("op"), Source: user, Protocol: "control", Method: "POST", Target: "/v1/storage"}
	var out any
	var err error
	switch str("op") {
	case "create":
		size, _ := num("size")
		switch str("kind") {
		case "filesystem":
			var ex []model.Export
			if xs, ok := body["exports"].([]any); ok {
				for _, x := range xs {
					m, _ := x.(map[string]any)
					c, _ := m["clients"].(string)
					a, _ := m["access"].(string)
					rs, _ := m["rootSquash"].(bool)
					ex = append(ex, model.Export{Clients: c, Access: a, RootSquash: rs})
				}
			}
			out, err = eng.CreateFileSystem(req, engine.FileSystemSpec{Name: str("name"), QuotaBytes: size, Exports: ex})
		case "volume":
			out, err = eng.CreateStorageVolume(req, engine.VolumeCreate{Name: str("name"), CapacityBytes: size})
		case "bucket":
			out, err = eng.CreateBucket(req, str("name"), size)
		case "snapshot":
			out, err = eng.CreateSnapshot(req, str("of"), str("name"))
		default:
			err = fmt.Errorf("create filesystem|volume|bucket|snapshot, not %q", str("kind"))
		}
	case "delete":
		err = eng.DeleteStorageObject(req, str("kind"), str("name"))
		out = map[string]any{"deleted": str("name")}
	case "map", "unmap":
		out, err = eng.MapVolume(req, str("name"), str("host"), str("op") == "unmap")
	case "register", "unregister":
		err = eng.RegisterHost(req, str("host"), str("op") == "unregister")
		out = map[string]any{"host": str("host")}
	case "fill", "offline", "online":
		a := schema.StorageAct{}
		switch str("op") {
		case "fill":
			a.Fill = str("name")
			if n, ok := num("usedBytes"); ok {
				a.UsedBytes = &n
			}
			if p, ok := body["usedPercent"].(float64); ok {
				a.UsedPercent = &p
			}
		case "offline":
			a.Offline = str("name")
		case "online":
			a.Online = str("name")
		}
		err = eng.StorageAct(sys, a, user)
		out = map[string]any{str("op"): str("name")}
	default:
		err = fmt.Errorf("storage op %q: create, delete, map, unmap, register, unregister, fill, offline or online", str("op"))
	}
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	writeJSON(w, out)
}

// emulationOp is `dctwin dpu emulation|attach|detach`: a DPU's NVMe emulation (P5).
func (s *Server) emulationOp(w http.ResponseWriter, body map[string]any, user schema.EventSource) {
	eng := s.T.Engine
	str := func(k string) string { v, _ := body[k].(string); return v }
	ds, err := eng.SelectDevices(str("device"))
	if err != nil || len(ds) != 1 {
		jsonErr(w, 400, fmt.Sprintf("device %q names %d devices, not one", str("device"), len(ds)))
		return
	}
	req := engine.Request{Path: ds[0].Path, Canonical: "dpu.emulation." + str("op"), Source: user, Protocol: "control", Method: "POST", Target: "/v1/dpu/emulation"}
	var out any
	switch str("op") {
	case "on", "off":
		err = eng.SetNVMeEmulation(req, str("op") == "on")
		out = map[string]any{"device": ds[0].Path, "emulation": str("op")}
	case "attach":
		out, err = eng.AttachEmulatedVolume(req, str("system"), str("volume"))
	case "detach":
		id, _ := body["id"].(float64)
		err = eng.DetachEmulatedVolume(req, int(id))
		out = map[string]any{"detached": int(id)}
	default:
		err = fmt.Errorf("op %q: on, off, attach or detach", str("op"))
	}
	if err != nil {
		jsonErr(w, 400, err.Error())
		return
	}
	writeJSON(w, out)
}

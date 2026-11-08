package storage

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// The Swordfish service of one storage system (S-Q3): SNIA Swordfish 1.2.9's resources over the
// Redfish protocol, at the URIs the schemas allow. The system is the Storage resource
// /redfish/v1/Storage/<name>, its nodes its StorageControllers, its pool a StoragePool, its file
// systems FileSystems whose export rules are their ExportedFileShares, its volumes Volumes that
// NVMe-oF Connections map to host Endpoints; its nodes and enclosures are Chassis holding the pool
// drives. Every write is an engine request and completes at once.

const (
	root   = "/redfish/v1"
	fabric = root + "/Fabrics/NVMeoF"
)

func ref(p string) map[string]any { return map[string]any{"@odata.id": p} }

func odata(path, typ string) map[string]any {
	return map[string]any{"@odata.id": path, "@odata.type": typ}
}

func collection(path, typ, name string, members []string) map[string]any {
	ms := make([]any, 0, len(members))
	for _, m := range members {
		ms = append(ms, ref(m))
	}
	d := odata(path, typ)
	d["Name"], d["Members"], d["Members@odata.count"] = name, ms, len(ms)
	return d
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("OData-Version", "4.0")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, id, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": id, "message": msg,
		"@Message.ExtendedInfo": []any{map[string]any{"@odata.type": "#Message.v1_1_1.Message", "MessageId": id, "Message": msg, "Severity": "Critical", "Resolution": "None."}},
	}})
}

// engineError answers an engine refusal with Redfish's status and message.
func engineError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, engine.ErrNoStorageObject), errors.Is(err, engine.ErrNoStorageSystem):
		writeError(w, http.StatusNotFound, "Base.1.18.ResourceNotFound", err.Error())
	case errors.Is(err, engine.ErrExists):
		writeError(w, http.StatusConflict, "Base.1.18.ResourceAlreadyExists", err.Error())
	case errors.Is(err, engine.ErrCapacity):
		writeError(w, http.StatusBadRequest, "Base.1.18.InsufficientStorage", err.Error())
	case errors.Is(err, engine.ErrUnsupported):
		writeError(w, http.StatusBadRequest, "Base.1.18.ActionNotSupported", err.Error())
	case errors.Is(err, engine.ErrPrecondition):
		if strings.Contains(err.Error(), "is up") {
			writeError(w, http.StatusServiceUnavailable, "Base.1.18.ServiceTemporarilyUnavailable", err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "Base.1.18.PropertyValueError", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "Base.1.18.InternalError", err.Error())
	}
}

// ServeHTTP is the system's Swordfish service.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	w.Header().Set("Cache-Control", "no-cache")
	if s.manager == "vast/vms" {
		s.serveVMS(w, r) // vms.go
		return
	}
	// the service root, its metadata and the OData service document need no authentication
	open := path == "/redfish" || path == root || path == root+"/$metadata" || path == root+"/odata"
	user, ok := s.authenticate(r)
	if !open && !(path == root+"/SessionService/Sessions" && r.Method == http.MethodPost) && !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="dc-twin storage"`)
		writeError(w, http.StatusUnauthorized, "Base.1.18.NoValidSession", "Authentication is required.")
		return
	}
	src := schema.EventSource{Class: "stack", Principal: user, Address: r.RemoteAddr}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.get(w, r, path)
	case http.MethodPost:
		s.post(w, r, path, src)
	case http.MethodPatch:
		s.patch(w, r, path, src)
	case http.MethodDelete:
		s.delete(w, r, path, src)
	default:
		writeError(w, http.StatusMethodNotAllowed, "Base.1.18.OperationNotAllowed", r.Method+" is not allowed here.")
	}
}

func (s *server) authenticate(r *http.Request) (string, bool) {
	if tok := r.Header.Get("X-Auth-Token"); tok != "" {
		s.mu.Lock()
		_, ok := s.sessions[tok]
		s.mu.Unlock()
		return s.user, ok
	}
	u, p, ok := r.BasicAuth()
	if !ok {
		return "", false
	}
	good := subtle.ConstantTimeCompare([]byte(u), []byte(s.user)) == 1 && subtle.ConstantTimeCompare([]byte(p), []byte(s.password)) == 1
	return u, good
}

func (s *server) req(r *http.Request, canonical string, src schema.EventSource) engine.Request {
	return engine.Request{Path: s.path, Canonical: canonical, Source: src, Protocol: "redfish", Method: r.Method, Target: r.URL.Path}
}

func (s *server) view() engine.StorageSystemView {
	v, _ := s.a.rt.Engine.StorageSystem(s.path)
	return v
}

func (s *server) sysURI() string { return root + "/Storage/" + s.name }

// ---------------------------------------------------------------- GET

func (s *server) get(w http.ResponseWriter, r *http.Request, path string) {
	if path == root+"/$metadata" {
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("OData-Version", "4.0")
		io.WriteString(w, metadata)
		return
	}
	v := s.view()
	doc, allow := s.render(v, path)
	if doc == nil {
		writeError(w, http.StatusNotFound, "Base.1.18.ResourceNotFound", "The requested resource of type unknown named "+path+" was not found.")
		return
	}
	if allow != "" {
		w.Header().Set("Allow", allow)
	}
	sum := sha256.Sum256([]byte(fmt.Sprint(doc)))
	w.Header().Set("ETag", `W/"`+hex.EncodeToString(sum[:8])+`"`)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func status(state, health string) map[string]any {
	return map[string]any{"State": state, "Health": health, "HealthRollup": health}
}

func objState(o engine.StorageObjectView) string {
	if o.Online {
		return "Enabled"
	}
	return "UnavailableOffline"
}

func chassisID(d string) string {
	parts := strings.Split(d, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "-" + parts[len(parts)-1]
	}
	return d
}

func hostID(nqn string) string {
	sum := sha256.Sum256([]byte(nqn))
	return "host-" + hex.EncodeToString(sum[:4])
}

// render returns the document at path and its Allow header.
func (s *server) render(v engine.StorageSystemView, path string) (map[string]any, string) {
	sys := s.sysURI()
	switch path {
	case "/redfish":
		return map[string]any{"v1": root + "/"}, "GET, HEAD"
	case root:
		d := odata(root, "#ServiceRoot.v1_22_0.ServiceRoot")
		d["Id"], d["Name"], d["RedfishVersion"], d["Vendor"] = "RootService", "dc-twin storage system "+s.name, "1.22.0", "dc-twin"
		d["UUID"] = uuidOf(s.path)
		d["Storage"], d["Fabrics"], d["Chassis"], d["SessionService"] = ref(root+"/Storage"), ref(root+"/Fabrics"), ref(root+"/Chassis"), ref(root+"/SessionService")
		d["Systems"] = ref(root + "/Systems")
		d["Links"] = map[string]any{"Sessions": ref(root + "/SessionService/Sessions")}
		return d, "GET, HEAD"
	case root + "/odata":
		var vals []any
		for _, n := range []string{"Storage", "Systems", "Fabrics", "Chassis", "SessionService"} {
			vals = append(vals, map[string]any{"name": n, "kind": "Singleton", "url": root + "/" + n})
		}
		return map[string]any{"@odata.context": root + "/$metadata", "value": vals}, "GET, HEAD"
	case root + "/SessionService":
		d := odata(path, "#SessionService.v1_1_9.SessionService")
		d["Id"], d["Name"], d["ServiceEnabled"], d["SessionTimeout"], d["Sessions"] = "SessionService", "Session Service", true, 1800, ref(path+"/Sessions")
		return d, "GET, HEAD"
	case root + "/SessionService/Sessions":
		s.mu.Lock()
		var ids []string
		for _, id := range s.sessions {
			ids = append(ids, path+"/"+id)
		}
		s.mu.Unlock()
		sort.Strings(ids)
		return collection(path, "#SessionCollection.SessionCollection", "Sessions", ids), "GET, HEAD, POST"
	case root + "/Storage":
		return collection(path, "#StorageCollection.StorageCollection", "Storage Systems", []string{sys}), "GET, HEAD"
	case root + "/Systems":
		return collection(path, "#ComputerSystemCollection.ComputerSystemCollection", "Computer Systems", []string{root + "/Systems/" + s.name}), "GET, HEAD"
	case root + "/Systems/" + s.name:
		// the system as Swordfish's discovery finds a storage server: a ComputerSystem whose
		// hosting role says so, on the chassis of its nodes and enclosures
		d := odata(path, "#ComputerSystem.v1_20_0.ComputerSystem")
		d["Id"], d["Name"], d["SystemType"], d["HostingRoles"] = s.name, s.name, "Physical", []any{"StorageServer"}
		d["Manufacturer"], d["Model"], d["UUID"] = "dc-twin", v.Template, uuidOf(s.path)
		d["PowerState"] = "On"
		if v.State == "Offline" {
			d["PowerState"] = "Off"
		}
		d["Status"] = status(stateOf(v), v.Health)
		var ch []any
		for _, n := range append(append([]engine.StorageNodeView{}, v.Nodes...), v.Enclosures...) {
			ch = append(ch, ref(root+"/Chassis/"+chassisID(n.Path)))
		}
		d["Links"] = map[string]any{"Chassis": ch}
		return d, "GET, HEAD"
	case sys:
		return s.storageDoc(v), "GET, HEAD"
	case sys + "/Controllers":
		var ids []string
		for _, n := range v.Nodes {
			ids = append(ids, path+"/"+chassisID(n.Path))
		}
		return collection(path, "#StorageControllerCollection.StorageControllerCollection", "Storage Nodes", ids), "GET, HEAD"
	case sys + "/StoragePools":
		return collection(path, "#StoragePoolCollection.StoragePoolCollection", "Storage Pools", []string{path + "/pool"}), "GET, HEAD"
	case sys + "/StoragePools/pool":
		return s.poolDoc(v), "GET, HEAD"
	case sys + "/StoragePools/pool/AllocatedVolumes", sys + "/Volumes":
		var ids []string
		for _, o := range v.Volumes {
			ids = append(ids, sys+"/Volumes/"+o.ID)
		}
		// a volume is made in the system's collection or in its pool's, alike
		return collection(path, "#VolumeCollection.VolumeCollection", "Volumes", ids), "GET, HEAD, POST"
	case sys + "/FileSystems":
		var ids []string
		for _, o := range v.FileSystems {
			ids = append(ids, path+"/"+o.ID)
		}
		return collection(path, "#FileSystemCollection.FileSystemCollection", "File Systems", ids), "GET, HEAD, POST"
	case root + "/Fabrics":
		return collection(path, "#FabricCollection.FabricCollection", "Fabrics", []string{fabric}), "GET, HEAD"
	case fabric:
		d := odata(path, "#Fabric.v1_4_0.Fabric")
		d["Id"], d["Name"], d["FabricType"] = "NVMeoF", "NVMe over Fabrics (TCP)", "NVMeOverFabrics"
		d["Status"] = status(stateOf(v), v.Health)
		d["Endpoints"], d["Connections"] = ref(path+"/Endpoints"), ref(path+"/Connections")
		return d, "GET, HEAD"
	case fabric + "/Endpoints":
		var ids []string
		for _, n := range v.Nodes {
			ids = append(ids, path+"/target-"+chassisID(n.Path))
		}
		for _, h := range v.Hosts {
			ids = append(ids, path+"/"+hostID(h))
		}
		return collection(path, "#EndpointCollection.EndpointCollection", "NVMe-oF Endpoints", ids), "GET, HEAD, POST"
	case fabric + "/Connections":
		var ids []string
		for _, o := range v.Volumes {
			if len(o.Hosts) > 0 {
				ids = append(ids, path+"/"+o.ID)
			}
		}
		return collection(path, "#ConnectionCollection.ConnectionCollection", "Volume Connections", ids), "GET, HEAD, POST"
	case root + "/Chassis":
		var ids []string
		for _, n := range append(append([]engine.StorageNodeView{}, v.Nodes...), v.Enclosures...) {
			ids = append(ids, path+"/"+chassisID(n.Path))
		}
		return collection(path, "#ChassisCollection.ChassisCollection", "Chassis", ids), "GET, HEAD"
	}
	switch {
	case strings.HasPrefix(path, root+"/SessionService/Sessions/"):
		id := strings.TrimPrefix(path, root+"/SessionService/Sessions/")
		s.mu.Lock()
		found := false
		for _, x := range s.sessions {
			found = found || x == id
		}
		s.mu.Unlock()
		if !found {
			return nil, ""
		}
		d := odata(path, "#Session.v1_7_0.Session")
		d["Id"], d["Name"], d["UserName"] = id, "User Session", s.user
		return d, "GET, HEAD, DELETE"
	case strings.HasPrefix(path, sys+"/Controllers/"):
		id := strings.TrimPrefix(path, sys+"/Controllers/")
		for _, n := range v.Nodes {
			if chassisID(n.Path) == id {
				return s.controllerDoc(v, n, path), "GET, HEAD"
			}
		}
	case strings.HasPrefix(path, sys+"/Volumes/"):
		id := strings.TrimPrefix(path, sys+"/Volumes/")
		for _, o := range v.Volumes {
			if o.ID == id {
				return s.volumeDoc(v, o), "GET, HEAD, DELETE"
			}
		}
	case strings.HasPrefix(path, sys+"/FileSystems/"):
		rest := strings.TrimPrefix(path, sys+"/FileSystems/")
		id, sub, _ := strings.Cut(rest, "/")
		for _, o := range v.FileSystems {
			if o.ID != id {
				continue
			}
			fs := sys + "/FileSystems/" + id
			switch {
			case sub == "":
				return s.fileSystemDoc(v, o), "GET, HEAD, PATCH, DELETE"
			case sub == "ExportedFileShares":
				var ids []string
				for i := range o.Exports {
					ids = append(ids, fs+"/ExportedFileShares/"+strconv.Itoa(i+1))
				}
				return collection(path, "#FileShareCollection.FileShareCollection", "Exported File Shares", ids), "GET, HEAD, POST"
			case strings.HasPrefix(sub, "ExportedFileShares/"):
				n, err := strconv.Atoi(strings.TrimPrefix(sub, "ExportedFileShares/"))
				if err == nil && n >= 1 && n <= len(o.Exports) {
					return s.shareDoc(v, o, n, path), "GET, HEAD, DELETE"
				}
			}
		}
	case strings.HasPrefix(path, fabric+"/Endpoints/"):
		id := strings.TrimPrefix(path, fabric+"/Endpoints/")
		for _, n := range v.Nodes {
			if "target-"+chassisID(n.Path) == id {
				d := odata(path, "#Endpoint.v1_8_2.Endpoint")
				d["Id"], d["Name"], d["EndpointProtocol"] = id, "NVMe/TCP target on "+n.Name, "NVMeOverFabrics"
				st, h := "Enabled", model.OK
				if !n.Up {
					st, h = "UnavailableOffline", model.Critical
				}
				d["Status"] = status(st, h)
				d["ConnectedEntities"] = []any{map[string]any{"EntityRole": "Target", "EntityType": "StorageSubsystem", "EntityLink": ref(sys)}}
				td := map[string]any{"TransportProtocol": "NVMeOverFabrics", "Port": NVMePort}
				addr := v.Addresses["node:"+n.Path] // at vm, the node's own storage address
				if addr == "" {
					addr = v.Addresses["data"]
				}
				if addr != "" {
					td["IPv4Address"] = map[string]any{"Address": addr}
				}
				d["IPTransportDetails"] = []any{td}
				return d, "GET, HEAD"
			}
		}
		for _, h := range v.Hosts {
			if hostID(h) == id {
				d := odata(path, "#Endpoint.v1_8_2.Endpoint")
				d["Id"], d["Name"], d["EndpointProtocol"] = id, "NVMe host", "NVMeOverFabrics"
				d["Identifiers"] = []any{map[string]any{"DurableName": h, "DurableNameFormat": "NQN"}}
				d["ConnectedEntities"] = []any{map[string]any{"EntityRole": "Initiator", "EntityType": "StorageInitiator"}}
				d["Status"] = status("Enabled", model.OK)
				return d, "GET, HEAD, DELETE"
			}
		}
	case strings.HasPrefix(path, fabric+"/Connections/"):
		id := strings.TrimPrefix(path, fabric+"/Connections/")
		for _, o := range v.Volumes {
			if o.ID == id && len(o.Hosts) > 0 {
				return s.connectionDoc(v, o, path), "GET, HEAD, DELETE"
			}
		}
	case strings.HasPrefix(path, root+"/Chassis/"):
		return s.chassisDoc(v, strings.TrimPrefix(path, root+"/Chassis/"), path)
	}
	return nil, ""
}

func stateOf(v engine.StorageSystemView) string {
	switch v.State {
	case "Offline":
		return "UnavailableOffline"
	case "Degraded":
		return "Enabled" // degraded is the health; the system still serves
	}
	return "Enabled"
}

// conditions are the system's alerts as Redfish Status.Conditions.
func conditions(v engine.StorageSystemView, sys string) []any {
	out := []any{}
	for _, a := range v.Alerts {
		id := "ResourceEvent.1.3.ResourceStatusChangedWarning"
		if a.Severity == model.Critical {
			id = "ResourceEvent.1.3.ResourceStatusChangedCritical"
		}
		c := map[string]any{"MessageId": id, "Message": a.Message, "Severity": a.Severity, "MessageArgs": []any{a.ID}}
		if a.Object != "" {
			c["OriginOfCondition"] = ref(sys)
		}
		out = append(out, c)
	}
	return out
}

func (s *server) storageDoc(v engine.StorageSystemView) map[string]any {
	sys := s.sysURI()
	d := odata(sys, "#Storage.v1_23_0.Storage")
	d["Id"], d["Name"], d["Description"] = s.name, s.name, fmt.Sprintf("%s storage system (%s)", v.Template, strings.Join(v.Protocols, ", "))
	st := status(stateOf(v), v.Health)
	st["Conditions"] = conditions(v, sys)
	d["Status"] = st
	d["StoragePools"], d["Volumes"], d["FileSystems"], d["Controllers"] = ref(sys+"/StoragePools"), ref(sys+"/Volumes"), ref(sys+"/FileSystems"), ref(sys+"/Controllers")
	var drives []any
	for _, n := range append(append([]engine.StorageNodeView{}, v.Nodes...), v.Enclosures...) {
		for _, id := range s.poolDriveIDs(n.Path) {
			drives = append(drives, ref(root+"/Chassis/"+chassisID(n.Path)+"/Drives/"+id))
		}
	}
	if drives == nil {
		drives = []any{}
	}
	d["Drives"], d["Drives@odata.count"] = drives, len(drives)
	d["Identifiers"] = []any{map[string]any{"DurableName": uuidOf(s.path), "DurableNameFormat": "UUID"}}
	return d
}

// poolDriveIDs are the ids of a device's pool drives.
func (s *server) poolDriveIDs(devPath string) []string {
	var out []string
	s.a.rt.Engine.Read(func(m *model.Model) {
		sys := m.StorageBy[s.path]
		for _, c := range sys.Drives {
			if c.Device.Path == devPath {
				out = append(out, c.Name)
			}
		}
	})
	return out
}

func (s *server) poolDoc(v engine.StorageSystemView) map[string]any {
	p := s.sysURI() + "/StoragePools/pool"
	d := odata(p, "#StoragePool.v1_9_2.StoragePool")
	d["Id"], d["Name"], d["Description"] = "pool", "Pool", fmt.Sprintf("%d+%d protection over %d drives", v.DataUnits, v.ParityUnits, v.Drives)
	d["SupportedPoolTypes"] = []any{"Block", "File", "Object"}
	d["Capacity"] = map[string]any{"Data": map[string]any{"AllocatedBytes": v.UsableBytes, "ConsumedBytes": v.UsedBytes, "ProvisionedBytes": v.ProvisionedBytes}}
	rem := 0
	if v.UsableBytes > 0 {
		rem = int(100 - v.UsedPercent)
		if rem < 0 {
			rem = 0
		}
	}
	d["RemainingCapacityPercent"] = rem
	if v.FullPercent > 0 {
		d["LowSpaceWarningThresholdPercents"] = []any{100 - v.FullPercent}
	}
	h := model.OK
	for _, a := range v.Alerts {
		if strings.HasPrefix(a.ID, "Pool") {
			h = model.WorstHealth(h, a.Severity)
		}
	}
	st := "Enabled"
	if v.DrivesFailed > v.ParityUnits {
		st = "UnavailableOffline"
	}
	d["Status"] = status(st, h)
	d["AllocatedVolumes"] = ref(p + "/AllocatedVolumes")
	return d
}

func (s *server) controllerDoc(v engine.StorageSystemView, n engine.StorageNodeView, path string) map[string]any {
	d := odata(path, "#StorageController.v1_12_0.StorageController")
	d["Id"], d["Name"] = chassisID(n.Path), n.Name
	st, h := "Enabled", model.OK
	if !n.Up {
		st, h = "UnavailableOffline", model.Critical
	}
	d["Status"] = status(st, h)
	var protos []any
	for _, p := range v.Protocols {
		switch p {
		case "nfs":
			protos = append(protos, "NFSv4")
		case "s3":
			protos = append(protos, "HTTP")
		case "nvme-tcp":
			protos = append(protos, "NVMeOverFabrics")
		}
	}
	d["SupportedControllerProtocols"] = protos
	d["SupportedDeviceProtocols"] = []any{"NVMe"}
	return d
}

func capacityOf(o engine.StorageObjectView) map[string]any {
	data := map[string]any{"ConsumedBytes": o.UsedBytes}
	if o.CapacityBytes > 0 {
		data["AllocatedBytes"] = o.CapacityBytes
	}
	return map[string]any{"Data": data}
}

func (s *server) volumeDoc(v engine.StorageSystemView, o engine.StorageObjectView) map[string]any {
	p := s.sysURI() + "/Volumes/" + o.ID
	d := odata(p, "#Volume.v1_11_0.Volume")
	d["Id"], d["Name"], d["CapacityBytes"] = o.ID, o.Name, o.CapacityBytes
	d["Capacity"] = capacityOf(o)
	d["Status"] = status(objState(o), o.Health)
	d["Identifiers"] = []any{map[string]any{"DurableName": strings.ToUpper(o.NGUID), "DurableNameFormat": "NGUID"}}
	d["AccessCapabilities"] = []any{"Read", "Write"}
	d["NVMeNamespaceProperties"] = map[string]any{"NamespaceId": "0x1", "NumberLBAFormats": 1, "FormattedLBASize": "LBA 0: 512 bytes"}
	d["Links"] = map[string]any{"OwningStorageResource": ref(s.sysURI())}
	return d
}

func (s *server) fileSystemDoc(v engine.StorageSystemView, o engine.StorageObjectView) map[string]any {
	p := s.sysURI() + "/FileSystems/" + o.ID
	d := odata(p, "#FileSystem.v1_4_2.FileSystem")
	d["Id"], d["Name"] = o.ID, o.Name
	d["Capacity"] = capacityOf(o)
	if o.CapacityBytes > 0 {
		rem := 100 - int(o.UsedBytes*100/o.CapacityBytes)
		d["RemainingCapacityPercent"] = max(rem, 0)
	}
	d["ExportedShares"] = ref(p + "/ExportedFileShares")
	d["AccessCapabilities"] = []any{"Read", "Write"}
	d["CaseSensitive"], d["CasePreserved"], d["MaxFileNameLengthBytes"] = true, true, 255
	d["Identifiers"] = []any{map[string]any{"DurableName": uuidOf(s.path + "/fs/" + o.ID), "DurableNameFormat": "UUID"}}
	d["Links"] = map[string]any{}
	return d
}

func (s *server) shareDoc(v engine.StorageSystemView, o engine.StorageObjectView, n int, path string) map[string]any {
	x := o.Exports[n-1]
	d := odata(path, "#FileShare.v1_3_0.FileShare")
	d["Id"], d["Name"] = strconv.Itoa(n), o.Name+" export to "+x.Clients
	d["FileSharePath"] = "/" + o.Name
	d["FileSharingProtocols"] = []any{"NFSv4_1"}
	caps := []any{"Read"}
	if x.Access == "rw" {
		caps = append(caps, "Write")
	}
	d["DefaultAccessCapabilities"] = caps
	d["RootAccess"] = !x.RootSquash
	d["Status"] = status(objState(o), o.Health)
	if o.CapacityBytes > 0 {
		d["FileShareTotalQuotaBytes"], d["FileShareRemainingQuotaBytes"], d["FileShareQuotaType"] = o.CapacityBytes, max(o.CapacityBytes-o.UsedBytes, 0), "Hard"
	}
	d["Links"] = map[string]any{"FileSystem": ref(s.sysURI() + "/FileSystems/" + o.ID)}
	// FileShare has no client list of its own: the rule's clients are in its description (and
	// are written as Oem.DcTwin.Clients)
	d["Description"] = fmt.Sprintf("NFS export of /%s to %s (%s)", o.Name, x.Clients, x.Access)
	return d
}

func (s *server) connectionDoc(v engine.StorageSystemView, o engine.StorageObjectView, path string) map[string]any {
	d := odata(path, "#Connection.v1_4_0.Connection")
	d["Id"], d["Name"], d["ConnectionType"] = o.ID, "Hosts of volume "+o.Name, "Storage"
	d["VolumeInfo"] = []any{map[string]any{"Volume": ref(s.sysURI() + "/Volumes/" + o.ID), "AccessCapabilities": []any{"Read", "Write"}}}
	var ini []any
	for _, h := range o.Hosts {
		ini = append(ini, ref(fabric+"/Endpoints/"+hostID(h)))
	}
	links := map[string]any{"InitiatorEndpoints": ini}
	if o.Node != "" {
		links["TargetEndpoints"] = []any{ref(fabric + "/Endpoints/target-" + chassisID(o.Node))}
	} else {
		links["TargetEndpoints"] = []any{}
	}
	d["Links"] = links
	d["Status"] = status(objState(o), o.Health)
	return d
}

func (s *server) chassisDoc(v engine.StorageSystemView, rest, path string) (map[string]any, string) {
	id, sub, _ := strings.Cut(rest, "/")
	var node *engine.StorageNodeView
	enclosure := false
	for i, n := range v.Nodes {
		if chassisID(n.Path) == id {
			node = &v.Nodes[i]
		}
	}
	for i, n := range v.Enclosures {
		if chassisID(n.Path) == id {
			node, enclosure = &v.Enclosures[i], true
		}
	}
	if node == nil {
		return nil, ""
	}
	cp := root + "/Chassis/" + id
	eng := s.a.rt.Engine
	var dev *model.Device
	eng.Read(func(m *model.Model) { dev = m.DeviceBy[node.Path] })
	if dev == nil {
		return nil, ""
	}
	switch {
	case sub == "":
		d := odata(cp, "#Chassis.v1_23_0.Chassis")
		d["Id"], d["Name"] = id, node.Name
		d["ChassisType"] = "RackMount"
		if enclosure {
			d["ChassisType"] = "StorageEnclosure"
		}
		st, h := "Enabled", eng.Health(dev)
		if !node.Up {
			st = "UnavailableOffline"
		}
		d["Status"] = status(st, h)
		d["SerialNumber"], d["Manufacturer"] = dev.Identity.Serial, "dc-twin"
		if idsp := dev.Profile.Spec.Identity; idsp != nil {
			d["Manufacturer"], d["Model"] = idsp.Manufacturer, idsp.Model
		}
		d["Drives"] = ref(cp + "/Drives")
		d["Links"] = map[string]any{"Storage": []any{ref(s.sysURI())}}
		return d, "GET, HEAD"
	case sub == "Drives":
		var ids []string
		for _, did := range s.poolDriveIDs(node.Path) {
			ids = append(ids, cp+"/Drives/"+did)
		}
		return collection(path, "#DriveCollection.DriveCollection", "Drives", ids), "GET, HEAD"
	case strings.HasPrefix(sub, "Drives/"):
		did := strings.TrimPrefix(sub, "Drives/")
		for _, c := range dev.Drives() {
			if c.Name != did || !containsStr(s.poolDriveIDs(node.Path), did) {
				continue
			}
			ds := eng.DriveStatus(c)
			if !ds.Present {
				return nil, ""
			}
			d := odata(path, "#Drive.v1_22_0.Drive")
			d["Id"], d["Name"] = c.Name, c.Name
			d["CapacityBytes"], d["Protocol"], d["MediaType"] = c.Drive.CapacityBytes, protocolOf(c.Drive.Protocol), "SSD"
			d["Model"], d["Revision"], d["SerialNumber"] = c.Model, c.Drive.Firmware, c.State.Serial
			if c.State.Serial == "" {
				d["SerialNumber"] = c.Drive.Serial
			}
			d["Status"] = status(ds.State, ds.Health)
			d["FailurePredicted"] = ds.FailurePredicted
			d["PredictedMediaLifeLeftPercent"] = ds.LifeLeftPercent
			d["Links"] = map[string]any{"Storage": ref(s.sysURI())}
			return d, "GET, HEAD"
		}
	}
	return nil, ""
}

func protocolOf(p string) string {
	switch strings.ToLower(p) {
	case "sas":
		return "SAS"
	case "sata":
		return "SATA"
	}
	return "NVMe"
}

func containsStr(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// uuidOf derives a stable UUID from a path.
func uuidOf(p string) string {
	h := sha256.Sum256([]byte("dc-twin storage " + p))
	h[6] = h[6]&0x0f | 0x50
	h[8] = h[8]&0x3f | 0x80
	x := hex.EncodeToString(h[:16])
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
}

// ---------------------------------------------------------------- writes

func readBody(r *http.Request) (map[string]any, error) {
	var body map[string]any
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return nil, err
	}
	return body, nil
}

func int64Of(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// sizeOf reads a size from CapacityBytes, or Capacity.Data.AllocatedBytes as Swordfish writes it.
func sizeOf(body map[string]any) (int64, bool) {
	if n, ok := int64Of(body["CapacityBytes"]); ok {
		return n, true
	}
	if c, ok := body["Capacity"].(map[string]any); ok {
		if d, ok := c["Data"].(map[string]any); ok {
			return int64Of(d["AllocatedBytes"])
		}
	}
	return 0, false
}

func idOf(v any) string {
	if m, ok := v.(map[string]any); ok {
		s, _ := m["@odata.id"].(string)
		return strings.TrimSuffix(s, "/")
	}
	return ""
}

func created(w http.ResponseWriter, loc string, doc map[string]any) {
	w.Header().Set("Location", loc)
	writeJSON(w, http.StatusCreated, doc)
}

func (s *server) post(w http.ResponseWriter, r *http.Request, path string, src schema.EventSource) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Base.1.18.MalformedJSON", "The request body is not valid JSON.")
		return
	}
	eng := s.a.rt.Engine
	sys := s.sysURI()
	switch {
	case path == root+"/SessionService/Sessions":
		u, _ := body["UserName"].(string)
		p, _ := body["Password"].(string)
		if subtle.ConstantTimeCompare([]byte(u), []byte(s.user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(s.password)) != 1 {
			writeError(w, http.StatusUnauthorized, "Base.1.18.NoValidSession", "Invalid username or password.")
			return
		}
		b := make([]byte, 24)
		_, _ = rand.Read(b)
		tok := hex.EncodeToString(b)
		s.mu.Lock()
		s.sessSeq++
		id := strconv.Itoa(s.sessSeq)
		s.sessions[tok] = id
		s.mu.Unlock()
		loc := root + "/SessionService/Sessions/" + id
		w.Header().Set("X-Auth-Token", tok)
		created(w, loc, map[string]any{"@odata.id": loc, "@odata.type": "#Session.v1_7_0.Session", "Id": id, "Name": "User Session", "UserName": u})
	case path == sys+"/Volumes" || path == sys+"/StoragePools/pool/AllocatedVolumes":
		name, _ := body["Name"].(string)
		size, _ := sizeOf(body)
		vol, err := eng.CreateStorageVolume(s.req(r, "storage.volume.create", src), engine.VolumeCreate{Name: name, CapacityBytes: size})
		if err != nil {
			engineError(w, err)
			return
		}
		v := s.view()
		for _, o := range v.Volumes {
			if o.ID == vol.ID {
				created(w, sys+"/Volumes/"+vol.ID, s.volumeDoc(v, o))
			}
		}
	case path == sys+"/FileSystems":
		name, _ := body["Name"].(string)
		size, _ := sizeOf(body)
		fs, err := eng.CreateFileSystem(s.req(r, "storage.filesystem.create", src), engine.FileSystemSpec{Name: name, QuotaBytes: size})
		if err != nil {
			engineError(w, err)
			return
		}
		v := s.view()
		for _, o := range v.FileSystems {
			if o.ID == fs.ID {
				created(w, sys+"/FileSystems/"+fs.ID, s.fileSystemDoc(v, o))
			}
		}
	case strings.HasPrefix(path, sys+"/FileSystems/") && strings.HasSuffix(path, "/ExportedFileShares"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, sys+"/FileSystems/"), "/ExportedFileShares")
		v := s.view()
		var fs *engine.StorageObjectView
		for i := range v.FileSystems {
			if v.FileSystems[i].ID == id {
				fs = &v.FileSystems[i]
			}
		}
		if fs == nil {
			writeError(w, http.StatusNotFound, "Base.1.18.ResourceNotFound", "No file system "+id)
			return
		}
		x := model.Export{Clients: "*", Access: "rw", RootSquash: true}
		if caps, ok := body["DefaultAccessCapabilities"].([]any); ok {
			x.Access = "ro"
			for _, c := range caps {
				if c == "Write" {
					x.Access = "rw"
				}
			}
		}
		if ra, ok := body["RootAccess"].(bool); ok {
			x.RootSquash = !ra
		}
		if oem, ok := body["Oem"].(map[string]any); ok {
			if dt, ok := oem["DcTwin"].(map[string]any); ok {
				if c, ok := dt["Clients"].(string); ok && c != "" {
					x.Clients = c
				}
			}
		}
		exports := append(append([]model.Export{}, fs.Exports...), x)
		if _, err := eng.UpdateFileSystem(s.req(r, "storage.export.create", src), id, nil, exports); err != nil {
			engineError(w, err)
			return
		}
		v = s.view()
		for _, o := range v.FileSystems {
			if o.ID == id {
				loc := path + "/" + strconv.Itoa(len(o.Exports))
				created(w, loc, s.shareDoc(v, o, len(o.Exports), loc))
			}
		}
	case path == fabric+"/Endpoints":
		nqn := ""
		if ids, ok := body["Identifiers"].([]any); ok {
			for _, x := range ids {
				if m, ok := x.(map[string]any); ok && m["DurableNameFormat"] == "NQN" {
					nqn, _ = m["DurableName"].(string)
				}
			}
		}
		if err := eng.RegisterHost(s.req(r, "storage.host.register", src), nqn, false); err != nil {
			engineError(w, err)
			return
		}
		doc, _ := s.render(s.view(), fabric+"/Endpoints/"+hostID(nqn))
		created(w, fabric+"/Endpoints/"+hostID(nqn), doc)
	case path == fabric+"/Connections":
		var volID string
		if vi, ok := body["VolumeInfo"].([]any); ok && len(vi) > 0 {
			if m, ok := vi[0].(map[string]any); ok {
				volID = strings.TrimPrefix(idOf(m["Volume"]), sys+"/Volumes/")
			}
		}
		links, _ := body["Links"].(map[string]any)
		inis, _ := links["InitiatorEndpoints"].([]any)
		if volID == "" || len(inis) == 0 {
			writeError(w, http.StatusBadRequest, "Base.1.18.PropertyMissing", "A connection needs VolumeInfo[0].Volume and Links.InitiatorEndpoints.")
			return
		}
		v := s.view()
		for _, x := range inis {
			epID := strings.TrimPrefix(idOf(x), fabric+"/Endpoints/")
			nqn := ""
			for _, h := range v.Hosts {
				if hostID(h) == epID {
					nqn = h
				}
			}
			if nqn == "" {
				writeError(w, http.StatusBadRequest, "Base.1.18.ResourceNotFound", "No initiator endpoint "+idOf(x))
				return
			}
			if _, err := eng.MapVolume(s.req(r, "storage.volume.map", src), volID, nqn, false); err != nil {
				engineError(w, err)
				return
			}
		}
		v = s.view()
		for _, o := range v.Volumes {
			if o.ID == volID {
				created(w, fabric+"/Connections/"+o.ID, s.connectionDoc(v, o, fabric+"/Connections/"+o.ID))
				return
			}
		}
		writeError(w, http.StatusNotFound, "Base.1.18.ResourceNotFound", "No volume "+volID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "Base.1.18.OperationNotAllowed", "POST is not allowed on "+path)
	}
}

func (s *server) patch(w http.ResponseWriter, r *http.Request, path string, src schema.EventSource) {
	body, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Base.1.18.MalformedJSON", "The request body is not valid JSON.")
		return
	}
	sys := s.sysURI()
	if strings.HasPrefix(path, sys+"/FileSystems/") && !strings.Contains(strings.TrimPrefix(path, sys+"/FileSystems/"), "/") {
		id := strings.TrimPrefix(path, sys+"/FileSystems/")
		size, ok := sizeOf(body)
		if !ok {
			writeError(w, http.StatusBadRequest, "Base.1.18.PropertyNotWritable", "Only Capacity.Data.AllocatedBytes (the quota) is writable.")
			return
		}
		if _, err := s.a.rt.Engine.UpdateFileSystem(s.req(r, "storage.filesystem.update", src), id, &size, nil); err != nil {
			engineError(w, err)
			return
		}
		s.get(w, r, path)
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "Base.1.18.OperationNotAllowed", "PATCH is not allowed on "+path)
}

func (s *server) delete(w http.ResponseWriter, r *http.Request, path string, src schema.EventSource) {
	eng := s.a.rt.Engine
	sys := s.sysURI()
	var err error
	switch {
	case strings.HasPrefix(path, root+"/SessionService/Sessions/"):
		id := strings.TrimPrefix(path, root+"/SessionService/Sessions/")
		s.mu.Lock()
		for tok, x := range s.sessions {
			if x == id {
				delete(s.sessions, tok)
			}
		}
		s.mu.Unlock()
	case strings.HasPrefix(path, sys+"/Volumes/"):
		err = eng.DeleteStorageObject(s.req(r, "storage.volume.delete", src), "volume", strings.TrimPrefix(path, sys+"/Volumes/"))
	case strings.HasPrefix(path, sys+"/FileSystems/"):
		rest := strings.TrimPrefix(path, sys+"/FileSystems/")
		id, sub, _ := strings.Cut(rest, "/")
		if sub == "" {
			err = eng.DeleteStorageObject(s.req(r, "storage.filesystem.delete", src), "filesystem", id)
			break
		}
		n, perr := strconv.Atoi(strings.TrimPrefix(sub, "ExportedFileShares/"))
		v := s.view()
		for _, o := range v.FileSystems {
			if o.ID == id && perr == nil && n >= 1 && n <= len(o.Exports) {
				ex := append(append([]model.Export{}, o.Exports[:n-1]...), o.Exports[n:]...)
				_, err = eng.UpdateFileSystem(s.req(r, "storage.export.delete", src), id, nil, ex)
				goto done
			}
		}
		err = fmt.Errorf("%w: %s", engine.ErrNoStorageObject, path)
	case strings.HasPrefix(path, fabric+"/Connections/"):
		id := strings.TrimPrefix(path, fabric+"/Connections/")
		v := s.view()
		err = fmt.Errorf("%w: %s", engine.ErrNoStorageObject, path)
		for _, o := range v.Volumes {
			if o.ID == id && len(o.Hosts) > 0 {
				err = nil
				for _, h := range o.Hosts {
					if _, e := eng.MapVolume(s.req(r, "storage.volume.unmap", src), id, h, true); e != nil {
						err = e
					}
				}
			}
		}
	case strings.HasPrefix(path, fabric+"/Endpoints/host-"):
		id := strings.TrimPrefix(path, fabric+"/Endpoints/")
		err = fmt.Errorf("%w: %s", engine.ErrNoStorageObject, path)
		for _, h := range s.view().Hosts {
			if hostID(h) == id {
				err = eng.RegisterHost(s.req(r, "storage.host.remove", src), h, true)
			}
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "Base.1.18.OperationNotAllowed", "DELETE is not allowed on "+path)
		return
	}
done:
	if err != nil {
		engineError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// metadata is the service's CSDL: references to the DMTF and Swordfish schemas it serves.
var metadata = func() string {
	type ref struct{ uri, ns, ver string }
	refs := []ref{
		{"v1/ServiceRoot_v1.xml", "ServiceRoot", "v1_22_0"},
		{"v1/StorageCollection_v1.xml", "StorageCollection", ""},
		{"v1/ComputerSystemCollection_v1.xml", "ComputerSystemCollection", ""},
		{"v1/ComputerSystem_v1.xml", "ComputerSystem", "v1_20_0"},
		{"v1/Storage_v1.xml", "Storage", "v1_23_0"},
		{"v1/StorageControllerCollection_v1.xml", "StorageControllerCollection", ""},
		{"v1/StorageController_v1.xml", "StorageController", "v1_12_0"},
		{"swordfish/v1/StoragePoolCollection_v1.xml", "StoragePoolCollection", ""},
		{"swordfish/v1/Capacity_v1.xml", "Capacity", "v1_2_2"},
		{"swordfish/v1/DataStorageLoSCapabilities_v1.xml", "DataStorageLoSCapabilities", ""},
		{"swordfish/v1/StoragePool_v1.xml", "StoragePool", "v1_9_2"},
		{"swordfish/v1/VolumeCollection_v1.xml", "VolumeCollection", ""},
		{"swordfish/v1/Volume_v1.xml", "Volume", "v1_11_0"},
		{"swordfish/v1/FileSystemCollection_v1.xml", "FileSystemCollection", ""},
		{"swordfish/v1/FileSystem_v1.xml", "FileSystem", "v1_4_2"},
		{"swordfish/v1/FileShareCollection_v1.xml", "FileShareCollection", ""},
		{"swordfish/v1/FileShare_v1.xml", "FileShare", "v1_3_0"},
		{"v1/FabricCollection_v1.xml", "FabricCollection", ""},
		{"v1/Fabric_v1.xml", "Fabric", "v1_4_0"},
		{"v1/EndpointCollection_v1.xml", "EndpointCollection", ""},
		{"v1/Endpoint_v1.xml", "Endpoint", "v1_8_2"},
		{"v1/ConnectionCollection_v1.xml", "ConnectionCollection", ""},
		{"v1/Connection_v1.xml", "Connection", "v1_4_0"},
		{"v1/ChassisCollection_v1.xml", "ChassisCollection", ""},
		{"v1/Chassis_v1.xml", "Chassis", "v1_23_0"},
		{"v1/DriveCollection_v1.xml", "DriveCollection", ""},
		{"v1/Drive_v1.xml", "Drive", "v1_22_0"},
		{"v1/SessionService_v1.xml", "SessionService", "v1_1_9"},
		{"v1/SessionCollection_v1.xml", "SessionCollection", ""},
		{"v1/Session_v1.xml", "Session", "v1_7_0"},
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<edmx:Edmx xmlns:edmx="http://docs.oasis-open.org/odata/ns/edmx" Version="4.0">` + "\n")
	for _, r := range refs {
		fmt.Fprintf(&b, `  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/%s"><edmx:Include Namespace="%s"/>`, r.uri, r.ns)
		if r.ver != "" {
			fmt.Fprintf(&b, `<edmx:Include Namespace="%s.%s"/>`, r.ns, r.ver)
		}
		b.WriteString("</edmx:Reference>\n")
	}
	b.WriteString(`  <edmx:DataServices>
    <Schema xmlns="http://docs.oasis-open.org/odata/ns/edm" Namespace="Service">
      <EntityContainer Name="Service" Extends="ServiceRoot.v1_22_0.ServiceContainer"/>
    </Schema>
  </edmx:DataServices>
</edmx:Edmx>
`)
	return b.String()
}()

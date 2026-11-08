package redfish

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Storage as the model has it (docs/reviews/2026-11-08-storage-deep-dive.md 5.1, 5.2): drives,
// storage controllers and their volumes. The generated tree renders all of it; a captured tree
// keeps its own Storage resources but binds its drives to the model's in order (drives it holds
// beyond the model's are absent), and serves the controllers its dialect names
// (storageControllers) from the model.

// storageSet is a Storage resource the twin renders from the model: a controller and the drives
// behind it, or (ctl nil) the drives attached to the host directly.
type storageSet struct {
	id     string
	ctl    *model.Component
	drives []*model.Component
}

func (b *bmc) generated() bool { return b.dialect.Spec.Backend == "generated" }

func (b *bmc) systemPath() string {
	if b.generated() {
		return "/redfish/v1/Systems/1"
	}
	return b.dialect.Spec.System
}

// modelStorages are the Storage resources rendered from the model: in the generated tree every
// controller and the host's direct drives (as "1"); in a captured tree the dialect's
// storageControllers.
func (b *bmc) modelStorages() []storageSet {
	d := b.dev
	var out []storageSet
	if b.generated() {
		var direct []*model.Component
		for _, c := range d.Drives() {
			if c.Drive.Controller == nil {
				direct = append(direct, c)
			}
		}
		if len(direct) > 0 {
			out = append(out, storageSet{id: "1", drives: direct})
		}
		for _, ctl := range d.Controllers() {
			out = append(out, storageSet{id: strings.ToUpper(strings.ReplaceAll(ctl.Name, "-", "_")), ctl: ctl, drives: ctl.Controller.Drives})
		}
		return out
	}
	for _, ctl := range d.Controllers() {
		if id, ok := b.dialect.Spec.StorageControllers[ctl.Group]; ok {
			if ctl.Index > 0 {
				id = fmt.Sprintf("%s.%d", id, ctl.Index)
			}
			out = append(out, storageSet{id: id, ctl: ctl, drives: ctl.Controller.Drives})
		}
	}
	return out
}

// storageAt resolves a model-rendered Storage resource from its path.
func (b *bmc) storageAt(path string) *storageSet {
	base := b.systemPath() + "/Storage/"
	if !strings.HasPrefix(path, base) {
		return nil
	}
	id := strings.TrimPrefix(path, base)
	for _, s := range b.modelStorages() {
		if s.id == id {
			s := s
			return &s
		}
	}
	return nil
}

// storageAllow is the Allow header (Redfish Specification, Allow) of a model-rendered volume
// collection or volume: a RAID controller's collection takes POST and its volumes DELETE; the
// host's directly attached drives have no controller to make volumes, so theirs are read-only.
func (b *bmc) storageAllow(path string) string {
	i := strings.LastIndex(path, "/Volumes")
	if i < 0 {
		return ""
	}
	set := b.storageAt(path[:i])
	if set == nil {
		return ""
	}
	member := path[i+len("/Volumes"):] != ""
	switch {
	case set.ctl == nil:
		return "GET, HEAD"
	case member:
		return "GET, HEAD, DELETE"
	}
	return "GET, HEAD, POST"
}

// driveAt resolves a drive resource path to the model's drive: a model-rendered one, or a
// captured one bound to the model's drive in the same place.
func (b *bmc) driveAt(path string) *model.Component {
	if i := strings.Index(path, "/Drives/"); i > 0 {
		if s := b.storageAt(path[:i]); s != nil {
			id := path[i+len("/Drives/"):]
			for _, c := range s.drives {
				if driveID(c) == id {
					return c
				}
			}
			return nil
		}
	}
	if k := b.capDriveIndex(path); k >= 0 {
		ds := b.boundDrives()
		if k < len(ds) {
			return ds[k]
		}
	}
	return nil
}

// boundDrives are the model's drives a captured tree's drives are bound to, in bay order: all of
// them but those behind a controller the dialect serves from the model.
func (b *bmc) boundDrives() []*model.Component {
	var out []*model.Component
	for _, c := range b.dev.Drives() {
		if c.Drive.Controller != nil {
			if _, own := b.dialect.Spec.StorageControllers[c.Drive.Controller.Group]; own {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// ---- captured drives

// capDrives are a captured tree's drives in bay order: the system's Storage/<id>/Drives/<id>
// resources, then the chassis' Drives/<id> resources (some BMCs list drives only there).
func (b *bmc) capDrives() (storage, chassis []string) {
	b.capOnce.Do(func() {
		if b.tree == nil {
			return
		}
		sys := b.dialect.Spec.System + "/Storage/"
		for p := range b.tree.docs {
			switch {
			case strings.HasPrefix(p, sys) && strings.Count(strings.TrimPrefix(p, sys), "/") == 2 && strings.Contains(p, "/Drives/"):
				b.capStorage = append(b.capStorage, p)
			case strings.HasPrefix(p, "/redfish/v1/Chassis/") && strings.Count(p, "/") == 6 && strings.Contains(p, "/Drives/"):
				b.capChassis = append(b.capChassis, p)
			}
		}
		sort.Slice(b.capStorage, func(i, j int) bool { return naturalLess(b.capStorage[i], b.capStorage[j]) })
		sort.Slice(b.capChassis, func(i, j int) bool { return naturalLess(b.capChassis[i], b.capChassis[j]) })
	})
	return b.capStorage, b.capChassis
}

// capDriveIndex is the bay a captured drive resource (or a resource under it) is bound to, or -1.
func (b *bmc) capDriveIndex(path string) int {
	if b.generated() || b.a.captureDrives {
		return -1
	}
	st, ch := b.capDrives()
	for _, list := range [][]string{st, ch} {
		for i, p := range list {
			if path == p || strings.HasPrefix(path, p+"/") {
				return i
			}
		}
	}
	return -1
}

// driveServed is whether the model has a present drive in bay k.
func (b *bmc) driveServed(k int) bool {
	ds := b.boundDrives()
	if k >= len(ds) {
		return false
	}
	return b.a.eng.DriveStatus(ds[k]).Present
}

// dropAbsentDrives removes, anywhere in a captured document, the links to captured drives the
// model does not have (collections' Members, Storage's Drives, Chassis' Links.Drives), with
// their counts.
func (b *bmc) dropAbsentDrives(v any) {
	if b.generated() || b.a.captureDrives {
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		if arr, ok := v.([]any); ok {
			for _, x := range arr {
				b.dropAbsentDrives(x)
			}
		}
		return
	}
	for k, x := range m {
		arr, ok := x.([]any)
		if !ok {
			b.dropAbsentDrives(x)
			continue
		}
		kept := arr[:0:0]
		dropped := false
		for _, el := range arr {
			if ref, ok := el.(map[string]any); ok {
				if id, _ := ref["@odata.id"].(string); id != "" {
					if bay := b.capDriveIndex(normalise(id)); bay >= 0 && !b.driveServed(bay) {
						dropped = true
						continue
					}
				}
			}
			kept = append(kept, el)
		}
		if dropped {
			m[k] = kept
			if _, ok := m[k+"@odata.count"]; ok {
				m[k+"@odata.count"] = len(kept)
			}
		}
	}
}

// bindCapturedDrive puts the model's drive in a captured drive resource: its size, serial, state
// and wear, and its sanitisation while one runs.
func (r *renderer) bindCapturedDrive(path string, doc map[string]any) {
	b := r.b
	k := b.capDriveIndex(path)
	if k < 0 || !(path == pathOfBay(b, k)) {
		return
	}
	c := b.boundDrives()[k]
	st := b.a.eng.DriveStatus(c)
	di := c.Drive
	doc["CapacityBytes"] = di.CapacityBytes
	doc["SerialNumber"] = st.Serial
	if di.Firmware != "" {
		doc["Revision"] = di.Firmware
	}
	if c.Vendor != "" {
		doc["Manufacturer"], doc["Model"] = c.Vendor, c.Model
	}
	doc["MediaType"] = strings.ToUpper(di.Media)
	doc["Protocol"] = protocolName(di.Protocol)
	doc["FailurePredicted"] = st.FailurePredicted
	doc["PredictedMediaLifeLeftPercent"] = st.LifeLeftPercent
	sm, _ := doc["Status"].(map[string]any)
	if sm == nil {
		sm = map[string]any{}
		doc["Status"] = sm
	}
	sm["Health"], sm["State"] = st.Health, st.State
	if _, ok := sm["HealthRollup"]; ok {
		sm["HealthRollup"] = st.Health
	}
	if st.Erasing {
		doc["Operations"] = []any{map[string]any{"Operation": "Sanitize", "OperationName": "Sanitize", "PercentageComplete": st.Percent}}
	} else {
		delete(doc, "Operations")
	}
	acts, _ := doc["Actions"].(map[string]any)
	if acts == nil {
		acts = map[string]any{}
		doc["Actions"] = acts
	}
	acts["#Drive.SecureErase"] = map[string]any{"target": path + "/Actions/Drive.SecureErase",
		"SanitizationType@Redfish.AllowableValues": []any{"CryptographicErase", "BlockErase", "Overwrite"}}
}

// pathOfBay is the captured Storage drive resource bound to bay k (the chassis' copy is bound too,
// but binds the same way through capDriveIndex).
func pathOfBay(b *bmc, k int) string {
	st, ch := b.capDrives()
	if k < len(st) {
		return st[k]
	}
	if k < len(ch) {
		return ch[k]
	}
	return ""
}

// naturalLess orders paths with their numbers compared as numbers (Disk.Bay.2 before Disk.Bay.10).
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		ad, bd := unicode.IsDigit(rune(a[0])), unicode.IsDigit(rune(b[0]))
		if ad && bd {
			i, j := 0, 0
			for i < len(a) && unicode.IsDigit(rune(a[i])) {
				i++
			}
			for j < len(b) && unicode.IsDigit(rune(b[j])) {
				j++
			}
			na, _ := strconv.Atoi(a[:i])
			nb, _ := strconv.Atoi(b[:j])
			if na != nb {
				return na < nb
			}
			a, b = a[i:], b[j:]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

// ---- model-rendered resources

func protocolName(p string) string {
	switch p {
	case "nvme":
		return "NVMe"
	case "sas":
		return "SAS"
	}
	return "SATA"
}

var formFactors = map[string]string{"m.2": "M2_2280", "u.2": "U2", "e1.s": "EDSFF_1U_Short", "e3.s": "EDSFF_E3_Short", "2.5": "Drive2_5", "3.5": "Drive3_5"}

var volumeTypes = map[string]string{"RAID0": "NonRedundant", "RAID1": "Mirrored", "RAID5": "StripedWithParity", "RAID6": "StripedWithParity",
	"RAID10": "SpannedMirrors", "RAID50": "SpannedStripesWithParity", "RAID60": "SpannedStripesWithParity"}

// modelStorageCollection adds the model-rendered Storage members to a captured tree's Storage
// collection.
func (b *bmc) modelStorageCollection(path string, doc map[string]any) {
	if b.generated() || path != b.dialect.Spec.System+"/Storage" {
		return
	}
	members, _ := doc["Members"].([]any)
	for _, s := range b.modelStorages() {
		members = append(members, ref(path+"/"+s.id))
	}
	doc["Members"] = members
	if _, ok := doc["Members@odata.count"]; ok {
		doc["Members@odata.count"] = len(members)
	}
}

// storageDoc renders the model's Storage resources under the system: the collection (generated
// tree only), each Storage, its controllers, drives and volumes.
func (r *renderer) storageDoc(path string) (map[string]any, bool) {
	b := r.b
	sys := b.systemPath()
	if path == sys+"/Storage" && b.generated() {
		var ids []string
		for _, s := range b.modelStorages() {
			ids = append(ids, path+"/"+s.id)
		}
		return collection(path, "#StorageCollection.StorageCollection", "Storage", ids), true
	}
	base := sys + "/Storage/"
	if !strings.HasPrefix(path, base) {
		return nil, false
	}
	rest := strings.TrimPrefix(path, base)
	id, sub, _ := strings.Cut(rest, "/")
	var set *storageSet
	for _, s := range b.modelStorages() {
		if s.id == id {
			s := s
			set = &s
		}
	}
	if set == nil {
		return nil, false
	}
	sp := base + id
	eng := b.a.eng
	vols := []engine.VolumeView{}
	if set.ctl != nil {
		for _, v := range eng.Volumes(b.dev) {
			if v.Controller == set.ctl.Name {
				vols = append(vols, v)
			}
		}
	}
	present := func() []*model.Component {
		var out []*model.Component
		for _, c := range set.drives {
			if eng.DriveStatus(c).Present {
				out = append(out, c)
			}
		}
		return out
	}
	switch {
	case sub == "":
		doc := odata(path, "#Storage.v1_15_0.Storage")
		var drs []any
		health := model.OK
		for _, c := range present() {
			drs = append(drs, ref(sp+"/Drives/"+driveID(c)))
			health = model.WorstHealth(health, eng.DriveStatus(c).Health)
		}
		for _, v := range vols {
			health = model.WorstHealth(health, v.Health)
		}
		name := "Storage"
		if set.ctl != nil {
			name = set.ctl.Model
			if name == "" {
				name = set.ctl.Name
			}
		}
		doc["Id"], doc["Name"], doc["Drives"], doc["Drives@odata.count"] = id, name, orEmpty(drs), len(drs)
		doc["Status"] = map[string]any{"Health": health, "HealthRollup": health, "State": "Enabled"}
		doc["Controllers"] = ref(sp + "/Controllers")
		doc["Volumes"] = ref(sp + "/Volumes")
		doc["StorageControllers"] = []any{r.controllerDoc(set, sp+"#/StorageControllers/0", true)}
		return doc, true
	case sub == "Controllers":
		return collection(path, "#StorageControllerCollection.StorageControllerCollection", "Storage Controllers", []string{path + "/0"}), true
	case sub == "Controllers/0":
		return r.controllerDoc(set, path, false), true
	case sub == "Drives":
		var ids []string
		for _, c := range present() {
			ids = append(ids, path+"/"+driveID(c))
		}
		return collection(path, "#DriveCollection.DriveCollection", "Drives", ids), true
	case strings.HasPrefix(sub, "Drives/"):
		did := strings.TrimPrefix(sub, "Drives/")
		for _, c := range present() {
			if driveID(c) == did {
				return r.driveDoc(path, c, vols, sp), true
			}
		}
		return nil, true
	case sub == "Volumes":
		var ids []string
		for _, v := range vols {
			ids = append(ids, path+"/"+v.ID)
		}
		doc := collection(path, "#VolumeCollection.VolumeCollection", "Volumes", ids)
		if set.ctl != nil {
			doc["@Redfish.OperationApplyTimeSupport"] = map[string]any{"@odata.type": "#Settings.v1_3_5.OperationApplyTimeSupport",
				"SupportedValues": []any{"Immediate", "OnReset"}}
		}
		return doc, true
	case strings.HasPrefix(sub, "Volumes/"):
		vid := strings.TrimPrefix(sub, "Volumes/")
		for _, v := range vols {
			if v.ID == vid {
				return volumeDoc(path, sp, v), true
			}
		}
		return nil, true
	}
	return nil, true
}

func orEmpty(a []any) []any {
	if a == nil {
		return []any{}
	}
	return a
}

func (r *renderer) controllerDoc(set *storageSet, path string, inline bool) map[string]any {
	doc := map[string]any{"@odata.id": path}
	if !inline {
		doc = odata(path, "#StorageController.v1_7_0.StorageController")
		doc["Id"] = "0"
	} else {
		doc["MemberId"] = "0" // an inline (deprecated) StorageControllers item has no Id
	}
	doc["Name"] = "Storage Controller"
	proto := "NVMe"
	if len(set.drives) > 0 {
		proto = protocolName(set.drives[0].Drive.Protocol)
	}
	doc["SupportedDeviceProtocols"] = []any{proto}
	doc["SupportedControllerProtocols"] = []any{"PCIe"}
	types := []any{}
	h := model.OK
	if set.ctl != nil {
		raid := set.ctl.Controller.RAIDTypes
		if len(raid) == 0 {
			raid = engine.RAIDTypes()
		}
		for _, t := range raid {
			types = append(types, t)
		}
		doc["Model"] = set.ctl.Model
		if fw := r.snap.firmware[set.ctl.Name]; fw != "" {
			doc["FirmwareVersion"] = fw
		}
		h, _ = r.b.a.eng.ComponentHealth(set.ctl)
	}
	// the host's directly attached NVMe drives have no RAID: an explicit empty list, which Ironic
	// reads as "no RAID here" (a missing one it would try)
	doc["SupportedRAIDTypes"] = types
	doc["Status"] = map[string]any{"Health": h, "State": "Enabled"}
	return doc
}

func (r *renderer) driveDoc(path string, c *model.Component, vols []engine.VolumeView, storagePath string) map[string]any {
	st := r.b.a.eng.DriveStatus(c)
	di := c.Drive
	doc := odata(path, "#Drive.v1_17_0.Drive")
	manuf := di.Manufacturer
	if manuf == "" {
		manuf = "dc-twin"
	}
	doc["Id"], doc["Name"], doc["Model"], doc["Manufacturer"] = driveID(c), c.Name, c.Model, manuf
	doc["SerialNumber"], doc["Revision"] = st.Serial, orDefault(di.Firmware, "1.0.0")
	if di.PartNumber != "" {
		doc["PartNumber"] = di.PartNumber
	}
	doc["CapacityBytes"], doc["MediaType"], doc["Protocol"] = di.CapacityBytes, strings.ToUpper(di.Media), protocolName(di.Protocol)
	if ff, ok := formFactors[di.FormFactor]; ok {
		doc["FormFactor"] = ff
	}
	doc["PhysicalLocation"] = map[string]any{"PartLocation": map[string]any{"LocationType": "Bay", "LocationOrdinalValue": di.Bay,
		"ServiceLabel": fmt.Sprintf("Bay %d", di.Bay)}}
	format := "EUI"
	if di.Protocol != "nvme" {
		format = "NAA"
	}
	doc["Identifiers"] = []any{map[string]any{"DurableNameFormat": format, "DurableName": durableName(st.EUI64)}}
	doc["Status"] = map[string]any{"Health": st.Health, "State": st.State}
	doc["FailurePredicted"], doc["PredictedMediaLifeLeftPercent"] = st.FailurePredicted, st.LifeLeftPercent
	doc["LocationIndicatorActive"] = false
	if st.Erasing {
		doc["Operations"] = []any{map[string]any{"Operation": "Sanitize", "OperationName": "Sanitize", "PercentageComplete": st.Percent}}
	}
	var vl []any
	for _, v := range vols {
		for _, m := range v.Drives {
			if m == c.Name {
				vl = append(vl, ref(storagePath+"/Volumes/"+v.ID))
			}
		}
	}
	doc["Links"] = map[string]any{"Volumes": orEmpty(vl)}
	doc["Actions"] = map[string]any{
		"#Drive.SecureErase": map[string]any{"target": path + "/Actions/Drive.SecureErase",
			"SanitizationType@Redfish.AllowableValues": []any{"CryptographicErase", "BlockErase", "Overwrite"}},
		"#Drive.Reset": map[string]any{"target": path + "/Actions/Drive.Reset", "ResetType@Redfish.AllowableValues": []any{"ForceRestart"}},
	}
	return doc
}

func volumeDoc(path, storagePath string, v engine.VolumeView) map[string]any {
	doc := odata(path, "#Volume.v1_9_0.Volume")
	var drs []any
	for _, m := range v.Drives {
		drs = append(drs, ref(storagePath+"/Drives/"+strings.ToUpper(strings.ReplaceAll(m, "-", "_"))))
	}
	doc["Id"], doc["Name"], doc["RAIDType"], doc["VolumeType"] = v.ID, v.Name, v.RAIDType, volumeTypes[v.RAIDType]
	doc["CapacityBytes"] = v.CapacityBytes
	doc["Links"] = map[string]any{"Drives": orEmpty(drs), "Drives@odata.count": len(drs)}
	doc["Status"] = map[string]any{"Health": v.Health, "State": v.State}
	doc["Encrypted"] = false
	if v.Rebuild >= 0 {
		doc["Operations"] = []any{map[string]any{"OperationName": "Rebuild", "PercentageComplete": v.Rebuild}}
	}
	if v.Pending != "" {
		doc["@Redfish.OperationApplyTime"] = "OnReset"
	}
	doc["Actions"] = map[string]any{"#Volume.Initialize": map[string]any{"target": path + "/Actions/Volume.Initialize"}}
	return doc
}

// ---- requests

// storageError writes an engine refusal in Redfish's shape.
func storageError(w http.ResponseWriter, err error) {
	status, id := http.StatusBadRequest, "Base.1.8.ActionNotSupported"
	switch {
	case errors.Is(err, engine.ErrPrecondition):
		status, id = http.StatusConflict, "Base.1.8.ResourceInUse"
	case errors.Is(err, engine.ErrUnsupported):
		status, id = http.StatusBadRequest, "Base.1.8.PropertyValueNotInList"
	case errors.Is(err, engine.ErrBMCDown):
		status, id = http.StatusServiceUnavailable, "Base.1.8.ServiceTemporarilyUnavailable"
	}
	writeError(w, status, id, err.Error())
}

func taskBody(loc, id, state string) map[string]any {
	return map[string]any{"@odata.id": loc, "@odata.type": "#Task.v1_7_1.Task", "Id": id, "Name": "Task " + id, "TaskState": state, "TaskStatus": "OK",
		"TaskMonitor": TaskMonitorPath + id}
}

// job is where this dialect hands back the job a storage request scheduled: its settings-job
// location (iDRAC's JID_) when it has one, else a TaskService task.
func (b *bmc) job(r *http.Request, path, canonical, message string, src schema.EventSource, takes time.Duration) string {
	if tmpl := b.dialect.Spec.SettingsJobs; tmpl != "" {
		n := 700000000001 + int64(b.a.eng.TaskCount(b.dev.Path))
		loc := strings.ReplaceAll(tmpl, "{n}", fmt.Sprintf("%012d", n))
		_, _ = b.a.eng.CreateTask(b.request(r, path, canonical, src), loc[strings.LastIndex(loc, "/")+1:], message, takes)
		return loc
	}
	t, _ := b.a.eng.CreateTask(b.request(r, path, canonical, src), "", message, takes)
	return "/redfish/v1/TaskService/Tasks/" + t.ID
}

// storagePost answers the storage requests the model has behaviour for; false leaves path to the
// generic handling.
func (b *bmc) storagePost(w http.ResponseWriter, r *http.Request, path string, body map[string]any, src schema.EventSource) bool {
	eng := b.a.eng
	switch {
	case strings.HasSuffix(path, "/Actions/Drive.SecureErase"):
		c := b.driveAt(strings.TrimSuffix(path, "/Actions/Drive.SecureErase"))
		if c == nil {
			return false
		}
		kind, _ := body["SanitizationType"].(string)
		passes := 0
		if n, ok := body["OverwritePasses"].(float64); ok {
			passes = int(n)
		}
		t, err := eng.SecureErase(b.request(r, path, "erase.start", src), c.Name, kind, passes)
		if err != nil {
			storageError(w, err)
			return true
		}
		w.Header().Set("Location", TaskMonitorPath+t.ID)
		writeJSON(w, http.StatusAccepted, taskBody("/redfish/v1/TaskService/Tasks/"+t.ID, t.ID, "Running"))
		return true
	case strings.HasSuffix(path, "/Volumes"):
		set := b.storageAt(strings.TrimSuffix(path, "/Volumes"))
		if set == nil {
			return false
		}
		if set.ctl == nil {
			writeError(w, http.StatusMethodNotAllowed, "Base.1.8.OperationNotAllowed", "The host's directly attached drives have no RAID controller.")
			return true
		}
		spec := engine.VolumeSpec{Controller: set.ctl.Name}
		spec.Name, _ = body["Name"].(string)
		spec.RAIDType, _ = body["RAIDType"].(string)
		if spec.RAIDType == "" {
			if vt, _ := body["VolumeType"].(string); vt != "" {
				for k, v := range volumeTypes {
					if v == vt && (spec.RAIDType == "" || k < spec.RAIDType) {
						spec.RAIDType = k
					}
				}
			}
		}
		if n, ok := body["CapacityBytes"].(float64); ok {
			spec.CapacityBytes = int64(n)
		}
		spec.ApplyTime, _ = body["@Redfish.OperationApplyTime"].(string)
		if links, ok := body["Links"].(map[string]any); ok {
			if arr, ok := links["Drives"].([]any); ok {
				for _, x := range arr {
					m, _ := x.(map[string]any)
					id, _ := m["@odata.id"].(string)
					if c := b.driveAt(normalise(id)); c != nil {
						spec.Drives = append(spec.Drives, c.Name)
					} else {
						writeError(w, http.StatusBadRequest, "Base.1.8.ResourceNotFound", "No drive at "+id)
						return true
					}
				}
			}
		}
		v, task, err := eng.CreateVolume(b.request(r, path, "volume.create", src), spec)
		if err != nil {
			storageError(w, err)
			return true
		}
		if b.dialect.Spec.SettingsJobs != "" { // iDRAC answers a volume create with its job
			loc := b.job(r, path, "volume.create", "create volume "+v.ID, src, 10*time.Second)
			w.Header().Set("Location", loc)
			writeJSON(w, http.StatusAccepted, map[string]any{"@Message.ExtendedInfo": []any{map[string]any{
				"Message": "Successfully scheduled the job.", "MessageArgs": []any{}, "MessageId": "IDRAC.2.9.JCP001", "Severity": "OK"}}})
			return true
		}
		if task != "" {
			w.Header().Set("Location", TaskMonitorPath+task)
			writeJSON(w, http.StatusAccepted, taskBody("/redfish/v1/TaskService/Tasks/"+task, task, "Pending"))
			return true
		}
		loc := path + "/" + v.ID
		w.Header().Set("Location", loc)
		writeJSON(w, http.StatusCreated, map[string]any{"@odata.id": loc, "Id": v.ID, "Name": v.Name, "RAIDType": v.RAIDType, "CapacityBytes": v.CapacityBytes})
		return true
	case strings.HasSuffix(path, "/Actions/Oem/DellStorage.ControllerDrivesDecommission"):
		set := b.storageAt(strings.TrimSuffix(path, "/Actions/Oem/DellStorage.ControllerDrivesDecommission"))
		if set == nil || set.ctl == nil {
			return false
		}
		n := 700000000001 + int64(eng.TaskCount(b.dev.Path))
		tmpl := orDefault(b.dialect.Spec.SettingsJobs, "/redfish/v1/TaskService/Tasks/JID_{n}")
		loc := strings.ReplaceAll(tmpl, "{n}", fmt.Sprintf("%012d", n))
		if _, err := eng.DecommissionController(b.request(r, path, "storage.decommission", src), set.ctl.Name, loc[strings.LastIndex(loc, "/")+1:]); err != nil {
			storageError(w, err)
			return true
		}
		w.Header().Set("Location", loc)
		writeJSON(w, http.StatusAccepted, map[string]any{"@Message.ExtendedInfo": []any{map[string]any{
			"Message": "Successfully scheduled the job.", "MessageArgs": []any{}, "MessageId": "IDRAC.2.9.JCP001", "Severity": "OK"}}})
		return true
	case strings.HasSuffix(path, "/Actions/DellLCService.GetRemoteServicesAPIStatus"):
		// the Lifecycle Controller is ready while the BMC is up and no job is running
		eng.LogRead(b.request(r, path, "health.read", src), 200)
		writeJSON(w, http.StatusOK, map[string]any{"LCStatus": "Ready", "RTStatus": "Ready", "ServerStatus": "OutOfPOST", "Status": "Ready",
			"TelemetryStatus": "Ready", "@Message.ExtendedInfo": []any{map[string]any{"MessageId": "IDRAC.2.9.LC061", "Message": "Lifecycle Controller Remote Services is ready.", "Severity": "Informational"}}})
		return true
	case strings.HasSuffix(path, "/Actions/Volume.Initialize"):
		vp := strings.TrimSuffix(path, "/Actions/Volume.Initialize")
		if i := strings.Index(vp, "/Volumes/"); i < 0 || b.storageAt(vp[:i]) == nil {
			return false
		}
		eng.LogRead(b.request(r, path, "volume.initialize", src), 204)
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// storageDelete answers a DELETE of a model volume; false leaves path to the generic handling.
func (b *bmc) storageDelete(w http.ResponseWriter, r *http.Request, path string, src schema.EventSource) bool {
	i := strings.Index(path, "/Volumes/")
	if i < 0 || b.storageAt(path[:i]) == nil {
		return false
	}
	body := readBody(r)
	at, _ := body["@Redfish.OperationApplyTime"].(string)
	task, err := b.a.eng.DeleteVolume(b.request(r, path, "volume.delete", src), path[i+len("/Volumes/"):], at)
	if err != nil {
		storageError(w, err)
		return true
	}
	if task != "" {
		w.Header().Set("Location", TaskMonitorPath+task)
		writeJSON(w, http.StatusAccepted, taskBody("/redfish/v1/TaskService/Tasks/"+task, task, "Pending"))
		return true
	}
	w.WriteHeader(http.StatusNoContent)
	return true
}

// TaskMonitorPath is where a task's monitor is (Redfish Specification, Asynchronous operations): a
// GET answers 202 with the task while it is pending or running, and 200 with it once it has ended,
// as sushy's TaskMonitor (and through it Ironic's RAID interface) polls it.
const TaskMonitorPath = "/redfish/v1/TaskService/TaskMonitors/"

func (b *bmc) taskMonitor(w http.ResponseWriter, r *http.Request, path string) {
	id := strings.TrimPrefix(path, TaskMonitorPath)
	rd := &renderer{b: b, snap: b.snapshot()}
	doc, ok := rd.dynamic("/redfish/v1/TaskService/Tasks/" + id)
	if !ok || doc == nil {
		b.logRead(r, path, 404)
		writeError(w, http.StatusNotFound, "Base.1.8.ResourceNotFound", "No task monitor at "+path)
		return
	}
	b.logRead(r, path, 200)
	switch doc["TaskState"] {
	case "New", "Pending", "Running", "Starting":
		w.Header().Set("Location", path)
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusAccepted, doc)
	default:
		writeJSON(w, http.StatusOK, doc)
	}
}

// durableName is an EUI-64 or NAA name as Resource.Identifier's pattern wants it: eight
// colon-separated hex pairs (the model keeps 16 hex digits, perhaps with 0x).
func durableName(eui string) string {
	h := strings.ToUpper(strings.TrimPrefix(strings.ToLower(eui), "0x"))
	if len(h) != 16 {
		return strings.ToUpper(eui)
	}
	var p []string
	for i := 0; i < 16; i += 2 {
		p = append(p, h[i:i+2])
	}
	return strings.Join(p, ":")
}

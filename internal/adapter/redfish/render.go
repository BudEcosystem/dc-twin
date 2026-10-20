package redfish

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// identity properties scrubbed in every captured tree (D26: no captured device's identity is
// served; equal captured values stay equal).
var scrubKeys = map[string]string{
	"SerialNumber": "serial", "UUID": "uuid", "MACAddress": "mac", "PermanentMACAddress": "mac",
	"HostName": "hostname", "FQDN": "hostname", "ServiceIdentification": "serial",
}

// renderer turns a captured resource into what this device's BMC answers now.
type renderer struct {
	b    *bmc
	snap snapshot
}

// snapshot is the model state a render reads, taken once per request under the read lock.
type snapshot struct {
	power, health, siteIP, hostname string
	ac, bmcUp                       bool
	boot                            model.BootState
	bios, biosPending               map[string]any
	accounts                        []model.Account
	sessions                        []model.Session
	logs                            []model.LogEntry
	tasks                           []model.Task
	subs                            []model.Subscription
	patches                         map[string]map[string]any
	firmware                        map[string]string
	comps                           []compState
	sensors                         map[string]float64
	faultSets                       map[string]any
	secureBoot, lockdown            bool
	slotIndex                       int
	model                           string
	now                             time.Time
	booted                          string
	powerSince, bootedSince         time.Time
	bmcClock                        time.Duration
}

type compState struct {
	group, name   string
	index         int
	health, state string
}

func (b *bmc) snapshot() snapshot {
	e := b.a.eng
	var s snapshot
	e.Read(func(m *model.Model) {
		d := b.dev
		st := &d.State
		s.power, s.ac, s.siteIP = st.Power, st.AC, d.Endpoint.SiteIP
		s.booted, s.powerSince, s.bootedSince = st.Booted, st.PowerSince, st.BootedSince
		s.bmcClock = st.BMCClock
		s.boot = st.Boot
		s.boot.Order = append([]string{}, st.Boot.Order...)
		s.bios, s.biosPending = copyMap(st.Bios), copyMap(st.BiosPending)
		s.accounts = append([]model.Account{}, st.Accounts...)
		s.logs = append([]model.LogEntry{}, st.Log...)
		s.tasks = append([]model.Task{}, st.Tasks...)
		s.subs = append([]model.Subscription{}, st.Subscribers...)
		s.patches = map[string]map[string]any{}
		for k, v := range st.Patches {
			s.patches[k] = copyMap(v)
		}
		s.firmware = map[string]string{}
		for k, v := range st.Firmware {
			s.firmware[k] = v.Version
		}
		s.secureBoot, s.lockdown = st.SecureBoot, st.Lockdown
		s.slotIndex = d.SlotIndex
		if d.Profile.Spec.Identity != nil {
			s.model = d.Profile.Spec.Identity.Model
		}
	})
	s.sessions = e.Sessions(b.dev)
	s.health = e.Health(b.dev)
	up, _, _ := e.BMCBehaviour(b.dev)
	s.bmcUp = up
	for _, c := range b.dev.Components {
		h, st := e.ComponentHealth(c)
		s.comps = append(s.comps, compState{group: c.Group, name: c.Name, index: c.Index, health: h, state: st})
	}
	s.sensors = e.Sensors(b.dev)
	s.faultSets = e.FaultSets(b.dev)
	s.hostname = b.hostname
	s.now = e.Now()
	return s
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func redfishPower(p string) string {
	switch p {
	case model.On, model.Off, model.PoweringOn, model.PoweringOff:
		return p
	}
	return "Off"
}

func redfishState(power string, ac bool) string {
	if !ac {
		return "Absent"
	}
	if power == model.On {
		return "Enabled"
	}
	return "StandbyOffline"
}

// render produces the document at path, or false when this BMC has no such resource.
func (r *renderer) render(path string) (map[string]any, bool) {
	b := r.b
	if dyn, ok := r.dynamic(path); ok {
		return dyn, true
	}
	if fw, ok := r.firmwareBootDoc(path); ok { // a vm host's firmware's own (vmboot.go)
		return fw, true
	}
	dpu, isDPU := b.dpuIndex(path)
	if isDPU && dpu >= len(b.dpus) {
		return nil, false // the capture's host has a DPU this model's does not
	}
	doc, ok := b.tree.get(path)
	if !ok {
		return nil, false
	}
	r.scrub(doc, path)
	if path == "/redfish/v1" {
		for k, v := range b.dialect.Spec.ServiceRoot {
			doc[k] = v
		}
	}
	// PATCHed properties persist (owned state), under the model's bindings below
	if p, ok := r.snap.patches[path]; ok {
		deepMerge(doc, p)
	}
	r.bind(path, doc)
	r.firmwareBootOrder(path, doc) // a vm host's boot order: its firmware's (vmboot.go)
	for _, o := range b.dialect.Spec.Overlays {
		if ok, _ := matchPath(o.Path, path); !ok {
			continue
		}
		v := o.Value
		if o.From != "" {
			v = r.from(o.From)
		}
		setDotted(doc, o.Set, v, strings.ContainsAny(o.Path, "*{"))
	}
	for k, v := range r.snap.faultSets {
		if strings.HasPrefix(k, "redfish:") {
			ref := strings.TrimPrefix(k, "redfish:")
			if p, prop, ok := strings.Cut(ref, "#"); ok && p == path {
				setDotted(doc, prop, v, false)
			}
		}
	}
	if isDPU {
		setSerials(doc, b.dpus[dpu].Identity.Serial)
	}
	b.dropAbsentDPUs(doc)
	return doc, true
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				deepMerge(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
}

func (r *renderer) from(src string) any {
	s := r.snap
	switch src {
	case "power":
		return redfishPower(s.power)
	case "health":
		return s.health
	case "slotIndex":
		return s.slotIndex
	case "model":
		return s.model
	case "serial":
		return r.b.dev.Identity.Serial
	case "uuid":
		return r.b.dev.Identity.UUID
	case "siteIP":
		return s.siteIP
	}
	return nil
}

// scrub replaces captured identities with the device's derived ones, recursively.
func (r *renderer) scrub(v any, path string) {
	b := r.b
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if kind, ok := scrubKeys[k]; ok {
				if s, ok := val.(string); ok && s != "" {
					x[k] = b.scrubValue(kind, s)
				}
				continue
			}
			if k == "BaseMAC" { // BlueField's OEM base (host PF) MAC, written without separators
				if s, ok := val.(string); ok && compactMAC.MatchString(s) {
					x[k] = b.scrubCompactMAC(s)
				}
				continue
			}
			if k == "AssociatedNetworkAddresses" {
				if arr, ok := val.([]any); ok {
					for i, a := range arr {
						if s, ok := a.(string); ok {
							arr[i] = b.scrubValue("mac", s)
						}
					}
				}
				continue
			}
			if k == "IPv4Addresses" || k == "IPv4StaticAddresses" || k == "IPv6Addresses" || k == "IPv6StaticAddresses" {
				x[k] = r.addresses(k, path, val)
				continue
			}
			if s, ok := val.(string); ok {
				if strings.Contains(s, "MAC(") { // a UEFI device path names its NIC by MAC
					s = devicePathMAC.ReplaceAllStringFunc(s, func(m string) string {
						return "MAC(" + b.scrubCompactMAC(m[4:])
					})
					x[k] = s
				}
				for from, to := range b.dialect.Spec.Replace {
					if strings.Contains(s, from) {
						x[k] = strings.ReplaceAll(s, from, b.expand(to, r.snap))
					}
				}
				continue
			}
			r.scrub(val, path)
		}
	case []any:
		for _, el := range x {
			r.scrub(el, path)
		}
	}
}

// addresses answers the BMC interface's addresses from its lease; every other captured
// address is withheld.
func (r *renderer) addresses(key, path string, val any) any {
	if path != r.b.dialect.Spec.BMCPort || !strings.HasPrefix(key, "IPv4") {
		return []any{}
	}
	if r.snap.siteIP == "" {
		return []any{}
	}
	origin := "DHCP"
	if key == "IPv4StaticAddresses" {
		return []any{}
	}
	gw, mask := "", "255.255.255.0"
	if l := r.b.lease(); l != nil {
		gw = l.Gateway
	}
	return []any{map[string]any{"Address": r.snap.siteIP, "AddressOrigin": origin, "SubnetMask": mask, "Gateway": gw}}
}

func (b *bmc) scrubValue(kind, s string) string {
	if v, ok := b.orig[kind]; ok && v == s {
		switch kind {
		case "serial":
			return b.dev.Identity.Serial
		case "uuid":
			return b.dev.Identity.UUID
		}
	}
	if kind == "mac" && strings.EqualFold(b.orig["bmcMAC"], s) {
		if s == strings.ToUpper(s) {
			return strings.ToUpper(b.dev.Identity.BMCMAC)
		}
		return b.dev.Identity.BMCMAC
	}
	if kind == "hostname" {
		return b.hostname
	}
	return b.a.id.Scrub(kind, b.dev.Path, b.dev.Identity.ProfileMajor, s)
}

func (b *bmc) expand(s string, snap snapshot) string {
	s = strings.ReplaceAll(s, "{siteIP}", snap.siteIP)
	return strings.ReplaceAll(s, "{hostname}", b.hostname)
}

// bind applies the model to the resource: power and health, boot, components, sensors, leak
// detectors, firmware versions and BIOS attributes.
func (r *renderer) bind(path string, doc map[string]any) {
	b, s := r.b, r.snap
	spec := b.dialect.Spec
	isSystem := strings.HasPrefix(path, "/redfish/v1/Systems/") && strings.Count(path, "/") == 4
	isChassis := strings.HasPrefix(path, "/redfish/v1/Chassis/") && strings.Count(path, "/") == 4
	if isSystem || isChassis {
		if _, ok := doc["PowerState"]; ok {
			ps := redfishPower(s.power)
			if b.dev.BaseClass == "power-shelf" && isChassis && !s.ac {
				ps = "Off"
			}
			doc["PowerState"] = ps
		}
	}
	if _, ok := doc["DateTime"]; ok && isManager(path) {
		doc["DateTime"] = bmcDateTime(s, doc["DateTimeLocalOffset"])
	}
	if path == spec.System {
		doc["PowerState"] = redfishPower(s.power)
		if bp, ok := doc["BootProgress"].(map[string]any); ok {
			bp["LastState"], bp["LastStateTime"] = bootProgress(s)
		}
		if st, ok := doc["Status"].(map[string]any); ok {
			st["Health"] = s.health
			st["State"] = redfishState(s.power, s.ac)
			if _, ok := st["HealthRollup"]; ok {
				st["HealthRollup"] = s.health
			}
		}
		if boot, ok := doc["Boot"].(map[string]any); ok {
			boot["BootSourceOverrideTarget"] = s.boot.OverrideTarget
			boot["BootSourceOverrideEnabled"] = s.boot.OverrideEnabled
			if s.boot.OverrideMode != "" {
				boot["BootSourceOverrideMode"] = s.boot.OverrideMode
			}
			if s.boot.HTTPBootURI != "" {
				boot["HttpBootUri"] = s.boot.HTTPBootURI
			}
			if _, captured := boot["BootOrder"]; captured || len(s.boot.Order) > 0 {
				order := make([]any, len(s.boot.Order))
				for i, o := range s.boot.Order {
					order[i] = o
				}
				boot["BootOrder"] = order
			}
		}
	}
	// the system's BiosVersion and the manager's FirmwareVersion follow the model wherever it
	// has the version (Ironic and MAAS read them there), with or without an inventory binding
	{
		if path == spec.System {
			if _, ok := doc["BiosVersion"]; ok && s.firmware["bios"] != "" {
				doc["BiosVersion"] = s.firmware["bios"]
			}
		}
		if path == spec.Manager {
			if _, ok := doc["FirmwareVersion"]; ok && s.firmware["bmc"] != "" {
				doc["FirmwareVersion"] = s.firmware["bmc"]
			}
		}
	}
	if path == spec.Chassis {
		if st, ok := doc["Status"].(map[string]any); ok {
			st["Health"] = s.health
			if _, ok := st["HealthRollup"]; ok {
				st["HealthRollup"] = s.health
			}
		}
	}
	if path == spec.BMCPort {
		doc["MACAddress"] = b.dev.Identity.BMCMAC
		if _, ok := doc["HostName"]; ok {
			doc["HostName"] = b.hostname
		}
	}
	for _, cb := range spec.Components {
		for _, res := range cb.Resources {
			ok, idx := matchPath(res, path)
			if !ok {
				continue
			}
			for _, c := range s.comps {
				if c.group == cb.Component && (idx < 0 || c.index == idx) {
					if st, ok := doc["Status"].(map[string]any); ok {
						st["Health"] = c.health
						st["State"] = c.state
						if _, ok := st["HealthRollup"]; ok {
							st["HealthRollup"] = c.health
						}
					}
				}
			}
		}
	}
	for _, sb := range spec.Sensors {
		for _, res := range sb.Resources {
			if ok, _ := matchPath(res, path); ok {
				if v, ok := s.sensors[sb.Sensor]; ok {
					prop := sb.Property
					if prop == "" {
						prop = "Reading"
					}
					setDotted(doc, prop, v, false)
				}
			}
		}
	}
	for _, res := range spec.Leak {
		if ok, _ := matchPath(res, path); ok {
			state := "OK"
			if s.sensors["leak"] >= 1 {
				state = "Critical"
			}
			doc["DetectorState"] = state
			if st, ok := doc["Status"].(map[string]any); ok {
				st["Health"] = state
			}
		}
	}
	if strings.HasPrefix(path, "/redfish/v1/UpdateService/FirmwareInventory/") {
		id := path[strings.LastIndex(path, "/")+1:]
		if comp, ok := spec.Firmware[id]; ok {
			if v, ok := s.firmware[comp]; ok {
				doc["Version"] = v
			}
		}
	}
	if strings.HasSuffix(strings.ToLower(path), "/bios") {
		if attrs, ok := doc["Attributes"].(map[string]any); ok {
			if s.biosPending["__reset__"] == nil {
				for k, v := range s.bios {
					attrs[k] = v
				}
			}
		}
	}
	if lp := strings.ToLower(path); strings.HasSuffix(lp, "/bios/settings") || strings.HasSuffix(lp, "/bios/sd") || strings.HasSuffix(lp, "/bios/pending") {
		// the pending settings object: staged values over whatever the capture holds there
		// (iLO's settings object mirrors every attribute; iDRAC's holds only pending ones)
		attrs, _ := doc["Attributes"].(map[string]any)
		if attrs == nil {
			attrs = map[string]any{}
		}
		// a settings object that mirrors every attribute shows the applied values once a reset
		// has applied them, then whatever is still staged
		if s.biosPending["__reset__"] == nil {
			for k, v := range s.bios {
				if _, mirrored := attrs[k]; mirrored {
					attrs[k] = v
				}
			}
		}
		for k, v := range s.biosPending {
			if k != "__reset__" {
				attrs[k] = v
			}
		}
		doc["Attributes"] = attrs
	}
	if strings.HasSuffix(path, "/SecureBoot") {
		if _, ok := doc["SecureBootEnable"]; ok {
			doc["SecureBootEnable"] = s.secureBoot
		}
	}
}

// dynamic renders collections and members whose contents are model state: accounts,
// sessions, log entries the twin wrote, tasks and event subscriptions.
func (r *renderer) dynamic(path string) (map[string]any, bool) {
	s := r.snap
	coll := func(odataType, name string, ids []string, base string) map[string]any {
		members := []any{}
		for _, id := range ids {
			members = append(members, map[string]any{"@odata.id": base + "/" + id})
		}
		return map[string]any{"@odata.id": base, "@odata.type": odataType, "Name": name, "Members": members, "Members@odata.count": len(members)}
	}
	switch {
	case path == "/redfish/v1/AccountService/Accounts":
		var ids []string
		for _, a := range s.accounts {
			ids = append(ids, a.ID)
		}
		doc := coll("#ManagerAccountCollection.ManagerAccountCollection", "Accounts Collection", ids, path)
		if base, ok := r.b.tree.get(path); ok {
			if t, ok := base["@odata.type"]; ok {
				doc["@odata.type"] = t
			}
		}
		return doc, true
	case strings.HasPrefix(path, "/redfish/v1/AccountService/Accounts/"):
		id := strings.TrimPrefix(path, "/redfish/v1/AccountService/Accounts/")
		for _, a := range s.accounts {
			if a.ID != id {
				continue
			}
			doc, ok := r.b.tree.get(path)
			if !ok {
				doc = map[string]any{"@odata.type": "#ManagerAccount.v1_10_0.ManagerAccount"}
			}
			doc["@odata.id"], doc["Id"], doc["Name"] = path, a.ID, "User Account"
			doc["UserName"], doc["RoleId"], doc["Enabled"], doc["Locked"], doc["Password"] = a.User, a.Role, a.Enabled, a.Locked, nil
			doc["PasswordChangeRequired"] = a.PasswordChangeRequired
			doc["Links"] = map[string]any{"Role": map[string]any{"@odata.id": "/redfish/v1/AccountService/Roles/" + a.Role}}
			return doc, true
		}
		return nil, true
	case path == "/redfish/v1/SessionService/Sessions":
		var ids []string
		for _, x := range s.sessions {
			ids = append(ids, x.ID)
		}
		return coll("#SessionCollection.SessionCollection", "Session Collection", ids, path), true
	case strings.HasPrefix(path, "/redfish/v1/SessionService/Sessions/"):
		id := strings.TrimPrefix(path, "/redfish/v1/SessionService/Sessions/")
		for _, x := range s.sessions {
			if x.ID == id {
				return map[string]any{"@odata.id": path, "@odata.type": "#Session.v1_7_0.Session", "Id": x.ID, "Name": "User Session", "UserName": x.User, "Password": nil}, true
			}
		}
		return nil, true
	case path == "/redfish/v1/TaskService/Tasks":
		var ids []string
		for _, t := range s.tasks {
			ids = append(ids, t.ID)
		}
		return coll("#TaskCollection.TaskCollection", "Task Collection", ids, path), true
	case strings.HasPrefix(path, "/redfish/v1/TaskService/Tasks/"):
		id := strings.TrimPrefix(path, "/redfish/v1/TaskService/Tasks/")
		for _, t := range s.tasks {
			if t.ID == id {
				state := t.State
				doc := map[string]any{"@odata.id": path, "@odata.type": "#Task.v1_7_1.Task", "Id": t.ID, "Name": "Task " + t.ID,
					"TaskState": state, "TaskStatus": taskStatus(state), "PercentComplete": t.Percent, "StartTime": t.Started.Format(time.RFC3339),
					"Messages": []any{map[string]any{"Message": t.Message, "MessageId": taskMessageID(state)}}}
				if !t.Ends.IsZero() {
					doc["EndTime"] = t.Ends.Format(time.RFC3339)
				}
				return doc, true
			}
		}
		return nil, true
	case path == "/redfish/v1/EventService/Subscriptions":
		var ids []string
		for _, x := range s.subs {
			ids = append(ids, x.ID)
		}
		return coll("#EventDestinationCollection.EventDestinationCollection", "Event Subscriptions", ids, path), true
	case strings.HasPrefix(path, "/redfish/v1/EventService/Subscriptions/"):
		id := strings.TrimPrefix(path, "/redfish/v1/EventService/Subscriptions/")
		for _, x := range s.subs {
			if x.ID == id {
				return map[string]any{"@odata.id": path, "@odata.type": "#EventDestination.v1_13_0.EventDestination", "Id": x.ID, "Name": "Subscription " + x.ID,
					"Destination": x.Destination, "Protocol": "Redfish", "Context": x.Context, "SubscriptionType": "RedfishEvent", "EventFormatType": "Event"}, true
			}
		}
		return nil, true
	}
	if svc, entry, ok := r.b.logPath(path); ok {
		return r.logResource(path, svc, entry)
	}
	return nil, false
}

// taskMessageID is the TaskEvent message a task's state ends with (DMTF's TaskEvent registry):
// an update a BMC lost when it went down ends in Exception with TaskAborted (D53).
func taskMessageID(state string) string {
	switch state {
	case "Completed":
		return "TaskEvent.1.0.TaskCompletedOK"
	case "Exception", "Killed":
		return "TaskEvent.1.0.TaskAborted"
	}
	return "TaskEvent.1.0.TaskProgressChanged"
}

func taskStatus(state string) string {
	switch state {
	case "Exception", "Killed":
		return "Critical"
	}
	return "OK"
}

// logPath recognises the system event log the twin writes to (SEL or EventLog).
func (b *bmc) logPath(path string) (service, entry string, ok bool) {
	for _, svc := range []string{"SEL", "EventLog", "Sel"} {
		for _, root := range []string{b.dialect.Spec.System, b.dialect.Spec.Manager} {
			if root == "" {
				continue
			}
			base := root + "/LogServices/" + svc + "/Entries"
			if path == base {
				return base, "", true
			}
			if strings.HasPrefix(path, base+"/twin-") {
				return base, strings.TrimPrefix(path, base+"/"), true
			}
		}
	}
	return "", "", false
}

func (r *renderer) logResource(path, base, entry string) (map[string]any, bool) {
	logs := r.snap.logs
	if entry != "" {
		n, _ := strconv.Atoi(strings.TrimPrefix(entry, "twin-"))
		for _, l := range logs {
			if l.ID == n {
				return logEntry(base+"/"+entry, entry, l), true
			}
		}
		return nil, true
	}
	doc, ok := r.b.tree.get(path)
	if !ok {
		doc = map[string]any{"@odata.id": path, "@odata.type": "#LogEntryCollection.LogEntryCollection", "Name": "Log Entries", "Members": []any{}}
	}
	r.scrub(doc, path)
	members, _ := doc["Members"].([]any)
	if _, cleared := r.snap.patches["twin:cleared:"+base]; cleared {
		members = nil
	}
	for _, l := range logs {
		members = append(members, logEntry(base+"/twin-"+strconv.Itoa(l.ID), "twin-"+strconv.Itoa(l.ID), l))
	}
	if members == nil {
		members = []any{}
	}
	doc["Members"] = members
	doc["Members@odata.count"] = len(members)
	return doc, true
}

func logEntry(path, id string, l model.LogEntry) map[string]any {
	sev := l.Severity
	if sev == "" {
		sev = "OK"
	}
	return map[string]any{"@odata.id": path, "@odata.type": "#LogEntry.v1_15_0.LogEntry", "Id": id, "Name": "Log Entry " + id,
		"Created": l.Created.Format(time.RFC3339), "EntryType": "Event", "Severity": sev, "Message": l.Message, "MessageId": l.MessageID}
}

// collectionMembers returns the member ids a dynamic collection has, sorted.
func sortedIDs(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var _ = engine.ErrNoDevice
var _ = schema.APIVersion
var _ = fmt.Sprint

// bootProgress is ComputerSystem.BootProgress from the model, as a BMC reports it: None while
// off, processor initialisation while powering on, hardware initialisation complete while the
// firmware is network booting (or stuck there), OSBootStarted once a walked network boot has
// handed over to its kernel, and OSRunning once the host has booted its OS. The time is when the
// model last changed the state it is derived from.
func bootProgress(s snapshot) (string, string) {
	at := s.bootedSince
	state := "SystemHardwareInitializationComplete"
	switch {
	case s.power == model.Off:
		state, at = "None", s.powerSince
	case s.power == model.PoweringOn:
		state, at = "PrimaryProcessorInitializationStarted", s.powerSince
	case s.booted == "os":
		state = "OSRunning"
	case s.booted == "booted":
		state = "OSBootStarted"
	case s.booted == "post":
		state = "MemoryInitializationStarted"
	}
	if at.IsZero() {
		at = s.powerSince
	}
	if at.IsZero() {
		at = s.now
	}
	return state, at.UTC().Format("2006-01-02T15:04:05+00:00")
}

// bmcDateTime is the BMC's clock now (the twin's clock plus what a stack set it to), in the
// Manager's DateTimeLocalOffset ("+05:30", "-05:00", "Z"; UTC when absent or unreadable). A
// captured DateTime is a moment of the capture: served as is, every BMC's clock would stand still
// and a stack that checks drift (NICo allows 300 s) would never find one in sync.
func bmcDateTime(s snapshot, offset any) string {
	t := s.now.Add(s.bmcClock)
	loc := time.UTC
	if o, _ := offset.(string); o != "" && o != "Z" {
		if z, err := time.Parse("-07:00", o); err == nil {
			_, secs := z.Zone()
			loc = time.FixedZone(o, secs)
		}
	}
	return t.In(loc).Format("2006-01-02T15:04:05-07:00")
}

// dpuIndex is the index of the host DPU whose captured resources path is (or is under), by the
// dialect's dpus patterns.
func (b *bmc) dpuIndex(path string) (int, bool) {
	if b.a.captureDPUs {
		return 0, false
	}
	for _, pat := range b.dialect.Spec.DPUs {
		n := strings.Count(pat, "/")
		if strings.Count(path, "/") < n {
			continue
		}
		prefix := path
		if k := nthIndex(path, '/', n+1); k >= 0 {
			prefix = path[:k]
		}
		if ok, idx := matchPath(pat, prefix); ok {
			if idx < 0 {
				idx = 0
			}
			return idx, true
		}
	}
	return 0, false
}

// nthIndex is the index of the nth occurrence of c in s (1-based), or -1.
func nthIndex(s string, c byte, n int) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			n--
			if n == 0 {
				return i
			}
		}
	}
	return -1
}

// dropAbsentDPUs removes from a collection the members that are DPUs the model's host lacks.
func (b *bmc) dropAbsentDPUs(doc map[string]any) {
	members, ok := doc["Members"].([]any)
	if !ok || len(b.dialect.Spec.DPUs) == 0 {
		return
	}
	kept := members[:0:0]
	for _, m := range members {
		if mm, ok := m.(map[string]any); ok {
			if id, _ := mm["@odata.id"].(string); id != "" {
				if idx, isDPU := b.dpuIndex(normalise(id)); isDPU && idx >= len(b.dpus) {
					continue
				}
			}
		}
		kept = append(kept, m)
	}
	if len(kept) != len(members) {
		doc["Members"] = kept
		if _, ok := doc["Members@odata.count"]; ok {
			doc["Members@odata.count"] = len(kept)
		}
	}
}

// setSerials sets every SerialNumber in a document (and its nested objects) to serial.
func setSerials(v any, serial string) {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if k == "SerialNumber" {
				if _, ok := x.(string); ok {
					t[k] = serial
					continue
				}
			}
			setSerials(x, serial)
		}
	case []any:
		for _, x := range t {
			setSerials(x, serial)
		}
	}
}

var (
	compactMAC    = regexp.MustCompile(`^[0-9A-Fa-f]{12}$`)
	devicePathMAC = regexp.MustCompile(`MAC\([0-9A-Fa-f]{12}`)
)

// scrubCompactMAC scrubs a MAC written without separators ("5c257308f352", "5C257308F362" in a
// UEFI device path) as the same MAC written with colons in lower case is scrubbed elsewhere in
// the tree, so one NIC keeps one derived MAC however the capture spells it.
func (b *bmc) scrubCompactMAC(s string) string {
	l := strings.ToLower(s)
	colon := l[0:2] + ":" + l[2:4] + ":" + l[4:6] + ":" + l[6:8] + ":" + l[8:10] + ":" + l[10:12]
	out := strings.ReplaceAll(b.scrubValue("mac", colon), ":", "")
	if s != l {
		return strings.ToUpper(out)
	}
	return out
}

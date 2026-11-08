package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Shared storage systems (docs/reviews/2026-11-08-storage-deep-dive.md 5.4, S-Q1): what a system
// holds (file systems with their exports, volumes with their host mappings, buckets, snapshots)
// is the engine's owned state, so a volume made through one API is read back through Swordfish,
// the exporter, the console, faults and scenario assertions alike. Its health, the node serving
// each object, its used capacity and its alerts are derived from its nodes, its drives, its
// objects and the faults on it.

var (
	ErrNoStorageSystem = errors.New("no such storage system")
	ErrNoStorageObject = errors.New("no such storage object")
	ErrCapacity        = errors.New("insufficient capacity")
	ErrExists          = errors.New("already exists")
)

// StorageNodeView is one node or enclosure of a system as the system sees it.
type StorageNodeView struct {
	Path, Name string
	Up         bool
	Why        string // why it is not serving
	Serving    int    // objects it serves now
}

// StorageObjectView is a file system, volume, bucket or snapshot with what is derived of it.
type StorageObjectView struct {
	Kind          string // filesystem, volume, bucket, snapshot
	ID, Name      string
	Home          string // the node it lives on
	Node          string // the node serving it now ("" when none can)
	CapacityBytes int64  // a volume's size, a file system's or bucket's quota (0: unlimited)
	UsedBytes     int64
	Online        bool
	Status        string // Online, Offline, FailingOver
	Health        string
	Exports       []model.Export
	Path          string // a file system's
	Quota         bool   // a file system has a quota object
	Hosts         []string
	NQN, NGUID    string
	Of            string
	Created       time.Time
}

// StorageAlert is one condition a system reports.
type StorageAlert struct {
	ID, Severity, Message, Object string
}

// StorageSystemView is a storage system with everything derived of it.
type StorageSystemView struct {
	Name, Path, Template, Manager, Fidelity string
	Protocols                               []string
	Health                                  string // OK, Warning, Critical
	State                                   string // Online, Degraded, Offline
	RawBytes, UsableBytes                   int64
	ProvisionedBytes, UsedBytes             int64
	UsedPercent                             float64
	FullPercent                             int
	DataUnits, ParityUnits                  int
	Nodes, Enclosures                       []StorageNodeView
	Drives, DrivesFailed                    int
	DevicesDown                             int // devices holding pool drives without power
	FileSystems, Volumes, Buckets           []StorageObjectView
	Snapshots                               []StorageObjectView
	Alerts                                  []StorageAlert
	Hosts                                   []string
	Endpoints, Addresses                    map[string]string
	DataNetwork, ManagementNetwork          string
	Provides, IsNot                         []string
	Params                                  map[string]any
}

// storageNodeDown is when each storage node stopped serving, for failover (storageTick).
type storageNodeDown struct {
	since    time.Time
	failover bool // its objects have moved
}

func (e *Engine) system(path string) (*model.StorageSystem, error) {
	if s := e.M.StorageBy[path]; s != nil {
		return s, nil
	}
	for _, s := range e.M.StorageSystems { // by name too
		if s.Name == path || strings.TrimPrefix(path, "storage/") == s.Name {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%w %q", ErrNoStorageSystem, path)
}

// StorageSystems returns every system's view, in estate order.
func (e *Engine) StorageSystems() []StorageSystemView {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]StorageSystemView, 0, len(e.M.StorageSystems))
	for _, s := range e.M.StorageSystems {
		out = append(out, e.storageView(s))
	}
	return out
}

// StorageSystem returns one system's view by path or name.
func (e *Engine) StorageSystem(path string) (StorageSystemView, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	s, err := e.system(path)
	if err != nil {
		return StorageSystemView{}, err
	}
	return e.storageView(s), nil
}

// SetStorageEndpoint records where this machine serves a system (api, metrics, nfs …) and the
// site address it is reached at (management, data), as SetEndpoint does for a device.
func (e *Engine) SetStorageEndpoint(path, kind, addr string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.M.StorageBy[path]; s != nil {
		s.Endpoints[kind] = addr
	}
}

// SetStorageAddress records a system's site address of a kind (management, data).
func (e *Engine) SetStorageAddress(path, kind, addr string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.M.StorageBy[path]; s != nil {
		s.Addresses[kind] = addr
	}
}

// ---------------------------------------------------------------- derived

func (e *Engine) storageFaults(s *model.StorageSystem) []*ActiveFault {
	var out []*ActiveFault
	for _, f := range e.faults {
		if f.System == s {
			out = append(out, f)
		}
	}
	return out
}

// nodeUp says whether a storage node serves: powered, not failed, its storage service not
// faulted.
func (e *Engine) nodeUp(s *model.StorageSystem, d *model.Device) (bool, string) {
	switch {
	case !d.State.AC:
		return false, "no input power"
	case d.State.Power != model.On:
		return false, "powered " + strings.ToLower(d.State.Power)
	case e.deviceHealth(d) == model.Critical:
		return false, "failed (health Critical)"
	}
	for _, f := range e.storageFaults(s) {
		if n, ok := f.Sets["storage.nodeDown"].(string); ok && n != "" && nodeNamed(s, d, n) {
			return false, "storage service failed (" + f.Fault + ")"
		}
	}
	return true, ""
}

// nodeNamed is whether a fault's node parameter names d: its path, a rack-qualified suffix
// (s01/node-02), or its bare name when no other node of the system has it (racks reuse slot names).
func nodeNamed(s *model.StorageSystem, d *model.Device, n string) bool {
	if n == d.Path || (strings.Contains(n, "/") && strings.HasSuffix(d.Path, "/"+n)) {
		return true
	}
	if n != d.Name {
		return false
	}
	same := 0
	for _, x := range append(append([]*model.Device{}, s.Nodes...), s.Enclosures...) {
		if x.Name == n {
			same++
		}
	}
	return same == 1
}

// servingNode is the node serving an object homed on home: home while it is up; once home has
// been down for the system's failoverSeconds, the next node that is up; none in between.
func (e *Engine) servingNode(s *model.StorageSystem, home string, up map[string]bool) (string, string) {
	if up[home] {
		return home, "Online"
	}
	if dn, ok := e.storageDown[home]; !ok || !dn.failover {
		return "", "FailingOver"
	}
	i := slices.IndexFunc(s.Nodes, func(d *model.Device) bool { return d.Path == home })
	for k := 1; k <= len(s.Nodes); k++ {
		n := s.Nodes[(i+k+len(s.Nodes))%len(s.Nodes)]
		if up[n.Path] {
			return n.Path, "Online"
		}
	}
	return "", "Offline"
}

func (e *Engine) storageView(s *model.StorageSystem) StorageSystemView {
	v := StorageSystemView{Name: s.Name, Path: s.Path, Template: s.Template, Manager: s.Manager, Fidelity: s.Fidelity,
		Protocols: s.Protocols, RawBytes: s.RawBytes, UsableBytes: s.UsableBytes, FullPercent: s.FullPercent,
		DataUnits: s.DataUnits, ParityUnits: s.ParityUnits, Endpoints: copyStrings(s.Endpoints), Addresses: copyStrings(s.Addresses),
		DataNetwork: s.DataNetwork, ManagementNetwork: s.ManagementNetwork, Provides: s.Provides, IsNot: s.IsNot, Params: s.Params}
	faults := e.storageFaults(s)
	up := map[string]bool{}
	nodesUp := 0
	for _, d := range s.Nodes {
		ok, why := e.nodeUp(s, d)
		up[d.Path] = ok
		if ok {
			nodesUp++
		}
		v.Nodes = append(v.Nodes, StorageNodeView{Path: d.Path, Name: d.Name, Up: ok, Why: why})
	}
	for _, d := range s.Enclosures {
		ok, why := e.nodeUp(s, d)
		v.Enclosures = append(v.Enclosures, StorageNodeView{Path: d.Path, Name: d.Name, Up: ok, Why: why})
	}
	// drives: a failed or absent pool drive costs its stripes a unit; a node or enclosure without
	// power takes all its drives, and as stripes spread evenly over the devices holding the pool
	// it costs each stripe ceil(width/devices) units. Lost units degrade the pool; more of them
	// than its parity fail it.
	v.Drives = len(s.Drives)
	devices := map[*model.Device]bool{} // device holding pool drives -> without power
	for _, c := range s.Drives {
		d := c.Device
		devices[d] = !d.State.AC || d.State.Power != model.On
		if devices[d] {
			continue
		}
		ds := e.driveStatus(c)
		if !ds.Present || ds.State == "UnavailableOffline" || ds.Health == model.Critical {
			v.DrivesFailed++
		}
	}
	for _, down := range devices {
		if down {
			v.DevicesDown++
		}
	}
	lost := v.DrivesFailed
	if len(devices) > 0 {
		lost += v.DevicesDown * ((s.DataUnits + s.ParityUnits + len(devices) - 1) / len(devices))
	}
	offline := map[string]string{} // object name or id -> why
	for _, f := range faults {
		if o, ok := f.Sets["storage.offline"].(string); ok && o != "" {
			offline[o] = "fault " + f.ID
		}
	}
	health := model.OK
	alert := func(sev, id, obj, msg string) {
		v.Alerts = append(v.Alerts, StorageAlert{ID: id, Severity: sev, Object: obj, Message: msg})
		health = model.WorstHealth(health, sev)
	}
	serving := map[string]int{}
	object := func(kind, id, name, home string, capacity, used int64, off bool, created time.Time) StorageObjectView {
		o := StorageObjectView{Kind: kind, ID: id, Name: name, Home: home, CapacityBytes: capacity, UsedBytes: used, Created: created}
		o.Node, o.Status = e.servingNode(s, home, up)
		if why, ok := offline[name]; ok || offline[id] != "" || off {
			if why == "" {
				why = "taken offline"
			}
			o.Node, o.Status = "", "Offline"
			alert(model.Warning, "ObjectOffline", id, fmt.Sprintf("%s %s is offline (%s)", kind, name, why))
		} else if o.Status != "Online" {
			alert(model.Warning, "ObjectUnavailable", id, fmt.Sprintf("%s %s is unavailable: its node %s is down (%s)", kind, name, base(home), o.Status))
		}
		o.Online = o.Status == "Online"
		o.Health = model.OK
		if !o.Online {
			o.Health = model.Warning
		}
		if capacity > 0 && used > capacity && kind != "volume" {
			alert(model.Warning, "QuotaExceeded", id, fmt.Sprintf("%s %s is over its quota: %d of %d bytes", kind, name, used, capacity))
			o.Health = model.Warning
		}
		if o.Node != "" {
			serving[o.Node]++
		}
		v.UsedBytes += used
		return o
	}
	for _, f := range s.State.FileSystems {
		o := object("filesystem", f.ID, f.Name, f.Home, f.QuotaBytes, f.UsedBytes, f.Offline, f.Created)
		o.Exports = append([]model.Export{}, f.Exports...)
		o.Path, o.Quota = f.Path, f.Quota
		v.FileSystems = append(v.FileSystems, o)
		v.ProvisionedBytes += f.QuotaBytes
	}
	for _, b := range s.State.Volumes {
		o := object("volume", b.ID, b.Name, b.Home, b.CapacityBytes, b.UsedBytes, b.Offline, b.Created)
		o.Hosts, o.NQN, o.NGUID = append([]string{}, b.Hosts...), b.NQN, b.NGUID
		v.Volumes = append(v.Volumes, o)
		v.ProvisionedBytes += b.CapacityBytes
	}
	for _, b := range s.State.Buckets {
		v.Buckets = append(v.Buckets, object("bucket", b.Name, b.Name, b.Home, b.QuotaBytes, b.UsedBytes, b.Offline, b.Created))
		v.ProvisionedBytes += b.QuotaBytes
	}
	for _, sn := range s.State.Snapshots {
		v.Snapshots = append(v.Snapshots, StorageObjectView{Kind: "snapshot", ID: sn.ID, Name: sn.Name, Of: sn.Of, UsedBytes: sn.UsedBytes,
			Online: true, Status: "Online", Health: model.OK, Created: sn.Created})
		v.UsedBytes += sn.UsedBytes
	}
	for i := range v.Nodes {
		v.Nodes[i].Serving = serving[v.Nodes[i].Path]
	}
	v.Hosts = append([]string{}, s.State.Hosts...)
	// a fault's fill is a floor under what the objects use
	for _, f := range faults {
		if p, ok := toFloat(f.Sets["storage.usedPercent"]); ok {
			if floor := int64(float64(s.UsableBytes) * p / 100); floor > v.UsedBytes {
				v.UsedBytes = floor
			}
		}
		if msg, ok := f.Sets["storage.alert"].(string); ok && msg != "" {
			sev, _ := f.Sets["storage.severity"].(string)
			if sev == "" {
				sev = model.Warning
			}
			alert(sev, "Fault:"+f.Fault, "", msg)
		}
	}
	if s.UsableBytes > 0 {
		v.UsedPercent = math.Round(float64(v.UsedBytes)/float64(s.UsableBytes)*1000) / 10
	}
	switch {
	case v.UsedPercent >= 100:
		alert(model.Critical, "PoolFull", "", fmt.Sprintf("The pool is full: %.1f%% used", v.UsedPercent))
	case s.FullPercent > 0 && v.UsedPercent >= float64(s.FullPercent):
		alert(model.Warning, "PoolNearlyFull", "", fmt.Sprintf("The pool is %.1f%% used (alert at %d%%)", v.UsedPercent, s.FullPercent))
	}
	for _, n := range v.Nodes {
		if !n.Up {
			alert(model.Warning, "NodeDown", n.Path, fmt.Sprintf("Storage node %s is down: %s", n.Name, n.Why))
		}
	}
	for _, n := range v.Enclosures {
		if !n.Up {
			alert(model.Warning, "EnclosureDown", n.Path, fmt.Sprintf("Enclosure %s is down: %s", n.Name, n.Why))
		}
	}
	switch {
	case lost > s.ParityUnits:
		alert(model.Critical, "PoolFailed", "", fmt.Sprintf("%d pool drives failed and %d of its devices are without power, more than its protection's %d parity", v.DrivesFailed, v.DevicesDown, s.ParityUnits))
	case lost > 0:
		alert(model.Warning, "PoolDegraded", "", fmt.Sprintf("%d pool drive(s) failed and %d of its devices are without power: the pool is degraded until they are back", v.DrivesFailed, v.DevicesDown))
	}
	v.State = "Online"
	switch {
	case nodesUp == 0 || lost > s.ParityUnits:
		v.State = "Offline"
		health = model.Critical
	case health != model.OK:
		v.State = "Degraded"
	}
	v.Health = health
	return v
}

func base(p string) string { return p[strings.LastIndex(p, "/")+1:] }

func copyStrings(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

// storageTick follows nodes going down and coming back: a node down for the system's
// failoverSeconds hands its objects to the next node that is up (servingNode).
func (e *Engine) storageTick() {
	for _, s := range e.M.StorageSystems {
		wait := 10 * time.Second
		if n, ok := toFloat(s.Params["failoverSeconds"]); ok {
			wait = time.Duration(n * float64(time.Second))
		}
		for _, d := range s.Nodes {
			ok, why := e.nodeUp(s, d)
			dn, was := e.storageDown[d.Path]
			switch {
			case !ok && !was:
				e.storageDown[d.Path] = &storageNodeDown{since: e.now}
				e.commit(schema.Event{Kind: "state", Source: behaviourSrc, Path: s.Path, Severity: model.Warning,
					State:   &schema.EventState{Field: "node." + d.Name, From: "up", To: "down"},
					Message: fmt.Sprintf("storage node %s down (%s); its objects fail over in %s", d.Name, why, wait)})
			case !ok && was && !dn.failover && e.now.Sub(dn.since) >= wait:
				dn.failover = true
				e.commit(schema.Event{Kind: "state", Source: behaviourSrc, Path: s.Path, Severity: model.Warning,
					State:   &schema.EventState{Field: "node." + d.Name + ".failover", From: false, To: true},
					Message: fmt.Sprintf("storage node %s's objects failed over", d.Name)})
			case ok && was:
				delete(e.storageDown, d.Path)
				e.commit(schema.Event{Kind: "state", Source: behaviourSrc, Path: s.Path,
					State:   &schema.EventState{Field: "node." + d.Name, From: "down", To: "up"},
					Message: fmt.Sprintf("storage node %s back; its objects return to it", d.Name)})
			}
		}
		h := e.storageView(s).Health
		if old, ok := e.health[s.Path]; !ok || old != h {
			if ok || h != model.OK {
				e.stateEvent(s.Path, "health", old, h, behaviourSrc, 0, sevOf(h))
			}
			e.health[s.Path] = h
		}
	}
}

func sevOf(h string) string {
	if h == model.OK {
		return ""
	}
	return h
}

// ---------------------------------------------------------------- owned state

// FileSystemSpec is a file system to create or change; Path defaults to "/<name>".
type FileSystemSpec struct {
	Name       string
	Path       string
	QuotaBytes int64
	Exports    []model.Export
}

// VolumeCreate is a volume to create.
type VolumeCreate struct {
	Name          string
	CapacityBytes int64
}

func (e *Engine) storageWrite(r Request) (*model.StorageSystem, error) {
	s, err := e.system(r.Path)
	if err != nil {
		return nil, err
	}
	r.Path = s.Path
	return s, nil
}

func (e *Engine) logStorage(r Request, s *model.StorageSystem, status int) uint64 {
	r.Path = s.Path
	return e.logRequest(r, httpStatus(r, status))
}

// provisioned is what a system's objects reserve of its pool.
func provisioned(s *model.StorageSystem) int64 {
	var n int64
	for _, f := range s.State.FileSystems {
		n += f.QuotaBytes
	}
	for _, v := range s.State.Volumes {
		n += v.CapacityBytes
	}
	for _, b := range s.State.Buckets {
		n += b.QuotaBytes
	}
	return n
}

func (e *Engine) reserve(s *model.StorageSystem, more int64) error {
	if have := s.UsableBytes - provisioned(s); more > have {
		return fmt.Errorf("%w: %d bytes asked, %d of %d usable bytes unreserved", ErrCapacity, more, max(have, 0), s.UsableBytes)
	}
	return nil
}

// home picks the node a new object lives on: the up node homing the fewest objects.
func (e *Engine) home(s *model.StorageSystem) (string, error) {
	count := map[string]int{}
	for _, f := range s.State.FileSystems {
		count[f.Home]++
	}
	for _, v := range s.State.Volumes {
		count[v.Home]++
	}
	for _, b := range s.State.Buckets {
		count[b.Home]++
	}
	best := ""
	for _, d := range s.Nodes {
		if ok, _ := e.nodeUp(s, d); ok && (best == "" || count[d.Path] < count[best]) {
			best = d.Path
		}
	}
	if best == "" {
		return "", fmt.Errorf("%w: no storage node of %s is up", ErrPrecondition, s.Name)
	}
	return best, nil
}

// safeToken checks what a vm storage node is configured with (paths, host NQNs, export clients):
// only characters that are neither shell syntax nor exports(5) syntax, so no client of a
// management API can inject a command or an export option.
func safeToken(what, v, extra string) error {
	if v == "" || len(v) > 223 || strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._:"+extra) != "" {
		return fmt.Errorf("%w: %s %q: letters, digits and '-._:%s' only", ErrPrecondition, what, v, extra)
	}
	return nil
}

func validPath(p string) error {
	if err := safeToken("path", p, "/"); err != nil {
		return err
	}
	if p[0] != '/' || strings.Contains(p, "//") || slices.Contains(strings.Split(p, "/"), "..") || slices.Contains(strings.Split(p, "/"), ".") {
		return fmt.Errorf("%w: path %q: absolute, with no empty, '.' or '..' segment", ErrPrecondition, p)
	}
	return nil
}

func validNQN(n string) error {
	if !strings.HasPrefix(n, "nqn.") {
		return fmt.Errorf("%w: host NQN %q", ErrPrecondition, n)
	}
	return safeToken("host NQN", n, "")
}

func validName(n string) error {
	if n == "" || len(n) > 63 || strings.Trim(n, "abcdefghijklmnopqrstuvwxyz0123456789-._") != "" || n[0] == '-' || n[0] == '.' {
		return fmt.Errorf("%w: name %q: 1-63 of a-z, 0-9, '-', '.', '_', not starting with '-' or '.'", ErrPrecondition, n)
	}
	return nil
}

func (e *Engine) nextID(s *model.StorageSystem) string {
	s.State.Seq++
	return strconv.Itoa(s.State.Seq)
}

func (e *Engine) objectEvent(r Request, s *model.StorageSystem, c uint64, kind, id, from, to string, data map[string]any) {
	e.commit(schema.Event{Kind: "state", Source: r.Source, Path: s.Path + "/" + kind + "/" + id, Cause: c,
		State: &schema.EventState{Field: kind, From: from, To: to}, Data: data})
}

// CreateFileSystem makes an exported file system.
func (e *Engine) CreateFileSystem(r Request, spec FileSystemSpec) (model.FileSystem, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.storageWrite(r)
	if err != nil {
		return model.FileSystem{}, err
	}
	fail := func(status int, err error) (model.FileSystem, error) {
		e.logStorage(r, s, status)
		return model.FileSystem{}, err
	}
	if !slices.Contains(s.Protocols, "nfs") {
		return fail(400, fmt.Errorf("%w: %s does not serve NFS", ErrUnsupported, s.Name))
	}
	if err := validName(spec.Name); err != nil {
		return fail(400, err)
	}
	for _, f := range s.State.FileSystems {
		if f.Name == spec.Name {
			return fail(409, fmt.Errorf("%w: file system %s", ErrExists, spec.Name))
		}
	}
	if err := e.reserve(s, spec.QuotaBytes); err != nil {
		return fail(400, err)
	}
	if err := checkExports(spec.Exports); err != nil {
		return fail(400, err)
	}
	home, err := e.home(s)
	if err != nil {
		return fail(503, err)
	}
	if spec.Path == "" {
		spec.Path = "/" + spec.Name
	}
	if err := validPath(spec.Path); err != nil {
		return fail(400, err)
	}
	for _, f := range s.State.FileSystems {
		if f.Path == spec.Path {
			return fail(409, fmt.Errorf("%w: a file system at %s", ErrExists, spec.Path))
		}
	}
	f := &model.FileSystem{ID: e.nextID(s), Name: spec.Name, Path: spec.Path, Home: home, QuotaBytes: spec.QuotaBytes,
		Quota: spec.QuotaBytes > 0, Exports: spec.Exports, Created: e.now}
	if f.Exports == nil {
		f.Exports = []model.Export{}
	}
	s.State.FileSystems = append(s.State.FileSystems, f)
	c := e.logStorage(r, s, 201)
	e.objectEvent(r, s, c, "filesystem", f.ID, "", "created", map[string]any{"name": f.Name, "quotaBytes": f.QuotaBytes, "home": f.Home, "exports": len(f.Exports)})
	return *f, nil
}

func checkExports(ex []model.Export) error {
	for _, x := range ex {
		if err := safeToken("export clients", x.Clients, "/*"); err != nil {
			return err
		}
		if x.Access != "rw" && x.Access != "ro" {
			return fmt.Errorf("%w: export access %q: rw or ro", ErrPrecondition, x.Access)
		}
	}
	return nil
}

// UpdateFileSystem changes a file system's quota (when quota is not nil) and exports (when
// exports is not nil).
func (e *Engine) UpdateFileSystem(r Request, id string, quota *int64, exports []model.Export) (model.FileSystem, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.storageWrite(r)
	if err != nil {
		return model.FileSystem{}, err
	}
	f := findFS(s, id)
	if f == nil {
		e.logStorage(r, s, 404)
		return model.FileSystem{}, fmt.Errorf("%w: file system %s", ErrNoStorageObject, id)
	}
	if quota != nil {
		if *quota > f.QuotaBytes {
			if err := e.reserve(s, *quota-f.QuotaBytes); err != nil {
				e.logStorage(r, s, 400)
				return model.FileSystem{}, err
			}
		}
		f.QuotaBytes = *quota
		f.Quota = *quota > 0
	}
	if exports != nil {
		if err := checkExports(exports); err != nil {
			e.logStorage(r, s, 400)
			return model.FileSystem{}, err
		}
		f.Exports = exports
	}
	c := e.logStorage(r, s, 200)
	e.objectEvent(r, s, c, "filesystem", f.ID, "", "updated", map[string]any{"quotaBytes": f.QuotaBytes, "exports": len(f.Exports)})
	return *f, nil
}

func findFS(s *model.StorageSystem, id string) *model.FileSystem {
	for _, f := range s.State.FileSystems {
		if f.ID == id || f.Name == id || (strings.HasPrefix(id, "/") && f.Path == id) {
			return f
		}
	}
	return nil
}

// SetQuota puts a quota on a file system (limit 0: none) or, with remove, takes its quota away.
func (e *Engine) SetQuota(r Request, id string, limit int64, remove bool) (model.FileSystem, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.storageWrite(r)
	if err != nil {
		return model.FileSystem{}, err
	}
	f := findFS(s, id)
	if f == nil {
		e.logStorage(r, s, 404)
		return model.FileSystem{}, fmt.Errorf("%w: file system %s", ErrNoStorageObject, id)
	}
	if remove {
		limit = 0
	}
	if limit > f.QuotaBytes {
		if err := e.reserve(s, limit-f.QuotaBytes); err != nil {
			e.logStorage(r, s, 400)
			return model.FileSystem{}, err
		}
	}
	f.QuotaBytes, f.Quota = limit, !remove
	c := e.logStorage(r, s, 200)
	e.objectEvent(r, s, c, "filesystem", f.ID, "", "quota", map[string]any{"quotaBytes": limit, "quota": f.Quota})
	return *f, nil
}

func findVolume(s *model.StorageSystem, id string) *model.BlockVolume {
	for _, v := range s.State.Volumes {
		if v.ID == id || v.Name == id {
			return v
		}
	}
	return nil
}

func findBucket(s *model.StorageSystem, name string) *model.Bucket {
	for _, b := range s.State.Buckets {
		if b.Name == name {
			return b
		}
	}
	return nil
}

// CreateStorageVolume makes a volume served over NVMe/TCP, mapped to no host.
func (e *Engine) CreateStorageVolume(r Request, spec VolumeCreate) (model.BlockVolume, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.storageWrite(r)
	if err != nil {
		return model.BlockVolume{}, err
	}
	fail := func(status int, err error) (model.BlockVolume, error) {
		e.logStorage(r, s, status)
		return model.BlockVolume{}, err
	}
	if !slices.Contains(s.Protocols, "nvme-tcp") {
		return fail(400, fmt.Errorf("%w: %s does not serve NVMe/TCP", ErrUnsupported, s.Name))
	}
	if err := validName(spec.Name); err != nil {
		return fail(400, err)
	}
	if spec.CapacityBytes <= 0 {
		return fail(400, fmt.Errorf("%w: a volume needs a capacity", ErrPrecondition))
	}
	if findVolume(s, spec.Name) != nil {
		return fail(409, fmt.Errorf("%w: volume %s", ErrExists, spec.Name))
	}
	if err := e.reserve(s, spec.CapacityBytes); err != nil {
		return fail(400, err)
	}
	home, err := e.home(s)
	if err != nil {
		return fail(503, err)
	}
	id := e.nextID(s)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%s/volume/%s/%s", e.M.Seed, s.Path, id, spec.Name)))
	v := &model.BlockVolume{ID: id, Name: spec.Name, Home: home, CapacityBytes: spec.CapacityBytes,
		NQN: "nqn.2026-11.io.dctwin:" + s.Name + ":" + spec.Name, NGUID: hex.EncodeToString(sum[:16]), Hosts: []string{}, Created: e.now}
	s.State.Volumes = append(s.State.Volumes, v)
	c := e.logStorage(r, s, 201)
	e.objectEvent(r, s, c, "volume", v.ID, "", "created", map[string]any{"name": v.Name, "capacityBytes": v.CapacityBytes, "home": v.Home, "nqn": v.NQN})
	return *v, nil
}

// MapVolume grants (or with unmap, revokes) a host NQN access to a volume.
func (e *Engine) MapVolume(r Request, id, hostNQN string, unmap bool) (model.BlockVolume, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.storageWrite(r)
	if err != nil {
		return model.BlockVolume{}, err
	}
	v := findVolume(s, id)
	if v == nil {
		e.logStorage(r, s, 404)
		return model.BlockVolume{}, fmt.Errorf("%w: volume %s", ErrNoStorageObject, id)
	}
	if validNQN(hostNQN) != nil {
		e.logStorage(r, s, 400)
		return model.BlockVolume{}, fmt.Errorf("%w: host NQN %q", ErrPrecondition, hostNQN)
	}
	if !slices.Contains(s.State.Hosts, hostNQN) {
		s.State.Hosts = append(s.State.Hosts, hostNQN)
	}
	i := slices.Index(v.Hosts, hostNQN)
	switch {
	case unmap && i >= 0:
		v.Hosts = slices.Delete(v.Hosts, i, i+1)
	case !unmap && i < 0:
		v.Hosts = append(v.Hosts, hostNQN)
	}
	c := e.logStorage(r, s, 200)
	verb := "mapped"
	if unmap {
		verb = "unmapped"
	}
	e.objectEvent(r, s, c, "volume", v.ID, "", verb, map[string]any{"host": hostNQN, "hosts": len(v.Hosts)})
	return *v, nil
}

// CreateBucket makes an S3 bucket.
func (e *Engine) CreateBucket(r Request, name string, quota int64) (model.Bucket, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.storageWrite(r)
	if err != nil {
		return model.Bucket{}, err
	}
	fail := func(status int, err error) (model.Bucket, error) {
		e.logStorage(r, s, status)
		return model.Bucket{}, err
	}
	if !slices.Contains(s.Protocols, "s3") {
		return fail(400, fmt.Errorf("%w: %s does not serve S3", ErrUnsupported, s.Name))
	}
	if err := validName(name); err != nil || len(name) < 3 || strings.ContainsAny(name, "_") {
		return fail(400, fmt.Errorf("%w: bucket name %q: 3-63 of a-z, 0-9, '-', '.'", ErrPrecondition, name))
	}
	if findBucket(s, name) != nil {
		return fail(409, fmt.Errorf("%w: bucket %s", ErrExists, name))
	}
	if err := e.reserve(s, quota); err != nil {
		return fail(400, err)
	}
	home, err := e.home(s)
	if err != nil {
		return fail(503, err)
	}
	b := &model.Bucket{Name: name, Home: home, QuotaBytes: quota, Created: e.now}
	s.State.Buckets = append(s.State.Buckets, b)
	c := e.logStorage(r, s, 201)
	e.objectEvent(r, s, c, "bucket", b.Name, "", "created", map[string]any{"quotaBytes": quota, "home": home})
	return *b, nil
}

// CreateSnapshot snapshots a file system or volume.
func (e *Engine) CreateSnapshot(r Request, of, name string) (model.Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.storageWrite(r)
	if err != nil {
		return model.Snapshot{}, err
	}
	src := ""
	if f := findFS(s, of); f != nil {
		src = f.ID
	} else if v := findVolume(s, of); v != nil {
		src = v.ID
	}
	if src == "" {
		e.logStorage(r, s, 404)
		return model.Snapshot{}, fmt.Errorf("%w: no file system or volume %s", ErrNoStorageObject, of)
	}
	if name == "" {
		name = fmt.Sprintf("snap-%d", s.State.Seq+1)
	}
	sn := &model.Snapshot{ID: e.nextID(s), Name: name, Of: src, Created: e.now}
	s.State.Snapshots = append(s.State.Snapshots, sn)
	c := e.logStorage(r, s, 201)
	e.objectEvent(r, s, c, "snapshot", sn.ID, "", "created", map[string]any{"of": src, "name": name})
	return *sn, nil
}

// DeleteStorageObject deletes a file system, volume, bucket or snapshot by kind and id (or name).
// A file system or volume with snapshots keeps them only while it exists: they go with it.
func (e *Engine) DeleteStorageObject(r Request, kind, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.storageWrite(r)
	if err != nil {
		return err
	}
	st := &s.State
	gone := ""
	switch kind {
	case "filesystem":
		if f := findFS(s, id); f != nil {
			gone = f.ID
			st.FileSystems = slices.DeleteFunc(st.FileSystems, func(x *model.FileSystem) bool { return x == f })
		}
	case "volume":
		if v := findVolume(s, id); v != nil {
			if dpu := e.presentedBy(s, v.ID); dpu != "" {
				e.logStorage(r, s, 409)
				return fmt.Errorf("%w: volume %s is attached to %s's NVMe emulation; detach it first", ErrPrecondition, v.Name, dpu)
			}
			gone = v.ID
			st.Volumes = slices.DeleteFunc(st.Volumes, func(x *model.BlockVolume) bool { return x == v })
		}
	case "bucket":
		if b := findBucket(s, id); b != nil {
			gone = b.Name
			st.Buckets = slices.DeleteFunc(st.Buckets, func(x *model.Bucket) bool { return x == b })
		}
	case "snapshot":
		for _, sn := range st.Snapshots {
			if sn.ID == id || sn.Name == id {
				gone = sn.ID
			}
		}
		st.Snapshots = slices.DeleteFunc(st.Snapshots, func(x *model.Snapshot) bool { return x.ID == gone })
	default:
		e.logStorage(r, s, 400)
		return fmt.Errorf("%w: object kind %q", ErrUnsupported, kind)
	}
	if gone == "" {
		e.logStorage(r, s, 404)
		return fmt.Errorf("%w: %s %s", ErrNoStorageObject, kind, id)
	}
	if kind == "filesystem" || kind == "volume" {
		st.Snapshots = slices.DeleteFunc(st.Snapshots, func(x *model.Snapshot) bool { return x.Of == gone })
	}
	c := e.logStorage(r, s, 204)
	e.objectEvent(r, s, c, kind, gone, "created", "deleted", nil)
	return nil
}

// presentedBy names a DPU whose NVMe emulation presents a system's volume, or "".
func (e *Engine) presentedBy(s *model.StorageSystem, volumeID string) string {
	for _, d := range e.M.Devices {
		if em := d.State.Emulation; em != nil {
			for _, f := range em.Functions {
				if f.System == s.Path && f.Volume == volumeID {
					return d.Path
				}
			}
		}
	}
	return ""
}

// ObserveStorageUse records what an object holds, as its node reports it (vm) or an act sets
// it; kind is filesystem, volume, bucket or snapshot.
func (e *Engine) ObserveStorageUse(path, kind, id string, used int64, src schema.EventSource) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.system(path)
	if err != nil {
		return err
	}
	var at *int64
	switch kind {
	case "filesystem":
		if f := findFS(s, id); f != nil {
			at = &f.UsedBytes
		}
	case "volume":
		if v := findVolume(s, id); v != nil {
			at = &v.UsedBytes
		}
	case "bucket":
		if b := findBucket(s, id); b != nil {
			at = &b.UsedBytes
		}
	case "snapshot":
		for _, sn := range s.State.Snapshots {
			if sn.ID == id || sn.Name == id {
				at = &sn.UsedBytes
			}
		}
	}
	if at == nil {
		return fmt.Errorf("%w: %s %s", ErrNoStorageObject, kind, id)
	}
	if *at != used {
		old := *at
		*at = used
		// a node's periodic report changes use continuously: only a change of at least 1 MiB or
		// 1% is an event (the value itself is always current)
		if d := used - old; src.Class != "observer" || d > 1<<20 || -d > 1<<20 {
			e.commit(schema.Event{Kind: "state", Source: src, Path: s.Path + "/" + kind + "/" + id,
				State: &schema.EventState{Field: "usedBytes", From: old, To: used}})
		}
	}
	return nil
}

// StorageAct is a technician's or scenario's act on a system: fill an object, or take one
// offline or bring it back.
func (e *Engine) StorageAct(path string, a schema.StorageAct, src schema.EventSource) error {
	e.mu.Lock()
	s, err := e.system(path)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	switch {
	case a.Offline != "" || a.Online != "":
		name, off := a.Offline, true
		if name == "" {
			name, off = a.Online, false
		}
		hit := false
		for _, f := range s.State.FileSystems {
			if f.ID == name || f.Name == name {
				f.Offline, hit = off, true
			}
		}
		for _, v := range s.State.Volumes {
			if v.ID == name || v.Name == name {
				v.Offline, hit = off, true
			}
		}
		for _, b := range s.State.Buckets {
			if b.Name == name {
				b.Offline, hit = off, true
			}
		}
		if !hit {
			e.mu.Unlock()
			return fmt.Errorf("%w: %s", ErrNoStorageObject, name)
		}
		e.commit(schema.Event{Kind: "act", Source: src, Path: s.Path, Message: fmt.Sprintf("%s %s", map[bool]string{true: "offline", false: "online"}[off], name)})
		e.mu.Unlock()
		return nil
	case a.Fill != "":
		kind, id, capacity := "", "", int64(0)
		if f := findFS(s, a.Fill); f != nil {
			kind, id, capacity = "filesystem", f.ID, f.QuotaBytes
		} else if v := findVolume(s, a.Fill); v != nil {
			kind, id, capacity = "volume", v.ID, v.CapacityBytes
		} else if b := findBucket(s, a.Fill); b != nil {
			kind, id, capacity = "bucket", b.Name, b.QuotaBytes
		}
		if kind == "" {
			e.mu.Unlock()
			return fmt.Errorf("%w: %s", ErrNoStorageObject, a.Fill)
		}
		var used int64
		switch {
		case a.UsedBytes != nil:
			used = *a.UsedBytes
		case a.UsedPercent != nil:
			if capacity == 0 {
				capacity = s.UsableBytes
			}
			used = int64(float64(capacity) * *a.UsedPercent / 100)
		default:
			e.mu.Unlock()
			return fmt.Errorf("%w: fill needs usedBytes or usedPercent", ErrPrecondition)
		}
		e.commit(schema.Event{Kind: "act", Source: src, Path: s.Path, Message: fmt.Sprintf("fill %s %s to %d bytes", kind, a.Fill, used)})
		e.mu.Unlock()
		return e.ObserveStorageUse(s.Path, kind, id, used, src)
	}
	e.mu.Unlock()
	return fmt.Errorf("%w: a storage act names fill, offline or online", ErrPrecondition)
}

// RegisterHost makes an NVMe host (its NQN) known to a system, or with remove forgets it and
// unmaps it from every volume.
func (e *Engine) RegisterHost(r Request, hostNQN string, remove bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := e.storageWrite(r)
	if err != nil {
		return err
	}
	if validNQN(hostNQN) != nil {
		e.logStorage(r, s, 400)
		return fmt.Errorf("%w: host NQN %q", ErrPrecondition, hostNQN)
	}
	i := slices.Index(s.State.Hosts, hostNQN)
	switch {
	case remove && i < 0:
		e.logStorage(r, s, 404)
		return fmt.Errorf("%w: host %s", ErrNoStorageObject, hostNQN)
	case remove:
		s.State.Hosts = slices.Delete(s.State.Hosts, i, i+1)
		for _, v := range s.State.Volumes {
			v.Hosts = slices.DeleteFunc(v.Hosts, func(h string) bool { return h == hostNQN })
		}
	case i < 0:
		s.State.Hosts = append(s.State.Hosts, hostNQN)
	}
	c := e.logStorage(r, s, 200)
	verb := "registered"
	if remove {
		verb = "removed"
	}
	e.objectEvent(r, s, c, "host", hostNQN, "", verb, nil)
	return nil
}

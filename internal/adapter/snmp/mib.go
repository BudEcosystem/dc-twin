package snmp

import (
	"fmt"
	"sort"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// entry is one instance the agent serves.
type entry struct {
	oid OID
	val value
	// read is the canonical name a read of this instance is logged as.
	read string
}

// view is a device's MIB at one moment: every served instance in lexicographic order, and the
// object types they belong to (so a GET can tell noSuchObject from noSuchInstance).
type view struct {
	entries []entry
	objects []OID // object type OIDs: a scalar's or a column's OID without its instance
}

func (v *view) add(object OID, instance OID, val value, read string) {
	v.entries = append(v.entries, entry{oid: object.Append(instance...), val: val, read: read})
	v.objects = append(v.objects, object)
}

func (v *view) sort() {
	sort.Slice(v.entries, func(i, j int) bool { return v.entries[i].oid.Compare(v.entries[j].oid) < 0 })
	// a dialect that binds two names to one OID is refused by bind; drop exact duplicates anyway
	// so GETNEXT can never return the same OID twice
	out := v.entries[:0]
	for i, e := range v.entries {
		if i > 0 && e.oid.Compare(out[len(out)-1].oid) == 0 {
			continue
		}
		out = append(out, e)
	}
	v.entries = out
}

// get answers a GET: the instance, or the v2c exception for its absence.
func (v *view) get(o OID) (entry, byte) {
	i := sort.Search(len(v.entries), func(i int) bool { return v.entries[i].oid.Compare(o) >= 0 })
	if i < len(v.entries) && v.entries[i].oid.Compare(o) == 0 {
		return v.entries[i], 0
	}
	for _, obj := range v.objects {
		if o.HasPrefix(obj) {
			return entry{}, tagNoSuchInstance
		}
	}
	return entry{}, tagNoSuchObject
}

// next answers a GETNEXT: the first instance strictly after o.
func (v *view) next(o OID) (entry, bool) {
	i := sort.Search(len(v.entries), func(i int) bool { return v.entries[i].oid.Compare(o) > 0 })
	if i < len(v.entries) {
		return v.entries[i], true
	}
	return entry{}, false
}

// ---------------------------------------------------------------- MIB-II system group

var (
	sysDescr    = mustOID("1.3.6.1.2.1.1.1")
	sysObjectID = mustOID("1.3.6.1.2.1.1.2")
	sysUpTime   = mustOID("1.3.6.1.2.1.1.3")
	sysContact  = mustOID("1.3.6.1.2.1.1.4")
	sysName     = mustOID("1.3.6.1.2.1.1.5")
	sysLocation = mustOID("1.3.6.1.2.1.1.6")
	sysServices = mustOID("1.3.6.1.2.1.1.7")
)

// ---------------------------------------------------------------- dialect binding

// The dialect's objects are bound by name: the name is the contract between a MibDialect and
// this adapter, the OID is the dialect's data. A scalar names its full instance OID (ending .1 for
// the first device or phase); a column names the table column, and the outlet number is its
// instance.
const (
	objDeviceName    = "deviceName"
	objDevicePower   = "devicePowerHundredthKW"
	objDeviceLoad    = "deviceLoadState"
	objPhaseCurrent  = "phaseCurrentTenthsA"
	objOutletName    = "outletName"
	objOutletState   = "outletState"
	objOutletCommand = "outletCommand"
)

var scalarNames = []string{objDeviceName, objDevicePower, objDeviceLoad, objPhaseCurrent}
var columnNames = []string{objOutletName, objOutletState, objOutletCommand}

// Outlet command values (PowerNet-MIB rPDU2OutletSwitchedControlCommand).
const (
	cmdImmediateOn     = 1
	cmdImmediateOff    = 2
	cmdImmediateReboot = 3
)

// Outlet states (rPDU2OutletSwitchedStatusState) and load states (rPDU2DeviceStatusLoadState).
const (
	outletOff = 1
	outletOn  = 2

	loadNormal       = 2
	loadNearOverload = 3
	loadOverload     = 4
)

// binding is a MibDialect resolved for serving.
type binding struct {
	name        string
	spec        *schema.MibSpec
	sysObjectID OID
	scalars     map[string]OID
	columns     map[string]OID
	unknown     []string // dialect objects this adapter has no semantics for, so does not serve
}

func bind(name string, d *schema.MibDialect) (*binding, error) {
	b := &binding{name: name, spec: &d.Spec, scalars: map[string]OID{}, columns: map[string]OID{}}
	var err error
	if b.sysObjectID, err = ParseOID(d.Spec.SysObjectID); err != nil {
		return nil, fmt.Errorf("MIB dialect %s: sysObjectID: %w", name, err)
	}
	seen := map[string]string{}
	keys := make([]string, 0, len(d.Spec.Objects))
	for k := range d.Spec.Objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		o, err := ParseOID(d.Spec.Objects[k])
		if err != nil {
			return nil, fmt.Errorf("MIB dialect %s: object %s: %w", name, k, err)
		}
		if prev, dup := seen[o.String()]; dup {
			return nil, fmt.Errorf("MIB dialect %s: objects %s and %s share OID %s", name, prev, k, o)
		}
		seen[o.String()] = k
		switch {
		case contains(scalarNames, k):
			b.scalars[k] = o
		case contains(columnNames, k):
			b.columns[k] = o
		default:
			b.unknown = append(b.unknown, k)
		}
	}
	return b, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// outletCommandIndex reports whether o names an instance of the dialect's outlet command
// column, and which outlet.
func (b *binding) outletCommandIndex(o OID) (int64, bool) {
	col, ok := b.columns[objOutletCommand]
	if !ok || len(o) != len(col)+1 || !o.HasPrefix(col) {
		return 0, false
	}
	return int64(o[len(col)]), true
}

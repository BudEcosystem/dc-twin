package nmxc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
)

// The partition table of one NVLink domain: the controller's own state, which the model does not
// hold. Its rules are the ones NICo's partition monitor is written against and NICo's nmxc-mock
// implements (crates/nmxc-mock/src/state.rs): ids are allocated from 1 unless the request names
// one, names are unique (an unnamed partition is "partition-<id>"), a GPU belongs to at most one
// partition, deleting an id that is not provisioned succeeds while deleting an unknown name does
// not, and removing a GPU that is not a member is not an error. A new domain boots with the
// factory partition holding every GPU, as a real controller does; NICo deletes it before it
// provisions its own. The table persists in the state directory, as the controller's database
// would.

// Factory partition, as NVOS reports it (the NVUE adapter's sdn/partition shows the same).
const (
	DefaultPartitionID   = 32766
	DefaultPartitionName = "Default Partition"
)

// Partition types, by their value names.
const (
	typeUID      = "NMX_PARTITION_TYPE_GPUUID_BASED"
	typeLocation = "NMX_PARTITION_TYPE_LOCATION_BASED"
)

type partition struct {
	ID         uint32   `json:"id"`
	Name       string   `json:"name"`
	GPUs       []uint64 `json:"gpus"`
	Type       string   `json:"type"`
	Resiliency string   `json:"resiliency,omitempty"`
	McastLimit uint32   `json:"multicastGroupsLimit,omitempty"`
}

type table struct {
	Partitions map[uint32]*partition `json:"partitions"`
	NextID     uint32                `json:"nextId"`
	gpus       map[uint64]bool
}

// stError is a controller return code (StReturnCode) with a message.
type stError struct {
	code string
	msg  string
}

func (e *stError) Error() string { return e.msg }

func st(code, format string, a ...any) error {
	return &stError{code: code, msg: fmt.Sprintf(format, a...)}
}

// returnCode is the StReturnCode name for an error, success for nil.
func returnCode(err error) string {
	var se *stError
	if err == nil {
		return "NMX_ST_SUCCESS"
	}
	if errors.As(err, &se) {
		return se.code
	}
	return "NMX_ST_GENERIC_ERROR"
}

func newTable(gpus []uint64, factory bool) *table {
	t := &table{Partitions: map[uint32]*partition{}, NextID: 1}
	t.setGPUs(gpus)
	if factory {
		t.Partitions[DefaultPartitionID] = &partition{ID: DefaultPartitionID, Name: DefaultPartitionName, GPUs: append([]uint64{}, gpus...), Type: typeUID, McastLimit: 1024}
	}
	return t
}

func (t *table) setGPUs(gpus []uint64) {
	t.gpus = map[uint64]bool{}
	for _, g := range gpus {
		t.gpus[g] = true
	}
}

// loadTable reads a persisted table; a missing file is a new domain.
func loadTable(path string, gpus []uint64, factory bool) (*table, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || path == "" {
		return newTable(gpus, factory), nil
	}
	if err != nil {
		return nil, err
	}
	t := &table{}
	if err := json.Unmarshal(b, t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if t.Partitions == nil {
		t.Partitions = map[uint32]*partition{}
	}
	if t.NextID == 0 {
		t.NextID = 1
	}
	t.setGPUs(gpus)
	return t, nil
}

func (t *table) save(path string) error {
	if path == "" {
		return nil
	}
	b, _ := json.MarshalIndent(t, "", "  ")
	return os.WriteFile(path, b, 0o644)
}

// list is every partition in id order.
func (t *table) list() []*partition {
	out := make([]*partition, 0, len(t.Partitions))
	for _, p := range t.Partitions {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// find is the partitions matching any id or name; every partition when both are empty.
func (t *table) find(ids []uint32, names []string) []*partition {
	if len(ids) == 0 && len(names) == 0 {
		return t.list()
	}
	var out []*partition
	for _, p := range t.list() {
		hit := false
		for _, id := range ids {
			hit = hit || id == p.ID
		}
		for _, n := range names {
			hit = hit || n == p.Name
		}
		if hit {
			out = append(out, p)
		}
	}
	return out
}

// of is the partition holding a GPU.
func (t *table) of(uid uint64) *partition {
	for _, p := range t.Partitions {
		for _, g := range p.GPUs {
			if g == uid {
				return p
			}
		}
	}
	return nil
}

// ref names a partition: a non-zero id, else a non-empty name.
type ref struct {
	id   uint32
	name string
}

func newRef(id uint32, name string) (ref, error) {
	switch {
	case id != 0:
		return ref{id: id}, nil
	case name != "":
		return ref{name: name}, nil
	}
	return ref{}, st("NMX_ST_BADPARAM", "neither a partition id nor a name was given")
}

func (t *table) resolve(r ref) (*partition, error) {
	if r.id != 0 {
		if p := t.Partitions[r.id]; p != nil {
			return p, nil
		}
		return nil, st("NMX_ST_PARTITION_ID_NOT_IN_USE", "partition %d is not provisioned", r.id)
	}
	for _, p := range t.Partitions {
		if p.Name == r.name {
			return p, nil
		}
	}
	return nil, st("NMX_ST_PARTITION_NAME_NOT_IN_USE", "partition %q is not provisioned", r.name)
}

// claimable checks GPUs are in the domain and free (or already in member) and de-duplicates.
func (t *table) claimable(uids []uint64, member *partition) ([]uint64, error) {
	var out []uint64
	seen := map[uint64]bool{}
	for _, u := range uids {
		if !t.gpus[u] {
			return nil, st("NMX_ST_BADPARAM", "GPU %#016x is not in this NVLink domain", u)
		}
		if p := t.of(u); p != nil && p != member {
			return nil, st("NMX_ST_RESOURCE_IN_USE", "GPU %#016x already belongs to partition %d", u, p.ID)
		}
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out, nil
}

func (t *table) create(name string, uids []uint64, id uint32, typ string, resiliency string, mcast uint32) (*partition, error) {
	if name != "" {
		for _, p := range t.Partitions {
			if p.Name == name {
				return nil, st("NMX_ST_PARTITION_NAME_IN_USE", "partition name %q is already in use", name)
			}
		}
	}
	if id != 0 && t.Partitions[id] != nil {
		return nil, st("NMX_ST_PARTITION_ID_IN_USE", "partition id %d is already in use", id)
	}
	gpus, err := t.claimable(uids, nil)
	if err != nil {
		return nil, err
	}
	if id == 0 {
		for t.Partitions[t.NextID] != nil {
			t.NextID++
		}
		id = t.NextID
		t.NextID++
	}
	if name == "" {
		name = fmt.Sprintf("partition-%d", id)
		for _, p := range t.Partitions {
			if p.Name == name {
				return nil, st("NMX_ST_PARTITION_NAME_IN_USE", "partition name %q is already in use", name)
			}
		}
	}
	if typ == "" {
		typ = typeUID
	}
	p := &partition{ID: id, Name: name, GPUs: gpus, Type: typ, Resiliency: resiliency, McastLimit: mcast}
	t.Partitions[id] = p
	return p, nil
}

func (t *table) remove(r ref) (uint32, error) {
	if r.id != 0 {
		delete(t.Partitions, r.id)
		return r.id, nil
	}
	p, err := t.resolve(r)
	if err != nil {
		return 0, err
	}
	delete(t.Partitions, p.ID)
	return p.ID, nil
}

func (t *table) addGPUs(r ref, uids []uint64) (uint32, error) {
	p, err := t.resolve(r)
	if err != nil {
		return 0, err
	}
	add, err := t.claimable(uids, p)
	if err != nil {
		return 0, err
	}
	for _, u := range add {
		have := false
		for _, g := range p.GPUs {
			have = have || g == u
		}
		if !have {
			p.GPUs = append(p.GPUs, u)
		}
	}
	return p.ID, nil
}

func (t *table) removeGPUs(r ref, uids []uint64) (uint32, error) {
	p, err := t.resolve(r)
	if err != nil {
		return 0, err
	}
	drop := map[uint64]bool{}
	for _, u := range uids {
		drop[u] = true
	}
	keep := p.GPUs[:0]
	for _, g := range p.GPUs {
		if !drop[g] {
			keep = append(keep, g)
		}
	}
	p.GPUs = keep
	return p.ID, nil
}

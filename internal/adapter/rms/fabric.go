package rms

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// FabricFile is where the adapter keeps the scale-up fabric and switch state it configures.
const FabricFile = "rms-fabric.json"

// fabric is what RMS has configured on the switch trays, by rack path: the scale-up fabric
// manager's primary, topology and static configuration, and each switch's protected services. It
// persists, as the switches' configuration would.
type fabric struct {
	mu    sync.Mutex
	file  string
	Racks map[string]*rackFabric `json:"racks"`
}

type rackFabric struct {
	Primary    string              `json:"primary,omitempty"` // device path
	Topology   string              `json:"topology,omitempty"`
	Static     []staticConfig      `json:"staticConfigs,omitempty"`
	Configured bool                `json:"configured"`
	Certs      map[string][]string `json:"certificates,omitempty"` // device path -> services
}

type staticConfig struct {
	File  string `json:"file"`
	Key   string `json:"key"`
	Value string `json:"value"`
}

func loadFabric(stateDir string) (*fabric, error) {
	f := &fabric{Racks: map[string]*rackFabric{}}
	if stateDir == "" {
		return f, nil
	}
	f.file = filepath.Join(stateDir, FabricFile)
	b, err := os.ReadFile(f.file)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, err
	}
	if f.Racks == nil {
		f.Racks = map[string]*rackFabric{}
	}
	return f, nil
}

func (f *fabric) rack(path string) *rackFabric {
	r := f.Racks[path]
	if r == nil {
		r = &rackFabric{}
		f.Racks[path] = r
	}
	if r.Certs == nil {
		r.Certs = map[string][]string{}
	}
	return r
}

// saveLocked persists; callers hold mu.
func (f *fabric) saveLocked() {
	if f.file == "" {
		return
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	_ = os.WriteFile(f.file, b, 0o644)
}

// lowest is the switch at the lowest rack position; device path breaks ties.
func lowest(cands []*node) *node {
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].dev.U != cands[j].dev.U {
			return cands[i].dev.U < cands[j].dev.U
		}
		return cands[i].dev.Path < cands[j].dev.Path
	})
	if len(cands) == 0 {
		return nil
	}
	return cands[0]
}

// ensurePrimary elects a rack's primary when it has none: the lowest switch.
func (f *fabric) ensurePrimary(rackPath string, cands []*node) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rack(rackPath)
	if r.Primary == "" {
		if n := lowest(cands); n != nil {
			r.Primary = n.dev.Path
			f.saveLocked()
		}
	}
	return r.Primary
}

// Package smstate is where the state a subnet manager left in a simulated InfiniBand fabric is
// read by the twin's other adapters: the ibsim adapter registers itself, for its engine, as the
// source of each fabric's port LIDs and logical states as ibsim holds them, and the ufm adapter
// asks it, as a UFM reads its embedded OpenSM's view instead of computing one. It is a package of
// its own so that neither adapter imports the other.
//
// The state is substrate-owned (D19, D47): it is what the stack's subnet manager set, not the
// model. A fabric no subnet manager has configured has none.
package smstate

import (
	"sync"

	"github.com/BudEcosystem/dc-twin/internal/engine"
)

// Port is one port as the substrate holds it.
type Port struct {
	LID   int    // base LID the subnet manager assigned (0: none)
	SMLID int    // the LID of the subnet manager the port was configured by
	State string // logical state: Down, Init, Armed, Active
	Phys  string // physical state: Polling, LinkUp, Disabled, …
}

// Source answers with a fabric's subnet-manager state.
type Source interface {
	// Ports returns the fabric's ports by GUID, 16 lower-case hex digits with no 0x: a channel
	// adapter's port GUID, and a switch's node GUID for its port 0 (every port of a switch shares
	// its LID). ok is false when the fabric is not simulated or no subnet manager has assigned a
	// LID in it, so the caller keeps what it would report without one.
	Ports(fabric string) (ports map[string]Port, ok bool)
}

var (
	mu      sync.Mutex
	sources = map[*engine.Engine]Source{}
)

// Register makes s the source for one twin's engine.
func Register(e *engine.Engine, s Source) {
	mu.Lock()
	defer mu.Unlock()
	sources[e] = s
}

// Unregister removes s if it is the engine's source.
func Unregister(e *engine.Engine, s Source) {
	mu.Lock()
	defer mu.Unlock()
	if sources[e] == s {
		delete(sources, e)
	}
}

// Lookup returns the engine's source, or nil.
func Lookup(e *engine.Engine) Source {
	mu.Lock()
	defer mu.Unlock()
	return sources[e]
}

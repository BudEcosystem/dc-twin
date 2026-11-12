package nvue

import (
	"context"
	"fmt"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// FactoryHostname is a Cumulus Linux switch's hostname as it leaves the factory, and
// FactoryPassword its user cumulus's password then.
const (
	FactoryHostname = "cumulus"
	FactoryPassword = "cumulus"
)

// Factory is a generic/nvue-frr switch's configuration as Cumulus Linux leaves the factory
// (D72, 18 — Factory state): its hostname, and its management port in the management VRF with
// DHCP. Nothing else: no loopback, no front-panel port configured, no BGP, no relay. A day-0
// tool configures the rest through NVUE, `nv` or its ZTP script.
func Factory(sw nos.Switch) Tree {
	t := Tree{}
	set := func(v any, path ...string) { Patch(t, path, v) }
	set(FactoryHostname, "system", "hostname")
	if sw.Mgmt {
		set("eth", "interface", nos.MgmtIf, "type")
		set(Tree{}, "interface", nos.MgmtIf, "ip", "address", "dhcp")
		set("mgmt", "interface", nos.MgmtIf, "ip", "vrf")
	}
	// on 5.15 and later, interface eth0 ipv4 address dhcp and interface eth0 vrf mgmt (that
	// 5.15 keeps the management port's DHCP as an address, dhcp, is UNCONFIRMED)
	if !switchVersion(sw).Less(v515) {
		t = upgrade(t)
	}
	return t
}

// ApplyFactory applies the factory configuration as the switch's first revision, its startup
// configuration, in place of ApplyStartup's design.
func (s *Server) ApplyFactory(ctx context.Context) error {
	t := Factory(s.sw)
	s.mu.Lock()
	s.startup = Clone(t)
	r := &revision{tree: Clone(t), changes: []change{{v: t}}, state: statePending, user: "system", created: time.Now()}
	s.revs[s.newID()] = r
	s.mu.Unlock()
	s.applyRevision(ctx, r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.state != stateApplied {
		return fmt.Errorf("the factory configuration: %s: %s", r.state, r.issue)
	}
	return nil
}

// ResetFactory returns the switch to its factory configuration, as Cumulus's factory reset (and
// `nv config replace` of the factory startup.yaml) does: every revision and every user's pending
// one is forgotten, and the factory tree applied over an empty configuration, so whatever the
// switch was given (its hostname, ports, BGP, users) is removed by the switch's own tools.
func (s *Server) ResetFactory(ctx context.Context) error {
	s.mu.Lock()
	s.revs, s.pending, s.next = map[string]*revision{}, map[string]string{}, 1
	s.applied = Tree{}
	s.mu.Unlock()
	return s.ApplyFactory(ctx)
}

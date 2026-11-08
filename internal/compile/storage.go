package compile

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/selector"
	"slices"
)

// storageSystems makes the estate's shared storage systems (docs/reviews/2026-11-08-storage-deep-dive.md
// 5.4, S-Q4) from the racks each selects: its storage nodes and enclosures by the template's
// device classes, its pool from their data drives of the template's media, a protection that fits
// those drives, and networks of the right roles that reach its nodes. Rack formats place the
// hardware; this makes it one system the compiler can check.
func (c *compiler) storageSystems() {
	c.m.StorageBy = map[string]*model.StorageSystem{}
	for i, ss := range c.est.Spec.StorageSystems {
		where := "storageSystems " + ss.Name
		if ss.Name == "" {
			c.errf("storageSystems[%d]: name is required", i)
			continue
		}
		path := c.m.Site + "/storage/" + ss.Name
		if c.m.StorageBy[path] != nil {
			c.errf("%s: declared twice", where)
			continue
		}
		t, ok := c.reg.StorageTemplates[ss.Template]
		if !ok {
			c.errf("%s: storage template %q not found", where, ss.Template)
			continue
		}
		if !c.enabled[pack.PackOf(ss.Template)] {
			c.errf("%s: template %s: pack %s is not pinned by the estate", where, ss.Template, pack.PackOf(ss.Template))
		}
		sys := &model.StorageSystem{Name: ss.Name, Path: path, Template: ss.Template, Manager: ss.Manager, Fidelity: ss.Fidelity,
			Params: c.checkParams(where, ss.Template, t.Spec.Params, ss.Params, false, true), Faults: t.Spec.Faults,
			Endpoints: map[string]string{}, Addresses: map[string]string{}}
		if sys.Fidelity == "" {
			sys.Fidelity = "model"
		}
		if sys.Fidelity != "model" && sys.Fidelity != "vm" {
			c.errf("%s: fidelity %q: model or vm", where, sys.Fidelity)
		}
		sys.IsNot = append(append([]string{}, t.Spec.IsNot.All...), t.Spec.IsNot.Model...)
		if sys.Fidelity == "vm" {
			sys.IsNot = append(append([]string{}, t.Spec.IsNot.All...), t.Spec.IsNot.VM...)
		}
		sys.Provides = t.Spec.Provides[sys.Fidelity]
		if sys.Manager == "" && len(t.Spec.Managers) > 0 {
			sys.Manager = t.Spec.Managers[0]
		}
		if !slices.Contains(t.Spec.Managers, sys.Manager) {
			c.errf("%s: manager %q: template %s serves %v", where, sys.Manager, ss.Template, t.Spec.Managers)
		}
		sys.Protocols = ss.Protocols
		if len(sys.Protocols) == 0 {
			sys.Protocols = t.Spec.Protocols
		}
		for _, p := range sys.Protocols {
			if !slices.Contains(t.Spec.Protocols, p) {
				c.errf("%s: protocol %q: template %s serves %v", where, p, ss.Template, t.Spec.Protocols)
			}
		}
		// its racks, then its nodes and enclosures in them
		sel, err := selector.Parse(ss.Select)
		if err != nil {
			c.errf("%s: select: %v", where, err)
			continue
		}
		media := t.Spec.Media
		if len(media) == 0 {
			media = []string{"ssd"}
		}
		for _, r := range c.m.Racks {
			if ss.Select != "" && !sel.MatchPath(r.Path) {
				continue
			}
			for _, d := range c.m.Devices {
				if d.Rack != r {
					continue
				}
				switch d.BaseClass {
				case t.Spec.Nodes:
				case t.Spec.Enclosures:
					if t.Spec.Enclosures == "" {
						continue
					}
				default:
					continue
				}
				var pool []*model.Component
				for _, dr := range d.Drives() {
					if dr.Drive != nil && dr.Drive.Role != "boot" && slices.Contains(media, orSSD(dr.Drive.Media)) {
						pool = append(pool, dr)
					}
				}
				if len(pool) == 0 {
					continue // a device of the class with no pool drives (a rack's generic server) is not the system's
				}
				if owner := c.storageOwner(d); owner != "" {
					c.errf("%s: %s is already a node of storage system %s", where, d.Path, owner)
					continue
				}
				if d.BaseClass == t.Spec.Nodes {
					sys.Nodes = append(sys.Nodes, d)
				} else {
					sys.Enclosures = append(sys.Enclosures, d)
				}
				sys.Drives = append(sys.Drives, pool...)
				for _, dr := range pool {
					sys.RawBytes += dr.Drive.CapacityBytes
				}
			}
		}
		if len(sys.Nodes) == 0 {
			c.errf("%s: its racks (%q) hold no %s with %v data drives to be its storage nodes", where, ss.Select, t.Spec.Nodes, media)
			continue
		}
		minNodes, _ := toInt(sys.Params["minNodes"])
		if len(sys.Nodes) < minNodes {
			c.errf("%s: %d storage nodes; template %s needs at least %d", where, len(sys.Nodes), ss.Template, minNodes)
		}
		// protection: data+parity units, which the drives must hold
		prot := fmt.Sprint(sys.Params["protection"])
		d, p, ok := parseProtection(prot)
		if !ok {
			c.errf("%s: protection %q: want data+parity, e.g. 8+2", where, prot)
		} else if d+p > len(sys.Drives) {
			c.errf("%s: protection %s needs %d drives; its nodes and enclosures have %d", where, prot, d+p, len(sys.Drives))
		} else {
			sys.DataUnits, sys.ParityUnits = d, p
			sys.UsableBytes = sys.RawBytes / int64(d+p) * int64(d)
		}
		if n, ok := toInt(sys.Params["poolFullPercent"]); ok {
			sys.FullPercent = n
		}
		// networks by name, of the roles they are used for, reaching every node
		sys.DataNetwork, sys.ManagementNetwork = ss.Networks.Data, ss.Networks.Management
		for _, nb := range []struct{ use, name, role string }{{"data", ss.Networks.Data, "storage"}, {"management", ss.Networks.Management, "management"}} {
			if nb.name == "" {
				c.errf("%s: networks.%s is required (a network of role %s)", where, nb.use, nb.role)
				continue
			}
			n := c.m.Networks[nb.name]
			if n == nil {
				c.errf("%s: networks.%s: no network %q", where, nb.use, nb.name)
				continue
			}
			if n.Role != nb.role {
				c.errf("%s: networks.%s: network %s has role %s, not %s", where, nb.use, nb.name, n.Role, nb.role)
				continue
			}
			if nb.use == "data" {
				for _, node := range sys.Nodes {
					if !onNetwork(node, n) {
						c.errf("%s: storage node %s has no port on its data network %s", where, node.Path, nb.name)
					}
				}
			}
		}
		if sys.Fidelity == "vm" {
			for _, node := range sys.Nodes {
				if node.Fidelity != "vm" || node.OS != "twin" {
					c.errf("%s: fidelity vm: storage node %s is at %s (os %q); its nodes must be vm hosts with os twin (fidelity.overrides)", where, node.Path, node.Fidelity, node.OS)
				}
			}
		}
		c.m.StorageSystems = append(c.m.StorageSystems, sys)
		c.m.StorageBy[path] = sys
	}
}

// storageOwner names the storage system a device is already part of.
func (c *compiler) storageOwner(d *model.Device) string {
	for _, s := range c.m.StorageSystems {
		for _, x := range append(append([]*model.Device{}, s.Nodes...), s.Enclosures...) {
			if x == d {
				return s.Name
			}
		}
	}
	return ""
}

func onNetwork(d *model.Device, n *model.Network) bool {
	for _, p := range d.Ports {
		if p.Network == n.Name || (n.Overlay != "" && p.Network == n.Overlay) {
			return true
		}
	}
	return false
}

// parseProtection reads "data+parity" (8+2).
func parseProtection(s string) (data, parity int, ok bool) {
	a, b, found := strings.Cut(strings.TrimSpace(s), "+")
	if !found {
		return 0, 0, false
	}
	d, err1 := strconv.Atoi(a)
	p, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || d < 1 || p < 0 {
		return 0, 0, false
	}
	return d, p, true
}

func orSSD(m string) string {
	if m == "" {
		return "ssd"
	}
	return m
}

// Package nvue is NVUE, the object model and REST API Cumulus Linux is configured through,
// emulated for the generic/nvue-frr NOS (D56): a switch built from what Cumulus Linux is built
// from (Debian, FRR, ifupdown2, ISC dhcrelay) without Cumulus, configured the way Cumulus is.
//
// A configuration is NVUE's tree (`nv config show -o json`): interfaces, the bridge domain and
// its VLANs and VNIs, VRFs with their L3 VNIs, BGP, the NVE, EVPN, the DHCP relay. Revisions are
// NVUE's: a client creates one, patches the tree in it and applies it. Applying renders the tree
// as Cumulus does, to /etc/network/interfaces for ifupdown2 and /etc/frr/frr.conf for FRR, and the
// switch's own ifreload and frr-reload.py take the switch there; the relay is restarted with the
// tree's servers and interfaces. Only the part of NVUE's schema below is emulated: a path outside
// it is refused by name, so a stack never configures something the switch silently drops.
package nvue

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// Tree is an NVUE configuration tree: objects are map[string]any, leaves strings, numbers or
// booleans, as NVUE's JSON has them.
type Tree = map[string]any

// leaf kinds in the schema
type kind int

const (
	kString kind = iota + 1
	kInt
	kOnOff
	kPrefix // an address with its length
	kAddr
	kASN      // a number, "external" or "internal"
	kNeighbor // a BGP neighbour: an address, or an interface for an unnumbered session
	kMatch    // a prefix list's match: an address with its length, or "any"
	kAddrOrPrefix
)

// node is the schema: children by name ("*" for any name: an interface, a VRF, an address), or a
// leaf of a kind. A name key whose value is {} (an address, a server) is a node with no children.
type node struct {
	kids map[string]*node
	leaf kind
	// key, for "*": what the names must be
	key kind
}

func obj(kids map[string]*node) *node { return &node{kids: kids} }
func any_(child *node, key kind) *node {
	return &node{kids: map[string]*node{"*": child}, key: key}
}
func lf(k kind) *node { return &node{leaf: k} }

var empty = obj(map[string]*node{})

var enable = map[string]*node{"enable": lf(kOnOff)}

func with(m map[string]*node, more map[string]*node) map[string]*node {
	out := map[string]*node{}
	for k, v := range m {
		out[k] = v
	}
	for k, v := range more {
		out[k] = v
	}
	return out
}

// a route policy's direction on a BGP peer or peer group
var policy = obj(map[string]*node{
	"inbound":  obj(map[string]*node{"route-map": lf(kString), "prefix-list": lf(kString)}),
	"outbound": obj(map[string]*node{"route-map": lf(kString), "prefix-list": lf(kString)}),
})

// a BGP peer's or peer group's address family
func peerAF() *node {
	return obj(with(enable, map[string]*node{
		"policy":                    policy,
		"default-route-origination": obj(enable),
		"aspath":                    obj(map[string]*node{"allow-my-asn": obj(with(enable, map[string]*node{"occurrences": lf(kInt), "origin": lf(kOnOff)}))}),
	}))
}

var peerAFs = obj(map[string]*node{"ipv4-unicast": peerAF(), "ipv6-unicast": peerAF(), "l2vpn-evpn": peerAF()})

// what a BGP neighbour and a peer group have in common
var peerCommon = map[string]*node{
	"remote-as":               lf(kASN),
	"description":             lf(kString),
	"multihop-ttl":            lf(kInt),
	"update-source":           lf(kString),
	"password":                lf(kString),
	"passive-mode":            lf(kOnOff),
	"nexthop-connected-check": lf(kOnOff),
	"timers": obj(map[string]*node{
		"connection-retry": lf(kInt), "hold": lf(kInt), "keepalive": lf(kInt), "route-advertisement": lf(kString),
	}),
	"address-family": peerAFs,
}

// a VRF's BGP address family
func vrfAF() *node {
	return obj(with(enable, map[string]*node{
		"redistribute": obj(map[string]*node{"connected": obj(enable), "static": obj(enable)}),
		"route-export": obj(map[string]*node{"to-evpn": obj(enable)}),
		"route-import": obj(map[string]*node{"from-vrf": obj(with(enable, map[string]*node{
			"list": any_(empty, kString), "route-map": lf(kString),
		}))}),
		"multipaths": obj(map[string]*node{"ebgp": lf(kInt), "ibgp": lf(kInt)}),
		"network":    any_(empty, kPrefix),
	}))
}

var via = any_(obj(map[string]*node{"type": lf(kString)}), kString)

// schema is the part of NVUE's object model the twin emulates: what Cumulus Linux's fabric
// switches are configured with (D56), and what NICo's DPU agent writes to a BlueField's HBN (its
// FNN and Ethernet Virtualizer templates, D71).
var schema = obj(map[string]*node{
	"system": obj(map[string]*node{
		"hostname":      lf(kString),
		"api":           obj(map[string]*node{"listening-address": any_(empty, kAddr)}),
		"config":        obj(map[string]*node{"snippet": obj(map[string]*node{"frr.conf": lf(kString)})}),
		"reflexive-acl": obj(enable),
	}),
	"router": obj(map[string]*node{
		"bgp": obj(map[string]*node{"enable": lf(kOnOff), "autonomous-system": lf(kInt), "router-id": lf(kAddr)}),
		"vrr": obj(enable),
		"policy": obj(map[string]*node{
			"community-list": any_(obj(map[string]*node{"rule": any_(obj(map[string]*node{
				"action": lf(kString), "community": any_(empty, kString),
			}), kInt)}), kString),
			"as-path-list": any_(obj(map[string]*node{"rule": any_(obj(map[string]*node{
				"action": lf(kString), "aspath-exp": lf(kString),
			}), kInt)}), kString),
			"prefix-list": any_(obj(map[string]*node{
				"type": lf(kString),
				"rule": any_(obj(map[string]*node{
					"action": lf(kString),
					"match":  any_(obj(map[string]*node{"min-prefix-len": lf(kInt), "max-prefix-len": lf(kInt)}), kMatch),
				}), kInt),
			}), kString),
			"route-map": any_(obj(map[string]*node{"rule": any_(obj(map[string]*node{
				"action": obj(map[string]*node{"permit": empty, "deny": empty}),
				"match": obj(map[string]*node{
					"type": lf(kString), "ip-prefix-list": lf(kString), "ip-prefix-len": lf(kInt),
					"community-list": lf(kString), "evpn-route-type": lf(kString), "interface": lf(kString),
					"tag": lf(kInt), "as-path-list": lf(kString),
				}),
				"set": obj(map[string]*node{
					"community": any_(empty, kString), "large-community": any_(empty, kString), "tag": lf(kInt),
				}),
			}), kInt)}), kString),
		}),
	}),
	"interface": any_(obj(map[string]*node{
		"type":        lf(kString),
		"description": lf(kString),
		"vlan":        lf(kInt),
		"link":        obj(map[string]*node{"state": obj(map[string]*node{"up": empty, "down": empty}), "mtu": lf(kInt)}),
		"ip": obj(map[string]*node{
			"address": any_(empty, kPrefix),
			"vrf":     lf(kString),
			"vrr": obj(with(enable, map[string]*node{
				"address":     any_(empty, kPrefix),
				"mac-address": lf(kString),
				"state":       obj(map[string]*node{"up": empty, "down": empty}),
			})),
		}),
		"bridge": obj(map[string]*node{"domain": any_(obj(map[string]*node{"access": lf(kInt)}), kString)}),
		"acl": any_(obj(map[string]*node{
			"inbound":  obj(map[string]*node{"control-plane": empty}),
			"outbound": obj(map[string]*node{"control-plane": empty}),
		}), kString),
	}), kString),
	"bridge": obj(map[string]*node{"domain": any_(obj(map[string]*node{
		"vlan": any_(obj(map[string]*node{"vni": any_(empty, kInt)}), kInt),
	}), kString)}),
	"vrf": any_(obj(map[string]*node{
		"evpn":     obj(with(enable, map[string]*node{"vni": any_(empty, kInt)})),
		"loopback": obj(map[string]*node{"ip": obj(map[string]*node{"address": any_(empty, kPrefix)})}),
		"router": obj(map[string]*node{
			"bgp": obj(map[string]*node{
				"enable":            lf(kOnOff),
				"autonomous-system": lf(kInt),
				"router-id":         lf(kAddr),
				"rd":                lf(kString),
				"neighbor": any_(obj(with(peerCommon, map[string]*node{
					"type":       lf(kString),
					"peer-group": lf(kString),
				})), kNeighbor),
				"peer-group": any_(obj(peerCommon), kString),
				"address-family": obj(map[string]*node{
					"ipv4-unicast": vrfAF(),
					"ipv6-unicast": vrfAF(),
					"l2vpn-evpn":   obj(enable),
				}),
				"path-selection": obj(map[string]*node{"multipath": obj(map[string]*node{
					"aspath-ignore": lf(kOnOff), "bandwidth": lf(kString),
				})}),
				"route-export": obj(map[string]*node{"to-evpn": obj(map[string]*node{"route-target": any_(empty, kString)})}),
				"route-import": obj(map[string]*node{"from-evpn": obj(map[string]*node{"route-target": any_(empty, kString)})}),
			}),
			"static": any_(obj(map[string]*node{
				"address-family": lf(kString),
				"via":            via,
				"distance":       any_(obj(map[string]*node{"via": via}), kInt),
			}), kPrefix),
			"nexthop-tracking": obj(map[string]*node{
				"ipv4": obj(map[string]*node{"resolved-via-default": lf(kOnOff)}),
				"ipv6": obj(map[string]*node{"resolved-via-default": lf(kOnOff)}),
			}),
		}),
	}), kString),
	"nve": obj(map[string]*node{"vxlan": obj(map[string]*node{
		"enable":          lf(kOnOff),
		"source":          obj(map[string]*node{"address": lf(kAddr)}),
		"arp-nd-suppress": lf(kOnOff),
	})}),
	"evpn": obj(enable),
	"service": obj(map[string]*node{"dhcp-relay": any_(obj(map[string]*node{
		"server":    any_(empty, kAddr),
		"interface": any_(empty, kString),
	}), kString)}),
	"acl": any_(obj(map[string]*node{
		"type": lf(kString),
		"rule": any_(obj(map[string]*node{
			"action":     obj(map[string]*node{"permit": empty, "deny": empty}),
			"hw-offload": lf(kOnOff),
			"match": obj(map[string]*node{
				"ip": obj(map[string]*node{
					"source-ip": lf(kAddrOrPrefix), "dest-ip": lf(kAddrOrPrefix), "protocol": lf(kString),
					"source-port": any_(empty, kString), "dest-port": any_(empty, kString),
					"icmp-type": lf(kString), "icmpv6-type": lf(kString),
				}),
				"conntrack": obj(map[string]*node{"new": empty, "established": empty, "related": empty}),
			}),
		}), kInt),
	}), kString),
})

// Validate checks a tree, or a subtree at a path, against the emulated schema.
func Validate(path []string, v any) error {
	n := schema
	for i, p := range path {
		c, err := n.child(p)
		if err != nil {
			return fmt.Errorf("/%s: %w", strings.Join(path[:i+1], "/"), err)
		}
		n = c
	}
	return n.validate("/"+strings.Join(path, "/"), v)
}

func (n *node) child(name string) (*node, error) {
	if c, ok := n.kids[name]; ok {
		return c, nil
	}
	if c, ok := n.kids["*"]; ok {
		if err := checkKey(n.key, name); err != nil {
			return nil, err
		}
		return c, nil
	}
	var names []string
	for k := range n.kids {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("%q: nothing below here is emulated", name)
	}
	return nil, fmt.Errorf("%q is not part of the NVUE schema the twin emulates (here: %s)", name, strings.Join(names, ", "))
}

func checkKey(k kind, name string) error {
	if name == "" {
		return fmt.Errorf("an empty name")
	}
	switch k {
	case kInt:
		if _, err := strconv.Atoi(name); err != nil {
			return fmt.Errorf("%q is not a number", name)
		}
	case kPrefix:
		if _, err := netip.ParsePrefix(name); err != nil {
			return fmt.Errorf("%q is not an address with its prefix length", name)
		}
	case kAddr:
		if _, err := netip.ParseAddr(name); err != nil {
			return fmt.Errorf("%q is not an IP address", name)
		}
	case kNeighbor:
		if _, err := netip.ParseAddr(name); err != nil && !ifName(name) {
			return fmt.Errorf("%q is neither an IP address nor an interface", name)
		}
	case kMatch:
		if _, err := netip.ParsePrefix(name); err != nil && name != "any" {
			return fmt.Errorf("%q is neither an address with its prefix length nor any", name)
		}
	}
	return nil
}

// ifName is whether a name can be a Linux interface's
func ifName(s string) bool {
	return s != "" && len(s) <= 15 && !strings.ContainsAny(s, " /:")
}

func (n *node) validate(at string, v any) error {
	if v == nil {
		return nil // a deletion in a patch
	}
	if n.leaf != 0 {
		return checkLeaf(n.leaf, at, v)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: an object, not %v", at, v)
	}
	for k, cv := range m {
		c, err := n.child(k)
		if err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
		if err := c.validate(at+"/"+k, cv); err != nil {
			return err
		}
	}
	return nil
}

func checkLeaf(k kind, at string, v any) error {
	s := str(v)
	switch k {
	case kInt:
		if _, err := strconv.ParseUint(s, 10, 32); err != nil {
			return fmt.Errorf("%s: %v is not a number", at, v)
		}
	case kOnOff:
		if s != "on" && s != "off" {
			return fmt.Errorf("%s: %v is not on or off", at, v)
		}
	case kPrefix:
		if _, err := netip.ParsePrefix(s); err != nil {
			return fmt.Errorf("%s: %v is not an address with its prefix length", at, v)
		}
	case kAddr:
		if _, err := netip.ParseAddr(s); err != nil {
			return fmt.Errorf("%s: %v is not an IP address", at, v)
		}
	case kAddrOrPrefix:
		if _, err := netip.ParsePrefix(s); err != nil {
			if _, err := netip.ParseAddr(s); err != nil {
				return fmt.Errorf("%s: %v is not an address", at, v)
			}
		}
	case kASN:
		if _, err := strconv.ParseUint(s, 10, 32); err != nil && s != "external" && s != "internal" {
			return fmt.Errorf("%s: %v is not an AS number, external or internal", at, v)
		}
	case kString:
		if _, ok := v.(map[string]any); ok {
			return fmt.Errorf("%s: a value, not an object", at)
		}
	}
	return nil
}

// Clone deep-copies a tree.
func Clone(t Tree) Tree {
	b, _ := json.Marshal(t)
	var out Tree
	_ = json.Unmarshal(b, &out)
	if out == nil {
		out = Tree{}
	}
	return out
}

// Get is the subtree at a path, and whether it exists.
func Get(t Tree, path []string) (any, bool) {
	var cur any = t
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// Patch merges v into the tree at a path, as NVUE's PATCH does (RFC 7396: null deletes), creating
// the objects on the way.
func Patch(t Tree, path []string, v any) {
	if len(path) == 0 {
		if m, ok := v.(map[string]any); ok {
			merge(t, m)
		}
		return
	}
	cur := t
	for _, p := range path[:len(path)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
	last := path[len(path)-1]
	if v == nil {
		delete(cur, last)
		return
	}
	if m, ok := v.(map[string]any); ok {
		if existing, ok := cur[last].(map[string]any); ok {
			merge(existing, m)
			return
		}
		fresh := map[string]any{}
		merge(fresh, m)
		cur[last] = fresh
		return
	}
	cur[last] = v
}

func merge(dst, src map[string]any) {
	for k, v := range src {
		if v == nil {
			delete(dst, k)
			continue
		}
		if m, ok := v.(map[string]any); ok {
			d, ok := dst[k].(map[string]any)
			if !ok {
				d = map[string]any{}
				dst[k] = d
			}
			merge(d, m)
			continue
		}
		dst[k] = v
	}
}

// Delete removes the subtree at a path (NVUE's `nv unset`).
func Delete(t Tree, path []string) {
	if len(path) == 0 {
		for k := range t {
			delete(t, k)
		}
		return
	}
	Patch(t, path, nil)
}

// helpers for reading a tree

func sub(v any, path ...string) map[string]any {
	cur, _ := v.(map[string]any)
	for _, p := range path {
		if cur == nil {
			return nil
		}
		cur, _ = cur[p].(map[string]any)
	}
	return cur
}

func str(v any, path ...string) string {
	if len(path) == 0 {
		if v == nil {
			return ""
		}
		if f, ok := v.(float64); ok {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		return fmt.Sprint(v)
	}
	m := sub(v, path[:len(path)-1]...)
	if m == nil {
		return ""
	}
	x, ok := m[path[len(path)-1]]
	if !ok || x == nil {
		return ""
	}
	if f, ok := x.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(x)
}

func on(v any, path ...string) bool { return str(v, path...) == "on" }

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

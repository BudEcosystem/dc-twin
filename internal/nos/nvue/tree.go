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
	"regexp"
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
	kIfAddr // an interface's address: an address with its length, or dhcp (Cumulus's eth0)
	kEnum   // one of the node's enum values
	kUser   // a Linux user name (system aaa user)
	kTZ     // a time zone's name in the tz database (US/Eastern, Etc/UTC)
	kSpeed  // a port's speed: 400G, 100M, auto
	// kBreakout is a port's breakout: <n>x, optionally with the lanes' speed (4x, 4x25G), or
	// disabled, loopback
	kBreakout
	kHost // a server: an IP address or a DNS name
	// kToggle is a switch as 5.15 spells it: enabled or disabled
	kToggle
)

// node is the schema: children by name ("*" for any name: an interface, a VRF, an address), or a
// leaf of a kind. A name key whose value is {} (an address, a server) is a node with no children.
type node struct {
	kids map[string]*node
	leaf kind
	// key, for "*": what the names must be
	key kind
	// enum is what a kEnum leaf may be
	enum []string
	// choice: the children are alternatives with nothing below them, one at a time, as NVUE's
	// JSON writes an enumeration it keeps as an object (link state {"up": {}}): setting one
	// removes the others (withChoices)
	choice bool
	// since and removed are the releases that introduced and removed the node, and instead what
	// to use where it is refused (version.go's gate); zero: every release has it
	since, removed Version
	instead        string
	// rename, for a kToggle leaf (5.15's state), is the 5.14 switch it replaces (enable), which
	// normalize renders it as
	rename string
	// alias: a switch one release's CLI does not have (5.14's enable on 5.15 and later, 5.15's state
	// before) that a tree, a startup.yaml or a REST PATCH, may still carry (Q30): it is refused by
	// name on the CLI only
	alias bool
	// keyed, for a leaf: NVUE's JSON may hold the value as an object of one key ({"2x": {}})
	keyed bool
}

func obj(kids map[string]*node) *node { return &node{kids: kids} }
func any_(child *node, key kind) *node {
	return &node{kids: map[string]*node{"*": child}, key: key}
}
func lf(k kind) *node              { return &node{leaf: k} }
func oneOf(values ...string) *node { return &node{leaf: kEnum, enum: values} }
func choiceOf(names ...string) *node {
	n := &node{kids: map[string]*node{}, choice: true}
	for _, v := range names {
		n.kids[v] = empty
	}
	return n
}

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
	return obj(toggle(map[string]*node{
		"policy":                    policy,
		"default-route-origination": obj(enable),
		"aspath": obj(map[string]*node{
			"allow-my-asn": obj(with(enable, map[string]*node{"occurrences": lf(kInt), "origin": lf(kOnOff)})),
			// FRR's remove-private-AS (remove) and its replace-AS form (replace)
			"private-as": oneOf("remove", "replace"),
		}),
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
	// BFD for the session: on (enable on to 5.14, state enabled from 5.15), its timers, or a
	// profile of router bfd's. The profile form before 5.15 is UNCONFIRMED; it is accepted in
	// every release
	"bfd": obj(toggle(map[string]*node{
		"profile":           lf(kString),
		"detect-multiplier": lf(kInt),
		"min-rx-interval":   lf(kInt),
		"min-tx-interval":   lf(kInt),
	})),
}

// a VRF's BGP address family
func vrfAF() *node {
	redistribute := func() *node { return obj(toggle(map[string]*node{"route-map": lf(kString)})) }
	return obj(toggle(map[string]*node{
		// static's switch is renamed as connected's is (5.15 names connected; static UNCONFIRMED)
		"redistribute": obj(map[string]*node{"connected": redistribute(), "static": redistribute()}),
		"route-export": obj(map[string]*node{"to-evpn": obj(toggle(nil))}),
		"route-import": obj(map[string]*node{"from-vrf": obj(toggle(map[string]*node{
			"list": any_(empty, kString), "route-map": lf(kString),
		}))}),
		"multipaths": obj(map[string]*node{"ebgp": lf(kInt), "ibgp": lf(kInt)}),
		"network":    any_(empty, kPrefix),
	}))
}

var via = any_(obj(map[string]*node{"type": lf(kString)}), kString)

// link is an interface's physical layer (`nv set interface <swp> link …`, Cumulus Linux 5.14's
// Switch Port Attributes, docs/reviews/2026-11-11-day0-vendor-facts.md C.3): its admin state and
// MTU, which ifupdown2 applies, and its speed, auto-negotiation and breakout, which are accepted
// and recorded (and read back by `nv show interface`) and do nothing, because a port of the twin
// is a TAP or a veth with no PHY to set and no lanes to split (18 — Fidelity). A breakout's
// subports (swp1s0 to swp1s3) are therefore never created: an interface named after one is
// refused at render, saying so.
var link = obj(map[string]*node{
	"state":          choiceOf("up", "down"),
	"mtu":            lf(kInt),
	"speed":          lf(kSpeed),
	"auto-negotiate": lf(kOnOff),
	// NVUE's JSON keys a breakout ({"2x": {}}, as NVCM's startup.yaml writes it); `nv set … link
	// breakout 2x` gives the same tree. A bare string is still taken, as the twin's own trees have it
	"breakout": &node{leaf: kBreakout, keyed: true},
})

// Roles are the roles of `nv set system aaa user <name> role` (Cumulus Linux 5.14's User
// Accounts page, vendor facts C.3), with the Linux groups each gives its user, as Cumulus maps
// them on that page: nvshow lets a user read through NVUE, nvset stage changes, nvapply apply
// them; a system administrator is also in sudo, as the factory user cumulus is (vendor facts B).
// The groups are read from the documentation, not from a switch, and are UNCONFIRMED. A role
// `system aaa role` defines from classes maps to groups the same way (system.go's roleGroups).
var Roles = map[string][]string{
	"system-admin": {"sudo", "nvshow", "nvset", "nvapply"},
	"nvue-admin":   {"nvshow", "nvset", "nvapply"},
	"nvue-monitor": {"nvshow"},
}

// DefaultRole is a user's role when its configuration names none: the least, since what NVUE
// gives a user with no role is not documented (UNCONFIRMED).
const DefaultRole = "nvue-monitor"

// schema is the part of NVUE's object model the twin emulates: what Cumulus Linux's fabric
// switches are configured with (D56), what NICo's DPU agent writes to a BlueField's HBN (its
// FNN and Ethernet Virtualizer templates, D71), and what NVIDIA's own day-0 tool configures (its
// net-configurator's spine and leaf templates, nc-v8.0.0, Cumulus 5.18). It holds the grammar
// before Cumulus Linux 5.15 and the grammar since (version.go): a node one release does not have
// is gated (since, until), and refused there by name.
var schema = obj(map[string]*node{
	// system: the switch's identity, users, time, API and logging, as a day-0 push sets them
	// (18 — The first push; vendor facts C.3). timezone is the form before Cumulus Linux 5.15,
	// date-time timezone the form since (and, the emulation has always taken, before: the
	// release it appeared in is UNCONFIRMED); the second wins.
	"system": obj(map[string]*node{
		"hostname":  lf(kString),
		"timezone":  until(v515, "system date-time timezone", lf(kTZ)),
		"date-time": obj(map[string]*node{"timezone": lf(kTZ)}),
		"aaa": obj(map[string]*node{
			"user": any_(obj(with(state, map[string]*node{
				"password":        lf(kString),
				"hashed-password": lf(kString),
				// a built-in role (Roles) or one `system aaa role` defines: Render refuses
				// any other
				"role":      lf(kString),
				"full-name": lf(kString),
			})), kUser),
			// NVUE's role-based access (net-configurator restates Cumulus's built-in classes and
			// roles with them): a class allows or denies command paths with a permission, a role
			// is classes. Recorded, and mapped to the Linux groups the switch authorises by
			// (renderSystem's roleGroups)
			"class": any_(obj(map[string]*node{
				"action":       oneOf("allow", "deny"),
				"command-path": any_(obj(map[string]*node{"permission": oneOf("ro", "rw", "act", "all")}), kString),
			}), kString),
			"role": any_(obj(map[string]*node{"class": any_(empty, kString)}), kString),
			// the order PAM tries the methods in: recorded; the twin's switch has local
			// accounts only
			"authentication": obj(map[string]*node{"order": any_(empty, kString)}),
		}),
		// state is recorded: NVUE's REST API is the twin's and keeps serving (disabling it would
		// cut off the client that disabled it, which the twin does not emulate)
		"api": obj(with(state, map[string]*node{"listening-address": any_(empty, kAddr)})),
		// 5.13's form (vendor facts C.3); the older service syslog <vrf> is not emulated
		"syslog": obj(map[string]*node{"server": any_(obj(map[string]*node{
			"port":     lf(kInt),
			"protocol": oneOf("udp", "tcp"),
			"vrf":      lf(kString),
		}), kHost)}),
		"config": obj(map[string]*node{
			"snippet": obj(map[string]*node{"frr.conf": lf(kString)}),
			// auto-save (5.9, vendor facts C.2): recorded; an apply does not save, `nv config
			// save` does
			"auto-save": since(v59, "nv config save after an apply", obj(toggle(nil))),
		}),
		"reflexive-acl": obj(enable),
		// 5.15's NTP, DNS and LLDP: service ntp <vrf> server, service dns <vrf> server and
		// service lldp before (normalize renders them as those). association-type pool is
		// chrony's pool
		"ntp": since(v515, "service ntp <vrf> server <host>", obj(map[string]*node{
			"server": any_(obj(with(state, map[string]*node{"iburst": lf(kOnOff), "association-type": oneOf("server", "pool")})), kHost),
			"vrf":    lf(kString),
		})),
		"dns":  since(v515, "service dns <vrf> server <address>", obj(map[string]*node{"server": any_(empty, kAddr)})),
		"lldp": since(v515, "service lldp", lldp),
		// the login banners, sshd's (Banner /etc/issue.net, before the password) and the message
		// of the day after a login (/etc/motd)
		"message": obj(map[string]*node{"pre-login": lf(kString), "post-login": lf(kString)}),
		// state is recorded: the switch's sshd keeps running (it is how the twin's own day-0
		// checks reach a factory switch)
		"ssh-server": obj(state),
		// SNMP (system snmp-server since 5.11): snmpd, read-only communities by source prefix,
		// on the listening addresses (localhost, or an address)
		"snmp-server": since(v511, "no SNMP is configured through NVUE", obj(toggle(map[string]*node{
			"listening-address":  any_(obj(map[string]*node{"vrf": lf(kString)}), kHost),
			"readonly-community": any_(obj(map[string]*node{"access": any_(empty, kMatch)}), kString),
		}))),
		// What Just Happened: Spectrum's ASIC reports the packets it drops and why. Recorded:
		// the twin forwards in the kernel, with no ASIC to report from (18 — Fidelity)
		"wjh": obj(toggle(map[string]*node{
			"channel": any_(obj(map[string]*node{"trigger": any_(empty, kString)}), kString),
		})),
		// The rest is recorded and rendered to nothing: what the ASIC or the platform does, which
		// the twin's kernel forwarding has no part of (18 — Fidelity). global is the switch's
		// fabric-wide MACs and ids (EVPN multihoming's and VRR's); security password-hardening is
		// PAM's password rules, which the twin's PAM does not enforce; reboot mode is how the next
		// reboot runs (cold, warm, fast)
		"global": obj(map[string]*node{
			"fabric-mac": lf(kString), "fabric-id": lf(kInt), "anycast-mac": lf(kString),
			"anycast-id": lf(kInt), "system-mac": lf(kString),
		}),
		"security": obj(map[string]*node{"password-hardening": obj(state)}),
		"reboot":   obj(map[string]*node{"mode": oneOf("cold", "warm", "fast")}),
		// ACLs on the switch's own traffic (the control plane), as `acl <name>` defines them:
		// rendered as an interface's control-plane ACL is, on the input hook
		"control-plane": obj(map[string]*node{"acl": any_(obj(map[string]*node{
			"inbound": empty, "outbound": empty,
		}), kString)}),
	}),
	"router": obj(map[string]*node{
		"bgp": obj(toggle(map[string]*node{
			"autonomous-system": lf(kInt),
			"router-id":         lf(kAddr),
			// graceful restart, rendered in the default VRF's router bgp: full is FRR's bgp
			// graceful-restart, helper-only FRR's default, off bgp graceful-restart-disable
			"graceful-restart": obj(map[string]*node{
				"mode":                         oneOf("full", "helper-only", "off"),
				"restart-time":                 lf(kInt),
				"path-selection-deferral-time": lf(kInt),
				"stale-routes-time":            lf(kInt),
			}),
		})),
		"vrr": obj(toggle(nil)),
		// Spectrum's adaptive routing: recorded (18 — Fidelity), on the router and per port
		"adaptive-routing": obj(toggle(map[string]*node{"link-utilization-threshold": lf(kOnOff)})),
		// BFD's profiles (FRR's bfd profile), which a peer's bfd profile names. The switch is
		// renamed in 5.15 as router bgp's is (UNCONFIRMED for bfd)
		"bfd": obj(toggle(map[string]*node{
			"profile": any_(obj(map[string]*node{
				"detect-multiplier": lf(kInt),
				"min-rx-interval":   lf(kInt),
				"min-tx-interval":   lf(kInt),
			}), kString),
		})),
		// PTM's check of the cabling against topology.dot, as NVUE controls it: FRR's
		// check-link-state hook only (vendor facts B). Accepted and recorded; the twin runs no
		// ptmd (18 — LLDP and cabling)
		"ptm": obj(enable),
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
				"action":      choiceOf("permit", "deny"),
				"description": lf(kString),
				"match": obj(map[string]*node{
					"type": lf(kString), "ip-prefix-list": lf(kString), "ip-prefix-len": lf(kInt),
					"community-list": lf(kString), "evpn-route-type": lf(kString), "interface": lf(kString),
					"tag": lf(kInt), "as-path-list": lf(kString),
				}),
				"set": obj(map[string]*node{
					"community": any_(empty, kString), "large-community": any_(empty, kString), "tag": lf(kInt),
					// the link bandwidth extended community (weighted ECMP): a number, or
					// cumulative, multipaths and their -non-transitive forms
					"ext-community-bw": lf(kString),
				}),
			}), kInt)}), kString),
		}),
	}),
	"interface": any_(obj(map[string]*node{
		"type":        lf(kString),
		"description": lf(kString),
		"vlan":        lf(kInt),
		"link":        link,
		// to 5.14 an interface's addresses, gateway, VRF and VRR are under ip; from 5.15 under
		// ipv4 and ipv6, with its VRF at interface <id> vrf and VRR's up|down at vrr-state
		"ip": until(v515, "ipv4 (address, gateway, vrr), ipv6, and interface <id> vrf", obj(map[string]*node{
			"address": any_(empty, kIfAddr),
			"gateway": any_(empty, kAddr),
			"vrf":     lf(kString),
			"vrr": obj(with(enable, map[string]*node{
				"address":     any_(empty, kPrefix),
				"mac-address": lf(kString),
				"state":       choiceOf("up", "down"),
			})),
		})),
		"ipv4": since(v515, "ip", obj(map[string]*node{
			"address": any_(empty, kIfAddr),
			"gateway": any_(empty, kAddr),
			"vrr": obj(toggle(map[string]*node{
				"address":     any_(empty, kPrefix),
				"mac-address": lf(kString),
				"vrr-state":   choiceOf("up", "down"),
			})),
		})),
		"ipv6": since(v515, "ip", obj(map[string]*node{
			"address": any_(empty, kPrefix),
			"gateway": any_(empty, kAddr),
		})),
		"vrf": since(v515, "ip vrf", lf(kString)),
		"bridge": obj(map[string]*node{"domain": any_(obj(map[string]*node{
			"access": lf(kInt),
			// a port's STP: recorded with the bridge's (bridge domain stp)
			"stp": obj(map[string]*node{
				"admin-edge": lf(kOnOff), "auto-edge": lf(kOnOff), "bpdu-guard": lf(kOnOff),
				"bpdu-filter": lf(kOnOff), "network": lf(kOnOff), "restrrole": lf(kOnOff),
			}),
		}), kString)}),
		// recorded: the ASIC's PFC watchdog and adaptive routing (18 — Fidelity). NVCM writes the
		// watchdog's state enable|disable in every release
		"qos":    obj(map[string]*node{"pfc-watchdog": obj(map[string]*node{"state": oneOf("enable", "disable")})}),
		"router": obj(map[string]*node{"adaptive-routing": obj(toggle(map[string]*node{"link-utilization-threshold": lf(kInt)}))}),
		// EVPN multihoming on a port: uplink is FRR's evpn mh uplink (a core-facing port whose loss
		// takes the segments down); segment, on a bond, its Ethernet segment
		"evpn": obj(map[string]*node{"multihoming": obj(map[string]*node{
			"uplink": lf(kOnOff),
			"segment": obj(toggle(map[string]*node{
				"df-preference": lf(kInt), "local-id": lf(kInt), "mac-address": lf(kString), "identifier": lf(kString),
			})),
		})}),
		// a bond (ifupdown2's bond-slaves, the kernel's 802.3ad), its members and LACP
		"bond": obj(map[string]*node{
			"member":      any_(empty, kString),
			"lacp-bypass": lf(kOnOff),
			"mode":        oneOf("lacp", "static"),
			"lacp-rate":   oneOf("fast", "slow"),
			"down-delay":  lf(kInt),
			"up-delay":    lf(kInt),
		}),
		"acl": any_(obj(map[string]*node{
			"inbound":  obj(map[string]*node{"control-plane": empty}),
			"outbound": obj(map[string]*node{"control-plane": empty}),
		}), kString),
	}), kString),
	"bridge": obj(map[string]*node{"domain": any_(obj(map[string]*node{
		"vlan": any_(obj(map[string]*node{"vni": any_(empty, kInt)}), kInt),
		// the bridge's spanning tree: recorded (ifupdown2's mstpctl attributes need mstpd, which the
		// image does not run; the twin's fabrics have no loops for it to break, 18 — Fidelity)
		"stp": obj(map[string]*node{"priority": lf(kInt), "state": choiceOf("up", "down")}),
	}), kString)}),
	"vrf": any_(obj(map[string]*node{
		// vlan: the L3 VNI's VLAN, NVUE's choice when unset (Cumulus picks one from its reserved range)
		"evpn": obj(toggle(map[string]*node{"vni": any_(empty, kInt), "vlan": lf(kInt)})),
		// a VRF's loopback: ip address, or ipv4 address as 5.15 names an interface's. Both are taken
		// in every release: NVCM 1.4.1's 5.16.1 configurations write ipv4 for one role (cleaf) and ip
		// for another (tor), and which a 5.15+ switch refuses is UNCONFIRMED (Q30)
		"loopback": obj(map[string]*node{
			"ip":   obj(map[string]*node{"address": any_(empty, kPrefix)}),
			"ipv4": obj(map[string]*node{"address": any_(empty, kPrefix)}),
		}),
		"router": obj(map[string]*node{
			"bgp": obj(toggle(map[string]*node{
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
					"l2vpn-evpn":   obj(toggle(nil)),
				}),
				// aspath-ignore is on|off to 5.14 and enabled|disabled from 5.15 (net-configurator's
				// templates), as every on|off leaf is taken there
				"path-selection": obj(map[string]*node{"multipath": obj(map[string]*node{
					"aspath-ignore": lf(kOnOff), "bandwidth": lf(kString),
				})}),
				"route-export": obj(map[string]*node{"to-evpn": obj(map[string]*node{"route-target": any_(empty, kString)})}),
				"route-import": obj(map[string]*node{"from-evpn": obj(map[string]*node{"route-target": any_(empty, kString)})}),
			})),
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
	"nve": obj(map[string]*node{"vxlan": obj(toggle(map[string]*node{
		"source":          obj(map[string]*node{"address": lf(kAddr)}),
		"arp-nd-suppress": lf(kOnOff),
	}))}),
	"evpn": obj(toggle(map[string]*node{
		// EVPN multihoming (FRR's evpn mh): its timers; the segments are on bonds
		"multihoming": obj(toggle(map[string]*node{
			"startup-delay": lf(kInt), "mac-holdtime": lf(kInt), "neighbor-holdtime": lf(kInt),
		})),
		// what EVPN advertises beyond MACs: an SVI's address (advertise-svi-ip) and the gateway's
		// MAC and address (advertise-default-gw)
		"route-advertise": obj(map[string]*node{
			"svi-ip": lf(kOnOff), "default-gateway": lf(kOnOff), "nexthop-setting": lf(kString),
		}),
	})),
	"service": obj(map[string]*node{
		"dhcp-relay": any_(obj(map[string]*node{
			"server":    any_(empty, kAddr),
			"interface": any_(empty, kString),
			// the relay as server groups: each group's servers and the interfaces towards them,
			// and the downstream interfaces relayed to a group (NVCM's form; dhcrelay -iu and -id)
			"server-group": any_(obj(map[string]*node{
				"server":             any_(empty, kAddr),
				"upstream-interface": any_(empty, kString),
			}), kString),
			"downstream-interface": any_(obj(map[string]*node{"server-group-name": lf(kString)}), kString),
		}), kString),
		// per VRF, as Cumulus has them to 5.14 (vendor facts C.3): service ntp <vrf> server <host>
		// and service dns <vrf> server <address>; system ntp, dns and lldp from 5.15
		"ntp":  until(v515, "system ntp server <host> (and system ntp vrf <vrf>)", any_(obj(map[string]*node{"server": any_(obj(map[string]*node{"iburst": lf(kOnOff)}), kHost)}), kString)),
		"dns":  until(v515, "system dns server <address>", any_(obj(map[string]*node{"server": any_(empty, kAddr)}), kString)),
		"lldp": until(v515, "system lldp", lldp),
	}),
	// qos roce: accepted and recorded, rendered to nothing: the twin forwards in the kernel, with
	// no ASIC buffers, PFC or ECN behind it (18 — Fidelity). `nv set qos roce` alone is mode
	// lossless, as on Cumulus (vendor facts C.3). traffic-pool (the ASIC's shared buffer split
	// between pools) likewise
	"qos": obj(map[string]*node{
		"roce": obj(toggle(map[string]*node{"mode": oneOf("lossless", "lossy")})),
		// priority flow control's buffers and thresholds: recorded, as roce is
		"pfc": any_(obj(map[string]*node{
			"port-buffer": lf(kInt), "switch-priority": any_(empty, kInt), "xoff-threshold": lf(kInt),
			"xon-threshold": lf(kInt), "cable-length": lf(kInt), "rx": lf(kString), "tx": lf(kString),
		}), kString),
		"traffic-pool": any_(obj(map[string]*node{
			"memory-percent":  lf(kInt),
			"switch-priority": any_(empty, kInt),
		}), kString),
	}),
	"acl": any_(obj(map[string]*node{
		"type": lf(kString),
		"rule": any_(obj(map[string]*node{
			"action":     choiceOf("permit", "deny"),
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

// lldp is lldpd's settings: service lldp to 5.14, system lldp from 5.15
var lldp = obj(map[string]*node{
	"state": oneOf("enabled", "disabled"),
	// lldpd's TLVs and transmit mode: recorded (lldpd sends what it sends)
	"dot1-tlv":           lf(kOnOff),
	"mode":               oneOf("default", "force-send-lldp"),
	"tx-interval":        lf(kInt),
	"tx-hold-multiplier": lf(kInt),
})

// Validate checks a tree, or a subtree at a path, against the emulated schema, in any release's
// grammar.
func Validate(path []string, v any) error { return ValidateAt(Version{}, path, v) }

// ValidateAt checks a tree, or a subtree at a path, against the emulated schema as a release has
// it: a node the release does not have is refused by name, with what to use instead.
func ValidateAt(ver Version, path []string, v any) error {
	n := schema
	for i, p := range path {
		c, err := n.childAt(ver, p)
		if err != nil {
			return fmt.Errorf("/%s: %w", strings.Join(path[:i+1], "/"), err)
		}
		n = c
	}
	return n.validate(ver, "/"+strings.Join(path, "/"), v)
}

func (n *node) child(name string) (*node, error) { return n.childAt(Version{}, name) }

// childAt is a child as a tree names it; childCLI as the CLI does (gateIn).
func (n *node) childAt(ver Version, name string) (*node, error)  { return n.childIn(ver, name, false) }
func (n *node) childCLI(ver Version, name string) (*node, error) { return n.childIn(ver, name, true) }

func (n *node) childIn(ver Version, name string, cli bool) (*node, error) {
	if c, ok := n.kids[name]; ok {
		if err := c.gateIn(ver, name, cli); err != nil {
			return nil, err
		}
		return c, nil
	}
	if c, ok := n.kids["*"]; ok {
		if err := checkKey(n.key, name); err != nil {
			return nil, err
		}
		return c, nil
	}
	var names []string
	for k, c := range n.kids {
		if c.gate(ver, k) == nil {
			names = append(names, k)
		}
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
	case kIfAddr:
		if _, err := netip.ParsePrefix(name); err != nil && name != "dhcp" {
			return fmt.Errorf("%q is not an address with its prefix length, or dhcp", name)
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
	case kUser:
		if !userName.MatchString(name) {
			return fmt.Errorf("%q is not a user name", name)
		}
		if name == "root" {
			return fmt.Errorf("root: NVUE does not manage the root account (it is locked, vendor facts B)")
		}
	case kHost:
		if _, err := netip.ParseAddr(name); err != nil && !hostName.MatchString(name) {
			return fmt.Errorf("%q is neither an IP address nor a host name", name)
		}
	}
	return nil
}

var (
	// a user name as Debian's useradd takes it, upper case included: NVIDIA Config Manager's
	// bootstrap data names its account nvConfigManager (whether NVUE's own pattern allows upper
	// case is UNCONFIRMED; Cumulus's useradd does)
	userName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,31}$`)
	hostName  = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*\.?$`)
	tzName    = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)
	speedRE   = regexp.MustCompile(`^([0-9]+[MG]|auto)$`)
	breakout  = regexp.MustCompile(`^([1-8])x([0-9]+G)?$|^disabled$|^loopback$`)
	subportRE = regexp.MustCompile(`^swp[0-9]+s[0-9]+$`)
)

// ifName is whether a name can be a Linux interface's
func ifName(s string) bool {
	return s != "" && len(s) <= 15 && !strings.ContainsAny(s, " /:")
}

func (n *node) validate(ver Version, at string, v any) error {
	if v == nil {
		return nil // a deletion in a patch
	}
	if n.leaf == kEnum {
		s := str(v)
		for _, e := range n.enum {
			if s == e {
				return nil
			}
		}
		return fmt.Errorf("%s: %v is not one of %s", at, v, strings.Join(n.enum, ", "))
	}
	if n.leaf != 0 {
		if m, ok := v.(map[string]any); ok && n.keyed && len(m) == 1 {
			for k, e := range m {
				if em, ok := e.(map[string]any); ok && len(em) == 0 {
					return checkLeaf(ver, n.leaf, at, k)
				}
			}
		}
		return checkLeaf(ver, n.leaf, at, v)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: an object, not %v", at, v)
	}
	for k, cv := range m {
		c, err := n.childAt(ver, k)
		if err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
		if err := c.validate(ver, at+"/"+k, cv); err != nil {
			return err
		}
	}
	return nil
}

func checkLeaf(ver Version, k kind, at string, v any) error {
	s := str(v)
	switch k {
	case kInt:
		if _, err := strconv.ParseUint(s, 10, 32); err != nil {
			return fmt.Errorf("%s: %v is not a number", at, v)
		}
	case kOnOff:
		// 5.15 spells an on|off leaf enabled|disabled (net-configurator's path-selection
		// multipath aspath-ignore enabled); whether it still takes on|off is UNCONFIRMED, and
		// it is taken
		new := s == "enabled" || s == "disabled"
		if new && !ver.IsZero() && ver.Less(v515) {
			return fmt.Errorf("%s: %v is Cumulus Linux 5.15's spelling and this switch runs %s: on or off", at, v, ver)
		}
		if s != "on" && s != "off" && !new {
			return fmt.Errorf("%s: %v is not on or off", at, v)
		}
	case kToggle:
		if s != "enabled" && s != "disabled" {
			return fmt.Errorf("%s: %v is not enabled or disabled", at, v)
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
	case kTZ:
		if !tzName.MatchString(s) || strings.Contains(s, "..") {
			return fmt.Errorf("%s: %v is not a time zone's name (US/Eastern, Etc/UTC)", at, v)
		}
	case kSpeed:
		if !speedRE.MatchString(s) {
			return fmt.Errorf("%s: %v is not a speed (400G, 100M, auto)", at, v)
		}
	case kBreakout:
		if !breakout.MatchString(s) {
			return fmt.Errorf("%s: %v is not a breakout (4x, 4x25G, disabled, loopback)", at, v)
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

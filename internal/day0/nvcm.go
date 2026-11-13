package day0

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"path/filepath"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// The design as NVIDIA Config Manager's own loader reads it (18 — Z7b, D80): a Design Builder
// blueprint for its MockTopologyDesign job (development/mock_topology at 1.4.1): context/<bp>/
// locations.yaml (the location tree and the site's config contexts), prefixes.yaml (the
// aggregates), bgp_routing_instances.yaml (each switch's AS), and devices/<hostname>.json (each
// switch in Nautobot's GraphQL shape: identity, interfaces, addresses, cables). Written beside the
// NetBox CSVs, for the Cumulus switches of the design (NVCM has no SONiC templates).
//
// What a design has no value for and NVCM needs, written as a site would fill it:
//   - each switch's management address, which NVCM reserves in its Kea: from its rack's management
//     network (the relay's subnet), host NVCMMgmtHostBase + the switch's place in the rack;
//   - the ZTP server's address (ztp.ipv4, and DHCP's {{ztp_server}}): NVCMZTPServer, a token the
//     site's tooling replaces with the address its ZTP service answers at;
//   - the intended firmware: the release the switch runs (NVCMRelease unless the estate says).
//
// Roles: a leaf is NVCM's In-Band-Leaf, a spine its Converged-Spine, an OOB switch its OOB-Leaf.
//
// An OOB-Leaf's access segment (its BMCs, 18 — Z7d) is NVCM's VLAN 201 in VRF OOB, as its
// templates have it: vlan201 at the segment's third address (VRR takes the gateway, the first),
// an L2 VNI for the VLAN, VRF OOB with its L3 VNI on VLAN 4002 and a loopback (lo-oob) from
// NVCMOOBLoopbacks, the access ports untagged in 201. Its relay's helpers are the site's BMC DHCP
// (NVCMBMCDHCP, a token the site's tooling fills, written to helpers.json for the loader to relate
// to VLAN 201, which the blueprint format cannot). A switch the twin configures that an exported
// switch is cabled to (the OOB aggregation switch) is in the blueprint too, as a device NVCM does
// not provision (no eth0, so no reservation): the leaves' underlay and EVPN peer.
const (
	NVCMBlueprint    = "dctwin"
	NVCMZTPServer    = "@ZTP_SERVER@"
	NVCMBMCDHCP      = "@BMC_DHCP@"
	NVCMMgmtHostBase = 200
	NVCMRelease      = "5.14.0"
	NVCMOOBVRF       = "OOB"
	NVCMOOBL3VNI     = 2000
	NVCMOOBL3VLAN    = 4002
	NVCMBMCVLAN      = 201
	NVCMBMCVNI       = 2001
	NVCMOOBLoopbacks = "10.254.240.0/24"
)

var nvcmRoles = map[string]string{"leaf": "In-Band-Leaf", "spine": "Converged-Spine", "oob": "OOB-Leaf"}

// NVCMMgmtAddrs is each Cumulus switch's management address in the blueprint, with its prefix
// length: hostname -> 10.30.3.200/24.
func NVCMMgmtAddrs(d Design) map[string]string {
	out := map[string]string{}
	byRack := map[string][]Switch{}
	for _, s := range d.Switches {
		// the switches NVCM brings up: factory Cumulus switches (one the twin configures is the
		// estate's, and its management port is not NVCM's to reserve)
		if !s.Factory || strings.Contains(s.NOS, "sonic") || nvcmRoles[s.Role] == "" {
			continue
		}
		byRack[s.Rack] = append(byRack[s.Rack], s)
	}
	for rack, sws := range byRack {
		var pfx netip.Prefix
		for _, m := range d.Mgmt {
			// a network's rack is its path (site-1/hall-a/n01), a switch's its name
			if (m.Rack == rack || strings.HasSuffix(m.Rack, "/"+rack)) && m.Prefix != "" {
				pfx, _ = netip.ParsePrefix(m.Prefix)
			}
		}
		if !pfx.IsValid() {
			continue
		}
		sort.Slice(sws, func(i, j int) bool { return sws[i].Path < sws[j].Path })
		for i, s := range sws {
			a := pfx.Masked().Addr()
			for k := 0; k < NVCMMgmtHostBase+i; k++ {
				a = a.Next()
			}
			if pfx.Contains(a) {
				out[s.Hostname] = fmt.Sprintf("%s/%d", a, pfx.Bits())
			}
		}
	}
	return out
}

func nvcmFiles(d Design, release string) (map[string][]byte, error) {
	if release == "" {
		release = NVCMRelease
	}
	site := d.Site
	if site == "" {
		site = d.Estate
	}
	site = strings.ToUpper(site)
	module := site + " fabric - {{ deployment_name }}"
	mgmt := NVCMMgmtAddrs(d)
	byName := map[string]Switch{}
	for _, s := range d.Switches {
		byName[s.Hostname] = s
	}
	// the switches NVCM provisions, and the ones the twin configures that they are cabled to
	exported := map[string]bool{}
	for h := range mgmt {
		exported[h] = true
	}
	anchors := map[string]bool{}
	for _, s := range d.Switches {
		if !exported[s.Hostname] {
			continue
		}
		for _, it := range s.Interfaces {
			peer, _, _ := strings.Cut(it.Peer, ":")
			if p, ok := byName[peer]; ok && it.Kind == "fabric" && !exported[peer] && !strings.Contains(p.NOS, "sonic") && p.Loopback != "" && nvcmAnchorRole(p) != "" {
				anchors[peer] = true
			}
		}
	}
	for h := range anchors {
		exported[h] = true
	}
	// each OOB-Leaf's VRF loopback, in path order
	oobLo := map[string]string{}
	oobSVI := map[string]netip.Prefix{}
	var bmcNet netip.Prefix
	{
		var leaves []Switch
		for _, s := range d.Switches {
			if mgmt[s.Hostname] != "" && s.Role == "oob" && len(nvcmBMCVlan(s).Ports) > 0 {
				leaves = append(leaves, s)
			}
		}
		sort.Slice(leaves, func(i, j int) bool { return leaves[i].Path < leaves[j].Path })
		a := netip.MustParsePrefix(NVCMOOBLoopbacks).Addr()
		var segs []netip.Prefix
		for _, s := range leaves {
			a = a.Next()
			oobLo[s.Hostname] = a.String() + "/32"
			if g, err := netip.ParsePrefix(nvcmBMCVlan(s).Gateway); err == nil {
				segs = append(segs, g.Masked())
			}
		}
		// VLAN 201 is one L2 segment across every OOB-Leaf (its L2 VNI), so it is one subnet, as
		// NVCM's designs have it: the smallest prefix holding every leaf's rack segment, VRR at its
		// first address, each leaf's vlan201 from its third on (the BMC DHCP keeps its first ten
		// back, as NICo's segments do)
		bmcNet = nvcmCover(segs)
		svi := bmcNet.Addr().Next().Next()
		for _, s := range leaves {
			oobSVI[s.Hostname] = netip.PrefixFrom(svi, bmcNet.Bits())
			svi = svi.Next()
		}
	}
	var helpers []any
	if len(oobLo) > 0 {
		helpers = append(helpers, map[string]any{"vid": NVCMBMCVLAN, "helpers": []string{NVCMBMCDHCP}, "prefix": bmcNet.String(),
			"gateway": bmcNet.Addr().Next().String()})
	}
	files := map[string][]byte{}
	bp := filepath.Join("context", NVCMBlueprint)
	aggregates := map[string]string{} // prefix -> role
	cover := func(a, role string) {
		p, err := netip.ParsePrefix(a)
		if err != nil {
			return
		}
		bits := 24
		if p.Bits() < bits {
			bits = p.Bits()
		}
		agg, _ := p.Addr().Prefix(bits)
		aggregates[agg.String()] = role
	}
	contexts := func(fabricMAC string) map[string]any {
		return map[string]any{
			// password_mappings is NVCM's own, scoped to its Cumulus Linux platform by its bootstrap
			// data: a device's copy has its keys camel-cased on the way (nvConfigManager)
			"intended-firmware": map[string]any{"version": release},
			"evpn_esi_base_mac": "44:38:39:ff:69:00",
			"fabric-mac":        fabricMAC,
			"dns":               map[string]any{"ipv4": []string{NVCMZTPServer}},
			"ntp":               map[string]any{"ipv4": []string{NVCMZTPServer}},
			"ztp":               map[string]any{"ipv4": []string{NVCMZTPServer}},
			"Dhcp4":             map[string]any{"option-def": []any{nvcmOption239()}},
		}
	}
	var instances, peerings []any
	seenPeering := map[string]bool{}
	for _, s := range d.Switches {
		role := nvcmRoles[s.Role]
		if anchors[s.Hostname] {
			role = nvcmAnchorRole(s)
		}
		if role == "" || strings.Contains(s.NOS, "sonic") || !exported[s.Hostname] {
			continue
		}
		ifs := []any{}
		if mgmt[s.Hostname] != "" {
			mp, _ := netip.ParsePrefix(mgmt[s.Hostname])
			cover(mgmt[s.Hostname], "ipminet0")
			ifs = append(ifs, map[string]any{
				"name": "eth0", "type": "A_1000BASE_T", "mgmt_only": true, "enabled": true, "tags": []any{},
				"mac_address": strings.ToUpper(s.MgmtMAC), "role": map[string]any{"name": "Management"},
				"ip_addresses": []any{nvcmIP(mp)},
			})
		} else {
			// NVCM's loader asks every Cumulus device for its eth0's MAC; with no address on it, a
			// switch NVCM does not provision gets no reservation
			ifs = append(ifs, map[string]any{
				"name": "eth0", "type": "A_1000BASE_T", "mgmt_only": true, "enabled": true, "tags": []any{},
				"mac_address": strings.ToUpper(s.MgmtMAC), "role": map[string]any{"name": "Management"},
				"ip_addresses": []any{},
			})
		}
		if s.Loopback != "" {
			lp, err := netip.ParsePrefix(s.Loopback)
			if err == nil {
				cover(s.Loopback, "superpod-loopback")
				ifs = append(ifs, map[string]any{
					"name": "lo", "type": "VIRTUAL", "mgmt_only": false, "enabled": true, "tags": []any{},
					"role": map[string]any{"name": "Switch-Loopback"}, "ip_addresses": []any{nvcmIP(lp)},
				})
			}
		}
		for _, it := range s.Interfaces {
			if it.Kind != "fabric" || it.Address == "" {
				continue
			}
			p, err := netip.ParsePrefix(it.Address)
			if err != nil {
				continue
			}
			peer, port, cabled := strings.Cut(it.Peer, ":")
			if anchors[s.Hostname] && !exported[peer] {
				continue // the twin's switch's other links are not NVCM's
			}
			cover(it.Address, "superpod-spine-p2p")
			dir := "Uplink"
			if s.Role == "spine" || s.Role == "super-spine" || anchors[s.Hostname] {
				dir = "Downlink"
			}
			m := map[string]any{
				"name": it.Name, "type": "A_400GBASE_X_OSFP", "mgmt_only": false, "enabled": true, "tags": []any{},
				"role": map[string]any{"name": dir}, "ip_addresses": []any{nvcmIP(p)},
			}
			if cabled {
				// a cable to a device in the blueprint: one to a device not in it is no cable NVCM has
				if exported[peer] {
					m["connected_interface"] = map[string]any{"name": port, "device": map[string]any{"name": peer}}
					// a BGP session on the link (NVCM's peerings: its BGP endpoints, the neighbours it
					// renders), once for each pair of ends
					a, b := s.Hostname+":"+it.Name, peer+":"+port
					if a > b {
						a, b = b, a
					}
					if exported[peer] && !seenPeering[a+"|"+b] {
						seenPeering[a+"|"+b] = true
						peerings = append(peerings, map[string]any{"device": s.Hostname, "source_interface": it.Name,
							"peer_device": peer, "peer_source_interface": port})
					}
					// an OOB-Leaf's EVPN session, loopback to loopback, with the switch the twin
					// configures (its VRF OOB's routes: the BMC DHCP's network)
					if anchors[peer] && oobLo[s.Hostname] != "" && !seenPeering[s.Hostname+"|lo|"+peer] {
						seenPeering[s.Hostname+"|lo|"+peer] = true
						peerings = append(peerings, map[string]any{"device": s.Hostname, "source_interface": "lo",
							"peer_device": peer, "peer_source_interface": "lo"})
					}
				}
			}
			ifs = append(ifs, m)
		}
		var vrfs []any
		data := map[string]any{}
		if lo := oobLo[s.Hostname]; lo != "" {
			vrf := map[string]any{"name": NVCMOOBVRF, "rd": fmt.Sprintf("*:%d", NVCMOOBL3VNI), "import_targets": []any{}, "export_targets": []any{}}
			vrfs = append(vrfs, vrf)
			v := nvcmBMCVlan(s)
			svi := oobSVI[s.Hostname]
			cover(bmcNet.String(), "ipminet0")
			cover(lo, "superpod-loopback")
			lp := netip.MustParsePrefix(lo)
			vlan := map[string]any{"vid": NVCMBMCVLAN, "name": fmt.Sprintf("Vlan%d", NVCMBMCVLAN)}
			ifs = append(ifs,
				map[string]any{"name": fmt.Sprintf("vlan%d", NVCMBMCVLAN), "type": "VIRTUAL", "mgmt_only": false, "enabled": true, "tags": []any{},
					"role": map[string]any{"name": fmt.Sprintf("VLAN-%d", NVCMBMCVLAN)}, "vrf": vrf, "untagged_vlan": vlan, "ip_addresses": []any{nvcmIP(svi)}},
				map[string]any{"name": "lo-" + strings.ToLower(NVCMOOBVRF), "type": "VIRTUAL", "mgmt_only": false, "enabled": true, "tags": []any{},
					"vrf": vrf, "ip_addresses": []any{nvcmIP(lp)}})
			for _, port := range v.Ports {
				ifs = append(ifs, map[string]any{"name": port, "type": "A_1000BASE_T", "mgmt_only": false, "enabled": true, "tags": []any{},
					"role": map[string]any{"name": "Downlink"}, "untagged_vlan": vlan, "ip_addresses": []any{}})
			}
			data["vxlans"] = []any{
				map[string]any{"name": fmt.Sprintf("Vlan%d", NVCMBMCVLAN), "vnid": NVCMBMCVNI, "vni_type": "l2", "l3_vlan_id": nil,
					"overlay": map[string]any{"name": fmt.Sprintf("Vlan%d", NVCMBMCVLAN)}, "vlan": vlan, "vrf": nil, "import_targets": []any{}, "export_targets": []any{}},
				map[string]any{"name": NVCMOOBVRF + "-L3", "vnid": NVCMOOBL3VNI, "vni_type": "l3", "l3_vlan_id": NVCMOOBL3VLAN,
					"overlay": map[string]any{"name": NVCMOOBVRF + "-L3"}, "vlan": nil, "vrf": vrf, "import_targets": []any{}, "export_targets": []any{}},
			}
		}
		if vrfs == nil {
			vrfs = []any{}
		}
		status := "Provisioning"
		if anchors[s.Hostname] {
			status = "Active" // the twin's: configured, not NVCM's to provision
		}
		dev := map[string]any{
			"name": s.Hostname, "tags": []any{},
			"role":        map[string]any{"name": role},
			"status":      map[string]any{"name": status},
			"device_type": map[string]any{"manufacturer": map[string]any{"name": "NVIDIA"}, "model": "SN5600"},
			"platform":    map[string]any{"name": "Cumulus Linux"},
			"serial":      s.Serial,
			"vrfs":        vrfs,
			// router_id comes from lo's address; a switch with no loopback is reached at eth0
			"primary_ip4_interface": map[bool]string{true: "lo", false: "eth0"}[s.Loopback != ""],
			"config_context":        contexts("00:00:5E:00:01:01"),
			"interfaces":            ifs,
		}
		data["device"] = dev
		b, err := json.MarshalIndent(map[string]any{"data": data}, "", "  ")
		if err != nil {
			return nil, err
		}
		files[filepath.Join(bp, "devices", s.Hostname+".json")] = append(b, '\n')
		if s.ASN != 0 {
			instances = append(instances, map[string]any{"device": s.Hostname, "asn": s.ASN})
		}
	}
	if len(instances) == 0 && len(files) == 0 {
		return nil, nil
	}
	locations := map[string]any{
		"locations": []any{
			map[string]any{"name": "dc-twin", "location_type": "Provider"},
			map[string]any{"name": d.Estate, "location_type": "Region", "parent": "dc-twin"},
			map[string]any{"name": site, "location_type": "Site", "parent": d.Estate},
			map[string]any{"name": module, "location_type": "Module", "parent": site, "ref": "location"},
		},
		"config_contexts": []any{
			map[string]any{"name": site + " site-asn", "description": "the site's BGP AS", "weight": 1000, "is_active": true,
				"locations": []string{site}, "data": map[string]any{"site_asn": "4230000001"}},
			map[string]any{"name": site + " Management Services", "description": "the site's management services (the ZTP server's address filled in by the site's tooling)",
				"weight": 1000, "is_active": true, "locations": []string{site}, "data": map[string]any{
					"dns": map[string]any{"ipv4": []string{NVCMZTPServer}}, "ntp": map[string]any{"ipv4": []string{NVCMZTPServer}},
					"ztp": map[string]any{"ipv4": []string{NVCMZTPServer}}, "provisioning_servers": map[string]any{"ipv4": []string{NVCMZTPServer}},
					"Dhcp4": map[string]any{"option-def": []any{nvcmOption239()}},
				}},
			map[string]any{"name": site + " ZTP options", "description": "Cumulus's option 239 on every switch's management port, by its reservation",
				"weight": 1000, "is_active": true, "locations": []string{site}, "data": map[string]any{
					"dhcp": map[string]any{"options": map[string]any{"interface_names": map[string]any{"eth0": map[string]any{
						"subnet_config": map[string]any{"reservations-global": true, "reservations-in-subnet": false},
						"reservation_options": map[string]any{
							"cumulus-provision-url": "http://{{ztp_server}}/v1/device/{{device_id}}/boot-script",
							"boot-file-name":        "http://{{ztp_server}}/v1/device/{{device_id}}/onie",
						},
					}}}},
				}},
		},
	}
	var aggs []any
	keys := make([]string, 0, len(aggregates))
	for k := range aggregates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		aggs = append(aggs, map[string]any{"prefix": k, "role": aggregates[k], "tags": []string{"role-aggregate"}})
	}
	for name, v := range map[string]any{
		"locations.yaml":             locations,
		"prefixes.yaml":              map[string]any{"aggregate_prefixes": aggs},
		"bgp_routing_instances.yaml": map[string]any{"bgp_routing_instances": instances, "bgp_peerings": peerings},
	} {
		b, err := yaml.Marshal(v)
		if err != nil {
			return nil, err
		}
		files[filepath.Join(bp, name)] = append([]byte("---\n"), b...)
	}
	if len(helpers) > 0 {
		b, err := json.MarshalIndent(map[string]any{"vlans": helpers}, "", "  ")
		if err != nil {
			return nil, err
		}
		files[filepath.Join(bp, "helpers.json")] = append(b, '\n')
	}
	files["README.md"] = []byte(fmt.Sprintf(`# The design as NVIDIA Config Manager's blueprint

A Design Builder blueprint for NVIDIA Config Manager 1.4.1's MockTopologyDesign job
(development/mock_topology), blueprint %q: copy context/%s into mock_topology/context/ and run the
job with {"blueprint": %q, "deployment_name": "<any>"}.

Before loading, replace %s in every file with the address NVCM's ZTP and DHCP services answer at,
and %s with the site's BMC DHCP server (the OOB-Leafs' VLAN %d relay). Each switch's eth0 address
(its reservation) is in its devices/<hostname>.json. helpers.json relates VLAN %d to its relay's
servers (NVCM's vlan_to_helper_address relationship), which the blueprint format cannot say: load
it through Nautobot's API after the design job.
`, NVCMBlueprint, NVCMBlueprint, NVCMBlueprint, NVCMZTPServer, NVCMBMCDHCP, NVCMBMCVLAN, NVCMBMCVLAN))
	return files, nil
}

// nvcmAnchorRole is NVCM's role for a switch the twin configures that NVCM's switches are cabled
// to: an OOB aggregation switch is its OOB-Spine; "" for any other.
func nvcmAnchorRole(s Switch) string {
	if s.Role == "oob" && strings.Contains(s.Hostname, "oob-agg") {
		return "OOB-Spine"
	}
	return ""
}

// nvcmBMCVlan is an OOB switch's access segment: the design's VLAN with access ports and a
// gateway.
func nvcmBMCVlan(s Switch) Vlan {
	for _, v := range s.Vlans {
		if len(v.Ports) > 0 && v.Gateway != "" {
			return v
		}
	}
	return Vlan{}
}

// nvcmCover is the smallest prefix holding every prefix given.
func nvcmCover(ps []netip.Prefix) netip.Prefix {
	if len(ps) == 0 {
		return netip.Prefix{}
	}
	out := ps[0]
	for _, p := range ps[1:] {
		for !out.Contains(p.Addr()) || out.Bits() > p.Bits() {
			out, _ = out.Addr().Prefix(out.Bits() - 1)
		}
	}
	return out.Masked()
}

func nvcmOption239() map[string]any {
	return map[string]any{"code": 239, "name": "cumulus-provision-url", "type": "string", "array": false, "space": "dhcp4", "encapsulate": "", "record-types": ""}
}

func nvcmIP(p netip.Prefix) map[string]any {
	return map[string]any{"address": p.String(), "host": p.Addr().String(), "mask_length": p.Bits(), "ip_version": 4}
}

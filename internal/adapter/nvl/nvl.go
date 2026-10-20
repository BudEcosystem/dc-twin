// Package nvl derives what NVIDIA's rack-scale management interfaces report about an NVL rack
// and its NVLink scale-up domain, once, so that NMX-C (internal/adapter/nmxc) and the Rack
// Management Service (internal/adapter/rms) cannot disagree: the domain's UUID, a rack's
// chassis serial number, a tray's placement, and the fabric GUIDs of GPUs and NVLink switch
// ASICs.
//
// Every value is identity/v1 material (docs/spec/13-schemas.md — Identity derivation) or a
// model field, so it is stable across up and down and differs between estates with different
// seeds. On real hardware the chassis of an NVL rack is the rack itself: its serial is the one
// NVML's platform info reports for each GPU and NMX-C reports for each tray, and the rack unit a
// tray occupies is its slot.
package nvl

import (
	"sort"
	"strconv"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/identity"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// GUIDPrefix is NVIDIA's OUI, the top 24 bits of every fabric GUID the twin derives.
const GUIDPrefix = "0xb83fd2"

// DomainUUID is a scale-up domain's UUID. It is the cluster UUID GPU Feature Discovery's
// nvidia.com/gpu.clique label carries (kwok.DomainID), so NMX-C's domain_uuid and a node's
// clique name the same domain.
func DomainUUID(seed int64, dom *model.Domain) string {
	return identity.New(seed).UUID(dom.Path, "domain:"+dom.Kind+":"+dom.Technology, 0)
}

// ChassisSerial is the serial number of an NVL rack's chassis: thirteen digits, so that RMS
// can report it as the integer its chassis_sn field holds.
func ChassisSerial(seed int64, r *model.Rack) string {
	return identity.New(seed).Serial(r.Path, "chassis:"+r.FormatName, "1{digits:12}", 0)
}

// Slot is the rack unit a tray occupies (EIA), its physical slot in the rack.
func Slot(d *model.Device) uint32 {
	if d.U < 0 {
		return 0
	}
	return uint32(d.U)
}

// TrayIndex is a tray's index among the trays of its kind in the rack, from 0: the value a GB
// tray's Redfish chassis reports as Oem.Nvidia.ComputeTrayIndex.
func TrayIndex(d *model.Device) uint32 {
	if d.SlotIndex < 0 {
		return 0
	}
	return uint32(d.SlotIndex)
}

// FabricGUID is the NVLink fabric GUID of a GPU or NVLink switch ASIC: NVIDIA's OUI and five
// derived bytes, as NMX-C reports gpu_uid and switch_uid.
func FabricGUID(seed int64, c *model.Component) uint64 {
	s := identity.New(seed).GUID(c.Device.Path, c.Device.Identity.ProfileMajor, "nvlink-fabric:"+c.Name, GUIDPrefix, c.Index)
	v, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	if err != nil {
		return 0
	}
	return v
}

// SystemGUID is the fabric GUID of a tray as a whole (NMX-C's system_uid).
func SystemGUID(seed int64, d *model.Device) uint64 {
	s := identity.New(seed).GUID(d.Path, d.Identity.ProfileMajor, "nvlink-system", GUIDPrefix, 0)
	v, _ := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
	return v
}

// ScaleUpDomains are the model's NVLink scale-up domains whose racks hold NVLink switch trays:
// the domains an NVLink controller manages. A per-server domain (an HGX baseboard's NVSwitch) has
// no switch tray and no NMX-C.
func ScaleUpDomains(m *model.Model) []*model.Domain {
	var out []*model.Domain
	for _, dom := range m.Domains {
		if dom.Kind != "scale-up" || dom.Technology != "nvlink" {
			continue
		}
		if len(SwitchTrays(dom)) == 0 {
			continue
		}
		out = append(out, dom)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// SwitchTrays are the NVLink switch trays of a domain's racks, by rack then name.
func SwitchTrays(dom *model.Domain) []*model.Device {
	var out []*model.Device
	for _, r := range dom.Racks {
		var trays []*model.Device
		for _, d := range r.Devices {
			if d.BaseClass == "switch-tray" {
				trays = append(trays, d)
			}
		}
		sort.Slice(trays, func(i, j int) bool { return trays[i].Name < trays[j].Name })
		out = append(out, trays...)
	}
	return out
}

// RackScale reports whether a rack is a rack-scale system (an NVL rack): the racks a Rack
// Management Service manages.
func RackScale(r *model.Rack) bool {
	return r != nil && r.Format != nil && r.Format.Spec.Archetype == "rack-scale"
}

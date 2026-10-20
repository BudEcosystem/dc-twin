package redfish

import (
	"fmt"
	"hash/fnv"
	"strings"
	"sync"

	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// A `vm` host's boot options are its firmware's (D19, D48). Its UEFI firmware enumerates the
// load options itself, one PXE and one HTTP boot option per NIC and IP version and the shell,
// and the vm adapter's observer commits them as `nvram.bootOptions` whenever the firmware writes
// its variable store. The BMC serves those as the system's BootOptions (in place of the
// capture's, which name the captured machine's NICs and disks), with the firmware's display name,
// its UEFI device path in text form and the alias it boots (UefiHttp, Pxe, UefiShell), and an
// ETag a client sending If-Match can use; its BootOrder is the
// firmware's, or a reordering of the same options a client has set (the model's boot order,
// which the vm adapter writes into the store before the machine next starts). A lifecycle
// manager that looks for "UEFI HTTPv4 (MAC:<its boot NIC>)" finds the option the guest's
// firmware really has.
type firmwareBoot struct {
	order   []string
	entries []firmwareOption
}

type firmwareOption struct {
	id, description, devicePath, alias string
	active                             bool
}

var firmwareBoots sync.Map // *bmc -> *firmwareBoot

// observeFirmware keeps the last boot options a vm host's firmware was observed holding.
func (a *Adapter) observeFirmware(ev schema.Event) {
	if ev.State == nil || ev.State.Field != "nvram.bootOptions" || ev.Source.Class != "observer" {
		return
	}
	b := a.bmcs[ev.Path]
	if b == nil {
		return
	}
	fb := &firmwareBoot{}
	if o, ok := ev.Data["bootOrder"].([]any); ok {
		for _, id := range o {
			fb.order = append(fb.order, fmt.Sprint(id))
		}
	}
	if es, ok := ev.Data["entries"].([]any); ok {
		for _, e := range es {
			m, _ := e.(map[string]any)
			if m == nil {
				continue
			}
			active, _ := m["active"].(bool)
			path, _ := m["devicePath"].(string)
			alias, _ := m["alias"].(string)
			fb.entries = append(fb.entries, firmwareOption{id: fmt.Sprint(m["id"]), description: fmt.Sprint(m["description"]),
				devicePath: path, alias: alias, active: active})
		}
	}
	if len(fb.entries) > 0 {
		firmwareBoots.Store(b, fb)
	}
}

func (b *bmc) firmwareBoot() *firmwareBoot {
	if v, ok := firmwareBoots.Load(b); ok {
		return v.(*firmwareBoot)
	}
	return nil
}

// bootOrder is the BootOrder the system shows: for a vm host whose firmware has been observed,
// the model's order when it is a reordering of the firmware's options (set by a client), else
// the firmware's own.
func (b *bmc) bootOrder(modelOrder []string) []string {
	fb := b.firmwareBoot()
	if fb == nil {
		return modelOrder
	}
	have := map[string]bool{}
	for _, id := range fb.order {
		have[id] = true
	}
	if len(modelOrder) == len(fb.order) {
		ok := true
		for _, id := range modelOrder {
			if !have[id] {
				ok = false
				break
			}
		}
		if ok {
			return modelOrder
		}
	}
	return fb.order
}

// firmwareBootOrder sets the BootOrder of a vm host's system, and of its settings object (where a
// client such as NICo's DGX implementation reads the order it reorders), to bootOrder.
func (r *renderer) firmwareBootOrder(path string, doc map[string]any) {
	b := r.b
	sys := b.dialect.Spec.System
	if sys == "" || b.firmwareBoot() == nil {
		return
	}
	boot, ok := doc["Boot"].(map[string]any)
	if !ok {
		return // (checked first: isSettingsObject renders the system and its Bios)
	}
	if path != sys && !(strings.HasPrefix(path, sys+"/") && strings.Count(path, "/") == strings.Count(sys, "/")+1 && b.isSettingsObject(path)) {
		return
	}
	bo := b.bootOrder(r.snap.boot.Order)
	order := make([]any, len(bo))
	for i, o := range bo {
		order[i] = o
	}
	boot["BootOrder"] = order
}

// optionID is the resource id the BMC gives a boot option whose firmware variable is ref
// ("Boot0004"): the variable's name on most BMCs, and its bare number ("0004") where the captured
// BMC numbers its options so, as the DGX H100's does (keeping "Boot0004" as the option's Name and
// BootOptionReference). Clients build one from the other: NICo's DGX implementation fetches
// BootOptions/<BootOrder entry less "Boot"> and puts "Boot"+Id back into the order it sets, so an
// option served at the wrong id is one it cannot find, and an order it cannot write.
func (b *bmc) optionID(ref string) string {
	if b.bareBootOptionIDs() {
		return strings.TrimPrefix(ref, "Boot")
	}
	return ref
}

// bareBootOptionIDs reports whether the captured system's boot options have bare ids.
func (b *bmc) bareBootOptionIDs() bool {
	coll, ok := b.tree.get(b.dialect.Spec.System + "/BootOptions")
	if !ok {
		return false
	}
	ms, _ := coll["Members"].([]any)
	for _, m := range ms {
		ref, _ := m.(map[string]any)
		id, _ := ref["@odata.id"].(string)
		return id != "" && !strings.HasPrefix(id[strings.LastIndex(id, "/")+1:], "Boot")
	}
	return false
}

// firmwareNIC is a network interface the firmware's boot options show: its MAC and the device
// path of the interface itself (the boot options' paths up to its MAC node).
type firmwareNIC struct {
	mac, devicePath string
}

// nics are the NICs the firmware enumerated, in its boot options' order: one per MAC.
func (fb *firmwareBoot) nics() []firmwareNIC {
	var out []firmwareNIC
	seen := map[string]bool{}
	for _, e := range fb.entries {
		i := strings.Index(e.devicePath, "/MAC(")
		if i < 0 {
			continue
		}
		end := strings.Index(e.devicePath[i:], ")")
		if end < 0 {
			continue
		}
		node := e.devicePath[i+len("/MAC(") : i+end] // "3C6D6626CCB8,0x1"
		hex, _, _ := strings.Cut(node, ",")
		if len(hex) != 12 || seen[hex] {
			continue
		}
		seen[hex] = true
		mac := make([]string, 6)
		for j := range mac {
			mac[j] = strings.ToUpper(hex[2*j : 2*j+2])
		}
		out = append(out, firmwareNIC{mac: strings.Join(mac, ":"), devicePath: e.devicePath[:i+end+1]})
	}
	return out
}

// firmwareEthernetDoc renders a vm host's system EthernetInterfaces from the NICs its firmware
// enumerated, in place of the capture's, which are the captured machine's adapters: a lifecycle
// manager takes the host's NICs from here (NICo's site explorer keys a zero-DPU host's interfaces,
// and the boot NIC it waits for, on these MACs), and a BMC reports the NICs its host has. The
// capture's member ids are kept, in order, for as many NICs as it has (its vendor's naming).
func (r *renderer) firmwareEthernetDoc(path string) (map[string]any, bool) {
	b := r.b
	sys := b.dialect.Spec.System
	base := sys + "/EthernetInterfaces"
	if sys == "" || !strings.HasPrefix(path, base) {
		return nil, false
	}
	fb := b.firmwareBoot()
	if fb == nil {
		return nil, false
	}
	nics := fb.nics()
	if len(nics) == 0 {
		return nil, false
	}
	var ids []string
	typ := "#EthernetInterface.v1_6_0.EthernetInterface"
	if coll, ok := b.tree.get(base); ok {
		ms, _ := coll["Members"].([]any)
		for _, m := range ms {
			ref, _ := m.(map[string]any)
			if id, _ := ref["@odata.id"].(string); id != "" {
				ids = append(ids, id[strings.LastIndex(id, "/")+1:])
			}
		}
		if len(ids) > 0 {
			if doc, ok := b.tree.get(base + "/" + ids[0]); ok {
				if t, _ := doc["@odata.type"].(string); t != "" {
					typ = t
				}
			}
		}
	}
	idOf := func(i int) string {
		if i < len(ids) {
			return ids[i]
		}
		return fmt.Sprintf("EthernetInterface%d", i)
	}
	if path == base {
		members := []any{}
		for i := range nics {
			members = append(members, map[string]any{"@odata.id": base + "/" + idOf(i)})
		}
		return map[string]any{"@odata.id": base, "@odata.type": "#EthernetInterfaceCollection.EthernetInterfaceCollection",
			"Name": "Ethernet Network Interface Collection", "Members": members, "Members@odata.count": len(members)}, true
	}
	for i, n := range nics {
		if path != base+"/"+idOf(i) {
			continue
		}
		return map[string]any{"@odata.id": path, "@odata.type": typ, "Id": idOf(i), "Name": "Ethernet Interface",
			"Description": "Ethernet Interface " + n.mac, "MACAddress": n.mac, "PermanentMACAddress": n.mac,
			"InterfaceEnabled": true, "LinkStatus": "LinkUp", "UefiDevicePath": n.devicePath,
			"Status": map[string]any{"State": "Enabled", "Health": "OK"}}, true
	}
	return nil, false
}

// firmwareBootDoc renders the BootOptions collection and its members from the firmware, and the
// system's EthernetInterfaces from the NICs they name.
func (r *renderer) firmwareBootDoc(path string) (map[string]any, bool) {
	b := r.b
	sys := b.dialect.Spec.System
	if doc, ok := r.firmwareEthernetDoc(path); ok {
		return doc, true
	}
	if sys == "" || !strings.HasPrefix(path, sys+"/BootOptions") {
		return nil, false
	}
	fb := b.firmwareBoot()
	if fb == nil {
		return nil, false
	}
	base := sys + "/BootOptions"
	if path == base {
		members := []any{}
		for _, e := range fb.entries {
			members = append(members, map[string]any{"@odata.id": base + "/" + b.optionID(e.id)})
		}
		return map[string]any{"@odata.id": base, "@odata.type": "#BootOptionCollection.BootOptionCollection",
			"Name": "Boot Options Collection", "Members": members, "Members@odata.count": len(members)}, true
	}
	id := strings.TrimPrefix(path, base+"/")
	bare := b.bareBootOptionIDs()
	for _, e := range fb.entries {
		if b.optionID(e.id) == id {
			h := fnv.New32a()
			h.Write([]byte(e.id + "|" + e.description + "|" + e.devicePath))
			name := e.description
			if bare {
				name = e.id // the DGX's BMC names an option by its variable
			}
			doc := map[string]any{"@odata.id": path, "@odata.type": "#BootOption.v1_0_3.BootOption", "@odata.etag": fmt.Sprintf(`W/"%d"`, h.Sum32()),
				"Id": id, "Name": name, "BootOptionReference": e.id, "DisplayName": e.description, "Description": e.description,
				"BootOptionEnabled": e.active}
			if e.devicePath != "" {
				doc["UefiDevicePath"] = e.devicePath
			}
			if e.alias != "" {
				doc["Alias"] = e.alias
			}
			return doc, true
		}
	}
	return nil, false
}

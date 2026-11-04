package nicoagent

import (
	"errors"
	"fmt"
	"strings"
)

// identity is what the DPU's software knows of the DPU, read from what its BMC serves: the
// software and the BMC describe one BlueField, and NICo checks them against each other (the
// machine id from the serial, the host pairing from the base MAC, the OOB interface by MAC).
type identity struct {
	serial, manufacturer, product, uuid string
	oobMAC                              string // Systems/<id>/EthernetInterfaces/oob0
	hostMAC                             string // Oem.Nvidia.BaseMAC: the host PF's MAC
	partNumber                          string // the DPU card's
	nic, uefi                           string // DPU_NIC and DPU_UEFI firmware versions
}

// resources is the redfish adapter as the stand-in reads it: a resource as the BMC serves it
// now, without logging a request (the stack's view is unchanged).
type resources interface {
	Get(devicePath, resourcePath string) (map[string]any, bool)
}

// system is the machine's ComputerSystem as its BMC serves it, and a reader for its resources.
func (m *machine) system() (string, func(string) map[string]any, error) {
	if m.a.rt.Peer == nil {
		return "", nil, errors.New("no redfish adapter to read the machine's BMC from")
	}
	rf, ok := m.a.rt.Peer("redfish").(resources)
	if !ok {
		return "", nil, errors.New("the redfish adapter is not enabled: the stand-in reads the machine's identity from its BMC")
	}
	get := func(p string) map[string]any {
		doc, _ := rf.Get(m.path, p)
		return doc
	}
	sys := ""
	if coll := get("/redfish/v1/Systems"); coll != nil {
		if ms, _ := coll["Members"].([]any); len(ms) > 0 {
			if mm, ok := ms[0].(map[string]any); ok {
				sys, _ = mm["@odata.id"].(string)
			}
		}
	}
	if sys == "" || get(sys) == nil {
		return "", nil, fmt.Errorf("%s's BMC serves no ComputerSystem", m.path)
	}
	return sys, get, nil
}

func (b *dpu) readIdentity() (identity, error) {
	var id identity
	sys, get, err := b.system()
	if err != nil {
		return id, err
	}
	doc := get(sys)
	id.serial, _ = doc["SerialNumber"].(string)
	id.manufacturer, _ = doc["Manufacturer"].(string)
	id.product, _ = doc["Model"].(string)
	id.uuid, _ = doc["UUID"].(string)
	id.serial = strings.TrimSpace(id.serial)
	if id.serial == "" {
		return id, fmt.Errorf("%s's BMC serves no system serial", b.path)
	}
	if oem := get(sys + "/Oem/Nvidia"); oem != nil {
		if s, _ := oem["BaseMAC"].(string); s != "" {
			id.hostMAC = colonMAC(s)
		}
	}
	if eth := get(sys + "/EthernetInterfaces/oob0"); eth != nil {
		id.oobMAC, _ = eth["MACAddress"].(string)
	}
	if card := get("/redfish/v1/Chassis/Card1"); card != nil {
		id.partNumber, _ = card["PartNumber"].(string)
	}
	if fw := get("/redfish/v1/UpdateService/FirmwareInventory/DPU_NIC"); fw != nil {
		id.nic, _ = fw["Version"].(string)
	}
	if fw := get("/redfish/v1/UpdateService/FirmwareInventory/DPU_UEFI"); fw != nil {
		id.uefi, _ = fw["Version"].(string)
	}
	if id.oobMAC == "" {
		return id, fmt.Errorf("%s's BMC serves no oob0 interface MAC", b.path)
	}
	if id.hostMAC == "" {
		return id, fmt.Errorf("%s's BMC serves no BaseMAC (the host PF's MAC NICo pairs the DPU with its host by)", b.path)
	}
	return id, nil
}

// colonMAC writes a MAC given without separators (BaseMAC's "5c257308f352") with colons.
func colonMAC(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.ContainsAny(s, ":-") || len(s) != 12 {
		return strings.ReplaceAll(s, "-", ":")
	}
	var b strings.Builder
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(s[i : i+2])
	}
	return b.String()
}

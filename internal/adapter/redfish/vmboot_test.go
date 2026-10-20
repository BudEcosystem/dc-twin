package redfish

import (
	"reflect"
	"testing"
)

// A vm host's NICs are the ones its firmware's boot options name, one per MAC, in their order,
// each with the device path of the interface itself (D48).
func TestFirmwareNICsAreTheBootOptionsMACs(t *testing.T) {
	fb := &firmwareBoot{entries: []firmwareOption{
		{id: "Boot0000", description: "BootManagerMenuApp", devicePath: "Fv(7CB8BDC9-F8EB-4F34-AAEA-3EE4AF6516A1)/FvFile(EEC25BDC-67F2-4D95-B1D5-F81B2039D11D)"},
		{id: "Boot0002", description: "UEFI PXEv4 (MAC:3C6D6626CCB8)", devicePath: "PciRoot(0x0)/Pci(0x1,0x0)/MAC(3C6D6626CCB8,0x1)/IPv4(0.0.0.0,0x0,DHCP,0.0.0.0,0.0.0.0,0.0.0.0)"},
		{id: "Boot0004", description: "UEFI HTTPv4 (MAC:3C6D6626CCB8)", devicePath: "PciRoot(0x0)/Pci(0x1,0x0)/MAC(3C6D6626CCB8,0x1)/IPv4(0.0.0.0,0x0,DHCP,0.0.0.0,0.0.0.0,0.0.0.0)/Uri()"},
		{id: "Boot0006", description: "UEFI PXEv4 (MAC:3C6D669FEC96)", devicePath: "PciRoot(0x0)/Pci(0x2,0x0)/MAC(3C6D669FEC96,0x1)/IPv4(0.0.0.0,0x0,DHCP,0.0.0.0,0.0.0.0,0.0.0.0)"},
		{id: "Boot000A", description: "EFI Internal Shell", devicePath: "Fv(7CB8BDC9-F8EB-4F34-AAEA-3EE4AF6516A1)/FvFile(7C04A583-9E3E-4F1C-AD65-E05268D0B4D1)"},
	}}
	want := []firmwareNIC{
		{mac: "3C:6D:66:26:CC:B8", devicePath: "PciRoot(0x0)/Pci(0x1,0x0)/MAC(3C6D6626CCB8,0x1)"},
		{mac: "3C:6D:66:9F:EC:96", devicePath: "PciRoot(0x0)/Pci(0x2,0x0)/MAC(3C6D669FEC96,0x1)"},
	}
	if got := fb.nics(); !reflect.DeepEqual(got, want) {
		t.Fatalf("nics: %+v, want %+v", got, want)
	}
}

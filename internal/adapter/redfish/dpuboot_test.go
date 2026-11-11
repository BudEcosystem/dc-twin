package redfish

import (
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// A BMC that names boot options by adapter (the Supermicro dialect's dpuBootName) names a vm
// host's firmware options for its DPU's host PF as it does (NICo's libredfish boots a host from
// its DPU by that name, D71), and leaves every other option as the firmware describes it.
func TestDPUBootOptionNames(t *testing.T) {
	dpu := &model.Device{Path: "h/bf3-0", PortBy: map[string]*model.Port{"pf0hpf": {Name: "pf0hpf", MAC: "b8:3f:d2:e0:09:65"}}}
	d := &schema.RedfishDialect{}
	d.Spec.DPUBootName = "UEFI {proto} {ip} Mellanox Network Adapter - {mac}(MAC:{compact})"
	b := &bmc{dialect: d, dpus: []*model.Device{dpu}}
	for in, want := range map[string]string{
		"UEFI HTTPv4 (MAC:B83FD2E00965)": "UEFI HTTP IPv4 Mellanox Network Adapter - B8:3F:D2:E0:09:65(MAC:B83FD2E00965)",
		"UEFI PXEv6 (MAC:B83FD2E00965)":  "UEFI PXE IPv6 Mellanox Network Adapter - B8:3F:D2:E0:09:65(MAC:B83FD2E00965)",
		"UEFI HTTPv4 (MAC:7CC255000001)": "UEFI HTTPv4 (MAC:7CC255000001)",
		"UEFI QEMU NVMe Ctrl X 1":        "UEFI QEMU NVMe Ctrl X 1",
	} {
		if got := b.dpuBootName(firmwareOption{description: in}); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	b.dialect = &schema.RedfishDialect{}
	if got := b.dpuBootName(firmwareOption{description: "UEFI HTTPv4 (MAC:B83FD2E00965)"}); got != "UEFI HTTPv4 (MAC:B83FD2E00965)" {
		t.Errorf("a dialect with no dpuBootName renamed an option: %q", got)
	}
}

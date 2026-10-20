package redfish_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stmcginnis/gofish"
)

// nicoHwType is a Go transcription of NICo's site-explorer classification, `hw_type()` in
// crates/bmc-explorer/src/lib.rs with `is_gb300`, `is_lenovo`, `is_liteon_powershelf` and
// `is_delta_powershelf` from crates/bmc-explorer/src/chassis.rs (dsx-ai-factory/infra-controller,
// read 2026-10-18). bmc-explorer itself needs NICo's workspace to build; this keeps its rules, so
// the twin's identity strings are checked against what NICo matches on, not against our reading
// of it. It reads only what NICo reads: the service root, the system and the chassis collection.
func nicoHwType(t *testing.T, c *gofish.APIClient) string {
	t.Helper()
	svc := c.Service
	var oemID string
	var oem map[string]json.RawMessage
	if json.Unmarshal(svc.Oem, &oem) == nil {
		for k := range oem {
			oemID = k
			break
		}
	}
	chassis, _ := svc.Chassis()
	isGB300, isLenovo, isLiteon, isDelta := false, false, false, false
	for _, ch := range chassis {
		if ch.Manufacturer == "NVIDIA" && ch.Model == "NVIDIA GB300" {
			isGB300 = true
		}
		if ch.Manufacturer == "Lenovo" {
			isLenovo = true
		}
		if ch.ID == "powershelf" || (ch.ID == "chassis" && strings.Contains(strings.ToLower(ch.Manufacturer), "lite-on")) {
			isLiteon = true
		}
		if ch.ID == "chassis" && strings.Contains(strings.ToLower(ch.Manufacturer), "delta") {
			isDelta = true
		}
	}
	systemID := ""
	if systems, err := svc.Systems(); err == nil && len(systems) > 0 {
		systemID = systems[0].ID
		for _, s := range systems {
			if s.ID == "DGX" || s.ID == "Bluefield" || s.ID == "BlueField_0" {
				systemID = s.ID
			}
		}
	}
	vendor, product := svc.Vendor, svc.Product
	if isGB300 {
		switch {
		case isLenovo:
			return "LenovoGb300"
		case vendor == "NVIDIA" && product == "GB BMC":
			return "DgxGb300"
		case vendor == "Supermicro":
			return "SupermicroGb300"
		}
	}
	if vendor == "" && oemID == "Supermicro" {
		vendor = "Supermicro"
	}
	switch {
	case vendor == "AMI" && systemID == "DGX":
		return "Viking"
	case vendor == "AMI":
		return "Ami"
	case vendor == "Dell":
		return "Dell"
	case vendor == "Lenovo" && oemID == "Ami":
		return "LenovoAmi"
	case vendor == "Lenovo":
		return "Lenovo"
	case vendor == "Supermicro":
		return "Supermicro"
	case vendor == "HPE":
		return "Hpe"
	case vendor == "Nvidia" && (systemID == "Bluefield" || systemID == "BlueField_0"):
		return "Bluefield"
	case vendor == "NVIDIA" && product == "VR NVL72":
		return "VeraRubin"
	case (vendor == "WIWYNN" || vendor == "NVIDIA") && (product == "GB200 NVL" || product == "GB BMC"):
		return "Gb200"
	case vendor == "NVIDIA" && product == "P3809":
		return "NvSwitch"
	case vendor == "Contoso" || vendor == "Sushy" || vendor == "RedVirt":
		return "Sushy"
	}
	switch {
	case isLiteon:
		return "LiteonPowerShelf"
	case isDelta:
		return "DeltaPowerShelf"
	}
	return "unknown"
}

// TestNICoClassifiesEveryEmulatedBMC is the M0 exit's identification check: each emulated BMC is
// classified as the hardware type its profile intends, never as Sushy and, for the NVIDIA and
// OEM dialects, never as unknown.
func TestNICoClassifiesEveryEmulatedBMC(t *testing.T) {
	tw, a := up(t)
	want := map[string]string{
		"site-1/hall-a/a01/compute-tray-01":       "DgxGb300",
		"site-1/hall-a/a02/compute-tray-01":       "Gb200",
		"site-1/hall-a/a01/switch-tray-01":        "NvSwitch",
		"site-1/hall-a/a01/compute-tray-01/bf3-0": "Bluefield",
		"site-1/hall-a/a01/power-shelf-01":        "LiteonPowerShelf",
		"site-1/hall-a/b01/server-01":             "Dell",
		"site-1/hall-a/b02/server-01":             "Supermicro",
		"site-1/hall-a/b03/server-01":             "Supermicro", // the AMD MI355X platform is a Supermicro chassis
		"site-1/hall-a/b04/server-01":             "Supermicro", // so is the Gaudi 3 platform
		"site-1/hall-a/b05/server-01":             "Lenovo",
		"site-1/hall-a/c01/server-02":             "Hpe",
		"site-1/hall-a/c02/server-01":             "unknown", // the generic DMTF tree is no NICo-supported type, by design
	}
	for path, typ := range want {
		got := nicoHwType(t, connect(t, a, dev(tw, path)))
		t.Logf("%-42s -> %s", path, got)
		if got != typ {
			t.Errorf("%s: NICo's rules classify it as %s, want %s", path, got, typ)
		}
	}
}

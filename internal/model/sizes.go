package model

import (
	"regexp"
	"strconv"
	"strings"
)

// What a host's component strings say about its size, read the same way by every projection (the
// Kubernetes Node, the generated Redfish tree) so they agree on one machine.

var (
	coresRe = regexp.MustCompile(`(?i)(\d+)\s*cores`)
	tbRe    = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*TB\b`)
	gbRe    = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*GB\b`)
)

// DefaultCPUCores is a CPU's core count when its model string does not say ("72 cores").
const DefaultCPUCores = 32

// CPUCores is a CPU component's core count: from its model string, else DefaultCPUCores.
func CPUCores(c *Component) int {
	if m := coresRe.FindStringSubmatch(c.Model); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return DefaultCPUCores
}

// HostArch is a host's CPU architecture in Kubernetes' spelling, "arm64" or "amd64": from its
// `vm` fidelity's guest architecture, else a Grace CPU, else amd64.
func HostArch(d *Device) string {
	if d.Profile != nil {
		if vm, ok := d.Profile.Spec.Fidelity["vm"]; ok {
			switch vm["arch"] {
			case "aarch64", "arm64":
				return "arm64"
			}
		}
	}
	for _, c := range d.Components {
		if c.Kind == "cpu" && strings.Contains(strings.ToLower(c.Model), "grace") {
			return "arm64"
		}
	}
	return "amd64"
}

// DriveBytes is a drive component's capacity from its model string ("NVMe 3.84 TB"), 0 when it
// does not say.
func DriveBytes(c *Component) float64 {
	if m := tbRe.FindStringSubmatch(c.Model); m != nil {
		t, _ := strconv.ParseFloat(m[1], 64)
		return t * 1e12
	}
	if m := gbRe.FindStringSubmatch(c.Model); m != nil {
		g, _ := strconv.ParseFloat(m[1], 64)
		return g * 1e9
	}
	return 0
}

// SerialSource is the component whose identity a component's serial derives from: itself, or the
// first of its group of SerialShare consecutive components (a superchip's GPUs share a board).
func SerialSource(c *Component) *Component {
	if c.SerialShare <= 1 || c.Device == nil {
		return c
	}
	first := c.Index / c.SerialShare * c.SerialShare
	for _, x := range c.Device.Components {
		if x.Group == c.Group && x.Index == first {
			return x
		}
	}
	return c
}

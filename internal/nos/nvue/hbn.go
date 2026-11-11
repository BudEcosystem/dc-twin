package nvue

import (
	"fmt"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// HBNBuild is what a BlueField's emulated HBN reports as its build (GET /system): the HBN in
// NICo's forge.bfb (doca_hbn:3.2.3-doca3.2.3), from 2.3 on, so NICo's agent names its interfaces
// p0_if, pf0hpf_if, ... (D71).
const HBNBuild = "HBN 3.2.3-doca3.2.3"

// HBNVFs is how many virtual functions' representors an emulated BlueField has (pf0vf0_if ...).
const HBNVFs = 16

// HBNPorts are a BlueField's HBN interfaces, as HBN 2.3 and later name them: the uplinks, the
// host's physical functions' representors, the virtual functions' and the DPU's own functions.
func HBNPorts() []string {
	ps := []string{"p0_if", "p1_if", "pf0hpf_if", "pf1hpf_if"}
	for i := 0; i < HBNVFs; i++ {
		ps = append(ps, fmt.Sprintf("pf0vf%d_if", i))
	}
	return append(ps, "pf0dpu1_if", "pf0dpu3_if")
}

// HBNSwitch is a BlueField's emulated HBN as a NOS NVUE configures: every HBN interface present,
// the BlueField's model ports (p0, p1, pf0hpf) by their HBN names.
func HBNSwitch(path, hostname string) nos.Switch {
	sw := nos.Switch{Path: path, Hostname: hostname, Native: true, Build: HBNBuild}
	for _, p := range HBNPorts() {
		name := p
		switch p {
		case "p0_if", "p1_if", "pf0hpf_if":
			name = strings.TrimSuffix(p, "_if")
		}
		sw.Panel = append(sw.Panel, nos.PanelPort{Name: name, Ifname: p})
	}
	return sw
}

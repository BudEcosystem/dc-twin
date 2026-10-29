package main

// The in-tree adapters; each registers itself with the twin runtime.
import (
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/bluefield" // BlueField BMCs beyond the generic service (D51)
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/bms"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/dhcp"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/ibsim" // InfiniBand fabrics under ibsim, for a stack's own subnet manager
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/ipmi"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/kwok"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/metrics"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/netsim/tap" // the TAP substrate (Linux; D20)
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/nicoagent"  // NICo's DPU software and scout, impersonated, for model devices (D55)
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/nmxc"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/nvue"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/redfish"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/rms"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/snmp"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/ufm"
	_ "github.com/BudEcosystem/dc-twin/internal/inhost" // the in-host agent of `vm` hosts (D50)
)

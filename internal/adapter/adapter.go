// Package adapter defines the interface every protocol adapter implements
// (docs/spec/02-architecture.md — Adapters): it projects committed model state onto one
// protocol, translates that protocol's requests into engine requests, and never changes state
// itself.
package adapter

import (
	"context"
	"log/slog"

	"github.com/BudEcosystem/dc-twin/internal/adapter/netsim"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Runtime is what an adapter is given.
type Runtime struct {
	Engine   *engine.Engine
	Reg      *pack.Registry
	Net      *netsim.Net
	StateDir string
	Assets   string // where obtained assets (captured mockups) live
	Log      *slog.Logger
	// Outputs are the installed stack's outputs (broker, DHCP server, accessor endpoints).
	Outputs map[string]string
	// AllowTCG accepts vm guests that would run without hardware acceleration (09).
	AllowTCG bool
}

// Adapter is one protocol engine for many devices.
type Adapter interface {
	Name() string
	Start(ctx context.Context, rt *Runtime) error
	Stop() error
	// Report is this adapter's contribution to the fidelity report: what it served, what was
	// stubbed or impersonated, and what it does not model.
	Report() Report
}

// Report is an adapter's fidelity-report contribution (07).
type Report struct {
	Adapter      string
	Served       int      // endpoints served
	Lines        []string // e.g. "addressing: stack DHCP through the twin relay (option 82)"
	IsNot        []string
	Impersonated []string // impersonators this adapter is
	Stubs        []string
	// SubstrateOwned is state the adapter's substrate owns that the run depended on (D19),
	// e.g. "power observed from QEMU for 1 host"; TCG lists guests running unaccelerated (09).
	SubstrateOwned []string
	TCG            []string
}

// Substrate is an adapter that runs devices at a fidelity other than `model` (a `vm`'s QEMU
// machine). Up asks it before refusing such a device.
type Substrate interface {
	Adapter
	// Accepts reports whether this adapter can run the device on this host, and why not.
	Accepts(est *schema.Estate, d *model.Device, allowTCG bool) error
}

// Console is an adapter that serves devices' consoles: a vm's serial port, the stand-in for
// Serial over LAN until the IPMI engine serves SOL.
type Console interface {
	// ConsoleLog returns the device's console transcript from byte offset from, and the
	// offset to continue from.
	ConsoleLog(path string, from int64) ([]byte, int64, error)
}

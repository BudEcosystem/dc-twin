// Package adapter defines the interface every protocol adapter implements
// (docs/spec/02-architecture.md — Adapters): it projects committed model state onto one
// protocol, translates that protocol's requests into engine requests, and never changes state
// itself.
package adapter

import (
	"context"
	"errors"
	"log/slog"
	"strings"

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
	// Claims names, for each device at `vm` fidelity, the one substrate adapter that runs it, as
	// up decided (ClaimSubstrate). A substrate adapter plans only the devices claimed for it
	// (Runs), so no device is planned by two. Nil when the Runtime was not made by up.
	Claims map[string]string
}

// Runs reports whether the substrate adapter name runs the device at path: whether up claimed the
// device for it. Without claims (a Runtime not made by up) every substrate runs what it plans.
func (rt *Runtime) Runs(name, path string) bool {
	if rt.Claims == nil {
		return true
	}
	return rt.Claims[path] == name
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

// Specialised is a substrate that runs one kind of device (the bluefield adapter: a BlueField's
// Arm), as against a general one that runs any machine (the vm adapter). Where both accept a
// device, the specialised one claims it.
type Specialised interface {
	Substrate
	Specialised()
}

// ClaimSubstrate picks the one substrate adapter that runs a device, among the enabled ones in
// the order --adapters names them: the first specialised substrate that accepts the device, else
// the first general one that does. The choice depends on the device and the adapters enabled,
// never on the order adapters start in. Refused by every one, the error gives each one's reason.
func ClaimSubstrate(est *schema.Estate, d *model.Device, subs []Substrate, allowTCG bool) (Substrate, error) {
	var why []string
	var general Substrate
	for _, s := range subs {
		if err := s.Accepts(est, d, allowTCG); err != nil {
			why = append(why, s.Name()+": "+err.Error())
			continue
		}
		if _, ok := s.(Specialised); ok {
			return s, nil
		}
		if general == nil {
			general = s
		}
	}
	if general != nil {
		return general, nil
	}
	if len(subs) == 0 {
		return nil, errors.New("the vm adapter is not enabled (--adapters)")
	}
	if len(why) == 1 {
		_, msg, _ := strings.Cut(why[0], ": ")
		return nil, errors.New(msg)
	}
	return nil, errors.New(strings.Join(why, "; "))
}

// Console is an adapter that serves devices' consoles: a vm's serial port, the stand-in for
// Serial over LAN until the IPMI engine serves SOL.
type Console interface {
	// ConsoleLog returns the device's console transcript from byte offset from, and the
	// offset to continue from.
	ConsoleLog(path string, from int64) ([]byte, int64, error)
}

//go:build linux

package tap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/day0/clsys"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// A factory Cumulus switch's reboot, which its own `reboot` (clsys) or NVUE's @reboot asks for and
// the twin carries out (18 — Z7a): the switch's container keeps running, and what a reboot does to
// it is done to it.
//
//   - With an install pending (`onie-install -a -i <url>`, as NVIDIA Config Manager's boot script
//     runs it when the switch's release is not the one it intends): the switch boots into ONIE,
//     whose discovery installs from the URL first (onie-discover --url, real fetch, modelled
//     install, D75); the NOS then boots at the image's release (its NVUE's grammar, its
//     /etc/os-release, its ZTP's CUMULUS-VERSION), in factory state, its ZTP armed again.
//   - Otherwise: the NOS boots again with its saved configuration (Cumulus saves on apply from
//     5.9), its management DHCP client starting again and its ZTP running if it has not finished.
//
// Either way the switch is not up (the observer's factory phase) until it has booted, so the
// critic's day0/fabric counts the boot, and the lease's loss and return are observed.

// rebootPoll is how the observer finds a reboot a switch asked for: its request file.
func (o *day0Observer) rebootPoll(sw *nosSwitch) {
	if sw.plan.NOS.Name != nos.NVUEFRR || sw.rebooting.Load() || !sw.factoryUp.Load() {
		return
	}
	b, err := containerFile(sw.c.Pid, clsys.RebootFile)
	if err != nil {
		return
	}
	var r clsys.Reboot
	if json.Unmarshal(b, &r) != nil {
		return // still being written
	}
	go func() {
		if err := o.s.reboot(o.s.ctx, sw, "reboot"); err != nil {
			o.s.log.Warn("a switch's reboot failed", "switch", sw.plan.Device.Path, "err", err)
		}
	}()
}

// reboot carries a switch's reboot out. via is how it was asked for: reboot (the switch's
// command) or nvue (@reboot).
func (s *Substrate) reboot(ctx context.Context, sw *nosSwitch, via string) error {
	if !sw.rebooting.CompareAndSwap(false, true) {
		return fmt.Errorf("%s is rebooting already", sw.plan.Device.Path)
	}
	defer sw.rebooting.Store(false)
	f := sw.plan.Config.Factory
	exec := func(script string) (string, error) { return s.execIn(sw, "sh", "-c", script) }
	pending := ""
	if b, err := containerFile(sw.c.Pid, clsys.PendingFile); err == nil {
		var p clsys.Pending
		if json.Unmarshal(b, &p) == nil {
			pending = p.URL
		}
	}
	canonical := "switch.reboot"
	if pending != "" {
		canonical = "switch.reboot-install"
	}
	s.eng.LogRead(engine.Request{Path: sw.plan.Device.Path, Canonical: canonical, Protocol: "nos", Method: via,
		Target: pending, Source: schema.EventSource{Class: "stack", Principal: "switch"}}, 200)
	// going down: ZTP and whatever it runs (the script that asked, its reboot waiting) end, the
	// management port's lease with them (the patterns bracketed so this shell's own command line,
	// which holds them, does not match)
	sw.factoryUp.Store(false)
	if _, err := exec("rm -f " + clsys.RebootFile + " " + clsys.PendingFile + "; pkill -f '[d]ctwin-day0 ztp daemon'; pkill -f '[z]tp-script-'; pkill -x reboot; " +
		"kill $(cat /run/dhclient." + nos.MgmtIf + ".pid 2>/dev/null) 2>/dev/null; rm -f /run/dhclient." + nos.MgmtIf + ".pid; ip -4 addr flush dev " + nos.MgmtIf + "; true"); err != nil {
		return fmt.Errorf("going down: %w", err)
	}
	if pending != "" {
		// ONIE: the URL first, then its waterfall, until an image installs (installTime)
		if out, err := exec("rm -f " + nos.Day0State + "/onie/status.json; until " + nos.Day0Mount + "/dctwin-day0 onie-discover install --url '" +
			strings.ReplaceAll(pending, "'", "") + "'; do sleep 5; done"); err != nil {
			return fmt.Errorf("ONIE's install from %s: %w: %s", pending, err, out)
		}
		var st struct {
			Version string `json:"version"`
		}
		if b, err := containerFile(sw.c.Pid, nos.Day0State+"/onie/status.json"); err == nil && json.Unmarshal(b, &st) == nil && st.Version != "" {
			v, err := nvue.ParseVersion(st.Version)
			if err != nil {
				return fmt.Errorf("the installed image's version %q: %w", st.Version, err)
			}
			sw.nvue.SetBuild(nvue.Build(v))
		}
		// a new NOS: its factory configuration and login, in its release's grammar
		if err := sw.nvue.ResetFactory(ctx); err != nil {
			return fmt.Errorf("the new NOS's factory configuration: %w", err)
		}
		if _, err := exec("echo '" + nvue.User + ":" + nvue.FactoryPassword + "' | chpasswd && chage -d 0 " + nvue.User +
			" && rm -f /run/dctwin/nvue-user-applied && ztp -R >/dev/null 2>&1; true"); err != nil {
			return fmt.Errorf("the new NOS's factory login: %w", err)
		}
	}
	// up: the release's files, the management DHCP client (ifupdown's post-up starts it), ZTP
	if _, err := exec(nos.Day0Mount + "/dctwin-day0 os-release; ifreload -a; true"); err != nil {
		return fmt.Errorf("booting: %w", err)
	}
	if f.ZTP {
		if err := s.docker.ExecDetached(s.ctx, sw.c.ID, "sh", "-c", "exec "+nos.Day0Mount+"/dctwin-day0 ztp daemon --nos "+sw.plan.NOS.ZTPFlavour()); err != nil {
			return fmt.Errorf("its ZTP: %w", err)
		}
	}
	sw.factoryUp.Store(true)
	return nil
}

// nvueActions are a factory switch's NVUE actions: @reboot, @reset (system/factory-default) and
// its ZTP's status (system/ztp).
func (s *Substrate) nvueActions(sw *nosSwitch) nvue.Actions {
	if sw.plan.Config.Factory == nil {
		return nvue.Actions{}
	}
	path := sw.plan.Device.Path
	return nvue.Actions{
		Reboot:       func(ctx context.Context) error { return s.reboot(ctx, sw, "nvue") },
		FactoryReset: func(ctx context.Context) error { return s.factoryReset(ctx, path, false) },
		ZTPStatus: func(ctx context.Context) (string, error) {
			var st struct {
				State string `json:"state"`
			}
			b, err := containerFile(sw.c.Pid, nos.Day0State+"/ztp/status.json")
			if err != nil {
				return "not-started", nil
			}
			if err := json.Unmarshal(b, &st); err != nil {
				return "", err
			}
			return map[string]string{"running": "in-progress", "discovering": "in-progress", "success": "success",
				"failed": "failed", "disabled": "disabled", "stopped": "disabled"}[st.State], nil
		},
	}
}

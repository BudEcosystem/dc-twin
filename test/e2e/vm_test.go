package e2e

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	ipmia "github.com/BudEcosystem/dc-twin/internal/adapter/ipmi"
	rfa "github.com/BudEcosystem/dc-twin/internal/adapter/redfish"
	"github.com/BudEcosystem/dc-twin/internal/adapter/vm"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/report"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
	"github.com/BudEcosystem/dc-twin/internal/twin"
	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/redfish"
)

const vmTray = "site-1/hall-a/a01/compute-tray-01"

// TestVMFidelity brings up one GB300 tray as a real aarch64 virtual machine behind its
// emulated BMC (examples/estates/gb300-vm.yaml) and drives it the way a lifecycle manager does,
// through Redfish and IPMI. The tray's power is observed from QEMU (D19): every change below is
// a QMP event or a process exit reported by the observer, and Redfish, IPMI, the engine and
// QEMU itself must agree after each step.
func TestVMFidelity(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a virtual machine")
	}
	inst, err := qemu.Find("aarch64", "", false)
	if err != nil {
		t.Skipf("vm fidelity needs QEMU for an accelerated aarch64 guest: %v", err)
	}
	g := vm.Guests["aarch64"]
	if g.Present(vm.GuestDir(twin.AssetsDir(), "", g)) != nil {
		if err := vm.FetchGuest(twin.AssetsDir(), testWriter{t}); err != nil {
			t.Skipf("vm fidelity needs the twin's guest image %s (from %s): %v", g.Name, g.Source, err)
		}
	}
	tw := up(t, "../../examples/estates/gb300-vm.yaml", []string{"redfish", "ipmi", "vm"}, nil)
	va := tw.Adapter("vm").(*vm.Adapter)
	d := tw.Engine.M.DeviceBy[vmTray]
	for _, mi := range va.Machines() {
		t.Logf("machine %s: %s %s, %d vCPU, %d MB, TPM %v", mi.Path, mi.Arch, mi.Accel, mi.CPUs, mi.MemoryMB, mi.TPM)
	}
	t.Logf("QEMU %s at %s, accelerator %s, firmware %s", inst.Version, inst.Binary, inst.Accel, inst.Code)

	// the estate starts the tray on: the machine boots, and the model learns it from QEMU
	waitBooted(t, tw, va, "os", 120*time.Second)
	t.Logf("QEMU command line: %s", va.Machines()[0].CommandLine)

	rf := tw.Adapter("redfish").(*rfa.Adapter)
	addr, _, ok := rf.BMC(vmTray)
	if !ok {
		t.Fatal("the vm tray has no BMC")
	}
	c, err := gofish.Connect(gofish.ClientConfig{Endpoint: "https://" + addr, Username: d.Identity.BMCUser, Password: d.Identity.BMCPassword, Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	sys := system(t, c, "System_0")
	chassis, _ := c.Service.Chassis()
	var c0 *redfish.Chassis
	for _, ch := range chassis {
		if ch.ID == "Chassis_0" {
			c0 = ch
		}
	}
	if c0 == nil {
		t.Fatal("no Chassis_0")
	}
	ipmi := ipmitoolOrSkip(tw, vmTray)
	agree(t, tw, va, c, sys, ipmi, model.On)

	// the guest's own view of the machine: what Linux read from the firmware's SMBIOS tables
	log := consoleText(t, va)
	id := d.Profile.Spec.Identity
	dmi := regexp.MustCompile(`DMI: ([^\r\n]*)`).FindStringSubmatch(log)
	if dmi == nil || !strings.HasPrefix(dmi[1], id.Manufacturer+" "+id.Model+"/") {
		t.Fatalf("Linux's DMI line should name %q %q: %v", id.Manufacturer, id.Model, dmi)
	}
	t.Logf("serial console: %s", strings.TrimSpace(dmi[0]))
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, "dc-twin guest: dmi") || strings.Contains(l, "dc-twin guest: nic") {
			t.Logf("serial console: %s", strings.TrimSpace(l))
		}
	}
	guest := func(key string) string {
		m := regexp.MustCompile(`dc-twin guest: dmi ` + key + `=([^\r\n]*)`).FindStringSubmatch(log)
		if m == nil {
			t.Fatalf("the guest did not report %s", key)
		}
		return strings.TrimSpace(m[1])
	}
	if s := guest("product_serial"); s != c0.SerialNumber || s != d.Identity.Serial {
		t.Fatalf("SMBIOS serial %q, Redfish Chassis_0 SerialNumber %q, model %q", s, c0.SerialNumber, d.Identity.Serial)
	}
	if u := guest("product_uuid"); !strings.EqualFold(u, sys.UUID) {
		t.Fatalf("SMBIOS UUID %q, Redfish System_0 UUID %q", u, sys.UUID)
	}
	if v, m := guest("sys_vendor"), guest("product_name"); v != c0.Manufacturer || m != c0.Model {
		t.Fatalf("SMBIOS %q %q, Redfish Chassis_0 %q %q", v, m, c0.Manufacturer, c0.Model)
	}
	t.Logf("SMBIOS = Redfish: serial %s, UUID %s, %s %s", c0.SerialNumber, sys.UUID, c0.Manufacturer, c0.Model)
	for _, p := range []string{"bf3-0:p0", "bf3-0:p1"} {
		mac := tw.Engine.M.Ports[vmTray+"/"+p].MAC
		if !strings.Contains(log, " "+mac) {
			t.Fatalf("the guest has no NIC with the model's MAC %s (%s)", mac, p)
		}
	}

	// Redfish ForceOff: QEMU quits, the observer reports the machine off
	since := tw.Engine.Seq()
	if err := sys.Reset(redfish.ForceOffResetType); err != nil {
		t.Fatal(err)
	}
	ev := waitPower(t, tw, since, model.Off)
	if ev.Data["reason"] != "host-qmp-quit" {
		t.Fatalf("ForceOff should be QEMU quitting: %v", ev.Data)
	}
	agree(t, tw, va, c, sys, ipmi, model.Off)

	// IPMI chassis power on: a second protocol, the same machine
	since = tw.Engine.Seq()
	if ipmi != nil {
		t.Logf("$ ipmitool chassis power on: %s", ipmi("chassis", "power", "on"))
	} else if err := sys.Reset(redfish.OnResetType); err != nil {
		t.Fatal(err)
	}
	waitPower(t, tw, since, model.On)
	waitBooted(t, tw, va, "os", 120*time.Second)
	agree(t, tw, va, c, sys, ipmi, model.On)

	// Redfish PowerCycle: off, then on again — a new process on the same NVRAM
	_, pid, _ := va.Status(vmTray)
	since = tw.Engine.Seq()
	if err := sys.Reset(redfish.PowerCycleResetType); err != nil {
		t.Fatal(err)
	}
	if ev := waitPower(t, tw, since, model.Off); ev.Data["reason"] != "host-qmp-quit" {
		t.Fatalf("a power cycle starts by pulling the power: %v", ev.Data)
	}
	if ev := waitPower(t, tw, since, model.On); ev.Data["reset"] != true {
		t.Fatalf("the power-on of a cycle is a reset (device.reset fires): %v", ev.Data)
	}
	waitBooted(t, tw, va, "os", 120*time.Second)
	if _, pid2, _ := va.Status(vmTray); pid2 == pid || pid2 == 0 {
		t.Fatalf("a power cycle runs a new process: %d → %d", pid, pid2)
	}
	agree(t, tw, va, c, sys, ipmi, model.On)

	// Redfish ForceRestart: the reset line, the same process
	_, pid, _ = va.Status(vmTray)
	since = tw.Engine.Seq()
	if err := sys.Reset(redfish.ForceRestartResetType); err != nil {
		t.Fatal(err)
	}
	reset := waitEvent(t, tw, since, func(e schema.Event) bool { return e.State != nil && e.State.Field == "reset" }, 30*time.Second)
	if reset.State.To != "host-qmp-system-reset" || reset.Source.Class != "observer" {
		t.Fatalf("ForceRestart should be observed as QEMU's system reset: %+v", reset)
	}
	waitEvent(t, tw, reset.Seq, func(e schema.Event) bool { return e.State != nil && e.State.Field == "boot" && e.State.To == "os" }, 120*time.Second)
	if _, pid2, _ := va.Status(vmTray); pid2 != pid {
		t.Fatalf("a restart resets the machine; the process changed %d → %d", pid, pid2)
	}
	agree(t, tw, va, c, sys, ipmi, model.On)

	// Redfish GracefulRestart: the guest shuts down on the power button, then the BMC powers on
	since = tw.Engine.Seq()
	if err := sys.Reset(redfish.GracefulRestartResetType); err != nil {
		t.Fatal(err)
	}
	off := waitPower(t, tw, since, model.Off)
	if off.Data["reason"] != "guest-shutdown" {
		t.Fatalf("a graceful restart is the guest shutting down first: %v", off.Data)
	}
	waitPower(t, tw, off.Seq, model.On)
	waitBooted(t, tw, va, "os", 120*time.Second)
	agree(t, tw, va, c, sys, ipmi, model.On)

	// Redfish GracefulShutdown: the ACPI power button; the guest decides, and here powers off
	since = tw.Engine.Seq()
	if err := sys.Reset(redfish.GracefulShutdownResetType); err != nil {
		t.Fatal(err)
	}
	ev = waitPower(t, tw, since, model.Off)
	if ev.Data["reason"] != "guest-shutdown" || ev.Data["guest"] != true {
		t.Fatalf("GracefulShutdown should end in the guest powering off: %v", ev.Data)
	}
	if !strings.Contains(consoleText(t, va), "dc-twin guest: power button pressed: powering off") {
		t.Fatal("the guest did not see the power button")
	}
	agree(t, tw, va, c, sys, ipmi, model.Off)

	// what the observer committed, in order
	for _, e := range tw.Engine.Events(0, func(e schema.Event) bool { return e.Path == vmTray && e.Source.Class == "observer" }) {
		q := ""
		if v, ok := e.Data["qmp"]; ok {
			q = fmt.Sprintf(" [QMP %v %v]", v, e.Data["reason"])
		}
		t.Logf("#%d %s %s → %v%s ←#%d %s", e.Seq, e.Kind, e.State.Field, e.State.To, q, e.Cause, e.Message)
	}

	rep := report.Build(tw, nil)
	text := rep.Text()
	for _, want := range []string{"1 vm (os: twin)", "power observed from QEMU", "UEFI NVRAM", "QEMU user-mode networking",
		"Grace is QEMU aarch64"} { // the profile's isNot at vm, beside its model one
		if !strings.Contains(text, want) {
			t.Fatalf("the fidelity report does not say %q:\n%s", want, text)
		}
	}
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, "vm") || strings.Contains(l, "substrate-owned") || strings.Contains(l, "tcg") {
			t.Logf("report: %s", l)
		}
	}
	if len(tw.Engine.Incidents()) > 0 {
		t.Fatalf("twin incidents: %v", tw.Engine.Incidents())
	}
}

// Without the vm adapter a vm device is refused at up, with the reason; nothing runs it at a
// fidelity it was not given.
func TestVMRefusedWithoutItsSubstrate(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../examples/estates/gb300-vm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// AllowTCG so that on a host of another architecture (an x86_64 lab) the refusal under test,
	// not the earlier TCG one, is what answers
	_, err = twin.UpEstate(est, "", reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{"redfish"}, AllowTCG: true})
	if err == nil || !strings.Contains(err.Error(), vmTray+" is at vm fidelity and cannot run here: the vm adapter is not enabled") {
		t.Fatalf("want a refusal naming the tray and the reason, got %v", err)
	}
}

func system(t *testing.T, c *gofish.APIClient, id string) *redfish.ComputerSystem {
	t.Helper()
	systems, err := c.Service.Systems()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range systems {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no system %s", id)
	return nil
}

// agree checks that every view of the tray's power says the same: the engine's observed
// field, Redfish PowerState, IPMI chassis status and QEMU's own query-status.
func agree(t *testing.T, tw *twin.Twin, va *vm.Adapter, c *gofish.APIClient, sys *redfish.ComputerSystem, ipmi func(...string) string, want string) {
	t.Helper()
	view := tw.Engine.DeviceView(tw.Engine.M.DeviceBy[vmTray])
	s, err := redfish.GetComputerSystem(c, sys.ODataID)
	if err != nil {
		t.Fatal(err)
	}
	st, pid, err := va.Status(vmTray)
	if err != nil {
		t.Fatal(err)
	}
	ip := ""
	if ipmi != nil {
		ip = strings.TrimSpace(ipmi("chassis", "power", "status"))
	}
	t.Logf("agree: engine power %v (observed %v) · Redfish PowerState %s · IPMI %q · QEMU %s (pid %d)", view["power"], view["observed"], s.PowerState, ip, st, pid)
	wantQEMU, wantIPMI := "running", "Chassis Power is on"
	if want == model.Off {
		wantQEMU, wantIPMI = "off", "Chassis Power is off"
	}
	if view["power"] != want || string(s.PowerState) != want || st != wantQEMU || (ipmi != nil && ip != wantIPMI) {
		t.Fatalf("the views disagree (want %s)", want)
	}
}

func waitEvent(t *testing.T, tw *twin.Twin, since uint64, f func(schema.Event) bool, within time.Duration) schema.Event {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if evs := tw.Engine.Events(since, func(e schema.Event) bool { return e.Path == vmTray && f(e) }); len(evs) > 0 {
			return evs[0]
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no such event within %s", within)
	return schema.Event{}
}

func waitPower(t *testing.T, tw *twin.Twin, since uint64, to string) schema.Event {
	t.Helper()
	ev := waitEvent(t, tw, since, func(e schema.Event) bool {
		return e.Kind == "state" && e.State != nil && e.State.Field == "power" && e.State.To == to
	}, 60*time.Second)
	if ev.Source.Class != "observer" || ev.Source.Principal != "qemu" {
		t.Fatalf("power %s was not observed from QEMU: %+v", to, ev.Source)
	}
	t.Logf("observed: #%d power → %s (%s) ←#%d", ev.Seq, to, ev.Message, ev.Cause)
	return ev
}

func waitBooted(t *testing.T, tw *twin.Twin, va *vm.Adapter, step string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if tw.Engine.DeviceView(tw.Engine.M.DeviceBy[vmTray])["booted"] == step {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the vm tray did not reach boot step %q within %s; console:\n%s", step, within, tail(consoleText(t, va), 40))
}

func consoleText(t *testing.T, va *vm.Adapter) string {
	t.Helper()
	b, _, err := va.ConsoleLog(vmTray, 0)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func tail(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ipmitoolOrSkip returns a runner for the real ipmitool against the tray's BMC, or nil when
// ipmitool is not installed (the IPMI checks are then skipped, and the log says so).
func ipmitoolOrSkip(tw *twin.Twin, path string) func(...string) string {
	bin, err := exec.LookPath("ipmitool")
	if err != nil {
		return nil
	}
	a, ok := tw.Adapter("ipmi").(*ipmia.Adapter)
	if !ok {
		return nil
	}
	host, port, err := net.SplitHostPort(a.Endpoint(path))
	if err != nil {
		return nil
	}
	d := tw.Engine.M.DeviceBy[path]
	return func(args ...string) string {
		base := []string{"-N", "1", "-R", "3", "-I", "lanplus", "-C", "17", "-H", host, "-p", port, "-U", d.Identity.BMCUser, "-E"}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append(base, args...)...)
		cmd.Env = append(os.Environ(), "IPMI_PASSWORD="+d.Identity.BMCPassword)
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
}

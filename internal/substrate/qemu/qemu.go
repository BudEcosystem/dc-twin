// Package qemu is the `vm` substrate (docs/spec/02-architecture.md — The substrate): one QEMU
// process per virtual machine, real UEFI firmware with a persisted NVRAM file, SMBIOS from the
// model's identity, virtio NICs with the model's MACs, a serial console logged to a file, and a
// QMP socket for control and events. It knows nothing of the model: the vm adapter derives a
// Config from a device and observes the machine through QMP and the process.
//
// Power maps onto the process the way libvirt maps a domain: a machine that is off has no
// process; power on starts one (paused, so the observer is connected before the first
// instruction runs) and continues it; a forced off is QMP `quit`; a graceful shutdown is ACPI
// (`system_powerdown`), which the guest may ignore; a reset is `system_reset`.
package qemu

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Config is one virtual machine. Everything in it is derived from the model by the caller.
type Config struct {
	Name     string // the device path, for -name and messages
	Arch     string // aarch64 | x86_64
	Accel    string // hvf | kvm | tcg
	CPUs     int
	MemoryMB int
	Binary   string // qemu-system-<arch>
	// Code is the UEFI firmware image (read-only pflash); VarsTemplate is copied once into Vars,
	// the device's own NVRAM, which persists across power cycles and runs.
	Code, VarsTemplate, Vars string
	UUID                     string
	SMBIOS                   []SMBIOS
	NICs                     []NIC
	// Kernel, Initrd and Append direct-boot a kernel through the firmware's QEMU kernel loader
	// (fw_cfg): UEFI runs it as an EFI application, so the guest still sees the firmware's
	// SMBIOS and ACPI tables. Empty: the firmware boots from its own boot order.
	Kernel, Initrd, Append string
	// Disks are extra block devices (path, format); none by default.
	Disks []Disk
	// RunDir holds the sockets (kept short: a unix socket path is limited to ~104 bytes).
	RunDir string
	// Console is the serial console's log file (appended to across power cycles).
	Console string
	// TPM is a swtpm control socket to attach as a TPM 2.0 device, if any.
	TPM   string
	Extra []string
}

// SMBIOS is one structure type and its fields in order (QEMU's -smbios type=N,k=v,…).
type SMBIOS struct {
	Type   int
	Fields [][2]string
}

// NIC is one virtio NIC: its MAC and the -netdev it is attached to (without id).
type NIC struct {
	ID     string
	MAC    string
	Netdev string // e.g. "user,restrict=on"
}

// Disk is an extra drive.
type Disk struct {
	Path, Format, Serial string
}

// QMPSocket, ConsoleSocket and PIDFile are where the machine's control files live.
func (c Config) QMPSocket() string     { return filepath.Join(c.RunDir, "qmp.sock") }
func (c Config) ConsoleSocket() string { return filepath.Join(c.RunDir, "console.sock") }
func (c Config) PIDFile() string       { return filepath.Join(c.RunDir, "qemu.pid") }

func esc(s string) string { return strings.ReplaceAll(s, ",", ",,") }

// Args is the QEMU command line for the configuration, without the binary. The machine
// starts paused (-S) so that the observer is attached before the guest runs.
func (c Config) Args() []string {
	a := []string{"-name", esc("dctwin:" + c.Name), "-nodefaults", "-no-user-config", "-display", "none", "-S"}
	switch c.Arch {
	case "aarch64":
		// GICv3 (or the host's): KVM on GICv3-only arm64 servers cannot create a GICv2
		a = append(a, "-machine", "virt,gic-version=max")
	default:
		a = append(a, "-machine", "q35")
	}
	a = append(a, "-accel", c.Accel)
	cpu := "host"
	if c.Accel == "tcg" {
		cpu = "max"
	}
	a = append(a, "-cpu", cpu, "-smp", strconv.Itoa(max(c.CPUs, 1)), "-m", strconv.Itoa(max(c.MemoryMB, 256)))
	a = append(a,
		"-drive", "if=pflash,format=raw,unit=0,readonly=on,file="+esc(c.Code),
		"-drive", "if=pflash,format=raw,unit=1,file="+esc(c.Vars))
	if c.UUID != "" {
		a = append(a, "-uuid", c.UUID)
	}
	for _, s := range c.SMBIOS {
		parts := []string{"type=" + strconv.Itoa(s.Type)}
		for _, f := range s.Fields {
			if f[1] == "" {
				continue
			}
			parts = append(parts, f[0]+"="+esc(f[1]))
		}
		a = append(a, "-smbios", strings.Join(parts, ","))
	}
	for i, n := range c.NICs {
		id := n.ID
		if id == "" {
			id = fmt.Sprintf("net%d", i)
		}
		a = append(a, "-netdev", n.Netdev+",id="+id, "-device", "virtio-net-pci,romfile=,netdev="+id+",mac="+n.MAC)
	}
	for i, d := range c.Disks {
		f := d.Format
		if f == "" {
			f = "qcow2"
		}
		a = append(a, "-drive", fmt.Sprintf("if=none,id=disk%d,format=%s,file=%s", i, f, esc(d.Path)),
			"-device", fmt.Sprintf("nvme,drive=disk%d,serial=%s", i, esc(d.Serial)))
	}
	if c.TPM != "" {
		// the sysbus TIS on aarch64 virt, without the physical-presence RAM page: its 1 KiB
		// mapping is refused by Hypervisor.framework (16 KiB pages) with HV_BAD_ARGUMENT
		dev := "tpm-tis-device,ppi=off"
		if c.Arch != "aarch64" {
			dev = "tpm-tis"
		}
		a = append(a, "-chardev", "socket,id=chrtpm,path="+esc(c.TPM), "-tpmdev", "emulator,id=tpm0,chardev=chrtpm", "-device", dev+",tpmdev=tpm0")
	}
	a = append(a, "-device", "virtio-rng-pci",
		"-chardev", "socket,id=con0,server=on,wait=off,path="+esc(c.ConsoleSocket())+",logfile="+esc(c.Console)+",logappend=on",
		"-serial", "chardev:con0",
		"-qmp", "unix:"+esc(c.QMPSocket())+",server=on,wait=off",
		"-pidfile", c.PIDFile())
	if c.Kernel != "" {
		a = append(a, "-kernel", c.Kernel)
		if c.Initrd != "" {
			a = append(a, "-initrd", c.Initrd)
		}
		if c.Append != "" {
			a = append(a, "-append", c.Append)
		}
	}
	return append(a, c.Extra...)
}

// CommandLine is the full command line, quoted for a reader (the fidelity report, logs).
func (c Config) CommandLine() string {
	parts := []string{c.Binary}
	for _, a := range c.Args() {
		if strings.ContainsAny(a, " \"'") {
			a = strconv.Quote(a)
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// Machine is a running QEMU process.
type Machine struct {
	Cfg     Config
	QMP     *QMP
	cmd     *exec.Cmd
	exited  chan struct{}
	exitErr error
	stderr  bytes.Buffer
	Started time.Time
}

// Start prepares the NVRAM (first start only), reaps a stale process from an earlier run,
// starts QEMU paused and connects QMP. The caller continues it with Cont.
func Start(cfg Config) (*Machine, error) {
	if err := os.MkdirAll(cfg.RunDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Vars), 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(cfg.Vars); os.IsNotExist(err) {
		if err := copyFile(cfg.VarsTemplate, cfg.Vars); err != nil {
			return nil, fmt.Errorf("nvram: %w", err)
		}
	}
	ReapStale(cfg.PIDFile())
	for _, s := range []string{cfg.QMPSocket(), cfg.ConsoleSocket()} {
		_ = os.Remove(s)
	}
	m := &Machine{Cfg: cfg, exited: make(chan struct{})}
	m.cmd = exec.Command(cfg.Binary, cfg.Args()...)
	m.cmd.Stdout = io.Discard
	m.cmd.Stderr = &limitedWriter{b: &m.stderr, n: 64 << 10}
	if err := m.cmd.Start(); err != nil {
		return nil, err
	}
	m.Started = time.Now()
	go func() {
		m.exitErr = m.cmd.Wait()
		close(m.exited)
	}()
	q, err := DialQMP(cfg.QMPSocket(), 15*time.Second, m.exited)
	if err != nil {
		_ = m.cmd.Process.Kill()
		<-m.exited
		return nil, fmt.Errorf("%w; qemu: %s", err, strings.TrimSpace(m.stderr.String()))
	}
	m.QMP = q
	return m, nil
}

// Cont lets the guest run (the machine was started paused).
func (m *Machine) Cont() error { _, err := m.QMP.Execute("cont", nil); return err }

// Powerdown presses the ACPI power button; the guest decides what to do with it.
func (m *Machine) Powerdown() error { _, err := m.QMP.Execute("system_powerdown", nil); return err }

// Reset is a hard reset of the machine (the reset line, not the guest's choice).
func (m *Machine) Reset() error { _, err := m.QMP.Execute("system_reset", nil); return err }

// Quit ends the process as pulling the power does, killing it if QMP does not answer.
func (m *Machine) Quit(timeout time.Duration) {
	if m.QMP != nil {
		_, _ = m.QMP.Execute("quit", nil)
	}
	select {
	case <-m.exited:
	case <-time.After(timeout):
		_ = m.cmd.Process.Kill()
		<-m.exited
	}
}

// Exited is closed when the process has ended.
func (m *Machine) Exited() <-chan struct{} { return m.exited }

// ExitError is the process's exit status and the tail of its stderr, once it has exited.
func (m *Machine) ExitError() string {
	s := strings.TrimSpace(m.stderr.String())
	if m.exitErr != nil {
		if s != "" {
			return m.exitErr.Error() + ": " + s
		}
		return m.exitErr.Error()
	}
	return s
}

// PID is the QEMU process id.
func (m *Machine) PID() int { return m.cmd.Process.Pid }

// ReapStale kills a QEMU process left by an earlier run of the same machine (its pidfile names
// it and its command line names the pidfile), so that a restarted twin owns its machines.
func ReapStale(pidfile string) {
	b, err := os.ReadFile(pidfile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil || !strings.Contains(string(out), pidfile) {
		return
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = p.Signal(syscall.SIGKILL)
	for i := 0; i < 50; i++ {
		if p.Signal(syscall.Signal(0)) != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = os.Remove(pidfile)
}

type limitedWriter struct {
	b *bytes.Buffer
	n int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.n - w.b.Len(); room > 0 {
		if len(p) > room {
			w.b.Write(p[:room])
		} else {
			w.b.Write(p)
		}
	}
	return len(p), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ---------------------------------------------------------------- the installation

// Installation is what this host offers for one guest architecture.
type Installation struct {
	Arch         string
	Binary       string
	Code         string
	VarsTemplate string
	Accel        string // hvf | kvm | tcg
	Version      string
}

// HostArch is this machine's architecture in QEMU's spelling.
func HostArch() string {
	switch runtime.GOARCH {
	case "arm64":
		return "aarch64"
	case "amd64":
		return "x86_64"
	}
	return runtime.GOARCH
}

// Find locates QEMU and UEFI firmware for a guest architecture and chooses the accelerator:
// the host's hypervisor (Hypervisor.framework on macOS, KVM on Linux) when the guest is of the
// host's architecture and the hypervisor is usable, else TCG, which only allowTCG accepts.
// binDir, when set, is searched before PATH.
func Find(arch, binDir string, allowTCG bool) (*Installation, error) {
	name := "qemu-system-" + arch
	var bin string
	if binDir != "" {
		if p := filepath.Join(binDir, name); isFile(p) {
			bin = p
		}
	}
	if bin == "" {
		p, err := exec.LookPath(name)
		if err != nil {
			return nil, fmt.Errorf("%s is not installed (on macOS: brew install qemu; on Linux: the distribution's qemu-system package)", name)
		}
		bin = p
	}
	in := &Installation{Arch: arch, Binary: bin}
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		bin = real
	}
	var code, vars []string
	share := []string{filepath.Join(filepath.Dir(bin), "..", "share", "qemu"), "/opt/homebrew/share/qemu", "/usr/local/share/qemu", "/usr/share/qemu"}
	switch arch {
	case "aarch64":
		for _, s := range share {
			code = append(code, filepath.Join(s, "edk2-aarch64-code.fd"))
			vars = append(vars, filepath.Join(s, "edk2-arm-vars.fd"))
		}
		code = append(code, "/usr/share/AAVMF/AAVMF_CODE.fd")
		vars = append(vars, "/usr/share/AAVMF/AAVMF_VARS.fd")
	case "x86_64":
		for _, s := range share {
			code = append(code, filepath.Join(s, "edk2-x86_64-code.fd"))
			vars = append(vars, filepath.Join(s, "edk2-i386-vars.fd"))
		}
		code = append(code, "/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_CODE.fd")
		vars = append(vars, "/usr/share/OVMF/OVMF_VARS_4M.fd", "/usr/share/OVMF/OVMF_VARS.fd")
	default:
		return nil, fmt.Errorf("no vm substrate for guest architecture %q", arch)
	}
	in.Code, in.VarsTemplate = first(code), first(vars)
	if in.Code == "" || in.VarsTemplate == "" {
		return nil, fmt.Errorf("%s has no UEFI firmware beside it (looked for %s and %s)", bin, filepath.Base(code[0]), filepath.Base(vars[0]))
	}
	in.Accel = "tcg"
	if arch == HostArch() {
		switch runtime.GOOS {
		case "darwin":
			if out, err := exec.Command("sysctl", "-n", "kern.hv_support").Output(); err == nil && strings.TrimSpace(string(out)) == "1" {
				in.Accel = "hvf"
			}
		case "linux":
			if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
				f.Close()
				in.Accel = "kvm"
			}
		}
	}
	if in.Accel == "tcg" && !allowTCG {
		why := "the guest's architecture differs from the host's"
		if arch == HostArch() {
			why = "this host's hypervisor (KVM or Hypervisor.framework) is not usable"
		}
		return nil, fmt.Errorf("a %s guest would run under TCG here (%s); pass --allow-tcg to accept it (09)", arch, why)
	}
	if out, err := exec.Command(in.Binary, "--version").Output(); err == nil {
		if f := strings.Fields(string(out)); len(f) >= 4 {
			in.Version = f[3]
		}
	}
	return in, nil
}

func first(paths []string) string {
	for _, p := range paths {
		if isFile(p) {
			if abs, err := filepath.Abs(p); err == nil {
				return filepath.Clean(abs)
			}
			return p
		}
	}
	return ""
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

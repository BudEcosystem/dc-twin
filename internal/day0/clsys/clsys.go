// Package clsys is what a Cumulus Linux switch has on its system that a day-0 tool's script calls
// beyond NVUE (18 — DSX OS: NVIDIA Config Manager's ZTP, Z7a), for the generic/nvue-frr NOS:
//
//   - `ip vrf exec <vrf> <cmd>`, which a container cannot do as Linux does (it needs a cgroup and a
//     BPF program): the command runs with the image's socket-binding preload, every IPv4 and IPv6
//     socket it opens bound to the VRF's device (SO_BINDTODEVICE), so its traffic is routed in the
//     VRF's table. Any other `ip` command is the system's ip.
//   - `onie-install -a -i <url> [-f] [-z <url>]`, Cumulus's way to reinstall from the NOS: it
//     leaves ONIE an install URL for the next boot (Cumulus Linux's Upgrading Cumulus Linux page;
//     NVIDIA Config Manager's boot script calls it when the switch's release is not the one it
//     intends). The twin writes it to onie/pending.json; the next reboot installs from it.
//   - `reboot`: a request the twin carries out (reboot.json, read by the twin's day-0 observer,
//     internal/adapter/netsim/tap): with an install pending, the switch returns to ONIE, which
//     installs from the URL, and the NOS boots at the image's release in factory state; otherwise
//     the NOS restarts its management DHCP and its ZTP, keeping its saved configuration. The
//     command does not return until the twin has acted (a reboot ends the caller), or fails after
//     RebootWait.
//   - decode-syseeprom (-s, -m): the ONIE EEPROM, from the switch's identity; timedatectl show
//     (NTPSynchronized from chrony, as the container runs no systemd).
//   - /etc/os-release and /etc/lsb-release, as Cumulus writes them (NAME="Cumulus Linux",
//     VERSION_ID=<release>): the release ONIE installed, else the switch's own (identity.json).
package clsys

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// The image's socket-binding preload and the environment it reads (test/linuxnet/nos-nvue).
const (
	Preload  = "/usr/lib/dctwin/vrfbind.so"
	VRFEnv   = "DCTWIN_VRF"
	systemIP = "ip"
)

// Paths the programs keep their state at, in the switch.
var (
	PendingFile = filepath.Join(nos.Day0State, "onie", "pending.json")
	RebootFile  = filepath.Join(nos.Day0State, "reboot.json")
	// RebootWait is how long reboot waits for the twin to act before it fails.
	RebootWait = 10 * time.Minute
)

// Pending is an install onie-install leaves for the next boot.
type Pending struct {
	URL    string `json:"url"`
	ZTPURL string `json:"ztpURL,omitempty"`
	Time   string `json:"time"`
}

// Reboot is a reboot the switch asked for.
type Reboot struct {
	Time string `json:"time"`
	// Install: an install was pending when it was asked for
	Install bool `json:"install,omitempty"`
}

// IP is `ip`: `ip vrf exec <vrf> <cmd> [args]` through the preload, anything else the system's.
func IP(args []string) int {
	if len(args) >= 3 && args[0] == "vrf" && (args[1] == "exec" || args[1] == "exe" || args[1] == "e") {
		return vrfExec(args[2], args[3:], os.Stderr)
	}
	return execSystem(systemIP, args, os.Stderr)
}

// VRFExec is `dctwin-vrf-exec <vrf> <cmd> [args]`.
func VRFExec(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: dctwin-vrf-exec <vrf> <command> [args]")
		return 2
	}
	return vrfExec(args[0], args[1:], os.Stderr)
}

func vrfExec(vrf string, argv []string, stderr io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "ip vrf exec: a command is needed")
		return 2
	}
	if _, err := os.Stat(filepath.Join("/sys/class/net", vrf)); err != nil {
		fmt.Fprintf(stderr, "Invalid VRF name\nFailed to get VRF id for %s\n", vrf)
		return 1
	}
	if _, err := os.Stat(Preload); err != nil {
		fmt.Fprintf(stderr, "ip vrf exec %s: the switch image has no %s to bind sockets to the VRF with\n", vrf, Preload)
		return 1
	}
	env := []string{VRFEnv + "=" + vrf}
	pre := Preload
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, VRFEnv+"="):
		case strings.HasPrefix(kv, "LD_PRELOAD="):
			for _, p := range strings.Fields(strings.ReplaceAll(strings.TrimPrefix(kv, "LD_PRELOAD="), ":", " ")) {
				if p != Preload {
					pre += ":" + p
				}
			}
		default:
			env = append(env, kv)
		}
	}
	return execv(argv, append(env, "LD_PRELOAD="+pre), stderr)
}

func execSystem(name string, args []string, stderr io.Writer) int {
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
		p := filepath.Join(dir, name)
		if self, _ := os.Executable(); sameFile(p, self) {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return execv(append([]string{p}, args...), os.Environ(), stderr)
		}
	}
	fmt.Fprintf(stderr, "%s: not found\n", name)
	return 127
}

func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

func execv(argv, env []string, stderr io.Writer) int {
	path := argv[0]
	if !strings.Contains(path, "/") {
		p, err := lookPath(path, env)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", argv[0], err)
			return 127
		}
		path = p
	}
	err := syscall.Exec(path, argv, env)
	fmt.Fprintf(stderr, "%s: %v\n", argv[0], err)
	return 126
}

func lookPath(name string, env []string) (string, error) {
	path := os.Getenv("PATH")
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
		}
	}
	for _, dir := range filepath.SplitList(path) {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", errors.New("command not found")
}

// OnieInstall is Cumulus's onie-install: with -a and -i <url>, an install from the URL at the next
// boot (-z the ZTP URL ONIE then hands the NOS; -f no prompt). Only the install from a URL is
// emulated: anything else is refused by name.
func OnieInstall(args []string) int { return onieInstall(args, os.Stdout, os.Stderr, time.Now) }

func onieInstall(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	// getopt's way, as the script is: flags combine (-fa), -i and -z take the next word or the
	// rest of theirs (-ihttp://…)
	var activate bool
	var image, ztp string
	val := map[byte]*string{'i': &image, 'z': &ztp}
	for k := 0; k < len(args); k++ {
		a := args[k]
		if !strings.HasPrefix(a, "-") || len(a) < 2 {
			fmt.Fprintf(stderr, "onie-install: unexpected %q\n", a)
			return 2
		}
		for n := 1; n < len(a); n++ {
			switch c := a[n]; c {
			case 'a':
				activate = true
			case 'f', 'y':
			case 'i', 'z':
				switch {
				case n+1 < len(a):
					*val[c] = a[n+1:]
				case k+1 < len(args):
					k++
					*val[c] = args[k]
				default:
					fmt.Fprintf(stderr, "onie-install: -%c needs a URL\n", c)
					return 2
				}
				n = len(a)
			default:
				fmt.Fprintf(stderr, "onie-install: -%c is not emulated by the twin (-a, -f, -y, -i <url>, -z <url> are)\n", c)
				return 2
			}
		}
	}
	switch {
	case image == "":
		fmt.Fprintln(stderr, "onie-install: only an install from an image URL (-i <url>) is emulated by the twin")
		return 1
	case !activate:
		fmt.Fprintln(stderr, "onie-install: -a is needed to put ONIE in install mode for the next boot (the twin does not stage an image without it)")
		return 1
	}
	for _, u := range []string{image, ztp} {
		if u == "" {
			continue
		}
		p, err := url.Parse(u)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			fmt.Fprintf(stderr, "onie-install: %q is not an http or https URL\n", u)
			return 1
		}
	}
	b, _ := json.MarshalIndent(Pending{URL: image, ZTPURL: ztp, Time: now().UTC().Format(time.RFC3339)}, "", "  ")
	if err := writeFile(PendingFile, append(b, '\n')); err != nil {
		fmt.Fprintf(stderr, "onie-install: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Updating ONIE's install environment: image %s\nONIE will install the image at the next reboot\n", image)
	return 0
}

// RebootCmd is `reboot`: a request the twin's observer carries out. It waits for the twin to act
// (the reboot ends this process with the switch's other processes) and fails after RebootWait.
func RebootCmd(args []string) int { return reboot(args, os.Stdout, os.Stderr, time.Now, time.Sleep) }

func reboot(args []string, stdout, stderr io.Writer, now func() time.Time, sleep func(time.Duration)) int {
	for _, a := range args {
		switch a {
		case "-f", "--force", "-n", "--no-wall", "now":
		default:
			fmt.Fprintf(stderr, "reboot: %q is not emulated\n", a)
			return 2
		}
	}
	_, err := os.Stat(PendingFile)
	r := Reboot{Time: now().UTC().Format(time.RFC3339), Install: err == nil}
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := writeFile(RebootFile, append(b, '\n')); err != nil {
		fmt.Fprintf(stderr, "reboot: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "The system is going down for reboot NOW!")
	for waited := time.Duration(0); waited < RebootWait; waited += time.Second {
		sleep(time.Second)
	}
	fmt.Fprintf(stderr, "reboot: the twin did not reboot the switch within %s\n", RebootWait)
	return 1
}

// OSRelease writes /etc/os-release and /etc/lsb-release as Cumulus has them, for the release ONIE
// installed (onie/status.json's version), else the switch's (identity.json's).
func OSRelease(args []string) int {
	fs := flag.NewFlagSet("os-release", flag.ContinueOnError)
	root := fs.String("root", "/", "the switch's root")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	v := Release(*root)
	if v == "" {
		fmt.Fprintln(os.Stderr, "os-release: no release: neither ONIE's status nor the switch's identity names one")
		return 1
	}
	files := map[string]string{
		"etc/os-release":  fmt.Sprintf("NAME=\"Cumulus Linux\"\nVERSION_ID=%[1]s\nVERSION=\"Cumulus Linux %[1]s\"\nPRETTY_NAME=\"Cumulus Linux\"\nID=cumulus-linux\nID_LIKE=debian\nCPE_NAME=cpe:/o:nvidia:cumulus_linux:%[1]s\nHOME_URL=\"http://www.cumulusnetworks.com/\"\nSUPPORT_URL=\"http://support.cumulusnetworks.com/\"\n", v),
		"etc/lsb-release": fmt.Sprintf("DISTRIB_ID=\"Cumulus Linux\"\nDISTRIB_RELEASE=%[1]s\nDISTRIB_DESCRIPTION=\"Cumulus Linux %[1]s\"\n", v),
	}
	for name, body := range files {
		p := filepath.Join(*root, name)
		// /etc/os-release is a link to ../usr/lib/os-release on Debian: replaced, not followed
		_ = os.Remove(p)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "os-release: %v\n", err)
			return 1
		}
	}
	return 0
}

// Release is the release a switch runs: ONIE's installed image's, else its identity's.
func Release(root string) string {
	var st struct {
		Phase   string `json:"phase"`
		Version string `json:"version"`
	}
	if b, err := os.ReadFile(filepath.Join(root, nos.Day0State, "onie", "status.json")); err == nil && json.Unmarshal(b, &st) == nil && st.Phase == "installed" && st.Version != "" {
		return st.Version
	}
	var id struct {
		Version string `json:"version"`
	}
	if b, err := os.ReadFile(filepath.Join(root, nos.Day0Dir, "identity.json")); err == nil && json.Unmarshal(b, &id) == nil {
		return id.Version
	}
	return ""
}

func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// DecodeSyseeprom is Cumulus's and SONiC's decode-syseeprom: the switch's ONIE EEPROM as its identity has it.
// -s prints the serial number, -m the base MAC; with no flag, the TLVs as a table.
func DecodeSyseeprom(args []string) int {
	return decodeSyseeprom(args, nos.Day0Dir, os.Stdout, os.Stderr)
}

func decodeSyseeprom(args []string, dir string, stdout, stderr io.Writer) int {
	var id nos.Identity
	b, err := os.ReadFile(filepath.Join(dir, "identity.json"))
	if err == nil {
		err = json.Unmarshal(b, &id)
	}
	if err != nil {
		fmt.Fprintf(stderr, "decode-syseeprom: the EEPROM cannot be read: %v\n", err)
		return 1
	}
	switch {
	case len(args) == 1 && args[0] == "-s":
		fmt.Fprintln(stdout, id.Serial)
		return 0
	case len(args) == 1 && args[0] == "-m":
		fmt.Fprintln(stdout, strings.ToUpper(id.BaseMAC))
		return 0
	case len(args) > 0:
		fmt.Fprintf(stderr, "decode-syseeprom: %s is not emulated (-s, -m, or none)\n", strings.Join(args, " "))
		return 2
	}
	fmt.Fprintln(stdout, "TlvInfo Header:\n   Id String:    TlvInfo\n   Version:      1")
	fmt.Fprintln(stdout, "TLV Name             Code Len Value\n-------------------- ---- --- -----")
	for _, r := range [][3]string{{"Product Name", "0x21", id.Model}, {"Part Number", "0x22", id.PartNumber}, {"Serial Number", "0x23", id.Serial},
		{"Base MAC Address", "0x24", strings.ToUpper(id.BaseMAC)}, {"Platform Name", "0x28", id.Platform}, {"Manufacturer", "0x2B", id.Manufacturer}} {
		if r[2] != "" {
			fmt.Fprintf(stdout, "%-20s %s %3d %s\n", r[0], r[1], len(r[2]), r[2])
		}
	}
	return 0
}

// Timedatectl is `timedatectl show -p NTPSynchronized` (and `timedatectl show`'s NTP fields),
// with no systemd: synchronised when chrony's tracking reports a normal leap status.
func Timedatectl(args []string) int {
	out, err := execOutput("chronyc", "-n", "tracking")
	if err != nil {
		// SONiC keeps time with ntpd: synchronised once it has a system peer (ntpq's '*'), which
		// reads as chrony's normal leap status
		if peers, perr := execOutput("ntpq", "-pn"); perr == nil {
			out, err = "Leap status     : Not synchronised\n", nil
			for _, l := range strings.Split(peers, "\n") {
				if strings.HasPrefix(strings.TrimSpace(l), "*") {
					out = "Leap status     : Normal\n"
				}
			}
		}
	}
	return timedatectl(args, out, err, os.Stdout, os.Stderr)
}

func timedatectl(args []string, tracking string, terr error, stdout, stderr io.Writer) int {
	synced := "no"
	if terr == nil {
		for _, l := range strings.Split(tracking, "\n") {
			if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "Leap status" && strings.TrimSpace(v) == "Normal" {
				synced = "yes"
			}
		}
	}
	props := map[string]string{"NTPSynchronized": synced, "NTP": "yes", "CanNTP": "yes", "Timezone": timezone()}
	if len(args) == 0 || args[0] != "show" {
		fmt.Fprintln(stderr, "timedatectl: only `timedatectl show [-p <property>] [--value]` is emulated by the twin")
		return 2
	}
	var want []string
	value := false
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "-p" || args[i] == "--property":
			if i+1 < len(args) {
				i++
				want = append(want, strings.Split(args[i], ",")...)
			}
		case strings.HasPrefix(args[i], "--property="):
			want = append(want, strings.Split(strings.TrimPrefix(args[i], "--property="), ",")...)
		case args[i] == "--value":
			value = true
		default:
			fmt.Fprintf(stderr, "timedatectl show: %s is not emulated\n", args[i])
			return 2
		}
	}
	if len(want) == 0 {
		want = []string{"Timezone", "NTP", "CanNTP", "NTPSynchronized"}
	}
	for _, p := range want {
		v, ok := props[p]
		if !ok {
			continue
		}
		if value {
			fmt.Fprintln(stdout, v)
		} else {
			fmt.Fprintf(stdout, "%s=%s\n", p, v)
		}
	}
	return 0
}

func timezone() string {
	if b, err := os.ReadFile("/etc/timezone"); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b))
	}
	return "Etc/UTC"
}

func execOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	b, err := cmd.Output()
	return string(b), err
}

package inhost

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// The agent's protocol. Both halves live in this package: GuestAgent is the guest's half, a
// busybox shell function the vm adapter's /init runs (internal/adapter/vm/guest.go splices it in),
// and the adapter in inhost.go is the host's half. They speak in lines on the guest's serial
// console, each marked with Marker, because the console is the one channel into an `os: twin`
// guest the substrate gives on every host: QEMU on macOS has no vsock device (vhost-vsock is
// Linux's), and the twin's own channel must never be a network the stack can see (D10). A
// console line is visible to whoever reads the console, so the guest sends nothing until the
// twin attaches, and what it sends is the guest's kernel log, which the console shows anyway.
//
// Guest to twin:
//
//	@@dc-twin listening                                  (through /dev/kmsg, as "dc-twin guest: in-host agent listening")
//	@@dc-twin hello boot=<boot_id> kernel=<release> arch=<machine> cpus=<n> memKiB=<n> kmod=<loaded|refused|absent>
//	@@dc-twin alive boot=<boot_id>                       (every HeartbeatEvery)
//	@@dc-twin kmsg <base64 of "<prio>,<seq>,<usec>,<flags>;<text>">  (each /dev/kmsg record, as the kernel gives it)
//
// Twin to guest:
//
//	@@dc-twin attach                                     (answered by hello; the first starts the kmsg stream)
//	@@dc-twin kmsg-write <N>text                         (one kernel record at level N: printk'd by the agent's
//	                                                      kernel module when it is loaded, else written to /dev/kmsg)
const (
	Marker = "@@dc-twin "
	// Listening is the line the agent's start puts in the kernel log, and so on the console: the
	// twin attaches when it sees it, never earlier, because bytes sent to the console while the
	// firmware owns it are keystrokes to the firmware.
	Listening = "dc-twin guest: in-host agent listening"
	// HeartbeatEvery is how often an attached agent says it is alive.
	HeartbeatEvery = 5
	// KernelModule is the agent's kernel half (kmod/dctwin_kmsg.c, built by kmod/build.sh for a
	// guest image's kernel and put beside the image); the vm adapter adds it to the guest's
	// initramfs at /KernelModule when it is there, and the agent loads it at start.
	KernelModule = "dctwin-kmsg.ko"
	// KernelModuleFile is the file the module exposes: a line written to it is printk'd, so its
	// record has facility kern, as a driver's has (D50).
	KernelModuleFile = "/proc/dctwin-kmsg"
)

// GuestAgent is the guest's half of the agent: a shell function for busybox ash, started in the
// background by the twin's /init, reading the twin's lines from the kernel's console tty (the
// first of /sys/class/tty/console/active, the console= the twin boots the guest with) and writing
// its own to it. It is all busybox applets that Alpine's netboot initramfs carries (stty,
// cat, grep, awk, printf, base64, tr). Reading /dev/kmsg goes through cat, because ash's read takes one byte
// at a time and /dev/kmsg refuses a read too small for its record (EINVAL); cat is restarted when
// the kernel reports records lost (EPIPE), and the twin drops what it has already seen by
// sequence number, as journald does with its kernel-seqnum file. A record travels base64-encoded:
// the vm adapter's boot observer reads the same console for the kernel's banner and the guest's
// ready line, and the boot log carried again in plain text would read to it as a second boot.
// A fault's line goes through the agent's kernel module when the guest image carries one and the
// kernel accepts it (kmod=loaded), so that its record is a kernel record; otherwise it is written
// to /dev/kmsg (kmod=absent or refused), where the kernel gives it facility user, and the twin
// says so in the journal entry it makes of it.
func GuestAgent() string {
	return strings.NewReplacer("@HB@", strconv.Itoa(HeartbeatEvery), "@M@", strings.TrimSpace(Marker),
		"@KMOD@", KernelModule, "@KFILE@", KernelModuleFile).Replace(guestAgent)
}

const guestAgent = `# the in-host agent (internal/inhost): the twin's lines arrive on the serial console, marked @M@;
# nothing is sent until the twin attaches
inhost_agent() {
	t=/dev/$(awk '{print $1; exit}' /sys/class/tty/console/active 2>/dev/null)
	[ -c "$t" ] || t=/dev/console
	stty -F $t -echo -isig -ixon -icrnl 2>/dev/null
	boot=$(cat /proc/sys/kernel/random/boot_id)
	kmsg=/dev/kmsg kmod=absent
	if [ -e /@KMOD@ ]; then
		# kmod's insmod (busybox may carry none); the kernel checks vermagic and symbol versions
		command -v kmod >/dev/null && ln -sf "$(command -v kmod)" /sbin/insmod 2>/dev/null
		insmod /@KMOD@ 2>/dev/null
		if [ -w @KFILE@ ]; then kmsg=@KFILE@ kmod=loaded; else kmod=refused; fi
	fi
	frame() { printf '@M@ %s\n' "$*" > $t; }
	attached=
	echo "dc-twin guest: in-host agent listening on $t" > /dev/kmsg
	while IFS= read -r line; do
		line=$(printf '%s' "$line" | tr -d '\r')
		case "$line" in
		"@M@ attach")
			frame "hello boot=$boot kernel=$(uname -r) arch=$(uname -m) cpus=$(grep -c ^processor /proc/cpuinfo) memKiB=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo) kmod=$kmod"
			if [ -z "$attached" ]; then
				attached=1
				(while :; do cat /dev/kmsg; sleep 1; done) | while IFS= read -r rec; do
					case "$rec" in " "*) ;; *) frame "kmsg $(printf '%s' "$rec" | base64 | tr -d '\n')" ;; esac
				done &
				(while :; do sleep @HB@; frame "alive boot=$boot"; done) &
			fi ;;
		"@M@ kmsg-write "*)
			printf '%s\n' "${line#@M@ kmsg-write }" > $kmsg ;;
		esac
	done < $t
}
inhost_agent &
`

// Frame is one line the guest's agent sent.
type Frame struct {
	Kind   string            // hello, alive, kmsg
	Fields map[string]string // hello and alive: key=value pairs
	Record string            // kmsg: the record as /dev/kmsg gave it, decoded
}

// ParseFrame finds the agent's frame in a console line. A kernel message the console printed
// while the agent was writing can precede the marker on the same line, so the marker is looked
// for anywhere; a line without one is the guest's ordinary console output.
func ParseFrame(line string) (Frame, bool) {
	i := strings.Index(line, Marker)
	if i < 0 {
		return Frame{}, false
	}
	rest := strings.TrimRight(line[i+len(Marker):], "\r\n")
	kind, body, _ := strings.Cut(rest, " ")
	switch kind {
	case "kmsg":
		rec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(body))
		if err != nil || len(rec) == 0 {
			return Frame{}, false
		}
		return Frame{Kind: kind, Record: string(rec)}, true
	case "hello", "alive":
		f := Frame{Kind: kind, Fields: map[string]string{}}
		for _, kv := range strings.Fields(body) {
			if k, v, ok := strings.Cut(kv, "="); ok {
				f.Fields[k] = v
			}
		}
		return f, true
	}
	return Frame{}, false // the twin's own lines echoed back, or something this agent does not send
}

// AttachLine and KmsgWriteLine are the twin's lines to the guest.
func AttachLine() string { return Marker + "attach\n" }

// KmsgWriteLine asks the agent to put text in the kernel log as one record at a syslog level
// (0–7). The text is made one printable ASCII line: the console is a tty, and both the module's
// file and /dev/kmsg take a record per write.
func KmsgWriteLine(level int, text string) string {
	if level < 0 || level > 7 {
		level = 4
	}
	return fmt.Sprintf("%skmsg-write <%d>%s\n", Marker, level, Printable(text, 900))
}

// Printable reduces s to printable ASCII on one line, at most n bytes.
func Printable(s string, n int) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < n; i++ {
		c := s[i]
		if c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
		} else if c == '\t' {
			b.WriteByte(' ')
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

package inhost

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Record is one kernel log record as /dev/kmsg gives it (Documentation/ABI/testing/dev-kmsg):
// "<prio>,<seq>,<usec>,<flags>[,…];<text>", prio being facility<<3 | level and usec the kernel's
// monotonic timestamp. Continuation lines (" KEY=value", the record's device properties) are not
// carried by the agent.
type Record struct {
	Prio  int
	Seq   uint64
	Usec  uint64
	Flags string
	Text  string // as the kernel escaped it: non-printable bytes are \xNN
}

// Level and Facility split the record's syslog priority.
func (r Record) Level() int    { return r.Prio & 7 }
func (r Record) Facility() int { return r.Prio >> 3 }

// ParseRecord parses one /dev/kmsg record.
func ParseRecord(s string) (Record, error) {
	head, text, ok := strings.Cut(s, ";")
	if !ok {
		return Record{}, fmt.Errorf("kmsg record without ';': %q", s)
	}
	f := strings.Split(head, ",")
	if len(f) < 3 {
		return Record{}, fmt.Errorf("kmsg record header %q", head)
	}
	var r Record
	var err error
	if r.Prio, err = strconv.Atoi(f[0]); err != nil || r.Prio < 0 || r.Prio > 999 {
		return Record{}, fmt.Errorf("kmsg priority %q", f[0])
	}
	if r.Seq, err = strconv.ParseUint(f[1], 10, 64); err != nil {
		return Record{}, fmt.Errorf("kmsg sequence %q", f[1])
	}
	if r.Usec, err = strconv.ParseUint(f[2], 10, 64); err != nil {
		return Record{}, fmt.Errorf("kmsg timestamp %q", f[2])
	}
	if len(f) > 3 {
		r.Flags = f[3]
	}
	r.Text = strings.TrimRight(text, "\r\n")
	return r, nil
}

// Entry is a journal entry's fields, in the order they are written.
type Entry struct {
	Realtime  time.Time
	Monotonic uint64 // the guest's CLOCK_MONOTONIC in µs, paired with BootID
	BootID    string // 32 hex digits, as journald writes _BOOT_ID
	Fields    [][2]string
}

// JournalEntry maps a kernel record to the journal entry journald writes for it when it reads
// /dev/kmsg (systemd src/journal/journald-kmsg.c, dev_kmsg_record): _SOURCE_BOOTTIME_TIMESTAMP and
// _SOURCE_MONOTONIC_TIMESTAMP from the record's timestamp, _TRANSPORT=kernel, PRIORITY and
// SYSLOG_FACILITY from its priority, SYSLOG_IDENTIFIER=kernel for the kernel facility and
// otherwise the "identifier[pid]:" a userspace writer put first (syslog_parse_identifier), the
// message with the kernel's \xNN escapes undone, and the fields every entry gets
// (server_dispatch_message): _BOOT_ID, _HOSTNAME and _RUNTIME_SCOPE=system.
//
// The twin's guest runs no journald (it is Alpine's netboot initramfs), so this mapping stands in
// for it. A fault's line printk'd by the in-host agent's kernel module (dctwin-kmsg) is a kernel
// record, facility kern, and is mapped exactly as above, as journald maps a driver's printk. Only
// where the module could not be loaded is the line written through /dev/kmsg, where the kernel
// marks it LOG_USER ("to be able to reliably distinguish kernel-generated messages from
// userspace-injected ones", printk.c devkmsg_write), and journald would then take a driver line's
// "NVRM:" for a syslog identifier and strip it from MESSAGE. Such a record (asKernel) is entered as
// a driver's printk would be (facility kern, SYSLOG_IDENTIFIER=kernel, the message whole), and
// carries DCTWIN_FAULT and DCTWIN_FAULT_ID and the facility the kernel gave it as
// DCTWIN_KMSG_FACILITY, so nobody reading the journal mistakes it for the driver's own (D50).
// _KERNEL_* device properties and _UDEV_* fields are not carried.
func JournalEntry(r Record, bootID, hostname string, received time.Time, asKernel bool, fault, faultID string) Entry {
	e := Entry{Realtime: received, Monotonic: r.Usec, BootID: strings.ReplaceAll(bootID, "-", "")}
	add := func(k, v string) { e.Fields = append(e.Fields, [2]string{k, v}) }
	usec := strconv.FormatUint(r.Usec, 10)
	add("_SOURCE_BOOTTIME_TIMESTAMP", usec)
	add("_SOURCE_MONOTONIC_TIMESTAMP", usec)
	add("_TRANSPORT", "kernel")
	add("PRIORITY", strconv.Itoa(r.Level()))
	text := r.Text
	if asKernel || r.Facility() == 0 {
		add("SYSLOG_FACILITY", "0")
		add("SYSLOG_IDENTIFIER", "kernel")
	} else {
		add("SYSLOG_FACILITY", strconv.Itoa(r.Facility()))
		id, pid, rest := syslogIdentifier(text)
		if id != "" {
			add("SYSLOG_IDENTIFIER", id)
		}
		if pid != "" {
			add("SYSLOG_PID", pid)
		}
		text = rest
	}
	add("MESSAGE", unescape(text))
	if asKernel {
		add("DCTWIN_FAULT", fault)
		add("DCTWIN_FAULT_ID", faultID)
		add("DCTWIN_KMSG_FACILITY", strconv.Itoa(r.Facility()))
	}
	add("_BOOT_ID", e.BootID)
	add("_HOSTNAME", hostname)
	add("_RUNTIME_SCOPE", "system")
	return e
}

// syslogIdentifier is journald's syslog_parse_identifier: a first word ending in ':' is the
// identifier, with "[pid]" before the colon taken as its pid, and one space after it skipped.
func syslogIdentifier(s string) (id, pid, rest string) {
	p := strings.TrimLeft(s, " \t\n")
	l := strings.IndexAny(p, " \t\n")
	if l < 0 {
		l = len(p)
	}
	if l == 0 || p[l-1] != ':' {
		return "", "", s
	}
	word := p[:l-1]
	if strings.HasSuffix(word, "]") {
		if k := strings.LastIndexByte(word, '['); k >= 0 {
			pid, word = word[k+1:len(word)-1], word[:k]
		}
	}
	rest = p[l:]
	if rest != "" && strings.ContainsRune(" \t\n", rune(rest[0])) {
		rest = rest[1:]
	}
	return word, pid, rest
}

// unescape undoes the kernel's \xNN escapes of non-printable bytes (journald's cunescape with
// UNESCAPE_RELAX: anything that is not a valid escape stays as it is).
func unescape(s string) string {
	if !strings.Contains(s, `\x`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Export writes the entry in the journal export format (systemd's JOURNAL_EXPORT_FORMATS),
// which systemd-journal-remote reads from a stream and writes into a journal file:
// __REALTIME_TIMESTAMP, __MONOTONIC_TIMESTAMP and _BOOT_ID address the entry, then one
// "FIELD=value" line per field, a value holding a newline in the binary form (the name, a newline,
// the length as a little-endian 64-bit integer, the value, a newline), and an empty line ending it.
func (e Entry) Export() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "__REALTIME_TIMESTAMP=%d\n", e.Realtime.UnixMicro())
	fmt.Fprintf(&b, "__MONOTONIC_TIMESTAMP=%d\n", e.Monotonic)
	for _, f := range e.Fields {
		if strings.ContainsAny(f[1], "\n") {
			b.WriteString(f[0])
			b.WriteByte('\n')
			var n [8]byte
			binary.LittleEndian.PutUint64(n[:], uint64(len(f[1])))
			b.Write(n[:])
			b.WriteString(f[1])
			b.WriteByte('\n')
			continue
		}
		b.WriteString(f[0])
		b.WriteByte('=')
		b.WriteString(f[1])
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.Bytes()
}

// Field returns the value of a field ("" when absent).
func (e Entry) Field(k string) string {
	for _, f := range e.Fields {
		if f[0] == k {
			return f[1]
		}
	}
	return ""
}

// render fills a fault catalogue line's placeholders as the engine fills the catalogue's other
// effects (internal/engine/faults.go subst): the fault's parameters, {component}, {index} and
// {pci}, the accelerator's PCI address in the bus numbering every projection of the model uses
// (the dcgm-exporter's pci_bus_id follows the same rule), {pcidev}, that address without its
// function as the NVIDIA driver prints it in Xid lines ("PCI:0000:38:00"), and {uuid}, the GPU's
// UUID as NVML, dcgm-exporter and Mokka report it ("GPU-…").
func render(line string, params map[string]any, component string, index int, uuid string) string {
	return paramRe.ReplaceAllStringFunc(line, func(m string) string {
		k := m[1 : len(m)-1]
		if v, ok := params[k]; ok {
			return fmt.Sprint(v)
		}
		switch k {
		case "component":
			if component != "" {
				return component
			}
		case "index":
			if component != "" {
				return strconv.Itoa(index)
			}
		case "pci":
			if component != "" {
				return PCIAddress(index)
			}
		case "pcidev":
			if component != "" {
				return strings.TrimSuffix(PCIAddress(index), ".0")
			}
		case "uuid":
			if uuid != "" {
				return uuid
			}
		}
		return m
	})
}

// PCIAddress is an accelerator's PCI address by its index in the model's bus numbering.
func PCIAddress(index int) string { return fmt.Sprintf("0000:%02x:00.0", 0x18+index*0x10) }

// paramRe is the catalogue's placeholder syntax, as the engine reads it.
var paramRe = regexp.MustCompile(`\{([a-zA-Z]+)\}`)

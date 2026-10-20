package bluefield

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// What the Arm boots, taken from where a BlueField takes it, without changing it.
//
// The installer: the boot stream the BMC received (saved as it streamed), split the way the
// BlueField's boot path splits it. Its Linux image (id 62, LZMA-compressed by mlx-mkbfb) is
// decompressed with xz, because the twin's QEMU kernel loader takes an arm64 Image, not the
// compressed form the BlueField's UEFI unpacks; its initramfs (id 63) is passed as it came; its
// boot arguments (id 59) are the kernel command line; and what followed the last image (the bf.cfg
// DPF appended) goes into the installer's root as /etc/bf.cfg, where the installer sources it:
// a second, uncompressed cpio archive after the initramfs, which Linux unpacks over the first. On
// a BlueField the boot FIFO carries the rest of the stream to the installer; QEMU has no boot FIFO,
// and this archive is its stand-in.
//
// The installed DPU OS: GRUB's default entry (the first menuentry of /boot/grub/grub.cfg, the
// installer's grub-set-default 0), its kernel and initrd and its command line verbatim, read from
// the disk's root file system with debugfs (e2fsprogs), without mounting it.

// bfbParts is a saved boot stream split for the installer.
type bfbParts struct {
	kernel string // a decompressed arm64 Image
	initrd string // the BFB's initramfs, then the bf.cfg archive
	args   string // the stream's boot arguments (id 59), "" when it carries none
	cfg    int    // bytes of bf.cfg handed to the installer (0: none)
}

// maxCfg bounds what is taken as a bf.cfg after the last image: DPF's are about 16 KB.
const maxCfg = 4 << 20

// splitBFB writes the installer's kernel and initrd from a saved boot stream into dir.
func splitBFB(stream, dir string) (*bfbParts, error) {
	in, err := os.Open(stream)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	r := bufio.NewReaderSize(in, 1<<20)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	parts := &bfbParts{initrd: filepath.Join(dir, "installer-initrd.bin")}
	packed := filepath.Join(dir, "image.packed")
	var haveKernel, haveInitrd bool
	hdr := make([]byte, 24)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			return nil, fmt.Errorf("boot stream: %w", err)
		}
		w0 := binary.LittleEndian.Uint64(hdr[0:8])
		w1 := binary.LittleEndian.Uint64(hdr[8:16])
		follow := binary.LittleEndian.Uint64(hdr[16:24])
		if uint32(w0) != bfbMagic {
			return nil, errNotBFB
		}
		if hl := int((w0>>52)&0xF) * 8; hl > 24 {
			if _, err := io.CopyN(io.Discard, r, int64(hl-24)); err != nil {
				return nil, err
			}
		}
		id := int((w0 >> 56) & 0xFF)
		length := int64(uint32(w1))
		pad := ((length + 7) &^ 7) - length
		switch id {
		case 62, 63:
			name := packed
			if id == 63 {
				name = parts.initrd
			}
			if err := copyTo(name, io.LimitReader(r, length), length); err != nil {
				return nil, fmt.Errorf("image %d: %w", id, err)
			}
			haveKernel, haveInitrd = haveKernel || id == 62, haveInitrd || id == 63
		case 59:
			b := make([]byte, length)
			if _, err := io.ReadFull(r, b); err != nil {
				return nil, err
			}
			parts.args = strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
		default:
			if _, err := io.CopyN(io.Discard, r, length); err != nil {
				return nil, err
			}
		}
		if _, err := io.CopyN(io.Discard, r, pad); err != nil {
			return nil, err
		}
		if follow == 0 {
			break
		}
	}
	if !haveKernel || !haveInitrd {
		return nil, errors.New("the boot stream carries no Linux image and initramfs (ids 62, 63)")
	}
	cfg, err := io.ReadAll(io.LimitReader(r, maxCfg+1))
	if err != nil {
		return nil, err
	}
	if len(cfg) > maxCfg {
		return nil, fmt.Errorf("%d bytes after the last image: more than a bf.cfg", len(cfg))
	}
	if len(bytes.TrimSpace(cfg)) > 0 {
		f, err := os.OpenFile(parts.initrd, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return nil, err
		}
		st, _ := f.Stat()
		// an uncompressed archive after a compressed one starts on a 4-byte boundary
		if r := st.Size() % 4; r != 0 {
			_, _ = f.Write(make([]byte, 4-r))
		}
		_, err = f.Write(cpioNewc([]cpioEntry{{name: "etc", dir: true}, {name: "etc/bf.cfg", data: cfg, mode: 0o644}}))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		parts.cfg = len(cfg)
	}
	parts.kernel = filepath.Join(dir, "Image")
	if err := unpackKernel(packed, parts.kernel); err != nil {
		return nil, err
	}
	_ = os.Remove(packed)
	return parts, nil
}

func copyTo(path string, r io.Reader, n int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	k, err := io.Copy(w, r)
	if err == nil && k != n {
		err = io.ErrUnexpectedEOF
	}
	if ferr := w.Flush(); err == nil {
		err = ferr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// unpackKernel writes an arm64 Image from a kernel as a BFB or /boot carries it: already an Image
// ("ARM\x64" at offset 56), gzip (Ubuntu's arm64 vmlinuz), or LZMA (mlx-mkbfb's image 62, through
// `xz --format=lzma`, since the twin has no LZMA decoder of its own).
func unpackKernel(src, dst string) error {
	head := make([]byte, 64)
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	n, _ := io.ReadFull(f, head)
	f.Close()
	head = head[:n]
	switch {
	case n >= 60 && string(head[56:60]) == "ARM\x64":
		return os.Rename(src, dst)
	case n >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		zr, err := gzip.NewReader(in)
		if err != nil {
			return err
		}
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, zr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	default:
		xz, err := exec.LookPath("xz")
		if err != nil {
			return errors.New("the BFB's Linux image is LZMA-compressed and xz is not installed")
		}
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		cmd := exec.Command(xz, "--format=lzma", "--decompress", "--stdout", src)
		cmd.Stdout = out
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err = cmd.Run()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("decompressing the Linux image: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
}

// cpioEntry is one member of a newc archive.
type cpioEntry struct {
	name string
	data []byte
	mode uint32
	dir  bool
}

// cpioNewc is a "newc" (070701) archive of the entries, owned by root, with its trailer.
func cpioNewc(entries []cpioEntry) []byte {
	var b bytes.Buffer
	pad := func() {
		for b.Len()%4 != 0 {
			b.WriteByte(0)
		}
	}
	write := func(ino int, name string, mode uint32, nlink int, data []byte) {
		fmt.Fprintf(&b, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
			ino, mode, 0, 0, nlink, 0, len(data), 0, 0, 0, 0, len(name)+1, 0)
		b.WriteString(name)
		b.WriteByte(0)
		pad()
		b.Write(data)
		pad()
	}
	for i, e := range entries {
		if e.dir {
			write(i+1, e.name, 0o040755, 2, nil)
			continue
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		write(i+1, e.name, 0o100000|mode, 1, e.data)
	}
	write(0, "TRAILER!!!", 0, 1, nil)
	return b.Bytes()
}

// rootPartition is the byte offset of a raw disk's first Linux file system partition (GPT type
// 0FC63DAF-8483-4772-8E79-3D69D8477DE4), the installer's root.
func rootPartition(disk string) (int64, error) {
	f, err := os.Open(disk)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	b := make([]byte, 512*64)
	if _, err := io.ReadFull(f, b); err != nil {
		return 0, err
	}
	if string(b[512:520]) != "EFI PART" {
		return 0, errors.New("the disk has no GPT: nothing is installed")
	}
	lba := binary.LittleEndian.Uint64(b[512+72:])
	n := binary.LittleEndian.Uint32(b[512+80:])
	size := binary.LittleEndian.Uint32(b[512+84:])
	linux := []byte{0xaf, 0x3d, 0xc6, 0x0f, 0x83, 0x84, 0x72, 0x47, 0x8e, 0x79, 0x3d, 0x69, 0xd8, 0x47, 0x7d, 0xe4}
	for i := uint32(0); i < n; i++ {
		o := int64(lba)*512 + int64(i)*int64(size)
		if o+int64(size) > int64(len(b)) {
			break
		}
		if bytes.Equal(b[o:o+16], linux) {
			return int64(binary.LittleEndian.Uint64(b[o+32:])) * 512, nil
		}
	}
	return 0, errors.New("the disk has no Linux file system partition")
}

// debugfsPath finds e2fsprogs' debugfs, which is in sbin on most distributions.
func debugfsPath() (string, error) {
	if p, err := exec.LookPath("debugfs"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/usr/sbin/debugfs", "/sbin/debugfs", "/opt/homebrew/opt/e2fsprogs/sbin/debugfs", "/usr/local/opt/e2fsprogs/sbin/debugfs"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("debugfs (e2fsprogs) is not installed: the twin reads the installed DPU OS's boot entry with it")
}

// debugfs runs requests against the file system at offset in a raw disk.
func debugfs(disk string, offset int64, write bool, requests ...string) (string, error) {
	bin, err := debugfsPath()
	if err != nil {
		return "", err
	}
	script, err := os.CreateTemp("", "dctwin-debugfs-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(script.Name())
	_, _ = script.WriteString(strings.Join(requests, "\n") + "\n")
	script.Close()
	args := []string{"-f", script.Name()}
	if write {
		args = append([]string{"-w"}, args...)
	}
	args = append(args, fmt.Sprintf("%s?offset=%d", disk, offset))
	var out, stderr bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("debugfs: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

// grubVar matches a GRUB variable reference on a linux line ($vt_handoff, ${extra}).
var grubVar = regexp.MustCompile(`\$\{?[A-Za-z_][A-Za-z0-9_]*\}?`)

// grubDefault is the kernel, initrd and command line of grub.cfg's first menuentry, the entry
// GRUB boots by default.
func grubDefault(cfg string) (kernel, initrd, cmdline string, err error) {
	depth, inEntry := 0, false
	for _, line := range strings.Split(cfg, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "menuentry ") && depth == 0 {
			inEntry = true
		}
		if inEntry {
			f := strings.Fields(t)
			switch {
			case len(f) >= 2 && (f[0] == "linux" || f[0] == "linuxefi") && kernel == "":
				kernel = f[1]
				cmdline = strings.Join(strings.Fields(grubVar.ReplaceAllString(strings.Join(f[2:], " "), "")), " ")
			case len(f) >= 2 && (f[0] == "initrd" || f[0] == "initrdefi") && initrd == "":
				initrd = f[1]
			}
		}
		depth += strings.Count(t, "{") - strings.Count(t, "}")
		if inEntry && depth == 0 && strings.Contains(t, "}") {
			break
		}
	}
	if kernel == "" {
		return "", "", "", errors.New("grub.cfg has no menuentry with a linux line")
	}
	return kernel, initrd, cmdline, nil
}

// installedBoot reads GRUB's default entry from an installed disk and writes its kernel (as an
// Image) and initrd into dir.
func installedBoot(disk, dir string) (kernel, initrd, cmdline string, err error) {
	off, err := rootPartition(disk)
	if err != nil {
		return "", "", "", err
	}
	cfg, err := debugfs(disk, off, false, "cat /boot/grub/grub.cfg")
	if err != nil {
		return "", "", "", err
	}
	k, i, cmdline, err := grubDefault(cfg)
	if err != nil {
		return "", "", "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", "", err
	}
	packed, kernel, initrd := filepath.Join(dir, "vmlinuz"), filepath.Join(dir, "Image"), filepath.Join(dir, "initrd.img")
	_ = os.Remove(packed)
	_ = os.Remove(initrd)
	reqs := []string{fmt.Sprintf("dump %s %s", k, packed)}
	if i != "" {
		reqs = append(reqs, fmt.Sprintf("dump %s %s", i, initrd))
	}
	if _, err := debugfs(disk, off, false, reqs...); err != nil {
		return "", "", "", err
	}
	if st, err := os.Stat(packed); err != nil || st.Size() == 0 {
		return "", "", "", fmt.Errorf("grub.cfg names %s, which is not on the root file system", k)
	}
	if err := unpackKernel(packed, kernel); err != nil {
		return "", "", "", err
	}
	if i == "" {
		initrd = ""
	} else if st, err := os.Stat(initrd); err != nil || st.Size() == 0 {
		return "", "", "", fmt.Errorf("grub.cfg names %s, which is not on the root file system", i)
	}
	return kernel, initrd, cmdline, nil
}

// fsClean reports whether the root file system was cleanly unmounted (no journal to recover), so
// that debugfs may write to it.
func fsClean(disk string, offset int64) (bool, error) {
	out, err := debugfs(disk, offset, false, "stats -h")
	if err != nil {
		return false, err
	}
	feat := ""
	state := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "Filesystem features:") {
			feat = l
		}
		if strings.HasPrefix(l, "Filesystem state:") {
			state = l
		}
	}
	return strings.Contains(state, "clean") && !strings.Contains(feat, "needs_recovery"), nil
}

// oobLinkName is the systemd link file the twin adds to an installed DPU OS (arm.go).
const oobLinkName = "/etc/systemd/network/10-dctwin-oob_net0.link"

// writeOOBLink names the NIC with the Arm's OOB MAC oob_net0 in an installed DPU OS: a systemd
// .link file matching the MAC, written into the root file system with debugfs while the disk is
// at rest and clean.
func writeOOBLink(disk, dir, mac string) error {
	off, err := rootPartition(disk)
	if err != nil {
		return err
	}
	if ok, err := fsClean(disk, off); err != nil {
		return err
	} else if !ok {
		return errors.New("the installed root file system was not cleanly unmounted; the twin does not write to it")
	}
	local := filepath.Join(dir, "oob_net0.link")
	body := "# Written by dc-twin: the BlueField's mlxbf_gige driver names its out-of-band port oob_net0;\n" +
		"# this machine's out-of-band port is a virtio NIC, named here by its MAC.\n" +
		"[Match]\nMACAddress=" + mac + "\n\n[Link]\nName=oob_net0\n"
	if err := os.WriteFile(local, []byte(body), 0o644); err != nil {
		return err
	}
	d, base := filepath.Dir(oobLinkName), filepath.Base(oobLinkName)
	if _, err := debugfs(disk, off, true, "cd "+d, "rm "+base, "write "+local+" "+base,
		"sif "+base+" mode 0100644", "sif "+base+" uid 0", "sif "+base+" gid 0"); err != nil {
		return err
	}
	out, err := debugfs(disk, off, false, "cat "+oobLinkName)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "Name=oob_net0") {
		return errors.New("the link file did not land on the root file system")
	}
	return nil
}

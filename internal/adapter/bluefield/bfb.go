package bluefield

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
	"strings"
)

// A BFB is a BlueField boot stream: the images a BlueField's boot ROM, ATF and UEFI take from
// rshim (boot loaders, certificates, the UEFI capsule, boot arguments, an `info` inventory, the
// Linux Image and the installer initramfs), each behind a 24-byte header that NVIDIA's
// mlx-mkbfb (Mellanox/bfscripts) writes:
//
//	word 0: magic 0x13026642 (low 32 bits) | major:4 @32 | minor:4 @36 | next_ver:4 @44 |
//	        cur_ver:4 @48 | header length / 8 :4 @52 | image id:8 @56
//	word 1: image length (low 32 bits) | CRC-32 of the data and its padding (high 32 bits)
//	word 2: bitmap of the image ids that follow (0 on the last image)
//
// The data is padded to a multiple of eight bytes. What follows the last image is not part of
// the stream: DPF's bfb-registry appends the DPU's bf.cfg there (nginx's concat module), which the
// installer reads. The BMC streams the whole of it to the DPU over rshim; the twin reads it as it
// arrives and checks every image's CRC, which is all a transfer can be checked for.
const bfbMagic = 0x13026642

var bfbImageNames = map[int]string{36: "psc-bl", 37: "psc-fw", 31: "bl2r-cert", 30: "bl2r", 42: "bl2r-auth-cert",
	6: "bl2-cert", 1: "bl2", 29: "sys", 43: "bl2-auth-cert", 38: "ddr-cert", 32: "ddr_ini", 33: "snps_images",
	40: "ddr5_snps_images", 34: "ddr_ate_imem", 35: "ddr_ate_dmem", 8: "bl30-key-cert", 12: "bl30-cert", 2: "bl30",
	39: "psc-app", 53: "psc-certs", 7: "trusted-key-cert", 9: "bl31-key-cert", 13: "bl31-cert", 3: "bl31",
	10: "bl32-key-cert", 14: "bl32-cert", 4: "bl32", 11: "bl33-key-cert", 15: "bl33-cert", 5: "bl33", 52: "capsule",
	55: "boot-acpi", 56: "boot-dtb", 57: "boot-desc", 58: "boot-path", 59: "boot-args", 60: "boot-timeout",
	61: "uefi-tests", 41: "nicfw", 54: "ramdisk", 49: "dpu-gi", 48: "cfg-gi", 50: "info", 62: "image", 63: "initramfs",
	51: "upgrade-image"}

// bfbFacts is what a pushed boot stream carried.
type bfbFacts struct {
	images   int
	bytes    int64
	byName   map[string]int64 // image name -> bytes
	versions map[string]string
	cfgBytes int64  // what followed the last image (the appended bf.cfg)
	cfgSHA   string // its digest: the twin records that a bf.cfg came, never its content (it carries join credentials)
}

func (b *bfbFacts) summary() string {
	if b == nil {
		return ""
	}
	var vs []string
	for k, v := range b.versions {
		vs = append(vs, k+" "+v)
	}
	sort.Strings(vs)
	s := fmt.Sprintf("%d images, %d bytes, Linux image %d bytes, initramfs %d bytes", b.images, b.bytes, b.byName["image"], b.byName["initramfs"])
	if len(vs) > 0 {
		s += "; " + strings.Join(vs, ", ")
	}
	if b.cfgBytes > 0 {
		s += fmt.Sprintf("; bf.cfg appended (%d bytes, sha256:%s)", b.cfgBytes, b.cfgSHA[:16])
	}
	return s
}

var errNotBFB = errors.New("not a BlueField boot stream (no BFB image header at its start)")

// readBFB consumes a boot stream from r, checking each image. progress is called with the bytes
// read so far.
func readBFB(r io.Reader, progress func(int64)) (*bfbFacts, error) {
	facts := &bfbFacts{byName: map[string]int64{}, versions: map[string]string{}}
	var n int64
	read := func(p []byte) error {
		k, err := io.ReadFull(r, p)
		n += int64(k)
		if progress != nil {
			progress(n)
		}
		return err
	}
	buf := make([]byte, 1<<20)
	hdr := make([]byte, 24)
	for {
		if err := read(hdr); err != nil {
			if facts.images == 0 {
				return nil, errNotBFB
			}
			return nil, fmt.Errorf("boot stream truncated after %d images: %w", facts.images, err)
		}
		w0 := binary.LittleEndian.Uint64(hdr[0:8])
		w1 := binary.LittleEndian.Uint64(hdr[8:16])
		follow := binary.LittleEndian.Uint64(hdr[16:24])
		if uint32(w0) != bfbMagic {
			if facts.images == 0 {
				return nil, errNotBFB
			}
			return nil, fmt.Errorf("image %d: no BFB header at offset %d", facts.images, n-24)
		}
		hl := int((w0>>52)&0xF) * 8
		id := int((w0 >> 56) & 0xFF)
		length := int64(uint32(w1))
		want := uint32(w1 >> 32)
		if hl > 24 {
			if err := read(make([]byte, hl-24)); err != nil {
				return nil, fmt.Errorf("image %d: truncated header: %w", facts.images, err)
			}
		}
		name := bfbImageNames[id]
		if name == "" {
			name = fmt.Sprintf("id-%d", id)
		}
		padded := (length + 7) &^ 7
		crc := uint32(0)
		var info []byte
		for rem := padded; rem > 0; {
			k := int64(len(buf))
			if rem < k {
				k = rem
			}
			if err := read(buf[:k]); err != nil {
				return nil, fmt.Errorf("image %d (%s): truncated after %d of %d bytes: %w", facts.images, name, padded-rem, length, err)
			}
			crc = crc32.Update(crc, crc32.IEEETable, buf[:k])
			if id == 50 && len(info) < 1<<20 {
				info = append(info, buf[:k]...)
			}
			rem -= k
		}
		if crc != want {
			return nil, fmt.Errorf("image %d (%s): CRC %08x, header says %08x", facts.images, name, crc, want)
		}
		facts.images++
		facts.byName[name] += length
		if id == 50 {
			parseBFBInfo(info, facts.versions)
		}
		if follow == 0 {
			break
		}
	}
	facts.bytes = n
	// what follows the last image: the appended bf.cfg
	h := sha256.New()
	k, err := io.Copy(h, r)
	n += k
	if progress != nil {
		progress(n)
	}
	if err != nil {
		return nil, fmt.Errorf("after the boot stream: %w", err)
	}
	facts.cfgBytes = k
	facts.cfgSHA = hex.EncodeToString(h.Sum(nil))
	return facts, nil
}

// parseBFBInfo reads the `info` image's software inventory (a Redfish SoftwareInventory
// collection, the same JSON DPF reads out of a BFB for its versions).
func parseBFBInfo(b []byte, into map[string]string) {
	i := strings.Index(string(b), "{")
	j := strings.LastIndex(string(b), "}")
	if i < 0 || j <= i {
		return
	}
	var inv struct {
		Members []struct{ Name, Version string }
	}
	if json.Unmarshal(b[i:j+1], &inv) != nil {
		return
	}
	for _, m := range inv.Members {
		if m.Name != "" && m.Version != "" {
			into[m.Name] = m.Version
		}
	}
}

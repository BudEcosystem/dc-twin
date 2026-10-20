package bluefield

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"os"
	"strings"
	"testing"
)

func image(id int, data []byte, last bool) []byte {
	p := append([]byte{}, data...)
	for len(p)%8 != 0 {
		p = append(p, 0)
	}
	h := make([]byte, 24)
	binary.LittleEndian.PutUint64(h[0:], uint64(bfbMagic)|uint64(3)<<52|uint64(id)<<56)
	binary.LittleEndian.PutUint64(h[8:], uint64(len(data))|uint64(crc32.ChecksumIEEE(p))<<32)
	if !last {
		binary.LittleEndian.PutUint64(h[16:], 1)
	}
	return append(h, p...)
}

// TestReadBFB checks what a BMC can check of a boot stream in transit: the header magic, each
// image's CRC, the stream ending where the last image says, and what follows it (the bf.cfg).
func TestReadBFB(t *testing.T) {
	info := []byte(`{"Members": [{"Name": "DOCA", "Version": "3.5.0"}]}`)
	good := append(append(image(50, info, false), image(62, []byte("kernel image"), true)...), []byte("BFCFG")...)
	var seen int64
	f, err := readBFB(bytes.NewReader(good), func(n int64) { seen = n })
	if err != nil {
		t.Fatal(err)
	}
	if f.images != 2 || f.versions["DOCA"] != "3.5.0" || f.cfgBytes != 5 || f.byName["image"] != 12 || seen != int64(len(good)) {
		t.Fatalf("facts %+v, progress %d of %d", f, seen, len(good))
	}
	bad := append([]byte{}, good...)
	bad[24+3] ^= 0xFF // a byte of the info image
	if _, err := readBFB(bytes.NewReader(bad), nil); err == nil || !strings.Contains(err.Error(), "CRC") {
		t.Fatalf("a corrupted image: %v", err)
	}
	if _, err := readBFB(bytes.NewReader(good[:40]), nil); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("a truncated stream: %v", err)
	}
	if _, err := readBFB(strings.NewReader("####### test-bfb.bfb"), nil); err != errNotBFB {
		t.Fatalf("not a BFB: %v", err)
	}
}

func TestParsePLDMRefusesOtherFiles(t *testing.T) {
	if _, err := parsePLDM(bytes.Repeat([]byte{1}, 64)); err == nil {
		t.Fatal("a file that is not a PLDM package was accepted")
	}
}

// TestParsePLDMRealPackage reads NVIDIA's BF3 BMC package (bf3-bmc-24.10-17_opn.fwpkg, the one DPF
// pushes to a BMC older than it supports; compat/dpf downloads it) when DCTWIN_BF3_FWPKG names it.
// The package is NVIDIA's and is not committed.
func TestParsePLDMRealPackage(t *testing.T) {
	path := os.Getenv("DCTWIN_BF3_FWPKG")
	if path == "" {
		t.Skip("DCTWIN_BF3_FWPKG is not set (compat/dpf downloads the package)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := parsePLDM(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.version != "cec1736ApFwPkg-02410017" || p.bmcVersion != "BF-24.10-17" || len(p.components) != 1 {
		t.Fatalf("%+v", p)
	}
}

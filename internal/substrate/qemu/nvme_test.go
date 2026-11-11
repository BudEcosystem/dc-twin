package qemu

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// TestDriveZeroesStaySparse: a guest's NVMe Format writes zeroes over the whole drive (QEMU's
// write-zeroes with may-unmap); through the drive's block node, as the twin opens it, the zeroes
// unmap the backing rather than allocate it, so formatting a 3.84 TB drive does not fill the
// host's disk. (On macOS QEMU's file driver punches no holes and writes the zeroes.)
func TestDriveZeroesStaySparse(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("QEMU unmaps a file's range on Linux only")
	}
	qio, err := exec.LookPath("qemu-io")
	if err != nil {
		t.Skip("no qemu-io")
	}
	path := filepath.Join(t.TempDir(), "drive.raw")
	if _, err := CreateBacking(path, 64<<30); err != nil {
		t.Fatal(err)
	}
	bd := Drive{Path: path}.blockdev("nvme0")
	delete(bd, "node-name")
	opts, _ := json.Marshal(bd)
	out, err := exec.Command(qio, "json:"+string(opts), "-c", "write -P 0xab 0 256M", "-c", "write -z -u 0 1G").CombinedOutput()
	if err != nil {
		t.Fatalf("qemu-io: %v: %s", err, out)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if blocks := fi.Sys().(*syscall.Stat_t).Blocks; blocks*512 > 1<<20 {
		t.Fatalf("after the guest's zeroes the backing holds %d bytes; want it unmapped (qemu-io: %s)", blocks*512, out)
	}
}

package vm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// segment is one gzip stream of tar entries, as an Alpine package is made of (signature,
// control, data), without the tar end-of-archive blocks between them.
func segment(t *testing.T, entries []*tar.Header, data map[string]string, last bool) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, h := range entries {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(data[h.Name]))
	}
	tw.Flush()
	if last {
		tw.Close()
	}
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	zw.Write(raw.Bytes())
	zw.Close()
	return z.Bytes()
}

// A package's files and links reach the archive with their modes; its control entries do not.
func TestUnpackAPK(t *testing.T) {
	ctl := segment(t, []*tar.Header{{Name: ".PKGINFO", Mode: 0o644, Size: 9, Typeflag: tar.TypeReg}}, map[string]string{".PKGINFO": "pkgname=x"}, false)
	data := segment(t, []*tar.Header{
		{Name: "usr/sbin/", Mode: 0o755, Typeflag: tar.TypeDir},
		{Name: "usr/sbin/rpc.nfsd", Mode: 0o755, Size: 5, Typeflag: tar.TypeReg},
		{Name: "usr/lib/libtirpc.so.3", Linkname: "libtirpc.so.3.0.0", Typeflag: tar.TypeSymlink},
		{Name: "lib/", Mode: 0o755, Typeflag: tar.TypeDir},
		{Name: "lib/ld-musl-x86_64.so.1", Mode: 0o755, Size: 4, Typeflag: tar.TypeReg},
	}, map[string]string{"usr/sbin/rpc.nfsd": "\x7fELF!", "lib/ld-musl-x86_64.so.1": "musl"}, true)
	p := filepath.Join(t.TempDir(), "x.apk")
	os.WriteFile(p, append(ctl, data...), 0o644)
	var buf bytes.Buffer
	cw := &cpioWriter{w: &buf}
	seen := map[string]bool{}
	if err := unpackAPK(p, cw, func(n string) { seen[filepath.Dir(n)] = true }); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"usr/sbin/rpc.nfsd\x00", "\x7fELF!", "usr/lib/libtirpc.so.3\x00", "libtirpc.so.3.0.0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("archive lacks %q", want)
		}
	}
	if strings.Contains(out, ".PKGINFO") {
		t.Fatal("a control entry reached the archive")
	}
	// merged /usr: lib/ goes under usr/lib, and no entry replaces the initramfs's /lib link
	if !strings.Contains(out, "usr/lib/ld-musl-x86_64.so.1\x00") || strings.Contains(out, "\x00lib/") || strings.Contains(out, "lib\x00\x00") {
		t.Fatalf("lib/ is not placed under usr/lib, or /lib itself is written")
	}
	// the file's mode: regular, 0755 (newc header field 2, hex)
	i := strings.Index(out, "usr/sbin/rpc.nfsd\x00")
	if hdr := out[i-110 : i]; hdr[14:22] != "000081ED" {
		t.Fatalf("mode field %q", hdr[14:22])
	}
}

// The storage script is the guest's: busybox sh, every command it runs a busybox applet or a
// packaged binary.
func TestStorageScriptCommands(t *testing.T) {
	for _, fn := range []string{"setup()", "fs()", "rmfs()", "exports()", "vol()", "hosts()", "rmvol()", "bucket()", "rmbucket()", "report()"} {
		if !strings.Contains(storageScript, "\n"+fn+" {") {
			t.Fatalf("dctwin-storage has no %s", fn)
		}
	}
	if len(StoragePackages["x86_64"]) != len(StoragePackages["aarch64"]) {
		t.Fatal("both architectures pin the same packages")
	}
}

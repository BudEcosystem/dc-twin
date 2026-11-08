package vm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// A storage node of a storage system at fidelity vm (docs/reviews/2026-11-08-storage-deep-dive.md
// S-Q2) is an `os: twin` guest that serves real targets: Linux's NFS server and nvmet (their
// kernel halves are in the image's module image) and an S3 server. Their userspace is Alpine's own
// packages at the image's release, pinned by digest and fetched with the image (`dctwin assets
// fetch`), unpacked into one more initramfs archive with the twin's dctwin-storage script, which
// the storage adapter drives through the in-host agent.

// StoragePackages are the Alpine packages a storage node's image adds, by architecture: nfs-utils
// (rpc.nfsd, rpc.mountd, exportfs) and the libraries it needs, e2fsprogs (mkfs.ext4) and rclone
// (`rclone serve s3`, MIT).
var StoragePackages = map[string][]GuestFile{
	"x86_64": {
		apk("x86_64", "main", "musl-1.2.6-r3", "e142255d0fa397f14318996bc10378093dcc069dc3dd1b6818c8f28a789c21da"),
		apk("x86_64", "main", "nfs-utils-2.6.4-r6", "6bdf2fe237792da8b4abf343282d89999678405bd50cef017d8a55dbd6dcdbf0"),
		apk("x86_64", "main", "libtirpc-1.3.5-r1", "73d0fa9459a9b506a88f5c545c77af86e46495b5cdd16b3502928ab306bb9616"),
		apk("x86_64", "main", "libtirpc-conf-1.3.5-r1", "eb9b5e86ecb79f9d4092d9747466491a5f6d6b0b2ebb1d3812dc5cba4a04c0ba"),
		apk("x86_64", "main", "libblkid-2.42.3-r1", "2512c2bff496ac2b8561a3c814d21d35ce10cda579b3d410f9f96da93a5aaac1"),
		apk("x86_64", "main", "libeconf-0.8.3-r0", "2a791c7957961f830cf31b04f35f814a63afcf633badc9052425ff1cf2f64380"),
		apk("x86_64", "main", "libuuid-2.42.3-r1", "8306e5bb577696c9069fe1dfd9e1dcc39d2d481c6a1b0e707fd03c3e21aa6aa2"),
		apk("x86_64", "main", "libmount-2.42.3-r1", "e2ae003260f2c1ec13666c6e108d5e8e3d64974b0667166a02432bd3be263a7a"),
		apk("x86_64", "main", "krb5-libs-1.22.2-r1", "79502e864cafa97c7da8a07937422ee020f470526fe0c7fc8c2306302a7c2439"),
		apk("x86_64", "main", "krb5-conf-1.0-r2", "28b8e361a1583c04e98c261c3eb05ca7f0c0e80a9984a7c1b3cbfd7b60c86f37"),
		apk("x86_64", "main", "libcom_err-1.47.4-r0", "4723d0fb8e286dc6adb54dd3cdaedcd8db54260f75ee778a5acb4c16922c0f52"),
		apk("x86_64", "main", "keyutils-libs-1.6.3-r4", "855c94e6bd750fcd2ca34df29ae3602bc93bb0b4512b56d8f4ecca06919ed1b8"),
		apk("x86_64", "main", "e2fsprogs-1.47.4-r0", "c7a99a18206827b09f0cfa2373e6f1db0963845ad8468a378a3b99a7e716a0e8"),
		apk("x86_64", "main", "e2fsprogs-libs-1.47.4-r0", "2e067395a6981b06cfd2cd3abf0e6c6574f7ef64d02e01f75fc5323d4a337fb9"),
		apk("x86_64", "community", "rclone-1.75.2-r0", "d2ae4f2055a5281dafc259272600695e2a55bf174c473eb2b6a96fe04fbaee34"),
	},
	"aarch64": {
		apk("aarch64", "main", "musl-1.2.6-r3", "2e17864102278d9049c3a56f1bceed9e8cf287065617e579325123443daed8a5"),
		apk("aarch64", "main", "nfs-utils-2.6.4-r6", "62a61bf25ddce20a6f6838fbd288c0e3e1dfb3b9bf79c08fc3e829b00192fc87"),
		apk("aarch64", "main", "libtirpc-1.3.5-r1", "b4a7f6af586ca80147d68294548b612d053fc8d17d6090a3a2c0a1e5b01e4c41"),
		apk("aarch64", "main", "libtirpc-conf-1.3.5-r1", "cf652172d027232b5be5598643ea3bd8e9b8c7a5796b2a90cc8059251cd835c8"),
		apk("aarch64", "main", "libblkid-2.42.3-r1", "9d0f24976c575c53c12526b6e078edf34c980c153bc8e5f1c1053cbece5b8cfa"),
		apk("aarch64", "main", "libeconf-0.8.3-r0", "cf0899d49ded0a6891e3cdf1b37887f2ecd77293afe1d8f3cb8937763c83f183"),
		apk("aarch64", "main", "libuuid-2.42.3-r1", "9ce20c7ffe2ccaa7c321893c10564abbca13c3f2edb82f60a35f1f68e004f86c"),
		apk("aarch64", "main", "libmount-2.42.3-r1", "a2a6fc6aab7a59c70ec060f5fb8c950be42d1a017934bf32e5dd6984eaab15cc"),
		apk("aarch64", "main", "krb5-libs-1.22.2-r1", "6ea86f0c4b700fdd1880bf03c9b7651a1086a787f8a4573534840291240d31c6"),
		apk("aarch64", "main", "krb5-conf-1.0-r2", "14cf4469b1aa931c30a05b5dca4f516faac46ac1c28147c99634b0f7b6f0442f"),
		apk("aarch64", "main", "libcom_err-1.47.4-r0", "a75b5955ff57046e33fc2842cfe466dc77d8e68305d5c86c6646b38a069490a2"),
		apk("aarch64", "main", "keyutils-libs-1.6.3-r4", "ff992a1a3ec0733710782107fc0d6cbb2c72eb66a58bd80de91d72b623cbcfc8"),
		apk("aarch64", "main", "e2fsprogs-1.47.4-r0", "52fd401e79ce6b0ff7648733ba4de3135083cd0132543534e72106e7ea767a00"),
		apk("aarch64", "main", "e2fsprogs-libs-1.47.4-r0", "52124971ae397d599aadd79263fcd68fc9e271922ccf8185b124f35fe24c60c8"),
		apk("aarch64", "community", "rclone-1.75.2-r0", "79ebe2368179e3784d6b469ded2565d1fff185b6d66c22d0da0ac88453515836"),
	},
}

func apk(arch, repo, name, sum string) GuestFile {
	return GuestFile{Name: "apk/" + name + ".apk", URL: "https://dl-cdn.alpinelinux.org/alpine/v3.24/" + repo + "/" + arch + "/" + name + ".apk", SHA256: sum}
}

// fetchStoragePackages downloads a guest image's storage packages beside it.
func fetchStoragePackages(dir, arch string, w io.Writer) error {
	if err := os.MkdirAll(filepath.Join(dir, "apk"), 0o755); err != nil {
		return err
	}
	for _, f := range StoragePackages[arch] {
		dst := filepath.Join(dir, f.Name)
		if sum, err := fileSHA256(dst); err == nil && sum == f.SHA256 {
			continue
		}
		fmt.Fprintf(w, "storage node packages (%s): fetching %s\n", arch, f.URL)
		if err := download(f.URL, dst, f.SHA256); err != nil {
			return fmt.Errorf("%w (Alpine keeps only a package's latest revision: a newer one means the pins in internal/adapter/vm/storagenode.go need updating)", err)
		}
	}
	return nil
}

// storageLayout versions how BuildStorageInitrd lays the packages out (merged): a change rebuilds
// every storage node's initrd.
const storageLayout = "merged-usr/2"

// BuildStorageInitrd writes <dir>/initrd-twin-storage: the twin's initrd (BuildInitrd), then an
// archive with the storage packages' files and /usr/sbin/dctwin-storage. Rebuilt when its inputs
// change.
func BuildStorageInitrd(g GuestImage, dir string) (string, error) {
	base, err := BuildInitrd(g, dir)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	baseSum, err := fileSHA256(base)
	if err != nil {
		return "", err
	}
	h.Write([]byte(baseSum + storageScript + storageLayout))
	for _, f := range StoragePackages[g.Arch] {
		h.Write([]byte(f.SHA256))
	}
	stamp := hex.EncodeToString(h.Sum(nil))
	out := filepath.Join(dir, "initrd-twin-storage")
	if b, err := os.ReadFile(out + ".sha256"); err == nil && strings.TrimSpace(string(b)) == stamp {
		if _, err := os.Stat(out); err == nil {
			return out, nil
		}
	}
	b, err := os.ReadFile(base)
	if err != nil {
		return "", err
	}
	buf := bytes.NewBuffer(b)
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
	cw := &cpioWriter{w: buf}
	dirs := map[string]bool{}
	mkdirs := func(p string) {
		parts := strings.Split(p, "/")
		for i := 1; i < len(parts); i++ {
			d := strings.Join(parts[:i], "/")
			if d == "usr" || d == "lib" || d == "bin" || d == "sbin" {
				continue // there already, as a directory or a merged-/usr link (merged)
			}
			if !dirs[d] {
				dirs[d] = true
				cw.header(d, 0o040755, 0)
			}
		}
	}
	for _, f := range StoragePackages[g.Arch] {
		pkg := filepath.Join(dir, f.Name)
		if sum, err := fileSHA256(pkg); err != nil || sum != f.SHA256 {
			return "", fmt.Errorf("storage node package %s is missing or not as pinned (run `dctwin assets fetch`)", f.Name)
		}
		if err := unpackAPK(pkg, cw, mkdirs); err != nil {
			return "", fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	mkdirs("usr/sbin/dctwin-storage")
	cw.file("usr/sbin/dctwin-storage", 0o100755, []byte(storageScript))
	cw.trailer()
	tmp := out + ".partial"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	_ = os.WriteFile(out+".sha256", []byte(stamp+"\n"), 0o644)
	return out, nil
}

// unpackAPK copies an Alpine package's files and symbolic links into the archive. A package is
// concatenated gzip streams (signature, control, data) of tar segments; the control entries
// (.PKGINFO, .SIGN.*, install scripts) are skipped.
func unpackAPK(path string, cw *cpioWriter, mkdirs func(string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name == "" || strings.HasPrefix(name, ".") || strings.Contains(name, "..") {
			continue
		}
		name = merged(name)
		if name == "" {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			mkdirs(strings.TrimSuffix(name, "/") + "/x")
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			mkdirs(name)
			cw.file(name, 0o100000|int(hdr.Mode&0o7777), data)
		case tar.TypeSymlink:
			mkdirs(name)
			cw.file(name, 0o120777, []byte(hdr.Linkname))
		}
	}
}

// merged places a package's path in a merged-/usr tree, as Alpine's initramfs is (its /lib,
// /bin and /sbin are links into /usr): a file under lib/ goes to usr/lib/, and the top-level
// directories themselves are never written, because the kernel's initramfs unpacker replaces an
// entry of another type, and a directory written over /lib would hide the kernel's modules.
func merged(name string) string {
	top, rest, _ := strings.Cut(strings.TrimSuffix(name, "/"), "/")
	switch top {
	case "lib", "bin", "sbin":
		if rest == "" {
			return ""
		}
		return "usr/" + top + "/" + rest
	}
	return name
}

// DialGuest connects into a running vm host's guest at dst, from the segment gateway of the
// host's port portRef, which it gives the gateway gw first when no relay named one (a network
// without DHCP). The storage adapter publishes a storage node's targets so.
func (a *Adapter) DialGuest(ctx context.Context, path, portRef string, gw netip.Prefix, dst netip.AddrPort) (net.Conn, error) {
	m := a.machines[path]
	if m == nil {
		return nil, fmt.Errorf("%s is not a vm host of this adapter", path)
	}
	for _, n := range m.nics {
		if n.port.Ref != portRef {
			continue
		}
		if n.hp == nil {
			return nil, fmt.Errorf("%s is spliced onto a real switch: the twin is not its gateway", portRef)
		}
		if gw.IsValid() {
			mac, _ := net.ParseMAC(gatewayMAC(portRef))
			if err := n.hp.SetGateway(gw, mac); err != nil {
				return nil, err
			}
		}
		return n.hp.Dial(ctx, dst)
	}
	return nil, fmt.Errorf("%s has no NIC on %s", path, portRef)
}

// storageNode reports whether a device is a node of a storage system at fidelity vm.
func storageNode(rt *adapter.Runtime, d *model.Device) bool {
	yes := false
	rt.Engine.Read(func(m *model.Model) {
		for _, s := range m.StorageSystems {
			for _, n := range s.Nodes {
				yes = yes || (n == d && s.Fidelity == "vm")
			}
		}
	})
	return yes
}

// gatewayMAC is the MAC the twin's gateway answers with on a port no relay named one for: locally
// administered, derived from the port.
func gatewayMAC(ref string) string {
	s := sha256.Sum256([]byte("gateway " + ref))
	return fmt.Sprintf("02:dc:7f:%02x:%02x:%02x", s[0], s[1], s[2])
}

// storageScript is /usr/sbin/dctwin-storage in a storage node's guest: the node's targets, set up
// and changed by the storage adapter through the in-host agent (whose exec carries 2 KiB, so the
// logic is here and the twin sends commands). Everything it serves is under /pool, an ext4 file
// system on the node's first data drive: file systems in /pool/fs (bind-mounted under /exports,
// the NFSv4 pseudo-root), volumes as files in /pool/vol (nvmet namespaces), buckets in /pool/s3.
const storageScript = `#!/bin/sh
# dctwin-storage (internal/adapter/vm/storagenode.go): a storage node's targets
P=/pool; X=/exports; C=/sys/kernel/config/nvmet
say() { echo "dc-twin guest: storage: $*" > /dev/kmsg; }
iface_of() { for n in /sys/class/net/*; do [ "$(cat $n/address 2>/dev/null)" = "$1" ] && { basename $n; return; }; done; }
disk_of() {
	for b in /sys/block/nvme*n1; do
		case $(basename $b) in *c*) continue;; esac
		[ "$(tr -d ' ' < $b/device/serial 2>/dev/null)" = "$1" ] && { basename $b; return; }
	done
}
setup() { # MAC ADDR/PREFIX DRIVE-SERIAL S3-ACCESS S3-SECRET
	for m in configfs ext4 nfsd nvmet nvmet-tcp; do modprobe $m 2>/dev/null; done
	ip link set lo up
	i=$(iface_of $1); [ -n "$i" ] || { echo "no NIC with MAC $1"; return 1; }
	ip link set $i up && { ip -4 addr show dev $i | grep -q " ${2%/*}/" || ip addr add $2 dev $i; } || return 1
	d=$(disk_of $3); [ -n "$d" ] || { echo "no drive with serial $3"; return 1; }
	mkdir -p $P
	if ! mountpoint -q $P; then
		blkid /dev/$d 2>/dev/null | grep -q 'TYPE="ext4"' || mkfs.ext4 -q -F -L dctwin-pool -E lazy_itable_init=1,lazy_journal_init=1 /dev/$d || return 1
		mount -t ext4 -o noinit_itable /dev/$d $P || return 1
		say "pool on $d"
	fi
	mkdir -p $P/fs $P/vol $P/s3 $X /var/lib/nfs/v4recovery /run /etc
	# rpc.mountd walks the mount table for crossmnt exports, through /etc/mtab, which the
	# initramfs has not: without it musl's getline gets a NULL file and mountd crashes
	[ -e /etc/mtab ] || ln -s /proc/self/mounts /etc/mtab
	mountpoint -q /proc/fs/nfsd || mount -t nfsd nfsd /proc/fs/nfsd || return 1
	grep -q "^$X " /etc/exports 2>/dev/null || echo "$X *(ro,fsid=0,no_subtree_check,crossmnt,insecure)" > /etc/exports
	touch /var/lib/nfs/etab /var/lib/nfs/rmtab
	exportfs -ra || return 1
	pidof rpc.mountd >/dev/null || rpc.mountd -N 2 -N 3 --no-udp || return 1
	if [ "$(cat /proc/fs/nfsd/threads)" = 0 ]; then
		echo 15 > /proc/fs/nfsd/nfsv4leasetime; echo 15 > /proc/fs/nfsd/nfsv4gracetime
		rpc.nfsd -N 3 --no-udp --port 2049 8 || return 1 # the port given: the initramfs has no /etc/services
		say "nfs up"
	fi
	if ! pidof rclone >/dev/null; then
		HOME=/run rclone serve s3 --addr :8333 --auth-key "$4,$5" --config /dev/null $P/s3 > /run/rclone.log 2>&1 &
		say "s3 up"
	fi
	mountpoint -q /sys/kernel/config || mount -t configfs none /sys/kernel/config
	if [ ! -d $C/ports/1 ]; then
		mkdir $C/ports/1 && echo tcp > $C/ports/1/addr_trtype && echo ipv4 > $C/ports/1/addr_adrfam &&
		echo ${2%/*} > $C/ports/1/addr_traddr && echo 4420 > $C/ports/1/addr_trsvcid || return 1
		say "nvmet up"
	fi
	echo setup ok
}
fs() { # NAME [PATH]: the file system, exported at /exports/PATH (default /NAME)
	e=$X${2:-/$1}; mkdir -p $P/fs/$1 $e && chmod 0777 $P/fs/$1 && { mountpoint -q $e || mount --bind $P/fs/$1 $e; }
}
rmfs() { # NAME: unmount its export wherever it is (mountinfo: the bind's root is /fs/NAME)
	for e in $(awk -v r="/fs/$1" '$4 == r {print $5}' /proc/self/mountinfo); do umount $e && rmdir $e 2>/dev/null; done
	rm -rf $P/fs/$1
}
exports() { # the export lines below the pseudo-root's, on stdin, as the twin writes them
	{ echo "$X *(ro,fsid=0,no_subtree_check,crossmnt,insecure)"; cat; } > /etc/exports.new &&
	mv /etc/exports.new /etc/exports && exportfs -ra
}
vol() { # NAME NQN BYTES NGUID
	f=$P/vol/$1.img; s=$C/subsystems/$2
	[ -f $f ] || truncate -s $3 $f || return 1
	[ -d $s ] && return 0
	mkdir $s && echo 0 > $s/attr_allow_any_host && mkdir $s/namespaces/1 &&
	echo $f > $s/namespaces/1/device_path && echo $4 > $s/namespaces/1/device_nguid &&
	echo 1 > $s/namespaces/1/enable && ln -s $s $C/ports/1/subsystems/$2
}
hosts() { # NQN HOST... (none: no host reaches it)
	s=$C/subsystems/$1; shift
	for h in $s/allowed_hosts/*; do [ -L "$h" ] && rm $h; done
	for h in "$@"; do mkdir -p $C/hosts/$h && ln -s $C/hosts/$h $s/allowed_hosts/$h; done
}
rmvol() { # NAME NQN
	s=$C/subsystems/$2
	if [ -d $s ]; then
		rm -f $C/ports/1/subsystems/$2; for h in $s/allowed_hosts/*; do [ -L "$h" ] && rm $h; done
		echo 0 > $s/namespaces/1/enable; rmdir $s/namespaces/1; rmdir $s
	fi
	rm -f $P/vol/$1.img
}
bucket() { mkdir -p $P/s3/$1; }
rmbucket() { rm -rf $P/s3/$1; }
report() {
	mountpoint -q $P && echo "pool $(df -k $P | awk 'NR==2{print $3*1024, $2*1024}')"
	for d in $P/fs/*; do [ -d "$d" ] && echo "fs $(basename $d) $(du -sk $d | awk '{print $1*1024}')"; done
	for f in $P/vol/*.img; do [ -f "$f" ] && echo "vol $(basename $f .img) $(du -k $f | awk '{print $1*1024}')"; done
	for d in $P/s3/*; do [ -d "$d" ] && echo "bucket $(basename $d) $(du -sk $d | awk '{print $1*1024}')"; done
	for s in $C/subsystems/*; do [ -d "$s" ] && echo "nqn $(basename $s) $(ls $s/allowed_hosts | tr '\n' ' ')"; done
	pidof rpc.mountd >/dev/null && [ "$(cat /proc/fs/nfsd/threads 2>/dev/null)" != 0 ] && echo "up nfs"
	pidof rclone >/dev/null && echo "up s3"
	[ -d $C/ports/1 ] && echo "up nvme-tcp $(cat $C/ports/1/addr_traddr)"
	echo "exports $(md5sum /etc/exports 2>/dev/null | cut -c1-12)"
}
cmd=$1; shift; $cmd "$@"
`

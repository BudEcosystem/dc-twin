package e2e

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sto "github.com/BudEcosystem/dc-twin/internal/adapter/storage"
	"github.com/BudEcosystem/dc-twin/internal/adapter/vm"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/inhost"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/substrate/qemu"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// A storage system at fidelity vm (docs/reviews/2026-11-08-storage-deep-dive.md P4, S-Q2): its two
// nodes boot the twin's storage image under KVM and set up their targets; a file system, a volume
// and a bucket made in the model appear on their nodes (an export, an nvmet subsystem admitting
// the mapped host, a bucket); an S3 object put through the published endpoint is in the node's
// pool, what a node holds is observed back into the model, the NVMe/TCP target answers an
// initialisation through its published endpoint, and deleting an object removes it from its node.
func TestVMStorageSystem(t *testing.T) {
	if testing.Short() {
		t.Skip("boots two virtual machines")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("needs KVM: %v", err)
	}
	qdir := os.Getenv("DCTWIN_QEMU_DIR")
	if _, err := qemu.Find("x86_64", qdir, false); err != nil {
		t.Skipf("needs QEMU for x86_64: %v", err)
	}
	g := vm.Guests["x86_64"]
	gdir := vm.GuestDir(twin.AssetsDir(), "", g)
	if g.Present(gdir) != nil {
		t.Skipf("the twin's x86_64 guest image is not in %s (dctwin assets fetch)", twin.AssetsDir())
	}
	for _, f := range vm.StoragePackages["x86_64"] {
		if _, err := os.Stat(filepath.Join(gdir, f.Name)); err != nil {
			t.Skipf("the storage node packages are not fetched (dctwin assets fetch): %v", err)
		}
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("needs curl (its --aws-sigv4 is the S3 client)")
	}
	tw := up(t, "../../examples/estates/storage-vm-system.yaml", []string{"vm", "inhost", "storage"}, func(e *schema.Estate) { e.Spec.Adapters.VM.QEMU = qdir })
	ia := tw.Adapter("inhost").(*inhost.Adapter)
	sa := tw.Adapter("storage").(*sto.Adapter)
	const sys = "site-1/storage/lab-fs"
	nodes := []string{"site-1/hall-a/s01/node-01", "site-1/hall-a/s01/node-02"}
	report := func(node string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, _, err := ia.Exec(ctx, node, "dctwin-storage report")
		if err != nil {
			return ""
		}
		return out
	}
	waitFor := func(what string, within time.Duration, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			if ok() {
				return
			}
			time.Sleep(2 * time.Second)
		}
		for _, l := range sa.Report().Lines {
			t.Log(l)
		}
		t.Fatalf("%s: not within %s", what, within)
	}
	for _, n := range nodes {
		n := n
		waitFor(n+" serves nfs, s3 and nvme-tcp", 6*time.Minute, func() bool {
			r := report(n)
			return strings.Contains(r, "up nfs") && strings.Contains(r, "up s3") && strings.Contains(r, "up nvme-tcp 10.50.")
		})
	}
	src := schema.EventSource{Class: "user", Principal: "test"}
	req := engine.Request{Path: sys, Canonical: "storage.create", Source: src}
	fs, err := tw.Engine.CreateFileSystem(req, engine.FileSystemSpec{Name: "models", QuotaBytes: 10 << 30,
		Exports: []model.Export{{Clients: "10.50.0.0/16", Access: "rw"}, {Clients: "127.0.0.1", Access: "rw"}}})
	if err != nil {
		t.Fatal(err)
	}
	vol, err := tw.Engine.CreateStorageVolume(req, engine.VolumeCreate{Name: "vol1", CapacityBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	const host = "nqn.2014-08.org.nvmexpress:uuid:00000000-0000-4000-8000-0000000000e2"
	if _, err := tw.Engine.MapVolume(req, "vol1", host, false); err != nil {
		t.Fatal(err)
	}
	bkt, err := tw.Engine.CreateBucket(req, "datasets", 0)
	if err != nil {
		t.Fatal(err)
	}
	waitFor("the file system and its export on "+fs.Home, 2*time.Minute, func() bool {
		r := report(fs.Home)
		return strings.Contains(r, "fs models ")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	exports, _, _ := ia.Exec(ctx, fs.Home, "cat /etc/exports; exportfs -v")
	cancel()
	if !strings.Contains(exports, "/exports/models 10.50.0.0/16(rw,sync,no_subtree_check,insecure,fsid=101,no_root_squash)") {
		t.Fatalf("exports on %s:\n%s", fs.Home, exports)
	}
	// the kernel NFS server answers a real NFSv4.1 mount (the guest's own client, over loopback)
	ctx, cancel = context.WithTimeout(context.Background(), 90*time.Second)
	out, rc, err := ia.Exec(ctx, fs.Home, "modprobe nfsv4; mkdir -p /mnt/m && mount -t nfs4 -o vers=4.1,soft,timeo=50 127.0.0.1:/models /mnt/m && echo hello > /mnt/m/probe && cat /mnt/m/probe && umount /mnt/m")
	cancel()
	if err != nil || rc != 0 || !strings.Contains(out, "hello") {
		t.Fatalf("NFS mount on %s: rc %d %v\n%s", fs.Home, rc, err, out)
	}
	// what a node restart leaves: the file system's data in the pool, its export's bind mount
	// gone; the node serves it again
	// (the NFSv4 server holds the export until it has released the client's state, a moment after
	// the client unmounts: the unmount is tried again for up to a minute)
	ctx, cancel = context.WithTimeout(context.Background(), 90*time.Second)
	out, rc, err = ia.Exec(ctx, fs.Home, "exportfs -ua && for i in $(seq 1 60); do umount /exports/models 2>&1 && exit 0; sleep 1; done; exit 1")
	cancel()
	if err != nil || rc != 0 {
		t.Fatalf("unmount the export: rc %d %v\n%s", rc, err, out)
	}
	waitFor("the file system exported again on "+fs.Home, 2*time.Minute, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		out, rc, err := ia.Exec(ctx, fs.Home, "mount -t nfs4 -o vers=4.1,soft,timeo=50 127.0.0.1:/models /mnt/m && cat /mnt/m/probe; umount /mnt/m")
		return err == nil && rc == 0 && strings.Contains(out, "hello")
	})
	waitFor("the volume's subsystem admitting its host on "+vol.Home, 2*time.Minute, func() bool {
		r := report(vol.Home)
		return strings.Contains(r, "vol vol1 ") && strings.Contains(r, "nqn "+vol.NQN+" "+host)
	})
	waitFor("the bucket on "+bkt.Home, 2*time.Minute, func() bool { return strings.Contains(report(bkt.Home), "bucket datasets ") })

	// S3, signed as any S3 client signs, through the published endpoint of the bucket's node
	c := sa.Credentials()[0]
	chassis := func(p string) string {
		parts := strings.Split(p, "/")
		return parts[len(parts)-2] + "-" + parts[len(parts)-1]
	}
	s3 := c.Endpoints[chassis(bkt.Home)+"/s3"]
	body := filepath.Join(t.TempDir(), "hello.txt")
	os.WriteFile(body, []byte("weights v1\n"), 0o644)
	curl := func(args ...string) string {
		t.Helper()
		a := append([]string{"-sS", "--fail-with-body", "--aws-sigv4", "aws:amz:us-east-1:s3", "--user", c.AccessKey + ":" + c.SecretKey}, args...)
		out, err := exec.Command("curl", a...).CombinedOutput()
		if err != nil {
			t.Fatalf("curl %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	curl("-T", body, "http://"+s3+"/datasets/hello.txt")
	if got := curl("http://" + s3 + "/datasets/hello.txt"); got != "weights v1\n" {
		t.Fatalf("S3 GET: %q", got)
	}
	if list := curl("http://" + s3 + "/"); !strings.Contains(list, "<Name>datasets</Name>") {
		t.Fatalf("S3 ListBuckets: %s", list)
	}

	// what the node holds is observed: 8 MiB written into the file system in its node
	ctx, cancel = context.WithTimeout(context.Background(), 60*time.Second)
	if out, rc, err := ia.Exec(ctx, fs.Home, "dd if=/dev/urandom of=/pool/fs/models/blob bs=1M count=8 2>&1 && sync"); err != nil || rc != 0 {
		t.Fatalf("write in the guest: %v %d %s", err, rc, out)
	}
	cancel()
	waitFor("the model observing the file system's use", time.Minute, func() bool {
		v, _ := tw.Engine.StorageSystem(sys)
		for _, f := range v.FileSystems {
			if f.Name == "models" && f.UsedBytes >= 8<<20 {
				return true
			}
		}
		return false
	})

	// NVMe/TCP: an initialisation request through the published endpoint is answered by nvmet
	nv := c.Endpoints[chassis(vol.Home)+"/nvme-tcp"]
	conn, err := net.DialTimeout("tcp", nv, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	icreq := make([]byte, 128)
	icreq[0], icreq[2] = 0x00, 128 // ICReq, hlen
	binary.LittleEndian.PutUint32(icreq[4:8], 128)
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	conn.Write(icreq)
	resp := make([]byte, 128)
	if _, err := io.ReadFull(conn, resp); err != nil || resp[0] != 0x01 {
		t.Fatalf("ICResp: type %#x, %v", resp[0], err)
	}
	conn.Close()

	// a deleted file system leaves its node
	if err := tw.Engine.DeleteStorageObject(req, "filesystem", "models"); err != nil {
		t.Fatal(err)
	}
	waitFor("the file system removed from "+fs.Home, 2*time.Minute, func() bool {
		r := report(fs.Home)
		return r != "" && !strings.Contains(r, "fs models ")
	})
	for _, l := range sa.Report().Lines {
		t.Log(l)
	}
}

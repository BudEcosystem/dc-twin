package qemu

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortDir is a temporary directory whose socket paths fit a unix socket address.
func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("/tmp", "dctwin-cable-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// qemuNIC dials a cable as QEMU's stream netdev does and speaks its framing.
type qemuNIC struct{ c net.Conn }

func dialNIC(t *testing.T, path string) *qemuNIC {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &qemuNIC{c}
}

func (q *qemuNIC) send(t *testing.T, f []byte) {
	t.Helper()
	b := binary.BigEndian.AppendUint32(nil, uint32(len(f)))
	if _, err := q.c.Write(append(b, f...)); err != nil {
		t.Fatal(err)
	}
}

func (q *qemuNIC) recv(t *testing.T) []byte {
	t.Helper()
	q.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var h [4]byte
	if _, err := io.ReadFull(q.c, h[:]); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, binary.BigEndian.Uint32(h[:]))
	if _, err := io.ReadFull(q.c, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func readFrame(t *testing.T, c *Cable) []byte {
	t.Helper()
	got := make(chan []byte, 1)
	go func() {
		b := make([]byte, maxFrame)
		n, err := c.ReadFrame(b)
		if err == nil {
			got <- b[:n]
		}
	}()
	select {
	case b := <-got:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("no frame from the NIC")
		return nil
	}
}

// A NIC's cable carries QEMU's stream framing both ways, has no link while no machine runs,
// and takes the connection of the next process after a power cycle.
func TestCableCarriesQEMUStreamFrames(t *testing.T) {
	path := filepath.Join(shortDir(t), "nic0.sock")
	c, err := ListenCable(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.WriteFrame([]byte("x")); !errors.Is(err, ErrNoLink) {
		t.Fatalf("a frame with no machine: %v, want ErrNoLink", err)
	}
	q := dialNIC(t, path)
	frame := bytes.Repeat([]byte{0xab}, 1514)
	q.send(t, frame)
	if got := readFrame(t, c); !bytes.Equal(got, frame) {
		t.Fatalf("frame from the guest: %d bytes", len(got))
	}
	if !c.Linked() {
		t.Fatal("not linked with a machine connected")
	}
	if err := c.WriteFrame([]byte("to the guest")); err != nil {
		t.Fatal(err)
	}
	if got := q.recv(t); string(got) != "to the guest" {
		t.Fatalf("frame to the guest: %q", got)
	}
	// the machine powers off and on: a new process connects
	q.c.Close()
	q2 := dialNIC(t, path)
	deadline := time.Now().Add(5 * time.Second)
	for c.Connects.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.WriteFrame([]byte("again")); err != nil {
		t.Fatal(err)
	}
	if got := q2.recv(t); string(got) != "again" {
		t.Fatalf("after a power cycle: %q", got)
	}
	q2.send(t, []byte("from the new process"))
	if got := readFrame(t, c); string(got) != "from the new process" {
		t.Fatalf("after a power cycle, from the guest: %q", got)
	}
}

func TestStreamNetdev(t *testing.T) {
	cfg := Config{Arch: "aarch64", Accel: "hvf", RunDir: "/run/x", NICs: []NIC{{MAC: "3c:6d:66:00:00:01", Netdev: StreamNetdev("/run/x/nic,0.sock")}}}
	a := strings.Join(cfg.Args(), " ")
	want := "-netdev stream,server=off,addr.type=unix,addr.path=/run/x/nic,,0.sock,id=net0 -device virtio-net-pci,romfile=,netdev=net0,mac=3c:6d:66:00:00:01"
	if !strings.Contains(a, want) {
		t.Fatalf("args lack %q:\n%s", want, a)
	}
}

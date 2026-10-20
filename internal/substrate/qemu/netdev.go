package qemu

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
)

// A NIC's cable on macOS (D48). QEMU's user-mode networking is a NAT of QEMU's own and puts a
// guest on no segment the twin models; a tap needs privileges macOS does not give (and a
// vmnet backend needs an entitlement). The `stream` netdev needs neither: QEMU connects to a
// unix socket and carries the NIC's Ethernet frames over it, each prefixed with its length as a
// 32-bit big-endian integer (QEMU's net/stream.c, the framing `-netdev socket` has always used).
// The twin holds the socket's listening end and is the switch port the NIC is cabled to.
//
// A machine that is off has no process, so the cable has no far end: QEMU connects when it
// starts and the connection ends with the process. A Cable outlives power cycles: it accepts
// each new process's connection, and drops what the port sends while there is none, as a
// switch port with no link does.

// StreamNetdev is the -netdev value (without id) that connects a NIC to a Cable's socket.
func StreamNetdev(socket string) string {
	return "stream,server=off,addr.type=unix,addr.path=" + esc(socket)
}

// maxFrame bounds a frame read from QEMU: a jumbo frame and its headers. A longer length
// prefix means the stream is out of step, and the connection is dropped.
const maxFrame = 65535

// Cable is the twin's end of one NIC's stream netdev.
type Cable struct {
	Path string
	l    *net.UnixListener

	mu     sync.Mutex
	conn   net.Conn
	frames chan []byte
	closed chan struct{}
	once   sync.Once
	wmu    sync.Mutex

	// Rx and Tx count frames from and to the guest; Dropped what the port sent with no link.
	Rx, Tx, Dropped, Connects atomic.Int64
}

// ErrNoLink is what WriteFrame returns while no QEMU process is connected.
var ErrNoLink = errors.New("qemu: the NIC has no link (no machine is running)")

// ListenCable opens a NIC's socket, replacing a stale one left by an earlier run.
func ListenCable(path string) (*Cable, error) {
	_ = os.Remove(path)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("the NIC's socket %s: %w", path, err)
	}
	l.SetUnlinkOnClose(true)
	c := &Cable{Path: path, l: l, frames: make(chan []byte, 512), closed: make(chan struct{})}
	go c.accept()
	return c, nil
}

func (c *Cable) accept() {
	for {
		conn, err := c.l.Accept()
		if err != nil {
			return
		}
		c.mu.Lock()
		if old := c.conn; old != nil {
			old.Close() // a new process's NIC: the old one's connection is gone with it
		}
		c.conn = conn
		c.mu.Unlock()
		c.Connects.Add(1)
		go c.read(conn)
	}
}

func (c *Cable) read(conn net.Conn) {
	defer func() {
		conn.Close()
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
	}()
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 || n > maxFrame {
			return
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(conn, b); err != nil {
			return
		}
		c.Rx.Add(1)
		select {
		case c.frames <- b:
		case <-c.closed:
			return
		default:
			// the port is not reading fast enough: a full ingress queue drops, as a switch's does
			c.Dropped.Add(1)
		}
	}
}

// ReadFrame blocks for the next frame the guest sent. It returns io.EOF once the cable is
// closed.
func (c *Cable) ReadFrame(b []byte) (int, error) {
	select {
	case f := <-c.frames:
		return copy(b, f), nil
	case <-c.closed:
		return 0, io.EOF
	}
}

// WriteFrame sends one frame to the guest's NIC.
func (c *Cable) WriteFrame(b []byte) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		c.Dropped.Add(1)
		return ErrNoLink
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	c.wmu.Lock()
	_, err := conn.Write(buf)
	c.wmu.Unlock()
	if err != nil {
		c.Dropped.Add(1)
		return err
	}
	c.Tx.Add(1)
	return nil
}

// Linked reports whether a machine's NIC is connected.
func (c *Cable) Linked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Close removes the socket and ends the connection.
func (c *Cable) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.l.Close()
		c.mu.Lock()
		if c.conn != nil {
			c.conn.Close()
		}
		c.mu.Unlock()
	})
	return nil
}

// Package nbd serves one export over the Network Block Device protocol (the fixed newstyle
// handshake, simple replies): how the twin hands QEMU a block device it does not hold itself.
// QEMU's nbd block driver is the client; the twin's stand-in for a BlueField's NVMe emulation
// serves a storage system's volume to a host VM's emulated NVMe drive with it (P5). It answers
// NBD_OPT_EXPORT_NAME, NBD_OPT_INFO and NBD_OPT_GO, and READ, WRITE, FLUSH and DISCONNECT;
// every other option and command is refused as unsupported.
package nbd

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
)

// Device is what an export reads from and writes to.
type Device interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
	Flush() error
	Size() int64
}

const (
	nbdMagic      = 0x4e42444d41474943 // "NBDMAGIC"
	optMagic      = 0x49484156454f5054 // "IHAVEOPT"
	repMagic      = 0x0003e889045565a9
	reqMagic      = 0x25609513
	simpleMagic   = 0x67446698
	flagFixed     = 1 << 0
	flagNoZeroes  = 1 << 1
	optExportName = 1
	optAbort      = 2
	optInfo       = 6
	optGo         = 7
	repAck        = 1
	repInfo       = 3
	repErrUnsup   = 1<<31 + 1
	tflagHasFlags = 1 << 0
	tflagFlush    = 1 << 2
	cmdRead       = 0
	cmdWrite      = 1
	cmdDisc       = 2
	cmdFlush      = 3
	eIO           = 5
	eInval        = 22
	eNotSup       = 95
)

// Serve answers NBD clients on l until it is closed, each connection serving dev as the export
// name (any name the client asks for is this export). Requests are served in order.
func Serve(l net.Listener, name string, dev Device) error {
	var mu sync.Mutex // one device, one request at a time
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			_ = serveConn(c, dev, &mu)
		}()
	}
}

func be16(v uint16) []byte { b := make([]byte, 2); binary.BigEndian.PutUint16(b, v); return b }
func be32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func be64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

func optReply(w io.Writer, opt, typ uint32, data []byte) error {
	b := append(append(append(append(be64(repMagic), be32(opt)...), be32(typ)...), be32(uint32(len(data)))...), data...)
	_, err := w.Write(b)
	return err
}

func serveConn(c net.Conn, dev Device, mu *sync.Mutex) error {
	if _, err := c.Write(append(append(be64(nbdMagic), be64(optMagic)...), be16(flagFixed|flagNoZeroes)...)); err != nil {
		return err
	}
	var cf [4]byte
	if _, err := io.ReadFull(c, cf[:]); err != nil {
		return err
	}
	noZeroes := binary.BigEndian.Uint32(cf[:])&flagNoZeroes != 0
	tflags := uint16(tflagHasFlags | tflagFlush)
	size := uint64(dev.Size())
	// options
	for {
		var h [16]byte
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return err
		}
		if binary.BigEndian.Uint64(h[0:8]) != optMagic {
			return errors.New("nbd: bad option magic")
		}
		opt, n := binary.BigEndian.Uint32(h[8:12]), binary.BigEndian.Uint32(h[12:16])
		if n > 4096 {
			return errors.New("nbd: option too long")
		}
		if _, err := io.CopyN(io.Discard, c, int64(n)); err != nil {
			return err
		}
		switch opt {
		case optExportName:
			b := append(be64(size), be16(tflags)...)
			if !noZeroes {
				b = append(b, make([]byte, 124)...)
			}
			if _, err := c.Write(b); err != nil {
				return err
			}
			return transmit(c, dev, mu)
		case optInfo, optGo:
			if err := optReply(c, opt, repInfo, append(append(be16(0), be64(size)...), be16(tflags)...)); err != nil {
				return err
			}
			if err := optReply(c, opt, repAck, nil); err != nil {
				return err
			}
			if opt == optGo {
				return transmit(c, dev, mu)
			}
		case optAbort:
			_ = optReply(c, opt, repAck, nil)
			return nil
		default:
			if err := optReply(c, opt, repErrUnsup, nil); err != nil {
				return err
			}
		}
	}
}

func transmit(c net.Conn, dev Device, mu *sync.Mutex) error {
	reply := func(errno uint32, handle []byte, data []byte) error {
		b := append(append(be32(simpleMagic), be32(errno)...), handle...)
		_, err := c.Write(append(b, data...))
		return err
	}
	var h [28]byte
	for {
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return err
		}
		if binary.BigEndian.Uint32(h[0:4]) != reqMagic {
			return errors.New("nbd: bad request magic")
		}
		typ := binary.BigEndian.Uint16(h[6:8])
		handle := append([]byte{}, h[8:16]...)
		off := int64(binary.BigEndian.Uint64(h[16:24]))
		n := binary.BigEndian.Uint32(h[24:28])
		switch typ {
		case cmdRead:
			if n > 32<<20 {
				return reply(eInval, handle, nil)
			}
			buf := make([]byte, n)
			mu.Lock()
			_, err := dev.ReadAt(buf, off)
			mu.Unlock()
			if err != nil {
				if err := reply(eIO, handle, nil); err != nil {
					return err
				}
				continue
			}
			if err := reply(0, handle, buf); err != nil {
				return err
			}
		case cmdWrite:
			if n > 32<<20 {
				return errors.New("nbd: write too large")
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(c, buf); err != nil {
				return err
			}
			mu.Lock()
			_, err := dev.WriteAt(buf, off)
			mu.Unlock()
			errno := uint32(0)
			if err != nil {
				errno = eIO
			}
			if err := reply(errno, handle, nil); err != nil {
				return err
			}
		case cmdFlush:
			mu.Lock()
			err := dev.Flush()
			mu.Unlock()
			errno := uint32(0)
			if err != nil {
				errno = eIO
			}
			if err := reply(errno, handle, nil); err != nil {
				return err
			}
		case cmdDisc:
			return nil
		default:
			if err := reply(eNotSup, handle, nil); err != nil {
				return err
			}
		}
	}
}

// Aligned is a device whose reads and writes need not be aligned to the underlying device's
// block: a read reads the blocks it touches, a write reads, merges and writes them back (an
// NVMe/TCP namespace takes whole blocks; QEMU's NBD client sends what its guest asks).
type Aligned struct {
	Dev   Device
	Block int64
}

func (a Aligned) Size() int64  { return a.Dev.Size() }
func (a Aligned) Flush() error { return a.Dev.Flush() }

func (a Aligned) span(p []byte, off int64) (start int64, buf []byte) {
	start = off / a.Block * a.Block
	end := (off + int64(len(p)) + a.Block - 1) / a.Block * a.Block
	return start, make([]byte, end-start)
}

func (a Aligned) ReadAt(p []byte, off int64) (int, error) {
	if off%a.Block == 0 && int64(len(p))%a.Block == 0 {
		return a.Dev.ReadAt(p, off)
	}
	start, buf := a.span(p, off)
	if _, err := a.Dev.ReadAt(buf, start); err != nil {
		return 0, err
	}
	return copy(p, buf[off-start:]), nil
}

func (a Aligned) WriteAt(p []byte, off int64) (int, error) {
	if off%a.Block == 0 && int64(len(p))%a.Block == 0 {
		return a.Dev.WriteAt(p, off)
	}
	start, buf := a.span(p, off)
	if _, err := a.Dev.ReadAt(buf, start); err != nil {
		return 0, err
	}
	copy(buf[off-start:], p)
	if _, err := a.Dev.WriteAt(buf, start); err != nil {
		return 0, err
	}
	return len(p), nil
}

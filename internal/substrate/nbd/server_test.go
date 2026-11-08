package nbd

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
)

type mem struct{ b []byte }

func (m *mem) ReadAt(p []byte, off int64) (int, error)  { return copy(p, m.b[off:]), nil }
func (m *mem) WriteAt(p []byte, off int64) (int, error) { return copy(m.b[off:], p), nil }
func (m *mem) Flush() error                             { return nil }
func (m *mem) Size() int64                              { return int64(len(m.b)) }

// strict takes only whole 4 KiB blocks, as an NVMe/TCP namespace on a file does.
type strict struct{ mem }

func (s *strict) ReadAt(p []byte, off int64) (int, error) {
	if off%4096 != 0 || len(p)%4096 != 0 {
		panic("unaligned read")
	}
	return s.mem.ReadAt(p, off)
}
func (s *strict) WriteAt(p []byte, off int64) (int, error) {
	if off%4096 != 0 || len(p)%4096 != 0 {
		panic("unaligned write")
	}
	return s.mem.WriteAt(p, off)
}

// A client does the fixed newstyle handshake with NBD_OPT_GO, writes unaligned, flushes and
// reads back; the device below takes only whole blocks.
func TestServeGoWriteReadFlush(t *testing.T) {
	dev := &strict{mem{b: make([]byte, 1<<20)}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go Serve(l, "vol1", Aligned{Dev: dev, Block: 4096})
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	hello := make([]byte, 18)
	io.ReadFull(c, hello)
	if binary.BigEndian.Uint64(hello[0:8]) != nbdMagic || binary.BigEndian.Uint64(hello[8:16]) != optMagic {
		t.Fatalf("greeting %x", hello)
	}
	c.Write(be32(flagFixed | flagNoZeroes))
	name := []byte("vol1")
	data := append(append(be32(uint32(len(name))), name...), be16(0)...)
	c.Write(append(append(append(be64(optMagic), be32(optGo)...), be32(uint32(len(data)))...), data...))
	var size uint64
	for {
		h := make([]byte, 20)
		io.ReadFull(c, h)
		typ, n := binary.BigEndian.Uint32(h[12:16]), binary.BigEndian.Uint32(h[16:20])
		body := make([]byte, n)
		io.ReadFull(c, body)
		if typ == repInfo {
			size = binary.BigEndian.Uint64(body[2:10])
		}
		if typ == repAck {
			break
		}
	}
	if size != 1<<20 {
		t.Fatalf("export size %d", size)
	}
	req := func(typ uint16, handle, off uint64, n uint32, payload []byte) []byte {
		b := append(append(append(append(append(be32(reqMagic), be16(0)...), be16(typ)...), be64(handle)...), be64(off)...), be32(n)...)
		c.Write(append(b, payload...))
		r := make([]byte, 16)
		io.ReadFull(c, r)
		if binary.BigEndian.Uint32(r[4:8]) != 0 || binary.BigEndian.Uint64(r[8:16]) != handle {
			t.Fatalf("reply %x", r)
		}
		if typ == cmdRead {
			out := make([]byte, n)
			io.ReadFull(c, out)
			return out
		}
		return nil
	}
	req(cmdWrite, 1, 5000, 11, []byte("hello world"))
	req(cmdFlush, 2, 0, 0, nil)
	if got := req(cmdRead, 3, 4999, 13, nil); !bytes.Equal(got, []byte("\x00hello world\x00")) {
		t.Fatalf("read back %q", got)
	}
	if !bytes.Equal(dev.b[5000:5011], []byte("hello world")) {
		t.Fatal("not on the device")
	}
}

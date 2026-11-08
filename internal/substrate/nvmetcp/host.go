// Package nvmetcp is a minimal NVMe over TCP host (NVM Express TCP Transport Specification 1.1,
// NVMe over Fabrics 1.1): what the twin's stand-in for a BlueField's NVMe emulation (SNAP) uses
// to reach a storage system's volume as SNAP reaches a remote target (docs/reviews/2026-11-08-storage-deep-dive.md
// 5.3, P5). It connects one admin queue and one I/O queue, enables the controller, identifies
// namespace 1 and reads, writes and flushes it, one command at a time; no digests, no TLS, no
// keep-alive (KATO 0), no in-capsule data on the I/O queue (writes go by R2T).
package nvmetcp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// PDU types.
const (
	pduICReq       = 0x00
	pduICResp      = 0x01
	pduCapsuleCmd  = 0x04
	pduCapsuleResp = 0x05
	pduH2CData     = 0x06
	pduC2HData     = 0x07
	pduR2T         = 0x09

	flagLastPDU = 0x04
	flagSuccess = 0x08
)

// Session is a connection to one subsystem's controller: its admin queue and one I/O queue.
type Session struct {
	admin, io *queue
	cntlid    uint16
	nsid      uint32
	lbaSize   int
	nlb       uint64
}

type queue struct {
	mu      sync.Mutex
	c       net.Conn
	cid     uint16
	maxData uint32 // the controller's MAXH2CDATA
}

// Dial connects to subnqn at addr (host:port) as hostnqn, enables the controller and identifies
// namespace 1. dial opens each queue's TCP connection (nil: net.Dialer).
func Dial(ctx context.Context, addr, subnqn, hostnqn string, dial func(ctx context.Context, addr string) (net.Conn, error)) (*Session, error) {
	if dial == nil {
		var d net.Dialer
		dial = func(ctx context.Context, a string) (net.Conn, error) { return d.DialContext(ctx, "tcp", a) }
	}
	var hostid [16]byte
	_, _ = rand.Read(hostid[:])
	s := &Session{nsid: 1}
	var err error
	if s.admin, s.cntlid, err = connectQueue(ctx, dial, addr, 0, 31, 0xffff, subnqn, hostnqn, hostid); err != nil {
		return nil, fmt.Errorf("admin queue: %w", err)
	}
	fail := func(err error) (*Session, error) { s.Close(); return nil, err }
	// CC: I/O queue entry sizes 64 and 16 bytes, enable; then wait for CSTS.RDY
	if _, err := s.admin.propSet(0x14, (4<<20)|(6<<16)|1); err != nil {
		return fail(fmt.Errorf("enable: %w", err))
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		csts, err := s.admin.propGet(0x1c)
		if err != nil {
			return fail(fmt.Errorf("CSTS: %w", err))
		}
		if csts&1 == 1 {
			break
		}
		if time.Now().After(deadline) {
			return fail(errors.New("the controller did not become ready"))
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Identify Namespace 1: its size and LBA format
	id := make([]byte, 4096)
	var sqe [64]byte
	sqe[0] = 0x06
	binary.LittleEndian.PutUint32(sqe[4:], s.nsid)
	binary.LittleEndian.PutUint32(sqe[40:], 0) // CNS 0
	if _, err := s.admin.read(sqe, id); err != nil {
		return fail(fmt.Errorf("identify namespace: %w", err))
	}
	s.nlb = binary.LittleEndian.Uint64(id[0:8])
	flbas := id[26] & 0x0f
	lbads := id[128+4*int(flbas)+2]
	if lbads < 9 || s.nlb == 0 {
		return fail(fmt.Errorf("namespace 1: %d blocks of 2^%d bytes", s.nlb, lbads))
	}
	s.lbaSize = 1 << lbads
	if s.io, _, err = connectQueue(ctx, dial, addr, 1, 31, s.cntlid, subnqn, hostnqn, hostid); err != nil {
		return fail(fmt.Errorf("I/O queue: %w", err))
	}
	return s, nil
}

// Size is the namespace's size in bytes; BlockSize its LBA size.
func (s *Session) Size() int64    { return int64(s.nlb) * int64(s.lbaSize) }
func (s *Session) BlockSize() int { return s.lbaSize }

// Close disconnects both queues.
func (s *Session) Close() error {
	if s.io != nil {
		s.io.c.Close()
	}
	if s.admin != nil {
		s.admin.c.Close()
	}
	return nil
}

func (s *Session) rw(op byte, p []byte, off int64) error {
	if off%int64(s.lbaSize) != 0 || len(p)%s.lbaSize != 0 {
		return fmt.Errorf("unaligned %d bytes at %d (LBA size %d)", len(p), off, s.lbaSize)
	}
	const chunk = 128 << 10
	for len(p) > 0 {
		n := min(len(p), chunk)
		var sqe [64]byte
		sqe[0] = op
		binary.LittleEndian.PutUint32(sqe[4:], s.nsid)
		binary.LittleEndian.PutUint64(sqe[40:], uint64(off/int64(s.lbaSize)))
		binary.LittleEndian.PutUint16(sqe[48:], uint16(n/s.lbaSize-1))
		var err error
		if op == 0x02 {
			_, err = s.io.read(sqe, p[:n])
		} else {
			_, err = s.io.write(sqe, p[:n])
		}
		if err != nil {
			return err
		}
		p, off = p[n:], off+int64(n)
	}
	return nil
}

// ReadAt reads LBA-aligned bytes.
func (s *Session) ReadAt(p []byte, off int64) (int, error) {
	if err := s.rw(0x02, p, off); err != nil {
		return 0, err
	}
	return len(p), nil
}

// WriteAt writes LBA-aligned bytes.
func (s *Session) WriteAt(p []byte, off int64) (int, error) {
	if err := s.rw(0x01, p, off); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Flush commits the namespace's volatile writes.
func (s *Session) Flush() error {
	var sqe [64]byte
	sqe[0] = 0x00
	binary.LittleEndian.PutUint32(sqe[4:], s.nsid)
	_, err := s.io.command(sqe, nil, nil)
	return err
}

// ---------------------------------------------------------------- queues

// connectQueue opens one queue's connection and connects it to the controller (cntlid 0xffff:
// a new one), returning the controller's id.
func connectQueue(ctx context.Context, dial func(context.Context, string) (net.Conn, error), addr string, qid, sqsize, cntlid uint16, subnqn, hostnqn string, hostid [16]byte) (*queue, uint16, error) {
	c, err := dial(ctx, addr)
	if err != nil {
		return nil, 0, err
	}
	q := &queue{c: c}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	// ICReq / ICResp
	ic := make([]byte, 128)
	ic[0], ic[2] = pduICReq, 128
	binary.LittleEndian.PutUint32(ic[4:], 128)
	if _, err := c.Write(ic); err != nil {
		c.Close()
		return nil, 0, err
	}
	resp := make([]byte, 128)
	if _, err := io.ReadFull(c, resp); err != nil || resp[0] != pduICResp {
		c.Close()
		return nil, 0, fmt.Errorf("ICResp: type %#x, %v", resp[0], err)
	}
	if resp[11] != 0 {
		c.Close()
		return nil, 0, errors.New("the controller requires digests")
	}
	q.maxData = binary.LittleEndian.Uint32(resp[12:16])
	// Fabrics Connect, its data in the capsule
	var sqe [64]byte
	sqe[0], sqe[4] = 0x7f, 0x01
	binary.LittleEndian.PutUint16(sqe[40:], 0) // RECFMT
	binary.LittleEndian.PutUint16(sqe[42:], qid)
	binary.LittleEndian.PutUint16(sqe[44:], sqsize)
	binary.LittleEndian.PutUint32(sqe[48:], 0) // KATO
	data := make([]byte, 1024)
	copy(data[0:16], hostid[:])
	binary.LittleEndian.PutUint16(data[16:], cntlid)
	copy(data[256:512], subnqn)
	copy(data[512:768], hostnqn)
	cqe, err := q.command(sqe, data, nil)
	if err != nil {
		c.Close()
		return nil, 0, fmt.Errorf("connect: %w", err)
	}
	c.SetDeadline(time.Time{})
	return q, binary.LittleEndian.Uint16(cqe[0:2]), nil
}

func (q *queue) propSet(offset uint32, v uint64) ([]byte, error) {
	var sqe [64]byte
	sqe[0], sqe[4] = 0x7f, 0x00
	sqe[40] = 0 // 4-byte property
	binary.LittleEndian.PutUint32(sqe[44:], offset)
	binary.LittleEndian.PutUint64(sqe[48:], v)
	return q.command(sqe, nil, nil)
}

func (q *queue) propGet(offset uint32) (uint64, error) {
	var sqe [64]byte
	sqe[0], sqe[4] = 0x7f, 0x04
	sqe[40] = 0
	binary.LittleEndian.PutUint32(sqe[44:], offset)
	cqe, err := q.command(sqe, nil, nil)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(cqe[0:8]), nil
}

func (q *queue) read(sqe [64]byte, into []byte) ([]byte, error)  { return q.command(sqe, nil, into) }
func (q *queue) write(sqe [64]byte, from []byte) ([]byte, error) { return q.commandR2T(sqe, from) }

// command sends a capsule (with in-capsule data, or asking for into's length from the
// controller) and reads to its response; it returns the CQE.
func (q *queue) command(sqe [64]byte, inCapsule, into []byte) ([]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cid++
	cid := q.cid
	binary.LittleEndian.PutUint16(sqe[2:], cid)
	sqe[1] = 0x40 // PSDT: SGL
	sgl := sqe[24:40]
	switch {
	case inCapsule != nil:
		binary.LittleEndian.PutUint64(sgl[0:], 0) // offset 0 in the capsule
		binary.LittleEndian.PutUint32(sgl[8:], uint32(len(inCapsule)))
		sgl[15] = 0x01 // data block, offset
	case into != nil:
		binary.LittleEndian.PutUint32(sgl[8:], uint32(len(into)))
		sgl[15] = 0x5a // transport data block
	}
	hdr := make([]byte, 8, 72+len(inCapsule))
	hdr[0], hdr[2] = pduCapsuleCmd, 72
	plen := 72 + len(inCapsule)
	if inCapsule != nil {
		hdr[3] = 72
	}
	binary.LittleEndian.PutUint32(hdr[4:], uint32(plen))
	pdu := append(append(hdr, sqe[:]...), inCapsule...)
	if _, err := q.c.Write(pdu); err != nil {
		return nil, err
	}
	return q.await(cid, into, nil)
}

// commandR2T sends a write whose data the controller asks for by R2T.
func (q *queue) commandR2T(sqe [64]byte, from []byte) ([]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cid++
	cid := q.cid
	binary.LittleEndian.PutUint16(sqe[2:], cid)
	sqe[1] = 0x40
	sgl := sqe[24:40]
	binary.LittleEndian.PutUint32(sgl[8:], uint32(len(from)))
	sgl[15] = 0x5a
	hdr := make([]byte, 8, 72)
	hdr[0], hdr[2] = pduCapsuleCmd, 72
	binary.LittleEndian.PutUint32(hdr[4:], 72)
	if _, err := q.c.Write(append(hdr, sqe[:]...)); err != nil {
		return nil, err
	}
	return q.await(cid, nil, from)
}

// await reads PDUs until the command's response: C2H data into into, R2Ts answered from from.
func (q *queue) await(cid uint16, into, from []byte) ([]byte, error) {
	ch := make([]byte, 8)
	for {
		if _, err := io.ReadFull(q.c, ch); err != nil {
			return nil, err
		}
		hlen, pdo := int(ch[2]), int(ch[3])
		plen := int(binary.LittleEndian.Uint32(ch[4:8]))
		if hlen < 8 || plen < hlen || plen > 1<<24 {
			return nil, fmt.Errorf("a PDU of type %#x with header %d and length %d", ch[0], hlen, plen)
		}
		rest := make([]byte, hlen-8)
		if _, err := io.ReadFull(q.c, rest); err != nil {
			return nil, err
		}
		h := append(append([]byte{}, ch...), rest...)
		switch ch[0] {
		case pduCapsuleResp:
			if plen > hlen {
				io.CopyN(io.Discard, q.c, int64(plen-hlen))
			}
			cqe := h[8:24]
			if st := binary.LittleEndian.Uint16(cqe[14:16]) >> 1; st != 0 {
				return cqe, fmt.Errorf("status %#x (type %d, code %#x)", st, (st>>8)&7, st&0xff)
			}
			return cqe, nil
		case pduC2HData:
			datao := int(binary.LittleEndian.Uint32(h[12:16]))
			datal := int(binary.LittleEndian.Uint32(h[16:20]))
			if pad := pdo - hlen; pdo > hlen {
				io.CopyN(io.Discard, q.c, int64(pad))
			}
			if into == nil || datao+datal > len(into) {
				return nil, fmt.Errorf("C2H data %d at %d beyond the %d bytes asked", datal, datao, len(into))
			}
			if _, err := io.ReadFull(q.c, into[datao:datao+datal]); err != nil {
				return nil, err
			}
			if ch[1]&flagSuccess != 0 {
				return make([]byte, 16), nil
			}
		case pduR2T:
			ttag := binary.LittleEndian.Uint16(h[10:12])
			r2to := int(binary.LittleEndian.Uint32(h[12:16]))
			r2tl := int(binary.LittleEndian.Uint32(h[16:20]))
			if from == nil || r2to+r2tl > len(from) {
				return nil, fmt.Errorf("R2T for %d at %d beyond the %d bytes written", r2tl, r2to, len(from))
			}
			max := int(q.maxData)
			if max <= 0 {
				max = r2tl
			}
			for o := r2to; o < r2to+r2tl; {
				n := min(max, r2to+r2tl-o)
				d := make([]byte, 24, 24+n)
				d[0], d[2], d[3] = pduH2CData, 24, 24
				if o+n == r2to+r2tl {
					d[1] = flagLastPDU
				}
				binary.LittleEndian.PutUint32(d[4:], uint32(24+n))
				binary.LittleEndian.PutUint16(d[8:], cid)
				binary.LittleEndian.PutUint16(d[10:], ttag)
				binary.LittleEndian.PutUint32(d[12:], uint32(o))
				binary.LittleEndian.PutUint32(d[16:], uint32(n))
				if _, err := q.c.Write(append(d, from[o:o+n]...)); err != nil {
					return nil, err
				}
				o += n
			}
		default:
			if plen > hlen {
				io.CopyN(io.Discard, q.c, int64(plen-hlen))
			}
		}
	}
}

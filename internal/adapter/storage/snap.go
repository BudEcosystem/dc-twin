package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/substrate/nbd"
	"github.com/BudEcosystem/dc-twin/internal/substrate/nvmetcp"
)

// The twin's stand-in for a BlueField's NVMe emulation (DOCA SNAP) at vm
// (docs/reviews/2026-11-08-storage-deep-dive.md 5.3, P5). For each function a DPU presents whose
// host is a running vm host, the stand-in connects to the volume's NVMe/TCP target as the DPU
// (its host NQN), as SNAP does to a remote target, and serves that namespace over NBD to a QEMU
// NVMe controller hot-plugged into the host (internal/adapter/vm/emulation.go): the host's guest
// reads and writes the storage system's volume through an NVMe drive. It is the twin's, not SNAP:
// its PCI id is QEMU's, it runs beside the host's machine rather than on the DPU, and only
// functions on volumes of storage systems at vm have a data path.

type emulator interface {
	AttachEmulatedNVMe(path string, slot int, socket, serial string, blockSize int) error
	DetachEmulatedNVMe(path string, slot int) error
	EmulatedNVMe(path string) map[int]string
}

type snap struct {
	a  *Adapter
	mu sync.Mutex
	// live are the functions served, by "<dpu>#<id>"
	live  map[string]*snapFn
	lines map[string]string // why a function has no data path, by key
}

type snapFn struct {
	key, host, serial string
	slot              int
	tgt               *target
	ln                net.Listener
	sock              string
}

// target is what a function's NBD export reads and writes: its NVMe/TCP session to the volume,
// dialled again when that session's connection breaks (the target's node restarted, the
// connection dropped or stalled), so the host's drive recovers without a detach.
type target struct {
	mu     sync.Mutex // guards sess and closed (never held across a dial)
	dialMu sync.Mutex // one dial at a time
	sess   *nvmetcp.Session
	closed bool
	dial   func() (*nvmetcp.Session, error)
}

func (t *target) current() *nvmetcp.Session {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sess
}

// do runs op on the session; when the session broke under it, it dials again (once, for every
// caller that saw the same session break) and runs op on the new one.
func (t *target) do(op func(*nvmetcp.Session) error) error {
	s := t.current()
	err := op(s)
	if err == nil || !s.Broken() {
		return err
	}
	t.dialMu.Lock()
	if t.current() == s {
		ns, derr := t.dial()
		if derr != nil {
			t.dialMu.Unlock()
			return fmt.Errorf("%w (and dialling again: %v)", err, derr)
		}
		t.mu.Lock()
		if t.closed {
			ns.Close()
		} else {
			t.sess = ns
		}
		t.mu.Unlock()
		s.Close()
	}
	t.dialMu.Unlock()
	return op(t.current())
}

func (t *target) ReadAt(p []byte, off int64) (int, error) {
	return len(p), t.do(func(s *nvmetcp.Session) error { _, err := s.ReadAt(p, off); return err })
}

func (t *target) WriteAt(p []byte, off int64) (int, error) {
	return len(p), t.do(func(s *nvmetcp.Session) error { _, err := s.WriteAt(p, off); return err })
}

func (t *target) Flush() error { return t.do(func(s *nvmetcp.Session) error { return s.Flush() }) }
func (t *target) Size() int64  { return t.current().Size() }

func (t *target) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	t.sess.Close()
}

func newSnap(a *Adapter) *snap {
	return &snap{a: a, live: map[string]*snapFn{}, lines: map[string]string{}}
}

func (a *Adapter) runSnap(ctx context.Context) {
	sn := a.snap
	t := time.NewTicker(reconcileEvery)
	defer t.Stop()
	for {
		sn.reconcile(ctx)
		select {
		case <-ctx.Done():
			sn.mu.Lock()
			for _, f := range sn.live {
				f.close()
			}
			sn.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

func (f *snapFn) close() {
	if f.ln != nil {
		f.ln.Close()
	}
	if f.tgt != nil {
		f.tgt.Close()
	}
	os.Remove(f.sock)
}

func (sn *snap) reconcile(ctx context.Context) {
	rt := sn.a.rt
	if rt.Peer == nil {
		return
	}
	vm, ok := rt.Peer("vm").(emulator)
	if !ok {
		return
	}
	fns := rt.Engine.EmulatedFunctions()
	want := map[string]bool{}
	sn.mu.Lock()
	defer sn.mu.Unlock()
	for dpu, list := range fns {
		var host *model.Device
		rt.Engine.Read(func(m *model.Model) {
			if d := m.DeviceBy[dpu]; d != nil {
				host = d.Parent
			}
		})
		if host == nil || host.Fidelity != "vm" {
			continue // at model a function is state only
		}
		plugged := vm.EmulatedNVMe(host.Path)
		for _, fn := range list {
			key := fmt.Sprintf("%s#%d", dpu, fn.ID)
			want[key] = true
			if f := sn.live[key]; f != nil {
				if plugged[f.slot] == f.serial {
					continue
				}
				// the host restarted (or is off): plug it again when it runs
				f.close()
				delete(sn.live, key)
			}
			if len(plugged) == 0 && !hostRunning(vm, host.Path) {
				sn.lines[key] = "its host is off"
				continue
			}
			f, err := sn.start(ctx, vm, host, fn, plugged)
			if err != nil {
				sn.lines[key] = err.Error()
				continue
			}
			delete(sn.lines, key)
			sn.live[key] = f
		}
	}
	for key, f := range sn.live {
		if !want[key] {
			_ = vm.DetachEmulatedNVMe(f.host, f.slot)
			f.close()
			delete(sn.live, key)
		}
	}
	for key := range sn.lines {
		if !want[key] {
			delete(sn.lines, key)
		}
	}
}

// hostRunning: a vm host reports no plugged functions both when off and when none are plugged;
// a host the adapter does not run is not running for the stand-in.
func hostRunning(vm emulator, path string) bool {
	type runner interface{ Running(path string) bool }
	if r, ok := vm.(runner); ok {
		return r.Running(path)
	}
	return true
}

// dial connects to the volume's NVMe/TCP target where it is served now, as the DPU.
func (sn *snap) dial(ctx context.Context, fn model.EmulatedFunction) (*nvmetcp.Session, error) {
	v, err := sn.a.rt.Engine.StorageSystem(fn.System)
	if err != nil {
		return nil, err
	}
	var vol *struct{ nqn, node string }
	for _, o := range v.Volumes {
		if o.ID == fn.Volume {
			vol = &struct{ nqn, node string }{o.NQN, o.Node}
		}
	}
	if vol == nil {
		return nil, fmt.Errorf("volume %s of %s is gone", fn.Volume, v.Name)
	}
	if v.Fidelity != "vm" {
		return nil, fmt.Errorf("%s is at fidelity %s: no target to connect to", v.Name, v.Fidelity)
	}
	if vol.node == "" {
		return nil, fmt.Errorf("volume %s is not served (its node is down)", fn.Volume)
	}
	ep := v.Endpoints["nvme-tcp@"+chassisID(vol.node)]
	if ep == "" {
		return nil, fmt.Errorf("the NVMe/TCP target of %s is not published", vol.node)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sess, err := nvmetcp.Dial(cctx, ep, vol.nqn, fn.HostNQN, nil)
	if err != nil {
		return nil, fmt.Errorf("NVMe/TCP to %s at %s as %s: %w", vol.nqn, ep, fn.HostNQN, err)
	}
	return sess, nil
}

func (sn *snap) start(ctx context.Context, vm emulator, host *model.Device, fn model.EmulatedFunction, plugged map[int]string) (*snapFn, error) {
	slot := -1
	for i := 0; i < 4; i++ {
		if _, used := plugged[i]; !used && !sn.slotTaken(host.Path, i) {
			slot = i
			break
		}
	}
	if slot < 0 {
		return nil, fmt.Errorf("%s has no spare port for another function", host.Path)
	}
	sess, err := sn.dial(ctx, fn)
	if err != nil {
		return nil, err
	}
	tgt := &target{sess: sess, dial: func() (*nvmetcp.Session, error) { return sn.dial(ctx, fn) }}
	dir := filepath.Join(sn.a.rt.StateDir, "snap")
	os.MkdirAll(dir, 0o700)
	sum := sha256.Sum256([]byte(host.Path + fn.Serial))
	sock := filepath.Join(dir, hex.EncodeToString(sum[:6])+".sock")
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		tgt.Close()
		return nil, err
	}
	f := &snapFn{key: fmt.Sprintf("%s#%d", fn.Serial, fn.ID), host: host.Path, serial: fn.Serial, slot: slot, tgt: tgt, ln: ln, sock: sock}
	go nbd.Serve(ln, "volume", nbd.Aligned{Dev: tgt, Block: int64(sess.BlockSize())})
	if err := vm.AttachEmulatedNVMe(host.Path, slot, sock, fn.Serial, sess.BlockSize()); err != nil {
		f.close()
		return nil, err
	}
	return f, nil
}

func (sn *snap) slotTaken(host string, slot int) bool {
	for _, f := range sn.live {
		if f.host == host && f.slot == slot {
			return true
		}
	}
	return false
}

func (sn *snap) reportLines() []string {
	sn.mu.Lock()
	defer sn.mu.Unlock()
	var out []string
	for _, f := range sn.live {
		out = append(out, fmt.Sprintf("NVMe emulation stand-in: %s presents %s on %s's spare port %d (NVMe/TCP %d-byte blocks, %d bytes, served over NBD)",
			f.key, f.serial, f.host, f.slot, f.tgt.current().BlockSize(), f.tgt.Size()))
	}
	for k, why := range sn.lines {
		out = append(out, "NVMe emulation stand-in: "+k+": no data path: "+why)
	}
	sort.Strings(out)
	return out
}

package engine

import (
	"errors"
	"slices"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// A BlueField-3's NVMe emulation at model: off refuses an attach; on, attaching a volume maps it
// to the DPU's host NQN in the storage system; emulation cannot go off with a function attached;
// detaching unmaps it.
func TestDPUNVMeEmulation(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../examples/estates/storage-dpu-vm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	est.Spec.Clock, est.Spec.Fidelity.Overrides = "virtual", nil
	est.Spec.StorageSystems[0].Fidelity = "model"
	res, err := compile.Compile(est, reg, compile.Options{HostArch: "x86_64"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(res.Model, reg, "")
	if err != nil {
		t.Fatal(err)
	}
	var dpu *model.Device
	for _, ch := range e.M.DeviceBy["site-1/hall-a/c01/server-01"].Children {
		if ch.BaseClass == "dpu" {
			dpu = ch
		}
	}
	if dpu == nil {
		t.Fatal("the host has no DPU")
	}
	sys := "site-1/storage/lab-fs"
	if _, err := e.CreateStorageVolume(Request{Path: sys, Source: stack}, VolumeCreate{Name: "vol1", CapacityBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	r := Request{Path: dpu.Path, Canonical: "dpu.emulation.attach", Source: stack}
	if _, err := e.AttachEmulatedVolume(r, sys, "vol1"); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("attach with emulation off: %v", err)
	}
	if err := e.SetNVMeEmulation(r, true); err != nil {
		t.Fatal(err)
	}
	f, err := e.AttachEmulatedVolume(r, sys, "vol1")
	if err != nil {
		t.Fatal(err)
	}
	v, _ := e.StorageSystem(sys)
	if !slices.Contains(v.Volumes[0].Hosts, DPUHostNQN(dpu)) || f.HostNQN != DPUHostNQN(dpu) || f.Serial == "" {
		t.Fatalf("function %+v; volume hosts %v", f, v.Volumes[0].Hosts)
	}
	if err := e.SetNVMeEmulation(r, false); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("off with a function attached: %v", err)
	}
	if _, err := e.AttachEmulatedVolume(r, sys, "vol1"); !errors.Is(err, ErrExists) {
		t.Fatalf("the same volume twice: %v", err)
	}
	if err := e.DeleteStorageObject(Request{Path: sys, Source: stack}, "volume", "vol1"); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("deleting a volume a DPU presents: %v", err)
	}
	if err := e.DetachEmulatedVolume(r, f.ID); err != nil {
		t.Fatal(err)
	}
	if v, _ = e.StorageSystem(sys); slices.Contains(v.Volumes[0].Hosts, DPUHostNQN(dpu)) {
		t.Fatalf("still mapped after detach: %v", v.Volumes[0].Hosts)
	}
	// a host is not a DPU
	if err := e.SetNVMeEmulation(Request{Path: "site-1/hall-a/c01/server-01", Source: stack}, true); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("emulation on a server: %v", err)
	}
}

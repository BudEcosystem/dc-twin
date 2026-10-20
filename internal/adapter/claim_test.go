package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

type fakeSubstrate struct {
	name   string
	refuse string
}

func (f *fakeSubstrate) Name() string                          { return f.name }
func (f *fakeSubstrate) Start(context.Context, *Runtime) error { return nil }
func (f *fakeSubstrate) Stop() error                           { return nil }
func (f *fakeSubstrate) Report() Report                        { return Report{Adapter: f.name} }
func (f *fakeSubstrate) Accepts(*schema.Estate, *model.Device, bool) error {
	if f.refuse != "" {
		return errors.New(f.refuse)
	}
	return nil
}

type fakeSpecialised struct{ fakeSubstrate }

func (*fakeSpecialised) Specialised() {}

// One substrate runs each vm device: a specialised one (the bluefield adapter, for a BlueField's
// Arm) before a general one (the vm adapter), whatever order --adapters names them in; the
// general one when the specialised one refuses; and every refusal said when all refuse.
func TestClaimSubstrate(t *testing.T) {
	d := &model.Device{Path: "site-1/hall-a/a01/compute-tray-01/bf3-0"}
	vm := &fakeSubstrate{name: "vm"}
	bf := &fakeSpecialised{fakeSubstrate{name: "bluefield"}}
	for _, order := range [][]Substrate{{vm, bf}, {bf, vm}} {
		s, err := ClaimSubstrate(nil, d, order, false)
		if err != nil || s.Name() != "bluefield" {
			t.Fatalf("claimed by %v (%v), want the bluefield adapter", s, err)
		}
	}
	if s, err := ClaimSubstrate(nil, d, []Substrate{vm}, false); err != nil || s != vm {
		t.Fatalf("vm alone: %v %v", s, err)
	}
	bfNo := &fakeSpecialised{fakeSubstrate{name: "bluefield", refuse: "the bluefield adapter runs only BlueField DPUs"}}
	if s, err := ClaimSubstrate(nil, d, []Substrate{bfNo, vm}, false); err != nil || s != vm {
		t.Fatalf("specialised refused: %v %v", s, err)
	}
	vmNo := &fakeSubstrate{name: "vm", refuse: "no QEMU"}
	if _, err := ClaimSubstrate(nil, d, []Substrate{vmNo}, false); err == nil || err.Error() != "no QEMU" {
		t.Fatalf("one refusal: %v", err)
	}
	if _, err := ClaimSubstrate(nil, d, []Substrate{vmNo, bfNo}, false); err == nil ||
		!strings.Contains(err.Error(), "vm: no QEMU") || !strings.Contains(err.Error(), "bluefield: the bluefield adapter runs only") {
		t.Fatalf("two refusals: %v", err)
	}
	if _, err := ClaimSubstrate(nil, d, nil, false); err == nil || !strings.Contains(err.Error(), "the vm adapter is not enabled") {
		t.Fatalf("no substrate: %v", err)
	}

	rt := &Runtime{}
	if !rt.Runs("vm", d.Path) {
		t.Fatal("a Runtime without claims refused a device its substrate plans")
	}
	rt.Claims = map[string]string{d.Path: "bluefield"}
	if rt.Runs("vm", d.Path) || !rt.Runs("bluefield", d.Path) || rt.Runs("vm", "elsewhere") {
		t.Fatal("Runs does not follow the claims")
	}
}

package engine

import (
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// LinkUp is the cable as the port's own end has it: a pulled cable or a port down by admin or
// fault, but not its own device's power, which a machine running a NIC has by running it. PortUp
// also counts the device's power.
func TestLinkUpIsTheCableNotThePower(t *testing.T) {
	e := virtualNVL(t)
	d := e.M.DeviceBy[tray7]
	var p *model.Port
	for _, d := range append([]*model.Device{d}, d.Children...) {
		for _, x := range d.Ports {
			if x.Kind == "ethernet" && x.Role != "management" && x.Link != nil && p == nil {
				p = x
			}
		}
	}
	if p == nil {
		t.Fatal("no cabled data port")
	}
	if !e.LinkUp(p) {
		t.Fatalf("%s: cabled, no fault, but no link", p.Ref)
	}
	e.mu.Lock()
	p.Device.State.Power = model.Off
	e.mu.Unlock()
	if e.PortUp(p) || !e.LinkUp(p) {
		t.Errorf("%s off: PortUp %v, LinkUp %v; want false, true", p.Ref, e.PortUp(p), e.LinkUp(p))
	}
	if err := e.SetLink(p.Ref, false, schema.EventSource{Class: "test", Principal: "test"}); err != nil {
		t.Fatal(err)
	}
	if e.LinkUp(p) {
		t.Errorf("%s: the cable is pulled, but LinkUp", p.Ref)
	}
}

package pack

import (
	"testing"

	"github.com/BudEcosystem/dc-twin/packs"
)

func TestInTreePacksLoad(t *testing.T) {
	r, err := Load(packs.FS, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Profiles) < 15 || len(r.Formats) < 8 || len(r.Faults) < 20 {
		t.Fatalf("too few objects: %d profiles, %d formats, %d faults", len(r.Profiles), len(r.Formats), len(r.Faults))
	}
	d, chain := r.Dialect("nvidia/gb300-bmc")
	if d == nil || len(chain) != 2 || d.Spec.Mockup != "libredfish/nvidia_gb200" || d.Spec.ServiceRoot["Product"] != "GB BMC" {
		t.Fatalf("gb300 dialect inheritance: %+v %v", d, chain)
	}
}

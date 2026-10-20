package identity

import "testing"

func TestStableAndProfileSensitive(t *testing.T) {
	d := New(1729)
	a := d.Serial("site-1/hall-a/a03/compute-tray-07", "nvidia/gb300-compute-tray@0", "1{digits:12}", 0)
	b := New(1729).Serial("site-1/hall-a/a03/compute-tray-07", "nvidia/gb300-compute-tray@0", "1{digits:12}", 0)
	c := d.Serial("site-1/hall-a/a03/compute-tray-07", "nvidia/gb200-compute-tray@0", "1{digits:12}", 0)
	if a != b || len(a) != 13 || a[0] != '1' {
		t.Fatalf("serial not stable or wrong shape: %q %q", a, b)
	}
	if a == c {
		t.Fatal("a different profile at the same path must give a different identity (D26)")
	}
	if d.MAC("p", "x@0", "bmc0", "aa:bb:cc", 0)[:8] != "aa:bb:cc" {
		t.Fatal("OUI")
	}
	u := d.UUID("p", "x@0", 0)
	if len(u) != 36 || u[14] != '4' {
		t.Fatalf("uuid %q", u)
	}
	if s := d.Scrub("mac", "p", "x@0", "B8:3F:D2:01:02:03"); s[:8] != "B8:3F:D2" || s == "B8:3F:D2:01:02:03" {
		t.Fatalf("scrub mac %q", s)
	}
}

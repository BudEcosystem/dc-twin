package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

// A secure erase takes the drive's erase time and leaves it erased; a host whose boot drive no
// longer holds an operating system stops in its firmware instead of booting to os.
func TestEraseLeavesTheHostWithNothingToBoot(t *testing.T) {
	e := virtualNVL(t)
	d := e.M.DeviceBy[tray7]
	bd := d.BootDrive()
	if bd == nil || bd.State.Contents != ContentsInstalled {
		t.Fatalf("boot drive %v", bd)
	}
	task, err := e.SecureErase(Request{Path: tray7, Canonical: "erase.start", Source: stack}, bd.Name, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if st := e.DriveStatus(bd); !st.Erasing || st.Contents != ContentsErasing {
		t.Fatalf("while it runs: %+v", st)
	}
	e.Advance(30 * time.Second)
	if bd.State.Contents != ContentsErased || d.State.Tasks[len(d.State.Tasks)-1].State != "Completed" || task.ID == "" {
		t.Fatalf("after the erase: %s, task %+v", bd.State.Contents, d.State.Tasks)
	}
	if err := e.Power(Request{Path: tray7, Canonical: "power.restart", Source: stack}); err != nil {
		t.Fatal(err)
	}
	e.Advance(5 * time.Minute)
	if !strings.HasPrefix(d.State.Booted, "firmware (no bootable device") {
		t.Fatalf("after a restart with an erased boot drive: booted %q", d.State.Booted)
	}
	// an installer writes it again
	if err := e.SetDriveContents(bd.Path, ContentsInstalled, user); err != nil {
		t.Fatal(err)
	}
	if err := e.Power(Request{Path: tray7, Canonical: "power.restart", Source: stack}); err != nil {
		t.Fatal(err)
	}
	e.Advance(5 * time.Minute)
	if d.State.Booted != "os" {
		t.Fatalf("after the install: booted %q", d.State.Booted)
	}
}

// A pulled drive is absent from every view and its device logs it; inserting it brings it back.
func TestPullAndInsertADrive(t *testing.T) {
	e := virtualNVL(t)
	d := e.M.DeviceBy[tray7]
	c := d.Drives()[1]
	if err := e.PullDrive(c.Path, user); err != nil {
		t.Fatal(err)
	}
	if st := e.DriveStatus(c); st.Present || st.State != "Absent" {
		t.Fatalf("pulled: %+v", st)
	}
	if last := d.State.Log[len(d.State.Log)-1]; !strings.HasSuffix(last.MessageID, "DriveRemoved") {
		t.Fatalf("log after a pull: %+v", last)
	}
	if err := e.InsertDrive(c.Path, user); err != nil {
		t.Fatal(err)
	}
	if st := e.DriveStatus(c); !st.Present || st.State == "Absent" {
		t.Fatalf("inserted: %+v", st)
	}
	// a fault's SMART bits reach the drive's status
	if _, err := e.Inject(c.Path, "generic/storage/predictive-failure", nil, "", user); err != nil {
		t.Fatal(err)
	}
	if st := e.DriveStatus(c); !st.FailurePredicted || st.CriticalWarning&SMARTReliability == 0 || st.Health != model.Warning {
		t.Fatalf("predictive failure: %+v", st)
	}
}

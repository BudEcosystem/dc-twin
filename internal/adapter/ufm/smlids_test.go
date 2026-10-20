package ufm

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/adapter/ibsim/smstate"
)

// fakeSM stands in for the ibsim adapter: the LIDs a subnet manager assigned, by GUID.
type fakeSM struct {
	ports map[string]smstate.Port
	ok    bool
	asked []string
}

func (f *fakeSM) Ports(fabric string) (map[string]smstate.Port, bool) {
	f.asked = append(f.asked, fabric)
	return f.ports, f.ok
}

type lidPort struct {
	GUID     string `json:"guid"`
	SystemID string `json:"systemID"`
	LID      int    `json:"lid"`
	PeerGUID string `json:"peer_guid"`
	PeerLID  int    `json:"peer_lid"`
	Desc     string `json:"description"`
}

func getPorts(t *testing.T, cred Credential) []lidPort {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, _ := http.NewRequest(http.MethodGet, cred.URL+"/ufmRest/resources/ports", nil)
	req.SetBasicAuth(cred.User, cred.Password)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []lidPort
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestLIDsFromTheSubnetManager: with a subnet manager's state registered for the twin (the ibsim
// adapter running the fabric, D47), UFM REST serves the LIDs the SM assigned, a switch port its
// switch's and a port the SM has not reached 0; with none registered, or one that reports no
// SM yet, it serves the LIDs the twin assigned, unchanged by the interlude.
func TestLIDsFromTheSubnetManager(t *testing.T) {
	tw := upLab(t, "ufm")
	a := tw.Adapter("ufm").(*Adapter)
	cred := a.Credentials()[0]
	own := getPorts(t, cred)
	ownLID := map[string]int{}
	for _, p := range own {
		ownLID[p.GUID] = p.LID
	}

	sm := &fakeSM{ports: map[string]smstate.Port{}, ok: true}
	unreached := ""
	next := 1000
	for _, p := range own {
		key := p.GUID // a channel adapter port's GUID; a switch port is its system's
		if !strings.HasPrefix(p.Desc, "Computer") {
			key = p.SystemID
		}
		if _, seen := sm.ports[key]; seen {
			continue
		}
		if unreached == "" && strings.HasPrefix(p.Desc, "Computer") {
			unreached = key // the SM has not reached this one: not in its table
			continue
		}
		sm.ports[key] = smstate.Port{LID: next, SMLID: 1000, State: "Active", Phys: "LinkUp"}
		next++
	}
	smstate.Register(tw.Engine, sm)
	got := getPorts(t, cred)
	if len(sm.asked) == 0 || sm.asked[0] != "backend-ib" {
		t.Fatalf("the source was asked for %v", sm.asked)
	}
	for _, p := range got {
		key := p.GUID
		if !strings.HasPrefix(p.Desc, "Computer") {
			key = p.SystemID
		}
		if p.LID != sm.ports[key].LID {
			t.Fatalf("%s (%s): lid %d, the SM's %d", p.GUID, p.Desc, p.LID, sm.ports[key].LID)
		}
	}
	for _, p := range got {
		if p.GUID == unreached && p.LID != 0 {
			t.Fatalf("a port the SM has not reached reports lid %d", p.LID)
		}
		if p.PeerGUID != "" && p.PeerLID == ownLID[p.PeerGUID] && p.PeerLID != 0 && p.PeerLID < 1000 {
			t.Fatalf("peer_lid of %s is the twin's own (%d)", p.GUID, p.PeerLID)
		}
	}
	if line := a.Report().Lines[0]; !strings.Contains(line, "LIDs the subnet manager assigned, read from ibsim") {
		t.Fatalf("report: %s", line)
	}
	t.Logf("with the SM's state: %d ports, e.g. %s lid %d (the twin's own %d)", len(got), got[0].GUID, got[0].LID, ownLID[got[0].GUID])

	sm.ok = false // ibsim runs, but no SM has assigned a LID yet
	for _, p := range getPorts(t, cred) {
		if p.LID != ownLID[p.GUID] {
			t.Fatalf("no SM yet: %s lid %d, the twin's %d", p.GUID, p.LID, ownLID[p.GUID])
		}
	}
	smstate.Unregister(tw.Engine, sm)
	for _, p := range getPorts(t, cred) {
		if p.LID != ownLID[p.GUID] {
			t.Fatalf("unregistered: %s lid %d, the twin's %d", p.GUID, p.LID, ownLID[p.GUID])
		}
	}
	if line := a.Report().Lines[0]; !strings.Contains(line, "LIDs assigned by the twin") {
		t.Fatalf("report: %s", line)
	}
}

package ibsim

import (
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/adapter/ibsim/smstate"
)

// dumpCapture is ibsim 0.12's console Dump, captured after `opensm --once` configured a switch
// and two channel adapters and one of them was then unlinked (most of the switch's 64 unlinked
// ports are left out).
const dumpCapture = "# Net status - Mon Oct 19 14:48:08 2026\n" +
	"\n" +
	"Ca 1 \"H-b83fd2c6aa950d28\"\tnodeguid b83fd2c6aa950d28\tsysimgguid b83fd2c6aa950d28\n" +
	"b83fd2c6aa950d29\t[1]\t\"S-fc6a1c0343dddb5e\"[64]\t lid 1 lmc 0 smlid 1  4x 100G Active/LinkUp\n" +
	"\n" +
	"Switch 64 \"S-fc6a1c0343dddb5e\"\tnodeguid fc6a1c0343dddb5e\tsysimgguid fc6a1c0343dddb5e\n" +
	"#\tlinearcap 49152 FDBtop 3 portchange 1\n" +
	"#\tForwarding table 0-3: [0]FF [1]40 [2]0 [3]7\n" +
	"fc6a1c0343dddb5e\t[0]\t\"Sma Port\"[0]\t lid 2 lmc 0 smlid 1  4x  2.5G Active/LinkUp\n" +
	"fc6a1c0343dddb5e\t[1]\t\t\t  4x  2.5G Down/Polling\n" +
	"fc6a1c0343dddb5e\t[7]\t\t\t  4x 100G Down/Polling\n" +
	"fc6a1c0343dddb5e\t[63]\t\t\t  4x  2.5G Down/Polling\n" +
	"fc6a1c0343dddb5e\t[64]\t\"H-b83fd2c6aa950d28\"[1]\t  4x 100G Active/LinkUp\n" +
	"\n" +
	"Ca 1 \"H-b83fd286a49f9170\"\tnodeguid b83fd286a49f9170\tsysimgguid b83fd286a49f9170\n" +
	"b83fd286a49f9171\t[1]\t\t\t lid 3 lmc 0 smlid 1  4x 100G Down/Polling\n" +
	"#  dumped 3 nodes\n"

func TestParseDump(t *testing.T) {
	got := parseDump(dumpCapture)
	want := map[string]smstate.Port{
		"b83fd2c6aa950d29": {LID: 1, SMLID: 1, State: "Active", Phys: "LinkUp"}, // the SM's CA port
		"fc6a1c0343dddb5e": {LID: 2, SMLID: 1, State: "Active", Phys: "LinkUp"}, // the switch, by node GUID
		"b83fd286a49f9171": {LID: 3, SMLID: 1, State: "Down", Phys: "Polling"},  // unlinked, keeps its LID
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d ports: %+v", len(got), got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("%s: %+v, want %+v", k, got[k], w)
		}
	}
}

package ibsim

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/ibsim/smstate"
)

// dumpTTL is how long one Dump of a fabric answers Ports: UFM REST sees what the subnet
// manager did within half a second, and a client polling UFM does not make ibsim print the
// whole fabric for every request.
const dumpTTL = 500 * time.Millisecond

// Ports is the adapter's smstate.Source: the LIDs, SM LIDs and port states ibsim holds for a
// fabric, which the stack's subnet manager set. It reads them with ibsim's console `Dump`
// (every node and port: `lid N lmc N smlid N … State/Phys`), at most once per dumpTTL. ok is
// false when the fabric does not run under ibsim or no LID has been assigned in it. The map
// is shared between callers until the next Dump: read it, do not change it.
func (a *Adapter) Ports(fabric string) (map[string]smstate.Port, bool) {
	var s *sim
	for _, x := range a.sims {
		if x.topo.Fabric == fabric {
			s = x
		}
	}
	if s == nil || s.proc == nil || !s.proc.running() {
		return nil, false
	}
	s.dumpMu.Lock()
	defer s.dumpMu.Unlock()
	if s.dumped.IsZero() || time.Since(s.dumped) > dumpTTL {
		out, err := s.proc.doQuiet("Dump")
		if err != nil {
			return nil, false
		}
		s.dump, s.dumped = parseDump(out), time.Now()
	}
	for _, p := range s.dump {
		if p.LID > 0 {
			return s.dump, true
		}
	}
	return s.dump, false
}

var (
	dumpNode = regexp.MustCompile(`^(Switch|Ca|Router) \d+ "[^"]*"\tnodeguid ([0-9a-f]+)\t`)
	dumpPort = regexp.MustCompile(`^([0-9a-f]+)\t\[(\d+)\]\t.*\t lid (\d+) lmc \d+ smlid (\d+) .*?(\S+)/(\S+?)(\t#.*)?$`)
)

// parseDump reads the output of ibsim's console `Dump` (sim_cmd.c dump_net and dump_port):
//
//	Switch 64 "S-fc6a1c0343dddb5e"	nodeguid fc6a1c0343dddb5e	sysimgguid fc6a1c0343dddb5e
//	#	linearcap 49152 FDBtop 3 portchange 1
//	fc6a1c0343dddb5e	[0]	"Sma Port"[0]	 lid 2 lmc 0 smlid 1  4x  2.5G Active/LinkUp
//	fc6a1c0343dddb5e	[64]	"H-b83fd2c6aa950d28"[1]	  4x 100G Active/LinkUp
//	Ca 1 "H-b83fd286a49f9170"	nodeguid b83fd286a49f9170	sysimgguid b83fd286a49f9170
//	b83fd286a49f9171	[1]			 lid 3 lmc 0 smlid 1  4x 100G Down/Polling
//
// (an unlinked port keeps the LID the SM gave it; ibsim 0.12 prints NDR as "100G")
//
// A switch's external ports carry no LID (they share port 0's), so a switch is keyed by its
// node GUID with port 0's state, and a channel adapter port by its port GUID.
func parseDump(out string) map[string]smstate.Port {
	ports := map[string]smstate.Port{}
	sw, node := false, ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := dumpNode.FindStringSubmatch(line); m != nil {
			sw, node = m[1] == "Switch", m[2]
			continue
		}
		m := dumpPort.FindStringSubmatch(line)
		if m == nil || node == "" {
			continue
		}
		key := m[1]
		if sw {
			if m[2] != "0" {
				continue
			}
			key = node
		}
		g, err := strconv.ParseUint(key, 16, 64)
		if err != nil {
			continue
		}
		lid, _ := strconv.Atoi(m[3])
		smlid, _ := strconv.Atoi(m[4])
		ports[fmt.Sprintf("%016x", g)] = smstate.Port{LID: lid, SMLID: smlid, State: m[5], Phys: m[6]}
	}
	return ports
}

package ibsim

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/adapter/ufm"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const labEstate = "../../../examples/estates/lab-model.yaml"

func labModel(t *testing.T) *model.Model {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: compile.HostArch()})
	if err != nil {
		t.Fatal(err)
	}
	return res.Model
}

func hex16(s string) string {
	h := strings.TrimPrefix(strings.ToLower(s), "0x")
	if len(h) > 16 {
		h = h[:16]
	}
	return h
}

// TestRenderLab renders the lab estate's rail-optimised InfiniBand fabric and checks it against
// the model: every switch once with its system GUID, every host InfiniBand port as a one-port
// channel adapter with the model's port GUID, every cable with the port numbers the rail
// generator gave it, and the UFM appliance's HCA first, where a subnet manager attaches.
func TestRenderLab(t *testing.T) {
	m := labModel(t)
	if got := Fabrics(m); len(got) != 1 || got[0] != "backend-ib" {
		t.Fatalf("InfiniBand fabrics %v", got)
	}
	topo, err := Render(m, "backend-ib")
	if err != nil {
		t.Fatal(err)
	}
	sw, ca, links := topo.Counts()
	// 5 racks x 4 rails and 2 spines; 36 trays x 4 + 6 HGX servers x 8 + the UFM appliance's
	// ib0; the 193 host links and 20 leaves x 2 spine uplinks
	if sw != 22 || ca != 193 || links != 193+40 {
		t.Fatalf("counts: %d switches, %d CAs, %d links", sw, ca, links)
	}
	if topo.SM == nil || topo.SM.Port != "site-1/hall-a/c03/ufm-1:ib0" || topo.Nodes[0] != topo.SM {
		t.Fatalf("SM attachment %+v", topo.SM)
	}
	if n, s, p := topo.Sizes(); n != 215 || s != 22 || p != 22*65+193 {
		t.Fatalf("sizes %d %d %d", n, s, p)
	}

	var modelLinks int
	for _, l := range m.Links {
		if l.Fabric == "backend-ib" {
			modelLinks++
		}
	}
	if modelLinks != links {
		t.Fatalf("model has %d backend-ib links, topology %d", modelLinks, links)
	}
	byPort := map[string]*Node{}
	for _, n := range topo.Nodes {
		if n.Switch {
			first := firstIBPort(m.DeviceBy[n.Device])
			if fmt.Sprintf("%016x", n.GUID) != hex16(first.GUID) || n.PortGUID != n.GUID || n.ID != fmt.Sprintf("S-%016x", n.GUID) {
				t.Fatalf("switch %s: GUID %x, model %s", n.Device, n.GUID, first.GUID)
			}
			if n.Ports != 64 {
				t.Fatalf("switch %s has %d ports", n.Device, n.Ports)
			}
			continue
		}
		p := m.Ports[n.Port]
		if fmt.Sprintf("0x%016x", n.PortGUID) != strings.ToLower(p.GUID) || n.GUID != n.PortGUID-1 || n.Ports != 1 {
			t.Fatalf("CA %s: node %x port %x, model %s", n.Port, n.GUID, n.PortGUID, p.GUID)
		}
		byPort[n.Port] = n
	}
	// a01's tray 7: cx8-0 on rail 1 (backend-ib-rail-a01-1 port 7), cx8-3 on rail 4
	l := linkOf(topo, "site-1/hall-a/a01/compute-tray-07:cx8-0")
	if l == nil || l.A.Node.Device != "site-1/hall-a/c03/backend-ib-rail-a01-1" || l.A.Port != 7 || l.B.Port != 1 || l.Speed != "NDR" || l.ModelSpeed != "NDR" {
		t.Fatalf("tray 7 cx8-0: %+v", l)
	}
	if n := byPort["site-1/hall-a/a01/compute-tray-07:cx8-3"]; n == nil || n.Desc != "a01-compute-tray-07 HCA-4" {
		t.Fatalf("tray 7 cx8-3: %+v", n)
	}
	if ufmLink := linkOf(topo, "site-1/hall-a/c03/ufm-1:ib0"); ufmLink == nil || ufmLink.A.Port != 64 || ufmLink.A.Node.Desc != "c03-backend-ib-rail-a01-1" {
		t.Fatalf("UFM appliance link %+v", ufmLink)
	}
	// identity/v1 fills a GUID to 64 bits after the profile's prefix (the QM9700's four bytes
	// included), so no model GUID needs cutting and the topology says nothing about it
	if len(topo.Notes) != 0 {
		t.Fatalf("notes %q", topo.Notes)
	}
	t.Logf("backend-ib: %d switches, %d CAs, %d links; SM at %s; notes %q", sw, ca, links, topo.SM.ID, topo.Notes)
}

func linkOf(t *Topology, ref string) *Link {
	for _, l := range t.Links {
		if l.A.Ref == ref || l.B.Ref == ref {
			return l
		}
	}
	return nil
}

// TestTextIsWhatIbsimParses reads the rendered file back with the rules of ibsim's parser
// (sim_net.c: a record per node, `[port]` lines until a line that is not one, GUID directives
// before a node, 256-byte lines) and checks that every cable appears at both of its ends with
// the same port numbers, and that the file is ibnetdiscover's format.
func TestTextIsWhatIbsimParses(t *testing.T) {
	topo, err := Render(labModel(t), "backend-ib")
	if err != nil {
		t.Fatal(err)
	}
	text := topo.Text()
	type rec struct {
		kind             string
		ports            int
		guid, sys, portG uint64
		desc             string
		peers            map[int]string // port -> "remote[port]"
	}
	recs := map[string]*rec{}
	var order []string
	header := regexp.MustCompile(`^(Switch|Ca)\t(\d+) "([^"]+)"\t\t# "([^"]*)"`)
	swPort := regexp.MustCompile(`^\[(\d+)\]\t"([^"]+)"\[(\d+)\](\(([0-9a-f]+)\))? \t\t# "[^"]*" lid 0 4x(SDR|DDR|QDR|FDR|EDR|HDR|NDR)$`)
	caPort := regexp.MustCompile(`^\[1\]\(([0-9a-f]+)\) \t"([^"]+)"\[(\d+)\]\t\t# lid 0 lmc 0 "[^"]*" lid 0 4x(SDR|DDR|QDR|FDR|EDR|HDR|NDR)$`)
	var cur *rec
	var guid, sys uint64
	sc := bufio.NewScanner(bytes.NewReader(text))
	for sc.Scan() {
		line := sc.Text()
		if len(line) >= 255 {
			t.Fatalf("line longer than ibsim reads: %q", line)
		}
		switch {
		case line == "":
			cur = nil
		case strings.HasPrefix(line, "#"):
			if cur != nil {
				t.Fatalf("comment inside a record: %q", line)
			}
		case strings.HasPrefix(line, "switchguid=0x"), strings.HasPrefix(line, "caguid=0x"):
			v := line[strings.Index(line, "=")+1:]
			if i := strings.Index(v, "("); i > 0 {
				v = v[:i]
			}
			guid, _ = strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 64)
		case strings.HasPrefix(line, "sysimgguid=0x"):
			sys, _ = strconv.ParseUint(strings.TrimPrefix(line, "sysimgguid=0x"), 16, 64)
		case header.MatchString(line):
			h := header.FindStringSubmatch(line)
			n, _ := strconv.Atoi(h[2])
			cur = &rec{kind: h[1], ports: n, guid: guid, sys: sys, desc: h[4], peers: map[int]string{}}
			if cur.sys == 0 {
				cur.sys = guid
			}
			if recs[h[3]] != nil {
				t.Fatalf("node %s twice", h[3])
			}
			recs[h[3]] = cur
			order = append(order, h[3])
			guid, sys = 0, 0
		case cur != nil && cur.kind == "Switch" && swPort.MatchString(line):
			p := swPort.FindStringSubmatch(line)
			n, _ := strconv.Atoi(p[1])
			cur.peers[n] = p[2] + "[" + p[3] + "]"
		case cur != nil && cur.kind == "Ca" && caPort.MatchString(line):
			p := caPort.FindStringSubmatch(line)
			cur.portG, _ = strconv.ParseUint(p[1], 16, 64)
			cur.peers[1] = p[2] + "[" + p[3] + "]"
		default:
			t.Fatalf("a line ibsim would not parse as intended: %q", line)
		}
	}
	if len(recs) != 215 || order[0] != topo.SM.ID {
		t.Fatalf("%d records, first %s", len(recs), order[0])
	}
	cables := 0
	for id, r := range recs {
		if r.guid == 0 || id != fmt.Sprintf("%s-%016x", map[string]string{"Switch": "S", "Ca": "H"}[r.kind], r.guid) {
			t.Fatalf("%s: GUID directive %x", id, r.guid)
		}
		if r.kind == "Ca" && r.portG != r.guid+1 {
			t.Fatalf("%s: port GUID %x is not node GUID + 1 (what ibsim answers)", id, r.portG)
		}
		for port, peer := range r.peers {
			cables++
			rid, rport, _ := strings.Cut(strings.TrimSuffix(peer, "]"), "[")
			back := recs[rid]
			if back == nil || back.peers[atoi(rport)] != fmt.Sprintf("%s[%d]", id, port) {
				t.Fatalf("%s[%d] -> %s does not come back", id, port, peer)
			}
		}
	}
	if cables != 2*(193+40) {
		t.Fatalf("%d cable ends", cables)
	}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

// TestTopologyMatchesUFM brings the lab estate up with the ufm adapter and checks that the
// subnet manager's view (the rendered topology) and UFM REST's describe the same fabric: every
// port UFM lists is in the topology with its port GUID, number and node description; a host's
// system GUID is its CAs' system image GUID; a switch's system GUID is the switch's GUID.
func TestTopologyMatchesUFM(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{"ufm"}, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	var topo *Topology
	tw.Engine.Read(func(m *model.Model) { topo, err = Render(m, "backend-ib") })
	if err != nil {
		t.Fatal(err)
	}
	cred := tw.Adapter("ufm").(*ufm.Adapter).Credentials()[0]
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, _ := http.NewRequest(http.MethodGet, cred.URL+"/ufmRest/resources/ports", nil)
	req.SetBasicAuth(cred.User, cred.Password)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var ports []struct {
		GUID            string `json:"guid"`
		Number          int    `json:"number"`
		SystemID        string `json:"systemID"`
		NodeDescription string `json:"node_description"`
		Description     string `json:"description"`
		LogicalState    string `json:"logical_state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ports); err != nil {
		t.Fatal(err)
	}
	cas := map[string]*Node{}
	switches := map[string]*Node{}
	for _, n := range topo.Nodes {
		if n.Switch {
			switches[fmt.Sprintf("%016x", n.GUID)] = n
		} else {
			cas[fmt.Sprintf("%016x", n.PortGUID)] = n
		}
	}
	var nCA, nSW int
	var wide string
	for _, p := range ports {
		if p.Description == "Computer IB Port" {
			n := cas[p.GUID]
			if n == nil || p.Number != 1 || p.NodeDescription != n.Desc || p.SystemID != fmt.Sprintf("%016x", n.SysImage) {
				t.Fatalf("UFM computer port %+v; topology %+v", p, n)
			}
			nCA++
			continue
		}
		n := switches[hex16(p.SystemID)]
		if n == nil || p.NodeDescription != n.Desc || p.Number < 1 || p.Number > n.Ports {
			t.Fatalf("UFM switch port %+v; topology %+v", p, n)
		}
		if len(p.SystemID) != 16 {
			t.Fatalf("switch system GUID %s is not 64 bits", p.SystemID)
		}
		wide = p.SystemID
		nSW++
	}
	if nCA != 193 || nSW != 193+2*40 {
		t.Fatalf("UFM reports %d computer and %d switch ports", nCA, nSW)
	}
	t.Logf("UFM REST and the topology agree on %d computer ports and %d switch ports; switch system GUIDs are 64-bit (e.g. %s)", nCA, nSW, wide)
}

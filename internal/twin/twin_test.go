package twin

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

func TestLabEstateComesUp(t *testing.T) {
	tw, err := Up("../../examples/estates/lab-model.yaml", Options{StateDir: t.TempDir(), Adapters: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	st := tw.Engine.Status()
	t.Logf("devices=%v power=%v health=%v domains=%v", st["devices"], st["power"], st["health"], st["domains"])
	if st["devices"].(int) < 200 {
		t.Fatalf("too few devices: %v", st["devices"])
	}
}

// An estate with a stack gets stack-inputs/expected-machines.json (D38): every BMC, and on each
// NVLink switch tray the MAC of its NVOS management port (mgmt0), which is not the BMC's (D46).
func TestStackInputsCarryNVOSMACs(t *testing.T) {
	dir := t.TempDir()
	tw, err := Up("../../examples/estates/nvl72-nico.yaml", Options{StateDir: dir, Adapters: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	b, err := os.ReadFile(filepath.Join(dir, "stack-inputs", "expected-machines.json"))
	if err != nil {
		t.Fatal(err)
	}
	var list []ExpectedMachine
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	devs := map[string]*model.Device{}
	tw.Engine.Read(func(m *model.Model) { devs = m.DeviceBy })
	trays, others := 0, 0
	for _, e := range list {
		d := devs[e.Path]
		if d == nil {
			t.Fatalf("%s is not in the model", e.Path)
		}
		if e.BMCMAC == "" || e.BMCMAC != d.Identity.BMCMAC || e.Serial != d.Identity.Serial || e.User == "" {
			t.Errorf("%s: %+v", e.Path, e)
		}
		if d.BaseClass != "switch-tray" {
			if len(e.NVOSMACs) != 0 {
				t.Errorf("%s (%s) lists NVOS MACs %v", e.Path, d.BaseClass, e.NVOSMACs)
			}
			others++
			continue
		}
		trays++
		mgmt := d.PortBy["mgmt0"]
		if len(e.NVOSMACs) != 1 || e.NVOSMACs[0] != mgmt.MAC {
			t.Errorf("%s: nvosMACs %v, want [%s] (mgmt0)", e.Path, e.NVOSMACs, mgmt.MAC)
		}
		if len(e.NVOSMACs) > 0 && e.NVOSMACs[0] == e.BMCMAC {
			t.Errorf("%s: the NVOS MAC is the BMC's", e.Path)
		}
	}
	if trays != 9 {
		t.Errorf("%d switch trays in expected-machines.json, want the NVL72 rack's 9", trays)
	}
	t.Logf("%d expected machines: %d switch trays with an NVOS MAC, %d other BMCs", len(list), trays, others)
	// the raw file names the field as documented
	var raw []map[string]any
	_ = json.Unmarshal(b, &raw)
	for _, r := range raw {
		if r["path"] == "site-1/hall-a/a03/switch-tray-01" {
			if _, ok := r["nvosMACs"]; !ok {
				t.Errorf("a switch tray's entry has no nvosMACs field: %v", r)
			}
			t.Logf("e.g. %v", r)
		}
	}
}

// The site gateway routes a stack process to the stack's own services at the site addresses its
// outputs name (siteServices), as a vm guest's segment gateway does, whatever adapters run: DPF
// and a DPU's Arm name the DPU cluster's API by one address. It routes nothing else, and the
// services are not estate routes. A malformed output refuses up.
func TestSiteGatewayRoutesStackServices(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	tw, err := Up("../../examples/estates/lab-model.yaml", Options{StateDir: t.TempDir(), Adapters: []string{},
		Outputs: map[string]string{"siteServices": "dpu-api@10.90.0.11:443=" + echo.Addr().String() + " tftp@udp:10.90.0.17:69=127.0.0.1:1069"}})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	connect := func(target string) (int, net.Conn, *bufio.Reader) {
		t.Helper()
		c, err := net.Dial("tcp", tw.Net.Gateway)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, c, br
	}
	code, c, br := connect("10.90.0.11:443")
	defer c.Close()
	if code != 200 {
		t.Fatalf("CONNECT to the stack's dpu-api at its site address: %d", code)
	}
	c.Write([]byte("ping"))
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		t.Fatalf("relayed %q, %v", got, err)
	}
	if code, c2, _ := connect("10.90.0.12:443"); code != http.StatusBadGateway {
		t.Fatalf("CONNECT to an address no service or endpoint has: %d", code)
	} else {
		c2.Close()
	}
	if code, c3, _ := connect("10.90.0.17:69"); code != http.StatusBadGateway {
		t.Fatalf("CONNECT to a UDP service's address: %d", code)
	} else {
		c3.Close()
	}
	if _, ok := tw.Net.Routes()["10.90.0.11:443"]; ok {
		t.Fatal("a stack service became an estate route")
	}
	if rep := strings.Join(tw.Net.SubstrateIsNot(), "\n"); !strings.Contains(rep, "The site gateway routes a stack process's TCP to the stack's own services") ||
		!strings.Contains(rep, "dpu-api@10.90.0.11:443="+echo.Addr().String()) || strings.Contains(rep, "tftp@") {
		t.Fatalf("the substrate report does not say the gateway routes the services: %v", tw.Net.SubstrateIsNot())
	}

	if _, err := Up("../../examples/estates/lab-model.yaml", Options{StateDir: t.TempDir(), Adapters: []string{},
		Outputs: map[string]string{"siteServices": "dpu-api@not-an-address=127.0.0.1:1"}}); err == nil || !strings.Contains(err.Error(), "siteServices") {
		t.Fatalf("a malformed siteServices output: %v", err)
	}
}

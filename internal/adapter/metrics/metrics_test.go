package metrics

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	pmodel "github.com/prometheus/common/model"

	"github.com/BudEcosystem/dc-twin/internal/adapter/kwok"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const labEstate = "../../../examples/estates/lab-model.yaml"

var testSrc = schema.EventSource{Class: "user", Principal: "metrics-test"}

func upLab(t *testing.T) (*twin.Twin, *Adapter) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{Adapters: []string{Name}, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	return tw, tw.Adapter(Name).(*Adapter)
}

// Scrape fetches and parses one endpoint with the legacy (strict) name validation.
func Scrape(addr string) (map[string]*dto.MetricFamily, []byte, error) {
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + addr + "/metrics")
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != 200 {
		return nil, body, fmt.Errorf("status %d", resp.StatusCode)
	}
	p := expfmt.NewTextParser(pmodel.LegacyValidation)
	fams, err := p.TextToMetricFamilies(bytes.NewReader(body))
	return fams, body, err
}

func labelOf(m *dto.Metric, name string) string {
	for _, l := range m.Label {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func valueOf(m *dto.Metric) float64 {
	if m.Gauge != nil {
		return m.Gauge.GetValue()
	}
	if m.Counter != nil {
		return m.Counter.GetValue()
	}
	return m.GetUntyped().GetValue()
}

func find(f *dto.MetricFamily, label, value string) *dto.Metric {
	if f == nil {
		return nil
	}
	for _, m := range f.Metric {
		if labelOf(m, label) == value {
			return m
		}
	}
	return nil
}

func endpoint(t *testing.T, a *Adapter, suffix string) *Endpoint {
	t.Helper()
	for _, ep := range a.Endpoints() {
		if strings.HasSuffix(ep.Device.Path, "/"+suffix) {
			return ep
		}
	}
	t.Fatalf("no endpoint for %s", suffix)
	return nil
}

func head(b []byte, n int) string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() && len(out) < n {
		out = append(out, sc.Text())
	}
	return strings.Join(out, "\n")
}

// tempFamily is the per-accelerator temperature each exporter dialect serves, and the label
// that tells accelerators apart.
var tempFamily = map[string][2]string{
	"dcgm":  {"DCGM_FI_DEV_GPU_TEMP", "gpu"},
	"amd":   {"gpu_junction_temperature", "gpu_id"},
	"gaudi": {"habanalabs_temperature_onchip", "id"},
}

func TestEveryExporterServesPerAcceleratorTemperature(t *testing.T) {
	_, a := upLab(t)
	seen := map[string]int{}
	for _, ep := range a.Endpoints() {
		fams, _, err := Scrape(ep.Local)
		if err != nil {
			t.Fatalf("%s: %v", ep.Device.Path, err)
		}
		tf := tempFamily[ep.Exporter.ID]
		f := fams[tf[0]]
		if f == nil || len(f.Metric) != len(ep.accels) {
			t.Fatalf("%s (%s): want %d %s series, got %v", ep.Device.Path, ep.Exporter.ID, len(ep.accels), tf[0], f)
		}
		for _, ai := range ep.accels {
			m := find(f, tf[1], fmt.Sprint(ai.index))
			if m == nil || valueOf(m) < 20 || valueOf(m) > 120 {
				t.Fatalf("%s accelerator %d: temperature %v", ep.Device.Path, ai.index, m)
			}
		}
		seen[ep.Exporter.ID]++
	}
	for _, id := range []string{"dcgm", "amd", "gaudi"} {
		if seen[id] == 0 {
			t.Fatalf("no %s endpoint in the lab estate (served: %v)", id, seen)
		}
	}
	t.Logf("endpoints scraped: %v", seen)
	r := a.Report()
	if len(r.Impersonated) != 3 {
		t.Fatalf("report impersonates %v", r.Impersonated)
	}
	t.Logf("report: %+v", r.Lines)
}

func TestNodeNameMatchesKwok(t *testing.T) {
	tw, _ := upLab(t)
	for _, d := range tw.Engine.M.Devices {
		if d.IsHost() && NodeName(d) != kwok.NodeName(d) {
			t.Fatalf("%s: metrics %q, kwok %q", d.Path, NodeName(d), kwok.NodeName(d))
		}
	}
}

func TestFaultsReachTheExporters(t *testing.T) {
	tw, a := upLab(t)
	e := tw.Engine
	inject := func(sel, fault string) {
		t.Helper()
		if _, err := e.Inject(sel, fault, nil, "", testSrc); err != nil {
			t.Fatal(err)
		}
	}

	// NVIDIA: Xid 79 takes the GPU off the bus; only its error series remain.
	tray := endpoint(t, a, "a01/compute-tray-07")
	inject("a01/compute-tray-07/gpu-2", "nvidia/xid/xid-79")
	fams, body, err := Scrape(tray.Local)
	if err != nil {
		t.Fatal(err)
	}
	m := find(fams["DCGM_FI_DEV_XID_ERRORS"], "gpu", "2")
	if m == nil || valueOf(m) != 79 || labelOf(m, "err_code") != "79" || labelOf(m, "err_msg") != "GPU has fallen off the bus" {
		t.Fatalf("xid series after Xid 79: %v", m)
	}
	if find(fams["DCGM_FI_DEV_GPU_TEMP"], "gpu", "2") != nil || len(fams["DCGM_FI_DEV_GPU_TEMP"].Metric) != 3 {
		t.Fatalf("an off-the-bus GPU still reports temperature")
	}
	if m := find(fams["DCGM_FI_DEV_XID_ERRORS"], "gpu", "1"); m == nil || valueOf(m) != 0 || labelOf(m, "err_msg") != "No Error" {
		t.Fatalf("healthy GPU's xid series: %v", m)
	}
	t.Logf("dcgm-exporter, %s after Xid 79 on gpu-2:\n%s", tray.Site, head(body, 40))

	// NVIDIA: Xid 48 counts a double-bit error.
	h100 := endpoint(t, a, "b01/server-01")
	inject("b01/server-01/gpu-0", "nvidia/xid/xid-48")
	fams, _, err = Scrape(h100.Local)
	if err != nil {
		t.Fatal(err)
	}
	if m := find(fams["DCGM_FI_DEV_ECC_DBE_VOL_TOTAL"], "gpu", "0"); m == nil || valueOf(m) != 1 {
		t.Fatalf("DBE counter after Xid 48: %v", m)
	}
	if m := find(fams["DCGM_FI_DEV_XID_ERRORS"], "gpu", "0"); m == nil || valueOf(m) != 48 {
		t.Fatalf("xid after Xid 48: %v", m)
	}
	if fams["DCGM_FI_DEV_FB_USED"] == nil {
		t.Fatalf("an H100 80GB host serves no framebuffer series")
	}

	// AMD: an uncorrectable RAS error.
	mi := endpoint(t, a, "b03/server-01")
	inject("b03/server-01/gpu-3", "amd/ras/uncorrectable-ecc")
	fams, body, err = Scrape(mi.Local)
	if err != nil {
		t.Fatal(err)
	}
	if m := find(fams["gpu_ecc_uncorrect_total"], "gpu_id", "3"); m == nil || valueOf(m) != 1 {
		t.Fatalf("gpu_ecc_uncorrect_total: %v", m)
	}
	if m := find(fams["gpu_health"], "gpu_id", "3"); m == nil || valueOf(m) != 0 {
		t.Fatalf("gpu_health: %v", m)
	}
	if m := find(fams["gpu_health"], "gpu_id", "0"); m == nil || valueOf(m) != 1 {
		t.Fatalf("gpu_health of a healthy GPU: %v", m)
	}
	t.Logf("amd-device-metrics-exporter, %s after an uncorrectable ECC error on gpu-3:\n%s", mi.Site, head(body, 30))

	// Intel: an HBM uncorrectable error, and the integrated RoCE ports going down.
	g3 := endpoint(t, a, "b04/server-02")
	inject("b04/server-02/hpu-5", "intel/gaudi/hbm-uncorrectable")
	inject("b04/server-02/hpu-1", "intel/gaudi/roce-port-down")
	fams, body, err = Scrape(g3.Local)
	if err != nil {
		t.Fatal(err)
	}
	if m := find(fams["habanalabs_pending_rows_with_double_bit_ecc_errors"], "id", "5"); m == nil || valueOf(m) != 1 {
		t.Fatalf("pending DBE rows: %v", m)
	}
	down, up := 0, 0
	for _, m := range fams["habanalabs_nic_port_status"].Metric {
		if labelOf(m, "id") == "1" && labelOf(m, "port_type") == "internal" {
			if valueOf(m) == 0 {
				down++
			} else {
				up++
			}
		}
	}
	if down != 21 || up != 0 {
		t.Fatalf("hpu-1 internal ports: %d down, %d up", down, up)
	}
	t.Logf("gaudi-metric-exporter, %s after HBM UE on hpu-5 and RoCE down on hpu-1:\n%s", g3.Site, head(body, 30))

	// A host that is powered off has no exporter: the scrape fails.
	off := endpoint(t, a, "b05/server-01")
	if err := e.Power(engine.Request{Path: off.Device.Path, Canonical: "power.off.force", Source: testSrc}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Scrape(off.Local); err == nil {
		t.Fatalf("a powered-off host's exporter answered")
	} else {
		t.Logf("scrape of powered-off %s: %v", off.Device.Path, err)
	}
}

package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

// Exporter is one exporter dialect: whose names it serves, at which pinned release, and how it
// orders a scrape. Metric and label names are interface facts taken from each exporter's
// source at the release named here; the comments on each family cite where.
type Exporter struct {
	ID           string // the profile's metrics.exporter value
	Impersonator string // the name the fidelity report prints
	Upstream     string
	Version      string
	Port         int    // the exporter's default listen port (the site name uses it)
	serialFormat string // identity/v1 serial format for per-accelerator serials
	render       func(io.Writer, *snapshot) error
}

// Exporters are the dialects the twin projects, by profile metrics.exporter value.
var Exporters = map[string]*Exporter{
	"dcgm": {ID: "dcgm", Impersonator: "dcgm-exporter", Upstream: "NVIDIA/dcgm-exporter", Version: "4.8.4",
		Port: 9400, serialFormat: "{digits:13}", render: renderDCGM},
	"amd": {ID: "amd", Impersonator: "amd-device-metrics-exporter", Upstream: "ROCm/device-metrics-exporter", Version: "v1.5.3",
		Port: 5000, serialFormat: "{hex:16}", render: renderAMD},
	"gaudi": {ID: "gaudi", Impersonator: "gaudi-metric-exporter", Upstream: "HabanaAI/gaudi-metric-exporter", Version: "1.24.2-481",
		Port: 41611, serialFormat: "AN{digits:8}", render: renderGaudi},
}

// ---------------------------------------------------------------- rendering

type label struct{ name, value string }

type sample struct {
	labels []label
	value  float64
}

type family struct {
	name, help string
	typ        dto.MetricType
	samples    []sample
}

func (f *family) add(v float64, labels ...label) {
	f.samples = append(f.samples, sample{labels: labels, value: v})
}

// write renders families in the exposition format through expfmt, the encoder both
// dcgm-exporter (internal/pkg/rendermetrics/render_metrics.go) and client_golang use. Families
// are sorted by name, as both do. With sortLabels (client_golang's MakeLabelPairs, which the AMD
// and Gaudi exporters go through) label pairs are sorted by name; without it (dcgm-exporter's
// renderer) they keep the renderer's order. Samples are sorted by their label signature.
func write(w io.Writer, fams []*family, sortLabels bool) error {
	sort.Slice(fams, func(i, j int) bool { return fams[i].name < fams[j].name })
	for _, f := range fams {
		if len(f.samples) == 0 {
			continue
		}
		mf := &dto.MetricFamily{Name: ptr(f.name), Help: ptr(f.help), Type: f.typ.Enum()}
		type keyed struct {
			sig string
			m   *dto.Metric
		}
		var ms []keyed
		for _, s := range f.samples {
			ls := append([]label{}, s.labels...)
			if sortLabels {
				sort.SliceStable(ls, func(i, j int) bool { return ls[i].name < ls[j].name })
			}
			m := &dto.Metric{}
			var sig strings.Builder
			for _, l := range ls {
				m.Label = append(m.Label, &dto.LabelPair{Name: ptr(l.name), Value: ptr(l.value)})
				sig.WriteString(l.name + "=" + l.value + "\xff")
			}
			v := s.value
			switch f.typ {
			case dto.MetricType_COUNTER:
				m.Counter = &dto.Counter{Value: &v}
			default:
				m.Gauge = &dto.Gauge{Value: &v}
			}
			ms = append(ms, keyed{sig.String(), m})
		}
		sort.SliceStable(ms, func(i, j int) bool { return ms[i].sig < ms[j].sig })
		for _, k := range ms {
			mf.Metric = append(mf.Metric, k.m)
		}
		if _, err := expfmt.MetricFamilyToText(w, mf); err != nil {
			return err
		}
	}
	return nil
}

func ptr(s string) *string { return &s }

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// ---------------------------------------------------------------- dcgm-exporter

// xidText is dcgm-exporter's err_msg for the Xids the twin's catalogues can raise, verbatim
// from NVIDIA/dcgm-exporter 4.8.4 internal/pkg/collector/xid_errors.go (Apache-2.0); any other
// code is "Unknown Error" there too (gpu_collector.go, unknownErr).
var xidText = map[int]string{
	0:  "No Error",
	13: "Graphics Engine Exception",
	31: "GPU memory page fault",
	48: "Double Bit ECC Error",
	63: "ECC page retirement or row remapping recording event",
	64: "ECC page retirement or row remapper recording failure",
	74: "NVLINK Error",
	79: "GPU has fallen off the bus",
	92: "High single-bit ECC error rate",
	94: "Contained ECC error",
	95: "Uncontained ECC error",
}

// renderDCGM serves dcgm-exporter 4.8.4's shape.
//
// Names, types and HELP: etc/default-counters.csv at tag 4.8.4. The ECC volatile totals are in
// that file but commented out; they are served as a counters file that enables them would.
// Labels, in order: gpu, UUID, pci_bus_id, device ("nvidia<gpu>"), modelName, hostname
// (internal/pkg/rendermetrics/render_metrics.go metricLabels for FE_GPU; the label is
// "hostname" since #655, "Hostname" before 4.6.0-4.8.3), then collector attributes sorted by
// name: err_code and err_msg on DCGM_FI_DEV_XID_ERRORS (internal/pkg/collector/gpu_collector.go).
// The DCGM_FI_DRIVER_VERSION label is omitted: no driver is modelled.
func renderDCGM(w io.Writer, s *snapshot) error {
	temp := &family{name: "DCGM_FI_DEV_GPU_TEMP", help: "GPU temperature (in C).", typ: dto.MetricType_GAUGE}
	power := &family{name: "DCGM_FI_DEV_POWER_USAGE", help: "Power draw (in W).", typ: dto.MetricType_GAUGE}
	util := &family{name: "DCGM_FI_DEV_GPU_UTIL", help: "GPU utilization (in %).", typ: dto.MetricType_GAUGE}
	xid := &family{name: "DCGM_FI_DEV_XID_ERRORS", help: "Value of the last XID error encountered.", typ: dto.MetricType_GAUGE}
	fbFree := &family{name: "DCGM_FI_DEV_FB_FREE", help: "Framebuffer memory free (in MiB).", typ: dto.MetricType_GAUGE}
	fbUsed := &family{name: "DCGM_FI_DEV_FB_USED", help: "Framebuffer memory used (in MiB).", typ: dto.MetricType_GAUGE}
	sbe := &family{name: "DCGM_FI_DEV_ECC_SBE_VOL_TOTAL", help: "Total number of single-bit volatile ECC errors.", typ: dto.MetricType_COUNTER}
	dbe := &family{name: "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", help: "Total number of double-bit volatile ECC errors.", typ: dto.MetricType_COUNTER}
	nvl := &family{name: "DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL", help: "Aggregate NVLink throughput across all lanes (in MB/s).", typ: dto.MetricType_GAUGE}
	for _, a := range s.accels {
		gpu := strconv.Itoa(a.id.index)
		ls := []label{
			{"gpu", gpu},
			{"UUID", "GPU-" + a.id.uuid},
			// The bus numbering the fault catalogue's {pci} uses for kernel-log lines.
			{"pci_bus_id", fmt.Sprintf("00000000:%02X:00.0", 0x18+a.id.index*0x10)},
			{"device", "nvidia" + gpu},
			{"modelName", a.id.c.Model},
			{"hostname", s.ep.Node},
		}
		code := a.xid
		msg, ok := xidText[code]
		if !ok {
			msg = "Unknown Error"
		}
		xid.add(float64(code), append(append([]label{}, ls...), label{"err_code", strconv.Itoa(code)}, label{"err_msg", msg})...)
		sbe.add(a.counters["ecc.volatile.sbe"], ls...)
		dbe.add(a.counters["ecc.volatile.dbe"], ls...)
		if a.offline {
			continue // NVML answers GPU_IS_LOST; DCGM's blank values are skipped by the exporter
		}
		if s.hasTemp {
			temp.add(s.temp, ls...)
		}
		if a.hasWatts {
			power.add(round2(a.watts), ls...)
		}
		util.add(math.Round(s.util*100), ls...)
		if a.id.memMiB > 0 {
			used := math.Round(a.id.memMiB * s.util)
			fbUsed.add(used, ls...)
			fbFree.add(a.id.memMiB-used, ls...)
		}
		for _, p := range a.ports {
			if p.kind == "scale-up" {
				nvl.add(0, ls...) // NVLink lanes carry state only: no traffic is modelled
				break
			}
		}
	}
	return write(w, []*family{temp, power, util, xid, fbFree, fbUsed, sbe, dbe, nvl}, false)
}

// ---------------------------------------------------------------- AMD Device Metrics Exporter

// renderAMD serves the AMD Device Metrics Exporter v1.5.3's shape.
//
// Names, HELP and types (all gauges): pkg/amdgpu/gpuagent/gpuagent_gpu_metrics.go
// initPrometheusMetrics; which fields an MI2xx or MI3xx part reports:
// docs/configuration/metricslist.md. Default labels are the mandatory set (gpuMandatoryLables
// in the same file): gpu_id, card_model, gpu_partition_id, gpu_compute_partition_type,
// gpu_memory_partition_type, deployment_mode, serial_number, pod, namespace, container, job_id,
// job_user, job_partition, hostname, lower-cased (GetExportLabels); values per
// populateLabelsFromGPU and utils.NormalizeStringWithoutPrefix (an unpartitioned bare-metal
// GPU: partition 0, "spx", "nps1", "baremetal"). Workload labels are empty: no pod runs.
// gpu_nodes_total carries only the non-GPU label, hostname (GetExporterNonGPULabels).
func renderAMD(w io.Writer, s *snapshot) error {
	nodes := &family{name: "gpu_nodes_total", help: "Number of GPUs in the node", typ: dto.MetricType_GAUGE}
	edge := &family{name: "gpu_edge_temperature", help: "Current edge temperature in Celsius", typ: dto.MetricType_GAUGE}
	junction := &family{name: "gpu_junction_temperature", help: "Current junction/hotspot temperature in Celsius", typ: dto.MetricType_GAUGE}
	pkg := &family{name: "gpu_package_power", help: "Current socket power in Watts", typ: dto.MetricType_GAUGE}
	avgPkg := &family{name: "gpu_average_package_power", help: "Average socket power in Watts", typ: dto.MetricType_GAUGE}
	usage := &family{name: "gpu_power_usage", help: "GPU Power usage in Watts", typ: dto.MetricType_GAUGE}
	gfx := &family{name: "gpu_gfx_activity", help: "Graphics engine usage in Percentage (0-100)", typ: dto.MetricType_GAUGE}
	vramTotal := &family{name: "gpu_total_vram", help: "Total VRAM memory of the GPU (in MB)", typ: dto.MetricType_GAUGE}
	vramUsed := &family{name: "gpu_used_vram", help: "Used VRAM memory of the GPU (in MB)", typ: dto.MetricType_GAUGE}
	eccCorr := &family{name: "gpu_ecc_correct_total", help: "Total Correctable error count", typ: dto.MetricType_GAUGE}
	eccUncorr := &family{name: "gpu_ecc_uncorrect_total", help: "Total Uncorrectable error count", typ: dto.MetricType_GAUGE}
	eccUMC := &family{name: "gpu_ecc_uncorrect_umc", help: "Uncorrectable error count in UMC block", typ: dto.MetricType_GAUGE}
	health := &family{name: "gpu_health", help: "Health of the GPU (0 = Unhealthy | 1 = Healthy)", typ: dto.MetricType_GAUGE}

	nodes.add(float64(len(s.accels)), label{"hostname", s.ep.Node})
	for _, a := range s.accels {
		ls := []label{
			{"gpu_id", strconv.Itoa(a.id.index)},
			{"card_model", a.id.c.Model},
			{"gpu_partition_id", "0"},
			{"gpu_compute_partition_type", "spx"},
			{"gpu_memory_partition_type", "nps1"},
			{"deployment_mode", "baremetal"},
			{"serial_number", a.id.serial},
			{"pod", ""}, {"namespace", ""}, {"container", ""},
			{"job_id", ""}, {"job_user", ""}, {"job_partition", ""},
			{"hostname", s.ep.Node},
		}
		ue := a.counters["ras.ue"]
		eccCorr.add(a.counters["ras.ce"], ls...)
		eccUncorr.add(ue, ls...)
		eccUMC.add(ue, ls...) // the RAS fault catalogue's uncorrectable error is in the umc block
		h := 1.0
		if a.health == model.Critical || a.offline {
			h = 0
		}
		health.add(h, ls...)
		if a.offline {
			continue
		}
		// MI3xx parts report junction temperature and package power; MI2xx parts edge
		// temperature and average package power (metricslist.md, Platform Support Summary).
		mi2 := strings.Contains(strings.ToUpper(a.id.c.Model), "MI2")
		if s.hasTemp {
			if mi2 {
				edge.add(s.temp, ls...)
			} else {
				junction.add(s.temp, ls...)
			}
		}
		if a.hasWatts {
			if mi2 {
				avgPkg.add(round2(a.watts), ls...)
			} else {
				pkg.add(round2(a.watts), ls...)
			}
			usage.add(round2(a.watts), ls...)
		}
		gfx.add(math.Round(s.util*100), ls...)
		if a.id.memMiB > 0 {
			vramTotal.add(a.id.memMiB, ls...)
			vramUsed.add(math.Round(a.id.memMiB*s.util), ls...)
		}
	}
	return write(w, []*family{nodes, edge, junction, pkg, avgPkg, usage, gfx, vramTotal, vramUsed, eccCorr, eccUncorr, eccUMC, health}, true)
}

// ---------------------------------------------------------------- Gaudi metric exporter

// renderGaudi serves the Intel Gaudi metric exporter 1.24.2-481's shape.
//
// Names: HabanaAI/gaudi-metric-exporter exporter.go Collect at tag 1.24.2-481, and the list in
// docs.habana.ai Orchestration/Prometheus_Metric_Exporter (1.24.2). Every family is a gauge
// (promMetric uses prometheus.GaugeValue). Labels (getMetricLabels, deviceLabels): Serial, UUID,
// device (the serial again), id (the device index), fit, spi, driver, pod_namespace, pod_name,
// plus port and port_type on habanalabs_nic_port_status (port_type "internal" for the in-node
// scale-up ports, "external" for scale-out: rhlml/hlml/src/device.rs PortType); client_golang
// sorts them by name. fit, spi and driver are firmware and driver versions, which the model does
// not have: empty. The upstream source is GPL-2.0: only names are taken from it, and the HELP
// strings here are the twin's own.
func renderGaudi(w io.Writer, s *snapshot) error {
	onchip := &family{name: "habanalabs_temperature_onchip", help: "ASIC temperature in Celsius.", typ: dto.MetricType_GAUGE}
	slowdown := &family{name: "habanalabs_temperature_threshold_slowdown", help: "Temperature in Celsius at which the device slows down.", typ: dto.MetricType_GAUGE}
	shutdown := &family{name: "habanalabs_temperature_threshold_shutdown", help: "Temperature in Celsius at which the device shuts down.", typ: dto.MetricType_GAUGE}
	power := &family{name: "habanalabs_power_mW", help: "Power usage in milliwatts.", typ: dto.MetricType_GAUGE}
	util := &family{name: "habanalabs_utilization", help: "Device utilization in percent.", typ: dto.MetricType_GAUGE}
	memTotal := &family{name: "habanalabs_memory_total_bytes", help: "Total device memory in bytes.", typ: dto.MetricType_GAUGE}
	memUsed := &family{name: "habanalabs_memory_used_bytes", help: "Used device memory in bytes.", typ: dto.MetricType_GAUGE}
	memFree := &family{name: "habanalabs_memory_free_bytes", help: "Free device memory in bytes.", typ: dto.MetricType_GAUGE}
	ecc := &family{name: "habanalabs_ecc_feature_mode", help: "ECC mode (1 enabled).", typ: dto.MetricType_GAUGE}
	dbe := &family{name: "habanalabs_pending_rows_with_double_bit_ecc_errors", help: "Memory rows pending replacement after double-bit ECC errors.", typ: dto.MetricType_GAUGE}
	sbe := &family{name: "habanalabs_pending_rows_with_single_bit_ecc_errors", help: "Memory rows pending replacement after single-bit ECC errors.", typ: dto.MetricType_GAUGE}
	ports := &family{name: "habanalabs_nic_port_status", help: "Integrated NIC port status (1 up, 0 down).", typ: dto.MetricType_GAUGE}
	config := &family{name: "habanalabs_device_config", help: "Device information (always 1).", typ: dto.MetricType_GAUGE}
	for _, a := range s.accels {
		ls := []label{
			{"Serial", a.id.serial},
			{"UUID", a.id.uuid},
			{"device", a.id.serial},
			{"id", strconv.Itoa(a.id.index)},
			{"fit", ""}, {"spi", ""}, {"driver", ""},
			{"pod_namespace", ""}, {"pod_name", ""},
		}
		// An HBM uncorrectable error leaves its row pending replacement.
		dbe.add(a.counters["hbm.ue"], ls...)
		sbe.add(a.counters["hbm.ce"], ls...)
		if a.offline {
			continue
		}
		config.add(1, ls...)
		ecc.add(1, ls...) // HBM ECC is always on in the model
		if s.hasTemp {
			onchip.add(s.temp, ls...)
		}
		if s.warn != nil {
			slowdown.add(*s.warn, ls...)
		}
		if s.crit != nil {
			shutdown.add(*s.crit, ls...)
		}
		if a.hasWatts {
			power.add(math.Round(a.watts*1000), ls...)
		}
		util.add(math.Round(s.util*100), ls...)
		if a.id.memMiB > 0 {
			total := a.id.memMiB * (1 << 20)
			used := math.Round(total * s.util)
			memTotal.add(total, ls...)
			memUsed.add(used, ls...)
			memFree.add(total-used, ls...)
		}
		// Port numbers follow the component's port order: in-node scale-up ports first, then
		// scale-out, as the profile lists them.
		for i, p := range a.ports {
			typ := "external"
			if p.kind == "scale-up" {
				typ = "internal"
			}
			v := 0.0
			if p.up {
				v = 1
			}
			ports.add(v, append(append([]label{}, ls...), label{"port", strconv.Itoa(i)}, label{"port_type", typ})...)
		}
	}
	return write(w, []*family{onchip, slowdown, shutdown, power, util, memTotal, memUsed, memFree, ecc, dbe, sbe, ports, config}, true)
}

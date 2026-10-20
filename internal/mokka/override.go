package mokka

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

// Mokka's failure modes (pkg/gpu/mocknvml/engine, FailureMode*). A device whose override holds
// one fails its guarded NVML calls from the next one (no after_calls, no probability: "active
// immediately"), and a Xid code with it is queued on the NVML event set, where the device
// plugin's health checker and DCGM read it.
const (
	ModeLost             = "lost"
	ModeFallenOffBus     = "fallen_off_bus"
	ModeECCUncorrectable = "ecc_uncorrectable"
)

// GPUState is one GPU's committed model state: what an override is rendered from.
type GPUState struct {
	Index  int
	Path   string
	Health string // OK, Warning, Critical
	State  string // Enabled, StandbyOffline, UnavailableOffline
	Faults []Fault
}

// Fault is an active fault on the GPU, with its catalogue entry's shim effects for this driver.
type Fault struct {
	ID, Fault, Title string
	Shims            []map[string]any // the bodies of `shim: {driver: nvidia/mokka, …}` effects
}

// ShimsOf returns a catalogue entry's shim effects for this driver.
func ShimsOf(effects []map[string]any) []map[string]any {
	var out []map[string]any
	for _, eff := range effects {
		if body, ok := eff["shim"].(map[string]any); ok && fmt.Sprint(body["driver"]) == ShimDriver {
			out = append(out, body)
		}
	}
	return out
}

// Failure is one device's failure block.
type Failure struct {
	Mode string
	Xid  int
	Why  string // the fault (or model state) it stands for
}

var xidRe = regexp.MustCompile(`(?i)\bxid[- ]?(\d+)\b`)

// xidOf is the Xid a fault stands for: its shim's xid, else the code in its catalogue name
// (nvidia/xid/xid-79) or its title ("… (Xid 79)"), the rule the dcgm-exporter impersonation uses.
func xidOf(f Fault, shim map[string]any) int {
	if v, ok := shim["xid"]; ok {
		if n, err := strconv.Atoi(fmt.Sprint(v)); err == nil {
			return n
		}
	}
	for _, s := range []string{f.Fault[strings.LastIndexByte(f.Fault, '/')+1:], f.Title} {
		if m := xidRe.FindStringSubmatch(s); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

// FailureOf is what Mokka is to report for one GPU, from the model, or nil for a healthy one.
//
// The catalogue is the pack's statement of what each fault is to the shim, so a fault's shim
// effect decides: `fault: fallen-off-bus` (and `lost`, `ecc-uncorrectable`) names the mode, and
// `xid: N` the code. A shim effect that names only a Xid leaves the mode to the model: a GPU the
// model holds offline has fallen off the bus, one it holds Critical stays addressable with
// uncorrectable errors (Xid 48's double-bit ECC), and a Warning is not a Mokka failure at all —
// Mokka has no mode that delivers a Xid without failing the device. The newest fault wins.
//
// Without any shim effect, a GPU the model holds offline is still reported lost: the model says
// it is gone, and an NVML that answered for it would contradict every other surface the twin
// serves.
func FailureOf(g GPUState) *Failure {
	var out *Failure
	for _, f := range g.Faults {
		for _, s := range f.Shims {
			fl := &Failure{Xid: xidOf(f, s), Why: f.Fault}
			switch strings.ReplaceAll(strings.ToLower(fmt.Sprint(s["fault"])), "_", "-") {
			case "fallen-off-bus":
				fl.Mode = ModeFallenOffBus
			case "lost":
				fl.Mode = ModeLost
			case "ecc-uncorrectable":
				fl.Mode = ModeECCUncorrectable
			default:
				switch {
				case g.State == "UnavailableOffline":
					fl.Mode = ModeFallenOffBus
				case g.Health == model.Critical:
					fl.Mode = ModeECCUncorrectable
				default:
					continue
				}
			}
			out = fl
		}
	}
	if out == nil && g.State == "UnavailableOffline" {
		why := "the model holds the GPU offline"
		if len(g.Faults) > 0 {
			why = g.Faults[len(g.Faults)-1].Fault
		}
		out = &Failure{Mode: ModeLost, Why: why}
	}
	return out
}

// Overrides is a host's override document (Mokka's overrides.yaml, mockctl.Doc): one `failure`
// block per failed GPU under its NVML index, nothing for a healthy one. An empty document returns
// every device to its profile. Version 1 is the document version nvml-mock-ctl writes.
func Overrides(gpus []GPUState) []byte {
	devices := map[string]map[string]any{}
	idx := make([]int, 0, len(gpus))
	for _, g := range gpus {
		f := FailureOf(g)
		if f == nil {
			continue
		}
		b := map[string]any{"mode": f.Mode}
		if f.Xid > 0 {
			b["xid"] = map[string]any{"code": f.Xid}
		}
		devices[strconv.Itoa(g.Index)] = map[string]any{"failure": b}
		idx = append(idx, g.Index)
	}
	doc := map[string]any{"version": 1}
	if len(devices) > 0 {
		doc["devices"] = devices
	}
	out, _ := yaml.Marshal(doc)
	sort.Ints(idx)
	head := "# dc-twin: the runtime overrides of this node's GPUs, rendered from committed model state; rewritten whole on every change.\n"
	if len(idx) == 0 {
		head += "# every GPU healthy\n"
	}
	return append([]byte(head), out...)
}

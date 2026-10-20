package kwok

import (
	"crypto/sha256"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/identity"
	"github.com/BudEcosystem/dc-twin/internal/model"
)

// This file is the model side of the projection: what a Node should say about a host, computed
// from committed model state only. It knows nothing about a cluster, so it is tested against an
// engine alone. kwok.go writes what it computes.

// Labels and annotations the projection owns. Everything under dc-twin.io/ is the twin's.
const (
	LabelRack    = "dc-twin.io/rack"
	LabelRow     = "dc-twin.io/row"
	LabelPath    = "dc-twin.io/path"
	LabelEstate  = "dc-twin.io/estate"
	LabelProfile = "dc-twin.io/profile"

	AnnImpersonated = "dc-twin.io/impersonated" // "kwok": D24's label on every object an impersonator registers
	AnnPath         = "dc-twin.io/device-path"  // the model path, unencoded (a label value cannot hold "/")
	AnnCordoned     = "dc-twin.io/cordoned"     // present only while the twin holds the node cordoned

	// TaintUnschedulable is the taint the node-lifecycle controller keeps in step with
	// spec.unschedulable; the projection sets both together, so the controller has nothing to undo.
	TaintUnschedulable = "node.kubernetes.io/unschedulable"
)

// Ready condition reasons. A real kubelet says KubeletReady or KubeletNotReady; the projection
// says why, from the model, because the model is the only thing it can know.
const (
	ReasonReady        = "KubeletReady"
	ReasonNoInputPower = "NoInputPower"
	ReasonPoweredOff   = "PoweredOff"
	ReasonPoweringOn   = "PoweringOn"
	ReasonPoweringOff  = "PoweringOff"
	ReasonNotBooted    = "NotBooted"
	ReasonCritical     = "HostHealthCritical"
)

// NodeName is the Node a model host registers as: "<rack>-<device>", lower-cased and reduced to
// a DNS-1123 subdomain. The metrics adapter labels its series with the same name (its hostname
// label), so a stack can join a node to its exporter.
func NodeName(d *model.Device) string {
	rack := ""
	if r := d.RackOf(); r != nil {
		rack = r.Name
	}
	n := d.Name
	if rack != "" {
		n = rack + "-" + d.Name
	}
	return dnsName(n)
}

var notDNS = regexp.MustCompile(`[^a-z0-9.-]+`)

func dnsName(s string) string {
	s = notDNS.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-.")
	if len(s) > 253 {
		s = strings.Trim(s[:253], "-.")
	}
	return s
}

var notLabel = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// labelValue reduces s to a valid label value (63 characters, alphanumeric at both ends); a
// value that has to be shortened keeps a hash of the original so two long paths stay distinct.
func labelValue(s string) string {
	v := notLabel.ReplaceAllString(strings.ReplaceAll(s, "/", "."), "-")
	v = strings.Trim(v, "-._")
	if len(v) > 63 {
		h := fmt.Sprintf("%x", sha256.Sum256([]byte(s)))[:10]
		v = strings.Trim(v[:52], "-._") + "-" + h
	}
	return v
}

// staticNode is what does not change while the estate runs: identity, labels, capacity.
type staticNode struct {
	labels      map[string]string
	annotations map[string]string
	capacity    corev1.ResourceList
	// accelResources are the extended resources a device plugin would advertise for the host's
	// accelerators: allocatable drops by one for each accelerator the model says is unhealthy.
	accelResources []corev1.ResourceName
	nodeInfo       corev1.NodeSystemInfo
	providerID     string
	groups         []string // accelerator component groups ("gpu", "hpu"), one condition each
}

// accelCondition is the condition type for an accelerator group: "gpu" gives GpuHealthy.
func accelCondition(group string) corev1.NodeConditionType {
	if group == "" {
		group = "accelerator"
	}
	return corev1.NodeConditionType(strings.ToUpper(group[:1]) + group[1:] + "Healthy")
}

// buildStatic computes a host's static node shape. Only immutable model fields are read: the
// compiler writes structure once and the engine never changes it.
func buildStatic(d *model.Device, estate, site string, seed int64, domain *model.Domain, kubeletVersion string) staticNode {
	s := staticNode{labels: map[string]string{}, annotations: map[string]string{}, capacity: corev1.ResourceList{}}
	name := NodeName(d)
	arch := nodeArch(d)
	// The labels a kubelet sets on itself.
	s.labels[corev1.LabelHostname] = name
	s.labels[corev1.LabelOSStable] = "linux"
	s.labels[corev1.LabelArchStable] = arch
	// Topology: zone is the hall, region the site.
	if d.Hall != nil {
		s.labels[corev1.LabelTopologyZone] = labelValue(d.Hall.Name)
	} else if r := d.RackOf(); r != nil && r.Hall != nil {
		s.labels[corev1.LabelTopologyZone] = labelValue(r.Hall.Name)
	}
	if site != "" {
		s.labels[corev1.LabelTopologyRegion] = labelValue(site)
	}
	if r := d.RackOf(); r != nil {
		s.labels[LabelRack] = labelValue(r.Name)
		if r.Row != nil {
			s.labels[LabelRow] = labelValue(r.Row.Name)
		}
	}
	s.labels[LabelPath] = labelValue(d.Path)
	s.labels[LabelEstate] = labelValue(estate)
	s.labels[LabelProfile] = labelValue(d.ProfileName)
	// What GPU feature discovery publishes for NVIDIA GPUs, derived from the same component the
	// dcgm projection names, so the two agree as NVML makes them agree on a real host.
	for k, v := range gfdLabels(d) {
		s.labels[k] = v
	}
	// The profile's labels, with {domain} as the host's scale-up domain id.
	domainID := ""
	if domain != nil {
		domainID = DomainID(seed, domain)
	}
	if k := d.Profile.Spec.Kubernetes; k != nil {
		for key, v := range k.Labels {
			if strings.Contains(v, "{domain}") {
				if domainID == "" {
					continue // a host outside any scale-up domain has no clique: GFD omits the label too
				}
				v = strings.ReplaceAll(v, "{domain}", domainID)
			}
			s.labels[key] = v
		}
	}
	s.annotations[AnnImpersonated] = "kwok"
	s.annotations[AnnPath] = d.Path

	// Capacity: what the profile advertises, plus what a kubelet would report for the machine.
	cores, mem, disk := machineSize(d)
	s.capacity[corev1.ResourceCPU] = *resource.NewQuantity(cores, resource.DecimalSI)
	s.capacity[corev1.ResourceMemory] = *resource.NewQuantity(mem, resource.BinarySI)
	s.capacity[corev1.ResourcePods] = *resource.NewQuantity(110, resource.DecimalSI) // the kubelet's default maxPods
	if disk > 0 {
		s.capacity[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(disk, resource.DecimalSI)
	}
	accels := 0
	groups := map[string]bool{}
	for _, c := range d.Components {
		if c.Kind == "accelerator" {
			accels++
			if !groups[c.Group] {
				groups[c.Group] = true
				s.groups = append(s.groups, c.Group)
			}
		}
	}
	if k := d.Profile.Spec.Kubernetes; k != nil {
		names := make([]string, 0, len(k.Resources))
		for rn := range k.Resources {
			names = append(names, rn)
		}
		sort.Strings(names)
		for _, rn := range names {
			n := k.Resources[rn]
			s.capacity[corev1.ResourceName(rn)] = *resource.NewQuantity(int64(n), resource.DecimalSI)
			// A device plugin's resource counts the accelerators it serves; that is how the
			// projection knows which resource an unhealthy accelerator is taken from.
			if accels > 0 && n == accels && strings.Contains(rn, "/") {
				s.accelResources = append(s.accelResources, corev1.ResourceName(rn))
			}
		}
	}
	sysUUID := d.Identity.UUID
	s.nodeInfo = corev1.NodeSystemInfo{
		MachineID:       strings.ReplaceAll(sysUUID, "-", ""),
		SystemUUID:      sysUUID,
		KernelVersion:   "",
		OSImage:         "dc-twin model host (no operating system runs)",
		KubeletVersion:  kubeletVersion,
		OperatingSystem: "linux",
		Architecture:    arch,
	}
	s.providerID = "dc-twin://" + d.Path
	return s
}

// DomainID is the value of a profile's {domain} placeholder: GPU Feature Discovery's
// nvidia.com/gpu.clique shape, "<ClusterUUID>.<CliqueID>" (k8s-device-plugin
// docs/gpu-feature-discovery/README.md, internal/lm/imex.go). The cluster UUID is identity/v1
// material for the domain's path, so it is stable across up/down and differs between estates
// with different seeds; the clique id is 1 because the model has one NVLink partition per
// domain.
func DomainID(seed int64, dom *model.Domain) string {
	return identity.New(seed).UUID(dom.Path, "domain:"+dom.Kind+":"+dom.Technology, 0) + ".1"
}

// scaleUpDomain finds the scale-up domain a host's accelerators belong to.
func scaleUpDomain(m *model.Model, d *model.Device) *model.Domain {
	paths := map[string]bool{}
	for _, c := range d.Components {
		if c.Kind == "accelerator" {
			paths[c.Path] = true
		}
	}
	if len(paths) == 0 {
		return nil
	}
	for _, dom := range m.Domains {
		if dom.Kind != "scale-up" {
			continue
		}
		for _, mem := range dom.Members {
			if paths[mem] {
				return dom
			}
		}
	}
	return nil
}

func nodeArch(d *model.Device) string { return model.HostArch(d) }

var gbRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*GB\b`)

// machineSize is cpu cores, memory bytes and local disk bytes, from the component models where
// they say ("NVIDIA Grace, 72 cores", "DDR5 64 GB", "NVMe 3.84 TB") and twin defaults where
// they do not: 32 cores a socket and 8 GiB a core. Local disk is the largest single drive (the
// root filesystem a kubelet reports as ephemeral-storage). The fidelity report says so.
func machineSize(d *model.Device) (cores, mem, disk int64) {
	var memBytes, diskBytes float64
	for _, c := range d.Components {
		switch c.Kind {
		case "cpu":
			cores += int64(model.CPUCores(c))
		case "memory":
			if m := gbRe.FindStringSubmatch(c.Model); m != nil {
				g, _ := strconv.ParseFloat(m[1], 64)
				memBytes += g * (1 << 30)
			}
		case "drive":
			// ephemeral-storage is the kubelet's root filesystem: one drive, not the sum.
			diskBytes = math.Max(diskBytes, model.DriveBytes(c))
		}
	}
	if cores == 0 {
		cores = 4
	}
	if memBytes == 0 {
		memBytes = float64(cores) * 8 * (1 << 30)
	}
	return cores, int64(memBytes), int64(diskBytes)
}

// observed is a host's committed state, read through the engine's accessors.
type observed struct {
	power, booted string
	ac            bool
	powerSince    time.Time
	health        string
	hostCritical  bool // Critical from something other than an accelerator
	accels        []accelObs
	faults        []engine.FaultView
}

type accelObs struct {
	group, name, path, health, state string
}

func observe(e *engine.Engine, d *model.Device) observed {
	var o observed
	e.Read(func(*model.Model) {
		o.power, o.booted, o.ac, o.powerSince = d.State.Power, d.State.Booted, d.State.AC, d.State.PowerSince
		// the boot id changes on every boot of the OS, a warm restart's included (power stays On)
		if !d.State.BootedSince.IsZero() && d.State.Booted == "os" {
			o.powerSince = d.State.BootedSince
		}
	})
	o.health = e.Health(d)
	o.hostCritical = e.HealthExcept(d, "accelerator") == model.Critical
	for _, c := range d.Components {
		if c.Kind != "accelerator" {
			continue
		}
		h, st := e.ComponentHealth(c)
		o.accels = append(o.accels, accelObs{group: c.Group, name: c.Name, path: c.Path, health: h, state: st})
	}
	o.faults = e.Faults(d.Path)
	return o
}

// up is whether a kubelet would be running: input power, powered on, and booted to its OS.
func (o observed) up() bool {
	return o.ac && o.power == model.On && o.booted == "os"
}

// desired is what the projection wants the Node to say now.
type desired struct {
	up          bool
	conditions  []corev1.NodeCondition // without times
	allocatable corev1.ResourceList
	cordon      bool
	cordonWhy   string
	bootID      string
}

func (o observed) faultText(target string) string {
	var parts []string
	for _, f := range o.faults {
		if target == "" || f.Target == target {
			parts = append(parts, fmt.Sprintf("%s [%s]", f.Title, f.Fault))
		}
	}
	return strings.Join(parts, "; ")
}

func desire(s staticNode, path string, o observed) desired {
	des := desired{up: o.up()}
	ready := corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: ReasonReady,
		Message: "kubelet is posting ready status"}
	notReady := func(reason, msg string) {
		ready.Status, ready.Reason, ready.Message = corev1.ConditionFalse, reason, "dc-twin model: "+msg
	}
	switch {
	case !o.ac:
		notReady(ReasonNoInputPower, "the host has no input power")
	case o.power == model.Off:
		notReady(ReasonPoweredOff, "the host is powered off")
	case o.power == model.PoweringOn:
		notReady(ReasonPoweringOn, "the host is powering on")
	case o.power == model.PoweringOff:
		notReady(ReasonPoweringOff, "the host is powering off")
	case o.booted != "os":
		notReady(ReasonNotBooted, fmt.Sprintf("the host has not booted its OS (boot step %q)", o.booted))
	case o.health == model.Critical && o.hostCritical:
		// A kubelet goes NotReady for what stops the host, not for one failed accelerator: after
		// an Xid it keeps posting Ready, the device plugin withholds the device, and the
		// accelerator's own condition says why. NotReady here would add the controller-manager's
		// not-ready:NoExecute taint and evict the pods on the healthy accelerators too.
		msg := "host health is Critical"
		if t := o.faultText(""); t != "" {
			msg += ": " + t
		}
		notReady(ReasonCritical, msg)
	}
	des.conditions = append(des.conditions, ready)
	// The pressure conditions a kubelet always posts; the model has no memory, disk or PID state.
	for _, p := range []struct {
		t      corev1.NodeConditionType
		reason string
		msg    string
	}{
		{corev1.NodeMemoryPressure, "KubeletHasSufficientMemory", "kubelet has sufficient memory available"},
		{corev1.NodeDiskPressure, "KubeletHasNoDiskPressure", "kubelet has no disk pressure"},
		{corev1.NodePIDPressure, "KubeletHasSufficientPID", "kubelet has sufficient PID available"},
	} {
		c := corev1.NodeCondition{Type: p.t, Status: corev1.ConditionFalse, Reason: p.reason, Message: p.msg}
		if !des.up {
			c.Status, c.Reason, c.Message = corev1.ConditionUnknown, "HostNotRunning", "dc-twin model: the host is not running"
		}
		des.conditions = append(des.conditions, c)
	}
	// One condition per accelerator group, from the components' health.
	unhealthy := 0
	for _, g := range s.groups {
		c := corev1.NodeCondition{Type: accelCondition(g)}
		total, bad := 0, []string{}
		worst := model.OK
		for _, a := range o.accels {
			if a.group != g {
				continue
			}
			total++
			if a.health != model.OK || a.state == "UnavailableOffline" {
				worst = model.WorstHealth(worst, a.health)
				line := fmt.Sprintf("%s %s, %s", a.name, a.health, a.state)
				if t := o.faultText(a.path); t != "" {
					line += ": " + t
				}
				bad = append(bad, line)
			}
		}
		switch {
		case !des.up:
			c.Status, c.Reason, c.Message = corev1.ConditionUnknown, "HostNotRunning", "dc-twin model: the host is not running"
		case len(bad) == 0:
			c.Status, c.Reason, c.Message = corev1.ConditionTrue, "AcceleratorsHealthy", fmt.Sprintf("%d/%d %s healthy", total, total, g)
		case worst == model.Critical:
			c.Status, c.Reason, c.Message = corev1.ConditionFalse, "AcceleratorCritical", strings.Join(bad, "; ")
		default:
			c.Status, c.Reason, c.Message = corev1.ConditionFalse, "AcceleratorDegraded", strings.Join(bad, "; ")
		}
		des.conditions = append(des.conditions, c)
	}
	for _, a := range o.accels {
		if a.health == model.Critical || a.state == "UnavailableOffline" {
			unhealthy++
		}
	}
	// Allocatable: what a kubelet reports after its default hard eviction threshold
	// (memory.available<100Mi), with each unhealthy accelerator withheld as a device plugin
	// withholds a device it has marked Unhealthy.
	des.allocatable = corev1.ResourceList{}
	for k, v := range s.capacity {
		des.allocatable[k] = v.DeepCopy()
	}
	if m, ok := des.allocatable[corev1.ResourceMemory]; ok {
		m.Sub(resource.MustParse("100Mi"))
		des.allocatable[corev1.ResourceMemory] = m
	}
	for _, rn := range s.accelResources {
		q := s.capacity[rn]
		n := q.Value() - int64(unhealthy)
		if n < 0 {
			n = 0
		}
		des.allocatable[rn] = *resource.NewQuantity(n, resource.DecimalSI)
	}
	if o.health == model.Critical {
		des.cordon = true
		des.cordonWhy = "host health is Critical"
		if t := o.faultText(""); t != "" {
			des.cordonWhy += ": " + t
		}
	}
	if des.up {
		// A boot id changes on every boot, as /proc/sys/kernel/random/boot_id does: the host's
		// path and the instant it last reached On.
		sum := sha256.Sum256([]byte(path + "\x00" + o.powerSince.UTC().Format(time.RFC3339Nano)))
		des.bootID = fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
	}
	return des
}

// fingerprint is a stable digest of the parts of a desired status that are written.
func (d desired) fingerprint() string {
	h := sha256.New()
	for _, c := range d.conditions {
		fmt.Fprintf(h, "%s|%s|%s|%s\n", c.Type, c.Status, c.Reason, c.Message)
	}
	names := make([]string, 0, len(d.allocatable))
	for k := range d.allocatable {
		names = append(names, string(k))
	}
	sort.Strings(names)
	for _, k := range names {
		q := d.allocatable[corev1.ResourceName(k)]
		fmt.Fprintf(h, "%s=%s\n", k, q.String())
	}
	fmt.Fprintf(h, "boot=%s\n", d.bootID)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// gfdLabels are GPU feature discovery's labels for a host's NVIDIA GPUs: the product is the NVML
// name (the component's model) with spaces as hyphens, the family and compute capability follow
// the product, and the machine is the system's product name. Nothing for a host without them.
func gfdLabels(d *model.Device) map[string]string {
	var gpu *model.Component
	n := 0
	for _, c := range d.Components {
		if c.Kind == "accelerator" && c.Vendor == "nvidia" {
			if gpu == nil {
				gpu = c
			}
			n++
		}
	}
	if gpu == nil {
		return nil
	}
	l := map[string]string{
		"nvidia.com/gpu.present": "true",
		"nvidia.com/gpu.count":   strconv.Itoa(n),
		"nvidia.com/gpu.product": labelValue(strings.ReplaceAll(gpu.Model, " ", "-")),
	}
	if gpu.MemoryMiB > 0 {
		l["nvidia.com/gpu.memory"] = strconv.FormatInt(int64(gpu.MemoryMiB), 10)
	}
	if fam, major, minor := gpuArch(gpu.Model); fam != "" {
		l["nvidia.com/gpu.family"] = fam
		l["nvidia.com/gpu.compute.major"] = major
		l["nvidia.com/gpu.compute.minor"] = minor
	}
	if m := d.Profile.Spec.Identity.Model; m != "" {
		l["nvidia.com/gpu.machine"] = labelValue(strings.ReplaceAll(m, " ", "-"))
	}
	return l
}

// gpuArch is GFD's architecture family and compute capability for a product name; "" when the
// name is not one it knows.
func gpuArch(product string) (family, major, minor string) {
	p := strings.ToUpper(product)
	for _, a := range []struct{ match, family, major, minor string }{
		{"GB300", "blackwell", "10", "3"}, {"B300", "blackwell", "10", "3"},
		{"GB200", "blackwell", "10", "0"}, {"B200", "blackwell", "10", "0"},
		{"GH200", "hopper", "9", "0"}, {"H200", "hopper", "9", "0"}, {"H100", "hopper", "9", "0"},
		{"L40", "ada-lovelace", "8", "9"}, {"L4", "ada-lovelace", "8", "9"},
		{"A100", "ampere", "8", "0"}, {"A30", "ampere", "8", "0"}, {"A10", "ampere", "8", "6"},
	} {
		if strings.Contains(p, a.match) {
			return a.family, a.major, a.minor
		}
	}
	return "", "", ""
}

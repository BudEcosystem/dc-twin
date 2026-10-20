package nvue

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// The trees NICo's bmc-mock serves and NICo's health collector reads, with the canonical
// request name each read is logged under.
var routes = map[string]struct {
	canonical string
	tree      func(*view) any
}{
	"/nvue_v1/system":                           {"inventory.read", (*view).system},
	"/nvue_v1/system/health":                    {"health.read", (*view).health},
	"/nvue_v1/system/reboot/reason":             {"health.read", (*view).rebootReason},
	"/nvue_v1/system/version":                   {"firmware.inventory.read", (*view).version},
	"/nvue_v1/platform":                         {"inventory.read", (*view).platform},
	"/nvue_v1/platform/environment":             {"health.read", (*view).environment},
	"/nvue_v1/platform/environment/fan":         {"health.read", (*view).fans},
	"/nvue_v1/platform/environment/led":         {"health.read", (*view).leds},
	"/nvue_v1/platform/environment/leakage":     {"health.read", (*view).leakage},
	"/nvue_v1/platform/environment/temperature": {"health.read", (*view).temperatures},
	"/nvue_v1/interface":                        {"network.read", (*view).interfaces},
	"/nvue_v1/cluster/apps":                     {"inventory.read", (*view).clusterApps},
	"/nvue_v1/sdn/partition":                    {"network.read", (*view).sdnPartitions},
}

func routeNames() []string {
	var out []string
	for k := range routes {
		out = append(out, strings.TrimPrefix(k, "/nvue_v1/"))
	}
	sort.Strings(out)
	return out
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ServeHTTP is nginx in front of NVUE, then NVUE: an unpowered tray answers nothing; HEAD and
// OPTIONS are nginx's 403; requests without the credentials of a tray account are nginx's 401;
// unknown paths are NVUE's 404 and writes its 405.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e := s.a.rt.Engine
	path := r.URL.Path
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	rt, known := routes[path]
	canonical := rt.canonical
	if !known {
		canonical = "other"
	}
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}
	user := ""
	defer func() {
		e.LogRead(engine.Request{Path: s.dev.Path, Canonical: canonical, Protocol: Protocol, Method: r.Method,
			Target: r.URL.RequestURI(), Source: schema.EventSource{Class: "stack", Principal: user, Address: r.RemoteAddr}}, rec.status)
	}()
	var on bool
	e.Read(func(*model.Model) { on = s.dev.State.AC && s.dev.State.Power == model.On })
	if !on {
		// NVOS runs on the tray's CPU: with the tray off nothing answers on its address.
		rec.status = 0
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				c.Close()
				return
			}
		}
		rec.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodHead || r.Method == http.MethodOptions {
		nginxPage(rec, http.StatusForbidden, "403 Forbidden")
		return
	}
	u, p, hasBasic := r.BasicAuth()
	if !hasBasic {
		nginxUnauthorized(rec)
		return
	}
	if _, ok := e.Authenticate(s.dev, u, p); !ok {
		nginxUnauthorized(rec)
		return
	}
	user = u
	if !known {
		problem(rec, http.StatusNotFound, "The requested URL was not found on the server. If you entered the URL manually please check your spelling and try again.")
		return
	}
	if r.Method != http.MethodGet {
		rec.Header().Set("Allow", "HEAD, OPTIONS, GET")
		problem(rec, http.StatusMethodNotAllowed, "The method is not allowed for the requested URL.")
		return
	}
	b, err := json.Marshal(rt.tree(s.view()))
	if err != nil {
		problem(rec, http.StatusInternalServerError, err.Error())
		return
	}
	rec.Header().Set("Content-Type", "application/json")
	rec.WriteHeader(http.StatusOK)
	_, _ = rec.Write(b)
}

func nginxUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="nvueapi"`)
	nginxPage(w, http.StatusUnauthorized, "401 Authorization Required")
}

func nginxPage(w http.ResponseWriter, status int, title string) {
	w.Header().Set("Server", "nginx")
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<html>\r\n<head><title>%s</title></head>\r\n<body>\r\n<center><h1>%s</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n", title, title)
}

// problem is an application/problem+json error as NVUE's Connexion server sends it.
func problem(w http.ResponseWriter, status int, detail string) {
	b, _ := json.Marshal(map[string]any{"detail": detail, "status": status, "title": http.StatusText(status), "type": "about:blank"})
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// ---- the projection

// nvlPort is one NVLink lane of the tray and the far end the model cables it to.
type nvlPort struct {
	name    string // acp<n>
	p, peer *model.Port
	adminUp bool
	speed   string
}

// view is what one response needs of the model, gathered under the engine's read lock, and
// the engine queries (health, port state, sensors, faults) made after it.
type view struct {
	s          *server
	d          *model.Device
	now        time.Time
	since      time.Time
	release    string
	serial     string
	sysType    string
	part       string
	mgmt       *model.Port
	mgmtAdmin  bool
	ports      []nvlPort
	asics      int
	domain     *model.Domain
	domMembers int
}

const timeFormat = "2006-01-02 15:04:05"

func (s *server) view() *view {
	e := s.a.rt.Engine
	d := s.dev
	v := &view{s: s, d: d, now: e.Now()}
	e.Read(func(*model.Model) {
		v.since = d.State.PowerSince
		if fw := d.State.Firmware["nvos"]; fw != nil {
			v.release = fw.Version
		}
		v.serial = d.Identity.Serial
		if id := d.Profile.Spec.Identity; id != nil {
			v.sysType, v.part = id.Model, id.PartNumber
		}
		if p := d.PortBy["mgmt0"]; p != nil {
			v.mgmt, v.mgmtAdmin = p, p.AdminUp
		}
		k := 0
		for _, c := range d.Components {
			if c.Kind != "asic" {
				continue
			}
			v.asics++
			for _, p := range c.Ports {
				if p.Kind != "scale-up" {
					continue
				}
				k++
				np := nvlPort{name: "acp" + strconv.Itoa(k), p: p, adminUp: p.AdminUp, speed: p.Speed}
				if p.Link != nil {
					np.peer = p.Link.Peer(p)
				}
				v.ports = append(v.ports, np)
			}
		}
		if r := d.RackOf(); r != nil {
			for _, dom := range r.Domains {
				if dom.Kind == "scale-up" {
					v.domain, v.domMembers = dom, len(dom.Members)
					break
				}
			}
		}
	})
	if v.since.IsZero() {
		v.since = e.Started()
	}
	if v.release == "" {
		v.release = "N/A"
	}
	return v
}

func (v *view) healthStatus() (string, map[string]any) {
	e := v.s.a.rt.Engine
	issues := map[string]any{}
	for _, f := range e.Faults(v.d.Path) {
		comp := ""
		switch {
		case f.Target == v.d.Path:
			comp = "System"
		case strings.HasPrefix(f.Target, v.d.Path+"/"):
			comp = strings.TrimPrefix(f.Target, v.d.Path+"/")
		case strings.HasPrefix(f.Target, v.d.Path+":"):
			comp = strings.TrimPrefix(f.Target, v.d.Path+":")
		default:
			continue
		}
		issues[comp] = map[string]any{"issue": f.Title}
	}
	for _, sn := range v.sensors() {
		if sn.state != model.OK {
			issues[sn.name] = map[string]any{"issue": fmt.Sprintf("%s %s", sn.quantity, strings.ToLower(sn.state))}
		}
	}
	if e.Health(v.d) != model.OK || len(issues) > 0 {
		return "Not OK", issues
	}
	return "OK", issues
}

func (v *view) system() any {
	status, _ := v.healthStatus()
	return map[string]any{
		"contact":         nil,
		"date-time":       map[string]any{"local-time": v.now.UTC().Format(timeFormat), "timezone": "Etc/UTC"},
		"health":          map[string]any{"status": status},
		"hostname":        hostname(v.d),
		"location":        nil,
		"product-name":    "nvos",
		"product-release": v.release,
		"status":          "System is ready",
		"uptime":          int64(v.now.Sub(v.since).Seconds()),
		"version":         map[string]any{"product-release": v.release},
	}
}

func (v *view) version() any {
	return map[string]any{
		"base-os":         "NVOS Debian GNU/Linux 12 (bookworm)",
		"image":           map[string]any{"build-id": "nvos-" + v.release},
		"product-release": v.release,
	}
}

func (v *view) health() any {
	status, issues := v.healthStatus()
	led := "green"
	if status != "OK" {
		led = "amber"
	}
	return map[string]any{"issues": issues, "status": status, "status-led": led}
}

func (v *view) rebootReason() any {
	return map[string]any{"gentime": v.since.UTC().Format(timeFormat), "reason": "reboot", "user": "admin"}
}

func (v *view) platform() any {
	return map[string]any{
		"asic-model":    "NVLink-5 switch",
		"manufacturer":  "Nvidia",
		"part-number":   v.part,
		"port-layout":   fmt.Sprintf("%d x 400G", len(v.ports)),
		"serial-number": v.serial,
		"system-type":   v.sysType,
	}
}

// sensor is one NVOS sensor bound to a model sensor.
type sensor struct {
	name, quantity, state string
	value                 float64
	spec                  schema.SensorSpec
}

// sensors names the tray's sensors as NVOS does: the model's asic-max reading is each NVLink
// Switch ASIC's (ASIC1, ASIC2), leak sensors are LEAKAGE-<n>, any other temperature keeps its
// model name.
func (v *view) sensors() []sensor {
	e := v.s.a.rt.Engine
	var out []sensor
	leaks := 0
	for _, sp := range v.d.Profile.Spec.Sensors {
		val, st, ok := e.Sensor(v.d, sp.Name)
		if !ok {
			continue
		}
		switch {
		case sp.Quantity == "leak":
			leaks++
			out = append(out, sensor{name: fmt.Sprintf("LEAKAGE-%d", leaks), quantity: "leak", state: st, value: val, spec: sp})
		case sp.Quantity == "temperature" && sp.Source == "asic-max":
			for i := 1; i <= v.asics; i++ {
				out = append(out, sensor{name: fmt.Sprintf("ASIC%d", i), quantity: "temperature", state: st, value: val, spec: sp})
			}
		case sp.Quantity == "temperature":
			out = append(out, sensor{name: sp.Name, quantity: "temperature", state: st, value: val, spec: sp})
		}
	}
	return out
}

func (v *view) leaking() bool {
	for _, s := range v.sensors() {
		if s.quantity == "leak" && s.value >= 1 {
			return true
		}
	}
	return false
}

func (v *view) ledColors() [][2]string {
	status := "green"
	if s, _ := v.healthStatus(); s != "OK" || v.leaking() {
		status = "amber"
	}
	return [][2]string{{"FAN_STATUS", "green"}, {"STATUS", status}, {"UID", "off"}}
}

func (v *view) environment() any {
	out := map[string]any{}
	for _, s := range v.sensors() {
		if s.quantity == "temperature" {
			out[s.name] = map[string]any{"state": strings.ToLower(s.state), "type": "temperature"}
		}
	}
	for _, l := range v.ledColors() {
		out[l[0]] = map[string]any{"state": l[1], "type": "led"}
	}
	return out
}

// fans: the tray profile is liquid-cooled and models no fans.
func (v *view) fans() any { return map[string]any{} }

func (v *view) leds() any {
	out := map[string]any{}
	for _, l := range v.ledColors() {
		out[l[0]] = map[string]any{"color": l[1]}
	}
	return out
}

func (v *view) leakage() any {
	out := map[string]any{}
	for _, s := range v.sensors() {
		if s.quantity == "leak" {
			st := "ok"
			if s.value >= 1 {
				st = "leak"
			}
			out[s.name] = map[string]any{"state": st}
		}
	}
	return out
}

func (v *view) temperatures() any {
	out := map[string]any{}
	for _, s := range v.sensors() {
		if s.quantity != "temperature" {
			continue
		}
		r := map[string]any{"current": fmt.Sprintf("%.2f", s.value), "state": strings.ToLower(s.state)}
		if s.spec.Warn != nil {
			r["max"] = fmt.Sprintf("%.2f", *s.spec.Warn)
		}
		if s.spec.Crit != nil {
			r["crit"] = fmt.Sprintf("%.2f", *s.spec.Crit)
		}
		out[s.name] = r
	}
	return out
}

// interfaces lists the NVLink ports, the management interface and the loopback. NVOS 25.02
// ignores filter_ on this tree, so every interface is listed whatever the query.
func (v *view) interfaces() any {
	e := v.s.a.rt.Engine
	out := map[string]any{}
	for _, np := range v.ports {
		up := np.adminUp && np.peer != nil && e.PortUp(np.p) && e.PortUp(np.peer)
		speed := np.speed
		if speed == "" {
			speed = "400G"
		}
		link := map[string]any{
			"auto-negotiate": "on", "connection-mode": "ndr", "lanes": "2X", "max-supported-mtu": 256, "mtu": 256,
			"speed": speed, "supported-lanes": "2X", "supported-speed": "400G",
		}
		switch {
		case up:
			link["state"] = map[string]any{"up": map[string]any{}}
			link["logical-state"], link["physical-state"] = "Active", "LinkUp"
			link["diagnostics"] = map[string]any{"0": map[string]any{"status": "No issue was observed"}}
		case !np.adminUp:
			link["state"] = map[string]any{"down": map[string]any{}}
			link["logical-state"], link["physical-state"] = "Down", "Disabled"
			link["diagnostics"] = map[string]any{"1": map[string]any{"status": "Closed by command"}}
		case np.peer == nil || !linkUp(e, np.p):
			link["state"] = map[string]any{"down": map[string]any{}}
			link["logical-state"], link["physical-state"] = "Down", "Polling"
			link["diagnostics"] = map[string]any{"1024": map[string]any{"status": "Cable is unplugged"}}
		default:
			link["state"] = map[string]any{"down": map[string]any{}}
			link["logical-state"], link["physical-state"] = "Down", "Polling"
			link["diagnostics"] = map[string]any{"2": map[string]any{"status": "Negotiation failure"}}
		}
		out[np.name] = map[string]any{"link": link, "type": "nvl"}
	}
	if v.mgmt != nil {
		st := map[string]any{"down": map[string]any{}}
		if v.mgmtAdmin && e.PortUp(v.mgmt) {
			st = map[string]any{"up": map[string]any{}}
		}
		speed := v.mgmt.Speed
		if speed == "" {
			speed = "1G"
		}
		out["eth0"] = map[string]any{"ifindex": 2, "type": "eth", "link": map[string]any{
			"auto-negotiate": "on", "duplex": "full", "mtu": 1500, "speed": speed, "state": st, "mac": strings.ToLower(v.mgmt.MAC)}}
	}
	out["lo"] = map[string]any{"ifindex": 1, "type": "loopback",
		"link": map[string]any{"mac": "00:00:00:00:00:00", "mtu": 65536, "state": map[string]any{"up": map[string]any{}}}}
	return out
}

// linkUp reports whether the lane's cable is in: a pulled or failed link, not a fault on an end.
func linkUp(e *engine.Engine, p *model.Port) bool {
	up := false
	e.Read(func(*model.Model) { up = p.Link != nil && p.Link.Up })
	return up
}

// clusterApps lists the NMX applications NVOS hosts, from the rack's scale-up domain: the
// controller reports its control plane configured while the domain is formed.
func (v *view) clusterApps() any {
	if v.domain == nil {
		return map[string]any{}
	}
	info, status := "CONTROL_PLANE_STATE_CONFIGURED", "ok"
	if st := v.s.a.rt.Engine.DomainState(v.domain.Path); st == "unformed" {
		info, status = "CONTROL_PLANE_STATE_NOT_CONFIGURED", "not ok"
	}
	return map[string]any{
		"nmx-controller": map[string]any{"addition-info": info, "app-id": "nmx-c-nvos", "app-ver": "4.21.156",
			"capabilities": "sm, gfm, fib, gw-api", "components-ver": "sm:2025.10.18, gfm:580.173.04, fib:4.21.156", "reason": "", "status": status},
		"nmx-telemetry": map[string]any{"addition-info": "", "app-id": "nmx-telemetry", "app-ver": "4.20.9",
			"capabilities": "nvl, gnmi, syslog, bmc", "components-ver": "nvl:1.23.7, aggr:1.30.1, cm:1.1.14, gnmi:1.4.1", "reason": "", "status": "ok"},
	}
}

// sdnPartitions is the factory default partition of the rack's scale-up domain, holding every
// GPU of the domain; its health follows the domain's state.
func (v *view) sdnPartitions() any {
	if v.domain == nil {
		return map[string]any{}
	}
	health := "healthy"
	switch v.s.a.rt.Engine.DomainState(v.domain.Path) {
	case "degraded":
		health = "degraded"
	case "unformed":
		health = "unhealthy"
	}
	return map[string]any{"32766": map[string]any{"health": health, "mcast-limit": 1024, "name": "Default Partition",
		"num-gpus": strconv.Itoa(v.domMembers), "partition-type": "gpuuid_based", "resiliency-mode": "adaptive_bandwidth"}}
}

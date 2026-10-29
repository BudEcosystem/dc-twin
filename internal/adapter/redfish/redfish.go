// Package redfish is the core's Redfish engine (D7, D25): one HTTPS service per emulated BMC,
// scoped by what the stacks under test call. A dialect is either a captured service tree bound
// to the model by data (identity scrubbing, overlays, component/sensor/leak bindings and an
// action map) or, for profiles with no capture, a DMTF tree generated from the model. Every
// request is translated into an engine request with a canonical name; nothing here changes
// state except through the engine.
package redfish

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/identity"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

func init() { twin.Register("redfish", func() adapter.Adapter { return New() }) }

// Adapter serves every BMC of the estate.
type Adapter struct {
	rt           *adapter.Runtime
	eng          *engine.Engine
	id           *identity.Deriver
	bmcs         map[string]*bmc
	servers      []*http.Server
	authOptional bool
	captureDPUs  bool
	mu           sync.Mutex
	dialects     map[string]int
	missing      map[string]string
	subID        int
	isNot        []string
	pushClient   *http.Client
}

type backend interface {
	render(rd *renderer, path string) (map[string]any, bool)
}

type mockupBackend struct{}

func (mockupBackend) render(rd *renderer, path string) (map[string]any, bool) { return rd.render(path) }

type bmc struct {
	a        *Adapter
	dev      *model.Device
	dialect  *schema.RedfishDialect
	tree     *tree
	orig     map[string]string
	backend  backend
	hostname string
	addr     string
	// dpus are the host's DPU sub-devices in component order, which the dialect's dpus patterns
	// bind the captured DPU resources to (structure, so read once).
	dpus []*model.Device
}

// New returns an unstarted adapter.
func New() *Adapter {
	return &Adapter{bmcs: map[string]*bmc{}, dialects: map[string]int{}, missing: map[string]string{},
		pushClient: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}}
}

func (a *Adapter) Name() string { return "redfish" }

// Hostname is the BMC's host name: "<rack>-<device>-bmc" (sub-devices include their parent).
func Hostname(d *model.Device) string {
	parts := []string{}
	for x := d; x != nil; x = x.Parent {
		parts = append([]string{x.Name}, parts...)
	}
	if r := d.RackOf(); r != nil {
		parts = append([]string{r.Name}, parts...)
	}
	return strings.Join(parts, "-") + "-bmc"
}

func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt, a.eng = rt, rt.Engine
	a.id = identity.New(rt.Engine.M.Seed)
	a.authOptional = rt.Engine.M.Estate.Spec.Adapters.Redfish.Auth == "optional"
	a.captureDPUs = rt.Engine.M.Estate.Spec.Adapters.Redfish.DPUs == "capture"
	var devices []*model.Device
	rt.Engine.Read(func(m *model.Model) {
		for _, d := range m.Devices {
			if d.HasBMC() && d.Dialect != "" {
				devices = append(devices, d)
			}
		}
	})
	type dialectRT struct {
		spec *schema.RedfishDialect
		tree *tree
		orig map[string]string
		err  error
	}
	cache := map[string]*dialectRT{}
	for _, d := range devices {
		drt, ok := cache[d.Dialect]
		if !ok {
			spec, _ := rt.Reg.Dialect(d.Dialect)
			drt = &dialectRT{spec: spec}
			if spec == nil {
				drt.err = fmt.Errorf("dialect %s not found", d.Dialect)
			} else if spec.Spec.Backend == "mockup" {
				dir, err := twin.AssetPath(rt.Reg, spec.Spec.Mockup)
				if err != nil {
					drt.err = err
				} else if t, err := loadTree(dir); err != nil {
					drt.err = err
				} else {
					drt.tree = t
					drt.orig = map[string]string{}
					for k, ref := range spec.Spec.Bind {
						drt.orig[k] = t.value(ref)
					}
					if spec.Spec.BMCPort != "" {
						drt.orig["bmcMAC"] = t.value(spec.Spec.BMCPort + "#MACAddress")
					}
				}
			}
			cache[d.Dialect] = drt
		}
		if drt.err != nil {
			a.missing[d.Dialect] = drt.err.Error()
			continue
		}
		b := &bmc{a: a, dev: d, dialect: drt.spec, tree: drt.tree, orig: drt.orig, hostname: Hostname(d)}
		for _, c := range d.Components {
			if c.Kind == "dpu" && c.Sub != nil {
				b.dpus = append(b.dpus, c.Sub)
			}
		}
		if drt.spec.Spec.Backend == "mockup" {
			b.backend = mockupBackend{}
			a.seed(b)
		} else {
			b.backend = generatedBackend{}
		}
		l, err := rt.Net.Listen(d.Path, "redfish")
		if err != nil {
			return err
		}
		cert, err := rt.Net.Certificate(b.hostname, []string{b.hostname}, nil)
		if err != nil {
			return err
		}
		tl := tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		srv := &http.Server{Handler: b, ReadHeaderTimeout: 30 * time.Second, ErrorLog: nil}
		go srv.Serve(tl)
		b.addr = l.Addr().String()
		a.servers = append(a.servers, srv)
		a.bmcs[d.Path] = b
		a.dialects[d.Dialect]++
		rt.Engine.SetEndpoint(d.Path, "redfish", b.addr)
		rt.Net.Route(b.hostname+":443", b.addr)
		rt.Engine.Read(func(*model.Model) {
			if d.Endpoint.SiteIP != "" {
				rt.Net.Route(d.Endpoint.SiteIP+":443", b.addr)
			}
		})
	}
	if len(a.missing) > 0 {
		var msgs []string
		for k, v := range a.missing {
			msgs = append(msgs, k+": "+v)
		}
		return fmt.Errorf("redfish dialects unavailable: %s", strings.Join(msgs, "; "))
	}
	// leases move a BMC's site address: route it through the gateway
	id, ch := rt.Engine.Subscribe(4096)
	a.subID = id
	go a.watch(ctx, ch)
	return nil
}

// seed copies what a captured BMC carries into the model once: its accounts, boot order and
// Secure Boot state (twin set-up, committed as twin events).
func (a *Adapter) seed(b *bmc) {
	spec := b.dialect.Spec
	if spec.SeedAccounts {
		if coll, ok := b.tree.get("/redfish/v1/AccountService/Accounts"); ok {
			var accts []model.Account
			members, _ := coll["Members"].([]any)
			for _, m := range members {
				ref, _ := m.(map[string]any)["@odata.id"].(string)
				doc, ok := b.tree.get(strings.TrimRight(ref, "/"))
				if !ok {
					continue
				}
				// empty slots are kept: an iDRAC has sixteen fixed account slots, most of them empty
				user, _ := doc["UserName"].(string)
				role, _ := doc["RoleId"].(string)
				enabled, isBool := doc["Enabled"].(bool)
				if !isBool && user != "" {
					enabled = true
				}
				idv, _ := doc["Id"].(string)
				accts = append(accts, model.Account{ID: idv, User: user, Role: role, Enabled: enabled})
			}
			a.eng.SeedAccounts(b.dev.Path, accts)
		}
	}
	if spec.System != "" {
		if sys, ok := b.tree.get(spec.System); ok {
			a.eng.Seed(b.dev.Path, "boot, secure boot and firmware seeded from the captured tree", func(d *model.Device) {
				if boot, ok := sys["Boot"].(map[string]any); ok {
					if order, ok := boot["BootOrder"].([]any); ok {
						d.State.Boot.Order = nil
						for _, o := range order {
							d.State.Boot.Order = append(d.State.Boot.Order, fmt.Sprint(o))
						}
					}
					if t, ok := boot["BootSourceOverrideTarget"].(string); ok {
						d.State.Boot.OverrideTarget = t
					}
					if t, ok := boot["BootSourceOverrideEnabled"].(string); ok {
						d.State.Boot.OverrideEnabled = t
					}
					if t, ok := boot["BootSourceOverrideMode"].(string); ok {
						d.State.Boot.OverrideMode = t
					}
				}
				// the captured firmware versions are this device's baseline, so every resource that
				// repeats a version (inventory, component integrity) agrees until an update lands;
				// a dialect presenting another product's capture keeps the profile's (D35). A
				// component the capture holds at more than one level (one GB200 GPU's firmware is at
				// another level than its siblings') keeps the profile's too: the model holds one
				// level for it, and which member came first would otherwise decide it.
				if !spec.FirmwareFromProfile {
					for comp, v := range capturedFirmware(spec.Firmware, b.tree) {
						if fw := d.State.Firmware[comp]; fw != nil {
							fw.Version, fw.Baseline = v, v
						}
					}
				}
				if len(spec.Firmware) == 0 {
					if v, ok := sys["BiosVersion"].(string); ok && v != "" && d.State.Firmware["bios"] != nil {
						d.State.Firmware["bios"].Version, d.State.Firmware["bios"].Baseline = v, v
					}
					if mgr, ok := b.tree.get(spec.Manager); ok {
						if v, ok := mgr["FirmwareVersion"].(string); ok && v != "" && d.State.Firmware["bmc"] != nil {
							d.State.Firmware["bmc"].Version, d.State.Firmware["bmc"].Baseline = v, v
						}
					}
				}
				if sb, ok := b.tree.get(spec.System + "/SecureBoot"); ok {
					if v, ok := sb["SecureBootEnable"].(bool); ok {
						d.State.SecureBoot = v
					}
				}
			})
		}
	}
}

func (b *bmc) lease() *model.Lease {
	var l *model.Lease
	b.a.eng.Read(func(*model.Model) {
		if b.dev.State.Lease != nil {
			c := *b.dev.State.Lease
			l = &c
		}
	})
	return l
}

func (a *Adapter) watch(ctx context.Context, ch <-chan schema.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				a.eng.Incident("redfish", "event subscription dropped: the Redfish projection fell behind the log")
				return
			}
			a.observeFirmware(ev) // a vm host's boot options (vmboot.go)
			if ev.State != nil && ev.State.Field == "lease" {
				if b := a.bmcs[ev.Path]; b != nil {
					if from, _ := ev.State.From.(string); from != "" {
						a.rt.Net.Unroute(from + ":443")
					}
					if to, _ := ev.State.To.(string); to != "" {
						a.rt.Net.Route(to+":443", b.addr)
					}
				}
			}
			a.push(ev)
		}
	}
}

func (a *Adapter) Stop() error {
	if a.subID != 0 {
		a.eng.Unsubscribe(a.subID)
	}
	for _, s := range a.servers {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
	}
	return nil
}

// BMC returns the served endpoint of a device's BMC (host:port), for tests and the CLI.
func (a *Adapter) BMC(path string) (addr, hostname string, ok bool) {
	b, ok := a.bmcs[path]
	if !ok {
		return "", "", false
	}
	return b.addr, b.hostname, true
}

func (a *Adapter) Report() adapter.Report {
	r := adapter.Report{Adapter: "redfish", Served: len(a.bmcs)}
	for d, n := range a.dialects {
		spec, _ := a.rt.Reg.Dialect(d)
		backend := spec.Spec.Backend
		if backend == "mockup" {
			backend = "captured tree " + spec.Spec.Mockup
		} else {
			backend = "generated DMTF tree"
		}
		r.Lines = append(r.Lines, fmt.Sprintf("redfish %s: %d BMCs (%s)", d, n, backend))
		r.IsNot = append(r.IsNot, spec.Spec.IsNot.All...)
	}
	if a.authOptional {
		r.Lines = append(r.Lines, "redfish auth: OPTIONAL (oracle mode: unauthenticated requests are accepted)")
	}
	if a.captureDPUs {
		r.Lines = append(r.Lines, "redfish dpus: CAPTURE (oracle mode: hosts serve the captured machine's DPUs, not the model's)")
	}
	r.IsNot = append(r.IsNot, "Query options ($expand, $select, $filter) are ignored; resources answer in full.",
		"Captured trees keep the captured device's static inventory; only bound properties follow the model.")
	return r
}

// ---------------------------------------------------------------- SSE and subscriptions

func (b *bmc) sseURI() string {
	if b.tree != nil {
		if es, ok := b.tree.get("/redfish/v1/EventService"); ok {
			if u, ok := es["ServerSentEventUri"].(string); ok {
				return normalise(u)
			}
		}
		return ""
	}
	return "/redfish/v1/EventService/SSE"
}

func (b *bmc) multipartURI() string {
	if b.tree != nil {
		if us, ok := b.tree.get("/redfish/v1/UpdateService"); ok {
			if u, ok := us["MultipartHttpPushUri"].(string); ok {
				return normalise(u)
			}
		}
	}
	return "/redfish/v1/UpdateService/upload"
}

func (b *bmc) xmlDoc(path string) ([]byte, bool) {
	if b.tree != nil {
		x, ok := b.tree.xml[path]
		return x, ok
	}
	if path == "/redfish/v1/$metadata" {
		return []byte(generatedMetadata), true
	}
	return nil, false
}

func (b *bmc) touches(ev schema.Event) bool {
	return ev.Path == b.dev.Path || strings.HasPrefix(ev.Path, b.dev.Path+"/") || strings.HasPrefix(ev.Path, b.dev.Path+":")
}

func (b *bmc) eventPayload(ev schema.Event) map[string]any {
	msgID, msg := "ResourceEvent.1.2.ResourceChanged", ev.Message
	if ev.State != nil {
		msg = fmt.Sprintf("%s changed from %v to %v.", ev.State.Field, ev.State.From, ev.State.To)
		switch {
		case ev.State.Field == "power":
			msgID = "ResourceEvent.1.2.ResourcePowerStateChanged"
		case ev.State.Field == "health" || strings.HasPrefix(ev.State.Field, "sensor."):
			msgID = "ResourceEvent.1.2.ResourceStatusChangedCritical"
			if ev.Severity == model.Warning {
				msgID = "ResourceEvent.1.2.ResourceStatusChangedWarning"
			} else if ev.Severity == model.OK || ev.Severity == "" {
				msgID = "ResourceEvent.1.2.ResourceStatusChangedOK"
			}
		case strings.HasPrefix(ev.State.Field, "log."):
			msgID = "ResourceEvent.1.2.ResourceCreated"
		}
	}
	sev := ev.Severity
	if sev == "" {
		sev = "OK"
	}
	origin := b.dialect.Spec.System
	if origin == "" {
		origin = b.dialect.Spec.Chassis
	}
	return map[string]any{"@odata.type": "#Event.v1_7_0.Event", "Id": fmt.Sprint(ev.Seq), "Name": "Event Array",
		"Events": []any{map[string]any{"EventType": "Alert", "EventId": fmt.Sprint(ev.Seq), "Severity": sev, "MessageSeverity": sev,
			"Message": msg, "MessageId": msgID, "EventTimestamp": ev.T, "OriginOfCondition": map[string]any{"@odata.id": origin}}}}
}

func (b *bmc) serveSSE(w http.ResponseWriter, r *http.Request) {
	b.logRead(r, r.URL.Path, 200)
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "Base.1.8.InternalError", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	fl.Flush()
	id, ch := b.a.eng.Subscribe(1024)
	defer b.a.eng.Unsubscribe(id)
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": keep-alive\n\n")
			fl.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Kind == "request" || !b.touches(ev) {
				continue
			}
			data, _ := jsonMarshal(b.eventPayload(ev))
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, data)
			fl.Flush()
		}
	}
}

// push delivers events to EventService subscriptions (best effort, as BMCs do).
func (a *Adapter) push(ev schema.Event) {
	if ev.Kind == "request" {
		return
	}
	for _, b := range a.bmcs {
		if !b.touches(ev) {
			continue
		}
		var subs []model.Subscription
		a.eng.Read(func(*model.Model) { subs = append(subs, b.dev.State.Subscribers...) })
		for _, s := range subs {
			if s.Destination == "" {
				continue
			}
			payload := b.eventPayload(ev)
			payload["Context"] = s.Context
			go func(dest string, p map[string]any) {
				data, _ := jsonMarshal(p)
				resp, err := a.pushClient.Post(dest, "application/json", strings.NewReader(string(data)))
				if err == nil {
					resp.Body.Close()
				}
			}(s.Destination, payload)
		}
	}
}

func (a *Adapter) broadcastTest(b *bmc) {
	a.push(schema.Event{Seq: a.eng.Seq(), Kind: "state", Path: b.dev.Path, Message: "Test event", T: time.Now().UTC().Format(time.RFC3339)})
}

// Get renders a device's resource as its BMC serves it now, without logging a request: the
// scenario runner and the console's inspector observe from outside the stack's view.
func (a *Adapter) Get(devicePath, resourcePath string) (map[string]any, bool) {
	b := a.bmcs[devicePath]
	if b == nil {
		return nil, false
	}
	rd := &renderer{b: b, snap: b.snapshot()}
	return b.backend.render(rd, normalise(resourcePath))
}

// capturedFirmware is each firmware component's captured level, by the inventory members the
// dialect binds to it, for the components whose members all report one level.
func capturedFirmware(bind map[string]string, t *tree) map[string]string {
	ids := make([]string, 0, len(bind))
	for id := range bind {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	levels := map[string]string{}
	mixed := map[string]bool{}
	for _, id := range ids {
		comp := bind[id]
		doc, ok := t.get("/redfish/v1/UpdateService/FirmwareInventory/" + id)
		if !ok {
			continue
		}
		v, _ := doc["Version"].(string)
		if v == "" {
			continue
		}
		if prev, seen := levels[comp]; seen && prev != v {
			mixed[comp] = true
		}
		levels[comp] = v
	}
	for comp := range mixed {
		delete(levels, comp)
	}
	return levels
}

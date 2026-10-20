package engine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// Management-controller state (accounts, sessions, logs, firmware, BIOS, boot, PATCHed
// properties) is owned by the model, so every interface that reads it agrees (G2).

func (e *Engine) dev(r Request) (*model.Device, error) {
	d := e.M.DeviceBy[r.Path]
	if d == nil {
		return nil, ErrNoDevice
	}
	return d, nil
}

// httpStatus is the status an engine-side write logs: an HTTP status for HTTP protocols, none
// for IPMI or SNMP (whose adapters log their own completion codes).
func httpStatus(r Request, s int) int {
	switch r.Protocol {
	case "redfish", "ufm-rest", "nvue", "control", "http", "rms":
		return s
	}
	return 0
}

func token() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (e *Engine) addLog(d *model.Device, severity, msg, id, service string, cause uint64) {
	n := len(d.State.Log) + 1
	d.State.Log = append(d.State.Log, model.LogEntry{ID: n, Created: e.now, Severity: severity, Message: msg, MessageID: id, Service: service})
	e.commit(schema.Event{Kind: "state", Source: behaviourSrc, Path: d.Path, Cause: cause, Severity: severity,
		State: &schema.EventState{Field: "log." + service, To: msg}})
}

// ClearLog empties a log service (log.clear).
func (e *Engine) ClearLog(r Request, service string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return err
	}
	e.logRequest(r, 0)
	var keep []model.LogEntry
	for _, l := range d.State.Log {
		if l.Service != service {
			keep = append(keep, l)
		}
	}
	d.State.Log = keep
	return nil
}

// Authenticate checks a BMC login.
func (e *Engine) Authenticate(d *model.Device, user, pass string) (model.Account, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, a := range d.State.Accounts {
		if a.User != "" && a.User == user && a.Password == pass && a.Enabled && !a.Locked {
			return a, true
		}
	}
	return model.Account{}, false
}

// SeedAccounts adds accounts a captured BMC carries (twin set-up, not a stack request).
func (e *Engine) SeedAccounts(path string, accts []model.Account) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.M.DeviceBy[path]
	if d == nil {
		return
	}
	have := map[string]bool{}
	for _, a := range d.State.Accounts {
		have[a.User] = true
	}
	for _, a := range accts {
		if a.User != "" && have[a.User] {
			// the factory account keeps the derived credential but takes the captured id
			for i := range d.State.Accounts {
				if d.State.Accounts[i].User == a.User {
					d.State.Accounts[i].ID = a.ID
					d.State.Accounts[i].Role = a.Role
				}
			}
			continue
		}
		if a.Password == "" && a.User != "" {
			a.Password = token()
		}
		d.State.Accounts = append(d.State.Accounts, a)
	}
}

// CreateAccount adds a BMC user (account.create).
func (e *Engine) CreateAccount(r Request, user, pass, role string) (model.Account, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return model.Account{}, err
	}
	for _, a := range d.State.Accounts {
		if a.User == user {
			return model.Account{}, fmt.Errorf("user %s exists", user)
		}
	}
	id := 1
	for _, a := range d.State.Accounts {
		if n, err := strconv.Atoi(a.ID); err == nil && n >= id {
			id = n + 1
		}
	}
	if role == "" {
		role = "Administrator"
	}
	a := model.Account{ID: strconv.Itoa(id), User: user, Password: pass, Role: role, Enabled: true}
	d.State.Accounts = append(d.State.Accounts, a)
	c := e.logRequest(r, httpStatus(r, 201))
	e.stateEvent(d.Path, "account."+a.ID, nil, user, r.Source, c, "")
	return a, nil
}

// DeleteAccount removes a BMC user (account.delete).
func (e *Engine) DeleteAccount(r Request, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return err
	}
	for i, a := range d.State.Accounts {
		if a.ID == id {
			d.State.Accounts = append(d.State.Accounts[:i], d.State.Accounts[i+1:]...)
			c := e.logRequest(r, httpStatus(r, 200))
			e.stateEvent(d.Path, "account."+id, a.User, nil, r.Source, c, "")
			return nil
		}
	}
	return fmt.Errorf("no account %s", id)
}

// UpdateAccount changes a user's name, password, role or enabled state (account.password …).
// Rotations are committed so a scenario can assert the stack rotated them.
func (e *Engine) UpdateAccount(r Request, id string, fields map[string]any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return err
	}
	for i := range d.State.Accounts {
		a := &d.State.Accounts[i]
		if a.ID != id && a.User != id {
			continue
		}
		c := e.logRequest(r, httpStatus(r, 200))
		if v, ok := fields["Password"].(string); ok {
			a.Password = v
			a.PasswordChangeRequired = false
			e.stateEvent(d.Path, "account."+a.ID+".password", nil, "[rotated]", r.Source, c, "")
		}
		if v, ok := fields["UserName"].(string); ok {
			old := a.User
			a.User = v
			e.stateEvent(d.Path, "account."+a.ID+".user", old, v, r.Source, c, "")
		}
		if v, ok := fields["RoleId"].(string); ok {
			a.Role = v
		}
		if v, ok := fields["Enabled"].(bool); ok {
			a.Enabled = v
		}
		if v, ok := fields["Locked"].(bool); ok {
			a.Locked = v
		}
		return nil
	}
	return fmt.Errorf("no account %s", id)
}

// CreateSession logs in (session.create) and returns the session with its token.
func (e *Engine) CreateSession(r Request, user, pass string) (model.Session, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return model.Session{}, err
	}
	ok := false
	for _, a := range d.State.Accounts {
		if a.User != "" && a.User == user && a.Password == pass && a.Enabled && !a.Locked {
			ok = true
		}
	}
	if !ok {
		e.logRequest(r, 401)
		return model.Session{}, fmt.Errorf("authentication failed")
	}
	s := model.Session{ID: fmt.Sprintf("%d", len(d.State.Sessions)+1) + token()[:6], Token: token(), User: user, Created: e.now}
	d.State.Sessions[s.ID] = s
	e.logRequest(r, httpStatus(r, 201))
	return s, nil
}

// SessionByToken resolves an X-Auth-Token.
func (e *Engine) SessionByToken(d *model.Device, tok string) (model.Session, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, s := range d.State.Sessions {
		if s.Token == tok {
			return s, true
		}
	}
	return model.Session{}, false
}

// DeleteSession logs out (session.delete).
func (e *Engine) DeleteSession(r Request, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return err
	}
	if _, ok := d.State.Sessions[id]; !ok {
		return fmt.Errorf("no session %s", id)
	}
	delete(d.State.Sessions, id)
	e.logRequest(r, httpStatus(r, 204))
	return nil
}

// Sessions lists a BMC's sessions in a stable order.
func (e *Engine) Sessions(d *model.Device) []model.Session {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []model.Session
	for _, s := range d.State.Sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Created.Before(out[j].Created) || (out[i].Created.Equal(out[j].Created) && out[i].ID < out[j].ID)
	})
	return out
}

// BootPatch is a change to boot override or order (boot.override, boot.order).
type BootPatch struct {
	Target, Enabled, Mode, URI *string
	Order                      []string
}

// SetBoot applies a boot override or a boot order.
func (e *Engine) SetBoot(r Request, p BootPatch) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return err
	}
	c := e.logRequest(r, httpStatus(r, 200))
	b := &d.State.Boot
	set := func(dst *string, src *string, field string) {
		if src != nil && *dst != *src {
			old := *dst
			*dst = *src
			e.stateEvent(d.Path, "boot."+field, old, *src, r.Source, c, "")
		}
	}
	set(&b.OverrideTarget, p.Target, "overrideTarget")
	set(&b.OverrideEnabled, p.Enabled, "overrideEnabled")
	set(&b.OverrideMode, p.Mode, "overrideMode")
	set(&b.HTTPBootURI, p.URI, "httpBootUri")
	if p.Order != nil && strings.Join(p.Order, ",") != strings.Join(b.Order, ",") {
		old := b.Order
		b.Order = append([]string{}, p.Order...)
		e.stateEvent(d.Path, "boot.order", old, b.Order, r.Source, c, "")
	}
	return nil
}

// SetBiosPending stages BIOS attributes, applied on the next boot (bios.set).
func (e *Engine) SetBiosPending(r Request, attrs map[string]any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return err
	}
	c := e.logRequest(r, httpStatus(r, 200))
	for k, v := range attrs {
		d.State.BiosPending[k] = v
	}
	e.stateEvent(d.Path, "bios.pending", nil, len(d.State.BiosPending), r.Source, c, "")
	return nil
}

// ResetBios restores defaults on the next boot (bios.reset); ClearPending drops staged values.
func (e *Engine) ResetBios(r Request) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return err
	}
	c := e.logRequest(r, httpStatus(r, 200))
	d.State.BiosPending = map[string]any{"__reset__": true}
	e.stateEvent(d.Path, "bios.pending", nil, "reset to defaults", r.Source, c, "")
	return nil
}

func (e *Engine) ClearBiosPending(r Request) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return err
	}
	e.logRequest(r, httpStatus(r, 200))
	d.State.BiosPending = map[string]any{}
	return nil
}

// SetFlag sets secure boot or lockdown (secureboot.set, lockdown.set).
func (e *Engine) SetFlag(r Request, flag string, on bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return err
	}
	c := e.logRequest(r, httpStatus(r, 200))
	var old bool
	switch flag {
	case "secureBoot":
		old, d.State.SecureBoot = d.State.SecureBoot, on
	case "lockdown":
		old, d.State.Lockdown = d.State.Lockdown, on
	default:
		return ErrUnsupported
	}
	e.stateEvent(d.Path, flag, old, on, r.Source, c, "")
	return nil
}

// Patch stores PATCHed properties of a protocol resource in the model (owned state).
func (e *Engine) Patch(r Request, resource string, props map[string]any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return err
	}
	c := e.logRequest(r, httpStatus(r, 200))
	cur := d.State.Patches[resource]
	if cur == nil {
		cur = map[string]any{}
	}
	deepMerge(cur, props)
	d.State.Patches[resource] = cur
	e.stateEvent(d.Path, "patch:"+resource, nil, keys(props), r.Source, c, "")
	return nil
}

// SetBMCClock sets the BMC's clock (bmc.clock, a Manager DateTime PATCH): the clock keeps running
// from the value set, so it is held as an offset from the twin's clock.
func (e *Engine) SetBMCClock(r Request, t time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return err
	}
	c := e.logRequest(r, httpStatus(r, 200))
	old := d.State.BMCClock
	d.State.BMCClock = t.Sub(e.now).Round(time.Second)
	e.stateEvent(d.Path, "bmc.clock", old.String(), d.State.BMCClock.String(), r.Source, c, "")
	return nil
}

func keys(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				deepMerge(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
}

// UpdateFirmware stages an update (firmware.update.simple / .multipart) and returns a task
// whose completion applies it per the component's applyOn.
//
// The task is the BMC's: it runs only while the BMC does. A BMC that goes down while the task
// runs (its reset, the loss of its standby power, a fault that makes it unreachable) loses the
// task: the image is not staged, and once the BMC is back its TaskService reports the task
// aborted (Exception), never running and never completed (abortFirmwareTasks, D53). A BMC that
// is down takes no update at all, as it takes no power request. An update whose task completed
// before the BMC went down is unaffected: a BMC component staged applyOn reset is applied when
// that BMC reboots (ResetBMC), which is how an operator updates a BMC (D51).
func (e *Engine) UpdateFirmware(r Request, components []string, version string) (model.Task, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return model.Task{}, err
	}
	if r.Source.Class == "stack" && d.HasBMC() && !e.bmcAvailable(d) {
		return model.Task{}, ErrBMCDown
	}
	c := e.logRequest(r, httpStatus(r, 202))
	fail := false
	for _, f := range e.faultsOn(d) {
		if v, ok := f.Sets["firmware.failNext"]; ok && v == true {
			fail = true
		}
	}
	if len(components) == 0 {
		for comp := range d.State.Firmware {
			components = append(components, comp)
		}
		sort.Strings(components)
	}
	t := model.Task{ID: strconv.Itoa(len(d.State.Tasks) + 1), State: "Running", Started: e.now, Canonical: r.Canonical, Message: "update " + version}
	d.State.Tasks = append(d.State.Tasks, t)
	idx := len(d.State.Tasks) - 1
	var timer uint64
	timer = e.after(20*time.Second, "firmware-task", func() {
		e.endFirmwareTask(d, timer)
		task := &d.State.Tasks[idx]
		task.Ends, task.Percent = e.now, 100
		if fail {
			task.State = "Exception"
			task.Message = "update failed"
			e.addLog(d, "Critical", "Firmware update failed.", "Update.1.0.UpdateFailed", "SEL", c)
			return
		}
		task.State = "Completed"
		immediate := false
		for _, comp := range components {
			fw := d.State.Firmware[comp]
			if fw == nil {
				continue
			}
			fw.Staged = version
			if fw.ApplyOn == "immediate" {
				immediate = true
			}
		}
		e.stateEvent(d.Path, "firmware.staged", nil, version, behaviourSrc, c, "")
		if immediate {
			e.applyStaged(d, "immediate", c)
		}
	})
	e.fwTasks[d.Path] = append(e.fwTasks[d.Path], fwTask{idx: idx, timer: timer})
	return t, nil
}

// fwTask is a firmware update task still running on a device's BMC: its index in the device's
// tasks and the timer that completes it.
type fwTask struct {
	idx   int
	timer uint64
}

// endFirmwareTask forgets a running firmware task whose timer has fired.
func (e *Engine) endFirmwareTask(d *model.Device, timer uint64) {
	run := e.fwTasks[d.Path]
	for i, t := range run {
		if t.timer == timer {
			run = append(run[:i], run[i+1:]...)
			break
		}
	}
	if len(run) == 0 {
		delete(e.fwTasks, d.Path)
	} else {
		e.fwTasks[d.Path] = run
	}
}

// abortFirmwareTasks ends the firmware tasks a BMC is running when it goes down: a BMC's tasks
// live in the BMC, so a reset, a loss of its standby power or a hang loses them. Each task is
// aborted where it stood (Exception, ended now, its percentage kept), its completion cancelled,
// and nothing is staged. A real BMC answers nothing while it is down; once back, bmcweb (which
// holds its tasks in memory) has forgotten the task and answers 404 for it. The twin keeps it,
// ended in Exception, as DMTF's TaskState says of a task "stopped due to an
// exception condition": a client polling it sees a failed update either way, and one keyed on
// the task id (RMS's jobs) finishes instead of waiting on a task that never ends (D53). Every
// path that takes a BMC down calls it: loseAC, ResetBMC, and a fault whose effect makes the BMC
// unavailable (injectLocked).
func (e *Engine) abortFirmwareTasks(d *model.Device, why string, cause uint64) {
	run := e.fwTasks[d.Path]
	if len(run) == 0 {
		return
	}
	delete(e.fwTasks, d.Path)
	for _, t := range run {
		e.cancel(t.timer)
		task := &d.State.Tasks[t.idx]
		if task.State != "Running" {
			continue
		}
		task.State, task.Ends = "Exception", e.now
		task.Message = "aborted: " + why
		e.stateEvent(d.Path, "task."+task.ID, "Running", "Exception", behaviourSrc, cause, "Warning")
	}
}

func (e *Engine) applyStaged(d *model.Device, on string, cause uint64) {
	applied := false
	for comp, fw := range d.State.Firmware {
		if fw.Staged == "" {
			continue
		}
		if on == "reset" && fw.ApplyOn == "ac-cycle" {
			continue
		}
		old := fw.Version
		fw.Version, fw.Staged = fw.Staged, ""
		e.stateEvent(d.Path, "firmware."+comp, old, fw.Version, behaviourSrc, cause, "")
		applied = true
	}
	if applied {
		e.fireTrigger(d, "firmware.applied", cause)
	}
}

// applyPendingOnBoot applies staged BIOS attributes and firmware when a host boots.
func (e *Engine) applyPendingOnBoot(d *model.Device, cause uint64) {
	if len(d.State.BiosPending) > 0 {
		if d.State.BiosPending["__reset__"] == true {
			d.State.Bios = map[string]any{}
		} else {
			for k, v := range d.State.BiosPending {
				d.State.Bios[k] = v
			}
		}
		e.stateEvent(d.Path, "bios", nil, "pending settings applied", behaviourSrc, cause, "")
		d.State.BiosPending = map[string]any{}
	}
	e.applyStaged(d, "reset", cause)
}

// boot decides how a powered-on host boots: a network boot at model fidelity is walked by
// the boot walker (05); anything else boots from disk.
func (e *Engine) boot(d *model.Device, cause uint64) {
	if !d.IsHost() {
		return
	}
	target := ""
	b := &d.State.Boot
	if b.OverrideEnabled == "Once" || b.OverrideEnabled == "Continuous" {
		target = b.OverrideTarget
		if b.OverrideEnabled == "Once" {
			b.OverrideEnabled = "Disabled"
			e.stateEvent(d.Path, "boot.overrideEnabled", "Once", "Disabled", behaviourSrc, cause, "")
		}
	}
	d.State.BootedSince = e.now
	if target == "" || target == "None" {
		d.State.Booted = "os"
		return
	}
	if target == "Pxe" || target == "UefiHttp" {
		d.State.Booted = "walking"
		for _, h := range e.hooks.OnBoot {
			h, t := h, target
			e.pendingHooks = append(e.pendingHooks, func() { h(d, t) })
		}
		return
	}
	d.State.Booted = "os"
}

// SetBooted records the step a boot walk reached.
func (e *Engine) SetBooted(path, step string, src schema.EventSource) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.M.DeviceBy[path]
	if d == nil {
		return
	}
	old := d.State.Booted
	d.State.Booted, d.State.BootedSince = step, e.now
	e.stateEvent(d.Path, "boot.walk", old, step, src, 0, "")
}

// AddSubscription records an EventService subscription (event.subscribe).
func (e *Engine) AddSubscription(r Request, s model.Subscription) (model.Subscription, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return s, err
	}
	s.ID = strconv.Itoa(len(d.State.Subscribers) + 1)
	d.State.Subscribers = append(d.State.Subscribers, s)
	e.logRequest(r, httpStatus(r, 201))
	return s, nil
}

// DeleteSubscription removes one.
func (e *Engine) DeleteSubscription(r Request, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return err
	}
	for i, s := range d.State.Subscribers {
		if s.ID == id {
			d.State.Subscribers = append(d.State.Subscribers[:i], d.State.Subscribers[i+1:]...)
			e.logRequest(r, httpStatus(r, 200))
			return nil
		}
	}
	return fmt.Errorf("no subscription %s", id)
}

// SetLease records the address a DHCP server leased to a device's port.
func (e *Engine) SetLease(path string, l *model.Lease, src schema.EventSource) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.M.DeviceBy[path]
	if d == nil {
		return
	}
	old := ""
	if d.State.Lease != nil {
		old = d.State.Lease.IP
	}
	d.State.Lease = l
	d.Endpoint.SiteIP = l.IP
	if old != l.IP {
		e.commit(schema.Event{Kind: "state", Source: src, Path: path, State: &schema.EventState{Field: "lease", From: old, To: l.IP},
			Data: map[string]any{"server": l.Server, "circuitId": l.Circuit, "remoteId": l.Remote, "vendorClass": l.VendorClass}})
	}
}

// SetUtilisation sets a host's load (an act; drives derived power and temperatures).
func (e *Engine) SetUtilisation(path string, u float64, src schema.EventSource) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.M.DeviceBy[path]
	if d == nil {
		return ErrNoDevice
	}
	old := d.State.Utilisation
	d.State.Utilisation = u
	e.stateEvent(path, "utilisation", old, u, src, 0, "")
	return nil
}

// SetLink pulls or replaces a cable (an act): cable.replaced clears link faults.
func (e *Engine) SetLink(id string, up bool, src schema.EventSource) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	l := e.M.LinkBy[id]
	if l == nil {
		for _, x := range e.M.Links {
			if x.A.Ref == id || x.B.Ref == id {
				l = x
			}
		}
	}
	if l == nil {
		return fmt.Errorf("no link or port %q", id)
	}
	c := e.commit(schema.Event{Kind: "act", Source: src, Path: l.ID, State: &schema.EventState{Field: "link", From: l.Up, To: up}})
	l.Up = up
	if up {
		for _, f := range append([]*ActiveFault{}, e.faults...) {
			if f.Port != nil && f.Port.Link == l {
				for _, t := range f.Spec.Clears.Triggers() {
					if t == "cable.replaced" {
						e.clearLocked(f, t, behaviourSrc, c)
					}
				}
			}
		}
	}
	e.rollup(c)
	return nil
}

// Reseat removes and re-adds a device in its slot (device.reseat): an AC cycle that also clears
// component-replacement faults.
func (e *Engine) Reseat(path string, src schema.EventSource) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.M.DeviceBy[path]
	if d == nil {
		return ErrNoDevice
	}
	c := e.commit(schema.Event{Kind: "act", Source: src, Path: path, Message: "reseat"})
	e.acCycle(d, src, c)
	e.fireTrigger(d, "device.reseat", c)
	return nil
}

// ReplaceComponent is a technician replacing a part (component.replaced).
func (e *Engine) ReplaceComponent(path string, src schema.EventSource) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.componentByPath(path)
	if c == nil {
		return fmt.Errorf("no component %s", path)
	}
	cause := e.commit(schema.Event{Kind: "act", Source: src, Path: path, Message: "replace component"})
	c.State.Replaced++
	for _, f := range append([]*ActiveFault{}, e.faults...) {
		if f.Component == c {
			for _, t := range f.Spec.Clears.Triggers() {
				if t == "component.replaced" {
					e.clearLocked(f, t, behaviourSrc, cause)
				}
			}
		}
	}
	e.rollup(cause)
	return nil
}

// Seed runs twin set-up against a device (seeding state from a captured tree) and commits it as
// a twin event, never as a request.
func (e *Engine) Seed(path, what string, f func(d *model.Device)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.M.DeviceBy[path]
	if d == nil {
		return
	}
	f(d)
	e.commit(schema.Event{Kind: "twin", Source: schema.EventSource{Class: "twin", Principal: "seed"}, Path: path, Message: what})
}

// SetEndpoint records where a device's interface is served on this host (twin set-up).
func (e *Engine) SetEndpoint(path, kind, addr string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.M.DeviceBy[path]
	if d == nil {
		return
	}
	switch kind {
	case "redfish":
		d.Endpoint.ListenTLS = addr
	case "ipmi":
		d.Endpoint.IPMI = addr
	case "snmp":
		d.Endpoint.SNMP = addr
	case "ssh":
		d.Endpoint.SSH = addr
	default:
		d.Endpoint.Listen = addr
	}
}

// CreateTask records a task or job a BMC scheduled (a BIOS settings job, an OEM job) that
// completes after a behaviour duration; its id is the one the BMC handed back.
func (e *Engine) CreateTask(r Request, id, message string, takes time.Duration) (model.Task, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	d, err := e.dev(r)
	if err != nil {
		return model.Task{}, err
	}
	e.logRequest(r, httpStatus(r, 202))
	t := model.Task{ID: id, State: "Running", Started: e.now, Canonical: r.Canonical, Message: message}
	d.State.Tasks = append(d.State.Tasks, t)
	idx := len(d.State.Tasks) - 1
	e.after(takes, "task", func() {
		d.State.Tasks[idx].State, d.State.Tasks[idx].Ends, d.State.Tasks[idx].Percent = "Completed", e.now, 100
	})
	return t, nil
}

// TaskCount is how many tasks a device's BMC has created.
func (e *Engine) TaskCount(path string) int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if d := e.M.DeviceBy[path]; d != nil {
		return len(d.State.Tasks)
	}
	return 0
}

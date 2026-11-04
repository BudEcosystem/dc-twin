// Package pack loads packs (docs/spec/06-packs-and-stacks.md): profiles, rack formats, fabric
// templates, fault catalogues and dialects, each named <pack>/<name>, and the stack definitions.
package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/schema"
	"sigs.k8s.io/yaml"
)

type Pack struct {
	ID       string
	Version  string
	Manifest schema.PackManifest
	Digest   string
	Files    []string
}

// FaultRef is one fault of a catalogue.
type FaultRef struct {
	ID        string
	Catalogue *schema.FaultCatalogue
	Spec      *schema.FaultSpec
}

type Registry struct {
	Packs      map[string]*Pack
	Profiles   map[string]*schema.DeviceProfile
	Formats    map[string]*schema.RackFormat
	Templates  map[string]*schema.FabricTemplate
	Catalogues map[string]*schema.FaultCatalogue
	Faults     map[string]*FaultRef
	Redfish    map[string]*schema.RedfishDialect
	Topics     map[string]*schema.TopicDialect
	MIBs       map[string]*schema.MibDialect
	Stacks     map[string]*schema.Stack
	Owner      map[string]string
}

func newRegistry() *Registry {
	return &Registry{
		Packs: map[string]*Pack{}, Profiles: map[string]*schema.DeviceProfile{},
		Formats: map[string]*schema.RackFormat{}, Templates: map[string]*schema.FabricTemplate{},
		Catalogues: map[string]*schema.FaultCatalogue{}, Faults: map[string]*FaultRef{},
		Redfish: map[string]*schema.RedfishDialect{}, Topics: map[string]*schema.TopicDialect{},
		MIBs: map[string]*schema.MibDialect{}, Stacks: map[string]*schema.Stack{}, Owner: map[string]string{},
	}
}

// Load reads every pack under packsFS (one directory per pack) and every stack under stacksFS.
func Load(packsFS, stacksFS fs.FS) (*Registry, error) {
	r := newRegistry()
	dirs, err := fs.ReadDir(packsFS, ".")
	if err != nil {
		return nil, err
	}
	var errs []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if err := r.loadPack(packsFS, d.Name()); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if stacksFS != nil {
		_ = fs.WalkDir(stacksFS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, "stack.yaml") {
				return nil
			}
			data, _ := fs.ReadFile(stacksFS, p)
			var s schema.Stack
			if err := schema.Decode(data, &s); err != nil {
				errs = append(errs, fmt.Sprintf("stacks/%s: %v", p, err))
				return nil
			}
			if err := schema.CheckHeader(schema.Header{APIVersion: s.APIVersion, Kind: s.Kind, Metadata: s.Metadata}, "Stack"); err != nil {
				errs = append(errs, fmt.Sprintf("stacks/%s: %v", p, err))
				return nil
			}
			if !s.Spec.IsNot.Declared() {
				errs = append(errs, fmt.Sprintf("stacks/%s: isNot is required", p))
			}
			r.Stacks[s.Metadata.Name] = &s
			return nil
		})
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return r, fmt.Errorf("packs: %s", strings.Join(errs, "; "))
	}
	return r, r.crossCheck()
}

func (r *Registry) loadPack(fsys fs.FS, id string) error {
	if strings.Contains(id, "/") {
		return fmt.Errorf("pack id %q contains /", id)
	}
	mdata, err := fs.ReadFile(fsys, path.Join(id, "pack.yaml"))
	if err != nil {
		return fmt.Errorf("pack %s: %w", id, err)
	}
	var m schema.PackManifest
	if err := schema.Decode(mdata, &m); err != nil {
		return fmt.Errorf("pack %s/pack.yaml: %w", id, err)
	}
	if m.Metadata.Name != id {
		return fmt.Errorf("pack %s: manifest names %q", id, m.Metadata.Name)
	}
	p := &Pack{ID: id, Version: m.Metadata.Version, Manifest: m}
	h := sha256.New()
	var errs []string
	_ = fs.WalkDir(fsys, id, func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(fp, ".yaml") || strings.HasSuffix(fp, ".json")) {
			return nil
		}
		data, _ := fs.ReadFile(fsys, fp)
		h.Write([]byte(fp))
		h.Write(data)
		p.Files = append(p.Files, fp)
		if strings.HasSuffix(fp, "pack.yaml") {
			return nil
		}
		for _, doc := range schema.SplitDocuments(data) {
			if err := r.loadObject(id, fp, doc); err != nil {
				errs = append(errs, err.Error())
			}
		}
		return nil
	})
	p.Digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	r.Packs[id] = p
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (r *Registry) loadObject(packID, file string, doc []byte) error {
	h, err := schema.KindOf(doc)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	if err := schema.CheckHeader(h, h.Kind); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	name := h.Metadata.Name
	if prefix, _, _ := strings.Cut(name, "/"); prefix != packID {
		return fmt.Errorf("%s: %s %q: an object's prefix must equal its pack (%s)", file, h.Kind, name, packID)
	}
	key := h.Kind + ":" + name
	if prev, dup := r.Owner[key]; dup {
		return fmt.Errorf("%s: %s %q already defined in %s", file, h.Kind, name, prev)
	}
	r.Owner[key] = file
	dec := func(v any) error {
		if err := schema.Decode(doc, v); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		return nil
	}
	switch h.Kind {
	case "DeviceProfile":
		var o schema.DeviceProfile
		if err := dec(&o); err != nil {
			return err
		}
		if !o.Spec.IsNot.Declared() {
			return fmt.Errorf("%s: isNot is required", file)
		}
		r.Profiles[name] = &o
	case "RackFormat":
		var o schema.RackFormat
		if err := dec(&o); err != nil {
			return err
		}
		if !o.Spec.IsNot.Declared() {
			return fmt.Errorf("%s: isNot is required", file)
		}
		r.Formats[name] = &o
	case "FabricTemplate":
		var o schema.FabricTemplate
		if err := dec(&o); err != nil {
			return err
		}
		if !o.Spec.IsNot.Declared() {
			return fmt.Errorf("%s: isNot is required", file)
		}
		r.Templates[name] = &o
	case "FaultCatalogue":
		var o schema.FaultCatalogue
		if err := dec(&o); err != nil {
			return err
		}
		r.Catalogues[name] = &o
		for i := range o.Spec.Faults {
			f := &o.Spec.Faults[i]
			if !f.IsNot.Declared() {
				return fmt.Errorf("%s: fault %s: isNot is required", file, f.Name)
			}
			id := name + "/" + f.Name
			r.Faults[id] = &FaultRef{ID: id, Catalogue: &o, Spec: f}
		}
	case "RedfishDialect":
		var o schema.RedfishDialect
		if err := dec(&o); err != nil {
			return err
		}
		if !o.Spec.IsNot.Declared() {
			return fmt.Errorf("%s: isNot is required", file)
		}
		r.Redfish[name] = &o
	case "TopicDialect":
		var o schema.TopicDialect
		if err := dec(&o); err != nil {
			return err
		}
		r.Topics[name] = &o
	case "MibDialect":
		var o schema.MibDialect
		if err := dec(&o); err != nil {
			return err
		}
		r.MIBs[name] = &o
	default:
		return fmt.Errorf("%s: unknown kind %q", file, h.Kind)
	}
	return nil
}

// crossCheck resolves references between pack objects.
func (r *Registry) crossCheck() error {
	var errs []string
	for name, p := range r.Profiles {
		for _, c := range p.Spec.Faults {
			if _, ok := r.Catalogues[c]; !ok {
				errs = append(errs, fmt.Sprintf("profile %s: fault catalogue %s unknown", name, c))
			}
		}
		if p.Spec.BMC != nil {
			if _, ok := r.Redfish[p.Spec.BMC.Dialect]; !ok {
				errs = append(errs, fmt.Sprintf("profile %s: dialect %s unknown", name, p.Spec.BMC.Dialect))
			}
		}
		for _, c := range p.Spec.Components {
			if c.Profile != "" {
				if _, ok := r.Profiles[c.Profile]; !ok {
					errs = append(errs, fmt.Sprintf("profile %s: component profile %s unknown", name, c.Profile))
				}
			}
		}
	}
	for name, d := range r.Redfish {
		if d.Spec.Inherits != "" {
			if _, ok := r.Redfish[d.Spec.Inherits]; !ok {
				errs = append(errs, fmt.Sprintf("dialect %s inherits unknown %s", name, d.Spec.Inherits))
			}
		}
	}
	for name, f := range r.Formats {
		for _, s := range f.Spec.Slots {
			if strings.HasPrefix(s.Profile, "$") {
				continue
			}
			if _, ok := r.Profiles[s.Profile]; !ok {
				errs = append(errs, fmt.Sprintf("format %s: slot %s: profile %s unknown", name, s.Names, s.Profile))
			}
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("packs: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Lock is the resolved pin set written to <estate>.lock.
type Lock struct {
	APIVersion string      `json:"apiVersion"`
	Kind       string      `json:"kind"`
	Estate     string      `json:"estate"`
	Packs      []LockEntry `json:"packs"`
}

type LockEntry struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Via     string `json:"via,omitempty"`
}

// Resolve checks the estate's exact pins against the loaded packs, adds transitive dependencies,
// and returns the lock.
func (r *Registry) Resolve(estate string, pins []string) (*Lock, map[string]bool, error) {
	lock := &Lock{APIVersion: schema.APIVersion, Kind: "Lock", Estate: estate}
	enabled := map[string]bool{}
	var errs []string
	var visit func(pin, via string)
	visit = func(pin, via string) {
		id, version, ok := strings.Cut(pin, "@")
		if !ok {
			errs = append(errs, fmt.Sprintf("pack %q: pin an exact version, name@version", pin))
			return
		}
		p, found := r.Packs[id]
		if !found {
			errs = append(errs, fmt.Sprintf("pack %s: not found", id))
			return
		}
		if p.Version != version {
			errs = append(errs, fmt.Sprintf("pack %s: pinned %s, available %s", id, version, p.Version))
			return
		}
		if enabled[id] {
			return
		}
		enabled[id] = true
		lock.Packs = append(lock.Packs, LockEntry{Name: id, Version: version, Digest: p.Digest, Via: via})
		for _, dep := range p.Manifest.Spec.DependsOn {
			visit(dep, id)
		}
	}
	for _, pin := range pins {
		visit(pin, "")
	}
	sort.Slice(lock.Packs, func(i, j int) bool { return lock.Packs[i].Name < lock.Packs[j].Name })
	if len(errs) > 0 {
		return lock, enabled, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return lock, enabled, nil
}

// PackOf returns the pack id of a <pack>/<name> object.
func PackOf(name string) string {
	p, _, _ := strings.Cut(name, "/")
	return p
}

// Marshal writes a lock as YAML.
func (l *Lock) Marshal() []byte {
	b, _ := yaml.Marshal(l)
	return b
}

// Dialect returns a Redfish dialect with its inheritance chain merged (child wins).
func (r *Registry) Dialect(name string) (*schema.RedfishDialect, []string) {
	d, ok := r.Redfish[name]
	if !ok {
		return nil, nil
	}
	chain := []string{name}
	if d.Spec.Inherits == "" {
		return d, chain
	}
	parent, pchain := r.Dialect(d.Spec.Inherits)
	merged := *parent
	merged.Metadata = d.Metadata
	s := parent.Spec
	c := d.Spec
	if c.Backend != "" {
		s.Backend = c.Backend
	}
	if c.Mockup != "" {
		s.Mockup = c.Mockup
	}
	if len(c.ServiceRoot) > 0 {
		sr := map[string]any{}
		for k, v := range s.ServiceRoot {
			sr[k] = v
		}
		for k, v := range c.ServiceRoot {
			sr[k] = v
		}
		s.ServiceRoot = sr
	}
	for _, f := range []struct {
		dst *string
		src string
	}{{&s.System, c.System}, {&s.Manager, c.Manager}, {&s.Chassis, c.Chassis}, {&s.BMCPort, c.BMCPort}} {
		if f.src != "" {
			*f.dst = f.src
		}
	}
	if c.Factory.User != "" {
		s.Factory = c.Factory
	}
	if c.FirmwareFromProfile {
		s.FirmwareFromProfile = true
	}
	if len(c.Bind) > 0 {
		s.Bind = c.Bind
	}
	if len(c.Replace) > 0 {
		s.Replace = c.Replace
	}
	if c.SeedAccounts {
		s.SeedAccounts = true
	}
	if len(c.DynamicCollections) > 0 {
		s.DynamicCollections = c.DynamicCollections
	}
	s.Scrub = append(append([]schema.ScrubRule{}, s.Scrub...), c.Scrub...)
	s.Overlays = append(append([]schema.Overlay{}, s.Overlays...), c.Overlays...)
	if len(c.Components) > 0 {
		s.Components = c.Components
	}
	if len(c.Sensors) > 0 {
		s.Sensors = c.Sensors
	}
	if len(c.Leak) > 0 {
		s.Leak = c.Leak
	}
	s.Actions = append(append([]schema.ActionBind{}, c.Actions...), s.Actions...)
	s.Requests = append(append([]schema.RequestMap{}, c.Requests...), s.Requests...)
	if len(c.Firmware) > 0 {
		s.Firmware = c.Firmware
	}
	s.Quirks = append(append([]schema.QuirkSpec{}, s.Quirks...), c.Quirks...)
	if c.Identity != (schema.DialectIdentity{}) {
		s.Identity = c.Identity
	}
	s.IsNot.All = append(append([]string{}, s.IsNot.All...), c.IsNot.All...)
	s.Sources = append(append([]schema.Source{}, s.Sources...), c.Sources...)
	merged.Spec = s
	return &merged, append(chain, pchain...)
}

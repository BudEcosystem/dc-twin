// Package bms is the twin's building management system (docs/spec/05-interfaces.md — Facility):
// a publisher on the stack's MQTT broker in the estate's BMS topic dialect. It publishes the
// points of every device whose profile has a `bms` mapping (rack leak points, CDUs) and the
// per-rack points the facility model derives (power, liquid supply, isolation status), and it
// answers the integration's isolation requests by asking the engine to isolate the rack.
//
// It never changes model state itself: values are read from committed state through the
// engine's accessors, and an isolation request becomes an engine.Isolate call after the
// dialect's delay. Every publication is kept in the publisher's log, which is what a
// scenario's `bms` observation reads (docs/spec/08-scenarios-and-faults.md).
//
// What it publishes is the AsyncAPI's. Where a stack component parses another shape, an opt-in
// mirror publishes that shape beside it (D40): nico-legacy for NICo's dsx-exchange-consumer,
// nico-api for nico-api's own integration, whose requests the BMS then takes as it takes the
// AsyncAPI's.
package bms

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

func init() { twin.Register("bms", func() adapter.Adapter { return New() }) }

// The optional mirrors (D40, Config.ConsumerMirror): messages in a shape a stack component parses
// where it diverges from the AsyncAPI, published beside the AsyncAPI messages, never instead.
const (
	// MirrorNICoLegacy mirrors the rack leak points in the pre-1.0 shape NICo's
	// dsx-exchange-consumer parses.
	MirrorNICoLegacy = "nico-legacy"
	// MirrorNICoAPI mirrors the metadata of the isolation requests in the shape nico-api's own
	// DSX Exchange integration (crates/bms-dsx-exchange) parses, and takes nico-api's requests
	// from the value topics it derives from that metadata.
	MirrorNICoAPI = "nico-api"
)

// nicoAPIMetadataPrefix is where nico-api learns routing from: its subscription
// (crates/rack/src/bms_client.rs:39, BMS/v1/PUB/Metadata/#) and the prefix its value-topic
// derivation requires (crates/bms-dsx-exchange/src/lib.rs:279), both fixed in its source.
const nicoAPIMetadataPrefix = "BMS/v1/PUB/Metadata/"

// Config is what a caller may tune. The zero value of a field means its default.
type Config struct {
	// Integration is the identifier of the integration that owns the integration-published
	// Rack points (RackLeakDetectTray and the isolation requests). Default "NICO", the value
	// DSX Exchange's own exemplar BMS data carries.
	Integration string
	// MetadataEvery, ValueEvery and HeartbeatEvery are the AsyncAPI's cadences: metadata
	// retained at start and every 100 s, values on change and every 100 s, the BMS heartbeat
	// every 10 s. They are wall durations: the stack's consumer runs on wall time.
	MetadataEvery, ValueEvery, HeartbeatEvery time.Duration
	// SampleEvery is how often derived analog points (temperatures following their lag) are
	// re-read between committed events. Default 1 s.
	SampleEvery time.Duration
	// ConnectWait is how long Start waits for the first connection and the initial
	// publication before returning (the client keeps retrying afterwards). Default 10 s.
	ConnectWait time.Duration
	// ClientID is the MQTT client identifier. Default "dc-twin-bms-<estate>-<random>".
	ClientID string
	// ConsumerMirror is a comma-separated list of mirrors; off (empty) by default, and a stack
	// output "bmsConsumerMirror" sets it when the config does not. NICo runs its consumer and
	// nico-api against one broker, so a NICo stack names both ("nico-legacy,nico-api"; quote
	// it in a YAML flow mapping, where a bare comma separates entries).
	//
	// MirrorNICoLegacy also publishes each Rack leak point in the shape NICo's
	// dsx-exchange-consumer parses: BMS/v1/PUB/Rack/<LeakDetectRack|LeakSensorFaultRack>/
	// <tagPath>/{Metadata,Value}, metadata {pointType, objectType, rackName, rackID}, value
	// {value, timestamp in seconds}.
	//
	// MirrorNICoAPI also publishes, for each RackElectricalIsolationRequest and
	// RackLiquidIsolationRequest, retained metadata {pointType, objectType, rackName, rackId,
	// integration} on BMS/v1/PUB/Metadata/Rack/<pointType>/<tagPath>/nico-api, and takes nico-api's
	// {value: 0|1, timestamp: ms, quality: "1"} from BMS/v1/<integration>/Value/Rack/<pointType>/
	// <tagPath>/nico-api as it takes an AsyncAPI request.
	ConsumerMirror string
}

// DefaultConfig is the AsyncAPI's cadence and the NICO integration.
func DefaultConfig() Config {
	return Config{Integration: "NICO", MetadataEvery: 100 * time.Second, ValueEvery: 100 * time.Second,
		HeartbeatEvery: 10 * time.Second, SampleEvery: time.Second, ConnectWait: 10 * time.Second}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.Integration == "" {
		c.Integration = d.Integration
	}
	if c.MetadataEvery <= 0 {
		c.MetadataEvery = d.MetadataEvery
	}
	if c.ValueEvery <= 0 {
		c.ValueEvery = d.ValueEvery
	}
	if c.HeartbeatEvery <= 0 {
		c.HeartbeatEvery = d.HeartbeatEvery
	}
	if c.SampleEvery <= 0 {
		c.SampleEvery = d.SampleEvery
	}
	if c.ConnectWait <= 0 {
		c.ConnectWait = d.ConnectWait
	}
	return c
}

// Publication is one MQTT message in the publisher's log (or, from Received, one message an
// integration published to the BMS).
type Publication struct {
	Topic    string
	Payload  []byte
	Retained bool
	// Time is the twin's clock at publication: wall time, or the virtual clock on a virtual
	// estate. Value payload timestamps carry the same instant.
	Time time.Time
	// Kind is "metadata", "value", "heartbeat", "mirror" or, for Received, "integration".
	Kind string
}

// Value decodes a value payload's `value` (false when the payload has none, or is metadata).
func (p Publication) Value() (float64, bool) {
	var v struct {
		Value *float64 `json:"value"`
	}
	if json.Unmarshal(p.Payload, &v) != nil || v.Value == nil {
		return 0, false
	}
	return *v.Value, true
}

// point is one BMS point: a topic pair and how its value is read from the model.
type point struct {
	objectType, pointType, tagPath string
	valueTopic, metaTopic          string
	meta                           map[string]any
	def                            pointDef
	source                         string // the device or rack it is derived from
	rack                           *model.Rack
	read                           func() (float64, bool)

	// integration-owned points
	integTopic string
	effect     string // electrical or liquid for an isolation request; "" for RackLeakDetectTray
	status     *point // the status point a request answers on
	delay      time.Duration

	legacy string     // the NICo consumer's point type, when mirrored
	napi   *apiMirror // the nico-api mirror of an isolation request, when mirrored
}

// apiMirror is an isolation request as nico-api reads it: metadata on a topic of its own (the
// AsyncAPI metadata keeps its topic, and a retained topic holds one message), and the value topic
// nico-api derives from that topic and publishes the request on.
type apiMirror struct {
	metaTopic, valueTopic string
	meta                  map[string]any
}

type action struct {
	due    time.Time // on the engine clock
	rack   *model.Rack
	kind   string
	on     bool
	status *point
}

// Adapter is the BMS publisher of one estate.
type Adapter struct {
	cfg     Config
	rt      *adapter.Runtime
	e       *engine.Engine
	log     *slog.Logger
	dialect *schema.TopicDialect
	why     string // set when the adapter is inactive
	site    string
	estate  string

	points    []*point
	heartbeat *point
	integHB   *point            // the integration's HeartbeatTimestampIntegration (metadata only)
	requests  map[string]*point // integration value topic -> point (AsyncAPI shape)
	apiReqs   map[string]*point // nico-api mirror value topic -> the request point it mirrors
	mirrors   map[string]bool
	skipped   []string

	broker     string // tcp://host:port
	brokerFrom string
	embedded   *mqtt.Server
	client     paho.Client

	stop      chan struct{}
	connected chan struct{}
	ready     chan struct{}
	readyOnce sync.Once
	wg        sync.WaitGroup

	last map[string]float64 // last published value per value topic (loop goroutine only)

	mu       sync.Mutex
	pubs     []Publication
	received []Publication
	lastReq  map[string]float64
	pending  map[string]action
	pubErrs  int
	lastErr  string
	conns    int
	rejected int // AsyncAPI-shaped integration messages that were not a request
	apiRej   int // messages on the nico-api mirror's topics not in nico-api's shape
	hb       heartbeatLog
}

// heartbeatLog is what the integration's heartbeats showed: how many, the last value (epoch
// milliseconds by the AsyncAPI and by nico-api), and in which shape its quality came.
type heartbeatLog struct {
	n, bad        int
	last          int64
	stringQuality bool // nico-api's quality "1" (a string; the AsyncAPI's is an integer)
}

// New is the adapter with the AsyncAPI's cadences.
func New() *Adapter { return NewWithConfig(DefaultConfig()) }

// NewWithConfig is the adapter with tuned cadences (tests) or another integration id.
func NewWithConfig(c Config) *Adapter {
	return &Adapter{cfg: c.withDefaults(), requests: map[string]*point{}, apiReqs: map[string]*point{},
		mirrors: map[string]bool{}, last: map[string]float64{}, lastReq: map[string]float64{}, pending: map[string]action{}}
}

// parseMirrors reads Config.ConsumerMirror: names separated by commas, blanks ignored.
func parseMirrors(s string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, f := range strings.Split(s, ",") {
		switch f = strings.TrimSpace(f); f {
		case "":
		case MirrorNICoLegacy, MirrorNICoAPI:
			out[f] = true
		default:
			return nil, fmt.Errorf("no mirror %q (the mirrors are %q and %q)", f, MirrorNICoLegacy, MirrorNICoAPI)
		}
	}
	return out, nil
}

func (a *Adapter) Name() string { return "bms" }

// Start builds the point list, connects to the broker (the stack's, or a twin-hosted one when
// the estate names none) and publishes metadata, values and the heartbeat.
func (a *Adapter) Start(ctx context.Context, rt *adapter.Runtime) error {
	a.rt, a.e = rt, rt.Engine
	a.log = rt.Log
	if a.log == nil {
		a.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	var ref *schema.DialectRef
	a.e.Read(func(m *model.Model) {
		a.site, a.estate = m.Site, m.Name
		if m.Estate != nil {
			ref = m.Estate.Spec.Facility.BMS
		}
	})
	if ref == nil {
		a.why = "inactive: the estate has no facility.bms"
		return nil
	}
	if a.dialect = rt.Reg.Topics[ref.Dialect]; a.dialect == nil {
		a.why = fmt.Sprintf("inactive: facility.bms dialect %q is in no loaded pack", ref.Dialect)
		return nil
	}
	if v := rt.Outputs["bmsConsumerMirror"]; v != "" && a.cfg.ConsumerMirror == "" {
		a.cfg.ConsumerMirror = v
	}
	mirrors, err := parseMirrors(a.cfg.ConsumerMirror)
	if err != nil {
		return fmt.Errorf("bms: consumer mirror %q: %w", a.cfg.ConsumerMirror, err)
	}
	a.mirrors = mirrors
	a.build()

	switch {
	case ref.Broker != "":
		a.broker, a.brokerFrom = brokerURL(ref.Broker), "the stack's broker (estate facility.bms.broker)"
	case rt.Outputs["exchangeBroker"] != "":
		a.broker, a.brokerFrom = brokerURL(rt.Outputs["exchangeBroker"]), "the stack's broker (stack output exchangeBroker)"
	default:
		addr, err := a.startEmbedded()
		if err != nil {
			return fmt.Errorf("bms: twin-hosted broker: %w", err)
		}
		a.broker = "tcp://" + addr
		a.brokerFrom = fmt.Sprintf("twin-hosted broker (mochi-mqtt %s, anonymous, no ACLs), because the estate names no broker and no stack output exchangeBroker", mqtt.Version)
	}

	id := a.cfg.ClientID
	if id == "" {
		b := make([]byte, 3)
		_, _ = rand.Read(b)
		id = "dc-twin-bms-" + tagSafe(a.estate) + "-" + hex.EncodeToString(b)
	}
	a.stop, a.connected, a.ready = make(chan struct{}), make(chan struct{}, 1), make(chan struct{})
	opts := paho.NewClientOptions().AddBroker(a.broker).SetClientID(id).SetProtocolVersion(4).
		SetCleanSession(true).SetAutoReconnect(true).SetConnectRetry(true).
		SetConnectRetryInterval(time.Second).SetMaxReconnectInterval(10 * time.Second).
		SetKeepAlive(30 * time.Second).SetConnectTimeout(5 * time.Second).
		SetOnConnectHandler(func(paho.Client) {
			select {
			case a.connected <- struct{}{}:
			default:
			}
		}).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			a.log.Warn("bms: broker connection lost", "broker", a.broker, "err", err)
		})
	if u := rt.Outputs["exchangeUsername"]; u != "" {
		opts.SetUsername(u)
		opts.SetPassword(rt.Outputs["exchangePassword"])
	}
	a.client = paho.NewClient(opts)
	a.client.Connect() // with ConnectRetry the token completes only once connected; the loop reacts to OnConnect

	a.wg.Add(1)
	go a.loop()
	select {
	case <-a.ready:
	case <-time.After(a.cfg.ConnectWait):
		a.log.Warn("bms: not connected yet; retrying in the background", "broker", a.broker)
	case <-ctx.Done():
	}
	return nil
}

// Stop disconnects and stops the twin-hosted broker.
func (a *Adapter) Stop() error {
	if a.stop == nil {
		return nil
	}
	select {
	case <-a.stop:
		return nil
	default:
		close(a.stop)
	}
	a.wg.Wait()
	if a.client != nil {
		a.client.Disconnect(250)
	}
	if a.embedded != nil {
		closeEmbedded(a.embedded)
	}
	return nil
}

// Broker is the broker the publisher connects to, as an MQTT URL (tcp://host:port).
func (a *Adapter) Broker() string { return a.broker }

// Embedded reports whether the broker is the twin-hosted one.
func (a *Adapter) Embedded() bool { return a.embedded != nil }

// Ready is closed after the first connection's initial publication.
func (a *Adapter) Ready() <-chan struct{} { return a.ready }

// Published returns the publications whose topic matches an MQTT filter (+ and #), at or
// after since, in publication order. It is the publisher's log scenarios observe.
func (a *Adapter) Published(filter string, since time.Time) []Publication {
	a.mu.Lock()
	defer a.mu.Unlock()
	return filterLog(a.pubs, filter, since)
}

// Received returns what integrations published to the BMS's subscribed topics.
func (a *Adapter) Received(filter string, since time.Time) []Publication {
	a.mu.Lock()
	defer a.mu.Unlock()
	return filterLog(a.received, filter, since)
}

func filterLog(log []Publication, filter string, since time.Time) []Publication {
	var out []Publication
	for _, p := range log {
		if !p.Time.Before(since) && Match(filter, p.Topic) {
			out = append(out, p)
		}
	}
	return out
}

// IntegrationTopic is where the integration publishes a rack's point value (pointType one of
// the integration-published Rack points), derived from the metadata topic as the AsyncAPI says.
func (a *Adapter) IntegrationTopic(rackName, pointType string) (string, bool) {
	for t, p := range a.requests {
		if p.pointType == pointType && p.rack != nil && p.rack.Name == rackName {
			return t, true
		}
	}
	return "", false
}

// Report is the adapter's fidelity-report contribution.
func (a *Adapter) Report() adapter.Report {
	r := adapter.Report{Adapter: "bms"}
	if a.dialect == nil {
		r.Lines = []string{a.why}
		return r
	}
	a.mu.Lock()
	npub, errs, lastErr, conns, nrecv, rejected := len(a.pubs), a.pubErrs, a.lastErr, a.conns, len(a.received), a.rejected
	apiRej, hb := a.apiRej, a.hb
	a.mu.Unlock()
	byType := map[string]int{}
	values, integ := 0, 0
	for _, p := range a.points {
		byType[p.objectType]++
		if p.def.integration {
			integ++
		} else {
			values++
		}
	}
	var types []string
	for k, v := range byType {
		types = append(types, fmt.Sprintf("%d %s", v, k))
	}
	sort.Strings(types)
	r.Served = len(a.points)
	r.Lines = append(r.Lines,
		fmt.Sprintf("publisher: %s@%s over MQTT 3.1.1 (paho), QoS 0; metadata retained", a.dialect.Metadata.Name, a.dialect.Metadata.Version),
		fmt.Sprintf("broker: %s at %s; %d connection(s) made", a.brokerFrom, a.broker, conns),
		fmt.Sprintf("points: %s; %d published by the BMS, %d owned by integration %s (metadata only, values subscribed)", strings.Join(types, ", "), values, integ, a.cfg.Integration),
		fmt.Sprintf("cadence: metadata at start and every %s; values on change and every %s; HeartbeatTimestampBms every %s", a.cfg.MetadataEvery, a.cfg.ValueEvery, a.cfg.HeartbeatEvery),
		fmt.Sprintf("log: %d publications, %d publish errors%s; %d integration messages received, %d rejected", npub, errs, errSuffix(lastErr), nrecv, rejected),
	)
	for _, q := range a.dialect.Spec.Requests {
		r.Lines = append(r.Lines, fmt.Sprintf("isolation: %s=1 isolates the rack (%s) after %ds × timeScale %.3g, answered on %s; 0 releases", q.PointType, q.Effect, q.DelayS, a.e.Scale(), q.Status))
	}
	if a.integHB != nil {
		l := fmt.Sprintf("integration heartbeat: HeartbeatTimestampIntegration metadata for %s; %d values received on %s", a.cfg.Integration, hb.n, a.integHB.integTopic)
		if hb.n > 0 {
			l += fmt.Sprintf(", the last %s", time.UnixMilli(hb.last).UTC().Format(time.RFC3339Nano))
		}
		if hb.stringQuality {
			l += `; quality came as the string "1" (nico-api's shape, outside the AsyncAPI)`
		}
		if hb.bad > 0 {
			l += fmt.Sprintf("; %d were not {value, timestamp, quality}", hb.bad)
		}
		r.Lines = append(r.Lines, l)
	}
	if a.mirrors[MirrorNICoLegacy] {
		r.Lines = append(r.Lines, "consumer mirror nico-legacy: Rack leak points are also published in the pre-1.0 shape NICo's dsx-exchange-consumer parses (BMS/v1/PUB/Rack/<LeakDetectRack|LeakSensorFaultRack>/<tagPath>/{Metadata,Value}); these messages are outside the AsyncAPI")
	}
	if a.mirrors[MirrorNICoAPI] {
		r.Lines = append(r.Lines, fmt.Sprintf("consumer mirror nico-api: %d isolation requests also have metadata in the shape nico-api's crates/bms-dsx-exchange parses ({pointType, objectType, rackName, rackId, integration} on BMS/v1/PUB/Metadata/Rack/<pointType>/<tagPath>/nico-api, outside the AsyncAPI), and nico-api's {value, timestamp, quality: \"1\"} on the value topic it derives is a request like the AsyncAPI one", len(a.apiReqs)))
		if apiRej > 0 {
			r.Lines = append(r.Lines, fmt.Sprintf("consumer mirror nico-api: %d messages on its topics were not nico-api's {value: 0|1, timestamp, quality: string} and were ignored", apiRej))
		}
	}
	r.Lines = append(r.Lines, a.skipped...)
	if a.embedded != nil {
		r.Stubs = append(r.Stubs, "DSX Exchange event bus: a twin-hosted MQTT broker stands in for the stack's NATS broker and auth callout")
	}
	r.IsNot = append(r.IsNot, a.dialect.Spec.IsNot.All...)
	r.IsNot = append(r.IsNot,
		"Every value carries quality 1: the twin has no notion of an untrustworthy reading.",
		"Rack liquid points are the cooling loop's CDU readings: supply and return temperature are the loop's, flow is the loop's flow split equally across its racks that are not liquid-isolated (0 when isolated). RackLiquidDifferentialPressure, its setpoint and RackControlValvePosition are not published.",
		"Liquid isolation is recorded in the model and shown as RackLiquidIsolationStatus and zero RackLiquidFlow; the engine does not yet model its thermal effect on the rack's trays.",
		"The isolation delay runs on the adapter's scheduler against the engine clock, not as an engine behaviour.",
		"The BMS publishes HeartbeatTimestampIntegration metadata for its one integration and logs the heartbeats it receives, but neither echoes them (HeartbeatEchoBms) nor watches them: it does not revert a request when its integration goes silent, and applies no guardrails beyond accepting 0 and 1.",
		"NICo's dsx-exchange-consumer (infra-controller main, read 2026-10-18) parses a pre-1.0 BMS shape — BMS/v1/{pointPath}/{Metadata,Value} topics, point types LeakDetectRack/LeakSensorFaultRack/LeakDetectRackTray, rackName and rackID, timestamps in seconds — so it does not recognise these AsyncAPI 1.0.0 messages unless the nico-legacy mirror is on.",
		"nico-api's own DSX Exchange integration (crates/bms-dsx-exchange, infra-controller 02b92f18) requires integration on every metadata message and rackName/rackId on the isolation requests, and publishes quality as the string \"1\": it routes isolation requests only through the nico-api mirror's metadata, and counts every other metadata message as a parse error.",
	)
	if a.embedded != nil {
		r.IsNot = append(r.IsNot, "The twin-hosted broker is not DSX Exchange: no NATS, no auth callout, no ACLs, no federation.")
	}
	if rejected > 0 {
		r.Lines = append(r.Lines, fmt.Sprintf("integration: %d messages were not {value, timestamp, quality} with value 0 or 1 and were ignored", rejected))
	}
	return r
}

func errSuffix(s string) string {
	if s == "" {
		return ""
	}
	return " (last: " + s + ")"
}

func brokerURL(s string) string {
	if strings.Contains(s, "://") {
		return s
	}
	return "tcp://" + s
}

// startEmbedded starts a twin-hosted MQTT broker on a substrate port.
func (a *Adapter) startEmbedded() (string, error) {
	var ln net.Listener
	var err error
	if a.rt.Net != nil {
		ln, err = a.rt.Net.Listen(a.site+"/facility/bms", "mqtt")
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return "", err
	}
	srv := mqtt.New(&mqtt.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := srv.AddHook(new(allowAll), nil); err != nil {
		ln.Close()
		return "", err
	}
	if err := srv.AddListener(listeners.NewNet("dc-twin-bms", ln)); err != nil {
		ln.Close()
		return "", err
	}
	if err := srv.Serve(); err != nil {
		ln.Close()
		return "", err
	}
	a.embedded = srv
	return ln.Addr().String(), nil
}

// ---------------------------------------------------------------- the point list

func (a *Adapter) allowed(objectType, pointType string) bool {
	if objectType != "Rack" || len(a.dialect.Spec.PointTypes) == 0 {
		return true
	}
	for _, p := range a.dialect.Spec.PointTypes {
		if p == pointType {
			return true
		}
	}
	return false
}

func (a *Adapter) skip(f string, args ...any) {
	a.skipped = append(a.skipped, "not published: "+fmt.Sprintf(f, args...))
}

func (a *Adapter) newPoint(objectType, pointType, tag string, def pointDef, meta map[string]any) *point {
	vars := map[string]string{"objectType": objectType, "pointType": pointType, "tagPath": tag, "site": tagSafe(a.site)}
	p := &point{objectType: objectType, pointType: pointType, tagPath: tag, def: def, meta: meta,
		valueTopic: expand(a.dialect.Spec.Value, vars), metaTopic: expand(a.dialect.Spec.Metadata, vars)}
	if def.stateText != nil {
		meta["stateText"] = def.stateText
	} else if def.engUnit != "" {
		meta["engUnit"] = def.engUnit
	}
	if def.integration {
		meta["integration"] = a.cfg.Integration
		t, ok := integrationTopic(p.metaTopic, a.cfg.Integration)
		if !ok {
			a.skip("%s/%s %s: the dialect's metadata topic has no PUB/Metadata segments to derive the integration's value topic from", objectType, pointType, tag)
			return nil
		}
		p.integTopic = t
	}
	a.points = append(a.points, p)
	return p
}

// build derives the point list from the compiled model. The structure is fixed after compile,
// so it is read once; values are read later through the engine's accessors.
func (a *Adapter) build() {
	a.e.Read(func(m *model.Model) {
		names := map[string]int{}
		for _, r := range m.Racks {
			names[r.Name]++
		}
		rackID := func(r *model.Rack) string {
			if names[r.Name] == 1 {
				return r.Name
			}
			return r.Path
		}
		rackMeta := func(r *model.Rack, pt string) map[string]any {
			return map[string]any{"objectType": "Rack", "pointType": pt, "rackLocationName": r.Name, "rackLocationId": rackID(r)}
		}
		cduOf := func(lp *model.Loop) *model.Device {
			for _, d := range m.Devices {
				if d.Name == lp.Name && strings.HasSuffix(d.Path, "/facility/"+lp.Name) {
					return d
				}
			}
			return nil
		}

		// (1) devices whose profile maps sensors to BMS points
		for _, d := range m.Devices {
			bm := d.Profile.Spec.BMS
			if bm == nil {
				continue
			}
			for _, bp := range bm.Points {
				pt, area, def, ok := lookup(bm.ObjectType, bp.PointType)
				if !ok {
					a.skip("%s point %s: %s/%s is not a BMS-published point type of the AsyncAPI", d.Path, bp.Name, bm.ObjectType, bp.PointType)
					continue
				}
				if def.integration || !a.allowed(bm.ObjectType, pt) {
					a.skip("%s point %s: %s/%s is not published by the BMS in dialect %s", d.Path, bp.Name, bm.ObjectType, pt, a.dialect.Metadata.Name)
					continue
				}
				var spec *schema.SensorSpec
				for i := range d.Profile.Spec.Sensors {
					if d.Profile.Spec.Sensors[i].Name == bp.Sensor {
						spec = &d.Profile.Spec.Sensors[i]
					}
				}
				if spec == nil {
					a.skip("%s point %s: the profile has no sensor %q", d.Path, bp.Name, bp.Sensor)
					continue
				}
				var meta map[string]any
				var rack *model.Rack
				switch {
				case bm.ObjectType == "Rack":
					if rack = d.RackOf(); rack == nil {
						a.skip("%s point %s: a Rack point on a device in no rack", d.Path, bp.Name)
						continue
					}
					meta = rackMeta(rack, pt)
				case equipmentObjectTypes[bm.ObjectType]:
					meta = map[string]any{"objectType": bm.ObjectType, "pointType": pt, "objectName": d.Name, "objectId": d.Path}
					if area != nil {
						meta["processArea"] = area
					}
					if lp := m.Loops[d.Name]; lp != nil && strings.HasSuffix(d.Path, "/facility/"+lp.Name) {
						var serves []string
						for _, r := range lp.Racks {
							serves = append(serves, rackID(r))
						}
						if len(serves) > 0 {
							meta["servesId"] = serves
						}
					}
				default:
					a.skip("%s point %s: objectType %s is not published from a device profile", d.Path, bp.Name, bm.ObjectType)
					continue
				}
				p := a.newPoint(bm.ObjectType, pt, tagPath(d.Path, bp.Name), def, meta)
				if p == nil {
					continue
				}
				dev, sensor, scale := d, spec.Name, unitScale(spec.Unit, def.engUnit)
				p.source, p.rack = d.Path, rack
				p.read = func() (float64, bool) {
					v, _, ok := a.e.Sensor(dev, sensor)
					return round(v*scale, 3), ok
				}
				if a.mirrors[MirrorNICoLegacy] && bm.ObjectType == "Rack" {
					p.legacy = legacyPointType[pt]
				}
			}
		}

		// (2) per-rack points the facility model derives
		for _, r := range m.Racks {
			r := r
			add := func(pt, sub string, read func() (float64, bool)) *point {
				def, ok := catalogue["Rack/"+pt]
				if !ok || !a.allowed("Rack", pt) {
					return nil
				}
				p := a.newPoint("Rack", pt, tagPath(r.Path, sub), def, rackMeta(r, pt))
				if p == nil {
					return nil
				}
				p.source, p.rack, p.read = r.Path, r, read
				return p
			}
			add("RackPower", "power", func() (float64, bool) { return round(a.e.RackPower(r)/1000, 3), true })
			status := map[string]*point{}
			if lp := r.Loop; lp != nil {
				if cdu := cduOf(lp); cdu != nil {
					supply, ret, flow := sensorBySource(cdu, "loop-supply"), sensorBySource(cdu, "loop-return"), sensorBySource(cdu, "loop-flow")
					if supply != "" {
						add("RackLiquidSupplyTemperature", "liquid/supplyTemperature", a.sensorRead(cdu, supply))
					}
					if ret != "" {
						add("RackLiquidReturnTemperature", "liquid/returnTemperature", a.sensorRead(cdu, ret))
					}
					if flow != "" {
						add("RackLiquidFlow", "liquid/flow", a.rackFlow(r, lp, cdu, flow))
					}
				} else {
					a.skip("rack %s liquid supply, return and flow: cooling loop %s has no CDU device (no profile)", r.Name, lp.Name)
				}
				status["liquid"] = add("RackLiquidIsolationStatus", "isolation/liquid/status", a.isolated(r, "liquid"))
			}
			if len(r.Inputs) > 0 {
				status["electrical"] = add("RackElectricalIsolationStatus", "isolation/electrical/status", a.isolated(r, "electrical"))
			}
			// integration-owned points: metadata here, values from the integration
			for _, q := range a.dialect.Spec.Requests {
				st := status[q.Effect]
				if st == nil || st.pointType != q.Status {
					continue
				}
				p := add(q.PointType, "isolation/"+q.Effect+"/request", nil)
				if p == nil {
					continue
				}
				p.effect, p.status = q.Effect, st
				p.delay = time.Duration(float64(q.DelayS) * float64(time.Second) * a.e.Scale())
				a.requests[p.integTopic] = p
				if a.mirrors[MirrorNICoAPI] {
					a.nicoAPIMirror(p, r.Name, rackID(r))
				}
			}
			if rackHasTrayDetectors(r) {
				if p := add("RackLeakDetectTray", "leak/tray", nil); p != nil {
					a.requests[p.integTopic] = p
				}
			}
		}
	})

	// the BMS heartbeat
	if hb := a.dialect.Spec.Heartbeat; hb != "" {
		vt := expand(hb, map[string]string{"site": tagSafe(a.site)})
		mt := strings.Replace(vt, "/Value/", "/Metadata/", 1)
		if mt != vt {
			a.heartbeat = &point{objectType: "System", pointType: "HeartbeatTimestampBms", valueTopic: vt, metaTopic: mt,
				meta: map[string]any{"objectType": "System", "pointType": "HeartbeatTimestampBms", "objectName": "dc-twin BMS (" + a.site + ")", "objectId": "BMS"},
				read: func() (float64, bool) { return float64(a.now().UnixMilli()), true }}
			a.points = append(a.points, a.heartbeat)
		} else {
			a.skip("heartbeat: the dialect's heartbeat topic %q has no /Value/ level to derive its metadata topic from", hb)
		}
	}

	// the integration's heartbeat: the AsyncAPI has the BMS publish the metadata of every
	// integration-published System point, one HeartbeatTimestampIntegration per integration with
	// objectName and objectId naming it, and the integration publish the value (epoch ms every
	// 10 s) on the topic derived from it. This shape is also all nico-api's parser needs
	// (pointType, objectType, integration; crates/bms-dsx-exchange/src/lib.rs:217-276), so it is
	// one message for both, not a mirror.
	meta := map[string]any{"objectType": "System", "pointType": "HeartbeatTimestampIntegration",
		"objectName": a.cfg.Integration, "objectId": a.cfg.Integration}
	a.integHB = a.newPoint("System", "HeartbeatTimestampIntegration", tagPath(a.site, "integration", a.cfg.Integration),
		catalogue["System/HeartbeatTimestampIntegration"], meta)
}

// nicoAPIMirror gives an isolation request its nico-api mirror (D40). nico-api learns where to
// publish a rack's request only from metadata that parses in its shape — integration, rackName
// and rackId — and the AsyncAPI forbids those names on the request's own metadata topic
// (rackLocationName/rackLocationId, unevaluatedProperties: false), so the mirror's metadata gets a
// topic of its own: the request's tagPath with one more level. nico-api derives the value topic
// from it (BMS/v1/<integration>/Value/Rack/<pointType>/<tagPath>/nico-api) and the twin takes a
// request there as it takes one on the AsyncAPI topic. rackId is the rack's AsyncAPI
// rackLocationId, which is the rack name the stack's expected machines carry unless two racks
// share a name; nico-api keys a request on its own RackId, so the two must agree.
func (a *Adapter) nicoAPIMirror(p *point, rackName, rackID string) {
	mt := nicoAPIMetadataPrefix + p.objectType + "/" + p.pointType + "/" + p.tagPath + "/" + MirrorNICoAPI
	vt, _ := integrationTopic(mt, a.cfg.Integration) // mt has the PUB/Metadata segments by construction
	p.napi = &apiMirror{metaTopic: mt, valueTopic: vt, meta: map[string]any{
		"pointType": p.pointType, "objectType": p.objectType, "rackName": rackName, "rackId": rackID, "integration": a.cfg.Integration}}
	a.apiReqs[vt] = p
}

func sensorBySource(d *model.Device, source string) string {
	for _, s := range d.Profile.Spec.Sensors {
		if s.Source == source {
			return s.Name
		}
	}
	return ""
}

func rackHasTrayDetectors(r *model.Rack) bool {
	for _, d := range r.Devices {
		if sensorBySource(d, "tray-leak-detector") != "" {
			return true
		}
	}
	return false
}

func (a *Adapter) sensorRead(d *model.Device, sensor string) func() (float64, bool) {
	return func() (float64, bool) {
		v, _, ok := a.e.Sensor(d, sensor)
		return round(v, 2), ok
	}
}

// rackFlow is the loop's flow shared equally by its racks that are not liquid-isolated.
func (a *Adapter) rackFlow(r *model.Rack, lp *model.Loop, cdu *model.Device, sensor string) func() (float64, bool) {
	return func() (float64, bool) {
		total, _, ok := a.e.Sensor(cdu, sensor)
		if !ok {
			return 0, false
		}
		n, self := 0, false
		a.e.Read(func(*model.Model) {
			for _, x := range lp.Racks {
				if !x.Isolated["liquid"] {
					n++
				}
			}
			self = r.Isolated["liquid"]
		})
		if self || n == 0 {
			return 0, true
		}
		return round(total/float64(n), 2), true
	}
}

func (a *Adapter) isolated(r *model.Rack, kind string) func() (float64, bool) {
	return func() (float64, bool) {
		on := false
		a.e.Read(func(*model.Model) { on = r.Isolated[kind] })
		if on {
			return 1, true
		}
		return 0, true
	}
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// now is the twin's clock: wall time, or the virtual clock on a virtual estate.
func (a *Adapter) now() time.Time {
	if a.e.Virtual() {
		return a.e.Now()
	}
	return time.Now().UTC()
}

// ---------------------------------------------------------------- publishing

type valueMsg struct {
	Value     *float64 `json:"value"`
	Timestamp int64    `json:"timestamp"`
	Quality   int      `json:"quality"`
}

var errTimeout = errors.New("publish not acknowledged within 5s")

func (a *Adapter) publish(topic string, payload []byte, retained bool, kind string) {
	if !a.client.IsConnectionOpen() {
		return
	}
	at := a.now()
	tok := a.client.Publish(topic, 0, retained, payload)
	err := errTimeout
	if tok.WaitTimeout(5 * time.Second) {
		err = tok.Error()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.pubErrs++
		a.lastErr = err.Error()
		return
	}
	a.pubs = append(a.pubs, Publication{Topic: topic, Payload: payload, Retained: retained, Time: at, Kind: kind})
	if len(a.pubs) > 500000 {
		a.pubs = append([]Publication{}, a.pubs[len(a.pubs)/2:]...)
	}
}

func (a *Adapter) publishMetadata() {
	for _, p := range a.points {
		b, _ := json.Marshal(p.meta)
		a.publish(p.metaTopic, b, true, "metadata")
		if p.legacy != "" {
			lm := map[string]any{"pointType": p.legacy, "objectType": "Rack", "rackName": p.meta["rackLocationName"], "rackID": p.meta["rackLocationId"]}
			b, _ := json.Marshal(lm)
			a.publish(a.legacyTopic(p, "Metadata"), b, true, "mirror")
		}
		if p.napi != nil { // metadata only: nico-api publishes the value, the BMS answers on the AsyncAPI status
			b, _ := json.Marshal(p.napi.meta)
			a.publish(p.napi.metaTopic, b, true, "mirror")
		}
	}
}

// publishValues publishes every BMS-owned point whose value changed (all of them when force).
func (a *Adapter) publishValues(force bool) {
	for _, p := range a.points {
		if p.def.integration || p == a.heartbeat {
			continue
		}
		a.publishValue(p, force)
	}
}

func (a *Adapter) publishValue(p *point, force bool) {
	if p == nil || p.read == nil {
		return
	}
	v, ok := p.read()
	if !ok {
		return
	}
	prev, seen := a.last[p.valueTopic]
	if !force && seen && !changed(prev, v, p.def.deadband) {
		return
	}
	a.last[p.valueTopic] = v
	at := a.now()
	b, _ := json.Marshal(valueMsg{Value: &v, Timestamp: at.UnixMilli(), Quality: 1})
	a.publish(p.valueTopic, b, false, "value")
	if p.legacy != "" {
		b, _ := json.Marshal(map[string]any{"value": int(v), "timestamp": at.Unix()})
		a.publish(a.legacyTopic(p, "Value"), b, false, "mirror")
	}
}

func changed(prev, v, deadband float64) bool {
	if deadband <= 0 {
		return prev != v
	}
	return math.Abs(prev-v) >= deadband
}

func (a *Adapter) publishHeartbeat() {
	if a.heartbeat == nil {
		return
	}
	v, _ := a.heartbeat.read()
	b, _ := json.Marshal(valueMsg{Value: &v, Timestamp: int64(v), Quality: 1})
	a.publish(a.heartbeat.valueTopic, b, false, "heartbeat")
}

// legacyTopic is the mirror topic NICo's consumer reads: its point path, then the suffix.
func (a *Adapter) legacyTopic(p *point, suffix string) string {
	prefix := "BMS/v1"
	if i := strings.Index(a.dialect.Spec.Value, "/PUB/"); i > 0 {
		prefix = a.dialect.Spec.Value[:i]
	}
	return prefix + "/PUB/" + p.objectType + "/" + p.legacy + "/" + p.tagPath + "/" + suffix
}

// ---------------------------------------------------------------- the loop

// loop owns publishing: after each connection it subscribes and publishes everything, then
// publishes values as committed events change them, on the sampling and republication
// cadences, and runs isolation actions as they fall due.
func (a *Adapter) loop() {
	defer a.wg.Done()
	subID, events := a.e.Subscribe(4096)
	defer func() { a.e.Unsubscribe(subID) }()
	meta := time.NewTicker(a.cfg.MetadataEvery)
	vals := time.NewTicker(a.cfg.ValueEvery)
	hb := time.NewTicker(a.cfg.HeartbeatEvery)
	sample := time.NewTicker(a.cfg.SampleEvery)
	tick := time.NewTicker(20 * time.Millisecond)
	defer func() { meta.Stop(); vals.Stop(); hb.Stop(); sample.Stop(); tick.Stop() }()
	dirty := false
	for {
		select {
		case <-a.stop:
			return
		case <-a.connected:
			a.onConnect()
		case ev, ok := <-events:
			if !ok { // dropped by the engine for lagging: resubscribe and re-read everything
				subID, events = a.e.Subscribe(4096)
				dirty = true
				continue
			}
			if ev.Kind != "request" {
				dirty = true
			}
		case <-tick.C:
			a.runDue()
			if dirty {
				dirty = false
				a.publishValues(false)
			}
		case <-sample.C:
			a.publishValues(false)
		case <-vals.C:
			a.publishValues(true)
		case <-meta.C:
			a.publishMetadata()
		case <-hb.C:
			a.publishHeartbeat()
		}
	}
}

func (a *Adapter) onConnect() {
	a.mu.Lock()
	a.conns++
	a.mu.Unlock()
	filters := map[string]byte{}
	for t := range a.requests {
		filters[t] = 0
	}
	for t := range a.apiReqs {
		filters[t] = 0
	}
	if a.integHB != nil {
		filters[a.integHB.integTopic] = 0
	}
	if len(filters) > 0 {
		tok := a.client.SubscribeMultiple(filters, a.onIntegration)
		if !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
			a.log.Warn("bms: subscribing to the integration's value topics failed", "err", tok.Error())
		}
	}
	a.publishMetadata()
	a.publishValues(true)
	a.publishHeartbeat()
	a.readyOnce.Do(func() { close(a.ready) })
}

// onIntegration handles a value an integration published (paho's router goroutine): it is
// logged; a heartbeat is counted; a request is committed when it changes and schedules the
// isolation. A topic of the nico-api mirror carries the request in nico-api's shape and acts on
// the same rack and effect as the AsyncAPI topic it mirrors.
func (a *Adapter) onIntegration(_ paho.Client, msg paho.Message) {
	topic, payload := msg.Topic(), append([]byte{}, msg.Payload()...)
	a.mu.Lock()
	a.received = append(a.received, Publication{Topic: topic, Payload: payload, Time: a.now(), Kind: "integration"})
	if len(a.received) > 500000 {
		a.received = append([]Publication{}, a.received[len(a.received)/2:]...)
	}
	a.mu.Unlock()
	if a.integHB != nil && topic == a.integHB.integTopic {
		a.onHeartbeat(payload)
		return
	}
	p, shape := a.requests[topic], "asyncapi"
	if p == nil {
		p, shape = a.apiReqs[topic], MirrorNICoAPI
	}
	if p == nil {
		return
	}
	var v float64
	var quality any
	ok := false
	if shape == MirrorNICoAPI {
		v, quality, ok = nicoAPIValue(payload)
	} else {
		var m struct {
			Value     *float64 `json:"value"`
			Timestamp *int64   `json:"timestamp"`
			Quality   *int     `json:"quality"`
		}
		if err := json.Unmarshal(payload, &m); err == nil && m.Value != nil && m.Timestamp != nil && m.Quality != nil {
			v, quality, ok = *m.Value, *m.Quality, true
		}
	}
	if !ok {
		a.mu.Lock()
		if shape == MirrorNICoAPI {
			a.apiRej++
		} else {
			a.rejected++
		}
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	prev, seen := a.lastReq[topic]
	a.lastReq[topic] = v
	a.mu.Unlock()
	if !seen || prev != v {
		what, data := fmt.Sprintf("BMS %s = %v on rack %s", p.pointType, v, p.rack.Name), map[string]any{"pointType": p.pointType, "value": v, "quality": quality}
		if shape == MirrorNICoAPI {
			what += " (nico-api mirror)"
			data["mirror"] = MirrorNICoAPI
		}
		a.e.Commit(schema.Event{Kind: "request", Source: schema.EventSource{Class: "stack", Principal: a.cfg.Integration},
			Path: p.rack.Path, Message: what,
			Request: &schema.EventRequest{Protocol: "mqtt", Method: "PUBLISH", Target: topic, Canonical: "other"},
			Data:    data})
	}
	if p.effect == "" { // RackLeakDetectTray: informational, no BMS action
		return
	}
	if v != 0 && v != 1 {
		if v != -1 { // -1 is the liquid request's Unknown: no action, not an error
			a.mu.Lock()
			a.rejected++
			a.mu.Unlock()
		}
		return
	}
	on := v == 1
	cur := false
	a.e.Read(func(*model.Model) { cur = p.rack.Isolated[p.effect] })
	key := p.rack.Path + "#" + p.effect
	a.mu.Lock()
	defer a.mu.Unlock()
	if act, ok := a.pending[key]; ok && act.on == on {
		return // already scheduled; a republished request does not restart the delay
	}
	if cur == on {
		delete(a.pending, key)
		return
	}
	a.pending[key] = action{due: a.e.Now().Add(p.delay), rack: p.rack, kind: p.effect, on: on, status: p.status}
}

// nicoAPIValue reads a request in nico-api's shape, ValueMessage as crates/bms-dsx-exchange
// serializes it (src/lib.rs:187-215): value a BinaryState, the integer 0 or 1 (lib.rs:51-73);
// timestamp epoch milliseconds (publisher.rs:75-81); quality GOOD_QUALITY, the string "1"
// (lib.rs:26), where the AsyncAPI's quality is an integer. Keys match exactly (encoding/json's
// struct matching would also take "Value" or "QUALITY"), and a number in quotes is not a number.
func nicoAPIValue(payload []byte) (v float64, quality any, ok bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return 0, nil, false
	}
	val, okV := jsonInt(m["value"])
	_, okT := jsonInt(m["timestamp"])
	q := bytes.TrimSpace(m["quality"])
	var qs string
	if !okV || !okT || len(q) == 0 || q[0] != '"' || json.Unmarshal(q, &qs) != nil || (val != 0 && val != 1) {
		return 0, nil, false
	}
	return float64(val), qs, true
}

// onHeartbeat logs the integration's HeartbeatTimestampIntegration value: {value, timestamp,
// quality}, value and timestamp epoch milliseconds, quality an integer (the AsyncAPI) or the
// string "1" (nico-api, crates/bms-dsx-exchange/src/publisher.rs:241-253, every 5 s). It is
// not committed to the event log: it would be one event every few seconds and says nothing about
// the model.
func (a *Adapter) onHeartbeat(payload []byte) {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(payload, &m)
	val, okV := jsonInt(m["value"])
	_, okT := jsonInt(m["timestamp"])
	q := bytes.TrimSpace(m["quality"])
	_, qInt := jsonInt(q)
	qStr := len(q) > 0 && q[0] == '"'
	a.mu.Lock()
	defer a.mu.Unlock()
	if !okV || !okT || !(qInt || qStr) {
		a.hb.bad++
		return
	}
	a.hb.n++
	a.hb.last = val
	a.hb.stringQuality = a.hb.stringQuality || qStr
}

// jsonInt reads a JSON integer literal (not a string holding one, not a fraction).
func jsonInt(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !(raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9') {
		return 0, false
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// runDue applies isolation actions whose delay has passed on the engine clock.
func (a *Adapter) runDue() {
	now := a.e.Now()
	var due []action
	a.mu.Lock()
	for k, act := range a.pending {
		if !now.Before(act.due) {
			due = append(due, act)
			delete(a.pending, k)
		}
	}
	a.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].due.Before(due[j].due) })
	for _, act := range due {
		cur := false
		a.e.Read(func(*model.Model) { cur = act.rack.Isolated[act.kind] })
		if cur != act.on {
			if err := a.e.Isolate(act.rack.Path, act.kind, act.on, schema.EventSource{Class: "twin", Principal: "bms"}); err != nil {
				a.log.Warn("bms: isolation failed", "rack", act.rack.Path, "kind", act.kind, "err", err)
				continue
			}
		}
		a.publishValue(act.status, false)
		if act.kind == "liquid" {
			for _, p := range a.points {
				if p.pointType == "RackLiquidFlow" {
					a.publishValue(p, false)
				}
			}
		}
	}
}

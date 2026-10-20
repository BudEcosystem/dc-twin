package bms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

func init() {
	// the spacing is deliberate: the list is trimmed
	twin.Register("bms-nico", func() adapter.Adapter {
		return NewWithConfig(Config{ConsumerMirror: "nico-legacy, nico-api"})
	})
}

// ---------------------------------------------------------------- nico-api's parser, transcribed
//
// nico-api's own DSX Exchange integration is NICo's crates/bms-dsx-exchange, driven by
// crates/rack/src/bms_client.rs (dsx-ai-factory/infra-controller 02b92f18, Apache-2.0; line
// numbers are that commit's). nico-api subscribes to BMS/v1/PUB/Metadata/# (bms_client.rs:39,
// 149), hands every message to parse_supported_metadata (bms_client.rs:220), and learns from a
// supported one the topic it then publishes that point's value on. The functions below transcribe
// that parse with serde's strictness, so a test reads the twin's publications as nico-api does.

// nicoMetadata is SupportedMetadata (lib.rs:75-155): a rack isolation request or the heartbeat.
type nicoMetadata struct {
	heartbeat                                 bool
	pointType                                 string // the payload's (lib.rs:98-104, 142-144)
	rackName, rackID, integration, valueTopic string
}

// nicoRawMetadata is serde_json::from_slice::<RawMetadata> (lib.rs:217-228, 234): a JSON object
// whose camelCase keys match exactly; pointType and objectType are String (required: absent or
// null fails), rackName, rackId and integration Option<String> with #[serde(default)] (absent or
// null is None); a non-string value fails, a duplicate of one of these keys fails, other keys
// are ignored, and nothing may follow the object.
func nicoRawMetadata(payload []byte) (map[string]*string, error) {
	d := json.NewDecoder(bytes.NewReader(payload))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("invalid type: expected struct RawMetadata")
	}
	fields, seen := map[string]*string{}, map[string]bool{}
	for d.More() {
		kt, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, _ := kt.(string)
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			return nil, err
		}
		switch key {
		case "pointType", "objectType", "rackName", "rackId", "integration":
			if seen[key] {
				return nil, fmt.Errorf("duplicate field `%s`", key)
			}
			seen[key] = true
			if string(raw) == "null" {
				continue
			}
			var s string
			if raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
				return nil, fmt.Errorf("invalid type for `%s`: expected a string", key)
			}
			fields[key] = &s
		}
	}
	if _, err := d.Token(); err != nil { // the closing brace
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing characters")
	}
	for _, k := range []string{"pointType", "objectType"} {
		if fields[k] == nil {
			return nil, fmt.Errorf("missing field `%s`", k)
		}
	}
	return fields, nil
}

// nicoRequired is required_field (lib.rs:309-327): present and not blank after trim.
func nicoRequired(pointType, field string, v *string) (string, error) {
	if v == nil || strings.TrimSpace(*v) == "" {
		return "", fmt.Errorf("metadata for point type `%s` is missing `%s` or empty", pointType, field)
	}
	return *v, nil
}

// nicoValueTopic is value_topic (lib.rs:278-307).
func nicoValueTopic(metadataTopic, integration string) (string, error) {
	bad := fmt.Errorf("metadata topic `%s` is not a valid BMS metadata topic", metadataTopic)
	path, ok := strings.CutPrefix(metadataTopic, "BMS/v1/PUB/Metadata/")
	if !ok {
		return "", bad
	}
	segs := strings.Split(path, "/")
	if len(segs) < 2 {
		return "", bad
	}
	objectType, pointType, tag := segs[0], segs[1], strings.Join(segs[2:], "/")
	if objectType == "" || pointType == "" || tag == "" {
		return "", bad
	}
	return fmt.Sprintf("BMS/v1/%s/Value/%s/%s/%s", integration, objectType, pointType, tag), nil
}

// nicoParseMetadata is parse_supported_metadata (lib.rs:230-237) and SupportedMetadata::from_raw
// (lib.rs:239-276). nil, nil is Ok(None): a point type nico-api does not handle. Note the order:
// integration is required of every message, and the topic checked, before the point type is.
func nicoParseMetadata(topic string, payload []byte) (*nicoMetadata, error) {
	f, err := nicoRawMetadata(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %w", err)
	}
	pt, ot := *f["pointType"], *f["objectType"]
	integration, err := nicoRequired(pt, "integration", f["integration"])
	if err != nil {
		return nil, err
	}
	vt, err := nicoValueTopic(topic, integration)
	if err != nil {
		return nil, err
	}
	switch {
	case ot == "Rack" && (pt == "RackLiquidIsolationRequest" || pt == "RackElectricalIsolationRequest"):
		name, err := nicoRequired(pt, "rackName", f["rackName"])
		if err != nil {
			return nil, err
		}
		id, err := nicoRequired(pt, "rackId", f["rackId"])
		if err != nil {
			return nil, err
		}
		return &nicoMetadata{pointType: pt, rackName: name, rackID: id, integration: integration, valueTopic: vt}, nil
	case ot == "System" && pt == "HeartbeatTimestampIntegration":
		return &nicoMetadata{heartbeat: true, pointType: pt, integration: integration, valueTopic: vt}, nil
	}
	return nil, nil
}

// nicoValueMessage is the payload nico-api publishes: ValueMessage's Serialize (lib.rs:204-215)
// through serde_json::to_vec (publisher.rs:34-36) — value a BinaryState (0 or 1, lib.rs:63-73)
// or the heartbeat's epoch milliseconds (source_mapping.rs:60-70), timestamp
// now.timestamp_millis() (publisher.rs:75-81), quality GOOD_QUALITY, the string "1" (lib.rs:26).
func nicoValueMessage(value, timestampMillis int64) []byte {
	return []byte(fmt.Sprintf(`{"value":%d,"timestamp":%d,"quality":"1"}`, value, timestampMillis))
}

// The transcription must decide as nico-api decides, or the test below proves nothing: NICo's own
// cases (lib.rs:333-431, publisher.rs:285-313), the shapes the twin published before the mirror
// (the failures the 2026-10-18 run logged), and serde's exactness.
func TestNICoAPIParserOracle(t *testing.T) {
	type c struct {
		topic, payload string
		want           string // "rack <pointType> <integration> <valueTopic>", "heartbeat …", "none", or "error <substring>"
	}
	cases := []c{
		// lib.rs:362-404
		{"BMS/v1/PUB/Metadata/Rack/RackLiquidIsolationRequest/site/rack-01",
			`{"pointType":"RackLiquidIsolationRequest","objectType":"Rack","rackName":"Rack-01","rackId":"rack-01","integration":"CM"}`,
			"rack RackLiquidIsolationRequest CM BMS/v1/CM/Value/Rack/RackLiquidIsolationRequest/site/rack-01"},
		{"BMS/v1/PUB/Metadata/System/HeartbeatTimestampIntegration/site",
			`{"pointType":"HeartbeatTimestampIntegration","objectType":"System","integration":"CM"}`,
			"heartbeat HeartbeatTimestampIntegration CM BMS/v1/CM/Value/System/HeartbeatTimestampIntegration/site"},
		{"BMS/v1/PUB/Metadata/Rack/RackElectricalIsolationRequest/site/rack-01",
			`{"pointType":"RackLiquidIsolationRequest","objectType":"Rack","rackName":"Rack-01","rackId":"rack-01","integration":"CM"}`,
			"rack RackLiquidIsolationRequest CM BMS/v1/CM/Value/Rack/RackElectricalIsolationRequest/site/rack-01"},
		// lib.rs:406-429
		{"BMS/v1/PUB/Metadata/Rack/RackLiquidIsolationRequest/site/rack-01",
			`{"pointType":"RackLiquidIsolationRequest","objectType":"Rack","rackName":"Rack-01","rackId":"rack-01","integration":""}`, "error missing `integration`"},
		{"BMS/v1/CM/Value/Rack/RackLiquidIsolationRequest/site/rack-01",
			`{"pointType":"RackLiquidIsolationRequest","objectType":"Rack","rackName":"Rack-01","rackId":"rack-01","integration":"CM"}`, "error not a valid BMS metadata topic"},
		// bms_client.rs:364-374: a point type nico-api does not route
		{"BMS/v1/PUB/Metadata/Rack/RackLeakDetect/site/rack-01",
			`{"pointType":"RackLeakDetect","objectType":"Rack","rackName":"Rack-01","rackId":"rack-01","integration":"CM"}`, "none"},
		// what the twin published before the mirror (nico-api's log, compat/nico/README.md)
		{"BMS/v1/PUB/Metadata/Rack/RackElectricalIsolationRequest/site-1/hall-a/a03/isolation/electrical/request",
			`{"integration":"NICO","objectType":"Rack","pointType":"RackElectricalIsolationRequest","rackLocationId":"a03","rackLocationName":"a03","stateText":[{"value":0,"text":"NoIsolationRequested"},{"value":1,"text":"IsolationRequested"}]}`,
			"error missing `rackName`"},
		{"BMS/v1/PUB/Metadata/Rack/RackPower/site-1/hall-a/a03/power",
			`{"engUnit":"kW","objectType":"Rack","pointType":"RackPower","rackLocationId":"a03","rackLocationName":"a03"}`, "error missing `integration`"},
		{"BMS/v1/PUB/Metadata/System/HeartbeatTimestampBms/site-1/bms",
			`{"objectId":"BMS","objectName":"dc-twin BMS (site-1)","objectType":"System","pointType":"HeartbeatTimestampBms"}`, "error missing `integration`"},
		// the AsyncAPI's own HeartbeatTimestampIntegration metadata is enough for nico-api
		{"BMS/v1/PUB/Metadata/System/HeartbeatTimestampIntegration/site-1/integration/NICO",
			`{"integration":"NICO","objectId":"NICO","objectName":"NICO","objectType":"System","pointType":"HeartbeatTimestampIntegration"}`,
			"heartbeat HeartbeatTimestampIntegration NICO BMS/v1/NICO/Value/System/HeartbeatTimestampIntegration/site-1/integration/NICO"},
		// serde's exactness: the consumer's rackID is not rackId; case, types, nulls, duplicates
		{"BMS/v1/PUB/Metadata/Rack/RackElectricalIsolationRequest/s/a03",
			`{"pointType":"RackElectricalIsolationRequest","objectType":"Rack","rackName":"a03","rackID":"a03","integration":"NICO"}`, "error missing `rackId`"},
		{"BMS/v1/PUB/Metadata/Rack/RackElectricalIsolationRequest/s/a03",
			`{"PointType":"RackElectricalIsolationRequest","objectType":"Rack","rackName":"a03","rackId":"a03","integration":"NICO"}`, "error missing field `pointType`"},
		{"BMS/v1/PUB/Metadata/Rack/RackElectricalIsolationRequest/s/a03",
			`{"pointType":"RackElectricalIsolationRequest","objectType":"Rack","rackName":"a03","rackId":3,"integration":"NICO"}`, "error invalid type"},
		{"BMS/v1/PUB/Metadata/Rack/RackElectricalIsolationRequest/s/a03",
			`{"pointType":"RackElectricalIsolationRequest","objectType":"Rack","rackName":"a03","rackId":null,"integration":"NICO"}`, "error missing `rackId`"},
		{"BMS/v1/PUB/Metadata/Rack/RackElectricalIsolationRequest/s/a03",
			`{"pointType":"RackElectricalIsolationRequest","objectType":"Rack","rackName":" ","rackId":"a03","integration":"NICO"}`, "error missing `rackName`"},
		{"BMS/v1/PUB/Metadata/Rack/RackElectricalIsolationRequest/s/a03",
			`{"pointType":"RackElectricalIsolationRequest","objectType":"Rack","rackName":"a03","rackId":"a03","rackId":"a04","integration":"NICO"}`, "error duplicate field"},
		{"BMS/v1/PUB/Metadata/Rack/RackElectricalIsolationRequest",
			`{"pointType":"RackElectricalIsolationRequest","objectType":"Rack","rackName":"a03","rackId":"a03","integration":"NICO"}`, "error not a valid BMS metadata topic"},
	}
	for _, x := range cases {
		m, err := nicoParseMetadata(x.topic, []byte(x.payload))
		got := "none"
		switch {
		case err != nil:
			got = "error " + err.Error()
		case m != nil && m.heartbeat:
			got = fmt.Sprintf("heartbeat %s %s %s", m.pointType, m.integration, m.valueTopic)
		case m != nil:
			got = fmt.Sprintf("rack %s %s %s", m.pointType, m.integration, m.valueTopic)
		}
		if strings.HasPrefix(x.want, "error ") {
			if !strings.HasPrefix(got, "error ") || !strings.Contains(got, strings.TrimPrefix(x.want, "error ")) {
				t.Errorf("%s %s: got %q, want an error containing %q", x.topic, x.payload, got, strings.TrimPrefix(x.want, "error "))
			}
			continue
		}
		if got != x.want {
			t.Errorf("%s %s:\n got  %q\n want %q", x.topic, x.payload, got, x.want)
		}
	}
	if got := string(nicoValueMessage(1, 1_712_345_678_901)); got != `{"value":1,"timestamp":1712345678901,"quality":"1"}` { // lib.rs:433-447
		t.Errorf("value message %s", got)
	}
}

// The nico-api mirror, end to end on the twin-hosted broker: everything the BMS publishes under
// BMS/v1/PUB/Metadata/# goes through nico-api's parse; exactly the mirror's request metadata and
// the integration heartbeat metadata are supported, one per rack and request, each naming a value
// topic the BMS takes; then "nico-api" publishes, in its own shape, what set_rack_leak_state
// publishes for a leaking rack (bms_client.rs:175-184) and its heartbeat, and the BMS isolates
// the rack, answers on the AsyncAPI status points, and releases on 0.
func TestNICoAPIMirror(t *testing.T) {
	est, reg := loadLab(t)
	tw, a := up(t, est, reg, "bms-nico")
	if !a.mirrors[MirrorNICoLegacy] || !a.mirrors[MirrorNICoAPI] {
		t.Fatalf("mirrors %v", a.mirrors)
	}
	nico, client := subscriber(t, a.Broker(), nicoAPIMetadataPrefix+"#") // bms_client.rs:39, 149

	// what nico-api learns: SourceId (rack id and request kind, or the one heartbeat) → value topic
	routes := map[string]string{}
	var hbTopic string
	parse := func() (supported int, errs map[string]int) {
		errs = map[string]int{}
		for _, p := range nico.all() {
			m, err := nicoParseMetadata(p.Topic, p.Payload)
			switch {
			case err != nil:
				why := err.Error()
				if _, field, ok := strings.Cut(why, " is missing "); ok {
					why = "missing " + strings.TrimSuffix(field, " or empty")
				}
				errs[why]++
			case m != nil && m.heartbeat:
				hbTopic = m.valueTopic
				supported++
			case m != nil:
				key := m.pointType + " " + m.rackID
				if prev, dup := routes[key]; dup && prev != m.valueTopic {
					t.Errorf("two supported metadata messages for %s: %s and %s", key, prev, m.valueTopic)
				}
				routes[key] = m.valueTopic
				if !strings.HasSuffix(p.Topic, "/"+MirrorNICoAPI) {
					t.Errorf("nico-api parsed a message that is not the mirror's: %s %s", p.Topic, p.Payload)
				}
				supported++
			}
		}
		return supported, errs
	}
	waitFor(t, 5*time.Second, "every mirrored request and the heartbeat metadata at nico-api", func() bool {
		routes, hbTopic = map[string]string{}, ""
		n, _ := parse()
		return n >= len(a.apiReqs)+1 && hbTopic != ""
	})
	routes, hbTopic = map[string]string{}, ""
	n, errs := parse()
	t.Logf("nico-api's parse of %d metadata messages: %d supported, failures %v", len(nico.all()), n, errs)
	if len(routes) != len(a.apiReqs) || len(a.apiReqs) == 0 {
		t.Fatalf("nico-api routes %d isolation requests; the BMS mirrors %d", len(routes), len(a.apiReqs))
	}
	for key, vt := range routes {
		p := a.apiReqs[vt]
		if p == nil {
			t.Errorf("nico-api would publish %s on %s, which the BMS does not take", key, vt)
			continue
		}
		if want := p.pointType + " " + p.meta["rackLocationId"].(string); key != want {
			t.Errorf("value topic %s: nico-api reads %s, the BMS acts on %s", vt, key, want)
		}
	}
	if a.integHB == nil || hbTopic != a.integHB.integTopic {
		t.Fatalf("nico-api would heartbeat on %q; the BMS takes %v", hbTopic, a.integHB)
	}
	// the AsyncAPI isolation request metadata still fails in nico-api, as it did before the mirror
	if errs["missing `rackName`"] == 0 {
		t.Errorf("no AsyncAPI request metadata failed on rackName: %v", errs)
	}

	// the AsyncAPI side is unchanged by the mirrors: every other publication still validates
	api := loadAsyncAPI(t)
	mirrored := 0
	for _, p := range a.Published("#", time.Time{}) {
		if p.Kind == "mirror" {
			mirrored++
			continue
		}
		if _, err := api.validate(p.Topic, p.Payload); err != nil {
			t.Errorf("%s %s: %v", p.Topic, p.Payload, err)
		}
	}
	if mirrored == 0 {
		t.Error("no mirror publications logged")
	}

	// nico-api's set_rack_leak_state(a01, true): liquid, then electrical (bms_client.rs:175-184)
	elec, liq := routes["RackElectricalIsolationRequest a01"], routes["RackLiquidIsolationRequest a01"]
	if elec == "" || liq == "" {
		t.Fatalf("no nico-api route for a01's requests: %v", routes)
	}
	trays, _ := tw.Engine.SelectDevices("a01/compute-tray-*")
	acIs := func(want bool) func() bool {
		return func() bool {
			for _, d := range trays {
				if tw.Engine.DeviceView(d)["ac"] != want {
					return false
				}
			}
			return true
		}
	}
	pub := func(topic string, payload []byte) {
		t.Helper()
		if tok := client.Publish(topic, 0, false, payload); !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
			t.Fatal(tok.Error())
		}
	}
	const elecStatus = "BMS/v1/PUB/Value/Rack/RackElectricalIsolationStatus/site-1/hall-a/a01/isolation/electrical/status"
	const liqStatus = "BMS/v1/PUB/Value/Rack/RackLiquidIsolationStatus/site-1/hall-a/a01/isolation/liquid/status"
	since := time.Now()
	pub(liq, nicoValueMessage(1, time.Now().UnixMilli()))
	pub(elec, nicoValueMessage(1, time.Now().UnixMilli()))
	waitFor(t, 5*time.Second, "a01's compute trays to lose AC on nico-api's request", acIs(false))
	waitFor(t, 3*time.Second, "RackElectricalIsolationStatus=1", func() bool { return hasValue(a.Published(elecStatus, since), 1) })
	waitFor(t, 3*time.Second, "RackLiquidIsolationStatus=1", func() bool { return hasValue(a.Published(liqStatus, since), 1) })
	liquid := false
	tw.Engine.Read(func(m *model.Model) { liquid = m.RackBy["site-1/hall-a/a01"].Isolated["liquid"] })
	if !liquid {
		t.Error("the model does not record a01 as liquid-isolated")
	}
	reqs := tw.Engine.Events(0, func(ev schema.Event) bool {
		return ev.Kind == "request" && ev.Source.Class == "stack" && ev.Request != nil && (ev.Request.Target == elec || ev.Request.Target == liq)
	})
	if len(reqs) != 2 || !strings.Contains(reqs[0].Message, "nico-api mirror") || reqs[0].Data["quality"] != "1" {
		t.Errorf("want nico-api's two requests committed as stack requests, got %+v", reqs)
	}
	isolations := func() int {
		return len(tw.Engine.Events(0, func(ev schema.Event) bool { return ev.State != nil && ev.State.Field == "isolation.electrical" }))
	}
	if isolations() != 1 {
		t.Errorf("want one isolation.electrical state event, got %d", isolations())
	}
	t.Logf("nico-api request %s → a01 isolated, status 1 after %s", elec, a.Published(elecStatus, since)[0].Time.Sub(since).Round(time.Millisecond))

	// nico-api republishes an unchanged value every 100 s (publisher.rs:143-173): nothing changes
	pub(elec, nicoValueMessage(1, time.Now().UnixMilli()))
	time.Sleep(400 * time.Millisecond)
	if isolations() != 1 {
		t.Errorf("a republished request re-isolated the rack (%d isolation events)", isolations())
	}

	// a message on a mirror topic that is not nico-api's shape is counted and changes nothing
	pub(elec, []byte(`{"value":0,"timestamp":1,"quality":1}`))     // the AsyncAPI's integer quality
	pub(elec, []byte(`{"value":"0","timestamp":1,"quality":"1"}`)) // a number in a string
	pub(elec, []byte(`{"Value":0,"timestamp":1,"quality":"1"}`))   // not the key nico-api writes
	waitFor(t, 3*time.Second, "three non-nico-api messages to be counted", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.apiRej == 3 })
	time.Sleep(200 * time.Millisecond)
	if !acIs(false)() {
		t.Error("a message outside nico-api's shape released the rack")
	}

	// leak cleared in NICo: set_rack_leak_state(a01, false) publishes 0 and the BMS releases
	since = time.Now()
	pub(liq, nicoValueMessage(0, time.Now().UnixMilli()))
	pub(elec, nicoValueMessage(0, time.Now().UnixMilli()))
	waitFor(t, 5*time.Second, "AC back on a01's trays", acIs(true))
	waitFor(t, 3*time.Second, "RackElectricalIsolationStatus=0", func() bool { return hasValue(a.Published(elecStatus, since), 0) })

	// nico-api's heartbeat (publisher.rs:241-253): logged, reported, not an engine event
	now := time.Now().UnixMilli()
	pub(hbTopic, nicoValueMessage(now, now))
	pub(hbTopic, nicoValueMessage(now+5000, now+5000))
	waitFor(t, 3*time.Second, "two heartbeats logged", func() bool { return len(a.Received(hbTopic, time.Time{})) == 2 })
	lines := strings.Join(a.Report().Lines, "\n")
	if !strings.Contains(lines, "2 values received on "+hbTopic) || !strings.Contains(lines, `quality came as the string "1"`) {
		t.Errorf("report:\n%s", lines)
	}
	if n := len(tw.Engine.Events(0, func(ev schema.Event) bool { return ev.Request != nil && ev.Request.Target == hbTopic })); n != 0 {
		t.Errorf("%d heartbeats committed as events", n)
	}
	if !strings.Contains(lines, "consumer mirror nico-api:") || !strings.Contains(lines, "consumer mirror nico-legacy:") {
		t.Errorf("the report does not name both mirrors:\n%s", lines)
	}
	t.Logf("report:\n  %s", strings.ReplaceAll(lines, "\n", "\n  "))
}

// The mirror list: a stack output selects mirrors when the config names none; names are trimmed;
// an unknown name stops the adapter rather than publishing nothing quietly.
func TestMirrorSelection(t *testing.T) {
	if m, err := parseMirrors(" nico-api ,, nico-legacy,nico-api "); err != nil || len(m) != 2 || !m[MirrorNICoAPI] || !m[MirrorNICoLegacy] {
		t.Errorf("parseMirrors = %v, %v", m, err)
	}
	if m, err := parseMirrors(""); err != nil || len(m) != 0 {
		t.Errorf("parseMirrors(\"\") = %v, %v", m, err)
	}
	if _, err := parseMirrors("nico-legacy,nico-flow"); err == nil || !strings.Contains(err.Error(), `"nico-flow"`) {
		t.Errorf("an unknown mirror was accepted: %v", err)
	}

	est, reg := loadLab(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{"bms"}, Log: quiet,
		Outputs: map[string]string{"bmsConsumerMirror": "nico-api"}})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	a := tw.Adapter("bms").(*Adapter)
	select {
	case <-a.Ready():
	case <-time.After(15 * time.Second):
		t.Fatalf("never ready: %v", a.Report().Lines)
	}
	if !a.mirrors[MirrorNICoAPI] || a.mirrors[MirrorNICoLegacy] || len(a.apiReqs) == 0 {
		t.Errorf("stack output nico-api: mirrors %v, %d mirrored requests", a.mirrors, len(a.apiReqs))
	}
	if n := len(a.Published("BMS/v1/PUB/Rack/#", time.Time{})); n != 0 {
		t.Errorf("the nico-legacy mirror published %d messages without being selected", n)
	}
	for _, p := range a.Published(nicoAPIMetadataPrefix+"Rack/+/#", time.Time{}) {
		if p.Kind == "mirror" && !p.Retained {
			t.Errorf("mirror metadata %s is not retained", p.Topic)
		}
	}

	_, err = twin.UpEstate(est, labEstate, reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{"bms"}, Log: quiet,
		Outputs: map[string]string{"bmsConsumerMirror": "nico-legacy,nico-flow"}})
	if err == nil || !strings.Contains(err.Error(), "nico-flow") {
		t.Errorf("an unknown mirror in the stack output did not stop the adapter: %v", err)
	}
}

// When the twin goes down, its broker closes a 3.1.1 client's connection without the server
// DISCONNECT 3.1.1 does not have (nico-api's rumqttc logged "Invalid incoming packet =
// Disconnect" at every twin shutdown). A raw client sees exactly what the broker writes.
func TestEmbeddedBrokerClosesMQTT311WithoutDisconnect(t *testing.T) {
	est, reg := loadLab(t)
	_, a := up(t, est, reg, "bms-fast")
	conn, err := net.Dial("tcp", strings.TrimPrefix(a.Broker(), "tcp://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	id := "raw-311"
	body := append([]byte{0, 4, 'M', 'Q', 'T', 'T', 4, 0x02, 0, 60, 0, byte(len(id))}, id...) // level 4, clean session
	if _, err := conn.Write(append([]byte{0x10, byte(len(body))}, body...)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	ack := make([]byte, 4)
	if _, err := io.ReadFull(conn, ack); err != nil || !bytes.Equal(ack, []byte{0x20, 2, 0, 0}) {
		t.Fatalf("CONNACK %x, %v", ack, err)
	}
	if err := a.Stop(); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	rest, err := io.ReadAll(conn)
	if len(rest) > 0 {
		t.Errorf("the broker wrote %x to a 3.1.1 client while closing (0xe0 is DISCONNECT)", rest)
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Error("the connection was left open")
	}
}

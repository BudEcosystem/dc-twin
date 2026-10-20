package bms

import (
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

var userSrc = schema.EventSource{Class: "user", Principal: "test"}

// fast cadences so periodic behaviour shows inside a test; Name() is still "bms".
func init() {
	twin.Register("bms-fast", func() adapter.Adapter {
		return NewWithConfig(Config{MetadataEvery: 2 * time.Second, ValueEvery: time.Second,
			HeartbeatEvery: 300 * time.Millisecond, SampleEvery: 200 * time.Millisecond})
	})
	twin.Register("bms-mirror", func() adapter.Adapter {
		return NewWithConfig(Config{ConsumerMirror: MirrorNICoLegacy})
	})
	twin.Register("bms-unreachable", func() adapter.Adapter {
		return NewWithConfig(Config{ConnectWait: 300 * time.Millisecond})
	})
}

func TestMatch(t *testing.T) {
	cases := []struct {
		filter, topic string
		want          bool
	}{
		{"BMS/v1/PUB/Value/Rack/RackLeakDetect/#", "BMS/v1/PUB/Value/Rack/RackLeakDetect/site-1/hall-a/a01/leak-point-01/leak", true},
		{"BMS/v1/PUB/Value/Rack/RackLeakDetect/#", "BMS/v1/PUB/Value/Rack/RackLeakDetect", true}, // # matches the parent level
		{"BMS/v1/PUB/Value/Rack/RackLeakDetect/#", "BMS/v1/PUB/Value/Rack/RackPower/site-1/hall-a/a01/power", false},
		{"BMS/v1/+/Value/Rack/#", "BMS/v1/NICO/Value/Rack/RackElectricalIsolationRequest/x", true},
		{"BMS/v1/+/Value/Rack/#", "BMS/v1/NICO/Metadata/Rack/RackElectricalIsolationRequest/x", false},
		{"BMS/+", "BMS/v1/PUB", false},
		{"BMS/v1/PUB/Value/+/+/+", "BMS/v1/PUB/Value/System/HeartbeatTimestampBms/site-1", true},
		{"#", "BMS/v1/anything", true},
		{"#", "$SYS/broker", false},
		{"a/b", "a/b", true},
		{"a/b", "a/b/c", false},
	}
	for _, c := range cases {
		if got := Match(c.filter, c.topic); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.filter, c.topic, got, c.want)
		}
	}
	if got, ok := integrationTopic("BMS/v1/PUB/Metadata/Rack/RackElectricalIsolationRequest/s/h/a01/isolation/electrical/request", "NICO"); !ok ||
		got != "BMS/v1/NICO/Value/Rack/RackElectricalIsolationRequest/s/h/a01/isolation/electrical/request" {
		t.Errorf("integrationTopic = %q", got)
	}
}

// The oracle must refuse what the AsyncAPI refuses, or every validation below proves nothing.
func TestAsyncAPIOracleRejects(t *testing.T) {
	api := loadAsyncAPI(t)
	good := `{"objectType":"Rack","pointType":"RackLeakDetect","rackLocationName":"a01","rackLocationId":"a01","stateText":[{"value":0,"text":"NoLeak"},{"value":1,"text":"Leak"}]}`
	if _, err := api.validate("BMS/v1/PUB/Metadata/Rack/RackLeakDetect/x/y", []byte(good)); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	bad := map[string]string{
		"an extra property (unevaluatedProperties: false)": `{"objectType":"Rack","pointType":"RackLeakDetect","rackLocationName":"a01","rackLocationId":"a01","objectId":"x","stateText":[{"value":0,"text":"NoLeak"}]}`,
		"no rackLocationId":                 `{"objectType":"Rack","pointType":"RackLeakDetect","rackLocationName":"a01","stateText":[{"value":0,"text":"NoLeak"}]}`,
		"engUnit instead of stateText":      `{"objectType":"Rack","pointType":"RackLeakDetect","rackLocationName":"a01","rackLocationId":"a01","engUnit":"C"}`,
		"payload naming another point type": `{"objectType":"Rack","pointType":"RackLeakSensorFault","rackLocationName":"a01","rackLocationId":"a01","stateText":[{"value":0,"text":"NoFault"}]}`,
	}
	for why, p := range bad {
		if _, err := api.validate("BMS/v1/PUB/Metadata/Rack/RackLeakDetect/x/y", []byte(p)); err == nil {
			t.Errorf("metadata with %s was accepted", why)
		}
	}
	if _, err := api.validate("BMS/v1/PUB/Value/Rack/RackLeakDetect/x", []byte(`{"value":1,"timestamp":1760800000000,"quality":1}`)); err != nil {
		t.Errorf("valid value rejected: %v", err)
	}
	for _, p := range []string{`{"value":1,"timestamp":"now","quality":1}`, `{"value":1,"timestamp":1}`, `{"value":1,"timestamp":1,"quality":1,"x":0}`} {
		if _, err := api.validate("BMS/v1/PUB/Value/Rack/RackLeakDetect/x", []byte(p)); err == nil {
			t.Errorf("value %s was accepted", p)
		}
	}
	if _, err := api.validate("BMS/v1/PUB/Value/Rack/RackLeakDetectTray/x", []byte(`{"value":1,"timestamp":1,"quality":1}`)); err == nil {
		t.Error("a BMS-published value of an integration-owned point type was accepted")
	}
	if _, err := api.validate("BMS/v1/PUB/Value/CDU/SupplyTemperature/x", []byte(`{"value":1,"timestamp":1,"quality":1}`)); err == nil {
		t.Error("a CDU point type outside the AsyncAPI was accepted")
	}
}

// The lab estate on the twin-hosted broker: points, AsyncAPI conformance, leak inject and clear,
// electrical and liquid isolation through the integration topics, and the cadences.
func TestEmbeddedBroker(t *testing.T) {
	est, reg := loadLab(t)
	tw, a := up(t, est, reg, "bms-fast")
	if !a.Embedded() {
		t.Fatalf("expected the twin-hosted broker; broker %s", a.Broker())
	}
	rep := a.Report()
	t.Logf("report lines:\n  %s", strings.Join(rep.Lines, "\n  "))
	if !strings.Contains(strings.Join(rep.Lines, "\n"), "twin-hosted broker") || len(rep.Stubs) == 0 {
		t.Errorf("the report does not label the twin-hosted broker: %v / %v", rep.Lines, rep.Stubs)
	}
	sub, client := subscriber(t, a.Broker(), "BMS/v1/#")
	api := loadAsyncAPI(t)

	// the point list the lab estate yields
	want := []string{
		"BMS/v1/PUB/Metadata/Rack/RackLeakDetect/site-1/hall-a/a01/leak-point-01/leak",
		"BMS/v1/PUB/Metadata/Rack/RackLeakSensorFault/site-1/hall-a/a01/leak-point-02/fault",
		"BMS/v1/PUB/Metadata/Rack/RackPower/site-1/hall-a/c01/power",
		"BMS/v1/PUB/Metadata/Rack/RackLiquidSupplyTemperature/site-1/hall-a/a02/liquid/supplyTemperature",
		"BMS/v1/PUB/Metadata/Rack/RackLiquidFlow/site-1/hall-a/a01/liquid/flow",
		"BMS/v1/PUB/Metadata/Rack/RackElectricalIsolationStatus/site-1/hall-a/a01/isolation/electrical/status",
		"BMS/v1/PUB/Metadata/Rack/RackElectricalIsolationRequest/site-1/hall-a/a01/isolation/electrical/request",
		"BMS/v1/PUB/Metadata/Rack/RackLiquidIsolationRequest/site-1/hall-a/a02/isolation/liquid/request",
		"BMS/v1/PUB/Metadata/Rack/RackLeakDetectTray/site-1/hall-a/a01/leak/tray",
		"BMS/v1/PUB/Metadata/CDU/LiquidTemperature/site-1/hall-a/facility/cdu-1/supply",
		"BMS/v1/PUB/Metadata/CDU/LiquidTemperature/site-1/hall-a/facility/cdu-1/return",
		"BMS/v1/PUB/Metadata/CDU/LiquidFlow/site-1/hall-a/facility/cdu-1/flow",
		"BMS/v1/PUB/Metadata/CDU/LeakDetect/site-1/hall-a/facility/cdu-1/leak",
		"BMS/v1/PUB/Metadata/System/HeartbeatTimestampBms/site-1/bms",
		"BMS/v1/PUB/Metadata/System/HeartbeatTimestampIntegration/site-1/integration/NICO",
	}
	for _, topic := range want {
		if len(a.Published(topic, time.Time{})) == 0 {
			t.Errorf("no metadata published on %s", topic)
		}
	}
	if n := len(a.Published("BMS/v1/PUB/Metadata/Rack/RackLiquidFlow/site-1/hall-a/b01/#", time.Time{})); n != 0 {
		t.Errorf("an air-cooled rack on no loop got liquid points")
	}
	// retained metadata reaches a subscriber that connected after the publication
	waitFor(t, 5*time.Second, "retained metadata at the late subscriber", func() bool {
		for _, p := range sub.all() {
			if p.Retained && strings.HasPrefix(p.Topic, "BMS/v1/PUB/Metadata/Rack/RackLeakDetect/") {
				return true
			}
		}
		return false
	})
	var md map[string]any
	_ = json.Unmarshal(a.Published("BMS/v1/PUB/Metadata/CDU/LiquidTemperature/site-1/hall-a/facility/cdu-1/supply", time.Time{})[0].Payload, &md)
	t.Logf("CDU supply metadata: %v", md)

	// leak: inject, observe 1 at the publisher's log and at the subscriber; clear, observe 0
	leakTopic := "BMS/v1/PUB/Value/Rack/RackLeakDetect/site-1/hall-a/a01/leak-point-01/leak"
	since := time.Now()
	if _, err := tw.Engine.Inject("a01/leak-point-01", "generic/leak/rack-point", nil, "rackleak", userSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "RackLeakDetect=1 in the publisher's log", func() bool { return hasValue(a.Published(leakTopic, since), 1) })
	waitFor(t, 3*time.Second, "RackLeakDetect=1 at the subscriber", func() bool { _, ok := sub.hasValue(leakTopic, since, 1); return ok })
	t.Logf("leak published %s after inject", a.Published(leakTopic, since)[0].Time.Sub(since).Round(time.Millisecond))
	if hasValue(a.Published("BMS/v1/PUB/Value/Rack/RackLeakDetect/site-1/hall-a/a01/leak-point-02/leak", since), 1) {
		t.Error("the other leak point of the rack reported a leak")
	}
	since = time.Now()
	if _, err := tw.Engine.Clear("rackleak", "", userSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "RackLeakDetect=0 after clear", func() bool { return hasValue(a.Published(leakTopic, since), 0) })

	// electrical isolation requested by the integration, derived from the metadata as the contract says
	trays, _ := tw.Engine.SelectDevices("a01/compute-tray-*")
	if len(trays) != 18 {
		t.Fatalf("a01 has %d compute trays", len(trays))
	}
	acOff := func() bool {
		for _, d := range trays {
			if tw.Engine.DeviceView(d)["ac"] != false {
				return false
			}
		}
		return true
	}
	reqTopic := integrationTopicFromMetadata(t, sub.all(), "a01", "RackElectricalIsolationRequest", "NICO")
	statusTopic := "BMS/v1/PUB/Value/Rack/RackElectricalIsolationStatus/site-1/hall-a/a01/isolation/electrical/status"
	since = time.Now()
	if tok := client.Publish(reqTopic, 0, false, valuePayload(1)); !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	waitFor(t, 5*time.Second, "a01's compute trays to lose AC", acOff)
	waitFor(t, 3*time.Second, "RackElectricalIsolationStatus=1", func() bool { return hasValue(a.Published(statusTopic, since), 1) })
	t.Logf("electrical isolation: trays off and status 1 %s after the request (delay 5s × timeScale 0.05)",
		a.Published(statusTopic, since)[0].Time.Sub(since).Round(time.Millisecond))
	if acOff2, _ := tw.Engine.SelectDevices("a02/compute-tray-01"); tw.Engine.DeviceView(acOff2[0])["ac"] != true {
		t.Error("isolating a01 took a02's power")
	}
	evs := tw.Engine.Events(0, func(ev schema.Event) bool {
		return ev.Kind == "request" && ev.Source.Class == "stack" && ev.Request != nil && ev.Request.Target == reqTopic
	})
	if len(evs) != 1 {
		t.Errorf("want the request committed once as a stack request, got %d", len(evs))
	}
	if n := len(tw.Engine.Events(0, func(ev schema.Event) bool { return ev.State != nil && ev.State.Field == "isolation.electrical" })); n != 1 {
		t.Errorf("want one isolation.electrical state event, got %d", n)
	}
	// a republished request (integrations republish every 100 s) changes nothing
	_ = client.Publish(reqTopic, 0, false, valuePayload(1))
	time.Sleep(400 * time.Millisecond)
	if n := len(tw.Engine.Events(0, func(ev schema.Event) bool { return ev.State != nil && ev.State.Field == "isolation.electrical" })); n != 1 {
		t.Errorf("a republished request re-isolated the rack (%d isolation events)", n)
	}
	// release
	since = time.Now()
	_ = client.Publish(reqTopic, 0, false, valuePayload(0))
	waitFor(t, 5*time.Second, "AC back on a01's trays", func() bool {
		for _, d := range trays {
			if tw.Engine.DeviceView(d)["ac"] != true {
				return false
			}
		}
		return true
	})
	waitFor(t, 3*time.Second, "RackElectricalIsolationStatus=0", func() bool { return hasValue(a.Published(statusTopic, since), 0) })

	// liquid isolation: status, and the rack's flow goes to zero while the other rack takes the loop
	lreq := integrationTopicFromMetadata(t, sub.all(), "a01", "RackLiquidIsolationRequest", "NICO")
	since = time.Now()
	_ = client.Publish(lreq, 0, false, valuePayload(1))
	waitFor(t, 5*time.Second, "RackLiquidIsolationStatus=1", func() bool {
		return hasValue(a.Published("BMS/v1/PUB/Value/Rack/RackLiquidIsolationStatus/site-1/hall-a/a01/isolation/liquid/status", since), 1)
	})
	waitFor(t, 3*time.Second, "a01 flow 0", func() bool {
		return hasValue(a.Published("BMS/v1/PUB/Value/Rack/RackLiquidFlow/site-1/hall-a/a01/liquid/flow", since), 0)
	})
	waitFor(t, 3*time.Second, "a02 flow takes the loop", func() bool {
		return hasValue(a.Published("BMS/v1/PUB/Value/Rack/RackLiquidFlow/site-1/hall-a/a02/liquid/flow", since), 600)
	})
	liquid := false
	tw.Engine.Read(func(m *model.Model) { liquid = m.RackBy["site-1/hall-a/a01"].Isolated["liquid"] })
	if !liquid {
		t.Error("the model does not record a01 as liquid-isolated")
	}

	// RackLeakDetectTray from the integration: informational, committed, no action
	tray := integrationTopicFromMetadata(t, sub.all(), "a01", "RackLeakDetectTray", "NICO")
	_ = client.Publish(tray, 0, false, valuePayload(1))
	waitFor(t, 3*time.Second, "the tray-leak report to be committed", func() bool {
		return len(tw.Engine.Events(0, func(ev schema.Event) bool { return ev.Request != nil && ev.Request.Target == tray })) == 1
	})
	// a malformed integration message is ignored and counted
	_ = client.Publish(reqTopic, 0, false, []byte(`{"value":"yes"}`))
	waitFor(t, 3*time.Second, "the malformed request to be counted", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.rejected == 1 })

	// the integration's heartbeat, on the topic derived from the BMS's HeartbeatTimestampIntegration
	// metadata (System metadata carries objectName/objectId; integration drives the topic)
	hbTopic := strings.Replace(strings.Replace("BMS/v1/PUB/Metadata/System/HeartbeatTimestampIntegration/site-1/integration/NICO", "/PUB/", "/NICO/", 1), "/Metadata/", "/Value/", 1)
	_ = client.Publish(hbTopic, 0, false, valuePayload(float64(time.Now().UnixMilli())))
	waitFor(t, 3*time.Second, "the integration heartbeat to be logged", func() bool { return len(a.Received(hbTopic, time.Time{})) == 1 })
	if lines := strings.Join(a.Report().Lines, "\n"); !strings.Contains(lines, "1 values received on "+hbTopic) || strings.Contains(lines, "quality came as the string") {
		t.Errorf("an AsyncAPI-shaped integration heartbeat is not reported as such:\n%s", lines)
	}

	// cadences: heartbeat every 300 ms carrying the clock; values republished every 1 s unchanged
	hbSince := time.Now()
	time.Sleep(1300 * time.Millisecond)
	hbs := a.Published("BMS/v1/PUB/Value/System/HeartbeatTimestampBms/#", hbSince)
	if len(hbs) < 3 {
		t.Errorf("want ≥ 3 heartbeats in 1.3 s at 300 ms, got %d", len(hbs))
	}
	if v, _ := hbs[len(hbs)-1].Value(); time.Since(time.UnixMilli(int64(v))) > 2*time.Second {
		t.Errorf("heartbeat value %v is not the current epoch milliseconds", v)
	}
	if n := len(a.Published("BMS/v1/PUB/Value/Rack/RackPower/site-1/hall-a/c02/power", hbSince)); n < 1 {
		t.Errorf("RackPower of an idle rack was not republished within 1.3 s (cadence 1 s)")
	}

	// every publication, as logged and as delivered by the broker, conforms to the AsyncAPI
	checked := map[string]int{}
	for _, p := range a.Published("#", time.Time{}) {
		loc, err := api.validate(p.Topic, p.Payload)
		if err != nil {
			t.Errorf("%s %s: %v", p.Topic, p.Payload, err)
			continue
		}
		checked[loc]++
	}
	delivered := 0
	for _, p := range sub.all() {
		if !Match("BMS/v1/PUB/#", p.Topic) {
			continue
		}
		if _, err := api.validate(p.Topic, p.Payload); err != nil {
			t.Errorf("delivered %s %s: %v", p.Topic, p.Payload, err)
		}
		delivered++
	}
	t.Logf("AsyncAPI-validated %d logged publications (%d schema locations) and %d delivered messages", len(a.Published("#", time.Time{})), len(checked), delivered)
	for loc, n := range checked {
		t.Logf("  %4d × %s", n, loc)
	}
}

// The optional mirror feeds NICo's dsx-exchange-consumer the shape its source parses: a topic
// ending /Metadata or /Value whose remainder after BMS/v1 is the shared point path, metadata
// {pointType, objectType, rackName, rackID} with pointType LeakDetectRack, and a value
// {value: 0|1, timestamp: epoch seconds}.
func TestNICoConsumerMirror(t *testing.T) {
	est, reg := loadLab(t)
	tw, a := up(t, est, reg, "bms-mirror")
	since := time.Now()
	if _, err := tw.Engine.Inject("a01/leak-point-01", "generic/leak/rack-point", nil, "rackleak", userSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "the mirrored leak value", func() bool {
		return hasValue(a.Published("BMS/v1/PUB/Rack/LeakDetectRack/+/+/a01/leak-point-01/leak/Value", since), 1)
	})
	// as the consumer does: strip the /Metadata or /Value suffix, then the prefix
	pointPath := func(topic string) string {
		s := strings.TrimSuffix(strings.TrimSuffix(topic, "/Metadata"), "/Value")
		return strings.TrimPrefix(s, "BMS/v1")
	}
	var meta struct {
		PointType  string `json:"pointType"`
		ObjectType string `json:"objectType"`
		RackName   string `json:"rackName"`
		RackID     string `json:"rackID"`
	}
	mp := a.Published("BMS/v1/PUB/Rack/LeakDetectRack/#", time.Time{})
	var metaTopic, valueTopic string
	var value struct {
		Value     *float64 `json:"value"`
		Timestamp int64    `json:"timestamp"`
	}
	for _, p := range mp {
		switch {
		case strings.HasSuffix(p.Topic, "/Metadata") && strings.Contains(p.Topic, "a01/leak-point-01"):
			metaTopic = p.Topic
			if err := json.Unmarshal(p.Payload, &meta); err != nil {
				t.Fatal(err)
			}
		case strings.HasSuffix(p.Topic, "/Value") && strings.Contains(p.Topic, "a01/leak-point-01") && !p.Time.Before(since):
			valueTopic = p.Topic
			if err := json.Unmarshal(p.Payload, &value); err != nil {
				t.Fatal(err)
			}
		}
	}
	if metaTopic == "" || valueTopic == "" || pointPath(metaTopic) != pointPath(valueTopic) {
		t.Fatalf("metadata %q and value %q do not share a point path", metaTopic, valueTopic)
	}
	if meta.PointType != "LeakDetectRack" || meta.ObjectType != "Rack" || meta.RackName != "a01" || meta.RackID != "a01" {
		t.Errorf("mirror metadata %+v", meta)
	}
	if value.Value == nil || *value.Value != 1 || value.Timestamp < time.Now().Add(-time.Minute).Unix() || value.Timestamp > time.Now().Unix()+1 {
		t.Errorf("mirror value %+v is not {1, epoch seconds}", value)
	}
	t.Logf("mirror: %s -> point path %s, metadata %+v", valueTopic, pointPath(valueTopic), meta)
	// the AsyncAPI channels carry the same leak, unaffected by the mirror
	if !hasValue(a.Published("BMS/v1/PUB/Value/Rack/RackLeakDetect/site-1/hall-a/a01/leak-point-01/leak", since), 1) {
		t.Error("the AsyncAPI value was not published alongside the mirror")
	}
}

// The stack's broker may come up after the twin: Start returns after ConnectWait, the client
// keeps retrying, the report says nothing connected, and Stop does not hang.
func TestBrokerNotUpYet(t *testing.T) {
	est, reg := loadLab(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens here
	est.Spec.Facility.BMS.Broker = addr
	start := time.Now()
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{"bms-unreachable"}})
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Start blocked for %s", d)
	}
	a := tw.Adapter("bms").(*Adapter)
	if lines := strings.Join(a.Report().Lines, "\n"); !strings.Contains(lines, "0 connection(s)") || !strings.Contains(lines, "estate facility.bms.broker") {
		t.Errorf("report:\n%s", lines)
	}
	if n := len(a.Published("#", time.Time{})); n != 0 {
		t.Errorf("%d publications logged with no broker", n)
	}
	done := make(chan struct{})
	go func() { tw.Down(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Down hung while the client was retrying")
	}
}

// An estate with no facility.bms starts nothing and says so.
func TestInactiveWithoutBMS(t *testing.T) {
	est, reg := loadLab(t)
	est.Spec.Facility.BMS = nil
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{"bms"}})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	r := tw.Adapter("bms").Report()
	if r.Served != 0 || len(r.Lines) != 1 || !strings.Contains(r.Lines[0], "inactive") {
		t.Errorf("report %+v", r)
	}
}

func hasValue(ps []Publication, want float64) bool {
	for _, p := range ps {
		if v, ok := p.Value(); ok && v == want {
			return true
		}
	}
	return false
}

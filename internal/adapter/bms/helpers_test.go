package bms

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"sigs.k8s.io/yaml"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const labEstate = "../../../examples/estates/lab-model.yaml"

func loadLab(t *testing.T) (*schema.Estate, *pack.Registry) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(labEstate)
	if err != nil {
		t.Fatal(err)
	}
	return est, reg
}

func up(t *testing.T, est *schema.Estate, reg *pack.Registry, adapterName string) (*twin.Twin, *Adapter) {
	t.Helper()
	tw, err := twin.UpEstate(est, labEstate, reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{adapterName},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	a, ok := tw.Adapter("bms").(*Adapter)
	if !ok {
		t.Fatalf("adapter bms is %T", tw.Adapter("bms"))
	}
	select {
	case <-a.Ready():
	case <-time.After(15 * time.Second):
		t.Fatalf("bms adapter never finished its initial publication; report: %v", a.Report().Lines)
	}
	return tw, a
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// ---------------------------------------------------------------- a subscriber on the broker

type collector struct {
	mu   sync.Mutex
	msgs []Publication
}

func (c *collector) add(topic string, payload []byte, retained bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs = append(c.msgs, Publication{Topic: topic, Payload: append([]byte{}, payload...), Retained: retained, Time: time.Now()})
}

func (c *collector) all() []Publication {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Publication{}, c.msgs...)
}

// last returns the latest message on a topic matching filter whose value equals want.
func (c *collector) hasValue(filter string, since time.Time, want float64) (Publication, bool) {
	for _, p := range c.all() {
		if p.Time.Before(since) || !Match(filter, p.Topic) {
			continue
		}
		if v, ok := p.Value(); ok && v == want {
			return p, true
		}
	}
	return Publication{}, false
}

func subscriber(t *testing.T, broker, filter string) (*collector, paho.Client) {
	t.Helper()
	c := &collector{}
	opts := paho.NewClientOptions().AddBroker(broker).SetClientID(fmt.Sprintf("bms-test-%d", time.Now().UnixNano())).
		SetProtocolVersion(4).SetCleanSession(true).SetConnectTimeout(5 * time.Second)
	cl := paho.NewClient(opts)
	if tok := cl.Connect(); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("subscriber connect to %s: %v", broker, tok.Error())
	}
	t.Cleanup(func() { cl.Disconnect(100) })
	if tok := cl.Subscribe(filter, 0, func(_ paho.Client, m paho.Message) { c.add(m.Topic(), m.Payload(), m.Retained()) }); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe %s: %v", filter, tok.Error())
	}
	return c, cl
}

func valuePayload(v float64) []byte {
	b, _ := json.Marshal(map[string]any{"value": v, "timestamp": time.Now().UnixMilli(), "quality": 1})
	return b
}

// integrationTopicFromMetadata follows the AsyncAPI's integration contract from the outside: find
// the BMS metadata for (rack, pointType) whose integration is ours, and derive the value topic
// by PUB → integration, Metadata → Value.
func integrationTopicFromMetadata(t *testing.T, msgs []Publication, rack, pointType, integration string) string {
	t.Helper()
	for _, p := range msgs {
		if !Match("BMS/v1/PUB/Metadata/Rack/"+pointType+"/#", p.Topic) {
			continue
		}
		var m map[string]any
		if json.Unmarshal(p.Payload, &m) != nil || m["rackLocationName"] != rack || m["integration"] != integration {
			continue
		}
		return strings.Replace(strings.Replace(p.Topic, "/PUB/", "/"+integration+"/", 1), "/Metadata/", "/Value/", 1)
	}
	t.Fatalf("no BMS metadata for %s %s owned by %s", rack, pointType, integration)
	return ""
}

// ---------------------------------------------------------------- the AsyncAPI as an oracle

// asyncAPI validates a publication against DSX Exchange's BMS AsyncAPI 1.0.0
// (testdata/bms.yaml, Apache-2.0, from github.com/dsx-ai-factory/dsx-exchange
// schemas/asyncapi/bms/bms.yaml, read 2026-10-18). It resolves the topic to its channel by
// address, checks the channel's parameter enums, finds the message the channel lists for the
// point type and validates the payload against that message's component schema. The schemas
// are compiled as JSON Schema 2020-12 so that their unevaluatedProperties: false is enforced.
type asyncAPI struct {
	doc      map[string]any
	c        *jsonschema.Compiler
	mu       sync.Mutex
	compiled map[string]*jsonschema.Schema
	channels []apiChannel
}

type apiChannel struct {
	name   string
	re     *regexp.Regexp
	params []string
	nparam int
}

const apiURL = "https://dc-twin.invalid/dsx-exchange/bms.json"

func loadAsyncAPI(t *testing.T) *asyncAPI {
	t.Helper()
	b, err := os.ReadFile("testdata/bms.yaml")
	if err != nil {
		t.Fatal(err)
	}
	j, err := yaml.YAMLToJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(j))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(j, &doc); err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource(apiURL, inst); err != nil {
		t.Fatal(err)
	}
	api := &asyncAPI{doc: doc, c: c, compiled: map[string]*jsonschema.Schema{}}
	param := regexp.MustCompile(`\\\{([A-Za-z]+)\\\}`) // a {parameter} after QuoteMeta
	for name, ch := range doc["channels"].(map[string]any) {
		addr := ch.(map[string]any)["address"].(string)
		var params []string
		expr := "^" + param.ReplaceAllStringFunc(regexp.QuoteMeta(addr), func(s string) string {
			p := s[2 : len(s)-2]
			params = append(params, p)
			if p == "tagPath" {
				return "(.+)"
			}
			return "([^/]+)"
		}) + "$"
		api.channels = append(api.channels, apiChannel{name: name, re: regexp.MustCompile(expr), params: params, nparam: len(params)})
	}
	sort.Slice(api.channels, func(i, j int) bool { return api.channels[i].nparam < api.channels[j].nparam })
	return api
}

func dig(v any, path ...string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func (api *asyncAPI) schema(loc string) (*jsonschema.Schema, error) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if s, ok := api.compiled[loc]; ok {
		return s, nil
	}
	s, err := api.c.Compile(apiURL + loc)
	if err != nil {
		return nil, err
	}
	api.compiled[loc] = s
	return s, nil
}

// validate checks a BMS publication; it returns the schema location it validated against.
func (api *asyncAPI) validate(topic string, payload []byte) (string, error) {
	var ch *apiChannel
	var vals map[string]string
	for i := range api.channels {
		m := api.channels[i].re.FindStringSubmatch(topic)
		if m == nil {
			continue
		}
		vals = map[string]string{}
		for k, p := range api.channels[i].params {
			vals[p] = m[k+1]
		}
		if vals["integration"] == "PUB" { // PUB is the BMS's namespace, never an integration's
			continue
		}
		ch = &api.channels[i]
		break
	}
	if ch == nil {
		return "", fmt.Errorf("topic %s matches no channel of the AsyncAPI", topic)
	}
	chDoc := dig(api.doc, "channels", ch.name)
	for p, v := range vals {
		enum, _ := dig(chDoc, "parameters", p, "enum").([]any)
		if enum == nil {
			continue
		}
		found := false
		for _, e := range enum {
			if e == v {
				found = true
			}
		}
		if !found {
			return "", fmt.Errorf("topic %s: %s %q is not in channel %s's enum", topic, p, v, ch.name)
		}
	}
	// the channel's messages: a value channel lists ValueMessage; a metadata channel one message
	// per point type, whose payload is the point type's component schema
	objectType := strings.Split(strings.TrimPrefix(topic, "BMS/v1/"), "/")[2]
	want := ""
	if strings.Contains(ch.name, "Metadata") {
		if objectType == "Rack" {
			want = vals["pointType"] + "Metadata"
		} else {
			want = objectType + vals["pointType"] + "Metadata"
		}
	}
	loc := ""
	msgs, _ := dig(chDoc, "messages").(map[string]any)
	for _, m := range msgs {
		ref, _ := dig(m, "$ref").(string)
		name := strings.TrimPrefix(ref, "#/components/messages/")
		payloadRef, _ := dig(api.doc, "components", "messages", name, "payload", "$ref").(string)
		switch {
		case want == "" && name == "ValueMessage":
			loc = "#/components/messages/ValueMessage/payload"
		case want != "" && payloadRef == "#/components/schemas/"+want:
			loc = "#/components/schemas/" + want
		}
	}
	if loc == "" {
		return "", fmt.Errorf("topic %s: channel %s lists no message for %q", topic, ch.name, want)
	}
	s, err := api.schema(loc)
	if err != nil {
		return loc, err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		return loc, err
	}
	if err := s.Validate(inst); err != nil {
		return loc, err
	}
	if want != "" { // a metadata payload names the topic's own object and point type
		var m map[string]any
		_ = json.Unmarshal(payload, &m)
		if m["objectType"] != objectType || m["pointType"] != vals["pointType"] {
			return loc, fmt.Errorf("topic %s: payload says %v/%v", topic, m["objectType"], m["pointType"])
		}
	}
	return loc, nil
}

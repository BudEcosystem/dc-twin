package bms

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// natsServer finds a nats-server binary: $DCTWIN_NATS_SERVER, else nats-server on PATH.
// Install one with: GOBIN=<dir> go install github.com/nats-io/nats-server/v2@latest
func natsServer(t *testing.T) string {
	if p := os.Getenv("DCTWIN_NATS_SERVER"); p != "" {
		return p
	}
	if p, err := exec.LookPath("nats-server"); err == nil {
		return p
	}
	t.Skip("no nats-server: set DCTWIN_NATS_SERVER to a nats-server binary (go install github.com/nats-io/nats-server/v2@latest)")
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// startNATS runs a real NATS server with JetStream (which its MQTT support requires, for
// retained messages among others) and an MQTT listener: DSX Exchange's event bus is NATS
// with MQTT 3.1.1 for publishers.
func startNATS(t *testing.T) (natsURL, mqttAddr string) {
	bin := natsServer(t)
	dir := t.TempDir()
	np, mp := freePort(t), freePort(t)
	conf := filepath.Join(dir, "nats.conf")
	body := fmt.Sprintf(`server_name: dctwin_bms_e2e
listen: 127.0.0.1:%d
jetstream { store_dir: %q }
mqtt { listen: 127.0.0.1:%d }
`, np, filepath.Join(dir, "js"), mp)
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out := &syncBuf{}
	cmd := exec.Command(bin, "-c", conf)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		if t.Failed() {
			t.Logf("nats-server output:\n%s", out.String())
		}
	})
	natsURL, mqttAddr = fmt.Sprintf("nats://127.0.0.1:%d", np), fmt.Sprintf("127.0.0.1:%d", mp)
	waitFor(t, 15*time.Second, "nats-server to listen for NATS and MQTT", func() bool {
		for _, a := range []string{fmt.Sprintf("127.0.0.1:%d", np), mqttAddr} {
			c, err := net.DialTimeout("tcp", a, 200*time.Millisecond)
			if err != nil {
				return false
			}
			c.Close()
		}
		return true
	})
	if v, err := exec.Command(bin, "--version").Output(); err == nil {
		t.Logf("%s: NATS %s, MQTT %s, JetStream in %s", strings.TrimSpace(string(v)), natsURL, mqttAddr, dir)
	}
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.Contains(l, "MQTT") || strings.Contains(l, "Server is ready") {
			t.Logf("nats-server: %s", strings.TrimSpace(l))
		}
	}
	return natsURL, mqttAddr
}

// The lab estate's BMS on a real NATS server as the stack's broker. A DSX Exchange consumer
// sees the MQTT topics as NATS subjects (BMS/v1/PUB/Value/... ↔ BMS.v1.PUB.Value....); an MQTT
// client sees the topics. Both must see the leak and its clearing, every payload must conform
// to the AsyncAPI, and an isolation request published through NATS must isolate the rack.
func TestNATSEndToEnd(t *testing.T) {
	natsURL, mqttAddr := startNATS(t)
	est, reg := loadLab(t)
	est.Spec.Facility.BMS.Broker = mqttAddr

	// (a) a NATS subscriber, as a DSX Exchange consumer on the bus sees it
	nc, err := nats.Connect(natsURL, nats.Name("dc-twin bms e2e"))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	natsMsgs := &collector{}
	for _, subj := range []string{"BMS.v1.PUB.Value.Rack.RackLeakDetect.>", "BMS.v1.PUB.Value.Rack.RackElectricalIsolationStatus.>", "BMS.v1.PUB.Metadata.>"} {
		if _, err := nc.Subscribe(subj, func(m *nats.Msg) { natsMsgs.add(m.Subject, m.Data, false) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}

	tw, a := up(t, est, reg, "bms") // the registered adapter, AsyncAPI cadences
	if a.Embedded() || a.Broker() != "tcp://"+mqttAddr {
		t.Fatalf("the adapter did not use the stack's broker: %s", a.Broker())
	}
	t.Logf("report: %s", strings.Join(a.Report().Lines[:3], " | "))

	// (b) an MQTT subscriber on the same broker
	mq, client := subscriber(t, "tcp://"+mqttAddr, "BMS/v1/PUB/#")
	api := loadAsyncAPI(t)
	waitFor(t, 5*time.Second, "retained metadata through NATS to the late MQTT subscriber", func() bool {
		_, ok := firstMatch(mq.all(), "BMS/v1/PUB/Metadata/Rack/RackLeakDetect/#")
		return ok
	})

	const topic = "BMS/v1/PUB/Value/Rack/RackLeakDetect/site-1/hall-a/a01/leak-point-01/leak"
	const subject = "BMS.v1.PUB.Value.Rack.RackLeakDetect.site-1.hall-a.a01.leak-point-01.leak"
	since := time.Now()
	if _, err := tw.Engine.Inject("a01/leak-point-01", "generic/leak/rack-point", nil, "rackleak", userSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "RackLeakDetect=1 on the NATS subject", func() bool { _, ok := natsMsgs.hasValue(subject, since, 1); return ok })
	waitFor(t, 5*time.Second, "RackLeakDetect=1 on the MQTT topic", func() bool { _, ok := mq.hasValue(topic, since, 1); return ok })
	n1, _ := natsMsgs.hasValue(subject, since, 1)
	m1, _ := mq.hasValue(topic, since, 1)
	t.Logf("leak: NATS  %s %s (+%s)", n1.Topic, n1.Payload, n1.Time.Sub(since).Round(time.Millisecond))
	t.Logf("leak: MQTT  %s %s (+%s)", m1.Topic, m1.Payload, m1.Time.Sub(since).Round(time.Millisecond))
	if !hasValue(a.Published(topic, since), 1) {
		t.Error("the publisher's log does not hold the leak")
	}

	since = time.Now()
	if _, err := tw.Engine.Clear("rackleak", "", userSrc); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "RackLeakDetect=0 on the NATS subject", func() bool { _, ok := natsMsgs.hasValue(subject, since, 0); return ok })
	waitFor(t, 5*time.Second, "RackLeakDetect=0 on the MQTT topic", func() bool { _, ok := mq.hasValue(topic, since, 0); return ok })
	n0, _ := natsMsgs.hasValue(subject, since, 0)
	t.Logf("clear: NATS %s %s (+%s)", n0.Topic, n0.Payload, n0.Time.Sub(since).Round(time.Millisecond))

	// isolation: the integration publishes its request over MQTT through NATS
	reqTopic := integrationTopicFromMetadata(t, mq.all(), "a01", "RackElectricalIsolationRequest", "NICO")
	trays, _ := tw.Engine.SelectDevices("a01/compute-tray-*")
	since = time.Now()
	if tok := client.Publish(reqTopic, 0, false, valuePayload(1)); !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	waitFor(t, 5*time.Second, "a01's 18 compute trays to lose AC", func() bool {
		for _, d := range trays {
			if tw.Engine.DeviceView(d)["ac"] != false {
				return false
			}
		}
		return true
	})
	const statusSubject = "BMS.v1.PUB.Value.Rack.RackElectricalIsolationStatus.site-1.hall-a.a01.isolation.electrical.status"
	waitFor(t, 5*time.Second, "RackElectricalIsolationStatus=1 on the NATS subject", func() bool { _, ok := natsMsgs.hasValue(statusSubject, since, 1); return ok })
	s1, _ := natsMsgs.hasValue(statusSubject, since, 1)
	view := tw.Engine.DeviceView(trays[0])
	t.Logf("isolation: request on %s; %s ac=%v power=%v; status %s %s (+%s)", reqTopic, view["path"], view["ac"], view["power"], s1.Topic, s1.Payload, s1.Time.Sub(since).Round(time.Millisecond))
	if got := a.Received(reqTopic, since); len(got) != 1 {
		t.Errorf("the BMS received the request %d times", len(got))
	}

	// every payload: as logged, as delivered over MQTT, and as seen on the NATS subjects
	bad := 0
	check := func(where, topic string, payload []byte) {
		if _, err := api.validate(topic, payload); err != nil {
			bad++
			t.Errorf("%s %s %s: %v", where, topic, payload, err)
		}
	}
	logged := a.Published("#", time.Time{})
	for _, p := range logged {
		check("logged", p.Topic, p.Payload)
	}
	delivered := mq.all()
	for _, p := range delivered {
		check("mqtt", p.Topic, p.Payload)
	}
	viaNATS := natsMsgs.all()
	for _, p := range viaNATS {
		check("nats", strings.ReplaceAll(p.Topic, ".", "/"), p.Payload)
	}
	t.Logf("AsyncAPI: %d logged, %d MQTT-delivered, %d NATS-delivered messages validated, %d invalid", len(logged), len(delivered), len(viaNATS), bad)
}

func firstMatch(ps []Publication, filter string) (Publication, bool) {
	for _, p := range ps {
		if Match(filter, p.Topic) {
			return p, true
		}
	}
	return Publication{}, false
}

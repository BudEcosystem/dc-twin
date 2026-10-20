// Command refstack is dc-twin's reference stack: a small lifecycle and health manager that
// exercises a twin end to end the way site software does, through datacenter protocols only —
// relayed DHCP for BMC discovery, Redfish (gofish) through the site gateway, and BMS points over
// MQTT. It implements the policies NICo documents (identify hardware by Redfish identity strings,
// shut down a tray whose own leak detector trips, raise a PreventAllocations alert for a BMS
// rack leak and power nothing off, ask the BMS to isolate a rack when too many trays leak), and
// exposes the accessors scenarios read. It is NOT NICo and makes no claim about NICo's behaviour
// beyond what its documentation states.
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/insomniacslk/dhcp/dhcpv4"
	mochi "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/redfish"
)

type Machine struct {
	Serial    string    `json:"serial"`
	BMCSerial string    `json:"bmc_serial"`
	BMCMAC    string    `json:"bmc_mac"`
	BMCIP     string    `json:"bmc_ip"`
	Circuit   string    `json:"circuit_id"`
	Vendor    string    `json:"vendor"`
	Product   string    `json:"product"`
	Type      string    `json:"hardware_type"`
	Model     string    `json:"model"`
	Rack      string    `json:"rack"`
	State     string    `json:"state"`
	Power     string    `json:"power"`
	Health    string    `json:"health"`
	Leak      string    `json:"leak_status"`
	Handling  string    `json:"leak_handling_status"`
	Updated   time.Time `json:"updated"`
	user, pw  string
	client    *gofish.APIClient
}

type Alert struct {
	Kind               string   `json:"kind"`
	Rack               string   `json:"rack"`
	Target             string   `json:"target,omitempty"`
	Classifications    []string `json:"classifications"`
	PreventAllocations bool     `json:"preventAllocations"`
	Since              string   `json:"since"`
}

type expected struct {
	BMCMAC   string `json:"bmcMAC"`
	Serial   string `json:"serial"`
	User     string `json:"user"`
	Password string `json:"password"`
	Rack     string `json:"rack"`
}

type stack struct {
	mu        sync.Mutex
	machines  map[string]*Machine // by BMC MAC
	alerts    map[string]*Alert
	leases    map[string]string // mac -> ip
	pool      map[string]int    // subnet -> next host
	expected  map[string]expected
	gateway   string
	isolation map[string]bool
	threshold int
	mq        mqtt.Client
	integ     string
	rackTags  map[string]map[string]string // rack -> pointType -> integration value topic
	log       *log.Logger
}

func main() {
	dhcpAddr := flag.String("dhcp", "127.0.0.1:16767", "UDP address of the stack's DHCP server (relayed requests only)")
	apiAddr := flag.String("api", "127.0.0.1:18080", "accessor API address")
	broker := flag.String("broker", "127.0.0.1:18830", "the stack's MQTT broker (the twin's BMS publishes here)")
	gateway := flag.String("gateway", "", "the twin's site gateway (HTTP CONNECT) for reaching BMC site addresses")
	expectedFile := flag.String("expected", "", "expected machines file (BMC MAC, serial, factory credentials), as an operator loads into a lifecycle manager")
	threshold := flag.Int("isolation-threshold", 2, "leaking trays in one rack above which the BMS is asked to isolate it")
	flag.Parse()
	s := &stack{machines: map[string]*Machine{}, alerts: map[string]*Alert{}, leases: map[string]string{}, pool: map[string]int{},
		expected: map[string]expected{}, gateway: *gateway, isolation: map[string]bool{}, threshold: *threshold, integ: "NICO",
		rackTags: map[string]map[string]string{}, log: log.New(os.Stderr, "refstack ", log.Ltime|log.Lmicroseconds)}
	if err := s.startBroker(*broker); err != nil {
		s.log.Fatal(err)
	}
	go s.dhcp(*dhcpAddr)
	go s.loadExpected(*expectedFile)
	go s.ingestLoop()
	go s.healthLoop()
	go s.bmsConsumer(*broker)
	s.log.Printf("up: dhcp %s, api http://%s, broker %s, gateway %s", *dhcpAddr, *apiAddr, *broker, *gateway)
	http.HandleFunc("/v1/machines", s.apiMachines)
	http.HandleFunc("/v1/machines/", s.apiMachine)
	http.HandleFunc("/v1/alerts", s.apiAlerts)
	s.log.Fatal(http.ListenAndServe(*apiAddr, nil))
}

// ---------------------------------------------------------------- the stack's broker

func (s *stack) startBroker(addr string) error {
	srv := mochi.New(&mochi.Options{InlineClient: true})
	_ = srv.AddHook(new(allowAll), nil)
	if err := srv.AddListener(listeners.NewTCP(listeners.Config{ID: "exchange", Address: addr})); err != nil {
		return err
	}
	go srv.Serve()
	return nil
}

// ---------------------------------------------------------------- DHCP (relayed only)

func (s *stack) dhcp(addr string) {
	ua, _ := net.ResolveUDPAddr("udp", addr)
	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		s.log.Fatal(err)
	}
	buf := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		req, err := dhcpv4.FromBytes(buf[:n])
		if err != nil {
			continue
		}
		// a relayed request only, as NICo's Kea hook accepts (stack doc)
		if req.GatewayIPAddr == nil || req.GatewayIPAddr.IsUnspecified() {
			s.log.Printf("dhcp: unrelayed request from %s refused", req.ClientHWAddr)
			continue
		}
		subnet, circuit := segment(req)
		mac := req.ClientHWAddr.String()
		s.mu.Lock()
		ip, ok := s.leases[mac]
		if !ok {
			// a renewing client keeps its address (ciaddr or option 50), as a real server allows
			if want := req.RequestedIPAddress(); want != nil && !want.IsUnspecified() {
				ip, ok = want.String(), true
			} else if req.ClientIPAddr != nil && !req.ClientIPAddr.IsUnspecified() {
				ip, ok = req.ClientIPAddr.String(), true
			}
			if ok {
				s.leases[mac] = ip
			}
		}
		if !ok {
			base := net.ParseIP(subnet).To4()
			s.pool[subnet]++
			host := 10 + s.pool[subnet]
			ip = fmt.Sprintf("%d.%d.%d.%d", base[0], base[1], base[2], host)
			s.leases[mac] = ip
		}
		s.mu.Unlock()
		var mt dhcpv4.MessageType
		switch req.MessageType() {
		case dhcpv4.MessageTypeDiscover:
			mt = dhcpv4.MessageTypeOffer
		case dhcpv4.MessageTypeRequest:
			mt = dhcpv4.MessageTypeAck
		default:
			continue
		}
		gw := net.ParseIP(subnet).To4()
		gwIP := net.IPv4(gw[0], gw[1], gw[2], 1)
		resp, err := dhcpv4.NewReplyFromRequest(req,
			dhcpv4.WithMessageType(mt), dhcpv4.WithYourIP(net.ParseIP(ip)), dhcpv4.WithServerIP(net.ParseIP("10.90.0.10")),
			dhcpv4.WithNetmask(net.CIDRMask(24, 32)), dhcpv4.WithRouter(gwIP), dhcpv4.WithLeaseTime(3600),
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP("10.90.0.10"))))
		if err != nil {
			continue
		}
		if ri := req.RelayAgentInfo(); ri != nil {
			resp.UpdateOption(dhcpv4.Option{Code: dhcpv4.OptionRelayAgentInformation, Value: ri})
		}
		conn.WriteToUDP(resp.ToBytes(), peer)
		if mt == dhcpv4.MessageTypeAck {
			vc := req.ClassIdentifier()
			s.mu.Lock()
			m := s.machines[mac]
			if m == nil {
				m = &Machine{BMCMAC: mac, State: "Discovered", Leak: "Unknown", Handling: "None"}
				s.machines[mac] = m
				s.log.Printf("dhcp: %s leased %s (circuit %s, vendor class %q)", mac, ip, circuit, vc)
			}
			m.BMCIP, m.Circuit, m.Updated = ip, circuit, time.Now()
			s.mu.Unlock()
		}
	}
}

// segment selects the subnet by RFC 3527 link selection, else by giaddr.
func segment(req *dhcpv4.DHCPv4) (string, string) {
	subnet := req.GatewayIPAddr.String()
	circuit := ""
	if ri := req.RelayAgentInfo(); ri != nil {
		if ls := ri.Get(dhcpv4.LinkSelectionSubOption); len(ls) == 4 {
			subnet = net.IP(ls).String()
		}
		circuit = string(ri.Get(dhcpv4.AgentCircuitIDSubOption))
	}
	ip := net.ParseIP(subnet).To4()
	if ip == nil {
		return "10.30.0.0", circuit
	}
	return fmt.Sprintf("%d.%d.%d.0", ip[0], ip[1], ip[2]), circuit
}

func (s *stack) loadExpected(path string) {
	for path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			var list []expected
			if json.Unmarshal(b, &list) == nil {
				s.mu.Lock()
				for _, e := range list {
					s.expected[strings.ToLower(e.BMCMAC)] = e
				}
				s.mu.Unlock()
				s.log.Printf("expected machines: %d loaded", len(list))
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- Redfish ingestion

func (s *stack) connect(m *Machine) (*gofish.APIClient, error) {
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	if s.gateway != "" {
		p, _ := url.Parse("http://" + s.gateway)
		tr.Proxy = http.ProxyURL(p)
	}
	return gofish.Connect(gofish.ClientConfig{Endpoint: "https://" + m.BMCIP, Username: m.user, Password: m.pw,
		HTTPClient: &http.Client{Transport: tr, Timeout: 10 * time.Second}, BasicAuth: true})
}

// hardwareType mirrors how NICo selects a code path from Redfish identity strings.
func hardwareType(svc *gofish.Service, chassis []*redfish.Chassis) string {
	v := strings.ToLower(svc.Vendor)
	switch {
	case v == "nvidia" && svc.Product == "P3809":
		return "nvswitch-tray"
	case v == "nvidia" && (svc.Product == "GB BMC" || svc.Product == "GB200 NVL"):
		for _, c := range chassis {
			if c.Manufacturer == "NVIDIA" && c.Model == "NVIDIA GB300" {
				return "gb300-tray"
			}
		}
		return "gb200-tray"
	case v == "nvidia":
		return "bluefield"
	case v == "dell":
		return "dell"
	case v == "hpe":
		return "hpe"
	case v == "lenovo":
		return "lenovo"
	case v == "ami":
		return "dgx"
	}
	for _, c := range chassis {
		if strings.EqualFold(c.Manufacturer, "LITE-ON TECHNOLOGY CORP.") {
			return "liteon-powershelf"
		}
	}
	if strings.Contains(string(svc.Oem), "Supermicro") {
		return "supermicro"
	}
	return "unknown"
}

func (s *stack) ingestLoop() {
	for {
		s.mu.Lock()
		var todo []*Machine
		for mac, m := range s.machines {
			if m.State == "Discovered" {
				if e, ok := s.expected[strings.ToLower(mac)]; ok {
					m.user, m.pw, m.Rack = e.User, e.Password, e.Rack
					todo = append(todo, m)
				}
			}
		}
		s.mu.Unlock()
		for _, m := range todo {
			c, err := s.connect(m)
			if err != nil {
				s.log.Printf("ingest %s at %s: %v", m.BMCMAC, m.BMCIP, err)
				continue
			}
			svc := c.Service
			chassis, _ := svc.Chassis()
			systems, _ := svc.Systems()
			// the machine's serial: the system's, else Chassis_0's (GB trays report it there),
			// else the first chassis that has one (power shelves)
			serial := ""
			for _, sy := range systems {
				if serial == "" && sy.SerialNumber != "" && (sy.ID == "System_0" || sy.ID == "Bluefield" || len(systems) == 1) {
					serial = sy.SerialNumber
				}
			}
			for _, ch := range chassis {
				if serial == "" && ch.ID == "Chassis_0" {
					serial = ch.SerialNumber
				}
			}
			for _, ch := range chassis {
				if serial == "" && ch.SerialNumber != "" {
					serial = ch.SerialNumber
				}
			}
			typ := hardwareType(svc, chassis)
			s.mu.Lock()
			m.client, m.Vendor, m.Product, m.Type, m.Serial, m.BMCSerial = c, svc.Vendor, svc.Product, typ, serial, serial
			if len(systems) > 0 {
				m.Model = systems[0].Model
			}
			m.State, m.Updated = "Ready", time.Now()
			s.mu.Unlock()
			s.log.Printf("ingest %s: %s %q/%q -> %s serial %s", m.BMCIP, typ, svc.Vendor, svc.Product, m.State, serial)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- health and the tray leak path

func (s *stack) healthLoop() {
	for {
		s.mu.Lock()
		var ms []*Machine
		for _, m := range s.machines {
			if m.client != nil {
				ms = append(ms, m)
			}
		}
		s.mu.Unlock()
		leaking := map[string]int{}
		for _, m := range ms {
			s.checkMachine(m)
			if m.Leak == "Leaking" {
				leaking[m.Rack]++
			}
		}
		for rack, n := range leaking {
			if n > s.threshold && !s.isolation[rack] {
				s.requestIsolation(rack)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (s *stack) checkMachine(m *Machine) {
	c := m.client
	systems, err := c.Service.Systems()
	if err != nil {
		s.mu.Lock()
		m.Health, m.Updated = "Unreachable", time.Now()
		s.mu.Unlock()
		return
	}
	var sys *redfish.ComputerSystem
	for _, x := range systems {
		if sys == nil || x.ID == "System_0" {
			sys = x
		}
	}
	if sys == nil {
		// power shelves without a Systems resource: nothing to manage here
		return
	}
	leak := "NoLeak"
	if m.Type == "gb300-tray" || m.Type == "gb200-tray" || m.Type == "nvswitch-tray" {
		chassis, _ := c.Service.Chassis()
		for _, ch := range chassis {
			var coll struct {
				Members []struct {
					ID string `json:"@odata.id"`
				}
			}
			if get(c, ch.ODataID+"/ThermalSubsystem/LeakDetection/LeakDetectors", &coll) != nil {
				continue
			}
			for _, mem := range coll.Members {
				var d struct{ DetectorState string }
				if get(c, mem.ID, &d) == nil && d.DetectorState != "" && d.DetectorState != "OK" {
					leak = "Leaking"
				}
			}
		}
	}
	s.mu.Lock()
	m.Power, m.Health, m.Updated = string(sys.PowerState), string(sys.Status.Health), time.Now()
	prevLeak := m.Leak
	m.Leak = leak
	key := "TrayLeak/" + m.Serial
	if leak == "Leaking" {
		if _, ok := s.alerts[key]; !ok {
			s.alerts[key] = &Alert{Kind: "TrayLeak", Rack: m.Rack, Target: m.Serial, PreventAllocations: true,
				Classifications: []string{"TrayLeak", "Hardware", "SensorCritical", "PreventAllocations"}, Since: time.Now().UTC().Format(time.RFC3339)}
		}
		m.State = "LeakDetected"
	} else if prevLeak == "Leaking" {
		delete(s.alerts, key)
		m.State = "Ready"
	}
	shut := leak == "Leaking" && m.Handling == "None" && sys.PowerState == redfish.OnPowerState
	if shut {
		m.Handling = "ShuttingDown"
	}
	s.mu.Unlock()
	if shut {
		// NICo's documented default: shut down the leaking tray, and only it
		s.log.Printf("tray leak on %s (%s): graceful shutdown", m.Serial, m.BMCIP)
		if err := sys.Reset(redfish.GracefulShutdownResetType); err != nil {
			s.mu.Lock()
			m.Handling = "Failed"
			s.mu.Unlock()
		}
	}
	if m.Handling == "ShuttingDown" && sys.PowerState == redfish.OffPowerState {
		s.mu.Lock()
		m.Handling = "Down"
		s.mu.Unlock()
	}
}

func get(c *gofish.APIClient, path string, v any) error {
	resp, err := c.Get(path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// ---------------------------------------------------------------- the BMS leak path

func (s *stack) bmsConsumer(broker string) {
	opts := mqtt.NewClientOptions().AddBroker("tcp://" + broker).SetClientID("refstack-exchange-consumer").SetAutoReconnect(true).
		SetConnectRetry(true).SetConnectRetryInterval(500 * time.Millisecond)
	s.mq = mqtt.NewClient(opts)
	for {
		if t := s.mq.Connect(); t.Wait() && t.Error() == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	// metadata first: it names the rack and the integration value topics (the AsyncAPI contract)
	s.mq.Subscribe("BMS/v1/PUB/Metadata/Rack/#", 1, func(_ mqtt.Client, msg mqtt.Message) {
		var md map[string]any
		if json.Unmarshal(msg.Payload(), &md) != nil {
			return
		}
		rack, _ := md["rackLocationId"].(string)
		pt, _ := md["pointType"].(string)
		if integ, _ := md["integration"].(string); integ == s.integ && rack != "" {
			value := strings.Replace(strings.Replace(msg.Topic(), "/PUB/", "/"+s.integ+"/", 1), "/Metadata/", "/Value/", 1)
			s.mu.Lock()
			if s.rackTags[rack] == nil {
				s.rackTags[rack] = map[string]string{}
			}
			s.rackTags[rack][pt] = value
			s.mu.Unlock()
		}
		s.mu.Lock()
		if rack != "" {
			if s.rackTags["topic:"+tagOf(msg.Topic())] == nil {
				s.rackTags["topic:"+tagOf(msg.Topic())] = map[string]string{}
			}
			s.rackTags["topic:"+tagOf(msg.Topic())]["rack"] = rack
		}
		s.mu.Unlock()
	})
	s.mq.Subscribe("BMS/v1/PUB/Value/Rack/RackLeakDetect/#", 1, func(_ mqtt.Client, msg mqtt.Message) {
		var v struct {
			Value   *float64 `json:"value"`
			Quality int      `json:"quality"`
		}
		if json.Unmarshal(msg.Payload(), &v) != nil || v.Value == nil {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		rack := s.rackTags["topic:"+tagOf(msg.Topic())]["rack"]
		if rack == "" {
			return
		}
		key := "RackLeak/" + rack + "/" + msg.Topic()
		if *v.Value >= 1 {
			if _, ok := s.alerts[key]; !ok {
				// a RackLeak health alert that blocks allocation; nothing is powered off
				s.alerts[key] = &Alert{Kind: "RackLeak", Rack: rack, PreventAllocations: true,
					Classifications: []string{"RackLeak", "Hardware", "SensorCritical", "PreventAllocations"}, Since: time.Now().UTC().Format(time.RFC3339)}
				s.log.Printf("BMS rack leak on %s: RackLeak alert, PreventAllocations", rack)
			}
		} else {
			delete(s.alerts, key)
		}
	})
}

func tagOf(topic string) string {
	parts := strings.Split(topic, "/")
	if len(parts) > 6 {
		return strings.Join(parts[6:], "/")
	}
	return topic
}

func (s *stack) requestIsolation(rack string) {
	s.mu.Lock()
	topic := s.rackTags[rack]["RackElectricalIsolationRequest"]
	s.isolation[rack] = true
	s.mu.Unlock()
	if topic == "" || s.mq == nil {
		s.log.Printf("isolation for %s wanted but the BMS published no request point", rack)
		return
	}
	payload, _ := json.Marshal(map[string]any{"value": 1, "timestamp": time.Now().UnixMilli(), "quality": 1})
	s.mq.Publish(topic, 1, false, payload)
	s.log.Printf("asked the BMS to isolate %s electrically (%s)", rack, topic)
}

// ---------------------------------------------------------------- accessors

func (s *stack) apiMachines(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	var out []*Machine
	for _, m := range s.machines {
		out = append(out, m)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].BMCIP < out[j].BMCIP })
	writeJSON(w, out)
}

func (s *stack) apiMachine(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/machines/")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.machines {
		if m.Serial == id || m.BMCSerial == id || strings.EqualFold(m.BMCMAC, id) {
			writeJSON(w, m)
			return
		}
	}
	http.Error(w, `{}`, http.StatusNotFound)
}

func (s *stack) apiAlerts(w http.ResponseWriter, r *http.Request) {
	rack := r.URL.Query().Get("rack")
	s.mu.Lock()
	out := []*Alert{}
	for _, a := range s.alerts {
		if rack == "" || a.Rack == rack {
			out = append(out, a)
		}
	}
	s.mu.Unlock()
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

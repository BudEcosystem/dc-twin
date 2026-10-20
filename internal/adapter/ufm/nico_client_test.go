package ufm

// A Go reproduction of NICo's UFM client (dsx-ai-factory/infra-controller,
// crates/ib-fabric/src/ib/ufmclient/{mod.rs,rest.rs}, read 2026-10-18): the same base paths,
// headers, request bodies (in serde's field order), status handling, the "{}" not-found quirk,
// and decoding that tolerates unknown fields but refuses a missing or null required field, as
// serde does. It exists because the Rust client cannot be built standalone.

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- the types the client deserializes (mod.rs)

type nicoSmConfig struct {
	SubnetPrefix string `json:"subnet_prefix"`
	MKey         string `json:"m_key"`
	SMKey        string `json:"sm_key"`
	SAKey        string `json:"sa_key"`
	MKeyPerPort  bool   `json:"m_key_per_port"`
}

type nicoQoS struct {
	MTULimit     uint16  `json:"mtu_limit"`
	ServiceLevel uint8   `json:"service_level"`
	RateLimit    float32 `json:"rate_limit"`
}

// nicoMembership is `#[serde(rename_all = "lowercase")] enum PortMembership { Limited, Full }`.
type nicoMembership string

func (m *nicoMembership) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s != "limited" && s != "full" {
		return fmt.Errorf("unknown variant %q, expected `limited` or `full`", s)
	}
	*m = nicoMembership(s)
	return nil
}

type nicoPortConfig struct {
	GUID       string         `json:"guid"`
	Index0     bool           `json:"index0"`
	Membership nicoMembership `json:"membership"`
}

type nicoPort struct {
	GUID          string `json:"guid"`
	Name          string `json:"name"`
	SystemID      string `json:"systemID"`
	LID           int32  `json:"lid"`
	DName         string `json:"dname"`
	SystemName    string `json:"system_name"`
	PhysicalState string `json:"physical_state"`
	LogicalState  string `json:"logical_state"`
}

// nicoPartitionData: `guids` is #[serde(default)]; qos_conf and membership are Option.
type nicoPartitionData struct {
	Partition  string           `json:"partition"`
	IPoverIB   bool             `json:"ip_over_ib"`
	QoSConf    *nicoQoS         `json:"qos_conf"`
	GUIDs      []nicoPortConfig `json:"guids" serde:"default"`
	Membership *nicoMembership  `json:"membership"`
}

type nicoPartition struct {
	Name       string
	PKey       uint16
	IPoIB      bool
	QoS        *nicoQoS
	GUIDs      map[string]bool // Some only when asked for
	Membership *nicoMembership
}

// ---- serde-like decoding

// serdeCheck refuses JSON that serde would refuse for t: a missing field that is neither an
// Option nor #[serde(default)], or a null where the type is not an Option. Unknown fields are
// ignored, as serde does without deny_unknown_fields.
func serdeCheck(raw json.RawMessage, t reflect.Type, where string) error {
	null := strings.TrimSpace(string(raw)) == "null"
	if t.Kind() == reflect.Pointer {
		if null {
			return nil
		}
		return serdeCheck(raw, t.Elem(), where)
	}
	if null {
		return fmt.Errorf("%s: invalid type: null, expected %s", where, t)
	}
	switch t.Kind() {
	case reflect.Struct:
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("%s: expected struct %s: %v", where, t.Name(), err)
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			v, ok := obj[name]
			if !ok {
				if f.Type.Kind() == reflect.Pointer || f.Tag.Get("serde") == "default" {
					continue
				}
				return fmt.Errorf("%s: missing field `%s`", where, name)
			}
			if err := serdeCheck(v, f.Type, where+"."+name); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return fmt.Errorf("%s: expected a sequence: %v", where, err)
		}
		for i, v := range arr {
			if err := serdeCheck(v, t.Elem(), fmt.Sprintf("%s[%d]", where, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return fmt.Errorf("%s: expected a map: %v", where, err)
		}
		for k, v := range obj {
			if err := serdeCheck(v, t.Elem(), where+"["+k+"]"); err != nil {
				return err
			}
		}
	}
	return nil
}

func serdeDecode(body string, v any) error {
	if err := serdeCheck(json.RawMessage(body), reflect.TypeOf(v).Elem(), "$"); err != nil {
		return err
	}
	return json.Unmarshal([]byte(body), v)
}

// ---- rest.rs

type ufmError struct {
	kind   string // HttpError, MalformedResponse, NotFound
	status int
	body   string
}

func (e *ufmError) Error() string { return fmt.Sprintf("%s (status %d): %s", e.kind, e.status, e.body) }

func isNotFound(err error) bool {
	e, ok := err.(*ufmError)
	return ok && e.kind == "NotFound"
}

type nicoClient struct {
	baseURL string
	auth    string
	http    *http.Client
	trace   func(method, url string, status int)
}

// newNicoClient is ufmclient::new_client: a token selects /ufmRestV3 and is sent as
// "Basic <token>"; otherwise /ufmRest with base64(user:password). TLS is not verified
// (DummyTlsVerifier), as NICo's client does without client certificates.
func newNicoClient(address, user, password, token string) (*nicoClient, error) {
	u, err := url.Parse(address)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("invalid UFM url: %s", address)
	}
	base, auth := "/ufmRest", ""
	if token != "" {
		base, auth = "/ufmRestV3", token
	} else {
		if password == "" || user == "" {
			return nil, fmt.Errorf("username or password is empty")
		}
		auth = base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	}
	host := u.Hostname()
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	return &nicoClient{
		baseURL: u.Scheme + "://" + host + "/" + strings.Trim(base, "/"),
		auth:    "Basic " + strings.TrimSpace(auth),
		http: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
	}, nil
}

func (c *nicoClient) execute(method, path, body string) (string, int, error) {
	u := c.baseURL + "/" + strings.Trim(path, "/")
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "carbide-ib-fabric")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.auth)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if c.trace != nil {
		c.trace(method, u, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", resp.StatusCode, &ufmError{kind: "HttpError", status: resp.StatusCode, body: string(b)}
	}
	return string(b), resp.StatusCode, nil
}

// get treats a body of exactly "{}" as not found (UFM answers 200 {} for a missing resource).
func (c *nicoClient) get(path string, v any) error {
	body, status, err := c.execute(http.MethodGet, path, "")
	if err != nil {
		return err
	}
	if body == "{}" {
		return &ufmError{kind: "NotFound", status: status, body: body}
	}
	if err := serdeDecode(body, v); err != nil {
		return &ufmError{kind: "MalformedResponse", status: status, body: err.Error()}
	}
	return nil
}

func (c *nicoClient) list(path string, v any) error {
	body, status, err := c.execute(http.MethodGet, path, "")
	if err != nil {
		return err
	}
	if err := serdeDecode(body, v); err != nil {
		return &ufmError{kind: "MalformedResponse", status: status, body: err.Error()}
	}
	return nil
}

// ---- mod.rs operations

func pkeyDisplay(k uint16) string { return fmt.Sprintf("0x%x", k) }

func parsePKeyNico(s string) (uint16, error) {
	s = strings.ToLower(s)
	base := 10
	if strings.HasPrefix(s, "0x") {
		base = 16
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), base, 16)
	return uint16(n), err
}

func (c *nicoClient) version() (string, error) {
	var v struct {
		UfmReleaseVersion string `json:"ufm_release_version"`
	}
	err := c.get("/app/ufm_version", &v)
	return v.UfmReleaseVersion, err
}

func (c *nicoClient) smConfig() (nicoSmConfig, error) {
	var v nicoSmConfig
	return v, c.get("/app/smconf", &v)
}

func (c *nicoClient) updatePartitionQoS(pkey uint16, q nicoQoS) error {
	// struct PkeyQoS { pkey, mtu_limit, service_level, rate_limit } in serde's field order
	// serde_json writes an f32 with ryu: 200.0, 2.5
	rate := strconv.FormatFloat(float64(q.RateLimit), 'f', -1, 32)
	if !strings.Contains(rate, ".") {
		rate += ".0"
	}
	body := fmt.Sprintf(`{"pkey":%q,"mtu_limit":%d,"service_level":%d,"rate_limit":%s}`, pkeyDisplay(pkey), q.MTULimit, q.ServiceLevel, rate)
	_, _, err := c.execute(http.MethodPut, "/resources/pkeys/qos_conf", body)
	return err
}

// bindPorts posts every GUID with the last port's membership and index0 (NICo binds with
// index0 true and full membership).
func (c *nicoClient) bindPorts(p nicoPartition, ports []nicoPortConfig) error {
	membership, index0 := "full", true
	guids := []string{}
	for _, pc := range ports {
		membership, index0 = string(pc.Membership), pc.Index0
		guids = append(guids, pc.GUID)
	}
	gj, _ := json.Marshal(guids)
	body := fmt.Sprintf(`{"pkey":%q,"ip_over_ib":%t,"membership":%q,"index0":%t,"guids":%s}`, pkeyDisplay(p.PKey), p.IPoIB, membership, index0, gj)
	_, _, err := c.execute(http.MethodPost, "/resources/pkeys", body)
	return err
}

func (c *nicoClient) unbindPorts(pkey uint16, guids []string) error {
	gj, _ := json.Marshal(guids)
	body := fmt.Sprintf(`{"pkey":%q,"guids":%s}`, pkeyDisplay(pkey), gj)
	_, _, err := c.execute(http.MethodPost, "/actions/remove_guids_from_pkey", body)
	return err
}

func toPartition(key uint16, d nicoPartitionData, withGUIDs bool) nicoPartition {
	p := nicoPartition{Name: d.Partition, PKey: key, IPoIB: d.IPoverIB, QoS: d.QoSConf, Membership: d.Membership}
	if withGUIDs {
		p.GUIDs = map[string]bool{}
		for _, g := range d.GUIDs {
			p.GUIDs[g.GUID] = true
		}
	}
	return p
}

func (c *nicoClient) listPartitions(guids, withQoS bool) (map[uint16]nicoPartition, error) {
	var path string
	switch {
	case guids && withQoS:
		return nil, fmt.Errorf("returning qos_conf and guids_data together is not supported by UFM")
	case guids:
		path = "/resources/pkeys?guids_data=true"
	default:
		path = "/resources/pkeys?qos_conf=true"
	}
	var raw map[string]nicoPartitionData
	if err := c.get(path, &raw); err != nil {
		return nil, err
	}
	out := map[uint16]nicoPartition{}
	for k, d := range raw {
		key, err := parsePKeyNico(k)
		if err != nil {
			return nil, fmt.Errorf("invalid pkey %q", k)
		}
		out[key] = toPartition(key, d, guids)
	}
	return out, nil
}

func (c *nicoClient) getPartition(pkey uint16, guids, withQoS bool) (nicoPartition, error) {
	path := "/resources/pkeys/" + pkeyDisplay(pkey)
	sep := "?"
	if guids {
		path += sep + "guids_data=true"
		sep = "&"
	}
	if withQoS {
		path += sep + "qos_conf=true"
	}
	var d nicoPartitionData
	if err := c.get(path, &d); err != nil {
		return nicoPartition{}, err
	}
	return toPartition(pkey, d, guids), nil
}

// listPort is list_port: every Computer port, filtered by a partition's GUIDs, a GUID set and
// a logical state, as filter_ports does.
func (c *nicoClient) listPort(pkey *uint16, guids map[string]bool, logicalState string) ([]nicoPort, error) {
	var ports []nicoPort
	if err := c.list("/resources/ports?sys_type=Computer", &ports); err != nil {
		return nil, err
	}
	var filter map[string]bool
	if pkey != nil {
		p, err := c.getPartition(*pkey, true, false)
		if err != nil {
			return nil, err
		}
		filter = p.GUIDs
	}
	if guids != nil {
		if filter == nil {
			filter = guids
		} else {
			both := map[string]bool{}
			for g := range filter {
				if guids[g] {
					both[g] = true
				}
			}
			filter = both
		}
	}
	var out []nicoPort
	for _, p := range ports {
		if filter != nil && !filter[p.GUID] {
			continue
		}
		if logicalState != "" && strings.TrimSpace(strings.ToLower(p.LogicalState)) != strings.TrimSpace(strings.ToLower(logicalState)) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// ---- lib.rs: what NICo's fabric monitor does each iteration

type fabricCheck struct {
	version            string
	sm                 nicoSmConfig
	insecure           bool
	defaultMembership  string
	portsByState       map[string]int
	partitions         map[uint16]nicoPartition
	computerPortsByGUI map[string]nicoPort
}

// monitorIteration is check_ib_fabric, get_ports_information and get_partition_information.
func (c *nicoClient) monitorIteration() (*fabricCheck, error) {
	fc := &fabricCheck{portsByState: map[string]int{}, partitions: map[uint16]nicoPartition{}, computerPortsByGUI: map[string]nicoPort{}}
	var err error
	if fc.version, err = c.version(); err != nil {
		return nil, fmt.Errorf("versions: %w", err)
	}
	if fc.sm, err = c.smConfig(); err != nil {
		return nil, fmt.Errorf("get_fabric_config: %w", err)
	}
	num := func(s string) uint64 { n, _ := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64); return n }
	fc.insecure = num(fc.sm.MKey) == 0 || num(fc.sm.SMKey) == 1 || num(fc.sm.SAKey) == 1 || !fc.sm.MKeyPerPort
	def, err := c.getPartition(DefaultPKey, true, true)
	if err != nil {
		return nil, fmt.Errorf("default partition: %w", err)
	}
	if def.Membership != nil {
		fc.defaultMembership = string(*def.Membership)
		if *def.Membership == "full" {
			fc.insecure = true
		}
	}
	ports, err := c.listPort(nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("find_ib_port: %w", err)
	}
	for _, p := range ports {
		st := "unknown"
		switch strings.TrimSpace(strings.ToLower(p.LogicalState)) {
		case "active":
			st = "Active"
		case "down":
			st = "Down"
		case "initialize":
			st = "Initialize"
		case "armed":
			st = "Armed"
		}
		fc.portsByState[st]++
		fc.computerPortsByGUI[p.GUID] = p
	}
	parts, err := c.listPartitions(false, true)
	if err != nil {
		return nil, fmt.Errorf("get_ib_networks: %w", err)
	}
	keys := make([]int, 0, len(parts))
	for k := range parts {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	for _, k := range keys {
		p, err := c.getPartition(uint16(k), true, true)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get_ib_network %s: %w", pkeyDisplay(uint16(k)), err)
		}
		fc.partitions[uint16(k)] = p
	}
	return fc, nil
}

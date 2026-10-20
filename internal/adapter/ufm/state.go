package ufm

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultPKey is the management (default) partition every subnet has.
const DefaultPKey uint16 = 0x7fff

// persisted is the adapter's own state for one fabric: what a UFM keeps in its database and
// OpenSM in its guid2lid file. None of it is in the model.
type persisted struct {
	Partitions  map[string]*partition `json:"partitions"` // by pkey, "0x7fff"
	Tokens      []*token              `json:"tokens"`
	LIDs        map[string]int        `json:"lids"` // port GUID (switch: system GUID) -> LID
	NextLID     int                   `json:"nextLid"`
	LastUpdated string                `json:"lastUpdated,omitempty"`
}

type partition struct {
	Name    string             `json:"name"`
	IPoIB   bool               `json:"ipOverIb"`
	Index0  bool               `json:"index0"`
	QoS     qos                `json:"qos"`
	Members map[string]*member `json:"members"` // by normalised GUID
}

type member struct {
	Membership string `json:"membership"`
	Index0     bool   `json:"index0"`
}

// qos is a partition's QoS as UFM renders it (qos_conf).
type qos struct {
	MTULimit     uint16  `json:"mtu_limit"`
	ServiceLevel uint8   `json:"service_level"`
	RateLimit    float64 `json:"rate_limit"`
}

var defaultQoS = qos{MTULimit: 2, ServiceLevel: 0, RateLimit: 2.5}

type token struct {
	AccessToken string `json:"access_token"`
	Revoked     bool   `json:"revoked"`
	IssuedAt    int64  `json:"issued_at"`
	ExpiresIn   int64  `json:"expires_in"`
	Username    string `json:"username"`
	TwinIssued  bool   `json:"twinIssued,omitempty"`
}

// tokenLifetime is UFM's: ten years.
const tokenLifetime = 315360000

func pkeyString(k uint16) string { return fmt.Sprintf("0x%x", k) }

// parsePKey reads a pkey as UFM and NICo spell it: 0x-prefixed hex, else decimal; at most 0x7fff.
func parsePKey(v any) (uint16, error) {
	var s string
	switch x := v.(type) {
	case string:
		s = strings.ToLower(strings.TrimSpace(x))
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		s = x.String()
	default:
		return 0, fmt.Errorf("pkey is required")
	}
	var n uint64
	var err error
	if h, ok := strings.CutPrefix(s, "0x"); ok {
		n, err = strconv.ParseUint(h, 16, 16)
	} else {
		n, err = strconv.ParseUint(s, 10, 16)
	}
	if err != nil || n > uint64(DefaultPKey) {
		return 0, fmt.Errorf("invalid pkey %q: a hex (0x) or decimal value up to 0x7fff", s)
	}
	return uint16(n), nil
}

// normGUID lower-cases a GUID and drops a 0x prefix: UFM spells GUIDs as bare hex.
func normGUID(g string) string {
	g = strings.ToLower(strings.TrimSpace(g))
	return strings.TrimPrefix(g, "0x")
}

func validGUID(g string) bool {
	if len(g) < 16 || len(g) > 20 {
		return false
	}
	for _, c := range g {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// rateLimits are the values UFM accepts for a partition's rate limit (2.5 is SDR).
var rateLimits = map[float64]bool{2.5: true, 5: true, 10: true, 14: true, 20: true, 25: true, 30: true, 40: true,
	56: true, 60: true, 80: true, 100: true, 112: true, 120: true, 168: true, 200: true, 300: true}

func (q qos) validate() error {
	if q.MTULimit != 2 && q.MTULimit != 4 {
		return fmt.Errorf("mtu_limit %d: must be 2 (2k) or 4 (4k)", q.MTULimit)
	}
	if q.ServiceLevel > 15 {
		return fmt.Errorf("service_level %d: must be 0-15", q.ServiceLevel)
	}
	if !rateLimits[q.RateLimit] {
		return fmt.Errorf("rate_limit %v: not a rate UFM supports", q.RateLimit)
	}
	return nil
}

func (s *server) load() error {
	st := &persisted{}
	b, err := os.ReadFile(s.statePath)
	switch {
	case s.statePath == "":
	case err == nil:
		if err := json.Unmarshal(b, st); err != nil {
			return fmt.Errorf("state %s: %w", s.statePath, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if st.Partitions == nil {
		st.Partitions = map[string]*partition{}
	}
	if st.LIDs == nil {
		st.LIDs = map[string]int{}
	}
	if st.NextLID < 1 {
		st.NextLID = 1
	}
	if st.Partitions[pkeyString(DefaultPKey)] == nil {
		st.Partitions[pkeyString(DefaultPKey)] = &partition{Name: "management", IPoIB: true, QoS: defaultQoS, Members: map[string]*member{}}
	}
	for _, p := range st.Partitions {
		if p.Members == nil {
			p.Members = map[string]*member{}
		}
	}
	s.st = st
	return nil
}

// saveLocked writes the state atomically; callers hold s.mu.
func (s *server) saveLocked() error {
	if s.statePath == "" || s.st == nil {
		return nil
	}
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.statePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath)
}

// touchLocked records a partition change (GET /resources/pkeys/last_updated) and persists.
func (s *server) touchLocked() {
	s.st.LastUpdated = time.Now().UTC().Format("Mon Jan _2 15:04:05 UTC 2006")
	_ = s.saveLocked()
}

func newToken() string {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alnum))))
		b.WriteByte(alnum[n.Int64()])
	}
	return b.String()
}

// ensureToken adds a token unless it is known (a revoked one stays revoked); callers hold s.mu.
func (s *server) ensureToken(tok, user string, twinIssued bool) *token {
	for _, t := range s.st.Tokens {
		if t.AccessToken == tok {
			return t
		}
	}
	t := &token{AccessToken: tok, IssuedAt: time.Now().Unix(), ExpiresIn: tokenLifetime, Username: user, TwinIssued: twinIssued}
	s.st.Tokens = append(s.st.Tokens, t)
	return t
}

// tokenUser returns the user a live token belongs to.
func (s *server) tokenUser(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.st.Tokens {
		if t.AccessToken == tok && !t.Revoked && time.Now().Unix() < t.IssuedAt+t.ExpiresIn {
			return t.Username, true
		}
	}
	return "", false
}

// lids assigns LIDs to keys not yet seen, in the order given (the topology's stable order),
// as OpenSM keeps guid2lid; it returns the table.
func (s *server) lids(keys []string) map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	out := make(map[string]int, len(keys))
	for _, k := range keys {
		l, ok := s.st.LIDs[k]
		if !ok {
			l = s.st.NextLID
			s.st.LIDs[k] = l
			s.st.NextLID++
			changed = true
		}
		out[k] = l
	}
	if changed {
		_ = s.saveLocked()
	}
	return out
}

// ---- partition data as UFM renders it

type portConfig struct {
	GUID       string `json:"guid"`
	Index0     bool   `json:"index0"`
	Membership string `json:"membership"`
}

type partitionData struct {
	GUIDs      *[]portConfig `json:"guids,omitempty"`
	Index0     *bool         `json:"index0,omitempty"`
	IPoverIB   bool          `json:"ip_over_ib"`
	Membership *string       `json:"membership,omitempty"`
	Partition  string        `json:"partition"`
	QoSConf    *qos          `json:"qos_conf,omitempty"`
}

// render renders a partition; callers hold s.mu. The default partition reports its default
// membership (limited unless the SM is configured insecure) when GUID data is asked for and
// the reported release is 6.19 or later, which is when UFM added it.
func (s *server) render(key uint16, p *partition, guids, withQoS bool) partitionData {
	out := partitionData{Partition: p.Name, IPoverIB: p.IPoIB}
	if withQoS {
		q := p.QoS
		out.QoSConf = &q
	}
	if guids {
		list := make([]portConfig, 0, len(p.Members))
		for g, m := range p.Members {
			list = append(list, portConfig{GUID: g, Index0: m.Index0, Membership: m.Membership})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].GUID < list[j].GUID })
		out.GUIDs = &list
		if key == DefaultPKey && versionAtLeast(s.a.cfg.Version, 6, 19) {
			m := "limited"
			if s.a.cfg.InsecureSM {
				m = "full"
			}
			f := false
			out.Membership, out.Index0 = &m, &f
		}
	}
	return out
}

func versionAtLeast(v string, major, minor int) bool {
	parts := strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '-' })
	if len(parts) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return ma > major || (ma == major && mi >= minor)
}

//go:build linux

package linuxnet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/BudEcosystem/dc-twin/internal/day0"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
)

// NVIDIA Config Manager's ZTP contract, the switch's side (18 — DSX OS: NVIDIA Config Manager's
// ZTP, Z7a, D80), before NVCM itself runs (compat/nvcm, Z7c): factory Cumulus switches run NVCM
// 1.4.1's own boot script (its golden, internal/nos/nvue/testdata/nvcm-1.4.1, copied beside the test by
// scripts/linux-substrate-e2e.sh, with this site's
// server, device ids and intended release put in, as NVCM's renderer puts them) against a stand-in
// for NVCM's ZTP service that keeps NVCM's contract:
//   - Kea hands each switch, by its management MAC, option 239 naming its own boot script
//     (/v1/device/<id>/boot-script), as NVCM's DHCP service reserves it;
//   - every device endpoint answers only the device's own address (NVCM's _authorize_request);
//   - validate_serial compares the serial the switch reads from `nv show platform`;
//   - the startup.yaml is fetched over SFTP as ztp:ztp on 2222 (curl before 5.16, nv action
//     fetch from 5.16), the firmware over HTTP (onie-install -i);
//   - provisioned records the switch done.
// The startup.yaml is the twin's own plan for the switch in NVCM's form: eth0 static at its
// reservation in vrf mgmt with its default route there, NVCM's users with hashed passwords, a
// numbered eBGP underlay, enable: on throughout (as NVCM's configurations write it in every
// release, Q30).

// nvcmGolden is the NVCM golden boot script's release whose form a release's script takes.
func nvcmGolden(release string) string {
	if release >= "5.16" {
		return "5.16.1"
	}
	return "5.14.0"
}

type nvcmDev struct {
	id, serial, mac, host string
}

// nvcmStub is the stand-in for NVCM's ZTP service, on the tooling's probe.
type nvcmStub struct {
	r       *day0Run
	intend  string // the release the boot scripts want (VERSION_ID)
	devs    map[string]*nvcmDev
	sftpDir string

	mu sync.Mutex
	// per device id: what it asked for
	scripts, validated, startups, firmware, provisioned map[string]int
	// badSerial: the next validate_serial of these devices is refused (a serial the record does
	// not have, until someone fixes the record)
	badSerial map[string]bool
	// faults: its boot script answered 500 once (the server failing); its startup missing from
	// the SFTP server, or one NVUE refuses (a path it does not have), for its first attempt
	serverError, noStartup, badStartup map[string]bool
	refused                            []string // requests from an address that is not the device's
}

func deviceID(serial string) string {
	h := sha256.Sum256([]byte("nvcm/" + serial))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// startNVCMStub reserves each factory switch's option 239 in Kea, serves the device endpoints
// over HTTP and the startup files over SFTP. Kea is reconfigured while the switches are still
// booting (the case's boot time is long enough for it).
func startNVCMStub(t *testing.T, r *day0Run, intend string) *nvcmStub {
	t.Helper()
	s := &nvcmStub{r: r, intend: intend, devs: map[string]*nvcmDev{}, sftpDir: t.TempDir(),
		scripts: map[string]int{}, validated: map[string]int{}, startups: map[string]int{}, firmware: map[string]int{},
		provisioned: map[string]int{}, badSerial: map[string]bool{}, serverError: map[string]bool{}, noStartup: map[string]bool{},
		badStartup: map[string]bool{}}
	var hosts []any
	for _, e := range r.factory() {
		d := &nvcmDev{id: deviceID(e.Serial), serial: e.Serial, mac: strings.ToLower(e.MgmtMAC), host: e.Hostname}
		s.devs[d.id] = d
		hosts = append(hosts, map[string]any{"hw-address": d.mac, "option-data": []any{
			map[string]any{"name": "cumulus-provision-url", "data": fmt.Sprintf("http://%s:%d/v1/device/%s/boot-script", day0Server, day0Port, d.id)}}})
	}
	// NVCM's Kea: the reservations by MAC carry option 239; no class hands every switch one URL
	cfg := nvcmKeaConfig(hosts)
	b, _ := json.Marshal(cfg)
	if out, code, err := r.d.Exec(context.Background(), r.kea.ID, "sh", "-c", "printf '%s' \"$1\" > /etc/kea/kea-dhcp4.json && kill -HUP 1", "sh", string(b)); err != nil || code != 0 {
		t.Fatalf("Kea's reservations: %v %d %s", err, code, out)
	}
	r.mux.HandleFunc("/v1/device/", s.serveDevice)
	s.serveSFTP(t)
	return s
}

func nvcmKeaConfig(hosts []any) map[string]any {
	var subnets []any
	for i := 0; i < 4; i++ {
		subnets = append(subnets, map[string]any{"id": i + 1, "subnet": fmt.Sprintf("10.30.%d.0/24", i),
			"pools":       []any{map[string]any{"pool": fmt.Sprintf("10.30.%d.10 - 10.30.%d.250", i, i)}},
			"option-data": []any{map[string]any{"name": "routers", "data": fmt.Sprintf("10.30.%d.1", i)}}})
	}
	return map[string]any{"Dhcp4": map[string]any{
		"interfaces-config":      map[string]any{"interfaces": []any{"svc0"}, "dhcp-socket-type": "udp"},
		"lease-database":         map[string]any{"type": "memfile", "persist": true, "name": "/var/lib/kea/kea-leases4.csv", "lfc-interval": 0},
		"valid-lifetime":         3600,
		"option-def":             []any{map[string]any{"name": "cumulus-provision-url", "code": 239, "type": "string", "space": "dhcp4"}},
		"reservations-global":    true,
		"reservations-in-subnet": true,
		"reservations":           hosts,
		"subnet4":                subnets,
		"loggers":                []any{map[string]any{"name": "kea-dhcp4", "severity": "INFO", "output-options": []any{map[string]any{"output": "/var/log/kea-dhcp4.log"}}}},
	}}
}

// from is the device a request is for, when it comes from the device's own address (its lease in
// Kea); else it is refused, as NVCM refuses an anonymous request from anywhere else.
func (s *nvcmStub) from(w http.ResponseWriter, req *http.Request, id string) *nvcmDev {
	d := s.devs[id]
	if d == nil {
		http.Error(w, "no such device", http.StatusNotFound)
		return nil
	}
	host, _, _ := net.SplitHostPort(req.RemoteAddr)
	if s.r.leases()[d.mac] != host {
		s.mu.Lock()
		s.refused = append(s.refused, fmt.Sprintf("%s %s from %s", req.Method, req.URL.Path, host))
		s.mu.Unlock()
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil
	}
	return d
}

func (s *nvcmStub) serveDevice(w http.ResponseWriter, req *http.Request) {
	rest := strings.TrimPrefix(req.URL.Path, "/v1/device/")
	id, what, _ := strings.Cut(rest, "/")
	d := s.from(w, req, id)
	if d == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case what == "boot-script" && req.Method == http.MethodGet:
		s.scripts[id]++
		if s.serverError[id] {
			delete(s.serverError, id)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		b, err := os.ReadFile(filepath.Join("testdata", "nvcm-1.4.1", "a09-u36-p01-spine-01", nvcmGolden(s.intend), "boot-script"))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		script := strings.NewReplacer(
			"VERSION_ID="+nvcmGolden(s.intend), "VERSION_ID="+s.intend,
			"http://192.0.2.10/", fmt.Sprintf("http://%s:%d/", day0Server, day0Port),
			"192.0.2.10:2222", day0Server+":2222",
			"faf21128-fc9d-4bf3-8bce-a293c1c27b05", id).Replace(string(b))
		_, _ = io.WriteString(w, script)
	case what == "validate_serial" && req.Method == http.MethodPost:
		var body struct {
			Serial string `json:"serial"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		s.validated[id]++
		if s.badSerial[id] {
			delete(s.badSerial, id)
			http.Error(w, `{"detail":"serial does not match"}`, 400)
			return
		}
		// NVCM: a case-insensitive suffix match (a part number may lead the serial)
		if !strings.HasSuffix(strings.ToLower(body.Serial), strings.ToLower(d.serial)) || body.Serial == "" {
			http.Error(w, `{"detail":"serial does not match"}`, 400)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	case what == "provisioned" && req.Method == http.MethodPost:
		s.provisioned[id]++
		_, _ = io.WriteString(w, `{"status":"Provisioned"}`)
	case what == "firmware" && req.Method == http.MethodGet:
		s.firmware[id]++
		_, _ = w.Write(s.r.installer)
	default:
		http.NotFound(w, req)
	}
}

// startup writes a switch's startup.yaml where the SFTP server serves it: the plan for its serial,
// as NVCM renders a configuration, in its release's form.
func (s *nvcmStub) startup(t *testing.T, d *nvcmDev, release string) {
	t.Helper()
	p, ok := s.r.planOf(d.serial)
	if !ok {
		t.Fatalf("%s: no plan for serial %s", d.host, d.serial)
	}
	lease := s.r.leases()[d.mac]
	if lease == "" {
		t.Fatalf("%s: no lease", d.host)
	}
	gw := lease[:strings.LastIndex(lease, ".")] + ".1"
	ipKey := "ip"
	if release >= "5.15" {
		ipKey = "ipv4"
	}
	addr := func(a string) map[string]any {
		return map[string]any{ipKey: map[string]any{"address": map[string]any{a: map[string]any{}}}}
	}
	ifs := map[string]any{
		"eth0": merge(addr(lease+"/24"), map[string]any{"type": "eth"}),
		"lo":   merge(addr(p.Loopback), map[string]any{"type": "loopback"}),
	}
	nbr := map[string]any{}
	for _, it := range p.Interfaces {
		if it.Kind != "fabric" {
			continue
		}
		ifs[it.Name] = merge(addr(it.Address), map[string]any{"type": "swp", "link": map[string]any{"state": map[string]any{"up": map[string]any{}}}})
		nbr[it.PeerAddr] = map[string]any{"remote-as": it.PeerASN, "type": "numbered"}
	}
	auto := map[string]any{"enable": "on"}
	if release >= "5.15" {
		auto = map[string]any{"state": "enabled"}
	}
	tree := map[string]any{
		"system": map[string]any{"hostname": p.Hostname, "config": map[string]any{"auto-save": auto},
			"aaa": map[string]any{"user": map[string]any{
				"cumulus":           map[string]any{"hashed-password": nvue.HashPassword(newPassword), "role": "system-admin", "full-name": "cumulus,,,"},
				"nv-config-manager": map[string]any{"hashed-password": nvue.HashPassword(newPassword), "role": "system-admin"},
			}}},
		"interface": ifs,
		"router":    map[string]any{"bgp": map[string]any{"enable": "on", "autonomous-system": p.ASN, "router-id": p.RouterID}},
		"vrf": map[string]any{
			"default": map[string]any{"router": map[string]any{"bgp": map[string]any{"enable": "on", "neighbor": nbr,
				"address-family": map[string]any{"ipv4-unicast": map[string]any{"enable": "on", "redistribute": map[string]any{"connected": map[string]any{"enable": "on"}}}}}}},
			"mgmt": map[string]any{"router": map[string]any{"static": map[string]any{"0.0.0.0/0": map[string]any{
				"address-family": "ipv4-unicast", "via": map[string]any{gw: map[string]any{"type": "ipv4-address"}}}}}},
		},
	}
	b, err := nvue.StartupYAML(tree, release)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.sftpDir, "device", d.id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "startup.yaml"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func merge(a, b map[string]any) map[string]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}

// writeStartups writes every switch's startup.yaml once it has its lease (its address is in it).
func (s *nvcmStub) writeStartups(t *testing.T, release string) {
	t.Helper()
	waitFor(t, 5*time.Minute, "every factory switch's management lease in Kea", func() bool {
		l := s.r.leases()
		for _, d := range s.devs {
			if l[d.mac] == "" {
				return false
			}
		}
		return true
	})
	for _, d := range s.devs {
		s.startup(t, d, release)
	}
}

// serveSFTP is NVCM's SFTP server's part: as ztp with any password, read-only, the files under
// /device/<id>/, to the device's own address.
func (s *nvcmStub) serveSFTP(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, _ []byte) (*ssh.Permissions, error) {
		if c.User() != "ztp" {
			return nil, fmt.Errorf("no user %s", c.User())
		}
		host, _, _ := net.SplitHostPort(c.RemoteAddr().String())
		return &ssh.Permissions{Extensions: map[string]string{"addr": host}}, nil
	}}
	cfg.AddHostKey(signer)
	go func() {
		var l net.Listener
		var err error
		for i := 0; i < 3600; i++ {
			if l, err = s.r.p.listen(day0Server + ":2222"); err == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if err != nil {
			t.Errorf("NVCM's SFTP server: %v", err)
			return
		}
		t.Cleanup(func() { l.Close() })
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go s.sftpConn(c, cfg)
		}
	}()
}

func (s *nvcmStub) sftpConn(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	addr := sc.Permissions.Extensions["addr"]
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, in, err := nc.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range in {
				ok := req.Type == "subsystem" && len(req.Payload) > 4 && string(req.Payload[4:]) == "sftp"
				_ = req.Reply(ok, nil)
				if ok {
					h := sftpFiles{s: s, addr: addr}
					srv := sftp.NewRequestServer(ch, sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h})
					_ = srv.Serve()
					srv.Close()
					return
				}
			}
		}()
	}
}

// sftpFiles serves NVCM's /device/<id>/<file>, read-only, to the device's own address.
type sftpFiles struct {
	s    *nvcmStub
	addr string
}

func (h sftpFiles) allowed(p string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(filepath.Clean(p), "/"), "/")
	if len(parts) != 3 || parts[0] != "device" {
		return "", os.ErrNotExist
	}
	d := h.s.devs[parts[1]]
	if d == nil {
		return "", os.ErrNotExist
	}
	if h.s.r.leases()[d.mac] != h.addr {
		h.s.mu.Lock()
		h.s.refused = append(h.s.refused, "sftp "+p+" from "+h.addr)
		h.s.mu.Unlock()
		return "", os.ErrPermission
	}
	return filepath.Join(h.s.sftpDir, filepath.Clean(p)), nil
}

func (h sftpFiles) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	p, err := h.allowed(r.Filepath)
	if err != nil {
		return nil, err
	}
	id := strings.Split(strings.TrimPrefix(r.Filepath, "/"), "/")[1]
	// a startup fault lasts the switch's first attempt (its first validate_serial): the boot
	// script's own retries of the fetch see it too, so the attempt fails as a whole
	h.s.mu.Lock()
	first := h.s.validated[id] <= 1
	missing, bad := h.s.noStartup[id] && first, h.s.badStartup[id] && first
	h.s.mu.Unlock()
	if missing {
		return nil, os.ErrNotExist
	}
	if bad {
		// a configuration with a path the switch's NVUE does not have: nv config replace refuses it
		return strings.NewReader("- set:\n    system:\n      hostname: refused\n      no-such-path: 1\n"), nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	h.s.mu.Lock()
	h.s.startups[strings.Split(strings.TrimPrefix(r.Filepath, "/"), "/")[1]]++
	h.s.mu.Unlock()
	return f, nil
}

func (h sftpFiles) Filewrite(*sftp.Request) (io.WriterAt, error) { return nil, os.ErrPermission }
func (h sftpFiles) Filecmd(*sftp.Request) error                  { return os.ErrPermission }

type listerAt []os.FileInfo

func (l listerAt) ListAt(out []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(out, l[off:])
	return n, nil
}

func (h sftpFiles) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	p, err := h.allowed(r.Filepath)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	return listerAt{st}, nil
}

// count is a device counter's value.
func (s *nvcmStub) count(m map[string]int, id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return m[id]
}

// checkProvisioned is what NVCM's contract says each switch did once, at least: its script
// fetched, its serial validated, its startup fetched over SFTP, provisioned posted; and its
// breakglass user gone, its ZTP successful, nothing fetched from the twin's own ZTP script.
func (s *nvcmStub) checkProvisioned(t *testing.T) {
	t.Helper()
	for _, d := range s.devs {
		for what, m := range map[string]map[string]int{"boot-script": s.scripts, "validate_serial": s.validated, "startup.yaml": s.startups, "provisioned": s.provisioned} {
			if s.count(m, d.id) == 0 {
				t.Errorf("%s never asked for %s", d.host, what)
			}
		}
		path := ""
		for _, e := range s.r.factory() {
			if e.Serial == d.serial {
				path = e.Path
			}
		}
		out, err := s.r.sub.Exec(context.Background(), path, "sh", "-c", "id breakglass 2>&1; id nv-config-manager; ztp -s; grep VERSION_ID /etc/os-release")
		if err != nil || !strings.Contains(out, "no such user") || !strings.Contains(out, "nv-config-manager") || !strings.Contains(strings.ToLower(out), "success") ||
			!strings.Contains(out, "VERSION_ID="+s.intend) {
			t.Errorf("%s after its ZTP (%v):\n%s", d.host, err, indent(out))
		}
	}
	if n := len(s.r.fetched("/ztp/cumulus.sh")); n > 0 {
		t.Errorf("%d fetches of the twin's own ZTP script: a switch leased before NVCM's reservations", n)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.refused) > 0 {
		t.Errorf("requests refused for their address: %v", s.refused)
	}
}

func nvcmCase(name, release, boot string) day0Case {
	return day0Case{estate: "linuxnet-day0-" + name, nos: nos.NVUEFRR, install: "preinstalled", ztp: "enabled", cumulus: release, boot: boot}
}

// The contract at 5.14.0, the release the DSX day-0 estate runs NVCM's templates at (the curl
// path for the startup file), with its edge cases: a serial the record does not have (the script
// fails, Cumulus's ZTP retries, the record fixed it passes); a request from another address
// refused; and NVCM's reprovision (POST system/factory-default @reset) taking a switch through
// ZTP again.
func TestDay0NVCMContract(t *testing.T) {
	r := startDay0(t, nvcmCase("nvcm-contract", "5.14.0", "40m"))
	s := startNVCMStub(t, r, "5.14.0")
	var first *nvcmDev
	for _, d := range s.devs {
		first = d
		break
	}
	s.mu.Lock()
	s.badSerial[first.id] = true
	s.mu.Unlock()
	s.writeStartups(t, "5.14.0")
	took := r.waitConverged(15 * time.Minute)
	t.Logf("every factory switch converged %s after up, through NVCM's boot script", took.Round(time.Second))
	s.checkProvisioned(t)
	if n := s.count(s.validated, first.id); n < 2 {
		t.Errorf("%s validated its serial %d times: the refused one was not retried", first.host, n)
	}
	// another switch is refused this one's script: NVCM answers a device's own address only
	var other string
	for _, e := range r.factory() {
		if e.Serial != first.serial {
			other = e.Path
			break
		}
	}
	out, err := r.sub.Exec(context.Background(), other, "dctwin-vrf-exec", "mgmt", "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		fmt.Sprintf("http://%s:%d/v1/device/%s/boot-script", day0Server, day0Port, first.id))
	if strings.TrimSpace(out) != "401" {
		t.Errorf("another switch's request for %s's script: %q %v", first.host, out, err)
	}
	s.mu.Lock()
	s.refused = nil
	s.mu.Unlock()
	r.checkPhases()

	// over SSH, as NVCM's (and a day-0 tool's) workflows read a switch: its EEPROM, its clock, its
	// release, and an apply with commit-confirm, confirmed
	ip0 := r.leases()[first.mac]
	cl, err := r.sshAs(ip0, nvue.User, newPassword)
	if err != nil {
		t.Fatalf("%s: SSH as %s: %v", first.host, nvue.User, err)
	}
	for _, c := range []struct{ cmd, want string }{
		{"echo '" + newPassword + "' | sudo -S -p '' decode-syseeprom -s", first.serial},
		{"timedatectl show -p NTPSynchronized", "NTPSynchronized="},
		{". /etc/lsb-release; echo $DISTRIB_RELEASE", "5.14.0"},
		{"nv set system message pre-login 'day 2' && nv config apply --confirm 300 -y", "confirm within 300 s"},
		{"nv config confirm", "confirmed"},
	} {
		sess, err := cl.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		out, err := sess.CombinedOutput(c.cmd)
		sess.Close()
		if err != nil || !strings.Contains(string(out), c.want) {
			t.Errorf("%s: %s: %v\n%s", first.host, c.cmd, err, out)
		}
	}
	cl.Close()

	// NVCM's reprovision, over NVUE as NVCM's client sends it
	ip := r.leases()[first.mac]
	before := s.count(s.provisioned, first.id)
	req, _ := http.NewRequest("POST", fmt.Sprintf("https://%s:%d/nvue_v1/system/factory-default", ip, nvue.APIPort),
		strings.NewReader(`{"@reset": {"state": "start", "parameters": {"force": true}}}`))
	req.SetBasicAuth(nvue.User, newPassword)
	res, err := r.httpClient().Do(req)
	if err != nil {
		t.Fatalf("@reset: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("@reset: %d", res.StatusCode)
	}
	waitFor(t, 10*time.Minute, first.host+" provisioned again after NVCM's reprovision", func() bool {
		return s.count(s.provisioned, first.id) > before
	})
	r.waitConverged(10 * time.Minute)
	s.checkProvisioned(t)
	r.judge()
}

// The firmware path: the switches run 5.13.1 and NVCM wants 5.14.0. Its boot script calls
// onie-install -fa -i …/firmware and reboot; the twin boots each into ONIE, which installs NVCM's
// firmware (the image the estate names), the NOS comes back at 5.14.0 in factory state, its ZTP
// runs NVCM's script again, and the release matches now.
func TestDay0NVCMFirmware(t *testing.T) {
	c := nvcmCase("nvcm-firmware", "5.13.1", "40m")
	c.images = fmt.Sprintf(`[{ match: { name: "^firmware$" }, nos: %s, version: "5.14.0" }]`, nos.NVUEFRR)
	r := startDay0(t, c)
	s := startNVCMStub(t, r, "5.14.0")
	s.writeStartups(t, "5.14.0")
	took := r.waitConverged(20 * time.Minute)
	t.Logf("every factory switch reinstalled at 5.14.0 and converged %s after up", took.Round(time.Second))
	s.checkProvisioned(t)
	for _, d := range s.devs {
		if s.count(s.firmware, d.id) == 0 || s.count(s.scripts, d.id) < 2 {
			t.Errorf("%s: firmware fetched %d times, boot script %d times", d.host, s.count(s.firmware, d.id), s.count(s.scripts, d.id))
		}
	}
	for _, e := range r.factory() {
		ip := r.leases()[strings.ToLower(e.MgmtMAC)]
		req, _ := http.NewRequest("GET", fmt.Sprintf("https://%s:%d/nvue_v1/system", ip, nvue.APIPort), nil)
		req.SetBasicAuth(nvue.User, newPassword)
		res, err := r.httpClient().Do(req)
		if err != nil {
			t.Errorf("%s: GET /system: %v", e.Hostname, err)
			continue
		}
		var sys map[string]any
		_ = json.NewDecoder(res.Body).Decode(&sys)
		res.Body.Close()
		if sys["product-release"] != "5.14.0" {
			t.Errorf("%s: NVUE reports %v after the reinstall", e.Hostname, sys["product-release"])
		}
	}
	r.judge()
}

// 5.16.1: the startup file fetched with nv action fetch (sftp, vrf mgmt), and NVCM's enable: on
// in a 5.16 file (Q30).
func TestDay0NVCM516(t *testing.T) {
	r := startDay0(t, nvcmCase("nvcm-516", "5.16.1", "40m"))
	s := startNVCMStub(t, r, "5.16.1")
	s.writeStartups(t, "5.16.1")
	took := r.waitConverged(15 * time.Minute)
	t.Logf("every factory 5.16.1 switch converged %s after up", took.Round(time.Second))
	s.checkProvisioned(t)
	r.judge()
}

// NVCM's ZTP when it fails, once per switch: the server answering 500 for the boot script, the
// startup missing from the SFTP server, a startup the switch's NVUE refuses by name. Each fails
// the switch's attempt (the boot script's own retries give up, its ERR trap exits 1), Cumulus's
// ZTP tries again, and the fabric converges; the refused path is reported against the twin's
// schema as a refusal (network.config.change, 400), not taken.
func TestDay0NVCMFaults(t *testing.T) {
	r := startDay0(t, nvcmCase("nvcm-faults", "5.14.0", "40m"))
	s := startNVCMStub(t, r, "5.14.0")
	var ids []string
	for id := range s.devs {
		ids = append(ids, id)
	}
	s.mu.Lock()
	s.serverError[ids[0]] = true
	s.noStartup[ids[1]] = true
	s.badStartup[ids[2]] = true
	s.mu.Unlock()
	s.writeStartups(t, "5.14.0")
	took := r.waitConverged(20 * time.Minute)
	t.Logf("every factory switch converged %s after up, each fault retried past", took.Round(time.Second))
	s.checkProvisioned(t)
	for i, id := range ids[:3] {
		d := s.devs[id]
		// every attempt fetches its script and validates its serial before its startup
		n := map[int]int{0: s.count(s.scripts, id), 1: s.count(s.validated, id), 2: s.count(s.validated, id)}[i]
		if n < 2 {
			t.Errorf("%s: fault %d retried %d times", d.host, i, n)
		}
	}
	r.judge()
}

var _ = day0.PhaseConverged

package bluefield_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"hash/crc32"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/bluefield"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/redfish"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

// One rack of one Grace Hopper server with a BlueField-3, at a hundredth of real time.
const estate = `apiVersion: dc-twin/v1alpha1
kind: Estate
metadata: { name: bluefield-front-test }
spec:
  seed: 31
  clock: wall
  timeScale: 0.01
  packs: [generic@0.1.0, nvidia@0.1.0]
  sites:
    - name: site-1
      halls:
        - name: hall-a
          rows:
            - name: a
              positions: 1
              racks:
                - { name: w01, position: 1, format: generic/cpu-rack, params: { server: nvidia/gh200-server, servers: 1 } }
  networks:
    oob: { role: management, dhcp: twin, cidr: 10.30.0.0/20, fabric: oob }
  fabrics:
    - { name: oob, template: oob-flat, nos: none }
  facility:
    power: { feeds: [{ name: A, capacityW: 60000 }] }
  stacks: []
`

const dpuPath = "site-1/hall-a/w01/server-01/bf3-0"

type env struct {
	tw   *twin.Twin
	a    *bluefield.Adapter
	dev  *model.Device
	addr string
	ca   *x509.Certificate
	key  *ecdsa.PrivateKey
}

func up(t *testing.T) *env {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := twin.AssetPath(reg, "libredfish/nvidia_dpu"); err != nil {
		t.Skipf("captured BlueField tree not present (dctwin assets fetch): %v", err)
	}
	if _, err := twin.AssetPath(reg, "libredfish/nvidia_gh200"); err != nil {
		t.Skipf("captured GH200 tree not present (dctwin assets fetch): %v", err)
	}
	f := filepath.Join(t.TempDir(), "estate.yaml")
	if err := os.WriteFile(f, []byte(estate), 0o644); err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate(f)
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, "", reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{"redfish", "bluefield"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	a := tw.Adapter("bluefield").(*bluefield.Adapter)
	addr, ok := a.Front(dpuPath)
	if !ok {
		t.Fatalf("no front for %s", dpuPath)
	}
	e := &env{tw: tw, a: a, dev: tw.Engine.M.DeviceBy[dpuPath], addr: addr}
	e.ca, e.key = newCA(t, "stack CA")
	// the front takes the BMC's host-name route over from the redfish adapter
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if tw.Net.Routes()["w01-server-01-bf3-0-bmc:443"] == addr {
			return e
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the front did not take the BMC's route: %v", tw.Net.Routes())
	return nil
}

func newCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c, key
}

func sign(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, tmpl *x509.Certificate, pub any) *x509.Certificate {
	t.Helper()
	tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, pub, caKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return c
}

func pemOf(c *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
}

// do sends a request to the BMC with basic auth (user != "") or a client certificate.
func (e *env) do(t *testing.T, method, path, user, pass string, body any, cert *tls.Certificate) (int, map[string]any) {
	t.Helper()
	cfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the test checks the served certificate itself
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	var rd io.Reader
	if b, ok := body.([]byte); ok {
		rd = bytes.NewReader(b)
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://"+e.addr+path, rd)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	return resp.StatusCode, doc
}

// try is do for a BMC that may be rebooting: a dropped connection is status 0.
func (e *env) try(method, path, user, pass string) int {
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // test
	req, _ := http.NewRequest(method, "https://"+e.addr+path, nil)
	req.SetBasicAuth(user, pass)
	resp, err := c.Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (e *env) canonicals() map[string]int {
	out := map[string]int{}
	for _, ev := range e.tw.Engine.Events(0, func(ev schema.Event) bool { return ev.Path == dpuPath && ev.Request != nil }) {
		out[ev.Request.Canonical]++
	}
	return out
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestMutualTLSTakeover drives a BlueField BMC through what DPF's zero-trust DPUDevice controller
// does to it: the factory password changed, a CA in the Truststore, a CSR from the BMC's own key,
// the signed certificate installed and served, and requests authenticated by a client
// certificate the BMC verifies against its Truststore and maps to the account named by its CN.
func TestMutualTLSTakeover(t *testing.T) {
	e := up(t)
	user, factory := e.dev.Identity.BMCUser, e.dev.Identity.BMCPassword
	const pw = "Twin-Test-Pass-2026"
	if s, _ := e.do(t, "GET", "/redfish/v1", "", "", nil, nil); s != 200 {
		t.Fatalf("service root: %d", s)
	}
	if s, doc := e.do(t, "PATCH", "/redfish/v1/AccountService/Accounts/"+user, user, factory, map[string]any{"Password": pw}, nil); s != 200 {
		t.Fatalf("password change answered %d %v (bmcweb answers 200)", s, doc)
	}
	trust := "/redfish/v1/Managers/Bluefield_BMC/Truststore/Certificates"
	if s, doc := e.do(t, "POST", trust, user, pw, map[string]any{"CertificateString": pemOf(e.ca), "CertificateType": "PEM"}, nil); s != 200 {
		t.Fatalf("truststore install: %d %v", s, doc)
	}
	if s, _ := e.do(t, "POST", trust, user, pw, map[string]any{"CertificateString": pemOf(e.ca), "CertificateType": "PEM"}, nil); s != 500 {
		t.Fatalf("installing the same CA twice answered %d, want bmcweb's 500", s)
	}
	_, coll := e.do(t, "GET", trust, user, pw, nil, nil)
	if n, _ := coll["Members@odata.count"].(float64); n != 1 {
		t.Fatalf("truststore: %v", coll)
	}
	// CSR from the BMC's key, signed by the stack's CA for the BMC's address
	s, doc := e.do(t, "POST", "/redfish/v1/CertificateService/Actions/CertificateService.GenerateCSR", user, pw, map[string]any{
		"CommonName": "10.30.0.99", "AlternativeNames": []string{"IP: 10.30.0.99", "DNS: localhost", "IP: 127.0.0.1"},
		"CertificateCollection": map[string]any{"@odata.id": "/redfish/v1/Managers/Bluefield_BMC/NetworkProtocol/HTTPS/Certificates"}}, nil)
	if s != 200 {
		t.Fatalf("GenerateCSR: %d %v", s, doc)
	}
	blk, _ := pem.Decode([]byte(doc["CSRString"].(string)))
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil || csr.CheckSignature() != nil || len(csr.IPAddresses) != 2 || csr.Subject.CommonName != "10.30.0.99" {
		t.Fatalf("CSR: %v %+v", err, csr)
	}
	// a certificate for another key is refused, as phosphor-certificate-manager refuses it
	other, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	wrong := sign(t, e.ca, e.key, &x509.Certificate{Subject: pkix.Name{CommonName: "x"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, &other.PublicKey)
	replace := "/redfish/v1/CertificateService/Actions/CertificateService.ReplaceCertificate"
	httpsURI := map[string]any{"@odata.id": "/redfish/v1/Managers/Bluefield_BMC/NetworkProtocol/HTTPS/Certificates/1"}
	if s, _ := e.do(t, "POST", replace, user, pw, map[string]any{"CertificateString": pemOf(wrong), "CertificateType": "PEM", "CertificateUri": httpsURI}, nil); s != 500 {
		t.Fatalf("a certificate for a key the BMC does not hold answered %d", s)
	}
	leaf := sign(t, e.ca, e.key, &x509.Certificate{Subject: csr.Subject, IPAddresses: csr.IPAddresses, DNSNames: csr.DNSNames,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, csr.PublicKey)
	if s, doc := e.do(t, "POST", replace, user, pw, map[string]any{"CertificateString": pemOf(leaf), "CertificateType": "PEM", "CertificateUri": httpsURI}, nil); s != 200 {
		t.Fatalf("ReplaceCertificate: %d %v", s, doc)
	}
	// the BMC now serves it: a client pinning the stack's CA and the BMC's address verifies it
	pool := x509.NewCertPool()
	pool.AddCert(e.ca)
	conn, err := tls.Dial("tcp", e.addr, &tls.Config{RootCAs: pool, ServerName: "10.30.0.99"})
	if err != nil {
		t.Fatalf("the installed certificate is not served: %v", err)
	}
	conn.Close()
	// mutual TLS: a client certificate from the stack's CA, CN root, authenticates as root
	ck, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cc := sign(t, e.ca, e.key, &x509.Certificate{Subject: pkix.Name{CommonName: user}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}, &ck.PublicKey)
	client := &tls.Certificate{Certificate: [][]byte{cc.Raw}, PrivateKey: ck}
	if s, doc := e.do(t, "GET", "/redfish/v1/Managers", "", "", nil, client); s != 200 {
		t.Fatalf("mTLS GET Managers (forwarded to the engine as %s): %d %v", user, s, doc)
	}
	if s, _ := e.do(t, "GET", trust, "", "", nil, client); s != 200 {
		t.Fatalf("mTLS GET Truststore: %d", s)
	}
	// a client certificate from a CA the BMC does not trust does not authenticate
	rogue, rogueKey := newCA(t, "rogue")
	rc := sign(t, rogue, rogueKey, &x509.Certificate{Subject: pkix.Name{CommonName: user}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}, &ck.PublicKey)
	if s, _ := e.do(t, "GET", trust, "", "", nil, &tls.Certificate{Certificate: [][]byte{rc.Raw}, PrivateKey: ck}); s != 401 {
		t.Fatalf("an untrusted client certificate answered %d", s)
	}
	got := e.canonicals()
	for _, c := range []string{"certificate.install", "certificate.csr", "certificate.replace", "certificate.read", "account.password"} {
		if got[c] == 0 {
			t.Errorf("no %s request in the log: %v", c, got)
		}
	}
}

// TestOEMWritesAndFactoryReset covers ConfigFWParameters' rshim writes and ResetToDefaults.
func TestOEMWritesAndFactoryReset(t *testing.T) {
	e := up(t)
	user, factory := e.dev.Identity.BMCUser, e.dev.Identity.BMCPassword
	if s, doc := e.do(t, "POST", "/redfish/v1/Systems/Bluefield/Oem/Nvidia/Actions/HostRshim.Set", user, factory, map[string]any{"HostRshim": "Enabled"}, nil); s != 200 {
		t.Fatalf("HostRshim.Set: %d %v", s, doc)
	}
	_, doc := e.do(t, "GET", "/redfish/v1/Systems/Bluefield/Oem/Nvidia", user, factory, nil, nil)
	if doc["HostRshim"] != "Enabled" || doc["Mode"] != "DpuMode" {
		t.Fatalf("Oem/Nvidia after HostRshim.Set: %v", doc)
	}
	if s, _ := e.do(t, "PATCH", "/redfish/v1/Managers/Bluefield_BMC/Oem/Nvidia", user, factory, map[string]any{"BmcRShim": map[string]any{"BmcRShimEnabled": false}}, nil); s != 200 {
		t.Fatalf("BmcRShim PATCH: %d", s)
	}
	_, doc = e.do(t, "GET", "/redfish/v1/Managers/Bluefield_BMC/Oem/Nvidia", user, factory, nil, nil)
	if v, _ := doc["BmcRShim"].(map[string]any); v["BmcRShimEnabled"] != false {
		t.Fatalf("BmcRShim after PATCH: %v", doc)
	}
	const pw = "Twin-Test-Pass-2026"
	if s, _ := e.do(t, "PATCH", "/redfish/v1/AccountService/Accounts/"+user, user, factory, map[string]any{"Password": pw}, nil); s != 200 {
		t.Fatal("password change")
	}
	if s, _ := e.do(t, "POST", "/redfish/v1/Managers/Bluefield_BMC/Truststore/Certificates", user, pw, map[string]any{"CertificateString": pemOf(e.ca)}, nil); s != 200 {
		t.Fatal("truststore install")
	}
	if s, _ := e.do(t, "POST", "/redfish/v1/Managers/Bluefield_BMC/Actions/Manager.ResetToDefaults", user, pw, map[string]any{"ResetToDefaultsType": "ResetAll"}, nil); s != 200 {
		t.Fatalf("ResetToDefaults: %d", s)
	}
	// the BMC reboots and comes back on its factory password, which must be changed first
	waitFor(t, "the BMC to reboot and return", 10*time.Second, func() bool {
		return e.try("GET", "/redfish/v1/Managers", user, factory) == 403
	})
	_, coll := e.do(t, "PATCH", "/redfish/v1/AccountService/Accounts/"+user, user, factory, map[string]any{"Password": pw}, nil)
	_ = coll
	if s, _ := e.do(t, "GET", "/redfish/v1/Managers", user, pw, nil, nil); s != 200 {
		t.Fatalf("after the factory password is changed, Managers answers %d", s)
	}
	_, coll = e.do(t, "GET", "/redfish/v1/Managers/Bluefield_BMC/Truststore/Certificates", user, pw, nil, nil)
	if n, _ := coll["Members@odata.count"].(float64); n != 0 {
		t.Fatalf("Truststore after ResetToDefaults: %v", coll)
	}
	_, doc = e.do(t, "GET", "/redfish/v1/Systems/Bluefield/Oem/Nvidia", user, pw, nil, nil)
	if doc["HostRshim"] != "Disabled" {
		t.Fatalf("HostRshim after ResetToDefaults is the capture's again: %v", doc["HostRshim"])
	}
}

// bfbImage is one image of a boot stream: a 24-byte header and the data padded to 8 bytes.
func bfbImage(id int, data []byte, last bool) []byte {
	padded := append([]byte{}, data...)
	for len(padded)%8 != 0 {
		padded = append(padded, 0)
	}
	crc := crc32.ChecksumIEEE(padded)
	h := make([]byte, 24)
	w0 := uint64(0x13026642) | uint64(1)<<32 | uint64(3)<<52 | uint64(id)<<56
	binary.LittleEndian.PutUint64(h[0:], w0)
	binary.LittleEndian.PutUint64(h[8:], uint64(len(data))|uint64(crc)<<32)
	follow := uint64(1) << 62
	if last {
		follow = 0
	}
	binary.LittleEndian.PutUint64(h[16:], follow)
	return append(h, padded...)
}

// TestBFBPush is OS Installing on a BlueField-3: SimpleUpdate to DPU_OS with an ImageURI, the
// BMC fetching it, checking the boot stream, and the task's progress; then a stream that is not a
// BFB, refused at the transfer.
func TestBFBPush(t *testing.T) {
	e := up(t)
	info := []byte(`{"//": "This JSON represents a Redfish Software Inventory collection", "Members": [{"Name": "DOCA", "Version": "3.5.0-89"}, {"Name": "BF3_ATF", "Version": "v2.2(release):4.13.0"}]}`)
	stream := append(bfbImage(50, info, false), bfbImage(62, bytes.Repeat([]byte("kernel"), 5000), true)...)
	stream = append(stream, []byte("# bf.cfg\nUPDATE_DPU_OS=yes\n")...)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bfb/":
			w.Write(stream)
		case "/bfb/not-a-bfb":
			w.Write([]byte("####################  This JSON represents ... not a boot stream"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	user, pass := e.dev.Identity.BMCUser, e.dev.Identity.BMCPassword
	// DPF's request: no scheme in ImageURI, TransferProtocol HTTPS, the registry's concat URL
	s, doc := e.do(t, "POST", "/redfish/v1/UpdateService/Actions/UpdateService.SimpleUpdate", user, pass, map[string]any{
		"TransferProtocol": "HTTPS", "ImageURI": host + "/bfb/??a.bfb,bfcfg/b.cfg?/bfb-to-install",
		"Targets": []string{"redfish/v1/UpdateService/FirmwareInventory/DPU_OS"}}, nil)
	if s != 202 {
		t.Fatalf("SimpleUpdate: %d %v", s, doc)
	}
	id := doc["Id"].(string)
	var task map[string]any
	waitFor(t, "the BFB task", 10*time.Second, func() bool {
		_, task = e.do(t, "GET", "/redfish/v1/TaskService/Tasks/"+id, user, pass, nil, nil)
		return task["TaskState"] != "Running"
	})
	if task["TaskState"] != "Completed" || task["PercentComplete"] != float64(100) {
		t.Fatalf("BFB task: %v", task)
	}
	if b, _ := json.Marshal(task["Messages"]); !strings.Contains(string(b), "DOCA 3.5.0-89") || !strings.Contains(string(b), "bf.cfg appended") {
		t.Fatalf("task messages do not describe the stream: %s", b)
	}
	_, coll := e.do(t, "GET", "/redfish/v1/TaskService/Tasks", user, pass, nil, nil)
	if b, _ := json.Marshal(coll); !strings.Contains(string(b), "/Tasks/"+id) {
		t.Fatalf("the task is not in TaskService: %s", b)
	}
	// the Arm was reset into the stream; no DPU OS runs, so OsIsRunning is no longer claimed
	_, sys := e.do(t, "GET", "/redfish/v1/Systems/Bluefield", user, pass, nil, nil)
	if bp, _ := sys["BootProgress"].(map[string]any); bp["OemLastState"] != nil {
		t.Fatalf("BootProgress after the push: %v", bp)
	}
	restarted := false
	for _, ev := range e.tw.Engine.Events(0, func(ev schema.Event) bool { return ev.Path == dpuPath && ev.Request != nil }) {
		if ev.Request.Canonical == "power.restart" && ev.Request.Protocol == "rshim" {
			restarted = true
		}
	}
	if !restarted {
		t.Fatal("the Arm was not reset into the boot stream")
	}
	// a stream that is not a BFB fails the task
	s, doc = e.do(t, "POST", "/redfish/v1/UpdateService/Actions/UpdateService.SimpleUpdate", user, pass, map[string]any{
		"TransferProtocol": "HTTPS", "ImageURI": host + "/bfb/not-a-bfb", "Targets": []string{"/redfish/v1/UpdateService/FirmwareInventory/DPU_OS"}}, nil)
	if s != 202 {
		t.Fatalf("SimpleUpdate: %d %v", s, doc)
	}
	id = doc["Id"].(string)
	waitFor(t, "the second task", 10*time.Second, func() bool {
		_, task = e.do(t, "GET", "/redfish/v1/TaskService/Tasks/"+id, user, pass, nil, nil)
		return task["TaskState"] != "Running"
	})
	if b, _ := json.Marshal(task); task["TaskState"] != "Exception" || !strings.Contains(string(b), "not a BlueField boot stream") {
		t.Fatalf("a non-BFB: %s", b)
	}
}

// pldmPackage builds a DSP0267 1.0 package with one component image carrying version.
func pldmPackage(version string) []byte {
	image := append(bytes.Repeat([]byte{0xA5}, 512), []byte("OpenBMC os-release VERSION="+version+"\n")...)
	pkgVer := "cec1736ApFwPkg-02410017"
	var b bytes.Buffer
	b.Write([]byte{0xF0, 0x18, 0x87, 0x8C, 0xCB, 0x7D, 0x49, 0x43, 0x98, 0x00, 0xA0, 0x2F, 0x05, 0x9A, 0xCA, 0x02})
	b.WriteByte(1)
	sizeAt := b.Len()
	b.Write([]byte{0, 0})     // header size, set below
	b.Write(make([]byte, 13)) // release date
	b.Write([]byte{8, 0})     // component bitmap bit length
	b.WriteByte(1)            // ASCII
	b.WriteByte(byte(len(pkgVer)))
	b.WriteString(pkgVer)
	b.WriteByte(1) // one device record
	rec := []byte{0, 0, 0, 0, 0, 0, 0, 1, 4, 0, 0, 1}
	rec[2] = 0 // descriptors: none, for the test
	rec = append(rec, []byte("dev1")...)
	binary.LittleEndian.PutUint16(rec[0:], uint16(len(rec)))
	b.Write(rec)
	b.Write([]byte{1, 0}) // one component
	comp := make([]byte, 22)
	cv := "cec1736ApFw-02410017"
	comp[21] = byte(len(cv))
	compAt := b.Len()
	b.Write(comp)
	b.WriteString(cv)
	b.Write([]byte{0, 0, 0, 0}) // package header checksum
	hdr := b.Bytes()
	binary.LittleEndian.PutUint16(hdr[sizeAt:], uint16(len(hdr)))
	binary.LittleEndian.PutUint32(hdr[compAt+12:], uint32(len(hdr)))
	binary.LittleEndian.PutUint32(hdr[compAt+16:], uint32(len(image)))
	return append(hdr, image...)
}

// TestBMCFirmwarePush is DPF's BMC firmware update: an octet-stream POST to UpdateService, the
// engine's task, the BMC reset DPF requests when the task completes, and BMC_Firmware then
// reporting the version the package's BMC image carries.
func TestBMCFirmwarePush(t *testing.T) {
	e := up(t)
	user, pass := e.dev.Identity.BMCUser, e.dev.Identity.BMCPassword
	if s, _ := e.do(t, "POST", "/redfish/v1/UpdateService", user, pass, []byte("not a package"), nil); s != 400 {
		t.Fatalf("a non-PLDM image answered %d", s)
	}
	s, doc := e.do(t, "POST", "/redfish/v1/UpdateService", user, pass, pldmPackage("BF-24.10-17"), nil)
	if s != 202 {
		t.Fatalf("push: %d %v", s, doc)
	}
	id := doc["Id"].(string)
	waitFor(t, "the firmware task", 10*time.Second, func() bool {
		_, task := e.do(t, "GET", "/redfish/v1/TaskService/Tasks/"+id, user, pass, nil, nil)
		return task["TaskState"] == "Completed"
	})
	// staged: the BMC runs the image after it reboots, which DPF requests once the task completes
	_, inv := e.do(t, "GET", "/redfish/v1/UpdateService/FirmwareInventory/BMC_Firmware", user, pass, nil, nil)
	if inv["Version"] == "BF-24.10-17" {
		t.Fatalf("the image ran before the BMC rebooted: %v", inv["Version"])
	}
	if s, _ := e.do(t, "POST", "/redfish/v1/Managers/Bluefield_BMC/Actions/Manager.Reset", user, pass, map[string]any{"ResetType": "GracefulRestart"}, nil); s/100 != 2 {
		t.Fatalf("Manager.Reset: %d", s)
	}
	waitFor(t, "the BMC to reboot", 10*time.Second, func() bool { return e.try("GET", "/redfish/v1/Managers", user, pass) != 200 })
	waitFor(t, "the BMC to return", 10*time.Second, func() bool { return e.try("GET", "/redfish/v1/Managers", user, pass) == 200 })
	_, inv = e.do(t, "GET", "/redfish/v1/UpdateService/FirmwareInventory/BMC_Firmware", user, pass, nil, nil)
	_, mgr := e.do(t, "GET", "/redfish/v1/Managers/Bluefield_BMC", user, pass, nil, nil)
	if inv["Version"] != "BF-24.10-17" || mgr["FirmwareVersion"] != "BF-24.10-17" {
		t.Fatalf("after the update: inventory %v, manager %v", inv["Version"], mgr["FirmwareVersion"])
	}
	accepted := 0
	for _, ev := range e.tw.Engine.Events(0, func(ev schema.Event) bool { return ev.Path == dpuPath && ev.Request != nil }) {
		if ev.Request.Canonical == "firmware.update.push" && ev.Request.Status == 202 {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("firmware.update.push accepted %d times; log %v", accepted, e.canonicals())
	}
}

// TestDPUBootFirmwareInventory: the BMC's DPU_UEFI, DPU_ATF and DPU_BSP are the model's boot
// firmware (seeded from the capture), and boot firmware staged by an installer's ARM capsule is
// what they report after the Arm's next reset, the versions DPF compares with its BFB's.
func TestDPUBootFirmwareInventory(t *testing.T) {
	e := up(t)
	user, pass := e.dev.Identity.BMCUser, e.dev.Identity.BMCPassword
	read := func(id string) any {
		_, inv := e.do(t, "GET", "/redfish/v1/UpdateService/FirmwareInventory/"+id, user, pass, nil, nil)
		return inv["Version"]
	}
	if v := read("DPU_UEFI"); v != "4.9.0-46-g7e3911bd4d" {
		t.Fatalf("DPU_UEFI before: %v", v)
	}
	e.tw.Engine.Seed(dpuPath, "an installer's ARM capsule (test)", func(d *model.Device) {
		d.State.Firmware["uefi"].Staged = "4.16.0-48-g3e1cb60aa0"
		d.State.Firmware["bsp"].Staged = "4.16.0.14075"
	})
	if v := read("DPU_UEFI"); v != "4.9.0-46-g7e3911bd4d" {
		t.Fatalf("DPU_UEFI changed before a reset: %v", v)
	}
	for _, o := range []engine.Observation{{Field: "power", To: model.On}, {Field: "reset", To: "arm"}} {
		o.Path, o.Observer = dpuPath, "test"
		if err := e.tw.Engine.Observe(o); err != nil {
			t.Fatal(err)
		}
	}
	if u, b := read("DPU_UEFI"), read("DPU_BSP"); u != "4.16.0-48-g3e1cb60aa0" || b != "4.16.0.14075" {
		t.Fatalf("after the Arm's reset: DPU_UEFI %v, DPU_BSP %v", u, b)
	}
	if v := read("DPU_ATF"); v != "v2.2(release):4.9.0-25-g0ce57e322" {
		t.Fatalf("DPU_ATF, not staged, changed: %v", v)
	}
}

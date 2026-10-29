package nicoagent

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/dhcp"
	_ "github.com/BudEcosystem/dc-twin/internal/adapter/redfish"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

func init() {
	twin.Register("nicoagent-test", func() adapter.Adapter {
		a := New()
		a.controlEvery, a.networkEvery, a.discoveryRetry, a.bootRetry = 150*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond
		return a
	})
}

func forgeOrSkip(t *testing.T, reg *pack.Registry) *Forge {
	t.Helper()
	f, err := LoadForge(reg)
	if err != nil {
		t.Skipf("skipped: NICo's Forge API definition is not obtained here (dctwin assets fetch): %v", err)
	}
	return f
}

// NICo's side, in the shapes its services answer a BlueField it is ingesting: its DHCP server
// (relayed requests only: an AArch64 HTTP client is given iPXE's URL, iPXE a next-server), its
// PXE service (iPXE with an embedded script, the boot script for a machine, the root CA) and
// nico-api (TLS, the machine certificate on calls after discovery).
const (
	pxeSite = "10.90.0.17"
	apiSite = "10.90.0.18"
)

// the script NICo builds into its iPXE (pxe/ipxe/local/embed.ipxe), cut to what a boot runs
const embedded = `#!ipxe
prompt --key p --timeout 4000 Hit the p key to open the menu... && goto menu || goto nico
:menu
exit
:nico
ifconf -c dhcp || goto failed
set nico-pxe-base http://${next-server}
time chain --name nico-boot ${nico-pxe-base}/api/v0/pxe/boot?buildarch=${buildarch}&platform=${platform}&manufacturer=${manufacturer}&product=${product}&serial=${serial} && exit 1 || goto failed
:failed
exit
`

type fakeNICo struct {
	t     *testing.T
	forge *Forge
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey
	caPEM []byte

	mu         sync.Mutex
	dhcpSeen   map[string][]dhcpSeen // chaddr -> requests
	bootFor    map[string]int        // serial -> boot scripts served
	discovered map[string]obj        // interface id -> DiscoverMachine's info
	completed  map[string]bool       // machine id -> DiscoveryCompleted
	calls      map[string]int        // method -> calls
	mtls       map[string]int        // method -> calls made with the machine certificate
	status     []obj                 // RecordDpuNetworkStatus requests
	blobs      map[string]int        // path -> fetches
	nextIP     int

	scouted  map[string]obj // host machine id -> scout's DiscoverMachine info
	rebooted map[string]bool
	hostStep map[string]int    // host machine id -> instructions answered
	admin    map[string]string // DPU machine id -> the admin address NICo gives its host
	hostFrom map[string]string // host machine id -> the source address its scout called from
}

type dhcpSeen struct{ class, user, circuit string }

func newFakeNICo(t *testing.T, f *Forge) *fakeNICo {
	n := &fakeNICo{t: t, forge: f, dhcpSeen: map[string][]dhcpSeen{}, bootFor: map[string]int{}, discovered: map[string]obj{},
		completed: map[string]bool{}, calls: map[string]int{}, mtls: map[string]int{}, blobs: map[string]int{}, nextIP: 100,
		scouted: map[string]obj{}, rebooted: map[string]bool{}, hostStep: map[string]int{}, admin: map[string]string{}, hostFrom: map[string]string{}}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "site root"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	n.ca, _ = x509.ParseCertificate(der)
	n.caKey = key
	n.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return n
}

func (n *fakeNICo) issue(cn string, ips []net.IP, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, IPAddresses: ips,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{usage},
		KeyUsage: x509.KeyUsageDigitalSignature}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, n.ca, &key.PublicKey, n.caKey)
	kb, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

// start runs NICo's services on loopback; it returns the DHCP server and the siteServices output.
func (n *fakeNICo) start() (dhcpAddr, services string) {
	// the PXE service's blob server (:8080) and boot API (:80)
	nbp := append(append([]byte("MZ\x90\x00 iPXE 1.21.1 "), []byte(embedded)...), 0)
	nbp = append(nbp, bytes.Repeat([]byte{0xa5}, 200_000)...)
	blobs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.mu.Lock()
		n.blobs[r.URL.Path]++
		n.mu.Unlock()
		switch r.URL.Path {
		case "/public/blobs/internal/aarch64/ipxe.efi", "/public/blobs/internal/x86_64/ipxe.efi":
			w.Write(nbp)
		case "/public/blobs/internal/aarch64/carbide.efi", "/public/blobs/internal/aarch64/carbide.root", "/public/blobs/internal/aarch64/scout.efi", "/public/blobs/internal/x86_64/scout.efi":
			w.Write(bytes.Repeat([]byte("k"), 50_000))
		default:
			http.NotFound(w, r)
		}
	}))
	n.t.Cleanup(blobs.Close)
	boot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v0/tls/root_ca":
			w.Write(n.caPEM)
		case "/api/v0/pxe/boot":
			q := r.URL.Query()
			serial := q.Get("serial")
			n.mu.Lock()
			n.bootFor[serial]++
			done := false
			for _, info := range n.discovered {
				if str(info, "dmi_data", "product_serial") == serial {
					done = true
				}
			}
			n.mu.Unlock()
			dir := map[string]string{"arm64": "aarch64", "x86_64": "x86_64"}[q.Get("buildarch")]
			if dir == "" || q.Get("platform") != "efi" {
				http.Error(w, "unexpected arch "+r.URL.RawQuery, 400)
				return
			}
			if !strings.Contains(q.Get("product"), "BlueField") { // a host: scout
				fmt.Fprintf(w, "#!ipxe\nset base-url http://%s:8080/public/blobs\nkernel ${base-url}/internal/%s/scout.efi console=tty0 cli_cmd=auto-detect machine_id=hiface-%s server_uri=https://%s:1079 pxe_uri=http://%s\nboot\n",
					pxeSite, dir, serial, apiSite, pxeSite)
				return
			}
			if done { // NICo serves a DPU past Init an exit: it boots its disk
				fmt.Fprintf(w, "#!ipxe\necho DPU %s: boot from disk\nsanboot --no-describe --drive 0x80 ||\nsanboot --no-describe --drive 0x81 ||\nexit\n", serial)
				return
			}
			fmt.Fprintf(w, "#!ipxe\nset base-url http://%s:8080/public/blobs\nkernel ${base-url}/internal/aarch64/carbide.efi console=tty0 ip=dhcp cli_cmd=auto-detect bfnet=oob_net0:dhcp machine_id=iface-%s server_uri=https://%s:1079 pxe_uri=http://%s\ninitrd ${base-url}/internal/aarch64/carbide.root\nboot\n",
				pxeSite, serial, apiSite, pxeSite)
		default:
			http.NotFound(w, r)
		}
	}))
	n.t.Cleanup(boot.Close)
	// nico-api
	cert, key := n.issue("nico-api", []net.IP{net.ParseIP(apiSite), net.ParseIP("127.0.0.1")}, x509.ExtKeyUsageServerAuth)
	pair, _ := tls.X509KeyPair(cert, key)
	pool := x509.NewCertPool()
	pool.AddCert(n.ca)
	gs := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.VerifyClientCertIfGiven})),
		grpc.UnknownServiceHandler(n.serveForge))
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		n.t.Fatal(err)
	}
	go gs.Serve(l)
	n.t.Cleanup(gs.Stop)
	// the DHCP server
	dc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		n.t.Fatal(err)
	}
	n.t.Cleanup(func() { dc.Close() })
	go n.serveDHCP(dc)
	services = strings.Join([]string{
		"pxe@" + pxeSite + ":80=" + strings.TrimPrefix(boot.URL, "http://"),
		"blobs@" + pxeSite + ":8080=" + strings.TrimPrefix(blobs.URL, "http://"),
		"api@" + apiSite + ":1079=" + l.Addr().String(),
	}, ",")
	return dc.LocalAddr().String(), services
}

func (n *fakeNICo) serveDHCP(c *net.UDPConn) {
	buf := make([]byte, 4096)
	for {
		k, from, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		m, err := dhcpv4.FromBytes(buf[:k])
		if err != nil || m.GatewayIPAddr.IsUnspecified() {
			continue
		}
		var circuit string
		link := m.GatewayIPAddr.To4()
		if rai := m.RelayAgentInfo(); rai != nil {
			circuit = string(rai.Get(dhcpv4.GenericOptionCode(1)))
			if ls := rai.Get(dhcpv4.GenericOptionCode(5)); len(ls) == 4 {
				link = net.IP(ls) // link selection (RFC 3527): the client's segment
			}
		}
		user := string(m.Options.Get(dhcpv4.OptionUserClassInformation))
		n.mu.Lock()
		n.dhcpSeen[m.ClientHWAddr.String()] = append(n.dhcpSeen[m.ClientHWAddr.String()], dhcpSeen{m.ClassIdentifier(), user, circuit})
		ip := net.IPv4(link[0], link[1], link[2], byte(n.nextIP))
		if m.MessageType() == dhcpv4.MessageTypeDiscover {
			n.nextIP++
		} else if req := m.RequestedIPAddress(); req != nil {
			ip = req
		}
		n.mu.Unlock()
		t := dhcpv4.MessageTypeOffer
		if m.MessageType() == dhcpv4.MessageTypeRequest {
			t = dhcpv4.MessageTypeAck
		}
		mods := []dhcpv4.Modifier{dhcpv4.WithMessageType(t), dhcpv4.WithYourIP(ip), dhcpv4.WithServerIP(net.ParseIP(pxeSite)),
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP(pxeSite))), dhcpv4.WithOption(dhcpv4.OptSubnetMask(net.CIDRMask(24, 32))),
			dhcpv4.WithOption(dhcpv4.OptRouter(link)), dhcpv4.WithOption(dhcpv4.OptIPAddressLeaseTime(time.Hour))}
		if strings.HasPrefix(m.ClassIdentifier(), "HTTPClient") && user != "iPXE" {
			mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier("HTTPClient")),
				dhcpv4.WithOption(dhcpv4.OptBootFileName("http://"+pxeSite+":8080/public/blobs/internal/"+map[bool]string{true: "aarch64", false: "x86_64"}[strings.HasSuffix(m.ClassIdentifier(), "00019")]+"/ipxe.efi")))
		}
		r, _ := dhcpv4.NewReplyFromRequest(m, mods...)
		_, _ = c.WriteToUDP(r.ToBytes(), &net.UDPAddr{IP: m.GatewayIPAddr, Port: from.Port})
	}
}

// serveForge answers the Forge methods a DPU calls, with messages of the compiled definition.
func (n *fakeNICo) serveForge(_ any, stream grpc.ServerStream) error {
	full, _ := grpc.MethodFromServerStream(stream)
	name := full[strings.LastIndex(full, "/")+1:]
	md := n.forge.Service.Methods().ByName(protoreflect.Name(name))
	if md == nil {
		return status.Error(codes.Unimplemented, name)
	}
	in := dynamicpb.NewMessage(md.Input())
	if err := stream.RecvMsg(in); err != nil {
		return err
	}
	b, _ := (protojson.MarshalOptions{UseProtoNames: true, Resolver: n.forge.types()}).Marshal(in)
	var req obj
	_ = json.Unmarshal(b, &req)
	withCert := false
	if p, ok := peer.FromContext(stream.Context()); ok {
		if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(ti.State.PeerCertificates) > 0 {
			withCert = true
		}
	}
	n.mu.Lock()
	n.calls[name]++
	if withCert {
		n.mtls[name]++
	}
	n.mu.Unlock()
	var res obj
	switch name {
	case "DiscoverMachine":
		if str(req, "discovery_reporter") == "MACHINE_DISCOVERY_REPORTER_SCOUT" {
			info, _ := req["info"].(map[string]any)
			if str(info, "tpm_ek_certificate") == "" {
				return status.Error(codes.InvalidArgument, "ignoring DiscoverMachine request for non-tpm enabled host")
			}
			id := "host-" + str(info, "dmi_data", "product_serial")
			from := ""
			if p, ok := peer.FromContext(stream.Context()); ok {
				from = p.Addr.String()
			}
			n.mu.Lock()
			n.scouted[id], n.hostFrom[id] = info, from
			n.mu.Unlock()
			res = obj{"machine_id": obj{"id": id}}
			break
		}
		if str(req, "discovery_reporter") != "MACHINE_DISCOVERY_REPORTER_DPU_AGENT" {
			return status.Error(codes.InvalidArgument, "not a DPU agent")
		}
		iface := str(req, "machine_interface_id", "value")
		info, _ := req["info"].(map[string]any)
		n.mu.Lock()
		n.discovered[iface] = info
		n.mu.Unlock()
		cert, key := n.issue(iface, nil, x509.ExtKeyUsageClientAuth)
		res = obj{"machine_id": obj{"id": "dpu-" + str(info, "dmi_data", "product_serial")}, "machine_interface_id": obj{"value": iface},
			"machine_certificate": obj{"public_key": base64.StdEncoding.EncodeToString(cert), "private_key": base64.StdEncoding.EncodeToString(key),
				"issuing_ca": base64.StdEncoding.EncodeToString(n.caPEM)}}
	case "RebootCompleted":
		n.mu.Lock()
		n.rebooted[str(req, "machine_id", "id")] = true
		n.mu.Unlock()
		res = obj{}
	case "CleanupMachineCompleted", "MachineValidationCompleted":
		id := str(req, "machine_id", "id")
		if name == "MachineValidationCompleted" && str(req, "validation_id", "value") != "val-"+id {
			return status.Error(codes.InvalidArgument, "unknown validation")
		}
		n.mu.Lock()
		n.hostStep[id]++
		n.mu.Unlock()
		res = obj{}
	case "ForgeAgentControl":
		id := str(req, "machine_id", "id")
		if strings.HasPrefix(id, "host-") {
			// discovery, then cleanup, then validation, then nothing
			n.mu.Lock()
			step, done := n.hostStep[id], n.completed[id]
			n.mu.Unlock()
			switch {
			case !done:
				res = obj{"discovery": obj{}}
			case step == 0:
				res = obj{"reset": obj{}}
			case step == 1:
				res = obj{"machine_validation": obj{"is_enabled": true, "validation_id": obj{"value": "val-" + id}}}
			default:
				res = obj{"noop": obj{}}
			}
			break
		}
		n.mu.Lock()
		done := n.completed[id]
		n.mu.Unlock()
		if done {
			res = obj{"noop": obj{}}
		} else {
			res = obj{"legacy_action": "DISCOVERY", "discovery": obj{}}
		}
	case "DiscoveryCompleted":
		n.mu.Lock()
		n.completed[str(req, "machine_id", "id")] = true
		n.mu.Unlock()
		res = obj{}
	case "GetManagedHostNetworkConfig":
		id := str(req, "dpu_machine_id", "id")
		n.mu.Lock()
		ip := n.admin[id]
		if ip == "" {
			ip = fmt.Sprintf("192.168.252.%d", 10+len(n.admin))
			n.admin[id] = ip
		}
		n.mu.Unlock()
		res = obj{"managed_host_config_version": "V3-T1700000000", "use_admin_network": true,
			// as NICo writes it: the addresses entries, the gateway a CIDR (the flat fields are deprecated)
			"admin_interface": obj{"function_type": "PHYSICAL", "addresses": []any{obj{"address_family": "ADDRESS_FAMILY_V4", "ip": ip,
				"gateway": "192.168.252.1/24", "interface_prefix": ip + "/32", "prefix": "192.168.252.0/24"}}}}
	case "RecordDpuNetworkStatus":
		n.mu.Lock()
		n.status = append(n.status, req)
		n.mu.Unlock()
		res = obj{}
	default:
		return status.Error(codes.Unimplemented, name)
	}
	out := dynamicpb.NewMessage(md.Output())
	ob, _ := json.Marshal(res)
	if err := (protojson.UnmarshalOptions{Resolver: n.forge.types()}).Unmarshal(ob, out); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	return stream.SendMsg(out)
}

func upDPF(t *testing.T, nico *fakeNICo) (*twin.Twin, *Adapter) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../../compat/dpf/estate.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dhcpAddr, services := nico.start()
	nw := est.Spec.Networks["oob"]
	nw.DHCP, nw.Server = "stack", dhcpAddr
	est.Spec.Networks["oob"] = nw
	est.Spec.Stacks = []schema.StackRef{{Name: "nvidia-dsx-os@0.1.0", Impersonators: []string{DPUAgent}}}
	tw, err := twin.UpEstate(est, "", reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{"dhcp", "redfish", "nicoagent-test"},
		Outputs: map[string]string{"siteServices": services}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tw.Down)
	a := tw.Adapter(Name).(*Adapter)
	t.Cleanup(func() {
		if t.Failed() {
			for _, l := range a.Report().Lines {
				t.Logf("report: %s", l)
			}
			nico.mu.Lock()
			t.Logf("NICo saw: calls %v, boots %v, blobs %v, dhcp from %d clients", nico.calls, nico.bootFor, nico.blobs, len(nico.dhcpSeen))
			nico.mu.Unlock()
		}
	})
	return tw, a
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A model BlueField, with the impersonator enabled, boots as NICo boots one it ingests: UEFI
// HTTP boot through the twin relay of its BMC's port with its OOB MAC, NICo's iPXE and the script
// built into it, the boot script for its serial, NICo's DPU image (the root CA, DiscoverMachine
// with the BMC's identity), and the agent (DiscoveryCompleted when told, the network status
// reported applied, over mutual TLS). Reset, it boots its disk and the agent carries on without a
// second discovery; powered off, it falls silent.
func TestModelBlueFieldBootsAndReportsAsNICosDPUSoftware(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	nico := newFakeNICo(t, forgeOrSkip(t, reg))
	tw, a := upDPF(t, nico)
	if a.inactive != "" {
		t.Fatalf("inactive: %s", a.inactive)
	}
	if len(a.dpus) != 2 {
		t.Fatalf("%d DPUs, want the estate's 2", len(a.dpus))
	}
	waitFor(t, "both DPUs' network status", 60*time.Second, func() bool {
		nico.mu.Lock()
		defer nico.mu.Unlock()
		seen := map[string]bool{}
		for _, s := range nico.status {
			seen[str(s, "dpu_machine_id", "id")] = true
		}
		return len(seen) == 2
	})
	rf := tw.Adapter("redfish").(resources)
	for _, p := range a.order {
		b := a.dpus[p]
		sys, _ := rf.Get(p, "/redfish/v1/Systems/Bluefield")
		oem, _ := rf.Get(p, "/redfish/v1/Systems/Bluefield/Oem/Nvidia")
		oob, _ := rf.Get(p, "/redfish/v1/Systems/Bluefield/EthernetInterfaces/oob0")
		serial, _ := sys["SerialNumber"].(string)
		oobMAC, _ := oob["MACAddress"].(string)
		nico.mu.Lock()
		info := nico.discovered["iface-"+serial]
		reqs := nico.dhcpSeen[strings.ToLower(oobMAC)]
		boots := nico.bootFor[serial]
		nico.mu.Unlock()
		// the DHCP NICo saw: the OOB MAC, behind the BMC's switch port, as firmware then iPXE
		if len(reqs) < 4 || !strings.HasPrefix(reqs[0].class, "HTTPClient:Arch:00019") || reqs[len(reqs)-1].user != "iPXE" {
			t.Errorf("%s: NICo's DHCP saw %+v from %s", p, reqs, oobMAC)
		}
		bmc := bmcPort(b.dev)
		if bmc == nil || reqs[0].circuit == "" || !strings.HasSuffix(reqs[0].circuit, ":"+bmc.Link.Peer(bmc).Name) {
			t.Errorf("%s: circuit id %q, want the BMC's switch port", p, reqs[0].circuit)
		}
		if boots != 1 {
			t.Errorf("%s: %d boot scripts served, want 1", p, boots)
		}
		// what DiscoverMachine carried: the identity the BMC serves
		if info == nil {
			t.Fatalf("%s: no DiscoverMachine for interface iface-%s", p, serial)
		}
		if got := str(info, "dmi_data", "product_serial"); got != serial {
			t.Errorf("%s: DMI product serial %q, want the BMC's %q", p, got, serial)
		}
		if str(info, "dmi_data", "board_serial") != "Unspecified Base Board Serial Number" || !strings.Contains(strings.ToLower(str(info, "dmi_data", "board_name")), "bluefield") {
			t.Errorf("%s: DMI %v", p, info["dmi_data"])
		}
		if got, want := str(info, "dpu_info", "factory_mac_address"), colonMAC(fmt.Sprint(oem["BaseMAC"])); got != want {
			t.Errorf("%s: factory MAC %q, want BaseMAC %q", p, got, want)
		}
		if str(info, "machine_arch") != "AARCH64" {
			t.Errorf("%s: machine_arch %q", p, str(info, "machine_arch"))
		}
		var booted string
		tw.Engine.Read(func(m *model.Model) { booted = m.DeviceBy[p].State.Booted })
		if booted != stepOS {
			t.Errorf("%s: boot progress %q, want %q", p, booted, stepOS)
		}
	}
	nico.mu.Lock()
	if nico.calls["DiscoverMachine"] != 2 || nico.mtls["DiscoverMachine"] != 0 {
		t.Errorf("DiscoverMachine: %d calls (%d with a certificate), want 2 anonymous", nico.calls["DiscoverMachine"], nico.mtls["DiscoverMachine"])
	}
	if nico.calls["DiscoveryCompleted"] != 2 || nico.mtls["RecordDpuNetworkStatus"] != nico.calls["RecordDpuNetworkStatus"] {
		t.Errorf("calls %v, with the machine certificate %v", nico.calls, nico.mtls)
	}
	st := nico.status[0]
	nico.mu.Unlock()
	if str(st, "network_config_version") != "V3-T1700000000" || str(st, "dpu_health", "source") != "forge-dpu-agent" {
		t.Errorf("network status %v", st)
	}
	ifs, _ := st["interfaces"].([]any)
	if len(ifs) != 1 || !strings.HasPrefix(fmt.Sprint(ifs[0].(map[string]any)["addresses"]), "[192.168.252.") {
		t.Errorf("interfaces %v", st["interfaces"])
	}

	// a reset (NICo restarting the DPU through its BMC): the boot script is now an exit, the DPU
	// boots its disk, and the installed agent carries on
	p := a.order[0]
	b := a.dpus[p]
	srcEv := schema.EventSource{Class: "scenario", Principal: "test"}
	if err := tw.Engine.Power(engine.Request{Path: p, Canonical: "power.restart", Protocol: "test", Source: srcEv}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the reset's boot from disk", 30*time.Second, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return strings.HasPrefix(b.last, "booted the installed DPU OS")
	})
	nico.mu.Lock()
	before := nico.calls["RecordDpuNetworkStatus"]
	discoveries := nico.calls["DiscoverMachine"]
	nico.mu.Unlock()
	waitFor(t, "the agent after the reset", 10*time.Second, func() bool {
		nico.mu.Lock()
		defer nico.mu.Unlock()
		return nico.calls["RecordDpuNetworkStatus"] > before+2
	})
	if discoveries != 2 {
		t.Errorf("DiscoverMachine %d times after a reset to disk, want still 2", discoveries)
	}
	// powered off, both fall silent
	for _, p := range a.order {
		if err := tw.Engine.Power(engine.Request{Path: p, Canonical: "power.off.force", Protocol: "test", Source: srcEv}); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "both DPUs off", 10*time.Second, func() bool {
		for _, b := range a.dpus {
			b.mu.Lock()
			running := b.cancel != nil
			b.mu.Unlock()
			if running {
				return false
			}
		}
		return true
	})
	nico.mu.Lock()
	quiet := nico.calls["RecordDpuNetworkStatus"]
	nico.mu.Unlock()
	time.Sleep(600 * time.Millisecond)
	nico.mu.Lock()
	if nico.calls["RecordDpuNetworkStatus"] != quiet {
		t.Errorf("the agent reported %d times after power off", nico.calls["RecordDpuNetworkStatus"]-quiet)
	}
	nico.mu.Unlock()
	r := a.Report()
	if len(r.Impersonated) != 1 {
		t.Errorf("report: impersonated %v", r.Impersonated)
	}
	for _, l := range r.Lines {
		t.Logf("report: %s", l)
	}
}

// Without the impersonator named by the estate the adapter does nothing, and says why.
func TestInactiveUnlessTheEstateEnablesIt(t *testing.T) {
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../../compat/dpf/estate.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tw, err := twin.UpEstate(est, "", reg, twin.Options{StateDir: t.TempDir(), Adapters: []string{Name},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer tw.Down()
	a := tw.Adapter(Name).(*Adapter)
	if !strings.Contains(a.inactive, DPUAgent) || len(a.Report().Impersonated) != 0 {
		t.Fatalf("inactive %q, report %+v", a.inactive, a.Report())
	}
}

func TestEmbeddedScriptAndMACs(t *testing.T) {
	img := append(append([]byte("MZ junk iPXE "), []byte("#!ipxe\necho hi\n")...), 0, 'x')
	if got := string(embeddedScript(img)); got != "#!ipxe\necho hi\n" {
		t.Errorf("embedded script %q", got)
	}
	if embeddedScript([]byte("no script")) != nil {
		t.Error("found a script in an image with none")
	}
	if got := colonMAC("5C257308F352"); got != "5c:25:73:08:f3:52" {
		t.Errorf("colonMAC %q", got)
	}
	if got := cmdlineArg("a=1 machine_id=x-y server_uri=https://h:1", "machine_id"); got != "x-y" {
		t.Errorf("cmdlineArg %q", got)
	}
	_ = url.URL{}
}

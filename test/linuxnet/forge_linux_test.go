//go:build linux

package linuxnet

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/BudEcosystem/dc-twin/internal/adapter/nicoagent"
)

type obj = map[string]any

// forgeHarness is a Forge API server for NICo's real DPU agent to talk to in a test (D71, P3): not
// NICo, which decides what each DPU's network is from its state machine and its database, but a
// stand-in that answers the agent's calls with what the test decides, and records what the agent
// reports. It speaks NICo's Forge gRPC API, compiled from NICo's own forge.proto, over TLS with a
// site root CA of its own; the agent's machine certificate is issued by it at DiscoverMachine.
type forgeHarness struct {
	t     *testing.T
	forge *nicoagent.Forge
	ca    *x509.Certificate
	caKey *ecdsa.PrivateKey
	CAPEM []byte

	mu       sync.Mutex
	handlers map[string]func(req obj, mtls bool) (obj, error)
	calls    map[string]int
	reqs     map[string][]obj
}

func newForgeHarness(t *testing.T, f *nicoagent.Forge) *forgeHarness {
	h := &forgeHarness{t: t, forge: f, handlers: map[string]func(obj, bool) (obj, error){}, calls: map[string]int{}, reqs: map[string][]obj{}}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dc-twin test site root"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	h.ca, _ = x509.ParseCertificate(der)
	h.caKey = key
	h.CAPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return h
}

// issue is a certificate from the harness's CA.
func (h *forgeHarness) issue(cn string, dns []string, ips []net.IP, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, DNSNames: dns, IPAddresses: ips,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{usage},
		KeyUsage: x509.KeyUsageDigitalSignature}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, h.ca, &key.PublicKey, h.caKey)
	kb, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

// handle sets what a method answers.
func (h *forgeHarness) handle(method string, f func(req obj, mtls bool) (obj, error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handlers[method] = f
}

// requests is a copy of what the agent sent a method.
func (h *forgeHarness) requests(method string) []obj {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]obj{}, h.reqs[method]...)
}

// serve serves the Forge API over TLS on a listener, as carbide-api.forge, accepting client
// certificates from its CA (the agent presents its machine certificate after DiscoverMachine).
func (h *forgeHarness) serve(ln net.Listener) *grpc.Server {
	certPEM, keyPEM := h.issue("carbide-api.forge", []string{"carbide-api.forge", "carbide-api"}, []net.IP{net.ParseIP("10.99.0.1")}, x509.ExtKeyUsageServerAuth)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		h.t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(h.ca)
	creds := credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.VerifyClientCertIfGiven,
		NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12})
	s := grpc.NewServer(grpc.Creds(creds), grpc.UnknownServiceHandler(h.stream))
	go func() { _ = s.Serve(ln) }()
	return s
}

func (h *forgeHarness) stream(_ any, stream grpc.ServerStream) error {
	full, _ := grpc.MethodFromServerStream(stream)
	if !strings.HasPrefix(full, "/"+nicoagent.ServiceName+"/") {
		return status.Error(codes.Unimplemented, full)
	}
	name := full[strings.LastIndex(full, "/")+1:]
	md := h.forge.Service.Methods().ByName(protoreflect.Name(name))
	if md == nil {
		return status.Error(codes.Unimplemented, name)
	}
	in := dynamicpb.NewMessage(md.Input())
	if err := stream.RecvMsg(in); err != nil {
		return err
	}
	b, _ := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}.Marshal(in)
	var req obj
	_ = json.Unmarshal(b, &req)
	mtls := false
	if p, ok := peerTLS(stream); ok && len(p.State.PeerCertificates) > 0 {
		mtls = true
	}
	h.mu.Lock()
	h.calls[name]++
	h.reqs[name] = append(h.reqs[name], req)
	f := h.handlers[name]
	h.mu.Unlock()
	if f == nil {
		return status.Error(codes.Unimplemented, "the test's Forge harness does not answer "+name)
	}
	res, err := f(req, mtls)
	if err != nil {
		return err
	}
	out := dynamicpb.NewMessage(md.Output())
	rb, _ := json.Marshal(res)
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(rb, out); err != nil {
		h.t.Errorf("the harness's %s response does not fit NICo's proto: %v\n%s", name, err, rb)
		return status.Error(codes.Internal, err.Error())
	}
	return stream.SendMsg(out)
}

func peerTLS(stream grpc.ServerStream) (credentials.TLSInfo, bool) {
	p, ok := peer.FromContext(stream.Context())
	if !ok {
		return credentials.TLSInfo{}, false
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	return ti, ok
}

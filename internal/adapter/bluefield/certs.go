package bluefield

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// trustCert is one certificate of the Manager's Truststore (bmcweb's CA certificates, against
// which it verifies client certificates when TLS authentication is on).
type trustCert struct {
	id   string
	pem  string
	cert *x509.Certificate
}

// truststoreLimit is how many certificates a BlueField BMC's Truststore accepts (DPF frees slots
// before installing because of it).
const truststoreLimit = 10

func fingerprint(c *x509.Certificate) string {
	s := sha256.Sum256(c.Raw)
	return hex.EncodeToString(s[:])
}

// parsePEMChain reads every CERTIFICATE block of s.
func parsePEMChain(s string) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	rest := []byte(s)
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no PEM certificate")
	}
	return out, nil
}

func pemOf(c *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
}

// verifyClient checks a client certificate chain against the Truststore and returns the account
// it names: bmcweb maps a client certificate to a user by its CommonName (the captured
// AccountService's CertificateMappingAttribute) and requires the client-auth usage.
func (f *front) verifyClient(chain []*x509.Certificate) (string, bool) {
	f.mu.Lock()
	roots := x509.NewCertPool()
	for _, t := range f.trust {
		roots.AddCert(t.cert)
	}
	n := len(f.trust)
	f.mu.Unlock()
	if n == 0 {
		return "", false
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	leaf := chain[0]
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return "", false
	}
	if leaf.Subject.CommonName == "" {
		return "", false
	}
	return leaf.Subject.CommonName, true
}

func certResource(uri, id, name string, c *x509.Certificate) map[string]any {
	ku := []any{}
	if c.KeyUsage&x509.KeyUsageKeyEncipherment != 0 {
		ku = append(ku, "KeyEncipherment")
	}
	if c.KeyUsage&x509.KeyUsageDigitalSignature != 0 {
		ku = append(ku, "DigitalSignature")
	}
	if c.KeyUsage&x509.KeyUsageCertSign != 0 {
		ku = append(ku, "KeyCertSign")
	}
	for _, e := range c.ExtKeyUsage {
		switch e {
		case x509.ExtKeyUsageServerAuth:
			ku = append(ku, "ServerAuthentication")
		case x509.ExtKeyUsageClientAuth:
			ku = append(ku, "ClientAuthentication")
		}
	}
	dn := func(n pkix.Name) map[string]any {
		m := map[string]any{"CommonName": n.CommonName}
		if len(n.Organization) > 0 {
			m["Organization"] = n.Organization[0]
		}
		if len(n.Country) > 0 {
			m["Country"] = n.Country[0]
		}
		return m
	}
	return map[string]any{"@odata.id": uri, "@odata.type": "#Certificate.v1_5_0.Certificate", "Id": id, "Name": name,
		"CertificateString": pemOf(c), "CertificateType": "PEM", "KeyUsage": ku, "Issuer": dn(c.Issuer), "Subject": dn(c.Subject),
		"ValidNotBefore": c.NotBefore.UTC().Format(time.RFC3339), "ValidNotAfter": c.NotAfter.UTC().Format(time.RFC3339)}
}

// ---------------------------------------------------------------- Truststore

func (f *front) trustURI() string { return f.manager + "/Truststore/Certificates" }

func (f *front) getTruststore(w http.ResponseWriter, r *http.Request, path string, p principal) {
	f.log(r, p, path, "certificate.read", http.StatusOK)
	f.mu.Lock()
	members := []any{}
	for _, t := range f.trust {
		members = append(members, map[string]any{"@odata.id": f.trustURI() + "/" + t.id})
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"@odata.id": f.trustURI(), "@odata.type": "#CertificateCollection.CertificateCollection",
		"Name": "TrustStore Certificates Collection", "Description": "A Collection of TrustStore certificate instances",
		"Members": members, "Members@odata.count": len(members),
		"@Redfish.SupportedCertificates": []any{"PEM"}})
}

func (f *front) findTrust(id string) (trustCert, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.trust {
		if t.id == id {
			return t, true
		}
	}
	return trustCert{}, false
}

func (f *front) getTrustCert(w http.ResponseWriter, r *http.Request, path string, p principal) {
	id := path[strings.LastIndex(path, "/")+1:]
	t, ok := f.findTrust(id)
	if !ok {
		f.log(r, p, path, "certificate.read", http.StatusNotFound)
		writeError(w, http.StatusNotFound, "Base.1.15.ResourceNotFound", "The requested resource of type Certificate named '"+id+"' was not found.")
		return
	}
	f.log(r, p, path, "certificate.read", http.StatusOK)
	writeJSON(w, http.StatusOK, certResource(path, id, "TrustStore Certificate", t.cert))
}

func (f *front) installTrust(w http.ResponseWriter, r *http.Request, path string, p principal) {
	var body struct{ CertificateString, CertificateType string }
	if err := decode(r, &body); err != nil || body.CertificateString == "" {
		f.log(r, p, path, "certificate.install", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.PropertyMissing", "CertificateString is required.")
		return
	}
	if body.CertificateType != "" && body.CertificateType != "PEM" {
		f.log(r, p, path, "certificate.install", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.PropertyValueNotInList", "CertificateType must be PEM.")
		return
	}
	chain, err := parsePEMChain(body.CertificateString)
	if err != nil {
		f.log(r, p, path, "certificate.install", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.PropertyValueFormatError", "CertificateString is not a PEM certificate: "+err.Error())
		return
	}
	c := chain[0]
	f.mu.Lock()
	for _, t := range f.trust {
		if fingerprint(t.cert) == fingerprint(c) {
			f.mu.Unlock()
			f.log(r, p, path, "certificate.install", http.StatusInternalServerError)
			// bmcweb answers an already-installed certificate with InternalError (DPF's comment on
			// the old install-then-replace sequence records it)
			writeError(w, http.StatusInternalServerError, "Base.1.15.InternalError", "The certificate is already in the Truststore.")
			return
		}
	}
	if len(f.trust) >= truststoreLimit {
		f.mu.Unlock()
		f.log(r, p, path, "certificate.install", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.CreateLimitReachedForResource", "The Truststore holds its limit of "+strconv.Itoa(truststoreLimit)+" certificates.")
		return
	}
	f.trustSeq++
	id := strconv.Itoa(f.trustSeq)
	f.trust = append(f.trust, trustCert{id: id, pem: pemOf(c), cert: c})
	n := len(f.trust)
	f.mu.Unlock()
	seq := f.log(r, p, path, "certificate.install", http.StatusOK)
	f.commit("certificate.truststore", n-1, n, "installed "+c.Subject.CommonName+" sha256:"+fingerprint(c)[:16], seq)
	uri := f.trustURI() + "/" + id
	w.Header().Set("Location", uri)
	writeJSON(w, http.StatusOK, certResource(uri, id, "TrustStore Certificate", c))
}

func (f *front) deleteTrustCert(w http.ResponseWriter, r *http.Request, path string, p principal) {
	id := path[strings.LastIndex(path, "/")+1:]
	f.mu.Lock()
	idx := -1
	for i, t := range f.trust {
		if t.id == id {
			idx = i
		}
	}
	if idx < 0 {
		f.mu.Unlock()
		f.log(r, p, path, "certificate.delete", http.StatusNotFound)
		writeError(w, http.StatusNotFound, "Base.1.15.ResourceNotFound", "The requested resource of type Certificate named '"+id+"' was not found.")
		return
	}
	gone := f.trust[idx]
	f.trust = append(f.trust[:idx], f.trust[idx+1:]...)
	n := len(f.trust)
	f.mu.Unlock()
	seq := f.log(r, p, path, "certificate.delete", http.StatusNoContent)
	f.commit("certificate.truststore", n+1, n, "removed "+gone.cert.Subject.CommonName, seq)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------- HTTPS certificate

func (f *front) httpsURI() string { return f.manager + "/NetworkProtocol/HTTPS/Certificates" }

func (f *front) getHTTPSCollection(w http.ResponseWriter, r *http.Request, path string, p principal) {
	f.log(r, p, path, "certificate.read", http.StatusOK)
	writeJSON(w, http.StatusOK, map[string]any{"@odata.id": f.httpsURI(), "@odata.type": "#CertificateCollection.CertificateCollection",
		"Name": "HTTPS Certificates Collection", "Members": []any{map[string]any{"@odata.id": f.httpsURI() + "/1"}}, "Members@odata.count": 1,
		"@Redfish.SupportedCertificates": []any{"PEM"}})
}

func (f *front) getHTTPSCert(w http.ResponseWriter, r *http.Request, path string, p principal) {
	cert, err := f.serverCert()
	if err != nil {
		f.log(r, p, path, "certificate.read", http.StatusInternalServerError)
		writeError(w, http.StatusInternalServerError, "Base.1.15.InternalError", err.Error())
		return
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Base.1.15.InternalError", err.Error())
		return
	}
	f.log(r, p, path, "certificate.read", http.StatusOK)
	writeJSON(w, http.StatusOK, certResource(path, "1", "HTTPS Certificate", leaf))
}

// generateCSR is CertificateService.GenerateCSR: the BMC makes a new key pair, keeps it pending
// and signs a CSR with it (bmcweb's default key, EC on secp384r1, when the request names none).
// AlternativeNames are taken as OpenSSL writes them, "IP: <addr>" or "DNS: <name>"; a bare
// value is a DNS name.
func (f *front) generateCSR(w http.ResponseWriter, r *http.Request, path string, p principal) {
	var body struct {
		CommonName, City, Country, Organization, OrganizationalUnit, State, KeyPairAlgorithm, KeyCurveId string
		AlternativeNames                                                                                 []string
		CertificateCollection                                                                            struct {
			ID string `json:"@odata.id"`
		}
	}
	if err := decode(r, &body); err != nil || body.CommonName == "" {
		f.log(r, p, path, "certificate.csr", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.PropertyMissing", "CommonName and CertificateCollection are required.")
		return
	}
	if coll := normalise(body.CertificateCollection.ID); coll != f.httpsURI() {
		f.log(r, p, path, "certificate.csr", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.ActionParameterNotSupported", "This BMC generates CSRs for its HTTPS certificate collection only.")
		return
	}
	curve := elliptic.P384()
	if body.KeyCurveId == "prime256v1" {
		curve = elliptic.P256()
	}
	if body.KeyPairAlgorithm != "" && body.KeyPairAlgorithm != "EC" {
		f.log(r, p, path, "certificate.csr", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.ActionParameterNotSupported", "KeyPairAlgorithm must be EC.")
		return
	}
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Base.1.15.InternalError", err.Error())
		return
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: body.CommonName}}
	for _, v := range []struct {
		s   string
		set *[]string
	}{{body.Country, &tmpl.Subject.Country}, {body.Organization, &tmpl.Subject.Organization}, {body.OrganizationalUnit, &tmpl.Subject.OrganizationalUnit},
		{body.State, &tmpl.Subject.Province}, {body.City, &tmpl.Subject.Locality}} {
		if v.s != "" {
			*v.set = []string{v.s}
		}
	}
	for _, an := range body.AlternativeNames {
		kind, val, ok := strings.Cut(an, ":")
		if !ok {
			tmpl.DNSNames = append(tmpl.DNSNames, strings.TrimSpace(an))
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToUpper(strings.TrimSpace(kind)) {
		case "IP":
			if ip := net.ParseIP(val); ip != nil {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			}
		default:
			tmpl.DNSNames = append(tmpl.DNSNames, val)
		}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Base.1.15.InternalError", err.Error())
		return
	}
	f.mu.Lock()
	f.pendingKey = key
	f.mu.Unlock()
	seq := f.log(r, p, path, "certificate.csr", http.StatusOK)
	f.commit("certificate.csr", nil, "pending", "CSR for CN="+body.CommonName+" with a new key pair held by the BMC", seq)
	writeJSON(w, http.StatusOK, map[string]any{"CSRString": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})),
		"CertificateCollection": map[string]any{"@odata.id": f.httpsURI()}})
}

// replaceCertificate is CertificateService.ReplaceCertificate for the HTTPS certificate (the
// certificate must match the key the BMC holds: the pending CSR key or the installed one) and for
// a Truststore certificate.
func (f *front) replaceCertificate(w http.ResponseWriter, r *http.Request, path string, p principal) {
	var body struct {
		CertificateString, CertificateType string
		CertificateURI                     struct {
			ID string `json:"@odata.id"`
		} `json:"CertificateUri"`
	}
	if err := decode(r, &body); err != nil || body.CertificateString == "" || body.CertificateURI.ID == "" {
		f.log(r, p, path, "certificate.replace", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.PropertyMissing", "CertificateString and CertificateUri are required.")
		return
	}
	chain, err := parsePEMChain(body.CertificateString)
	if err != nil {
		f.log(r, p, path, "certificate.replace", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.PropertyValueFormatError", "CertificateString is not a PEM certificate: "+err.Error())
		return
	}
	uri := normalise(body.CertificateURI.ID)
	switch {
	case uri == f.httpsURI()+"/1":
		f.mu.Lock()
		var key crypto.Signer
		if f.pendingKey != nil && sameKey(chain[0].PublicKey, &f.pendingKey.PublicKey) {
			key = f.pendingKey
		} else if f.installed != nil {
			if s, ok := f.installed.PrivateKey.(crypto.Signer); ok && sameKey(chain[0].PublicKey, s.Public()) {
				key = s
			}
		}
		if key == nil {
			f.mu.Unlock()
			f.log(r, p, path, "certificate.replace", http.StatusInternalServerError)
			// phosphor-certificate-manager refuses a certificate whose key it does not hold
			writeError(w, http.StatusInternalServerError, "Base.1.15.InternalError", "The certificate does not match a private key held by the BMC.")
			return
		}
		tc := tls.Certificate{PrivateKey: key, Leaf: chain[0]}
		for _, c := range chain {
			tc.Certificate = append(tc.Certificate, c.Raw)
		}
		old := "factory"
		if f.installed != nil {
			old = "installed"
		}
		f.installed, f.installedPM, f.pendingKey = &tc, pemOf(chain[0]), nil
		f.mu.Unlock()
		seq := f.log(r, p, path, "certificate.replace", http.StatusOK)
		f.commit("certificate.https", old, "installed", fmt.Sprintf("HTTPS certificate CN=%s issued by %s, sha256:%s", chain[0].Subject.CommonName, chain[0].Issuer.CommonName, fingerprint(chain[0])[:16]), seq)
		w.Header().Set("Location", uri)
		writeJSON(w, http.StatusOK, certResource(uri, "1", "HTTPS Certificate", chain[0]))
	case strings.HasPrefix(uri, f.trustURI()+"/"):
		id := uri[strings.LastIndex(uri, "/")+1:]
		f.mu.Lock()
		found := false
		for i := range f.trust {
			if f.trust[i].id == id {
				f.trust[i] = trustCert{id: id, pem: pemOf(chain[0]), cert: chain[0]}
				found = true
			}
		}
		f.mu.Unlock()
		if !found {
			f.log(r, p, path, "certificate.replace", http.StatusNotFound)
			writeError(w, http.StatusNotFound, "Base.1.15.ResourceNotFound", "No certificate at "+uri)
			return
		}
		seq := f.log(r, p, path, "certificate.replace", http.StatusOK)
		f.commit("certificate.truststore", nil, id, "replaced with "+chain[0].Subject.CommonName, seq)
		writeJSON(w, http.StatusOK, certResource(uri, id, "TrustStore Certificate", chain[0]))
	default:
		f.log(r, p, path, "certificate.replace", http.StatusBadRequest)
		writeError(w, http.StatusBadRequest, "Base.1.15.ActionParameterNotSupported", "CertificateUri "+uri+" is not a certificate this BMC replaces.")
	}
}

func sameKey(a, b crypto.PublicKey) bool {
	x, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && x.Equal(b)
}

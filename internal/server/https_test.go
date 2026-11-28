package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func selfSigned(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "orch.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour), DNSNames: []string{"orch.test"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}

func TestDashboardHTTPS(t *testing.T) {
	h := newHarness(t)
	cert, key := selfSigned(t)

	// A mismatched pair is refused and nothing starts.
	_, otherKey := selfSigned(t)
	if code := h.do("PUT", "/api/settings/https", adminToken, HTTPSConfig{Mode: "upload", Addr: "127.0.0.1:18446", CertPEM: cert, KeyPEM: otherKey}, nil); code != http.StatusBadRequest {
		t.Fatalf("mismatched pair: %d", code)
	}
	var v httpsView
	if code := h.do("PUT", "/api/settings/https", adminToken, HTTPSConfig{Mode: "upload", Addr: "127.0.0.1:18444", HSTS: true, CertPEM: cert, KeyPEM: key}, &v); code != http.StatusOK || !v.Listening || v.Subject != "orch.test" || v.Expires == nil {
		t.Fatalf("enable: %d %+v", code, v)
	}
	addr := h.srv.httpsAddr()
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // self-signed test cert
	resp, err := c.Get("https://" + addr + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// HSTS was requested but the self-signed certificate must keep it off.
	if resp.StatusCode != 200 || resp.Header.Get("Strict-Transport-Security") != "" {
		t.Fatalf("https response: %d hsts=%q", resp.StatusCode, resp.Header.Get("Strict-Transport-Security"))
	}
	// The saved certificate is never returned.
	var s struct {
		HTTPS map[string]any `json:"https"`
	}
	h.do("GET", "/api/settings", adminToken, nil, &s)
	if _, leaked := s.HTTPS["cert_pem"]; leaked || s.HTTPS["key_pem"] != nil {
		t.Fatal("settings exposes certificate material")
	}
	if code := h.do("PUT", "/api/settings/https", adminToken, HTTPSConfig{Mode: "off"}, nil); code != http.StatusOK {
		t.Fatalf("disable: %d", code)
	}
	if h.srv.httpsAddr() != "" {
		t.Fatal("listener still running")
	}
	if _, err := c.Get("https://" + addr + "/api/health"); err == nil {
		t.Fatal("old listener still answering")
	}
	if code := h.do("PUT", "/api/settings/https", adminToken, HTTPSConfig{Mode: "auto", Host: "bad host/"}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad host: %d", code)
	}
}

func TestHTTPSTestEndpointExplainsSelfSigned(t *testing.T) {
	h := newHarness(t)
	cert, key := selfSigned(t)
	if code := h.do("PUT", "/api/settings/https", adminToken, HTTPSConfig{Mode: "upload", Host: "", Addr: "127.0.0.1:18445", CertPEM: cert, KeyPEM: key}, nil); code != http.StatusOK {
		t.Fatalf("enable: %d", code)
	}
	var res httpsTestResult
	if code := h.do("POST", "/api/settings/https/test", adminToken, nil, &res); code != http.StatusOK {
		t.Fatalf("test: %d", code)
	}
	if !res.Handshake || !res.SelfSigned || res.Trusted || res.Subject != "orch.test" || len(res.SANs) != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	// The harness is reached by 127.0.0.1 or similar, which the certificate doesn't cover.
	if res.NameMatches {
		t.Fatalf("name should not match: %+v", res)
	}
	joined := ""
	for _, a := range res.Advice {
		joined += a + "\n"
	}
	if !strings.Contains(joined, "self-signed") || !strings.Contains(joined, "not valid for") {
		t.Fatalf("advice missing: %q", joined)
	}
	var v httpsView
	h.do("GET", "/api/settings", adminToken, nil, &struct {
		HTTPS *httpsView `json:"https"`
	}{&v})
	if !v.SelfSigned || v.Issuer != "orch.test" || len(v.SANs) != 1 {
		t.Fatalf("view: %+v", v)
	}
	// Without a Docker socket the server can't tell whether the port is published, and says so.
	if v.PortKnown || v.PublishNote == "" {
		t.Fatalf("publish state: %+v", v)
	}
	if code := h.do("POST", "/api/settings/https/publish-port", adminToken, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("publish without docker: %d", code)
	}
}

func TestCustomHTTPSPortValidation(t *testing.T) {
	h := newHarness(t)
	cert, key := selfSigned(t)
	for _, addr := range []string{":8080", ":0", ":70000", ":abc", ":80", "nonsense"} {
		if code := h.do("PUT", "/api/settings/https", adminToken, HTTPSConfig{Mode: "upload", Addr: addr, CertPEM: cert, KeyPEM: key}, nil); code != http.StatusBadRequest {
			t.Fatalf("%q accepted: %d", addr, code)
		}
	}
	// A custom port is accepted and is what the view reports (127.0.0.1 so it can bind).
	if code := h.do("PUT", "/api/settings/https", adminToken, HTTPSConfig{Mode: "upload", Addr: "127.0.0.1:18443", CertPEM: cert, KeyPEM: key}, nil); code != http.StatusOK {
		t.Fatalf("custom port refused: %d", code)
	}
	var v httpsView
	h.do("GET", "/api/settings", adminToken, nil, &struct {
		HTTPS *httpsView `json:"https"`
	}{&v})
	if v.ContainerPort != "18443" || v.Addr != "127.0.0.1:18443" {
		t.Fatalf("view ignores the custom port: %+v", v)
	}
}

func TestHSTSRefusedWhileCertificateUntrusted(t *testing.T) {
	h := newHarness(t)
	cert, key := selfSigned(t)
	var v httpsView
	if code := h.do("PUT", "/api/settings/https", adminToken, HTTPSConfig{Mode: "upload", Addr: "127.0.0.1:18447", HSTS: true, CertPEM: cert, KeyPEM: key}, &v); code != http.StatusOK {
		t.Fatalf("save: %d", code)
	}
	if v.HSTS || v.HSTSBlocked == "" || !strings.Contains(v.HSTSBlocked, "not valid for") {
		t.Fatalf("HSTS should be refused for a self-signed certificate that doesn't match the name: %+v", v)
	}
	if h.srv.hstsEnabled() {
		t.Fatal("HSTS header would be sent")
	}
}

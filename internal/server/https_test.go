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
	if code := h.do("PUT", "/api/settings/https", adminToken, HTTPSConfig{Mode: "upload", Addr: "127.0.0.1:0", CertPEM: cert, KeyPEM: otherKey}, nil); code != http.StatusBadRequest {
		t.Fatalf("mismatched pair: %d", code)
	}
	var v httpsView
	if code := h.do("PUT", "/api/settings/https", adminToken, HTTPSConfig{Mode: "upload", Addr: "127.0.0.1:0", HSTS: true, CertPEM: cert, KeyPEM: key}, &v); code != http.StatusOK || !v.Listening || v.Subject != "orch.test" || v.Expires == nil {
		t.Fatalf("enable: %d %+v", code, v)
	}
	addr := h.srv.httpsAddr()
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} // self-signed test cert
	resp, err := c.Get("https://" + addr + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Strict-Transport-Security") == "" {
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

package server

import (
	"context"
	"crypto/x509"
	"net/http"
	"testing"
	"time"
)

func leafExpiring(in time.Duration) *x509.Certificate {
	return &x509.Certificate{NotAfter: time.Now().Add(in)}
}

func TestAssessCert(t *testing.T) {
	day := 24 * time.Hour
	now := time.Now()
	cases := []struct {
		name, mode, renewErr, want string
		left                       time.Duration
	}{
		{"upload healthy", "upload", "", "ok", 90 * day},
		{"upload warning at 30 days", "upload", "", "warning", 29 * day},
		{"upload critical at 7 days", "upload", "", "critical", 6 * day},
		{"expired", "upload", "", "critical", -2 * day},
		{"auto healthy at 40 days", "auto", "", "ok", 40 * day},
		{"auto renewing normally at 20 days", "auto", "", "ok", 20 * day},
		{"auto not renewed at 13 days", "auto", "", "warning", 13 * day},
		{"auto critical at 4 days", "auto", "", "critical", 4 * day},
		{"renewal failure surfaces early", "auto", "rate limited", "warning", 25 * day},
		{"failure with plenty of time left is quiet", "auto", "rate limited", "ok", 60 * day},
	}
	for _, c := range cases {
		h := assessCert(c.mode, leafExpiring(c.left), c.renewErr, now)
		if h.Level != c.want {
			t.Errorf("%s: level %q, want %q (%+v)", c.name, h.Level, c.want, h)
		}
		if h.Level != "ok" && h.Message == "" {
			t.Errorf("%s: no message", c.name)
		}
	}
	if h := assessCert("upload", leafExpiring(-3*day), "", now); h.Days >= 0 {
		t.Errorf("expired certificate should report negative days: %+v", h)
	}
	if h := assessCert("auto", nil, "", now); h.Level != "ok" {
		t.Errorf("no certificate yet is not an expiry problem: %+v", h)
	}
}

func TestExpiringCertificateAlertsAndKeepsHTTPOpen(t *testing.T) {
	h := newHarness(t)
	cert, key := selfSignedWithLife(t, -time.Hour, 3*24*time.Hour) // ends in 3 days
	var v httpsView
	if code := h.do("PUT", "/api/settings/https", adminToken, HTTPSConfig{Mode: "upload", Addr: "127.0.0.1:18460", CertPEM: cert, KeyPEM: key}, &v); code != http.StatusOK {
		t.Fatalf("enable: %d", code)
	}
	if v.Expiry.Level != "critical" {
		t.Fatalf("view should grade the certificate: %+v", v.Expiry)
	}
	var alerts []Alert
	h.do("GET", "/api/alerts", adminToken, nil, &alerts)
	found := false
	for _, a := range alerts {
		found = found || a.ID == "https-cert" && a.Severity == "critical"
	}
	if !found {
		t.Fatalf("expected a certificate alert: %+v", alerts)
	}
}

func TestExpiredCertificateNeverRedirectsToHTTPS(t *testing.T) {
	h := newHarness(t)
	cert, key := selfSignedWithLife(t, -48*time.Hour, -time.Hour) // already expired
	if code := h.do("PUT", "/api/settings/https", adminToken, HTTPSConfig{Mode: "upload", Addr: "127.0.0.1:18461", CertPEM: cert, KeyPEM: key}, nil); code != http.StatusOK {
		t.Fatalf("enable: %d", code)
	}
	if !h.srv.certExpired() || h.srv.httpsRedirectSafe(context.Background()) {
		t.Fatal("an expired certificate must switch the redirect off so port 8080 stays usable")
	}
}

func TestResetHTTPS(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cert, key := selfSigned(t)
	b := marshalHTTPSConfig(HTTPSConfig{Mode: "upload", HSTS: true, CertPEM: cert, KeyPEM: key})
	h.st.SetSetting(ctx, settingHTTPS, b)
	h.st.SetSetting(ctx, settingForceHTTPS, "1")

	done, err := ResetHTTPS(ctx, h.st, false)
	if err != nil || len(done) != 2 {
		t.Fatalf("reset: %v %v", done, err)
	}
	cfg := h.srv.httpsConfig(ctx)
	if cfg.HSTS || cfg.Mode != "upload" || cfg.CertPEM == "" || h.srv.forceHTTPS(ctx) {
		t.Fatalf("HSTS and the redirect go off, the certificate and mode stay: %+v", cfg)
	}
	if done, _ := ResetHTTPS(ctx, h.st, false); len(done) != 0 {
		t.Fatalf("second run should change nothing: %v", done)
	}
	if _, err := ResetHTTPS(ctx, h.st, true); err != nil {
		t.Fatal(err)
	}
	if got := h.srv.httpsConfig(ctx); got.Mode != "off" {
		t.Fatalf("-off should switch HTTPS off: %+v", got)
	}
}

func TestRecoveryEnvAtBoot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.st.SetSetting(ctx, settingHTTPS, marshalHTTPSConfig(HTTPSConfig{Mode: "auto", Host: "orch.test", HSTS: true}))
	h.st.SetSetting(ctx, settingForceHTTPS, "1")
	t.Setenv("ORCH_HTTPS_RECOVERY", "1")
	h.srv.recoverHTTPSFromEnv(ctx)
	if cfg := h.srv.httpsConfig(ctx); cfg.HSTS || cfg.Mode != "auto" || h.srv.forceHTTPS(ctx) {
		t.Fatalf("recovery env should clear HSTS and the redirect only: %+v", cfg)
	}
}

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

// HTTPS for the dashboard itself. Off by default (plain HTTP, for use behind your own proxy
// or tunnel). With a certificate uploaded, or an automatic Let's Encrypt one, the server also
// listens for HTTPS on a second port and applies changes without a restart.

const settingHTTPS = "https_config"

type HTTPSConfig struct {
	Mode    string `json:"mode"` // off | upload | auto
	Host    string `json:"host,omitempty"`
	Addr    string `json:"addr,omitempty"` // listen address, default :8443
	HSTS    bool   `json:"hsts"`
	CertPEM string `json:"cert_pem,omitempty"`
	KeyPEM  string `json:"key_pem,omitempty"`
}

type httpsView struct {
	Mode      string     `json:"mode"`
	Host      string     `json:"host"`
	Addr      string     `json:"addr"`
	HSTS      bool       `json:"hsts"`
	Listening bool       `json:"listening"`
	Error     string     `json:"error,omitempty"`
	CertSet   bool       `json:"cert_set"`
	Subject   string     `json:"subject,omitempty"`
	Expires   *time.Time `json:"expires,omitempty"`
}

type httpsState struct {
	mu   sync.Mutex
	srv  *http.Server
	ln   net.Listener
	cfg  HTTPSConfig
	err  string
	leaf *x509.Certificate
}

const defaultHTTPSAddr = ":8443"

func (s *Server) httpsConfig(ctx context.Context) HTTPSConfig {
	c := HTTPSConfig{Mode: "off"}
	if v, err := s.store.GetSetting(ctx, settingHTTPS); err == nil {
		json.Unmarshal([]byte(v), &c)
	}
	if c.Addr == "" {
		c.Addr = defaultHTTPSAddr
	}
	return c
}

func parseCertPair(certPEM, keyPEM string) (tls.Certificate, *x509.Certificate, error) {
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return pair, nil, fmt.Errorf("the certificate and key don't match or can't be read: %w", err)
	}
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return pair, nil, errors.New("no certificate found in the PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return pair, nil, err
	}
	return pair, leaf, nil
}

// applyHTTPS starts, replaces or stops the HTTPS listener. On failure the previous listener
// keeps running.
func (s *Server) applyHTTPS(cfg HTTPSConfig) error {
	st := &s.https
	st.mu.Lock()
	defer st.mu.Unlock()
	var (
		tlsConf *tls.Config
		leaf    *x509.Certificate
	)
	switch cfg.Mode {
	case "off", "":
	case "upload":
		pair, l, err := parseCertPair(cfg.CertPEM, cfg.KeyPEM)
		if err != nil {
			return err
		}
		leaf = l
		tlsConf = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	case "auto":
		if cfg.Host == "" || strings.ContainsAny(cfg.Host, "/: ") {
			return errors.New("enter the hostname the dashboard is reached at, such as orch.example.com")
		}
		dir := filepath.Join(os.TempDir(), "orchestrator-certs")
		if s.cfg.DBPath != "" {
			dir = filepath.Join(filepath.Dir(s.cfg.DBPath), "certs")
		}
		m := &autocert.Manager{Prompt: autocert.AcceptTOS, HostPolicy: autocert.HostWhitelist(cfg.Host), Cache: autocert.DirCache(dir)}
		tlsConf = m.TLSConfig()
		tlsConf.MinVersion = tls.VersionTLS12
	default:
		return errors.New("unknown HTTPS mode")
	}
	if st.srv != nil {
		st.srv.Close()
		st.srv, st.ln = nil, nil
	}
	st.cfg, st.leaf, st.err = cfg, leaf, ""
	if tlsConf == nil {
		return nil
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		st.cfg.Mode = "off"
		st.err = "could not listen on " + cfg.Addr + ": " + err.Error()
		return errors.New(st.err)
	}
	srv := &http.Server{Handler: s.Handler(), TLSConfig: tlsConf, ReadHeaderTimeout: 10 * time.Second}
	st.srv, st.ln = srv, ln
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("https server", "err", err)
		}
	}()
	return nil
}

func (s *Server) httpsAddr() string {
	s.https.mu.Lock()
	defer s.https.mu.Unlock()
	if s.https.ln == nil {
		return ""
	}
	return s.https.ln.Addr().String()
}

// startHTTPS brings the saved HTTPS setup up at boot.
func (s *Server) startHTTPS(ctx context.Context) {
	cfg := s.httpsConfig(ctx)
	if cfg.Mode == "off" {
		s.https.mu.Lock()
		s.https.cfg = cfg
		s.https.mu.Unlock()
		return
	}
	if err := s.applyHTTPS(cfg); err != nil {
		s.log.Error("dashboard https", "err", err)
		s.https.mu.Lock()
		s.https.err = err.Error()
		s.https.mu.Unlock()
	}
}

func (s *Server) httpsView(ctx context.Context) httpsView {
	saved := s.httpsConfig(ctx)
	st := &s.https
	st.mu.Lock()
	defer st.mu.Unlock()
	v := httpsView{Mode: saved.Mode, Host: saved.Host, Addr: saved.Addr, HSTS: saved.HSTS, Listening: st.ln != nil, Error: st.err, CertSet: saved.CertPEM != ""}
	if st.leaf != nil {
		v.Subject = st.leaf.Subject.CommonName
		exp := st.leaf.NotAfter
		v.Expires = &exp
	}
	return v
}

func (s *Server) handleSetHTTPS(w http.ResponseWriter, r *http.Request) {
	var req HTTPSConfig
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	old := s.httpsConfig(ctx)
	req.Host = strings.ToLower(strings.TrimSpace(req.Host))
	req.Addr = strings.TrimSpace(req.Addr)
	if req.Addr == "" {
		req.Addr = defaultHTTPSAddr
	}
	if req.Mode == "upload" && req.CertPEM == "" && req.KeyPEM == "" {
		req.CertPEM, req.KeyPEM = old.CertPEM, old.KeyPEM
	}
	if req.Mode == "upload" && req.KeyPEM == "" {
		req.KeyPEM = old.KeyPEM
	}
	if req.Mode != "upload" {
		req.CertPEM, req.KeyPEM = "", ""
	}
	if err := s.applyHTTPS(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	b, _ := json.Marshal(req)
	if err := s.store.SetSetting(ctx, settingHTTPS, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	s.event(ctx, "", "info", "settings.https", "Dashboard HTTPS set to "+req.Mode)
	writeJSON(w, http.StatusOK, s.httpsView(ctx))
}

// redirectTarget is the HTTPS address for a plain-HTTP request.
func (s *Server) redirectTarget(r *http.Request) string {
	s.https.mu.Lock()
	cfg, on := s.https.cfg, s.https.ln != nil
	s.https.mu.Unlock()
	host := r.Host
	if on {
		h, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			h = r.Host
		}
		if cfg.Mode == "auto" && cfg.Host != "" {
			h = cfg.Host
		}
		_, port, _ := net.SplitHostPort(cfg.Addr)
		if port == "" || port == "443" {
			host = h
		} else {
			host = net.JoinHostPort(h, port)
		}
	}
	return "https://" + host + r.URL.RequestURI()
}

func (s *Server) hstsEnabled() bool {
	s.https.mu.Lock()
	defer s.https.mu.Unlock()
	return s.https.cfg.HSTS
}

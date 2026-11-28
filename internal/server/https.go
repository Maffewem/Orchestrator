package server

import (
	"bytes"
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

	"github.com/Maffewem/Orchestrator/internal/selfupdate"
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
	Challenge string     `json:"challenge,omitempty"` // "dns" when Cloudflare DNS validation is used

	Issuer     string     `json:"issuer,omitempty"`
	SANs       []string   `json:"sans,omitempty"`
	SelfSigned bool       `json:"self_signed,omitempty"`
	NotBefore  *time.Time `json:"not_before,omitempty"`

	// Publishing: the HTTPS listener runs inside the server container, so Docker has to
	// publish its port. ContainerPort is the port of Addr; PublishedHost is the host port it
	// is mapped to ("" when it is not published). PortKnown is false when the server cannot
	// inspect its own container (no Docker socket), so nothing can be said either way.
	ContainerPort string `json:"container_port,omitempty"`
	PortKnown     bool   `json:"port_known"`
	PublishedHost string `json:"published_host,omitempty"`
	CanPublish    bool   `json:"can_publish"`
	PublishNote   string `json:"publish_note,omitempty"`
}

type httpsState struct {
	mu   sync.Mutex
	srv  *http.Server
	ln   net.Listener
	cfg  HTTPSConfig
	err  string
	leaf *x509.Certificate

	dns       *dnsCertManager // set while the DNS challenge is used
	cancelDNS context.CancelFunc
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
		tlsConf   *tls.Config
		leaf      *x509.Certificate
		dnsMgr    *dnsCertManager
		cancelDNS context.CancelFunc
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
		if s.sslMode(context.Background()) == sslDNS {
			dm := newDNSCertManager(s, cfg.Host, dir, s.cloudflareToken(context.Background()))
			dctx, dcancel := context.WithCancel(context.Background())
			cancelDNS = dcancel
			go dm.run(dctx)
			tlsConf = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: dm.getCertificate}
			dnsMgr = dm
			break
		}
		m := &autocert.Manager{Prompt: autocert.AcceptTOS, HostPolicy: autocert.HostWhitelist(cfg.Host), Cache: autocert.DirCache(dir)}
		tlsConf = m.TLSConfig()
		tlsConf.MinVersion = tls.VersionTLS12
	default:
		return errors.New("unknown HTTPS mode")
	}
	if st.cancelDNS != nil {
		st.cancelDNS()
		st.cancelDNS, st.dns = nil, nil
	}
	if st.srv != nil {
		st.srv.Close()
		st.srv, st.ln = nil, nil
	}
	st.cfg, st.leaf, st.err = cfg, leaf, ""
	st.cancelDNS, st.dns = cancelDNS, dnsMgr
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
	v := s.httpsViewBase(ctx)
	if v.Mode != "off" {
		s.fillPublishState(ctx, &v)
	}
	return v
}

func (s *Server) httpsViewBase(ctx context.Context) httpsView {
	saved := s.httpsConfig(ctx)
	st := &s.https
	st.mu.Lock()
	defer st.mu.Unlock()
	v := httpsView{Mode: saved.Mode, Host: saved.Host, Addr: saved.Addr, HSTS: saved.HSTS, Listening: st.ln != nil, Error: st.err, CertSet: saved.CertPEM != ""}
	if st.dns != nil {
		v.Challenge = "dns"
		l, e := st.dns.status()
		if l != nil {
			st.leaf = l
		}
		if e != "" && v.Error == "" {
			v.Error = "Certificate not issued yet: " + e
		}
	}
	if st.leaf != nil {
		l := st.leaf
		v.Subject = l.Subject.CommonName
		exp, nb := l.NotAfter, l.NotBefore
		v.Expires, v.NotBefore = &exp, &nb
		v.Issuer = l.Issuer.CommonName
		if v.Issuer == "" && len(l.Issuer.Organization) > 0 {
			v.Issuer = l.Issuer.Organization[0]
		}
		v.SANs = certNames(l)
		v.SelfSigned = isSelfSigned(l)
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

// certNames lists the host names and addresses a certificate is valid for.
func certNames(c *x509.Certificate) []string {
	out := append([]string{}, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	return out
}

// isSelfSigned reports whether the certificate signed itself (so no browser trusts it unless
// it is added by hand).
func isSelfSigned(c *x509.Certificate) bool {
	return bytes.Equal(c.RawIssuer, c.RawSubject) && c.CheckSignature(c.SignatureAlgorithm, c.RawTBSCertificate, c.Signature) == nil
}

// portOf is the port of a listen address such as ":8443" or "0.0.0.0:8443".
func portOf(addr string) string {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return p
}

// fillPublishState works out whether Docker publishes the HTTPS port of the server container.
func (s *Server) fillPublishState(ctx context.Context, v *httpsView) {
	v.ContainerPort = portOf(v.Addr)
	self, reason := s.selfInfo(ctx)
	if self == nil {
		v.PublishNote = "This server can't inspect its own container (" + reason + "), so it can't tell whether port " + v.ContainerPort + " is published or publish it for you."
		return
	}
	v.PortKnown = true
	v.PublishedHost = self.Ports[v.ContainerPort+"/tcp"]
	v.CanPublish = v.PublishedHost == ""
	if v.PublishedHost == "" {
		v.PublishNote = "The server container does not publish port " + v.ContainerPort + ", so browsers get connection refused or an SSL error. Publish it below (the dashboard restarts for a few seconds), or re-create the container with -p " + v.ContainerPort + ":" + v.ContainerPort + "."
	}
}

// httpsRedirectSafe reports whether sending browsers to the HTTPS listener can work: it is
// running and (as far as the server can tell) Docker publishes its port. Otherwise the
// redirect is skipped so plain HTTP keeps working and nobody is locked out.
func (s *Server) httpsRedirectSafe(ctx context.Context) bool {
	s.https.mu.Lock()
	on, addr := s.https.ln != nil, s.https.cfg.Addr
	s.https.mu.Unlock()
	if !on {
		return false
	}
	if self, _ := s.selfInfo(ctx); self != nil {
		return self.Ports[portOf(addr)+"/tcp"] != ""
	}
	return true
}

type httpsTestResult struct {
	Listening     bool       `json:"listening"`
	Handshake     bool       `json:"handshake"`
	Error         string     `json:"error,omitempty"`
	Subject       string     `json:"subject,omitempty"`
	Issuer        string     `json:"issuer,omitempty"`
	SANs          []string   `json:"sans,omitempty"`
	SelfSigned    bool       `json:"self_signed"`
	Expires       *time.Time `json:"expires,omitempty"`
	Expired       bool       `json:"expired"`
	TestedName    string     `json:"tested_name"`
	NameMatches   bool       `json:"name_matches"`
	Trusted       bool       `json:"trusted"` // chains to a CA this server trusts
	TrustError    string     `json:"trust_error,omitempty"`
	PortPublished bool       `json:"port_published"`
	PortKnown     bool       `json:"port_known"`
	Advice        []string   `json:"advice"`
}

// handleTestHTTPS connects to the dashboard's own HTTPS listener from inside the server and
// reports what a browser would see, in terms an administrator can act on.
func (s *Server) handleTestHTTPS(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	view := s.httpsView(ctx)
	res := httpsTestResult{Listening: view.Listening, PortKnown: view.PortKnown, PortPublished: view.PublishedHost != ""}
	name := view.Host
	if name == "" {
		name = r.Host
		if h, _, err := net.SplitHostPort(name); err == nil {
			name = h
		}
	}
	res.TestedName = name
	switch {
	case view.Mode == "off":
		res.Advice = append(res.Advice, "Dashboard HTTPS is off, so only plain HTTP is served. Nothing is wrong: choose a mode above if you want HTTPS.")
	case !view.Listening:
		res.Error = view.Error
		res.Advice = append(res.Advice, "The HTTPS listener is not running. Fix the error shown, then save again.")
	default:
		port := portOf(s.httpsAddr()) // the real port, also when the setting says :0
		d := &net.Dialer{Timeout: 5 * time.Second}
		conn, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort("127.0.0.1", port), &tls.Config{InsecureSkipVerify: true, ServerName: name})
		if err != nil {
			res.Error = err.Error()
			res.Advice = append(res.Advice, "The listener accepted no TLS handshake. In automatic mode the certificate may not be issued yet (see the error above).")
		} else {
			defer conn.Close()
			res.Handshake = true
			certs := conn.ConnectionState().PeerCertificates
			if len(certs) > 0 {
				leaf := certs[0]
				res.Subject, res.SANs, res.SelfSigned = leaf.Subject.CommonName, certNames(leaf), isSelfSigned(leaf)
				exp := leaf.NotAfter
				res.Expires, res.Expired = &exp, time.Now().After(exp)
				if leaf.Issuer.CommonName != "" {
					res.Issuer = leaf.Issuer.CommonName
				} else if len(leaf.Issuer.Organization) > 0 {
					res.Issuer = leaf.Issuer.Organization[0]
				}
				res.NameMatches = leaf.VerifyHostname(name) == nil
				pool := x509.NewCertPool()
				for _, c := range certs[1:] {
					pool.AddCert(c)
				}
				if _, verr := leaf.Verify(x509.VerifyOptions{DNSName: name, Intermediates: pool}); verr != nil {
					res.TrustError = verr.Error()
				} else {
					res.Trusted = true
				}
			}
		}
		res.Advice = append(res.Advice, httpsAdvice(res, view)...)
	}
	writeJSON(w, http.StatusOK, res)
}

// httpsAdvice turns a test result into plain-language next steps.
func httpsAdvice(res httpsTestResult, view httpsView) []string {
	var out []string
	if view.PortKnown && view.PublishedHost == "" {
		out = append(out, "Port "+view.ContainerPort+" is not published by Docker, so browsers cannot reach the HTTPS listener. Use \"Publish port\" below.")
	}
	if !res.Handshake {
		return out
	}
	if res.Expired {
		out = append(out, "The certificate has expired. Upload a new one, or in automatic mode check the error above.")
	}
	if !res.NameMatches {
		out = append(out, "The certificate is not valid for "+res.TestedName+" (it covers: "+strings.Join(res.SANs, ", ")+"). Browsers show a name-mismatch error. Open the dashboard using a name on the certificate, not the server's IP address, or issue a certificate that includes the name or address you use.")
	}
	if res.SelfSigned {
		out = append(out, "The certificate is self-signed. A browser warning (\"Your connection is not private\") is expected and safe to click through for your own server, or install the certificate on your devices as trusted. To remove the warning for good, use a certificate from a real authority: choose the automatic option with a hostname you own (Cloudflare DNS validation works for names that are only reachable on your LAN).")
	} else if !res.Trusted && res.NameMatches && !res.Expired {
		out = append(out, "The certificate does not chain to a public authority this server trusts ("+res.TrustError+"). That is normal for a private CA: install your CA on the devices that open the dashboard, or use a certificate from a public authority.")
	}
	if res.Trusted && res.NameMatches && !res.Expired {
		out = append(out, "The certificate is valid for "+res.TestedName+". If a browser still shows an error, check that you are opening https://"+res.TestedName+":"+portOrDefault(view)+" (not https on the plain HTTP port 8080).")
	}
	return out
}

func portOrDefault(v httpsView) string {
	if v.PublishedHost != "" {
		return v.PublishedHost
	}
	return v.ContainerPort
}

// handlePublishHTTPSPort re-creates the server container so it also publishes the HTTPS port.
// Everything else about the container is kept, and the previous container stays as
// <name>-previous. If the new one doesn't come up healthy the old one is put back.
func (s *Server) handlePublishHTTPSPort(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := s.httpsConfig(ctx)
	port := portOf(cfg.Addr)
	if port == "" {
		writeError(w, http.StatusBadRequest, "the HTTPS listen address "+cfg.Addr+" has no port")
		return
	}
	self, reason := s.selfInfo(ctx)
	if self == nil {
		writeError(w, http.StatusBadRequest, "this server can't re-create its own container: "+reason)
		return
	}
	if self.Ports[port+"/tcp"] != "" {
		writeError(w, http.StatusConflict, "port "+port+" is already published as host port "+self.Ports[port+"/tcp"])
		return
	}
	if !s.begin("publish-port") {
		writeError(w, http.StatusConflict, "another server update or restart is already running")
		return
	}
	s.event(ctx, "", "info", "settings.https", "Publishing the dashboard HTTPS port "+port+"; the dashboard restarts shortly")
	if err := selfupdate.StartHelper(ctx, s.docker, self, self.ImageID, port+":"+port); err != nil {
		s.finish()
		writeError(w, http.StatusBadGateway, "could not start the updater: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "restarting", "port": port})
}

package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

// Certificate expiry watch for the dashboard's own HTTPS listener.
//
// An expired certificate under HSTS is the worst case for this dashboard: browsers refuse the
// page and offer no click-through. So the expiry is watched well ahead of time, shown on the
// HTTPS settings and the alerts bell, and logged as an event when the level changes. Plain HTTP
// on the dashboard port stays available as the way back in (see ResetHTTPS and the README).

const (
	certWarnUpload   = 30 * 24 * time.Hour // an uploaded certificate is never renewed by the server
	certCriticalDays = 7 * 24 * time.Hour
	certWarnAuto     = 14 * 24 * time.Hour // automatic renewal starts at 30 days left, so 14 means it is failing
	certCritAuto     = 5 * 24 * time.Hour
	certWatchEvery   = 6 * time.Hour
)

// certHealth is how worried to be about the served certificate.
type certHealth struct {
	Level   string `json:"level"` // ok | warning | critical
	Days    int    `json:"days"`  // whole days left, negative once expired
	Message string `json:"message,omitempty"`
}

// assessCert grades a certificate by its remaining life and, for automatic modes, by whether the
// last renewal attempt failed. renewErr is "" when there is no known failure.
func assessCert(mode string, leaf *x509.Certificate, renewErr string, now time.Time) certHealth {
	if leaf == nil {
		return certHealth{Level: "ok"}
	}
	left := leaf.NotAfter.Sub(now)
	h := certHealth{Level: "ok", Days: int(left.Hours() / 24)}
	warn, crit := certWarnUpload, certCriticalDays
	if mode == "auto" {
		warn, crit = certWarnAuto, certCritAuto
	}
	switch {
	case left <= 0:
		h.Level = "critical"
		h.Days = -int((-left).Hours() / 24)
		h.Message = "The dashboard certificate expired on " + leaf.NotAfter.Format("2 Jan 2006") + ". Browsers refuse HTTPS and, with HSTS on, cannot click through. Use plain HTTP by IP address (port 8080) to fix it."
	case left <= crit:
		h.Level = "critical"
	case left <= warn:
		h.Level = "warning"
	}
	if h.Level != "ok" && left > 0 {
		if mode == "auto" {
			h.Message = fmt.Sprintf("The dashboard certificate expires in %d days and has not renewed, although automatic renewal should have started at 30 days.", h.Days)
		} else {
			h.Message = fmt.Sprintf("The uploaded dashboard certificate expires in %d days. Uploaded certificates are not renewed automatically: upload a new one.", h.Days)
		}
	}
	if renewErr != "" && h.Level == "ok" && left <= 30*24*time.Hour {
		h.Level = "warning"
		h.Message = "Renewing the dashboard certificate failed: " + renewErr
	} else if renewErr != "" && h.Message != "" {
		h.Message += " Last renewal error: " + renewErr
	}
	return h
}

// servedLeaf is the certificate the HTTPS listener currently serves, or nil when there is none
// known. Uploaded and DNS-challenge certificates are known directly; for the TLS-ALPN (autocert)
// mode the certificate is read from the listener itself.
func (s *Server) servedLeaf() (*x509.Certificate, string) {
	st := &s.https
	st.mu.Lock()
	defer st.mu.Unlock()
	leaf, errMsg := st.leaf, ""
	if st.dns != nil {
		l, e := st.dns.status()
		if l != nil {
			leaf = l
		}
		errMsg = e
	}
	if leaf == nil {
		leaf = st.autoLeaf
	}
	return leaf, errMsg
}

// certExpired reports whether the served certificate is known to be past its end date.
func (s *Server) certExpired() bool {
	leaf, _ := s.servedLeaf()
	return leaf != nil && time.Now().After(leaf.NotAfter)
}

// certHealth is the current grade of the dashboard certificate ("ok" while HTTPS is off).
func (s *Server) certHealth() certHealth {
	s.https.mu.Lock()
	mode, on := s.https.cfg.Mode, s.https.ln != nil
	s.https.mu.Unlock()
	if !on {
		return certHealth{Level: "ok"}
	}
	leaf, renewErr := s.servedLeaf()
	return assessCert(mode, leaf, renewErr, time.Now())
}

// refreshAutoLeaf reads the certificate the listener serves for the configured host. In automatic
// mode this also nudges the certificate manager to renew a certificate that is close to its end.
func (s *Server) refreshAutoLeaf() {
	s.https.mu.Lock()
	cfg, ln := s.https.cfg, s.https.ln
	s.https.mu.Unlock()
	if ln == nil || cfg.Mode != "auto" || cfg.Host == "" {
		return
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", net.JoinHostPort("127.0.0.1", portOf(ln.Addr().String())), &tls.Config{InsecureSkipVerify: true, ServerName: cfg.Host})
	if err != nil {
		return
	}
	defer conn.Close()
	if certs := conn.ConnectionState().PeerCertificates; len(certs) > 0 {
		s.https.mu.Lock()
		s.https.autoLeaf = certs[0]
		s.https.mu.Unlock()
	}
}

// certWatchLoop re-checks the certificate every few hours and records an event whenever its grade
// changes, so a failing renewal shows up in the activity feed and the alerts bell long before the
// certificate runs out.
func (s *Server) certWatchLoop(ctx context.Context) {
	last := "ok"
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.refreshAutoLeaf()
		if h := s.certHealth(); h.Level != last {
			switch h.Level {
			case "ok":
				s.event(ctx, "", "info", "settings.https", "The dashboard certificate is healthy again")
			default:
				lvl := "warning"
				if h.Level == "critical" {
					lvl = "error"
				}
				s.event(ctx, "", lvl, "settings.https", h.Message)
			}
			last = h.Level
		}
		t.Reset(certWatchEvery)
	}
}

// ResetHTTPS is the way back in after an HTTPS lockout (an expired certificate under HSTS, a
// redirect to a port that no longer works). It switches off HSTS and the HTTP to HTTPS redirect,
// and, with off set, dashboard HTTPS altogether. Plain HTTP on the dashboard port is untouched.
// It returns what it changed. The running server reads HSTS at start, so restart it afterwards.
func ResetHTTPS(ctx context.Context, st *store.Store, off bool) ([]string, error) {
	var done []string
	if v, err := st.GetSetting(ctx, settingForceHTTPS); err == nil && v != "" && v != "0" {
		if err := st.SetSetting(ctx, settingForceHTTPS, "0"); err != nil {
			return done, err
		}
		done = append(done, "turned off the HTTP to HTTPS redirect")
	}
	raw, err := st.GetSetting(ctx, settingHTTPS)
	if err != nil || raw == "" {
		return done, nil
	}
	cfg := HTTPSConfig{Mode: "off"}
	if err := unmarshalHTTPSConfig(raw, &cfg); err != nil {
		return done, err
	}
	changed := false
	if cfg.HSTS {
		cfg.HSTS, changed = false, true
		done = append(done, "turned off HSTS")
	}
	if off && cfg.Mode != "off" && cfg.Mode != "" {
		cfg.Mode, changed = "off", true
		done = append(done, "turned off dashboard HTTPS")
	}
	if changed {
		if err := st.SetSetting(ctx, settingHTTPS, marshalHTTPSConfig(cfg)); err != nil {
			return done, err
		}
	}
	return done, nil
}

// recoverHTTPSFromEnv applies ORCH_HTTPS_RECOVERY at boot: "1" turns HSTS and the redirect off,
// "off" also turns dashboard HTTPS off. It lets someone without shell access to the database
// (for example only able to edit the container's environment) get back in.
func (s *Server) recoverHTTPSFromEnv(ctx context.Context) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("ORCH_HTTPS_RECOVERY")))
	if v == "" || v == "0" {
		return
	}
	done, err := ResetHTTPS(ctx, s.store, v == "off")
	if err != nil {
		s.log.Error("ORCH_HTTPS_RECOVERY", "err", err)
		return
	}
	if len(done) > 0 {
		msg := "ORCH_HTTPS_RECOVERY is set: " + strings.Join(done, ", ") + ". Remove the variable once you are back in."
		s.log.Warn(msg)
		s.event(ctx, "", "warning", "settings.https", msg)
	}
}

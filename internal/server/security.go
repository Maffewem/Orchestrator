package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	settingSessionHours = "auth_session_hours"
	settingMinPassword  = "auth_min_password"
	settingForceHTTPS   = "https_redirect"
	settingAgentsHTTPS  = "https_agents"

	minSessionHours = 1
	maxSessionHours = 24 * 90
	minPasswordCap  = 8
	maxPasswordCap  = 64
)

// sessionLifetime is how long a new sign-in lasts; the default is sessionTTL.
func (s *Server) sessionLifetime(ctx context.Context) time.Duration {
	if v, err := s.store.GetSetting(ctx, settingSessionHours); err == nil {
		if h, err := strconv.Atoi(v); err == nil && h >= minSessionHours && h <= maxSessionHours {
			return time.Duration(h) * time.Hour
		}
	}
	return sessionTTL
}

// minPasswordLen is the shortest password accepted for new or changed passwords.
func (s *Server) minPasswordLen(ctx context.Context) int {
	if v, err := s.store.GetSetting(ctx, settingMinPassword); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n >= minPasswordCap && n <= maxPasswordCap {
			return n
		}
	}
	return minPassword
}

func (s *Server) forceHTTPS(ctx context.Context) bool {
	v, _ := s.store.GetSetting(ctx, settingForceHTTPS)
	return v == "1"
}

func (s *Server) requireAgentHTTPS(ctx context.Context) bool {
	v, _ := s.store.GetSetting(ctx, settingAgentsHTTPS)
	return v == "1"
}

type securityView struct {
	SessionHours int  `json:"session_hours"`
	MinPassword  int  `json:"min_password"`
	ForceHTTPS   bool `json:"force_https"`
	// RequireHTTPSAgents makes the server refuse agents that connect over plain HTTP.
	RequireHTTPSAgents bool `json:"require_https_agents"`
}

func (s *Server) securityView(ctx context.Context) securityView {
	return securityView{
		SessionHours: int(s.sessionLifetime(ctx) / time.Hour),
		MinPassword:  s.minPasswordLen(ctx),
		ForceHTTPS:   s.forceHTTPS(ctx),

		RequireHTTPSAgents: s.requireAgentHTTPS(ctx),
	}
}

// handleSetSecurity saves session lifetime, minimum password length and the HTTPS redirect.
func (s *Server) handleSetSecurity(w http.ResponseWriter, r *http.Request) {
	var req securityView
	if !decode(w, r, &req) {
		return
	}
	if req.SessionHours < minSessionHours || req.SessionHours > maxSessionHours {
		writeError(w, http.StatusBadRequest, "session lifetime must be between 1 hour and 90 days")
		return
	}
	if req.MinPassword < minPasswordCap || req.MinPassword > maxPasswordCap {
		writeError(w, http.StatusBadRequest, "minimum password length must be between 8 and 64")
		return
	}
	// Refuse a redirect that would lock this very request out: the admin must be on HTTPS already.
	if req.ForceHTTPS && !requestIsHTTPS(r) {
		writeError(w, http.StatusConflict, "open the dashboard over HTTPS first, then turn the redirect on")
		return
	}
	ctx := r.Context()
	if req.RequireHTTPSAgents && !s.requireAgentHTTPS(ctx) {
		// Agents installed with an http:// URL would be locked out, so list them first.
		devs, _ := s.store.ListDevices(ctx)
		var plain []string
		for _, d := range devs {
			if !d.Offline && s.agentLink(d.ID) != "https" {
				plain = append(plain, d.Name)
			}
		}
		if len(plain) > 0 {
			writeError(w, http.StatusConflict, "these devices still connect over plain HTTP and would be cut off: "+strings.Join(plain, ", ")+". Re-run the install command from the HTTPS dashboard on each, then try again")
			return
		}
	}
	agentsHTTPS := "0"
	if req.RequireHTTPSAgents {
		agentsHTTPS = "1"
	}
	force := "0"
	if req.ForceHTTPS {
		force = "1"
	}
	for k, v := range map[string]string{
		settingSessionHours: strconv.Itoa(req.SessionHours),
		settingMinPassword:  strconv.Itoa(req.MinPassword),
		settingForceHTTPS:   force,
		settingAgentsHTTPS:  agentsHTTPS,
	} {
		if err := s.store.SetSetting(ctx, k, v); err != nil {
			s.internal(w, err)
			return
		}
	}
	s.event(ctx, "", "info", "settings.security", "Security settings updated")
	writeJSON(w, http.StatusOK, s.securityView(ctx))
}

func requestIsHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// httpsRedirect sends plain-HTTP browser requests to HTTPS when the setting is on. Agents,
// webhooks, the installer and the health check are exempt so they keep working on any URL.
func (s *Server) httpsRedirect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		exempt := strings.HasPrefix(p, "/api/agent/") || strings.HasPrefix(p, "/api/github/") ||
			strings.HasPrefix(p, "/downloads/") || p == "/install.sh" || p == "/api/health"
		if strings.HasPrefix(p, "/api/agent/") && !requestIsHTTPS(r) && s.requireAgentHTTPS(r.Context()) {
			writeError(w, http.StatusUpgradeRequired, "this server only accepts agents over HTTPS: re-run the install command from the HTTPS dashboard")
			return
		}
		if !exempt && !requestIsHTTPS(r) && s.forceHTTPS(r.Context()) {
			http.Redirect(w, r, s.redirectTarget(r), http.StatusMovedPermanently)
			return
		}
		if r.TLS != nil && s.hstsEnabled() {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// noteAgentLink remembers whether a device's last heartbeat arrived over HTTPS.
func (s *Server) noteAgentLink(deviceID string, r *http.Request) {
	s.links.Store(deviceID, requestIsHTTPS(r))
}

// agentLink is "https", "http", or "unknown" before the device has reported since the server started.
func (s *Server) agentLink(deviceID string) string {
	v, ok := s.links.Load(deviceID)
	switch {
	case !ok:
		return "unknown"
	case v.(bool):
		return "https"
	}
	return "http"
}

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

const (
	settingCloudflareToken   = "cloudflare_token"
	settingCloudflareProxied = "cloudflare_proxied"
	settingSSLMode           = "ssl_mode"
)

const (
	sslHTTP = "http" // Caddy's default: HTTP / TLS-ALPN challenges, needs ports 80 and 443 reachable
	sslDNS  = "dns"  // DNS-01 challenge through Cloudflare: works behind the Cloudflare proxy and without port 80
)

func (s *Server) cloudflareToken(ctx context.Context) string {
	v, _ := s.store.GetSetting(ctx, settingCloudflareToken)
	return v
}

func (s *Server) cloudflareProxied(ctx context.Context) bool {
	v, _ := s.store.GetSetting(ctx, settingCloudflareProxied)
	return v == "1"
}

func (s *Server) sslMode(ctx context.Context) string {
	if v, _ := s.store.GetSetting(ctx, settingSSLMode); v == sslDNS && s.cloudflareToken(ctx) != "" {
		return sslDNS
	}
	return sslHTTP
}

// sslDNSToken is the token to hand to a device's proxy, or "" when certificates should use
// the default challenge.
func (s *Server) sslDNSToken(ctx context.Context) string {
	if s.sslMode(ctx) == sslDNS {
		return s.cloudflareToken(ctx)
	}
	return ""
}

func (s *Server) cloudflareStatus(ctx context.Context) map[string]any {
	_, acct := s.cfDeployCreds(ctx)
	own, _ := s.store.GetSetting(ctx, settingCFDeployToken)
	return map[string]any{"connected": s.cloudflareToken(ctx) != "", "proxied": s.cloudflareProxied(ctx),
		"deploy": map[string]any{"configured": s.cfDeployReady(ctx), "account_id": acct, "own_token": own != ""}}
}

// withDNSChallenge makes Caddy obtain certificates with the Cloudflare DNS-01 challenge. The
// token reaches Caddy through the CF_API_TOKEN environment variable, never the Caddyfile.
func withDNSChallenge(caddyfile string) string {
	return strings.Replace(caddyfile, "{\n\tadmin off\n", "{\n\tadmin off\n\tacme_dns cloudflare {env.CF_API_TOKEN}\n", 1)
}

// cfError is a failed Cloudflare API call; Status is the HTTP status (0 when none arrived).
type cfError struct {
	Status int
	Msg    string
}

func (e *cfError) Error() string { return "cloudflare: " + e.Msg }

// cfCall performs one Cloudflare API request with a JSON body and decodes result into out.
func (s *Server) cfCall(ctx context.Context, token, method, path string, body, out any) error {
	var rdr io.Reader
	ct := ""
	if body != nil {
		b, _ := json.Marshal(body)
		rdr, ct = bytes.NewReader(b), "application/json"
	}
	return s.cfRaw(ctx, 15*time.Second, token, method, path, ct, rdr, out)
}

// cfRaw performs one Cloudflare API request with an arbitrary body (uploads are multipart).
func (s *Server) cfRaw(ctx context.Context, timeout time.Duration, token, method, path, contentType string, body io.Reader, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, s.cloudflareAPI+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &cfError{Msg: err.Error()}
	}
	defer resp.Body.Close()
	var env struct {
		Success bool                       `json:"success"`
		Errors  []struct{ Message string } `json:"errors"`
		Result  json.RawMessage            `json:"result"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&env)
	if resp.StatusCode/100 != 2 || !env.Success {
		msg := resp.Status
		if len(env.Errors) > 0 {
			msg = env.Errors[0].Message
		}
		return &cfError{Status: resp.StatusCode, Msg: msg}
	}
	if out != nil && len(env.Result) > 0 {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cfRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

// cfZoneFor finds the Cloudflare zone that contains host by trying ever shorter suffixes.
func (s *Server) cfZoneFor(ctx context.Context, token, host string) (*cfZone, error) {
	labels := strings.Split(host, ".")
	for i := 0; i+1 < len(labels); i++ {
		name := strings.Join(labels[i:], ".")
		var zs []cfZone
		if err := s.cfCall(ctx, token, http.MethodGet, "/zones?name="+url.QueryEscape(name), nil, &zs); err != nil {
			return nil, err
		}
		if len(zs) > 0 {
			return &zs[0], nil
		}
	}
	return nil, nil
}

// syncDNS points each hostname at the device with an A record in the matching Cloudflare
// zone. It only creates or updates records, never deletes, and never touches a name that
// already has a CNAME. Problems become events: a proxy sync should not fail over DNS.
func (s *Server) syncDNS(ctx context.Context, d *store.Device, hosts []string) {
	tok := s.cloudflareToken(ctx)
	ips := ipv4s(d)
	if tok == "" || len(hosts) == 0 || len(ips) == 0 {
		return
	}
	proxied := s.cloudflareProxied(ctx)
	if isPrivateIP(ips[0]) {
		proxied = false // Cloudflare can't reach a private address: this record only works on the LAN
	}
	zones := map[string]*cfZone{}
	for _, h := range hosts {
		z, seen := zones[zoneOf(h)]
		if !seen {
			var err error
			if z, err = s.cfZoneFor(ctx, tok, h); err != nil {
				s.event(ctx, d.ID, "warning", "dns.sync", err.Error())
				return
			}
			zones[zoneOf(h)] = z
		}
		if z == nil {
			s.event(ctx, d.ID, "warning", "dns.sync", "no Cloudflare zone for "+h+": add the domain to Cloudflare or give the token access to it")
			continue
		}
		if err := s.cfUpsertA(ctx, tok, z.ID, h, ips[0], proxied, d.ID); err != nil {
			s.event(ctx, d.ID, "warning", "dns.sync", h+": "+err.Error())
		}
	}
}

func (s *Server) cfUpsertA(ctx context.Context, tok, zone, host, ip string, proxied bool, deviceID string) error {
	var recs []cfRecord
	if err := s.cfCall(ctx, tok, http.MethodGet, "/zones/"+zone+"/dns_records?name="+url.QueryEscape(host), nil, &recs); err != nil {
		return err
	}
	var a *cfRecord
	for i, r := range recs {
		switch r.Type {
		case "CNAME":
			return fmt.Errorf("a CNAME record already exists, leaving it alone")
		case "A":
			a = &recs[i]
		}
	}
	want := map[string]any{"type": "A", "name": host, "content": ip, "proxied": proxied, "ttl": 1, "comment": "Managed by Orchestrator"}
	switch {
	case a == nil:
		if err := s.cfCall(ctx, tok, http.MethodPost, "/zones/"+zone+"/dns_records", want, nil); err != nil {
			return err
		}
		s.event(ctx, deviceID, "info", "dns.sync", fmt.Sprintf("Cloudflare: %s -> %s created", host, ip))
	case a.Content != ip || a.Proxied != proxied:
		if err := s.cfCall(ctx, tok, http.MethodPut, "/zones/"+zone+"/dns_records/"+a.ID, want, nil); err != nil {
			return err
		}
		s.event(ctx, deviceID, "info", "dns.sync", fmt.Sprintf("Cloudflare: %s -> %s updated", host, ip))
	}
	return nil
}

func (s *Server) resyncAllProxies(ctx context.Context) {
	devs, _ := s.store.ListDevices(ctx)
	for _, d := range devs {
		s.syncProxy(ctx, d.ID)
	}
}

// handleSetCloudflare verifies and stores the Cloudflare API token (empty removes it).
func (s *Server) handleSetCloudflare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token   string `json:"token"`
		Proxied *bool  `json:"proxied"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	tok := strings.TrimSpace(req.Token)
	switch {
	case tok != "":
		var v struct {
			Status string `json:"status"`
		}
		if err := s.cfCall(ctx, tok, http.MethodGet, "/user/tokens/verify", nil, &v); err != nil {
			writeError(w, http.StatusBadRequest, "Cloudflare rejected the token: "+strings.TrimPrefix(err.Error(), "cloudflare: "))
			return
		}
		if v.Status != "active" {
			writeError(w, http.StatusBadRequest, "the Cloudflare token is "+v.Status+", not active")
			return
		}
		var zones []cfZone
		if err := s.cfCall(ctx, tok, http.MethodGet, "/zones?per_page=50", nil, &zones); err != nil {
			writeError(w, http.StatusBadRequest, "the token is valid but cannot list zones. Give it Zone > Zone > Read and Zone > DNS > Edit: "+err.Error())
			return
		}
		s.store.SetSetting(ctx, settingCloudflareToken, tok)
		names := []string{}
		for _, z := range zones {
			names = append(names, z.Name)
		}
		s.event(ctx, "", "info", "settings.cloudflare", "Cloudflare connected")
		if req.Proxied != nil {
			s.setCloudflareProxied(ctx, *req.Proxied)
		}
		s.resyncAllProxies(context.WithoutCancel(ctx))
		writeJSON(w, http.StatusOK, map[string]any{"connected": true, "zones": names, "proxied": s.cloudflareProxied(ctx)})
	case req.Token == "" && req.Proxied != nil && s.cloudflareToken(ctx) != "":
		s.setCloudflareProxied(ctx, *req.Proxied)
		s.resyncAllProxies(context.WithoutCancel(ctx))
		writeJSON(w, http.StatusOK, map[string]any{"connected": true, "proxied": *req.Proxied})
	default:
		s.store.SetSetting(ctx, settingCloudflareToken, "")
		s.store.SetSetting(ctx, settingSSLMode, sslHTTP)
		s.event(ctx, "", "info", "settings.cloudflare", "Cloudflare disconnected")
		s.resyncAllProxies(context.WithoutCancel(ctx))
		writeJSON(w, http.StatusOK, map[string]any{"connected": false})
	}
}

func (s *Server) setCloudflareProxied(ctx context.Context, on bool) {
	v := ""
	if on {
		v = "1"
	}
	s.store.SetSetting(ctx, settingCloudflareProxied, v)
}

// handleSetSSL chooses how devices obtain certificates.
func (s *Server) handleSetSSL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Mode != sslHTTP && req.Mode != sslDNS {
		writeError(w, http.StatusBadRequest, "mode must be http or dns")
		return
	}
	if req.Mode == sslDNS && s.cloudflareToken(r.Context()) == "" {
		writeError(w, http.StatusConflict, "connect Cloudflare first: the DNS challenge uses its API token")
		return
	}
	s.store.SetSetting(r.Context(), settingSSLMode, req.Mode)
	s.event(r.Context(), "", "info", "settings.ssl", "Certificate challenge set to "+req.Mode)
	s.resyncAllProxies(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusOK, map[string]string{"ssl_mode": req.Mode})
}

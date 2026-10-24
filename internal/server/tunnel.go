package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// A device can run one Cloudflare Tunnel (a cloudflared container, managed by the agent). Any
// project on the device can then be reached through it. Settings are kept per device.

const tunnelContainer = "orchestrator-tunnel"

// tunnelRoute sends a public hostname through the tunnel to a service on the device.
type tunnelRoute struct {
	Hostname string `json:"hostname"`
	// Service is a URL such as http://localhost:3000, or "project:<name>" for that project's first published port.
	Service string `json:"service"`
	// Project is the ID of the project that owns the route when it was set from the project's own tab.
	Project string `json:"project,omitempty"`
}

type tunnelConfig struct {
	Token  string        `json:"token"`
	Routes []tunnelRoute `json:"routes,omitempty"`
}

var tunnelServiceRe = regexp.MustCompile(`^(https?|tcp|ssh|rdp|unix)://\S+$`)

func tunnelKey(deviceID string) string { return "tunnel:" + deviceID }

func (s *Server) tunnelConfig(ctx context.Context, deviceID string) (tunnelConfig, bool) {
	var c tunnelConfig
	v, _ := s.store.GetSetting(ctx, tunnelKey(deviceID))
	if v == "" || json.Unmarshal([]byte(v), &c) != nil || c.Token == "" {
		return tunnelConfig{}, false
	}
	return c, true
}

func (s *Server) saveTunnelConfig(ctx context.Context, deviceID string, c *tunnelConfig) error {
	if c == nil {
		return s.store.SetSetting(ctx, tunnelKey(deviceID), "")
	}
	b, _ := json.Marshal(c)
	return s.store.SetSetting(ctx, tunnelKey(deviceID), string(b))
}

// tunnelToken is what cloudflared's connector token decodes to.
type tunnelToken struct {
	Account string `json:"a"`
	Tunnel  string `json:"t"`
}

func parseTunnelToken(tok string) (tunnelToken, error) {
	var t tunnelToken
	b, err := base64.StdEncoding.DecodeString(tok)
	if err != nil {
		b, err = base64.RawStdEncoding.DecodeString(tok)
	}
	if err != nil || json.Unmarshal(b, &t) != nil || t.Account == "" || t.Tunnel == "" {
		return t, errors.New("that doesn't look like a tunnel token; copy the token from the connector command in Cloudflare Zero Trust > Networks > Tunnels")
	}
	return t, nil
}

type tunnelView struct {
	Supported bool          `json:"supported"` // the agent can run cloudflared
	Enabled   bool          `json:"enabled"`
	State     string        `json:"state,omitempty"` // container state: running, restarting, exited...
	TunnelID  string        `json:"tunnel_id,omitempty"`
	Routes    []tunnelRoute `json:"routes"`
	// CanRoute is true when Cloudflare is connected, so routes can be applied from here.
	CanRoute bool     `json:"can_route"`
	Warnings []string `json:"warnings,omitempty"`
}

func (s *Server) tunnelView(ctx context.Context, d *store.Device) tunnelView {
	v := tunnelView{Supported: hasCap(d, api.CapTunnel), Routes: []tunnelRoute{}, CanRoute: s.cloudflareToken(ctx) != ""}
	cfg, ok := s.tunnelConfig(ctx, d.ID)
	if !ok {
		return v
	}
	v.Enabled = true
	if t, err := parseTunnelToken(cfg.Token); err == nil {
		v.TunnelID = t.Tunnel
	}
	if cfg.Routes != nil {
		v.Routes = cfg.Routes
	}
	for _, c := range d.Containers {
		if c.Name == tunnelContainer {
			v.State = c.State
		}
	}
	return v
}

func (s *Server) handleGetTunnel(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.tunnelView(r.Context(), d))
}

// handleSetTunnel stores the tunnel token and has the agent run cloudflared. An empty token turns the tunnel off.
func (s *Server) handleSetTunnel(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	if !hasCap(d, api.CapTunnel) {
		writeError(w, http.StatusConflict, "this device's agent is too old for tunnels; update it from the Updates page first")
		return
	}
	tok := strings.TrimSpace(req.Token)
	// People paste the whole connector command; the token is its last word.
	if f := strings.Fields(tok); len(f) > 1 {
		tok = f[len(f)-1]
	}
	cfg, had := s.tunnelConfig(ctx, d.ID)
	if tok == "" {
		if !had {
			writeJSON(w, http.StatusOK, s.tunnelView(ctx, d))
			return
		}
		if _, err := s.store.CreateCommand(ctx, d.ID, api.ActionTunnelSync, tunnelContainer, map[string]string{"token": ""}); err != nil {
			s.internal(w, err)
			return
		}
		s.saveTunnelConfig(ctx, d.ID, nil)
		s.event(ctx, d.ID, "info", "tunnel.removed", "Cloudflare Tunnel removed from "+d.Name)
		writeJSON(w, http.StatusOK, s.tunnelView(ctx, d))
		return
	}
	if _, err := parseTunnelToken(tok); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg.Token = tok
	if err := s.saveTunnelConfig(ctx, d.ID, &cfg); err != nil {
		s.internal(w, err)
		return
	}
	if _, err := s.store.CreateCommand(ctx, d.ID, api.ActionTunnelSync, tunnelContainer, map[string]string{"token": tok}); err != nil {
		s.internal(w, err)
		return
	}
	s.event(ctx, d.ID, "info", "tunnel.enabled", "Cloudflare Tunnel set up on "+d.Name)
	writeJSON(w, http.StatusAccepted, s.tunnelView(ctx, d))
}

// resolveRoute turns a route's service into the URL cloudflared connects to.
func (s *Server) resolveRoute(projects []*store.Project, deviceID string, rt tunnelRoute) (string, error) {
	name, isProject := strings.CutPrefix(rt.Service, "project:")
	if !isProject {
		return rt.Service, nil
	}
	for _, p := range projects {
		if p.DeviceID == deviceID && p.Name == name {
			if port := upstreamPort(p); port != "" {
				return "http://localhost:" + port, nil
			}
			return "", fmt.Errorf("project %s publishes no port to send traffic to", name)
		}
	}
	return "", fmt.Errorf("no project %s on this device", name)
}

// handleSetTunnelRoutes saves the hostname routes and, when Cloudflare is connected, pushes them to
// the tunnel's ingress configuration and creates the DNS records. Without Cloudflare they are only
// saved; the tunnel's public hostnames are then managed in the Cloudflare dashboard.
func (s *Server) handleSetTunnelRoutes(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Routes []tunnelRoute `json:"routes"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	cfg, has := s.tunnelConfig(ctx, d.ID)
	if !has {
		writeError(w, http.StatusConflict, "set up the tunnel on this device first")
		return
	}
	if err := cleanRoutes(req.Routes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg.Routes = req.Routes
	view, err := s.saveRoutes(ctx, d, cfg)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// saveRoutes stores cfg and, when Cloudflare is connected, applies its routes.
func (s *Server) saveRoutes(ctx context.Context, d *store.Device, cfg tunnelConfig) (tunnelView, error) {
	if err := s.saveTunnelConfig(ctx, d.ID, &cfg); err != nil {
		return tunnelView{}, err
	}
	view := s.tunnelView(ctx, d)
	if tok := s.cloudflareToken(ctx); tok != "" {
		view.Warnings = s.applyTunnelRoutes(ctx, d, cfg, tok)
	} else if len(cfg.Routes) > 0 {
		view.Warnings = []string{"Cloudflare isn't connected in Settings, so these routes were saved but not applied. Add the public hostnames in Cloudflare Zero Trust, or connect a token with Cloudflare Tunnel > Edit."}
	}
	return view, nil
}

// applyTunnelRoutes pushes ingress rules to Cloudflare and points each hostname at the tunnel.
// Problems are returned as warnings so one bad hostname doesn't block the others.
func (s *Server) applyTunnelRoutes(ctx context.Context, d *store.Device, cfg tunnelConfig, cfToken string) []string {
	t, err := parseTunnelToken(cfg.Token)
	if err != nil {
		return []string{err.Error()}
	}
	projects, _ := s.store.ListProjects(ctx)
	var warns []string
	ingress := []map[string]string{}
	var applied []tunnelRoute
	for _, rt := range cfg.Routes {
		svc, err := s.resolveRoute(projects, d.ID, rt)
		if err != nil {
			warns = append(warns, rt.Hostname+": "+err.Error())
			continue
		}
		ingress = append(ingress, map[string]string{"hostname": rt.Hostname, "service": svc})
		applied = append(applied, rt)
	}
	ingress = append(ingress, map[string]string{"service": "http_status:404"})
	path := "/accounts/" + t.Account + "/cfd_tunnel/" + t.Tunnel + "/configurations"
	body := map[string]any{"config": map[string]any{"ingress": ingress}}
	if err := s.cfCall(ctx, cfToken, http.MethodPut, path, body, nil); err != nil {
		return append(warns, "couldn't update the tunnel configuration (the token needs Account > Cloudflare Tunnel > Edit): "+strings.TrimPrefix(err.Error(), "cloudflare: "))
	}
	for _, rt := range applied {
		if err := s.cfUpsertTunnelCNAME(ctx, cfToken, rt.Hostname, t.Tunnel+".cfargotunnel.com"); err != nil {
			warns = append(warns, rt.Hostname+": "+err.Error())
		}
	}
	s.event(ctx, d.ID, "info", "tunnel.routes", fmt.Sprintf("Cloudflare Tunnel on %s: %d route(s) applied", d.Name, len(applied)))
	return warns
}

// cfUpsertTunnelCNAME points host at the tunnel. It never replaces an A/AAAA record.
func (s *Server) cfUpsertTunnelCNAME(ctx context.Context, tok, host, target string) error {
	z, err := s.cfZoneFor(ctx, tok, host)
	if err != nil {
		return err
	}
	if z == nil {
		return errors.New("no Cloudflare zone for this hostname; add the domain to Cloudflare or give the token access to it")
	}
	var recs []cfRecord
	if err := s.cfCall(ctx, tok, http.MethodGet, "/zones/"+z.ID+"/dns_records?name="+url.QueryEscape(host), nil, &recs); err != nil {
		return err
	}
	want := map[string]any{"type": "CNAME", "name": host, "content": target, "proxied": true, "ttl": 1, "comment": "Managed by Orchestrator"}
	for _, rec := range recs {
		switch {
		case rec.Type == "CNAME" && rec.Content == target && rec.Proxied:
			return nil
		case rec.Type == "CNAME":
			return s.cfCall(ctx, tok, http.MethodPut, "/zones/"+z.ID+"/dns_records/"+rec.ID, want, nil)
		default:
			return fmt.Errorf("a %s record already exists for this name, leaving it alone", rec.Type)
		}
	}
	return s.cfCall(ctx, tok, http.MethodPost, "/zones/"+z.ID+"/dns_records", want, nil)
}

// cleanRoutes normalises hostnames and services in place and rejects duplicates and bad values.
func cleanRoutes(routes []tunnelRoute) error {
	seen := map[string]bool{}
	for i := range routes {
		rt := &routes[i]
		rt.Hostname, rt.Service = strings.ToLower(strings.TrimSpace(rt.Hostname)), strings.TrimSpace(rt.Service)
		if !validHostname(rt.Hostname) || seen[rt.Hostname] {
			return fmt.Errorf("%q is not a valid, unique hostname", rt.Hostname)
		}
		seen[rt.Hostname] = true
		if !tunnelServiceRe.MatchString(rt.Service) && !strings.HasPrefix(rt.Service, "project:") {
			return fmt.Errorf("service %q should look like http://localhost:3000 or project:<name>", rt.Service)
		}
	}
	return nil
}

// projectPorts are the TCP host ports a project publishes on its device: from its settings and
// from its running containers, which is how the ports of a Compose stack's services are found.
func projectPorts(p *store.Project, d *store.Device) []int {
	seen := map[int]bool{}
	var out []int
	add := func(n int) {
		if n > 0 && n < 65536 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, m := range p.Runtime.Ports {
		if h, _, ok := strings.Cut(m, ":"); ok && !strings.HasSuffix(m, "/udp") {
			n, _ := strconv.Atoi(h)
			add(n)
		}
	}
	for _, c := range d.Containers {
		mine := c.Labels["orchestrator.project"] == p.ID || (p.Source.Type == store.SourceCompose && c.ComposeProject == p.Name)
		if !mine {
			continue
		}
		for _, pt := range c.Ports {
			if pt.Type == "tcp" {
				add(pt.PublicPort)
			}
		}
	}
	sort.Ints(out)
	return out
}

type projectTunnelView struct {
	tunnelView
	Ports []int `json:"ports"`
	// Routes holds only this project's routes; the device page lists all of them.
	Device string `json:"device_id"`
}

func (s *Server) projectTunnel(ctx context.Context, p *store.Project, d *store.Device) projectTunnelView {
	v := s.tunnelView(ctx, d)
	mine := []tunnelRoute{}
	for _, rt := range v.Routes {
		if rt.Project == p.ID {
			mine = append(mine, rt)
		}
	}
	v.Routes = mine
	return projectTunnelView{tunnelView: v, Ports: projectPorts(p, d), Device: d.ID}
}

func (s *Server) handleGetProjectTunnel(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	d, err := s.store.GetDevice(r.Context(), p.DeviceID)
	if err != nil {
		writeError(w, http.StatusConflict, "the project's device no longer exists")
		return
	}
	writeJSON(w, http.StatusOK, s.projectTunnel(r.Context(), p, d))
}

// handleSetProjectTunnel replaces the routes this project owns on its device's tunnel and leaves the rest alone.
func (s *Server) handleSetProjectTunnel(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	var req struct {
		Routes []struct {
			Hostname string `json:"hostname"`
			Port     int    `json:"port"`
		} `json:"routes"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	d, err := s.store.GetDevice(ctx, p.DeviceID)
	if err != nil {
		writeError(w, http.StatusConflict, "the project's device no longer exists")
		return
	}
	cfg, has := s.tunnelConfig(ctx, d.ID)
	if !has {
		writeError(w, http.StatusConflict, "set up the Cloudflare Tunnel on the project's device first")
		return
	}
	var mine []tunnelRoute
	for _, in := range req.Routes {
		if in.Port < 1 || in.Port > 65535 {
			writeError(w, http.StatusBadRequest, "choose a port for each hostname")
			return
		}
		mine = append(mine, tunnelRoute{Hostname: in.Hostname, Service: "http://localhost:" + strconv.Itoa(in.Port), Project: p.ID})
	}
	all := mine
	for _, rt := range cfg.Routes {
		if rt.Project != p.ID {
			all = append(all, rt)
		}
	}
	if err := cleanRoutes(all); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg.Routes = all
	v, err := s.saveRoutes(ctx, d, cfg)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := s.projectTunnel(ctx, p, d)
	out.Warnings = v.Warnings
	writeJSON(w, http.StatusOK, out)
}

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	// Managed routes are kept in step with a project's domains (see reconcileTunnelHosts) rather than edited by hand.
	Managed bool `json:"managed,omitempty"`
	// Project is the ID of the project that owns the route when it was set from the project's own tab.
	Project string `json:"project,omitempty"`
}

type tunnelConfig struct {
	// Via is where tunnel traffic for managed hostnames goes: "" straight to the worker's port, or
	// "proxy" through the device's Caddy, which adds login, path routes and preview URLs.
	Via    string        `json:"via,omitempty"`
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
	Via       string        `json:"via"` // "" direct | proxy
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
	v.Via = cfg.Via
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
// When several devices run the same tunnel, any of their connectors may receive a request, so
// "localhost" would be the wrong machine: project routes then use the owning device's address.
func (s *Server) resolveRoute(projects []*store.Project, dev *store.Device, shared bool, via string, rt tunnelRoute) (string, error) {
	deviceID := dev.ID
	if rt.Managed && via == "proxy" && strings.HasPrefix(rt.Service, "project:") {
		// Through the device proxy, which picks the worker by hostname and path.
		if !shared {
			return "http://localhost:80", nil
		}
		if ip := deviceIPv4(dev); ip != "" {
			return "http://" + ip + ":80", nil
		}
		return "", fmt.Errorf("%s reports no IPv4 address, which a shared tunnel needs to reach its proxy", dev.Name)
	}
	name, isProject := strings.CutPrefix(rt.Service, "project:")
	if !isProject {
		return rt.Service, nil
	}
	for _, p := range projects {
		if p.DeviceID == deviceID && p.Name == name {
			if port := upstreamPort(p); port != "" {
				if !shared {
					return "http://localhost:" + port, nil
				}
				if ip := deviceIPv4(dev); ip != "" {
					return "http://" + ip + ":" + port, nil
				}
				return "", fmt.Errorf("%s reports no IPv4 address, which a shared tunnel needs to reach %s", dev.Name, name)
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
	// One tunnel has one ingress configuration, so devices sharing it are applied together.
	peers := s.tunnelPeers(ctx, t.Tunnel, d)
	shared := len(peers) > 1
	seen := map[string]bool{}
	for _, pd := range peers {
		pcfg := cfg
		if pd.ID != d.ID {
			pcfg, _ = s.tunnelConfig(ctx, pd.ID)
		}
		for _, rt := range pcfg.Routes {
			if seen[rt.Hostname] {
				continue
			}
			seen[rt.Hostname] = true
			svc, err := s.resolveRoute(projects, pd, shared, pcfg.Via, rt)
			if err != nil {
				warns = append(warns, rt.Hostname+": "+err.Error())
				continue
			}
			ingress = append(ingress, map[string]string{"hostname": rt.Hostname, "service": svc})
			applied = append(applied, rt)
		}
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

// tunnelPeers returns the devices running the tunnel with this id, always including d.
func (s *Server) tunnelPeers(ctx context.Context, tunnelID string, d *store.Device) []*store.Device {
	out := []*store.Device{d}
	all, _ := s.store.ListDevices(ctx)
	for _, o := range all {
		if o.ID == d.ID {
			continue
		}
		if c, ok := s.tunnelConfig(ctx, o.ID); ok {
			if t, err := parseTunnelToken(c.Token); err == nil && t.Tunnel == tunnelID {
				out = append(out, o)
			}
		}
	}
	return out
}

// deviceIPv4 is the first non-loopback IPv4 address the agent reported.
func deviceIPv4(d *store.Device) string {
	for _, a := range d.Host.Addresses {
		ip := net.ParseIP(a)
		if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
			return a
		}
	}
	return ""
}

// moveTunnelRoutes follows a migrated project: its "project:<name>" routes and Tunnel-tab routes leave the old device's
// tunnel for the new device's, and Cloudflare is updated so the public hostname keeps working.
// It returns log lines for the migration.
func (s *Server) moveTunnelRoutes(ctx context.Context, p *store.Project, oldDev string, dst *store.Device) []string {
	oldCfg, ok := s.tunnelConfig(ctx, oldDev)
	if !ok {
		return nil
	}
	var moving, keep []tunnelRoute
	for _, rt := range oldCfg.Routes {
		// Routes set from the project's Tunnel tab point at a host port and carry the project ID; the
		// port comes from the project's own settings, so it is the same on the new device.
		if rt.Service == "project:"+p.Name || rt.Project == p.ID {
			moving = append(moving, rt)
		} else {
			keep = append(keep, rt)
		}
	}
	if len(moving) == 0 {
		return nil
	}
	newCfg, ok := s.tunnelConfig(ctx, dst.ID)
	if !ok {
		return []string{"Tunnel routes for " + p.Name + " stay on the old device: " + dst.Name + " has no Cloudflare Tunnel. Set one up there and add the routes again"}
	}
	newCfg.Routes = append(newCfg.Routes, moving...)
	oldCfg.Routes = keep
	if err := s.saveTunnelConfig(ctx, dst.ID, &newCfg); err != nil {
		return []string{"Couldn't move tunnel routes: " + err.Error()}
	}
	if err := s.saveTunnelConfig(ctx, oldDev, &oldCfg); err != nil {
		return []string{"Couldn't move tunnel routes: " + err.Error()}
	}
	tok := s.cloudflareToken(ctx)
	if tok == "" {
		return []string{"Tunnel routes moved to " + dst.Name + " but Cloudflare isn't connected, so update the public hostnames in Cloudflare Zero Trust"}
	}
	warns := s.applyTunnelRoutes(ctx, dst, newCfg, tok)
	if od, err := s.store.GetDevice(ctx, oldDev); err == nil {
		// A different tunnel needs its own ingress rewritten; a shared one was just applied with the union.
		a, _ := parseTunnelToken(oldCfg.Token)
		b, _ := parseTunnelToken(newCfg.Token)
		if a.Tunnel != b.Tunnel {
			warns = append(warns, s.applyTunnelRoutes(ctx, od, oldCfg, tok)...)
		}
	}
	return append([]string{fmt.Sprintf("Moved %d tunnel route(s) to %s", len(moving), dst.Name)}, warns...)
}

// checkTunnelable says why a project's hostnames can't go through its device's tunnel. Login
// protection and path routes need the device proxy in the path (the tunnel's "proxy" mode).
func (s *Server) checkTunnelable(ctx context.Context, p *store.Project, route bool) error {
	cfg, ok := s.tunnelConfig(ctx, p.DeviceID)
	if !ok {
		return errors.New("set up a Cloudflare Tunnel on this project's device first (Device > Settings)")
	}
	if upstreamPort(p) == "" {
		return errors.New("the project publishes no port for the tunnel to send traffic to")
	}
	if cfg.Via != "proxy" {
		if s.projectProtected(ctx, p.ID) {
			return errors.New("login protection is served by the device proxy, which a direct tunnel bypasses: switch the tunnel to \"Through the device proxy\" on the device's Settings page, or turn off \"Require login\"")
		}
		if route {
			return errors.New("a route with a path needs the tunnel set to \"Through the device proxy\" on the device's Settings page")
		}
	}
	return nil
}

// tunnelBypassesLogin reports whether a project has hostnames reaching it through a direct tunnel,
// where the device proxy's login check would not apply.
func (s *Server) tunnelBypassesLogin(ctx context.Context, p *store.Project) bool {
	cfg, ok := s.tunnelConfig(ctx, p.DeviceID)
	if !ok || cfg.Via == "proxy" {
		return false
	}
	if pf := s.urlPrefs(ctx, p.ID); pf.Production && pf.Tunnel {
		return true
	}
	list, _ := s.store.ListDomains(ctx, p.ID)
	for _, dm := range list {
		if dm.Access == "tunnel" {
			return true
		}
	}
	return false
}

// reconcileTunnelHosts makes the device tunnel's managed routes match the project: its custom domains
// set to "tunnel" and, if switched on, its production URL. Other routes are left alone. The returned
// warnings (also logged as events) say why a hostname could not be applied, for the dashboard to show.
func (s *Server) reconcileTunnelHosts(ctx context.Context, p *store.Project) []string {
	cfg, ok := s.tunnelConfig(ctx, p.DeviceID)
	if !ok {
		return nil
	}
	want := map[string]bool{}
	if pf := s.urlPrefs(ctx, p.ID); pf.Production && pf.Tunnel {
		if base := s.baseDomain(ctx); base != "" {
			want[slug(p.Name)+"."+base] = true
		}
	}
	if list, err := s.store.ListDomains(ctx, p.ID); err == nil {
		for _, dm := range list {
			if dm.Access == "tunnel" && (dm.Kind == store.DomainCustom || cfg.Via == "proxy") {
				want[dm.Hostname] = true
			}
		}
	}
	if cfg.Via == "proxy" {
		if pf := s.urlPrefs(ctx, p.ID); pf.Preview && pf.PreviewTunnel {
			if base := s.baseDomain(ctx); base != "" {
				pvs, _ := s.store.ListPreviews(ctx, p.ID)
				for _, pv := range pvs {
					want[pv.Slug+"-"+slug(p.Name)+"."+base] = true
				}
			}
		}
	}
	svc := "project:" + p.Name
	var routes []tunnelRoute
	changed := false
	have := map[string]bool{}
	for _, rt := range cfg.Routes {
		if rt.Managed && rt.Service == svc {
			if !want[rt.Hostname] {
				changed = true
				continue
			}
			have[rt.Hostname] = true
		} else if want[rt.Hostname] {
			have[rt.Hostname] = true // already routed by hand
		}
		routes = append(routes, rt)
	}
	for h := range want {
		if !have[h] {
			changed = true
			routes = append(routes, tunnelRoute{Hostname: h, Service: svc, Managed: true})
		}
	}
	var warns []string
	if pf := s.urlPrefs(ctx, p.ID); pf.Production && pf.Tunnel && s.baseDomain(ctx) == "" {
		warns = append(warns, "The production URL needs a base domain (Settings) before it can go through the tunnel")
	}
	if changed {
		cfg.Routes = routes
		if err := s.saveTunnelConfig(ctx, p.DeviceID, &cfg); err != nil {
			s.log.Error("save tunnel routes", "err", err)
			return append(warns, "couldn't save the tunnel routes: "+err.Error())
		}
	}
	if !changed && len(want) == 0 {
		return warns
	}
	d, err := s.store.GetDevice(ctx, p.DeviceID)
	if err != nil {
		return warns
	}
	tok := s.cloudflareToken(ctx)
	if tok == "" {
		if len(want) > 0 {
			warns = append(warns, "Cloudflare isn't connected in Settings, so the tunnel hostnames were saved but not applied. Connect a token with Cloudflare Tunnel > Edit, or add them in Cloudflare Zero Trust")
		}
		return warns
	}
	// Applied every time, not only when the routes changed, so a hostname that failed earlier is retried.
	for _, w := range s.applyTunnelRoutes(ctx, d, cfg, tok) {
		s.event(ctx, d.ID, "warning", "tunnel.routes", w)
		warns = append(warns, w)
	}
	return warns
}

// handleSetTunnelVia chooses whether tunnel traffic goes straight to workers or through the device proxy.
func (s *Server) handleSetTunnelVia(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Via string `json:"via"`
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
	if req.Via != "" && req.Via != "proxy" {
		writeError(w, http.StatusBadRequest, "via must be empty (direct) or proxy")
		return
	}
	if req.Via == "proxy" && !hasCap(d, api.CapProxy) {
		writeError(w, http.StatusConflict, errAgentTooOld(d, "the device proxy").Error())
		return
	}
	projects, _ := s.store.ListProjects(ctx)
	if req.Via == "" {
		// Going back to direct would bypass the login check and cut off path routes and previews.
		for _, p := range projects {
			if p.DeviceID == d.ID && s.tunnelUsesProxyFeatures(ctx, p) {
				writeError(w, http.StatusConflict, p.Name+" relies on the device proxy through the tunnel (login, a path route or preview URLs). Switch those back to direct DNS first")
				return
			}
		}
	}
	cfg.Via = req.Via
	if err := s.saveTunnelConfig(ctx, d.ID, &cfg); err != nil {
		s.internal(w, err)
		return
	}
	for _, p := range projects {
		if p.DeviceID == d.ID {
			s.reconcileTunnelHosts(ctx, p)
		}
	}
	cfg, _ = s.tunnelConfig(ctx, d.ID) // reconcile may have changed the routes
	if tok := s.cloudflareToken(ctx); tok != "" {
		for _, wn := range s.applyTunnelRoutes(ctx, d, cfg, tok) {
			s.event(ctx, d.ID, "warning", "tunnel.routes", wn)
		}
	}
	s.syncProxy(ctx, d.ID)
	s.event(ctx, d.ID, "info", "tunnel.via", fmt.Sprintf("Tunnel on %s now goes %s", d.Name, map[string]string{"": "straight to workers", "proxy": "through the device proxy"}[req.Via]))
	writeJSON(w, http.StatusOK, s.tunnelView(ctx, d))
}

// tunnelUsesProxyFeatures is true when a project's tunnel hostnames need the proxy in the path.
func (s *Server) tunnelUsesProxyFeatures(ctx context.Context, p *store.Project) bool {
	pf := s.urlPrefs(ctx, p.ID)
	if pf.Preview && pf.PreviewTunnel {
		return true
	}
	list, _ := s.store.ListDomains(ctx, p.ID)
	hasTunnel := pf.Production && pf.Tunnel
	for _, dm := range list {
		if dm.Access == "tunnel" {
			hasTunnel = true
			if dm.Kind == store.DomainRoute {
				return true
			}
		}
	}
	return hasTunnel && s.projectProtected(ctx, p.ID)
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
	out := []int{}
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

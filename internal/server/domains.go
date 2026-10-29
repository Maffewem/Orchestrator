package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

const (
	settingBaseDomain = "base_domain"
	settingProxyUsed  = "proxy_used:"  // + device ID
	settingURLPrefs   = "domain_urls:" // + project ID
	settingProtect    = "protect:"     // + project ID: "1" when visitors must log in
)

// lookupHost resolves a hostname; tests replace it.
var lookupHost = func(ctx context.Context, host string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupHost(ctx, host)
}

var hostRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func validHostname(h string) bool { return len(h) <= 253 && hostRE.MatchString(h) }

// routePathRE is the paths a route may match. They are written into the Caddyfile, so
// whitespace, quotes, braces and comment characters are all refused.
var routePathRE = regexp.MustCompile(`^/[A-Za-z0-9._~!$&()*+,;=:@%/-]*$`)

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// slug turns a project name into a DNS label.
func slug(name string) string {
	s := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	return s
}

// zoneOf guesses the registrable domain (last two labels) for the Zone column.
func zoneOf(host string) string {
	p := strings.Split(host, ".")
	if len(p) <= 2 {
		return host
	}
	return strings.Join(p[len(p)-2:], ".")
}

// urlPrefs says which of a project's built-in URLs are switched on.
type urlPrefs struct {
	Production bool `json:"production"`
	Preview    bool `json:"preview"`
	// Tunnel serves the production URL through the device's Cloudflare Tunnel instead of its proxy and an A record.
	Tunnel bool `json:"tunnel"`
	// PreviewTunnel does the same for preview URLs; each preview gets its own tunnel hostname.
	// Needs the tunnel to go through the device proxy.
	PreviewTunnel bool `json:"preview_tunnel"`
}

func (s *Server) urlPrefs(ctx context.Context, projectID string) urlPrefs {
	p := urlPrefs{Production: true}
	if v, _ := s.store.GetSetting(ctx, settingURLPrefs+projectID); v != "" {
		json.Unmarshal([]byte(v), &p)
	}
	return p
}

func (s *Server) projectProtected(ctx context.Context, projectID string) bool {
	v, _ := s.store.GetSetting(ctx, settingProtect+projectID)
	return v == "1"
}

func (s *Server) baseDomain(ctx context.Context) string {
	v, _ := s.store.GetSetting(ctx, settingBaseDomain)
	return v
}

type dnsRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type domainRow struct {
	ID       string      `json:"id,omitempty"`
	Hostname string      `json:"hostname"`
	Type     string      `json:"type"`
	Path     string      `json:"path,omitempty"`
	Zone     string      `json:"zone"`
	Access   string      `json:"access,omitempty"` // "" or tunnel
	Status   string      `json:"status"`           // active | lan | tunnel | pending | mismatch
	Detail   string      `json:"detail"`
	Records  []dnsRecord `json:"records"`
}

type urlRow struct {
	Hostname string      `json:"hostname"`
	Enabled  bool        `json:"enabled"`
	Status   string      `json:"status,omitempty"`
	Detail   string      `json:"detail,omitempty"`
	Records  []dnsRecord `json:"records,omitempty"`
	Tunnel   bool        `json:"tunnel,omitempty"`
}

type domainsView struct {
	BaseDomain string      `json:"base_domain"`
	Production *urlRow     `json:"production"`
	Preview    *urlRow     `json:"preview"`
	Domains    []domainRow `json:"domains"`
	Addresses  []string    `json:"addresses"`
	Proxy      string      `json:"proxy"` // ok | agent_too_old
	Protected  bool        `json:"protected"`
	DNS        string      `json:"dns"` // manual | cloudflare
	SSL        string      `json:"ssl"` // http | dns
	// LANOnly is true when every address the device reports is private, so public DNS can't reach it.
	LANOnly bool `json:"lan_only"`
	// TunnelReady is true when the device runs a Cloudflare Tunnel that hostnames can be routed through.
	TunnelReady bool `json:"tunnel_ready"`
	// TunnelVia is "" (the tunnel sends traffic straight to workers) or "proxy" (through the device proxy).
	TunnelVia string `json:"tunnel_via"`
}

// ipv4s returns the IPv4 addresses a device reported.
func ipv4s(d *store.Device) []string {
	var out []string
	for _, a := range d.Host.Addresses {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			out = append(out, a)
		}
	}
	return out
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// isPrivateIP is true for LAN, loopback, link-local and Tailscale/CGNAT addresses: ones a public
// DNS record can't usefully point at, and Cloudflare's proxy can't reach.
func isPrivateIP(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip))
}

func allPrivate(ips []string) bool {
	for _, ip := range ips {
		if !isPrivateIP(ip) {
			return false
		}
	}
	return len(ips) > 0
}

// dnsStatus compares what host resolves to with the device's addresses.
func dnsStatus(ctx context.Context, host string, want []string) (status, detail string, recs []dnsRecord) {
	for _, ip := range want {
		recs = append(recs, dnsRecord{Type: "A", Name: host, Value: ip})
	}
	got, err := lookupHost(ctx, host)
	if err != nil || len(got) == 0 {
		return "pending", "No DNS record found yet. Add the record below at your DNS provider.", recs
	}
	for _, g := range got {
		for _, w := range want {
			if g == w {
				if allPrivate(want) {
					return "lan", "DNS points at this device's private address, so the name only works on your network. Route it through the Cloudflare Tunnel to reach it from outside.", recs
				}
				return "active", "DNS points at this device.", recs
			}
		}
	}
	return "mismatch", "DNS resolves to " + strings.Join(got, ", ") + ", not this device. If the record is proxied (for example through Cloudflare) that is expected.", recs
}

func (s *Server) domainsView(ctx context.Context, p *store.Project) (*domainsView, error) {
	d, err := s.store.GetDevice(ctx, p.DeviceID)
	if err != nil {
		return nil, err
	}
	base := s.baseDomain(ctx)
	want := ipv4s(d)
	v := &domainsView{BaseDomain: base, Domains: []domainRow{}, Addresses: want, LANOnly: allPrivate(want)}
	tcfg, hasTunnel := s.tunnelConfig(ctx, p.DeviceID)
	v.TunnelReady = hasTunnel
	v.TunnelVia = tcfg.Via
	tunnelID := ""
	if hasTunnel {
		if t, err := parseTunnelToken(tcfg.Token); err == nil {
			tunnelID = t.Tunnel
		}
	}
	tunnelRow := func(host string) (string, string, []dnsRecord) {
		return "tunnel", "Served through this device's Cloudflare Tunnel, so no A record or open port is needed. " +
				map[bool]string{true: "The CNAME is created for you.", false: "Create this CNAME in your DNS (Cloudflare isn't connected, so it can't be added for you)."}[s.cloudflareToken(ctx) != ""],
			[]dnsRecord{{Type: "CNAME", Name: host, Value: tunnelID + ".cfargotunnel.com"}}
	}
	switch {
	case !hasCap(d, api.CapProxy):
		v.Proxy = "agent_too_old"
	default:
		v.Proxy = "ok"
	}
	v.Protected = s.projectProtected(ctx, p.ID)
	v.DNS, v.SSL = "manual", s.sslMode(ctx)
	if s.cloudflareToken(ctx) != "" {
		v.DNS = "cloudflare"
	}
	prefs := s.urlPrefs(ctx, p.ID)
	if base != "" {
		host := slug(p.Name) + "." + base
		row := &urlRow{Hostname: host, Enabled: prefs.Production, Tunnel: prefs.Tunnel}
		if prefs.Production && prefs.Tunnel {
			row.Status, row.Detail, row.Records = tunnelRow(host)
		} else if prefs.Production {
			row.Status, row.Detail, row.Records = dnsStatus(ctx, host, want)
		}
		v.Production = row
		v.Preview = &urlRow{Hostname: "<branch>-" + slug(p.Name) + "." + base, Enabled: prefs.Preview, Tunnel: prefs.PreviewTunnel}
	}
	list, err := s.store.ListDomains(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	for _, dm := range list {
		var st, detail string
		var recs []dnsRecord
		if dm.Access == "tunnel" {
			st, detail, recs = tunnelRow(dm.Hostname)
		} else {
			st, detail, recs = dnsStatus(ctx, dm.Hostname, want)
		}
		v.Domains = append(v.Domains, domainRow{ID: dm.ID, Hostname: dm.Hostname, Type: dm.Kind, Path: dm.Path, Access: dm.Access,
			Zone: zoneOf(dm.Hostname), Status: st, Detail: detail, Records: recs})
	}
	return v, nil
}

func (s *Server) handleGetDomains(w http.ResponseWriter, r *http.Request) {
	s.writeDomains(w, r, http.StatusOK)
}

func (s *Server) writeDomains(w http.ResponseWriter, r *http.Request, status int) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	v, err := s.domainsView(r.Context(), p)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, status, v)
}

func (s *Server) handleSetURLs(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	var req urlPrefs
	if !decode(w, r, &req) {
		return
	}
	if req.Tunnel && req.Production {
		if err := s.checkTunnelable(r.Context(), p, false); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if req.PreviewTunnel && req.Preview {
		cfg, _ := s.tunnelConfig(r.Context(), p.DeviceID)
		if cfg.Via != "proxy" {
			writeError(w, http.StatusConflict, "preview URLs through the tunnel need the tunnel set to \"Through the device proxy\" on the device's Settings page")
			return
		}
	}
	b, _ := json.Marshal(req)
	if err := s.store.SetSetting(r.Context(), settingURLPrefs+p.ID, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	s.reconcileTunnelHosts(r.Context(), p)
	s.syncProxy(r.Context(), p.DeviceID)
	s.handleGetDomains(w, r)
}

func (s *Server) handleAddDomain(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	var req struct {
		Hostname string `json:"hostname"`
		Type     string `json:"type"`
		Path     string `json:"path"`
		Access   string `json:"access"` // "" or tunnel
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Access != "" && req.Access != "tunnel" {
		writeError(w, http.StatusBadRequest, "access must be empty or tunnel")
		return
	}
	if req.Access == "tunnel" {
		if err := s.checkTunnelable(r.Context(), p, req.Type == store.DomainRoute); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
	}
	host := strings.ToLower(strings.TrimSpace(req.Hostname))
	host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/")
	if !validHostname(host) {
		writeError(w, http.StatusBadRequest, "enter a hostname such as app.example.com (no scheme, port or path)")
		return
	}
	kind, path := store.DomainCustom, ""
	if req.Type == store.DomainRoute {
		kind, path = store.DomainRoute, strings.TrimSpace(req.Path)
		if !routePathRE.MatchString(path) {
			writeError(w, http.StatusBadRequest, "a route path must start with / , for example /api/*")
			return
		}
	}
	if _, err := s.store.CreateDomain(r.Context(), p.ID, host, kind, path, req.Access); errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, host+path+" is already in use")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), p.DeviceID, "info", "domain.added", fmt.Sprintf("%s%s added to %s", host, path, p.Name))
	s.reconcileTunnelHosts(r.Context(), p)
	s.syncProxy(r.Context(), p.DeviceID)
	s.writeDomains(w, r, http.StatusCreated)
}

func (s *Server) handleDeleteDomain(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	dm, err := s.store.GetDomain(r.Context(), r.PathValue("did"))
	if err != nil || dm.ProjectID != p.ID {
		writeError(w, http.StatusNotFound, "no such domain")
		return
	}
	if err := s.store.DeleteDomain(r.Context(), dm.ID); err != nil {
		s.internal(w, err)
		return
	}
	s.reconcileTunnelHosts(r.Context(), p)
	s.syncProxy(r.Context(), p.DeviceID)
	w.WriteHeader(http.StatusNoContent)
}

// handleSetDomainAccess switches a hostname between the device proxy (DNS to the device) and the Cloudflare Tunnel.
func (s *Server) handleSetDomainAccess(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	dm, err := s.store.GetDomain(r.Context(), r.PathValue("did"))
	if err != nil || dm.ProjectID != p.ID {
		writeError(w, http.StatusNotFound, "no such domain")
		return
	}
	var req struct {
		Access string `json:"access"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Access != "" && req.Access != "tunnel" {
		writeError(w, http.StatusBadRequest, "access must be empty or tunnel")
		return
	}
	if req.Access == "tunnel" {
		if err := s.checkTunnelable(r.Context(), p, dm.Kind == store.DomainRoute); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
	}
	if err := s.store.SetDomainAccess(r.Context(), dm.ID, req.Access); err != nil {
		s.internal(w, err)
		return
	}
	s.reconcileTunnelHosts(r.Context(), p)
	s.syncProxy(r.Context(), p.DeviceID)
	s.writeDomains(w, r, http.StatusOK)
}

func (s *Server) handleSetBaseDomain(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseDomain string `json:"base_domain"`
	}
	if !decode(w, r, &req) {
		return
	}
	base := strings.ToLower(strings.Trim(strings.TrimSpace(req.BaseDomain), "."))
	if base != "" && !validHostname(base) {
		writeError(w, http.StatusBadRequest, "enter a domain such as example.com")
		return
	}
	if err := s.store.SetSetting(r.Context(), settingBaseDomain, base); err != nil {
		s.internal(w, err)
		return
	}
	devs, _ := s.store.ListDevices(r.Context())
	for _, d := range devs {
		s.syncProxy(r.Context(), d.ID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"base_domain": base})
}

// upstreamPort is the first published TCP host port of a project.
func upstreamPort(p *store.Project) string {
	for _, m := range p.Runtime.Ports {
		host, _, ok := strings.Cut(m, ":")
		if ok && !strings.HasSuffix(m, "/udp") && host != "" {
			return host
		}
	}
	return ""
}

type site struct{ path, upstream, project string }

// buildCaddyfile renders the proxy configuration for one device. It returns the number of
// hostnames served.
func buildCaddyfile(base, serverURL string, projects []*store.Project, doms map[string][]*store.Domain, prefs map[string]urlPrefs, protected map[string]bool, previews map[string][]*store.Preview) (string, int) {
	file, hosts := buildCaddy(base, serverURL, projects, doms, prefs, protected, previews)
	return file, len(hosts)
}

// buildCaddy is buildCaddyfile that also returns the hostnames it serves.
func buildCaddy(base, serverURL string, projects []*store.Project, doms map[string][]*store.Domain, prefs map[string]urlPrefs, protected map[string]bool, previews map[string][]*store.Preview) (string, []string) {
	sites := map[string][]site{}
	for _, p := range projects {
		port := upstreamPort(p)
		if port == "" {
			continue
		}
		up := "127.0.0.1:" + port
		if base != "" {
			pf := prefs[p.ID]
			if pf.Production {
				h := slug(p.Name) + "." + base
				sites[h] = append(sites[h], site{"", up, p.ID})
			}
			if pf.Preview {
				for _, pv := range previews[p.ID] {
					h := pv.Slug + "-" + slug(p.Name) + "." + base
					sites[h] = append(sites[h], site{"", "127.0.0.1:" + strconv.Itoa(pv.HostPort), p.ID})
				}
			}
		}
		for _, d := range doms[p.ID] {
			sites[d.Hostname] = append(sites[d.Hostname], site{d.Path, up, p.ID})
		}
	}
	hosts := make([]string, 0, len(sites))
	for h := range sites {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	var b strings.Builder
	b.WriteString("# Managed by Orchestrator. Changes are overwritten.\n{\n\tadmin off\n}\n")
	for _, h := range hosts {
		fmt.Fprintf(&b, "\n%s {\n", h)
		entries := sites[h]
		sort.SliceStable(entries, func(i, j int) bool { return len(entries[i].path) > len(entries[j].path) })
		for _, e := range entries {
			head := "handle"
			if e.path != "" {
				head = "handle " + e.path
			}
			fmt.Fprintf(&b, "\t%s {\n", head)
			if protected[e.project] {
				if serverURL == "" {
					// Fail closed: without a way to ask the control plane, nobody gets in.
					b.WriteString("\t\trespond \"Access control is unavailable: update the agent on this device.\" 503\n\t}\n")
					continue
				}
				fmt.Fprintf(&b, "\t\tforward_auth %s {\n\t\t\turi /api/gate/verify?project=%s\n\t\t\tcopy_headers X-Orchestrator-User\n\t\t}\n", serverURL, e.project)
			}
			fmt.Fprintf(&b, "\t\treverse_proxy %s\n\t}\n", e.upstream)
		}
		b.WriteString("}\n")
	}
	return b.String(), hosts
}

// syncProxy pushes the current proxy configuration to a device. Failures are logged and
// recorded as events: a domain change should still save when the device is unreachable.
func (s *Server) syncProxy(ctx context.Context, deviceID string) {
	s.pushProxy(ctx, deviceID, false)
}

// pushProxy is syncProxy with an option to create the proxy even when nothing needs serving
// yet, which is what the device page's "Enable proxy" button does.
func (s *Server) pushProxy(ctx context.Context, deviceID string, force bool) error {
	d, err := s.store.GetDevice(ctx, deviceID)
	if err != nil {
		return err
	}
	all, err := s.store.ListProjects(ctx)
	if err != nil {
		return err
	}
	var projects []*store.Project
	doms := map[string][]*store.Domain{}
	prefs := map[string]urlPrefs{}
	protected := map[string]bool{}
	previews := map[string][]*store.Preview{}
	for _, p := range all {
		if p.DeviceID != deviceID {
			continue
		}
		projects = append(projects, p)
		doms[p.ID], _ = s.store.ListDomains(ctx, p.ID)
		prefs[p.ID] = s.urlPrefs(ctx, p.ID)
		protected[p.ID] = s.projectProtected(ctx, p.ID)
		previews[p.ID], _ = s.store.ListPreviews(ctx, p.ID)
	}
	// Hostnames reached through the tunnel. In direct mode the proxy and DNS skip them; in proxy
	// mode the proxy serves them over plain HTTP for cloudflared, and DNS still skips them.
	for _, p := range projects {
		s.reconcileTunnelHosts(ctx, p) // previews come and go
	}
	tunnelHosts, viaProxy := s.tunnelHostsFor(ctx, d, projects, doms, prefs, previews)
	if !viaProxy {
		for id, list := range doms {
			var keep []*store.Domain
			for _, dm := range list {
				if !tunnelHosts[dm.Hostname] {
					keep = append(keep, dm)
				}
			}
			doms[id] = keep
		}
		for id, pf := range prefs {
			if pf.Tunnel {
				pf.Production = false
			}
			prefs[id] = pf
		}
	}
	file, hosts := buildCaddy(s.baseDomain(ctx), d.Host.Server, projects, doms, prefs, protected, previews)
	n := len(hosts)
	if viaProxy {
		file = markHTTPOnly(file, tunnelHosts)
	}
	var dnsHosts []string
	for _, h := range hosts {
		if !tunnelHosts[h] {
			dnsHosts = append(dnsHosts, h)
		}
	}
	args := map[string]string{"caddyfile": file}
	if tok := s.sslDNSToken(ctx); tok != "" && hasCap(d, api.CapProxyDNS) {
		args["caddyfile"] = withDNSChallenge(file)
		args["cloudflare_token"] = tok
	}
	used, _ := s.store.GetSetting(ctx, settingProxyUsed+deviceID)
	if n == 0 && used == "" && !force {
		return nil // nothing to serve and no proxy to empty
	}
	s.syncDNS(ctx, d, dnsHosts) // DNS does not depend on the agent being new enough to run Caddy
	if !hasCap(d, api.CapProxy) {
		s.event(ctx, d.ID, "warning", "proxy.sync", errAgentTooOld(d, "domains").Error())
		return errAgentTooOld(d, "domains")
	}
	if _, err := s.store.CreateCommand(ctx, d.ID, api.ActionProxySync, "proxy", args); err != nil {
		s.log.Warn("queue proxy sync", "device", d.ID, "err", err)
		return err
	}
	s.store.SetSetting(ctx, settingProxyUsed+deviceID, "1")
	return nil
}

// proxyState describes the device's Caddy container as the last heartbeat saw it:
// running, stopped, none (never created), or agent_too_old.
func proxyState(d *store.Device) string {
	if !hasCap(d, api.CapProxy) {
		return "agent_too_old"
	}
	for _, c := range d.Containers {
		if c.Name == "orchestrator-proxy" || c.Labels["orchestrator.role"] == "proxy" {
			if c.State == "running" {
				return "running"
			}
			return "stopped"
		}
	}
	return "none"
}

// handleEnableProxy creates the device's proxy now instead of waiting for the first domain.
func (s *Server) handleEnableProxy(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	if err := s.pushProxy(r.Context(), d.ID, true); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.event(r.Context(), d.ID, "info", "proxy.enable", identityFrom(r).Name+" enabled the proxy on "+d.Name)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// handleSetAccess turns "visitors must sign in with an Orchestrator account" on or off.
func (s *Server) handleSetAccess(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	var req struct {
		Protected bool `json:"protected"`
	}
	if !decode(w, r, &req) {
		return
	}
	v := ""
	if req.Protected {
		v = "1"
		if s.tunnelBypassesLogin(r.Context(), p) {
			writeError(w, http.StatusConflict, "some of this worker's hostnames go through a direct Cloudflare Tunnel, which would bypass the login. Switch the tunnel to \"Through the device proxy\" on the device's Settings page, or serve those hostnames by DNS instead")
			return
		}
	}
	if err := s.store.SetSetting(r.Context(), settingProtect+p.ID, v); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), p.DeviceID, "info", "domain.access", fmt.Sprintf("%s set login for %s to %v", identityFrom(r).Name, p.Name, req.Protected))
	s.syncProxy(r.Context(), p.DeviceID)
	s.writeDomains(w, r, http.StatusOK)
}

// tunnelHostsFor returns the hostnames this device's projects reach through the tunnel, and whether
// the tunnel goes through the proxy.
func (s *Server) tunnelHostsFor(ctx context.Context, d *store.Device, projects []*store.Project, doms map[string][]*store.Domain, prefs map[string]urlPrefs, previews map[string][]*store.Preview) (map[string]bool, bool) {
	cfg, ok := s.tunnelConfig(ctx, d.ID)
	out := map[string]bool{}
	if !ok {
		return out, false
	}
	base := s.baseDomain(ctx)
	for _, p := range projects {
		pf := prefs[p.ID]
		if base != "" && pf.Production && pf.Tunnel {
			out[slug(p.Name)+"."+base] = true
		}
		if base != "" && cfg.Via == "proxy" && pf.Preview && pf.PreviewTunnel {
			for _, pv := range previews[p.ID] {
				out[pv.Slug+"-"+slug(p.Name)+"."+base] = true
			}
		}
		for _, dm := range doms[p.ID] {
			if dm.Access == "tunnel" {
				out[dm.Hostname] = true
			}
		}
	}
	return out, cfg.Via == "proxy"
}

// markHTTPOnly rewrites those site blocks to listen on plain HTTP, so Caddy neither redirects
// cloudflared to HTTPS nor tries to get a certificate for a name that doesn't point at this device.
func markHTTPOnly(file string, hosts map[string]bool) string {
	for h := range hosts {
		file = strings.Replace(file, "\n"+h+" {\n", "\nhttp://"+h+" {\n", 1)
	}
	return file
}

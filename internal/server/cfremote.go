package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// What Cloudflare itself knows about a Pages project or Worker: its deployment history, preview
// deployments, custom domains and configuration. It is read-only and refreshed by the monitor,
// so a project imported into Orchestrator shows the history that already exists on Cloudflare.
// Secret values are never read into this structure: secrets and bindings are listed by name only.

const settingCFRemote = "cfremote:" // + project ID: JSON cfRemote

const (
	cfRemoteMaxDeployments = 50
	cfRemoteMaxPreviews    = 20
	cfRemoteMaxZones       = 25
	cfRemoteMaxValueLen    = 200
)

type cfRemote struct {
	Kind        string               `json:"kind"`
	Name        string               `json:"name"`
	FetchedAt   time.Time            `json:"fetched_at"`
	Deployments []cfRemoteDeployment `json:"deployments"`
	Previews    []cfRemotePreview    `json:"previews"`
	Domains     []cfRemoteDomain     `json:"domains"`
	Settings    *cfRemoteSettings    `json:"settings,omitempty"`
	// Notes says, per section ("deployments", "previews", "domains", "routes", "settings"), why
	// it could not be read, usually a missing token permission.
	Notes map[string]string `json:"notes,omitempty"`
}

type cfRemoteDeployment struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"` // Worker version number, Pages short ID
	URL         string `json:"url,omitempty"`
	Environment string `json:"environment"` // production or preview
	Status      string `json:"status"`      // success, failure, building, canceled or active
	CreatedAt   string `json:"created_at"`
	Branch      string `json:"branch,omitempty"`
	Commit      string `json:"commit,omitempty"`
	Message     string `json:"message,omitempty"`
	Author      string `json:"author,omitempty"`
	Source      string `json:"source,omitempty"` // how it was made: github, wrangler, dashboard, api
	Live        bool   `json:"live,omitempty"`   // the deployment serving production now
	Percentage  *int   `json:"percentage,omitempty"`
}

type cfRemotePreview struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"` // branch, or Worker version
	URL       string   `json:"url,omitempty"`
	Aliases   []string `json:"aliases,omitempty"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
	Commit    string   `json:"commit,omitempty"`
	Message   string   `json:"message,omitempty"`
	Author    string   `json:"author,omitempty"`
}

type cfRemoteDomain struct {
	Host   string `json:"host"`
	Kind   string `json:"kind"`             // custom-domain, route, pages-domain
	Status string `json:"status,omitempty"` // active, pending ... where Cloudflare reports one
	Detail string `json:"detail,omitempty"` // route pattern or zone
}

type cfRemoteVar struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Value  string `json:"value,omitempty"` // plain text and JSON variables only
	Secret bool   `json:"secret,omitempty"`
}

type cfRemoteBinding struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Target string `json:"target,omitempty"` // the namespace, bucket, service ... it points at
}

type cfRemoteSettings struct {
	CompatibilityDate  string            `json:"compatibility_date,omitempty"`
	CompatibilityFlags []string          `json:"compatibility_flags,omitempty"`
	UsageModel         string            `json:"usage_model,omitempty"`
	Observability      *bool             `json:"observability,omitempty"`
	ProductionBranch   string            `json:"production_branch,omitempty"`
	Repo               string            `json:"repo,omitempty"`
	BuildCommand       string            `json:"build_command,omitempty"`
	OutputDir          string            `json:"output_dir,omitempty"`
	RootDir            string            `json:"root_dir,omitempty"`
	Variables          []cfRemoteVar     `json:"variables"`
	Bindings           []cfRemoteBinding `json:"bindings"`
}

func (s *Server) cfRemoteOf(ctx context.Context, projectID string) *cfRemote {
	v, _ := s.store.GetSetting(ctx, settingCFRemote+projectID)
	if v == "" {
		return nil
	}
	var r cfRemote
	if json.Unmarshal([]byte(v), &r) != nil {
		return nil
	}
	return &r
}

func (s *Server) saveCFRemote(ctx context.Context, projectID string, r *cfRemote) {
	b, _ := json.Marshal(r)
	s.store.SetSetting(ctx, settingCFRemote+projectID, string(b))
}

// cfWhyDenied turns a Cloudflare error into a sentence saying what the token lacks.
func cfWhyDenied(err error, need string) string {
	switch cfHTTPStatus(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "The Cloudflare token is not allowed to read this. Give it " + need + "."
	case http.StatusNotFound:
		return "Cloudflare has nothing here."
	}
	return strings.TrimPrefix(err.Error(), "cloudflare: ")
}

func clip(s string) string {
	if len(s) > cfRemoteMaxValueLen {
		return s[:cfRemoteMaxValueLen] + "…"
	}
	return s
}

// cfFetchRemote reads history, previews, domains and settings. Each section fails on its own.
func (s *Server) cfFetchRemote(ctx context.Context, tok, acct string, t *store.CloudflareTarget, name string) *cfRemote {
	r := &cfRemote{Kind: t.Kind, Name: name, FetchedAt: time.Now().UTC(), Notes: map[string]string{},
		Deployments: []cfRemoteDeployment{}, Previews: []cfRemotePreview{}, Domains: []cfRemoteDomain{}}
	if t.Kind == api.CloudflarePages {
		s.cfRemotePages(ctx, tok, acct, name, r)
	} else {
		s.cfRemoteWorker(ctx, tok, acct, name, r)
	}
	return r
}

type cfPagesRemoteDeployment struct {
	ID          string   `json:"id"`
	ShortID     string   `json:"short_id"`
	URL         string   `json:"url"`
	Environment string   `json:"environment"`
	CreatedOn   string   `json:"created_on"`
	Aliases     []string `json:"aliases"`
	Trigger     struct {
		Type     string `json:"type"`
		Metadata struct {
			Branch  string `json:"branch"`
			Hash    string `json:"commit_hash"`
			Message string `json:"commit_message"`
		} `json:"metadata"`
	} `json:"deployment_trigger"`
	Stage struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"latest_stage"`
}

func pagesStatus(stage, status string) string {
	switch status {
	case "success":
		return "success"
	case "failure":
		return "failure"
	case "canceled":
		return "canceled"
	}
	return "building"
}

func (s *Server) cfRemotePages(ctx context.Context, tok, acct, name string, r *cfRemote) {
	base := fmt.Sprintf("/accounts/%s/pages/projects/%s", acct, url.PathEscape(name))
	const need = "Account > Cloudflare Pages > Read (or Edit)"

	var proj struct {
		Name             string   `json:"name"`
		Subdomain        string   `json:"subdomain"`
		Domains          []string `json:"domains"`
		ProductionBranch string   `json:"production_branch"`
		Canonical        *struct {
			ID string `json:"id"`
		} `json:"canonical_deployment"`
		Source *struct {
			Type   string `json:"type"`
			Config struct {
				Owner string `json:"owner"`
				Repo  string `json:"repo_name"`
			} `json:"config"`
		} `json:"source"`
		Build struct {
			Command string `json:"build_command"`
			Dir     string `json:"destination_dir"`
			Root    string `json:"root_dir"`
		} `json:"build_config"`
		Configs map[string]map[string]json.RawMessage `json:"deployment_configs"`
	}
	projErr := s.cfCall(ctx, tok, http.MethodGet, base, nil, &proj)
	if projErr != nil {
		r.Notes["settings"] = cfWhyDenied(projErr, need)
	}

	var deps []cfPagesRemoteDeployment
	if err := s.cfCall(ctx, tok, http.MethodGet, base+fmt.Sprintf("/deployments?per_page=%d&sort_by=created_on&sort_order=desc", cfRemoteMaxDeployments), nil, &deps); err != nil {
		r.Notes["deployments"] = cfWhyDenied(err, need)
		r.Notes["previews"] = r.Notes["deployments"]
	}
	liveID := ""
	if proj.Canonical != nil {
		liveID = proj.Canonical.ID
	}
	seenBranch := map[string]bool{}
	for _, d := range deps {
		out := cfRemoteDeployment{ID: d.ID, Label: d.ShortID, URL: d.URL, Environment: d.Environment, Status: pagesStatus(d.Stage.Name, d.Stage.Status),
			CreatedAt: d.CreatedOn, Branch: d.Trigger.Metadata.Branch, Commit: d.Trigger.Metadata.Hash, Message: firstLine(d.Trigger.Metadata.Message), Source: d.Trigger.Type}
		switch d.Trigger.Type {
		case "github:push":
			out.Source = "GitHub"
		case "ad_hoc":
			out.Source = "Direct upload"
		}
		out.Live = d.ID == liveID && liveID != ""
		r.Deployments = append(r.Deployments, out)
		if d.Environment == "preview" && !seenBranch[out.Branch] && len(r.Previews) < cfRemoteMaxPreviews {
			seenBranch[out.Branch] = true
			r.Previews = append(r.Previews, cfRemotePreview{ID: d.ID, Label: out.Branch, URL: d.URL, Aliases: d.Aliases, Status: out.Status,
				CreatedAt: d.CreatedOn, Commit: out.Commit, Message: out.Message})
		}
	}

	if projErr == nil {
		def := "https://" + proj.Subdomain
		_ = def
		for _, h := range proj.Domains {
			if h == proj.Subdomain || strings.HasSuffix(h, ".pages.dev") {
				continue
			}
			r.Domains = append(r.Domains, cfRemoteDomain{Host: h, Kind: "pages-domain", Status: "active"})
		}
		// The domains endpoint says whether each is verified and has its certificate.
		var doms []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if err := s.cfCall(ctx, tok, http.MethodGet, base+"/domains", nil, &doms); err == nil {
			for i := range r.Domains {
				for _, d := range doms {
					if d.Name == r.Domains[i].Host && d.Status != "" {
						r.Domains[i].Status = d.Status
					}
				}
			}
		}
		set := &cfRemoteSettings{ProductionBranch: proj.ProductionBranch, BuildCommand: proj.Build.Command, OutputDir: proj.Build.Dir, RootDir: proj.Build.Root,
			Variables: []cfRemoteVar{}, Bindings: []cfRemoteBinding{}}
		if proj.Source != nil && proj.Source.Type == "github" && proj.Source.Config.Owner != "" {
			set.Repo = proj.Source.Config.Owner + "/" + proj.Source.Config.Repo
		}
		if prod := proj.Configs["production"]; prod != nil {
			cfReadPagesConfig(prod, set)
		}
		r.Settings = set
	}
}

// cfReadPagesConfig reads a Pages deployment configuration. Only names are kept for anything
// that is not a plain-text variable.
func cfReadPagesConfig(cfg map[string]json.RawMessage, set *cfRemoteSettings) {
	str := func(k string) string {
		var v string
		json.Unmarshal(cfg[k], &v)
		return v
	}
	set.CompatibilityDate = str("compatibility_date")
	json.Unmarshal(cfg["compatibility_flags"], &set.CompatibilityFlags)
	set.UsageModel = str("usage_model")
	var env map[string]struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	json.Unmarshal(cfg["env_vars"], &env)
	for k, v := range env {
		cv := cfRemoteVar{Name: k, Type: v.Type}
		if v.Type == "plain_text" {
			cv.Value = clip(v.Value)
		} else {
			cv.Secret = true
		}
		set.Variables = append(set.Variables, cv)
	}
	for k, raw := range cfg {
		switch k {
		case "env_vars", "compatibility_date", "compatibility_flags", "usage_model", "fail_open", "always_use_latest_compatibility_date", "placement", "limits":
			continue
		}
		var m map[string]map[string]any // BINDING_NAME -> its settings
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		for bn, v := range m {
			set.Bindings = append(set.Bindings, cfRemoteBinding{Name: bn, Type: strings.TrimSuffix(k, "s"), Target: cfBindingTarget(v)})
		}
	}
	sortSettings(set)
}

// cfBindingTarget names what a binding points at, from the fields Cloudflare uses for that.
func cfBindingTarget(v map[string]any) string {
	for _, k := range []string{"service", "bucket_name", "namespace_id", "class_name", "queue_name", "project_id", "index_name", "dataset", "id", "name"} {
		if s, ok := v[k].(string); ok && s != "" {
			return clip(s)
		}
	}
	return ""
}

func sortSettings(set *cfRemoteSettings) {
	sort.Slice(set.Variables, func(i, j int) bool { return set.Variables[i].Name < set.Variables[j].Name })
	sort.Slice(set.Bindings, func(i, j int) bool { return set.Bindings[i].Name < set.Bindings[j].Name })
}

type cfWorkerRemoteDeployment struct {
	ID          string            `json:"id"`
	Source      string            `json:"source"`
	AuthorEmail string            `json:"author_email"`
	CreatedOn   string            `json:"created_on"`
	Annotations map[string]string `json:"annotations"`
	Versions    []struct {
		ID         string  `json:"version_id"`
		Percentage float64 `json:"percentage"`
	} `json:"versions"`
}

type cfWorkerRemoteVersion struct {
	ID       string `json:"id"`
	Number   int    `json:"number"`
	Metadata struct {
		CreatedOn   string `json:"created_on"`
		Source      string `json:"source"`
		AuthorEmail string `json:"author_email"`
	} `json:"metadata"`
	Annotations map[string]string `json:"annotations"`
}

func (s *Server) cfRemoteWorker(ctx context.Context, tok, acct, name string, r *cfRemote) {
	base := fmt.Sprintf("/accounts/%s/workers/scripts/%s", acct, url.PathEscape(name))
	const need = "Account > Workers Scripts > Read (or Edit)"

	var sub struct {
		Subdomain string `json:"subdomain"`
	}
	s.cfCall(ctx, tok, http.MethodGet, fmt.Sprintf("/accounts/%s/workers/subdomain", acct), nil, &sub)
	var sw struct {
		Enabled  bool `json:"enabled"`
		Previews bool `json:"previews_enabled"`
	}
	s.cfCall(ctx, tok, http.MethodGet, base+"/subdomain", nil, &sw)
	devURL := func(prefix string) string {
		if sub.Subdomain == "" || !sw.Enabled {
			return ""
		}
		if prefix != "" {
			prefix += "-"
		}
		return fmt.Sprintf("https://%s%s.%s.workers.dev", prefix, name, sub.Subdomain)
	}

	var vs struct {
		Items []cfWorkerRemoteVersion `json:"items"`
	}
	versions := map[string]cfWorkerRemoteVersion{}
	if err := s.cfCall(ctx, tok, http.MethodGet, base+"/versions?per_page=50", nil, &vs); err != nil && cfHTTPStatus(err) != http.StatusNotFound {
		r.Notes["previews"] = cfWhyDenied(err, need)
	}
	for _, v := range vs.Items {
		versions[v.ID] = v
	}

	var ds struct {
		Deployments []cfWorkerRemoteDeployment `json:"deployments"`
	}
	if err := s.cfCall(ctx, tok, http.MethodGet, base+"/deployments", nil, &ds); err != nil {
		r.Notes["deployments"] = cfWhyDenied(err, need)
	}
	inUse := map[string]bool{}
	for i, d := range ds.Deployments {
		if i >= cfRemoteMaxDeployments {
			break
		}
		out := cfRemoteDeployment{ID: d.ID, Environment: "production", Status: "success", CreatedAt: d.CreatedOn, Author: d.AuthorEmail,
			Source: cfWorkerSource(d.Source), Message: firstLine(d.Annotations["workers/message"]), Live: i == 0}
		if i == 0 {
			out.Status = "active"
			out.URL = devURL("")
		}
		var labels []string
		for _, v := range d.Versions {
			if i == 0 {
				inUse[v.ID] = true
			}
			if ver, ok := versions[v.ID]; ok {
				labels = append(labels, fmt.Sprintf("v%d", ver.Number))
				if out.Message == "" {
					out.Message = firstLine(ver.Annotations["workers/message"])
				}
				if out.Author == "" {
					out.Author = ver.Metadata.AuthorEmail
				}
			}
			if len(d.Versions) == 1 {
				p := int(v.Percentage)
				out.Percentage = &p
			}
		}
		out.Label = strings.Join(labels, " + ")
		r.Deployments = append(r.Deployments, out)
	}
	// Versions that are not live are the Worker's previews: each has its own workers.dev address
	// when Preview URLs are on for the Worker.
	for _, v := range vs.Items {
		if inUse[v.ID] || len(r.Previews) >= cfRemoteMaxPreviews {
			continue
		}
		pv := cfRemotePreview{ID: v.ID, Label: fmt.Sprintf("v%d", v.Number), Status: "success", CreatedAt: v.Metadata.CreatedOn,
			Message: firstLine(v.Annotations["workers/message"]), Author: v.Metadata.AuthorEmail}
		if sw.Previews && len(v.ID) >= 8 {
			pv.URL = devURL(v.ID[:8])
		}
		r.Previews = append(r.Previews, pv)
	}
	if len(r.Previews) > 0 && !sw.Previews && sub.Subdomain != "" {
		r.Notes["previews"] = "Preview URLs are off for this Worker on Cloudflare, so these versions have no address."
	}

	// Custom domains and routes.
	var doms []struct {
		Hostname string `json:"hostname"`
		Zone     string `json:"zone_name"`
		Service  string `json:"service"`
		Env      string `json:"environment"`
	}
	if err := s.cfCall(ctx, tok, http.MethodGet, fmt.Sprintf("/accounts/%s/workers/domains?service=%s", acct, url.QueryEscape(name)), nil, &doms); err != nil {
		r.Notes["domains"] = cfWhyDenied(err, "Account > Workers Scripts > Read")
	}
	for _, d := range doms {
		if d.Service == "" || d.Service == name {
			r.Domains = append(r.Domains, cfRemoteDomain{Host: d.Hostname, Kind: "custom-domain", Status: "active", Detail: d.Zone})
		}
	}
	s.cfWorkerRoutes(ctx, tok, acct, name, r)

	// Settings: bindings by name, plain-text variables, compatibility.
	var st struct {
		Compat  string   `json:"compatibility_date"`
		Flags   []string `json:"compatibility_flags"`
		Usage   string   `json:"usage_model"`
		Observe *struct {
			Enabled bool `json:"enabled"`
		} `json:"observability"`
		Bindings []map[string]any `json:"bindings"`
	}
	if err := s.cfCall(ctx, tok, http.MethodGet, base+"/settings", nil, &st); err != nil {
		r.Notes["settings"] = cfWhyDenied(err, need)
		return
	}
	set := &cfRemoteSettings{CompatibilityDate: st.Compat, CompatibilityFlags: st.Flags, UsageModel: st.Usage, Variables: []cfRemoteVar{}, Bindings: []cfRemoteBinding{}}
	if st.Observe != nil {
		set.Observability = &st.Observe.Enabled
	}
	for _, b := range st.Bindings {
		bn, _ := b["name"].(string)
		bt, _ := b["type"].(string)
		if bn == "" {
			continue
		}
		switch bt {
		case "plain_text":
			v, _ := b["text"].(string)
			set.Variables = append(set.Variables, cfRemoteVar{Name: bn, Type: bt, Value: clip(v)})
		case "json":
			raw, _ := json.Marshal(b["json"])
			set.Variables = append(set.Variables, cfRemoteVar{Name: bn, Type: bt, Value: clip(string(raw))})
		case "secret_text", "secrets_store_secret":
			set.Variables = append(set.Variables, cfRemoteVar{Name: bn, Type: bt, Secret: true})
		default:
			set.Bindings = append(set.Bindings, cfRemoteBinding{Name: bn, Type: bt, Target: cfBindingTarget(b)})
		}
	}
	sortSettings(set)
	r.Settings = set
}

func cfWorkerSource(s string) string {
	switch s {
	case "wrangler":
		return "Wrangler"
	case "dash", "dash_template":
		return "Dashboard"
	case "api":
		return "API"
	}
	return s
}

// cfWorkerRoutes finds routes that point at the Worker. Routes belong to zones, so this lists
// the account's zones, which needs the Zone Read permission; without it the routes are skipped.
func (s *Server) cfWorkerRoutes(ctx context.Context, tok, acct, name string, r *cfRemote) {
	var zones []cfZone
	if err := s.cfCall(ctx, tok, http.MethodGet, "/zones?per_page=50&account.id="+url.QueryEscape(acct), nil, &zones); err != nil {
		r.Notes["routes"] = cfWhyDenied(err, "Zone > Zone > Read and Zone > Workers Routes > Read") + " Custom domains are still listed; routes are not."
		return
	}
	for i, z := range zones {
		if i >= cfRemoteMaxZones {
			break
		}
		var routes []struct {
			Pattern string `json:"pattern"`
			Script  string `json:"script"`
		}
		if err := s.cfCall(ctx, tok, http.MethodGet, "/zones/"+z.ID+"/workers/routes", nil, &routes); err != nil {
			r.Notes["routes"] = cfWhyDenied(err, "Zone > Workers Routes > Read") + " Some routes may be missing."
			continue
		}
		for _, rt := range routes {
			if rt.Script == name {
				r.Domains = append(r.Domains, cfRemoteDomain{Host: cfRouteHost(rt.Pattern), Kind: "route", Status: "active", Detail: rt.Pattern})
			}
		}
	}
}

// cfRouteHost is the hostname part of a route pattern such as "example.com/api/*".
func cfRouteHost(pattern string) string {
	h, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(pattern, "https://"), "http://"), "/")
	return h
}

// cfVisitHost picks the custom domain to link to: a custom domain or Pages domain that is
// active, otherwise a route whose host has no wildcard.
func cfVisitHost(ds []cfRemoteDomain) string {
	for _, kind := range []string{"custom-domain", "pages-domain", "route"} {
		for _, d := range ds {
			if d.Kind == kind && d.Status == "active" && d.Host != "" && !strings.Contains(d.Host, "*") {
				return d.Host
			}
		}
	}
	return ""
}

func (s *Server) handleGetCloudflareRemote(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	if p.Source.Type != store.SourceCloudflare {
		writeError(w, http.StatusBadRequest, "this project is not deployed to Cloudflare")
		return
	}
	writeJSON(w, http.StatusOK, s.cfRemoteOf(r.Context(), p.ID))
}

package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// cfFound is a Pages project or Worker that exists in the Cloudflare account.
type cfFound struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	URL    string `json:"url,omitempty"`
	Repo   string `json:"repo,omitempty"`   // Pages projects connected to GitHub
	Branch string `json:"branch,omitempty"` // their production branch
	// Imported is the Orchestrator project that already manages it, if any.
	Imported string `json:"imported,omitempty"`
}

type cfPagesListed struct {
	Name             string `json:"name"`
	Subdomain        string `json:"subdomain"`
	ProductionBranch string `json:"production_branch"`
	Source           *struct {
		Type   string `json:"type"`
		Config struct {
			Owner            string `json:"owner"`
			RepoName         string `json:"repo_name"`
			ProductionBranch string `json:"production_branch"`
		} `json:"config"`
	} `json:"source"`
}

// cfDiscover lists what the account already has, marking what Orchestrator manages.
func (s *Server) cfDiscover(ctx context.Context) ([]cfFound, error) {
	tok, acct := s.cfDeployCreds(ctx)
	if tok == "" || acct == "" {
		return nil, fmt.Errorf("connect Cloudflare Pages & Workers in Settings first")
	}
	managed := map[string]string{}
	projects, _ := s.store.ListProjects(ctx)
	for _, p := range projects {
		if p.Source.Type == store.SourceCloudflare && p.Source.Cloudflare != nil {
			managed[p.Source.Cloudflare.Kind+"/"+cfNameOf(p)] = p.Name
		}
	}
	var out []cfFound
	var pages []cfPagesListed
	pagesErr := s.cfCall(ctx, tok, http.MethodGet, "/accounts/"+acct+"/pages/projects?per_page=100", nil, &pages)
	for _, pg := range pages {
		f := cfFound{Kind: api.CloudflarePages, Name: pg.Name, Branch: pg.ProductionBranch, Imported: managed["pages/"+pg.Name]}
		if pg.Subdomain != "" {
			f.URL = "https://" + pg.Subdomain
		}
		if pg.Source != nil && pg.Source.Type == "github" && pg.Source.Config.Owner != "" {
			f.Repo = pg.Source.Config.Owner + "/" + pg.Source.Config.RepoName
			if pg.Source.Config.ProductionBranch != "" {
				f.Branch = pg.Source.Config.ProductionBranch
			}
		}
		out = append(out, f)
	}
	var scripts []struct {
		ID string `json:"id"`
	}
	workersErr := s.cfCall(ctx, tok, http.MethodGet, "/accounts/"+acct+"/workers/scripts", nil, &scripts)
	var sub struct {
		Subdomain string `json:"subdomain"`
	}
	s.cfCall(ctx, tok, http.MethodGet, "/accounts/"+acct+"/workers/subdomain", nil, &sub)
	for _, sc := range scripts {
		f := cfFound{Kind: api.CloudflareWorker, Name: sc.ID, Imported: managed["worker/"+sc.ID]}
		if sub.Subdomain != "" {
			f.URL = fmt.Sprintf("https://%s.%s.workers.dev", sc.ID, sub.Subdomain)
		}
		out = append(out, f)
	}
	if pagesErr != nil && workersErr != nil {
		return nil, pagesErr
	}
	return out, nil
}

func (s *Server) handleCloudflareDiscover(w http.ResponseWriter, r *http.Request) {
	if !s.cfDeployReady(r.Context()) {
		writeJSON(w, http.StatusOK, []cfFound{})
		return
	}
	found, err := s.cfDiscover(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if found == nil {
		found = []cfFound{}
	}
	writeJSON(w, http.StatusOK, found)
}

type cfImportResult struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Project string `json:"project,omitempty"`
	Error   string `json:"error,omitempty"`
}

// handleCloudflareImport creates a project for each chosen Pages project or Worker. Nothing is
// deployed or changed on Cloudflare: the project only watches what is there, and auto-deploy
// stays off until the person turns it on.
func (s *Server) handleCloudflareImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Items []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	found, err := s.cfDiscover(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	byKey := map[string]cfFound{}
	for _, f := range found {
		byKey[f.Kind+"/"+f.Name] = f
	}
	results := []cfImportResult{}
	for _, it := range req.Items {
		res := cfImportResult{Kind: it.Kind, Name: it.Name}
		f, ok := byKey[it.Kind+"/"+it.Name]
		switch {
		case !ok:
			res.Error = "not found in this Cloudflare account"
		case f.Imported != "":
			res.Error = "already managed as " + f.Imported
		default:
			p, err := s.importCloudflare(ctx, f)
			if err != nil {
				res.Error = err.Error()
			} else {
				res.Project = p.Name
			}
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, results)
}

func (s *Server) importCloudflare(ctx context.Context, f cfFound) (*store.Project, error) {
	name := slug(f.Name)
	if name == "" {
		name = "cloudflare-" + f.Kind
	}
	src := store.Source{Type: store.SourceCloudflare, Repo: f.Repo, Branch: f.Branch,
		Cloudflare: &store.CloudflareTarget{Kind: f.Kind}}
	if f.Name != name {
		src.Cloudflare.Name = f.Name
	}
	in := projectInput{Name: name, Source: src}
	if err := s.validateProject(ctx, &in); err != nil {
		return nil, fmt.Errorf("%s: %w", f.Name, err)
	}
	p := &store.Project{Name: in.Name, Source: in.Source, Runtime: in.Runtime}
	if err := s.store.CreateProject(ctx, p); err != nil {
		if err == store.ErrConflict {
			return nil, fmt.Errorf("a project named %s already exists; rename it first", name)
		}
		return nil, err
	}
	s.event(ctx, "", "info", "project.created", fmt.Sprintf("Project %s imported from Cloudflare %s %s", p.Name, f.Kind, url.PathEscape(f.Name)))
	s.cfPollProject(ctx, p)
	return p, nil
}

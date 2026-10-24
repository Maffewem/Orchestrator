package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

const (
	settingPreviews = "previews:" // + project ID: JSON previewSettings

	defaultMaxPreviews = 5
	previewPortBase    = 20000
	previewPortSpan    = 10000
)

// previewSettings is a project's preview switch. Previews exist for open pull requests from
// the project's own repository; forks are skipped so untrusted code never builds here.
type previewSettings struct {
	Enabled bool `json:"enabled"`
	Max     int  `json:"max"`
}

func (s *Server) previewSettings(ctx context.Context, projectID string) previewSettings {
	ps := previewSettings{Max: defaultMaxPreviews}
	if v, _ := s.store.GetSetting(ctx, settingPreviews+projectID); v != "" {
		json.Unmarshal([]byte(v), &ps)
	}
	if ps.Max < 1 {
		ps.Max = defaultMaxPreviews
	}
	return ps
}

// branchSlug makes a DNS-safe label for a branch name.
func branchSlug(branch string) string {
	s := slug(branch)
	if len(s) > 30 {
		s = strings.Trim(s[:30], "-")
	}
	if s == "" {
		s = "branch"
	}
	return s
}

// containerPort returns the container side of the first published TCP mapping.
func containerPort(ports []string) string {
	for _, m := range ports {
		if strings.HasSuffix(m, "/udp") {
			continue
		}
		m = strings.TrimSuffix(m, "/tcp")
		if i := strings.LastIndex(m, ":"); i >= 0 {
			m = m[i+1:]
		}
		if m != "" {
			return m
		}
	}
	return ""
}

// allocatePort picks a free host port for a preview, deterministic per branch so a
// recreated preview usually lands on the same URL.
func (s *Server) allocatePort(ctx context.Context, deviceID, key string) (int, error) {
	h := fnv.New32a()
	h.Write([]byte(key))
	start := int(h.Sum32() % previewPortSpan)
	for i := 0; i < previewPortSpan; i++ {
		port := previewPortBase + (start+i)%previewPortSpan
		used, err := s.store.PreviewPortInUse(ctx, deviceID, port)
		if err != nil {
			return 0, err
		}
		if !used {
			return port, nil
		}
	}
	return 0, errors.New("no free preview ports on this device")
}

type pullRequest struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

// wantedPreviews lists the open same-repo pull requests into the tracked branch, newest first.
func (s *Server) wantedPreviews(ctx context.Context, p *store.Project, max int) ([]pullRequest, error) {
	var prs []pullRequest
	path := fmt.Sprintf("/repos/%s/pulls?state=open&per_page=30&sort=updated&direction=desc", p.Source.Repo)
	if err := s.githubGet(ctx, path, &prs); err != nil {
		return nil, err
	}
	var out []pullRequest
	for _, pr := range prs {
		if pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, p.Source.Repo) {
			continue // fork, or the fork was deleted
		}
		if pr.Base.Ref != p.Source.Branch || pr.Head.SHA == "" {
			continue
		}
		out = append(out, pr)
		if len(out) == max {
			break
		}
	}
	return out, nil
}

// syncPreviews makes the project's previews match its open pull requests: new ones are
// created, moved ones rebuilt, and closed or surplus ones removed.
func (s *Server) syncPreviews(ctx context.Context, p *store.Project) {
	if p.Source.Type != store.SourceGitHub {
		return
	}
	s.previewMu.Lock()
	defer s.previewMu.Unlock()

	existing, err := s.store.ListPreviews(ctx, p.ID)
	if err != nil {
		s.log.Error("previews: list", "err", err)
		return
	}
	ps := s.previewSettings(ctx, p.ID)
	if !ps.Enabled {
		for _, pv := range existing {
			s.removePreview(ctx, p, pv, "previews were turned off")
		}
		return
	}
	d, err := s.store.GetDevice(ctx, p.DeviceID)
	if err != nil {
		return
	}
	if !hasCap(d, api.CapPreview) {
		s.log.Warn("previews: agent too old", "device", d.Name)
		return
	}
	if containerPort(p.Runtime.Ports) == "" {
		return // nothing to publish a preview on
	}
	prs, err := s.wantedPreviews(ctx, p, ps.Max)
	if err != nil {
		s.log.Warn("previews: list pull requests", "project", p.Name, "err", err)
		return
	}
	byBranch := map[string]*store.Preview{}
	for _, pv := range existing {
		byBranch[pv.Branch] = pv
	}
	wanted := map[string]bool{}
	changed := false
	for _, pr := range prs {
		wanted[pr.Head.Ref] = true
		commit := &store.Commit{Repo: p.Source.Repo, Branch: pr.Head.Ref, SHA: pr.Head.SHA,
			Message: pr.Title, Author: pr.User.Login, URL: pr.HTMLURL}
		pv := byBranch[pr.Head.Ref]
		if pv != nil && pv.SHA == pr.Head.SHA {
			continue
		}
		if w := previewWatch(p.Source); (len(w.Include) > 0 || len(w.Exclude) > 0) && !wants(w, s.pullFiles(ctx, p.Source.Repo, pr.Number)) {
			continue // nothing the Previews tab watches changed
		}
		if pv == nil {
			pv = &store.Preview{ProjectID: p.ID, Branch: pr.Head.Ref, PR: pr.Number, Title: pr.Title, Author: pr.User.Login}
			if err := s.newPreview(ctx, p, pv); err != nil {
				s.log.Warn("previews: create", "project", p.Name, "branch", pr.Head.Ref, "err", err)
				continue
			}
		}
		pv.PR, pv.Title, pv.Author = pr.Number, pr.Title, pr.User.Login
		if _, err := s.deployPreview(ctx, p, pv, commit, store.TriggerPush); err != nil {
			s.log.Warn("previews: deploy", "project", p.Name, "branch", pr.Head.Ref, "err", err)
			s.event(ctx, p.DeviceID, "error", "deployment.rejected",
				fmt.Sprintf("Preview of %s for %s could not deploy: %v", pr.Head.Ref, p.Name, err))
			continue
		}
		changed = true
	}
	for _, pv := range existing {
		if !wanted[pv.Branch] {
			s.removePreview(ctx, p, pv, "its pull request is closed")
			changed = true
		}
	}
	if changed {
		s.syncProxy(ctx, p.DeviceID)
	}
}

// newPreview reserves a slug and host port for a branch.
func (s *Server) newPreview(ctx context.Context, p *store.Project, pv *store.Preview) error {
	port, err := s.allocatePort(ctx, p.DeviceID, p.ID+"/"+pv.Branch)
	if err != nil {
		return err
	}
	pv.HostPort = port
	base := branchSlug(pv.Branch)
	pv.Slug = base
	for i := 0; i < 5; i++ {
		err = s.store.CreatePreview(ctx, pv)
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		// Another branch already has this slug (feat/x and feat-x): add a short hash.
		h := fnv.New32a()
		h.Write([]byte(pv.Branch + strconv.Itoa(i)))
		pv.Slug = fmt.Sprintf("%.23s-%06x", base, h.Sum32()&0xffffff)
	}
	return err
}

// previewWatch is the watch paths for previews: their own when set, else the production ones.
func previewWatch(src store.Source) store.Watch {
	if o := src.Preview; o != nil && (len(o.Include) > 0 || len(o.Exclude) > 0) {
		return o.Watch
	}
	return src.Watch
}

// previewSource is the project's source with the Previews tab's overrides applied; whatever
// is left empty there keeps its production value.
func previewSource(src store.Source) store.Source {
	o := src.Preview
	if o == nil {
		return src
	}
	if o.Context != "" {
		src.Context = o.Context
	}
	if src.Node != nil {
		n := *src.Node
		if o.Build != "" {
			n.Build = o.Build
		}
		if o.Start != "" && !n.Static {
			n.Start = o.Start
		}
		src.Node = &n
	}
	return src
}

// deployPreview builds one revision of a preview on the project's device.
func (s *Server) deployPreview(ctx context.Context, p *store.Project, pv *store.Preview, commit *store.Commit, trigger string) (*store.Deployment, error) {
	cp := *p
	cp.Name = p.Name + "--" + pv.Slug
	cp.Source = previewSource(p.Source)
	spec, err := specFor(&cp, commit)
	if err != nil {
		return nil, err
	}
	cport := containerPort(p.Runtime.Ports)
	if cport == "" {
		return nil, errors.New("the project publishes no TCP port to preview")
	}
	spec.Ports = []string{fmt.Sprintf("%d:%s", pv.HostPort, cport)}
	spec.Labels = map[string]string{"orchestrator.project": p.ID, "orchestrator.preview": pv.Branch}
	d, err := s.store.CreateDeployment(ctx, store.NewDeployment{
		ProjectID: p.ID, DeviceID: p.DeviceID, Trigger: trigger, Commit: commit, Spec: spec, PreviewID: pv.ID,
	})
	if err != nil {
		return nil, err
	}
	pv.SHA, pv.DeploymentID = commit.SHA, d.ID
	if err := s.store.UpdatePreview(ctx, pv); err != nil {
		return nil, err
	}
	s.event(ctx, p.DeviceID, "info", "deployment.queued",
		fmt.Sprintf("Preview of %s queued for %s@%.7s", p.Name, commit.Branch, commit.SHA))
	return d, nil
}

// removePreview deletes a preview's record and queues removal of its container.
func (s *Server) removePreview(ctx context.Context, p *store.Project, pv *store.Preview, why string) {
	if d, err := s.store.GetDevice(ctx, p.DeviceID); err == nil && hasCap(d, api.CapPreview) {
		target := serviceName(p.Name + "--" + pv.Slug)
		if _, err := s.store.CreateCommand(ctx, d.ID, api.ActionRemove, target, nil); err != nil {
			s.log.Warn("previews: queue remove", "err", err)
		}
	}
	if err := s.store.DeletePreview(ctx, pv.ID); err != nil {
		s.log.Error("previews: delete", "err", err)
		return
	}
	s.event(ctx, p.DeviceID, "info", "preview.removed", fmt.Sprintf("Preview %s of %s removed: %s", pv.Branch, p.Name, why))
}

// previewView is a preview with what the dashboard shows about it.
type previewView struct {
	*store.Preview
	URL    string `json:"url"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) previewURL(ctx context.Context, p *store.Project, pv *store.Preview, d *store.Device) string {
	if base := s.baseDomain(ctx); base != "" && s.urlPrefs(ctx, p.ID).Preview {
		return "https://" + pv.Slug + "-" + slug(p.Name) + "." + base
	}
	if d != nil {
		if ips := ipv4s(d); len(ips) > 0 {
			return fmt.Sprintf("http://%s:%d", ips[0], pv.HostPort)
		}
	}
	return ""
}

type previewsResponse struct {
	Settings  previewSettings `json:"settings"`
	Previews  []previewView   `json:"previews"`
	Supported bool            `json:"supported"` // false when the agent predates previews
}

func (s *Server) previewsResponse(ctx context.Context, p *store.Project) (*previewsResponse, error) {
	list, err := s.store.ListPreviews(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	d, _ := s.store.GetDevice(ctx, p.DeviceID)
	out := &previewsResponse{Settings: s.previewSettings(ctx, p.ID), Previews: []previewView{}, Supported: d != nil && hasCap(d, api.CapPreview)}
	for _, pv := range list {
		v := previewView{Preview: pv, URL: s.previewURL(ctx, p, pv, d), Status: store.DeploymentQueued}
		if dep, err := s.store.GetDeployment(ctx, pv.DeploymentID); err == nil {
			v.Status, v.Error = dep.Status, dep.Error
		}
		out.Previews = append(out.Previews, v)
	}
	return out, nil
}

func (s *Server) handleListPreviews(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	v, err := s.previewsResponse(r.Context(), p)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleSetPreviewSettings(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	if p.Source.Type != store.SourceGitHub {
		writeError(w, http.StatusBadRequest, "previews need a project deployed from GitHub")
		return
	}
	var req previewSettings
	if !decode(w, r, &req) {
		return
	}
	if req.Max < 1 || req.Max > 20 {
		writeError(w, http.StatusBadRequest, "keep between 1 and 20 previews")
		return
	}
	if req.Enabled {
		d, err := s.store.GetDevice(r.Context(), p.DeviceID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "the project's device no longer exists")
			return
		}
		if !hasCap(d, api.CapPreview) {
			writeError(w, http.StatusConflict, errAgentTooOld(d, "previews").Error())
			return
		}
		if containerPort(p.Runtime.Ports) == "" {
			writeError(w, http.StatusBadRequest, "add a port such as 8080:80 to the project first: a preview needs a port to publish")
			return
		}
	}
	b, _ := json.Marshal(req)
	if err := s.store.SetSetting(r.Context(), settingPreviews+p.ID, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	s.syncPreviews(r.Context(), p)
	v, err := s.previewsResponse(r.Context(), p)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleDeletePreview(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	pv, err := s.store.GetPreview(r.Context(), r.PathValue("pid"))
	if err != nil || pv.ProjectID != p.ID {
		writeError(w, http.StatusNotFound, "no such preview")
		return
	}
	s.previewMu.Lock()
	s.removePreview(r.Context(), p, pv, "deleted from the dashboard")
	s.previewMu.Unlock()
	s.syncProxy(r.Context(), p.DeviceID)
	w.WriteHeader(http.StatusNoContent)
}

// handleRollback redeploys an earlier successful production deployment. GitHub projects
// reuse the image still on the device, so a rollback skips the build when it can.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	old, ok := s.lookupDeployment(w, r)
	if !ok {
		return
	}
	if old.PreviewID != "" {
		writeError(w, http.StatusBadRequest, "previews can't be rolled back to; delete the preview instead")
		return
	}
	if old.Status != store.DeploymentSucceeded {
		writeError(w, http.StatusBadRequest, "only a deployment that succeeded can be rolled back to")
		return
	}
	p, err := s.store.GetProject(r.Context(), old.ProjectID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusGone, "the project was deleted")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	d, err := s.deployWith(r.Context(), p, old.Commit, store.TriggerRollback, func(spec *api.DeploySpec) {
		if p.Source.Type == store.SourceImage {
			spec.Image = old.Spec.Image
		} else if p.Source.Type == store.SourceCompose {
			spec.Compose = old.Spec.Compose
		} else {
			spec.Reuse = true
		}
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, d)
}

// handlePullRequestEvent keeps previews in step with GitHub without waiting for the poller.
func (s *Server) handlePullRequestEvent(ctx context.Context, repo string) int {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		s.log.Error("webhook: list projects", "err", err)
		return 0
	}
	n := 0
	for _, p := range projects {
		if p.Source.Type == store.SourceGitHub && strings.EqualFold(p.Source.Repo, repo) && s.previewSettings(ctx, p.ID).Enabled {
			s.syncPreviews(ctx, p)
			n++
		}
	}
	return n
}

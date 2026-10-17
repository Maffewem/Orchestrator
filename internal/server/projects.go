package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

var (
	projectNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	repoRe        = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	portRe        = regexp.MustCompile(`^(\d{1,5}:)?\d{1,5}(/(tcp|udp))?$`)
	restarts      = map[string]bool{"": true, "no": true, "always": true, "unless-stopped": true, "on-failure": true}
)

// ProjectView is a project with its latest deployment, for list and overview pages.
type ProjectView struct {
	*store.Project
	DeviceName string            `json:"device_name"`
	Latest     *store.Deployment `json:"latest_deployment"`
}

func (s *Server) projectView(ctx context.Context, p *store.Project) (ProjectView, error) {
	v := ProjectView{Project: p}
	if d, err := s.store.GetDevice(ctx, p.DeviceID); err == nil {
		v.DeviceName = d.Name
	} else if !errors.Is(err, store.ErrNotFound) {
		return v, err
	}
	deps, err := s.store.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, Limit: 1})
	if err != nil {
		return v, err
	}
	if len(deps) > 0 {
		v.Latest = deps[0]
	}
	return v, nil
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]ProjectView, 0, len(projects))
	for _, p := range projects {
		v, err := s.projectView(r.Context(), p)
		if err != nil {
			s.internal(w, err)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

type projectInput struct {
	Name     string        `json:"name"`
	DeviceID string        `json:"device_id"`
	Source   store.Source  `json:"source"`
	Runtime  store.Runtime `json:"runtime"`
}

func (s *Server) validateProject(ctx context.Context, in *projectInput) error {
	in.Name = strings.TrimSpace(in.Name)
	if !projectNameRe.MatchString(in.Name) {
		return errors.New("name must be lowercase letters, digits and dashes")
	}
	if _, err := s.store.GetDevice(ctx, in.DeviceID); err != nil {
		return errors.New("choose a device to deploy to")
	}
	src := &in.Source
	switch src.Type {
	case store.SourceGitHub:
		src.Repo = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(src.Repo), githubRemote), ".git")
		if !repoRe.MatchString(src.Repo) {
			return errors.New("repository must look like owner/name")
		}
		if src.Branch = strings.TrimSpace(src.Branch); src.Branch == "" {
			src.Branch = "main"
		}
		if strings.Contains(src.Context, "..") || strings.Contains(src.Dockerfile, "..") {
			return errors.New("paths must stay inside the repository")
		}
		src.Image = ""
	case store.SourceImage:
		if src.Image = strings.TrimSpace(src.Image); src.Image == "" {
			return errors.New("image is required")
		}
		src.Repo, src.Branch, src.Context, src.Dockerfile, src.AutoDeploy = "", "", "", "", false
	default:
		return errors.New("source type must be github or image")
	}
	in.Runtime.Ports = cleanLabels(in.Runtime.Ports)
	for _, p := range in.Runtime.Ports {
		if !portRe.MatchString(p) {
			return fmt.Errorf("port %q should look like 8080:80 or 53/udp", p)
		}
	}
	in.Runtime.Env = cleanLabels(in.Runtime.Env)
	for _, e := range in.Runtime.Env {
		if k, _, ok := strings.Cut(e, "="); !ok || k == "" {
			return fmt.Errorf("environment variable %q should look like KEY=value", e)
		}
	}
	if !restarts[in.Runtime.Restart] {
		return errors.New("unknown restart policy")
	}
	return nil
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var in projectInput
	if !decode(w, r, &in) {
		return
	}
	if err := s.validateProject(r.Context(), &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p := &store.Project{Name: in.Name, DeviceID: in.DeviceID, Source: in.Source, Runtime: in.Runtime}
	if err := s.store.CreateProject(r.Context(), p); errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "a project with that name already exists")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), p.DeviceID, "info", "project.created", "Project "+p.Name+" created")
	v, err := s.projectView(r.Context(), p)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) lookupProject(w http.ResponseWriter, r *http.Request) (*store.Project, bool) {
	p, err := s.store.GetProject(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such project")
		return nil, false
	} else if err != nil {
		s.internal(w, err)
		return nil, false
	}
	return p, true
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	v, err := s.projectView(r.Context(), p)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	in := projectInput{Name: p.Name, DeviceID: p.DeviceID, Source: p.Source, Runtime: p.Runtime}
	if !decode(w, r, &in) {
		return
	}
	if err := s.validateProject(r.Context(), &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.Name, p.DeviceID, p.Source, p.Runtime = in.Name, in.DeviceID, in.Source, in.Runtime
	if err := s.store.UpdateProject(r.Context(), p); errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "a project with that name already exists")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	v, err := s.projectView(r.Context(), p)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteProject(r.Context(), p.ID); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), p.DeviceID, "warning", "project.deleted", "Project "+p.Name+" deleted (its container was left running)")
	w.WriteHeader(http.StatusNoContent)
}

// deploy queues a deployment of p. GitHub projects build commit; image projects ignore it.
func (s *Server) deploy(ctx context.Context, p *store.Project, commit *store.Commit, trigger string) (*store.Deployment, error) {
	if _, err := s.store.GetDevice(ctx, p.DeviceID); err != nil {
		return nil, errors.New("the project's device no longer exists")
	}
	if p.Source.Type != store.SourceGitHub {
		commit = nil
	}
	spec, err := specFor(p, commit)
	if err != nil {
		return nil, err
	}
	d, err := s.store.CreateDeployment(ctx, store.NewDeployment{
		ProjectID: p.ID, DeviceID: p.DeviceID, Trigger: trigger, Commit: commit, Spec: spec,
	})
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("Deployment of %s queued (%s)", p.Name, trigger)
	if commit != nil {
		msg = fmt.Sprintf("Deployment of %s queued for %s@%.7s (%s)", p.Name, commit.Branch, commit.SHA, trigger)
	}
	s.event(ctx, p.DeviceID, "info", "deployment.queued", msg)
	return d, nil
}

func (s *Server) handleDeployProject(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupProject(w, r)
	if !ok {
		return
	}
	var commit *store.Commit
	if p.Source.Type == store.SourceGitHub {
		var err error
		if commit, err = s.latestCommit(r.Context(), p.Source.Repo, p.Source.Branch); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	d, err := s.deploy(r.Context(), p, commit, store.TriggerManual)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, d)
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	f := store.DeploymentFilter{ProjectID: q.Get("project"), DeviceID: q.Get("device"), Limit: limit}
	if id := r.PathValue("id"); id != "" {
		f.ProjectID = id
	}
	deps, err := s.store.ListDeployments(r.Context(), f)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deps)
}

func (s *Server) lookupDeployment(w http.ResponseWriter, r *http.Request) (*store.Deployment, bool) {
	d, err := s.store.GetDeployment(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such deployment")
		return nil, false
	} else if err != nil {
		s.internal(w, err)
		return nil, false
	}
	return d, true
}

func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	if d, ok := s.lookupDeployment(w, r); ok {
		writeJSON(w, http.StatusOK, d)
	}
}

// handleRetryDeployment redeploys the same revision with the project's current settings.
func (s *Server) handleRetryDeployment(w http.ResponseWriter, r *http.Request) {
	old, ok := s.lookupDeployment(w, r)
	if !ok {
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
	d, err := s.deploy(r.Context(), p, old.Commit, store.TriggerRetry)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, d)
}

// handleStepUpdate receives deployment progress from an agent.
func (s *Server) handleStepUpdate(w http.ResponseWriter, r *http.Request) {
	d := deviceFrom(r)
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid step")
		return
	}
	var u api.StepUpdate
	if !decode(w, r, &u) {
		return
	}
	err = s.store.UpdateStep(r.Context(), d.ID, r.PathValue("id"), idx, u)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such deployment step")
		return
	} else if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) finishDeployment(ctx context.Context, dev *store.Device, c *api.Command, res api.CommandResult) {
	if err := s.store.FinishDeployment(ctx, c.Target, res.OK, res.Error); err != nil {
		s.log.Error("finish deployment", "err", err)
		return
	}
	name := c.Target
	if c.Deploy != nil {
		name = c.Deploy.Service
	}
	if res.OK {
		s.event(ctx, dev.ID, "info", "deployment.succeeded", fmt.Sprintf("%s deployed to %s", name, dev.Name))
	} else {
		s.event(ctx, dev.ID, "error", "deployment.failed", fmt.Sprintf("%s failed to deploy to %s: %s", name, dev.Name, res.Error))
	}
}

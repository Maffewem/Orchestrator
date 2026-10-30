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
	volumeRe      = regexp.MustCompile(`^[^:\s]+:/[^:\s]*(:ro|:rw)?$`)
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

// forRole hides environment values from viewers: they are secrets (tokens, passwords) that
// only people who can change or deploy the project need to see.
func forRole(r *http.Request, v ProjectView) ProjectView {
	if id := identityFrom(r); id != nil && store.RoleRank(id.Role) >= store.RoleRank(store.RoleOperator) {
		return v
	}
	p := *v.Project
	p.Runtime.Env = make([]string, len(p.Runtime.Env))
	for i, e := range v.Project.Runtime.Env {
		k, _, _ := strings.Cut(e, "=")
		p.Runtime.Env[i] = k + "="
	}
	v.Project = &p
	return v
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
		out = append(out, forRole(r, v))
	}
	writeJSON(w, http.StatusOK, out)
}

type projectInput struct {
	Name     string        `json:"name"`
	DeviceID string        `json:"device_id"`
	Source   store.Source  `json:"source"`
	Runtime  store.Runtime `json:"runtime"`
}

var nodeVersionRe = regexp.MustCompile(`^(lts|[0-9]+(\.[0-9]+){0,2})$`)

// cleanNode trims the Node build settings and rejects the ones the agent could not use.
func cleanNode(n *api.NodeSpec) error {
	for _, f := range []*string{&n.Install, &n.Build, &n.Start, &n.Version, &n.Output} {
		*f = strings.TrimSpace(*f)
	}
	for _, f := range []string{n.Install, n.Build, n.Start, n.Output} {
		if strings.ContainsAny(f, "\n\r\x00") { // these become lines of a generated Dockerfile
			return errors.New("build commands must be a single line")
		}
	}
	if n.Version != "" && !nodeVersionRe.MatchString(n.Version) {
		return errors.New("node version should look like 22, 20.11 or lts")
	}
	if strings.Contains(n.Output, "..") || strings.HasPrefix(n.Output, "/") {
		return errors.New("output directory must stay inside the project")
	}
	if n.Static {
		n.Start = ""
	} else {
		n.Output = ""
	}
	return nil
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
		if src.Dockerfile = strings.TrimSpace(src.Dockerfile); api.LooksLikeComposeFile(src.Dockerfile) {
			return errors.New("the Dockerfile setting points at a Docker Compose file; use the path of a Dockerfile (empty means \"Dockerfile\")")
		}
		src.Image = ""
		if err := cleanWatch(&src.Watch); err != nil {
			return err
		}
		if pv := src.Preview; pv != nil {
			pv.Context, pv.Build, pv.Start = strings.TrimSpace(pv.Context), strings.TrimSpace(pv.Build), strings.TrimSpace(pv.Start)
			if strings.Contains(pv.Context, "..") {
				return errors.New("paths must stay inside the repository")
			}
			if err := cleanWatch(&pv.Watch); err != nil {
				return err
			}
			if src.Node == nil {
				pv.Build, pv.Start = "", "" // only Node builds have these commands
			}
			if pv.Context == "" && pv.Build == "" && pv.Start == "" && len(pv.Include) == 0 && len(pv.Exclude) == 0 {
				src.Preview = nil
			}
		}
		if n := src.Node; n != nil {
			if err := cleanNode(n); err != nil {
				return err
			}
			src.Dockerfile = ""
			if len(in.Runtime.Ports) == 0 {
				if n.Static {
					in.Runtime.Ports = []string{"8080:80"}
				} else {
					in.Runtime.Ports = []string{"3000:3000"}
				}
			}
		}
	case store.SourceCompose:
		if src.Repo = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(src.Repo), githubRemote), ".git"); src.Repo != "" {
			if !repoRe.MatchString(src.Repo) {
				return errors.New("repository must look like owner/name")
			}
			if src.Branch = strings.TrimSpace(src.Branch); src.Branch == "" {
				src.Branch = "main"
			}
			src.ComposeFile = strings.Trim(strings.TrimSpace(src.ComposeFile), "/")
			if src.ComposeFile == "" {
				src.ComposeFile = "docker-compose.yml"
			}
			if strings.Contains(src.ComposeFile, "..") {
				return errors.New("the compose file path must stay inside the repository")
			}
			if err := cleanWatch(&src.Watch); err != nil {
				return err
			}
			src.Compose = ""
		} else {
			src.Compose = strings.ReplaceAll(src.Compose, "\r\n", "\n")
			if err := checkComposeFile(src.Compose); err != nil {
				return err
			}
			src.Branch, src.ComposeFile, src.AutoDeploy, src.Watch = "", "", false, store.Watch{}
		}
		src.Context, src.Dockerfile, src.Node, src.Image, src.Preview = "", "", nil, "", nil
		in.Runtime.Ports, in.Runtime.Volumes, in.Runtime.Command, in.Runtime.Labels, in.Runtime.Network = nil, nil, nil, nil, ""
	case store.SourceImage:
		if src.Image = strings.TrimSpace(src.Image); src.Image == "" {
			return errors.New("image is required")
		}
		src.Repo, src.Branch, src.Context, src.Dockerfile, src.AutoDeploy, src.Node = "", "", "", "", false, nil
		src.Watch, src.Preview = store.Watch{}, nil
	default:
		return errors.New("source type must be github, image or compose")
	}
	in.Runtime.Ports = cleanLabels(in.Runtime.Ports)
	for _, p := range in.Runtime.Ports {
		if !portRe.MatchString(p) {
			return fmt.Errorf("port %q should look like 8080:80 or 53/udp", p)
		}
	}
	in.Runtime.WebPort = strings.TrimSpace(in.Runtime.WebPort)
	if in.Runtime.WebPort != "" {
		if n, err := strconv.Atoi(in.Runtime.WebPort); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("web port %q should be a host port number", in.Runtime.WebPort)
		}
	}
	in.Runtime.Env = cleanLabels(dropEnvComments(in.Runtime.Env))
	for _, e := range in.Runtime.Env {
		if k, _, ok := strings.Cut(e, "="); !ok || k == "" {
			return fmt.Errorf("environment variable %q should look like KEY=value", e)
		}
	}
	if !restarts[in.Runtime.Restart] {
		return errors.New("unknown restart policy")
	}
	in.Runtime.Volumes = cleanLabels(in.Runtime.Volumes)
	for _, v := range in.Runtime.Volumes {
		if !volumeRe.MatchString(v) {
			return fmt.Errorf("volume %q should look like name:/path or /host/path:/path[:ro]", v)
		}
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
	if d, _ := s.store.GetDevice(r.Context(), in.DeviceID); d != nil && d.Maintenance {
		writeError(w, http.StatusConflict, errMaintenance(d).Error())
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
	writeJSON(w, http.StatusOK, forRole(r, v))
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
	if in.DeviceID != p.DeviceID {
		if d, _ := s.store.GetDevice(r.Context(), in.DeviceID); d != nil && d.Maintenance {
			writeError(w, http.StatusConflict, errMaintenance(d).Error())
			return
		}
	}
	p.Name, p.DeviceID, p.Source, p.Runtime = in.Name, in.DeviceID, in.Source, in.Runtime
	if err := s.store.UpdateProject(r.Context(), p); errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "a project with that name already exists")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	s.syncProxy(r.Context(), p.DeviceID)
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
	if pvs, _ := s.store.ListPreviews(r.Context(), p.ID); len(pvs) > 0 {
		s.previewMu.Lock()
		for _, pv := range pvs {
			s.removePreview(r.Context(), p, pv, "its project was deleted")
		}
		s.previewMu.Unlock()
	}
	removed := false
	if r.URL.Query().Get("remove_container") == "1" {
		d, err := s.store.GetDevice(r.Context(), p.DeviceID)
		switch {
		case err != nil:
			// The device is gone, so there is no container left to remove.
		case p.Source.Type == store.SourceCompose && !hasCap(d, api.CapCompose):
			writeError(w, http.StatusConflict, "this device's agent is too old to remove stacks; update it from the Updates page first")
			return
		case !hasCap(d, api.CapPreview):
			writeError(w, http.StatusConflict, "this device's agent is too old to remove containers; update it from the Updates page first")
			return
		default:
			action, target := api.ActionRemove, serviceOf(p)
			if p.Source.Type == store.SourceCompose {
				action, target = api.ActionStackDown, p.Name
			}
			if _, err := s.store.CreateCommand(r.Context(), d.ID, action, target, nil); err != nil {
				s.internal(w, err)
				return
			}
			removed = true
		}
	}
	if err := s.store.DeleteProject(r.Context(), p.ID); err != nil {
		s.internal(w, err)
		return
	}
	defer s.syncProxy(context.WithoutCancel(r.Context()), p.DeviceID)
	left := " (its container was left running)"
	if removed {
		left = " (its container was removed)"
	}
	s.event(r.Context(), p.DeviceID, "warning", "project.deleted", "Project "+p.Name+" deleted"+left)
	w.WriteHeader(http.StatusNoContent)
}

// deploy queues a deployment of p. GitHub projects build commit; image projects ignore it.
func (s *Server) deploy(ctx context.Context, p *store.Project, commit *store.Commit, trigger string) (*store.Deployment, error) {
	return s.deployWith(ctx, p, commit, trigger, nil)
}

// deployWith is deploy with a chance to adjust the spec before it is queued.
func (s *Server) deployWith(ctx context.Context, p *store.Project, commit *store.Commit, trigger string, tweak func(*api.DeploySpec)) (*store.Deployment, error) {
	dev, err := s.store.GetDevice(ctx, p.DeviceID)
	if err != nil {
		return nil, errors.New("the project's device no longer exists")
	}
	if p.Source.Type == store.SourceCompose && !hasCap(dev, api.CapCompose) {
		return nil, errors.New("this device's agent is too old for stacks; update it from the Updates page first")
	}
	if !p.Source.FromRepo() {
		commit = nil
	}
	if p.Source.Type == store.SourceCompose && p.Source.Repo != "" {
		// The compose file is read at the commit being deployed, so a push changes the stack.
		if commit == nil {
			return nil, errors.New("no commit to read the compose file from")
		}
		yaml, err := s.repoFile(ctx, p.Source.Repo, p.Source.ComposeFile, commit.SHA)
		if err != nil {
			return nil, err
		}
		if err := checkComposeFile(yaml); err != nil {
			return nil, fmt.Errorf("%s: %w", p.Source.ComposeFile, err)
		}
		cp := *p
		cp.Source.Compose = yaml
		p = &cp
	}
	spec, err := specFor(p, commit)
	if err != nil {
		return nil, err
	}
	if tweak != nil {
		tweak(&spec)
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
	if msg := s.adoptedWarning(r, p); msg != "" {
		writeError(w, http.StatusConflict, msg)
		return
	}
	var commit *store.Commit
	if p.Source.FromRepo() {
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
	var d *store.Deployment
	if old.PreviewID != "" {
		pv, perr := s.store.GetPreview(r.Context(), old.PreviewID)
		if perr != nil || old.Commit == nil {
			writeError(w, http.StatusGone, "the preview was removed")
			return
		}
		d, err = s.deployPreview(r.Context(), p, pv, old.Commit, store.TriggerRetry)
	} else {
		d, err = s.deploy(r.Context(), p, old.Commit, store.TriggerRetry)
	}
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

// adoptedWarning explains what the first deployment of an adopted project would change, unless
// the request carries confirm=1. Once Orchestrator has recreated the container (it then has the
// project label) there is nothing left to warn about.
func (s *Server) adoptedWarning(r *http.Request, p *store.Project) string {
	if !p.Runtime.Adopted || r.URL.Query().Get("confirm") == "1" {
		return ""
	}
	dev, err := s.store.GetDevice(r.Context(), p.DeviceID)
	if err != nil {
		return ""
	}
	for _, c := range dev.Containers {
		if c.Name != serviceOf(p) || c.Labels["orchestrator.project"] != "" {
			continue
		}
		msg := "Deploying recreates " + c.Name + ": the running container is replaced by a new one built from this project's settings."
		if len(p.Runtime.Unsupported) > 0 {
			msg += " These settings can't be carried over and will be lost: " + strings.Join(p.Runtime.Unsupported, ", ") + "."
		}
		return msg + " Confirm to continue."
	}
	return ""
}

// dropEnvComments removes .env comment lines so pasted files with # notes save cleanly.
func dropEnvComments(in []string) []string {
	out := make([]string, 0, len(in))
	for _, l := range in {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		out = append(out, l)
	}
	return out
}

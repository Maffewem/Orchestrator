package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Adopting turns containers that already run on a device (started by hand, by Portainer or by
// docker compose) into projects. Adoption only reads the container: it is never stopped,
// renamed or recreated. Until the project is redeployed it keeps running exactly as it was.

// Results of adopting one container.
const (
	AdoptReady   = "ready"   // dry run: this would be created
	AdoptAdopted = "adopted" // the project exists now
	AdoptSkipped = "skipped"
	AdoptFailed  = "failed"
)

// AdoptItem is the outcome for one container.
type AdoptItem struct {
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name"`
	Compose       string `json:"compose_project,omitempty"`
	Status        string `json:"status"`
	// Reason explains a skipped or failed container.
	Reason      string   `json:"reason,omitempty"`
	ProjectName string   `json:"project_name,omitempty"`
	ProjectID   string   `json:"project_id,omitempty"`
	Image       string   `json:"image,omitempty"`
	Ports       []string `json:"ports,omitempty"`
	Volumes     []string `json:"volumes,omitempty"`
	Env         int      `json:"env_count"`
	// Warnings are things a later redeploy would not carry over.
	Warnings []string `json:"warnings,omitempty"`
}

type adoptRequest struct {
	Containers []struct {
		ID string `json:"id"`
		// Name is the project name; empty derives it from the container name.
		Name string `json:"name"`
	} `json:"containers"`
	// DryRun reads the containers and reports what would be created, without creating anything.
	DryRun bool `json:"dry_run"`
}

// projectNameFor turns a container name into a valid project name.
func projectNameFor(container string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(container) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	return name
}

// waitCommand polls until the agent has answered a command.
func (s *Server) waitCommand(ctx context.Context, id string, limit time.Duration) (*api.Command, error) {
	deadline := time.Now().Add(limit)
	for {
		c, err := s.store.GetCommand(ctx, id)
		if err != nil {
			return nil, err
		}
		if c.Status == api.CommandSucceeded || c.Status == api.CommandFailed {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("the device did not answer in time")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (s *Server) handleAdopt(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req adoptRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.Containers) == 0 {
		writeError(w, http.StatusBadRequest, "choose at least one container")
		return
	}
	if len(req.Containers) > 50 {
		writeError(w, http.StatusBadRequest, "adopt at most 50 containers at a time")
		return
	}
	if !slices.Contains(dev.Host.Capabilities, api.CapInspect) {
		writeError(w, http.StatusConflict, "this device's agent is too old to adopt containers; update it from the Updates page first")
		return
	}
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	taken := map[string]bool{}
	managed := map[string]string{} // container name on this device -> project
	for _, p := range projects {
		taken[p.Name] = true
		if p.DeviceID == dev.ID {
			managed[serviceOf(p)] = p.Name
		}
	}

	items := make([]AdoptItem, len(req.Containers))
	cmds := make([]*api.Command, len(req.Containers))
	for i, rc := range req.Containers {
		it := &items[i]
		it.ContainerID = rc.ID
		var c *api.Container
		for j := range dev.Containers {
			if dev.Containers[j].ID == rc.ID {
				c = &dev.Containers[j]
			}
		}
		if c == nil {
			it.Status, it.Reason = AdoptFailed, "container is not known on this device"
			continue
		}
		it.ContainerName, it.Compose = c.Name, c.ComposeProject
		switch {
		case c.Labels["orchestrator.project"] != "" || managed[c.Name] != "":
			it.Status, it.Reason = AdoptSkipped, "already managed by a project"
		case strings.HasPrefix(c.Name, "orchestrator"):
			it.Status, it.Reason = AdoptSkipped, "part of Orchestrator itself"
		}
		if it.Status != "" {
			continue
		}
		cmd, err := s.store.CreateCommand(r.Context(), dev.ID, api.ActionInspect, c.ID, nil)
		if err != nil {
			s.internal(w, err)
			return
		}
		cmds[i] = cmd
	}

	for i, cmd := range cmds {
		if cmd == nil {
			continue
		}
		it := &items[i]
		done, err := s.waitCommand(r.Context(), cmd.ID, 45*time.Second)
		if err != nil {
			it.Status, it.Reason = AdoptFailed, err.Error()
			continue
		}
		if done.Status == api.CommandFailed {
			it.Status, it.Reason = AdoptFailed, done.Error
			continue
		}
		var spec api.ContainerSpec
		if err := json.Unmarshal([]byte(done.Output), &spec); err != nil {
			it.Status, it.Reason = AdoptFailed, "the agent sent an unreadable answer"
			continue
		}
		name := strings.TrimSpace(req.Containers[i].Name)
		if name == "" {
			name = projectNameFor(spec.Name)
		}
		s.adoptOne(r.Context(), dev, it, &spec, name, taken, req.DryRun)
	}
	writeJSON(w, http.StatusOK, items)
}

// adoptOne checks one inspected container and, unless dryRun, creates its project.
func (s *Server) adoptOne(ctx context.Context, dev *store.Device, it *AdoptItem, spec *api.ContainerSpec, name string, taken map[string]bool, dryRun bool) {
	switch {
	case !projectNameRe.MatchString(name):
		it.Status, it.Reason = AdoptFailed, fmt.Sprintf("%q is not a valid project name (lowercase letters, digits and dashes)", name)
		return
	case taken[name]:
		it.Status, it.Reason = AdoptFailed, "a project named "+name+" already exists; choose another name"
		return
	case spec.Image == "" || strings.HasPrefix(spec.Image, "sha256:"):
		it.Status, it.Reason = AdoptFailed, "the container was created from an image ID, not a tag, so it can't be pulled again"
		return
	}
	restart := spec.Restart
	if !restarts[restart] {
		restart = "unless-stopped"
	}
	p := &store.Project{
		Name:     name,
		DeviceID: dev.ID,
		Source:   store.Source{Type: store.SourceImage, Image: spec.Image},
		Runtime: store.Runtime{
			Ports: cleanLabels(spec.Ports), Env: cleanLabels(spec.Env), Command: spec.Command, Restart: restart,
			Volumes: spec.Volumes, Labels: spec.Labels, Network: spec.Network,
			Adopted: true, Unsupported: spec.Unsupported,
		},
	}
	if spec.Name != serviceName(name) {
		p.Runtime.Container = spec.Name
	}
	it.ProjectName, it.Image, it.Ports, it.Volumes, it.Env = name, spec.Image, p.Runtime.Ports, p.Runtime.Volumes, len(p.Runtime.Env)
	it.Warnings = slices.Clone(spec.Unsupported)
	if dryRun {
		it.Status = AdoptReady
		return
	}
	if err := s.store.CreateProject(ctx, p); err != nil {
		it.Status, it.Reason = AdoptFailed, "could not create the project: "+err.Error()
		return
	}
	taken[name] = true
	it.Status, it.ProjectID = AdoptAdopted, p.ID
	s.event(ctx, dev.ID, "info", "project.adopted", fmt.Sprintf("Container %s on %s adopted as project %s (left running as it was)", spec.Name, dev.Name, name))
}

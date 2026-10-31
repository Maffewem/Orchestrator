package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

const maxComposeBytes = 256 << 10

var (
	composeServicesRe = regexp.MustCompile(`(?m)^services:\s*(#.*)?$`)
	// ${NAME:?message} fails when NAME is unset or empty, ${NAME?message} when it is unset.
	composeRequiredRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:?)\?([^}]*)\}`)
)

// checkComposeFile rejects input that cannot be a compose file. The agent runs `docker compose
// config` for the real validation; this catches the obvious mistakes before a deployment is queued.
func checkComposeFile(yaml string) error {
	switch {
	case strings.TrimSpace(yaml) == "":
		return errors.New("paste a docker-compose file")
	case len(yaml) > maxComposeBytes:
		return errors.New("the compose file is too large (256 KB at most)")
	case strings.Contains(yaml, "\x00"):
		return errors.New("the compose file contains binary data")
	case !composeServicesRe.MatchString(yaml):
		return errors.New("the compose file needs a top-level \"services:\" section")
	}
	return nil
}

// checkComposeVars makes sure every ${VAR:?message} in the file has a value in env, so a
// missing secret is reported with the file's own message before anything runs on the device.
func checkComposeVars(yaml string, env []string) error {
	have := map[string]string{}
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			have[k] = v
		}
	}
	var missing []string
	for _, m := range composeRequiredRe.FindAllStringSubmatch(stripYAMLComments(yaml), -1) {
		v, set := have[m[1]]
		if set && (v != "" || m[2] == "") {
			continue
		}
		msg := strings.TrimSpace(m[3])
		if msg == "" {
			msg = "is required"
		}
		missing = append(missing, fmt.Sprintf("%s (%s)", m[1], msg))
	}
	if len(missing) > 0 {
		return errors.New("set these in the environment variables first: " + strings.Join(dedupe(missing), ", "))
	}
	return nil
}

// stripYAMLComments drops whole-line comments so a commented-out service does not demand its variables.
func stripYAMLComments(yaml string) string {
	lines := strings.Split(yaml, "\n")
	out := lines[:0]
	for _, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// handleStackAction starts, stops or tears down all containers of a compose project.
func (s *Server) handleStackAction(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupDeviceProject(w, r)
	if !ok {
		return
	}
	var req struct {
		Action  string `json:"action"`
		Volumes bool   `json:"volumes"`
	}
	if !decode(w, r, &req) {
		return
	}
	if p.Source.Type != store.SourceCompose {
		writeError(w, http.StatusBadRequest, "only compose projects are stacks")
		return
	}
	action := map[string]string{"start": api.ActionStackStart, "stop": api.ActionStackStop, "down": api.ActionStackDown}[req.Action]
	if action == "" {
		writeError(w, http.StatusBadRequest, "action must be start, stop or down")
		return
	}
	d, err := s.store.GetDevice(r.Context(), p.DeviceID)
	if err != nil {
		writeError(w, http.StatusConflict, "the project's device no longer exists")
		return
	}
	if !hasCap(d, api.CapCompose) {
		writeError(w, http.StatusConflict, "this device's agent is too old for stacks; update it from the Updates page first")
		return
	}
	args := map[string]string{}
	if req.Volumes && action == api.ActionStackDown {
		args["volumes"] = "delete"
	}
	c, err := s.store.CreateCommand(r.Context(), d.ID, action, p.Name, args)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), d.ID, "info", "stack."+req.Action, "Stack "+p.Name+": "+req.Action)
	writeJSON(w, http.StatusAccepted, c)
}

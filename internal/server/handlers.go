package server

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// DeviceView is a device as the dashboard sees it, with derived status and health.
type DeviceView struct {
	*store.Device
	Status string `json:"status"` // online | offline
	Health string `json:"health"` // ok | warning | critical
	// Issues explains a non-ok health, Server Manager style.
	Issues []string `json:"issues"`
}

func (s *Server) view(d *store.Device) DeviceView {
	v := DeviceView{Device: d, Status: "online", Health: "ok", Issues: []string{}}
	if d.Offline || time.Since(d.LastSeen) > s.offlineAfter() {
		v.Status = "offline"
		v.Health = "critical"
		v.Issues = append(v.Issues, "Agent is not responding")
		return v
	}
	raise := func(level, issue string) {
		v.Issues = append(v.Issues, issue)
		if level == "critical" || v.Health == "ok" {
			v.Health = level
		}
	}
	if d.Host.Docker == nil || d.Host.Docker.Error != "" {
		raise("warning", "Docker engine is unreachable")
	}
	check := func(name string, used, total uint64) {
		if total == 0 {
			return
		}
		pct := float64(used) / float64(total) * 100
		switch {
		case pct >= 97:
			raise("critical", fmt.Sprintf("%s usage is %.0f%%", name, pct))
		case pct >= 90:
			raise("warning", fmt.Sprintf("%s usage is %.0f%%", name, pct))
		}
	}
	check("Memory", d.Metrics.MemoryUsed, d.Metrics.MemoryTotal)
	check("Disk", d.Metrics.DiskUsed, d.Metrics.DiskTotal)
	return v
}

// --- agent API ---

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req api.RegisterRequest
	if !decode(w, r, &req) {
		return
	}
	join, err := s.store.GetSetting(r.Context(), settingJoinToken)
	if err != nil {
		s.internal(w, err)
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.JoinToken), []byte(join)) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid join token")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = req.Host.Hostname
	}
	if name == "" {
		name = "unnamed-device"
	}
	d, token, err := s.store.CreateDevice(r.Context(), name, cleanLabels(req.Labels), req.Host)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), d.ID, "info", "device.registered", fmt.Sprintf("%s joined the fleet", d.Name))
	writeJSON(w, http.StatusCreated, api.RegisterResponse{DeviceID: d.ID, AgentToken: token})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	d := deviceFrom(r)
	var req api.HeartbeatRequest
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	if err := s.store.UpdateDeviceState(ctx, d.ID, req.Host, req.Metrics, req.Containers); err != nil {
		s.internal(w, err)
		return
	}
	if d.Offline {
		s.event(ctx, d.ID, "info", "device.online", d.Name+" is back online")
	}
	s.diffContainers(r, d, req.Containers)

	s.metrics.add(d.ID, req.Metrics)

	cmds, err := s.store.DispatchCommands(ctx, d.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	withCredentials(cmds, s.githubToken(ctx))
	writeJSON(w, http.StatusOK, api.HeartbeatResponse{
		Commands:        cmds,
		IntervalSeconds: int(s.cfg.HeartbeatInterval / time.Second),
	})
}

// diffContainers turns container state changes into activity events.
func (s *Server) diffContainers(r *http.Request, d *store.Device, now []api.Container) {
	// The first snapshot after registration is not a change.
	if len(d.Containers) == 0 {
		return
	}
	before := make(map[string]api.Container, len(d.Containers))
	for _, c := range d.Containers {
		before[c.ID] = c
	}
	for _, c := range now {
		old, ok := before[c.ID]
		delete(before, c.ID)
		switch {
		case !ok:
			s.event(r.Context(), d.ID, "info", "container.created", fmt.Sprintf("Container %s created (%s)", c.Name, c.Image))
		case old.State != c.State:
			level := "info"
			if c.State == "exited" || c.State == "dead" {
				level = "warning"
			}
			s.event(r.Context(), d.ID, level, "container.state", fmt.Sprintf("Container %s is now %s", c.Name, c.State))
		}
	}
	for _, c := range before {
		s.event(r.Context(), d.ID, "info", "container.removed", fmt.Sprintf("Container %s removed", c.Name))
	}
}

func (s *Server) handleCommandResult(w http.ResponseWriter, r *http.Request) {
	d := deviceFrom(r)
	var res api.CommandResult
	if !decode(w, r, &res) {
		return
	}
	c, err := s.store.CompleteCommand(r.Context(), d.ID, r.PathValue("id"), res)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such command")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	switch c.Action {
	case api.ActionLogs:
	case api.ActionDeploy:
		s.finishDeployment(r.Context(), d, c, res)
	case api.ActionBackup:
		if !res.OK {
			s.backupCommandFailed(r, c, res)
			s.event(r.Context(), d.ID, "error", "backup.failed", fmt.Sprintf("Backup of %s failed: %s", containerName(d, c.Target), res.Error))
		}
	case api.ActionAgentUpdate:
		if res.OK {
			s.event(r.Context(), d.ID, "info", "agent.update", fmt.Sprintf("Agent on %s: %s", d.Name, res.Output))
		} else {
			s.event(r.Context(), d.ID, "error", "agent.update", fmt.Sprintf("Agent update on %s failed: %s", d.Name, res.Error))
		}
	case api.ActionProxySync:
		if res.OK {
			s.event(r.Context(), d.ID, "info", "proxy.sync", "Domain proxy on "+d.Name+" updated")
		} else {
			s.event(r.Context(), d.ID, "error", "proxy.sync", fmt.Sprintf("Domain proxy on %s failed: %s", d.Name, res.Error))
		}
	case api.ActionRemove:
		if !res.OK {
			s.event(r.Context(), d.ID, "error", "container.remove", fmt.Sprintf("Removing %s failed: %s", c.Target, res.Error))
		}
	case api.ActionRestore:
		lvl, msg := "info", "restored from backup"
		if !res.OK {
			lvl, msg = "error", "restore failed: "+res.Error
		}
		s.event(r.Context(), d.ID, lvl, "backup.restore", fmt.Sprintf("%s %s", containerName(d, c.Target), msg))
	default:
		verb := strings.TrimPrefix(c.Action, "container.")
		if res.OK {
			s.event(r.Context(), d.ID, "info", "command."+verb, fmt.Sprintf("%s %s succeeded", verb, containerName(d, c.Target)))
		} else {
			s.event(r.Context(), d.ID, "error", "command."+verb, fmt.Sprintf("%s %s failed: %s", verb, containerName(d, c.Target), res.Error))
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func containerName(d *store.Device, id string) string {
	for _, c := range d.Containers {
		if c.ID == id {
			return c.Name
		}
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// --- admin API ---

type overview struct {
	Devices struct {
		Total    int `json:"total"`
		Online   int `json:"online"`
		Offline  int `json:"offline"`
		Warning  int `json:"warning"`
		Critical int `json:"critical"`
	} `json:"devices"`
	Containers struct {
		Total   int `json:"total"`
		Running int `json:"running"`
		Stopped int `json:"stopped"`
	} `json:"containers"`
	RecentEvents []store.Event `json:"recent_events"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.ListDevices(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	var o overview
	for _, d := range devices {
		v := s.view(d)
		o.Devices.Total++
		if v.Status == "online" {
			o.Devices.Online++
		} else {
			o.Devices.Offline++
		}
		switch v.Health {
		case "warning":
			o.Devices.Warning++
		case "critical":
			o.Devices.Critical++
		}
		for _, c := range d.Containers {
			o.Containers.Total++
			if c.State == "running" {
				o.Containers.Running++
			} else {
				o.Containers.Stopped++
			}
		}
	}
	if o.RecentEvents, err = s.store.ListEvents(r.Context(), "", 10); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.ListDevices(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]DeviceView, 0, len(devices))
	for _, d := range devices {
		out = append(out, s.view(d))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) lookupDevice(w http.ResponseWriter, r *http.Request) (*store.Device, bool) {
	d, err := s.store.GetDevice(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such device")
		return nil, false
	} else if err != nil {
		s.internal(w, err)
		return nil, false
	}
	return d, true
}

func (s *Server) handleGetDevice(w http.ResponseWriter, r *http.Request) {
	if d, ok := s.lookupDevice(w, r); ok {
		writeJSON(w, http.StatusOK, s.view(d))
	}
}

func (s *Server) handleUpdateDevice(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Name   *string   `json:"name"`
		Labels *[]string `json:"labels"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "name cannot be empty")
			return
		}
		d.Name = name
	}
	if req.Labels != nil {
		d.Labels = cleanLabels(*req.Labels)
	}
	if err := s.store.UpdateDeviceMeta(r.Context(), d.ID, d.Name, d.Labels); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(d))
}

func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteDevice(r.Context(), d.ID); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "warning", "device.removed", d.Name+" was removed from the fleet")
	w.WriteHeader(http.StatusNoContent)
}

var allowedActions = map[string]bool{
	api.ActionStart:   true,
	api.ActionStop:    true,
	api.ActionRestart: true,
	api.ActionLogs:    true,
}

func (s *Server) handleCreateCommand(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Action string            `json:"action"`
		Target string            `json:"target"`
		Args   map[string]string `json:"args"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !allowedActions[req.Action] {
		writeError(w, http.StatusBadRequest, "unsupported action")
		return
	}
	known := false
	for _, c := range d.Containers {
		if c.ID == req.Target {
			known = true
			break
		}
	}
	if !known {
		writeError(w, http.StatusBadRequest, "container is not known on this device")
		return
	}
	c, err := s.store.CreateCommand(r.Context(), d.ID, req.Action, req.Target, req.Args)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, c)
}

func (s *Server) handleGetCommand(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetCommand(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such command")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

type fleetContainer struct {
	api.Container
	DeviceID        string   `json:"device_id"`
	DeviceName      string   `json:"device_name"`
	DeviceStatus    string   `json:"device_status"`
	DeviceAddresses []string `json:"device_addresses"`
	// ProjectID/ProjectName are set for containers deployed by Orchestrator.
	ProjectID   string `json:"project_id,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
}

func (s *Server) handleListContainers(w http.ResponseWriter, r *http.Request) {
	devices, err := s.store.ListDevices(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	projects, err := s.store.ListProjects(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	names := map[string]string{}
	for _, p := range projects {
		names[p.ID] = p.Name
	}
	out := []fleetContainer{}
	for _, d := range devices {
		status := s.view(d).Status
		addrs := d.Host.Addresses
		if addrs == nil {
			addrs = []string{}
		}
		for _, c := range d.Containers {
			fc := fleetContainer{Container: c, DeviceID: d.ID, DeviceName: d.Name, DeviceStatus: status, DeviceAddresses: addrs}
			if id := c.Labels["orchestrator.project"]; names[id] != "" {
				fc.ProjectID, fc.ProjectName = id, names[id]
			}
			out = append(out, fc)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.ListEvents(r.Context(), r.URL.Query().Get("device"), limit)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	join, err := s.store.GetSetting(r.Context(), settingJoinToken)
	if err != nil {
		s.internal(w, err)
		return
	}
	secret, err := s.store.GetSetting(r.Context(), settingWebhookSecret)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"join_token":                 join,
		"heartbeat_interval_seconds": int(s.cfg.HeartbeatInterval / time.Second),
		"version":                    s.cfg.Version,
		"base_domain":                s.baseDomain(r.Context()),
		"backup_password_set":        s.backupPassword(r.Context()) != "",
		"cloudflare":                 s.cloudflareStatus(r.Context()),
		"ssl_mode":                   s.sslMode(r.Context()),
		"github": map[string]any{
			"connected":      s.githubToken(r.Context()) != "",
			"webhook_path":   "/api/github/webhook",
			"webhook_secret": secret,
		},
	})
}

// handleSetGitHub stores (or, with an empty token, removes) the GitHub access token.
func (s *Server) handleSetGitHub(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	tok := strings.TrimSpace(req.Token)
	if err := s.store.SetSetting(r.Context(), settingGitHubToken, tok); err != nil {
		s.internal(w, err)
		return
	}
	if tok != "" {
		var me struct {
			Login string `json:"login"`
		}
		if err := s.githubGet(r.Context(), "/user", &me); err != nil {
			s.store.SetSetting(r.Context(), settingGitHubToken, "")
			writeError(w, http.StatusBadRequest, "GitHub rejected the token: "+err.Error())
			return
		}
		s.event(r.Context(), "", "info", "settings.github", "GitHub connected as "+me.Login)
		writeJSON(w, http.StatusOK, map[string]any{"connected": true, "login": me.Login})
		return
	}
	s.event(r.Context(), "", "info", "settings.github", "GitHub disconnected")
	writeJSON(w, http.StatusOK, map[string]any{"connected": false})
}

func (s *Server) handleRotateJoinToken(w http.ResponseWriter, r *http.Request) {
	tok := store.NewToken()
	if err := s.store.SetSetting(r.Context(), settingJoinToken, tok); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "info", "settings.join_token", "Join token was rotated")
	writeJSON(w, http.StatusOK, map[string]string{"join_token": tok})
}

func cleanLabels(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, l := range in {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

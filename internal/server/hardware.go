package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Device hardware: identify and LED rules, and SSH control. All of it is administrator-only, goes
// through the device's agent and is recorded in the device's Activity. The console is a stream
// (see streams.go).

// identifyHub remembers which devices are flashing their LEDs, so every open dashboard can highlight them.
type identifyHub struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (h *identifyHub) set(id string, t time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.until == nil {
		h.until = map[string]time.Time{}
	}
	h.until[id] = t
}

func (h *identifyHub) get(id string) *time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.until[id]
	if !ok {
		return nil
	}
	if time.Now().After(t) {
		delete(h.until, id)
		return nil
	}
	return &t
}

func (h *identifyHub) clear(id string) {
	h.mu.Lock()
	delete(h.until, id)
	h.mu.Unlock()
}

func actorName(r *http.Request) string {
	if id := identityFrom(r); id != nil {
		return id.Name
	}
	return "token"
}

// hwCommand sends an agent command and waits for its answer, writing the error response itself.
func (s *Server) hwCommand(w http.ResponseWriter, r *http.Request, d *store.Device, capName, feature, action string, args map[string]string) (*api.Command, bool) {
	if !hasCap(d, capName) {
		writeError(w, http.StatusConflict, errAgentTooOld(d, feature).Error())
		return nil, false
	}
	if d.Offline || time.Since(d.LastSeen) > s.offlineAfter() {
		writeError(w, http.StatusConflict, d.Name+" is offline")
		return nil, false
	}
	cmd, err := s.store.CreateCommand(r.Context(), d.ID, action, "", args)
	if err != nil {
		s.internal(w, err)
		return nil, false
	}
	done, err := s.waitCommand(r.Context(), cmd.ID, 40*time.Second)
	switch {
	case err != nil:
		writeError(w, http.StatusGatewayTimeout, err.Error())
		return nil, false
	case done.Status == api.CommandFailed:
		code := http.StatusBadGateway
		if strings.HasPrefix(done.Error, "refused:") {
			code = http.StatusConflict
		}
		writeError(w, code, done.Error)
		return nil, false
	}
	return done, true
}

// passthrough answers with the agent's JSON as it is.
func passthrough(w http.ResponseWriter, c *api.Command) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if c.Output == "" {
		w.Write([]byte("{}"))
		return
	}
	w.Write([]byte(c.Output))
}

// --- LEDs ---

func (s *Server) handleListLEDs(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	if c, ok := s.hwCommand(w, r, d, api.CapLED, "LED control", api.ActionLEDList, nil); ok {
		passthrough(w, c)
	}
}

func (s *Server) handleFlashLEDs(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Seconds int    `json:"seconds"`
		LED     string `json:"led"`
		Stop    bool   `json:"stop"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Stop {
		s.flashing.clear(d.ID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if req.Seconds <= 0 {
		req.Seconds = 15
	}
	if req.Seconds > 60 {
		req.Seconds = 60
	}
	args := map[string]string{"seconds": strconv.Itoa(req.Seconds)}
	if req.LED != "" {
		args["led"] = req.LED
	}
	c, ok := s.hwCommand(w, r, d, api.CapLED, "LED control", api.ActionLEDIdentify, args)
	if !ok {
		return
	}
	secs, _ := strconv.Atoi(c.Output)
	if secs <= 0 {
		secs = req.Seconds
	}
	until := time.Now().Add(time.Duration(secs) * time.Second)
	s.flashing.set(d.ID, until)
	s.event(r.Context(), d.ID, "info", "led.identify", fmt.Sprintf("%s flashed the LEDs on %s for %ds", actorName(r), d.Name, secs))
	writeJSON(w, http.StatusOK, map[string]any{"until": until})
}

func (s *Server) handleSetLEDConfig(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var cfg api.LEDConfig
	if !decode(w, r, &cfg) {
		return
	}
	b, _ := json.Marshal(cfg)
	if _, ok := s.hwCommand(w, r, d, api.CapLED, "LED control", api.ActionLEDConfig, map[string]string{"config": string(b)}); !ok {
		return
	}
	state := "off"
	if cfg.Enabled {
		state = fmt.Sprintf("on, %d rule(s), profile %s", len(cfg.Rules), cfg.Profile)
	}
	s.event(r.Context(), d.ID, "info", "led.config", fmt.Sprintf("%s changed the LED rules on %s: %s", actorName(r), d.Name, state))
	if c, ok := s.hwCommand(w, r, d, api.CapLED, "LED control", api.ActionLEDList, nil); ok {
		passthrough(w, c)
	}
}

// --- SSH ---

func (s *Server) handleSSHStatus(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	if c, ok := s.hwCommand(w, r, d, api.CapSSH, "SSH control", api.ActionSSHStatus, nil); ok {
		passthrough(w, c)
	}
}

type sshSettingsReq struct {
	api.SSHSettings
	// Confirm is the device's name, typed back, for a change that could lock everyone out.
	Confirm string `json:"confirm"`
	// TrialSeconds is how long the change stays on trial before it reverts by itself (0 keeps it at once).
	TrialSeconds *int `json:"trial_seconds"`
}

func (s *Server) handleSSHCheck(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req sshSettingsReq
	if !decode(w, r, &req) {
		return
	}
	b, _ := json.Marshal(req.SSHSettings)
	if c, ok := s.hwCommand(w, r, d, api.CapSSH, "SSH control", api.ActionSSHCheck, map[string]string{"settings": string(b)}); ok {
		passthrough(w, c)
	}
}

func confirmed(typed string, d *store.Device) bool {
	return strings.TrimSpace(typed) != "" && strings.EqualFold(strings.TrimSpace(typed), d.Name)
}

func (s *Server) handleSSHConfig(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req sshSettingsReq
	if !decode(w, r, &req) {
		return
	}
	trial := 120
	if req.TrialSeconds != nil {
		trial = max(0, min(*req.TrialSeconds, 600))
	}
	b, _ := json.Marshal(req.SSHSettings)
	args := map[string]string{"settings": string(b), "trial": strconv.Itoa(trial)}
	if confirmed(req.Confirm, d) {
		args["confirm"] = "1"
	}
	c, ok := s.hwCommand(w, r, d, api.CapSSH, "SSH control", api.ActionSSHConfig, args)
	if !ok {
		return
	}
	s.event(r.Context(), d.ID, "warning", "ssh.config", fmt.Sprintf("%s set SSH on %s: password sign-in %s, key sign-in %s, root login %s (%s)",
		actorName(r), d.Name, onOff(req.PasswordAuth), onOff(req.PubkeyAuth), req.RootLogin, c.Output))
	writeJSON(w, http.StatusOK, map[string]string{"result": c.Output})
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (s *Server) handleSSHConfirm(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		Revert bool `json:"revert"`
	}
	if !decode(w, r, &req) {
		return
	}
	args := map[string]string{}
	if req.Revert {
		args["revert"] = "1"
	}
	c, ok := s.hwCommand(w, r, d, api.CapSSH, "SSH control", api.ActionSSHConfirm, args)
	if !ok {
		return
	}
	s.event(r.Context(), d.ID, "warning", "ssh.confirm", fmt.Sprintf("%s: SSH change on %s %s", actorName(r), d.Name, c.Output))
	writeJSON(w, http.StatusOK, map[string]string{"result": c.Output})
}

func (s *Server) handleSSHAddKey(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		User string `json:"user"`
		Key  string `json:"key"`
	}
	if !decode(w, r, &req) {
		return
	}
	c, ok := s.hwCommand(w, r, d, api.CapSSH, "SSH control", api.ActionSSHKey, map[string]string{"op": "add", "user": req.User, "key": req.Key})
	if !ok {
		return
	}
	s.event(r.Context(), d.ID, "warning", "ssh.key.add", fmt.Sprintf("%s authorized an SSH key (%s) for %s on %s", actorName(r), c.Output, req.User, d.Name))
	writeJSON(w, http.StatusOK, map[string]string{"fingerprint": c.Output})
}

func (s *Server) handleSSHRemoveKey(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req struct {
		User        string `json:"user"`
		Fingerprint string `json:"fingerprint"`
		Confirm     string `json:"confirm"`
	}
	if !decode(w, r, &req) {
		return
	}
	args := map[string]string{"op": "remove", "user": req.User, "fingerprint": req.Fingerprint}
	if confirmed(req.Confirm, d) {
		args["confirm"] = "1"
	}
	if _, ok := s.hwCommand(w, r, d, api.CapSSH, "SSH control", api.ActionSSHKey, args); !ok {
		return
	}
	s.event(r.Context(), d.ID, "warning", "ssh.key.remove", fmt.Sprintf("%s removed SSH key %s of %s on %s", actorName(r), req.Fingerprint, req.User, d.Name))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

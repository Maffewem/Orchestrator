package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/selfupdate"
	"github.com/Maffewem/Orchestrator/internal/store"
)

const (
	settingUpdateSchedule = "update_schedule"
	settingUpdateSetAt    = "update_schedule_set_at"
	settingUpdateLastAuto = "update_last_auto"
	settingUpdatePending  = "update_pending"
	settingUpdateAgents   = "update_agents_pending"
	settingUpdateCheck    = "update_check"
	settingUpdateResult   = "update_result"
	settingAgentUpdate    = "agent_update_" // + device id: progress of the last agent update

	// An agent update that has not finished after this long is reported as failed.
	agentUpdateTimeout = 12 * time.Minute
	// How long a finished agent update stays visible.
	agentUpdateKeep = 10 * time.Minute

	// Automatic agent updates retry a device at most this often.
	agentRetryAfter = 6 * time.Hour
)

type updateState struct {
	mu         sync.Mutex
	busy       string // "checking" | "updating"
	stage      string // while busy == "updating": "pulling" | "handover"
	self       *selfupdate.Self
	selfReason string
	selfAt     time.Time
	agentTried map[string]time.Time
	sums       map[string]sumEntry
}

type sumEntry struct {
	mod time.Time
	sum string
}

type updateCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Version   string    `json:"version,omitempty"`
	Newer     bool      `json:"newer"`
	Error     string    `json:"error,omitempty"`
}

type updateResult struct {
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Message string    `json:"message"`
}

type updatePending struct {
	FromVersion string    `json:"from_version"`
	FromImage   string    `json:"from_image"`
	To          string    `json:"to"`
	At          time.Time `json:"at"`
}

func (s *Server) getJSON(ctx context.Context, key string, v any) bool {
	raw, err := s.store.GetSetting(ctx, key)
	return err == nil && raw != "" && json.Unmarshal([]byte(raw), v) == nil
}

func (s *Server) setJSON(ctx context.Context, key string, v any) {
	b, _ := json.Marshal(v)
	if err := s.store.SetSetting(ctx, key, string(b)); err != nil {
		s.log.Error("save setting", "key", key, "err", err)
	}
}

func (s *Server) loadSchedule(ctx context.Context) Schedule {
	sch := defaultSchedule()
	s.getJSON(ctx, settingUpdateSchedule, &sch)
	return sch
}

func (s *Server) setResult(ctx context.Context, ok bool, msg string) {
	s.setJSON(ctx, settingUpdateResult, updateResult{At: time.Now().UTC(), OK: ok, Message: msg})
	level := "info"
	if !ok {
		level = "error"
	}
	s.event(ctx, "", level, "update", msg)
}

// selfInfo finds out (and briefly remembers) whether this server can replace its own container.
func (s *Server) selfInfo(ctx context.Context) (*selfupdate.Self, string) {
	s.up.mu.Lock()
	defer s.up.mu.Unlock()
	if time.Since(s.up.selfAt) < 30*time.Second {
		return s.up.self, s.up.selfReason
	}
	s.up.self, s.up.selfReason, s.up.selfAt = nil, "", time.Now()
	if s.docker == nil {
		s.up.selfReason = "Docker is not configured"
		return nil, s.up.selfReason
	}
	self, err := selfupdate.Identify(ctx, s.docker)
	if err != nil {
		s.up.selfReason = err.Error()
		return nil, s.up.selfReason
	}
	s.up.self = self
	return self, ""
}

// begin marks a long-running update task as started; it returns false if one is running.
func (s *Server) begin(kind string) bool {
	s.up.mu.Lock()
	defer s.up.mu.Unlock()
	if s.up.busy != "" {
		return false
	}
	s.up.busy = kind
	return true
}

func (s *Server) finish() {
	s.up.mu.Lock()
	s.up.busy = ""
	s.up.mu.Unlock()
}

func (s *Server) agentOutdated(d *store.Device) bool {
	v := s.cfg.Version
	return v != "" && v != "dev" && d.Host.AgentVersion != "" && d.Host.AgentVersion != v
}

// --- status ---

type agentUpdateView struct {
	DeviceID    string `json:"device_id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Arch        string `json:"arch"`
	Online      bool   `json:"online"`
	InContainer bool   `json:"in_container"`
	Outdated    bool   `json:"outdated"`
	CanUpdate   bool   `json:"can_update"` // agent understands agent.update
	// Progress of an update started from the dashboard, kept on the server because the
	// agent disconnects while it restarts.
	Progress *agentProgress `json:"progress,omitempty"`
}

// agentUpdateRec is what the server remembers about an agent update it queued.
type agentUpdateRec struct {
	CommandID string    `json:"command_id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	At        time.Time `json:"at"`
}

// agentProgress is the derived state: queued | downloading | restarting | done | failed.
type agentProgress struct {
	State   string    `json:"state"`
	Message string    `json:"message"`
	To      string    `json:"to"`
	At      time.Time `json:"at"`
}

// agentProgress works the state out from the stored command and the version the device
// reports, so it survives the agent (and the server) restarting mid-update.
func (s *Server) agentProgress(ctx context.Context, d *store.Device) *agentProgress {
	var rec agentUpdateRec
	if !s.getJSON(ctx, settingAgentUpdate+d.ID, &rec) {
		return nil
	}
	p := &agentProgress{To: rec.To, At: rec.At}
	if d.Host.AgentVersion != "" && d.Host.AgentVersion == rec.To {
		p.State, p.Message = "done", "Reconnected on "+rec.To
		if time.Since(rec.At) > agentUpdateKeep {
			return nil
		}
		return p
	}
	age := time.Since(rec.At)
	cmd, err := s.store.GetCommand(ctx, rec.CommandID)
	switch {
	case err != nil:
		p.State, p.Message = "failed", "The update command was lost"
	case cmd.Status == api.CommandFailed:
		p.State, p.Message = "failed", orDefault(cmd.Error, "The agent reported a failure")
	case age > agentUpdateTimeout:
		p.State = "failed"
		if cmd.Status == api.CommandSucceeded {
			p.Message = "The agent restarted but did not come back on " + rec.To + "; it may have rolled back"
		} else {
			p.Message = "The agent did not respond to the update"
		}
	case cmd.Status == api.CommandSucceeded:
		p.State, p.Message = "restarting", "Restarting; waiting for it to reconnect"
	case cmd.Status == api.CommandDispatched:
		p.State, p.Message = "downloading", "Downloading and checking the new build"
	default:
		p.State, p.Message = "queued", "Queued; starts at the agent's next check-in"
	}
	return p
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

type updateStatus struct {
	Version   string `json:"version"`
	Image     string `json:"image,omitempty"`
	CanUpdate bool   `json:"can_update"`
	Reason    string `json:"reason,omitempty"`
	Busy      string `json:"busy,omitempty"`
	Updating  bool   `json:"updating"`
	// Stage is where a server update is: pulling | handover | restarting.
	Stage      string            `json:"stage,omitempty"`
	Check      *updateCheck      `json:"check,omitempty"`
	Result     *updateResult     `json:"result,omitempty"`
	Schedule   Schedule          `json:"schedule"`
	NextRun    *time.Time        `json:"next_run,omitempty"`
	Agents     []agentUpdateView `json:"agents"`
	ServerTime time.Time         `json:"server_time"`
}

func (s *Server) updateStatus(ctx context.Context) updateStatus {
	st := updateStatus{Version: s.cfg.Version, Schedule: s.loadSchedule(ctx), Agents: []agentUpdateView{}, ServerTime: time.Now().UTC()}
	self, reason := s.selfInfo(ctx)
	if self != nil {
		st.CanUpdate, st.Image = true, self.ImageRef
	} else {
		st.Reason = reason
	}
	s.up.mu.Lock()
	st.Busy = s.up.busy
	if st.Busy == "updating" {
		st.Stage = s.up.stage
	}
	s.up.mu.Unlock()
	var chk updateCheck
	if s.getJSON(ctx, settingUpdateCheck, &chk) {
		st.Check = &chk
	}
	var res updateResult
	if s.getJSON(ctx, settingUpdateResult, &res) {
		st.Result = &res
	}
	var p updatePending
	if s.getJSON(ctx, settingUpdatePending, &p) && time.Since(p.At) < 10*time.Minute {
		st.Updating, st.Stage = true, "restarting"
	}
	if next, ok := st.Schedule.nextSlot(time.Now()); ok {
		next = next.UTC()
		st.NextRun = &next
	}
	if devs, err := s.store.ListDevices(ctx); err == nil {
		for _, d := range devs {
			v := s.view(d)
			st.Agents = append(st.Agents, agentUpdateView{
				DeviceID: d.ID, Name: d.Name, Version: d.Host.AgentVersion, Arch: d.Host.Arch,
				Online: v.Status == "online", InContainer: d.Host.InContainer, Outdated: s.agentOutdated(d), CanUpdate: hasCap(d, api.CapUpdate),
			})
		}
	}
	return st
}

func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.updateStatus(r.Context()))
}

func (s *Server) handleSetUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	var sch Schedule
	if !decode(w, r, &sch) {
		return
	}
	if err := sch.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.setJSON(r.Context(), settingUpdateSchedule, sch)
	// A new schedule waits for its next slot instead of firing for one that already passed.
	s.store.SetSetting(r.Context(), settingUpdateSetAt, time.Now().UTC().Format(time.RFC3339))
	writeJSON(w, http.StatusOK, s.updateStatus(r.Context()))
}

// --- check and apply ---

func (s *Server) runCheck(ctx context.Context) (*selfupdate.Latest, error) {
	self, reason := s.selfInfo(ctx)
	chk := updateCheck{CheckedAt: time.Now().UTC()}
	if self == nil {
		chk.Error = reason
		s.setJSON(ctx, settingUpdateCheck, chk)
		return nil, errors.New(reason)
	}
	// Pulling a big image can take minutes on a Pi; it must not die with a request.
	latest, err := selfupdate.Check(ctx, s.docker, self)
	if err != nil {
		chk.Error = err.Error()
		s.setJSON(ctx, settingUpdateCheck, chk)
		return nil, err
	}
	chk.Version, chk.Newer = latest.Version, latest.Newer
	s.setJSON(ctx, settingUpdateCheck, chk)
	return latest, nil
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !s.begin("checking") {
		writeError(w, http.StatusConflict, "an update task is already running")
		return
	}
	go func() {
		defer s.finish()
		if _, err := s.runCheck(context.Background()); err != nil {
			s.log.Warn("update check", "err", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, s.updateStatus(r.Context()))
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if self, reason := s.selfInfo(r.Context()); self == nil {
		writeError(w, http.StatusConflict, reason)
		return
	}
	if !s.begin("updating") {
		writeError(w, http.StatusConflict, "an update task is already running")
		return
	}
	withAgents := r.URL.Query().Get("agents") == "1"
	go func() {
		defer s.finish()
		s.applyServerUpdate(context.Background(), withAgents)
	}()
	writeJSON(w, http.StatusAccepted, s.updateStatus(r.Context()))
}

// applyServerUpdate pulls the newest image and hands over to the updater helper, which
// restarts the server on it. It returns after the helper has started (or failed to).
func (s *Server) applyServerUpdate(ctx context.Context, withAgents bool) bool {
	self, reason := s.selfInfo(ctx)
	if self == nil {
		s.setResult(ctx, false, "Update not started: "+reason)
		return false
	}
	s.setStage("pulling")
	latest, err := s.runCheck(ctx)
	if err != nil {
		s.setResult(ctx, false, "Update not started: "+err.Error())
		return false
	}
	if !latest.Newer {
		s.setResult(ctx, true, "Already on the latest build")
		return false
	}
	s.setJSON(ctx, settingUpdatePending, updatePending{FromVersion: s.cfg.Version, FromImage: self.ImageID, To: latest.Version, At: time.Now().UTC()})
	if withAgents {
		s.store.SetSetting(ctx, settingUpdateAgents, "1")
	}
	s.setStage("handover")
	s.event(ctx, "", "info", "update.started", fmt.Sprintf("Updating the server to %s; the dashboard restarts shortly", orUnknown(latest.Version)))
	if err := selfupdate.StartHelper(ctx, s.docker, self, latest.ImageID); err != nil {
		s.store.SetSetting(ctx, settingUpdatePending, "")
		s.store.SetSetting(ctx, settingUpdateAgents, "")
		s.setResult(ctx, false, "Update not started: "+err.Error())
		return false
	}
	return true
}

func (s *Server) setStage(v string) {
	s.up.mu.Lock()
	s.up.stage = v
	s.up.mu.Unlock()
}

func orUnknown(v string) string {
	if v == "" {
		return "the latest build"
	}
	return v
}

// afterBoot reports how a previous update ended, and finishes agent updates it queued.
func (s *Server) afterBoot(ctx context.Context) {
	var p updatePending
	if s.getJSON(ctx, settingUpdatePending, &p) {
		ok := s.cfg.Version != p.FromVersion
		if self, _ := s.selfInfo(ctx); self != nil {
			ok = self.ImageID != p.FromImage
		}
		// A server still on the old image this soon may just be restarting before the
		// helper swaps it; give the helper a moment. One already on a new image is done
		// (the swap finishes within seconds, so this must not wait for the settle time).
		if wait := 15*time.Second - time.Since(p.At); !ok && wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		s.store.SetSetting(ctx, settingUpdatePending, "")
		if ok {
			s.setResult(ctx, true, fmt.Sprintf("Updated the server from %s to %s", orUnknown(p.FromVersion), orUnknown(s.cfg.Version)))
		} else {
			s.store.SetSetting(ctx, settingUpdateAgents, "")
			s.setResult(ctx, false, fmt.Sprintf("The update to %s did not come up healthy, so the server was rolled back to %s", orUnknown(p.To), orUnknown(p.FromVersion)))
		}
	}
	if v, _ := s.store.GetSetting(ctx, settingUpdateAgents); v == "1" {
		s.store.SetSetting(ctx, settingUpdateAgents, "")
		select { // let the agents reconnect to the new server first
		case <-ctx.Done():
		case <-time.After(45 * time.Second):
			s.updateAgents(ctx, "", false)
		}
	}
}

// --- agents ---

// updateAgents queues an update for outdated, online agents (or just one device).
func (s *Server) updateAgents(ctx context.Context, only string, auto bool) int {
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		s.log.Error("update agents", "err", err)
		return 0
	}
	n := 0
	for _, d := range devs {
		if (only != "" && d.ID != only) || !s.agentOutdated(d) || d.Host.InContainer || !hasCap(d, api.CapUpdate) || s.view(d).Status != "online" {
			continue
		}
		s.up.mu.Lock()
		min := 2 * time.Minute
		if auto {
			min = agentRetryAfter
		}
		recent := time.Since(s.up.agentTried[d.ID]) < min
		if !recent {
			s.up.agentTried[d.ID] = time.Now()
		}
		s.up.mu.Unlock()
		if recent {
			continue
		}
		cmd, err := s.store.CreateCommand(ctx, d.ID, api.ActionAgentUpdate, "", nil)
		if err != nil {
			s.log.Error("queue agent update", "device", d.Name, "err", err)
			continue
		}
		s.setJSON(ctx, settingAgentUpdate+d.ID, agentUpdateRec{CommandID: cmd.ID, From: d.Host.AgentVersion, To: s.cfg.Version, At: time.Now().UTC()})
		s.event(ctx, d.ID, "info", "agent.update", fmt.Sprintf("Updating the agent on %s to %s", d.Name, s.cfg.Version))
		n++
	}
	return n
}

func (s *Server) handleUpdateAgents(w http.ResponseWriter, r *http.Request) {
	n := s.updateAgents(r.Context(), r.URL.Query().Get("device"), false)
	writeJSON(w, http.StatusAccepted, map[string]int{"queued": n})
}

// handleAgentUpdateInfo tells an agent which build is served for its architecture.
func (s *Server) handleAgentUpdateInfo(w http.ResponseWriter, r *http.Request) {
	arch := r.URL.Query().Get("arch")
	switch arch {
	case "amd64", "arm64", "armv7", "armv6":
	default:
		writeError(w, http.StatusBadRequest, "unknown architecture")
		return
	}
	name := "orchestrator-agent-linux-" + arch
	if s.cfg.DownloadsDir == "" {
		writeError(w, http.StatusNotFound, "this server has no agent builds")
		return
	}
	sum, err := s.fileSum(filepath.Join(s.cfg.DownloadsDir, name))
	if err != nil {
		writeError(w, http.StatusNotFound, "no agent build for "+arch)
		return
	}
	writeJSON(w, http.StatusOK, api.AgentUpdateInfo{Version: s.cfg.Version, SHA256: sum, URL: "/downloads/" + name})
}

func (s *Server) fileSum(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	s.up.mu.Lock()
	e, ok := s.up.sums[path]
	s.up.mu.Unlock()
	if ok && e.mod.Equal(fi.ModTime()) {
		return e.sum, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	s.up.mu.Lock()
	s.up.sums[path] = sumEntry{fi.ModTime(), sum}
	s.up.mu.Unlock()
	return sum, nil
}

// --- schedule ---

// updateLoop runs the scheduled check-and-update.
func (s *Server) updateLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.scheduleTick(ctx, time.Now())
		}
	}
}

func (s *Server) scheduleTick(ctx context.Context, now time.Time) {
	sch := s.loadSchedule(ctx)
	if sch.Mode == "off" {
		return
	}
	baseline := time.Time{}
	for _, key := range []string{settingUpdateSetAt, settingUpdateLastAuto} {
		if v, _ := s.store.GetSetting(ctx, key); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil && t.After(baseline) {
				baseline = t
			}
		}
	}
	if baseline.IsZero() { // schedule saved before baselines existed
		baseline = now
		s.store.SetSetting(ctx, settingUpdateSetAt, now.UTC().Format(time.RFC3339))
	}
	if !sch.due(baseline, now) || !s.begin("updating") {
		return
	}
	defer s.finish()
	s.store.SetSetting(ctx, settingUpdateLastAuto, now.UTC().Format(time.RFC3339))
	s.event(ctx, "", "info", "update.scheduled", "Scheduled update check started")
	if s.applyServerUpdate(ctx, sch.Agents) {
		return // the server restarts; agents follow once it is back
	}
	if sch.Agents {
		if n := s.updateAgents(ctx, "", true); n > 0 {
			s.event(ctx, "", "info", "update.agents", fmt.Sprintf("Updating %d agent(s) to match the server", n))
		}
	}
}

func newUpdateState() *updateState {
	return &updateState{agentTried: map[string]time.Time{}, sums: map[string]sumEntry{}}
}

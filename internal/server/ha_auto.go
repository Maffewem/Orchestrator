package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

// Automatic standby promotion (opt-in).
//
// A standby never promotes just because it cannot reach the active server: that could be a
// broken link between the two servers alone. It promotes only when
//
//  1. it has had no successful copy for AutoAfter seconds (default 45, at least 30),
//  2. the copy it holds lists at least haAutoMinFleet devices that were online, and
//  3. a strict majority of them have, in the last haKnockWindow, tried this standby's address,
//     which an agent only does after the active server failed to answer it.
//
// Fewer devices than haAutoMinFleet means there is no meaningful majority, so promotion stays
// manual and the note says so. Epoch fencing then protects against a second writer: agents obey
// only the highest epoch, and the old server turns itself into a standby when it comes back.

const (
	haAutoMinFleet  = 3
	haAutoDefault   = 45
	haAutoMinAfter  = 30
	haKnockWindow   = 90 * time.Second // agents retry an unreachable server every 15 to 30 s
	haAgentLiveness = 10 * time.Minute
)

// haAutoState is the standby's in-memory view of its agents.
type haAutoState struct {
	mu     sync.Mutex
	agents map[string]struct{}  // token hashes of devices in the last good copy
	knocks map[string]time.Time // when each of them last tried this standby
}

func autoAfter(st haState) int {
	if st.AutoAfter < haAutoMinAfter {
		return haAutoDefault
	}
	return st.AutoAfter
}

// haLoadAgents refreshes the expected agents from the standby's verified copy.
func (s *Server) haLoadAgents() {
	agents, err := store.SnapshotAgents(s.standbyPath(), haAgentLiveness)
	if err != nil {
		return
	}
	s.haAuto.mu.Lock()
	s.haAuto.agents = agents
	s.haAuto.mu.Unlock()
}

// haNoteKnock records that an agent known from the copy tried this standby.
func (s *Server) haNoteKnock(r *http.Request) {
	tok := bearer(r)
	if tok == "" {
		return
	}
	h := store.HashToken(tok)
	s.haAuto.mu.Lock()
	defer s.haAuto.mu.Unlock()
	if _, ok := s.haAuto.agents[h]; !ok {
		return
	}
	if s.haAuto.knocks == nil {
		s.haAuto.knocks = map[string]time.Time{}
	}
	s.haAuto.knocks[h] = time.Now()
}

// haKnockCount returns how many expected agents knocked recently and how many were expected.
func (s *Server) haKnockCount(now time.Time) (knocked, fleet int) {
	s.haAuto.mu.Lock()
	defer s.haAuto.mu.Unlock()
	for h := range s.haAuto.agents {
		fleet++
		if t, ok := s.haAuto.knocks[h]; ok && now.Sub(t) <= haKnockWindow {
			knocked++
		}
	}
	return
}

// haAutoDecide is the pure rule: should this standby promote now?
func haAutoDecide(st haState, now time.Time, knocked, fleet int, haveCopy bool) (bool, string) {
	if !st.AutoPromote || st.Role != haRoleStandby || st.Token == "" {
		return false, ""
	}
	if !haveCopy || st.LastSync.IsZero() {
		return false, "Automatic promotion is waiting for a first verified copy."
	}
	wait := time.Duration(autoAfter(st)) * time.Second
	if now.Sub(st.LastSync) < wait {
		return false, ""
	}
	if fleet < haAutoMinFleet {
		return false, fmt.Sprintf("The active server is unreachable but only %d device(s) are known, so there is no majority to confirm it: promote by hand if it is really down.", fleet)
	}
	if knocked*2 <= fleet {
		return false, fmt.Sprintf("The active server is unreachable from here, but only %d of %d devices also lost it, so it may still be running: not promoting.", knocked, fleet)
	}
	return true, fmt.Sprintf("Taking over automatically: no copy for %s and %d of %d devices cannot reach the active server.", now.Sub(st.LastSync).Round(time.Second), knocked, fleet)
}

// haAutoCheck runs on every standby tick after the pull.
func (s *Server) haAutoCheck(ctx context.Context) {
	st := s.loadHA(ctx)
	if st.SyncError == "" {
		// A good pull: remember the agents it lists, clear any note.
		s.haLoadAgents()
		if st.AutoNote != "" {
			st.AutoNote = ""
			s.saveHA(ctx, st)
		}
		return
	}
	if s.cfg.DBPath == "" || s.cfg.Restart == nil {
		return
	}
	s.haAuto.mu.Lock()
	unloaded := s.haAuto.agents == nil
	s.haAuto.mu.Unlock()
	if unloaded { // restarted while the active server was already down
		s.haLoadAgents()
	}
	_, statErr := os.Stat(s.standbyPath())
	knocked, fleet := s.haKnockCount(time.Now())
	ok, note := haAutoDecide(st, time.Now(), knocked, fleet, statErr == nil)
	if !ok {
		if note != st.AutoNote {
			st.AutoNote = note
			s.saveHA(ctx, st)
		}
		return
	}
	if err := s.stagePromotion(ctx, st, note); err != nil {
		s.log.Error("automatic promotion", "err", err)
		return
	}
	s.event(ctx, "", "error", "ha.auto_promote", note)
}

// handleHASetAuto turns automatic promotion on or off on a standby.
func (s *Server) handleHASetAuto(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled      bool `json:"enabled"`
		AfterSeconds int  `json:"after_seconds"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.AfterSeconds != 0 && (req.AfterSeconds < haAutoMinAfter || req.AfterSeconds > 3600) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("wait between %d and 3600 seconds", haAutoMinAfter))
		return
	}
	ctx := r.Context()
	st := s.loadHA(ctx)
	if st.Role != haRoleStandby {
		writeError(w, http.StatusConflict, "automatic promotion is a setting of the standby server: open this page on the standby")
		return
	}
	st.AutoPromote, st.AutoAfter, st.AutoNote = req.Enabled, req.AfterSeconds, ""
	s.saveHA(ctx, st)
	msg := "Automatic promotion turned off"
	if req.Enabled {
		msg = fmt.Sprintf("Automatic promotion turned on (after %d s)", autoAfter(st))
	}
	s.event(ctx, "", "info", "ha.auto", fmt.Sprintf("%s (%s)", msg, identityFrom(r).Name))
	writeJSON(w, http.StatusOK, s.haView(ctx))
}

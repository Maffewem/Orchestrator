package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/backupcrypt"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Server high availability (Beta): manual standby.
//
// A second server runs in standby: it is read-only, does no background work and answers no
// agents. Every haSyncEvery it pulls a sealed, verified snapshot of the active server's
// database (including the secret key, encrypted with the pairing token). Promote stages the
// newest verified snapshot as the next database and restarts; the promoted server carries a
// higher epoch and keeps telling the old server to step down until it answers. The old server
// then restarts as a standby of the new one.

const (
	settingHA     = "ha:state"
	haSyncEvery   = 15 * time.Second
	haRoleActive  = "active"
	haRoleStandby = "standby"

	haTokenHeader = "X-HA-Token"
	haEpochHeader = "X-Orch-Epoch"
)

// haState is this server's place in a pair.
type haState struct {
	Role  string `json:"role"` // active (default) or standby
	Epoch int    `json:"epoch"`
	// Token is the pairing secret both servers share; it also seals the snapshots in transit.
	Token string `json:"token,omitempty"`
	// PeerURL is the active server's address (on a standby) or the standby's (on an active, once known).
	PeerURL string `json:"peer_url,omitempty"`
	// SelfURL is how the other server reaches this one.
	SelfURL string `json:"self_url,omitempty"`
	// Previous is the server this one took over from; it is told to step down until it answers.
	Previous string `json:"previous,omitempty"`

	LastSync    time.Time `json:"last_sync,omitempty"`
	SyncError   string    `json:"sync_error,omitempty"`
	SyncVersion string    `json:"sync_version,omitempty"`
	SyncBytes   int64     `json:"sync_bytes,omitempty"`
}

type haPromotion struct {
	Epoch    int    `json:"epoch"`
	Previous string `json:"previous"`
	Token    string `json:"token"`
	SelfURL  string `json:"self_url"`
}

func (s *Server) loadHA(ctx context.Context) haState {
	var st haState
	s.getJSON(ctx, settingHA, &st)
	if st.Role == "" {
		st.Role = haRoleActive
	}
	if st.Epoch == 0 {
		st.Epoch = 1
	}
	return st
}

func (s *Server) saveHA(ctx context.Context, st haState) {
	s.setJSON(ctx, settingHA, st)
	r := st.Role
	s.haRole.Store(&r)
	s.haEpoch.Store(int64(st.Epoch))
}

// haStandby reports whether this server is a standby: read-only, with no background work.
func (s *Server) haStandby() bool {
	p := s.haRole.Load()
	return p != nil && *p == haRoleStandby
}

func (s *Server) standbyPath() string  { return s.cfg.DBPath + ".standby" }
func (s *Server) promotedPath() string { return s.cfg.DBPath + ".promoted" }

// loadHAState reads the role into memory and finishes a promotion that was staged before the
// restart.
func (s *Server) loadHAState(ctx context.Context) {
	st := s.loadHA(ctx)
	if s.cfg.DBPath != "" {
		if b, err := os.ReadFile(s.promotedPath()); err == nil {
			var pr haPromotion
			if json.Unmarshal(b, &pr) == nil {
				// The database now is the active server's snapshot: it says "active" already, but
				// its pairing belongs to the old server, so rebuild it from the promotion record.
				st = haState{Role: haRoleActive, Epoch: pr.Epoch, Token: pr.Token, SelfURL: pr.SelfURL, Previous: pr.Previous}
				s.event(ctx, "", "warning", "ha.promoted", fmt.Sprintf("This server took over as the active server (epoch %d)", pr.Epoch))
			}
			os.Remove(s.promotedPath())
			os.Remove(s.standbyPath())
		}
	}
	s.saveHA(ctx, st)
}

func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) peerAllowed(r *http.Request, st haState) bool {
	return tokenEqual(r.Header.Get(haTokenHeader), st.Token)
}

// haMiddleware makes a standby read-only and stamps the epoch on every response.
func (s *Server) haMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(haEpochHeader, strconv.FormatInt(s.haEpoch.Load(), 10))
		if s.haStandby() && strings.HasPrefix(r.URL.Path, "/api/") {
			p := r.URL.Path
			switch {
			case strings.HasPrefix(p, "/api/server-ha"), p == "/api/health", p == "/api/agent/identify", strings.HasPrefix(p, "/api/auth/"):
			case strings.HasPrefix(p, "/api/agent/"), p == "/api/github/webhook":
				writeError(w, http.StatusServiceUnavailable, "this is the standby server; devices should use the active server")
				return
			case r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions:
				writeError(w, http.StatusLocked, "this is the standby server and is read-only. Make changes on the active server, or promote this one from Settings > Server high availability")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// --- active side: serving snapshots ---

func (s *Server) handleHASnapshot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := s.loadHA(ctx)
	// A server that was just told to step down still serves its last data to the one taking over.
	if !s.peerAllowed(r, st) || (st.Role != haRoleActive && st.PeerURL == "") {
		writeError(w, http.StatusForbidden, "not allowed")
		return
	}
	if s.cfg.DBPath == "" {
		writeError(w, http.StatusConflict, "this server has no database file to copy")
		return
	}
	tmp := s.cfg.DBPath + ".ha-snapshot"
	os.Remove(tmp)
	defer func() { os.Remove(tmp); os.Remove(tmp + "-wal"); os.Remove(tmp + "-shm") }()
	if err := s.store.Snapshot(ctx, tmp, true); err != nil {
		s.internal(w, err)
		return
	}
	f, err := os.Open(tmp)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer f.Close()
	var sealed bytes.Buffer
	enc, err := backupcrypt.NewWriter(&sealed, st.Token)
	if err != nil {
		s.internal(w, err)
		return
	}
	if _, err := io.Copy(enc, f); err != nil {
		s.internal(w, err)
		return
	}
	if err := enc.Close(); err != nil {
		s.internal(w, err)
		return
	}
	s.haLastPull.Store(time.Now().Unix())
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Orch-Version", s.cfg.Version)
	w.Header().Set("Content-Length", strconv.Itoa(sealed.Len()))
	w.Write(sealed.Bytes())
}

// --- standby side: pulling ---

func (s *Server) haLoop(ctx context.Context) {
	t := time.NewTicker(s.haEvery())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if s.haStandby() {
				s.haPullOnce(ctx)
			} else {
				s.haFenceOnce(ctx)
			}
		}
	}
}

func (s *Server) haEvery() time.Duration {
	if s.haTick > 0 {
		return s.haTick
	}
	return haSyncEvery
}

// haPullOnce copies the active server's snapshot, checks that it opens and replaces the
// previous verified copy. A failed pull leaves the last good copy in place.
func (s *Server) haPullOnce(ctx context.Context) error {
	st := s.loadHA(ctx)
	err := s.pullSnapshot(ctx, &st)
	if err != nil {
		st.SyncError = err.Error()
	} else {
		st.SyncError, st.LastSync = "", time.Now().UTC()
	}
	s.saveHA(ctx, st)
	return err
}

func (s *Server) pullSnapshot(ctx context.Context, st *haState) error {
	if st.PeerURL == "" || s.cfg.DBPath == "" {
		return errors.New("not paired")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, st.PeerURL+"/api/server-ha/snapshot", nil)
	if err != nil {
		return err
	}
	req.Header.Set(haTokenHeader, st.Token)
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("the active server does not answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the active server refused the copy (HTTP %d)", resp.StatusCode)
	}
	plain, err := backupcrypt.NewReader(resp.Body, st.Token)
	if err != nil {
		return fmt.Errorf("could not open the copy: %w", err)
	}
	tmp := s.standbyPath() + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, plain)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	defer func() { os.Remove(tmp + "-wal"); os.Remove(tmp + "-shm") }()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("receiving the copy: %w", err)
	}
	if err := store.CheckSnapshot(tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("the copy is not usable: %w", err)
	}
	if e, _ := strconv.Atoi(resp.Header.Get(haEpochHeader)); e > st.Epoch {
		st.Epoch = e
	}
	st.SyncVersion, st.SyncBytes = resp.Header.Get("X-Orch-Version"), n
	return os.Rename(tmp, s.standbyPath())
}

// --- pairing ---

type haView struct {
	Role        string     `json:"role"`
	Epoch       int        `json:"epoch"`
	PeerURL     string     `json:"peer_url,omitempty"`
	SelfURL     string     `json:"self_url,omitempty"`
	Paired      bool       `json:"paired"`
	Token       string     `json:"token,omitempty"` // shown on the active server so it can be typed into the standby
	LastSync    time.Time  `json:"last_sync,omitempty"`
	SyncError   string     `json:"sync_error,omitempty"`
	SyncVersion string     `json:"sync_version,omitempty"`
	Ready       bool       `json:"ready"`                       // standby: a verified copy exists and is recent
	StandbySeen *time.Time `json:"standby_last_pull,omitempty"` // active: when the standby last fetched a copy
	Previous    string     `json:"previous,omitempty"`
	Version     string     `json:"version"`
	CanRestart  bool       `json:"can_restart"`
}

func (s *Server) haView(ctx context.Context) haView {
	st := s.loadHA(ctx)
	v := haView{Role: st.Role, Epoch: st.Epoch, PeerURL: st.PeerURL, SelfURL: st.SelfURL, Paired: st.Token != "", LastSync: st.LastSync, SyncError: st.SyncError, SyncVersion: st.SyncVersion, Previous: st.Previous, Version: s.cfg.Version, CanRestart: s.cfg.Restart != nil && s.cfg.DBPath != ""}
	if st.Role == haRoleActive {
		v.Token = st.Token
		if t := s.haLastPull.Load(); t > 0 {
			ts := time.Unix(t, 0).UTC()
			v.StandbySeen = &ts
		}
	} else if _, err := os.Stat(s.standbyPath()); err == nil && !st.LastSync.IsZero() && time.Since(st.LastSync) < 4*s.haEvery() {
		v.Ready = true
	}
	return v
}

func (s *Server) handleHAStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.haView(r.Context()))
}

// handleHAPair (active server) creates the pairing token to type into the standby.
func (s *Server) handleHAPair(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := s.loadHA(ctx)
	if st.Role != haRoleActive {
		writeError(w, http.StatusConflict, "only the active server can create a pairing token")
		return
	}
	st.Token = newToken()
	s.saveHA(ctx, st)
	s.event(ctx, "", "info", "ha.pair", "A pairing token for a standby server was created")
	writeJSON(w, http.StatusOK, s.haView(ctx))
}

type haJoinRequest struct {
	ActiveURL string `json:"active_url"`
	Token     string `json:"token"`
	SelfURL   string `json:"self_url"`
}

// handleHAJoin makes this server a standby of the active one. Everything on this server is
// replaced by the active server's data if it is ever promoted, so it should be a fresh server.
func (s *Server) handleHAJoin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req haJoinRequest
	if !decode(w, r, &req) {
		return
	}
	active, err := cleanBaseURL(req.ActiveURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "the active server's address: "+err.Error())
		return
	}
	self := ""
	if strings.TrimSpace(req.SelfURL) != "" {
		if self, err = cleanBaseURL(req.SelfURL); err != nil {
			writeError(w, http.StatusBadRequest, "this server's address: "+err.Error())
			return
		}
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "enter the pairing token from the active server")
		return
	}
	if cur := s.loadHA(ctx); cur.Role == haRoleActive && cur.Token != "" && cur.PeerURL == "" && s.haLastPull.Load() > 0 {
		writeError(w, http.StatusConflict, "this server is already the active side of a pair")
		return
	}
	st := haState{Role: haRoleStandby, Epoch: 1, Token: req.Token, PeerURL: active, SelfURL: self}
	if err := s.pullSnapshot(ctx, &st); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	st.LastSync = time.Now().UTC()
	s.saveHA(ctx, st)
	s.event(ctx, "", "warning", "ha.join", "This server is now a standby of "+active)
	writeJSON(w, http.StatusOK, s.haView(ctx))
}

// handleHALeave turns a standby back into an ordinary server without promoting a copy.
func (s *Server) handleHALeave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := s.loadHA(ctx)
	if st.Role != haRoleStandby {
		writeError(w, http.StatusConflict, "this server is not a standby")
		return
	}
	os.Remove(s.standbyPath())
	s.saveHA(ctx, haState{Role: haRoleActive, Epoch: st.Epoch})
	s.event(ctx, "", "warning", "ha.leave", "This server left its pair and is a standalone server again; restarting")
	s.restartSoon(500 * time.Millisecond)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "restarting"})
}

func (s *Server) restartSoon(after time.Duration) {
	if s.haRestartIn > 0 {
		after = s.haRestartIn
	}
	if s.cfg.Restart != nil {
		go func() {
			time.Sleep(after)
			s.cfg.Restart()
		}()
	}
}

// --- promotion ---

type haPromoteRequest struct {
	// ActiveIsDown confirms that the active server is stopped, so two servers never write.
	ActiveIsDown bool `json:"active_is_down"`
}

func (s *Server) handleHAPromote(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req haPromoteRequest
	if !decode(w, r, &req) {
		return
	}
	st := s.loadHA(ctx)
	if st.Role != haRoleStandby {
		writeError(w, http.StatusConflict, "this server is not a standby")
		return
	}
	if s.cfg.DBPath == "" || s.cfg.Restart == nil {
		writeError(w, http.StatusConflict, "this server can't restart itself, so it can't take over")
		return
	}
	if _, err := os.Stat(s.standbyPath()); err != nil {
		writeError(w, http.StatusConflict, "there is no verified copy of the active server yet")
		return
	}
	if !req.ActiveIsDown {
		// Planned switch: ask the active server to step down first, so nothing is lost or doubled.
		if err := s.askStepDown(ctx, st); err != nil {
			writeError(w, http.StatusConflict, "the active server could not be asked to step down ("+err.Error()+"). If it is really down, confirm that and promote anyway")
			return
		}
		// One last copy so the planned switch loses nothing.
		if err := s.pullSnapshot(ctx, &st); err != nil {
			writeError(w, http.StatusBadGateway, "the final copy failed: "+err.Error())
			return
		}
	}
	if err := copyFile(s.standbyPath(), store.PendingRestorePath(s.cfg.DBPath)); err != nil {
		s.internal(w, err)
		return
	}
	b, _ := json.Marshal(haPromotion{Epoch: st.Epoch + 1, Previous: st.PeerURL, Token: st.Token, SelfURL: st.SelfURL})
	if err := os.WriteFile(s.promotedPath(), b, 0o600); err != nil {
		s.internal(w, err)
		return
	}
	s.event(ctx, "", "warning", "ha.promote", "Promoting this standby to the active server; restarting")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "restarting"})
	s.restartSoon(500 * time.Millisecond)
}

type haFenceRequest struct {
	Epoch  int    `json:"epoch"`
	NewURL string `json:"new_url"`
}

func (s *Server) haPost(ctx context.Context, base, token, path string, body any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(haTokenHeader, token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *Server) askStepDown(ctx context.Context, st haState) error {
	return s.haPost(ctx, st.PeerURL, st.Token, "/api/server-ha/fence", haFenceRequest{Epoch: st.Epoch + 1, NewURL: st.SelfURL})
}

// handleHAFence is the old active server being told to step down because a higher epoch took over.
func (s *Server) handleHAFence(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := s.loadHA(ctx)
	var req haFenceRequest
	if !s.peerAllowed(r, st) || !decode(w, r, &req) {
		if !s.peerAllowed(r, st) {
			writeError(w, http.StatusForbidden, "not allowed")
		}
		return
	}
	if req.Epoch <= st.Epoch {
		writeError(w, http.StatusConflict, "that epoch is not newer than this server's")
		return
	}
	next := haState{Role: haRoleStandby, Epoch: req.Epoch, Token: st.Token, PeerURL: req.NewURL, SelfURL: st.PeerURL}
	if next.PeerURL == "" {
		writeError(w, http.StatusBadRequest, "the new server did not say where it can be reached")
		return
	}
	s.saveHA(ctx, next)
	s.event(ctx, "", "warning", "ha.fenced", fmt.Sprintf("Another server took over (epoch %d). This server is now its standby; restarting", req.Epoch))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	s.restartSoon(10 * time.Second) // long enough for the new server to fetch a last copy
}

// haFenceOnce runs on a freshly promoted server: until the old server has stepped down, keep telling it to.
func (s *Server) haFenceOnce(ctx context.Context) {
	st := s.loadHA(ctx)
	if st.Previous == "" || st.Token == "" {
		return
	}
	if st.SelfURL == "" {
		return // the old server can't be told where to find this one
	}
	if err := s.haPost(ctx, st.Previous, st.Token, "/api/server-ha/fence", haFenceRequest{Epoch: st.Epoch, NewURL: st.SelfURL}); err != nil {
		return
	}
	st.Previous = ""
	s.saveHA(ctx, st)
	s.event(ctx, "", "info", "ha.fenced", "The previous server stepped down and is now this server's standby")
}

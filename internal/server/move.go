package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/backupcrypt"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Moving the control plane ("promote a device to be the main server").
//
// The server being replaced (the "old" server) copies its whole database, including the key
// that seals secrets, into a password-protected transfer bundle and has an agent on the chosen
// device start a new server from it. The old server stays intact, but frozen, until the new one
// has been checked and the agents are switched over. Both servers can hand control back and
// forth afterwards: a server in standby only redirects agents to the server that took over, so
// there is never more than one server giving agents work.
//
// Phases of a move, as seen by the old server:
//
//	preparing -> installing -> verifying -> ready -> switched -> done
//	                                         (failed: nothing changed, move can be retried)
const (
	settingMove      = "server_move"       // the move in progress (old server)
	settingStandbyTo = "server_standby_to" // set while this server only redirects agents elsewhere
	settingMovedFrom = "server_moved_from" // set on the new server until the move is confirmed

	movePreparing  = "preparing"
	moveInstalling = "installing"
	moveVerifying  = "verifying"
	moveReady      = "ready"
	moveSwitched   = "switched"
	moveDone       = "done"
	moveFailed     = "failed"

	moveBundlePath = "/api/agent/server-move/bundle"
)

type serverMove struct {
	Phase      string    `json:"phase"`
	DeviceID   string    `json:"device_id"`
	DeviceName string    `json:"device_name"`
	NewURL     string    `json:"new_url"` // how the new server is reached directly, e.g. http://192.168.0.50:8080
	OldURL     string    `json:"old_url"` // how agents reach this server today
	Port       int       `json:"port"`
	Token      string    `json:"token,omitempty"`    // shared with the new server for hand-back
	Password   string    `json:"password,omitempty"` // the transfer bundle's one-off password
	Bundle     string    `json:"bundle,omitempty"`
	CommandID  string    `json:"command_id,omitempty"`
	PrevURLs   []string  `json:"prev_urls,omitempty"`
	Error      string    `json:"error,omitempty"`
	Steps      []string  `json:"steps"` // plain-language progress lines
	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// movedFrom is what the new server remembers about the server it took over from.
type movedFrom struct {
	OldURL string    `json:"old_url"`
	NewURL string    `json:"new_url"`
	Token  string    `json:"token"`
	At     time.Time `json:"at"`
}

func (m *serverMove) frozen() bool {
	switch m.Phase {
	case movePreparing, moveInstalling, moveVerifying, moveReady, moveSwitched, moveDone:
		return true
	}
	return false
}

func (s *Server) loadMove(ctx context.Context) *serverMove {
	var m serverMove
	if s.getJSON(ctx, settingMove, &m) && m.Phase != "" {
		return &m
	}
	return nil
}

func (s *Server) saveMove(ctx context.Context, m *serverMove) {
	if m == nil {
		s.store.SetSetting(ctx, settingMove, "")
		s.moveFrozen.Store(false)
		return
	}
	m.UpdatedAt = time.Now().UTC()
	b, _ := json.Marshal(m)
	s.store.SetSetting(ctx, settingMove, string(b))
	s.moveFrozen.Store(m.frozen())
}

func (s *Server) loadMovedFrom(ctx context.Context) *movedFrom {
	var m movedFrom
	if s.getJSON(ctx, settingMovedFrom, &m) && m.Token != "" {
		return &m
	}
	return nil
}

// setStandby makes this server redirect agents to url ("" ends standby).
func (s *Server) setStandby(ctx context.Context, to string) {
	s.store.SetSetting(ctx, settingStandbyTo, to)
	s.standbyTo.Store(&to)
}

// standby returns where this server sends agents, or "" when it is the active server.
func (s *Server) standby() string {
	if p := s.standbyTo.Load(); p != nil {
		return *p
	}
	return ""
}

// loadMoveState reads the move settings into the in-memory flags the request path checks.
func (s *Server) loadMoveState(ctx context.Context) {
	v, _ := s.store.GetSetting(ctx, settingStandbyTo)
	s.standbyTo.Store(&v)
	if m := s.loadMove(ctx); m != nil {
		s.moveFrozen.Store(m.frozen())
	}
}

// recoverMove runs at start: a move that was still working when the server stopped can't
// carry on, and nothing had been switched, so it is marked failed and the server unfreezes.
func (s *Server) recoverMove(ctx context.Context) {
	m := s.loadMove(ctx)
	if m == nil {
		return
	}
	switch m.Phase {
	case movePreparing, moveInstalling, moveVerifying:
		s.failMove(ctx, m, "The server restarted while the move was running. Nothing was changed on this server. If a new server was already started on "+m.DeviceName+", remove its container there before trying again.")
	}
}

// movePaused reports whether changes through the dashboard are paused.
func (s *Server) movePaused() bool { return s.moveFrozen.Load() || s.standby() != "" }

// pausedMiddleware refuses dashboard changes while a move is under way or the server is in
// standby. Agents, sign-in and the move endpoints themselves are exempt.
func (s *Server) pausedMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions &&
			strings.HasPrefix(r.URL.Path, "/api/") && s.movePaused() {
			p := r.URL.Path
			if !strings.HasPrefix(p, "/api/agent/") && !strings.HasPrefix(p, "/api/auth/") && !strings.HasPrefix(p, "/api/server-move") {
				msg := "Changes are paused while this server is being moved. Finish or cancel the move first (Settings > Move the server)."
				if to := s.standby(); to != "" {
					msg = "This server handed over to " + to + ". Make changes there, or take this server back from Move the server."
				}
				writeError(w, http.StatusLocked, msg)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// --- starting a move ---

type moveCandidate struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Online    bool     `json:"online"`
	Eligible  bool     `json:"eligible"`
	Reason    string   `json:"reason,omitempty"`
	Addresses []string `json:"addresses"`
	Arch      string   `json:"arch"`
}

type moveView struct {
	Move      *serverMove     `json:"move,omitempty"`
	StandbyTo string          `json:"standby_to,omitempty"`
	MovedFrom *movedFrom      `json:"moved_from,omitempty"`
	Devices   []moveCandidate `json:"devices"`
	// OnThisServer counts devices that are connected here right now.
	OnThisServer int    `json:"on_this_server"`
	Image        string `json:"image,omitempty"`
	CanMove      bool   `json:"can_move"`
	CannotWhy    string `json:"cannot_why,omitempty"`
}

func (s *Server) candidate(d *store.Device) moveCandidate {
	online := !d.Offline && time.Since(d.LastSeen) <= s.offlineAfter()
	c := moveCandidate{ID: d.ID, Name: d.Name, Online: online, Addresses: lanAddresses(d), Arch: d.Host.Arch}
	switch {
	case !online:
		c.Reason = "Offline"
	case !hasCap(d, api.CapServerInstall):
		c.Reason = "Its agent is too old. Re-run the install command on that device to update it"
	case d.Host.Docker == nil || d.Host.Docker.Error != "":
		c.Reason = "Docker is not available on it"
	case len(c.Addresses) == 0:
		c.Reason = "It reports no network address"
	default:
		c.Eligible = true
	}
	return c
}

func lanAddresses(d *store.Device) []string {
	var out []string
	for _, n := range d.Host.Network {
		if n.Address != "" {
			out = append(out, n.Address)
		}
	}
	if len(out) == 0 {
		out = append(out, d.Host.Addresses...)
	}
	return out
}

func (s *Server) moveView(ctx context.Context) moveView {
	v := moveView{StandbyTo: s.standby(), MovedFrom: s.loadMovedFrom(ctx)}
	if m := s.loadMove(ctx); m != nil {
		cp := *m
		cp.Token, cp.Password, cp.Bundle = "", "", ""
		v.Move = &cp
	}
	if v.MovedFrom != nil {
		mf := *v.MovedFrom
		mf.Token = ""
		v.MovedFrom = &mf
	}
	devs, _ := s.store.ListDevices(ctx)
	for _, d := range devs {
		v.Devices = append(v.Devices, s.candidate(d))
		if !d.Offline && time.Since(d.LastSeen) <= s.offlineAfter() {
			v.OnThisServer++
		}
	}
	if self, _ := s.selfInfo(ctx); self != nil {
		v.Image = self.ImageRef
	}
	v.CanMove = true
	if v.StandbyTo != "" {
		v.CanMove, v.CannotWhy = false, "This server has handed over to "+v.StandbyTo+"."
	} else if v.MovedFrom != nil {
		v.CanMove, v.CannotWhy = false, "This server took over from another one. Confirm the move or move back before starting another."
	} else if v.Move != nil && v.Move.frozen() {
		v.CanMove, v.CannotWhy = false, "A move is already in progress."
	}
	return v
}

func (s *Server) handleGetMove(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.moveView(r.Context()))
}

type startMoveRequest struct {
	DeviceID string `json:"device_id"`
	Port     int    `json:"port"`
	Address  string `json:"address"` // the device address the new server is reached on
	OldURL   string `json:"old_url"` // how agents reach this server today (the dashboard's own address)
	Image    string `json:"image"`   // optional; defaults to the image this server runs
}

func cleanBaseURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return "", fmt.Errorf("%q is not an address such as http://192.168.0.10:8080", raw)
	}
	return raw, nil
}

func (s *Server) handleStartMove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req startMoveRequest
	if !decode(w, r, &req) {
		return
	}
	view := s.moveView(ctx)
	if !view.CanMove {
		writeError(w, http.StatusConflict, view.CannotWhy)
		return
	}
	if s.cfg.DBPath == "" {
		writeError(w, http.StatusConflict, "this server runs without a database file, so it can't be copied")
		return
	}
	dev, err := s.store.GetDevice(ctx, req.DeviceID)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such device")
		return
	}
	if c := s.candidate(dev); !c.Eligible {
		writeError(w, http.StatusConflict, dev.Name+" can't become the server: "+strings.ToLower(c.Reason[:1])+c.Reason[1:])
		return
	}
	if req.Port == 0 {
		req.Port = 8080
	}
	if req.Port < 1 || req.Port > 65535 {
		writeError(w, http.StatusBadRequest, "port must be between 1 and 65535")
		return
	}
	addr := strings.TrimSpace(req.Address)
	if addr == "" {
		addr = lanAddresses(dev)[0]
	}
	newURL, err := cleanBaseURL(fmt.Sprintf("http://%s:%d", addr, req.Port))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	oldURL := dev.Host.Server
	if req.OldURL != "" {
		if oldURL, err = cleanBaseURL(req.OldURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if oldURL == "" {
		writeError(w, http.StatusBadRequest, "give the address agents use to reach this server today (old_url)")
		return
	}
	image := strings.TrimSpace(req.Image)
	if image == "" {
		if self, _ := s.selfInfo(ctx); self != nil {
			image = self.ImageRef
		}
	}
	if image == "" {
		writeError(w, http.StatusConflict, "can't tell which image this server runs. Enter it (for example ghcr.io/maffewem/orchestrator:stable)")
		return
	}
	if deps, cmds, err := s.store.BusyWork(ctx); err != nil {
		s.internal(w, err)
		return
	} else if deps+cmds > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("%d deployment(s) and %d command(s) are still running. Wait for them to finish, then start the move", deps, cmds))
		return
	}

	m := &serverMove{
		Phase: movePreparing, DeviceID: dev.ID, DeviceName: dev.Name, NewURL: newURL, OldURL: oldURL, Port: req.Port,
		Token: randHex(24), Password: randHex(24), PrevURLs: s.manualAgentURLs(ctx), StartedAt: time.Now().UTC(),
		Steps: []string{"Pausing changes on this server and copying its data"},
	}
	s.saveMove(ctx, m)
	s.event(ctx, dev.ID, "warn", "servermove.started", "Moving the server to "+dev.Name+"; changes are paused until it finishes")
	go s.runMove(context.Background(), m, image)
	writeJSON(w, http.StatusAccepted, s.moveView(ctx))
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) moveDir() string { return filepath.Join(s.cfg.BackupDir, "move") }

func (m *serverMove) step(s *Server, ctx context.Context, line string) {
	m.Steps = append(m.Steps, line)
	s.saveMove(ctx, m)
}

func (s *Server) failMove(ctx context.Context, m *serverMove, why string) {
	m.Phase, m.Error = moveFailed, why
	if m.Bundle != "" {
		os.Remove(filepath.Join(s.moveDir(), m.Bundle))
	}
	m.Password, m.Bundle = "", ""
	s.saveMove(ctx, m)
	s.event(ctx, m.DeviceID, "error", "servermove.failed", "Server move failed: "+why)
}

// makeBundle writes an encrypted copy of the database that carries the secret key.
func (s *Server) makeBundle(ctx context.Context, dir, password string, set map[string]string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".snapshot-%d.tmp", time.Now().UnixNano()))
	defer os.Remove(tmp)
	drop := []string{settingMove, settingStandbyTo, settingMovedFrom, "update_pending"}
	if err := s.store.SnapshotForMove(ctx, tmp, set, drop); err != nil {
		return "", fmt.Errorf("snapshot: %w", err)
	}
	name := "orchestrator-move-" + time.Now().UTC().Format("20060102-150405") + ".db.enc"
	final := filepath.Join(dir, name)
	in, err := os.Open(tmp)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(final, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	w, err := backupcrypt.NewWriter(out, password)
	if err != nil {
		out.Close()
		os.Remove(final)
		return "", err
	}
	_, err = io.Copy(w, in)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(final)
		return "", err
	}
	return name, nil
}

func (s *Server) runMove(ctx context.Context, m *serverMove, image string) {
	// 1. Copy.
	moved, _ := json.Marshal(movedFrom{OldURL: m.OldURL, NewURL: m.NewURL, Token: m.Token, At: time.Now().UTC()})
	urls := strings.Join(append([]string{m.NewURL}, m.OldURL), "\n")
	name, err := s.makeBundle(ctx, s.moveDir(), m.Password, map[string]string{settingMovedFrom: string(moved), settingAgentURLs: urls})
	if err != nil {
		s.failMove(ctx, m, "could not copy the data: "+err.Error())
		return
	}
	m.Bundle, m.Phase = name, moveInstalling
	m.step(s, ctx, "Data copied into an encrypted transfer bundle")

	// 2. Install on the device.
	cmd, err := s.store.CreateCommand(ctx, m.DeviceID, api.ActionServerInstall, "", map[string]string{
		"image": image, "port": fmt.Sprint(m.Port), "bundle": moveBundlePath, "password": m.Password,
	})
	if err != nil {
		s.failMove(ctx, m, "could not ask "+m.DeviceName+" to install the server: "+err.Error())
		return
	}
	m.CommandID = cmd.ID
	m.step(s, ctx, "Asked "+m.DeviceName+" to download the bundle and start the new server (this can take a few minutes while Docker pulls the image)")
	deadline := time.Now().Add(25 * time.Minute)
	for {
		c, err := s.store.GetCommand(ctx, cmd.ID)
		if err != nil {
			s.failMove(ctx, m, err.Error())
			return
		}
		if c.Status == api.CommandSucceeded {
			break
		}
		if c.Status == api.CommandFailed || c.Status == api.CommandCancelled {
			why := c.Error
			if why == "" {
				why = "the command " + c.Status
			}
			s.failMove(ctx, m, m.DeviceName+" could not start the new server: "+why)
			return
		}
		if time.Now().After(deadline) {
			s.failMove(ctx, m, m.DeviceName+" did not finish in time")
			return
		}
		time.Sleep(s.movePollEvery())
	}
	m.Phase = moveVerifying
	m.step(s, ctx, "New server started on "+m.DeviceName)

	// 3. Check it really is this server's data.
	if err := s.verifyNewServer(ctx, m.NewURL); err != nil {
		s.failMove(ctx, m, "the new server started but failed the check: "+err.Error())
		return
	}
	os.Remove(filepath.Join(s.moveDir(), m.Bundle))
	m.Bundle, m.Password = "", ""
	m.Phase = moveReady
	m.step(s, ctx, "The new server answers and proved it holds this server's identity. Nothing has switched yet")
	s.event(ctx, m.DeviceID, "info", "servermove.ready", "New server on "+m.DeviceName+" is ready; devices have not been switched yet")
}

func (s *Server) movePollEvery() time.Duration {
	if s.moveTick > 0 {
		return s.moveTick
	}
	return 2 * time.Second
}

// verifyNewServer waits for the new server to answer, then has it sign a fresh nonce with the
// identity key it was restored with. Only a server restored from this one's data can.
func (s *Server) verifyNewServer(ctx context.Context, base string) error {
	cl := &http.Client{Timeout: 10 * time.Second}
	var lastErr error
	for i := 0; i < 60; i++ {
		if i > 0 {
			time.Sleep(s.movePollEvery())
		}
		nonce := randHex(16)
		resp, err := cl.Get(base + "/api/agent/identify?nonce=" + nonce)
		if err != nil {
			lastErr = err
			continue
		}
		var res api.IdentifyResponse
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&res)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("unexpected answer (HTTP %d)", resp.StatusCode)
			continue
		}
		sig, err := base64.StdEncoding.DecodeString(res.Signature)
		if err != nil || !ed25519.Verify(s.identity.Public().(ed25519.PublicKey), api.IdentifyMessage(nonce), sig) {
			return errors.New("it answered with a different identity, so it was not restored from this server's data")
		}
		return nil
	}
	return fmt.Errorf("it never answered at %s: %v", base, lastErr)
}

// handleMoveBundle gives the target device the transfer bundle, and only while it is needed.
func (s *Server) handleMoveBundle(w http.ResponseWriter, r *http.Request) {
	m := s.loadMove(r.Context())
	if m == nil || m.Phase != moveInstalling || m.Bundle == "" || m.DeviceID != deviceFrom(r).ID {
		writeError(w, http.StatusForbidden, "no server move is waiting for this device")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, filepath.Join(s.moveDir(), m.Bundle))
}

// --- switching, confirming, going back ---

func (s *Server) handleMoveSwitch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m := s.loadMove(ctx)
	if m == nil || m.Phase != moveReady {
		writeError(w, http.StatusConflict, "the new server is not ready to take over yet")
		return
	}
	if err := s.verifyNewServer(ctx, m.NewURL); err != nil {
		writeError(w, http.StatusBadGateway, "the new server does not answer any more: "+err.Error())
		return
	}
	s.store.SetSetting(ctx, settingAgentURLs, strings.Join([]string{m.NewURL, m.OldURL}, "\n"))
	m.Phase = moveSwitched
	m.step(s, ctx, "Devices were told to connect to "+m.NewURL+". This server keeps its data and now only redirects them")
	s.setStandby(ctx, m.NewURL)
	s.event(ctx, m.DeviceID, "warn", "servermove.switched", "Devices are switching to the new server at "+m.NewURL)
	writeJSON(w, http.StatusOK, s.moveView(ctx))
}

// handleMoveFinish is the old server's own "everything works, finish the move" button.
func (s *Server) handleMoveFinish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m := s.loadMove(ctx)
	if m == nil || m.Phase != moveSwitched {
		writeError(w, http.StatusConflict, "switch the devices over first")
		return
	}
	s.markDone(ctx, m)
	writeJSON(w, http.StatusOK, s.moveView(ctx))
}

func (s *Server) markDone(ctx context.Context, m *serverMove) {
	m.Phase = moveDone
	m.step(s, ctx, "Move confirmed. This server stays as a standby that redirects devices; you can shut it down")
	s.event(ctx, m.DeviceID, "info", "servermove.done", "Server move to "+m.DeviceName+" confirmed")
}

// handleMoveCancel takes this server back: it ends standby, restores the address list and
// tells the new server (when it can be reached) to send its devices back here.
func (s *Server) handleMoveCancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	m := s.loadMove(ctx)
	if m == nil {
		writeError(w, http.StatusConflict, "no server move to cancel")
		return
	}
	switch m.Phase {
	case movePreparing, moveInstalling, moveVerifying:
		writeError(w, http.StatusConflict, "the new server is still being set up; wait for that to finish, then cancel")
		return
	}
	note := ""
	if m.Token != "" && (m.Phase == moveSwitched || m.Phase == moveDone || m.Phase == moveReady) {
		if err := s.peerCall(ctx, m.NewURL, m.Token, "standby", m.OldURL); err != nil {
			note = "The new server could not be reached to send its devices back (" + err.Error() + "). Devices come back by themselves once it is switched off."
		}
	}
	s.setStandby(ctx, "")
	s.store.SetSetting(ctx, settingAgentURLs, strings.Join(m.PrevURLs, "\n"))
	if m.Bundle != "" {
		os.Remove(filepath.Join(s.moveDir(), m.Bundle))
	}
	s.saveMove(ctx, nil)
	s.event(ctx, m.DeviceID, "warn", "servermove.cancelled", "Server move cancelled; this server is in charge again")
	v := s.moveView(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"view": v, "note": note})
}

// --- the new server's side ---

// handleMoveAccept is the new server confirming that it works: the old server is told the
// move is final and the banner goes away.
func (s *Server) handleMoveAccept(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	mf := s.loadMovedFrom(ctx)
	if mf == nil || s.standby() != "" {
		writeError(w, http.StatusConflict, "this server did not take over from another one")
		return
	}
	note := ""
	if err := s.peerCall(ctx, mf.OldURL, mf.Token, "finished", ""); err != nil {
		note = "The previous server could not be reached to tell it. It does not need to be: the move is final here."
	}
	s.store.SetSetting(ctx, settingAgentURLs, mf.NewURL)
	s.store.SetSetting(ctx, settingMovedFrom, "")
	s.event(ctx, "", "info", "servermove.accepted", "This server is now the main server (took over from "+mf.OldURL+")")
	writeJSON(w, http.StatusOK, map[string]any{"view": s.moveView(ctx), "note": note})
}

// handleMoveBack is the new server handing control back to the one it took over from.
func (s *Server) handleMoveBack(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	mf := s.loadMovedFrom(ctx)
	if mf == nil || s.standby() != "" {
		writeError(w, http.StatusConflict, "nothing to move back")
		return
	}
	s.setStandby(ctx, mf.OldURL)
	note := ""
	if err := s.peerCall(ctx, mf.OldURL, mf.Token, "resume", ""); err != nil {
		note = "The previous server could not be reached. Devices will go back to it when it is running again; on it, open Move the server and choose \"Take this server back\"."
	}
	s.event(ctx, "", "warn", "servermove.back", "Handing control back to "+mf.OldURL)
	writeJSON(w, http.StatusOK, map[string]any{"view": s.moveView(ctx), "note": note})
}

// --- the two servers talking to each other ---

type peerRequest struct {
	Action string `json:"action"` // standby | resume | finished
	To     string `json:"to,omitempty"`
}

func (s *Server) peerCall(ctx context.Context, base, token, action, to string) error {
	b, _ := json.Marshal(peerRequest{Action: action, To: to})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/server-move/peer", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Move-Token", token)
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

func tokenEqual(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// handlePeer is the only move endpoint that is not for a signed-in user. It is authenticated
// by the random token the two servers share from the moment of the copy.
func (s *Server) handlePeer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req peerRequest
	if !decode(w, r, &req) {
		return
	}
	token := r.Header.Get("X-Move-Token")
	m, mf := s.loadMove(ctx), s.loadMovedFrom(ctx)
	switch req.Action {
	case "standby": // the old server asks the new one to send its devices back
		if mf == nil || !tokenEqual(token, mf.Token) {
			writeError(w, http.StatusForbidden, "not allowed")
			return
		}
		if _, err := cleanBaseURL(req.To); err != nil || req.To != mf.OldURL {
			writeError(w, http.StatusBadRequest, "unexpected address")
			return
		}
		s.setStandby(ctx, mf.OldURL)
		s.event(ctx, "", "warn", "servermove.back", "The previous server took control back; devices are returning to it")
	case "resume", "finished":
		if m == nil || !tokenEqual(token, m.Token) {
			writeError(w, http.StatusForbidden, "not allowed")
			return
		}
		if req.Action == "finished" {
			s.markDone(ctx, m)
		} else {
			s.setStandby(ctx, "")
			s.store.SetSetting(ctx, settingAgentURLs, strings.Join(m.PrevURLs, "\n"))
			s.saveMove(ctx, nil)
			s.event(ctx, m.DeviceID, "warn", "servermove.cancelled", "The new server handed control back; this server is in charge again")
		}
	default:
		writeError(w, http.StatusBadRequest, "unknown action")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- manual export ---

// handleMoveExport streams a password-protected copy of this server for moving it by hand
// (install-server.sh --import on another machine). Unlike the guided move it freezes nothing,
// so anything changed after the copy is not in it.
func (s *Server) handleMoveExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "choose a password of at least 8 characters; the copy contains this server's secret key")
		return
	}
	dir := filepath.Join(s.moveDir(), "export")
	name, err := s.makeBundle(r.Context(), dir, req.Password, nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	p := filepath.Join(dir, name)
	defer os.Remove(p)
	s.event(r.Context(), "", "info", "servermove.export", "A transfer copy of this server was downloaded")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	http.ServeFile(w, r, p)
}

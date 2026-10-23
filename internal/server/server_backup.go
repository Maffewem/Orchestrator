package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/backupcrypt"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Backups of the Orchestrator server itself: a snapshot of its SQLite database (users,
// projects, settings, devices, history). Worker data is backed up separately.

const (
	settingServerBackupPolicy  = "server_backup_policy"
	settingServerBackupLastRun = "server_backup_last_run"
	serverBackupPrefix         = "orchestrator-"
)

// ServerBackupPolicy is the automatic schedule for server backups.
type ServerBackupPolicy struct {
	Mode    string `json:"mode"` // off | daily | weekly
	Time    string `json:"time"`
	Weekday int    `json:"weekday"`
	TZ      string `json:"tz"`
	Keep    int    `json:"keep"`
}

func defaultServerBackupPolicy() ServerBackupPolicy {
	return ServerBackupPolicy{Mode: "off", Time: "03:00", TZ: "UTC", Keep: 14}
}

func (p ServerBackupPolicy) schedule() Schedule {
	return Schedule{Mode: p.Mode, Time: p.Time, Weekday: p.Weekday, TZ: p.TZ}
}

func (p ServerBackupPolicy) validate() error {
	if err := p.schedule().validate(); err != nil {
		return err
	}
	if p.Keep < 1 || p.Keep > 365 {
		return errors.New("keep must be between 1 and 365")
	}
	return nil
}

func (s *Server) serverBackupPolicy(ctx context.Context) ServerBackupPolicy {
	pol := defaultServerBackupPolicy()
	if v, err := s.store.GetSetting(ctx, settingServerBackupPolicy); err == nil {
		json.Unmarshal([]byte(v), &pol)
	}
	return pol
}

func (s *Server) serverBackupDir() string { return filepath.Join(s.cfg.BackupDir, "server") }

type serverBackup struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Encrypted bool      `json:"encrypted"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) listServerBackups() []serverBackup {
	out := []serverBackup{}
	entries, _ := os.ReadDir(s.serverBackupDir())
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, serverBackupPrefix) || !(strings.HasSuffix(n, ".db") || strings.HasSuffix(n, ".db.enc")) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, serverBackup{Name: n, Size: info.Size(), Encrypted: strings.HasSuffix(n, ".enc"), CreatedAt: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

var serverBackupMu sync.Mutex

// createServerBackup snapshots the database into the server backup directory, encrypted when
// a backup password is set, and applies retention when scheduled.
func (s *Server) createServerBackup(ctx context.Context) (*serverBackup, error) {
	serverBackupMu.Lock()
	defer serverBackupMu.Unlock()
	dir := s.serverBackupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".snapshot-%d.tmp", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if err := s.store.Snapshot(ctx, tmp); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	name := serverBackupPrefix + time.Now().UTC().Format("20060102-150405") + ".db"
	pw := s.backupPassword(ctx)
	if pw != "" {
		name += ".enc"
	}
	in, err := os.Open(tmp)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	final := filepath.Join(dir, name)
	out, err := os.OpenFile(final+".part", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	var w io.WriteCloser = out
	if pw != "" {
		if w, err = backupcrypt.NewWriter(out, pw); err != nil {
			out.Close()
			return nil, err
		}
	}
	_, err = io.Copy(w, in)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if pw != "" {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		os.Remove(final + ".part")
		return nil, err
	}
	if err := os.Rename(final+".part", final); err != nil {
		return nil, err
	}
	info, _ := os.Stat(final)
	return &serverBackup{Name: name, Size: info.Size(), Encrypted: pw != "", CreatedAt: info.ModTime().UTC()}, nil
}

// pruneServerBackups keeps the newest keep backups.
func (s *Server) pruneServerBackups(keep int) {
	all := s.listServerBackups()
	for i := keep; i < len(all); i++ {
		os.Remove(filepath.Join(s.serverBackupDir(), all[i].Name))
	}
}

// serverBackupTick runs the scheduled server backup when it is due.
func (s *Server) serverBackupTick(ctx context.Context, now time.Time) {
	pol := s.serverBackupPolicy(ctx)
	if pol.Mode == "off" {
		return
	}
	var baseline time.Time
	if v, err := s.store.GetSetting(ctx, settingServerBackupLastRun); err == nil {
		baseline, _ = time.Parse(time.RFC3339, v)
	}
	if baseline.IsZero() {
		baseline = now // first run waits for the next slot after the schedule was enabled
		s.store.SetSetting(ctx, settingServerBackupLastRun, now.UTC().Format(time.RFC3339))
		return
	}
	if !pol.schedule().due(baseline, now) {
		return
	}
	s.store.SetSetting(ctx, settingServerBackupLastRun, now.UTC().Format(time.RFC3339))
	b, err := s.createServerBackup(ctx)
	if err != nil {
		s.event(ctx, "", "error", "serverbackup.failed", "Scheduled server backup failed: "+err.Error())
		return
	}
	s.pruneServerBackups(pol.Keep)
	go s.replicateServerBackup(b.Name, pol.Keep)
}

type serverBackupsView struct {
	Policy       ServerBackupPolicy `json:"policy"`
	NextRun      *time.Time         `json:"next_run,omitempty"`
	Backups      []serverBackup     `json:"backups"`
	PasswordSet  bool               `json:"password_set"`
	Dir          string             `json:"dir"`
	Destinations []destView         `json:"destinations"`
}

func (s *Server) serverBackupsView(ctx context.Context) serverBackupsView {
	v := serverBackupsView{Policy: s.serverBackupPolicy(ctx), Backups: s.listServerBackups(), PasswordSet: s.backupPassword(ctx) != "", Dir: s.serverBackupDir(), Destinations: s.destViews(ctx)}
	if next, ok := v.Policy.schedule().nextSlot(time.Now()); ok {
		v.NextRun = &next
	}
	return v
}

func (s *Server) handleListServerBackups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.serverBackupsView(r.Context()))
}

func (s *Server) handleCreateServerBackup(w http.ResponseWriter, r *http.Request) {
	b, err := s.createServerBackup(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "info", "serverbackup.created", "Server backup "+b.Name+" created")
	go s.replicateServerBackup(b.Name, s.serverBackupPolicy(context.Background()).Keep)
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) handleSetServerBackupPolicy(w http.ResponseWriter, r *http.Request) {
	var p ServerBackupPolicy
	if !decode(w, r, &p) {
		return
	}
	if err := p.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	b, _ := json.Marshal(p)
	if err := s.store.SetSetting(r.Context(), settingServerBackupPolicy, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	// The next slot counts from now, so enabling a schedule never fires an old slot at once.
	s.store.SetSetting(r.Context(), settingServerBackupLastRun, time.Now().UTC().Format(time.RFC3339))
	s.pruneServerBackups(p.Keep)
	writeJSON(w, http.StatusOK, s.serverBackupsView(r.Context()))
}

// serverBackupFile resolves a backup name from the URL to a path inside the backup directory.
func (s *Server) serverBackupFile(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if name != filepath.Base(name) || !strings.HasPrefix(name, serverBackupPrefix) {
		writeError(w, http.StatusBadRequest, "invalid backup name")
		return "", false
	}
	p := filepath.Join(s.serverBackupDir(), name)
	if _, err := os.Stat(p); err != nil {
		writeError(w, http.StatusNotFound, "no such backup")
		return "", false
	}
	return p, true
}

// handleDownloadServerBackup serves the file as stored: protected backups stay protected.
func (s *Server) handleDownloadServerBackup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.serverBackupFile(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(p)))
	http.ServeFile(w, r, p)
}

func (s *Server) handleDeleteServerBackup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.serverBackupFile(w, r)
	if !ok {
		return
	}
	if err := os.Remove(p); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRestoreServerBackup stages a stored backup as the next database and restarts the
// server: POST /api/server-backups/{name}/restore.
func (s *Server) handleRestoreServerBackup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.serverBackupFile(w, r)
	if !ok {
		return
	}
	f, err := os.Open(p)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer f.Close()
	s.stageRestore(w, r, f, strings.HasSuffix(p, ".enc"))
}

// handleRestoreServerUpload restores from an uploaded file (the request body).
func (s *Server) handleRestoreServerUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<30)
	s.stageRestore(w, r, r.Body, r.URL.Query().Get("encrypted") == "1")
}

func (s *Server) stageRestore(w http.ResponseWriter, r *http.Request, src io.Reader, encrypted bool) {
	if s.cfg.DBPath == "" || s.cfg.Restart == nil {
		writeError(w, http.StatusConflict, "this server can't restart itself, so it can't restore a backup")
		return
	}
	if encrypted {
		pw := s.backupPassword(r.Context())
		if pw == "" {
			writeError(w, http.StatusConflict, "this backup is password-protected and no backup password is set. Set the password it was made with in Settings")
			return
		}
		plain, err := backupcrypt.NewReader(src, pw)
		if err != nil {
			writeError(w, http.StatusConflict, "could not open this backup: "+err.Error()+". It was made with a different password: set that one in Settings to restore it")
			return
		}
		src = plain
	}
	pending := store.PendingRestorePath(s.cfg.DBPath)
	check := pending + ".check"
	defer os.Remove(check)
	out, err := os.OpenFile(check, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		s.internal(w, err)
		return
	}
	_, err = io.Copy(out, src)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the backup: "+err.Error())
		return
	}
	if err := store.CheckSnapshot(check); err != nil {
		writeError(w, http.StatusBadRequest, "that file is not a usable Orchestrator backup: "+err.Error())
		os.Remove(check + "-wal")
		os.Remove(check + "-shm")
		return
	}
	os.Remove(check + "-wal")
	os.Remove(check + "-shm")
	if err := os.Rename(check, pending); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "warn", "serverbackup.restore", "Restoring a server backup; restarting")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "restarting"})
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.cfg.Restart()
	}()
}

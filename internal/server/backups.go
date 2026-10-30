package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/backupcrypt"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// maxBackupBytes caps one uploaded backup archive.
const maxBackupBytes = 50 << 30

func (s *Server) backupPath(id string) string { return filepath.Join(s.cfg.BackupDir, id+".tar.gz") }

// backupFile is where a backup lives: password-protected ones get their own extension so a
// copy taken off the server is recognisable for what it is.
func (s *Server) backupFile(b *store.Backup) string {
	if b.Encrypted {
		return s.backupPath(b.ID) + ".enc"
	}
	return s.backupPath(b.ID)
}

func (s *Server) removeBackupFiles(id string) {
	os.Remove(s.backupPath(id))
	os.Remove(s.backupPath(id) + ".enc")
}

const settingBackupPassword = "backup_password"

func (s *Server) backupPassword(ctx context.Context) string {
	v, _ := s.store.GetSetting(ctx, settingBackupPassword)
	return v
}

func findContainer(d *store.Device, id string) *api.Container {
	for i := range d.Containers {
		if d.Containers[i].ID == id {
			return &d.Containers[i]
		}
	}
	return nil
}

func hasCap(d *store.Device, c string) bool {
	for _, have := range d.Host.Capabilities {
		if have == c {
			return true
		}
	}
	return false
}

// errAgentTooOld explains what to do when an agent predates a feature.
func errAgentTooOld(d *store.Device, feature string) error {
	how := "Update it from the Updates page, or re-run the install command on that device."
	if !hasCap(d, api.CapUpdate) {
		how = "It is too old to update from the dashboard: re-run the install command from Devices > Add device on that device."
	}
	return fmt.Errorf("the agent on %s is too old for %s. %s", d.Name, feature, how)
}

// createBackup records a backup and queues the agent command for it.
func (s *Server) createBackup(ctx context.Context, d *store.Device, c *api.Container, source string) (*store.Backup, error) {
	if !hasCap(d, api.CapBackup) {
		return nil, errAgentTooOld(d, "backups")
	}
	if err := os.MkdirAll(s.cfg.BackupDir, 0o700); err != nil {
		return nil, err
	}
	b := &store.Backup{DeviceID: d.ID, DeviceName: d.Name, ContainerName: c.Name, Image: c.Image, Source: source}
	if err := s.store.CreateBackup(ctx, b); err != nil {
		return nil, err
	}
	if _, err := s.store.CreateCommand(ctx, d.ID, api.ActionBackup, c.ID, map[string]string{"backup_id": b.ID}); err != nil {
		return nil, err
	}
	s.event(ctx, d.ID, "info", "backup.queued", fmt.Sprintf("Backup of %s queued (%s)", c.Name, source))
	return b, nil
}

// handleCreateBackup queues a backup of one container: POST /api/devices/{id}/containers/{cid}/backup.
func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	d, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	c := findContainer(d, r.PathValue("cid"))
	if c == nil {
		writeError(w, http.StatusBadRequest, "container is not known on this device")
		return
	}
	b, err := s.createBackup(r.Context(), d, c, "manual")
	if err != nil {
		if !hasCap(d, api.CapBackup) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, b)
}

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	bs, err := s.store.ListBackups(r.Context(), r.URL.Query().Get("device_id"))
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bs)
}

func (s *Server) lookupBackup(w http.ResponseWriter, r *http.Request) (*store.Backup, bool) {
	b, err := s.store.GetBackup(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such backup")
		return nil, false
	} else if err != nil {
		s.internal(w, err)
		return nil, false
	}
	return b, true
}

func (s *Server) serveBackup(w http.ResponseWriter, r *http.Request, b *store.Backup) {
	if b.Status != store.BackupComplete {
		writeError(w, http.StatusConflict, "backup is not complete")
		return
	}
	f, err := os.Open(s.backupFile(b))
	if err != nil {
		writeError(w, http.StatusNotFound, "backup file is missing")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.tar.gz"`, b.ContainerName, b.CreatedAt.Format("20060102-150405")))
	if !b.Encrypted {
		http.ServeContent(w, r, "", b.CreatedAt, f)
		return
	}
	// Protected backups are decrypted on the way out with the password in Settings. The
	// first chunk is checked before any bytes are sent, so a wrong password is a clean error.
	pw := s.backupPassword(r.Context())
	if pw == "" {
		writeError(w, http.StatusConflict, "this backup is password-protected and no backup password is set. Set the password it was made with in Settings")
		return
	}
	plain, err := backupcrypt.NewReader(f, pw)
	if err != nil {
		writeError(w, http.StatusConflict, "could not open this backup: "+err.Error()+". It was made with a different password: set that one in Settings to restore it")
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(b.Size, 10))
	io.Copy(w, plain)
}

func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	if b, ok := s.lookupBackup(w, r); ok {
		s.serveBackup(w, r, b)
	}
}

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	b, ok := s.lookupBackup(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteBackup(r.Context(), b.ID); err != nil {
		s.internal(w, err)
		return
	}
	s.removeBackupFiles(b.ID)
	w.WriteHeader(http.StatusNoContent)
}

// handleRestoreBackup queues a restore into the container that still carries the backed-up
// name on the original device: POST /api/backups/{id}/restore.
func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	b, ok := s.lookupBackup(w, r)
	if !ok {
		return
	}
	if b.Status != store.BackupComplete {
		writeError(w, http.StatusConflict, "backup is not complete")
		return
	}
	d, err := s.store.GetDevice(r.Context(), b.DeviceID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "the device this backup came from no longer exists")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	if !hasCap(d, api.CapBackup) {
		writeError(w, http.StatusConflict, errAgentTooOld(d, "restoring backups").Error())
		return
	}
	var target string
	for _, c := range d.Containers {
		if c.Name == b.ContainerName {
			target = c.ID
		}
	}
	if target == "" {
		writeError(w, http.StatusConflict, fmt.Sprintf("no container named %q on %s: deploy it again, then restore", b.ContainerName, d.Name))
		return
	}
	c, err := s.store.CreateCommand(r.Context(), d.ID, api.ActionRestore, target, map[string]string{"backup_id": b.ID})
	if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), d.ID, "warning", "backup.restore", fmt.Sprintf("Restoring %s from a backup taken %s", b.ContainerName, b.CreatedAt.Format("2 Jan 15:04")))
	writeJSON(w, http.StatusAccepted, c)
}

// --- agent side ---

func (s *Server) agentBackup(w http.ResponseWriter, r *http.Request) (*store.Backup, bool) {
	b, ok := s.lookupBackup(w, r)
	if !ok {
		return nil, false
	}
	if b.DeviceID != deviceFrom(r).ID && !(r.Method == http.MethodGet && s.mig.mayDownload(b.ID, deviceFrom(r).ID)) {
		writeError(w, http.StatusForbidden, "backup belongs to another device")
		return nil, false
	}
	return b, true
}

// handleUploadBackup receives the archive streamed by the agent.
func (s *Server) handleUploadBackup(w http.ResponseWriter, r *http.Request) {
	b, ok := s.agentBackup(w, r)
	if !ok {
		return
	}
	if b.Status != store.BackupPending {
		writeError(w, http.StatusConflict, "backup is not waiting for data")
		return
	}
	if err := os.MkdirAll(s.cfg.BackupDir, 0o700); err != nil {
		s.internal(w, err)
		return
	}
	tmp, err := os.CreateTemp(s.cfg.BackupDir, b.ID+".*.part")
	if err != nil {
		s.internal(w, err)
		return
	}
	defer os.Remove(tmp.Name())
	// With a backup password set, the archive is encrypted as it arrives: it never touches
	// the disk in the clear.
	pw := s.backupPassword(r.Context())
	var n int64
	if pw != "" {
		var enc io.WriteCloser
		if enc, err = backupcrypt.NewWriter(tmp, pw); err == nil {
			n, err = io.Copy(enc, http.MaxBytesReader(w, r.Body, maxBackupBytes))
			if cerr := enc.Close(); err == nil {
				err = cerr
			}
		}
	} else {
		n, err = io.Copy(tmp, http.MaxBytesReader(w, r.Body, maxBackupBytes))
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		s.store.FinishBackup(r.Context(), b.ID, 0, "upload failed: "+err.Error())
		writeError(w, http.StatusBadRequest, "upload failed")
		return
	}
	b.Encrypted = pw != ""
	if err := os.Rename(tmp.Name(), s.backupFile(b)); err != nil {
		s.internal(w, err)
		return
	}
	if err := s.store.FinishBackupAs(r.Context(), b.ID, n, "", b.Encrypted); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), b.DeviceID, "info", "backup.complete", fmt.Sprintf("Backup of %s finished (%s)", b.ContainerName, humanSize(n)))
	if b.Source == "scheduled" {
		s.pruneBackups(r.Context(), b.DeviceID, b.ContainerName)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentDownloadBackup(w http.ResponseWriter, r *http.Request) {
	if b, ok := s.agentBackup(w, r); ok {
		s.serveBackup(w, r, b)
	}
}

// backupCommandFailed marks a backup failed when the agent reports it couldn't finish.
func (s *Server) backupCommandFailed(r *http.Request, c *api.Command, res api.CommandResult) {
	if err := s.store.FinishBackup(r.Context(), c.Args["backup_id"], 0, res.Error); err != nil {
		s.log.Error("finish backup", "err", err)
	}
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

const minBackupPassword = 8

// handleSetBackupPassword sets (or, with an empty password, removes) the password new backups
// are encrypted with. Existing backups keep the password they were made with.
func (s *Server) handleSetBackupPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Password != "" && len(req.Password) < minBackupPassword {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the backup password must be at least %d characters", minBackupPassword))
		return
	}
	if err := s.store.SetSetting(r.Context(), settingBackupPassword, req.Password); err != nil {
		s.internal(w, err)
		return
	}
	msg := "Backup password removed: new backups are not encrypted"
	if req.Password != "" {
		msg = "Backup password set: new backups are encrypted"
	}
	s.event(r.Context(), "", "info", "backup.password", fmt.Sprintf("%s (%s)", msg, identityFrom(r).Name))
	writeJSON(w, http.StatusOK, map[string]bool{"backup_password_set": req.Password != ""})
}

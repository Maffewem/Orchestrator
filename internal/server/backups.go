package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// maxBackupBytes caps one uploaded backup archive.
const maxBackupBytes = 50 << 30

func (s *Server) backupPath(id string) string { return filepath.Join(s.cfg.BackupDir, id+".tar.gz") }

func findContainer(d *store.Device, id string) *api.Container {
	for i := range d.Containers {
		if d.Containers[i].ID == id {
			return &d.Containers[i]
		}
	}
	return nil
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
	if err := os.MkdirAll(s.cfg.BackupDir, 0o700); err != nil {
		s.internal(w, err)
		return
	}
	b := &store.Backup{DeviceID: d.ID, DeviceName: d.Name, ContainerName: c.Name, Image: c.Image}
	if err := s.store.CreateBackup(r.Context(), b); err != nil {
		s.internal(w, err)
		return
	}
	if _, err := s.store.CreateCommand(r.Context(), d.ID, api.ActionBackup, c.ID, map[string]string{"backup_id": b.ID}); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), d.ID, "info", "backup.queued", fmt.Sprintf("Backup of %s queued", c.Name))
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
	f, err := os.Open(s.backupPath(b.ID))
	if err != nil {
		writeError(w, http.StatusNotFound, "backup file is missing")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.tar.gz"`, b.ContainerName, b.CreatedAt.Format("20060102-150405")))
	http.ServeContent(w, r, "", b.CreatedAt, f)
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
	os.Remove(s.backupPath(b.ID))
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
	if b.DeviceID != deviceFrom(r).ID {
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
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, maxBackupBytes))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		s.store.FinishBackup(r.Context(), b.ID, 0, "upload failed: "+err.Error())
		writeError(w, http.StatusBadRequest, "upload failed")
		return
	}
	if err := os.Rename(tmp.Name(), s.backupPath(b.ID)); err != nil {
		s.internal(w, err)
		return
	}
	if err := s.store.FinishBackup(r.Context(), b.ID, n, ""); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), b.DeviceID, "info", "backup.complete", fmt.Sprintf("Backup of %s finished (%s)", b.ContainerName, humanSize(n)))
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

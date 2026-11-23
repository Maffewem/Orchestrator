package server

import (
	"io/fs"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

// backupStorage describes how much of the backup disk the backups themselves occupy.
type backupStorage struct {
	Dir         string `json:"dir"`
	BackupBytes int64  `json:"backup_bytes"`
	FreeBytes   int64  `json:"free_bytes"`
	TotalBytes  int64  `json:"total_bytes"`
	Level       string `json:"level"` // ok | warning | critical
}

// storageLevel judges whether backups hold most of the space that is left. Backups count as a
// problem only when they are a large share of the disk (a quarter or more) and also most of what
// is not free; a nearly full disk with few backups is not the backups' doing.
func storageLevel(backup, free, total int64) string {
	if total <= 0 || backup <= 0 || backup*4 < total {
		return "ok"
	}
	share := float64(backup) / float64(backup+free)
	switch {
	case share >= 0.9 || free < 1<<30:
		return "critical"
	case share >= 0.8:
		return "warning"
	}
	return "ok"
}

var (
	usageMu    sync.Mutex
	usageAt    time.Time
	usageCache backupStorage
)

// backupUsage walks the backup folder (everything in it counts: container backups, server
// snapshots, staging and partial files). The result is cached for a minute.
func (s *Server) backupUsage() backupStorage {
	usageMu.Lock()
	defer usageMu.Unlock()
	if usageCache.Dir == s.cfg.BackupDir && time.Since(usageAt) < time.Minute {
		return usageCache
	}
	u := backupStorage{Dir: s.cfg.BackupDir, Level: "ok"}
	filepath.WalkDir(s.cfg.BackupDir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				u.BackupBytes += info.Size()
			}
		}
		return nil
	})
	if total, free, err := diskSpace(s.cfg.BackupDir); err == nil {
		u.TotalBytes, u.FreeBytes = int64(total), int64(free)
		u.Level = storageLevel(u.BackupBytes, u.FreeBytes, u.TotalBytes)
	}
	usageCache, usageAt = u, time.Now()
	return u
}

func (s *Server) handleBackupStorage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backupUsage())
}

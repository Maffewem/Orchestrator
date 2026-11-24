package server

import (
	"context"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
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

	// Where the folder really is: the server container's mounts say which host folder or Docker
	// volume sits behind Dir, and which device the server runs on.
	ContainerBytes int64         `json:"container_bytes"` // worker backups
	ServerBytes    int64         `json:"server_bytes"`    // server database snapshots
	OtherBytes     int64         `json:"other_bytes"`     // partial uploads and anything else
	Location       storeLocation `json:"location"`
}

type storeLocation struct {
	Device    string `json:"device,omitempty"`    // device the server runs on, when known
	DeviceID  string `json:"device_id,omitempty"` //
	Container string `json:"container,omitempty"` // the server's container
	HostPath  string `json:"host_path,omitempty"` // folder on that device
	Volume    string `json:"volume,omitempty"`    // Docker volume name, when it is a named volume
	Note      string `json:"note,omitempty"`      // why a part is unknown, or a caution
}

// locate says where a folder inside the server container lives on its device.
func (s *Server) locate(ctx context.Context, dir string) storeLocation {
	var loc storeLocation
	if devs, err := s.store.ListDevices(ctx); err == nil {
		if h := serverHostDevice(devs); h != nil {
			loc.Device, loc.DeviceID = h.Name, h.ID
		}
	}
	self, why := s.selfInfo(ctx)
	if self == nil {
		loc.Note = "The server cannot inspect its own container (" + why + "), so the folder on the device is unknown. If it runs in Docker, check the volume mounted at " + dir + "."
		return loc
	}
	loc.Container = self.Name
	best := -1
	for _, m := range self.Mounts {
		d := strings.TrimSuffix(m.Destination, "/")
		if (dir == d || strings.HasPrefix(dir, d+"/")) && len(d) > best {
			best = len(d)
			loc.HostPath = filepath.Join(m.Source, strings.TrimPrefix(dir, d))
			loc.Volume = ""
			if m.Type == "volume" {
				loc.Volume = m.Name
			}
		}
	}
	if best < 0 {
		loc.Note = "This folder is not on a mounted volume, so it lives inside the server container and is lost when the container is replaced."
	}
	return loc
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
	serverDir := s.serverBackupDir()
	filepath.WalkDir(s.cfg.BackupDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n := info.Size()
				u.BackupBytes += n
				switch {
				case strings.HasPrefix(p, serverDir+string(filepath.Separator)):
					u.ServerBytes += n
				case strings.Contains(filepath.Base(p), ".tar.gz") && !strings.HasSuffix(p, ".part"):
					u.ContainerBytes += n
				default:
					u.OtherBytes += n
				}
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

// backupUsageWithLocation adds the (uncached) description of where the folder is.
func (s *Server) backupUsageWithLocation(ctx context.Context) backupStorage {
	u := s.backupUsage()
	u.Location = s.locate(ctx, u.Dir)
	return u
}

// where renders a location for a sentence: "/data/backups (on Claude: /srv/orch/backups)".
func (u backupStorage) where() string {
	out := u.Dir
	l := u.Location
	switch {
	case l.HostPath != "" && l.Device != "":
		out += " (on " + l.Device + ": " + l.HostPath + ")"
	case l.HostPath != "":
		out += " (on the server's host: " + l.HostPath + ")"
	case l.Device != "":
		out += " (on " + l.Device + ")"
	}
	return out
}

func (s *Server) handleBackupStorage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backupUsageWithLocation(r.Context()))
}

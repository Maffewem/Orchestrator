package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Destinations receive a verified copy of every server backup (see backup_mirror.go), so
// losing the server's disk does not lose the backups. Each destination keeps its own number of
// backups (or the schedule's), S3 objects included.

const settingBackupDests = "server_backup_destinations"

type BackupDest struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // s3 | folder | device | rclone
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`

	Endpoint  string `json:"endpoint,omitempty"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`

	Path     string `json:"path,omitempty"`      // folder on the server, or on the device
	DeviceID string `json:"device_id,omitempty"` // device kind

	RcloneConf string `json:"rclone_conf,omitempty"` // rclone kind: the remote's config section (holds tokens)
	Remote     string `json:"remote,omitempty"`      // rclone kind: remote name; defaults to the first section

	// Keep is how many backups this destination holds; 0 follows the schedule's count.
	Keep int `json:"keep,omitempty"`

	LastAt    *time.Time `json:"last_at,omitempty"`
	LastSize  int64      `json:"last_size,omitempty"` // size of the last copy sent
	LastError string     `json:"last_error,omitempty"`
}

// destView is a destination as the dashboard sees it: the secret is never sent back.
type destView struct {
	BackupDest
	SecretSet bool `json:"secret_set"`
	// Location says in words where the copies land: device and folder, bucket and prefix, or
	// remote and folder. UsedBytes is the space the copies take, known only for folders on the
	// server (other kinds report the size of the last copy sent).
	Location  string `json:"location"`
	UsedBytes *int64 `json:"used_bytes,omitempty"`
}

func (s *Server) destLocation(ctx context.Context, d BackupDest) (string, *int64) {
	switch d.Kind {
	case "s3":
		where := "bucket " + d.Bucket
		if p := strings.Trim(d.Prefix, "/"); p != "" {
			where += ", folder " + p
		}
		if d.Endpoint != "" {
			where += " at " + d.Endpoint
		}
		return where, nil
	case "folder":
		loc := s.locate(ctx, d.Path)
		out := d.Path + " inside the server container"
		switch {
		case loc.HostPath != "" && loc.Device != "":
			out = "folder " + d.Path + ", which is " + loc.HostPath + " on " + loc.Device
		case loc.HostPath != "":
			out = "folder " + d.Path + ", which is " + loc.HostPath + " on the server's host"
		case loc.Note != "":
			out = "folder " + d.Path + " (" + loc.Note + ")"
		}
		var n int64
		filepath.WalkDir(d.Path, func(_ string, e fs.DirEntry, err error) error {
			if err == nil && e.Type().IsRegular() {
				if info, err := e.Info(); err == nil {
					n += info.Size()
				}
			}
			return nil
		})
		return out, &n
	case "device":
		name := "an unknown device"
		if dev, err := s.store.GetDevice(ctx, d.DeviceID); err == nil {
			name = dev.Name
		}
		return "folder " + d.Path + " on " + name, nil
	case "rclone":
		remote := d.Remote
		if remote == "" {
			remote = "the remote"
		}
		return "cloud storage " + remote + ", folder " + d.Path, nil
	}
	return "", nil
}

func (s *Server) backupDests(ctx context.Context) []BackupDest {
	var out []BackupDest
	if v, err := s.store.GetSetting(ctx, settingBackupDests); err == nil {
		json.Unmarshal([]byte(v), &out)
	}
	return out
}

func (s *Server) saveBackupDests(ctx context.Context, d []BackupDest) error {
	b, _ := json.Marshal(d)
	return s.store.SetSetting(ctx, settingBackupDests, string(b))
}

func (s *Server) destViews(ctx context.Context) []destView {
	out := []destView{}
	for _, d := range s.backupDests(ctx) {
		v := destView{BackupDest: d, SecretSet: d.SecretKey != "" || d.RcloneConf != ""}
		v.SecretKey, v.RcloneConf = "", ""
		v.Location, v.UsedBytes = s.destLocation(ctx, d)
		out = append(out, v)
	}
	return out
}

func (d BackupDest) validate() error {
	switch d.Kind {
	case "s3":
		if d.Endpoint == "" || d.Bucket == "" || d.AccessKey == "" || d.SecretKey == "" {
			return errors.New("S3 needs an endpoint, bucket, access key and secret key")
		}
		if _, err := d.s3().objectURL("x"); err != nil {
			return err
		}
	case "folder":
		if !filepath.IsAbs(d.Path) {
			return errors.New("the folder must be an absolute path on the server")
		}
	case "device":
		if d.DeviceID == "" || !path.IsAbs(d.Path) {
			return errors.New("pick a device and an absolute folder path on it")
		}
	case "rclone":
		if err := validateRcloneConf(d.RcloneConf); err != nil {
			return err
		}
		if _, err := exec.LookPath(rcloneBinary()); err != nil {
			return errors.New("rclone is not installed on the server. The official image includes it; otherwise install rclone or set ORCH_RCLONE")
		}
	default:
		return errors.New("unknown destination type")
	}
	return nil
}

func (d BackupDest) s3() s3Target {
	region := d.Region
	if region == "" {
		region = "auto"
	}
	return s3Target{Endpoint: d.Endpoint, Region: region, Bucket: d.Bucket, AccessKey: d.AccessKey, SecretKey: d.SecretKey}
}

func (d BackupDest) key(name string) string {
	return strings.Trim(d.Prefix, "/") + "/" + name
}

// handleSetBackupDests replaces the destination list. A blank secret keeps the stored one.
func (s *Server) handleSetBackupDests(w http.ResponseWriter, r *http.Request) {
	var req []BackupDest
	if !decode(w, r, &req) {
		return
	}
	old := map[string]BackupDest{}
	for _, d := range s.backupDests(r.Context()) {
		old[d.ID] = d
	}
	for i := range req {
		d := &req[i]
		if d.ID == "" {
			d.ID = store.NewToken()[:10]
		}
		if o, ok := old[d.ID]; ok {
			if d.SecretKey == "" {
				d.SecretKey = o.SecretKey
			}
			if d.RcloneConf == "" {
				d.RcloneConf = o.RcloneConf
			}
			d.LastAt, d.LastError, d.LastSize = o.LastAt, o.LastError, o.LastSize
		}
		if d.Name == "" {
			d.Name = d.Kind
		}
		if d.Enabled {
			if err := d.validate(); err != nil {
				writeError(w, http.StatusBadRequest, d.Name+": "+err.Error())
				return
			}
		}
	}
	if err := s.saveBackupDests(r.Context(), req); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.serverBackupsView(r.Context()))
}

// handleTestBackupDest checks that a destination can be written to: POST .../destinations/{id}/test.
func (s *Server) handleTestBackupDest(w http.ResponseWriter, r *http.Request) {
	for _, d := range s.backupDests(r.Context()) {
		if d.ID != r.PathValue("id") {
			continue
		}
		if err := d.validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.testDest(r.Context(), d); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	writeError(w, http.StatusNotFound, "no such destination")
}

func (s *Server) testDest(ctx context.Context, d BackupDest) error {
	probe := "orchestrator-test-" + time.Now().UTC().Format("20060102-150405") + ".txt"
	tmp, err := os.CreateTemp("", "orch-probe-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	tmp.WriteString("Orchestrator destination test\n")
	tmp.Close()
	switch d.Kind {
	case "s3":
		if err := d.s3().PutFile(ctx, d.key(probe), tmp.Name()); err != nil {
			return err
		}
		return d.s3().Delete(ctx, d.key(probe))
	case "folder":
		if err := copyFile(tmp.Name(), filepath.Join(d.Path, probe)); err != nil {
			return err
		}
		return os.Remove(filepath.Join(d.Path, probe))
	case "device":
		return s.deviceReady(ctx, d.DeviceID)
	case "rclone":
		return s.rcloneTest(ctx, d, probe, tmp.Name())
	}
	return errors.New("unknown destination type")
}

func (s *Server) deviceReady(ctx context.Context, id string) error {
	dev, err := s.store.GetDevice(ctx, id)
	if err != nil {
		return errors.New("that device no longer exists")
	}
	if s.view(dev).Status != "online" {
		return errors.New("the device is offline")
	}
	if !hasCap(dev, api.CapFileStore) {
		return errors.New("this device's agent is too old to receive files. Update it from the Updates page")
	}
	return nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// sendToDest copies one backup to a destination. It returns the checksum the destination
// computed itself when it can (devices), or "" when the caller must read the copy back.
func (s *Server) sendToDest(ctx context.Context, d BackupDest, name, src string, keep int) (string, error) {
	switch d.Kind {
	case "s3":
		return "", d.s3().PutFile(ctx, d.key(name), src)
	case "folder":
		if err := copyFile(src, filepath.Join(d.Path, name)); err != nil {
			return "", err
		}
		pruneFolder(d.Path, keep)
		return "", nil
	case "rclone":
		return "", s.rcloneSend(ctx, d, name, src, keep)
	case "device":
		if err := s.deviceReady(ctx, d.DeviceID); err != nil {
			return "", err
		}
		return s.deviceCommand(ctx, d.DeviceID, api.ActionFileStore, map[string]string{"name": name, "dir": d.Path, "keep": fmt.Sprint(keep)})
	}
	return "", errors.New("unknown destination type")
}

func pruneFolder(dir string, keep int) {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, serverBackupPrefix) && (strings.HasSuffix(n, ".db") || strings.HasSuffix(n, ".db.enc")) {
			names = append(names, n)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // names carry a UTC timestamp
	for i := keep; i < len(names); i++ {
		os.Remove(filepath.Join(dir, names[i]))
	}
}

// handleAgentServerBackup lets an agent fetch a server backup it was told to store.
func (s *Server) handleAgentServerBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != filepath.Base(name) || !strings.HasPrefix(name, serverBackupPrefix) {
		writeError(w, http.StatusBadRequest, "invalid backup name")
		return
	}
	// Only a device that is a configured destination may fetch backups.
	d := deviceFrom(r)
	ok := false
	for _, x := range s.backupDests(r.Context()) {
		if x.Kind == "device" && x.Enabled && x.DeviceID == d.ID {
			ok = true
		}
	}
	if !ok {
		writeError(w, http.StatusForbidden, "this device is not a backup destination")
		return
	}
	http.ServeFile(w, r, filepath.Join(s.serverBackupDir(), name))
}

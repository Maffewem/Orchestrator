package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Destinations receive a copy of every server backup, so losing the server's disk does not
// lose the backups. Retention only applies to the local copies and to folders; use a bucket
// lifecycle rule to expire objects in S3.

const settingBackupDests = "server_backup_destinations"

type BackupDest struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // s3 | folder | device
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

	LastAt    *time.Time `json:"last_at,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

// destView is a destination as the dashboard sees it: the secret is never sent back.
type destView struct {
	BackupDest
	SecretSet bool `json:"secret_set"`
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
		v := destView{BackupDest: d, SecretSet: d.SecretKey != ""}
		v.SecretKey = ""
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
			d.LastAt, d.LastError = o.LastAt, o.LastError
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

// replicateServerBackup copies a fresh backup to every enabled destination and records each
// outcome. It runs in the background so a slow bucket never delays the backup itself.
func (s *Server) replicateServerBackup(name string, keep int) {
	ctx := context.Background()
	dests := s.backupDests(ctx)
	if len(dests) == 0 {
		return
	}
	src := filepath.Join(s.serverBackupDir(), name)
	results := map[string]error{}
	for _, d := range dests {
		if !d.Enabled {
			continue
		}
		results[d.ID] = s.sendToDest(ctx, d, name, src, keep)
	}
	// Re-read so edits made while uploading are kept; only the status fields change.
	cur := s.backupDests(ctx)
	now := time.Now().UTC()
	for i := range cur {
		if err, ok := results[cur[i].ID]; ok {
			cur[i].LastAt = &now
			cur[i].LastError = ""
			if err != nil {
				cur[i].LastError = err.Error()
				s.event(ctx, "", "error", "serverbackup.copy_failed", fmt.Sprintf("Copy of %s to %s failed: %v", name, cur[i].Name, err))
			}
		}
	}
	s.saveBackupDests(ctx, cur)
}

func (s *Server) sendToDest(ctx context.Context, d BackupDest, name, src string, keep int) error {
	switch d.Kind {
	case "s3":
		return d.s3().PutFile(ctx, d.key(name), src)
	case "folder":
		if err := copyFile(src, filepath.Join(d.Path, name)); err != nil {
			return err
		}
		pruneFolder(d.Path, keep)
		return nil
	case "device":
		if err := s.deviceReady(ctx, d.DeviceID); err != nil {
			return err
		}
		_, err := s.store.CreateCommand(ctx, d.DeviceID, api.ActionFileStore, "", map[string]string{"name": name, "dir": d.Path, "keep": fmt.Sprint(keep)})
		return err
	}
	return errors.New("unknown destination type")
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

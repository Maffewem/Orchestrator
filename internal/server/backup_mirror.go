package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Mirrored server backups. A backup is copied to every selected destination; each copy is
// checked against the SHA-256 of the local file, retried when it fails, and recorded per
// destination so the dashboard can show which mirrors are healthy, re-sync a missing one and
// restore from any of them.

const settingBackupCopies = "server_backup_copies"

// mirrorBackoff is the wait before each retry of a failed copy.
var mirrorBackoff = []time.Duration{5 * time.Second, 30 * time.Second}

// Copy is the state of one backup on one destination.
type Copy struct {
	Status     string     `json:"status"` // ok | failed | pending
	Verified   bool       `json:"verified"`
	SHA256     string     `json:"sha256,omitempty"`
	Size       int64      `json:"size,omitempty"`
	At         *time.Time `json:"at,omitempty"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	Note       string     `json:"note,omitempty"`
}

type copyRecord struct {
	SHA256 string          `json:"sha256"`
	Copies map[string]Copy `json:"copies"`
}

var copiesMu sync.Mutex

func (s *Server) loadCopies(ctx context.Context) map[string]copyRecord {
	out := map[string]copyRecord{}
	if v, err := s.store.GetSetting(ctx, settingBackupCopies); err == nil {
		json.Unmarshal([]byte(v), &out)
	}
	return out
}

// mutateCopies applies fn to the stored records under a lock.
func (s *Server) mutateCopies(ctx context.Context, fn func(map[string]copyRecord)) {
	copiesMu.Lock()
	defer copiesMu.Unlock()
	m := s.loadCopies(ctx)
	fn(m)
	// Forget records that no longer point at anything.
	for name, rec := range m {
		keep := false
		for _, c := range rec.Copies {
			if c.Status != "failed" {
				keep = true
			}
		}
		if _, err := os.Stat(filepath.Join(s.serverBackupDir(), name)); err == nil {
			keep = true
		}
		if !keep {
			delete(m, name)
		}
	}
	b, _ := json.Marshal(m)
	s.store.SetSetting(ctx, settingBackupCopies, string(b))
}

func (s *Server) setCopy(ctx context.Context, name, sha, destID string, c Copy) {
	s.mutateCopies(ctx, func(m map[string]copyRecord) {
		rec := m[name]
		if rec.Copies == nil {
			rec.Copies = map[string]Copy{}
		}
		if sha != "" {
			rec.SHA256 = sha
		}
		rec.Copies[destID] = c
		m[name] = rec
	})
}

func fileSHA(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// selectedDests returns the destinations a schedule copies to: the enabled ones it lists, or
// every enabled one when it lists none.
func selectedDests(all []BackupDest, chosen []string) []BackupDest {
	var out []BackupDest
	for _, d := range all {
		if !d.Enabled {
			continue
		}
		if len(chosen) > 0 {
			found := false
			for _, id := range chosen {
				found = found || id == d.ID
			}
			if !found {
				continue
			}
		}
		out = append(out, d)
	}
	return out
}

func (d BackupDest) keepOr(policyKeep int) int {
	if d.Keep > 0 {
		return d.Keep
	}
	return policyKeep
}

// remoteHash reads a copy back from its destination and returns its checksum and size.
func (s *Server) remoteHash(ctx context.Context, d BackupDest, name string) (string, int64, error) {
	switch d.Kind {
	case "folder":
		sum, n, err := fileSHA(filepath.Join(d.Path, name))
		if errors.Is(err, os.ErrNotExist) {
			return "", 0, errors.New("the copy is missing from the folder")
		}
		return sum, n, err
	case "device":
		dev, err := s.store.GetDevice(ctx, d.DeviceID)
		if err != nil {
			return "", 0, errors.New("that device no longer exists")
		}
		if !hasCap(dev, api.CapFileFetch) {
			return "", 0, errUnverifiable
		}
		if err := s.deviceReady(ctx, d.DeviceID); err != nil {
			return "", 0, err
		}
		out, err := s.deviceCommand(ctx, d.DeviceID, api.ActionFileFetch, map[string]string{"name": name, "dir": d.Path, "mode": "hash"})
		if err != nil {
			return "", 0, err
		}
		return strings.TrimSpace(out), 0, nil
	}
	tmp, err := os.CreateTemp("", "orch-verify-*")
	if err != nil {
		return "", 0, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := s.downloadCopy(ctx, d, name, tmp.Name()); err != nil {
		return "", 0, err
	}
	return fileSHA(tmp.Name())
}

var errUnverifiable = errors.New("this device's agent is too old to report a checksum")

// deviceCommand queues a command on a device and waits for its result.
func (s *Server) deviceCommand(ctx context.Context, deviceID, action string, args map[string]string) (string, error) {
	cmd, err := s.store.CreateCommand(ctx, deviceID, action, "", args)
	if err != nil {
		return "", err
	}
	done, err := s.waitCommand(ctx, cmd.ID, 35*time.Minute)
	if err != nil {
		return "", err
	}
	if done.Status != api.CommandSucceeded {
		msg := done.Error
		if msg == "" {
			msg = "the device reported a failure"
		}
		return "", errors.New(msg)
	}
	return done.Output, nil
}

// downloadCopy fetches a copy from its destination into the local file dst.
func (s *Server) downloadCopy(ctx context.Context, d BackupDest, name, dst string) error {
	switch d.Kind {
	case "s3":
		return d.s3().GetFile(ctx, d.key(name), dst)
	case "folder":
		return copyFile(filepath.Join(d.Path, name), dst)
	case "rclone":
		_, nc, err := d.rcloneRun(ctx, "copyto", d.rcloneTarget(name), dst)
		s.storeRcloneConf(ctx, d.ID, nc)
		return err
	case "device":
		dev, err := s.store.GetDevice(ctx, d.DeviceID)
		if err != nil || !hasCap(dev, api.CapFileFetch) {
			return errors.New("this device's agent is too old to send the copy back. Update it from the Updates page")
		}
		stage := filepath.Join(s.serverBackupDir(), ".fetch", name)
		os.Remove(stage)
		if _, err := s.deviceCommand(ctx, d.DeviceID, api.ActionFileFetch, map[string]string{"name": name, "dir": d.Path, "mode": "send"}); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		return os.Rename(stage, dst)
	}
	return errors.New("unknown destination type")
}

// mirrorOne copies a backup to one destination, retrying, then checks the result against sha.
func (s *Server) mirrorOne(ctx context.Context, d BackupDest, name, src, sha string, size int64, keep int) Copy {
	var lastErr error
	for attempt := 0; attempt <= len(mirrorBackoff); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Copy{Status: "failed", Error: ctx.Err().Error()}
			case <-time.After(mirrorBackoff[attempt-1]):
			}
		}
		now := time.Now().UTC()
		c := Copy{Status: "ok", SHA256: sha, Size: size, At: &now}
		out, err := s.sendToDest(ctx, d, name, src, d.keepOr(keep))
		if err != nil {
			lastErr = err
			continue
		}
		got := strings.TrimSpace(out)
		if got == "" { // kinds that do not report a checksum are read back
			var rerr error
			got, _, rerr = s.remoteHash(ctx, d, name)
			if errors.Is(rerr, errUnverifiable) {
				c.Note = "Copied, but not verified: " + rerr.Error() + "."
				return c
			}
			if rerr != nil {
				lastErr = fmt.Errorf("copied, but reading it back failed: %w", rerr)
				continue
			}
		}
		if got != sha {
			lastErr = fmt.Errorf("checksum mismatch: the copy is %.12s, expected %.12s", got, sha)
			continue
		}
		v := time.Now().UTC()
		c.Verified, c.VerifiedAt = true, &v
		return c
	}
	return Copy{Status: "failed", SHA256: sha, Error: lastErr.Error()}
}

// mirror copies a backup to its destinations in parallel and records each outcome. only limits
// it to those destination ids; force copies even where a verified copy exists.
func (s *Server) mirror(ctx context.Context, name string, only map[string]bool, force bool) (ok, total int, err error) {
	src := filepath.Join(s.serverBackupDir(), name)
	pol := s.serverBackupPolicy(ctx)
	var dests []BackupDest
	if only != nil {
		for _, d := range selectedDests(s.backupDests(ctx), nil) {
			if only[d.ID] {
				dests = append(dests, d)
			}
		}
	} else {
		dests = selectedDests(s.backupDests(ctx), pol.Destinations)
	}
	if len(dests) == 0 {
		return 0, 0, nil
	}
	sha, size, err := fileSHA(src)
	if err != nil {
		return 0, 0, fmt.Errorf("the local backup is not readable: %w", err)
	}
	rec := s.loadCopies(ctx)[name]
	var todo []BackupDest
	for _, d := range dests {
		if c, have := rec.Copies[d.ID]; !force && have && c.Status == "ok" && c.SHA256 == sha && (c.Verified || c.Note != "") {
			ok++
			total++
			continue
		}
		todo = append(todo, d)
		s.setCopy(ctx, name, sha, d.ID, Copy{Status: "pending", SHA256: sha})
	}
	results := make([]Copy, len(todo))
	var wg sync.WaitGroup
	for i, d := range todo {
		wg.Add(1)
		go func(i int, d BackupDest) {
			defer wg.Done()
			results[i] = s.mirrorOne(ctx, d, name, src, sha, size, pol.Keep)
			s.setCopy(ctx, name, sha, d.ID, results[i])
			if results[i].Status == "ok" {
				s.pruneDestCopies(ctx, d, d.keepOr(pol.Keep))
			}
		}(i, d)
	}
	wg.Wait()

	status := map[string]Copy{}
	failed := []string{}
	for i, d := range todo {
		status[d.ID] = results[i]
		if results[i].Status == "ok" {
			ok++
		} else {
			failed = append(failed, d.Name)
			s.event(ctx, "", "error", "serverbackup.copy_failed", fmt.Sprintf("Copy of %s to %s failed: %s", name, d.Name, results[i].Error))
		}
	}
	total += len(todo)
	// Re-read so edits made while copying are kept; only the status fields change.
	cur := s.backupDests(ctx)
	for i := range cur {
		if c, have := status[cur[i].ID]; have {
			now := time.Now().UTC()
			cur[i].LastAt, cur[i].LastError = &now, c.Error
			if c.Status == "ok" {
				cur[i].LastSize = size
			}
		}
	}
	s.saveBackupDests(ctx, cur)
	switch {
	case len(failed) > 0 && ok > 0:
		s.event(ctx, "", "error", "serverbackup.partial", fmt.Sprintf("Backup %s is only on %d of %d destinations. Missing from: %s. Use Re-sync on the Server backups panel.", name, ok, total, strings.Join(failed, ", ")))
	case len(failed) > 0:
		s.event(ctx, "", "error", "serverbackup.unmirrored", fmt.Sprintf("Backup %s could not be copied to any destination (%s). Only the local copy exists.", name, strings.Join(failed, ", ")))
	case len(todo) > 0:
		s.event(ctx, "", "info", "serverbackup.mirrored", fmt.Sprintf("Backup %s copied and verified on %d destinations", name, total))
	}
	return ok, total, nil
}

// replicateServerBackup mirrors a fresh backup in the background so a slow bucket never delays
// the backup itself.
func (s *Server) replicateServerBackup(name string, keep int) {
	ctx := context.Background()
	if _, _, err := s.mirror(ctx, name, nil, false); err != nil {
		s.event(ctx, "", "error", "serverbackup.copy_failed", fmt.Sprintf("Mirroring %s failed: %v", name, err))
	}
}

// pruneDestCopies drops the records, and for S3 the objects, of copies beyond the newest keep.
func (s *Server) pruneDestCopies(ctx context.Context, d BackupDest, keep int) {
	rec := s.loadCopies(ctx)
	var names []string
	for n, r := range rec {
		if c, ok := r.Copies[d.ID]; ok && c.Status == "ok" {
			names = append(names, n)
		}
	}
	if len(names) <= keep {
		return
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names))) // names carry a UTC timestamp
	for _, n := range names[keep:] {
		if d.Kind == "s3" {
			if err := d.s3().Delete(ctx, d.key(n)); err != nil {
				continue
			}
		}
		s.mutateCopies(ctx, func(m map[string]copyRecord) { delete(m[n].Copies, d.ID) })
	}
}

// ensureLocal makes sure the backup file exists on this server, fetching it from a copy if not.
func (s *Server) ensureLocal(ctx context.Context, name string) error {
	dst := filepath.Join(s.serverBackupDir(), name)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	rec := s.loadCopies(ctx)[name]
	for _, d := range s.backupDests(ctx) {
		if c, ok := rec.Copies[d.ID]; ok && c.Status == "ok" {
			if err := s.fetchVerified(ctx, d, name, rec.SHA256, dst); err == nil {
				return nil
			}
		}
	}
	return errors.New("no copy of this backup could be read from any destination")
}

// fetchVerified downloads a copy to dst and checks it against sha.
func (s *Server) fetchVerified(ctx context.Context, d BackupDest, name, sha, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	part := dst + ".part"
	defer os.Remove(part)
	if err := s.downloadCopy(ctx, d, name, part); err != nil {
		return err
	}
	got, _, err := fileSHA(part)
	if err != nil {
		return err
	}
	if sha != "" && got != sha {
		return fmt.Errorf("the copy on %s does not match its checksum, so it was not used", d.Name)
	}
	return os.Rename(part, dst)
}

// verifyCopies re-reads every recorded copy of a backup and updates its verified state.
func (s *Server) verifyCopies(ctx context.Context, name string) {
	rec := s.loadCopies(ctx)[name]
	sha := rec.SHA256
	if l, _, err := fileSHA(filepath.Join(s.serverBackupDir(), name)); err == nil {
		sha = l
	}
	var wg sync.WaitGroup
	for _, d := range s.backupDests(ctx) {
		c, ok := rec.Copies[d.ID]
		if !ok || c.Status == "pending" {
			continue
		}
		wg.Add(1)
		go func(d BackupDest, c Copy) {
			defer wg.Done()
			got, size, err := s.remoteHash(ctx, d, name)
			now := time.Now().UTC()
			switch {
			case errors.Is(err, errUnverifiable):
				c.Note = "Not verified: " + err.Error() + "."
			case err != nil:
				c.Status, c.Verified, c.Error, c.Note = "failed", false, err.Error(), ""
			case got != sha:
				c.Status, c.Verified, c.Error, c.Note = "failed", false, fmt.Sprintf("checksum mismatch: the copy is %.12s, expected %.12s", got, sha), ""
			default:
				c.Status, c.Verified, c.VerifiedAt, c.Error, c.Note = "ok", true, &now, "", ""
				if size > 0 {
					c.Size = size
				}
			}
			s.setCopy(ctx, name, "", d.ID, c)
		}(d, c)
	}
	wg.Wait()
}

type copyView struct {
	DestID     string     `json:"dest_id"`
	Dest       string     `json:"dest"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"` // ok | failed | pending | missing
	Verified   bool       `json:"verified"`
	Size       int64      `json:"size,omitempty"`
	At         *time.Time `json:"at,omitempty"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	Note       string     `json:"note,omitempty"`
}

// backupsWithCopies lists the local backups plus any that exist only on a destination, each
// with the state of its copies on the destinations that should hold it.
func (s *Server) backupsWithCopies(ctx context.Context) []serverBackup {
	local := s.listServerBackups()
	recs := s.loadCopies(ctx)
	dests := s.backupDests(ctx)
	wanted := map[string]bool{}
	for _, d := range selectedDests(dests, s.serverBackupPolicy(ctx).Destinations) {
		wanted[d.ID] = true
	}
	seen := map[string]bool{}
	for i := range local {
		local[i].Local = true
		seen[local[i].Name] = true
	}
	for name, rec := range recs {
		if seen[name] {
			continue
		}
		b := serverBackup{Name: name, Encrypted: strings.HasSuffix(name, ".enc")}
		for _, c := range rec.Copies {
			if c.Status == "ok" && c.At != nil {
				b.Size, b.CreatedAt = c.Size, *c.At
			}
		}
		if t, err := time.Parse("20060102-150405", strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(name, serverBackupPrefix), ".enc"), ".db")); err == nil {
			b.CreatedAt = t.UTC()
		}
		local = append(local, b)
	}
	for i := range local {
		b := &local[i]
		rec := recs[b.Name]
		b.SHA256 = rec.SHA256
		for _, d := range dests {
			c, have := rec.Copies[d.ID]
			if !have && !wanted[d.ID] {
				continue
			}
			v := copyView{DestID: d.ID, Dest: d.Name, Kind: d.Kind, Status: "missing"}
			if have {
				v = copyView{DestID: d.ID, Dest: d.Name, Kind: d.Kind, Status: c.Status, Verified: c.Verified, Size: c.Size, At: c.At, VerifiedAt: c.VerifiedAt, Error: c.Error, Note: c.Note}
			}
			if wanted[d.ID] {
				b.Wanted++
				if v.Status == "ok" {
					b.Mirrored++
				}
			}
			b.Copies = append(b.Copies, v)
		}
	}
	sort.Slice(local, func(i, j int) bool { return local[i].CreatedAt.After(local[j].CreatedAt) })
	return local
}

type syncRequest struct {
	DestID string `json:"dest_id"`
}

// handleSyncServerBackup re-copies a backup to the destinations that lack a good copy, or to
// one chosen destination: POST /api/server-backups/{name}/sync.
func (s *Server) handleSyncServerBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != filepath.Base(name) || !strings.HasPrefix(name, serverBackupPrefix) {
		writeError(w, http.StatusBadRequest, "invalid backup name")
		return
	}
	var req syncRequest
	if r.ContentLength != 0 && !decode(w, r, &req) {
		return
	}
	var only map[string]bool
	if req.DestID != "" {
		only = map[string]bool{req.DestID: true}
	}
	ctx := context.Background()
	if err := s.ensureLocal(ctx, name); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	go func() {
		if _, _, err := s.mirror(ctx, name, only, req.DestID != ""); err != nil {
			s.event(ctx, "", "error", "serverbackup.copy_failed", fmt.Sprintf("Re-sync of %s failed: %v", name, err))
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "syncing"})
}

// handleVerifyServerBackup re-checks every copy of a backup: POST /api/server-backups/{name}/verify.
func (s *Server) handleVerifyServerBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != filepath.Base(name) || !strings.HasPrefix(name, serverBackupPrefix) {
		writeError(w, http.StatusBadRequest, "invalid backup name")
		return
	}
	go s.verifyCopies(context.Background(), name)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "verifying"})
}

// handleAgentServerBackupUpload receives a copy an agent was told to send back.
func (s *Server) handleAgentServerBackupUpload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != filepath.Base(name) || !strings.HasPrefix(name, serverBackupPrefix) {
		writeError(w, http.StatusBadRequest, "invalid backup name")
		return
	}
	d := deviceFrom(r)
	allowed := false
	for _, x := range s.backupDests(r.Context()) {
		allowed = allowed || (x.Kind == "device" && x.Enabled && x.DeviceID == d.ID)
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "this device is not a backup destination")
		return
	}
	dir := filepath.Join(s.serverBackupDir(), ".fetch")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.internal(w, err)
		return
	}
	part := filepath.Join(dir, name+".part")
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		s.internal(w, err)
		return
	}
	_, err = io.Copy(f, http.MaxBytesReader(w, r.Body, 4<<30))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.Rename(part, filepath.Join(dir, name)); err != nil {
		s.internal(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

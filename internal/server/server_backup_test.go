package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

func TestServerBackupAndRestore(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "orch.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	restarted := make(chan struct{}, 1)
	srv, _, err := New(context.Background(), st, Config{AdminToken: adminToken, HeartbeatInterval: time.Second, BackupDir: dir, DBPath: dbPath, Restart: func() { restarted <- struct{}{} }})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, srv: srv, st: st}
	h.ts = newTestServer(t, srv)

	var b serverBackup
	if code := h.do("POST", "/api/server-backups", adminToken, nil, &b); code != http.StatusCreated || b.Encrypted {
		t.Fatalf("create: %d %+v", code, b)
	}
	// With a password the next backup is protected.
	if code := h.do("PUT", "/api/settings/backup-password", adminToken, map[string]string{"password": "s3cret-pass"}, nil); code >= 300 {
		t.Fatalf("set password: %d", code)
	}
	time.Sleep(1100 * time.Millisecond) // names carry a one-second timestamp
	var enc serverBackup
	h.do("POST", "/api/server-backups", adminToken, nil, &enc)
	if !enc.Encrypted {
		t.Fatalf("expected an encrypted backup: %+v", enc)
	}
	raw, _ := os.ReadFile(filepath.Join(srv.serverBackupDir(), enc.Name))
	if bytes.Contains(raw, []byte("SQLite format 3")) {
		t.Fatal("encrypted backup contains plaintext database")
	}
	var view serverBackupsView
	h.do("GET", "/api/server-backups", adminToken, nil, &view)
	if len(view.Backups) != 2 {
		t.Fatalf("listed %d backups", len(view.Backups))
	}
	if code := h.do("GET", "/api/server-backups/"+b.Name+"/download", "nope", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous download: %d", code)
	}
	if code := h.do("POST", "/api/server-backups/..%2Fx/restore", adminToken, nil, nil); code == http.StatusAccepted {
		t.Fatal("path traversal accepted")
	}

	// Retention keeps the newest.
	srv.pruneServerBackups(1)
	if n := len(srv.listServerBackups()); n != 1 {
		t.Fatalf("after prune: %d", n)
	}

	// Restore the encrypted backup: it is staged and the server asks to restart.
	if code := h.do("POST", "/api/server-backups/"+enc.Name+"/restore", adminToken, nil, nil); code != http.StatusAccepted {
		t.Fatalf("restore: %d", code)
	}
	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("no restart requested")
	}
	if _, err := os.Stat(store.PendingRestorePath(dbPath)); err != nil {
		t.Fatalf("restore not staged: %v", err)
	}
	st.Close()
	if ok, err := store.ApplyPendingRestore(dbPath); !ok || err != nil {
		t.Fatalf("apply: %v %v", ok, err)
	}
	if err := store.CheckSnapshot(dbPath); err != nil {
		t.Fatalf("restored db: %v", err)
	}

	// A file that isn't a database is refused.
	st2, _ := store.Open(filepath.Join(dir, "x.db"))
	defer st2.Close()
	srv.cfg.DBPath = filepath.Join(dir, "x.db")
	req, _ := http.NewRequest("POST", h.ts.URL+"/api/server-backups/restore", bytes.NewReader([]byte("not a database at all")))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("garbage restore: %d", resp.StatusCode)
	}
}

func TestServerBackupDestinations(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(":memory:")
	defer st.Close()
	srv, _, _ := New(context.Background(), st, Config{AdminToken: adminToken, HeartbeatInterval: time.Second, BackupDir: dir})
	h := &harness{t: t, srv: srv, st: st}
	h.ts = newTestServer(t, srv)

	var got struct{ path, auth string }
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			got.path, got.auth = r.URL.Path, r.Header.Get("Authorization")
			io.Copy(io.Discard, r.Body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s3.Close()
	folder := filepath.Join(dir, "nas")

	dests := []BackupDest{
		{Kind: "s3", Name: "bucket", Enabled: true, Endpoint: s3.URL, Bucket: "bk", Prefix: "orch", AccessKey: "AK", SecretKey: "SK"},
		{Kind: "folder", Name: "nas", Enabled: true, Path: folder},
	}
	var view serverBackupsView
	if code := h.do("PUT", "/api/server-backups/destinations", adminToken, dests, &view); code != http.StatusOK {
		t.Fatalf("save: %d", code)
	}
	if len(view.Destinations) != 2 || view.Destinations[0].SecretKey != "" || !view.Destinations[0].SecretSet {
		t.Fatalf("secret leaked or lost: %+v", view.Destinations)
	}
	if code := h.do("POST", "/api/server-backups/destinations/"+view.Destinations[0].ID+"/test", adminToken, nil, nil); code != http.StatusOK {
		t.Fatalf("test s3: %d", code)
	}
	if code := h.do("PUT", "/api/server-backups/destinations", adminToken, []BackupDest{{Kind: "folder", Enabled: true, Path: "relative"}}, nil); code != http.StatusBadRequest {
		t.Fatalf("relative folder accepted: %d", code)
	}
	// Saving again without the secret keeps it.
	view.Destinations[0].Prefix = "orch2"
	var again []BackupDest
	for _, d := range view.Destinations {
		again = append(again, d.BackupDest)
	}
	h.do("PUT", "/api/server-backups/destinations", adminToken, again, nil)
	if k := srv.backupDests(context.Background())[0].SecretKey; k != "SK" {
		t.Fatalf("secret not kept: %q", k)
	}

	var b serverBackup
	h.do("POST", "/api/server-backups", adminToken, nil, &b)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(folder, b.Name)); err == nil && strings.HasSuffix(got.path, b.Name) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("copy missing: path=%q", got.path)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.HasPrefix(got.path, "/bk/orch2/") || !strings.Contains(got.auth, "Credential=AK/") {
		t.Fatalf("s3 request: %+v", got)
	}
}

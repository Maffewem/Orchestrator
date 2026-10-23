package server

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
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

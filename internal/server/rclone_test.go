package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

func newBackupTestServer(t *testing.T) *Server {
	t.Helper()
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	srv, _, err := New(context.Background(), st, Config{AdminToken: adminToken, HeartbeatInterval: time.Second, BackupDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func TestValidateRcloneConf(t *testing.T) {
	good := "[gdrive]\ntype = drive\ntoken = {\"access_token\":\"x\"}\n"
	if err := validateRcloneConf(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "type = drive", "[x]\ntype = sftp\nssh = sh -c evil\n", "[x]\npass_command = cat /etc/shadow\n"} {
		if validateRcloneConf(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if (BackupDest{RcloneConf: good}).rcloneRemote() != "gdrive" {
		t.Fatal("remote name not derived")
	}
}

func TestOldBackupNames(t *testing.T) {
	listing := "orchestrator-server-20260101-000000.db\nnotes.txt\norchestrator-server-20260103-000000.db.enc\norchestrator-server-20260102-000000.db\n"
	got := oldBackupNames(strings.ReplaceAll(listing, "orchestrator-server-", serverBackupPrefix), 2)
	if len(got) != 1 || !strings.Contains(got[0], "20260101") {
		t.Fatalf("%v", got)
	}
}

// A fake rclone records its arguments and rewrites the config like a token refresh would.
func TestRcloneSend(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\nif [ \"$3\" = lsf ]; then :; fi\ncase \"$*\" in *lsf*) printf '" + serverBackupPrefix + "20260101-000000.db\\n" + serverBackupPrefix + "20260102-000000.db\\n" + serverBackupPrefix + "20260103-000000.db\\n';; esac\necho '# refreshed' >> \"$2\"\n"
	bin := filepath.Join(dir, "rclone")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCH_RCLONE", bin)

	srv := newBackupTestServer(t)
	d := BackupDest{ID: "d1", Kind: "rclone", Name: "drive", Enabled: true, Path: "Backups/orch", RcloneConf: "[gdrive]\ntype = drive\n"}
	if err := srv.saveBackupDests(context.Background(), []BackupDest{d}); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, serverBackupPrefix+"20260103-000000.db")
	os.WriteFile(src, []byte("x"), 0o600)
	if _, err := srv.sendToDest(context.Background(), d, filepath.Base(src), src, 2); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	calls := string(b)
	if !strings.Contains(calls, "copyto "+src+" gdrive:Backups/orch/"+filepath.Base(src)) {
		t.Fatalf("no upload: %s", calls)
	}
	if !strings.Contains(calls, "deletefile gdrive:Backups/orch/"+serverBackupPrefix+"20260101-000000.db") || strings.Contains(calls, "deletefile gdrive:Backups/orch/"+serverBackupPrefix+"20260102") {
		t.Fatalf("wrong pruning: %s", calls)
	}
	if got := srv.backupDests(context.Background())[0].RcloneConf; !strings.Contains(got, "# refreshed") {
		t.Fatalf("refreshed config not stored: %q", got)
	}
}

func TestPrepareBackupDirMarker(t *testing.T) {
	srv := newBackupTestServer(t)
	if err := srv.prepareBackupDir(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(srv.cfg.BackupDir, api.NoBackupMarker)); err != nil {
		t.Fatal(err)
	}
	if total, free, err := diskSpace(srv.cfg.BackupDir); err != nil || free == 0 || total < free {
		t.Fatalf("disk space: %d %d %v", total, free, err)
	}
}

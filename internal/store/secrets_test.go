package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func newProject(env ...string) *Project {
	return &Project{Name: "web", Source: Source{Type: SourceImage, Image: "nginx"}, Runtime: Runtime{Ports: []string{}, Env: env}}
}

func rawValue(t *testing.T, path, query string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(query).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEnvSealedAtRestReadableThroughStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "o.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	dev, _, err := st.CreateDevice(ctx, "pi", nil, api.HostInfo{})
	if err != nil {
		t.Fatal(err)
	}
	p := newProject("# the db password is hunter2", "", "DB_PASS=hunter2", "EMPTY=")
	p.DeviceID = dev.ID
	if err := st.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "github_token", "ghp_abc123"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(ctx, NewDeployment{ProjectID: p.ID, DeviceID: dev.ID, Trigger: "manual", Spec: api.DeploySpec{Service: "web", Image: "nginx", Env: []string{"DB_PASS=hunter2"}}}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	for _, q := range []string{`SELECT runtime FROM projects`, `SELECT value FROM settings WHERE key='github_token'`, `SELECT spec FROM deployments`, `SELECT payload FROM commands`} {
		if v := rawValue(t, path, q); strings.Contains(v, "hunter2") || strings.Contains(v, "ghp_abc123") {
			t.Fatalf("%s holds plaintext: %s", q, v)
		}
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Runtime.Env, "|") != "# the db password is hunter2||DB_PASS=hunter2|EMPTY=" {
		t.Fatalf("env = %q", got.Runtime.Env)
	}
	if v, _ := st.GetSetting(ctx, "github_token"); v != "ghp_abc123" {
		t.Fatalf("token = %q", v)
	}
}

func TestPlaintextMigratedOnOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "o.db")
	st, _ := Open(path)
	p := newProject("A=1")
	st.CreateProject(ctx, p)
	// simulate an older version that stored plaintext
	st.db.Exec(`UPDATE projects SET runtime = ?`, `{"ports":[],"env":["A=secretvalue"],"restart":"no"}`)
	st.db.Exec(`INSERT INTO settings(key, value) VALUES('cloudflare_token','cf-plain') ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	st.Close()
	st, _ = Open(path)
	st.Close()
	if v := rawValue(t, path, `SELECT runtime FROM projects`); strings.Contains(v, "secretvalue") {
		t.Fatalf("not migrated: %s", v)
	}
	if v := rawValue(t, path, `SELECT value FROM settings WHERE key='cloudflare_token'`); strings.Contains(v, "cf-plain") {
		t.Fatalf("setting not migrated: %s", v)
	}
	st, _ = Open(path)
	defer st.Close()
	got, _ := st.GetProject(ctx, p.ID)
	if got.Runtime.Env[0] != "A=secretvalue" {
		t.Fatalf("env = %q", got.Runtime.Env)
	}
	if v, _ := st.GetSetting(ctx, "cloudflare_token"); v != "cf-plain" {
		t.Fatalf("token = %q", v)
	}
}

func TestBackupCarriesKeyOnlyWhenAsked(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := Open(filepath.Join(dir, "a.db"))
	p := newProject("A=topsecret")
	st.CreateProject(ctx, p)
	plain, withKey := filepath.Join(dir, "plain.db"), filepath.Join(dir, "key.db")
	if err := st.Snapshot(ctx, plain, false); err != nil {
		t.Fatal(err)
	}
	if err := st.Snapshot(ctx, withKey, true); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// A new server (different key) restoring the plain backup cannot read the value.
	os.Rename(plain, filepath.Join(dir, "b.db"))
	b, _ := Open(filepath.Join(dir, "b.db"))
	got, _ := b.GetProject(ctx, p.ID)
	if !IsSealed(got.Runtime.Env[0]) {
		t.Fatalf("expected sealed, got %q", got.Runtime.Env)
	}
	b.Close()

	// The backup that carries the key restores readably and the key setting is removed.
	os.Rename(withKey, filepath.Join(dir, "c.db"))
	c, _ := Open(filepath.Join(dir, "c.db"))
	defer c.Close()
	got, _ = c.GetProject(ctx, p.ID)
	if got.Runtime.Env[0] != "A=topsecret" {
		t.Fatalf("env = %q", got.Runtime.Env)
	}
	if _, err := c.GetSetting(ctx, settingBackupKey); err == nil {
		t.Fatal("backup key should be removed once adopted")
	}
}

// Checking a backup before it is staged must leave the key it carries in place, or restoring a
// protected backup on a fresh server loses every sealed value.
func TestCheckSnapshotKeepsBackupKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := Open(filepath.Join(dir, "a.db"))
	p := newProject("A=topsecret")
	st.CreateProject(ctx, p)
	withKey := filepath.Join(dir, "key.db")
	if err := st.Snapshot(ctx, withKey, true); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := CheckSnapshot(withKey); err != nil {
		t.Fatal(err)
	}
	os.Remove(withKey + "-wal")
	os.Remove(withKey + "-shm")
	os.Rename(withKey, filepath.Join(dir, "fresh.db"))
	c, err := Open(filepath.Join(dir, "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, _ := c.GetProject(ctx, p.ID)
	if got.Runtime.Env[0] != "A=topsecret" {
		t.Fatalf("env after check = %q", got.Runtime.Env)
	}
	bad := filepath.Join(dir, "bad.db")
	os.WriteFile(bad, []byte("not a database at all, just text padding padding padding"), 0o600)
	if CheckSnapshot(bad) == nil {
		t.Fatal("a non-database must be refused")

func TestStepLogIsCappedAndEventsPruned(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		st.AddEvent(ctx, "", "info", "k", "m")
	}
	if n, err := st.PruneEvents(ctx, 3); err != nil || n != 7 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if ev, _ := st.ListEvents(ctx, "", 100); len(ev) != 3 {
		t.Fatalf("kept %d events", len(ev))
	}
	if n, err := st.PruneEvents(ctx, 3); err != nil || n != 0 {
		t.Fatalf("second prune %d, %v", n, err)
	}
}

func TestStepLogStopsGrowing(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	dev, _, err := st.CreateDevice(ctx, "dev", nil, api.HostInfo{Hostname: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	pr := newProject()
	pr.DeviceID = dev.ID
	if err := st.CreateProject(ctx, pr); err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDeployment(ctx, NewDeployment{ProjectID: pr.ID, DeviceID: dev.ID, Trigger: "manual", Spec: api.DeploySpec{Service: "x", Image: "i"}})
	if err != nil {
		t.Fatal(err)
	}
	chunk := []api.LogLine{{Text: strings.Repeat("x", 100000)}}
	for i := 0; i < 40; i++ { // 4 MB offered
		if err := st.UpdateStep(ctx, dev.ID, d.ID, 0, api.StepUpdate{Lines: chunk}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := st.GetDeployment(ctx, d.ID)
	n := len(got.Steps[0].Log)
	if n < maxStepLog || n > maxStepLog+200000 {
		t.Fatalf("log is %d bytes, want about %d", n, maxStepLog)
	}
}

func TestMoreSettingsSealedAndOldPlaintextMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "o.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	raw := map[string]string{"cloudflare_deploy_token": "cf-deploy", "tunnel:dev_1": `{"token":"tun-secret"}`, "ha:state": `{"token":"peer-secret"}`, "backup_oauth_clients": `{"google":{"client_secret":"g"}}`}
	for k, v := range raw {
		if _, err := st.db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)`, k, v); err != nil { // as an older version stored them
			t.Fatal(err)
		}
	}
	st.Close()
	st, err = Open(path) // opening seals what is still plain
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for k, v := range raw {
		var stored string
		st.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, k).Scan(&stored)
		if !strings.HasPrefix(stored, sealPrefix) || strings.Contains(stored, "secret") {
			t.Errorf("%s not sealed at rest: %q", k, stored)
		}
		if got, _ := st.GetSetting(ctx, k); got != v {
			t.Errorf("%s reads back %q, want %q", k, got, v)
		}
	}
	st.SetSetting(ctx, "tunnel:dev_2", "new-secret")
	var stored string
	st.db.QueryRow(`SELECT value FROM settings WHERE key = 'tunnel:dev_2'`).Scan(&stored)
	if !strings.HasPrefix(stored, sealPrefix) {
		t.Errorf("new tunnel setting stored in the clear: %q", stored)
	}
	if m, _ := st.ListSettingsWithPrefix(ctx, "tunnel:"); m["tunnel:dev_2"] != "new-secret" {
		t.Errorf("prefix listing = %v", m)
	}
}

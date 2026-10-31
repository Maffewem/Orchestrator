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

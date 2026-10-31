package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Secrets (environment values, tokens and keys) are sealed with AES-256-GCM under a key the
// server holds outside the database: ORCH_SECRET_KEY (64 hex characters) if set, otherwise a
// random key kept in <db>.key (mode 0600). Anything that reads the database file alone,
// such as a copied backup, sees only "enc:v1:..." text.
const sealPrefix = "enc:v1:"

// sensitiveSettings are the settings sealed at rest.
var sensitiveSettings = map[string]bool{
	"github_token": true, "github_webhook_secret": true, "cloudflare_token": true,
	"backup_password": true, "sso_config": true, "server_backup_destinations": true,
	"https_config": true, "join_token": true,
}

// settingBackupKey carries the secret key inside a password-protected server backup so that
// restoring it on a fresh server can read the sealed values.
const settingBackupKey = "backup_secret_key"

type box struct {
	aead cipher.AEAD
	key  []byte
	path string // key file; empty when the key came from the environment or is ephemeral
}

func newBox(key []byte, path string) (*box, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &box{aead: g, key: key, path: path}, nil
}

// loadBox finds the server's secret key, creating it on first run. An in-memory database gets a throwaway key.
func loadBox(dbPath string) (*box, error) {
	if env := strings.TrimSpace(os.Getenv("ORCH_SECRET_KEY")); env != "" {
		k, err := hex.DecodeString(env)
		if err != nil || len(k) != 32 {
			return nil, errors.New("ORCH_SECRET_KEY must be 64 hex characters (32 bytes)")
		}
		return newBox(k, "")
	}
	if dbPath == "" || strings.Contains(dbPath, ":memory:") || strings.Contains(dbPath, "mode=memory") {
		k := make([]byte, 32)
		rand.Read(k)
		return newBox(k, "")
	}
	path := dbPath + ".key"
	if b, err := os.ReadFile(path); err == nil {
		k, err := hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(k) != 32 {
			return nil, fmt.Errorf("%s is not a valid secret key", path)
		}
		return newBox(k, path)
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(k)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return newBox(k, path)
}

func (b *box) seal(plain string) string {
	nonce := make([]byte, b.aead.NonceSize())
	rand.Read(nonce)
	return sealPrefix + base64.RawURLEncoding.EncodeToString(b.aead.Seal(nonce, nonce, []byte(plain), nil))
}

// open returns the plaintext of a sealed string; ok is false when v is not sealed or cannot be opened.
func (b *box) open(v string) (string, bool) {
	if !strings.HasPrefix(v, sealPrefix) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(v[len(sealPrefix):])
	if err != nil || len(raw) < b.aead.NonceSize() {
		return "", false
	}
	out, err := b.aead.Open(nil, raw[:b.aead.NonceSize()], raw[b.aead.NonceSize():], nil)
	return string(out), err == nil
}

// IsSealed reports whether an environment line still holds a sealed value (one that could not be opened).
func IsSealed(line string) bool {
	if strings.HasPrefix(line, sealPrefix) {
		return true
	}
	_, v, ok := strings.Cut(line, "=")
	return ok && strings.HasPrefix(v, sealPrefix)
}

// sealLine seals one stored environment line. KEY=value keeps its name readable; comments,
// which may mention secrets too, are sealed whole. Blank lines and empty values stay as they are.
func (b *box) sealLine(l string) string {
	t := strings.TrimSpace(l)
	if t == "" {
		return l
	}
	if strings.HasPrefix(t, "#") {
		return b.seal(l)
	}
	k, v, ok := strings.Cut(l, "=")
	if !ok {
		return b.seal(l)
	}
	if v == "" {
		return l
	}
	return k + "=" + b.seal(v)
}

func (b *box) openLine(l string) string {
	if p, ok := b.open(l); ok {
		return p
	}
	if k, v, ok := strings.Cut(l, "="); ok {
		if p, ok := b.open(v); ok {
			return k + "=" + p
		}
	}
	return l
}

func (b *box) sealLines(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, l := range in {
		out[i] = b.sealLine(l)
	}
	return out
}

func (b *box) openLines(in []string) {
	for i, l := range in {
		in[i] = b.openLine(l)
	}
}

// sealSpec returns a copy of spec with its environment values sealed.
func (b *box) sealSpec(spec api.DeploySpec) api.DeploySpec {
	spec.Env = b.sealLines(spec.Env)
	if spec.Compose != nil {
		c := *spec.Compose
		c.Env = b.sealLines(c.Env)
		spec.Compose = &c
	}
	return spec
}

func (b *box) openSpec(spec *api.DeploySpec) {
	b.openLines(spec.Env)
	if spec.Compose != nil {
		b.openLines(spec.Compose.Env)
	}
}

func (b *box) sealRuntime(r Runtime) Runtime {
	r.Env = b.sealLines(r.Env)
	return r
}

// EnvAssignments returns the KEY=value lines of a stored environment, without comments and blank lines.
func EnvAssignments(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			out = append(out, l)
		}
	}
	return out
}

// unsealed reports whether any line would be changed by sealing, that is, still holds plaintext.
func unsealed(lines []string) bool {
	for _, l := range lines {
		if IsSealed(l) {
			continue
		}
		if t := strings.TrimSpace(l); t != "" {
			if _, v, ok := strings.Cut(l, "="); !ok || v != "" || strings.HasPrefix(t, "#") {
				return true
			}
		}
	}
	return false
}

// migrateSecrets seals plaintext values left by earlier versions. It is safe to run on every start.
func (s *Store) migrateSecrets() error {
	b := s.box
	// projects.runtime
	rows, err := s.db.Query(`SELECT id, runtime FROM projects`)
	if err != nil {
		return err
	}
	type fix struct{ id, val string }
	var fixes []fix
	for rows.Next() {
		var id, js string
		if err := rows.Scan(&id, &js); err != nil {
			rows.Close()
			return err
		}
		var r Runtime
		if json.Unmarshal([]byte(js), &r) == nil && unsealed(r.Env) {
			fixes = append(fixes, fix{id, mustJSON(b.sealRuntimeKeep(r))})
		}
	}
	rows.Close()
	for _, f := range fixes {
		if _, err := s.db.Exec(`UPDATE projects SET runtime = ? WHERE id = ?`, f.val, f.id); err != nil {
			return err
		}
	}
	for _, t := range []struct{ sel, upd string }{
		{`SELECT id, spec FROM deployments`, `UPDATE deployments SET spec = ? WHERE id = ?`},
		{`SELECT id, payload FROM commands WHERE payload != 'null'`, `UPDATE commands SET payload = ? WHERE id = ?`},
	} {
		rows, err := s.db.Query(t.sel)
		if err != nil {
			return err
		}
		fixes = nil
		for rows.Next() {
			var id, js string
			if err := rows.Scan(&id, &js); err != nil {
				rows.Close()
				return err
			}
			var sp api.DeploySpec
			if json.Unmarshal([]byte(js), &sp) != nil {
				continue
			}
			if unsealed(sp.Env) || (sp.Compose != nil && unsealed(sp.Compose.Env)) {
				fixes = append(fixes, fix{id, mustJSON(b.sealSpecKeep(sp))})
			}
		}
		rows.Close()
		for _, f := range fixes {
			if _, err := s.db.Exec(t.upd, f.val, f.id); err != nil {
				return err
			}
		}
	}
	for k := range sensitiveSettings {
		var v string
		err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, k).Scan(&v)
		if errors.Is(err, sql.ErrNoRows) || v == "" || strings.HasPrefix(v, sealPrefix) {
			continue
		} else if err != nil {
			return err
		}
		if _, err := s.db.Exec(`UPDATE settings SET value = ? WHERE key = ?`, b.seal(v), k); err != nil {
			return err
		}
	}
	return nil
}

// sealRuntimeKeep and sealSpecKeep seal only lines that are not sealed yet.
func (b *box) sealRuntimeKeep(r Runtime) Runtime {
	r.Env = b.sealMissing(r.Env)
	return r
}

func (b *box) sealSpecKeep(sp api.DeploySpec) api.DeploySpec {
	sp.Env = b.sealMissing(sp.Env)
	if sp.Compose != nil {
		c := *sp.Compose
		c.Env = b.sealMissing(c.Env)
		sp.Compose = &c
	}
	return sp
}

func (b *box) sealMissing(in []string) []string {
	out := make([]string, len(in))
	for i, l := range in {
		if IsSealed(l) {
			out[i] = l
		} else {
			out[i] = b.sealLine(l)
		}
	}
	return out
}

// adoptBackupKey takes over the secret key a restored server backup carries, so its sealed
// values stay readable on a server that never had the original key.
func (s *Store) adoptBackupKey() error {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, settingBackupKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, settingBackupKey); err != nil {
		return err
	}
	k, err := hex.DecodeString(strings.TrimSpace(v))
	if err != nil || len(k) != 32 || string(k) == string(s.box.key) {
		return nil
	}
	if s.box.path == "" { // key from the environment (or in-memory): not ours to replace
		return nil
	}
	nb, err := newBox(k, s.box.path)
	if err != nil {
		return err
	}
	os.Rename(s.box.path, s.box.path+".pre-restore")
	if err := os.WriteFile(s.box.path, []byte(hex.EncodeToString(k)+"\n"), 0o600); err != nil {
		return err
	}
	s.box = nb
	return nil
}

// withBackupKey stores the secret key in the database at path, for a backup that will be password-protected.
func (s *Store) withBackupKey(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		settingBackupKey, hex.EncodeToString(s.box.key))
	return err
}

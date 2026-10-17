// Package store persists control-plane state in SQLite.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS domains (
	id          TEXT PRIMARY KEY,
	project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	hostname    TEXT NOT NULL,
	kind        TEXT NOT NULL,
	path        TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	UNIQUE(hostname, path)
);
CREATE TABLE IF NOT EXISTS devices (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	token_hash  TEXT NOT NULL UNIQUE,
	labels      TEXT NOT NULL DEFAULT '[]',
	host        TEXT NOT NULL DEFAULT '{}',
	metrics     TEXT NOT NULL DEFAULT '{}',
	containers  TEXT NOT NULL DEFAULT '[]',
	created_at  INTEGER NOT NULL,
	last_seen   INTEGER NOT NULL,
	offline     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS commands (
	id          TEXT PRIMARY KEY,
	device_id   TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	action      TEXT NOT NULL,
	target      TEXT NOT NULL,
	args        TEXT NOT NULL DEFAULT '{}',
	payload     TEXT NOT NULL DEFAULT 'null',
	status      TEXT NOT NULL,
	output      TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS commands_device_status ON commands(device_id, status);
CREATE TABLE IF NOT EXISTS events (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	device_id   TEXT NOT NULL DEFAULT '',
	level       TEXT NOT NULL,
	kind        TEXT NOT NULL,
	message     TEXT NOT NULL,
	created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS events_device ON events(device_id, id);
CREATE TABLE IF NOT EXISTS projects (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL UNIQUE,
	device_id   TEXT NOT NULL DEFAULT '',
	source      TEXT NOT NULL,
	runtime     TEXT NOT NULL DEFAULT '{}',
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS deployments (
	id          TEXT PRIMARY KEY,
	project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	device_id   TEXT NOT NULL,
	trigger     TEXT NOT NULL,
	git_commit  TEXT NOT NULL DEFAULT 'null',
	spec        TEXT NOT NULL,
	status      TEXT NOT NULL,
	error       TEXT NOT NULL DEFAULT '',
	command_id  TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	started_at  INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS deployments_project ON deployments(project_id, created_at);
CREATE TABLE IF NOT EXISTS backups (
	id             TEXT PRIMARY KEY,
	device_id      TEXT NOT NULL,
	device_name    TEXT NOT NULL,
	container_name TEXT NOT NULL,
	image          TEXT NOT NULL DEFAULT '',
	status         TEXT NOT NULL,
	size           INTEGER NOT NULL DEFAULT 0,
	error          TEXT NOT NULL DEFAULT '',
	created_at     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS backup_policies (
	device_id      TEXT NOT NULL,
	container_name TEXT NOT NULL,
	policy         TEXT NOT NULL,
	set_at         INTEGER NOT NULL,
	last_run       INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (device_id, container_name)
);
CREATE TABLE IF NOT EXISTS deployment_steps (
	deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
	idx           INTEGER NOT NULL,
	name          TEXT NOT NULL,
	status        TEXT NOT NULL,
	log           TEXT NOT NULL DEFAULT '',
	started_at    INTEGER NOT NULL DEFAULT 0,
	finished_at   INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (deployment_id, idx)
);
`

type Store struct {
	db *sql.DB
}

// Open opens (and migrates) the SQLite database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer; a single connection keeps ":memory:" databases shared too.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := addColumn(db, "backups", "source", "TEXT NOT NULL DEFAULT 'manual'"); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// addColumn adds a column to a table created by an earlier version, if it is missing.
func addColumn(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		found = found || n == column
	}
	rows.Close()
	if found {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + decl)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// Device is a registered agent and its most recently reported state.
type Device struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Labels     []string        `json:"labels"`
	Host       api.HostInfo    `json:"host"`
	Metrics    api.Metrics     `json:"metrics"`
	Containers []api.Container `json:"containers"`
	CreatedAt  time.Time       `json:"created_at"`
	LastSeen   time.Time       `json:"last_seen"`
	Offline    bool            `json:"-"`
}

type Event struct {
	ID        int64     `json:"id"`
	DeviceID  string    `json:"device_id,omitempty"`
	Level     string    `json:"level"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// NewToken returns a random URL-safe secret.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}

// HashToken is how secrets are stored at rest.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// --- settings ---

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

// --- devices ---

const deviceCols = `id, name, labels, host, metrics, containers, created_at, last_seen, offline`

func scanDevice(sc interface{ Scan(...any) error }) (*Device, error) {
	var d Device
	var labels, host, metrics, containers string
	var created, seen int64
	var offline int
	if err := sc.Scan(&d.ID, &d.Name, &labels, &host, &metrics, &containers, &created, &seen, &offline); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		raw string
		dst any
	}{{labels, &d.Labels}, {host, &d.Host}, {metrics, &d.Metrics}, {containers, &d.Containers}} {
		if err := json.Unmarshal([]byte(f.raw), f.dst); err != nil {
			return nil, err
		}
	}
	if d.Labels == nil {
		d.Labels = []string{}
	}
	if d.Containers == nil {
		d.Containers = []api.Container{}
	}
	d.CreatedAt = time.Unix(created, 0).UTC()
	d.LastSeen = time.Unix(seen, 0).UTC()
	d.Offline = offline != 0
	return &d, nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// CreateDevice registers a new device and returns it together with its plaintext agent token.
func (s *Store) CreateDevice(ctx context.Context, name string, labels []string, host api.HostInfo) (*Device, string, error) {
	if labels == nil {
		labels = []string{}
	}
	token := NewToken()
	now := time.Now().UTC().Truncate(time.Second)
	d := &Device{ID: newID("dev_"), Name: name, Labels: labels, Host: host, Containers: []api.Container{}, CreatedAt: now, LastSeen: now}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO devices(id, name, token_hash, labels, host, created_at, last_seen) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Name, HashToken(token), mustJSON(labels), mustJSON(host), now.Unix(), now.Unix())
	if err != nil {
		return nil, "", err
	}
	return d, token, nil
}

func (s *Store) DeviceByToken(ctx context.Context, token string) (*Device, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE token_hash = ?`, HashToken(token))
	d, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func (s *Store) GetDevice(ctx context.Context, id string) (*Device, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, id)
	d, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func (s *Store) ListDevices(ctx context.Context) ([]*Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deviceCols+` FROM devices ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDeviceState records a heartbeat.
func (s *Store) UpdateDeviceState(ctx context.Context, id string, host api.HostInfo, metrics api.Metrics, containers []api.Container) error {
	if containers == nil {
		containers = []api.Container{}
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE devices SET host = ?, metrics = ?, containers = ?, last_seen = ?, offline = 0 WHERE id = ?`,
		mustJSON(host), mustJSON(metrics), mustJSON(containers), time.Now().Unix(), id)
	return err
}

func (s *Store) UpdateDeviceMeta(ctx context.Context, id, name string, labels []string) error {
	if labels == nil {
		labels = []string{}
	}
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET name = ?, labels = ? WHERE id = ?`, name, mustJSON(labels), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeleteDevice(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkOffline flags devices not seen since the cutoff and returns the ones that just went offline.
func (s *Store) MarkOffline(ctx context.Context, cutoff time.Time) ([]*Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`UPDATE devices SET offline = 1 WHERE offline = 0 AND last_seen < ? RETURNING `+deviceCols, cutoff.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- commands ---

const commandCols = `id, device_id, action, target, args, payload, status, output, error, created_at, updated_at`

func scanCommand(sc interface{ Scan(...any) error }) (*api.Command, error) {
	var c api.Command
	var args, payload string
	var created, updated int64
	if err := sc.Scan(&c.ID, &c.DeviceID, &c.Action, &c.Target, &args, &payload, &c.Status, &c.Output, &c.Error, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(args), &c.Args); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(payload), &c.Deploy); err != nil {
		return nil, err
	}
	c.CreatedAt = time.Unix(created, 0).UTC()
	c.UpdatedAt = time.Unix(updated, 0).UTC()
	return &c, nil
}

func (s *Store) CreateCommand(ctx context.Context, deviceID, action, target string, args map[string]string) (*api.Command, error) {
	c := newCommand(deviceID, action, target, args, nil)
	return c, insertCommand(ctx, s.db, c)
}

func newCommand(deviceID, action, target string, args map[string]string, deploy *api.DeploySpec) *api.Command {
	now := time.Now().UTC().Truncate(time.Second)
	return &api.Command{ID: newID("cmd_"), DeviceID: deviceID, Action: action, Target: target, Args: args,
		Deploy: deploy, Status: api.CommandPending, CreatedAt: now, UpdatedAt: now}
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertCommand(ctx context.Context, db execer, c *api.Command) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO commands(id, device_id, action, target, args, payload, status, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.DeviceID, c.Action, c.Target, mustJSON(c.Args), mustJSON(c.Deploy), c.Status, c.CreatedAt.Unix(), c.UpdatedAt.Unix())
	return err
}

func (s *Store) GetCommand(ctx context.Context, id string) (*api.Command, error) {
	c, err := scanCommand(s.db.QueryRowContext(ctx, `SELECT `+commandCols+` FROM commands WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// DispatchCommands moves a device's pending commands to dispatched and returns them, oldest first.
func (s *Store) DispatchCommands(ctx context.Context, deviceID string) ([]api.Command, error) {
	rows, err := s.db.QueryContext(ctx,
		`UPDATE commands SET status = ?, updated_at = ? WHERE device_id = ? AND status = ? RETURNING `+commandCols,
		api.CommandDispatched, time.Now().Unix(), deviceID, api.CommandPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Command{}
	for rows.Next() {
		c, err := scanCommand(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// RETURNING order is unspecified.
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// CompleteCommand stores a result for a command owned by deviceID.
func (s *Store) CompleteCommand(ctx context.Context, deviceID, id string, res api.CommandResult) (*api.Command, error) {
	status := api.CommandSucceeded
	if !res.OK {
		status = api.CommandFailed
	}
	row := s.db.QueryRowContext(ctx,
		`UPDATE commands SET status = ?, output = ?, error = ?, updated_at = ? WHERE id = ? AND device_id = ? RETURNING `+commandCols,
		status, res.Output, res.Error, time.Now().Unix(), id, deviceID)
	c, err := scanCommand(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return c, err
}

// ExpireCommands fails commands that were never picked up (pending before pendingCutoff) or
// never answered (dispatched before dispatchedCutoff), along with any deployment they drive.
func (s *Store) ExpireCommands(ctx context.Context, pendingCutoff, dispatchedCutoff time.Time) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`UPDATE commands SET status = ?, error = 'timed out waiting for agent', updated_at = ?
		 WHERE (status = ? AND updated_at < ?) OR (status = ? AND updated_at < ?)`,
		api.CommandFailed, now, api.CommandPending, pendingCutoff.Unix(), api.CommandDispatched, dispatchedCutoff.Unix())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return n, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id FROM deployments d JOIN commands c ON c.id = d.command_id
		 WHERE c.status = ? AND d.status IN (?, ?)`, api.CommandFailed, DeploymentQueued, DeploymentRunning)
	if err != nil {
		return n, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return n, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := s.FinishDeployment(ctx, id, false, "timed out waiting for agent"); err != nil {
			return n, err
		}
	}
	return n, nil
}

// --- events ---

func (s *Store) AddEvent(ctx context.Context, deviceID, level, kind, message string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events(device_id, level, kind, message, created_at) VALUES(?, ?, ?, ?, ?)`,
		deviceID, level, kind, message, time.Now().Unix())
	return err
}

// ListEvents returns the newest events, optionally for one device.
func (s *Store) ListEvents(ctx context.Context, deviceID string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, device_id, level, kind, message, created_at FROM events`
	args := []any{}
	if deviceID != "" {
		q += ` WHERE device_id = ?`
		args = append(args, deviceID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var created int64
		if err := rows.Scan(&e.ID, &e.DeviceID, &e.Level, &e.Kind, &e.Message, &created); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

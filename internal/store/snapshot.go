package store

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/Maffewem/Orchestrator/internal/api"
	"os"
	"time"
)

// Snapshot writes a consistent copy of the whole database to path (which must not exist).
// With withKey the secret key is stored inside the copy, so only do that for a copy that will be
// password-protected: it is what lets a restore on a new server read the sealed values.
func (s *Store) Snapshot(ctx context.Context, path string, withKey bool) error {
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return err
	}
	if withKey {
		return s.withBackupKey(ctx, path)
	}
	return nil
}

// CheckSnapshot opens the database file at path and confirms it is an intact Orchestrator
// database. It deliberately does not go through Open: opening a store adopts the secret key a
// protected backup carries and re-seals values under a throwaway key file, which would strip the
// key out of the very file that is about to be restored.
func CheckSnapshot(path string) error {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var res string
	if err := db.QueryRow(`PRAGMA quick_check(1)`).Scan(&res); err != nil {
		return fmt.Errorf("not an Orchestrator database: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("the database is damaged: %s", res)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return fmt.Errorf("not an Orchestrator database: %w", err)
	}
	return nil
}

// PendingRestorePath is where a database waiting to replace the live one is kept.
func PendingRestorePath(dbPath string) string { return dbPath + ".restore" }

// ApplyPendingRestore swaps in a restored database left by a previous run. The old database
// is kept next to it as .pre-restore. Call it before Open.
func ApplyPendingRestore(dbPath string) (bool, error) {
	pending := PendingRestorePath(dbPath)
	if _, err := os.Stat(pending); err != nil {
		return false, nil
	}
	if _, err := os.Stat(dbPath); err == nil {
		if err := os.Rename(dbPath, dbPath+".pre-restore"); err != nil {
			return false, err
		}
	}
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
	return true, os.Rename(pending, dbPath)
}

// SnapshotForMove writes a copy of the database for a server move. It always carries the
// secret key (the copy must be protected before it leaves the server), applies set to the
// copy's settings and removes the settings named in drop, so the new server starts from a
// clean slate of its own move state while the live database is untouched.
func (s *Store) SnapshotForMove(ctx context.Context, path string, set map[string]string, drop []string) error {
	if err := s.Snapshot(ctx, path, true); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, k := range drop {
		if _, err := db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, k); err != nil {
			return err
		}
	}
	for k, v := range set {
		if _, err := db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return nil
}

// BusyWork counts deployments that are queued or running and commands still waiting for or
// being carried out by an agent. A server move waits for both to reach zero, because work in
// flight would finish against a server that is about to hand over.
func (s *Store) BusyWork(ctx context.Context) (deployments, commands int, err error) {
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployments WHERE status IN (?, ?)`, DeploymentQueued, DeploymentRunning).Scan(&deployments); err != nil {
		return
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commands WHERE status IN (?, ?)`, api.CommandPending, api.CommandDispatched).Scan(&commands)
	return
}

// SnapshotAgents lists the token hashes of devices in the database file at path that were seen
// within window of its newest sighting and are not marked offline. A standby uses it to know
// which agents it should expect to hear from, without opening the file as a live store.
func SnapshotAgents(path string, window time.Duration) (map[string]struct{}, error) {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.Query(`SELECT token_hash FROM devices WHERE offline = 0 AND last_seen >= (SELECT COALESCE(MAX(last_seen), 0) FROM devices) - ?`, int64(window.Seconds()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out[h] = struct{}{}
	}
	return out, rows.Err()
}

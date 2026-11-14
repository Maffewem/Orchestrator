package store

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/Maffewem/Orchestrator/internal/api"
	"os"
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

// CheckSnapshot opens the database file at path and confirms it is an Orchestrator database.
func CheckSnapshot(path string) error {
	st, err := Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := st.CountUsers(context.Background()); err != nil {
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

package store

import (
	"context"
	"fmt"
	"os"
)

// Snapshot writes a consistent copy of the whole database to path (which must not exist).
func (s *Store) Snapshot(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
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

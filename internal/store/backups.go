package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Backup statuses.
const (
	BackupPending  = "pending"
	BackupComplete = "complete"
	BackupFailed   = "failed"
)

// Backup is a snapshot of one container's configuration and data. The archive itself
// lives on the control plane's disk, named after the backup ID.
type Backup struct {
	ID            string    `json:"id"`
	DeviceID      string    `json:"device_id"`
	DeviceName    string    `json:"device_name"`
	ContainerName string    `json:"container_name"`
	Image         string    `json:"image"`
	Status        string    `json:"status"`
	Size          int64     `json:"size"`
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

const backupCols = `id, device_id, device_name, container_name, image, status, size, error, created_at`

func scanBackup(r interface{ Scan(...any) error }) (*Backup, error) {
	var b Backup
	var created int64
	if err := r.Scan(&b.ID, &b.DeviceID, &b.DeviceName, &b.ContainerName, &b.Image, &b.Status, &b.Size, &b.Error, &created); err != nil {
		return nil, err
	}
	b.CreatedAt = time.Unix(created, 0).UTC()
	return &b, nil
}

func (s *Store) CreateBackup(ctx context.Context, b *Backup) error {
	b.ID, b.Status, b.CreatedAt = newID("bkp_"), BackupPending, time.Now().UTC().Truncate(time.Second)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO backups(`+backupCols+`) VALUES(?, ?, ?, ?, ?, ?, 0, '', ?)`,
		b.ID, b.DeviceID, b.DeviceName, b.ContainerName, b.Image, b.Status, b.CreatedAt.Unix())
	return err
}

func (s *Store) GetBackup(ctx context.Context, id string) (*Backup, error) {
	b, err := scanBackup(s.db.QueryRowContext(ctx, `SELECT `+backupCols+` FROM backups WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// ListBackups returns backups newest first, optionally for one device.
func (s *Store) ListBackups(ctx context.Context, deviceID string) ([]*Backup, error) {
	q, args := `SELECT `+backupCols+` FROM backups`, []any{}
	if deviceID != "" {
		q += ` WHERE device_id = ?`
		args = append(args, deviceID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at DESC, rowid DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Backup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// FinishBackup records the outcome of a backup. A completed backup is never downgraded.
func (s *Store) FinishBackup(ctx context.Context, id string, size int64, errMsg string) error {
	status := BackupComplete
	if errMsg != "" {
		status = BackupFailed
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE backups SET status = ?, size = ?, error = ? WHERE id = ? AND status != 'complete'`,
		status, size, errMsg, id)
	return err
}

func (s *Store) DeleteBackup(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM backups WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

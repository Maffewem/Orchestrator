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
	ID            string `json:"id"`
	DeviceID      string `json:"device_id"`
	DeviceName    string `json:"device_name"`
	ContainerName string `json:"container_name"`
	Image         string `json:"image"`
	Source        string `json:"source"` // manual | scheduled
	Status        string `json:"status"`
	Size          int64  `json:"size"`
	Error         string `json:"error,omitempty"`
	// Encrypted means the file on disk is password-protected (see package backupcrypt).
	Encrypted bool      `json:"encrypted"`
	CreatedAt time.Time `json:"created_at"`
}

const backupCols = `id, device_id, device_name, container_name, image, status, size, error, created_at, source, encrypted`

func scanBackup(r interface{ Scan(...any) error }) (*Backup, error) {
	var b Backup
	var created int64
	var enc int
	if err := r.Scan(&b.ID, &b.DeviceID, &b.DeviceName, &b.ContainerName, &b.Image, &b.Status, &b.Size, &b.Error, &created, &b.Source, &enc); err != nil {
		return nil, err
	}
	b.CreatedAt = time.Unix(created, 0).UTC()
	b.Encrypted = enc != 0
	return &b, nil
}

func (s *Store) CreateBackup(ctx context.Context, b *Backup) error {
	b.ID, b.Status, b.CreatedAt = newID("bkp_"), BackupPending, time.Now().UTC().Truncate(time.Second)
	if b.Source == "" {
		b.Source = "manual"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO backups(`+backupCols+`) VALUES(?, ?, ?, ?, ?, ?, 0, '', ?, ?, 0)`,
		b.ID, b.DeviceID, b.DeviceName, b.ContainerName, b.Image, b.Status, b.CreatedAt.Unix(), b.Source)
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
	return s.FinishBackupAs(ctx, id, size, errMsg, false)
}

// FinishBackupAs is FinishBackup that also records whether the stored file is encrypted.
// size is always the size of the archive itself, not of the encrypted file.
func (s *Store) FinishBackupAs(ctx context.Context, id string, size int64, errMsg string, encrypted bool) error {
	status := BackupComplete
	if errMsg != "" {
		status = BackupFailed
	}
	enc := 0
	if encrypted {
		enc = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE backups SET status = ?, size = ?, error = ?, encrypted = ? WHERE id = ? AND status != 'complete'`,
		status, size, errMsg, enc, id)
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

// PruneScheduledBackups deletes the oldest completed scheduled backups of a worker beyond
// keep, and returns their IDs so the caller can remove the files. Manual backups are kept.
func (s *Store) PruneScheduledBackups(ctx context.Context, deviceID, container string, keep int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM backups WHERE device_id = ? AND container_name = ? AND source = 'scheduled' AND status = 'complete'
		 ORDER BY created_at DESC, rowid DESC LIMIT -1 OFFSET ?`, deviceID, container, keep)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM backups WHERE id = ?`, id); err != nil {
			return ids, err
		}
	}
	return ids, nil
}

// BackupPolicy is a worker's stored backup schedule (policy is opaque JSON to the store).
type BackupPolicy struct {
	DeviceID      string
	ContainerName string
	Policy        string
	SetAt         time.Time
	LastRun       time.Time
}

func scanPolicy(r interface{ Scan(...any) error }) (*BackupPolicy, error) {
	var p BackupPolicy
	var set, last int64
	if err := r.Scan(&p.DeviceID, &p.ContainerName, &p.Policy, &set, &last); err != nil {
		return nil, err
	}
	p.SetAt, p.LastRun = time.Unix(set, 0).UTC(), time.Unix(last, 0).UTC()
	return &p, nil
}

const policyCols = `device_id, container_name, policy, set_at, last_run`

func (s *Store) GetBackupPolicy(ctx context.Context, deviceID, container string) (*BackupPolicy, error) {
	p, err := scanPolicy(s.db.QueryRowContext(ctx, `SELECT `+policyCols+` FROM backup_policies WHERE device_id = ? AND container_name = ?`, deviceID, container))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// SetBackupPolicy saves a policy. It restarts the schedule's baseline (so it waits for its
// next slot) but keeps the time of the last run.
func (s *Store) SetBackupPolicy(ctx context.Context, deviceID, container, policy string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO backup_policies(`+policyCols+`) VALUES(?, ?, ?, ?, 0)
		 ON CONFLICT(device_id, container_name) DO UPDATE SET policy = excluded.policy, set_at = excluded.set_at`,
		deviceID, container, policy, time.Now().Unix())
	return err
}

func (s *Store) ListBackupPolicies(ctx context.Context) ([]*BackupPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+policyCols+` FROM backup_policies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*BackupPolicy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) MarkBackupPolicyRun(ctx context.Context, deviceID, container string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backup_policies SET last_run = ? WHERE device_id = ? AND container_name = ?`,
		at.Unix(), deviceID, container)
	return err
}

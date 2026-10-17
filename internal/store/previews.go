package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Preview is a temporary environment for one open pull request: its own container,
// published on its own host port, rebuilt on every push to the branch.
type Preview struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	Branch       string    `json:"branch"`
	Slug         string    `json:"slug"`
	PR           int       `json:"pr"`
	Title        string    `json:"title"`
	Author       string    `json:"author"`
	SHA          string    `json:"sha"`
	HostPort     int       `json:"host_port"`
	DeploymentID string    `json:"deployment_id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

const previewCols = `id, project_id, branch, slug, pr, title, author, sha, host_port, deployment_id, created_at, updated_at`

func scanPreview(r interface{ Scan(...any) error }) (*Preview, error) {
	var p Preview
	var created, updated int64
	if err := r.Scan(&p.ID, &p.ProjectID, &p.Branch, &p.Slug, &p.PR, &p.Title, &p.Author, &p.SHA, &p.HostPort, &p.DeploymentID, &created, &updated); err != nil {
		return nil, err
	}
	p.CreatedAt = time.Unix(created, 0).UTC()
	p.UpdatedAt = time.Unix(updated, 0).UTC()
	return &p, nil
}

func (s *Store) CreatePreview(ctx context.Context, p *Preview) error {
	p.ID = newID("pre_")
	p.CreatedAt = time.Now().UTC().Truncate(time.Second)
	p.UpdatedAt = p.CreatedAt
	_, err := s.db.ExecContext(ctx, `INSERT INTO previews(`+previewCols+`) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.ProjectID, p.Branch, p.Slug, p.PR, p.Title, p.Author, p.SHA, p.HostPort, p.DeploymentID, p.CreatedAt.Unix(), p.UpdatedAt.Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrConflict
	}
	return err
}

// UpdatePreview records a new revision (and its deployment) for a preview.
func (s *Store) UpdatePreview(ctx context.Context, p *Preview) error {
	p.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	_, err := s.db.ExecContext(ctx, `UPDATE previews SET pr = ?, title = ?, author = ?, sha = ?, deployment_id = ?, updated_at = ? WHERE id = ?`,
		p.PR, p.Title, p.Author, p.SHA, p.DeploymentID, p.UpdatedAt.Unix(), p.ID)
	return err
}

func (s *Store) ListPreviews(ctx context.Context, projectID string) ([]*Preview, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+previewCols+` FROM previews WHERE project_id = ? ORDER BY updated_at DESC, rowid DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Preview
	for rows.Next() {
		p, err := scanPreview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPreview(ctx context.Context, id string) (*Preview, error) {
	p, err := scanPreview(s.db.QueryRowContext(ctx, `SELECT `+previewCols+` FROM previews WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) DeletePreview(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM previews WHERE id = ?`, id)
	return err
}

// PreviewPortInUse reports whether another preview or project on the device uses a host port.
func (s *Store) PreviewPortInUse(ctx context.Context, deviceID string, port int) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM previews v JOIN projects p ON p.id = v.project_id WHERE p.device_id = ? AND v.host_port = ?`, deviceID, port).Scan(&n)
	return n > 0, err
}

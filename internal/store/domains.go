package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Domain kinds.
const (
	DomainCustom = "domain" // a hostname served by the project
	DomainRoute  = "route"  // a hostname plus path prefix, e.g. example.com/api/*
)

// Domain is a custom hostname (or hostname/path route) attached to a project.
type Domain struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Hostname  string `json:"hostname"`
	Kind      string `json:"type"`
	Path      string `json:"path,omitempty"`
	// Access is "" (served by the device proxy, DNS points at the device) or "tunnel" (reached through
	// the device's Cloudflare Tunnel; no A record and no proxy site).
	Access    string    `json:"access,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

const domainCols = `id, project_id, hostname, kind, path, created_at, access`

func scanDomain(r interface{ Scan(...any) error }) (*Domain, error) {
	var d Domain
	var created int64
	if err := r.Scan(&d.ID, &d.ProjectID, &d.Hostname, &d.Kind, &d.Path, &created, &d.Access); err != nil {
		return nil, err
	}
	d.CreatedAt = time.Unix(created, 0).UTC()
	return &d, nil
}

// CreateDomain attaches a hostname to a project. Hostname+path must be unique.
func (s *Store) CreateDomain(ctx context.Context, projectID, hostname, kind, path, access string) (*Domain, error) {
	d := &Domain{ID: newID("dom_"), ProjectID: projectID, Hostname: hostname, Kind: kind, Path: path, Access: access, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	_, err := s.db.ExecContext(ctx, `INSERT INTO domains(id, project_id, hostname, kind, path, created_at, access) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.ProjectID, d.Hostname, d.Kind, d.Path, d.CreatedAt.Unix(), d.Access)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return nil, ErrConflict
	}
	return d, err
}

func (s *Store) ListDomains(ctx context.Context, projectID string) ([]*Domain, error) {
	q := `SELECT ` + domainCols + ` FROM domains`
	var args []any
	if projectID != "" {
		q += ` WHERE project_id = ?`
		args = append(args, projectID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at, rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Domain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetDomain(ctx context.Context, id string) (*Domain, error) {
	d, err := scanDomain(s.db.QueryRowContext(ctx, `SELECT `+domainCols+` FROM domains WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func (s *Store) DeleteDomain(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM domains WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetDomainAccess(ctx context.Context, id, access string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE domains SET access = ? WHERE id = ?`, access, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

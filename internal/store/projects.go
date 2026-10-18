package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

var ErrConflict = errors.New("conflict")

// Source types for a project.
const (
	SourceGitHub = "github"
	SourceImage  = "image"
)

// Source is where a project's container image comes from.
type Source struct {
	Type string `json:"type"`
	// GitHub source: owner/name, the branch that deploys, and where the Dockerfile lives.
	Repo       string `json:"repo,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Context    string `json:"context,omitempty"`
	Dockerfile string `json:"dockerfile,omitempty"`
	// Node, when set, builds a Node.js project with a generated Dockerfile instead of the
	// repository's own (Dockerfile is then ignored). Context is its root directory.
	Node *api.NodeSpec `json:"node,omitempty"`
	// AutoDeploy deploys every push to Branch.
	AutoDeploy bool `json:"auto_deploy"`
	// Image source: a registry reference such as nginx:1.27.
	Image string `json:"image,omitempty"`
}

// Runtime is how the project's container runs on its device.
type Runtime struct {
	Ports   []string `json:"ports"`
	Env     []string `json:"env"`
	Command []string `json:"command,omitempty"`
	Restart string   `json:"restart"`
}

// Project is a deployable service: a source, a target device and runtime settings.
type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	DeviceID  string    `json:"device_id"`
	Source    Source    `json:"source"`
	Runtime   Runtime   `json:"runtime"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const projectCols = `id, name, device_id, source, runtime, created_at, updated_at`

func scanProject(sc interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	var source, runtime string
	var created, updated int64
	if err := sc.Scan(&p.ID, &p.Name, &p.DeviceID, &source, &runtime, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(source), &p.Source); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(runtime), &p.Runtime); err != nil {
		return nil, err
	}
	if p.Runtime.Ports == nil {
		p.Runtime.Ports = []string{}
	}
	if p.Runtime.Env == nil {
		p.Runtime.Env = []string{}
	}
	p.CreatedAt = time.Unix(created, 0).UTC()
	p.UpdatedAt = time.Unix(updated, 0).UTC()
	return &p, nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (s *Store) CreateProject(ctx context.Context, p *Project) error {
	now := time.Now().UTC().Truncate(time.Second)
	p.ID, p.CreatedAt, p.UpdatedAt = newID("prj_"), now, now
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projects(id, name, device_id, source, runtime, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.DeviceID, mustJSON(p.Source), mustJSON(p.Runtime), now.Unix(), now.Unix())
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) UpdateProject(ctx context.Context, p *Project) error {
	p.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET name = ?, device_id = ?, source = ?, runtime = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.DeviceID, mustJSON(p.Source), mustJSON(p.Runtime), p.UpdatedAt.Unix(), p.ID)
	if isUnique(err) {
		return ErrConflict
	} else if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetProject(ctx context.Context, id string) (*Project, error) {
	p, err := scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) ListProjects(ctx context.Context) ([]*Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectCols+` FROM projects ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectsForPush returns GitHub projects that auto-deploy pushes to repo/branch.
func (s *Store) ProjectsForPush(ctx context.Context, repo, branch string) ([]*Project, error) {
	all, err := s.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	out := []*Project{}
	for _, p := range all {
		src := p.Source
		if src.Type == SourceGitHub && src.AutoDeploy && strings.EqualFold(src.Repo, repo) && src.Branch == branch {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Store) DeleteProject(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

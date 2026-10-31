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
	// SourceCompose is a Docker Compose stack; its YAML is stored with the project.
	SourceCompose = "compose"
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
	// AutoDeploy deploys every push to Branch. Off is "Disable builds" in the dashboard.
	AutoDeploy bool `json:"auto_deploy"`
	// Watch limits which changed files trigger a production build (everything when empty).
	Watch
	// Preview overrides the production build settings for preview deployments; empty
	// fields fall back to the production ones.
	Preview *PreviewBuild `json:"preview,omitempty"`
	// Image source: a registry reference such as nginx:1.27.
	Image string `json:"image,omitempty"`
	// Compose source: the docker-compose YAML. ${VAR} references are filled from the project's environment.
	Compose string `json:"compose,omitempty"`
	// ComposeFile is the path of the compose file in Repo (compose source from GitHub); empty means docker-compose.yml.
	ComposeFile string `json:"compose_file,omitempty"`
}

// FromRepo reports whether the source is a GitHub repository: a built project, or a Compose
// stack whose compose file lives in a repository. Pushes to its branch redeploy it.
func (s Source) FromRepo() bool {
	return s.Type == SourceGitHub || (s.Type == SourceCompose && s.Repo != "")
}

// Watch is a pair of path globs, relative to the repository root. A build is wanted when a
// changed file matches Include (empty means any file) and does not match Exclude.
type Watch struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

// PreviewBuild is the build configuration used for preview deployments.
type PreviewBuild struct {
	Context string `json:"context,omitempty"` // root directory
	Build   string `json:"build,omitempty"`   // Node build command
	Start   string `json:"start,omitempty"`   // Node preview (start) command
	Watch
}

// Runtime is how the project's container runs on its device.
type Runtime struct {
	Ports []string `json:"ports"`
	// WebPort is the host port that carries web traffic when a project publishes several; empty means the first TCP port.
	WebPort string   `json:"web_port,omitempty"`
	Env     []string `json:"env"`
	Command []string `json:"command,omitempty"`
	Restart string   `json:"restart"`
	// Volumes are "source:/path[:ro]" mounts (named volumes or host paths); Labels are extra container labels.
	Volumes []string          `json:"volumes,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	// Network is a Docker network name or "host"; empty is the default bridge.
	Network string `json:"network,omitempty"`
	// Container is the name of the container when it differs from the project name (adopted containers).
	Container string `json:"container,omitempty"`
	// Adopted marks a project created from a container that already existed. Orchestrator left that
	// container as it was; Unsupported lists settings it could not read back and a redeploy would drop.
	Adopted     bool     `json:"adopted,omitempty"`
	Unsupported []string `json:"unsupported,omitempty"`
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

func scanProject(b *box, sc interface{ Scan(...any) error }) (*Project, error) {
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
	b.openLines(p.Runtime.Env)
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
		p.ID, p.Name, p.DeviceID, mustJSON(p.Source), mustJSON(s.box.sealRuntime(p.Runtime)), now.Unix(), now.Unix())
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) UpdateProject(ctx context.Context, p *Project) error {
	p.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	res, err := s.db.ExecContext(ctx,
		`UPDATE projects SET name = ?, device_id = ?, source = ?, runtime = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.DeviceID, mustJSON(p.Source), mustJSON(s.box.sealRuntime(p.Runtime)), p.UpdatedAt.Unix(), p.ID)
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
	p, err := scanProject(s.box, s.db.QueryRowContext(ctx, `SELECT `+projectCols+` FROM projects WHERE id = ?`, id))
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
		p, err := scanProject(s.box, rows)
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
		if src.FromRepo() && src.AutoDeploy && strings.EqualFold(src.Repo, repo) && src.Branch == branch {
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

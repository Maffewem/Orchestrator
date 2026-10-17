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

// Deployment statuses.
const (
	DeploymentQueued    = "queued"
	DeploymentRunning   = "running"
	DeploymentSucceeded = "succeeded"
	DeploymentFailed    = "failed"
)

// Deployment triggers.
const (
	TriggerManual = "manual"
	TriggerPush   = "push"
	TriggerRetry  = "retry"
	TriggerPoll   = "poll"
)

// Commit is the git revision a deployment was built from.
type Commit struct {
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author"`
	URL     string `json:"url,omitempty"`
}

// Deployment is one attempt to roll a project out to a device.
type Deployment struct {
	ID         string         `json:"id"`
	ProjectID  string         `json:"project_id"`
	DeviceID   string         `json:"device_id"`
	Trigger    string         `json:"trigger"`
	Commit     *Commit        `json:"commit"`
	Spec       api.DeploySpec `json:"spec"`
	Status     string         `json:"status"`
	Error      string         `json:"error,omitempty"`
	CommandID  string         `json:"command_id"`
	CreatedAt  time.Time      `json:"created_at"`
	StartedAt  *time.Time     `json:"started_at"`
	FinishedAt *time.Time     `json:"finished_at"`
	Steps      []Step         `json:"steps,omitempty"`
}

// Step is one stage of a deployment, with its log.
type Step struct {
	Index      int        `json:"index"`
	Name       string     `json:"name"`
	Status     string     `json:"status"`
	Log        string     `json:"log"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

// Deployment and step times are stored as Unix milliseconds; 0 means "not yet".
func milliPtr(v int64) *time.Time {
	if v == 0 {
		return nil
	}
	t := time.UnixMilli(v).UTC()
	return &t
}

// formatLines renders log lines as "<RFC3339 millis> <text>\n", one per line.
func formatLines(lines []api.LogLine) string {
	var b strings.Builder
	for _, l := range lines {
		ts := l.Time
		if ts.IsZero() {
			ts = time.Now()
		}
		for _, text := range strings.Split(strings.TrimRight(l.Text, "\n"), "\n") {
			b.WriteString(ts.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
			b.WriteByte(' ')
			b.WriteString(strings.TrimRight(text, "\r"))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

const deploymentCols = `id, project_id, device_id, trigger, git_commit, spec, status, error, command_id, created_at, started_at, finished_at`

func scanDeployment(sc interface{ Scan(...any) error }) (*Deployment, error) {
	var d Deployment
	var commit, spec string
	var created, started, finished int64
	if err := sc.Scan(&d.ID, &d.ProjectID, &d.DeviceID, &d.Trigger, &commit, &spec, &d.Status, &d.Error, &d.CommandID, &created, &started, &finished); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(spec), &d.Spec); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(commit), &d.Commit); err != nil {
		return nil, err
	}
	d.CreatedAt = time.UnixMilli(created).UTC()
	d.StartedAt = milliPtr(started)
	d.FinishedAt = milliPtr(finished)
	return &d, nil
}

// NewDeployment is the input to CreateDeployment.
type NewDeployment struct {
	ProjectID string
	DeviceID  string
	Trigger   string
	Commit    *Commit
	Spec      api.DeploySpec
}

// CreateDeployment records a deployment with all its steps pending and queues the deploy
// command for the device's agent, atomically.
func (s *Store) CreateDeployment(ctx context.Context, in NewDeployment) (*Deployment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	id := newID("dep_")
	spec := in.Spec
	cmd := newCommand(in.DeviceID, api.ActionDeploy, id, nil, &spec)
	if err := insertCommand(ctx, tx, cmd); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO deployments(id, project_id, device_id, trigger, git_commit, spec, status, command_id, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.ProjectID, in.DeviceID, in.Trigger, mustJSON(in.Commit), mustJSON(spec), DeploymentQueued, cmd.ID, now.UnixMilli()); err != nil {
		return nil, err
	}
	names := api.StepsFor(spec)
	steps := make([]Step, len(names))
	for i, name := range names {
		steps[i] = Step{Index: i, Name: name, Status: api.StepPending}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO deployment_steps(deployment_id, idx, name, status) VALUES(?, ?, ?, ?)`,
			id, i, name, api.StepPending); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Deployment{ID: id, ProjectID: in.ProjectID, DeviceID: in.DeviceID, Trigger: in.Trigger, Commit: in.Commit,
		Spec: spec, Status: DeploymentQueued, CommandID: cmd.ID, CreatedAt: now, Steps: steps}, nil
}

// GetDeployment returns a deployment with its steps.
func (s *Store) GetDeployment(ctx context.Context, id string) (*Deployment, error) {
	d, err := scanDeployment(s.db.QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM deployments WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT idx, name, status, log, started_at, finished_at FROM deployment_steps WHERE deployment_id = ? ORDER BY idx`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	d.Steps = []Step{}
	for rows.Next() {
		var st Step
		var started, finished int64
		if err := rows.Scan(&st.Index, &st.Name, &st.Status, &st.Log, &started, &finished); err != nil {
			return nil, err
		}
		st.StartedAt = milliPtr(started)
		st.FinishedAt = milliPtr(finished)
		d.Steps = append(d.Steps, st)
	}
	return d, rows.Err()
}

// DeploymentFilter narrows ListDeployments; zero values match everything.
type DeploymentFilter struct {
	DeviceID  string
	ProjectID string
	Limit     int
}

// ListDeployments returns deployments newest first, without steps.
func (s *Store) ListDeployments(ctx context.Context, f DeploymentFilter) ([]*Deployment, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q := `SELECT ` + deploymentCols + ` FROM deployments WHERE 1 = 1`
	args := []any{}
	if f.DeviceID != "" {
		q += ` AND device_id = ?`
		args = append(args, f.DeviceID)
	}
	if f.ProjectID != "" {
		q += ` AND project_id = ?`
		args = append(args, f.ProjectID)
	}
	q += ` ORDER BY created_at DESC, rowid DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Deployment{}
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateStep applies an agent's progress report to one step of a deployment on deviceID.
func (s *Store) UpdateStep(ctx context.Context, deviceID, deploymentID string, idx int, u api.StepUpdate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM deployments WHERE id = ? AND device_id = ?`, deploymentID, deviceID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if status == DeploymentSucceeded || status == DeploymentFailed {
		return nil // late report for a finished deployment
	}
	now := time.Now().UnixMilli()
	if status == DeploymentQueued {
		if _, err := tx.ExecContext(ctx, `UPDATE deployments SET status = ?, started_at = ? WHERE id = ?`,
			DeploymentRunning, now, deploymentID); err != nil {
			return err
		}
	}

	set := `log = log || ?`
	args := []any{formatLines(u.Lines)}
	switch u.Status {
	case "":
	case api.StepRunning:
		set += `, status = ?, started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END`
		args = append(args, u.Status, now)
	case api.StepSucceeded, api.StepFailed, api.StepSkipped:
		set += `, status = ?, finished_at = ?, started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END`
		args = append(args, u.Status, now, now)
	default:
		return errors.New("invalid step status")
	}
	args = append(args, deploymentID, idx)
	res, err := tx.ExecContext(ctx, `UPDATE deployment_steps SET `+set+` WHERE deployment_id = ? AND idx = ?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// FinishDeployment marks a deployment done. Steps still running fail (or succeed, on
// success) and steps never reached are skipped.
func (s *Store) FinishDeployment(ctx context.Context, id string, ok bool, errMsg string) error {
	status, open := DeploymentSucceeded, api.StepSucceeded
	if !ok {
		status, open = DeploymentFailed, api.StepFailed
	}
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE deployments SET status = ?, error = ?, finished_at = ?,
		 started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END WHERE id = ?`,
		status, errMsg, now, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE deployment_steps SET status = ?, finished_at = ? WHERE deployment_id = ? AND status = ?`,
		open, now, id, api.StepRunning); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE deployment_steps SET status = ? WHERE deployment_id = ? AND status = ?`,
		api.StepSkipped, id, api.StepPending); err != nil {
		return err
	}
	return tx.Commit()
}

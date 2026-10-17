package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// User roles, from least to most privileged.
const (
	RoleViewer   = "viewer"   // read-only
	RoleOperator = "operator" // deploy, restart, back up, manage domains
	RoleAdmin    = "admin"    // everything, including users, settings and updates
)

// RoleRank orders roles; unknown roles rank 0 and can do nothing.
func RoleRank(r string) int {
	switch r {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

// User is a dashboard account.
type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	Role         string    `json:"role"`
	Disabled     bool      `json:"disabled"`
	CreatedAt    time.Time `json:"created_at"`
	PasswordHash string    `json:"-"`
}

const userCols = `id, username, role, disabled, created_at, password_hash`

func scanUser(r interface{ Scan(...any) error }) (*User, error) {
	var u User
	var dis, created int64
	if err := r.Scan(&u.ID, &u.Username, &u.Role, &dis, &created, &u.PasswordHash); err != nil {
		return nil, err
	}
	u.Disabled = dis != 0
	u.CreatedAt = time.Unix(created, 0).UTC()
	return &u, nil
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *Store) CreateUser(ctx context.Context, username, hash, role string) (*User, error) {
	u := &User{ID: newID("usr_"), Username: username, Role: role, PasswordHash: hash, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(id, username, role, disabled, created_at, password_hash) VALUES(?, ?, ?, 0, ?, ?)`,
		u.ID, u.Username, u.Role, u.CreatedAt.Unix(), u.PasswordHash)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return nil, ErrConflict
	}
	return u, err
}

func (s *Store) GetUser(ctx context.Context, id string) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) UserByName(ctx context.Context, name string) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE username = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUser saves role, disabled and password hash. Disabling a user ends their sessions.
func (s *Store) UpdateUser(ctx context.Context, u *User) error {
	dis := 0
	if u.Disabled {
		dis = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE users SET role = ?, disabled = ?, password_hash = ? WHERE id = ?`, u.Role, dis, u.PasswordHash, u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if u.Disabled {
		return s.DeleteUserSessions(ctx, u.ID)
	}
	return nil
}

func (s *Store) DeleteUser(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountEnabledAdmins counts active administrators.
func (s *Store) CountEnabledAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

// --- sessions ---

// CreateSession stores a session for a token hash.
func (s *Store) CreateSession(ctx context.Context, tokenHash, userID string, ttl time.Duration) error {
	now := time.Now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(token_hash, user_id, created_at, expires_at) VALUES(?, ?, ?, ?)`,
		tokenHash, userID, now.Unix(), now.Add(ttl).Unix())
	return err
}

// SessionUser returns the enabled user a live session belongs to.
func (s *Store) SessionUser(ctx context.Context, tokenHash string) (*User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT u.id, u.username, u.role, u.disabled, u.created_at, u.password_hash
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ? AND s.expires_at > ? AND u.disabled = 0`, tokenHash, time.Now().Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// PurgeSessions removes expired sessions.
func (s *Store) PurgeSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, time.Now().Unix())
	return err
}

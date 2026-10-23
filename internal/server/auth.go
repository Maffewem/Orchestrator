package server

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

const (
	sessionTTL    = 14 * 24 * time.Hour
	minPassword   = 10
	maxFailures   = 5
	lockoutPeriod = time.Minute
)

// pbkdf2Iterations is a variable so tests can use a cheap setting.
var pbkdf2Iterations = 600_000

var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,31}$`)

// hashPassword returns "pbkdf2-sha256$iterations$salt$hash".
func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iterations, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(dk)), nil
}

func checkPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iters, err := strconv.Atoi(parts[1])
	if err != nil || iters < 1 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iters, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash makes a login for an unknown user cost as much as a real one.
var dummyHash, _ = hashPassword("not-a-real-password")

func (s *Server) validatePassword(ctx context.Context, pw string) error {
	if n := s.minPasswordLen(ctx); len(pw) < n {
		return fmt.Errorf("the password must be at least %d characters", n)
	}
	return nil
}

// identity is who a request is acting as.
type identity struct {
	Name string `json:"username"`
	Role string `json:"role"`
	// Token is true for the legacy admin token, which always acts as an administrator.
	Token bool `json:"token_login"`
}

const identityKey ctxKey = 100

func identityFrom(r *http.Request) *identity {
	id, _ := r.Context().Value(identityKey).(*identity)
	return id
}

// identify resolves a bearer token to an identity: the admin token or a login session.
func (s *Server) identify(ctx context.Context, tok string) *identity {
	if tok == "" {
		return nil
	}
	h := store.HashToken(tok)
	if subtle.ConstantTimeCompare([]byte(h), []byte(s.adminHash)) == 1 {
		return &identity{Name: "admin token", Role: store.RoleAdmin, Token: true}
	}
	if u, err := s.store.SessionUser(ctx, h); err == nil {
		return &identity{Name: u.Username, Role: u.Role}
	}
	return nil
}

// routeRole is the least privileged role allowed to call a route. GET routes default to
// viewer, everything else to admin, unless listed here.
func routeRole(pattern string) string {
	operator := map[string]bool{
		"POST /api/devices/{id}/commands":                    true,
		"POST /api/devices/{id}/containers/{cid}/backup":     true,
		"PUT /api/devices/{id}/workers/{name}/backup-policy": true,
		"POST /api/backups/{id}/restore":                     true,
		"DELETE /api/backups/{id}":                           true,
		"GET /api/backups/{id}/download":                     true,
		"POST /api/projects":                                 true,
		"PATCH /api/projects/{id}":                           true,
		"POST /api/projects/{id}/deployments":                true,
		"POST /api/deployments/{id}/retry":                   true,
		"PUT /api/projects/{id}/domains/urls":                true,
		"POST /api/projects/{id}/domains":                    true,
		"POST /api/deployments/{id}/rollback":                true,
		"POST /api/devices/{id}/streams":                     true,
		"GET /api/streams/{id}/output":                       true,
		"POST /api/streams/{id}/input":                       true,
		"DELETE /api/streams/{id}":                           true,
		"PUT /api/projects/{id}/previews/settings":           true,
		"DELETE /api/projects/{id}/previews/{pid}":           true,
		"DELETE /api/projects/{id}/domains/{did}":            true,
	}
	adminGets := map[string]bool{
		"GET /api/settings":                       true,
		"GET /api/users":                          true,
		"GET /api/system/update":                  true,
		"GET /api/github/repos":                   true,
		"GET /api/server-backups":                 true,
		"GET /api/server-backups/{name}/download": true,
	}
	switch {
	case pattern == "POST /api/auth/logout" || pattern == "POST /api/auth/password":
		return store.RoleViewer
	case operator[pattern]:
		return store.RoleOperator
	case strings.HasPrefix(pattern, "GET ") && !adminGets[pattern]:
		return store.RoleViewer
	}
	return store.RoleAdmin
}

// authed requires at least the role the route is registered for.
func (s *Server) authed(pattern string, h http.HandlerFunc) http.Handler {
	need := routeRole(pattern)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := s.identify(r.Context(), bearer(r))
		if id == nil {
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		if store.RoleRank(id.Role) < store.RoleRank(need) {
			writeError(w, http.StatusForbidden, fmt.Sprintf("your role (%s) can't do this; it needs %s", id.Role, need))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), identityKey, id)))
	})
}

// --- login throttling ---

type limiter struct {
	mu    sync.Mutex
	fails map[string]*failure
}

type failure struct {
	n     int
	until time.Time
}

func (l *limiter) locked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := l.fails[key]
	return f != nil && time.Now().Before(f.until)
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails == nil {
		l.fails = map[string]*failure{}
	}
	f := l.fails[key]
	if f == nil {
		f = &failure{}
		l.fails[key] = f
	}
	f.n++
	if f.n >= maxFailures {
		f.n = 0
		f.until = time.Now().Add(lockoutPeriod)
	}
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.fails, key)
	l.mu.Unlock()
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// verifyLogin checks credentials with throttling. Scopes keep visitors of a protected worker
// (who all arrive from the device's address) from locking people out of the dashboard. It returns the user or an error message.
func (s *Server) verifyLogin(ctx context.Context, r *http.Request, scope, name, pw string) (*store.User, string) {
	key := scope + "|" + strings.ToLower(name) + "|" + clientIP(r)
	if s.logins.locked(key) {
		return nil, "too many failed attempts; try again in a minute"
	}
	u, err := s.store.UserByName(ctx, strings.ToLower(strings.TrimSpace(name)))
	hash := dummyHash
	if err == nil {
		hash = u.PasswordHash
	}
	ok := checkPassword(pw, hash)
	if err != nil || !ok || u.Disabled {
		s.logins.fail(key)
		return nil, "wrong username or password"
	}
	s.logins.reset(key)
	return u, ""
}

func (s *Server) newSession(ctx context.Context, u *store.User) (string, error) {
	tok := store.NewToken()
	if err := s.store.CreateSession(ctx, store.HashToken(tok), u.ID, s.sessionLifetime(ctx)); err != nil {
		return "", err
	}
	return tok, nil
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	n, err := s.store.CountUsers(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"setup_required": n == 0})
}

type loginResponse struct {
	Token string `json:"token"`
	*identity
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	u, msg := s.verifyLogin(r.Context(), r, "login", req.Username, req.Password)
	if u == nil {
		writeError(w, http.StatusUnauthorized, msg)
		return
	}
	tok, err := s.newSession(r.Context(), u)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "info", "auth.login", u.Username+" signed in")
	writeJSON(w, http.StatusOK, loginResponse{Token: tok, identity: &identity{Name: u.Username, Role: u.Role}})
}

// handleSetup creates the first administrator. It needs the admin token, and works only
// while there are no accounts.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AdminToken string `json:"admin_token"`
		Username   string `json:"username"`
		Password   string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	key := "setup|" + clientIP(r)
	if s.logins.locked(key) {
		writeError(w, http.StatusTooManyRequests, "too many failed attempts; try again in a minute")
		return
	}
	if n, err := s.store.CountUsers(r.Context()); err != nil {
		s.internal(w, err)
		return
	} else if n > 0 {
		writeError(w, http.StatusConflict, "accounts already exist; sign in instead")
		return
	}
	if subtle.ConstantTimeCompare([]byte(store.HashToken(strings.TrimSpace(req.AdminToken))), []byte(s.adminHash)) != 1 {
		s.logins.fail(key)
		writeError(w, http.StatusUnauthorized, "that admin token was not accepted")
		return
	}
	u, status, msg := s.createUser(r.Context(), req.Username, req.Password, store.RoleAdmin)
	if u == nil {
		writeError(w, status, msg)
		return
	}
	tok, err := s.newSession(r.Context(), u)
	if err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "info", "auth.setup", "First administrator "+u.Username+" created")
	writeJSON(w, http.StatusCreated, loginResponse{Token: tok, identity: &identity{Name: u.Username, Role: u.Role}})
}

func (s *Server) createUser(ctx context.Context, username, pw, role string) (*store.User, int, string) {
	name := strings.ToLower(strings.TrimSpace(username))
	if !usernameRE.MatchString(name) {
		return nil, http.StatusBadRequest, "usernames are 3 to 32 characters: letters, digits, dot, dash or underscore"
	}
	if store.RoleRank(role) == 0 {
		return nil, http.StatusBadRequest, "role must be viewer, operator or admin"
	}
	if err := s.validatePassword(ctx, pw); err != nil {
		return nil, http.StatusBadRequest, err.Error()
	}
	hash, err := hashPassword(pw)
	if err != nil {
		return nil, http.StatusInternalServerError, "internal error"
	}
	u, err := s.store.CreateUser(ctx, name, hash, role)
	if errors.Is(err, store.ErrConflict) {
		return nil, http.StatusConflict, "that username is taken"
	} else if err != nil {
		return nil, http.StatusInternalServerError, "internal error"
	}
	return u, 0, ""
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, identityFrom(r))
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.store.DeleteSession(r.Context(), store.HashToken(bearer(r)))
	w.WriteHeader(http.StatusNoContent)
}

// --- user management (admin) ---

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	us, err := s.store.ListUsers(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	if us == nil {
		us = []*store.User{}
	}
	writeJSON(w, http.StatusOK, us)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !decode(w, r, &req) {
		return
	}
	u, status, msg := s.createUser(r.Context(), req.Username, req.Password, req.Role)
	if u == nil {
		writeError(w, status, msg)
		return
	}
	s.event(r.Context(), "", "info", "user.created", fmt.Sprintf("%s created user %s (%s)", identityFrom(r).Name, u.Username, u.Role))
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.GetUser(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such user")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	var req struct {
		Role     *string `json:"role"`
		Password *string `json:"password"`
		Disabled *bool   `json:"disabled"`
	}
	if !decode(w, r, &req) {
		return
	}
	wasAdmin := u.Role == store.RoleAdmin && !u.Disabled
	if req.Role != nil {
		if store.RoleRank(*req.Role) == 0 {
			writeError(w, http.StatusBadRequest, "role must be viewer, operator or admin")
			return
		}
		u.Role = *req.Role
	}
	if req.Disabled != nil {
		u.Disabled = *req.Disabled
	}
	if req.Password != nil {
		if err := s.validatePassword(r.Context(), *req.Password); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if u.PasswordHash, err = hashPassword(*req.Password); err != nil {
			s.internal(w, err)
			return
		}
	}
	if wasAdmin && (u.Role != store.RoleAdmin || u.Disabled) {
		if n, _ := s.store.CountEnabledAdmins(r.Context()); n <= 1 {
			writeError(w, http.StatusConflict, "this is the last administrator; promote someone else first")
			return
		}
	}
	if err := s.store.UpdateUser(r.Context(), u); err != nil {
		s.internal(w, err)
		return
	}
	if req.Password != nil || req.Role != nil {
		s.store.DeleteUserSessions(r.Context(), u.ID) // a changed password or role means signing in again
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.GetUser(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such user")
		return
	} else if err != nil {
		s.internal(w, err)
		return
	}
	if u.Role == store.RoleAdmin && !u.Disabled {
		if n, _ := s.store.CountEnabledAdmins(r.Context()); n <= 1 {
			writeError(w, http.StatusConflict, "this is the last administrator")
			return
		}
	}
	if err := s.store.DeleteUser(r.Context(), u.ID); err != nil {
		s.internal(w, err)
		return
	}
	s.event(r.Context(), "", "warning", "user.deleted", fmt.Sprintf("%s deleted user %s", identityFrom(r).Name, u.Username))
	w.WriteHeader(http.StatusNoContent)
}

// handleGateVerify is called by a device's Caddy (forward_auth) for workers that require a
// login. It accepts HTTP Basic credentials of any enabled dashboard user.
func (s *Server) handleGateVerify(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetProject(r.Context(), r.URL.Query().Get("project"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such project")
		return
	}
	if !s.projectProtected(r.Context(), p.ID) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	name, pw, ok := r.BasicAuth()
	if ok {
		if u, msg := s.verifyLogin(r.Context(), r, "gate", name, pw); u != nil {
			w.Header().Set("X-Orchestrator-User", u.Username)
			w.WriteHeader(http.StatusNoContent)
			return
		} else if strings.Contains(msg, "too many") {
			writeError(w, http.StatusTooManyRequests, msg)
			return
		}
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="`+p.Name+`", charset="UTF-8"`)
	writeError(w, http.StatusUnauthorized, "sign in with your Orchestrator account")
}

// handleChangePassword lets a signed-in user change their own password.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r)
	if id.Token {
		writeError(w, http.StatusBadRequest, "the admin token has no password; sign in with an account")
		return
	}
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decode(w, r, &req) {
		return
	}
	u, msg := s.verifyLogin(r.Context(), r, "login", id.Name, req.Current)
	if u == nil {
		writeError(w, http.StatusForbidden, "the current password is wrong")
		_ = msg
		return
	}
	if err := s.validatePassword(r.Context(), req.New); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := hashPassword(req.New)
	if err != nil {
		s.internal(w, err)
		return
	}
	u.PasswordHash = hash
	if err := s.store.UpdateUser(r.Context(), u); err != nil {
		s.internal(w, err)
		return
	}
	// Other sessions end; this one carries on.
	keep := store.HashToken(bearer(r))
	s.store.DeleteUserSessions(r.Context(), u.ID)
	s.store.CreateSession(r.Context(), keep, u.ID, s.sessionLifetime(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

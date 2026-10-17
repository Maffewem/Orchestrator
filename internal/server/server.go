// Package server is the Orchestrator control plane: the agent API, the admin API and the dashboard.
package server

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

const (
	settingJoinToken = "join_token"
	settingAdminHash = "admin_token_hash"
)

type Config struct {
	// HeartbeatInterval is how often agents are asked to check in.
	HeartbeatInterval time.Duration
	// AdminToken, when set, overrides the generated admin token.
	AdminToken string
	// UI is the built dashboard (index.html at its root). May be nil.
	UI      fs.FS
	Version string
	Logger  *slog.Logger
	// GitHubAPI is the GitHub REST base URL; tests point it at a fake.
	GitHubAPI string
	// DownloadsDir holds agent binaries (orchestrator-agent-linux-<arch>) for /install.sh.
	DownloadsDir string
	// PollInterval is how often GitHub projects are checked for new commits. Zero uses
	// the default of 60s; negative disables polling.
	PollInterval time.Duration
}

type Server struct {
	store     *store.Store
	cfg       Config
	log       *slog.Logger
	adminHash string
	githubAPI string
	metrics   *metricsHistory

	pollMu sync.Mutex
	polled map[string]string // project ID -> last commit SHA the poller acted on
}

// New prepares the server. It returns a freshly generated admin token when one had to be
// created, so the caller can show it once; otherwise the returned token is empty.
func New(ctx context.Context, st *store.Store, cfg Config) (*Server, string, error) {
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 5 * time.Second
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 60 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.GitHubAPI == "" {
		cfg.GitHubAPI = "https://api.github.com"
	}
	s := &Server{store: st, cfg: cfg, log: cfg.Logger, githubAPI: cfg.GitHubAPI, metrics: newMetricsHistory(720), polled: map[string]string{}}

	for _, key := range []string{settingJoinToken, settingWebhookSecret} {
		if _, err := st.GetSetting(ctx, key); errors.Is(err, store.ErrNotFound) {
			if err := st.SetSetting(ctx, key, store.NewToken()); err != nil {
				return nil, "", err
			}
		} else if err != nil {
			return nil, "", err
		}
	}

	var generated string
	switch {
	case cfg.AdminToken != "":
		s.adminHash = store.HashToken(cfg.AdminToken)
	default:
		h, err := st.GetSetting(ctx, settingAdminHash)
		if errors.Is(err, store.ErrNotFound) {
			generated = store.NewToken()
			h = store.HashToken(generated)
			if err := st.SetSetting(ctx, settingAdminHash, h); err != nil {
				return nil, "", err
			}
		} else if err != nil {
			return nil, "", err
		}
		s.adminHash = h
	}
	return s, generated, nil
}

// offlineAfter is how long a device may stay silent before it is reported offline.
func (s *Server) offlineAfter() time.Duration { return 3 * s.cfg.HeartbeatInterval }

// Run performs background housekeeping until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	go s.pollLoop(ctx)
	t := time.NewTicker(s.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

func (s *Server) sweep(ctx context.Context) {
	gone, err := s.store.MarkOffline(ctx, time.Now().Add(-s.offlineAfter()))
	if err != nil {
		s.log.Error("mark offline", "err", err)
	}
	for _, d := range gone {
		s.event(ctx, d.ID, "warning", "device.offline", d.Name+" stopped responding")
	}
	if _, err := s.store.ExpireCommands(ctx, time.Now().Add(-2*time.Minute), time.Now().Add(-30*time.Minute)); err != nil {
		s.log.Error("expire commands", "err", err)
	}
}

func (s *Server) event(ctx context.Context, deviceID, level, kind, msg string) {
	if err := s.store.AddEvent(ctx, deviceID, level, kind, msg); err != nil {
		s.log.Error("add event", "err", err)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.cfg.Version})
	})

	// Agent API: register with the join token, then authenticate with the per-device token.
	mux.HandleFunc("POST /api/agent/register", s.handleRegister)
	mux.Handle("POST /api/agent/heartbeat", s.agentAuth(s.handleHeartbeat))
	mux.Handle("POST /api/agent/commands/{id}/result", s.agentAuth(s.handleCommandResult))
	mux.Handle("POST /api/agent/deployments/{id}/steps/{idx}", s.agentAuth(s.handleStepUpdate))

	// GitHub push webhooks, authenticated by their HMAC signature.
	mux.HandleFunc("POST /api/github/webhook", s.handleGitHubWebhook)

	// Admin API used by the dashboard.
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.adminAuth(h)) }
	admin("GET /api/auth/check", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	admin("GET /api/overview", s.handleOverview)
	admin("GET /api/devices", s.handleListDevices)
	admin("GET /api/devices/{id}", s.handleGetDevice)
	admin("GET /api/devices/{id}/metrics", s.handleDeviceMetrics)
	admin("PATCH /api/devices/{id}", s.handleUpdateDevice)
	admin("DELETE /api/devices/{id}", s.handleDeleteDevice)
	admin("POST /api/devices/{id}/commands", s.handleCreateCommand)
	admin("GET /api/containers", s.handleListContainers)
	admin("GET /api/commands/{id}", s.handleGetCommand)
	admin("GET /api/events", s.handleListEvents)
	admin("GET /api/projects", s.handleListProjects)
	admin("POST /api/projects", s.handleCreateProject)
	admin("GET /api/projects/{id}", s.handleGetProject)
	admin("PATCH /api/projects/{id}", s.handleUpdateProject)
	admin("DELETE /api/projects/{id}", s.handleDeleteProject)
	admin("POST /api/projects/{id}/deployments", s.handleDeployProject)
	admin("GET /api/projects/{id}/deployments", s.handleListDeployments)
	admin("GET /api/deployments", s.handleListDeployments)
	admin("GET /api/deployments/{id}", s.handleGetDeployment)
	admin("POST /api/deployments/{id}/retry", s.handleRetryDeployment)
	admin("GET /api/github/repos", s.handleListGitHubRepos)
	admin("GET /api/settings", s.handleGetSettings)
	admin("PUT /api/settings/github", s.handleSetGitHub)
	admin("POST /api/settings/join-token/rotate", s.handleRotateJoinToken)

	mux.HandleFunc("GET /install.sh", s.handleInstallScript)
	if s.cfg.DownloadsDir != "" {
		mux.Handle("GET /downloads/", http.StripPrefix("/downloads/", http.FileServer(http.Dir(s.cfg.DownloadsDir))))
	}

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})
	mux.Handle("/", s.uiHandler())
	return mux
}

type ctxKey int

const deviceKey ctxKey = 0

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if t, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

func (s *Server) adminAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" || subtle.ConstantTimeCompare([]byte(store.HashToken(tok)), []byte(s.adminHash)) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid admin token")
			return
		}
		next(w, r)
	})
}

func (s *Server) agentAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "missing agent token")
			return
		}
		d, err := s.store.DeviceByToken(r.Context(), tok)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "unknown agent token")
			return
		} else if err != nil {
			s.internal(w, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), deviceKey, d)))
	})
}

func deviceFrom(r *http.Request) *store.Device { return r.Context().Value(deviceKey).(*store.Device) }

func (s *Server) uiHandler() http.Handler {
	if s.cfg.UI == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "dashboard not built: run `make web`", http.StatusNotFound)
		})
	}
	files := http.FileServerFS(s.cfg.UI)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if f, err := s.cfg.UI.Open(p); err == nil {
				f.Close()
				files.ServeHTTP(w, r)
				return
			}
		}
		// Client-side routes all render the SPA shell.
		index, err := fs.ReadFile(s.cfg.UI, "index.html")
		if err != nil {
			http.Error(w, "dashboard not built: run `make web`", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	})
}

//go:embed install.sh
var installScript string

// handleInstallScript serves the agent installer with this server's URL filled in.
func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	io.WriteString(w, strings.ReplaceAll(installScript, "{{SERVER}}", scheme+"://"+r.Host))
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

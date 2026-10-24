// Package server is the Orchestrator control plane: the agent API, the admin API and the dashboard.
package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/agent"
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
	// BackupDir holds container backup archives.
	BackupDir string
	// DBPath and Restart let a server backup be restored: the database is swapped in at
	// the next start, so the process must be restarted by whatever supervises it.
	DBPath  string
	Restart func()
	// DockerHost is the engine used for self-updates (default: the local socket).
	DockerHost string
}

type Server struct {
	store         *store.Store
	cfg           Config
	log           *slog.Logger
	adminHash     string
	githubAPI     string
	cloudflareAPI string
	metrics       *metricsHistory
	logins        limiter

	docker *agent.Docker // nil when no Docker client could be configured
	up     *updateState

	pollMu sync.Mutex
	// previewMu serialises preview syncs so a webhook and the poller can't race.
	previewMu    sync.Mutex
	streams      *streamHub
	sso          ssoState
	https        httpsState
	ssoGitHubURL string            // overrides https://github.com in tests
	polled       map[string]string // project ID -> last commit SHA the poller acted on
}

// New prepares the server. It returns a freshly generated admin token when one had to be
// created, so the caller can show it once; otherwise the returned token is empty.
func New(ctx context.Context, st *store.Store, cfg Config) (*Server, string, error) {
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 5 * time.Second
	}
	if cfg.BackupDir == "" {
		cfg.BackupDir = filepath.Join(os.TempDir(), "orchestrator-backups")
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
	if cfg.DockerHost == "" {
		cfg.DockerHost = "unix:///var/run/docker.sock"
	}
	s := &Server{store: st, cfg: cfg, log: cfg.Logger, githubAPI: cfg.GitHubAPI, cloudflareAPI: "https://api.cloudflare.com/client/v4", metrics: newMetricsHistory(720), polled: map[string]string{}, up: newUpdateState(), streams: newStreamHub()}
	s.docker, _ = agent.NewDocker(cfg.DockerHost)

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
	go s.updateLoop(ctx)
	go s.backupLoop(ctx)
	s.startHTTPS(ctx)
	go s.afterBoot(ctx)
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
	mux.Handle("POST /api/agent/backups/{id}", s.agentAuth(s.handleUploadBackup))
	mux.Handle("GET /api/agent/backups/{id}", s.agentAuth(s.handleAgentDownloadBackup))
	mux.Handle("GET /api/agent/streams", s.agentAuth(s.handleAgentStreamPoll))
	mux.Handle("POST /api/agent/streams/{id}/output", s.agentAuth(s.handleAgentStreamOutput))
	mux.Handle("GET /api/agent/streams/{id}/input", s.agentAuth(s.handleAgentStreamInput))
	mux.Handle("GET /api/agent/server-backups/{name}", s.agentAuth(s.handleAgentServerBackup))
	mux.Handle("GET /api/agent/update", s.agentAuth(s.handleAgentUpdateInfo))

	// GitHub push webhooks, authenticated by their HMAC signature.
	mux.HandleFunc("POST /api/github/webhook", s.handleGitHubWebhook)

	// Admin API used by the dashboard.
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.authed(pattern, h)) }
	mux.HandleFunc("GET /api/auth/status", s.handleAuthStatus)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/setup", s.handleSetup)
	mux.HandleFunc("GET /api/auth/sso/providers", s.handleSSOProviders)
	mux.HandleFunc("GET /api/auth/sso/{provider}/start", s.handleSSOStart)
	mux.HandleFunc("GET /api/auth/sso/{provider}/callback", s.handleSSOCallback)
	mux.HandleFunc("POST /api/auth/sso/exchange", s.handleSSOExchange)
	mux.HandleFunc("/api/gate/verify", s.handleGateVerify)
	admin("GET /api/auth/me", s.handleMe)
	admin("POST /api/auth/logout", s.handleLogout)
	admin("POST /api/auth/password", s.handleChangePassword)
	admin("GET /api/users", s.handleListUsers)
	admin("POST /api/users", s.handleCreateUser)
	admin("PATCH /api/users/{id}", s.handleUpdateUser)
	admin("DELETE /api/users/{id}", s.handleDeleteUser)
	admin("GET /api/overview", s.handleOverview)
	admin("GET /api/devices", s.handleListDevices)
	admin("GET /api/devices/{id}", s.handleGetDevice)
	admin("GET /api/devices/{id}/metrics", s.handleDeviceMetrics)
	admin("PATCH /api/devices/{id}", s.handleUpdateDevice)
	admin("DELETE /api/devices/{id}", s.handleDeleteDevice)
	admin("POST /api/devices/{id}/commands", s.handleCreateCommand)
	admin("GET /api/containers", s.handleListContainers)
	admin("POST /api/devices/{id}/adopt", s.handleAdopt)
	admin("POST /api/devices/{id}/proxy", s.handleEnableProxy)
	admin("POST /api/devices/{id}/containers/{cid}/backup", s.handleCreateBackup)
	admin("GET /api/system/update", s.handleUpdateStatus)
	admin("PUT /api/system/update/schedule", s.handleSetUpdateSchedule)
	admin("POST /api/system/update/check", s.handleUpdateCheck)
	admin("POST /api/system/update/apply", s.handleUpdateApply)
	admin("POST /api/system/update/agents", s.handleUpdateAgents)
	admin("GET /api/devices/{id}/workers/{name}/backup-policy", s.handleGetBackupPolicy)
	admin("PUT /api/devices/{id}/workers/{name}/backup-policy", s.handleSetBackupPolicy)
	admin("GET /api/backups", s.handleListBackups)
	admin("GET /api/backups/{id}/download", s.handleDownloadBackup)
	admin("DELETE /api/backups/{id}", s.handleDeleteBackup)
	admin("POST /api/backups/{id}/restore", s.handleRestoreBackup)
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
	admin("GET /api/projects/{id}/domains", s.handleGetDomains)
	admin("PUT /api/projects/{id}/domains/urls", s.handleSetURLs)
	admin("POST /api/projects/{id}/domains", s.handleAddDomain)
	admin("DELETE /api/projects/{id}/domains/{did}", s.handleDeleteDomain)
	admin("POST /api/deployments/{id}/rollback", s.handleRollback)
	admin("GET /api/projects/{id}/previews", s.handleListPreviews)
	admin("PUT /api/projects/{id}/previews/settings", s.handleSetPreviewSettings)
	admin("DELETE /api/projects/{id}/previews/{pid}", s.handleDeletePreview)
	admin("POST /api/devices/{id}/streams", s.handleOpenStream)
	admin("GET /api/streams/{id}/output", s.handleStreamOutput)
	admin("POST /api/streams/{id}/input", s.handleStreamInput)
	admin("DELETE /api/streams/{id}", s.handleCloseStream)
	admin("PUT /api/settings/domain", s.handleSetBaseDomain)
	admin("PUT /api/projects/{id}/domains/access", s.handleSetAccess)
	admin("GET /api/settings", s.handleGetSettings)
	admin("PUT /api/settings/github", s.handleSetGitHub)
	admin("PUT /api/settings/cloudflare", s.handleSetCloudflare)
	admin("PUT /api/settings/ssl", s.handleSetSSL)
	admin("PUT /api/settings/security", s.handleSetSecurity)
	admin("PUT /api/settings/sso", s.handleSetSSO)
	admin("PUT /api/settings/https", s.handleSetHTTPS)
	admin("GET /api/server-backups", s.handleListServerBackups)
	admin("POST /api/server-backups", s.handleCreateServerBackup)
	admin("PUT /api/server-backups/destinations", s.handleSetBackupDests)
	admin("POST /api/server-backups/destinations/{id}/test", s.handleTestBackupDest)
	admin("PUT /api/server-backups/policy", s.handleSetServerBackupPolicy)
	admin("POST /api/server-backups/restore", s.handleRestoreServerUpload)
	admin("GET /api/server-backups/{name}/download", s.handleDownloadServerBackup)
	admin("DELETE /api/server-backups/{name}", s.handleDeleteServerBackup)
	admin("POST /api/server-backups/{name}/restore", s.handleRestoreServerBackup)
	admin("PUT /api/settings/backup-password", s.handleSetBackupPassword)
	admin("POST /api/settings/join-token/rotate", s.handleRotateJoinToken)

	mux.HandleFunc("GET /install.sh", s.handleInstallScript)
	if s.cfg.DownloadsDir != "" {
		mux.Handle("GET /downloads/", http.StripPrefix("/downloads/", http.FileServer(http.Dir(s.cfg.DownloadsDir))))
	}

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})
	mux.Handle("/", s.uiHandler())
	return securityHeaders(s.httpsRedirect(mux))
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
				if strings.HasPrefix(p, "assets/") {
					// Vite hashes these names, so a given URL never changes content.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		// A missing asset must 404, not fall back to the HTML shell: a stale index.html asking
		// for a removed hashed file would otherwise get HTML back as its stylesheet or script.
		if strings.HasPrefix(p, "assets/") || path.Ext(p) != "" {
			http.NotFound(w, r)
			return
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

var installHostRE = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?|\[[0-9A-Fa-f:.]+\])(:[0-9]{1,5})?$`)

//go:embed install.sh
var installScript string

// handleInstallScript serves the agent installer with this server's URL filled in.
func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	// The host lands inside a double-quoted shell string, so anything but a plain host[:port]
	// (a forged Host header, say) is refused instead of being run by whoever pipes this to sh.
	if !installHostRE.MatchString(r.Host) {
		writeError(w, http.StatusBadRequest, "invalid Host header")
		return
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

// securityHeaders stops the dashboard being framed (clickjacking) or content-sniffed.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// Responses carry identities, tokens and device data: never cache them.
			h.Set("Cache-Control", "no-store")
		}
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

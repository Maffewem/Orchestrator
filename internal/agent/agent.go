// Package agent is the Orchestrator device agent. It registers with the control plane,
// reports host and container state on every heartbeat, and runs the commands it is given.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Version is set at build time.
var Version = "dev"

type Config struct {
	Server     string
	JoinToken  string
	Name       string
	Labels     []string
	StatePath  string
	DockerHost string
	// HostRoot is where the host filesystem is mounted when the agent runs in a container.
	HostRoot string
	Logger   *slog.Logger
}

type state struct {
	Server     string `json:"server"`
	DeviceID   string `json:"device_id"`
	AgentToken string `json:"agent_token"`
}

type Agent struct {
	cfg      Config
	log      *slog.Logger
	http     *http.Client
	docker   *Docker
	state    state
	interval time.Duration
	work     chan api.Command
	restart  bool // set by an update: exit after reporting

	cancelMu  sync.Mutex
	cancelled map[string]bool // command ids the user gave up on
	running   string          // id of the command being run
	abort     context.CancelFunc

	lastCPUTotal, lastCPUIdle uint64
}

var errUnauthorized = errors.New("control plane rejected the agent token")

func New(cfg Config) (*Agent, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cfg.Server = strings.TrimRight(cfg.Server, "/")
	if cfg.Server == "" {
		return nil, errors.New("server URL is required")
	}
	d, err := NewDocker(cfg.DockerHost)
	if err != nil {
		return nil, err
	}
	return &Agent{
		cfg:      cfg,
		log:      cfg.Logger,
		http:     &http.Client{Timeout: 30 * time.Second},
		docker:   d,
		interval: 5 * time.Second,
		work:     make(chan api.Command, 64),
	}, nil
}

// Run registers if needed and heartbeats until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	a.checkTrial()
	if err := a.ensureRegistered(ctx); err != nil {
		return err
	}
	a.log.Info("agent running", "device", a.state.DeviceID, "server", a.cfg.Server)
	go a.worker(ctx)
	go a.streamLoop(ctx)

	backoff := time.Second
	for {
		err := a.heartbeat(ctx)
		wait := a.interval
		switch {
		case errors.Is(err, errUnauthorized):
			// The device was removed from the fleet; join again if we can.
			a.log.Warn("agent token rejected, re-registering")
			a.state = state{}
			if err := a.ensureRegistered(ctx); err != nil {
				return err
			}
			wait = 0
		case err != nil:
			a.log.Warn("heartbeat failed", "err", err)
			wait = backoff
			backoff = min(backoff*2, time.Minute)
		default:
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

func (a *Agent) ensureRegistered(ctx context.Context) error {
	if b, err := os.ReadFile(a.cfg.StatePath); err == nil {
		var st state
		if json.Unmarshal(b, &st) == nil && st.AgentToken != "" && st.Server == a.cfg.Server {
			a.state = st
			return nil
		}
	}
	if a.cfg.JoinToken == "" {
		return errors.New("not registered yet: a join token is required (see Devices → Add device)")
	}
	req := api.RegisterRequest{JoinToken: a.cfg.JoinToken, Name: a.cfg.Name, Labels: a.cfg.Labels, Host: a.hostInfo()}
	var res api.RegisterResponse
	for {
		err := a.post(ctx, "/api/agent/register", "", req, &res)
		if err == nil {
			break
		}
		if errors.Is(err, errUnauthorized) {
			return errors.New("the control plane rejected the join token")
		}
		a.log.Warn("register failed, retrying", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	a.state = state{Server: a.cfg.Server, DeviceID: res.DeviceID, AgentToken: res.AgentToken}
	if err := os.MkdirAll(filepath.Dir(a.cfg.StatePath), 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(a.state)
	if err := os.WriteFile(a.cfg.StatePath, b, 0o600); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	a.log.Info("registered", "device", res.DeviceID)
	return nil
}

func (a *Agent) heartbeat(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	host := a.hostInfo()
	host.Docker = a.docker.Info(hctx)
	containers, err := a.docker.Containers(hctx)
	if err != nil {
		containers = nil
	}
	req := api.HeartbeatRequest{Host: host, Metrics: a.metrics(), Containers: containers}
	var res api.HeartbeatResponse
	if err := a.post(ctx, "/api/agent/heartbeat", a.state.AgentToken, req, &res); err != nil {
		return err
	}
	a.confirmUpdate()
	if res.IntervalSeconds > 0 {
		a.interval = time.Duration(res.IntervalSeconds) * time.Second
	}
	a.cancelCommands(res.Cancel)
	for _, c := range res.Commands {
		select {
		case a.work <- c:
		default:
			a.report(ctx, c.ID, api.CommandResult{Error: "agent is busy, try again"})
		}
	}
	return nil
}

// worker runs commands one at a time so deployments on a device never interleave.
func (a *Agent) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case c := <-a.work:
			rctx, ok := a.begin(ctx, c.ID)
			if !ok {
				a.log.Info("skipping cancelled command", "id", c.ID)
				continue
			}
			a.log.Info("running command", "id", c.ID, "action", c.Action)
			res := a.execute(rctx, c)
			a.end()
			a.report(ctx, c.ID, res)
			if a.restart {
				// The new binary is in place; systemd (Restart=always) starts it.
				a.log.Info("restarting to finish the update")
				os.Exit(0)
			}
		}
	}
}

func (a *Agent) execute(ctx context.Context, c api.Command) api.CommandResult {
	var err error
	var output string
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	switch c.Action {
	case api.ActionStart:
		err = a.docker.Start(cctx, c.Target)
	case api.ActionStop:
		err = a.docker.Stop(cctx, c.Target)
	case api.ActionRestart:
		err = a.docker.Restart(cctx, c.Target)
	case api.ActionLogs:
		tail, _ := strconv.Atoi(c.Args["tail"])
		if tail <= 0 || tail > 5000 {
			tail = 300
		}
		output, err = a.docker.Logs(cctx, c.Target, tail)
	case api.ActionDeploy:
		if c.Deploy == nil {
			err = errors.New("deploy command without a spec")
			break
		}
		// Builds on a Pi can take a while; deployments get their own, longer budget.
		dctx, dcancel := context.WithTimeout(ctx, 25*time.Minute)
		err = a.deploy(dctx, c.Target, *c.Deploy)
		dcancel()
	case api.ActionAgentUpdate:
		uctx, ucancel := context.WithTimeout(ctx, 10*time.Minute)
		output, a.restart, err = a.selfUpdate(uctx)
		ucancel()
	case api.ActionRemove:
		var id string
		if id, err = a.docker.ContainerByName(cctx, c.Target); err == nil && id != "" {
			if c.Args["volumes"] == "delete" {
				output, err = a.docker.RemoveWithVolumes(cctx, id)
			} else {
				err = a.docker.Remove(cctx, id)
			}
		}
	case api.ActionInspect:
		var spec api.ContainerSpec
		if spec, err = a.docker.InspectSpec(cctx, c.Target); err == nil {
			var b []byte
			b, err = json.Marshal(spec)
			output = string(b)
		}
	case api.ActionFileStore:
		err = a.storeServerBackup(cctx, c.Args["name"], c.Args["dir"], c.Args["keep"])
	case api.ActionStackStart, api.ActionStackStop, api.ActionStackDown:
		sctx, scancel := context.WithTimeout(ctx, 10*time.Minute)
		output, err = a.stackAction(sctx, c.Action, c.Target, c.Args["volumes"])
		scancel()
	case api.ActionTunnelSync:
		err = a.syncTunnel(cctx, c.Args["token"])
	case api.ActionProxySync:
		err = a.syncProxy(cctx, c.Args["caddyfile"], c.Args["cloudflare_token"])
	case api.ActionBackup, api.ActionRestore:
		// Large volumes over a slow link can take a while.
		bctx, bcancel := context.WithTimeout(ctx, 6*time.Hour)
		if c.Action == api.ActionBackup {
			err = a.backup(bctx, c.Target, c.Args["backup_id"])
		} else {
			err = a.restoreBackup(bctx, c.Target, c.Args["backup_id"])
		}
		bcancel()
	default:
		err = fmt.Errorf("unsupported action %q", c.Action)
	}
	if err != nil {
		return api.CommandResult{Output: output, Error: redact(err.Error())}
	}
	return api.CommandResult{OK: true, Output: output}
}

func (a *Agent) report(ctx context.Context, id string, res api.CommandResult) {
	for attempt := 0; attempt < 5; attempt++ {
		err := a.post(ctx, "/api/agent/commands/"+id+"/result", a.state.AgentToken, res, nil)
		if err == nil {
			return
		}
		a.log.Warn("report result failed", "command", id, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
}

func (a *Agent) post(ctx context.Context, path, token string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.Server+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "orchestrator-agent/"+Version)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// begin returns the context a command runs in, or false when it was cancelled before it started.
func (a *Agent) begin(ctx context.Context, id string) (context.Context, bool) {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	if a.cancelled[id] {
		return nil, false
	}
	rctx, cancel := context.WithCancel(ctx)
	a.running, a.abort = id, cancel
	return rctx, true
}

func (a *Agent) end() {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	if a.abort != nil {
		a.abort()
	}
	a.running, a.abort = "", nil
}

// cancelCommands aborts the running command if the control plane lists it as cancelled, and
// remembers the ids so queued commands are skipped.
func (a *Agent) cancelCommands(ids []string) {
	if len(ids) == 0 {
		return
	}
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	if a.cancelled == nil {
		a.cancelled = map[string]bool{}
	}
	for _, id := range ids {
		a.cancelled[id] = true
		if id == a.running && a.abort != nil {
			a.log.Info("cancelling command", "id", id)
			a.abort()
		}
	}
}

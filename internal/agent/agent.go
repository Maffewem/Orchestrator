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
	Server    string
	JoinToken string
	Name      string
	Labels    []string
	// Setup is the ID of the setup plan from the Add device wizard (first registration only).
	Setup      string
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
	// ServerKey is the server's public identity key, used to recognise it at a new address.
	ServerKey string `json:"server_key,omitempty"`
	// Servers are the addresses the server announced; Found is the last one discovery found.
	Servers []string `json:"servers,omitempty"`
	Found   string   `json:"found,omitempty"`
	// Epoch is the highest server epoch seen (see fence.go); a server below it is ignored.
	Epoch int `json:"epoch,omitempty"`
}

type Agent struct {
	digests sync.Map // deployment ID -> image digest of a pulled image, returned as the deploy output
	cfg     Config
	srvMu   sync.Mutex
	servers []string // every known control-plane URL; cfg.Server is the one in use
	// configured are the URLs from ORCH_SERVER; servers adds announced and discovered ones.
	configured   []string
	epochMu      sync.Mutex
	stale        map[string]api.StaleServer // stale servers seen lately, by address
	moved        string                     // address a server in standby sent us to (a server move)
	moveHold     time.Time                  // do not follow a redirect again before this time
	fails        int                        // heartbeats failed in a row
	lastDiscover time.Time
	scanHosts    func() []string // hosts discovery probes; nil means the local networks

	tunMu   sync.Mutex
	tunInfo *api.TunnelInfo // cloudflared details for the heartbeat, refreshed by tunnelInfo
	tunAt   time.Time

	netMu    sync.Mutex
	netCache []api.NetAddr
	netAt    time.Time
	log      *slog.Logger
	http     *http.Client
	docker   *Docker
	state    state
	interval time.Duration
	work     chan api.Command
	restart  bool // set by an update: exit after reporting
	// afterReport runs once the result of the current command has been reported (used by device.power).
	afterReport func()

	cancelMu  sync.Mutex
	cancelled map[string]bool // command ids the user gave up on
	running   string          // id of the command being run
	abort     context.CancelFunc

	lastCPUTotal, lastCPUIdle uint64

	hw hwState // previous disk, network and CPU counters for the device graphs

	stats statsState // per-container samples waiting for the next heartbeat

	leases leaseTable // DHCP addresses held for workers with a dedicated address
}

var errUnauthorized = errors.New("control plane rejected the agent token")

func New(cfg Config) (*Agent, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	// The server may be several comma-separated URLs (e.g. an IP and a hostname); the agent
	// moves to the next one whenever the current one stops answering.
	var urls []string
	for _, u := range strings.Split(cfg.Server, ",") {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			urls = append(urls, u)
		}
	}
	if len(urls) == 0 {
		return nil, errors.New("server URL is required")
	}
	cfg.Server = urls[0]
	d, err := NewDocker(cfg.DockerHost)
	if err != nil {
		return nil, err
	}
	return &Agent{
		cfg:        cfg,
		servers:    urls,
		configured: urls,
		log:        cfg.Logger,
		http:       &http.Client{Timeout: 30 * time.Second},
		docker:     d,
		interval:   5 * time.Second,
		work:       make(chan api.Command, 64),
	}, nil
}

// Run registers if needed and heartbeats until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	a.checkTrial()
	if err := a.ensureRegistered(ctx); err != nil {
		return err
	}
	a.log.Info("agent running", "device", a.state.DeviceID, "server", a.server())
	go a.worker(ctx)
	go a.streamLoop(ctx)
	go a.statsLoop(ctx)
	go a.dedicatedLoop(ctx)

	backoff := time.Second
	for {
		err := a.heartbeat(ctx)
		wait := a.interval
		switch {
		case errors.Is(err, errUnauthorized) && a.movedTo() != "" && a.server() == a.movedTo():
			// A server we were redirected to does not know us. Never re-register against it
			// on the old server's say-so: go back to the address that did.
			a.log.Warn("the server we were moved to rejected this agent; going back")
			a.holdMove()
			a.nextServer()
			wait = backoff
		case errors.Is(err, errUnauthorized):
			// The device was removed from the server; join again if we can.
			a.log.Warn("agent token rejected, re-registering")
			// Drop the saved identity too, or ensureRegistered would load the rejected token again.
			a.state = state{Epoch: a.epoch()} // the epoch outlives the identity
			os.Remove(a.cfg.StatePath)
			if err := a.ensureRegistered(ctx); err != nil {
				return err
			}
			wait = 0
		case err != nil:
			a.log.Warn("heartbeat failed", "err", err)
			wait = backoff
			backoff = min(backoff*2, time.Minute)
			if a.movedTo() != "" && a.server() == a.movedTo() {
				a.holdMove() // the new server is not answering: stay on the old one for a while
			}
			a.nextServer()
			if a.maybeDiscover(ctx) {
				wait, backoff = 0, time.Second
			}
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

func (a *Agent) server() string {
	a.srvMu.Lock()
	defer a.srvMu.Unlock()
	return a.cfg.Server
}

// nextServer switches to the next configured URL, if there is more than one.
func (a *Agent) nextServer() {
	a.srvMu.Lock()
	defer a.srvMu.Unlock()
	if len(a.servers) < 2 {
		return
	}
	for i, u := range a.servers {
		if u == a.cfg.Server {
			a.cfg.Server = a.servers[(i+1)%len(a.servers)]
			a.log.Info("trying the next control plane URL", "server", a.cfg.Server)
			return
		}
	}
}

func (a *Agent) ensureRegistered(ctx context.Context) error {
	if b, err := os.ReadFile(a.cfg.StatePath); err == nil {
		var st state
		if json.Unmarshal(b, &st) == nil && st.AgentToken != "" {
			// The server's address may have changed (new IP, moved to ethernet, new
			// hostname). Keep the identity; if it is a different server the first
			// heartbeat gets a 401 and the agent re-registers with its join token.
			a.state = st
			if st.Server != a.server() {
				a.log.Info("control plane URL changed", "from", st.Server, "to", a.server())
				a.state.Server = a.server()
				a.saveState()
			}
			a.setServers("")
			return nil
		}
	}
	if a.cfg.JoinToken == "" {
		return errors.New("not registered yet: a join token is required (see Devices → Add device)")
	}
	req := api.RegisterRequest{JoinToken: a.cfg.JoinToken, Name: a.cfg.Name, Labels: a.cfg.Labels, Setup: a.cfg.Setup, Host: a.hostInfo()}
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
	a.state = state{Server: a.server(), DeviceID: res.DeviceID, AgentToken: res.AgentToken, ServerKey: res.ServerKey, Epoch: a.epoch()}
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
	req := api.HeartbeatRequest{Host: host, Metrics: a.metrics(), Containers: containers, Stats: a.takeStats(), Epoch: a.epoch(), StaleServers: a.staleServers()}
	var res api.HeartbeatResponse
	if err := a.post(ctx, "/api/agent/heartbeat", a.state.AgentToken, req, &res); err != nil {
		return err
	}
	a.confirmUpdate()
	a.learn(res)
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
			if f := a.afterReport; f != nil {
				a.afterReport = nil
				f()
			}
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
		if err == nil {
			a.restoreDedicated(cctx) // brings the device's access address and route back
		}
	case api.ActionStop:
		// A stopped worker gives up the device's access address too, so the same address can be
		// brought up for it on another device.
		a.releaseHostAccess(cctx, c.Target)
		err = a.docker.Stop(cctx, c.Target)
	case api.ActionUpdate:
		retries, _ := strconv.Atoi(c.Args["retries"])
		mem, _ := strconv.Atoi(c.Args["memory_mb"])
		err = a.docker.Update(cctx, c.Target, c.Args["restart"], retries, mem)
		if err == nil && c.Args["stop"] == "1" {
			a.releaseHostAccess(cctx, c.Target)
			err = a.docker.Stop(cctx, c.Target)
		}
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
		if v, ok := a.digests.LoadAndDelete(c.Target); ok && err == nil {
			output, _ = v.(string)
		}
	case api.ActionAgentUpdate:
		uctx, ucancel := context.WithTimeout(ctx, 10*time.Minute)
		output, a.restart, err = a.selfUpdate(uctx)
		ucancel()
	case api.ActionRemove:
		var id string
		if id, err = a.docker.ContainerByName(cctx, c.Target); err == nil && id != "" {
			parent, ip := a.docker.containerParentOn(cctx, id), a.docker.containerIPOn(cctx, id, macvlanPrefix)
			a.releaseHostAccess(cctx, id) // the device's access address goes with the worker
			if c.Args["volumes"] == "delete" {
				output, err = a.docker.RemoveWithVolumes(cctx, id)
			} else {
				err = a.docker.Remove(cctx, id)
			}
			if err == nil && parent != "" {
				a.dropLease(c.Target)
				dropShimRoute(cctx, parent, ip)
				a.releaseParent(cctx, parent)
			}
		}
	case api.ActionNetworkCheck:
		var d api.DedicatedIP
		d.Mode, d.IP, d.MAC, d.Parent, d.HostIP = c.Args["mode"], c.Args["ip"], c.Args["mac"], c.Args["parent"], c.Args["host_ip"]
		d.VLAN, _ = strconv.Atoi(c.Args["vlan"])
		d.Subnet, d.Gateway = c.Args["subnet"], c.Args["gateway"]
		current := ""
		if id, _ := a.docker.ContainerByName(cctx, c.Args["service"]); id != "" {
			current = a.docker.containerIPOn(cctx, id, macvlanPrefix)
		}
		var b []byte
		if b, err = json.Marshal(a.checkDedicated(cctx, d, current, avoidSet(c.Args["avoid"]))); err == nil {
			output = string(b)
		}
	case api.ActionInspect:
		var spec api.ContainerSpec
		if spec, err = a.docker.InspectSpec(cctx, c.Target); err == nil {
			var b []byte
			b, err = json.Marshal(spec)
			output = string(b)
		}
	case api.ActionServerInstall:
		ictx, icancel := context.WithTimeout(ctx, 20*time.Minute)
		output, err = a.installServer(ictx, c.Args)
		icancel()
	case api.ActionFileStore:
		err = a.storeServerBackup(cctx, c.Args["name"], c.Args["dir"], c.Args["keep"])
	case api.ActionStackStart, api.ActionStackStop, api.ActionStackDown:
		sctx, scancel := context.WithTimeout(ctx, 10*time.Minute)
		output, err = a.stackAction(sctx, c.Action, c.Target, c.Args["volumes"])
		scancel()
	case api.ActionTunnelSync:
		err = a.syncTunnel(cctx, c.Args["token"], c.Args["update"] == "1")
	case api.ActionProxySync:
		err = a.syncProxy(cctx, c.Args["caddyfile"], c.Args["cloudflare_token"])
	case api.ActionDiskList, api.ActionDiskFormat, api.ActionDiskPartition, api.ActionDiskMount, api.ActionDiskUnmount, api.ActionShareMount:
		// Formatting a large disk can take a long time; the other disk actions are quick.
		dctx, dcancel := context.WithTimeout(ctx, 30*time.Minute)
		output, err = a.diskAction(dctx, c)
		dcancel()
	case api.ActionDockerReport, api.ActionDockerPrune:
		gctx, gcancel := context.WithTimeout(ctx, 30*time.Minute)
		output, err = a.dockerGC(gctx, c)
		gcancel()
	case api.ActionDevicePower:
		output, err = a.powerAction(c)
	case api.ActionImageFlash, api.ActionBootGet, api.ActionBootSet:
		ictx, icancel := context.WithTimeout(ctx, 6*time.Hour) // large images over a slow link
		output, err = a.imageAction(ictx, c)
		icancel()
	case api.ActionBackup, api.ActionRestore:
		// Large volumes over a slow link can take a while.
		bctx, bcancel := context.WithTimeout(ctx, 6*time.Hour)
		if c.Action == api.ActionBackup {
			err = a.backup(bctx, c.Target, c.Args["backup_id"], c.Args["exclude"])
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.server()+path, bytes.NewReader(b))
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
	if err := a.fence(resp); err != nil {
		return err
	}
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

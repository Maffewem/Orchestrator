package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Clustering (Beta). Devices already report to one server, so a "cluster" here is the set of
// devices that workers may be rescheduled between. A worker with a failover policy is moved to
// another online device when its device has been offline for the grace period. The move reuses
// the migration engine: the newest backup on the server is restored into the new container, and
// the old copy is stopped when its device comes back (or the worker is moved back).

const (
	clusterConfigKey   = "cluster:config"
	clusterPolicyKey   = "cluster:policy:"
	clusterFailoverKey = "cluster:failover:"

	defaultGrace  = 120
	minGrace      = 30
	failoverRetry = 10 * time.Minute
)

// ClusterConfig is the server-wide switch and timing for automatic failover.
type ClusterConfig struct {
	// Enabled turns automatic failover on. Off by default: nothing moves by itself.
	Enabled bool `json:"enabled"`
	// GraceSeconds is how long a device must stay offline before its workers are moved.
	GraceSeconds int `json:"grace_seconds"`
}

// ClusterPolicy says how one worker may be rescheduled.
type ClusterPolicy struct {
	// Failover lets the worker move to another device when its own goes offline.
	Failover bool `json:"failover"`
	// Allowed restricts the devices it may move to (device ids). Empty means any online device.
	Allowed []string `json:"allowed,omitempty"`
	// FailBack moves the worker back to its home device once that has been online for the grace period.
	FailBack bool `json:"fail_back"`
	// StartEmpty lets a failover go ahead without data when the server has no backup of the worker.
	StartEmpty bool `json:"start_empty"`
}

// failoverRecord remembers a worker that was moved by failover, so the stale copy on the home
// device can be stopped (or the worker moved back) when that device returns.
type failoverRecord struct {
	FromID   string    `json:"from_id"`
	FromName string    `json:"from_name"`
	ToID     string    `json:"to_id"`
	ToName   string    `json:"to_name"`
	At       time.Time `json:"at"`
	WithData bool      `json:"with_data"`
	Home     bool      `json:"home"` // true once the worker is back on its home device
}

type clusterState struct {
	mu      sync.Mutex
	started time.Time
	tried   map[string]time.Time // project id -> last failover attempt
	back    map[string]time.Time // project id -> when its home device was first seen online again
	grace   time.Duration        // test hook; overrides the configured grace period
}

func (s *Server) clusterConfig(ctx context.Context) ClusterConfig {
	c := ClusterConfig{GraceSeconds: defaultGrace}
	if raw, err := s.store.GetSetting(ctx, clusterConfigKey); err == nil {
		json.Unmarshal([]byte(raw), &c)
	}
	if c.GraceSeconds < minGrace {
		c.GraceSeconds = defaultGrace
	}
	return c
}

func (s *Server) clusterPolicy(ctx context.Context, project string) ClusterPolicy {
	var p ClusterPolicy
	if raw, err := s.store.GetSetting(ctx, clusterPolicyKey+project); err == nil {
		json.Unmarshal([]byte(raw), &p)
	}
	return p
}

func (s *Server) failoverRecord(ctx context.Context, project string) *failoverRecord {
	raw, err := s.store.GetSetting(ctx, clusterFailoverKey+project)
	if err != nil || raw == "" {
		return nil
	}
	var r failoverRecord
	if json.Unmarshal([]byte(raw), &r) != nil {
		return nil
	}
	return &r
}

func (s *Server) saveFailoverRecord(ctx context.Context, project string, r *failoverRecord) {
	val := ""
	if r != nil {
		b, _ := json.Marshal(r)
		val = string(b)
	}
	if err := s.store.SetSetting(ctx, clusterFailoverKey+project, val); err != nil {
		s.log.Error("save failover record", "err", err)
	}
}

// pickFailoverTarget chooses the online device a worker should move to: allowed by the policy,
// not in maintenance, able to run it, and the least loaded by number of workers.
func (s *Server) pickFailoverTarget(ctx context.Context, p *store.Project, pol ClusterPolicy, devices []*store.Device, projects []*store.Project) *store.Device {
	allowed := map[string]bool{}
	for _, id := range pol.Allowed {
		allowed[id] = true
	}
	count := map[string]int{}
	for _, q := range projects {
		count[q.DeviceID]++
	}
	var best *store.Device
	for _, d := range devices {
		switch {
		case d.ID == p.DeviceID, d.Offline, d.Maintenance:
			continue
		case len(allowed) > 0 && !allowed[d.ID]:
			continue
		case p.Source.Type == store.SourceCompose && !hasCap(d, api.CapCompose):
			continue
		case p.Source.Type == store.SourceGitHub && !hasCap(d, api.CapPreview):
			continue
		case !hasCap(d, api.CapBackup):
			continue
		}
		if best == nil || count[d.ID] < count[best.ID] || (count[d.ID] == count[best.ID] && d.Name < best.Name) {
			best = d
		}
	}
	return best
}

// clusterSweep runs on every server tick: it fails workers over from devices that stayed offline,
// and tidies up or moves back workers whose home device returned.
func (s *Server) clusterSweep(ctx context.Context) {
	cfg := s.clusterConfig(ctx)
	cs := &s.cluster
	cs.mu.Lock()
	if cs.started.IsZero() {
		cs.started = time.Now()
		cs.tried, cs.back = map[string]time.Time{}, map[string]time.Time{}
	}
	started := cs.started
	if cs.grace != 0 {
		started = time.Time{}.Add(1)
	}
	cs.mu.Unlock()
	if !cfg.Enabled {
		return
	}
	grace := time.Duration(cfg.GraceSeconds) * time.Second
	if cs.grace != 0 {
		grace = cs.grace
	}
	// After a server restart every device looks silent until it has had a chance to heartbeat.
	if time.Since(started) < grace {
		return
	}
	devices, err := s.store.ListDevices(ctx)
	if err != nil {
		return
	}
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return
	}
	byID := map[string]*store.Device{}
	online := 0
	for _, d := range devices {
		byID[d.ID] = d
		if !d.Offline {
			online++
		}
	}
	// With no device online the server itself is probably the one cut off: do nothing.
	if online == 0 {
		return
	}
	for _, p := range projects {
		if p.Source.Type == store.SourceCloudflare {
			continue
		}
		pol := s.clusterPolicy(ctx, p.ID)
		if !pol.Failover {
			continue
		}
		rec := s.failoverRecord(ctx, p.ID)
		cur := byID[p.DeviceID]
		if cur != nil && cur.Offline && !cur.Maintenance && time.Since(cur.LastSeen) >= grace {
			s.tryFailover(ctx, p, pol, cur, devices, projects)
			continue
		}
		if rec != nil && !rec.Home && cur != nil && cur.ID == rec.FromID {
			rec.Home = true
			s.saveFailoverRecord(ctx, p.ID, rec)
		} else if rec != nil && !rec.Home {
			s.handleHomeReturn(ctx, p, pol, rec, byID[rec.FromID], grace)
		}
	}
}

func (s *Server) tryFailover(ctx context.Context, p *store.Project, pol ClusterPolicy, src *store.Device, devices []*store.Device, projects []*store.Project) {
	cs := &s.cluster
	cs.mu.Lock()
	if last, ok := cs.tried[p.ID]; ok && time.Since(last) < failoverRetry {
		cs.mu.Unlock()
		return
	}
	cs.tried[p.ID] = time.Now()
	cs.mu.Unlock()
	if v := s.migrationView(p.ID); v != nil && v.Status == migRunning {
		return
	}
	dst := s.pickFailoverTarget(ctx, p, pol, devices, projects)
	if dst == nil {
		s.event(ctx, src.ID, "error", "cluster.failover", fmt.Sprintf("%s is offline but no other online device can take %s over", src.Name, p.Name))
		return
	}
	hasBackup := false
	if bs, err := s.store.ListBackups(ctx, src.ID); err == nil {
		for _, b := range bs {
			if b.ContainerName == serviceOf(p) && b.Status == store.BackupComplete {
				hasBackup = true
				break
			}
		}
	}
	isStack := p.Source.Type == store.SourceCompose
	withData := hasBackup && !isStack
	if !withData && !isStack && !pol.StartEmpty {
		s.event(ctx, src.ID, "error", "cluster.failover", fmt.Sprintf("%s is offline and %s has no backup on the server, so it was not moved. Allow starting empty in its cluster settings, or back it up", src.Name, p.Name))
		return
	}
	req := migrateRequest{DeviceID: dst.ID, FromBackup: withData}
	if !withData {
		f := false
		req.WithData = &f
	}
	s.event(ctx, dst.ID, "warning", "cluster.failover", fmt.Sprintf("%s has been offline for %s: failing %s over to %s", src.Name, time.Since(src.LastSeen).Round(time.Second), p.Name, dst.Name))
	if _, _, err := s.startMigration(ctx, p, req); err != nil {
		s.event(ctx, dst.ID, "error", "cluster.failover", fmt.Sprintf("Failing %s over to %s failed to start: %s", p.Name, dst.Name, err))
		return
	}
	s.saveFailoverRecord(ctx, p.ID, &failoverRecord{FromID: src.ID, FromName: src.Name, ToID: dst.ID, ToName: dst.Name, At: time.Now().UTC(), WithData: withData})
}

// handleHomeReturn deals with a worker that was failed over once its original device is back:
// either move it home again (when the policy asks for it and the device stayed up for the grace
// period) or stop the stale copy so two instances never run at once.
func (s *Server) handleHomeReturn(ctx context.Context, p *store.Project, pol ClusterPolicy, rec *failoverRecord, home *store.Device, grace time.Duration) {
	cs := &s.cluster
	if home == nil { // the old device was deleted: nothing left to tidy
		s.saveFailoverRecord(ctx, p.ID, nil)
		return
	}
	if home.Offline || home.Maintenance {
		cs.mu.Lock()
		delete(cs.back, p.ID)
		cs.mu.Unlock()
		return
	}
	// A migration must have finished first, so the stale copy isn't stopped mid-move.
	if v := s.migrationView(p.ID); v != nil && v.Status == migRunning {
		return
	}
	name := serviceOf(p)
	if pol.FailBack {
		cs.mu.Lock()
		since, ok := cs.back[p.ID]
		if !ok {
			cs.back[p.ID] = time.Now()
			since = time.Now()
		}
		cs.mu.Unlock()
		if time.Since(since) < grace || p.DeviceID == home.ID {
			return
		}
		t := true
		s.event(ctx, home.ID, "info", "cluster.failback", fmt.Sprintf("%s is back: moving %s home from %s", home.Name, p.Name, rec.ToName))
		if _, _, err := s.startMigration(ctx, p, migrateRequest{DeviceID: home.ID, StopSource: true, WithData: &t}); err != nil {
			s.event(ctx, home.ID, "error", "cluster.failback", fmt.Sprintf("Moving %s back to %s failed to start: %s", p.Name, home.Name, err))
			cs.mu.Lock()
			cs.back[p.ID] = time.Now() // retry after another grace period
			cs.mu.Unlock()
			return
		}
		rec.Home = true
		s.saveFailoverRecord(ctx, p.ID, rec)
		return
	}
	// No fail-back: the worker stays where it is, so the copy left on the home device is stale.
	if c := findByName(home, name); c != nil && hasCap(home, api.CapPreview) && p.DeviceID != home.ID {
		if p.Source.Type == store.SourceCompose {
			s.store.CreateCommand(ctx, home.ID, api.ActionStackStop, p.Name, nil)
		} else if c.State == "running" {
			s.store.CreateCommand(ctx, home.ID, api.ActionStop, c.ID, nil)
		}
		s.event(ctx, home.ID, "info", "cluster.failover", fmt.Sprintf("%s is back. %s now runs on %s, so the old copy here was stopped", home.Name, p.Name, rec.ToName))
	}
	rec.Home = true
	s.saveFailoverRecord(ctx, p.ID, rec)
}

func (s *Server) migrationView(project string) *MigrationView {
	s.mig.mu.Lock()
	defer s.mig.mu.Unlock()
	if v := s.mig.byProject[project]; v != nil {
		c := *v
		return &c
	}
	return nil
}

// --- API ---

type clusterWorker struct {
	ProjectID string          `json:"project_id"`
	Name      string          `json:"name"`
	DeviceID  string          `json:"device_id"`
	Device    string          `json:"device"`
	Policy    ClusterPolicy   `json:"policy"`
	Failover  *failoverRecord `json:"last_failover,omitempty"`
}

type clusterView struct {
	Config  ClusterConfig   `json:"config"`
	Workers []clusterWorker `json:"workers"`
}

func (s *Server) handleGetCluster(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		s.internal(w, err)
		return
	}
	devices, _ := s.store.ListDevices(ctx)
	names := map[string]string{}
	for _, d := range devices {
		names[d.ID] = d.Name
	}
	v := clusterView{Config: s.clusterConfig(ctx), Workers: []clusterWorker{}}
	for _, p := range projects {
		if p.Source.Type == store.SourceCloudflare {
			continue
		}
		v.Workers = append(v.Workers, clusterWorker{ProjectID: p.ID, Name: p.Name, DeviceID: p.DeviceID, Device: names[p.DeviceID], Policy: s.clusterPolicy(ctx, p.ID), Failover: s.failoverRecord(ctx, p.ID)})
	}
	sort.Slice(v.Workers, func(i, j int) bool { return v.Workers[i].Name < v.Workers[j].Name })
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleSetCluster(w http.ResponseWriter, r *http.Request) {
	var c ClusterConfig
	if !decode(w, r, &c) {
		return
	}
	if c.GraceSeconds < minGrace || c.GraceSeconds > 3600 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the wait before failing over must be between %d and 3600 seconds", minGrace))
		return
	}
	b, _ := json.Marshal(c)
	if err := s.store.SetSetting(r.Context(), clusterConfigKey, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleSetClusterPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupDeviceProject(w, r)
	if !ok {
		return
	}
	var pol ClusterPolicy
	if !decode(w, r, &pol) {
		return
	}
	for i, id := range pol.Allowed {
		pol.Allowed[i] = strings.TrimSpace(id)
		if _, err := s.store.GetDevice(r.Context(), pol.Allowed[i]); err != nil {
			writeError(w, http.StatusBadRequest, "an allowed device no longer exists")
			return
		}
	}
	if pol.Failover && p.Source.Type == store.SourceCompose && len(pol.Allowed) == 1 && pol.Allowed[0] == p.DeviceID {
		writeError(w, http.StatusBadRequest, "choose at least one device other than the one it runs on")
		return
	}
	b, _ := json.Marshal(pol)
	if err := s.store.SetSetting(r.Context(), clusterPolicyKey+p.ID, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

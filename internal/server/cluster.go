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

	"github.com/Maffewem/Orchestrator/internal/agent"
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
	// Replicas are extra devices that run their own independent copy of the worker, deployed
	// together with the main one. Nothing is synced or moved: the app must cope with several copies.
	Replicas []string `json:"replicas,omitempty"`
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
		case len(portConflictsOn(p, d)) > 0:
			continue // its published ports are taken on that device
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
		s.reconcileReplicas(ctx, p, pol, byID)
		if !cfg.Enabled || !pol.Failover {
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
	Replicas  []replicaState  `json:"replicas,omitempty"`
}

type replicaState struct {
	DeviceID    string `json:"device_id"`
	Device      string `json:"device"`
	Online      bool   `json:"online"`
	Maintenance bool   `json:"maintenance,omitempty"` // the device is in maintenance: nothing is placed on it until that ends
	Running     bool   `json:"running"`
	ContainerID string `json:"container_id,omitempty"`
	State       string `json:"state,omitempty"`
	// The newest deployment of the worker on this device, so a replica that never came up shows why.
	DeploymentID     string `json:"deployment_id,omitempty"`
	DeploymentStatus string `json:"deployment_status,omitempty"`
	DeploymentError  string `json:"deployment_error,omitempty"`
	// Conflicts are host ports the worker publishes that another container on this device already holds.
	Conflicts []string `json:"conflicts,omitempty"`
}

// replicaStates describes each replica of a worker: whether its device is up, whether the
// container exists there and the outcome of its latest deployment.
func (s *Server) replicaStates(ctx context.Context, p *store.Project, pol ClusterPolicy) []replicaState {
	var out []replicaState
	for _, id := range pol.Replicas {
		st := replicaState{DeviceID: id}
		if d, err := s.store.GetDevice(ctx, id); err == nil {
			st.Device, st.Online, st.Maintenance = d.Name, !d.Offline, d.Maintenance
			if c := findByName(d, serviceOf(p)); c != nil {
				st.ContainerID, st.State, st.Running = c.ID, c.State, c.State == "running"
			}
			if !st.Running {
				st.Conflicts = portConflictsOn(p, d)
			}
		}
		if deps, err := s.store.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, DeviceID: id, Limit: 1}); err == nil && len(deps) > 0 {
			st.DeploymentID, st.DeploymentStatus, st.DeploymentError = deps[0].ID, string(deps[0].Status), deps[0].Error
		}
		out = append(out, st)
	}
	return out
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
	byID := map[string]*store.Device{}
	for _, d := range devices {
		names[d.ID] = d.Name
		byID[d.ID] = d
	}
	v := clusterView{Config: s.clusterConfig(ctx), Workers: []clusterWorker{}}
	for _, p := range projects {
		if p.Source.Type == store.SourceCloudflare {
			continue
		}
		pol := s.clusterPolicy(ctx, p.ID)
		cw := clusterWorker{ProjectID: p.ID, Name: p.Name, DeviceID: p.DeviceID, Device: names[p.DeviceID], Policy: pol, Failover: s.failoverRecord(ctx, p.ID)}
		cw.Replicas = s.replicaStates(ctx, p, pol)
		v.Workers = append(v.Workers, cw)
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
	var req clusterPolicyRequest
	if !decode(w, r, &req) {
		return
	}
	pol := req.ClusterPolicy
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
	// Warn before placing a copy on a device where its published ports are already taken.
	if r.URL.Query().Get("confirm") != "1" {
		before := map[string]bool{}
		for _, id := range s.clusterPolicy(r.Context(), p.ID).Replicas {
			before[id] = true
		}
		var bad []string
		for _, id := range pol.Replicas {
			if before[id] || id == p.DeviceID {
				continue
			}
			if d, err := s.store.GetDevice(r.Context(), id); err == nil {
				for _, c := range portConflictsOn(p, d) {
					bad = append(bad, d.Name+": "+c)
				}
			}
		}
		if len(bad) > 0 {
			writeError(w, http.StatusConflict, "The copy would not start: "+strings.Join(bad, "; "))
			return
		}
	}
	seen := map[string]bool{}
	for _, id := range pol.Replicas {
		if id == p.DeviceID || seen[id] {
			writeError(w, http.StatusBadRequest, "a replica must be on a different device from the main copy, and each device can be chosen once")
			return
		}
		seen[id] = true
		if _, err := s.store.GetDevice(r.Context(), id); err != nil {
			writeError(w, http.StatusBadRequest, "a replica device no longer exists")
			return
		}
	}
	// Taking a device off the list must say what happens to the copy already running there.
	removed := s.removedReplicas(r.Context(), p, pol)
	if len(removed) > 0 && req.OnRemove != "keep" && req.OnRemove != "stop" && req.OnRemove != "remove" {
		names := []string{}
		for _, d := range removed {
			names = append(names, d.Name)
		}
		writeError(w, http.StatusConflict, "A copy of "+p.Name+" is running on "+strings.Join(names, ", ")+". Choose what happens to it: leave it running, stop it, or remove it")
		return
	}
	b, _ := json.Marshal(pol)
	if err := s.store.SetSetting(r.Context(), clusterPolicyKey+p.ID, string(b)); err != nil {
		s.internal(w, err)
		return
	}
	for _, d := range removed {
		s.retireReplica(r.Context(), p, d, req.OnRemove, req.DeleteVolumes)
	}
	writeJSON(w, http.StatusOK, pol)
}

// replicaTrigger marks deployments that only exist to run an extra copy; they never fan out again.
const replicaTrigger = "replica"

// rollReplicas updates the replicas of a worker one at a time, after the main copy has gone
// live. Each copy is replaced blue/green on its own device, so it keeps serving until its new
// version is up; rolling means they never all swap at the same moment, and a bad release is
// stopped at the first copy that fails instead of reaching every copy. It returns at once.
func (s *Server) rollReplicas(ctx context.Context, p *store.Project, main *store.Deployment, commit *store.Commit, tweak func(*api.DeploySpec)) {
	if p.Source.Type == store.SourceCloudflare {
		return
	}
	pol := s.clusterPolicy(ctx, p.ID)
	if len(pol.Replicas) == 0 {
		return
	}
	cp := *p
	go func() {
		bg := context.WithoutCancel(ctx)
		wait := func(dep *store.Deployment) error {
			return s.waitFor(bg, 45*time.Minute, func() (bool, error) {
				cur, err := s.store.GetDeployment(bg, dep.ID)
				if err != nil {
					return false, err
				}
				if cur.Status == store.DeploymentFailed {
					return false, fmt.Errorf("%s", cur.Error)
				}
				return cur.Status == store.DeploymentSucceeded, nil
			})
		}
		if err := wait(main); err != nil {
			s.event(bg, cp.DeviceID, "warning", "cluster.rolling", fmt.Sprintf("The new version of %s did not go live on its main device (%v), so the replicas were left on the version they run", cp.Name, err))
			return
		}
		for _, id := range pol.Replicas {
			dep := s.deployReplica(bg, &cp, id, commit, tweak)
			if dep == nil {
				continue // offline, in maintenance or refused: the server starts it when it can
			}
			if err := wait(dep); err != nil {
				s.event(bg, id, "error", "cluster.rolling", fmt.Sprintf("The new version of %s failed on a replica (%v). The remaining replicas were not updated, and the replica that failed keeps its previous version", cp.Name, err))
				return
			}
		}
	}()
}

func (s *Server) deployReplica(ctx context.Context, p *store.Project, deviceID string, commit *store.Commit, tweak func(*api.DeploySpec)) *store.Deployment {
	if deviceID == p.DeviceID {
		return nil
	}
	d, err := s.store.GetDevice(ctx, deviceID)
	if err != nil || d.Offline || d.Maintenance {
		return nil
	}
	cp := *p
	cp.DeviceID = deviceID
	inner := tweak
	tweak = func(spec *api.DeploySpec) {
		// The address belongs to the main copy; a replica serves on its own device's ports.
		spec.Dedicated = nil
		if inner != nil {
			inner(spec)
		}
	}
	dep, err := s.deployWith(ctx, &cp, commit, replicaTrigger, tweak)
	if err != nil {
		s.event(ctx, deviceID, "error", "cluster.replica", fmt.Sprintf("Deploying a replica of %s to %s failed: %s", p.Name, d.Name, err))
		return nil
	}
	return dep
}

// reconcileReplicas starts a replica on any chosen device that has none, for example a newly
// added device or one that came back. A device is tried at most every failoverRetry.
func (s *Server) reconcileReplicas(ctx context.Context, p *store.Project, pol ClusterPolicy, byID map[string]*store.Device) {
	for _, id := range pol.Replicas {
		d := byID[id]
		if d == nil || d.Offline || d.Maintenance || d.ID == p.DeviceID || !s.replicaNeedsDeploy(ctx, p, d) {
			continue
		}
		key := p.ID + "@" + id
		s.cluster.mu.Lock()
		last, seen := s.cluster.tried[key]
		if seen && time.Since(last) < failoverRetry {
			s.cluster.mu.Unlock()
			continue
		}
		s.cluster.tried[key] = time.Now()
		s.cluster.mu.Unlock()
		var commit *store.Commit
		if p.Source.FromRepo() {
			var err error
			if commit, err = s.latestCommit(ctx, p.Source.Repo, p.Source.Branch); err != nil {
				continue
			}
		}
		s.deployReplica(ctx, p, id, commit, nil)
	}
}

// portConflictsOn lists the worker's published host ports that another container on d holds.
func portConflictsOn(p *store.Project, d *store.Device) []string {
	if len(p.Runtime.Ports) == 0 || p.Source.Type == store.SourceCompose || p.Runtime.Dedicated != nil {
		return nil // a dedicated address is served on its own IP, so device ports can't clash
	}
	return agent.ContainerPortConflicts(api.DeploySpec{Service: serviceOf(p), Ports: p.Runtime.Ports}, d.Containers)
}

// replicaNeedsDeploy reports whether the server should start a deployment for a replica: only
// when the device has no container and its last attempt is not failed or still going. A failed
// replica stays failed until someone retries it, or the next deployment of the worker repeats it.
func (s *Server) replicaNeedsDeploy(ctx context.Context, p *store.Project, d *store.Device) bool {
	if findByName(d, serviceOf(p)) != nil {
		return false
	}
	deps, err := s.store.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, DeviceID: d.ID, Limit: 1})
	if err != nil || len(deps) == 0 {
		return err == nil
	}
	switch deps[0].Status {
	case store.DeploymentFailed, store.DeploymentQueued, store.DeploymentRunning:
		return false
	}
	return true
}

// handleRetryReplica starts a fresh deployment of a replica, for example once a port conflict is fixed.
func (s *Server) handleRetryReplica(w http.ResponseWriter, r *http.Request) {
	p, ok := s.lookupDeviceProject(w, r)
	if !ok {
		return
	}
	id := r.PathValue("device")
	pol := s.clusterPolicy(r.Context(), p.ID)
	found := false
	for _, x := range pol.Replicas {
		found = found || x == id
	}
	if !found {
		writeError(w, http.StatusNotFound, "that device is not a replica of this worker")
		return
	}
	d, err := s.store.GetDevice(r.Context(), id)
	if err != nil || d.Offline || d.Maintenance {
		writeError(w, http.StatusConflict, "that device is offline or in maintenance")
		return
	}
	var commit *store.Commit
	if p.Source.FromRepo() {
		if commit, err = s.latestCommit(r.Context(), p.Source.Repo, p.Source.Branch); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	cp := *p
	cp.DeviceID = id
	dep, err := s.deployWith(r.Context(), &cp, commit, replicaTrigger, nil)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, dep)
}

// clusterPolicyRequest is a policy plus what to do with copies that the policy no longer lists.
type clusterPolicyRequest struct {
	ClusterPolicy
	// OnRemove is keep (leave the copy running, untracked), stop, or remove, for each replica
	// device taken off the list that still has the copy.
	OnRemove      string `json:"on_remove,omitempty"`
	DeleteVolumes bool   `json:"delete_volumes,omitempty"` // with remove: also delete its volumes
}

// removedReplicas lists the devices that were replicas and no longer are, and still hold the copy.
func (s *Server) removedReplicas(ctx context.Context, p *store.Project, next ClusterPolicy) []*store.Device {
	keep := map[string]bool{}
	for _, id := range next.Replicas {
		keep[id] = true
	}
	var out []*store.Device
	for _, id := range s.clusterPolicy(ctx, p.ID).Replicas {
		if keep[id] || id == p.DeviceID {
			continue
		}
		if d, err := s.store.GetDevice(ctx, id); err == nil && findByName(d, serviceOf(p)) != nil {
			out = append(out, d)
		}
	}
	return out
}

func (s *Server) retireReplica(ctx context.Context, p *store.Project, d *store.Device, how string, volumes bool) {
	c := findByName(d, serviceOf(p))
	if c == nil || how == "keep" {
		if how == "keep" {
			s.event(ctx, d.ID, "warning", "cluster.replica", fmt.Sprintf("%s is no longer a replica of %s but its container was left running on %s", d.Name, p.Name, d.Name))
		}
		return
	}
	switch how {
	case "stop":
		s.store.CreateCommand(ctx, d.ID, api.ActionStop, c.ID, nil)
		s.event(ctx, d.ID, "warning", "cluster.replica", fmt.Sprintf("The replica of %s on %s is being stopped (its container and data are kept)", p.Name, d.Name))
	case "remove":
		var args map[string]string
		if volumes {
			args = map[string]string{"volumes": "delete"}
		}
		s.store.CreateCommand(ctx, d.ID, api.ActionRemove, c.Name, args)
		s.event(ctx, d.ID, "warning", "cluster.replica", fmt.Sprintf("The replica of %s on %s is being removed%s", p.Name, d.Name, map[bool]string{true: " together with its volumes", false: " (its volumes are kept)"}[volumes]))
	}
}

package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

func TestClusterFailover(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.srv.migratePoll = 10 * time.Millisecond
	h.srv.cluster.grace = time.Nanosecond
	caps := api.HostInfo{Capabilities: []string{api.CapBackup, api.CapPreview}}
	oldReg, newReg := h.register("pi-old"), h.register("pi-new")
	h.do("POST", "/api/agent/heartbeat", newReg.AgentToken, api.HeartbeatRequest{Host: caps}, nil)
	var p store.Project
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "web", "device_id": oldReg.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx"}}, &p)

	// Off by default: nothing moves.
	h.st.MarkOffline(ctx, time.Now().Add(time.Hour))
	h.do("POST", "/api/agent/heartbeat", newReg.AgentToken, api.HeartbeatRequest{Host: caps}, nil)
	h.srv.clusterSweep(ctx)
	if got, _ := h.st.GetProject(ctx, p.ID); got.DeviceID != oldReg.DeviceID {
		t.Fatal("moved while clustering was off")
	}

	if code := h.do("PUT", "/api/cluster", adminToken, ClusterConfig{Enabled: true, GraceSeconds: 10}, nil); code != http.StatusBadRequest {
		t.Fatalf("grace too short: %d", code)
	}
	h.do("PUT", "/api/cluster", adminToken, ClusterConfig{Enabled: true, GraceSeconds: 60}, nil)
	// A worker with no policy is left alone.
	h.srv.clusterSweep(ctx)
	if got, _ := h.st.GetProject(ctx, p.ID); got.DeviceID != oldReg.DeviceID {
		t.Fatal("moved without a policy")
	}
	// No backup and not allowed to start empty: it stays put.
	h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, ClusterPolicy{Failover: true}, nil)
	h.srv.clusterSweep(ctx)
	if got, _ := h.st.GetProject(ctx, p.ID); got.DeviceID != oldReg.DeviceID {
		t.Fatal("moved without a backup")
	}
	// Allowed to start empty: it moves to the online device.
	h.srv.cluster.tried = map[string]time.Time{}
	h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, ClusterPolicy{Failover: true, StartEmpty: true, Allowed: []string{newReg.DeviceID}}, nil)
	h.srv.clusterSweep(ctx)
	got, _ := h.st.GetProject(ctx, p.ID)
	for i := 0; i < 200 && got.DeviceID != newReg.DeviceID; i++ {
		time.Sleep(10 * time.Millisecond) // the move runs in the background
		got, _ = h.st.GetProject(ctx, p.ID)
	}
	if got.DeviceID != newReg.DeviceID {
		t.Fatalf("project is on %s, want the standby", got.DeviceID)
	}
	if rec := h.srv.failoverRecord(ctx, p.ID); rec == nil || rec.FromID != oldReg.DeviceID {
		t.Fatalf("failover record: %+v", rec)
	}
	var view clusterView
	if code := h.do("GET", "/api/cluster", adminToken, nil, &view); code != http.StatusOK || len(view.Workers) != 1 || view.Workers[0].Failover == nil {
		t.Fatalf("cluster view: %d %+v", code, view)
	}
	var hist []failoverEntry
	if code := h.do("GET", "/api/cluster/history?project="+p.ID, adminToken, nil, &hist); code != http.StatusOK || len(hist) == 0 {
		t.Fatalf("history: %d %+v", code, hist)
	}
	last := hist[0]
	if last.Kind != kindFailover || last.FromID != oldReg.DeviceID || last.ToID != newReg.DeviceID || last.Worker != "web" {
		t.Fatalf("history entry: %+v", last)
	}
}

func TestFailoverHistoryDedupesRepeatedFailures(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	e := failoverEntry{ProjectID: "p1", Worker: "web", Kind: kindFailover, FromID: "d1", Status: histFailed, Error: "No other online device can take it over."}
	a := h.srv.addHistory(ctx, e)
	b := h.srv.addHistory(ctx, e)
	if a != b || len(h.srv.loadHistory(ctx)) != 1 {
		t.Fatalf("repeated identical failure stored twice: %+v", h.srv.loadHistory(ctx))
	}
	e.Error = "different"
	h.srv.addHistory(ctx, e)
	if len(h.srv.loadHistory(ctx)) != 2 {
		t.Fatal("a different failure should be stored")
	}
	// A running entry with no matching migration is closed once it is stale.
	id := h.srv.addHistory(ctx, failoverEntry{ProjectID: "p2", Worker: "api", Kind: kindFailback, Status: histRunning, At: time.Now().Add(-2 * time.Hour)})
	h.srv.settleHistory(ctx)
	for _, x := range h.srv.loadHistory(ctx) {
		if x.ID == id && (x.Status != histFailed || x.Finished == nil) {
			t.Fatalf("stale entry not closed: %+v", x)
		}
	}
}

func TestFitProblemsAndCapacity(t *testing.T) {
	gb := uint64(1 << 30)
	mk := func(cpus int, memTotal, memUsed uint64) *store.Device {
		return &store.Device{Host: api.HostInfo{CPUs: cpus}, Metrics: api.Metrics{MemoryTotal: memTotal, MemoryUsed: memUsed, DiskTotal: 100 * gb, DiskUsed: 10 * gb}}
	}
	need := workerNeed{Cores: 1, Mem: 2 * gb, Disk: int64(5 * gb)}
	if p := fitProblems(need, mk(4, 8*gb, 1*gb), workerNeed{}); len(p) != 0 {
		t.Fatalf("roomy device should fit: %v", p)
	}
	if p := fitProblems(need, mk(4, 2*gb, 1*gb), workerNeed{}); len(p) != 1 {
		t.Fatalf("small memory should be the only problem: %v", p)
	}
	// A device already promised to another worker has less room.
	if p := fitProblems(need, mk(4, 8*gb, 1*gb), workerNeed{Mem: 5 * gb}); len(p) != 1 {
		t.Fatalf("reserved memory ignored: %v", p)
	}
	// Unknown needs and unreported devices never block.
	if p := fitProblems(workerNeed{}, mk(0, 0, 0), workerNeed{}); len(p) != 0 {
		t.Fatalf("unknown need blocked: %v", p)
	}
}

func TestCapacityReportStrandsWorkerWhenNoOtherDevice(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	caps := api.HostInfo{Capabilities: []string{api.CapBackup, api.CapPreview}}
	a, b := h.register("pi-a"), h.register("pi-b")
	h.do("POST", "/api/agent/heartbeat", a.AgentToken, api.HeartbeatRequest{Host: caps}, nil)
	h.do("POST", "/api/agent/heartbeat", b.AgentToken, api.HeartbeatRequest{Host: caps}, nil)
	var p store.Project
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "web", "device_id": a.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx"}}, &p)
	h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, ClusterPolicy{Failover: true, StartEmpty: true, Allowed: []string{a.DeviceID}}, nil)
	var view clusterView
	h.do("GET", "/api/cluster", adminToken, nil, &view)
	if !view.Capacity.Enabled || view.Capacity.SurvivesOne {
		t.Fatalf("a worker allowed nowhere else should not survive losing its device: %+v", view.Capacity)
	}
	h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, ClusterPolicy{Failover: true, StartEmpty: true}, nil)
	h.do("GET", "/api/cluster", adminToken, nil, &view)
	if !view.Capacity.SurvivesOne {
		t.Fatalf("with a second device it should survive: %+v", view.Capacity)
	}
	_ = ctx
}

func TestClusterNoTargetAndAllOffline(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.srv.cluster.grace = time.Nanosecond
	reg := h.register("only")
	var p store.Project
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "web", "device_id": reg.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx"}}, &p)
	h.do("PUT", "/api/cluster", adminToken, ClusterConfig{Enabled: true, GraceSeconds: 60}, nil)
	h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, ClusterPolicy{Failover: true, StartEmpty: true}, nil)
	h.st.MarkOffline(ctx, time.Now().Add(time.Hour))
	h.srv.clusterSweep(ctx) // every device offline: the server may be the one cut off
	if got, _ := h.st.GetProject(ctx, p.ID); got.DeviceID != reg.DeviceID {
		t.Fatal("moved with nothing online")
	}
}

func TestClusterReplicas(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.srv.cluster.grace = time.Hour // failover stays out of the way
	caps := api.HostInfo{Capabilities: []string{api.CapBackup, api.CapPreview}}
	a, b := h.register("pi-a"), h.register("pi-b")
	h.do("POST", "/api/agent/heartbeat", a.AgentToken, api.HeartbeatRequest{Host: caps}, nil)
	h.do("POST", "/api/agent/heartbeat", b.AgentToken, api.HeartbeatRequest{Host: caps}, nil)
	var p store.Project
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "web", "device_id": a.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx"}}, &p)
	if code := h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, ClusterPolicy{Replicas: []string{a.DeviceID}}, nil); code != http.StatusBadRequest {
		t.Fatalf("replica on the main device: %d", code)
	}
	if code := h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, ClusterPolicy{Replicas: []string{b.DeviceID}}, nil); code != http.StatusOK {
		t.Fatalf("set replicas: %d", code)
	}
	// The sweep starts a missing replica on the chosen device.
	h.srv.clusterSweep(ctx)
	deps, _ := h.st.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, Limit: 10})
	if len(deps) != 1 || deps[0].DeviceID != b.DeviceID || deps[0].Trigger != replicaTrigger {
		t.Fatalf("replica deployments: %+v", deps)
	}
	// A normal deployment updates the main copy first; the replica follows once it is live.
	h.srv.migratePoll = 10 * time.Millisecond
	var main store.Deployment
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, &main); code != http.StatusAccepted && code != http.StatusCreated {
		t.Fatalf("deploy: %d", code)
	}
	count := func(dev string) int {
		deps, _ := h.st.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, DeviceID: dev, Limit: 20})
		return len(deps)
	}
	time.Sleep(100 * time.Millisecond)
	if count(b.DeviceID) != 1 {
		t.Fatalf("the replica started before the main copy was live: %d deployments", count(b.DeviceID))
	}
	h.st.FinishDeployment(ctx, main.ID, true, "")
	for i := 0; i < 200 && count(b.DeviceID) < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if count(a.DeviceID) != 1 || count(b.DeviceID) != 2 {
		t.Fatalf("deployments a=%d b=%d", count(a.DeviceID), count(b.DeviceID))
	}
}

func TestRollingUpdateStopsAtFirstFailure(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.srv.cluster.grace = time.Hour
	h.srv.migratePoll = 10 * time.Millisecond
	caps := api.HostInfo{Capabilities: []string{api.CapBackup, api.CapPreview}}
	a, b, c := h.register("pi-a"), h.register("pi-b"), h.register("pi-c")
	for _, r := range []api.RegisterResponse{a, b, c} {
		h.do("POST", "/api/agent/heartbeat", r.AgentToken, api.HeartbeatRequest{Host: caps}, nil)
	}
	var p store.Project
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "web", "device_id": a.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx"}}, &p)
	h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, ClusterPolicy{Replicas: []string{b.DeviceID, c.DeviceID}}, nil)
	count := func(dev string) int {
		deps, _ := h.st.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, DeviceID: dev, Limit: 20})
		return len(deps)
	}
	waitFor := func(cond func() bool) bool {
		for i := 0; i < 200; i++ {
			if cond() {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}

	// If the main copy fails, no replica is touched.
	var d1 store.Deployment
	h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, &d1)
	h.st.FinishDeployment(ctx, d1.ID, false, "build failed")
	time.Sleep(150 * time.Millisecond)
	if count(b.DeviceID) != 0 || count(c.DeviceID) != 0 {
		t.Fatalf("replicas updated after a failed main deployment: b=%d c=%d", count(b.DeviceID), count(c.DeviceID))
	}

	// Main succeeds: the first replica goes alone; if it fails the second is not started.
	var d2 store.Deployment
	h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, &d2)
	h.st.FinishDeployment(ctx, d2.ID, true, "")
	if !waitFor(func() bool { return count(b.DeviceID) == 1 }) {
		t.Fatal("the first replica was not started")
	}
	if count(c.DeviceID) != 0 {
		t.Fatal("two replicas were updated at the same time")
	}
	deps, _ := h.st.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, DeviceID: b.DeviceID, Limit: 5})
	h.st.FinishDeployment(ctx, deps[0].ID, false, "unhealthy")
	time.Sleep(150 * time.Millisecond)
	if count(c.DeviceID) != 0 {
		t.Fatal("the rolling update carried on after a replica failed")
	}
}

func TestProjectViewShowsReplicasAndKeepsMainDeployment(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.srv.cluster.grace = time.Hour
	caps := api.HostInfo{Capabilities: []string{api.CapBackup, api.CapPreview}}
	a, b := h.register("pi-a"), h.register("pi-b")
	h.do("POST", "/api/agent/heartbeat", a.AgentToken, api.HeartbeatRequest{Host: caps}, nil)
	h.do("POST", "/api/agent/heartbeat", b.AgentToken, api.HeartbeatRequest{Host: caps}, nil)
	var p store.Project
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "web", "device_id": a.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx"}}, &p)
	h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, ClusterPolicy{Replicas: []string{b.DeviceID}}, nil)
	h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, nil)
	h.srv.clusterSweep(ctx)

	var v ProjectView
	if code := h.do("GET", "/api/projects/"+p.ID, adminToken, nil, &v); code != http.StatusOK {
		t.Fatalf("get project: %d", code)
	}
	if v.Latest == nil || v.Latest.DeviceID != a.DeviceID {
		t.Fatalf("the latest deployment must be the main copy's: %+v", v.Latest)
	}
	if len(v.Replicas) != 1 || v.Replicas[0].DeviceID != b.DeviceID || v.Replicas[0].DeploymentID == "" || v.Replicas[0].DeploymentStatus != "queued" {
		t.Fatalf("replicas: %+v", v.Replicas)
	}
}

func TestReplicaPortConflictFailureRetryAndRemoval(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.srv.cluster.grace = time.Hour
	caps := api.HostInfo{Capabilities: []string{api.CapBackup, api.CapPreview}}
	a, b := h.register("pi-a"), h.register("pi-b")
	h.do("POST", "/api/agent/heartbeat", a.AgentToken, api.HeartbeatRequest{Host: caps}, nil)
	// pi-b already publishes host port 8000 for another container.
	busy := []api.Container{{ID: "pt", Name: "portainer", State: "running", Ports: []api.Port{{PublicPort: 8000, PrivatePort: 8000, Type: "tcp"}}}}
	h.do("POST", "/api/agent/heartbeat", b.AgentToken, api.HeartbeatRequest{Host: caps, Containers: busy}, nil)
	var p store.Project
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "web", "device_id": a.DeviceID,
		"source":  map[string]any{"type": "image", "image": "nginx"},
		"runtime": map[string]any{"ports": []string{"8000:80"}}}, &p)

	// A device where the published port is taken is flagged before the copy is placed.
	pol := ClusterPolicy{Replicas: []string{b.DeviceID}}
	if code := h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, pol, nil); code != http.StatusConflict {
		t.Fatalf("conflicting replica: %d", code)
	}
	if code := h.do("PUT", "/api/projects/"+p.ID+"/cluster?confirm=1", adminToken, pol, nil); code != http.StatusOK {
		t.Fatalf("confirmed replica: %d", code)
	}

	// The sweep tries once. After the deployment fails it is not repeated by the server.
	h.srv.clusterSweep(ctx)
	deps, _ := h.st.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, DeviceID: b.DeviceID, Limit: 10})
	if len(deps) != 1 {
		t.Fatalf("replica deployments: %d", len(deps))
	}
	if err := h.st.FinishDeployment(ctx, deps[0].ID, false, "port conflict: port 8000/tcp is already published"); err != nil {
		t.Fatal(err)
	}
	h.srv.cluster.tried = map[string]time.Time{}
	h.srv.clusterSweep(ctx)
	if deps, _ = h.st.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, DeviceID: b.DeviceID, Limit: 10}); len(deps) != 1 {
		t.Fatalf("a failed replica was retried by the server: %d deployments", len(deps))
	}
	var v ProjectView
	h.do("GET", "/api/projects/"+p.ID, adminToken, nil, &v)
	if len(v.Replicas) != 1 || v.Replicas[0].DeploymentStatus != "failed" || v.Replicas[0].DeploymentError == "" || len(v.Replicas[0].Conflicts) == 0 {
		t.Fatalf("replica state: %+v", v.Replicas)
	}
	var alerts []Alert
	h.do("GET", "/api/alerts", adminToken, nil, &alerts)
	found := false
	for _, al := range alerts {
		found = found || al.Link == "/cluster"
	}
	if !found {
		t.Fatalf("no alert for the failed replica: %+v", alerts)
	}

	// Retrying starts a new deployment.
	if code := h.do("POST", "/api/projects/"+p.ID+"/cluster/replicas/"+b.DeviceID+"/retry", adminToken, nil, nil); code != http.StatusAccepted {
		t.Fatalf("retry: %d", code)
	}
	if deps, _ = h.st.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, DeviceID: b.DeviceID, Limit: 10}); len(deps) != 2 {
		t.Fatalf("after retry: %d deployments", len(deps))
	}

	// Removing a replica that has its container needs an explicit choice.
	h.do("POST", "/api/agent/heartbeat", b.AgentToken, api.HeartbeatRequest{Host: caps, Containers: []api.Container{{ID: "w1", Name: "web", State: "running"}}}, nil)
	if code := h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, ClusterPolicy{}, nil); code != http.StatusConflict {
		t.Fatalf("untick without a choice: %d", code)
	}
	body := map[string]any{"on_remove": "remove", "delete_volumes": true}
	if code := h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, body, nil); code != http.StatusOK {
		t.Fatalf("untick with a choice: %d", code)
	}
	var hb api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", b.AgentToken, api.HeartbeatRequest{Host: caps, Containers: []api.Container{{ID: "w1", Name: "web", State: "running"}}}, &hb)
	removed := false
	for _, c := range hb.Commands {
		if c.Action == api.ActionRemove && c.Target == "web" && c.Args["volumes"] == "delete" {
			removed = true
		}
	}
	if !removed {
		t.Fatalf("no remove command: %+v", hb.Commands)
	}
}

func TestReplicaOnDeviceInMaintenanceWaits(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.srv.cluster.grace = time.Hour
	caps := api.HostInfo{Capabilities: []string{api.CapBackup, api.CapPreview}}
	a, b := h.register("pi-a"), h.register("pi-b")
	h.do("POST", "/api/agent/heartbeat", a.AgentToken, api.HeartbeatRequest{Host: caps}, nil)
	h.do("POST", "/api/agent/heartbeat", b.AgentToken, api.HeartbeatRequest{Host: caps}, nil)
	var p store.Project
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "web", "device_id": a.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx"}}, &p)
	if err := h.st.SetDeviceMaintenance(ctx, b.DeviceID, true, "patching", time.Time{}); err != nil {
		t.Fatal(err)
	}
	h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, ClusterPolicy{Replicas: []string{b.DeviceID}}, nil)
	h.srv.clusterSweep(ctx)
	if deps, _ := h.st.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, DeviceID: b.DeviceID, Limit: 5}); len(deps) != 0 {
		t.Fatal("deployed to a device in maintenance")
	}
	var v ProjectView
	h.do("GET", "/api/projects/"+p.ID, adminToken, nil, &v)
	if len(v.Replicas) != 1 || !v.Replicas[0].Maintenance {
		t.Fatalf("replica state: %+v", v.Replicas)
	}
}

func TestEachCopyHasItsOwnAddress(t *testing.T) {
	h := newHarness(t)
	h.srv.cluster.grace = time.Hour
	a, b := h.register("pi-a"), h.register("pi-b")
	h.do("POST", "/api/agent/heartbeat", a.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Addresses: []string{"192.168.0.10"}, Capabilities: []string{api.CapBackup, api.CapPreview}}}, nil)
	h.do("POST", "/api/agent/heartbeat", b.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Addresses: []string{"192.168.0.20"}, Capabilities: []string{api.CapBackup, api.CapPreview}}}, nil)
	var p store.Project
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "web", "device_id": a.DeviceID,
		"source":  map[string]any{"type": "image", "image": "nginx"},
		"runtime": map[string]any{"ports": []string{"8080:80"}}}, &p)
	h.do("PUT", "/api/projects/"+p.ID+"/cluster", adminToken, ClusterPolicy{Replicas: []string{b.DeviceID}}, nil)
	var v ProjectView
	h.do("GET", "/api/projects/"+p.ID, adminToken, nil, &v)
	if v.MainURL != "http://192.168.0.10:8080" || len(v.Replicas) != 1 || v.Replicas[0].URL != "http://192.168.0.20:8080" {
		t.Fatalf("addresses: main %q replicas %+v", v.MainURL, v.Replicas)
	}
}

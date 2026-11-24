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
	// A normal deployment fans out to the replica as well.
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, nil); code != http.StatusAccepted && code != http.StatusCreated {
		t.Fatalf("deploy: %d", code)
	}
	deps, _ = h.st.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, Limit: 10})
	onA, onB := 0, 0
	for _, d := range deps {
		if d.DeviceID == a.DeviceID {
			onA++
		} else if d.DeviceID == b.DeviceID {
			onB++
		}
	}
	if onA != 1 || onB != 2 {
		t.Fatalf("deployments a=%d b=%d", onA, onB)
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

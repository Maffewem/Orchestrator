package server

import (
	"net/http"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

func TestCancelDeployment(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi")
	var p store.Project
	h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "web", "device_id": reg.DeviceID, "source": map[string]any{"type": "image", "image": "nginx"},
	}, &p)
	var dep store.Deployment
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, &dep); code != http.StatusAccepted {
		t.Fatalf("deploy: %d", code)
	}
	// The agent picks the command up and starts working on it.
	var hb api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{}, &hb)
	if len(hb.Commands) != 1 {
		t.Fatalf("commands: %+v", hb.Commands)
	}
	cmdID := hb.Commands[0].ID

	var got store.Deployment
	if code := h.do("POST", "/api/deployments/"+dep.ID+"/cancel", adminToken, nil, &got); code != http.StatusOK || got.Status != store.DeploymentFailed || got.Error == "" {
		t.Fatalf("cancel: %d %+v", code, got)
	}
	// The next heartbeat tells the agent to stop.
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{}, &hb)
	if len(hb.Cancel) != 1 || hb.Cancel[0] != cmdID {
		t.Fatalf("cancel list: %v", hb.Cancel)
	}
	// Its late failure report is accepted and doesn't change the outcome.
	if code := h.do("POST", "/api/agent/commands/"+cmdID+"/result", reg.AgentToken, api.CommandResult{Error: "context canceled"}, nil); code != http.StatusNoContent {
		t.Fatalf("late result: %d", code)
	}
	h.do("GET", "/api/deployments/"+dep.ID, adminToken, nil, &got)
	if got.Error == "context canceled" || got.Status != store.DeploymentFailed {
		t.Fatalf("after late result: %+v", got)
	}
	if code := h.do("POST", "/api/deployments/"+dep.ID+"/cancel", adminToken, nil, nil); code != http.StatusConflict {
		t.Fatalf("second cancel: %d", code)
	}
}

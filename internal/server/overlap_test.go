package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Several pushes landing at once: every webhook is accepted, queued builds for the same
// worker are superseded by the newest one, and what reaches the agent is the newest commit.
func TestOverlappingPushes(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi-3")
	var p ProjectView
	if code := h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "aurora", "device_id": reg.DeviceID,
		"source": map[string]any{"type": "github", "repo": "Maffewem/Aurora", "branch": "main", "auto_deploy": true},
	}, &p); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	var settings struct {
		GitHub struct {
			WebhookSecret string `json:"webhook_secret"`
		} `json:"github"`
	}
	h.do("GET", "/api/settings", adminToken, nil, &settings)

	push := func(sha string) int {
		body, _ := json.Marshal(map[string]any{
			"ref": "refs/heads/main", "repository": map[string]any{"full_name": "maffewem/aurora"},
			"head_commit": map[string]any{"id": sha, "message": "m", "author": map[string]any{"username": "x"}},
		})
		req, _ := http.NewRequest("POST", h.ts.URL+"/api/github/webhook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", sign(settings.GitHub.WebhookSecret, body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Ten pushes at the same instant.
	var wg sync.WaitGroup
	codes := make([]int, 10)
	for i := range codes {
		wg.Add(1)
		go func() { defer wg.Done(); codes[i] = push(fmt.Sprintf("%040d", i+1)) }()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusAccepted {
			t.Fatalf("push %d: %d", i, c)
		}
	}
	queued, err := h.st.QueuedPushDeployments(context.Background(), p.ID)
	if err != nil || len(queued) != 1 {
		t.Fatalf("queued after burst: %d %v", len(queued), err)
	}
	var hb api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{}, &hb)
	if len(hb.Commands) != 1 || hb.Commands[0].Target != queued[0].ID {
		t.Fatalf("commands: %+v", hb.Commands)
	}

	// A push while a build is running does not interrupt it; the newer one waits behind it.
	h.st.UpdateStep(context.Background(), reg.DeviceID, queued[0].ID, 0, api.StepUpdate{Status: api.StepRunning})
	push(fmt.Sprintf("%040d", 99))
	q2, _ := h.st.QueuedPushDeployments(context.Background(), p.ID)
	if len(q2) != 1 {
		t.Fatalf("queued behind running build: %d", len(q2))
	}
	running, _ := h.st.GetDeployment(context.Background(), queued[0].ID)
	if running.Status != store.DeploymentRunning {
		t.Fatalf("running build disturbed: %s", running.Status)
	}
}

// Commands created within the same second leave in the order they were queued.
func TestDispatchKeepsQueueOrder(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi-4")
	var ids []string
	for i := 0; i < 30; i++ {
		c, err := h.st.CreateCommand(context.Background(), reg.DeviceID, api.ActionRestart, fmt.Sprintf("c%02d", i), nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	var hb api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{}, &hb)
	if len(hb.Commands) != len(ids) {
		t.Fatalf("got %d commands", len(hb.Commands))
	}
	for i, c := range hb.Commands {
		if c.ID != ids[i] {
			t.Fatalf("command %d out of order", i)
		}
	}
}

package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// fakeAgent answers its device's commands the way a healthy agent would.
func (h *harness) fakeAgent(t *testing.T, reg api.RegisterResponse, containers func() []api.Container, log *[]string, mu *sync.Mutex, stop <-chan struct{}) {
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			var hb api.HeartbeatResponse
			h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
				Host:       api.HostInfo{Capabilities: []string{api.CapBackup, api.CapPreview}},
				Containers: containers(),
			}, &hb)
			for _, c := range hb.Commands {
				mu.Lock()
				*log = append(*log, reg.DeviceID+":"+c.Action)
				mu.Unlock()
				switch c.Action {
				case api.ActionBackup:
					req, _ := http.NewRequest("POST", h.ts.URL+"/api/agent/backups/"+c.Args["backup_id"], bytes.NewReader([]byte("data")))
					req.Header.Set("Authorization", "Bearer "+reg.AgentToken)
					if resp, err := http.DefaultClient.Do(req); err == nil {
						resp.Body.Close()
					}
				case api.ActionRestore:
					req, _ := http.NewRequest("GET", h.ts.URL+"/api/agent/backups/"+c.Args["backup_id"], nil)
					req.Header.Set("Authorization", "Bearer "+reg.AgentToken)
					resp, err := http.DefaultClient.Do(req)
					if err != nil || resp.StatusCode != 200 {
						h.do("POST", "/api/agent/commands/"+c.ID+"/result", reg.AgentToken, api.CommandResult{Error: "download refused"}, nil)
						continue
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				h.do("POST", "/api/agent/commands/"+c.ID+"/result", reg.AgentToken, api.CommandResult{OK: true}, nil)
			}
		}
	}()
}

func TestMigrateProjectWithData(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.BackupDir = t.TempDir()
	h.srv.migratePoll = 10 * time.Millisecond
	ctx := context.Background()
	oldReg, newReg := h.register("pi-old"), h.register("nas-new")

	var (
		mu       sync.Mutex
		log      []string
		deployed bool
	)
	stop := make(chan struct{})
	defer close(stop)
	h.fakeAgent(t, oldReg, func() []api.Container {
		return []api.Container{{ID: "old1", Name: "web", Image: "nginx", State: "running"}}
	}, &log, &mu, stop)
	h.fakeAgent(t, newReg, func() []api.Container {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range log {
			if l == newReg.DeviceID+":"+api.ActionDeploy {
				deployed = true
			}
		}
		if !deployed {
			return nil
		}
		return []api.Container{{ID: "new1", Name: "web", Image: "nginx", State: "running"}}
	}, &log, &mu, stop)
	time.Sleep(50 * time.Millisecond)

	var p store.Project
	if code := h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "web", "device_id": oldReg.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx"},
	}, &p); code != http.StatusCreated {
		t.Fatalf("create project: %d", code)
	}
	if code := h.do("POST", "/api/projects/"+p.ID+"/migrate", adminToken, map[string]any{"device_id": oldReg.DeviceID}, nil); code != http.StatusBadRequest {
		t.Fatalf("same device: %d", code)
	}
	var v MigrationView
	if code := h.do("POST", "/api/projects/"+p.ID+"/migrate", adminToken, map[string]any{"device_id": newReg.DeviceID, "stop_source": true}, &v); code != http.StatusAccepted {
		t.Fatalf("migrate: %d", code)
	}
	for i := 0; i < 400 && v.Status == migRunning; i++ {
		time.Sleep(25 * time.Millisecond)
		h.do("GET", "/api/projects/"+p.ID+"/migration", adminToken, nil, &v)
	}
	if v.Status != migDone {
		t.Fatalf("migration: %+v", v)
	}
	got, _ := h.st.GetProject(ctx, p.ID)
	if got.DeviceID != newReg.DeviceID {
		t.Fatalf("project is on %s", got.DeviceID)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	want := []string{oldReg.DeviceID + ":" + api.ActionBackup, newReg.DeviceID + ":" + api.ActionDeploy, newReg.DeviceID + ":" + api.ActionRestore, oldReg.DeviceID + ":" + api.ActionStop}
	idx := 0
	for _, l := range log {
		if idx < len(want) && l == want[idx] {
			idx++
		}
	}
	if idx != len(want) {
		t.Fatalf("command order: %v", log)
	}
}

func TestMigrateRevertsOnFailedDeploy(t *testing.T) {
	h := newHarness(t)
	h.srv.migratePoll = 10 * time.Millisecond
	oldReg, newReg := h.register("pi-old"), h.register("nas-new")
	h.do("POST", "/api/agent/heartbeat", newReg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: []string{api.CapBackup, api.CapPreview}}}, &api.HeartbeatResponse{})

	var p store.Project
	h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "web", "device_id": oldReg.DeviceID, "source": map[string]any{"type": "image", "image": "nginx"},
	}, &p)
	var v MigrationView
	h.do("POST", "/api/projects/"+p.ID+"/migrate", adminToken, map[string]any{"device_id": newReg.DeviceID, "with_data": false}, &v)

	var hb api.HeartbeatResponse
	for i := 0; i < 200 && len(hb.Commands) == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		h.do("POST", "/api/agent/heartbeat", newReg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: []string{api.CapBackup, api.CapPreview}}}, &hb)
	}
	if len(hb.Commands) != 1 {
		t.Fatalf("no deploy reached the new device")
	}
	h.do("POST", "/api/agent/commands/"+hb.Commands[0].ID+"/result", newReg.AgentToken, api.CommandResult{Error: "pull failed"}, nil)
	for i := 0; i < 200 && v.Status == migRunning; i++ {
		time.Sleep(20 * time.Millisecond)
		h.do("GET", "/api/projects/"+p.ID+"/migration", adminToken, nil, &v)
	}
	got, _ := h.st.GetProject(context.Background(), p.ID)
	if v.Status != migFailed || got.DeviceID != oldReg.DeviceID {
		t.Fatalf("status %q, project on %s (want %s)", v.Status, got.DeviceID, oldReg.DeviceID)
	}
}

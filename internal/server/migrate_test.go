package server

import (
	"bytes"
	"context"
	"encoding/base64"
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

func TestMaintenanceBlocksNewWork(t *testing.T) {
	h := newHarness(t)
	a, b := h.register("pi-a"), h.register("pi-b")
	var p store.Project
	body := map[string]any{"name": "web", "device_id": a.DeviceID, "source": map[string]any{"type": "image", "image": "nginx"}}
	if code := h.do("POST", "/api/projects", adminToken, body, &p); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	var dv DeviceView
	if code := h.do("PATCH", "/api/devices/"+b.DeviceID, adminToken, map[string]any{"maintenance": true}, &dv); code != http.StatusOK || !dv.Maintenance {
		t.Fatalf("enable: %d %+v", code, dv.Device)
	}
	body["name"] = "other"
	body["device_id"] = b.DeviceID
	if code := h.do("POST", "/api/projects", adminToken, body, nil); code != http.StatusConflict {
		t.Fatalf("create on maintenance device: %d", code)
	}
	if code := h.do("POST", "/api/projects/"+p.ID+"/migrate", adminToken, map[string]any{"device_id": b.DeviceID}, nil); code != http.StatusConflict {
		t.Fatalf("migrate to maintenance device: %d", code)
	}
	if code := h.do("POST", "/api/devices/"+b.DeviceID+"/adopt", adminToken, map[string]any{"containers": []map[string]any{{"id": "x"}}}, nil); code != http.StatusConflict {
		t.Fatalf("adopt on maintenance device: %d", code)
	}
	h.do("PATCH", "/api/devices/"+b.DeviceID, adminToken, map[string]any{"maintenance": false}, nil)
	if code := h.do("POST", "/api/projects", adminToken, body, nil); code != http.StatusCreated {
		t.Fatalf("create after maintenance: %d", code)
	}
}

func TestMigrationMovesTunnelRoutes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := h.register("pi-a"), h.register("pi-b")
	da, _ := h.st.GetDevice(ctx, a.DeviceID)
	db, _ := h.st.GetDevice(ctx, b.DeviceID)
	tok := func(id string) string {
		return base64.StdEncoding.EncodeToString([]byte(`{"a":"acct","t":"` + id + `"}`))
	}
	p := &store.Project{Name: "web", DeviceID: b.DeviceID, Source: store.Source{Type: store.SourceImage, Image: "nginx"},
		Runtime: store.Runtime{Ports: []string{"8080:80"}}}
	if err := h.st.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	h.srv.saveTunnelConfig(ctx, da.ID, &tunnelConfig{Token: tok("t1"), Routes: []tunnelRoute{
		{Hostname: "web.example.com", Service: "project:web"}, {Hostname: "other.example.com", Service: "http://localhost:9"}}})
	h.srv.saveTunnelConfig(ctx, db.ID, &tunnelConfig{Token: tok("t2")})

	lines := h.srv.moveTunnelRoutes(ctx, p, da.ID, db)
	if len(lines) == 0 {
		t.Fatal("expected a log line")
	}
	oc, _ := h.srv.tunnelConfig(ctx, da.ID)
	nc, _ := h.srv.tunnelConfig(ctx, db.ID)
	if len(oc.Routes) != 1 || oc.Routes[0].Hostname != "other.example.com" || len(nc.Routes) != 1 || nc.Routes[0].Hostname != "web.example.com" {
		t.Fatalf("old %+v new %+v", oc.Routes, nc.Routes)
	}

	// On a shared tunnel the origin is the owning device's address, not localhost.
	db.Host.Addresses = []string{"127.0.0.1", "192.168.1.20"}
	got, err := h.srv.resolveRoute([]*store.Project{p}, db, true, "", tunnelRoute{Hostname: "x", Service: "project:web"})
	if err != nil || got != "http://192.168.1.20:8080" {
		t.Fatalf("shared origin %q %v", got, err)
	}
	if got, _ := h.srv.resolveRoute([]*store.Project{p}, db, false, "", tunnelRoute{Hostname: "x", Service: "project:web"}); got != "http://localhost:8080" {
		t.Fatalf("single origin %q", got)
	}
}

func TestMaintenanceNoteAndExpiry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.register("pi-a")
	var dv DeviceView
	if code := h.do("PATCH", "/api/devices/"+a.DeviceID, adminToken, map[string]any{"maintenance": true, "maintenance_note": "SD card swap", "maintenance_hours": 2}, &dv); code != http.StatusOK ||
		!dv.Maintenance || dv.MaintenanceNote != "SD card swap" || dv.MaintenanceUntil == nil {
		t.Fatalf("enable: %d %+v", code, dv.Device)
	}
	if ended, _ := h.st.ExpireMaintenance(ctx, time.Now()); len(ended) != 0 {
		t.Fatal("expired early")
	}
	ended, _ := h.st.ExpireMaintenance(ctx, time.Now().Add(3*time.Hour))
	if len(ended) != 1 {
		t.Fatalf("expected one expiry, got %d", len(ended))
	}
	d, _ := h.st.GetDevice(ctx, a.DeviceID)
	if d.Maintenance || d.MaintenanceNote != "" {
		t.Fatalf("still in maintenance: %+v", d)
	}
}

func TestMigrationFromBackupAndStacks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	oldReg, newReg := h.register("pi-old"), h.register("pi-new")
	h.do("POST", "/api/agent/heartbeat", newReg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: []string{api.CapBackup, api.CapPreview, api.CapCompose}}}, nil)
	var p store.Project
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "web", "device_id": oldReg.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx"}}, &p)
	h.st.MarkOffline(ctx, time.Now().Add(time.Hour))
	h.do("POST", "/api/agent/heartbeat", newReg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: []string{api.CapBackup, api.CapPreview, api.CapCompose}}}, nil)
	body := map[string]any{"device_id": newReg.DeviceID}
	if code := h.do("POST", "/api/projects/"+p.ID+"/migrate", adminToken, body, nil); code != http.StatusConflict {
		t.Fatalf("offline source without backup choice: %d", code)
	}
	body["from_backup"] = true
	if code := h.do("POST", "/api/projects/"+p.ID+"/migrate", adminToken, body, nil); code != http.StatusConflict {
		t.Fatalf("no backup on the server: %d", code)
	}
	var s store.Project
	if code := h.do("POST", "/api/projects", adminToken, map[string]any{"name": "stack", "device_id": oldReg.DeviceID,
		"source": map[string]any{"type": "compose", "compose": "services:\n  web:\n    image: nginx\n"}}, &s); code != http.StatusCreated {
		t.Fatalf("create stack: %d", code)
	}
	if code := h.do("POST", "/api/projects/"+s.ID+"/migrate", adminToken, map[string]any{"device_id": newReg.DeviceID}, nil); code != http.StatusAccepted {
		t.Fatalf("stacks can be moved: %d", code)
	}
}

func TestRecoverInterruptedMigration(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a, b := h.register("pi-a"), h.register("pi-b")
	p := &store.Project{Name: "web", DeviceID: b.DeviceID, Source: store.Source{Type: store.SourceImage, Image: "nginx"}}
	if err := h.st.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	// The project was already switched to pi-b but never ran there.
	h.srv.persistMigration(p.ID, MigrationView{Status: migRunning, From: "pi-a", To: "pi-b", FromID: a.DeviceID, ToID: b.DeviceID})
	h.srv.recoverMigrations(ctx)
	got, _ := h.st.GetProject(ctx, p.ID)
	if got.DeviceID != a.DeviceID {
		t.Fatalf("project should be back on pi-a, is on %s", got.DeviceID)
	}
	var v MigrationView
	if code := h.do("GET", "/api/projects/"+p.ID+"/migration", adminToken, nil, &v); code != http.StatusOK || v.Status != migFailed {
		t.Fatalf("saved migration: %d %+v", code, v)
	}
}

func TestRequireHTTPSAgents(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi-a")
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{}, nil)
	sec := map[string]any{"session_hours": 24, "min_password": 8, "force_https": false, "require_https_agents": true}
	if code := h.do("PUT", "/api/settings/security", adminToken, sec, nil); code != http.StatusConflict {
		t.Fatalf("a plain-HTTP agent must block the switch: %d", code)
	}
	h.srv.links.Store(reg.DeviceID, true)
	if code := h.do("PUT", "/api/settings/security", adminToken, sec, nil); code != http.StatusOK {
		t.Fatalf("enable: %d", code)
	}
	if code := h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{}, nil); code != http.StatusUpgradeRequired {
		t.Fatalf("plain HTTP agent: %d", code)
	}
}

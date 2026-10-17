package server

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

func TestBackupLifecycle(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.BackupDir = t.TempDir()
	reg := h.register("pi-4")
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
		Containers: []api.Container{{ID: "abc123", Name: "web", Image: "nginx", State: "running"}},
	}, &api.HeartbeatResponse{})

	var b store.Backup
	if code := h.do("POST", "/api/devices/"+reg.DeviceID+"/containers/nope/backup", adminToken, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown container: %d", code)
	}
	if code := h.do("POST", "/api/devices/"+reg.DeviceID+"/containers/abc123/backup", adminToken, nil, &b); code != http.StatusAccepted || b.Status != store.BackupPending {
		t.Fatalf("create: %d %+v", code, b)
	}
	if code := h.do("GET", "/api/backups/"+b.ID+"/download", adminToken, nil, nil); code != http.StatusConflict {
		t.Fatalf("download before upload: %d", code)
	}

	var hb api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
		Containers: []api.Container{{ID: "abc123", Name: "web"}},
	}, &hb)
	if len(hb.Commands) != 1 || hb.Commands[0].Action != api.ActionBackup || hb.Commands[0].Args["backup_id"] != b.ID {
		t.Fatalf("agent command: %+v", hb.Commands)
	}

	upload := func(token string) int {
		req, _ := http.NewRequest("POST", h.ts.URL+"/api/agent/backups/"+b.ID, bytes.NewReader([]byte("archive-bytes")))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	other := h.register("pi-5")
	if code := upload(other.AgentToken); code != http.StatusForbidden {
		t.Fatalf("other device upload: %d", code)
	}
	if code := upload(reg.AgentToken); code != http.StatusNoContent {
		t.Fatalf("upload: %d", code)
	}

	var list []store.Backup
	h.do("GET", "/api/backups", adminToken, nil, &list)
	if len(list) != 1 || list[0].Status != store.BackupComplete || list[0].Size != 13 || list[0].ContainerName != "web" {
		t.Fatalf("list: %+v", list)
	}
	req, _ := http.NewRequest("GET", h.ts.URL+"/api/backups/"+b.ID+"/download", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "archive-bytes" {
		t.Fatalf("download = %q", body)
	}

	var cmd api.Command
	if code := h.do("POST", "/api/backups/"+b.ID+"/restore", adminToken, nil, &cmd); code != http.StatusAccepted || cmd.Action != api.ActionRestore || cmd.Target != "abc123" {
		t.Fatalf("restore: %d %+v", code, cmd)
	}

	if code := h.do("DELETE", "/api/backups/"+b.ID, adminToken, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code := h.do("GET", "/api/backups/"+b.ID+"/download", adminToken, nil, nil); code != http.StatusNotFound {
		t.Fatalf("download after delete: %d", code)
	}
}

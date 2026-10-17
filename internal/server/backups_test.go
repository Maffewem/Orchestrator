package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/backupcrypt"
	"github.com/Maffewem/Orchestrator/internal/store"
)

func TestBackupLifecycle(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.BackupDir = t.TempDir()
	reg := h.register("pi-4")
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
		Host:       api.HostInfo{Capabilities: []string{api.CapBackup, api.CapUpdate}},
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
		Host:       api.HostInfo{Capabilities: []string{api.CapBackup, api.CapUpdate}},
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

func TestOldAgentIsToldToUpdate(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.BackupDir = t.TempDir()
	reg := h.register("pi-old")
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
		Containers: []api.Container{{ID: "abc123", Name: "web", State: "running"}},
	}, &api.HeartbeatResponse{})

	var e struct {
		Error string `json:"error"`
	}
	req, _ := http.NewRequest("POST", h.ts.URL+"/api/devices/"+reg.DeviceID+"/containers/abc123/backup", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&e)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(e.Error, "too old") || !strings.Contains(e.Error, "install command") {
		t.Fatalf("%d %q", resp.StatusCode, e.Error)
	}
	if list, _ := h.st.ListBackups(context.Background(), ""); len(list) != 0 {
		t.Fatalf("a backup was recorded for an agent that can't run it: %+v", list)
	}
	code := h.do("PUT", "/api/devices/"+reg.DeviceID+"/workers/web/backup-policy", adminToken,
		BackupPolicy{Mode: "daily", Time: "02:00", TZ: "UTC", Keep: 3}, &e)
	if code != http.StatusConflict {
		t.Fatalf("policy for old agent: %d", code)
	}
}

func TestBackupPolicyScheduleAndRetention(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.BackupDir = t.TempDir()
	ctx := context.Background()
	reg := h.register("pi-sched")
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
		Host:       api.HostInfo{Capabilities: []string{api.CapBackup}},
		Containers: []api.Container{{ID: "abc123", Name: "web", Image: "nginx", State: "running"}},
	}, &api.HeartbeatResponse{})
	path := "/api/devices/" + reg.DeviceID + "/workers/web/backup-policy"

	var v backupPolicyView
	h.do("GET", path, adminToken, nil, &v)
	if v.Mode != "off" || v.Keep != 7 {
		t.Fatalf("default policy: %+v", v)
	}
	if code := h.do("PUT", path, adminToken, BackupPolicy{Mode: "daily", Time: "02:00", TZ: "UTC", Keep: 0}, nil); code != http.StatusBadRequest {
		t.Fatalf("keep 0 accepted: %d", code)
	}
	if code := h.do("PUT", "/api/devices/"+reg.DeviceID+"/workers/ghost/backup-policy", adminToken, BackupPolicy{Mode: "off", Time: "02:00", TZ: "UTC", Keep: 2}, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown worker accepted: %d", code)
	}
	if code := h.do("PUT", path, adminToken, BackupPolicy{Mode: "daily", Time: "02:00", TZ: "UTC", Keep: 2}, &v); code != 200 || v.NextRun == nil {
		t.Fatalf("set policy: %d %+v", code, v)
	}

	// Backdate the baseline so a slot has passed, then tick.
	h.srv.backupTick(ctx, time.Now().Add(-time.Hour)) // before any new slot: nothing
	if list, _ := h.st.ListBackups(ctx, ""); len(list) != 0 {
		t.Fatalf("backup before its slot: %d", len(list))
	}
	h.srv.backupTick(ctx, time.Now().Add(48*time.Hour))
	list, _ := h.st.ListBackups(ctx, "")
	if len(list) != 1 || list[0].Source != "scheduled" {
		t.Fatalf("scheduled backup not created: %+v", list)
	}
	h.srv.backupTick(ctx, time.Now().Add(48*time.Hour+time.Minute))
	if list, _ := h.st.ListBackups(ctx, ""); len(list) != 1 {
		t.Fatalf("ran twice for one slot: %d", len(list))
	}

	// Retention keeps the newest 2 scheduled backups and leaves manual ones alone.
	mk := func(source string) string {
		b := &store.Backup{DeviceID: reg.DeviceID, DeviceName: "pi-sched", ContainerName: "web", Source: source}
		h.st.CreateBackup(ctx, b)
		h.st.FinishBackup(ctx, b.ID, 1, "")
		os.WriteFile(h.srv.backupPath(b.ID), []byte("x"), 0o600)
		return b.ID
	}
	manual := mk("manual")
	for i := 0; i < 4; i++ {
		mk("scheduled")
	}
	h.srv.pruneBackups(ctx, reg.DeviceID, "web")
	scheduled, kept := 0, false
	all, _ := h.st.ListBackups(ctx, "")
	for _, b := range all {
		if b.Source == "scheduled" && b.Status == store.BackupComplete {
			scheduled++
		}
		kept = kept || b.ID == manual
	}
	if scheduled != 2 || !kept {
		t.Fatalf("retention: %d scheduled complete kept, manual kept=%v", scheduled, kept)
	}
}

func TestBackupPassword(t *testing.T) {
	backupcrypt.Iterations = 1000
	h := newHarness(t)
	h.srv.cfg.BackupDir = t.TempDir()
	reg := h.register("pi-pw")
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
		Host:       api.HostInfo{Capabilities: []string{api.CapBackup}},
		Containers: []api.Container{{ID: "abc123", Name: "web", Image: "nginx", State: "running"}},
	}, &api.HeartbeatResponse{})

	// take makes a backup the way an agent does: queue it, then upload the archive.
	take := func(archive string) store.Backup {
		var b store.Backup
		if code := h.do("POST", "/api/devices/"+reg.DeviceID+"/containers/abc123/backup", adminToken, nil, &b); code != http.StatusAccepted {
			t.Fatalf("queue: %d", code)
		}
		req, _ := http.NewRequest("POST", h.ts.URL+"/api/agent/backups/"+b.ID, strings.NewReader(archive))
		req.Header.Set("Authorization", "Bearer "+reg.AgentToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("upload: %v %v", err, resp)
		}
		resp.Body.Close()
		got, _ := h.st.GetBackup(context.Background(), b.ID)
		return *got
	}
	download := func(b store.Backup) (int, string) {
		req, _ := http.NewRequest("GET", h.ts.URL+"/api/backups/"+b.ID+"/download", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	plain := take("plain archive bytes")
	if plain.Encrypted {
		t.Fatal("backup encrypted without a password")
	}

	if code := h.do("PUT", "/api/settings/backup-password", adminToken, map[string]string{"password": "short"}, nil); code != http.StatusBadRequest {
		t.Fatalf("short password: %d", code)
	}
	h.do("PUT", "/api/settings/backup-password", adminToken, map[string]string{"password": "correct horse battery"}, nil)
	var set struct {
		Set bool `json:"backup_password_set"`
	}
	h.do("GET", "/api/settings", adminToken, nil, &set)
	if !set.Set {
		t.Fatal("settings should say a backup password is set")
	}

	secret := "super secret database contents"
	enc := take(secret)
	if !enc.Encrypted || enc.Size != int64(len(secret)) {
		t.Fatalf("encrypted backup: %+v", enc)
	}
	onDisk, err := os.ReadFile(h.srv.backupFile(&enc))
	if err != nil || !strings.HasSuffix(h.srv.backupFile(&enc), ".tar.gz.enc") || bytes.Contains(onDisk, []byte(secret)) {
		t.Fatalf("file on disk: %v %v", err, strings.HasSuffix(h.srv.backupFile(&enc), ".enc"))
	}
	if code, body := download(enc); code != http.StatusOK || body != secret {
		t.Fatalf("download: %d %q", code, body)
	}
	if code, body := download(plain); code != http.StatusOK || body != "plain archive bytes" {
		t.Fatalf("older plain backup: %d %q", code, body)
	}

	// A different password can't open it, and says so before sending anything.
	h.do("PUT", "/api/settings/backup-password", adminToken, map[string]string{"password": "another password"}, nil)
	if code, body := download(enc); code != http.StatusConflict || !strings.Contains(body, "different password") {
		t.Fatalf("wrong password: %d %q", code, body)
	}
	h.do("PUT", "/api/settings/backup-password", adminToken, map[string]string{"password": ""}, nil)
	if code, body := download(enc); code != http.StatusConflict || !strings.Contains(body, "no backup password is set") {
		t.Fatalf("no password: %d %q", code, body)
	}
	if after := take("now plain again"); after.Encrypted {
		t.Fatal("removing the password should stop encrypting")
	}

	h.do("DELETE", "/api/backups/"+enc.ID, adminToken, nil, nil)
	if _, err := os.Stat(h.srv.backupFile(&enc)); !os.IsNotExist(err) {
		t.Fatalf("encrypted file not removed: %v", err)
	}
}

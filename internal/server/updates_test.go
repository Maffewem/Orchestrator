package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestUpdateStatusScheduleAndAgents(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Version = "v2"
	h.srv.cfg.DockerHost = "unix:///nonexistent.sock"
	reg := h.register("pi-6")
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
		Host: api.HostInfo{Hostname: "pi-6", AgentVersion: "v1", Arch: "arm64", Capabilities: []string{api.CapUpdate}},
	}, &api.HeartbeatResponse{})

	var st updateStatus
	if code := h.do("GET", "/api/system/update", adminToken, nil, &st); code != 200 {
		t.Fatalf("status: %d", code)
	}
	if st.CanUpdate || st.Reason == "" || st.Version != "v2" || st.Schedule.Mode != "off" {
		t.Fatalf("status = %+v", st)
	}
	if len(st.Agents) != 1 || !st.Agents[0].Outdated || !st.Agents[0].Online {
		t.Fatalf("agents = %+v", st.Agents)
	}
	if code := h.do("POST", "/api/system/update/apply", adminToken, nil, nil); code != http.StatusConflict {
		t.Fatalf("apply without docker: %d", code)
	}

	bad := Schedule{Mode: "daily", Time: "9am", TZ: "UTC"}
	if code := h.do("PUT", "/api/system/update/schedule", adminToken, bad, nil); code != http.StatusBadRequest {
		t.Fatalf("bad schedule: %d", code)
	}
	good := Schedule{Mode: "weekly", Weekday: 2, Time: "04:15", TZ: "Europe/London", Agents: true}
	if code := h.do("PUT", "/api/system/update/schedule", adminToken, good, &st); code != 200 || st.Schedule != good || st.NextRun == nil {
		t.Fatalf("set schedule: %d %+v", code, st)
	}

	var q map[string]int
	h.do("POST", "/api/system/update/agents", adminToken, nil, &q)
	if q["queued"] != 1 {
		t.Fatalf("queued = %v", q)
	}
	var hb api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
		Host: api.HostInfo{Hostname: "pi-6", AgentVersion: "v1", Capabilities: []string{api.CapUpdate}},
	}, &hb)
	if len(hb.Commands) != 1 || hb.Commands[0].Action != api.ActionAgentUpdate {
		t.Fatalf("commands = %+v", hb.Commands)
	}
	h.do("POST", "/api/system/update/agents", adminToken, nil, &q)
	if q["queued"] != 0 {
		t.Fatalf("queued again straight away: %v", q)
	}
}

func TestAgentUpdateInfo(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	h.srv.cfg.DownloadsDir, h.srv.cfg.Version = dir, "v9"
	bin := []byte("agent-build")
	os.WriteFile(filepath.Join(dir, "orchestrator-agent-linux-arm64"), bin, 0o755)
	reg := h.register("pi-7")

	var info api.AgentUpdateInfo
	if code := h.do("GET", "/api/agent/update?arch=arm64", reg.AgentToken, nil, &info); code != 200 {
		t.Fatalf("info: %d", code)
	}
	sum := sha256.Sum256(bin)
	if info.Version != "v9" || info.SHA256 != hex.EncodeToString(sum[:]) || info.URL != "/downloads/orchestrator-agent-linux-arm64" {
		t.Fatalf("info = %+v", info)
	}
	if code := h.do("GET", "/api/agent/update?arch=armv6", reg.AgentToken, nil, nil); code != http.StatusNotFound {
		t.Fatalf("missing build: %d", code)
	}
	if code := h.do("GET", "/api/agent/update?arch=../x", reg.AgentToken, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("bad arch: %d", code)
	}
}

func TestScheduleTickFiresOncePerSlot(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Version = "v2"
	h.srv.cfg.DockerHost = "unix:///nonexistent.sock"
	ctx := context.Background()
	reg := h.register("pi-8")
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{AgentVersion: "v1", Capabilities: []string{api.CapUpdate}}}, &api.HeartbeatResponse{})

	sch := Schedule{Mode: "daily", Time: "03:00", TZ: "UTC", Agents: true}
	h.srv.setJSON(ctx, settingUpdateSchedule, sch)
	set := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	h.st.SetSetting(ctx, settingUpdateSetAt, set.Format(time.RFC3339))

	h.srv.scheduleTick(ctx, time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)) // no slot since it was set
	if n := h.queuedAgentUpdates(reg); n != 0 {
		t.Fatalf("fired before its slot: %d", n)
	}
	h.srv.scheduleTick(ctx, time.Date(2026, 10, 18, 3, 0, 20, 0, time.UTC))
	if n := h.queuedAgentUpdates(reg); n != 1 {
		t.Fatalf("not fired at its slot: %d", n)
	}
	h.srv.up.agentTried = map[string]time.Time{}
	h.srv.scheduleTick(ctx, time.Date(2026, 10, 18, 3, 1, 0, 0, time.UTC))
	if n := h.queuedAgentUpdates(reg); n != 0 {
		t.Fatalf("fired twice for one slot: %d", n)
	}
}

func (h *harness) queuedAgentUpdates(reg api.RegisterResponse) int {
	var hb api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{AgentVersion: "v1", Capabilities: []string{api.CapUpdate}}}, &hb)
	n := 0
	for _, c := range hb.Commands {
		if c.Action == api.ActionAgentUpdate {
			n++
		}
	}
	return n
}

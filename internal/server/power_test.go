package server

import (
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestDevicePowerNeedsMaintenanceNameAndCapability(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	host := api.HostInfo{Hostname: "pi", Arch: "arm64", Capabilities: []string{api.CapPower}}
	reg := h.register("pi")
	var hb api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: host, Metrics: api.Metrics{UptimeSeconds: 100000}}, &hb)
	url := "/api/devices/" + reg.DeviceID + "/power"
	body := map[string]any{"mode": "restart", "confirm": "pi", "exit_maintenance": true}

	if code := h.do("POST", url, adminToken, body, nil); code != 409 {
		t.Fatalf("not in maintenance: %d", code)
	}
	if err := h.st.SetDeviceMaintenance(ctx, reg.DeviceID, true, "", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if code := h.do("POST", url, h.makeUser("u-operator", "operator"), body, nil); code != 403 {
		t.Fatalf("operator: %d", code)
	}
	if code := h.do("POST", url, adminToken, map[string]any{"mode": "restart", "confirm": "wrong"}, nil); code != 400 {
		t.Fatalf("wrong name: %d", code)
	}
	if code := h.do("POST", url, adminToken, map[string]any{"mode": "explode", "confirm": "pi"}, nil); code != 400 {
		t.Fatalf("bad mode: %d", code)
	}
	// An agent without the capability is told to update.
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Hostname: "pi"}}, &hb)
	if code := h.do("POST", url, adminToken, body, nil); code != 409 {
		t.Fatalf("old agent: %d", code)
	}
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: host, Metrics: api.Metrics{UptimeSeconds: 100000}}, &hb)

	var cmd api.Command
	if code := h.do("POST", url, adminToken, body, &cmd); code != 202 {
		t.Fatalf("request: %d", code)
	}
	if cmd.Action != api.ActionDevicePower || cmd.Args["mode"] != "reboot" || cmd.Args["confirm"] != "reboot" {
		t.Fatalf("command: %+v", cmd)
	}
	if code := h.do("POST", url, adminToken, body, nil); code != 409 {
		t.Fatalf("second request while pending: %d", code)
	}
	var v DeviceView
	h.do("GET", "/api/devices/"+reg.DeviceID, adminToken, nil, &v)
	if v.Power == nil || v.Power.Mode != "restart" || v.PowerImpact == nil {
		t.Fatalf("view: %+v", v.Power)
	}

	// Still the same boot: nothing changes. A fresh boot clears the marker and leaves maintenance.
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: host, Metrics: api.Metrics{UptimeSeconds: 100010}}, &hb)
	if h.srv.loadPower(ctx, reg.DeviceID) == nil {
		t.Fatal("marker cleared without a reboot")
	}
	time.Sleep(5200 * time.Millisecond)
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: host, Metrics: api.Metrics{UptimeSeconds: 3}}, &hb)
	if h.srv.loadPower(ctx, reg.DeviceID) != nil {
		t.Fatal("marker not cleared after the device rebooted")
	}
	d, _ := h.st.GetDevice(ctx, reg.DeviceID)
	if d.Maintenance {
		t.Fatal("maintenance mode should have ended")
	}
}

func TestDevicePowerFailureClearsMarker(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	reg := h.register("pi")
	var hb api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Hostname: "pi", Capabilities: []string{api.CapPower}}}, &hb)
	h.st.SetDeviceMaintenance(ctx, reg.DeviceID, true, "", time.Time{})
	var cmd api.Command
	h.do("POST", "/api/devices/"+reg.DeviceID+"/power", adminToken, map[string]any{"mode": "shutdown", "confirm": "pi"}, &cmd)
	h.do("POST", "/api/agent/commands/"+cmd.ID+"/result", reg.AgentToken, api.CommandResult{Error: "no systemd"}, nil)
	if h.srv.loadPower(ctx, reg.DeviceID) != nil {
		t.Fatal("a refused request must not leave the device marked as shutting down")
	}
}

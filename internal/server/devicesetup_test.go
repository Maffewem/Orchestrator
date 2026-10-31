package server

import (
	"net/http"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestDeviceSetupPlan(t *testing.T) {
	h := newHarness(t)
	var plan struct {
		ID string `json:"id"`
	}
	if code := h.do("POST", "/api/device-setups", adminToken, map[string]any{"proxy": true, "maintenance": true, "maintenance_note": "burn-in"}, &plan); code != http.StatusCreated || plan.ID == "" {
		t.Fatalf("create plan: %d %q", code, plan.ID)
	}
	if code := h.do("POST", "/api/device-setups", adminToken, map[string]any{"tunnel_token": "junk"}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad tunnel token: %d", code)
	}
	var settings struct {
		JoinToken string `json:"join_token"`
	}
	h.do("GET", "/api/settings", adminToken, nil, &settings)
	var reg api.RegisterResponse
	h.do("POST", "/api/agent/register", "", api.RegisterRequest{JoinToken: settings.JoinToken, Name: "pi-new", Setup: plan.ID, Host: api.HostInfo{Hostname: "pi-new"}}, &reg)
	var dev struct {
		Maintenance     bool   `json:"maintenance"`
		MaintenanceNote string `json:"maintenance_note"`
	}
	h.do("GET", "/api/devices/"+reg.DeviceID, adminToken, nil, &dev)
	if !dev.Maintenance || dev.MaintenanceNote != "burn-in" {
		t.Fatalf("plan not applied: %+v", dev)
	}
	// A plan is single use.
	var reg2 api.RegisterResponse
	h.do("POST", "/api/agent/register", "", api.RegisterRequest{JoinToken: settings.JoinToken, Name: "pi-two", Setup: plan.ID, Host: api.HostInfo{Hostname: "pi-two"}}, &reg2)
	h.do("GET", "/api/devices/"+reg2.DeviceID, adminToken, nil, &dev)
	if dev.Maintenance {
		t.Fatal("plan applied twice")
	}
}

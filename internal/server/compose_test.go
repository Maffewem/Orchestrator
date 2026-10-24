package server

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

const claudeUIStack = `services:
  claudeui:
    image: ghcr.io/example/claudeui:latest
    ports: ["3000:3000"]
    environment:
      API_KEY: ${API_KEY:?set API_KEY}
#  cloudflared:
#    image: cloudflare/cloudflared
#    environment:
#      TUNNEL_TOKEN: ${TUNNEL_TOKEN:?needed for the tunnel}
`

func TestComposeVars(t *testing.T) {
	if err := checkComposeVars(claudeUIStack, nil); err == nil || !strings.Contains(err.Error(), "API_KEY") || strings.Contains(err.Error(), "TUNNEL_TOKEN") {
		t.Fatalf("missing var: %v", err)
	}
	if err := checkComposeVars(claudeUIStack, []string{"API_KEY="}); err == nil {
		t.Fatal("empty value should fail ${VAR:?}")
	}
	if err := checkComposeVars(claudeUIStack, []string{"API_KEY=x"}); err != nil {
		t.Fatal(err)
	}
	if err := checkComposeVars("services:\n a:\n  image: x\n  user: ${U?no}\n", []string{"U="}); err != nil {
		t.Fatalf("${VAR?} accepts empty: %v", err)
	}
	if checkComposeFile("version: '3'") == nil || checkComposeFile("") == nil || checkComposeFile(claudeUIStack) != nil {
		t.Fatal("compose file validation")
	}
}

func TestComposeStackDeploy(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi-4")
	hb := func(caps []string) (resp api.HeartbeatResponse) {
		h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: caps}}, &resp)
		return resp
	}
	hb(nil)
	body := map[string]any{
		"name": "claudeui", "device_id": reg.DeviceID,
		"source":  map[string]any{"type": "compose", "compose": claudeUIStack},
		"runtime": map[string]any{"env": []string{"API_KEY=abc"}, "ports": []string{"1:1"}},
	}
	var p ProjectView
	if code := h.do("POST", "/api/projects", adminToken, body, &p); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if len(p.Runtime.Ports) != 0 {
		t.Fatalf("stack ports should be dropped: %v", p.Runtime.Ports)
	}
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("old agent should be refused: %d", code)
	}
	hb([]string{api.CapCompose})
	var dep struct {
		ID    string
		Steps []struct{ Name string }
	}
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, &dep); code != http.StatusAccepted {
		t.Fatalf("deploy: %d", code)
	}
	resp := hb([]string{api.CapCompose})
	if len(resp.Commands) != 1 || resp.Commands[0].Deploy == nil || resp.Commands[0].Deploy.Compose == nil {
		t.Fatalf("commands: %+v", resp.Commands)
	}
	if c := resp.Commands[0].Deploy.Compose; c.Env[0] != "API_KEY=abc" || resp.Commands[0].Deploy.Service != "claudeui" {
		t.Fatalf("spec: %+v", resp.Commands[0].Deploy)
	}
	var names []string
	for _, s := range dep.Steps {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "Initializing,Pulling image,Deploying,Verifying" {
		t.Fatalf("steps: %v", names)
	}

	if code := h.do("POST", "/api/projects/"+p.ID+"/stack", adminToken, map[string]any{"action": "stop"}, nil); code != http.StatusAccepted {
		t.Fatalf("stop: %d", code)
	}
	if code := h.do("POST", "/api/projects/"+p.ID+"/stack", adminToken, map[string]any{"action": "nope"}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad action: %d", code)
	}
	// Missing required variable is reported before anything is queued.
	body["runtime"] = map[string]any{"env": []string{}}
	if code := h.do("PATCH", "/api/projects/"+p.ID, adminToken, body, nil); code != http.StatusOK {
		t.Fatalf("patch: %d", code)
	}
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("missing var: %d", code)
	}
}

func TestTunnelSetup(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi-4")
	hb := func(caps []string) {
		h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: caps}}, nil)
	}
	hb(nil)
	path := "/api/devices/" + reg.DeviceID + "/tunnel"
	tok := base64.StdEncoding.EncodeToString([]byte(`{"a":"acct","t":"tun-1","s":"c2VjcmV0"}`))
	if code := h.do("PUT", path, adminToken, map[string]any{"token": tok}, nil); code != http.StatusConflict {
		t.Fatalf("old agent: %d", code)
	}
	hb([]string{api.CapTunnel})
	if code := h.do("PUT", path, adminToken, map[string]any{"token": "garbage"}, nil); code != http.StatusBadRequest {
		t.Fatalf("garbage token: %d", code)
	}
	var v tunnelView
	// Pasting the whole connector command works.
	if code := h.do("PUT", path, adminToken, map[string]any{"token": "cloudflared service install " + tok}, &v); code != http.StatusAccepted || !v.Enabled || v.TunnelID != "tun-1" {
		t.Fatalf("enable: %d %+v", code, v)
	}
	var resp api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: []string{api.CapTunnel}}}, &resp)
	if len(resp.Commands) != 1 || resp.Commands[0].Action != api.ActionTunnelSync || resp.Commands[0].Args["token"] != tok {
		t.Fatalf("commands: %+v", resp.Commands)
	}
	// The token is never returned to the browser.
	var raw map[string]any
	h.do("GET", path, adminToken, nil, &raw)
	if _, leaked := raw["token"]; leaked {
		t.Fatal("token leaked")
	}
	routes := map[string]any{"routes": []map[string]string{{"hostname": "chat.example.com", "service": "http://localhost:3000"}}}
	if code := h.do("PUT", path+"/routes", adminToken, routes, &v); code != http.StatusOK || len(v.Routes) != 1 || len(v.Warnings) == 0 {
		t.Fatalf("routes without Cloudflare: %d %+v", code, v)
	}
	routes["routes"] = []map[string]string{{"hostname": "bad host", "service": "x"}}
	if code := h.do("PUT", path+"/routes", adminToken, routes, nil); code != http.StatusBadRequest {
		t.Fatalf("bad route: %d", code)
	}
	if code := h.do("PUT", path, adminToken, map[string]any{"token": ""}, &v); code != http.StatusOK || v.Enabled {
		t.Fatalf("disable: %d %+v", code, v)
	}
}

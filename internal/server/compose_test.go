package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
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

func TestComposeStackFromGitHub(t *testing.T) {
	yaml := "services:\n  app:\n    build: .\n    image: app\n"
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/Maffewem/Stack/commits/"):
			json.NewEncoder(w).Encode(map[string]any{"sha": "abc1234def", "commit": map[string]any{"message": "update", "author": map[string]any{"name": "m"}}})
		case r.URL.Path == "/repos/Maffewem/Stack/contents/deploy/compose.yml" && r.URL.Query().Get("ref") == "abc1234def":
			json.NewEncoder(w).Encode(map[string]any{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(yaml))})
		default:
			http.NotFound(w, r)
		}
	}))
	defer gh.Close()
	h := newHarness(t)
	h.srv.githubAPI = gh.URL
	reg := h.register("pi-4")
	var resp api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: []string{api.CapCompose}}}, &resp)
	var p ProjectView
	body := map[string]any{
		"name": "stack", "device_id": reg.DeviceID,
		"source": map[string]any{"type": "compose", "repo": "Maffewem/Stack", "branch": "main", "compose_file": "/deploy/compose.yml", "auto_deploy": true, "compose": "ignored"},
	}
	if code := h.do("POST", "/api/projects", adminToken, body, &p); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if p.Source.Compose != "" || p.Source.ComposeFile != "deploy/compose.yml" {
		t.Fatalf("source: %+v", p.Source)
	}
	var dep struct{ Steps []struct{ Name string } }
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, &dep); code != http.StatusAccepted {
		t.Fatalf("deploy: %d", code)
	}
	h.srv.githubToken(context.Background())
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: []string{api.CapCompose}}}, &resp)
	if len(resp.Commands) != 1 {
		t.Fatalf("commands: %+v", resp.Commands)
	}
	c := resp.Commands[0].Deploy.Compose
	if c == nil || c.YAML != yaml || c.Remote != "https://github.com/Maffewem/Stack.git#abc1234def" {
		t.Fatalf("compose spec: %+v", c)
	}
	var names []string
	for _, s := range dep.Steps {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "Initializing,Cloning,Pulling image,Deploying,Verifying" {
		t.Fatalf("steps: %v", names)
	}
	// Credentials are added on the way out, never stored.
	cmds := []api.Command{{Deploy: &api.DeploySpec{Compose: &api.ComposeSpec{Remote: c.Remote}}}}
	withCredentials(cmds, "tok")
	if !strings.Contains(cmds[0].Deploy.Compose.Remote, "x-access-token:tok@github.com/Maffewem/Stack.git#abc1234def") || strings.Contains(c.Remote, "tok") {
		t.Fatalf("credentials: %s", cmds[0].Deploy.Compose.Remote)
	}
	// Pushes to the branch find the project.
	ps, _ := h.srv.store.ProjectsForPush(context.Background(), "Maffewem/Stack", "main")
	if len(ps) != 1 {
		t.Fatalf("push projects: %d", len(ps))
	}
}

func TestProjectTunnelRoutes(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi-4")
	caps := []string{api.CapTunnel, api.CapCompose}
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
		Host: api.HostInfo{Capabilities: caps},
		Containers: []api.Container{{ID: "c1", Name: "claudeui-web-1", State: "running", ComposeProject: "claudeui",
			Ports: []api.Port{{PrivatePort: 3000, PublicPort: 3010, Type: "tcp"}, {PrivatePort: 53, PublicPort: 5353, Type: "udp"}}}},
	}, nil)
	var p ProjectView
	h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "claudeui", "device_id": reg.DeviceID,
		"source": map[string]any{"type": "compose", "compose": claudeUIStack},
	}, &p)
	path := "/api/projects/" + p.ID + "/tunnel"
	routes := map[string]any{"routes": []map[string]any{{"hostname": "chat.example.com", "port": 3010}}}
	if code := h.do("PUT", path, adminToken, routes, nil); code != http.StatusConflict {
		t.Fatalf("no tunnel yet: %d", code)
	}
	tok := base64.StdEncoding.EncodeToString([]byte(`{"a":"acct","t":"tun-1","s":"x"}`))
	h.do("PUT", "/api/devices/"+reg.DeviceID+"/tunnel", adminToken, map[string]any{"token": tok}, nil)
	// A route set on the device page must survive the project saving its own.
	h.do("PUT", "/api/devices/"+reg.DeviceID+"/tunnel/routes", adminToken,
		map[string]any{"routes": []map[string]string{{"hostname": "other.example.com", "service": "http://localhost:9000"}}}, nil)
	var v projectTunnelView
	if code := h.do("PUT", path, adminToken, routes, &v); code != http.StatusOK {
		t.Fatalf("set: %d", code)
	}
	if len(v.Routes) != 1 || v.Routes[0].Service != "http://localhost:3010" || v.Routes[0].Project != p.ID {
		t.Fatalf("project routes: %+v", v.Routes)
	}
	if len(v.Ports) != 1 || v.Ports[0] != 3010 {
		t.Fatalf("ports (tcp only, from the stack's containers): %v", v.Ports)
	}
	var dev tunnelView
	h.do("GET", "/api/devices/"+reg.DeviceID+"/tunnel", adminToken, nil, &dev)
	if len(dev.Routes) != 2 {
		t.Fatalf("device routes: %+v", dev.Routes)
	}
	routes["routes"] = []map[string]any{{"hostname": "other.example.com", "port": 3010}}
	if code := h.do("PUT", path, adminToken, routes, nil); code != http.StatusBadRequest {
		t.Fatalf("hostname owned elsewhere: %d", code)
	}
	routes["routes"] = []map[string]any{}
	h.do("PUT", path, adminToken, routes, &v)
	h.do("GET", "/api/devices/"+reg.DeviceID+"/tunnel", adminToken, nil, &dev)
	if len(v.Routes) != 0 || len(dev.Routes) != 1 {
		t.Fatalf("after clearing: %+v / %+v", v.Routes, dev.Routes)
	}
}

func TestProjectPortsEmptyIsList(t *testing.T) {
	// A nil slice would serialise as null and crash the Tunnel card.
	if got := projectPorts(&store.Project{}, &store.Device{}); got == nil {
		t.Fatal("projectPorts returned nil")
	}
}

func TestUpstreamPortWebPort(t *testing.T) {
	p := &store.Project{Runtime: store.Runtime{Ports: []string{"502:502", "9080:80"}}}
	if got := upstreamPort(p); got != "502" {
		t.Fatalf("default is the first port: %q", got)
	}
	p.Runtime.WebPort = "9080"
	if got := upstreamPort(p); got != "9080" {
		t.Fatalf("chosen web port: %q", got)
	}
}

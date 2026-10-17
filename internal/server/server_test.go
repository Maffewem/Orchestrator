package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

const adminToken = "test-admin-token"

type harness struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
	st  *store.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, _, err := New(context.Background(), st, Config{AdminToken: adminToken, HeartbeatInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{t: t, srv: srv, ts: ts, st: st}
}

func (h *harness) do(method, path, token string, body any, out any) int {
	h.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, h.ts.URL+path, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			h.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func (h *harness) register(name string) api.RegisterResponse {
	h.t.Helper()
	var settings struct {
		JoinToken string `json:"join_token"`
	}
	h.do("GET", "/api/settings", adminToken, nil, &settings)
	var reg api.RegisterResponse
	code := h.do("POST", "/api/agent/register", "", api.RegisterRequest{
		JoinToken: settings.JoinToken, Name: name, Host: api.HostInfo{Hostname: name, Arch: "arm64"},
	}, &reg)
	if code != http.StatusCreated {
		h.t.Fatalf("register: %d", code)
	}
	return reg
}

func TestAuth(t *testing.T) {
	h := newHarness(t)
	if code := h.do("GET", "/api/devices", "", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code := h.do("GET", "/api/devices", "wrong", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", code)
	}
	if code := h.do("POST", "/api/agent/register", "", api.RegisterRequest{JoinToken: "nope"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("bad join token: %d", code)
	}
	if code := h.do("POST", "/api/agent/heartbeat", "nope", api.HeartbeatRequest{}, nil); code != http.StatusUnauthorized {
		t.Fatalf("bad agent token: %d", code)
	}
}

func TestDeviceLifecycleAndContainerCommand(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi-kitchen")

	hb := api.HeartbeatRequest{
		Host:       api.HostInfo{Hostname: "pi-kitchen", Docker: &api.DockerInfo{Version: "27.0"}},
		Metrics:    api.Metrics{MemoryUsed: 50, MemoryTotal: 100},
		Containers: []api.Container{{ID: "abc123", Name: "web", State: "running"}},
	}
	var res api.HeartbeatResponse
	if code := h.do("POST", "/api/agent/heartbeat", reg.AgentToken, hb, &res); code != 200 {
		t.Fatalf("heartbeat: %d", code)
	}

	var devices []DeviceView
	h.do("GET", "/api/devices", adminToken, nil, &devices)
	if len(devices) != 1 || devices[0].Status != "online" || devices[0].Health != "ok" || len(devices[0].Containers) != 1 {
		t.Fatalf("unexpected devices: %+v", devices)
	}

	var cmd api.Command
	code := h.do("POST", "/api/devices/"+reg.DeviceID+"/commands", adminToken,
		map[string]string{"action": api.ActionRestart, "target": "abc123"}, &cmd)
	if code != http.StatusAccepted {
		t.Fatalf("create command: %d", code)
	}
	if code := h.do("POST", "/api/devices/"+reg.DeviceID+"/commands", adminToken,
		map[string]string{"action": api.ActionRestart, "target": "unknown"}, nil); code != http.StatusBadRequest {
		t.Fatalf("unknown container should be rejected: %d", code)
	}

	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, hb, &res)
	if len(res.Commands) != 1 || res.Commands[0].ID != cmd.ID {
		t.Fatalf("command not dispatched: %+v", res.Commands)
	}
	h.do("POST", "/api/agent/commands/"+cmd.ID+"/result", reg.AgentToken, api.CommandResult{OK: true}, nil)
	h.do("GET", "/api/commands/"+cmd.ID, adminToken, nil, &cmd)
	if cmd.Status != api.CommandSucceeded {
		t.Fatalf("command status = %s", cmd.Status)
	}

	// A state change becomes an activity event.
	hb.Containers[0].State = "exited"
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, hb, &res)
	var events []store.Event
	h.do("GET", "/api/events?device="+reg.DeviceID, adminToken, nil, &events)
	found := false
	for _, e := range events {
		if e.Kind == "container.state" && strings.Contains(e.Message, "exited") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no container.state event in %+v", events)
	}

	var samples []MetricSample
	h.do("GET", "/api/devices/"+reg.DeviceID+"/metrics", adminToken, nil, &samples)
	if len(samples) != 3 || samples[0].MemPercent != 50 {
		t.Fatalf("metrics history: %+v", samples)
	}
}

func TestImageProjectDeployment(t *testing.T) {
	h := newHarness(t)
	reg := h.register("vm-1")

	var p ProjectView
	code := h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "pihole", "device_id": reg.DeviceID,
		"source":  map[string]any{"type": "image", "image": "pihole/pihole:latest"},
		"runtime": map[string]any{"ports": []string{"8081:80", "53:53/udp"}, "env": []string{"TZ=Europe/London"}},
	}, &p)
	if code != http.StatusCreated {
		t.Fatalf("create project: %d", code)
	}
	if code := h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "Bad Name", "device_id": reg.DeviceID, "source": map[string]any{"type": "image", "image": "x"},
	}, nil); code != http.StatusBadRequest {
		t.Fatalf("invalid name accepted: %d", code)
	}

	var dep store.Deployment
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, &dep); code != http.StatusAccepted {
		t.Fatalf("deploy: %d", code)
	}
	if dep.Status != store.DeploymentQueued || len(dep.Steps) != 4 || dep.Steps[1].Name != "Pulling image" {
		t.Fatalf("unexpected deployment: %+v", dep)
	}

	var res api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{}, &res)
	if len(res.Commands) != 1 || res.Commands[0].Action != api.ActionDeploy || res.Commands[0].Deploy.Image != "pihole/pihole:latest" {
		t.Fatalf("deploy command not dispatched: %+v", res.Commands)
	}
	cmd := res.Commands[0]
	if cmd.Deploy.Service != "pihole" || cmd.Deploy.Restart != "unless-stopped" || len(cmd.Deploy.Ports) != 2 {
		t.Fatalf("spec: %+v", cmd.Deploy)
	}

	step := func(idx int, status, line string) {
		u := api.StepUpdate{Status: status}
		if line != "" {
			u.Lines = []api.LogLine{{Time: time.Now(), Text: line}}
		}
		if code := h.do("POST", "/api/agent/deployments/"+dep.ID+"/steps/"+string(rune('0'+idx)), reg.AgentToken, u, nil); code != http.StatusNoContent {
			t.Fatalf("step update %d: %d", idx, code)
		}
	}
	step(0, api.StepRunning, "Initializing")
	step(0, api.StepSucceeded, "")
	step(1, api.StepRunning, "Pulling pihole/pihole:latest")
	step(1, "", "abc: Pull complete")

	h.do("GET", "/api/deployments/"+dep.ID, adminToken, nil, &dep)
	if dep.Status != store.DeploymentRunning || dep.StartedAt == nil || dep.Steps[1].Status != api.StepRunning ||
		!strings.Contains(dep.Steps[1].Log, "Pull complete") {
		t.Fatalf("mid-deploy state: %+v", dep)
	}

	h.do("POST", "/api/agent/commands/"+cmd.ID+"/result", reg.AgentToken, api.CommandResult{Error: "pull failed"}, nil)
	h.do("GET", "/api/deployments/"+dep.ID, adminToken, nil, &dep)
	if dep.Status != store.DeploymentFailed || dep.Error != "pull failed" ||
		dep.Steps[1].Status != api.StepFailed || dep.Steps[2].Status != api.StepSkipped {
		t.Fatalf("final state: %+v", dep)
	}

	var retry store.Deployment
	if code := h.do("POST", "/api/deployments/"+dep.ID+"/retry", adminToken, nil, &retry); code != http.StatusAccepted {
		t.Fatalf("retry: %d", code)
	}
	if retry.Trigger != store.TriggerRetry {
		t.Fatalf("retry trigger = %s", retry.Trigger)
	}
	var projects []ProjectView
	h.do("GET", "/api/projects", adminToken, nil, &projects)
	if len(projects) != 1 || projects[0].Latest == nil || projects[0].Latest.ID != retry.ID || projects[0].DeviceName != "vm-1" {
		t.Fatalf("projects: %+v", projects)
	}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestGitHubPushDeploys(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi-2")
	h.st.SetSetting(context.Background(), settingGitHubToken, "ghp_secret")

	var p ProjectView
	code := h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "aurora", "device_id": reg.DeviceID,
		"source": map[string]any{"type": "github", "repo": "Maffewem/Aurora", "branch": "main", "context": "app", "auto_deploy": true},
	}, &p)
	if code != http.StatusCreated {
		t.Fatalf("create project: %d", code)
	}

	var settings struct {
		GitHub struct {
			WebhookSecret string `json:"webhook_secret"`
		} `json:"github"`
	}
	h.do("GET", "/api/settings", adminToken, nil, &settings)

	push := func(ref string, sig func([]byte) string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]any{
			"ref":        ref,
			"repository": map[string]any{"full_name": "maffewem/aurora"},
			"head_commit": map[string]any{
				"id": "ef9f3ea1234567890", "message": "Same header on every page", "url": "https://github.com/x",
				"author": map[string]any{"name": "Matthew", "username": "Maffewem"},
			},
		})
		req, _ := http.NewRequest("POST", h.ts.URL+"/api/github/webhook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", sig(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	if code, _ := push("refs/heads/main", func(b []byte) string { return sign("wrong", b) }); code != http.StatusUnauthorized {
		t.Fatalf("bad signature accepted: %d", code)
	}
	good := func(b []byte) string { return sign(settings.GitHub.WebhookSecret, b) }
	if _, out := push("refs/heads/other", good); len(out["deployments"].([]any)) != 0 {
		t.Fatalf("other branch deployed: %+v", out)
	}
	code, out := push("refs/heads/main", good)
	if code != http.StatusAccepted || len(out["deployments"].([]any)) != 1 {
		t.Fatalf("push: %d %+v", code, out)
	}

	var deps []store.Deployment
	h.do("GET", "/api/projects/"+p.ID+"/deployments", adminToken, nil, &deps)
	if len(deps) != 1 || deps[0].Trigger != store.TriggerPush || deps[0].Commit.Author != "Maffewem" || deps[0].Commit.Branch != "main" {
		t.Fatalf("deployments: %+v", deps)
	}
	if strings.Contains(deps[0].Spec.Build.Remote, "ghp_secret") {
		t.Fatal("token stored with the deployment")
	}

	var res api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{}, &res)
	if len(res.Commands) != 1 {
		t.Fatalf("commands: %+v", res.Commands)
	}
	b := res.Commands[0].Deploy.Build
	want := "https://x-access-token:ghp_secret@github.com/Maffewem/Aurora.git#ef9f3ea1234567890:app"
	if b == nil || b.Remote != want {
		t.Fatalf("remote = %+v, want %s", b, want)
	}
	if res.Commands[0].Deploy.Image != "orchestrator/aurora:ef9f3ea" {
		t.Fatalf("image = %s", res.Commands[0].Deploy.Image)
	}
}

func TestOfflineSweep(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi-3")
	ctx := context.Background()
	// Nothing is offline yet.
	h.srv.sweep(ctx)
	gone, _ := h.st.MarkOffline(ctx, time.Now().Add(time.Hour))
	if len(gone) != 1 || gone[0].ID != reg.DeviceID {
		t.Fatalf("mark offline: %+v", gone)
	}
	var d DeviceView
	h.do("GET", "/api/devices/"+reg.DeviceID, adminToken, nil, &d)
	if d.Status != "offline" || d.Health != "critical" {
		t.Fatalf("device: %+v", d)
	}
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Docker: &api.DockerInfo{}}}, nil)
	h.do("GET", "/api/devices/"+reg.DeviceID, adminToken, nil, &d)
	if d.Status != "online" {
		t.Fatalf("device did not come back: %+v", d)
	}
}

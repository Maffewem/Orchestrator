package server

import (
	"net/http"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestDockerPruneIsAdminOnlyAndConfirmed(t *testing.T) {
	h := newHarness(t)
	var settings struct {
		JoinToken string       `json:"join_token"`
		DockerGC  dockerGCView `json:"docker_gc"`
	}
	h.do("GET", "/api/settings", adminToken, nil, &settings)
	if settings.DockerGC.Keep != 3 || settings.DockerGC.Mode != "off" {
		t.Fatalf("defaults: %+v", settings.DockerGC)
	}
	var reg api.RegisterResponse
	h.do("POST", "/api/agent/register", "", api.RegisterRequest{JoinToken: settings.JoinToken, Name: "pi",
		Host: api.HostInfo{Hostname: "pi", Arch: "arm64", Capabilities: []string{api.CapDockerGC}}}, &reg)
	base := "/api/devices/" + reg.DeviceID
	for _, role := range []string{"viewer", "operator"} {
		tok := h.makeUser("g-"+role, role)
		if code := h.do("GET", base+"/docker", tok, nil, nil); code != http.StatusForbidden {
			t.Errorf("%s report: %d", role, code)
		}
		if code := h.do("POST", base+"/docker/prune", tok, map[string]any{"dangling": true, "confirm": "prune"}, nil); code != http.StatusForbidden {
			t.Errorf("%s prune: %d", role, code)
		}
		if code := h.do("PUT", "/api/settings/docker-gc", tok, defaultDockerGCPolicy(), nil); code != http.StatusForbidden {
			t.Errorf("%s policy: %d", role, code)
		}
	}
	if code := h.do("POST", base+"/commands", adminToken, map[string]any{"action": api.ActionDockerPrune}, nil); code != http.StatusBadRequest {
		t.Errorf("generic command: %d", code)
	}
	for _, body := range []map[string]any{
		{"dangling": true},                   // no confirmation
		{"dangling": true, "confirm": "yes"}, // wrong confirmation
		{"confirm": "prune"},                 // nothing chosen
		{"build_cache": true, "cache_days": -1, "confirm": "prune"},
	} {
		if code := h.do("POST", base+"/docker/prune", adminToken, body, nil); code != http.StatusBadRequest {
			t.Errorf("%v accepted: %d", body, code)
		}
	}
	var cmd api.Command
	if code := h.do("POST", base+"/docker/prune", adminToken, map[string]any{"trim": true, "dangling": true, "confirm": "prune"}, &cmd); code != http.StatusAccepted {
		t.Fatalf("prune: %d", code)
	}
	if cmd.Action != api.ActionDockerPrune || cmd.Args["keep"] != "3" || cmd.Args["trim"] != "1" || cmd.Args["unused"] != "" || cmd.Args["by"] != "admin token" {
		t.Errorf("command: %+v", cmd)
	}
	bad := defaultDockerGCPolicy()
	bad.Keep = -1
	if code := h.do("PUT", "/api/settings/docker-gc", adminToken, bad, nil); code != http.StatusBadRequest {
		t.Errorf("bad policy: %d", code)
	}
	p := defaultDockerGCPolicy()
	p.Keep, p.MaxGB, p.Mode = 5, 20, "weekly"
	var v dockerGCView
	if code := h.do("PUT", "/api/settings/docker-gc", adminToken, p, &v); code != http.StatusOK || v.Keep != 5 || v.NextRun == nil {
		t.Errorf("save: %d %+v", code, v)
	}
}

func TestKeepImagesOnlyForBuiltWorkers(t *testing.T) {
	s := &Server{}
	spec := api.DeploySpec{Image: "nginx"}
	s.applyKeepImages(nil, &spec) //nolint
	if spec.KeepImages != 0 {
		t.Errorf("pulled image got keep %d", spec.KeepImages)
	}
}

func TestImageByDigest(t *testing.T) {
	d := "sha256:abc"
	for in, want := range map[string]string{
		"nginx:latest":             "nginx@sha256:abc",
		"nginx":                    "nginx@sha256:abc",
		"localhost:5000/app:v2":    "localhost:5000/app@sha256:abc",
		"localhost:5000/app":       "localhost:5000/app@sha256:abc",
		"ghcr.io/o/app@sha256:xyz": "ghcr.io/o/app@sha256:xyz",
	} {
		if got := imageByDigest(in, d); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

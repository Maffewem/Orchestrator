package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/store"
)

func TestPollerDeploysNewCommits(t *testing.T) {
	var sha atomic.Value
	sha.Store("aaaaaaa1111111")
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"sha": sha.Load(), "html_url": "https://github.com/x",
			"commit": map[string]any{"message": "m", "author": map[string]any{"name": "Matthew"}},
		})
	}))
	defer gh.Close()

	h := newHarness(t)
	h.srv.githubAPI = gh.URL
	h.st.SetSetting(context.Background(), settingGitHubToken, "ghp_secret")
	reg := h.register("pi-3")

	var p ProjectView
	code := h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "aurora", "device_id": reg.DeviceID,
		"source": map[string]any{"type": "github", "repo": "Maffewem/Aurora", "branch": "main", "auto_deploy": true},
	}, &p)
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	count := func() ([]*store.Deployment, int) {
		ds, err := h.st.ListDeployments(context.Background(), store.DeploymentFilter{ProjectID: p.ID})
		if err != nil {
			t.Fatal(err)
		}
		return ds, len(ds)
	}

	ctx := context.Background()
	h.srv.pollRepos(ctx)
	if _, n := count(); n != 1 {
		t.Fatalf("unchanged head redeployed: %d deployments", n)
	}

	sha.Store("bbbbbbb2222222")
	h.srv.pollRepos(ctx)
	ds, n := count()
	if n != 2 || ds[0].Trigger != store.TriggerPoll || ds[0].Commit.SHA != "bbbbbbb2222222" {
		t.Fatalf("new commit not deployed: %d %+v", n, ds[0])
	}
	h.srv.pollRepos(ctx)
	if _, n := count(); n != 2 {
		t.Fatalf("same commit deployed twice: %d", n)
	}
}

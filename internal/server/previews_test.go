package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

type fakePR struct {
	number      int
	branch, sha string
	repo, base  string
}

func prJSON(prs []fakePR) []map[string]any {
	out := []map[string]any{}
	for _, p := range prs {
		out = append(out, map[string]any{
			"number": p.number, "title": "PR " + p.branch, "html_url": "https://github.com/x",
			"user": map[string]any{"login": "matt"},
			"base": map[string]any{"ref": p.base},
			"head": map[string]any{"ref": p.branch, "sha": p.sha, "repo": map[string]any{"full_name": p.repo}},
		})
	}
	return out
}

func TestPreviewsFollowPullRequests(t *testing.T) {
	var mu sync.Mutex
	prs := []fakePR{
		{1, "feat/a", "aaaaaaa1", "Maffewem/Aurora", "main"},
		{2, "feat-b", "bbbbbbb1", "Maffewem/Aurora", "main"},
		{3, "fork-branch", "fffffff1", "stranger/Aurora", "main"},
		{4, "stacked", "ccccccc1", "Maffewem/Aurora", "feat/a"},
		{5, "feat/c", "ddddddd1", "Maffewem/Aurora", "main"},
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(prJSON(prs))
	}))
	defer gh.Close()

	h := newHarness(t)
	h.srv.githubAPI = gh.URL
	reg := h.register("pi-4")
	hb := func(caps []string) (resp api.HeartbeatResponse) {
		h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
			Host: api.HostInfo{Addresses: []string{"192.168.1.20"}, Capabilities: caps}}, &resp)
		return resp
	}
	hb(nil)

	var p ProjectView
	h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "aurora", "device_id": reg.DeviceID,
		"source":  map[string]any{"type": "github", "repo": "Maffewem/Aurora", "branch": "main"},
		"runtime": map[string]any{"ports": []string{"8081:80"}},
	}, &p)
	settings := map[string]any{"enabled": true, "max": 2}
	if code := h.do("PUT", "/api/projects/"+p.ID+"/previews/settings", adminToken, settings, nil); code != http.StatusConflict {
		t.Fatalf("old agent: %d", code)
	}
	hb([]string{api.CapPreview, api.CapProxy})
	var view previewsResponse
	if code := h.do("PUT", "/api/projects/"+p.ID+"/previews/settings", adminToken, settings, &view); code != http.StatusOK {
		t.Fatalf("enable: %d", code)
	}
	// Forks and PRs into other branches are skipped; max caps the rest, newest first.
	if len(view.Previews) != 2 || view.Previews[0].Branch == "fork-branch" {
		t.Fatalf("previews: %+v", view.Previews)
	}
	byBranch := map[string]previewView{}
	for _, v := range view.Previews {
		byBranch[v.Branch] = v
	}
	a, b := byBranch["feat/a"], byBranch["feat-b"]
	if a.Slug != "feat-a" || b.Slug != "feat-b" || a.HostPort < 20000 || a.HostPort == b.HostPort {
		t.Fatalf("slug/port: %+v %+v", a.Preview, b.Preview)
	}
	if want := "http://192.168.1.20:" + itoa(a.HostPort); a.URL != want {
		t.Fatalf("url = %q, want %q", a.URL, want)
	}

	ctx := context.Background()
	dep, err := h.st.GetDeployment(ctx, a.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if dep.PreviewID != a.ID || dep.Spec.Service != "aurora--feat-a" || dep.Spec.Ports[0] != itoa(a.HostPort)+":80" ||
		dep.Spec.Labels["orchestrator.preview"] != "feat/a" || !strings.HasSuffix(dep.Spec.Build.Remote, "#aaaaaaa1") {
		t.Fatalf("preview deployment: %+v", dep)
	}
	prod, _ := h.st.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID})
	if len(prod) != 0 {
		t.Fatalf("previews leaked into production deployments: %d", len(prod))
	}

	// A push to the branch rebuilds the same preview.
	mu.Lock()
	prs[0].sha = "aaaaaaa2"
	mu.Unlock()
	proj, _ := h.st.GetProject(ctx, p.ID)
	h.srv.syncPreviews(ctx, proj)
	h.srv.syncPreviews(ctx, proj) // unchanged: no second build
	all, _ := h.st.ListDeployments(ctx, store.DeploymentFilter{ProjectID: p.ID, Preview: a.ID})
	if len(all) != 2 || all[0].Commit.SHA != "aaaaaaa2" {
		t.Fatalf("rebuild: %d deployments", len(all))
	}

	// Retrying a preview deployment stays a preview.
	var retry store.Deployment
	if code := h.do("POST", "/api/deployments/"+all[1].ID+"/retry", adminToken, nil, &retry); code != http.StatusAccepted || retry.PreviewID != a.ID {
		t.Fatalf("retry: %d %+v", code, retry)
	}
	// ...and can't be rolled back to.
	h.st.FinishDeployment(ctx, all[1].ID, true, "")
	if code := h.do("POST", "/api/deployments/"+all[1].ID+"/rollback", adminToken, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("rollback to preview: %d", code)
	}

	// Closing the PR removes the container and frees a slot for the next PR.
	mu.Lock()
	prs = prs[1:]
	mu.Unlock()
	h.srv.syncPreviews(ctx, proj)
	left, _ := h.st.ListPreviews(ctx, p.ID)
	if len(left) != 2 || left[0].Branch != "feat/c" && left[1].Branch != "feat/c" {
		t.Fatalf("after close: %+v", left)
	}
	removed := false
	for _, c := range hb([]string{api.CapPreview, api.CapProxy}).Commands {
		if c.Action == api.ActionRemove && c.Target == "aurora--feat-a" {
			removed = true
		}
	}
	if !removed {
		t.Fatal("no container.remove for the closed PR")
	}

	// Turning previews off removes the rest.
	h.do("PUT", "/api/projects/"+p.ID+"/previews/settings", adminToken, map[string]any{"enabled": false, "max": 2}, &view)
	if len(view.Previews) != 0 {
		t.Fatalf("still has previews: %+v", view.Previews)
	}
}

func TestPreviewHostsInCaddyfile(t *testing.T) {
	p := &store.Project{ID: "p1", Name: "blog", Runtime: store.Runtime{Ports: []string{"8081:80"}}}
	pv := map[string][]*store.Preview{"p1": {{Slug: "feat-a", HostPort: 20123}}}
	on := map[string]urlPrefs{"p1": {Production: true, Preview: true}}
	got, _ := buildCaddyfile("example.com", "", []*store.Project{p}, nil, on, nil, pv)
	for _, want := range []string{"\nfeat-a-blog.example.com {", "reverse_proxy 127.0.0.1:20123", "reverse_proxy 127.0.0.1:8081"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q:\n%s", want, got)
		}
	}
	off := map[string]urlPrefs{"p1": {Production: true}}
	if got, _ := buildCaddyfile("example.com", "", []*store.Project{p}, nil, off, nil, pv); strings.Contains(got, "feat-a") {
		t.Fatalf("preview served while switched off:\n%s", got)
	}
}

func TestRollback(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi-4")
	ctx := context.Background()
	var gp, ip ProjectView
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "aurora", "device_id": reg.DeviceID,
		"source": map[string]any{"type": "github", "repo": "Maffewem/Aurora", "branch": "main"}}, &gp)
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "web", "device_id": reg.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx:1.27"}}, &ip)

	proj, _ := h.st.GetProject(ctx, gp.ID)
	old, err := h.srv.deploy(ctx, proj, &store.Commit{Repo: "Maffewem/Aurora", Branch: "main", SHA: "1111111abc"}, store.TriggerPush)
	if err != nil {
		t.Fatal(err)
	}
	if code := h.do("POST", "/api/deployments/"+old.ID+"/rollback", adminToken, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("rollback to unfinished: %d", code)
	}
	h.st.FinishDeployment(ctx, old.ID, true, "")
	var d store.Deployment
	if code := h.do("POST", "/api/deployments/"+old.ID+"/rollback", adminToken, nil, &d); code != http.StatusAccepted {
		t.Fatalf("rollback: %d", code)
	}
	if d.Trigger != store.TriggerRollback || !d.Spec.Reuse || d.Commit.SHA != "1111111abc" || d.Spec.Image != old.Spec.Image {
		t.Fatalf("rollback deployment: %+v", d)
	}

	// An image project rolls back to the image the old deployment ran.
	iproj, _ := h.st.GetProject(ctx, ip.ID)
	iold, _ := h.srv.deploy(ctx, iproj, nil, store.TriggerManual)
	h.st.FinishDeployment(ctx, iold.ID, true, "")
	iproj.Source.Image = "nginx:1.28"
	h.st.UpdateProject(ctx, iproj)
	if code := h.do("POST", "/api/deployments/"+iold.ID+"/rollback", adminToken, nil, &d); code != http.StatusAccepted || d.Spec.Image != "nginx:1.27" {
		t.Fatalf("image rollback: %d %+v", code, d.Spec)
	}
}

func itoa(n int) string { return strings.TrimSpace(strings.Join([]string{strconv.Itoa(n)}, "")) }

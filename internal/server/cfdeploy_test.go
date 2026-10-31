package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "o-site-abc1234/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: "o-site-abc1234/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// cfFake plays GitHub and the Cloudflare API for the deployment tests.
type cfFake struct {
	t  *testing.T
	mu sync.Mutex

	gh, cf *httptest.Server

	pagesProjects map[string]string // name -> production branch
	manifests     []map[string]string
	deployFields  []map[string]string
	uploadedKeys  []string
	workerUploads map[string]string // script -> metadata JSON
	deleted       []string
	rollbacks     []string
	prs           []fakePR
	reqs, errs    int64
	pagesStage    string
	gitRepos      map[string]string // Pages project -> connected GitHub repository
}

func newCFFake(t *testing.T) *cfFake {
	f := &cfFake{t: t, pagesProjects: map[string]string{}, workerUploads: map[string]string{}, gitRepos: map[string]string{}, pagesStage: "success"}
	f.gh = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/commits/main"):
			json.NewEncoder(w).Encode(map[string]any{"sha": "abc1234def", "html_url": "https://github.com/o/site/commit/abc1234",
				"commit": map[string]any{"message": "Ship it\n\nbody", "author": map[string]any{"name": "Matt"}}})
		case strings.Contains(r.URL.Path, "/tarball/"):
			w.Write(tarball(t, map[string]string{"README.md": "# site"}))
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			f.mu.Lock()
			defer f.mu.Unlock()
			json.NewEncoder(w).Encode(prJSON(f.prs))
		default:
			t.Errorf("github: unexpected %s %s", r.Method, r.URL)
		}
	}))
	f.cf = httptest.NewServer(http.HandlerFunc(f.serveCF))
	t.Cleanup(func() { f.gh.Close(); f.cf.Close() })
	return f
}

func (f *cfFake) serveCF(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"success": true, "result": v}) }
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "not found"}}})
	}
	if r.URL.Path == "/site-live" || strings.HasPrefix(r.URL.Path, "/preview-") {
		io.WriteString(w, "hello")
		return
	}
	auth := r.Header.Get("Authorization")
	p := r.URL.Path
	switch {
	case p == "/graphql":
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"viewer": map[string]any{"accounts": []any{map[string]any{
			"workersInvocationsAdaptive": []any{map[string]any{"sum": map[string]any{"requests": f.reqs, "errors": f.errs, "subrequests": 3},
				"quantiles": map[string]any{"cpuTimeP50": 1200, "cpuTimeP99": 9000}}}}}}}})
	case p == "/accounts/acct/pages/projects" && r.Method == http.MethodGet:
		list := []any{}
		for name, branch := range f.pagesProjects {
			e := map[string]any{"name": name, "subdomain": name + ".pages.dev", "production_branch": branch}
			if repo := f.gitRepos[name]; repo != "" {
				o, n, _ := strings.Cut(repo, "/")
				e["source"] = map[string]any{"type": "github", "config": map[string]any{"owner": o, "repo_name": n, "production_branch": branch}}
			}
			list = append(list, e)
		}
		ok(list)
	case p == "/accounts/acct/workers/scripts" && r.Method == http.MethodGet:
		list := []any{}
		for name := range f.workerUploads {
			list = append(list, map[string]string{"id": name})
		}
		ok(list)
	case p == "/accounts/acct/workers/subdomain":
		ok(map[string]string{"subdomain": "matt"})
	case p == "/accounts/acct/pages/projects" && r.Method == http.MethodPost:
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		f.pagesProjects[b["name"]] = b["production_branch"]
		ok(b)
	case strings.HasPrefix(p, "/accounts/acct/pages/projects/") && strings.HasSuffix(p, "/upload-token"):
		ok(map[string]string{"jwt": "jwt1"})
	case p == "/pages/assets/check-missing":
		if auth != "Bearer jwt1" {
			f.t.Errorf("check-missing used %q", auth)
		}
		var b struct{ Hashes []string }
		json.NewDecoder(r.Body).Decode(&b)
		ok(b.Hashes)
	case p == "/pages/assets/upload":
		var b []struct{ Key string }
		json.NewDecoder(r.Body).Decode(&b)
		for _, u := range b {
			f.uploadedKeys = append(f.uploadedKeys, u.Key)
		}
		ok(map[string]int{"successful_key_count": len(b)})
	case p == "/pages/assets/upsert-hashes":
		ok(nil)
	case strings.HasSuffix(p, "/deployments") && r.Method == http.MethodPost && strings.Contains(p, "/pages/projects/"):
		if auth != "Bearer deploytok" {
			f.t.Errorf("deployment used %q", auth)
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			f.t.Errorf("multipart: %v", err)
		}
		fields := map[string]string{}
		for k, v := range r.MultipartForm.Value {
			fields[k] = v[0]
		}
		for k := range r.MultipartForm.File {
			fields["file:"+k] = "1"
		}
		f.deployFields = append(f.deployFields, fields)
		var m map[string]string
		json.Unmarshal([]byte(fields["manifest"]), &m)
		f.manifests = append(f.manifests, m)
		id, u := "d1", f.cf.URL+"/site-live"
		if fields["branch"] != "main" {
			id, u = "pv-"+strings.ReplaceAll(fields["branch"], "/", "-"), f.cf.URL+"/preview-"+strings.ReplaceAll(fields["branch"], "/", "-")
		}
		ok(map[string]string{"id": id, "url": u})
	case strings.HasSuffix(p, "/rollback"):
		f.rollbacks = append(f.rollbacks, p)
		ok(map[string]string{"id": "d1-again", "url": f.cf.URL + "/site-live"})
	case strings.HasPrefix(p, "/accounts/acct/pages/projects/") && r.Method == http.MethodGet:
		name := strings.TrimPrefix(p, "/accounts/acct/pages/projects/")
		if _, found := f.pagesProjects[name]; !found {
			notFound()
			return
		}
		ok(map[string]any{"name": name, "subdomain": name + ".pages.dev", "domains": []string{name + ".pages.dev", "www.example.com"},
			"latest_deployment": map[string]any{"id": "d1", "url": "x", "created_on": "2026-10-31T10:00:00Z",
				"latest_stage": map[string]string{"name": "deploy", "status": f.pagesStage}}})
	case strings.HasPrefix(p, "/accounts/acct/workers/scripts/") && strings.HasSuffix(p, "/subdomain"):
		ok(map[string]bool{"enabled": true})
	case strings.HasPrefix(p, "/accounts/acct/workers/scripts/") && strings.HasSuffix(p, "/deployments"):
		name := strings.TrimSuffix(strings.TrimPrefix(p, "/accounts/acct/workers/scripts/"), "/deployments")
		if _, found := f.workerUploads[name]; !found {
			notFound()
			return
		}
		ok(map[string]any{"deployments": []any{map[string]string{"id": "w-deploy-1", "created_on": "2026-10-31T10:05:00Z"}}})
	case strings.HasPrefix(p, "/accounts/acct/workers/scripts/") && r.Method == http.MethodPut:
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			f.t.Errorf("multipart: %v", err)
		}
		name := strings.TrimPrefix(p, "/accounts/acct/workers/scripts/")
		meta := r.MultipartForm.Value["metadata"]
		if len(meta) == 0 {
			// metadata is sent as a file-less part with a JSON content type; Go files it under Value
			f.t.Errorf("no metadata part: %v", r.MultipartForm.File)
		}
		f.workerUploads[name] = strings.Join(meta, "")
		ok(map[string]string{"id": name})
	case strings.HasPrefix(p, "/accounts/acct/workers/scripts/") && r.Method == http.MethodDelete:
		f.deleted = append(f.deleted, strings.TrimPrefix(p, "/accounts/acct/workers/scripts/"))
		ok(nil)
	default:
		f.t.Errorf("cloudflare: unexpected %s %s", r.Method, r.URL)
		notFound()
	}
}

func (f *cfFake) connect(h *harness) {
	h.srv.cloudflareAPI = f.cf.URL
	h.srv.githubAPI = f.gh.URL
	h.srv.cfWait = time.Millisecond
	h.srv.cfg.BuildDir = f.t.TempDir()
	h.srv.cfVerifyURL = func(string) string { return f.cf.URL + "/site-live" }
}

func waitDeployment(t *testing.T, h *harness, id string) store.Deployment {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var d store.Deployment
		h.do("GET", "/api/deployments/"+id, adminToken, nil, &d)
		if d.Status == store.DeploymentSucceeded || d.Status == store.DeploymentFailed {
			return d
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("deployment did not finish")
	return store.Deployment{}
}

func stepStatuses(d store.Deployment) string {
	var out []string
	for _, s := range d.Steps {
		out = append(out, s.Name+"="+s.Status)
	}
	return strings.Join(out, " ")
}

func TestCloudflarePagesDeployRollbackAndMonitor(t *testing.T) {
	f := newCFFake(t)
	h := newHarness(t)
	f.connect(h)

	project := map[string]any{
		"name": "site", "device_id": "ignored",
		"source": map[string]any{"type": "cloudflare", "repo": "o/site", "branch": "main",
			"cloudflare": map[string]any{"kind": "pages"},
			"node":       map[string]any{"build": "mkdir -p dist/img && echo '<h1>hi</h1>' > dist/index.html && echo body > dist/style.css && echo x > dist/img/a.png && echo '/* /index.html 200' > dist/_redirects"}},
		"runtime": map[string]any{"env": []string{"GREETING=hello"}},
	}
	var p ProjectView
	if code := h.do("POST", "/api/projects", adminToken, project, &p); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if p.DeviceID != "" || p.DeviceName != "Cloudflare" {
		t.Fatalf("a Cloudflare project has no device: %+v", p.Project)
	}

	// Not connected yet.
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("deploy before connecting: %d", code)
	}
	if code := h.do("PUT", "/api/settings/cloudflare/deploy", adminToken, map[string]string{"account_id": "acct", "token": "deploytok"}, nil); code != http.StatusOK {
		t.Fatalf("connect: %d", code)
	}

	var dep store.Deployment
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, &dep); code != http.StatusAccepted {
		t.Fatalf("deploy: %d", code)
	}
	dep = waitDeployment(t, h, dep.ID)
	if dep.Status != store.DeploymentSucceeded {
		t.Fatalf("deployment: %s %s\n%+v", dep.Status, dep.Error, dep.Steps)
	}
	want := "Cloning=succeeded Installing=skipped Building=succeeded Uploading=succeeded Verifying=succeeded"
	if got := stepStatuses(dep); got != want {
		t.Fatalf("steps:\n got %s\nwant %s", got, want)
	}
	if dep.RemoteID != "d1" || !strings.HasSuffix(dep.URL, "/site-live") || dep.CommandID != "" {
		t.Fatalf("remote: %+v", dep)
	}
	if !strings.Contains(dep.Steps[2].Log, "$ mkdir -p dist/img") {
		t.Fatalf("build log: %q", dep.Steps[2].Log)
	}

	f.mu.Lock()
	m, fields := f.manifests[0], f.deployFields[0]
	if f.pagesProjects["site"] != "main" || fields["branch"] != "main" || fields["commit_hash"] != "abc1234def" || fields["commit_message"] != "Ship it" {
		t.Errorf("project/fields: %v %v", f.pagesProjects, fields)
	}
	if fields["file:_redirects"] == "" {
		t.Errorf("_redirects should be sent as a file part: %v", fields)
	}
	if len(m) != 3 || m["/index.html"] == "" || m["/style.css"] == "" || m["/img/a.png"] == "" || m["/_redirects"] != "" {
		t.Errorf("manifest: %v", m)
	}
	for path, hash := range m {
		if len(hash) != 32 {
			t.Errorf("%s hash %q", path, hash)
		}
	}
	if len(f.uploadedKeys) != 3 {
		t.Errorf("uploaded %v", f.uploadedKeys)
	}
	f.mu.Unlock()

	// A second deployment of the same output re-uses what is uploaded; here every hash is
	// reported missing again by the fake, so just check rollback re-promotes without a build.
	var rb store.Deployment
	if code := h.do("POST", "/api/deployments/"+dep.ID+"/rollback", adminToken, nil, &rb); code != http.StatusAccepted {
		t.Fatalf("rollback: %d", code)
	}
	if rb.Status != store.DeploymentSucceeded || rb.RemoteID != "d1-again" || rb.Trigger != store.TriggerRollback {
		t.Fatalf("rollback result: %+v", rb)
	}
	f.mu.Lock()
	if len(f.rollbacks) != 1 || !strings.Contains(f.rollbacks[0], "/pages/projects/site/deployments/d1/rollback") || len(f.manifests) != 1 {
		t.Errorf("rollbacks %v, manifests %d", f.rollbacks, len(f.manifests))
	}
	f.mu.Unlock()

	// Monitoring.
	var st cfStatus
	if code := h.do("POST", "/api/projects/"+p.ID+"/cloudflare/refresh", adminToken, nil, &st); code != http.StatusOK {
		t.Fatalf("refresh: %d", code)
	}
	if st.State != cfStateLive || st.URL != "https://site.pages.dev" || st.Analytics != "none" || len(st.Domains) != 2 {
		t.Fatalf("status: %+v", st)
	}
	var pv ProjectView
	h.do("GET", "/api/projects/"+p.ID, adminToken, nil, &pv)
	if pv.Cloudflare == nil || pv.Cloudflare.State != cfStateLive {
		t.Fatalf("project view has no status: %+v", pv.Cloudflare)
	}
	f.mu.Lock()
	f.pagesStage = "failure"
	f.mu.Unlock()
	h.do("POST", "/api/projects/"+p.ID+"/cloudflare/refresh", adminToken, nil, &st)
	if st.State != cfStateFailed {
		t.Fatalf("failing deployment: %+v", st)
	}
	evs, _ := h.st.ListEvents(t.Context(), "", 20)
	found := false
	for _, e := range evs {
		found = found || e.Kind == "cloudflare.failed"
	}
	if !found {
		t.Errorf("no cloudflare.failed event: %+v", evs)
	}
	var ov overview
	h.do("GET", "/api/overview", adminToken, nil, &ov)
	if ov.Cloudflare.Projects != 1 || ov.Cloudflare.Failing != 1 {
		t.Fatalf("overview: %+v", ov.Cloudflare)
	}
}

func TestCloudflareWorkerFailureAndAnalytics(t *testing.T) {
	f := newCFFake(t)
	h := newHarness(t)
	f.connect(h)
	h.do("PUT", "/api/settings/cloudflare/deploy", adminToken, map[string]string{"account_id": "acct", "token": "deploytok"}, nil)

	mk := func(name, build string) ProjectView {
		var p ProjectView
		code := h.do("POST", "/api/projects", adminToken, map[string]any{
			"name": name,
			"source": map[string]any{"type": "cloudflare", "repo": "o/site", "branch": "main",
				"cloudflare": map[string]any{"kind": "worker", "compatibility_date": "2025-06-01"},
				"node":       map[string]any{"build": build}},
		}, &p)
		if code != http.StatusCreated {
			t.Fatalf("create %s: %d", name, code)
		}
		return p
	}
	edge := mk("edge", "mkdir -p dist && echo 'export default { fetch() { return new Response(\"ok\") } }' > dist/index.js")
	bad := mk("broken", "echo compiling && exit 3")

	var d store.Deployment
	h.do("POST", "/api/projects/"+edge.ID+"/deployments", adminToken, nil, &d)
	d = waitDeployment(t, h, d.ID)
	if d.Status != store.DeploymentSucceeded {
		t.Fatalf("worker deploy: %s %s %s", d.Status, d.Error, stepStatuses(d))
	}
	if d.URL != "https://edge.matt.workers.dev" || d.RemoteID != "w-deploy-1" {
		t.Fatalf("worker remote: %+v", d)
	}
	f.mu.Lock()
	meta := f.workerUploads["edge"]
	f.mu.Unlock()
	if !strings.Contains(meta, `"main_module":"index.js"`) || !strings.Contains(meta, `"compatibility_date":"2025-06-01"`) {
		t.Fatalf("worker metadata: %s", meta)
	}

	h.do("POST", "/api/projects/"+bad.ID+"/deployments", adminToken, nil, &d)
	d = waitDeployment(t, h, d.ID)
	if d.Status != store.DeploymentFailed || !strings.Contains(d.Error, "exited with status 3") {
		t.Fatalf("failing build: %s %s", d.Status, d.Error)
	}
	if got := stepStatuses(d); got != "Cloning=succeeded Installing=skipped Building=failed Uploading=skipped Verifying=skipped" {
		t.Fatalf("failed steps: %s", got)
	}
	if !strings.Contains(d.Steps[2].Log, "compiling") {
		t.Fatalf("build output missing: %q", d.Steps[2].Log)
	}

	f.mu.Lock()
	f.reqs, f.errs = 200, 40
	f.mu.Unlock()
	var st cfStatus
	h.do("POST", "/api/projects/"+edge.ID+"/cloudflare/refresh", adminToken, nil, &st)
	if st.State != cfStateLive || st.Requests != 200 || st.Errors != 40 || st.Analytics != "ok" || st.CPUP99 != 9000 || st.URL != "https://edge.matt.workers.dev" {
		t.Fatalf("worker status: %+v", st)
	}
	evs, _ := h.st.ListEvents(t.Context(), "", 30)
	n := 0
	for _, e := range evs {
		if e.Kind == "cloudflare.errors" {
			n++
		}
	}
	h.do("POST", "/api/projects/"+edge.ID+"/cloudflare/refresh", adminToken, nil, &st)
	evs, _ = h.st.ListEvents(t.Context(), "", 30)
	m := 0
	for _, e := range evs {
		if e.Kind == "cloudflare.errors" {
			m++
		}
	}
	if n != 1 || m != 1 {
		t.Fatalf("the high error rate should raise one event, got %d then %d", n, m)
	}
	// The never-deployed Worker is reported as missing, not as an error.
	h.do("POST", "/api/projects/"+bad.ID+"/cloudflare/refresh", adminToken, nil, &st)
	if st.State != cfStateMissing {
		t.Fatalf("undeployed worker: %+v", st)
	}
}

func TestCloudflarePreviewsFollowPullRequests(t *testing.T) {
	f := newCFFake(t)
	f.prs = []fakePR{{1, "feat/x", "1111111", "o/site", "main"}}
	h := newHarness(t)
	f.connect(h)
	h.do("PUT", "/api/settings/cloudflare/deploy", adminToken, map[string]string{"account_id": "acct", "token": "deploytok"}, nil)
	var p ProjectView
	h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "site",
		"source": map[string]any{"type": "cloudflare", "repo": "o/site", "branch": "main",
			"cloudflare": map[string]any{"kind": "pages"},
			"node":       map[string]any{"build": "mkdir -p dist && echo hi > dist/index.html"}},
	}, &p)

	var view previewsResponse
	if code := h.do("PUT", "/api/projects/"+p.ID+"/previews/settings", adminToken, map[string]any{"enabled": true, "max": 3}, &view); code != http.StatusOK {
		t.Fatalf("enable previews: %d", code)
	}
	if !view.Supported || len(view.Previews) != 1 {
		t.Fatalf("previews: %+v", view)
	}
	dep := waitDeployment(t, h, view.Previews[0].DeploymentID)
	if dep.Status != store.DeploymentSucceeded || dep.PreviewID == "" {
		t.Fatalf("preview deployment: %s %s", dep.Status, dep.Error)
	}
	f.mu.Lock()
	if got := f.deployFields[len(f.deployFields)-1]["branch"]; got != "feat/x" {
		t.Errorf("preview should deploy branch feat/x, got %q", got)
	}
	f.mu.Unlock()
	h.do("GET", "/api/projects/"+p.ID+"/previews", adminToken, nil, &view)
	if !strings.Contains(view.Previews[0].URL, "/preview-feat-x") || view.Previews[0].Status != store.DeploymentSucceeded {
		t.Fatalf("preview view: %+v", view.Previews[0])
	}
	// The pull request closes: the preview goes.
	f.mu.Lock()
	f.prs = nil
	f.mu.Unlock()
	h.srv.handlePullRequestEvent(t.Context(), "o/site")
	h.do("GET", "/api/projects/"+p.ID+"/previews", adminToken, nil, &view)
	if len(view.Previews) != 0 {
		t.Fatalf("preview should be gone: %+v", view.Previews)
	}
}

func TestCloudflareProjectValidationAndDeviceFreeEndpoints(t *testing.T) {
	h := newHarness(t)
	bad := []map[string]any{
		{"type": "cloudflare", "repo": "o/site"},
		{"type": "cloudflare", "repo": "o/site", "cloudflare": map[string]any{"kind": "lambda"}},
		{"type": "cloudflare", "repo": "o/site", "cloudflare": map[string]any{"kind": "pages", "output": "../x"}},
		{"type": "cloudflare", "repo": "o/site", "cloudflare": map[string]any{"kind": "pages", "name": "Bad Name"}},
		{"type": "cloudflare", "repo": "o/site", "cloudflare": map[string]any{"kind": "worker", "compatibility_date": "yesterday"}},
	}
	for i, src := range bad {
		if code := h.do("POST", "/api/projects", adminToken, map[string]any{"name": "x", "source": src}, nil); code != http.StatusBadRequest {
			t.Errorf("case %d: %d", i, code)
		}
	}
	var p ProjectView
	if code := h.do("POST", "/api/projects", adminToken, map[string]any{"name": "ok", "source": map[string]any{
		"type": "cloudflare", "repo": "o/site", "cloudflare": map[string]any{"kind": "pages"}}}, &p); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if p.Source.Branch != "main" {
		t.Errorf("branch defaults to main: %q", p.Source.Branch)
	}
	// Endpoints written for device projects must not break on a project without a device.
	for _, path := range []string{"", "/domains", "/previews", "/tunnel", "/migration", "/deployments", "/cloudflare"} {
		if code := h.do("GET", "/api/projects/"+p.ID+path, adminToken, nil, nil); code >= 500 {
			t.Errorf("GET %s: %d", path, code)
		}
	}
	if code := h.do("DELETE", "/api/projects/"+p.ID+"?remove_container=1", adminToken, nil, nil); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	// Disconnecting is allowed with nothing connected; a bad account is refused.
	if code := h.do("PUT", "/api/settings/cloudflare/deploy", adminToken, map[string]string{"account_id": "../x", "token": "t"}, nil); code != http.StatusBadRequest {
		t.Errorf("bad account id: %d", code)
	}
	if code := h.do("PUT", "/api/settings/cloudflare/deploy", adminToken, map[string]string{"account_id": ""}, nil); code != http.StatusOK {
		t.Errorf("disconnect: %d", code)
	}
}

func TestPagesHashMatchesWrangler(t *testing.T) {
	// BLAKE3 of the empty string, which is what an empty extensionless file hashes.
	if got := pagesHash(pagesFile{Path: "empty"}); got != "af1349b9f5f9a1a6a0404dea36dcc949" {
		t.Errorf("hash of an empty file: %s", got)
	}
	a, b := pagesHash(pagesFile{Path: "a.html", Content: []byte("x")}), pagesHash(pagesFile{Path: "a.css", Content: []byte("x")})
	if a == b || len(a) != 32 {
		t.Errorf("the extension is part of the hash: %s %s", a, b)
	}
}

func TestExtractTarballSkipsEscapesAndLinks(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(h tar.Header, body string) {
		h.Size = int64(len(body))
		tw.WriteHeader(&h)
		tw.Write([]byte(body))
	}
	add(tar.Header{Name: "top/ok.txt", Typeflag: tar.TypeReg, Mode: 0o644}, "ok")
	add(tar.Header{Name: "top/bin/run.sh", Typeflag: tar.TypeReg, Mode: 0o755}, "#!/bin/sh")
	add(tar.Header{Name: "top/../../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644}, "bad")
	add(tar.Header{Name: "top/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, "")
	tw.Close()
	gz.Close()

	root := t.TempDir()
	dir := filepath.Join(root, "src")
	n, err := extractTarball(&buf, dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("files written: %d", n)
	}
	if _, err := os.Stat(filepath.Join(root, "escape.txt")); err == nil {
		t.Error("a path outside the directory was written")
	}
	if _, err := os.Lstat(filepath.Join(dir, "link")); err == nil {
		t.Error("a symlink was created")
	}
	if st, _ := os.Stat(filepath.Join(dir, "bin", "run.sh")); st == nil || st.Mode()&0o100 == 0 {
		t.Error("the executable bit was lost")
	}
}

func TestBuildEnvHidesServerSecrets(t *testing.T) {
	t.Setenv("ORCH_ADMIN_TOKEN", "super-secret")
	h := newHarness(t)
	env := h.srv.cfBuildEnv(t.TempDir(), &api.CloudflareSpec{Branch: "main", SHA: "abc"}, []string{"A=1"})
	for _, e := range env {
		if strings.Contains(e, "super-secret") || strings.HasPrefix(e, "ORCH_") {
			t.Errorf("server secret leaked into the build environment: %s", e)
		}
	}
	if env[len(env)-1] != "A=1" {
		t.Errorf("project variables come last so they win: %v", env)
	}
}

func TestCloudflareDiscoversAndImportsExisting(t *testing.T) {
	f := newCFFake(t)
	f.pagesProjects["blog"], f.gitRepos["blog"] = "trunk", "o/blog"
	f.pagesProjects["Old_Docs"] = "main" // not connected to Git
	f.workerUploads["api"] = "{}"
	h := newHarness(t)
	f.connect(h)

	var none []cfFound
	h.do("GET", "/api/cloudflare/discover", adminToken, nil, &none)
	if len(none) != 0 {
		t.Fatalf("nothing is discovered before Cloudflare is connected: %+v", none)
	}
	h.do("PUT", "/api/settings/cloudflare/deploy", adminToken, map[string]string{"account_id": "acct", "token": "deploytok"}, nil)
	var found []cfFound
	h.do("GET", "/api/cloudflare/discover", adminToken, nil, &found)
	if len(found) != 3 {
		t.Fatalf("discovered: %+v", found)
	}
	byName := map[string]cfFound{}
	for _, x := range found {
		byName[x.Name] = x
	}
	if b := byName["blog"]; b.Repo != "o/blog" || b.Branch != "trunk" || b.URL != "https://blog.pages.dev" || b.Imported != "" {
		t.Fatalf("blog: %+v", b)
	}
	if a := byName["api"]; a.Kind != "worker" || a.URL != "https://api.matt.workers.dev" {
		t.Fatalf("api: %+v", a)
	}

	items := []map[string]string{{"kind": "pages", "name": "blog"}, {"kind": "worker", "name": "api"}, {"kind": "pages", "name": "Old_Docs"}, {"kind": "pages", "name": "ghost"}}
	var res []cfImportResult
	if code := h.do("POST", "/api/cloudflare/import", adminToken, map[string]any{"items": items}, &res); code != http.StatusOK {
		t.Fatalf("import: %d", code)
	}
	if res[0].Project != "blog" || res[1].Project != "api" || res[2].Project != "old-docs" || res[3].Error == "" {
		t.Fatalf("results: %+v", res)
	}
	var projects []ProjectView
	h.do("GET", "/api/projects", adminToken, nil, &projects)
	got := map[string]ProjectView{}
	for _, p := range projects {
		got[p.Name] = p
	}
	blog, api, docs := got["blog"], got["api"], got["old-docs"]
	if blog.Source.Repo != "o/blog" || blog.Source.Branch != "trunk" || blog.Source.AutoDeploy || blog.Cloudflare == nil || blog.Cloudflare.State != cfStateLive {
		t.Fatalf("blog project: %+v %+v", blog.Source, blog.Cloudflare)
	}
	if api.Source.Repo != "" || api.Cloudflare == nil || api.Cloudflare.State != cfStateLive || docs.Source.Cloudflare.Name != "Old_Docs" {
		t.Fatalf("api/docs: %+v %+v", api.Source, docs.Source.Cloudflare)
	}
	// Imported without a repository, so deploying says what to do instead of failing obscurely.
	h.do("PUT", "/api/settings/cloudflare/deploy", adminToken, map[string]string{"account_id": "acct", "token": "deploytok"}, nil)
	var e struct{ Error string }
	if code := h.do("POST", "/api/projects/"+api.ID+"/deployments", adminToken, nil, nil); code != http.StatusBadRequest {
		t.Fatalf("deploy without a repo: %d %s", code, e.Error)
	}
	// Importing again is refused; the list marks what is managed.
	h.do("POST", "/api/cloudflare/import", adminToken, map[string]any{"items": items[:1]}, &res)
	if !strings.Contains(res[0].Error, "already managed") {
		t.Fatalf("second import: %+v", res)
	}
	h.do("GET", "/api/cloudflare/discover", adminToken, nil, &found)
	for _, x := range found {
		if x.Name == "blog" && x.Imported != "blog" {
			t.Fatalf("blog should be marked imported: %+v", x)
		}
	}
}

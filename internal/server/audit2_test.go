package server

import (
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Deployment records carry the spec the agent was sent, environment included. Viewers must
// not read the values there any more than on the project itself.
func TestViewerDoesNotSeeDeploymentEnvValues(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi")
	var p ProjectView
	if code := h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "app", "device_id": reg.DeviceID,
		"source":  map[string]any{"type": "image", "image": "nginx:alpine"},
		"runtime": map[string]any{"env": []string{"DB_PASSWORD=hunter2"}},
	}, &p); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	var dep store.Deployment
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments", adminToken, nil, &dep); code != http.StatusAccepted {
		t.Fatalf("deploy: %d", code)
	}
	viewer, operator := h.makeUser("viewer1", "viewer"), h.makeUser("operator1", "operator")

	leaks := func(tok string) []string {
		t.Helper()
		var found []string
		var one store.Deployment
		var list, perProject []store.Deployment
		var projects []ProjectView
		var proj ProjectView
		h.do("GET", "/api/deployments/"+dep.ID, tok, nil, &one)
		h.do("GET", "/api/deployments", tok, nil, &list)
		h.do("GET", "/api/projects/"+p.ID+"/deployments", tok, nil, &perProject)
		h.do("GET", "/api/projects", tok, nil, &projects)
		h.do("GET", "/api/projects/"+p.ID, tok, nil, &proj)
		check := func(where string, env []string) {
			if len(env) != 1 || !strings.HasPrefix(env[0], "DB_PASSWORD=") {
				t.Fatalf("%s: env = %v", where, env)
			}
			if env[0] == "DB_PASSWORD=hunter2" {
				found = append(found, where)
			}
		}
		check("deployment", one.Spec.Env)
		check("deployments", list[0].Spec.Env)
		check("project deployments", perProject[0].Spec.Env)
		check("projects latest", projects[0].Latest.Spec.Env)
		check("project latest", proj.Latest.Spec.Env)
		return found
	}
	if got := leaks(viewer); len(got) != 0 {
		t.Fatalf("viewer saw the secret in %v", got)
	}
	if got := leaks(operator); len(got) != 5 {
		t.Fatalf("operator should see values everywhere, saw them in %v", got)
	}
	// The stored deployment is unchanged.
	if d, _ := h.st.GetDeployment(t.Context(), dep.ID); d.Spec.Env[0] != "DB_PASSWORD=hunter2" {
		t.Fatalf("stored spec changed: %v", d.Spec.Env)
	}
}

// Cloudflare builds run on the server itself, so an operator must not be able to choose what
// they run: that would hand them the server's database, secret key and Docker socket.
func TestOnlyAdminsConfigureCloudflareBuilds(t *testing.T) {
	f := newCFFake(t)
	h := newHarness(t)
	f.connect(h)
	if code := h.do("PUT", "/api/settings/cloudflare/deploy", adminToken, map[string]string{"account_id": "acct", "token": "deploytok"}, nil); code != http.StatusOK {
		t.Fatalf("connect: %d", code)
	}
	operator := h.makeUser("operator1", "operator")
	marker := filepath.Join(t.TempDir(), "pwned")
	project := func(build string) map[string]any {
		return map[string]any{"name": "site", "source": map[string]any{"type": "cloudflare", "repo": "o/site", "branch": "main",
			"cloudflare": map[string]any{"kind": "pages"}, "node": map[string]any{"build": build}}}
	}

	if code := h.do("POST", "/api/projects", operator, project("touch "+marker), nil); code != http.StatusForbidden {
		t.Fatalf("operator created a Cloudflare project: %d", code)
	}
	var p ProjectView
	if code := h.do("POST", "/api/projects", adminToken, project("mkdir -p dist && echo hi > dist/index.html"), &p); code != http.StatusCreated {
		t.Fatalf("admin create: %d", code)
	}
	if code := h.do("PATCH", "/api/projects/"+p.ID, operator, map[string]any{"source": project("touch " + marker)["source"]}, nil); code != http.StatusForbidden {
		t.Fatalf("operator changed the build: %d", code)
	}
	if code := h.do("PATCH", "/api/projects/"+p.ID, operator, map[string]any{"runtime": map[string]any{"env": []string{"NODE_OPTIONS=--require /tmp/x.js"}}}, nil); code != http.StatusForbidden {
		t.Fatalf("operator changed the build environment: %d", code)
	}

	// Previews build pull request branches on the server: only an admin turns them on.
	if code := h.do("PUT", "/api/projects/"+p.ID+"/previews/settings", operator, map[string]any{"enabled": true, "max": 3}, nil); code != http.StatusForbidden {
		t.Fatalf("operator turned on Cloudflare previews: %d", code)
	}
	if code := h.do("PUT", "/api/projects/"+p.ID+"/previews/settings", adminToken, map[string]any{"enabled": true, "max": 3}, nil); code != http.StatusOK {
		t.Fatalf("admin previews: %d", code)
	}
	if code := h.do("PUT", "/api/projects/"+p.ID+"/previews/settings", operator, map[string]any{"enabled": true, "max": 5}, nil); code != http.StatusOK {
		t.Fatalf("operator changing the limit of enabled previews: %d", code)
	}
	if code := h.do("PUT", "/api/projects/"+p.ID+"/previews/settings", operator, map[string]any{"enabled": false, "max": 5}, nil); code != http.StatusOK {
		t.Fatalf("operator turning previews off: %d", code)
	}

	// Turning a device project into a Cloudflare one is refused too.
	reg := h.register("pi")
	var img ProjectView
	h.do("POST", "/api/projects", operator, map[string]any{"name": "img", "device_id": reg.DeviceID, "source": map[string]any{"type": "image", "image": "nginx"}}, &img)
	if code := h.do("PATCH", "/api/projects/"+img.ID, operator, map[string]any{"source": project("touch " + marker)["source"]}, nil); code != http.StatusForbidden {
		t.Fatalf("operator converted a project to Cloudflare: %d", code)
	}

	// Operators still deploy what an admin set up.
	var dep store.Deployment
	if code := h.do("POST", "/api/projects/"+p.ID+"/deployments", operator, nil, &dep); code != http.StatusAccepted {
		t.Fatalf("operator deploy: %d", code)
	}
	if dep = waitDeployment(t, h, dep.ID); dep.Status != store.DeploymentSucceeded {
		t.Fatalf("deployment: %s %s", dep.Status, dep.Error)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("an operator's command ran on the server")
	}
}

// Moving a project to another device through its settings must clear its routes from the
// device it left, as a migration does.
func TestChangingDeviceResyncsTheOldProxy(t *testing.T) {
	h := newHarness(t)
	a, b := h.register("a"), h.register("b")
	for _, reg := range []api.RegisterResponse{a, b} {
		h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{
			Addresses: []string{"192.168.1.20"}, Capabilities: []string{api.CapProxy}, Server: "http://192.168.1.5:8080"}}, &api.HeartbeatResponse{})
	}
	var p ProjectView
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "blog", "device_id": a.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx"}, "runtime": map[string]any{"ports": []string{"8081:80"}}}, &p)
	if code := h.do("POST", "/api/projects/"+p.ID+"/domains", adminToken, map[string]string{"hostname": "blog.example.com"}, nil); code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("add domain: %d", code)
	}
	if code := h.do("PATCH", "/api/projects/"+p.ID, adminToken, map[string]any{"device_id": b.DeviceID}, nil); code != http.StatusOK {
		t.Fatalf("move: %d", code)
	}
	cmds, err := h.st.DispatchCommands(t.Context(), a.DeviceID)
	if err != nil {
		t.Fatal(err)
	}
	last := ""
	for _, c := range cmds {
		if c.Action == api.ActionProxySync {
			last = c.Args["caddyfile"]
		}
	}
	if last == "" || strings.Contains(last, "blog.example.com") {
		t.Fatalf("old device's proxy still serves the project:\n%s", last)
	}
}

// The worker login is asked about on every request to a protected worker. A success is
// remembered briefly, but never past a password change or a disabled account.
func TestGateCacheFollowsAccountChanges(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi")
	var p ProjectView
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "blog", "device_id": reg.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx"}}, &p)
	h.do("PUT", "/api/projects/"+p.ID+"/domains/access", adminToken, map[string]bool{"protected": true}, nil)
	h.makeUser("visitor", "viewer")
	var users []store.User
	h.do("GET", "/api/users", adminToken, nil, &users)
	id := ""
	for _, u := range users {
		if u.Username == "visitor" {
			id = u.ID
		}
	}
	gate := func(pw string) int {
		req, _ := http.NewRequest("GET", h.ts.URL+"/api/gate/verify?project="+p.ID, nil)
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("visitor:"+pw)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	old := "visitor-password-1"
	for i := 0; i < 3; i++ {
		if code := gate(old); code != http.StatusNoContent {
			t.Fatalf("visit %d: %d", i, code)
		}
	}
	if code := h.do("PATCH", "/api/users/"+id, adminToken, map[string]any{"password": "a-new-password-2"}, nil); code != http.StatusOK {
		t.Fatalf("change password: %d", code)
	}
	if code := gate(old); code != http.StatusUnauthorized {
		t.Fatalf("old password still accepted from the cache: %d", code)
	}
	if code := gate("a-new-password-2"); code != http.StatusNoContent {
		t.Fatalf("new password: %d", code)
	}
	if code := h.do("PATCH", "/api/users/"+id, adminToken, map[string]any{"disabled": true}, nil); code != http.StatusOK {
		t.Fatalf("disable: %d", code)
	}
	if code := gate("a-new-password-2"); code != http.StatusUnauthorized {
		t.Fatalf("disabled account still accepted from the cache: %d", code)
	}
}

// Renaming a Cloudflare project in the dashboard must keep it pointed at the Cloudflare project
// it deployed to, so the monitor does not treat that project as new.
func TestRenamingCloudflareProjectKeepsItsCloudflareName(t *testing.T) {
	f := newCFFake(t)
	h := newHarness(t)
	f.connect(h)
	var p ProjectView
	body := map[string]any{"name": "site", "source": map[string]any{"type": "cloudflare", "repo": "o/site", "branch": "main",
		"cloudflare": map[string]any{"kind": "pages"}}}
	if code := h.do("POST", "/api/projects", adminToken, body, &p); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	body["name"] = "renamed"
	if code := h.do("PATCH", "/api/projects/"+p.ID, adminToken, body, &p); code != http.StatusOK {
		t.Fatalf("rename: %d", code)
	}
	if p.Source.Cloudflare == nil || p.Source.Cloudflare.Name != "site" {
		t.Fatalf("Cloudflare name after rename: %+v", p.Source.Cloudflare)
	}
}

package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/store"
)

func TestHostAccessOf(t *testing.T) {
	img := store.Source{Type: store.SourceImage, Image: "nginx"}
	cases := []struct {
		name string
		src  store.Source
		rt   store.Runtime
		want int
	}{
		{"named volume only", img, store.Runtime{Volumes: []string{"data:/data"}}, 0},
		{"host folder", img, store.Runtime{Volumes: []string{"/:/host"}}, 1},
		{"docker socket", img, store.Runtime{Volumes: []string{"/var/run/docker.sock:/var/run/docker.sock:ro"}}, 1},
		{"relative path", img, store.Runtime{Volumes: []string{"./x:/x", "..:/y"}}, 2},
		{"host network", img, store.Runtime{Network: "host"}, 1},
		{"shared namespace", img, store.Runtime{Network: "container:abc"}, 1},
		{"user network", img, store.Runtime{Network: "mynet"}, 0},
		{"compose inline", store.Source{Type: store.SourceCompose, Compose: "services: {}"}, store.Runtime{}, 1},
		{"compose repo", store.Source{Type: store.SourceCompose, Repo: "o/r", ComposeFile: "c.yml"}, store.Runtime{}, 1},
	}
	for _, c := range cases {
		if got := hostAccessOf(c.src, c.rt); len(got) != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

func TestOperatorCannotUseHostLevelOptions(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi")
	op := h.makeUser("olive", "operator")
	mk := func(token, name string, rt map[string]any, src map[string]any) (int, store.Project) {
		var p store.Project
		code := h.do("POST", "/api/projects", token, map[string]any{"name": name, "device_id": reg.DeviceID, "source": src, "runtime": rt}, &p)
		return code, p
	}
	img := map[string]any{"type": "image", "image": "nginx"}
	compose := map[string]any{"type": "compose", "compose": "services:\n  a:\n    image: nginx\n    privileged: true\n"}

	bad := []struct {
		name string
		rt   map[string]any
		src  map[string]any
	}{
		{"mnt", map[string]any{"volumes": []string{"/:/host"}}, img},
		{"sock", map[string]any{"volumes": []string{"/var/run/docker.sock:/s"}}, img},
		{"net", map[string]any{"network": "host"}, img},
		{"stack", map[string]any{}, compose},
	}
	for _, b := range bad {
		if code, _ := mk(op, "op-"+b.name, b.rt, b.src); code != http.StatusForbidden {
			t.Errorf("operator %s: %d, want 403", b.name, code)
		}
	}
	if code, _ := mk(op, "op-ok", map[string]any{"volumes": []string{"data:/data"}}, img); code != http.StatusCreated {
		t.Fatalf("operator named volume: %d", code)
	}
	// An admin may; the operator may then keep it unchanged but not add to or change it.
	code, p := mk(adminToken, "adm", map[string]any{"volumes": []string{"/srv:/srv"}}, img)
	if code != http.StatusCreated {
		t.Fatalf("admin: %d", code)
	}
	var view ProjectView
	if code := h.do("PATCH", "/api/projects/"+p.ID, op, map[string]any{"runtime": map[string]any{"volumes": []string{"/srv:/srv"}, "env": []string{"A=1"}}}, &view); code != http.StatusOK {
		t.Fatalf("operator editing env while keeping the mount: %d", code)
	}
	if len(view.HostAccess) != 1 || !strings.Contains(view.HostAccess[0], "/srv") {
		t.Errorf("warning list: %v", view.HostAccess)
	}
	if code := h.do("PATCH", "/api/projects/"+p.ID, op, map[string]any{"runtime": map[string]any{"volumes": []string{"/:/srv"}}}, nil); code != http.StatusForbidden {
		t.Errorf("operator changing the mount: %d", code)
	}
	// Turning an existing operator project into a host-network one is also refused.
	if code := h.do("PATCH", "/api/projects/"+p.ID, op, map[string]any{"runtime": map[string]any{"volumes": []string{"/srv:/srv"}, "network": "host"}}, nil); code != http.StatusForbidden {
		t.Errorf("operator adding host network: %d", code)
	}
	// Adopting containers is admin only.
	if code := h.do("POST", "/api/devices/"+reg.DeviceID+"/adopt", op, map[string]any{}, nil); code != http.StatusForbidden {
		t.Errorf("operator adopt: %d", code)
	}
}

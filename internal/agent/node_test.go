package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func project(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestParseRemote(t *testing.T) {
	repo, ref, dir := parseRemote("https://x:tok@github.com/o/r.git#abc123:apps/web")
	if repo != "https://x:tok@github.com/o/r.git" || ref != "abc123" || dir != "apps/web" {
		t.Fatalf("got %q %q %q", repo, ref, dir)
	}
	if _, ref, dir := parseRemote("https://github.com/o/r.git#abc"); ref != "abc" || dir != "" {
		t.Fatalf("got %q %q", ref, dir)
	}
}

func TestPlanNodeServerDetectsEverything(t *testing.T) {
	dir := project(t, map[string]string{
		"package.json":   `{"scripts":{"start":"node server.js"},"engines":{"node":">=20.5"}}`,
		"pnpm-lock.yaml": "",
	})
	p, err := planNode(dir, &api.NodeSpec{}, nil, "3000")
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "20" || p.Start != "npm start" || p.Build != "" || !strings.Contains(p.Install, "pnpm install") {
		t.Fatalf("plan %+v", p)
	}
	for _, want := range []string{"FROM node:20-alpine AS install", "PORT=3000", `CMD ["sh", "-c", "npm start"]`} {
		if !strings.Contains(p.Dockerfile, want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, p.Dockerfile)
		}
	}
}

func TestPlanNodeStaticWithBuildArgs(t *testing.T) {
	dir := project(t, map[string]string{
		"package.json":      `{"scripts":{"build":"vite build"}}`,
		"package-lock.json": "{}",
		".nvmrc":            "v18.19.0\n",
	})
	p, err := planNode(dir, &api.NodeSpec{Static: true}, []string{"VITE_API"}, "80")
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "18.19.0" || p.Install != "npm ci" || p.Build != "npm run build" {
		t.Fatalf("plan %+v", p)
	}
	for _, want := range []string{"ARG VITE_API", "FROM nginx:alpine", "COPY --from=build /out /usr/share/nginx/html", "dist build out public"} {
		if !strings.Contains(p.Dockerfile, want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, p.Dockerfile)
		}
	}
}

func TestPlanNodeErrors(t *testing.T) {
	if _, err := planNode(t.TempDir(), &api.NodeSpec{}, nil, "3000"); err == nil {
		t.Error("missing package.json should fail")
	}
	dir := project(t, map[string]string{"package.json": `{}`})
	if _, err := planNode(dir, &api.NodeSpec{}, nil, "3000"); err == nil {
		t.Error("server without start script should fail")
	}
	if _, err := planNode(dir, &api.NodeSpec{Static: true}, nil, "80"); err == nil {
		t.Error("static without build should fail")
	}
	if _, err := planNode(dir, &api.NodeSpec{Start: "x", Version: "1; rm"}, nil, "80"); err == nil {
		t.Error("bad version should fail")
	}
}

func TestContainerPort(t *testing.T) {
	for in, want := range map[string]string{"": "3000", "8080:80": "80", "3000:3000/tcp": "3000", "9000": "9000"} {
		var ports []string
		if in != "" {
			ports = []string{in}
		}
		if got := containerPort(ports); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
}

func TestCloneSubdirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "apps", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(repo, "apps", "web", "package.json"), []byte(`{}`), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	sha, _ := cmd.Output()
	st := &steps{}
	root, err := clone(context.Background(), st, repo+"#"+strings.TrimSpace(string(sha))+":apps/web", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !exists(root, "package.json") {
		t.Fatalf("package.json missing in %s", root)
	}
	if _, err := clone(context.Background(), st, repo+"#"+strings.TrimSpace(string(sha))+":nope", t.TempDir()); err == nil {
		t.Error("missing root directory should fail")
	}
}

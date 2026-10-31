package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/api"
)

var (
	envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	versionRe = regexp.MustCompile(`^(lts|[0-9]+(\.[0-9]+){0,2})$`)
)

// parseRemote splits a Docker-style git context (https://host/owner/repo.git#ref:dir).
func parseRemote(remote string) (repo, ref, dir string) {
	repo, frag, _ := strings.Cut(remote, "#")
	ref, dir, _ = strings.Cut(frag, ":")
	return repo, ref, dir
}

func runLogged(ctx context.Context, st *steps, dir string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(l) != "" {
			st.logf("%s", strings.TrimRight(l, "\r"))
		}
	}
	if err != nil {
		return fmt.Errorf("%s %s: %s", name, args[0], redact(err.Error()))
	}
	return nil
}

// clone fetches exactly one commit into a fresh directory and returns the project directory.
func clone(ctx context.Context, st *steps, remote, work string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is not installed on this device")
	}
	repo, ref, dir := parseRemote(remote)
	src := filepath.Join(work, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		return "", err
	}
	if ref == "" {
		ref = "HEAD"
	}
	for _, c := range [][]string{
		{"init", "-q"},
		{"fetch", "-q", "--depth", "1", "--", repo, ref},
		{"checkout", "-q", "FETCH_HEAD"},
	} {
		if err := runLogged(ctx, st, src, "git", c...); err != nil {
			return "", err
		}
	}
	root := filepath.Join(src, filepath.Clean("/"+dir))
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("root directory %q does not exist in the repository", dir)
	}
	return root, nil
}

type pkgJSON struct {
	Scripts map[string]string `json:"scripts"`
	Engines struct {
		Node string `json:"node"`
	} `json:"engines"`
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// nodePlan is the resolved build: the generated Dockerfile and the detected choices.
type nodePlan struct {
	Dockerfile string
	Version    string
	Install    string
	Build      string
	Start      string
	Static     bool
}

var majorRe = regexp.MustCompile(`[0-9]+`)

func detectVersion(dir string, pkg pkgJSON) string {
	for _, f := range []string{".nvmrc", ".node-version"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			if v := strings.TrimPrefix(strings.TrimSpace(string(b)), "v"); versionRe.MatchString(v) {
				return v
			}
			if m := majorRe.FindString(string(b)); m != "" {
				return m
			}
		}
	}
	if m := majorRe.FindString(pkg.Engines.Node); m != "" {
		return m
	}
	return "22"
}

// planNode fills in whatever the user left empty from the repository and writes the Dockerfile.
// build args are the env names available at build time (static sites bake them in).
func planNode(dir string, n *api.NodeSpec, buildArgs []string, port string) (*nodePlan, error) {
	var pkg pkgJSON
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, fmt.Errorf("no package.json in the project root directory")
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return nil, fmt.Errorf("package.json is not valid JSON: %w", err)
	}
	p := &nodePlan{Version: n.Version, Install: n.Install, Build: n.Build, Start: n.Start, Static: n.Static}
	if p.Version == "" {
		p.Version = detectVersion(dir, pkg)
	}
	if !versionRe.MatchString(p.Version) {
		return nil, fmt.Errorf("node version %q should look like 22 or 20.11", p.Version)
	}
	if p.Install == "" {
		switch {
		case exists(dir, "pnpm-lock.yaml"):
			p.Install = "corepack enable && pnpm install --frozen-lockfile"
		case exists(dir, "yarn.lock"):
			p.Install = "corepack enable && yarn install --frozen-lockfile"
		case exists(dir, "package-lock.json"), exists(dir, "npm-shrinkwrap.json"):
			p.Install = "npm ci"
		default:
			p.Install = "npm install"
		}
	}
	if p.Build == "" {
		if _, ok := pkg.Scripts["build"]; ok {
			p.Build = "npm run build"
		}
	}
	if !p.Static && p.Start == "" {
		if _, ok := pkg.Scripts["start"]; !ok {
			return nil, fmt.Errorf("package.json has no start script; set a start command or choose a static site")
		}
		p.Start = "npm start"
	}
	if p.Static && p.Build == "" {
		return nil, fmt.Errorf("a static site needs a build command (package.json has no build script)")
	}

	var d strings.Builder
	fmt.Fprintf(&d, "FROM node:%s-alpine AS install\nWORKDIR /app\nCOPY . .\nRUN %s\n", p.Version, p.Install)
	d.WriteString("FROM install AS build\n")
	for _, a := range buildArgs {
		fmt.Fprintf(&d, "ARG %s\nENV %s=$%s\n", a, a, a)
	}
	if p.Build != "" {
		fmt.Fprintf(&d, "RUN %s\n", p.Build)
	} else {
		d.WriteString("RUN true\n")
	}
	if p.Static {
		out := strings.Trim(n.Output, "/")
		find := "for d in dist build out public; do if [ -d \"$d\" ]; then cp -r \"$d/.\" /out && break; fi; done"
		if out != "" {
			find = fmt.Sprintf("cp -r %q/. /out", out)
		}
		fmt.Fprintf(&d, "RUN mkdir /out && %s && [ \"$(ls -A /out)\" ] || (echo 'no build output found; set the output directory' >&2; exit 1)\n", find)
		d.WriteString("FROM nginx:alpine\nCOPY --from=build /out /usr/share/nginx/html\nEXPOSE 80\n")
	} else {
		fmt.Fprintf(&d, "FROM build\nENV NODE_ENV=production PORT=%s\nEXPOSE %s\nCMD [\"sh\", \"-c\", %s]\n", port, port, strconv.Quote(p.Start))
	}
	p.Dockerfile = d.String()
	return p, nil
}

// containerPort is the port the app must listen on, from the first port mapping (default 3000).
func containerPort(ports []string) string {
	if len(ports) > 0 {
		p := ports[len(ports)-1]
		if i := strings.LastIndex(p, ":"); i >= 0 {
			p = p[i+1:]
		}
		p, _, _ = strings.Cut(p, "/")
		if _, err := strconv.Atoi(p); err == nil {
			return p
		}
	}
	return "3000"
}

// buildNode clones the repository and builds its image through the Cloning, Installing and
// Building steps. Each docker build targets a stage so the timeline reflects real progress;
// the layer cache makes the later builds reuse the earlier work.
func (a *Agent) buildNode(ctx context.Context, st *steps, spec api.DeploySpec) error {
	b := spec.Build
	work, err := os.MkdirTemp("", "orch-node-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	st.begin(ctx, api.StepIndex(spec, api.StepCloning))
	_, ref, dir := parseRemote(b.Remote)
	st.logf("Cloning commit %.7s (root directory %q)", ref, dir)
	root, err := clone(ctx, st, b.Remote, work)
	if err != nil {
		return err
	}

	var args []string
	for _, e := range spec.Env {
		if k, _, ok := strings.Cut(e, "="); ok && envNameRe.MatchString(k) {
			args = append(args, k)
		}
	}
	plan, err := planNode(root, b.Node, args, containerPort(spec.Ports))
	if err != nil {
		return err
	}
	st.logf("Node %s, install: %s", plan.Version, plan.Install)
	if plan.Build != "" {
		st.logf("Build: %s", plan.Build)
	}
	if plan.Static {
		st.logf("Static site served by nginx on port 80")
	} else {
		st.logf("Start: %s (listening on PORT=%s)", plan.Start, containerPort(spec.Ports))
	}

	logf := func(l string) { st.logf("%s", l) }
	stages := []struct {
		step, target, tag string
	}{
		{api.StepInstalling, "install", ""},
		{api.StepBuilding, "", spec.Image},
	}
	for _, s := range stages {
		st.begin(ctx, api.StepIndex(spec, s.step))
		if s.target == "" {
			// The final image: re-uses the install layer, runs the build command.
			if err := a.docker.BuildLocal(ctx, root, plan.Dockerfile, "", s.tag, spec.Env, logf); err != nil {
				return err
			}
			continue
		}
		if err := a.docker.BuildLocal(ctx, root, plan.Dockerfile, s.target, "", spec.Env, logf); err != nil {
			return err
		}
	}
	st.logf("Success: built %s", spec.Image)
	return nil
}

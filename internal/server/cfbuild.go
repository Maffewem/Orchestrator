package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// cfFetchSource downloads the repository at sha from GitHub and unpacks it into dir. It uses
// the tarball API, so the server needs no git. It returns the number of files written.
func (s *Server) cfFetchSource(ctx context.Context, repo, sha, dir string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	u := fmt.Sprintf("%s/repos/%s/tarball/%s", s.githubAPI, repo, url.PathEscape(sha))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if tok := s.githubToken(ctx); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("downloading the source: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("downloading the source: GitHub answered %s", resp.Status)
	}
	return extractTarball(resp.Body, dir)
}

// extractTarball unpacks a gzipped tar whose files all sit under one top directory (as GitHub's
// do) into dir, without that directory. Links, devices and paths that escape dir are skipped.
func extractTarball(r io.Reader, dir string) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("reading the source archive: %w", err)
	}
	defer gz.Close()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	tr := tar.NewReader(gz)
	files := 0
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files, nil
		} else if err != nil {
			return files, fmt.Errorf("reading the source archive: %w", err)
		}
		_, rel, ok := strings.Cut(path.Clean(h.Name), "/")
		if !ok || rel == "" || h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		dest := filepath.Join(dir, filepath.FromSlash(rel))
		if !strings.HasPrefix(dest, filepath.Clean(dir)+string(filepath.Separator)) {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(dest, 0o755)
		case tar.TypeReg:
			if total += h.Size; total > cfMaxSourceBytes {
				return files, errors.New("the repository is larger than 1 GB")
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return files, err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return files, err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return files, err
			}
			files++
		}
	}
}

// cfBuildEnv is the environment build commands run in. It is deliberately not the server's:
// that holds the admin token and secret key. The Cloudflare Pages build variables are set too,
// so a build written for Cloudflare's own builders behaves the same here.
func (s *Server) cfBuildEnv(work string, cs *api.CloudflareSpec, project []string) []string {
	home := filepath.Join(work, "home")
	os.MkdirAll(home, 0o700)
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"TMPDIR=" + os.TempDir(),
		"LANG=C.UTF-8",
		"CI=true",
		"npm_config_cache=" + filepath.Join(work, "npm-cache"),
		"npm_config_update_notifier=false",
		"npm_config_fund=false",
		"npm_config_audit=false",
		"CF_PAGES=1",
		"CF_PAGES_BRANCH=" + cs.Branch,
		"CF_PAGES_COMMIT_SHA=" + cs.SHA,
	}
	return append(env, project...)
}

// cfRun runs one shell command in dir, logging its output to a step.
func (s *Server) cfRun(ctx context.Context, steps cfSteps, idx int, dir string, env []string, command string) error {
	steps.logf(idx, "$ %s", command)
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir, cmd.Env = dir, env
	ownProcessGroup(cmd)
	w := steps.writer(idx)
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	w.Flush()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		return fmt.Errorf("%q exited with status %d", command, ee.ExitCode())
	case err != nil:
		return fmt.Errorf("could not run %q: %w", command, err)
	}
	return nil
}

type packageJSON struct {
	Scripts map[string]string `json:"scripts"`
}

func readPackageJSON(dir string) *packageJSON {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil
	}
	var p packageJSON
	if json.Unmarshal(b, &p) != nil {
		return &packageJSON{}
	}
	return &p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// installCommand is the install command to run: the configured one, or one chosen from the lock file.
func installCommand(dir, configured string, hasPackage bool) string {
	switch {
	case configured != "":
		return configured
	case !hasPackage:
		return ""
	case exists(filepath.Join(dir, "pnpm-lock.yaml")) && hasTool("pnpm"):
		return "pnpm install --frozen-lockfile"
	case exists(filepath.Join(dir, "yarn.lock")) && hasTool("yarn"):
		return "yarn install --frozen-lockfile"
	case exists(filepath.Join(dir, "package-lock.json")), exists(filepath.Join(dir, "npm-shrinkwrap.json")):
		return "npm ci"
	}
	return "npm install"
}

func hasTool(name string) bool { _, err := exec.LookPath(name); return err == nil }

func buildCommand(configured string, pkg *packageJSON) string {
	switch {
	case configured != "":
		return configured
	case pkg != nil && pkg.Scripts["build"] != "":
		return "npm run build"
	}
	return ""
}

// inside resolves rel under root and refuses anything that leaves it, including through symlinks the build made.
func inside(root, rel string) (string, error) {
	p := filepath.Join(root, filepath.FromSlash(rel))
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if rp != rr && !strings.HasPrefix(rp, rr+string(filepath.Separator)) {
		return "", fmt.Errorf("%s points outside the project", rel)
	}
	return rp, nil
}

// pagesOutput finds the directory to upload: the configured one, or the first usual build output.
func pagesOutput(root, configured string) (string, error) {
	cands := []string{configured}
	if configured == "" {
		cands = []string{"dist", "build", "out", "public", "_site"}
	}
	for _, c := range cands {
		if st, err := os.Stat(filepath.Join(root, filepath.FromSlash(c))); err == nil && st.IsDir() {
			return inside(root, c)
		}
	}
	if configured != "" {
		return "", fmt.Errorf("the output directory %q does not exist after the build", configured)
	}
	if exists(filepath.Join(root, "index.html")) {
		return inside(root, ".")
	}
	return "", errors.New("no output directory found (looked for dist, build, out, public and _site): set the output directory in the project settings")
}

// workerEntry finds the Worker's entry module: the configured one, or the first usual build output.
func workerEntry(root, configured string) (string, error) {
	cands := []string{configured}
	if configured == "" {
		cands = []string{"dist/index.js", "dist/worker.js", "dist/index.mjs", "src/index.js", "index.js", "worker.js"}
	}
	for _, c := range cands {
		if st, err := os.Stat(filepath.Join(root, filepath.FromSlash(c))); err == nil && st.Mode().IsRegular() {
			return inside(root, c)
		}
	}
	if configured != "" {
		return "", fmt.Errorf("the entry file %q does not exist after the build", configured)
	}
	return "", errors.New("no Worker entry found (looked for dist/index.js, src/index.js, index.js and worker.js): set the entry file in the project settings")
}

// readSite reads every regular file under dir, skipping version-control and dependency folders.
func readSite(dir string) ([]pagesFile, error) {
	var files []pagesFile
	var total int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch name := d.Name(); {
		case d.IsDir() && (name == ".git" || name == "node_modules"):
			return filepath.SkipDir
		case d.IsDir(), !d.Type().IsRegular(), name == ".DS_Store":
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if total += int64(len(b)); total > 1<<30 {
			return errors.New("the output is larger than 1 GB")
		}
		files = append(files, pagesFile{Path: filepath.ToSlash(rel), Content: b})
		return nil
	})
	return files, err
}

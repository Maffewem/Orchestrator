package server

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/agent"
)

// Cloudflare builds run the repository's own install and build commands. They run in a
// throwaway Docker container, never on the machine that hosts the control plane, because that
// machine holds the database, the secret key and (usually) the Docker socket. The container
// has no mounts, no Docker socket, no added capabilities and no inherited environment: it gets
// only the build's own variables. If Docker cannot be used the build fails with a message; it
// never falls back to running on the host.

const (
	// cfBoxWork is the directory inside the sandbox that holds the build.
	cfBoxWork = "/work"
	// DefaultBuildImage is the image Cloudflare builds run in unless ORCH_BUILD_IMAGE says otherwise.
	DefaultBuildImage = "node:22-bookworm-slim"

	cfBoxMemory = 2 << 30
	cfBoxCPUs   = 2
	cfBoxPIDs   = 2048
	cfBoxUID    = 1000
)

// cfBox is one sandbox. The source tree lives at /work/src inside it.
type cfBox interface {
	// Put copies the local directory src into the sandbox as /work/src.
	Put(ctx context.Context, src string) error
	// Exec runs a shell command in /work/src/<dir>, writing its output to out.
	Exec(ctx context.Context, dir, command string, out io.Writer) (exitCode int, err error)
	// Fetch replaces the local directory dst with /work/src from the sandbox, leaving out
	// node_modules and .git. Only regular files and directories come back.
	Fetch(ctx context.Context, dst string) (files int, err error)
	// Close removes the sandbox.
	Close()
}

// cfBoxFactory starts a sandbox that has exactly env as its environment.
type cfBoxFactory func(ctx context.Context, env []string, logf func(string)) (cfBox, error)

func (s *Server) cfNewBox(ctx context.Context, env []string, logf func(string)) (cfBox, error) {
	if s.cfBoxer != nil {
		return s.cfBoxer(ctx, env, logf)
	}
	if s.docker == nil {
		return nil, errNoSandbox("no Docker engine is configured")
	}
	if err := s.docker.Ping(ctx); err != nil {
		return nil, errNoSandbox("Docker could not be reached: " + err.Error())
	}
	img := s.cfg.BuildImage
	if img == "" {
		img = DefaultBuildImage
	}
	id, err := s.docker.SandboxStart(ctx, agent.SandboxSpec{
		Image: img, Env: env, MemoryBytes: cfBoxMemory, CPUs: cfBoxCPUs, PIDs: cfBoxPIDs,
		MaxSeconds: int(cfBuildTimeout.Seconds()) + 300, Label: "cloudflare",
	}, logf)
	if err != nil {
		return nil, fmt.Errorf("starting the build container: %w", err)
	}
	return &dockerBox{d: s.docker, id: id}, nil
}

func errNoSandbox(why string) error {
	return errors.New("Cloudflare builds run in a throwaway Docker container, separate from the Orchestrator server, but " + why +
		". Give the server access to Docker (mount /var/run/docker.sock, or set DOCKER_HOST) and retry. The build was not run on the server itself.")
}

type dockerBox struct {
	d  *agent.Docker
	id string
}

func (b *dockerBox) Close() {
	b.d.SandboxRemove(context.Background(), b.id)
}

func (b *dockerBox) Put(ctx context.Context, src string) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(tarTree(pw, src)) }()
	err := b.d.SandboxPut(ctx, b.id, "/", pr)
	pr.CloseWithError(err)
	return err
}

// tarTree writes src as /work/src (and an empty /work/home) owned by the build user.
func tarTree(w io.Writer, src string) error {
	tw := tar.NewWriter(w)
	dir := func(name string) error {
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: 0o755, Uid: cfBoxUID, Gid: cfBoxUID})
	}
	for _, d := range []string{"work/", "work/home/"} {
		if err := dir(d); err != nil {
			return err
		}
	}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		name := "work/src"
		if rel != "." {
			name += "/" + filepath.ToSlash(rel)
		}
		switch {
		case d.IsDir():
			return dir(name + "/")
		case !d.Type().IsRegular():
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := int64(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: info.Size(), Uid: cfBoxUID, Gid: cfBoxUID}); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func (b *dockerBox) Exec(ctx context.Context, dir, command string, out io.Writer) (int, error) {
	wd := cfBoxWork + "/src"
	if dir != "" && dir != "." {
		wd += "/" + strings.Trim(filepath.ToSlash(dir), "/")
	}
	return b.d.SandboxExec(ctx, b.id, wd, command, out, out)
}

func (b *dockerBox) Fetch(ctx context.Context, dst string) (int, error) {
	if err := os.RemoveAll(dst); err != nil {
		return 0, err
	}
	pr, pw := io.Pipe()
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := extractTar(pr, dst, false)
		io.Copy(io.Discard, pr)
		done <- result{n, err}
	}()
	var stderr bytes.Buffer
	code, err := b.d.SandboxExec(ctx, b.id, cfBoxWork+"/src", "tar -cf - --exclude=node_modules --exclude=.git .", pw, &stderr)
	pw.Close()
	res := <-done
	switch {
	case err != nil:
		return 0, err
	case code != 0:
		return 0, fmt.Errorf("reading the build output failed: %s", strings.TrimSpace(stderr.String()))
	}
	return res.n, res.err
}

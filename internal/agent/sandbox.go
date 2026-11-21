package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// A sandbox is a throwaway container that runs code nobody here has reviewed, such as a
// repository's build scripts. It has no bind mounts, no Docker socket, no added capabilities,
// no host namespaces and only the environment it is given. Files go in and out as tar streams
// through the engine's API, so nothing on the machine running the control plane is shared.

// SandboxSpec configures one sandbox.
type SandboxSpec struct {
	Image string
	// Env is the whole environment of every command run in the sandbox.
	Env []string
	// MemoryBytes, CPUs and PIDs cap the sandbox; zero means no cap for that resource.
	MemoryBytes int64
	CPUs        float64
	PIDs        int64
	// MaxSeconds is how long the sandbox may live before it stops by itself.
	MaxSeconds int
	// Label marks the container so leftovers can be found and removed.
	Label string
}

// SandboxStart pulls the image if it is not present, then creates and starts the sandbox. It
// returns the container ID.
func (d *Docker) SandboxStart(ctx context.Context, spec SandboxSpec, logf func(string)) (string, error) {
	if spec.Image == "" {
		return "", errors.New("no sandbox image")
	}
	if _, err := d.ImageInspect(ctx, spec.Image); err != nil {
		var de *dockerError
		if !errors.As(err, &de) || de.Status != http.StatusNotFound {
			return "", err
		}
		logf("Pulling " + spec.Image)
		if err := d.Pull(ctx, spec.Image, logf); err != nil {
			return "", fmt.Errorf("pulling %s: %w", spec.Image, err)
		}
	}
	secs := spec.MaxSeconds
	if secs <= 0 {
		secs = 3600
	}
	host := map[string]any{
		"CapDrop":         []string{"ALL"},
		"SecurityOpt":     []string{"no-new-privileges"},
		"Privileged":      false,
		"NetworkMode":     "bridge",
		"PidMode":         "",
		"IpcMode":         "private",
		"RestartPolicy":   map[string]any{"Name": "no"},
		"AutoRemove":      false,
		"PublishAllPorts": false,
	}
	if spec.MemoryBytes > 0 {
		host["Memory"], host["MemorySwap"] = spec.MemoryBytes, spec.MemoryBytes
	}
	if spec.CPUs > 0 {
		host["NanoCpus"] = int64(spec.CPUs * 1e9)
	}
	if spec.PIDs > 0 {
		host["PidsLimit"] = spec.PIDs
	}
	labels := map[string]string{"orchestrator.sandbox": "1"}
	if spec.Label != "" {
		labels["orchestrator.sandbox.for"] = spec.Label
	}
	body := map[string]any{
		"Image":      spec.Image,
		"Cmd":        []string{"sleep", fmt.Sprint(secs)},
		"Entrypoint": []string{},
		"User":       "1000:1000",
		"Env":        spec.Env,
		"Labels":     labels,
		"HostConfig": host,
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := d.call(ctx, http.MethodPost, "/containers/create", nil, body, &created); err != nil {
		return "", err
	}
	if err := d.call(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil, nil); err != nil {
		d.call(context.WithoutCancel(ctx), http.MethodDelete, "/containers/"+created.ID, url.Values{"force": {"true"}}, nil, nil)
		return "", err
	}
	return created.ID, nil
}

// SandboxRemove deletes the sandbox, running or not.
func (d *Docker) SandboxRemove(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"true"}, "v": {"true"}}, nil, nil)
}

// SandboxPut unpacks the tar stream under dir inside the sandbox.
func (d *Docker) SandboxPut(ctx context.Context, id, dir string, tarball io.Reader) error {
	resp, err := d.do(ctx, http.MethodPut, "/containers/"+id+"/archive", url.Values{"path": {dir}}, tarball, "application/x-tar")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// SandboxExec runs `sh -c command` in dir inside the sandbox. Its standard output goes to
// stdout and its standard error to stderr; stdout may be nil. It returns the exit code.
func (d *Docker) SandboxExec(ctx context.Context, id, dir, command string, stdout, stderr io.Writer) (int, error) {
	var created struct {
		ID string `json:"Id"`
	}
	err := d.call(ctx, http.MethodPost, "/containers/"+id+"/exec", nil, map[string]any{
		"AttachStdout": true, "AttachStderr": true, "Tty": false,
		"WorkingDir": dir, "Cmd": []string{"sh", "-c", command},
	}, &created)
	if err != nil {
		return -1, err
	}
	resp, err := d.request(ctx, http.MethodPost, "/exec/"+created.ID+"/start", nil, map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	if stdout == nil {
		stdout = io.Discard
	}
	if err := demuxTo(resp.Body, stdout, stderr); err != nil {
		if ctx.Err() != nil {
			return -1, ctx.Err()
		}
		return -1, err
	}
	var st struct {
		ExitCode int  `json:"ExitCode"`
		Running  bool `json:"Running"`
	}
	if err := d.call(context.WithoutCancel(ctx), http.MethodGet, "/exec/"+created.ID+"/json", nil, nil, &st); err != nil {
		return -1, err
	}
	return st.ExitCode, nil
}

// demuxTo splits Docker's multiplexed attach stream: each frame has an 8-byte header (stream
// type, three zero bytes, big-endian length).
func demuxTo(r io.Reader, out, errw io.Writer) error {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		n := int64(binary.BigEndian.Uint32(hdr[4:]))
		w := out
		if hdr[0] == 2 {
			w = errw
		}
		if w == nil {
			w = io.Discard
		}
		if _, err := io.CopyN(w, r, n); err != nil {
			return err
		}
	}
}

// Ping reports whether the engine answers.
func (d *Docker) Ping(ctx context.Context) error {
	resp, err := d.do(ctx, http.MethodGet, "/_ping", nil, nil, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

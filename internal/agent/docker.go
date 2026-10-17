package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Docker is a minimal Docker Engine API client using only the standard library, so the
// agent stays a small static binary that cross-compiles for any Raspberry Pi.
type Docker struct {
	client *http.Client
	base   string
}

// NewDocker connects to a Docker host such as unix:///var/run/docker.sock or tcp://host:2375.
func NewDocker(host string) (*Docker, error) {
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("docker host: %w", err)
	}
	switch u.Scheme {
	case "unix":
		path := u.Path
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		}
		return &Docker{client: &http.Client{Transport: tr}, base: "http://docker"}, nil
	case "tcp", "http":
		return &Docker{client: &http.Client{}, base: "http://" + u.Host}, nil
	default:
		return nil, fmt.Errorf("unsupported docker host scheme %q", u.Scheme)
	}
}

type dockerError struct {
	Status  int
	Message string
}

func (e *dockerError) Error() string { return fmt.Sprintf("docker: %s (HTTP %d)", e.Message, e.Status) }

func isStatus(err error, code int) bool {
	var de *dockerError
	return errors.As(err, &de) && de.Status == code
}

func (d *Docker) request(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	u := d.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var e struct {
			Message string `json:"message"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(raw, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
		}
		return nil, &dockerError{Status: resp.StatusCode, Message: e.Message}
	}
	return resp, nil
}

func (d *Docker) call(ctx context.Context, method, path string, q url.Values, body, out any) error {
	resp, err := d.request(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Info summarises the engine. Errors are reported inside the result so the control plane
// can show an unhealthy engine rather than a missing one.
func (d *Docker) Info(ctx context.Context) *api.DockerInfo {
	var v struct {
		Version    string `json:"Version"`
		APIVersion string `json:"ApiVersion"`
	}
	if err := d.call(ctx, http.MethodGet, "/version", nil, nil, &v); err != nil {
		return &api.DockerInfo{Error: err.Error()}
	}
	var info struct {
		Containers        int `json:"Containers"`
		ContainersRunning int `json:"ContainersRunning"`
		Images            int `json:"Images"`
	}
	if err := d.call(ctx, http.MethodGet, "/info", nil, nil, &info); err != nil {
		return &api.DockerInfo{Version: v.Version, APIVersion: v.APIVersion, Error: err.Error()}
	}
	return &api.DockerInfo{
		Version: v.Version, APIVersion: v.APIVersion,
		ContainersRunning: info.ContainersRunning, ContainersTotal: info.Containers, Images: info.Images,
	}
}

type dockerContainer struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
	Ports   []struct {
		IP          string `json:"IP"`
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
}

func (c dockerContainer) toAPI() api.Container {
	name := c.ID
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}
	out := api.Container{
		ID: c.ID, Name: name, Image: c.Image, State: c.State, Status: c.Status, Created: c.Created,
		Labels: c.Labels, ComposeProject: c.Labels["com.docker.compose.project"],
	}
	for _, p := range c.Ports {
		out.Ports = append(out.Ports, api.Port{IP: p.IP, PrivatePort: p.PrivatePort, PublicPort: p.PublicPort, Type: p.Type})
	}
	return out
}

func (d *Docker) listContainers(ctx context.Context, filters map[string][]string) ([]dockerContainer, error) {
	q := url.Values{"all": {"1"}}
	if filters != nil {
		f, _ := json.Marshal(filters)
		q.Set("filters", string(f))
	}
	var raw []dockerContainer
	err := d.call(ctx, http.MethodGet, "/containers/json", q, nil, &raw)
	return raw, err
}

func (d *Docker) Containers(ctx context.Context) ([]api.Container, error) {
	raw, err := d.listContainers(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]api.Container, 0, len(raw))
	for _, c := range raw {
		out = append(out, c.toAPI())
	}
	return out, nil
}

// ContainerByName returns the ID of the container with exactly this name, or "".
func (d *Docker) ContainerByName(ctx context.Context, name string) (string, error) {
	raw, err := d.listContainers(ctx, map[string][]string{"name": {"^/" + name + "$"}})
	if err != nil || len(raw) == 0 {
		return "", err
	}
	return raw[0].ID, nil
}

func (d *Docker) Start(ctx context.Context, id string) error {
	err := d.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
	if isStatus(err, http.StatusNotModified) {
		return nil
	}
	return err
}

func (d *Docker) Stop(ctx context.Context, id string) error {
	err := d.call(ctx, http.MethodPost, "/containers/"+id+"/stop", url.Values{"t": {"10"}}, nil, nil)
	if isStatus(err, http.StatusNotModified) {
		return nil
	}
	return err
}

func (d *Docker) Restart(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodPost, "/containers/"+id+"/restart", url.Values{"t": {"10"}}, nil, nil)
}

func (d *Docker) Remove(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}}, nil, nil)
}

func (d *Docker) Rename(ctx context.Context, id, name string) error {
	return d.call(ctx, http.MethodPost, "/containers/"+id+"/rename", url.Values{"name": {name}}, nil, nil)
}

// ContainerState is the part of an inspect result deployments care about.
type ContainerState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Restarting bool   `json:"Restarting"`
	ExitCode   int    `json:"ExitCode"`
	Error      string `json:"Error"`
}

func (d *Docker) Inspect(ctx context.Context, id string) (ContainerState, bool, error) {
	var v struct {
		State  ContainerState `json:"State"`
		Config struct {
			Tty bool `json:"Tty"`
		} `json:"Config"`
	}
	err := d.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &v)
	return v.State, v.Config.Tty, err
}

// Logs returns the last tail lines of a container's combined output, with timestamps.
func (d *Docker) Logs(ctx context.Context, id string, tail int) (string, error) {
	_, tty, err := d.Inspect(ctx, id)
	if err != nil {
		return "", err
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}, "tail": {strconv.Itoa(tail)}}
	resp, err := d.request(ctx, http.MethodGet, "/containers/"+id+"/logs", q, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, 4<<20)
	if tty {
		b, err := io.ReadAll(body)
		return string(b), err
	}
	return demux(body)
}

// demux flattens Docker's multiplexed stdout/stderr stream (8-byte frame headers).
func demux(r io.Reader) (string, error) {
	var out strings.Builder
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return out.String(), nil
			}
			return out.String(), err
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		if _, err := io.CopyN(&out, r, int64(n)); err != nil {
			return out.String(), nil
		}
	}
}

// splitImage separates an image reference into repository and tag for the pull API.
func splitImage(ref string) (repo, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref, "" // pinned by digest
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, "latest"
}

// jsonStream reads a Docker progress stream, calling fn for each message.
func jsonStream(r io.Reader, fn func(msg streamMsg)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m streamMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
		fn(m)
	}
	return sc.Err()
}

type streamMsg struct {
	Stream   string `json:"stream"`
	Status   string `json:"status"`
	ID       string `json:"id"`
	Progress string `json:"progress"`
	Error    string `json:"error"`
}

// Pull pulls an image, reporting meaningful progress lines (not per-byte updates).
func (d *Docker) Pull(ctx context.Context, ref string, logf func(string)) error {
	repo, tag := splitImage(ref)
	q := url.Values{"fromImage": {repo}}
	if tag != "" {
		q.Set("tag", tag)
	}
	resp, err := d.request(ctx, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return jsonStream(resp.Body, func(m streamMsg) {
		if m.Status == "" || m.Progress != "" || strings.HasPrefix(m.Status, "Waiting") ||
			strings.HasPrefix(m.Status, "Downloading") || strings.HasPrefix(m.Status, "Extracting") {
			return
		}
		if m.ID != "" {
			logf(m.ID + ": " + m.Status)
		} else {
			logf(m.Status)
		}
	})
}

// Build builds an image from a remote git context. The Docker engine does the clone.
func (d *Docker) Build(ctx context.Context, remote, dockerfile, tag string, logf func(string)) error {
	q := url.Values{"remote": {remote}, "t": {tag}, "rm": {"1"}, "forcerm": {"1"}}
	if dockerfile != "" {
		q.Set("dockerfile", dockerfile)
	}
	resp, err := d.request(ctx, http.MethodPost, "/build", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return jsonStream(resp.Body, func(m streamMsg) {
		text := strings.TrimRight(m.Stream, "\n")
		if text == "" {
			text = m.Status
		}
		if strings.TrimSpace(text) != "" {
			logf(text)
		}
	})
}

// Create creates (but does not start) a container for spec named spec.Service.
func (d *Docker) Create(ctx context.Context, name string, spec api.DeploySpec, labels map[string]string) (string, error) {
	exposed := map[string]struct{}{}
	bindings := map[string][]map[string]string{}
	for _, p := range spec.Ports {
		host, container, ok := strings.Cut(p, ":")
		if !ok {
			container, host = host, ""
		}
		if !strings.Contains(container, "/") {
			container += "/tcp"
		}
		exposed[container] = struct{}{}
		if host != "" {
			bindings[container] = append(bindings[container], map[string]string{"HostPort": host})
		}
	}
	all := map[string]string{}
	for k, v := range spec.Labels {
		all[k] = v
	}
	for k, v := range labels {
		all[k] = v
	}
	body := map[string]any{
		"Image":        spec.Image,
		"Env":          spec.Env,
		"Labels":       all,
		"ExposedPorts": exposed,
		"HostConfig": map[string]any{
			"PortBindings":  bindings,
			"RestartPolicy": map[string]string{"Name": spec.Restart},
		},
	}
	if len(spec.Command) > 0 {
		body["Cmd"] = spec.Command
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := d.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, body, &out)
	return out.ID, err
}

// waitRunning reports whether a container is still running after settle.
func (d *Docker) waitRunning(ctx context.Context, id string, settle time.Duration) (ContainerState, error) {
	deadline := time.Now().Add(settle)
	for {
		st, _, err := d.Inspect(ctx, id)
		if err != nil {
			return st, err
		}
		if !st.Running || st.Restarting {
			return st, nil
		}
		if time.Now().After(deadline) {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

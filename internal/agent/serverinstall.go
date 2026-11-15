package agent

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// installServer sets up a new Orchestrator server on this device from a transfer bundle the
// current server offers (the "promote a device to be the main server" flow). It never touches
// the server it was copied from, and it refuses to run if a server already lives here.
//
//	pull image -> volume seeded by a one-off helper (orchestrator-server import-backup)
//	           -> server container started with the data volume -> wait for /api/health
func (a *Agent) installServer(ctx context.Context, args map[string]string) (string, error) {
	image, bundle, password := args["image"], args["bundle"], args["password"]
	port, err := strconv.Atoi(args["port"])
	if err != nil || port < 1 || port > 65535 {
		return "", errors.New("invalid port")
	}
	if image == "" || bundle == "" || password == "" {
		return "", errors.New("incomplete server.install command")
	}
	name, volume := "orchestrator", "orchestrator-data"
	if id, _ := a.docker.ContainerByName(ctx, name); id != "" {
		return "", fmt.Errorf("a container called %q already runs on this device, so it already has a server", name)
	}
	if l, err := net.Listen("tcp", fmt.Sprintf(":%d", port)); err != nil {
		return "", fmt.Errorf("port %d is already in use on this device; choose another", port)
	} else {
		l.Close()
	}

	var out strings.Builder
	logf := func(m string) { out.WriteString(m + "\n") }
	logf("pulling " + image)
	if err := a.docker.Pull(ctx, image, logf); err != nil {
		return out.String(), fmt.Errorf("pull %s: %w", image, err)
	}

	work, err := os.MkdirTemp(filepath.Dir(a.cfg.StatePath), "server-move-")
	if err != nil {
		return out.String(), err
	}
	defer os.RemoveAll(work)
	if err := a.downloadBundle(ctx, bundle, filepath.Join(work, "bundle.tar"), password); err != nil {
		return out.String(), err
	}
	logf("transfer bundle downloaded")

	if err := a.docker.seedServerVolume(ctx, image, volume, filepath.Join(work, "bundle.tar"), logf); err != nil {
		return out.String(), err
	}
	logf("data volume seeded")

	id, err := a.docker.createServer(ctx, name, image, volume, port)
	if err != nil {
		return out.String(), fmt.Errorf("create the server container: %w", err)
	}
	if err := a.docker.Start(ctx, id); err != nil {
		a.docker.RemoveIfExists(ctx, id)
		return out.String(), fmt.Errorf("start the server container: %w", err)
	}
	health := fmt.Sprintf("http://127.0.0.1:%d/api/health", port)
	for i := 0; i < 60; i++ {
		resp, err := http.Get(health)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				logf(fmt.Sprintf("server answering on port %d", port))
				return out.String(), nil
			}
		}
		select {
		case <-ctx.Done():
			return out.String(), ctx.Err()
		case <-time.After(time.Second):
		}
	}
	logs, _ := a.docker.Logs(ctx, id, 30)
	return out.String(), fmt.Errorf("the new server did not answer on port %d; its log:\n%s", port, logs)
}

// downloadBundle fetches the transfer bundle from the current server and wraps it, with its
// password, in a tar file ready to be copied into the helper container.
func (a *Agent) downloadBundle(ctx context.Context, path, dst, password string) error {
	u, err := url.Parse(a.server())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.String(), "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.state.AgentToken)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("could not download the transfer bundle (HTTP %d)", resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// The bundle size is unknown until it has been read, and a tar header needs it, so it goes
	// through a temporary file first.
	tmp, err := os.CreateTemp(filepath.Dir(dst), "bundle-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, resp.Body)
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "bundle.enc", Mode: 0o600, Size: n, Uid: 10001, Gid: 10001}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, tmp); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "password", Mode: 0o600, Size: int64(len(password)), Uid: 10001, Gid: 10001}); err != nil {
		return err
	}
	if _, err := io.WriteString(tw, password); err != nil {
		return err
	}
	return tw.Close()
}

// seedServerVolume runs a one-off container that decrypts the bundle and stages it as the
// database the server swaps in on its first start.
func (d *Docker) seedServerVolume(ctx context.Context, image, volume, tarPath string, logf func(string)) error {
	body := map[string]any{
		"Image":      image,
		"Entrypoint": []string{"orchestrator-server"},
		"Cmd":        []string{"import-backup", "-in", "/tmp/bundle.enc", "-db", "/data/orchestrator.db", "-password-file", "/tmp/password"},
		"User":       "10001",
		"Labels":     map[string]string{"orchestrator.role": "server-move"},
		"HostConfig": map[string]any{"Binds": []string{volume + ":/data"}, "NetworkMode": "none"},
	}
	var created struct {
		ID string `json:"Id"`
	}
	name := fmt.Sprintf("orchestrator-import-%d", time.Now().Unix())
	if err := d.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, body, &created); err != nil {
		return fmt.Errorf("create the import helper: %w", err)
	}
	defer d.RemoveIfExists(context.Background(), created.ID)
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	resp, err := d.do(ctx, http.MethodPut, "/containers/"+created.ID+"/archive", url.Values{"path": {"/tmp"}}, f, "application/x-tar")
	if err != nil {
		return fmt.Errorf("hand the bundle to the import helper: %w", err)
	}
	resp.Body.Close()
	if err := d.Start(ctx, created.ID); err != nil {
		return fmt.Errorf("start the import helper: %w", err)
	}
	var res struct {
		StatusCode int `json:"StatusCode"`
	}
	if err := d.call(ctx, http.MethodPost, "/containers/"+created.ID+"/wait", nil, nil, &res); err != nil {
		return err
	}
	if res.StatusCode != 0 {
		logs, _ := d.Logs(ctx, created.ID, 20)
		return fmt.Errorf("the import failed (exit %d): %s", res.StatusCode, strings.TrimSpace(logs))
	}
	return nil
}

// createServer creates (without starting) the server container: restart on boot, data in the
// named volume and, when the Docker socket is there, mounted so the dashboard can update the server.
func (d *Docker) createServer(ctx context.Context, name, image, volume string, port int) (string, error) {
	binds := []string{volume + ":/data"}
	host := map[string]any{
		"PortBindings":  map[string][]map[string]string{"8080/tcp": {{"HostPort": strconv.Itoa(port)}}},
		"RestartPolicy": map[string]string{"Name": "unless-stopped"},
	}
	if gid, ok := dockerSocketGroup(); ok {
		binds = append(binds, "/var/run/docker.sock:/var/run/docker.sock")
		host["GroupAdd"] = []string{strconv.Itoa(gid)}
	}
	host["Binds"] = binds
	body := map[string]any{
		"Image":        image,
		"ExposedPorts": map[string]struct{}{"8080/tcp": {}},
		"HostConfig":   host,
		"Labels":       map[string]string{"orchestrator.role": "server"},
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := d.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, body, &out)
	return out.ID, err
}

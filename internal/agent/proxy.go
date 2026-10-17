package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

const (
	proxyName  = "orchestrator-proxy"
	proxyImage = "caddy:2-alpine"
)

// syncProxy writes the Caddyfile for the per-device proxy and makes sure a Caddy
// container is running with it. Caddy shares the host network so it can reach published
// ports on 127.0.0.1 and bind 80/443 itself.
func (a *Agent) syncProxy(ctx context.Context, caddyfile string) error {
	if caddyfile == "" {
		return fmt.Errorf("proxy.sync without a Caddyfile")
	}
	dir := filepath.Join(filepath.Dir(a.cfg.StatePath), "proxy")
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		return err
	}
	file := filepath.Join(dir, "Caddyfile")
	old, _ := os.ReadFile(file)
	if err := os.WriteFile(file, []byte(caddyfile), 0o644); err != nil {
		return err
	}
	id, err := a.docker.ContainerByName(ctx, proxyName)
	if err != nil {
		return err
	}
	if id != "" {
		if string(old) == caddyfile {
			if running, _, herr := a.docker.Health(ctx, id); herr == nil && running {
				return nil
			}
		}
		// A restart re-reads the file; the connection blip is a second or two.
		return a.docker.Restart(ctx, id)
	}
	if err := a.docker.Pull(ctx, proxyImage, func(string) {}); err != nil {
		return err
	}
	body := map[string]any{
		"Image":  proxyImage,
		"Cmd":    []string{"caddy", "run", "--config", "/etc/caddy/Caddyfile"},
		"Labels": map[string]string{"orchestrator.role": "proxy"},
		"HostConfig": map[string]any{
			"NetworkMode":   "host",
			"RestartPolicy": map[string]string{"Name": "unless-stopped"},
			"Binds":         []string{dir + "/Caddyfile:/etc/caddy/Caddyfile:ro", dir + "/data:/data"},
		},
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := a.docker.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {proxyName}}, body, &out); err != nil {
		return err
	}
	return a.docker.Start(ctx, out.ID)
}

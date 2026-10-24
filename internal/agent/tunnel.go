package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

const (
	tunnelName  = "orchestrator-tunnel"
	tunnelImage = "cloudflare/cloudflared:latest"
)

// syncTunnel runs cloudflared with the given tunnel token on the host network, so tunnel
// routes can point at localhost ports. An empty token removes the container. The token lives
// in the container's environment; a changed token (by fingerprint label) recreates it.
func (a *Agent) syncTunnel(ctx context.Context, token string) error {
	id, err := a.docker.ContainerByName(ctx, tunnelName)
	if err != nil {
		return err
	}
	if token == "" {
		if id == "" {
			return nil
		}
		return a.docker.RemoveIfExists(ctx, id)
	}
	fp := tokenFingerprint(token)
	if id != "" {
		if cur, _ := a.docker.LabelOf(ctx, id, "orchestrator.tunnel"); cur == fp {
			if running, _, herr := a.docker.Health(ctx, id); herr == nil && running {
				return nil
			}
			return a.docker.Restart(ctx, id)
		}
		if err := a.docker.RemoveIfExists(ctx, id); err != nil {
			return err
		}
	}
	if err := a.docker.Pull(ctx, tunnelImage, func(string) {}); err != nil {
		return fmt.Errorf("pull %s: %w", tunnelImage, err)
	}
	body := map[string]any{
		"Image":  tunnelImage,
		"Env":    []string{"TUNNEL_TOKEN=" + token},
		"Cmd":    []string{"tunnel", "--no-autoupdate", "run"},
		"Labels": map[string]string{"orchestrator.role": "tunnel", "orchestrator.tunnel": fp},
		"HostConfig": map[string]any{
			"NetworkMode":   "host",
			"RestartPolicy": map[string]string{"Name": "unless-stopped"},
		},
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := a.docker.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {tunnelName}}, body, &out); err != nil {
		return err
	}
	return a.docker.Start(ctx, out.ID)
}

package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

const (
	tunnelName  = "orchestrator-tunnel"
	tunnelImage = "cloudflare/cloudflared:latest"
	// tunnelInfoTTL bounds how often the heartbeat asks Docker about cloudflared.
	tunnelInfoTTL = 60 * time.Second
)

// syncTunnel runs cloudflared with the given tunnel token on the host network, so tunnel
// routes can point at localhost ports. An empty token removes the container. The token lives
// in the container's environment; a changed token (by fingerprint label) recreates it.
//
// A container that already runs is only restarted, never re-pulled, so it keeps the cloudflared
// version it was created with. With update set the newest image is pulled first and the
// container is recreated (same token, same settings) when that image differs from the one it runs.
func (a *Agent) syncTunnel(ctx context.Context, token string, update bool) error {
	defer a.forgetTunnelInfo()
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
		cur, _ := a.docker.LabelOf(ctx, id, "orchestrator.tunnel")
		if cur == fp {
			if !update {
				if running, _, herr := a.docker.Health(ctx, id); herr == nil && running {
					return nil
				}
				return a.docker.Restart(ctx, id)
			}
			// Pull before touching the running container, so a failed pull (no network, registry
			// down) leaves the tunnel as it was.
			if err := a.docker.Pull(ctx, tunnelImage, func(string) {}); err != nil {
				return fmt.Errorf("pull %s: %w", tunnelImage, err)
			}
			have, _, _ := a.docker.ContainerImage(ctx, id)
			latest, _ := a.docker.ImageID(ctx, tunnelImage)
			if have != "" && have == latest {
				if running, _, herr := a.docker.Health(ctx, id); herr == nil && running {
					return nil // already on the newest image
				}
				return a.docker.Restart(ctx, id)
			}
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

func (a *Agent) forgetTunnelInfo() {
	a.tunMu.Lock()
	a.tunAt = time.Time{}
	a.tunMu.Unlock()
}

var cloudflaredVersionRe = regexp.MustCompile(`version\s+(\d+(?:\.\d+)+)`)

// parseCloudflaredVersion extracts "2025.11.1" from "cloudflared version 2025.11.1 (built ...)".
func parseCloudflaredVersion(out string) string {
	if m := cloudflaredVersionRe.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return ""
}

// tunnelInfo reports the running cloudflared version for the heartbeat, or nil when the device
// runs no tunnel container. The answer is cached briefly; the version is read from inside the
// container, so it is what is really running and not what a tag says.
func (a *Agent) tunnelInfo() *api.TunnelInfo {
	if a.docker == nil {
		return nil
	}
	a.tunMu.Lock()
	defer a.tunMu.Unlock()
	if !a.tunAt.IsZero() && time.Since(a.tunAt) < tunnelInfoTTL {
		return a.tunInfo
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	a.tunInfo = nil
	id, err := a.docker.ContainerByName(ctx, tunnelName)
	if err != nil {
		return nil // Docker unreachable: try again next heartbeat
	}
	a.tunAt = time.Now()
	if id == "" {
		return nil
	}
	info := &api.TunnelInfo{Image: tunnelImage}
	if _, ref, err := a.docker.ContainerImage(ctx, id); err == nil && ref != "" {
		info.Image = ref
	}
	if out, err := a.docker.ExecOutput(ctx, id, "cloudflared", "--version"); err == nil {
		info.Version = parseCloudflaredVersion(out)
	}
	a.tunInfo = info
	return info
}

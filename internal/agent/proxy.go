package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

const (
	proxyName  = "orchestrator-proxy"
	proxyImage = "caddy:2-alpine"
	// proxyDNSImage is Caddy built with the Cloudflare DNS module, used for the DNS challenge.
	// It is a community multi-arch build (amd64, arm64, armv7, armv6).
	proxyDNSImage = "ghcr.io/caddybuilds/caddy-cloudflare:2"
)

// syncProxy writes the Caddyfile for the per-device proxy and makes sure a Caddy
// container is running with it. Caddy shares the host network so it can reach published
// ports on 127.0.0.1 and bind 80/443 itself.
//
// With a cloudflareToken Caddy runs the image that has the Cloudflare DNS module and gets
// the token as CF_API_TOKEN, so certificates use the DNS challenge. The token is also kept in
// a 0600 file next to the Caddyfile so the container can be recreated after a restart.
func (a *Agent) syncProxy(ctx context.Context, caddyfile, cloudflareToken string) error {
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
	image, env, mode := proxyImage, []string(nil), "http"
	if cloudflareToken != "" {
		image, env, mode = proxyDNSImage, []string{"CF_API_TOKEN=" + cloudflareToken}, "dns:"+tokenFingerprint(cloudflareToken)
	}
	id, err := a.docker.ContainerByName(ctx, proxyName)
	if err != nil {
		return err
	}
	if id != "" {
		// The image and token are fixed at creation: when either changes, start over. The
		// certificates live in the data directory, so nothing is lost.
		if cur, _ := a.docker.LabelOf(ctx, id, "orchestrator.proxy"); cur != mode && !(cur == "" && mode == "http") {
			if err := a.docker.RemoveIfExists(ctx, id); err != nil {
				return err
			}
			id = ""
		}
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
	if err := a.docker.Pull(ctx, image, func(string) {}); err != nil {
		return err
	}
	body := map[string]any{
		"Image":  image,
		"Env":    env,
		"Cmd":    []string{"caddy", "run", "--config", "/etc/caddy/Caddyfile"},
		"Labels": map[string]string{"orchestrator.role": "proxy", "orchestrator.proxy": mode},
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

// tokenFingerprint identifies a token in a label without revealing it.
func tokenFingerprint(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:6])
}

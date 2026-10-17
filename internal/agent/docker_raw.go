package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// The methods below let the control plane replace its own container. They work on
// Docker's raw inspect documents so a recreated container keeps every setting.

// ImageInfo is the part of an image inspect result updates care about.
type ImageInfo struct {
	ID     string            `json:"Id"`
	Labels map[string]string `json:"-"`
}

// ImageInspect describes a local image by name or ID.
func (d *Docker) ImageInspect(ctx context.Context, ref string) (*ImageInfo, error) {
	var v struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := d.call(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, &v); err != nil {
		return nil, err
	}
	return &ImageInfo{ID: v.ID, Labels: v.Config.Labels}, nil
}

// ContainerNameOf resolves a container ID or name to its ID, name and image.
func (d *Docker) ContainerNameOf(ctx context.Context, id string) (cid, name, imageID, imageRef string, err error) {
	var v struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Image  string `json:"Image"`
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}
	if err = d.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &v); err != nil {
		return
	}
	return v.ID, strings.TrimPrefix(v.Name, "/"), v.Image, v.Config.Image, nil
}

// Health reports a container's running state and health ("" when it has no healthcheck).
func (d *Docker) Health(ctx context.Context, id string) (running bool, health string, err error) {
	var v struct {
		State struct {
			Running bool `json:"Running"`
			Health  *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
	}
	if err = d.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &v); err != nil {
		return
	}
	if v.State.Health != nil {
		health = v.State.Health.Status
	}
	return v.State.Running, health, nil
}

// RecreateFrom creates (without starting) a container called name that copies the settings
// of the inspected container, running image instead. It returns the new container's ID.
func (d *Docker) RecreateFrom(ctx context.Context, inspect json.RawMessage, name, image string) (string, error) {
	var src struct {
		Config          map[string]any `json:"Config"`
		HostConfig      map[string]any `json:"HostConfig"`
		NetworkSettings struct {
			Networks map[string]struct {
				Aliases []string `json:"Aliases"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(inspect, &src); err != nil {
		return "", err
	}
	body := src.Config
	body["Image"] = image
	delete(body, "Hostname") // the old container's ID would stick
	body["HostConfig"] = src.HostConfig

	first, _ := src.HostConfig["NetworkMode"].(string)
	if _, ok := src.NetworkSettings.Networks[first]; !ok {
		first = ""
		for n := range src.NetworkSettings.Networks {
			first = n
			break
		}
	}
	if first != "" {
		body["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{first: map[string]any{}}}
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := d.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, body, &out); err != nil {
		return "", err
	}
	for n := range src.NetworkSettings.Networks {
		if n == first || n == "none" || n == "host" {
			continue
		}
		if err := d.call(ctx, http.MethodPost, "/networks/"+n+"/connect", nil, map[string]any{"Container": out.ID}, nil); err != nil {
			return out.ID, fmt.Errorf("connect %s: %w", n, err)
		}
	}
	return out.ID, nil
}

// Available reports whether the Docker socket is reachable.
func (d *Docker) Available(ctx context.Context) bool {
	return d.call(ctx, http.MethodGet, "/_ping", nil, nil, nil) == nil
}

// InContainer reports whether this process runs inside a container.
func InContainer() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// DisableRestart sets a container's restart policy to "no" so a retired container stays down.
func (d *Docker) DisableRestart(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodPost, "/containers/"+id+"/update", nil,
		map[string]any{"RestartPolicy": map[string]string{"Name": "no"}}, nil)
}

// RemoveIfExists removes a container, ignoring "no such container".
func (d *Docker) RemoveIfExists(ctx context.Context, id string) error {
	if err := d.Remove(ctx, id); err != nil && !isStatus(err, http.StatusNotFound) {
		return err
	}
	return nil
}

// CreateHelper creates (without starting) a short-lived container used to replace another.
func (d *Docker) CreateHelper(ctx context.Context, name, image string, cmd []string, binds []string) (string, error) {
	body := map[string]any{
		"Image":      image,
		"Entrypoint": []string{"orchestrator-server"},
		"Cmd":        cmd,
		"User":       "0",
		"Labels":     map[string]string{"orchestrator.role": "updater"},
		"HostConfig": map[string]any{"Binds": binds, "AutoRemove": true, "NetworkMode": "none"},
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := d.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// hasImage reports whether an image reference exists locally.
func (d *Docker) hasImage(ctx context.Context, ref string) bool {
	_, err := d.ImageInspect(ctx, ref)
	return err == nil
}

// Package selfupdate lets the control plane replace its own Docker container with a newer
// image. A container can't restart itself, so the running server starts a short-lived
// helper (from the new image) that swaps containers and rolls back if the new one doesn't
// come up healthy.
package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/agent"
)

const socketPath = "/var/run/docker.sock"

// Self describes the container this process runs in.
type Self struct {
	ID       string
	Name     string
	ImageID  string
	ImageRef string
	// SocketBind is the host path of the Docker socket mounted into this container.
	SocketBind string
	// Mounts are the container's mounts; they tell where its folders really live on the host.
	Mounts []Mount
}

// Mount is one mount of the server container.
type Mount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

// Identify finds the container this process runs in. It fails when there is no Docker
// socket or the process isn't inside a container managed by that engine.
func Identify(ctx context.Context, d *agent.Docker) (*Self, error) {
	if !d.Available(ctx) {
		return nil, errors.New("the Docker socket is not available inside the server container")
	}
	var id, name, imageID, ref string
	var err error
	tried := candidateIDs()
	for _, c := range tried {
		if id, name, imageID, ref, err = d.ContainerNameOf(ctx, c); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("the server could not find its own container through the Docker socket (tried %s). "+
			"If it was installed by hand, set ORCH_CONTAINER=<container name> on it, or re-run install-server.sh", strings.Join(tried, ", "))
	}
	raw, err := d.InspectRaw(ctx, id)
	if err != nil {
		return nil, err
	}
	var v struct {
		Mounts []Mount `json:"Mounts"`
	}
	json.Unmarshal(raw, &v)
	s := &Self{ID: id, Name: name, ImageID: imageID, ImageRef: ref, SocketBind: socketPath, Mounts: v.Mounts}
	for _, m := range v.Mounts {
		if m.Destination == socketPath {
			s.SocketBind = m.Source
		}
	}
	if strings.HasPrefix(ref, "sha256:") || ref == "" || !strings.ContainsAny(ref, "/:") {
		// Created from an image ID (as after a swap by an older build): use a tag the image carries.
		img, ierr := d.ImageInspect(ctx, imageID)
		if ierr == nil && len(img.RepoTags) > 0 {
			ref = img.RepoTags[0]
			for _, t := range img.RepoTags {
				if strings.HasSuffix(t, ":stable") || strings.HasSuffix(t, ":dev") {
					ref = t
					break
				}
			}
			s.ImageRef = ref
		} else {
			return nil, errors.New("the server container was created from an image ID, not a tag, and the image has no tag, so there is nothing to pull. Re-create it with install-server.sh")
		}
	}
	return s, nil
}

// candidateIDs lists the names under which Docker may know this container: an explicit
// ORCH_CONTAINER override, the hostname (the short ID unless --hostname was set) and the
// container IDs visible in /proc (which survive a custom hostname or an old swap).
func candidateIDs() []string {
	var out []string
	seen := map[string]bool{}
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	add(os.Getenv("ORCH_CONTAINER"))
	host, _ := os.Hostname()
	add(host)
	re := regexp.MustCompile(`[0-9a-f]{64}`)
	for _, f := range []string{"/proc/self/mountinfo", "/proc/self/cgroup"} {
		if b, err := os.ReadFile(f); err == nil {
			for _, m := range re.FindAllString(string(b), 3) {
				add(m)
			}
		}
	}
	return out
}

// Latest is what a pull of the container's image tag turned up.
type Latest struct {
	ImageID string
	Version string
	Newer   bool
}

// Check pulls the image tag the server runs from and compares it with the running image.
func Check(ctx context.Context, d *agent.Docker, s *Self) (*Latest, error) {
	if err := d.Pull(ctx, s.ImageRef, func(string) {}); err != nil {
		return nil, fmt.Errorf("pull %s: %w", s.ImageRef, err)
	}
	img, err := d.ImageInspect(ctx, s.ImageRef)
	if err != nil {
		return nil, err
	}
	v := img.Labels["org.opencontainers.image.version"]
	// Images published before the workflow fix carry the tag name as their version.
	if v == "" || v == "latest" {
		if r := img.Labels["org.opencontainers.image.revision"]; len(r) >= 7 {
			v = r[:7]
		}
	}
	return &Latest{ImageID: img.ID, Version: v, Newer: img.ID != s.ImageID}, nil
}

// StartHelper launches the updater from the new image. It returns as soon as the helper
// is started; the server is stopped by the helper a moment later.
func StartHelper(ctx context.Context, d *agent.Docker, s *Self, newImageID string) error {
	const helper = "orchestrator-updater"
	d.RemoveIfExists(ctx, helper)
	id, err := d.CreateHelper(ctx, helper, newImageID,
		[]string{"self-update", "-container", s.Name, "-image", s.ImageRef},
		[]string{s.SocketBind + ":" + socketPath})
	if err != nil {
		return err
	}
	return d.Start(ctx, id)
}

// SwapOptions configures Swap.
type SwapOptions struct {
	Container string        // name of the container to replace
	Image     string        // image reference to run
	Timeout   time.Duration // how long the new container has to become healthy
	Stable    time.Duration // without a healthcheck: how long it must stay running
	Log       func(string)
}

// Swap replaces the named container with one running opts.Image, keeping the old one (as
// <name>-previous, stopped) for a manual rollback. If the new container fails to start or
// become healthy, the old one is put back and an error is returned.
func Swap(ctx context.Context, d *agent.Docker, o SwapOptions) error {
	logf := o.Log
	if logf == nil {
		logf = func(string) {}
	}
	if o.Timeout == 0 {
		o.Timeout = 90 * time.Second
	}
	if o.Stable == 0 {
		o.Stable = 30 * time.Second
	}
	prev := o.Container + "-previous"
	var orig struct {
		HostConfig struct {
			RestartPolicy struct{ Name string } `json:"RestartPolicy"`
		} `json:"HostConfig"`
	}

	inspect, err := d.InspectRaw(ctx, o.Container)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", o.Container, err)
	}
	if err := d.RemoveIfExists(ctx, prev); err != nil {
		return fmt.Errorf("remove old %s: %w", prev, err)
	}
	logf("stopping " + o.Container)
	if err := d.Rename(ctx, o.Container, prev); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	json.Unmarshal(inspect, &orig)
	policy := orig.HostConfig.RestartPolicy.Name
	if policy == "" || policy == "no" {
		policy = "unless-stopped"
	}
	d.DisableRestart(ctx, prev)
	if err := d.Stop(ctx, prev); err != nil {
		d.Rename(ctx, prev, o.Container)
		d.SetRestart(ctx, o.Container, policy)
		return fmt.Errorf("stop: %w", err)
	}

	rollback := func(cause error) error {
		logf("rolling back: " + cause.Error())
		d.RemoveIfExists(ctx, o.Container)
		var errs []error
		if err := d.Rename(ctx, prev, o.Container); err != nil {
			errs = append(errs, err)
		}
		// The retired copy had its restart policy switched off; without restoring it the
		// server would stay down after the next reboot or crash.
		if err := d.SetRestart(ctx, o.Container, policy); err != nil {
			errs = append(errs, err)
		}
		if err := d.Start(ctx, o.Container); err != nil {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			return fmt.Errorf("%w (rollback also failed: %v)", cause, errors.Join(errs...))
		}
		return fmt.Errorf("%w (rolled back to the previous version)", cause)
	}

	logf("starting the new version")
	id, err := d.RecreateFrom(ctx, inspect, o.Container, o.Image)
	if err != nil {
		return rollback(fmt.Errorf("create: %w", err))
	}
	if err := d.Start(ctx, id); err != nil {
		return rollback(fmt.Errorf("start: %w", err))
	}
	if err := waitHealthy(ctx, d, id, o); err != nil {
		return rollback(err)
	}
	logf("the new version is healthy")
	return nil
}

func waitHealthy(ctx context.Context, d *agent.Docker, id string, o SwapOptions) error {
	deadline := time.Now().Add(o.Timeout)
	var runningSince time.Time
	for time.Now().Before(deadline) {
		running, health, err := d.Health(ctx, id)
		switch {
		case err != nil:
			return err
		case !running:
			return errors.New("the new version exited")
		case health == "healthy":
			return nil
		case health == "unhealthy":
			return errors.New("the new version reported unhealthy")
		case health == "":
			if runningSince.IsZero() {
				runningSince = time.Now()
			} else if time.Since(runningSince) >= o.Stable {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return errors.New("the new version did not become healthy in time")
}

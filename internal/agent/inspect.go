package agent

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// inspectDoc is the part of `docker inspect` that adoption reads.
type inspectDoc struct {
	Name   string `json:"Name"`
	Image  string `json:"Image"` // image ID
	Config struct {
		Image      string            `json:"Image"`
		Env        []string          `json:"Env"`
		Cmd        []string          `json:"Cmd"`
		Entrypoint []string          `json:"Entrypoint"`
		User       string            `json:"User"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		PortBindings  map[string][]struct{ HostIp, HostPort string } `json:"PortBindings"`
		RestartPolicy struct{ Name string }                          `json:"RestartPolicy"`
		NetworkMode   string                                         `json:"NetworkMode"`
		Privileged    bool                                           `json:"Privileged"`
		CapAdd        []string                                       `json:"CapAdd"`
		CapDrop       []string                                       `json:"CapDrop"`
		Devices       []any                                          `json:"Devices"`
		ExtraHosts    []string                                       `json:"ExtraHosts"`
		Memory        int64                                          `json:"Memory"`
		NanoCpus      int64                                          `json:"NanoCpus"`
		Tmpfs         map[string]string                              `json:"Tmpfs"`
		Sysctls       map[string]string                              `json:"Sysctls"`
		SecurityOpt   []string                                       `json:"SecurityOpt"`
		PidMode       string                                         `json:"PidMode"`
		ReadonlyRoot  bool                                           `json:"ReadonlyRootfs"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]any `json:"Networks"`
	} `json:"NetworkSettings"`
}

type imageDoc struct {
	Config struct {
		Env        []string          `json:"Env"`
		Cmd        []string          `json:"Cmd"`
		Entrypoint []string          `json:"Entrypoint"`
		User       string            `json:"User"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
}

// InspectSpec reads a container's configuration and translates it into the settings a
// deployment can recreate. It only reads; nothing about the container changes.
func (d *Docker) InspectSpec(ctx context.Context, id string) (api.ContainerSpec, error) {
	var c inspectDoc
	if err := d.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &c); err != nil {
		return api.ContainerSpec{}, err
	}
	var img imageDoc
	_ = d.call(ctx, http.MethodGet, "/images/"+c.Image+"/json", nil, nil, &img) // image may be gone; then nothing is subtracted
	return specFromInspect(c, img), nil
}

func specFromInspect(c inspectDoc, img imageDoc) api.ContainerSpec {
	spec := api.ContainerSpec{
		Name:    strings.TrimPrefix(c.Name, "/"),
		Image:   c.Config.Image,
		Restart: c.HostConfig.RestartPolicy.Name,
	}
	var unsupported []string
	note := func(cond bool, what string) {
		if cond {
			unsupported = append(unsupported, what)
		}
	}
	// Values the image itself provides come back by themselves; keep only what was set on the container.
	inherited := map[string]bool{}
	for _, e := range img.Config.Env {
		inherited[e] = true
	}
	for _, e := range c.Config.Env {
		if !inherited[e] {
			spec.Env = append(spec.Env, e)
		}
	}
	if !equalStrings(c.Config.Cmd, img.Config.Cmd) {
		spec.Command = c.Config.Cmd
	}
	note(!equalStrings(c.Config.Entrypoint, img.Config.Entrypoint), "custom entrypoint")
	note(c.Config.User != img.Config.User, "user")
	for k, v := range c.Config.Labels {
		if iv, ok := img.Config.Labels[k]; ok && iv == v {
			continue
		}
		if strings.HasPrefix(k, "com.docker.compose.") || strings.HasPrefix(k, "orchestrator.") {
			continue
		}
		if spec.Labels == nil {
			spec.Labels = map[string]string{}
		}
		spec.Labels[k] = v
	}

	// Networking: the default bridge needs nothing, host and one user-defined network are carried over.
	switch mode := c.HostConfig.NetworkMode; {
	case mode == "" || mode == "default" || mode == "bridge":
	case mode == "host" || len(c.NetworkSettings.Networks) <= 1 && !strings.HasPrefix(mode, "container:") && mode != "none":
		spec.Network = mode
	default:
		unsupported = append(unsupported, "network mode "+mode)
	}
	note(len(c.NetworkSettings.Networks) > 1, "more than one network")

	if spec.Network != "host" {
		keys := make([]string, 0, len(c.HostConfig.PortBindings))
		for k := range c.HostConfig.PortBindings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			for _, b := range c.HostConfig.PortBindings[k] {
				if b.HostIp != "" && b.HostIp != "0.0.0.0" && b.HostIp != "::" {
					unsupported = append(unsupported, fmt.Sprintf("port %s bound to %s only", k, b.HostIp))
				}
				port := k
				if strings.HasSuffix(port, "/tcp") {
					port = strings.TrimSuffix(port, "/tcp")
				}
				if b.HostPort != "" {
					port = b.HostPort + ":" + port
				}
				spec.Ports = append(spec.Ports, port)
			}
		}
	}

	for _, m := range c.Mounts {
		src := m.Source
		switch m.Type {
		case "volume":
			src = m.Name
		case "bind":
		default:
			unsupported = append(unsupported, m.Type+" mount at "+m.Destination)
			continue
		}
		v := src + ":" + m.Destination
		if !m.RW {
			v += ":ro"
		}
		spec.Volumes = append(spec.Volumes, v)
	}

	h := c.HostConfig
	note(h.Privileged, "privileged mode")
	note(len(h.CapAdd) > 0, "added capabilities")
	note(len(h.CapDrop) > 0, "dropped capabilities")
	note(len(h.Devices) > 0, "device mappings")
	note(len(h.ExtraHosts) > 0, "extra hosts")
	note(h.Memory > 0, "memory limit")
	note(h.NanoCpus > 0, "CPU limit")
	note(len(h.Tmpfs) > 0, "tmpfs mounts")
	note(len(h.Sysctls) > 0, "sysctls")
	note(len(h.SecurityOpt) > 0, "security options")
	note(h.PidMode != "", "pid mode")
	note(h.ReadonlyRoot, "read-only root filesystem")
	spec.Unsupported = unsupported
	return spec
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

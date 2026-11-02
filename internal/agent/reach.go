package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// ContainerIP returns the container's address on the default bridge network (or its first network).
func (d *Docker) ContainerIP(ctx context.Context, id string) (string, error) {
	var v struct {
		NetworkSettings struct {
			IPAddress string `json:"IPAddress"`
			Networks  map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := d.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &v); err != nil {
		return "", err
	}
	if v.NetworkSettings.IPAddress != "" {
		return v.NetworkSettings.IPAddress, nil
	}
	for _, n := range v.NetworkSettings.Networks {
		if n.IPAddress != "" {
			return n.IPAddress, nil
		}
	}
	return "", nil
}

// publishedTCPPorts lists the container-side TCP ports spec publishes on the host.
func publishedTCPPorts(spec api.DeploySpec) []string {
	var out []string
	for _, m := range spec.Ports {
		host, c, ok := strings.Cut(m, ":")
		if !ok || host == "" || strings.HasSuffix(c, "/udp") {
			continue
		}
		c = strings.TrimSuffix(c, "/tcp")
		if _, err := fmt.Sscanf(c, "%d", new(int)); err == nil {
			out = append(out, c)
		}
	}
	return out
}

// waitReachable dials the container's own address on each published port until it answers, so a
// server that listens only on 127.0.0.1 or on a different port than the one mapped is caught
// here instead of showing as Ready. Containers on host or custom networks are not probed.
func (a *Agent) waitReachable(ctx context.Context, st *steps, id string, spec api.DeploySpec, wait time.Duration) error {
	ports := publishedTCPPorts(spec)
	if len(ports) == 0 || (spec.Network != "" && spec.Network != "bridge") {
		return nil
	}
	ip, err := a.docker.ContainerIP(ctx, id)
	if err != nil || ip == "" {
		st.logf("Skipping the port check: container address unknown")
		return nil
	}
	for _, p := range ports {
		addr := net.JoinHostPort(ip, p)
		st.logf("Checking that %s answers...", addr)
		deadline := time.Now().Add(wait)
		for {
			c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
			if derr == nil {
				c.Close()
				st.logf("Port %s is accepting connections", p)
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("nothing is listening on container port %s (the app must listen on 0.0.0.0:%s, not 127.0.0.1 or another port)", p, p)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}

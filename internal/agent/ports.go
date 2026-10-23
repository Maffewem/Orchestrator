package agent

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// hostPort returns the host side of a Docker-style mapping like "8080:80" or "53:53/udp".
// Mappings without a host port (random assignment) report ok=false.
func hostPort(m string) (port int, proto string, ok bool) {
	proto = "tcp"
	if base, p, found := strings.Cut(m, "/"); found {
		m, proto = base, p
	}
	host, _, found := strings.Cut(m, ":")
	if !found {
		return 0, proto, false
	}
	n, err := strconv.Atoi(host)
	return n, proto, err == nil && n > 0
}

// portConflicts lists the host ports in spec that something else already holds. Ports that
// the service's own current container publishes are fine, since it is stopped before the
// new one starts. inUse reports whether a process on the device is listening on a port.
func portConflicts(spec api.DeploySpec, containers []api.Container, inUse func(proto string, port int) bool) []string {
	var out []string
	for _, m := range spec.Ports {
		port, proto, ok := hostPort(m)
		if !ok {
			continue
		}
		owner, own := "", false
		for _, c := range containers {
			if c.State != "running" && c.State != "restarting" {
				continue
			}
			for _, p := range c.Ports {
				if p.PublicPort == port && p.Type == proto {
					if strings.TrimPrefix(c.Name, "/") == spec.Service {
						own = true
					} else if owner == "" {
						owner = strings.TrimPrefix(c.Name, "/")
					}
				}
			}
		}
		switch {
		case owner != "":
			out = append(out, fmt.Sprintf("port %d/%s is already published by container %q; pick a different host port or stop that container", port, proto, owner))
		case !own && inUse(proto, port):
			out = append(out, fmt.Sprintf("port %d/%s is already in use by another process on this device; pick a different host port", port, proto))
		}
	}
	return out
}

// portBusy tries to bind the port on this machine.
func portBusy(proto string, port int) bool {
	addr := ":" + strconv.Itoa(port)
	if proto == "udp" {
		c, err := net.ListenPacket("udp", addr)
		if err != nil {
			return true
		}
		c.Close()
		return false
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return true
	}
	l.Close()
	return false
}

// portStartError turns Docker's bind failures into a message that names the cause.
func portStartError(err error) error {
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "port is already allocated") || strings.Contains(s, "address already in use") {
		return fmt.Errorf("a host port is already in use: %w", err)
	}
	return err
}

package agent

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// The Linux macvlan driver doesn't let a device talk to its own macvlan containers. A "shim"
// macvlan interface on the device itself, with a spare address and a route to each worker,
// restores that. Nothing here persists across a reboot, so shims are rebuilt from the
// container labels when the agent starts.

func ipRun(ctx context.Context, args ...string) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "ip", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return nil
}

// ensureShim gives the device a route to workerIP through its own macvlan interface.
func ensureShim(ctx context.Context, parent, hostIP string, workerIP net.IP) error {
	if _, err := exec.LookPath("ip"); err != nil {
		return fmt.Errorf("the ip command (iproute2) is missing on this device")
	}
	if ifc, err := net.InterfaceByName(shimName); err != nil || ifc == nil {
		if err := ipRun(ctx, "link", "add", shimName, "link", parent, "type", "macvlan", "mode", "bridge"); err != nil {
			return err
		}
	}
	if err := ipRun(ctx, "addr", "replace", hostIP+"/32", "dev", shimName); err != nil {
		return err
	}
	if err := ipRun(ctx, "link", "set", shimName, "up"); err != nil {
		return err
	}
	return ipRun(ctx, "route", "replace", workerIP.String()+"/32", "dev", shimName)
}

// dropShimRoute removes the route to a worker that has left its dedicated address.
func dropShimRoute(ctx context.Context, workerIP string) {
	if workerIP != "" {
		_ = ipRun(ctx, "route", "del", workerIP+"/32", "dev", shimName)
	}
}

// heldLease is a DHCP address the agent keeps renewing for a worker.
type heldLease struct {
	parent string
	mac    net.HardwareAddr
	lease  *dhcpLease
	renew  time.Time
}

type leaseTable struct {
	mu sync.Mutex
	m  map[string]*heldLease // by container name
}

func (a *Agent) holdLease(service, parent string, mac net.HardwareAddr, l *dhcpLease) {
	a.leases.mu.Lock()
	defer a.leases.mu.Unlock()
	if a.leases.m == nil {
		a.leases.m = map[string]*heldLease{}
	}
	a.leases.m[service] = &heldLease{parent: parent, mac: mac, lease: l, renew: time.Now().Add(max(l.Lease/2, time.Minute))}
}

func (a *Agent) dropLease(service string) {
	a.leases.mu.Lock()
	delete(a.leases.m, service)
	a.leases.mu.Unlock()
}

// dedicatedLoop renews leases and rebuilds host shims. Leases are rediscovered from the
// containers' labels, so a restart of the agent loses nothing.
func (a *Agent) dedicatedLoop(ctx context.Context) {
	a.restoreDedicated(ctx)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.leases.mu.Lock()
		var due []string
		for name, h := range a.leases.m {
			if time.Now().After(h.renew) {
				due = append(due, name)
			}
		}
		a.leases.mu.Unlock()
		for _, name := range due {
			a.renewLease(ctx, name)
		}
	}
}

func (a *Agent) renewLease(ctx context.Context, name string) {
	a.leases.mu.Lock()
	h := a.leases.m[name]
	a.leases.mu.Unlock()
	if h == nil {
		return
	}
	if id, _ := a.docker.ContainerByName(ctx, name); id == "" {
		a.dropLease(name) // the worker is gone: let the lease lapse
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	l, err := newDHCPClient(h.parent, h.mac).Renew(cctx, h.lease)
	cancel()
	a.leases.mu.Lock()
	defer a.leases.mu.Unlock()
	if err != nil {
		a.log.Warn("could not renew the DHCP lease", "worker", name, "err", err)
		h.renew = time.Now().Add(time.Minute)
		return
	}
	h.lease, h.renew = l, time.Now().Add(max(l.Lease/2, time.Minute))
}

// restoreDedicated picks up leases and shims for workers already running with a dedicated address.
func (a *Agent) restoreDedicated(ctx context.Context) {
	cs, err := a.docker.Containers(ctx)
	if err != nil {
		return
	}
	for _, c := range cs {
		mode := c.Labels[labelDedicated]
		if mode == "" || c.State != "running" {
			continue
		}
		var ip net.IP
		var parent string
		for _, n := range c.Networks {
			if strings.HasPrefix(n.Name, macvlanPrefix) {
				ip, parent = net.ParseIP(n.IP), strings.TrimPrefix(n.Name, macvlanPrefix)
			}
		}
		if ip == nil {
			continue
		}
		if mode == api.DedicatedDHCP {
			if mac, err := parseMAC(c.Labels[labelDedMAC]); err == nil {
				a.holdLease(c.Name, parent, mac, &dhcpLease{IP: ip, Lease: time.Minute})
			}
		}
		if hip := c.Labels[labelDedHost]; hip != "" {
			if err := ensureShim(ctx, parent, hip, ip); err != nil {
				a.log.Warn("could not restore the device's route to a worker", "worker", c.Name, "err", err)
			}
		}
	}
}

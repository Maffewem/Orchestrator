package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// Host-level options let a container reach beyond itself: a bind mount of a host folder
// (including the Docker socket or the device's root filesystem), the host or another
// container's network namespace, and any Compose file (which can ask for privileged mode,
// added capabilities, devices, pid/ipc host and bind mounts). Anyone who can set them can
// take over the device, so changing them needs an administrator. Operators keep deploying,
// restarting, rolling back and editing environment variables.

const errHostAccessAdmin = "only administrators can change host-level options (host folder mounts, host or shared networking, reserved labels, Compose files) or a worker's own network address: the first give a container control of the device, and an own address can place a worker on another VLAN or on an address the network trusts. Operators can still deploy, restart, roll back and edit environment variables"

// dedicatedAccessPrefix starts the host-access strings that describe own network addresses.
const dedicatedAccessPrefix = "Dedicated network address"

// namedVolumeRe matches a Docker named volume; anything else in a volume source is a host path.
var namedVolumeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// hostAccessOf lists the host-level options a project uses, in a stable order. The strings are
// shown to people, and also compared so an operator can save a project without touching them.
func hostAccessOf(src store.Source, rt store.Runtime) []string {
	var out []string
	for _, v := range rt.Volumes {
		if src.Type == store.SourceCompose {
			break // a stack's volumes come from its file
		}
		source, _, _ := strings.Cut(strings.TrimSpace(v), ":")
		if !namedVolumeRe.MatchString(source) {
			out = append(out, "Host folder mount "+v)
		}
	}
	if n := strings.TrimSpace(rt.Network); n == "host" || strings.HasPrefix(n, "container:") {
		out = append(out, "Network mode "+n)
	}
	// Labels in these namespaces tell Orchestrator and Docker Compose which containers are theirs
	// (projects, the proxy, the tunnel, build sandboxes). A worker must not claim them.
	for k := range rt.Labels {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "orchestrator.") || strings.HasPrefix(lk, "com.docker.") {
			out = append(out, "Reserved container label "+k)
		}
	}
	if d := rt.Dedicated; d != nil {
		out = append(out, dedicatedAccessPrefix+" ("+d.Mode+" "+d.IP+" "+d.HostIP+")")
	}
	copies := make([]string, 0, len(rt.CopyAddresses))
	for dev, d := range rt.CopyAddresses {
		if d != nil {
			copies = append(copies, dedicatedAccessPrefix+" for another copy ("+dev+" "+d.Mode+" "+d.IP+" "+d.HostIP+")")
		}
	}
	out = append(out, copies...)
	if src.Type == store.SourceCompose {
		if src.Repo != "" {
			out = append(out, "Compose file "+src.ComposeFile+" from "+src.Repo)
		} else {
			sum := sha256.Sum256([]byte(src.Compose))
			out = append(out, "Compose file (inline, "+hex.EncodeToString(sum[:4])+")")
		}
	}
	sort.Strings(out)
	return out
}

// mayUseHostAccess reports whether the caller is an administrator.
func mayUseHostAccess(r *http.Request) bool {
	id := identityFrom(r)
	return id != nil && id.Role == store.RoleAdmin
}

// hostAccessAllowed decides whether the caller may save a project that uses want, given what
// the stored project already uses. Operators may keep what is already there but not add or change any.
func hostAccessAllowed(r *http.Request, had, want []string) bool {
	if mayUseHostAccess(r) {
		return true
	}
	have := map[string]bool{}
	for _, h := range had {
		have[h] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// networkAccessLines lists each copy's own address in plain words, naming the copy by its device,
// for example "Claude (main): own address 192.168.5.2, device access address 192.168.5.250".
func (s *Server) networkAccessLines(ctx context.Context, p *store.Project) []string {
	line := func(deviceID, role string, d *api.DedicatedIP) string {
		name := "A removed device"
		if dev, err := s.store.GetDevice(ctx, deviceID); err == nil {
			name = dev.Name
		}
		own := "own address from DHCP"
		if d.Mode == api.DedicatedStatic {
			own = "own address " + d.IP
		}
		out := fmt.Sprintf("%s (%s): %s", name, role, own)
		if d.HostIP != "" {
			out += ", device access address " + d.HostIP
		}
		if d.VLAN > 0 {
			out += fmt.Sprintf(", VLAN %d", d.VLAN)
		}
		return out
	}
	var out []string
	if d := p.Runtime.Dedicated; d != nil {
		out = append(out, line(p.DeviceID, "main", d))
	}
	var ids []string
	for id, d := range p.Runtime.CopyAddresses {
		if d != nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, line(id, "replica", p.Runtime.CopyAddresses[id]))
	}
	return out
}

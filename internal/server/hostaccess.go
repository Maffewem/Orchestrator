package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/Maffewem/Orchestrator/internal/store"
)

// Host-level options let a container reach beyond itself: a bind mount of a host folder
// (including the Docker socket or the device's root filesystem), the host or another
// container's network namespace, and any Compose file (which can ask for privileged mode,
// added capabilities, devices, pid/ipc host and bind mounts). Anyone who can set them can
// take over the device, so changing them needs an administrator. Operators keep deploying,
// restarting, rolling back and editing environment variables.

const errHostAccessAdmin = "only administrators can use host-level options (host folder mounts, host or shared networking, Compose files), because they give a container control of the device; operators can still deploy, restart, roll back and edit environment variables"

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

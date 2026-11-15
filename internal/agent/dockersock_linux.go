//go:build linux

package agent

import (
	"os"
	"syscall"
)

// dockerSocketGroup returns the group that owns the Docker socket, which the unprivileged
// server container must join to use it.
func dockerSocketGroup() (int, bool) {
	fi, err := os.Stat("/var/run/docker.sock")
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Gid), true
}

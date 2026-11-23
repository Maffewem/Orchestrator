//go:build unix

package server

import (
	"errors"
	"syscall"
)

// freeBytes reports the space available to unprivileged writers on the filesystem holding dir.
func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

func isNoSpace(err error) bool { return errors.Is(err, syscall.ENOSPC) }

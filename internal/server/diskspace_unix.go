//go:build unix

package server

import (
	"errors"
	"syscall"
)

// diskSpace reports the total size and the space available to unprivileged writers on the
// filesystem holding dir.
func diskSpace(dir string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Blocks) * uint64(st.Bsize), uint64(st.Bavail) * uint64(st.Bsize), nil
}

func isNoSpace(err error) bool { return errors.Is(err, syscall.ENOSPC) }

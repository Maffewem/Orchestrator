//go:build unix

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockState takes an exclusive lock next to the state file, so two agent processes can never run
// with the same identity (a duplicate service, or a container and a systemd unit). Two of them
// would both collect the device's commands and run deployments side by side. The lock is held
// until the process exits and is released by the kernel even if it is killed.
func LockState(statePath string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(statePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another agent is already running with %s (stop it first, or use a different -state path)", statePath)
	}
	return func() { f.Close() }, nil
}

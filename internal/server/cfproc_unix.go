//go:build unix

package server

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup makes cmd lead its own process group and kill the whole group when its
// context ends, so a cancelled build doesn't leave npm's children running.
func ownProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

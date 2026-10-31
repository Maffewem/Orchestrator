//go:build !unix

package server

import "os/exec"

func ownProcessGroup(cmd *exec.Cmd) {}

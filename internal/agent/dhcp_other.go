//go:build !linux

package agent

import "syscall"

// dhcpControl has nothing to set on platforms where dedicated addresses are unsupported.
func dhcpControl(string) func(network, address string, c syscall.RawConn) error { return nil }

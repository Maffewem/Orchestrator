//go:build !linux

package agent

import (
	"errors"
	"syscall"
)

// dhcpControl has nothing to set on platforms where dedicated addresses are unsupported.
func dhcpControl(string) func(network, address string, c syscall.RawConn) error { return nil }

func newRawRecv(string) (rawReader, error) {
	return nil, errors.New("packet sockets are only used on Linux")
}

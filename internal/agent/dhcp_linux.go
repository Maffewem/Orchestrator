//go:build linux

package agent

import "syscall"

// dhcpControl allows broadcasts and a shared port 68 (a DHCP client may already run on the
// device) and, when iface is set, keeps the socket on that interface.
func dhcpControl(iface string) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			h := int(fd)
			if serr = syscall.SetsockoptInt(h, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); serr != nil {
				return
			}
			if serr = syscall.SetsockoptInt(h, syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1); serr != nil {
				return
			}
			if iface != "" {
				serr = syscall.SetsockoptString(h, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, iface)
			}
		})
		if err != nil {
			return err
		}
		return serr
	}
}

//go:build linux

package agent

import (
	"net"
	"os"
	"path/filepath"
)

// defaultRoute returns the interface carrying the IPv4 default route and its gateway.
func defaultRoute() (string, net.IP) {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return "", nil
	}
	return parseDefaultRoute(string(b))
}

// isWireless reports whether the interface is a Wi-Fi adapter.
func isWireless(iface string) bool {
	base := filepath.Join("/sys/class/net", iface)
	for _, p := range []string{"wireless", "phy80211"} {
		if _, err := os.Stat(filepath.Join(base, p)); err == nil {
			return true
		}
	}
	return false
}

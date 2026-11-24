//go:build !linux

package agent

import "net"

func defaultRoute() (string, net.IP) { return "", nil }
func isWireless(string) bool         { return false }

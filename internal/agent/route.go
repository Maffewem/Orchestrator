package agent

import (
	"encoding/hex"
	"net"
	"strconv"
	"strings"
)

// parseDefaultRoute reads Linux's /proc/net/route and returns the interface and gateway of the
// lowest-metric default route. Addresses there are hexadecimal in the kernel's byte order.
func parseDefaultRoute(table string) (string, net.IP) {
	best, bestMetric := "", int64(-1)
	var gw net.IP
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(f[3], 16, 32)
		if err != nil || flags&0x3 != 0x3 { // route is up and uses a gateway
			continue
		}
		raw, err := hex.DecodeString(f[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		metric, _ := strconv.ParseInt(f[6], 10, 64)
		if bestMetric < 0 || metric < bestMetric {
			best, bestMetric, gw = f[0], metric, net.IPv4(raw[3], raw[2], raw[1], raw[0])
		}
	}
	return best, gw
}

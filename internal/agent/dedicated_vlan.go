package agent

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// 802.1Q VLAN support for dedicated addresses. A worker on VLAN 1005 of eth0 attaches to the
// tagged sub-interface eth0.1005, which then serves as the macvlan parent. One physical port
// can therefore carry several VLANs, as long as the switch port is a trunk that carries them.

// splitVLAN reads a sub-interface name like eth0.1005 into its physical interface and VLAN ID.
func splitVLAN(name string) (phys string, id int, ok bool) {
	i := strings.LastIndex(name, ".")
	if i <= 0 {
		return "", 0, false
	}
	id, err := strconv.Atoi(name[i+1:])
	if err != nil || id < 1 || id > 4094 {
		return "", 0, false
	}
	return name[:i], id, true
}

// ensureVLAN makes sure the tagged sub-interface for VLAN id on phys exists and is up, and returns
// its name. The interface isn't persistent: it is recreated when the agent starts, and Docker
// recreates it for its macvlan network.
func ensureVLAN(ctx context.Context, phys string, id int) (string, error) {
	if id < 1 || id > 4094 {
		return "", fmt.Errorf("the VLAN ID must be between 1 and 4094")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		return "", fmt.Errorf("the ip command (iproute2) is missing on this device, and a VLAN needs it")
	}
	if _, err := net.InterfaceByName(phys); err != nil {
		return "", fmt.Errorf("network interface %q not found on this device", phys)
	}
	name := fmt.Sprintf("%s.%d", phys, id)
	if len(name) > 15 {
		return "", fmt.Errorf("%q is too long for a Linux interface name (15 characters)", name)
	}
	if _, err := net.InterfaceByName(name); err != nil {
		if err := ipRun(ctx, "link", "add", "link", phys, "name", name, "type", "vlan", "id", strconv.Itoa(id)); err != nil {
			return "", fmt.Errorf("create VLAN %d on %s: %w (the agent needs root and the 8021q kernel module)", id, phys, err)
		}
	}
	if err := ipRun(ctx, "link", "set", name, "up"); err != nil {
		return "", err
	}
	return name, nil
}

// deleteVLAN removes a VLAN sub-interface (and any macvlan interface on it, such as the device
// access shim). One that carries an IPv4 address was set up by the user and is left alone.
func deleteVLAN(ctx context.Context, name string) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return
		}
	}
	_ = ipRun(ctx, "link", "del", name)
}

// avoidSet reads the comma-separated addresses the server knows are taken.
func avoidSet(list string) map[string]bool {
	m := map[string]bool{}
	for _, a := range strings.Split(list, ",") {
		if a = strings.TrimSpace(a); a != "" {
			m[a] = true
		}
	}
	return m
}

// vlanState describes the sub-interface for an error message: whether it exists, is up and has a
// carrier, and the reverse-path filter setting that can drop replies.
func vlanState(l *lan) string {
	ifc, err := net.InterfaceByName(l.Parent)
	if err != nil {
		return fmt.Sprintf("The sub-interface %s does not exist.", l.Parent)
	}
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			return "?"
		}
		return strings.TrimSpace(string(b))
	}
	return fmt.Sprintf("Sub-interface %s on %s: exists, flags %v, state %s, carrier %s, rp_filter %s/%s.",
		l.Parent, l.physical(), ifc.Flags, read("/sys/class/net/"+l.Parent+"/operstate"), read("/sys/class/net/"+l.Parent+"/carrier"),
		read("/proc/sys/net/ipv4/conf/all/rp_filter"), read("/proc/sys/net/ipv4/conf/"+l.Parent+"/rp_filter"))
}

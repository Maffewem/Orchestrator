package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Dedicated addresses attach a worker to the device's LAN through a Docker macvlan network:
// the worker gets its own MAC and IP, so it can listen on ports (80, 443, ...) the device
// itself uses. Published ports don't apply; the worker is reached on its own address.

const (
	macvlanPrefix  = "orch-lan-"
	shimName       = "orch-shim"
	labelDedicated = "orchestrator.dedicated"
	labelDedMAC    = "orchestrator.dedicated-mac"
	labelDedHost   = "orchestrator.dedicated-host-ip"
)

// lan is the local network a macvlan attaches to.
type lan struct {
	Parent  string
	Subnet  *net.IPNet
	Gateway net.IP
	Own     []net.IP // addresses the device itself holds on any interface
}

// findLAN describes the network of parent (or of the default route when parent is empty).
func findLAN(parent string) (*lan, error) {
	route, gw := defaultRoute()
	if parent == "" {
		parent = route
	}
	if parent == "" {
		return nil, errors.New("this device has no default route, so there is no network to attach to; choose the interface in the worker's settings")
	}
	ifc, err := net.InterfaceByName(parent)
	if err != nil {
		return nil, fmt.Errorf("network interface %q not found on this device", parent)
	}
	l := &lan{Parent: parent}
	all, _ := net.Interfaces()
	for _, i := range all {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				l.Own = append(l.Own, ipn.IP)
			}
		}
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
			l.Subnet = &net.IPNet{IP: ipn.IP.Mask(ipn.Mask), Mask: ipn.Mask}
			break
		}
	}
	if l.Subnet == nil {
		return nil, fmt.Errorf("%s has no IPv4 address, so there is no subnet to join", parent)
	}
	if parent == route && gw != nil && l.Subnet.Contains(gw) {
		l.Gateway = gw
	}
	return l, nil
}

// parseMAC accepts a unicast, locally administered address, which is what Orchestrator generates.
func parseMAC(s string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(s)
	if err != nil || len(mac) != 6 {
		return nil, fmt.Errorf("%q is not a MAC address like 02:ab:cd:12:34:56", s)
	}
	if mac[0]&1 != 0 {
		return nil, fmt.Errorf("%s is a multicast address; the first byte must be even", s)
	}
	return mac, nil
}

// staticProblems lists why ip can't be a worker's address on l.
func staticProblems(ip net.IP, l *lan) []string {
	var out []string
	switch {
	case ip.To4() == nil:
		return []string{fmt.Sprintf("%v is not an IPv4 address", ip)}
	case !l.Subnet.Contains(ip):
		return []string{fmt.Sprintf("%s is outside this device's network %s", ip, l.Subnet)}
	}
	ones, bits := l.Subnet.Mask.Size()
	base := l.Subnet.IP.To4()
	last := make(net.IP, 4)
	for i := range last {
		last[i] = base[i] | ^l.Subnet.Mask[i]
	}
	if bits-ones > 1 && (ip.Equal(base) || ip.Equal(last)) {
		out = append(out, fmt.Sprintf("%s is the network or broadcast address of %s", ip, l.Subnet))
	}
	if l.Gateway != nil && ip.Equal(l.Gateway) {
		out = append(out, fmt.Sprintf("%s is this network's gateway", ip))
	}
	for _, o := range l.Own {
		if ip.Equal(o) {
			out = append(out, fmt.Sprintf("%s is already an address of this device itself", ip))
		}
	}
	return out
}

// probeIP asks the LAN whether anything answers for ip. arping (duplicate address detection)
// is exact; ping is the fallback and misses hosts that ignore it. checked is false when no
// tool was available or none could run.
func probeIP(ctx context.Context, iface string, ip net.IP) (inUse, checked bool) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if p, err := exec.LookPath("arping"); err == nil {
		// iputils: exit 0 means no duplicate; 1 means a reply came back. Other arping builds
		// differ, so anything else falls through to ping.
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, p, "-D", "-c", "2", "-w", "3", "-I", iface, ip.String())
		cmd.Stdout = &out
		err := cmd.Run()
		if err == nil {
			return false, true
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return true, true
		}
	}
	if p, err := exec.LookPath("ping"); err == nil {
		err := exec.CommandContext(ctx, p, "-c", "2", "-W", "1", "-I", iface, ip.String()).Run()
		if err == nil {
			return true, true
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return false, true
		}
	}
	return false, false
}

// checkDedicated validates a dedicated address without changing anything. current is the address
// the worker's own running container already holds, which is allowed to answer.
func (a *Agent) checkDedicated(ctx context.Context, d api.DedicatedIP, current string) api.NetworkCheck {
	var out api.NetworkCheck
	bad := func(f string, args ...any) { out.Problems = append(out.Problems, fmt.Sprintf(f, args...)) }
	if runtime.GOOS != "linux" {
		bad("dedicated addresses need a Linux device")
		return out
	}
	l, err := findLAN(d.Parent)
	if err != nil {
		bad("%v", err)
		return out
	}
	out.Parent, out.Subnet = l.Parent, l.Subnet.String()
	if l.Gateway != nil {
		out.Gateway = l.Gateway.String()
	}
	if isWireless(l.Parent) {
		out.Wireless = true
		bad("%s is a Wi-Fi interface. Wi-Fi access points accept only one MAC address per client, so a worker with its own MAC can't join the network that way. Use a wired interface.", l.Parent)
		return out
	}
	mac, err := parseMAC(d.MAC)
	if err != nil {
		bad("%v", err)
		return out
	}
	switch d.Mode {
	case api.DedicatedStatic:
		ip := net.ParseIP(d.IP)
		if ip == nil {
			bad("%q is not an IP address", d.IP)
			break
		}
		if ps := staticProblems(ip, l); len(ps) > 0 {
			out.Problems = append(out.Problems, ps...)
			break
		}
		if d.IP != current {
			switch inUse, checked := probeIP(ctx, l.Parent, ip); {
			case inUse:
				bad("something on the network already answers for %s", d.IP)
			case !checked:
				out.Notes = append(out.Notes, "Couldn't check whether the address is free: install arping or ping on the device. Make sure nothing else uses it.")
			}
		}
	case api.DedicatedDHCP:
		cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		offer, err := newDHCPClient(l.Parent, mac).Offer(cctx)
		cancel()
		if err != nil {
			bad("DHCP: %v", err)
			break
		}
		out.Offer = offer.IP.String()
		if !l.Subnet.Contains(offer.IP) {
			bad("the DHCP server offered %s, which is outside %s", offer.IP, l.Subnet)
		}
		out.Notes = append(out.Notes, fmt.Sprintf("Reserve %s for %s in your router to keep the address fixed.", offer.IP, mac))
	default:
		bad("mode must be dhcp or static")
	}
	if d.HostIP != "" {
		hip := net.ParseIP(d.HostIP)
		switch {
		case hip == nil:
			bad("%q is not an IP address for the device's own access", d.HostIP)
		default:
			if ps := staticProblems(hip, l); len(ps) > 0 {
				for _, p := range ps {
					bad("device access address: %s", p)
				}
			}
		}
	} else {
		out.Notes = append(out.Notes, "Without a device access address, this device itself can't reach the worker: the Linux macvlan driver blocks traffic between a host and its own containers. Other devices on the network can.")
	}
	return out
}

// macvlanNet makes sure the macvlan network for l exists and returns its name.
func (d *Docker) macvlanNet(ctx context.Context, l *lan) (string, error) {
	name := macvlanPrefix + l.Parent
	var nets []struct {
		Name       string
		Driver     string
		Options    map[string]string
		Containers map[string]any
		IPAM       struct {
			Config []struct{ Subnet, Gateway string }
		}
	}
	filter := fmt.Sprintf(`{"name":[%q]}`, name)
	if err := d.call(ctx, http.MethodGet, "/networks", url.Values{"filters": {filter}}, nil, &nets); err != nil {
		return "", err
	}
	for _, n := range nets {
		if n.Name != name {
			continue
		}
		same := n.Driver == "macvlan" && n.Options["parent"] == l.Parent && len(n.IPAM.Config) > 0 && n.IPAM.Config[0].Subnet == l.Subnet.String()
		if same && (l.Gateway == nil || n.IPAM.Config[0].Gateway == "" || n.IPAM.Config[0].Gateway == l.Gateway.String()) {
			return name, nil
		}
		if len(n.Containers) > 0 {
			return "", fmt.Errorf("the macvlan network %s was set up for another subnet and other workers still use it; move or redeploy them first", name)
		}
		if err := d.call(ctx, http.MethodDelete, "/networks/"+name, nil, nil, nil); err != nil {
			return "", err
		}
	}
	cfg := map[string]string{"Subnet": l.Subnet.String()}
	if l.Gateway != nil {
		cfg["Gateway"] = l.Gateway.String()
	}
	body := map[string]any{
		"Name":    name,
		"Driver":  "macvlan",
		"Options": map[string]string{"parent": l.Parent},
		"IPAM":    map[string]any{"Driver": "default", "Config": []map[string]string{cfg}},
		"Labels":  map[string]string{"orchestrator.managed": "1"},
	}
	if err := d.call(ctx, http.MethodPost, "/networks/create", nil, body, nil); err != nil {
		return "", fmt.Errorf("create macvlan network on %s: %w", l.Parent, err)
	}
	return name, nil
}

// removeMacvlanIfUnused deletes the macvlan network for parent when no container uses it.
func (d *Docker) removeMacvlanIfUnused(ctx context.Context, parent string) {
	_ = d.call(ctx, http.MethodDelete, "/networks/"+macvlanPrefix+parent, nil, nil, nil)
}

// attachment is what Create needs to put a container on its dedicated address.
type attachment struct {
	Network string
	IP      net.IP
	MAC     net.HardwareAddr
	Lease   *dhcpLease // set in DHCP mode
	LAN     *lan
}

// prepareDedicated validates the settings, leases an address in DHCP mode and creates the network.
func (a *Agent) prepareDedicated(ctx context.Context, st *steps, spec api.DeploySpec) (*attachment, error) {
	d := *spec.Dedicated
	if runtime.GOOS != "linux" {
		return nil, errors.New("dedicated addresses need a Linux device")
	}
	if spec.Network != "" {
		return nil, errors.New("a dedicated address can't be combined with another Docker network")
	}
	current := ""
	if id, _ := a.docker.ContainerByName(ctx, spec.Service); id != "" {
		current = a.docker.containerIPOn(ctx, id, macvlanPrefix)
	}
	if d.Mode == api.DedicatedStatic {
		st.logf("Checking that %s is free on the network…", d.IP)
	} else {
		st.logf("Asking the network's DHCP server for an address for %s…", d.MAC)
	}
	chk := a.checkDedicated(ctx, d, current)
	if len(chk.Problems) > 0 {
		for _, p := range chk.Problems {
			st.logf("Error: %s", p)
		}
		return nil, fmt.Errorf("dedicated address: %s", chk.Problems[0])
	}
	for _, n := range chk.Notes {
		st.logf("Note: %s", n)
	}
	l, err := findLAN(d.Parent)
	if err != nil {
		return nil, err
	}
	mac, _ := parseMAC(d.MAC)
	at := &attachment{IP: net.ParseIP(d.IP), MAC: mac, LAN: l}
	if d.Mode == api.DedicatedDHCP {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		lease, err := newDHCPClient(l.Parent, mac).Acquire(cctx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("DHCP: %w", err)
		}
		if !l.Subnet.Contains(lease.IP) {
			return nil, fmt.Errorf("the DHCP server leased %s, which is outside %s", lease.IP, l.Subnet)
		}
		if ps := staticProblems(lease.IP, l); len(ps) > 0 {
			return nil, fmt.Errorf("the DHCP server leased an unusable address: %s", ps[0])
		}
		at.IP, at.Lease = lease.IP, lease
		st.logf("DHCP leased %s to %s for %s", lease.IP, mac, lease.Lease.Round(time.Second))
	}
	if at.Network, err = a.docker.macvlanNet(ctx, l); err != nil {
		return nil, err
	}
	st.logf("Attaching to %s on %s as %s (%s)", at.Network, l.Parent, at.IP, mac)
	return at, nil
}

// containerIPOn returns the container's address on a network whose name starts with prefix.
func (d *Docker) containerIPOn(ctx context.Context, id, prefix string) string {
	var v struct {
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	if err := d.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &v); err != nil {
		return ""
	}
	for n, e := range v.NetworkSettings.Networks {
		if strings.HasPrefix(n, prefix) {
			return e.IPAddress
		}
	}
	return ""
}

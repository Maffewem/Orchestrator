package agent

import (
	"bytes"
	"context"
	"encoding/binary"
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
	// Phys is the physical interface under a VLAN sub-interface (empty without a VLAN). Subnet is
	// nil for a DHCP VLAN until a lease or offer says what the VLAN's network is.
	Phys string
}

// physical is the interface that actually carries the traffic.
func (l *lan) physical() string {
	if l.Phys != "" {
		return l.Phys
	}
	return l.Parent
}

// adopt takes the network from a DHCP lease or offer (used on a VLAN, where the device itself
// has no address to read the subnet from).
func (l *lan) adopt(d *dhcpLease) {
	l.Subnet = d.Subnet()
	if d.Router != nil && l.Subnet.Contains(d.Router) {
		l.Gateway = d.Router
	}
}

// findLAN describes the network d attaches to: the network of d.Parent (or of the default route
// when it is empty), or with a VLAN, the tagged sub-interface on that port, created if missing.
func findLAN(ctx context.Context, d api.DedicatedIP) (*lan, error) {
	parent := d.Parent
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
	if d.VLAN > 0 {
		name, err := ensureVLAN(ctx, parent, d.VLAN)
		if err != nil {
			return nil, err
		}
		l.Parent, l.Phys = name, parent
	}
	all, _ := net.Interfaces()
	for _, i := range all {
		if isShimName(i.Name) {
			// The access interface made for an earlier deployment of a worker: its address is
			// the worker's to reuse, not one the device holds for itself.
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				l.Own = append(l.Own, ipn.IP)
			}
		}
	}
	if d.VLAN > 0 {
		// The device has no address on the VLAN: the subnet is given (static) or learned (DHCP).
		if d.Subnet != "" {
			_, sn, err := net.ParseCIDR(d.Subnet)
			if err != nil || sn.IP.To4() == nil {
				return nil, errors.New("the VLAN's subnet isn't valid; enter the mask, for example 24 or 255.255.255.0")
			}
			l.Subnet = sn
			if g := net.ParseIP(d.Gateway); g != nil && sn.Contains(g) {
				l.Gateway = g
			}
		}
		return l, nil
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
		return nil, errors.New("the MAC address isn't valid; use one like 02:ab:cd:12:34:56")
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
func (a *Agent) checkDedicated(ctx context.Context, d api.DedicatedIP, current string, avoid map[string]bool) api.NetworkCheck {
	var out api.NetworkCheck
	bad := func(f string, args ...any) { out.Problems = append(out.Problems, fmt.Sprintf(f, args...)) }
	if runtime.GOOS != "linux" {
		bad("dedicated addresses need a Linux device")
		return out
	}
	if d.VLAN < 0 || d.VLAN > 4094 {
		bad("the VLAN ID must be between 1 and 4094")
		return out
	}
	if d.VLAN > 0 && d.Mode == api.DedicatedStatic && d.Subnet == "" {
		bad("a static address on a VLAN needs the VLAN's subnet, like 192.168.5.0/24")
		return out
	}
	l, err := findLAN(ctx, d)
	if err != nil {
		bad("%v", err)
		return out
	}
	out.Parent = l.Parent
	if l.Subnet != nil {
		out.Subnet = l.Subnet.String()
	}
	if l.Gateway != nil {
		out.Gateway = l.Gateway.String()
	}
	if isWireless(l.physical()) {
		out.Wireless = true
		bad("%s is a Wi-Fi interface. Wi-Fi access points accept only one MAC address per client, so a worker with its own MAC can't join the network that way. Use a wired interface.", l.physical())
		return out
	}
	if d.VLAN > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("VLAN %d is carried on %s as %s.", d.VLAN, l.physical(), l.Parent))
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
			bad("the worker's IP address isn't valid; enter it like 192.168.5.20")
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
		if d.VLAN > 0 && len(out.Problems) == 0 {
			a.probeVLAN(ctx, d, l, &out)
		}
	case api.DedicatedDHCP:
		cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		offer, err := newDHCPClient(l.Parent, mac).Offer(cctx)
		cancel()
		if err != nil {
			if d.VLAN > 0 {
				bad("DHCP: %v. %s Check that the network for VLAN %d (its VLAN ID must be exactly %d, with DHCP enabled) is tagged on the switch port.", err, vlanState(l), d.VLAN, d.VLAN)
			} else {
				bad("DHCP: %v", err)
			}
			break
		}
		out.Offer = offer.IP.String()
		if d.VLAN > 0 {
			out.VLANReachable = "yes" // the VLAN's DHCP server answered
		}
		if l.Subnet == nil {
			l.adopt(offer)
			out.Subnet = l.Subnet.String()
			if l.Gateway != nil {
				out.Gateway = l.Gateway.String()
			}
		}
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
			bad("Address for this device to reach the worker: that isn't a valid IP address")
		case l.Subnet == nil:
			// a DHCP VLAN whose subnet isn't known yet (no offer)
		default:
			ps := staticProblems(hip, l)
			for _, p := range ps {
				bad("Address for this device to reach the worker: %s", p)
			}
			if len(ps) == 0 && avoid[hip.String()] {
				bad("Address for this device to reach the worker: %s is already used by another worker or device known to Orchestrator", hip)
			} else if len(ps) == 0 && !heldByShim(l.Parent, hip) && !hip.Equal(net.ParseIP(d.IP)) {
				// Our own access interface answers for its address, so only a different one is probed.
				switch inUse, checked := probeIP(ctx, l.Parent, hip); {
				case inUse:
					bad("Address for this device to reach the worker: something on the network already answers for %s", hip)
				case !checked:
					out.Notes = append(out.Notes, "Couldn't check whether the address for this device to reach the worker is free: install arping or ping on the device.")
				}
			} else if len(ps) == 0 && hip.Equal(net.ParseIP(d.IP)) {
				bad("Address for this device to reach the worker: it must differ from the worker's address")
			}
		}
	} else if len(out.Problems) == 0 && l.Subnet != nil {
		skip := []net.IP{net.ParseIP(d.IP)}
		if out.Offer != "" {
			skip = append(skip, net.ParseIP(out.Offer))
		}
		ip, checked := suggestHost(ctx, l, avoid, skip...)
		switch {
		case ip == "":
			out.Notes = append(out.Notes, "Couldn't find a free address to suggest for this device to reach the worker near the top of "+l.Subnet.String()+". Pick one by hand.")
		case !checked:
			out.SuggestHostIP = ip
			out.Notes = append(out.Notes, "Couldn't check whether "+ip+" is free (install arping or ping on the device). Make sure it is outside your DHCP range and unused.")
		default:
			out.SuggestHostIP = ip
			out.Notes = append(out.Notes, ip+" didn't answer on the network. Make sure it is outside your router's DHCP range.")
		}
		out.Notes = append(out.Notes, "Without an address for this device to reach the worker, this device itself can't reach it: the Linux macvlan driver blocks traffic between a host and its own containers. Other devices on the network can.")
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

// removeMacvlanIfUnused deletes the macvlan network for parent when no container uses it and
// reports whether it is gone (Docker refuses while a container is attached).
func (d *Docker) removeMacvlanIfUnused(ctx context.Context, parent string) bool {
	err := d.call(ctx, http.MethodDelete, "/networks/"+macvlanPrefix+parent, nil, nil, nil)
	return err == nil || strings.Contains(strings.ToLower(err.Error()), "not found")
}

// releaseParent cleans up after a worker has left parent: it removes the then-unused macvlan
// network and, for a VLAN sub-interface Orchestrator made, the sub-interface (and with it the
// device access shim on it).
func (a *Agent) releaseParent(ctx context.Context, parent string) {
	if parent == "" || !a.docker.removeMacvlanIfUnused(ctx, parent) {
		return
	}
	if _, _, ok := splitVLAN(parent); ok {
		deleteVLAN(ctx, parent)
	}
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
	chk := a.checkDedicated(ctx, d, current, nil)
	if len(chk.Problems) > 0 {
		for _, p := range chk.Problems {
			st.logf("Error: %s", p)
		}
		return nil, fmt.Errorf("dedicated address: %s", chk.Problems[0])
	}
	for _, n := range chk.Notes {
		st.logf("Note: %s", n)
	}
	l, err := findLAN(ctx, d)
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
		if l.Subnet == nil {
			l.adopt(lease)
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

// containerParentOn returns the parent interface of the container's macvlan network.
func (d *Docker) containerParentOn(ctx context.Context, id string) string {
	var v struct {
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
		}
	}
	if err := d.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &v); err != nil {
		return ""
	}
	for n := range v.NetworkSettings.Networks {
		if strings.HasPrefix(n, macvlanPrefix) {
			return strings.TrimPrefix(n, macvlanPrefix)
		}
	}
	return ""
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

// heldByShim reports whether the device's own access interface for parent already holds ip.
func heldByShim(parent string, ip net.IP) bool {
	ifc, err := net.InterfaceByName(shimFor(parent))
	if err != nil {
		return false
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// hostCandidates lists addresses worth suggesting for the device's own access, most preferred
// first: the top of the subnet (leaving the very last ones to routers), skipping the gateway, the
// device itself, anything in avoid and anything in skip.
func hostCandidates(l *lan, avoid map[string]bool, skip []net.IP, max int) []net.IP {
	if l.Subnet == nil || l.Subnet.IP.To4() == nil {
		return nil
	}
	ones, bits := l.Subnet.Mask.Size()
	if bits-ones < 2 {
		return nil
	}
	base := binary.BigEndian.Uint32(l.Subnet.IP.To4())
	last := base + uint32(1)<<(bits-ones) - 2 // last usable host
	start := last
	if last-base > 16 {
		start = last - 4 // .250 on a /24
	}
	var out []net.IP
	for cand, n := start, 0; cand > base && n < 80 && len(out) < max; cand, n = cand-1, n+1 {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], cand)
		ip := net.IP(b[:])
		if len(staticProblems(ip, l)) > 0 || avoid[ip.String()] {
			continue
		}
		skipped := false
		for _, s := range skip {
			if s != nil && s.Equal(ip) {
				skipped = true
			}
		}
		if !skipped {
			out = append(out, ip)
		}
	}
	return out
}

// suggestHost finds an address near the top of the subnet that nothing answers for. checked is
// false when no probe tool was available, so the address is only likely to be free.
func suggestHost(ctx context.Context, l *lan, avoid map[string]bool, skip ...net.IP) (ip string, checked bool) {
	for _, c := range hostCandidates(l, avoid, skip, 5) {
		inUse, ok := probeIP(ctx, l.Parent, c)
		if !ok {
			return c.String(), false
		}
		if !inUse {
			return c.String(), true
		}
	}
	return "", false
}

// probeVLAN tells whether the VLAN is actually reaching this device: "nothing answers for the
// address" is also what a VLAN that carries no traffic looks like, so it asks for the gateway too
// (the one entered, otherwise the subnet's first address, where routers usually are).
func (a *Agent) probeVLAN(ctx context.Context, d api.DedicatedIP, l *lan, out *api.NetworkCheck) {
	gw, guessed := l.Gateway, false
	if gw == nil && l.Subnet != nil {
		gw, guessed = firstHostIP(l.Subnet), true
	}
	if gw == nil {
		return
	}
	switch inUse, checked := probeIP(ctx, l.Parent, gw); {
	case !checked:
		out.Notes = append(out.Notes, fmt.Sprintf("Couldn't tell whether VLAN %d reaches this device (install arping on the device). The switch port must carry VLAN %d tagged and the router must serve it.", d.VLAN, d.VLAN))
	case inUse:
		out.VLANReachable = "yes"
		out.Notes = append(out.Notes, fmt.Sprintf("The VLAN is working: %s answered on VLAN %d.", gw, d.VLAN))
	case guessed:
		out.VLANReachable = "no"
		out.Notes = append(out.Notes, fmt.Sprintf("Nothing answered on VLAN %d, not even %s (the router's likely address). Either that isn't the router's address, or the VLAN isn't reaching this device: the switch port must carry VLAN %d tagged. Enter the gateway to check it properly.", d.VLAN, gw, d.VLAN))
	default:
		out.VLANReachable = "no"
		out.Problems = append(out.Problems, fmt.Sprintf("Nothing answered on VLAN %d, not even the gateway %s. The VLAN may not be reaching this device: check that the switch port carries VLAN %d tagged, that the router serves it, and that the gateway address is right.", d.VLAN, gw, d.VLAN))
	}
}

// firstHostIP is the first usable address of a subnet.
func firstHostIP(sn *net.IPNet) net.IP {
	b := sn.IP.To4()
	if b == nil {
		return nil
	}
	ip := append(net.IP(nil), b...)
	ip[3]++
	return ip
}

// isShimName reports whether an interface is a device access interface made by this agent
// (orch-shim, or orch-shimv<id> for a VLAN).
func isShimName(name string) bool { return strings.HasPrefix(name, shimName) }

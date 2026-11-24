package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// A worker with a dedicated address gets its own IP and MAC on the device's LAN through a Docker
// macvlan network, so it can use ports (80, 443) the device itself already holds. Its ports are
// served on that address, not published on the device, so it never conflicts with the device's
// ports, and it has no Caddy or tunnel routes (those reach workers through the device's ports).

// newMAC returns a random, locally administered, unicast MAC address.
func newMAC() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	b[0] = 0x02 // locally administered, unicast: never clashes with a manufacturer address
	return net.HardwareAddr(b[:]).String()
}

// lanOn returns the device's address entry for parent, or for the default-route interface when
// parent is empty. It is nil when the device has not reported a matching interface.
func lanOn(d *store.Device, parent string) *api.NetAddr {
	var first *api.NetAddr
	for i := range d.Host.Network {
		n := &d.Host.Network[i]
		switch {
		case parent != "" && n.Interface == parent:
			return n
		case parent == "" && n.Gateway != "":
			return n
		case first == nil:
			first = n
		}
	}
	if parent == "" {
		return first
	}
	return nil
}

// subnetOf returns the network an interface address belongs to.
func subnetOf(n *api.NetAddr) *net.IPNet {
	ip := net.ParseIP(n.Address)
	if ip == nil || n.Prefix <= 0 {
		return nil
	}
	return &net.IPNet{IP: ip.Mask(net.CIDRMask(n.Prefix, 32)), Mask: net.CIDRMask(n.Prefix, 32)}
}

// cleanDedicated normalises the settings (filling in the MAC) and returns what is wrong with them
// by itself, without looking at other workers or devices.
func cleanDedicated(d *api.DedicatedIP) error {
	d.Parent = strings.TrimSpace(d.Parent)
	d.IP, d.HostIP = strings.TrimSpace(d.IP), strings.TrimSpace(d.HostIP)
	d.Subnet, d.Gateway = strings.TrimSpace(d.Subnet), strings.TrimSpace(d.Gateway)
	d.MAC = strings.ToLower(strings.TrimSpace(d.MAC))
	if d.MAC == "" {
		d.MAC = newMAC()
	}
	if mac, err := net.ParseMAC(d.MAC); err != nil || len(mac) != 6 || mac[0]&1 != 0 {
		return fmt.Errorf("%q is not a usable MAC address (like 02:ab:cd:12:34:56)", d.MAC)
	}
	if d.VLAN < 0 || d.VLAN > 4094 {
		return errors.New("the VLAN ID must be between 1 and 4094 (leave it empty for no VLAN)")
	}
	if d.VLAN == 0 || d.Mode == api.DedicatedDHCP {
		// Without a VLAN the subnet is the device's own; with DHCP it is learned from the lease.
		d.Subnet, d.Gateway = "", ""
	}
	if d.VLAN > 0 && d.Mode == api.DedicatedStatic {
		_, sn, err := net.ParseCIDR(d.Subnet)
		if err != nil || sn.IP.To4() == nil {
			return fmt.Errorf("a static address on a VLAN needs the VLAN's subnet, like 192.168.5.0/24 (got %q)", d.Subnet)
		}
		d.Subnet = sn.String()
		if d.Gateway != "" {
			gw := net.ParseIP(d.Gateway)
			if gw == nil || !sn.Contains(gw) {
				return fmt.Errorf("the gateway %q is not an address inside %s", d.Gateway, sn)
			}
			d.Gateway = gw.String()
		}
	}
	switch d.Mode {
	case api.DedicatedDHCP:
		d.IP = ""
	case api.DedicatedStatic:
		ip := net.ParseIP(d.IP)
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("%q is not an IPv4 address", d.IP)
		}
		d.IP = ip.String()
	default:
		return errors.New("choose DHCP or a static address")
	}
	if d.HostIP != "" {
		ip := net.ParseIP(d.HostIP)
		if ip == nil || ip.To4() == nil {
			return fmt.Errorf("the device access address %q is not an IPv4 address", d.HostIP)
		}
		d.HostIP = ip.String()
		if d.HostIP == d.IP {
			return errors.New("the device access address must differ from the worker's address")
		}
	}
	return nil
}

// takenAddresses maps every address Orchestrator knows to who holds it (devices' own addresses and
// other workers' addresses and device access addresses), and every other worker's MAC to its name.
func (s *Server) takenAddresses(ctx context.Context, exceptProject string) (taken, macs map[string]string) {
	taken, macs = map[string]string{}, map[string]string{}
	devs, _ := s.store.ListDevices(ctx)
	for _, od := range devs {
		for _, n := range od.Host.Network {
			taken[n.Address] = "the device " + od.Name
		}
	}
	projects, _ := s.store.ListProjects(ctx)
	for _, p := range projects {
		o := p.Runtime.Dedicated
		if p.ID == exceptProject || o == nil {
			continue
		}
		if o.IP != "" {
			taken[o.IP] = "the worker " + p.Name
		}
		if o.HostIP != "" {
			taken[o.HostIP] = "the device access address of " + p.Name
		}
		macs[o.MAC] = p.Name
	}
	return taken, macs
}

// dedicatedProblems lists why d can't be used by project id on device dev, from what the server
// already knows: the device's reported network and every other worker's claim on an address.
func (s *Server) dedicatedProblems(ctx context.Context, id string, dev *store.Device, d *api.DedicatedIP) []string {
	var out []string
	if !hasCap(dev, api.CapDedicatedIP) {
		out = append(out, errAgentTooOld(dev, "dedicated addresses").Error())
		return out
	}
	if d.VLAN > 0 && !hasCap(dev, api.CapDedicatedVLAN) {
		return append(out, errAgentTooOld(dev, "VLANs for dedicated addresses").Error())
	}
	var lan *net.IPNet
	var gw string
	if n := lanOn(dev, d.Parent); n == nil {
		out = append(out, fmt.Sprintf("%s has no network interface called %q", dev.Name, d.Parent))
	} else {
		if n.Wireless {
			out = append(out, fmt.Sprintf("%s is a Wi-Fi interface, which can't carry a worker's own address. Use a wired interface", n.Interface))
		}
		lan, gw = subnetOf(n), n.Gateway
	}
	if d.VLAN > 0 {
		// The VLAN has its own network: the one typed in (static) or learned from DHCP (checked by
		// the agent), not the physical port's own.
		lan, gw = nil, d.Gateway
		if _, sn, err := net.ParseCIDR(d.Subnet); err == nil {
			lan = sn
		}
	}
	taken, macs := s.takenAddresses(ctx, id)
	if who, dup := macs[d.MAC]; dup {
		out = append(out, "that MAC address already belongs to "+who)
	}
	for _, c := range []struct{ what, ip string }{{"address", d.IP}, {"device access address", d.HostIP}} {
		if c.ip == "" {
			continue
		}
		if who := taken[c.ip]; who != "" {
			out = append(out, fmt.Sprintf("the %s %s is already used by %s", c.what, c.ip, who))
		}
		ip := net.ParseIP(c.ip)
		if lan != nil && !lan.Contains(ip) {
			out = append(out, fmt.Sprintf("the %s %s is outside %s's network %s", c.what, c.ip, dev.Name, lan))
		}
		if gw != "" && c.ip == gw {
			out = append(out, fmt.Sprintf("the %s %s is the network's gateway", c.what, c.ip))
		}
	}
	return out
}

// validateDedicated checks a project's dedicated address while it is saved.
func (s *Server) validateDedicated(ctx context.Context, id string, in *projectInput) error {
	d := in.Runtime.Dedicated
	if d == nil {
		return nil
	}
	switch {
	case in.Source.Type == store.SourceCompose:
		return errors.New("a dedicated address isn't available for stacks; give the stack's own services addresses in its compose file")
	case in.Source.Type == store.SourceCloudflare:
		return errors.New("a dedicated address isn't available for Cloudflare projects")
	case strings.TrimSpace(in.Runtime.Network) != "":
		return errors.New("a dedicated address replaces the Docker network setting; clear the network or the address")
	}
	if err := cleanDedicated(d); err != nil {
		return err
	}
	dev, err := s.store.GetDevice(ctx, in.DeviceID)
	if err != nil {
		return errors.New("choose a device to deploy to")
	}
	if ps := s.dedicatedProblems(ctx, id, dev, d); len(ps) > 0 {
		return errors.New(strings.ToUpper(ps[0][:1]) + ps[0][1:])
	}
	return nil
}

// dedicatedCheckRequest asks whether an address would work on a device before it is saved.
type dedicatedCheckRequest struct {
	api.DedicatedIP
	ProjectID string `json:"project_id,omitempty"`
	Service   string `json:"service,omitempty"`
}

type dedicatedCheckView struct {
	api.NetworkCheck
	// MAC is the worker's address, generated when the request had none.
	MAC string `json:"mac"`
}

// handleCheckDedicated validates a dedicated address on a device: what the server knows, then the
// device itself (is the address free on the LAN, what would DHCP offer).
func (s *Server) handleCheckDedicated(w http.ResponseWriter, r *http.Request) {
	dev, ok := s.lookupDevice(w, r)
	if !ok {
		return
	}
	var req dedicatedCheckRequest
	if !decode(w, r, &req) {
		return
	}
	if dev.Offline {
		writeError(w, http.StatusConflict, dev.Name+" is offline, so its network can't be checked")
		return
	}
	d := req.DedicatedIP
	if err := cleanDedicated(&d); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	view := dedicatedCheckView{MAC: d.MAC}
	view.Problems = s.dedicatedProblems(r.Context(), req.ProjectID, dev, &d)
	if len(view.Problems) > 0 {
		writeJSON(w, http.StatusOK, view)
		return
	}
	service := req.Service
	if req.ProjectID != "" {
		if p, err := s.store.GetProject(r.Context(), req.ProjectID); err == nil {
			service = serviceOf(p)
		}
	}
	taken, _ := s.takenAddresses(r.Context(), req.ProjectID)
	avoid := make([]string, 0, len(taken))
	for ip := range taken {
		avoid = append(avoid, ip)
	}
	cmd, err := s.store.CreateCommand(r.Context(), dev.ID, api.ActionNetworkCheck, "", map[string]string{
		"mode": d.Mode, "ip": d.IP, "mac": d.MAC, "parent": d.Parent, "host_ip": d.HostIP, "service": service,
		"avoid": strings.Join(avoid, ","), "vlan": strconv.Itoa(d.VLAN), "subnet": d.Subnet, "gateway": d.Gateway,
	})
	if err != nil {
		s.internal(w, err)
		return
	}
	done, err := s.waitCommand(r.Context(), cmd.ID, 40*time.Second)
	switch {
	case err != nil:
		writeError(w, http.StatusGatewayTimeout, err.Error())
		return
	case done.Status == api.CommandFailed:
		writeError(w, http.StatusBadGateway, done.Error)
		return
	}
	if err := json.Unmarshal([]byte(done.Output), &view.NetworkCheck); err != nil {
		writeError(w, http.StatusBadGateway, "the agent sent an unreadable answer")
		return
	}
	if d.VLAN > 0 && !hasCap(dev, api.CapDedicatedDHCPRaw) {
		// An agent from before the DHCP fix (#107) gives a shorter error and can miss replies on a VLAN.
		view.Notes = append(view.Notes, fmt.Sprintf("The agent on %s (version %s) is older than the VLAN DHCP fix: it can miss DHCP replies on a VLAN and gives a shorter error. Update it to the latest Beta from the Updates page, then check again.", dev.Name, dev.Host.AgentVersion))
		if len(view.Problems) > 0 {
			view.Problems = append(view.Problems, "This result comes from an outdated agent. Update the agent first.")
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// dedicatedAddress is the worker's address as the device last reported it (the leased one in DHCP mode).
func dedicatedAddress(p *store.Project, d *store.Device) (ip, mac string) {
	if p.Runtime.Dedicated == nil || d == nil {
		return "", ""
	}
	if c := findByName(d, serviceOf(p)); c != nil {
		for _, n := range c.Networks {
			if strings.HasPrefix(n.Name, "orch-lan-") {
				return n.IP, n.MAC
			}
		}
	}
	return p.Runtime.Dedicated.IP, p.Runtime.Dedicated.MAC
}

// dedicatedMoveProblem says why p can't move to dst with its dedicated address.
func dedicatedMoveProblem(p *store.Project, dst *store.Device) error {
	d := p.Runtime.Dedicated
	if d == nil {
		return nil
	}
	if !hasCap(dst, api.CapDedicatedIP) {
		return errAgentTooOld(dst, "dedicated addresses")
	}
	if d.VLAN > 0 && !hasCap(dst, api.CapDedicatedVLAN) {
		return errAgentTooOld(dst, "VLANs for dedicated addresses")
	}
	n := lanOn(dst, d.Parent)
	switch {
	case n == nil:
		return fmt.Errorf("%s has no network interface called %q. Clear the interface in %s's settings, or choose another device", dst.Name, d.Parent, p.Name)
	case n.Wireless:
		return fmt.Errorf("%s reaches the network over Wi-Fi, which can't carry a dedicated address", dst.Name)
	}
	// A VLAN has its own network, so the device's own subnet doesn't matter; the destination's
	// switch port must carry the VLAN, which can't be checked from here.
	if lan := subnetOf(n); lan != nil && d.VLAN == 0 {
		for _, ip := range []string{d.IP, d.HostIP} {
			if ip != "" && !lan.Contains(net.ParseIP(ip)) {
				return fmt.Errorf("%s is outside %s's network %s. Give %s a new address, or use DHCP, before moving it", ip, dst.Name, lan, p.Name)
			}
		}
	}
	return nil
}

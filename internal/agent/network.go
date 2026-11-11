package agent

import (
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"slices"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// serverVia says where the control-plane URL in use came from.
func (a *Agent) serverVia() string {
	a.srvMu.Lock()
	defer a.srvMu.Unlock()
	switch cur := a.cfg.Server; {
	case slices.Contains(a.configured, cur) || len(a.configured) == 0:
		return "configured"
	case cur == a.state.Found:
		return "discovered"
	default:
		return "announced"
	}
}

// network lists the device's addresses with how each was assigned. Asking `ip` costs a
// process, so the answer is reused for a minute unless the addresses themselves changed.
func (a *Agent) network() []api.NetAddr {
	var out []api.NetAddr
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || virtualIface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, ad := range addrs {
			if ipn, ok := ad.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, api.NetAddr{Interface: ifc.Name, Address: ipn.IP.String()})
			}
		}
	}
	a.netMu.Lock()
	defer a.netMu.Unlock()
	same := len(out) == len(a.netCache)
	for i := 0; same && i < len(out); i++ {
		same = out[i].Address == a.netCache[i].Address && out[i].Interface == a.netCache[i].Interface
	}
	if !same || time.Since(a.netAt) > time.Minute {
		assigned := assignments()
		for i := range out {
			out[i].Assigned = assigned[out[i].Address]
		}
		a.netCache, a.netAt = out, time.Now()
	}
	return a.netCache
}

// assignments maps each IPv4 address to "dhcp" or "static", using iproute2's view of the
// kernel's address flags. It returns nothing where `ip` is missing (some containers).
func assignments() map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "ip", "-j", "-4", "addr", "show").Output()
	if err != nil {
		return nil
	}
	return parseAssignments(b)
}

func parseAssignments(b []byte) map[string]string {
	var links []struct {
		AddrInfo []struct {
			Local   string `json:"local"`
			Dynamic bool   `json:"dynamic"`
		} `json:"addr_info"`
	}
	if json.Unmarshal(b, &links) != nil {
		return nil
	}
	out := map[string]string{}
	for _, l := range links {
		for _, ai := range l.AddrInfo {
			if ai.Local == "" {
				continue
			}
			// The kernel marks an address with a lease lifetime as dynamic; that is what a
			// DHCP client sets. A router reservation still shows as dhcp: the lease is real,
			// the router just always offers the same address.
			if ai.Dynamic {
				out[ai.Local] = "dhcp"
			} else {
				out[ai.Local] = "static"
			}
		}
	}
	return out
}

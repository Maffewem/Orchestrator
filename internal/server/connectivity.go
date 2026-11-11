package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// netNoticeFor is how long a device card keeps saying that its network changed.
const netNoticeFor = 6 * time.Hour

type netNotice struct {
	at  time.Time
	msg string
}

// netNotices remembers the latest network change per device for the dashboard. It is kept in
// memory: the activity log has the durable record.
type netNotices struct {
	mu sync.Mutex
	m  map[string]netNotice
}

func (n *netNotices) set(id, msg string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.m == nil {
		n.m = map[string]netNotice{}
	}
	n.m[id] = netNotice{time.Now(), msg}
}

func (n *netNotices) get(id string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if v, ok := n.m[id]; ok && time.Since(v.at) < netNoticeFor {
		return v.msg
	}
	return ""
}

// noteNetworkChanges compares a heartbeat with the device's previous state and records what
// changed about how it is connected: its own addresses, and the address it reaches us at.
func (s *Server) noteNetworkChanges(ctx context.Context, d *store.Device, now api.HostInfo) {
	if old, cur := d.Host.Addresses, now.Addresses; len(old) > 0 && len(cur) > 0 {
		var gone, added []string
		for _, a := range old {
			if !slices.Contains(cur, a) {
				gone = append(gone, a)
			}
		}
		for _, a := range cur {
			if !slices.Contains(old, a) {
				added = append(added, a)
			}
		}
		var msg string
		switch {
		case len(gone) > 0 && len(added) > 0:
			msg = fmt.Sprintf("IP address changed from %s to %s", strings.Join(gone, ", "), strings.Join(added, ", "))
		case len(gone) > 0:
			msg = fmt.Sprintf("IP address %s went away (still has %s)", strings.Join(gone, ", "), strings.Join(cur, ", "))
		case len(added) > 0:
			msg = fmt.Sprintf("Gained IP address %s", strings.Join(added, ", "))
		}
		if msg != "" {
			s.event(ctx, d.ID, "warning", "device.address", d.Name+": "+msg)
			s.netNotes.set(d.ID, msg)
		}
	}
	if d.Host.Server != "" && now.Server != "" && now.Server != d.Host.Server {
		how := "its settings changed"
		switch now.ServerVia {
		case "discovered":
			how = "it found the server by searching its network"
		case "announced":
			how = "it fell back to an address from Settings"
		}
		msg := fmt.Sprintf("Now reaches the server at %s instead of %s (%s)", now.Server, d.Host.Server, how)
		s.event(ctx, d.ID, "warning", "device.server_url", d.Name+": "+msg)
		s.netNotes.set(d.ID, msg)
		// The device proxy asks that address whether a visitor may enter, so it has to follow.
		s.syncProxy(ctx, d.ID)
	}
}

// ConnAddress is one address agents use, or could use, to reach this server.
type ConnAddress struct {
	URL    string `json:"url"`
	Listed bool   `json:"listed"` // in Settings, so announced to every agent
	// Kind is ip, name or loopback.
	Kind string `json:"kind"`
	// Assigned is dhcp or static when the address is an IP of the device hosting the server.
	Assigned string   `json:"assigned,omitempty"`
	Devices  []string `json:"devices"` // names of the devices using it right now
}

// ConnDevice is how one device is connected.
type ConnDevice struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Server string `json:"server"`
	Via    string `json:"via"`
	// Recovers reports whether the agent can find the server again by itself.
	Recovers bool          `json:"recovers"`
	Network  []api.NetAddr `json:"network"`
}

type ConnView struct {
	// Host is the name of the device this server runs on, when one of them does.
	Host        string        `json:"host"`
	Addresses   []ConnAddress `json:"addresses"`
	Suggestions []string      `json:"suggestions"`
	Devices     []ConnDevice  `json:"devices"`
	Warnings    []string      `json:"warnings"`
	Changes     []store.Event `json:"changes"`
}

func urlHost(raw string) (host, port string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	port = u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return u.Hostname(), port
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func (s *Server) connectivity(ctx context.Context) (ConnView, error) {
	v := ConnView{Addresses: []ConnAddress{}, Suggestions: []string{}, Devices: []ConnDevice{}, Warnings: []string{}, Changes: []store.Event{}}
	devs, err := s.store.ListDevices(ctx)
	if err != nil {
		return v, err
	}
	// The device this server runs on: its agent talks to loopback, or other agents talk to
	// one of its addresses.
	var host *store.Device
	for _, d := range devs {
		if h, _ := urlHost(d.Host.Server); isLoopback(h) {
			host = d
		}
	}
	for _, d := range devs {
		for _, o := range devs {
			if h, _ := urlHost(o.Host.Server); host == nil && h != "" && slices.Contains(d.Host.Addresses, h) {
				host = d
			}
		}
	}
	assigned := map[string]string{}
	if host != nil {
		v.Host = host.Name
		for _, n := range host.Host.Network {
			assigned[n.Address] = n.Assigned
		}
	}

	byURL := map[string]*ConnAddress{}
	add := func(raw string) *ConnAddress {
		if a, ok := byURL[raw]; ok {
			return a
		}
		h, _ := urlHost(raw)
		a := &ConnAddress{URL: raw, Kind: "name", Devices: []string{}}
		switch {
		case isLoopback(h):
			a.Kind = "loopback"
		case net.ParseIP(h) != nil:
			a.Kind, a.Assigned = "ip", assigned[h]
		}
		byURL[raw] = a
		return a
	}
	for _, u := range s.agentURLs(ctx) {
		add(u).Listed = true
	}
	port := ""
	var old, found []string
	for _, d := range devs {
		dv := s.view(d)
		cd := ConnDevice{ID: d.ID, Name: d.Name, Status: dv.Status, Server: d.Host.Server, Via: d.Host.ServerVia, Recovers: hasCap(d, api.CapDiscover), Network: d.Host.Network}
		if cd.Network == nil {
			cd.Network = []api.NetAddr{}
			for _, a := range d.Host.Addresses {
				cd.Network = append(cd.Network, api.NetAddr{Address: a})
			}
		}
		v.Devices = append(v.Devices, cd)
		if d.Host.Server == "" {
			continue
		}
		a := add(d.Host.Server)
		a.Devices = append(a.Devices, d.Name)
		if _, p := urlHost(d.Host.Server); port == "" && strings.HasPrefix(d.Host.Server, "http://") {
			port = p
		}
		if dv.Status == "online" && !cd.Recovers {
			old = append(old, d.Name)
		}
		if d.Host.ServerVia == "discovered" && !a.Listed {
			found = append(found, d.Name)
		}
	}
	for _, a := range byURL {
		v.Addresses = append(v.Addresses, *a)
	}
	sort.Slice(v.Addresses, func(i, j int) bool {
		a, b := v.Addresses[i], v.Addresses[j]
		if len(a.Devices) != len(b.Devices) {
			return len(a.Devices) > len(b.Devices)
		}
		return a.URL < b.URL
	})

	// Addresses of the host device that are not in use or listed yet.
	if host != nil && port != "" {
		for _, n := range host.Host.Addresses {
			if u := "http://" + net.JoinHostPort(n, port); byURL[u] == nil {
				v.Suggestions = append(v.Suggestions, u)
			}
		}
	}

	for _, a := range v.Addresses {
		if a.Kind != "ip" || a.Assigned != "dhcp" || len(a.Devices) == 0 {
			continue
		}
		h, _ := urlHost(a.URL)
		v.Warnings = append(v.Warnings, fmt.Sprintf(
			"%s %s this server at %s, an address %s gets from the router by DHCP, so it can change. Reserve it for %s on the router or give the server a fixed address.",
			plural(len(a.Devices), "device"), map[bool]string{true: "reaches", false: "reach"}[len(a.Devices) == 1], h, v.Host, v.Host))
	}
	if len(old) > 0 {
		v.Warnings = append(v.Warnings, fmt.Sprintf(
			"%s %s an agent that cannot find the server again by itself. Update it under Updates.",
			strings.Join(old, ", "), map[bool]string{true: "runs", false: "run"}[len(old) == 1]))
	}
	if len(found) > 0 {
		v.Warnings = append(v.Warnings, fmt.Sprintf(
			"%s had to search the network to find this server. Add the address it found to the list so every device learns it.",
			strings.Join(found, ", ")))
	}

	events, _ := s.store.ListEvents(ctx, "", 500)
	for _, e := range events {
		if (e.Kind == "device.address" || e.Kind == "device.server_url") && len(v.Changes) < 8 {
			v.Changes = append(v.Changes, e)
		}
	}
	return v, nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func (s *Server) handleConnectivity(w http.ResponseWriter, r *http.Request) {
	v, err := s.connectivity(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

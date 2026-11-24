package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

func dedicatedHost(caps ...string) api.HostInfo {
	return api.HostInfo{Hostname: "pi", Capabilities: caps, Network: []api.NetAddr{
		{Interface: "eth0", Address: "192.168.0.184", Prefix: 24, Gateway: "192.168.0.1", Assigned: "dhcp"},
	}}
}

func TestDedicatedAddressSettings(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi")
	beat := func(token string, host api.HostInfo, cs ...api.Container) {
		h.do("POST", "/api/agent/heartbeat", token, api.HeartbeatRequest{Host: host, Containers: cs}, &api.HeartbeatResponse{})
	}
	img := map[string]any{"type": "image", "image": "caddy"}
	create := func(name string, ded map[string]any, extra map[string]any) (int, store.Project) {
		rt := map[string]any{"ports": []string{"80:80", "443:443"}, "dedicated": ded}
		for k, v := range extra {
			rt[k] = v
		}
		var p store.Project
		code := h.do("POST", "/api/projects", adminToken, map[string]any{"name": name, "device_id": reg.DeviceID, "source": img, "runtime": rt}, &p)
		return code, p
	}

	// An agent from before the feature can't take it.
	beat(reg.AgentToken, dedicatedHost())
	if code, _ := create("old", map[string]any{"mode": "dhcp"}, nil); code != http.StatusBadRequest {
		t.Fatalf("old agent: %d", code)
	}
	beat(reg.AgentToken, dedicatedHost(api.CapDedicatedIP))

	code, p := create("web", map[string]any{"mode": "dhcp"}, nil)
	if code != http.StatusCreated {
		t.Fatalf("dhcp: %d", code)
	}
	d := p.Runtime.Dedicated
	if d == nil || d.MAC == "" || !strings.HasPrefix(d.MAC, "02:") {
		t.Fatalf("a locally administered MAC should be generated: %+v", d)
	}
	// Editing keeps the same MAC so a router reservation keeps working.
	var again store.Project
	if code := h.do("PATCH", "/api/projects/"+p.ID, adminToken, map[string]any{"runtime": map[string]any{"ports": []string{"80:80"}, "dedicated": map[string]any{"mode": "dhcp", "mac": d.MAC}}}, &again); code != http.StatusOK || again.Runtime.Dedicated.MAC != d.MAC {
		t.Fatalf("edit: %d %+v", code, again.Runtime.Dedicated)
	}

	bad := map[string]map[string]any{
		"outside the subnet": {"mode": "static", "ip": "10.1.1.5"},
		"gateway":            {"mode": "static", "ip": "192.168.0.1"},
		"the device itself":  {"mode": "static", "ip": "192.168.0.184"},
		"not an address":     {"mode": "static", "ip": "nope"},
		"unknown interface":  {"mode": "dhcp", "parent": "eth9"},
		"bad mac":            {"mode": "dhcp", "mac": "01:00:5e:00:00:01"},
		"no mode":            {},
		"duplicate mac":      {"mode": "dhcp", "mac": d.MAC},
	}
	for name, ded := range bad {
		if code, _ := create("bad-"+strings.ReplaceAll(name, " ", "-"), ded, nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	if code, _ := create("with-net", map[string]any{"mode": "dhcp"}, map[string]any{"network": "mynet"}); code != http.StatusBadRequest {
		t.Errorf("a docker network can't be combined: %d", code)
	}

	// Static addresses are unique across workers.
	if code, _ := create("s1", map[string]any{"mode": "static", "ip": "192.168.0.201"}, nil); code != http.StatusCreated {
		t.Fatalf("static: %d", code)
	}
	if code, _ := create("s2", map[string]any{"mode": "static", "ip": "192.168.0.201"}, nil); code != http.StatusBadRequest {
		t.Errorf("duplicate static address: %d", code)
	}

	// Wi-Fi can't carry it.
	host := dedicatedHost(api.CapDedicatedIP)
	host.Network[0].Wireless = true
	beat(reg.AgentToken, host)
	if code, _ := create("wifi", map[string]any{"mode": "dhcp"}, nil); code != http.StatusBadRequest {
		t.Errorf("wifi: %d", code)
	}

	// Operators can't give a worker its own place on the LAN.
	beat(reg.AgentToken, dedicatedHost(api.CapDedicatedIP))
	op := h.makeUser("olive", "operator")
	var denied map[string]any
	if code := h.do("POST", "/api/projects", op, map[string]any{"name": "op", "device_id": reg.DeviceID, "source": img, "runtime": map[string]any{"dedicated": map[string]any{"mode": "dhcp"}}}, &denied); code != http.StatusForbidden {
		t.Errorf("operator: %d", code)
	}
}

func TestDedicatedWorkersSkipDeviceRouting(t *testing.T) {
	p := &store.Project{Name: "web", Runtime: store.Runtime{Ports: []string{"80:80"}}}
	if upstreamPort(p) != "80" {
		t.Fatal("a normal worker routes through its host port")
	}
	p.Runtime.Dedicated = &api.DedicatedIP{Mode: api.DedicatedDHCP, MAC: "02:00:00:00:00:01"}
	if upstreamPort(p) != "" {
		t.Fatal("a dedicated worker has no host port to route to")
	}
	d := &store.Device{Containers: []api.Container{{Name: "other", State: "running", Ports: []api.Port{{PublicPort: 80, Type: "tcp"}}}}}
	if got := portConflictsOn(p, d); len(got) != 0 {
		t.Fatalf("a dedicated worker can't clash with the device's ports: %v", got)
	}
	p.Runtime.Dedicated = nil
	if got := portConflictsOn(p, d); len(got) != 1 {
		t.Fatalf("a normal worker does clash: %v", got)
	}
}

func TestDedicatedMoveProblems(t *testing.T) {
	p := &store.Project{Name: "web", Runtime: store.Runtime{Dedicated: &api.DedicatedIP{Mode: api.DedicatedStatic, IP: "192.168.0.201", MAC: "02:00:00:00:00:01"}}}
	dst := &store.Device{Name: "cm4", Host: dedicatedHost(api.CapDedicatedIP)}
	if err := dedicatedMoveProblem(p, dst); err != nil {
		t.Fatal(err)
	}
	dst.Host.Network[0].Address, dst.Host.Network[0].Gateway = "10.0.0.5", "10.0.0.1"
	if err := dedicatedMoveProblem(p, dst); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("a static address from another subnet: %v", err)
	}
	p.Runtime.Dedicated.Mode, p.Runtime.Dedicated.IP = api.DedicatedDHCP, ""
	if err := dedicatedMoveProblem(p, dst); err != nil {
		t.Fatalf("DHCP moves anywhere: %v", err)
	}
	dst.Host.Capabilities = nil
	if err := dedicatedMoveProblem(p, dst); err == nil {
		t.Fatal("an old agent can't host it")
	}
}

func TestDedicatedWorkerUpstream(t *testing.T) {
	p := &store.Project{ID: "p1", Name: "web", DeviceID: "d1", Runtime: store.Runtime{
		Ports: []string{"8080:80", "443:443"}, WebPort: "443",
		Dedicated: &api.DedicatedIP{Mode: api.DedicatedDHCP, MAC: "02:00:00:00:00:01"},
	}}
	dev := &store.Device{ID: "d1", Name: "pi", Containers: []api.Container{{Name: "web", State: "running",
		Networks: []api.ContainerNet{{Name: "orch-lan-eth0", IP: "192.168.0.77", MAC: "02:00:00:00:00:01"}}}}}
	if got := upstreamAddr(p, dev); got != "" {
		t.Fatalf("without the device access address nothing on the device can reach it: %q", got)
	}
	p.Runtime.Dedicated.HostIP = "192.168.0.250"
	if got := upstreamAddr(p, dev); got != "192.168.0.77:443" {
		t.Fatalf("upstream = %q", got)
	}
	p.Runtime.WebPort = ""
	if got := upstreamAddr(p, dev); got != "192.168.0.77:80" {
		t.Fatalf("first port = %q", got)
	}

	// Caddy and the tunnel both send traffic to that address.
	file, _ := buildCaddy("example.com", "", []*store.Project{p}, nil, map[string]urlPrefs{"p1": {Production: true}}, nil, nil, func(p *store.Project) string { return upstreamAddr(p, dev) })
	if !strings.Contains(file, "reverse_proxy 192.168.0.77:80") {
		t.Fatalf("caddyfile: %s", file)
	}
	s := &Server{}
	svc, err := s.resolveRoute([]*store.Project{p}, dev, false, "", tunnelRoute{Hostname: "w.example.com", Service: "project:web"})
	if err != nil || svc != "http://192.168.0.77:80" {
		t.Fatalf("tunnel service %q %v", svc, err)
	}
	p.Runtime.Dedicated.HostIP = ""
	if _, err := s.resolveRoute([]*store.Project{p}, dev, false, "", tunnelRoute{Hostname: "w.example.com", Service: "project:web"}); err == nil || !strings.Contains(err.Error(), "address for the device itself") {
		t.Fatalf("a worker the device can't reach must say why: %v", err)
	}
}

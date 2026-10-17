package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestDomains(t *testing.T) {
	lookup := lookupHost
	t.Cleanup(func() { lookupHost = lookup })
	lookupHost = func(_ context.Context, host string) ([]string, error) {
		switch host {
		case "app.example.com":
			return []string{"192.168.1.20"}, nil
		case "other.example.com":
			return []string{"104.1.2.3"}, nil
		}
		return nil, context.DeadlineExceeded
	}

	h := newHarness(t)
	reg := h.register("pi-4")
	caps := []string{api.CapProxy}
	hb := func(c []string) {
		h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
			Host: api.HostInfo{Addresses: []string{"192.168.1.20", "fe80::1"}, Capabilities: c}}, &api.HeartbeatResponse{})
	}
	hb(caps)

	var p ProjectView
	h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "blog", "device_id": reg.DeviceID,
		"source":  map[string]any{"type": "image", "image": "nginx"},
		"runtime": map[string]any{"ports": []string{"8081:80"}},
	}, &p)

	if code := h.do("PUT", "/api/settings/domain", adminToken, map[string]string{"base_domain": "not a domain"}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad base domain: %d", code)
	}
	if code := h.do("PUT", "/api/settings/domain", adminToken, map[string]string{"base_domain": "Example.com"}, nil); code != http.StatusOK {
		t.Fatalf("base domain: %d", code)
	}

	var v domainsView
	h.do("GET", "/api/projects/"+p.ID+"/domains", adminToken, nil, &v)
	if v.Production == nil || v.Production.Hostname != "blog.example.com" || !v.Production.Enabled || v.Production.Status != "pending" {
		t.Fatalf("production: %+v", v.Production)
	}
	if v.Preview == nil || v.Preview.Hostname != "*-blog.example.com" || v.Preview.Enabled {
		t.Fatalf("preview: %+v", v.Preview)
	}
	if len(v.Production.Records) != 1 || v.Production.Records[0].Value != "192.168.1.20" {
		t.Fatalf("records should list the IPv4 address only: %+v", v.Production.Records)
	}

	for _, bad := range []string{"localhost", "a b.com", "http://", ""} {
		if code := h.do("POST", "/api/projects/"+p.ID+"/domains", adminToken, map[string]string{"hostname": bad}, nil); code != http.StatusBadRequest {
			t.Fatalf("hostname %q: %d", bad, code)
		}
	}
	if code := h.do("POST", "/api/projects/"+p.ID+"/domains", adminToken, map[string]string{"hostname": "https://App.Example.com/", "type": "domain"}, &v); code != http.StatusCreated {
		t.Fatalf("add: %d", code)
	}
	if code := h.do("POST", "/api/projects/"+p.ID+"/domains", adminToken, map[string]string{"hostname": "app.example.com"}, nil); code != http.StatusConflict {
		t.Fatalf("duplicate: %d", code)
	}
	if code := h.do("POST", "/api/projects/"+p.ID+"/domains", adminToken, map[string]string{"hostname": "other.example.com", "type": "route", "path": "api"}, nil); code != http.StatusBadRequest {
		t.Fatalf("route without slash: %d", code)
	}
	h.do("POST", "/api/projects/"+p.ID+"/domains", adminToken, map[string]string{"hostname": "other.example.com", "type": "route", "path": "/api/*"}, nil)

	h.do("GET", "/api/projects/"+p.ID+"/domains", adminToken, nil, &v)
	if len(v.Domains) != 2 || v.Domains[0].Status != "active" || v.Domains[1].Status != "mismatch" || v.Domains[1].Zone != "example.com" || v.Domains[1].Type != "route" {
		t.Fatalf("domains: %+v", v.Domains)
	}

	// Every change queued a proxy.sync; the last one must carry all sites.
	var resp api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: caps}}, &resp)
	var file string
	for _, c := range resp.Commands {
		if c.Action == api.ActionProxySync {
			file = c.Args["caddyfile"]
		}
	}
	for _, want := range []string{"blog.example.com {", "app.example.com {", "other.example.com {", "handle /api/* {", "reverse_proxy 127.0.0.1:8081"} {
		if !strings.Contains(file, want) {
			t.Fatalf("caddyfile missing %q:\n%s", want, file)
		}
	}

	h.do("PUT", "/api/projects/"+p.ID+"/domains/urls", adminToken, map[string]bool{"production": false, "preview": true}, &v)
	if v.Production.Enabled || !v.Preview.Enabled {
		t.Fatalf("toggles: %+v %+v", v.Production, v.Preview)
	}
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: caps}}, &resp)
	file = ""
	for _, c := range resp.Commands {
		file = c.Args["caddyfile"]
	}
	if !strings.Contains(file, "http://*-blog.example.com {") || strings.Contains(file, "\nblog.example.com {") {
		t.Fatalf("toggled caddyfile:\n%s", file)
	}

	if code := h.do("DELETE", "/api/projects/"+p.ID+"/domains/"+v.Domains[0].ID, adminToken, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code := h.do("DELETE", "/api/projects/"+p.ID+"/domains/"+v.Domains[0].ID, adminToken, nil, nil); code != http.StatusNotFound {
		t.Fatalf("delete twice: %d", code)
	}

	// An agent without the capability is reported, not silently ignored.
	hb(nil)
	h.do("GET", "/api/projects/"+p.ID+"/domains", adminToken, nil, &v)
	if v.Proxy != "agent_too_old" {
		t.Fatalf("proxy = %q", v.Proxy)
	}
}

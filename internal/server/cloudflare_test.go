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
)

func TestCloudflareSSLAndDNS(t *testing.T) {
	var mu sync.Mutex
	records := []cfRecord{}
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "Invalid API Token"}}})
			return
		}
		ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"success": true, "result": v}) }
		switch {
		case r.URL.Path == "/user/tokens/verify":
			ok(map[string]string{"status": "active"})
		case r.URL.Path == "/zones":
			if r.URL.Query().Get("name") == "example.com" || r.URL.Query().Get("name") == "" {
				ok([]cfZone{{ID: "z1", Name: "example.com"}})
			} else {
				ok([]cfZone{})
			}
		case r.URL.Path == "/zones/z1/dns_records" && r.Method == http.MethodGet:
			ok(records)
		case r.URL.Path == "/zones/z1/dns_records" && r.Method == http.MethodPost:
			var rec cfRecord
			json.NewDecoder(r.Body).Decode(&rec)
			rec.ID = "r1"
			records = append(records, rec)
			ok(rec)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	defer cf.Close()

	h := newHarness(t)
	h.srv.cloudflareAPI = cf.URL
	reg := h.register("pi-4")
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
		Host: api.HostInfo{Addresses: []string{"192.168.1.20"}, Capabilities: []string{api.CapProxy, api.CapProxyDNS}}}, &api.HeartbeatResponse{})
	var p ProjectView
	h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "blog", "device_id": reg.DeviceID,
		"source":  map[string]any{"type": "image", "image": "nginx"},
		"runtime": map[string]any{"ports": []string{"8081:80"}},
	}, &p)

	if code := h.do("PUT", "/api/settings/ssl", adminToken, map[string]string{"mode": "dns"}, nil); code != http.StatusConflict {
		t.Fatalf("dns mode without Cloudflare: %d", code)
	}
	if code := h.do("PUT", "/api/settings/cloudflare", adminToken, map[string]string{"token": "bad"}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad token: %d", code)
	}
	h.do("PUT", "/api/settings/domain", adminToken, map[string]string{"base_domain": "example.com"}, nil)
	if code := h.do("PUT", "/api/settings/cloudflare", adminToken, map[string]string{"token": "good"}, nil); code != http.StatusOK {
		t.Fatalf("good token: %d", code)
	}
	if code := h.do("PUT", "/api/settings/ssl", adminToken, map[string]string{"mode": "dns"}, nil); code != http.StatusOK {
		t.Fatalf("dns mode: %d", code)
	}

	mu.Lock()
	if len(records) != 1 || records[0].Name != "blog.example.com" || records[0].Content != "192.168.1.20" || records[0].Proxied {
		t.Fatalf("records: %+v", records)
	}
	mu.Unlock()

	// The device's newest proxy command carries the DNS challenge and the token.
	var resp api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Addresses: []string{"192.168.1.20"}, Capabilities: []string{api.CapProxy, api.CapProxyDNS}}}, &resp)
	var last map[string]string
	for _, c := range resp.Commands {
		if c.Action == api.ActionProxySync {
			last = c.Args
		}
	}
	if last["cloudflare_token"] != "good" || !strings.Contains(last["caddyfile"], "acme_dns cloudflare {env.CF_API_TOKEN}") {
		t.Fatalf("proxy args: %+v", last)
	}
	if strings.Contains(last["caddyfile"], "good") {
		t.Fatal("token leaked into the Caddyfile")
	}

	var st struct {
		Cloudflare map[string]any `json:"cloudflare"`
		SSLMode    string         `json:"ssl_mode"`
	}
	var m map[string]any
	h.do("GET", "/api/settings", adminToken, nil, &m)
	b, _ := json.Marshal(m)
	raw := string(b)
	if strings.Contains(raw, "good") {
		t.Fatal("settings response leaks the token")
	}
	json.Unmarshal([]byte(raw), &st)
	if st.Cloudflare["connected"] != true || st.SSLMode != "dns" {
		t.Fatalf("settings: %s", raw)
	}

	// Disconnecting falls back to the default challenge.
	h.do("PUT", "/api/settings/cloudflare", adminToken, map[string]string{"token": ""}, nil)
	if h.srv.sslMode(context.Background()) != sslHTTP {
		t.Fatal("ssl mode should fall back to http")
	}
}

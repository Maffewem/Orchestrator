package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func TestIdentifyAndAgentURLs(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi-1")
	key, err := base64.StdEncoding.DecodeString(reg.ServerKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		t.Fatalf("register returned server key %q", reg.ServerKey)
	}

	// The server proves its identity by signing the caller's nonce.
	nonce := "00112233445566778899aabbccddeeff"
	var id api.IdentifyResponse
	if code := h.do("GET", "/api/agent/identify?nonce="+nonce, "", nil, &id); code != http.StatusOK {
		t.Fatalf("identify: %d", code)
	}
	sig, _ := base64.StdEncoding.DecodeString(id.Signature)
	if id.ServerKey != reg.ServerKey || !ed25519.Verify(key, api.IdentifyMessage(nonce), sig) {
		t.Fatal("identify signature does not verify against the key from registration")
	}
	if code := h.do("GET", "/api/agent/identify?nonce=short", "", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("short nonce: %d", code)
	}

	// Addresses set in Settings reach agents on their next heartbeat.
	for _, bad := range []string{"192.168.0.10:8080", "ftp://x", "http://x/path", "http://user:pw@x"} {
		if code := h.do("PUT", "/api/settings/agent-urls", adminToken, map[string]any{"urls": []string{bad}}, nil); code != http.StatusBadRequest {
			t.Fatalf("%s accepted: %d", bad, code)
		}
	}
	urls := []string{"http://192.168.0.184:8080/", " http://claude.local:8080", "http://192.168.0.184:8080"}
	if code := h.do("PUT", "/api/settings/agent-urls", adminToken, map[string]any{"urls": urls}, nil); code != http.StatusOK {
		t.Fatalf("set agent urls: %d", code)
	}
	var hb api.HeartbeatResponse
	if code := h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{}, &hb); code != http.StatusOK {
		t.Fatalf("heartbeat: %d", code)
	}
	if hb.ServerKey != reg.ServerKey || len(hb.ServerURLs) != 2 || hb.ServerURLs[0] != "http://192.168.0.184:8080" || hb.ServerURLs[1] != "http://claude.local:8080" {
		t.Fatalf("heartbeat = key %q urls %v", hb.ServerKey, hb.ServerURLs)
	}
}

func TestConnectivityWarnsAboutNetworkChanges(t *testing.T) {
	h := newHarness(t)
	pi := h.register("claude")
	cm4 := h.register("cm4")
	beat := func(reg api.RegisterResponse, host api.HostInfo) {
		t.Helper()
		if code := h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: host}, nil); code != http.StatusOK {
			t.Fatalf("heartbeat: %d", code)
		}
	}
	caps := []string{api.CapDiscover}
	server := api.HostInfo{Hostname: "claude", Capabilities: caps, Server: "http://127.0.0.1:8080", ServerVia: "configured",
		Addresses: []string{"192.168.2.185"}, Network: []api.NetAddr{{Interface: "wlan0", Address: "192.168.2.185", Assigned: "dhcp"}}}
	beat(pi, server)
	beat(cm4, api.HostInfo{Hostname: "cm4", Server: "http://192.168.2.185:8080", Addresses: []string{"192.168.2.50"}})

	var v ConnView
	h.do("GET", "/api/connectivity", adminToken, nil, &v)
	if v.Host != "claude" || len(v.Warnings) != 2 {
		t.Fatalf("host %q warnings %q", v.Host, v.Warnings)
	}
	if !strings.Contains(v.Warnings[0], "192.168.2.185") || !strings.Contains(v.Warnings[0], "DHCP") || !strings.Contains(v.Warnings[1], "cm4 runs an agent") {
		t.Fatalf("warnings %q", v.Warnings)
	}

	// The Pi moves to ethernet; cm4 (now updated) finds it by discovery.
	server.Addresses = []string{"192.168.0.184"}
	server.Network = []api.NetAddr{{Interface: "eth0", Address: "192.168.0.184", Assigned: "dhcp"}}
	beat(pi, server)
	beat(cm4, api.HostInfo{Hostname: "cm4", Capabilities: caps, Server: "http://192.168.0.184:8080", ServerVia: "discovered", Addresses: []string{"192.168.2.50"}})

	h.do("GET", "/api/connectivity", adminToken, nil, &v)
	if len(v.Changes) != 2 {
		t.Fatalf("changes %+v", v.Changes)
	}
	all := v.Changes[0].Message + "|" + v.Changes[1].Message
	if !strings.Contains(all, "claude: IP address changed from 192.168.2.185 to 192.168.0.184") || !strings.Contains(all, "found the server by searching its network") {
		t.Fatalf("changes %q", all)
	}
	var devs []DeviceView
	h.do("GET", "/api/devices", adminToken, nil, &devs)
	for _, d := range devs {
		if j := strings.Join(d.Issues, "|"); d.Health != "warning" || !(strings.Contains(j, "IP address changed") || strings.Contains(j, "Now reaches the server")) {
			t.Fatalf("%s: health %s issues %v", d.Name, d.Health, d.Issues)
		}
	}
	found := false
	for _, w := range v.Warnings {
		found = found || strings.Contains(w, "cm4 had to search the network")
	}
	if !found {
		t.Fatalf("warnings %q", v.Warnings)
	}
}

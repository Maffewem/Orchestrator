package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
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

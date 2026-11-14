package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/store"
)

func TestAPIKeys(t *testing.T) {
	h := newHarness(t)
	var created struct {
		Key   store.APIKey `json:"key"`
		Token string       `json:"token"`
	}
	if code := h.do("POST", "/api/api-keys", adminToken, map[string]any{"name": "ci", "role": "operator"}, &created); code != http.StatusCreated || !strings.HasPrefix(created.Token, "orch_") {
		t.Fatalf("create: %d %+v", code, created)
	}
	if strings.Contains(created.Key.Prefix, created.Token[12:]) || created.Key.Role != "operator" {
		t.Fatalf("bad key view %+v", created.Key)
	}
	if code := h.do("POST", "/api/api-keys", adminToken, map[string]any{"name": "x", "role": "root"}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad role: %d", code)
	}
	// The key works at its role: read and operate, but not admin routes.
	if code := h.do("GET", "/api/devices", created.Token, nil, nil); code != http.StatusOK {
		t.Fatalf("viewer route with operator key: %d", code)
	}
	if code := h.do("GET", "/api/settings", created.Token, nil, nil); code != http.StatusForbidden {
		t.Fatalf("admin route with operator key: %d", code)
	}
	// An admin key still cannot mint keys, add webhooks or manage users.
	var adm struct{ Token string }
	h.do("POST", "/api/api-keys", adminToken, map[string]any{"name": "root", "role": "admin"}, &adm)
	if code := h.do("GET", "/api/settings", adm.Token, nil, nil); code != http.StatusOK {
		t.Fatalf("admin key on settings: %d", code)
	}
	for _, r := range [][2]string{{"POST", "/api/api-keys"}, {"GET", "/api/api-keys"}, {"POST", "/api/webhooks"}, {"GET", "/api/webhooks"}, {"GET", "/api/users"}} {
		if code := h.do(r[0], r[1], adm.Token, map[string]any{}, nil); code != http.StatusForbidden {
			t.Fatalf("%s %s with an admin key: %d", r[0], r[1], code)
		}
	}
	var keys []store.APIKey
	h.do("GET", "/api/api-keys", adminToken, nil, &keys)
	if len(keys) != 2 || keys[0].LastUsed == nil {
		t.Fatalf("list: %+v", keys)
	}
	// Revoking stops it; a made-up key is refused.
	if code := h.do("DELETE", "/api/api-keys/"+created.Key.ID, adminToken, nil, nil); code != http.StatusNoContent {
		t.Fatalf("revoke: %d", code)
	}
	if code := h.do("GET", "/api/devices", created.Token, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked key: %d", code)
	}
	if code := h.do("GET", "/api/devices", "orch_"+strings.Repeat("a", 64), nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("unknown key: %d", code)
	}
	// An expired key is refused.
	_, tok, _ := h.st.CreateAPIKey(t.Context(), "old", "viewer", "t", time.Now().Add(-time.Hour))
	if code := h.do("GET", "/api/devices", tok, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("expired key: %d", code)
	}
}

func TestOpenAPI(t *testing.T) {
	h := newHarness(t)
	var doc struct {
		OpenAPI string                               `json:"openapi"`
		Paths   map[string]map[string]map[string]any `json:"paths"`
	}
	if code := h.do("GET", "/api/openapi.json", "", nil, &doc); code != http.StatusOK || doc.OpenAPI == "" {
		t.Fatalf("openapi: %d", code)
	}
	if doc.Paths["/api/devices"]["get"]["x-required-role"] != "viewer" || doc.Paths["/api/projects/{id}/deployments"]["post"]["x-required-role"] != "operator" {
		t.Fatalf("roles missing: %v", doc.Paths["/api/devices"])
	}
	if doc.Paths["/api/api-keys"]["post"]["x-api-key-access"] != false {
		t.Fatal("api-keys should be marked closed to keys")
	}
}

func TestEventMatches(t *testing.T) {
	cases := []struct {
		p    []string
		kind string
		want bool
	}{
		{[]string{"*"}, "device.offline", true},
		{[]string{"deployment.*"}, "deployment.failed", true},
		{[]string{"deployment.*"}, "device.offline", false},
		{[]string{"device.offline"}, "device.offline", true},
		{[]string{"device.offline"}, "device.online", false},
		{[]string{"deploy.*"}, "deployment.failed", false},
	}
	for _, c := range cases {
		if got := eventMatches(c.p, c.kind); got != c.want {
			t.Errorf("%v %s = %v", c.p, c.kind, got)
		}
	}
}

func TestWebhookDelivery(t *testing.T) {
	hookRetryDelays = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	h := newHarness(t)

	var mu sync.Mutex
	var gotBody []byte
	var gotHdr http.Header
	fails := 1 // fail the first call to prove the retry
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		gotBody, _ = io.ReadAll(r.Body)
		gotHdr = r.Header.Clone()
	}))
	defer recv.Close()

	if code := h.do("POST", "/api/webhooks", adminToken, map[string]any{"name": "bad", "url": "ftp://x", "events": []string{"*"}}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad url: %d", code)
	}
	if code := h.do("POST", "/api/webhooks", adminToken, map[string]any{"name": "bad", "url": recv.URL, "events": []string{}}, nil); code != http.StatusBadRequest {
		t.Fatalf("no events: %d", code)
	}
	var created struct {
		Webhook webhookView `json:"webhook"`
		Secret  string      `json:"secret"`
	}
	if code := h.do("POST", "/api/webhooks", adminToken, map[string]any{"name": "alerts", "url": recv.URL, "events": []string{"deployment.failed", "device.*"}}, &created); code != http.StatusCreated || !strings.HasPrefix(created.Secret, "whsec_") {
		t.Fatalf("create: %d %+v", code, created)
	}
	var listed []map[string]any
	h.do("GET", "/api/webhooks", adminToken, nil, &listed)
	if _, leaked := listed[0]["secret"]; leaked || listed[0]["secret_hint"] == "" {
		t.Fatalf("list should hide the secret: %v", listed[0])
	}

	// A matching event is delivered, after one retry, with a valid signature.
	h.srv.event(t.Context(), "dev1", "warning", "device.offline", "pi stopped responding")
	h.srv.event(t.Context(), "", "info", "deployment.succeeded", "not subscribed")
	var ds []store.Delivery
	for i := 0; i < 100; i++ {
		h.do("GET", "/api/webhooks/"+created.Webhook.ID+"/deliveries", adminToken, nil, &ds)
		if len(ds) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(ds) != 1 || !ds[0].OK || ds[0].Attempts != 2 || ds[0].Event != "device.offline" {
		t.Fatalf("deliveries: %+v", ds)
	}
	mu.Lock()
	defer mu.Unlock()
	var ev hookEvent
	if err := json.Unmarshal(gotBody, &ev); err != nil || ev.Type != "device.offline" || ev.DeviceID != "dev1" {
		t.Fatalf("body %s (%v)", gotBody, err)
	}
	if want := hookSign(created.Secret, gotHdr.Get("X-Orchestrator-Timestamp"), gotBody); gotHdr.Get("X-Orchestrator-Signature") != want {
		t.Fatalf("signature %q, want %q", gotHdr.Get("X-Orchestrator-Signature"), want)
	}
}

func TestWebhookTestAndRotate(t *testing.T) {
	hookRetryDelays = nil
	h := newHarness(t)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusGone) }))
	defer recv.Close()
	var c struct {
		Webhook webhookView `json:"webhook"`
		Secret  string      `json:"secret"`
	}
	h.do("POST", "/api/webhooks", adminToken, map[string]any{"name": "w", "url": recv.URL, "events": []string{"*"}}, &c)
	var d store.Delivery
	if code := h.do("POST", "/api/webhooks/"+c.Webhook.ID+"/test", adminToken, nil, &d); code != http.StatusOK || d.OK || d.HTTPStatus != 410 || d.Attempts != 1 {
		t.Fatalf("test ping: %d %+v", code, d)
	}
	var u struct{ Secret string }
	h.do("PATCH", "/api/webhooks/"+c.Webhook.ID, adminToken, map[string]any{"rotate_secret": true, "enabled": false}, &u)
	if u.Secret == "" || u.Secret == c.Secret {
		t.Fatalf("rotate: %+v", u)
	}
	w, _ := h.st.GetWebhook(t.Context(), c.Webhook.ID)
	if w.Secret != u.Secret || w.Enabled {
		t.Fatalf("stored %+v", w)
	}
	if code := h.do("DELETE", "/api/webhooks/"+c.Webhook.ID, adminToken, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
}

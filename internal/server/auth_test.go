package server

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

func init() { pbkdf2Iterations = 1000 }

type session struct {
	Token string `json:"token"`
	Name  string `json:"username"`
	Role  string `json:"role"`
}

func TestAccounts(t *testing.T) {
	h := newHarness(t)
	var st struct {
		SetupRequired bool `json:"setup_required"`
	}
	h.do("GET", "/api/auth/status", "", nil, &st)
	if !st.SetupRequired {
		t.Fatal("a fresh server should ask for setup")
	}
	if code := h.do("POST", "/api/auth/setup", "", map[string]string{"admin_token": "nope", "username": "matt", "password": "correct horse"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("setup with a wrong token: %d", code)
	}
	if code := h.do("POST", "/api/auth/setup", "", map[string]string{"admin_token": adminToken, "username": "matt", "password": "short"}, nil); code != http.StatusBadRequest {
		t.Fatalf("short password: %d", code)
	}
	var admin session
	if code := h.do("POST", "/api/auth/setup", "", map[string]string{"admin_token": adminToken, "username": "Matt", "password": "correct horse"}, &admin); code != http.StatusCreated || admin.Role != "admin" || admin.Name != "matt" {
		t.Fatalf("setup: %d %+v", code, admin)
	}
	if code := h.do("POST", "/api/auth/setup", "", map[string]string{"admin_token": adminToken, "username": "again", "password": "correct horse"}, nil); code != http.StatusConflict {
		t.Fatalf("second setup: %d", code)
	}
	var me identity
	if code := h.do("GET", "/api/auth/me", admin.Token, nil, &me); code != http.StatusOK || me.Name != "matt" {
		t.Fatalf("me: %d %+v", code, me)
	}

	// Create an operator and a viewer.
	var op, vw store.User
	h.do("POST", "/api/users", admin.Token, map[string]string{"username": "ops", "password": "operator-pass", "role": "operator"}, &op)
	h.do("POST", "/api/users", admin.Token, map[string]string{"username": "eyes", "password": "viewer-pass-1", "role": "viewer"}, &vw)
	if code := h.do("POST", "/api/users", admin.Token, map[string]string{"username": "ops", "password": "operator-pass", "role": "viewer"}, nil); code != http.StatusConflict {
		t.Fatalf("duplicate user: %d", code)
	}
	login := func(u, p string) (int, session) {
		var s session
		code := h.do("POST", "/api/auth/login", "", map[string]string{"username": u, "password": p}, &s)
		return code, s
	}
	_, opS := login("ops", "operator-pass")
	_, vwS := login("eyes", "viewer-pass-1")
	if opS.Token == "" || vwS.Token == "" {
		t.Fatal("logins failed")
	}

	reg := h.register("pi")
	var p ProjectView
	body := map[string]any{"name": "web", "device_id": reg.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx"}, "runtime": map[string]any{"ports": []string{"8081:80"}}}
	if code := h.do("POST", "/api/projects", vwS.Token, body, nil); code != http.StatusForbidden {
		t.Fatalf("viewer creating a project: %d", code)
	}
	if code := h.do("POST", "/api/projects", opS.Token, body, &p); code != http.StatusCreated {
		t.Fatalf("operator creating a project: %d", code)
	}
	if code := h.do("GET", "/api/projects", vwS.Token, nil, nil); code != http.StatusOK {
		t.Fatalf("viewer reading: %d", code)
	}
	if code := h.do("GET", "/api/settings", opS.Token, nil, nil); code != http.StatusForbidden {
		t.Fatalf("operator reading settings: %d", code)
	}
	if code := h.do("DELETE", "/api/projects/"+p.ID, opS.Token, nil, nil); code != http.StatusForbidden {
		t.Fatalf("operator deleting a project: %d", code)
	}
	if code := h.do("GET", "/api/users", vwS.Token, nil, nil); code != http.StatusForbidden {
		t.Fatalf("viewer listing users: %d", code)
	}
	if code := h.do("GET", "/api/projects", "bogus", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", code)
	}

	// Disabling a user ends their session; a role change does too.
	h.do("PATCH", "/api/users/"+vw.ID, admin.Token, map[string]any{"disabled": true}, nil)
	if code := h.do("GET", "/api/projects", vwS.Token, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("disabled user still signed in: %d", code)
	}
	if code, _ := login("eyes", "viewer-pass-1"); code != http.StatusUnauthorized {
		t.Fatalf("disabled user logged in: %d", code)
	}

	// The last administrator can't be removed, demoted or disabled.
	var users []store.User
	h.do("GET", "/api/users", admin.Token, nil, &users)
	var matt store.User
	for _, u := range users {
		if u.Username == "matt" {
			matt = u
		}
	}
	for _, patch := range []map[string]any{{"role": "viewer"}, {"disabled": true}} {
		if code := h.do("PATCH", "/api/users/"+matt.ID, admin.Token, patch, nil); code != http.StatusConflict {
			t.Fatalf("last admin patch %v: %d", patch, code)
		}
	}
	if code := h.do("DELETE", "/api/users/"+matt.ID, admin.Token, nil, nil); code != http.StatusConflict {
		t.Fatalf("last admin delete: %d", code)
	}

	// A user can change their own password; other sessions end, this one stays.
	_, opS2 := login("ops", "operator-pass")
	if code := h.do("POST", "/api/auth/password", opS.Token, map[string]string{"current": "wrong", "new": "another-pass-9"}, nil); code != http.StatusForbidden {
		t.Fatalf("wrong current password: %d", code)
	}
	if code := h.do("POST", "/api/auth/password", opS.Token, map[string]string{"current": "operator-pass", "new": "another-pass-9"}, nil); code != http.StatusNoContent {
		t.Fatalf("change password: %d", code)
	}
	if code := h.do("GET", "/api/auth/me", opS2.Token, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("other session survived: %d", code)
	}
	if code, _ := login("ops", "operator-pass"); code != http.StatusUnauthorized {
		t.Fatalf("old password works: %d", code)
	}

	// Logout ends the session.
	h.do("POST", "/api/auth/logout", opS.Token, nil, nil)
	if code := h.do("GET", "/api/auth/me", opS.Token, nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", code)
	}
	// The legacy admin token still works.
	if code := h.do("GET", "/api/settings", adminToken, nil, nil); code != http.StatusOK {
		t.Fatalf("admin token: %d", code)
	}
}

func TestLoginLockout(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/auth/setup", "", map[string]string{"admin_token": adminToken, "username": "matt", "password": "correct horse"}, nil)
	for i := 0; i < maxFailures; i++ {
		if code := h.do("POST", "/api/auth/login", "", map[string]string{"username": "matt", "password": "wrong"}, nil); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	// Even the right password is refused while locked out.
	if code := h.do("POST", "/api/auth/login", "", map[string]string{"username": "matt", "password": "correct horse"}, nil); code != http.StatusUnauthorized {
		t.Fatalf("locked out login: %d", code)
	}
}

func TestPasswordHash(t *testing.T) {
	a, _ := hashPassword("hunter2hunter2")
	b, _ := hashPassword("hunter2hunter2")
	if a == b {
		t.Fatal("hashes should be salted")
	}
	if !checkPassword("hunter2hunter2", a) || checkPassword("hunter3hunter3", a) || checkPassword("x", "garbage") {
		t.Fatal("password check wrong")
	}
}

func TestProtectedWorker(t *testing.T) {
	h := newHarness(t)
	var admin session
	h.do("POST", "/api/auth/setup", "", map[string]string{"admin_token": adminToken, "username": "matt", "password": "correct horse"}, &admin)
	reg := h.register("pi")
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{
		Addresses: []string{"192.168.1.20"}, Capabilities: []string{api.CapProxy}, Server: "http://192.168.1.5:8080"}}, &api.HeartbeatResponse{})
	var p ProjectView
	h.do("POST", "/api/projects", adminToken, map[string]any{"name": "blog", "device_id": reg.DeviceID,
		"source": map[string]any{"type": "image", "image": "nginx"}, "runtime": map[string]any{"ports": []string{"8081:80"}}}, &p)
	h.do("POST", "/api/projects/"+p.ID+"/domains", adminToken, map[string]string{"hostname": "blog.example.com"}, nil)

	gate := func(user, pw string) (int, string) {
		req, _ := http.NewRequest("GET", h.ts.URL+"/api/gate/verify?project="+p.ID, nil)
		if user != "" {
			req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pw)))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("WWW-Authenticate")
	}
	if code, _ := gate("", ""); code != http.StatusNoContent {
		t.Fatalf("unprotected worker should be open: %d", code)
	}
	var v domainsView
	if code := h.do("PUT", "/api/projects/"+p.ID+"/domains/access", admin.Token, map[string]bool{"protected": true}, &v); code != http.StatusOK || !v.Protected {
		t.Fatalf("protect: %d %+v", code, v)
	}
	if code, challenge := gate("", ""); code != http.StatusUnauthorized || !strings.HasPrefix(challenge, "Basic ") {
		t.Fatalf("anonymous: %d %q", code, challenge)
	}
	if code, _ := gate("matt", "wrong password"); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", code)
	}
	if code, _ := gate("matt", "correct horse"); code != http.StatusNoContent {
		t.Fatalf("right password: %d", code)
	}

	var resp api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: []string{api.CapProxy}, Server: "http://192.168.1.5:8080"}}, &resp)
	var file string
	for _, c := range resp.Commands {
		file = c.Args["caddyfile"]
	}
	for _, want := range []string{"forward_auth http://192.168.1.5:8080", "uri /api/gate/verify?project=" + p.ID, "reverse_proxy 127.0.0.1:8081"} {
		if !strings.Contains(file, want) {
			t.Fatalf("caddyfile missing %q:\n%s", want, file)
		}
	}

	// Without a known control plane address the site must fail closed.
	got, _ := buildCaddyfile("", "", []*store.Project{{ID: "p1", Name: "x", Runtime: store.Runtime{Ports: []string{"9000:80"}}}},
		map[string][]*store.Domain{"p1": {{Hostname: "x.example.com"}}}, nil, map[string]bool{"p1": true}, nil)
	if strings.Contains(got, "reverse_proxy") || !strings.Contains(got, "503") {
		t.Fatalf("expected a fail-closed site:\n%s", got)
	}
}

func TestSecuritySettings(t *testing.T) {
	h := newHarness(t)
	var admin session
	h.do("POST", "/api/auth/setup", "", map[string]string{"admin_token": adminToken, "username": "matt", "password": "correct horse"}, &admin)

	if code := h.do("PUT", "/api/settings/security", admin.Token, map[string]any{"session_hours": 8, "min_password": 14}, nil); code != http.StatusOK {
		t.Fatalf("save: %d", code)
	}
	if code := h.do("PUT", "/api/settings/security", admin.Token, map[string]any{"session_hours": 0, "min_password": 14}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad lifetime: %d", code)
	}
	if code := h.do("POST", "/api/users", admin.Token, map[string]string{"username": "ops", "password": "only-12-chars", "role": "viewer"}, nil); code != http.StatusBadRequest {
		t.Fatalf("password shorter than the new minimum: %d", code)
	}
	if code := h.do("POST", "/api/users", admin.Token, map[string]string{"username": "ops", "password": "long-enough-pass", "role": "viewer"}, nil); code != http.StatusCreated {
		t.Fatalf("long password: %d", code)
	}
	if got := h.srv.sessionLifetime(t.Context()).Hours(); got != 8 {
		t.Fatalf("session lifetime = %v hours", got)
	}
	// Turning the redirect on from a plain HTTP request is refused.
	if code := h.do("PUT", "/api/settings/security", admin.Token, map[string]any{"session_hours": 8, "min_password": 14, "force_https": true}, nil); code != http.StatusConflict {
		t.Fatalf("redirect over http: %d", code)
	}
}

func TestLoginAddressWideThrottle(t *testing.T) {
	var l limiter
	for i := 0; i < maxFailures; i++ {
		l.fail("login|*|1.2.3.4")
	}
	if !l.locked("login|*|1.2.3.4") {
		t.Fatal("address-wide key should lock after repeated failures")
	}
}

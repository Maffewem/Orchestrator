package server

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// pairedHarnesses returns an active server and a standby that has joined it.
func pairedHarnesses(t *testing.T) (a, b *harness, token string) {
	t.Helper()
	var ra, rb atomic.Int32
	a = fileHarness(t, filepath.Join(t.TempDir(), "a.db"), &ra)
	b = fileHarness(t, filepath.Join(t.TempDir(), "b.db"), &rb)
	var v haView
	if code := a.do("POST", "/api/server-ha/pair", adminToken, nil, &v); code != http.StatusOK {
		t.Fatalf("pair: %d", code)
	}
	if code := b.do("POST", "/api/server-ha/join", adminToken, haJoinRequest{ActiveURL: a.ts.URL, Token: v.Token, SelfURL: b.ts.URL}, nil); code != http.StatusOK {
		t.Fatalf("join: %d", code)
	}
	return a, b, v.Token
}

func settingsView(t *testing.T, h *harness) (manual, auto []string) {
	t.Helper()
	var s struct {
		URLs []string `json:"agent_urls"`
		Auto []string `json:"agent_urls_auto"`
	}
	if code := h.do("GET", "/api/settings", adminToken, nil, &s); code != http.StatusOK {
		t.Fatalf("settings: %d", code)
	}
	return s.URLs, s.Auto
}

func TestStandbyAddressJoinsAgentListAndLeaves(t *testing.T) {
	ctx := context.Background()
	a, b, _ := pairedHarnesses(t)

	// Joining pulled a copy, which told the active server where the standby is.
	got := a.srv.agentURLs(ctx)
	if len(got) != 1 || got[0] != b.ts.URL {
		t.Fatalf("agent URLs after pairing = %v", got)
	}
	// Agents receive it in their next heartbeat reply, after the manual addresses.
	a.do("PUT", "/api/settings/agent-urls", adminToken, map[string]any{"urls": []string{"http://manual:8080"}}, nil)
	reg := a.register("pi")
	var hb api.HeartbeatResponse
	a.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{}, &hb)
	if strings.Join(hb.ServerURLs, ",") != "http://manual:8080,"+b.ts.URL {
		t.Fatalf("announced = %v", hb.ServerURLs)
	}
	// The manual list in Settings stays the admin's own; the automatic address is separate, so
	// saving the manual list never copies it in.
	manual, auto := settingsView(t, a)
	if strings.Join(manual, ",") != "http://manual:8080" || len(auto) != 1 || auto[0] != b.ts.URL {
		t.Fatalf("settings manual=%v auto=%v", manual, auto)
	}
	// Listing the same address by hand does not duplicate it.
	a.do("PUT", "/api/settings/agent-urls", adminToken, map[string]any{"urls": []string{b.ts.URL, "http://manual:8080"}}, nil)
	if got := a.srv.agentURLs(ctx); len(got) != 2 {
		t.Fatalf("duplicate not removed: %v", got)
	}
	a.do("PUT", "/api/settings/agent-urls", adminToken, map[string]any{"urls": []string{"http://manual:8080"}}, nil)

	// The standby leaving takes its address away again.
	if code := b.do("POST", "/api/server-ha/leave", adminToken, nil, nil); code != http.StatusAccepted {
		t.Fatalf("leave: %d", code)
	}
	if got := a.srv.agentURLs(ctx); len(got) != 1 || got[0] != "http://manual:8080" {
		t.Fatalf("agent URLs after unpairing = %v", got)
	}
	if st := a.srv.loadHA(ctx); st.PeerURL != "" {
		t.Fatalf("peer address kept: %q", st.PeerURL)
	}
}

func TestNewPairingTokenDropsOldStandbyAddress(t *testing.T) {
	ctx := context.Background()
	a, _, _ := pairedHarnesses(t)
	if len(a.srv.agentURLs(ctx)) != 1 {
		t.Fatal("standby address missing")
	}
	a.do("POST", "/api/server-ha/pair", adminToken, nil, nil)
	if got := a.srv.agentURLs(ctx); len(got) != 0 {
		t.Fatalf("old standby still announced: %v", got)
	}
	// A standby with a wrong token can't plant an address.
	req, _ := http.NewRequest("GET", a.ts.URL+"/api/server-ha/snapshot", nil)
	req.Header.Set(haTokenHeader, "wrong")
	req.Header.Set(haStandbyHeader, "http://evil:1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := a.srv.agentURLs(ctx); len(got) != 0 {
		t.Fatalf("unauthenticated address accepted: %v", got)
	}
}

func TestStandbyAddressIsTheOldServersAfterFailover(t *testing.T) {
	ctx := context.Background()
	a, b, token := pairedHarnesses(t)
	// B fences A, as a promoted B would; A learns its own address from the request.
	if err := b.srv.haPost(ctx, a.ts.URL, token, "/api/server-ha/fence", haFenceRequest{Epoch: 2, NewURL: b.ts.URL, YourURL: a.ts.URL}); err != nil {
		t.Fatal(err)
	}
	st := a.srv.loadHA(ctx)
	if st.Role != haRoleStandby || st.PeerURL != b.ts.URL || st.SelfURL != a.ts.URL {
		t.Fatalf("old server after fence: %+v", st)
	}
	// A standby's own list never carries the other server as an automatic address.
	if got := a.srv.agentURLs(ctx); len(got) != 0 {
		t.Fatalf("standby announces %v", got)
	}
}

func TestServerRefusesAgentThatSawNewerEpoch(t *testing.T) {
	var r atomic.Int32
	a := fileHarness(t, filepath.Join(t.TempDir(), "a.db"), &r) // epoch 1
	reg := a.register("pi")
	call := func(epoch string) int {
		req, _ := http.NewRequest("POST", a.ts.URL+"/api/agent/heartbeat", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+reg.AgentToken)
		req.Header.Set("Content-Type", "application/json")
		if epoch != "" {
			req.Header.Set(haEpochHeader, epoch)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.Header.Get(haEpochHeader) != "1" {
			t.Fatalf("response epoch = %q", resp.Header.Get(haEpochHeader))
		}
		return resp.StatusCode
	}
	if code := call(""); code != http.StatusOK {
		t.Fatalf("no epoch (older agent): %d", code)
	}
	if code := call("1"); code != http.StatusOK {
		t.Fatalf("same epoch: %d", code)
	}
	if code := call("2"); code != http.StatusConflict {
		t.Fatalf("agent that saw epoch 2: %d", code)
	}
}

func TestStaleServerReportShowsOnDevice(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi")
	stale := []api.StaleServer{{URL: "http://old:8080", Epoch: 1, Seen: time.Now()}}
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Hostname: "pi"}, StaleServers: stale, Epoch: 2}, nil)

	var devs []DeviceView
	h.do("GET", "/api/devices", adminToken, nil, &devs)
	if len(devs) != 1 || devs[0].Health != "warning" || len(devs[0].Issues) == 0 || !strings.Contains(strings.Join(devs[0].Issues, " "), "http://old:8080") {
		t.Fatalf("device: %+v", devs)
	}
	var evs []struct{ Kind string }
	h.do("GET", "/api/events", adminToken, nil, &evs)
	n := 0
	for _, e := range evs {
		if e.Kind == "device.stale_server" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("stale events = %d", n)
	}
	// Reporting the same stale server again is not a new event, and an empty report clears it.
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{StaleServers: stale}, nil)
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{}, nil)
	h.do("GET", "/api/devices", adminToken, nil, &devs)
	if strings.Contains(strings.Join(devs[0].Issues, " "), "old:8080") {
		t.Fatalf("warning not cleared: %v", devs[0].Issues)
	}
}

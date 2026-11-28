package server

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

// answeringAgent heartbeats and answers every command with reply(command).
func answeringAgent(t *testing.T, h *harness, reg api.RegisterResponse, caps []string, reply func(api.Command) api.CommandResult) (seen func() []api.Command, stop func()) {
	var mu sync.Mutex
	var cmds []api.Command
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			var res api.HeartbeatResponse
			h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: caps}}, &res)
			for _, c := range res.Commands {
				mu.Lock()
				cmds = append(cmds, c)
				mu.Unlock()
				h.do("POST", "/api/agent/commands/"+c.ID+"/result", reg.AgentToken, reply(c), nil)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()
	return func() []api.Command { mu.Lock(); defer mu.Unlock(); return append([]api.Command(nil), cmds...) }, func() { close(done) }
}

func TestHardwareNeedsAdminAndIsAudited(t *testing.T) {
	h := newHarness(t)
	reg := h.register("blade-1")
	base := "/api/devices/" + reg.DeviceID

	// An agent from before the feature is told to update, not queued.
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{Host: api.HostInfo{Capabilities: []string{api.CapStream}}}, &api.HeartbeatResponse{})
	if code := h.do("GET", base+"/ssh", adminToken, nil, nil); code != http.StatusConflict {
		t.Fatalf("old agent: %d", code)
	}

	seen, stop := answeringAgent(t, h, reg, []string{api.CapLED, api.CapSSH, api.CapConsole, api.CapStream}, func(c api.Command) api.CommandResult {
		switch c.Action {
		case api.ActionSSHConfig:
			if c.Args["confirm"] != "1" {
				return api.CommandResult{Error: "refused: this could lock everyone out of SSH"}
			}
			return api.CommandResult{OK: true, Output: "changed; reverts in 120 seconds unless confirmed"}
		case api.ActionLEDIdentify:
			return api.CommandResult{OK: true, Output: "15"}
		case api.ActionSSHStatus, api.ActionLEDList:
			return api.CommandResult{OK: true, Output: `{"leds":[]}`}
		}
		return api.CommandResult{OK: true, Output: "{}"}
	})
	defer stop()

	op := h.makeUser("olly", "operator")
	viewer := h.makeUser("vera", "viewer")
	for _, tc := range []struct{ method, path string }{
		{"GET", base + "/ssh"}, {"GET", base + "/leds"}, {"POST", base + "/leds/identify"},
		{"PUT", base + "/ssh/config"}, {"POST", base + "/ssh/keys"}, {"POST", base + "/ssh/keys/remove"},
		{"PUT", base + "/leds/config"}, {"POST", base + "/ssh/confirm"},
	} {
		for _, tok := range []string{op, viewer} {
			if code := h.do(tc.method, tc.path, tok, map[string]any{}, nil); code != http.StatusForbidden {
				t.Fatalf("%s %s with a lower role: %d", tc.method, tc.path, code)
			}
		}
	}
	if code := h.do("POST", "/api/devices/"+reg.DeviceID+"/streams", op, map[string]any{"kind": "console"}, nil); code != http.StatusForbidden {
		t.Fatalf("operator console: %d", code)
	}

	settings := map[string]any{"password_auth": false, "pubkey_auth": true, "root_login": "no"}
	// Without typing the device name back the agent refuses, and the server says why.
	if code := h.do("PUT", base+"/ssh/config", adminToken, settings, nil); code != http.StatusConflict {
		t.Fatalf("unconfirmed lockout: %d", code)
	}
	settings["confirm"] = "BLADE-1"
	if code := h.do("PUT", base+"/ssh/config", adminToken, settings, nil); code != http.StatusOK {
		t.Fatalf("confirmed change: %d", code)
	}
	var flash struct {
		Until time.Time `json:"until"`
	}
	if code := h.do("POST", base+"/leds/identify", adminToken, map[string]any{"seconds": 15}, &flash); code != http.StatusOK || !flash.Until.After(time.Now()) {
		t.Fatalf("identify: %d %v", code, flash)
	}
	var dev DeviceView
	h.do("GET", base, adminToken, nil, &dev)
	if dev.IdentifyingUntil == nil {
		t.Fatal("the device view should say the device is flashing")
	}

	var console struct {
		ID string `json:"id"`
	}
	if code := h.do("POST", "/api/devices/"+reg.DeviceID+"/streams", adminToken, map[string]any{"kind": "console", "cols": 120, "rows": 40}, &console); code != http.StatusCreated {
		t.Fatalf("console: %d", code)
	}
	var poll api.StreamPoll
	h.do("GET", "/api/agent/streams", reg.AgentToken, nil, &poll)
	if len(poll.Streams) != 1 || poll.Streams[0].Kind != api.StreamConsole || poll.Streams[0].Container != "" || poll.Streams[0].Cols != 120 {
		t.Fatalf("console stream for the agent: %+v", poll)
	}
	// Typing is audited like a container shell, and says it was on the device.
	h.do("POST", "/api/streams/"+console.ID+"/input", adminToken, map[string]any{"data": "bHMgLWxhCg=="}, nil) // "ls -la\n"

	var events []struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}
	h.do("GET", "/api/events?device="+reg.DeviceID+"&limit=100", adminToken, nil, &events)
	kinds := map[string]string{}
	for _, e := range events {
		kinds[e.Kind] = e.Message
	}
	for _, k := range []string{"ssh.config", "led.identify", "console.opened", "shell.input"} {
		if kinds[k] == "" {
			t.Errorf("no %q event in the activity log: %v", k, kinds)
		}
	}
	if !strings.Contains(kinds["shell.input"], "on the device: ls -la") {
		t.Errorf("console input audit: %q", kinds["shell.input"])
	}
	// The generic commands API can't be used to reach these actions.
	if code := h.do("POST", base+"/commands", adminToken, map[string]any{"action": api.ActionSSHConfig, "target": "x"}, nil); code != http.StatusBadRequest {
		t.Fatalf("generic commands API: %d", code)
	}
	if len(seen()) == 0 {
		t.Fatal("agent saw no commands")
	}
}

func TestBladeOnBasicSupplyWarnsUnderLoad(t *testing.T) {
	h := newHarness(t)
	reg := h.register("blade-2")
	beat := func(src string, load float64) DeviceView {
		h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
			Host:    api.HostInfo{CPUs: 4, Power: &api.PowerSource{Source: src, MaxWatts: 15}},
			Metrics: api.Metrics{Load5: load},
		}, &api.HeartbeatResponse{})
		var v DeviceView
		h.do("GET", "/api/devices/"+reg.DeviceID, adminToken, nil, &v)
		return v
	}
	has := func(v DeviceView) bool {
		for _, i := range v.Issues {
			if strings.Contains(i, "5 V-only") {
				return true
			}
		}
		return false
	}
	if !has(beat(api.PowerBasic, 5)) {
		t.Fatal("a loaded blade on a 5 V supply should warn")
	}
	if has(beat(api.PowerBasic, 0.5)) || has(beat(api.PowerPoEPlus, 5)) {
		t.Fatal("no warning when idle, or on PoE+")
	}
}

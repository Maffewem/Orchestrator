package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
)

type streamOut struct {
	Data   []byte `json:"data"`
	Next   int64  `json:"next"`
	Closed bool   `json:"closed"`
	Error  string `json:"error"`
}

func TestStreams(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi-4")
	beat := func(caps []string) {
		h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
			Host: api.HostInfo{Capabilities: caps},
			Containers: []api.Container{
				{ID: "c1", Name: "aurora", State: "running"},
				{ID: "c2", Name: "old", State: "exited"},
			}}, &api.HeartbeatResponse{})
	}
	open := func(body map[string]any) (int, string) {
		var out struct {
			ID string `json:"id"`
		}
		code := h.do("POST", "/api/devices/"+reg.DeviceID+"/streams", adminToken, body, &out)
		return code, out.ID
	}

	beat(nil)
	if code, _ := open(map[string]any{"kind": "logs", "container": "aurora"}); code != http.StatusConflict {
		t.Fatalf("old agent: %d", code)
	}
	beat([]string{api.CapStream})
	if code, _ := open(map[string]any{"kind": "logs", "container": "nope"}); code != http.StatusNotFound {
		t.Fatalf("unknown container: %d", code)
	}
	if code, _ := open(map[string]any{"kind": "shell", "container": "old"}); code != http.StatusConflict {
		t.Fatalf("shell in stopped container: %d", code)
	}
	if code, _ := open(map[string]any{"kind": "other", "container": "aurora"}); code != http.StatusBadRequest {
		t.Fatalf("bad kind: %d", code)
	}

	code, id := open(map[string]any{"kind": "shell", "container": "aurora", "cols": 100, "rows": 30})
	if code != http.StatusCreated {
		t.Fatalf("open shell: %d", code)
	}

	// The agent hears about it straight away and gets the resolved container ID.
	var poll api.StreamPoll
	h.do("GET", "/api/agent/streams", reg.AgentToken, nil, &poll)
	if len(poll.Streams) != 1 || poll.Streams[0].ID != id || poll.Streams[0].Container != "c1" || poll.Streams[0].Cols != 100 {
		t.Fatalf("poll: %+v", poll)
	}

	// Output flows to the browser by offset; nothing is repeated.
	h.do("POST", "/api/agent/streams/"+id+"/output", reg.AgentToken, api.StreamOutput{Data: []byte("hello ")}, nil)
	h.do("POST", "/api/agent/streams/"+id+"/output", reg.AgentToken, api.StreamOutput{Data: []byte("world")}, nil)
	var out streamOut
	h.do("GET", "/api/streams/"+id+"/output?from=0", adminToken, nil, &out)
	if string(out.Data) != "hello world" || out.Next != 11 {
		t.Fatalf("output: %+v", out)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		h.do("POST", "/api/agent/streams/"+id+"/output", reg.AgentToken, api.StreamOutput{Data: []byte("!")}, nil)
	}()
	h.do("GET", "/api/streams/"+id+"/output?from=11", adminToken, nil, &out)
	if string(out.Data) != "!" || out.Next != 12 {
		t.Fatalf("long-poll output: %+v", out)
	}

	// Keystrokes and the new size reach the agent.
	h.do("POST", "/api/streams/"+id+"/input", adminToken, api.StreamInput{Data: []byte("ls\n"), Cols: 120, Rows: 40}, nil)
	var in api.StreamInput
	h.do("GET", "/api/agent/streams/"+id+"/input", reg.AgentToken, nil, &in)
	if string(in.Data) != "ls\n" || in.Cols != 120 || in.Rows != 40 {
		t.Fatalf("input: %+v", in)
	}

	// An agent-side exit ends the stream with its reason.
	h.do("POST", "/api/agent/streams/"+id+"/output", reg.AgentToken, api.StreamOutput{Closed: true, Error: "exec failed"}, nil)
	h.do("GET", "/api/streams/"+id+"/output?from=12", adminToken, nil, &out)
	if !out.Closed || out.Error != "exec failed" {
		t.Fatalf("closed: %+v", out)
	}
	if code := h.do("POST", "/api/streams/"+id+"/input", adminToken, api.StreamInput{Data: []byte("x")}, nil); code != http.StatusGone {
		t.Fatalf("input after close: %d", code)
	}

	// Closing from the browser tells the agent to stop.
	_, id2 := open(map[string]any{"kind": "logs", "container": "aurora"})
	h.do("GET", "/api/agent/streams", reg.AgentToken, nil, &poll)
	h.do("DELETE", "/api/streams/"+id2, adminToken, nil, nil)
	var res struct{ Closed bool }
	h.do("POST", "/api/agent/streams/"+id2+"/output", reg.AgentToken, api.StreamOutput{Data: []byte("x")}, &res)
	if !res.Closed {
		t.Fatal("agent was not told the stream closed")
	}
}

func TestShellNeedsAdmin(t *testing.T) {
	h := newHarness(t)
	reg := h.register("pi-4")
	h.do("POST", "/api/agent/heartbeat", reg.AgentToken, api.HeartbeatRequest{
		Host:       api.HostInfo{Capabilities: []string{api.CapStream}},
		Containers: []api.Container{{ID: "c1", Name: "aurora", State: "running"}}}, &api.HeartbeatResponse{})
	op := h.makeUser("olly", "operator")
	if code := h.do("POST", "/api/devices/"+reg.DeviceID+"/streams", op, map[string]any{"kind": "shell", "container": "aurora"}, nil); code != http.StatusForbidden {
		t.Fatalf("operator shell: %d", code)
	}
	var out struct {
		ID string `json:"id"`
	}
	if code := h.do("POST", "/api/devices/"+reg.DeviceID+"/streams", op, map[string]any{"kind": "logs", "container": "aurora"}, &out); code != http.StatusCreated {
		t.Fatalf("operator logs: %d", code)
	}
	viewer := h.makeUser("vera", "viewer")
	if code := h.do("GET", "/api/streams/"+out.ID+"/output?from=0", viewer, nil, nil); code != http.StatusForbidden {
		t.Fatalf("viewer logs: %d", code)
	}
}

// makeUser creates an account and returns a session token for it.
func (h *harness) makeUser(name, role string) string {
	h.t.Helper()
	pw := name + "-password-1"
	if code := h.do("POST", "/api/users", adminToken, map[string]string{"username": name, "password": pw, "role": role}, nil); code != http.StatusCreated {
		h.t.Fatalf("create %s: %d", name, code)
	}
	var s session
	if code := h.do("POST", "/api/auth/login", "", map[string]string{"username": name, "password": pw}, &s); code != http.StatusOK {
		h.t.Fatalf("login %s: %d", name, code)
	}
	return s.Token
}

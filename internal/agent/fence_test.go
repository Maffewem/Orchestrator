package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"path/filepath"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func respWithEpoch(h string) *http.Response {
	r := &http.Response{Header: http.Header{}}
	if h != "" {
		r.Header.Set("X-Orch-Epoch", h)
	}
	return r
}

func newFenceAgent(t *testing.T, server string) *Agent {
	t.Helper()
	a := &Agent{cfg: Config{Server: server, StatePath: filepath.Join(t.TempDir(), "state.json")}, servers: []string{server}, log: nopLogger()}
	a.state = state{DeviceID: "d1", AgentToken: "tok"}
	return a
}

func storedEpoch(t *testing.T, a *Agent) int {
	t.Helper()
	b, err := os.ReadFile(a.cfg.StatePath)
	if err != nil {
		return 0
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st.Epoch
}

func TestFenceEpochRules(t *testing.T) {
	a := newFenceAgent(t, "http://s1")

	// An older server that sends no header is never fenced, and nothing is stored.
	if err := a.fence(respWithEpoch("")); err != nil {
		t.Fatal(err)
	}
	if a.epoch() != 0 {
		t.Fatalf("epoch = %d", a.epoch())
	}
	// An agent with no stored epoch accepts the first one it sees and saves it.
	if err := a.fence(respWithEpoch("2")); err != nil {
		t.Fatal(err)
	}
	if a.epoch() != 2 || storedEpoch(t, a) != 2 {
		t.Fatalf("epoch %d, stored %d", a.epoch(), storedEpoch(t, a))
	}
	// The same epoch is fine; a higher one raises it.
	if err := a.fence(respWithEpoch("2")); err != nil {
		t.Fatal(err)
	}
	if err := a.fence(respWithEpoch("3")); err != nil || a.epoch() != 3 || storedEpoch(t, a) != 3 {
		t.Fatalf("raise: err %v epoch %d stored %d", err, a.epoch(), storedEpoch(t, a))
	}
	// A lower one is refused, never lowers the stored epoch, and is remembered for the report.
	err := a.fence(respWithEpoch("2"))
	if _, ok := err.(*errStaleServer); !ok {
		t.Fatalf("lower epoch: %v", err)
	}
	if a.epoch() != 3 || storedEpoch(t, a) != 3 {
		t.Fatal("a stale server lowered the epoch")
	}
	if st := a.staleServers(); len(st) != 1 || st[0].URL != "http://s1" || st[0].Epoch != 2 {
		t.Fatalf("stale = %+v", st)
	}
	// Garbage in the header is ignored rather than trusted.
	if err := a.fence(respWithEpoch("abc")); err != nil {
		t.Fatal(err)
	}
	// Once that address reports a current epoch again, the warning clears.
	if err := a.fence(respWithEpoch("3")); err != nil {
		t.Fatal(err)
	}
	if st := a.staleServers(); len(st) != 0 {
		t.Fatalf("stale not cleared: %+v", st)
	}
}

func TestFenceStoredEpochSurvivesRestartAndReRegister(t *testing.T) {
	a := newFenceAgent(t, "http://s1")
	a.fence(respWithEpoch("4"))
	// A restart reloads the epoch from state.json.
	b := &Agent{cfg: a.cfg, servers: []string{"http://s1"}, log: nopLogger()}
	if err := b.ensureRegistered(t.Context()); err != nil {
		t.Fatal(err)
	}
	if b.epoch() != 4 {
		t.Fatalf("epoch after restart = %d", b.epoch())
	}
	if err := b.fence(respWithEpoch("3")); err == nil {
		t.Fatal("a stale server was accepted after a restart")
	}
}

func TestPostRefusesStaleServerEvenOn401(t *testing.T) {
	// A stale server answering 401 must not make the agent drop its identity.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Orch-Epoch", "1")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	a := newFenceAgent(t, ts.URL)
	a.http = ts.Client()
	a.state.Epoch = 2
	err := a.post(t.Context(), "/api/agent/heartbeat", "tok", map[string]string{}, nil)
	if _, ok := err.(*errStaleServer); !ok {
		t.Fatalf("err = %v", err)
	}
}

// epochServer is a fake control plane with a fixed epoch. It records what agents send and, when
// it has a command, hands it out.
type epochServer struct {
	*httptest.Server
	mu      sync.Mutex
	beats   int
	last    api.HeartbeatRequest
	results []string // command ids reported to this server
	cmd     string
	header  atomic.Bool
}

func newEpochServer(t *testing.T, epoch int, cmd string) *epochServer {
	t.Helper()
	s := &epochServer{cmd: cmd}
	s.header.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agent/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var req api.HeartbeatRequest
		json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.beats++
		s.last = req
		s.mu.Unlock()
		if s.header.Load() {
			w.Header().Set("X-Orch-Epoch", strconv.Itoa(epoch))
		}
		res := api.HeartbeatResponse{IntervalSeconds: 1}
		if s.cmd != "" {
			res.Commands = []api.Command{{ID: s.cmd, Action: "no-such-action"}}
		}
		json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("POST /api/agent/commands/{id}/result", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.results = append(s.results, r.PathValue("id"))
		s.mu.Unlock()
		if s.header.Load() {
			w.Header().Set("X-Orch-Epoch", strconv.Itoa(epoch))
		}
		w.Write([]byte("{}"))
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *epochServer) snapshot() (int, api.HeartbeatRequest, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.beats, s.last, append([]string(nil), s.results...)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(25 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
	}
}

// Two servers with different epochs: the agent has seen epoch 3 from the new server, so the
// old one (epoch 2) that comes back and offers a command is ignored, and the agent tells the
// current server about it.
func TestRunIgnoresStaleServerAndReportsIt(t *testing.T) {
	stale := newEpochServer(t, 2, "stale-cmd")
	current := newEpochServer(t, 3, "")

	a := discoveryAgent(t, nil, "")
	a.configured = []string{stale.URL, current.URL}
	a.cfg.Server = stale.URL
	a.setServers("")
	a.state.Epoch = 3
	a.saveState()
	ctx, cancel := contextWithCancel(t)
	defer cancel()
	go a.Run(ctx)

	waitFor(t, "a heartbeat to the current server", func() bool {
		_, last, _ := current.snapshot()
		return len(last.StaleServers) > 0
	})
	_, last, _ := current.snapshot()
	if last.Epoch != 3 || last.StaleServers[0].URL != stale.URL || last.StaleServers[0].Epoch != 2 {
		t.Fatalf("report to the current server: %+v", last)
	}
	// Give a queued command time to (wrongly) run and report; none should.
	time.Sleep(300 * time.Millisecond)
	for _, srv := range []*epochServer{stale, current} {
		if _, _, res := srv.snapshot(); len(res) != 0 {
			t.Fatalf("a command from the stale server ran: %v", res)
		}
	}
	if storedEpoch(t, a) != 3 {
		t.Fatalf("stored epoch = %d", storedEpoch(t, a))
	}
}

// An agent that has never seen an epoch, and servers that send none, keep working as before.
func TestRunUpgradePaths(t *testing.T) {
	// No stored epoch: the first one seen is accepted and stored.
	srv := newEpochServer(t, 5, "")
	a := discoveryAgent(t, nil, "")
	a.configured = []string{srv.URL}
	a.cfg.Server = srv.URL
	a.setServers("")
	a.saveState()
	ctx, cancel := contextWithCancel(t)
	go a.Run(ctx)
	waitFor(t, "the epoch to be stored", func() bool { return storedEpoch(t, a) == 5 })
	cancel()

	// A server with no epoch header (an older build) is still accepted by an agent that has one.
	old := newEpochServer(t, 0, "")
	old.header.Store(false)
	b := discoveryAgent(t, nil, "")
	b.configured = []string{old.URL}
	b.cfg.Server = old.URL
	b.setServers("")
	b.state.Epoch = 9
	b.saveState()
	ctx2, cancel2 := contextWithCancel(t)
	defer cancel2()
	go b.Run(ctx2)
	waitFor(t, "heartbeats to a header-less server", func() bool { n, _, _ := old.snapshot(); return n >= 2 })
	if len(b.staleServers()) != 0 {
		t.Fatal("a server without an epoch was treated as stale")
	}
}

func contextWithCancel(t *testing.T) (ctx context.Context, cancel context.CancelFunc) {
	return context.WithCancel(t.Context())
}

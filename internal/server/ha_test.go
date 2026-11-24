package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/store"
)

// fileHarness is a harness whose database is a real file, so it can be copied and restored.
func fileHarness(t *testing.T, db string, restarts *atomic.Int32) *harness {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, _, err := New(context.Background(), st, Config{AdminToken: adminToken, HeartbeatInterval: time.Second, DBPath: db, Version: "test", Restart: func() { restarts.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	srv.haRestartIn = 50 * time.Millisecond
	srv.loadHAState(context.Background())
	return &harness{t: t, srv: srv, ts: newTestServer(t, srv), st: st}
}

func TestServerHAStandbyPromoteAndFence(t *testing.T) {
	ctx := context.Background()
	dirA, dirB := t.TempDir(), t.TempDir()
	var restartsA, restartsB atomic.Int32
	a := fileHarness(t, filepath.Join(dirA, "a.db"), &restartsA)
	b := fileHarness(t, filepath.Join(dirB, "b.db"), &restartsB)
	a.st.SetSetting(ctx, "marker", "from-a")

	var v haView
	if code := a.do("POST", "/api/server-ha/pair", adminToken, nil, &v); code != http.StatusOK || v.Token == "" {
		t.Fatalf("pair: %d %+v", code, v)
	}
	token := v.Token
	// A wrong token is refused and nothing is leaked.
	if code := b.do("POST", "/api/server-ha/join", adminToken, haJoinRequest{ActiveURL: a.ts.URL, Token: "nope", SelfURL: b.ts.URL}, nil); code != http.StatusBadGateway {
		t.Fatalf("join with a wrong token: %d", code)
	}
	if code := b.do("POST", "/api/server-ha/join", adminToken, haJoinRequest{ActiveURL: a.ts.URL, Token: token, SelfURL: b.ts.URL}, &v); code != http.StatusOK || v.Role != haRoleStandby || !v.Ready {
		t.Fatalf("join: %d %+v", code, v)
	}
	if a.do("GET", "/api/server-ha", adminToken, nil, &v); v.StandbySeen == nil {
		t.Fatal("the active server did not notice the standby fetching a copy")
	}

	// A standby is read-only and refuses agents.
	if code := b.do("POST", "/api/projects", adminToken, map[string]any{"name": "x"}, nil); code != http.StatusLocked {
		t.Fatalf("standby write: %d", code)
	}
	if code := b.do("GET", "/api/devices", adminToken, nil, nil); code != http.StatusOK {
		t.Fatalf("standby read: %d", code)
	}
	if code := b.do("POST", "/api/agent/heartbeat", "x", api.HeartbeatRequest{}, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("standby heartbeat: %d", code)
	}

	// A change on the active server reaches the standby on the next copy.
	a.st.SetSetting(ctx, "marker", "second")
	if err := b.srv.haPullOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// Promote without confirming the active server is down asks it to step down first.
	if code := b.do("POST", "/api/server-ha/promote", adminToken, haPromoteRequest{}, nil); code != http.StatusAccepted {
		t.Fatalf("planned promote: %d", code)
	}
	if code := a.do("GET", "/api/server-ha", adminToken, nil, &v); code != http.StatusOK || v.Role != haRoleStandby {
		t.Fatalf("old server did not step down: %d %+v", code, v)
	}
	time.Sleep(700 * time.Millisecond)
	if restartsA.Load() == 0 || restartsB.Load() == 0 {
		t.Fatalf("restarts: a=%d b=%d", restartsA.Load(), restartsB.Load())
	}

	// Simulate B's restart: the staged copy replaces its database.
	dbB := filepath.Join(dirB, "b.db")
	b.st.Close()
	if ok, err := store.ApplyPendingRestore(dbB); err != nil || !ok {
		t.Fatalf("apply restore: %v %v", ok, err)
	}
	st2, err := store.Open(dbB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st2.Close() })
	srv2, _, err := New(ctx, st2, Config{AdminToken: adminToken, HeartbeatInterval: time.Second, DBPath: dbB, Version: "test", Restart: func() {}})
	if err != nil {
		t.Fatal(err)
	}
	srv2.loadHAState(ctx)
	if got, _ := st2.GetSetting(ctx, "marker"); got != "second" {
		t.Fatalf("promoted data: %q", got)
	}
	st := srv2.loadHA(ctx)
	if st.Role != haRoleActive || st.Epoch < 2 || st.Previous != a.ts.URL {
		t.Fatalf("promoted state: %+v", st)
	}
	if srv2.haStandby() {
		t.Fatal("still a standby after promotion")
	}
	if _, err := os.Stat(dbB + ".promoted"); err == nil {
		t.Fatal("promotion marker left behind")
	}
}

func TestServerHAStaleServerIsFenced(t *testing.T) {
	ctx := context.Background()
	var r atomic.Int32
	a := fileHarness(t, filepath.Join(t.TempDir(), "a.db"), &r)
	var v haView
	a.do("POST", "/api/server-ha/pair", adminToken, nil, &v)
	// A fence with the wrong token, or a stale epoch, changes nothing.
	body := haFenceRequest{Epoch: 5, NewURL: "http://10.0.0.9:8080"}
	if code := a.do("POST", "/api/server-ha/fence", "", body, nil); code != http.StatusForbidden {
		t.Fatalf("no token: %d", code)
	}
	if err := a.srv.haPost(ctx, a.ts.URL, v.Token, "/api/server-ha/fence", haFenceRequest{Epoch: 1, NewURL: "http://10.0.0.9:8080"}); err == nil {
		t.Fatal("a stale epoch was accepted")
	}
	if a.srv.loadHA(ctx).Role != haRoleActive {
		t.Fatal("stepped down for a stale epoch")
	}
	if err := a.srv.haPost(ctx, a.ts.URL, v.Token, "/api/server-ha/fence", body); err != nil {
		t.Fatal(err)
	}
	if st := a.srv.loadHA(ctx); st.Role != haRoleStandby || st.Epoch != 5 || st.PeerURL != "http://10.0.0.9:8080" {
		t.Fatalf("fenced state: %+v", st)
	}
}

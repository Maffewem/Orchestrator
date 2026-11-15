package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"github.com/Maffewem/Orchestrator/internal/backupcrypt"
	"github.com/Maffewem/Orchestrator/internal/store"
)

type movePair struct {
	old, neu *harness
	newDir   string
	devTok   string
	devID    string
	port     string
}

// newMovePair starts an "old" server that has a device, a stored secret and a pending move
// target; the "new" server is created when the fake agent runs server.install.
func newMovePair(t *testing.T) *movePair {
	t.Helper()
	dir := t.TempDir()
	oldDB := filepath.Join(dir, "old", "orch.db")
	os.MkdirAll(filepath.Dir(oldDB), 0o755)
	st, err := store.Open(oldDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, _, err := New(context.Background(), st, Config{AdminToken: adminToken, HeartbeatInterval: time.Second, BackupDir: filepath.Join(dir, "old", "backups"), DBPath: oldDB, Restart: func() {}})
	if err != nil {
		t.Fatal(err)
	}
	srv.moveTick = 10 * time.Millisecond
	old := &harness{t: t, srv: srv, st: st}
	old.ts = newTestServer(t, srv)
	if err := st.SetSetting(context.Background(), "github_token", "ghp_secret_token"); err != nil {
		t.Fatal(err)
	}

	// The device that will become the server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	host := api.HostInfo{Hostname: "pi2", Server: old.ts.URL, Docker: &api.DockerInfo{Version: "27"}, Capabilities: []string{api.CapServerInstall},
		Network: []api.NetAddr{{Interface: "eth0", Address: "127.0.0.1"}}}
	dev, tok, err := st.CreateDevice(context.Background(), "pi2", nil, host)
	if err != nil {
		t.Fatal(err)
	}
	st.UpdateDeviceState(context.Background(), dev.ID, host, api.Metrics{}, nil)
	p := &movePair{old: old, newDir: filepath.Join(dir, "new"), devTok: tok, devID: dev.ID, port: port}
	p.listener(ln)
	return p
}

var movePairListeners = map[*movePair]net.Listener{}

func (p *movePair) listener(ln net.Listener) { movePairListeners[p] = ln }

// fakeAgentInstall does what the agent's server.install does, in process: fetch the bundle
// with the device token, decrypt it with the one-off password, open it as a database (which
// adopts the secret key it carries) and serve a new server from it.
func (p *movePair) fakeAgentInstall(t *testing.T, c api.Command) {
	t.Helper()
	req, _ := http.NewRequest("GET", p.old.ts.URL+c.Args["bundle"], nil)
	req.Header.Set("Authorization", "Bearer "+p.devTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("bundle download: %v %v", err, resp)
	}
	defer resp.Body.Close()
	r, err := backupcrypt.NewReader(resp.Body, c.Args["password"])
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(p.newDir, 0o755)
	newDB := filepath.Join(p.newDir, "orch.db")
	f, _ := os.Create(newDB)
	if _, err := io.Copy(f, r); err != nil {
		t.Fatal(err)
	}
	f.Close()
	st, err := store.Open(newDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, _, err := New(context.Background(), st, Config{AdminToken: adminToken, HeartbeatInterval: time.Second, BackupDir: filepath.Join(p.newDir, "backups"), DBPath: newDB, Restart: func() {}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Listener.Close()
	ts.Listener = movePairListeners[p]
	ts.Start()
	t.Cleanup(ts.Close)
	p.neu = &harness{t: t, srv: srv, st: st, ts: ts}
}

func (p *movePair) waitPhase(t *testing.T, want string) serverMove {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var v moveView
		p.old.do("GET", "/api/server-move", adminToken, nil, &v)
		if v.Move != nil && (v.Move.Phase == want || v.Move.Phase == moveFailed) {
			if v.Move.Phase != want {
				t.Fatalf("move failed: %s", v.Move.Error)
			}
			return *v.Move
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for phase %s", want)
	return serverMove{}
}

func (p *movePair) heartbeat(h *harness) api.HeartbeatResponse {
	var res api.HeartbeatResponse
	h.do("POST", "/api/agent/heartbeat", p.devTok, api.HeartbeatRequest{Host: api.HostInfo{Hostname: "pi2"}}, &res)
	return res
}

// startAndInstall begins a move and plays the device's part.
func (p *movePair) startAndInstall(t *testing.T) {
	t.Helper()
	var v moveView
	if code := p.old.do("POST", "/api/server-move", adminToken, map[string]any{
		"device_id": p.devID, "port": mustAtoi(p.port), "address": "127.0.0.1", "old_url": p.old.ts.URL, "image": "test:img",
	}, &v); code != http.StatusAccepted {
		t.Fatalf("start: %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for p.neu == nil && time.Now().Before(deadline) {
		res := p.heartbeat(p.old)
		for _, c := range res.Commands {
			if c.Action != api.ActionServerInstall {
				continue
			}
			p.fakeAgentInstall(t, c)
			body, _ := json.Marshal(api.CommandResult{OK: true, Output: "ok"})
			req, _ := http.NewRequest("POST", p.old.ts.URL+"/api/agent/commands/"+c.ID+"/result", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+p.devTok)
			http.DefaultClient.Do(req)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if p.neu == nil {
		t.Fatal("the install command never reached the device")
	}
}

func mustAtoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func TestServerMoveEndToEnd(t *testing.T) {
	p := newMovePair(t)
	p.startAndInstall(t)
	m := p.waitPhase(t, moveReady)
	if m.NewURL != "http://127.0.0.1:"+p.port {
		t.Fatalf("new url %q", m.NewURL)
	}

	// The copy carries the key: the new server reads secrets sealed by the old one.
	if v, err := p.neu.st.GetSetting(context.Background(), "github_token"); err != nil || v != "ghp_secret_token" {
		t.Fatalf("secret on the new server: %q %v", v, err)
	}
	// The new server starts clean, knowing where it came from, and with its device list intact.
	var nv moveView
	p.neu.do("GET", "/api/server-move", adminToken, nil, &nv)
	if nv.MovedFrom == nil || nv.MovedFrom.OldURL != p.old.ts.URL || nv.Move != nil || nv.StandbyTo != "" {
		t.Fatalf("new server move state: %+v", nv)
	}
	if res := p.heartbeat(p.neu); res.MoveTo != "" || len(res.ServerURLs) != 2 {
		t.Fatalf("new server heartbeat: %+v", res)
	}

	// While the move is open the old server refuses dashboard changes but keeps agents working.
	if code := p.old.do("POST", "/api/projects", adminToken, map[string]any{"name": "x"}, nil); code != http.StatusLocked {
		t.Fatalf("write during a move: %d", code)
	}
	if code := p.old.do("GET", "/api/devices", adminToken, nil, nil); code != 200 {
		t.Fatalf("read during a move: %d", code)
	}
	// The bundle is gone once the new server was verified.
	if entries, _ := os.ReadDir(p.old.srv.moveDir()); len(entries) != 0 {
		t.Fatalf("transfer bundle left behind: %v", entries)
	}

	// Switch: the old server now only redirects agents.
	if code := p.old.do("POST", "/api/server-move/switch", adminToken, nil, nil); code != 200 {
		t.Fatalf("switch: %d", code)
	}
	if res := p.heartbeat(p.old); res.MoveTo != m.NewURL || len(res.Commands) != 0 {
		t.Fatalf("old heartbeat after switch: %+v", res)
	}
	// The agent's token works on the new server unchanged.
	if res := p.heartbeat(p.neu); res.IntervalSeconds == 0 {
		t.Fatalf("new server rejected the agent: %+v", res)
	}

	// Confirm from the new server: the old one learns the move is final.
	if code := p.neu.do("POST", "/api/server-move/accept", adminToken, nil, nil); code != 200 {
		t.Fatalf("accept: %d", code)
	}
	var ov moveView
	p.old.do("GET", "/api/server-move", adminToken, nil, &ov)
	if ov.Move == nil || ov.Move.Phase != moveDone || ov.StandbyTo != m.NewURL {
		t.Fatalf("old server after confirm: %+v", ov)
	}
	nv = moveView{}
	p.neu.do("GET", "/api/server-move", adminToken, nil, &nv)
	if nv.MovedFrom != nil {
		t.Fatal("the new server still shows the move as open")
	}
	if code := p.neu.do("POST", "/api/projects", adminToken, map[string]any{"name": "ok"}, nil); code == http.StatusLocked {
		t.Fatal("the new server must accept changes")
	}
}

func TestServerMoveBackFromNewServer(t *testing.T) {
	p := newMovePair(t)
	p.startAndInstall(t)
	m := p.waitPhase(t, moveReady)
	p.old.do("POST", "/api/server-move/switch", adminToken, nil, nil)

	if code := p.neu.do("POST", "/api/server-move/back", adminToken, nil, nil); code != 200 {
		t.Fatalf("move back: %d", code)
	}
	// The new server now redirects to the old one, which is in charge again with its old address list.
	if res := p.heartbeat(p.neu); res.MoveTo != p.old.ts.URL {
		t.Fatalf("new server heartbeat: %+v", res)
	}
	if res := p.heartbeat(p.old); res.MoveTo != "" || res.IntervalSeconds == 0 {
		t.Fatalf("old server heartbeat: %+v", res)
	}
	var ov moveView
	p.old.do("GET", "/api/server-move", adminToken, nil, &ov)
	if ov.Move != nil || ov.StandbyTo != "" || len(p.old.srv.agentURLs(context.Background())) != 0 {
		t.Fatalf("old server state: %+v", ov)
	}
	if code := p.old.do("POST", "/api/projects", adminToken, map[string]any{"name": "again"}, nil); code == http.StatusLocked {
		t.Fatal("the old server must accept changes again")
	}
	_ = m
}

func TestServerMoveCancelFromOldServer(t *testing.T) {
	p := newMovePair(t)
	p.startAndInstall(t)
	p.waitPhase(t, moveReady)
	p.old.do("POST", "/api/server-move/switch", adminToken, nil, nil)

	var out struct {
		Note string `json:"note"`
	}
	if code := p.old.do("POST", "/api/server-move/cancel", adminToken, nil, &out); code != 200 || out.Note != "" {
		t.Fatalf("cancel: %d %q", code, out.Note)
	}
	if res := p.heartbeat(p.neu); res.MoveTo != p.old.ts.URL {
		t.Fatalf("the new server should send its devices back: %+v", res)
	}
	if res := p.heartbeat(p.old); res.MoveTo != "" {
		t.Fatalf("old server still redirects: %+v", res)
	}
}

func TestServerMoveRefusals(t *testing.T) {
	p := newMovePair(t)
	// Busy work blocks a move.
	p.old.st.CreateCommand(context.Background(), p.devID, api.ActionRestart, "x", nil)
	if code := p.old.do("POST", "/api/server-move", adminToken, map[string]any{"device_id": p.devID, "address": "127.0.0.1", "old_url": p.old.ts.URL, "image": "i"}, nil); code != http.StatusConflict {
		t.Fatalf("busy: %d", code)
	}
	p.old.st.ExpireCommands(context.Background(), time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	// A bad address is refused before anything is paused.
	if code := p.old.do("POST", "/api/server-move", adminToken, map[string]any{"device_id": p.devID, "address": "bad host", "old_url": p.old.ts.URL, "image": "i"}, nil); code != http.StatusBadRequest {
		t.Fatalf("bad address: %d", code)
	}
	if p.old.srv.movePaused() {
		t.Fatal("a refused move must not pause the server")
	}
	// The peer endpoint needs the shared token.
	b, _ := json.Marshal(peerRequest{Action: "resume"})
	resp, _ := http.Post(p.old.ts.URL+"/api/server-move/peer", "application/json", bytes.NewReader(b))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("peer without token: %d", resp.StatusCode)
	}
	// The bundle is not served when no move waits for the device.
	req, _ := http.NewRequest("GET", p.old.ts.URL+moveBundlePath, nil)
	req.Header.Set("Authorization", "Bearer "+p.devTok)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != http.StatusForbidden {
		t.Fatalf("bundle without a move: %d", r.StatusCode)
	}
}

func TestServerMoveExport(t *testing.T) {
	p := newMovePair(t)
	if code := p.old.do("POST", "/api/server-move/export", adminToken, map[string]string{"password": "short"}, nil); code != http.StatusBadRequest {
		t.Fatalf("short password: %d", code)
	}
	b, _ := json.Marshal(map[string]string{"password": "a-long-password"})
	req, _ := http.NewRequest("POST", p.old.ts.URL+"/api/server-move/export", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("export: %v %v", err, resp)
	}
	raw, _ := io.ReadAll(resp.Body)
	if bytes.Contains(raw, []byte("SQLite format 3")) || strings.Contains(string(raw), "ghp_secret_token") {
		t.Fatal("export is not protected")
	}
	r, err := backupcrypt.NewReader(bytes.NewReader(raw), "a-long-password")
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(r)
	if !bytes.HasPrefix(plain, []byte("SQLite format 3")) {
		t.Fatal("decrypted export is not a database")
	}
}

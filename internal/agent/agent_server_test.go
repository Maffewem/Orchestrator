package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Maffewem/Orchestrator/internal/api"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestServerURLChangeKeepsIdentity(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(p, []byte(`{"server":"http://old:8080","device_id":"d1","agent_token":"tok"}`), 0o600)
	a := &Agent{cfg: Config{Server: "http://new:8080", StatePath: p}, servers: []string{"http://new:8080"}, log: nopLogger()}
	if err := a.ensureRegistered(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.state.DeviceID != "d1" || a.state.Server != "http://new:8080" {
		t.Fatalf("state = %+v", a.state)
	}
}

func TestNextServerRotates(t *testing.T) {
	a := &Agent{cfg: Config{Server: "http://a"}, servers: []string{"http://a", "http://b"}, log: nopLogger()}
	a.nextServer()
	if a.server() != "http://b" {
		t.Fatal(a.server())
	}
	a.nextServer()
	if a.server() != "http://a" {
		t.Fatal(a.server())
	}
}

func nopLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeServer answers identify with priv and accepts heartbeats carrying token.
func fakeServer(t *testing.T, priv ed25519.PrivateKey, beats *atomic.Int32) *httptest.Server {
	t.Helper()
	pub := base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/agent/identify", func(w http.ResponseWriter, r *http.Request) {
		sig := ed25519.Sign(priv, api.IdentifyMessage(r.URL.Query().Get("nonce")))
		json.NewEncoder(w).Encode(api.IdentifyResponse{ServerKey: pub, Signature: base64.StdEncoding.EncodeToString(sig)})
	})
	mux.HandleFunc("POST /api/agent/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		beats.Add(1)
		json.NewEncoder(w).Encode(api.HeartbeatResponse{ServerKey: pub, ServerURLs: []string{"http://announced:1"}})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func discoveryAgent(t *testing.T, key ed25519.PublicKey, port string) *Agent {
	t.Helper()
	// 127.0.0.1:1 stands in for the old address: nothing listens there.
	old := "http://127.0.0.1:1"
	if port != "" {
		old = "http://127.0.0.3:" + port // nothing listens on this address, but the port is right
	}
	a, err := New(Config{Server: old, DockerHost: "unix:///nonexistent.sock", StatePath: filepath.Join(t.TempDir(), "state.json"), Logger: nopLogger()})
	if err != nil {
		t.Fatal(err)
	}
	a.state = state{Server: old, DeviceID: "d1", AgentToken: "tok", ServerKey: base64.StdEncoding.EncodeToString(key)}
	a.scanHosts = func() []string { return nil } // loopback only
	return a
}

func TestDiscoverFindsServerAtNewAddress(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	var beats atomic.Int32
	ts := fakeServer(t, priv, &beats)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	a := discoveryAgent(t, priv.Public().(ed25519.PublicKey), port)

	a.fails = 5
	if !a.maybeDiscover(t.Context()) {
		t.Fatal("expected discovery to find the server")
	}
	if a.server() != ts.URL {
		t.Fatalf("server = %s, want %s", a.server(), ts.URL)
	}
	if err := a.heartbeat(t.Context()); err != nil || beats.Load() != 1 {
		t.Fatalf("heartbeat at the found address: err=%v beats=%d", err, beats.Load())
	}
	// The find and the announced addresses survive a restart.
	var st state
	b, _ := os.ReadFile(a.cfg.StatePath)
	json.Unmarshal(b, &st)
	if st.Found != ts.URL || st.Server != ts.URL || len(st.Servers) != 1 {
		t.Fatalf("saved state = %+v", st)
	}
}

func TestDiscoverRejectsImpostor(t *testing.T) {
	_, real, _ := ed25519.GenerateKey(nil)
	_, fake, _ := ed25519.GenerateKey(nil)
	var beats atomic.Int32
	ts := fakeServer(t, fake, &beats) // answers on the right port with the wrong key
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	a := discoveryAgent(t, real.Public().(ed25519.PublicKey), port)

	a.fails = 5
	if a.maybeDiscover(t.Context()) || a.server() == ts.URL {
		t.Fatal("agent switched to a server that could not prove its identity")
	}
	if beats.Load() != 0 {
		t.Fatal("agent token was sent to the impostor")
	}
}

func TestDiscoverWaitsForRepeatedFailures(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	var beats atomic.Int32
	ts := fakeServer(t, priv, &beats)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	a := discoveryAgent(t, priv.Public().(ed25519.PublicKey), port)
	if a.maybeDiscover(t.Context()) || a.maybeDiscover(t.Context()) {
		t.Fatal("discovery ran before three failed heartbeats")
	}
	if !a.maybeDiscover(t.Context()) {
		t.Fatal("discovery did not run on the third failure")
	}
}

func TestHostsInNarrowsLargeNetworks(t *testing.T) {
	_, n24, _ := net.ParseCIDR("192.168.0.0/24")
	n24.IP = net.ParseIP("192.168.0.189")
	if got := hostsIn(n24, map[string]bool{}); len(got) != 254 || got[0] != "192.168.0.1" {
		t.Fatalf("/24: %d hosts, first %v", len(got), got[:1])
	}
	_, n8, _ := net.ParseCIDR("10.0.0.0/8")
	n8.IP = net.ParseIP("10.4.7.20")
	if got := hostsIn(n8, map[string]bool{}); len(got) != 254 || got[0] != "10.4.7.1" {
		t.Fatalf("/8: %d hosts, first %v", len(got), got[:1])
	}
}

// Run itself must rotate to a fallback URL and, failing that, discover the server: this is
// the path a real agent takes when the control plane changes address.
func TestRunRecoversThroughFallbackAndDiscovery(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	var beats atomic.Int32
	ts := fakeServer(t, priv, &beats)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	wait := func(what string) {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); beats.Load() == 0; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: no heartbeat reached the server", what)
			}
		}
	}

	// A dead first URL with a working fallback.
	a := discoveryAgent(t, priv.Public().(ed25519.PublicKey), "")
	a.configured = []string{"http://127.0.0.1:1", ts.URL}
	a.setServers("")
	a.saveState()
	ctx, cancel := context.WithCancel(t.Context())
	go a.Run(ctx)
	wait("fallback")
	cancel()

	// Only a dead URL on the right port: discovery has to find it.
	beats.Store(0)
	b := discoveryAgent(t, priv.Public().(ed25519.PublicKey), port)
	b.saveState()
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel2()
	go b.Run(ctx2)
	wait("discovery")
}

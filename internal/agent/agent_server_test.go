package agent

import (
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

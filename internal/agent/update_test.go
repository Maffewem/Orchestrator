package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Maffewem/Orchestrator/internal/api"
)

func newUpdateAgent(t *testing.T, srvURL, dir string) (*Agent, string) {
	t.Helper()
	exe := filepath.Join(dir, "orchestrator-agent")
	os.WriteFile(exe, []byte("#!/bin/sh\necho old\n"), 0o755)
	old := executablePath
	executablePath = func() (string, error) { return exe, nil }
	t.Cleanup(func() { executablePath = old })
	a, err := New(Config{Server: srvURL, DockerHost: "unix:///nonexistent.sock", StatePath: filepath.Join(dir, "state.json")})
	if err != nil {
		t.Fatal(err)
	}
	a.state.AgentToken = "tok"
	return a, exe
}

func updateServer(t *testing.T, version string, binary []byte, sum string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(api.AgentUpdateInfo{Version: version, SHA256: sum, URL: "/downloads/agent"})
	})
	mux.HandleFunc("/downloads/agent", func(w http.ResponseWriter, r *http.Request) { w.Write(binary) })
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestSelfUpdateReplacesBinaryAndKeepsPrevious(t *testing.T) {
	if InContainer() {
		t.Skip("self-update is refused inside containers")
	}
	bin := []byte("#!/bin/sh\necho " + "v2" + "\n")
	sum := sha256.Sum256(bin)
	ts := updateServer(t, "v2", bin, hex.EncodeToString(sum[:]))
	dir := t.TempDir()
	a, exe := newUpdateAgent(t, ts.URL, dir)

	msg, restart, err := a.selfUpdate(context.Background())
	if err != nil || !restart || !strings.Contains(msg, "v2") {
		t.Fatalf("%q %v %v", msg, restart, err)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(bin) {
		t.Fatalf("binary not replaced: %q", b)
	}
	if b, _ := os.ReadFile(exe + ".prev"); !strings.Contains(string(b), "old") {
		t.Fatalf("previous not kept: %q", b)
	}
	if _, err := os.Stat(a.trialPath()); err != nil {
		t.Fatal("no trial marker")
	}
	a.confirmUpdate()
	if _, err := os.Stat(a.trialPath()); err == nil {
		t.Fatal("trial marker not cleared after confirm")
	}
}

func TestSelfUpdateRejectsBadChecksumAndBadBinary(t *testing.T) {
	if InContainer() {
		t.Skip("self-update is refused inside containers")
	}
	dir := t.TempDir()
	good := []byte("#!/bin/sh\necho v2\n")
	ts := updateServer(t, "v2", good, strings.Repeat("0", 64))
	a, exe := newUpdateAgent(t, ts.URL, dir)
	if _, _, err := a.selfUpdate(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad checksum accepted: %v", err)
	}

	junk := []byte("not an executable")
	sum := sha256.Sum256(junk)
	ts2 := updateServer(t, "v3", junk, hex.EncodeToString(sum[:]))
	a.cfg.Server = ts2.URL
	if _, _, err := a.selfUpdate(context.Background()); err == nil {
		t.Fatal("non-executable accepted")
	}
	if b, _ := os.ReadFile(exe); !strings.Contains(string(b), "old") {
		t.Fatal("binary replaced despite failures")
	}
}

func TestSelfUpdateNoopWhenCurrent(t *testing.T) {
	if InContainer() {
		t.Skip("self-update is refused inside containers")
	}
	ts := updateServer(t, Version, []byte("x"), strings.Repeat("a", 64))
	a, _ := newUpdateAgent(t, ts.URL, t.TempDir())
	msg, restart, err := a.selfUpdate(context.Background())
	if err != nil || restart || !strings.Contains(msg, "already") {
		t.Fatalf("%q %v %v", msg, restart, err)
	}
}

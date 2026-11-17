package agent

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseCloudflaredVersion(t *testing.T) {
	got := parseCloudflaredVersion("cloudflared version 2025.11.1 (built 2025-11-07-1200 UTC)\n")
	if got != "2025.11.1" {
		t.Fatalf("got %q", got)
	}
	if parseCloudflaredVersion("nonsense") != "" {
		t.Fatal("expected empty")
	}
}

// fakeTunnelDocker serves just enough of the Docker API for syncTunnel.
func fakeTunnelDocker(t *testing.T, runningImage, pulledImage string) (*Agent, *[]string, *map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	var created map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"Id":"old","Names":["/orchestrator-tunnel"]}]`))
	})
	mux.HandleFunc("GET /containers/old/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Image":"` + runningImage + `","Config":{"Image":"cloudflare/cloudflared:latest","Labels":{"orchestrator.tunnel":"` + tokenFingerprint("tok") + `"}},"State":{"Running":true,"Status":"running"}}`))
	})
	mux.HandleFunc("GET /images/cloudflare/cloudflared:latest/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Id":"` + pulledImage + `"}`))
	})
	mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, "create")
		json.Unmarshal(b, &created)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"Id":"new"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: mux}}
	srv.Start()
	t.Cleanup(srv.Close)
	d, err := NewDocker("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	return &Agent{docker: d}, &calls, &created
}

func TestSyncTunnelUpdateRecreates(t *testing.T) {
	a, calls, created := fakeTunnelDocker(t, "sha256:old", "sha256:new")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.syncTunnel(ctx, "tok", true); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(*calls, ",")
	for _, want := range []string{"POST /images/create", "DELETE /containers/old", "create", "POST /containers/new/start"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in %s", want, all)
		}
	}
	env, _ := (*created)["Env"].([]any)
	if len(env) != 1 || env[0] != "TUNNEL_TOKEN=tok" {
		t.Fatalf("token not kept: %v", (*created)["Env"])
	}
}

func TestSyncTunnelUpdateKeepsCurrent(t *testing.T) {
	a, calls, _ := fakeTunnelDocker(t, "sha256:same", "sha256:same")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.syncTunnel(ctx, "tok", true); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(*calls, ",")
	if strings.Contains(all, "DELETE") || strings.Contains(all, "create,") {
		t.Fatalf("recreated a current container: %s", all)
	}
}

func TestSyncTunnelPlainDoesNotPull(t *testing.T) {
	a, calls, _ := fakeTunnelDocker(t, "sha256:old", "sha256:new")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.syncTunnel(ctx, "tok", false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(*calls, ","), "images/create") {
		t.Fatal("plain sync pulled")
	}
}

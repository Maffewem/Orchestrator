package agent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"github.com/Maffewem/Orchestrator/internal/api"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
)

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s)))
	return append(h, s...)
}

func TestDemux(t *testing.T) {
	var b bytes.Buffer
	b.Write(frame(1, "hello\n"))
	b.Write(frame(2, "oops\n"))
	got, err := demux(&b)
	if err != nil || got != "hello\noops\n" {
		t.Fatalf("demux = %q, %v", got, err)
	}
}

func TestSplitImage(t *testing.T) {
	cases := map[string][2]string{
		"nginx":                     {"nginx", "latest"},
		"nginx:1.27":                {"nginx", "1.27"},
		"ghcr.io/o/app:v1":          {"ghcr.io/o/app", "v1"},
		"registry.local:5000/app":   {"registry.local:5000/app", "latest"},
		"registry.local:5000/app:2": {"registry.local:5000/app", "2"},
		"app@sha256:abc":            {"app@sha256:abc", ""},
	}
	for in, want := range cases {
		repo, tag := splitImage(in)
		if repo != want[0] || tag != want[1] {
			t.Errorf("splitImage(%q) = %q, %q", in, repo, tag)
		}
	}
}

func TestRedact(t *testing.T) {
	in := "failed to clone https://x-access-token:ghp_abc@github.com/o/r.git"
	if got := redact(in); got != "failed to clone https://***@github.com/o/r.git" {
		t.Fatalf("redact = %q", got)
	}
}

func TestLockStateAllowsOneAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	release, err := LockState(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockState(path); err == nil {
		t.Fatal("a second agent got the lock")
	}
	release()
	again, err := LockState(path)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	again()
}

func TestSignedWorkPolicy(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	a := &Agent{log: slog.New(slog.NewTextHandler(io.Discard, nil)), cfg: Config{StatePath: filepath.Join(t.TempDir(), "s.json")}}
	a.state = state{DeviceID: "d1", ServerKey: base64.StdEncoding.EncodeToString(pub)}
	unsigned := api.Command{ID: "c1", DeviceID: "d1", Action: "restart", Target: "x"}
	if err := a.acceptCommand(unsigned); err != nil {
		t.Fatalf("an old server's unsigned command must work during rollout: %v", err)
	}
	signed := api.Command{ID: "c2", DeviceID: "d1", Action: "restart", Target: "x"}
	api.SignCommand(priv, &signed)
	wire, _ := json.Marshal(signed)
	var got api.Command
	json.Unmarshal(wire, &got)
	if err := a.acceptCommand(got); err != nil {
		t.Fatalf("signed command: %v", err)
	}
	if err := a.acceptCommand(unsigned); err == nil {
		t.Fatal("unsigned command accepted after the server started signing (downgrade)")
	}
	_, evil, _ := ed25519.GenerateKey(rand.Reader)
	forged := api.Command{ID: "c3", DeviceID: "d1", Action: "restart", Target: "x"}
	api.SignCommand(evil, &forged)
	wire, _ = json.Marshal(forged)
	json.Unmarshal(wire, &got)
	if err := a.acceptCommand(got); err == nil {
		t.Fatal("a command signed by another key was accepted")
	}
	other := api.Command{ID: "c4", DeviceID: "d2", Action: "restart", Target: "x"}
	api.SignCommand(priv, &other)
	wire, _ = json.Marshal(other)
	json.Unmarshal(wire, &got)
	if err := a.acceptCommand(got); err == nil {
		t.Fatal("a command signed for another device was accepted")
	}
	req := api.StreamRequest{ID: "s1", Kind: "shell", Container: "web"}
	if err := a.acceptStream(req); err == nil {
		t.Fatal("unsigned stream accepted after signing began")
	}
	req.Sig = api.SignText(priv, api.StreamSigningText("d1", req))
	if err := a.acceptStream(req); err != nil {
		t.Fatalf("signed stream: %v", err)
	}
	req.Container = "other"
	if err := a.acceptStream(req); err == nil {
		t.Fatal("changed stream accepted")
	}
	// the pinned key is not replaced by whatever a heartbeat says
	a.learn(api.HeartbeatResponse{ServerKey: base64.StdEncoding.EncodeToString(pub[:31]) + "A"})
	if a.state.ServerKey != base64.StdEncoding.EncodeToString(pub) {
		t.Fatal("the pinned server key was replaced")
	}
}
